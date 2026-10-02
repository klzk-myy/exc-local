#pragma once

// PHASE-02 TASK-2.3.8 + TASK-2.3.14 — CrossShardCoordinator: two-phase commit
// (2PC) for cross-shard basket orders (spec §2.2a, §13.1, §24 #214; §23 codes
// CROSS_SHARD_TIMEOUT / CROSS_SHARD_LIMIT_EXCEEDED).
//
// Shards partition by currency pair (spec §2.2), so a multi-pair basket order
// must reserve balances and activate its legs atomically across engines. The
// participant with the LOWEST shard_id is the coordinator (Task 2.3.8 rule);
// this component implements both roles on every engine:
//
//   coordinator -> participant : ReserveReq   (Phase 1: lock balance, leg
//                                               order logically RESERVED,
//                                               5s TTL — supersedes 30s,
//                                               §24 #214)
//   participant -> coordinator : ReserveAck / ReserveNack
//   coordinator -> participant : Commit       (Phase 2: activate within the
//                                               5s window)
//   participant -> coordinator : CommitAck    (applied flag — a leg whose
//                                               reservation expired mid-
//                                               commit answers applied=0)
//   coordinator -> participant : Release      (compensate/abort — unlock, or
//                                               cancel+restitute a committed
//                                               leg)
//   participant -> coordinator : Release      (Expiry notice — the TTL lapsed
//                                               un-committed; coordinator
//                                               compensates the remaining legs)
//   participant -> coordinator : ReleaseAck   (compensation bookkeeping;
//                                               zero-orphan safety comes from
//                                               the participant-side TTL,
//                                               never from ack delivery)
//
// Layered deadlines (Task 2.3.8 implementation notes + remediation #35):
//   * reserve_ttl_ns    5s  — Phase-1 budget AND participant lock TTL.
//   * total_deadline_ns 8s  — 5s reserve + 3s commit/compensate (the plan's
//                             prior "10s" prose was self-contradictory).
//   * reaper_interval   2s  — CompensationReaper cadence in ENGINE-LOGICAL
//                             time. Spec text says "background goroutine scans
//                             pending_reservations every 2s" — that Go-side
//                             goroutine is a later-phase gateway concern; in
//                             this C++ component the reaper is a tick-driven
//                             scan inside on_time_tick() (same deterministic-
//                             clock discipline as ExpiryScheduler). Effective
//                             worst-case seizure is therefore TTL + one
//                             reaper interval — spec-conformant.
//
// Task 2.3.14 constraints:
//   * Shard non-response within 5s -> coordinator cancels the reservation and
//     compensates ALL participant shards.
//   * Max 10 concurrent in-flight cross-shard ops per account; excess rejected
//     CROSS_SHARD_LIMIT_EXCEEDED (HTTP 429).
//   * Metrics hooks: cross_shard_reservation_timeout_total and
//     cross_shard_reservation_active are exposed in CrossShardMetrics (the
//     POD of record) plus a snapshot sink; Prometheus wiring is Phase-07
//     Task 7.3.8 (same convention as CrossShardMarginCoordinator counters).
//
// SUPERSESSION NOTE (spec §2.2a / §24 #404, remediation #37): the pessimistic
// blocking-2PC path implemented here is the RESERVATION/BOOKKEEPING substrate
// of record; OptimisticShardCoordinator (Task 2.3.25, matching/
// OptimisticShardCoordinator.hpp) supersedes blocking locks as the canonical
// routing path for the matching hot path. 2PC machinery remains for
// reservation bookkeeping, gateway-mediated flows, and its own DoD surface.
//
// Threading: single-writer contract identical to Wal /
// CrossShardMarginCoordinator — submit()/cancel()/on_time_tick()/recover()
// run on the matching-loop thread only. Fixed-capacity open-addressed tables
// inside the object: zero allocation, noexcept on every mutating entry point,
// fail-closed on capacity/WAL/channel faults (spec §2.7 pessimism).
//
// Determinism: NO clock is ever read inside. All deadlines/TTL/reaper cadence
// derive from the caller-supplied engine-logical now_ns (the WAL TIME_TICK
// stamp — same discipline as ExpiryScheduler::on_time_tick).
//
// Codec: fixed-size packed little-endian structs, NOT FlatBuffers — same
// precedent as the margin-ctl seam (largest frame 57 bytes, encoding/binary
// mirrors layout on the Go side). A shared CrossShardCtlHeader carries
// src/dst shard routing fields; OptimisticShardCoordinator reuses the header
// layout with its own magic so one Aeron stream pair can carry both
// protocols.
//
// WAL: WalEntry.hpp owns the on-disk contract. This header defines the
// payload structs; the BASKET_* enumerators were appended at the
// WalEventType tail (values 14–17 — the original 8–11 placeholders were
// consumed by later engine events before wiring landed; the kWalEvtBasket*
// aliases below bind the real values).

#include <cstddef>
#include <cstdint>
#include <cstring>

#include "ipc/IpcChannel.hpp"
#include "wal/Wal.hpp"
#include "wal/WalEntry.hpp"

namespace exch {

// §23 error literals (code of record until Phase-05 Task 5.3.21 registry —
// same convention as risk/CrossShardMarginCoordinator.h).
inline constexpr char kCodeCrossShardTimeout[] =
    "CROSS_SHARD_TIMEOUT";          // HTTP 504 — reserve/commit deadline missed
inline constexpr char kCodeCrossShardLimitExceeded[] =
    "CROSS_SHARD_LIMIT_EXCEEDED";   // HTTP 429 — >10 concurrent ops/account

// --- shared control header -------------------------------------------------
// Routing fields live in the header (not per-body) so a relay can forward
// frames without decoding the body. Production wires Aeron per-shard streams
// on `aeron:ipc?alias=xshard_ctl_*`; tests route on dst_shard directly.
// OptimisticShardCoordinator.hpp reuses this header with its own magic.

#pragma pack(push, 1)
struct CrossShardCtlHeader {
    uint32_t magic;     // protocol discriminator (kBasketCtlMagic / kOptCtlMagic)
    uint8_t  type;      // per-protocol type enum
    uint8_t  flags;     // reserved, must be 0
    uint16_t version;   // per-protocol version
    uint32_t src_shard;
    uint32_t dst_shard;
};
#pragma pack(pop)
static_assert(sizeof(CrossShardCtlHeader) == 16);
// Fixed offset of dst_shard inside every cross-shard control frame — the
// relay/test-mesh reads this without knowing the body type.
inline constexpr uint32_t kCrossShardCtlDstOffset = 12;

// --- Basket 2PC wire codec ---------------------------------------------------

inline constexpr uint32_t kBasketCtlMagic = 0x32424858u;   // "XHB2" LE
inline constexpr uint16_t kBasketCtlVersion = 1u;

enum class BasketCtlType : uint8_t {
    ReserveReq  = 1,   // coordinator -> participant (Phase 1)
    ReserveAck  = 2,   // participant -> coordinator
    ReserveNack = 3,   // participant -> coordinator
    Commit      = 4,   // coordinator -> participant (Phase 2)
    CommitAck   = 5,   // participant -> coordinator (applied flag)
    Release     = 6,   // both directions (compensate / expiry notice)
    ReleaseAck  = 7,   // participant -> coordinator
};

enum class BasketNackReason : uint32_t {
    InsufficientBalance = 1,
    UnknownAccount      = 2,
    Overload            = 3,
    ShardUnreachable    = 4,   // synthesized locally on send failure
    TtlExpired          = 5,   // reservation arrived already-expired
};

enum class BasketReleaseReason : uint32_t {
    Compensate         = 1,  // coordinator rollback (peer NACK / timeout)
    Expired            = 2,  // participant TTL expiry notice (coord-bound)
    CoordinatorCancel  = 3,  // explicit cancel_basket()
    RecoveryOrphan     = 4,  // WAL-recovered lock swept post-restart
    CommitNotApplied   = 5,  // commit lost the TTL race -> release for retry
};

#pragma pack(push, 1)
struct BasketReserveReqBody {
    uint64_t op_hi;            // 128-bit operation_id (hi/lo)
    uint64_t op_lo;
    uint32_t leg_index;        // leg ordinal inside the basket
    uint32_t instrument_id;
    uint64_t account_id;       // balance account ON the hosting shard
    uint64_t order_id;         // leg order (0 = bookkeeping-only leg)
    int64_t  amount;           // balance to lock, 1e8-scaled ticks
    uint64_t expires_at_ns;    // authoritative TTL assigned by coordinator
};

struct BasketVoteBody {        // shared ReserveAck / ReserveNack shape
    uint64_t op_hi;
    uint64_t op_lo;
    uint32_t leg_index;
    uint32_t reason;           // 0 = granted; else BasketNackReason
    int64_t  amount;           // reserved amount (echo)
    uint64_t expires_at_ns;    // granted TTL (echo)
};

struct BasketCommitBody {
    uint64_t op_hi;
    uint64_t op_lo;
    uint32_t leg_index;
    uint32_t _pad;
};

struct BasketCommitAckBody {
    uint64_t op_hi;
    uint64_t op_lo;
    uint32_t leg_index;
    uint8_t  applied;          // 1 = leg activated; 0 = reservation gone
    uint8_t  _pad[3];
};

struct BasketReleaseBody {
    uint64_t op_hi;
    uint64_t op_lo;
    uint32_t leg_index;
    uint32_t reason;           // BasketReleaseReason
};

struct BasketReleaseAckBody {
    uint64_t op_hi;
    uint64_t op_lo;
    uint32_t leg_index;
    uint32_t _pad;
};
#pragma pack(pop)

static_assert(sizeof(BasketReserveReqBody) == 56);
static_assert(sizeof(BasketVoteBody) == 40);
static_assert(sizeof(BasketCommitBody) == 24);
static_assert(sizeof(BasketCommitAckBody) == 24);
static_assert(sizeof(BasketReleaseBody) == 24);
static_assert(sizeof(BasketReleaseAckBody) == 24);

inline constexpr uint32_t kBasketCtlMaxFrame =
    sizeof(CrossShardCtlHeader) + sizeof(BasketReserveReqBody);  // 16 + 56 = 72

struct BasketCtlView {
    BasketCtlType type;
    uint32_t src_shard;
    uint32_t dst_shard;
    union {
        BasketReserveReqBody req;
        BasketVoteBody vote;
        BasketCommitBody commit;
        BasketCommitAckBody cack;
        BasketReleaseBody rel;
        BasketReleaseAckBody rack;
    };
};

enum class BasketCtlDecode : uint8_t {
    Ok = 0,
    TooShort,
    BadMagic,
    BadVersion,
    UnknownType,
};

[[nodiscard]] const char* basket_ctl_decode_str(BasketCtlDecode r) noexcept;

// Serializes header+body into dst (cap >= kBasketCtlMaxFrame for any type).
// Returns bytes written, 0 on bad args (dst null / cap short / type unknown).
[[nodiscard]] uint32_t basket_ctl_encode(uint8_t* dst, uint32_t cap,
                                         BasketCtlType type, const void* body,
                                         uint32_t src_shard,
                                         uint32_t dst_shard) noexcept;

// Validates + copies a frame into *out. Untrusted bytes are never aliased.
[[nodiscard]] BasketCtlDecode basket_ctl_decode(const void* buf, uint32_t len,
                                                BasketCtlView* out) noexcept;

// --- WAL payloads ------------------------------------------------------------
// The original "reserved values 8..11" note predated PREVENTED_MATCH /
// OCO_LINK / AUCTION_PHASE / ORDER_NEW_EX landing in those slots — the
// on-disk append order won. The real values are appended at the
// WalEventType tail (14..17); these aliases keep call sites symbolic.
// (IMP-PLAN Phase-3 Task 4 — the constants were never on disk unwired.)
inline constexpr uint8_t kWalEvtBasketBegin =
    static_cast<uint8_t>(WalEventType::BASKET_BEGIN);      // coordinator op intent
inline constexpr uint8_t kWalEvtBasketReserve =
    static_cast<uint8_t>(WalEventType::BASKET_RESERVE);    // committed leg lock
inline constexpr uint8_t kWalEvtBasketLegDone =
    static_cast<uint8_t>(WalEventType::BASKET_LEG_DONE);   // leg commit/release
inline constexpr uint8_t kWalEvtBasketOutcome =
    static_cast<uint8_t>(WalEventType::BASKET_OUTCOME);    // terminal op result

#pragma pack(push, 1)
// Logged when a basket is admitted to the protocol — uncommitted intent:
// replay creates a tombstone so a redelivered op_id dedups fail-closed and
// the initiator's concurrency slot is accounted for.
struct WalBasketBeginPayload {
    uint64_t op_hi;
    uint64_t op_lo;
    uint64_t initiator_account;
    uint32_t leg_count;
    uint32_t coordinator_shard;
    uint64_t reserve_deadline_ns;
    uint64_t total_deadline_ns;
};

// Logged when a participant (or the coordinator's own locally-hosted leg)
// durably locks balance — the only record that may restore a live lock.
struct WalBasketReservePayload {
    uint64_t op_hi;
    uint64_t op_lo;
    uint64_t coordinator_shard;
    uint64_t account_id;
    uint64_t order_id;
    int64_t  amount;
    uint64_t expires_at_ns;
    uint32_t leg_index;
    uint32_t instrument_id;
};

// Logged when a leg terminates: kind 0 = commit applied, 1 = released.
struct WalBasketLegDonePayload {
    uint64_t op_hi;
    uint64_t op_lo;
    uint32_t leg_index;
    uint8_t  kind;     // 0 commit / 1 release
    uint8_t  reason;   // BasketReleaseReason when kind=1
    uint8_t  _pad[2];
};

// Logged once per op at a terminal coordinator state (Committed /
// Compensated / Failed) — replay caches the result for op_id dedup.
struct WalBasketOutcomePayload {
    uint64_t op_hi;
    uint64_t op_lo;
    uint64_t initiator_account;
    uint64_t duration_ns;
    uint8_t  status;   // BasketStatus (terminal)
    uint8_t  code;     // BasketCode
    uint8_t  _pad[6];
};
#pragma pack(pop)

static_assert(sizeof(WalBasketBeginPayload) == 48);
static_assert(sizeof(WalBasketReservePayload) == 64);
static_assert(sizeof(WalBasketLegDonePayload) == 24);
static_assert(sizeof(WalBasketOutcomePayload) == 40);

// --- public types --------------------------------------------------------------

// 128-bit operation identity (UUID v4 bytes or caller-supplied). {0,0} is
// the nil id — never a valid basket.
struct BasketOpId {
    uint64_t hi;
    uint64_t lo;
};
[[nodiscard]] inline bool operator==(const BasketOpId& a,
                                     const BasketOpId& b) noexcept {
    return a.hi == b.hi && a.lo == b.lo;
}
[[nodiscard]] inline bool operator!=(const BasketOpId& a,
                                     const BasketOpId& b) noexcept {
    return !(a == b);
}
[[nodiscard]] inline bool basket_op_id_is_nil(const BasketOpId& id) noexcept {
    return id.hi == 0 && id.lo == 0;
}

// One basket leg as admitted by the coordinator.
struct BasketLegSpec {
    uint32_t shard_id;       // hosting shard (decides wire vs local path)
    uint32_t instrument_id;
    uint64_t order_id;       // leg order id (0 = bookkeeping-only leg)
    uint64_t account_id;     // balance account on the hosting shard
    int64_t  amount;         // balance to lock for this leg (1e8 ticks)
};

enum class BasketStatus : uint8_t {
    Unknown = 0,    // no record
    Reserving,      // Phase 1 in-flight
    Committing,     // Phase 2 in-flight
    Committed,      // terminal — all legs activated
    Compensating,   // rollback in-flight
    Compensated,    // terminal — every leg released (zero held state)
    Failed,         // terminal — rollback attempted; resolution uncertain
                    // (a committed leg never confirmed its release)
    Rejected,       // terminal — never entered the protocol (gate rejections
                    // are NOT tombstoned: a retry must re-evaluate the gate)
};

enum class BasketCode : uint8_t {
    Ok = 0,
    BadArgs,
    NotCoordinator,        // submitted on a shard that is not min(participants)
    LimitExceeded,         // -> CROSS_SHARD_LIMIT_EXCEEDED
    ParticipantNack,       // a leg refused/dropped the reservation
    CommitRefused,         // commit lost the TTL race (CommitAck applied=0)
    ReserveTimeout,        // -> CROSS_SHARD_TIMEOUT (Phase-1 deadline)
    CommitTimeout,         // -> CROSS_SHARD_TIMEOUT (Phase-2/rollback deadline)
    ChannelDown,           // control channel closed at submit
    CapacityExceeded,      // op/participant table full — fail closed
    WalFailure,
    Cancelled,             // operator cancel_basket()
    RecoveryInterrupted,   // restart mid-protocol — tombstoned fail-closed
};

// Public op snapshot: status() result and the dedup-cached value returned to
// a resubmitted operation_id.
struct BasketResult {
    BasketOpId op_id;
    uint8_t status;          // BasketStatus
    uint8_t code;            // BasketCode
    uint8_t leg_count;
    uint8_t legs_committed;
    uint8_t legs_released;
    uint8_t legs_nacked;
    uint8_t _pad[2];
};

// Per-leg notification for the engine binding (order state transitions) and
// tests. RESERVED/ACTIVATED/RELEASED map to order marking per Task 2.3.8;
// RESTITUTED is the post-commit cancel-compensation path.
enum class BasketLegEvent : uint8_t {
    Reserved = 1,    // balance locked; order logically RESERVED (5s TTL)
    Activated,       // commit applied; order goes live
    Released,        // pre-commit unwind (compensation / TTL expiry / cancel)
    Restituted,      // post-commit cancel-compensation (funds credited back)
};

struct BasketLegNotice {
    BasketOpId op_id;
    uint64_t account_id;
    uint64_t order_id;
    int64_t  amount;
    uint32_t leg_index;
    uint32_t shard_id;
    uint8_t  event;          // BasketLegEvent
    uint8_t  reason;         // BasketReleaseReason when Released/Restituted
    uint8_t  _pad[6];
};
using BasketLegSink = void (*)(void* ctx, const BasketLegNotice& n) noexcept;

// Metrics POD — the mapping of record for the Task 2.3.8/2.3.14 Prometheus
// names (wiring Phase-07 Task 7.3.8). reservation_active is a GAUGE
// recomputed on every sink emission: live coordinator ops + live
// participant locks on this shard.
struct CrossShardMetrics {
    uint64_t transactions_total = 0;          // cross_shard_transactions_total
    uint64_t transactions_success = 0;        // Committed
    uint64_t transactions_failure = 0;        // Compensated / Failed
    uint64_t transaction_duration_ns_sum = 0; // for avg duration
    uint64_t reservation_timeout_total = 0;   // cross_shard_reservation_timeout_total
    uint64_t reservation_active = 0;          // cross_shard_reservation_active
    uint64_t compensations_total = 0;         // ops entering Compensating
    uint64_t limit_exceeded_total = 0;        // CROSS_SHARD_LIMIT_EXCEEDED rejects
    uint64_t releases_sent = 0;
    uint64_t commits_sent = 0;
    uint64_t reaper_scans = 0;
    uint64_t wal_failures = 0;
    uint64_t bad_frames = 0;
};
using CrossShardMetricsSink =
    void (*)(void* ctx, const CrossShardMetrics& m) noexcept;

struct BasketCoordinatorOptions {
    uint64_t reserve_ttl_ns = 5'000'000'000;      // 5s — §24 #214 (supersedes 30s)
    uint64_t total_deadline_ns = 8'000'000'000;   // 5s reserve + 3s commit/compensate
    uint64_t reaper_interval_ns = 2'000'000'000;  // Task 2.3.14 compensation reaper
    uint32_t max_ops_per_account = 10;            // Task 2.3.14 concurrent cap
    uint16_t shard_id = 0;                        // THIS engine's shard
};

// --- coordinator + participant roles -------------------------------------------

// Internal leg lifecycle (coordinator view):
//   Pending ──Ack──> Reserved ──Commit──> CommitSent ──CommitAck(applied)──> Committed
//     │                 │                    │
//     │   Nack ╰──> Nacked                   ╰──CommitAck(!applied)──> [op compensates]
//     ╰──Release──> ReleaseSent ──ReleaseAck──> Released       [terminal]
// Participant lock lifecycle (parts_ table):
//   (none) ──Req+headroom──> Reserved ──Commit──> Committed     [durable]
//                  │             ╰──Release/TTL──> Released    [tombstone]
//                  ╰──Req+shortfall──> Denied                  [tombstone]

class CrossShardCoordinator {
public:
    // Fixed capacities — open-addressed tables inside the object, pow2,
    // zero allocation anywhere. Tombstone discipline identical to
    // CrossShardMarginCoordinator (Denied/Released/terminal slots reusable
    // only after key-absence is proven; probes end on Empty).
    static constexpr uint32_t kMaxOps = 1024;        // coordinator op records
    static constexpr uint32_t kMaxPartRes = 2048;    // participant locks
    static constexpr uint32_t kMaxAccounts = 4096;   // local balance store
    static constexpr uint32_t kMaxLegs = 8;          // legs per basket

    // ctl may be nullptr (standalone — coordinator submits fail closed,
    // participant role unreachable); wal may be nullptr (tests/bench).
    CrossShardCoordinator(IpcChannel* ctl, Wal* wal,
                          BasketCoordinatorOptions opts) noexcept;

    CrossShardCoordinator(const CrossShardCoordinator&) = delete;
    CrossShardCoordinator& operator=(const CrossShardCoordinator&) = delete;

    // Coordinator election: the participant with the LOWEST shard_id
    // coordinates (Task 2.3.8 rule). Pure function.
    [[nodiscard]] static uint32_t coordinator_shard(const BasketLegSpec* legs,
                                                    uint32_t n) noexcept;

    // --- Coordinator role -----------------------------------------------------

    // Deterministic id generator for callers without a UUID source:
    // {hi: mixed shard+time, lo: ++seq} — unique per shard per process.
    // Caller-supplied UUID v4 ids are equally accepted via submit().
    [[nodiscard]] BasketOpId generate_op_id(uint64_t now_ns) noexcept;

    // Admits a basket. MUST be invoked on the coordinator shard
    // (coordinator_shard(legs) == opts.shard_id; else Rejected/NotCoordinator).
    //
    // Dedup: an op_id already in the table (in-flight OR terminal) returns
    // its cached BasketResult immediately — same ID, same answer, zero
    // side-effects (Task 2.3.8 "operation_id deduplicates retries").
    // Gate rejections (BadArgs/NotCoordinator/LimitExceeded/Capacity) are
    // NOT cached — a retry must re-evaluate the gate.
    //
    // On admission: WAL intent, then Phase-1 Reserve for every leg (remote
    // legs get ReserveReq frames; legs homed on THIS shard take the
    // in-process participant path). If a send fails the op enters
    // compensation in the same call — partially-issued state is never
    // parked (fail-closed, spec §2.7).
    [[nodiscard]] BasketResult submit(const BasketLegSpec* legs,
                                      uint32_t leg_count,
                                      uint64_t initiator_account,
                                      BasketOpId op_id,
                                      uint64_t now_ns) noexcept;

    // Cached result for an op_id — Unknown when there is no record.
    [[nodiscard]] BasketResult status(BasketOpId op_id) const noexcept;

    // Operator-initiated cancel: an in-flight op enters compensation.
    // Terminal/unknown ops are an idempotent no-op (returns false only for
    // Unknown — the op never existed here).
    bool cancel(BasketOpId op_id, uint64_t now_ns) noexcept;

    // --- Engine pump ----------------------------------------------------------

    // Single pump entry: drains inbound control frames (bounded per call)
    // then runs the CompensationReaper when >= reaper_interval_ns of
    // engine-logical time elapsed. now_ns MUST be the engine's WAL TIME_TICK
    // clock — no system clock is ever read.
    void on_time_tick(uint64_t now_ns) noexcept;

    // --- Participant balance plumbing ------------------------------------------
    // Local store (tests/standalone) or an installed provider for the real
    // ledger. lock = available -= amt / locked += amt; commit consumes the
    // lock; release credits it back; committed-leg release restitutes to
    // available (cancel-compensation).
    void set_balance(uint64_t account_id, int64_t available) noexcept;
    void set_balance_provider(int64_t (*fn)(void* ctx,
                                          uint64_t account_id) noexcept,
                              void* ctx) noexcept;
    [[nodiscard]] int64_t available(uint64_t account_id) const noexcept;
    [[nodiscard]] int64_t locked(uint64_t account_id) const noexcept;

    // --- Sinks -------------------------------------------------------------------
    void set_leg_sink(BasketLegSink fn, void* ctx) noexcept;
    void set_metrics_sink(CrossShardMetricsSink fn, void* ctx) noexcept;

    // --- Recovery ------------------------------------------------------------------
    // Replays BASKET_* entries: Begin -> pending tombstone, Reserve ->
    // participant lock restored (expired ones swept on the first tick),
    // LegDone -> leg transition, Outcome -> terminal dedup tombstone.
    // Ops left non-terminal by the log are marked Failed/RecoveryInterrupted
    // (fail-closed — a half-known op must never auto-commit). Returns the
    // number of records applied.
    size_t recover(WalReader& reader) noexcept;

    // --- Introspection --------------------------------------------------------------
    [[nodiscard]] const CrossShardMetrics& metrics() const noexcept {
        return metrics_;
    }
    [[nodiscard]] uint16_t shard_id() const noexcept { return opts_.shard_id; }
    [[nodiscard]] uint32_t live_ops() const noexcept { return live_ops_; }
    [[nodiscard]] uint32_t live_locks() const noexcept { return live_locks_; }
    // Participant-side lock record state for a (op,leg) — test/diagnostic
    // surface mirroring ReservationState naming (0 empty, 1 reserved,
    // 2 committed, 3 denied, 4 released).
    [[nodiscard]] uint8_t part_state(BasketOpId op_id,
                                     uint32_t leg_index) const noexcept;

private:
    enum class LegSt : uint8_t {
        Pending = 0,
        Reserved,
        Nacked,
        CommitSent,
        Committed,
        ReleaseSent,
        Released,
    };

    enum class PartSt : uint8_t {
        Empty = 0,
        Reserved,
        Committed,
        Denied,
        Released,
    };

    struct LegRec {
        uint64_t account_id;
        uint64_t order_id;
        int64_t  amount;
        uint32_t shard_id;
        uint32_t instrument_id;
        uint8_t  state;   // LegSt
        uint8_t  local;   // hosted on this shard — no wire
        uint8_t  was_committed;  // leg activated before compensation —
                                 // an unconfirmed unwind lands the op in
                                 // Failed (uncertain), never Compensated
        uint8_t  _pad;
    };

    struct OpRec {
        BasketOpId id;
        uint64_t initiator_account;
        uint64_t t_submit_ns;
        uint64_t reserve_deadline_ns;
        uint64_t total_deadline_ns;
        uint8_t  state;       // BasketStatus (Empty sentinel uses Unknown+
                              // leg_count==0&&initiator==0 -> see occupied_)
        uint8_t  code;
        uint8_t  leg_count;
        uint8_t  occupied;    // 0 = free slot (probe terminator)
        uint8_t  recovered;   // restored from WAL
        uint8_t  _pad[3];
        LegRec legs[kMaxLegs];
    };

    struct PartRec {
        BasketOpId op_id;
        uint64_t coordinator_shard;
        uint64_t account_id;
        uint64_t order_id;
        int64_t  amount;
        uint64_t expires_at_ns;
        uint32_t leg_index;
        uint32_t instrument_id;
        uint8_t  state;      // PartSt
        uint8_t  recovered;  // restored from WAL — expiry sweeps as
                             // RecoveryOrphan for audit
        uint8_t  _pad[6];
    };

    struct AccountBal {
        uint64_t account_id;   // 0 = empty
        int64_t  available;
        int64_t  locked;
    };

    // --- table primitives ---
    static uint32_t hash_id(BasketOpId id) noexcept;
    OpRec* find_op(BasketOpId id) noexcept;
    const OpRec* find_op(BasketOpId id) const noexcept;
    OpRec* insert_op(BasketOpId id) noexcept;
    PartRec* find_part(BasketOpId id, uint32_t leg_index) noexcept;
    const PartRec* find_part(BasketOpId id, uint32_t leg_index) const noexcept;
    PartRec* insert_part(BasketOpId id, uint32_t leg_index) noexcept;
    AccountBal* find_bal(uint64_t account_id) noexcept;
    const AccountBal* find_bal(uint64_t account_id) const noexcept;
    AccountBal* ensure_bal(uint64_t account_id) noexcept;

    // --- coordinator transitions ---
    // Phase-1 completion check: all legs Reserved -> issue Commit for each
    // (local legs commit synchronously); when every leg is Committed the op
    // finalizes Committed.
    void maybe_commit(OpRec& op, uint64_t now_ns) noexcept;
    // Enter compensation: Release every non-terminal leg (remote wire or
    // in-process), tombstone accounting, then maybe_terminal().
    void begin_compensation(OpRec& op, BasketCode code,
                            uint64_t now_ns) noexcept;
    // Committed/Compensated/Failed finalize: WAL outcome + metrics + sink.
    void finalize_op(OpRec& op, BasketStatus st, BasketCode code,
                     uint64_t now_ns) noexcept;
    void maybe_terminal(OpRec& op, uint64_t now_ns) noexcept;
    BasketResult snapshot(const OpRec& op) const noexcept;
    uint32_t live_ops_of(uint64_t account_id) const noexcept;

    // --- participant transitions ---
    // In-process reserve used by inbound ReserveReq AND by coordinator legs
    // homed on this shard (identical lock/TTL semantics either way).
    PartRec* local_reserve(const BasketReserveReqBody& m, uint64_t coord_shard,
                           bool emit_wire_ack) noexcept;
    // Apply commit/release to a participant record; emits leg notices,
    // adjusts the balance store, WALs the transition.
    void part_commit(PartRec& r) noexcept;
    void part_release(PartRec& r, BasketReleaseReason reason,
                      bool send_wire_ack) noexcept;

    // --- wire send helpers ---
    bool send_frame(BasketCtlType t, const void* body, uint32_t dst) noexcept;
    bool send_reserve_req(const OpRec& op, const LegRec& leg,
                          uint32_t leg_index) noexcept;
    bool send_commit(const OpRec& op, uint32_t leg_index) noexcept;
    bool send_release(const OpRec& op, uint32_t leg_index,
                      BasketReleaseReason reason) noexcept;
    bool send_expired_notice(const PartRec& r) noexcept;

    // --- inbound dispatch ---
    void on_frame(const uint8_t* buf, uint32_t len, uint64_t now_ns) noexcept;
    void on_reserve_req(const BasketCtlView& v, uint64_t now_ns) noexcept;
    void on_reserve_vote(const BasketCtlView& v, bool ack,
                         uint64_t now_ns) noexcept;
    void on_commit(const BasketCtlView& v, uint64_t now_ns) noexcept;
    void on_commit_ack(const BasketCtlView& v, uint64_t now_ns) noexcept;
    void on_release(const BasketCtlView& v, uint64_t now_ns) noexcept;
    void on_release_ack(const BasketCtlView& v, uint64_t now_ns) noexcept;

    // CompensationReaper — runs at most once per reaper_interval_ns of
    // engine-logical time (Task 2.3.14). Sweeps coordinator ops past their
    // layered deadlines and participant locks past TTL.
    void reap(uint64_t now_ns) noexcept;
    void emit_leg(BasketOpId op, uint32_t leg_index, uint32_t shard,
                  uint64_t account, uint64_t order, int64_t amount,
                  BasketLegEvent ev, BasketReleaseReason reason) noexcept;
    void emit_metrics() noexcept;

    // --- WAL helpers (false = append failed; callers fail closed) ---
    bool wal_begin(const OpRec& op) noexcept;
    bool wal_reserve(const PartRec& r) noexcept;
    bool wal_leg_done(const PartRec& r, uint8_t kind,
                      BasketReleaseReason reason) noexcept;
    bool wal_outcome(const OpRec& op) noexcept;

    IpcChannel* ctl_;
    Wal* wal_;
    BasketCoordinatorOptions opts_;

    // Fixed-capacity open-addressed tables — NSDMI zero-init is REQUIRED:
    // occupied==0 / account_id==0 / state==Empty are the probe terminators,
    // so a garbage byte would corrupt every lookup (fail-closed contract).
    OpRec ops_[kMaxOps]{};
    PartRec parts_[kMaxPartRes]{};
    AccountBal bals_[kMaxAccounts]{};

    uint64_t id_seq_ = 0;
    uint64_t next_reap_ns_ = 0;   // first tick always reaps
    uint32_t live_ops_ = 0;
    uint32_t live_locks_ = 0;

    int64_t (*bal_fn_)(void*, uint64_t) noexcept = nullptr;
    void* bal_ctx_ = nullptr;
    BasketLegSink leg_sink_ = nullptr;
    void* leg_ctx_ = nullptr;
    CrossShardMetricsSink met_sink_ = nullptr;
    void* met_ctx_ = nullptr;

    CrossShardMetrics metrics_{};
};

}  // namespace exch
