#pragma once

// Pre-allocated freelist pool (Task 1.3.1, spec §3.1/§3.6). One backing block
// allocated at construction; alloc()/free() only move freelist pointers —
// zero heap traffic in the hot path. Free slots store an intrusive FreeNode
// overlay in the object storage. Exhausted alloc() returns nullptr; the caller
// maps that to ORDER_BOOK_CAPACITY_EXCEEDED (spec §3.6.1).
//
// Task 1.3.12: the pool tracks a high-watermark (peak live slots) for
// capacity telemetry, and alloc_or_throw() provides the cold-path error
// sentinel — it throws OrderBookCapacityExceeded
// (ORDER_BOOK_CAPACITY_EXCEEDED, HTTP 503, L2), an exception that performs
// no heap allocation itself (string-literal members only, spec §3.6.1).

#include <atomic>
#include <cassert>
#include <cstddef>
#include <new>
#include <type_traits>

#include "utils/error_severity.hpp"

namespace exch {

template <typename T>
class MemoryPool {
public:
    static_assert(std::is_default_constructible_v<T>);
    static_assert(std::is_trivially_destructible_v<T>);
    static_assert(sizeof(T) >= sizeof(void*));
    static_assert(alignof(T) >= alignof(void*));

    explicit MemoryPool(std::size_t capacity)
        : capacity_(capacity), remaining_(capacity) {
        storage_ = static_cast<T*>(
            ::operator new[](capacity * sizeof(T), std::align_val_t{alignof(T)}));
        for (std::size_t i = 0; i < capacity_; ++i) {
            FreeNode* n = node_at(i);
            n->next = (i + 1 < capacity_) ? node_at(i + 1) : nullptr;
        }
        free_head_ = capacity_ > 0 ? node_at(0) : nullptr;
    }

    ~MemoryPool() {
        ::operator delete[](storage_, std::align_val_t{alignof(T)});
    }

    MemoryPool(const MemoryPool&) = delete;
    MemoryPool& operator=(const MemoryPool&) = delete;
    MemoryPool(MemoryPool&&) = delete;
    MemoryPool& operator=(MemoryPool&&) = delete;

    // Returns a zero-initialized slot, or nullptr when the pool is exhausted.
    [[nodiscard]] T* alloc() noexcept {
        FreeNode* n = free_head_;
        if (n == nullptr) return nullptr;
        free_head_ = n->next;
        --remaining_;
        const std::size_t used = capacity_ - remaining_;
        const std::size_t hw = high_watermark_.load(std::memory_order_relaxed);
        if (used > hw) {
            high_watermark_.store(used, std::memory_order_relaxed);
        }
        return new (n) T();
    }

    // Cold-path convenience: identical to alloc() but throws
    // OrderBookCapacityExceeded (ORDER_BOOK_CAPACITY_EXCEEDED, HTTP 503, L2)
    // on exhaustion instead of returning nullptr. The exception carries only
    // string-literal members — the throw path performs no heap allocation
    // (spec §3.6.1). Hot paths should keep using alloc() and map nullptr to
    // the error code without throwing.
    [[nodiscard]] T* alloc_or_throw() {
        T* p = alloc();
        if (p == nullptr) throw OrderBookCapacityExceeded{};
        return p;
    }

    void free(T* p) noexcept {
        if (p == nullptr) return;
        assert(owns(p) && "MemoryPool::free of foreign pointer");
        FreeNode* n = new (p) FreeNode;
        n->next = free_head_;
        free_head_ = n;
        ++remaining_;
    }

    [[nodiscard]] std::size_t capacity() const noexcept { return capacity_; }
    [[nodiscard]] std::size_t size() const noexcept { return capacity_ - remaining_; }
    [[nodiscard]] std::size_t remaining() const noexcept { return remaining_; }
    // Peak live-slot count ever reached (Task 1.3.12). Relaxed atomic so the
    // health/telemetry thread may read it while the owner thread allocates.
    [[nodiscard]] std::size_t high_watermark() const noexcept {
        return high_watermark_.load(std::memory_order_relaxed);
    }
    // Re-bases the watermark at the current live count (admin/diagnostics).
    void reset_high_watermark() noexcept {
        high_watermark_.store(capacity_ - remaining_, std::memory_order_relaxed);
    }
    [[nodiscard]] bool owns(const T* p) const noexcept {
        const auto byte_off =
            reinterpret_cast<const char*>(p) - reinterpret_cast<const char*>(storage_);
        return p >= storage_ && byte_off >= 0 &&
               byte_off < static_cast<std::ptrdiff_t>(capacity_ * sizeof(T)) &&
               byte_off % static_cast<std::ptrdiff_t>(sizeof(T)) == 0;
    }

private:
    struct FreeNode {
        FreeNode* next;
    };

    [[nodiscard]] FreeNode* node_at(std::size_t i) noexcept {
        return reinterpret_cast<FreeNode*>(storage_ + i);
    }

    T* storage_ = nullptr;
    std::size_t capacity_ = 0;
    std::size_t remaining_ = 0;
    FreeNode* free_head_ = nullptr;
    std::atomic<std::size_t> high_watermark_{0};
};

}  // namespace exch
