package oracle

import (
	"sync"
	"time"

	"exchange/pkg/decimal"
)

// ---------------------------------------------------------------------------
// Stale-price fallback & flash-crash detection (Task 19.5.3.6,
// oracle side).
//
// When a symbol's fresh cohort collapses the service keeps publishing —
// not a fabricated mark, but a STALE_REFERENCE carrying the last valid
// mark plus the tier the liquidation engine must apply (2%/5%/10%
// haircut; auction-only at ≥15s; FORCE_CASH at >60s). The risk-side
// consumer is internal/risk/liquidation_fallback.go.
//
// Flash crash: a >5% mark move inside 1s immediately before staleness
// engages FLASH_COOL — a 5s cooling freeze (liquidation HALT signal),
// then the tiered ladder takes over. This stops a spurious spike from
// cascading stop-outs before feeds recover.
// ---------------------------------------------------------------------------

// FallbackState is the per-symbol stale-liquidation signal published to
// Redis for the risk engine.
type FallbackState string

const (
	// FallbackStalePrice — tiered haircut ladder active.
	FallbackStalePrice FallbackState = "STALE_PRICE"
	// FallbackFlashCool — 5s crash freeze; liquidation halts.
	FallbackFlashCool FallbackState = "FLASH_COOL"
)

// StaleReference is the published stale-fallback record for a symbol.
type StaleReference struct {
	Symbol   string
	State    FallbackState
	Tier     StalenessTier
	LastMark decimal.Decimal
	LastAt   time.Time // stamp of the last fresh mark
	SinceAt  time.Time // when staleness began (tier anchor)
	// FreezeUntil is non-zero only under FLASH_COOL — liquidation halts
	// until this stamp (5s cooling), then the tier ladder applies.
	FreezeUntil time.Time
}

// ReferencePrice is the liquidation reference for the tier: last mark
// adjusted by the tier haircut in the direction pessimistic for the
// liquidated side — closing a long sells lower, closing a short buys
// higher (§2.7 zero-loss pessimism).
func (r StaleReference) ReferencePrice(sideLong bool) decimal.Decimal {
	h := r.Tier.HaircutPct()
	if !r.LastMark.IsPositive() || !h.IsPositive() {
		return r.LastMark
	}
	if sideLong {
		return r.LastMark.Mul(decimal.One.Sub(h))
	}
	return r.LastMark.Mul(decimal.One.Add(h))
}

// Redis key contracts for the fallback channel — the risk-side consumer
// reads these (internal/risk/liquidation_fallback.go).
func FallbackKey(sym string) string       { return "oracle:fallback:" + sym }
func FallbackFreezeKey(sym string) string { return "oracle:fallback:" + sym + ":freeze" }

// Flash-crash constants — spec Task 19.5.3.6 step 3.
var (
	// FlashMoveThreshold is the >5% mark move that arms the breaker.
	FlashMoveThreshold = decimal.RequireFromString("0.05")
	// FlashMoveWindow bounds the move — >5% inside 1s.
	FlashMoveWindow = time.Second
	// FlashCoolingPeriod is the liquidation freeze length (5s).
	FlashCoolingPeriod = 5 * time.Second
)

// FallbackTracker is the oracle-side stale-liquidation state machine.
// The service calls Observe once per symbol per evaluation round;
// fresh results refresh the baseline (and clear the episode), stale
// results produce the StaleReference the publisher fans out.
type FallbackTracker struct {
	mu      sync.Mutex
	prev    map[string]decimal.Decimal // latest fresh mark per symbol
	prevAt  map[string]time.Time
	prev2   map[string]decimal.Decimal // mark before latest (move detection)
	prev2At map[string]time.Time
	since   map[string]time.Time // staleness onset per symbol
	freeze  map[string]time.Time // active freeze expiry per symbol
	flashed map[string]bool      // freeze already engaged this episode
	now     func() time.Time
}

// NewFallbackTracker returns an empty tracker.
func NewFallbackTracker(now func() time.Time) *FallbackTracker {
	if now == nil {
		now = time.Now
	}
	return &FallbackTracker{
		prev: map[string]decimal.Decimal{}, prevAt: map[string]time.Time{},
		prev2: map[string]decimal.Decimal{}, prev2At: map[string]time.Time{},
		since: map[string]time.Time{}, freeze: map[string]time.Time{},
		flashed: map[string]bool{}, now: now,
	}
}

// Observe ingests one symbol's evaluation round. It returns the
// StaleReference to publish — nil while the symbol is healthy or when
// there is no prior mark to anchor the ladder on.
func (t *FallbackTracker) Observe(r MarkResult, staleAge float64) *StaleReference {
	now := t.now()
	t.mu.Lock()
	defer t.mu.Unlock()

	if r.OK {
		// Slide the baseline: prev2 captures the mark one publish back
		// so the pre-staleness spike is measurable on episode onset.
		if cur, ok := t.prev[r.Symbol]; ok {
			t.prev2[r.Symbol], t.prev2At[r.Symbol] = cur, t.prevAt[r.Symbol]
		}
		t.prev[r.Symbol], t.prevAt[r.Symbol] = r.Mark, r.At
		delete(t.since, r.Symbol)
		delete(t.freeze, r.Symbol)
		delete(t.flashed, r.Symbol)
		return nil
	}

	since, ok := t.since[r.Symbol]
	if !ok {
		since = now
		t.since[r.Symbol] = since
	}
	last, lok := t.prev[r.Symbol]
	lastAt := t.prevAt[r.Symbol]
	if !lok || !last.IsPositive() {
		return nil // no last mark — nothing to anchor the ladder on
	}
	if staleAge < 0 {
		staleAge = now.Sub(since).Seconds()
	}
	tier := TierStaleness(time.Duration(staleAge * float64(time.Second)))
	if tier == TierFresh {
		return nil
	}
	ref := &StaleReference{
		Symbol: r.Symbol, State: FallbackStalePrice, Tier: tier,
		LastMark: last, LastAt: lastAt, SinceAt: since,
	}

	// Flash-crash trigger: the last two fresh marks spanned >5% inside
	// the 1s move window AND the outage began within 1s of the spike —
	// i.e. the market ran then the feeds died. One freeze per episode.
	if !t.flashed[r.Symbol] {
		if p2, p2at, ok2 := t.prev2[r.Symbol], t.prev2At[r.Symbol], true; ok2 {
			if p2.IsPositive() && lastAt.Sub(p2at) <= FlashMoveWindow {
				move := last.Sub(p2).Abs().Div(p2)
				if move.GreaterThan(FlashMoveThreshold) &&
					since.Sub(lastAt) <= FlashMoveWindow {
					t.freeze[r.Symbol] = now.Add(FlashCoolingPeriod)
					t.flashed[r.Symbol] = true
				}
			}
		}
	}
	if fz, ok := t.freeze[r.Symbol]; ok && now.Before(fz) {
		ref.State = FallbackFlashCool
		ref.FreezeUntil = fz
	}
	return ref
}

// Frozen reports whether symbol's liquidation is inside the cooling
// window — the risk-side consumer double-checks against the published
// freeze key, this is the in-process fast path.
func (t *FallbackTracker) Frozen(symbol string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	fz, ok := t.freeze[symbol]
	return ok && t.now().Before(fz)
}
