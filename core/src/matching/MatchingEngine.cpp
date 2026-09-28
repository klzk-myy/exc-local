// Task 2.3.2 — MatchingEngine: price-time priority matching orchestrator
// (spec §3.2, §3.7, §6.5). See matching/MatchingEngine.hpp for the design
// contract: validate -> risk hook -> WAL -> mutate -> publish, zero-alloc
// hot path, deterministic (no wall clock — logical time only via
// on_time_tick), fail-closed on WAL/table/book-structural faults.
//
// WAL logging discipline: externally-caused events are journaled —
// ORDER_NEW (acceptance), ORDER_CANCEL (user/expiry/STP/FOK/IOC-remainder),
// ORDER_MODIFY (amends + STP decrements), TRADE (per fill), TIME_TICK.
// Internally-DERIVED transitions — iceberg slice refresh, stop trigger
// activation — are NOT journaled: they are deterministic functions of the
// logged stream and re-derive identically on replay (single source of
// truth; a derived entry would double-apply under verbatim replay).

#include "matching/MatchingEngine.hpp"

#include <new>

#include "book/Instrument.hpp"
#include "matching/IpcPublisher.hpp"

namespace exch {

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
                               IpcPublisher* publisher) noexcept
    : shard_id_(shard_id),
      book_(book),
      orders_(orders),
      wal_(wal),
      publisher_(publisher),
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
}

MatchingEngine::~MatchingEngine() {
    delete[] heap_;
    delete[] meta_;
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
    if (aux.trade_group_id == 0 && aux.gtd_expiry_ns <= 0) return true;
    // An expiring order without heap storage would silently miss its GTD —
    // fail closed instead of tracking only half the metadata.
    if (aux.gtd_expiry_ns > 0 && heap_ == nullptr) return false;
    OrderMeta* m = meta_ensure(order_id);
    if (m == nullptr) return false;
    m->trade_group_id = aux.trade_group_id;
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
}

void MatchingEngine::emit_cancel_event(uint64_t order_id, uint64_t account_id,
                                       uint8_t wal_reason) noexcept {
    if (wal_ != nullptr &&
        wal_->write_order_cancel(order_id, account_id, wal_reason, now_ns_) !=
            WalStatus::Ok) {
        wal_fault_ = true;
    }
    if (publisher_ != nullptr) {
        (void)publisher_->publish_order_cancel(order_id, account_id, now_ns_);
    }
}

uint32_t MatchingEngine::instrument_id_of(const OrderAux& aux) const noexcept {
    if (aux.instrument_id != 0) return aux.instrument_id;
    const Instrument* i = book_.instrument();
    return i != nullptr ? static_cast<uint32_t>(i->instrument_id) : 0;
}

void MatchingEngine::publish_depth() noexcept {
    if (publisher_ != nullptr) {
        (void)publisher_->publish_book_snapshot(book_, instrument_id_of(OrderAux{}),
                                          now_ns_);
    }
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
        if (check_account && acct != account_id) return false;
        if (wal_ != nullptr &&
            wal_->write_order_cancel(order_id, acct, wal_reason, now_ns_) !=
                WalStatus::Ok) {
            wal_fault_ = true;
            return false;
        }
        s = stops_.remove(order_id);
        meta_erase(order_id);
        if (publisher_ != nullptr) {
            (void)publisher_->publish_order_cancel(order_id, acct, now_ns_);
        }
        orders_.free(s);
        return true;
    }

    Order* o = book_.find_order(order_id);
    if (o == nullptr) return false;
    if (check_account && o->account_id != account_id) return false;
    const uint64_t acct = o->account_id;
    if (wal_ != nullptr &&
        wal_->write_order_cancel(order_id, acct, wal_reason, now_ns_) !=
            WalStatus::Ok) {
        wal_fault_ = true;
        return false;
    }
    if (book_.cancel_order(order_id) != BookError::OK) {
        wal_fault_ = true;  // journaled cancel could not apply — halt
        return false;
    }
    meta_erase(order_id);
    icebergs_.erase(order_id);  // releases hidden remainder if iceberg
    if (publisher_ != nullptr) {
        (void)publisher_->publish_order_cancel(order_id, acct, now_ns_);
    }
    return true;
}

// ---------------------------------------------------------------------------
// STP application — maker/taker same account or same nonzero trade group.
// Mutates res: sets dead when the taker's remainder must be cancelled.
// ---------------------------------------------------------------------------

bool MatchingEngine::apply_stp(Order* maker, StpAction action,
                               int64_t& taker_rem, TakerResult& res) noexcept {
    const uint64_t maker_id = maker->id;
    const uint64_t maker_acct = maker->account_id;
    switch (action) {
        case StpAction::PROCEED:
            return false;  // caller proceeds with a normal fill
        case StpAction::CANCEL_TAKER:
            res.dead = true;
            res.dead_reason = kWalCancelReasonStp;
            res.dead_code = kRejectStpCancelled;
            return false;
        case StpAction::CANCEL_MAKER:
        case StpAction::CANCEL_BOTH:
            (void)cancel_internal(maker_id, 0, kWalCancelReasonStp, false);
            if (action == StpAction::CANCEL_BOTH) {
                res.dead = true;
                res.dead_reason = kWalCancelReasonStp;
                res.dead_code = kRejectStpCancelled;
            }
            return true;  // maker consumed from the queue
        case StpAction::DECREMENT: {
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
            bool maker_died = false;
            bool applied = true;
            if (new_node_qty <= maker->filled_qty_units) {
                // Slice fully consumed by the decrement -> cancel it.
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
                    if (publisher_ != nullptr) {
                        (void)publisher_->publish_order_cancel(maker_id, maker_acct,
                                                         now_ns_);
                    }
                }
            } else if (node_take > 0) {
                // qty-down at the same price preserves queue priority.
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
                rec->total_qty_units -= hidden_take;
                if (maker_died) {
                    replenish_iceberg(rec);  // may erase the record
                } else if (rec->total_qty_units <= rec->filled_total_units) {
                    icebergs_.erase(maker_id);
                    meta_erase(maker_id);
                }
            } else if (maker_died) {
                meta_erase(maker_id);
            }
            taker_rem -= dec;
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
    int64_t limit_ticks) noexcept {
    TakerResult res{remaining_qty_units(taker), false, 0, nullptr};
    const Side opp = taker.side == Side::BUY ? Side::SELL : Side::BUY;
    const uint32_t instr = instrument_id_of(aux);

    while (res.remaining > 0 && !res.dead && !wal_fault_) {
        const PriceLevel* lvl = opp == Side::SELL ? book_.best_ask()
                                                  : book_.best_bid();
        if (lvl == nullptr) break;
        if (has_limit &&
            !crosses(taker.side, limit_ticks, lvl->price_ticks)) {
            break;
        }
        Order* maker = lvl->head;
        if (maker == nullptr) break;  // structural divergence — bail

        // Self-trade prevention: same account, or same nonzero STP group
        // (the maker's group resolves through the meta index).
        const bool self =
            maker->account_id == taker.account_id ||
            (aux.trade_group_id != 0 &&
             aux.trade_group_id == group_of(maker->id));
        if (self) {
            const StpAction a = SelfTradeGuard::action(taker.stp_mode);
            (void)apply_stp(maker, a, res.remaining, res);
            continue;  // re-read best level — maker may be gone
        }

        const int64_t m_rem = remaining_qty_units(*maker);
        const int64_t fill =
            res.remaining < m_rem ? res.remaining : m_rem;
        const int64_t px = lvl->price_ticks;
        const uint64_t maker_id = maker->id;
        const uint64_t tid = next_trade_id_++;
        const uint64_t buy_id =
            taker.side == Side::BUY ? taker.id : maker_id;
        const uint64_t sell_id =
            taker.side == Side::BUY ? maker_id : taker.id;

        // WAL BEFORE mutation; publish after.
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
        if (publisher_ != nullptr) {
            (void)publisher_->publish_trade(tid, buy_id, sell_id, px, fill,
                                      book_.book_seq(), now_ns_);
        }

        // Iceberg maker: account the fill against the record; when the
        // visible slice died, refresh the next slice at the level tail.
        if (auto* rec = icebergs_.find(maker_id)) {
            rec->filled_total_units += fill;
            if (book_.find_order(maker_id) == nullptr) {
                replenish_iceberg(rec);
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
    // Fresh priority stamp >= any surviving order at this level (the slice
    // lands at the FIFO tail regardless; stamps stay non-decreasing for the
    // book's structural validator).
    uint64_t tail_ts = 0;
    for (uint32_t d = 0; ; ++d) {
        const PriceLevel* l = book_.level(tmpl.side, d);
        if (l == nullptr) break;
        if (l->price_ticks == tmpl.price_ticks && l->tail != nullptr) {
            tail_ts = l->tail->timestamp_ns;
            break;
        }
    }
    uint64_t ts = now_ns_ > tmpl.timestamp_ns ? now_ns_ : tmpl.timestamp_ns;
    if (tail_ts > ts) ts = tail_ts;
    tmpl.timestamp_ns = ts;
    tmpl.ingress_seq = ++emit_seq_;

    Order* node = nullptr;
    const BookError e = book_.add_order(tmpl, &node);
    if (e != BookError::OK) {
        // Fail-closed: the hidden remainder cannot rest -> terminal cancel.
        icebergs_.erase(rid);
        meta_erase(rid);
        emit_cancel_event(rid, account, kWalCancelReasonUser);
        last_reject_ = kRejectBookCapacity;
        ++reject_count_;
    }
    // No WAL entry — the refresh re-derives from logged TRADE/ORDER_NEW.
}

// ---------------------------------------------------------------------------
// FOK feasibility — mirrors walk_match exactly, zero mutation
// ---------------------------------------------------------------------------

bool MatchingEngine::fok_feasible(const Order& taker, uint32_t taker_group,
                                  bool has_limit,
                                  int64_t limit_ticks) const noexcept {
    int64_t rem = remaining_qty_units(taker);
    const Side opp = taker.side == Side::BUY ? Side::SELL : Side::BUY;
    for (std::size_t d = 0; rem > 0; ++d) {
        const PriceLevel* lvl = book_.level(opp, d);
        if (lvl == nullptr) break;
        if (has_limit &&
            !crosses(taker.side, limit_ticks, lvl->price_ticks)) {
            break;
        }
        for (const Order* m = lvl->head; m != nullptr && rem > 0;
             m = m->next) {
            const bool self =
                m->account_id == taker.account_id ||
                (taker_group != 0 && taker_group == group_of(m->id));
            if (self) {
                const StpAction a = SelfTradeGuard::action(taker.stp_mode);
                if (a == StpAction::CANCEL_MAKER) continue;  // maker skipped
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
        emit_cancel_event(taker.id, taker.account_id, kWalCancelReasonUser);
        last_reject_ = kRejectBookCapacity;
        ++reject_count_;
        return;
    }
    if (r.dead) {
        emit_cancel_event(taker.id, taker.account_id, r.dead_reason);
        if (r.dead_code != nullptr) {
            last_reject_ = r.dead_code;
            ++reject_count_;
        }
        return;
    }
    if (r.remaining <= 0) return;  // fully filled

    const bool never_rests = taker.type == OrderType::MARKET ||
                             taker.type == OrderType::STOP ||
                             taker.tif == TimeInForce::IOC ||
                             taker.tif == TimeInForce::FOK;
    if (never_rests) {
        emit_cancel_event(
            taker.id, taker.account_id,
            taker.tif == TimeInForce::FOK ? kWalCancelReasonFokUnfilled
                                        : kWalCancelReasonIocRemainder);
        if (taker.tif == TimeInForce::FOK) {
            last_reject_ = kRejectFokUnfilled;
            ++reject_count_;
        }
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
                              kWalCancelReasonUser);
            last_reject_ = kRejectBookCapacity;
            ++reject_count_;
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
        emit_cancel_event(taker.id, taker.account_id, kWalCancelReasonUser);
        last_reject_ = e == BookError::DUPLICATE_ID ? kRejectDuplicateId
                                                  : kRejectBookCapacity;
        ++reject_count_;
        return;
    }

    // Per-order aux tracking (STP group / GTD-DAY expiry) — fail-closed: if
    // tracking cannot be allocated the rest is undone and the order dies.
    if (!track_meta(taker.id, aux)) {
        (void)book_.cancel_order(taker.id);
        if (rec != nullptr) icebergs_.erase(taker.id);
        emit_cancel_event(taker.id, taker.account_id, kWalCancelReasonUser);
        last_reject_ = kRejectBookCapacity;
        ++reject_count_;
    }
}

// ---------------------------------------------------------------------------
// stop queue drain — triggered entries process in pop order, then re-poll
// (their own fills may trigger further stops)
// ---------------------------------------------------------------------------

void MatchingEngine::drain_triggers() noexcept {
    if (wal_fault_) return;  // unjournaled triggers cannot be activated
    for (;;) {
        Order* chain = stops_.pop_triggered(last_price_ticks_);
        if (chain == nullptr) break;
        for (Order* n = chain; n != nullptr;) {
            Order* next = n->next;
            n->next = nullptr;
            // Rebuild aux from the meta index (group/expiry tracked at
            // enqueue) — pending stops carry no OrderAux of their own.
            OrderAux aux{};
            if (const OrderMeta* m = meta_find(n->id)) {
                aux.trade_group_id = m->trade_group_id;
                aux.gtd_expiry_ns = m->expiry_ns;
            }
            process_triggered(n, aux);
            n = next;
        }
    }
}

void MatchingEngine::process_triggered(Order* node,
                                       const OrderAux& aux) noexcept {
    // STOP -> market semantics (sweep to empty, remainder cancelled).
    // STOP_LIMIT -> limit at price_ticks; tif governs the remainder.
    const bool has_limit = node->type == OrderType::STOP_LIMIT;

    if (node->tif == TimeInForce::FOK &&
        !fok_feasible(*node, aux.trade_group_id, has_limit,
                      node->price_ticks)) {
        emit_cancel_event(node->id, node->account_id,
                          kWalCancelReasonFokUnfilled);
        last_reject_ = kRejectFokUnfilled;
        ++reject_count_;
    } else {
        TakerResult r = walk_match(*node, aux, has_limit,
                                   node->price_ticks);
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
            default:
                invalid = kRejectOrderInvalid;  // type unsupported here
        }
    }
    if (invalid == nullptr &&
        (order->tif == TimeInForce::GTD || order->tif == TimeInForce::DAY) &&
        aux.gtd_expiry_ns <= 0) {
        // DAY orders arrive with their session close precomputed into
        // aux.gtd_expiry_ns by the gateway (deterministic engine clock).
        invalid = kRejectOrderInvalid;
    }

    if (invalid != nullptr) {
        reject(order, invalid);
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

    // --- WAL: acceptance is the first state change ---------------------------
    // Iceberg display slice is resolved once here so the WAL payload and the
    // resting slice agree.
    int64_t display = order->qty_units;
    if (order->type == OrderType::ICEBERG) {
        display = IcebergManager::visible_slice(order->qty_units,
                                                order->display_qty_units,
                                                book_.instrument());
    }
    if (wal_ != nullptr &&
        wal_->write_order_new(*order, aux, instrument_id_of(aux), display,
                              now_ns_) != WalStatus::Ok) {
        wal_fault_ = true;
        reject(order, kRejectBookCapacity);
        orders_.free(order);
        return;
    }

    // --- dispatch ------------------------------------------------------------
    bool adopted = false;  // set when the stop queue takes ownership
    switch (order->type) {
        case OrderType::LIMIT:
        case OrderType::ICEBERG: {
            if (order->tif == TimeInForce::FOK &&
                !fok_feasible(*order, aux.trade_group_id, true,
                              order->price_ticks)) {
                emit_cancel_event(order->id, order->account_id,
                                  kWalCancelReasonFokUnfilled);
                last_reject_ = kRejectFokUnfilled;
                ++reject_count_;
                break;
            }
            TakerResult r = walk_match(*order, aux, true,
                                       order->price_ticks);
            finish_taker(*order, aux, r, display);
            break;
        }
        case OrderType::MARKET: {
            if (order->tif == TimeInForce::FOK &&
                !fok_feasible(*order, aux.trade_group_id, false, 0)) {
                emit_cancel_event(order->id, order->account_id,
                                  kWalCancelReasonFokUnfilled);
                last_reject_ = kRejectFokUnfilled;
                ++reject_count_;
                break;
            }
            TakerResult r = walk_match(*order, aux, false, 0);
            finish_taker(*order, aux, r, 0);
            break;
        }
        case OrderType::STOP:
        case OrderType::STOP_LIMIT: {
            // Already-crossed stops trigger immediately instead of queueing.
            const bool due =
                last_price_ticks_ > 0 &&
                (order->side == Side::BUY
                     ? last_price_ticks_ >= aux.stop_price_ticks
                     : last_price_ticks_ <= aux.stop_price_ticks);
            if (due) {
                process_triggered(order, aux);
                break;
            }
            // Track group/expiry alongside the pending stop.
            if (!track_meta(order->id, aux)) {
                emit_cancel_event(order->id, order->account_id,
                                  kWalCancelReasonUser);
                last_reject_ = kRejectBookCapacity;
                ++reject_count_;
                break;
            }
            if (!stops_.enqueue(order, aux.stop_price_ticks)) {
                meta_erase(order->id);
                emit_cancel_event(order->id, order->account_id,
                                  kWalCancelReasonUser);
                last_reject_ = kRejectBookCapacity;
                ++reject_count_;
                break;
            }
            adopted = true;
            break;
        }
        default:
            break;  // unreachable — validated above
    }

    if (!adopted) orders_.free(order);
    drain_triggers();
    if (publisher_ != nullptr && book_.book_seq() != seq0) publish_depth();
}

// ---------------------------------------------------------------------------
// cancel / amend / tick
// ---------------------------------------------------------------------------

void MatchingEngine::on_cancel_received(uint64_t order_id,
                                        uint64_t account_id) noexcept {
    const uint64_t seq0 = book_.book_seq();
    if (!cancel_internal(order_id, account_id, kWalCancelReasonUser, true)) {
        // Absent or foreign order — idempotent no-op, never a second
        // mutation (spec §24 #10).
        last_reject_ = kRejectUnknownOrder;
        ++reject_count_;
    }
    drain_triggers();
    if (publisher_ != nullptr && book_.book_seq() != seq0) publish_depth();
}

void MatchingEngine::on_amend_received(uint64_t order_id,
                                       int64_t new_price_ticks,
                                       int64_t new_qty_units,
                                       int64_t new_stop_price_ticks,
                                       uint64_t ingress_seq) noexcept {
    const uint64_t seq0 = book_.book_seq();
    if (wal_fault_) {
        last_reject_ = kRejectBookCapacity;
        ++reject_count_;
        return;
    }

    // Pending stop order?
    if (auto* p = stops_.find(order_id)) {
        const int64_t price =
            new_price_ticks > 0 ? new_price_ticks : p->order->price_ticks;
        const int64_t qty =
            new_qty_units > 0 ? new_qty_units : p->order->qty_units;
        const int64_t stop = new_stop_price_ticks > 0
                                 ? new_stop_price_ticks
                                 : p->stop_price_ticks;
        if (qty <= 0 ||
            (p->order->type == OrderType::STOP_LIMIT && price <= 0) ||
            stop <= 0) {
            last_reject_ = kRejectOrderInvalid;
            ++reject_count_;
            return;
        }
        if (wal_ != nullptr &&
            wal_->write_order_modify(order_id, price, qty, stop, now_ns_) !=
                WalStatus::Ok) {
            wal_fault_ = true;
            last_reject_ = kRejectBookCapacity;
            ++reject_count_;
            return;
        }
        p->order->qty_units = qty;
        p->order->quantity = Decimal::from_mantissa(qty);
        if (p->order->type == OrderType::STOP_LIMIT) {
            p->order->price_ticks = price;
        }
        if (stop != p->stop_price_ticks) {
            p->stop_price_ticks = stop;
            stops_.resort(p->order);
        }
        return;
    }

    Order* o = book_.find_order(order_id);
    if (o == nullptr) {
        last_reject_ = kRejectUnknownOrder;
        ++reject_count_;
        return;
    }
    if (o->tif == TimeInForce::IOC || o->tif == TimeInForce::FOK) {
        last_reject_ = kRejectOrderInvalid;  // NOT_MODIFIABLE
        ++reject_count_;
        return;
    }
    const int64_t price =
        new_price_ticks > 0 ? new_price_ticks : o->price_ticks;
    if (price <= 0) {
        last_reject_ = kRejectOrderInvalid;
        ++reject_count_;
        return;
    }

    // Iceberg: amend quantity is the order TOTAL; the live slice is resized
    // and the hidden remainder absorbs the difference.
    auto* rec = icebergs_.find(order_id);
    int64_t qty;
    int64_t want_total = 0;
    if (rec != nullptr) {
        want_total =
            new_qty_units > 0 ? new_qty_units : rec->total_qty_units;
        if (want_total <= rec->filled_total_units) {
            last_reject_ = kRejectOrderInvalid;
            ++reject_count_;
            return;
        }
        const int64_t want_rem = want_total - rec->filled_total_units;
        qty = want_rem < rec->display_qty_units ? want_rem
                                                : rec->display_qty_units;
    } else {
        qty = new_qty_units > 0 ? new_qty_units : o->qty_units;
    }
    if (qty <= o->filled_qty_units) {
        last_reject_ = kRejectOrderInvalid;
        ++reject_count_;
        return;
    }

    // Pre-validate the lose-priority path's guards (CROSSED / LEVEL_CAPACITY)
    // so WAL-first ordering stays honest — the journaled modify will apply.
    const bool repriced = price != o->price_ticks;
    const bool qty_up = qty > o->qty_units;
    if (repriced || qty_up) {
        const PriceLevel* opposite =
            o->side == Side::BUY ? book_.best_ask() : book_.best_bid();
        if (opposite != nullptr &&
            crosses(o->side, price, opposite->price_ticks)) {
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
                const PriceLevel* l = book_.level(o->side, d);
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
                    const PriceLevel* l = book_.level(o->side, d);
                    if (l == nullptr) break;
                    if (l->price_ticks == o->price_ticks &&
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

    if (wal_ != nullptr &&
        wal_->write_order_modify(order_id, price,
                                 rec != nullptr ? want_total : qty, 0,
                                 now_ns_) != WalStatus::Ok) {
        wal_fault_ = true;
        last_reject_ = kRejectBookCapacity;
        ++reject_count_;
        return;
    }
    const uint64_t ts = now_ns_ > o->timestamp_ns ? now_ns_ : o->timestamp_ns;
    const BookError e = book_.modify_order(order_id, price, qty, ts,
                                           ingress_seq);
    if (e != BookError::OK) {
        wal_fault_ = true;  // unreachable per preflight — fail closed
        return;
    }
    if (rec != nullptr) {
        rec->total_qty_units = want_total;
        rec->tmpl.price_ticks = price;
    }
    if (publisher_ != nullptr && book_.book_seq() != seq0) publish_depth();
}

void MatchingEngine::on_time_tick(uint64_t now_ns) noexcept {
    const uint64_t seq0 = book_.book_seq();
    if (wal_ != nullptr &&
        wal_->write_time_tick(now_ns) != WalStatus::Ok) {
        wal_fault_ = true;  // unjournaled clock must not drive mutations
        return;
    }
    if (now_ns > now_ns_) now_ns_ = now_ns;  // monotone logical clock
    expire_due();
    drain_triggers();
    if (publisher_ != nullptr && book_.book_seq() != seq0) publish_depth();
}

}  // namespace exch
