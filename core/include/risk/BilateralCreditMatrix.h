#pragma once

// PHASE-02 TASK-2.3.24 — in-memory bilateral credit matrix and counterparty
// matching filter (spec §3.3b, §13.8, §24 #403).
//
// Principal-to-principal institutional matching requires sufficient *mutual*
// directed credit: both maker→taker and taker→maker relationships must have
// headroom >= fill notional before a match may commit (spec §13.8.1). The
// matrix lives in shared memory so the Go Risk service can update limits
// out-of-band (CREDIT_UPDATE control messages over Aeron, decoded through
// credit_ctl_decode) while the matching loop reads lock-free.
//
// Shared-memory layout (ABI-fixed; the Go writer mirrors it byte-for-byte —
// services/internal/ipc mirrors ShmRing the same way):
//
//   shm object: "/exchange_credit_matrix" (kDefaultShmName)
//   offset  0   u64 magic          = kMagic ("EXCRDIT1")
//   offset  8   u32 version        = kVersion (1)
//   offset 12   u32 max_parties    = kMaxParties (1024)
//   offset 16   u64 update_seq     — bumped (release) on every applied
//                                    CREDIT_UPDATE; liveness/observability
//                                    for the control plane
//   offset 24   u64 writer_pid     — stamped by whoever applies updates
//                                    (kill(pid,0) probe, same trick as
//                                    ShmRing producer_pid)
//   offset 32.. reserved zero pad to kHeaderBytes (256)
//   offset 256  u64 cell[a][b]     — directed remaining credit in notional
//                                    ticks (1e8-scaled); row-major:
//                                    cell_off(a,b) = 256 + (a*1024 + b)*8
//
//   total size = 256 + 1024*1024*8 = 8,388,864 bytes (~8 MiB)
//
// Concurrency contract:
//   - can_match()/credit_limit() are wait-free pure reads (two relaxed atomic
//     loads) — safe from the matching loop while the Go writer mutates.
//   - try_debit() is lock-free CAS on both directed cells; it can NEVER drive
//     a cell below zero (CAS precondition cur >= n) — concurrent debits
//     cannot double-spend. A failed second direction is compensated by
//     re-crediting the first, so the pair is never left half-debited.
//   - apply_update() is a plain atomic store; writers must serialize
//     CREDIT_UPDATE application themselves (single Go writer per spec §13.8).
//   - Zero-initialized cell == NO credit → screened out (fail-closed; the
//     matching engine consults this matrix only for credit-screened
//     institutional flow — anonymous retail flow does not route through it).
//
// Consume-or-skip contract for the matching loop (spec §3.3b / §24 #403):
//   walking the opposite book, for each resting order the engine calls
//   consume_or_skip(maker_party, taker_party, fill_notional):
//     Consume → proceed with the fill (credit already debited both ways).
//     Skip    → leave the resting order in place with its price-time
//               priority intact and advance to the next eligible order.
//   Skip must NEVER remove or reorder the resting order. When every resting
//   order on the opposite side is skipped, the taker is rejected with
//   BILATERAL_CREDIT_EXHAUSTED (HTTP 409, spec §23) — never partially matched
//   against credit-ineligible counterparties.
//
// This class is not the engine hook itself — MatchingEngine wiring lands with
// the Task 2.3.4 matching loop; this is the standalone screened-liquidity
// primitive + its control-plane codec.

#include <atomic>
#include <cstdint>
#include <string_view>

namespace exch {

// --- Shared-memory layout constants (Go parity) ------------------------------

inline constexpr uint64_t kCreditMatrixMagic = 0x3154494452435845ULL;  // "EXCRDIT1" LE
inline constexpr uint32_t kCreditMatrixVersion = 1u;
inline constexpr uint32_t kCreditMaxParties = 1024u;  // spec §3.3b sizing

inline constexpr uint64_t kCreditOffMagic = 0;
inline constexpr uint64_t kCreditOffVersion = 8;
inline constexpr uint64_t kCreditOffMaxParties = 12;
inline constexpr uint64_t kCreditOffUpdateSeq = 16;
inline constexpr uint64_t kCreditOffWriterPid = 24;
inline constexpr uint64_t kCreditHeaderBytes = 256;
inline constexpr uint64_t kCreditCellBytes = sizeof(uint64_t);
inline constexpr uint64_t kCreditMatrixBytes =
    kCreditHeaderBytes +
    static_cast<uint64_t>(kCreditMaxParties) * kCreditMaxParties *
        kCreditCellBytes;  // 8,388,864

// Byte offset of cell[a][b] inside the shm image (row-major directed edges).
[[nodiscard]] inline constexpr uint64_t credit_cell_offset(uint32_t a,
                                                           uint32_t b) noexcept {
    return kCreditHeaderBytes +
           (static_cast<uint64_t>(a) * kCreditMaxParties + b) *
               kCreditCellBytes;
}

// --- CREDIT_UPDATE control-plane codec ---------------------------------------
//
// Plain packed struct, deliberately NOT FlatBuffers: control messages are
// fixed-size 32-byte frames; pulling the generated schema into the matching
// hot loop buys nothing and adds a verify pass. Little-endian, versioned;
// the Go writer encodes with encoding/binary (mirrored in
// services/internal/ipc — same discipline as the ShmRing layout contract).
//
// One CREDIT_UPDATE sets ONE directed edge: limit[a][b] = new_limit. The Go
// Risk service emits two updates (a→b, b→a) for symmetric bilateral changes;
// asymmetric pools (spot vs forward/NDF, spec §13.8.2) stay expressible.

inline constexpr uint32_t kCreditCtlMagic = 0x55445243u;  // "CRDU" LE
inline constexpr uint16_t kCreditCtlVersion = 1u;

enum class CreditCtlType : uint8_t {
    CreditUpdate = 1,  // CREDIT_UPDATE
};

#pragma pack(push, 1)
struct CreditCtlHeader {
    uint32_t magic;     // kCreditCtlMagic
    uint8_t  type;      // CreditCtlType
    uint8_t  flags;     // reserved, must be 0
    uint16_t version;   // kCreditCtlVersion
};

struct CreditUpdateMsg {
    CreditCtlHeader hdr;
    uint64_t seq;        // producer sequence — gap detection for the writer
    uint32_t party_a;    // directed edge a -> b ...
    uint32_t party_b;
    uint64_t new_limit;  // ... receives this remaining-credit value (ticks)
};
#pragma pack(pop)

static_assert(sizeof(CreditCtlHeader) == 8);
static_assert(sizeof(CreditUpdateMsg) == 32);

enum class CreditCtlDecode : uint8_t {
    Ok = 0,
    TooShort,       // len < sizeof(header+body)
    BadMagic,
    BadVersion,
    UnknownType,
    PartyOutOfRange,  // party >= kMaxParties — fail closed, never apply
};

[[nodiscard]] const char* credit_ctl_decode_str(CreditCtlDecode r) noexcept;

// Validates header + copies the body into *out (unchecked raw shm/Aeron
// buffers are never aliased — parsing writes into caller memory, fail-closed
// on every malformed field).
[[nodiscard]] CreditCtlDecode credit_ctl_decode(const void* buf, uint32_t len,
                                                CreditUpdateMsg* out) noexcept;

// --- The matrix ---------------------------------------------------------------

class BilateralCreditMatrix {
public:
    enum class Gate : uint8_t {
        Consume = 0,  // credit reserved (both directions debited) — fill may proceed
        Skip,         // insufficient mutual credit — leave order in place, try next
    };

    static constexpr std::string_view kDefaultShmName = "exchange_credit_matrix";

    BilateralCreditMatrix() = default;
    BilateralCreditMatrix(std::string_view shm_name, bool create) {
        open(shm_name, create);
    }
    ~BilateralCreditMatrix() { close(); }

    BilateralCreditMatrix(const BilateralCreditMatrix&) = delete;
    BilateralCreditMatrix& operator=(const BilateralCreditMatrix&) = delete;
    BilateralCreditMatrix(BilateralCreditMatrix&& o) noexcept { move_from(o); }
    BilateralCreditMatrix& operator=(BilateralCreditMatrix&& o) noexcept {
        if (this != &o) { close(); move_from(o); }
        return *this;
    }

    // `create` is advisory like ShmRing: a zero-size fresh image is
    // ftruncate'd + initialized here; an existing live image (magic stamped)
    // is attached as-is and NEVER re-initialized, so a late joiner cannot wipe
    // live limits. Stale images are the operator's shm_unlink responsibility.
    bool open(std::string_view shm_name, bool create) noexcept;
    void close() noexcept;
    [[nodiscard]] bool is_open() const noexcept { return base_ != nullptr; }

    // --- Hot path (matching loop) --------------------------------------------

    // Mutual-headroom screen: BOTH directed edges must hold >= notional_ticks.
    // Wait-free (two relaxed loads), allocation-free, no syscall. n <= 0 or
    // either party >= kMaxParties fails closed.
    [[nodiscard]] bool can_match(uint32_t maker, uint32_t taker,
                                 int64_t notional_ticks) const noexcept {
        if (!base_ || notional_ticks <= 0 || maker >= kCreditMaxParties ||
            taker >= kCreditMaxParties)
            return false;
        const uint64_t n = static_cast<uint64_t>(notional_ticks);
        // relaxed: credit screening tolerates a stale read of a concurrently
        // updated limit — the authoritative check is try_debit's CAS, which
        // the fill path must still pass before committing (spec §13.8.3).
        return cell(maker, taker).load(std::memory_order_relaxed) >= n &&
               cell(taker, maker).load(std::memory_order_relaxed) >= n;
    }

    // Atomic bilateral debit. Both directions must succeed; a failure on the
    // second edge rolls the first back, so under races no side is debited
    // without the other. Lock-free CAS — never drives a cell negative, so
    // concurrent debits can never double-spend below zero.
    [[nodiscard]] bool try_debit(uint32_t maker, uint32_t taker,
                                 int64_t notional_ticks) noexcept {
        if (!base_ || notional_ticks <= 0 || maker >= kCreditMaxParties ||
            taker >= kCreditMaxParties)
            return false;
        const uint64_t n = static_cast<uint64_t>(notional_ticks);
        if (!debit_one(maker, taker, n))
            return false;
        if (!debit_one(taker, maker, n)) {
            // Compensate the first leg — the pair is never left half-debited.
            cell(maker, taker).fetch_add(n, std::memory_order_relaxed);
            return false;
        }
        return true;
    }

    // Consume-or-skip fused form for the match loop: a single call that the
    // caller uses as `if (consume_or_skip(...) == Consume) fill(); else skip;`
    // Semantically can_match + try_debit without the check-then-act window:
    // debit failure under a concurrent limit update yields Skip identically to
    // a failed pre-check (priority preserved — the resting order is untouched).
    [[nodiscard]] Gate consume_or_skip(uint32_t maker, uint32_t taker,
                                       int64_t notional_ticks) noexcept {
        return try_debit(maker, taker, notional_ticks) ? Gate::Consume
                                                       : Gate::Skip;
    }

    // Current remaining directed credit (diagnostics/tests; relaxed read).
    [[nodiscard]] uint64_t credit_limit(uint32_t a, uint32_t b) const noexcept {
        if (!base_ || a >= kCreditMaxParties || b >= kCreditMaxParties)
            return 0;
        return cell(a, b).load(std::memory_order_relaxed);
    }

    // --- Control plane (Go Risk writer path) ----------------------------------

    // Sets directed edge a->b to new_limit (not a delta — the Go Risk service
    // owns absolute-limit bookkeeping; the engine only debits). Out-of-range
    // parties fail closed. Bumps the shared update_seq for observability.
    bool apply_update(uint32_t party_a, uint32_t party_b,
                      uint64_t new_limit) noexcept {
        if (!base_ || party_a >= kCreditMaxParties ||
            party_b >= kCreditMaxParties)
            return false;
        cell(party_a, party_b).store(new_limit, std::memory_order_release);
        update_seq().fetch_add(1, std::memory_order_release);
        return true;
    }

    // Decode + apply seam for the Aeron control channel: one CREDIT_UPDATE
    // frame -> one directed edge store. Malformed frames are dropped
    // (decode result returned for metrics; never throws, never applies a
    // partially valid message — spec §2.7 fail-closed).
    CreditCtlDecode on_control_message(const void* buf,
                                       uint32_t len) noexcept {
        CreditUpdateMsg m;
        const CreditCtlDecode r = credit_ctl_decode(buf, len, &m);
        if (r != CreditCtlDecode::Ok)
            return r;
        if (!apply_update(m.party_a, m.party_b, m.new_limit))
            return CreditCtlDecode::PartyOutOfRange;
        return CreditCtlDecode::Ok;
    }

    // Observability: control-plane application counter + writer pid.
    [[nodiscard]] uint64_t updates_applied() const noexcept {
        return base_ ? const_update_seq().load(std::memory_order_acquire) : 0;
    }
    [[nodiscard]] uint64_t writer_pid() const noexcept {
        if (!base_)
            return 0;
        const auto* p = reinterpret_cast<const std::atomic<uint64_t>*>(
            base_ + kCreditOffWriterPid);
        return p->load(std::memory_order_acquire);
    }
    void stamp_writer_pid(uint64_t pid) noexcept {
        if (!base_)
            return;
        auto* p = reinterpret_cast<std::atomic<uint64_t>*>(base_ +
                                                         kCreditOffWriterPid);
        p->store(pid, std::memory_order_release);
    }

    // Raw base pointer for tests/benchmarks (do not expose mutators).
    [[nodiscard]] const uint8_t* base() const noexcept { return base_; }
    [[nodiscard]] uint64_t map_size() const noexcept { return map_size_; }

private:
    void move_from(BilateralCreditMatrix& o) noexcept {
        base_ = o.base_;
        map_size_ = o.map_size_;
        o.base_ = nullptr;
        o.map_size_ = 0;
    }

    std::atomic<uint64_t>& cell(uint32_t a, uint32_t b) noexcept {
        return *reinterpret_cast<std::atomic<uint64_t>*>(base_ +
                                                         credit_cell_offset(a, b));
    }
    const std::atomic<uint64_t>& cell(uint32_t a, uint32_t b) const noexcept {
        return *reinterpret_cast<const std::atomic<uint64_t>*>(
            base_ + credit_cell_offset(a, b));
    }
    std::atomic<uint64_t>& update_seq() noexcept {
        return *reinterpret_cast<std::atomic<uint64_t>*>(base_ +
                                                         kCreditOffUpdateSeq);
    }
    const std::atomic<uint64_t>& const_update_seq() const noexcept {
        return *reinterpret_cast<const std::atomic<uint64_t>*>(base_ +
                                                               kCreditOffUpdateSeq);
    }

    // Lock-free decrement of one directed cell; false when headroom < n.
    // CAS precondition cur >= n makes a negative balance unrepresentable.
    bool debit_one(uint32_t a, uint32_t b, uint64_t n) noexcept {
        auto& c = cell(a, b);
        uint64_t cur = c.load(std::memory_order_relaxed);
        while (cur >= n) {
            if (c.compare_exchange_weak(cur, cur - n,
                                        std::memory_order_acq_rel,
                                        std::memory_order_relaxed))
                return true;
        }
        return false;
    }

    uint8_t* base_ = nullptr;
    uint64_t map_size_ = 0;
};

}  // namespace exch
