#pragma once

// PHASE-01 TASK-1.3.6 — custom binary WAL: mmap(MAP_SHARED) + fsync batch
// (1ms / 100 events), optional O_DIRECT mode with 4KB-aligned posix_memalign
// block flushing, CRC32C per entry, 1GB segment rotation with archive-ready
// tracking, and detect+truncate recovery for torn tail writes (the graduated
// ladder orchestrating repair vs rebase lands in Phase-04 Task 4.3.9; this is
// the step-1 primitive per spec §3.5).
//
// Concurrency contract: SINGLE WRITER. append()/flush()/rotate()/close() must
// run on the owning thread (the matching loop, Task 2.3.4). tail_seq() is
// atomic only so monitoring threads may observe progress. WalReader is
// independent and may scan a sealed or live segment from another thread.
//
// Failure model (spec §2.7 fail-closed): all mutating calls return WalStatus.
// ENOSPC/EFBIG surfaces as NoSpace BEFORE any partial mutation — append() either
// commits the full entry to the dirty region or changes nothing, so a retry
// after space frees is safe.
//
// Engine integration: Phase-02 Task 2.3.4. S3 archive + trim: Phase-04
// (pending_archive() hands it the sealed segment list; S3 upload is NOT here).

#include <atomic>
#include <cstdint>
#include <string>
#include <string_view>
#include <vector>

#include "wal/WalEntry.hpp"

namespace exch {

enum class WalStatus : uint8_t {
    Ok = 0,
    NotOpen,               // call without successful open()
    AlreadyOpen,
    BadHeader,             // magic/version/shard_id mismatch on existing file
    PayloadTooLarge,       // payload_len > kWalMaxPayload or entry > segment limit
    SeqRegression,         // explicit seq < next seq, or seq == kWalPadSeq
    NoSpace,               // ENOSPC/EFBIG — no partial mutation committed
    DirectIoUnavailable,   // O_DIRECT requested but kernel/fs rejected it
    Io,                    // other syscall failure; errno in last_errno()
};

[[nodiscard]] const char* wal_status_str(WalStatus s) noexcept;

struct WalOptions {
    bool direct_io = false;   // O_DIRECT + 4KB-aligned posix_memalign flushing
    uint64_t segment_limit = kWalDefaultSegmentLimit;   // rotate at 1 GiB
    uint64_t flush_interval_ns = kWalDefaultFlushNs;    // batch window: 1 ms
    uint32_t flush_events = kWalDefaultFlushEvents;     // batch window: 100 events
    uint64_t map_chunk = 16u << 20;                     // mmap growth quantum 16 MiB
    bool strict_direct = false;  // fail open() instead of buffered fallback
};

// --- Writer -------------------------------------------------------------------
//
// Segment layout: `path` is the file used initially; on rotation the next
// segment is `{parent}/{next_seq}.wal` — deploy under `wal/{shard}/` to get the
// spec §3.4 `wal/{shard}/{seq_base}.wal` layout. A sealed segment is pushed to
// pending_archive() and fsynced before the new segment accepts appends.

class Wal {
public:
    Wal(std::string_view path, uint16_t shard_id);
    Wal(std::string_view path, uint16_t shard_id, WalOptions options);
    ~Wal();

    Wal(const Wal&) = delete;
    Wal& operator=(const Wal&) = delete;
    Wal(Wal&&) = delete;
    Wal& operator=(Wal&&) = delete;

    // Creates or recovers the segment. Recovery scans the existing file:
    // a torn/garbage tail is truncated at the last valid record (recovery
    // ladder step 1 primitive), then appends resume at last_seq+1.
    [[nodiscard]] WalStatus open();

    // Appends one entry; seq auto-assigned, timestamp = now_ns(). On success
    // *out_seq (if non-null) receives the committed sequence.
    [[nodiscard]] WalStatus append(WalEventType type, const void* payload,
                                   uint32_t payload_len,
                                   uint64_t* out_seq = nullptr);
    // Explicit seq/timestamp form for deterministic replay tooling. Requires
    // seq >= tail_seq() and seq != kWalPadSeq.
    [[nodiscard]] WalStatus append(uint64_t seq, uint64_t timestamp_ns,
                                   WalEventType type, const void* payload,
                                   uint32_t payload_len);

    // Durability barrier: fsync (mmap mode) or aligned pwrite + fsync
    // (O_DIRECT mode). Also invoked automatically when flush_events pending or
    // flush_interval_ns elapsed — checked on every append().
    [[nodiscard]] WalStatus flush();

    // Forces rotation regardless of size; automatic when the next entry would
    // push the segment past segment_limit.
    [[nodiscard]] WalStatus rotate();

    void close() noexcept;

    [[nodiscard]] bool is_open() const noexcept { return open_; }
    [[nodiscard]] uint16_t shard_id() const noexcept { return shard_id_; }
    // Next sequence number to be assigned (== count of committed entries when
    // using auto-seq from 0). Atomic read — safe to poll from another thread.
    [[nodiscard]] uint64_t tail_seq() const noexcept {
        return next_seq_.load(std::memory_order_relaxed);
    }
    // Current segment path (changes on rotation).
    [[nodiscard]] const std::string& path() const noexcept { return path_; }
    [[nodiscard]] int last_errno() const noexcept { return last_errno_; }

    // Sealed segments awaiting S3 archive (Phase-04 Task 4.3.2 consumes).
    [[nodiscard]] const std::vector<std::string>& pending_archive() const noexcept {
        return pending_archive_;
    }

    // Byte accounting. In mmap mode logical == physical; in O_DIRECT mode the
    // physical offset additionally counts pad bytes — tracked independently.
    [[nodiscard]] uint64_t logical_offset() const noexcept { return logical_; }
    [[nodiscard]] uint64_t physical_offset() const noexcept { return physical_; }
    [[nodiscard]] uint64_t segment_limit() const noexcept { return opt_.segment_limit; }
    [[nodiscard]] bool using_direct_io() const noexcept { return direct_; }
    // True when O_DIRECT was requested but the fs rejected it (strict_direct=0
    // falls back to the mmap path so the WAL still works on tmpfs/overlayfs).
    [[nodiscard]] bool direct_fallback() const noexcept { return direct_fallback_; }

    // Recovery scan of the current segment as seen at open() (entries, last
    // valid seq, whether a corrupt tail was truncated, pad count).
    [[nodiscard]] const WalScanResult& last_scan() const noexcept { return last_scan_; }

    // Counters for tests/metrics.
    [[nodiscard]] uint64_t appends() const noexcept { return appends_; }
    [[nodiscard]] uint64_t flushes() const noexcept { return flushes_; }
    [[nodiscard]] uint64_t rotations() const noexcept { return rotations_; }

private:
    WalStatus recover_existing(uint64_t file_size);
    WalStatus init_fresh();
    WalStatus ensure_map_capacity(uint64_t need);   // mmap mode
    WalStatus ensure_stage_capacity(uint64_t need); // O_DIRECT mode
    WalStatus flush_direct();                       // aligned block write
    void fsync_dir(const std::string& dir) noexcept;
    // Records errno -> last_errno_, returns s (use as `return note_errno(...)`).
    [[nodiscard]] WalStatus note_errno(WalStatus s) noexcept;

    std::string path_;             // current segment path
    const uint16_t shard_id_;
    WalOptions opt_;

    int fd_ = -1;
    bool open_ = false;
    bool direct_ = false;          // effective O_DIRECT state
    bool direct_fallback_ = false;

    // mmap mode state: file is ftruncate'd to mapped_ and fully mapped.
    uint8_t* map_ = nullptr;
    uint64_t mapped_ = 0;

    // O_DIRECT mode state: staged_ bytes sit in a 4KB-aligned buffer pending
    // the next padded block write; physical_ is the on-disk offset.
    uint8_t* stage_ = nullptr;
    uint64_t stage_cap_ = 0;
    uint64_t staged_ = 0;

    uint64_t logical_ = 0;         // stream bytes (header + entries, no pads)
    uint64_t physical_ = 0;        // file bytes committed (incl. pads)
    uint64_t pads_written_ = 0;

    std::atomic<uint64_t> next_seq_{0};
    uint32_t pending_events_ = 0;
    uint64_t last_flush_ns_ = 0;
    int last_errno_ = 0;

    std::vector<std::string> pending_archive_;
    WalScanResult last_scan_{};

    uint64_t appends_ = 0;
    uint64_t flushes_ = 0;
    uint64_t rotations_ = 0;
};

// --- Reader -------------------------------------------------------------------
// mmap PROT_READ scan of a segment for recovery/replay tooling. next() yields
// entries head->tail, skipping pad regions, and reports End/Corrupt. Purely
// read-only — truncation decisions belong to Wal::open().

class WalReader {
public:
    WalReader() = default;
    explicit WalReader(std::string_view path) { (void)open(path); }
    ~WalReader();

    WalReader(const WalReader&) = delete;
    WalReader& operator=(const WalReader&) = delete;

    [[nodiscard]] WalStatus open(std::string_view path);
    void close() noexcept;

    // Entry/End/Corrupt. On Corrupt, offset() is the truncate target.
    [[nodiscard]] WalScanStep next(WalEntryView& out) noexcept;

    [[nodiscard]] bool is_open() const noexcept { return fd_ >= 0; }
    [[nodiscard]] uint32_t magic() const noexcept { return magic_; }
    [[nodiscard]] uint16_t version() const noexcept { return version_; }
    [[nodiscard]] uint16_t shard_id() const noexcept { return hdr_shard_; }
    [[nodiscard]] uint64_t file_size() const noexcept { return size_; }
    [[nodiscard]] uint64_t offset() const noexcept { return pos_; }
    [[nodiscard]] uint64_t entries_seen() const noexcept { return entries_; }
    [[nodiscard]] uint64_t last_seq() const noexcept { return last_seq_; }
    [[nodiscard]] bool corrupt_seen() const noexcept { return corrupt_; }
    [[nodiscard]] const std::string& path() const noexcept { return path_; }

private:
    std::string path_;
    const uint8_t* map_ = nullptr;
    uint64_t size_ = 0;
    uint64_t pos_ = sizeof(WalFileHeader);
    uint64_t entries_ = 0;
    uint64_t last_seq_ = 0;
    uint32_t magic_ = 0;
    uint16_t version_ = 0;
    uint16_t hdr_shard_ = 0;
    bool corrupt_ = false;
    int fd_ = -1;
};

}  // namespace exch
