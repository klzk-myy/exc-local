package orders

// Phase-16 — gateway-side order-type seams. These interfaces let
// orders.Service consult the Task 16.3.9 fixing pipeline and the Task
// 16.3.16 guaranteed-stop machinery without importing the algo package
// (algo implements them and wires in through cmd/gateway; the import
// direction stays algo → orders only).

import (
	"context"
	"time"

	"exchange/pkg/decimal"
)

// FixingHooks is the Phase-16 Task 16.3.9 benchmark-fixing seam —
// *algo.FixingService satisfies it in production. FIXING orders are
// gateway-queued (RESERVED) and executed by the fix executor against
// benchmark_fixings rows; they never touch the engine wire.
//
// Fail-closed contract: a nil seam rejects FIXING submissions with
// INVALID_REQUEST — a fixing order without the cutoff gate and the
// reservation journal is unimplementable. Cancels never hard-fail on a
// nil seam (a cancel must never trap an open reservation) but a wired
// seam's AssertMutable gate is always enforced.
type FixingHooks interface {
	// AdmitSubmit enforces the T-15-minute cutoff against the
	// benchmark's next publication; violations surface as coded
	// FIXING_CUTOFF_EXCEEDED.
	AdmitSubmit(ctx context.Context, inst *Instrument,
		req *SubmitRequest, now time.Time) error
	// Reserve posts the balance-reservation journal for a
	// freshly-inserted FIXING order and records the reservation in
	// orders.algo_params (evidence for later release/adjust).
	Reserve(ctx context.Context, o *Order, inst *Instrument) error
	// AssertMutable gates cancel and modify through the same cutoff —
	// FIXING_CANCELLATION_RESTRICTED once the window opens.
	AssertMutable(ctx context.Context, o *Order, inst *Instrument,
		now time.Time) error
	// Release unwinds the reservation (pre-cutoff cancel, rejected
	// placement, executor shortfall).
	Release(ctx context.Context, o *Order, inst *Instrument) error
	// AdjustReservation re-sizes the lock after a quantity amend.
	AdjustReservation(ctx context.Context, o *Order, inst *Instrument) error
}

// GSLOHooks is the Phase-16 Task 16.3.16 guaranteed-stop-loss seam —
// *algo.GSLOService satisfies it in production. The engine sibling
// guarantees the exact-stop fill; the Go side owns premium debit/refund,
// the per-instrument exposure cap and insurance-fund gap absorption,
// all through the GL/ledger path (never direct balance writes).
//
// Fail-closed contract: a nil seam rejects gslo submissions with
// INVALID_REQUEST — a "guaranteed" stop without the premium + cap
// machinery is a fabrication.
type GSLOHooks interface {
	// AdmitSubmit enforces the per-instrument GSLO exposure cap and
	// returns the premium the placement must debit (quote currency).
	// Violations surface as coded GSLO_EXPOSURE_EXCEEDED /
	// PREMIUM_INSUFFICIENT.
	AdmitSubmit(ctx context.Context, acct *Account, inst *Instrument,
		req *SubmitRequest, ref *decimal.Decimal) error
	// ChargePremium posts the placement premium journal once the order
	// row exists (idempotency-keyed on the order id) and records the
	// charged amount in orders.algo_params for later refund.
	ChargePremium(ctx context.Context, o *Order, inst *Instrument) error
	// RefundPremium posts the premium-refund journal for a pre-trigger
	// cancellation (or a submit-path unwind after a dispatch failure).
	RefundPremium(ctx context.Context, o *Order, inst *Instrument) error
}

// AuctionGate is the Phase-16 Task 16.3.25 freeze seam for queued
// session-uncross orders (MOO/MOC, spec §6.2b). Frozen reports whether
// the instrument's call auction is armed — `instrument:auction:{symbol}`
// CALL/EXTEND key present — or inside the T-30-second pre-trigger
// freeze window computed from the auction calendar (DAILY_CLOSE for
// MOC, the 21:00-UTC Sunday reopen for MOO).
//
// Fail-closed contract: a nil seam rejects MOO/MOC submissions with
// INVALID_REQUEST — a queued order the gateway can never freeze-gate
// would let post-freeze amends slide through. A gate ERROR fails closed
// too: an unreadable freeze state must never admit a mutation.
type AuctionGate interface {
	Frozen(ctx context.Context, inst *Instrument,
		orderType string, now time.Time) (bool, error)
}

// PrivateNotify publishes a private-channel event frame to one
// account's WS subscribers — bound to ws.Server.PublishPrivate in
// cmd/gateway. Task 16.3.25 emits "order.queued" / "order.auction_fill"
// / "order.cancelled" through it; composite orders reuse the same seam
// for leg-placement notifications. Nil → notifications skipped (tests).
type PrivateNotify func(accountID int64, channel string, data any)

// ConditionalGate is the Phase-16 Task 16.3.22 admission-side guard for
// conditional triggers and pegged orders — *algo.TriggerGuard satisfies
// it in production. MARK_PRICE/INDEX_PRICE triggers require a fresh
// oracle feed (≤5s, spec §24 #317); pegged orders require a viable BBO
// — violations surface CONDITIONAL_TRIGGER_ORACLE_STALE /
// PEGGED_PRICING_UNAVAILABLE.
//
// Fail-open contract differs from the other seams: a nil gate means the
// oracle/book checks are not enforced at admission (the engine still
// evaluates triggers authoritatively) — the gateway wires the guard in
// production; tests may leave it nil.
type ConditionalGate interface {
	AdmitConditional(ctx context.Context, inst *Instrument,
		req *SubmitRequest) error
}
