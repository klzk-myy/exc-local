#pragma once

// PHASE-02 TASK-2.3.4 — boot recovery: snapshot load + WAL replay + the
// fail-closed boot invariant (spec §3.5/§18.1). The graduated ladder
// orchestration (rebase → recovery_reports → MarketDataOnly halt) is Phase-04
// Task 4.3.9; this class supplies its step-1 primitive: detect + truncate a
// torn tail via Wal::open() recovery, then replay to a verified tail.
//
// Sequence model (documented choice — see SnapshotStore.hpp for the blob side):
//   * `wal_tail`  — WAL cursor domain: seq of the last valid entry + 1
//                   (== Wal::tail_seq(), the count of committed entries when
//                   the stream is dense from 0).
//   * `snapshot_seq` (a.k.a. the snapshot blob's header.book_seq) — the WAL
//                   cursor the snapshot covers: entries with seq <
//                   snapshot_seq are already reflected in the snapshot.
//                   Replay resumes at seq == snapshot_seq (== spec §3.5's
//                   "snapshot_seq + 1" in last-seq naming).
//   * recomputed book_seq — the WAL cursor the recovered book is consistent
//                   through: wal_tail after a complete replay.
//
// Boot-time invariant (spec §3.5 "book_seq == WAL tail"), enforced fail-closed
// per bound book:
//   1. snapshot_seq <= wal_tail  — a snapshot ahead of the log claims state
//      the WAL never committed (forward divergence, spec §18.5);
//   2. stream_base <= snapshot_seq — a WAL trimmed past the snapshot boundary
//      leaves entries the snapshot cannot account for (uncovered prefix);
//   3. the scanned stream is strictly seq-contiguous — a gap/dup/regression
//      means lost entries (SeqGap is reported separately, same fail-closed);
//   4. every dispatched entry applied or proven a safe dedup no-op — a
//      rejected replay means the reconstructed book diverges from the log.
// With the stream fully replayed, recomputed_book_seq == wal_tail per book;
// the equality is asserted and reported per book.
//
// Replay model (deterministic re-derivation, spec §3.5 "same input stream =>
// same book, same fills"): journaled events are driven through a journal-free
// MatchingEngine per bound book — one constructed at recovery time with
// wal == nullptr and publisher == nullptr (both sinks are null-guarded inside
// the engine; replay never re-journals and never publishes).
//   * ORDER_NEW — the Order + OrderAux (stop_price/gtd/trade_group/instrument
//     all travel in WalOrderNewPayload) is rebuilt and fed to
//     on_order_received_ex with the WAL envelope stamps (timestamp_ns =
//     entry ts, ingress_seq = entry seq — the EnginePump convention), so
//     marketable takers re-match instead of failing add_order(CROSSED).
//   * ORDER_CANCEL — on_cancel_received(order_id, account_id); idempotent
//     no-op when the order is already gone (engine-derived cancels —
//     IOC/FOK/STP/expiry remainders — re-derive inside the replayed
//     admission and their journaled records no-op here).
//   * ORDER_MODIFY — on_amend_received with the resolved payload values and
//     the entry seq as the §6.9 amend fence input (monotone in WAL order).
//     Engine-derived modifies (STP decrements) re-apply as book-level no-ops.
//   * TRADE — fills are NOT re-applied: the replayed taker's walk_match
//     already re-derived them (a cumulative per-order "journaled fill"
//     ledger decides derived-vs-orphan: expected journaled qty <= the
//     order's applied filled qty means the engine already emitted it).
//     A trade the engine could not reproduce — the aggressor's ORDER_NEW
//     sits in a snapshot-covered prefix or a trimmed segment — is applied
//     verbatim to the resting maker via apply_fill (zero missing trades).
//   * TIME_TICK — on_time_tick(tick_ns) fans out to every bound engine so
//     GTD/DAY expiry and trigger sweeps re-derive at the journaled clock.
//     Every entry's header timestamp additionally drives the same clock —
//     engine-authored entries stamp now_ns_ into the header, so this is a
//     monotone no-op on real streams and a deterministic clock on
//     tick-free journals.
//   * BOOK_SNAPSHOT / MARGIN_* — shard-level events: consumed and counted.
//   * PREVENTED_MATCH — pinned payload size + explicit audit no-op
//     (the accompanying cancel/modify entries carry the state change).
//
// Replay idempotence (spec: zero duplicate / zero missing trades):
//   * ORDER_NEW whose admission mutates nothing (duplicate id, engine-level
//     reject) counts as a dedup no-op — detected by book_seq delta;
//   * ORDER_CANCEL/ORDER_MODIFY on an unknown/consumed order id is a no-op;
//   * journaled trade_ids are dedup'd verbatim.
//
// CANCEL/MODIFY carry no instrument_id — dispatch resolves order_id → book via
// the live order map built during snapshot restore + ORDER_NEW replay.
//
// Replay capacity: each bound book gets an internal order pool sized to the
// book's max_orders() bound (pending stops + in-flight takers live there —
// the book's own pool keeps holding resting nodes). Bound books must be
// empty at recovery start (fail-closed InvariantViolated) — replaying onto a
// populated book would double-apply the journal.

#include <cstdint>
#include <initializer_list>
#include <span>
#include <string>
#include <string_view>
#include <vector>

#include "book/OrderBook.hpp"
#include "recovery/SnapshotStore.hpp"

namespace exch {

// Phase-04 vocabulary (spec §3.5 ladder outcomes) — kept from the stub.
enum class RecoveryOutcome : uint8_t {
    CLEAN,             // invariant held — resume matching
    WAL_REPAIRED,      // truncated at last CRC-valid entry
    SNAPSHOT_REBASED,  // reload + forward replay
    HALTED,            // fail-closed halt (WAL_RECOVERY_HALT state)
};

// Typed failure surface — every path returns one of these, never throws.
enum class RecoveryStatus : uint8_t {
    Ok = 0,             // replay complete; boot invariant verified
    SnapshotLoadFailed, // sink I/O error reading a snapshot
    SnapshotCorrupt,    // unparsable blob / instrument or seq mismatch
    WalOpenFailed,      // unreadable segment, bad magic/version/shard, layout
    WalCorrupt,         // corrupt record inside a sealed (non-tail) segment
    SeqGap,             // non-contiguous entry seqs — WAL data lost mid-stream
    InvariantViolated,  // snapshot ahead of tail / uncovered prefix / cursor !=
                        // wal_tail — never start with suspect state
    ApplyFailed,        // a logged mutation was rejected by the book
    Io,                 // other syscall failure (errno surfaced via detail)
};

[[nodiscard]] const char* recovery_status_str(RecoveryStatus s) noexcept;

struct RecoveryBookBinding {
    uint32_t instrument_id;
    OrderBook* book;   // caller-owned; recovered in place
};

struct PerBookRecovery {
    uint32_t instrument_id = 0;
    bool snapshot_loaded = false;
    uint64_t snapshot_seq = 0;        // WAL cursor covered by the snapshot
    uint64_t snapshot_book_seq = 0;   // header.book_seq as stored
    uint64_t orders_restored = 0;     // orders re-inserted from snapshot
    uint64_t entries_consumed = 0;    // entries >= snapshot_seq for this book
    uint64_t mutations_applied = 0;   // real book mutations during replay
    uint64_t dedup_skips = 0;         // idempotent no-ops
    uint64_t covered_skips = 0;       // entries < snapshot_seq (snapshot-covered)
    uint64_t trades_derived = 0;      // journaled fills re-derived by the
                                      // replay engine (not re-applied)
    uint64_t trades_applied = 0;      // orphan fills applied verbatim to a
                                      // resting maker (aggressor's ORDER_NEW
                                      // outside the scanned stream)
    uint64_t engine_trades_emitted = 0; // fills the replay engine emitted —
                                        // == trades_derived on a faithful
                                        // replay (verification surface)
    uint64_t pending_stops = 0;       // stop orders still pending post-replay
    uint64_t recomputed_book_seq = 0; // WAL cursor reached == wal_tail on Ok
    bool book_seq_verified = false;   // recomputed_book_seq == wal_tail held
};

struct RecoveryResult {
    RecoveryStatus status = RecoveryStatus::Ok;
    uint64_t wal_tail = 0;            // last valid seq + 1; 0 = empty stream
    uint64_t stream_base = 0;         // seq of first scanned entry (0 if empty)
    uint64_t wal_entries = 0;         // valid entries scanned
    uint32_t segments_scanned = 0;
    bool tail_truncated = false;      // torn tail truncated at valid_end
    uint64_t truncate_offset = 0;     // byte offset of the truncation point
    uint64_t mutations_applied = 0;
    uint64_t entries_replayed = 0;    // entries >= their book's snapshot_seq
    uint64_t dedup_skips = 0;
    uint64_t covered_skips = 0;
    uint64_t shard_events = 0;        // TIME_TICK/BOOK_SNAPSHOT/MARGIN_*
    uint64_t audit_events = 0;        // PREVENTED_MATCH audit records (no-op)
    uint64_t foreign_entries = 0;     // targeting instruments not bound
    uint64_t trades_derived = 0;      // engine re-derived fills (all books)
    uint64_t trades_applied = 0;      // orphan fills applied to makers
    uint64_t first_divergent_seq = UINT64_MAX;  // recovery_reports field
    uint32_t detail_instrument = 0;
    char detail[96] = {};             // static context for ops/runbooks
    std::vector<PerBookRecovery> books;

    // Phase-04 ladder vocabulary (spec §3.5): CLEAN when fully verified,
    // WAL_REPAIRED when a torn tail was truncated, else HALTED.
    [[nodiscard]] RecoveryOutcome outcome() const noexcept {
        if (status == RecoveryStatus::Ok) {
            return tail_truncated ? RecoveryOutcome::WAL_REPAIRED
                                  : RecoveryOutcome::CLEAN;
        }
        return RecoveryOutcome::HALTED;
    }
    [[nodiscard]] bool ok() const noexcept {
        return status == RecoveryStatus::Ok;
    }
};

class RecoveryManager {
public:
    explicit RecoveryManager(uint32_t shard_id) noexcept;
    // snapshots may be omitted (nullptr behavior via the shard_id-only ctor) —
    // recovery then replays the WAL onto empty books (WAL-only boot).
    RecoveryManager(uint32_t shard_id, ISnapshotSink& snapshots) noexcept;

    // Boot path. `wal_dir` is the shard segment directory (the
    // wal/{shard}/{seq_base}.wal layout). Each binding's book is loaded from
    // its latest snapshot then the WAL is replayed across all bindings.
    // noexcept: every failure is a typed RecoveryStatus in the result.
    [[nodiscard]] RecoveryResult
    recover(std::string_view wal_dir,
            std::span<const RecoveryBookBinding> bindings) noexcept;
    [[nodiscard]] RecoveryResult
    recover(std::string_view wal_dir,
            std::initializer_list<RecoveryBookBinding> bindings) noexcept {
        return recover(wal_dir,
                       std::span<const RecoveryBookBinding>(bindings.begin(),
                                                            bindings.size()));
    }
    [[nodiscard]] RecoveryResult recover(std::string_view wal_dir) noexcept {
        return recover(wal_dir, std::span<const RecoveryBookBinding>{});
    }

    // Stub-compatible surface for the Phase-04 ladder driver: outcome of the
    // most recent recover() call (CLEAN before any run).
    [[nodiscard]] RecoveryOutcome recover() noexcept { return last_outcome_; }

    [[nodiscard]] uint32_t shard_id() const noexcept { return shard_id_; }
    // Smallest snapshot_seq used in the last run (0 = replay from genesis).
    [[nodiscard]] uint64_t snapshot_seq() const noexcept { return snapshot_seq_; }
    [[nodiscard]] uint64_t wal_tail() const noexcept { return wal_tail_; }

private:
    struct BookState;  // per-binding working set (pimpl to keep the header lean)

    // Returns false → res.status already set; caller aborts.
    bool load_snapshot(RecoveryResult& res, BookState& bs) noexcept;
    void set_fail(RecoveryResult& res, RecoveryStatus s, uint64_t seq,
                  uint32_t instrument, const char* why) noexcept;

    uint32_t shard_id_;
    ISnapshotSink* snapshots_ = nullptr;
    uint64_t snapshot_seq_ = 0;
    uint64_t wal_tail_ = 0;
    RecoveryOutcome last_outcome_ = RecoveryOutcome::CLEAN;
};

}  // namespace exch
