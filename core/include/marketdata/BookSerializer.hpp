#pragma once

// Task 2.3.13 #4 — L2 order-book snapshot serialization (spec §6.6 item 4,
// §10.1, §24 #188).
//
// Wire shape mirrors the FlatBuffers `BookSnapshot{instrument_id, seq,
// bids[], asks[]}` / `PriceLevel{price, qty, count}` schema consumed by
// services/internal/ipc/wire/BookSnapshot.go — but as a packed little-endian
// frame (same codec discipline as wal/WalEntry.hpp) so the serializer is a
// pure memcpy into a caller-supplied buffer: zero heap, noexcept, usable on
// the matching thread between mutations.
//
//   [L2SnapshotHeader][bid_count x L2LevelRecord][ask_count x L2LevelRecord]
//
// Safe-serialization contract (spec §6.6): only actually populated levels
// are emitted. A book with fewer than the advertised feed depth (or an
// empty book) produces a frame carrying the EXACT level counts — never
// synthesized zero-price padding rows. Consumers MUST NOT assume a fixed
// depth.

#include <cstddef>
#include <cstdint>

#include "book/OrderBook.hpp"

namespace exch {

#pragma pack(push, 1)
struct L2SnapshotHeader {
    uint32_t instrument_id;
    uint8_t  _rsv[4];
    uint64_t seq;          // book_seq_ at capture (BookSnapshot.seq)
    uint64_t ts_ns;        // engine logical clock stamp
    uint16_t bid_levels;   // populated bid records that follow — exact count
    uint16_t ask_levels;   // populated ask records — exact count
};

struct L2LevelRecord {
    int64_t  price_ticks;
    int64_t  total_qty_units;
    uint32_t order_count;
    uint8_t  _pad[4];
};
#pragma pack(pop)

static_assert(sizeof(L2SnapshotHeader) == 28);
static_assert(sizeof(L2LevelRecord) == 24);
static_assert(std::is_trivially_copyable_v<L2SnapshotHeader>);
static_assert(std::is_trivially_copyable_v<L2LevelRecord>);

// Feed depth convention (spec §6.6: "fewer than 20 levels"): an L2 feed
// advertises 20 levels; thinner books emit fewer records, never padding.
inline constexpr uint16_t kL2FeedDepth = 20;
// Emit every populated level regardless of depth.
inline constexpr uint16_t kL2FullDepth = 0xFFFF;

// Levels actually emitted: min(populated, max_depth).
[[nodiscard]] constexpr uint16_t l2_levels_emitted(uint32_t populated,
                                                   uint16_t max_depth) noexcept {
    const uint32_t capped =
        populated < static_cast<uint32_t>(max_depth)
            ? populated
            : static_cast<uint32_t>(max_depth);
    return static_cast<uint16_t>(capped);
}

// Frame size for a given emitted level count — header + exact records.
[[nodiscard]] constexpr std::size_t l2_wire_size(uint16_t bid_levels,
                                                 uint16_t ask_levels) noexcept {
    return sizeof(L2SnapshotHeader) +
           sizeof(L2LevelRecord) *
               (static_cast<std::size_t>(bid_levels) +
                static_cast<std::size_t>(ask_levels));
}

// Serialize an L2 snapshot of `book` into `dst`.
//   max_depth caps the per-side level count (kL2FeedDepth by default);
//   kL2FullDepth emits the whole populated book.
// Returns bytes written, or 0 when dst is null / dst_cap is too small —
// fail-closed: a truncated frame is never emitted (§2.7).
// Single-threaded matching-core access contract applies (OrderBook doc).
[[nodiscard]] std::size_t serialize_l2_snapshot(
    const OrderBook& book, uint32_t instrument_id, uint64_t ts_ns,
    uint8_t* dst, std::size_t dst_cap,
    uint16_t max_depth = kL2FeedDepth) noexcept;

}  // namespace exch
