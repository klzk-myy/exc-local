// Task 2.3.1 (spec §3.1) + Task 2.3.23 (spec §3.3a, §24 #402) — flat-array
// order book, int64 10^8-tick arithmetic only. No floating-point math, no
// heap allocation in any mutating path (the id-index bucket array is the
// sole allocation and happens once at construction).

#include "book/OrderBook.hpp"

#include <cstring>
#include <new>

#include "utils/safe_math.hpp"

namespace exch {

namespace {

// Smallest power of two >= n (index capacity; n >= 1 expected).
[[nodiscard]] std::size_t next_pow2(std::size_t n) noexcept {
    std::size_t p = 1;
    while (p < n) p <<= 1;
    return p;
}

[[nodiscard]] uint32_t log2_floor(std::size_t p) noexcept {
    uint32_t r = 0;
    while (p > 1) { p >>= 1; ++r; }
    return r;
}

// Task 2.3.20 (spec §6.9 fairness): FIFO within a level is ordered by the
// (timestamp_ns, ingress_seq) priority key, not blind arrival order — an
// amend re-stamp or a timestamped replayed entry could otherwise land behind
// a strictly younger-keyed node. Walk back from the tail past every node
// with a greater key and splice in place; equal keys stay behind existing
// equals (stable). The common case is O(1): non-decreasing keys trip the
// tail comparison once, matching the old push_back cost.
void level_insert_priority(PriceLevel& lvl, Order* o) noexcept {
    Order* pos = lvl.tail;
    while (pos != nullptr &&
           (pos->timestamp_ns > o->timestamp_ns ||
            (pos->timestamp_ns == o->timestamp_ns &&
             pos->ingress_seq > o->ingress_seq))) {
        pos = pos->prev;
    }
    o->prev = pos;
    o->next = pos != nullptr ? pos->next : lvl.head;
    if (pos != nullptr) pos->next = o; else lvl.head = o;
    if (o->next != nullptr) o->next->prev = o; else lvl.tail = o;
    ++lvl.order_count;
}

}  // namespace

const char* book_error_name(BookError e) noexcept {
    switch (e) {
        case BookError::OK:                return "OK";
        case BookError::CAPACITY_EXCEEDED: return "CAPACITY_EXCEEDED";
        case BookError::LEVEL_CAPACITY:    return "LEVEL_CAPACITY";
        case BookError::DUPLICATE_ID:      return "DUPLICATE_ID";
        case BookError::NOT_FOUND:         return "NOT_FOUND";
        case BookError::INVALID_PRICE:     return "INVALID_PRICE";
        case BookError::INVALID_QTY:       return "INVALID_QTY";
        case BookError::CROSSED:           return "CROSSED";
        case BookError::NOT_MODIFIABLE:    return "NOT_MODIFIABLE";
        case BookError::OVERFLOW:          return "OVERFLOW";
    }
    return "?";
}

OrderBook::OrderBook() noexcept {
    for (std::size_t i = 0; i < kMaxLevels; ++i) {
        bids_[i].reset();
        asks_[i].reset();
    }
}

OrderBook::OrderBook(MemoryPool<Order>& orders, std::size_t max_orders) noexcept
    : orders_(&orders) {
    for (std::size_t i = 0; i < kMaxLevels; ++i) {
        bids_[i].reset();
        asks_[i].reset();
    }
    max_orders_ = max_orders < kMaxOrders ? max_orders : kMaxOrders;

    // Index sized so chains stay ~1 deep: buckets = pow2 >= 2 * effective
    // capacity. Load factor <= 0.5 keeps index_find/index_remove ~O(1).
    const std::size_t effective =
        orders.capacity() < max_orders_ ? orders.capacity() : max_orders_;
    if (effective == 0) return;  // fail-closed: adds reject CAPACITY_EXCEEDED
    index_cap_ = next_pow2(2 * effective < 64 ? 64 : 2 * effective);
    buckets_ = new (std::nothrow) Order*[index_cap_]();
    if (buckets_ == nullptr) {
        index_cap_ = 0;  // fail-closed: adds reject CAPACITY_EXCEEDED
        return;
    }
    index_shift_ = static_cast<uint32_t>(64 - log2_floor(index_cap_));
}

OrderBook::OrderBook(MemoryPool<Order>& orders, const Instrument& instrument,
                     std::size_t max_orders) noexcept
    : OrderBook(orders, max_orders) {
    instrument_ = &instrument;
}

OrderBook::~OrderBook() { delete[] buckets_; }

// --- binary search -----------------------------------------------------------
// Compact sorted arrays: bids_ descending (index 0 = highest), asks_
// ascending (index 0 = lowest). Returns the position of the matching level
// or, when absent, the insertion point that preserves the ordering.

std::size_t OrderBook::find_level_index(Side s, int64_t price_ticks,
                                        bool& found) const noexcept {
    const PriceLevel* arr = side_levels(s);
    const std::size_t n = side_count(s);
    const bool desc = (s == Side::BUY);
    std::size_t lo = 0, hi = n;
    uint32_t probes = 0;
    while (lo < hi) {
        const std::size_t mid = (lo + hi) >> 1;
        ++probes;
        const int64_t p = arr[mid].price_ticks;
        if (p == price_ticks) {
            last_lookup_probes_ = probes;
            found = true;
            return mid;
        }
        // desc: shrink right while entries are above target;
        // asc:  shrink left while entries are below target.
        const bool go_right = desc ? (p > price_ticks) : (p < price_ticks);
        if (go_right) lo = mid + 1; else hi = mid;
    }
    last_lookup_probes_ = probes;
    found = false;
    return lo;
}

std::size_t OrderBook::level_index_of(const Order& o, bool& found) const
    noexcept {
    return find_level_index(o.side, o.price_ticks, found);
}

PriceLevel* OrderBook::insert_level_at(Side s, std::size_t idx,
                                       int64_t price_ticks) noexcept {
    PriceLevel* arr = side_levels(s);
    uint32_t& n = side_count(s);
    // Move only descriptors — resting Orders are stable pool slots and never
    // move; their next/prev links do not point at PriceLevel storage.
    std::memmove(arr + idx + 1, arr + idx,
                 (n - static_cast<uint32_t>(idx)) * sizeof(PriceLevel));
    arr[idx].reset();
    arr[idx].price_ticks = price_ticks;
    ++n;
    return &arr[idx];
}

void OrderBook::remove_level_at(Side s, std::size_t idx) noexcept {
    PriceLevel* arr = side_levels(s);
    uint32_t& n = side_count(s);
    std::memmove(arr + idx, arr + idx + 1,
                 (n - static_cast<uint32_t>(idx) - 1) * sizeof(PriceLevel));
    --n;
    arr[n].reset();
}

// --- id index -----------------------------------------------------------------

void OrderBook::index_insert(Order* o) noexcept {
    const std::size_t b = bucket_of(o->id);
    o->hash_next = buckets_[b];
    buckets_[b] = o;
}

Order* OrderBook::index_find(uint64_t id) const noexcept {
    if (buckets_ == nullptr) return nullptr;
    for (Order* o = buckets_[bucket_of(id)]; o != nullptr; o = o->hash_next) {
        if (o->id == id) return o;
    }
    return nullptr;
}

void OrderBook::index_remove(Order* o) noexcept {
    Order** link = &buckets_[bucket_of(o->id)];
    while (*link != o) link = &(*link)->hash_next;
    *link = o->hash_next;
    o->hash_next = nullptr;
}

// --- mutations ----------------------------------------------------------------

BookError OrderBook::add_order(const Order& tmpl, Order** out) noexcept {
    *out = nullptr;
    // Validation first — a rejected add must leave zero side-effects (spec
    // §2.7 atomic L2 reject), so every failure check precedes any mutation.
    if (orders_ == nullptr || buckets_ == nullptr ||
        live_orders_ >= max_orders_) {
        return BookError::CAPACITY_EXCEEDED;
    }
    if (tmpl.price_ticks <= 0) return BookError::INVALID_PRICE;
    if (tmpl.qty_units <= 0) return BookError::INVALID_QTY;
    if (tmpl.filled_qty_units < 0 || tmpl.filled_qty_units >= tmpl.qty_units) {
        return BookError::INVALID_QTY;  // resting orders are live, not dead
    }
    if (index_find(tmpl.id) != nullptr) return BookError::DUPLICATE_ID;

    // Never-crossed invariant: a resting limit that meets or crosses the
    // opposite best would have matched — the engine sweeps before resting
    // (Task 2.3.2), so this is an engine-bug guard, not flow control.
    // Phase-15 Task 15.3.6: during an armed CALL auction the engine holds
    // allow_crossed_ and accumulates crossing interest deliberately — the
    // guard re-engages the moment continuous trading resumes.
    if (!allow_crossed_) {
        if (tmpl.side == Side::BUY) {
            if (ask_count_ > 0 && tmpl.price_ticks >= asks_[0].price_ticks) {
                return BookError::CROSSED;
            }
        } else {
            if (bid_count_ > 0 && tmpl.price_ticks <= bids_[0].price_ticks) {
                return BookError::CROSSED;
            }
        }
    }

    bool found = false;
    const std::size_t idx = find_level_index(tmpl.side, tmpl.price_ticks, found);
    const int64_t rem = remaining_qty_units(tmpl);
    int64_t new_total = rem;
    if (found) {
        const PriceLevel& lvl = side_levels(tmpl.side)[idx];
        if (!safe_math::try_add(lvl.total_qty_units, rem, new_total)) {
            return BookError::OVERFLOW;
        }
    } else if (side_count(tmpl.side) == kMaxLevels) {
        return BookError::LEVEL_CAPACITY;
    }

    Order* o = orders_->alloc();  // the only fallible step; still pre-mutation
    if (o == nullptr) return BookError::CAPACITY_EXCEEDED;

    // Commit — every step below cannot fail.
    *o = tmpl;                    // POD copy of all caller-stamped fields
    o->next = o->prev = o->hash_next = nullptr;
    o->quantity = Decimal::from_mantissa(o->qty_units);  // compat mirror
    PriceLevel* lvl =
        found ? &side_levels(tmpl.side)[idx]
              : insert_level_at(tmpl.side, idx, tmpl.price_ticks);
    level_insert_priority(*lvl, o);
    lvl->total_qty_units = new_total;
    // visible_* mirrors total_qty_units for L2-visible members only — the
    // aggregate is a subset of new_total, so the checked add above already
    // bounds it (visible_qty_units <= total_qty_units, always).
    if (l2_visible(*o)) {
        lvl->visible_qty_units += rem;
        ++lvl->visible_count;
    }
    index_insert(o);
    ++live_orders_;
    ++book_seq_;
    *out = o;
    return BookError::OK;
}

BookError OrderBook::cancel_order(uint64_t id, Order* snapshot_out) noexcept {
    Order* o = index_find(id);
    if (o == nullptr) return BookError::NOT_FOUND;
    if (snapshot_out != nullptr) *snapshot_out = *o;

    bool found = false;
    const std::size_t idx = level_index_of(*o, found);
    // Level must exist — an indexed resting order implies its level. Treat a
    // miss as structural corruption: refuse to mutate further (fail-closed).
    if (!found) return BookError::NOT_FOUND;
    PriceLevel* lvl = &side_levels(o->side)[idx];

    int64_t new_total;
    if (!safe_math::try_sub(lvl->total_qty_units, remaining_qty_units(*o),
                            new_total)) {
        return BookError::OVERFLOW;
    }

    lvl->unlink(o);
    lvl->total_qty_units = new_total;
    if (l2_visible(*o)) {
        lvl->visible_qty_units -= remaining_qty_units(*o);
        --lvl->visible_count;
    }
    if (lvl->empty()) remove_level_at(o->side, idx);
    index_remove(o);
    orders_->free(o);
    --live_orders_;
    ++book_seq_;
    return BookError::OK;
}

BookError OrderBook::modify_order(uint64_t id, int64_t new_price_ticks,
                                  int64_t new_qty_units,
                                  uint64_t new_timestamp_ns,
                                  uint64_t new_ingress_seq,
                                  bool force_requeue) noexcept {
    Order* o = index_find(id);
    if (o == nullptr) return BookError::NOT_FOUND;
    // IOC/FOK never rest long enough to amend — rejected outright per the
    // Task 2.3.20 contract (gateway maps to ORDER_AMEND_REJECTED).
    if (o->tif == TimeInForce::IOC || o->tif == TimeInForce::FOK) {
        return BookError::NOT_MODIFIABLE;
    }
    if (new_price_ticks <= 0) return BookError::INVALID_PRICE;
    if (new_qty_units <= 0) return BookError::INVALID_QTY;
    // Amending to <= already-filled leaves nothing live — caller cancels.
    if (new_qty_units <= o->filled_qty_units) return BookError::INVALID_QTY;
    if (!force_requeue && new_price_ticks == o->price_ticks &&
        new_qty_units == o->qty_units) {
        return BookError::OK;  // no-op: no mutation → no seq bump
    }

    bool found = false;
    const std::size_t src_idx = level_index_of(*o, found);
    if (!found) return BookError::NOT_FOUND;  // index/level divergence
    PriceLevel* src = &side_levels(o->side)[src_idx];
    const int64_t new_rem = new_qty_units - o->filled_qty_units;

    if (!force_requeue && new_price_ticks == o->price_ticks &&
        new_qty_units < o->qty_units) {
        // Keep-priority path (spec §6.6a / Task 2.3.20): qty-down-only stays
        // in place with its original timestamp and level position. A forced
        // requeue (ICEBERG display_qty change, spec §6.9) skips this branch
        // and loses priority even though the node quantity shrank.
        const int64_t delta = o->qty_units - new_qty_units;
        int64_t new_total;
        if (!safe_math::try_sub(src->total_qty_units, delta, new_total)) {
            return BookError::OVERFLOW;
        }
        o->qty_units = new_qty_units;
        o->quantity = Decimal::from_mantissa(new_qty_units);
        src->total_qty_units = new_total;
        if (l2_visible(*o)) src->visible_qty_units -= delta;
        ++book_seq_;
        return BookError::OK;
    }

    // Lose-priority path: price change or qty-up re-inserts at the FIFO tail
    // with a fresh timestamp (Task 2.3.20 amend-priority contract).
    // (a) Crossing guard — before any unlink, so a reject is atomic.
    // (allow_crossed_: CALL-auction accumulation may hold crossed levels.)
    if (!allow_crossed_) {
        if (o->side == Side::BUY) {
            if (ask_count_ > 0 && new_price_ticks >= asks_[0].price_ticks) {
                return BookError::CROSSED;
            }
        } else {
            if (bid_count_ > 0 && new_price_ticks <= bids_[0].price_ticks) {
                return BookError::CROSSED;
            }
        }
    }
    // (b) Level-capacity guard: if the move needs a fresh level while the
    // side is full, reject — unless unlinking would empty the source level
    // and free a slot (only-order-at-level move). Computed pre-mutation.
    bool dst_found = false;
    (void)find_level_index(o->side, new_price_ticks, dst_found);
    if (!dst_found) {
        const uint32_t eff_count =
            side_count(o->side) - (src->order_count == 1 ? 1u : 0u);
        if (eff_count == kMaxLevels) return BookError::LEVEL_CAPACITY;
    }
    // (c) Aggregate-overflow guards, still pre-mutation.
    int64_t src_total_after;
    if (!safe_math::try_sub(src->total_qty_units, remaining_qty_units(*o),
                            src_total_after)) {
        return BookError::OVERFLOW;
    }

    // Commit — unfailable below.
    // l2_visible() is constant over a resting order's lifetime (type/flags
    // never change in place), so one evaluation covers the src unlink and
    // the dst re-insert below.
    const bool vis = l2_visible(*o);
    src->unlink(o);
    src->total_qty_units = src_total_after;
    if (vis) {
        src->visible_qty_units -= remaining_qty_units(*o);
        --src->visible_count;
    }
    if (src->empty()) remove_level_at(o->side, src_idx);

    o->price_ticks = new_price_ticks;
    o->qty_units = new_qty_units;
    o->quantity = Decimal::from_mantissa(new_qty_units);
    o->timestamp_ns = new_timestamp_ns;
    o->ingress_seq = new_ingress_seq;

    // Re-search post-removal: indices may have shifted under the unlink.
    const std::size_t dst_idx =
        find_level_index(o->side, new_price_ticks, dst_found);
    PriceLevel* dst = dst_found
        ? &side_levels(o->side)[dst_idx]
        : insert_level_at(o->side, dst_idx, new_price_ticks);
    int64_t dst_total;
    if (dst_found) {
        // Overflow already excluded pre-mutation? The level total cannot have
        // grown since the check — but compute checked anyway for parity.
        if (!safe_math::try_add(dst->total_qty_units, new_rem, dst_total)) {
            dst_total = INT64_MAX;  // unreachable per precheck; fail-closed cap
        }
    } else {
        dst_total = new_rem;
    }
    level_insert_priority(*dst, o);
    dst->total_qty_units = dst_total;
    if (vis) {
        dst->visible_qty_units += new_rem;
        ++dst->visible_count;
    }
    ++book_seq_;
    return BookError::OK;
}

BookError OrderBook::apply_fill(Order* maker, int64_t fill_units,
                                Order* snapshot_out) noexcept {
    if (fill_units <= 0) return BookError::INVALID_QTY;
    const int64_t rem = remaining_qty_units(*maker);
    if (fill_units > rem) return BookError::INVALID_QTY;

    bool found = false;
    const std::size_t idx = level_index_of(*maker, found);
    if (!found) return BookError::NOT_FOUND;
    PriceLevel* lvl = &side_levels(maker->side)[idx];

    int64_t new_total;
    if (!safe_math::try_sub(lvl->total_qty_units, fill_units, new_total)) {
        return BookError::OVERFLOW;
    }
    int64_t new_filled;
    if (!safe_math::try_add(maker->filled_qty_units, fill_units, new_filled)) {
        return BookError::OVERFLOW;  // fill_units <= rem makes this impossible
    }

    lvl->total_qty_units = new_total;
    maker->filled_qty_units = new_filled;
    const bool vis = l2_visible(*maker);
    if (vis) lvl->visible_qty_units -= fill_units;
    // Post-fill copy-out: the caller's fill report wants the terminal
    // filled_qty — the maker pointer dies below when it fully fills.
    if (snapshot_out != nullptr) *snapshot_out = *maker;
    if (maker->filled_qty_units >= maker->qty_units) {
        lvl->unlink(maker);
        if (vis) --lvl->visible_count;
        if (lvl->empty()) remove_level_at(maker->side, idx);
        index_remove(maker);
        orders_->free(maker);   // maker pointer is dead past this point
        --live_orders_;
    }
    ++book_seq_;
    return BookError::OK;
}

// --- queries ------------------------------------------------------------------

const PriceLevel* OrderBook::best_bid() const noexcept {
    return bid_count_ > 0 ? &bids_[0] : nullptr;
}

const PriceLevel* OrderBook::best_ask() const noexcept {
    return ask_count_ > 0 ? &asks_[0] : nullptr;
}

Order* OrderBook::front_order(Side side) noexcept {
    const PriceLevel* lvl = side == Side::BUY ? best_bid() : best_ask();
    return lvl != nullptr ? lvl->head : nullptr;
}

const PriceLevel* OrderBook::level(Side side, std::size_t depth) const noexcept {
    const PriceLevel* arr = side_levels(side);
    return depth < side_count(side) ? &arr[depth] : nullptr;
}

Order* OrderBook::find_order(uint64_t id) noexcept { return index_find(id); }
const Order* OrderBook::find_order(uint64_t id) const noexcept {
    return index_find(id);
}

BookSnapshot OrderBook::snapshot() const {
    BookSnapshot s;
    s.seq = book_seq_;
    s.bids.reserve(bid_count_);
    s.asks.reserve(ask_count_);
    for (uint32_t i = 0; i < bid_count_; ++i) {
        s.bids.push_back({bids_[i].price_ticks, bids_[i].total_qty_units,
                          bids_[i].order_count});
    }
    for (uint32_t i = 0; i < ask_count_; ++i) {
        s.asks.push_back({asks_[i].price_ticks, asks_[i].total_qty_units,
                          asks_[i].order_count});
    }
    return s;
}

bool OrderBook::spread_pips(int64_t& out_pips) const noexcept {
    if (instrument_ == nullptr || bid_count_ == 0 || ask_count_ == 0) {
        return false;
    }
    // Spec formula (Task 2.3.23): (ask - bid) / (pip_factor * 1000).
    out_pips = exch::spread_pips(asks_[0].price_ticks, bids_[0].price_ticks,
                                 *instrument_);
    return true;
}

bool OrderBook::crossed() const noexcept {
    return bid_count_ > 0 && ask_count_ > 0 &&
           bids_[0].price_ticks >= asks_[0].price_ticks;
}

bool OrderBook::validate(const char** violation) const noexcept {
    const char* why = nullptr;
    std::size_t counted = 0;
    for (int s = 0; s < 2 && why == nullptr; ++s) {
        const Side side = s == 0 ? Side::BUY : Side::SELL;
        const PriceLevel* arr = side_levels(side);
        const uint32_t n = side_count(side);
        for (uint32_t i = 0; i < n; ++i) {
            const PriceLevel& lvl = arr[i];
            if (lvl.order_count == 0 || lvl.head == nullptr) {
                why = "empty level occupies array slot"; break;
            }
            if (i + 1 < n) {
                const bool bad = side == Side::BUY
                    ? !(lvl.price_ticks > arr[i + 1].price_ticks)
                    : !(lvl.price_ticks < arr[i + 1].price_ticks);
                if (bad) { why = "levels not strictly sorted"; break; }
            }
            if (!level_chain_consistent(lvl, side, &why)) break;
            // Aggregate check: level total == Σ remaining over the chain.
            int64_t sum = 0;
            for (const Order* o = lvl.head; o != nullptr; o = o->next) {
                sum += remaining_qty_units(*o);
                ++counted;
            }
            if (sum != lvl.total_qty_units) {
                why = "level total_qty_units != Σ remaining"; break;
            }
        }
    }
    if (why == nullptr) {
        if (counted != live_orders_) {
            why = "live_orders_ != orders in levels";
        } else if (!allow_crossed_ && crossed()) {
            why = "book crossed (best_bid >= best_ask)";
        } else {
            // Index completeness: every level order resolvable by id.
            for (int s = 0; s < 2 && why == nullptr; ++s) {
                const Side side = s == 0 ? Side::BUY : Side::SELL;
                const PriceLevel* arr = side_levels(side);
                for (uint32_t i = 0; i < side_count(side); ++i) {
                    for (const Order* o = arr[i].head; o != nullptr;
                         o = o->next) {
                        if (index_find(o->id) != o) {
                            why = "order unreachable via id index"; break;
                        }
                    }
                    if (why != nullptr) break;
                }
            }
        }
    }
    if (violation != nullptr) *violation = why;
    return why == nullptr;
}

}  // namespace exch
