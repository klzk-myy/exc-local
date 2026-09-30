// Package margin hosts the Phase-22 margin-recognition machinery that
// does not belong to the account-margin evaluator proper.
//
// spread_offsets.go — Phase-22 Task 22.3.13 (spec §15.7, §13.11,
// §24 #398): recognized option-spread margin offsets.
//
// Recognition contract (deterministic, fail-closed):
//   - Input is the account's open option legs. Output is a set of
//     RecognizedSpread pairs plus the residual naked legs.
//   - Four strategies: vertical spreads (same type+expiry, opposite
//     sides, different strikes — bound = max loss = strike width × qty),
//     straddles (short call + short put, same strike+expiry), strangles
//     (same but different strikes — bound = larger naked leg + the
//     configured fraction of the smaller), and calendar spreads (same
//     type+strike, short near expiry + long far expiry — bound = the
//     configured fraction of the near leg's naked margin).
//   - A leg is consumed by at most ONE spread (the §15.7
//     never-double-counted rule, enforced both here and inside
//     derivatives.IMAggregate).
//   - Bound margin never goes negative and never exceeds the legs' naked
//     margin sum.
//   - Offsets apply only in PORTFOLIO margin mode — the service layer
//     reads margin_accounts and refuses ISOLATED/CROSS accounts.
//   - Detection ordering is fixed: verticals first, then
//     straddle/strangle, then calendars; within a class legs are matched
//     by sorted position id so the result is stable across runs.
//   - spread_id is a deterministic key (acct:kind:legA:legB) — the live
//     unique index on option_spread_offsets makes re-detection idempotent.
//
// Admin configurability: option_spread_offset_params holds maker-checker
// rows (proposed_by ≠ approved_by enforced by CHECK); the service reads
// the ACTIVE row per type and falls back to the documented defaults when
// none is configured.
package margin

import (
	"context"
	"encoding/json"
	stderrors "errors"
	"fmt"
	"sort"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"exchange/internal/audit"
	"exchange/internal/risk"
	"exchange/pkg/decimal"
	excerrors "exchange/pkg/errors"
)

// Spread kinds — the option_spread_type_enum domain (migration 085).
const (
	SpreadVerticalCall = "VERTICAL_CALL"
	SpreadVerticalPut  = "VERTICAL_PUT"
	SpreadStraddle     = "STRADDLE"
	SpreadStrangle     = "STRANGLE"
	SpreadCalendar     = "CALENDAR"
)

// Row statuses — option_spread_offset_status_enum.
const (
	SpreadStatusDetected = "DETECTED"
	SpreadStatusApplied  = "APPLIED"
	SpreadStatusBroken   = "BROKEN"
	SpreadStatusRetired  = "RETIRED"
)

// Error codes (registered append-only in errs/codes.go).
const (
	// CodeSpreadOffsetParamInvalid — scaffold (Task 22.3.13): bad bps,
	// unknown type, or maker-checker violation.
	CodeSpreadOffsetParamInvalid = "SPREAD_OFFSET_PARAM_INVALID"
	// CodeSpreadOffsetInternal — scaffold (Task 22.3.13): store/read
	// degradation on the offsets path.
	CodeSpreadOffsetInternal = "SPREAD_OFFSET_INTERNAL"
)

// ---------------------------------------------------------------------------
// Types
// ---------------------------------------------------------------------------

// OptionLeg is one open option position leg — the detection input.
// NakedMarginUSD is the leg's standalone margin in USD (the caller's
// convention — delta-margin, premium-bound, or leverage-derived); a leg
// with zero naked margin cannot supply relief. ContractSize converts
// strike-width into quote currency; QuoteToUSD converts quote → USD.
type OptionLeg struct {
	PositionID     int64
	AccountID      int64
	InstrumentID   int64           // the OPTION instrument
	UnderlyingID   int64           // deliverable spot/forward instrument
	OptionType     string          // CALL | PUT (BINARY never spreads)
	Side           string          // LONG | SHORT
	Quantity       decimal.Decimal // open quantity
	Strike         decimal.Decimal
	ExpiryAt       time.Time
	NakedMarginUSD decimal.Decimal
	ContractSize   decimal.Decimal
	QuoteToUSD     decimal.Decimal
}

// SignedQty returns the side-signed quantity (SHORT is negative).
func (l OptionLeg) SignedQty() decimal.Decimal {
	if l.Side == "SHORT" {
		return l.Quantity.Neg()
	}
	return l.Quantity
}

// SpreadParams — the configured relief table (bps of the relieved leg's
// naked margin forgiven; VERTICAL_* rows are informational — verticals
// bind to max loss regardless of the configured bps).
type SpreadParams struct {
	OffsetsBps map[string]decimal.Decimal // kind → 0..10000
}

// DefaultSpreadParams — documented defaults when no ACTIVE admin row
// exists: straddle/strangle/calendar forgive 50% of the relieved leg;
// verticals always bind to max loss (bps recorded = achieved relief).
func DefaultSpreadParams() SpreadParams {
	half := decimal.NewFromInt(5000)
	full := decimal.NewFromInt(10000)
	return SpreadParams{OffsetsBps: map[string]decimal.Decimal{
		SpreadVerticalCall: full,
		SpreadVerticalPut:  full,
		SpreadStraddle:     half,
		SpreadStrangle:     half,
		SpreadCalendar:     half,
	}}
}

// OffsetBpsFor resolves the configured relief for a kind, falling back to
// the documented default.
func (p SpreadParams) OffsetBpsFor(kind string) decimal.Decimal {
	if v, ok := p.OffsetsBps[kind]; ok {
		return v
	}
	return DefaultSpreadParams().OffsetsBps[kind]
}

// RecognizedSpread is one detected pair — the persist row AND the
// margin-relief output.
type RecognizedSpread struct {
	ID             string // deterministic: acct:kind:legLow:legHigh
	Kind           string
	AccountID      int64
	InstrumentID   int64 // underlying instrument
	OrderID        *int64
	LongLegID      int64
	ShortLegID     int64 // 0 for all-long combos
	MatchedQty     decimal.Decimal
	OffsetBps      decimal.Decimal // relief actually applied
	OffsetUSD      decimal.Decimal // relief vs naked sum
	BoundMarginUSD decimal.Decimal // combined margin after relief
	LegRefs        []int64         // both leg position ids (audit)
}

// SpreadDetection is the engine output: recognized pairs plus legs that
// remain naked (unmatched or residual quantity).
type SpreadDetection struct {
	Spreads   []RecognizedSpread
	NakedLegs []OptionLeg // unconsumed position ids (subset of input legs)
}

// ---------------------------------------------------------------------------
// Detection — pure, deterministic
// ---------------------------------------------------------------------------

// legBucket groups detection candidates: same account + underlying.
type legBucket struct {
	accountID int64
	underID   int64
}

// DetectSpreads runs recognition over a leg set. Deterministic: legs are
// bucketed by (account, underlying) then scanned in position-id order;
// strategy classes run vertical → straddle/strangle → calendar; each leg
// is consumed by at most one spread (partial fills leave a residual
// naked leg — recorded in NakedLegs with the leftover qty).
func DetectSpreads(legs []OptionLeg, params SpreadParams) *SpreadDetection {
	sorted := append([]OptionLeg(nil), legs...)
	sort.Slice(sorted, func(i, j int) bool {
		if sorted[i].AccountID != sorted[j].AccountID {
			return sorted[i].AccountID < sorted[j].AccountID
		}
		if sorted[i].UnderlyingID != sorted[j].UnderlyingID {
			return sorted[i].UnderlyingID < sorted[j].UnderlyingID
		}
		return sorted[i].PositionID < sorted[j].PositionID
	})
	det := &SpreadDetection{}
	consumed := map[int64]bool{}
	buckets := map[legBucket][]OptionLeg{}
	var order []legBucket
	for _, l := range sorted {
		if l.OptionType == "BINARY" || !l.Quantity.IsPositive() {
			continue // binaries never spread; empty legs can't pair
		}
		b := legBucket{l.AccountID, l.UnderlyingID}
		if _, ok := buckets[b]; !ok {
			order = append(order, b)
		}
		buckets[b] = append(buckets[b], l)
	}
	for _, b := range order {
		legs := buckets[b]
		// 1. Verticals — same type+expiry, opposite sides.
		det.matchVerticals(legs, consumed, params)
		// 2. Straddles/strangles — same expiry, call+put, same side.
		det.matchStraddles(legs, consumed, params)
		// 3. Calendars — same type+strike, opposite sides, different expiry.
		det.matchCalendars(legs, consumed, params)
	}
	// Residual naked set: unconsumed legs in input order.
	for _, l := range sorted {
		if consumed[l.PositionID] || l.OptionType == "BINARY" || !l.Quantity.IsPositive() {
			continue
		}
		det.NakedLegs = append(det.NakedLegs, l)
	}
	return det
}

// emit records a recognized pair and marks both legs consumed.
func (d *SpreadDetection) emit(a, b OptionLeg, kind string,
	matchedQty, boundMargin, offsetBps decimal.Decimal, consumed map[int64]bool) {
	nakedSum := a.NakedMarginUSD.Add(b.NakedMarginUSD)
	if boundMargin.GreaterThan(nakedSum) {
		boundMargin = nakedSum // never exceed the naked sum
	}
	if boundMargin.IsNegative() {
		boundMargin = decimal.Zero
	}
	offset := nakedSum.Sub(boundMargin)
	if offset.IsNegative() {
		offset = decimal.Zero
	}
	lo, hi := a.PositionID, b.PositionID
	if lo > hi {
		lo, hi = hi, lo
	}
	longID, shortID := a.PositionID, int64(0)
	switch {
	case a.Side == "LONG" && b.Side == "SHORT":
		longID, shortID = a.PositionID, b.PositionID
	case b.Side == "LONG" && a.Side == "SHORT":
		longID, shortID = b.PositionID, a.PositionID
	case a.Side == "LONG" && b.Side == "LONG":
		longID = lo
	default: // both SHORT
		shortID = lo
		longID = hi
	}
	d.Spreads = append(d.Spreads, RecognizedSpread{
		ID:   fmt.Sprintf("%d:%s:%d:%d", a.AccountID, kind, lo, hi),
		Kind: kind, AccountID: a.AccountID, InstrumentID: a.UnderlyingID,
		LongLegID: longID, ShortLegID: shortID,
		MatchedQty: matchedQty, OffsetBps: offsetBps,
		OffsetUSD: offset, BoundMarginUSD: boundMargin,
		LegRefs: []int64{lo, hi},
	})
	consumed[a.PositionID] = true
	consumed[b.PositionID] = true
}

// matchVerticals pairs opposite-side legs sharing (type, expiry) within
// one bucket. Same-strike pairs collapse to zero bound (a flat combo —
// full relief); different strikes bind to the width × matched qty.
func (d *SpreadDetection) matchVerticals(legs []OptionLeg, consumed map[int64]bool, p SpreadParams) {
	type key struct {
		typ    string
		expiry time.Time
	}
	groups := map[key][]int{}
	var keys []key
	for i, l := range legs {
		k := key{l.OptionType, l.ExpiryAt.UTC().Truncate(time.Microsecond)}
		if _, ok := groups[k]; !ok {
			keys = append(keys, k)
		}
		groups[k] = append(groups[k], i)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].typ != keys[j].typ {
			return keys[i].typ < keys[j].typ
		}
		return keys[i].expiry.Before(keys[j].expiry)
	})
	for _, k := range keys {
		var longs, shorts []int
		for _, i := range groups[k] {
			switch legs[i].Side {
			case "LONG":
				longs = append(longs, i)
			case "SHORT":
				shorts = append(shorts, i)
			}
		}
		for _, li := range longs {
			if consumed[legs[li].PositionID] {
				continue
			}
			for _, si := range shorts {
				if consumed[legs[si].PositionID] {
					continue
				}
				a, b := legs[li], legs[si]
				matched := decimal.Min(a.Quantity, b.Quantity)
				kind := SpreadVerticalCall
				if a.OptionType == "PUT" {
					kind = SpreadVerticalPut
				}
				width := a.Strike.Sub(b.Strike).Abs()
				bound := width.Mul(matched).Mul(a.ContractSize).Mul(a.QuoteToUSD).Round(8)
				// record achieved relief bps: offset = nakedSum − bound
				nakedSum := a.NakedMarginUSD.Add(b.NakedMarginUSD)
				bps := decimal.Zero
				if nakedSum.IsPositive() {
					bps = decimal.Max(decimal.Zero, nakedSum.Sub(bound)).
						Div(nakedSum).Mul(decimal.NewFromInt(10000)).Round(4)
				}
				d.emit(a, b, kind, matched, bound, bps, consumed)
				break // each leg pairs at most once (single-consumption)
			}
		}
	}
}

// matchStraddles pairs same-expiry CALL+PUT legs of the SAME side
// (short-short or long-long). Same strike = straddle; different strikes
// = strangle. Short combos: bound = max(nakedA, nakedB) +
// (1−bps)·min(nakedA,nakedB). Long combos: bound = 0 (premium already
// paid — max loss bounded by definition).
func (d *SpreadDetection) matchStraddles(legs []OptionLeg, consumed map[int64]bool, p SpreadParams) {
	type key struct{ expiry time.Time }
	groups := map[time.Time][]int{}
	var keys []time.Time
	for i, l := range legs {
		exp := l.ExpiryAt.UTC().Truncate(time.Microsecond)
		if _, ok := groups[exp]; !ok {
			keys = append(keys, exp)
		}
		groups[exp] = append(groups[exp], i)
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i].Before(keys[j]) })
	for _, exp := range keys {
		var calls, puts []int
		for _, i := range groups[exp] {
			switch legs[i].OptionType {
			case "CALL":
				calls = append(calls, i)
			case "PUT":
				puts = append(puts, i)
			}
		}
		for _, ci := range calls {
			a := legs[ci]
			if consumed[a.PositionID] {
				continue
			}
			for _, pi := range puts {
				b := legs[pi]
				if consumed[b.PositionID] || a.Side != b.Side {
					continue
				}
				matched := decimal.Min(a.Quantity, b.Quantity)
				kind := SpreadStrangle
				if a.Strike.Equal(b.Strike) {
					kind = SpreadStraddle
				}
				var bound decimal.Decimal
				bps := p.OffsetBpsFor(kind)
				if a.Side == "SHORT" {
					hi := decimal.Max(a.NakedMarginUSD, b.NakedMarginUSD)
					lo := decimal.Min(a.NakedMarginUSD, b.NakedMarginUSD)
					reliefFrac := bps.Div(decimal.NewFromInt(10000))
					bound = hi.Add(lo.Mul(decimal.NewFromInt(1).Sub(reliefFrac))).Round(8)
				} else {
					bound = decimal.Zero // all-long: max loss = premium paid
					bps = decimal.NewFromInt(10000)
				}
				d.emit(a, b, kind, matched, bound, bps, consumed)
				break
			}
		}
	}
}

// matchCalendars pairs same (type, strike) legs with opposite sides and
// different expiries — the SHORT leg must be the nearer expiry (a long
// calendar). Bound = (1−bps)·short naked margin.
func (d *SpreadDetection) matchCalendars(legs []OptionLeg, consumed map[int64]bool, p SpreadParams) {
	type key struct {
		typ    string
		strike string // decimal text — decimal.Decimal isn't a value key
	}
	groups := map[key][]int{}
	var keys []key
	for i, l := range legs {
		k := key{l.OptionType, l.Strike.String()}
		if _, ok := groups[k]; !ok {
			keys = append(keys, k)
		}
		groups[k] = append(groups[k], i)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].typ != keys[j].typ {
			return keys[i].typ < keys[j].typ
		}
		return keys[i].strike < keys[j].strike
	})
	for _, k := range keys {
		for _, i := range groups[k] {
			a := legs[i]
			if consumed[a.PositionID] || a.Side != "SHORT" {
				continue
			}
			for _, j := range groups[k] {
				b := legs[j]
				if consumed[b.PositionID] || b.Side != "LONG" {
					continue
				}
				if !a.ExpiryAt.Before(b.ExpiryAt) {
					continue // short leg must be the nearer expiry
				}
				matched := decimal.Min(a.Quantity, b.Quantity)
				bps := p.OffsetBpsFor(SpreadCalendar)
				reliefFrac := bps.Div(decimal.NewFromInt(10000))
				bound := a.NakedMarginUSD.Mul(decimal.NewFromInt(1).Sub(reliefFrac)).Round(8)
				d.emit(a, b, SpreadCalendar, matched, bound, bps, consumed)
				break
			}
		}
	}
}

// ---------------------------------------------------------------------------
// Persistence + admin parameters
// ---------------------------------------------------------------------------

// SpreadOffsetService owns detection over live books plus the
// admin-configurable param lifecycle (maker-checker).
type SpreadOffsetService struct {
	pool *pgxpool.Pool
	logf func(string, ...any)
	now  func() time.Time
	// nakedMargin computes a leg's standalone USD margin — injected (the
	// margin engine owns leg pricing). nil = NakedMarginUSD column on the
	// input legs is used verbatim (callers supply it).
	nakedMargin func(ctx context.Context, leg OptionLeg) (decimal.Decimal, error)
}

// NewSpreadOffsetService wires the service; pool is required.
func NewSpreadOffsetService(pool *pgxpool.Pool,
	nakedMargin func(ctx context.Context, leg OptionLeg) (decimal.Decimal, error),
	logf func(string, ...any)) (*SpreadOffsetService, error) {
	if pool == nil {
		return nil, excerrors.New(CodeSpreadOffsetInternal, "spread offsets: nil pool")
	}
	if logf == nil {
		logf = func(string, ...any) {}
	}
	return &SpreadOffsetService{pool: pool, nakedMargin: nakedMargin,
		logf: logf, now: func() time.Time { return time.Now().UTC() }}, nil
}

// OptionLegSource feeds detection — the derivatives book binds it.
// Production: PgOptionLegSource (below) over positions+option_positions.
type OptionLegSource interface {
	OpenOptionLegs(ctx context.Context, accountID int64) ([]OptionLeg, error)
}

// Recompute re-detects an account's spreads and reconciles the
// option_spread_offsets rows: new detections INSERT (idempotent on the
// spread_id live-unique), vanished spreads transition DETECTED/APPLIED →
// BROKEN. Runs inside a SERIALIZABLE tx. Returns the live detections.
// PORTFOLIO mode gate: non-PORTFOLIO accounts get zero offsets.
func (s *SpreadOffsetService) Recompute(ctx context.Context, src OptionLegSource,
	accountID int64) (*SpreadDetection, error) {
	mode, err := s.marginMode(ctx, accountID)
	if err != nil {
		return nil, excerrors.Wrap(CodeSpreadOffsetInternal, "spread: mode read", err)
	}
	if mode != "PORTFOLIO" {
		return &SpreadDetection{}, nil // offsets apply only in PORTFOLIO mode
	}
	legs, err := src.OpenOptionLegs(ctx, accountID)
	if err != nil {
		return nil, excerrors.Wrap(CodeSpreadOffsetInternal, "spread: legs read", err)
	}
	if s.nakedMargin != nil {
		for i := range legs {
			m, err := s.nakedMargin(ctx, legs[i])
			if err != nil {
				return nil, excerrors.Wrap(CodeSpreadOffsetInternal, "spread: naked margin", err)
			}
			legs[i].NakedMarginUSD = m
		}
	}
	params, err := s.ActiveParams(ctx)
	if err != nil {
		return nil, err
	}
	det := DetectSpreads(legs, params)
	if err := s.persistDetections(ctx, det, accountID); err != nil {
		return nil, excerrors.Wrap(CodeSpreadOffsetInternal, "spread: persist", err)
	}
	return det, nil
}

// marginMode reads margin_accounts.margin_mode; absent row = CROSS
// (Phase-19 default) — never PORTFOLIO by accident.
func (s *SpreadOffsetService) marginMode(ctx context.Context, accountID int64) (string, error) {
	var mode string
	err := s.pool.QueryRow(ctx,
		`SELECT margin_mode FROM margin_accounts WHERE account_id=$1`, accountID).Scan(&mode)
	if stderrors.Is(err, pgx.ErrNoRows) {
		return "CROSS", nil
	}
	return mode, err
}

// persistDetections reconciles live detections against stored rows for
// the account.
func (s *SpreadOffsetService) persistDetections(ctx context.Context, det *SpreadDetection, accountID int64) error {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	for _, sp := range det.Spreads {
		var shortID *int64
		if sp.ShortLegID != 0 {
			shortID = &sp.ShortLegID
		}
		_, err := tx.Exec(ctx, `
			INSERT INTO option_spread_offsets
			    (spread_id, account_id, instrument_id, order_id, spread_type,
			     long_position_id, short_position_id, matched_qty,
			     offset_bps, offset_amount_usd, bound_margin_usd, status)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,'APPLIED')
			ON CONFLICT (spread_id) WHERE status IN ('DETECTED','APPLIED')
			DO UPDATE SET matched_qty=EXCLUDED.matched_qty,
			    offset_bps=EXCLUDED.offset_bps,
			    offset_amount_usd=EXCLUDED.offset_amount_usd,
			    bound_margin_usd=EXCLUDED.bound_margin_usd,
			    status='APPLIED', updated_at=now()`,
			sp.ID, sp.AccountID, sp.InstrumentID, sp.OrderID, sp.Kind,
			sp.LongLegID, shortID, sp.MatchedQty, sp.OffsetBps,
			sp.OffsetUSD, sp.BoundMarginUSD)
		if err != nil {
			return err
		}
	}
	// Spreads no longer detected → BROKEN (legs closed/flattened).
	live := make([]string, 0, len(det.Spreads))
	for _, sp := range det.Spreads {
		live = append(live, sp.ID)
	}
	q := `UPDATE option_spread_offsets SET status='BROKEN', updated_at=now()
	      WHERE account_id=$1 AND status IN ('DETECTED','APPLIED')`
	if len(live) == 0 {
		_, err = tx.Exec(ctx, q, accountID)
	} else {
		_, err = tx.Exec(ctx, q+` AND spread_id <> ALL($2)`, accountID, live)
	}
	if err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// LiveOffsets returns the account's applied spreads — the feed the IM
// aggregation consumes (derivatives.UMRIMService via IMSpreadOffsetSource).
func (s *SpreadOffsetService) LiveOffsets(ctx context.Context, accountID int64) ([]RecognizedSpread, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT spread_id, spread_type, account_id, instrument_id, order_id,
		       long_position_id, COALESCE(short_position_id,0), matched_qty,
		       offset_bps, offset_amount_usd, bound_margin_usd
		FROM option_spread_offsets
		WHERE account_id=$1 AND status='APPLIED' ORDER BY spread_id`, accountID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []RecognizedSpread
	for rows.Next() {
		var sp RecognizedSpread
		var orderID *int64
		if err := rows.Scan(&sp.ID, &sp.Kind, &sp.AccountID, &sp.InstrumentID, &orderID,
			&sp.LongLegID, &sp.ShortLegID, &sp.MatchedQty, &sp.OffsetBps,
			&sp.OffsetUSD, &sp.BoundMarginUSD); err != nil {
			return nil, err
		}
		sp.OrderID = orderID
		lo, hi := sp.LongLegID, sp.ShortLegID
		sp.LegRefs = []int64{lo}
		if hi != 0 && hi != lo {
			sp.LegRefs = append(sp.LegRefs, hi)
		}
		out = append(out, sp)
	}
	return out, rows.Err()
}

// SpreadOffsets satisfies risk.SpreadOffsetSource — the Phase-19
// margin aggregator consumes this feed directly (pre-SIMM application
// point; risk.SpreadOffsetBook.Apply guards second application).
func (s *SpreadOffsetService) SpreadOffsets(accountID int64) ([]risk.SpreadOffset, error) {
	live, err := s.LiveOffsets(context.Background(), accountID)
	if err != nil {
		return nil, err
	}
	out := make([]risk.SpreadOffset, 0, len(live))
	for _, sp := range live {
		out = append(out, risk.SpreadOffset{
			AccountID:  sp.AccountID,
			LongLegID:  sp.LongLegID,
			ShortLegID: sp.ShortLegID,
			OffsetUSD:  sp.OffsetUSD,
			Strategy:   sp.Kind,
		})
	}
	return out, nil
}

// ActiveParams loads the ACTIVE option_spread_offset_params rows into a
// SpreadParams; defaults fill any unconfigured kind.
func (s *SpreadOffsetService) ActiveParams(ctx context.Context) (SpreadParams, error) {
	p := DefaultSpreadParams()
	rows, err := s.pool.Query(ctx, `
		SELECT spread_type, offset_bps FROM option_spread_offset_params
		WHERE status='ACTIVE'`)
	if err != nil {
		return p, excerrors.Wrap(CodeSpreadOffsetInternal, "spread: params read", err)
	}
	defer rows.Close()
	for rows.Next() {
		var kind string
		var bps decimal.Decimal
		if err := rows.Scan(&kind, &bps); err != nil {
			return p, err
		}
		p.OffsetsBps[kind] = bps
	}
	return p, rows.Err()
}

// ProposeOffset submits a param change (PENDING_APPROVAL). A second
// principal must Approve before it activates — the DB CHECK enforces
// proposer ≠ approver even if this gate is bypassed.
func (s *SpreadOffsetService) ProposeOffset(ctx context.Context, kind string,
	bps decimal.Decimal, proposedBy string) (int64, error) {
	if _, ok := DefaultSpreadParams().OffsetsBps[kind]; !ok {
		return 0, excerrors.New(CodeSpreadOffsetParamInvalid,
			fmt.Sprintf("unknown spread type %q", kind))
	}
	if bps.IsNegative() || bps.GreaterThan(decimal.NewFromInt(10000)) {
		return 0, excerrors.New(CodeSpreadOffsetParamInvalid,
			fmt.Sprintf("offset_bps %s out of range 0..10000", bps.String()))
	}
	if proposedBy == "" {
		return 0, excerrors.New(CodeSpreadOffsetParamInvalid, "proposed_by required")
	}
	var id int64
	err := s.pool.QueryRow(ctx, `
		INSERT INTO option_spread_offset_params
		    (spread_type, offset_bps, status, proposed_by)
		VALUES ($1,$2,'PENDING_APPROVAL',$3)
		RETURNING id`, kind, bps, proposedBy).Scan(&id)
	if err != nil {
		return 0, err
	}
	s.appendAudit(ctx, &id, "SPREAD_OFFSET_PARAM_PROPOSED", map[string]any{
		"spread_type": kind, "offset_bps": bps.String(), "proposed_by": proposedBy})
	return id, nil
}

// Approve activates a proposed param row — maker-checker: approver must
// differ from proposer; the prior ACTIVE row for the type retires in the
// same SERIALIZABLE tx.
func (s *SpreadOffsetService) Approve(ctx context.Context, id int64, approvedBy string) error {
	if approvedBy == "" {
		return excerrors.New(CodeSpreadOffsetParamInvalid, "approved_by required")
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	var kind, proposer string
	err = tx.QueryRow(ctx, `
		SELECT spread_type, proposed_by FROM option_spread_offset_params
		WHERE id=$1 AND status='PENDING_APPROVAL' FOR UPDATE`, id).Scan(&kind, &proposer)
	if stderrors.Is(err, pgx.ErrNoRows) {
		return excerrors.New(CodeSpreadOffsetParamInvalid,
			fmt.Sprintf("spread offset param %d not pending approval", id))
	}
	if err != nil {
		return err
	}
	if proposer == approvedBy {
		return excerrors.New(CodeSpreadOffsetParamInvalid,
			"maker-checker: approver must differ from proposer")
	}
	if _, err := tx.Exec(ctx, `
		UPDATE option_spread_offset_params SET status='RETIRED', updated_at=now()
		WHERE spread_type=$1 AND status='ACTIVE'`, kind); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `
		UPDATE option_spread_offset_params
		SET status='ACTIVE', approved_by=$2, updated_at=now()
		WHERE id=$1`, id, approvedBy); err != nil {
		return err
	}
	s.appendAuditTx(ctx, tx, &id, "SPREAD_OFFSET_PARAM_APPROVED", map[string]any{
		"spread_type": kind, "approved_by": approvedBy})
	return tx.Commit(ctx)
}

// appendAudit writes an audit row outside a tx (best-effort).
func (s *SpreadOffsetService) appendAudit(ctx context.Context, recordID *int64, action string, payload map[string]any) {
	b, _ := json.Marshal(payload)
	_, err := audit.AppendAuto(ctx, s.pool, "option_spread_offset_params", recordID, action, b)
	if err != nil {
		s.logf("spread: audit append %s: %v", action, err)
	}
}

// appendAuditTx writes an audit row inside the caller's tx.
func (s *SpreadOffsetService) appendAuditTx(ctx context.Context, tx pgx.Tx, recordID *int64, action string, payload map[string]any) {
	b, _ := json.Marshal(payload)
	if _, err := audit.Append(ctx, tx, "option_spread_offset_params", recordID, action, b); err != nil {
		s.logf("spread: audit append %s: %v", action, err)
	}
}

// ---------------------------------------------------------------------------
// PgOptionLegSource — production leg feed
// ---------------------------------------------------------------------------

// PgOptionLegSource loads open option legs for an account from
// positions+option_positions (migration 255), joined to instruments for
// contract_size/base+quote. NakedMarginUSD/QuoteToUSD are caller-filled
// (margin-engine seam) or zero — zero naked margin ⇒ zero relief.
type PgOptionLegSource struct {
	Pool *pgxpool.Pool
}

// OpenOptionLegs implements OptionLegSource.
func (s PgOptionLegSource) OpenOptionLegs(ctx context.Context, accountID int64) ([]OptionLeg, error) {
	rows, err := s.Pool.Query(ctx, `
		SELECT o.position_id, o.account_id, o.instrument_id,
		       COALESCE(o.underlying_instrument_id, i.id),
		       o.option_type, o.side::text, o.quantity, o.strike,
		       o.expiry_at, COALESCE(i.contract_size, 1)
		FROM option_positions o
		JOIN instruments i ON i.id = o.instrument_id
		WHERE o.account_id=$1 AND o.status='OPEN'
		ORDER BY o.position_id`, accountID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []OptionLeg
	for rows.Next() {
		var l OptionLeg
		if err := rows.Scan(&l.PositionID, &l.AccountID, &l.InstrumentID,
			&l.UnderlyingID, &l.OptionType, &l.Side, &l.Quantity, &l.Strike,
			&l.ExpiryAt, &l.ContractSize); err != nil {
			return nil, err
		}
		l.QuoteToUSD = decimal.NewFromInt(1) // caller's nakedMargin seam re-denominates
		out = append(out, l)
	}
	return out, rows.Err()
}
