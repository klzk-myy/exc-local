#pragma once

// Task 2.3.22 — Price-Improvement delta recorder (spec §6.6b #3, §24 #400;
// MiFID II RTS 27/28 best-execution evidence consumed by the Phase-20
// Task 20.3.9 TCA engine).
//
// When a fill executes at a price BETTER than the taker's limit (e.g. a buy
// limited at 1.1050 lifting a 1.1040 offer), the fill record carries the
// improvement delta. Spec formula (verbatim): price_improvement_delta =
// limit_price - execution_price. For a BUY the delta is positive when
// improved; for a SELL the same formula yields a negative number when
// improved — the side-aware `improved` flag and `abs(delta)` give consumers
// the favorable magnitude without re-deriving the sign convention.
//
// MARKET orders and other no-limit fills (limit_ticks <= 0) record
// delta == 0 / improved == false — improvement is defined against a limit.
//
// ----------------------------------------------------------------------
// MATCHING-ENGINE SEAM (integration owned by the orchestrator):
//   * Call site: inside MatchingEngine::walk_match()'s fill block, AFTER
//     px/fill are known and before/with the publish step — the taker-side
//     improvement is:
//         improvement_.on_fill(taker.side, taker.id, tid,
//                              has_limit ? limit_ticks : 0, px, fill);
//     (maker-side improvement is always 0 — makers execute AT their limit;
//     only the taker can beat its own bound. A second on_fill for the maker
//     is legal and records delta==0.)
//   * Fill-record stamping: register set_stamp_sink() with a function that
//     writes ImprovementStamp onto the engine's fill/trade record (or
//     forwards it to the private-stream / FIX ExecutionReport encoder) —
//     the recorder itself never touches I/O or the WAL.
//   * TCA aggregation: fills_seen_ / fills_improved_ /
//     improvement_ticks_total_ (side-aware favorable magnitude sum) are the
//     RTS 27/28 counters the Phase-20 report drains.
//
// Determinism & footprint: pure int64 arithmetic, fixed POD storage, zero
// heap, noexcept; identical fill stream => identical stamps and counters.

#include <cstdint>
#include <type_traits>

#include "book/Order.hpp"  // Side
#include "utils/safe_math.hpp"

namespace exch {

// Fill-record extension produced per fill. delta_ticks is the SPEC-LITERAL
// signed value limit - exec (§6.6b #3): positive => buy improvement,
// negative => sell improvement. improved is the side-aware flag; the
// favorable magnitude is abs(delta_ticks) whenever improved is set.
struct ImprovementStamp {
    uint64_t order_id;
    uint64_t trade_id;
    int64_t limit_ticks;
    int64_t exec_ticks;
    int64_t delta_ticks;    // limit_ticks - exec_ticks (spec §6.6b #3)
    int64_t qty_units;
    uint8_t side;           // Side ordinal
    uint8_t improved;       // side-aware better-than-limit flag
    uint8_t _pad[6];
};

static_assert(std::is_trivially_copyable_v<ImprovementStamp>);
static_assert(std::is_standard_layout_v<ImprovementStamp>);

class PriceImprovementRecorder {
public:
    // Fill-record stamping seam: the engine (or its publisher) wires a sink
    // that persists `stamp` onto the fill record / streams it. Called inline
    // on the matching thread; a null sink simply skips the callback while
    // counters still accumulate.
    using stamp_sink_fn = void (*)(void* ctx,
                                   const ImprovementStamp& stamp) noexcept;
    void set_stamp_sink(stamp_sink_fn fn, void* ctx) noexcept {
        sink_ = fn;
        sink_ctx_ = ctx;
    }

    // Pure computation — no recording side effects. Safe for the fok/preview
    // paths that need the value without mutating TCA counters.
    [[nodiscard]] static ImprovementStamp
    compute(Side side, uint64_t order_id, uint64_t trade_id,
            int64_t limit_ticks, int64_t exec_ticks,
            int64_t qty_units) noexcept {
        ImprovementStamp s{};
        s.order_id = order_id;
        s.trade_id = trade_id;
        s.limit_ticks = limit_ticks;
        s.exec_ticks = exec_ticks;
        s.qty_units = qty_units;
        s.side = static_cast<uint8_t>(side);
        if (limit_ticks > 0 && exec_ticks > 0) {
            // Checked subtraction (spec §3.6.2): a wrapped delta would be a
            // nonsense TCA figure — on overflow report delta==0/not improved
            // rather than persist garbage.
            int64_t d = 0;
            if (safe_math::try_sub(limit_ticks, exec_ticks, d)) {
                s.delta_ticks = d;
                s.improved = static_cast<uint8_t>(
                    side == Side::BUY ? exec_ticks < limit_ticks
                                      : exec_ticks > limit_ticks);
            }
        }
        return s;
    }

    // Record one stamp: counters + last-stamp + sink. Returns the stamp so
    // the caller can forward it without recomputing.
    ImprovementStamp record(const ImprovementStamp& s) noexcept {
        ++fills_seen_;
        if (s.improved != 0) {
            ++fills_improved_;
            // Favorable magnitude (side-aware): BUY improves when limit>exec,
            // SELL when exec>limit — accumulate the positive magnitude.
            const int64_t mag =
                s.delta_ticks >= 0 ? s.delta_ticks : -s.delta_ticks;
            improvement_ticks_total_ += mag;
        }
        last_ = s;
        if (sink_ != nullptr) sink_(sink_ctx_, s);
        return s;
    }

    // Convenience: compute + record in one call — the hot-path entry point.
    ImprovementStamp on_fill(Side side, uint64_t order_id, uint64_t trade_id,
                             int64_t limit_ticks, int64_t exec_ticks,
                             int64_t qty_units) noexcept {
        return record(
            compute(side, order_id, trade_id, limit_ticks, exec_ticks,
                    qty_units));
    }

    // --- TCA counters (RTS 27/28 aggregates) ----------------------------------
    [[nodiscard]] uint64_t fills_seen() const noexcept { return fills_seen_; }
    [[nodiscard]] uint64_t fills_improved() const noexcept {
        return fills_improved_;
    }
    // Sum of favorable improvement magnitudes (side-aware), tick units.
    [[nodiscard]] int64_t improvement_ticks_total() const noexcept {
        return improvement_ticks_total_;
    }
    [[nodiscard]] const ImprovementStamp& last() const noexcept {
        return last_;
    }

private:
    stamp_sink_fn sink_ = nullptr;
    void* sink_ctx_ = nullptr;
    uint64_t fills_seen_ = 0;
    uint64_t fills_improved_ = 0;
    int64_t improvement_ticks_total_ = 0;
    ImprovementStamp last_{};
};

}  // namespace exch
