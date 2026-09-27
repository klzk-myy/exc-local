// PHASE-02 STUB (Task 2.3.2) — spec §3.2 matching algorithm lands there.
#include "matching/MatchingEngine.hpp"

namespace exch {

MatchingEngine::MatchingEngine(uint32_t shard_id, OrderBook& book,
                               MemoryPool<Order>& orders) noexcept
    : shard_id_(shard_id), book_(book), orders_(orders) {}

void MatchingEngine::on_order_received(Order* /*order*/) noexcept {
    ++received_count_;
}

}  // namespace exch
