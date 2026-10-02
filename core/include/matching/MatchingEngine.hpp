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
#include "matching/ImpliedMatcher.hpp"
#include "matching/PriceImprovementRecorder.hpp"
#include "matching/SelfTradeGuard.hpp"
#include "matching/StopOrderTrigger.hpp"
#include "matching/TradeThroughGuard.hpp"
#include "matching/WalWriter.hpp"
#include "utils/MemoryPool.hpp"

namespace exch {

class IpcPublisher;
class L3Publisher;      // ipc/L3Publisher.hpp (Phase-17 Task 17.3.1)
class InstrumentFeed;   // risk/InstrumentFeed.hpp (Phase-15 control feed)
class PriceOracleFeed;  // risk/PriceOracleFeed.hpp (Phase-16 mark/index)
class BilateralCreditMatrix;  // risk/BilateralCreditMatrix.h (spec §3.3b)
class IPartyMap;              // risk/RiskInterfaces.hpp

class MatchingEngine : public IEngineIngress {
public:
    // wal / publisher / l3 may be nullptr -> journal/publish degrade to
    // no-ops. l3 is the Phase-17 order-level stream (spec §11) — it must be
    // bound BEFORE any wal-journal bookkeeping so L3 events can carry the
    // exact WAL seq (journal_seq()); journal-free engines emit a
    // deterministic virtual cursor (replay, unit tests).
    MatchingEngine(uint32_t shard_id, OrderBook& book,
                   MemoryPool<Order>& orders, WalWriter* wal = nullptr,
                   IpcPublisher* publisher = nullptr,
                   L3Publisher* l3 = nullptr) noexcept;
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
    // fields the 5-arg IEngineIngress shim cannot express. <=0 means
    // "unchanged".
    struct AmendRequest {
        int64_t  price_ticks = 0;
        int64_t  qty_units = 0;          // ICEBERG: the order TOTAL
        int64_t  stop_price_ticks = 0;   // trigger price (loses priority)
        int64_t  display_qty_units = 0;  // ICEBERG visible slice (loses priority)
        int64_t  gtd_expiry_ns = 0;      // >0 re-arms the GTD/DAY timer (§6.9)
        uint64_t ingress_seq = 0;        // wire order_seq (§6.9 #1 fence)
        // Phase-16 Task 16.3.17: trigger-source switch on amend is
        // rejected — 0xFF means "unchanged".
        uint8_t  trigger_source = 0xFF;
    };
    void on_amend_received_ex(uint64_t order_id,
                            const AmendRequest& req) noexcept;
    // Wire ingress (IMP-PLAN Phase-3 Task 2): EnginePump's OrderAmend
    // decode forwards the full AmendWire surface — order_seq is the
    // §6.9 #1 stale-fence input (envelope seq stands in when the wire
    // field is 0/unsequenced).
    void on_amend_received_ex(uint64_t order_id,
                              const IEngineIngress::AmendWire& req,
                              uint64_t ingress_seq) noexcept override;
    // Deterministic logical clock: WAL TIME_TICK -> GTD/DAY expiry sweep ->
    // stop re-check. Monotonic-guarded (a stale tick is journaled but does
    // not move the clock backwards).
    void on_time_tick(uint64_t now_ns) noexcept override;

    // Phase-14 Task 14.3.1 — OCO (one-cancels-other) pair linkage
    // (spec §6.2/§6.5, §24 #47; wire OcoLink event). The gateway sequences
    // this command BEFORE either member's OrderNew on the shard ring, so
    // the engine installs the link while both legs are still unplaced:
    // whichever leg reaches terminal FILLED first cancels the sibling
    // atomically on the matching thread (journaled ORDER_CANCEL reason 7),
    // and a leg whose sibling already terminated is rejected
    // OCO_SIBLING_CANCEL_RACE on arrival — the trailing-order reject of the
    // spec §6.5/§6.8 deterministic race resolution. Validation failures are
    // never journaled; a re-sent identical link is an idempotent no-op.
    void on_oco_link_received(uint64_t link_id, uint64_t order_id_a,
                            uint64_t order_id_b, uint64_t account_id,
                            uint32_t instrument_id) noexcept override;

    // OCO introspection (tests / recovery accounting): member count is the
    // number of linked order ids (2 per live pair, 1 while a doomed entry
    // awaits its leg), link count is live pairs.
    [[nodiscard]] uint64_t oco_member_count() const noexcept {
        return oco_live_;
    }
    [[nodiscard]] uint64_t oco_link_count() const noexcept {
        return oco_live_ / 2;
    }
    // -1 = unlinked, 0 = armed, 1 = doomed (sibling won — reject on arrival).
    [[nodiscard]] int oco_member_state(uint64_t order_id) const noexcept;

    // --- Recovery / wiring hooks ---------------------------------------------

    // Trade ids are monotonic per engine; B2 recovery seeds the next id from
    // the WAL tail so post-replay trades continue the sequence. On a bound
    // trade-id stream (curve shard) the shared counter is seeded instead —
    // same id space the journaled rows were drawn from.
    void seed_trade_id(uint64_t next_trade_id) noexcept {
        uint64_t& s = tid_stream();
        if (next_trade_id > s) s = next_trade_id;
    }
    // Curve-shard co-location (Task 22.3.12): engines sharing one WAL and
    // an implied matcher must draw trade ids from ONE stream — implied
    // fills print into sibling books under the driving engine's seq, and
    // the shared journal's max_trade_id is global. The host binds the
    // shared counter on every co-located engine before any ingress; a
    // standalone engine keeps its member stream (pre-22 identical).
    void bind_trade_id_stream(uint64_t* shared) noexcept {
        tid_stream_ = shared != nullptr ? shared : &next_trade_id_;
    }
    [[nodiscard]] uint64_t& tid_stream() noexcept {
        return tid_stream_ != nullptr ? *tid_stream_ : next_trade_id_;
    }
    // Ownership probe (Task 22.3.12 host routing): true when order_id is
    // live anywhere in this engine's stores — resting book, pending stop
    // queue, or auction-parked list. Read-only; used by a multi-engine
    // ingress to route cancel/amend wire messages that carry no
    // instrument_id.
    [[nodiscard]] bool owns_order(uint64_t order_id) noexcept {
        return l3_order_detail(order_id) != nullptr;
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

    // IMP-PLAN Phase-3 Task 4 — synchronous per-fill observer on the TAKER
    // leg (order_id, fill price ticks, fill qty). The OptimisticShardCoordinator
    // match/unwind executors arm a capture context around
    // on_order_received to recover filled qty + VWAP for a synthetic leg —
    // the publisher path can't serve them (it has no return channel).
    // Single-writer on the matching thread; the observer MUST NOT mutate
    // engine state or re-enter order ingress.
    using fill_observe_fn = void (*)(void* ctx, uint64_t taker_order_id,
                                     int64_t price_ticks,
                                     int64_t qty_units) noexcept;
    void set_fill_observer(fill_observe_fn fn, void* ctx) noexcept {
        fill_obs_fn_ = fn;
        fill_obs_ctx_ = ctx;
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
    [[nodiscard]] uint64_t next_trade_id() noexcept {
        return tid_stream();
    }
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
    static constexpr char kRejectInstrumentRestricted[] = "INSTRUMENT_RESTRICTED";
    // Phase-15 Task 15.3.4 (§7.3) — outside the published 24/5 window.
    static constexpr char kRejectMarketClosed[] = "MARKET_CLOSED";
    // Phase-15 Task 15.3.10 (§23-registered 409 rows).
    static constexpr char kRejectAuctionClearingFailed[] =
        "AUCTION_CLEARING_FAILED";
    static constexpr char kRejectCrossedBook[] = "CROSSED_BOOK_DETECTED";
    // Phase-14 Task 14.3.1 OCO codes (spec §6.5/§23 — OCO_SIBLING_CANCEL_RACE
    // is the §23-registered code; the other two are engine-internal link
    // validation verdicts surfaced via last_reject()/logs only).
    static constexpr char kRejectOcoSiblingRace[] = "OCO_SIBLING_CANCEL_RACE";
    static constexpr char kRejectOcoLinkInvalid[] = "OCO_LINK_INVALID";
    static constexpr char kRejectOcoLinkConflict[] = "OCO_LINK_CONFLICT";
    // Phase-16 Task 16.3.11/16.3.22 (spec §23): a pegged order cannot price
    // — the visible BBO is missing, one-sided or crossed (quarantine/CALL
    // excluded via the auction gate). Admission rejects outright; a resting
    // peg whose reference vanishes holds its current level and counts a
    // peg_unavailable observation.
    static constexpr char kRejectPeggedPricingUnavailable[] =
        "PEGGED_PRICING_UNAVAILABLE";
    // Phase-16 Task 16.3.17/16.3.22 (spec §6.2a): MARK/INDEX-sourced
    // conditionals freeze when the oracle value is missing or older than
    // kOracleStaleNs — they stay pending (never trigger on stale data).
    static constexpr char kRejectConditionalOracleStale[] =
        "CONDITIONAL_TRIGGER_ORACLE_STALE";
    // Phase-16 Task 16.3.16 (spec §23 registered row): the instrument's
    // aggregate guaranteed-stop liability cap is exceeded at admission.
    static constexpr char kRejectGsloExposureLimit[] =
        "GSLO_EXPOSURE_EXCEEDED";
    // Phase-16 Task 16.3.25 (spec §6.2b): unfilled parked auction orders
    // (MARKET/IOC/FOK + MOO/MOC) swept at/after the uncross surface this
    // code — WAL reason kWalCancelReasonAuctionCancelled (8).
    static constexpr char kRejectAuctionCancelled[] = "AUCTION_CANCELLED";
    // Phase-22 Task 22.3.12 — post_only order whose remainder would take
    // implied liquidity rejects with the same code the pre-trade gate
    // uses (services emit POST_ONLY_VIOLATION for the outright case).
    static constexpr char kRejectPostOnlyViolation[] = "POST_ONLY_VIOLATION";
    // GSLO fills print against the synthetic venue counterparty id — the
    // gap liability moves to the exposure pool, never a real order id.
    static constexpr uint64_t kGsloVenueOrderId = ~uint64_t{0};
    // §6.2b auction freeze window (T-30s before the armed deadline):
    // parked MOO/MOC cancels/amends inside it reject AMEND_IN_AUCTION_.
    static constexpr uint64_t kAuctionFreezeNs =
        30ull * 1'000'000'000ull;
    // Mark/index oracle staleness gate (spec §19.5 5s convention).
    static constexpr uint64_t kOracleStaleNs = 5ull * 1'000'000'000ull;

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

    // --- Phase-15 instrument feed / auction surface --------------------------
    // Binds the control-plane InstrumentFeed (Tasks 15.3.3/15.3.4/15.3.6).
    // Unbound = dev/test mode: the lifecycle gate falls back to the static
    // book instrument status and the market-hours/auction keys are not
    // enforced (identical convention to the SuspensionFlags seam — an
    // unwired feed leaves enforcement to the Go admission layer).
    // Mutable pointer: the engine writes the auction-result request slot
    // (matching→control channel) — reads still go through the immutable
    // snapshot copy.
    void bind_instrument_feed(InstrumentFeed* feed) noexcept {
        feed_ = feed;
    }
    [[nodiscard]] const InstrumentFeed* instrument_feed() const noexcept {
        return feed_;
    }

    // --- Phase-22 Task 22.3.12 implied matching (spec §24 #199/#200) -----
    // Binds this engine's book to an ImpliedMatcher shared across the
    // curve's co-located books — all books registered on ONE matching
    // thread (the curve-shard host topology; spec §27). Unbound or
    // disabled matcher => zero-cost no-ops everywhere; behaviour is
    // identical to the pre-Phase-22 engine.
    //
    // Wiring contract for a host process:
    //   * construct one MatchingEngine per linked instrument on the same
    //     thread, each with its own pool/book/wal/pub;
    //   * call bind_implied(&matcher) on each before any ingress —
    //     registration resolves the bound book's instrument lazily, so
    //     book.set_instrument may precede or follow the bind;
    //   * after EVERY ingress event (order/cancel/amend/tick) drain the
    //     cross-engine fixpoint: for each engine e, while
    //     e.take_implied_dirty() run e.implied_sync() — implied fills the
    //     matcher produced in e's book need its conditional-settle wave.
    //     Bound the drain (e.g. 8 passes) — deterministic replay needs a
    //     stable fixed point, never an unbounded loop.
    void bind_implied(ImpliedMatcher* m) noexcept {
        implied_ = m;
        implied_registered_ = false;
        implied_dirty_ = false;
        // Register eagerly when the instrument is already bound so a host
        // can register links immediately after the bind; the lazy path in
        // implied_ensure_registered covers instrument-set-later hosts.
        if (implied_ != nullptr) (void)implied_ensure_registered();
    }
    [[nodiscard]] bool implied_enabled() const noexcept {
        return implied_ != nullptr && implied_->enabled();
    }
    // Phase-3 Task 5 (IMP-PLAN) / spec §3.3b + §24 #403 — bilateral
    // credit-screened liquidity. Binding engages the match-time
    // consume_or_skip gate in walk_match: each candidate maker/taker pair
    // whose accounts BOTH map to credit parties must debit mutual
    // headroom for the fill's quote notional before the fill commits;
    // Skip leaves the maker in place (price-time priority intact) and the
    // walk advances to the next member — levels exhausted purely by skips
    // are stepped past, and a remainder that finds no credit-eligible
    // contra anywhere dies as BILATERAL_CREDIT_EXHAUSTED. Unscreened
    // pairs (either party unmapped) proceed ungated — anonymous retail
    // flow never routes through the matrix. Cold-path bind; both objects
    // must outlive the engine.
    void bind_credit(BilateralCreditMatrix* m,
                     const IPartyMap* map) noexcept {
        credit_ = m;
        credit_map_ = map;
    }
    // One-shot drain flag: set by the owner hook when a matcher fill
    // touched this engine's book. The host consumes it to schedule the
    // engine's implied_sync (rescan + conditional settle).
    [[nodiscard]] bool take_implied_dirty() noexcept {
        const bool d = implied_dirty_;
        implied_dirty_ = false;
        return d;
    }
    // Host-facing rescan+settle entry — see bind_implied contract.
    void implied_sync() noexcept;

    // Auction phase values == WalAuctionPhasePayload.phase wire enum.
    static constexpr uint8_t kAuctionPhaseCall       = 0;
    static constexpr uint8_t kAuctionPhaseExtend     = 1;
    static constexpr uint8_t kAuctionPhaseUncross    = 2;
    static constexpr uint8_t kAuctionPhaseCancel     = 3;
    static constexpr uint8_t kAuctionPhaseQuarantine = 4;
    static constexpr uint8_t kAuctionPhaseStrikeFail = 5;
    static constexpr uint8_t kAuctionPhaseNone       = 0xff;
    // WalAuctionPhasePayload.reason values.
    static constexpr uint8_t kAuctionReasonNone          = 0;
    static constexpr uint8_t kAuctionReasonControl       = 1;
    static constexpr uint8_t kAuctionReasonDeadlineMoved = 2;
    static constexpr uint8_t kAuctionReasonClearingFailed = 3;
    static constexpr uint8_t kAuctionReasonCrossedBook   = 4;

    [[nodiscard]] uint8_t auction_phase() const noexcept {
        return auction_phase_;
    }
    [[nodiscard]] int64_t auction_id() const noexcept { return auction_id_; }
    [[nodiscard]] int64_t auction_deadline_ns() const noexcept {
        return auction_deadline_ns_;
    }
    [[nodiscard]] uint8_t auction_extensions() const noexcept {
        return auction_extensions_;
    }
    [[nodiscard]] bool auction_awaiting() const noexcept {
        return auction_awaiting_;
    }
    [[nodiscard]] int64_t last_completed_auction_id() const noexcept {
        return last_completed_auction_id_;
    }
    // Engine-local quarantine (Task 15.3.10): crossed-book halt or an
    // auction-clearing-failure suspend. New orders + amends reject with
    // quarantine_code(); cancels stay open (§7.1 semantics for the mapped
    // status). Only a successful armed-CALL uncross clears it.
    [[nodiscard]] bool quarantined() const noexcept { return quarantined_; }
    [[nodiscard]] const char* quarantine_code() const noexcept {
        return quarantine_code_;
    }
    // Parked = orders accumulated during CALL that can never rest
    // (MARKET / IOC / FOK) — they participate in the uncross then die.
    [[nodiscard]] uint32_t auction_parked_count() const noexcept {
        return parked_count_;
    }
    [[nodiscard]] const Order* auction_parked_head() const noexcept {
        return parked_head_;
    }

    // RecoveryManager replay entry (journal-free): applies one journaled
    // AUCTION_PHASE row. Returns true when a transition was applied.
    bool on_auction_phase_replay(const WalAuctionPhasePayload& p) noexcept;
    // Post-recovery adoption (main.cpp): move the replayed auction position
    // into the live engine — phase/id/deadline/extensions, the consumed-id
    // dedupe ledger, the quarantine flag, and the parked ingress nodes
    // (pool-owned; nullptr when none).
    void adopt_auction_state(uint8_t phase, int64_t auction_id,
                             int64_t deadline_ns, uint8_t extensions,
                             bool awaiting, int64_t last_completed,
                             bool quarantined, const char* quarantine_code,
                             Order* parked_head, uint32_t parked_count) noexcept;
    // ORDER_CANCEL reason 6 — execution-collar remainder expiry
    // (EXECUTION_RULE_PRICE_RANGE_EXCEEDED). Continues the kWalCancelReason*
    // family in matching/WalWriter.hpp.
    static constexpr uint8_t kWalCancelReasonExecRuleRange = 6;

    // --- Phase-16 oracle feed + conditional surfaces (Tasks 16.3.17/22) -----
    // Binds the control-plane mark/index feed (PriceOracleFeed). Unbound =
    // dev/test mode: the local seam below supplies the references; an engine
    // with neither treats MARK/INDEX sources as unavailable (pending
    // conditionals on those sources freeze — fail closed, never trigger).
    void bind_oracle_feed(const PriceOracleFeed* feed) noexcept {
        oracle_feed_ = feed;
    }
    // Journal-free replay/test seam: installs the local oracle snapshot
    // consulted only when no feed is bound. verifiable=false freezes both
    // sources; a zero timestamp freezes its own source.
    void set_oracle_snapshot(int64_t mark_ticks, int64_t mark_ts_ns,
                             int64_t index_ticks, int64_t index_ts_ns,
                             bool verifiable) noexcept;
    [[nodiscard]] const PriceOracleFeed* oracle_feed() const noexcept {
        return oracle_feed_;
    }
    // Settle-loop observations of a live conditional whose MARK/INDEX
    // source was stale/missing (CONDITIONAL_TRIGGER_ORACLE_STALE freeze).
    [[nodiscard]] uint64_t oracle_stale_suspensions() const noexcept {
        return oracle_stale_suspensions_;
    }

    // RecoveryManager replay entry (journal-free): applies one journaled
    // ORDER_TRIGGERED row. LAST-sourced triggers re-derive inside the
    // replayed settle wave — the row is authoritative for MARK/INDEX
    // sources, which the feedless replay engine cannot re-evaluate.
    void on_order_triggered_replay(
        const WalOrderTriggeredPayload& p) noexcept;

    // GSLO exposure export (Task 16.3.16): aggregate armed-notional + live
    // count + fired fills; the per-instrument cap is operator-configurable
    // (0 = uncapped — the admission gate is then inert).
    [[nodiscard]] int64_t gslo_notional_units() const noexcept {
        return gslo_notional_units_;
    }
    [[nodiscard]] uint64_t gslo_live_count() const noexcept {
        return gslo_live_count_;
    }
    [[nodiscard]] uint64_t gslo_fills() const noexcept { return gslo_fills_; }
    void set_gslo_max_exposure_units(int64_t units) noexcept {
        gslo_max_exposure_units_ = units;
    }

    // Pegged-order introspection (Task 16.3.11/16.3.22): committed reprices
    // and hold events where the reference vanished under a resting peg.
    [[nodiscard]] uint64_t peg_repriced_total() const noexcept {
        return peg_repriced_total_;
    }
    [[nodiscard]] uint64_t peg_unavailable_total() const noexcept {
        return peg_unavailable_total_;
    }
    [[nodiscard]] std::size_t pegged_live() const noexcept {
        return pegs_live_;
    }

    // --- L3 journal correlation (Phase-17 Task 17.3.1, spec §11/§24 #318) --
    // RecoveryManager re-anchors the virtual WAL cursor per replayed WAL
    // entry so the replayed engine's emitted L3 wal_seq stream is
    // bit-identical to live. Consumed by journal_seq() at every journal
    // site (live engines read wal_->tail_seq() instead).
    void set_replay_wal_seq(uint64_t seq) noexcept;

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
        // Phase-16 (Tasks 16.3.17/22): the pending order's trigger source +
        // instrument — restored into the rebuilt aux when a conditional
        // pops so the journaled ORDER_TRIGGERED carries the committed
        // source (LAST_PRICE = 0 is the in-band default).
        uint32_t instrument_id = 0;
        uint8_t  trigger_source = 0;
    };

    // Phase-14 Task 14.3.1 — OCO member side table: one entry per linked
    // leg, keyed by order id (open addressing identical to the meta table,
    // allocated once at construction — zero heap on the hot path, full =
    // fail-closed link reject before any journal append).
    struct OcoMember {
        uint64_t order_id = 0;      // key; 0 = empty
        uint64_t link_id = 0;       // OCO group id (orders.oco_group_id)
        uint64_t sibling_id = 0;
        uint32_t instrument_id = 0; // link's instrument — leg sanity check
        uint8_t  state = 0;         // kOcoArmed / kOcoDoomed
    };
    static constexpr uint8_t kOcoArmed  = 0;
    static constexpr uint8_t kOcoDoomed = 1;  // sibling filled first

    struct TakerResult {
        int64_t remaining;
        bool dead;      // taker remainder must be cancelled (STP/FOK guard)
        uint8_t dead_reason;  // kWalCancelReason*
        const char* dead_code;
    };

    // --- helpers --------------------------------------------------------------

    void reject(const Order* order, const char* code) noexcept;
    // WAL ORDER_CANCEL + L3 Cancel + L2 OrderCancel. `detail` lets the L3
    // row carry side/price/remaining when the caller holds the node; when
    // nullptr the emit path resolves book->stops->parked itself.
    void emit_cancel_event(uint64_t order_id, uint64_t account_id,
                           uint8_t wal_reason, const Order* detail = nullptr) noexcept;

    // --- L3 journal correlation (Phase-17 Task 17.3.1, spec §11/§24 #318) --
    // The exact sequence the NEXT journaled row will commit with. Live:
    // wal_->tail_seq() — the committed seq is captured BEFORE the append so
    // it cannot be confused with a later tail. Journal-free (wal==nullptr,
    // replay + unit tests): a virtual cursor consumed at every journal site
    // so derived rows land on the same consecutive seqs the single writer
    // stamped live. Call journal_seq() exactly once per journal site,
    // unconditionally, before the write guard.
    [[nodiscard]] uint64_t journal_seq() noexcept;
    // Emit helpers — no-ops when l3_ is unbound. flags come from the order
    // (hidden/peg/iceberg bits); callers pass scalars where the node may
    // already be dead.
    [[nodiscard]] uint8_t l3_flags_of(const Order& o) const noexcept;
    const Order* l3_order_detail(uint64_t order_id) noexcept;
    void l3_emit_ev(uint8_t kind, uint64_t order_id, uint64_t account_id,
                    Side side, uint32_t instrument_id, int64_t price_ticks,
                    int64_t ref_price_ticks, int64_t qty_units,
                    int64_t qty_delta, uint64_t trade_id, uint8_t fill_role,
                    uint8_t flags, uint8_t cancel_reason,
                    uint64_t wal_seq) noexcept;
    void l3_cancel(uint64_t order_id, uint64_t account_id, uint8_t wal_reason,
                   uint64_t wal_seq, const Order* detail) noexcept;
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

    // OCO member map (open addressing, linear probe, load <= 0.5 — same
    // scheme as the meta table). oco_on_dead is THE terminal funnel: every
    // code path that makes an order dead must call it — by_fill=true marks
    // the fill winner (sibling cancelled atomically or doomed if its
    // OrderNew is still in flight); by_fill=false releases the pair so the
    // surviving leg continues as a standalone order.
    OcoMember* oco_find(uint64_t order_id) noexcept;
    const OcoMember* oco_find(uint64_t order_id) const noexcept;
    OcoMember* oco_ensure(uint64_t order_id) noexcept;  // nullptr when full
    void oco_erase(uint64_t order_id) noexcept;
    void oco_on_dead(uint64_t order_id, bool by_fill) noexcept;

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

    // Phase-16 conditional settle wave (Tasks 16.3.3/15/16/17/22): replaces
    // the bare trigger drain at every ingress tail — trail re-anchor ->
    // sourced trigger pop (ORDER_TRIGGERED journaled per activation) ->
    // pegged reprice -> repeat until stable. Bounded pass count; a
    // journal/structural fault halts the wave.
    void settle() noexcept;

    // --- Phase-22 Task 22.3.12 implied-matching internals -------------------
    // Lazy self-registration — bind_implied may precede
    // book_.set_instrument; the first use resolves the served id.
    bool implied_ensure_registered() noexcept;
    // Post-sweep implied take: fills the taker's remainder against
    // implied liquidity (implied-in and implied-out links alike),
    // folding the result into the in-flight TakerResult. FOK takers and
    // slippage-protected MARKET orders skip the path outright (§27:
    // implied never participates in an all-or-nothing feasibility proof
    // and never extends a §6.6a protection bound).
    void implied_take(Order& order, const OrderAux& aux, TakerResult& r,
                      bool has_limit, int64_t limit_ticks) noexcept;
    // Commit-point rescan: forwards own-book mutations to the matcher and
    // runs a bounded local fixpoint (implied fills in book_ as an out
    // book can in turn move book_ as a leg of further links). Implied
    // fills update last_price_ticks_ via the owner hook, so a dirty pass
    // re-runs settle() to fire LAST-sourced triggers.
    void implied_rescan() noexcept;
    // Owner bookkeeping for one matcher-produced fill inside book_ —
    // mirrors walk_match's post-fill block (iceberg reserve accounting,
    // meta slot release, OCO winner arm, L3 Fill emit, price-improvement
    // recording, LAST-price reference).
    void on_implied_fill(const FillNote& n) noexcept;
    static void implied_fill_trampoline(
        void* ctx, const FillNote& n) noexcept;

    // Evaluation references for the trigger queue: last trade always
    // available; mark/index come from the bound PriceOracleFeed (or the
    // local seam) and are zeroed when unverifiable or stale.
    [[nodiscard]] StopOrderTrigger::TriggerRefs trigger_refs() noexcept;
    // TRAILING_STOP re-anchor pass — rides the trigger queue's trail
    // chain, moves armed stops only on favorable reference moves.
    void trail_eval(const StopOrderTrigger::TriggerRefs& refs) noexcept;
    // Journals one ORDER_TRIGGERED row (no-op when wal_ == nullptr).
    void journal_triggered(const Order* node,
                           const StopOrderTrigger::TriggeredInfo& info,
                           const OrderAux& aux) noexcept;
    // Triggered-node terminal path: GSLO flags route to the guaranteed
    // fill; everything else runs the existing taker conversion.
    void process_triggered_info(Order* node, const OrderAux& aux,
                                const StopOrderTrigger::TriggeredInfo& info)
        noexcept;
    // Task 16.3.16 GSLO helpers: armed-notional accounting against the
    // engine exposure counter (release on pop/cancel, resync on stop
    // moves), and the guaranteed fill itself (full remainder at the armed
    // stop vs the synthetic venue leg — no book walk).
    [[nodiscard]] int64_t gslo_notional(const Order& o,
                                        int64_t stop_ticks) const noexcept;
    void gslo_resync(StopOrderTrigger::Pending& p) noexcept;
    void gslo_release(int64_t notional_units) noexcept;
    void gslo_fill(Order* node, const OrderAux& aux,
                   int64_t stop_ticks) noexcept;

    // Task 16.3.11/16.3.22 pegged-order table — compact array (cold-alloc
    // at construction); order_id key, linear scans bounded by kPegsCap.
    struct PegRec {
        uint64_t order_id = 0;
        int64_t  offset_ticks = 0;   // signed; + = more aggressive
        int64_t  limit_ticks = 0;    // collar; 0 = none
        uint8_t  mode = 0;           // kPeg*
        uint8_t  priced_ok = 1;      // 0 while the reference is missing
    };
    PegRec* peg_find(uint64_t order_id) noexcept;
    const PegRec* peg_find(uint64_t order_id) const noexcept;
    void peg_erase(uint64_t order_id) noexcept;
    // Visible-BBO references: levels whose whole depth is hidden/pegged
    // carry no public liquidity — the public BBO is what pegged orders
    // track (and what hidden makers midpoint against).
    [[nodiscard]] int64_t visible_best_price(Side s) const noexcept;
    [[nodiscard]] int64_t visible_midpoint() const noexcept;  // 0 = none
    // Peg reference per mode (MID / PRIMARY same-side / MARKET opposite),
    // resolved against the visible BBO. false = unavailable.
    [[nodiscard]] bool peg_reference(uint8_t mode, Side side,
                                     int64_t& ref_ticks) const noexcept;
    // Target pegged price: ref ± offset, clamped to the collar.
    [[nodiscard]] bool peg_target(const PegRec& rec, Side side,
                                  int64_t& price_ticks,
                                  int64_t& ref_ticks) const noexcept;
    // One reprice sweep over the peg table. Returns true when a reprice
    // committed (the caller re-runs until stable — a reprice can move the
    // reference another peg follows).
    bool repeg_pass() noexcept;
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
    void flush_depth() noexcept;
    // L2 depth coalesce window — the spec's feed contract is a 100ms
    // conflated top-20 stream (§10.2) that is explicitly allowed to lag the
    // internal book (§6.6b); 10ms stays 10x fresher than contract while
    // bounding serialize work to ~100 frames/s under sustained load.
    static constexpr uint64_t kDepthPubIntervalNs = 10'000'000;

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

    // --- Phase-15 auction / lifecycle helpers ---------------------------------
    // admission_gate: quarantine -> instrument status -> market-hours,
    // evaluated per new order (feed-bound) or against the static instrument
    // (feed-unbound dev/test mode). Returns the rejection code or nullptr.
    [[nodiscard]] const char* admission_gate(const Order& o) const noexcept;
    // Effective instrument status: feed-verified when bound (unverifiable
    // feed fails closed to SUSPENDED), else the static book instrument.
    [[nodiscard]] InstrumentStatus effective_status() const noexcept;

    // Auction machinery (Tasks 15.3.6/15.3.10). control_sync observes the
    // feed's armed key (arm / deadline-move / withdraw) at every ingress
    // event; deadline_check resolves the uncross on the logical clock
    // (on_time_tick only — a deadline is a clock condition, never driven by
    // ad-hoc ingress). on_auction_phase_replay routes journaled rows
    // through the same journal-free state machine so live and replayed
    // engines converge identically.
    void auction_control_sync() noexcept;
    void auction_deadline_check() noexcept;
    void auction_resolve_deadline() noexcept;
    bool auction_journal(uint8_t phase, uint8_t reason, int64_t price_ticks,
                         int64_t qty_units) noexcept;
    void auction_enter_call(int64_t deadline_ns) noexcept;
    void auction_exit(uint8_t phase, uint8_t reason) noexcept;
    // CALL-mode admission: rest GTC/GTD/DAY limits + icebergs directly into
    // the (crossed-tolerant) book, enqueue stops untriggered, park
    // MARKET/IOC/FOK orders for the uncross. Returns true when the node was
    // adopted by a container (book/stop/park), false on a cancel-reject.
    bool auction_accumulate(Order* order, const OrderAux& aux) noexcept;
    // Single-price uncross math + execution. auction_indicative computes the
    // max-executable-volume price over resting levels + parked orders
    // (iceberg hidden remainder counts); auction_uncross drains eligible
    // pairs at that price in strict price-time order (parked markets rank
    // ahead of all limits). dry_run applies no mutation — used to learn the
    // cleared volume BEFORE the UNCROSS WAL row is journaled.
    struct AuctionIndicative {
        int64_t price_ticks = 0;
        int64_t exec_qty_units = 0;
        int64_t buy_qty_units = 0;    // eligible buy qty at the clear price
        int64_t sell_qty_units = 0;
        bool    has_candidates = false;  // any limit price exists at all
    };
    [[nodiscard]] AuctionIndicative auction_indicative() const noexcept;
    int64_t auction_uncross(int64_t price_ticks, bool dry_run) noexcept;
    // Indicative publication — emits an AuctionEvent only when the
    // (price,qty) clearing pair changed since the last publish.
    void publish_auction_indicative() noexcept;
    // Quarantine (Task 15.3.10): engine-local halt — orders reject with
    // `code`, amends likewise, cancels unaffected.
    void quarantine_enter(const char* code, uint8_t reason) noexcept;
    void quarantine_check() noexcept;   // continuous-mode crossed-book probe
    // Parked-order list ops (intrusive FIFO over Order::next).
    bool parked_push(Order* o) noexcept;
    Order* parked_find(uint64_t order_id) noexcept;
    void parked_drain(uint8_t wal_reason) noexcept;  // cancel-all (terminal)
    // Effective remaining including the iceberg hidden remainder — same
    // accounting as effective_remaining_units but for the auction sweep.
    [[nodiscard]] int64_t effective_level_units(
        const PriceLevel& lvl, Side side) const noexcept;

    uint32_t shard_id_;
    OrderBook& book_;
    MemoryPool<Order>& orders_;
    WalWriter* wal_;
    IpcPublisher* publisher_;
    L3Publisher* l3_ = nullptr;             // Phase-17 L3 stream (spec §11)
    // Journal-free WAL-seq cursor for replay/anchor semantics — see
    // journal_seq()/set_replay_wal_seq() above.
    uint64_t replay_wal_seq_ = 0;

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

    // Phase-14 Task 14.3.1 — bounded OCO member table (ctor-allocated once,
    // nothrow; null => links reject fail-closed).
    OcoMember* oco_ = nullptr;
    std::size_t oco_cap_ = 0;
    std::size_t oco_live_ = 0;

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

    // --- Phase-15 instrument feed + auction state (Tasks 15.3.3–15.3.10) -----
    // Control-plane feed — nullptr in journal-free replay engines and
    // unbound test harnesses (gates then read the static instrument only).
    InstrumentFeed* feed_ = nullptr;  // mutable: result-request write slot
    // Armed-CALL state machine. auction_phase_ is kAuctionPhase* (None when
    // no auction is armed). auction_id_ is the consumed armed value's
    // deadline — identity dedupe: a completed/consumed deadline can never
    // re-arm (last_completed_auction_id_). auction_awaiting_ marks the
    // post-strike-failure window where the Go EXTEND ladder decides.
    uint8_t  auction_phase_ = kAuctionPhaseNone;
    int64_t  auction_id_ = 0;
    int64_t  auction_deadline_ns_ = 0;
    uint8_t  auction_extensions_ = 0;
    bool     auction_awaiting_ = false;
    int64_t  last_completed_auction_id_ = 0;
    // Defensive local cap mirroring the Go ladder (EXTEND ≤3 → FAILED).
    static constexpr uint8_t kAuctionMaxExtensions = 3;
    // Engine-local quarantine (crossed-book halt / clearing-fail suspend).
    bool        quarantined_ = false;
    const char* quarantine_code_ = nullptr;
    // Parked CALL-accumulated non-resting orders — intrusive FIFO over
    // Order::next (bounded; full => fail-closed BOOK_CAPACITY).
    static constexpr uint32_t kParkedOrderCap = 1024;
    Order*   parked_head_ = nullptr;
    Order*   parked_tail_ = nullptr;
    uint32_t parked_count_ = 0;
    // Indicative-price publish dedupe (emit only on change).
    int64_t  last_indicative_price_ = -1;
    int64_t  last_indicative_qty_ = -1;
    self_trade_fn self_trade_fn_ = nullptr;
    void* self_trade_ctx_ = nullptr;
    prevented_match_fn prevented_fn_ = nullptr;
    void* prevented_ctx_ = nullptr;
    fill_observe_fn fill_obs_fn_ = nullptr;
    void* fill_obs_ctx_ = nullptr;
    int64_t prevented_qty_total_ = 0;  // Task 2.3.18 engine-wide counter

    // --- Phase-16 state (Tasks 16.3.3/11/13/15/16/17/22/25) -------------------
    // Mark/index oracle feed — bound control-plane feed wins; the local
    // snapshot is the journal-free replay + unit-test seam.
    const PriceOracleFeed* oracle_feed_ = nullptr;
    int64_t oracle_mark_ticks_ = 0;
    int64_t oracle_mark_ts_ns_ = 0;
    int64_t oracle_index_ticks_ = 0;
    int64_t oracle_index_ts_ns_ = 0;
    bool    oracle_local_ok_ = false;
    uint64_t oracle_stale_suspensions_ = 0;
    // Pegged-order record table (compact array — see PegRec above).
    static constexpr std::size_t kPegsCap = 4096;
    PegRec* pegs_ = nullptr;
    std::size_t pegs_cap_ = 0;
    std::size_t pegs_live_ = 0;
    uint64_t peg_repriced_total_ = 0;
    uint64_t peg_unavailable_total_ = 0;
    // GSLO exposure pool accounting (armed guaranteed-stop notional).
    int64_t  gslo_notional_units_ = 0;
    uint64_t gslo_live_count_ = 0;
    uint64_t gslo_fills_ = 0;
    int64_t  gslo_max_exposure_units_ = 0;  // 0 = uncapped

    // Phase-22 Task 22.3.12 — implied matching. implied_ is the shared
    // matcher (co-located curve books, same thread); implied_registered_
    // latches the one-time register_book call; implied_dirty_ is the
    // host-drain flag the owner hook sets on matcher-produced fills.
    ImpliedMatcher* implied_ = nullptr;
    bool implied_registered_ = false;
    bool implied_dirty_ = false;

    // Bilateral credit screen (spec §3.3b): bound by bind_credit; both
    // null ⇒ gate inert. Walk-only consultation — consume_or_skip per
    // fill, skip-marker cursor preserves price-time priority.
    BilateralCreditMatrix* credit_ = nullptr;  // mutable: consume debits
    const IPartyMap* credit_map_ = nullptr;

    uint64_t received_count_ = 0;
    uint64_t trades_emitted_ = 0;
    uint64_t next_trade_id_ = 1;
    uint64_t* tid_stream_ = nullptr;  // shared curve stream when bound
    uint64_t emit_seq_ = 0;          // engine-generated priority stamps
    uint64_t now_ns_ = 0;            // logical clock (TIME_TICK driven)
    bool depth_dirty_ = false;       // coalesced L2 frame pending
    bool depth_ever_emitted_ = false;
    uint64_t last_depth_pub_ns_ = 0;
    int64_t last_price_ticks_ = 0;
    bool wal_fault_ = false;
    const char* last_reject_ = nullptr;
    uint64_t reject_count_ = 0;
};

}  // namespace exch
