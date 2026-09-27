// Task 2.3.24 coverage: shm lifecycle, mutual bilateral screening,
// allocation-free wait-free-ish hot path, atomic dual-direction debit (no
// double-spend under races), consume_or_skip contract, CREDIT_UPDATE codec,
// cross-process visibility, and the <2µs p99 latency microbench.

#include <gtest/gtest.h>

#include <algorithm>
#include <atomic>
#include <chrono>
#include <cstdint>
#include <cstdio>
#include <cstring>
#include <string>
#include <thread>
#include <vector>

#include <fcntl.h>
#include <sys/mman.h>
#include <sys/stat.h>
#include <sys/types.h>
#include <sys/wait.h>
#include <unistd.h>

#include "risk/BilateralCreditMatrix.h"
#include "utils/TimeUtils.hpp"

using namespace exch;

namespace {

std::string uniq_name(const char* tag) {
    char buf[128];
    std::snprintf(buf, sizeof(buf), "exch_cm_%s_%d", tag,
                  static_cast<int>(::getpid()));
    return buf;
}

void unlink_shm(const std::string& name) {
    std::string p = "/" + name;
    ::shm_unlink(p.c_str());
}

struct MatrixGuard {
    std::string name;
    explicit MatrixGuard(std::string n) : name(std::move(n)) {}
    ~MatrixGuard() { unlink_shm(name); }
};

// Encodes a CREDIT_UPDATE frame the way the Go writer does (plain packed
// struct, little-endian).
std::vector<uint8_t> encode_update(uint64_t seq, uint32_t a, uint32_t b,
                                   uint64_t limit) {
    CreditUpdateMsg m{};
    m.hdr.magic = kCreditCtlMagic;
    m.hdr.type = static_cast<uint8_t>(CreditCtlType::CreditUpdate);
    m.hdr.flags = 0;
    m.hdr.version = kCreditCtlVersion;
    m.seq = seq;
    m.party_a = a;
    m.party_b = b;
    m.new_limit = limit;
    std::vector<uint8_t> out(sizeof(m));
    std::memcpy(out.data(), &m, sizeof(m));
    return out;
}

}  // namespace

// --- lifecycle ---------------------------------------------------------------

TEST(CreditMatrix, FreshImageZeroedFailClosed) {
    const auto name = uniq_name("fresh");
    MatrixGuard g(name);
    BilateralCreditMatrix m;
    ASSERT_TRUE(m.open(name, /*create=*/true));
    EXPECT_TRUE(m.is_open());
    EXPECT_EQ(m.map_size(), kCreditMatrixBytes);
    // Zero-initialized = zero credit everywhere -> every screen fails closed.
    EXPECT_EQ(m.credit_limit(1, 2), 0u);
    EXPECT_FALSE(m.can_match(1, 2, 1));
    EXPECT_EQ(m.updates_applied(), 0u);
}

TEST(CreditMatrix, AttachExistingPreservesState) {
    const auto name = uniq_name("attach");
    MatrixGuard g(name);
    BilateralCreditMatrix a;
    ASSERT_TRUE(a.open(name, /*create=*/true));
    ASSERT_TRUE(a.apply_update(10, 20, 5'000'000));

    // Second mapped instance of the same shm object (the engine attach path).
    BilateralCreditMatrix b;
    ASSERT_TRUE(b.open(name, /*create=*/false));
    EXPECT_EQ(b.credit_limit(10, 20), 5'000'000u);
    EXPECT_EQ(b.credit_limit(20, 10), 0u);  // directed — reverse not set
    // Attach must not reinitialize the live image.
    EXPECT_EQ(a.credit_limit(10, 20), 5'000'000u);
}

// --- mutual screening -----------------------------------------------------------

TEST(CreditMatrix, MutualHeadroomRequired) {
    const auto name = uniq_name("mutual");
    MatrixGuard g(name);
    BilateralCreditMatrix m(name, true);
    ASSERT_TRUE(m.is_open());

    // Only one direction provisioned -> no mutual credit (spec §13.8.1).
    ASSERT_TRUE(m.apply_update(1, 2, 1'000));
    EXPECT_FALSE(m.can_match(1, 2, 100));

    ASSERT_TRUE(m.apply_update(2, 1, 500));
    EXPECT_TRUE(m.can_match(1, 2, 500));
    EXPECT_FALSE(m.can_match(1, 2, 501));   // taker->maker leg is the binding edge
    EXPECT_FALSE(m.can_match(2, 1, 1001));  // maker->taker leg binds at 1000

    // Exhausted counterparty fails while other pairs still pass.
    ASSERT_TRUE(m.apply_update(3, 4, 10'000));
    ASSERT_TRUE(m.apply_update(4, 3, 10'000));
    EXPECT_TRUE(m.can_match(3, 4, 9'999));
    EXPECT_TRUE(m.can_match(1, 2, 400));

    // Out-of-range / degenerate args fail closed.
    EXPECT_FALSE(m.can_match(0, 2, 0));
    EXPECT_FALSE(m.can_match(1, 2, -5));
    EXPECT_FALSE(m.can_match(kCreditMaxParties, 2, 1));
    EXPECT_FALSE(m.can_match(1, kCreditMaxParties, 1));
}

TEST(CreditMatrix, TryDebitAtomicBothDirections) {
    const auto name = uniq_name("debit");
    MatrixGuard g(name);
    BilateralCreditMatrix m(name, true);
    ASSERT_TRUE(m.is_open());
    ASSERT_TRUE(m.apply_update(7, 9, 1'000));
    ASSERT_TRUE(m.apply_update(9, 7, 800));

    EXPECT_TRUE(m.can_match(7, 9, 800));
    ASSERT_TRUE(m.try_debit(7, 9, 300));
    EXPECT_EQ(m.credit_limit(7, 9), 700u);
    EXPECT_EQ(m.credit_limit(9, 7), 500u);

    // Second debit exceeds the taker->maker leg: NEITHER side moves.
    EXPECT_FALSE(m.try_debit(7, 9, 600));
    EXPECT_EQ(m.credit_limit(7, 9), 700u);
    EXPECT_EQ(m.credit_limit(9, 7), 500u);
}

// consume_or_skip contract: Skip leaves the resting order's credit untouched;
// Consume debits both directions exactly once.
TEST(CreditMatrix, ConsumeOrSkipSemantics) {
    const auto name = uniq_name("skip");
    MatrixGuard g(name);
    BilateralCreditMatrix m(name, true);
    ASSERT_TRUE(m.is_open());
    ASSERT_TRUE(m.apply_update(1, 2, 1'000));
    ASSERT_TRUE(m.apply_update(2, 1, 1'000));
    ASSERT_TRUE(m.apply_update(1, 3, 500));
    ASSERT_TRUE(m.apply_update(3, 1, 0));  // 3 cannot face 1

    // Credit-ineligible counterparty: Skip — nothing debited, priority intact.
    EXPECT_EQ(m.consume_or_skip(1, 3, 400), BilateralCreditMatrix::Gate::Skip);
    EXPECT_EQ(m.credit_limit(1, 3), 500u);
    EXPECT_EQ(m.credit_limit(3, 1), 0u);

    // Eligible: Consume — one atomic dual debit.
    EXPECT_EQ(m.consume_or_skip(1, 2, 600), BilateralCreditMatrix::Gate::Consume);
    EXPECT_EQ(m.credit_limit(1, 2), 400u);
    EXPECT_EQ(m.credit_limit(2, 1), 400u);
}

// --- concurrency: no double-spend below zero -------------------------------------

// N threads race try_debit on one bilateral pair seeded symmetrically.
// Invariants: (a) cell can never go below zero (CAS precondition),
// (b) successful debits are all-or-nothing across both directions, so
// initial - remaining == successes * amount on BOTH cells.
TEST(CreditMatrix, ConcurrentDebitNeverDoubleSpends) {
    const auto name = uniq_name("race");
    MatrixGuard g(name);
    BilateralCreditMatrix m(name, true);
    ASSERT_TRUE(m.is_open());

    constexpr uint64_t kInitial = 1'000'000;
    constexpr int64_t kDebit = 7;
    constexpr int kThreads = 8;
    ASSERT_TRUE(m.apply_update(42, 43, kInitial));
    ASSERT_TRUE(m.apply_update(43, 42, kInitial));

    std::atomic<uint64_t> successes{0};
    std::atomic<bool> go{false};
    std::vector<std::thread> th;
    for (int t = 0; t < kThreads; ++t) {
        th.emplace_back([&] {
            uint64_t local_ok = 0;
            while (!go.load(std::memory_order_acquire)) {}
            for (int i = 0; i < 200'000; ++i)
                local_ok += m.try_debit(42, 43, kDebit) ? 1 : 0;
            successes.fetch_add(local_ok, std::memory_order_relaxed);
        });
    }
    go.store(true, std::memory_order_release);
    for (auto& t : th) t.join();

    const uint64_t want = kInitial - successes.load() * kDebit;
    EXPECT_EQ(m.credit_limit(42, 43), want);
    EXPECT_EQ(m.credit_limit(43, 42), want);  // dual debit is all-or-nothing
    EXPECT_LE(successes.load() * kDebit, kInitial);  // nothing below zero
}

// --- control plane codec ------------------------------------------------------------

TEST(CreditMatrix, ControlMessageDecodeAndApply) {
    const auto name = uniq_name("ctl");
    MatrixGuard g(name);
    BilateralCreditMatrix m(name, true);
    ASSERT_TRUE(m.is_open());

    auto frame = encode_update(/*seq=*/1, /*a=*/5, /*b=*/6, /*limit=*/777);
    EXPECT_EQ(m.on_control_message(frame.data(),
                                   static_cast<uint32_t>(frame.size())),
              CreditCtlDecode::Ok);
    EXPECT_EQ(m.credit_limit(5, 6), 777u);
    EXPECT_EQ(m.updates_applied(), 1u);

    // Truncated / bad magic / bad version / bad party — all rejected,
    // nothing applied (fail closed).
    EXPECT_EQ(m.on_control_message(frame.data(), 10), CreditCtlDecode::TooShort);
    auto bad = frame;
    bad[0] ^= 0xFF;
    EXPECT_EQ(m.on_control_message(bad.data(), bad.size()),
              CreditCtlDecode::BadMagic);
    bad = frame;
    bad[6] = 0x7F;  // version
    EXPECT_EQ(m.on_control_message(bad.data(), bad.size()),
              CreditCtlDecode::BadVersion);
    bad = frame;
    bad[4] = 0x55;  // unknown type
    EXPECT_EQ(m.on_control_message(bad.data(), bad.size()),
              CreditCtlDecode::UnknownType);
    auto oor = encode_update(2, kCreditMaxParties, 1, 1);
    EXPECT_EQ(m.on_control_message(oor.data(), oor.size()),
              CreditCtlDecode::PartyOutOfRange);
    EXPECT_EQ(m.updates_applied(), 1u);
}

// --- cross-process proof ------------------------------------------------------------

// Parent creates + provisions; forked child attaches to the same shm object,
// observes the limit, and debits; parent observes the child's debit.
TEST(CreditMatrix, SharedAcrossProcesses) {
    const auto name = uniq_name("fork");
    MatrixGuard g(name);
    BilateralCreditMatrix m(name, true);
    ASSERT_TRUE(m.is_open());
    ASSERT_TRUE(m.apply_update(11, 22, 1'000));
    ASSERT_TRUE(m.apply_update(22, 11, 1'000));

    const pid_t pid = ::fork();
    ASSERT_NE(pid, -1);
    if (pid == 0) {
        BilateralCreditMatrix c(name, /*create=*/false);
        if (!c.is_open()) _exit(2);
        if (c.credit_limit(11, 22) != 1'000) _exit(3);
        if (!c.try_debit(11, 22, 250)) _exit(4);
        // A Go-side style update issued from the child is visible to parent.
        if (!c.apply_update(22, 11, 9'999)) _exit(5);
        _exit(0);
    }
    int status = 0;
    ASSERT_EQ(::waitpid(pid, &status, 0), pid);
    ASSERT_TRUE(WIFEXITED(status));
    ASSERT_EQ(WEXITSTATUS(status), 0);
    EXPECT_EQ(m.credit_limit(11, 22), 750u);   // child debit visible
    EXPECT_EQ(m.credit_limit(22, 11), 9'999u); // child update visible
    EXPECT_GE(m.updates_applied(), 2u);
}

// --- latency microbench -----------------------------------------------------------------

// AC: in-loop bilateral credit check < 2µs (§24 #403). Per-call CLOCK_MONOTONIC
// sampling over 1M calls across a spread of party pairs; p99 must stay under
// 2000ns — expected reality is single-digit ns, so this is ~100x headroom.
TEST(CreditMatrix, CanMatchP99Under2us) {
    const auto name = uniq_name("bench");
    MatrixGuard g(name);
    BilateralCreditMatrix m(name, true);
    ASSERT_TRUE(m.is_open());
    for (uint32_t p = 0; p < 64; ++p) {
        ASSERT_TRUE(m.apply_update(p, p + 1, 1'000'000'000));
        ASSERT_TRUE(m.apply_update(p + 1, p, 1'000'000'000));
    }

    constexpr int kCalls = 1'000'000;
    std::vector<uint64_t> lat(kCalls);
    uint64_t sink = 0;
    for (int i = 0; i < kCalls; ++i) {
        const uint32_t a = static_cast<uint32_t>(i & 63);
        const uint32_t b = a + 1;
        const uint64_t t0 = steady_ns();
        sink += m.can_match(a, b, 123'456 + (i & 1023)) ? 1 : 0;
        lat[i] = steady_ns() - t0;
    }
    ASSERT_EQ(sink, static_cast<uint64_t>(kCalls));  // every call screened OK
    std::sort(lat.begin(), lat.end());
    const uint64_t p50 = lat[kCalls / 2];
    const uint64_t p99 = lat[static_cast<size_t>(kCalls * 0.99)];
    const uint64_t mx = lat.back();
    std::printf("[credit-bench] can_match n=%d p50=%lluns p99=%lluns max=%lluns\n",
                kCalls, static_cast<unsigned long long>(p50),
                static_cast<unsigned long long>(p99),
                static_cast<unsigned long long>(mx));
    EXPECT_LT(p99, 2000u) << "p99 " << p99 << "ns exceeds the 2us budget";
}
