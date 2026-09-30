#pragma once

// Flat-array + intrusive-linked-list order book (Task 2.3.1, spec §3.1),
// Task 2.3.23 integer-tick arithmetic (spec §3.3a, §24 #402).
//
// Layout: bids_ sorted descending / asks_ ascending by price_ticks, kept
// compact (empty levels are removed by shift, so index 0 is always the best
// populated level). Level lookup is a binary search — O(log N) levels; order
// insertion at a found level is O(1) tail-append. Creating/removing a level
// memmoves the flat array — bounded by kMaxLevels, amortized rare, and the
// price of O(1) best_bid()/best_ask() which the matching loop reads far more
// often than it creates levels.
//
// Order identity: every resting order is registered in id_index_, a
// power-of-two bucket array using intrusive `hash_next` chains (Fibonacci
// hashing) — O(1) expected cancel/modify by id with zero per-op allocation.
//
// FIFO within a price level is keyed on (timestamp_ns, ingress_seq)
// explicitly — Task 2.3.20 fairness: level insertion splices a node ahead
// of any resting node with a strictly greater key rather than blindly
// tail-appending, so an amend re-stamp can never park an older-keyed node
// behind a younger one (deterministic under WAL replay — same stamps, same
// chain).
//
// Memory: orders come exclusively from the MemoryPool<Order> bound at
// construction — zero new/delete in add/cancel/modify/fill (spec §3.6.1).
// The one heap allocation in the whole object is the id-index bucket array,
// once, at construction (cold path; nothrow — failure leaves the book
// fail-closed: all mutations reject CAPACITY_EXCEEDED).
//
// Fail-closed boundary: the book is a passive container. A resting add whose
// limit price would meet or cross the opposite best is rejected CROSSED —
// the matching engine (Task 2.3.2) sweeps liquidity before resting a
// remainder, so a resting order that crosses is by definition an engine bug;
// rejecting it preserves the never-crossed invariant (spec §3.1, §24 #1).
//
// Consistency contract: the matching core is single-threaded (spec §3.1).
// getSnapshot/snapshot() is consistent because it runs on that thread;
// external readers must serialize (market-data snapshots go through the
// matching thread's event queue, Phase-06).
//
// Naming note: the task text lists camelCase signatures; the repo convention
// is snake_case — addOrder/cancelOrder/modifyOrder/getBestBid/getBestAsk/
// getLevel/getSnapshot map to add_order/cancel_order/modify_order/best_bid/
// best_ask/level/snapshot below.

#include <cstddef>
#include <cstdint>
#include <vector>

#include "book/Instrument.hpp"
#include "book/Order.hpp"
#include "book/PriceLevel.hpp"
#include "utils/MemoryPool.hpp"

namespace exch {

// Mutation result — returned, never thrown (hot path is noexcept).
// Caller/gateway mapping to spec §23 codes lands via the Phase-05 Task
// 5.3.21 registry; the canonical target is noted per enumerator.
enum class BookError : uint8_t {
    OK = 0,
    // Order pool exhausted / max_orders cap hit / book has no pool or index.
    // → ORDER_BOOK_CAPACITY_EXCEEDED (HTTP 503, L2; spec §3.6.1).
    CAPACITY_EXCEEDED,
    // Level array is full — same §3.6.1 capacity family.
    LEVEL_CAPACITY,
    // id already rests in the book → duplicate submission rejection.
    DUPLICATE_ID,
    // → ORDER_NOT_FOUND (HTTP 404).
    NOT_FOUND,
    // price_ticks <= 0 — a resting order always has a positive limit price.
    INVALID_PRICE,
    // qty_units <= 0, fill > remaining, amend qty <= already filled.
    INVALID_QTY,
    // Resting insert would meet/cross the opposite best — engine bug guard
    // (BOOK_CROSS_ERROR family, spec §24 #1 invariant).
    CROSSED,
    // IOC/FOK amend rejected outright (Task 2.3.20) → ORDER_AMEND_REJECTED.
    NOT_MODIFIABLE,
    // Checked arithmetic failed → ARITHMETIC_OVERFLOW_DETECTED (HTTP 400).
    OVERFLOW,
};

[[nodiscard]] const char* book_error_name(BookError e) noexcept;

// Market-data snapshot row — matches proto/exchange.fbs PriceLevel fields.
struct SnapshotLevel {
    int64_t price_ticks;
    int64_t total_qty_units;
    uint32_t order_count;
};

// Value-type book snapshot. Consistency contract: taken under
// single-threaded matching-core access (or an external serialization point);
// the vector allocations make it a cold-path call.
struct BookSnapshot {
    uint64_t seq;                       // book_seq_ at capture
    std::vector<SnapshotLevel> bids;    // descending price_ticks
    std::vector<SnapshotLevel> asks;    // ascending price_ticks
};

class OrderBook {
public:
    // Levels per side of the book (spec §3.1 MAX_LEVELS).
    static constexpr std::size_t kMaxLevels = 4096;
    // §3.6.1 pre-allocated bound: 2^20 resting orders per instrument.
    static constexpr std::size_t kMaxOrders = std::size_t{1} << 20;

    // Detached book — no order pool, no id index. Mutations fail-closed with
    // CAPACITY_EXCEEDED. Exists so scaffolding (main.cpp, early stubs) can
    // construct a book before the pool wiring lands (Task 2.3.2 owns that).
    OrderBook() noexcept;
    // Book bound to the shard order pool. Allocates the id-index bucket array
    // once (nothrow); on allocation failure the book stays fail-closed.
    explicit OrderBook(MemoryPool<Order>& orders,
                       std::size_t max_orders = kMaxOrders) noexcept;
    OrderBook(MemoryPool<Order>& orders, const Instrument& instrument,
              std::size_t max_orders = kMaxOrders) noexcept;
    ~OrderBook();

    OrderBook(const OrderBook&) = delete;
    OrderBook& operator=(const OrderBook&) = delete;
    OrderBook(OrderBook&&) = delete;
    OrderBook& operator=(OrderBook&&) = delete;

    // Instrument is reference data — bound by pointer (lives longer than the
    // book; reference-data reloads re-point it). Required for spread_pips().
    void set_instrument(const Instrument& instrument) noexcept {
        instrument_ = &instrument;
    }

    // --- Mutations (matching hot path: noexcept, zero heap allocation) -----

    // Insert a resting order. tmpl supplies all public fields except the
    // link pointers (reset here) — timestamp_ns/ingress_seq must already be
    // stamped by Aeron ingress (deterministic replay, Task 2.3.20). On
    // success *out receives the pooled, now-resting order; on any failure
    // *out is nullptr and the book is untouched (atomic L2 reject, §2.7).
    [[nodiscard]] BookError add_order(const Order& tmpl, Order** out) noexcept;

    // Remove a resting order by id and return its slot to the pool. When
    // snapshot_out is non-null the order is copied there first (the pointer
    // is dead after return — balance-release accounting needs the copy).
    [[nodiscard]] BookError cancel_order(uint64_t id,
                                         Order* snapshot_out = nullptr) noexcept;

    // Amend/replace (Task 2.3.20 + spec §6.6a/§6.9 contract):
    //   * price change OR qty increase  → remove + re-insert under the
    //     (timestamp_ns, ingress_seq) priority key with fresh stamps
    //     (priority lost — lands at the level tail in practice);
    //   * qty decrease at same price    → in-place, position + timestamp
    //     preserved;
    //   * force_requeue                 → unconditional remove + re-insert
    //     even when price/qty are unchanged or down — ICEBERG display_qty
    //     changes lose priority per spec §6.9 item 2;
    //   * IOC/FOK orders                → NOT_MODIFIABLE;
    //   * new_qty <= filled_qty         → INVALID_QTY (cancel instead);
    //   * new price meeting/crossing opposite best → CROSSED (order
    //     untouched — validation precedes any unlink, atomically).
    // Order id and filled_qty_units are preserved across the amend.
    // A no-op amend (same price + same qty + !force_requeue) is OK without
    // a seq bump.
    [[nodiscard]] BookError modify_order(uint64_t id, int64_t new_price_ticks,
                                         int64_t new_qty_units,
                                         uint64_t new_timestamp_ns,
                                         uint64_t new_ingress_seq,
                                         bool force_requeue = false) noexcept;

    // Apply a fill against a resting maker: increments filled_qty_units,
    // decrements the level aggregate; on full fill unlinks + frees the order
    // (dropping its level if it emptied). When snapshot_out is non-null the
    // order is copied there AFTER the fill is applied — post-fill state —
    // before any free, so the caller's fill report sees terminal quantities.
    // The maker pointer is dead whenever it fully fills.
    [[nodiscard]] BookError apply_fill(Order* maker, int64_t fill_units,
                                       Order* snapshot_out = nullptr) noexcept;

    // --- Queries -----------------------------------------------------------

    // Best bid = highest-priced populated bid level (index 0 — compact array).
    [[nodiscard]] const PriceLevel* best_bid() const noexcept;
    // Best ask = lowest-priced populated ask level.
    [[nodiscard]] const PriceLevel* best_ask() const noexcept;
    // Mutable head of a side's best level — the matching engine's maker walk
    // entry point (then follows Order::next within the level).
    [[nodiscard]] Order* front_order(Side side) noexcept;
    // K-th best level on a side, 0 == best; nullptr past the tail.
    [[nodiscard]] const PriceLevel* level(Side side, std::size_t depth) const noexcept;
    // O(1) expected id → resting order lookup (hash index; nullptr if absent).
    [[nodiscard]] Order* find_order(uint64_t id) noexcept;
    [[nodiscard]] const Order* find_order(uint64_t id) const noexcept;
    // Full-depth snapshot (cold path — allocates; see BookSnapshot contract).
    [[nodiscard]] BookSnapshot snapshot() const;

    // Integer spread in whole pips — spec Task 2.3.23 formula. Returns false
    // when no instrument is bound or either side is empty (undefined spread).
    [[nodiscard]] bool spread_pips(int64_t& out_pips) const noexcept;
    // Invariant probe: true iff both sides populated AND best_bid >= best_ask
    // — must never be true outside an armed CALL auction (spec §3.1 /
    // §24 #1; §23 BOOK_CROSS_ERROR family).
    [[nodiscard]] bool crossed() const noexcept;
    // Phase-15 Task 15.3.6 — auction-scoped crossing relaxation. While an
    // armed CALL accumulates orders the book may hold crossed levels
    // deliberately; set_allow_crossed(true) suspends the CROSSED insertion
    // guard and the validate() crossing audit for exactly that window.
    // Default false — continuous mode is never-crossed, always.
    void set_allow_crossed(bool allow) noexcept { allow_crossed_ = allow; }
    [[nodiscard]] bool allow_crossed() const noexcept { return allow_crossed_; }
    // Cold-path structural audit: sorted levels, consistent chains + totals,
    // index completeness, not-crossed. Used by tests and the Phase-02.5 soak
    // harness; *violation (optional) receives a static reason string.
    [[nodiscard]] bool validate(const char** violation = nullptr) const noexcept;

    [[nodiscard]] uint64_t book_seq() const noexcept { return book_seq_; }
    // Recovery-only rebase: snapshot restore installs the covered WAL cursor
    // so post-restart mutations never re-issue a prior generation's seqs
    // (TradeFill.seq consumers dedup on it). Also bumps once for off-book
    // mutations that still publish a fill (gslo_fill).
    void set_book_seq(uint64_t seq) noexcept { book_seq_ = seq; }
    void bump_book_seq() noexcept { ++book_seq_; }
    [[nodiscard]] uint32_t bid_count() const noexcept { return bid_count_; }
    [[nodiscard]] uint32_t ask_count() const noexcept { return ask_count_; }
    [[nodiscard]] std::size_t live_orders() const noexcept { return live_orders_; }
    [[nodiscard]] std::size_t max_orders() const noexcept { return max_orders_; }
    [[nodiscard]] const Instrument* instrument() const noexcept { return instrument_; }
    // Probes used by the most recent level binary search — O(log N) evidence
    // for tests (log2(kMaxLevels)+1 bound).
    [[nodiscard]] uint32_t last_lookup_probes() const noexcept {
        return last_lookup_probes_;
    }

private:
    [[nodiscard]] PriceLevel* side_levels(Side s) noexcept {
        return s == Side::BUY ? bids_ : asks_;
    }
    [[nodiscard]] const PriceLevel* side_levels(Side s) const noexcept {
        return s == Side::BUY ? bids_ : asks_;
    }
    [[nodiscard]] uint32_t& side_count(Side s) noexcept {
        return s == Side::BUY ? bid_count_ : ask_count_;
    }
    [[nodiscard]] uint32_t side_count(Side s) const noexcept {
        return s == Side::BUY ? bid_count_ : ask_count_;
    }

    // Binary search: index of the level at `price` (found=true) or the
    // insertion point preserving sort order (found=false).
    std::size_t find_level_index(Side s, int64_t price_ticks,
                                 bool& found) const noexcept;
    PriceLevel* insert_level_at(Side s, std::size_t idx,
                                int64_t price_ticks) noexcept;
    void remove_level_at(Side s, std::size_t idx) noexcept;
    // Locate the level an order rests at (by its recorded price — invariant).
    std::size_t level_index_of(const Order& o, bool& found) const noexcept;

    // id → Order* index: pow2 buckets + intrusive hash_next chains.
    std::size_t bucket_of(uint64_t id) const noexcept {
        return (id * kIdHashMul) >> index_shift_;
    }
    void index_insert(Order* o) noexcept;
    Order* index_find(uint64_t id) const noexcept;
    void index_remove(Order* o) noexcept;

    // Fibonacci hashing multiplier (2^64 / golden ratio) — high bits of the
    // product index into the pow2 bucket array via index_shift_.
    static constexpr uint64_t kIdHashMul = 0x9E3779B97F4A7C15ull;

    PriceLevel bids_[kMaxLevels];  // descending price_ticks
    PriceLevel asks_[kMaxLevels];  // ascending price_ticks
    uint32_t bid_count_ = 0;
    uint32_t ask_count_ = 0;
    uint64_t book_seq_ = 0;                    // bumped on every mutation

    MemoryPool<Order>* orders_ = nullptr;      // bound order pool (may be 0)
    const Instrument* instrument_ = nullptr;   // bound reference data
    Order** buckets_ = nullptr;                // id index (ctor-allocated)
    std::size_t index_cap_ = 0;                // bucket count (pow2)
    uint32_t index_shift_ = 64;                // 64 - log2(index_cap_)
    std::size_t max_orders_ = 0;               // resting-order cap (§3.6.1)
    std::size_t live_orders_ = 0;
    mutable uint32_t last_lookup_probes_ = 0;  // binary-search instrument'n
    bool allow_crossed_ = false;  // Task 15.3.6 — armed CALL accumulation only
};

}  // namespace exch
