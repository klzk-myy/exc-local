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
//   [WalSnapshotOrderExt × order_count]          60 B — restore extension
//   [WalSnapshotAuxHeader]                       48 B — ext v3+ only: engine
//                                                       side-table counts
//   [WalSnapshotOrderAux    × meta_count]        48 B — OrderMeta rows
//                                                       (resting + pending)
//   [WalSnapshotPendingOrder × pending_count]   148 B — off-book conditional
//                                                       orders (stop queue)
//   [WalSnapshotIceberg     × iceberg_count]     32 B — hidden-reserve rows
//   [WalSnapshotOco         × oco_count]         40 B — OCO link rows
//   [WalSnapshotPeg         × peg_count]         32 B — pegged-order rows
//   [WalSnapshotCounters]                        16 B — ext v2+ only: engine
//                                                       allocator counters
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
#include <limits>
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

// Counters trailer (ext version >= 2, appended after the order-ext records —
// after the aux block on v3+): engine-owned allocator high-water marks the
// OrderBook blob cannot carry — the trade-id stream lives on the engine, not
// the book. A v1 snapshot loses the mark, so a fully snapshot-covered boot
// would restart the allocator at 1 and re-issue journaled trade ids
// (Phase-09 swap-drill finding).
#pragma pack(push, 1)
struct WalSnapshotCounters {
    uint32_t magic;          // kSnapCtrMagic
    uint16_t version;        // kSnapCtrVersion
    uint16_t _pad;
    uint64_t next_trade_id;  // engine trade-id allocator counter at capture
};

// --- Aux block (ext version >= 3) -----------------------------------------
// Engine-private side-table rows the book blob cannot carry: the OrderMeta
// table (GTD/DAY expiry, STP trade-group, amend fence, prevented qty,
// conditional trigger source/instrument), the pending-stop queue (whole
// off-book orders), iceberg hidden-reserve records, OCO links, and pegged
// order records. Without them a snapshot-covered restart loses conditional
// orders outright and un-arms expiry/STP bookkeeping on the survivors —
// book-visible state alone is not the full engine state.
struct WalSnapshotAuxHeader {
    uint32_t magic;          // kSnapAuxMagic
    uint16_t version;        // kSnapAuxVersion
    uint16_t flags;          // reserved, 0
    uint64_t meta_count;     // WalSnapshotOrderAux rows
    uint64_t pending_count;  // WalSnapshotPendingOrder rows
    uint64_t iceberg_count;  // WalSnapshotIceberg rows
    uint64_t oco_count;      // WalSnapshotOco rows
    uint64_t peg_count;      // WalSnapshotPeg rows
};

// One MatchingEngine::OrderMeta row. Keyed by order id — the order itself is
// either a book row (resting) or a WalSnapshotPendingOrder row (off-book).
struct WalSnapshotOrderAux {
    uint64_t order_id;
    int64_t  expiry_ns;          // GTD/DAY deadline; 0 = none (heap re-arm)
    uint64_t amend_seq;          // §6.9 amend fence — last APPLIED ingress seq
    int64_t  prevented_qty_units;// cumulative STP-suppressed qty
    uint32_t trade_group_id;     // STP group (migration 072)
    uint32_t instrument_id;      // pending conditional's instrument
    uint8_t  trigger_source;     // kTriggerSource* for conditional orders
    uint8_t  amend_seen;         // amend fence armed
    uint8_t  _pad[6];
};

// Off-book pending conditional order: embeds the pinned + ext shapes so the
// restore side needs no second record family — ext.level_index carries the
// kSnapLevelOffBook sentinel (the order has no level). ord.stop_price_ticks
// is the armed trigger threshold (the field the book rows leave zeroed).
struct WalSnapshotPendingOrder {
    WalSnapshotOrder    ord;
    WalSnapshotOrderExt ext;
    int64_t  anchor_ticks;            // trailing: most-favorable reference
    int64_t  activation_price_ticks;  // trailing gate; 0 = armed
    int64_t  trail_distance;          // trailing distance (unit per trail_unit)
    int64_t  gslo_notional_units;     // GSLO exposure reserved at admission
    uint8_t  trigger_source;
    uint8_t  trail_unit;              // kTrailUnit*; 0 = plain stop
    uint8_t  armed;                   // trailing gate passed
    uint8_t  _pad[5];
};

// IcebergManager::Record minus the re-slice template — the template is the
// restored book node for the live slice (only its qty_units differs: it is
// the slice remainder, tmpl.qty_units is the TOTAL carried here).
struct WalSnapshotIceberg {
    uint64_t order_id;
    int64_t  total_qty_units;
    int64_t  filled_total_units;
    int64_t  display_qty_units;
};

struct WalSnapshotOco {
    uint64_t order_id;
    uint64_t link_id;
    uint64_t sibling_id;
    uint32_t instrument_id;
    uint8_t  state;            // kOcoArmed / kOcoDoomed
    uint8_t  _pad[11];
};

struct WalSnapshotPeg {
    uint64_t order_id;
    int64_t  offset_ticks;
    int64_t  limit_ticks;
    uint8_t  mode;             // kPeg*
    uint8_t  priced_ok;
    uint8_t  _pad[6];
};
#pragma pack(pop)

inline constexpr uint32_t kSnapExtMagic = 0x31455853u;   // 'SXE1'
inline constexpr uint16_t kSnapExtVersion = 3;
inline constexpr uint32_t kSnapCtrMagic = 0x32544353u;   // 'SCT2'
inline constexpr uint16_t kSnapCtrVersion = 1;
inline constexpr uint32_t kSnapAuxMagic = 0x33554153u;   // 'SAU3'
inline constexpr uint16_t kSnapAuxVersion = 1;
// level_index sentinel on WalSnapshotPendingOrder.ext — the pending order
// lives in the engine's stop queue, not in any book level.
inline constexpr uint32_t kSnapLevelOffBook = UINT32_MAX;
static_assert(sizeof(WalSnapshotExtHeader) == 16);
static_assert(sizeof(WalSnapshotOrderExt) == 60);
static_assert(sizeof(WalSnapshotCounters) == 16);
static_assert(sizeof(WalSnapshotAuxHeader) == 48);
static_assert(sizeof(WalSnapshotOrderAux) == 48);
static_assert(sizeof(WalSnapshotPendingOrder) == 148);
static_assert(sizeof(WalSnapshotIceberg) == 32);
static_assert(sizeof(WalSnapshotOco) == 40);
static_assert(sizeof(WalSnapshotPeg) == 32);

// --- Engine-side aux provider (serialize input) ----------------------------
// Wire-ready engine side-table contents the book cannot see. The matching
// engine marshals its meta/stop/iceberg/oco/peg tables into these PODs; the
// serializer stays engine-agnostic. SnapshotAuxPending.order is a pool-owned
// node read for its full order template — the writer never mutates it.
struct SnapshotAuxPending {
    Order*   order;                  // full order template (not book-resting)
    int64_t  stop_price_ticks = 0;
    int64_t  anchor_ticks = 0;
    int64_t  activation_price_ticks = 0;
    int64_t  trail_distance = 0;
    int64_t  gslo_notional_units = 0;
    uint8_t  trigger_source = 0;
    uint8_t  trail_unit = 0;
    uint8_t  armed = 1;
};

// Iceberg restore input: the re-slice template (qty_units = TOTAL) plus the
// manager's cumulative counters.
struct SnapshotAuxIceberg {
    Order    tmpl;
    int64_t  filled_total_units = 0;
    int64_t  display_qty_units = 0;
};

struct SnapshotAuxState {
    std::vector<WalSnapshotOrderAux> metas;        // resting + pending orders
    std::vector<SnapshotAuxPending>  pendings;     // off-book conditionals
    std::vector<SnapshotAuxIceberg>  icebergs;     // hidden-reserve records
    std::vector<WalSnapshotOco>      ocos;
    std::vector<WalSnapshotPeg>      pegs;
};

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

    // Phase-04 Task 4.3.5 ladder level 2: load the NEWER-THAN-NOTHING
    // previous generation — the second-newest retained snapshot. Used only
    // by the snapshot-rebase path when the latest snapshot fails integrity
    // (CRC/parse) so recovery can rebase onto the prior verified generation
    // instead of halting. Default: sinks that retain a single generation
    // report has_prior=false (a legal answer — never an error).
    [[nodiscard]] virtual bool load_prior(uint32_t /*instrument_id*/,
                                          uint64_t* seq_out,
                                          std::vector<uint8_t>* blob_out,
                                          bool* has_prior) noexcept {
        *seq_out = 0;
        blob_out->clear();
        *has_prior = false;
        return true;
    }
};

// --- Phase-02 file sink ------------------------------------------------------
//
// Layout: {root}/i{instrument_id}/snap_{seq:020}.bin
//   [SnapFileHeader][payload = the blob]   — the payload CRC rides in the
//   header (payload_crc); no trailer is written.
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
// File-snapshot payload bound. SnapFileHeader.payload_len is u32, so the
// format's representable maximum IS the cap; integrity on load comes from
// the exact file-size cross-check + payload CRC (a corrupt length field
// fails the size check before any allocation). kWalMaxPayload (64 MiB)
// bounds WAL *entries* — standalone snapshot files legitimately exceed it
// on large books (Phase-02.5 soak finding: >64MiB books serialize-failed
// silently and recovery degraded to full replay).
inline constexpr uint64_t kSnapMaxPayload =
    std::numeric_limits<uint32_t>::max();
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
    // Second-newest retained snap_*.bin (Task 4.3.5 rebase fallback). False
    // on I/O/integrity failure exactly like load_latest.
    [[nodiscard]] bool load_prior(uint32_t instrument_id, uint64_t* seq_out,
                                  std::vector<uint8_t>* blob_out,
                                  bool* has_prior) noexcept override;

    [[nodiscard]] const std::string& root() const noexcept { return root_; }
    [[nodiscard]] int last_errno() const noexcept { return last_errno_; }

    // Phase-04 Task 4.3.1 — the snapshot-ready notification must name the
    // file exactly the way store() wrote it; expose the paths rather than
    // duplicating the {root}/i{iid}/snap_{seq:020}.bin formula.
    [[nodiscard]] std::string snapshot_path(uint32_t instrument_id,
                                            uint64_t seq) const {
        return file_for(instrument_id, seq);
    }
    // Path relative to root() — the field carried in SnapReadyMsg
    // ("i{iid}/snap_{seq:020}.bin").
    [[nodiscard]] std::string snapshot_rel_path(uint32_t instrument_id,
                                                uint64_t seq) const {
        return file_for(instrument_id, seq).substr(root_.size() + 1);
    }

private:
    // {root}/i{instrument_id}/snap_{seq:020}.bin
    [[nodiscard]] std::string dir_for(uint32_t instrument_id) const;
    [[nodiscard]] std::string file_for(uint32_t instrument_id,
                                       uint64_t seq) const;
    // Shared loader behind load_latest/load_prior: rank 0 = newest.
    [[nodiscard]] bool load_nth(uint32_t instrument_id, uint32_t rank,
                                uint64_t* seq_out,
                                std::vector<uint8_t>* blob_out,
                                bool* has_snapshot) noexcept;

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
    uint64_t next_trade_id = 0;                  // v2 counters; 0 = v1/absent
    // v3 aux block — engine side-table rows (empty on v1/v2 blobs).
    std::vector<WalSnapshotOrderAux>    metas;
    std::vector<WalSnapshotPendingOrder> pendings;
    std::vector<WalSnapshotIceberg>     icebergs;
    std::vector<WalSnapshotOco>         ocos;
    std::vector<WalSnapshotPeg>         pegs;
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
    // OR now_ns - last_snapshot_ns >= interval_ns. `aux` = engine side-table
    // export (matching thread collects it on the same call) — nullptr keeps
    // the v2 blob shape for book-only/test callers.
    [[nodiscard]] SnapshotOutcome maybe_snapshot(const OrderBook& book,
                                                 uint32_t instrument_id,
                                                 uint64_t wal_seq,
                                                 uint64_t now_ns,
                                                 uint64_t trades_delta,
                                                 uint64_t next_trade_id = 0,
                                                 const SnapshotAuxState* aux = nullptr) noexcept;

    // Unconditional snapshot (drain/shutdown paths, tests).
    [[nodiscard]] SnapshotOutcome force_snapshot(const OrderBook& book,
                                                 uint32_t instrument_id,
                                                 uint64_t wal_seq,
                                                 uint64_t next_trade_id = 0,
                                                 const SnapshotAuxState* aux = nullptr) noexcept;

    // Cadence pre-check (no mutation): identical predicate to the one inside
    // maybe_snapshot — callers that must marshal engine aux state use it to
    // skip the export on non-firing ticks.
    [[nodiscard]] bool would_snapshot(uint64_t now_ns,
                                      uint64_t trades_delta) const noexcept {
        return !first_done_ ||
               trades_acc_ + trades_delta >= policy_.trade_interval ||
               (first_done_ &&
                (now_ns - last_ns_) >= policy_.interval_ns);
    }

    // Serialize the book into the pinned+extension blob. `out` is replaced.
    // header_out.book_seq = wal_seq (the covered WAL cursor). Cold path —
    // allocates; matching thread calls between events (spec §3.5 cadence).
    // `aux` != nullptr writes the v3 side-table block; nullptr emits the v2
    // shape (older readers can still parse it).
    [[nodiscard]] static bool serialize_book(const OrderBook& book,
                                             uint32_t instrument_id,
                                             uint64_t wal_seq,
                                             std::vector<uint8_t>& out,
                                             WalBookSnapshotHeader& header_out,
                                             uint64_t next_trade_id = 0,
                                             const SnapshotAuxState* aux = nullptr) noexcept;

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
