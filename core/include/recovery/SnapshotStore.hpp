#pragma once

// PHASE-02 TASK-2.3.4 — book snapshot persistence + cadence hook (spec §3.5:
// "every 100,000 trades or 5 minutes, whichever first").
//
// Blob layout (the PINNED contract bytes come first, wal/WalEntry.hpp):
//   [WalBookSnapshotHeader]                      24 B — instrument_id,
//                                                level_count, order_count,
//                                                book_seq
//   [WalSnapshotLevel  × level_count]            16 B — bids descending, then
//                                                       asks ascending
//   [WalSnapshotOrder  × order_count]            48 B — FIFO per level, in the
//                                                       same order as levels
//   [WalSnapshotExtHeader]                       16 B — magic/version/count
//   [WalSnapshotOrderExt × order_count]          48 B — restore extension
//
// The pinned records alone cannot rebuild a book: WalSnapshotLevel has no
// order_count and WalSnapshotOrder has no price/filled/timestamp — so the
// extension block carries each order's level_index plus the full-fidelity
// fields (qty, filled, ts, ingress, type, flags). Appending a trailer region
// is the contract-sanctioned extension style ("extend by APPENDING new
// structs/enumerators only"). Readers that only know the pinned region can
// still walk levels; RecoveryManager requires the extension to restore.
//
// Sequence-domain note (spec §3.5 "book_seq == WAL tail"): the WAL cursor is
// the shard-stream seq. `snapshot_seq` / WalBookSnapshotHeader.book_seq carry
// the WAL tail cursor the snapshot covers — entries with seq < snapshot_seq
// are reflected in the snapshot; replay resumes at seq == snapshot_seq (i.e.
// the spec's "snapshot_seq + 1" in last-seq naming). Under the writer
// convention of one WAL entry per book mutation the cursor equals the book's
// book_seq_ counter — "snapshot advances book_seq to tail" (Phase-02 AC #33).
//
// Sink seam: the durable home is PostgreSQL `book_snapshots` (migration 023,
// Phase-04). ISnapshotSink is the pluggable boundary; FileSnapshotSink is the
// Phase-02 implementation ({root}/i{instrument_id}/snap_{seq}.bin, fsync file
// + atomic rename + fsync dir).

#include <cstdint>
#include <string>
#include <vector>

#include "book/OrderBook.hpp"
#include "wal/WalEntry.hpp"

namespace exch {

// --- Extension records (appended trailer; packed like the WAL structs) ------

#pragma pack(push, 1)
struct WalSnapshotExtHeader {
    uint32_t magic;          // kSnapExtMagic
    uint16_t version;        // kSnapExtVersion
    uint16_t flags;          // reserved, 0
    uint64_t order_count;    // mirrors WalBookSnapshotHeader.order_count
};

// Full-fidelity restore record, one per WalSnapshotOrder in stream order.
struct WalSnapshotOrderExt {
    uint64_t order_id;         // must equal the pinned record's order_id
    int64_t  qty_units;        // ORIGINAL total qty (pinned record = remaining)
    int64_t  filled_qty_units;
    int64_t  price_ticks;      // cross-check vs the owning level's price
    uint64_t timestamp_ns;     // price-time priority stamp (replay cursor ts)
    uint64_t ingress_seq;      // deterministic FIFO tie-break
    uint32_t level_index;      // index into the levels[] array
    uint8_t  type;             // OrderType
    uint8_t  side;             // Side — cross-check vs level side
    uint8_t  flags;            // kOrderFlag* bitfield
    uint8_t  _pad[5];
};
#pragma pack(pop)

inline constexpr uint32_t kSnapExtMagic = 0x31455853u;   // 'SXE1'
inline constexpr uint16_t kSnapExtVersion = 1;
static_assert(sizeof(WalSnapshotExtHeader) == 16);
static_assert(sizeof(WalSnapshotOrderExt) == 60);

// --- Pluggable sink ----------------------------------------------------------
//
// The snapshot byte owner boundary: Phase-02 ships FileSnapshotSink; Phase-04
// implements a PostgreSQL sink over the same interface (book_snapshots table,
// migration 023). Implementations must be fail-closed: false on any I/O or
// integrity failure, never partially-mutated state.

class ISnapshotSink {
public:
    virtual ~ISnapshotSink() = default;

    // Persist `blob` (WalBookSnapshotHeader + levels + orders + ext) for
    // `instrument_id` covering WAL cursor `seq`. Must be durable (fsync-class)
    // and atomic (no torn snapshot is ever readable) before returning true.
    [[nodiscard]] virtual bool store(uint32_t instrument_id, uint64_t seq,
                                     const uint8_t* blob,
                                     uint64_t len) noexcept = 0;

    // Load the newest snapshot for `instrument_id`. `*has_snapshot` receives
    // false on a clean cold start (no file/row — NOT an error). Returns false
    // on I/O or integrity failure (torn write, bad CRC, truncated blob).
    [[nodiscard]] virtual bool load_latest(uint32_t instrument_id,
                                           uint64_t* seq_out,
                                           std::vector<uint8_t>* blob_out,
                                           bool* has_snapshot) noexcept = 0;
};

// --- Phase-02 file sink ------------------------------------------------------
//
// Layout: {root}/i{instrument_id}/snap_{seq:020}.bin
//   [SnapFileHeader][payload = the blob][crc32c(payload) u32 trailer]
// store() writes snap_*.bin.tmp-<pid>, fsyncs the fd, rename()s into place and
// fsyncs the directory — a snapshot is either fully present or absent.

#pragma pack(push, 1)
struct SnapFileHeader {
    uint32_t magic;          // kSnapFileMagic 'SNAP'
    uint16_t version;        // kSnapFileVersion
    uint16_t shard_id;       // owning shard — cross-check on load
    uint64_t seq;            // WAL cursor covered == header.book_seq
    uint32_t instrument_id;
    uint32_t payload_len;    // blob bytes following this header
    uint32_t payload_crc;    // wal_crc32c over the blob
    uint32_t _pad;
};
#pragma pack(pop)

inline constexpr uint32_t kSnapFileMagic = 0x50414E53u;  // 'SNAP'
inline constexpr uint16_t kSnapFileVersion = 1;
static_assert(sizeof(SnapFileHeader) == 32);

class FileSnapshotSink final : public ISnapshotSink {
public:
    // root = the shard's snapshot directory (created on first store).
    // keep_per_instrument bounds retained snap_*.bin files (newest first).
    explicit FileSnapshotSink(std::string root, uint16_t shard_id,
                              uint32_t keep_per_instrument = 2) noexcept;

    [[nodiscard]] bool store(uint32_t instrument_id, uint64_t seq,
                             const uint8_t* blob, uint64_t len) noexcept override;
    [[nodiscard]] bool load_latest(uint32_t instrument_id, uint64_t* seq_out,
                                   std::vector<uint8_t>* blob_out,
                                   bool* has_snapshot) noexcept override;

    [[nodiscard]] const std::string& root() const noexcept { return root_; }
    [[nodiscard]] int last_errno() const noexcept { return last_errno_; }

private:
    // {root}/i{instrument_id}/snap_{seq:020}.bin
    [[nodiscard]] std::string dir_for(uint32_t instrument_id) const;
    [[nodiscard]] std::string file_for(uint32_t instrument_id,
                                       uint64_t seq) const;

    std::string root_;
    uint16_t shard_id_;
    uint32_t keep_;
    int last_errno_ = 0;
};

// --- Parsed snapshot (RecoveryManager's restore input) ------------------------

struct ParsedSnapshotOrder {
    Order tmpl;              // add_order-ready template (links ignored)
    int64_t remaining;       // pinned record qty_units (cross-checked)
    uint32_t level_index;    // owning level in levels[]
};

struct ParsedSnapshot {
    WalBookSnapshotHeader header{};              // seq cursor in .book_seq
    std::vector<WalSnapshotLevel> levels;        // bids desc, then asks asc
    std::vector<ParsedSnapshotOrder> orders;     // level-major FIFO order
};

// --- SnapshotStore: serializer + cadence hook ---------------------------------

enum class SnapshotOutcome : uint8_t {
    Skipped = 0,        // cadence threshold not met
    Taken,              // serialized + stored
    SerializeFailed,    // book shape could not be encoded (suspect input)
    StoreFailed,        // sink rejected (I/O)
};

[[nodiscard]] const char* snapshot_outcome_str(SnapshotOutcome o) noexcept;

struct SnapshotPolicy {
    uint64_t trade_interval = 100'000;                  // every 100k trades
    uint64_t interval_ns = 300ull * 1'000'000'000ull;   // or 5 minutes
};

class SnapshotStore {
public:
    SnapshotStore(ISnapshotSink& sink, SnapshotPolicy policy = {}) noexcept;

    // Engine hook (matching thread). `wal_seq` = the WAL tail cursor the
    // snapshot covers (wal.tail_seq() at capture). `now_ns` = engine logical
    // clock (TIME_TICK discipline — never clock_gettime on the matching
    // thread). `trades_delta` = trades executed since the previous call.
    // Serializes + persists only when trades accumulated >= trade_interval
    // OR now_ns - last_snapshot_ns >= interval_ns.
    [[nodiscard]] SnapshotOutcome maybe_snapshot(const OrderBook& book,
                                                 uint32_t instrument_id,
                                                 uint64_t wal_seq,
                                                 uint64_t now_ns,
                                                 uint64_t trades_delta) noexcept;

    // Unconditional snapshot (drain/shutdown paths, tests).
    [[nodiscard]] SnapshotOutcome force_snapshot(const OrderBook& book,
                                                 uint32_t instrument_id,
                                                 uint64_t wal_seq) noexcept;

    // Serialize the book into the pinned+extension blob. `out` is replaced.
    // header_out.book_seq = wal_seq (the covered WAL cursor). Cold path —
    // allocates; matching thread calls between events (spec §3.5 cadence).
    [[nodiscard]] static bool serialize_book(const OrderBook& book,
                                             uint32_t instrument_id,
                                             uint64_t wal_seq,
                                             std::vector<uint8_t>& out,
                                             WalBookSnapshotHeader& header_out) noexcept;

    // Parse + structurally validate a blob. Fails (returns false) on size
    // mismatch, bad ext magic/version, count divergence, out-of-range level
    // index, or pinned/ext field disagreement. Cold path — allocates.
    [[nodiscard]] static bool parse_book(const uint8_t* blob, uint64_t len,
                                         ParsedSnapshot& out) noexcept;

    [[nodiscard]] uint64_t snapshots_taken() const noexcept { return taken_; }
    [[nodiscard]] uint64_t last_snapshot_seq() const noexcept { return last_seq_; }
    [[nodiscard]] uint64_t last_snapshot_ns() const noexcept { return last_ns_; }
    [[nodiscard]] uint64_t trades_accumulated() const noexcept { return trades_acc_; }

private:
    ISnapshotSink& sink_;
    SnapshotPolicy policy_;
    uint64_t taken_ = 0;
    uint64_t last_seq_ = 0;
    uint64_t last_ns_ = 0;
    uint64_t trades_acc_ = 0;
    bool first_done_ = false;
};

}  // namespace exch
