#pragma once

// Task 2.3.2 — conditional-trigger queue for STOP / STOP_LIMIT orders
// (spec §3.2 #7, §3.7.5). Pending stops live OUTSIDE the resting book: the
// component owns two price-sorted intrusive chains of pool-allocated Order
// nodes — buy-stops ascending by stop_price (trigger when the evaluation
// reference >= stop), sell-stops descending (trigger when ref <= stop) —
// plus an open-addressed order_id index for O(1) cancel/amend.
//
// Phase-16 extensions (Tasks 16.3.3/15/16/17, spec §6.2a):
//   * Every pending carries a trigger_source (kTriggerSourceLast/Mark/
//     Index) — pop_triggered() resolves each entry against that source's
//     reference price; an unavailable/stale reference (value <= 0) freezes
//     evaluation for that source fail-closed (CONDITIONAL_TRIGGER_
//     ORACLE_STALE semantics — the order simply stays pending).
//   * TRAILING_STOP pendings carry {trail_unit, trail_distance, anchor,
//     activation_price} and ride a third intrusive chain (Order::hash_next
//     — unused while the node is not in the book index) so the engine's
//     re-anchor pass walks only trailing entries.
//   * pop_triggered() records a TriggeredInfo per detached node (armed
//     threshold + the observed reference + source) — the engine stamps it
//     into the ORDER_TRIGGERED WAL row and uses stop_price for GSLO fills.
//
// Trigger evaluation is deterministic: pop_triggered(refs) detaches every
// due entry into a single chain ordered by (timestamp_ns, ingress_seq) —
// the same order replay produces under identical input. Triggered entries
// leave the queue permanently (they run through the engine's normal
// market/limit taker path or the GSLO guaranteed-fill path).
//
// Storage: one cold-allocated table at construction; zero heap traffic on
// the hot path; a full table is fail-closed (enqueue returns false and the
// engine rejects the order rather than dropping the trigger silently).
// Order nodes are adopted from the shard order pool — enqueue takes
// ownership; remove()/pop_triggered() hand ownership back to the caller.

#include <cstddef>
#include <cstdint>
#include <new>

#include "book/Order.hpp"

namespace exch {

class StopOrderTrigger {
public:
    // capacity bounds simultaneously-pending stops (<= order pool size).
    explicit StopOrderTrigger(
        std::size_t capacity = kDefaultCapacity) noexcept {
        if (capacity == 0) capacity = 1;
        capacity_ = next_pow2(capacity * 2);  // open addressing, load <= 0.5
        slots_ = new (std::nothrow) Pending[capacity_]();
        infos_ = new (std::nothrow) TriggeredInfo[capacity_]();
        if (slots_ == nullptr || infos_ == nullptr) capacity_ = 0;  // fail-closed
    }
    ~StopOrderTrigger() {
        delete[] slots_;
        delete[] infos_;
    }

    StopOrderTrigger(const StopOrderTrigger&) = delete;
    StopOrderTrigger& operator=(const StopOrderTrigger&) = delete;

    struct Pending {
        uint64_t order_id = 0;         // key; 0 = empty
        Order*   order = nullptr;      // pool-owned node, chained next/prev
        int64_t  stop_price_ticks = 0; // armed trigger threshold
        // --- Phase-16 conditional metadata ---------------------------------
        int64_t  anchor_ticks = 0;            // trailing: most-favorable ref
        int64_t  activation_price_ticks = 0;  // trailing gate; 0 = armed
        int64_t  trail_distance = 0;          // pips | pct*100 | ticks
        int64_t  gslo_notional_units = 0;     // GSLO exposure accounting
        uint8_t  trigger_source = 0;          // kTriggerSource*
        uint8_t  trail_unit = 0;              // kTrailUnit*; 0 = plain stop
        uint8_t  armed = 1;                   // trailing gate passed
    };

    // Per-source evaluation references handed to pop_triggered(). A value
    // <= 0 means "unavailable" — entries on that source simply do not
    // trigger (fail-closed freeze; spec §6.2a/§24 #255).
    struct TriggerRefs {
        int64_t last = 0;
        int64_t mark = 0;
        int64_t index = 0;
        [[nodiscard]] constexpr int64_t ref(uint8_t source) const noexcept {
            return source == kTriggerSourceMark  ? mark
                 : source == kTriggerSourceIndex ? index
                                                 : last;
        }
    };

    // Metadata captured for each detached node by pop_triggered, in chain
    // order — ORDER_TRIGGERED payload + GSLO execution price source.
    struct TriggeredInfo {
        uint64_t order_id = 0;
        int64_t  stop_ticks = 0;
        int64_t  observed_ticks = 0;
        int64_t  gslo_notional_units = 0;  // engine releases on terminal
        uint8_t  source = 0;
    };

    static constexpr std::size_t kDefaultCapacity = 1u << 15;  // 32k pending

    // Adopt a pending stop. order->side decides the queue: BUY triggers on
    // ref >= stop (price rallies into the stop), SELL on ref <= stop.
    // false = table full or duplicate id — caller cancels/rejects.
    [[nodiscard]] bool enqueue(Order* order,
                               int64_t stop_price_ticks) noexcept {
        return enqueue(order, stop_price_ticks, Pending{});
    }

    // Phase-16 form: `meta` supplies trigger_source plus the trailing
    // fields (trail_unit/anchor/activation/armed) — ignored for plain
    // stops. Orders with trail_unit != 0 additionally ride the trail chain.
    [[nodiscard]] bool enqueue(Order* order, int64_t stop_price_ticks,
                               const Pending& meta) noexcept {
        if (slots_ == nullptr || order == nullptr ||
            live_ * 2 >= capacity_) {
            return false;
        }
        std::size_t i = bucket(order->id);
        while (slots_[i].order_id != 0) {
            if (slots_[i].order_id == order->id) return false;  // dup
            i = (i + 1) & (capacity_ - 1);
        }
        slots_[i].order_id = order->id;
        slots_[i].order = order;
        slots_[i].stop_price_ticks = stop_price_ticks;
        slots_[i].anchor_ticks = meta.anchor_ticks;
        slots_[i].activation_price_ticks = meta.activation_price_ticks;
        slots_[i].trail_distance = meta.trail_distance;
        slots_[i].gslo_notional_units = meta.gslo_notional_units;
        slots_[i].trigger_source = meta.trigger_source;
        slots_[i].trail_unit = meta.trail_unit;
        slots_[i].armed = meta.armed;
        if (slots_[i].trigger_source <= kTriggerSourceIndex) {
            ++source_live_[slots_[i].trigger_source];
        }
        if (meta.trail_unit != kTrailUnitNone) {
            order->hash_next = trail_head_;
            trail_head_ = order;
            ++trail_live_;
        }
        ++live_;
        order->next = order->prev = nullptr;
        insert_sorted(order, stop_price_ticks);
        return true;
    }

    // Unlink + drop the pending record; returns the pool node (caller frees
    // or reuses it). nullptr when not pending — cancel stays idempotent.
    [[nodiscard]] Order* remove(uint64_t order_id) noexcept {
        Pending* p = find_slot(order_id);
        if (p == nullptr) return nullptr;
        Order* o = p->order;
        if (p->trail_unit != kTrailUnitNone) unlink_trail(o);
        unlink_sorted(o);
        erase_slot(order_id);
        return o;
    }

    [[nodiscard]] bool pending(uint64_t order_id) const noexcept {
        return find_slot(order_id) != nullptr;
    }
    // Mutable access for the amend/re-anchor path: caller may rewrite the
    // Pending record (stop_price_ticks, anchor, armed, trail fields, GSLO
    // notional) then MUST call resort(order) to restore queue order.
    [[nodiscard]] Pending* find(uint64_t order_id) noexcept {
        return find_slot(order_id);
    }
    // Re-sort after an in-place stop_price edit (unlink + sorted insert).
    void resort(Order* order) noexcept {
        if (order == nullptr) return;
        const Pending* p = find_slot(order->id);
        const int64_t stop = p != nullptr ? p->stop_price_ticks : 0;
        unlink_sorted(order);
        insert_sorted(order, stop);
    }

    // Walk the trailing chain (hash_next links) invoking f(order, pending)
    // for every pending with trail_unit != 0. The callback may rewrite the
    // Pending record in place — but MUST NOT remove() or enqueue() (chain
    // links are captured before each call).
    template <typename F>
    void for_each_trailing(F&& f) noexcept {
        for (Order* n = trail_head_; n != nullptr;) {
            Order* next = n->hash_next;
            Pending* p = find_slot(n->id);
            if (p != nullptr) f(n, p);
            n = next;
        }
    }
    [[nodiscard]] std::size_t trail_live() const noexcept { return trail_live_; }

    // Pending count per trigger source — the engine consults this to decide
    // whether oracle staleness actually gates anything (an empty MARK
    // source must not freeze the whole pipeline).
    [[nodiscard]] std::size_t source_live(uint8_t source) const noexcept {
        return source <= kTriggerSourceIndex ? source_live_[source] : 0;
    }

    // Detach every triggered entry into one chain via Order::next.
    // Ordering contract: per side the due set preserves the sorted queue
    // (best stop first); the two sides merge by (timestamp_ns, ingress_seq)
    // — identical to the book's priority key.
    //
    // Each entry's Pending.trigger_source selects its reference from
    // `refs`; a source whose reference is <= 0 freezes only ITS entries
    // (fail-closed; spec §6.2a/§24 #255). Because the queues mix sources,
    // a frozen head must never shadow a due entry behind it — the pop
    // therefore scans each side chain once (O(pending), allocation-free)
    // instead of checking heads alone.
    //
    // TriggeredInfo rows are recorded per detached node (keyed by id —
    // consume via take_triggered); pop_count() bounds the array until the
    // next pop_triggered() call.
    [[nodiscard]] Order* pop_triggered(int64_t last_price_ticks) noexcept {
        return pop_triggered(TriggerRefs{last_price_ticks, 0, 0});
    }
    [[nodiscard]] Order* pop_triggered(const TriggerRefs& refs) noexcept {
        pop_count_ = 0;
        // Phase A: detach each side's due entries into a per-side chain.
        Order* due[2] = {nullptr, nullptr};
        for (int sd = 0; sd < 2; ++sd) {
            Order* n = sd == 0 ? buy_head_ : sell_head_;
            Order* dt = nullptr;
            while (n != nullptr) {
                Order* nxt = n->next;  // capture before relink/erase
                Pending* p = find_slot(n->id);
                const int64_t r =
                    p != nullptr ? refs.ref(p->trigger_source) : 0;
                const bool is_due =
                    p != nullptr && p->armed != 0 && r > 0 &&
                    (sd == 0 ? r >= p->stop_price_ticks
                             : r <= p->stop_price_ticks);
                if (is_due) {
                    // Pop metadata BEFORE erase_slot releases the record.
                    if (pop_count_ < capacity_) {
                        infos_[pop_count_++] = TriggeredInfo{
                            n->id, p->stop_price_ticks, r,
                            p->gslo_notional_units, p->trigger_source};
                    }
                    if (p->trail_unit != kTrailUnitNone) unlink_trail(n);
                    unlink_sorted(n);
                    erase_slot(n->id);  // pending index -> remove() misses
                    n->next = n->prev = nullptr;
                    if (dt != nullptr) dt->next = n; else due[sd] = n;
                    dt = n;
                }
                n = nxt;
            }
        }
        // Phase B: merge by (timestamp_ns, ingress_seq) — earlier wins.
        Order* head = nullptr;
        Order* tail = nullptr;
        while (due[0] != nullptr || due[1] != nullptr) {
            Order* pick;
            if (due[0] == nullptr)       pick = due[1];
            else if (due[1] == nullptr)  pick = due[0];
            else pick = earlier(due[0], due[1]) ? due[0] : due[1];
            Order*& dq = pick->side == Side::BUY ? due[0] : due[1];
            dq = pick->next;
            pick->next = nullptr;
            if (tail != nullptr) tail->next = pick; else head = pick;
            tail = pick;
        }
        return head;
    }

    // Consume the pop metadata captured for order_id (engine reads it
    // exactly once per activated node — the row is single-read). Returns
    // false when the node predates metadata capture (defensive dedup).
    bool take_triggered(uint64_t order_id, TriggeredInfo* out) noexcept {
        for (uint32_t i = 0; i < pop_count_; ++i) {
            if (infos_[i].order_id != order_id) continue;
            if (out != nullptr) *out = infos_[i];
            infos_[i].order_id = 0;  // consumed — single-read discipline
            return true;
        }
        return false;
    }

    // Pop metadata from the most recent pop_triggered() call, in chain
    // order (pop_info()[i] describes the i-th node on the returned chain).
    [[nodiscard]] const TriggeredInfo* pop_infos() const noexcept {
        return infos_;
    }
    [[nodiscard]] uint32_t pop_count() const noexcept { return pop_count_; }

    [[nodiscard]] std::size_t size() const noexcept { return live_; }
    [[nodiscard]] bool empty() const noexcept { return live_ == 0; }

    // Cold-path enumeration (snapshot export): invoke f(order, pending) for
    // every live entry. Traversal order is slot order — restore re-sorts via
    // enqueue, so determinism is preserved.
    template <typename F>
    void for_each_pending(F&& f) const noexcept {
        for (std::size_t i = 0; i < capacity_; ++i) {
            if (slots_[i].order_id != 0) f(slots_[i].order, slots_[i]);
        }
    }

    // Replay→live adoption handoff: invoke f(order, pending) for every live
    // entry, then reset the queue wholesale — node ownership moves to the
    // caller. Only legal on a queue that will not be used again.
    template <typename F>
    void drain(F&& f) noexcept {
        for (std::size_t i = 0; i < capacity_; ++i) {
            if (slots_[i].order_id != 0) {
                f(slots_[i].order, slots_[i]);
                slots_[i] = Pending{};
            }
        }
        live_ = trail_live_ = 0;
        pop_count_ = 0;
        source_live_[0] = source_live_[1] = source_live_[2] = 0;
        buy_head_ = buy_tail_ = sell_head_ = sell_tail_ = nullptr;
        trail_head_ = nullptr;
    }

private:
    [[nodiscard]] std::size_t bucket(uint64_t id) const noexcept {
        return static_cast<std::size_t>(id * 0x9E3779B97F4A7C15ull) &
               (capacity_ - 1);
    }
    static std::size_t next_pow2(std::size_t n) noexcept {
        std::size_t p = 1;
        while (p < n) p <<= 1;
        return p;
    }
    [[nodiscard]] Pending* find_slot(uint64_t order_id) noexcept {
        if (slots_ == nullptr || live_ == 0) return nullptr;
        std::size_t i = bucket(order_id);
        for (;;) {
            if (slots_[i].order_id == 0) return nullptr;
            if (slots_[i].order_id == order_id) return &slots_[i];
            i = (i + 1) & (capacity_ - 1);
        }
    }
    [[nodiscard]] const Pending* find_slot(uint64_t order_id) const noexcept {
        return const_cast<StopOrderTrigger*>(this)->find_slot(order_id);
    }
    [[nodiscard]] int64_t stop_of(uint64_t order_id) const noexcept {
        const Pending* p = find_slot(order_id);
        return p != nullptr ? p->stop_price_ticks : 0;
    }
    void erase_slot(uint64_t order_id) noexcept {
        // Same probe-chain compaction as IcebergManager::erase.
        if (slots_ == nullptr) return;
        std::size_t i = bucket(order_id);
        for (;;) {
            if (slots_[i].order_id == 0) return;
            if (slots_[i].order_id == order_id) break;
            i = (i + 1) & (capacity_ - 1);
        }
        // Per-source bookkeeping: count before the slot is cleared.
        const uint8_t src = slots_[i].trigger_source;
        std::size_t j = i;
        for (;;) {
            j = (j + 1) & (capacity_ - 1);
            if (slots_[j].order_id == 0) break;
            const std::size_t home = bucket(slots_[j].order_id);
            if (((i - home) & (capacity_ - 1)) <
                ((j - home) & (capacity_ - 1))) {
                slots_[i] = slots_[j];
                i = j;
            }
        }
        slots_[i] = Pending{};
        if (src <= kTriggerSourceIndex && source_live_[src] > 0) {
            --source_live_[src];
        }
        --live_;
    }

    // FIFO tie-break between same-tick triggers on opposite sides:
    // (timestamp_ns, ingress_seq) — identical to the book's priority key.
    [[nodiscard]] static bool earlier(const Order* a, const Order* b) noexcept {
        if (a->timestamp_ns != b->timestamp_ns)
            return a->timestamp_ns < b->timestamp_ns;
        return a->ingress_seq < b->ingress_seq;
    }

    // Insert into the side's sorted chain: BUY ascending stop_price, SELL
    // descending; equal prices append after existing equals (FIFO).
    void insert_sorted(Order* order, int64_t stop_price_ticks) noexcept {
        const bool buy = order->side == Side::BUY;
        Order*& head = buy ? buy_head_ : sell_head_;
        Order*& tail = buy ? buy_tail_ : sell_tail_;
        Order* pos = head;
        while (pos != nullptr) {
            const int64_t ps = stop_of(pos->id);
            // For buys: stop before positions with a HIGHER stop; for sells:
            // before positions with a LOWER stop. Equal -> keep walking
            // (stable FIFO within one stop price).
            const bool advance =
                buy ? !(ps > stop_price_ticks) : !(ps < stop_price_ticks);
            if (!advance) break;
            pos = pos->next;
        }
        order->next = pos;
        order->prev = pos != nullptr ? pos->prev : tail;
        if (pos != nullptr) {
            pos->prev = order;
            if (pos == head) head = order;
        } else {
            tail = order;
        }
        if (order->prev != nullptr) order->prev->next = order;
        else head = order;
    }

    void unlink_sorted(Order* order) noexcept {
        const bool buy = order->side == Side::BUY;
        Order*& head = buy ? buy_head_ : sell_head_;
        Order*& tail = buy ? buy_tail_ : sell_tail_;
        if (order->prev != nullptr) order->prev->next = order->next;
        else if (head == order) head = order->next;
        if (order->next != nullptr) order->next->prev = order->prev;
        else if (tail == order) tail = order->prev;
        order->next = order->prev = nullptr;
    }

    void unlink_trail(Order* order) noexcept {
        Order** link = &trail_head_;
        while (*link != nullptr) {
            if (*link == order) {
                *link = order->hash_next;
                order->hash_next = nullptr;
                --trail_live_;
                return;
            }
            link = &(*link)->hash_next;
        }
    }

    Pending* slots_ = nullptr;
    TriggeredInfo* infos_ = nullptr;
    std::size_t capacity_ = 0;
    std::size_t live_ = 0;
    std::size_t trail_live_ = 0;
    std::size_t source_live_[3] = {0, 0, 0};
    uint32_t pop_count_ = 0;
    Order* buy_head_ = nullptr;   // ascending stop_price
    Order* buy_tail_ = nullptr;
    Order* sell_head_ = nullptr;  // descending stop_price
    Order* sell_tail_ = nullptr;
    Order* trail_head_ = nullptr; // trail_unit != 0 subset (hash_next links)
};

}  // namespace exch
