#pragma once

// Order book node — spec §3.1 shape, Task 2.3.23 integer-tick representation.
// POD aggregate; allocated from MemoryPool<Order> (1M per shard), never via
// new/delete in the hot path.
//
// Task 2.3.23 (spec §3.3a, §24 #402): price and quantity are stored as int64
// fixed-point at the 10^8 pipette scale — `price_ticks` and `qty_units` are
// the PRIMARY fields consumed by the matching hot path; native IEEE types are
// forbidden there. The mantissa of utils/Decimal is the identical scale, so
// Decimal views below are free reinterpretations (no conversion math) for
// cold paths such as reports and Decimal-based callers.
//
// `quantity` is a legacy compat mirror of `qty_units` (pre-2.3.23 field,
// kept because utils tests and early Phase-02 stubs reference it). The book
// keeps quantity.mantissa() == qty_units synchronized on every mutation;
// callers must treat it as read-only. `filled_qty_units` has no mirror.

#include <cstddef>
#include <cstdint>
#include <type_traits>

#include "utils/Decimal.hpp"

namespace exch {

enum class Side : uint8_t { BUY, SELL };

// spec §5.4 order_type enum (16 values).
enum class OrderType : uint8_t {
    LIMIT, MARKET, STOP, STOP_LIMIT, ICEBERG, TWAP, VWAP, TRAILING_STOP,
    BRACKET, OCO, SPREAD, SCALE, PEG, FIXING, MOO, MOC
};

// spec §5.4 time_in_force enum.
enum class TimeInForce : uint8_t { GTC, IOC, FOK, GTD, DAY };

// spec §5.4 stp_mode enum (verbatim order; Task 2.3.11 owns the behavior,
// Task 2.3.21 resolves account defaults). Field storage only here.
enum class StpMode : uint8_t {
    CANCEL_NEWEST = 0, CANCEL_OLDEST, CANCEL_BOTH, DECREMENT, NONE
};

// spec §6.5 execution flags (bitfield on Order::flags; §3.3 check #13).
inline constexpr uint8_t kOrderFlagPostOnly = 1u << 0;   // reject if marketable
inline constexpr uint8_t kOrderFlagReduceOnly = 1u << 1; // may only reduce
// (bits 2/3 = market-with-protection / STP-transfer — MatchingEngine.hpp /
//  SelfTradeGuard.hpp hold those definitions.)
// Phase-16 Task 16.3.13 (spec §24 #197): fully-hidden limit order — rests on
// the book normally but is excluded from public L2/L3 projection and fills
// at the visible-BBO midpoint instead of its level price.
inline constexpr uint8_t kOrderFlagHidden = 1u << 4;
// Phase-16 Task 16.3.16 (spec §24 #321): guaranteed stop-loss — on trigger
// the engine executes the full remaining qty at the armed stop price as a
// self-trade (no book walk), absorbing gap risk into the GSLO exposure
// pool. Only meaningful on conditional types.
inline constexpr uint8_t kOrderFlagGslo = 1u << 5;

// Phase-16 trigger-source selector (spec §6.2a; orders.trigger_source
// migration 066; evaluation values are also the WAL ORDER_TRIGGERED bytes).
inline constexpr uint8_t kTriggerSourceLast  = 0;
inline constexpr uint8_t kTriggerSourceMark  = 1;
inline constexpr uint8_t kTriggerSourceIndex = 2;

// Phase-16 peg modes (Task 16.3.11; orders.peg_mode migration 038).
inline constexpr uint8_t kPegNone    = 0;
inline constexpr uint8_t kPegMid     = 1;  // midpoint of book BBO
inline constexpr uint8_t kPegPrimary = 2;  // same-side best
inline constexpr uint8_t kPegMarket  = 3;  // opposite-side best

// Phase-16 trailing-stop distance units (Tasks 16.3.3/16.3.15).
inline constexpr uint8_t kTrailUnitNone       = 0;
inline constexpr uint8_t kTrailUnitPips       = 1;  // distance = N pips
inline constexpr uint8_t kTrailUnitPercentage = 2;  // distance = N/100 % of anchor
inline constexpr uint8_t kTrailUnitAbsolute   = 3;  // distance = N ticks

struct Order {
    uint64_t id;
    uint64_t account_id;
    Side side;
    OrderType type;
    TimeInForce tif;
    StpMode stp_mode;   // §6.5; resolved per Task 2.3.21 before matching
    uint8_t flags;      // kOrderFlag* bitfield
    // --- Task 2.3.23 primary fixed-point fields (10^8 scale) ----------------
    int64_t price_ticks;         // limit price in ticks; primary match key
    int64_t qty_units;           // order quantity in units
    int64_t filled_qty_units;    // cumulative filled quantity in units
    int64_t display_qty_units;   // ICEBERG visible slice; 0 == fully visible
    // Compat mirror of qty_units — synchronized by OrderBook on add/amend.
    Decimal quantity;
    uint64_t timestamp_ns;       // price-time priority stamp (Aeron ingress clock)
    uint64_t ingress_seq;        // per-shard ingress seq — deterministic FIFO
                                 // tie-break: (price, timestamp_ns, ingress_seq)
    Order* next;                 // intrusive level FIFO chain — no allocator (§3.1)
    Order* prev;
    Order* hash_next;            // intrusive id→order index chain (OrderBook)
};

static_assert(std::is_aggregate_v<Order>);
static_assert(std::is_trivially_copyable_v<Order>);
static_assert(std::is_trivially_destructible_v<Order>);
static_assert(std::is_standard_layout_v<Order>);
static_assert(sizeof(Order) <= 128);  // pool-dense node; watch field creep

// Orders per shard — 1M pre-allocated pool entries (Task 1.3.1).
inline constexpr std::size_t kOrderPoolCapacity = 1'000'000;

[[nodiscard]] inline int64_t remaining_qty_units(const Order& o) noexcept {
    return o.qty_units - o.filled_qty_units;
}
[[nodiscard]] inline bool is_filled(const Order& o) noexcept {
    return o.filled_qty_units >= o.qty_units;
}

// True when the order may appear in public L2 depth. Pegged orders are
// L2-hidden by spec §6.2/Task 16.3.11; kOrderFlagHidden covers Task 16.3.13.
[[nodiscard]] constexpr bool l2_visible(const Order& o) noexcept {
    return o.type != OrderType::PEG && (o.flags & kOrderFlagHidden) == 0;
}

// Cold-path Decimal views — identity scale (mantissa == ticks), zero math.
[[nodiscard]] inline Decimal price_decimal(const Order& o) noexcept {
    return Decimal::from_mantissa(o.price_ticks);
}
[[nodiscard]] inline Decimal qty_decimal(const Order& o) noexcept {
    return Decimal::from_mantissa(o.qty_units);
}
[[nodiscard]] inline Decimal filled_decimal(const Order& o) noexcept {
    return Decimal::from_mantissa(o.filled_qty_units);
}

// Diagnostic names (cold path; tests/logs/WAL recovery reports).
[[nodiscard]] const char* side_name(Side s) noexcept;
[[nodiscard]] const char* order_type_name(OrderType t) noexcept;
[[nodiscard]] const char* tif_name(TimeInForce t) noexcept;
[[nodiscard]] const char* stp_mode_name(StpMode m) noexcept;

}  // namespace exch
