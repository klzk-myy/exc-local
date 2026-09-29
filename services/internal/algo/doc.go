// Package algo implements the Phase-16 algo order framework and its
// strategy drivers:
//
//   - Task 16.3.8  — framework: parent state machine
//     NEW/PENDING → RUNNING → PAUSED → COMPLETED/CANCELLED/EXPIRED,
//     delayed dispatch (start_at), durable parent state in
//     algo_orders/algo_order_children (migration 224), pause/resume/
//     cancel endpoints behind POST|DELETE /api/v1/orders/algo*.
//   - Task 16.3.1  — TWAP: equal slices per interval at current mid,
//     unfilled slices cancelled + resubmitted at interval end.
//   - Task 16.3.2  — VWAP: slices proportional to a historical volume
//     profile (VolumeProfileSource seam — PG trailing-window buckets in
//     this binary, flat-profile fallback documented in vwap.go; a
//     ClickHouse bucket source can bind the same seam).
//   - Task 16.3.12 — anti-gaming: ±30% timing jitter, ±15% size
//     perturbation preserving total exactly (remainder → final slice),
//     0–3 pip price discretion, VWAP profile Gaussian noise σ=5%.
//     The PRNG seed is per-run from crypto/rand and deliberately
//     never persisted — anti-gaming must NOT be replay-deterministic.
//     The materialized schedule it produces (perturbed slice qtys +
//     timing bounds) IS persisted in algo_orders.state so a restart
//     resumes the same plan rather than re-drawing mid-flight; full
//     auditability comes from the actual dispatched times/qty/prices
//     recorded on algo_order_children rows.
//   - Task 16.3.18 — VP: participation_rate (1%–50%) of observed trade
//     volume per 5s interval, optional price_limit, max_duration, same
//     anti-gaming (±20% size / ±15% timing per the task text). Volume
//     source = the `trades` JetStream stream (TradeVolumeTracker fed by
//     TradeVolumeConsumer) or the PgVolumeSource fallback.
//   - Task 16.3.6  — SPREAD: two legs, spread price = leg1 − leg2
//     enforced at submit; legs execute marketable-IOC with
//     compensate-on-fail unwind (true cross-instrument atomicity does
//     not exist in a per-shard engine — see spread.go).
//   - Task 16.3.7  — SCALE: ≤20 limit levels, EQUAL/LINEAR/CUSTOM
//     weighting, each level tracked as an independent child row.
//
// Children are NEVER dispatched around the order pipeline: every slice
// goes through the ChildExecutor seam, which production binds to
// orders.Service.Submit/Cancel — the same admission, risk, balance and
// idempotency path as a REST submit (fail-closed §2.7). Each child
// carries a derived client_order_id "algo:{parent}:{seq}" so a retry
// after a crash replays the stored ack instead of double-submitting.
package algo
