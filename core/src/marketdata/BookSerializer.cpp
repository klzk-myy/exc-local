// Task 2.3.13 #4 — L2 snapshot serializer (spec §6.6 item 4). Emits only
// populated price levels; exact counts, zero padding records. See
// marketdata/BookSerializer.hpp for the frame layout and contract.

#include "marketdata/BookSerializer.hpp"

#include <cstring>

namespace exch {

namespace {

// Emit up to `count` levels carrying VISIBLE depth (Phase-16 Tasks
// 16.3.11/16.3.13): a level's record aggregates only l2_visible members —
// pegged and flag-hidden orders contribute neither qty nor count, and a
// level with no visible member is skipped (never leaks structure).
[[nodiscard]] std::size_t emit_side(const OrderBook& book, Side side,
                                    uint16_t count, uint8_t* dst) noexcept {
    std::size_t off = 0;
    uint16_t emitted = 0;
    for (uint32_t i = 0; emitted < count; ++i) {
        const PriceLevel* lvl = book.level(side, i);
        if (lvl == nullptr) break;
        int64_t q = 0;
        uint32_t c = 0;
        for (const Order* o = lvl->head; o != nullptr; o = o->next) {
            if (!l2_visible(*o)) continue;
            q += remaining_qty_units(*o);
            ++c;
        }
        if (c == 0) continue;
        const L2LevelRecord rec{lvl->price_ticks, q,
                                static_cast<int32_t>(c), {0}};
        std::memcpy(dst + off, &rec, sizeof(rec));
        off += sizeof(rec);
        ++emitted;
    }
    return off;
}

// Count levels with at least one l2_visible member, capped at max_depth —
// the header counts MUST match what emit_side writes (hidden-only levels
// are omitted, so raw bid_count()/ask_count() would over-report).
[[nodiscard]] uint16_t count_visible_side(const OrderBook& book, Side side,
                                          uint16_t max_depth) noexcept {
    uint16_t n = 0;
    for (uint32_t i = 0; n < max_depth; ++i) {
        const PriceLevel* lvl = book.level(side, i);
        if (lvl == nullptr) break;
        for (const Order* o = lvl->head; o != nullptr; o = o->next) {
            if (l2_visible(*o) && remaining_qty_units(*o) > 0) {
                ++n;
                break;
            }
        }
    }
    return n;
}

}  // namespace

std::size_t serialize_l2_snapshot(const OrderBook& book,
                                  uint32_t instrument_id, uint64_t ts_ns,
                                  uint8_t* dst, std::size_t dst_cap,
                                  uint16_t max_depth) noexcept {
    if (dst == nullptr) return 0;
    const uint16_t nb = count_visible_side(book, Side::BUY, max_depth);
    const uint16_t na = count_visible_side(book, Side::SELL, max_depth);
    const std::size_t need = l2_wire_size(nb, na);
    if (dst_cap < need) return 0;  // fail-closed: never a torn frame

    const L2SnapshotHeader hdr{instrument_id, {0}, book.book_seq(), ts_ns,
                               nb, na};
    std::memcpy(dst, &hdr, sizeof(hdr));
    std::size_t off = sizeof(hdr);
    off += emit_side(book, Side::BUY, nb, dst + off);
    off += emit_side(book, Side::SELL, na, dst + off);
    return off;
}

}  // namespace exch
