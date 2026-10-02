// PostgreSQL read/write side of the order pipeline. Decimal columns are
// always read via `::text` and written via `$n::numeric` — the repo
// convention for the shopspring-free pgx binding (see
// internal/risk PgStore).
package orders

import (
	"context"
	"encoding/json"
	stderrors "errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"exchange/pkg/decimal"
)

// pgUniqueViolation is the SQLSTATE the idempotency path must detect.
const pgUniqueViolation = "23505"

// Store is the persistence seam — PgStore implements it over pgx; unit
// tests may substitute fakes for service-level coverage.
type Store interface {
	InstrumentBySymbol(ctx context.Context, symbol string) (*Instrument, error)
	InstrumentByID(ctx context.Context, id int64) (*Instrument, error)
	AccountByID(ctx context.Context, id int64) (*Account, error)
	// ReferencePrice returns the best available reference (latest trade
	// price) for band/min-notional evaluation; nil = no reference yet.
	ReferencePrice(ctx context.Context, instrumentID int64) (*decimal.Decimal, error)
	// AvailableBalance reads balances.available for the currency —
	// pre-trade sufficiency check only; mutation is engine/ledger-side
	// (spec §8.4 item 5 layer boundary + §5.3 locking protocol owner).
	AvailableBalance(ctx context.Context, accountID int64, currency string) (*decimal.Decimal, error)

	// DedupLookup resolves (account, client_order_id) → stored order.
	DedupLookup(ctx context.Context, accountID int64, clientOrderID string) (*DedupRow, error)
	// InsertOrderTX runs dedup-insert + order-insert in one tx. On a
	// unique-constraint race it returns the conflicting DedupRow via
	// dedupConflict so the caller can replay or reject.
	InsertOrderTx(ctx context.Context, p InsertParams) (*Order, *DedupRow, error)
	GetOrder(ctx context.Context, orderID int64) (*Order, error)
	ListOrders(ctx context.Context, q ListQuery) ([]Order, int64, error)
	OpenOrders(ctx context.Context, scope MassCancelScope) ([]Order, error)
	// ApplyCancel / ApplyFill / MarkActive / MarkRejected mutate the
	// read model — idempotent on terminal state.
	ApplyCancel(ctx context.Context, orderID int64) error
	ApplyFill(ctx context.Context, orderID int64, price, qty decimal.Decimal) error
	MarkActive(ctx context.Context, orderID int64) error
	// MarkReserved flips a PENDING FIXING order into its queued state
	// (Phase-16 Task 16.3.9 — fixing orders never touch the engine).
	MarkReserved(ctx context.Context, orderID int64) error
	MarkRejected(ctx context.Context, orderID int64) error
	// AmendCAS applies an amend + its audit rows atomically with the
	// order_seq compare-and-swap (STALE_MODIFY fencing). ok=false means
	// the CAS missed (concurrent mutation) — caller maps to STALE_MODIFY.
	AmendCAS(ctx context.Context, orderID int64, expectedSeq, newSeq uint64,
		f AmendFields, audits []AuditEntry) (*Order, bool, error)
	// RevertAmend restores prev field values + seq after a wire dispatch
	// failure so PG never diverges from the engine's untouched state.
	RevertAmend(ctx context.Context, prev *Order) error

	// InsertOcoPairTx persists both legs of an OCO pair + the shared
	// oco_group_id in ONE transaction (Phase-14 Task 14.3.1, spec §6.5):
	// a dedup conflict or insert failure on either leg aborts the pair —
	// the engine link can never outlive its rows. The conflict path returns
	// the colliding DedupRow via dedupConflict exactly like InsertOrderTx.
	InsertOcoPairTx(ctx context.Context, groupID int64, a, b InsertParams) (*Order, *Order, error)

	WriteAudit(ctx context.Context, entries []AuditEntry) error
	AuditTrail(ctx context.Context, orderID int64) ([]AuditEntry, error)
	Amendments(ctx context.Context, orderID int64) ([]AuditEntry, error)

	// §8.8 Idempotency-Key store (batch POST).
	IdemLookup(ctx context.Context, accountID int64, key string) (*IdemRow, error)
	IdemStore(ctx context.Context, accountID int64, key, endpoint, reqHash string,
		status int, body []byte) error
}

// DedupRow is one client_order_id_dedup record (spec §5.4a).
type DedupRow struct {
	AccountID     int64
	ClientOrderID string
	OrderID       int64
	RequestHash   string
	CreatedAt     time.Time
}

// IdemRow is one idempotency_keys record (spec §8.8).
type IdemRow struct {
	RequestHash  string
	StatusCode   int
	ResponseBody []byte
	CreatedAt    time.Time
}

// AuditEntry is one order_audit row (migration 153).
type AuditEntry struct {
	OrderID    int64
	AccountID  int64
	Operation  string // MODIFY|AMEND|CANCEL_REPLACE|CANCEL|BATCH_*|MASS_CANCEL
	FieldName  string
	OldValue   string
	NewValue   string
	ModifiedBy string
	RequestID  string
	IPAddress  string
	ModifiedAt time.Time // read side only
}

// InsertParams carries every column written on order insert.
type InsertParams struct {
	AccountID     int64
	InstrumentID  int64
	ClientOrderID string
	Side          string
	OrderType     string
	Quantity      decimal.Decimal // for quote-denominated markets: derived base qty
	QuoteQuantity *decimal.Decimal
	Price         *decimal.Decimal
	StopPrice     *decimal.Decimal
	DisplayQty    *decimal.Decimal
	TimeInForce   string
	ShardID       int
	OrderSeq      uint64
	PostOnly      bool
	ReduceOnly    bool
	STPMode       string
	SessionID     string
	RequestHash   string // dedup payload fingerprint ("" when no client_order_id)
	// CoDExempt persists the cancel-on-disconnect exemption flag
	// (migration 229, spec §9.9).
	CoDExempt bool
	// OcoGroupID links both legs of an OCO pair (migration 218); nil =
	// standalone order.
	OcoGroupID *int64
	// ---- Phase-16 Task 16.3.10 exec-param columns (migration 038) +
	// Task 16.3.17 trigger source (migration 066) ----
	PegMode         *string
	PegOffset       *decimal.Decimal
	PegLimit        *decimal.Decimal
	TriggerSource   string // "" persists the column default LAST_PRICE
	Hidden          bool
	GSLO            bool
	FixingBenchmark *string
	AlgoType        *string
	AlgoParams      json.RawMessage // nil ⇒ NULL
	// DiscretionaryOffsetPips — spec §6.11 band (migration 103); nil/0
	// persists the column default.
	DiscretionaryOffsetPips *decimal.Decimal
	// GTDExpiry — effective resting deadline (migration 284): client GTD
	// date or the spec §27 R8 90-day GTC cap.
	GTDExpiry *time.Time
}

// ListQuery is the §8.8 cursor-paginated history query. The filterable
// set matches ListSpec "/api/v1/orders": symbol (resolved to
// instrument_id), status, side, type, client_order_id, from/to.
type ListQuery struct {
	AccountID     int64
	InstrumentID  int64 // 0 = all
	Status        string
	Side          string
	OrderType     string
	ClientOrderID string
	From          *time.Time
	To            *time.Time
	CursorID      int64 // (created_at,id) keyset — both parts required together
	CursorAt      time.Time
	Limit         int
}

// ErrDedupConflict is returned by InsertOrderTx when the unique
// (account_id, client_order_id) key already exists — the caller inspects
// the attached row to decide replay vs IDEMPOTENCY_KEY_COLLISION.
type dedupConflict struct{ row *DedupRow }

func (e *dedupConflict) Error() string { return "client_order_id already exists for account" }

// DedupConflictRow unwraps a dedup conflict error into its stored row.
func DedupConflictRow(err error) *DedupRow {
	var c *dedupConflict
	if stderrors.As(err, &c) {
		return c.row
	}
	return nil
}

// --- PgStore ---------------------------------------------------------------

type PgStore struct{ pool *pgxpool.Pool }

func NewPgStore(pool *pgxpool.Pool) *PgStore { return &PgStore{pool: pool} }

func isNoRows(err error) bool { return stderrors.Is(err, pgx.ErrNoRows) }

const instrumentCols = `
	id, symbol, base_currency, quote_currency, instrument_type::text, status::text,
	tick_size::text, lot_size::text, min_order_qty::text, max_order_qty::text,
	min_notional::text, price_band_pct_up::text, price_band_pct_down::text,
	max_leverage, min_price::text, max_price::text, max_spread_pips::text,
	max_open_orders`

func scanInstrument(row pgx.Row) (*Instrument, error) {
	var (
		i                                     Instrument
		tick, lot, minQ, maxQ, minN, up, down string
		minP, maxP, spread                    *string
	)
	err := row.Scan(&i.ID, &i.Symbol, &i.BaseCurrency, &i.QuoteCurrency,
		&i.InstrumentType, &i.Status, &tick, &lot, &minQ, &maxQ, &minN,
		&up, &down, &i.MaxLeverage, &minP, &maxP, &spread, &i.MaxOpenOrders)
	if err != nil {
		return nil, err
	}
	i.TickSize = decimal.RequireFromString(tick)
	i.LotSize = decimal.RequireFromString(lot)
	i.MinOrderQty = decimal.RequireFromString(minQ)
	i.MaxOrderQty = decimal.RequireFromString(maxQ)
	i.MinNotional = decimal.RequireFromString(minN)
	i.PriceBandPctUp = decimal.RequireFromString(up)
	i.PriceBandPctDown = decimal.RequireFromString(down)
	i.MinPrice = mustParseDecPtr(minP)
	i.MaxPrice = mustParseDecPtr(maxP)
	i.MaxSpreadPips = mustParseDecPtr(spread)
	return &i, nil
}

func mustParseDecPtr(s *string) *decimal.Decimal {
	if s == nil {
		return nil
	}
	d, err := decimal.NewFromString(*s)
	if err != nil {
		return nil // corrupt data treated as absent — bounds are fail-closed elsewhere
	}
	return &d
}

func (s *PgStore) InstrumentBySymbol(ctx context.Context, symbol string) (*Instrument, error) {
	inst, err := scanInstrument(s.pool.QueryRow(ctx,
		`SELECT `+instrumentCols+` FROM instruments WHERE symbol = $1`,
		strings.ToUpper(strings.TrimSpace(symbol))))
	if isNoRows(err) {
		return nil, nil
	}
	return inst, err
}

func (s *PgStore) InstrumentByID(ctx context.Context, id int64) (*Instrument, error) {
	inst, err := scanInstrument(s.pool.QueryRow(ctx,
		`SELECT `+instrumentCols+` FROM instruments WHERE id = $1`, id))
	if isNoRows(err) {
		return nil, nil
	}
	return inst, err
}

func (s *PgStore) AccountByID(ctx context.Context, id int64) (*Account, error) {
	a := &Account{}
	err := s.pool.QueryRow(ctx, `
		SELECT id, user_id, account_type::text, kyc_tier::text, status::text,
		       client_category::text, nbp,
		       trade_group_id, default_stp_mode, cancel_on_disconnect
		FROM accounts WHERE id = $1`, id).
		Scan(&a.ID, &a.UserID, &a.Type, &a.KycTier, &a.Status,
			&a.ClientCategory, &a.NBP,
			&a.TradeGroupID, &a.DefaultSTPMode, &a.CancelOnDisconnect)
	if isNoRows(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return a, nil
}

func (s *PgStore) ReferencePrice(ctx context.Context, instrumentID int64) (*decimal.Decimal, error) {
	var px *string
	err := s.pool.QueryRow(ctx, `
		SELECT price::text FROM trades
		WHERE instrument_id = $1 ORDER BY id DESC LIMIT 1`, instrumentID).Scan(&px)
	if isNoRows(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return mustParseDecPtr(px), nil
}

func (s *PgStore) AvailableBalance(ctx context.Context, accountID int64, currency string) (*decimal.Decimal, error) {
	var avail *string
	err := s.pool.QueryRow(ctx, `
		SELECT available::text FROM balances
		WHERE account_id = $1 AND currency = $2`, accountID, currency).Scan(&avail)
	if isNoRows(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return mustParseDecPtr(avail), nil
}

func (s *PgStore) DedupLookup(ctx context.Context, accountID int64, clientOrderID string) (*DedupRow, error) {
	row := &DedupRow{}
	err := s.pool.QueryRow(ctx, `
		SELECT account_id, client_order_id, order_id, request_hash, created_at
		FROM client_order_id_dedup
		WHERE account_id = $1 AND client_order_id = $2`,
		accountID, clientOrderID).
		Scan(&row.AccountID, &row.ClientOrderID, &row.OrderID, &row.RequestHash, &row.CreatedAt)
	if isNoRows(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return row, nil
}

const orderCols = `
	id, account_id, instrument_id, COALESCE(client_order_id,''), side::text,
	order_type::text, quantity::text, quote_quantity::text, price::text,
	stop_price::text, display_qty::text, time_in_force::text, status::text,
	filled_qty::text, avg_fill_price::text, shard_id, book_seq, order_seq,
	post_only, reduce_only, COALESCE(stp_mode,''), COALESCE(session_id,''),
	oco_group_id,
	created_at, updated_at,
	peg_mode, peg_offset::text, peg_limit::text,
	COALESCE(trigger_source,''), hidden, gslo, fixing_benchmark,
	algo_type, algo_params, cod_exempt, discretionary_offset_pips::text,
	gtd_expire_at`

func scanOrder(row pgx.Row) (*Order, error) {
	var (
		o                                     Order
		qty, tif, status, filled, side, otype string
		quote, price, stop, display, avg      *string
		pegOff, pegLim                        *string
		algoParams                            []byte
	)
	var discOff string
	err := row.Scan(&o.ID, &o.AccountID, &o.InstrumentID, &o.ClientOrderID,
		&side, &otype, &qty, &quote, &price, &stop, &display, &tif, &status,
		&filled, &avg, &o.ShardID, &o.BookSeq, &o.OrderSeq,
		&o.PostOnly, &o.ReduceOnly, &o.STPMode, &o.SessionID,
		&o.OcoGroupID,
		&o.CreatedAt, &o.UpdatedAt,
		&o.PegMode, &pegOff, &pegLim,
		&o.TriggerSource, &o.Hidden, &o.GSLO, &o.FixingBenchmark,
		&o.AlgoType, &algoParams, &o.CoDExempt, &discOff,
		&o.GTDExpiry)
	if err != nil {
		return nil, err
	}
	o.Side, o.OrderType, o.TimeInForce, o.Status = side, otype, tif, status
	o.Quantity = decimal.RequireFromString(qty)
	o.FilledQty = decimal.RequireFromString(filled)
	o.QuoteQuantity = mustParseDecPtr(quote)
	o.Price = mustParseDecPtr(price)
	o.StopPrice = mustParseDecPtr(stop)
	o.DisplayQty = mustParseDecPtr(display)
	o.AvgFillPrice = mustParseDecPtr(avg)
	o.PegOffset = mustParseDecPtr(pegOff)
	o.PegLimit = mustParseDecPtr(pegLim)
	// discretionary_offset_pips is NOT NULL DEFAULT 0.0 — nil the zero so
	// the view only surfaces an actual band.
	if discOff != "" && discOff != "0" && discOff != "0.0" {
		if d, derr := decimal.NewFromString(discOff); derr == nil && d.IsPositive() {
			o.DiscretionaryOffsetPips = &d
		}
	}
	if len(algoParams) > 0 {
		o.AlgoParams = append(json.RawMessage(nil), algoParams...)
	}
	return &o, nil
}

func (s *PgStore) GetOrder(ctx context.Context, orderID int64) (*Order, error) {
	o, err := scanOrder(s.pool.QueryRow(ctx,
		`SELECT `+orderCols+` FROM orders WHERE id = $1`, orderID))
	if isNoRows(err) {
		return nil, nil
	}
	return o, err
}

// AmendCAS is the STALE_MODIFY primitive: one transaction locks the row
// (FOR UPDATE), verifies order_seq, applies the mutable fields, bumps
// seq and writes the audit rows — the compare-and-swap can never leave a
// torn read model.
func (s *PgStore) AmendCAS(ctx context.Context, orderID int64,
	expectedSeq, newSeq uint64, f AmendFields, audits []AuditEntry) (*Order, bool, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, false, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var cur uint64
	err = tx.QueryRow(ctx,
		`SELECT order_seq FROM orders WHERE id=$1 FOR UPDATE`, orderID).Scan(&cur)
	if isNoRows(err) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	if cur != expectedSeq {
		return nil, false, nil
	}
	sets := []string{"order_seq=$2", "updated_at=now()"}
	args := []any{orderID, newSeq}
	n := 2
	if f.Price != nil {
		n++
		sets = append(sets, fmt.Sprintf("price=$%d::numeric", n))
		args = append(args, f.Price.String())
	}
	if f.Quantity != nil {
		n++
		sets = append(sets, fmt.Sprintf("quantity=$%d::numeric", n))
		args = append(args, f.Quantity.String())
	}
	if f.StopPrice != nil {
		n++
		sets = append(sets, fmt.Sprintf("stop_price=$%d::numeric", n))
		args = append(args, f.StopPrice.String())
	}
	if f.DisplayQty != nil {
		n++
		sets = append(sets, fmt.Sprintf("display_qty=$%d::numeric", n))
		args = append(args, f.DisplayQty.String())
	}
	if f.TimeInForce != "" {
		n++
		sets = append(sets, fmt.Sprintf("time_in_force=$%d", n))
		args = append(args, f.TimeInForce)
	}
	if f.GTDExpiry != nil {
		n++
		sets = append(sets, fmt.Sprintf("gtd_expire_at=$%d", n))
		args = append(args, *f.GTDExpiry)
	}
	o, err := scanOrder(tx.QueryRow(ctx,
		`UPDATE orders SET `+strings.Join(sets, ", ")+
			` WHERE id=$1 RETURNING `+orderCols, args...))
	if err != nil {
		return nil, false, err
	}
	for _, e := range audits {
		if _, err := tx.Exec(ctx, `
			INSERT INTO order_audit (order_id, account_id, operation,
			    field_name, old_value, new_value, modified_by, request_id,
			    ip_address)
			VALUES ($1,$2,$3,$4,$5,$6,$7,NULLIF($8,''),NULLIF($9,''))`,
			e.OrderID, e.AccountID, e.Operation, e.FieldName,
			nilIfEmpty(e.OldValue), nilIfEmpty(e.NewValue), e.ModifiedBy,
			e.RequestID, e.IPAddress); err != nil {
			return nil, false, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, false, err
	}
	return o, true, nil
}

// RevertAmend undoes a committed CAS when the wire dispatch failed —
// restores the previous mutable fields and sequence verbatim.
func (s *PgStore) RevertAmend(ctx context.Context, prev *Order) error {
	_, err := s.pool.Exec(ctx, `
		UPDATE orders SET order_seq=$2, price=$3::numeric, quantity=$4::numeric,
		    stop_price=$5::numeric, display_qty=$6::numeric, time_in_force=$7,
		    updated_at=now()
		WHERE id=$1`,
		prev.ID, prev.OrderSeq, decPtrStr(prev.Price), prev.Quantity.String(),
		decPtrStr(prev.StopPrice), decPtrStr(prev.DisplayQty), prev.TimeInForce)
	return err
}

// MarkReserved flips a freshly-inserted FIXING order into its queued
// state (Phase-16 Task 16.3.9): RESERVED = "balance reservation posted,
// waiting for the benchmark fix". Only PENDING can queue — anything else
// means a race won first.
func (s *PgStore) MarkReserved(ctx context.Context, orderID int64) error {
	_, err := s.pool.Exec(ctx, `
		UPDATE orders SET status='RESERVED', updated_at=now()
		WHERE id=$1 AND status='PENDING'`, orderID)
	return err
}

func (s *PgStore) MarkActive(ctx context.Context, orderID int64) error {
	_, err := s.pool.Exec(ctx, `
		UPDATE orders SET status='ACTIVE', updated_at=now()
		WHERE id=$1 AND status IN ('PENDING','RESERVED')`, orderID)
	return err
}

func (s *PgStore) MarkRejected(ctx context.Context, orderID int64) error {
	_, err := s.pool.Exec(ctx, `
		UPDATE orders SET status='REJECTED', updated_at=now()
		WHERE id=$1 AND status = ANY($2)`, orderID, OpenStatuses)
	return err
}

// dedupLookupTx is DedupLookup scoped to an open transaction — the pair
// insert must resolve conflicts inside its own snapshot.
func dedupLookupTx(ctx context.Context, tx pgx.Tx, accountID int64,
	clientOrderID string) (*DedupRow, error) {
	row := &DedupRow{}
	err := tx.QueryRow(ctx, `
		SELECT account_id, client_order_id, order_id, request_hash, created_at
		FROM client_order_id_dedup
		WHERE account_id = $1 AND client_order_id = $2`,
		accountID, clientOrderID).
		Scan(&row.AccountID, &row.ClientOrderID, &row.OrderID,
			&row.RequestHash, &row.CreatedAt)
	if isNoRows(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return row, nil
}

// insertOrderInTx is the single-leg insert body shared by InsertOrderTx
// and InsertOcoPairTx: dedup fast-path → orders insert → dedup row. On a
// unique-constraint race it returns the colliding row via dedupConflict so
// the caller can replay or reject deterministically.
func insertOrderInTx(ctx context.Context, tx pgx.Tx, p InsertParams) (int64, error) {
	if p.ClientOrderID != "" {
		dup, err := dedupLookupTx(ctx, tx, p.AccountID, p.ClientOrderID)
		if err != nil {
			return 0, fmt.Errorf("dedup lookup: %w", err)
		}
		if dup != nil {
			return 0, &dedupConflict{row: dup}
		}
	}

	var id int64
	var algoParams any
	if len(p.AlgoParams) > 0 {
		// string — never []byte: under simple-protocol transports a
		// []byte arg encodes as bytea and the ::jsonb cast fails.
		algoParams = string(p.AlgoParams)
	}
	err := tx.QueryRow(ctx, `
		INSERT INTO orders (account_id, instrument_id, client_order_id,
		    side, order_type, quantity, quote_quantity, price, stop_price,
		    display_qty, time_in_force, status, shard_id, order_seq,
		    post_only, reduce_only, stp_mode, session_id, oco_group_id,
		    peg_mode, peg_offset, peg_limit, trigger_source, hidden, gslo,
		    fixing_benchmark, algo_type, algo_params, cod_exempt,
		    discretionary_offset_pips, gtd_expire_at)
		VALUES ($1,$2,NULLIF($3,''),$4,$5,$6::numeric,$7::numeric,$8::numeric,
		        $9::numeric,$10::numeric,$11,'PENDING',$12,$13,$14,$15,
		        NULLIF($16,''),NULLIF($17,''),$18,
		        NULLIF($19,''),$20::numeric,$21::numeric,
		        COALESCE(NULLIF($22,''),'LAST_PRICE'),$23,$24,
		        NULLIF($25,''),NULLIF($26,''),$27::jsonb,$28,
		        COALESCE($29::numeric,0),$30)
		RETURNING id`,
		p.AccountID, p.InstrumentID, p.ClientOrderID, p.Side, p.OrderType,
		p.Quantity.String(), decPtrStr(p.QuoteQuantity), decPtrStr(p.Price),
		decPtrStr(p.StopPrice), decPtrStr(p.DisplayQty), p.TimeInForce,
		p.ShardID, p.OrderSeq, p.PostOnly, p.ReduceOnly, p.STPMode,
		p.SessionID, p.OcoGroupID,
		p.PegMode, decPtrStr(p.PegOffset), decPtrStr(p.PegLimit),
		p.TriggerSource, p.Hidden, p.GSLO,
		p.FixingBenchmark, p.AlgoType, algoParams, p.CoDExempt,
		decPtrStr(p.DiscretionaryOffsetPips), p.GTDExpiry).
		Scan(&id)
	if err != nil {
		var pgErr *pgconn.PgError
		if stderrors.As(err, &pgErr) && pgErr.Code == pgUniqueViolation &&
			p.ClientOrderID != "" {
			// Legacy row pre-dating the dedup table (or a race between the
			// lookup and the insert) — resolve to the stored dedup row so
			// the caller still replays/rejects deterministically.
			row, lerr := dedupLookupTx(ctx, tx, p.AccountID, p.ClientOrderID)
			if lerr != nil || row == nil {
				return 0, fmt.Errorf("insert order: %w", err)
			}
			return 0, &dedupConflict{row: row}
		}
		return 0, fmt.Errorf("insert order: %w", err)
	}
	if p.ClientOrderID != "" {
		_, err = tx.Exec(ctx, `
			INSERT INTO client_order_id_dedup
			    (account_id, client_order_id, order_id, request_hash)
			VALUES ($1,$2,$3,$4)`,
			p.AccountID, p.ClientOrderID, id, p.RequestHash)
		if err != nil {
			var pgErr *pgconn.PgError
			if stderrors.As(err, &pgErr) && pgErr.Code == pgUniqueViolation {
				row, lerr := dedupLookupTx(ctx, tx, p.AccountID, p.ClientOrderID)
				if lerr != nil || row == nil {
					return 0, fmt.Errorf("dedup insert: %w", err)
				}
				return 0, &dedupConflict{row: row}
			}
			return 0, fmt.Errorf("dedup insert: %w", err)
		}
	}
	return id, nil
}

// InsertOrderTx inserts the dedup row and the order in one transaction —
// dedup first so a 23505 on it means "already submitted" (the orders
// table's own partial unique index on (account_id, client_order_id) is
// the backstop for pre-dedup rows).
func (s *PgStore) InsertOrderTx(ctx context.Context, p InsertParams) (*Order, *DedupRow, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	id, err := insertOrderInTx(ctx, tx, p)
	if err != nil {
		var c *dedupConflict
		if stderrors.As(err, &c) {
			return nil, c.row, err
		}
		return nil, nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, nil, fmt.Errorf("commit order insert: %w", err)
	}
	o, err := s.GetOrder(ctx, id)
	return o, nil, err
}

// InsertOcoPairTx persists both OCO legs + their shared oco_group_id in
// one transaction (Phase-14 Task 14.3.1, spec §6.5): both dedup checks,
// both order rows and both dedup rows commit or roll back together, so an
// engine link never outlives its rows. A dedup conflict on either leg
// aborts the pair and surfaces via dedupConflict exactly like the
// single-leg path.
func (s *PgStore) InsertOcoPairTx(ctx context.Context, groupID int64,
	a, b InsertParams) (*Order, *Order, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	a.OcoGroupID = &groupID
	b.OcoGroupID = &groupID
	ida, err := insertOrderInTx(ctx, tx, a)
	if err != nil {
		return nil, nil, err
	}
	idb, err := insertOrderInTx(ctx, tx, b)
	if err != nil {
		return nil, nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, nil, fmt.Errorf("commit oco pair insert: %w", err)
	}
	oa, err := s.GetOrder(ctx, ida)
	if err != nil {
		return nil, nil, err
	}
	ob, err := s.GetOrder(ctx, idb)
	if err != nil {
		return nil, nil, err
	}
	return oa, ob, nil
}

func decPtrStr(d *decimal.Decimal) any {
	if d == nil {
		return nil
	}
	return d.String()
}

// ListOrders runs the (created_at, id) keyset cursor with an exact total
// for the §8.8 envelope {data,next_cursor,limit,total}.
func (s *PgStore) ListOrders(ctx context.Context, q ListQuery) ([]Order, int64, error) {
	where := "account_id = $1"
	args := []any{q.AccountID}
	n := 1
	if q.InstrumentID != 0 {
		n++
		where += fmt.Sprintf(" AND instrument_id = $%d", n)
		args = append(args, q.InstrumentID)
	}
	if q.Status != "" {
		n++
		where += fmt.Sprintf(" AND status = $%d", n)
		args = append(args, q.Status)
	}
	if q.Side != "" {
		n++
		where += fmt.Sprintf(" AND side = $%d", n)
		args = append(args, q.Side)
	}
	if q.OrderType != "" {
		n++
		where += fmt.Sprintf(" AND order_type = $%d", n)
		args = append(args, q.OrderType)
	}
	if q.ClientOrderID != "" {
		n++
		where += fmt.Sprintf(" AND client_order_id = $%d", n)
		args = append(args, q.ClientOrderID)
	}
	if q.From != nil {
		n++
		where += fmt.Sprintf(" AND created_at >= $%d", n)
		args = append(args, *q.From)
	}
	if q.To != nil {
		n++
		where += fmt.Sprintf(" AND created_at <= $%d", n)
		args = append(args, *q.To)
	}
	var total int64
	if err := s.pool.QueryRow(ctx,
		`SELECT count(*) FROM orders WHERE `+where, args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	page := where
	if !q.CursorAt.IsZero() {
		n++
		page += fmt.Sprintf(" AND (created_at, id) < ($%d, $%d)", n, n+1)
		args = append(args, q.CursorAt, q.CursorID)
		n++
	}
	n++
	page += fmt.Sprintf(" ORDER BY created_at DESC, id DESC LIMIT $%d", n)
	args = append(args, q.Limit+1) // +1 → next_cursor
	rows, err := s.pool.Query(ctx,
		`SELECT `+orderCols+` FROM orders WHERE `+page, args...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	var out []Order
	for rows.Next() {
		o, err := scanOrder(rows)
		if err != nil {
			return nil, 0, err
		}
		out = append(out, *o)
	}
	return out, total, rows.Err()
}

// OpenOrders selects cancellable orders matching the mass-cancel scope.
// The caller's account is always part of the scope on client paths; the
// admin path may pass AccountID=0 for cross-account scope.
func (s *PgStore) OpenOrders(ctx context.Context, scope MassCancelScope) ([]Order, error) {
	where := "status = ANY($1)"
	args := []any{OpenStatuses}
	n := 1
	if scope.AccountID != 0 {
		n++
		where += fmt.Sprintf(" AND account_id = $%d", n)
		args = append(args, scope.AccountID)
	}
	if scope.InstrumentID != 0 {
		n++
		where += fmt.Sprintf(" AND instrument_id = $%d", n)
		args = append(args, scope.InstrumentID)
	}
	if scope.Side != "" {
		n++
		where += fmt.Sprintf(" AND side = $%d", n)
		args = append(args, scope.Side)
	}
	if scope.OrderType != "" {
		n++
		where += fmt.Sprintf(" AND order_type = $%d", n)
		args = append(args, scope.OrderType)
	}
	if scope.SessionID != "" {
		n++
		where += fmt.Sprintf(" AND session_id = $%d", n)
		args = append(args, scope.SessionID)
	}
	// spec §9.9 + migration 229 — only the session-scope
	// cancel-on-disconnect sweep honours cod_exempt; every other
	// mass-cancel reason (dead-man, admin, close-all) takes the order.
	if scope.Reason == "cancel_on_disconnect" {
		where += " AND cod_exempt = false"
	}
	rows, err := s.pool.Query(ctx,
		`SELECT `+orderCols+` FROM orders WHERE `+where+` ORDER BY id`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Order
	for rows.Next() {
		o, err := scanOrder(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *o)
	}
	return out, rows.Err()
}

// ApplyCancel marks an open order CANCELLED; terminal orders are left
// untouched (racing fills win — the engine's single-thread order decides
// truth, the read model follows).
func (s *PgStore) ApplyCancel(ctx context.Context, orderID int64) error {
	_, err := s.pool.Exec(ctx, `
		UPDATE orders SET status='CANCELLED', updated_at=now()
		WHERE id=$1 AND status = ANY($2)`, orderID, OpenStatuses)
	return err
}

// ApplyCancelReason is the engine-echo variant: reason
// CancelReasonExpired (GTD/DAY expiry or the spec §27 R8 90-day
// auto-expire) lands the row EXPIRED, every other reason CANCELLED.
// Read-model only — the engine's cancel is already authoritative.
func (s *PgStore) ApplyCancelReason(ctx context.Context, orderID int64,
	reason uint8) error {
	status := "CANCELLED"
	if reason == CancelReasonExpired {
		status = "EXPIRED"
	}
	_, err := s.pool.Exec(ctx, `
		UPDATE orders SET status=$3::order_status_enum, updated_at=now()
		WHERE id=$1 AND status = ANY($2)`, orderID, OpenStatuses, status)
	return err
}

// ApplyFill folds an engine TradeFill into the read model: filled_qty
// accumulates, avg_fill_price becomes the volume-weighted average, and
// status flips to PARTIALLY_FILLED / FILLED.
func (s *PgStore) ApplyFill(ctx context.Context, orderID int64, price, qty decimal.Decimal) error {
	_, err := s.pool.Exec(ctx, `
		UPDATE orders SET
		    filled_qty = filled_qty + $2::numeric,
		    avg_fill_price = (
		        (COALESCE(avg_fill_price,0) * filled_qty + $3::numeric * $2::numeric)
		        / NULLIF(filled_qty + $2::numeric, 0)),
		    status = (CASE
		        WHEN filled_qty + $2::numeric >= quantity THEN 'FILLED'
		        ELSE 'PARTIALLY_FILLED' END)::order_status_enum,
		    updated_at = now()
		WHERE id = $1 AND status <> 'FILLED'`,
		orderID, qty.String(), price.String())
	return err
}

func (s *PgStore) WriteAudit(ctx context.Context, entries []AuditEntry) error {
	if len(entries) == 0 {
		return nil
	}
	b := &pgx.Batch{}
	for _, e := range entries {
		b.Queue(`
			INSERT INTO order_audit (order_id, account_id, operation,
			    field_name, old_value, new_value, modified_by, request_id,
			    ip_address)
			VALUES ($1,$2,$3,$4,$5,$6,$7,NULLIF($8,''),NULLIF($9,''))`,
			e.OrderID, e.AccountID, e.Operation, e.FieldName,
			nilIfEmpty(e.OldValue), nilIfEmpty(e.NewValue), e.ModifiedBy,
			e.RequestID, e.IPAddress)
	}
	return s.pool.SendBatch(ctx, b).Close()
}

func nilIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// AuditTrail returns the full modify history for the admin endpoint.
func (s *PgStore) AuditTrail(ctx context.Context, orderID int64) ([]AuditEntry, error) {
	return s.auditQuery(ctx,
		`SELECT order_id, account_id, operation, field_name,
		        COALESCE(old_value,''), COALESCE(new_value,''), modified_by,
		        COALESCE(request_id,''), COALESCE(ip_address,''), modified_at
		 FROM order_audit WHERE order_id=$1 ORDER BY audit_id`, orderID)
}

// Amendments is the client-visible subset: MODIFY/AMEND/CANCEL_REPLACE.
func (s *PgStore) Amendments(ctx context.Context, orderID int64) ([]AuditEntry, error) {
	return s.auditQuery(ctx, `
		SELECT order_id, account_id, operation, field_name,
		       COALESCE(old_value,''), COALESCE(new_value,''), modified_by,
		       COALESCE(request_id,''), COALESCE(ip_address,''), modified_at
		FROM order_audit
		WHERE order_id=$1 AND operation IN ('MODIFY','AMEND','CANCEL_REPLACE')
		ORDER BY audit_id`, orderID)
}

func (s *PgStore) auditQuery(ctx context.Context, q string, arg int64) ([]AuditEntry, error) {
	rows, err := s.pool.Query(ctx, q, arg)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AuditEntry
	for rows.Next() {
		var e AuditEntry
		if err := rows.Scan(&e.OrderID, &e.AccountID, &e.Operation,
			&e.FieldName, &e.OldValue, &e.NewValue, &e.ModifiedBy,
			&e.RequestID, &e.IPAddress, &e.ModifiedAt); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

func (s *PgStore) IdemLookup(ctx context.Context, accountID int64, key string) (*IdemRow, error) {
	row := &IdemRow{}
	err := s.pool.QueryRow(ctx, `
		SELECT request_hash, status_code, response_body, created_at
		FROM idempotency_keys
		WHERE account_id=$1 AND idempotency_key=$2`,
		accountID, key).Scan(&row.RequestHash, &row.StatusCode,
		&row.ResponseBody, &row.CreatedAt)
	if isNoRows(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return row, nil
}

func (s *PgStore) IdemStore(ctx context.Context, accountID int64, key, endpoint, reqHash string,
	status int, body []byte) error {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO idempotency_keys (account_id, idempotency_key, endpoint,
		    request_hash, status_code, response_body)
		VALUES ($1,$2,$3,$4,$5,$6)
		ON CONFLICT (account_id, idempotency_key) DO NOTHING`,
		accountID, key, endpoint, reqHash, status, body)
	return err
}

// compile-time check.
var _ Store = (*PgStore)(nil)
