// Task 2.3.13 #4 — L2 snapshot serializer (spec §6.6 item 4). Emits only
// populated price levels; exact counts, zero padding records. See
// marketdata/BookSerializer.hpp for the frame layout and contract.

#include "marketdata/BookSerializer.hpp"

#include <cstring>

namespace exch {

namespace {

[[nodiscard]] std::size_t emit_side(const OrderBook& book, Side side,
                                    uint16_t count, uint8_t* dst) noexcept {
    std::size_t off = 0;
    for (uint16_t i = 0; i < count; ++i) {
        const PriceLevel* lvl = book.level(side, i);
        if (lvl == nullptr) break;  // populated <= count by construction
        const L2LevelRecord rec{lvl->price_ticks, lvl->total_qty_units,
                                lvl->order_count, {0}};
        std::memcpy(dst + off, &rec, sizeof(rec));
        off += sizeof(rec);
    }
    return off;
}

}  // namespace

std::size_t serialize_l2_snapshot(const OrderBook& book,
                                  uint32_t instrument_id, uint64_t ts_ns,
                                  uint8_t* dst, std::size_t dst_cap,
                                  uint16_t max_depth) noexcept {
    if (dst == nullptr) return 0;
    const uint16_t nb = l2_levels_emitted(book.bid_count(), max_depth);
    const uint16_t na = l2_levels_emitted(book.ask_count(), max_depth);
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
