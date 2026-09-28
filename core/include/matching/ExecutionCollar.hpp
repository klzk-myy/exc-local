#pragma once

// Task 2.3.17 — Reference-Price Execution Rule (execution collar).
// Spec §22.2 (Reference-Price Execution Rules), spec §24 #277, migration 072
// (`instruments.execution_rule` JSONB + `orders.expiry_reason`).
//
// Per-side execution collars against a PriceOracle reference price, enforced
// during the COMPLETE taker phase and independent of the submission-time
// price bands (spec §3.3 check #6, Instrument::price_band_pct_*):
//
//   * On taker-phase entry the engine snapshots the current non-stale
//     reference price and the configured per-side up/down multipliers into a
//     CollarBounds value; the bounds are held CONSTANT for the whole taker
//     phase — mid-sweep oracle/book/reference updates never move them.
//   * Matching stops before the first maker price outside the allowed range;
//     the remaining quantity expires with
//     EXECUTION_RULE_PRICE_RANGE_EXCEEDED (spec §23, HTTP 409) and the
//     persisted orders.expiry_reason (migration 072).
//   * Absence disables only that direction: no configured rule, no
//     multiplier on that bound, or no reference price at all
//     (ref_price_ticks <= 0) => the direction is NOT enforced (spec §22.2:
//     "a missing rule/reference/multiplier disables only that direction").
//   * A STALE required reference fails CLOSED (Phase-19.5 oracle staleness
//     gate): the whole phase is BLOCKED — every maker price is disallowed
//     and the taker quantity expires with the same expiry reason. Staleness
//     is evaluated two ways, either of which can declare stale: the built-in
//     deterministic age gate (now_ns - ref_ts_ns > max_ref_age_ns_, default
//     5s per the canonical staleness gate) and an injectable predicate for
//     richer oracle semantics (multi-source quorum, PHASE-19.5 feed state).
//     Determinism: time arrives ONLY via the now_ns/ref_ts_ns parameters —
//     this component never reads a wall clock, so WAL replay reproduces the
//     identical bounds and the identical expiry decision.
//   * Engine-level gate: because the check lives inside the taker sweep it
//     applies identically to direct, SOR-returned, batch, WS and FIX orders
//     (Task 2.3.17 impl. note 4) — gateways never see it.
//
// ----------------------------------------------------------------------
// MATCHING-ENGINE SEAM (integration owned by the orchestrator):
//   1. Configuration source: the engine parses instruments.execution_rule
//      (migration 072 JSONB) cold-path into an ExecutionCollarRule per
//      instrument; `configured` = the JSONB object exists at all. Multiplier
//      fields map to CollarMultipliers::{up,down}_pct100 at the canonical
//      percent*100 scale (Instrument.hpp convention: 200 == 2.00%).
//   2. Reference source: ref_price_ticks + ref_ts_ns come from the Phase-19.5
//      PriceOracle cache for the order's instrument (instrument_id via
//      OrderAux). Wire set_staleness_probe() to the oracle's staleness
//      verdict when the composite-feed quorum logic lands; until then the
//      built-in age gate supplies the fail-closed behavior.
//   3. Call ExecutionCollar::begin_phase(side, rule, ref, ref_ts, now_ns_)
//      ONCE at taker-phase entry — in MatchingEngine::on_order_received()'s
//      LIMIT/ICEBERG and MARKET dispatch arms and in process_triggered(),
//      immediately before walk_match() — and pass the returned CollarBounds
//      into walk_match() (new by-value parameter) and fok_feasible() (the
//      feasibility mirror must apply the identical gate so FOK verdicts
//      agree with the sweep, spec §22.2 replay consistency).
//   4. Inside walk_match(), directly AFTER the existing
//      `if (has_limit && !crosses(...)) break;` price check:
//          if (!ExecutionCollar::price_allowed(bounds, lvl->price_ticks)) {
//              res.dead        = true;
//              res.dead_reason = ExecutionCollar::kWalCancelReason;
//              res.dead_code   = ExecutionCollar::kExpiryReason;
//              break;
//          }
//      finish_taker()'s existing dead path then WAL-cancels the remainder
//      once; the Phase-05 order-state service maps the cancel event +
//      dead_code onto orders.expiry_reason (migration 072) and emits the
//      private stream / FIX ExecutionReport expiry reason (spec §22.2).
//   5. kWalCancelReason (= 5) is the next free slot in WalWriter.hpp's
//      kWalCancelReason* space (0..4 taken); the integrator should alias the
//      constant there so the WAL reason table stays the single source.
//      WalOrderCancelPayload.reason is a plain u8 — appending the value is
//      replay-compatible (WalEntry.hpp: extend by APPENDING only).
//
// Storage/latency: stateless value component — zero heap, zero allocation,
// all arithmetic int64 with 128-bit checked intermediates (spec §3.6.2).

#include <cstdint>
#include <type_traits>

#include "book/Order.hpp"         // Side
#include "utils/safe_math.hpp"    // mul_wide_i64 / try_narrow_i128

namespace exch {

// Per-side multiplier pair at the percent*100 scale (Instrument.hpp
// price_band_pct_* convention: value 200 == 2.00%). Each bound participates
// only when its has_* flag is set — spec §22.2 partial-direction disable.
struct CollarMultipliers {
    int64_t up_pct100 = 0;    // hi = ref * (10000 + up) / 10000
    int64_t down_pct100 = 0;  // lo = ref * (10000 - down) / 10000
    bool has_up = false;
    bool has_down = false;
};

// Parsed per-instrument instruments.execution_rule (migration 072 JSONB).
// `configured` distinguishes "no rule object at all" from "rule object with
// no multiplier on this side" — both leave the direction unenforced, but the
// flag preserves the provenance for diagnostics.
struct ExecutionCollarRule {
    CollarMultipliers buy;    // gates BUY takers sweeping asks
    CollarMultipliers sell;   // gates SELL takers sweeping bids
    bool configured = false;
};

enum class CollarPhase : uint8_t {
    NOT_CONFIGURED = 0,  // rule/reference/multiplier absent -> unenforced
    ACTIVE,              // bounds valid — price_allowed() gates makers
    BLOCKED,             // stale/invalid required reference -> fail closed
};

// The taker-phase snapshot. Returned by value from begin_phase() and held
// unchanged for the whole sweep — the "held constant" guarantee is the value
// semantics itself.
struct CollarBounds {
    CollarPhase phase = CollarPhase::NOT_CONFIGURED;
    bool lo_enforced = false;
    bool hi_enforced = false;
    int64_t ref_ticks = 0;  // snapshotted reference (provenance stamping)
    int64_t lo_ticks = 0;   // valid iff lo_enforced
    int64_t hi_ticks = 0;   // valid iff hi_enforced
};

static_assert(std::is_trivially_copyable_v<CollarBounds>);
static_assert(std::is_trivially_copyable_v<CollarMultipliers>);
static_assert(std::is_trivially_copyable_v<ExecutionCollarRule>);

class ExecutionCollar {
public:
    // Persisted expiry reason (orders.expiry_reason VARCHAR(64), migration
    // 072) and the §23 registry code surfaced to clients (HTTP 409).
    static constexpr char kExpiryReason[] =
        "EXECUTION_RULE_PRICE_RANGE_EXCEEDED";
    // WAL cancel-reason slot proposed for the remainder expiry — next free
    // value after kWalCancelReasonIocRemainder (=4) in WalWriter.hpp; the
    // integrator aliases it there (append-only contract).
    static constexpr uint8_t kWalCancelReason = 5;

    // Optional staleness predicate (Phase-19.5 oracle gate). Returns true
    // when the reference is stale. now_ns is the engine's deterministic
    // logical clock; ref_ts_ns is the oracle's stamp on the reference.
    using stale_probe_fn = bool (*)(void* ctx, int64_t ref_price_ticks,
                                    uint64_t ref_ts_ns,
                                    uint64_t now_ns) noexcept;

    // max_ref_age_ns: built-in staleness gate (canonical 5s default);
    // 0 disables the age gate and leaves staleness entirely to the probe.
    explicit ExecutionCollar(
        uint64_t max_ref_age_ns = kDefaultMaxRefAgeNs) noexcept
        : max_ref_age_ns_(max_ref_age_ns) {}

    static constexpr uint64_t kDefaultMaxRefAgeNs = 5'000'000'000ull;  // 5s

    void set_staleness_probe(stale_probe_fn fn, void* ctx) noexcept {
        probe_ = fn;
        probe_ctx_ = ctx;
    }
    void set_max_ref_age_ns(uint64_t ns) noexcept { max_ref_age_ns_ = ns; }

    // Staleness verdict: probe first (if wired), then the deterministic age
    // gate. An un-timestamped (ref_ts_ns == 0) or FUTURE-dated reference is
    // untrustworthy -> stale (fail closed; the forward-jump guard also keeps
    // the uint64 subtraction wrap-free).
    [[nodiscard]] bool stale(int64_t ref_price_ticks, uint64_t ref_ts_ns,
                             uint64_t now_ns) const noexcept {
        if (probe_ != nullptr &&
            probe_(probe_ctx_, ref_price_ticks, ref_ts_ns, now_ns)) {
            return true;
        }
        if (max_ref_age_ns_ == 0) return false;
        if (ref_ts_ns == 0 || ref_ts_ns > now_ns) return true;
        return now_ns - ref_ts_ns > max_ref_age_ns_;
    }

    // Taker-phase entry snapshot. Pure function of the inputs — deterministic
    // across WAL replay. Caller holds the returned value for the sweep.
    [[nodiscard]] CollarBounds
    begin_phase(Side taker_side, const ExecutionCollarRule& rule,
                int64_t ref_price_ticks, uint64_t ref_ts_ns,
                uint64_t now_ns) const noexcept {
        const CollarMultipliers& m =
            taker_side == Side::BUY ? rule.buy : rule.sell;
        CollarBounds b;
        b.ref_ticks = ref_price_ticks;
        if (!rule.configured || (!m.has_up && !m.has_down)) {
            return b;  // direction unenforced (spec §22.2)
        }
        if (ref_price_ticks <= 0) {
            // Missing reference disables only this direction — NOT a fail.
            return b;
        }
        if (stale(ref_price_ticks, ref_ts_ns, now_ns)) {
            b.phase = CollarPhase::BLOCKED;  // fail closed (Phase-19.5)
            return b;
        }
        // Bound math in 128-bit intermediates (spec §3.6.2). A negative
        // multiplier inverts the bound's meaning — misconfiguration reads as
        // a fault and fails closed rather than silently widening the collar.
        if (m.has_down) {
            if (m.down_pct100 < 0) {
                b.phase = CollarPhase::BLOCKED;
                return b;
            }
            const safe_math::int128_t w =
                safe_math::mul_wide_i64(ref_price_ticks,
                                        10'000 - m.down_pct100) /
                10'000;
            if (!safe_math::try_narrow_i128(w, b.lo_ticks)) {
                b.phase = CollarPhase::BLOCKED;
                return b;
            }
            if (b.lo_ticks < 1) b.lo_ticks = 1;  // prices are strictly >0
            b.lo_enforced = true;
        }
        if (m.has_up) {
            if (m.up_pct100 < 0) {
                b.phase = CollarPhase::BLOCKED;
                return b;
            }
            const safe_math::int128_t w =
                safe_math::mul_wide_i64(ref_price_ticks,
                                        10'000 + m.up_pct100) /
                10'000;
            if (!safe_math::try_narrow_i128(w, b.hi_ticks)) {
                b.phase = CollarPhase::BLOCKED;
                return b;
            }
            b.hi_enforced = true;
        }
        b.phase = CollarPhase::ACTIVE;
        return b;
    }

    // Per-maker gate inside the taker sweep. NOT_CONFIGURED admits
    // everything; BLOCKED admits nothing (fail closed); ACTIVE applies the
    // snapshotted bounds — bound at lo/hi inclusive IS allowed.
    [[nodiscard]] static constexpr bool
    price_allowed(const CollarBounds& b, int64_t maker_price_ticks) noexcept {
        switch (b.phase) {
            case CollarPhase::NOT_CONFIGURED: return true;
            case CollarPhase::BLOCKED:        return false;
            case CollarPhase::ACTIVE:         break;
        }
        if (b.lo_enforced && maker_price_ticks < b.lo_ticks) return false;
        if (b.hi_enforced && maker_price_ticks > b.hi_ticks) return false;
        return true;
    }

    [[nodiscard]] static const char* phase_name(CollarPhase p) noexcept {
        switch (p) {
            case CollarPhase::NOT_CONFIGURED: return "NOT_CONFIGURED";
            case CollarPhase::ACTIVE:         return "ACTIVE";
            case CollarPhase::BLOCKED:        return "BLOCKED";
        }
        return "UNKNOWN";
    }

private:
    uint64_t max_ref_age_ns_;
    stale_probe_fn probe_ = nullptr;
    void* probe_ctx_ = nullptr;
};

}  // namespace exch
