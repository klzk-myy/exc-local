// Package instruments implements the Phase-15 Tasks 15.3.11/15.3.12/
// 15.3.13 instrument reference, session/tenor calendar, listing and
// delisting operations, and the daily closing-auction / benchmark-fixing
// schedulers (spec §7.1/§7.4/§7.5, §24 #343/#352/#401).
//
// The package consumes the Phase-15 sibling seams rather than
// duplicating them: lifecycle transitions run through
// admin.InstrumentService (Task 15.3.1 state machine), four-eyes
// approvals through admin.DualControlService (Task 7.3.2 queue +
// Task 15.3.2 executors), audit rows through admin.Log +
// audit.Append (Task 7.3.3), and holiday math through the Phase-03
// settlement.HolidayCalendar (Task 3.3.8 currency_holidays calendar).
//
// Engine control-plane contracts (shared with the Task 15.3.1/15.3.6
// sibling cluster — same keys, same payload shapes):
//
//	instrument:status:{symbol}   = plain enum word ("ACTIVE", ...).
//	instrument:auction:{symbol}  = "CALL:{deadline_unix_ns}" (and
//	                              "EXTEND:{deadline_unix_ns}" while a
//	                              failing auction is extended).
//	instrument:auction:{symbol}:queue = JSON array of queued MOC/MOO/
//	                              FIXING order ids for the uncross (the
//	                              Phase-16 order types already exist in
//	                              order_type_enum; their matching engine
//	                              is Phase-16 scope — this package owns
//	                              the queue-and-publish seam).
//	instrument:fixing:{symbol}   = "FIXING:{benchmark}:{rate}:{unix_ns}"
//	                              written when a benchmark fixing fires.
//
// No corporate actions: fiat spot FX has no splits or dividends — this
// package deliberately emits no corporate-action events;
// CORPORATE_ACTION_SCHEDULED stays reserved and is never emitted
// (spec §7.4 item 4). Currency redenomination / peg-break follows the
// RESTRICTED → DELISTED ladder with the §7.4 force-close path.
package instruments

import (
	"context"
	"encoding/json"
	"errors"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/shopspring/decimal"

	excerrors "exchange/pkg/errors"
)

// ---------------------------------------------------------------------------
// §7.4 instrument reference model
// ---------------------------------------------------------------------------

// Reference is the complete per-symbol reference row — the §5.1
// instruments columns plus the migration-087 contract_size /
// decimal_places / pip_size and the §5.44.10 instruments_reference
// envelope. DECIMAL fields ride as fixed-point strings (::text on the
// wire), matching the marketapi convention.
type Reference struct {
	InstrumentID    int64           `json:"instrument_id"`
	Symbol          string          `json:"symbol"`
	BaseCurrency    string          `json:"base_currency"`
	QuoteCurrency   string          `json:"quote_currency"`
	InstrumentType  string          `json:"instrument_type"`
	Status          string          `json:"status"`
	TickSize        string          `json:"tick_size"`
	LotSize         string          `json:"lot_size"`
	MinOrderQty     string          `json:"min_order_qty"`
	MaxOrderQty     string          `json:"max_order_qty"`
	MinNotional     string          `json:"min_notional"`
	ContractSize    string          `json:"contract_size"`
	DecimalPlaces   int             `json:"decimal_places"`
	PipSize         string          `json:"pip_size"`
	SettlementCycle int             `json:"settlement_cycle"`
	MaxLeverage     int64           `json:"max_leverage"`
	MarginRate      string          `json:"margin_rate,omitempty"`
	TradingHours    json.RawMessage `json:"trading_hours,omitempty"`
	Tenor           json.RawMessage `json:"tenor,omitempty"`
	FixingCalendar  json.RawMessage `json:"fixing_calendar,omitempty"`
	DelistSchedule  json.RawMessage `json:"delist_schedule,omitempty"`
}

// ReferenceInput is the §7.4 reference row a listing proposal supplies —
// every field required (the auto-check fails closed on a partial row).
type ReferenceInput struct {
	BaseCurrency    string `json:"base_currency"`
	QuoteCurrency   string `json:"quote_currency"`
	InstrumentType  string `json:"instrument_type"`
	TickSize        string `json:"tick_size"`
	LotSize         string `json:"lot_size"`
	MinOrderQty     string `json:"min_order_qty"`
	MaxOrderQty     string `json:"max_order_qty"`
	MinNotional     string `json:"min_notional"`
	ContractSize    string `json:"contract_size"`
	DecimalPlaces   int    `json:"decimal_places"`
	PipSize         string `json:"pip_size"`
	SettlementCycle int    `json:"settlement_cycle"`
	MaxLeverage     int64  `json:"max_leverage"`
}

// Validate checks the reference row for completeness and sanity — the
// listing auto-check uses this verbatim so a proposed row cannot pass
// with synthetic or missing values.
func (r ReferenceInput) Validate() []string {
	var fails []string
	fail := func(cond bool, msg string) {
		if !cond {
			fails = append(fails, msg)
		}
	}
	fail(len(r.BaseCurrency) == 3 && len(r.QuoteCurrency) == 3,
		"base_currency/quote_currency must be ISO 4217 3-letter codes")
	switch strings.ToUpper(r.InstrumentType) {
	case "SPOT", "FORWARD", "SWAP", "NDF", "OPTION":
	default:
		fails = append(fails, "instrument_type must be SPOT|FORWARD|SWAP|NDF|OPTION")
	}
	posDec := func(name, v string) decimal.Decimal {
		d, err := decimal.NewFromString(v)
		if err != nil || !d.IsPositive() {
			fails = append(fails, name+" must be a positive decimal")
			return decimal.Zero
		}
		return d
	}
	nonNegDec := func(name, v string) decimal.Decimal {
		d, err := decimal.NewFromString(v)
		if err != nil || d.IsNegative() {
			fails = append(fails, name+" must be a non-negative decimal")
			return decimal.Zero
		}
		return d
	}
	posDec("tick_size", r.TickSize)
	posDec("lot_size", r.LotSize)
	minQty := posDec("min_order_qty", r.MinOrderQty)
	maxQty := posDec("max_order_qty", r.MaxOrderQty)
	if minQty.IsPositive() && maxQty.IsPositive() && minQty.GreaterThan(maxQty) {
		fails = append(fails, "min_order_qty must not exceed max_order_qty")
	}
	nonNegDec("min_notional", r.MinNotional)
	posDec("contract_size", r.ContractSize)
	posDec("pip_size", r.PipSize)
	fail(r.DecimalPlaces >= 0 && r.DecimalPlaces <= 8,
		"decimal_places must be between 0 and 8")
	fail(r.SettlementCycle >= 0 && r.SettlementCycle <= 2,
		"settlement_cycle must be 0 (same-day), 1 (T+1) or 2 (T+2)")
	fail(r.MaxLeverage > 0, "max_leverage must be positive")
	return fails
}

// Complete reports whether a stored row carries every §7.4 field —
// reference completeness, not just non-emptiness.
func (r Reference) Complete() bool {
	in := ReferenceInput{
		BaseCurrency:    r.BaseCurrency,
		QuoteCurrency:   r.QuoteCurrency,
		InstrumentType:  r.InstrumentType,
		TickSize:        r.TickSize,
		LotSize:         r.LotSize,
		MinOrderQty:     r.MinOrderQty,
		MaxOrderQty:     r.MaxOrderQty,
		MinNotional:     r.MinNotional,
		ContractSize:    r.ContractSize,
		DecimalPlaces:   r.DecimalPlaces,
		PipSize:         r.PipSize,
		SettlementCycle: r.SettlementCycle,
		MaxLeverage:     r.MaxLeverage,
	}
	return len(in.Validate()) == 0
}

// ---------------------------------------------------------------------------
// Reference store
// ---------------------------------------------------------------------------

// ReferenceStore reads reference rows. The listing auto-check uses it to
// prove the proposed symbol does not already carry a reference row, and
// the ops board joins it for envelope data.
type ReferenceStore struct {
	pool *pgxpool.Pool
}

// NewReferenceStore wires the store.
func NewReferenceStore(pool *pgxpool.Pool) *ReferenceStore {
	return &ReferenceStore{pool: pool}
}

const referenceCols = `
	i.id, i.symbol, i.base_currency, i.quote_currency,
	i.instrument_type::text, i.status::text,
	i.tick_size::text, i.lot_size::text,
	i.min_order_qty::text, i.max_order_qty::text, i.min_notional::text,
	i.contract_size::text, i.decimal_places, i.pip_size::text,
	i.settlement_cycle, i.max_leverage,
	COALESCE(r.margin_rate::text, ''), r.trading_hours, r.tenor,
	r.fixing_calendar, r.delist_schedule`

func scanReference(row pgx.Row) (*Reference, error) {
	var r Reference
	err := row.Scan(&r.InstrumentID, &r.Symbol, &r.BaseCurrency,
		&r.QuoteCurrency, &r.InstrumentType, &r.Status,
		&r.TickSize, &r.LotSize, &r.MinOrderQty, &r.MaxOrderQty,
		&r.MinNotional, &r.ContractSize, &r.DecimalPlaces, &r.PipSize,
		&r.SettlementCycle, &r.MaxLeverage, &r.MarginRate,
		&r.TradingHours, &r.Tenor, &r.FixingCalendar, &r.DelistSchedule)
	if err != nil {
		return nil, err
	}
	return &r, nil
}

// ReferenceBySymbol returns nil for an unknown symbol.
func (s *ReferenceStore) ReferenceBySymbol(ctx context.Context, symbol string) (*Reference, error) {
	r, err := scanReference(s.pool.QueryRow(ctx, `
		SELECT `+referenceCols+`
		FROM instruments i
		LEFT JOIN instruments_reference r ON r.instrument_id = i.id
		WHERE i.symbol = $1`, strings.ToUpper(strings.TrimSpace(symbol))))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, excerrors.Wrap("INTERNAL_ERROR", "reference lookup", err)
	}
	return r, nil
}

// References returns every instrument's reference row ordered by symbol.
func (s *ReferenceStore) References(ctx context.Context) ([]Reference, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT `+referenceCols+`
		FROM instruments i
		LEFT JOIN instruments_reference r ON r.instrument_id = i.id
		ORDER BY i.symbol`)
	if err != nil {
		return nil, excerrors.Wrap("INTERNAL_ERROR", "list references", err)
	}
	defer rows.Close()
	out := []Reference{}
	for rows.Next() {
		var r Reference
		if err := rows.Scan(&r.InstrumentID, &r.Symbol, &r.BaseCurrency,
			&r.QuoteCurrency, &r.InstrumentType, &r.Status,
			&r.TickSize, &r.LotSize, &r.MinOrderQty, &r.MaxOrderQty,
			&r.MinNotional, &r.ContractSize, &r.DecimalPlaces, &r.PipSize,
			&r.SettlementCycle, &r.MaxLeverage, &r.MarginRate,
			&r.TradingHours, &r.Tenor, &r.FixingCalendar,
			&r.DelistSchedule); err != nil {
			return nil, excerrors.Wrap("INTERNAL_ERROR", "scan reference", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// UpsertReferenceRow writes the §5.44.10 envelope for an instrument
// inside the caller's transaction (the listing-approval path patches the
// sibling's DRAFT insert with the proposer's full reference row the same
// way — see ListingService).
func upsertReferenceEnvelope(ctx context.Context, tx pgx.Tx, r Reference) error {
	if r.MarginRate == "" {
		return excerrors.New("INVALID_REQUEST", "margin_rate is required")
	}
	for name, raw := range map[string]json.RawMessage{
		"trading_hours":   r.TradingHours,
		"tenor":           r.Tenor,
		"fixing_calendar": r.FixingCalendar,
	} {
		if len(raw) == 0 {
			continue
		}
		if !json.Valid(raw) {
			return excerrors.New("INVALID_REQUEST", name+" is not valid JSON")
		}
	}
	trading := r.TradingHours
	if len(trading) == 0 {
		trading = json.RawMessage(`{}`)
	}
	tenor := r.Tenor
	if len(tenor) == 0 {
		tenor = json.RawMessage(`[]`)
	}
	fixing := r.FixingCalendar
	if len(fixing) == 0 {
		fixing = json.RawMessage(`{}`)
	}
	_, err := tx.Exec(ctx, `
		INSERT INTO instruments_reference
		    (instrument_id, symbol, margin_rate, trading_hours,
		     contract_size, decimal_places, pip_size, tenor, fixing_calendar)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)
		ON CONFLICT (instrument_id) DO UPDATE SET
		    margin_rate     = EXCLUDED.margin_rate,
		    trading_hours   = EXCLUDED.trading_hours,
		    contract_size   = EXCLUDED.contract_size,
		    decimal_places  = EXCLUDED.decimal_places,
		    pip_size        = EXCLUDED.pip_size,
		    tenor           = EXCLUDED.tenor,
		    fixing_calendar = EXCLUDED.fixing_calendar,
		    updated_at      = now()`,
		r.InstrumentID, r.Symbol, r.MarginRate, trading,
		r.ContractSize, r.DecimalPlaces, r.PipSize, tenor, fixing)
	if err != nil {
		return excerrors.Wrap("INTERNAL_ERROR", "upsert instruments_reference", err)
	}
	return nil
}

// mergeDelistSchedule merges patch into
// instruments_reference.delist_schedule inside tx. The schedule object is
// the §7.5 ladder's durable state — written under FOR UPDATE so
// concurrent ladder steps serialize.
func mergeDelistSchedule(ctx context.Context, tx pgx.Tx,
	instrumentID int64, patch map[string]any) error {

	var cur json.RawMessage
	err := tx.QueryRow(ctx, `
		SELECT delist_schedule FROM instruments_reference
		WHERE instrument_id = $1 FOR UPDATE`, instrumentID).Scan(&cur)
	if errors.Is(err, pgx.ErrNoRows) {
		// The envelope may not exist yet for a manually created DRAFT —
		// seed it carrying only the schedule (the listing path fills the
		// rest when a full reference row is approved).
		cur = json.RawMessage(`{}`)
	} else if err != nil {
		return excerrors.Wrap("INTERNAL_ERROR", "lock delist schedule", err)
	}
	merged := map[string]any{}
	if err := json.Unmarshal(cur, &merged); err != nil {
		return excerrors.Wrap("INTERNAL_ERROR", "decode delist schedule", err)
	}
	for k, v := range patch {
		merged[k] = v
	}
	raw, err := json.Marshal(merged)
	if err != nil {
		return err
	}
	tag, err := tx.Exec(ctx, `
		UPDATE instruments_reference SET delist_schedule = $2, updated_at = now()
		WHERE instrument_id = $1`, instrumentID, raw)
	if err != nil {
		return excerrors.Wrap("INTERNAL_ERROR", "write delist schedule", err)
	}
	if tag.RowsAffected() == 0 {
		// No envelope row yet — create one minimally populated from the
		// instruments row so the schedule survives.
		if _, err := tx.Exec(ctx, `
			INSERT INTO instruments_reference
			    (instrument_id, symbol, margin_rate, contract_size,
			     decimal_places, pip_size, delist_schedule)
			SELECT id, symbol, ROUND(1.0 / max_leverage, 6),
			       contract_size, decimal_places, pip_size, $2
			FROM instruments WHERE id = $1`, instrumentID, raw); err != nil {
			return excerrors.Wrap("INTERNAL_ERROR", "seed delist schedule row", err)
		}
	}
	return nil
}
