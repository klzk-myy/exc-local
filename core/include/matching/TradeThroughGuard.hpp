#pragma once

// Task 2.3.22 — Trade-Through Protection & Price Improvement gate
// (spec §6.6b, §23 `TRADE_THROUGH_DETECTED` HTTP 409, §24 #400; MiFID II
// RTS 27/28 best-execution evidence for the Phase-20 TCA engine).
//
// Protected quote (spec §6.6b #1 + #6 staleness contract): the INTERNAL
// book's best bid/ask at match time — NOT the public L2 feed (the feed may
// lag via conflation delay; it is display-only). The cache is refreshed by
// on_book_mutation() on every book mutation: a seqlock pair (odd/even
// sequence around relaxed stores) so a non-matching-thread reader can never
// observe a torn bid/ask pair, while the matching thread itself always reads
// a consistent snapshot.
//
// Pre-execution check (spec §6.6b #2) — evaluated per aggressive taker:
//   * LIMIT (and ICEBERG, the limit-family taker): a limit price beyond the
//     opposite protected quote (buy limit > protected_ask, sell limit <
//     protected_bid) means the order is willing to execute WORSE than the
//     protected quote -> REJECT, code TRADE_THROUGH_DETECTED (HTTP 409).
//     A limit exactly AT the quote passes — it can only fill at-or-better.
//   * MARKET: the §6.6a synthetic-limit protection price is clipped to the
//     protected quote -> verdict CLIP with effective_limit_ticks =
//     min(protection, ask) for buys / max(protection, bid) for sells (and
//     the quote itself when no slippage band is configured). The engine
//     sweeps to the clipped bound; the excess remainder is cancelled
//     SLIPPAGE_EXCEEDED (verdict.remainder_code) and reported through
//     on_remainder_cancelled() for the TCA event stream.
//   * IOC / FOK (any order type carrying that TIF): when NO liquidity rests
//     at-or-better than the protected quote -> REJECT TRADE_THROUGH_DETECTED.
//     The liquidity figure is engine-computed (the component never walks the
//     book) via the same level walk as fok_feasible() bounded at the quote.
//   * Missing opposite quote (empty side) disables the limit/clip branch —
//     there is no protected price to cross or clip to; empty-book handling
//     stays with §6.6 (ORDER_REJECTED_NO_LIQUIDITY) elsewhere. The IOC/FOK
//     presence check still applies (liquidity <= 0 -> reject).
//
// Auction bypass (spec §6.6b #4): set_auction(true) during a call auction
// (§7.1 CALL state, owned by Phase-15 lifecycle) bypasses every check — the
// uncross price is the single market-clearing price for all participants.
// Bypassed checks are counted (bypassed_checks_) rather than logged per
// order.
//
// Event seam (spec §6.6b #5): every prevention/clipping decision emits a POD
// TradeThroughEvent through the registered event_sink_fn — the Phase-20 TCA
// bridge (NATS) attaches there; the component itself never performs I/O.
// All callbacks run on the matching thread in the caller's call stack.
//
// ----------------------------------------------------------------------
// MATCHING-ENGINE SEAM (integration owned by the orchestrator):
//   * Quote refresh: call on_book_mutation(best_bid, best_ask) after every
//     committed book mutation — the natural site is alongside the existing
//     `if (publisher_ != nullptr && book_.book_seq() != seq0) publish_depth()`
//     tails in on_order_received / on_cancel_received / on_amend_received /
//     on_time_tick (drain_triggers), reading book_.best_bid()/best_ask()
//     (nullptr level -> pass 0 for that side).
//   * Pre-execution gate: in the on_order_received dispatch (LIMIT/ICEBERG
//     and MARKET arms) and process_triggered(), AFTER validation/risk/WAL-
//     accept and BEFORE fok_feasible()/walk_match():
//         const TtVerdict v = trade_through_.check(input);
//         if (v.decision == TtDecision::REJECT) {
//             emit_cancel_event(order->id, order->account_id,
//                               kWalCancelReasonUser);
//             last_reject_ = v.code;   // TRADE_THROUGH_DETECTED
//             ++reject_count_;
//             break;                   // order freed by the existing tail
//         }
//         // CLIP: pass v.effective_limit_ticks as walk_match's limit_ticks
//         //       (has_limit=true); a remainder left at the clipped bound is
//         //       terminal-cancelled by finish_taker with dead_code =
//         //       v.remainder_code (SLIPPAGE_EXCEEDED), then
//         //       on_remainder_cancelled() emits the TCA event.
//   * liquidity_at_or_better_units (IOC/FOK input): sum remaining_qty_units
//     over opposite-side levels while price is at-or-better than the
//     protected quote — reuse the fok_feasible() walk bounded at the quote.
//   * Price improvement: inside walk_match()'s fill block, after px is known,
//     call PriceImprovementRecorder::on_fill(taker.side, taker.id, tid,
//     limit_ticks-if-any, px, fill) — see matching/PriceImprovementRecorder.hpp.
//   * ts_ns stamps: pass the engine logical clock (now_ns_), never a wall
//     clock — event stream stays replay-deterministic.
//
// Determinism & footprint: single-writer state, fixed POD storage, zero heap,
// noexcept throughout; identical inputs produce identical verdicts/events.

#include <atomic>
#include <cstdint>
#include <type_traits>

#include "book/Order.hpp"  // Side, OrderType, TimeInForce

namespace exch {

// Consistent read of the protected quote; *_ticks == 0 means that side is
// absent (empty book side — prices are always strictly positive).
struct ProtectedQuote {
    int64_t bid_ticks = 0;
    int64_t ask_ticks = 0;
    uint64_t seq = 0;  // seqlock value the pair was read under (even)
};

// What the guard needs from the engine for one pre-execution verdict.
// Invariants: limit_ticks > 0 for LIMIT/ICEBERG; protection_price_ticks is
// the §6.6a synthetic-limit bound for MARKET (0 = slippage band unset);
// liquidity_at_or_better_units is consumed only for IOC/FOK.
struct TtCheckInput {
    uint64_t order_id = 0;
    Side side = Side::BUY;
    OrderType type = OrderType::LIMIT;
    TimeInForce tif = TimeInForce::GTC;
    int64_t limit_ticks = 0;
    int64_t protection_price_ticks = 0;
    int64_t liquidity_at_or_better_units = 0;
    uint64_t ts_ns = 0;  // engine logical clock (TIME_TICK), for event stamps
};

enum class TtDecision : uint8_t {
    PASS = 0,  // no protected-quote constraint applies
    REJECT,    // whole order rejected — `code` is the §23 registry string
    CLIP,      // MARKET only: sweep bound = effective_limit_ticks
};

struct TtVerdict {
    TtDecision decision = TtDecision::PASS;
    const char* code = nullptr;            // REJECT: "TRADE_THROUGH_DETECTED"
    const char* remainder_code = nullptr;  // CLIP: "SLIPPAGE_EXCEEDED"
    int64_t effective_limit_ticks = 0;     // CLIP: clipped sweep bound
};

enum class TtEventKind : uint8_t {
    LIMIT_TRADE_THROUGH_REJECTED = 0,
    IOC_FOK_NO_LIQUIDITY_REJECTED,
    MARKET_CLIPPED_TO_QUOTE,
    MARKET_REMAINDER_SLIPPAGE,
};

// POD event for the Phase-20 TCA/NATS bridge — trivially serializable, fixed
// layout, carries both quote legs + the clipped bound for provenance.
struct TradeThroughEvent {
    uint64_t order_id;
    uint64_t ts_ns;                       // engine logical clock stamp
    int64_t limit_ticks;                  // order limit (0 when market/unset)
    int64_t protected_bid_ticks;
    int64_t protected_ask_ticks;
    int64_t effective_limit_ticks;        // CLIP: bound; else 0
    int64_t liquidity_at_or_better_units;
    int64_t remainder_units;              // MARKET_REMAINDER_SLIPPAGE only
    uint8_t kind;                         // TtEventKind
    uint8_t side;                         // Side ordinal
    uint8_t _pad[6];
};

static_assert(std::is_trivially_copyable_v<ProtectedQuote>);
static_assert(std::is_trivially_copyable_v<TtCheckInput>);
static_assert(std::is_trivially_copyable_v<TtVerdict>);
static_assert(std::is_trivially_copyable_v<TradeThroughEvent>);
static_assert(std::is_standard_layout_v<TradeThroughEvent>);

class TradeThroughGuard {
public:
    // §23 registry codes (spec §6.6b): surfaced verbatim to the gateway; the
    // Phase-05 Task 5.3.21 registry owns the HTTP mapping (409 / 400).
    static constexpr char kCodeTradeThroughDetected[] = "TRADE_THROUGH_DETECTED";
    static constexpr char kCodeSlippageExceeded[] = "SLIPPAGE_EXCEEDED";

    TradeThroughGuard() noexcept = default;
    TradeThroughGuard(const TradeThroughGuard&) = delete;
    TradeThroughGuard& operator=(const TradeThroughGuard&) = delete;

    // --- Protected-quote cache (spec §6.6b #1/#6) ----------------------------
    // Call after EVERY committed book mutation on the matching thread.
    // 0 = side absent. Seqlock publish: external readers never see a
    // half-updated pair; the matching thread itself is the sole writer.
    void on_book_mutation(int64_t best_bid_ticks,
                          int64_t best_ask_ticks) noexcept {
        quote_seq_.fetch_add(1, std::memory_order_relaxed);  // odd = writing
        bid_.store(best_bid_ticks, std::memory_order_relaxed);
        ask_.store(best_ask_ticks, std::memory_order_relaxed);
        quote_seq_.fetch_add(1, std::memory_order_release);  // even = stable
    }

    // Consistent quote snapshot (spin only if a writer is mid-pair — bounded
    // single-writer critical section of two stores).
    [[nodiscard]] ProtectedQuote quote() const noexcept {
        ProtectedQuote q;
        for (;;) {
            const uint64_t s0 = quote_seq_.load(std::memory_order_acquire);
            if (s0 & 1u) continue;  // writer mid-update
            q.bid_ticks = bid_.load(std::memory_order_relaxed);
            q.ask_ticks = ask_.load(std::memory_order_relaxed);
            const uint64_t s1 = quote_seq_.load(std::memory_order_acquire);
            if (s0 == s1) {
                q.seq = s0;
                return q;
            }
        }
    }

    // --- Auction bypass (spec §6.6b #4) --------------------------------------
    // Phase-15 instrument lifecycle drives this: CALL auction state on/off.
    void set_auction(bool in_auction) noexcept { auction_ = in_auction; }
    [[nodiscard]] bool in_auction() const noexcept { return auction_; }

    // --- Event seam (spec §6.6b #5 -> Phase-20 TCA over NATS) -----------------
    using event_sink_fn = void (*)(void* ctx,
                                   const TradeThroughEvent& e) noexcept;
    void set_event_sink(event_sink_fn fn, void* ctx) noexcept {
        sink_ = fn;
        sink_ctx_ = ctx;
    }

    // --- Pre-execution check (spec §6.6b #2) ----------------------------------
    // Emits a TradeThroughEvent for every prevention/clipping decision.
    [[nodiscard]] TtVerdict check(const TtCheckInput& in) noexcept {
        ++checks_;
        if (auction_) {  // call-auction uncross: checks suspended (§6.6b #4)
            ++bypassed_;
            return pass();
        }
        const ProtectedQuote q = quote();
        const int64_t opp =
            in.side == Side::BUY ? q.ask_ticks : q.bid_ticks;

        // IOC/FOK presence gate — applies across order types carrying the
        // TIF: nothing executable at-or-better than the protected quote.
        if ((in.tif == TimeInForce::IOC || in.tif == TimeInForce::FOK) &&
            in.liquidity_at_or_better_units <= 0) {
            return reject(in, q, TtEventKind::IOC_FOK_NO_LIQUIDITY_REJECTED);
        }

        switch (in.type) {
            // Limit-family takers: resting LIMIT/ICEBERG plus STOP_LIMIT at
            // trigger time (process_triggered re-runs check on the activated
            // taker — spec §6.6b makes the gate engine-level, not per-form).
            case OrderType::LIMIT:
            case OrderType::ICEBERG:
            case OrderType::STOP_LIMIT:
                if (opp > 0 && in.limit_ticks > 0 &&
                    trades_through(in.side, in.limit_ticks, opp)) {
                    return reject(in, q,
                                  TtEventKind::LIMIT_TRADE_THROUGH_REJECTED);
                }
                return pass();
            // MARKET plus triggered STOP (market semantics on activation).
            case OrderType::MARKET:
            case OrderType::STOP: {
                if (opp <= 0) return pass();  // no protected quote to clip to
                const bool has_prot = in.protection_price_ticks > 0;
                // Clip the §6.6a protection price to the quote; with no
                // slippage band configured the quote itself is the bound.
                const int64_t eff =
                    in.side == Side::BUY
                        ? (has_prot && in.protection_price_ticks < opp
                               ? in.protection_price_ticks
                               : opp)
                        : (has_prot && in.protection_price_ticks > opp
                               ? in.protection_price_ticks
                               : opp);
                // A prevention event fires only when clipping the quote
                // actually tightened the sweep bound (or supplied it).
                const bool tightened =
                    !has_prot ||
                    (in.side == Side::BUY ? eff < in.protection_price_ticks
                                          : eff > in.protection_price_ticks);
                ++clips_;
                if (tightened) {
                    emit(TtEventKind::MARKET_CLIPPED_TO_QUOTE, in, q, eff, 0);
                }
                TtVerdict v;
                v.decision = TtDecision::CLIP;
                v.remainder_code = kCodeSlippageExceeded;
                v.effective_limit_ticks = eff;
                return v;
            }
            default:
                // Conditional/algo order forms reach this guard through the
                // trigger path re-check (STOP -> market arm, STOP_LIMIT ->
                // limit arm); any other type has no aggressive bound here.
                return pass();
        }
    }

    // Engine calls this when it terminal-cancels a CLIPped market-order
    // remainder — emits MARKET_REMAINDER_SLIPPAGE for the TCA stream.
    void on_remainder_cancelled(uint64_t order_id, Side side,
                                int64_t remaining_units,
                                uint64_t ts_ns) noexcept {
        const ProtectedQuote q = quote();
        TtCheckInput in;
        in.order_id = order_id;
        in.side = side;
        in.type = OrderType::MARKET;
        in.ts_ns = ts_ns;
        emit(TtEventKind::MARKET_REMAINDER_SLIPPAGE, in, q, 0,
             remaining_units);
    }

    // --- Introspection --------------------------------------------------------
    [[nodiscard]] uint64_t checks() const noexcept { return checks_; }
    [[nodiscard]] uint64_t rejections() const noexcept { return rejections_; }
    [[nodiscard]] uint64_t clips() const noexcept { return clips_; }
    [[nodiscard]] uint64_t bypassed_checks() const noexcept {
        return bypassed_;
    }
    [[nodiscard]] uint64_t events_emitted() const noexcept {
        return events_emitted_;
    }

    [[nodiscard]] static const char* decision_name(TtDecision d) noexcept;
    [[nodiscard]] static const char* event_kind_name(TtEventKind k) noexcept;

private:
    // "would match worse than the protected quote" — the order's own bound
    // sits beyond the quote it would consume through.
    [[nodiscard]] static constexpr bool trades_through(
        Side side, int64_t limit_ticks, int64_t opp_quote) noexcept {
        return side == Side::BUY ? limit_ticks > opp_quote
                                 : limit_ticks < opp_quote;
    }

    [[nodiscard]] static TtVerdict pass() noexcept { return TtVerdict{}; }

    TtVerdict reject(const TtCheckInput& in, const ProtectedQuote& q,
                     TtEventKind kind) noexcept {
        ++rejections_;
        emit(kind, in, q, 0, 0);
        TtVerdict v;
        v.decision = TtDecision::REJECT;
        v.code = kCodeTradeThroughDetected;
        return v;
    }

    void emit(TtEventKind kind, const TtCheckInput& in,
              const ProtectedQuote& q, int64_t effective_limit_ticks,
              int64_t remainder_units) noexcept {
        TradeThroughEvent e{};
        e.order_id = in.order_id;
        e.ts_ns = in.ts_ns;
        e.limit_ticks = in.limit_ticks;
        e.protected_bid_ticks = q.bid_ticks;
        e.protected_ask_ticks = q.ask_ticks;
        e.effective_limit_ticks = effective_limit_ticks;
        e.liquidity_at_or_better_units = in.liquidity_at_or_better_units;
        e.remainder_units = remainder_units;
        e.kind = static_cast<uint8_t>(kind);
        e.side = static_cast<uint8_t>(in.side);
        ++events_emitted_;
        if (sink_ != nullptr) sink_(sink_ctx_, e);
    }

    // Protected-quote cache — seqlock pair + two relaxed atomics.
    std::atomic<uint64_t> quote_seq_{0};
    std::atomic<int64_t> bid_{0};
    std::atomic<int64_t> ask_{0};

    bool auction_ = false;  // matching-thread only (spec §6.6b #4)

    event_sink_fn sink_ = nullptr;
    void* sink_ctx_ = nullptr;

    uint64_t checks_ = 0;
    uint64_t rejections_ = 0;
    uint64_t clips_ = 0;
    uint64_t bypassed_ = 0;
    uint64_t events_emitted_ = 0;
};

}  // namespace exch
