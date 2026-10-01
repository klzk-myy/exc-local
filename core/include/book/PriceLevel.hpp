#pragma once

// One book price level (Task 2.3.1, spec §3.1): 64-byte cache-line aligned,
// orders hang off an intrusive FIFO queue (head = oldest / highest time
// priority, tail = newest). Prices are int64 10^8 ticks (Task 2.3.23).
//
// push_back/unlink maintain only the intrusive chain + order_count — the
// *total_qty_units* and *visible_* aggregates are owned by OrderBook, which
// updates them under checked arithmetic (spec §3.6.2) on the same mutation
// path, so a level method can never observe a half-updated total. The
// visible_* pair exists so the L2 publisher emits depth in O(levels) without
// walking order chains on the matching thread (Phase-02.5 soak finding:
// per-mutation chain walks were the dominant hot-path cost).

#include <cstdint>
#include <type_traits>

#include "book/Order.hpp"

namespace exch {

struct alignas(64) PriceLevel {
    int64_t price_ticks;       // this level's limit price (10^8 ticks)
    Order* head;               // FIFO front — oldest order, fills first
    Order* tail;               // newest order at the price
    int64_t total_qty_units;   // Σ remaining qty (10^8 units) — book-maintained
    int64_t visible_qty_units;  // Σ remaining qty of L2-visible members only
    uint32_t order_count;
    uint32_t visible_count;  // count of L2-visible members (0 → omit level)

    void reset() noexcept {
        price_ticks = 0;
        head = tail = nullptr;
        total_qty_units = 0;
        visible_qty_units = 0;
        order_count = 0;
        visible_count = 0;
    }

    [[nodiscard]] bool empty() const noexcept { return order_count == 0; }

    // O(1) tail append. Tail-append is what makes the queue FIFO: orders at
    // one price execute strictly in arrival order (spec §3.1 price-time
    // priority — the single matching thread guarantees insertion order ==
    // timestamp order, so no per-insert comparison is needed).
    void push_back(Order* o) noexcept {
        o->prev = tail;
        o->next = nullptr;
        if (tail != nullptr) tail->next = o; else head = o;
        tail = o;
        ++order_count;
    }

    // O(1) unlink from anywhere in the chain (intrusive prev/next).
    void unlink(Order* o) noexcept {
        if (o->prev != nullptr) o->prev->next = o->next; else head = o->next;
        if (o->next != nullptr) o->next->prev = o->prev; else tail = o->prev;
        o->next = o->prev = nullptr;
        --order_count;
    }
};

static_assert(alignof(PriceLevel) == 64);
static_assert(sizeof(PriceLevel) == 64);
static_assert(std::is_trivially_copyable_v<PriceLevel>);

// Cold-path chain consistency walk for OrderBook::validate(): checks chain
// linkage (head.prev/tail.next null, next/prev pairwise consistent), order
// count, order side, level price equality, and non-decreasing timestamps
// (FIFO). Returns false and stores a static reason string on violation.
[[nodiscard]] bool level_chain_consistent(const PriceLevel& lvl, Side side,
                                          const char** violation) noexcept;

}  // namespace exch
