#pragma once

// Task 2.3.10 — Time-in-Force expiry scheduler for GTD / DAY orders
// (spec §5.4 `time_in_force`, spec §6.7 24/5 session lifecycle).
//
// Standalone component owned by the matching thread. The engine
// (Task 2.3.2) wires it:
//   * on_order_accepted() — after an order is accepted to rest on the book
//     (GTD carries the explicit expiry; DAY is computed here; GTC/IOC/FOK are
//     no-ops — IOC/FOK lifetimes end inside matching, Task 2.3.2).
//   * on_order_done()     — when an order leaves the book (fill/cancel), so
//     its pending expiry registration is dropped. Unknown ids are a no-op.
//   * on_time_tick()      — driven ONLY by WalTimeTickPayload::tick_ns stamps
//     (EnginePump::tick stamps TIME_TICK WAL entries on the matching thread —
//     single WAL-writer invariant, remediation #4). This scheduler NEVER
//     reads a system clock: expiry order is a pure function of the WAL
//     stream, so replay reproduces the identical cancel sequence at any
//     replay speed (Phase-02 AC: "WAL replay of GTD expiry produces identical
//     cancellation sequence regardless of replay speed").
//   * The engine converts each drained id into ORDER_CANCEL with
//     WalOrderCancelPayload.reason = 1 (EXPIRED — GTD/DAY) → status EXPIRED →
//     Phase-06 `order_expired` WS event (this component is the producer).
//
// Expiry vs in-flight fill: serialized by the engine's single-threaded event
// order — an expired id is emitted exactly once, then removed; if the engine
// already completed the order, its cancel of the emitted id is a book no-op.
//
// DAY-expiry rule implemented (spec §6.7 + Task 2.3.10 "trading-day end"):
//   The FX trading week is ONE continuous session: Sunday 21:00:00 UTC open
//   → Friday 22:00:00 UTC close (24/5). Intra-week day boundaries are the
//   22:00 UTC instants (New York close) Sun..Fri; Friday 22:00 UTC is both a
//   day boundary and the weekly close.
//   * Accepted while the session is OPEN → expires at the next in-session
//     22:00 UTC boundary strictly after accept time: same-day 22:00 when the
//     accept precedes it (Mon 10:00 → Mon 22:00), else the next boundary
//     (Mon 23:00 → Tue 22:00; Sun 21:30 → Sun 22:00; Sun 22:30 → Mon 22:00;
//     Fri 21:59 → Fri 22:00). An order accepted exactly AT a boundary belongs
//     to the following boundary (Mon 22:00:00 → Tue 22:00).
//   * Accepted during the WEEKEND GAP [Fri 22:00, Sun 21:00) — including the
//     Sunday 20:45–21:00 pre-open: joins the upcoming trading week and
//     expires at that week's Friday 22:00 UTC close (spec §6.7 "DAY orders:
//     expire at 22:00 UTC Friday (end of trading day)"; Phase-02 AC "orders
//     spanning weekend close expire Friday 22:00 UTC"). The order rests
//     through the gap into the new week rather than dying at a Sunday
//     boundary — expiry is a session notion, never a closed-window event.
//   Pure function of now_ns; deterministic; no clock reads.
//
// Data structure: binary min-heap over (expiry_ns, order_id) in a flat
// fixed-capacity array + open-addressed order_id→slot index (pow2 linear
// probing, backward-shift deletion, splitmix64 mixing). All storage is
// allocated once at construction; register/remove is O(log n) worst case and
// tick-drain is O(k log n) for k due expiries with ZERO heap allocation.
//   * Equal expiries fire in ascending order_id — a deterministic total
//     order identical across WAL replays and snapshot restores.
//   * out-array overflow: when more than `cap` expiries are due, the cap
//     earliest are emitted and the rest stay pending for the next tick.
//   * Pending-set overflow is counted in register_overflows() — unreachable
//     when capacity >= live-order bound (default = kOrderPoolCapacity, the
//     per-shard order pool size).

#include <cstddef>
#include <cstdint>

#include "book/Order.hpp"  // TimeInForce wire ordinals, kOrderPoolCapacity

namespace exch {

class ExpiryScheduler {
   public:
    // Snapshot wire format (little-endian; restore_pending validates):
    //   header: magic u32 'EXPS' | version u32 | count u32 | _pad u32
    //           | reserved u64                                  (24 bytes)
    //   records[count]: expiry_ns u64 | order_id u64            (16 bytes each)
    // Records are emitted sorted by (expiry_ns, order_id) — the drain order —
    // so a restored scheduler emits byte-identical snapshots.
    static constexpr uint32_t kSnapshotMagic = 0x53505845u;  // "EXPS" LE
    static constexpr uint32_t kSnapshotVersion = 1u;
    static constexpr uint32_t kSnapshotHeaderBytes = 24u;
    static constexpr uint32_t kSnapshotRecordBytes = 16u;

    // `capacity` bounds simultaneously-pending expiries; defaults to the
    // per-shard order pool size so a full live-book can always register.
    // Allocates the heap array + id index once (nothrow — on allocation
    // failure the scheduler stays fail-closed empty: nothing registers,
    // register_overflows() counts every refused order).
    explicit ExpiryScheduler(std::size_t capacity = kOrderPoolCapacity) noexcept;
    ~ExpiryScheduler();

    ExpiryScheduler(const ExpiryScheduler&) = delete;
    ExpiryScheduler& operator=(const ExpiryScheduler&) = delete;
    ExpiryScheduler(ExpiryScheduler&&) = delete;
    ExpiryScheduler& operator=(ExpiryScheduler&&) = delete;

    // Registers an expiry. tif is the wire ordinal (proto TimeInForce ≡
    // book/Order.hpp TimeInForce). GTD uses gtd_expiry_ns verbatim
    // (<= 0 clamps to 0 → fires on the next tick — fail-closed; upstream
    // validation owns INVALID_EXPIRY_TIMESTAMP). DAY computes the expiry
    // from now_ns per the rule above. GTC/IOC/FOK: no-op.
    // Re-registering an existing order_id replaces its expiry — the
    // "GTD timers reset on any accepted amend" path (spec §6.6a) reuses
    // this entry point.
    void on_order_accepted(uint64_t order_id, uint8_t tif, int64_t gtd_expiry_ns,
                           uint64_t now_ns) noexcept;

    // Drops a pending registration (order filled / cancelled / triggered
    // away). No-op for unknown ids — safe to call unconditionally.
    void on_order_done(uint64_t order_id) noexcept;

    // Drains every pending expiry <= tick_ns into out_order_ids in ascending
    // (expiry_ns, order_id) order, removing them from the pending set;
    // returns the emitted count. tick_ns MUST be a WAL TIME_TICK stamp —
    // never a system clock. If more than `cap` expiries are due, the cap
    // earliest are emitted and the rest stay pending for the next tick.
    uint32_t on_time_tick(uint64_t tick_ns, uint64_t* out_order_ids, uint32_t cap) noexcept;

    // Earliest pending expiry, or UINT64_MAX when empty — the engine uses it
    // for tick scheduling / observability.
    [[nodiscard]] uint64_t next_expiry_ns() const noexcept;

    // Bytes snapshot_pending will write for the current pending set.
    [[nodiscard]] uint32_t snapshot_bytes_needed() const noexcept;

    // Serializes the pending set (sorted records) into buf; returns bytes
    // written, or 0 when buf is null or cap < snapshot_bytes_needed()
    // (nothing written). Cold path — WAL/recovery checkpoint support.
    uint32_t snapshot_pending(uint8_t* buf, uint32_t cap) const noexcept;

    // Rebuilds the pending set from a snapshot_pending image, replacing
    // current contents. Returns false on bad magic/version, truncated or
    // trailing length, over-capacity count, or duplicate order_id — on
    // failure the pending set is left EMPTY (fail-closed; the caller treats
    // restore failure as a recovery fault).
    bool restore_pending(const uint8_t* buf, uint32_t len) noexcept;

    // --- Pure calendar helpers (public for tests + engine diagnostics) ----
    // DAY-rule computation above; pure integer math on now_ns.
    [[nodiscard]] static uint64_t day_expiry_ns(uint64_t now_ns) noexcept;
    // True iff now_ns is inside the continuous session [Sun 21:00, Fri 22:00)
    // UTC. Sunday 20:45–21:00 pre-open counts as CLOSED for expiry purposes
    // (orders accepted there join the upcoming week).
    [[nodiscard]] static bool is_session_open(uint64_t now_ns) noexcept;

    [[nodiscard]] std::size_t size() const noexcept { return heap_size_; }
    [[nodiscard]] std::size_t capacity() const noexcept { return heap_cap_; }
    // Registrations refused because the pending set was full (or storage
    // allocation failed) — should stay 0 in production.
    [[nodiscard]] uint64_t register_overflows() const noexcept { return register_overflows_; }
    // Pending expiry for an id, UINT64_MAX if none — diagnostics/tests.
    [[nodiscard]] uint64_t pending_expiry_of(uint64_t order_id) const noexcept;

   private:
    struct Node {
        uint64_t expiry_ns;
        uint64_t order_id;
    };
    struct Slot {
        uint64_t order_id;  // valid iff heap_idx != kEmptySlot
        uint32_t heap_idx;
        uint32_t _pad;
    };
    static constexpr uint32_t kEmptySlot = ~uint32_t{0};
    static constexpr std::size_t kNoSlot = ~std::size_t{0};

    // Total order over (expiry_ns, order_id) — min-heap root = earliest.
    [[nodiscard]] static bool node_less(const Node& a, const Node& b) noexcept {
        return a.expiry_ns < b.expiry_ns || (a.expiry_ns == b.expiry_ns && a.order_id < b.order_id);
    }

    void sift_up(std::size_t i) noexcept;
    void sift_down(std::size_t i) noexcept;
    void swap_nodes(std::size_t i, std::size_t j) noexcept;
    void remove_at(std::size_t i) noexcept;

    // order_id → heap index: pow2 open addressing, linear probe,
    // backward-shift delete, splitmix64 mix (deterministic, no allocation).
    [[nodiscard]] static uint64_t mix(uint64_t v) noexcept;
    [[nodiscard]] std::size_t map_find(uint64_t order_id) const noexcept;
    // Inserts/updates; always succeeds — callers guarantee the table exists
    // and holds < heap_cap_ entries (table is sized ~2x that bound).
    void map_put(uint64_t order_id, uint32_t heap_idx) noexcept;
    void map_erase_at(std::size_t idx) noexcept;
    void map_repoint(uint64_t order_id, uint32_t heap_idx) noexcept;
    void clear() noexcept;

    Node* heap_ = nullptr;  // [heap_cap_] flat min-heap array
    std::size_t heap_cap_ = 0;
    std::size_t heap_size_ = 0;
    Slot* table_ = nullptr;  // pow2 index, ~2x heap_cap_ buckets
    std::size_t table_mask_ = 0;
    uint64_t register_overflows_ = 0;
};

}  // namespace exch
