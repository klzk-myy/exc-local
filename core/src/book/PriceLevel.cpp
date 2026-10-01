// Task 2.3.1 — PriceLevel cold-path integrity walk. Hot ops (push_back/
// unlink) are inline in the header where the matching thread can inline them.

#include "book/PriceLevel.hpp"

namespace exch {

bool level_chain_consistent(const PriceLevel& lvl, Side side,
                            const char** violation) noexcept {
    if (violation != nullptr) *violation = nullptr;
    const char* why = nullptr;
    uint32_t n = 0;
    uint32_t vis_n = 0;
    int64_t vis_qty = 0;
    uint64_t prev_ts = 0;
    const Order* prev = nullptr;
    for (const Order* o = lvl.head; o != nullptr; o = o->next) {
        if (o->prev != prev)        { why = "broken prev linkage"; break; }
        if (o->side != side)        { why = "order on wrong book side"; break; }
        if (o->price_ticks != lvl.price_ticks) {
            why = "order price != level price"; break;
        }
        // FIFO: timestamps may be equal (deterministic replay ties broken by
        // ingress_seq) but never decrease toward the tail.
        if (o->timestamp_ns < prev_ts) {
            why = "timestamp order violated (FIFO)"; break;
        }
        if (o->qty_units <= 0 || remaining_qty_units(*o) <= 0) {
            why = "resting order with non-positive remaining qty"; break;
        }
        if (l2_visible(*o)) {
            ++vis_n;
            vis_qty += remaining_qty_units(*o);
        }
        prev_ts = o->timestamp_ns;
        prev = o;
        ++n;
    }
    if (why == nullptr) {
        if (prev != lvl.tail)          why = "tail does not end the chain";
        else if (lvl.head != nullptr && lvl.head->prev != nullptr)
            why = "head has a predecessor";
        else if (lvl.tail != nullptr && lvl.tail->next != nullptr)
            why = "tail has a successor";
        else if (n != lvl.order_count) why = "order_count != chain length";
        else if ((n == 0) != (lvl.head == nullptr))
            why = "empty level with dangling head";
        else if (vis_n != lvl.visible_count)
            why = "visible_count != visible chain members";
        else if (vis_qty != lvl.visible_qty_units)
            why = "visible_qty_units != visible members' remaining sum";
    }
    if (why != nullptr) {
        if (violation != nullptr) *violation = why;
        return false;
    }
    return true;
}

}  // namespace exch
