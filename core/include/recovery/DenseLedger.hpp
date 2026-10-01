#pragma once

// Recovery replay ledgers over u64 ids (Task 4.3.x, Phase-02.5 finding).
//
// The journal's order/trade ids are BIGSERIAL-dense — monotone sequence
// allocations with small gaps. A std::unordered_map pays a malloc'd node,
// a rehash cascade, and a cache miss per entry; at 100M-entry WAL scale
// that dominated restart-to-ready (observed: recovery > 19s AC).
//
// These structures exploit density instead: a two-level index where the
// high bits select a lazily-allocated leaf of 2^kChunkShift consecutive
// slots. Dense ids pack into contiguous leaves (~4B/entry); sparse or
// hostile ids degrade to per-leaf allocation only for ranges actually
// touched — never a single giant buffer.
//
// Replay-scope only: contents die with RecoveryManager::recover().

#include <algorithm>
#include <cstdint>
#include <memory>
#include <unordered_map>

namespace exch::recovery {

inline constexpr uint64_t kLedgerChunkShift = 20;  // 1M ids/leaf
inline constexpr uint64_t kLedgerChunkSize = 1u << kLedgerChunkShift;
inline constexpr uint64_t kLedgerChunkMask = kLedgerChunkSize - 1;

// u64 id -> T. empty_value() marks an unwritten slot; get() never
// allocates, ref()/set() allocate the leaf on first write.
template <typename T>
class DenseLedger {
   public:
    explicit DenseLedger(T empty = T{}) noexcept : empty_(empty) {}

    [[nodiscard]] T get(uint64_t id) const noexcept {
        const auto it = chunks_.find(id >> kLedgerChunkShift);
        if (it == chunks_.end()) return empty_;
        return it->second[id & kLedgerChunkMask];
    }
    [[nodiscard]] bool contains(uint64_t id) const noexcept { return get(id) != empty_; }
    void set(uint64_t id, T v) { ref(id) = v; }
    // Writable slot; allocates the leaf on first touch.
    T& ref(uint64_t id) {
        const uint64_t key = id >> kLedgerChunkShift;
        auto it = chunks_.find(key);
        if (it == chunks_.end()) {
            it = chunks_.emplace(key, std::make_unique<T[]>(static_cast<size_t>(kLedgerChunkSize)))
                     .first;
            std::fill_n(it->second.get(), kLedgerChunkSize, empty_);
        }
        return it->second[id & kLedgerChunkMask];
    }
    [[nodiscard]] uint64_t leaves() const noexcept { return chunks_.size(); }

   private:
    std::unordered_map<uint64_t, std::unique_ptr<T[]>> chunks_;
    T empty_;
};

// u64 membership set, 1 bit per id (128KB leaf per 1M ids).
class DenseSet {
   public:
    [[nodiscard]] bool contains(uint64_t id) const noexcept {
        const auto it = chunks_.find(id >> kLedgerChunkShift);
        if (it == chunks_.end()) return false;
        return (it->second[(id & kLedgerChunkMask) >> 6] & (uint64_t{1} << (id & 63))) != 0;
    }
    // Returns true if the bit was already set (set-membership probe).
    bool insert(uint64_t id) {
        const uint64_t key = id >> kLedgerChunkShift;
        auto it = chunks_.find(key);
        if (it == chunks_.end()) {
            it = chunks_.emplace(key, std::make_unique<uint64_t[]>(kLedgerChunkSize >> 6)).first;
        }
        uint64_t& word = it->second[(id & kLedgerChunkMask) >> 6];
        const uint64_t bit = uint64_t{1} << (id & 63);
        const bool was = (word & bit) != 0;
        word |= bit;
        return was;
    }

   private:
    std::unordered_map<uint64_t, std::unique_ptr<uint64_t[]>> chunks_;
};

}  // namespace exch::recovery
