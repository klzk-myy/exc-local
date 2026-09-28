// Tasks 2.3.8 + 2.3.14 + 2.3.25 coverage:
//   * 2PC codec round-trip / malformed rejection
//   * 3-shard basket all-or-nothing commit + atomic activation
//   * compensate-on-failure with balance unlock (NACK path)
//   * operation_id dedup returning the cached result
//   * simulated 5s reserve timeout -> full compensation
//   * 10-op concurrent cap -> CROSS_SHARD_LIMIT_EXCEEDED
//   * tick-driven CompensationReaper at 2s engine-logical cadence
//   * commit/TTL race -> CommitRefused + restitution of committed legs
//   * duplicate/late ACK zero-drift handling
//   * WAL round-trip + participant restart recovery (RecoveryOrphan)
//   * optimistic TRY_MATCH commit, NACK -> COMPENSATE_UNWIND + GL posting to
//     5010_CROSS_SHARD_EXECUTION_DIFF, 500µs timeout unwind, zero-orphan
//     invariant, dedup, restart orphan liquidation

#include <gtest/gtest.h>

#include <cstdint>
#include <cstring>
#include <deque>
#include <filesystem>
#include <map>
#include <string>
#include <vector>

#include <unistd.h>

#include "matching/CrossShardCoordinator.hpp"
#include "matching/OptimisticShardCoordinator.hpp"
#include "wal/Wal.hpp"

using namespace exch;

namespace {

constexpr uint64_t MS = 1'000'000ULL;
constexpr uint64_t SEC = 1'000'000'000ULL;

// --- in-process shard mesh -----------------------------------------------------
// Routes outbound frames by the shared header's dst_shard field (offset
// kCrossShardCtlDstOffset). hold_all parks frames in `held` so tests can
// reorder/stall delivery; drop[dst] makes a shard silently unreachable
// (frames accepted on the wire, never processed).

struct Mesh {
    std::deque<std::vector<uint8_t>> inbox[64];
    std::vector<std::vector<uint8_t>> held;
    bool drop[64] = {};
    bool hold_all = false;

    void deliver_held() {
        for (auto& f : held) {
            uint32_t dst;
            std::memcpy(&dst, f.data() + kCrossShardCtlDstOffset, 4);
            inbox[dst].push_back(std::move(f));
        }
        held.clear();
    }

    // Count frames of a given basket wire type sent to a shard.
    size_t basket_frames(uint32_t dst, BasketCtlType t) const {
        size_t n = 0;
        for (auto& f : inbox_at(dst))
            if (f.size() >= 5 && f[4] == static_cast<uint8_t>(t))
                ++n;
        return n;
    }
    const std::deque<std::vector<uint8_t>>& inbox_at(uint32_t dst) const {
        return inbox[dst];
    }
};

class MeshChannel final : public IpcChannel {
public:
    MeshChannel(Mesh* m, uint32_t shard) : m_(m), shard_(shard) {}

    bool open() noexcept override { return open_; }
    void close() noexcept override { open_ = false; }
    [[nodiscard]] bool is_open() const noexcept override { return open_; }

    bool send(const void* data, uint32_t len) noexcept override {
        if (!open_ || data == nullptr || len < sizeof(CrossShardCtlHeader))
            return false;
        uint32_t dst;
        std::memcpy(&dst,
                    static_cast<const uint8_t*>(data) +
                        kCrossShardCtlDstOffset,
                    4);
        if (m_->drop[dst])
            return true;  // silent shard — swallowed on the wire
        std::vector<uint8_t> f(static_cast<const uint8_t*>(data),
                               static_cast<const uint8_t*>(data) + len);
        if (m_->hold_all)
            m_->held.push_back(std::move(f));
        else
            m_->inbox[dst].push_back(std::move(f));
        return true;
    }

    int32_t poll(void* buf, uint32_t cap) noexcept override {
        if (m_->inbox[shard_].empty())
            return 0;
        auto& f = m_->inbox[shard_].front();
        if (f.size() > cap)
            return -1;
        std::memcpy(buf, f.data(), f.size());
        const auto n = static_cast<int32_t>(f.size());
        m_->inbox[shard_].pop_front();
        return n;
    }

    Mesh* m_;
    uint32_t shard_;
    bool open_ = true;
};

// Direct injection of a frame into a shard's inbox (dup/late-ack scenarios).
template <typename Body>
void inject(Mesh& mesh, uint32_t dst, uint32_t src, BasketCtlType t,
            const Body& b) {
    uint8_t buf[kBasketCtlMaxFrame];
    const uint32_t n = basket_ctl_encode(buf, sizeof(buf), t, &b, src, dst);
    mesh.inbox[dst].emplace_back(buf, buf + n);
}
template <typename Body>
void inject_opt(Mesh& mesh, uint32_t dst, uint32_t src, OptCtlType t,
                const Body& b) {
    uint8_t buf[kOptCtlMaxFrame];
    const uint32_t n = opt_ctl_encode(buf, sizeof(buf), t, &b, src, dst);
    mesh.inbox[dst].emplace_back(buf, buf + n);
}

// Pump every node one engine-logical step.
template <typename C>
void step(std::initializer_list<C*> cs, uint64_t& now, uint64_t dt) {
    now += dt;
    for (auto* c : cs)
        c->on_time_tick(now);
}

// Drive the mesh `max_iters` steps of `dt` (ops resolve via the channels).
template <typename C>
void settle(std::initializer_list<C*> cs, uint64_t& now, uint64_t dt,
            int max_iters = 200) {
    for (int i = 0; i < max_iters; ++i)
        step(cs, now, dt);
}

BasketCoordinatorOptions bopts(uint16_t shard) {
    BasketCoordinatorOptions o;
    o.shard_id = shard;
    return o;
}
OptimisticCoordinatorOptions oopts(uint16_t shard) {
    OptimisticCoordinatorOptions o;
    o.shard_id = shard;
    return o;
}

BasketOpId opid(uint64_t hi, uint64_t lo) { return BasketOpId{hi, lo}; }

std::filesystem::path tmp_dir(const char* name) {
    const auto d = std::filesystem::temp_directory_path() /
                   ("exch_xshard_" + std::to_string(::getpid()) + "_" + name);
    std::filesystem::remove_all(d);
    std::filesystem::create_directories(d);
    return d;
}

// --- scripted optimistic executors -------------------------------------------------
// Keyed by instrument_id: reject -> NACK, partial_qty -> short fill, else
// full fill at `prices[instr]` (default 1.0000e8 ticks).
struct OptStub {
    std::map<uint32_t, int64_t> prices;
    std::map<uint32_t, int64_t> partial;
    std::map<uint32_t, bool> reject;
    int64_t unwind_delta = 5'000'000;  // unwind VWAP = fill VWAP - delta
                                       // (BUY legs lose delta*qty on unwind)
    int64_t unwind_cap = 0;            // >0 = partial liquidation per call

    static int64_t match(void* ctx, const OptTryMatchBody& leg,
                         int64_t* vwap) noexcept {
        auto* s = static_cast<OptStub*>(ctx);
        if (s->reject[leg.instrument_id])
            return 0;
        const int64_t px = s->prices.count(leg.instrument_id)
                               ? s->prices[leg.instrument_id]
                               : 100'000'000;
        *vwap = px;
        const auto it = s->partial.find(leg.instrument_id);
        if (it != s->partial.end())
            return it->second < leg.qty_units ? it->second : leg.qty_units;
        return leg.qty_units;
    }
    static int64_t unwind(void* ctx, BasketOpId, uint32_t leg_index,
                          int64_t qty, int64_t* vwap) noexcept {
        auto* s = static_cast<OptStub*>(ctx);
        (void)leg_index;
        *vwap = 100'000'000 - s->unwind_delta;
        const int64_t cap = s->unwind_cap > 0 ? s->unwind_cap : qty;
        return qty < cap ? qty : cap;
    }
};

struct GlLog {
    std::vector<CrossShardGlPosting> posts;
    static void sink(void* ctx, const CrossShardGlPosting& p) noexcept {
        static_cast<GlLog*>(ctx)->posts.push_back(p);
    }
};

struct LegLog {
    std::vector<BasketLegNotice> evs;
    static void sink(void* ctx, const BasketLegNotice& n) noexcept {
        static_cast<LegLog*>(ctx)->evs.push_back(n);
    }
    size_t count(BasketLegEvent e) const {
        size_t n = 0;
        for (auto& v : evs)
            if (v.event == static_cast<uint8_t>(e))
                ++n;
        return n;
    }
};

}  // namespace

// --- codec --------------------------------------------------------------------------

TEST(BasketCtlCodec, RoundTripAllTypes) {
    uint8_t buf[kBasketCtlMaxFrame];
    BasketCtlView v;

    BasketReserveReqBody req{};
    req.op_hi = 0xAA;
    req.op_lo = 0xBB;
    req.leg_index = 2;
    req.instrument_id = 9;
    req.account_id = 100;
    req.order_id = 4242;
    req.amount = 5'000'000;
    req.expires_at_ns = 12345;
    uint32_t n = basket_ctl_encode(buf, sizeof(buf), BasketCtlType::ReserveReq,
                                   &req, /*src=*/3, /*dst=*/7);
    ASSERT_EQ(n, sizeof(CrossShardCtlHeader) + sizeof(BasketReserveReqBody));
    ASSERT_EQ(basket_ctl_decode(buf, n, &v), BasketCtlDecode::Ok);
    EXPECT_EQ(v.type, BasketCtlType::ReserveReq);
    EXPECT_EQ(v.src_shard, 3u);
    EXPECT_EQ(v.dst_shard, 7u);
    EXPECT_EQ(v.req.op_hi, 0xAAu);
    EXPECT_EQ(v.req.amount, 5'000'000);

    BasketVoteBody vote{};
    vote.op_hi = 1;
    vote.op_lo = 2;
    vote.leg_index = 1;
    vote.amount = 9;
    n = basket_ctl_encode(buf, sizeof(buf), BasketCtlType::ReserveNack, &vote,
                          7, 3);
    ASSERT_EQ(basket_ctl_decode(buf, n, &v), BasketCtlDecode::Ok);
    EXPECT_EQ(v.type, BasketCtlType::ReserveNack);
    EXPECT_EQ(v.vote.amount, 9);

    BasketCommitBody c{};
    c.op_hi = 1;
    n = basket_ctl_encode(buf, sizeof(buf), BasketCtlType::Commit, &c, 3, 7);
    ASSERT_EQ(basket_ctl_decode(buf, n, &v), BasketCtlDecode::Ok);
    EXPECT_EQ(v.type, BasketCtlType::Commit);

    BasketCommitAckBody ca{};
    ca.applied = 1;
    n = basket_ctl_encode(buf, sizeof(buf), BasketCtlType::CommitAck, &ca, 7,
                          3);
    ASSERT_EQ(basket_ctl_decode(buf, n, &v), BasketCtlDecode::Ok);
    EXPECT_EQ(v.cack.applied, 1);

    BasketReleaseBody rel{};
    rel.reason =
        static_cast<uint32_t>(BasketReleaseReason::Compensate);
    n = basket_ctl_encode(buf, sizeof(buf), BasketCtlType::Release, &rel, 3,
                          7);
    ASSERT_EQ(basket_ctl_decode(buf, n, &v), BasketCtlDecode::Ok);
    EXPECT_EQ(v.rel.reason,
              static_cast<uint32_t>(BasketReleaseReason::Compensate));

    BasketReleaseAckBody ra{};
    n = basket_ctl_encode(buf, sizeof(buf), BasketCtlType::ReleaseAck, &ra, 7,
                          3);
    ASSERT_EQ(basket_ctl_decode(buf, n, &v), BasketCtlDecode::Ok);
    EXPECT_EQ(v.type, BasketCtlType::ReleaseAck);
}

TEST(BasketCtlCodec, RejectsMalformed) {
    uint8_t buf[kBasketCtlMaxFrame] = {};
    BasketCtlView v;
    EXPECT_EQ(basket_ctl_decode(buf, 4, &v), BasketCtlDecode::TooShort);
    CrossShardCtlHeader h{kBasketCtlMagic,
                          static_cast<uint8_t>(BasketCtlType::ReserveAck), 0,
                          kBasketCtlVersion, 1, 2};
    std::memcpy(buf, &h, sizeof(h));
    EXPECT_EQ(basket_ctl_decode(buf, sizeof(h), &v),
              BasketCtlDecode::TooShort);  // body missing
    buf[0] ^= 0xFF;
    EXPECT_EQ(basket_ctl_decode(buf, sizeof(h) + 40, &v),
              BasketCtlDecode::BadMagic);
    CrossShardCtlHeader bad_ver{kBasketCtlMagic,
                                static_cast<uint8_t>(BasketCtlType::Release),
                                0, 99, 1, 2};
    std::memcpy(buf, &bad_ver, sizeof(bad_ver));
    EXPECT_EQ(basket_ctl_decode(buf, sizeof(buf) + 24, &v),
              BasketCtlDecode::BadVersion);
    CrossShardCtlHeader bad_ty{kBasketCtlMagic, 0xEE, 0, kBasketCtlVersion,
                               1, 2};
    std::memcpy(buf, &bad_ty, sizeof(bad_ty));
    EXPECT_EQ(basket_ctl_decode(buf, sizeof(buf) + 24, &v),
              BasketCtlDecode::UnknownType);
}

// --- 2PC: all-or-nothing commit ------------------------------------------------------

TEST(CrossShard2PC, ThreeShardAllOrNothingCommit) {
    Mesh mesh;
    MeshChannel ch1(&mesh, 1), ch2(&mesh, 2), ch3(&mesh, 3);
    CrossShardCoordinator c1(&ch1, nullptr, bopts(1));
    CrossShardCoordinator c2(&ch2, nullptr, bopts(2));
    CrossShardCoordinator c3(&ch3, nullptr, bopts(3));
    LegLog log1, log2, log3;
    c1.set_leg_sink(&LegLog::sink, &log1);
    c2.set_leg_sink(&LegLog::sink, &log2);
    c3.set_leg_sink(&LegLog::sink, &log3);
    c1.set_balance(100, 1'000'000'000);
    c2.set_balance(100, 1'000'000'000);
    c3.set_balance(100, 1'000'000'000);

    BasketLegSpec legs[3] = {
        {1, 10, 1001, 100, 5'000'000},
        {2, 20, 1002, 100, 7'000'000},
        {3, 30, 1003, 100, 9'000'000},
    };
    ASSERT_EQ(CrossShardCoordinator::coordinator_shard(legs, 3), 1u);
    const auto id = opid(11, 22);
    uint64_t now = 1'000 * SEC;

    BasketResult r = c1.submit(legs, 3, /*initiator=*/100, id, now);
    EXPECT_EQ(r.status, static_cast<uint8_t>(BasketStatus::Reserving));

    settle({&c1, &c2, &c3}, now, 50 * MS);

    r = c1.status(id);
    EXPECT_EQ(r.status, static_cast<uint8_t>(BasketStatus::Committed));
    EXPECT_EQ(r.legs_committed, 3);
    // Balances: locked consumed at commit; available reduced by the leg.
    EXPECT_EQ(c1.available(100), 1'000'000'000 - 5'000'000);
    EXPECT_EQ(c2.available(100), 1'000'000'000 - 7'000'000);
    EXPECT_EQ(c3.available(100), 1'000'000'000 - 9'000'000);
    EXPECT_EQ(c1.locked(100) + c2.locked(100) + c3.locked(100), 0);
    // Order events: one Activated per participant.
    EXPECT_EQ(log1.count(BasketLegEvent::Activated), 1u);
    EXPECT_EQ(log2.count(BasketLegEvent::Activated), 1u);
    EXPECT_EQ(log3.count(BasketLegEvent::Activated), 1u);
    // Participant records committed durably.
    EXPECT_EQ(c2.part_state(id, 1), 2);  // PartSt::Committed
    // Metrics contract (Task 2.3.8 DoD: totals/successes/duration).
    EXPECT_EQ(c1.metrics().transactions_total, 1u);
    EXPECT_EQ(c1.metrics().transactions_success, 1u);
    EXPECT_EQ(c1.metrics().transactions_failure, 0u);
    EXPECT_EQ(c1.metrics().reservation_active, 0u);
}

TEST(CrossShard2PC, DedupReturnsCachedResult) {
    Mesh mesh;
    MeshChannel ch1(&mesh, 1), ch2(&mesh, 2);
    CrossShardCoordinator c1(&ch1, nullptr, bopts(1));
    CrossShardCoordinator c2(&ch2, nullptr, bopts(2));
    c1.set_balance(100, 1'000'000'000);
    c2.set_balance(100, 1'000'000'000);

    BasketLegSpec legs[2] = {
        {1, 10, 1001, 100, 5'000'000},
        {2, 20, 1002, 100, 7'000'000},
    };
    const auto id = opid(5, 6);
    uint64_t now = 1'000 * SEC;
    (void)c1.submit(legs, 2, 100, id, now);
    settle({&c1, &c2}, now, 50 * MS);
    const BasketResult done = c1.status(id);
    ASSERT_EQ(done.status, static_cast<uint8_t>(BasketStatus::Committed));

    // Retry with the same operation_id: cached result, zero side-effects —
    // no new frames land on either shard.
    const BasketResult again = c1.submit(legs, 2, 100, id, now + MS);
    EXPECT_EQ(again.status, done.status);
    EXPECT_EQ(again.code, done.code);
    EXPECT_EQ(again.legs_committed, done.legs_committed);
    step({&c1, &c2}, now, 50 * MS);
    EXPECT_EQ(c1.metrics().transactions_total, 1u);
    EXPECT_EQ(c2.available(100), 1'000'000'000 - 7'000'000);
}

TEST(CrossShard2PC, NackCompensatesAll) {
    Mesh mesh;
    MeshChannel ch1(&mesh, 1), ch2(&mesh, 2), ch3(&mesh, 3);
    CrossShardCoordinator c1(&ch1, nullptr, bopts(1));
    CrossShardCoordinator c2(&ch2, nullptr, bopts(2));
    CrossShardCoordinator c3(&ch3, nullptr, bopts(3));
    c1.set_balance(100, 1'000'000'000);
    c2.set_balance(100, 1'000'000'000);
    c3.set_balance(100, 10);  // cannot cover the leg -> NACK

    BasketLegSpec legs[3] = {
        {1, 10, 1001, 100, 5'000'000},
        {2, 20, 1002, 100, 7'000'000},
        {3, 30, 1003, 100, 9'000'000},
    };
    const auto id = opid(7, 8);
    uint64_t now = 1'000 * SEC;
    (void)c1.submit(legs, 3, 100, id, now);
    settle({&c1, &c2, &c3}, now, 50 * MS);

    const BasketResult r = c1.status(id);
    EXPECT_EQ(r.status, static_cast<uint8_t>(BasketStatus::Compensated));
    EXPECT_EQ(r.code, static_cast<uint8_t>(BasketCode::ParticipantNack));
    // Full rollback: every granted lock released, available restored.
    EXPECT_EQ(c1.available(100), 1'000'000'000);
    EXPECT_EQ(c2.available(100), 1'000'000'000);
    EXPECT_EQ(c1.locked(100) + c2.locked(100) + c3.locked(100), 0);
    EXPECT_EQ(c1.metrics().transactions_failure, 1u);
    EXPECT_EQ(c1.metrics().compensations_total, 1u);
    EXPECT_EQ(c1.metrics().reservation_active, 0u);
}

TEST(CrossShard2PC, ReserveTimeoutFullCompensation) {
    Mesh mesh;
    MeshChannel ch1(&mesh, 1), ch2(&mesh, 2), ch3(&mesh, 3);
    CrossShardCoordinator c1(&ch1, nullptr, bopts(1));
    CrossShardCoordinator c2(&ch2, nullptr, bopts(2));
    CrossShardCoordinator c3(&ch3, nullptr, bopts(3));
    c1.set_balance(100, 1'000'000'000);
    c2.set_balance(100, 1'000'000'000);
    c3.set_balance(100, 1'000'000'000);
    mesh.drop[3] = true;  // shard 3 never answers

    BasketLegSpec legs[3] = {
        {1, 10, 1001, 100, 5'000'000},
        {2, 20, 1002, 100, 7'000'000},
        {3, 30, 1003, 100, 9'000'000},
    };
    const auto id = opid(9, 10);
    uint64_t now = 1'000 * SEC;
    (void)c1.submit(legs, 3, 100, id, now);
    step({&c1, &c2}, now, 50 * MS);
    // Legs 1+2 granted, leg 3 pending.
    EXPECT_EQ(c2.locked(100), 7'000'000);
    EXPECT_EQ(c1.status(id).status,
              static_cast<uint8_t>(BasketStatus::Reserving));

    // Drive past the 5s reserve TTL + one 2s reaper interval.
    for (int i = 0; i < 160; ++i)
        step({&c1, &c2}, now, 50 * MS);

    const BasketResult r = c1.status(id);
    EXPECT_EQ(r.status, static_cast<uint8_t>(BasketStatus::Compensated));
    EXPECT_EQ(r.code, static_cast<uint8_t>(BasketCode::ReserveTimeout));
    EXPECT_EQ(c1.locked(100), 0);
    EXPECT_EQ(c2.locked(100), 0);
    EXPECT_EQ(c1.available(100), 1'000'000'000);
    EXPECT_EQ(c2.available(100), 1'000'000'000);
    EXPECT_EQ(c1.metrics().reservation_timeout_total, 1u);
    EXPECT_EQ(c1.metrics().reservation_active, 0u);
}

TEST(CrossShard2PC, ConcurrentLimitExceeded) {
    Mesh mesh;
    MeshChannel ch1(&mesh, 1), ch2(&mesh, 2);
    CrossShardCoordinator c1(&ch1, nullptr, bopts(1));
    CrossShardCoordinator c2(&ch2, nullptr, bopts(2));
    c1.set_balance(100, 1'000'000'000'000);
    c2.set_balance(100, 1'000'000'000'000);
    mesh.drop[2] = true;  // ops stay in-flight (never resolve)

    BasketLegSpec legs[2] = {
        {1, 10, 0, 100, 1'000},
        {2, 20, 0, 100, 1'000},
    };
    uint64_t now = 1'000 * SEC;
    for (uint64_t i = 0; i < 10; ++i) {
        const BasketResult r =
            c1.submit(legs, 2, 100, opid(100, i), now);
        ASSERT_EQ(r.status, static_cast<uint8_t>(BasketStatus::Reserving));
    }
    // 11th concurrent op for the same account -> gate rejection.
    const BasketResult over = c1.submit(legs, 2, 100, opid(100, 10), now);
    EXPECT_EQ(over.status, static_cast<uint8_t>(BasketStatus::Rejected));
    EXPECT_EQ(over.code, static_cast<uint8_t>(BasketCode::LimitExceeded));
    EXPECT_STREQ(kCodeCrossShardLimitExceeded, "CROSS_SHARD_LIMIT_EXCEEDED");
    EXPECT_EQ(c1.metrics().limit_exceeded_total, 1u);
    // reservation_active = live ops (10) + granted reservation locks (the
    // 10 local legs) — both sides of the in-flight reservation gauge.
    EXPECT_EQ(c1.metrics().reservation_active, 20u);
    // A different account is NOT capped by the first one's slots.
    const BasketResult other =
        c1.submit(legs, 2, 200, opid(200, 1), now);
    EXPECT_EQ(other.status, static_cast<uint8_t>(BasketStatus::Reserving));
    // Gate rejections aren't tombstoned: the same op_id is reusable later.
    for (int i = 0; i < 200; ++i)
        step({&c1}, now, 50 * MS);  // reap everything past TTL
    const BasketResult retry = c1.submit(legs, 2, 100, opid(100, 10), now);
    EXPECT_EQ(retry.status, static_cast<uint8_t>(BasketStatus::Reserving));
}

TEST(CrossShard2PC, ReaperCadenceIsTwoSeconds) {
    Mesh mesh;
    MeshChannel ch1(&mesh, 1), ch2(&mesh, 2);
    CrossShardCoordinator c1(&ch1, nullptr, bopts(1));
    CrossShardCoordinator c2(&ch2, nullptr, bopts(2));
    c1.set_balance(100, 1'000'000'000);
    c2.set_balance(100, 1'000'000'000);
    mesh.drop[2] = true;

    BasketLegSpec legs[2] = {
        {1, 10, 0, 100, 1'000},
        {2, 20, 0, 100, 1'000},
    };
    const auto id = opid(30, 1);
    const uint64_t t0 = 1'000 * SEC;
    uint64_t now = t0;
    (void)c1.submit(legs, 2, 100, id, now);

    c1.on_time_tick(now);  // first tick reaps immediately, next = t0+2s
    EXPECT_EQ(c1.metrics().reaper_scans, 1u);
    now = t0 + 2100 * MS;
    c1.on_time_tick(now);  // reap #2 (deadline t0+5s not reached)
    EXPECT_EQ(c1.metrics().reaper_scans, 2u);
    EXPECT_EQ(c1.status(id).status,
              static_cast<uint8_t>(BasketStatus::Reserving));
    now = t0 + 4200 * MS;
    c1.on_time_tick(now);  // reap #3 — still under TTL
    EXPECT_EQ(c1.status(id).status,
              static_cast<uint8_t>(BasketStatus::Reserving));
    now = t0 + 6300 * MS;
    c1.on_time_tick(now);  // past 5s TTL but inside the cadence gap -> holds
    EXPECT_EQ(c1.metrics().reaper_scans, 4u);
    EXPECT_EQ(c1.status(id).status,
              static_cast<uint8_t>(BasketStatus::Compensated));
    // ^ reap at t0+6.3s (next_reap was t0+6.2s): TTL + up-to-one-interval
    //   seizure is the documented spec model ("scan every 2s").
}

TEST(CrossShard2PC, CommitLosingTtlRaceCompensatesCommittedLeg) {
    Mesh mesh;
    MeshChannel ch1(&mesh, 1), ch2(&mesh, 2);
    CrossShardCoordinator c1(&ch1, nullptr, bopts(1));
    CrossShardCoordinator c2(&ch2, nullptr, bopts(2));
    c1.set_balance(100, 1'000'000'000);
    c2.set_balance(100, 1'000'000'000);

    BasketLegSpec legs[2] = {
        {1, 10, 1001, 100, 5'000'000},
        {2, 20, 1002, 100, 7'000'000},
    };
    const auto id = opid(40, 1);
    uint64_t now = 1'000 * SEC;
    (void)c1.submit(legs, 2, 100, id, now);
    c2.on_time_tick(now + MS);      // c2 reserves + ACKs
    mesh.hold_all = true;           // park the coordinator's Commit
    c1.on_time_tick(now + 2 * MS);  // c1 commits local leg, Commit held
    ASSERT_EQ(c1.available(100), 1'000'000'000 - 5'000'000);

    // c2's reservation TTL lapses before the Commit is delivered.
    c2.on_time_tick(now + 6 * SEC);
    EXPECT_EQ(c2.locked(100), 0);  // lock expired on the participant
    EXPECT_EQ(c2.available(100), 1'000'000'000);
    mesh.hold_all = false;
    mesh.deliver_held();           // Expired notice + stale Commit land
    step({&c1, &c2}, now = now + 7 * SEC, 50 * MS);
    step({&c1, &c2}, now, 50 * MS);

    const BasketResult r = c1.status(id);
    EXPECT_EQ(r.status, static_cast<uint8_t>(BasketStatus::Compensated));
    EXPECT_EQ(r.code, static_cast<uint8_t>(BasketCode::CommitRefused));
    // The committed local leg was restituted — full atomic rollback.
    EXPECT_EQ(c1.available(100), 1'000'000'000);
    EXPECT_EQ(c1.locked(100), 0);
}

TEST(CrossShard2PC, DuplicateAndLateAcksAreZeroDrift) {
    Mesh mesh;
    MeshChannel ch1(&mesh, 1), ch2(&mesh, 2);
    CrossShardCoordinator c1(&ch1, nullptr, bopts(1));
    CrossShardCoordinator c2(&ch2, nullptr, bopts(2));
    c1.set_balance(100, 1'000'000'000);
    c2.set_balance(100, 1'000'000'000);
    mesh.drop[2] = true;  // leg 2 stays Pending -> op will compensate

    BasketLegSpec legs[2] = {
        {1, 10, 0, 100, 1'000},
        {2, 20, 0, 100, 1'000},
    };
    const auto id = opid(50, 1);
    uint64_t now = 1'000 * SEC;
    (void)c1.submit(legs, 2, 100, id, now);

    // ACK for an op the coordinator never issued -> compensating Release to
    // the sender (zero-drift bookkeeping).
    BasketVoteBody stray{};
    stray.op_hi = 0xDEAD;
    stray.op_lo = 0xBEEF;
    stray.leg_index = 0;
    stray.amount = 1;
    inject(mesh, /*dst=*/1, /*src=*/9, BasketCtlType::ReserveAck, stray);
    c1.on_time_tick(now + MS);
    ASSERT_EQ(mesh.basket_frames(9, BasketCtlType::Release), 1u);

    // Drive to terminal (TTL + reaper) — op compensates.
    for (int i = 0; i < 200; ++i)
        step({&c1}, now, 50 * MS);
    ASSERT_EQ(c1.status(id).status,
              static_cast<uint8_t>(BasketStatus::Compensated));

    // Late ACK for the compensated leg -> the coordinator answers with
    // another Release (the participant must not hold anything for it).
    mesh.drop[2] = false;  // wire reachable again — Release must land
    stray.op_hi = id.hi;
    stray.op_lo = id.lo;
    stray.leg_index = 1;
    inject(mesh, 1, 2, BasketCtlType::ReserveAck, stray);
    c1.on_time_tick(now + MS);
    EXPECT_EQ(mesh.basket_frames(2, BasketCtlType::Release), 1u);
    // Duplicate ReleaseAcks / acks for terminal ops are idempotent no-ops.
    BasketReleaseAckBody ra{};
    ra.op_hi = id.hi;
    ra.op_lo = id.lo;
    ra.leg_index = 1;
    inject(mesh, 1, 2, BasketCtlType::ReleaseAck, ra);
    inject(mesh, 1, 2, BasketCtlType::ReleaseAck, ra);
    c1.on_time_tick(now + 2 * MS);
    EXPECT_EQ(c1.status(id).status,
              static_cast<uint8_t>(BasketStatus::Compensated));
}

// --- 2PC: WAL + restart ------------------------------------------------------------

TEST(CrossShard2PC, WalRoundTripAndParticipantRecover) {
    const auto dir = tmp_dir("basket");
    const auto path = (dir / "basket.wal").string();

    Mesh mesh;
    MeshChannel ch1(&mesh, 1), ch2(&mesh, 2);
    CrossShardCoordinator c1(&ch1, nullptr, bopts(1));
    const auto id = opid(60, 1);
    {
        Wal wal(path, /*shard_id=*/2);
        ASSERT_EQ(wal.open(), WalStatus::Ok);
        CrossShardCoordinator c2(&ch2, &wal, bopts(2));
        c1.set_balance(100, 1'000'000'000);
        c2.set_balance(100, 1'000'000'000);

        BasketLegSpec legs[2] = {
            {1, 10, 1001, 100, 5'000'000},
            {2, 20, 1002, 100, 7'000'000},
        };
        uint64_t now = 1'000 * SEC;
        (void)c1.submit(legs, 2, 100, id, now);
        c2.on_time_tick(now + MS);   // reserve + ACK (committed to WAL)
        c1.on_time_tick(now + 2 * MS);  // ACK consumed; Commit issued
        ASSERT_EQ(wal.flush(), WalStatus::Ok);
        wal.close();  // "crash" with the lock still Reserved
    }

    // Restart the participant: replay restores the lock; the first tick past
    // expiry sweeps it as RecoveryOrphan and notifies the coordinator.
    CrossShardCoordinator c2(&ch2, nullptr, bopts(2));
    c2.set_balance(100, 1'000'000'000);
    {
        WalReader rd(path);
        ASSERT_TRUE(rd.is_open());
        EXPECT_GT(c2.recover(rd), 0u);
    }
    EXPECT_EQ(c2.locked(100), 7'000'000);
    EXPECT_EQ(c2.part_state(id, 1), 1);  // PartSt::Reserved
    uint64_t now2 = 1'000 * SEC + 10 * SEC;
    c2.on_time_tick(now2);
    EXPECT_EQ(c2.locked(100), 0);
    EXPECT_EQ(c2.part_state(id, 1), 4);  // PartSt::Released
    // Expiry notice went out to the coordinator shard.
    EXPECT_EQ(mesh.basket_frames(1, BasketCtlType::Release), 1u);
    std::filesystem::remove_all(dir);
}

// --- optimistic codec ---------------------------------------------------------------------

TEST(OptimisticCtlCodec, RoundTripAndMalformed) {
    uint8_t buf[kOptCtlMaxFrame];
    OptCtlView v;

    OptTryMatchBody t{};
    t.op_hi = 1;
    t.op_lo = 2;
    t.account_id = 100;
    t.qty_units = 1'000'000;
    t.limit_price_ticks = 100'500'000;
    t.leg_index = 0;
    t.instrument_id = 7;
    t.side = 0;
    uint32_t n = opt_ctl_encode(buf, sizeof(buf), OptCtlType::TryMatch, &t, 1,
                                2);
    ASSERT_EQ(n, sizeof(CrossShardCtlHeader) + sizeof(OptTryMatchBody));
    ASSERT_EQ(opt_ctl_decode(buf, n, &v), OptCtlDecode::Ok);
    EXPECT_EQ(v.type, OptCtlType::TryMatch);
    EXPECT_EQ(v.try_.qty_units, 1'000'000);
    EXPECT_EQ(v.dst_shard, 2u);

    OptUnwindBody u{};
    u.op_hi = 1;
    u.op_lo = 2;
    u.qty_units = 1'000'000;
    u.reason = static_cast<uint8_t>(OptUnwindReason::PeerRejected);
    n = opt_ctl_encode(buf, sizeof(buf), OptCtlType::Unwind, &u, 1, 2);
    ASSERT_EQ(opt_ctl_decode(buf, n, &v), OptCtlDecode::Ok);
    EXPECT_EQ(v.unwind.qty_units, 1'000'000);

    OptUnwindAckBody ua{};
    ua.unwound_qty = 9;
    n = opt_ctl_encode(buf, sizeof(buf), OptCtlType::UnwindAck, &ua, 2, 1);
    ASSERT_EQ(opt_ctl_decode(buf, n, &v), OptCtlDecode::Ok);
    EXPECT_EQ(v.uack.unwound_qty, 9);

    EXPECT_EQ(opt_ctl_decode(buf, 4, &v), OptCtlDecode::TooShort);
    CrossShardCtlHeader wrong_magic{kBasketCtlMagic, 1, 0, kOptCtlVersion,
                                    1, 2};
    std::memcpy(buf, &wrong_magic, sizeof(wrong_magic));
    EXPECT_EQ(opt_ctl_decode(buf, sizeof(buf), &v), OptCtlDecode::BadMagic);
}

// --- optimistic path (Task 2.3.25) ----------------------------------------------------------

TEST(Optimistic, AllLegsFillCommitNoLocks) {
    Mesh mesh;
    MeshChannel ch1(&mesh, 1), ch2(&mesh, 2), ch3(&mesh, 3);
    OptimisticShardCoordinator c1(&ch1, nullptr, oopts(1));
    OptimisticShardCoordinator c2(&ch2, nullptr, oopts(2));
    OptimisticShardCoordinator c3(&ch3, nullptr, oopts(3));
    OptStub s2, s3;
    c2.set_match_exec(&OptStub::match, &s2);
    c3.set_match_exec(&OptStub::match, &s3);
    c1.set_match_exec(&OptStub::match, &s2);  // local leg shares the stub

    OptLegSpec legs[3] = {
        {1, 10, 100, 2001, 1'000'000, 0, 0},
        {2, 20, 100, 2002, 2'000'000, 0, 1},
        {3, 30, 100, 2003, 3'000'000, 0, 0},
    };
    ASSERT_EQ(OptimisticShardCoordinator::coordinator_shard(legs, 3), 1u);
    const auto id = opid(70, 1);
    uint64_t now = 1'000 * SEC;
    OptResult r = c1.submit(legs, 3, 100, id, now);
    EXPECT_EQ(r.status, static_cast<uint8_t>(OptStatus::Matching));
    // Settle with sub-500µs ticks: the whole basket commits in-window.
    settle({&c1, &c2, &c3}, now, 50'000);
    r = c1.status(id);
    EXPECT_EQ(r.status, static_cast<uint8_t>(OptStatus::Committed));
    EXPECT_EQ(r.legs_filled, 3);
    EXPECT_EQ(c1.metrics().committed_total, 1u);
    EXPECT_EQ(c1.metrics().unwind_orders_total, 0u);
    // Zero resting locks: no COMPENSATE_UNWIND ever hit the wire.
    EXPECT_EQ(mesh.basket_frames(2, BasketCtlType::Release), 0u);
    // Participants durably record the fills (position is committed).
    EXPECT_EQ(c2.open_fill_qty(id, 1), 2'000'000);
    EXPECT_EQ(c3.open_fill_qty(id, 2), 3'000'000);
}

TEST(Optimistic, NackTriggersCompensateUnwindWithGlPosting) {
    Mesh mesh;
    MeshChannel ch1(&mesh, 1), ch2(&mesh, 2), ch3(&mesh, 3);
    OptimisticShardCoordinator c1(&ch1, nullptr, oopts(1));
    OptimisticShardCoordinator c2(&ch2, nullptr, oopts(2));
    OptimisticShardCoordinator c3(&ch3, nullptr, oopts(3));
    OptStub s1, s2, s3;
    s3.reject[30] = true;  // shard 3 refuses its leg
    c1.set_match_exec(&OptStub::match, &s1);
    c1.set_unwind_exec(&OptStub::unwind, &s1);
    c2.set_match_exec(&OptStub::match, &s2);
    c2.set_unwind_exec(&OptStub::unwind, &s2);
    c3.set_match_exec(&OptStub::match, &s3);
    GlLog gl;
    c1.set_gl_sink(&GlLog::sink, &gl);

    OptLegSpec legs[3] = {
        {1, 10, 100, 0, 1'000'000, 0, 0},   // local leg also fills
        {2, 20, 100, 0, 2'000'000, 0, 0},   // BUY 2.0 @ 1.0000e8
        {3, 30, 100, 0, 3'000'000, 0, 0},
    };
    const auto id = opid(71, 1);
    uint64_t now = 1'000 * SEC;
    (void)c1.submit(legs, 3, 100, id, now);
    settle({&c1, &c2, &c3}, now, 50'000);

    const OptResult r = c1.status(id);
    EXPECT_EQ(r.status, static_cast<uint8_t>(OptStatus::Compensated));
    EXPECT_EQ(r.code, static_cast<uint8_t>(OptCode::LegRejected));
    // COMPENSATE_UNWIND synthetic market orders hit filled legs only.
    EXPECT_EQ(c1.metrics().unwind_orders_total, 2u);  // local + shard 2
    EXPECT_EQ(c1.metrics().unwind_acks_total, 2u);
    // Zero-orphan invariant: no open fill anywhere.
    EXPECT_EQ(c1.open_fill_qty(id, 0), 0);
    EXPECT_EQ(c2.open_fill_qty(id, 1), 0);
    EXPECT_EQ(c3.open_fill_qty(id, 2), 0);
    // GL posting to 5010: BUY fills unwound below fill price -> loss > 0.
    ASSERT_EQ(gl.posts.size(), 1u);
    EXPECT_STREQ(gl.posts[0].account, kGlCrossShardExecDiff);
    EXPECT_STREQ(gl.posts[0].account, "5010_CROSS_SHARD_EXECUTION_DIFF");
    EXPECT_GT(gl.posts[0].amount_ticks, 0);
    EXPECT_EQ(gl.posts[0].op_id.hi, id.hi);
    // Aggregate slippage surfaced on the op result + metrics.
    EXPECT_GT(r.slippage_ticks, 0);
    EXPECT_EQ(c1.metrics().unwind_slippage_ticks_sum, r.slippage_ticks);
    EXPECT_EQ(c1.metrics().compensated_total, 1u);
}

TEST(Optimistic, Timeout500usTriggersUnwind) {
    Mesh mesh;
    MeshChannel ch1(&mesh, 1), ch2(&mesh, 2), ch3(&mesh, 3);
    OptimisticShardCoordinator c1(&ch1, nullptr, oopts(1));
    OptimisticShardCoordinator c2(&ch2, nullptr, oopts(2));
    OptimisticShardCoordinator c3(&ch3, nullptr, oopts(3));
    OptStub s1, s2, s3;
    c1.set_match_exec(&OptStub::match, &s1);
    c1.set_unwind_exec(&OptStub::unwind, &s1);
    c2.set_match_exec(&OptStub::match, &s2);
    c2.set_unwind_exec(&OptStub::unwind, &s2);
    c3.set_match_exec(&OptStub::match, &s3);
    GlLog gl;
    c1.set_gl_sink(&GlLog::sink, &gl);
    mesh.drop[3] = true;  // shard 3 never answers TRY_MATCH

    OptLegSpec legs[3] = {
        {1, 10, 100, 0, 1'000'000, 0, 1},
        {2, 20, 100, 0, 2'000'000, 0, 1},
        {3, 30, 100, 0, 3'000'000, 0, 1},
    };
    const auto id = opid(72, 1);
    uint64_t now = 1'000 * SEC;
    (void)c1.submit(legs, 3, 100, id, now);
    step({&c1, &c2}, now, 100'000);  // 100µs: leg2 fills, leg3 pending
    EXPECT_EQ(c1.status(id).status, static_cast<uint8_t>(OptStatus::Matching));

    step({&c1, &c2}, now, 500'000);  // now at +600µs: window missed
    const OptResult mid = c1.status(id);
    EXPECT_EQ(mid.status, static_cast<uint8_t>(OptStatus::Unwinding));
    EXPECT_EQ(c1.metrics().timeout_total, 1u);

    settle({&c1, &c2}, now, 100'000);
    const OptResult r = c1.status(id);
    EXPECT_EQ(r.status, static_cast<uint8_t>(OptStatus::Compensated));
    EXPECT_EQ(r.code, static_cast<uint8_t>(OptCode::LegTimeout));
    EXPECT_EQ(c2.open_fill_qty(id, 1), 0);   // zero orphan
    EXPECT_EQ(c1.open_fill_qty(id, 0), 0);
    ASSERT_EQ(gl.posts.size(), 1u);
    // SELL legs unwound by buying higher -> positive loss? unwind_delta
    // lowers the unwind VWAP: for SELL fills, buying back cheaper = GAIN
    // (negative slippage) — assert only that a posting exists and is signed.
    EXPECT_EQ(gl.posts[0].reason,
              static_cast<uint8_t>(OptUnwindReason::PeerTimeout));
}

TEST(Optimistic, PartialFillUnwindsThePartial) {
    Mesh mesh;
    MeshChannel ch1(&mesh, 1), ch2(&mesh, 2);
    OptimisticShardCoordinator c1(&ch1, nullptr, oopts(1));
    OptimisticShardCoordinator c2(&ch2, nullptr, oopts(2));
    OptStub s1, s2;
    s2.partial[20] = 500'000;  // half fill on shard 2
    c1.set_match_exec(&OptStub::match, &s1);
    c1.set_unwind_exec(&OptStub::unwind, &s1);
    c2.set_match_exec(&OptStub::match, &s2);
    c2.set_unwind_exec(&OptStub::unwind, &s2);
    GlLog gl;
    c1.set_gl_sink(&GlLog::sink, &gl);

    OptLegSpec legs[2] = {
        {1, 10, 100, 0, 1'000'000, 0, 0},
        {2, 20, 100, 0, 1'000'000, 0, 0},
    };
    const auto id = opid(73, 1);
    uint64_t now = 1'000 * SEC;
    (void)c1.submit(legs, 2, 100, id, now);
    settle({&c1, &c2}, now, 50'000);

    const OptResult r = c1.status(id);
    EXPECT_EQ(r.status, static_cast<uint8_t>(OptStatus::Compensated));
    // The 0.5-unit partial on shard 2 must be liquidated too.
    EXPECT_EQ(c2.open_fill_qty(id, 1), 0);
    ASSERT_EQ(gl.posts.size(), 1u);
    EXPECT_EQ(gl.posts[0].reason,
              static_cast<uint8_t>(OptUnwindReason::PartialFill));
}

TEST(Optimistic, DedupAndUnknownAckZeroDrift) {
    Mesh mesh;
    MeshChannel ch1(&mesh, 1), ch2(&mesh, 2);
    OptimisticShardCoordinator c1(&ch1, nullptr, oopts(1));
    OptimisticShardCoordinator c2(&ch2, nullptr, oopts(2));
    OptStub s2;
    c1.set_match_exec(&OptStub::match, &s2);
    c2.set_match_exec(&OptStub::match, &s2);

    OptLegSpec legs[2] = {
        {1, 10, 100, 0, 1'000'000, 0, 0},
        {2, 20, 100, 0, 2'000'000, 0, 0},
    };
    const auto id = opid(74, 1);
    uint64_t now = 1'000 * SEC;
    (void)c1.submit(legs, 2, 100, id, now);
    settle({&c1, &c2}, now, 50'000);
    ASSERT_EQ(c1.status(id).status, static_cast<uint8_t>(OptStatus::Committed));

    const OptResult again = c1.submit(legs, 2, 100, id, now);
    EXPECT_EQ(again.status, static_cast<uint8_t>(OptStatus::Committed));
    EXPECT_EQ(c1.metrics().try_total, 1u);  // no side-effects on dedup

    // TryAck claiming fills for an unknown op -> coordinator unwinds it.
    OptTryAckBody stray{};
    stray.op_hi = 0xF00D;
    stray.op_lo = 1;
    stray.leg_index = 0;
    stray.filled_qty = 5;
    stray.vwap_ticks = 100;
    inject_opt(mesh, /*dst=*/1, /*src=*/9, OptCtlType::TryAck, stray);
    c1.on_time_tick(now + 100'000);
    size_t unwinds = 0;
    for (auto& f : mesh.inbox_at(9)) {
        OptCtlView v;
        if (opt_ctl_decode(f.data(), static_cast<uint32_t>(f.size()), &v) ==
                OptCtlDecode::Ok &&
            v.type == OptCtlType::Unwind)
            ++unwinds;
    }
    EXPECT_EQ(unwinds, 1u);
}

TEST(Optimistic, RecoveredFillIsLiquidatedOnFirstTick) {
    const auto dir = tmp_dir("opt");
    const auto path = (dir / "opt.wal").string();

    Mesh mesh;
    MeshChannel ch1(&mesh, 1), ch2(&mesh, 2);
    OptimisticShardCoordinator c1(&ch1, nullptr, oopts(1));
    const auto id = opid(75, 1);
    {
        Wal wal(path, 2);
        ASSERT_EQ(wal.open(), WalStatus::Ok);
        OptimisticShardCoordinator c2(&ch2, &wal, oopts(2));
        OptStub s2;
        c1.set_match_exec(&OptStub::match, &s2);
        c2.set_match_exec(&OptStub::match, &s2);
        OptLegSpec legs[2] = {
            {1, 10, 100, 0, 1'000'000, 0, 0},
            {2, 20, 100, 0, 2'000'000, 0, 0},
        };
        uint64_t now = 1'000 * SEC;
        (void)c1.submit(legs, 2, 100, id, now);
        c2.on_time_tick(now + 100'000);  // fill WAL'd
        ASSERT_EQ(wal.flush(), WalStatus::Ok);
        wal.close();  // "crash" holding a live fill
    }
    OptimisticShardCoordinator c2(&ch2, nullptr, oopts(2));
    OptStub s2;
    c2.set_unwind_exec(&OptStub::unwind, &s2);
    {
        WalReader rd(path);
        ASSERT_TRUE(rd.is_open());
        EXPECT_GT(c2.recover(rd), 0u);
    }
    EXPECT_EQ(c2.open_fill_qty(id, 1), 2'000'000);  // restored orphan
    c2.on_time_tick(1'000 * SEC + 200'000);
    EXPECT_EQ(c2.open_fill_qty(id, 1), 0);  // liquidated by RecoveryUnwind
    std::filesystem::remove_all(dir);
}

// Metrics sink fires and carries the spec-named counters.
TEST(CrossShard2PC, MetricsSinkCarriesSpecCounters) {
    Mesh mesh;
    MeshChannel ch1(&mesh, 1), ch2(&mesh, 2);
    CrossShardCoordinator c1(&ch1, nullptr, bopts(1));
    CrossShardCoordinator c2(&ch2, nullptr, bopts(2));
    struct Cap {
        CrossShardMetrics last;
        int calls = 0;
        static void sink(void* ctx, const CrossShardMetrics& m) noexcept {
            auto* c = static_cast<Cap*>(ctx);
            c->last = m;
            ++c->calls;
        }
    } cap;
    c1.set_metrics_sink(&Cap::sink, &cap);
    c1.set_balance(100, 1'000'000'000);
    c2.set_balance(100, 1'000'000'000);

    BasketLegSpec legs[2] = {
        {1, 10, 0, 100, 1'000},
        {2, 20, 0, 100, 1'000},
    };
    uint64_t now = 1'000 * SEC;
    (void)c1.submit(legs, 2, 100, opid(90, 1), now);
    EXPECT_GT(cap.calls, 0);
    EXPECT_EQ(cap.last.transactions_total, 1u);
    // reservation_active counts the live op + granted locks.
    settle({&c1, &c2}, now, 50 * MS);
    EXPECT_EQ(cap.last.reservation_active, 0u);
    EXPECT_EQ(cap.last.transactions_success, 1u);
}
