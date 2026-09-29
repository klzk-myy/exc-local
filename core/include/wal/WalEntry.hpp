#pragma once

// Binary WAL wire format (Task 1.3.6, spec §3.4).
//
// Frame: [WalFileHeader] then repeated entries:
//   WalEntryHeader | payload[payload_len] | crc32 u32
// The CRC32 (CRC32C, Castagnoli — SSE4.2 `crc32` instruction or table fallback)
// covers the 21-byte entry header followed by the payload; the stored u32
// trailer follows the payload.
//
// O_DIRECT mode pads every flushed batch to a 4KB block boundary:
//   - pad >= 8 bytes: u64 kWalPadSeq sentinel (never a valid seq) then zeros
//   - pad 1..7 bytes: zeros only
// The scanner below recognizes both forms plus the "zeros to EOF" tail of a
// preallocated segment, so readers of either mode share one code path.
//
// Little-endian host assumed (x86-64 target per spec §3).

#include <cstddef>
#include <cstdint>
#include <cstring>

namespace exch {

inline constexpr uint32_t kWalMagic = 0x57414C00;
inline constexpr uint16_t kWalVersion = 1;

enum class WalEventType : uint8_t {
    ORDER_NEW,
    ORDER_CANCEL,
    ORDER_MODIFY,
    TRADE,
    TIME_TICK,      // deterministic clock tick for GTD/DAY replay (Task 2.3.10)
    BOOK_SNAPSHOT,
    // Task 2.3.12 — cross-shard margin slice commitments (spec §5.35/§13.1).
    // Payloads are the packed structs in risk/CrossShardMarginCoordinator.h.
    // Only committed state transitions are logged: an ACK'd grant writes
    // MARGIN_RESERVE, a release/expiry/compensation writes MARGIN_RELEASE.
    // In-flight (un-ACK'd) requests are never logged — they carry no
    // committed state and must not survive restart (fail-closed recovery).
    MARGIN_RESERVE,
    MARGIN_RELEASE,
    // Task 2.3.18 — immutable STP audit record (spec §6.5, §24 #279-280).
    // Emitted when a mutually-requested TRANSFER prevents a cross-account
    // match inside one trade_group_id: NO TRADE is emitted. The payload is
    // WalPreventedMatchPayload. Recovery replays it as a book-level no-op
    // (the maker-side ORDER_CANCEL/ORDER_MODIFY entries that accompany it
    // carry the actual state change); Phase-03's GL service consumes it for
    // the balanced prevented-notional posting.
    PREVENTED_MATCH,
    // Phase-14 Task 14.3.1 — OCO (one-cancels-other) linkage record
    // (spec §6.2/§6.5, §24 #47). Journaled when the engine installs an OCO
    // pair — BEFORE either member's ORDER_NEW entry per the wire protocol
    // (the OcoLink IPC event precedes both legs), so replay reconstructs
    // the link before the fills/cancels that exercise it. Payload is
    // WalOcoLinkPayload. The sibling cancel itself is journaled as an
    // ORDER_CANCEL with reason kWalCancelReasonOcoLink (7) — it re-derives
    // on replay exactly like every other engine-driven cancel.
    OCO_LINK,
    // Phase-15 Tasks 15.3.6/15.3.10 — instrument auction phase transition
    // (spec §7.3 reopening call auction + crossed-book quarantine). One row
    // per committed transition: CALL (control key armed -> accumulate),
    // EXTEND (deadline push, 30s cadence), UNCROSS (single-price clearing
    // committed — its TRADE rows follow immediately), CANCEL (control key
    // withdrawn mid-CALL), QUARANTINE (auction-clearing failure or an
    // unexplained crossed book — fail-closed halt of the instrument).
    // Payload is WalAuctionPhasePayload. Replay re-runs the transition in
    // the journal-free engine — the engine's own deadline resolution then
    // no-ops on the already-consumed auction id (idempotent re-derivation).
    AUCTION_PHASE,
    // Phase-16 (Tasks 16.3.3/15/11/13/16/17) — extended order-admission
    // record. Written INSTEAD OF ORDER_NEW whenever the aux record carries
    // fields the 80-byte legacy payload cannot express (trigger_source,
    // peg_mode/offset/limit, trail_unit/distance/activation). The head is a
    // verbatim WalOrderNewPayload so the base fields decode identically.
    // Payload is WalOrderNewExPayload.
    ORDER_NEW_EX,
    // Phase-16 Task 16.3.17 (spec §6.2a): audit row emitted when a pending
    // conditional order converts to a live taker (or, for GSLO, an
    // immediate guaranteed fill) — records the trigger_source and the
    // exact evaluation price that crossed the armed threshold.
    // Informational on replay (the replaying engine re-derives the
    // trigger); wal_audit counts and verifies it. Payload is
    // WalOrderTriggeredPayload.
    ORDER_TRIGGERED,
    // Phase-16 Task 16.3.11: one row per committed pegged-order reprice —
    // old/new price plus the book reference the reprice followed.
    // Informational on replay (the replaying engine re-derives each
    // transition); the log supplies the audit trail. Payload is
    // WalPegRepricePayload.
    PEG_REPRICE,
};

#pragma pack(push, 1)
struct WalFileHeader {
    uint32_t magic;     // kWalMagic
    uint16_t version;   // kWalVersion
    uint16_t shard_id;
};

struct WalEntryHeader {
    uint64_t seq;          // monotonic sequence; kWalPadSeq is reserved (pad sentinel)
    uint64_t timestamp_ns;
    uint8_t event_type;    // WalEventType
    uint32_t payload_len;  // payload follows; crc32 u32 trailer after it
};

// --- Phase-02 engine event payloads (Task 2.3.2 writer, Task 2.3.4 replayer) --
//
// These packed structs are the WAL contract for matching-engine state changes.
// The engine (matching/WalWriter) encodes them verbatim; RecoveryManager
// decodes them verbatim. Both sides must agree on field order — extend by
// APPENDING new structs/enumerators only, never reorder.

struct WalOrderNewPayload {
    uint64_t order_id;
    uint64_t account_id;
    uint32_t instrument_id;
    uint8_t  side;          // wire::Side (0=Buy,1=Sell)
    uint8_t  type;          // wire::OrderType
    uint8_t  tif;           // wire::TimeInForce
    uint8_t  flags;         // bit0 post_only, bit1 reduce_only, bit2 stp_cancel_newest...
    int64_t  price_ticks;
    int64_t  qty_units;
    int64_t  visible_qty_units;   // iceberg display slice; == qty_units when non-iceberg
    int64_t  stop_price_ticks;    // 0 unless stop order
    uint32_t stp_mode;            // wire STP mode (Task 2.3.11); 0 = CANCEL_NEWEST
    uint32_t trade_group_id;      // 0 = none (accounts.trade_group_id, migration 072)
    int64_t  gtd_expiry_ns;       // 0 unless TIF=GTD
    uint8_t  _pad[8];
};

struct WalOrderCancelPayload {
    uint64_t order_id;
    uint64_t account_id;    // for auth-check replay consistency
    uint8_t  reason;        // 0=user, 1=expired(GTD/DAY), 2=STP, 3=FOK_unfilled, 4=IOC_remainder
    uint8_t  _pad[7];
};

struct WalOrderModifyPayload {
    uint64_t order_id;
    int64_t  new_price_ticks;
    int64_t  new_qty_units;
    int64_t  new_stop_price_ticks;
    uint8_t  _pad[8];
};

struct WalTradePayload {
    uint64_t trade_id;
    uint64_t buy_order_id;
    uint64_t sell_order_id;
    uint32_t instrument_id;
    uint8_t  _pad0[4];
    int64_t  price_ticks;
    int64_t  qty_units;
};

struct WalTimeTickPayload {
    uint64_t tick_ns;       // engine logical clock value applied to expiry checks
    uint8_t  _pad[8];
};

// BOOK_SNAPSHOT payload: WalBookSnapshotHeader followed by `level_count` ×
// WalSnapshotLevel records, then `order_count` × WalSnapshotOrder records.
// Levels first (bids descending then asks ascending), then orders FIFO per
// level in the same order. This gives deterministic bit-identical replay.
struct WalBookSnapshotHeader {
    uint32_t instrument_id;
    uint32_t level_count;
    uint64_t order_count;
    uint64_t book_seq;      // book_seq_ at snapshot time — boot invariant source
};

struct WalSnapshotLevel {
    int64_t  price_ticks;
    uint8_t  side;          // 0=bid, 1=ask
    uint8_t  _pad[7];
};

struct WalSnapshotOrder {
    uint64_t order_id;
    uint64_t account_id;
    int64_t  qty_units;         // remaining (qty - filled)
    int64_t  visible_qty_units;
    int64_t  stop_price_ticks;
    uint32_t stp_mode;
    uint8_t  tif;
    uint8_t  _pad[3];
};

// Task 2.3.18 — PREVENTED_MATCH payload: immutable audit row for a prevented
// self-/group-match (mirrors the `prevented_matches` table, migration 072).
// maker_order_id is deliberately the leading field: the RecoveryManager's
// generic cancel/modify dispatcher reads the first u64 as an order id for
// book routing, so an unmodified replayer resolves this record to the
// maker's book and no-ops under its default arm — no book mutation, fully
// deterministic.
struct WalPreventedMatchPayload {
    uint64_t maker_order_id;
    uint64_t taker_order_id;
    uint64_t maker_account_id;
    uint64_t taker_account_id;
    int64_t  price_ticks;                // book level price of the prevented match
    int64_t  maker_prevented_qty_units;  // qty suppressed on the resting order
    int64_t  taker_prevented_qty_units;  // incoming remainder suppressed
    int64_t  prevented_notional_units;   // maker qty*price/1e8, saturates INT64_MAX
    uint32_t trade_group_id;             // shared group (migration 072)
    uint8_t  mode;                       // StpAction applied (5 = TRANSFER)
    uint8_t  _pad0[3];
    uint64_t ts_ns;                      // engine logical clock (== header ts)
};

// Phase-14 Task 14.3.1 — OCO_LINK payload: the durable record that two
// order ids form one OCO pair. link_id doubles as orders.oco_group_id on
// the service side; order_id_a/order_id_b are the two member legs.
// Replayed verbatim into the replay engine's link table — recovery
// validates exact payload length and re-runs the engine's own link
// validation (conflict/duplicate arms are idempotent no-ops there).
struct WalOcoLinkPayload {
    uint64_t link_id;
    uint64_t order_id_a;
    uint64_t order_id_b;
    uint64_t account_id;
    uint32_t instrument_id;
    uint8_t  _pad[4];
};

// Phase-15 Tasks 15.3.6/15.3.10 — AUCTION_PHASE payload. instrument_id is
// the leading-dispatch field (RecoveryManager routes instrument-scoped
// entries through it, identical to ORDER_NEW/TRADE/OCO_LINK).
//
//   phase:     0=CALL 1=EXTEND 2=UNCROSS 3=CANCEL 4=QUARANTINE
//              5=STRIKE_FAILED (deadline struck, uncross could not form a
//              clearing price — :result=FAILED written; the Go EXTEND
//              ladder decides extend-vs-suspend)
//   reason:    0=none, 1=control_key, 2=deadline_moved, 3=clearing_failed,
//              4=crossed_book_detected
//   auction_id: opaque id of the armed CALL — the parsed CALL:<deadline>
//              deadline itself. Identity dedupes re-armed notifications of
//              the same auction and blocks re-entry after completion.
//   deadline_ns: current uncross deadline (CALL/EXTEND rows).
//   cleared_price_ticks/cleared_qty_units: the single clearing price and
//              executed volume (UNCROSS rows; 0 elsewhere).
//   extension_count: extensions consumed at the time of this transition.
//   flags:     bit0 = post-transition the instrument is quarantined
//              (engine-local halt/suspend — orders reject, cancels live).
struct WalAuctionPhasePayload {
    uint32_t instrument_id;
    uint8_t  phase;
    uint8_t  reason;
    uint8_t  extension_count;
    uint8_t  flags;
    uint64_t auction_id;
    int64_t  deadline_ns;
    int64_t  cleared_price_ticks;
    int64_t  cleared_qty_units;
    uint8_t  _pad[16];
};

// Phase-16 — ORDER_NEW_EX: extended admission record. `base` is a verbatim
// WalOrderNewPayload; the extension carries the conditional/peg metadata
// the 80-byte legacy payload cannot express. Written only when the aux
// record has non-default advanced fields — plain orders keep emitting
// ORDER_NEW so pre-16 journals are byte-identical.
struct WalOrderNewExPayload {
    WalOrderNewPayload base;
    uint8_t  trigger_source;         // kTriggerSource*
    uint8_t  peg_mode;               // kPeg*
    uint8_t  trail_unit;             // kTrailUnit*
    uint8_t  _f0;
    int64_t  peg_offset_ticks;       // signed tick offset off the reference
    int64_t  peg_limit_ticks;        // collar; 0 = none
    int64_t  trail_distance;         // pips | pct*100 | ticks per trail_unit
    int64_t  activation_price_ticks; // trailing gate; 0 = armed at admission
    uint8_t  _pad[8];
};

// Phase-16 — ORDER_TRIGGERED: conditional-order activation audit row.
// order_kind is the wal order-type byte of the armed order
// (kWalOrderType*); observed_price_ticks is the evaluation reference
// (LAST trade price / mark / index) that crossed the armed threshold.
struct WalOrderTriggeredPayload {
    uint64_t order_id;
    uint64_t account_id;
    uint32_t instrument_id;
    uint8_t  trigger_source;         // kTriggerSource*
    uint8_t  order_kind;             // kWalOrderType* of the armed order
    uint8_t  _pad0[2];
    int64_t  stop_price_ticks;       // armed threshold that fired
    int64_t  observed_price_ticks;   // reference value that crossed it
    uint8_t  _pad[8];
};

// Phase-16 — PEG_REPRICE: one row per committed pegged-order reprice.
struct WalPegRepricePayload {
    uint64_t order_id;
    uint32_t instrument_id;
    uint8_t  peg_mode;               // kPeg*
    uint8_t  _pad0[3];
    int64_t  old_price_ticks;
    int64_t  new_price_ticks;
    int64_t  ref_price_ticks;        // book reference the reprice followed
    uint8_t  _pad[8];
};
#pragma pack(pop)

static_assert(sizeof(WalFileHeader) == 8);
static_assert(sizeof(WalEntryHeader) == 21);
static_assert(sizeof(WalOrderNewPayload) == 80);
static_assert(sizeof(WalOrderCancelPayload) == 24);
static_assert(sizeof(WalOrderModifyPayload) == 40);
static_assert(sizeof(WalTradePayload) == 48);
static_assert(sizeof(WalTimeTickPayload) == 16);
static_assert(sizeof(WalBookSnapshotHeader) == 24);
static_assert(sizeof(WalSnapshotLevel) == 16);
static_assert(sizeof(WalSnapshotOrder) == 48);
static_assert(sizeof(WalPreventedMatchPayload) == 80);
static_assert(sizeof(WalOcoLinkPayload) == 40);
static_assert(sizeof(WalAuctionPhasePayload) == 56);
static_assert(sizeof(WalOrderNewExPayload) == 124);
static_assert(sizeof(WalOrderTriggeredPayload) == 48);
static_assert(sizeof(WalPegRepricePayload) == 48);
static_assert(__BYTE_ORDER__ == __ORDER_LITTLE_ENDIAN__,
              "WAL wire format is little-endian (spec §3.4)");

// --- Format constants -------------------------------------------------------

inline constexpr uint64_t kWalBlockSize = 4096;          // O_DIRECT flush quantum
inline constexpr uint64_t kWalPadSeq = ~uint64_t{0};     // seq slot => pad marker
inline constexpr uint32_t kWalMaxPayload = 64u * 1024 * 1024;  // sanity cap 64 MiB
inline constexpr uint64_t kWalDefaultSegmentLimit = 1ull << 30;  // 1 GiB rotation
inline constexpr uint64_t kWalDefaultFlushNs = 1'000'000;        // 1 ms batch window
inline constexpr uint32_t kWalDefaultFlushEvents = 100;
inline constexpr uint32_t kWalEntryOverhead =
    sizeof(WalEntryHeader) + sizeof(uint32_t);           // 21 + 4 = 25

// --- CRC32C -----------------------------------------------------------------

// Runtime SSE4.2 probe (cpuid via __builtin_cpu_supports). crc32c() dispatches
// to the hardware instruction when present, else the table-driven software
// implementation. Both compute identical CRC32C values.
[[nodiscard]] bool wal_crc32c_hardware() noexcept;
[[nodiscard]] uint32_t wal_crc32c(const void* data, std::size_t len) noexcept;
[[nodiscard]] uint32_t wal_crc32c_sw(const void* data, std::size_t len) noexcept;
// Streaming form: crc value carries across calls.
[[nodiscard]] uint32_t wal_crc32c_continue(uint32_t crc, const void* data,
                                           std::size_t len) noexcept;

// --- Entry encode / pad emit -------------------------------------------------

// Serializes header+payload+crc into dst (must hold kWalEntryOverhead+len bytes).
// Returns bytes written.
uint64_t wal_encode_entry(uint8_t* dst, uint64_t seq, uint64_t timestamp_ns,
                          WalEventType type, const void* payload,
                          uint32_t payload_len) noexcept;

// Emits a pad region of pad_bytes at dst (pad_bytes >= 1). Writes the kWalPadSeq
// sentinel when pad_bytes >= 8, else zeros only.
void wal_emit_pad(uint8_t* dst, uint64_t pad_bytes) noexcept;

// --- Scanner -----------------------------------------------------------------
// Shared by Wal recovery (detect + truncate) and WalReader iteration. Operates
// on a memory image of the segment (mmap'd or read into a buffer).

enum class WalScanStep : uint8_t { Entry, Pad, End, Corrupt };

struct WalEntryView {
    uint64_t seq;
    uint64_t timestamp_ns;
    WalEventType type;
    const uint8_t* payload;   // points into the scanned image; not copied
    uint32_t payload_len;
    uint64_t offset;          // byte offset of this record inside the segment
    uint64_t record_bytes;    // kWalEntryOverhead + payload_len
};

// Examines the record at *pos in image [base, size).
//   Entry:   *out filled, *pos advances past the record.
//   Pad:     *pos advances to the next 4KB boundary.
//   End:     clean end of log (*pos unchanged = valid tail offset).
//   Corrupt: torn/garbage record (*pos unchanged = truncate target).
[[nodiscard]] WalScanStep wal_scan_step(const uint8_t* base, uint64_t size,
                                        uint64_t* pos, WalEntryView* out) noexcept;

struct WalScanResult {
    bool header_ok = false;      // magic + version valid
    uint16_t header_version = 0;
    uint16_t header_shard = 0;
    uint64_t entries = 0;        // valid entries seen
    uint64_t last_seq = 0;       // seq of last valid entry (valid iff entries > 0)
    uint64_t pads = 0;           // O_DIRECT pad regions skipped
    uint64_t entry_bytes = 0;    // sum of record_bytes over valid entries
    uint64_t pad_bytes = 0;      // sum of pad-region sizes skipped
    uint64_t valid_end = sizeof(WalFileHeader);  // truncate target
    bool corrupt = false;        // torn/garbage tail detected at valid_end
};

// Validates the file header then scans entries from offset sizeof(WalFileHeader)
// until End/Corrupt. valid_end is the offset after the last valid record or pad.
[[nodiscard]] WalScanResult wal_scan(const uint8_t* base, uint64_t size) noexcept;

// --- Unaligned load helpers (internal; public for tests) ----------------------

[[nodiscard]] inline uint64_t wal_load64(const void* p) noexcept {
    uint64_t v;
    std::memcpy(&v, p, sizeof(v));
    return v;
}
[[nodiscard]] inline uint32_t wal_load32(const void* p) noexcept {
    uint32_t v;
    std::memcpy(&v, p, sizeof(v));
    return v;
}

}  // namespace exch
