#pragma once

// Task 2.3.2 — price-time priority matching orchestrator (spec §3.2, §3.7,
// §6.5). LIMIT / MARKET / STOP / STOP_LIMIT / ICEBERG over the Task 2.3.1
// flat-array book; FOK/IOC/GTC/GTD/DAY time-in-force; self-trade prevention
// via SelfTradeGuard; iceberg slices via IcebergManager; conditional orders
// via StopOrderTrigger.
//
// Pipeline per ingress event (spec §3.2):
//     validate -> risk hook -> WAL append -> mutate book -> IPC publish
// Every mutation is journaled BEFORE it is applied and published after —
// crash replay therefore sees a prefix-consistent stream.
//
// Ingress Order* lifecycle: EnginePump hands over a pool-owned node and the
// engine owns it from here — it is freed back to orders_ on every terminal
// path, or adopted by the stop queue while pending. The book always holds
// its own pooled copy (OrderBook::add_order is template-based).
//
// Determinism: the engine NEVER reads a wall clock. Logical time advances
// only via on_time_tick(now_ns) (Task 2.3.10 TIME_TICK contract); order
// timestamps arrive pre-stamped on the Order. Same input stream => same
// book, same WAL, same fills.
//
// Order-auxiliary data: the POD Order has no fields for stop_price /
// GTD/DAY expiry / trade_group_id (see WalOrderNewPayload). Ingress passes
// them alongside via OrderAux; the engine then tracks them per resting or
// pending order id in fixed-capacity open-addressed side tables allocated
// once at construction (zero heap on the hot path; a full table is a
// fail-closed BOOK_CAPACITY reject, never silent loss of tracking).
//
// Degradation: wal == nullptr and/or publisher == nullptr are legal (unit
// tests) — journal/publish steps no-op while matching still runs.
//
// Owner wiring: EnginePump's IEngineIngress is the inbound contract —
// on_order_received / on_cancel_received / on_time_tick are virtual here so
// the pump may drive the engine directly or via MatchingEngineIngress.

#include <cstddef>
#include <cstdint>

#include "book/OrderBook.hpp"
#include "ipc/EnginePump.hpp"
#include "matching/IcebergManager.hpp"
#include "matching/SelfTradeGuard.hpp"
#include "matching/StopOrderTrigger.hpp"
#include "matching/WalWriter.hpp"
#include "utils/MemoryPool.hpp"

namespace exch {

class IpcPublisher;

class MatchingEngine : public IEngineIngress {
public:
    // wal / publisher may be nullptr -> journal/publish degrade to no-ops.
    MatchingEngine(uint32_t shard_id, OrderBook& book,
                   MemoryPool<Order>& orders, WalWriter* wal = nullptr,
                   IpcPublisher* publisher = nullptr) noexcept;
    ~MatchingEngine() override;

    MatchingEngine(const MatchingEngine&) = delete;
    MatchingEngine& operator=(const MatchingEngine&) = delete;

    // --- Ingress (IEngineIngress; spec §3.2 entry points) --------------------

    // Order* is pool-owned and transfers to the engine (see file docstring).
    void on_order_received(Order* order) noexcept override;
    // Rich form — aux carries stop_price / GTD-DAY expiry / trade_group /
    // instrument_id (fields that have no slot on the Order POD).
    void on_order_received(Order* order, const OrderAux& aux) noexcept;
    // IEngineIngress aux-aware entry — forwards to the rich form.
    void on_order_received_ex(Order* order, const OrderAux& aux) noexcept override {
        on_order_received(order, aux);
    }
    // Idempotent: a cancel of an absent/expired/filled order is a no-op
    // (UNKNOWN_ORDER recorded, never a second mutation — spec §24 #10).
    void on_cancel_received(uint64_t order_id,
                            uint64_t account_id) noexcept override;
    // Minimal amend per WalOrderModifyPayload: <=0 fields mean "unchanged".
    void on_amend_received(uint64_t order_id, int64_t new_price_ticks,
                           int64_t new_qty_units, int64_t new_stop_price_ticks,
                           uint64_t ingress_seq) noexcept override;
    // Deterministic logical clock: WAL TIME_TICK -> GTD/DAY expiry sweep ->
    // stop re-check. Monotonic-guarded (a stale tick is journaled but does
    // not move the clock backwards).
    void on_time_tick(uint64_t now_ns) noexcept override;

    // --- Recovery / wiring hooks ---------------------------------------------

    // Trade ids are monotonic per engine; B2 recovery seeds the next id from
    // the WAL tail so post-replay trades continue the sequence.
    void seed_trade_id(uint64_t next_trade_id) noexcept {
        next_trade_id_ = next_trade_id;
    }
    // Pre-trade risk slot (Task 2.3.3). fn returns nullptr to accept, else a
    // static reject code string surfaced via last_reject(). Order& is
    // non-const: the checker stamps the resolved stp_mode (Task 2.3.21).
    // Default: accept everything.
    using risk_check_fn = const char* (*)(void* ctx, Order& order) noexcept;
    void set_risk_hook(risk_check_fn fn, void* ctx) noexcept {
        risk_fn_ = fn;
        risk_ctx_ = ctx;
    }

    // --- Introspection --------------------------------------------------------

    [[nodiscard]] uint32_t shard_id() const noexcept { return shard_id_; }
    [[nodiscard]] uint64_t book_seq() const noexcept { return book_.book_seq(); }
    [[nodiscard]] uint64_t received_count() const noexcept { return received_count_; }
    [[nodiscard]] uint64_t trades_emitted() const noexcept { return trades_emitted_; }
    [[nodiscard]] uint64_t next_trade_id() const noexcept { return next_trade_id_; }
    [[nodiscard]] uint64_t last_price_ticks() const noexcept { return last_price_ticks_; }
    [[nodiscard]] uint64_t now_ns() const noexcept { return now_ns_; }
    // Address of the logical clock — EngineRiskBinding points here so the
    // risk hook reads the same TIME_TICK-driven time (never a wall clock).
    [[nodiscard]] const uint64_t* now_ns_ptr() const noexcept { return &now_ns_; }
    [[nodiscard]] bool wal_faulted() const noexcept { return wal_fault_; }
    // Last engine-level reject code (see kReject*); nullptr when none.
    [[nodiscard]] const char* last_reject() const noexcept { return last_reject_; }
    [[nodiscard]] uint64_t reject_count() const noexcept { return reject_count_; }

    [[nodiscard]] OrderBook& book() noexcept { return book_; }
    [[nodiscard]] const OrderBook& book() const noexcept { return book_; }
    [[nodiscard]] MemoryPool<Order>& orders() noexcept { return orders_; }
    [[nodiscard]] StopOrderTrigger& stops() noexcept { return stops_; }
    [[nodiscard]] const StopOrderTrigger& stops() const noexcept { return stops_; }
    [[nodiscard]] IcebergManager& icebergs() noexcept { return icebergs_; }
    [[nodiscard]] const IcebergManager& icebergs() const noexcept {
        return icebergs_;
    }

    // Engine-level reject codes (Task 2.3.2 set; risk-side codes are Wave C).
    static constexpr char kRejectUnknownOrder[] = "UNKNOWN_ORDER";
    static constexpr char kRejectStpCancelled[] = "STP_CANCELLED";
    static constexpr char kRejectFokUnfilled[] = "FOK_UNFILLED";
    static constexpr char kRejectBookCapacity[] = "BOOK_CAPACITY";
    static constexpr char kRejectDuplicateId[] = "DUPLICATE_ID";
    static constexpr char kRejectOrderInvalid[] = "ORDER_INVALID";
    static constexpr char kRejectRiskRejected[] = "RISK_REJECTED";

private:
    // Per-order aux side table: trade_group_id (STP, migration 072) and
    // GTD/DAY expiry for resting book orders AND pending stops. Only orders
    // carrying non-default aux occupy a slot.
    struct OrderMeta {
        uint64_t order_id = 0;        // key; 0 = empty
        uint32_t trade_group_id = 0;
        int64_t  expiry_ns = 0;       // 0 = no expiry
        int32_t  heap_index = -1;     // expiry-heap slot, -1 when unqueued
    };

    struct TakerResult {
        int64_t remaining;
        bool dead;      // taker remainder must be cancelled (STP/FOK guard)
        uint8_t dead_reason;  // kWalCancelReason*
        const char* dead_code;
    };

    // --- helpers --------------------------------------------------------------

    void reject(const Order* order, const char* code) noexcept;
    void emit_cancel_event(uint64_t order_id, uint64_t account_id,
                           uint8_t wal_reason) noexcept;
    // Locate-and-terminate an order wherever it lives: pending stop queue,
    // resting book node (incl. iceberg slices + hidden remainder), or
    // nowhere (idempotent no-op). WAL ORDER_CANCEL precedes the mutation.
    bool cancel_internal(uint64_t order_id, uint64_t account_id,
                         uint8_t wal_reason, bool check_account) noexcept;

    // FOK feasibility walk — pure read, must mirror walk_match's decisions.
    bool fok_feasible(const Order& taker, uint32_t taker_group,
                      bool has_limit, int64_t limit_ticks) const noexcept;
    // The price-time sweep. limit_ticks valid iff has_limit (LIMIT /
    // STOP_LIMIT); MARKET/STOP pass has_limit=false (walk to empty).
    TakerResult walk_match(Order& taker, const OrderAux& aux,
                           bool has_limit, int64_t limit_ticks) noexcept;
    void rest_remainder(Order& taker, const OrderAux& aux,
                        int64_t display_qty) noexcept;
    // Engine-driven terminal handling for a non-resting taker remainder.
    void finish_taker(Order& order, const OrderAux& aux, TakerResult r,
                      int64_t display) noexcept;

    // Iceberg slice refresh after the live slice node died; inserts the next
    // visible slice at the FIFO tail of its level (or erases the record).
    void replenish_iceberg(IcebergManager::Record* rec) noexcept;
    // STP application inside the walk. Returns true when the maker was
    // consumed from the queue (cancelled/fully decremented).
    bool apply_stp(Order* maker, StpAction action, int64_t& taker_rem,
                   TakerResult& res) noexcept;

    // meta map
    OrderMeta* meta_find(uint64_t order_id) noexcept;
    const OrderMeta* meta_find(uint64_t order_id) const noexcept;
    OrderMeta* meta_ensure(uint64_t order_id) noexcept;   // nullptr when full
    void meta_erase(uint64_t order_id) noexcept;
    uint32_t group_of(uint64_t order_id) const noexcept;
    // Create/update the aux entry for an order id; fail-closed when the meta
    // table or expiry heap is unavailable/full (never silently untracked).
    bool track_meta(uint64_t order_id, const OrderAux& aux) noexcept;

    // expiry min-heap keyed by (expiry_ns, order_id) — total order.
    [[nodiscard]] bool expiry_track(OrderMeta& m) noexcept;
    void expiry_untrack(OrderMeta& m) noexcept;
    void expiry_sift_up(std::size_t i) noexcept;
    void expiry_sift_down(std::size_t i) noexcept;
    void expiry_remove_at(std::size_t i) noexcept;
    void expire_due() noexcept;

    void drain_triggers() noexcept;
    // aux carries the order's group/expiry: rebuilt from the meta index for
    // queued pops, or forwarded from ingress for an immediately-due arrival.
    void process_triggered(Order* node, const OrderAux& aux) noexcept;
    void publish_depth() noexcept;

    [[nodiscard]] uint32_t instrument_id_of(const OrderAux& aux) const noexcept;

    uint32_t shard_id_;
    OrderBook& book_;
    MemoryPool<Order>& orders_;
    WalWriter* wal_;
    IpcPublisher* publisher_;

    StopOrderTrigger stops_;
    IcebergManager icebergs_;

    // Side tables (ctor-allocated once, nothrow — null => feature degrade):
    OrderMeta* meta_ = nullptr;      // open-addressed order meta index
    std::size_t meta_cap_ = 0;
    std::size_t meta_live_ = 0;
    struct ExpiryEntry {             // binary min-heap, aligned with meta_
        int64_t expiry_ns;
        uint64_t order_id;
    };
    ExpiryEntry* heap_ = nullptr;
    std::size_t heap_cap_ = 0;
    std::size_t heap_size_ = 0;

    risk_check_fn risk_fn_ = nullptr;
    void* risk_ctx_ = nullptr;

    uint64_t received_count_ = 0;
    uint64_t trades_emitted_ = 0;
    uint64_t next_trade_id_ = 1;
    uint64_t emit_seq_ = 0;          // engine-generated priority stamps
    uint64_t now_ns_ = 0;            // logical clock (TIME_TICK driven)
    int64_t last_price_ticks_ = 0;
    bool wal_fault_ = false;
    const char* last_reject_ = nullptr;
    uint64_t reject_count_ = 0;
};

}  // namespace exch
