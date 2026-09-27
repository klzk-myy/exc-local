#pragma once

// PHASE-02 STUB (Task 2.3.1) — one book price level; spec §3.1 layout.
// 64-byte cache-line aligned; orders hang off an intrusive FIFO queue.

#include <cstdint>
#include <type_traits>

#include "book/Order.hpp"
#include "utils/Decimal.hpp"

namespace exch {

struct alignas(64) PriceLevel {
    Decimal price;
    Order* head;  // oldest order (FIFO front)
    Order* tail;  // newest order
    uint64_t total_qty;
    uint32_t order_count;
};

static_assert(alignof(PriceLevel) == 64);
static_assert(sizeof(PriceLevel) == 64);
static_assert(std::is_trivially_copyable_v<PriceLevel>);

}  // namespace exch
