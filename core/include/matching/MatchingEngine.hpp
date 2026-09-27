#pragma once

// PHASE-02 STUB (Task 2.3.2) — price-time priority matching per spec §3.2.
// Phase-02 decomposes this further into WalWriter / IpcPublisher /
// SelfTradeGuard / IcebergManager / StopOrderTrigger (spec §3.7).

#include <cstdint>

#include "book/OrderBook.hpp"
#include "utils/MemoryPool.hpp"

namespace exch {

class MatchingEngine {
public:
    MatchingEngine(uint32_t shard_id, OrderBook& book,
                   MemoryPool<Order>& orders) noexcept;

    // PHASE-02 (Task 2.3.2): pre-trade risk → match → WAL append → IPC publish.
    // Stub only counts received orders.
    void on_order_received(Order* order) noexcept;

    [[nodiscard]] uint32_t shard_id() const noexcept { return shard_id_; }
    [[nodiscard]] uint64_t received_count() const noexcept { return received_count_; }
    [[nodiscard]] OrderBook& book() noexcept { return book_; }
    [[nodiscard]] const OrderBook& book() const noexcept { return book_; }
    [[nodiscard]] MemoryPool<Order>& orders() noexcept { return orders_; }

private:
    uint32_t shard_id_;
    OrderBook& book_;
    MemoryPool<Order>& orders_;
    uint64_t received_count_ = 0;
};

}  // namespace exch
