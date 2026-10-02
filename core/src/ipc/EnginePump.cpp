// Task 2.3.7 — IPC pump implementation (see header for the contract).

#include "ipc/EnginePump.hpp"

#include <cstdio>

#include "ipc/IpcChannel.hpp"
#include "ipc/SharedMemChannel.hpp"
#include "ipc/CtlDemux.hpp"
#include "matching/CrossShardCoordinator.hpp"
#include "matching/OptimisticShardCoordinator.hpp"
#include "matching/ExpiryScheduler.hpp"
#include "matching/IpcPublisher.hpp"
#include "matching/MatchingEngine.hpp"
#include "risk/CrossShardMarginCoordinator.h"
#include "utils/TimeUtils.hpp"
#include "wal/Wal.hpp"
#include "wal/WalEntry.hpp"

#if __has_include("exchange_generated.h") && __has_include(<flatbuffers/flatbuffers.h>)
#define EXCH_PUMP_WIRE 1
#include "exchange_generated.h"
#else
#define EXCH_PUMP_WIRE 0
#endif

namespace exch {

namespace {

// P1-style operational alert line — no allocation, no throw, cold path only.
void stderr_report(void* /*ctx*/, const char* code, const char* detail) {
    std::fprintf(stderr, "[P1] %s: %s\n", code != nullptr ? code : "?",
                 detail != nullptr ? detail : "");
}

void stderr_poison(void* /*ctx*/, const uint8_t* data, uint32_t len, const char* why) {
    // Bounded hex dump of the frame head — enough to identify the producer
    // bug without flooding the log under a garbage stream.
    char hex[2 * 32 + 1];
    const uint32_t n = len < 32 ? len : 32;
    for (uint32_t i = 0; i < n; ++i) std::snprintf(hex + 2 * i, 3, "%02x", data[i]);
    hex[2 * n] = '\0';
    std::fprintf(stderr, "[P1] POISON_PILL quarantined len=%u why=%s head=%s\n", len,
                 why != nullptr ? why : "?", hex);
}

#if EXCH_PUMP_WIRE
// Wire (exc::wire, proto/exchange.fbs) -> book enums. Malformed enum values
// fail closed: FlatBuffers VerifyField checks width, not enum range, so a
// corrupt/out-of-range value must be rejected here rather than coerced.
bool map_side(exc::wire::Side s, Side* out) noexcept {
    switch (s) {
        case exc::wire::Side_Buy:
            *out = Side::BUY;
            return true;
        case exc::wire::Side_Sell:
            *out = Side::SELL;
            return true;
        default:
            return false;
    }
}

bool map_order_type(exc::wire::OrderType t, OrderType* out) noexcept {
    switch (t) {
        case exc::wire::OrderType_Market:
            *out = OrderType::MARKET;
            return true;
        case exc::wire::OrderType_Limit:
            *out = OrderType::LIMIT;
            return true;
        case exc::wire::OrderType_StopMarket:
            *out = OrderType::STOP;
            return true;
        case exc::wire::OrderType_StopLimit:
            *out = OrderType::STOP_LIMIT;
            return true;
        case exc::wire::OrderType_Iceberg:
            *out = OrderType::ICEBERG;
            return true;
        case exc::wire::OrderType_Peg:
            // Phase-16 Task 16.3.11 — aux.peg_mode selects the reference.
            *out = OrderType::PEG;
            return true;
        case exc::wire::OrderType_Fixing:
            // Phase-16 Task 16.3.9 — FIXING orders are never dispatched to
            // the engine (the fixing executor crosses them off-wire); a
            // stray frame maps through and the engine rejects it
            // ORDER_INVALID — fail closed, never silent-drop.
            *out = OrderType::FIXING;
            return true;
        default:
            return false;
    }
}

// Phase-16 Tasks 16.3.13/16.3.16 — wire flag bits differ from the engine
// bitfield: the wire uses bit2=hidden/bit3=gslo while the engine reserves
// bits 2/3 for internal markers (market-with-protection, STP transfer) and
// keeps hidden/gslo on bits 4/5. Translate rather than memcpy so a wire
// bit can never masquerade as an internal marker.
[[nodiscard]] constexpr uint8_t translate_flags(uint8_t wire_flags) noexcept {
    uint8_t f = wire_flags & (kOrderFlagPostOnly | kOrderFlagReduceOnly);
    if ((wire_flags & 0x4u) != 0) f |= kOrderFlagHidden;
    if ((wire_flags & 0x8u) != 0) f |= kOrderFlagGslo;
    return f;
}

bool map_tif(exc::wire::TimeInForce t, TimeInForce* out) noexcept {
    switch (t) {
        case exc::wire::TimeInForce_GTC:
            *out = TimeInForce::GTC;
            return true;
        case exc::wire::TimeInForce_IOC:
            *out = TimeInForce::IOC;
            return true;
        case exc::wire::TimeInForce_FOK:
            *out = TimeInForce::FOK;
            return true;
        case exc::wire::TimeInForce_GTD:
            *out = TimeInForce::GTD;
            return true;
        case exc::wire::TimeInForce_DAY:
            *out = TimeInForce::DAY;
            return true;
        default:
            return false;
    }
}
#endif  // EXCH_PUMP_WIRE

// B1 ingress-surface detection. Requires-expressions only yield false on
// substitution failure — the engine type must be a dependent template
// parameter, so the probes live here as concepts, not inline in the adapter.
template <typename E>
concept EngineCancelIngress =
    requires(E* e, uint64_t a, uint64_t b) { e->on_cancel_received(a, b); };

template <typename E>
concept EngineTickIngress = requires(E* e, uint64_t t) { e->on_time_tick(t); };

// if constexpr only discards in a template context — the calls route through
// these dependent-type helpers so a missing member compiles to a counted
// no-op today and binds automatically once B1's engine lands.
template <typename E>
void call_cancel(E* e, uint64_t order_id, uint64_t account_id, uint64_t* unrouted) noexcept {
    if constexpr (EngineCancelIngress<E>) {
        e->on_cancel_received(order_id, account_id);
    } else {
        (void)e;
        (void)order_id;
        (void)account_id;
        ++*unrouted;
    }
}

template <typename E>
void call_tick(E* e, uint64_t now_ns, uint64_t* unrouted) noexcept {
    if constexpr (EngineTickIngress<E>) {
        e->on_time_tick(now_ns);
    } else {
        (void)e;
        (void)now_ns;
        ++*unrouted;
    }
}

}  // namespace

// --- MatchingEngineIngress ---------------------------------------------------

void MatchingEngineIngress::on_order_received(Order* order) noexcept {
    if (engine_ != nullptr) engine_->on_order_received(order);
}

void MatchingEngineIngress::on_order_received_ex(Order* order,
                                                 const OrderAux& aux) noexcept {
    if (engine_ != nullptr) engine_->on_order_received(order, aux);
}

void MatchingEngineIngress::on_cancel_received(uint64_t order_id, uint64_t account_id) noexcept {
    // B1 seam: on_cancel_received lands with Task 2.3.2.
    if (engine_ != nullptr) call_cancel(engine_, order_id, account_id, &unrouted_cancels_);
}

void MatchingEngineIngress::on_time_tick(uint64_t now_ns) noexcept {
    // B1 seam: engine clock tick for Task 2.3.10 expiry / stop-order sweeps.
    if (engine_ != nullptr) call_tick(engine_, now_ns, &unrouted_ticks_);
}

void MatchingEngineIngress::on_amend_received(uint64_t order_id,
                                              int64_t price_ticks,
                                              int64_t qty_units,
                                              int64_t stop_price_ticks,
                                              uint64_t ingress_seq) noexcept {
    if (engine_ != nullptr) {
        engine_->on_amend_received(order_id, price_ticks, qty_units,
                                   stop_price_ticks, ingress_seq);
    }
}

void MatchingEngineIngress::on_amend_received_ex(
    uint64_t order_id, const AmendWire& req, uint64_t ingress_seq) noexcept {
    if (engine_ != nullptr) {
        engine_->on_amend_received_ex(order_id, req, ingress_seq);
    }
}

void MatchingEngineIngress::on_oco_link_received(
    uint64_t link_id, uint64_t order_id_a, uint64_t order_id_b,
    uint64_t account_id, uint32_t instrument_id) noexcept {
    if (engine_ != nullptr) {
        engine_->on_oco_link_received(link_id, order_id_a, order_id_b,
                                      account_id, instrument_id);
    } else {
        ++unrouted_links_;
    }
}

// --- CurveIngress (Task 22.3.12) ---------------------------------------------

bool CurveIngress::add_engine(MatchingEngine* engine,
                              uint32_t instrument_id) noexcept {
    if (engine == nullptr || instrument_id == 0 || n_ >= kMaxEngines ||
        engine_for(instrument_id) != nullptr) {
        return false;
    }
    entries_[n_].engine = engine;
    entries_[n_].instrument_id = instrument_id;
    ++n_;
    return true;
}

MatchingEngine* CurveIngress::engine_for(uint32_t instrument_id) noexcept {
    for (uint32_t i = 0; i < n_; ++i) {
        if (entries_[i].instrument_id == instrument_id) {
            return entries_[i].engine;
        }
    }
    return nullptr;
}

MatchingEngine* CurveIngress::owner_of(uint64_t order_id) noexcept {
    for (uint32_t i = 0; i < n_; ++i) {
        if (entries_[i].engine->owns_order(order_id)) {
            return entries_[i].engine;
        }
    }
    return nullptr;
}

void CurveIngress::drain() noexcept {
    for (uint32_t pass = 0; pass < kMaxDrainPasses; ++pass) {
        bool any = false;
        for (uint32_t i = 0; i < n_; ++i) {
            if (entries_[i].engine->take_implied_dirty()) {
                entries_[i].engine->implied_sync();
                any = true;
            }
        }
        if (!any) return;
    }
    ++drain_saturated_;
}

void CurveIngress::on_order_received(Order* order) noexcept {
    // Plain form carries no instrument_id — routable only on a single-
    // engine binding; on a curve the wire always uses the aux form.
    if (n_ == 1) {
        entries_[0].engine->on_order_received(order);
    } else {
        ++unrouted_orders_;
        if (orders_ != nullptr && order != nullptr) orders_->free(order);
    }
    drain();
}

void CurveIngress::on_order_received_ex(Order* order,
                                        const OrderAux& aux) noexcept {
    MatchingEngine* e = engine_for(aux.instrument_id);
    if (e != nullptr) {
        e->on_order_received(order, aux);
    } else {
        ++unrouted_orders_;
        if (orders_ != nullptr && order != nullptr) orders_->free(order);
    }
    drain();
}

void CurveIngress::on_cancel_received(uint64_t order_id,
                                      uint64_t account_id) noexcept {
    MatchingEngine* e = owner_of(order_id);
    if (e != nullptr) {
        e->on_cancel_received(order_id, account_id);
    } else {
        ++unrouted_cancels_;
    }
    drain();
}

void CurveIngress::on_time_tick(uint64_t now_ns) noexcept {
    for (uint32_t i = 0; i < n_; ++i) {
        entries_[i].engine->on_time_tick(now_ns);
    }
    drain();
}

void CurveIngress::on_amend_received(uint64_t order_id, int64_t price_ticks,
                                     int64_t qty_units,
                                     int64_t stop_price_ticks,
                                     uint64_t ingress_seq) noexcept {
    MatchingEngine* e = owner_of(order_id);
    if (e != nullptr) {
        e->on_amend_received(order_id, price_ticks, qty_units,
                             stop_price_ticks, ingress_seq);
    } else {
        ++unrouted_amends_;
    }
    drain();
}

void CurveIngress::on_amend_received_ex(uint64_t order_id,
                                        const AmendWire& req,
                                        uint64_t ingress_seq) noexcept {
    MatchingEngine* e = owner_of(order_id);
    if (e != nullptr) {
        e->on_amend_received_ex(order_id, req, ingress_seq);
    } else {
        ++unrouted_amends_;
    }
    drain();
}

void CurveIngress::on_oco_link_received(uint64_t link_id, uint64_t order_id_a,
                                        uint64_t order_id_b,
                                        uint64_t account_id,
                                        uint32_t instrument_id) noexcept {
    MatchingEngine* e = engine_for(instrument_id);
    if (e != nullptr) {
        e->on_oco_link_received(link_id, order_id_a, order_id_b,
                                account_id, instrument_id);
    } else {
        ++unrouted_links_;
    }
    drain();
}

// --- LatencyHistogram ---------------------------------------------------------

void LatencyHistogram::record(uint64_t ns) noexcept {
    // floor(log2(ns)); ns==0 lands in bucket 0. clzll(0) is UB — the |1 guard.
    const uint32_t b = static_cast<uint32_t>(63 - __builtin_clzll(ns | 1));
    buckets[b].fetch_add(1, std::memory_order_relaxed);
}

uint64_t LatencyHistogram::total() const noexcept {
    uint64_t t = 0;
    for (uint32_t i = 0; i < kBuckets; ++i) t += buckets[i].load(std::memory_order_relaxed);
    return t;
}

uint64_t LatencyHistogram::bucket_upper_ns(uint32_t bucket) noexcept {
    if (bucket >= kBuckets) return UINT64_MAX;
    if (bucket == kBuckets - 1) return UINT64_MAX;  // saturating bucket
    return (uint64_t{1} << (bucket + 1)) - 1;
}

uint64_t LatencyHistogram::max_ns() const noexcept {
    for (int i = kBuckets - 1; i >= 0; --i)
        if (buckets[i].load(std::memory_order_relaxed) != 0)
            return bucket_upper_ns(static_cast<uint32_t>(i));
    return 0;
}

uint64_t LatencyHistogram::percentile_upper_ns(double q) const noexcept {
    const uint64_t t = total();
    if (t == 0) return 0;
    if (q < 0.0) q = 0.0;
    if (q > 1.0) q = 1.0;
    uint64_t want = static_cast<uint64_t>(q * static_cast<double>(t));
    if (want == 0) want = 1;
    uint64_t cum = 0;
    for (uint32_t i = 0; i < kBuckets; ++i) {
        cum += buckets[i].load(std::memory_order_relaxed);
        if (cum >= want) return bucket_upper_ns(i);
    }
    return bucket_upper_ns(kBuckets - 1);
}

// --- EnginePump ----------------------------------------------------------------

EnginePump::EnginePump(IpcChannel* inbound, IpcChannel* outbound, IEngineIngress* engine,
                       MemoryPool<Order>* orders, Wal* wal, const Options& opts) noexcept
    : inbound_(inbound),
      outbound_(outbound),
      engine_(engine),
      orders_(orders),
      wal_(wal),
      opts_(opts),
      shm_in_(dynamic_cast<SharedMemChannel*>(inbound)) {}

uint32_t EnginePump::run_once(uint32_t max_batch) noexcept {
    // Cross-shard service first: ctl frames drained + coordinator deadlines
    // enforced even while the order ring is closed/empty. The 500µs
    // optimistic-match window and the margin RPC's 500µs soft budget both
    // need sub-tick drain cadence — this runs every loop pass.
    service_cross_shard();

    uint32_t drained = 0;
    if (inbound_ == nullptr || !inbound_->is_open()) return drained;

    while (drained < max_batch) {
        if (shm_in_ != nullptr) {
            // Zero-copy fast path: dispatch reads the FlatBuffer directly out
            // of the ring slot; consume() commits only after dispatch so a
            // quarantined frame is still consumed exactly once.
            uint32_t len = 0;
            const uint8_t* p = shm_in_->poll_view(&len);
            if (p == nullptr) break;
            msgs_in_.fetch_add(1, std::memory_order_relaxed);
            dispatch(p, len);
            shm_in_->consume();
            ++drained;
        } else {
            const int32_t n = inbound_->poll(inbuf_, sizeof(inbuf_));
            if (n <= 0) {
                if (n < 0) {
                    poll_errors_.fetch_add(1, std::memory_order_relaxed);
                    report("IPC_POLL_ERROR", "inbound poll() error or oversized frame");
                }
                break;
            }
            msgs_in_.fetch_add(1, std::memory_order_relaxed);
            dispatch(inbuf_, static_cast<uint32_t>(n));
            ++drained;
        }
    }
    return drained;
}

void EnginePump::tick(uint64_t now_ns) noexcept {
    last_tick_ns_ = now_ns;  // ctl handling reads this between ticks
    if (wal_ != nullptr && opts_.wal_log_ticks) {
        WalTimeTickPayload p{};
        p.tick_ns = now_ns;
        if (wal_->append(WalEventType::TIME_TICK, &p, static_cast<uint32_t>(sizeof(p))) ==
            WalStatus::Ok) {
            wal_appends_.fetch_add(1, std::memory_order_relaxed);
        } else {
            // Fail-closed: an unlogged tick must not advance the engine clock —
            // replay would diverge (Task 2.3.10). The tick is skipped, not
            // silently applied.
            wal_failures_.fetch_add(1, std::memory_order_relaxed);
            report("WAL_APPEND_FAILED", "TIME_TICK append failed; tick skipped");
            return;
        }
    }
    ticks_.fetch_add(1, std::memory_order_relaxed);
    if (engine_ != nullptr) engine_->on_time_tick(now_ns);
}

bool EnginePump::send_outbound(const void* data, uint32_t len) noexcept {
    if (outbound_ == nullptr || !outbound_->is_open()) {
        note_outbound_drop();
        return false;
    }
    if (!outbound_->send(data, len)) {
        note_outbound_drop();
        return false;
    }
    msgs_out_.fetch_add(1, std::memory_order_relaxed);
    return true;
}

// --- Phase-3 Task 4 — cross-shard service ------------------------------------
//
// One physical ctl channel feeds BOTH protocols through CtlDemux
// (kBasketCtlMagic / kOptCtlMagic queues). Each coordinator then drains its
// own queue inside its unchanged on_time_tick — which also runs the
// reaper/deadline sweep gated on its own cadence. last_tick_ns_ is the
// engine-logical clock (WAL TIME_TICK discipline — deterministic replay).
void EnginePump::service_cross_shard() noexcept {
    if (xctl_demux_ != nullptr) xctl_demux_->drain();
    if (opt_coord_ != nullptr) opt_coord_->on_time_tick(last_tick_ns_);
    if (basket_coord_ != nullptr) basket_coord_->on_time_tick(last_tick_ns_);
    if (margin_coord_ != nullptr) margin_coord_->poll();

    // Terminal sweep: emit Event{BasketResult} once per settled op. The
    // coordinator keeps the op's cached result for wire-level dedup; this
    // tracker exists only to push the outcome onto the outbound ring.
    if (opt_coord_ == nullptr) return;
    uint32_t i = 0;
    while (i < pending_baskets_n_) {
        const PendingBasket& p = pending_baskets_[i];
        const OptResult r =
            opt_coord_->status(BasketOpId{p.op_hi, p.op_lo});
        const auto st = static_cast<OptStatus>(r.status);
        if (st == OptStatus::Committed || st == OptStatus::Compensated ||
            st == OptStatus::Failed || st == OptStatus::Rejected) {
            emit_basket_result(p.op_hi, p.op_lo, p.account_id, r.status,
                               r.code, r.leg_count, r.legs_filled,
                               r.legs_unwound, r.slippage_ticks,
                               last_tick_ns_ - p.start_ns);
            pending_baskets_[i] = pending_baskets_[--pending_baskets_n_];
            continue;  // re-examine the swapped-in entry
        }
        ++i;
    }
}

void EnginePump::emit_basket_result(uint64_t op_hi, uint64_t op_lo,
                                    uint64_t account_id, uint8_t status,
                                    uint8_t code, uint8_t leg_count,
                                    uint8_t legs_filled, uint8_t legs_unwound,
                                    int64_t slippage_ticks,
                                    uint64_t duration_ns) noexcept {
    if (out_pub_ == nullptr || !out_pub_->bound()) {
        note_outbound_drop();
        return;
    }
    (void)out_pub_->publish_basket_result(op_hi, op_lo, account_id, status,
                                          code, leg_count, legs_filled,
                                          legs_unwound, slippage_ticks,
                                          duration_ns, now_ns());
}

void EnginePump::note_outbound_drop() noexcept {
    const uint64_t d = overload_drops_.fetch_add(1, std::memory_order_relaxed) + 1;
    // Emit at the first drop then once per overload_report_every — sustained
    // saturation pages once per window, never spams.
    const uint64_t stride = opts_.overload_report_every == 0 ? 1 : opts_.overload_report_every;
    if (d == 1 || d - last_overload_report_ >= stride) {
        last_overload_report_ = d;
        overload_reports_.fetch_add(1, std::memory_order_relaxed);
        report("ENGINE_OVERLOAD",
               "outbound ring full; fill/book event dropped — gateway "
               "reconciles via WAL/snapshot replay (spec §2.3)");
    }
}

uint64_t EnginePump::inbound_occupancy() const noexcept {
    return shm_in_ != nullptr ? shm_in_->occupancy() : 0;
}

uint64_t EnginePump::inbound_capacity() const noexcept {
    return shm_in_ != nullptr ? shm_in_->inbound().capacity() : 0;
}

void EnginePump::set_poison_sink(poison_sink_fn fn, void* ctx) noexcept {
    poison_sink_ = fn;
    poison_ctx_ = ctx;
}

void EnginePump::set_report_sink(report_fn fn, void* ctx) noexcept {
    report_sink_ = fn;
    report_ctx_ = ctx;
}

void EnginePump::report(const char* code, const char* detail) noexcept {
    try {
        if (report_sink_ != nullptr)
            report_sink_(report_ctx_, code, detail);
        else
            stderr_report(nullptr, code, detail);
    } catch (...) {
    }  // alert path never propagates into the matching loop
}

void EnginePump::quarantine(const uint8_t* data, uint32_t len, const char* why) noexcept {
    poison_pills_.fetch_add(1, std::memory_order_relaxed);
    try {
        if (poison_sink_ != nullptr)
            poison_sink_(poison_ctx_, data, len, why);
        else
            stderr_poison(nullptr, data, len, why);
    } catch (...) {
    }
}

// Per-message boundary: verify -> decode -> dispatch, fully noexcept. Any
// failure path quarantines the frame (poison pill) instead of crashing the
// matching thread (Task 2.3.19, spec §2.7).
void EnginePump::dispatch(const uint8_t* data, uint32_t len) noexcept {
    const uint64_t t0 = opts_.measure_latency ? steady_ns() : 0;
    // Task 9.3.11 — EXCTRACE header (byte contract: ipc/TraceContext.hpp):
    // a magic'd 64B block at offset 0 shifts the FlatBuffers payload to
    // +kTraceBlockLen. A valid traceparent arms trace_slot_ for the echo
    // window (publishers copy the block verbatim onto response frames) and
    // queues an `order.match` span; a malformed parent still decodes at
    // +64 — untraced, never a fault (spec §2.7).
    CoreSpanContext tsc{};
    const bool block_present = has_trace_block(data, len);
    const bool traced =
        block_present && parse_trace_context(data, len, tsc);
    if (block_present) {
        if (traced) {
            trace_slot_.arm(tsc.block);
            traced_frames_in_.fetch_add(1, std::memory_order_relaxed);
        }
        data += kTraceBlockLen;
        len -= kTraceBlockLen;
    }
    struct TraceDisarm {  // disarm on every exit path, noexcept by shape
        TraceSlot* slot;
        ~TraceDisarm() {
            if (slot != nullptr) slot->disarm();
        }
    } disarm{traced ? &trace_slot_ : nullptr};
    const uint64_t span_start_ns = traced ? now_ns() : 0;
    uint8_t wire_event_type = 0xFF;
    try {
#if EXCH_PUMP_WIRE
        flatbuffers::Verifier v(data, len);
        const exc::wire::Event* ev = nullptr;
        if (!v.VerifyBuffer<exc::wire::Event>()) {
            quarantine(data, len, "flatbuffers_verify_failed");
        } else if ((ev = exc::wire::GetEvent(data)) == nullptr) {
            quarantine(data, len, "null_event");
        } else {
        wire_event_type = static_cast<uint8_t>(ev->type_type());
        switch (ev->type_type()) {
            case exc::wire::EventType_OrderNew: {
                const exc::wire::OrderNew* m = ev->type_as_OrderNew();
                if (m == nullptr) {
                    quarantine(data, len, "ordernew_payload_missing");
                    break;
                }
                if (shed_new_orders_) {
                    // >80% watermark (EngineLoop policy): consume-but-reject
                    // new orders — no side-effects, gateway retries by
                    // client_order_id (CAPACITY_EXCEEDED family, HTTP 503).
                    const uint64_t sd =
                        shed_drops_.fetch_add(1, std::memory_order_relaxed) + 1;
                    const uint64_t stride =
                        opts_.overload_report_every == 0 ? 1 : opts_.overload_report_every;
                    if (sd == 1 || sd - last_shed_report_ >= stride) {
                        last_shed_report_ = sd;
                        report("CAPACITY_EXCEEDED", "shedding inbound OrderNew (>80% watermark)");
                    }
                    break;
                }
                Side side;
                OrderType type;
                TimeInForce tif;
                const uint8_t trail_unit = m->trailing_offset_unit();
                if (!map_side(m->side(), &side) || !map_order_type(m->type(), &type) ||
                    !map_tif(m->tif(), &tif) || m->qty() <= 0 ||
                    m->trigger_source() > kTriggerSourceIndex ||
                    m->peg_mode() > kPegMarket ||
                    trail_unit > kTrailUnitAbsolute ||
                    // price is required unless the type derives/lacks a
                    // limit (MARKET, PEG) or never rests at a wire price
                    // (TRAILING_STOP via trailing_offset_unit, FIXING —
                    // engine rejects it regardless).
                    (type != OrderType::MARKET && type != OrderType::PEG &&
                     type != OrderType::FIXING && trail_unit == 0 &&
                     m->price() <= 0)) {
                    decode_errors_.fetch_add(1, std::memory_order_relaxed);
                    report("DECODE_ERROR", "OrderNew field validation failed; frame rejected");
                    break;
                }
                // Phase-16 Tasks 16.3.3/16.3.15 — the wire has no trailing
                // order type: StopMarket + trailing_offset_unit != 0 IS the
                // trailing-stop encoding. Any other base type carrying a
                // trail distance is malformed.
                if (trail_unit != kTrailUnitNone) {
                    if (type != OrderType::STOP) {
                        decode_errors_.fetch_add(1, std::memory_order_relaxed);
                        report("DECODE_ERROR",
                               "trailing_offset_unit set on non-StopMarket OrderNew");
                        break;
                    }
                    type = OrderType::TRAILING_STOP;
                }
                if (engine_ == nullptr || orders_ == nullptr) {
                    unrouted_drops_.fetch_add(1, std::memory_order_relaxed);
                    report("ENGINE_UNWIRED", "no ingress/pool bound; OrderNew dropped");
                    break;
                }
                Order* o = orders_->alloc();
                if (o == nullptr) {
                    // L2 capacity boundary (spec §3.6.1): atomic reject.
                    order_alloc_failures_.fetch_add(1, std::memory_order_relaxed);
                    report("ORDER_BOOK_CAPACITY_EXCEEDED",
                           "order pool exhausted; OrderNew rejected");
                    break;
                }
                o->id = m->order_id();
                o->account_id = m->account_id();
                o->side = side;
                o->type = type;
                o->tif = tif;
                // stp_mode: 0xFF wire sentinel = unset -> resolve order ->
                // account default -> CANCEL_NEWEST in pre-trade (2.3.21).
                o->stp_mode = static_cast<StpMode>(m->stp_mode());
                // bit0 post_only, bit1 reduce_only pass through verbatim;
                // wire bit2/3 (hidden/gslo) translate to engine bits 4/5 —
                // engine bits 2/3 are internal-only markers.
                o->flags = translate_flags(m->flags());
                o->price_ticks = m->price();
                o->qty_units = m->qty();
                o->filled_qty_units = 0;
                o->display_qty_units = m->display_qty();  // 0 = engine default
                o->quantity = Decimal::from_mantissa(m->qty());  // compat mirror
                // Fairness stamps: ingress wall ts + per-shard wire seq —
                // the deterministic (price, ts, ingress_seq) tie-break
                // (Task 2.3.20). instrument_id routes at the shard level —
                // one book per engine instance today.
                o->timestamp_ns = ev->ts();
                o->ingress_seq = ev->seq();
                o->next = o->prev = o->hash_next = nullptr;

                // OrderAux — wire fields with no slot on the POD Order node.
                // DAY expiry resolves in-core from the canonical 24/5 session
                // calendar when the gateway sends gtd_expiry_ns == 0.
                OrderAux aux{};
                aux.stop_price_ticks = m->stop_price();
                aux.gtd_expiry_ns = m->gtd_expiry_ns();
                if (tif == TimeInForce::DAY && aux.gtd_expiry_ns == 0) {
                    aux.gtd_expiry_ns = static_cast<int64_t>(
                        ExpiryScheduler::day_expiry_ns(ev->ts()));
                }
                aux.trade_group_id = m->trade_group_id();
                aux.instrument_id = m->instrument_id();
                aux.discretionary_offset_pips = m->discretionary_offset_pips();
                // Phase-16 aux (Tasks 16.3.3/11/15/17): the wire ordinals
                // are the engine kTriggerSource*/kPeg*/kTrailUnit* values
                // verbatim — ranged above, so a plain assignment is safe.
                aux.trigger_source = m->trigger_source();
                aux.peg_mode = m->peg_mode();
                aux.trail_unit = trail_unit;
                aux.peg_offset_ticks = m->peg_offset();
                aux.peg_limit_ticks = m->peg_limit();
                aux.trail_distance = m->trailing_offset();
                aux.activation_price_ticks = m->activation_price();

                if (wal_ != nullptr && opts_.wal_log_commands) {
                    // Route through WalWriter so the command log emits the
                    // ORDER_NEW_EX extension payload whenever the aux
                    // carries Phase-16 fields — a hand-rolled legacy
                    // ORDER_NEW would drop them and diverge on replay.
                    WalWriter cmd_log(wal_);
                    const WalStatus ws = cmd_log.write_order_new(
                        *o, aux, m->instrument_id(), m->qty(), ev->ts());
                    if (ws != WalStatus::Ok) {
                        wal_failures_.fetch_add(1, std::memory_order_relaxed);
                        orders_->free(o);
                        report("WAL_APPEND_FAILED", "ORDER_NEW append failed; command not applied");
                        break;
                    }
                    wal_appends_.fetch_add(1, std::memory_order_relaxed);
                }
                engine_->on_order_received_ex(o, aux);
                msgs_dispatched_.fetch_add(1, std::memory_order_relaxed);
                break;
            }
            case exc::wire::EventType_OrderCancel: {
                const exc::wire::OrderCancel* m = ev->type_as_OrderCancel();
                if (m == nullptr) {
                    quarantine(data, len, "ordercancel_payload_missing");
                    break;
                }
                // Cancels are never shed — they release capacity and risk.
                if (wal_ != nullptr && opts_.wal_log_commands) {
                    WalOrderCancelPayload p{};
                    p.order_id = m->order_id();
                    p.account_id = m->account_id();
                    p.reason = 0;  // user-initiated
                    if (wal_->append(WalEventType::ORDER_CANCEL, &p,
                                     static_cast<uint32_t>(sizeof(p))) != WalStatus::Ok) {
                        wal_failures_.fetch_add(1, std::memory_order_relaxed);
                        report("WAL_APPEND_FAILED",
                               "ORDER_CANCEL append failed; command not applied");
                        break;
                    }
                    wal_appends_.fetch_add(1, std::memory_order_relaxed);
                }
                if (engine_ == nullptr) {
                    unrouted_drops_.fetch_add(1, std::memory_order_relaxed);
                    report("ENGINE_UNWIRED", "no ingress bound; OrderCancel dropped");
                    break;
                }
                engine_->on_cancel_received(m->order_id(), m->account_id());
                msgs_dispatched_.fetch_add(1, std::memory_order_relaxed);
                break;
            }
            case exc::wire::EventType_TimeTick: {
                const exc::wire::TimeTick* m = ev->type_as_TimeTick();
                if (m == nullptr) {
                    quarantine(data, len, "timetick_payload_missing");
                    break;
                }
                if (engine_ == nullptr) {
                    unrouted_drops_.fetch_add(1, std::memory_order_relaxed);
                    break;
                }
                engine_->on_time_tick(m->tick_ns());
                msgs_dispatched_.fetch_add(1, std::memory_order_relaxed);
                break;
            }
            case exc::wire::EventType_OrderAmend: {
                const exc::wire::OrderAmend* m = ev->type_as_OrderAmend();
                if (m == nullptr) {
                    quarantine(data, len, "orderamend_payload_missing");
                    break;
                }
                if (engine_ == nullptr) {
                    unrouted_drops_.fetch_add(1, std::memory_order_relaxed);
                    report("ENGINE_UNWIRED", "no ingress bound; OrderAmend dropped");
                    break;
                }
                // Full AmendWire forward (Phase-3 Task 2): the legacy
                // 5-arg call silently dropped the order_seq stale-fence,
                // the gtd_expiry_ns re-arm, display_qty and trigger_source
                // — the fields were on the wire but unreachable.
                IEngineIngress::AmendWire aw{};
                aw.order_seq = m->order_seq();
                aw.price_ticks = m->price();
                aw.qty_units = m->qty();
                aw.stop_price_ticks = m->stop_price();
                aw.gtd_expiry_ns = m->gtd_expiry_ns();
                aw.display_qty_units = m->display_qty();
                // Wire value passes through verbatim: the Go encoder
                // always writes the field (0xFF = unchanged), so a wire
                // 0 is an explicit amend-back-to-LAST_PRICE request —
                // remapping it to 0xFF would silently drop the switch.
                aw.trigger_source = m->trigger_source();
                engine_->on_amend_received_ex(m->order_id(), aw, ev->seq());
                msgs_dispatched_.fetch_add(1, std::memory_order_relaxed);
                break;
            }
            case exc::wire::EventType_OcoLink: {
                const exc::wire::OcoLink* m = ev->type_as_OcoLink();
                if (m == nullptr) {
                    quarantine(data, len, "ocolink_payload_missing");
                    break;
                }
                // Links are never shed — they precede both legs' OrderNew
                // and each leg carries its own backpressure handling.
                if (engine_ == nullptr) {
                    unrouted_drops_.fetch_add(1, std::memory_order_relaxed);
                    report("ENGINE_UNWIRED", "no ingress bound; OcoLink dropped");
                    break;
                }
                engine_->on_oco_link_received(m->link_id(), m->order_id_a(),
                                              m->order_id_b(), m->account_id(),
                                              m->instrument_id());
                msgs_dispatched_.fetch_add(1, std::memory_order_relaxed);
                break;
            }
            case exc::wire::EventType_BasketSubmit: {
                const exc::wire::BasketSubmit* m = ev->type_as_BasketSubmit();
                if (m == nullptr) {
                    quarantine(data, len, "basketsubmit_payload_missing");
                    break;
                }
                if (opt_coord_ == nullptr) {
                    unrouted_drops_.fetch_add(1, std::memory_order_relaxed);
                    report("ENGINE_UNWIRED",
                           "no cross-shard coordinator bound; BasketSubmit dropped");
                    break;
                }
                const auto* wlegs = m->legs();
                const uint32_t n =
                    wlegs != nullptr ? wlegs->size() : 0;
                if (n < 2 || n > CrossShardCoordinator::kMaxLegs ||
                    m->account_id() == 0 ||
                    (m->op_id_hi() == 0 && m->op_id_lo() == 0)) {
                    decode_errors_.fetch_add(1, std::memory_order_relaxed);
                    report("DECODE_ERROR", "BasketSubmit gate failed (legs/account/op_id)");
                    break;
                }
                OptLegSpec legs[CrossShardCoordinator::kMaxLegs]{};
                bool bad = false;
                for (uint32_t i = 0; i < n; ++i) {
                    const auto* l = wlegs->Get(i);
                    if (l == nullptr || l->qty() <= 0 || l->side() > 1) {
                        bad = true;
                        break;
                    }
                    legs[i].shard_id = l->shard_id();
                    legs[i].instrument_id = l->instrument_id();
                    legs[i].account_id = m->account_id();
                    legs[i].order_id = l->order_id();
                    legs[i].qty_units = l->qty();
                    legs[i].limit_price_ticks = l->limit_price();
                    legs[i].side = static_cast<uint8_t>(l->side());
                }
                if (bad) {
                    decode_errors_.fetch_add(1, std::memory_order_relaxed);
                    report("DECODE_ERROR", "BasketSubmit leg validation failed");
                    break;
                }
                const BasketOpId op{m->op_id_hi(), m->op_id_lo()};
                const OptResult r = opt_coord_->submit(
                    legs, n, m->account_id(), op, last_tick_ns_);
                const auto st = static_cast<OptStatus>(r.status);
                if (st == OptStatus::Committed || st == OptStatus::Compensated ||
                    st == OptStatus::Failed || st == OptStatus::Rejected) {
                    // Gate rejections + synchronous resolutions publish
                    // immediately — no tracker slot spent.
                    emit_basket_result(op.hi, op.lo, m->account_id(),
                                       r.status, r.code, r.leg_count,
                                       r.legs_filled, r.legs_unwound,
                                       r.slippage_ticks, 0);
                } else if (pending_baskets_n_ < kMaxPendingBaskets) {
                    pending_baskets_[pending_baskets_n_++] =
                        PendingBasket{op.hi, op.lo, m->account_id(),
                                      last_tick_ns_};
                } else {
                    // Tracker exhausted — the op still runs to completion
                    // coordinator-side; the client reconciles via leg fills
                    // and coordinator metrics. Loud, not silent.
                    report("ENGINE_OVERLOAD",
                           "pending basket tracker full; BasketResult will not be emitted");
                }
                msgs_dispatched_.fetch_add(1, std::memory_order_relaxed);
                break;
            }
            default:
                // TradeFill/BookSnapshot/NONE inbound are outbound-only or
                // empty unions — protocol violation, counted not crashed.
                decode_errors_.fetch_add(1, std::memory_order_relaxed);
                break;
        }
        }
#else
        // Built without FlatBuffers headers: cannot decode — fail closed and
        // count every frame as unrouted rather than guess at bytes.
        (void)data;
        (void)len;
        unrouted_drops_.fetch_add(1, std::memory_order_relaxed);
#endif
    } catch (...) {
        // Structured poison-pill interceptor (Task 2.3.19): a decode/dispatch
        // fault quarantines the frame; the matching thread never crashes.
        quarantine(data, len, "dispatch_exception");
    }
    // Task 9.3.11 — emit the engine's `order.match` span, remote-parented
    // on the frame's traceparent. Emitted for every traced frame that
    // reached dispatch — including shed/quarantined ones: the engine leg
    // ran, and the sink is where the tail-sampling decision belongs.
    if (traced && span_sink_ != nullptr) {
        CoreSpan s{};
        std::snprintf(s.name, sizeof(s.name), "order.match");
        std::memcpy(s.trace_id, tsc.trace_id, sizeof(s.trace_id));
        std::memcpy(s.parent_span_id, tsc.parent_span_id,
                    sizeof(s.parent_span_id));
        render_hex16(mint_span_id(++span_salt_ ^ span_start_ns), s.span_id);
        s.kind = kSpanKindConsumer;
        s.start_unix_ns = span_start_ns;
        s.end_unix_ns = now_ns();
        s.event_type = wire_event_type;
        s.sampled = tsc.sampled;
        try {
            span_sink_(span_ctx_, s);
        } catch (...) {
        }  // a throwing sink must not propagate into the matching loop
        spans_emitted_.fetch_add(1, std::memory_order_relaxed);
    }
    if (opts_.measure_latency) {
        const uint64_t dt = steady_ns() - t0;
        dispatch_latency_.record(dt);
    }
}

}  // namespace exch
