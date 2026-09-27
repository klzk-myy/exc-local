// PHASE-02 STUB (Task 2.3.1) — real flat-array level maintenance lands there.
#include "book/OrderBook.hpp"

namespace exch {

const PriceLevel* OrderBook::best_bid() const noexcept {
    for (std::size_t i = 0; i < bid_count_; ++i) {
        if (bids_[i].order_count > 0) return &bids_[i];
    }
    return nullptr;
}

const PriceLevel* OrderBook::best_ask() const noexcept {
    for (std::size_t i = 0; i < ask_count_; ++i) {
        if (asks_[i].order_count > 0) return &asks_[i];
    }
    return nullptr;
}

}  // namespace exch
