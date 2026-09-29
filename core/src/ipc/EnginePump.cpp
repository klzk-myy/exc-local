// Task 2.3.7 — IPC pump implementation (see header for the contract).

#include "ipc/EnginePump.hpp"

#include <cstdio>

#include "ipc/IpcChannel.hpp"
#include "ipc/SharedMemChannel.hpp"
#include "matching/ExpiryScheduler.hpp"
#include "matching/MatchingEngine.hpp"
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
        default:
            return false;
    }
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
    uint32_t drained = 0;
    if (inbound_ == nullptr || !inbound_->is_open()) return 0;

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
    try {
#if EXCH_PUMP_WIRE
        flatbuffers::Verifier v(data, len);
        if (!v.VerifyBuffer<exc::wire::Event>()) {
            quarantine(data, len, "flatbuffers_verify_failed");
            return;
        }
        const exc::wire::Event* ev = exc::wire::GetEvent(data);
        if (ev == nullptr) {
            quarantine(data, len, "null_event");
            return;
        }
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
                if (!map_side(m->side(), &side) || !map_order_type(m->type(), &type) ||
                    !map_tif(m->tif(), &tif) || m->qty() <= 0 ||
                    (type != OrderType::MARKET && m->price() <= 0)) {
                    decode_errors_.fetch_add(1, std::memory_order_relaxed);
                    report("DECODE_ERROR", "OrderNew field validation failed; frame rejected");
                    break;
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
                o->flags = m->flags();  // bit0 post_only, bit1 reduce_only
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

                if (wal_ != nullptr && opts_.wal_log_commands) {
                    WalOrderNewPayload p{};
                    p.order_id = m->order_id();
                    p.account_id = m->account_id();
                    p.instrument_id = m->instrument_id();
                    p.side = static_cast<uint8_t>(m->side());
                    p.type = static_cast<uint8_t>(m->type());
                    p.tif = static_cast<uint8_t>(m->tif());
                    p.price_ticks = m->price();
                    p.qty_units = m->qty();
                    p.visible_qty_units = m->qty();
                    if (wal_->append(WalEventType::ORDER_NEW, &p,
                                     static_cast<uint32_t>(sizeof(p))) != WalStatus::Ok) {
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
                engine_->on_amend_received(m->order_id(), m->price(), m->qty(),
                                           m->stop_price(), ev->seq());
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
            default:
                // TradeFill/BookSnapshot/NONE inbound are outbound-only or
                // empty unions — protocol violation, counted not crashed.
                decode_errors_.fetch_add(1, std::memory_order_relaxed);
                break;
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
        return;
    }
    if (opts_.measure_latency) {
        const uint64_t dt = steady_ns() - t0;
        dispatch_latency_.record(dt);
    }
}

}  // namespace exch
