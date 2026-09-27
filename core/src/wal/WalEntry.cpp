// Task 1.3.6 — WAL entry (de)serialization, CRC32C (SSE4.2 hw + table sw),
// pad emission, and the scan primitive shared by recovery and replay.

#include "wal/WalEntry.hpp"

#include <array>

namespace exch {
namespace {

// CRC32C (Castagnoli, reflected poly 0x82F63B78) — same polynomial the SSE4.2
// `crc32` instruction implements, so hw and sw paths are interchangeable.
constexpr std::array<uint32_t, 256> make_crc32c_table() {
    std::array<uint32_t, 256> t{};
    for (uint32_t i = 0; i < 256; ++i) {
        uint32_t c = i;
        for (int k = 0; k < 8; ++k)
            c = (c & 1u) ? (0x82F63B78u ^ (c >> 1)) : (c >> 1);
        t[i] = c;
    }
    return t;
}
constexpr auto kCrc32cTable = make_crc32c_table();

constexpr uint32_t kCrcInit = 0xFFFFFFFFu;

#if defined(__x86_64__) || defined(__i386__)
#define EXCH_WAL_X86 1
#endif

// Runtime dispatch selected once: SSE4.2 `crc32` instruction when the CPU
// advertises it (cpuid), software table otherwise.
const bool g_crc32c_hw =
#ifdef EXCH_WAL_X86
    __builtin_cpu_supports("sse4.2") != 0;
#else
    false;
#endif

#ifdef EXCH_WAL_X86
// target() attribute lets this compile with the builtins even when the TU is
// not built with -msse4.2 (Debug config has no -march=native).
__attribute__((target("sse4.2"), noinline))
uint32_t crc32c_hw(uint32_t crc, const uint8_t* p, std::size_t n) noexcept {
    while (n >= 8) {
        crc = static_cast<uint32_t>(
            __builtin_ia32_crc32di(crc, wal_load64(p)));
        p += 8;
        n -= 8;
    }
    if (n >= 4) {
        crc = __builtin_ia32_crc32si(crc, wal_load32(p));
        p += 4;
        n -= 4;
    }
    if (n >= 2) {
        uint16_t v;
        __builtin_memcpy(&v, p, sizeof(v));
        crc = __builtin_ia32_crc32hi(crc, v);
        p += 2;
        n -= 2;
    }
    if (n >= 1) {
        crc = __builtin_ia32_crc32qi(crc, *p);
    }
    return crc;
}
#endif

uint32_t crc32c_sw_impl(uint32_t crc, const uint8_t* p, std::size_t n) noexcept {
    for (std::size_t i = 0; i < n; ++i)
        crc = kCrc32cTable[(crc ^ p[i]) & 0xFFu] ^ (crc >> 8);
    return crc;
}

inline uint32_t crc32c_dispatch(uint32_t crc, const uint8_t* p,
                                std::size_t n) noexcept {
#ifdef EXCH_WAL_X86
    if (g_crc32c_hw) return crc32c_hw(crc, p, n);
#endif
    return crc32c_sw_impl(crc, p, n);
}

// Count of consecutive zero bytes at p (bounded by n). u64-strided for speed on
// the long zero tail of preallocated segments.
uint64_t zero_run(const uint8_t* p, uint64_t n) noexcept {
    uint64_t i = 0;
    while (i + 8 <= n) {
        if (wal_load64(p + i) != 0) break;
        i += 8;
    }
    while (i < n && p[i] == 0) ++i;
    return i;
}

inline uint64_t align_up_4k(uint64_t v) noexcept {
    return (v + (kWalBlockSize - 1)) & ~(kWalBlockSize - 1);
}

}  // namespace

// --- CRC32C ------------------------------------------------------------------

bool wal_crc32c_hardware() noexcept { return g_crc32c_hw; }

uint32_t wal_crc32c_continue(uint32_t crc, const void* data,
                             std::size_t len) noexcept {
    // crc is transported un-complemented so callers can chain.
    return crc32c_dispatch(crc ^ kCrcInit,
                           static_cast<const uint8_t*>(data), len) ^
           kCrcInit;
}

uint32_t wal_crc32c(const void* data, std::size_t len) noexcept {
    return wal_crc32c_continue(0, data, len);
}

uint32_t wal_crc32c_sw(const void* data, std::size_t len) noexcept {
    return crc32c_sw_impl(kCrcInit, static_cast<const uint8_t*>(data), len) ^
           kCrcInit;
}

// --- Encoding -----------------------------------------------------------------

uint64_t wal_encode_entry(uint8_t* dst, uint64_t seq, uint64_t timestamp_ns,
                          WalEventType type, const void* payload,
                          uint32_t payload_len) noexcept {
    WalEntryHeader h{seq, timestamp_ns, static_cast<uint8_t>(type), payload_len};
    std::memcpy(dst, &h, sizeof(h));
    if (payload_len != 0) std::memcpy(dst + sizeof(h), payload, payload_len);
    const uint32_t crc = wal_crc32c(dst, sizeof(h) + payload_len);
    const uint64_t total = sizeof(h) + payload_len;
    std::memcpy(dst + total, &crc, sizeof(crc));
    return total + sizeof(crc);
}

void wal_emit_pad(uint8_t* dst, uint64_t pad_bytes) noexcept {
    if (pad_bytes >= sizeof(kWalPadSeq)) {
        std::memcpy(dst, &kWalPadSeq, sizeof(kWalPadSeq));
        std::memset(dst + sizeof(kWalPadSeq), 0,
                    static_cast<std::size_t>(pad_bytes - sizeof(kWalPadSeq)));
    } else {
        std::memset(dst, 0, static_cast<std::size_t>(pad_bytes));
    }
}

// --- Scanner ------------------------------------------------------------------
//
// Record classification at pos (all comparisons bounded by `size`):
//   1. u64 at pos == kWalPadSeq      -> strict pad: zeros must fill to the next
//                                     4KB boundary and the boundary must be in
//                                     the file, else Corrupt (torn pad).
//   2. 21 bytes fit and the header is plausible (payload_len bounded, record
//      inside the file) and the stored crc32c matches -> Entry.
//   3. Otherwise classify the zero run z starting at pos:
//        z reaches EOF                       -> End (untouched prealloc tail)
//        z ends exactly at the 4KB boundary  -> Pad (short zero pad, 1..7 B)
//        otherwise                           -> Corrupt (garbage or torn write)
WalScanStep wal_scan_step(const uint8_t* base, uint64_t size, uint64_t* pos,
                          WalEntryView* out) noexcept {
    const uint64_t p = *pos;
    const uint64_t avail = size - p;
    if (avail == 0) return WalScanStep::End;

    if (avail >= sizeof(uint64_t) && wal_load64(base + p) == kWalPadSeq) {
        const uint64_t end = align_up_4k(p);
        if (end > size) return WalScanStep::Corrupt;
        if (zero_run(base + p + sizeof(uint64_t),
                     end - p - sizeof(uint64_t)) !=
            end - p - sizeof(uint64_t))
            return WalScanStep::Corrupt;
        *pos = end;
        return WalScanStep::Pad;
    }

    if (avail >= sizeof(WalEntryHeader)) {
        WalEntryHeader h;
        std::memcpy(&h, base + p, sizeof(h));
        const uint64_t rec = sizeof(h) + h.payload_len + sizeof(uint32_t);
        if (h.payload_len <= kWalMaxPayload && rec <= avail) {
            const uint32_t stored = wal_load32(base + p + sizeof(h) + h.payload_len);
            if (wal_crc32c(base + p, sizeof(h) + h.payload_len) == stored) {
                if (out != nullptr) {
                    out->seq = h.seq;
                    out->timestamp_ns = h.timestamp_ns;
                    out->type = static_cast<WalEventType>(h.event_type);
                    out->payload = base + p + sizeof(h);
                    out->payload_len = h.payload_len;
                    out->offset = p;
                    out->record_bytes = rec;
                }
                *pos = p + rec;
                return WalScanStep::Entry;
            }
        }
    }

    const uint64_t z = zero_run(base + p, avail);
    if (z == avail) return WalScanStep::End;
    const uint64_t to_boundary =
        align_up_4k(p) - p;  // 0 when p is already on a boundary
    // A legitimate pad ends AT its block boundary; a zero run shorter or one
    // that spills past the boundary means unwritten gap followed by data —
    // impossible in a well-formed segment -> Corrupt.
    if (to_boundary > 0 && z == to_boundary) {
        *pos = p + z;
        return WalScanStep::Pad;
    }
    return WalScanStep::Corrupt;
}

WalScanResult wal_scan(const uint8_t* base, uint64_t size) noexcept {
    WalScanResult r;
    if (size < sizeof(WalFileHeader)) return r;  // header_ok=false, valid_end=8
    WalFileHeader fh;
    std::memcpy(&fh, base, sizeof(fh));
    if (fh.magic != kWalMagic || fh.version != kWalVersion) return r;
    r.header_ok = true;
    r.header_version = fh.version;
    r.header_shard = fh.shard_id;

    uint64_t pos = sizeof(WalFileHeader);
    WalEntryView v;
    for (;;) {
        const uint64_t before = pos;
        const WalScanStep s = wal_scan_step(base, size, &pos, &v);
        if (s == WalScanStep::Entry) {
            ++r.entries;
            r.last_seq = v.seq;
            r.entry_bytes += v.record_bytes;
        } else if (s == WalScanStep::Pad) {
            ++r.pads;
            r.pad_bytes += pos - before;
        } else {
            r.corrupt = (s == WalScanStep::Corrupt);
            break;  // End or Corrupt: pos is the valid tail either way
        }
    }
    r.valid_end = pos;
    return r;
}

}  // namespace exch
