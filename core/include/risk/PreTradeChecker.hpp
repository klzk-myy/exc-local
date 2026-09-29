#pragma once

// Pre-Trade Risk — the kill-switch gate + all 14 in-process checks
// (Task 2.3.3 + Task 2.3.9 collar, spec §3.3; no IPC, <10µs total, zero
// heap allocation on the check path).
//
// Check order is the spec §3.3 / contract order — cheapest first, short-
// circuit on first failure:
//   0.  Kill-switch suspension lattice (Phase-11 Tasks 11.3.4/11.3.8/
//       11.3.12): bound SuspensionFlags consult the last Redis-polled
//       `halt:*` snapshot in-process — GLOBAL → ACCOUNT → COUNTERPARTY →
//       INSTRUMENT → INSTRUMENT_CLASS. A bound checker with no
//       successfully-polled snapshot fails CLOSED (TRADING_HALTED);
//       an unbound seam keeps the legacy behavior (the Go admission
//       gates are the authoritative layer and fail closed there).
//       Cancels never reach this pipeline (engine cancels are ungated),
//       satisfying the cancel-exempt halt contract.
//   0b. MiFID II RTS 9 order-to-trade breach (Phase-13 Task 13.3.6):
//       the same refreshed snapshot carries `otr:breach:{account}` —
//       breached accounts reject OTR_LIMIT_EXCEEDED; cancels exempt.
//   1.  Account status (ACTIVE only)
//   2.  Instrument status (ACTIVE; RESTRICTED=limit-only; DELISTED=
//       reduce_only-only per §7.1 + remediation #35)
//   3.  Balance (available >= required debit)
//   4.  Position limit (max open positions; skipped for reduce_only)
//   5.  Order-rate collar — in-process token bucket (Task 2.3.9)
//   6.  Price band vs last price (instrument.price_band_pct_up/down)
//   7.  Max/min order qty
//   8.  Margin — Phase-2 stub (balance >= notional*ratio); IMarginCheck seam
//       lets Phase-19 swap the real engine without touching call sites
//   9.  Circuit breaker — Phase-2 stub (>5% move in 60s window);
//       Phase-13 Task 13.3.1 owns the five-tier system
//   10. KYC tier — Phase-2 stub (required=T0 gates nothing); Phase-14 owns
//   11. Tick/lot quantization
//   12. Min notional
//   13. Execution flags (post_only marketability, reduce_only position)
//   14. STP — validates stp_mode, resolves order->account->CANCEL_NEWEST
//       (Task 2.3.21), gates NONE to Professional/ECP (Task 2.3.16, §24 #274).
//       The same-account MATCH is engine-side (SelfTradeGuard); this check
//       validates + resolves and stamps the resolved mode on the order.
//
// When bound, an optional bilateral-credit admission screen (spec §3.3b)
// runs after check 14 — see bind_credit().
//
// All dependencies are injected via risk/RiskInterfaces.hpp providers; every
// failure mode fails CLOSED (spec §2.7). No heap allocation inside check():
// the rate-collar table is a fixed open-addressing array allocated once at
// construction (same discipline as OrderBook's id index).

#include <cstddef>
#include <cstdint>
#include <new>

#include "book/Instrument.hpp"
#include "book/Order.hpp"
#include "book/OrderBook.hpp"
#include "risk/BilateralCreditMatrix.h"
#include "risk/RiskInterfaces.hpp"
#include "risk/SuspensionFlags.hpp"

namespace exch {

// Kept from the Phase-02 stub — single-arg check() shim below returns it.
enum class RiskDecision : uint8_t { ACCEPT, REJECT };

// Wire convention for "order omitted stp_mode" (Task 2.3.21 resolution order:
// per-order value -> accounts.default_stp_mode -> CANCEL_NEWEST). The IPC
// decoder stores 0xFF when the client field is absent; StpMode's fixed
// uint8_t underlying type makes the out-of-enum sentinel representable.
inline constexpr uint8_t kStpModeUnset = 0xFF;

[[nodiscard]] constexpr bool is_known_stp_mode(StpMode m) noexcept {
    return static_cast<uint8_t>(m) <= static_cast<uint8_t>(StpMode::NONE);
}

// Task 2.3.21 resolution — also exported for the matching engine, which calls
// it (or relies on the mode check() already stamped) before SelfTradeGuard.
[[nodiscard]] constexpr StpMode resolve_stp_mode(
    StpMode order_mode, StpMode account_default) noexcept {
    if (is_known_stp_mode(order_mode)) return order_mode;
    if (static_cast<uint8_t>(order_mode) == kStpModeUnset &&
        is_known_stp_mode(account_default)) {
        return account_default;
    }
    return StpMode::CANCEL_NEWEST;
}

// --- IMarketState over a live OrderBook (production adapter) -----------------
//
// Binds checks 6/9/13 to the book the engine is matching: best bid/ask come
// from the flat level arrays (O(1)); last price falls back to the bound
// instrument's last_price_ticks. price_ticks_ago has no source in Phase-2
// (trade history ring is a later task) — inject a history callback via
// set_price_history() or leave unbound (returns 0 → check 9 passes, the
// documented placeholder semantic).
class OrderBookMarketState final : public IMarketState {
public:
    explicit OrderBookMarketState(const OrderBook* book) noexcept
        : book_(book) {}

    void set_book(const OrderBook* book) noexcept { book_ = book; }

    // fn(ctx, instrument_id, at_or_before_ns) -> price ticks (0 = none).
    void set_price_history(int64_t (*fn)(void*, uint64_t, uint64_t) noexcept,
                           void* ctx) noexcept {
        hist_fn_ = fn;
        hist_ctx_ = ctx;
    }

    [[nodiscard]] int64_t last_price_ticks(
        uint64_t /*instrument_id*/) const noexcept override {
        if (book_ == nullptr) return 0;
        const Instrument* i = book_->instrument();
        return i != nullptr ? i->last_price_ticks : 0;
    }
    [[nodiscard]] int64_t price_ticks_ago(
        uint64_t instrument_id, uint64_t window_ns,
        uint64_t now_ns) const noexcept override {
        if (hist_fn_ == nullptr || now_ns < window_ns) return 0;
        return hist_fn_(hist_ctx_, instrument_id, now_ns - window_ns);
    }
    [[nodiscard]] int64_t best_bid_ticks(
        uint64_t /*instrument_id*/) const noexcept override {
        if (book_ == nullptr) return 0;
        const PriceLevel* l = book_->best_bid();
        return l != nullptr ? l->price_ticks : 0;
    }
    [[nodiscard]] int64_t best_ask_ticks(
        uint64_t /*instrument_id*/) const noexcept override {
        if (book_ == nullptr) return 0;
        const PriceLevel* l = book_->best_ask();
        return l != nullptr ? l->price_ticks : 0;
    }

private:
    const OrderBook* book_ = nullptr;
    int64_t (*hist_fn_)(void*, uint64_t, uint64_t) noexcept = nullptr;
    void* hist_ctx_ = nullptr;
};

// --- The checker --------------------------------------------------------------

class PreTradeChecker {
public:
    // Fixed-capacity open-addressing collar table (account_id -> bucket).
    // 2^16 slots * 24B = 1.5MB, allocated ONCE here (cold path, nothrow) —
    // never on the check path. A null table fails check 5 closed.
    static constexpr std::size_t kCollarTableSlots = std::size_t{1} << 16;
    // Probe bound: linear probing past this many slots rejects fail-closed
    // (table nearly full); keeps check 5 O(1)-bounded.
    static constexpr uint32_t kCollarMaxProbe = 64;
    static constexpr int64_t kTokenScale = 1000;  // milli-tokens

    explicit PreTradeChecker(RiskConfig cfg = {}) noexcept;
    ~PreTradeChecker();

    PreTradeChecker(const PreTradeChecker&) = delete;
    PreTradeChecker& operator=(const PreTradeChecker&) = delete;
    PreTradeChecker(PreTradeChecker&&) = delete;
    PreTradeChecker& operator=(PreTradeChecker&&) = delete;

    // --- Wiring (cold path) ---------------------------------------------------

    void bind_accounts(const IAccountState* p) noexcept { accounts_ = p; }
    void bind_positions(const IPositionState* p) noexcept { positions_ = p; }
    void bind_market(const IMarketState* p) noexcept { market_ = p; }
    // Phase-19 seam: non-null hook replaces the check-8 stub math entirely.
    void bind_margin(const IMarginCheck* p) noexcept { margin_ = p; }
    // Instrument used by the legacy single-arg check(order) path.
    void bind_instrument(const Instrument* p) noexcept { instrument_ = p; }
    // Optional §3.3b admission screen; both must be non-null to engage.
    void bind_credit(const BilateralCreditMatrix* m,
                     const IPartyMap* map) noexcept {
        credit_ = m;
        party_map_ = map;
    }
    // Phase-11 Task 11.3.4/11.3.8 kill-switch seam: non-null engages the
    // suspension lattice as check 0. The bound flags object must outlive
    // the checker; its snapshot must be refreshed by a
    // SuspensionRefresher control loop (unwired-but-bound fails closed).
    void bind_suspensions(const SuspensionFlags* f) noexcept {
        suspensions_ = f;
    }
    // Time source for the legacy check(order) path (ctx-less callers).
    // Defaults to steady_ns (monotonic — correct base for the collar window).
    void set_clock(uint64_t (*fn)(void*) noexcept, void* ctx) noexcept {
        clock_fn_ = fn;
        clock_ctx_ = ctx;
    }

    // --- The 14-check pipeline --------------------------------------------------

    // Primary entry. On a passing order whose stp_mode arrived as the
    // kStpModeUnset sentinel, the resolved mode (order -> account default ->
    // CANCEL_NEWEST) is stamped back onto `order` before matching — the
    // resolved value lands in WAL/fill records deterministically.
    [[nodiscard]] RiskVerdict check(Order& order,
                                    const CheckContext& ctx) noexcept {
        return run(order, ctx, &order);
    }
    // Const flavor: identical verdict, no stp_mode stamping.
    [[nodiscard]] RiskVerdict check(const Order& order,
                                    const CheckContext& ctx) noexcept {
        return run(order, ctx, nullptr);
    }

    // Stub-era signature kept for legacy callers (test_matching, main.cpp).
    // A checker with no bound IAccountState is "detached" and accepts — the
    // pre-2.3.3 stub contract. Once providers are bound this runs the full
    // pipeline against the bound instrument + clock. Production wiring MUST
    // bind providers before order flow (spec §2.7: an unbound checker gates
    // nothing; a bound-but-unknown account rejects fail-closed).
    [[nodiscard]] RiskDecision check(const Order& order) noexcept;

    // Resolved STP mode for an order (account consulted). Exported for the
    // engine's SelfTradeGuard path — identical rule to check 14.
    [[nodiscard]] StpMode resolved_stp(const Order& order) const noexcept;

    // --- Telemetry / cold-path maintenance -------------------------------------

    [[nodiscard]] bool configured() const noexcept {
        return accounts_ != nullptr;
    }
    [[nodiscard]] std::size_t collar_slots_used() const noexcept {
        return collar_used_;
    }
    // Evict one account's bucket (account close/off-boarding; cold path).
    // Tombstone-free: the slot is simply cleared — a later order re-inserts.
    void reset_account_collar(uint64_t account_id) noexcept;

private:
    struct CollarSlot {
        uint64_t key = 0;          // account_id; 0 == empty (id 0 never valid)
        int64_t tokens_x1000 = 0;  // milli-token balance
        uint64_t last_ns = 0;      // last consume/refill stamp
    };

    [[nodiscard]] RiskVerdict run(const Order& order, const CheckContext& ctx,
                                  Order* stamp) noexcept;
    [[nodiscard]] RiskVerdict check_collar(uint64_t account_id,
                                           uint64_t now_ns) noexcept;
    // Effective reference price for qty↔notional math on price-less orders:
    // limit price, else last, else opposite best. 0 when nothing exists.
    [[nodiscard]] int64_t effective_price_ticks(
        const Order& order, uint64_t instrument_id) const noexcept;

    RiskConfig cfg_;

    const IAccountState* accounts_ = nullptr;
    const IPositionState* positions_ = nullptr;
    const IMarketState* market_ = nullptr;
    const IMarginCheck* margin_ = nullptr;
    const Instrument* instrument_ = nullptr;              // legacy path only
    const BilateralCreditMatrix* credit_ = nullptr;       // optional §3.3b
    const IPartyMap* party_map_ = nullptr;
    const SuspensionFlags* suspensions_ = nullptr;        // Task 11.3.4/8/12
    uint64_t (*clock_fn_)(void*) noexcept = nullptr;      // legacy path only
    void* clock_ctx_ = nullptr;

    CollarSlot* collar_ = nullptr;  // ctor-allocated kCollarTableSlots
    std::size_t collar_used_ = 0;
};

}  // namespace exch
