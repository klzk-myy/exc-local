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
