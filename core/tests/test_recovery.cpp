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
#include "matching/MatchingEngine.hpp"
#include "matching/WalWriter.hpp"
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

// Order-type wire code — must match MatchingEngine::write_order_new
// (MARKET=0, LIMIT=1, STOP=2, STOP_LIMIT=3, ICEBERG=250); the enum
// underlying values differ (LIMIT=0/MARKET=1), so a raw enum cast
// would mis-encode the journal.
uint8_t wire_type(OrderType t) {
    switch (t) {
        case OrderType::MARKET:     return 0;
        case OrderType::LIMIT:      return 1;
        case OrderType::STOP:       return 2;
        case OrderType::STOP_LIMIT: return 3;
        case OrderType::ICEBERG:    return kWalOrderTypeIceberg;
        default:                    return 1;  // LIMIT convention
    }
}

WalOrderNewPayload p_new(const Order& o, uint32_t iid = kIid) {
    WalOrderNewPayload p{};
    p.order_id = o.id;
    p.account_id = o.account_id;
    p.instrument_id = iid;
    p.side = static_cast<uint8_t>(o.side);
    p.type = wire_type(o.type);
    p.tif = static_cast<uint8_t>(o.tif);
    p.flags = o.flags;
    p.price_ticks = o.price_ticks;
    p.qty_units = o.qty_units;
    p.visible_qty_units = o.qty_units;  // wire: == qty when non-iceberg
    p.stp_mode = static_cast<uint32_t>(o.stp_mode);
    return p;
}

// account_id must match the order's account — the replay engine enforces
// the same auth check as live cancel_internal (spec §3.5 replay fidelity).
WalOrderCancelPayload p_cancel(uint64_t id, uint8_t reason = 0,
                               uint64_t acct = 42) {
    WalOrderCancelPayload p{};
    p.order_id = id;
    p.account_id = acct;
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

    // Snapshot-only boot is forward divergence (snapshot_seq=10 > wal_tail=0
    // on the empty stream): strict recover() must fail closed at Phase-3
    // before any book mutation — silently booting at tail 0 under a seq-10
    // snapshot would let post-boot journaling collide with the covered seq
    // domain (latent zero-loss).
    {
        Fixture strict_f;
        RecoveryManager strict_rm(kShard, sink);
        const RecoveryResult sres = strict_rm.recover(
            wal_dir.string(), {{kIid, &strict_f.book}});
        ASSERT_EQ(sres.status, RecoveryStatus::InvariantViolated);
        EXPECT_NE(std::string(sres.detail).find("ahead of WAL tail"),
                  std::string::npos);
        EXPECT_EQ(strict_f.book.live_orders(), 0u);  // no book mutation
    }

    // The graduated ladder resolves it: writes the {10}.wal rebase-marker
    // segment (anchors the seq domain at the snapshot cursor) and rebases
    // in place at level 2 — no halt needed.
    Fixture dst;
    RecoveryManager rm(kShard, sink);
    const auto report_path = root / "reports.jsonl";
    const RecoveryLadderResult lad = rm.recover_ladder(
        wal_dir.string(), {{kIid, &dst.book}}, report_path.string());
    ASSERT_EQ(lad.outcome, RecoveryOutcome::SNAPSHOT_REBASED)
        << lad.result.detail;
    EXPECT_EQ(lad.level, 2u);
    EXPECT_TRUE(lad.rebase_marker_written);
    EXPECT_TRUE(std::filesystem::exists(wal_dir / "10.wal"));
    EXPECT_TRUE(lad.report_written);
    ASSERT_EQ(lad.result.books.size(), 1u);
    EXPECT_TRUE(lad.result.books[0].snapshot_loaded);
    EXPECT_EQ(lad.result.books[0].snapshot_seq, 10u);
    EXPECT_EQ(lad.result.books[0].orders_restored, 3u);
    EXPECT_TRUE(lad.result.books[0].book_seq_verified);
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
    const WalOrderCancelPayload c3 = p_cancel(3, 0, /*acct*/ 7);
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
    const RecoveryResult res = rm.recover(dir.string(), {{kIid, &got.book, &got.pool}});
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
    const RecoveryResult r1 = rm1.recover(dir.string(), {{kIid, &a.book, &a.pool}});
    const RecoveryResult r2 = rm2.recover(dir.string(), {{kIid, &b.book, &b.pool}});
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

namespace {

// Structural book state: per-side level chain -> (price, [(id, remaining,
// filled)]). Replay stamps (timestamp_ns/ingress_seq) intentionally differ
// from a live book's, so equality is defined on the resting-state contract.
struct OrderLine {
    uint64_t id;
    int64_t remaining;
    int64_t filled;
    bool operator==(const OrderLine&) const = default;
};
std::vector<std::tuple<Side, int64_t, std::vector<OrderLine>>>
book_state(const OrderBook& b) {
    std::vector<std::tuple<Side, int64_t, std::vector<OrderLine>>> out;
    for (Side s : {Side::BUY, Side::SELL}) {
        for (std::size_t d = 0;; ++d) {
            const PriceLevel* lvl = b.level(s, d);
            if (lvl == nullptr) break;
            std::vector<OrderLine> lines;
            for (const Order* o = lvl->head; o != nullptr; o = o->next) {
                lines.push_back(
                    {o->id, remaining_qty_units(*o), o->filled_qty_units});
            }
            out.emplace_back(s, lvl->price_ticks, std::move(lines));
        }
    }
    return out;
}

}  // namespace

TEST(RecoveryManager, LiveWalWithMarketableTakersReplays) {
    // The Phase-02 replay gap, end-to-end: the engine journals ORDER_NEW at
    // admission, so a marketable taker's NEW precedes its TRADE tail and
    // naive reinsertion reports CROSSED. Deferred-taker replay must converge
    // to the live book's resting state.
    const auto dir = tmp_dir("live_taker_flow");
    const auto wpath = dir / "0.wal";

    MemoryPool<Order> live_pool{512};
    OrderBook live_book{live_pool};
    Instrument instr;
    instr.instrument_id = kIid;
    instr.pip_factor = 10;
    instr.pip_size_ticks = 10'000;
    instr.tick_size_ticks = 1;
    instr.lot_size_units = 1;
    live_book.set_instrument(instr);
    Wal wal(wpath.string(), kShard);
    ASSERT_EQ(wal.open(), WalStatus::Ok);
    WalWriter ww(&wal);
    MatchingEngine eng(kShard, live_book, live_pool, &ww, nullptr);
    OrderAux aux{};
    aux.instrument_id = kIid;

    auto send = [&](uint64_t id, Side side, int64_t px, int64_t qty,
                    TimeInForce tif = TimeInForce::GTC, uint64_t acct = 1) {
        Order* o = live_pool.alloc();
        ASSERT_NE(o, nullptr);
        *o = mk_order(id, side, px, qty, /*ts*/ 100 + id, /*seq*/ id, acct);
        o->tif = tif;
        eng.on_order_received(o, aux);
    };

    send(1, Side::SELL, 10100, 20, TimeInForce::GTC, 9);  // maker
    send(2, Side::BUY, 10100, 30, TimeInForce::GTC, 8);   // fills 20, rests 10
    send(3, Side::SELL, 10200, 15, TimeInForce::GTC, 9);  // deeper maker
    send(4, Side::BUY, 10300, 25, TimeInForce::GTC, 8);   // fills 15, rests 10
    send(5, Side::BUY, 10200, 5, TimeInForce::IOC, 8);    // no liquidity -> dead
    send(6, Side::SELL, 9900, 5, TimeInForce::GTC, 7);    // sweeps best bid 10300
    eng.on_time_tick(9'999);
    wal.close();
    const char* v = nullptr;
    ASSERT_TRUE(live_book.validate(&v)) << (v ? v : "");

    Fixture got;
    RecoveryManager rm(kShard);
    const RecoveryResult res = rm.recover(dir.string(), {{kIid, &got.book, &got.pool}});
    ASSERT_EQ(res.status, RecoveryStatus::Ok) << res.detail;
    EXPECT_TRUE(got.book.validate(&v)) << (v ? v : "");

    // Resting-state equality: same levels, same FIFO order, same remaining
    // and filled quantities — the gap previously failed at replayed NEW id2.
    EXPECT_EQ(book_state(got.book), book_state(live_book));

    // Sanity on the scenario itself.
    const Order* o2 = got.book.find_order(2);
    ASSERT_NE(o2, nullptr);
    EXPECT_EQ(o2->price_ticks, 10100);
    EXPECT_EQ(remaining_qty_units(*o2), 10);
    const Order* o4 = got.book.find_order(4);
    ASSERT_NE(o4, nullptr);
    EXPECT_EQ(remaining_qty_units(*o4), 5);  // 10 rested - 5 sold to id6
}

TEST(RecoveryManager, DeferredTakerRestingAcrossMidStream) {
    // Priority ordering: a deferred taker's remainder must sit at its level
    // BEFORE a later same-price resting order — materialization order is
    // WAL order, not stream-end order.
    const auto dir = tmp_dir("pending_fifo");
    const auto wpath = dir / "0.wal";

    MemoryPool<Order> live_pool{512};
    OrderBook live_book{live_pool};
    Instrument instr;
    instr.instrument_id = kIid;
    instr.pip_factor = 10;
    instr.pip_size_ticks = 10'000;
    instr.tick_size_ticks = 1;
    instr.lot_size_units = 1;
    live_book.set_instrument(instr);
    Wal wal(wpath.string(), kShard);
    ASSERT_EQ(wal.open(), WalStatus::Ok);
    WalWriter ww(&wal);
    MatchingEngine eng(kShard, live_book, live_pool, &ww, nullptr);
    OrderAux aux{};
    aux.instrument_id = kIid;

    auto send = [&](uint64_t id, Side side, int64_t px, int64_t qty,
                    uint64_t acct = 1) {
        Order* o = live_pool.alloc();
        ASSERT_NE(o, nullptr);
        *o = mk_order(id, side, px, qty, 100 + id, id, acct);
        eng.on_order_received(o, aux);
    };

    send(1, Side::SELL, 10100, 10, 9);   // maker
    send(2, Side::BUY, 10100, 30, 8);    // fills 10, rests 20 @10100 (deferred)
    send(3, Side::BUY, 10100, 7, 9);     // later same-level rest — behind id2
    wal.close();

    Fixture got;
    RecoveryManager rm(kShard);
    const RecoveryResult res = rm.recover(dir.string(), {{kIid, &got.book, &got.pool}});
    ASSERT_EQ(res.status, RecoveryStatus::Ok) << res.detail;
    EXPECT_EQ(book_state(got.book), book_state(live_book));

    const PriceLevel* lvl = got.book.level(Side::BUY, 0);
    ASSERT_NE(lvl, nullptr);
    ASSERT_NE(lvl->head, nullptr);
    EXPECT_EQ(lvl->head->id, 2u);               // deferred taker keeps head
    EXPECT_EQ(lvl->head->next->id, 3u);
}

TEST(RecoveryManager, LiveWalFullFlowReplaysByteIdentical) {
    // Engine-driven replay end-to-end: a live MatchingEngine journals a
    // WAL containing real trades, a partial-fill-then-rest taker, IOC and
    // MARKET remainders, a pending stop whose amended trigger drains on a
    // tick, a GTD expiry, an iceberg slice refresh, a user cancel and an
    // amend. Recovery into a fresh book re-derives all of it through a
    // journal-free MatchingEngine and must reproduce the live resting
    // state byte-for-byte — replay feeds the same (ts, seq) stamps the
    // journal envelope records.
    const auto dir = tmp_dir("live_full_flow");
    const auto wpath = dir / "0.wal";

    MemoryPool<Order> live_pool{1024};
    OrderBook live_book{live_pool};
    Instrument instr{};
    instr.instrument_id = kIid;
    instr.pip_factor = 10;
    instr.pip_size_ticks = 10'000;
    instr.tick_size_ticks = 1;
    instr.lot_size_units = 1;
    live_book.set_instrument(instr);
    Wal wal(wpath.string(), kShard);
    ASSERT_EQ(wal.open(), WalStatus::Ok);
    WalWriter ww(&wal);
    MatchingEngine eng(kShard, live_book, live_pool, &ww, nullptr);

    // Ingress stamps replay can reproduce exactly: the ORDER_NEW journal
    // entry records (seq = tail_seq, ts = engine now_ns_) at append time.
    auto send = [&](uint64_t id, Side side, OrderType type, int64_t px,
                    int64_t qty, uint64_t acct,
                    TimeInForce tif = TimeInForce::GTC, int64_t stop = 0,
                    int64_t gtd = 0, int64_t disp = 0) {
        Order* o = live_pool.alloc();
        ASSERT_NE(o, nullptr);
        *o = Order{};
        o->id = id;
        o->account_id = acct;
        o->side = side;
        o->type = type;
        o->tif = tif;
        o->stp_mode = StpMode::CANCEL_NEWEST;
        o->flags = 0;
        o->price_ticks = px;
        o->qty_units = qty;
        o->display_qty_units = disp;
        o->quantity = Decimal::from_mantissa(qty);
        o->timestamp_ns = eng.now_ns();
        o->ingress_seq = wal.tail_seq();
        OrderAux aux{};
        aux.stop_price_ticks = stop;
        aux.gtd_expiry_ns = gtd;
        aux.instrument_id = kIid;
        eng.on_order_received_ex(o, aux);
    };

    eng.on_time_tick(1'000);
    send(1, Side::SELL, OrderType::LIMIT, 10100, 40, 7);
    send(2, Side::SELL, OrderType::LIMIT, 10200, 30, 7);
    send(3, Side::BUY,  OrderType::LIMIT, 9900,  15, 8);
    eng.on_time_tick(1'100);
    // Marketable taker: consumes the 10100 maker fully + 10 @10200, dies.
    send(4, Side::BUY, OrderType::LIMIT, 10200, 50, 8);
    // Partial-fill-then-rest taker: fills the 10200 remainder, rests 15.
    send(5, Side::BUY, OrderType::LIMIT, 10200, 35, 8);
    // IOC taker: sweeps both bids, cancels the unfilled remainder.
    send(6, Side::SELL, OrderType::LIMIT, 10000, 55, 9, TimeInForce::IOC);
    // Fresh ask + MARKET taker (no slippage protection configured).
    send(7, Side::SELL, OrderType::LIMIT, 10350, 25, 7);
    send(8, Side::BUY, OrderType::MARKET, 0, 10, 8);
    // Pending stop: amended trigger, then drained by the next tick.
    send(9, Side::BUY, OrderType::STOP_LIMIT, 10500, 5, 8,
         TimeInForce::GTC, /*stop*/ 10400);
    eng.on_amend_received(9, /*px*/ 0, /*qty*/ 0,
                          /*stop*/ 10350, wal.tail_seq());
    eng.on_time_tick(2'200);   // drains due stop -> limit buy fills @10350
    // GTD expiry through a real TIME_TICK.
    send(10, Side::SELL, OrderType::LIMIT, 10400, 10, 7,
         TimeInForce::GTD, 0, /*gtd*/ 2'500);
    eng.on_time_tick(3'000);   // expires id10 -> journaled cancel
    // Iceberg + slice refresh under a crossing taker.
    send(11, Side::SELL, OrderType::ICEBERG, 10500, 100, 7,
         TimeInForce::GTC, 0, 0, /*disp*/ 25);
    send(12, Side::BUY, OrderType::LIMIT, 10500, 40, 8);  // 25 + 15 vs slices
    // User cancel kills the live slice + hidden iceberg record.
    eng.on_cancel_received(11, 7);
    // Amend a resting order (qty-down keeps priority + stamps).
    send(13, Side::BUY, OrderType::LIMIT, 9800, 30, 8);
    eng.on_amend_received(13, /*px*/ 0, /*qty*/ 20,
                          /*stop*/ 0, wal.tail_seq());
    wal.close();

    const char* v = nullptr;
    ASSERT_TRUE(live_book.validate(&v)) << (v ? v : "");

    Fixture got;
    got.book.set_instrument(instr);
    RecoveryManager rm(kShard);
    const RecoveryResult res = rm.recover(dir.string(), {{kIid, &got.book, &got.pool}});
    ASSERT_EQ(res.status, RecoveryStatus::Ok) << res.detail;
    ASSERT_EQ(res.books.size(), 1u);
    EXPECT_TRUE(res.books[0].book_seq_verified);
    EXPECT_TRUE(got.book.validate(&v)) << (v ? v : "");

    // Byte-identical resting state: ids, prices, quantities, filled
    // quantities, display slices, (ts, seq) stamps and FIFO order.
    EXPECT_EQ(fingerprint(got.book), fingerprint(live_book));

    // Trade invariants: every emitted fill was journaled, every journaled
    // fill was re-derived — zero duplicates and zero missing.
    EXPECT_EQ(res.trades_applied, 0u);
    EXPECT_EQ(res.trades_derived, eng.trades_emitted());
    EXPECT_EQ(res.books[0].trades_derived, eng.trades_emitted());
    EXPECT_EQ(res.books[0].engine_trades_emitted, eng.trades_emitted());
}

TEST(RecoveryManager, SnapshotPlusLiveWalTailReplaysThroughEngine) {
    // Snapshot mid-run, then continue trading: tail entries replay through
    // the engine on top of the restored image — trades and partial rests
    // after the boundary are re-derived, covered entries are skipped.
    const auto root = tmp_dir("snap_live_tail");
    const auto wal_dir = root / "wal";
    std::filesystem::create_directories(wal_dir);
    const auto wpath = wal_dir / "0.wal";

    MemoryPool<Order> live_pool{1024};
    OrderBook live_book{live_pool};
    Wal wal(wpath.string(), kShard);
    ASSERT_EQ(wal.open(), WalStatus::Ok);
    WalWriter ww(&wal);
    MatchingEngine eng(kShard, live_book, live_pool, &ww, nullptr);
    OrderAux aux{};
    aux.instrument_id = kIid;

    auto send = [&](uint64_t id, Side side, int64_t px, int64_t qty,
                    uint64_t acct) {
        Order* o = live_pool.alloc();
        ASSERT_NE(o, nullptr);
        *o = mk_order(id, side, px, qty, eng.now_ns(), wal.tail_seq(), acct);
        eng.on_order_received(o, aux);
    };

    send(1, Side::SELL, 10100, 40, 7);
    send(2, Side::BUY, 10000, 30, 8);
    const uint64_t snap_seq = wal.tail_seq();  // snapshot covers seqs 0,1
    {
        std::vector<uint8_t> blob;
        WalBookSnapshotHeader hdr{};
        ASSERT_TRUE(SnapshotStore::serialize_book(live_book, kIid, snap_seq,
                                                blob, hdr));
        FileSnapshotSink sink(root.string(), kShard);
        ASSERT_TRUE(sink.store(kIid, snap_seq, blob.data(), blob.size()));
    }
    // Post-boundary flow the engine must re-derive rather than re-apply.
    send(3, Side::BUY, 10100, 50, 8);   // fills 40 @10100, rests 10
    send(4, Side::SELL, 10000, 20, 7);  // fills 10 @10100 + 10 @10000
    send(5, Side::BUY, 9900, 10, 8);    // plain rest
    send(6, Side::BUY, 9850, 7, 8);     // plain rest
    eng.on_amend_received(5, /*px*/ 0, /*qty*/ 5,
                          /*stop*/ 0, wal.tail_seq());
    eng.on_cancel_received(2, 8);
    wal.close();

    Fixture dst;
    FileSnapshotSink sink(root.string(), kShard);
    RecoveryManager rm(kShard, sink);
    const RecoveryResult res = rm.recover(wal_dir.string(),
                                          {{kIid, &dst.book, &dst.pool}});
    ASSERT_EQ(res.status, RecoveryStatus::Ok) << res.detail;
    ASSERT_EQ(res.books.size(), 1u);
    EXPECT_TRUE(res.books[0].snapshot_loaded);
    EXPECT_EQ(res.books[0].snapshot_seq, snap_seq);
    EXPECT_EQ(res.books[0].covered_skips, snap_seq);
    EXPECT_TRUE(res.books[0].book_seq_verified);
    EXPECT_EQ(res.trades_derived, eng.trades_emitted());
    EXPECT_EQ(res.trades_applied, 0u);
    EXPECT_EQ(fingerprint(dst.book), fingerprint(live_book));
}

TEST(RecoveryManager, DerivedAndOrphanTradesCoexist) {
    // A journaled fill the replay engine re-derives (the aggressor's
    // ORDER_NEW is in the stream) and an orphan fill (the aggressor's NEW
    // is outside the scanned stream) must each land exactly once.
    const auto dir = tmp_dir("mixed_trades");
    const auto wpath = dir / "0.wal";

    const Order o1 = mk_order(1, Side::BUY, 10000 * kTick, 50, 100, 0);
    Order oioc = mk_order(2, Side::SELL, 9990 * kTick, 20, 101, 1, 8);
    oioc.tif = TimeInForce::IOC;
    const WalOrderNewPayload n1 = p_new(o1);
    const WalOrderNewPayload n2 = p_new(oioc);
    const WalTradePayload t1 = p_trade(500, /*buy*/ 1, /*sell*/ 2,
                                     10000 * kTick, 20);
    const WalTradePayload t2 = p_trade(501, /*buy*/ 1, /*sell*/ 999,
                                     10000 * kTick, 10);
    write_wal(wpath.string(), kShard,
              {
                  {0, 100, WalEventType::ORDER_NEW, &n1, sizeof(n1)},
                  {1, 101, WalEventType::ORDER_NEW, &n2, sizeof(n2)},
                  {2, 102, WalEventType::TRADE, &t1, sizeof(t1)},
                  {3, 103, WalEventType::TRADE, &t2, sizeof(t2)},
              });

    Fixture f;
    RecoveryManager rm(kShard);
    const RecoveryResult res =
        rm.recover(dir.string(), {{kIid, &f.book, &f.pool}});
    ASSERT_EQ(res.status, RecoveryStatus::Ok) << res.detail;
    EXPECT_EQ(res.trades_derived, 1u);   // t1 re-derived by order 2's walk
    EXPECT_EQ(res.trades_applied, 1u);   // t2 orphan-applied to the maker
    const Order* r = f.book.find_order(1);
    ASSERT_NE(r, nullptr);
    EXPECT_EQ(r->filled_qty_units, 30);  // 20 derived + 10 orphan, once each
    EXPECT_EQ(remaining_qty_units(*r), 20);
}

TEST(RecoveryManager, RecoveredNodesStayUsableAfterRecoverReturns) {
    // Ownership contract: every resting node in the recovered book is a
    // book-pool clone (engine insert paths copy the template), so the book
    // must remain fully mutable AFTER recover() returns — the replay
    // engine and its scratch pool may already be gone. Verify by mutating
    // the recovered book: cancel a replayed resting order (writes to its
    // node + pushes it onto the bound pool's freelist) and reuse the slot
    // via a fresh add_order.
    const auto dir = tmp_dir("pool_lifetime");
    const auto wpath = dir / "0.wal";

    const Order o1 = mk_order(1, Side::SELL, 10100 * kTick, 40, 100, 0, 7);
    const Order o2 = mk_order(2, Side::BUY, 10100 * kTick, 60, 101, 1, 8);
    const WalOrderNewPayload n1 = p_new(o1);
    const WalOrderNewPayload n2 = p_new(o2);
    const WalTradePayload t1 = p_trade(700, /*buy*/ 2, /*sell*/ 1,
                                     10100 * kTick, 40);
    write_wal(wpath.string(), kShard,
              {
                  {0, 100, WalEventType::ORDER_NEW, &n1, sizeof(n1)},
                  {1, 101, WalEventType::ORDER_NEW, &n2, sizeof(n2)},
                  {2, 102, WalEventType::TRADE, &t1, sizeof(t1)},
              });

    Fixture f;
    // No orders in the binding → the adopted-arena path: the manager must
    // retain the arena for as long as the recovered book is used.
    RecoveryManager rm(kShard);
    const RecoveryResult res = rm.recover(dir.string(), {{kIid, &f.book}});
    ASSERT_EQ(res.status, RecoveryStatus::Ok) << res.detail;

    // Order 2 is a replayed engine-adopted node resting at 10100 with 20
    // left. Post-recovery mutation must touch valid memory.
    const Order* r = f.book.find_order(2);
    ASSERT_NE(r, nullptr);
    EXPECT_EQ(remaining_qty_units(*r), 20);
    EXPECT_EQ(f.book.cancel_order(2), BookError::OK);
    EXPECT_EQ(f.book.find_order(2), nullptr);

    // Reuse the freed slot through the book's bound pool: a new resting
    // order at the same level must link and validate cleanly.
    Order t = mk_order(9, Side::BUY, 10000 * kTick, 5, 200, 3, 8);
    Order* out = nullptr;
    EXPECT_EQ(f.book.add_order(t, &out), BookError::OK);
    EXPECT_NE(out, nullptr);
    const char* v = nullptr;
    EXPECT_TRUE(f.book.validate(&v)) << (v ? v : "");
    EXPECT_NE(f.book.find_order(9), nullptr);
}

TEST(RecoveryManager, PreventedMatchRecordIsAuditNoop) {
    const auto dir = tmp_dir("prevented_match");
    const auto wpath = dir / "0.wal";
    const Order o1 = mk_order(1, Side::BUY, 10000 * kTick, 50, 100, 0);
    const Order o4 = mk_order(4, Side::SELL, 10100 * kTick, 20, 103, 3);
    const WalOrderNewPayload n1 = p_new(o1);
    const WalOrderNewPayload n4 = p_new(o4);
    WalPreventedMatchPayload pm{};
    pm.maker_order_id = 1;
    pm.taker_order_id = 55;         // never journaled — synthetic audit row
    pm.maker_account_id = 42;
    pm.taker_account_id = 42;
    pm.price_ticks = 10000 * kTick;
    pm.maker_prevented_qty_units = 10;
    pm.taker_prevented_qty_units = 10;
    pm.prevented_notional_units = 1000;
    pm.trade_group_id = 77;
    pm.mode = 5;                    // TRANSFER
    pm.ts_ns = 102;
    write_wal(wpath.string(), kShard,
              {
                  {0, 100, WalEventType::ORDER_NEW, &n1, sizeof(n1)},
                  {1, 102, WalEventType::PREVENTED_MATCH, &pm, sizeof(pm)},
                  {2, 103, WalEventType::ORDER_NEW, &n4, sizeof(n4)},
              });

    Fixture f;
    RecoveryManager rm(kShard);
    const RecoveryResult res = rm.recover(dir.string(), {{kIid, &f.book}});
    ASSERT_EQ(res.status, RecoveryStatus::Ok) << res.detail;
    EXPECT_EQ(res.audit_events, 1u);    // explicitly classified, no book op
    EXPECT_EQ(res.shard_events, 0u);
    EXPECT_EQ(res.mutations_applied, 2u);
    ASSERT_EQ(f.book.live_orders(), 2u);
    EXPECT_NE(f.book.find_order(1), nullptr);
    EXPECT_NE(f.book.find_order(4), nullptr);
}

TEST(RecoveryManager, PreventedMatchTruncatedPayloadFailsClosed) {
    const auto dir = tmp_dir("pm_badsize");
    const auto wpath = dir / "0.wal";
    const Order o1 = mk_order(1, Side::BUY, 10000 * kTick, 50, 100, 0);
    const WalOrderNewPayload n1 = p_new(o1);
    WalPreventedMatchPayload pm{};
    write_wal(wpath.string(), kShard,
              {
                  {0, 100, WalEventType::ORDER_NEW, &n1, sizeof(n1)},
                  // Truncated payload — the pinned size contract must
                  // reject the entry before any dispatch touches it.
                  {1, 101, WalEventType::PREVENTED_MATCH, &pm,
                   static_cast<uint32_t>(sizeof(pm) - 8)},
              });
    Fixture f;
    RecoveryManager rm(kShard);
    const RecoveryResult res = rm.recover(dir.string(), {{kIid, &f.book}});
    EXPECT_EQ(res.status, RecoveryStatus::InvariantViolated);
    EXPECT_EQ(res.outcome(), RecoveryOutcome::HALTED);
}

// --- Graduated recovery ladder (Task 4.3.5/4.3.9) -----------------------------

namespace {

std::string read_all(const std::filesystem::path& p) {
    std::ifstream ifs(p, std::ios::binary);
    return std::string(std::istreambuf_iterator<char>(ifs),
                       std::istreambuf_iterator<char>());
}

}  // namespace

TEST(RecoveryLadder, CleanBootWritesNoReport) {
    const auto dir = tmp_dir("ladder_clean");
    const auto wpath = dir / "0.wal";
    const Order o1 = mk_order(1, Side::BUY, 10000 * kTick, 50, 100, 0);
    const WalOrderNewPayload n1 = p_new(o1);
    write_wal(wpath.string(), kShard,
              {{0, 100, WalEventType::ORDER_NEW, &n1, sizeof(n1)}});
    const auto report_path = dir / "reports.jsonl";

    Fixture f;
    RecoveryManager rm(kShard);
    const RecoveryLadderResult lad = rm.recover_ladder(
        dir.string(), {{kIid, &f.book}}, report_path.string());
    ASSERT_EQ(lad.outcome, RecoveryOutcome::CLEAN);
    EXPECT_EQ(lad.level, 1u);
    EXPECT_TRUE(lad.result.ok());
    EXPECT_EQ(lad.result.wal_tail, 1u);
    EXPECT_FALSE(lad.report_written);  // nothing to report on a clean boot
    EXPECT_FALSE(std::filesystem::exists(report_path));
    EXPECT_EQ(f.book.live_orders(), 1u);
}

TEST(RecoveryLadder, CorruptTailRepairsAtLevel1) {
    const auto dir = tmp_dir("ladder_tail");
    const auto wpath = dir / "0.wal";
    const Order o1 = mk_order(1, Side::BUY, 10000 * kTick, 50, 100, 0);
    const WalOrderNewPayload n1 = p_new(o1);
    write_wal(wpath.string(), kShard,
              {{0, 100, WalEventType::ORDER_NEW, &n1, sizeof(n1)}});
    inject_torn_tail(wpath.string(), valid_end_of(wpath.string()));
    const auto report_path = dir / "reports.jsonl";

    Fixture f;
    RecoveryManager rm(kShard);
    const RecoveryLadderResult lad = rm.recover_ladder(
        dir.string(), {{kIid, &f.book}}, report_path.string());
    ASSERT_EQ(lad.outcome, RecoveryOutcome::WAL_REPAIRED) << lad.result.detail;
    EXPECT_EQ(lad.level, 1u);
    EXPECT_TRUE(lad.result.tail_truncated);
    EXPECT_EQ(lad.result.wal_tail, 1u);
    ASSERT_TRUE(lad.report_written);
    const std::string line = read_all(report_path);
    EXPECT_NE(line.find("\"outcome\":\"WAL_REPAIRED\""), std::string::npos);
    EXPECT_NE(line.find("\"stage\":\"boot_ladder\""), std::string::npos);
    EXPECT_NE(line.find("\"wal_tail\":1"), std::string::npos);
    EXPECT_EQ(f.book.live_orders(), 1u);
}

TEST(RecoveryLadder, CorruptLatestSnapshotRebasesAtLevel2) {
    const auto root = tmp_dir("ladder_snapcorrupt");
    const auto wal_dir = root / "wal";
    std::filesystem::create_directories(wal_dir);
    const auto wpath = wal_dir / "0.wal";

    const Order o1 = mk_order(1, Side::BUY, 10000 * kTick, 50, 100, 0);
    const Order o2 = mk_order(2, Side::BUY, 9900 * kTick, 10, 101, 1);
    const WalOrderNewPayload n1 = p_new(o1);
    const WalOrderNewPayload n2 = p_new(o2);
    write_wal(wpath.string(), kShard,
              {
                  {0, 100, WalEventType::ORDER_NEW, &n1, sizeof(n1)},
                  {1, 101, WalEventType::ORDER_NEW, &n2, sizeof(n2)},
              });

    // Corrupt the only snapshot generation — level 2 must fall back to
    // genesis replay (the stream is complete from seq 0, so it is legal).
    const auto snap_dir = root / "i7";
    std::filesystem::create_directories(snap_dir);
    {
        std::ofstream ofs(snap_dir / "snap_00000000000000000002.bin",
                          std::ios::binary);
        const std::string junk(64, '\xAB');
        ofs.write(junk.data(), static_cast<std::streamsize>(junk.size()));
    }
    const auto report_path = root / "reports.jsonl";

    Fixture f;
    FileSnapshotSink sink(root.string(), kShard);
    RecoveryManager rm(kShard, sink);
    const RecoveryLadderResult lad = rm.recover_ladder(
        wal_dir.string(), {{kIid, &f.book}}, report_path.string());
    ASSERT_EQ(lad.outcome, RecoveryOutcome::SNAPSHOT_REBASED)
        << lad.result.detail;
    EXPECT_EQ(lad.level, 2u);
    EXPECT_TRUE(lad.result.snapshot_fallback);
    EXPECT_EQ(lad.result.wal_tail, 2u);
    ASSERT_TRUE(lad.report_written);
    EXPECT_NE(read_all(report_path).find("\"outcome\":\"SNAPSHOT_REBASED\""),
              std::string::npos);
    EXPECT_EQ(f.book.live_orders(), 2u);
    EXPECT_NE(f.book.find_order(1), nullptr);
    EXPECT_NE(f.book.find_order(2), nullptr);
}

TEST(RecoveryLadder, ForwardDivergenceRebasesViaMarkerSegment) {
    const auto root = tmp_dir("ladder_fwd");
    const auto wal_dir = root / "wal";
    std::filesystem::create_directories(wal_dir);
    const auto wpath = wal_dir / "0.wal";

    const Order o1 = mk_order(1, Side::BUY, 10000 * kTick, 50, 100, 0);
    const WalOrderNewPayload n1 = p_new(o1);
    write_wal(wpath.string(), kShard,
              {{0, 100, WalEventType::ORDER_NEW, &n1, sizeof(n1)}});
    // WAL tail is 1 — the snapshot below claims coverage through seq 9
    // (entries seqs 1..8 were already snapshot-covered when the log was
    // trimmed: the rebase-marker segment resumes the seq space at 9).

    Fixture src;
    add(src.book, o1);
    std::vector<uint8_t> blob;
    WalBookSnapshotHeader hdr{};
    ASSERT_TRUE(SnapshotStore::serialize_book(src.book, kIid, /*seq*/ 9,
                                              blob, hdr));
    FileSnapshotSink sink(root.string(), kShard);
    ASSERT_TRUE(sink.store(kIid, 9, blob.data(), blob.size()));
    const auto report_path = root / "reports.jsonl";

    Fixture dst;
    RecoveryManager rm(kShard, sink);
    const RecoveryLadderResult lad = rm.recover_ladder(
        wal_dir.string(), {{kIid, &dst.book}}, report_path.string());
    ASSERT_EQ(lad.outcome, RecoveryOutcome::SNAPSHOT_REBASED)
        << lad.result.detail;
    EXPECT_EQ(lad.level, 2u);
    EXPECT_TRUE(lad.rebase_marker_written);
    // Marker segment anchors the seq space at snapshot_seq: tail = 9+1.
    EXPECT_EQ(lad.result.wal_tail, 10u);
    // Bounded prescan: segment 0.wal's span sits below every snapshot cursor
    // (next base 9 <= snapshot 9), so its nominal seq span covers seqs 1..8
    // without an entry walk — no lost range is ever materialised.
    EXPECT_EQ(lad.result.covered_gap_seqs, 0u);
    EXPECT_EQ(lad.result.prescan_segments_skipped, 1u);
    ASSERT_TRUE(std::filesystem::exists(wal_dir / "9.wal"));
    ASSERT_TRUE(lad.report_written);
    EXPECT_NE(read_all(report_path).find("\"outcome\":\"SNAPSHOT_REBASED\""),
              std::string::npos);
    // The book is the snapshot image — the marker entry mutates nothing.
    ASSERT_EQ(dst.book.live_orders(), 1u);
    EXPECT_EQ(fingerprint(dst.book), fingerprint(src.book));
}

TEST(RecoveryLadder, CoveredSealedCorruptionSkipsToCleanBoot) {
    const auto root = tmp_dir("ladder_covered");
    const auto wal_dir = root / "wal";
    std::filesystem::create_directories(wal_dir);
    const auto p0 = wal_dir / "0.wal";
    const auto p3 = wal_dir / "3.wal";

    const Order o1 = mk_order(1, Side::BUY, 10000 * kTick, 50, 100, 0);
    const Order o5 = mk_order(5, Side::SELL, 10100 * kTick, 20, 103, 3);
    const WalOrderNewPayload n1 = p_new(o1);
    const WalOrderNewPayload n5 = p_new(o5);
    write_wal(p0.string(), kShard,
              {{0, 100, WalEventType::ORDER_NEW, &n1, sizeof(n1)}});
    write_wal(p3.string(), kShard,
              {{3, 104, WalEventType::ORDER_NEW, &n5, sizeof(n5)}});
    // Sealed damage in 0.wal: seqs 1..2 are unverifiable — and with the
    // snapshot at seq 3 covering the whole span, bounded prescan never
    // walks those bytes: the filename contract pins 0.wal's nominal span
    // at [0,3) entirely below the coverage cursor. Boot is CLEAN — the
    // covered domain is unreachable through any bound cursor, so zero-loss
    // holds; integrity of covered sealed journal is the offline wal_audit
    // sweep's job (spec §27 remediation — bounded prescan trade-off).
    inject_torn_tail(p0.string(), valid_end_of(p0.string()));

    Fixture src;
    add(src.book, o1);  // snapshot covers through seq 3 (seqs 0..2 inside)
    std::vector<uint8_t> blob;
    WalBookSnapshotHeader hdr{};
    ASSERT_TRUE(SnapshotStore::serialize_book(src.book, kIid, /*seq*/ 3,
                                              blob, hdr));
    FileSnapshotSink sink(root.string(), kShard);
    ASSERT_TRUE(sink.store(kIid, 3, blob.data(), blob.size()));
    const auto report_path = root / "reports.jsonl";

    Fixture dst;
    RecoveryManager rm(kShard, sink);
    const RecoveryLadderResult lad = rm.recover_ladder(
        wal_dir.string(), {{kIid, &dst.book}}, report_path.string());
    ASSERT_EQ(lad.outcome, RecoveryOutcome::CLEAN) << lad.result.detail;
    EXPECT_EQ(lad.level, 1u);
    EXPECT_EQ(lad.result.prescan_segments_skipped, 1u);
    EXPECT_EQ(lad.result.wal_tail, 4u);
    // Snapshot order restored + seq-3 entry replayed.
    EXPECT_EQ(dst.book.live_orders(), 2u);
    EXPECT_NE(dst.book.find_order(1), nullptr);
    EXPECT_NE(dst.book.find_order(5), nullptr);
}

TEST(RecoveryLadder, UncoveredSealedCorruptionHaltsAndReports) {
    const auto dir = tmp_dir("ladder_halt");
    const auto p0 = dir / "0.wal";
    const auto p5 = dir / "5.wal";
    const Order o1 = mk_order(1, Side::BUY, 10000 * kTick, 50, 100, 0);
    const Order o5 = mk_order(5, Side::SELL, 10100 * kTick, 20, 103, 3);
    const WalOrderNewPayload n1 = p_new(o1);
    const WalOrderNewPayload n5 = p_new(o5);
    write_wal(p0.string(), kShard,
              {{0, 100, WalEventType::ORDER_NEW, &n1, sizeof(n1)}});
    write_wal(p5.string(), kShard,
              {{5, 104, WalEventType::ORDER_NEW, &n5, sizeof(n5)}});
    // Sealed damage hiding the [1,5) range — no snapshot coverage (no sink),
    // so neither repair nor rebase can prove the lost seqs. Fail closed.
    inject_torn_tail(p0.string(), valid_end_of(p0.string()));
    const auto report_path = dir / "reports.jsonl";

    Fixture f;
    RecoveryManager rm(kShard);  // no snapshot sink
    const RecoveryLadderResult lad = rm.recover_ladder(
        dir.string(), {{kIid, &f.book}}, report_path.string());
    ASSERT_EQ(lad.outcome, RecoveryOutcome::HALTED);
    EXPECT_EQ(lad.level, 3u);
    EXPECT_EQ(lad.result.status, RecoveryStatus::WalCorrupt);
    ASSERT_TRUE(lad.report_written);
    const std::string line = read_all(report_path);
    EXPECT_NE(line.find("\"outcome\":\"WAL_RECOVERY_HALT\""),
              std::string::npos);
    EXPECT_NE(line.find("\"stage\":\"boot_ladder\""), std::string::npos);
    EXPECT_NE(line.find("\"shard_id\":3"), std::string::npos);
    EXPECT_NE(line.find("\"first_divergent_seq\":5"), std::string::npos);
    EXPECT_NE(line.find("runbook"), std::string::npos);
    EXPECT_EQ(f.book.live_orders(), 0u);  // fail-closed — nothing applied
}
