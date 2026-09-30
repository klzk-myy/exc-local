// Task 20.3.13 — daily account snapshots (spec §16.7, §24 #362,
// migration 093).
//
// balance_snapshots is a per-account SHA-256 hash chain (audit
// convention, internal/audit): each row's `hash` covers the canonical
// preimage "BS1|acct|ccy|avail|locked|positions_json|date|created_at|prev"
// and `prev_hash` links to the account's previous row ordered by
// (snapshot_date, id). The first row of an account anchors to
// audit.GenesisPrevHash (SHA-256 of the empty input).
//
// Build discipline (fail-closed):
//   - SnapshotBuilder.BuildDay writes one row per (account, currency)
//     balance row for EVERY funded account — an account holding any
//     balances row is snapshotted every day, zero balances included —
//     and embeds the account's full open-position set as positions_json.
//   - Rebuilding a date is allowed ONLY while it is the account's chain
//     tip (a crashed partial build); inserting a date earlier than the
//     tip would fork the ordered chain and is refused loudly.
//   - Per-account builds serialize on a pg_advisory_xact_lock keyed by
//     the account id, so two builder instances cannot interleave rows
//     for one chain.
//
// VerifyAccountChain is the §24 #345 auditor-evidence seam and the input
// Task 24.3.18's evidence pack consumes: it re-walks the stored chain,
// re-canonicalizes positions_json (jsonb key order is not preserved —
// verification always re-derives the canonical form, never hashes the
// stored text) and reports every link/payload violation.
package analytics

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"exchange/internal/audit"
	"exchange/pkg/decimal"
)

// snapshotLockSeed namespaces the per-account advisory locks.
const snapshotLockSeed int64 = 0x53534150 // "SSAP"

// SnapshotPosition is one open position inside positions_json. Decimal
// values serialize as strings so the canonical form survives the jsonb
// round-trip byte-for-byte (jsonb preserves string scalars verbatim).
// Field order is the canonical key order — Go marshals struct fields in
// declaration order, which is what makes the preimage stable.
type SnapshotPosition struct {
	PositionID       int64  `json:"position_id"`
	InstrumentID     int64  `json:"instrument_id"`
	Symbol           string `json:"symbol"`
	Side             string `json:"side"`
	Quantity         string `json:"quantity"`
	EntryPrice       string `json:"entry_price"`
	MarkPrice        string `json:"mark_price,omitempty"`
	LiquidationPrice string `json:"liquidation_price,omitempty"`
	UnrealizedPnL    string `json:"unrealized_pnl"`
	RealizedPnL      string `json:"realized_pnl"`
	MarginUsed       string `json:"margin_used"`
}

// CanonicalPositionsJSON renders the deterministic compact JSON array
// used for both storage and hashing: positions sorted by position_id,
// struct key order fixed, no whitespace.
func CanonicalPositionsJSON(pos []SnapshotPosition) string {
	if len(pos) == 0 {
		return "[]"
	}
	sort.Slice(pos, func(i, j int) bool { return pos[i].PositionID < pos[j].PositionID })
	b, err := json.Marshal(pos)
	if err != nil {
		// Struct-only fields can never fail; guard anyway.
		return "[]"
	}
	return string(b)
}

// decanonicalizePositions parses a stored positions_json blob back into
// the canonical byte form — the only valid input for hash recomputation
// (jsonb normalizes whitespace and key order).
func decanonicalizePositions(raw string) (string, error) {
	var pos []SnapshotPosition
	if err := json.Unmarshal([]byte(raw), &pos); err != nil {
		return "", fmt.Errorf("positions_json decode: %w", err)
	}
	return CanonicalPositionsJSON(pos), nil
}

// SnapshotHash computes the row hash over the canonical preimage. Money
// fields arrive as their stored DECIMAL text (the preimage hashes exactly
// what the column holds, so re-verification is byte-stable).
func SnapshotHash(accountID int64, currency, available, locked, positionsCanonical string,
	snapshotDate, createdAt time.Time, prevHash string) string {
	preimage := fmt.Sprintf("BS1|%d|%s|%s|%s|%s|%s|%d|%s",
		accountID, currency, available, locked, positionsCanonical,
		snapshotDate.UTC().Format("2006-01-02"),
		createdAt.UTC().UnixMicro(), prevHash)
	sum := sha256.Sum256([]byte(preimage))
	return hex.EncodeToString(sum[:])
}

// ---------------------------------------------------------------------------
// Builder — the 23:59 UTC daily writer (cmd/snapshot_builder)
// ---------------------------------------------------------------------------

// SnapshotBuildResult reports one BuildDay run.
type SnapshotBuildResult struct {
	Date     string `json:"date"`
	Accounts int    `json:"accounts"`
	Rows     int    `json:"rows"`
}

// SnapshotBuilder computes + persists the daily snapshot set.
type SnapshotBuilder struct {
	pool *pgxpool.Pool
	now  func() time.Time
}

// NewSnapshotBuilder wires the builder over the pool.
func NewSnapshotBuilder(pool *pgxpool.Pool) *SnapshotBuilder {
	return &SnapshotBuilder{pool: pool, now: func() time.Time { return time.Now().UTC() }}
}

// WithClock swaps the timestamp source (tests pin created_at).
func (b *SnapshotBuilder) WithClock(now func() time.Time) *SnapshotBuilder {
	if now != nil {
		b.now = now
	}
	return b
}

// BuildDay snapshots every funded account for date (normalized to its
// UTC calendar day). Per-account builds commit independently — a crash
// mid-run leaves earlier accounts committed and the interrupted account
// rolled back; the tip-rebuild rule lets the rerun redo it cleanly.
//
// Fail-closed rules (chain integrity):
//   - date earlier than an account's chain tip → error (gap backfill
//     would fork the chain; remediation is rebuilding from the tip);
//   - date equal to the tip → the tip day's rows are deleted and
//     re-chained (safe: nothing newer links to them);
//   - UNIQUE(account, currency, date) still backstops duplicates.
func (b *SnapshotBuilder) BuildDay(ctx context.Context, date time.Time) (*SnapshotBuildResult, error) {
	day := date.UTC().Truncate(24 * time.Hour)
	acctRows, err := b.pool.Query(ctx, `
		SELECT DISTINCT account_id FROM balances ORDER BY account_id`)
	if err != nil {
		return nil, fmt.Errorf("snapshot: list funded accounts: %w", err)
	}
	var accounts []int64
	for acctRows.Next() {
		var id int64
		if err := acctRows.Scan(&id); err != nil {
			acctRows.Close()
			return nil, fmt.Errorf("snapshot: scan account: %w", err)
		}
		accounts = append(accounts, id)
	}
	acctRows.Close()
	if err := acctRows.Err(); err != nil {
		return nil, fmt.Errorf("snapshot: list accounts: %w", err)
	}

	res := &SnapshotBuildResult{Date: day.Format("2006-01-02")}
	for _, accountID := range accounts {
		n, err := b.buildAccountDay(ctx, accountID, day)
		if err != nil {
			return res, fmt.Errorf("snapshot: account %d: %w", accountID, err)
		}
		res.Accounts++
		res.Rows += n
	}
	return res, nil
}

// buildAccountDay writes one account's day inside a single serialized
// transaction and returns the row count.
func (b *SnapshotBuilder) buildAccountDay(ctx context.Context, accountID int64, day time.Time) (int, error) {
	tx, err := b.pool.Begin(ctx)
	if err != nil {
		return 0, fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// Serialize builders per account — two daemons racing one chain must
	// not interleave rows. The id goes in as a string arg so pgx binds it
	// as text against the || concat (an int64 arg infers text OID and
	// fails to encode).
	if _, err := tx.Exec(ctx,
		`SELECT pg_advisory_xact_lock(hashtextextended('balance_snapshots:' || $1, $2))`,
		strconv.FormatInt(accountID, 10), snapshotLockSeed); err != nil {
		return 0, fmt.Errorf("chain lock: %w", err)
	}

	// Chain tip for this account (latest row by the canonical order).
	var tipDate time.Time
	var tipHash string
	err = tx.QueryRow(ctx, `
		SELECT snapshot_date, hash FROM balance_snapshots
		WHERE account_id = $1
		ORDER BY snapshot_date DESC, id DESC LIMIT 1`, accountID).
		Scan(&tipDate, &tipHash)
	if err != nil && err != pgx.ErrNoRows {
		return 0, fmt.Errorf("chain tip: %w", err)
	}
	hasTip := err == nil
	if hasTip && tipDate.After(day) {
		return 0, fmt.Errorf("refusing to backfill %s: chain tip is %s — "+
			"gap backfill would fork the per-account chain",
			day.Format("2006-01-02"), tipDate.Format("2006-01-02"))
	}

	var sameDay int64
	if err := tx.QueryRow(ctx, `
		SELECT count(*) FROM balance_snapshots
		WHERE account_id = $1 AND snapshot_date = $2`,
		accountID, day).Scan(&sameDay); err != nil {
		return 0, fmt.Errorf("same-day probe: %w", err)
	}
	if sameDay > 0 {
		// Only reachable when tipDate == day (gap-days were refused
		// above): rebuild the tip — delete and re-chain.
		if _, err := tx.Exec(ctx, `
			DELETE FROM balance_snapshots
			WHERE account_id = $1 AND snapshot_date = $2`,
			accountID, day); err != nil {
			return 0, fmt.Errorf("tip rebuild delete: %w", err)
		}
		// Re-read the tip — it now anchors to the previous day.
		err = tx.QueryRow(ctx, `
			SELECT snapshot_date, hash FROM balance_snapshots
			WHERE account_id = $1
			ORDER BY snapshot_date DESC, id DESC LIMIT 1`, accountID).
			Scan(&tipDate, &tipHash)
		if err == pgx.ErrNoRows {
			hasTip = false
		} else if err != nil {
			return 0, fmt.Errorf("chain tip re-read: %w", err)
		}
	}

	prev := audit.GenesisPrevHash
	if hasTip {
		prev = tipHash
	}

	// Balance rows for the account — every currency, zero/zero included.
	balRows, err := tx.Query(ctx, `
		SELECT currency, available::text, locked::text
		FROM balances WHERE account_id = $1 ORDER BY currency`, accountID)
	if err != nil {
		return 0, fmt.Errorf("balances read: %w", err)
	}
	type balRow struct{ ccy, avail, locked string }
	var bals []balRow
	for balRows.Next() {
		var br balRow
		if err := balRows.Scan(&br.ccy, &br.avail, &br.locked); err != nil {
			balRows.Close()
			return 0, fmt.Errorf("balances scan: %w", err)
		}
		bals = append(bals, br)
	}
	balRows.Close()
	if err := balRows.Err(); err != nil {
		return 0, fmt.Errorf("balances rows: %w", err)
	}

	// Open positions (quantity <> 0) → canonical positions_json, shared
	// by every currency row of the account for the day.
	posRows, err := tx.Query(ctx, `
		SELECT p.id, p.instrument_id, COALESCE(i.symbol, ''), p.side::text,
		       p.quantity::text, p.entry_price::text,
		       COALESCE(p.mark_price::text, ''),
		       COALESCE(p.liquidation_price::text, ''),
		       p.unrealized_pnl::text, p.realized_pnl::text,
		       p.margin_used::text
		FROM positions p
		LEFT JOIN instruments i ON i.id = p.instrument_id
		WHERE p.account_id = $1 AND p.quantity <> 0
		ORDER BY p.id`, accountID)
	if err != nil {
		return 0, fmt.Errorf("positions read: %w", err)
	}
	var positions []SnapshotPosition
	for posRows.Next() {
		var p SnapshotPosition
		if err := posRows.Scan(&p.PositionID, &p.InstrumentID, &p.Symbol,
			&p.Side, &p.Quantity, &p.EntryPrice, &p.MarkPrice,
			&p.LiquidationPrice, &p.UnrealizedPnL, &p.RealizedPnL,
			&p.MarginUsed); err != nil {
			posRows.Close()
			return 0, fmt.Errorf("positions scan: %w", err)
		}
		positions = append(positions, p)
	}
	posRows.Close()
	if err := posRows.Err(); err != nil {
		return 0, fmt.Errorf("positions rows: %w", err)
	}
	posJSON := CanonicalPositionsJSON(positions)
	createdAt := b.now().UTC().Truncate(time.Microsecond)

	for _, br := range bals {
		h := SnapshotHash(accountID, br.ccy, br.avail, br.locked,
			posJSON, day, createdAt, prev)
		if _, err := tx.Exec(ctx, `
			INSERT INTO balance_snapshots
			    (account_id, currency, available, locked, positions_json,
			     snapshot_date, hash, prev_hash, created_at)
			VALUES ($1,$2,$3::numeric,$4::numeric,$5::jsonb,$6,$7,$8,$9)`,
			accountID, br.ccy, br.avail, br.locked, posJSON,
			day, h, prev, createdAt); err != nil {
			return 0, fmt.Errorf("insert %s: %w", br.ccy, err)
		}
		prev = h
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("commit: %w", err)
	}
	return len(bals), nil
}

// ---------------------------------------------------------------------------
// Read path + chain verification
// ---------------------------------------------------------------------------

// SnapshotRow is one balance_snapshots row as stored. PositionsJSON is
// the raw jsonb text (handler passes it through; the verifier re-canonicalizes).
type SnapshotRow struct {
	ID            int64
	AccountID     int64
	Currency      string
	Available     decimal.Decimal
	Locked        decimal.Decimal
	PositionsJSON string
	SnapshotDate  time.Time
	Hash          string
	PrevHash      string
	CreatedAt     time.Time
}

// SnapshotCursor is the (snapshot_date, id) keyset for the DESC history
// stream — rendered by the api layer as the §8.8 opaque token.
type SnapshotCursor struct {
	SnapshotDate time.Time
	ID           int64
}

// SnapshotStore is the PG read path for the history endpoint.
// PGQuerier is satisfied by *pgxpool.Pool.
type SnapshotStore struct{ pg PGQuerier }

// NewSnapshotStore wires the read store.
func NewSnapshotStore(pg PGQuerier) *SnapshotStore { return &SnapshotStore{pg: pg} }

// History returns one keyset page (newest date first) plus the total
// matching count. date filters to exactly one snapshot day when set.
func (s *SnapshotStore) History(ctx context.Context, accountID int64,
	date *time.Time, after *SnapshotCursor, limit int) ([]SnapshotRow, int64, error) {
	if s == nil || s.pg == nil {
		return nil, 0, fmt.Errorf("snapshot store has no PG handle")
	}
	if limit <= 0 {
		limit = 100
	}
	where := " WHERE account_id = $1"
	args := []any{accountID}
	if date != nil {
		args = append(args, *date)
		where += fmt.Sprintf(" AND snapshot_date = $%d", len(args))
	}
	if after != nil {
		args = append(args, after.SnapshotDate, after.ID)
		where += fmt.Sprintf(" AND (snapshot_date, id) < ($%d, $%d)",
			len(args)-1, len(args))
	}
	var total int64
	cnt, err := s.pg.Query(ctx,
		"SELECT count(*) FROM balance_snapshots"+where, args...)
	if err != nil {
		return nil, 0, fmt.Errorf("snapshot history count: %w", err)
	}
	if cnt.Next() {
		if err := cnt.Scan(&total); err != nil {
			cnt.Close()
			return nil, 0, fmt.Errorf("snapshot history count scan: %w", err)
		}
	}
	cnt.Close()
	if err := cnt.Err(); err != nil {
		return nil, 0, fmt.Errorf("snapshot history count rows: %w", err)
	}

	args = append(args, limit)
	rows, err := s.pg.Query(ctx, `
		SELECT id, account_id, currency, available::text, locked::text,
		       positions_json::text, snapshot_date, hash, prev_hash, created_at
		FROM balance_snapshots`+where+
		` ORDER BY snapshot_date DESC, id DESC LIMIT $`+
		fmt.Sprint(len(args)), args...)
	if err != nil {
		return nil, 0, fmt.Errorf("snapshot history: %w", err)
	}
	defer rows.Close()

	out := make([]SnapshotRow, 0, limit)
	for rows.Next() {
		var r SnapshotRow
		var avail, locked string
		if err := rows.Scan(&r.ID, &r.AccountID, &r.Currency, &avail,
			&locked, &r.PositionsJSON, &r.SnapshotDate, &r.Hash,
			&r.PrevHash, &r.CreatedAt); err != nil {
			return nil, 0, fmt.Errorf("snapshot history scan: %w", err)
		}
		if r.Available, err = decimal.NewFromString(avail); err != nil {
			return nil, 0, fmt.Errorf("snapshot available %q: %w", avail, err)
		}
		if r.Locked, err = decimal.NewFromString(locked); err != nil {
			return nil, 0, fmt.Errorf("snapshot locked %q: %w", locked, err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("snapshot history rows: %w", err)
	}
	return out, total, nil
}

// ---------------------------------------------------------------------------
// Chain verification — §24 #345 auditor evidence / Task 24.3.18 input
// ---------------------------------------------------------------------------

// SnapshotViolation is one detected chain break. Field is the offending
// column ("prev_hash", "hash", "positions_json"); Detail adds the
// operator-facing explanation.
type SnapshotViolation struct {
	RowID        int64  `json:"row_id"`
	SnapshotDate string `json:"snapshot_date"`
	Currency     string `json:"currency"`
	Field        string `json:"field"`
	Expected     string `json:"expected,omitempty"`
	Actual       string `json:"actual,omitempty"`
	Detail       string `json:"detail,omitempty"`
}

// SnapshotChainReport is the VerifyAccountChain output — the evidence
// object auditors/operators consume (spec §24 #345; Task 24.3.18's
// evidence pack embeds this report verbatim per account).
type SnapshotChainReport struct {
	AccountID   int64               `json:"account_id"`
	RowsChecked int                 `json:"rows_checked"`
	Violations  []SnapshotViolation `json:"violations"`
	OK          bool                `json:"ok"`
}

// VerifyAccountChain re-walks the per-account snapshot chain in the
// canonical (snapshot_date, id) order and verifies:
//   - row 1 links to audit.GenesisPrevHash; every later row links to its
//     predecessor's hash;
//   - each row's stored hash equals SnapshotHash over its canonical
//     fields with positions_json re-canonicalized (jsonb key order is
//     not stored, so the raw text is never hashed);
//   - positions_json parses into the canonical schema.
//
// Auditor evidence for spec §24 #345 — Task 24.3.18's evidence pack
// calls this per account and embeds the report; OK=false means at least
// one tampered/rewritten/unreconstructable row.
func VerifyAccountChain(ctx context.Context, pg PGQuerier, accountID int64) (*SnapshotChainReport, error) {
	if pg == nil {
		return nil, fmt.Errorf("snapshot verify: no PG handle")
	}
	rows, err := pg.Query(ctx, `
		SELECT id, currency, available::text, locked::text,
		       positions_json::text, snapshot_date, hash, prev_hash, created_at
		FROM balance_snapshots
		WHERE account_id = $1
		ORDER BY snapshot_date ASC, id ASC`, accountID)
	if err != nil {
		return nil, fmt.Errorf("snapshot verify read: %w", err)
	}
	defer rows.Close()

	rep := &SnapshotChainReport{AccountID: accountID, OK: true}
	prev := audit.GenesisPrevHash
	for rows.Next() {
		var (
			id                 int64
			ccy, avail, locked string
			posJSON            string
			day, createdAt     time.Time
			hash, prevHash     string
		)
		if err := rows.Scan(&id, &ccy, &avail, &locked, &posJSON,
			&day, &hash, &prevHash, &createdAt); err != nil {
			return rep, fmt.Errorf("snapshot verify scan: %w", err)
		}
		rep.RowsChecked++
		dateStr := day.UTC().Format("2006-01-02")

		canon, cerr := decanonicalizePositions(posJSON)
		if cerr != nil {
			rep.OK = false
			rep.Violations = append(rep.Violations, SnapshotViolation{
				RowID: id, SnapshotDate: dateStr, Currency: ccy,
				Field: "positions_json", Detail: cerr.Error(),
			})
			canon = posJSON // continue: hash check will also flag
		}
		if prevHash != prev {
			rep.OK = false
			rep.Violations = append(rep.Violations, SnapshotViolation{
				RowID: id, SnapshotDate: dateStr, Currency: ccy,
				Field: "prev_hash", Expected: prev, Actual: prevHash,
				Detail: "chain link broken — row does not anchor to its " +
					"predecessor in (snapshot_date, id) order",
			})
		}
		want := SnapshotHash(accountID, ccy, avail, locked, canon,
			day, createdAt, prevHash)
		if hash != want {
			rep.OK = false
			rep.Violations = append(rep.Violations, SnapshotViolation{
				RowID: id, SnapshotDate: dateStr, Currency: ccy,
				Field: "hash", Expected: want, Actual: hash,
				Detail: "payload hash mismatch — canonical fields were " +
					"altered after the snapshot was written",
			})
		}
		prev = hash // walk the stored chain regardless of validity
	}
	if err := rows.Err(); err != nil {
		return rep, fmt.Errorf("snapshot verify rows: %w", err)
	}
	return rep, nil
}
