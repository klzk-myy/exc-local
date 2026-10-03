// account_state.go — AccountStateProjector: the Go-side publisher half of
// the engine's production IAccountState/IPositionState binding (C++ peer:
// core/include/risk/AccountStateCache.hpp — AccountStateRefresher polls
// `HGETALL account:state` on a control thread and fails closed on a
// missing/stale snapshot).
//
// The control plane is the authority over accounts/balances/positions/
// instruments; each Publish round builds the WHOLE hash out of one
// consistent read set and swaps it in atomically:
//
//	account:state:next  (build)  ->  RENAME ->  account:state
//
// RENAME is atomic in Redis: the engine either sees the previous hash or
// the new one — never a torn mid-write mix. A crash between DEL and HSET
// only delays the next publish; the engine's heartbeat TTL (default 30s)
// then fails closed.
//
// Field grammar (parsed by core/src/risk/AccountStateCache.cpp):
//
//	"{account_id}"           -> "<STATUS>,<KYC>,<CATEGORY>,<STP>,<OPEN_POS>"
//	"a:{account_id}:{CCY}"   -> available balance, int64 (1e8 units)
//	"p:{account_id}:{INSTR}" -> net position units, signed int64 (1e8 scale)
//	"i:{instrument_id}"      -> "BASE/QUOTE"
//	"c:{account_id}"         -> bilateral credit party index (spec §3.3b)
//	"__hb__"                 -> publish heartbeat, unix seconds
//
// Fail-closed posture (spec §2.7): a Publish error leaves the previous
// snapshot standing until its heartbeat ages out; Run keeps retrying on
// the interval. The publisher never writes partial state — the build is
// staged under :next and promoted only on a complete row set.
package risk

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	goredis "github.com/redis/go-redis/v9"

	"exchange/pkg/decimal"
)

// AccountStateKey is the live hash the engine refresher reads;
// accountStateNextKey is the publish staging key promoted by RENAME.
const (
	AccountStateKey       = "account:state"
	accountStateNextKey   = "account:state:next"
	accountHeartbeatField = "__hb__"
)

// AccountStateProjector periodically projects the control-plane account
// corpus into the `account:state` hash. Single-writer by convention — the
// RENAME swap makes duplicate publishers benign (last writer wins), but
// deployments run it inside the singleton cmd/risk coordinator.
type AccountStateProjector struct {
	Pool     *pgxpool.Pool
	Rdb      goredis.Cmdable
	Interval time.Duration // publish cadence; default 1s when zero
	Log      *slog.Logger
	// now is injectable for tests; nil -> time.Now.
	now func() time.Time
}

// NewAccountStateProjector binds the dependencies — nil pool/redis is
// rejected fail-closed (a projector that cannot read or write is worse
// than none: it would hold a stale heartbeat alive).
func NewAccountStateProjector(pool *pgxpool.Pool, rdb goredis.Cmdable,
	log *slog.Logger) (*AccountStateProjector, error) {
	if pool == nil {
		return nil, fmt.Errorf("account state projector: nil pgx pool")
	}
	if rdb == nil {
		return nil, fmt.Errorf("account state projector: nil redis")
	}
	if log == nil {
		log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	return &AccountStateProjector{
		Pool: pool, Rdb: rdb,
		Interval: time.Second, Log: log, now: time.Now,
	}, nil
}

// Run publishes on the interval until ctx is done; one failed round logs
// and retries — the heartbeat TTL is the engine-side liveness gate.
func (p *AccountStateProjector) Run(ctx context.Context) {
	interval := p.Interval
	if interval <= 0 {
		interval = time.Second
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	// First publish immediately — the engine's boot-time synchronous poll
	// should see a fresh snapshot, not an empty hash.
	if err := p.Publish(ctx); err != nil {
		p.Log.Warn("account_state: initial publish failed", "err", err)
	}
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := p.Publish(ctx); err != nil {
				p.Log.Warn("account_state: publish failed", "err", err)
			}
		}
	}
}

// Publish stages the whole `account:state` hash from the current PG corpus
// and promotes it atomically. Any query failure aborts before the RENAME —
// the previous snapshot stands and ages out by its heartbeat.
func (p *AccountStateProjector) Publish(ctx context.Context) error {
	fields, err := p.build(ctx)
	if err != nil {
		return err
	}
	fields[accountHeartbeatField] = strconv.FormatInt(p.now().Unix(), 10)

	pipe := p.Rdb.TxPipeline()
	pipe.Del(ctx, accountStateNextKey)
	pipe.HSet(ctx, accountStateNextKey, fields)
	pipe.Rename(ctx, accountStateNextKey, AccountStateKey)
	if _, err := pipe.Exec(ctx); err != nil {
		return fmt.Errorf("account_state: publish swap: %w", err)
	}
	return nil
}

// build assembles the hash field map: instrument ccy pairs, account
// records, per-account open-position counts, balances, net positions.
func (p *AccountStateProjector) build(ctx context.Context) (map[string]any, error) {
	fields := make(map[string]any, 4096)

	// Instruments — the engine resolves BUY/SELL debits off the pair.
	rows, err := p.Pool.Query(ctx,
		`SELECT id, base_currency, quote_currency FROM instruments`)
	if err != nil {
		return nil, fmt.Errorf("account_state: instruments: %w", err)
	}
	for rows.Next() {
		var id int64
		var base, quote string
		if err := rows.Scan(&id, &base, &quote); err != nil {
			rows.Close()
			return nil, fmt.Errorf("account_state: instrument scan: %w", err)
		}
		fields[fmt.Sprintf("i:%d", id)] = base + "/" + quote
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("account_state: instruments rows: %w", err)
	}

	// Accounts — status/kyc/category/stp + open-position count folded in.
	// OPEN_POS counts non-zero NET positions per instrument (the same
	// aggregation the p: fields use): a LONG/SHORT pair on one instrument
	// nets to a single open position — raw row count would overstate the
	// engine's max_open_positions gate.
	rows, err = p.Pool.Query(ctx, `
		SELECT a.id, a.status::text, a.kyc_tier::text,
		       a.client_category::text, a.default_stp_mode,
		       COALESCE(pc.n, 0)
		FROM accounts a
		LEFT JOIN (
		    SELECT account_id, count(*) AS n
		    FROM (
		        SELECT account_id, instrument_id
		        FROM positions WHERE quantity > 0
		        GROUP BY account_id, instrument_id
		        HAVING sum(CASE WHEN side = 'LONG'
		                        THEN quantity ELSE -quantity END) <> 0
		    ) net
		    GROUP BY account_id
		) pc ON pc.account_id = a.id`)
	if err != nil {
		return nil, fmt.Errorf("account_state: accounts: %w", err)
	}
	for rows.Next() {
		var id, n int64
		var status, kyc, cat, stp string
		if err := rows.Scan(&id, &status, &kyc, &cat, &stp, &n); err != nil {
			rows.Close()
			return nil, fmt.Errorf("account_state: account scan: %w", err)
		}
		fields[strconv.FormatInt(id, 10)] =
			fmt.Sprintf("%s,%s,%s,%s,%d", status, kyc, cat, stp, n)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("account_state: accounts rows: %w", err)
	}

	// Balances — available only; locked is a control-plane reservation the
	// engine must never see as spendable (spec §5.3).
	rows, err = p.Pool.Query(ctx,
		`SELECT account_id, currency, available::text FROM balances`)
	if err != nil {
		return nil, fmt.Errorf("account_state: balances: %w", err)
	}
	for rows.Next() {
		var id int64
		var ccy, availText string
		if err := rows.Scan(&id, &ccy, &availText); err != nil {
			rows.Close()
			return nil, fmt.Errorf("account_state: balance scan: %w", err)
		}
		d, derr := decimal.NewFromString(availText)
		if derr != nil {
			rows.Close()
			return nil, fmt.Errorf("account_state: balance parse %q: %w",
				availText, derr)
		}
		fields[fmt.Sprintf("a:%d:%s", id, ccy)] =
			strconv.FormatInt(decimal.Scaled(d), 10)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("account_state: balances rows: %w", err)
	}

	// Net positions — LONG adds, SHORT subtracts (signed 1e8 units); the
	// engine's reduce-only gate reads this (check 13).
	rows, err = p.Pool.Query(ctx, `
		SELECT account_id, instrument_id,
		       sum(CASE WHEN side = 'LONG' THEN quantity ELSE -quantity END)::text
		FROM positions WHERE quantity > 0
		GROUP BY account_id, instrument_id`)
	if err != nil {
		return nil, fmt.Errorf("account_state: positions: %w", err)
	}
	for rows.Next() {
		var id, instr int64
		var netText string
		if err := rows.Scan(&id, &instr, &netText); err != nil {
			rows.Close()
			return nil, fmt.Errorf("account_state: position scan: %w", err)
		}
		d, derr := decimal.NewFromString(netText)
		if derr != nil {
			rows.Close()
			return nil, fmt.Errorf("account_state: position parse %q: %w",
				netText, derr)
		}
		fields[fmt.Sprintf("p:%d:%d", id, instr)] =
			strconv.FormatInt(decimal.Scaled(d), 10)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("account_state: positions rows: %w", err)
	}

	// Credit party indexes (spec §3.3b screened liquidity) — the engine's
	// IPartyMap. Accounts without a credit_parties row simply have no
	// c: field and remain unscreened (anonymous flow). The table exists
	// only after migration 053; a missing-table error is tolerated so the
	// projector still runs on minimal schemas, but a real query failure
	// must not silently strip screening from a populated corpus — the
	// publish aborts and the previous snapshot stands.
	rows, err = p.Pool.Query(ctx,
		`SELECT account_id, party_index FROM credit_parties`)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "42P01" {
			return fields, nil
		}
		return nil, fmt.Errorf("account_state: credit parties: %w", err)
	}
	for rows.Next() {
		var id int64
		var idx uint32
		if err := rows.Scan(&id, &idx); err != nil {
			rows.Close()
			return nil, fmt.Errorf("account_state: credit party scan: %w", err)
		}
		fields[fmt.Sprintf("c:%d", id)] = strconv.FormatUint(uint64(idx), 10)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("account_state: credit parties rows: %w", err)
	}

	return fields, nil
}
