#pragma once

// PHASE-04 TASK-4.3.1 — WAL snapshotting to PostgreSQL: the notification /
// acknowledgement seam between the C++ core and the Go recovery service
// (spec §3.5).
//
// Snapshot payloads are multi-MB FlatBuffers-style blobs — far past any
// MTU-class transport — so they travel through the filesystem the way they
// always have (FileSnapshotSink atomic rename into the shared snap dir).
// What crosses process boundaries is a COMPACT descriptor:
//
//   core -> Go   "{base}_{shard}_snap"      SnapReadyMsg   (SPSC ShmRing)
//   Go -> core   "{base}_{shard}_snap_ack"  SnapAckMsg     (SPSC ShmRing)
//
// SnapReadyMsg carries the snap file's path (relative to the shared snap
// root), its byte size, the WAL cursor the snapshot covers
// (header.book_seq == Wal::tail_seq() at capture — entries with seq <
// snapshot_seq are reflected in the snapshot; replay resumes at seq ==
// snapshot_seq), and a CRC32C over the whole file. The Go service reads
// the file, verifies size + CRC, INSERTs into book_snapshots (migration
// 023), then acknowledges with SnapAckMsg{snapshot_seq}.
//
// The ack is the trim gate: SnapshotManager::poll_acks() drains the ack
// ring, tracks the highest confirmed snapshot_seq, and calls
// Wal::trim_sealed() to delete SEALED segments whose last entry seq is
// strictly below it. The strict inequality is the fail-closed boundary:
// under the tail-cursor convention the entry with seq == snapshot_seq is
// written AFTER the snapshot, so a segment containing it is never covered.
// The active segment is never a candidate (it is not in pending_archive_
// and is checked defensively).
//
// Notification loss is tolerated: a full/absent ring drops the event
// (notify_drops_ counts) and the Go service's periodic snap-dir scan is
// the catch-up path — the file rename is the durable signal, the ring is
// only the low-latency hint. Ack loss delays trimming, never correctness.

#include <cstdint>
#include <string>
#include <string_view>

#include "ipc/ShmRing.hpp"
#include "recovery/SnapshotStore.hpp"
#include "wal/Wal.hpp"

namespace exch {

// --- Wire records (packed POD, little-endian — ShmRing slot payloads) -------

#pragma pack(push, 1)
struct SnapReadyMsg {
    uint32_t magic;          // kSnapReadyMagic 'SRDY'
    uint16_t version;        // kSnapMsgVersion
    uint16_t shard_id;
    uint32_t instrument_id;
    uint32_t reserved;       // 0
    uint64_t snapshot_seq;   // WAL cursor covered (== header.book_seq)
    uint64_t byte_size;      // snap file bytes (SnapFileHeader + payload)
    uint32_t file_crc32c;    // wal_crc32c over the whole snap file
    uint32_t rel_path_len;   // bytes used in rel_path (no NUL counted)
    char     rel_path[256];  // "i{iid}/snap_{seq:020}.bin", NUL-padded
};

struct SnapAckMsg {
    uint32_t magic;          // kSnapAckMagic 'SACK'
    uint16_t version;        // kSnapMsgVersion
    uint16_t shard_id;
    uint32_t instrument_id;
    uint32_t status;         // kSnapAckPersisted on success
    uint64_t snapshot_seq;   // persisted snapshot's WAL cursor
    uint64_t snapshot_id;    // book_snapshots.snapshot_id (diagnostic)
};
#pragma pack(pop)

inline constexpr uint32_t kSnapReadyMagic = 0x59445253u;  // 'SRDY'
inline constexpr uint32_t kSnapAckMagic   = 0x4B434153u;  // 'SACK'
inline constexpr uint16_t kSnapMsgVersion = 1;
inline constexpr uint32_t kSnapAckPersisted = 0;

static_assert(sizeof(SnapReadyMsg) == 296);
static_assert(sizeof(SnapAckMsg) == 32);

// Canonical shm object names (no leading slash — shm_open adds it).
[[nodiscard]] std::string snap_ready_name(std::string_view base,
                                          uint16_t shard);
[[nodiscard]] std::string snap_ack_name(std::string_view base,
                                        uint16_t shard);

// --- SnapshotManager ---------------------------------------------------------
//
// Owns the two ShmRing endpoints and the confirmed-seq trim cursor. All
// methods are noexcept and non-blocking (ShmRing try_write / peek); the
// matching-thread call sites are the snapshot-hook tail and the shutdown
// drain — cold paths only.

class SnapshotManager {
public:
    // sink = the FileSnapshotSink whose files the ready records describe
    // (paths come from the sink so the naming formula is never duplicated).
    SnapshotManager(uint16_t shard_id,
                    const FileSnapshotSink& sink) noexcept;

    // Opens the notify ring as Producer and the ack ring as Consumer.
    // Either may fail independently: a missing notify ring only loses the
    // low-latency hint (Go's dir scan still sees every snapshot); a missing
    // ack ring means no confirmations arrive and the WAL simply never
    // trims — both are safe degraded states, so open() reports per-ring
    // success and never fails the boot.
    void open(std::string_view notify_name, std::string_view ack_name,
              uint32_t capacity = 128,
              uint32_t slot_payload = ShmRing::kDefaultSlotPayload) noexcept;
    void close() noexcept;

    [[nodiscard]] bool notify_open() const noexcept { return notify_.is_open(); }
    [[nodiscard]] bool ack_open() const noexcept { return ack_.is_open(); }

    // Emit SnapReadyMsg for a snapshot the sink just durably stored.
    // Re-reads the snap file to stamp byte_size + file_crc32c — the
    // integrity pair the Go writer verifies before INSERT. Returns false
    // on file I/O failure or ring backpressure (counted in notify_drops_).
    [[nodiscard]] bool notify_stored(uint32_t instrument_id,
                                     uint64_t snapshot_seq) noexcept;

    // Drain pending SnapAckMsg records; on a higher confirmed snapshot_seq,
    // trim sealed WAL segments fully covered by it. Returns the number of
    // segments deleted by this call.
    uint64_t poll_acks(Wal& wal) noexcept;

    // Highest snapshot_seq confirmed persisted in PostgreSQL so far.
    [[nodiscard]] uint64_t confirmed_seq() const noexcept { return confirmed_seq_; }
    [[nodiscard]] uint64_t notifies_sent() const noexcept { return notifies_sent_; }
    [[nodiscard]] uint64_t notify_drops() const noexcept { return notify_drops_; }
    [[nodiscard]] uint64_t acks_seen() const noexcept { return acks_seen_; }
    [[nodiscard]] uint64_t segments_trimmed() const noexcept { return trimmed_; }

private:
    uint16_t shard_id_;
    const FileSnapshotSink& sink_;
    ShmRing notify_;   // producer: core -> Go (SnapReadyMsg)
    ShmRing ack_;      // consumer: Go -> core (SnapAckMsg)
    uint64_t confirmed_seq_ = 0;
    uint64_t notifies_sent_ = 0;
    uint64_t notify_drops_ = 0;
    uint64_t acks_seen_ = 0;
    uint64_t trimmed_ = 0;
};

}  // namespace exch
