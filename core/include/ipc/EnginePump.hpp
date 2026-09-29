#pragma once

// Task 2.3.7 — IPC pump for the matching core (spec §2.3, §3.5, §2.7).
//
//   Go gateway --[ Event{OrderNew | OrderCancel} ]--> inbound IpcChannel
//   core      --[ Event{TradeFill | BookSnapshot} ]--> outbound IpcChannel
//
// EnginePump::run_once drains framed inbound messages on the single matching
// thread: verify the FlatBuffers Event envelope -> decode -> dispatch into
// IEngineIngress (OrderNew allocates an Order from the shard pool; OrderCancel
// routes by id). No heap allocation, no exceptions escaping — malformed frames
// are quarantined as poison pills, never crash the loop (Task 2.3.19).
//
// Backpressure contract (Task 2.3.7 DoD + spec §2.7.3):
//   * Inbound: the pump always drains what poll() yields — the SPSC transport
//     applies backpressure to the producer when the ring fills. The >80% shed /
//     >95% halt watermarks are applied by EngineLoop (it owns the cadence);
//     when it sets shed mode the pump consumes-but-rejects OrderNew (cancels
//     always pass — they release capacity).
//   * Outbound: engine fills are emitted by B1's IpcPublisher through the
//     outbound channel. A send() == false means the ring is FULL — the message
//     is NOT queued. Every failure bumps overload_drops and every
//     overload_report_every drops emits one ENGINE_OVERLOAD alert (P1 line,
//     no throw). IpcPublisher must route outbound sends through
//     EnginePump::send_outbound()/note_outbound_drop() (or replicate this
//     accounting) so the metric and the alert stay coherent.
//   * Drop->retry contract: inbound commands the pump sheds/rejects carry no
//     application-level ACK, so the Go gateway retries by client_order_id
//     (dedup upstream); a missed outbound fill/book update is recovered by the
//     gateway from the WAL/snapshot replay stream on reconnect (Phase-04
//     seam) — the ENGINE_OVERLOAD alert exists so a sustained gap pages P1
//     instead of silently desynchronizing the gateway's order state.
//
// Durability: when a Wal is bound and Options::wal_log_ticks is set, tick()
// stamps+appends a TIME_TICK entry BEFORE driving the engine clock (Task
// 2.3.10, remediation #4 — deterministic replay: on recovery, time advances
// exactly as recorded). Default OFF because B1's engine-side WalWriter owns
// the journal; see Options.
//
// INTEGRATION NOTE (B1 / Task 2.3.2): MatchingEngine MUST inherit
// IEngineIngress — on_order_received is the pinned signature;
// on_cancel_received(order_id, account_id) and on_time_tick(now_ns) are the
// cancel/expiry seams this pump calls. MatchingEngineIngress below adapts a
// concrete MatchingEngine* and keeps compiling before B1 lands (member
// detection -> counted no-ops), so the pump is testable standalone with any
// recording fake implementing IEngineIngress.
// B1's WalWriter.hpp additionally defines OrderAux (stop_price, gtd_expiry,
// trade_group_id, instrument_id) marked "populated by ingress": the OrderNew
// wire fields that have no slot on the POD Order node. When B1 extends the
// ingress signature to carry it, OrderNew::instrument_id is decoded in
// dispatch() ready to forward.

#include <atomic>
#include <cstdint>

#include "book/Order.hpp"
#include "matching/WalWriter.hpp"  // OrderAux — wire-absent order fields
#include "utils/MemoryPool.hpp"

namespace exch {

class IpcChannel;
class SharedMemChannel;
class Wal;
class MatchingEngine;

// --- Pump -> engine seam ----------------------------------------------------

class IEngineIngress {
   public:
    virtual ~IEngineIngress() = default;

    // Order* is pool-owned; the engine/book owns its lifecycle from here.
    virtual void on_order_received(Order* order) noexcept = 0;
    // Rich form: aux carries the wire-absent fields (stop_price, gtd_expiry,
    // trade_group, instrument_id). Default forwards to the plain form so
    // test fakes stay minimal; MatchingEngine overrides.
    virtual void on_order_received_ex(Order* order, const OrderAux& aux) noexcept {
        (void)aux;
        on_order_received(order);
    }
    virtual void on_cancel_received(uint64_t order_id, uint64_t account_id) noexcept = 0;
    // Deterministic engine clock for GTD/DAY expiry + stop-order sweeps.
    virtual void on_time_tick(uint64_t now_ns) noexcept = 0;
    // Atomic amend/replace (Task 2.3.20). order_seq is the expected engine
    // seq for STALE_MODIFY races — unused until that task wires it.
    virtual void on_amend_received(uint64_t order_id, int64_t price_ticks,
                                   int64_t qty_units, int64_t stop_price_ticks,
                                   uint64_t ingress_seq) noexcept {
        (void)order_id; (void)price_ticks; (void)qty_units;
        (void)stop_price_ticks; (void)ingress_seq;
    }
    // OCO pair linkage command (Phase-14 Task 14.3.1, spec §6.2/§6.5 —
    // wire OcoLink, union member 7). Sequenced BEFORE both legs' OrderNew
    // on the shard ring: the engine installs the bounded link while both
    // legs are still unplaced. Default no-op keeps standalone fakes
    // compiling; MatchingEngine overrides with the journaled install.
    virtual void on_oco_link_received(uint64_t link_id, uint64_t order_id_a,
                                      uint64_t order_id_b, uint64_t account_id,
                                      uint32_t instrument_id) noexcept {
        (void)link_id; (void)order_id_a; (void)order_id_b;
        (void)account_id; (void)instrument_id;
    }
};

// Adapter binding the concrete Task 2.3.2 MatchingEngine to IEngineIngress.
// Until B1 lands the full ingress surface, C++20 member detection degrades
// missing entry points to counted no-ops — never an implicit drop.
class MatchingEngineIngress final : public IEngineIngress {
   public:
    MatchingEngineIngress() = default;
    explicit MatchingEngineIngress(MatchingEngine* engine) noexcept : engine_(engine) {}

    void bind(MatchingEngine* engine) noexcept { engine_ = engine; }

    void on_order_received(Order* order) noexcept override;
    void on_order_received_ex(Order* order, const OrderAux& aux) noexcept override;
    void on_cancel_received(uint64_t order_id, uint64_t account_id) noexcept override;
    void on_time_tick(uint64_t now_ns) noexcept override;
    void on_amend_received(uint64_t order_id, int64_t price_ticks,
                           int64_t qty_units, int64_t stop_price_ticks,
                           uint64_t ingress_seq) noexcept override;
    void on_oco_link_received(uint64_t link_id, uint64_t order_id_a,
                              uint64_t order_id_b, uint64_t account_id,
                              uint32_t instrument_id) noexcept override;

    // Ingress calls that had nowhere to go (pre-B1 stub engine).
    [[nodiscard]] uint64_t unrouted_cancels() const noexcept { return unrouted_cancels_; }
    [[nodiscard]] uint64_t unrouted_ticks() const noexcept { return unrouted_ticks_; }
    [[nodiscard]] uint64_t unrouted_links() const noexcept { return unrouted_links_; }

   private:
    MatchingEngine* engine_ = nullptr;
    uint64_t unrouted_cancels_ = 0;
    uint64_t unrouted_ticks_ = 0;
    uint64_t unrouted_links_ = 0;
};

// --- Bounded latency histogram ----------------------------------------------
// HDR-style log2 buckets, fixed storage — zero allocation. Bucket i counts
// samples in [2^i, 2^{i+1}) ns; bucket 63 saturates at >= 2^62 ns.

struct LatencyHistogram {
    static constexpr uint32_t kBuckets = 64;

    void record(uint64_t ns) noexcept;
    [[nodiscard]] uint64_t total() const noexcept;
    // Upper bound (ns) of the highest non-empty bucket — approx max.
    [[nodiscard]] uint64_t max_ns() const noexcept;
    // Smallest bucket whose cumulative count reaches ceil(q*total); returns
    // that bucket's upper bound in ns (0 when empty). q in [0,1].
    [[nodiscard]] uint64_t percentile_upper_ns(double q) const noexcept;
    [[nodiscard]] static uint64_t bucket_upper_ns(uint32_t bucket) noexcept;

    std::atomic<uint64_t> buckets[kBuckets] = {};
};

// --- EnginePump ---------------------------------------------------------------

class EnginePump {
   public:
    // Inbound frames larger than this are rejected by poll() (ShmRing leaves
    // an oversized slot pending — fail closed rather than truncate a command).
    static constexpr uint32_t kMaxInboundBytes = 64 * 1024;

    struct Options {
        // Stamp+append TIME_TICK to the WAL on every tick() before driving
        // engine->on_time_tick. DEFAULT OFF — B1's engine-side
        // WalWriter::write_time_tick is the authoritative stamp site (Task
        // 2.3.10: the matching thread stamps+appends); enabling both
        // double-logs ticks. Enable only when the bound engine does not
        // journal TIME_TICK itself (standalone pump, stub engine, tests).
        bool wal_log_ticks = false;
        // Append ORDER_NEW/ORDER_CANCEL command records before dispatch.
        // DEFAULT OFF — B1's WalWriter owns the durable state-change journal
        // (Task 2.3.2); enabling both double-logs commands which would apply
        // twice on replay. Enable only for a dedicated ingress-log segment.
        bool wal_log_commands = false;
        // ENGINE_OVERLOAD stride — one P1 alert per N outbound drops.
        uint64_t overload_report_every = 1024;
        // Per-message dispatch latency sampling into latency_hist.
        bool measure_latency = true;
    };

    // All pointers are borrowed and must outlive the pump; inbound/engine are
    // required for useful work but nullptr is tolerated (fail-closed: nothing
    // is dispatched, counters show where the traffic went). wal is optional.
    EnginePump(IpcChannel* inbound, IpcChannel* outbound, IEngineIngress* engine,
               MemoryPool<Order>* orders, Wal* wal = nullptr) noexcept
        : EnginePump(inbound, outbound, engine, orders, wal, Options{}) {}
    EnginePump(IpcChannel* inbound, IpcChannel* outbound, IEngineIngress* engine,
               MemoryPool<Order>* orders, Wal* wal, const Options& opts) noexcept;

    EnginePump(const EnginePump&) = delete;
    EnginePump& operator=(const EnginePump&) = delete;

    // Drain up to max_batch inbound messages; returns the count consumed
    // (including shed/quarantined frames — they were taken off the wire).
    // noexcept: the poison-pill boundary lives inside per-message dispatch.
    uint32_t run_once(uint32_t max_batch) noexcept;

    // Deterministic engine clock tick: WAL TIME_TICK (when bound) then
    // engine->on_time_tick(now_ns). Driven by EngineLoop's cadence.
    void tick(uint64_t now_ns) noexcept;

    // EngineLoop watermark policy input: when set, run_once consumes but
    // rejects OrderNew (shed_drops++) while cancels still dispatch.
    void set_shed_new_orders(bool shed) noexcept { shed_new_orders_ = shed; }
    [[nodiscard]] bool shedding() const noexcept { return shed_new_orders_; }

    // Outbound path — the channel send plus drop accounting. IpcPublisher
    // (B1) should call this (or note_outbound_drop() on its own send()==false)
    // so overload_drops / ENGINE_OVERLOAD stay authoritative here.
    bool send_outbound(const void* data, uint32_t len) noexcept;
    void note_outbound_drop() noexcept;

    // Watermark inputs for EngineLoop: inbound occupancy/capacity when the
    // transport exposes them (SharedMemChannel today); 0/0 => policy inert.
    [[nodiscard]] uint64_t inbound_occupancy() const noexcept;
    [[nodiscard]] uint64_t inbound_capacity() const noexcept;

    // Quarantine sink: invoked per poisoned frame (verify failure, decode
    // exception) with the raw bytes — wire a file logger in main.cpp
    // (/var/log/exchange/poison_pill.log per Task 2.3.19). Default: stderr.
    // A throwing sink is swallowed at the call site (noexcept pump).
    using poison_sink_fn = void (*)(void* ctx, const uint8_t* data, uint32_t len, const char* why);
    void set_poison_sink(poison_sink_fn fn, void* ctx) noexcept;

    // Alert sink for ENGINE_OVERLOAD / CAPACITY_EXCEEDED / WAL_APPEND_FAILED /
    // ORDER_BOOK_CAPACITY_EXCEEDED reports. Default: P1-style stderr line.
    using report_fn = void (*)(void* ctx, const char* code, const char* detail);
    void set_report_sink(report_fn fn, void* ctx) noexcept;

    // --- Metrics (single writer = matching thread; relaxed readers OK) ------
    [[nodiscard]] uint64_t msgs_in() const noexcept {
        return msgs_in_.load(std::memory_order_relaxed);
    }
    [[nodiscard]] uint64_t msgs_dispatched() const noexcept {
        return msgs_dispatched_.load(std::memory_order_relaxed);
    }
    [[nodiscard]] uint64_t msgs_out() const noexcept {
        return msgs_out_.load(std::memory_order_relaxed);
    }
    [[nodiscard]] uint64_t overload_drops() const noexcept {
        return overload_drops_.load(std::memory_order_relaxed);
    }
    [[nodiscard]] uint64_t overload_reports() const noexcept {
        return overload_reports_.load(std::memory_order_relaxed);
    }
    [[nodiscard]] uint64_t shed_drops() const noexcept {
        return shed_drops_.load(std::memory_order_relaxed);
    }
    [[nodiscard]] uint64_t poison_pills() const noexcept {
        return poison_pills_.load(std::memory_order_relaxed);
    }
    [[nodiscard]] uint64_t decode_errors() const noexcept {
        return decode_errors_.load(std::memory_order_relaxed);
    }
    [[nodiscard]] uint64_t order_alloc_failures() const noexcept {
        return order_alloc_failures_.load(std::memory_order_relaxed);
    }
    [[nodiscard]] uint64_t unrouted_drops() const noexcept {
        return unrouted_drops_.load(std::memory_order_relaxed);
    }
    [[nodiscard]] uint64_t poll_errors() const noexcept {
        return poll_errors_.load(std::memory_order_relaxed);
    }
    [[nodiscard]] uint64_t wal_appends() const noexcept {
        return wal_appends_.load(std::memory_order_relaxed);
    }
    [[nodiscard]] uint64_t wal_failures() const noexcept {
        return wal_failures_.load(std::memory_order_relaxed);
    }
    [[nodiscard]] uint64_t ticks() const noexcept { return ticks_.load(std::memory_order_relaxed); }
    [[nodiscard]] const LatencyHistogram& dispatch_latency() const noexcept {
        return dispatch_latency_;
    }

   private:
    void dispatch(const uint8_t* data, uint32_t len) noexcept;
    void quarantine(const uint8_t* data, uint32_t len, const char* why) noexcept;
    void report(const char* code, const char* detail) noexcept;

    IpcChannel* inbound_;
    IpcChannel* outbound_;
    IEngineIngress* engine_;
    MemoryPool<Order>* orders_;
    Wal* wal_;
    Options opts_;
    SharedMemChannel* shm_in_;  // cached cast for zero-copy poll_view/consume

    bool shed_new_orders_ = false;
    uint64_t last_overload_report_ = 0;
    uint64_t last_shed_report_ = 0;

    poison_sink_fn poison_sink_ = nullptr;
    void* poison_ctx_ = nullptr;
    report_fn report_sink_ = nullptr;
    void* report_ctx_ = nullptr;

    std::atomic<uint64_t> msgs_in_{0};
    std::atomic<uint64_t> msgs_dispatched_{0};
    std::atomic<uint64_t> msgs_out_{0};
    std::atomic<uint64_t> overload_drops_{0};
    std::atomic<uint64_t> overload_reports_{0};
    std::atomic<uint64_t> shed_drops_{0};
    std::atomic<uint64_t> poison_pills_{0};
    std::atomic<uint64_t> decode_errors_{0};
    std::atomic<uint64_t> order_alloc_failures_{0};
    std::atomic<uint64_t> unrouted_drops_{0};
    std::atomic<uint64_t> poll_errors_{0};
    std::atomic<uint64_t> wal_appends_{0};
    std::atomic<uint64_t> wal_failures_{0};
    std::atomic<uint64_t> ticks_{0};
    LatencyHistogram dispatch_latency_;

    alignas(64) uint8_t inbuf_[kMaxInboundBytes];
};

}  // namespace exch
