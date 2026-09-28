// Task 2.3.4 coverage — binary WAL integration + RecoveryManager (spec §3.5,
// §18.1): FileSnapshotSink round-trip, snapshot cadence, snapshot-only /
// WAL-only / snapshot+WAL / stale-snapshot boots, torn-tail truncation via
// Wal::open recovery, idempotent replay (duplicate ORDER_NEW, TRADE dedup),
// book_seq invariant enforcement, and byte-identical deterministic replay.
//
// Determinism contract under test: every WAL entry is written with the
// explicit (seq, timestamp_ns) append form, and recovered books are compared
// via SnapshotStore::serialize_book bytes — order ids, prices, original +
// filled + display quantities, timestamps, ingress seqs, and FIFO order all
// live in that blob, so byte equality IS full-state equality.

#include <gtest/gtest.h>

#include <cstdint>
#include <cstring>
#include <filesystem>
#include <fstream>
#include <string>
#include <tuple>
#include <vector>

#include <fcntl.h>
#include <unistd.h>

#include "book/OrderBook.hpp"
#include "book/PriceLevel.hpp"
#include "recovery/RecoveryManager.hpp"
#include "recovery/SnapshotStore.hpp"
#include "utils/MemoryPool.hpp"
#include "wal/Wal.hpp"
#include "wal/WalEntry.hpp"

using namespace exch;

namespace {

constexpr uint32_t kIid = 7;
constexpr uint16_t kShard = 3;
constexpr int64_t kTick = 1;  // prices are int64 10^8 ticks; small ints fine

std::filesystem::path tmp_dir(const char* name) {
    const auto d = std::filesystem::temp_directory_path() /
                   ("exch_recovery_" + std::to_string(::getpid()) + "_" + name);
    std::filesystem::remove_all(d);
    std::filesystem::create_directories(d);
    return d;
}

// Caller-side template — mirrors what replayed ORDER_NEW reconstructs.
Order mk_order(uint64_t id, Side side, int64_t price, int64_t qty,
               uint64_t ts, uint64_t ingress, uint64_t acct = 42) {
    Order o{};
    o.id = id;
    o.account_id = acct;
    o.side = side;
    o.type = OrderType::LIMIT;
    o.tif = TimeInForce::GTC;
    o.stp_mode = StpMode::CANCEL_NEWEST;
    o.flags = 0;
    o.price_ticks = price;
    o.qty_units = qty;
    o.filled_qty_units = 0;
    o.display_qty_units = 0;  // 0 = fully visible (non-iceberg convention)
    o.timestamp_ns = ts;
    o.ingress_seq = ingress;
    return o;
}

WalOrderNewPayload p_new(const Order& o, uint32_t iid = kIid) {
    WalOrderNewPayload p{};
    p.order_id = o.id;
    p.account_id = o.account_id;
    p.instrument_id = iid;
    p.side = static_cast<uint8_t>(o.side);
    p.type = static_cast<uint8_t>(o.type);
    p.tif = static_cast<uint8_t>(o.tif);
    p.flags = o.flags;
    p.price_ticks = o.price_ticks;
    p.qty_units = o.qty_units;
    p.visible_qty_units = o.qty_units;  // wire: == qty when non-iceberg
    p.stp_mode = static_cast<uint32_t>(o.stp_mode);
    return p;
}

WalOrderCancelPayload p_cancel(uint64_t id, uint8_t reason = 0) {
    WalOrderCancelPayload p{};
    p.order_id = id;
    p.account_id = 42;
    p.reason = reason;
    return p;
}

WalOrderModifyPayload p_modify(uint64_t id, int64_t px, int64_t qty) {
    WalOrderModifyPayload p{};
    p.order_id = id;
    p.new_price_ticks = px;
    p.new_qty_units = qty;
    return p;
}

WalTradePayload p_trade(uint64_t trade_id, uint64_t buy_id, uint64_t sell_id,
                        int64_t px, int64_t qty, uint32_t iid = kIid) {
    WalTradePayload p{};
    p.trade_id = trade_id;
    p.buy_order_id = buy_id;
    p.sell_order_id = sell_id;
    p.instrument_id = iid;
    p.price_ticks = px;
    p.qty_units = qty;
    return p;
}

// Create + populate one WAL segment file. Deterministic: explicit seq + ts.
void write_wal(const std::string& path, uint16_t shard,
               const std::vector<std::tuple<uint64_t, uint64_t, WalEventType,
                                            const void*, uint32_t>>& entries) {
    Wal w(path, shard);
    ASSERT_EQ(w.open(), WalStatus::Ok);
    for (const auto& [seq, ts, t, p, n] : entries) {
        ASSERT_EQ(w.append(seq, ts, t, p, n), WalStatus::Ok);
    }
    ASSERT_EQ(w.flush(), WalStatus::Ok);
    w.close();
}

// Byte offset just past the last valid record (the scanner's End position).
uint64_t valid_end_of(const std::string& path) {
    WalReader r;
    EXPECT_EQ(r.open(path), WalStatus::Ok);
    WalEntryView ev;
    WalScanStep s;
    do {
        s = r.next(ev);
    } while (s == WalScanStep::Entry || s == WalScanStep::Pad);
    EXPECT_EQ(s, WalScanStep::End);
    return r.offset();
}

// Append a CRC-invalid record at `off` — the torn-tail fixture. The record
// is structurally complete (correct framing) with a corrupted payload byte,
// so the scanner reports Corrupt exactly at `off`.
void inject_torn_tail(const std::string& path, uint64_t off) {
    WalOrderCancelPayload junk = p_cancel(0xDEAD, 4);
    std::vector<uint8_t> rec(kWalEntryOverhead + sizeof(junk));
    const uint64_t n = wal_encode_entry(rec.data(), /*seq*/ 99, /*ts*/ 1,
                                        WalEventType::ORDER_CANCEL, &junk,
                                        sizeof(junk));
    ASSERT_EQ(n, rec.size());
    rec[sizeof(WalEntryHeader)] ^= 0xFF;  // flip a payload byte → bad CRC
    const int fd = ::open(path.c_str(), O_WRONLY | O_CLOEXEC);
    ASSERT_GE(fd, 0);
    ASSERT_EQ(::pwrite(fd, rec.data(), rec.size(), static_cast<off_t>(off)),
              static_cast<ssize_t>(rec.size()));
    ::close(fd);
}

// Full-state fingerprint: level structure + every order's pinned/extension
// fields in level-major FIFO order.
std::vector<uint8_t> fingerprint(const OrderBook& book) {
    std::vector<uint8_t> blob;
    WalBookSnapshotHeader hdr{};
    EXPECT_TRUE(SnapshotStore::serialize_book(book, kIid, /*wal_seq*/ 0,
                                              blob, hdr));
    return blob;
}

struct Fixture {
    MemoryPool<Order> pool{4096};
    OrderBook book{pool};
};

void add(OrderBook& b, const Order& o) {
    Order* out = nullptr;
    ASSERT_EQ(b.add_order(o, &out), BookError::OK);
    ASSERT_NE(out, nullptr);
}

}  // namespace

// --- FileSnapshotSink ---------------------------------------------------------

TEST(FileSnapshotSink, RoundTripPreservesFullBook) {
    const auto root = tmp_dir("snap_rt");
    Fixture f;
    add(f.book, mk_order(1, Side::BUY, 10000 * kTick, 50, 100, 0, 7));
    add(f.book, mk_order(2, Side::BUY, 10000 * kTick, 30, 101, 1, 8));
    add(f.book, mk_order(3, Side::BUY, 9900 * kTick, 10, 102, 2));
    add(f.book, mk_order(4, Side::SELL, 10100 * kTick, 20, 103, 3));
    add(f.book, mk_order(5, Side::SELL, 10200 * kTick, 15, 104, 4));
    // Partial fill makes qty/filled/remaining distinct — the pinned record
    // alone cannot express this; the extension must carry it.
    ASSERT_EQ(f.book.apply_fill(f.book.find_order(1), 20), BookError::OK);
    const char* v = nullptr;
    ASSERT_TRUE(f.book.validate(&v)) << (v ? v : "");

    std::vector<uint8_t> blob;
    WalBookSnapshotHeader hdr{};
    ASSERT_TRUE(SnapshotStore::serialize_book(f.book, kIid, /*seq*/ 1234,
                                              blob, hdr));
    EXPECT_EQ(hdr.instrument_id, kIid);
    EXPECT_EQ(hdr.order_count, 5u);
    EXPECT_EQ(hdr.level_count, 4u);  // bids: 10000(x2), 9900; asks: 10100, 10200
    EXPECT_EQ(hdr.book_seq, 1234u);

    FileSnapshotSink sink(root.string(), kShard);
    ASSERT_TRUE(sink.store(kIid, 1234, blob.data(), blob.size()));

    uint64_t seq = 0;
    bool has = false;
    std::vector<uint8_t> loaded;
    ASSERT_TRUE(sink.load_latest(kIid, &seq, &loaded, &has));
    ASSERT_TRUE(has);
    EXPECT_EQ(seq, 1234u);
    EXPECT_EQ(loaded, blob);  // byte-identical payload

    ParsedSnapshot parsed;
    ASSERT_TRUE(SnapshotStore::parse_book(loaded.data(), loaded.size(), parsed));
    ASSERT_EQ(parsed.orders.size(), 5u);
    // Order 1 must carry original qty 50, filled 20 (remaining 30 pinned).
    const ParsedSnapshotOrder* po1 = nullptr;
    for (const auto& po : parsed.orders)
        if (po.tmpl.id == 1) po1 = &po;
    ASSERT_NE(po1, nullptr);
    EXPECT_EQ(po1->tmpl.qty_units, 50);
    EXPECT_EQ(po1->tmpl.filled_qty_units, 20);
    EXPECT_EQ(po1->remaining, 30);
    EXPECT_EQ(po1->tmpl.price_ticks, 10000 * kTick);
}

TEST(FileSnapshotSink, LoadLatestNewestAndTmpIgnored) {
    const auto root = tmp_dir("snap_newest");
    Fixture f;
    add(f.book, mk_order(1, Side::BUY, 10000 * kTick, 50, 100, 0));
    std::vector<uint8_t> blob;
    WalBookSnapshotHeader hdr{};
    ASSERT_TRUE(SnapshotStore::serialize_book(f.book, kIid, 100, blob, hdr));

    FileSnapshotSink sink(root.string(), kShard);
    ASSERT_TRUE(sink.store(kIid, 100, blob.data(), blob.size()));
    ASSERT_TRUE(sink.store(kIid, 200, blob.data(), blob.size()));

    // A torn tmp from a crashed store at seq 300 must NEVER shadow the good
    // newest snapshot (a partial name match would select it otherwise).
    const auto dir = root / "i7";
    {
        std::ofstream ofs(dir / "snap_00000000000000000300.bin.tmp-999",
                          std::ios::binary);
        ofs << "garbage-partial-write";
    }

    uint64_t seq = 0;
    bool has = false;
    std::vector<uint8_t> loaded;
    ASSERT_TRUE(sink.load_latest(kIid, &seq, &loaded, &has));
    ASSERT_TRUE(has);
    EXPECT_EQ(seq, 200u);
}

// --- Snapshot cadence ----------------------------------------------------------

TEST(SnapshotStoreCadence, TradesOrTimeTrigger) {
    const auto root = tmp_dir("cadence");
    FileSnapshotSink sink(root.string(), kShard);
    SnapshotPolicy pol{};
    pol.trade_interval = 3;
    pol.interval_ns = 1'000;
    SnapshotStore ss(sink, pol);

    Fixture f;
    add(f.book, mk_order(1, Side::BUY, 10000 * kTick, 50, 100, 0));

    // First call snapshots unconditionally (bounds worst-case replay depth).
    EXPECT_EQ(ss.maybe_snapshot(f.book, kIid, 10, /*now*/ 0, /*trades*/ 0),
              SnapshotOutcome::Taken);
    // Below both thresholds → skipped.
    EXPECT_EQ(ss.maybe_snapshot(f.book, kIid, 11, /*now*/ 500, /*trades*/ 2),
              SnapshotOutcome::Skipped);
    // Trade threshold hit (accumulated 2+1).
    EXPECT_EQ(ss.maybe_snapshot(f.book, kIid, 12, /*now*/ 600, /*trades*/ 1),
              SnapshotOutcome::Taken);
    // Time threshold hit.
    EXPECT_EQ(ss.maybe_snapshot(f.book, kIid, 13, /*now*/ 600 + 1'001,
                                /*trades*/ 0),
              SnapshotOutcome::Taken);
    EXPECT_EQ(ss.snapshots_taken(), 3u);
    EXPECT_EQ(ss.last_snapshot_seq(), 13u);
}

// --- RecoveryManager: boots -----------------------------------------------------

TEST(RecoveryManager, EmptyWalBootsClean) {
    const auto dir = tmp_dir("empty_wal");
    Fixture f;
    RecoveryManager rm(kShard);  // no snapshot sink — WAL-only manager
    const RecoveryResult res = rm.recover(dir.string(), {{kIid, &f.book}});
    EXPECT_EQ(res.status, RecoveryStatus::Ok) << res.detail;
    EXPECT_TRUE(res.ok());
    EXPECT_EQ(res.outcome(), RecoveryOutcome::CLEAN);
    EXPECT_EQ(res.wal_tail, 0u);
    EXPECT_EQ(res.wal_entries, 0u);
    EXPECT_FALSE(res.tail_truncated);
    ASSERT_EQ(res.books.size(), 1u);
    EXPECT_TRUE(res.books[0].book_seq_verified);
    EXPECT_EQ(f.book.live_orders(), 0u);
}

TEST(RecoveryManager, SnapshotOnlyBootRestoresBook) {
    const auto root = tmp_dir("snap_only");
    const auto wal_dir = root / "wal";
    std::filesystem::create_directories(wal_dir);

    // Authoritative book snapshotted at WAL cursor 10.
    Fixture src;
    add(src.book, mk_order(1, Side::BUY, 10000 * kTick, 50, 100, 0, 7));
    add(src.book, mk_order(2, Side::BUY, 9900 * kTick, 10, 101, 1, 8));
    add(src.book, mk_order(4, Side::SELL, 10100 * kTick, 20, 103, 3, 9));
    ASSERT_EQ(src.book.apply_fill(src.book.find_order(1), 20), BookError::OK);

    std::vector<uint8_t> blob;
    WalBookSnapshotHeader hdr{};
    ASSERT_TRUE(SnapshotStore::serialize_book(src.book, kIid, /*seq*/ 10,
                                              blob, hdr));
    FileSnapshotSink sink(root.string(), kShard);
    ASSERT_TRUE(sink.store(kIid, 10, blob.data(), blob.size()));

    Fixture dst;
    RecoveryManager rm(kShard, sink);
    const RecoveryResult res = rm.recover(wal_dir.string(),
                                          {{kIid, &dst.book}});
    ASSERT_EQ(res.status, RecoveryStatus::Ok) << res.detail;
    EXPECT_EQ(res.wal_tail, 0u);
    ASSERT_EQ(res.books.size(), 1u);
    EXPECT_TRUE(res.books[0].snapshot_loaded);
    EXPECT_EQ(res.books[0].snapshot_seq, 10u);
    EXPECT_EQ(res.books[0].orders_restored, 3u);
    EXPECT_TRUE(res.books[0].book_seq_verified);
    EXPECT_EQ(dst.book.live_orders(), 3u);
    EXPECT_EQ(fingerprint(dst.book), fingerprint(src.book));
    const Order* o1 = dst.book.find_order(1);
    ASSERT_NE(o1, nullptr);
    EXPECT_EQ(remaining_qty_units(*o1), 30);
}

TEST(RecoveryManager, WalOnlyBootReplays) {
    const auto dir = tmp_dir("wal_only");
    const auto wpath = dir / "0.wal";
    const Order o1 = mk_order(1, Side::BUY, 10000 * kTick, 50, 100, 0);
    const Order o4 = mk_order(4, Side::SELL, 10100 * kTick, 20, 103, 3);
    const WalOrderNewPayload n1 = p_new(o1);
    const WalOrderNewPayload n4 = p_new(o4);
    const WalTradePayload t1 = p_trade(500, /*buy*/ 1, /*sell*/ 999,
                                       10000 * kTick, 20);
    const WalOrderCancelPayload c4 = p_cancel(4);
    write_wal(wpath.string(), kShard,
              {
                  {0, 100, WalEventType::ORDER_NEW, &n1, sizeof(n1)},
                  {1, 103, WalEventType::ORDER_NEW, &n4, sizeof(n4)},
                  {2, 104, WalEventType::TRADE, &t1, sizeof(t1)},
                  {3, 105, WalEventType::ORDER_CANCEL, &c4, sizeof(c4)},
              });

    Fixture f;
    // Sink bound but empty root — exercises the load_latest(has=false) cold
    // start path (replay from genesis) rather than the no-sink shortcut.
    FileSnapshotSink sink(tmp_dir("wal_only_sink").string(), kShard);
    RecoveryManager rm(kShard, sink);
    const RecoveryResult res = rm.recover(dir.string(), {{kIid, &f.book}});
    ASSERT_EQ(res.status, RecoveryStatus::Ok) << res.detail;
    EXPECT_EQ(res.wal_tail, 4u);
    EXPECT_EQ(res.stream_base, 0u);
    EXPECT_EQ(res.wal_entries, 4u);
    EXPECT_EQ(res.mutations_applied, 4u);   // add + add + fill + cancel
    EXPECT_EQ(res.dedup_skips, 0u);
    EXPECT_FALSE(res.tail_truncated);
    ASSERT_EQ(res.books.size(), 1u);
    EXPECT_TRUE(res.books[0].book_seq_verified);
    EXPECT_EQ(res.books[0].recomputed_book_seq, 4u);

    ASSERT_EQ(f.book.live_orders(), 1u);
    const Order* r = f.book.find_order(1);
    ASSERT_NE(r, nullptr);
    EXPECT_EQ(r->filled_qty_units, 20);
    EXPECT_EQ(remaining_qty_units(*r), 30);
    EXPECT_EQ(f.book.find_order(4), nullptr);
}

TEST(RecoveryManager, SnapshotPlusWalReplaysOnlyTail) {
    const auto root = tmp_dir("snap_plus");
    const auto wal_dir = root / "wal";
    std::filesystem::create_directories(wal_dir);
    const auto wpath = wal_dir / "0.wal";

    const Order o1 = mk_order(1, Side::BUY, 10000 * kTick, 50, 100, 0);
    const Order o2 = mk_order(2, Side::BUY, 9900 * kTick, 10, 101, 1);
    const Order o4 = mk_order(4, Side::SELL, 10100 * kTick, 20, 103, 3);
    const WalOrderNewPayload n1 = p_new(o1);
    const WalOrderNewPayload n2 = p_new(o2);
    const WalOrderNewPayload n4 = p_new(o4);
    const WalTradePayload t1 = p_trade(500, 1, 999, 10000 * kTick, 20);
    const WalOrderCancelPayload c2 = p_cancel(2);
    const WalOrderNewPayload n9 = p_new(
        mk_order(9, Side::SELL, 10200 * kTick, 5, 110, 9));

    // WAL: seqs 0..3 covered by the snapshot, 4..5 replayed.
    write_wal(wpath.string(), kShard,
              {
                  {0, 100, WalEventType::ORDER_NEW, &n1, sizeof(n1)},
                  {1, 101, WalEventType::ORDER_NEW, &n2, sizeof(n2)},
                  {2, 103, WalEventType::ORDER_NEW, &n4, sizeof(n4)},
                  {3, 104, WalEventType::TRADE, &t1, sizeof(t1)},
                  {4, 105, WalEventType::ORDER_CANCEL, &c2, sizeof(c2)},
                  {5, 110, WalEventType::ORDER_NEW, &n9, sizeof(n9)},
              });

    // Snapshot = book state after seqs 0..3 (orders 1,2,4 resting; 1 filled 20).
    Fixture src;
    add(src.book, o1);
    add(src.book, o2);
    add(src.book, o4);
    ASSERT_EQ(src.book.apply_fill(src.book.find_order(1), 20), BookError::OK);
    std::vector<uint8_t> blob;
    WalBookSnapshotHeader hdr{};
    ASSERT_TRUE(SnapshotStore::serialize_book(src.book, kIid, /*seq*/ 4,
                                              blob, hdr));
    FileSnapshotSink sink(root.string(), kShard);
    ASSERT_TRUE(sink.store(kIid, 4, blob.data(), blob.size()));

    Fixture dst;
    RecoveryManager rm(kShard, sink);
    const RecoveryResult res = rm.recover(wal_dir.string(),
                                          {{kIid, &dst.book}});
    ASSERT_EQ(res.status, RecoveryStatus::Ok) << res.detail;
    EXPECT_EQ(res.wal_tail, 6u);
    EXPECT_EQ(res.wal_entries, 6u);
    ASSERT_EQ(res.books.size(), 1u);
    const PerBookRecovery& pr = res.books[0];
    EXPECT_TRUE(pr.snapshot_loaded);
    EXPECT_EQ(pr.snapshot_seq, 4u);
    EXPECT_EQ(pr.orders_restored, 3u);
    EXPECT_EQ(pr.covered_skips, 4u);      // seqs 0..3 snapshot-covered
    EXPECT_EQ(pr.entries_consumed, 2u);   // seqs 4,5 replayed
    EXPECT_EQ(pr.mutations_applied, 2u);  // cancel + add
    EXPECT_TRUE(pr.book_seq_verified);

    // Expected post-replay book: order1(filled20) + order4 + order9; 2 gone.
    // Replayed ORDER_NEW stamps ingress_seq = the entry's WAL seq (5), not the
    // original ingress — the deterministic replay cursor is the WAL order.
    Fixture want;
    add(want.book, o1);
    add(want.book, o4);
    ASSERT_EQ(want.book.apply_fill(want.book.find_order(1), 20),
              BookError::OK);
    add(want.book, mk_order(9, Side::SELL, 10200 * kTick, 5, 110, 5));
    EXPECT_EQ(fingerprint(dst.book), fingerprint(want.book));
}

TEST(RecoveryManager, StaleSnapshotLongerWalStillConverges) {
    const auto root = tmp_dir("stale_snap");
    const auto wal_dir = root / "wal";
    std::filesystem::create_directories(wal_dir);
    const auto wpath = wal_dir / "0.wal";

    const Order o1 = mk_order(1, Side::BUY, 10000 * kTick, 50, 100, 0);
    const Order o4 = mk_order(4, Side::SELL, 10100 * kTick, 20, 103, 3);
    const WalOrderNewPayload n1 = p_new(o1);
    const WalOrderNewPayload n4 = p_new(o4);
    const WalTradePayload t1 = p_trade(500, 1, 999, 10000 * kTick, 20);
    const WalOrderCancelPayload c1 = p_cancel(1);
    const WalOrderNewPayload n8 = p_new(
        mk_order(8, Side::BUY, 9800 * kTick, 7, 112, 5));
    write_wal(wpath.string(), kShard,
              {
                  {0, 100, WalEventType::ORDER_NEW, &n1, sizeof(n1)},
                  {1, 103, WalEventType::ORDER_NEW, &n4, sizeof(n4)},
                  {2, 104, WalEventType::TRADE, &t1, sizeof(t1)},
                  {3, 105, WalEventType::ORDER_CANCEL, &c1, sizeof(c1)},
                  {4, 112, WalEventType::ORDER_NEW, &n8, sizeof(n8)},
              });

    // Snapshot only covers seq 0 (order 1 resting, unfilled) — stale by 4.
    Fixture src;
    add(src.book, o1);
    std::vector<uint8_t> blob;
    WalBookSnapshotHeader hdr{};
    ASSERT_TRUE(SnapshotStore::serialize_book(src.book, kIid, /*seq*/ 1,
                                              blob, hdr));
    FileSnapshotSink sink(root.string(), kShard);
    ASSERT_TRUE(sink.store(kIid, 1, blob.data(), blob.size()));

    Fixture dst;
    RecoveryManager rm(kShard, sink);
    const RecoveryResult res = rm.recover(wal_dir.string(),
                                          {{kIid, &dst.book}});
    ASSERT_EQ(res.status, RecoveryStatus::Ok) << res.detail;
    const PerBookRecovery& pr = res.books[0];
    EXPECT_EQ(pr.covered_skips, 1u);
    EXPECT_EQ(pr.entries_consumed, 4u);
    // Book: order1 filled 20 then cancelled; 4 + 8 resting.
    ASSERT_EQ(dst.book.live_orders(), 2u);
    EXPECT_NE(dst.book.find_order(4), nullptr);
    EXPECT_NE(dst.book.find_order(8), nullptr);
    EXPECT_EQ(dst.book.find_order(1), nullptr);
}

TEST(RecoveryManager, CorruptTailIsTruncatedAndReplayed) {
    const auto dir = tmp_dir("corrupt_tail");
    const auto wpath = dir / "0.wal";
    const Order o1 = mk_order(1, Side::BUY, 10000 * kTick, 50, 100, 0);
    const Order o4 = mk_order(4, Side::SELL, 10100 * kTick, 20, 103, 3);
    const WalOrderNewPayload n1 = p_new(o1);
    const WalOrderNewPayload n4 = p_new(o4);
    write_wal(wpath.string(), kShard,
              {
                  {0, 100, WalEventType::ORDER_NEW, &n1, sizeof(n1)},
                  {1, 103, WalEventType::ORDER_NEW, &n4, sizeof(n4)},
              });

    const uint64_t off = valid_end_of(wpath.string());
    inject_torn_tail(wpath.string(), off);  // CRC-invalid record at the tail

    Fixture f;
    RecoveryManager rm(kShard);
    const RecoveryResult res = rm.recover(dir.string(), {{kIid, &f.book}});
    ASSERT_EQ(res.status, RecoveryStatus::Ok) << res.detail;
    EXPECT_TRUE(res.tail_truncated);
    EXPECT_EQ(res.truncate_offset, off);
    EXPECT_EQ(res.wal_tail, 2u);
    EXPECT_EQ(res.wal_entries, 2u);
    EXPECT_EQ(res.outcome(), RecoveryOutcome::WAL_REPAIRED);
    ASSERT_EQ(f.book.live_orders(), 2u);  // both valid entries applied
    EXPECT_NE(f.book.find_order(1), nullptr);
    EXPECT_NE(f.book.find_order(4), nullptr);
    // Post-truncation the file is cut exactly at the last valid record.
    std::error_code ec;
    EXPECT_EQ(std::filesystem::file_size(wpath, ec), off);
}

TEST(RecoveryManager, DuplicateOrderNewIsIdempotentNoop) {
    const auto dir = tmp_dir("dup_new");
    const auto wpath = dir / "0.wal";
    const Order o1 = mk_order(1, Side::BUY, 10000 * kTick, 50, 100, 0);
    const WalOrderNewPayload n1 = p_new(o1);
    const WalOrderNewPayload n1dup = p_new(
        mk_order(1, Side::BUY, 10000 * kTick, 50, 200, 1));
    write_wal(wpath.string(), kShard,
              {
                  {0, 100, WalEventType::ORDER_NEW, &n1, sizeof(n1)},
                  {1, 200, WalEventType::ORDER_NEW, &n1dup, sizeof(n1dup)},
              });

    Fixture f;
    RecoveryManager rm(kShard);
    const RecoveryResult res = rm.recover(dir.string(), {{kIid, &f.book}});
    ASSERT_EQ(res.status, RecoveryStatus::Ok) << res.detail;
    EXPECT_EQ(res.mutations_applied, 1u);
    EXPECT_EQ(res.dedup_skips, 1u);
    EXPECT_EQ(f.book.live_orders(), 1u);
    const Order* r = f.book.find_order(1);
    ASSERT_NE(r, nullptr);
    EXPECT_EQ(r->timestamp_ns, 100u);  // first write wins — no overwrite
}

TEST(RecoveryManager, TradeReplayDecrementsResting) {
    const auto dir = tmp_dir("trade_dec");
    const auto wpath = dir / "0.wal";
    const Order o1 = mk_order(1, Side::BUY, 10000 * kTick, 50, 100, 0);
    const WalOrderNewPayload n1 = p_new(o1);
    const WalTradePayload t1 = p_trade(500, 1, 999, 10000 * kTick, 20);
    const WalTradePayload t2 = p_trade(501, 1, 998, 10000 * kTick, 30);
    write_wal(wpath.string(), kShard,
              {
                  {0, 100, WalEventType::ORDER_NEW, &n1, sizeof(n1)},
                  {1, 101, WalEventType::TRADE, &t1, sizeof(t1)},
                  {2, 102, WalEventType::TRADE, &t2, sizeof(t2)},
              });

    Fixture f;
    RecoveryManager rm(kShard);
    const RecoveryResult res = rm.recover(dir.string(), {{kIid, &f.book}});
    ASSERT_EQ(res.status, RecoveryStatus::Ok) << res.detail;
    // 20 + 30 fully consumed the 50 resting — the order is gone.
    EXPECT_EQ(f.book.live_orders(), 0u);
    EXPECT_EQ(res.mutations_applied, 3u);
}

TEST(RecoveryManager, DuplicateTradeIsIdempotentNoop) {
    const auto dir = tmp_dir("dup_trade");
    const auto wpath = dir / "0.wal";
    const Order o1 = mk_order(1, Side::BUY, 10000 * kTick, 50, 100, 0);
    const WalOrderNewPayload n1 = p_new(o1);
    const WalTradePayload t1 = p_trade(500, 1, 999, 10000 * kTick, 20);
    write_wal(wpath.string(), kShard,
              {
                  {0, 100, WalEventType::ORDER_NEW, &n1, sizeof(n1)},
                  {1, 101, WalEventType::TRADE, &t1, sizeof(t1)},
                  {2, 102, WalEventType::TRADE, &t1, sizeof(t1)},  // same trade_id
              });

    Fixture f;
    RecoveryManager rm(kShard);
    const RecoveryResult res = rm.recover(dir.string(), {{kIid, &f.book}});
    ASSERT_EQ(res.status, RecoveryStatus::Ok) << res.detail;
    EXPECT_EQ(res.dedup_skips, 1u);
    const Order* r = f.book.find_order(1);
    ASSERT_NE(r, nullptr);
    EXPECT_EQ(r->filled_qty_units, 20);  // applied exactly once — zero dupes
}

// --- Invariant enforcement -------------------------------------------------------

TEST(RecoveryManager, ForgedBookSeqFailsInvariant) {
    const auto root = tmp_dir("forged_seq");
    const auto wal_dir = root / "wal";
    std::filesystem::create_directories(wal_dir);
    const auto wpath = wal_dir / "0.wal";

    const Order o1 = mk_order(1, Side::BUY, 10000 * kTick, 50, 100, 0);
    const WalOrderNewPayload n1 = p_new(o1);
    write_wal(wpath.string(), kShard,
              {{0, 100, WalEventType::ORDER_NEW, &n1, sizeof(n1)}});
    // WAL tail is 1.

    // Forged snapshot claims coverage through seq 9 — ahead of the WAL tail.
    Fixture src;
    add(src.book, o1);
    std::vector<uint8_t> blob;
    WalBookSnapshotHeader hdr{};
    ASSERT_TRUE(SnapshotStore::serialize_book(src.book, kIid, /*seq*/ 9,
                                              blob, hdr));
    FileSnapshotSink sink(root.string(), kShard);
    ASSERT_TRUE(sink.store(kIid, 9, blob.data(), blob.size()));

    Fixture dst;
    RecoveryManager rm(kShard, sink);
    const RecoveryResult res = rm.recover(wal_dir.string(),
                                          {{kIid, &dst.book}});
    EXPECT_EQ(res.status, RecoveryStatus::InvariantViolated);
    EXPECT_EQ(res.outcome(), RecoveryOutcome::HALTED);
    EXPECT_EQ(res.first_divergent_seq, 9u);
    EXPECT_EQ(res.detail_instrument, kIid);
}

TEST(RecoveryManager, SequenceGapFailsClosed) {
    const auto dir = tmp_dir("seq_gap");
    const Order o1 = mk_order(1, Side::BUY, 10000 * kTick, 50, 100, 0);
    const Order o2 = mk_order(2, Side::BUY, 9900 * kTick, 10, 101, 1);
    const WalOrderNewPayload n1 = p_new(o1);
    const WalOrderNewPayload n2 = p_new(o2);
    const WalOrderCancelPayload c2 = p_cancel(2);
    // Two segments with a missing seq 1 between them (0 then 2..3).
    write_wal((dir / "0.wal").string(), kShard,
              {{0, 100, WalEventType::ORDER_NEW, &n1, sizeof(n1)}});
    write_wal((dir / "1.wal").string(), kShard,
              {{2, 101, WalEventType::ORDER_NEW, &n2, sizeof(n2)},
               {3, 102, WalEventType::ORDER_CANCEL, &c2, sizeof(c2)}});

    Fixture f;
    RecoveryManager rm(kShard);
    const RecoveryResult res = rm.recover(dir.string(), {{kIid, &f.book}});
    EXPECT_EQ(res.status, RecoveryStatus::SeqGap);
    EXPECT_EQ(res.outcome(), RecoveryOutcome::HALTED);
    EXPECT_EQ(res.first_divergent_seq, 2u);
}

TEST(RecoveryManager, MidSegmentCorruptionFailsClosed) {
    const auto dir = tmp_dir("mid_corrupt");
    const Order o1 = mk_order(1, Side::BUY, 10000 * kTick, 50, 100, 0);
    const Order o2 = mk_order(2, Side::BUY, 9900 * kTick, 10, 101, 1);
    const WalOrderNewPayload n1 = p_new(o1);
    const WalOrderNewPayload n2 = p_new(o2);
    const auto p0 = dir / "0.wal";
    const auto p1 = dir / "1.wal";
    write_wal(p0.string(), kShard,
              {{0, 100, WalEventType::ORDER_NEW, &n1, sizeof(n1)}});
    write_wal(p1.string(), kShard,
              {{1, 101, WalEventType::ORDER_NEW, &n2, sizeof(n2)}});
    // Corrupt the OLDER segment (non-tail): torn byte range after its entry.
    inject_torn_tail(p0.string(), valid_end_of(p0.string()));

    Fixture f;
    RecoveryManager rm(kShard);
    const RecoveryResult res = rm.recover(dir.string(), {{kIid, &f.book}});
    EXPECT_EQ(res.status, RecoveryStatus::WalCorrupt);
    EXPECT_EQ(res.outcome(), RecoveryOutcome::HALTED);
}

TEST(RecoveryManager, CorruptSnapshotFailsLoad) {
    const auto root = tmp_dir("corrupt_snap");
    const auto wal_dir = root / "wal";
    std::filesystem::create_directories(wal_dir);
    const auto snap_dir = root / "i7";
    std::filesystem::create_directories(snap_dir);
    {
        std::ofstream ofs(snap_dir / "snap_00000000000000000005.bin",
                          std::ios::binary);
        const std::string junk(64, '\xAB');
        ofs.write(junk.data(), static_cast<std::streamsize>(junk.size()));
    }
    FileSnapshotSink sink(root.string(), kShard);
    Fixture f;
    RecoveryManager rm(kShard, sink);
    const RecoveryResult res = rm.recover(wal_dir.string(),
                                          {{kIid, &f.book}});
    EXPECT_EQ(res.status, RecoveryStatus::SnapshotLoadFailed);
    EXPECT_EQ(res.outcome(), RecoveryOutcome::HALTED);
}

// --- Full-state fidelity + determinism ------------------------------------------

TEST(RecoveryManager, FullStateByteIdenticalToLiveBook) {
    const auto dir = tmp_dir("full_state");
    const auto wpath = dir / "0.wal";

    // Live book built by direct book ops — the reference.
    Fixture want;
    add(want.book, mk_order(1, Side::BUY, 10000 * kTick, 50, 100, 0, 7));
    add(want.book, mk_order(2, Side::BUY, 10000 * kTick, 30, 101, 1, 8));
    add(want.book, mk_order(3, Side::BUY, 9900 * kTick, 10, 102, 2, 7));
    add(want.book, mk_order(4, Side::SELL, 10100 * kTick, 20, 103, 3, 9));
    ASSERT_EQ(want.book.apply_fill(want.book.find_order(1), 20), BookError::OK);
    ASSERT_EQ(want.book.modify_order(2, 10000 * kTick, 40, 105, 5),
              BookError::OK);  // qty-up loses priority → FIFO tail
    add(want.book, mk_order(5, Side::SELL, 10200 * kTick, 15, 106, 6, 9));
    ASSERT_EQ(want.book.cancel_order(3), BookError::OK);
    ASSERT_EQ(want.book.apply_fill(want.book.find_order(1), 30), BookError::OK);
    const char* v = nullptr;
    ASSERT_TRUE(want.book.validate(&v)) << (v ? v : "");

    // WAL mirroring every op (explicit seq/ts — replay stamps identically).
    const Order o1 = mk_order(1, Side::BUY, 10000 * kTick, 50, 100, 0, 7);
    const Order o2 = mk_order(2, Side::BUY, 10000 * kTick, 30, 101, 1, 8);
    const Order o3 = mk_order(3, Side::BUY, 9900 * kTick, 10, 102, 2, 7);
    const Order o4 = mk_order(4, Side::SELL, 10100 * kTick, 20, 103, 3, 9);
    const Order o5 = mk_order(5, Side::SELL, 10200 * kTick, 15, 106, 6, 9);
    const WalOrderNewPayload n1 = p_new(o1);
    const WalOrderNewPayload n2 = p_new(o2);
    const WalOrderNewPayload n3 = p_new(o3);
    const WalOrderNewPayload n4 = p_new(o4);
    const WalOrderNewPayload n5 = p_new(o5);
    const WalTradePayload t1 = p_trade(500, 1, 999, 10000 * kTick, 20);
    const WalOrderModifyPayload m2 = p_modify(2, 10000 * kTick, 40);
    const WalOrderCancelPayload c3 = p_cancel(3);
    const WalTradePayload t2 = p_trade(501, 1, 998, 10000 * kTick, 30);
    write_wal(wpath.string(), kShard,
              {
                  {0, 100, WalEventType::ORDER_NEW, &n1, sizeof(n1)},
                  {1, 101, WalEventType::ORDER_NEW, &n2, sizeof(n2)},
                  {2, 102, WalEventType::ORDER_NEW, &n3, sizeof(n3)},
                  {3, 103, WalEventType::ORDER_NEW, &n4, sizeof(n4)},
                  {4, 104, WalEventType::TRADE, &t1, sizeof(t1)},
                  {5, 105, WalEventType::ORDER_MODIFY, &m2, sizeof(m2)},
                  {6, 106, WalEventType::ORDER_NEW, &n5, sizeof(n5)},
                  {7, 107, WalEventType::ORDER_CANCEL, &c3, sizeof(c3)},
                  {8, 108, WalEventType::TRADE, &t2, sizeof(t2)},
              });

    Fixture got;
    RecoveryManager rm(kShard);
    const RecoveryResult res = rm.recover(dir.string(), {{kIid, &got.book}});
    ASSERT_EQ(res.status, RecoveryStatus::Ok) << res.detail;
    EXPECT_TRUE(got.book.validate(&v)) << (v ? v : "");

    // Byte-identical serialized state == identical order ids, quantities,
    // filled quantities, prices, and FIFO order (the blob carries all).
    EXPECT_EQ(fingerprint(got.book), fingerprint(want.book));

    // FIFO order spot-check: level 10000 holds only order 2 (order 1 filled
    // out at seq 8); order 2 re-entered at the tail via the qty-up amend.
    const PriceLevel* lvl = got.book.level(Side::BUY, 0);
    ASSERT_NE(lvl, nullptr);
    EXPECT_EQ(lvl->price_ticks, 10000 * kTick);
    ASSERT_NE(lvl->head, nullptr);
    EXPECT_EQ(lvl->head->id, 2u);
    EXPECT_EQ(lvl->head->qty_units, 40);
    EXPECT_EQ(lvl->head->next, nullptr);
}

TEST(RecoveryManager, DoubleReplayIsByteIdentical) {
    const auto dir = tmp_dir("det_replay");
    const auto wpath = dir / "0.wal";
    const Order o1 = mk_order(1, Side::BUY, 10000 * kTick, 50, 100, 0);
    const Order o4 = mk_order(4, Side::SELL, 10100 * kTick, 20, 103, 3);
    const WalOrderNewPayload n1 = p_new(o1);
    const WalOrderNewPayload n4 = p_new(o4);
    const WalTradePayload t1 = p_trade(500, 1, 999, 10000 * kTick, 20);
    write_wal(wpath.string(), kShard,
              {
                  {0, 100, WalEventType::ORDER_NEW, &n1, sizeof(n1)},
                  {1, 103, WalEventType::ORDER_NEW, &n4, sizeof(n4)},
                  {2, 104, WalEventType::TRADE, &t1, sizeof(t1)},
              });

    Fixture a, b;
    RecoveryManager rm1(kShard), rm2(kShard);
    const RecoveryResult r1 = rm1.recover(dir.string(), {{kIid, &a.book}});
    const RecoveryResult r2 = rm2.recover(dir.string(), {{kIid, &b.book}});
    ASSERT_TRUE(r1.ok() && r2.ok());
    // Replaying the identical WAL twice must produce byte-identical state.
    EXPECT_EQ(fingerprint(a.book), fingerprint(b.book));
    EXPECT_EQ(r1.wal_tail, r2.wal_tail);
    EXPECT_EQ(r1.mutations_applied, r2.mutations_applied);
}

TEST(RecoveryManager, ForeignInstrumentEntriesCountedNotApplied) {
    const auto dir = tmp_dir("foreign");
    const auto wpath = dir / "0.wal";
    const Order o1 = mk_order(1, Side::BUY, 10000 * kTick, 50, 100, 0);
    const WalOrderNewPayload n1 = p_new(o1, /*iid*/ 7);
    const Order oF = mk_order(55, Side::BUY, 9900 * kTick, 5, 101, 1);
    const WalOrderNewPayload nF = p_new(oF, /*iid*/ 42);  // unbound instrument
    write_wal(wpath.string(), kShard,
              {
                  {0, 100, WalEventType::ORDER_NEW, &n1, sizeof(n1)},
                  {1, 101, WalEventType::ORDER_NEW, &nF, sizeof(nF)},
              });

    Fixture f;
    RecoveryManager rm(kShard);  // only book 7 bound
    const RecoveryResult res = rm.recover(dir.string(), {{kIid, &f.book}});
    ASSERT_EQ(res.status, RecoveryStatus::Ok) << res.detail;
    EXPECT_EQ(res.foreign_entries, 1u);
    EXPECT_EQ(res.mutations_applied, 1u);
    ASSERT_EQ(f.book.live_orders(), 1u);
    EXPECT_NE(f.book.find_order(1), nullptr);
    EXPECT_EQ(f.book.find_order(55), nullptr);
}
