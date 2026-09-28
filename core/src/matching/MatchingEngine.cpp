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
#include "utils/safe_math.hpp"

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
        if (has_limit &&
            !crosses(taker.side, limit_ticks, lvl->price_ticks)) {
            break;
        }
        // §22.2 execution collar (Task 2.3.17): the phase-entry CollarBounds
        // snapshot gates every maker — the first out-of-range level stops
        // the sweep and the remainder expires with the persisted reason.
        if (!ExecutionCollar::price_allowed(cb, lvl->price_ticks)) {
            res.dead = true;
            res.dead_reason = kWalCancelReasonExecRuleRange;
            res.dead_code = ExecutionCollar::kExpiryReason;
            break;
        }
        Order* maker = lvl->head;
        if (maker == nullptr) break;  // structural divergence — bail

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
        const int64_t px = lvl->price_ticks;
        const uint64_t maker_id = maker->id;
        const uint64_t maker_acct = maker->account_id;
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
        // §6.6b #3 price improvement (Task 2.3.22): delta vs the taker's
        // OWN limit (not the walk bound — a discretionary band improves
        // against the nominal limit; MARKET takers pass limit 0 -> delta 0).
        improvement_.on_fill(taker.side, taker.id, tid,
                             has_limit ? taker.price_ticks : 0, px, fill);
        if (publisher_ != nullptr) {
            (void)publisher_->publish_trade(tid, buy_id, sell_id, px, fill,
                                      book_.book_seq(), now_ns_);
        }

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
        if (book_.find_order(maker_id) == nullptr) meta_erase(maker_id);
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
                                  bool has_limit, int64_t limit_ticks,
                                  const CollarBounds& cb) const noexcept {
    int64_t rem = remaining_qty_units(taker);
    const Side opp = taker.side == Side::BUY ? Side::SELL : Side::BUY;
    for (std::size_t d = 0; rem > 0; ++d) {
        const PriceLevel* lvl = book_.level(opp, d);
        if (lvl == nullptr) break;
        if (has_limit &&
            !crosses(taker.side, limit_ticks, lvl->price_ticks)) {
            break;
        }
        // §22.2 collar mirror (Task 2.3.17): the feasibility verdict must
        // agree with the sweep — an out-of-range maker makes FOK
        // infeasible (the remainder would expire, not fill).
        if (!ExecutionCollar::price_allowed(cb, lvl->price_ticks)) {
            return false;
        }
        for (const Order* m = lvl->head; m != nullptr && rem > 0;
             m = m->next) {
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
        emit_cancel_event(taker.id, taker.account_id, kWalCancelReasonUser);
        last_reject_ = kRejectBookCapacity;
        ++reject_count_;
        meta_erase(taker.id);  // a prevented-qty slot may exist (2.3.18)
        return;
    }
    if (r.dead) {
        emit_cancel_event(taker.id, taker.account_id, r.dead_reason);
        if (r.dead_code != nullptr) {
            last_reject_ = r.dead_code;
            ++reject_count_;
        }
        meta_erase(taker.id);  // terminal — drop the prevented-qty slot
        return;
    }
    if (r.remaining <= 0) {
        meta_erase(taker.id);  // filled taker rests nothing — no meta to keep
        return;
    }

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
                              kWalCancelReasonUser);
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
        emit_cancel_event(taker.id, taker.account_id, kWalCancelReasonUser);
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
        emit_cancel_event(taker.id, taker.account_id, kWalCancelReasonUser);
        last_reject_ = kRejectBookCapacity;
        ++reject_count_;
        meta_erase(taker.id);
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
                              kWalCancelReasonUser);
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
                                  kWalCancelReasonSlippageExceeded);
                last_reject_ = kRejectSlippageExceeded;
                ++reject_count_;
            } else if (r.dead) {
                emit_cancel_event(node->id, node->account_id, r.dead_reason);
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
                          kWalCancelReasonFokUnfilled);
        last_reject_ = kRejectFokUnfilled;
        ++reject_count_;
    } else {
        TakerResult r = walk_match(*node, aux, has_limit,
                                   node->price_ticks, cb);
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
         order->type != OrderType::STOP_LIMIT);
    if (takes_now) {
        const bool opposite_empty =
            order->side == Side::BUY ? book_.ask_count() == 0
                                     : book_.bid_count() == 0;
        if (opposite_empty) {
            reject(order, kRejectNoLiquidity);
            orders_.free(order);
            return;
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
    if (order->type == OrderType::MARKET) {
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
            // The opposite side is non-empty per the gate above.
            const int64_t best = order->side == Side::BUY
                                     ? ask->price_ticks
                                     : bid->price_ticks;
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
                                      kWalCancelReasonUser);
                    last_reject_ = v.code;
                    ++reject_count_;
                    break;
                }
            }
            if (order->tif == TimeInForce::FOK &&
                !fok_feasible(*order, aux.trade_group_id, true,
                              walk_bound, cb)) {
                emit_cancel_event(order->id, order->account_id,
                                  kWalCancelReasonFokUnfilled);
                last_reject_ = kRejectFokUnfilled;
                ++reject_count_;
                break;
            }
            TakerResult r = walk_match(*order, aux, true, walk_bound, cb);
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
                                      kWalCancelReasonUser);
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
                                  kWalCancelReasonFokUnfilled);
                last_reject_ = kRejectFokUnfilled;
                ++reject_count_;
                break;
            }
            TakerResult r = walk_match(*order, aux, market_protected,
                                       protection_price_ticks, cb);
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
                                      kWalCancelReasonSlippageExceeded);
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
    if (!cancel_internal(order_id, account_id, kWalCancelReasonUser, true)) {
        // Absent or foreign order — idempotent no-op, never a second
        // mutation (spec §24 #10).
        last_reject_ = kRejectUnknownOrder;
        ++reject_count_;
    }
    drain_triggers();
    if (book_.book_seq() != seq0) {
        refresh_protected_quote();  // §6.6b #6 — internal book is the quote
        if (publisher_ != nullptr) publish_depth();
    }
}

// --- Task 2.3.13/2.3.15/2.3.20 helpers -----------------------------------------

const char* MatchingEngine::amend_state_gate() const noexcept {
    // §7.1 / Task 2.3.20 #5: amends reject in the auction/suspension states
    // — cancels stay allowed there (on_cancel_received is ungated). The
    // §5.1 InstrumentStatus enum has no CALL enumerator yet; when Phase-15
    // lands the auction phase it maps to kRejectAmendInAuction (constant is
    // already reserved). Per-state codes take precedence over the generic
    // ORDER_AMEND_REJECTED. A book without bound reference data (unit
    // tests) runs ungated.
    const Instrument* instr = book_.instrument();
    if (instr == nullptr) return nullptr;
    switch (instr->status) {
        case InstrumentStatus::CANCEL_ONLY:
            return kRejectInstrumentCancelOnly;
        case InstrumentStatus::SUSPENDED:
            return kRejectInstrumentSuspended;
        case InstrumentStatus::HALTED:
            return kRejectInstrumentHalted;
        case InstrumentStatus::DELISTED:
            return kRejectInstrumentDelisted;
        default:
            return nullptr;  // DRAFT/ACTIVE/RESTRICTED: amends permitted
    }
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
    if (qty <= 0 || stop <= 0 ||
        (s->type == OrderType::STOP_LIMIT && price <= 0) ||
        (instr != nullptr && instr->max_order_qty_units > 0 &&
         qty > instr->max_order_qty_units)) {
        last_reject_ = kRejectOrderInvalid;
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
    if (wal_ != nullptr &&
        wal_->write_time_tick(now_ns) != WalStatus::Ok) {
        wal_fault_ = true;  // unjournaled clock must not drive mutations
        return;
    }
    if (now_ns > now_ns_) now_ns_ = now_ns;  // monotone logical clock
    expire_due();
    drain_triggers();
    if (book_.book_seq() != seq0) {
        refresh_protected_quote();  // §6.6b #6 — internal book is the quote
        if (publisher_ != nullptr) publish_depth();
    }
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

}  // namespace exch
