// Task 2.3.12 coverage: codec round-trip, reserve→ACK happy path, layered
// 500µs/10ms timeout ladder, pessimistic fallback code mapping, idempotent
// release, hosted-slice participant role, out-of-order release, WAL
// round-trip, and restart-mid-reservation recovery semantics.

#include <gtest/gtest.h>

#include <atomic>
#include <cstdint>
#include <cstring>
#include <deque>
#include <filesystem>
#include <string>
#include <vector>

#include <unistd.h>

#include "risk/CrossShardMarginCoordinator.h"
#include "wal/Wal.hpp"

using namespace exch;

namespace {

// --- scripted coordinator double (the real Go Risk Coordinator is Phase-19) ----

class FakeCtlChannel final : public IpcChannel {
public:
    enum class Mode { Manual, AckAll, NackAll, Silent };

    bool open() noexcept override { return open_; }
    void close() noexcept override { open_ = false; }
    [[nodiscard]] bool is_open() const noexcept override { return open_; }

    bool send(const void* data, uint32_t len) noexcept override {
        if (!open_ || data == nullptr)
            return false;
        sent_.emplace_back(static_cast<const uint8_t*>(data),
                           static_cast<const uint8_t*>(data) + len);
        MarginCtlView v;
        if (mode_ == Mode::Manual ||
            margin_ctl_decode(data, len, &v) != MarginCtlDecode::Ok)
            return true;
        if (v.type == MarginCtlType::ReserveReq && mode_ == Mode::AckAll) {
            MarginReserveAckBody a{};
            a.reservation_id = v.req.reservation_id;
            a.account_id = v.req.account_id;
            a.shard_id = 7;  // arbitrary host shard (coordinator picks)
            a.granted_amount = v.req.amount;
            a.expires_at_ns = v.req.expires_at_ns;
            push_inbound(MarginCtlType::ReserveAck, a);
        } else if (v.type == MarginCtlType::ReserveReq &&
                   mode_ == Mode::NackAll) {
            MarginReserveNackBody n{};
            n.reservation_id = v.req.reservation_id;
            n.account_id = v.req.account_id;
            n.shard_id = 7;
            n.reason = static_cast<uint32_t>(NackReason::InsufficientHeadroom);
            push_inbound(MarginCtlType::ReserveNack, n);
        }
        return true;
    }

    int32_t poll(void* buf, uint32_t cap) noexcept override {
        if (inbound_.empty())
            return 0;
        auto& f = inbound_.front();
        if (f.size() > cap)
            return -1;
        std::memcpy(buf, f.data(), f.size());
        const auto n = static_cast<int32_t>(f.size());
        inbound_.pop_front();
        return n;
    }

    template <typename Body>
    void push_inbound(MarginCtlType t, const Body& b) {
        std::vector<uint8_t> f(kMarginCtlMaxFrame);
        const uint32_t n =
            margin_ctl_encode(f.data(), static_cast<uint32_t>(f.size()), t, &b);
        f.resize(n);
        inbound_.push_back(std::move(f));
    }

    // Counts of engine->coordinator frames by type.
    [[nodiscard]] size_t sent_of(MarginCtlType t) const {
        size_t n = 0;
        for (auto& f : sent_) {
            MarginCtlView v;
            if (margin_ctl_decode(f.data(), static_cast<uint32_t>(f.size()),
                                  &v) == MarginCtlDecode::Ok &&
                v.type == t)
                ++n;
        }
        return n;
    }
    [[nodiscard]] bool last_release_reason(uint8_t& reason) const {
        for (auto it = sent_.rbegin(); it != sent_.rend(); ++it) {
            MarginCtlView v;
            if (margin_ctl_decode(it->data(),
                                  static_cast<uint32_t>(it->size()),
                                  &v) == MarginCtlDecode::Ok &&
                v.type == MarginCtlType::Release) {
                reason = v.rel.reason;
                return true;
            }
        }
        return false;
    }

    Mode mode_ = Mode::Manual;
    bool open_ = true;
    std::deque<std::vector<uint8_t>> inbound_;   // coordinator -> engine
    std::vector<std::vector<uint8_t>> sent_;     // engine -> coordinator
};

// Deterministic clocks. step_ns > 0 auto-advances on every read (drives the
// layered deadline sweep without real-time sleeps).
struct ClockBox {
    int64_t t = 1'000'000'000;            // monotonic ns
    uint64_t w = 1'700'000'000'000'000'000ULL;  // realtime ns
    int64_t step_ns = 0;

    static int64_t steady(void* c) noexcept {
        auto* b = static_cast<ClockBox*>(c);
        b->t += b->step_ns;
        return b->t;
    }
    static uint64_t wall(void* c) noexcept {
        return static_cast<ClockBox*>(c)->w;
    }
};

MarginCoordinatorOptions opts_for(ClockBox& c, uint16_t shard = 3) {
    MarginCoordinatorOptions o;
    o.shard_id = shard;
    o.steady_ns_fn = &ClockBox::steady;
    o.steady_ctx = &c;
    o.now_ns_fn = &ClockBox::wall;
    o.now_ctx = &c;
    return o;
}

std::filesystem::path tmp_dir(const char* name) {
    const auto d = std::filesystem::temp_directory_path() /
                   ("exch_mcoord_" + std::to_string(::getpid()) + "_" + name);
    std::filesystem::remove_all(d);
    std::filesystem::create_directories(d);
    return d;
}

}  // namespace

// --- codec ----------------------------------------------------------------------

TEST(MarginCtlCodec, RoundTripAllTypes) {
    uint8_t buf[kMarginCtlMaxFrame];
    MarginCtlView v;

    MarginReserveReqBody req{};
    req.reservation_id = (uint64_t{3} << 48) | 7;
    req.account_id = 100;
    req.order_id = 42;
    req.src_shard = 3;
    req.dst_shard = kMarginCoordinatorPicks;
    req.instrument_id = 9;
    req.req_flags = kMarginReqFlagCorrelationOffset;
    req.amount = 5'000'000;
    req.expires_at_ns = 12345;
    uint32_t n = margin_ctl_encode(buf, sizeof(buf), MarginCtlType::ReserveReq,
                                   &req);
    ASSERT_EQ(n, sizeof(MarginCtlHeader) + sizeof(MarginReserveReqBody));
    ASSERT_EQ(margin_ctl_decode(buf, n, &v), MarginCtlDecode::Ok);
    EXPECT_EQ(v.type, MarginCtlType::ReserveReq);
    EXPECT_EQ(v.req.reservation_id, req.reservation_id);
    EXPECT_EQ(v.req.account_id, 100u);
    EXPECT_EQ(v.req.order_id, 42u);
    EXPECT_EQ(v.req.dst_shard, kMarginCoordinatorPicks);
    EXPECT_EQ(v.req.req_flags, kMarginReqFlagCorrelationOffset);
    EXPECT_EQ(v.req.amount, 5'000'000);

    MarginReserveAckBody ack{};
    ack.reservation_id = 77;
    ack.account_id = 100;
    ack.shard_id = 5;
    ack.granted_amount = 2'500;
    n = margin_ctl_encode(buf, sizeof(buf), MarginCtlType::ReserveAck, &ack);
    ASSERT_EQ(margin_ctl_decode(buf, n, &v), MarginCtlDecode::Ok);
    EXPECT_EQ(v.ack.granted_amount, 2'500);
    EXPECT_EQ(v.ack.shard_id, 5u);

    MarginReserveNackBody nk{};
    nk.reservation_id = 77;
    nk.reason = static_cast<uint32_t>(NackReason::InsufficientHeadroom);
    n = margin_ctl_encode(buf, sizeof(buf), MarginCtlType::ReserveNack, &nk);
    ASSERT_EQ(margin_ctl_decode(buf, n, &v), MarginCtlDecode::Ok);
    EXPECT_EQ(v.nack.reason,
              static_cast<uint32_t>(NackReason::InsufficientHeadroom));

    MarginReleaseBody rel{};
    rel.reservation_id = 77;
    rel.reason = static_cast<uint8_t>(ReleaseReason::Expired);
    n = margin_ctl_encode(buf, sizeof(buf), MarginCtlType::Release, &rel);
    ASSERT_EQ(margin_ctl_decode(buf, n, &v), MarginCtlDecode::Ok);
    EXPECT_EQ(v.rel.reason, static_cast<uint8_t>(ReleaseReason::Expired));
}

TEST(MarginCtlCodec, RejectsMalformed) {
    uint8_t buf[kMarginCtlMaxFrame] = {};
    MarginCtlView v;
    EXPECT_EQ(margin_ctl_decode(buf, 4, &v), MarginCtlDecode::TooShort);
    MarginCtlHeader h{kMarginCtlMagic,
                      static_cast<uint8_t>(MarginCtlType::ReserveAck), 0,
                      kMarginCtlVersion};
    std::memcpy(buf, &h, sizeof(h));
    EXPECT_EQ(margin_ctl_decode(buf, sizeof(h), &v),
              MarginCtlDecode::TooShort);  // header only, body missing
    buf[0] ^= 0xFF;
    EXPECT_EQ(margin_ctl_decode(buf, sizeof(h) + 40, &v),
              MarginCtlDecode::BadMagic);
    MarginCtlHeader bad_ver{kMarginCtlMagic,
                            static_cast<uint8_t>(MarginCtlType::Release), 0,
                            99};
    std::memcpy(buf, &bad_ver, sizeof(bad_ver));
    EXPECT_EQ(margin_ctl_decode(buf, sizeof(buf), &v),
              MarginCtlDecode::BadVersion);
    MarginCtlHeader bad_ty{kMarginCtlMagic, 0xEE, 0, kMarginCtlVersion};
    std::memcpy(buf, &bad_ty, sizeof(bad_ty));
    EXPECT_EQ(margin_ctl_decode(buf, sizeof(buf), &v),
              MarginCtlDecode::UnknownType);
}

// --- requester path ---------------------------------------------------------------

TEST(MarginCoordinator, ReserveAckHappyPath) {
    ClockBox clk;
    FakeCtlChannel ch;
    ch.mode_ = FakeCtlChannel::Mode::AckAll;
    CrossShardMarginCoordinator c(&ch, nullptr, opts_for(clk));
    c.set_local_headroom(100, 10'000);

    const uint64_t id = c.begin_reserve(100, /*instr=*/9, /*amount=*/5'000,
                                        /*expires=*/0, /*corr=*/false,
                                        kMarginCoordinatorPicks, /*ord=*/42);
    ASSERT_NE(id, 0u);
    EXPECT_EQ(id >> 48, 3u);  // high16 = our shard
    EXPECT_EQ(c.state_of(id), ReservationState::Pending);
    EXPECT_EQ(ch.sent_of(MarginCtlType::ReserveReq), 1u);

    EXPECT_EQ(c.resolve_wait(id), MarginResolve::Granted);
    EXPECT_EQ(c.state_of(id), ReservationState::Granted);
    EXPECT_EQ(c.granted_slices(100), 5'000);
    // local 10k + granted 5k covers 14k, not 15'001.
    EXPECT_TRUE(c.covers(14'000, 100));
    EXPECT_FALSE(c.covers(15'001, 100));
    EXPECT_EQ(c.evaluate(100, 16'000).shortfall, 1'000);
}

TEST(MarginCoordinator, SoftBudgetFallbackThenHardDeadlineCompensate) {
    ClockBox clk;
    clk.step_ns = 100'000;  // each steady read advances 100µs
    FakeCtlChannel ch;      // Manual: coordinator never answers
    CrossShardMarginCoordinator c(&ch, nullptr, opts_for(clk));

    const uint64_t id = c.begin_reserve(100, 9, 5'000);
    ASSERT_NE(id, 0u);
    // 500µs budget expires -> pessimistic floor (CROSS_SHARD_MARGIN_UNAVAILABLE
    // is the mapped rejection; the REQ remains in-flight).
    EXPECT_EQ(c.resolve_wait(id), MarginResolve::Fallback);
    EXPECT_EQ(c.state_of(id), ReservationState::TimedOut);
    EXPECT_EQ(c.soft_timeouts(), 1u);

    // The REQ lives on until the >10ms hard deadline, then is cancelled +
    // compensated: RELEASE emitted, record released.
    clk.t += 20'000'000;
    c.poll();
    EXPECT_EQ(c.state_of(id), ReservationState::Released);
    EXPECT_EQ(c.hard_timeouts(), 1u);
    EXPECT_GE(c.compensations(), 1u);
    EXPECT_EQ(ch.sent_of(MarginCtlType::Release), 1u);
    uint8_t reason = 0;
    ASSERT_TRUE(ch.last_release_reason(reason));
    EXPECT_EQ(reason,
              static_cast<uint8_t>(ReleaseReason::TimeoutCompensate));
    EXPECT_STREQ(kCodeCrossShardMarginUnavailable,
                 "CROSS_SHARD_MARGIN_UNAVAILABLE");
    EXPECT_STREQ(kCodeCrossShardMarginTimeout, "CROSS_SHARD_MARGIN_TIMEOUT");
}

// ACK landing inside the (500µs, 10ms] window: slice is committed then
// immediately compensated — the triggering order already took the floor.
TEST(MarginCoordinator, LateAckCompensated) {
    ClockBox clk;
    clk.step_ns = 100'000;
    FakeCtlChannel ch;
    CrossShardMarginCoordinator c(&ch, nullptr, opts_for(clk));

    const uint64_t id = c.begin_reserve(100, 9, 5'000);
    ASSERT_EQ(c.resolve_wait(id), MarginResolve::Fallback);

    MarginReserveAckBody ack{};
    ack.reservation_id = id;
    ack.account_id = 100;
    ack.shard_id = 7;
    ack.granted_amount = 5'000;
    ch.push_inbound(MarginCtlType::ReserveAck, ack);
    c.poll();
    EXPECT_EQ(c.state_of(id), ReservationState::Released);
    EXPECT_EQ(c.granted_slices(100), 0);  // grant + release nets to zero
    EXPECT_EQ(ch.sent_of(MarginCtlType::Release), 1u);
    uint8_t reason = 0;
    ASSERT_TRUE(ch.last_release_reason(reason));
    EXPECT_EQ(reason,
              static_cast<uint8_t>(ReleaseReason::TimeoutCompensate));
}

TEST(MarginCoordinator, NackDeniesReservation) {
    ClockBox clk;
    FakeCtlChannel ch;
    ch.mode_ = FakeCtlChannel::Mode::NackAll;
    CrossShardMarginCoordinator c(&ch, nullptr, opts_for(clk));
    c.set_local_headroom(100, 1'000);

    const uint64_t id = c.begin_reserve(100, 9, 5'000);
    EXPECT_EQ(c.resolve_wait(id), MarginResolve::Denied);
    EXPECT_EQ(c.state_of(id), ReservationState::Denied);
    EXPECT_EQ(c.granted_slices(100), 0);
    EXPECT_FALSE(c.covers(6'000, 100));
    EXPECT_EQ(c.nacks(), 1u);
}

TEST(MarginCoordinator, ReleaseIsIdempotent) {
    ClockBox clk;
    FakeCtlChannel ch;
    ch.mode_ = FakeCtlChannel::Mode::AckAll;
    CrossShardMarginCoordinator c(&ch, nullptr, opts_for(clk));
    c.set_local_headroom(100, 0);

    const uint64_t id = c.begin_reserve(100, 9, 5'000);
    ASSERT_EQ(c.resolve_wait(id), MarginResolve::Granted);
    ASSERT_TRUE(c.release(id, ReleaseReason::OrderRejected));
    EXPECT_EQ(c.state_of(id), ReservationState::Released);
    EXPECT_EQ(c.granted_slices(100), 0);
    // Second + third releases: no-op success, no extra wire frames.
    EXPECT_TRUE(c.release(id));
    EXPECT_TRUE(c.release(id, ReleaseReason::Complete));
    EXPECT_EQ(ch.sent_of(MarginCtlType::Release), 1u);
    // Unknown id is also an idempotent no-op.
    EXPECT_TRUE(c.release(0xDEADBEEF));
    EXPECT_EQ(ch.sent_of(MarginCtlType::Release), 1u);
}

TEST(MarginCoordinator, OutOfOrderReleaseThenAck) {
    ClockBox clk;
    clk.step_ns = 1'000;
    FakeCtlChannel ch;
    CrossShardMarginCoordinator c(&ch, nullptr, opts_for(clk));

    const uint64_t id = c.begin_reserve(100, 9, 5'000);
    ASSERT_NE(id, 0u);

    // RELEASE for the still-Pending reservation (coordinator-initiated
    // cancel, delivered out-of-order).
    MarginReleaseBody rel{};
    rel.reservation_id = id;
    rel.account_id = 100;
    rel.shard_id = 7;
    rel.reason = static_cast<uint8_t>(ReleaseReason::CoordinatorInitiated);
    ch.push_inbound(MarginCtlType::Release, rel);
    c.poll();
    EXPECT_EQ(c.state_of(id), ReservationState::Released);
    EXPECT_EQ(c.granted_slices(100), 0);

    // The ACK that overtook the RELEASE arrives now: slice granted on the
    // coordinator's books — we answer with a compensating RELEASE.
    MarginReserveAckBody ack{};
    ack.reservation_id = id;
    ack.account_id = 100;
    ack.shard_id = 7;
    ack.granted_amount = 5'000;
    ch.push_inbound(MarginCtlType::ReserveAck, ack);
    c.poll();
    EXPECT_EQ(c.state_of(id), ReservationState::Released);
    EXPECT_EQ(c.granted_slices(100), 0);
    EXPECT_EQ(ch.sent_of(MarginCtlType::Release), 1u);  // the compensation
}

// --- participant role (coordinator -> engine REQ) ----------------------------------

TEST(MarginCoordinator, HostedSliceGrantAndDedupe) {
    ClockBox clk;
    FakeCtlChannel ch;
    CrossShardMarginCoordinator c(&ch, nullptr, opts_for(clk));
    c.set_local_headroom(200, 10'000);

    MarginReserveReqBody req{};
    req.reservation_id = (uint64_t{9} << 48) | 1;  // remote shard 9's id space
    req.account_id = 200;
    req.src_shard = 9;
    req.dst_shard = 3;
    req.amount = 4'000;
    ch.push_inbound(MarginCtlType::ReserveReq, req);
    c.poll();
    EXPECT_EQ(ch.sent_of(MarginCtlType::ReserveAck), 1u);
    EXPECT_EQ(c.state_of(req.reservation_id),
              ReservationState::GrantedHosted);
    EXPECT_EQ(c.hosted_locks(200), 4'000);
    // Local admission sees the lock: usable = 10000 - 4000 = 6000.
    EXPECT_TRUE(c.covers(6'000, 200));
    EXPECT_FALSE(c.covers(6'001, 200));

    // Idempotent redelivery: second REQ for the same id re-ACKs without a
    // second lock.
    ch.push_inbound(MarginCtlType::ReserveReq, req);
    c.poll();
    EXPECT_EQ(ch.sent_of(MarginCtlType::ReserveAck), 2u);
    EXPECT_EQ(c.hosted_locks(200), 4'000);

    // Coordinator releases the hosted slice.
    MarginReleaseBody rel{};
    rel.reservation_id = req.reservation_id;
    rel.account_id = 200;
    rel.shard_id = 3;
    rel.reason = static_cast<uint8_t>(ReleaseReason::CoordinatorInitiated);
    ch.push_inbound(MarginCtlType::Release, rel);
    c.poll();
    EXPECT_EQ(c.state_of(req.reservation_id), ReservationState::Released);
    EXPECT_EQ(c.hosted_locks(200), 0);
    EXPECT_TRUE(c.covers(10'000, 200));
}

TEST(MarginCoordinator, HostedReqInsufficientHeadroomNacks) {
    ClockBox clk;
    FakeCtlChannel ch;
    CrossShardMarginCoordinator c(&ch, nullptr, opts_for(clk));
    c.set_local_headroom(200, 1'000);  // less than requested 4k

    MarginReserveReqBody req{};
    req.reservation_id = (uint64_t{9} << 48) | 2;
    req.account_id = 200;
    req.src_shard = 9;
    req.amount = 4'000;
    ch.push_inbound(MarginCtlType::ReserveReq, req);
    c.poll();
    EXPECT_EQ(ch.sent_of(MarginCtlType::ReserveAck), 0u);
    EXPECT_EQ(ch.sent_of(MarginCtlType::ReserveNack), 1u);
    EXPECT_EQ(c.state_of(req.reservation_id), ReservationState::Denied);
    EXPECT_EQ(c.hosted_locks(200), 0);
    // Replayed REQ deterministically re-NACKs (tombstone dedupe).
    ch.push_inbound(MarginCtlType::ReserveReq, req);
    c.poll();
    EXPECT_EQ(ch.sent_of(MarginCtlType::ReserveNack), 2u);
}

// --- WAL round-trip + recovery --------------------------------------------------------

TEST(MarginCoordinator, WalRoundTripReservationEntries) {
    const auto dir = tmp_dir("wrt");
    const auto path = (dir / "margin.wal").string();

    ClockBox clk;
    FakeCtlChannel ch;
    ch.mode_ = FakeCtlChannel::Mode::AckAll;
    uint64_t id;
    uint64_t expires;
    {
        Wal wal(path, /*shard_id=*/3);
        ASSERT_EQ(wal.open(), WalStatus::Ok);
        CrossShardMarginCoordinator c(&ch, &wal, opts_for(clk));
        expires = clk.w + 60'000'000'000ULL;  // +60s, live
        id = c.begin_reserve(100, 9, 5'000, expires, false,
                             kMarginCoordinatorPicks, /*order=*/4242);
        ASSERT_EQ(c.resolve_wait(id), MarginResolve::Granted);
        ASSERT_TRUE(c.release(id, ReleaseReason::Complete));
        ASSERT_EQ(wal.flush(), WalStatus::Ok);
        wal.close();
    }
    WalReader rd(path);
    ASSERT_TRUE(rd.is_open());
    WalEntryView v;
    int intents = 0, grants = 0, releases = 0;
    while (rd.next(v) == WalScanStep::Entry) {
        if (v.type == WalEventType::MARGIN_RESERVE) {
            WalMarginReservePayload p;
            ASSERT_EQ(v.payload_len, sizeof(p));
            std::memcpy(&p, v.payload, sizeof(p));
            EXPECT_EQ(p.reservation_id, id);
            EXPECT_EQ(p.account_id, 100u);
            EXPECT_EQ(p.order_id, 4242u);
            EXPECT_EQ(p.consumer_shard, 3u);
            EXPECT_EQ(p.amount, 5'000);
            if (p.origin == 2) {
                ++intents;  // uncommitted intent (id reservation)
            } else {
                EXPECT_EQ(p.origin, 0);        // Local grant
                EXPECT_EQ(p.host_shard, 7u);   // granted by fake shard 7
                EXPECT_EQ(p.expires_at_ns, expires);
                ++grants;
            }
        } else if (v.type == WalEventType::MARGIN_RELEASE) {
            WalMarginReleasePayload p;
            ASSERT_EQ(v.payload_len, sizeof(p));
            std::memcpy(&p, v.payload, sizeof(p));
            EXPECT_EQ(p.reservation_id, id);
            EXPECT_EQ(p.reason,
                      static_cast<uint8_t>(ReleaseReason::Complete));
            ++releases;
        }
    }
    EXPECT_EQ(intents, 1);
    EXPECT_EQ(grants, 1);
    EXPECT_EQ(releases, 1);
    std::filesystem::remove_all(dir);
}

// Restart mid-reservation: replay restores committed state exactly — granted
// slice live again, released slice stays released, nothing Pending revives.
TEST(MarginCoordinator, RestartMidReservationRecovery) {
    const auto dir = tmp_dir("recover");
    const auto path = (dir / "margin.wal").string();

    ClockBox clk;
    FakeCtlChannel ch;
    ch.mode_ = FakeCtlChannel::Mode::AckAll;
    uint64_t id_keep, id_released, id_pending;
    {
        Wal wal(path, 3);
        ASSERT_EQ(wal.open(), WalStatus::Ok);
        CrossShardMarginCoordinator c(&ch, &wal, opts_for(clk));
        c.set_local_headroom(100, 1'000);
        id_keep = c.begin_reserve(100, 9, 5'000);
        ASSERT_EQ(c.resolve_wait(id_keep), MarginResolve::Granted);
        id_released = c.begin_reserve(100, 9, 2'000);
        ASSERT_EQ(c.resolve_wait(id_released), MarginResolve::Granted);
        ASSERT_TRUE(c.release(id_released));
        // Pending at crash: never WAL'd — must not resurrect.
        ch.mode_ = FakeCtlChannel::Mode::Silent;
        id_pending = c.begin_reserve(100, 9, 3'000);
        ASSERT_NE(id_pending, 0u);
        ASSERT_EQ(wal.flush(), WalStatus::Ok);
        wal.close();
    }

    ClockBox clk2;  // new process clock
    FakeCtlChannel ch2;
    CrossShardMarginCoordinator c2(&ch2, nullptr, opts_for(clk2));
    WalReader rd(path);
    ASSERT_TRUE(rd.is_open());
    const size_t applied = c2.recover(rd);
    // intents(3) + grants(2) + release(1) = 6 MARGIN_* entries applied.
    EXPECT_EQ(applied, 6u);

    EXPECT_EQ(c2.state_of(id_keep), ReservationState::Granted);
    EXPECT_EQ(c2.granted_slices(100), 5'000);
    EXPECT_EQ(c2.state_of(id_released), ReservationState::Released);
    EXPECT_EQ(c2.state_of(id_pending), ReservationState::Empty);  // never committed

    // Id sequence reseeded past the WAL max — no collision with restored ids.
    const uint64_t next = c2.begin_reserve(100, 9, 1'000);
    ASSERT_NE(next, 0u);
    EXPECT_GT(next & 0x0000FFFFFFFFFFFFULL,
              id_pending & 0x0000FFFFFFFFFFFFULL);
    std::filesystem::remove_all(dir);
}

// A recovered grant whose expires_at already passed is released on the first
// poll() — RecoveryOrphan reason, RELEASE wire emitted.
TEST(MarginCoordinator, RecoveryOrphanExpirySweep) {
    const auto dir = tmp_dir("orphan");
    const auto path = (dir / "margin.wal").string();

    ClockBox clk;
    FakeCtlChannel ch;
    ch.mode_ = FakeCtlChannel::Mode::AckAll;
    // Expiry is in the future while live (grant commits + WALs), but in the
    // past once the recovered coordinator's clock reads it.
    const uint64_t expires = clk.w + 60'000'000'000ULL;
    {
        Wal wal(path, 3);
        ASSERT_EQ(wal.open(), WalStatus::Ok);
        CrossShardMarginCoordinator c(&ch, &wal, opts_for(clk));
        const uint64_t id = c.begin_reserve(100, 9, 5'000, expires);
        ASSERT_EQ(c.resolve_wait(id), MarginResolve::Granted);
        ASSERT_EQ(wal.flush(), WalStatus::Ok);
        wal.close();
    }
    ClockBox clk2;
    clk2.w += 120'000'000'000ULL;  // recovered engine boots past expiry
    FakeCtlChannel ch2;
    CrossShardMarginCoordinator c2(&ch2, nullptr, opts_for(clk2));
    WalReader rd(path);
    ASSERT_TRUE(rd.is_open());
    ASSERT_EQ(c2.recover(rd), 2u);  // intent + grant
    c2.poll();
    EXPECT_EQ(c2.granted_slices(100), 0);
    EXPECT_EQ(ch2.sent_of(MarginCtlType::Release), 1u);
    uint8_t reason = 0;
    ASSERT_TRUE(ch2.last_release_reason(reason));
    EXPECT_EQ(reason, static_cast<uint8_t>(ReleaseReason::RecoveryOrphan));
    std::filesystem::remove_all(dir);
}

// --- misc -----------------------------------------------------------------------------

TEST(MarginCoordinator, StandaloneFailsClosed) {
    ClockBox clk;
    // No channel at all: every cross-shard need fails closed immediately.
    CrossShardMarginCoordinator c(nullptr, nullptr, opts_for(clk));
    c.set_local_headroom(100, 10'000);
    EXPECT_TRUE(c.covers(10'000, 100));
    EXPECT_EQ(c.begin_reserve(100, 9, 1'000), 0u);
    // Local provider override.
    c.set_local_margin_provider(
        [](void*, uint64_t) noexcept -> int64_t { return 42; }, nullptr);
    EXPECT_TRUE(c.covers(42, 55));
    EXPECT_FALSE(c.covers(43, 55));
}

TEST(MarginCoordinator, ReservationTableFailClosedWhenFull) {
    ClockBox clk;
    FakeCtlChannel ch;  // Silent — everything stays Pending (live slots)
    CrossShardMarginCoordinator c(&ch, nullptr, opts_for(clk));
    uint32_t ok = 0;
    for (uint32_t i = 0; i < CrossShardMarginCoordinator::kMaxReservations + 8;
         ++i) {
        if (c.begin_reserve(100 + i, 9, 1) != 0)
            ++ok;
    }
    EXPECT_EQ(ok, CrossShardMarginCoordinator::kMaxReservations);
    // Full of live reservations -> further requests fail closed (0).
    EXPECT_EQ(c.reqs_sent(), ok);
}

// Evaluate treats hosted locks as negative headroom and stays exact at the
// int64 extremes; an aggregate that would overflow reports covered=false
// (fail closed, spec §2.7.1).
TEST(MarginCoordinator, EvaluateMathExactAndFailClosed) {
    ClockBox clk;
    FakeCtlChannel ch;
    CrossShardMarginCoordinator c(&ch, nullptr, opts_for(clk));
    c.set_local_headroom(100, INT64_MAX - 10);

    MarginReserveReqBody req{};
    req.reservation_id = (uint64_t{9} << 48) | 5;
    req.account_id = 100;
    req.src_shard = 9;
    req.amount = 100;
    ch.push_inbound(MarginCtlType::ReserveReq, req);
    c.poll();
    EXPECT_EQ(c.hosted_locks(100), 100);
    // usable = INT64_MAX-10 - 100 = INT64_MAX-110 — exact, no overflow.
    EXPECT_TRUE(c.evaluate(100, INT64_MAX - 200).covered);
    EXPECT_TRUE(c.evaluate(100, INT64_MAX - 110).covered);   // boundary: equal
    EXPECT_FALSE(c.evaluate(100, INT64_MAX - 109).covered);  // one tick short

    // Real overflow: local INT64_MAX-10 + granted slice -> usable wraps;
    // try_add fails -> covered=false (a wrapped "huge" usable must never
    // admit an order).
    FakeCtlChannel ch2;
    ch2.mode_ = FakeCtlChannel::Mode::AckAll;
    CrossShardMarginCoordinator c2(&ch2, nullptr, opts_for(clk));
    c2.set_local_headroom(100, INT64_MAX - 10);
    const uint64_t id2 = c2.begin_reserve(100, 9, 5'000);
    ASSERT_NE(id2, 0u);
    ASSERT_EQ(c2.resolve_wait(id2), MarginResolve::Granted);
    EXPECT_FALSE(c2.covers(INT64_MAX - 20, 100));  // aggregate overflow -> closed
    EXPECT_FALSE(c2.covers(1, 999));  // unknown account -> 0 headroom
}
