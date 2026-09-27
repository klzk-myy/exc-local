// Task 1.3.6 coverage: wire constants, header dump, 1000-entry round trip with
// CRC32C verification, O_DIRECT aligned block flushing, 1GB rotation (shrunken
// threshold), torn-tail crash recovery, and clean ENOSPC failure.

#include <gtest/gtest.h>

#include <algorithm>
#include <cerrno>
#include <cstdint>
#include <cstdio>
#include <cstring>
#include <filesystem>
#include <fstream>
#include <string>
#include <vector>

#include <fcntl.h>
#include <signal.h>
#include <sys/resource.h>
#include <sys/stat.h>
#include <unistd.h>

#include "utils/TimeUtils.hpp"
#include "wal/Wal.hpp"
#include "wal/WalEntry.hpp"

using namespace exch;

namespace {

std::filesystem::path tmp_dir(const char* name) {
    const auto d = std::filesystem::temp_directory_path() /
                   ("exch_wal_" + std::to_string(::getpid()) + "_" + name);
    std::filesystem::remove_all(d);
    std::filesystem::create_directories(d);
    return d;
}

// Deterministic payload: byte i of record n = (n*31 + i) & 0xFF.
std::vector<uint8_t> payload_for(uint64_t n, uint32_t len) {
    std::vector<uint8_t> v(len);
    for (uint32_t i = 0; i < len; ++i)
        v[i] = static_cast<uint8_t>((n * 31 + i) & 0xFF);
    return v;
}

uint64_t file_size(const std::string& p) {
    struct stat st {};
    return ::stat(p.c_str(), &st) == 0 ? static_cast<uint64_t>(st.st_size) : 0;
}

// Drains a reader; returns steps seen at stop (End or Corrupt).
WalScanStep drain(WalReader& r, std::vector<WalEntryView>& out,
                  uint32_t expected_type_count = 0) {
    WalEntryView v;
    for (;;) {
        const WalScanStep s = r.next(v);
        if (s == WalScanStep::Entry) {
            EXPECT_GT(v.record_bytes, 0u);
            // CRC was verified by the scanner — additionally verify payload
            // integrity bytes for self-checking tests.
            out.push_back(v);
            if (expected_type_count) {
                // payload self-check: stored bytes must match generator
                EXPECT_EQ(v.payload_len,
                          payload_for(v.seq, v.payload_len).size());
                EXPECT_EQ(std::memcmp(v.payload,
                                      payload_for(v.seq, v.payload_len).data(),
                                      v.payload_len),
                          0);
            }
        } else {
            return s;
        }
    }
}

}  // namespace

// --- Existing smoke coverage (kept) ------------------------------------------

TEST(WalSmoke, WireFormatConstants) {
    EXPECT_EQ(kWalMagic, 0x57414C00u);
    EXPECT_EQ(kWalVersion, 1u);
    static_assert(sizeof(WalFileHeader) == 8);
    static_assert(sizeof(WalEntryHeader) == 21);
    SUCCEED();
}

TEST(WalSmoke, EventTypesPresent) {
    EXPECT_EQ(static_cast<uint8_t>(WalEventType::ORDER_NEW), 0);
    EXPECT_EQ(static_cast<uint8_t>(WalEventType::TIME_TICK), 4);
    EXPECT_EQ(static_cast<uint8_t>(WalEventType::BOOK_SNAPSHOT), 5);
}

TEST(WalSmoke, ConstructNotOpen) {
    Wal wal("var/wal/shard2.wal", 2);
    EXPECT_EQ(wal.shard_id(), 2u);
    EXPECT_FALSE(wal.is_open());
    EXPECT_EQ(wal.tail_seq(), 0u);
    EXPECT_EQ(wal.path(), "var/wal/shard2.wal");
}

// --- CRC32C --------------------------------------------------------------------

TEST(WalCrc32c, KnownVectorAndDispatch) {
    // CRC32C("123456789") = 0xE3069283 (Castagnoli check value).
    const char* check = "123456789";
    EXPECT_EQ(wal_crc32c(check, 9), 0xE3069283u);
    EXPECT_EQ(wal_crc32c_sw(check, 9), 0xE3069283u);

    std::vector<uint8_t> blob(8191);
    for (size_t i = 0; i < blob.size(); ++i) blob[i] = uint8_t(i * 7 + 3);
    // hw dispatch and table fallback must agree bit-for-bit.
    EXPECT_EQ(wal_crc32c(blob.data(), blob.size()),
              wal_crc32c_sw(blob.data(), blob.size()));
    // Streaming form equals one-shot.
    const uint32_t a = wal_crc32c_continue(0, blob.data(), 4096);
    const uint32_t b = wal_crc32c_continue(a, blob.data() + 4096, 4095);
    EXPECT_EQ(b, wal_crc32c(blob.data(), blob.size()));
    std::printf("crc32c hardware path: %s\n",
                wal_crc32c_hardware() ? "SSE4.2" : "software");
}

// --- DoD 1: header -------------------------------------------------------------

TEST(WalHeader, CreatedWithMagicVersionShard) {
    const auto dir = tmp_dir("hdr");
    const std::string p = (dir / "0.wal").string();
    {
        Wal w(p, 7);
        ASSERT_EQ(w.open(), WalStatus::Ok);
        const auto pay = payload_for(0, 16);
        ASSERT_EQ(w.append(WalEventType::ORDER_NEW, pay.data(), pay.size()),
                  WalStatus::Ok);
        ASSERT_EQ(w.flush(), WalStatus::Ok);
    }
    // Raw bytes: magic u32, version u16, shard u16 packed in 8 bytes.
    std::ifstream f(p, std::ios::binary);
    WalFileHeader h{};
    ASSERT_TRUE(f.read(reinterpret_cast<char*>(&h), sizeof(h)));
    EXPECT_EQ(h.magic, kWalMagic);
    EXPECT_EQ(h.version, kWalVersion);
    EXPECT_EQ(h.shard_id, 7u);

    WalReader r;
    ASSERT_EQ(r.open(p), WalStatus::Ok);
    EXPECT_EQ(r.magic(), kWalMagic);
    EXPECT_EQ(r.version(), kWalVersion);
    EXPECT_EQ(r.shard_id(), 7u);
    std::printf("header dump: magic=0x%08x version=%u shard=%u\n", r.magic(),
                r.version(), r.shard_id());
}

// --- DoD 2: 1000 entries, fsync, all CRC32 verified ------------------------------

TEST(WalRoundTrip, ThousandEntriesAllCrcVerified) {
    const auto dir = tmp_dir("rt1k");
    const std::string p = (dir / "0.wal").string();
    {
        Wal w(p, 3);
        ASSERT_EQ(w.open(), WalStatus::Ok);
        for (uint64_t i = 0; i < 1000; ++i) {
            const auto pay = payload_for(i, 24 + (i % 40));
            ASSERT_EQ(w.append(WalEventType::ORDER_NEW, pay.data(),
                               static_cast<uint32_t>(pay.size())),
                      WalStatus::Ok)
                << "append " << i;
        }
        ASSERT_EQ(w.flush(), WalStatus::Ok);
        EXPECT_EQ(w.tail_seq(), 1000u);
        EXPECT_GE(w.flushes(), 10u);  // 100-event batches forced auto-flush
    }
    WalReader r;
    ASSERT_EQ(r.open(p), WalStatus::Ok);
    std::vector<WalEntryView> got;
    ASSERT_EQ(drain(r, got, 1), WalScanStep::End);
    ASSERT_EQ(got.size(), 1000u);
    for (uint64_t i = 0; i < 1000; ++i) EXPECT_EQ(got[i].seq, i);
    EXPECT_FALSE(r.corrupt_seen());
    EXPECT_EQ(r.last_seq(), 999u);
}

TEST(WalBatchFlush, EventAndTimeTriggers) {
    const auto dir = tmp_dir("batch");
    // Count trigger: 100 pending events forces a flush on the 100th append.
    {
        WalOptions o;
        o.flush_interval_ns = UINT64_MAX;  // disable time trigger
        Wal w((dir / "count.wal").string(), 0, o);
        ASSERT_EQ(w.open(), WalStatus::Ok);
        const auto pay = payload_for(0, 8);
        for (int i = 0; i < 99; ++i)
            ASSERT_EQ(w.append(WalEventType::TRADE, pay.data(), 8),
                      WalStatus::Ok);
        EXPECT_EQ(w.flushes(), 0u);
        ASSERT_EQ(w.append(WalEventType::TRADE, pay.data(), 8), WalStatus::Ok);
        EXPECT_GE(w.flushes(), 1u);
    }
    // Time trigger: tiny interval flushes on the next append.
    {
        WalOptions o;
        o.flush_interval_ns = 1;
        o.flush_events = 1'000'000;
        Wal w((dir / "time.wal").string(), 0, o);
        ASSERT_EQ(w.open(), WalStatus::Ok);
        const auto pay = payload_for(0, 8);
        ASSERT_EQ(w.append(WalEventType::TRADE, pay.data(), 8), WalStatus::Ok);
        ASSERT_EQ(w.append(WalEventType::TRADE, pay.data(), 8), WalStatus::Ok);
        EXPECT_GE(w.flushes(), 1u);
    }
}

// --- DoD 3: O_DIRECT aligned block flushing --------------------------------------

TEST(WalDirect, AlignedBlockFlushing) {
    const auto dir = tmp_dir("odirect");
    const std::string p = (dir / "0.wal").string();
    WalOptions o;
    o.direct_io = true;
    o.segment_limit = 4ull << 20;
    uint64_t expect_entries = 0;
    {
        Wal w(p, 5, o);
        ASSERT_EQ(w.open(), WalStatus::Ok) << "errno=" << w.last_errno();
        // Structural proof of page-cache-bypass intent: fd was opened with
        // O_DIRECT (no EINVAL), buffers are posix_memalign 4KB-aligned, and
        // every flushed write is offset/length 4KB-aligned.
        EXPECT_TRUE(w.using_direct_io() || w.direct_fallback());
        if (w.direct_fallback())
            std::printf("O_DIRECT unsupported here — staged fallback path\n");
        for (uint64_t i = 0; i < 300; ++i) {
            const auto pay = payload_for(i, 40);
            ASSERT_EQ(w.append(WalEventType::ORDER_NEW, pay.data(), 40),
                      WalStatus::Ok);
            ++expect_entries;
        }
        ASSERT_EQ(w.flush(), WalStatus::Ok);
        // After a flush the physical write offset is always a 4KB multiple and
        // strictly ahead of the logical stream offset by the pad bytes.
        EXPECT_EQ(w.physical_offset() % kWalBlockSize, 0u);
        EXPECT_GT(w.physical_offset(), w.logical_offset());
    }
    // File size is a whole number of 4KB blocks (padded batches).
    const uint64_t sz = file_size(p);
    ASSERT_EQ(sz % kWalBlockSize, 0u);
    ASSERT_GT(sz, 0u);

    WalReader r;
    ASSERT_EQ(r.open(p), WalStatus::Ok);
    std::vector<WalEntryView> got;
    ASSERT_EQ(drain(r, got, 1), WalScanStep::End);
    ASSERT_EQ(got.size(), expect_entries);
    for (uint64_t i = 0; i < got.size(); ++i) EXPECT_EQ(got[i].seq, i);
}

// Pad shorter than the 8-byte sentinel => zeros-only pad form still parses.
TEST(WalDirect, ShortZeroPad) {
    const auto dir = tmp_dir("shortpad");
    const std::string p = (dir / "0.wal").string();
    WalOptions o;
    o.direct_io = true;
    // staged = 8 (header) + (25 + 4056) = 4089 -> pad = 7 bytes, < sentinel.
    {
        Wal w(p, 1, o);
        ASSERT_EQ(w.open(), WalStatus::Ok);
        const auto pay = payload_for(0, 4056);
        ASSERT_EQ(w.append(WalEventType::TRADE, pay.data(),
                           static_cast<uint32_t>(pay.size())),
                  WalStatus::Ok);
        ASSERT_EQ(w.flush(), WalStatus::Ok);
        EXPECT_EQ(w.physical_offset(), kWalBlockSize);  // exactly one block
        EXPECT_EQ(w.logical_offset(), 8u + 25u + 4056u);
    }
    EXPECT_EQ(file_size(p), kWalBlockSize);
    WalReader r;
    ASSERT_EQ(r.open(p), WalStatus::Ok);
    std::vector<WalEntryView> got;
    ASSERT_EQ(drain(r, got, 1), WalScanStep::End);
    ASSERT_EQ(got.size(), 1u);
    EXPECT_EQ(got[0].seq, 0u);
}

// --- DoD 4: rotation -------------------------------------------------------------

TEST(WalRotation, RotatesAtLimitAndQueuesArchive) {
    const auto dir = tmp_dir("rot");
    // Layout wal/{shard}/{seq_base}.wal — shard dir "2".
    const auto shard_dir = dir / "2";
    const std::string p = (shard_dir / "0.wal").string();
    WalOptions o;
    o.segment_limit = 32 * kWalBlockSize;  // 128 KiB stands in for 1 GiB
    std::string first_seg;
    uint64_t total = 0;
    {
        Wal w(p, 2, o);
        ASSERT_EQ(w.open(), WalStatus::Ok);
        const auto pay = payload_for(0, 900);
        for (int i = 0; i < 400; ++i)
            ASSERT_EQ(w.append(WalEventType::ORDER_NEW, pay.data(), 900),
                      WalStatus::Ok);
        ASSERT_EQ(w.flush(), WalStatus::Ok);
        EXPECT_GE(w.rotations(), 1u);
        first_seg = w.pending_archive().front();
        total = w.tail_seq();
    }
    // Old segment sealed and listed for S3 archive; the sealed file is the
    // initial segment (named "0.wal" here — seq_base of the first segment).
    EXPECT_TRUE(std::filesystem::exists(first_seg));
    EXPECT_EQ(first_seg, p);
    // Every sealed segment parses clean; seq space is continuous across files.
    WalReader r0;
    ASSERT_EQ(r0.open(first_seg), WalStatus::Ok);
    std::vector<WalEntryView> seg0;
    ASSERT_EQ(drain(r0, seg0), WalScanStep::End);
    ASSERT_GT(seg0.size(), 0u);
    EXPECT_EQ(seg0.back().seq + 1, seg0.size());  // dense inside segment

    // Remaining segments found in the shard dir continue the seq stream —
    // ordered by numeric seq_base filename (directory order is arbitrary).
    std::vector<std::string> rest;
    for (const auto& de : std::filesystem::directory_iterator(shard_dir)) {
        const auto f = de.path().string();
        if (f == first_seg || de.path().extension() != ".wal") continue;
        rest.push_back(f);
    }
    std::sort(rest.begin(), rest.end(), [](const std::string& a,
                                           const std::string& b) {
        return std::stoull(std::filesystem::path(a).stem()) <
               std::stoull(std::filesystem::path(b).stem());
    });
    uint64_t seen = seg0.size();
    for (const auto& f : rest) {
        WalReader r;
        ASSERT_EQ(r.open(f), WalStatus::Ok) << f;
        std::vector<WalEntryView> seg;
        ASSERT_EQ(drain(r, seg), WalScanStep::End) << f;
        if (!seg.empty()) {
            EXPECT_EQ(seg.front().seq, seen) << f;
        }
        seen += seg.size();
    }
    EXPECT_EQ(seen, total);
}

TEST(WalRotation, ExplicitRotatePreservesSequence) {
    const auto dir = tmp_dir("rotexp");
    const std::string p = (dir / "0.wal").string();
    {
        Wal w(p, 9);
        ASSERT_EQ(w.open(), WalStatus::Ok);
        const auto pay = payload_for(0, 32);
        for (int i = 0; i < 5; ++i)
            ASSERT_EQ(w.append(WalEventType::TRADE, pay.data(), 32),
                      WalStatus::Ok);
        ASSERT_EQ(w.rotate(), WalStatus::Ok);          // explicit, pre-limit
        EXPECT_EQ(w.tail_seq(), 5u);                   // seq must NOT reset
        for (int i = 5; i < 8; ++i)
            ASSERT_EQ(w.append(WalEventType::TRADE, pay.data(), 32),
                      WalStatus::Ok);
        ASSERT_EQ(w.flush(), WalStatus::Ok);
        EXPECT_EQ(w.tail_seq(), 8u);
        ASSERT_EQ(w.pending_archive().size(), 1u);
    }
    // New segment is named by seq_base of its first entry.
    WalReader r;
    ASSERT_EQ(r.open((dir / "5.wal").string()), WalStatus::Ok);
    std::vector<WalEntryView> got;
    ASSERT_EQ(drain(r, got), WalScanStep::End);
    ASSERT_EQ(got.size(), 3u);
    EXPECT_EQ(got.front().seq, 5u);
}

TEST(WalRotation, DirectModeToo) {
    const auto dir = tmp_dir("rotd");
    const std::string p = (dir / "0.wal").string();
    WalOptions o;
    o.direct_io = true;
    o.segment_limit = 16 * kWalBlockSize;  // 64 KiB
    {
        Wal w(p, 0, o);
        ASSERT_EQ(w.open(), WalStatus::Ok);
        const auto pay = payload_for(0, 700);
        for (int i = 0; i < 300; ++i)
            ASSERT_EQ(w.append(WalEventType::TRADE, pay.data(), 700),
                      WalStatus::Ok);
        EXPECT_GE(w.rotations(), 1u);
        EXPECT_FALSE(w.pending_archive().empty());
    }
    // Count segment files on disk: rotation must have produced >= 2.
    unsigned segments = 0;
    for (const auto& de : std::filesystem::directory_iterator(dir))
        if (de.path().extension() == ".wal") ++segments;
    EXPECT_GE(segments, 2u);
}

// --- DoD 5: crash simulation — torn tail detect + truncate -----------------------

TEST(WalCrashRecovery, TornTailTruncatesAndAppendsResume) {
    const auto dir = tmp_dir("crash");
    const std::string p = (dir / "0.wal").string();
    uint64_t logical_end = 0;
    {
        Wal w(p, 4);
        ASSERT_EQ(w.open(), WalStatus::Ok);
        for (uint64_t i = 0; i < 10; ++i) {
            const auto pay = payload_for(i, 30);
            ASSERT_EQ(w.append(WalEventType::ORDER_CANCEL, pay.data(), 30),
                      WalStatus::Ok);
        }
        ASSERT_EQ(w.flush(), WalStatus::Ok);
        logical_end = w.logical_offset();
        EXPECT_EQ(w.tail_seq(), 10u);
    }
    // Simulate kill mid-write: half a header (10 of 21 bytes) lands after the
    // last committed entry — a torn record, not a full one.
    {
        const int fd = ::open(p.c_str(), O_WRONLY);
        ASSERT_GE(fd, 0);
        const uint8_t junk[10] = {0xDE, 0xAD, 0xBE, 0xEF, 0x10,
                                  0x20, 0x30, 0x40, 0x50, 0x60};
        ASSERT_EQ(::pwrite(fd, junk, sizeof(junk),
                           static_cast<off_t>(logical_end)),
                  static_cast<ssize_t>(sizeof(junk)));
        ::fsync(fd);
        ::close(fd);
    }
    // Reopen: reader detects the partial tail and truncates at the last valid
    // offset; appends continue after the truncated seq.
    {
        Wal w(p, 4);
        ASSERT_EQ(w.open(), WalStatus::Ok);
        EXPECT_TRUE(w.last_scan().corrupt);
        EXPECT_EQ(w.last_scan().valid_end, logical_end);
        EXPECT_EQ(w.tail_seq(), 10u);
        for (uint64_t i = 10; i < 15; ++i) {
            const auto pay = payload_for(i, 30);
            ASSERT_EQ(w.append(WalEventType::ORDER_NEW, pay.data(), 30),
                      WalStatus::Ok);
        }
        ASSERT_EQ(w.flush(), WalStatus::Ok);
        EXPECT_EQ(w.tail_seq(), 15u);
    }
    WalReader r;
    ASSERT_EQ(r.open(p), WalStatus::Ok);
    std::vector<WalEntryView> got;
    ASSERT_EQ(drain(r, got, 1), WalScanStep::End);
    ASSERT_EQ(got.size(), 15u);
    for (uint64_t i = 0; i < 15; ++i) EXPECT_EQ(got[i].seq, i);
}

TEST(WalCrashRecovery, CorruptCrcTruncatesLastEntry) {
    const auto dir = tmp_dir("crc");
    const std::string p = (dir / "0.wal").string();
    uint64_t entry9_off = 0;
    {
        Wal w(p, 4);
        ASSERT_EQ(w.open(), WalStatus::Ok);
        for (uint64_t i = 0; i < 10; ++i) {
            const auto pay = payload_for(i, 30);
            ASSERT_EQ(w.append(WalEventType::ORDER_NEW, pay.data(), 30),
                      WalStatus::Ok);
        }
        ASSERT_EQ(w.flush(), WalStatus::Ok);
        entry9_off = 8 + 9 * (25 + 30);  // start of seq 9 record
    }
    // Bit-flip inside entry 9's payload -> CRC mismatch on scan.
    {
        const int fd = ::open(p.c_str(), O_RDWR);
        ASSERT_GE(fd, 0);
        uint8_t b = 0;
        ASSERT_EQ(::pread(fd, &b, 1, static_cast<off_t>(entry9_off + 22)), 1);
        b ^= 0xFF;
        ASSERT_EQ(::pwrite(fd, &b, 1, static_cast<off_t>(entry9_off + 22)), 1);
        ::fsync(fd);
        ::close(fd);
    }
    Wal w(p, 4);
    ASSERT_EQ(w.open(), WalStatus::Ok);
    EXPECT_TRUE(w.last_scan().corrupt);
    EXPECT_EQ(w.last_scan().entries, 9u);
    EXPECT_EQ(w.last_scan().valid_end, entry9_off);
    EXPECT_EQ(w.tail_seq(), 9u);  // resume right after last valid seq
    w.close();
}

// Direct-mode torn tail: surviving partial last block is lifted back into the
// staging buffer and the file is cut to its containing 4KB boundary, so the
// next aligned pwrite stays legal.
TEST(WalCrashRecovery, DirectModeTornTail) {
    const auto dir = tmp_dir("crashd");
    const std::string p = (dir / "0.wal").string();
    WalOptions o;
    o.direct_io = true;
    uint64_t logical_end = 0;
    {
        Wal w(p, 6, o);
        ASSERT_EQ(w.open(), WalStatus::Ok);
        for (uint64_t i = 0; i < 12; ++i) {
            const auto pay = payload_for(i, 100);
            ASSERT_EQ(w.append(WalEventType::ORDER_MODIFY, pay.data(), 100),
                      WalStatus::Ok);
        }
        ASSERT_EQ(w.flush(), WalStatus::Ok);
        logical_end = w.logical_offset();
    }
    // Torn final block: garbage bytes land after the committed stream.
    {
        const int fd = ::open(p.c_str(), O_WRONLY);
        ASSERT_GE(fd, 0);
        std::vector<uint8_t> junk(300);
        for (size_t i = 0; i < junk.size(); ++i) junk[i] = uint8_t(0xA5 ^ i);
        ASSERT_EQ(::pwrite(fd, junk.data(), junk.size(),
                           static_cast<off_t>(logical_end)),
                  static_cast<ssize_t>(junk.size()));
        ::fsync(fd);
        ::close(fd);
    }
    {
        Wal w(p, 6, o);
        ASSERT_EQ(w.open(), WalStatus::Ok);
        EXPECT_TRUE(w.last_scan().corrupt);
        EXPECT_EQ(w.last_scan().valid_end, logical_end);
        EXPECT_EQ(w.tail_seq(), 12u);
        // Physical write position realigned to the containing 4KB boundary.
        EXPECT_EQ(w.physical_offset() % kWalBlockSize, 0u);
        for (uint64_t i = 12; i < 15; ++i) {
            const auto pay = payload_for(i, 100);
            ASSERT_EQ(w.append(WalEventType::ORDER_NEW, pay.data(), 100),
                      WalStatus::Ok);
        }
        ASSERT_EQ(w.flush(), WalStatus::Ok);
    }
    WalReader r;
    ASSERT_EQ(r.open(p), WalStatus::Ok);
    std::vector<WalEntryView> got;
    ASSERT_EQ(drain(r, got, 1), WalScanStep::End);
    ASSERT_EQ(got.size(), 15u);
    for (uint64_t i = 0; i < 15; ++i) EXPECT_EQ(got[i].seq, i);
    EXPECT_EQ(file_size(p) % kWalBlockSize, 0u);
}

// Clean reopen (no corruption): scan sees End, writer resumes at tail seq.
TEST(WalRecovery, CleanReopenResumesSequence) {
    for (const bool direct : {false, true}) {
        const auto dir = tmp_dir(direct ? "reopd" : "reopm");
        const std::string p = (dir / "0.wal").string();
        WalOptions o;
        o.direct_io = direct;
        {
            Wal w(p, 1, o);
            ASSERT_EQ(w.open(), WalStatus::Ok);
            for (uint64_t i = 0; i < 20; ++i) {
                const auto pay = payload_for(i, 40);
                ASSERT_EQ(w.append(WalEventType::TRADE, pay.data(), 40),
                          WalStatus::Ok);
            }
            ASSERT_EQ(w.flush(), WalStatus::Ok);
        }
        {
            Wal w(p, 1, o);
            ASSERT_EQ(w.open(), WalStatus::Ok) << "direct=" << direct;
            EXPECT_FALSE(w.last_scan().corrupt);
            EXPECT_EQ(w.last_scan().entries, 20u);
            EXPECT_EQ(w.tail_seq(), 20u);
            for (uint64_t i = 20; i < 23; ++i) {
                const auto pay = payload_for(i, 40);
                ASSERT_EQ(w.append(WalEventType::TRADE, pay.data(), 40),
                          WalStatus::Ok);
            }
            ASSERT_EQ(w.flush(), WalStatus::Ok);
        }
        WalReader r;
        ASSERT_EQ(r.open(p), WalStatus::Ok);
        std::vector<WalEntryView> got;
        ASSERT_EQ(drain(r, got, 1), WalScanStep::End) << "direct=" << direct;
        ASSERT_EQ(got.size(), 23u);
        for (uint64_t i = 0; i < 23; ++i) EXPECT_EQ(got[i].seq, i);
    }
}

TEST(WalExplicitSeq, MonotonicAndRegressionRejected) {
    const auto dir = tmp_dir("seq");
    Wal w((dir / "0.wal").string(), 0);
    ASSERT_EQ(w.open(), WalStatus::Ok);
    const auto pay = payload_for(0, 8);
    ASSERT_EQ(w.append(100, now_ns(), WalEventType::TRADE, pay.data(), 8),
              WalStatus::Ok);
    EXPECT_EQ(w.tail_seq(), 101u);
    EXPECT_EQ(w.append(50, now_ns(), WalEventType::TRADE, pay.data(), 8),
              WalStatus::SeqRegression);
    EXPECT_EQ(w.append(kWalPadSeq, now_ns(), WalEventType::TRADE, pay.data(), 8),
              WalStatus::SeqRegression);
    w.close();
}

// --- Disk full: ENOSPC/EFBIG must fail clean, not corrupt ------------------------

TEST(WalDiskFull, FailsCleanAndStaysRecoverable) {
    const auto dir = tmp_dir("enospc");
    const std::string p = (dir / "0.wal").string();

    const struct sigaction sa_old = [] {
        struct sigaction sa {}, old {};
        sa.sa_handler = SIG_IGN;
        ::sigaction(SIGXFSZ, &sa, &old);
        return old;
    }();
    struct rlimit old_lim {};
    ASSERT_EQ(::getrlimit(RLIMIT_FSIZE, &old_lim), 0);
    // Soft cap only — raising the hard cap back would need CAP_SYS_RESOURCE.
    const struct rlimit small{rlim_t{48} * 1024, old_lim.rlim_max};
    ASSERT_EQ(::setrlimit(RLIMIT_FSIZE, &small), 0);

    uint64_t committed = 0;
    WalStatus last = WalStatus::Ok;
    {
        Wal w(p, 0);
        // open may fail outright under the cap — that is also a clean fail.
        last = w.open();
        if (last == WalStatus::Ok) {
            for (int i = 0; i < 2000; ++i) {
                const auto pay = payload_for(i, 200);
                last = w.append(WalEventType::TRADE, pay.data(), 200);
                if (last != WalStatus::Ok) break;
                ++committed;
            }
            EXPECT_EQ(last, WalStatus::NoSpace)
                << "status=" << wal_status_str(last)
                << " errno=" << w.last_errno();
            EXPECT_EQ(w.tail_seq(), committed);  // nothing half-committed
        }
    }
    ASSERT_EQ(::setrlimit(RLIMIT_FSIZE, &old_lim), 0);
    ::signal(SIGXFSZ, sa_old.sa_handler);

    // Space restored: file must still be a readable WAL — the failure never
    // produced a half-written record.
    WalReader r;
    ASSERT_EQ(r.open(p), WalStatus::Ok);
    std::vector<WalEntryView> got;
    ASSERT_EQ(drain(r, got, 1), WalScanStep::End);
    EXPECT_EQ(got.size(), committed);
}

TEST(WalReader, RejectsForeignFile) {
    const auto dir = tmp_dir("foreign");
    const std::string p = (dir / "bad.wal").string();
    {
        std::ofstream f(p, std::ios::binary);
        const char junk[64] = {};
        f.write(junk, sizeof(junk));  // all-zero -> not a WAL header
    }
    WalReader r;
    EXPECT_EQ(r.open(p), WalStatus::BadHeader);
    // A Wal opened on a zeroed stub reinitializes (torn-create recovery).
    Wal w(p, 0);
    EXPECT_EQ(w.open(), WalStatus::Ok);
    EXPECT_EQ(w.tail_seq(), 0u);
    w.close();
}
