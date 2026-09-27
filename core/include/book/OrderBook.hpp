#pragma once

// PHASE-02 STUB (Task 2.3.1) — flat-array order book per spec §3.1.
// bids_ sorted descending by price, asks_ ascending; binary-search level
// lookup, intrusive FIFO queues per level, monotonic book_seq_.

#include <cstdint>

#include "book/PriceLevel.hpp"

namespace exch {

class OrderBook {
public:
    // Levels per side of the book; Phase-02 tunes against instrument config.
    static constexpr std::size_t kMaxLevels = 4096;

    OrderBook() noexcept : bids_{}, asks_{} {}

    [[nodiscard]] uint64_t book_seq() const noexcept { return book_seq_; }
    [[nodiscard]] uint32_t bid_count() const noexcept { return bid_count_; }
    [[nodiscard]] uint32_t ask_count() const noexcept { return ask_count_; }

    // Best bid = highest-priced populated bid level; nullptr when side empty.
    [[nodiscard]] const PriceLevel* best_bid() const noexcept;
    // Best ask = lowest-priced populated ask level; nullptr when side empty.
    [[nodiscard]] const PriceLevel* best_ask() const noexcept;

    // PHASE-02 (Task 2.3.1): add_order / cancel_order / modify_order /
    // level(depth) / get_snapshot land with the real book implementation.

private:
    PriceLevel bids_[kMaxLevels];
    PriceLevel asks_[kMaxLevels];
    uint32_t bid_count_ = 0;
    uint32_t ask_count_ = 0;
    uint64_t book_seq_ = 0;
};

}  // namespace exch
