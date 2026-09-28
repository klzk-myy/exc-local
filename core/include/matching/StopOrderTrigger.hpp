#pragma once

// Task 2.3.2 — conditional-trigger queue for STOP / STOP_LIMIT orders
// (spec §3.2 #7, §3.7.5). Pending stops live OUTSIDE the resting book: the
// component owns two price-sorted intrusive chains of pool-allocated Order
// nodes — buy-stops ascending by stop_price (trigger when last >= stop),
// sell-stops descending (trigger when last <= stop) — plus an
// open-addressed order_id index for O(1) cancel/amend.
//
// Trigger evaluation is deterministic: pop_triggered(last_price) detaches
// every due entry into a single chain ordered by (timestamp_ns,
// ingress_seq) — the same order replay produces under identical input.
// Triggered entries leave the queue permanently (they run through the
// engine's normal market/limit taker path).
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
        if (slots_ == nullptr) capacity_ = 0;  // fail-closed
    }
    ~StopOrderTrigger() { delete[] slots_; }

    StopOrderTrigger(const StopOrderTrigger&) = delete;
    StopOrderTrigger& operator=(const StopOrderTrigger&) = delete;

    struct Pending {
        uint64_t order_id = 0;         // key; 0 = empty
        Order*   order = nullptr;      // pool-owned node, chained next/prev
        int64_t  stop_price_ticks = 0;
    };

    static constexpr std::size_t kDefaultCapacity = 1u << 15;  // 32k pending

    // Adopt a pending stop. order->side decides the queue: BUY triggers on
    // last >= stop (price rallies into the stop), SELL on last <= stop.
    // false = table full or duplicate id — caller cancels/rejects.
    [[nodiscard]] bool enqueue(Order* order,
                               int64_t stop_price_ticks) noexcept {
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
        unlink_sorted(o);
        erase_slot(order_id);
        return o;
    }

    [[nodiscard]] bool pending(uint64_t order_id) const noexcept {
        return find_slot(order_id) != nullptr;
    }
    // Mutable access for the amend path: caller may rewrite stop_price_ticks
    // then MUST call resort(order) to restore queue order.
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

    // Detach every triggered entry into one chain via Order::next,
    // ordered by (timestamp_ns, ingress_seq) across both sides.
    // last_price_ticks <= 0 means "no trade yet" — nothing can trigger.
    [[nodiscard]] Order* pop_triggered(int64_t last_price_ticks) noexcept {
        if (last_price_ticks <= 0) return nullptr;
        Order* head = nullptr;
        Order* tail = nullptr;
        for (;;) {
            Order* b = buy_head_;
            Order* s = sell_head_;
            const int64_t b_stop = b != nullptr ? stop_of(b->id) : 0;
            const int64_t s_stop = s != nullptr ? stop_of(s->id) : 0;
            const bool b_due = b != nullptr && last_price_ticks >= b_stop;
            const bool s_due = s != nullptr && last_price_ticks <= s_stop;
            if (!b_due && !s_due) break;
            Order* pick;
            if (b_due && s_due) {
                // Both heads due: earlier ingress wins (ts, then seq).
                pick = earlier(b, s) ? b : s;
            } else {
                pick = b_due ? b : s;
            }
            if (pick == b) {
                buy_head_ = b->next;
                if (buy_head_ != nullptr) buy_head_->prev = nullptr;
                else buy_tail_ = nullptr;
            } else {
                sell_head_ = s->next;
                if (sell_head_ != nullptr) sell_head_->prev = nullptr;
                else sell_tail_ = nullptr;
            }
            erase_slot(pick->id);   // leaves pending index -> remove() misses
            pick->next = pick->prev = nullptr;
            if (tail != nullptr) tail->next = pick; else head = pick;
            tail = pick;
        }
        return head;
    }

    [[nodiscard]] std::size_t size() const noexcept { return live_; }
    [[nodiscard]] bool empty() const noexcept { return live_ == 0; }

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

    Pending* slots_ = nullptr;
    std::size_t capacity_ = 0;
    std::size_t live_ = 0;
    Order* buy_head_ = nullptr;   // ascending stop_price
    Order* buy_tail_ = nullptr;
    Order* sell_head_ = nullptr;  // descending stop_price
    Order* sell_tail_ = nullptr;
};

}  // namespace exch
