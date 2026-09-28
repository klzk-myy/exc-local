// Task 1.3.6 — binary WAL engine (spec §3.4).
//
// Two write paths behind one format:
//   * mmap mode (default): file is fallocate-preallocated in map_chunk
//     quanta and fully mapped MAP_SHARED; appends memcpy the encoded entry at
//     the logical tail; flush() = fsync.
//   * O_DIRECT mode: entries are encoded into a 4KB-aligned staging buffer
//     (posix_memalign); flush() pads the batch to the next 4KB boundary
//     (kWalPadSeq sentinel + zeros) and pwrites the block — offset, length and
//     buffer all 4KB-aligned, so no EINVAL and a real page-cache bypass on
//     filesystems that honor O_DIRECT. physical_ counts on-disk bytes
//     (incl. pads); logical_ counts stream bytes (header + entries) — tracked
//     independently per the task amendment.
//
// Recovery: open() maps the existing file read-only and runs wal_scan(); the
// tail is truncated at the last valid record (detect + truncate primitive —
// the full graduated ladder lives in Phase-04 Task 4.3.9). For O_DIRECT files
// the surviving partial last block is lifted into the staging buffer and the
// file is cut back to the containing 4KB boundary so subsequent aligned writes
// stay legal.
//
// Durability ordering: batch fsync (1ms OR 100 events, whichever first,
// checked per append), fsync before rotation, parent-dir fsync after creating
// a new segment so the name survives a crash.

#include "wal/Wal.hpp"

#include <algorithm>
#include <cerrno>
#include <cstdlib>
#include <cstring>
#include <filesystem>

#include <fcntl.h>
#include <sys/mman.h>
#include <sys/stat.h>
#include <unistd.h>

#include "utils/TimeUtils.hpp"

namespace exch {
namespace {

constexpr int kOpenMode = 0644;

inline uint64_t align_up_4k(uint64_t v) noexcept {
    return (v + (kWalBlockSize - 1)) & ~(kWalBlockSize - 1);
}
inline uint64_t align_down_4k(uint64_t v) noexcept {
    return v & ~(kWalBlockSize - 1);
}

WalStatus errno_status(int e) noexcept {
    return (e == ENOSPC || e == EFBIG) ? WalStatus::NoSpace : WalStatus::Io;
}

// Whole-file read-only mapping used by recovery and WalReader. Returns
// MAP_FAILED-safe nullptr with *out_fd left -1 when size == 0.
const uint8_t* map_ro(int fd, uint64_t size) noexcept {
    if (size == 0) return nullptr;
    void* m = ::mmap(nullptr, size, PROT_READ, MAP_SHARED, fd, 0);
    return (m == MAP_FAILED) ? nullptr : static_cast<const uint8_t*>(m);
}

}  // namespace

const char* wal_status_str(WalStatus s) noexcept {
    switch (s) {
        case WalStatus::Ok: return "Ok";
        case WalStatus::NotOpen: return "NotOpen";
        case WalStatus::AlreadyOpen: return "AlreadyOpen";
        case WalStatus::BadHeader: return "BadHeader";
        case WalStatus::PayloadTooLarge: return "PayloadTooLarge";
        case WalStatus::SeqRegression: return "SeqRegression";
        case WalStatus::NoSpace: return "NoSpace";
        case WalStatus::DirectIoUnavailable: return "DirectIoUnavailable";
        case WalStatus::Io: return "Io";
    }
    return "?";
}

// --- Wal ----------------------------------------------------------------------

Wal::Wal(std::string_view path, uint16_t shard_id)
    : Wal(path, shard_id, WalOptions{}) {}

Wal::Wal(std::string_view path, uint16_t shard_id, WalOptions options)
    : path_(path), shard_id_(shard_id), opt_(options) {
    if (opt_.segment_limit < 2 * kWalBlockSize) opt_.segment_limit = 2 * kWalBlockSize;
    if (opt_.map_chunk == 0) opt_.map_chunk = kWalBlockSize;
}

Wal::~Wal() { close(); }

WalStatus Wal::note_errno(WalStatus s) noexcept {
    last_errno_ = errno;
    return s;
}

WalStatus Wal::open() {
    if (open_) return WalStatus::AlreadyOpen;
    last_errno_ = 0;

    const std::filesystem::path fs_path(path_);
    std::error_code ec;
    if (fs_path.has_parent_path())
        std::filesystem::create_directories(fs_path.parent_path(), ec);

    if (opt_.direct_io) {
        fd_ = ::open(path_.c_str(), O_WRONLY | O_CREAT | O_CLOEXEC | O_DIRECT,
                     kOpenMode);
        if (fd_ < 0 && (errno == EINVAL || errno == EOPNOTSUPP)) {
            // Filesystem rejects O_DIRECT (e.g. tmpfs/overlayfs on some
            // kernels): fall back to the identical staged-block layout over
            // buffered pwrite unless strict mode demands hard failure.
            if (opt_.strict_direct) {
                return note_errno(WalStatus::DirectIoUnavailable);
            }
            direct_fallback_ = true;
            fd_ = ::open(path_.c_str(), O_WRONLY | O_CREAT | O_CLOEXEC,
                         kOpenMode);
        }
        direct_ = (fd_ >= 0) && !direct_fallback_;
    } else {
        fd_ = ::open(path_.c_str(), O_RDWR | O_CREAT | O_CLOEXEC, kOpenMode);
    }
    if (fd_ < 0) {
        const WalStatus s = errno_status(errno);
        return note_errno(s);
    }

    struct stat st {};
    if (::fstat(fd_, &st) != 0) {
        const WalStatus s = note_errno(WalStatus::Io);
        ::close(fd_);
        fd_ = -1;
        return s;
    }

    const uint64_t file_size = static_cast<uint64_t>(st.st_size);
    WalStatus s;
    if (file_size == 0) {
        s = init_fresh();
    } else {
        s = recover_existing(file_size);
    }
    if (s != WalStatus::Ok) {
        if (map_ != nullptr) {
            ::munmap(map_, mapped_);
            map_ = nullptr;
            mapped_ = 0;
        }
        ::free(stage_);
        stage_ = nullptr;
        stage_cap_ = staged_ = 0;
        ::close(fd_);
        fd_ = -1;
        return s;
    }

    // Any other sealed *.wal segment in the shard directory is archive-ready
    // (e.g. left behind by a previous process before the S3 uploader ran).
    if (fs_path.has_parent_path()) {
        std::vector<std::string> found;
        for (const auto& de :
             std::filesystem::directory_iterator(fs_path.parent_path(), ec)) {
            std::error_code tec;
            if (!de.is_regular_file(tec) || tec) continue;
            const auto p = de.path();
            if (p.extension() != ".wal") continue;
            if (p == fs_path) continue;
            found.push_back(p.string());
        }
        std::sort(found.begin(), found.end());
        pending_archive_.insert(pending_archive_.end(), found.begin(),
                                found.end());
    }

    last_flush_ns_ = steady_ns();
    open_ = true;
    return WalStatus::Ok;
}

WalStatus Wal::init_fresh() {
    if (direct_ || opt_.direct_io) {
        // Staged write path: header sits in the staging buffer until the first
        // batch flush writes one aligned block covering [0, 4KB).
        const WalStatus s = ensure_stage_capacity(2 * kWalBlockSize);
        if (s != WalStatus::Ok) return s;
        WalFileHeader h{kWalMagic, kWalVersion, shard_id_};
        std::memcpy(stage_, &h, sizeof(h));
        staged_ = sizeof(h);
        logical_ = sizeof(h);
        physical_ = 0;
    } else {
        const WalStatus s = ensure_map_capacity(kWalBlockSize);
        if (s != WalStatus::Ok) return s;
        WalFileHeader h{kWalMagic, kWalVersion, shard_id_};
        std::memcpy(map_, &h, sizeof(h));
        logical_ = sizeof(h);
        physical_ = logical_;
    }
    // NOTE: next_seq_ is deliberately NOT reset here — init_fresh also runs
    // inside rotate(), where the sequence counter must carry across segments.
    // Fresh-open already has next_seq_ == 0 from member init.
    pending_events_ = 0;
    return WalStatus::Ok;
}

WalStatus Wal::recover_existing(uint64_t file_size) {
    // Read-only view for the scan regardless of the writer's fd mode.
    int rfd = ::open(path_.c_str(), O_RDONLY | O_CLOEXEC);
    if (rfd < 0) {
        return note_errno(WalStatus::Io);
    }
    const uint8_t* img = map_ro(rfd, file_size);
    if (img == nullptr) {
        ::close(rfd);
        return note_errno(WalStatus::Io);
    }
    last_scan_ = wal_scan(img, file_size);

    if (!last_scan_.header_ok) {
        // A brand-new segment whose header write was torn (all-zero start)
        // carries no data: reinitialize instead of failing. Nonzero garbage in
        // the header is genuinely foreign/corrupt -> BadHeader (fail closed).
        const uint64_t probe = file_size < 8 ? file_size : 8;
        bool zeros = true;
        for (uint64_t i = 0; i < probe; ++i)
            if (img[i] != 0) { zeros = false; break; }
        ::munmap(const_cast<uint8_t*>(img), file_size);
        ::close(rfd);
        if (zeros && file_size <= kWalBlockSize) {
            if (::ftruncate(fd_, 0) != 0) {
                return note_errno(WalStatus::Io);
            }
            return init_fresh();
        }
        return WalStatus::BadHeader;
    }
    if (last_scan_.header_shard != shard_id_) {
        ::munmap(const_cast<uint8_t*>(img), file_size);
        ::close(rfd);
        return WalStatus::BadHeader;
    }

    const uint64_t valid_end = last_scan_.valid_end;
    // Resume sequence: normally last+1, but a numeric filename stem is the
    // segment's seq_base (rotate() names {next_seq}.wal). A crash landing
    // between rotate() and the first append leaves a header-only segment —
    // without the stem floor, next_seq would rewind to 0 and duplicate the
    // sealed segments' sequence range (found by Phase-02.5 failover bench).
    uint64_t resume = last_scan_.entries > 0 ? last_scan_.last_seq + 1 : 0;
    {
        const std::string stem =
            std::filesystem::path(path_).stem().string();
        char* end = nullptr;
        const unsigned long long base = std::strtoull(stem.c_str(), &end, 10);
        if (end != stem.c_str() && *end == '\0' && base > resume) {
            resume = base;
        }
    }
    next_seq_.store(resume, std::memory_order_relaxed);

    if (opt_.direct_io) {
        // Keep the surviving partial last block in the staging buffer and cut
        // the file back to its containing 4KB boundary — O_DIRECT writes may
        // only start on aligned offsets.
        const uint64_t blk = align_down_4k(valid_end);
        const uint64_t tail = valid_end - blk;
        WalStatus s = ensure_stage_capacity(tail + 2 * kWalBlockSize);
        if (s == WalStatus::Ok && tail != 0)
            std::memcpy(stage_, img + blk, tail);
        ::munmap(const_cast<uint8_t*>(img), file_size);
        ::close(rfd);
        if (s != WalStatus::Ok) return s;
        staged_ = tail;
        // logical_ = stream bytes (header + entries); pads live in physical_.
        logical_ = sizeof(WalFileHeader) + last_scan_.entry_bytes;
        physical_ = blk;
        pads_written_ = last_scan_.pad_bytes;
        if (valid_end < file_size && ::ftruncate(fd_, static_cast<off_t>(blk)) != 0) {
            return note_errno(WalStatus::Io);
        }
        return WalStatus::Ok;
    }

    ::munmap(const_cast<uint8_t*>(img), file_size);
    ::close(rfd);
    // mmap mode: cut the torn tail, then the map regrows lazily on append.
    if (valid_end < file_size && ::ftruncate(fd_, static_cast<off_t>(valid_end)) != 0) {
        return note_errno(WalStatus::Io);
    }
    logical_ = valid_end;
    physical_ = valid_end;
    mapped_ = 0;
    return WalStatus::Ok;
}

WalStatus Wal::ensure_map_capacity(uint64_t need) {
    if (need <= mapped_) return WalStatus::Ok;
    uint64_t cap = opt_.map_chunk;
    if (mapped_ * 2 > cap) cap = mapped_ * 2;
    if (cap < need) cap = need;
    cap = align_up_4k(cap);
    if (cap > opt_.segment_limit) cap = opt_.segment_limit;
    if (cap < need) return WalStatus::PayloadTooLarge;  // single entry > limit

    // Reserve blocks first so a full disk surfaces as ENOSPC/EFBIG here —
    // before any mmap write could take a fatal SIGBUS. Filesystems without
    // fallocate degrade to ftruncate; the residual SIGBUS risk on those is
    // documented. NOTE: posix_fallocate returns the errno VALUE, not -1.
    int fe = ::posix_fallocate(fd_, 0, static_cast<off_t>(cap));
    if (fe != 0 && fe != EOPNOTSUPP && fe != ENOSYS && fe != EINVAL) {
        // Try the exact need — a quota/rlimit may admit the smaller span.
        const uint64_t exact = align_up_4k(need);
        if (cap != exact) {
            cap = exact;
            fe = ::posix_fallocate(fd_, 0, static_cast<off_t>(cap));
        }
        if (fe != 0 && fe != EOPNOTSUPP && fe != ENOSYS && fe != EINVAL) {
            last_errno_ = fe;
            return errno_status(fe);
        }
    }
    if (::ftruncate(fd_, static_cast<off_t>(cap)) != 0) {
        const WalStatus s = errno_status(errno);
        return note_errno(s);
    }
    if (map_ != nullptr) ::munmap(map_, mapped_);
    void* m = ::mmap(nullptr, cap, PROT_READ | PROT_WRITE, MAP_SHARED, fd_, 0);
    if (m == MAP_FAILED) {
        map_ = nullptr;
        mapped_ = 0;
        return note_errno(WalStatus::Io);
    }
    map_ = static_cast<uint8_t*>(m);
    mapped_ = cap;
    return WalStatus::Ok;
}

WalStatus Wal::ensure_stage_capacity(uint64_t need) {
    if (need <= stage_cap_) return WalStatus::Ok;
    uint64_t cap = stage_cap_ * 2;
    if (cap < need) cap = need;
    cap = align_up_4k(cap);
    void* p = nullptr;
    if (::posix_memalign(&p, kWalBlockSize, cap) != 0 || p == nullptr)
        return WalStatus::Io;
    if (staged_ != 0) std::memcpy(p, stage_, staged_);
    ::free(stage_);
    stage_ = static_cast<uint8_t*>(p);
    stage_cap_ = cap;
    return WalStatus::Ok;
}

WalStatus Wal::append(WalEventType type, const void* payload,
                      uint32_t payload_len, uint64_t* out_seq) {
    const uint64_t seq = next_seq_.load(std::memory_order_relaxed);
    const WalStatus s = append(seq, now_ns(), type, payload, payload_len);
    if (s == WalStatus::Ok && out_seq != nullptr) *out_seq = seq;
    return s;
}

WalStatus Wal::append(uint64_t seq, uint64_t timestamp_ns, WalEventType type,
                      const void* payload, uint32_t payload_len) {
    if (!open_) return WalStatus::NotOpen;
    if (payload_len > kWalMaxPayload) return WalStatus::PayloadTooLarge;
    // kWalPadSeq is reserved (pad sentinel); regressions would corrupt replay
    // ordering, so both are rejected rather than patched over.
    const uint64_t want_next = next_seq_.load(std::memory_order_relaxed);
    if (seq == kWalPadSeq || seq < want_next) return WalStatus::SeqRegression;

    const uint64_t rec = kWalEntryOverhead + payload_len;
    if (sizeof(WalFileHeader) + rec + kWalBlockSize > opt_.segment_limit)
        return WalStatus::PayloadTooLarge;  // can never fit one segment

    // Rotation decision is made on the projected PHYSICAL end of segment.
    if (opt_.direct_io) {
        const uint64_t end = physical_ + staged_ + rec;
        if (align_up_4k(end) > opt_.segment_limit) {
            const WalStatus s = rotate();
            if (s != WalStatus::Ok) return s;
        }
    } else if (logical_ + rec > opt_.segment_limit) {
        const WalStatus s = rotate();
        if (s != WalStatus::Ok) return s;
    }

    if (opt_.direct_io) {
        WalStatus s = ensure_stage_capacity(staged_ + rec + kWalBlockSize);
        if (s != WalStatus::Ok) return s;
        wal_encode_entry(stage_ + staged_, seq, timestamp_ns, type, payload,
                         payload_len);
        staged_ += rec;
        logical_ += rec;  // stream bytes only — pads live in physical_
    } else {
        WalStatus s = ensure_map_capacity(logical_ + rec);
        if (s != WalStatus::Ok) return s;
        wal_encode_entry(map_ + logical_, seq, timestamp_ns, type, payload,
                         payload_len);
        logical_ += rec;
        physical_ = logical_;
    }

    next_seq_.store(seq + 1, std::memory_order_relaxed);
    ++pending_events_;
    ++appends_;

    // Batch flush: flush_events pending OR flush_interval_ns elapsed.
    const uint64_t now = steady_ns();
    if (pending_events_ >= opt_.flush_events ||
        now - last_flush_ns_ >= opt_.flush_interval_ns)
        return flush();
    return WalStatus::Ok;
}

WalStatus Wal::flush() {
    if (!open_) return WalStatus::NotOpen;
    last_flush_ns_ = steady_ns();

    if (opt_.direct_io) {
        const WalStatus s = flush_direct();
        if (s != WalStatus::Ok) return s;
    }
    if (fd_ >= 0 && ::fsync(fd_) != 0) {
        const WalStatus s = errno_status(errno);
        return note_errno(s);
    }
    pending_events_ = 0;
    ++flushes_;
    return WalStatus::Ok;
}

// O_DIRECT flush: pad the staged batch to the next 4KB boundary (sentinel +
// zeros), then pwrite the whole block range. physical_ is always 4KB-aligned
// here by construction (init 0, recovery align_down, post-flush advance).
WalStatus Wal::flush_direct() {
    if (staged_ == 0) return WalStatus::Ok;
    const uint64_t pad = align_up_4k(staged_) - staged_;
    const uint64_t total = staged_ + pad;
    if (pad != 0) wal_emit_pad(stage_ + staged_, pad);

    uint64_t done = 0;
    while (done < total) {
        const ssize_t w =
            ::pwrite(fd_, stage_ + done, total - done,
                     static_cast<off_t>(physical_ + done));
        if (w < 0) {
            if (errno == EINTR) continue;
            const WalStatus s = errno_status(errno);
            // Staged bytes are kept: a retry rewrites the same block range
            // (pwrite is idempotent at a fixed offset).
            return note_errno(s);
        }
        if (w == 0) {
            last_errno_ = ENOSPC;  // defensive: regular-file write of 0 bytes
            return WalStatus::NoSpace;
        }
        done += static_cast<uint64_t>(w);
    }
    physical_ += total;
    pads_written_ += pad;
    staged_ = 0;
    return WalStatus::Ok;
}

WalStatus Wal::rotate() {
    if (!open_) return WalStatus::NotOpen;
    WalStatus s = flush();
    if (s != WalStatus::Ok) return s;

    const std::filesystem::path old_path(path_);
    const std::filesystem::path dir = old_path.parent_path();
    const std::string next = (dir / (std::to_string(next_seq_.load(
                                         std::memory_order_relaxed)) +
                                     ".wal"))
                                 .string();

    // Seal the current segment: dirty pages already fsynced by flush().
    if (map_ != nullptr) {
        ::munmap(map_, mapped_);
        map_ = nullptr;
        mapped_ = 0;
    }
    if (fd_ >= 0) {
        ::close(fd_);
        fd_ = -1;
    }
    pending_archive_.push_back(path_);
    path_ = next;

    if (opt_.direct_io) {
        fd_ = ::open(path_.c_str(), O_WRONLY | O_CREAT | O_TRUNC | O_CLOEXEC |
                                        (direct_ ? O_DIRECT : 0),
                     kOpenMode);
    } else {
        fd_ = ::open(path_.c_str(), O_RDWR | O_CREAT | O_TRUNC | O_CLOEXEC,
                     kOpenMode);
    }
    if (fd_ < 0) {
        const WalStatus st = errno_status(errno);
        return note_errno(st);
    }
    ++rotations_;
    s = init_fresh();  // seq counter intentionally preserved across segments
    if (s != WalStatus::Ok) return s;
    fsync_dir(dir.empty() ? "." : dir.string());
    return WalStatus::Ok;
}

void Wal::fsync_dir(const std::string& dir) noexcept {
    const int d = ::open(dir.c_str(), O_RDONLY | O_DIRECTORY | O_CLOEXEC);
    if (d < 0) return;
    ::fsync(d);
    ::close(d);
}

void Wal::close() noexcept {
    if (!open_) return;
    (void)flush();  // best effort — a failed final flush is a durability
                    // alarm, not something a destructor can propagate
    if (map_ != nullptr) {
        ::munmap(map_, mapped_);
        map_ = nullptr;
        mapped_ = 0;
    }
    if (fd_ >= 0) {
        ::close(fd_);
        fd_ = -1;
    }
    ::free(stage_);
    stage_ = nullptr;
    stage_cap_ = 0;
    staged_ = 0;
    open_ = false;
}

// --- WalReader -----------------------------------------------------------------

WalReader::~WalReader() { close(); }

WalStatus WalReader::open(std::string_view path) {
    close();
    path_ = path;
    fd_ = ::open(path_.c_str(), O_RDONLY | O_CLOEXEC);
    if (fd_ < 0) return errno_status(errno);

    struct stat st {};
    if (::fstat(fd_, &st) != 0 || st.st_size < 0) {
        close();
        return WalStatus::Io;
    }
    size_ = static_cast<uint64_t>(st.st_size);
    if (size_ < sizeof(WalFileHeader)) {
        close();
        return WalStatus::BadHeader;
    }
    map_ = map_ro(fd_, size_);
    if (map_ == nullptr) {
        close();
        return WalStatus::Io;
    }
    WalFileHeader h;
    std::memcpy(&h, map_, sizeof(h));
    magic_ = h.magic;
    version_ = h.version;
    hdr_shard_ = h.shard_id;
    pos_ = sizeof(WalFileHeader);
    entries_ = 0;
    last_seq_ = 0;
    corrupt_ = false;
    if (magic_ != kWalMagic || version_ != kWalVersion) {
        close();
        return WalStatus::BadHeader;
    }
    return WalStatus::Ok;
}

WalScanStep WalReader::next(WalEntryView& out) noexcept {
    if (map_ == nullptr) return WalScanStep::Corrupt;
    for (;;) {
        const WalScanStep s = wal_scan_step(map_, size_, &pos_, &out);
        if (s == WalScanStep::Pad) continue;
        if (s == WalScanStep::Entry) {
            ++entries_;
            last_seq_ = out.seq;
        } else if (s == WalScanStep::Corrupt) {
            corrupt_ = true;
        }
        return s;
    }
}

void WalReader::close() noexcept {
    if (map_ != nullptr) {
        ::munmap(const_cast<uint8_t*>(map_), size_);
        map_ = nullptr;
    }
    if (fd_ >= 0) {
        ::close(fd_);
        fd_ = -1;
    }
    size_ = 0;
    pos_ = sizeof(WalFileHeader);
}

}  // namespace exch
