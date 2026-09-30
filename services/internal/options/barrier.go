package options

import (
	"context"
	"fmt"
	"time"

	pkgerrors "exchange/pkg/errors"
)

// ---------------------------------------------------------------------------
// BarrierOption — knock-in / knock-out FX options (Phase-22 Task 22.3.5;
// spec §15.1, §15.7; §24 #61). Pricing is Monte Carlo (MCPricer.
// PriceBarrier); this file owns the contract geometry, the expiry payoff,
// and the live monitoring state machine.
// ---------------------------------------------------------------------------

// BarrierOption is a vanilla option conditioned on a barrier touch.
// The option always has an underlying European right/strike — the
// barrier only gates whether that claim is alive at expiry.
type BarrierOption struct {
	Right  OptionRight   // CALL or PUT payoff of the surviving option
	Strike float64       // strike rate, quote per base
	Style  ExerciseStyle // only ExerciseEuropean (Americans → lattice)
	Type   BarrierType   // UP_AND_IN / UP_AND_OUT / DOWN_AND_IN / DOWN_AND_OUT
	Level  float64       // barrier rate, quote per base
}

// validate enforces the fail-closed contract — geometry checks included:
// a non-positive barrier level is meaningless, and an American barrier
// is refused for the same reason as VanillaOption.
func (o BarrierOption) validate(op string) error {
	switch {
	case !o.Right.Valid():
		return invalidInput(op, fmt.Sprintf("option right must be CALL|PUT, got %q", o.Right))
	case !finite(o.Strike) || o.Strike <= 0:
		return invalidInput(op, fmt.Sprintf("strike must be positive and finite, got %v", o.Strike))
	case o.Style != ExerciseEuropean:
		return invalidInput(op,
			fmt.Sprintf("exercise style %q not priced here — American barriers require the lattice pricer (spec §15.2)", o.Style))
	case !o.Type.Valid():
		return invalidInput(op, fmt.Sprintf("barrier type must be UP_AND_IN|UP_AND_OUT|DOWN_AND_IN|DOWN_AND_OUT, got %q", o.Type))
	case !finite(o.Level) || o.Level <= 0:
		return invalidInput(op, fmt.Sprintf("barrier level must be positive and finite, got %v", o.Level))
	}
	return nil
}

// Alive reports whether the option's claim survives given the knock
// state: knock-in options live iff touched; knock-out options live iff
// never touched.
func (o BarrierOption) Alive(knocked bool) bool {
	if o.Type.IsKnockIn() {
		return knocked
	}
	return !knocked
}

// ExpiryPayoff is the cash-settlement amount per unit of base notional
// (Task 22.3.5 step 6 — cash settlement at expiry): the vanilla
// intrinsic when the claim is alive, zero otherwise.
func (o BarrierOption) ExpiryPayoff(sT float64, knocked bool) float64 {
	if !o.Alive(knocked) {
		return 0
	}
	return VanillaOption{Right: o.Right, Strike: o.Strike, Style: ExerciseEuropean}.ExpiryPayoff(sT)
}

// ---------------------------------------------------------------------------
// BarrierMonitor — the discrete mark-tick knock evaluation contract
// (spec §15.7 item 3, Task 22.3.5 AC "monitored continuously against
// mark price"):
//   - knock events evaluate on discrete mark ticks fed by the Phase-19.5
//     oracle publisher, via EvaluateBarrierTick (Task 22.3.15 — the
//     single owner of the tick-evaluation semantics);
//   - ticks outside the 5s staleness gate are skipped — a missed touch
//     on a dead feed NEVER fabricates a knock;
//   - out-of-order or non-finite ticks are rejected (fail-closed);
//   - a post-gap tick (MarkTick.PostGap, or > GapThreshold silence)
//     carries the gap flag — the weekend-gap rule (first post-gap mark
//     applies, flagged);
//   - a barrier knocks at most once.
// ---------------------------------------------------------------------------

// DefaultGapThreshold flags gaps longer than 30s — comfortably below the
// ~71h Friday-close→Sunday-open weekend silence so the first post-gap
// mark always carries the flag (spec §15.7 item 3).
const DefaultGapThreshold = 30 * time.Second

// ---------------------------------------------------------------------------
// Per-tick evaluation primitives — the §15.7 item-3 contract shared by
// the monitor below and sibling callers (determinism_test.go).
// ---------------------------------------------------------------------------

// MarkTick is one discrete mark-price observation from the Phase-19.5
// oracle publisher. Producers set Stale via MarkStale(now, tick.At)
// (determinism.go) before feeding the monitor — the 5s staleness gate
// is symmetric (a future-dated mark is as suspect as a lagged one).
type MarkTick struct {
	Price float64
	At    time.Time // mark timestamp
	// PostGap flags the first mark after a weekend/market gap (the
	// producer sets it; the monitor also infers gaps from silence).
	PostGap bool
	// Stale marks the tick non-evaluable — a missed touch on a dead
	// feed never fabricates a knock (spec §15.7 item 3).
	Stale bool
}

// BarrierKind enumerates the monitored single-barrier directions —
// the evaluation vocabulary EvaluateBarrierTick consumes. The spec §5.4
// persistence enum is BarrierType (types.go); monitorBarrierKind maps
// between them.
type BarrierKind int

const (
	BarrierKnockOutUp BarrierKind = iota
	BarrierKnockOutDown
	BarrierKnockInUp
	BarrierKnockInDown
)

func (k BarrierKind) String() string {
	switch k {
	case BarrierKnockOutUp:
		return "KO_UP"
	case BarrierKnockOutDown:
		return "KO_DOWN"
	case BarrierKnockInUp:
		return "KI_UP"
	case BarrierKnockInDown:
		return "KI_DOWN"
	default:
		return "UNKNOWN"
	}
}

// IsKnockIn reports whether the barrier activates (vs kills) the option.
func (k BarrierKind) IsKnockIn() bool {
	return k == BarrierKnockInUp || k == BarrierKnockInDown
}

// IsUp reports whether the barrier sits above the reference price.
func (k BarrierKind) IsUp() bool {
	return k == BarrierKnockOutUp || k == BarrierKnockInUp
}

// IsDown reports whether the barrier sits below the reference price.
func (k BarrierKind) IsDown() bool {
	return k == BarrierKnockOutDown || k == BarrierKnockInDown
}

// BarrierEvent is the deterministic verdict for one tick.
type BarrierEvent struct {
	Triggered bool        // touch evaluated on this tick
	Kind      BarrierKind // echo of the barrier evaluated
	Mark      float64     // mark that evaluated (0 when stale)
	GapTouch  bool        // touch landed on a post-gap mark
	Reason    string      // audit reason code
}

// Barrier reason codes — stable tokens for the audit trail.
const (
	BarrierReasonKnockOut = "KNOCK_OUT_TOUCH"
	BarrierReasonKnockIn  = "KNOCK_IN_TOUCH"
	BarrierReasonNoTouch  = "NO_TOUCH"
	BarrierReasonStale    = "STALE_MARK"
)

// EvaluateBarrierTick evaluates one discrete mark tick against one
// barrier. Discrete semantics per §15.7 item 3: an up-barrier touches
// when mark ≥ level, a down-barrier when mark ≤ level — no intra-tick
// interpolation is fabricated. A tick outside the 5s staleness gate
// (MarkStale — in either direction; a future-dated mark is equally
// suspect) or flagged Stale by the caller returns
// CONDITIONAL_TRIGGER_ORACLE_STALE with Triggered=false: a missed touch
// on a dead feed never fabricates a knock.
func EvaluateBarrierTick(now time.Time, tick MarkTick, kind BarrierKind,
	level float64) (BarrierEvent, error) {
	const op = "EvaluateBarrierTick"
	if !finite(tick.Price) || tick.Price <= 0 || !finite(level) || level <= 0 {
		return BarrierEvent{}, invalidInput(op,
			"non-positive/non-finite mark or level")
	}
	if tick.Stale || MarkStale(now, tick.At) {
		return BarrierEvent{Kind: kind, Reason: BarrierReasonStale},
			pkgerrors.New(CodeConditionalTriggerOracleStale,
				fmt.Sprintf("options.%s: mark age %s outside the 5s gate", op, now.Sub(tick.At)))
	}
	if !kind.IsUp() && !kind.IsDown() {
		return BarrierEvent{}, invalidInput(op,
			fmt.Sprintf("unknown barrier kind %d", int(kind)))
	}
	touched := barrierTouched(kind, tick.Price, level)
	ev := BarrierEvent{
		Triggered: touched, Kind: kind, Mark: tick.Price,
		GapTouch: touched && tick.PostGap,
		Reason:   BarrierReasonNoTouch,
	}
	if touched {
		if kind.IsKnockIn() {
			ev.Reason = BarrierReasonKnockIn
		} else {
			ev.Reason = BarrierReasonKnockOut
		}
	}
	return ev, nil
}

// barrierTouched applies the discrete per-tick comparison shared by
// EvaluateBarrierTick and the monitor — up-barrier: mark ≥ level,
// down-barrier: mark ≤ level. kind must already be validated.
func barrierTouched(kind BarrierKind, mark, level float64) bool {
	if kind.IsUp() {
		return mark >= level
	}
	return mark <= level
}

// KnockEvent is the outcome of evaluating one mark tick through the
// monitor — it wraps the EvaluateBarrierTick verdict (the §15.7
// discrete-tick evaluation and 5s staleness gate above) with the
// monitor's latch state.
type KnockEvent struct {
	BarrierType BarrierType `json:"barrier_type"`
	Level       float64     `json:"level"`
	// Price and At are the evaluated mark (zero when Evaluated is false).
	Price float64   `json:"price,omitempty"`
	At    time.Time `json:"at,omitempty"`
	// Evaluated is false when the tick was stale/outside the 5s gate —
	// no knock evaluation happened, no state changed.
	Evaluated bool `json:"evaluated"`
	// Knocked is true only on the single tick that first touches the
	// barrier (the event to persist / notify on).
	Knocked bool `json:"knocked"`
	// Gap is set when this tick was flagged post-gap by the caller
	// (MarkTick.PostGap) or followed > GapThreshold silence — the
	// weekend-gap flag of spec §15.7 item 3.
	Gap bool `json:"gap"`
	// Reason is the audit reason code (KNOCK_IN_TOUCH / KNOCK_OUT_TOUCH /
	// NO_TOUCH / STALE_MARK / INVALID_*).
	Reason string `json:"reason"`
	// TicksSeen counts evaluated ticks so far (replay audit aid).
	TicksSeen int `json:"ticks_seen"`
}

// BarrierEventRecord is the option_barrier_events row shape (migration
// 252) — the durable knock log behind Task 22.3.5's "cash settlement
// with barrier event logging" AC.
type BarrierEventRecord struct {
	OrderID      int64       `json:"order_id"`
	InstrumentID int64       `json:"instrument_id"`
	BarrierType  BarrierType `json:"barrier_type"`
	BarrierLevel float64     `json:"barrier_level"`
	Event        string      `json:"event"` // KNOCK_IN | KNOCK_OUT
	MarkPrice    float64     `json:"mark_price"`
	Gap          bool        `json:"gap"`
	ObservedAt   time.Time   `json:"observed_at"`
}

// Record maps the event to a BarrierEventRecord for the persistence
// seam — nil unless this tick knocked (callers persist only the
// transition, matching UNIQUE(order_id) on the table).
func (e KnockEvent) Record(orderID, instrumentID int64) *BarrierEventRecord {
	if !e.Knocked {
		return nil
	}
	return &BarrierEventRecord{
		OrderID:      orderID,
		InstrumentID: instrumentID,
		BarrierType:  e.BarrierType,
		BarrierLevel: e.Level,
		Event:        e.BarrierType.KnockEventName(),
		MarkPrice:    e.Price,
		Gap:          e.Gap,
		ObservedAt:   e.At,
	}
}

// BarrierEventSink is the persistence seam — the orchestrator binds it
// to an option_barrier_events writer; this package stays storage-free.
type BarrierEventSink interface {
	RecordBarrierEvent(ctx context.Context, rec BarrierEventRecord) error
}

// BarrierMonitor tracks one option's barrier against the mark-tick
// stream. It is single-option, single-threaded by contract — the
// orchestrator runs one per live barrier order on the mark consumer
// goroutine (Phase-19.5 oracle subscription seam). Per-tick evaluation
// delegates to EvaluateBarrierTick so the staleness gate and touch
// inequality have exactly one implementation.
type BarrierMonitor struct {
	typ   BarrierType
	kind  BarrierKind
	level float64
	// GapThreshold flags post-gap ticks by observed silence when the
	// caller did not set MarkTick.PostGap; default DefaultGapThreshold.
	GapThreshold time.Duration

	knocked    bool
	knockPrice float64
	knockAt    time.Time
	lastAt     time.Time
	seen       int
	staleSeen  int
	started    bool
}

// monitorBarrierKind maps the spec §5.4 barrier_type enum onto the
// sibling BarrierKind evaluation vocabulary.
func monitorBarrierKind(t BarrierType) BarrierKind {
	switch t {
	case BarrierUpAndOut:
		return BarrierKnockOutUp
	case BarrierDownAndOut:
		return BarrierKnockOutDown
	case BarrierUpAndIn:
		return BarrierKnockInUp
	default:
		return BarrierKnockInDown
	}
}

// NewBarrierMonitor builds a monitor for the given barrier.
func NewBarrierMonitor(t BarrierType, level float64) (*BarrierMonitor, error) {
	if !t.Valid() {
		return nil, invalidInput("NewBarrierMonitor",
			fmt.Sprintf("barrier type must be UP_AND_IN|UP_AND_OUT|DOWN_AND_IN|DOWN_AND_OUT, got %q", t))
	}
	if !finite(level) || level <= 0 {
		return nil, invalidInput("NewBarrierMonitor",
			fmt.Sprintf("barrier level must be positive and finite, got %v", level))
	}
	return &BarrierMonitor{
		typ:          t,
		kind:         monitorBarrierKind(t),
		level:        level,
		GapThreshold: DefaultGapThreshold,
	}, nil
}

// Observe evaluates one mark tick. The producer sets t.Stale via
// MarkStale(now, t.At) (determinism.go, Task 22.3.15) — a tick so
// flagged is skipped untouched: a dead feed never fabricates a knock
// (spec §15.7 item 3). The skipped tick is counted and reported as
// Evaluated=false, Reason=STALE_MARK. Non-finite/non-positive marks and
// backward timestamps are rejected fail-closed. The first touching
// tick flips the knock latch and returns Knocked=true — subsequent
// ticks evaluate but never re-knock.
func (m *BarrierMonitor) Observe(t MarkTick) (KnockEvent, error) {
	const op = "BarrierMonitor.Observe"
	if !finite(t.Price) || t.Price <= 0 {
		return KnockEvent{}, invalidInput(op, fmt.Sprintf("mark price must be positive and finite, got %v", t.Price))
	}
	if m.started && t.At.Before(m.lastAt) {
		// Out-of-order mark — replay paths must rebuild state in order;
		// a backward tick is a sequencing violation, refuse it (fail-closed).
		return KnockEvent{}, invalidInput(op,
			fmt.Sprintf("mark tick out of order: %s before last %s", t.At, m.lastAt))
	}
	gap := t.PostGap || (m.started && t.At.Sub(m.lastAt) > m.GapThreshold)
	if t.At.After(m.lastAt) {
		m.lastAt = t.At
	}
	m.started = true
	if t.Stale {
		// Expected condition, not a fault: skip the tick entirely —
		// the knock latch and counters stay untouched apart from the
		// stale-tick audit counter.
		m.staleSeen++
		return KnockEvent{BarrierType: m.typ, Level: m.level, At: t.At,
			Evaluated: false, Reason: BarrierReasonStale, TicksSeen: m.seen}, nil
	}
	touched := barrierTouched(m.kind, t.Price, m.level)
	ev := KnockEvent{
		BarrierType: m.typ,
		Level:       m.level,
		Price:       t.Price,
		At:          t.At,
		Evaluated:   true,
		Gap:         gap,
		Reason:      BarrierReasonNoTouch,
	}
	if touched {
		if m.typ.IsKnockIn() {
			ev.Reason = BarrierReasonKnockIn
		} else {
			ev.Reason = BarrierReasonKnockOut
		}
		if !m.knocked {
			m.knocked = true
			m.knockPrice = t.Price
			m.knockAt = t.At
			ev.Knocked = true
		}
	}
	m.seen++
	ev.TicksSeen = m.seen
	return ev, nil
}

// Knocked reports whether the barrier has touched.
func (m *BarrierMonitor) Knocked() bool { return m.knocked }

// Alive is BarrierOption.Alive bound to the monitor's state — for
// knock-out options this is !Knocked(), for knock-in it is Knocked().
func (m *BarrierMonitor) Alive() bool {
	if m.typ.IsKnockIn() {
		return m.knocked
	}
	return !m.knocked
}

// KnockDetail returns the touching mark and its timestamp — the audit
// fields for option_barrier_events / OPTION_ASSIGNMENT-adjacent logs.
func (m *BarrierMonitor) KnockDetail() (price float64, at time.Time, knocked bool) {
	return m.knockPrice, m.knockAt, m.knocked
}

// TicksSeen returns the count of evaluated (non-stale) ticks.
func (m *BarrierMonitor) TicksSeen() int { return m.seen }

// StaleTicksSeen returns the count of ticks skipped by the staleness
// gate — the audit signal for feed outages during the option's life.
func (m *BarrierMonitor) StaleTicksSeen() int { return m.staleSeen }
