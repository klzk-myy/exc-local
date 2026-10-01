// Task 2.3.2 — MatchingEngine: price-time priority matching orchestrator
// (spec §3.2, §3.7, §6.5). See matching/MatchingEngine.hpp for the design
// contract: validate -> risk hook -> WAL -> mutate -> publish, zero-alloc
// hot path, deterministic (no wall clock — logical time only via
// on_time_tick), fail-closed on WAL/table/book-structural faults.
//
// WAL logging discipline: externally-caused events are journaled —
// ORDER_NEW/ORDER_NEW_EX (acceptance), ORDER_CANCEL (user/expiry/STP/
// FOK/IOC-remainder), ORDER_MODIFY (amends + STP decrements), TRADE (per
// fill), TIME_TICK, ORDER_TRIGGERED (conditional activation — committed
// so MARK/INDEX-sourced triggers replay with the exact observed
// reference and deterministic sequence, the GSLO branch never re-arms),
// PEG_REPRICE (committed reprice — anchor for replay divergence checks).
// Internally-DERIVED transitions — iceberg slice refresh, trailing-stop
// re-anchor — are NOT journaled: they are deterministic functions of the
// logged stream and re-derive identically on replay (single source of
// truth; a derived entry would double-apply under verbatim replay).

#include "matching/MatchingEngine.hpp"

#include <algorithm>
#include <new>

#include "book/Instrument.hpp"
#include "ipc/L3Publisher.hpp"
#include "matching/IpcPublisher.hpp"
#include "risk/InstrumentFeed.hpp"
#include "risk/PriceOracleFeed.hpp"
#include "utils/safe_math.hpp"

namespace exch {

namespace {

// Phase-16 Tasks 16.3.3/16.3.15 — trailing distance in ticks:
//   PIPS/PIPETTE-ish units convert through the instrument pip lattice
//   (distance is in pips, i.e. integer pip count);
//   PERCENTAGE is the fractional-percentage integer convention — distance
//   25 means 0.25% of the reference (25 / 10'000 of the price), computed
//   via the 128-bit safe path and clamped to i64 on overflow;
//   ABSOLUTE is the raw tick distance verbatim.
[[nodiscard]] int64_t trail_distance_ticks(int64_t distance,
                                           uint8_t unit,
                                           int64_t ref_ticks,
                                           const Instrument& inst) noexcept {
    if (distance <= 0) return 0;
    switch (unit) {
    case kTrailUnitPips:
        return pips_to_ticks(static_cast<uint64_t>(distance), inst);
    case kTrailUnitPercentage: {
        const safe_math::int128_t w =
            safe_math::mul_wide_i64(ref_ticks, distance) / 10'000;
        int64_t t = 0;
        if (!safe_math::try_narrow_i128(w, t)) return 0;
        return t < 0 ? 0 : t;
    }
    case kTrailUnitAbsolute:
    default:
        return distance;
    }
}

// Signed arithmetic: clamp the unsigned i128 square root to INT64_MAX
// before ± application so a degenerate distance cannot wrap.
[[nodiscard]] int64_t trail_stop_from_anchor(bool buy,
                                             int64_t anchor_ticks,
                                             int64_t distance,
                                             uint8_t unit,
                                             const Instrument& inst) noexcept {
    const int64_t dist =
        trail_distance_ticks(distance, unit, anchor_ticks, inst);
    if (buy) {
        const safe_math::int128_t w = anchor_ticks;
        int64_t t = 0;
        (void)safe_math::try_narrow_i128(w + dist, t);
        return t;
    }
    return anchor_ticks - dist;  // degenerate dist > anchor clamps <= 0
}

// MOO/MOC participate in the indicative cross as MARKET liquidity; a
// pending-at-auction close trailed-in stop keeps its trigger semantics.
[[nodiscard]] bool auction_market_like(OrderType t) noexcept {
    return t == OrderType::MARKET || t == OrderType::MOO ||
           t == OrderType::MOC;
}

}  // namespace

namespace {

[[nodiscard]] constexpr std::size_t next_pow2(std::size_t n) noexcept {
    std::size_t p = 1;
    while (p < n) p <<= 1;
    return p;
}

// A taker's limit price still trades at this maker level price.
[[nodiscard]] constexpr bool crosses(Side taker_side, int64_t limit_ticks,
                                     int64_t level_price_ticks) noexcept {
    return taker_side == Side::BUY ? limit_ticks >= level_price_ticks
                                   : limit_ticks <= level_price_ticks;
}

}  // namespace

MatchingEngine::MatchingEngine(uint32_t shard_id, OrderBook& book,
                               MemoryPool<Order>& orders, WalWriter* wal,
                               IpcPublisher* publisher,
                               L3Publisher* l3) noexcept
    : shard_id_(shard_id),
      book_(book),
      orders_(orders),
      wal_(wal),
      publisher_(publisher),
      l3_(l3),
      stops_(orders.capacity() < StopOrderTrigger::kDefaultCapacity
                 ? orders.capacity()
                 : StopOrderTrigger::kDefaultCapacity),
      icebergs_(orders.capacity() < IcebergManager::kDefaultCapacity
                    ? orders.capacity()
                    : IcebergManager::kDefaultCapacity) {
    // Order-meta side index: covers every live order's group/expiry at
    // load <= 0.5. Bounded at 2^20 entries — beyond that, orders needing
    // meta are rejected BOOK_CAPACITY (fail-closed, never untracked).
    std::size_t cap = next_pow2(2 * orders.capacity());
    if (cap > (std::size_t{1} << 20)) cap = std::size_t{1} << 20;
    meta_ = new (std::nothrow) OrderMeta[cap]();
    if (meta_ != nullptr) {
        meta_cap_ = cap;
        heap_ = new (std::nothrow) ExpiryEntry[cap];
        heap_cap_ = heap_ != nullptr ? cap : 0;
    }
    // Phase-14 Task 14.3.1 — OCO member side table: two entries per linked
    // pair, same bounded capacity domain as meta_. A null table degrades
    // link install to a fail-closed reject (oco_ensure returns nullptr).
    oco_ = new (std::nothrow) OcoMember[cap]();
    oco_cap_ = oco_ != nullptr ? cap : 0;
    // Phase-16 Task 16.3.11 — pegged-order record table: fixed compact
    // array (order_id key, linear scans). A null table degrades pegged
    // admission to fail-closed ORDER_INVALID (never an untracked peg).
    pegs_ = new (std::nothrow) PegRec[kPegsCap];
    pegs_cap_ = pegs_ != nullptr ? kPegsCap : 0;
}

MatchingEngine::~MatchingEngine() {
    delete[] heap_;
    delete[] meta_;
    delete[] oco_;
    delete[] pegs_;
}

// ---------------------------------------------------------------------------
// meta map (open addressing, linear probe, load <= 0.5 — Fibonacci hashing
// identical to OrderBook's id index)
// ---------------------------------------------------------------------------

MatchingEngine::OrderMeta* MatchingEngine::meta_find(uint64_t id) noexcept {
    if (meta_ == nullptr || meta_live_ == 0) return nullptr;
    const std::size_t mask = meta_cap_ - 1;
    std::size_t i =
        static_cast<std::size_t>(id * 0x9E3779B97F4A7C15ull) & mask;
    for (;;) {
        if (meta_[i].order_id == 0) return nullptr;
        if (meta_[i].order_id == id) return &meta_[i];
        i = (i + 1) & mask;
    }
}

const MatchingEngine::OrderMeta* MatchingEngine::meta_find(
    uint64_t id) const noexcept {
    return const_cast<MatchingEngine*>(this)->meta_find(id);
}

MatchingEngine::OrderMeta* MatchingEngine::meta_ensure(uint64_t id) noexcept {
    if (meta_ == nullptr) return nullptr;
    const std::size_t mask = meta_cap_ - 1;
    std::size_t i =
        static_cast<std::size_t>(id * 0x9E3779B97F4A7C15ull) & mask;
    for (;;) {
        if (meta_[i].order_id == 0) {
            if (meta_live_ * 2 >= meta_cap_) return nullptr;  // full
            meta_[i].order_id = id;
            meta_[i].trade_group_id = 0;
            meta_[i].expiry_ns = 0;
            meta_[i].heap_index = -1;
            meta_[i].amend_seq = 0;
            meta_[i].amend_seen = false;
            meta_[i].prevented_qty_units = 0;
            ++meta_live_;
            return &meta_[i];
        }
        if (meta_[i].order_id == id) return &meta_[i];
        i = (i + 1) & mask;
    }
}

void MatchingEngine::meta_erase(uint64_t id) noexcept {
    if (meta_ == nullptr || meta_live_ == 0) return;
    const std::size_t mask = meta_cap_ - 1;
    std::size_t i =
        static_cast<std::size_t>(id * 0x9E3779B97F4A7C15ull) & mask;
    for (;;) {
        if (meta_[i].order_id == 0) return;
        if (meta_[i].order_id == id) break;
        i = (i + 1) & mask;
    }
    if (meta_[i].heap_index >= 0) {
        expiry_remove_at(static_cast<std::size_t>(meta_[i].heap_index));
    }
    // Probe-chain compaction (same scheme as IcebergManager::erase).
    std::size_t j = i;
    for (;;) {
        j = (j + 1) & mask;
        if (meta_[j].order_id == 0) break;
        const std::size_t home = static_cast<std::size_t>(
            meta_[j].order_id * 0x9E3779B97F4A7C15ull) & mask;
        if (((i - home) & mask) < ((j - home) & mask)) {
            meta_[i] = meta_[j];  // heap_index travels with the entry
            i = j;
        }
    }
    meta_[i] = OrderMeta{};
    --meta_live_;
}

uint32_t MatchingEngine::group_of(uint64_t order_id) const noexcept {
    const OrderMeta* m = meta_find(order_id);
    return m != nullptr ? m->trade_group_id : 0;
}

// ---------------------------------------------------------------------------
// OCO member map + link lifecycle (Phase-14 Task 14.3.1, spec §6.2/§6.5)
//
// The link arrives as a dedicated OcoLink wire event sequenced BEFORE both
// legs' OrderNew on the shard ring. Each member occupies one open-addressed
// entry pointing at its sibling; states:
//   ARMED  — member live (book/pending-stop) or not yet arrived; its fill
//            cancels the sibling atomically (journaled reason-7 cancel).
//   DOOMED — sibling already reached terminal FILLED while this leg's
//            OrderNew was still in flight on the same ring; the arriving
//            order is rejected OCO_SIBLING_CANCEL_RACE without a WAL
//            ORDER_NEW (the deterministic loser of the §6.5 race).
// Any non-fill terminal (user cancel, expiry, STP, IOC/FOK remainder,
// admission reject) dissolves the pair: the surviving leg keeps running
// unlinked — one-cancels-other is a fill contract, not a cancel cascade.
// ---------------------------------------------------------------------------

MatchingEngine::OcoMember* MatchingEngine::oco_find(uint64_t id) noexcept {
    if (oco_ == nullptr || oco_live_ == 0) return nullptr;
    const std::size_t mask = oco_cap_ - 1;
    std::size_t i =
        static_cast<std::size_t>(id * 0x9E3779B97F4A7C15ull) & mask;
    for (;;) {
        if (oco_[i].order_id == 0) return nullptr;
        if (oco_[i].order_id == id) return &oco_[i];
        i = (i + 1) & mask;
    }
}

const MatchingEngine::OcoMember* MatchingEngine::oco_find(
    uint64_t id) const noexcept {
    return const_cast<MatchingEngine*>(this)->oco_find(id);
}

MatchingEngine::OcoMember* MatchingEngine::oco_ensure(uint64_t id) noexcept {
    if (oco_ == nullptr) return nullptr;
    const std::size_t mask = oco_cap_ - 1;
    std::size_t i =
        static_cast<std::size_t>(id * 0x9E3779B97F4A7C15ull) & mask;
    for (;;) {
        if (oco_[i].order_id == 0) {
            if (oco_live_ * 2 >= oco_cap_) return nullptr;  // full
            oco_[i].order_id = id;
            oco_[i].link_id = 0;
            oco_[i].sibling_id = 0;
            oco_[i].instrument_id = 0;
            oco_[i].state = kOcoArmed;
            ++oco_live_;
            return &oco_[i];
        }
        if (oco_[i].order_id == id) return &oco_[i];
        i = (i + 1) & mask;
    }
}

void MatchingEngine::oco_erase(uint64_t id) noexcept {
    if (oco_ == nullptr || oco_live_ == 0) return;
    const std::size_t mask = oco_cap_ - 1;
    std::size_t i =
        static_cast<std::size_t>(id * 0x9E3779B97F4A7C15ull) & mask;
    for (;;) {
        if (oco_[i].order_id == 0) return;
        if (oco_[i].order_id == id) break;
        i = (i + 1) & mask;
    }
    // Probe-chain compaction (same scheme as meta_erase).
    std::size_t j = i;
    for (;;) {
        j = (j + 1) & mask;
        if (oco_[j].order_id == 0) break;
        const std::size_t home = static_cast<std::size_t>(
            oco_[j].order_id * 0x9E3779B97F4A7C15ull) & mask;
        if (((i - home) & mask) < ((j - home) & mask)) {
            oco_[i] = oco_[j];
            i = j;
        }
    }
    oco_[i] = OcoMember{};
    --oco_live_;
}

void MatchingEngine::oco_on_dead(uint64_t order_id, bool by_fill) noexcept {
    const OcoMember* m = oco_find(order_id);
    if (m == nullptr) return;  // unlinked order — nothing to do
    const uint64_t sibling = m->sibling_id;
    oco_erase(order_id);  // the dead member releases the pair
    if (!by_fill) {
        // Non-fill terminal: the sibling continues as a standalone order —
        // drop its member entry so a later fill of it finds no stale link.
        oco_erase(sibling);
        return;
    }
    // Fill winner (spec §6.5): cancel the sibling atomically on this thread.
    // The member entry is authoritative for "link still open" while
    // book/stops membership is authoritative for "sibling currently live":
    //   * live sibling    -> journaled ORDER_CANCEL (reason OCO_LINK) +
    //     published terminal notice, inside cancel_internal — identical to
    //     a user cancel of a resting book order or a pending stop;
    //   * entry but not live -> the sibling's OrderNew is still sequenced
    //     behind us on the ring: mark DOOMED so the doomed-leg gate in
    //     on_order_received rejects it OCO_SIBLING_CANCEL_RACE on arrival;
    //   * no entry -> sibling already terminated; pair already closed.
    OcoMember* s = oco_find(sibling);
    if (s == nullptr || s->state == kOcoDoomed) return;
    if (book_.find_order(sibling) != nullptr ||
        stops_.find(sibling) != nullptr) {
        oco_erase(sibling);  // unlink first — the cancel below then can't
                             // re-enter this hook for the sibling
        (void)cancel_internal(sibling, 0, kWalCancelReasonOcoLink,
                              /*check_account*/ false);
    } else {
        s->state = kOcoDoomed;
    }
}

int MatchingEngine::oco_member_state(uint64_t order_id) const noexcept {
    const OcoMember* m = oco_find(order_id);
    return m == nullptr ? -1 : static_cast<int>(m->state);
}

void MatchingEngine::on_oco_link_received(uint64_t link_id,
                                          uint64_t order_id_a,
                                          uint64_t order_id_b,
                                          uint64_t account_id,
                                          uint32_t instrument_id) noexcept {
    if (wal_fault_) {
        // Halted engine — fail closed, link is never silently half-applied.
        last_reject_ = kRejectBookCapacity;
        ++reject_count_;
        return;
    }
    if (link_id == 0 || order_id_a == 0 || order_id_b == 0 ||
        order_id_a == order_id_b) {
        // Malformed link command — rejected pre-journal, so replay never
        // sees it (rejects are not state changes).
        last_reject_ = kRejectOcoLinkInvalid;
        ++reject_count_;
        return;
    }
    const OcoMember* ea = oco_find(order_id_a);
    const OcoMember* eb = oco_find(order_id_b);
    if (ea != nullptr || eb != nullptr) {
        const bool same =
            ea != nullptr && eb != nullptr && ea->link_id == link_id &&
            ea->sibling_id == order_id_b && eb->link_id == link_id &&
            eb->sibling_id == order_id_a;
        if (same) return;  // idempotent re-send of an installed link
        last_reject_ = kRejectOcoLinkConflict;
        ++reject_count_;
        return;
    }
    // Capacity check precedes the journal append: a rejected link leaves no
    // WAL row, so replay stays consistent — a journaled link that failed
    // to install in memory would diverge.
    if (oco_ == nullptr || (oco_live_ + 2) * 2 >= oco_cap_) {
        last_reject_ = kRejectBookCapacity;
        ++reject_count_;
        return;
    }
    // WAL first — the link is a committed state change.
    WalOcoLinkPayload p{};
    p.link_id = link_id;
    p.order_id_a = order_id_a;
    p.order_id_b = order_id_b;
    p.account_id = account_id;
    p.instrument_id = instrument_id;
    (void)journal_seq();   // consume the link row's seq (no L3 side)
    if (wal_ != nullptr &&
        wal_->write_oco_link(p, now_ns_) != WalStatus::Ok) {
        wal_fault_ = true;
        last_reject_ = kRejectBookCapacity;
        ++reject_count_;
        return;
    }
    OcoMember* ma = oco_ensure(order_id_a);
    OcoMember* mb = oco_ensure(order_id_b);
    // The capacity gate above guarantees both inserts succeed.
    ma->link_id = link_id;
    ma->sibling_id = order_id_b;
    ma->instrument_id = instrument_id;
    ma->state = kOcoArmed;
    mb->link_id = link_id;
    mb->sibling_id = order_id_a;
    mb->instrument_id = instrument_id;
    mb->state = kOcoArmed;
}

// ---------------------------------------------------------------------------
// expiry min-heap — key (expiry_ns, order_id), total order => deterministic
// ---------------------------------------------------------------------------

bool MatchingEngine::expiry_track(OrderMeta& m) noexcept {
    if (m.expiry_ns <= 0) return true;  // nothing to track
    if (heap_ == nullptr || heap_size_ >= heap_cap_) return false;
    const std::size_t i = heap_size_++;
    heap_[i] = {m.expiry_ns, m.order_id};
    m.heap_index = static_cast<int32_t>(i);
    expiry_sift_up(i);
    return true;
}

bool MatchingEngine::track_meta(uint64_t order_id,
                                const OrderAux& aux) noexcept {
    if (aux.trade_group_id == 0 && aux.gtd_expiry_ns <= 0 &&
        aux.trigger_source == kTriggerSourceLast &&
        aux.instrument_id == 0) {
        return true;
    }
    // An expiring order without heap storage would silently miss its GTD —
    // fail closed instead of tracking only half the metadata.
    if (aux.gtd_expiry_ns > 0 && heap_ == nullptr) return false;
    OrderMeta* m = meta_ensure(order_id);
    if (m == nullptr) return false;
    m->trade_group_id = aux.trade_group_id;
    m->trigger_source = aux.trigger_source;
    m->instrument_id = aux.instrument_id;
    if (aux.gtd_expiry_ns > 0) {
        m->expiry_ns = aux.gtd_expiry_ns;
        if (m->heap_index < 0 && !expiry_track(*m)) return false;
    }
    return true;
}

void MatchingEngine::expiry_untrack(OrderMeta& m) noexcept {
    if (m.heap_index >= 0) {
        expiry_remove_at(static_cast<std::size_t>(m.heap_index));
        m.heap_index = -1;
    }
}

void MatchingEngine::expiry_sift_up(std::size_t i) noexcept {
    while (i > 0) {
        const std::size_t p = (i - 1) >> 1;
        const ExpiryEntry c = heap_[i];
        const ExpiryEntry q = heap_[p];
        if (c.expiry_ns > q.expiry_ns ||
            (c.expiry_ns == q.expiry_ns && c.order_id > q.order_id)) {
            break;
        }
        heap_[i] = q;
        if (OrderMeta* m = meta_find(q.order_id)) {
            m->heap_index = static_cast<int32_t>(i);
        }
        heap_[p] = c;
        if (OrderMeta* m = meta_find(c.order_id)) {
            m->heap_index = static_cast<int32_t>(p);
        }
        i = p;
    }
}

void MatchingEngine::expiry_sift_down(std::size_t i) noexcept {
    for (;;) {
        const std::size_t l = 2 * i + 1;
        const std::size_t r = l + 1;
        std::size_t small = i;
        if (l < heap_size_ &&
            (heap_[l].expiry_ns < heap_[small].expiry_ns ||
             (heap_[l].expiry_ns == heap_[small].expiry_ns &&
              heap_[l].order_id < heap_[small].order_id))) {
            small = l;
        }
        if (r < heap_size_ &&
            (heap_[r].expiry_ns < heap_[small].expiry_ns ||
             (heap_[r].expiry_ns == heap_[small].expiry_ns &&
              heap_[r].order_id < heap_[small].order_id))) {
            small = r;
        }
        if (small == i) break;
        const ExpiryEntry tmp = heap_[i];
        heap_[i] = heap_[small];
        heap_[small] = tmp;
        if (OrderMeta* m = meta_find(heap_[i].order_id)) {
            m->heap_index = static_cast<int32_t>(i);
        }
        if (OrderMeta* m = meta_find(heap_[small].order_id)) {
            m->heap_index = static_cast<int32_t>(small);
        }
        i = small;
    }
}

void MatchingEngine::expiry_remove_at(std::size_t i) noexcept {
    if (heap_ == nullptr || i >= heap_size_) return;
    if (OrderMeta* m = meta_find(heap_[i].order_id)) m->heap_index = -1;
    --heap_size_;
    if (i != heap_size_) {
        heap_[i] = heap_[heap_size_];
        if (OrderMeta* m = meta_find(heap_[i].order_id)) {
            m->heap_index = static_cast<int32_t>(i);
        }
        expiry_sift_up(i);
        expiry_sift_down(i);
    }
}

void MatchingEngine::expire_due() noexcept {
    while (heap_size_ > 0 &&
           heap_[0].expiry_ns <= static_cast<int64_t>(now_ns_)) {
        const uint64_t id = heap_[0].order_id;
        expiry_remove_at(0);
        (void)cancel_internal(id, 0, kWalCancelReasonExpired, false);
    }
}

// ---------------------------------------------------------------------------
// reject / publish helpers
// ---------------------------------------------------------------------------

void MatchingEngine::reject(const Order* order, const char* code) noexcept {
    last_reject_ = code;
    ++reject_count_;
    if (publisher_ != nullptr && order != nullptr) {
        (void)publisher_->publish_order_cancel(order->id, order->account_id,
                                         now_ns_);
    }
    // Phase-14 Task 14.3.1 — a rejected leg never trades; dissolve its OCO
    // pair so the sibling (if already live) continues standalone.
    if (order != nullptr) oco_on_dead(order->id, /*by_fill=*/false);
}

// --- L3 journal correlation + emission (Phase-17 Task 17.3.1, spec §11) ---
//
// journal_seq() is the seq the NEXT journaled row will commit with: the
// real WAL tail in live mode (WalWriter assigns seq == tail_seq() at call
// time), or the virtual cursor in journal-free replay — consumed at EVERY
// journal site so derived rows keep the consecutive seqs the single
// writer stamped live. RecoveryManager re-anchors the cursor per replayed
// entry via set_replay_wal_seq(); unanchored journal-free engines (unit
// tests) simply count from 0.
uint64_t MatchingEngine::journal_seq() noexcept {
    return wal_ != nullptr ? wal_->tail_seq() : replay_wal_seq_++;
}

void MatchingEngine::set_replay_wal_seq(uint64_t seq) noexcept {
    replay_wal_seq_ = seq;
}

uint8_t MatchingEngine::l3_flags_of(const Order& o) const noexcept {
    uint8_t f = kL3FlagDetail;
    if (!l2_visible(o)) f |= kL3FlagHidden;
    if (o.type == OrderType::PEG) f |= kL3FlagPegged;
    if (o.type == OrderType::ICEBERG) f |= kL3FlagIceberg;
    return f;
}

const Order* MatchingEngine::l3_order_detail(uint64_t order_id) noexcept {
    if (const Order* o = book_.find_order(order_id)) return o;
    if (const StopOrderTrigger::Pending* s = stops_.find(order_id))
        return s->order;
    if (const Order* p = parked_find(order_id)) return p;
    return nullptr;
}

void MatchingEngine::l3_emit_ev(uint8_t kind, uint64_t order_id,
                                uint64_t account_id, Side side,
                                uint32_t instrument_id, int64_t price_ticks,
                                int64_t ref_price_ticks, int64_t qty_units,
                                int64_t qty_delta, uint64_t trade_id,
                                uint8_t fill_role, uint8_t flags,
                                uint8_t cancel_reason,
                                uint64_t wal_seq) noexcept {
    if (l3_ == nullptr) return;
    L3Event ev{};
    ev.instrument_id = instrument_id;
    ev.kind = kind;
    ev.order_id = order_id;
    ev.account_id = account_id;
    ev.side = side;
    ev.price_ticks = price_ticks;
    ev.ref_price_ticks = ref_price_ticks;
    ev.qty_units = qty_units;
    ev.qty_delta = qty_delta;
    ev.trade_id = trade_id;
    ev.fill_role = fill_role;
    ev.flags = flags;
    ev.cancel_reason = cancel_reason;
    ev.wal_seq = wal_seq;
    ev.ts_ns = now_ns_;
    (void)l3_->publish(ev);
}

void MatchingEngine::l3_cancel(uint64_t order_id, uint64_t account_id,
                               uint8_t wal_reason, uint64_t wal_seq,
                               const Order* detail) noexcept {
    if (l3_ == nullptr) return;
    if (detail != nullptr) {
        // qty = post-event remaining (0); delta = -(remaining removed).
        // Order-level accounting for icebergs uses the hidden-reserve
        // effective remaining, not the visible slice.
        const int64_t rem = effective_remaining_units(detail);
        l3_emit_ev(static_cast<uint8_t>(L3Kind::Cancel), order_id,
                   detail->account_id, detail->side,
                   instrument_id_of(OrderAux{}), detail->price_ticks,
                   /*ref_price*/ 0, /*qty*/ 0, /*qty_delta*/ -rem,
                   /*trade_id*/ 0, kL3RoleNone, l3_flags_of(*detail),
                   wal_reason, wal_seq);
        return;
    }
    // Detail unresolvable (never-admitted / already-torn-down id): emit the
    // minimal honest row — kL3FlagDetail left clear.
    l3_emit_ev(static_cast<uint8_t>(L3Kind::Cancel), order_id, account_id,
               Side::BUY, instrument_id_of(OrderAux{}), 0, 0, 0, 0, 0,
               kL3RoleNone, 0, wal_reason, wal_seq);
}

void MatchingEngine::emit_cancel_event(uint64_t order_id, uint64_t account_id,
                                       uint8_t wal_reason,
                                       const Order* detail) noexcept {
    const uint64_t wseq = journal_seq();
    bool journaled = true;   // journal-free: vacuously committed
    if (wal_ != nullptr &&
        wal_->write_order_cancel(order_id, account_id, wal_reason, now_ns_) !=
            WalStatus::Ok) {
        wal_fault_ = true;
        journaled = false;
    }
    // L3 Cancel only once the row is durable — wal_seq must point at a
    // committed journal row (§24 #318).
    if (journaled) {
        if (detail == nullptr) detail = l3_order_detail(order_id);
        l3_cancel(order_id, account_id, wal_reason, wseq, detail);
    }
    if (publisher_ != nullptr) {
        (void)publisher_->publish_order_cancel(order_id, account_id, now_ns_,
                                               wal_reason);
    }
    // Phase-14 Task 14.3.1 — every emit_cancel_event caller is a
    // non-fill terminal path (taker remainder kill, admission reject,
    // STP taker death, iceberg replenish failure). Release the OCO pair:
    // the surviving leg continues as a standalone order.
    oco_on_dead(order_id, /*by_fill=*/false);
}

uint32_t MatchingEngine::instrument_id_of(const OrderAux& aux) const noexcept {
    if (aux.instrument_id != 0) return aux.instrument_id;
    const Instrument* i = book_.instrument();
    return i != nullptr ? static_cast<uint32_t>(i->instrument_id) : 0;
}

// ---------------------------------------------------------------------------
// Phase-22 Task 22.3.12 — implied matching (spec §24 #199/#200)
// ---------------------------------------------------------------------------

bool MatchingEngine::implied_ensure_registered() noexcept {
    if (implied_ == nullptr) return false;
    if (implied_registered_) return true;
    const Instrument* ins = book_.instrument();
    if (ins == nullptr || ins->instrument_id == 0) return false;
    // Registration is a cold-path append into the matcher's book table;
    // a failure (duplicate id / capacity) latches registered_ true and
    // leaves the engine running outright-only — fail safe, not fatal.
    implied_registered_ = implied_->register_book(
        static_cast<uint32_t>(ins->instrument_id), book_, wal_, publisher_,
        &implied_fill_trampoline, this);
    if (!implied_registered_) {
        implied_registered_ = true;   // do not retry every event
        implied_ = nullptr;           // unbind — outright-only engine
    }
    return implied_ != nullptr;
}

void MatchingEngine::implied_take(Order& order, const OrderAux& aux,
                                  TakerResult& r, bool has_limit,
                                  int64_t limit_ticks) noexcept {
    if (implied_ == nullptr || r.remaining <= 0 || r.dead || wal_fault_) {
        return;
    }
    // FOK stays outright-only (§27 Phase-22 ruling): an implied-only or
    // mixed-liquidity feasibility proof would need a combined read-only
    // capacity walk that preserves STP/collar verdicts exactly; a false
    // positive would commit a partial FOK. post_only takers probe first —
    // crossing implied liquidity rejects POST_ONLY_VIOLATION, mirroring
    // the pre-trade gate's outright rule.
    if (order.tif == TimeInForce::FOK) return;
    if (!implied_ensure_registered()) {
        return;
    }
    const ImpliedMatcher::Result ir =
        implied_->match_incoming(instrument_id_of(aux), order, now_ns_,
                                 tid_stream(), has_limit, limit_ticks);
    if (ir.wal_fault) {
        wal_fault_ = true;  // shared fault flag — journaled prefix is
                            // replay-consistent, remainder cannot rest
        return;
    }
    if ((order.flags & kOrderFlagPostOnly) != 0 && ir.would_match) {
        r.dead = true;
        r.dead_reason = kWalCancelReasonUser;
        r.dead_code = kRejectPostOnlyViolation;
        return;
    }
    if (ir.filled_units > 0) {
        r.remaining -= ir.filled_units;
        if (r.remaining < 0) r.remaining = 0;  // defensive — matcher is
                                               // bounded by remaining
    }
}

void MatchingEngine::implied_rescan() noexcept {
    if (implied_ == nullptr || wal_fault_) return;
    if (!implied_ensure_registered()) return;
    const Instrument* ins = book_.instrument();
    if (ins == nullptr) return;
    const uint32_t served = static_cast<uint32_t>(ins->instrument_id);
    // Bounded local fixpoint (same discipline as settle()'s 8 passes):
    // an implied fill inside book_ can move book_ in its role as a LEG
    // of other links, which may expose fresh crossings — rescan until
    // stable. Implied fills also advance last_price_ticks_, so a dirty
    // pass re-runs the conditional settle wave to fire LAST triggers.
    constexpr int kMaxImpliedPasses = 8;
    for (int pass = 0; pass < kMaxImpliedPasses; ++pass) {
        implied_dirty_ = false;
        const ImpliedMatcher::Result r =
            implied_->on_book_changed(served, now_ns_, tid_stream());
        if (r.wal_fault) {
            wal_fault_ = true;
            return;
        }
        if (!implied_dirty_) return;   // no fills touched book_ — stable
        settle();
        if (wal_fault_) return;
    }
}

void MatchingEngine::implied_sync() noexcept {
    implied_rescan();
}

void MatchingEngine::implied_fill_trampoline(
    void* ctx, const FillNote& n) noexcept {
    static_cast<MatchingEngine*>(ctx)->on_implied_fill(n);
}

void MatchingEngine::on_implied_fill(
    const FillNote& n) noexcept {
    // Any trade printed in this book advances the LAST-source trigger
    // reference and the emitted-trade counter — the settle wave fired by
    // implied_rescan()/the host drain then evaluates stop triggers
    // against it exactly like an outright fill.
    last_price_ticks_ = n.price_ticks;
    ++trades_emitted_;
    implied_dirty_ = true;
    if (!n.taker_leg) {
        // Resting-order bookkeeping walk_match performs around apply_fill
        // — the matcher's raw fill bypassed it: iceberg reserves account
        // the fill and replenish a dead visible slice; a terminal order
        // releases its meta slot and arms the OCO winner.
        if (auto* rec = icebergs_.find(n.order_id)) {
            rec->filled_total_units += n.qty_units;
            if (book_.find_order(n.order_id) == nullptr) {
                replenish_iceberg(rec);
            }
        }
        if (book_.find_order(n.order_id) == nullptr) {
            meta_erase(n.order_id);
            if (icebergs_.find(n.order_id) == nullptr) {
                oco_on_dead(n.order_id, /*by_fill=*/true);
            }
        }
    }
    // Price-improvement accounting on the out-book presentation fill —
    // delta vs the filled order's OWN limit, same rule as walk_match.
    if (n.out_presentation) {
        improvement_.on_fill(n.side, n.order_id, n.trade_id,
                             n.limit_ticks, n.price_ticks, n.qty_units);
    }
    if (l3_ != nullptr) {
        const Order* live = book_.find_order(n.order_id);
        const int64_t rem_after =
            live != nullptr ? effective_remaining_units(live)
                            : (n.order_remaining_after >= 0
                                   ? n.order_remaining_after : 0);
        l3_emit_ev(static_cast<uint8_t>(L3Kind::Fill), n.order_id,
                   n.account_id, n.side, n.instrument_id, n.price_ticks,
                   /*ref*/ 0, rem_after, /*delta*/ -n.qty_units,
                   n.trade_id,
                   n.taker_leg ? kL3RoleTaker : kL3RoleMaker, 0, 0,
                   n.wal_seq);
    }
}

void MatchingEngine::publish_depth() noexcept {
    // Conflated L2 depth (spec §10.2 — the feed is a 100ms-conflated top-20
    // stream; §6.6b — it may lag the internal book and is display-only).
    // Serialization walks the top-40 level chains + builds a flatbuffer on
    // the matching thread, so hot-path mutations coalesce: emit immediately
    // when the interval has elapsed (a quiet book keeps per-mutation
    // freshness), else defer to the next on_time_tick flush (~1ms cadence).
    if (publisher_ == nullptr) return;
    depth_dirty_ = true;
    if (depth_ever_emitted_ && now_ns_ - last_depth_pub_ns_ < kDepthPubIntervalNs) {
        return;
    }
    flush_depth();
}

void MatchingEngine::flush_depth() noexcept {
    if (!depth_dirty_ || publisher_ == nullptr) return;
    depth_dirty_ = false;
    depth_ever_emitted_ = true;
    last_depth_pub_ns_ = now_ns_;
    (void)publisher_->publish_book_snapshot(book_, instrument_id_of(OrderAux{}), now_ns_);
}

void MatchingEngine::publish_auction_indicative() noexcept {
    // CALL-phase indicative stream (Task 15.3.6): emit only when the
    // clearing pair moved — dedupe keeps the wire quiet during quiet
    // accumulation windows.
    if (auction_phase_ != kAuctionPhaseCall || publisher_ == nullptr) {
        return;
    }
    const AuctionIndicative ind = auction_indicative();
    if (ind.price_ticks == last_indicative_price_ &&
        ind.exec_qty_units == last_indicative_qty_) {
        return;
    }
    (void)publisher_->publish_auction_event(
        instrument_id_of(OrderAux{}), static_cast<uint64_t>(auction_id_),
        kAuctionPhaseCall, /*signal*/ 0, ind.price_ticks, ind.exec_qty_units,
        ind.buy_qty_units - ind.sell_qty_units, auction_deadline_ns_, 0,
        now_ns_);
    last_indicative_price_ = ind.price_ticks;
    last_indicative_qty_ = ind.exec_qty_units;
}

// ---------------------------------------------------------------------------
// cancel — pending stop first, then resting book order; idempotent (absent
// is a no-op, never a second mutation)
// ---------------------------------------------------------------------------

bool MatchingEngine::cancel_internal(uint64_t order_id, uint64_t account_id,
                                     uint8_t wal_reason,
                                     bool check_account) noexcept {
    if (wal_fault_) return false;

    // Pending stop order?
    if (auto* p = stops_.find(order_id)) {
        Order* s = p->order;
        const uint64_t acct = s->account_id;
        const int64_t gslo_n = p->gslo_notional_units;  // read before remove
        if (check_account && acct != account_id) return false;
        const uint64_t wseq = journal_seq();
        if (wal_ != nullptr &&
            wal_->write_order_cancel(order_id, acct, wal_reason, now_ns_) !=
                WalStatus::Ok) {
            wal_fault_ = true;
            return false;
        }
        l3_cancel(order_id, acct, wal_reason, wseq, s);  // pending detail
        s = stops_.remove(order_id);
        gslo_release(gslo_n);  // Task 16.3.16 — exposure released on cancel
        meta_erase(order_id);
        if (publisher_ != nullptr) {
            (void)publisher_->publish_order_cancel(order_id, acct, now_ns_,
                                                   wal_reason);
        }
        oco_on_dead(order_id, /*by_fill=*/false);
        orders_.free(s);
        return true;
    }

    // Auction-parked order? (MARKET/IOC/FOK resting in the parked list
    // while a CALL accumulates — Task 15.3.6; cancel stays available.)
    if (Order* pk = parked_find(order_id)) {
        const uint64_t acct = pk->account_id;
        if (check_account && acct != account_id) return false;
        // Task 16.3.25 (spec §6.2b): inside the T-30s freeze before an
        // armed deadline a USER cancel of a parked MOO/MOC is refused — the
        // order is committed to the uncross. Engine-driven drains pass a
        // non-user reason and are never frozen.
        if ((pk->type == OrderType::MOO || pk->type == OrderType::MOC) &&
            wal_reason == kWalCancelReasonUser &&
            auction_phase_ == kAuctionPhaseCall &&
            auction_deadline_ns_ > 0 &&
            now_ns_ + kAuctionFreezeNs >=
                static_cast<uint64_t>(auction_deadline_ns_)) {
            last_reject_ = kRejectAmendInAuction;  // freeze code
            ++reject_count_;
            return true;  // handled: refused — the order stays parked
        }
        const uint64_t wseq = journal_seq();
        if (wal_ != nullptr &&
            wal_->write_order_cancel(order_id, acct, wal_reason, now_ns_) !=
                WalStatus::Ok) {
            wal_fault_ = true;
            return false;
        }
        l3_cancel(order_id, acct, wal_reason, wseq, pk);  // parked detail
        Order* prev = nullptr;
        for (Order* c = parked_head_; c != nullptr && c != pk;
             c = c->next) {
            prev = c;
        }
        if (prev != nullptr) prev->next = pk->next;
        else parked_head_ = pk->next;
        if (parked_tail_ == pk) parked_tail_ = prev;
        pk->next = nullptr;
        --parked_count_;
        meta_erase(order_id);
        if (publisher_ != nullptr) {
            (void)publisher_->publish_order_cancel(order_id, acct, now_ns_,
                                                   wal_reason);
        }
        oco_on_dead(order_id, /*by_fill=*/false);
        orders_.free(pk);
        return true;
    }

    Order* o = book_.find_order(order_id);
    if (o == nullptr) return false;
    if (check_account && o->account_id != account_id) return false;
    const uint64_t acct = o->account_id;
    const uint64_t wseq = journal_seq();
    if (wal_ != nullptr &&
        wal_->write_order_cancel(order_id, acct, wal_reason, now_ns_) !=
            WalStatus::Ok) {
        wal_fault_ = true;
        return false;
    }
    l3_cancel(order_id, acct, wal_reason, wseq, o);  // resting detail
    if (book_.cancel_order(order_id) != BookError::OK) {
        wal_fault_ = true;  // journaled cancel could not apply — halt
        return false;
    }
    meta_erase(order_id);
    icebergs_.erase(order_id);  // releases hidden remainder if iceberg
    peg_erase(order_id);        // drops a live peg record (no-op otherwise)
    if (publisher_ != nullptr) {
        (void)publisher_->publish_order_cancel(order_id, acct, now_ns_,
                                               wal_reason);
    }
    oco_on_dead(order_id, /*by_fill=*/false);
    return true;
}

// ---------------------------------------------------------------------------
// STP application — maker/taker same account or same nonzero trade group
// (Tasks 2.3.11/2.3.16/2.3.18). Mutates res: sets dead when the taker's
// remainder must be cancelled. Every suppressive outcome journals
// ORDER_CANCEL/ORDER_MODIFY with reason=STP *before* the book mutation;
// TRANSFER additionally journals PREVENTED_MATCH for cross-account group
// matches. PROCEED is the Task 2.3.16 NONE path — no suppression, the
// caller emits the fill normally and flags it to the SELF_TRADE sink.
// ---------------------------------------------------------------------------

int64_t MatchingEngine::effective_remaining_units(
    const Order* maker) const noexcept {
    if (maker == nullptr) return 0;
    if (const auto* rec = icebergs_.find(maker->id)) {
        // Hidden remainder counts: STP suppression consumes the whole
        // iceberg, not just the live visible slice.
        return rec->total_qty_units - rec->filled_total_units;
    }
    return remaining_qty_units(*maker);
}

void MatchingEngine::note_prevented(uint64_t order_id,
                                    int64_t qty_units) noexcept {
    if (qty_units <= 0) return;
    prevented_qty_total_ += qty_units;
    // Per-order cumulative (orders.prevented_qty mirror while live). The
    // slot is best-effort: meta_ensure can only fail when the bounded table
    // is saturated — the engine-total counter and the ORDER_CANCEL/MODIFY
    // (plus PREVENTED_MATCH for TRANSFER) journal remain the authoritative
    // record, so suppression accounting never blocks prevention itself.
    if (OrderMeta* m = meta_ensure(order_id)) {
        m->prevented_qty_units += qty_units;
    }
}

int64_t MatchingEngine::prevented_qty_units(uint64_t order_id) const noexcept {
    const OrderMeta* m = meta_find(order_id);
    return m != nullptr ? m->prevented_qty_units : 0;
}

void MatchingEngine::emit_prevented_match(
    uint64_t maker_id, uint64_t maker_account_id, uint32_t maker_group,
    const Order& taker, uint32_t taker_group, int64_t price_ticks,
    int64_t maker_prevented, int64_t taker_prevented) noexcept {
    // Task 2.3.18 (spec §6.5, §24 #279-280): immutable audit record for a
    // mutually-requested TRANSFER suppression across accounts in one trade
    // group. Journaled BEFORE it is observable downstream; the Phase-03 GL
    // service posts the balanced prevented-notional movement from the WAL
    // (the engine never writes PostgreSQL). The maker ORDER_CANCEL/MODIFY
    // entries carrying the actual book mutation precede this record.
    WalPreventedMatchPayload p{};
    p.maker_order_id = maker_id;
    p.taker_order_id = taker.id;
    p.maker_account_id = maker_account_id;
    p.taker_account_id = taker.account_id;
    p.price_ticks = price_ticks;
    p.maker_prevented_qty_units = maker_prevented;
    p.taker_prevented_qty_units = taker_prevented;
    int64_t notional = 0;
    p.prevented_notional_units =
        notional_units(maker_prevented, price_ticks, notional)
            ? notional
            : INT64_MAX;  // saturating — audit field, deterministic either way
    // The shared nonzero group id (either side resolves it identically —
    // self-match precondition is group equality).
    p.trade_group_id = taker_group != 0 ? taker_group : maker_group;
    p.mode = static_cast<uint8_t>(StpAction::TRANSFER);
    p.ts_ns = now_ns_;
    (void)journal_seq();   // consume the audit row's seq (no L3 side)
    if (wal_ != nullptr &&
        wal_->write_prevented_match(p, now_ns_) != WalStatus::Ok) {
        wal_fault_ = true;
    }
    if (prevented_fn_ != nullptr) prevented_fn_(prevented_ctx_, p);
}

bool MatchingEngine::apply_stp(Order& taker, uint32_t taker_group,
                               Order* maker, int64_t level_price_ticks,
                               StpAction action, int64_t& taker_rem,
                               TakerResult& res) noexcept {
    const uint64_t maker_id = maker->id;
    const uint64_t maker_acct = maker->account_id;
    switch (action) {
        case StpAction::PROCEED:
            return false;  // caller proceeds with a normal fill
        case StpAction::CANCEL_TAKER:
            note_prevented(taker.id, taker_rem);
            res.dead = true;
            res.dead_reason = kWalCancelReasonStp;
            res.dead_code = kRejectStpCancelled;
            return false;
        case StpAction::CANCEL_MAKER:
        case StpAction::CANCEL_BOTH:
            // Suppressed maker qty includes any iceberg hidden remainder —
            // cancel_internal erases the record and the whole order dies.
            note_prevented(maker_id, effective_remaining_units(maker));
            (void)cancel_internal(maker_id, 0, kWalCancelReasonStp, false);
            if (action == StpAction::CANCEL_BOTH) {
                note_prevented(taker.id, taker_rem);
                res.dead = true;
                res.dead_reason = kWalCancelReasonStp;
                res.dead_code = kRejectStpCancelled;
            }
            return true;  // maker consumed from the queue
        case StpAction::DECREMENT:
        case StpAction::TRANSFER: {
            // §6.5: the resting order is reduced by the taker quantity and
            // the incoming remainder is cancelled. An iceberg maker is
            // reduced across slice-then-hidden.
            IcebergManager::Record* rec = icebergs_.find(maker_id);
            const int64_t node_rem = remaining_qty_units(*maker);
            const int64_t eff_rem =
                rec != nullptr
                    ? rec->total_qty_units - rec->filled_total_units
                    : node_rem;
            const int64_t dec = taker_rem < eff_rem ? taker_rem : eff_rem;
            const int64_t node_take = dec < node_rem ? dec : node_rem;
            const int64_t new_node_qty = maker->qty_units - node_take;
            const int64_t maker_price = maker->price_ticks;
            const uint64_t maker_ts = maker->timestamp_ns;
            const uint64_t maker_seq = maker->ingress_seq;
            // L3 detail captured pre-mutation — the node may be pool-freed.
            const Side maker_side = maker->side;
            const uint8_t maker_flags = l3_flags_of(*maker);
            bool maker_died = false;
            bool applied = true;
            bool stp_journaled = false;   // a CANCEL/MODIFY row committed
            uint64_t stp_wseq = 0;
            if (new_node_qty <= maker->filled_qty_units) {
                // Slice fully consumed by the decrement -> cancel it.
                stp_wseq = journal_seq();
                if (wal_ != nullptr &&
                    wal_->write_order_cancel(maker_id, maker_acct,
                                             kWalCancelReasonStp, now_ns_) !=
                        WalStatus::Ok) {
                    wal_fault_ = true;
                    applied = false;
                } else if (book_.cancel_order(maker_id) != BookError::OK) {
                    wal_fault_ = true;
                    applied = false;
                } else {
                    maker_died = true;
                    stp_journaled = true;
                    if (publisher_ != nullptr) {
                        (void)publisher_->publish_order_cancel(maker_id, maker_acct,
                                                         now_ns_);
                    }
                }
            } else if (node_take > 0) {
                // qty-down at the same price preserves queue priority.
                stp_wseq = journal_seq();
                if (wal_ != nullptr &&
                    wal_->write_order_modify(maker_id, maker_price,
                                             new_node_qty, 0, now_ns_) !=
                        WalStatus::Ok) {
                    wal_fault_ = true;
                    applied = false;
                } else if (book_.modify_order(maker_id, maker_price,
                                              new_node_qty, maker_ts,
                                              maker_seq) != BookError::OK) {
                    wal_fault_ = true;
                    applied = false;
                } else {
                    stp_journaled = true;
                }
            }
            if (!applied) {
                res.dead = true;
                res.dead_reason = kWalCancelReasonStp;
                res.dead_code = kRejectStpCancelled;
                return false;
            }
            const int64_t hidden_take = dec - node_take;
            if (rec != nullptr) {
                if (maker_died) {
                    // Task 2.3.11 iceberg edge case: cancelling the slice
                    // node does NOT shrink total_qty_units — the slice's
                    // remaining units return to the hidden reserve and would
                    // replenish back. Suppress the WHOLE dec (slice share +
                    // hidden share) from the order total so the resting
                    // quantity is reduced by the taker quantity exactly.
                    rec->total_qty_units -= dec;
                    replenish_iceberg(rec);  // may erase the record
                } else {
                    // Surviving slice: the node shrink already accounts for
                    // dec (hidden_take == 0 here — node died otherwise).
                    rec->total_qty_units -= hidden_take;
                    if (rec->total_qty_units <= rec->filled_total_units) {
                        icebergs_.erase(maker_id);
                        meta_erase(maker_id);
                    }
                }
            } else if (maker_died) {
                meta_erase(maker_id);
            }
            // Task 2.3.18 prevented-qty accounting (orders.prevented_qty):
            // `dec` was suppressed on the maker side; the taker's entire
            // post-decrement remainder is cancelled below (spec §6.5
            // literal), so both sides accrue suppression. Liveness is tested
            // through the book — an iceberg slice node may have died while
            // the order itself survives via replenish, and its meta slot is
            // still the right accumulator. A truly dead maker's meta slot is
            // gone — its suppression lives in prevented_qty_total_ + WAL.
            if (book_.find_order(maker_id) != nullptr) {
                note_prevented(maker_id, dec);
            } else {
                prevented_qty_total_ += dec;
            }
            // L3 (Task 17.3.1): order-level view of the suppression —
            // Cancel when the order died, Modify of the order-level
            // remaining when it survives (iceberg slice collapse or a
            // qty-decremented live node). wal_seq correlates to the
            // committed ORDER_CANCEL/ORDER_MODIFY row above.
            if (l3_ != nullptr && stp_journaled) {
                const bool order_dead =
                    book_.find_order(maker_id) == nullptr &&
                    icebergs_.find(maker_id) == nullptr;
                if (maker_died && order_dead) {
                    l3_emit_ev(static_cast<uint8_t>(L3Kind::Cancel),
                               maker_id, maker_acct, maker_side,
                               instrument_id_of(OrderAux{}), maker_price, 0,
                               /*qty*/ 0, /*delta*/ -eff_rem, 0,
                               kL3RoleNone, maker_flags,
                               kWalCancelReasonStp, stp_wseq);
                } else {
                    l3_emit_ev(static_cast<uint8_t>(L3Kind::Modify),
                               maker_id, maker_acct, maker_side,
                               instrument_id_of(OrderAux{}), maker_price, 0,
                               /*qty*/ eff_rem - dec, /*delta*/ -dec, 0,
                               kL3RoleNone, maker_flags, 0, stp_wseq);
                }
            }
            if (book_.find_order(maker_id) == nullptr &&
                icebergs_.find(maker_id) == nullptr) {
                // Maker fully dead under STP (non-fill terminal) — release
                // its OCO pair; a live sibling continues standalone.
                oco_on_dead(maker_id, /*by_fill=*/false);
            }
            const int64_t taker_prevented = taker_rem - dec;
            note_prevented(taker.id, taker_prevented);
            taker_rem -= dec;
            // TRANSFER (mutual kOrderFlagStpTransfer, resolved by the guard):
            // identical DECREMENT mechanics; distinct accounts inside one
            // group additionally journal PREVENTED_MATCH (spec §24 #279-280).
            // Same-account TRANSFER is a plain decrement — no audit event.
            if (action == StpAction::TRANSFER &&
                maker_acct != taker.account_id) {
                // maker node may be pool-freed here (slice fully consumed)
                // — pass the scalars captured pre-mutation.
                emit_prevented_match(maker_id, maker_acct,
                                     group_of(maker_id), taker, taker_group,
                                     level_price_ticks, dec, taker_prevented);
            }
            res.dead = true;  // taker remainder cancelled (spec §6.5 literal)
            res.dead_reason = kWalCancelReasonStp;
            res.dead_code = kRejectStpCancelled;
            return true;
        }
    }
    return false;
}

// ---------------------------------------------------------------------------
// the price-time sweep
// ---------------------------------------------------------------------------

MatchingEngine::TakerResult MatchingEngine::walk_match(
    Order& taker, const OrderAux& aux, bool has_limit,
    int64_t limit_ticks, const CollarBounds& cb) noexcept {
    TakerResult res{remaining_qty_units(taker), false, 0, nullptr};
    const Side opp = taker.side == Side::BUY ? Side::SELL : Side::BUY;
    const uint32_t instr = instrument_id_of(aux);

    while (res.remaining > 0 && !res.dead && !wal_fault_) {
        const PriceLevel* lvl = opp == Side::SELL ? book_.best_ask()
                                                  : book_.best_bid();
        if (lvl == nullptr) break;
        // Phase-16 Task 16.3.13 — hidden/dark makers print at the
        // visible-BBO midpoint rather than the level price. An
        // unmatchable hidden member (no mid, mid beyond the taker limit,
        // or collar-blocked) yields its slot to the next member of the
        // level — visible members behind it still fill at the level.
        // When no member of the best level is fillable the sweep halts:
        // mid is bounded by the visible BBO, so no deeper visible level
        // can cross either.
        Order* maker = lvl->head;
        int64_t eff_px = lvl->price_ticks;
        while (maker != nullptr) {
            if ((maker->flags & kOrderFlagHidden) != 0) {
                const int64_t m2 = visible_midpoint();
                if (m2 > 0 &&
                    (!has_limit ||
                     crosses(taker.side, limit_ticks, m2)) &&
                    ExecutionCollar::price_allowed(cb, m2)) {
                    eff_px = m2;
                    break;
                }
                maker = maker->next;
                continue;
            }
            break;  // visible/pegged maker fills at the level price
        }
        if (maker == nullptr) break;  // level unfillable — sweep halts
        if (has_limit &&
            !crosses(taker.side, limit_ticks, eff_px)) {
            break;
        }
        // §22.2 execution collar (Task 2.3.17): the phase-entry CollarBounds
        // snapshot gates every maker — the first out-of-range level stops
        // the sweep and the remainder expires with the persisted reason.
        // The effective price gates (midpoint for hidden makers).
        if (!ExecutionCollar::price_allowed(cb, eff_px)) {
            res.dead = true;
            res.dead_reason = kWalCancelReasonExecRuleRange;
            res.dead_code = ExecutionCollar::kExpiryReason;
            break;
        }

        // Self-trade prevention (Tasks 2.3.11/2.3.16/2.3.18): same account,
        // or same nonzero STP group (the maker's group resolves through the
        // meta index). TRANSFER is the mutual-flag exception resolved by the
        // guard; NONE resolves to PROCEED (admission-gated upstream).
        const uint32_t maker_group = group_of(maker->id);
        const bool self =
            maker->account_id == taker.account_id ||
            (aux.trade_group_id != 0 && aux.trade_group_id == maker_group);
        bool self_fill = false;  // set when PROCEED lets a self-pair fill
        if (self) {
            const StpAction a = SelfTradeGuard::action(
                taker.stp_mode,
                (taker.flags & kOrderFlagStpTransfer) != 0,
                (maker->flags & kOrderFlagStpTransfer) != 0);
            if (a != StpAction::PROCEED) {
                (void)apply_stp(taker, aux.trade_group_id, maker,
                                lvl->price_ticks, a, res.remaining, res);
                continue;  // re-read best level — maker may be gone
            }
            self_fill = true;
        }

        const int64_t m_rem = remaining_qty_units(*maker);
        const int64_t fill =
            res.remaining < m_rem ? res.remaining : m_rem;
        const int64_t px = eff_px;  // midpoint for hidden makers (16.3.13)
        const uint64_t maker_id = maker->id;
        const uint64_t maker_acct = maker->account_id;
        const uint64_t tid = tid_stream()++;
        const uint64_t buy_id =
            taker.side == Side::BUY ? taker.id : maker_id;
        const uint64_t sell_id =
            taker.side == Side::BUY ? maker_id : taker.id;
        // L3 pre-mutation capture — apply_fill may free the maker node.
        // Order-level remaining for an iceberg maker is the hidden reserve.
        const Side maker_side = maker->side;
        const uint8_t maker_flags =
            l3_ != nullptr ? l3_flags_of(*maker) : 0;
        const int64_t maker_rem_pre =
            l3_ != nullptr ? effective_remaining_units(maker) : 0;

        // WAL BEFORE mutation; publish after. The committed seq is
        // captured pre-append so L3 Fill events correlate exactly.
        const uint64_t wseq = journal_seq();
        if (wal_ != nullptr &&
            wal_->write_trade(tid, buy_id, sell_id, instr, px, fill,
                              now_ns_) != WalStatus::Ok) {
            wal_fault_ = true;
            break;
        }
        const BookError e = book_.apply_fill(maker, fill);
        if (e != BookError::OK) {
            // Structural divergence (book rejected a WAL-journaled trade) —
            // fail closed; the journaled prefix stays replay-consistent.
            wal_fault_ = true;
            break;
        }
        taker.filled_qty_units += fill;
        res.remaining -= fill;
        last_price_ticks_ = px;
        ++trades_emitted_;
        // §6.6b #3 price improvement (Task 2.3.22): delta vs the taker's
        // OWN limit (not the walk bound — a discretionary band improves
        // against the nominal limit; MARKET takers pass limit 0 -> delta 0).
        improvement_.on_fill(taker.side, taker.id, tid,
                             has_limit ? taker.price_ticks : 0, px, fill);
        if (publisher_ != nullptr) {
            (void)publisher_->publish_trade(tid, buy_id, sell_id, px, fill,
                                      book_.book_seq(), now_ns_);
        }
        // L3 Fill — both legs of the same TRADE row share wal_seq:
        // taker post-fill remaining + maker post-fill remaining.
        l3_emit_ev(static_cast<uint8_t>(L3Kind::Fill), taker.id,
                   taker.account_id, taker.side, instr, px, /*ref*/ 0,
                   remaining_qty_units(taker), /*delta*/ -fill, tid,
                   kL3RoleTaker, l3_ != nullptr ? l3_flags_of(taker) : 0,
                   0, wseq);
        l3_emit_ev(static_cast<uint8_t>(L3Kind::Fill), maker_id, maker_acct,
                   maker_side, instr, px, /*ref*/ 0,
                   maker_rem_pre - fill, /*delta*/ -fill, tid,
                   kL3RoleMaker, maker_flags, 0, wseq);

        // Task 2.3.16 surveillance flag (§24 #274): the pair only reached
        // the fill path because resolved stp_mode == NONE — flag every such
        // fill to the SELF_TRADE sink (Phase-17 wash-trade monitoring).
        if (self_fill && self_trade_fn_ != nullptr) {
            const SelfTradeEvent ev{tid, taker.id, maker_id,
                                    taker.account_id, maker_acct,
                                    maker_group, instr, px, fill, now_ns_};
            self_trade_fn_(self_trade_ctx_, ev);
        }

        // Iceberg maker: account the fill against the record; when the
        // visible slice died, refresh the next slice at the level tail.
        if (auto* rec = icebergs_.find(maker_id)) {
            rec->filled_total_units += fill;
            if (book_.find_order(maker_id) == nullptr) {
                replenish_iceberg(rec);
            }
        }
        // Terminal makers release their meta slot — keeps the order-meta
        // table (and the Task 2.3.20 amend fence) free of dead ids so an
        // id cannot linger past its last fill.
        if (book_.find_order(maker_id) == nullptr) {
            meta_erase(maker_id);
            // Phase-14 Task 14.3.1 — maker reached terminal FILLED: OCO
            // winner arm. An iceberg maker whose slice died but whose
            // hidden remainder replenished is still live — the trigger
            // fires only when nothing of the order remains (record gone).
            if (icebergs_.find(maker_id) == nullptr) {
                oco_on_dead(maker_id, /*by_fill=*/true);
            }
        }
    }
    return res;
}

// ---------------------------------------------------------------------------
// iceberg refresh — the new slice re-enters at the FIFO tail of its price
// level with a fresh non-decreasing timestamp (refresh loses priority)
// ---------------------------------------------------------------------------

void MatchingEngine::replenish_iceberg(IcebergManager::Record* rec) noexcept {
    if (rec == nullptr) return;
    const uint64_t rid = rec->order_id;
    const uint64_t account = rec->tmpl.account_id;
    const int64_t next = IcebergManager::next_slice(*rec);
    if (next <= 0) {
        icebergs_.erase(rid);
        meta_erase(rid);
        return;
    }
    Order tmpl = rec->tmpl;
    tmpl.qty_units = next;
    tmpl.filled_qty_units = 0;
    tmpl.display_qty_units = rec->display_qty_units;
    // Fresh priority key strictly greater than the level tail's — the level
    // chain is now ordered by (timestamp_ns, ingress_seq) explicitly
    // (Task 2.3.20), so "lands at the FIFO tail" requires exceeding the
    // tail's whole key, not just its timestamp.
    uint64_t tail_ts = 0;
    uint64_t tail_seq = 0;
    for (uint32_t d = 0; ; ++d) {
        const PriceLevel* l = book_.level(tmpl.side, d);
        if (l == nullptr) break;
        if (l->price_ticks == tmpl.price_ticks && l->tail != nullptr) {
            tail_ts = l->tail->timestamp_ns;
            tail_seq = l->tail->ingress_seq;
            break;
        }
    }
    uint64_t ts = now_ns_ > tmpl.timestamp_ns ? now_ns_ : tmpl.timestamp_ns;
    if (tail_ts > ts) ts = tail_ts;
    tmpl.timestamp_ns = ts;
    // (ts,seq) must exceed the tail's key: when ts ties, seq > tail_seq is
    // what puts the refreshed slice behind every survivor.
    const uint64_t fresh = ++emit_seq_;
    tmpl.ingress_seq = fresh > tail_seq ? fresh : tail_seq + 1;

    Order* node = nullptr;
    const BookError e = book_.add_order(tmpl, &node);
    if (e != BookError::OK) {
        // Fail-closed: the hidden remainder cannot rest -> terminal cancel.
        // Journal + L3 emit precede the teardown so the iceberg record is
        // still resolvable for order-level remaining-qty accounting.
        emit_cancel_event(rid, account, kWalCancelReasonUser, &rec->tmpl);
        icebergs_.erase(rid);
        meta_erase(rid);
        last_reject_ = kRejectBookCapacity;
        ++reject_count_;
    }
    // No WAL entry — the refresh re-derives from logged TRADE/ORDER_NEW.
}

// ---------------------------------------------------------------------------
// FOK feasibility — mirrors walk_match exactly, zero mutation
// ---------------------------------------------------------------------------

bool MatchingEngine::fok_feasible(const Order& taker, uint32_t taker_group,
                                  bool has_limit, int64_t limit_ticks,
                                  const CollarBounds& cb) const noexcept {
    int64_t rem = remaining_qty_units(taker);
    const Side opp = taker.side == Side::BUY ? Side::SELL : Side::BUY;
    int64_t mid = -1;  // lazy visible-midpoint cache (16.3.13)
    for (std::size_t d = 0; rem > 0; ++d) {
        const PriceLevel* lvl = book_.level(opp, d);
        if (lvl == nullptr) break;
        if (has_limit &&
            !crosses(taker.side, limit_ticks, lvl->price_ticks)) {
            break;
        }
        for (const Order* m = lvl->head; m != nullptr && rem > 0;
             m = m->next) {
            // Phase-16 Task 16.3.13 — hidden makers fill at the visible
            // midpoint: contributes nothing when the mid is absent or
            // beyond the taker limit; other makers still count.
            if ((m->flags & kOrderFlagHidden) != 0) {
                if (mid < 0) mid = visible_midpoint();
                if (mid <= 0) continue;
                if (has_limit && !crosses(taker.side, limit_ticks, mid)) {
                    continue;
                }
                if (!ExecutionCollar::price_allowed(cb, mid)) {
                    return false;
                }
            } else if (!ExecutionCollar::price_allowed(cb,
                                                     lvl->price_ticks)) {
                // §22.2 collar mirror (Task 2.3.17): the feasibility
                // verdict must agree with the sweep — an out-of-range
                // maker makes FOK infeasible.
                return false;
            }
            const bool self =
                m->account_id == taker.account_id ||
                (taker_group != 0 && taker_group == group_of(m->id));
            if (self) {
                const StpAction a = SelfTradeGuard::action(
                    taker.stp_mode,
                    (taker.flags & kOrderFlagStpTransfer) != 0,
                    (m->flags & kOrderFlagStpTransfer) != 0);
                if (a == StpAction::CANCEL_MAKER) continue;  // maker skipped
                // DECREMENT/TRANSFER cancel the taker remainder too — FOK
                // cannot fill -> infeasible, same as CANCEL_*.
                if (a != StpAction::PROCEED) return false;   // taker dies
            }
            int64_t avail = remaining_qty_units(*m);
            if (const auto* rec = icebergs_.find(m->id)) {
                // Refreshed slices land behind the rest of this level but
                // stay reachable at the same price — count the whole
                // unfilled iceberg remainder.
                avail = rec->total_qty_units - rec->filled_total_units;
            }
            rem -= rem < avail ? rem : avail;
        }
    }
    return rem == 0;
}

// ---------------------------------------------------------------------------
// taker remainder disposition + resting
// ---------------------------------------------------------------------------

void MatchingEngine::finish_taker(Order& taker, const OrderAux& aux,
                                  TakerResult r, int64_t display) noexcept {
    if (wal_fault_) {
        // Engine halted — a remainder cannot rest unjournaled.
        emit_cancel_event(taker.id, taker.account_id, kWalCancelReasonUser, &taker);
        last_reject_ = kRejectBookCapacity;
        ++reject_count_;
        meta_erase(taker.id);  // a prevented-qty slot may exist (2.3.18)
        return;
    }
    if (r.dead) {
        emit_cancel_event(taker.id, taker.account_id, r.dead_reason, &taker);
        if (r.dead_code != nullptr) {
            last_reject_ = r.dead_code;
            ++reject_count_;
        }
        meta_erase(taker.id);  // terminal — drop the prevented-qty slot
        return;
    }
    if (r.remaining <= 0) {
        meta_erase(taker.id);  // filled taker rests nothing — no meta to keep
        // Phase-14 Task 14.3.1 — taker reached terminal FILLED: OCO winner
        // arm; the sibling leg is cancelled atomically on this thread
        // (journaled reason-7 ORDER_CANCEL) or doomed if still in flight.
        oco_on_dead(taker.id, /*by_fill=*/true);
        return;
    }

    const bool never_rests = taker.type == OrderType::MARKET ||
                             taker.type == OrderType::STOP ||
                             taker.type == OrderType::TRAILING_STOP ||
                             taker.type == OrderType::MOO ||
                             taker.type == OrderType::MOC ||
                             taker.tif == TimeInForce::IOC ||
                             taker.tif == TimeInForce::FOK;
    if (never_rests) {
        emit_cancel_event(
            taker.id, taker.account_id,
            taker.tif == TimeInForce::FOK ? kWalCancelReasonFokUnfilled
                                        : kWalCancelReasonIocRemainder, &taker);
        if (taker.tif == TimeInForce::FOK) {
            last_reject_ = kRejectFokUnfilled;
            ++reject_count_;
        }
        meta_erase(taker.id);  // terminal — drop the prevented-qty slot
        return;
    }
    rest_remainder(taker, aux, display);
}

void MatchingEngine::rest_remainder(Order& taker, const OrderAux& aux,
                                    int64_t display) noexcept {
    const int64_t rem = remaining_qty_units(taker);
    if (rem <= 0) return;
    Order tmpl = taker;  // node qty stays the ORIGINAL total (book keeps
                         // qty_units - filled_qty_units as the live rest)

    IcebergManager::Record* rec = nullptr;
    if (taker.type == OrderType::ICEBERG) {
        rec = icebergs_.register_order(tmpl, taker.filled_qty_units, display);
        if (rec == nullptr) {
            emit_cancel_event(taker.id, taker.account_id,
                              kWalCancelReasonUser, &taker);
            last_reject_ = kRejectBookCapacity;
            ++reject_count_;
            meta_erase(taker.id);
            return;
        }
        // The book node carries the visible slice only.
        tmpl.qty_units = display < rem ? display : rem;
        tmpl.filled_qty_units = 0;
    }

    Order* node = nullptr;
    const BookError e = book_.add_order(tmpl, &node);
    if (e != BookError::OK) {
        if (rec != nullptr) icebergs_.erase(taker.id);
        emit_cancel_event(taker.id, taker.account_id, kWalCancelReasonUser, &taker);
        last_reject_ = e == BookError::DUPLICATE_ID ? kRejectDuplicateId
                                                  : kRejectBookCapacity;
        ++reject_count_;
        meta_erase(taker.id);
        return;
    }

    // Per-order aux tracking (STP group / GTD-DAY expiry) — fail-closed: if
    // tracking cannot be allocated the rest is undone and the order dies.
    if (!track_meta(taker.id, aux)) {
        (void)book_.cancel_order(taker.id);
        if (rec != nullptr) icebergs_.erase(taker.id);
        emit_cancel_event(taker.id, taker.account_id, kWalCancelReasonUser, &taker);
        last_reject_ = kRejectBookCapacity;
        ++reject_count_;
        meta_erase(taker.id);
    }
}

// ---------------------------------------------------------------------------
// stop queue drain — triggered entries process in pop order, then re-poll
// (their own fills may trigger further stops)
// ---------------------------------------------------------------------------

// ---------------------------------------------------------------------------
// Phase-16 conditional settle wave (Tasks 16.3.3/11/13/15/16/17/22)
// ---------------------------------------------------------------------------

StopOrderTrigger::TriggerRefs MatchingEngine::trigger_refs() noexcept {
    StopOrderTrigger::TriggerRefs refs{};
    refs.last = last_price_ticks_;
    // Mark/index resolve through the bound feed; the local seam serves the
    // journal-free replay engine and unit tests. A stale stamp zeroes its
    // own source only — LAST stays live.
    int64_t mark = 0, mark_ts = 0, index = 0, index_ts = 0;
    bool ok = false;
    if (oracle_feed_ != nullptr) {
        const PriceOracleFeed::Snapshot s = oracle_feed_->snapshot();
        ok = s.verifiable;
        mark = s.mark_ticks;  mark_ts = s.mark_ts_ns;
        index = s.index_ticks; index_ts = s.index_ts_ns;
    } else if (oracle_local_ok_) {
        ok = true;
        mark = oracle_mark_ticks_;  mark_ts = oracle_mark_ts_ns_;
        index = oracle_index_ticks_; index_ts = oracle_index_ts_ns_;
    }
    if (ok) {
        const uint64_t stale = kOracleStaleNs;
        if (mark > 0 && (mark_ts == 0 ||
                         now_ns_ - static_cast<uint64_t>(mark_ts) <= stale)) {
            refs.mark = mark;
        }
        if (index > 0 && (index_ts == 0 ||
                          now_ns_ - static_cast<uint64_t>(index_ts) <= stale)) {
            refs.index = index;
        }
    }
    // Staleness accounting (CONDITIONAL_TRIGGER_ORACLE_STALE freeze
    // observations — spec §6.2a): only count a source as stale when a live
    // conditional actually waits on it.
    if (stops_.source_live(kTriggerSourceMark) > 0 && refs.mark <= 0 &&
        oracle_feed_ != nullptr && mark > 0) {
        ++oracle_stale_suspensions_;
    }
    if (stops_.source_live(kTriggerSourceIndex) > 0 && refs.index <= 0 &&
        oracle_feed_ != nullptr && index > 0) {
        ++oracle_stale_suspensions_;
    }
    return refs;
}

void MatchingEngine::set_oracle_snapshot(int64_t mark_ticks,
                                         int64_t mark_ts_ns,
                                         int64_t index_ticks,
                                         int64_t index_ts_ns,
                                         bool verifiable) noexcept {
    oracle_mark_ticks_ = mark_ticks;
    oracle_mark_ts_ns_ = mark_ts_ns;
    oracle_index_ticks_ = index_ticks;
    oracle_index_ts_ns_ = index_ts_ns;
    oracle_local_ok_ = verifiable;
}

void MatchingEngine::trail_eval(
    const StopOrderTrigger::TriggerRefs& refs) noexcept {
    const Instrument* inst = book_.instrument();
    if (inst == nullptr) return;  // no pip lattice — cannot re-anchor
    stops_.for_each_trailing([&](Order* o, StopOrderTrigger::Pending* p) {
        const int64_t ref = refs.ref(p->trigger_source);
        if (ref <= 0) return;  // stale/missing source — frozen, stays pending
        if (p->armed == 0) {
            // Activation gate: the trail watches the reference only once
            // it prints past the activation price (SELL trails arm on a
            // rally to the gate; BUY trails arm on a dip).
            const bool active =
                p->activation_price_ticks <= 0 ||
                (o->side == Side::BUY
                     ? ref <= p->activation_price_ticks
                     : ref >= p->activation_price_ticks);
            if (!active) return;
            p->armed = 1;
            p->anchor_ticks = ref;
        } else if (p->anchor_ticks <= 0) {
            // Deferred arming (LAST with no print at admission): the first
            // live reference seeds the anchor. A seeded explicit stop
            // stands until the first FAVORABLE move recomputes; a stop of
            // 0 derives immediately.
            p->anchor_ticks = ref;
            if (p->stop_price_ticks > 0) return;
        } else {
            // Re-anchor strictly on favorable moves — SELL trails ratchet
            // UP with new highs, BUY trails ratchet DOWN with new lows.
            const bool favorable =
                o->side == Side::SELL ? ref > p->anchor_ticks
                                      : ref < p->anchor_ticks;
            if (!favorable) return;  // adverse move — the stop stands
            p->anchor_ticks = ref;
        }
        const int64_t stop = trail_stop_from_anchor(
            o->side == Side::BUY, p->anchor_ticks, p->trail_distance,
            p->trail_unit, *inst);
        if (stop <= 0 || stop == p->stop_price_ticks) return;
        p->stop_price_ticks = stop;
        stops_.resort(o);
        if (p->gslo_notional_units > 0) gslo_resync(*p);
    });
}

void MatchingEngine::journal_triggered(
    const Order* node, const StopOrderTrigger::TriggeredInfo& info,
    const OrderAux& aux) noexcept {
    (void)journal_seq();   // consume the trigger row's seq (no L3 side)
    if (wal_ == nullptr) return;
    WalOrderTriggeredPayload p{};
    p.order_id = node->id;
    p.account_id = node->account_id;
    p.instrument_id = instrument_id_of(aux);
    p.trigger_source = info.source;
    p.order_kind = wal_order_type_wire(node->type);
    p.stop_price_ticks = info.stop_ticks;
    p.observed_price_ticks = info.observed_ticks;
    if (wal_->write_order_triggered(p, now_ns_) != WalStatus::Ok) {
        wal_fault_ = true;
    }
}

void MatchingEngine::process_triggered_info(
    Order* node, const OrderAux& aux,
    const StopOrderTrigger::TriggeredInfo& info) noexcept {
    if ((node->flags & kOrderFlagGslo) != 0) {
        // Task 16.3.16: a flagged trigger is a guaranteed fill at the armed
        // stop — no collar check, no sweep, no remainder. The exposure the
        // slot reserved is released with the fill.
        gslo_fill(node, aux, info.stop_ticks);
        gslo_release(info.gslo_notional_units);
        const uint64_t nid = node->id;
        orders_.free(node);
        meta_erase(nid);
        return;
    }
    process_triggered(node, aux);
}

int64_t MatchingEngine::gslo_notional(const Order& o,
                                      int64_t stop_ticks) const noexcept {
    int64_t n = 0;
    if (!notional_units(remaining_qty_units(o), stop_ticks, n)) {
        return INT64_MAX;  // saturating — an overflowing estimate can only
                           // trip the exposure cap, never undercount it
    }
    return n;
}

void MatchingEngine::gslo_resync(StopOrderTrigger::Pending& p) noexcept {
    if (p.order == nullptr) return;
    const int64_t n = gslo_notional(*p.order, p.stop_price_ticks);
    gslo_notional_units_ += n - p.gslo_notional_units;
    p.gslo_notional_units = n;
}

void MatchingEngine::gslo_release(int64_t notional_units) noexcept {
    gslo_notional_units_ -= notional_units;
    if (gslo_live_count_ > 0) --gslo_live_count_;
}

void MatchingEngine::gslo_fill(Order* node, const OrderAux& aux,
                               int64_t stop_ticks) noexcept {
    const int64_t fill = remaining_qty_units(*node);
    if (fill <= 0) return;
    const uint64_t tid = tid_stream()++;
    // Venue leg = the synthetic counterparty id — the gap liability moves
    // to the exposure pool, never against a real order id.
    const uint64_t buy_id =
        node->side == Side::BUY ? node->id : kGsloVenueOrderId;
    const uint64_t sell_id =
        node->side == Side::BUY ? kGsloVenueOrderId : node->id;
    const uint64_t wseq = journal_seq();
    if (wal_ != nullptr &&
        wal_->write_trade(tid, buy_id, sell_id, instrument_id_of(aux),
                          stop_ticks, fill, now_ns_) != WalStatus::Ok) {
        wal_fault_ = true;
        return;
    }
    node->filled_qty_units += fill;
    // Off-book mutation: gslo fills settle a pending trigger outside the
    // resting book, so apply_fill's seq bump never runs — advance the
    // mutation counter explicitly or the published fill seq collides with
    // the previous mutation's (Phase-09 swap-drill finding).
    book_.bump_book_seq();
    last_price_ticks_ = stop_ticks;
    ++trades_emitted_;
    ++gslo_fills_;
    if (publisher_ != nullptr) {
        (void)publisher_->publish_trade(tid, buy_id, sell_id, stop_ticks,
                                        fill, book_.book_seq(), now_ns_);
    }
    // L3 Fill — the guaranteed stop fills the taker leg in full; the venue
    // leg is a synthetic counterparty (flags bit2), never a client order.
    l3_emit_ev(static_cast<uint8_t>(L3Kind::Fill), node->id,
               node->account_id, node->side, instrument_id_of(aux),
               stop_ticks, /*ref*/ 0, /*qty*/ 0, /*delta*/ -fill, tid,
               kL3RoleTaker, l3_flags_of(*node), 0, wseq);
    l3_emit_ev(static_cast<uint8_t>(L3Kind::Fill), kGsloVenueOrderId,
               /*account_id*/ 0,
               node->side == Side::BUY ? Side::SELL : Side::BUY,
               instrument_id_of(aux), stop_ticks, /*ref*/ 0, /*qty*/ 0,
               /*delta*/ -fill, tid, kL3RoleMaker,
               kL3FlagSynthetic | kL3FlagDetail, 0, wseq);
    oco_on_dead(node->id, /*by_fill=*/true);
}

// --- pegged-order table (Task 16.3.11/16.3.22) --------------------------------

MatchingEngine::PegRec* MatchingEngine::peg_find(uint64_t order_id) noexcept {
    for (std::size_t i = 0; i < pegs_live_; ++i) {
        if (pegs_[i].order_id == order_id) return &pegs_[i];
    }
    return nullptr;
}

const MatchingEngine::PegRec* MatchingEngine::peg_find(
    uint64_t order_id) const noexcept {
    for (std::size_t i = 0; i < pegs_live_; ++i) {
        if (pegs_[i].order_id == order_id) return &pegs_[i];
    }
    return nullptr;
}

void MatchingEngine::peg_erase(uint64_t order_id) noexcept {
    for (std::size_t i = 0; i < pegs_live_; ++i) {
        if (pegs_[i].order_id != order_id) continue;
        pegs_[i] = pegs_[pegs_live_ - 1];
        pegs_[--pegs_live_].order_id = 0;
        return;
    }
}

int64_t MatchingEngine::visible_best_price(Side s) const noexcept {
    for (uint32_t d = 0; d < OrderBook::kMaxLevels; ++d) {
        const PriceLevel* l = book_.level(s, d);
        if (l == nullptr) break;
        // A level whose whole depth is pegged/hidden carries no PUBLIC
        // liquidity — the peg reference tracks the visible BBO only.
        for (const Order* o = l->head; o != nullptr; o = o->next) {
            if (l2_visible(*o) && remaining_qty_units(*o) > 0) {
                return l->price_ticks;
            }
        }
    }
    return 0;
}

int64_t MatchingEngine::visible_midpoint() const noexcept {
    const int64_t b = visible_best_price(Side::BUY);
    const int64_t a = visible_best_price(Side::SELL);
    if (b <= 0 || a <= 0) return 0;
    return b + (a - b) / 2;  // overflow-free midpoint; crossed book still
                             // resolves (a < b -> mid inside the cross)
}

bool MatchingEngine::peg_reference(uint8_t mode, Side side,
                                   int64_t& ref_ticks) const noexcept {
    switch (mode) {
    case kPegMid:
        ref_ticks = visible_midpoint();
        break;
    case kPegPrimary:
        ref_ticks = visible_best_price(side);
        break;
    case kPegMarket: {
        const Side opp = side == Side::BUY ? Side::SELL : Side::BUY;
        ref_ticks = visible_best_price(opp);
        break;
    }
    default:
        ref_ticks = 0;
        break;
    }
    return ref_ticks > 0;
}

bool MatchingEngine::peg_target(const PegRec& rec, Side side,
                                int64_t& price_ticks,
                                int64_t& ref_ticks) const noexcept {
    if (!peg_reference(rec.mode, side, ref_ticks)) return false;
    const safe_math::int128_t t =
        safe_math::int128_t{ref_ticks} +
        (side == Side::BUY ? rec.offset_ticks : -rec.offset_ticks);
    int64_t p = 0;
    if (!safe_math::try_narrow_i128(t, p) || p <= 0) return false;
    if (rec.limit_ticks > 0) {
        // Collar: the pegged price never crosses the configured limit.
        if (side == Side::BUY ? p > rec.limit_ticks
                              : p < rec.limit_ticks) {
            p = rec.limit_ticks;
        }
    }
    price_ticks = p;
    return true;
}

bool MatchingEngine::repeg_pass() noexcept {
    bool any = false;
    for (std::size_t i = 0; i < pegs_live_; ++i) {
        if (pegs_[i].order_id == 0) continue;
        Order* o = book_.find_order(pegs_[i].order_id);
        if (o == nullptr) {
            // Terminal already (fill/cancel) — prune lazily; single
            // engine thread owns the table so swap-remove is safe inline.
            pegs_[i] = pegs_[pegs_live_ - 1];
            pegs_[--pegs_live_].order_id = 0;
            --i;
            continue;
        }
        PegRec& rec = pegs_[i];
        int64_t target = 0, ref = 0;
        if (!peg_target(rec, o->side, target, ref)) {
            if (rec.priced_ok != 0) {
                rec.priced_ok = 0;
                ++peg_unavailable_total_;  // §6.2a hold — the level stays
            }
            continue;
        }
        rec.priced_ok = 1;
        if (target == o->price_ticks) continue;
        // Journal BEFORE mutation: the committed price is the replay
        // divergence anchor (PEG_REPRICE row). The seq is consumed
        // unconditionally so journal-free replay counts identically.
        const uint64_t wseq = journal_seq();
        if (wal_ != nullptr) {
            WalPegRepricePayload wp{};
            wp.order_id = rec.order_id;
            wp.instrument_id = instrument_id_of(OrderAux{});
            wp.peg_mode = rec.mode;
            wp.old_price_ticks = o->price_ticks;
            wp.new_price_ticks = target;
            wp.ref_price_ticks = ref;
            if (wal_->write_peg_reprice(wp, now_ns_) != WalStatus::Ok) {
                wal_fault_ = true;
                return any;
            }
        }
        const uint64_t ts =
            now_ns_ > o->timestamp_ns ? now_ns_ : o->timestamp_ns;
        const uint64_t seq = ++emit_seq_;
        if (book_.modify_order(rec.order_id, target, o->qty_units, ts, seq,
                               /*force_requeue*/ true) != BookError::OK) {
            wal_fault_ = true;  // journaled reprice could not apply — halt
            return any;
        }
        ++peg_repriced_total_;
        // L3 MODIFY (Task 17.3.1): PEG_REPRICE surfaces as a price-change
        // modify — qty delta 0, price carries the new resting price,
        // ref_price carries the peg reference for auditability.
        l3_emit_ev(static_cast<uint8_t>(L3Kind::Modify), rec.order_id,
                   o->account_id, o->side, instrument_id_of(OrderAux{}),
                   target, /*ref_price*/ ref, remaining_qty_units(*o),
                   /*delta*/ 0, 0, kL3RoleNone, l3_flags_of(*o), 0, wseq);
        any = true;
    }
    return any;
}

void MatchingEngine::settle() noexcept {
    if (wal_fault_) return;  // unjournaled transitions must not commit
    // Bounded wave: trail re-anchor -> sourced trigger pop -> pegged
    // reprice -> repeat while progress was made (a triggered stop's own
    // fill can trigger further stops; a reprice can move a reference).
    constexpr int kMaxPasses = 8;
    const StopOrderTrigger::TriggerRefs refs = trigger_refs();
    for (int pass = 0; pass < kMaxPasses && !wal_fault_; ++pass) {
        trail_eval(refs);
        bool progress = false;
        for (;;) {
            Order* chain = stops_.pop_triggered(refs);
            if (chain == nullptr) break;
            progress = true;
            for (Order* n = chain; n != nullptr;) {
                Order* next = n->next;
                n->next = nullptr;
                // Rebuild aux from the meta index (group/expiry/source
                // tracked at enqueue) — pending stops carry no OrderAux.
                OrderAux aux{};
                if (const OrderMeta* m = meta_find(n->id)) {
                    aux.trade_group_id = m->trade_group_id;
                    aux.gtd_expiry_ns = m->expiry_ns;
                    aux.instrument_id = m->instrument_id;
                    aux.trigger_source = m->trigger_source;
                }
                StopOrderTrigger::TriggeredInfo info{};
                info.order_id = n->id;
                info.source = aux.trigger_source;
                (void)stops_.take_triggered(n->id, &info);
                journal_triggered(n, info, aux);
                process_triggered_info(n, aux, info);
                n = next;
            }
        }
        // Repricing is suspended inside an armed CALL — the accumulated
        // book's crossed BBO is not a meaningful reference; the uncross
        // settles prices, then the post-CALL settle resumes repricing.
        const bool repriced =
            auction_phase_ == kAuctionPhaseNone && repeg_pass();
        if (!progress && !repriced) break;
    }
}

void MatchingEngine::on_order_triggered_replay(
    const WalOrderTriggeredPayload& p) noexcept {
    if (wal_fault_) return;
    // LAST-sourced triggers re-derive inside the replayed settle wave —
    // the journaled row is informational there. MARK/INDEX rows are
    // authoritative: the feedless replay engine cannot re-evaluate the
    // oracle, so the committed row drives the activation.
    StopOrderTrigger::Pending* pend = stops_.find(p.order_id);
    if (pend == nullptr || pend->order == nullptr) return;
    if (pend->trigger_source == kTriggerSourceLast) return;
    Order* node = pend->order;
    const int64_t gslo_notional = pend->gslo_notional_units;
    OrderAux aux{};
    if (const OrderMeta* m = meta_find(node->id)) {
        aux.trade_group_id = m->trade_group_id;
        aux.gtd_expiry_ns = m->expiry_ns;
        aux.instrument_id = m->instrument_id;
        aux.trigger_source = m->trigger_source;
    }
    StopOrderTrigger::TriggeredInfo info{};
    info.order_id = p.order_id;
    info.stop_ticks = p.stop_price_ticks;
    info.observed_ticks = p.observed_price_ticks;
    info.gslo_notional_units = gslo_notional;
    info.source = pend->trigger_source;
    (void)stops_.remove(node->id);
    process_triggered_info(node, aux, info);
}

void MatchingEngine::process_triggered(Order* node,
                                       const OrderAux& aux) noexcept {
    // STOP -> market semantics (sweep to empty, remainder cancelled).
    // STOP_LIMIT -> limit at price_ticks; tif governs the remainder.
    const bool has_limit = node->type == OrderType::STOP_LIMIT;

    // §22.2 collar snapshot (Task 2.3.17) — the trigger instant is the
    // taker-phase entry, so bounds are taken now, not at queue time.
    const CollarBounds cb =
        collar_.begin_phase(node->side, collar_rule_, ref_price_ticks_,
                            ref_ts_ns_, now_ns_);
    // §6.6b trade-through re-check at activation (Task 2.3.22): a queued
    // stop re-enters the taker arms — STOP_LIMIT gates as a limit taker,
    // STOP clips as a market taker.
    if (tt_enabled_) {
        TtCheckInput tin;
        tin.order_id = node->id;
        tin.side = node->side;
        tin.type = node->type;
        tin.tif = node->tif;
        tin.limit_ticks = has_limit ? node->price_ticks : 0;
        tin.ts_ns = now_ns_;
        if (node->tif == TimeInForce::IOC ||
            node->tif == TimeInForce::FOK) {
            const ProtectedQuote q = trade_through_.quote();
            tin.liquidity_at_or_better_units =
                liquidity_at_or_better_units(
                    node->side,
                    node->side == Side::BUY ? q.ask_ticks : q.bid_ticks);
        }
        const TtVerdict v = trade_through_.check(tin);
        if (v.decision == TtDecision::REJECT) {
            emit_cancel_event(node->id, node->account_id,
                              kWalCancelReasonUser, node);
            last_reject_ = v.code;
            ++reject_count_;
            const uint64_t nid0 = node->id;
            orders_.free(node);
            if (book_.find_order(nid0) == nullptr) meta_erase(nid0);
            return;
        }
        if (v.decision == TtDecision::CLIP) {
            // Market-semantics clip: the protected quote becomes the sweep
            // bound; a remainder cancels SLIPPAGE_EXCEEDED (§6.6b #2) and
            // the guard emits MARKET_REMAINDER_SLIPPAGE for the TCA stream.
            TakerResult r = walk_match(*node, aux, true,
                                       v.effective_limit_ticks, cb);
            if (r.remaining > 0 && !r.dead && !wal_fault_) {
                trade_through_.on_remainder_cancelled(node->id, node->side,
                                                      r.remaining, now_ns_);
                emit_cancel_event(node->id, node->account_id,
                                  kWalCancelReasonSlippageExceeded, node);
                last_reject_ = kRejectSlippageExceeded;
                ++reject_count_;
            } else if (r.dead) {
                emit_cancel_event(node->id, node->account_id, r.dead_reason, node);
                if (r.dead_code != nullptr) {
                    last_reject_ = r.dead_code;
                    ++reject_count_;
                }
            }
            const uint64_t nid1 = node->id;
            orders_.free(node);
            meta_erase(nid1);  // STOPs never rest — terminal either way
            return;
        }
    }

    if (node->tif == TimeInForce::FOK &&
        !fok_feasible(*node, aux.trade_group_id, has_limit,
                      node->price_ticks, cb)) {
        emit_cancel_event(node->id, node->account_id,
                          kWalCancelReasonFokUnfilled, node);
        last_reject_ = kRejectFokUnfilled;
        ++reject_count_;
    } else {
        TakerResult r = walk_match(*node, aux, has_limit,
                                   node->price_ticks, cb);
        // Triggered stops take implied liquidity too — a STOP_LIMIT's
        // nominal price_ticks bounds the combo the same way it bounds
        // the outright sweep; a plain STOP sweeps unbounded (market
        // semantics — the collar snapshot still gates every leg fill
        // via the engine that owns the leg book).
        implied_take(*node, aux, r, has_limit,
                     has_limit ? node->price_ticks : 0);
        finish_taker(*node, aux, r, 0);
    }
    const uint64_t nid = node->id;
    // The pool node is scratch — the book/resting slice holds its own copy.
    orders_.free(node);
    // Meta stays only while something is still alive for this order id
    // (a resting STOP_LIMIT remainder); otherwise drop it.
    if (book_.find_order(nid) == nullptr) meta_erase(nid);
}

// ---------------------------------------------------------------------------
// ingress
// ---------------------------------------------------------------------------

void MatchingEngine::on_order_received(Order* order) noexcept {
    on_order_received(order, OrderAux{});
}

void MatchingEngine::on_order_received(Order* order,
                                       const OrderAux& aux) noexcept {
    ++received_count_;
    if (order == nullptr) return;
    const uint64_t seq0 = book_.book_seq();
    if (wal_fault_) {
        // Engine halted by a journal/structural fault — fail closed: reject
        // rather than mutating state the WAL can no longer reproduce.
        reject(order, kRejectBookCapacity);
        orders_.free(order);
        return;
    }
    // Phase-15 (Task 15.3.6): armed-key observation precedes every verdict
    // — a CALL armed since the last event reroutes this order into
    // accumulation, an EXTEND moves the deadline, a withdrawn key aborts
    // the auction. No-op when no feed is bound (replay/test harness).
    auction_control_sync();

    // --- validate (engine-level, fail-closed) ------------------------------
    const char* invalid = nullptr;
    if (order->id == 0 || order->qty_units <= 0 ||
        order->filled_qty_units != 0) {
        invalid = kRejectOrderInvalid;
    } else {
        switch (order->type) {
            case OrderType::LIMIT:
            case OrderType::ICEBERG:
                if (order->price_ticks <= 0) invalid = kRejectOrderInvalid;
                break;
            case OrderType::MARKET:
                break;
            case OrderType::STOP:
                if (aux.stop_price_ticks <= 0) invalid = kRejectOrderInvalid;
                break;
            case OrderType::STOP_LIMIT:
                if (order->price_ticks <= 0 || aux.stop_price_ticks <= 0) {
                    invalid = kRejectOrderInvalid;
                }
                break;
            case OrderType::PEG:
                // Task 16.3.11: mode must be a real peg; price_ticks is
                // ignored — the admission reference computes the level.
                if (aux.peg_mode == kPegNone || aux.peg_mode > kPegMarket ||
                    order->tif == TimeInForce::IOC ||
                    order->tif == TimeInForce::FOK) {
                    invalid = kRejectOrderInvalid;
                }
                break;
            case OrderType::TRAILING_STOP:
                // Tasks 16.3.3/16.3.15: a real distance unit + positive
                // distance are mandatory; stop_price_ticks is an optional
                // seed for the armed stop (anchor derives it otherwise).
                if (aux.trail_unit == kTrailUnitNone ||
                    aux.trail_unit > kTrailUnitAbsolute ||
                    aux.trail_distance <= 0) {
                    invalid = kRejectOrderInvalid;
                }
                break;
            case OrderType::MOO:
            case OrderType::MOC:
                // Task 16.3.25 (spec §6.2b): auction-scope orders exist
                // only inside an armed CALL — the order service holds
                // them RESERVED until the auction key arms (§27 decision:
                // pre-CALL queueing is gateway-side, never engine-side).
                if (auction_phase_ != kAuctionPhaseCall) {
                    invalid = kRejectOrderInvalid;
                }
                break;
            default:
                invalid = kRejectOrderInvalid;  // type unsupported here
        }
    }
    // Phase-16 cross-field admission checks (fail closed, pre-WAL).
    if (invalid == nullptr) {
        if (aux.trigger_source > kTriggerSourceIndex) {
            invalid = kRejectOrderInvalid;
        } else if ((order->flags & kOrderFlagGslo) != 0 &&
                   order->type != OrderType::STOP &&
                   order->type != OrderType::STOP_LIMIT &&
                   order->type != OrderType::TRAILING_STOP) {
            invalid = kRejectOrderInvalid;  // GSLO is a conditional flag
        } else if ((order->flags & kOrderFlagHidden) != 0 &&
                   order->type != OrderType::LIMIT &&
                   order->type != OrderType::ICEBERG) {
            invalid = kRejectOrderInvalid;  // hidden = resting order only
        }
    }
    if (invalid == nullptr &&
        (order->tif == TimeInForce::GTD || order->tif == TimeInForce::DAY) &&
        aux.gtd_expiry_ns <= 0) {
        // DAY orders arrive with their session close precomputed into
        // aux.gtd_expiry_ns by the gateway (deterministic engine clock).
        invalid = kRejectOrderInvalid;
    }
    // §6.11 discretionary offset intake validation (Task 2.3.26): a
    // malformed offset is an input rejection — precedes the WAL append
    // exactly like the other ORDER_INVALID-family checks. An instrument-
    // less book cannot price the band -> fail closed.
    if (invalid == nullptr && aux.discretionary_offset_pips != 0) {
        const Instrument* ins = book_.instrument();
        invalid = ins == nullptr
                      ? DiscretionaryExecutor::kRejectOffsetInvalid
                      : DiscretionaryExecutor::validate(
                            *order, aux.discretionary_offset_pips, *ins);
    }

    // Phase-16 Task 16.3.11/16.3.22 — pegged admission pricing (pre-WAL):
    // the admission target derives from the CURRENT visible reference;
    // an unpriceable peg rests at its collar when configured, else
    // rejects PEGGED_PRICING_UNAVAILABLE (fail closed — a peg with no
    // price is meaningless). Pegged orders are L2-hidden by l2_visible().
    int64_t peg_admission_price = 0;
    if (invalid == nullptr && order->type == OrderType::PEG) {
        const PegRec probe{order->id, aux.peg_offset_ticks,
                           aux.peg_limit_ticks, aux.peg_mode, 1};
        int64_t ref = 0;
        if (peg_target(probe, order->side, peg_admission_price, ref)) {
            // computed target (collar-clamped inside peg_target)
        } else if (aux.peg_limit_ticks > 0) {
            peg_admission_price = aux.peg_limit_ticks;
        } else {
            invalid = kRejectPeggedPricingUnavailable;
        }
        if (invalid == nullptr && (pegs_ == nullptr || pegs_live_ >= pegs_cap_)) {
            invalid = kRejectBookCapacity;
        }
        if (invalid == nullptr) order->price_ticks = peg_admission_price;
    }
    // Task 16.3.16 — GSLO exposure cap at admission (armed-notional at the
    // effective stop; 0-capped engines run uncapped). The cap is checked
    // here so a breach rejects without journaling.
    if (invalid == nullptr && (order->flags & kOrderFlagGslo) != 0 &&
        gslo_max_exposure_units_ > 0) {
        const int64_t n = gslo_notional(*order, aux.stop_price_ticks > 0
                                        ? aux.stop_price_ticks
                                        : order->price_ticks);
        if (gslo_notional_units_ + n > gslo_max_exposure_units_) {
            invalid = kRejectGsloExposureLimit;
        }
    }

    if (invalid != nullptr) {
        reject(order, invalid);
        orders_.free(order);
        return;
    }

    // Phase-14 Task 14.3.1 — OCO doomed-leg gate (spec §6.5/§6.8, §24 #47):
    // the sibling leg already reached terminal FILLED — its fill is
    // sequenced before this OrderNew on the ring — so this order can never
    // trade. Rejected OCO_SIBLING_CANCEL_RACE; the emitted reason-7
    // ORDER_CANCEL is journaled + published so the order-service sees the
    // terminal state for the leg it persisted. Deterministic on replay:
    // the restored doomed mark reproduces the identical verdict, and a
    // doomed leg's ORDER_NEW is never journaled (rejects aren't WAL rows).
    if (const OcoMember* om = oco_find(order->id)) {
        if (om->state == kOcoDoomed) {
            emit_cancel_event(order->id, order->account_id,
                              kWalCancelReasonOcoLink, order);
            last_reject_ = kRejectOcoSiblingRace;
            ++reject_count_;
            orders_.free(order);
            return;
        }
        // Defense-in-depth: a linked leg for a different instrument than
        // its link is malformed input (the gateway validates pairs before
        // dispatch; the link is still released to keep the table clean).
        if (om->instrument_id != 0 && aux.instrument_id != 0 &&
            om->instrument_id != aux.instrument_id) {
            reject(order, kRejectOcoLinkInvalid);
            orders_.free(order);
            return;
        }
    }

    // --- Phase-15 lifecycle/session gates (Tasks 15.3.3/15.3.4/15.3.10) ---
    // Quarantine > instrument status > market hours; a bound-but-
    // unverifiable feed fails closed on both axes. During an armed CALL
    // the instrument is ACTIVE-and-open by construction (control_sync
    // aborts the CALL otherwise), so the same gate admits accumulating
    // orders without a special case.
    if (const char* gate = admission_gate(*order)) {
        reject(order, gate);
        orders_.free(order);
        return;
    }

    // --- risk hook slot (Task 2.3.3) -----------------------------------------
    if (risk_fn_ != nullptr) {
        if (const char* code = risk_fn_(risk_ctx_, *order)) {
            reject(order, code);
            orders_.free(order);
            return;
        }
    }

    // --- Task 2.3.13 liquidity gates (spec §6.6) -------------------------------
    // Side-aware empty-book safeguard (remediation #35): a BUY fails only
    // when the ASK side is empty, a SELL only when the BID side is empty.
    // Applies to orders that take liquidity now — MARKET (any tif) and
    // IOC/FOK book orders; pending STOP/STOP_LIMIT queue instead, so they
    // are exempt here. Rejection precedes the WAL append: a reject is not a
    // state change and must not appear in the journal.
    const bool takes_now =
        order->type == OrderType::MARKET ||
        ((order->tif == TimeInForce::IOC ||
          order->tif == TimeInForce::FOK) &&
         order->type != OrderType::STOP &&
         order->type != OrderType::STOP_LIMIT &&
         order->type != OrderType::TRAILING_STOP);
    // An armed CALL exempts non-resting orders — they park for the uncross
    // regardless of the currently (possibly crossed) accumulated book.
    if (takes_now && auction_phase_ != kAuctionPhaseCall) {
        const bool opposite_empty =
            order->side == Side::BUY ? book_.ask_count() == 0
                                     : book_.bid_count() == 0;
        if (opposite_empty) {
            // Task 22.3.12 — a locally empty opposite side does not mean
            // no liquidity: implied quotes synthesize the out book from
            // its legs. Exempt the reject only when the read-only probe
            // shows real implied capacity — FOK stays outright-only
            // (§27 ruling: no combined-liquidity feasibility proof).
            bool implied_liquidity = false;
            if (order->tif != TimeInForce::FOK &&
                implied_ != nullptr && implied_->enabled() &&
                implied_ensure_registered()) {
                implied_liquidity =
                    implied_->evaluate_incoming(instrument_id_of(aux),
                                                *order)
                        .fillable_qty_units > 0;
            }
            if (!implied_liquidity) {
                reject(order, kRejectNoLiquidity);
                orders_.free(order);
                return;
            }
        }
    }

    // --- Task 2.3.13 #1 wide-spread band + Task 2.3.15 §6.6a protection ------
    // MARKET orders only, continuous trading (auction/halt rejects land
    // upstream in the lifecycle gate). Requires bound reference data — an
    // instrument-less book (unit tests) runs unprotected.
    bool market_protected = false;
    int64_t protection_price_ticks = 0;
    int64_t protection_bps = 0;
    int64_t protection_best_ticks = 0;
    if (order->type == OrderType::MARKET &&
        auction_phase_ != kAuctionPhaseCall) {
        const Instrument* instr = book_.instrument();
        if (instr != nullptr) {
            const PriceLevel* bid = book_.best_bid();
            const PriceLevel* ask = book_.best_ask();
            // Wide-spread rejection needs both sides populated — the empty
            // opposite case was already handled by NO_LIQUIDITY.
            if (bid != nullptr && ask != nullptr &&
                instr->max_spread_pips > 0 &&
                spread_pips(ask->price_ticks, bid->price_ticks, *instr) >
                    instr->max_spread_pips) {
                reject(order, kRejectWideSpread);
                orders_.free(order);
                return;
            }
            // The opposite side is non-empty per the gate above — except
            // a Task 22.3.12 implied-exempted taker, whose reference
            // quote lives on the legs; no local quote means no
            // protection bound can be computed (documented in §27).
            const PriceLevel* opp_lvl =
                order->side == Side::BUY ? ask : bid;
            if (opp_lvl != nullptr) {
            const int64_t best = opp_lvl->price_ticks;
            const int64_t bps = effective_slippage_bps(*instr);
            int64_t prot = 0;
            if (bps < kSlippageUnboundedBps &&
                slippage_protection_price(order->side, best, bps, prot)) {
                market_protected = true;
                protection_price_ticks = prot;
                protection_bps = bps;
                protection_best_ticks = best;
                // MARKET_WITH_PROTECTION marker — flows into
                // WalOrderNewPayload.flags below (bit2).
                order->flags |= kOrderFlagMarketWithProtection;
            }
            // bps >= kSlippageUnboundedBps: documented "protection off"
            // escape — the order sweeps unbounded (pre-protection behavior).
            // An overflowing protection price (degenerate config) skips
            // conversion rather than clamping to an invisible bound.
            }
        }
    }

    // --- WAL: acceptance is the first state change ---------------------------
    // Iceberg display slice is resolved once here so the WAL payload and the
    // resting slice agree.
    int64_t display = order->qty_units;
    if (order->type == OrderType::ICEBERG) {
        display = IcebergManager::visible_slice(order->qty_units,
                                                order->display_qty_units,
                                                book_.instrument());
    }
    const uint64_t wseq = journal_seq();   // the ORDER_NEW row's seq
    if (wal_ != nullptr &&
        wal_->write_order_new(*order, aux, instrument_id_of(aux), display,
                              now_ns_) != WalStatus::Ok) {
        wal_fault_ = true;
        reject(order, kRejectBookCapacity);
        orders_.free(order);
        return;
    }
    // L3 ADD (Task 17.3.1): emitted post-commit at the head of the
    // lifecycle — the order's own row in every downstream path (matching
    // fills, parked rests, pending conditionals all hang off this seq).
    // qty carries the order TOTAL (iceberg display slicing is internal);
    // ref_price carries the pending-conditional trigger when present.
    l3_emit_ev(static_cast<uint8_t>(L3Kind::Add), order->id,
               order->account_id, order->side, instrument_id_of(aux),
               order->price_ticks,
               /*ref_price*/ aux.stop_price_ticks > 0
                               ? aux.stop_price_ticks : 0,
               order->qty_units, /*delta*/ order->qty_units, 0,
               kL3RoleNone, l3_flags_of(*order), 0, wseq);

    // --- CALL accumulation (Task 15.3.6) -------------------------------------
    // Acceptance is journaled; instead of the continuous dispatch the
    // order accumulates — resting limits/icebergs into the crossed-tolerant
    // book, stops into the untriggered queue, MARKET/IOC/FOK into the
    // parked list for the deadline uncross.
    if (auction_phase_ == kAuctionPhaseCall) {
        if (!auction_accumulate(order, aux)) orders_.free(order);
        publish_auction_indicative();
        if (book_.book_seq() != seq0 && publisher_ != nullptr) {
            publish_depth();
        }
        return;
    }

    // --- dispatch ------------------------------------------------------------
    bool adopted = false;  // set when the stop queue takes ownership
    switch (order->type) {
        case OrderType::LIMIT:
        case OrderType::ICEBERG: {
            // §6.11 discretionary band (Task 2.3.26): the offset rides
            // OrderAux (hidden from the Order POD and public L2/L3);
            // pre-validated at intake — here the aggressive bound simply
            // substitutes the nominal limit for the sweep while the
            // remainder rests at limit.
            int64_t walk_bound = order->price_ticks;
            if (aux.discretionary_offset_pips != 0) {
                walk_bound = DiscretionaryExecutor::eval(
                                 order->side, order->price_ticks,
                                 aux.discretionary_offset_pips,
                                 *book_.instrument())
                                 .aggressive_price_ticks;
            }
            // §22.2 collar (Task 2.3.17): bounds snapshot once per phase.
            const CollarBounds cb =
                collar_.begin_phase(order->side, collar_rule_,
                                    ref_price_ticks_, ref_ts_ns_, now_ns_);
            // §6.6b trade-through intake check (Task 2.3.22): configured
            // through instruments.execution_rule — absent => unenforced.
            if (tt_enabled_) {
                TtCheckInput tin;
                tin.order_id = order->id;
                tin.side = order->side;
                tin.type = order->type;
                tin.tif = order->tif;
                tin.limit_ticks = order->price_ticks;
                tin.ts_ns = now_ns_;
                if (order->tif == TimeInForce::IOC ||
                    order->tif == TimeInForce::FOK) {
                    const ProtectedQuote q = trade_through_.quote();
                    tin.liquidity_at_or_better_units =
                        liquidity_at_or_better_units(
                            order->side, order->side == Side::BUY
                                             ? q.ask_ticks
                                             : q.bid_ticks);
                }
                const TtVerdict v = trade_through_.check(tin);
                if (v.decision == TtDecision::REJECT) {
                    emit_cancel_event(order->id, order->account_id,
                                      kWalCancelReasonUser, order);
                    last_reject_ = v.code;
                    ++reject_count_;
                    break;
                }
            }
            if (order->tif == TimeInForce::FOK &&
                !fok_feasible(*order, aux.trade_group_id, true,
                              walk_bound, cb)) {
                emit_cancel_event(order->id, order->account_id,
                                  kWalCancelReasonFokUnfilled, order);
                last_reject_ = kRejectFokUnfilled;
                ++reject_count_;
                break;
            }
            TakerResult r = walk_match(*order, aux, true, walk_bound, cb);
            // Task 22.3.12 — the discretionary walk bound (walk_bound)
            // substitutes the nominal limit for the implied sweep exactly
            // as it did for the outright sweep.
            implied_take(*order, aux, r, /*has_limit=*/true, walk_bound);
            finish_taker(*order, aux, r, display);
            break;
        }
        case OrderType::MARKET: {
            // §22.2 collar snapshot (Task 2.3.17) — same phase-entry
            // semantics as the limit arms.
            const CollarBounds cb =
                collar_.begin_phase(order->side, collar_rule_,
                                    ref_price_ticks_, ref_ts_ns_, now_ns_);
            // §6.6b trade-through (Task 2.3.22): the synthetic-limit bound
            // is clipped to the protected quote — stricter than the §6.6a
            // bps band; the quote supplies the bound when the band is off.
            bool tt_clipped = false;
            if (tt_enabled_) {
                TtCheckInput tin;
                tin.order_id = order->id;
                tin.side = order->side;
                tin.type = order->type;
                tin.tif = order->tif;
                tin.limit_ticks = 0;
                tin.protection_price_ticks =
                    market_protected ? protection_price_ticks : 0;
                tin.ts_ns = now_ns_;
                if (order->tif == TimeInForce::IOC ||
                    order->tif == TimeInForce::FOK) {
                    const ProtectedQuote q = trade_through_.quote();
                    tin.liquidity_at_or_better_units =
                        liquidity_at_or_better_units(
                            order->side, order->side == Side::BUY
                                             ? q.ask_ticks
                                             : q.bid_ticks);
                }
                const TtVerdict v = trade_through_.check(tin);
                if (v.decision == TtDecision::REJECT) {
                    emit_cancel_event(order->id, order->account_id,
                                      kWalCancelReasonUser, order);
                    last_reject_ = v.code;
                    ++reject_count_;
                    break;
                }
                if (v.decision == TtDecision::CLIP) {
                    market_protected = true;
                    protection_price_ticks = v.effective_limit_ticks;
                    tt_clipped = true;
                }
            }
            // §6.6a synthetic-limit conversion: a protected market order
            // sweeps only to protection_price_ticks — the walk is a plain
            // limited sweep, FOK feasibility is evaluated inside the band.
            if (order->tif == TimeInForce::FOK &&
                !fok_feasible(*order, aux.trade_group_id, market_protected,
                              protection_price_ticks, cb)) {
                emit_cancel_event(order->id, order->account_id,
                                  kWalCancelReasonFokUnfilled, order);
                last_reject_ = kRejectFokUnfilled;
                ++reject_count_;
                break;
            }
            TakerResult r = walk_match(*order, aux, market_protected,
                                       protection_price_ticks, cb);
            // Task 22.3.12 — implied liquidity fills an unprotected
            // market remainder; a §6.6a-protected order never sweeps
            // implied (the protection bound is a hard wall, not a quote
            // the combo could honor leg-by-leg).
            if (!market_protected) {
                implied_take(*order, aux, r, /*has_limit=*/false, 0);
            }
            if (tt_clipped && r.remaining > 0 && !r.dead && !wal_fault_) {
                // §6.6b remainder: cancelled SLIPPAGE_EXCEEDED; the TCA
                // stream sees MARKET_REMAINDER_SLIPPAGE through the guard.
                trade_through_.on_remainder_cancelled(
                    order->id, order->side, r.remaining, now_ns_);
            }
            if (market_protected) {
                // Surveillance seam (Phase-17 owns the NATS bridge): one
                // MarketOrderProtectionTriggered per conversion.
                if (protection_fn_ != nullptr) {
                    const MarketOrderProtectionEvent pe{
                        order->id, order->account_id, instrument_id_of(aux),
                        order->side, protection_best_ticks,
                        protection_price_ticks, protection_bps,
                        r.remaining > 0 ? r.remaining : 0, now_ns_};
                    protection_fn_(protection_ctx_, pe);
                }
                // §6.6a #3: an unfilled remainder after sweeping to the
                // protection price is rejected SLIPPAGE_EXCEEDED — it never
                // fills outside the collar and never rests.
                if (!r.dead && r.remaining > 0 && !wal_fault_) {
                    emit_cancel_event(order->id, order->account_id,
                                      kWalCancelReasonSlippageExceeded, order);
                    last_reject_ = kRejectSlippageExceeded;
                    ++reject_count_;
                    break;
                }
            }
            finish_taker(*order, aux, r, 0);
            break;
        }
        case OrderType::STOP:
        case OrderType::STOP_LIMIT: {
            // Already-crossed stops trigger immediately instead of
            // queueing — the trigger reference is the source-selected one
            // (Task 16.3.17); a stale/absent source simply cannot be due.
            const int64_t ref0 =
                trigger_refs().ref(aux.trigger_source);
            const bool due =
                ref0 > 0 &&
                (order->side == Side::BUY
                     ? ref0 >= aux.stop_price_ticks
                     : ref0 <= aux.stop_price_ticks);
            if (due) {
                StopOrderTrigger::TriggeredInfo info{};
                info.order_id = order->id;
                info.stop_ticks = aux.stop_price_ticks;
                info.observed_ticks = ref0;
                info.source = aux.trigger_source;
                journal_triggered(order, info, aux);
                process_triggered_info(order, aux, info);
                break;
            }
            // Track group/expiry/source alongside the pending stop.
            if (!track_meta(order->id, aux)) {
                emit_cancel_event(order->id, order->account_id,
                                  kWalCancelReasonUser, order);
                last_reject_ = kRejectBookCapacity;
                ++reject_count_;
                break;
            }
            // Task 16.3.16: a GSLO-flagged pending stop reserves its armed
            // notional in the engine exposure counter (admission cap was
            // checked pre-WAL); released on trigger/cancel.
            int64_t gslo_n = 0;
            if ((order->flags & kOrderFlagGslo) != 0) {
                gslo_n = gslo_notional(*order, aux.stop_price_ticks);
            }
            StopOrderTrigger::Pending pm{};
            pm.trigger_source = aux.trigger_source;
            pm.gslo_notional_units = gslo_n;
            if (!stops_.enqueue(order, aux.stop_price_ticks, pm)) {
                meta_erase(order->id);
                emit_cancel_event(order->id, order->account_id,
                                  kWalCancelReasonUser, order);
                last_reject_ = kRejectBookCapacity;
                ++reject_count_;
                break;
            }
            if (gslo_n > 0) {
                gslo_notional_units_ += gslo_n;
                ++gslo_live_count_;
            }
            adopted = true;
            break;
        }
        case OrderType::TRAILING_STOP: {
            // Tasks 16.3.3/16.3.15 — trail arming at admission:
            //   activation_price set   -> dormant until the reference
            //                           prints past the gate (armed=0);
            //   activation unset       -> arm on the live reference now;
            //                           LAST with no print yet defers to
            //                           the first settle evaluation;
            //                           an absent MARK/INDEX reference is
            //                           CONDITIONAL_TRIGGER_ORACLE_STALE —
            //                           a post-WAL admission cancel
            //                           (the ORDER_NEW row is committed).
            // Journal-free engines (WAL replay, unit tests) keep the
            // pending instead: the admission reject is a LIVE-ingress gate
            // only — the ORDER_NEW_EX row is committed upstream, so the
            // replaying engine must hold the order dormant until its
            // authoritative ORDER_TRIGGERED row (or ORDER_CANCEL) lands.
            const int64_t ref0 =
                trigger_refs().ref(aux.trigger_source);
            bool armed = aux.activation_price_ticks <= 0;
            if (armed && ref0 <= 0 && wal_ != nullptr &&
                aux.trigger_source != kTriggerSourceLast) {
                emit_cancel_event(order->id, order->account_id,
                                  kWalCancelReasonUser, order);
                last_reject_ = kRejectConditionalOracleStale;
                ++reject_count_;
                break;
            }
            const int64_t anchor = (armed && ref0 > 0) ? ref0 : 0;
            const Instrument* tins = book_.instrument();
            int64_t stop = aux.stop_price_ticks;
            if (armed && anchor > 0 && tins != nullptr) {
                const int64_t s = trail_stop_from_anchor(
                    order->side == Side::BUY, anchor, aux.trail_distance,
                    aux.trail_unit, *tins);
                if (s > 0) stop = s;
            }
            // An immediately-due trail (ref already past the armed stop)
            // fires at admission — journal + activate, never queued.
            if (armed && stop > 0 && ref0 > 0 &&
                (order->side == Side::BUY ? ref0 >= stop
                                          : ref0 <= stop)) {
                StopOrderTrigger::TriggeredInfo info{};
                info.order_id = order->id;
                info.stop_ticks = stop;
                info.observed_ticks = ref0;
                info.source = aux.trigger_source;
                journal_triggered(order, info, aux);
                process_triggered_info(order, aux, info);
                break;
            }
            if (!track_meta(order->id, aux)) {
                emit_cancel_event(order->id, order->account_id,
                                  kWalCancelReasonUser, order);
                last_reject_ = kRejectBookCapacity;
                ++reject_count_;
                break;
            }
            int64_t gslo_n = 0;
            if ((order->flags & kOrderFlagGslo) != 0 && stop > 0) {
                gslo_n = gslo_notional(*order, stop);
            }
            StopOrderTrigger::Pending pm{};
            pm.trigger_source = aux.trigger_source;
            pm.trail_unit = aux.trail_unit;
            pm.anchor_ticks = anchor;
            pm.activation_price_ticks = aux.activation_price_ticks;
            pm.trail_distance = aux.trail_distance;
            pm.armed = armed ? 1 : 0;
            pm.gslo_notional_units = gslo_n;
            if (!stops_.enqueue(order, stop, pm)) {
                meta_erase(order->id);
                emit_cancel_event(order->id, order->account_id,
                                  kWalCancelReasonUser, order);
                last_reject_ = kRejectBookCapacity;
                ++reject_count_;
                break;
            }
            if (gslo_n > 0) {
                gslo_notional_units_ += gslo_n;
                ++gslo_live_count_;
            }
            adopted = true;
            break;
        }
        case OrderType::PEG: {
            // Task 16.3.11/16.3.22: price_ticks already carries the
            // admission target (computed pre-WAL). Rest as a hidden-from-
            // L2 limit; the peg record drives deterministic repricing in
            // the settle wave.
            Order tmpl = *order;
            Order* node = nullptr;
            const BookError e = book_.add_order(tmpl, &node);
            if (e != BookError::OK) {
                emit_cancel_event(order->id, order->account_id,
                                  kWalCancelReasonUser, order);
                last_reject_ = e == BookError::DUPLICATE_ID
                                   ? kRejectDuplicateId
                                   : kRejectBookCapacity;
                ++reject_count_;
                break;
            }
            if (!track_meta(order->id, aux)) {
                (void)book_.cancel_order(order->id);
                emit_cancel_event(order->id, order->account_id,
                                  kWalCancelReasonUser, order);
                last_reject_ = kRejectBookCapacity;
                ++reject_count_;
                break;
            }
            pegs_[pegs_live_++] =
                PegRec{order->id, aux.peg_offset_ticks,
                       aux.peg_limit_ticks, aux.peg_mode, 1};
            break;
        }
        case OrderType::MOO:
        case OrderType::MOC:
            // Unreachable in continuous trading — admission gated to armed
            // CALL and auction_accumulate() claims them there. Fail closed
            // if one ever reaches the dispatch table.
            emit_cancel_event(order->id, order->account_id,
                              kWalCancelReasonUser, order);
            last_reject_ = kRejectOrderInvalid;
            ++reject_count_;
            break;
        default:
            break;  // unreachable — validated above
    }

    if (!adopted) orders_.free(order);
    settle();
    // Task 22.3.12 — a committed mutation can move an implied quote on
    // any link this book is a leg of (or put a newly-rested/amended
    // member of this book across an existing quote as its out book).
    if (book_.book_seq() != seq0) implied_rescan();
    quarantine_check();  // Task 15.3.10 — post-mutation crossed-book probe
    if (book_.book_seq() != seq0) {
        refresh_protected_quote();  // §6.6b #6 — internal book is the quote
        if (publisher_ != nullptr) publish_depth();
    }
}

// ---------------------------------------------------------------------------
// cancel / amend / tick
// ---------------------------------------------------------------------------

void MatchingEngine::on_cancel_received(uint64_t order_id,
                                        uint64_t account_id) noexcept {
    const uint64_t seq0 = book_.book_seq();
    auction_control_sync();  // armed-key observation before the verdict
    if (!cancel_internal(order_id, account_id, kWalCancelReasonUser, true)) {
        // Absent or foreign order — idempotent no-op, never a second
        // mutation (spec §24 #10).
        last_reject_ = kRejectUnknownOrder;
        ++reject_count_;
    }
    settle();
    // Task 22.3.12 — a cancel removes leg liquidity (quote worsens, no
    // new fills possible) but costs only a bounded scan; keep the rescan
    // uniform across every mutation tail.
    if (book_.book_seq() != seq0) implied_rescan();
    publish_auction_indicative();  // a CALL cancel can move the indicative
    quarantine_check();
    if (book_.book_seq() != seq0) {
        refresh_protected_quote();  // §6.6b #6 — internal book is the quote
        if (publisher_ != nullptr) publish_depth();
    }
}

// --- Task 2.3.13/2.3.15/2.3.20 helpers -----------------------------------------

const char* MatchingEngine::amend_state_gate() const noexcept {
    // §7.1 / Task 2.3.20 #5 + Phase-15 (Tasks 15.3.3/15.3.6/15.3.10):
    // quarantine outranks; an armed CALL rejects every amend with
    // AMEND_IN_AUCTION_REJECTED (cancels stay allowed — on_cancel_received
    // is ungated); otherwise the effective status drives the §7.1 amend
    // matrix (DRAFT fails closed alongside SUSPENDED). A book with neither
    // reference data nor a bound feed (unit tests) runs ungated.
    if (quarantined_) {
        return quarantine_code_ != nullptr ? quarantine_code_
                                           : kRejectInstrumentHalted;
    }
    if (auction_phase_ != kAuctionPhaseNone) return kRejectAmendInAuction;
    if (feed_ == nullptr && book_.instrument() == nullptr) return nullptr;
    return instrument_amend_gate(effective_status());
}

int64_t MatchingEngine::effective_slippage_bps(const Instrument& i) noexcept {
    // §6.6a sentinel: max_slippage_bps == 0 -> defaults (100 bps majors /
    // 200 bps exotics). Instrument carries no major/exotic class field —
    // settlement_cycle >= 2 (the §6.3 exotic T+2 settlement) is the
    // deterministic proxy.
    if (i.max_slippage_bps > 0) return i.max_slippage_bps;
    return i.settlement_cycle >= 2 ? kSlippageDefaultExoticBps
                                   : kSlippageDefaultMajorBps;
}

bool MatchingEngine::slippage_protection_price(Side side, int64_t best_ticks,
                                               int64_t bps,
                                               int64_t& out) noexcept {
    if (best_ticks <= 0 || bps <= 0) return false;
    // §6.6a literal integer formula: BUY collar = best + best*bps/10000,
    // SELL collar = best - best*bps/10000. Widening 128-bit product keeps
    // the intermediate exact; narrowing failure rejects the conversion.
    const safe_math::int128_t delta =
        safe_math::mul_wide_i64(best_ticks, bps) / 10'000;
    const safe_math::int128_t collar =
        side == Side::BUY ? safe_math::int128_t{best_ticks} + delta
                          : safe_math::int128_t{best_ticks} - delta;
    if (collar <= 0 || !safe_math::try_narrow_i128(collar, out)) return false;
    return true;
}

bool MatchingEngine::rearm_expiry(OrderMeta& m, int64_t new_expiry_ns) noexcept {
    // GTD/DAY re-arm after a journaled amend (§6.9 #2). The expiry heap is
    // engine-internal; capacity was preflighted by the caller so the only
    // remaining failure is heap_ == nullptr (cold-alloc fault).
    expiry_untrack(m);
    m.expiry_ns = new_expiry_ns;
    if (new_expiry_ns <= 0) return true;         // cleared — no timer
    if (!expiry_track(m)) return false;
    // Tick-forward expiry: a re-armed timestamp already in the past fires
    // immediately — the amend applies, then the order expires. Deterministic
    // (the cancel is journaled like any expiry).
    if (new_expiry_ns <= static_cast<int64_t>(now_ns_)) {
        expiry_untrack(m);
        (void)cancel_internal(m.order_id, 0, kWalCancelReasonExpired, false);
    }
    return true;
}

void MatchingEngine::on_amend_received(uint64_t order_id,
                                       int64_t new_price_ticks,
                                       int64_t new_qty_units,
                                       int64_t new_stop_price_ticks,
                                       uint64_t ingress_seq) noexcept {
    // IEngineIngress shim -> the single matching-thread replace path.
    on_amend_received_ex(order_id,
                         AmendRequest{new_price_ticks, new_qty_units,
                                      new_stop_price_ticks, 0, 0,
                                      ingress_seq});
}

void MatchingEngine::on_amend_received_ex(uint64_t order_id,
                                          const AmendRequest& req) noexcept {
    ++received_count_;
    const uint64_t seq0 = book_.book_seq();
    // An armed key observed here gates the amend below (CALL => reject).
    auction_control_sync();
    if (const char* code = amend_state_gate()) {
        last_reject_ = code;
        ++reject_count_;
        return;
    }
    if (wal_fault_) {
        last_reject_ = kRejectBookCapacity;
        ++reject_count_;
        return;
    }

    StopOrderTrigger::Pending* p = stops_.find(order_id);
    Order* o = book_.find_order(order_id);
    if (p == nullptr && o == nullptr) {
        last_reject_ = kRejectUnknownOrder;
        ++reject_count_;
        return;
    }
    // IOC/FOK amendments (§4.2 / Task 2.3.20): rejected outright — such
    // orders never rest, so a live record is defensive-only either way.
    const TimeInForce tif = p != nullptr ? p->order->tif : o->tif;
    if (tif == TimeInForce::IOC || tif == TimeInForce::FOK) {
        last_reject_ = kRejectAmendRejected;
        ++reject_count_;
        return;
    }

    // The meta slot doubles as the §6.9 #1 amend fence. All amendable
    // orders carry pool ids, so the slot is always creatable unless the
    // cold alloc failed — fail closed rather than amend unfenced.
    OrderMeta* m = meta_ensure(order_id);
    if (m == nullptr) {
        last_reject_ = kRejectBookCapacity;
        ++reject_count_;
        return;
    }
    // Concurrent-amend arbitration: the first APPLIED amend wins; a request
    // whose ingress_seq does not advance past the last applied seq is the
    // stale loser (STALE_MODIFY) — exactly one winner, no double-apply, and
    // identical under replay since only the winner was journaled.
    // ingress_seq == 0 marks an unsequenced source (WAL replay re-feeds
    // journaled winners without their envelope seq) — admitted unfenced.
    if (req.ingress_seq != 0 && m->amend_seen &&
        req.ingress_seq <= m->amend_seq) {
        last_reject_ = kRejectStaleModify;
        ++reject_count_;
        return;
    }

    if (p != nullptr) {
        amend_pending_stop(*p, req, *m);
    } else {
        amend_resting(*o, req, *m);
    }
    // Task 22.3.12 — an amended price can put a resting member across an
    // implied quote (out book) or improve a leg into fresh crossings.
    if (book_.book_seq() != seq0) implied_rescan();
    quarantine_check();
    if (book_.book_seq() != seq0) {
        refresh_protected_quote();  // §6.6b #6 — internal book is the quote
        if (publisher_ != nullptr) publish_depth();
    }
}

void MatchingEngine::amend_pending_stop(StopOrderTrigger::Pending& p,
                                        const AmendRequest& req,
                                        OrderMeta& m) noexcept {
    // Atomic (§6.9 #4): validate -> WAL -> mutate. A failure path leaves the
    // pending record untouched.
    Order* s = p.order;
    const int64_t price =
        req.price_ticks > 0 ? req.price_ticks : s->price_ticks;
    const int64_t qty =
        req.qty_units > 0 ? req.qty_units : s->qty_units;
    const int64_t stop = req.stop_price_ticks > 0 ? req.stop_price_ticks
                                                  : p.stop_price_ticks;
    const int64_t nexp =
        req.gtd_expiry_ns != 0 ? req.gtd_expiry_ns : m.expiry_ns;
    const Instrument* instr = book_.instrument();
    if (qty <= 0 ||
        (p.stop_price_ticks <= 0 && req.stop_price_ticks <= 0 &&
         p.trail_unit == kTrailUnitNone) ||
        (s->type == OrderType::STOP_LIMIT && price <= 0) ||
        (instr != nullptr && instr->max_order_qty_units > 0 &&
         qty > instr->max_order_qty_units)) {
        last_reject_ = kRejectOrderInvalid;
        ++reject_count_;
        return;
    }
    // Task 16.3.17/16.3.22: a trigger-source switch on a live pending
    // order is rejected — the source is admission-time metadata (the wire
    // carries no field; 0xFF = unchanged).
    if (req.trigger_source != 0xFF &&
        req.trigger_source != p.trigger_source) {
        last_reject_ = kRejectAmendRejected;
        ++reject_count_;
        return;
    }
    const bool expiry_change = nexp != m.expiry_ns;
    if (expiry_change && nexp > 0 &&
        (heap_ == nullptr ||
         (m.heap_index < 0 && heap_size_ >= heap_cap_))) {
        last_reject_ = kRejectBookCapacity;
        ++reject_count_;
        return;
    }

    const int64_t old_rem = s->qty_units - s->filled_qty_units;
    const uint64_t wseq = journal_seq();
    if (wal_ != nullptr &&
        wal_->write_order_modify(p.order_id, price, qty, stop, now_ns_) !=
            WalStatus::Ok) {
        wal_fault_ = true;
        last_reject_ = kRejectBookCapacity;
        ++reject_count_;
        return;
    }

    s->qty_units = qty;
    s->quantity = Decimal::from_mantissa(qty);
    if (s->type == OrderType::STOP_LIMIT) s->price_ticks = price;
    // L3 MODIFY (Task 17.3.1): pending-conditional amend — price carries
    // the (stop-limit) limit price, ref_price the new trigger. qty delta
    // is the change in remaining, 0 when only the trigger moved.
    l3_emit_ev(static_cast<uint8_t>(L3Kind::Modify), p.order_id,
               s->account_id, s->side, instrument_id_of(OrderAux{}),
               s->type == OrderType::STOP_LIMIT ? s->price_ticks : 0,
               /*ref_price*/ stop, s->qty_units - s->filled_qty_units,
               /*delta*/ (s->qty_units - s->filled_qty_units) - old_rem,
               0, kL3RoleNone, l3_flags_of(*s), 0, wseq);
    if (stop != p.stop_price_ticks) {
        // Trigger-price change loses priority (§6.9 #5): the pending chain
        // sorts strictly by stop price, so a re-stamp + resort re-parks the
        // order behind every equal-priced peer.
        p.stop_price_ticks = stop;
        s->timestamp_ns =
            now_ns_ > s->timestamp_ns ? now_ns_ : s->timestamp_ns;
        s->ingress_seq = ++emit_seq_;
        stops_.resort(s);
    }
    if (p.gslo_notional_units > 0) gslo_resync(p);  // qty/stop moved
    // Fence lands before rearm_expiry: an immediately-due GTD cancels the
    // order (and erases this meta slot) inside the call.
    m.amend_seq = req.ingress_seq;
    m.amend_seen = true;
    if (expiry_change && !rearm_expiry(m, nexp)) {
        last_reject_ = kRejectBookCapacity;  // heap_==nullptr, preflighted
        ++reject_count_;
    }
}

void MatchingEngine::amend_resting(Order& o, const AmendRequest& req,
                                   OrderMeta& m) noexcept {
    // Task 16.3.11/16.3.22: a pegged order's price is engine-managed — a
    // manual price change is rejected outright; qty amends are legal.
    if (o.type == OrderType::PEG && req.price_ticks > 0 &&
        req.price_ticks != o.price_ticks) {
        last_reject_ = kRejectAmendRejected;
        ++reject_count_;
        return;
    }
    const int64_t price =
        req.price_ticks > 0 ? req.price_ticks : o.price_ticks;
    const int64_t nexp =
        req.gtd_expiry_ns != 0 ? req.gtd_expiry_ns : m.expiry_ns;
    const Instrument* instr = book_.instrument();
    if (price <= 0) {
        last_reject_ = kRejectOrderInvalid;
        ++reject_count_;
        return;
    }

    // Iceberg: amend quantity is the order TOTAL; the live slice is resized
    // and the hidden remainder absorbs the difference.
    IcebergManager::Record* rec = icebergs_.find(o.id);
    int64_t qty;
    int64_t want_total = 0;
    int64_t ndisplay = 0;
    bool display_change = false;
    bool total_up = false;
    if (rec != nullptr) {
        want_total =
            req.qty_units > 0 ? req.qty_units : rec->total_qty_units;
        if (want_total <= rec->filled_total_units) {
            last_reject_ = kRejectOrderInvalid;
            ++reject_count_;
            return;
        }
        if (instr != nullptr && instr->max_order_qty_units > 0 &&
            want_total > instr->max_order_qty_units) {
            last_reject_ = kRejectOrderInvalid;
            ++reject_count_;
            return;
        }
        ndisplay = req.display_qty_units > 0 ? req.display_qty_units
                                             : rec->display_qty_units;
        display_change = ndisplay != rec->display_qty_units;
        total_up = want_total > rec->total_qty_units;
        const int64_t want_rem = want_total - rec->filled_total_units;
        qty = want_rem < ndisplay ? want_rem : ndisplay;
    } else {
        qty = req.qty_units > 0 ? req.qty_units : o.qty_units;
        if (instr != nullptr && instr->max_order_qty_units > 0 &&
            qty > instr->max_order_qty_units) {
            last_reject_ = kRejectOrderInvalid;
            ++reject_count_;
            return;
        }
    }
    if (qty <= o.filled_qty_units) {
        last_reject_ = kRejectOrderInvalid;
        ++reject_count_;
        return;
    }

    // §6.9 #5 priority decision: price change / qty increase / ICEBERG
    // display change all lose priority; qty-down-only keeps place.
    const bool repriced = price != o.price_ticks;
    const bool qty_up = qty > o.qty_units;
    const bool force_requeue =
        repriced || qty_up || display_change || total_up;

    // Pre-validate the lose-priority path's guards (CROSSED / LEVEL_CAPACITY)
    // so WAL-first ordering stays honest — the journaled modify will apply.
    if (repriced || qty_up) {
        const PriceLevel* opposite =
            o.side == Side::BUY ? book_.best_ask() : book_.best_bid();
        if (opposite != nullptr &&
            crosses(o.side, price, opposite->price_ticks)) {
            last_reject_ = kRejectOrderInvalid;
            ++reject_count_;
            return;
        }
        if (repriced) {
            // Destination level must exist or fit — account for the source
            // level freeing when this order is its only member.
            bool dst_found = false;
            uint32_t cnt = 0;
            for (uint32_t d = 0; ; ++d) {
                const PriceLevel* l = book_.level(o.side, d);
                if (l == nullptr) break;
                ++cnt;
                if (l->price_ticks == price) dst_found = true;
            }
            if (!dst_found) {
                uint32_t eff = cnt;
                // The book's own level-capacity guard replicates whether the
                // source level empties — find it via order count at the
                // order's current level.
                for (uint32_t d = 0; ; ++d) {
                    const PriceLevel* l = book_.level(o.side, d);
                    if (l == nullptr) break;
                    if (l->price_ticks == o.price_ticks &&
                        l->order_count <= 1) {
                        --eff;
                        break;
                    }
                }
                if (eff >= OrderBook::kMaxLevels) {
                    last_reject_ = kRejectBookCapacity;
                    ++reject_count_;
                    return;
                }
            }
        }
    }
    const bool expiry_change = nexp != m.expiry_ns;
    if (expiry_change && nexp > 0 &&
        (heap_ == nullptr ||
         (m.heap_index < 0 && heap_size_ >= heap_cap_))) {
        last_reject_ = kRejectBookCapacity;
        ++reject_count_;
        return;
    }

    // L3 captures the pre-mutation order-level remaining for qty_delta.
    const int64_t old_rem =
        rec != nullptr ? rec->total_qty_units - rec->filled_total_units
                       : o.qty_units - o.filled_qty_units;
    const uint64_t wseq = journal_seq();
    if (wal_ != nullptr &&
        wal_->write_order_modify(o.id, price,
                                 rec != nullptr ? want_total : qty, 0,
                                 now_ns_) != WalStatus::Ok) {
        wal_fault_ = true;
        last_reject_ = kRejectBookCapacity;
        ++reject_count_;
        return;
    }
    const uint64_t ts = now_ns_ > o.timestamp_ns ? now_ns_ : o.timestamp_ns;
    const uint64_t seq =
        req.ingress_seq != 0 ? req.ingress_seq : ++emit_seq_;
    const BookError e =
        book_.modify_order(o.id, price, qty, ts, seq, force_requeue);
    if (e != BookError::OK) {
        wal_fault_ = true;  // unreachable per preflight — fail closed
        return;
    }
    if (rec != nullptr) {
        rec->total_qty_units = want_total;
        rec->display_qty_units = ndisplay;
        rec->tmpl.price_ticks = price;
        rec->tmpl.qty_units = want_total;
        rec->tmpl.display_qty_units = ndisplay;
        o.display_qty_units = ndisplay;  // book-node informational mirror
    }
    // L3 MODIFY (Task 17.3.1): covers keep-priority qty-down and atomic
    // cancel-replace amends alike — the post-mutation price + order-level
    // remaining (iceberg: total less filled, not the visible slice).
    {
        const int64_t new_rem =
            rec != nullptr ? rec->total_qty_units - rec->filled_total_units
                           : o.qty_units - o.filled_qty_units;
        l3_emit_ev(static_cast<uint8_t>(L3Kind::Modify), o.id,
                   o.account_id, o.side, instrument_id_of(OrderAux{}),
                   price, /*ref_price*/ 0, new_rem, /*delta*/ new_rem - old_rem,
                   0, kL3RoleNone, l3_flags_of(o), 0, wseq);
    }
    m.amend_seq = req.ingress_seq;
    m.amend_seen = true;
    if (expiry_change) {
        // Preflighted — a due-immediately expiry cancels the order (meta
        // slot dies with it; the fence write above is already moot).
        (void)rearm_expiry(m, nexp);
    }
}

void MatchingEngine::on_time_tick(uint64_t now_ns) noexcept {
    const uint64_t seq0 = book_.book_seq();
    // The TIME_TICK row occupies a seq in every mode — consume it
    // unconditionally so journal-free replay keeps journal alignment.
    (void)journal_seq();
    if (wal_ != nullptr &&
        wal_->write_time_tick(now_ns) != WalStatus::Ok) {
        wal_fault_ = true;  // unjournaled clock must not drive mutations
        return;
    }
    if (now_ns > now_ns_) now_ns_ = now_ns;  // monotone logical clock
    // Phase-15 ordering: observe the armed key first (an EXTEND moves the
    // deadline before it can strike), then resolve a due CALL deadline,
    // then the continuous-mode crossed-book probe.
    auction_control_sync();
    auction_deadline_check();
    quarantine_check();
    expire_due();
    settle();
    // Task 22.3.12 — settle()'s pegged repricing can move leg quotes;
    // GTD/DAY expiry cancels only shrink capacity.
    if (book_.book_seq() != seq0) implied_rescan();
    publish_auction_indicative();  // expiry cancels can move the indicative
    if (book_.book_seq() != seq0) {
        refresh_protected_quote();  // §6.6b #6 — internal book is the quote
        if (publisher_ != nullptr) publish_depth();
    }
    flush_depth();  // drain depth frames coalesced since the last tick
    if (snapshot_fn_ != nullptr) {
        snapshot_fn_(snapshot_ctx_, now_ns_, trades_emitted_);
    }
}

// ---------------------------------------------------------------------------
// §6.6b helpers (Task 2.3.22)
// ---------------------------------------------------------------------------

void MatchingEngine::refresh_protected_quote() noexcept {
    const PriceLevel* bid = book_.best_bid();
    const PriceLevel* ask = book_.best_ask();
    trade_through_.on_book_mutation(bid != nullptr ? bid->price_ticks : 0,
                                    ask != nullptr ? ask->price_ticks : 0);
}

// Opposite-side liquidity at-or-better than the protected quote — the
// IOC/FOK presence input (§6.6b #2). Levels are price-ordered from best:
// for a BUY the asks ascend, so the walk stops at the first price beyond
// the quote (strictly worse is never "at-or-better").
int64_t MatchingEngine::liquidity_at_or_better_units(
    Side taker_side, int64_t quote_ticks) const noexcept {
    if (quote_ticks <= 0) return 0;
    const Side opp = taker_side == Side::BUY ? Side::SELL : Side::BUY;
    int64_t total = 0;
    for (uint32_t d = 0; d < OrderBook::kMaxLevels; ++d) {
        const PriceLevel* lvl = book_.level(opp, d);
        if (lvl == nullptr) break;
        const bool worse = taker_side == Side::BUY
                               ? lvl->price_ticks > quote_ticks
                               : lvl->price_ticks < quote_ticks;
        if (worse) break;
        total += lvl->total_qty_units;
    }
    return total;
}

// ---------------------------------------------------------------------------
// Phase-15 lifecycle / market-hours admission gates (Tasks 15.3.3/15.3.4)
// ---------------------------------------------------------------------------

InstrumentStatus MatchingEngine::effective_status() const noexcept {
    if (feed_ != nullptr) {
        const InstrumentFeed::Snapshot s = feed_->snapshot();
        // Unverifiable control plane (missing/malformed/unread key) fails
        // closed — a missing status key must never read as ACTIVE.
        if (!s.verifiable) return InstrumentStatus::SUSPENDED;
        return s.status;
    }
    const Instrument* i = book_.instrument();
    return i != nullptr ? i->status : InstrumentStatus::ACTIVE;
}

const char* MatchingEngine::admission_gate(const Order& o) const noexcept {
    // Engine-local quarantine (crossed-book halt / clearing-failure suspend)
    // outranks everything — the instrument is halted for matching purposes
    // regardless of what the lifecycle feed says. During an armed CALL the
    // quarantine still rejects new flow: the armed uncross resolves the
    // EXISTING book, it does not admit new orders into a halted instrument.
    if (quarantined_) {
        return quarantine_code_ != nullptr ? quarantine_code_
                                           : kRejectInstrumentHalted;
    }
    const char* st = instrument_entry_gate(effective_status(), o.type,
                                           o.flags);
    if (st != nullptr) return st;
    if (feed_ != nullptr) {
        const InstrumentFeed::Snapshot s = feed_->snapshot();
        // market:hours missing/unpublishable or feed unverifiable => the
        // schedule is unknown => fail closed (MARKET_CLOSED).
        if (!s.verifiable || !s.market_known ||
            !market_entry_allowed(s.market, now_ns_)) {
            return kRejectMarketClosed;
        }
    }
    return nullptr;
}

// ---------------------------------------------------------------------------
// Phase-15 auction machinery (Tasks 15.3.6/15.3.10)
// ---------------------------------------------------------------------------

// Journals one AUCTION_PHASE row. instrument_id comes from the bound book
// instrument (the dispatch field RecoveryManager routes on).
bool MatchingEngine::auction_journal(uint8_t phase, uint8_t reason,
                                     int64_t price_ticks,
                                     int64_t qty_units) noexcept {
    WalAuctionPhasePayload p{};
    const Instrument* i = book_.instrument();
    p.instrument_id = i != nullptr ? static_cast<uint32_t>(i->instrument_id)
                                   : 0;
    p.phase = phase;
    p.reason = reason;
    p.extension_count = auction_extensions_;
    p.flags = quarantined_ ? 1u : 0u;
    p.auction_id = static_cast<uint64_t>(
        auction_id_ > 0 ? auction_id_ : 0);
    p.deadline_ns = auction_deadline_ns_;
    p.cleared_price_ticks = price_ticks;
    p.cleared_qty_units = qty_units;
    (void)journal_seq();   // consume the phase row's seq (no L3 side —
                           // uncross fills/cancels journal their own rows)
    if (wal_ != nullptr &&
        wal_->write_auction_phase(p, now_ns_) != WalStatus::Ok) {
        wal_fault_ = true;
        return false;
    }
    return true;
}

void MatchingEngine::auction_enter_call(int64_t deadline_ns) noexcept {
    auction_id_ = deadline_ns;
    auction_deadline_ns_ = deadline_ns;
    auction_extensions_ = 0;
    auction_awaiting_ = false;
    auction_phase_ = kAuctionPhaseCall;
    auction_mode_ = true;               // §6.6b checks suspend in CALL
    trade_through_.set_auction(true);
    book_.set_allow_crossed(true);      // accumulate crossing interest
    (void)auction_journal(kAuctionPhaseCall, kAuctionReasonControl, 0, 0);
    // Publish the initial indicative so the WS bridge can stream it during
    // accumulation.
    if (publisher_ != nullptr) {
        const AuctionIndicative ind = auction_indicative();
        (void)publisher_->publish_auction_event(
            instrument_id_of(OrderAux{}),
            static_cast<uint64_t>(auction_id_), kAuctionPhaseCall,
            /*signal*/ 0, ind.price_ticks, ind.exec_qty_units,
            ind.buy_qty_units - ind.sell_qty_units, deadline_ns, 0, now_ns_);
        last_indicative_price_ = ind.price_ticks;
        last_indicative_qty_ = ind.exec_qty_units;
    }
}

void MatchingEngine::auction_exit(uint8_t phase, uint8_t reason) noexcept {
    // Terminal transitions: UNCROSS / CANCEL. Restores the continuous-mode
    // contract: crossed accumulation stops, §6.6b guards re-engage, parked
    // orders (MARKET/IOC/FOK — can never rest) are cancelled, and the armed
    // key is released via the control-thread result write.
    const int64_t done_id = auction_id_;
    if (phase != kAuctionPhaseCancel) {
        last_completed_auction_id_ = done_id;
    }
    auction_phase_ = kAuctionPhaseNone;
    auction_mode_ = false;
    trade_through_.set_auction(false);
    book_.set_allow_crossed(false);
    auction_awaiting_ = false;
    last_indicative_price_ = -1;
    last_indicative_qty_ = -1;
    if (phase == kAuctionPhaseCancel) {
        // Cancelled CALL: any accumulated resting book may be crossed —
        // fail-closed quarantine rather than resuming continuous trading
        // on a book that would never have existed without the auction.
        // quarantine_enter legitimates the forensic crossed state (the
        // allow_crossed flag) so book audits — including the recovery
        // boot invariant — read it as a halted CALL residue, not an
        // invariant violation.
        if (book_.crossed() && !quarantined_) {
            quarantine_enter(kRejectInstrumentHalted,
                             kAuctionReasonCrossedBook);
        }
    }
    (void)done_id;
    (void)reason;
}

void MatchingEngine::auction_control_sync() noexcept {
    // Cheap per-event observation of the armed key. Only the feed carries
    // auction state — a replay engine (feed_ == nullptr) reconstructs the
    // same transitions from journaled AUCTION_PHASE rows.
    if (feed_ == nullptr) return;
    const InstrumentFeed::Snapshot s = feed_->snapshot();
    if (!s.verifiable) {
        // Feed degraded mid-auction — hold CALL (fail closed already gates
        // all new flow through effective_status()/market gate); do NOT
        // complete a half-verified uncross.
        return;
    }
    if (auction_phase_ == kAuctionPhaseNone) {
        if (s.auction_armed &&
            s.auction_deadline_ns != last_completed_auction_id_) {
            auction_enter_call(s.auction_deadline_ns);
        }
        return;
    }
    // In CALL: armed-key lifecycle drives the transition.
    if (!s.auction_armed || s.status != InstrumentStatus::ACTIVE) {
        // Key withdrawn by the control plane (skip/direct resume or Go
        // ladder completion), or the instrument left ACTIVE while armed —
        // either way the CALL cannot complete; abort without uncrossing.
        if (auction_journal(kAuctionPhaseCancel, kAuctionReasonControl,
                            0, 0)) {
            parked_drain(kWalCancelReasonAuctionCancelled);
            auction_exit(kAuctionPhaseCancel, kAuctionReasonControl);
        }
        return;
    }
    if (s.auction_deadline_ns != auction_deadline_ns_) {
        // EXTEND:{new_dl} — the Go ladder pushed the deadline (its 30s
        // steps). The engine counts observed extensions; past the local
        // cap the extension is refused fail-closed (defensive mirror of
        // the 3-extension contract).
        if (auction_extensions_ >= kAuctionMaxExtensions) {
            if (feed_ != nullptr) {
                feed_->request_auction_result(auction_id_, false);
            }
            if (auction_journal(kAuctionPhaseQuarantine,
                                kAuctionReasonClearingFailed, 0, 0)) {
                quarantine_enter(kRejectAuctionClearingFailed,
                                 kAuctionReasonClearingFailed);
                parked_drain(kWalCancelReasonAuctionCancelled);
                auction_exit(kAuctionPhaseCancel, kAuctionReasonControl);
            }
            return;
        }
        ++auction_extensions_;
        auction_deadline_ns_ = s.auction_deadline_ns;
        auction_id_ = s.auction_deadline_ns;
        auction_awaiting_ = false;
        (void)auction_journal(kAuctionPhaseExtend,
                              kAuctionReasonDeadlineMoved, 0, 0);
        if (publisher_ != nullptr) {
            const AuctionIndicative ind = auction_indicative();
            (void)publisher_->publish_auction_event(
                instrument_id_of(OrderAux{}),
                static_cast<uint64_t>(auction_id_), kAuctionPhaseExtend,
                /*signal*/ 2, ind.price_ticks, ind.exec_qty_units,
                ind.buy_qty_units - ind.sell_qty_units,
                auction_deadline_ns_, 0, now_ns_);
        }
    }
}

void MatchingEngine::quarantine_enter(const char* code,
                                      uint8_t reason) noexcept {
    quarantined_ = true;
    quarantine_code_ = code;
    // A quarantined book that holds crossed levels is a forensic halt
    // state, not a violated invariant: mark the crossing as legitimately
    // accumulated so book_->validate() (and the recovery boot audit that
    // runs it) does not halt on the residue. No admission path can cross
    // it further — the quarantine gates every mutation except cancels.
    if (book_.crossed()) book_.set_allow_crossed(true);
    if (publisher_ != nullptr) {
        (void)publisher_->publish_auction_event(
            instrument_id_of(OrderAux{}),
            static_cast<uint64_t>(auction_id_ > 0 ? auction_id_ : 0),
            kAuctionPhaseQuarantine,
            /*signal*/ reason == kAuctionReasonCrossedBook ? 4 : 3,
            0, 0, 0, auction_deadline_ns_, 0, now_ns_);
    }
}

void MatchingEngine::quarantine_check() noexcept {
    // Task 15.3.10 — a continuous-mode book must NEVER show bid >= ask
    // without an immediate match. Outside an armed CALL that is an
    // anomaly: quarantine to HALTED + alert, resting orders preserved.
    if (auction_phase_ != kAuctionPhaseNone || quarantined_ ||
        feed_ == nullptr) {
        return;
    }
    const InstrumentFeed::Snapshot s = feed_->snapshot();
    if (!s.verifiable) return;  // feed already fails closed on its own
    if (!book_.crossed()) return;
    if (auction_journal(kAuctionPhaseQuarantine,
                        kAuctionReasonCrossedBook, 0, 0)) {
        quarantine_enter(kRejectCrossedBook, kAuctionReasonCrossedBook);
    }
}

void MatchingEngine::auction_deadline_check() noexcept {
    if (auction_phase_ != kAuctionPhaseCall || auction_awaiting_ ||
        now_ns_ < static_cast<uint64_t>(auction_deadline_ns_)) {
        return;
    }
    auction_resolve_deadline();
}

void MatchingEngine::auction_resolve_deadline() noexcept {
    const AuctionIndicative ind = auction_indicative();
    if (ind.has_candidates) {
        // A clearing price forms (possibly zero-volume — a non-crossed book
        // uncrosses trivially and reopens clean).
        if (ind.exec_qty_units > 0) {
            // Learn the committed volume BEFORE the WAL row (dry run is the
            // identical pairing pass with mutations suppressed).
            const int64_t cleared = auction_uncross(ind.price_ticks,
                                                    /*dry_run=*/true);
            if (!auction_journal(kAuctionPhaseUncross, kAuctionReasonNone,
                                 ind.price_ticks, cleared)) {
                return;  // wal_fault_ — engine halted; stay in CALL
            }
            (void)auction_uncross(ind.price_ticks, /*dry_run=*/false);
        } else {
            if (!auction_journal(kAuctionPhaseUncross, kAuctionReasonNone,
                                 ind.price_ticks, 0)) {
                return;
            }
        }
        // Terminal: release the armed key (the :result SET happens on the
        // control thread via the feed's request slot).
        if (feed_ != nullptr) {
            feed_->request_auction_result(auction_id_, /*cleared=*/true);
        }
        parked_drain(kWalCancelReasonAuctionCancelled);
        const bool was_quarantined = quarantined_;
        const uint64_t done_id = static_cast<uint64_t>(auction_id_);
        const int64_t done_deadline = auction_deadline_ns_;
        auction_exit(kAuctionPhaseUncross, kAuctionReasonNone);
        if (was_quarantined) {
            // Uncross-override complete: allocation drains the crossing
            // pair-set in price-time order, so a clean book resumes
            // continuous trading; a residual crossed state (defensive —
            // allocation proves impossible) re-quarantines immediately.
            quarantined_ = false;
            quarantine_code_ = nullptr;
            if (book_.crossed()) {
                if (auction_journal(kAuctionPhaseQuarantine,
                                    kAuctionReasonCrossedBook, 0, 0)) {
                    quarantine_enter(kRejectCrossedBook,
                                     kAuctionReasonCrossedBook);
                }
            }
        }
        if (publisher_ != nullptr) {
            (void)publisher_->publish_auction_event(
                instrument_id_of(OrderAux{}), done_id, kAuctionPhaseUncross,
                /*signal*/ 1, ind.price_ticks, ind.exec_qty_units,
                ind.buy_qty_units - ind.sell_qty_units, done_deadline,
                ind.exec_qty_units, now_ns_);
        }
        return;
    }
    // No candidate price exists — unpriceable crossed interest (parked
    // market orders with no limit anchor) or a structurally dead book.
    // Report FAILED for this deadline strike; the Go EXTEND ladder decides
    // extend-vs-suspend. The instrument keeps accumulating while armed.
    auction_awaiting_ = true;
    if (feed_ != nullptr) {
        feed_->request_auction_result(auction_id_, /*cleared=*/false);
    }
    if (!auction_journal(kAuctionPhaseStrikeFail,
                         kAuctionReasonClearingFailed, 0, 0)) {
        return;
    }
    if (publisher_ != nullptr) {
        (void)publisher_->publish_auction_event(
            instrument_id_of(OrderAux{}),
            static_cast<uint64_t>(auction_id_), kAuctionPhaseStrikeFail,
            /*signal*/ 3, 0, 0, 0, auction_deadline_ns_, 0, now_ns_);
    }
}

bool MatchingEngine::auction_accumulate(Order* order,
                                        const OrderAux& aux) noexcept {
    switch (order->type) {
        case OrderType::STOP:
        case OrderType::STOP_LIMIT:
        case OrderType::TRAILING_STOP: {
            // No trigger evaluation during CALL — there is no continuous
            // last-price to cross; the uncross itself sets it. Trailing
            // metadata rides the pending record; the trail arms in the
            // post-uncross settle wave.
            if (!track_meta(order->id, aux)) {
                emit_cancel_event(order->id, order->account_id,
                                  kWalCancelReasonUser, order);
                last_reject_ = kRejectBookCapacity;
                ++reject_count_;
                return false;
            }
            StopOrderTrigger::Pending pm{};
            pm.trigger_source = aux.trigger_source;
            pm.trail_unit = aux.trail_unit;
            pm.activation_price_ticks = aux.activation_price_ticks;
            pm.trail_distance = aux.trail_distance;
            pm.armed = 0;  // dormant until post-uncross settle evaluates
            if ((order->flags & kOrderFlagGslo) != 0 &&
                aux.stop_price_ticks > 0) {
                pm.gslo_notional_units =
                    gslo_notional(*order, aux.stop_price_ticks);
            }
            if (!stops_.enqueue(order, aux.stop_price_ticks, pm)) {
                meta_erase(order->id);
                emit_cancel_event(order->id, order->account_id,
                                  kWalCancelReasonUser, order);
                last_reject_ = kRejectBookCapacity;
                ++reject_count_;
                return false;
            }
            if (pm.gslo_notional_units > 0) {
                gslo_notional_units_ += pm.gslo_notional_units;
                ++gslo_live_count_;
            }
            return true;
        }
        case OrderType::PEG: {
            // Rests at the admission price (pre-WAL computed target or
            // collar fallback); repricing is suspended during CALL and
            // resumes in the post-uncross settle wave.
            if (pegs_ == nullptr || pegs_live_ >= pegs_cap_) {
                emit_cancel_event(order->id, order->account_id,
                                  kWalCancelReasonUser, order);
                last_reject_ = kRejectBookCapacity;
                ++reject_count_;
                return false;
            }
            Order tmpl = *order;
            Order* node = nullptr;
            if (book_.add_order(tmpl, &node) != BookError::OK ||
                !track_meta(order->id, aux)) {
                (void)book_.cancel_order(order->id);
                emit_cancel_event(order->id, order->account_id,
                                  kWalCancelReasonUser, order);
                last_reject_ = kRejectBookCapacity;
                ++reject_count_;
                return false;
            }
            pegs_[pegs_live_++] =
                PegRec{order->id, aux.peg_offset_ticks,
                       aux.peg_limit_ticks, aux.peg_mode, 0};
            return true;
        }
        default:
            break;
    }
    const bool never_rests =
        auction_market_like(order->type) ||
        order->tif == TimeInForce::IOC || order->tif == TimeInForce::FOK;
    if (never_rests) {
        // Park for the uncross — the journaled ORDER_NEW is already
        // committed upstream.
        if (!track_meta(order->id, aux) || !parked_push(order)) {
            meta_erase(order->id);
            emit_cancel_event(order->id, order->account_id,
                              kWalCancelReasonUser, order);
            last_reject_ = kRejectBookCapacity;
            ++reject_count_;
            return false;
        }
        return true;
    }
    // Resting limit (GTC/GTD/DAY): accumulate straight into the book — the
    // CROSSED insertion guard is suspended for the auction window.
    Order tmpl = *order;
    IcebergManager::Record* rec = nullptr;
    int64_t display = order->qty_units;
    if (order->type == OrderType::ICEBERG) {
        display = IcebergManager::visible_slice(
            order->qty_units, order->display_qty_units,
            book_.instrument());
        rec = icebergs_.register_order(tmpl, 0, display);
        if (rec == nullptr) {
            emit_cancel_event(order->id, order->account_id,
                              kWalCancelReasonUser, order);
            last_reject_ = kRejectBookCapacity;
            ++reject_count_;
            return false;
        }
        tmpl.qty_units = display < tmpl.qty_units ? display
                                                  : tmpl.qty_units;
        tmpl.filled_qty_units = 0;
    }
    Order* node = nullptr;
    const BookError e = book_.add_order(tmpl, &node);
    if (e != BookError::OK) {
        if (rec != nullptr) icebergs_.erase(order->id);
        emit_cancel_event(order->id, order->account_id,
                          kWalCancelReasonUser, order);
        last_reject_ = e == BookError::DUPLICATE_ID ? kRejectDuplicateId
                                                  : kRejectBookCapacity;
        ++reject_count_;
        return false;
    }
    if (!track_meta(order->id, aux)) {
        (void)book_.cancel_order(order->id);
        if (rec != nullptr) icebergs_.erase(order->id);
        emit_cancel_event(order->id, order->account_id,
                          kWalCancelReasonUser, order);
        last_reject_ = kRejectBookCapacity;
        ++reject_count_;
        return false;
    }
    return true;
}

int64_t MatchingEngine::effective_level_units(const PriceLevel& lvl,
                                              Side side) const noexcept {
    // Iceberg hidden remainder counts toward auction liquidity — the
    // uncross refills the slice in place (level tail), so the whole
    // unfilled total is eligible at the level price.
    int64_t q = lvl.total_qty_units;
    for (const Order* o = lvl.head; o != nullptr; o = o->next) {
        if (o->type != OrderType::ICEBERG) continue;
        if (const auto* rec = icebergs_.find(o->id)) {
            q += rec->total_qty_units - rec->filled_total_units -
                 remaining_qty_units(*o);
        }
    }
    (void)side;
    return q;
}

MatchingEngine::AuctionIndicative MatchingEngine::auction_indicative()
    const noexcept {
    AuctionIndicative ind{};
    if (parked_head_ == nullptr && book_.bid_count() == 0 &&
        book_.ask_count() == 0) {
        return ind;  // empty book — trivially no candidates
    }
    // Collect parked non-market limits per side (sorted by price) and the
    // parked market totals; parked markets are eligible at every price.
    struct PQ { int64_t price; int64_t qty; };
    PQ pb[kParkedOrderCap];
    PQ ps[kParkedOrderCap];
    uint32_t nb = 0, ns = 0;
    int64_t mkt_b = 0, mkt_s = 0;
    for (const Order* o = parked_head_; o != nullptr; o = o->next) {
        const int64_t rem = remaining_qty_units(*o);
        if (rem <= 0) continue;
        if (auction_market_like(o->type)) {
            // MARKET + MOO/MOC (16.3.25) are market liquidity at the
            // uncross — eligible at every candidate clearing price.
            if (o->side == Side::BUY) mkt_b += rem; else mkt_s += rem;
            continue;
        }
        if (o->side == Side::BUY) {
            if (nb < kParkedOrderCap) pb[nb++] = {o->price_ticks, rem};
        } else {
            if (ns < kParkedOrderCap) ps[ns++] = {o->price_ticks, rem};
        }
    }
    std::sort(pb, pb + nb,
              [](const PQ& a, const PQ& b) { return a.price < b.price; });
    std::sort(ps, ps + ns,
              [](const PQ& a, const PQ& b) { return a.price < b.price; });

    int64_t tot_b = mkt_b, tot_s = mkt_s;
    for (uint32_t i = 0; i < nb; ++i) tot_b += pb[i].qty;
    for (uint32_t i = 0; i < ns; ++i) tot_s += ps[i].qty;
    // Book totals: level totals plus iceberg hidden remainder.
    for (uint32_t d = 0; d < book_.bid_count(); ++d) {
        const PriceLevel* l = book_.level(Side::BUY, d);
        if (l != nullptr) tot_b += effective_level_units(*l, Side::BUY);
    }
    for (uint32_t d = 0; d < book_.ask_count(); ++d) {
        const PriceLevel* l = book_.level(Side::SELL, d);
        if (l != nullptr) tot_s += effective_level_units(*l, Side::SELL);
    }
    if (tot_b <= 0 || tot_s <= 0) return ind;

    // Merge ascending candidate streams: book bids (level array read
    // back-to-front), book asks, parked buy limits, parked sell limits.
    const uint32_t blvl = book_.bid_count(), alvl = book_.ask_count();
    ind.has_candidates = blvl > 0 || alvl > 0 || nb > 0 || ns > 0;
    if (!ind.has_candidates) return ind;

    uint32_t bi = blvl;  // bids asc index into the level array (blvl-1..0)
    uint32_t ai = 0;     // asks asc index
    uint32_t pbi = 0, psi = 0;
    // Cumulative walks: bidsBelow tracks bids/parked-buys strictly below
    // the candidate; sellsLe tracks asks/parked-sells at-or-below it.
    uint32_t bwalk = blvl;
    uint32_t pbwalk = 0;
    int64_t below_b = 0;
    uint32_t awalk = 0;
    uint32_t pswalk = 0;
    int64_t le_s = 0;
    const int64_t ref = last_price_ticks_;

    auto next_price = [&](int64_t* out) -> bool {
        int64_t best = INT64_MAX;
        bool any = false;
        if (bi > 0) {
            const PriceLevel* l = book_.level(Side::BUY, bi - 1);
            if (l != nullptr) { best = l->price_ticks; any = true; }
        }
        if (ai < alvl) {
            const PriceLevel* l = book_.level(Side::SELL, ai);
            if (l != nullptr && (!any || l->price_ticks < best)) {
                best = l->price_ticks; any = true;
            }
        }
        if (pbi < nb && (!any || pb[pbi].price < best)) {
            best = pb[pbi].price; any = true;
        }
        if (psi < ns && (!any || ps[psi].price < best)) {
            best = ps[psi].price; any = true;
        }
        if (!any) return false;
        *out = best;
        // Consume every stream head equal to the emitted price.
        while (bi > 0) {
            const PriceLevel* l = book_.level(Side::BUY, bi - 1);
            if (l == nullptr || l->price_ticks != best) break;
            --bi;
        }
        while (ai < alvl) {
            const PriceLevel* l = book_.level(Side::SELL, ai);
            if (l == nullptr || l->price_ticks != best) break;
            ++ai;
        }
        while (pbi < nb && pb[pbi].price == best) ++pbi;
        while (psi < ns && ps[psi].price == best) ++psi;
        return true;
    };

    int64_t p = 0;
    while (next_price(&p)) {
        while (bwalk > 0) {
            const PriceLevel* l = book_.level(Side::BUY, bwalk - 1);
            if (l == nullptr || l->price_ticks >= p) break;
            below_b += effective_level_units(*l, Side::BUY);
            --bwalk;
        }
        while (pbwalk < nb && pb[pbwalk].price < p) {
            below_b += pb[pbwalk].qty;
            ++pbwalk;
        }
        while (awalk < alvl) {
            const PriceLevel* l = book_.level(Side::SELL, awalk);
            if (l == nullptr || l->price_ticks > p) break;
            le_s += effective_level_units(*l, Side::SELL);
            ++awalk;
        }
        while (pswalk < ns && ps[pswalk].price <= p) {
            le_s += ps[pswalk].qty;
            ++pswalk;
        }
        const int64_t bq = tot_b - below_b;
        const int64_t aq = le_s + mkt_s;
        const int64_t v = bq < aq ? bq : aq;
        if (v <= 0) continue;
        const int64_t imb = bq - aq;
        const int64_t aimb = imb < 0 ? -imb : imb;
        const int64_t aimbest = ind.exec_qty_units > 0
            ? (ind.buy_qty_units > ind.sell_qty_units
                   ? ind.buy_qty_units - ind.sell_qty_units
                   : ind.sell_qty_units - ind.buy_qty_units)
            : INT64_MAX;
        bool better = v > ind.exec_qty_units;
        if (!better && v == ind.exec_qty_units) {
            if (aimb < aimbest) {
                better = true;
            } else if (aimb == aimbest) {
                if (ref > 0) {
                    const int64_t dc = p > ref ? p - ref : ref - p;
                    const int64_t db = ind.price_ticks > ref
                        ? ind.price_ticks - ref : ref - ind.price_ticks;
                    if (dc < db || (dc == db && p < ind.price_ticks)) {
                        better = true;
                    }
                } else if (p < ind.price_ticks) {
                    better = true;
                }
            }
        }
        if (better) {
            ind.price_ticks = p;
            ind.exec_qty_units = v;
            ind.buy_qty_units = bq;
            ind.sell_qty_units = aq;
        }
    }
    return ind;
}

namespace {

// Parked-order effective priority price for the uncross merge: MARKET
// orders rank ahead of every limit (single-price venue convention) — they
// sort to INT64_MAX on the buy frontier, INT64_MIN on the sell frontier.
inline int64_t parked_eff_price(const Order* o, Side s) noexcept {
    return auction_market_like(o->type)  // MARKET/MOO/MOC rank first
               ? (s == Side::BUY ? INT64_MAX : INT64_MIN)
               : o->price_ticks;
}

// Strict priority comparator over parked orders: markets rank first, then
// side-aware price, then FIFO by (timestamp_ns, ingress_seq).
inline bool parked_before(const Order* a, const Order* b, Side s) noexcept {
    const int64_t pa = parked_eff_price(a, s), pb2 = parked_eff_price(b, s);
    if (pa != pb2) return s == Side::BUY ? pa > pb2 : pa < pb2;
    if (a->timestamp_ns != b->timestamp_ns) {
        return a->timestamp_ns < b->timestamp_ns;
    }
    return a->ingress_seq < b->ingress_seq;
}

}  // namespace

int64_t MatchingEngine::auction_uncross(int64_t price_ticks,
                                        bool dry_run) noexcept {
    // Build the per-side parked priority arrays (markets first, then
    // price-time). Book order cursors walk levels live so iceberg slice
    // refills stay reachable at the level tail.
    Order* pb[kParkedOrderCap];
    Order* psl[kParkedOrderCap];
    uint32_t nb = 0, ns = 0;
    for (Order* o = parked_head_; o != nullptr; o = o->next) {
        if (remaining_qty_units(*o) <= 0) continue;
        if (o->side == Side::BUY) { if (nb < kParkedOrderCap) pb[nb++] = o; }
        else if (ns < kParkedOrderCap) psl[ns++] = o; }
    std::sort(pb, pb + nb, [](const Order* a, const Order* b) {
        return parked_before(a, b, Side::BUY);
    });
    std::sort(psl, psl + ns, [](const Order* a, const Order* b) {
        return parked_before(a, b, Side::SELL);
    });
    uint32_t bi = 0, si = 0;

    // Book cursors: current node per side; the successor is resolved while
    // the node is still alive so a fully-consuming fill can advance past a
    // freed node without touching it.
    auto first_book = [&](Side s) -> Order* {
        for (uint32_t d = 0; d < OrderBook::kMaxLevels; ++d) {
            const PriceLevel* l = book_.level(s, d);
            if (l == nullptr) break;
            const bool ok = s == Side::BUY ? l->price_ticks >= price_ticks
                                           : l->price_ticks <= price_ticks;
            if (!ok) break;
            if (l->head != nullptr) return l->head;
        }
        return nullptr;
    };
    auto next_book = [&](Order* cur, Side s) -> Order* {
        if (cur == nullptr) return nullptr;
        if (cur->next != nullptr) return cur->next;
        const int64_t cp = cur->price_ticks;
        bool past = false;
        for (uint32_t d = 0; d < OrderBook::kMaxLevels; ++d) {
            const PriceLevel* l = book_.level(s, d);
            if (l == nullptr) break;
            const bool ok = s == Side::BUY ? l->price_ticks >= price_ticks
                                           : l->price_ticks <= price_ticks;
            if (!ok) break;
            if (!past) {
                if (l->price_ticks == cp) past = true;
                continue;
            }
            if (l->head != nullptr) return l->head;
        }
        return nullptr;
    };

    Order* b_book = first_book(Side::BUY);
    Order* s_book = first_book(Side::SELL);

    // Remaining eligible pools — the FOK frontier check reads them.
    int64_t pool_b = 0, pool_s = 0;
    for (uint32_t i = 0; i < nb; ++i) pool_b += remaining_qty_units(*pb[i]);
    for (uint32_t i = 0; i < ns; ++i) pool_s += remaining_qty_units(*psl[i]);
    for (uint32_t d = 0; d < OrderBook::kMaxLevels; ++d) {
        const PriceLevel* l = book_.level(Side::BUY, d);
        if (l == nullptr || l->price_ticks < price_ticks) break;
        pool_b += effective_level_units(*l, Side::BUY);
    }
    for (uint32_t d = 0; d < OrderBook::kMaxLevels; ++d) {
        const PriceLevel* l = book_.level(Side::SELL, d);
        if (l == nullptr || l->price_ticks > price_ticks) break;
        pool_s += effective_level_units(*l, Side::SELL);
    }

    int64_t cleared = 0;
    while (!wal_fault_) {
        // Frontier selection: parked candidates merge with book heads in
        // strict price-time order; parked markets (eff price = INT64_MAX)
        // always lead.
        Order* pb_cur = bi < nb ? pb[bi] : nullptr;
        Order* b = pb_cur;
        if (b_book != nullptr) {
            const int64_t pp =
                b != nullptr ? parked_eff_price(b, Side::BUY) : INT64_MIN;
            if (b == nullptr || b_book->price_ticks > pp ||
                (b_book->price_ticks == pp &&
                 (b_book->timestamp_ns < b->timestamp_ns ||
                  (b_book->timestamp_ns == b->timestamp_ns &&
                   b_book->ingress_seq <= b->ingress_seq)))) {
                b = b_book;
            }
        }
        Order* ps_cur = si < ns ? psl[si] : nullptr;
        Order* s = ps_cur;
        if (s_book != nullptr) {
            const int64_t pp =
                s != nullptr ? parked_eff_price(s, Side::SELL) : INT64_MAX;
            if (s == nullptr || s_book->price_ticks < pp ||
                (s_book->price_ticks == pp &&
                 (s_book->timestamp_ns < s->timestamp_ns ||
                  (s_book->timestamp_ns == s->timestamp_ns &&
                   s_book->ingress_seq <= s->ingress_seq)))) {
                s = s_book;
            }
        }
        if (b == nullptr || s == nullptr) break;

        // FOK semantics inside the uncross: an all-or-nothing order that
        // cannot fill in full at the clearing price is skipped (cancelled)
        // WITHOUT consuming opposite-side liquidity.
        const int64_t brem = remaining_qty_units(*b);
        const int64_t srem = remaining_qty_units(*s);
        if (b->tif == TimeInForce::FOK && brem > pool_s) {
            // Successor BEFORE mutation — a book-leg removal frees the node.
            Order* bsucc = (b == b_book)
                               ? next_book(b_book, Side::BUY) : nullptr;
            if (!dry_run) {
                emit_cancel_event(b->id, b->account_id,
                                  kWalCancelReasonFokUnfilled, b);
                if (b != b_book) {
                    // Mark consumed — the terminal drain frees the node
                    // without re-journaling a second cancel.
                    b->filled_qty_units = b->qty_units;
                } else {
                    // Unreachable today (FOK never rests) — defensive: keep
                    // book invariants exact if that ever changes.
                    (void)book_.cancel_order(b->id);
                }
            }
            pool_b -= brem;
            if (b == pb_cur) ++bi; else b_book = bsucc;
            continue;
        }
        if (s->tif == TimeInForce::FOK && srem > pool_b) {
            Order* ssucc = (s == s_book)
                               ? next_book(s_book, Side::SELL) : nullptr;
            if (!dry_run) {
                emit_cancel_event(s->id, s->account_id,
                                  kWalCancelReasonFokUnfilled, s);
                if (s != s_book) {
                    s->filled_qty_units = s->qty_units;
                } else {
                    (void)book_.cancel_order(s->id);
                }
            }
            pool_s -= srem;
            if (s == ps_cur) ++si; else s_book = ssucc;
            continue;
        }

        const int64_t fill = brem < srem ? brem : srem;
        // Resolve book successors BEFORE mutation when the fill will
        // consume the node (apply_fill may free it).
        Order* nb_book = b_book;
        Order* ns_book = s_book;
        if (b == b_book && fill == brem) nb_book = next_book(b_book, Side::BUY);
        if (s == s_book && fill == srem) ns_book = next_book(s_book, Side::SELL);

        if (!dry_run) {
            // Capture ids up front — apply_fill may free a fully-consumed
            // book node, so nothing below may dereference a dead pointer.
            const uint64_t bid = b->id;
            const uint64_t sid = s->id;
            const uint64_t tid = tid_stream()++;
            const uint32_t instr = instrument_id_of(OrderAux{});
            // L3 pre-mutation capture — apply_fill may free either leg.
            const Side b_side = b->side;
            const Side s_side = s->side;
            const uint64_t b_acct = b->account_id;
            const uint64_t s_acct = s->account_id;
            const uint8_t b_flags = l3_ != nullptr ? l3_flags_of(*b) : 0;
            const uint8_t s_flags = l3_ != nullptr ? l3_flags_of(*s) : 0;
            const int64_t b_rem_pre =
                l3_ != nullptr ? effective_remaining_units(b) : 0;
            const int64_t s_rem_pre =
                l3_ != nullptr ? effective_remaining_units(s) : 0;
            const uint64_t wseq = journal_seq();
            if (wal_ != nullptr &&
                wal_->write_trade(tid, bid, sid, instr, price_ticks,
                                  fill, now_ns_) != WalStatus::Ok) {
                wal_fault_ = true;
                break;
            }
            // Book leg: apply_fill keeps level totals/index consistent and
            // frees fully-consumed nodes. Parked legs bump node accounting
            // directly (they live outside the book).
            if (b == b_book) {
                const BookError e = book_.apply_fill(b, fill);
                if (e != BookError::OK) { wal_fault_ = true; break; }
                if (auto* rec = icebergs_.find(bid)) {
                    rec->filled_total_units += fill;
                    if (book_.find_order(bid) == nullptr) {
                        replenish_iceberg(rec);
                    }
                }
                if (book_.find_order(bid) == nullptr) {
                    meta_erase(bid);
                    if (icebergs_.find(bid) == nullptr) {
                        oco_on_dead(bid, /*by_fill=*/true);
                    }
                }
            } else {
                b->filled_qty_units += fill;
            }
            if (s == s_book) {
                const BookError e = book_.apply_fill(s, fill);
                if (e != BookError::OK) { wal_fault_ = true; break; }
                if (auto* rec = icebergs_.find(sid)) {
                    rec->filled_total_units += fill;
                    if (book_.find_order(sid) == nullptr) {
                        replenish_iceberg(rec);
                    }
                }
                if (book_.find_order(sid) == nullptr) {
                    meta_erase(sid);
                    if (icebergs_.find(sid) == nullptr) {
                        oco_on_dead(sid, /*by_fill=*/true);
                    }
                }
            } else {
                s->filled_qty_units += fill;
            }
            last_price_ticks_ = price_ticks;
            ++trades_emitted_;
            if (publisher_ != nullptr) {
                (void)publisher_->publish_trade(tid, bid, sid,
                                                price_ticks, fill,
                                                book_.book_seq(), now_ns_);
            }
            // L3 Fill — one TRADE row, both uncross legs share wal_seq;
            // role 3 marks auction fills vs continuous 1/2 (spec §11.3).
            l3_emit_ev(static_cast<uint8_t>(L3Kind::Fill), bid, b_acct,
                       b_side, instr, price_ticks, /*ref*/ 0,
                       b_rem_pre - fill, /*delta*/ -fill, tid,
                       kL3RoleAuction, b_flags, 0, wseq);
            l3_emit_ev(static_cast<uint8_t>(L3Kind::Fill), sid, s_acct,
                       s_side, instr, price_ticks, /*ref*/ 0,
                       s_rem_pre - fill, /*delta*/ -fill, tid,
                       kL3RoleAuction, s_flags, 0, wseq);
        }
        cleared += fill;
        pool_b -= fill;
        pool_s -= fill;
        if (b == pb_cur) {
            if (fill == brem) ++bi;
        } else {
            b_book = nb_book;
        }
        if (s == ps_cur) {
            if (fill == srem) ++si;
        } else {
            s_book = ns_book;
        }
        // Partially-consumed parked nodes keep their array slot — the
        // cursor re-reads their remaining on the next iteration.
    }
    return cleared;
}

void MatchingEngine::parked_drain(uint8_t wal_reason) noexcept {
    while (parked_head_ != nullptr) {
        Order* o = parked_head_;
        parked_head_ = o->next;
        if (parked_head_ == nullptr) parked_tail_ = nullptr;
        o->next = nullptr;
        --parked_count_;
        const int64_t rem = remaining_qty_units(*o);
        if (rem > 0) {
            // Unfilled at terminal — IOC-style remainder cancel (FOK uses
            // its dedicated unfilled reason).
            emit_cancel_event(o->id, o->account_id,
                              o->tif == TimeInForce::FOK
                                  ? kWalCancelReasonFokUnfilled
                                  : wal_reason, o);
        }
        meta_erase(o->id);
        oco_on_dead(o->id, /*by_fill=*/rem <= 0);
        orders_.free(o);
    }
}

bool MatchingEngine::parked_push(Order* o) noexcept {
    if (parked_count_ >= kParkedOrderCap) return false;
    o->next = nullptr;
    if (parked_tail_ != nullptr) parked_tail_->next = o;
    else parked_head_ = o;
    parked_tail_ = o;
    ++parked_count_;
    return true;
}

Order* MatchingEngine::parked_find(uint64_t order_id) noexcept {
    for (Order* o = parked_head_; o != nullptr; o = o->next) {
        if (o->id == order_id) return o;
    }
    return nullptr;
}

bool MatchingEngine::on_auction_phase_replay(
    const WalAuctionPhasePayload& p) noexcept {
    // Journal-free application: wal_ == nullptr in the replay engine, so
    // every internal journal call is a no-op — the transitions re-derive
    // exactly what the live engine committed.
    if (wal_fault_) return false;
    switch (p.phase) {
        case kAuctionPhaseCall:
            if (auction_phase_ != kAuctionPhaseNone) return false;
            auction_id_ = static_cast<int64_t>(p.auction_id);
            auction_deadline_ns_ = p.deadline_ns;
            auction_extensions_ = 0;
            auction_awaiting_ = false;
            auction_phase_ = kAuctionPhaseCall;
            auction_mode_ = true;
            trade_through_.set_auction(true);
            book_.set_allow_crossed(true);
            return true;
        case kAuctionPhaseExtend:
            if (auction_phase_ == kAuctionPhaseNone) return false;
            if (p.deadline_ns != auction_deadline_ns_) {
                auction_deadline_ns_ = p.deadline_ns;
                auction_id_ = static_cast<int64_t>(p.auction_id);
                ++auction_extensions_;
                auction_awaiting_ = false;
            }
            return true;
        case kAuctionPhaseUncross:
            // Deadline resolution re-runs in the replay engine: the
            // replayed book holds the same accumulated orders, so the same
            // clearing price and fills re-derive — the journaled TRADE rows
            // that follow dedup through the journaled-fill ledger.
            if (auction_phase_ == kAuctionPhaseCall) {
                auction_deadline_ns_ = p.deadline_ns;
                auction_awaiting_ = false;
                auction_resolve_deadline();
            }
            return true;
        case kAuctionPhaseCancel:
            if (auction_phase_ == kAuctionPhaseNone) return false;
            parked_drain(kWalCancelReasonAuctionCancelled);
            auction_exit(kAuctionPhaseCancel, kAuctionReasonControl);
            return true;
        case kAuctionPhaseQuarantine:
            if ((p.flags & 1u) != 0 && !quarantined_) {
                quarantined_ = true;
                quarantine_code_ =
                    p.reason == kAuctionReasonCrossedBook
                        ? kRejectCrossedBook
                        : kRejectAuctionClearingFailed;
                // Forensic legitimation — same as quarantine_enter: a
                // halted book may legitimately hold crossed levels.
                if (book_.crossed()) book_.set_allow_crossed(true);
            }
            if (auction_phase_ != kAuctionPhaseNone &&
                p.reason == kAuctionReasonClearingFailed) {
                parked_drain(kWalCancelReasonAuctionCancelled);
                auction_exit(kAuctionPhaseCancel, kAuctionReasonControl);
            }
            return true;
        case kAuctionPhaseStrikeFail:
            if (auction_phase_ == kAuctionPhaseCall) {
                auction_awaiting_ = true;
            }
            return true;
        default:
            return false;
    }
}

void MatchingEngine::adopt_auction_state(
    uint8_t phase, int64_t auction_id, int64_t deadline_ns,
    uint8_t extensions, bool awaiting, int64_t last_completed,
    bool quarantined, const char* quarantine_code, Order* parked_head,
    uint32_t parked_count) noexcept {
    auction_phase_ = phase;
    auction_id_ = auction_id;
    auction_deadline_ns_ = deadline_ns;
    auction_extensions_ = extensions;
    auction_awaiting_ = awaiting;
    last_completed_auction_id_ = last_completed;
    quarantined_ = quarantined;
    quarantine_code_ = quarantine_code;
    parked_head_ = parked_head;
    parked_count_ = parked_count;
    parked_tail_ = nullptr;
    if (parked_head_ != nullptr) {
        Order* t = parked_head_;
        while (t->next != nullptr) t = t->next;
        parked_tail_ = t;
    }
    if (phase != kAuctionPhaseNone) {
        auction_mode_ = true;
        trade_through_.set_auction(true);
        book_.set_allow_crossed(true);
    }
}

}  // namespace exch
