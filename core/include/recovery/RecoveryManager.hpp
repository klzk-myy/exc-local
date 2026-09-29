#pragma once

// PHASE-02 TASK-2.3.4 — boot recovery: snapshot load + WAL replay + the
// fail-closed boot invariant (spec §3.5/§18.1). PHASE-04 TASKS 4.3.5/4.3.9
// extend this class with THE graduated recovery ladder (recover_ladder):
// level 1 torn-tail CRC repair (detect + truncate via Wal::open()) →
// level 2 snapshot rebase (prior-generation fallback / genesis replay /
// forward-divergence rebase marker) → level 3 fail-closed halt emitting a
// recovery_reports JSONL row + ALERT_P1.
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
// Snapshot coverage note (documented boundary): the pinned snapshot format
// restores book-visible state only — levels, resting orders, FIFO
// (timestamp_ns, ingress_seq) keys, filled/remaining quantities, tif,
// stp_mode, type, flags. Engine-private meta is NOT serialised and cannot
// be re-seeded through the public MatchingEngine API: the pending-stop
// queue, the GTD/DAY expiry heap, per-order STP trade_group_id, iceberg
// slice records, §6.9 amend fences, and cumulative prevented_qty all live
// in the engine's OrderMeta/stop/iceberg side tables and are populated
// only at live admission. Journaled EFFECTS of that meta still re-derive
// faithfully from the tail — expiry cancels, triggered-stop outcomes, and
// STP suppression cancels/modifies arrive as ordinary tail entries — so
// the book-visible end state converges whenever the tail's decisions do
// not themselves depend on lost meta. The residual divergence window is a
// snapshot that covers aux-bearing live orders followed by a tail whose
// decisions need that meta (e.g., same-group STP against a restored
// maker, or a pending stop whose trigger decision the tail expects to
// re-derive). WAL-only recovery is unaffected: every replayed ORDER_NEW
// re-registers its aux at admission. Closing the gap requires extending
// the snapshot contract (an aux side-block beside WalSnapshotOrderExt)
// plus an engine meta-restore path — outside this component's ownership.
//
// Replay capacity: ingress nodes are allocated from the binding's order
// pool (RecoveryBookBinding::orders — the pool the book itself allocates
// from when supplied) or, when omitted, a manager-retained arena sized to
// the book's max_orders() bound. Resting recovered orders are always
// book-pool clones (every engine insert path copies the template), so
// recovered books never reference replay scratch. The arena is retained
// anyway — engine-private queues (pending stops) adopt ingress nodes —
// keeping the contract safe under any present or future adoption path.
// Bound books must be empty at recovery start (fail-closed
// InvariantViolated) — replaying onto a populated book would
// double-apply the journal.

#include <cstdint>
#include <initializer_list>
#include <memory>
#include <span>
#include <string>
#include <string_view>
#include <unordered_map>
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
    // The order pool the book is bound to (the one its OrderBook allocates
    // Order nodes from). Passing it makes the replay engine's scratch and
    // the book's node draws share one capacity domain — identical to live
    // wiring, so pool-exhaustion thresholds reproduce 1:1. Ownership note:
    // every book insertion path clones the template into the book pool
    // (resting recovered orders are always book-pool owned); the replay
    // pool only hosts engine scratch — terminal paths free to it and the
    // pending-stop queue adopts nodes that die with the replay engine.
    // Sharing the book pool is therefore safe, not required — but when
    // nullptr the manager allocates an internal arena sized to
    // book->max_orders() and RETAINS it for the manager's lifetime
    // (adopted_pools_) as belt-and-suspenders insurance against any
    // engine adoption path: keep the RecoveryManager alive at least as
    // long as the recovered book is in use.
    MemoryPool<Order>* orders = nullptr;
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
    uint64_t last_valid_seq = 0;      // last CRC-valid entry seq (0 if empty)
    uint64_t covered_gap_seqs = 0;    // seqs lost but fully covered by every
                                      // bound snapshot (Task 4.3.5 rebase
                                      // semantics — see header contract)
    uint64_t mutations_applied = 0;
    uint64_t entries_replayed = 0;    // entries >= their book's snapshot_seq
    uint64_t dedup_skips = 0;
    uint64_t covered_skips = 0;
    uint64_t covered_segments_skipped = 0;  // contiguous sealed segments
        // entirely below every bound snapshot_seq — prescan either
        // CRC-verified them or pinned their seq span via the filename
        // contract — so replay attributed their entries to covered_skips
        // without walking them (boot-time optimisation; entry-path
        // semantics preserved)
    uint64_t prescan_segments_skipped = 0;  // sealed segments whose seq span
        // sits entirely below every bound book's snapshot cursor — prescan
        // verified the header and took the filename seq contract instead of
        // a full CRC walk (Phase-02.5 recovery-time finding: cold-boot CRC
        // verification of a multi-GB covered journal is disk-rate bound)
    uint64_t shard_events = 0;        // TIME_TICK/BOOK_SNAPSHOT/MARGIN_*
    uint64_t audit_events = 0;        // PREVENTED_MATCH audit records (no-op)
    uint64_t foreign_entries = 0;     // targeting instruments not bound
    uint64_t trades_derived = 0;      // engine re-derived fills (all books)
    uint64_t trades_applied = 0;      // orphan fills applied to makers
    uint64_t max_trade_id = 0;        // largest journaled trade_id seen —
                                      // engine reseeds next_trade_id from it
                                      // so post-recovery fills can't collide
                                      // with replay-deduped ids
    bool snapshot_fallback = false;   // level-2 rebase: prior-generation or
                                      // genesis snapshot path was taken
    uint64_t first_divergent_seq = UINT64_MAX;  // recovery_reports field
    uint32_t detail_instrument = 0;
    char detail[96] = {};             // static context for ops/runbooks
    std::vector<PerBookRecovery> books;

    // Phase-04 ladder vocabulary (spec §3.5): CLEAN when fully verified,
    // WAL_REPAIRED when a torn tail was truncated or snapshot-covered seqs
    // were lost, else HALTED.
    [[nodiscard]] RecoveryOutcome outcome() const noexcept {
        if (status == RecoveryStatus::Ok) {
            return (tail_truncated || covered_gap_seqs > 0)
                       ? RecoveryOutcome::WAL_REPAIRED
                       : RecoveryOutcome::CLEAN;
        }
        return RecoveryOutcome::HALTED;
    }
    [[nodiscard]] bool ok() const noexcept {
        return status == RecoveryStatus::Ok;
    }
};

// Level-2 (snapshot rebase) knobs for recover(). Strict boot recovery uses
// the defaults; recover_ladder() flips these on the second attempt.
struct RecoverOptions {
    // On a corrupt/unloadable latest snapshot: fall back to the previous
    // retained generation (ISnapshotSink::load_prior), else rebase to
    // genesis (snapshot-less replay — legal only when the WAL stream is
    // complete from seq 0; the stream_base <= snapshot_seq invariant
    // enforces that fail-closed).
    bool snapshot_fallback = false;
    // Level-2 only: a corrupt record inside a sealed (non-tail) segment is
    // tolerated when the seqs it can cover are entirely below every bound
    // book's snapshot_seq (the snapshot is provably authoritative through
    // its cursor, so the damaged bytes carried only redundant journal).
    // Strict level-1 always fails WalCorrupt on sealed-segment damage —
    // immutable bytes changed is suspect media (spec §2.7).
    bool tolerate_covered_damage = false;
};

// One row of the migration-065 recovery_reports contract, emitted as a
// single JSON line by the ladder (and persisted to PostgreSQL by the Go
// consumer — services/cmd/wal-recovery persist-reports; the C++ core never
// speaks PG directly).
struct RecoveryReport {
    uint32_t shard_id = 0;
    int64_t book_seq = -1;            // recomputed cursor; -1 = unknown
    uint64_t wal_tail = 0;
    int64_t last_valid_seq = -1;      // -1 = no valid entry
    uint64_t snapshot_seq = 0;
    int64_t first_divergent_seq = -1; // -1 = none
    const char* stage = "boot_ladder";
    const char* outcome = "CLEAN";    // CLEAN | WAL_REPAIRED |
                                      // SNAPSHOT_REBASED | WAL_RECOVERY_HALT
    std::string detail_json;          // pre-encoded JSON object fragment
};

// Result of the graduated ladder (spec §3.5/§18.1, Task 4.3.5/4.3.9 — this
// class is THE single owner of that ladder).
struct RecoveryLadderResult {
    RecoveryOutcome outcome = RecoveryOutcome::CLEAN;
    uint8_t level = 1;                // deepest level reached: 1 repair,
                                      // 2 snapshot rebase, 3 fail-closed halt
    bool books_dirty_on_fail = false; // level-1 mutated a bound book before
                                      // failing → rebase skipped (needs
                                      // empty books) → straight to halt
    bool rebase_marker_written = false;  // {snapshot_seq}.wal marker segment
    RecoveryResult result;            // the deciding attempt's result
    RecoveryReport report;            // populated for every non-clean run
    bool report_written = false;      // JSONL line appended to report_path
    std::string report_path;          // echo of the sink used
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
    // `opts` defaults to the strict level-1 contract; recover_ladder()
    // drives the level-2 rebase attempt with snapshot_fallback enabled.
    [[nodiscard]] RecoveryResult
    recover(std::string_view wal_dir,
            std::span<const RecoveryBookBinding> bindings,
            const RecoverOptions& opts) noexcept;
    [[nodiscard]] RecoveryResult
    recover(std::string_view wal_dir,
            std::span<const RecoveryBookBinding> bindings) noexcept {
        return recover(wal_dir, bindings, RecoverOptions{});
    }
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

    // === Graduated recovery ladder (Phase-04 Task 4.3.5/4.3.9, spec §3.5) ===
    // THE single owner of the ladder. Level 1 = recover() (torn-tail CRC
    // repair + replay, snapshot-covered WAL gaps tolerated). Level 2 =
    // snapshot rebase: for snapshot corruption it falls back to the prior
    // retained generation (or genesis when the stream is complete); for
    // forward divergence (snapshot_seq > wal_tail) it writes a
    // {snapshot_seq}.wal rebase-marker segment so the WAL seq space resumes
    // past snapshot coverage, then re-verifies. Level 3 = fail-closed halt:
    // outcome WAL_RECOVERY_HALT, a recovery_reports JSON line appended to
    // `report_log_path` (a Go consumer persists it — the core never speaks
    // PG), an `ALERT_P1 WAL_RECOVERY_HALT` stderr line with the runbook
    // link, and the caller enters MarketDataOnly (main.cpp).
    //
    // Bound books must be empty before the call; when level-1 fails AFTER
    // mutating a book (mid-replay apply failure), rebase is unsafe in
    // place — the ladder reports books_dirty_on_fail and halts: restart the
    // process for fresh books (documented operational path).
    [[nodiscard]] RecoveryLadderResult
    recover_ladder(std::string_view wal_dir,
                   std::span<const RecoveryBookBinding> bindings,
                   std::string_view report_log_path) noexcept;
    [[nodiscard]] RecoveryLadderResult
    recover_ladder(std::string_view wal_dir,
                   std::initializer_list<RecoveryBookBinding> bindings,
                   std::string_view report_log_path) noexcept {
        return recover_ladder(wal_dir,
                              std::span<const RecoveryBookBinding>(
                                  bindings.begin(), bindings.size()),
                              report_log_path);
    }

    // Stub-compatible surface for the Phase-04 ladder driver: outcome of the
    // most recent recover() call (CLEAN before any run).
    [[nodiscard]] RecoveryOutcome recover() noexcept { return last_outcome_; }

    [[nodiscard]] uint32_t shard_id() const noexcept { return shard_id_; }
    // Smallest snapshot_seq used in the last run (0 = replay from genesis).
    [[nodiscard]] uint64_t snapshot_seq() const noexcept { return snapshot_seq_; }
    [[nodiscard]] uint64_t wal_tail() const noexcept { return wal_tail_; }

    // --- Phase-15 recovered auction state (Tasks 15.3.6/15.3.10) ----------
    // Snapshot of the replay engine's auction/lifecycle position per bound
    // book, captured at the end of a successful recover(). main.cpp copies
    // it into the live engine via MatchingEngine::adopt_auction_state().
    // parked_head nodes are owned by the binding's pool (or the retained
    // arena) — they stay valid for the binding's lifetime; the live engine
    // frees them back to ITS pool only when binding->orders IS the live
    // pool (the main.cpp wiring; otherwise the arena keeps them alive and
    // the engine treats the list as borrowed scratch — do not adopt
    // parked nodes into a live engine bound to a different pool).
    struct RecoveredAuctionState {
        uint8_t  phase = 0xff;       // MatchingEngine::kAuctionPhase*
        int64_t  auction_id = 0;
        int64_t  deadline_ns = 0;
        uint8_t  extensions = 0;
        bool     awaiting = false;
        int64_t  last_completed = 0;
        bool     quarantined = false;
        const char* quarantine_code = nullptr;  // engine string literal
        Order*   parked_head = nullptr;
        uint32_t parked_count = 0;
    };
    // nullptr when the instrument was not bound (or no recover() ran).
    [[nodiscard]] const RecoveredAuctionState* recovered_auction_state(
        uint32_t instrument_id) const noexcept;

private:
    struct BookState;  // per-binding working set (pimpl to keep the header lean)

    // Returns false → res.status already set; caller aborts.
    // read_snapshot verifies + parses only (no book mutation);
    // restore_snapshot applies the verified image (first book mutation).
    bool read_snapshot(RecoveryResult& res, BookState& bs,
                       const RecoverOptions& opts) noexcept;
    bool restore_snapshot(RecoveryResult& res, BookState& bs) noexcept;
    void set_fail(RecoveryResult& res, RecoveryStatus s, uint64_t seq,
                  uint32_t instrument, const char* why) noexcept;

    uint32_t shard_id_;
    ISnapshotSink* snapshots_ = nullptr;
    uint64_t snapshot_seq_ = 0;
    uint64_t wal_tail_ = 0;
    RecoveryOutcome last_outcome_ = RecoveryOutcome::CLEAN;
    // Replay arenas adopted when a binding omits its order pool — see
    // RecoveryBookBinding::orders. No recovered state strictly requires
    // them (book inserts clone into the book pool; engine-private queues
    // die with the replay engine), but retaining them for the manager's
    // lifetime keeps the ownership contract safe under any adoption path.
    // Arenas accumulate across recover() calls: freeing one early could
    // still dangle books recovered in earlier calls.
    std::vector<std::unique_ptr<MemoryPool<Order>>> adopted_pools_;
    // Per-instrument auction/lifecycle state captured at the end of each
    // recover() — see RecoveredAuctionState above.
    std::unordered_map<uint32_t, RecoveredAuctionState>
        recovered_auctions_;
};

}  // namespace exch
