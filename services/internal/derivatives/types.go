// Package derivatives implements Phase-22 FX derivatives foundations —
// Tasks 22.3.1 (FX forwards), 22.3.2 (FX swaps) and 22.3.3 (NDFs).
//
// Canonical contracts (spec §6.3, §7.4, §15.1, §15.3, §15.7, §24
// #57/#58/#59/#396):
//   - Forward pricing is covered interest parity on a money-market basis:
//     F = S × (1 + r_quote·d/DCC_quote) / (1 + r_base·d/DCC_base) where the
//     day-count denominator is per currency (ACT/360 for USD, EUR, CHF,
//     JPY; ACT/365 for GBP, AUD, NZD, CAD, SGD, HKD) — rates.DayCount is
//     the single source.
//   - Spot value dates follow the pair's settlement_cycle (T+0 same-day
//     USD/CAD + USD/MXN, T+1 default, T+2 exotics) walked forward under
//     ISDA Modified Following across every settlement center — the
//     settlement.HolidayCalendar engine, never local date math.
//   - A FORWARD's physical delivery and both legs of a SWAP settle as
//     dated settlement_instructions rows (linked by
//     settlement_instructions.derivative_contract_id, migration 254), so
//     the Phase-03 dispatch machinery carries them unchanged. An NDF
//     writes a single net cash leg in its settlement currency on the
//     fixing date (spec §6.3 "Cash-settled NDFs settle at fixing date").
//   - Fail-closed (spec §2.7): a missing/stale/incomplete yield curve is
//     YIELD_CURVE_UNAVAILABLE, a missing NDF fixing is
//     BENCHMARK_UNAVAILABLE — never a default-zero rate.
//
// Persistence: every write path runs inside one SERIALIZABLE transaction
// (PgxContractStore.InTx) with the §5.40 conflict-retry schedule —
// SQLSTATE 40001/40P01 → backoff 5/15/45 ms + jitter, exhaustion →
// TRANSACTION_CONFLICT_RETRY_EXHAUSTED.
package derivatives

import (
	"context"
	stderrors "errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"exchange/pkg/decimal"
	excerrors "exchange/pkg/errors"
)

// ---------------------------------------------------------------------------
// Error codes — aliases into the canonical errs registry (Task 5.3.21).
// BENCHMARK_UNAVAILABLE and DERIVATIVE_STATE_CONFLICT are registered in
// errs.localCodes by this phase (spec-cited codes pending §23 rows).
// ---------------------------------------------------------------------------
const (
	CodeInvalidRequest          = "INVALID_REQUEST"                      // 400
	CodeNotFound                = "NOT_FOUND"                            // 404
	CodeValueDateOnHoliday      = "VALUE_DATE_ON_HOLIDAY"                // 422
	CodeYieldCurveUnavailable   = "YIELD_CURVE_UNAVAILABLE"              // 503
	CodePriceOracleUnavailable  = "PRICE_ORACLE_UNAVAILABLE"             // 503
	CodeBenchmarkUnavailable    = "BENCHMARK_UNAVAILABLE"                // 503
	CodeDerivativeStateConflict = "DERIVATIVE_STATE_CONFLICT"            // 409
	CodeTxnConflictExhausted    = "TRANSACTION_CONFLICT_RETRY_EXHAUSTED" // 503
	// CodeFairValueDivergence — spec §27.1 MTF/Fair-Value matrix row
	// (503, L1): an explicit agreed forward/NDF rate diverges from the
	// CIP fair value beyond the canonical band (25 bps — the §27.1
	// oracle-divergence convention).
	CodeFairValueDivergence = "FAIR_VALUE_DIVERGENCE" // 503
)

// FairValueDivergenceBps is the |agreed − CIP fair| / fair band in basis
// points. 25 mirrors the spec §27.1 oracle-divergence threshold.
const FairValueDivergenceBps = 25

// ---------------------------------------------------------------------------
// Instrument vocabulary (spec §5.1 instrument_type_enum — reuse the same
// strings; migration 001 owns the enum).
// ---------------------------------------------------------------------------

// Instrument types this package prices/books.
const (
	InstrumentForward = "FORWARD"
	InstrumentSwap    = "SWAP"
	InstrumentNDF     = "NDF"
)

// ContractSide is the contract holder's direction on the base currency.
type ContractSide string

const (
	SideBuy  ContractSide = "BUY"  // holder buys base, pays quote
	SideSell ContractSide = "SELL" // holder sells base, receives quote
)

// ContractKind mirrors derivative_kind_enum (migration 254).
type ContractKind string

const (
	KindForward ContractKind = "FORWARD"
	KindSwap    ContractKind = "SWAP"
	KindNDF     ContractKind = "NDF"
)

// ContractStatus mirrors derivative_contract_status_enum.
type ContractStatus string

const (
	StatusOpen             ContractStatus = "OPEN"
	StatusPartiallySettled ContractStatus = "PARTIALLY_SETTLED" // SWAP near leg settled, far pending
	StatusSettled          ContractStatus = "SETTLED"
	StatusFailed           ContractStatus = "FAILED"
	StatusCancelled        ContractStatus = "CANCELLED"
)

// LegDirection mirrors settlement_direction_enum (migration 019).
type LegDirection string

const (
	LegPay     LegDirection = "PAY"
	LegReceive LegDirection = "RECEIVE"
)

// LegTag marks which contract leg a settlement instruction belongs to.
// FORWARD legs tag FAR (single delivery at maturity); SWAP legs tag NEAR
// and FAR; the NDF cash leg tags FIXING.
type LegTag string

const (
	LegTagNear   LegTag = "NEAR"
	LegTagFar    LegTag = "FAR"
	LegTagFixing LegTag = "FIXING"
)

var currencyRe = regexp.MustCompile(`^[A-Z]{3}$`)

// Pair is a currency pair in base/quote terms (BASE/QUOTE, quote per
// base — the spec §5.1 convention).
type Pair struct {
	Base  string
	Quote string
}

// NewPair validates and normalizes a pair.
func NewPair(base, quote string) (Pair, error) {
	p := Pair{Base: strings.ToUpper(strings.TrimSpace(base)),
		Quote: strings.ToUpper(strings.TrimSpace(quote))}
	if !currencyRe.MatchString(p.Base) || !currencyRe.MatchString(p.Quote) {
		return Pair{}, excerrors.New(CodeInvalidRequest,
			fmt.Sprintf("invalid currency pair %q/%q", base, quote))
	}
	if p.Base == p.Quote {
		return Pair{}, excerrors.New(CodeInvalidRequest,
			"degenerate pair "+p.Base+"/"+p.Quote)
	}
	return p, nil
}

// Symbol renders "BASE/QUOTE".
func (p Pair) Symbol() string { return p.Base + "/" + p.Quote }

// ---------------------------------------------------------------------------
// Contract + settlement-leg records (migration 254)
// ---------------------------------------------------------------------------

// Contract is one booked forward/swap/NDF — a dated currency obligation
// created from a matched fill (TradeID). Quantities are in base currency;
// rates are quote-per-base.
type Contract struct {
	ID           int64
	TradeID      int64 // originating fill — settlement_instructions.trade_id
	AccountID    int64
	InstrumentID int64
	Kind         ContractKind
	Side         ContractSide
	Pair         Pair
	// Notional is the base-currency amount exchanged per leg.
	Notional decimal.Decimal
	// SpotRate is the reference spot at booking (a swap's near-leg rate).
	SpotRate decimal.Decimal
	// ForwardRate is the agreed outright rate — the forward's rate, the
	// swap's far-leg rate, or the NDF contract rate.
	ForwardRate decimal.Decimal
	// SwapPoints = ForwardRate − SpotRate for swaps (and the same
	// definition reported on forwards for audit).
	SwapPoints decimal.Decimal
	// SpotValueDate is the pair's spot value date at trade time.
	SpotValueDate time.Time
	// ValueDate is the forward maturity / swap far-leg / NDF settlement
	// date (NDF cash legs settle on the fixing date).
	ValueDate time.Time
	// NearLegValueDate is the swap near-leg value date (spot for a
	// standard swap); nil for plain forwards/NDFs.
	NearLegValueDate *time.Time
	// NDF fields (nil for FORWARD/SWAP).
	NdfFixingDate   *time.Time
	NdfFixingSource string
	// SettlementCurrency is the deliverable currency the NDF cash leg is
	// paid in (base or quote of the pair).
	SettlementCurrency string
	FixingRate         *decimal.Decimal
	SettlementAmount   *decimal.Decimal // signed, settlement currency
	Status             ContractStatus
	BookedAt           time.Time
	SettledAt          *time.Time
	IdempotencyKey     string
}

// SettlementLeg is one dated payment obligation written to
// settlement_instructions and linked back to its contract.
type SettlementLeg struct {
	ContractID int64
	TradeID    int64
	AccountID  int64
	Tag        LegTag
	Currency   string
	Amount     decimal.Decimal // always positive; direction carries the sign
	Direction  LegDirection
	ValueDate  time.Time
}

// NdfFixing is one recorded fixing observation (migration 254).
type NdfFixing struct {
	ID           int64
	ContractID   int64
	FixingDate   time.Time
	Source       string // ndf_fixing_source vocabulary (ndfs.go)
	Rate         decimal.Decimal
	PriorDayHold bool // §15.7 last-resort tier flag
	ObservedAt   time.Time
}

// ---------------------------------------------------------------------------
// Store — the SERIALIZABLE persistence seam
// ---------------------------------------------------------------------------

// ContractTx is the transaction-scoped persistence surface. All financial
// mutations flow through it so a booking/settle either commits atomically
// or not at all.
type ContractTx interface {
	// InsertContract writes the contract row. Idempotent on
	// IdempotencyKey: a replay returns the existing id with created=false.
	InsertContract(ctx context.Context, c *Contract) (id int64, created bool, err error)
	// ContractForUpdate loads a contract row under FOR UPDATE.
	ContractForUpdate(ctx context.Context, id int64) (*Contract, error)
	// InsertLegs writes settlement_instructions rows (linked to the
	// contract). Idempotent per (trade_id, account_id, currency,
	// direction) — the settlement_instructions_leg_ux index absorbs
	// replays.
	InsertLegs(ctx context.Context, legs []SettlementLeg) (int, error)
	// ContractLegs loads the settlement legs linked to a contract.
	ContractLegs(ctx context.Context, contractID int64) ([]PersistedLeg, error)
	// UpdateContractStatus transitions the contract status row.
	UpdateContractStatus(ctx context.Context, id int64, st ContractStatus, settledAt *time.Time) error
	// InsertNdfFixing records an NDF fixing observation. The
	// UNIQUE(contract_id, fixing_date) constraint rejects duplicates.
	InsertNdfFixing(ctx context.Context, f *NdfFixing) (int64, error)
	// SetContractFixing stamps the observed fixing rate on the contract
	// while it remains OPEN (status flips at settlement).
	SetContractFixing(ctx context.Context, id int64, rate decimal.Decimal) error
	// SetContractOutcome stamps the fixing rate, settlement amount,
	// status and settled_at atomically.
	SetContractOutcome(ctx context.Context, id int64, fixingRate decimal.Decimal,
		amount decimal.Decimal, st ContractStatus, settledAt time.Time) error
	// DueContracts lists OPEN/PARTIALLY_SETTLED contracts with value_date
	// or near_leg_value_date on/before asOf (the maturity/settle sweep).
	DueContracts(ctx context.Context, asOf time.Time, limit int) ([]Contract, error)
}

// PersistedLeg is a settlement_instructions row read back for rollup.
type PersistedLeg struct {
	ID        int64
	Direction LegDirection
	Currency  string
	Amount    decimal.Decimal
	ValueDate time.Time
	Status    string // PENDING | SETTLED | FAILED | RECONCILED
}

// ContractStore is the top-level persistence seam.
type ContractStore interface {
	// InTx runs fn inside a SERIALIZABLE transaction, retrying on
	// SQLSTATE 40001/40P01 per the §5.40 schedule (5/15/45 ms + jitter,
	// max 3 attempts) — exhaustion surfaces
	// TRANSACTION_CONFLICT_RETRY_EXHAUSTED.
	InTx(ctx context.Context, fn func(context.Context, ContractTx) error) error
}

// ---------------------------------------------------------------------------
// PgxContractStore — PostgreSQL implementation (migration 254)
// ---------------------------------------------------------------------------

// PgxContractStore implements ContractStore over pgx. Numerics cross the
// wire as text (::text read, string param on write) — the package-wide
// convention from internal/settlement.
type PgxContractStore struct {
	Pool     *pgxpool.Pool
	MaxTries int // default 3
}

// NewPgxContractStore wires the store.
func NewPgxContractStore(pool *pgxpool.Pool) *PgxContractStore {
	return &PgxContractStore{Pool: pool, MaxTries: 3}
}

var conflictBackoff = []time.Duration{5 * time.Millisecond, 15 * time.Millisecond, 45 * time.Millisecond}

// InTx runs fn inside a SERIALIZABLE tx with the §5.40 retry schedule.
func (s *PgxContractStore) InTx(ctx context.Context, fn func(context.Context, ContractTx) error) error {
	tries := s.MaxTries
	if tries <= 0 {
		tries = 3
	}
	var lastErr error
	for attempt := 0; attempt < tries; attempt++ {
		err := s.runOnce(ctx, fn)
		if err == nil {
			return nil
		}
		lastErr = err
		if !isSerializationConflict(err) {
			return err
		}
		if attempt+1 < tries {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(conflictBackoff[minInt(attempt, len(conflictBackoff)-1)]):
			}
		}
	}
	return excerrors.Wrap(CodeTxnConflictExhausted,
		"derivatives: SERIALIZABLE conflict retry budget exhausted", lastErr)
}

func (s *PgxContractStore) runOnce(ctx context.Context, fn func(context.Context, ContractTx) error) error {
	tx, err := s.Pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return fmt.Errorf("derivatives tx begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := fn(ctx, pgxContractTx{tx: tx}); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("derivatives tx commit: %w", err)
	}
	return nil
}

// isSerializationConflict reports the §5.40 whole-transaction retry
// candidates (40001 serialization_failure / 40P01 deadlock).
func isSerializationConflict(err error) bool {
	var pgErr *pgconn.PgError
	if stderrors.As(err, &pgErr) {
		return pgErr.Code == "40001" || pgErr.Code == "40P01"
	}
	return false
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

type pgxContractTx struct{ tx pgx.Tx }

func (t pgxContractTx) InsertContract(ctx context.Context, c *Contract) (int64, bool, error) {
	var id int64
	err := t.tx.QueryRow(ctx, `
		INSERT INTO derivative_contracts
		    (trade_id, account_id, instrument_id, kind, side,
		     base_currency, quote_currency, notional, spot_rate,
		     forward_rate, swap_points, spot_value_date, value_date,
		     near_leg_value_date, ndf_fixing_date, ndf_fixing_source,
		     settlement_currency, status, idempotency_key)
		VALUES ($1,$2,$3,$4::derivative_kind_enum,$5,$6,$7,$8::numeric,$9::numeric,
		        $10::numeric,$11::numeric,$12,$13,$14,$15,NULLIF($16,''),
		        NULLIF($17,''),'OPEN',NULLIF($18,''))
		ON CONFLICT (idempotency_key) WHERE idempotency_key IS NOT NULL DO NOTHING
		RETURNING id`,
		c.TradeID, c.AccountID, c.InstrumentID, string(c.Kind), string(c.Side),
		c.Pair.Base, c.Pair.Quote, c.Notional.String(), c.SpotRate.String(),
		c.ForwardRate.String(), c.SwapPoints.String(), c.SpotValueDate, c.ValueDate,
		c.NearLegValueDate, c.NdfFixingDate, c.NdfFixingSource,
		c.SettlementCurrency, c.IdempotencyKey).Scan(&id)
	if stderrors.Is(err, pgx.ErrNoRows) {
		if c.IdempotencyKey == "" {
			return 0, false, excerrors.New(CodeInvalidRequest,
				"contract insert replayed without idempotency key")
		}
		var existing int64
		if err := t.tx.QueryRow(ctx,
			`SELECT id FROM derivative_contracts WHERE idempotency_key = $1`,
			c.IdempotencyKey).Scan(&existing); err != nil {
			return 0, false, fmt.Errorf("derivatives: replay lookup: %w", err)
		}
		return existing, false, nil
	}
	if err != nil {
		return 0, false, fmt.Errorf("derivatives: insert contract: %w", err)
	}
	return id, true, nil
}

func (t pgxContractTx) ContractForUpdate(ctx context.Context, id int64) (*Contract, error) {
	c, err := scanContract(t.tx.QueryRow(ctx, contractCols+`
		  FROM derivative_contracts WHERE id = $1 FOR UPDATE`, id))
	if stderrors.Is(err, pgx.ErrNoRows) {
		return nil, excerrors.New(CodeNotFound,
			fmt.Sprintf("derivative contract %d not found", id))
	}
	if err != nil {
		return nil, fmt.Errorf("derivatives: load contract %d: %w", id, err)
	}
	return c, nil
}

const contractCols = `
		SELECT id, trade_id, account_id, instrument_id, kind::text, side,
		       base_currency, quote_currency, notional::text, spot_rate::text,
		       forward_rate::text, swap_points::text, spot_value_date, value_date,
		       near_leg_value_date, ndf_fixing_date,
		       COALESCE(ndf_fixing_source, ''), COALESCE(settlement_currency, ''),
		       fixing_rate::text, settlement_amount::text, status::text,
		       booked_at, settled_at, COALESCE(idempotency_key, '')`

func scanContract(row pgx.Row) (*Contract, error) {
	var c Contract
	var notional, spot, fwd, points string
	var fixingRate, settleAmt *string
	err := row.Scan(&c.ID, &c.TradeID, &c.AccountID, &c.InstrumentID,
		(*string)(&c.Kind), (*string)(&c.Side), &c.Pair.Base, &c.Pair.Quote,
		&notional, &spot, &fwd, &points, &c.SpotValueDate, &c.ValueDate,
		&c.NearLegValueDate, &c.NdfFixingDate, &c.NdfFixingSource,
		&c.SettlementCurrency, &fixingRate, &settleAmt,
		(*string)(&c.Status), &c.BookedAt, &c.SettledAt, &c.IdempotencyKey)
	if err != nil {
		return nil, err
	}
	parse := func(name, s string, dst *decimal.Decimal) error {
		d, err := decimal.NewFromString(s)
		if err != nil {
			return fmt.Errorf("contract %s %q malformed: %w", name, s, err)
		}
		*dst = d
		return nil
	}
	if err := parse("notional", notional, &c.Notional); err != nil {
		return nil, err
	}
	if err := parse("spot_rate", spot, &c.SpotRate); err != nil {
		return nil, err
	}
	if err := parse("forward_rate", fwd, &c.ForwardRate); err != nil {
		return nil, err
	}
	if err := parse("swap_points", points, &c.SwapPoints); err != nil {
		return nil, err
	}
	if fixingRate != nil {
		d, err := decimal.NewFromString(*fixingRate)
		if err != nil {
			return nil, fmt.Errorf("contract fixing_rate %q malformed: %w", *fixingRate, err)
		}
		c.FixingRate = &d
	}
	if settleAmt != nil {
		d, err := decimal.NewFromString(*settleAmt)
		if err != nil {
			return nil, fmt.Errorf("contract settlement_amount %q malformed: %w", *settleAmt, err)
		}
		c.SettlementAmount = &d
	}
	return &c, nil
}

func (t pgxContractTx) InsertLegs(ctx context.Context, legs []SettlementLeg) (int, error) {
	inserted := 0
	for _, l := range legs {
		tag, err := t.tx.Exec(ctx, `
			INSERT INTO settlement_instructions
			    (trade_id, account_id, currency, amount, direction,
			     settlement_date, status, derivative_contract_id, leg_tag)
			VALUES ($1,$2,$3,$4::numeric,$5::settlement_direction_enum,$6,'PENDING',$7,$8)
			ON CONFLICT (trade_id, account_id, currency, direction) DO NOTHING`,
			l.TradeID, l.AccountID, l.Currency, l.Amount.String(),
			string(l.Direction), l.ValueDate, l.ContractID, string(l.Tag))
		if err != nil {
			return inserted, fmt.Errorf("derivatives: insert leg %s %s %s: %w",
				l.Tag, l.Direction, l.Currency, err)
		}
		inserted += int(tag.RowsAffected())
	}
	return inserted, nil
}

func (t pgxContractTx) ContractLegs(ctx context.Context, contractID int64) ([]PersistedLeg, error) {
	rows, err := t.tx.Query(ctx, `
		SELECT id, direction::text, currency, amount::text, settlement_date, status::text
		  FROM settlement_instructions
		 WHERE derivative_contract_id = $1
		 ORDER BY id`, contractID)
	if err != nil {
		return nil, fmt.Errorf("derivatives: legs for contract %d: %w", contractID, err)
	}
	defer rows.Close()
	var out []PersistedLeg
	for rows.Next() {
		var l PersistedLeg
		var amt string
		if err := rows.Scan(&l.ID, (*string)(&l.Direction), &l.Currency,
			&amt, &l.ValueDate, &l.Status); err != nil {
			return nil, err
		}
		l.Amount, err = decimal.NewFromString(amt)
		if err != nil {
			return nil, fmt.Errorf("leg %d amount %q malformed: %w", l.ID, amt, err)
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

func (t pgxContractTx) UpdateContractStatus(ctx context.Context, id int64, st ContractStatus, settledAt *time.Time) error {
	tag, err := t.tx.Exec(ctx, `
		UPDATE derivative_contracts
		   SET status = $2::derivative_contract_status_enum, settled_at = $3,
		       updated_at = now()
		 WHERE id = $1`, id, string(st), settledAt)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return excerrors.New(CodeNotFound,
			fmt.Sprintf("derivative contract %d not found", id))
	}
	return nil
}

func (t pgxContractTx) InsertNdfFixing(ctx context.Context, f *NdfFixing) (int64, error) {
	var id int64
	err := t.tx.QueryRow(ctx, `
		INSERT INTO ndf_fixings (contract_id, fixing_date, source, rate, prior_day_hold)
		VALUES ($1,$2,$3,$4::numeric,$5)
		ON CONFLICT (contract_id, fixing_date) DO NOTHING
		RETURNING id`,
		f.ContractID, f.FixingDate, f.Source, f.Rate.String(), f.PriorDayHold).
		Scan(&id)
	if stderrors.Is(err, pgx.ErrNoRows) {
		var existing int64
		if err := t.tx.QueryRow(ctx,
			`SELECT id FROM ndf_fixings WHERE contract_id = $1 AND fixing_date = $2`,
			f.ContractID, f.FixingDate).Scan(&existing); err != nil {
			return 0, fmt.Errorf("derivatives: fixing replay lookup: %w", err)
		}
		return existing, nil
	}
	if err != nil {
		return 0, fmt.Errorf("derivatives: insert ndf fixing: %w", err)
	}
	return id, nil
}

func (t pgxContractTx) SetContractFixing(ctx context.Context, id int64, rate decimal.Decimal) error {
	tag, err := t.tx.Exec(ctx, `
		UPDATE derivative_contracts
		   SET fixing_rate = $2::numeric, updated_at = now()
		 WHERE id = $1 AND status = 'OPEN'`, id, rate.String())
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return excerrors.New(CodeDerivativeStateConflict,
			fmt.Sprintf("derivative contract %d not OPEN at fixing", id))
	}
	return nil
}

func (t pgxContractTx) SetContractOutcome(ctx context.Context, id int64,
	fixingRate, amount decimal.Decimal, st ContractStatus, settledAt time.Time) error {
	tag, err := t.tx.Exec(ctx, `
		UPDATE derivative_contracts
		   SET fixing_rate = $2::numeric, settlement_amount = $3::numeric,
		       status = $4::derivative_contract_status_enum, settled_at = $5,
		       updated_at = now()
		 WHERE id = $1 AND status = 'OPEN'`,
		id, fixingRate.String(), amount.String(), string(st), settledAt)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return excerrors.New(CodeDerivativeStateConflict,
			fmt.Sprintf("derivative contract %d lost OPEN status mid-settle", id))
	}
	return nil
}

// ensureLegsMatch verifies the leg insert landed: the shared
// settlement_instructions (trade_id, account_id, currency, direction)
// unique index silently absorbs duplicates via ON CONFLICT DO NOTHING, so
// a colliding leg from a different writer (e.g. a spot-settlement pass on
// the same trade) would drop a delivery obligation without error. After
// writing, re-read the contract's legs and require every expected leg to
// exist with identical currency, direction, amount and value date —
// anything less is a coded rejection and the transaction rolls back
// (fail-closed, spec §2.7).
func ensureLegsMatch(ctx context.Context, tx ContractTx, contractID int64, want []SettlementLeg) error {
	got, err := tx.ContractLegs(ctx, contractID)
	if err != nil {
		return err
	}
	for _, w := range want {
		matched := false
		for _, g := range got {
			if g.Currency == w.Currency && g.Direction == w.Direction &&
				g.Amount.Equal(w.Amount) && normDay(g.ValueDate).Equal(normDay(w.ValueDate)) {
				matched = true
				break
			}
		}
		if !matched {
			return excerrors.New(CodeDerivativeStateConflict, fmt.Sprintf(
				"contract %d leg %s %s %s @ %s absent or conflicts with an existing instruction",
				contractID, w.Tag, w.Direction, w.Currency, w.ValueDate.Format("2006-01-02")))
		}
	}
	return nil
}

func (t pgxContractTx) DueContracts(ctx context.Context, asOf time.Time, limit int) ([]Contract, error) {
	if limit <= 0 {
		limit = 500
	}
	rows, err := t.tx.Query(ctx, contractCols+`
		  FROM derivative_contracts
		 WHERE status IN ('OPEN','PARTIALLY_SETTLED')
		   AND (value_date <= $1 OR near_leg_value_date <= $1)
		 ORDER BY value_date, id
		 LIMIT $2`, asOf, limit)
	if err != nil {
		return nil, fmt.Errorf("derivatives: due scan: %w", err)
	}
	defer rows.Close()
	var out []Contract
	for rows.Next() {
		c, err := scanContract(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *c)
	}
	return out, rows.Err()
}
