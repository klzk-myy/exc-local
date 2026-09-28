#pragma once

// PHASE-02 TASK-2.3.25 — OptimisticShardCoordinator: optimistic cross-shard
// reservation with asynchronous compensating unwinds (spec §2.2a, §24 #404;
// remediation #37).
//
// SUPERSESSION: this is the CANONICAL cross-shard routing path for the
// matching hot path — it supersedes the blocking 5-second 2PC balance locks
// of Task 2.3.8. CrossShardCoordinator remains the reservation/bookkeeping
// substrate of record; this coordinator holds ZERO resting multi-second
// locks on participating shards — a TRY_MATCH either fills inside the
// 500µs window or the leg is liquidated, so no lock ever survives an op.
//
// Protocol:
//   coordinator -> participant : TRY_MATCH      (parallel, non-blocking)
//   participant -> coordinator : TryAck {filled_qty, vwap}   (fill report;
//                                filled_qty < requested = partial = FAILURE)
//                                TryNack {reason}
//   coordinator -> participant : COMPENSATE_UNWIND (synthetic market order
//                                liquidating the leg's filled_qty)
//   participant -> coordinator : UnwindAck {unwound_qty, vwap}
//
// Deadlines (engine-logical time, driven by on_time_tick — no clock reads):
//   * match_deadline   = submit + 500µs — a leg unanswered by then is failed
//   * unwind_deadline  = unwind start + 1s — unconfirmed unwinds escalate the
//                        op to Failed and count orphan_legs_total (loud,
//                        never silent)
//
// Atomicity model: participants execute TRY_MATCH IMMEDIATELY through the
// engine-installed OptMatchExec callback — no cross-shard mutex, no balance
// lock. All legs fill -> Committed (fills are already durable — nothing to
// commit). Any leg Rejected/partial/timed-out -> Unwinding: every leg with
// fills gets COMPENSATE_UNWIND; the participant's OptUnwindExec runs the
// synthetic market order and reports the unwind VWAP. Slippage vs the
// original fill aggregates into a single posting to GL account
// `5010_CROSS_SHARD_EXECUTION_DIFF` (emitted via CrossShardGlPosting through
// the sink — the GL posting proper is Phase-03 Task 3.3.6 double-entry;
// this component produces the signed record).
//
// Threading / storage / codec: identical contract to CrossShardCoordinator —
// single-writer on the matching-loop thread, fixed-capacity open-addressed
// tables, packed little-endian control structs on the shared
// CrossShardCtlHeader layout (own magic so one Aeron stream pair can carry
// both protocols).
//
// WAL: reserves event-type values 12..15 (integrator appends
// `OPT_BEGIN = 12, OPT_FILL = 13, OPT_UNWIND = 14, OPT_OUTCOME = 15` to
// WalEventType in WalEntry.hpp — owned elsewhere, do not edit here).
// Participant fill records are WAL'd so a restart can unwind orphans
// (recover() + first-tick sweep = RecoveryUnwind reason).

#include <cstddef>
#include <cstdint>
#include <cstring>

#include "ipc/IpcChannel.hpp"
#include "matching/CrossShardCoordinator.hpp"  // CrossShardCtlHeader, BasketOpId
#include "wal/Wal.hpp"
#include "wal/WalEntry.hpp"

namespace exch {

// --- Optimistic wire codec -----------------------------------------------------

inline constexpr uint32_t kOptCtlMagic = 0x4F484F58u;    // "XOHO" LE
inline constexpr uint16_t kOptCtlVersion = 1u;

enum class OptCtlType : uint8_t {
    TryMatch   = 1,  // coordinator -> participant
    TryAck     = 2,  // participant -> coordinator (fill report)
    TryNack    = 3,  // participant -> coordinator
    Unwind     = 4,  // coordinator -> participant (COMPENSATE_UNWIND)
    UnwindAck  = 5,  // participant -> coordinator
};

enum class OptNackReason : uint32_t {
    Rejected = 1,        // executor refused (risk/liquidity/mode)
    UnknownAccount = 2,
    Overload = 3,
    ShardUnreachable = 4,  // synthesized locally on send failure
    AlreadyDone = 5,       // dedup tombstone — deterministic re-NACK
};

enum class OptUnwindReason : uint8_t {
    PeerRejected = 1,    // sibling leg NACK'd
    PeerTimeout  = 2,    // sibling leg missed the 500µs window
    PartialFill  = 3,    // sibling leg filled short of requested qty
    Cancelled    = 4,
    RecoveryUnwind = 5,  // WAL-recovered fill liquidated post-restart
};

#pragma pack(push, 1)
struct OptTryMatchBody {
    uint64_t op_hi;
    uint64_t op_lo;
    uint64_t account_id;        // executing account on the hosting shard
    uint64_t order_id;          // leg order id (0 = synthetic-only)
    int64_t  qty_units;         // requested fill, 1e8 units
    int64_t  limit_price_ticks; // protection price; 0 = pure market
    uint32_t leg_index;
    uint32_t instrument_id;
    uint8_t  side;              // 0 = Buy, 1 = Sell (book/Order.hpp ordinals)
    uint8_t  _pad[7];
};

struct OptTryAckBody {
    uint64_t op_hi;
    uint64_t op_lo;
    uint32_t leg_index;
    uint8_t  _pad[4];
    int64_t  filled_qty;    // < requested = partial = treated as failed leg
    int64_t  vwap_ticks;    // volume-weighted avg fill price, 1e8 ticks
};

struct OptTryNackBody {
    uint64_t op_hi;
    uint64_t op_lo;
    uint32_t leg_index;
    uint32_t reason;        // OptNackReason
};

struct OptUnwindBody {      // COMPENSATE_UNWIND — synthetic market order
    uint64_t op_hi;
    uint64_t op_lo;
    uint32_t leg_index;
    uint8_t  reason;        // OptUnwindReason
    uint8_t  _pad[3];
    int64_t  qty_units;     // quantity to liquidate (filled_qty)
};

struct OptUnwindAckBody {
    uint64_t op_hi;
    uint64_t op_lo;
    uint32_t leg_index;
    uint8_t  _pad[4];
    int64_t  unwound_qty;
    int64_t  vwap_ticks;    // unwind execution VWAP
};
#pragma pack(pop)

static_assert(sizeof(OptTryMatchBody) == 64);
static_assert(sizeof(OptTryAckBody) == 40);
static_assert(sizeof(OptTryNackBody) == 24);
static_assert(sizeof(OptUnwindBody) == 32);
static_assert(sizeof(OptUnwindAckBody) == 40);

inline constexpr uint32_t kOptCtlMaxFrame =
    sizeof(CrossShardCtlHeader) + sizeof(OptTryMatchBody);  // 16 + 64 = 80

struct OptCtlView {
    OptCtlType type;
    uint32_t src_shard;
    uint32_t dst_shard;
    union {
        OptTryMatchBody try_;
        OptTryAckBody ack;
        OptTryNackBody nack;
        OptUnwindBody unwind;
        OptUnwindAckBody uack;
    };
};

enum class OptCtlDecode : uint8_t {
    Ok = 0,
    TooShort,
    BadMagic,
    BadVersion,
    UnknownType,
};

[[nodiscard]] const char* opt_ctl_decode_str(OptCtlDecode r) noexcept;
[[nodiscard]] uint32_t opt_ctl_encode(uint8_t* dst, uint32_t cap,
                                      OptCtlType type, const void* body,
                                      uint32_t src_shard,
                                      uint32_t dst_shard) noexcept;
[[nodiscard]] OptCtlDecode opt_ctl_decode(const void* buf, uint32_t len,
                                          OptCtlView* out) noexcept;

// --- WAL payloads (reserved WalEventType values 12..15 — see header note) ----

inline constexpr uint8_t kWalEvtOptBegin   = 12;  // coordinator op intent
inline constexpr uint8_t kWalEvtOptFill    = 13;  // participant-side fill
inline constexpr uint8_t kWalEvtOptUnwind  = 14;  // participant-side unwind
inline constexpr uint8_t kWalEvtOptOutcome = 15;  // coordinator terminal +
                                                  // GL-posting summary

#pragma pack(push, 1)
struct WalOptBeginPayload {
    uint64_t op_hi;
    uint64_t op_lo;
    uint64_t account_id;
    uint32_t leg_count;
    uint32_t coordinator_shard;
    uint64_t match_deadline_ns;
};

// Participant-durable fill — the ONLY record that may restore an unwoundable
// position on replay (zero-orphan invariant across restart).
struct WalOptFillPayload {
    uint64_t op_hi;
    uint64_t op_lo;
    uint64_t coordinator_shard;
    uint64_t account_id;
    uint64_t order_id;
    int64_t  filled_qty;
    int64_t  vwap_ticks;
    uint32_t leg_index;
    uint32_t instrument_id;
    uint8_t  side;
    uint8_t  _pad[7];
};

struct WalOptUnwindPayload {
    uint64_t op_hi;
    uint64_t op_lo;
    uint32_t leg_index;
    uint8_t  reason;        // OptUnwindReason
    uint8_t  _pad[3];
    int64_t  unwound_qty;
    int64_t  vwap_ticks;
};

struct WalOptOutcomePayload {
    uint64_t op_hi;
    uint64_t op_lo;
    uint64_t account_id;
    int64_t  slippage_ticks;  // signed total unwind slippage (>0 = loss)
    uint64_t duration_ns;
    uint8_t  status;          // OptStatus terminal
    uint8_t  code;            // OptCode
    uint8_t  _pad[6];
};
#pragma pack(pop)

static_assert(sizeof(WalOptBeginPayload) == 40);
static_assert(sizeof(WalOptFillPayload) == 72);
static_assert(sizeof(WalOptUnwindPayload) == 40);
static_assert(sizeof(WalOptOutcomePayload) == 48);

// --- public types ----------------------------------------------------------------

// GL account of record for unwind slippage (spec §2.2a / Task 2.3.25 #4).
inline constexpr char kGlCrossShardExecDiff[] =
    "5010_CROSS_SHARD_EXECUTION_DIFF";

struct OptLegSpec {
    uint32_t shard_id;           // hosting shard (min = coordinator)
    uint32_t instrument_id;
    uint64_t account_id;
    uint64_t order_id;
    int64_t  qty_units;
    int64_t  limit_price_ticks;  // 0 = pure market order
    uint8_t  side;               // 0 = Buy, 1 = Sell
};

enum class OptStatus : uint8_t {
    Unknown = 0,
    Matching,       // TRY_MATCH in-flight (<= 500µs)
    Committed,      // terminal — all legs filled
    Unwinding,      // COMPENSATE_UNWIND in-flight
    Compensated,    // terminal — every fill unwound, GL posting emitted
    Failed,         // terminal — unwind unconfirmed past deadline (orphans)
    Rejected,       // terminal — never entered the protocol
};

enum class OptCode : uint8_t {
    Ok = 0,
    BadArgs,
    NotCoordinator,
    LegRejected,
    LegTimeout,      // -> CROSS_SHARD_TIMEOUT
    ChannelDown,
    CapacityExceeded,
    WalFailure,
    Cancelled,
    RecoveryInterrupted,
};

struct OptResult {
    BasketOpId op_id;
    int64_t  slippage_ticks;   // terminal ops: total unwind slippage
    uint8_t  status;           // OptStatus
    uint8_t  code;             // OptCode
    uint8_t  leg_count;
    uint8_t  legs_filled;
    uint8_t  legs_unwound;
    uint8_t  _pad[3];
};

// Participant-side executors (installed by the engine; tests script them).
// match_exec: execute the leg immediately; return filled qty (<= 0 = reject)
// and set *vwap_ticks. unwind_exec: run the COMPENSATE_UNWIND synthetic
// market order for qty_units; return unwound qty and set *vwap_ticks.
using OptMatchExec = int64_t (*)(void* ctx, const OptTryMatchBody& leg,
                                 int64_t* vwap_ticks) noexcept;
using OptUnwindExec = int64_t (*)(void* ctx, BasketOpId op_id,
                                  uint32_t leg_index, int64_t qty_units,
                                  int64_t* vwap_ticks) noexcept;

// GL posting record — the 5010_CROSS_SHARD_EXECUTION_DIFF journal entry of
// record (actual double-entry posting lands in Phase-03 Task 3.3.6).
struct CrossShardGlPosting {
    BasketOpId op_id;
    char account[40];        // kGlCrossShardExecDiff
    int64_t amount_ticks;    // signed; > 0 = unwind loss borne by the house
    uint64_t ts_ns;
    uint8_t reason;          // dominant OptUnwindReason for the op
    uint8_t _pad[7];
};
using OptGlSink = void (*)(void* ctx,
                           const CrossShardGlPosting& p) noexcept;

struct OptimisticMetrics {
    uint64_t try_total = 0;
    uint64_t committed_total = 0;
    uint64_t compensated_total = 0;
    uint64_t failed_total = 0;
    uint64_t timeout_total = 0;         // legs missing the 500µs window
    uint64_t nack_total = 0;
    uint64_t unwind_orders_total = 0;   // COMPENSATE_UNWIND commands issued
    uint64_t unwind_acks_total = 0;
    int64_t  unwind_slippage_ticks_sum = 0;
    uint64_t orphan_legs_total = 0;     // unwound-unconfirmed at deadline
    uint64_t ops_active = 0;            // gauge
    uint64_t wal_failures = 0;
    uint64_t bad_frames = 0;
};
using OptimisticMetricsSink =
    void (*)(void* ctx, const OptimisticMetrics& m) noexcept;

struct OptimisticCoordinatorOptions {
    uint64_t match_budget_ns = 500'000;        // 500µs TRY_MATCH window
    uint64_t unwind_deadline_ns = 1'000'000'000;  // 1s unwind budget
    uint16_t shard_id = 0;
};

class OptimisticShardCoordinator {
public:
    static constexpr uint32_t kMaxOps = 1024;
    static constexpr uint32_t kMaxPartFills = 2048;
    static constexpr uint32_t kMaxLegs = 8;

    // ctl may be nullptr (standalone — submits fail closed); wal optional.
    OptimisticShardCoordinator(IpcChannel* ctl, Wal* wal,
                               OptimisticCoordinatorOptions opts) noexcept;

    OptimisticShardCoordinator(const OptimisticShardCoordinator&) = delete;
    OptimisticShardCoordinator& operator=(const OptimisticShardCoordinator&) =
        delete;

    // Coordinator election: lowest participant shard_id (same rule as 2PC).
    [[nodiscard]] static uint32_t coordinator_shard(const OptLegSpec* legs,
                                                    uint32_t n) noexcept;

    // Dedup semantics identical to 2PC: a known op_id returns its cached
    // OptResult with zero side-effects. Gate rejections are not tombstoned.
    [[nodiscard]] OptResult submit(const OptLegSpec* legs, uint32_t leg_count,
                                   uint64_t account, BasketOpId op_id,
                                   uint64_t now_ns) noexcept;
    [[nodiscard]] OptResult status(BasketOpId op_id) const noexcept;
    bool cancel(BasketOpId op_id, uint64_t now_ns) noexcept;

    // Pump: drains inbound control frames (bounded), then enforces the 500µs
    // match window + unwind deadline across live ops (O(live)).
    void on_time_tick(uint64_t now_ns) noexcept;

    void set_match_exec(OptMatchExec fn, void* ctx) noexcept;
    void set_unwind_exec(OptUnwindExec fn, void* ctx) noexcept;
    void set_gl_sink(OptGlSink fn, void* ctx) noexcept;
    void set_metrics_sink(OptimisticMetricsSink fn, void* ctx) noexcept;

    // Replays OPT_* entries; fills restored live are liquidated by the
    // unwind executor on the first tick after recover (RecoveryUnwind).
    size_t recover(WalReader& reader) noexcept;

    [[nodiscard]] const OptimisticMetrics& metrics() const noexcept {
        return metrics_;
    }
    [[nodiscard]] uint16_t shard_id() const noexcept { return opts_.shard_id; }
    [[nodiscard]] uint32_t live_ops() const noexcept { return live_ops_; }
    // Participant-side open (filled, not yet unwound) quantity for an
    // (op,leg) — the zero-orphan observability hook.
    [[nodiscard]] int64_t open_fill_qty(BasketOpId op_id,
                                        uint32_t leg_index) const noexcept;

private:
    enum class LegSt : uint8_t {
        Pending = 0,
        Filled,
        Rejected,
        UnwindSent,
        Unwound,
    };

    enum class FillSt : uint8_t {
        Empty = 0,
        Filled,     // participant holds an unwoundable fill
        Unwound,    // terminal — flattened
        Denied,     // tombstone
    };

    struct LegRec {
        uint64_t account_id;
        uint64_t order_id;
        int64_t  qty_units;
        int64_t  limit_price_ticks;
        int64_t  filled_qty;
        int64_t  fill_vwap;
        uint32_t shard_id;
        uint32_t instrument_id;
        uint8_t  side;
        uint8_t  state;   // LegSt
        uint8_t  local;
        uint8_t  _pad[5];
    };

    struct OpRec {
        BasketOpId id;
        uint64_t account;
        uint64_t t_submit_ns;
        uint64_t match_deadline_ns;
        uint64_t unwind_deadline_ns;
        int64_t  slippage_ticks;
        uint8_t  state;       // OptStatus
        uint8_t  code;
        uint8_t  leg_count;
        uint8_t  occupied;
        uint8_t  recovered;
        uint8_t  unwind_reason;  // dominant OptUnwindReason
        uint8_t  _pad[2];
        LegRec legs[kMaxLegs];
    };

    // Participant-side fill record — what an inbound UNWIND liquidates.
    struct FillRec {
        BasketOpId op_id;
        uint64_t coordinator_shard;
        uint64_t account_id;
        uint64_t order_id;
        int64_t  filled_qty;
        int64_t  vwap_ticks;
        uint32_t leg_index;
        uint32_t instrument_id;
        uint8_t  side;
        uint8_t  state;      // FillSt
        uint8_t  recovered;
        uint8_t  _pad[5];
    };

    static uint32_t hash_id(BasketOpId id) noexcept;
    OpRec* find_op(BasketOpId id) noexcept;
    const OpRec* find_op(BasketOpId id) const noexcept;
    OpRec* insert_op(BasketOpId id) noexcept;
    FillRec* find_fill(BasketOpId id, uint32_t leg_index) noexcept;
    const FillRec* find_fill(BasketOpId id, uint32_t leg_index) const noexcept;
    FillRec* insert_fill(BasketOpId id, uint32_t leg_index) noexcept;

    // Coordinator transitions.
    void maybe_resolve(OpRec& op, uint64_t now_ns) noexcept;
    void begin_unwind(OpRec& op, OptCode code, OptUnwindReason reason,
                      uint64_t now_ns) noexcept;
    void finalize_op(OpRec& op, OptStatus st, OptCode code,
                     uint64_t now_ns) noexcept;
    OptResult snapshot(const OpRec& op) const noexcept;

    // Participant transitions (executor-driven; local legs share them).
    FillRec* local_try_match(const OptTryMatchBody& m, uint64_t coord_shard,
                             bool emit_wire_ack) noexcept;
    void local_unwind(FillRec& r, OptUnwindReason reason,
                      bool send_wire_ack) noexcept;

    bool send_frame(OptCtlType t, const void* body, uint32_t dst) noexcept;
    void on_frame(const uint8_t* buf, uint32_t len, uint64_t now_ns) noexcept;
    void on_try_match(const OptCtlView& v, uint64_t now_ns) noexcept;
    void on_try_ack(const OptCtlView& v, uint64_t now_ns) noexcept;
    void on_try_nack(const OptCtlView& v, uint64_t now_ns) noexcept;
    void on_unwind(const OptCtlView& v, uint64_t now_ns) noexcept;
    void on_unwind_ack(const OptCtlView& v, uint64_t now_ns) noexcept;

    // Per-leg unwind slippage, signed in 1e8-scaled quote ticks:
    // BUY leg unwound by a SELL (and vice versa) loses (fill - unwind)*qty.
    // 128-bit intermediate; narrowing failure clamps to INT64_MAX (fail-loud
    // positive loss, never a wrap).
    [[nodiscard]] static int64_t leg_slippage(const LegRec& l,
                                              int64_t unwind_qty,
                                              int64_t unwind_vwap) noexcept;

    void emit_gl_posting(const OpRec& op, uint64_t now_ns) noexcept;
    void emit_metrics() noexcept;

    bool wal_begin(const OpRec& op) noexcept;
    bool wal_fill(const FillRec& r) noexcept;
    bool wal_unwind(const FillRec& r, OptUnwindReason reason,
                    int64_t unwound_qty, int64_t vwap) noexcept;
    bool wal_outcome(const OpRec& op) noexcept;

    IpcChannel* ctl_;
    Wal* wal_;
    OptimisticCoordinatorOptions opts_;

    // Zero-init required — Empty/occupied==0 are the probe terminators.
    OpRec ops_[kMaxOps]{};
    FillRec fills_[kMaxPartFills]{};

    uint64_t id_seq_ = 0;
    uint32_t live_ops_ = 0;

    OptMatchExec match_exec_ = nullptr;
    void* match_ctx_ = nullptr;
    OptUnwindExec unwind_exec_ = nullptr;
    void* unwind_ctx_ = nullptr;
    OptGlSink gl_sink_ = nullptr;
    void* gl_ctx_ = nullptr;
    OptimisticMetricsSink met_sink_ = nullptr;
    void* met_ctx_ = nullptr;

    OptimisticMetrics metrics_{};
};

}  // namespace exch
