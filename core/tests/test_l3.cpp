// Task 17.3.1 — L3 order-level market data (spec §11):
//   * ADD / MODIFY / CANCEL / FILL emission per journal site, maker+taker
//     legs, hidden + pegged + synthetic flags
//   * per-instrument sequence counters (gaps consumed on transport drops)
//   * exact WAL-seq correlation (journal-first, §24 #318 engine half)
//   * salted-FNV account pseudonym — raw ids never reach the wire
//   * bit-identical event stream under journal-free replay anchoring

#include <gtest/gtest.h>

#include <cstdio>
#include <cstring>
#include <string>
#include <unistd.h>
#include <vector>

#include "book/Order.hpp"
#include "book/OrderBook.hpp"
#include "exchange_generated.h"
#include "ipc/IpcChannel.hpp"
#include "ipc/L3Publisher.hpp"
#include "matching/MatchingEngine.hpp"
#include "matching/WalWriter.hpp"
#include "utils/MemoryPool.hpp"
#include "wal/Wal.hpp"

using namespace exch;

namespace {

constexpr int64_t P(int64_t v) { return v; }
uint64_t g_ts = 1'000'000'000;
int g_wal_ix = 0;

Order* mk(MemoryPool<Order>& pool, uint64_t id, Side side, OrderType type,
          int64_t price, int64_t qty, uint64_t acct = 1,
          TimeInForce tif = TimeInForce::GTC,
          StpMode stp = StpMode::CANCEL_NEWEST, uint8_t flags = 0) {
    Order* o = pool.alloc();
    if (o == nullptr) return nullptr;
    *o = Order{};
    o->id = id;
    o->account_id = acct;
    o->side = side;
    o->type = type;
    o->tif = tif;
    o->stp_mode = stp;
    o->flags = flags;
    o->price_ticks = price;
    o->qty_units = qty;
    o->quantity = Decimal::from_mantissa(qty);
    o->timestamp_ns = ++g_ts;
    o->ingress_seq = g_ts;
    return o;
}

// Captures every outbound frame for decode; can simulate backpressure.
struct CaptureChannel final : IpcChannel {
    bool open() noexcept override { return true; }
    void close() noexcept override {}
    bool is_open() const noexcept override { return true; }
    bool send(const void* p, uint32_t n) noexcept override {
        if (fail) return false;
        frames.emplace_back(static_cast<const uint8_t*>(p),
                            static_cast<const uint8_t*>(p) + n);
        return true;
    }
    int32_t poll(void*, uint32_t) noexcept override { return 0; }
    std::vector<std::vector<uint8_t>> frames;
    bool fail = false;
};

// Decoded L3 row for value comparisons.
struct L3Rec {
    uint32_t instrument_id;
    uint8_t kind;
    uint64_t order_id;
    uint64_t account_hash;
    uint8_t side;
    int64_t price;
    int64_t ref_price;
    int64_t qty;
    int64_t qty_delta;
    uint64_t seq;
    uint64_t wal_seq;
    uint64_t trade_id;
    uint8_t fill_role;
    uint8_t flags;
    uint8_t cancel_reason;
    uint64_t ts;

    bool operator==(const L3Rec& o) const {
        return std::memcmp(this, &o, sizeof(L3Rec)) == 0;
    }
};

std::vector<L3Rec> decode_l3(const CaptureChannel& ch) {
    std::vector<L3Rec> out;
    for (const auto& f : ch.frames) {
        const auto* ev = exc::wire::GetEvent(f.data());
        if (ev == nullptr ||
            ev->type_type() != exc::wire::EventType_L3OrderEvent) {
            continue;  // non-L3 frames (L2 fills/depth) share the channel
        }
        const auto* t = ev->type_as_L3OrderEvent();
        L3Rec r{};
        r.instrument_id = t->instrument_id();
        r.kind = static_cast<uint8_t>(t->kind());
        r.order_id = t->order_id();
        r.account_hash = t->account_hash();
        r.side = static_cast<uint8_t>(t->side());
        r.price = t->price();
        r.ref_price = t->ref_price();
        r.qty = t->qty();
        r.qty_delta = t->qty_delta();
        r.seq = t->seq();
        r.wal_seq = t->wal_seq();
        r.trade_id = t->trade_id();
        r.fill_role = t->fill_role();
        r.flags = t->flags();
        r.cancel_reason = t->cancel_reason();
        r.ts = ev->ts();
        out.push_back(r);
    }
    return out;
}

// Live fixture: real WAL file (exact seq correlation) + L3 capture.
struct LiveFix {
    LiveFix()
        : path("/tmp/exc_l3_" + std::to_string(::getpid()) + "_" +
               std::to_string(++g_wal_ix) + ".wal"),
          wal(path, /*shard=*/7), ww(&wal), l3(&chan),
          engine(7, book, pool, &ww, nullptr, &l3) {
        std::remove(path.c_str());
    }
    ~LiveFix() { wal.close(); std::remove(path.c_str()); }

    std::string path;
    MemoryPool<Order> pool{512};
    OrderBook book{pool};
    Wal wal;
    WalWriter ww;
    CaptureChannel chan;
    L3Publisher l3;
    MatchingEngine engine;

    void open() { ASSERT_EQ(wal.open(), WalStatus::Ok); }
    std::vector<L3Rec> events() { return decode_l3(chan); }
};

// A journal-free replay engine driven by the same WAL stream, mirroring
// RecoveryManager's anchor policy: universal clock drive anchors ev.seq;
// driven rows re-anchor per arm; verbatim-applied trigger/phase rows
// anchor ev.seq+1.
struct ReplayDrive {
    MemoryPool<Order> pool{512};
    OrderBook book{pool};
    CaptureChannel chan;
    L3Publisher l3{&chan};
    MatchingEngine engine{7, book, pool, nullptr, nullptr, &l3};

    std::vector<L3Rec> events() { return decode_l3(chan); }

    void run(const std::string& path) {
        WalReader rd;
        ASSERT_EQ(rd.open(path), WalStatus::Ok);
        WalEntryView ev;
        while (rd.next(ev) == WalScanStep::Entry) {
            engine.set_replay_wal_seq(ev.seq);
            engine.on_time_tick(ev.timestamp_ns);
            switch (ev.type) {
                case WalEventType::TIME_TICK: {
                    WalTimeTickPayload tt;
                    std::memcpy(&tt, ev.payload, sizeof(tt));
                    engine.set_replay_wal_seq(ev.seq);
                    engine.on_time_tick(tt.tick_ns);
                    break;
                }
                case WalEventType::ORDER_NEW:
                case WalEventType::ORDER_NEW_EX: {
                    WalOrderNewPayload p;
                    std::memcpy(&p, ev.payload, sizeof(p));
                    Order* o = pool.alloc();
                    ASSERT_NE(o, nullptr);
                    *o = Order{};
                    o->id = p.order_id;
                    o->account_id = p.account_id;
                    o->side = static_cast<Side>(p.side);
                    // WAL type byte is the WIRE encoding (RecoveryManager's
                    // switch): 0=Market 1=Limit 2=Stop 3=StopLimit,
                    // 4=Iceberg 5=TrailingStop 6=Peg 7=Moo 8=Moc.
                    switch (p.type) {
                        case 0: o->type = OrderType::MARKET; break;
                        case 2: o->type = OrderType::STOP; break;
                        case 3: o->type = OrderType::STOP_LIMIT; break;
                        case kWalOrderTypeIceberg:
                            o->type = OrderType::ICEBERG; break;
                        case kWalOrderTypeTrailingStop:
                            o->type = OrderType::TRAILING_STOP; break;
                        case kWalOrderTypePeg:
                            o->type = OrderType::PEG; break;
                        case kWalOrderTypeMoo:
                            o->type = OrderType::MOO; break;
                        case kWalOrderTypeMoc:
                            o->type = OrderType::MOC; break;
                        default: o->type = OrderType::LIMIT; break;
                    }
                    o->tif = static_cast<TimeInForce>(p.tif);
                    o->stp_mode = static_cast<StpMode>(p.stp_mode);
                    o->flags = p.flags;
                    o->price_ticks = p.price_ticks;
                    o->qty_units = p.qty_units;
                    o->quantity = Decimal::from_mantissa(p.qty_units);
                    o->timestamp_ns = ev.timestamp_ns;
                    o->ingress_seq = ev.seq;
                    OrderAux aux{};
                    aux.stop_price_ticks = p.stop_price_ticks;
                    aux.gtd_expiry_ns = p.gtd_expiry_ns;
                    aux.trade_group_id = p.trade_group_id;
                    aux.instrument_id = p.instrument_id;
                    if (ev.type == WalEventType::ORDER_NEW_EX) {
                        WalOrderNewExPayload px;
                        std::memcpy(&px, ev.payload, sizeof(px));
                        aux.trigger_source = px.trigger_source;
                        aux.peg_mode = px.peg_mode;
                        aux.trail_unit = px.trail_unit;
                        aux.peg_offset_ticks = px.peg_offset_ticks;
                        aux.peg_limit_ticks = px.peg_limit_ticks;
                        aux.trail_distance = px.trail_distance;
                        aux.activation_price_ticks = px.activation_price_ticks;
                    }
                    engine.set_replay_wal_seq(ev.seq);
                    engine.on_order_received_ex(o, aux);
                    break;
                }
                case WalEventType::ORDER_CANCEL: {
                    WalOrderCancelPayload p;
                    std::memcpy(&p, ev.payload, sizeof(p));
                    engine.set_replay_wal_seq(ev.seq);
                    engine.on_cancel_received(p.order_id, p.account_id);
                    break;
                }
                case WalEventType::ORDER_MODIFY: {
                    WalOrderModifyPayload p;
                    std::memcpy(&p, ev.payload, sizeof(p));
                    engine.set_replay_wal_seq(ev.seq);
                    engine.on_amend_received(p.order_id, p.new_price_ticks,
                                             p.new_qty_units,
                                             p.new_stop_price_ticks, ev.seq);
                    break;
                }
                default:
                    break;  // TRADE/PEG_REPRICE/derived rows re-derive
            }
        }
    }
};

}  // namespace

// --- Publisher-level invariants ------------------------------------------------

TEST(L3, PerSymbolSeqsAreIndependent) {
    CaptureChannel chan;
    L3Publisher pub(&chan);
    L3Event e{};
    e.kind = static_cast<uint8_t>(L3Kind::Add);
    e.instrument_id = 11;
    for (int i = 0; i < 3; ++i) ASSERT_TRUE(pub.publish(e));
    e.instrument_id = 22;
    ASSERT_TRUE(pub.publish(e));
    auto v = decode_l3(chan);
    ASSERT_EQ(v.size(), 4u);
    EXPECT_EQ(v[0].seq, 1u);
    EXPECT_EQ(v[1].seq, 2u);
    EXPECT_EQ(v[2].seq, 3u);
    EXPECT_EQ(v[3].seq, 1u);  // instrument 22 starts its own counter
}

TEST(L3, AccountHashIsPseudonymAndDeterministic) {
    // Salted FNV-1a — stable across calls; never the raw id.
    EXPECT_EQ(L3Publisher::hash_account(42), L3Publisher::hash_account(42));
    EXPECT_NE(L3Publisher::hash_account(42), 42u);
    EXPECT_NE(L3Publisher::hash_account(1), L3Publisher::hash_account(2));
    CaptureChannel chan;
    L3Publisher pub(&chan);
    L3Event e{};
    e.kind = static_cast<uint8_t>(L3Kind::Add);
    e.account_id = 0x0123456789ABCDEFull;
    ASSERT_TRUE(pub.publish(e));
    auto v = decode_l3(chan);
    ASSERT_EQ(v.size(), 1u);
    EXPECT_EQ(v[0].account_hash, L3Publisher::hash_account(e.account_id));
    // The raw u64 must not appear verbatim anywhere in the frame.
    uint8_t raw[8];
    std::memcpy(raw, &e.account_id, 8);
    bool found = false;
    for (std::size_t i = 0; i + 8 <= chan.frames[0].size(); ++i)
        if (std::memcmp(chan.frames[0].data() + i, raw, 8) == 0) found = true;
    EXPECT_FALSE(found) << "raw account_id leaked onto the wire";
}

TEST(L3, DropConsumesSeqForGapDetection) {
    CaptureChannel chan;
    L3Publisher pub(&chan);
    L3Event e{};
    e.kind = static_cast<uint8_t>(L3Kind::Add);
    e.instrument_id = 5;
    ASSERT_TRUE(pub.publish(e));          // seq 1 delivered
    chan.fail = true;
    EXPECT_FALSE(pub.publish(e));         // seq 2 lost — consumed anyway
    chan.fail = false;
    ASSERT_TRUE(pub.publish(e));          // seq 3 lands -> consumer sees gap
    auto v = decode_l3(chan);
    ASSERT_EQ(v.size(), 2u);
    EXPECT_EQ(v[0].seq, 1u);
    EXPECT_EQ(v[1].seq, 3u);
    EXPECT_EQ(pub.published(), 2u);
    EXPECT_EQ(pub.drops(), 1u);
    EXPECT_EQ(pub.symbol_seq(5), 4u);
}

// --- Engine lifecycle emission ---------------------------------------------------

TEST(L3, RestingLimitEmitsAdd) {
    LiveFix f;
    f.open();
    f.engine.on_order_received(
        mk(f.pool, 1, Side::SELL, OrderType::LIMIT, P(5000), 100, 42));
    auto v = f.events();
    ASSERT_EQ(v.size(), 1u);
    const L3Rec& r = v[0];
    EXPECT_EQ(r.kind, static_cast<uint8_t>(L3Kind::Add));
    EXPECT_EQ(r.order_id, 1u);
    EXPECT_EQ(r.side, 1u);  // Sell
    EXPECT_EQ(r.price, P(5000));
    EXPECT_EQ(r.qty, 100);
    EXPECT_EQ(r.qty_delta, 100);
    EXPECT_EQ(r.seq, 1u);               // per-instrument seqs are 1-based
    EXPECT_EQ(r.wal_seq, 0u);           // first journal row (ORDER_NEW)
    EXPECT_EQ(r.account_hash, L3Publisher::hash_account(42));
    EXPECT_TRUE(r.flags & kL3FlagDetail);
    EXPECT_FALSE(r.flags & kL3FlagHidden);
}

TEST(L3, FillEmitsTakerAndMakerLegsSharingWalSeq) {
    LiveFix f;
    f.open();
    f.engine.on_order_received(
        mk(f.pool, 1, Side::SELL, OrderType::LIMIT, P(5000), 60, 1));
    f.engine.on_order_received(
        mk(f.pool, 2, Side::BUY, OrderType::LIMIT, P(5000), 40, 2));
    auto v = f.events();
    ASSERT_EQ(v.size(), 4u);
    EXPECT_EQ(v[0].kind, static_cast<uint8_t>(L3Kind::Add));   // maker
    EXPECT_EQ(v[1].kind, static_cast<uint8_t>(L3Kind::Add));   // taker
    const L3Rec& t = v[2];
    const L3Rec& m = v[3];
    EXPECT_EQ(t.kind, static_cast<uint8_t>(L3Kind::Fill));
    EXPECT_EQ(t.fill_role, kL3RoleTaker);
    EXPECT_EQ(t.order_id, 2u);
    EXPECT_EQ(t.side, 0u);             // Buy
    EXPECT_EQ(t.price, P(5000));
    EXPECT_EQ(t.qty, 0);               // fully filled remainder
    EXPECT_EQ(t.qty_delta, -40);
    EXPECT_EQ(m.kind, static_cast<uint8_t>(L3Kind::Fill));
    EXPECT_EQ(m.fill_role, kL3RoleMaker);
    EXPECT_EQ(m.order_id, 1u);
    EXPECT_EQ(m.side, 1u);
    EXPECT_EQ(m.qty, 20);              // 60 - 40 remaining
    EXPECT_EQ(m.qty_delta, -40);
    // Both legs correlate to the single TRADE row (wal seq 2).
    EXPECT_EQ(t.wal_seq, 2u);
    EXPECT_EQ(m.wal_seq, 2u);
    EXPECT_EQ(t.trade_id, m.trade_id);
    EXPECT_GT(t.trade_id, 0u);
    // Per-instrument seqs are monotone 1,2,3,4.
    for (std::size_t i = 0; i < v.size(); ++i) EXPECT_EQ(v[i].seq, i + 1);
}

TEST(L3, CancelEmitsWithWalCorrelation) {
    LiveFix f;
    f.open();
    f.engine.on_order_received(
        mk(f.pool, 1, Side::BUY, OrderType::LIMIT, P(4000), 70, 9));
    f.engine.on_cancel_received(1, 9);
    auto v = f.events();
    ASSERT_EQ(v.size(), 2u);
    const L3Rec& c = v[1];
    EXPECT_EQ(c.kind, static_cast<uint8_t>(L3Kind::Cancel));
    EXPECT_EQ(c.order_id, 1u);
    EXPECT_EQ(c.side, 0u);
    EXPECT_EQ(c.price, P(4000));
    EXPECT_EQ(c.qty, 0);
    EXPECT_EQ(c.qty_delta, -70);       // remaining removed
    EXPECT_EQ(c.cancel_reason, kWalCancelReasonUser);
    EXPECT_EQ(c.wal_seq, 1u);          // ORDER_CANCEL row
}

TEST(L3, QtyDownAmendEmitsModifyKeepingPrice) {
    LiveFix f;
    f.open();
    f.engine.on_order_received(
        mk(f.pool, 1, Side::BUY, OrderType::LIMIT, P(4000), 70, 3));
    f.engine.on_amend_received(1, /*price*/ 0, /*qty*/ 50, /*stop*/ 0,
                               /*seq*/ 900);
    auto v = f.events();
    ASSERT_EQ(v.size(), 2u);
    const L3Rec& m = v[1];
    EXPECT_EQ(m.kind, static_cast<uint8_t>(L3Kind::Modify));
    EXPECT_EQ(m.order_id, 1u);
    EXPECT_EQ(m.price, P(4000));       // unchanged
    EXPECT_EQ(m.qty, 50);
    EXPECT_EQ(m.qty_delta, -20);
    EXPECT_EQ(m.wal_seq, 1u);          // ORDER_MODIFY row
}

TEST(L3, PriceChangeAmendEmitsModifyWithNewPrice) {
    LiveFix f;
    f.open();
    f.engine.on_order_received(
        mk(f.pool, 1, Side::BUY, OrderType::LIMIT, P(4000), 70, 3));
    f.engine.on_amend_received(1, P(4100), 0, 0, 901);
    auto v = f.events();
    ASSERT_EQ(v.size(), 2u);
    const L3Rec& m = v[1];
    EXPECT_EQ(m.kind, static_cast<uint8_t>(L3Kind::Modify));
    EXPECT_EQ(m.price, P(4100));
    EXPECT_EQ(m.qty, 70);
    EXPECT_EQ(m.qty_delta, 0);
    EXPECT_EQ(m.wal_seq, 1u);
    // The order re-queues at the amended level.
    EXPECT_EQ(f.book.find_order(1)->price_ticks, P(4100));
}

TEST(L3, IocRemainderCancelCarriesReason4) {
    LiveFix f;
    f.open();
    f.engine.on_order_received(
        mk(f.pool, 1, Side::SELL, OrderType::LIMIT, P(5000), 30, 1));
    f.engine.on_order_received(
        mk(f.pool, 2, Side::BUY, OrderType::LIMIT, P(5000), 50, 2,
           TimeInForce::IOC));
    auto v = f.events();
    // ADD maker, ADD taker, FILL taker, FILL maker, CANCEL taker remainder.
    ASSERT_EQ(v.size(), 5u);
    const L3Rec& c = v[4];
    EXPECT_EQ(c.kind, static_cast<uint8_t>(L3Kind::Cancel));
    EXPECT_EQ(c.order_id, 2u);
    EXPECT_EQ(c.qty, 0);
    EXPECT_EQ(c.qty_delta, -20);       // 50 - 30 filled remainder
    EXPECT_EQ(c.cancel_reason, kWalCancelReasonIocRemainder);
    // WAL rows: NEW(0), NEW(1), TRADE(2), derived CANCEL(3).
    EXPECT_EQ(c.wal_seq, 3u);
}

TEST(L3, HiddenOrderEmitsHiddenFlagOnAddAndFill) {
    LiveFix f;
    f.open();
    // Hidden makers fill at the visible-BBO midpoint (Task 16.3.13), so a
    // visible spread must exist for the hidden order to match at all.
    f.engine.on_order_received(
        mk(f.pool, 10, Side::BUY, OrderType::LIMIT, P(4000), 10, 9));
    f.engine.on_order_received(
        mk(f.pool, 11, Side::SELL, OrderType::LIMIT, P(6000), 10, 9));
    Order* h = mk(f.pool, 1, Side::SELL, OrderType::LIMIT, P(5000), 30, 1);
    h->flags |= kOrderFlagHidden;
    f.engine.on_order_received(h);
    f.engine.on_order_received(
        mk(f.pool, 2, Side::BUY, OrderType::LIMIT, P(5000), 10, 2));
    auto v = f.events();
    ASSERT_EQ(v.size(), 6u);  // ADD ADD ADD(hidden) ADD FILL(taker) FILL(hidden)
    EXPECT_TRUE(v[2].flags & kL3FlagHidden);   // hidden ADD
    const L3Rec& mfill = v[5];
    EXPECT_EQ(mfill.kind, static_cast<uint8_t>(L3Kind::Fill));
    EXPECT_EQ(mfill.order_id, 1u);
    EXPECT_TRUE(mfill.flags & kL3FlagHidden);  // hidden maker leg
    EXPECT_EQ(mfill.price, P(5000));           // mid fill
}

TEST(L3, PegRepriceEmitsModify) {
    LiveFix f;
    f.open();
    // Visible BBO: bid 100 / ask 300 -> mid 200.
    f.engine.on_order_received(
        mk(f.pool, 1, Side::BUY, OrderType::LIMIT, P(100), 10, 1));
    f.engine.on_order_received(
        mk(f.pool, 2, Side::SELL, OrderType::LIMIT, P(300), 10, 2));
    OrderAux peg{};
    peg.peg_mode = kPegMid;
    f.engine.on_order_received_ex(
        mk(f.pool, 3, Side::BUY, OrderType::PEG, 0, 10, 3), peg);
    {
        auto v = f.events();
        ASSERT_EQ(v.size(), 3u);
        const L3Rec& add = v[2];
        EXPECT_EQ(add.kind, static_cast<uint8_t>(L3Kind::Add));
        EXPECT_EQ(add.price, P(200));              // admission mid
        EXPECT_TRUE(add.flags & kL3FlagPegged);
        EXPECT_TRUE(add.flags & kL3FlagHidden);    // pegs are L2-hidden
    }
    // Move the far ask up: mid 100..500 -> 300; the next tick's settle
    // wave reprices the peg.
    f.engine.on_amend_received(2, P(500), 0, 0, 910);
    f.engine.on_time_tick(++g_ts * 10);
    auto v = f.events();
    // +1 MODIFY (amend of ask) +1 MODIFY (PEG reprice)
    ASSERT_EQ(v.size(), 5u);
    const L3Rec& re = v[4];
    EXPECT_EQ(re.kind, static_cast<uint8_t>(L3Kind::Modify));
    EXPECT_EQ(re.order_id, 3u);
    EXPECT_EQ(re.price, P(300));                 // new mid
    EXPECT_EQ(re.ref_price, P(300));             // peg reference
    EXPECT_EQ(re.qty, 10);
    EXPECT_EQ(re.qty_delta, 0);
    EXPECT_TRUE(re.flags & kL3FlagPegged);
    // The PEG_REPRICE row follows the TIME_TICK + ORDER_MODIFY rows.
    EXPECT_EQ(re.wal_seq, 5u);
}

// --- WAL correlation (audit half of §24 #318) --------------------------------------

TEST(L3, EveryEventWalSeqPointsAtAMatchingRow) {
    LiveFix f;
    f.open();
    f.engine.on_order_received(
        mk(f.pool, 1, Side::SELL, OrderType::LIMIT, P(5000), 60, 1));
    f.engine.on_order_received(
        mk(f.pool, 2, Side::BUY, OrderType::LIMIT, P(5000), 40, 2,
           TimeInForce::IOC));
    f.engine.on_order_received(
        mk(f.pool, 3, Side::BUY, OrderType::LIMIT, P(4000), 70, 3));
    f.engine.on_amend_received(3, 0, 50, 0, 920);
    f.engine.on_cancel_received(3, 3);
    f.wal.close();

    auto v = f.events();
    ASSERT_EQ(v.size(), 7u);  // ADD ADD ADD FILL FILL MODIFY CANCEL... see below
    // Index WAL rows by seq.
    WalReader rd;
    ASSERT_EQ(rd.open(f.path), WalStatus::Ok);
    std::vector<WalEventType> rows;
    WalEntryView ev;
    while (rd.next(ev) == WalScanStep::Entry) {
        if (ev.seq >= rows.size()) rows.resize(ev.seq + 1);
        rows[ev.seq] = ev.type;
    }
    for (const L3Rec& r : v) {
        ASSERT_LT(r.wal_seq, rows.size())
            << "wal_seq " << r.wal_seq << " beyond journal tail";
        switch (r.kind) {
            case static_cast<uint8_t>(L3Kind::Add):
                EXPECT_TRUE(rows[r.wal_seq] == WalEventType::ORDER_NEW ||
                            rows[r.wal_seq] == WalEventType::ORDER_NEW_EX);
                break;
            case static_cast<uint8_t>(L3Kind::Fill):
                EXPECT_EQ(rows[r.wal_seq], WalEventType::TRADE);
                break;
            case static_cast<uint8_t>(L3Kind::Cancel):
                EXPECT_EQ(rows[r.wal_seq], WalEventType::ORDER_CANCEL);
                break;
            case static_cast<uint8_t>(L3Kind::Modify):
                EXPECT_TRUE(rows[r.wal_seq] == WalEventType::ORDER_MODIFY ||
                            rows[r.wal_seq] == WalEventType::PEG_REPRICE);
                break;
        }
    }
}

// --- Replay determinism --------------------------------------------------------------

TEST(L3, JournalFreeReplayReproducesIdenticalStream) {
    LiveFix f;
    f.open();
    // A lifecycle mix: rest, sweep, amend, cancel, IOC remainder, hidden.
    f.engine.on_order_received(
        mk(f.pool, 1, Side::SELL, OrderType::LIMIT, P(5000), 60, 1));
    Order* h = mk(f.pool, 2, Side::SELL, OrderType::LIMIT, P(5100), 25, 4);
    h->flags |= kOrderFlagHidden;
    f.engine.on_order_received(h);
    f.engine.on_order_received(
        mk(f.pool, 3, Side::BUY, OrderType::LIMIT, P(5000), 40, 2));
    // Amend the 60-qty maker to total 50 (40 already filled) — qty must
    // exceed filled or the amend rejects without journaling.
    f.engine.on_amend_received(1, 0, 50, 0, 930);
    f.engine.on_order_received(
        mk(f.pool, 4, Side::BUY, OrderType::LIMIT, P(5100), 50, 5,
           TimeInForce::IOC));
    f.engine.on_time_tick(++g_ts * 10);
    f.engine.on_cancel_received(1, 1);
    f.wal.close();
    const auto live = f.events();
    ASSERT_GT(live.size(), 8u);

    ReplayDrive d;
    d.run(f.path);
    const auto replay = d.events();
    ASSERT_EQ(replay.size(), live.size());
    for (std::size_t i = 0; i < live.size(); ++i) {
        EXPECT_TRUE(replay[i] == live[i])
            << "L3 event " << i << " diverged under replay: live kind="
            << int(live[i].kind) << " wal_seq=" << live[i].wal_seq
            << " replay kind=" << int(replay[i].kind)
            << " wal_seq=" << replay[i].wal_seq;
    }
}

TEST(L3, JournalFreeEngineStillPublishesWithVirtualSeqs) {
    // Journal-free (no WAL): the cursor counts journal sites so emitted
    // wal_seq values stay deterministic for unit tests / shadow replay.
    MemoryPool<Order> pool{512};
    OrderBook book{pool};
    CaptureChannel chan;
    L3Publisher l3(&chan);
    MatchingEngine e(7, book, pool, nullptr, nullptr, &l3);
    e.on_order_received(
        mk(pool, 1, Side::SELL, OrderType::LIMIT, P(5000), 60, 1));
    e.on_order_received(
        mk(pool, 2, Side::BUY, OrderType::LIMIT, P(5000), 40, 2));
    auto v = decode_l3(chan);
    ASSERT_EQ(v.size(), 4u);
    EXPECT_EQ(v[0].wal_seq, 0u);  // NEW
    EXPECT_EQ(v[1].wal_seq, 1u);  // NEW
    EXPECT_EQ(v[2].wal_seq, 2u);  // TRADE — both legs
    EXPECT_EQ(v[3].wal_seq, 2u);
}
