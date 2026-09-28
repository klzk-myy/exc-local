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
#include "matching/DiscretionaryExecutor.hpp"
#include "matching/ExecutionCollar.hpp"
#include "matching/IcebergManager.hpp"
#include "matching/PriceImprovementRecorder.hpp"
#include "matching/SelfTradeGuard.hpp"
#include "matching/StopOrderTrigger.hpp"
#include "matching/TradeThroughGuard.hpp"
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
    // Atomic amend/replace (Task 2.3.20, spec §6.9). Every amend routes
    // through the single matching-thread replace path — never cancel+new.
    // <=0 fields mean "unchanged" (WalOrderModifyPayload convention).
    void on_amend_received(uint64_t order_id, int64_t new_price_ticks,
                           int64_t new_qty_units, int64_t new_stop_price_ticks,
                           uint64_t ingress_seq) noexcept override;
    // Rich amend form — carries the ICEBERG display_qty and GTD re-arm
    // fields the 5-arg IEngineIngress shim cannot express. The pump's
    // OrderAmend decode forwards only price/qty/stop/seq today (EnginePump
    // is Wave-B owned); the wire fields order_seq/gtd_expiry_ns land here
    // when Task 5.3.22 completes the routing. <=0 means "unchanged".
    struct AmendRequest {
        int64_t  price_ticks = 0;
        int64_t  qty_units = 0;          // ICEBERG: the order TOTAL
        int64_t  stop_price_ticks = 0;   // trigger price (loses priority)
        int64_t  display_qty_units = 0;  // ICEBERG visible slice (loses priority)
        int64_t  gtd_expiry_ns = 0;      // >0 re-arms the GTD/DAY timer (§6.9)
        uint64_t ingress_seq = 0;        // envelope seq — stale-fence input
    };
    void on_amend_received_ex(uint64_t order_id,
                            const AmendRequest& req) noexcept;
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

    // Snapshot cadence seam (Task 2.3.4 / Phase-02.5 soak hardening): invoked
    // at the tail of every on_time_tick on the matching thread — the book is
    // quiescent between events so serialization is consistent. fn receives
    // the logical clock and the cumulative trades-emitted count (the callee
    // tracks its own delta). Cost when the cadence isn't met: two compares.
    using snapshot_hook_fn = void (*)(void* ctx, uint64_t now_ns,
                                      uint64_t trades_emitted) noexcept;
    void set_snapshot_hook(snapshot_hook_fn fn, void* ctx) noexcept {
        snapshot_fn_ = fn;
        snapshot_ctx_ = ctx;
    }

    // §6.6a surveillance seam (Task 2.3.15): MarketOrderProtectionTriggered
    // is emitted once per MARKET order converted to a synthetic limit;
    // clipped_qty_units > 0 identifies an actual SLIPPAGE_EXCEEDED remainder
    // rejection. NATS bridging lands with Phase-17 — today the event is
    // exposed through this sink callback.
    struct MarketOrderProtectionEvent {
        uint64_t order_id;
        uint64_t account_id;
        uint32_t instrument_id;
        Side     side;
        int64_t  best_price_ticks;        // opposite best at arrival
        int64_t  protection_price_ticks;  // synthetic limit applied
        int64_t  slippage_bps;            // effective band in basis points
        int64_t  clipped_qty_units;       // unfilled remainder rejected
        uint64_t ts_ns;                   // engine logical clock
    };
    using protection_event_fn =
        void (*)(void* ctx, const MarketOrderProtectionEvent& ev) noexcept;
    void set_protection_event_sink(protection_event_fn fn, void* ctx) noexcept {
        protection_fn_ = fn;
        protection_ctx_ = ctx;
    }

    // §6.5/§24 #274 surveillance seam (Task 2.3.16): emitted once per
    // EXECUTED self-trade fill — i.e. a maker/taker pair sharing account_id
    // or a nonzero trade_group_id that traded because the taker's resolved
    // stp_mode is NONE (Professional/ECP-gated upstream by check 14). Phase-17
    // owns the NATS bridge for wash-trade monitoring; today the event is
    // exposed through this sink callback.
    struct SelfTradeEvent {
        uint64_t trade_id;
        uint64_t taker_order_id;
        uint64_t maker_order_id;
        uint64_t taker_account_id;
        uint64_t maker_account_id;   // == taker_account_id for same-account
        uint32_t trade_group_id;     // shared group, 0 when same-account-only
        uint32_t instrument_id;
        int64_t  price_ticks;
        int64_t  qty_units;
        uint64_t ts_ns;              // engine logical clock
    };
    using self_trade_fn =
        void (*)(void* ctx, const SelfTradeEvent& ev) noexcept;
    void set_self_trade_sink(self_trade_fn fn, void* ctx) noexcept {
        self_trade_fn_ = fn;
        self_trade_ctx_ = ctx;
    }

    // §6.5/§24 #279-280 audit seam (Task 2.3.18): emitted once per
    // PREVENTED_MATCH journal append — mutually-requested TRANSFER across
    // accounts inside one trade group suppresses a match; the payload is
    // the identical record journaled to the WAL (Phase-03 GL consumes the
    // WAL; Phase-05 exposes this sink / the accessors below for order state).
    using prevented_match_fn =
        void (*)(void* ctx, const WalPreventedMatchPayload& ev) noexcept;
    void set_prevented_match_sink(prevented_match_fn fn, void* ctx) noexcept {
        prevented_fn_ = fn;
        prevented_ctx_ = ctx;
    }

    // Task 2.3.18 prevented-quantity introspection (orders.prevented_qty,
    // migration 072). Per-order cumulative STP-suppressed qty while the
    // order is live (the meta slot dies with the order — terminal records
    // persist via the WAL journal and the prevented_matches table).
    // prevented_qty_total() is the engine-wide cumulative counter.
    [[nodiscard]] int64_t prevented_qty_units(uint64_t order_id) const noexcept;
    [[nodiscard]] int64_t prevented_qty_total() const noexcept {
        return prevented_qty_total_;
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
    // Task 2.3.13 (spec §6.6 / §23 registry rows).
    static constexpr char kRejectNoLiquidity[] = "ORDER_REJECTED_NO_LIQUIDITY";
    static constexpr char kRejectWideSpread[] = "MARKET_ORDER_REJECTED_WIDE_SPREAD";
    // Task 2.3.15 (spec §6.6a; MARKET_SLIPPAGE_EXCEEDED is the §23-registered
    // gateway alias for the same event on the submission path).
    static constexpr char kRejectSlippageExceeded[] = "SLIPPAGE_EXCEEDED";
    // Task 2.3.20 (spec §6.9 / §7.1; AMEND_IN_AUCTION_REJECTED is reserved
    // for the CALL auction phase — the §5.1 InstrumentStatus enum has no
    // CALL enumerator yet; it is defined here so Phase-15 lands the state
    // without touching this file again).
    static constexpr char kRejectStaleModify[] = "STALE_MODIFY";
    static constexpr char kRejectAmendRejected[] = "ORDER_AMEND_REJECTED";
    static constexpr char kRejectAmendInAuction[] = "AMEND_IN_AUCTION_REJECTED";
    static constexpr char kRejectInstrumentCancelOnly[] = "INSTRUMENT_CANCEL_ONLY";
    static constexpr char kRejectInstrumentSuspended[] = "INSTRUMENT_SUSPENDED";
    static constexpr char kRejectInstrumentHalted[] = "INSTRUMENT_HALTED";
    static constexpr char kRejectInstrumentDelisted[] = "INSTRUMENT_DELISTED";

    // WAL marker for the §6.6a MARKET_WITH_PROTECTION conversion:
    // Order::flags bit2 flows verbatim into WalOrderNewPayload.flags
    // (bits 0/1 = post_only/reduce_only per kOrderFlag*; the payload
    // comment's "bit2 stp_cancel_newest" note is stale — stp_mode travels
    // in its own payload field). WalEntry.hpp is frozen, so the flag bit is
    // the recorded marker.
    static constexpr uint8_t kOrderFlagMarketWithProtection = 1u << 2;
    // ORDER_CANCEL reason 5 — extends the kWalCancelReason* family defined
    // in matching/WalWriter.hpp (Wave-B owned header, so the constant lives
    // here). The payload field is a free-form u8; replay treats reason as
    // informational only (RecoveryManager never branches on it).
    static constexpr uint8_t kWalCancelReasonSlippageExceeded = 5;
    // §6.6a default bands when Instrument::max_slippage_bps == 0 (the
    // "unconfigured" sentinel): 100 bps majors, 200 bps exotics. Instrument
    // carries no major/exotic class field — settlement_cycle >= 2 (the §6.3
    // exotic T+2 settlement) is the deterministic proxy.
    static constexpr int64_t kSlippageDefaultMajorBps = 100;
    static constexpr int64_t kSlippageDefaultExoticBps = 200;
    // A configured band >= 10000 bps (>=100%) is the documented
    // "protection off" escape — the collar can never bind.
    static constexpr int64_t kSlippageUnboundedBps = 10'000;

    // --- Tasks 2.3.17/2.3.22/2.3.26 execution-rule seams ---------------------
    // All three protections are configured per instrument through the
    // instruments.execution_rule JSONB object (migration 072). The engine
    // receives the parsed rule via the setters below — an absent rule means
    // the feature is NOT enforced (spec §22.2 absence semantics, applied
    // uniformly to the §6.6b trade-through guard so a book without an
    // execution rule matches with plain price-time sweeps).
    //
    // §22.2 reference-price collar (Task 2.3.17): begin_phase snapshots the
    // bounds once per taker phase; out-of-range remainders expire with
    // EXECUTION_RULE_PRICE_RANGE_EXCEEDED journaled under cancel reason 6.
    void set_execution_rule(const ExecutionCollarRule& rule) noexcept {
        collar_rule_ = rule;
    }
    // Phase-19.5 oracle feed: non-stale reference + its stamp. ts_ns == 0
    // marks "no reference seen yet" — the collar age gate fails closed.
    void set_reference_price(int64_t ref_price_ticks,
                             uint64_t ref_ts_ns) noexcept {
        ref_price_ticks_ = ref_price_ticks;
        ref_ts_ns_ = ref_ts_ns;
    }
    [[nodiscard]] ExecutionCollar& execution_collar() noexcept {
        return collar_;
    }
    // §6.6b trade-through protection (Task 2.3.22): enabled when the
    // instrument's execution_rule configures it. The guard itself keeps the
    // seqlock protected-quote cache fed by refresh_protected_quote() after
    // every committed mutation, and emits TradeThroughEvents to its sink
    // (Phase-20 TCA bridge).
    void set_trade_through_protection(bool enabled) noexcept {
        tt_enabled_ = enabled;
    }
    [[nodiscard]] TradeThroughGuard& trade_through_guard() noexcept {
        return trade_through_;
    }
    [[nodiscard]] PriceImprovementRecorder&
    improvement_recorder() noexcept {
        return improvement_;
    }
    // Phase-15 CALL-state feed: during auctions the §6.6b checks suspend
    // (uncross = single market-clearing price). Exposed on the guard AND
    // mirrored on the engine so a future auction gate can consult it.
    void set_auction_mode(bool in_auction) noexcept {
        auction_mode_ = in_auction;
        trade_through_.set_auction(in_auction);
    }
    // ORDER_CANCEL reason 6 — execution-collar remainder expiry
    // (EXECUTION_RULE_PRICE_RANGE_EXCEEDED). Continues the kWalCancelReason*
    // family in matching/WalWriter.hpp.
    static constexpr uint8_t kWalCancelReasonExecRuleRange = 6;

private:
    // Per-order aux side table: trade_group_id (STP, migration 072) and
    // GTD/DAY expiry for resting book orders AND pending stops. Only orders
    // carrying non-default aux occupy a slot.
    struct OrderMeta {
        uint64_t order_id = 0;        // key; 0 = empty
        uint32_t trade_group_id = 0;
        int64_t  expiry_ns = 0;       // 0 = no expiry
        int32_t  heap_index = -1;     // expiry-heap slot, -1 when unqueued
        // Task 2.3.20 amend fence (§6.9 #1): the in-flight amend map keyed
        // by order_id. amend_seq records the ingress_seq of the last
        // APPLIED amend; a non-advancing seq is the loser of a concurrent
        // pair and draws STALE_MODIFY — exactly one winner, zero
        // double-apply, deterministic under replay.
        uint64_t amend_seq = 0;
        bool     amend_seen = false;
        // Task 2.3.18 — cumulative STP-suppressed quantity for this order
        // (orders.prevented_qty, migration 072). Accumulates across every
        // prevention mode while the order is live.
        int64_t  prevented_qty_units = 0;
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
                      bool has_limit, int64_t limit_ticks,
                      const CollarBounds& cb) const noexcept;
    // The price-time sweep. limit_ticks valid iff has_limit (LIMIT /
    // STOP_LIMIT); MARKET/STOP pass has_limit=false (walk to empty).
    // cb is the per-phase §22.2 collar snapshot (NOT_CONFIGURED passes all).
    TakerResult walk_match(Order& taker, const OrderAux& aux,
                           bool has_limit, int64_t limit_ticks,
                           const CollarBounds& cb) noexcept;
    void rest_remainder(Order& taker, const OrderAux& aux,
                        int64_t display_qty) noexcept;
    // Engine-driven terminal handling for a non-resting taker remainder.
    void finish_taker(Order& order, const OrderAux& aux, TakerResult r,
                      int64_t display) noexcept;

    // Iceberg slice refresh after the live slice node died; inserts the next
    // visible slice at the FIFO tail of its level (or erases the record).
    void replenish_iceberg(IcebergManager::Record* rec) noexcept;
    // STP application inside the walk (Tasks 2.3.11/2.3.16/2.3.18).
    // `taker_group` is the incoming order's resolved trade_group_id and
    // `level_price_ticks` the price of the level being swept (the prevented
    // match's would-be execution price). Returns true when the maker was
    // consumed from the queue (cancelled/fully decremented).
    bool apply_stp(Order& taker, uint32_t taker_group, Order* maker,
                   int64_t level_price_ticks, StpAction action,
                   int64_t& taker_rem, TakerResult& res) noexcept;
    // Task 2.3.18 helpers: cumulative prevented-qty accounting (meta slot +
    // engine total) and the PREVENTED_MATCH journal+sink emission for
    // mutual-TRANSFER cross-account group matches.
    void note_prevented(uint64_t order_id, int64_t qty_units) noexcept;
    // Scalar args: the maker node may already be pool-freed by the decrement
    // path's book_.cancel_order — callers pass values captured pre-mutation.
    void emit_prevented_match(uint64_t maker_id, uint64_t maker_account_id,
                              uint32_t maker_group, const Order& taker,
                              uint32_t taker_group, int64_t price_ticks,
                              int64_t maker_prevented,
                              int64_t taker_prevented) noexcept;
    // Effective remaining maker quantity including the iceberg hidden
    // remainder (slice + unseen) — the qty STP suppression accounts for.
    [[nodiscard]] int64_t effective_remaining_units(
        const Order* maker) const noexcept;

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
    // §6.6b helpers: pushes the internal-book top into the guard's seqlock
    // cache after every committed mutation (spec §6.6b #6 staleness
    // contract), and the IOC/FOK presence-walk bounded at the quote.
    void refresh_protected_quote() noexcept;
    [[nodiscard]] int64_t liquidity_at_or_better_units(
        Side taker_side, int64_t quote_ticks) const noexcept;
    // aux carries the order's group/expiry: rebuilt from the meta index for
    // queued pops, or forwarded from ingress for an immediately-due arrival.
    void process_triggered(Order* node, const OrderAux& aux) noexcept;
    void publish_depth() noexcept;

    // --- Task 2.3.13/2.3.15/2.3.20 helpers -------------------------------------
    // §7.1/§6.9 amend gate: per-state reject code, nullptr when amends are
    // permitted. CALL (auction phase) has no InstrumentStatus enumerator yet
    // — Phase-15 Task 15.3.x owns it; it maps to kRejectAmendInAuction.
    [[nodiscard]] const char* amend_state_gate() const noexcept;
    // Amend worker halves of on_amend_received_ex (the single matching-
    // thread replace path). m is the order's ensured meta slot (fence +
    // expiry re-arm); every failure path only records last_reject_.
    void amend_pending_stop(StopOrderTrigger::Pending& p,
                            const AmendRequest& req, OrderMeta& m) noexcept;
    void amend_resting(Order& o, const AmendRequest& req, OrderMeta& m) noexcept;
    // GTD re-arm after a journaled amend — preflighted by the caller.
    bool rearm_expiry(OrderMeta& m, int64_t new_expiry_ns) noexcept;
    // Effective slippage band (bps) for the instrument — §6.6a sentinel
    // handling documented on the kSlippage* constants.
    [[nodiscard]] static int64_t effective_slippage_bps(
        const Instrument& i) noexcept;
    // Synthetic-limit price for a MARKET order against `best_ticks` on the
    // opposite side: best*(10000±bps)/10000, integer math only. Returns
    // false when the band is unbounded (>=100%) or overflows — callers then
    // run unprotected or reject, per site.
    [[nodiscard]] static bool slippage_protection_price(
        Side side, int64_t best_ticks, int64_t bps, int64_t& out) noexcept;

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
    snapshot_hook_fn snapshot_fn_ = nullptr;
    void* snapshot_ctx_ = nullptr;
    protection_event_fn protection_fn_ = nullptr;
    void* protection_ctx_ = nullptr;

    // Tasks 2.3.17/2.3.22 execution-rule state (instruments.execution_rule
    // JSONB parsed upstream): collar + reference feed, trade-through guard,
    // and the price-improvement recorder. All single-writer on the matching
    // thread; the guard's quote cache is seqlock-published for readers.
    ExecutionCollar collar_;
    ExecutionCollarRule collar_rule_{};
    int64_t ref_price_ticks_ = 0;
    uint64_t ref_ts_ns_ = 0;
    TradeThroughGuard trade_through_;
    PriceImprovementRecorder improvement_;
    bool tt_enabled_ = false;
    bool auction_mode_ = false;
    self_trade_fn self_trade_fn_ = nullptr;
    void* self_trade_ctx_ = nullptr;
    prevented_match_fn prevented_fn_ = nullptr;
    void* prevented_ctx_ = nullptr;
    int64_t prevented_qty_total_ = 0;  // Task 2.3.18 engine-wide counter

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
