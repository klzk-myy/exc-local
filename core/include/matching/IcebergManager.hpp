#pragma once

// Task 2.3.2 — ICEBERG order bookkeeping (spec §3.2 #6, §3.7.4).
//
// An iceberg rests as a sequence of *visible slices*: the book node carries
// only the display quantity; the hidden remainder lives here. When a slice
// fully fills the manager hands the engine the next slice template — the
// refreshed node re-enters the book at the FIFO tail of its price level
// (standard venue semantics: refresh loses time priority).
//
// Bookkeeping invariant per record:
//   total_filled + live_node_remaining + hidden == total_qty_units
// where hidden = total_qty_units - filled_total_units - live_node_remaining.
//
// Storage: one cold-allocated open-addressed table (linear probe, load <=
// 0.5) keyed by order_id — zero heap traffic on the hot path; full table is
// fail-closed (engine rejects the order BOOK_CAPACITY rather than losing
// the hidden remainder).
//
// The record's `tmpl` is the source for re-slice nodes (side, price, flags,
// tif, stp_mode); account identity comes with it. Group/expiry metadata
// lives in the engine's order-meta index keyed by the same order_id — the
// record does not duplicate it.

#include <cstddef>
#include <cstdint>
#include <new>

#include "book/Instrument.hpp"
#include "book/Order.hpp"
#include "utils/safe_math.hpp"

namespace exch {

class IcebergManager {
public:
    // capacity bounds simultaneously-live icebergs; must stay <= order pool.
    explicit IcebergManager(std::size_t capacity = kDefaultCapacity) noexcept {
        if (capacity == 0) capacity = 1;
        capacity_ = next_pow2(capacity * 2);  // load factor <= 0.5
        slots_ = new (std::nothrow) Record[capacity_]();
        if (slots_ == nullptr) capacity_ = 0;  // fail-closed
    }
    ~IcebergManager() { delete[] slots_; }

    IcebergManager(const IcebergManager&) = delete;
    IcebergManager& operator=(const IcebergManager&) = delete;

    struct Record {
        uint64_t order_id = 0;          // key; 0 = empty slot
        Order    tmpl{};                // re-slice template (POD copy)
        int64_t  total_qty_units = 0;   // original order quantity
        int64_t  filled_total_units = 0;// cumulative fills incl. live slice
        int64_t  display_qty_units = 0; // configured slice size (>0)
    };

    static constexpr std::size_t kDefaultCapacity = 1u << 15;  // 32k icebergs
    // Default visible slice when neither the order nor the instrument
    // specifies one: 10% of the order's total quantity (spec §3.2 #6).
    static constexpr int64_t kDefaultDisplayPct100 = 1000;     // 10.00%

    // Register a resting iceberg. tmpl = the full order (its qty_units is the
    // TOTAL); filled_so_far = taker fills already applied before the rest.
    // Returns the record or nullptr when the table is full (fail-closed).
    [[nodiscard]] Record* register_order(const Order& tmpl,
                                         int64_t filled_so_far,
                                         int64_t display_qty_units) noexcept {
        if (slots_ == nullptr || live_ * 2 >= capacity_) return nullptr;
        std::size_t i = bucket(tmpl.id);
        while (slots_[i].order_id != 0) {
            i = (i + 1) & (capacity_ - 1);
        }
        Record& r = slots_[i];
        r.order_id = tmpl.id;
        r.tmpl = tmpl;
        r.total_qty_units = tmpl.qty_units;
        r.filled_total_units = filled_so_far;
        r.display_qty_units = display_qty_units;
        ++live_;
        return &r;
    }

    [[nodiscard]] Record* find(uint64_t order_id) noexcept {
        if (slots_ == nullptr || live_ == 0) return nullptr;
        std::size_t i = bucket(order_id);
        for (;;) {
            const uint64_t k = slots_[i].order_id;
            if (k == 0) return nullptr;
            if (k == order_id) return &slots_[i];
            i = (i + 1) & (capacity_ - 1);
        }
    }
    [[nodiscard]] const Record* find(uint64_t order_id) const noexcept {
        return const_cast<IcebergManager*>(this)->find(order_id);
    }

    void erase(uint64_t order_id) noexcept {
        // Open-addressed delete via probe-chain compaction (no tombstones):
        // walk forward from the hole; an entry at slot j can move into hole
        // i iff i lies on j's own probe path, i.e. home(key_j) <= i < j in
        // cyclic order — equivalently (i - home) mod N < (j - home) mod N.
        if (slots_ == nullptr) return;
        std::size_t i = bucket(order_id);
        for (;;) {
            if (slots_[i].order_id == 0) return;         // absent
            if (slots_[i].order_id == order_id) break;   // found
            i = (i + 1) & (capacity_ - 1);
        }
        std::size_t j = i;
        for (;;) {
            j = (j + 1) & (capacity_ - 1);
            if (slots_[j].order_id == 0) break;          // end of cluster
            const std::size_t home = bucket(slots_[j].order_id);
            if (((i - home) & (capacity_ - 1)) <
                ((j - home) & (capacity_ - 1))) {
                slots_[i] = slots_[j];                   // fill the hole
                i = j;                                   // hole moves to j
            }
        }
        slots_[i] = Record{};
        --live_;
    }

    // Cold-path enumeration (snapshot export): invoke f(record) for every
    // live entry in slot order. Read-only — records may not be modified.
    template <typename F>
    void for_each(F&& f) const noexcept {
        for (std::size_t i = 0; i < capacity_; ++i) {
            if (slots_[i].order_id != 0) f(slots_[i]);
        }
    }

    // Hidden quantity not yet committed to any slice (0 at live node — the
    // node's remaining slice is excluded from hidden by definition).
    [[nodiscard]] static int64_t
    hidden_qty(const Record& r, int64_t live_node_remaining) noexcept {
        const int64_t h = r.total_qty_units - r.filled_total_units -
                          live_node_remaining;
        return h > 0 ? h : 0;
    }

    // Next slice size after the current slice dies: min(display, hidden).
    // <= 0 means the iceberg is done.
    [[nodiscard]] static int64_t next_slice(const Record& r) noexcept {
        const int64_t rem = r.total_qty_units - r.filled_total_units;
        if (rem <= 0) return 0;
        return rem < r.display_qty_units ? rem : r.display_qty_units;
    }

    // Slice sizing at rest time: explicit order display_qty wins, else the
    // instrument's display_ratio (percent*100), else the 10% default.
    // Clamped into [1, qty]; the result is the size of the resting slice.
    [[nodiscard]] static int64_t visible_slice(
        int64_t qty_units, int64_t display_qty_units,
        const Instrument* instrument) noexcept {
        int64_t slice = display_qty_units;
        if (slice <= 0 && instrument != nullptr &&
            instrument->display_ratio > 0) {
            int64_t v = 0;
            if (iceberg_visible_units(*instrument, qty_units, v)) slice = v;
        }
        if (slice <= 0) {
            // default 10% = kDefaultDisplayPct100 / 10000 (128-bit product)
            slice = static_cast<int64_t>(
                safe_math::mul_wide_i64(qty_units, kDefaultDisplayPct100) /
                10'000);
        }
        if (slice <= 0) slice = 1;
        return slice > qty_units ? qty_units : slice;
    }

    [[nodiscard]] std::size_t size() const noexcept { return live_; }
    [[nodiscard]] bool contains(uint64_t order_id) const noexcept {
        return find(order_id) != nullptr;
    }

private:
    [[nodiscard]] std::size_t bucket(uint64_t id) const noexcept {
        // Fibonacci hashing — same scheme as OrderBook's id index.
        return static_cast<std::size_t>(id * 0x9E3779B97F4A7C15ull) &
               (capacity_ - 1);
    }
    static std::size_t next_pow2(std::size_t n) noexcept {
        std::size_t p = 1;
        while (p < n) p <<= 1;
        return p;
    }

    Record* slots_ = nullptr;
    std::size_t capacity_ = 0;
    std::size_t live_ = 0;
};

}  // namespace exch
