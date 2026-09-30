// Task 19.3.7 — Prime-Broker pre-trade credit limits (spec §5.22/§13.7,
// NOP + DSL). Institutional PB flow ONLY: an account with no
// pb_credit_limits row is not a PB client and the gate is a no-op for it.
//
// Semantics (fail-closed §2.7):
//
//	NOP — net open position. Utilization =
//	      current_net_open_position = booked net exposure (USD) +
//	      Σ ACTIVE reservations for the scope. An accepted order debits
//	      the full USD notional: the conservative bound (a fill is
//	      exposure; netting only lowers it later via SyncNetOpenPosition).
//	DSL — daily settled limit. Utilization =
//	      current_daily_settled = today's executed+reserved USD notional;
//	      reset at value-date rollover via ResetDailySettled.
//
// Reservations are PG-authoritative: one SERIALIZABLE-able transaction
// SELECTs each limit row FOR UPDATE, checks headroom, debits both
// counters and inserts pb_credit_reservations (migration 232). Release
// (cancel/reject/expiry) credits counters back exactly; consume (fill)
// converts the reservation into utilization. The pb_credit:{pb}:{client}
// Redis hash is a post-commit mirror for low-latency utilization reads
// and the 90% alert stream — PG remains authoritative; a Redis outage
// never widens credit.
package risk

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"exchange/internal/position"
	excredis "exchange/internal/redis"
	"exchange/pkg/decimal"
	excerrors "exchange/pkg/errors"
)

// PBUtilizationAlertPct is the spec §13.7 / task telemetry threshold —
// utilization ≥90% of either bound pages the PB credit officer.
const PBUtilizationAlertPct = 90

// PBCreditLimit mirrors one pb_credit_limits row (migration 037).
// CurrencyPair nil = global scope; NOPLimit/DSLLimit nil = uncapped bound.
type PBCreditLimit struct {
	ID              int64
	PrimeBrokerID   int64
	ClientAccountID int64
	CurrencyPair    *string // compact form, e.g. "EURUSD"; NULL = global
	NOPLimit        *decimal.Decimal
	CurrentNOP      decimal.Decimal
	DSLLimit        *decimal.Decimal
	CurrentDSL      decimal.Decimal
	UpdatedAt       time.Time
}

// Scope returns "GLOBAL" or the compact currency pair.
func (l PBCreditLimit) Scope() string {
	if l.CurrencyPair == nil || *l.CurrencyPair == "" {
		return "GLOBAL"
	}
	return *l.CurrencyPair
}

// PBDebit is one per-scope reservation debit applied atomically.
type PBDebit struct {
	LimitID int64
	Amount  decimal.Decimal // USD
}

// PBReservation mirrors one ACTIVE/HISTORICAL pb_credit_reservations row.
type PBReservation struct {
	ID              int64
	OrderID         int64
	ClientAccountID int64
	LimitID         int64
	ReservedAmount  decimal.Decimal
	Status          string // ACTIVE | CONSUMED | RELEASED
}

// PBBreachError carries which bound failed inside the debit transaction —
// the service maps NOP→PB_NOP_LIMIT_EXCEEDED / DSL→PB_DSL_LIMIT_EXCEEDED.
type PBBreachError struct {
	Bound     string // "NOP" or "DSL"
	Scope     string
	LimitID   int64
	Limit     decimal.Decimal
	Current   decimal.Decimal
	Requested decimal.Decimal
}

func (e *PBBreachError) Error() string {
	return fmt.Sprintf("pb %s limit breached (scope %s limit %s used %s +req %s)",
		e.Bound, e.Scope, e.Limit.String(), e.Current.String(), e.Requested.String())
}

// PBInstrumentExposure is one position's signed USD-relevant notional for
// NOP re-computation.
type PBInstrumentExposure struct {
	InstrumentID   int64
	CurrencyPair   string          // compact "EURUSD"
	QuoteCurrency  string          // e.g. "USD"
	SignedNotional decimal.Decimal // signed qty × mark, in QUOTE ccy
}

// PBCreditStore is the persistence seam — PgPBCreditStore is the
// production impl; tests inject fakes.
type PBCreditStore interface {
	LimitsFor(ctx context.Context, clientID int64) ([]PBCreditLimit, error)
	// ReserveTx debits every listed limit row (NOP+DSL counters) and
	// inserts one reservation row each, all-or-nothing. A breach aborts
	// the tx and returns *PBBreachError — nothing is debited.
	ReserveTx(ctx context.Context, orderID, clientID int64, debits []PBDebit) error
	// ReleaseTx credits back every ACTIVE reservation of the order
	// (cancel/reject/expiry). Idempotent — no ACTIVE rows is a no-op.
	// Returns the released rows for the Redis mirror.
	ReleaseTx(ctx context.Context, orderID int64) ([]PBReservation, error)
	// ConsumeTx marks the order's reservations CONSUMED — counters keep
	// the debit because the position/settlement now carries it.
	ConsumeTx(ctx context.Context, orderID int64) error
	// AdjustTx re-sizes an order's reservations by deltaUSD (positive =
	// debit more headroom → re-checks limits; negative = credit back).
	// *PBBreachError on limit breach (no partial apply).
	AdjustTx(ctx context.Context, orderID int64, deltaUSD decimal.Decimal) error
	// ReleaseTerminalTx releases reservations whose order is in a
	// terminal state — the crash/expiry sweep.
	ReleaseTerminalTx(ctx context.Context) (int64, error)
	// PositionExposure aggregates open positions per instrument for NOP
	// re-computation (signed quote-ccy notional at mark).
	PositionExposure(ctx context.Context, clientID int64) ([]PBInstrumentExposure, error)
	// SyncNOPTx overwrites current_net_open_position with the supplied
	// booked exposures (per-scope USD) + active reservations.
	SyncNOPTx(ctx context.Context, clientID int64, globalUSD decimal.Decimal,
		pairUSD map[string]decimal.Decimal) error
	// ResetDailySettled zeroes every current_daily_settled — value-date
	// rollover.
	ResetDailySettled(ctx context.Context) (int64, error)
	// Admin CRUD.
	UpsertLimit(ctx context.Context, l PBCreditLimit) (PBCreditLimit, error)
	DeleteLimit(ctx context.Context, id int64) error
	ListLimits(ctx context.Context, clientID int64) ([]PBCreditLimit, error)
	ReservationsFor(ctx context.Context, orderID int64) ([]PBReservation, error)
}

// ---------------------------------------------------------------------------
// Pg implementation
// ---------------------------------------------------------------------------

// PgPBCreditStore is the production PBCreditStore.
type PgPBCreditStore struct{ P *pgxpool.Pool }

func NewPgPBCreditStore(p *pgxpool.Pool) *PgPBCreditStore {
	return &PgPBCreditStore{P: p}
}

func normPair(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z':
			b.WriteRune(r - 'a' + 'A')
		case r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			b.WriteRune(r)
		}
	}
	return b.String()
}

func scanLimit(row pgx.Row) (PBCreditLimit, error) {
	var l PBCreditLimit
	var nop, dsl *decimal.Decimal
	err := row.Scan(&l.ID, &l.PrimeBrokerID, &l.ClientAccountID, &l.CurrencyPair,
		&nop, &l.CurrentNOP, &dsl, &l.CurrentDSL, &l.UpdatedAt)
	l.NOPLimit, l.DSLLimit = nop, dsl
	return l, err
}

const pbLimitCols = `id, prime_broker_id, client_account_id, currency_pair,
    net_open_position_limit, current_net_open_position,
    daily_settled_limit, current_daily_settled, updated_at`

func (s *PgPBCreditStore) LimitsFor(ctx context.Context, clientID int64) ([]PBCreditLimit, error) {
	rows, err := s.P.Query(ctx, `
		SELECT `+pbLimitCols+` FROM pb_credit_limits
		WHERE client_account_id = $1 ORDER BY id`, clientID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []PBCreditLimit
	for rows.Next() {
		l, err := scanLimit(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

// ReserveTx — the atomic check-and-debit. Each row is SELECTed FOR UPDATE,
// then both bounds verified before the UPDATE; a breach anywhere aborts
// the whole transaction (orders never partially reserve).
func (s *PgPBCreditStore) ReserveTx(ctx context.Context, orderID, clientID int64,
	debits []PBDebit) error {
	tx, err := s.P.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	for _, d := range debits {
		var l PBCreditLimit
		var nopLim, dslLim *decimal.Decimal
		var scope *string
		if err := tx.QueryRow(ctx, `
			SELECT `+pbLimitCols+` FROM pb_credit_limits
			WHERE id = $1 FOR UPDATE`, d.LimitID).Scan(
			&l.ID, &l.PrimeBrokerID, &l.ClientAccountID, &scope,
			&nopLim, &l.CurrentNOP, &dslLim, &l.CurrentDSL, &l.UpdatedAt); err != nil {
			return err
		}
		l.CurrencyPair, l.NOPLimit, l.DSLLimit = scope, nopLim, dslLim
		if l.NOPLimit != nil && l.CurrentNOP.Add(d.Amount).Cmp(*l.NOPLimit) > 0 {
			return &PBBreachError{Bound: "NOP", Scope: l.Scope(), LimitID: l.ID,
				Limit: *l.NOPLimit, Current: l.CurrentNOP, Requested: d.Amount}
		}
		if l.DSLLimit != nil && l.CurrentDSL.Add(d.Amount).Cmp(*l.DSLLimit) > 0 {
			return &PBBreachError{Bound: "DSL", Scope: l.Scope(), LimitID: l.ID,
				Limit: *l.DSLLimit, Current: l.CurrentDSL, Requested: d.Amount}
		}
		if _, err := tx.Exec(ctx, `
			UPDATE pb_credit_limits SET
			    current_net_open_position = current_net_open_position + $2,
			    current_daily_settled     = current_daily_settled + $2,
			    updated_at = now()
			WHERE id = $1`, d.LimitID, d.Amount); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO pb_credit_reservations
			    (order_id, client_account_id, limit_id, reserved_amount)
			VALUES ($1, $2, $3, $4)`,
			orderID, clientID, d.LimitID, d.Amount); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

func (s *PgPBCreditStore) ReleaseTx(ctx context.Context, orderID int64) ([]PBReservation, error) {
	tx, err := s.P.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	rows, err := tx.Query(ctx, `
		SELECT id, limit_id, reserved_amount FROM pb_credit_reservations
		WHERE order_id = $1 AND status = 'ACTIVE' FOR UPDATE`, orderID)
	if err != nil {
		return nil, err
	}
	type rel struct {
		resID, limitID int64
		amt            decimal.Decimal
	}
	var rels []rel
	for rows.Next() {
		var r rel
		if err := rows.Scan(&r.resID, &r.limitID, &r.amt); err != nil {
			rows.Close()
			return nil, err
		}
		rels = append(rels, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	var out []PBReservation
	for _, r := range rels {
		if _, err := tx.Exec(ctx, `
			UPDATE pb_credit_limits SET
			    current_net_open_position = GREATEST(current_net_open_position - $2, 0),
			    current_daily_settled     = GREATEST(current_daily_settled - $2, 0),
			    updated_at = now()
			WHERE id = $1`, r.limitID, r.amt); err != nil {
			return nil, err
		}
		if _, err := tx.Exec(ctx, `
			UPDATE pb_credit_reservations SET status='RELEASED', released_at=now()
			WHERE id=$1`, r.resID); err != nil {
			return nil, err
		}
		out = append(out, PBReservation{ID: r.resID, OrderID: orderID,
			LimitID: r.limitID, ReservedAmount: r.amt, Status: "RELEASED"})
	}
	return out, tx.Commit(ctx)
}

func (s *PgPBCreditStore) ConsumeTx(ctx context.Context, orderID int64) error {
	// Counters stay debited — the open position / day's settled notional
	// now carries the utilization.
	_, err := s.P.Exec(ctx, `
		UPDATE pb_credit_reservations SET status='CONSUMED', released_at=now()
		WHERE order_id = $1 AND status = 'ACTIVE'`, orderID)
	return err
}

// AdjustTx re-sizes reservations for order amendments. Positive delta
// debits every ACTIVE row's scope (re-checking limits); negative delta
// credits back row-by-row up to each row's reserved_amount.
func (s *PgPBCreditStore) AdjustTx(ctx context.Context, orderID int64,
	deltaUSD decimal.Decimal) error {
	tx, err := s.P.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	rows, err := tx.Query(ctx, `
		SELECT id, limit_id, reserved_amount FROM pb_credit_reservations
		WHERE order_id=$1 AND status='ACTIVE' ORDER BY id FOR UPDATE`, orderID)
	if err != nil {
		return err
	}
	type res struct {
		id, limitID int64
		amt         decimal.Decimal
	}
	var rs []res
	for rows.Next() {
		var r res
		if err := rows.Scan(&r.id, &r.limitID, &r.amt); err != nil {
			rows.Close()
			return err
		}
		rs = append(rs, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	if len(rs) == 0 {
		return tx.Commit(ctx) // no reservation to adjust — no-op
	}
	if deltaUSD.IsPositive() {
		for _, r := range rs {
			var l PBCreditLimit
			var nopLim, dslLim *decimal.Decimal
			var scope *string
			if err := tx.QueryRow(ctx, `
				SELECT `+pbLimitCols+` FROM pb_credit_limits
				WHERE id=$1 FOR UPDATE`, r.limitID).Scan(
				&l.ID, &l.PrimeBrokerID, &l.ClientAccountID, &scope,
				&nopLim, &l.CurrentNOP, &dslLim, &l.CurrentDSL, &l.UpdatedAt); err != nil {
				return err
			}
			l.CurrencyPair, l.NOPLimit, l.DSLLimit = scope, nopLim, dslLim
			if l.NOPLimit != nil && l.CurrentNOP.Add(deltaUSD).Cmp(*l.NOPLimit) > 0 {
				return &PBBreachError{Bound: "NOP", Scope: l.Scope(), LimitID: l.ID,
					Limit: *l.NOPLimit, Current: l.CurrentNOP, Requested: deltaUSD}
			}
			if l.DSLLimit != nil && l.CurrentDSL.Add(deltaUSD).Cmp(*l.DSLLimit) > 0 {
				return &PBBreachError{Bound: "DSL", Scope: l.Scope(), LimitID: l.ID,
					Limit: *l.DSLLimit, Current: l.CurrentDSL, Requested: deltaUSD}
			}
			if _, err := tx.Exec(ctx, `
				UPDATE pb_credit_limits SET
				    current_net_open_position = current_net_open_position + $2,
				    current_daily_settled     = current_daily_settled + $2,
				    updated_at = now()
				WHERE id=$1`, r.limitID, deltaUSD); err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `
				UPDATE pb_credit_reservations SET reserved_amount = reserved_amount + $2
				WHERE id=$1`, r.id, deltaUSD); err != nil {
				return err
			}
		}
		return tx.Commit(ctx)
	}
	// Negative delta — credit back, never below zero per row.
	remaining := deltaUSD.Neg()
	for _, r := range rs {
		if !remaining.IsPositive() {
			break
		}
		back := r.amt
		if back.Cmp(remaining) > 0 {
			back = remaining
		}
		if _, err := tx.Exec(ctx, `
			UPDATE pb_credit_limits SET
			    current_net_open_position = GREATEST(current_net_open_position - $2, 0),
			    current_daily_settled     = GREATEST(current_daily_settled - $2, 0),
			    updated_at = now()
			WHERE id=$1`, r.limitID, back); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `
			UPDATE pb_credit_reservations SET reserved_amount = reserved_amount - $2
			WHERE id=$1`, r.id, back); err != nil {
			return err
		}
		remaining = remaining.Sub(back)
	}
	return tx.Commit(ctx)
}

func (s *PgPBCreditStore) ReleaseTerminalTx(ctx context.Context) (int64, error) {
	// Two steps inside one tx: collect ACTIVE reservations on terminal
	// orders, then release through the same counter credit path.
	tx, err := s.P.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	rows, err := tx.Query(ctx, `
		SELECT r.id, r.limit_id, r.reserved_amount
		FROM pb_credit_reservations r
		JOIN orders o ON o.id = r.order_id
		WHERE r.status = 'ACTIVE'
		  AND o.status IN ('FILLED','CANCELLED','REJECTED','EXPIRED')
		FOR UPDATE OF r`)
	if err != nil {
		return 0, err
	}
	type rel struct {
		resID, limitID int64
		amt            decimal.Decimal
	}
	var rels []rel
	for rows.Next() {
		var r rel
		if err := rows.Scan(&r.resID, &r.limitID, &r.amt); err != nil {
			rows.Close()
			return 0, err
		}
		rels = append(rels, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}
	for _, r := range rels {
		if _, err := tx.Exec(ctx, `
			UPDATE pb_credit_limits SET
			    current_net_open_position = GREATEST(current_net_open_position - $2, 0),
			    current_daily_settled     = GREATEST(current_daily_settled - $2, 0),
			    updated_at = now()
			WHERE id=$1`, r.limitID, r.amt); err != nil {
			return 0, err
		}
		if _, err := tx.Exec(ctx, `
			UPDATE pb_credit_reservations SET status='RELEASED', released_at=now()
			WHERE id=$1`, r.resID); err != nil {
			return 0, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, err
	}
	return int64(len(rels)), nil
}

func (s *PgPBCreditStore) PositionExposure(ctx context.Context,
	clientID int64) ([]PBInstrumentExposure, error) {
	rows, err := s.P.Query(ctx, `
		SELECT p.instrument_id, i.symbol, i.quote_currency,
		       SUM(CASE WHEN p.side='LONG' THEN p.quantity ELSE -p.quantity END
		           * COALESCE(p.mark_price, p.entry_price))
		FROM positions p JOIN instruments i ON i.id = p.instrument_id
		WHERE p.account_id = $1
		GROUP BY p.instrument_id, i.symbol, i.quote_currency`, clientID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []PBInstrumentExposure
	for rows.Next() {
		var e PBInstrumentExposure
		if err := rows.Scan(&e.InstrumentID, &e.CurrencyPair, &e.QuoteCurrency,
			&e.SignedNotional); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

func (s *PgPBCreditStore) SyncNOPTx(ctx context.Context, clientID int64,
	globalUSD decimal.Decimal, pairUSD map[string]decimal.Decimal) error {
	tx, err := s.P.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	rows, err := tx.Query(ctx, `
		SELECT id, currency_pair FROM pb_credit_limits
		WHERE client_account_id=$1 FOR UPDATE`, clientID)
	if err != nil {
		return err
	}
	type lim struct {
		id   int64
		pair *string
	}
	var ls []lim
	for rows.Next() {
		var l lim
		if err := rows.Scan(&l.id, &l.pair); err != nil {
			rows.Close()
			return err
		}
		ls = append(ls, l)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, l := range ls {
		booked := globalUSD
		if l.pair != nil && *l.pair != "" {
			booked = pairUSD[normPair(*l.pair)]
		}
		if _, err := tx.Exec(ctx, `
			UPDATE pb_credit_limits SET
			    current_net_open_position = $2 + COALESCE((
			        SELECT SUM(r.reserved_amount) FROM pb_credit_reservations r
			        WHERE r.limit_id = $1 AND r.status = 'ACTIVE'), 0),
			    updated_at = now()
			WHERE id=$1`, l.id, booked.Abs()); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

func (s *PgPBCreditStore) ResetDailySettled(ctx context.Context) (int64, error) {
	tag, err := s.P.Exec(ctx, `
		UPDATE pb_credit_limits SET current_daily_settled = 0, updated_at = now()
		WHERE current_daily_settled <> 0`)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

func (s *PgPBCreditStore) UpsertLimit(ctx context.Context, l PBCreditLimit) (PBCreditLimit, error) {
	var pair any
	if l.CurrencyPair != nil && *l.CurrencyPair != "" {
		pair = normPair(*l.CurrencyPair)
	}
	var nop, dsl any
	if l.NOPLimit != nil {
		nop = *l.NOPLimit
	}
	if l.DSLLimit != nil {
		dsl = *l.DSLLimit
	}
	var out PBCreditLimit
	var nopLim, dslLim *decimal.Decimal
	var scope *string
	err := s.P.QueryRow(ctx, `
		INSERT INTO pb_credit_limits
		    (prime_broker_id, client_account_id, currency_pair,
		     net_open_position_limit, daily_settled_limit)
		VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (prime_broker_id, client_account_id, COALESCE(currency_pair, ''))
		DO UPDATE SET net_open_position_limit = EXCLUDED.net_open_position_limit,
		              daily_settled_limit     = EXCLUDED.daily_settled_limit,
		              updated_at = now()
		RETURNING `+pbLimitCols,
		l.PrimeBrokerID, l.ClientAccountID, pair, nop, dsl).Scan(
		&out.ID, &out.PrimeBrokerID, &out.ClientAccountID, &scope,
		&nopLim, &out.CurrentNOP, &dslLim, &out.CurrentDSL, &out.UpdatedAt)
	out.CurrencyPair, out.NOPLimit, out.DSLLimit = scope, nopLim, dslLim
	return out, err
}

func (s *PgPBCreditStore) DeleteLimit(ctx context.Context, id int64) error {
	tag, err := s.P.Exec(ctx, `DELETE FROM pb_credit_limits WHERE id=$1`, id)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return excerrors.New("RISK_LIMITS_NOT_FOUND",
			fmt.Sprintf("pb credit limit %d not found", id))
	}
	return nil
}

func (s *PgPBCreditStore) ListLimits(ctx context.Context, clientID int64) ([]PBCreditLimit, error) {
	q := `SELECT ` + pbLimitCols + ` FROM pb_credit_limits`
	args := []any{}
	if clientID > 0 {
		q += ` WHERE client_account_id = $1`
		args = append(args, clientID)
	}
	q += ` ORDER BY client_account_id, id`
	rows, err := s.P.Query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []PBCreditLimit
	for rows.Next() {
		l, err := scanLimit(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

func (s *PgPBCreditStore) ReservationsFor(ctx context.Context, orderID int64) ([]PBReservation, error) {
	rows, err := s.P.Query(ctx, `
		SELECT id, order_id, client_account_id, limit_id, reserved_amount, status
		FROM pb_credit_reservations WHERE order_id=$1`, orderID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []PBReservation
	for rows.Next() {
		var r PBReservation
		if err := rows.Scan(&r.ID, &r.OrderID, &r.ClientAccountID, &r.LimitID,
			&r.ReservedAmount, &r.Status); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// ---------------------------------------------------------------------------
// Service — the orders.Options PBCreditGate impl
// ---------------------------------------------------------------------------

// PBUtilization is the telemetry/alert view of one limit row.
type PBUtilization struct {
	Limit   PBCreditLimit
	NOPPct  decimal.Decimal // 0 when uncapped
	DSLPct  decimal.Decimal
	Alerted bool // ≥ PBUtilizationAlertPct on either bound
}

// PBAlert is emitted once per (limit, bound) crossing of the 90% band.
type PBAlert struct {
	Limit PBCreditLimit
	Bound string // "NOP" or "DSL"
	Pct   decimal.Decimal
	At    time.Time
}

// PBCreditService enforces §13.7 NOP/DSL before order admission.
// Construct via NewPBCreditService; safe for concurrent use.
type PBCreditService struct {
	store PBCreditStore
	conv  *position.Converter
	rdb   *excredis.Client // best-effort utilization mirror; nil disables
	now   func() time.Time

	// OnAlert fires once per (limit,bound) 90% crossing — wire to the
	// Phase-07 alert taxonomy / Risk Manager notification bus.
	OnAlert func(ctx context.Context, a PBAlert)
}

// NewPBCreditService — conv converts quote-ccy notional to USD (identity
// for USD quotes). nil store fails closed (gate treats every order as
// unconfigured → allowed only when LimitsFor returns zero rows; a store
// error rejects admission).
func NewPBCreditService(store PBCreditStore, conv *position.Converter,
	rdb *excredis.Client) *PBCreditService {
	if conv == nil {
		conv = position.NewConverter(nil, "")
	}
	return &PBCreditService{store: store, conv: conv, rdb: rdb,
		now: func() time.Time { return time.Now().UTC() }}
}

// toUSD converts quote-ccy notional to USD at the mark mid — identity for
// USD. Conversion failure fails closed (cannot size the credit exposure).
func (s *PBCreditService) toUSD(ctx context.Context, amount decimal.Decimal,
	quoteCcy string) (decimal.Decimal, error) {
	q := strings.ToUpper(strings.TrimSpace(quoteCcy))
	if q == "" || q == "USD" {
		return amount.Abs(), nil
	}
	c, err := s.conv.Convert(ctx, amount.Abs(), q, "USD")
	if err != nil {
		return decimal.Zero, excerrors.Wrap("RISK_LIMITS_INTERNAL",
			fmt.Sprintf("pb credit: %s→USD mark unavailable", q), err)
	}
	return c.ToAmount, nil
}

// ReserveHeadroom implements orders.PBCreditGate. Steps:
//  1. load the client's pb_credit_limits rows — zero rows ⇒ not a PB
//     client ⇒ admit without reservation;
//  2. pick applicable scopes: the GLOBAL row plus the pair-scoped row
//     matching the instrument;
//  3. atomic ReserveTx debit; breach → mapped coded error.
//
// notional is in quoteCcy (qty × eval price as computed by Submit).
func (s *PBCreditService) ReserveHeadroom(ctx context.Context, orderID, accountID int64,
	symbol, quoteCcy string, notional decimal.Decimal) error {
	if s.store == nil {
		return excerrors.New("RISK_LIMITS_INTERNAL",
			"pb credit store not configured")
	}
	limits, err := s.store.LimitsFor(ctx, accountID)
	if err != nil {
		return excerrors.Wrap("RISK_LIMITS_INTERNAL", "pb credit limits", err)
	}
	if len(limits) == 0 {
		return nil // not a PB-managed account
	}
	usd, err := s.toUSD(ctx, notional, quoteCcy)
	if err != nil {
		return err
	}
	pair := normPair(symbol)
	var debits []PBDebit
	for _, l := range limits {
		if l.CurrencyPair == nil || *l.CurrencyPair == "" || normPair(*l.CurrencyPair) == pair {
			debits = append(debits, PBDebit{LimitID: l.ID, Amount: usd})
		}
	}
	if len(debits) == 0 {
		return nil // limits exist but none apply to this pair
	}
	if err := s.store.ReserveTx(ctx, orderID, accountID, debits); err != nil {
		var be *PBBreachError
		if errors.As(err, &be) {
			return s.breachError(be)
		}
		return excerrors.Wrap("RISK_LIMITS_INTERNAL", "pb credit reserve", err)
	}
	s.mirror(ctx, limits, debits)
	return nil
}

func (s *PBCreditService) breachError(be *PBBreachError) error {
	code := "PB_NOP_LIMIT_EXCEEDED"
	if be.Bound == "DSL" {
		code = "PB_DSL_LIMIT_EXCEEDED"
	}
	return excerrors.New(code, fmt.Sprintf(
		"pb %s credit limit breached (scope %s): limit %s, current %s, requested %s",
		be.Bound, be.Scope, be.Limit.String(), be.Current.String(), be.Requested.String()))
}

// ReleaseHeadroom — orders.PBCreditGate terminal hook (cancel / reject /
// expire). Idempotent.
func (s *PBCreditService) ReleaseHeadroom(ctx context.Context, orderID int64) error {
	if s.store == nil {
		return excerrors.New("RISK_LIMITS_INTERNAL", "pb credit store not configured")
	}
	if _, err := s.store.ReleaseTx(ctx, orderID); err != nil {
		return excerrors.Wrap("RISK_LIMITS_INTERNAL", "pb credit release", err)
	}
	return nil
}

// OnFill converts the order's ACTIVE reservations into consumed
// utilization — NOP exposure is now carried by the position (and stays
// debited until SyncNetOpenPosition re-bases it), DSL stays because the
// fill settles inside today's value date. Safe to call on every fill —
// the ACTIVE→CONSUMED transition is once-only per reservation row.
func (s *PBCreditService) OnFill(ctx context.Context, orderID int64) error {
	if s.store == nil {
		return excerrors.New("RISK_LIMITS_INTERNAL", "pb credit store not configured")
	}
	if err := s.store.ConsumeTx(ctx, orderID); err != nil {
		return excerrors.Wrap("RISK_LIMITS_INTERNAL", "pb credit consume", err)
	}
	return nil
}

// AdjustHeadroom re-sizes reservations on order amend (qty-up re-checks
// limits; qty-down credits back). deltaNotional is in quoteCcy, signed.
func (s *PBCreditService) AdjustHeadroom(ctx context.Context, orderID int64,
	symbol, quoteCcy string, deltaNotional decimal.Decimal) error {
	if s.store == nil {
		return excerrors.New("RISK_LIMITS_INTERNAL", "pb credit store not configured")
	}
	res, err := s.store.ReservationsFor(ctx, orderID)
	if err != nil {
		return excerrors.Wrap("RISK_LIMITS_INTERNAL", "pb credit reservations", err)
	}
	hasActive := false
	for _, r := range res {
		if r.Status == "ACTIVE" {
			hasActive = true
		}
	}
	if !hasActive {
		return nil // order holds no PB reservation
	}
	usd, err := s.toUSD(ctx, deltaNotional, quoteCcy)
	if err != nil {
		return err
	}
	if deltaNotional.IsNegative() {
		usd = usd.Neg()
	}
	if err := s.store.AdjustTx(ctx, orderID, usd); err != nil {
		var be *PBBreachError
		if errors.As(err, &be) {
			return s.breachError(be)
		}
		return excerrors.Wrap("RISK_LIMITS_INTERNAL", "pb credit adjust", err)
	}
	return nil
}

// SyncNetOpenPosition re-bases a client's current_net_open_position to
// booked signed exposure (USD) + active reservations. Invoked by the
// reconciliation loop / after settlement flows so the incrementally
// maintained counter cannot drift.
func (s *PBCreditService) SyncNetOpenPosition(ctx context.Context, clientID int64) error {
	if s.store == nil {
		return excerrors.New("RISK_LIMITS_INTERNAL", "pb credit store not configured")
	}
	exp, err := s.store.PositionExposure(ctx, clientID)
	if err != nil {
		return excerrors.Wrap("RISK_LIMITS_INTERNAL", "pb position exposure", err)
	}
	var global decimal.Decimal
	pairUSD := map[string]decimal.Decimal{}
	for _, e := range exp {
		usd, err := s.toUSD(ctx, e.SignedNotional, e.QuoteCurrency)
		if err != nil {
			return err
		}
		if e.SignedNotional.IsNegative() {
			usd = usd.Neg()
		}
		global = global.Add(usd)
		p := normPair(e.CurrencyPair)
		pairUSD[p] = pairUSD[p].Add(usd)
	}
	return s.store.SyncNOPTx(ctx, clientID, global, pairUSD)
}

// ResetDailySettled rolls the DSL counters at the value-date boundary —
// wire to the EOD/rollover scheduler.
func (s *PBCreditService) ResetDailySettled(ctx context.Context) (int64, error) {
	if s.store == nil {
		return 0, excerrors.New("RISK_LIMITS_INTERNAL", "pb credit store not configured")
	}
	return s.store.ResetDailySettled(ctx)
}

// SweepOrphans releases reservations whose order went terminal while the
// release hook was unreachable (crash, consumer lag). Run periodically.
func (s *PBCreditService) SweepOrphans(ctx context.Context) (int64, error) {
	if s.store == nil {
		return 0, excerrors.New("RISK_LIMITS_INTERNAL", "pb credit store not configured")
	}
	return s.store.ReleaseTerminalTx(ctx)
}

// Utilization returns the live view per limit row for telemetry/admin —
// and fires OnAlert on each 90% crossing (the alert IS the spec §13.7
// notification point; consumers decide the paging channel).
func (s *PBCreditService) Utilization(ctx context.Context, clientID int64) ([]PBUtilization, error) {
	if s.store == nil {
		return nil, excerrors.New("RISK_LIMITS_INTERNAL", "pb credit store not configured")
	}
	limits, err := s.store.LimitsFor(ctx, clientID)
	if err != nil {
		return nil, excerrors.Wrap("RISK_LIMITS_INTERNAL", "pb credit limits", err)
	}
	out := make([]PBUtilization, 0, len(limits))
	for _, l := range limits {
		u := PBUtilization{Limit: l}
		if l.NOPLimit != nil && l.NOPLimit.IsPositive() {
			u.NOPPct = l.CurrentNOP.Div(*l.NOPLimit).Mul(decimal.NewFromInt(100))
		}
		if l.DSLLimit != nil && l.DSLLimit.IsPositive() {
			u.DSLPct = l.CurrentDSL.Div(*l.DSLLimit).Mul(decimal.NewFromInt(100))
		}
		u.Alerted = u.NOPPct.Cmp(decimal.NewFromInt(PBUtilizationAlertPct)) >= 0 ||
			u.DSLPct.Cmp(decimal.NewFromInt(PBUtilizationAlertPct)) >= 0
		out = append(out, u)
	}
	return out, nil
}

// checkAlerts emits OnAlert for rows that crossed the band during the
// just-committed mutation. Called post-commit with fresh rows.
func (s *PBCreditService) checkAlerts(ctx context.Context, clientID int64) {
	if s.OnAlert == nil {
		return
	}
	us, err := s.Utilization(ctx, clientID)
	if err != nil {
		return
	}
	for _, u := range us {
		if !u.Alerted {
			continue
		}
		bound := "NOP"
		if u.DSLPct.Cmp(u.NOPPct) > 0 {
			bound = "DSL"
		}
		pct := u.NOPPct
		if bound == "DSL" {
			pct = u.DSLPct
		}
		// Redis NX dedupe — one alert per (limit, bound) until the flag
		// is cleared by utilization dropping below the band.
		if s.rdb != nil {
			key := fmt.Sprintf("pb_credit_alert:%d:%s", u.Limit.ID, bound)
			ok, err := s.rdb.SetNX(ctx, key, "1", 24*time.Hour).Result()
			if err != nil || !ok {
				continue
			}
		}
		s.OnAlert(ctx, PBAlert{Limit: u.Limit, Bound: bound, Pct: pct, At: s.now()})
	}
}

// mirror refreshes the pb_credit:{pb}:{client} Redis hash post-commit —
// a read-only view for dashboards and the sub-millisecond cache layer
// named by the task. Best-effort: errors are swallowed because PG is
// authoritative (a stale mirror under-reports utilization = conservative).
func (s *PBCreditService) mirror(ctx context.Context, limits []PBCreditLimit,
	debits []PBDebit) {
	if s.rdb == nil || len(limits) == 0 {
		return
	}
	fresh, err := s.store.LimitsFor(ctx, limits[0].ClientAccountID)
	if err != nil || len(fresh) == 0 {
		return
	}
	l := fresh[0]
	key := fmt.Sprintf("pb_credit:%d:%d", l.PrimeBrokerID, l.ClientAccountID)
	fields := map[string]any{}
	for _, fl := range fresh {
		scope := "global"
		if fl.CurrencyPair != nil && *fl.CurrencyPair != "" {
			scope = "pair:" + normPair(*fl.CurrencyPair)
		}
		b, _ := json.Marshal(map[string]any{
			"nop_limit":   decPtr(fl.NOPLimit),
			"nop_current": fl.CurrentNOP.String(),
			"dsl_limit":   decPtr(fl.DSLLimit),
			"dsl_current": fl.CurrentDSL.String(),
		})
		fields[scope] = string(b)
	}
	_ = s.rdb.HSet(ctx, key, fields).Err()
	s.checkAlerts(ctx, l.ClientAccountID)
}

func decPtr(d *decimal.Decimal) any {
	if d == nil {
		return nil
	}
	return d.String()
}
