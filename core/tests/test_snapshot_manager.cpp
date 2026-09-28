// Task 4.3.1 coverage — WAL snapshotting to PostgreSQL seam (spec §3.5):
// SnapReadyMsg emission after a durable snapshot store, SnapAckMsg drain,
// confirmed-seq monotonicity, and the WAL trim gate (sealed segments whose
// last entry seq is strictly below the confirmed snapshot_seq are deleted;
// the boundary entry seq == snapshot_seq is never covered, the active
// segment is never a candidate, malformed ack slots are consumed not
// wedged).

#include <gtest/gtest.h>

#include <cstdint>
#include <cstdio>
#include <cstring>
#include <filesystem>
#include <fstream>
#include <string>
#include <vector>

#include <fcntl.h>
#include <unistd.h>

#include "book/OrderBook.hpp"
#include "ipc/ShmRing.hpp"
#include "recovery/SnapshotManager.hpp"
#include "recovery/SnapshotStore.hpp"
#include "utils/MemoryPool.hpp"
#include "wal/Wal.hpp"
#include "wal/WalEntry.hpp"

using namespace exch;

namespace {

constexpr uint16_t kShard = 3;
constexpr uint32_t kIid = 7;

std::filesystem::path tmp_dir(const char* name) {
    const auto d = std::filesystem::temp_directory_path() /
                   ("exch_snapmgr_" + std::to_string(::getpid()) + "_" + name);
    std::filesystem::remove_all(d);
    std::filesystem::create_directories(d);
    return d;
}

std::string uniq_base(const char* tag) {
    char buf[128];
    std::snprintf(buf, sizeof(buf), "exch_test_%s_%d", tag,
                  static_cast<int>(::getpid()));
    return {buf};
}

void unlink_ring(const std::string& name) {
    ::shm_unlink(("/" + name).c_str());
}

// Minimal well-formed snapshot blob (header + ext header — the empty-book
// encoding serialize_book produces).
std::vector<uint8_t> empty_book_blob(uint64_t seq) {
    std::vector<uint8_t> b;
    WalBookSnapshotHeader h{};
    h.instrument_id = kIid;
    h.level_count = 0;
    h.order_count = 0;
    h.book_seq = seq;
    WalSnapshotExtHeader xh{};
    xh.magic = kSnapExtMagic;
    xh.version = kSnapExtVersion;
    xh.order_count = 0;
    const auto put = [&b](const auto& v) {
        const uint8_t* p = reinterpret_cast<const uint8_t*>(&v);
        b.insert(b.end(), p, p + sizeof(v));
    };
    put(h);
    put(xh);
    return b;
}

// Segment seq base from a pending_archive() path (".../{base}.wal").
uint64_t seg_base(const std::string& path) {
    const std::string stem = std::filesystem::path(path).stem().string();
    return std::stoull(stem);
}

// Append n small entries; returns the tail seq after them.
uint64_t append_n(Wal& w, uint64_t n) {
    char payload[64] = {};
    for (uint64_t i = 0; i < n; ++i) {
        EXPECT_EQ(w.append(WalEventType::TIME_TICK, payload, sizeof(payload)),
                  WalStatus::Ok);
    }
    return w.tail_seq();
}

// Parse a SnapAckMsg-ready buffer back (test-side decoder).
SnapAckMsg mk_ack(uint16_t shard, uint32_t iid, uint64_t seq, uint64_t id,
                  uint32_t status = kSnapAckPersisted) {
    SnapAckMsg m{};
    m.magic = kSnapAckMagic;
    m.version = kSnapMsgVersion;
    m.shard_id = shard;
    m.instrument_id = iid;
    m.status = status;
    m.snapshot_seq = seq;
    m.snapshot_id = id;
    return m;
}

}  // namespace

TEST(SnapshotManager, Crc32cMatchesGoCastagnoliContract) {
    // Canonical CRC-32C vector — the same polynomial+init the Go side
    // computes via crc32.MakeTable(crc32.Castagnoli) (services wal.go
    // CRC32C). Pin it so descriptor verification is provably cross-language.
    EXPECT_EQ(wal_crc32c("123456789", 9), 0xE3069283u);
}

TEST(SnapshotManager, ReadyNotifyCarriesFileDescriptor) {
    const auto snap_root = tmp_dir("ready");
    const std::string base = uniq_base("snapready");
    const std::string ready_name = snap_ready_name(base, kShard);
    const std::string ack_name = snap_ack_name(base, kShard);
    unlink_ring(ready_name);
    unlink_ring(ack_name);

    FileSnapshotSink sink(snap_root.string(), kShard);
    SnapshotManager mgr(kShard, sink);
    mgr.open(ready_name, ack_name);
    ASSERT_TRUE(mgr.notify_open());
    ASSERT_TRUE(mgr.ack_open());

    const auto blob = empty_book_blob(42);
    ASSERT_TRUE(sink.store(kIid, 42, blob.data(), blob.size()));
    ASSERT_TRUE(mgr.notify_stored(kIid, 42));
    EXPECT_EQ(mgr.notifies_sent(), 1u);

    // Consumer side (Go service role): attach + peek the descriptor.
    ShmRing consumer(ready_name, ShmRing::Role::Consumer, /*create=*/true);
    ASSERT_TRUE(consumer.is_open());
    uint32_t len = 0;
    const uint8_t* p = consumer.peek(&len);
    ASSERT_NE(p, nullptr);
    ASSERT_EQ(len, sizeof(SnapReadyMsg));
    SnapReadyMsg m{};
    std::memcpy(&m, p, sizeof(m));
    EXPECT_EQ(m.magic, kSnapReadyMagic);
    EXPECT_EQ(m.version, kSnapMsgVersion);
    EXPECT_EQ(m.shard_id, kShard);
    EXPECT_EQ(m.instrument_id, kIid);
    EXPECT_EQ(m.snapshot_seq, 42u);
    EXPECT_EQ(std::string(m.rel_path, m.rel_path_len),
              sink.snapshot_rel_path(kIid, 42));

    // byte_size + crc32c describe the whole snap file as the Go writer
    // will read it.
    std::ifstream f(sink.snapshot_path(kIid, 42), std::ios::binary);
    const std::vector<uint8_t> bytes((std::istreambuf_iterator<char>(f)),
                                     std::istreambuf_iterator<char>());
    EXPECT_EQ(m.byte_size, bytes.size());
    EXPECT_EQ(m.file_crc32c, wal_crc32c(bytes.data(), bytes.size()));
    consumer.consume();
    mgr.close();
    unlink_ring(ready_name);
    unlink_ring(ack_name);
}

TEST(SnapshotManager, NotifyFailsClosedWithoutFile) {
    const auto snap_root = tmp_dir("nofile");
    FileSnapshotSink sink(snap_root.string(), kShard);
    SnapshotManager mgr(kShard, sink);
    const std::string base = uniq_base("snapnofile");
    mgr.open(snap_ready_name(base, kShard), snap_ack_name(base, kShard));
    // No snapshot stored for seq 77 — the file cannot be described.
    EXPECT_FALSE(mgr.notify_stored(kIid, 77));
    EXPECT_EQ(mgr.notify_drops(), 1u);
    EXPECT_EQ(mgr.notifies_sent(), 0u);
    mgr.close();
    unlink_ring(snap_ready_name(base, kShard));
    unlink_ring(snap_ack_name(base, kShard));
}

TEST(SnapshotManager, AckConfirmTrimsCoveredSealedSegments) {
    const auto dir = tmp_dir("trim");
    const std::string wal0 = (dir / "0.wal").string();

    WalOptions opt;
    opt.segment_limit = 2 * kWalBlockSize;  // rotate early — tiny entries
    Wal wal(wal0, kShard, opt);
    ASSERT_EQ(wal.open(), WalStatus::Ok);
    // Enough entries to seal several segments (~89B/entry, 8KiB limit).
    const uint64_t tail = append_n(wal, 600);
    ASSERT_GE(wal.pending_archive().size(), 2u);

    // snapshot_seq = the active segment's seq base: every sealed segment's
    // last entry is strictly below it -> all sealants are covered.
    const uint64_t snap_seq = seg_base(wal.path());
    ASSERT_GT(snap_seq, 0u);
    const std::vector<std::string> sealed = wal.pending_archive();
    ASSERT_FALSE(sealed.empty());
    for (const auto& s : sealed) ASSERT_TRUE(std::filesystem::exists(s));

    const std::string base = uniq_base("snapack");
    const std::string ready_name = snap_ready_name(base, kShard);
    const std::string ack_name = snap_ack_name(base, kShard);
    unlink_ring(ready_name);
    unlink_ring(ack_name);

    FileSnapshotSink sink(dir.string() + "/snap", kShard);
    SnapshotManager mgr(kShard, sink);
    mgr.open(ready_name, ack_name);

    // Go-side ack: persisted snapshot at snap_seq.
    ShmRing ack_prod(ack_name, ShmRing::Role::Producer, /*create=*/true);
    const SnapAckMsg ack = mk_ack(kShard, kIid, snap_seq, /*id=*/101);
    ASSERT_TRUE(ack_prod.try_write(&ack, sizeof(ack)));

    const uint64_t removed = mgr.poll_acks(wal);
    EXPECT_EQ(removed, sealed.size());
    EXPECT_EQ(mgr.confirmed_seq(), snap_seq);
    EXPECT_EQ(mgr.segments_trimmed(), sealed.size());
    EXPECT_TRUE(wal.pending_archive().empty());
    for (const auto& s : sealed) EXPECT_FALSE(std::filesystem::exists(s));
    // The active segment survives untouched.
    EXPECT_TRUE(std::filesystem::exists(wal.path()));
    EXPECT_EQ(wal.tail_seq(), tail);

    mgr.close();
    wal.close();
    unlink_ring(ready_name);
    unlink_ring(ack_name);
}

TEST(SnapshotManager, BoundaryEntryIsNeverCovered) {
    const auto dir = tmp_dir("boundary");
    WalOptions opt;
    opt.segment_limit = 2 * kWalBlockSize;
    Wal wal((dir / "0.wal").string(), kShard, opt);
    ASSERT_EQ(wal.open(), WalStatus::Ok);
    append_n(wal, 400);
    ASSERT_GE(wal.pending_archive().size(), 1u);

    // Pick the first sealed segment; its last entry seq is base(next)-1.
    const auto sealed = wal.pending_archive();
    const uint64_t last0 = seg_base(sealed.size() > 1 ? sealed[1]
                                                    : wal.path()) - 1;
    // Confirming snapshot_seq == last0 leaves the segment's boundary entry
    // uncovered (entries seq < snapshot_seq only) -> keep it.
    EXPECT_EQ(wal.trim_sealed(last0), 0u);
    EXPECT_TRUE(std::filesystem::exists(sealed[0]));
    // Confirming last0 + 1 covers it entirely -> deleted.
    EXPECT_GE(wal.trim_sealed(last0 + 1), 1u);
    EXPECT_FALSE(std::filesystem::exists(sealed[0]));
    wal.close();
}

TEST(SnapshotManager, MalformedAndForeignAcksConsumedNotWedged) {
    const auto dir = tmp_dir("badack");
    const std::string base = uniq_base("snapbad");
    const std::string ack_name = snap_ack_name(base, kShard);
    unlink_ring(snap_ready_name(base, kShard));
    unlink_ring(ack_name);

    FileSnapshotSink sink(dir.string(), kShard);
    SnapshotManager mgr(kShard, sink);
    mgr.open(snap_ready_name(base, kShard), ack_name);
    ShmRing prod(ack_name, ShmRing::Role::Producer, /*create=*/true);
    ASSERT_TRUE(prod.is_open());

    // Junk slot (wrong length).
    const uint8_t junk[7] = {0xde, 0xad, 0xbe, 0xef, 1, 2, 3};
    ASSERT_TRUE(prod.try_write(junk, sizeof(junk)));
    // Well-formed but wrong shard.
    const SnapAckMsg foreign = mk_ack(kShard + 1, kIid, 999, 1);
    ASSERT_TRUE(prod.try_write(&foreign, sizeof(foreign)));
    // Well-formed shard but non-persisted status.
    const SnapAckMsg failed = mk_ack(kShard, kIid, 999, 2, /*status=*/1);
    ASSERT_TRUE(prod.try_write(&failed, sizeof(failed)));

    Wal wal((dir / "0.wal").string(), kShard);
    ASSERT_EQ(wal.open(), WalStatus::Ok);
    EXPECT_EQ(mgr.poll_acks(wal), 0u);
    EXPECT_EQ(mgr.confirmed_seq(), 0u);
    EXPECT_EQ(mgr.acks_seen(), 0u);
    EXPECT_EQ(prod.occupancy(), 0u);  // all three slots consumed, none wedged
    wal.close();
    mgr.close();
    unlink_ring(snap_ready_name(base, kShard));
    unlink_ring(ack_name);
}

TEST(SnapshotManager, ConfirmedSeqIsMonotone) {
    const auto dir = tmp_dir("monotone");
    const std::string base = uniq_base("snapmono");
    unlink_ring(snap_ready_name(base, kShard));
    unlink_ring(snap_ack_name(base, kShard));

    FileSnapshotSink sink(dir.string(), kShard);
    SnapshotManager mgr(kShard, sink);
    mgr.open(snap_ready_name(base, kShard), snap_ack_name(base, kShard));
    ShmRing prod(snap_ack_name(base, kShard), ShmRing::Role::Producer,
                 /*create=*/true);

    Wal wal((dir / "0.wal").string(), kShard);
    ASSERT_EQ(wal.open(), WalStatus::Ok);

    const SnapAckMsg hi = mk_ack(kShard, kIid, 500, 1);
    const SnapAckMsg lo = mk_ack(kShard, kIid, 200, 2);
    ASSERT_TRUE(prod.try_write(&hi, sizeof(hi)));
    ASSERT_TRUE(prod.try_write(&lo, sizeof(lo)));
    mgr.poll_acks(wal);
    EXPECT_EQ(mgr.confirmed_seq(), 500u);
    EXPECT_EQ(mgr.acks_seen(), 2u);  // both well-formed; max wins

    wal.close();
    mgr.close();
    unlink_ring(snap_ready_name(base, kShard));
    unlink_ring(snap_ack_name(base, kShard));
}

TEST(SnapshotManager, EndToEndFileToAckTrim) {
    // The full seam minus Postgres: sink stores a snapshot, manager emits
    // SnapReadyMsg, a simulated Go writer verifies the descriptor against
    // the file, acks, and the WAL trims the covered sealed segment.
    const auto dir = tmp_dir("e2e");
    const std::string base = uniq_base("snape2e");
    const std::string ready_name = snap_ready_name(base, kShard);
    const std::string ack_name = snap_ack_name(base, kShard);
    unlink_ring(ready_name);
    unlink_ring(ack_name);

    WalOptions opt;
    opt.segment_limit = 2 * kWalBlockSize;
    Wal wal((dir / "0.wal").string(), kShard, opt);
    ASSERT_EQ(wal.open(), WalStatus::Ok);
    append_n(wal, 300);
    const uint64_t snap_seq = seg_base(wal.path());  // covers all sealed
    ASSERT_GT(snap_seq, 0u);

    FileSnapshotSink sink(dir.string() + "/snap", kShard);
    SnapshotManager mgr(kShard, sink);
    mgr.open(ready_name, ack_name);

    const auto blob = empty_book_blob(snap_seq);
    ASSERT_TRUE(sink.store(kIid, snap_seq, blob.data(), blob.size()));
    ASSERT_TRUE(mgr.notify_stored(kIid, snap_seq));

    // --- Simulated Go writer ------------------------------------------------
    ShmRing ready_cons(ready_name, ShmRing::Role::Consumer, /*create=*/true);
    ShmRing ack_prod(ack_name, ShmRing::Role::Producer, /*create=*/true);
    uint32_t len = 0;
    const uint8_t* p = ready_cons.peek(&len);
    ASSERT_NE(p, nullptr);
    SnapReadyMsg rd{};
    ASSERT_EQ(len, sizeof(rd));
    std::memcpy(&rd, p, sizeof(rd));
    ready_cons.consume();
    // Descriptor verification (what snapshot_service.go does).
    const auto file = std::filesystem::path(sink.root()) /
                      std::string(rd.rel_path, rd.rel_path_len);
    std::ifstream f(file, std::ios::binary);
    const std::vector<uint8_t> bytes((std::istreambuf_iterator<char>(f)),
                                     std::istreambuf_iterator<char>());
    ASSERT_EQ(bytes.size(), rd.byte_size);
    ASSERT_EQ(wal_crc32c(bytes.data(), bytes.size()), rd.file_crc32c);
    const SnapAckMsg ack = mk_ack(kShard, kIid, rd.snapshot_seq, 9001);
    ASSERT_TRUE(ack_prod.try_write(&ack, sizeof(ack)));
    // ------------------------------------------------------------------------

    EXPECT_GT(mgr.poll_acks(wal), 0u);
    EXPECT_EQ(mgr.confirmed_seq(), snap_seq);
    EXPECT_TRUE(wal.pending_archive().empty());

    wal.close();
    mgr.close();
    unlink_ring(ready_name);
    unlink_ring(ack_name);
}
