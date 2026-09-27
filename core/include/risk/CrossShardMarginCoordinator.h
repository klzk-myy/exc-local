#pragma once

// PHASE-02 TASK-2.3.12 — cross-shard margin coordination interface
// (spec §5.35 `shard_margin_reservations`, §13.1 partition-degradation
// safeguard, §24 #176).
//
// Shards are partitioned by currency pair (spec §2.2), so a PORTFOLIO-margin
// account's collateral spans engines. When an order arrives at this shard
// whose required initial margin exceeds local headroom, the engine asks the
// Go Risk Coordinator (Phase-19 Task 19.3.11 — a later phase; tests drive a
// scripted double here) for an atomic slice reservation over Aeron IPC:
//
//   engine -> coordinator : MARGIN_RESERVE_REQ  (slice request)
//   coordinator -> engine : MARGIN_RESERVE_ACK  (slice granted, may be < req)
//                           MARGIN_RESERVE_NACK (refused)
//                           MARGIN_RESERVE_REQ  (coordinator asks THIS shard
//                                                to host a lock for a remote
//                                                reservation — participant
//                                                role)
//   both directions       : MARGIN_RELEASE      (normal release, cancel of
//                                                an in-flight REQ, or
//                                                timeout compensation)
//
// Layered timeout budget (spec §13.1 amended 2026-09-19, remediation #8 —
// supersedes the single 10ms threshold):
//   * 500µs RPC budget   : no ACK by t0+500µs -> pessimistic fallback applies
//                          IMMEDIATELY: standalone local evaluation only;
//                          orders relying on external-shard correlation
//                          offsets reject with CROSS_SHARD_MARGIN_UNAVAILABLE
//                          (HTTP 503, spec §23). The REQ stays in-flight.
//   * >10ms hard deadline: the still-unanswered REQ is cancelled and
//                          compensated (RELEASE emitted, WAL'd) — and a late
//                          ACK landing inside (500µs, 10ms] is granted then
//                          immediately released for the same reason: the
//                          triggering admission already took the floor.
//
// Channel: an IpcChannel* — production wires AeronChannel on
// `aeron:ipc?alias=margin_ctl_*` (separate stream ids from the order-flow
// channel so control traffic never queues behind market data). nullptr =>
// standalone mode: every cross-shard need fails closed.
//
// WAL contract — two payload kinds on MARGIN_RESERVE/MARGIN_RELEASE:
//   outbound REQ issued  -> MARGIN_RESERVE origin=2 (INTENT: uncommitted;
//                           logged so replay reseeds the local id sequence
//                           past every announced id — a late ACK for a
//                           pre-restart REQ can then never collide with a
//                           recycled id. Never restores live state.)
//   slice committed      -> MARGIN_RESERVE origin=0 (Local grant usable by
//                           our admissions) / origin=1 (Hosted lock of local
//                           headroom for a remote shard)
//   slice released etc.  -> MARGIN_RELEASE (reason = ReleaseReason)
// A Pending request's intent entry carries no committed state — replay turns
// it into nothing but a tombstone-grade id reservation (fail-closed).
// recover() replays a segment into the live tables; granted slices whose
// expires_at already passed are released (RecoveryOrphan) on the first
// poll().
//
// Threading: single-writer contract identical to Wal — poll()/begin_reserve()/
// resolve_wait()/release() run on the matching-loop thread only. covers() and
// evaluate() are read-mostly noexcept O(1) probes over fixed-capacity
// open-addressed tables: zero allocation, zero syscalls — admission stays
// inside the <10µs slice-reservation budget (Phase-02 AC).
//
// Codec choice: fixed-size packed structs, NOT FlatBuffers — the largest
// frame is 69 bytes, every field is fixed-offset, and the Go side encodes
// with encoding/binary (same layout-mirroring discipline as ShmRing). Adding
// a generated-code verify step to a 4-type control protocol buys nothing.
// Little-endian on the wire (x86-64 target per spec §3).

#include <cstddef>
#include <cstdint>
#include <cstring>

#include "ipc/IpcChannel.hpp"
#include "wal/Wal.hpp"
#include "wal/WalEntry.hpp"

namespace exch {

// §23 error codes emitted by this interface (registered in Phase-05
// Task 5.3.21 — these literals are the code of record until then, same
// convention as utils/error_severity.hpp).
inline constexpr char kCodeCrossShardMarginUnavailable[] =
    "CROSS_SHARD_MARGIN_UNAVAILABLE";  // HTTP 503 — 500µs budget missed
inline constexpr char kCodeCrossShardMarginTimeout[] =
    "CROSS_SHARD_MARGIN_TIMEOUT";      // HTTP 504 — >10ms hard deadline

// --- Wire codec ---------------------------------------------------------------

inline constexpr uint32_t kMarginCtlMagic = 0x4D475243u;   // "MRGC" LE
inline constexpr uint16_t kMarginCtlVersion = 1u;

enum class MarginCtlType : uint8_t {
    ReserveReq  = 1,  // MARGIN_RESERVE_REQ
    ReserveAck  = 2,  // MARGIN_RESERVE_ACK
    ReserveNack = 3,  // MARGIN_RESERVE_NACK
    Release     = 4,  // MARGIN_RELEASE
};

enum class ReleaseReason : uint8_t {
    Complete = 1,              // order lifecycle done — normal release
    OrderRejected = 2,         // admission denied — slice never used
    TimeoutCompensate = 3,     // hard-deadline cancel / late-ACK unwind
    Expired = 4,               // expires_at_ns reached
    CoordinatorInitiated = 5,  // inbound RELEASE from the coordinator
    RecoveryOrphan = 6,        // recovered slice released on restart sweep
};

enum class NackReason : uint32_t {
    InsufficientHeadroom = 1,
    UnknownAccount = 2,
    CoordinatorOverload = 3,
    ShardUnreachable = 4,
};

// dst_shard sentinel: let the coordinator pick which shard hosts the slice.
inline constexpr uint32_t kMarginCoordinatorPicks = 0xFFFFFFFFu;

// REQ flag bits.
inline constexpr uint8_t kMarginReqFlagCorrelationOffset = 1u << 0;

#pragma pack(push, 1)
struct MarginCtlHeader {
    uint32_t magic;     // kMarginCtlMagic
    uint8_t  type;      // MarginCtlType
    uint8_t  flags;     // reserved, must be 0
    uint16_t version;   // kMarginCtlVersion
};

struct MarginReserveReqBody {
    uint64_t reservation_id;  // issuer-assigned: high16=issuer shard, low48=seq
    uint64_t account_id;
    uint64_t order_id;        // admission context (0 = none)
    uint32_t src_shard;       // shard whose order needs the slice (consumer)
    uint32_t dst_shard;       // host shard or kMarginCoordinatorPicks
    uint32_t instrument_id;
    uint8_t  req_flags;       // kMarginReqFlag*
    int64_t  amount;          // requested slice, 1e8-scaled ticks
    uint64_t expires_at_ns;   // CLOCK_REALTIME expiry; 0 = no expiry
};

struct MarginReserveAckBody {
    uint64_t reservation_id;
    uint64_t account_id;
    uint32_t shard_id;        // shard hosting the granted slice
    int64_t  granted_amount;  // <= requested (partial slice grant allowed)
    uint64_t expires_at_ns;   // authoritative expiry assigned by coordinator
};

struct MarginReserveNackBody {
    uint64_t reservation_id;
    uint64_t account_id;
    uint32_t shard_id;
    uint32_t reason;          // NackReason
};

struct MarginReleaseBody {
    uint64_t reservation_id;
    uint64_t account_id;
    uint32_t shard_id;        // shard hosting the slice being released
    uint8_t  reason;          // ReleaseReason
};
#pragma pack(pop)

static_assert(sizeof(MarginCtlHeader) == 8);
static_assert(sizeof(MarginReserveReqBody) == 53);
static_assert(sizeof(MarginReserveAckBody) == 36);
static_assert(sizeof(MarginReserveNackBody) == 24);
static_assert(sizeof(MarginReleaseBody) == 21);

// Largest control frame: 8 + 53 = 61 bytes.
inline constexpr uint32_t kMarginCtlMaxFrame =
    sizeof(MarginCtlHeader) + sizeof(MarginReserveReqBody);

struct MarginCtlView {
    MarginCtlType type;
    union {
        MarginReserveReqBody req;
        MarginReserveAckBody ack;
        MarginReserveNackBody nack;
        MarginReleaseBody rel;
    };
};

enum class MarginCtlDecode : uint8_t {
    Ok = 0,
    TooShort,
    BadMagic,
    BadVersion,
    UnknownType,
};

[[nodiscard]] const char* margin_ctl_decode_str(MarginCtlDecode r) noexcept;

// Serializes header+body into dst (cap >= kMarginCtlMaxFrame for any type).
// Returns bytes written, 0 on bad args (dst null / cap short / type unknown).
[[nodiscard]] uint32_t margin_ctl_encode(uint8_t* dst, uint32_t cap,
                                         MarginCtlType type,
                                         const void* body) noexcept;

// Validates + copies a frame into *out. Untrusted bytes are never aliased.
[[nodiscard]] MarginCtlDecode margin_ctl_decode(const void* buf, uint32_t len,
                                                MarginCtlView* out) noexcept;

// --- WAL payloads (WalEventType::MARGIN_RESERVE / MARGIN_RELEASE) -------------

#pragma pack(push, 1)
// Written when a slice commitment becomes durable: an ACK to our own REQ
// (origin=0 Local — the slice is usable by OUR admissions), our ACK to a
// coordinator REQ (origin=1 Hosted — a slice of LOCAL headroom locked for a
// remote shard's reservation), or when an outbound REQ is issued (origin=2
// Intent — uncommitted; replay only reseeds the id sequence and counts it).
struct WalMarginReservePayload {
    uint64_t reservation_id;
    uint64_t account_id;
    uint64_t order_id;         // admission context (0 = none)
    uint32_t consumer_shard;   // shard whose orders may draw on the slice
    uint32_t host_shard;       // shard holding the locked headroom
    uint32_t instrument_id;
    uint8_t  origin;           // 0 = Local, 1 = Hosted
    int64_t  amount;           // committed slice, 1e8-scaled ticks
    uint64_t expires_at_ns;
};

struct WalMarginReleasePayload {
    uint64_t reservation_id;
    uint64_t account_id;
    uint8_t  origin;
    uint8_t  reason;           // ReleaseReason
};
#pragma pack(pop)

static_assert(sizeof(WalMarginReservePayload) == 53);
static_assert(sizeof(WalMarginReleasePayload) == 18);

// --- Engine-side coordinator ---------------------------------------------------

struct MarginCoordinatorOptions {
    // Layered budget per spec §13.1: soft RPC budget, then hard deadline.
    int64_t rpc_budget_ns = 500'000;        // 500µs
    int64_t hard_deadline_ns = 10'000'000;  // 10ms
    uint16_t shard_id = 0;                  // THIS engine's shard
    // Clock injection for deterministic tests; null => real clocks.
    int64_t (*steady_ns_fn)(void* ctx) = nullptr;
    void* steady_ctx = nullptr;
    uint64_t (*now_ns_fn)(void* ctx) = nullptr;
    void* now_ctx = nullptr;
};

// Lifecycle of a reservation record (single table for both roles):
//
//   Pending ──ACK──> Granted ──release──> Released
//      │  ╲            (order admitted against the slice)
//      │   ╲──NACK──> Denied            [terminal]
//      │   ╲──t>soft─> TimedOut ──ACK──> Granted→compensated → Released
//      │                ╲──t>hard──────────compensate────────> Released
//      ╰──RELEASE/inbound───────────────> Released            [terminal]
//
//   Remote-origin: (no record) ──REQ+headroom──> GrantedHosted ──> Released
//                         ╲──REQ+shortfall───> Denied           [tombstone]
//
enum class ReservationState : uint8_t {
    Empty = 0,        // free slot (probe terminator)
    Pending,          // our REQ in-flight, inside all budgets
    TimedOut,         // soft budget expired — fallback applied, still in-flight
    Granted,          // slice usable by local admission
    GrantedHosted,    // slice of local headroom locked for a remote reservation
    Denied,           // NACK'd (tombstone — replays of the REQ re-NACK)
    Released,         // terminal: released / expired / compensated (tombstone)
};

struct MarginReservation {
    uint64_t id;
    uint64_t account_id;
    uint64_t order_id;
    uint32_t consumer_shard;
    uint32_t host_shard;
    uint32_t instrument_id;
    int64_t  amount;
    uint64_t expires_at_ns;   // realtime
    int64_t  t_soft_ns;       // steady: soft-budget deadline (local reqs)
    int64_t  t_hard_ns;       // steady: hard deadline
    uint8_t  state;           // ReservationState
    uint8_t  origin;          // 0 = Local, 1 = Hosted(remote request)
    uint8_t  req_flags;
};

enum class MarginResolve : uint8_t {
    Granted = 0,    // ACK inside budget — covers() now includes the slice
    Denied,         // NACK — nothing granted
    Fallback,       // soft budget expired -> pessimistic floor
                    // (reject relying orders: CROSS_SHARD_MARGIN_UNAVAILABLE)
    HardTimeout,    // crossed >10ms deadline — cancelled + compensated
                    // (order cancel: CROSS_SHARD_MARGIN_TIMEOUT)
    Unknown,        // id not found / not ours
};

// Result of evaluate() — local headroom + granted slices vs requirement.
struct MarginCheck {
    bool covered;       // usable_margin >= required
    int64_t shortfall;  // max(0, required - usable_margin) — the slice to ask for
};

class CrossShardMarginCoordinator {
public:
    // Fixed capacities — open-addressed tables inside the object, zero
    // allocation anywhere (hot-path noexcept guarantee). Both pow2.
    static constexpr uint32_t kMaxReservations = 4096;
    static constexpr uint32_t kMaxAccounts = 4096;

    // ctl may be nullptr (standalone — every cross-shard need fails closed);
    // wal may be nullptr (tests/bench without persistence).
    CrossShardMarginCoordinator(IpcChannel* ctl, Wal* wal,
                                MarginCoordinatorOptions opts) noexcept;

    CrossShardMarginCoordinator(const CrossShardMarginCoordinator&) = delete;
    CrossShardMarginCoordinator& operator=(const CrossShardMarginCoordinator&) = delete;

    // --- Requester path (order admission) --------------------------------------

    // Sends MARGIN_RESERVE_REQ for `amount` of `account_id`'s portfolio IM.
    // Returns the new reservation id (>0), or 0 on failure (channel down /
    // table full / bad args — all fail closed; the caller applies the
    // pessimistic floor).
    uint64_t begin_reserve(uint64_t account_id, uint32_t instrument_id,
                           int64_t amount, uint64_t expires_at_ns = 0,
                           bool needs_correlation_offset = false,
                           uint32_t dst_shard = kMarginCoordinatorPicks,
                           uint64_t order_id = 0) noexcept;

    // Bounded wait: drains the control channel until the reservation resolves
    // or the 500µs soft budget expires. NEVER spins past t_soft — Fallback is
    // returned at budget expiry while the REQ stays in-flight (poll()
    // continues its hard-deadline lifecycle in the background).
    [[nodiscard]] MarginResolve resolve_wait(uint64_t reservation_id) noexcept;

    // usable = local_headroom(account) - hosted_locks(account)
    //          + granted_slices(account);  covered iff usable >= required.
    // Overflow in the aggregate fails closed (covered=false). noexcept O(1).
    [[nodiscard]] MarginCheck evaluate(uint64_t account_id,
                                       int64_t required_margin) noexcept;
    [[nodiscard]] bool covers(int64_t required_margin,
                              uint64_t account_id) noexcept {
        return evaluate(account_id, required_margin).covered;
    }

    // --- Lifecycle --------------------------------------------------------------

    // Idempotent release: safe on any state, safe twice, safe on unknown id.
    // Granted/GrantedHosted -> RELEASE wire msg + WAL. Pending/TimedOut ->
    // cancel-in-flight (same wire msg). Terminal/unknown -> no-op true.
    // reason is caller-supplied for audit (Complete, OrderRejected, ...).
    bool release(uint64_t reservation_id, ReleaseReason reason) noexcept;
    bool release(uint64_t reservation_id) noexcept {
        return release(reservation_id, ReleaseReason::Complete);
    }

    // Drives the protocol: drains inbound frames (REQ/ACK/NACK/RELEASE), then
    // sweeps live reservations for soft/hard deadlines and realtime expiry.
    // Called every matching-loop iteration; work is O(live reservations).
    void poll() noexcept;

    // --- Local margin plumbing ----------------------------------------------------

    // Engine-local available margin for an account (1e8-scaled ticks). The
    // internal map is the default store; install a provider to bind the real
    // margin ledger instead (provider wins when set).
    void set_local_headroom(uint64_t account_id, int64_t headroom) noexcept;
    void set_local_margin_provider(int64_t (*fn)(void* ctx,
                                                 uint64_t account_id) noexcept,
                                   void* ctx) noexcept;

    // --- Recovery ------------------------------------------------------------------

    // Replays one WAL segment's MARGIN_RESERVE / MARGIN_RELEASE entries into
    // the tables and reseeds the local id sequence. Returns applied count.
    // Run BEFORE opening for traffic; granted slices restored here whose
    // expires_at already passed are released on the first poll().
    size_t recover(WalReader& reader) noexcept;

    // --- Introspection (metrics/tests) ----------------------------------------------

    [[nodiscard]] ReservationState state_of(uint64_t reservation_id) const noexcept;
    [[nodiscard]] int64_t granted_slices(uint64_t account_id) const noexcept; // usable inbound grants
    [[nodiscard]] int64_t hosted_locks(uint64_t account_id) const noexcept;   // outbound locks on local headroom
    [[nodiscard]] uint32_t live_reservations() const noexcept { return live_n_; }
    [[nodiscard]] uint16_t shard_id() const noexcept { return opts_.shard_id; }

    // Counters (monotonic; Prometheus mapping in Phase-07 Task 7.3.8).
    [[nodiscard]] uint64_t reqs_sent() const noexcept { return n_reqs_; }
    [[nodiscard]] uint64_t acks() const noexcept { return n_acks_; }
    [[nodiscard]] uint64_t nacks() const noexcept { return n_nacks_; }
    [[nodiscard]] uint64_t soft_timeouts() const noexcept { return n_soft_; }
    [[nodiscard]] uint64_t hard_timeouts() const noexcept { return n_hard_; }
    [[nodiscard]] uint64_t compensations() const noexcept { return n_comp_; }
    [[nodiscard]] uint64_t releases() const noexcept { return n_rel_; }
    [[nodiscard]] uint64_t wal_failures() const noexcept { return n_wal_fail_; }
    [[nodiscard]] uint64_t bad_frames() const noexcept { return n_bad_; }

private:
    struct Slot {
        MarginReservation r;
    };
    struct AccountAgg {
        uint64_t account_id = 0;      // 0 = empty (account ids are BIGSERIAL ≥1)
        int64_t granted_in = 0;       // sum of usable Granted slices
        int64_t locked_out = 0;       // sum of GrantedHosted slices
        int64_t local_headroom = 0;   // set_local_headroom store
    };

    // Open-addressed lookups (pow2 capacities; linear probe).
    MarginReservation* find_res(uint64_t id) noexcept;
    const MarginReservation* find_res(uint64_t id) const noexcept;
    // Insert-at-id: returns slot or nullptr (table full -> fail closed).
    // Reuses first tombstone (Denied/Released) seen after key absence is
    // proven — classic tombstone discipline, probes still end on Empty.
    MarginReservation* insert_res(uint64_t id) noexcept;
    AccountAgg* find_agg(uint64_t account_id) noexcept;
    const AccountAgg* find_agg(uint64_t account_id) const noexcept;
    AccountAgg* ensure_agg(uint64_t account_id) noexcept;  // find-or-insert

    static uint32_t hash64(uint64_t v) noexcept {
        // Fibonacci mix — ids carry a shard prefix that defeats low-bit
        // hashing, so mix high bits down before masking.
        return static_cast<uint32_t>((v * 0x9E3779B97F4A7C15ULL) >> 32);
    }

    int64_t steady() const noexcept;
    uint64_t wall_now() const noexcept;
    int64_t local_available(uint64_t account_id) const noexcept;

    // Message send helpers (encode + ctl_->send); false on channel failure.
    bool send_req(const MarginReservation& r) noexcept;
    bool send_ack(const MarginReservation& r) noexcept;
    bool send_nack(uint64_t id, uint64_t account, uint32_t shard,
                   NackReason reason) noexcept;
    bool send_release(const MarginReservation& r,
                      ReleaseReason reason) noexcept;

    // Inbound dispatch.
    void on_frame(const uint8_t* buf, uint32_t len) noexcept;
    void on_req(const MarginReserveReqBody& m) noexcept;   // participant role
    void on_ack(const MarginReserveAckBody& m) noexcept;
    void on_nack(const MarginReserveNackBody& m) noexcept;
    void on_release(const MarginReleaseBody& m) noexcept;

    // Deadline/expiry sweep over the live-id list (O(live)).
    void sweep() noexcept;

    // WAL helpers — false only matters for the grant path (fail closed).
    bool wal_reserve(const MarginReservation& r) noexcept;
    // Uncommitted-intent record (origin=2): reserves the id in the WAL so
    // replay reseeds the sequence. Written before the REQ hits the wire.
    bool wal_intent(const MarginReservation& r) noexcept;
    bool wal_release(const MarginReservation& r,
                     ReleaseReason reason) noexcept;

    // Commit a local grant: state->Granted, agg += , WAL (compensate on WAL
    // failure — a slice we cannot persist is a slice we must not hold).
    void grant_local(MarginReservation& r, int64_t amount,
                     uint64_t expires_at_ns) noexcept;
    // Shared release path: unwind agg, WAL, tombstone, drop from live list.
    // send_wire=false during replay/inbound-release handling; write_wal=false
    // during replay (the entry being replayed IS the record).
    void finalize_release(MarginReservation& r, ReleaseReason reason,
                          bool send_wire, bool write_wal) noexcept;
    void live_drop(uint64_t id) noexcept;

    IpcChannel* ctl_;
    Wal* wal_;
    MarginCoordinatorOptions opts_;

    Slot slots_[kMaxReservations];
    AccountAgg aggs_[kMaxAccounts];
    uint64_t live_ids_[kMaxReservations];  // live reservation ids for sweep
    uint32_t live_n_ = 0;
    uint64_t local_seq_ = 0;               // low-48 id sequence (WAL-reseeded)

    int64_t (*local_fn_)(void*, uint64_t) noexcept = nullptr;
    void* local_ctx_ = nullptr;

    uint64_t n_reqs_ = 0, n_acks_ = 0, n_nacks_ = 0, n_soft_ = 0, n_hard_ = 0;
    uint64_t n_comp_ = 0, n_rel_ = 0, n_wal_fail_ = 0, n_bad_ = 0;
};

}  // namespace exch
