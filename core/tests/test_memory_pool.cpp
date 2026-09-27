#include <gtest/gtest.h>

#include <atomic>
#include <cstdio>
#include <cstdlib>
#include <new>
#include <unordered_set>
#include <vector>

#include "book/Order.hpp"
#include "utils/MemoryPool.hpp"

using exch::MemoryPool;
using exch::Order;

// Allocator hook: counting is armed only inside the hot-path window so gtest
// framework allocations do not pollute the measurement.
namespace {
std::atomic<uint64_t> g_heap_calls{0};
std::atomic<bool> g_armed{false};

void* counted_malloc(std::size_t n) {
    if (g_armed.load(std::memory_order_relaxed))
        g_heap_calls.fetch_add(1, std::memory_order_relaxed);
    void* p = std::malloc(n == 0 ? 1 : n);
    if (p == nullptr) throw std::bad_alloc();
    return p;
}
}  // namespace

void* operator new(std::size_t n) { return counted_malloc(n); }
void* operator new[](std::size_t n) { return counted_malloc(n); }
void* operator new(std::size_t n, std::align_val_t a) {
    if (g_armed.load(std::memory_order_relaxed))
        g_heap_calls.fetch_add(1, std::memory_order_relaxed);
    void* p = nullptr;
    if (::posix_memalign(&p, static_cast<std::size_t>(a), n) != 0 || p == nullptr)
        throw std::bad_alloc();
    return p;
}

void operator delete(void* p) noexcept { std::free(p); }
void operator delete[](void* p) noexcept { std::free(p); }
void operator delete(void* p, std::size_t) noexcept { std::free(p); }
void operator delete[](void* p, std::size_t) noexcept { std::free(p); }
void operator delete(void* p, std::align_val_t) noexcept { std::free(p); }
void operator delete[](void* p, std::align_val_t) noexcept { std::free(p); }

// posix_memalign/free is a legal pair; GCC's alloc/dealloc pairing heuristic
// can't see through the out-param and flags the inlined deallocations below.
#pragma GCC diagnostic push
#pragma GCC diagnostic ignored "-Wmismatched-new-delete"
namespace {

long rss_kb() {
    long kb = -1;
    if (FILE* f = std::fopen("/proc/self/status", "r")) {
        char line[256];
        while (std::fgets(line, sizeof(line), f)) {
            if (std::sscanf(line, "VmRSS: %ld kB", &kb) == 1) break;
        }
        std::fclose(f);
    }
    return kb;
}

TEST(MemoryPool, AllocReturnsZeroedDistinctSlots) {
    MemoryPool<Order> pool(4);
    EXPECT_EQ(pool.capacity(), 4u);
    EXPECT_EQ(pool.size(), 0u);
    EXPECT_EQ(pool.remaining(), 4u);

    Order* a = pool.alloc();
    ASSERT_NE(a, nullptr);
    EXPECT_EQ(a->id, 0u);
    EXPECT_EQ(a->next, nullptr);
    EXPECT_EQ(a->prev, nullptr);
    EXPECT_TRUE(a->quantity.is_zero());
    EXPECT_TRUE(pool.owns(a));

    Order* b = pool.alloc();
    ASSERT_NE(b, nullptr);
    EXPECT_NE(a, b);
    EXPECT_EQ(pool.size(), 2u);
}

TEST(MemoryPool, ExhaustionReturnsNullptr) {
    MemoryPool<Order> pool(2);
    EXPECT_NE(pool.alloc(), nullptr);
    EXPECT_NE(pool.alloc(), nullptr);
    EXPECT_EQ(pool.alloc(), nullptr);  // exhausted -> nullptr (ORDER_BOOK_CAPACITY_EXCEEDED path)
    EXPECT_EQ(pool.remaining(), 0u);
}

TEST(MemoryPool, FreeIsLifo) {
    MemoryPool<Order> pool(2);
    Order* a = pool.alloc();
    Order* b = pool.alloc();
    pool.free(a);
    EXPECT_EQ(pool.alloc(), a);  // most recently freed slot comes back first
    pool.free(b);
    EXPECT_EQ(pool.alloc(), b);
    pool.free(nullptr);          // free(nullptr) is a no-op
    EXPECT_EQ(pool.size(), 2u);
}

TEST(MemoryPool, OneMillionOrdersZeroHeapHotPath) {
    MemoryPool<Order> pool(exch::kOrderPoolCapacity);  // 1M
    std::vector<Order*> ptrs;
    ptrs.reserve(exch::kOrderPoolCapacity);

    // Warm-up: fault in every pool page and the full vector capacity so the
    // measured window below exercises only the freelist hot path.
    for (uint64_t i = 0; i < exch::kOrderPoolCapacity; ++i) {
        Order* o = pool.alloc();
        ASSERT_NE(o, nullptr);
        o->id = i;
        ptrs.push_back(o);
    }
    for (Order* o : ptrs) pool.free(o);
    ptrs.clear();

    const long rss_before = rss_kb();
    g_heap_calls.store(0);
    g_armed.store(true);

    for (int round = 0; round < 3; ++round) {
        for (uint64_t i = 0; i < exch::kOrderPoolCapacity; ++i) {
            Order* o = pool.alloc();
            ASSERT_NE(o, nullptr);
            o->id = i;  // dirty every slot
            ptrs.push_back(o);
        }
        EXPECT_EQ(pool.remaining(), 0u);
        for (Order* o : ptrs) pool.free(o);
        ptrs.clear();
        EXPECT_EQ(pool.remaining(), exch::kOrderPoolCapacity);
    }

    g_armed.store(false);
    const long rss_after = rss_kb();

    EXPECT_EQ(g_heap_calls.load(), 0u);  // zero heap traffic while armed
    if (rss_before >= 0 && rss_after >= 0) {
        EXPECT_LE(rss_after - rss_before, 4096);  // kB — stable RSS across rounds
    }
}

TEST(MemoryPool, SlotsDoNotOverlap) {
    constexpr std::size_t n = 10'000;
    MemoryPool<Order> pool(n);
    std::unordered_set<Order*> seen;
    for (std::size_t i = 0; i < n; ++i) {
        Order* o = pool.alloc();
        ASSERT_NE(o, nullptr);
        EXPECT_TRUE(seen.insert(o).second);
    }
}

}  // namespace
#pragma GCC diagnostic pop
