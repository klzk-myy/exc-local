#pragma once

// Order book node — spec §3.1 verbatim shape. POD aggregate; allocated from
// MemoryPool<Order> (1M per shard), never via new/delete in the hot path.

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

struct Order {
    uint64_t id;
    uint64_t account_id;
    Side side;
    OrderType type;
    Decimal quantity;
    Decimal price;
    Decimal filled_qty;
    TimeInForce tif;
    uint64_t timestamp_ns;  // price-time priority stamp (utils::now_ns)
    Order* next;            // intrusive linked list — no allocator (spec §3.1)
    Order* prev;
};

static_assert(std::is_aggregate_v<Order>);
static_assert(std::is_trivially_copyable_v<Order>);
static_assert(std::is_trivially_destructible_v<Order>);
static_assert(std::is_standard_layout_v<Order>);

// Orders per shard — 1M pre-allocated pool entries (Task 1.3.1).
inline constexpr std::size_t kOrderPoolCapacity = 1'000'000;

[[nodiscard]] inline Decimal remaining_qty(const Order& o) {
    return o.quantity - o.filled_qty;
}
[[nodiscard]] inline bool is_filled(const Order& o) {
    return !(o.filled_qty < o.quantity);
}

}  // namespace exch
