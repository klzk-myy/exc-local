// PHASE-02 TASK-2.3.4 — snapshot load + WAL replay + fail-closed boot
// invariant (spec §3.5/§18.1). See include/recovery/RecoveryManager.hpp for
// the sequence model and invariant contract.

#include "recovery/RecoveryManager.hpp"

#include <fcntl.h>
#include <unistd.h>

#include <algorithm>
#include <atomic>
#include <cerrno>
#include <cinttypes>
#include <cstdio>
#include <cstdlib>
#include <cstring>
#include <filesystem>
#include <memory>
#include <system_error>
#include <thread>
#include <unordered_map>

#include "matching/MatchingEngine.hpp"
#include "recovery/DenseLedger.hpp"
#include "utils/TimeUtils.hpp"
#include "wal/Wal.hpp"

namespace exch {

const RecoveryManager::RecoveredAuctionState*
RecoveryManager::recovered_auction_state(uint32_t instrument_id)
    const noexcept {
    const auto it = recovered_auctions_.find(instrument_id);
    return it == recovered_auctions_.end() ? nullptr : &it->second;
}

SnapshotAuxState* RecoveryManager::recovered_aux_state(
    uint32_t instrument_id) noexcept {
    const auto it = recovered_aux_.find(instrument_id);
    return it == recovered_aux_.end() ? nullptr : &it->second;
}

const char* recovery_status_str(RecoveryStatus s) noexcept {
    switch (s) {
        case RecoveryStatus::Ok:                return "Ok";
        case RecoveryStatus::SnapshotLoadFailed:return "SnapshotLoadFailed";
        case RecoveryStatus::SnapshotCorrupt:   return "SnapshotCorrupt";
        case RecoveryStatus::WalOpenFailed:     return "WalOpenFailed";
        case RecoveryStatus::WalCorrupt:        return "WalCorrupt";
        case RecoveryStatus::SeqGap:            return "SeqGap";
        case RecoveryStatus::InvariantViolated: return "InvariantViolated";
        case RecoveryStatus::ApplyFailed:       return "ApplyFailed";
        case RecoveryStatus::Io:                return "Io";
    }
    return "?";
}

// Per-binding working set.
struct RecoveryManager::BookState {
    uint32_t instrument_id = 0;
    OrderBook* book = nullptr;
    uint64_t snapshot_seq = 0;      // covered cursor (0 = genesis/no snapshot)
    uint64_t snapshot_book_seq = 0; // WalBookSnapshotHeader.book_seq as stored
    bool snapshot_loaded = false;
    // Parsed snapshot held between the read phase (Phase 2, no mutation) and
    // the restore phase (Phase 4) — splitting them keeps bound books clean
    // across every pre-replay failure so the ladder's level-2 rebase can run
    // in place.
    ParsedSnapshot parsed;
    bool snapshot_have = false;     // a verified snapshot blob was read
    uint8_t snapshot_gen = 0;       // 0 = latest, 1 = prior-generation fallback
    uint64_t orders_restored = 0;
    uint64_t entries_consumed = 0;  // seq >= snapshot_seq dispatched here
    uint64_t mutations_applied = 0;
    uint64_t dedup_skips = 0;
    uint64_t covered_skips = 0;
    uint64_t trades_derived = 0;    // journaled fills the engine re-derived
    uint64_t trades_applied = 0;    // orphan fills applied to the maker
    uint64_t cursor = 0;            // WAL cursor this book's state reaches
    PerBookRecovery report;

    // Replay machinery (journal-free engine: wal/publisher both nullptr).
    // replay_pool is BORROWED — it is either the binding's own order pool
    // (RecoveryBookBinding::orders — one capacity domain shared with the
    // book, identical to live wiring) or an arena owned by
    // RecoveryManager::adopted_pools_ that outlives this BookState. Book
    // insertions clone into the book's own pool, so recovered books never
    // reference this arena; retention is insurance for engine-private
    // adoption (the pending-stop queue holds ingress nodes).
    MemoryPool<Order>* replay_pool = nullptr;
    std::unique_ptr<MatchingEngine> engine;
    // Cumulative journaled fill qty per order id — the derived-vs-orphan
    // ledger for TRADE replay (seeded with filled_qty_units of
    // snapshot-restored orders; accrued on both legs of every consumed
    // journaled trade). DenseLedger: order ids are BIGSERIAL-dense, so a
    // chunked index replaces ~50ns-per-node unordered_map inserts with
    // ~4ns direct writes (Phase-02.5: the replay ledgers dominated the
    // 19.9s restart-to-ready vs the <10s AC).
    recovery::DenseLedger<int64_t> journaled_fills;
};

namespace {

struct SegmentInfo {
    std::string path;
    bool has_entries = false;
    uint64_t first_seq = 0;
    uint64_t last_seq = 0;
    uint64_t entries = 0;
    bool corrupt = false;
    uint64_t valid_end = sizeof(WalFileHeader);
    // Numeric filename stem — rotations and rebase markers are named
    // {base_seq}.wal. UINT64_MAX when the stem isn't a seq literal.
    uint64_t name_seq = UINT64_MAX;
    uint64_t file_size = 0;
    // Set when the seq span came from the filename contract (snapshot-covered
    // segment skipped by bounded prescan) rather than a CRC-verified walk.
    bool nominal = false;
};

// Scan one segment read-only. Returns false on unreadable/bad-header/foreign
// shard — the caller maps that to WalOpenFailed.
bool scan_segment(const std::string& path, uint16_t shard_id,
                  SegmentInfo& info) noexcept {
    WalReader r;
    if (r.open(path) != WalStatus::Ok) return false;
    if (r.magic() != kWalMagic || r.version() != kWalVersion ||
        r.shard_id() != shard_id) {
        return false;
    }
    info.path = path;
    WalEntryView ev;
    for (;;) {
        const WalScanStep s = r.next(ev);
        if (s == WalScanStep::Entry) {
            if (!info.has_entries) {
                info.has_entries = true;
                info.first_seq = ev.seq;
            }
            info.last_seq = ev.seq;
            ++info.entries;
        } else {
            if (s == WalScanStep::Corrupt) {
                info.corrupt = true;
                info.valid_end = r.offset();  // truncate target per contract
            }
            break;  // End or Corrupt
        }
    }
    return true;
}

// Header-only check for snapshot-covered segments the bounded prescan skips:
// verifies the file is a WAL segment of this shard without walking entries
// (mmap touches the header page only). Returns false on the same conditions
// scan_segment would — unreadable/bad-header/foreign shard.
bool check_segment_header(const std::string& path, uint16_t shard_id) {
    WalReader r;
    if (r.open(path) != WalStatus::Ok) return false;
    return r.magic() == kWalMagic && r.version() == kWalVersion &&
           r.shard_id() == shard_id;
}

// Fixed expected payload sizes per event type (pinned contract); 0 = accept
// any length (shard-level payloads we don't consume).
uint32_t expected_payload_len(WalEventType t) noexcept {
    switch (t) {
        case WalEventType::ORDER_NEW:      return sizeof(WalOrderNewPayload);
        case WalEventType::ORDER_CANCEL:   return sizeof(WalOrderCancelPayload);
        case WalEventType::ORDER_MODIFY:   return sizeof(WalOrderModifyPayload);
        case WalEventType::TRADE:          return sizeof(WalTradePayload);
        case WalEventType::TIME_TICK:      return sizeof(WalTimeTickPayload);
        case WalEventType::PREVENTED_MATCH:return sizeof(WalPreventedMatchPayload);
        case WalEventType::OCO_LINK:      return sizeof(WalOcoLinkPayload);
        case WalEventType::AUCTION_PHASE: return sizeof(WalAuctionPhasePayload);
        // Phase-16 (Tasks 16.3.3/11/15/17): extended admission, conditional
        // activation audit and peg reprice rows.
        case WalEventType::ORDER_NEW_EX:  return sizeof(WalOrderNewExPayload);
        case WalEventType::ORDER_TRIGGERED: return sizeof(WalOrderTriggeredPayload);
        case WalEventType::PEG_REPRICE:   return sizeof(WalPegRepricePayload);
        default:                           return 0;  // BOOK_SNAPSHOT/MARGIN_*
                                                    // /BASKET_*/OPT_* — the
                                                    // coordinators replay
                                                    // their own rows.
    }
}

bool is_shard_event(WalEventType t) noexcept {
    return t == WalEventType::TIME_TICK ||
           t == WalEventType::BOOK_SNAPSHOT ||
           t == WalEventType::MARGIN_RESERVE ||
           t == WalEventType::MARGIN_RELEASE ||
           // Phase-3 Task 4: cross-shard coordinator rows are shard-level
           // bookkeeping — replayed by the coordinator's own recover(), never
           // routed through instrument books.
           t == WalEventType::BASKET_BEGIN ||
           t == WalEventType::BASKET_RESERVE ||
           t == WalEventType::BASKET_LEG_DONE ||
           t == WalEventType::BASKET_OUTCOME ||
           t == WalEventType::OPT_BEGIN ||
           t == WalEventType::OPT_FILL ||
           t == WalEventType::OPT_UNWIND ||
           t == WalEventType::OPT_OUTCOME;
}

}  // namespace

RecoveryManager::RecoveryManager(uint32_t shard_id) noexcept
    : shard_id_(shard_id) {}

RecoveryManager::RecoveryManager(uint32_t shard_id,
                                 ISnapshotSink& snapshots) noexcept
    : shard_id_(shard_id), snapshots_(&snapshots) {}

void RecoveryManager::set_fail(RecoveryResult& res, RecoveryStatus s,
                               uint64_t seq, uint32_t instrument,
                               const char* why) noexcept {
    if (res.status == RecoveryStatus::Ok) {
        res.status = s;
        res.first_divergent_seq = seq;
        res.detail_instrument = instrument;
        std::snprintf(res.detail, sizeof(res.detail), "%s", why);
    }
}

// Snapshot read phase (Phase 2): fetch + integrity-verify + structurally
// parse the snapshot blob. NO book mutation — bs.parsed carries the verified
// image until restore_snapshot() applies it. With opts.snapshot_fallback
// (ladder level 2), a corrupt/unloadable latest generation falls back to the
// prior retained generation, then to genesis (snapshot-less replay — the
// stream_base <= snapshot_seq invariant enforces the WAL must be complete).
bool RecoveryManager::read_snapshot(RecoveryResult& res, BookState& bs,
                                    const RecoverOptions& opts) noexcept {
    try {
        if (snapshots_ == nullptr) return true;  // WAL-only boot

        // Generation chain: 0 = latest, 1 = prior (load_prior). The strict
        // level-1 path tries only the latest; any failure there is typed and
        // returned immediately.
        for (uint8_t gen = 0; gen <= 1; ++gen) {
            uint64_t seq = 0;
            bool has = false;
            std::vector<uint8_t> blob;
            const bool loaded =
                (gen == 0)
                    ? snapshots_->load_latest(bs.instrument_id, &seq, &blob,
                                              &has)
                    : snapshots_->load_prior(bs.instrument_id, &seq, &blob,
                                             &has);
            if (!loaded) {
                if (gen == 0 && opts.snapshot_fallback) continue;  // try prior
                if (gen == 1) {
                    // Prior generation also unusable — degrade to genesis
                    // replay (the stream_base invariant still enforces that
                    // the WAL must be complete from seq 0).
                    res.snapshot_fallback = true;
                    return true;
                }
                set_fail(res, RecoveryStatus::SnapshotLoadFailed, UINT64_MAX,
                         bs.instrument_id, "snapshot sink load failed");
                return false;
            }
            if (!has) {
                // Clean cold start (gen 0) or no retained prior (gen 1 —
                // genesis replay under the same completeness invariant).
                if (gen == 1) res.snapshot_fallback = true;
                return true;
            }

            ParsedSnapshot parsed;
            if (!SnapshotStore::parse_book(blob.data(), blob.size(), parsed) ||
                parsed.header.instrument_id != bs.instrument_id ||
                parsed.header.book_seq != seq) {
                if (gen == 0 && opts.snapshot_fallback) continue;
                if (gen == 1) {  // prior also corrupt → genesis replay
                    res.snapshot_fallback = true;
                    return true;
                }
                set_fail(res, RecoveryStatus::SnapshotCorrupt, seq,
                         bs.instrument_id,
                         "snapshot blob failed integrity/structural checks");
                return false;
            }
            bs.parsed = std::move(parsed);
            // v2 counters trailer: snapshot-covered trades never re-enter the
            // replay loop, so the WAL tail scan alone would restart the
            // allocator at 1 and re-issue journaled ids on the next fill.
            // The captured counter is authoritative for every seq the
            // snapshot covers (Phase-09 swap-drill finding).
            if (bs.parsed.next_trade_id > 1 &&
                bs.parsed.next_trade_id - 1 > res.max_trade_id) {
                res.max_trade_id = bs.parsed.next_trade_id - 1;
            }
            bs.snapshot_have = true;
            bs.snapshot_seq = seq;
            bs.snapshot_book_seq = bs.parsed.header.book_seq;
            bs.snapshot_gen = gen;
            if (gen != 0) res.snapshot_fallback = true;
            return true;
        }
        return true;
    } catch (...) {
        set_fail(res, RecoveryStatus::Io, UINT64_MAX, bs.instrument_id,
                 "unexpected allocation failure during snapshot read");
        return false;
    }
}

// Snapshot restore phase (Phase 4 — first book mutation): apply the verified
// bs.parsed image by re-insertion. Never called unless every pre-replay
// check has already passed, so bound books stay empty through all prescan
// failures (the ladder's level-2 rebase requires empty books).
bool RecoveryManager::restore_snapshot(RecoveryResult& res,
                                       BookState& bs) noexcept {
    try {
        if (!bs.snapshot_have) return true;  // genesis replay

        if (bs.book->live_orders() != 0) {
            set_fail(res, RecoveryStatus::InvariantViolated, bs.snapshot_seq,
                     bs.instrument_id,
                     "bound book not empty at recovery start");
            return false;
        }

        // Rebuild levels+orders by re-insertion: add_order() tail-appends into
        // each level, so replaying the level-major FIFO stream order restores
        // price-time priority exactly.
        for (const auto& po : bs.parsed.orders) {
            Order* out = nullptr;
            const BookError e = bs.book->add_order(po.tmpl, &out);
            if (e != BookError::OK || out == nullptr) {
                set_fail(res, RecoveryStatus::SnapshotCorrupt, bs.snapshot_seq,
                         bs.instrument_id,
                         "snapshot order rejected by book on restore");
                return false;
            }
            ++bs.orders_restored;
        }
        const char* violation = nullptr;
        if (!bs.book->validate(&violation)) {
            set_fail(res, RecoveryStatus::SnapshotCorrupt, bs.snapshot_seq,
                     bs.instrument_id,
                     violation != nullptr ? violation
                                          : "restored book failed audit");
            return false;
        }
        // Rebase the mutation counter to the covered WAL cursor: the cursor
        // strictly dominates the pre-snapshot mutation count (TIME_TICK and
        // non-mutation rows inflate it), so post-restart fills/L3 events can
        // never re-issue a prior generation's book_seq values (Phase-09
        // swap-drill finding: fills collided across generations).
        bs.book->set_book_seq(bs.snapshot_book_seq);
        bs.snapshot_loaded = true;
        bs.cursor = bs.snapshot_seq;
        return true;
    } catch (...) {
        set_fail(res, RecoveryStatus::Io, UINT64_MAX, bs.instrument_id,
                 "unexpected allocation failure during snapshot restore");
        return false;
    }
}



RecoveryResult RecoveryManager::recover(
    std::string_view wal_dir,
    std::span<const RecoveryBookBinding> bindings,
    const RecoverOptions& opts) noexcept {
    RecoveryResult res;
    recovered_auctions_.clear();  // stale state from a prior run must never
                                  // be adopted by a later caller
    recovered_aux_.clear();       // same contract for drained aux state
    try {
        std::vector<BookState> states(bindings.size());
        std::unordered_map<uint32_t, BookState*> by_instrument;
        // order_owner: order id -> states[] index + 1 (0 = unseen). The
        // "seen-ever" ledger for CANCEL/MODIFY routing and trade-leg
        // classification — journaled order ids stay set even after the
        // order is consumed, which is what separates an engine-derived
        // fill from an orphan row.
        recovery::DenseLedger<uint32_t> order_owner;
        recovery::DenseSet applied_trades;

        for (std::size_t i = 0; i < bindings.size(); ++i) {
            states[i].instrument_id = bindings[i].instrument_id;
            states[i].book = bindings[i].book;
            states[i].replay_pool = bindings[i].orders;
            states[i].report.instrument_id = bindings[i].instrument_id;
            if (bindings[i].book == nullptr) {
                set_fail(res, RecoveryStatus::InvariantViolated, UINT64_MAX,
                         bindings[i].instrument_id, "null book binding");
                wal_tail_ = res.wal_tail;
                last_outcome_ = res.outcome();
                return res;
            }
            // Engine-driven replay must start from an empty book: replaying
            // the journal onto populated state would double-apply every
            // mutation (a taker ORDER_NEW would re-execute its fills).
            if (bindings[i].book->live_orders() != 0) {
                set_fail(res, RecoveryStatus::InvariantViolated, UINT64_MAX,
                         bindings[i].instrument_id,
                         "bound book not empty at recovery start");
                wal_tail_ = res.wal_tail;
                last_outcome_ = res.outcome();
                return res;
            }
            if (by_instrument.count(bindings[i].instrument_id) != 0) {
                set_fail(res, RecoveryStatus::InvariantViolated, UINT64_MAX,
                         bindings[i].instrument_id, "duplicate binding");
                wal_tail_ = res.wal_tail;
                last_outcome_ = res.outcome();
                return res;
            }
            by_instrument[bindings[i].instrument_id] = &states[i];
        }

        // Monotone stamp clamps for replayed mutations — seeded with the
        // newest restored (timestamp_ns, ingress_seq) key so a replayed
        // order inserting into a level that holds snapshot-restored orders
        // keeps the (ts, seq) chain strictly ordered (level_chain_consistent
        // audit + FIFO fidelity). Restored orders keep their ext timestamps
        // verbatim; replayed ones get max(wal_ts, running max) /
        // max(wal_seq, running max).
        uint64_t max_ts = 0;
        uint64_t max_seq = 0;

        // --- Phase 1a: WAL enumeration (filenames only) ---------------------
        // Runs BEFORE any book mutation so that every prescan/snapshot
        // failure leaves the bound books clean — the recover_ladder level-2
        // rebase can then re-run recover() in place. Filenames carry the
        // segment's base seq ({base_seq}.wal for rotations and rebase
        // markers) — enough to order the stream and, once the verified
        // snapshot bound is known, to decide which sealed segments a CRC
        // walk can skip.
        std::vector<SegmentInfo> segs;
        {
            std::error_code ec;
            const std::filesystem::path dir(wal_dir);
            if (std::filesystem::exists(dir, ec) && !ec) {
                for (const auto& de :
                     std::filesystem::directory_iterator(dir, ec)) {
                    if (ec) break;
                    std::error_code tec;
                    if (!de.is_regular_file(tec) || tec) continue;
                    if (de.path().extension() != ".wal") continue;
                    SegmentInfo si;
                    si.path = de.path().string();
                    std::error_code sec;
                    si.file_size = de.file_size(sec);
                    if (sec) si.file_size = 0;
                    const std::string stem = de.path().stem().string();
                    if (!stem.empty() &&
                        std::all_of(stem.begin(), stem.end(), [](char c) {
                            return c >= '0' && c <= '9';
                        })) {
                        si.name_seq =
                            std::strtoull(stem.c_str(), nullptr, 10);
                    }
                    segs.push_back(std::move(si));
                }
            }
        }
        // Name order = stream order for rotations (a sealed segment holds
        // entries strictly below the next segment's base). Non-numeric stems
        // (UINT64_MAX) sort last and are always fully scanned.
        std::sort(segs.begin(), segs.end(), [](const SegmentInfo& a,
                                               const SegmentInfo& b) {
            if (a.name_seq != b.name_seq) return a.name_seq < b.name_seq;
            return a.path < b.path;
        });

        // --- Phase 2 (pre-scan): snapshot read — verify + parse, NO mutation -
        // Runs before the entry walk so the verified coverage bound can gate
        // which sealed segments need a CRC scan at all (Phase-02.5 finding:
        // walking a multi-GB covered journal is disk-rate bound — ~34s cold
        // for 15GB at ~440MB/s — while the entries it proves can never be
        // applied: snapshot coverage already pins their seq domain).
        for (auto& bs : states) {
            if (!read_snapshot(res, bs, opts)) {
                res.books.push_back(bs.report);
                wal_tail_ = res.wal_tail;
                last_outcome_ = res.outcome();
                return res;
            }
        }

        // Coverage bound: seqs below min_snapshot are inside EVERY bound
        // book's snapshot image. A bound book without a snapshot contributes
        // 0 — it needs genesis replay, so nothing is covered for it (this
        // also guards the covered-segment replay skip below: a segment must
        // never be skipped while any bound book still needs its entries).
        uint64_t min_snapshot = UINT64_MAX;
        for (auto& bs : states) {
            const uint64_t cov = bs.snapshot_have ? bs.snapshot_seq : 0;
            if (cov < min_snapshot) min_snapshot = cov;
        }
        if (min_snapshot == UINT64_MAX) min_snapshot = 0;  // no snapshots

        // --- Phase 1b: bounded CRC prescan ----------------------------------
        // A sealed segment is fully covered when the NEXT segment's base seq
        // is <= min_snapshot: every entry it can contain sits below the
        // coverage cursor, so the prescan takes its nominal seq span from the
        // filename contract instead of CRC-walking bytes (a header check
        // still proves magic/version/shard). The last segment, every segment
        // overlapping the coverage boundary, and any segment whose span
        // cannot be bounded by a numeric successor are always scanned — tail
        // detection, torn-tail repair, divergence and uncovered-gap checks
        // keep full fidelity. Trade-off (documented, spec §27): bitrot inside
        // a covered sealed segment no longer gates strict boot — those bytes
        // are unreachable through any bound cursor; the offline wal_audit
        // sweep remains the covered-journal integrity check.
        std::vector<std::size_t> scan_idx;
        for (std::size_t i = 0; i < segs.size(); ++i) {
            SegmentInfo& si = segs[i];
            const bool covered =
                si.name_seq != UINT64_MAX && i + 1 < segs.size() &&
                segs[i + 1].name_seq != UINT64_MAX &&
                segs[i + 1].name_seq > si.name_seq &&
                segs[i + 1].name_seq <= min_snapshot;
            if (!covered) {
                scan_idx.push_back(i);
                continue;
            }
            // Header-verify the skipped segment: a foreign/unreadable file
            // still fails closed even when its span is snapshot-covered.
            if (!check_segment_header(si.path,
                                      static_cast<uint16_t>(shard_id_))) {
                set_fail(res, RecoveryStatus::WalOpenFailed, UINT64_MAX, 0,
                         "segment unreadable/bad header/foreign shard");
                wal_tail_ = res.wal_tail;
                last_outcome_ = res.outcome();
                return res;
            }
            si.nominal = true;
            if (si.file_size > sizeof(WalFileHeader)) {
                si.has_entries = true;
                si.first_seq = si.name_seq;
                si.last_seq = segs[i + 1].name_seq - 1;
                si.entries = si.last_seq - si.first_seq + 1;
            }
            ++res.prescan_segments_skipped;
        }

        // Segments are independent mmapped files — scan them in parallel
        // (bounded worker pool) so a long uncovered tail doesn't dominate
        // boot time. Each worker writes only its own SegmentInfo slot; any
        // failure fails the whole prescan after the workers drain.
        {
            const std::size_t nscan = scan_idx.size();
            const unsigned hw = std::thread::hardware_concurrency();
            const std::size_t nthreads =
                std::min<std::size_t>(
                    {nscan, 8, hw == 0 ? 1 : static_cast<std::size_t>(hw)});
            if (nthreads <= 1) {
                for (const std::size_t i : scan_idx) {
                    if (!scan_segment(segs[i].path,
                                      static_cast<uint16_t>(shard_id_),
                                      segs[i])) {
                        set_fail(res, RecoveryStatus::WalOpenFailed,
                                 UINT64_MAX, 0,
                                 "segment unreadable/bad header/foreign "
                                 "shard");
                        wal_tail_ = res.wal_tail;
                        last_outcome_ = res.outcome();
                        return res;
                    }
                }
            } else {
                std::atomic<std::size_t> next{0};
                std::atomic<bool> failed{false};
                std::vector<std::thread> workers;
                workers.reserve(nthreads);
                const uint16_t sid = static_cast<uint16_t>(shard_id_);
                for (std::size_t t = 0; t < nthreads; ++t) {
                    workers.emplace_back([&]() noexcept {
                        for (;;) {
                            const std::size_t i = next.fetch_add(
                                1, std::memory_order_relaxed);
                            if (i >= nscan) return;
                            if (!scan_segment(segs[scan_idx[i]].path, sid,
                                              segs[scan_idx[i]])) {
                                failed.store(true,
                                             std::memory_order_release);
                            }
                        }
                    });
                }
                for (auto& w : workers) w.join();
                if (failed.load(std::memory_order_acquire)) {
                    set_fail(res, RecoveryStatus::WalOpenFailed,
                             UINT64_MAX, 0,
                             "segment unreadable/bad header/foreign shard");
                    wal_tail_ = res.wal_tail;
                    last_outcome_ = res.outcome();
                    return res;
                }
            }
        }
        // Order segments by first entry seq (empties carry no seqs — sort
        // last; nominal covered segments carry their filename base).
        std::sort(segs.begin(), segs.end(), [](const SegmentInfo& a,
                                               const SegmentInfo& b) {
            if (a.has_entries != b.has_entries) return a.has_entries;
            if (a.has_entries && b.has_entries && a.first_seq != b.first_seq)
                return a.first_seq < b.first_seq;
            return a.path < b.path;
        });

        // --- Phase 1b: torn-tail repair on the tail segment ------------------
        // The tail = non-empty segment with the greatest seq span (last in the
        // sorted order). wal_scan already detected Corrupt; Wal::open()
        // performs the detect+truncate recovery (ladder step 1, spec §3.5) —
        // mmap-mode recovery cuts the file exactly at valid_end. No appends
        // are ever issued by this path.
        std::size_t tail_idx = segs.size();
        for (std::size_t i = segs.size(); i-- > 0;) {
            if (segs[i].has_entries) { tail_idx = i; break; }
        }
        if (tail_idx < segs.size() && segs[tail_idx].corrupt) {
            SegmentInfo& tail = segs[tail_idx];
            Wal w(tail.path, static_cast<uint16_t>(shard_id_));
            if (w.open() != WalStatus::Ok) {
                set_fail(res, RecoveryStatus::WalOpenFailed, UINT64_MAX, 0,
                         "tail segment recovery open failed");
                wal_tail_ = res.wal_tail;
                last_outcome_ = res.outcome();
                return res;
            }
            res.tail_truncated = true;
            res.truncate_offset = w.last_scan().valid_end;
            tail.corrupt = false;
            tail.entries = w.last_scan().entries;
            if (w.last_scan().entries > 0) tail.last_seq = w.last_scan().last_seq;
            tail.valid_end = w.last_scan().valid_end;
            w.close();
        }

        // --- Phase 1c: stream accounting + lost ranges (still pre-mutation) --
        // A lost range [begin, end) is a span of seqs no readable segment
        // can provide: a plain gap (segment trimmed/missing) or the
        // unreadable tail of a corrupt sealed segment. Coverage is decided
        // in Phase 3 once snapshot_seq is known — a range is recoverable
        // without operator action iff end <= every bound book's
        // snapshot_seq (the snapshot is authoritative through its cursor).
        struct LostRange { uint64_t begin, end; bool corrupt_caused; };
        std::vector<LostRange> lost_ranges;
        res.stream_base = 0;
        bool prescan_started = false;
        uint64_t prescan_tail = 0;
        for (std::size_t i = 0; i < segs.size(); ++i) {
            const SegmentInfo& si = segs[i];
            if (!si.has_entries) continue;
            if (!prescan_started) {
                prescan_started = true;
                res.stream_base = si.first_seq;
                prescan_tail = si.first_seq;
            }
            if (si.first_seq > prescan_tail) {
                // Plain gap OR the lost tail of a corrupt predecessor.
                bool corrupt_caused = false;
                // The previous readable segment is index-wise earlier; find
                // whether damage at its end caused this gap.
                for (std::size_t j = i; j-- > 0;) {
                    if (segs[j].has_entries) {
                        corrupt_caused = segs[j].corrupt;
                        break;
                    }
                }
                lost_ranges.push_back(
                    LostRange{prescan_tail, si.first_seq, corrupt_caused});
            } else if (si.first_seq < prescan_tail) {
                // Overlapping/duplicate seq ranges across segments —
                // divergent journal copies; never a legal shape.
                set_fail(res, RecoveryStatus::SeqGap, si.first_seq, 0,
                         "WAL seq regression — overlapping segments");
                wal_tail_ = res.wal_tail;
                last_outcome_ = res.outcome();
                return res;
            }
            prescan_tail = si.last_seq + 1;
            res.wal_entries += si.entries;
        }
        res.wal_tail = prescan_started ? prescan_tail : 0;
        res.last_valid_seq = prescan_started ? prescan_tail - 1 : 0;

        // Non-tail corruption (sealed segment damage): the entries between
        // the corrupt record and that segment's end are unverifiable. The
        // lost range above already records the seq span; sealed damage is
        // additionally fail-closed in strict mode even when the range is
        // empty — immutable bytes changed is suspect media (spec §2.7). The
        // level-2 rebase (opts.tolerate_covered_damage) may tolerate it only
        // when the lost range is fully snapshot-covered (Phase 3).
        bool sealed_damage = false;
        for (std::size_t i = 0; i < segs.size(); ++i) {
            if (i != tail_idx && segs[i].corrupt) sealed_damage = true;
        }

        // --- Phase 3: coverage + divergence verification (books still clean) -
        // min_snapshot was computed ahead of the prescan (pre-scan Phase 2):
        // the bound every lost range must satisfy — a seq is covered only if
        // below EVERY bound book's snapshot cursor (books without a snapshot
        // contribute 0).
        for (const LostRange& lr : lost_ranges) {
            if (lr.end <= min_snapshot) {
                res.covered_gap_seqs += lr.end - lr.begin;
                continue;  // snapshot-covered loss — replay skips it
            }
            set_fail(res,
                     lr.corrupt_caused ? RecoveryStatus::WalCorrupt
                                       : RecoveryStatus::SeqGap,
                     lr.end, 0,
                     lr.corrupt_caused
                         ? "corrupt record inside a sealed segment"
                         : "non-contiguous WAL seq — entries lost");
            wal_tail_ = res.wal_tail;
            last_outcome_ = res.outcome();
            return res;
        }
        if (sealed_damage) {
            // Strict mode never tolerates sealed-segment damage; the rebase
            // pass may — but only when every corrupt segment's lost range
            // was covered (checked above — an uncovered corrupt range already
            // failed). An empty lost range means the damaged bytes never
            // entered the committed seq chain (torn tail append before
            // rotation); still fail-closed in strict mode.
            if (!opts.tolerate_covered_damage) {
                set_fail(res, RecoveryStatus::WalCorrupt, prescan_tail, 0,
                         "corrupt record inside a sealed segment");
                wal_tail_ = res.wal_tail;
                last_outcome_ = res.outcome();
                return res;
            }
        }

        // Forward divergence (spec §18.5): the snapshot claims coverage
        // beyond the surviving log — level-2 rebase resolves it by writing
        // a {snapshot_seq}.wal marker so the seq space resumes past snapshot
        // coverage. Runs whenever a snapshot was read — even when the stream
        // is EMPTY (prescan_tail stays 0): every covered entry is gone and
        // resuming journaling below the cursor would collide with the
        // snapshot-covered seq domain (latent zero-loss + trade-id reseed).
        // Fails before any book mutation so the ladder can retry in place.
        for (auto& bs : states) {
            if (bs.snapshot_have && bs.snapshot_seq > prescan_tail) {
                set_fail(res, RecoveryStatus::InvariantViolated,
                         bs.snapshot_seq, bs.instrument_id,
                         "snapshot_seq ahead of WAL tail");
                wal_tail_ = res.wal_tail;
                last_outcome_ = res.outcome();
                return res;
            }
        }
        if (prescan_started) {
            for (auto& bs : states) {
                if (res.stream_base > bs.snapshot_seq) {
                    // WAL trimmed past this book's snapshot boundary —
                    // entries it needed are unverifiably gone.
                    set_fail(res, RecoveryStatus::InvariantViolated,
                             res.stream_base, bs.instrument_id,
                             "WAL trimmed beyond snapshot coverage");
                    wal_tail_ = res.wal_tail;
                    last_outcome_ = res.outcome();
                    return res;
                }
            }
        }

        // --- Phase 4: snapshot restore (first book mutation) -----------------
        for (std::size_t bsi = 0; bsi < states.size(); ++bsi) {
            auto& bs = states[bsi];
            if (!restore_snapshot(res, bs)) {
                res.books.push_back(bs.report);
                wal_tail_ = res.wal_tail;
                last_outcome_ = res.outcome();
                return res;
            }
            // Register restored orders for CANCEL/MODIFY dispatch and seed
            // the journaled-fill ledger with the snapshot baseline (the
            // covered fills are already inside each node's filled_qty).
            const OrderBook* b = bs.book;
            for (int s = 0; s < 2; ++s) {
                const Side side = s == 0 ? Side::BUY : Side::SELL;
                const uint32_t n =
                    side == Side::BUY ? b->bid_count() : b->ask_count();
                for (uint32_t i = 0; i < n; ++i) {
                    const PriceLevel* lvl = b->level(side, i);
                    for (const Order* o = lvl->head; o != nullptr; o = o->next) {
                        order_owner.set(o->id, static_cast<uint32_t>(bsi) + 1);
                        bs.journaled_fills.set(o->id, o->filled_qty_units);
                        if (o->timestamp_ns > max_ts) max_ts = o->timestamp_ns;
                        if (o->ingress_seq > max_seq) max_seq = o->ingress_seq;
                    }
                }
            }
        }

        // --- Replay machinery: one journal-free MatchingEngine per book ------
        // wal == nullptr and publisher == nullptr — every wal_/publisher_
        // dereference inside the engine is null-guarded, so the replayed
        // stream is re-derived but never re-journaled or re-published.
        for (auto& bs : states) {
            if (bs.replay_pool == nullptr) {
                // Binding omitted its order pool — adopt an internal arena.
                // It lands in adopted_pools_ BEFORE use so every early
                // return keeps it alive: engine-private structures (the
                // pending-stop queue) retain ingress nodes, and retaining
                // the arena keeps the contract safe even if a future
                // engine path adopts nodes into longer-lived structures.
                std::size_t cap = bs.book->max_orders();
                if (cap == 0) cap = 1024;          // detached book floor
                if (cap > kOrderPoolCapacity) cap = kOrderPoolCapacity;
                adopted_pools_.push_back(
                    std::unique_ptr<MemoryPool<Order>>(
                        new MemoryPool<Order>(cap)));
                bs.replay_pool = adopted_pools_.back().get();
            }
            bs.engine.reset(new MatchingEngine(shard_id_, *bs.book,
                                               *bs.replay_pool,
                                               /*wal*/ nullptr,
                                               /*publisher*/ nullptr));
        }

        // --- Phase 4b: snapshot aux block (v3) --------------------------------
        // Engine side-table rows the book blob cannot carry (OrderMeta /
        // pending stops / iceberg reserves / OCO links / peg records —
        // SnapshotStore.hpp). Adopt them into the replay engine BEFORE any
        // journaled entry lands so post-snapshot cancels, amends, triggers
        // and expiries update them identically to live admission; the
        // post-replay state drains to the live engine via
        // recovered_aux_state().
        for (std::size_t bsi = 0; bsi < states.size(); ++bsi) {
            auto& bs = states[bsi];
            const ParsedSnapshot& ps = bs.parsed;
            if (bs.engine == nullptr ||
                (ps.metas.empty() && ps.pendings.empty() &&
                 ps.icebergs.empty() && ps.ocos.empty() && ps.pegs.empty())) {
                continue;
            }
            SnapshotAuxState aux;
            try {
                aux.metas = ps.metas;
                for (const auto& w : ps.pendings) {
                    Order* o = bs.replay_pool != nullptr
                                   ? bs.replay_pool->alloc() : nullptr;
                    if (o == nullptr) {
                        set_fail(res, RecoveryStatus::ApplyFailed,
                                 bs.snapshot_seq, bs.instrument_id,
                                 "snapshot pending order exceeds pool");
                        res.books.push_back(bs.report);
                        wal_tail_ = res.wal_tail;
                        last_outcome_ = res.outcome();
                        return res;
                    }
                    // Shared row→Order mapping (SnapshotStore owns the wire
                    // contract) — identical to the book-restore tmpl fill.
                    SnapshotStore::order_from_rows(w.ord, w.ext, *o);
                    SnapshotAuxPending s{};
                    s.order = o;
                    s.stop_price_ticks = w.ord.stop_price_ticks;
                    s.anchor_ticks = w.anchor_ticks;
                    s.activation_price_ticks = w.activation_price_ticks;
                    s.trail_distance = w.trail_distance;
                    s.gslo_notional_units = w.gslo_notional_units;
                    s.trigger_source = w.trigger_source;
                    s.trail_unit = w.trail_unit;
                    s.armed = w.armed;
                    aux.pendings.push_back(s);
                    // Pending orders sit outside the book — register them for
                    // CANCEL/MODIFY dispatch + the journaled-fill baseline the
                    // same way restore_snapshot registers resting rows.
                    order_owner.set(o->id,
                                    static_cast<uint32_t>(bsi) + 1);
                    bs.journaled_fills.set(o->id, o->filled_qty_units);
                    if (o->timestamp_ns > max_ts) max_ts = o->timestamp_ns;
                    if (o->ingress_seq > max_seq) max_seq = o->ingress_seq;
                }
                for (const auto& w : ps.icebergs) {
                    // The re-slice template is the restored book node (live
                    // slice) with the record's TOTAL qty — a dangling record
                    // means hidden quantity is unaccountable: fail closed.
                    const Order* live = bs.book->find_order(w.order_id);
                    if (live == nullptr) {
                        set_fail(res, RecoveryStatus::SnapshotCorrupt,
                                 bs.snapshot_seq, bs.instrument_id,
                                 "iceberg record without a resting order");
                        res.books.push_back(bs.report);
                        wal_tail_ = res.wal_tail;
                        last_outcome_ = res.outcome();
                        return res;
                    }
                    SnapshotAuxIceberg s{};
                    s.tmpl = *live;
                    s.tmpl.qty_units = w.total_qty_units;
                    s.filled_total_units = w.filled_total_units;
                    s.display_qty_units = w.display_qty_units;
                    aux.icebergs.push_back(s);
                }
                aux.ocos = ps.ocos;
                aux.pegs = ps.pegs;
            } catch (...) {
                set_fail(res, RecoveryStatus::Io, bs.snapshot_seq,
                         bs.instrument_id,
                         "allocation failure adopting snapshot aux rows");
                res.books.push_back(bs.report);
                wal_tail_ = res.wal_tail;
                last_outcome_ = res.outcome();
                return res;
            }
            bs.engine->adopt_aux_state(std::move(aux));
        }

        // --- Phase 5: replay --------------------------------------------------
        // Deterministic re-derivation: every journaled ingress/mutation
        // event is driven through the per-book replay MatchingEngine
        // (wal/publisher nullptr). The engine re-executes sweeps, trigger
        // drains, expiry sweeps, STP and amend rules — direct book mutation
        // is used ONLY for journaled orphan fills (a TRADE whose aggressor
        // ORDER_NEW lies outside the scanned stream) and snapshot restore.
        uint64_t expected = 0;
        bool replay_started = false;

        for (const auto& si : segs) {
            if (!si.has_entries) continue;
            ++res.segments_scanned;

            // Fast path (Phase-02.5 recovery-time finding): a segment whose
            // entire verified span sits below every bound snapshot cursor
            // can only produce covered skips — prescan already CRC-verified
            // every entry, and none of them can be applied. Skip the walk
            // when the segment is internally contiguous
            // (entries == last-first+1) so intra-segment gaps still get the
            // full per-entry continuity check below. Boundary continuity
            // is verified here with exactly the entry-path semantics.
            // Covered-segment entries are attributed to covered_skips per
            // book (matching per-entry accounting); the by-type counters
            // (shard_events/audit/foreign/dedup) only count walked entries.
            if (min_snapshot > 0 && si.last_seq < min_snapshot &&
                si.entries == si.last_seq - si.first_seq + 1) {
                if (!replay_started) {
                    replay_started = true;
                    expected = si.first_seq;
                }
                if (si.first_seq > expected) {
                    // Lost range between segments — legal only when fully
                    // snapshot-covered (same rule as the entry path).
                    if (si.first_seq > min_snapshot) {
                        set_fail(res, RecoveryStatus::SeqGap,
                                 si.first_seq, 0,
                                 "non-contiguous WAL seq — entries lost");
                        break;
                    }
                } else if (si.first_seq < expected) {
                    set_fail(res, RecoveryStatus::SeqGap, si.first_seq, 0,
                             "WAL seq regression — overlapping streams");
                    break;
                }
                expected = si.last_seq + 1;
                res.covered_skips += si.entries;
                ++res.covered_segments_skipped;
                for (auto& b : states) {
                    b.covered_skips += si.entries;
                    if (si.last_seq + 1 > b.cursor) {
                        b.cursor = si.last_seq + 1;
                    }
                }
                continue;
            }

            WalReader r;
            if (r.open(si.path) != WalStatus::Ok) {
                set_fail(res, RecoveryStatus::WalOpenFailed, UINT64_MAX, 0,
                         "segment reopen failed during replay");
                break;
            }
            WalEntryView ev;
            for (;;) {
                const WalScanStep s = r.next(ev);
                if (s == WalScanStep::Pad) continue;
                if (s == WalScanStep::End) break;
                if (s == WalScanStep::Corrupt) {
                    // Reachable only for damage the prescan tolerated
                    // (level-2 covered sealed-segment corruption — the
                    // segment's remaining bytes are snapshot-covered
                    // journal). Post-truncation a tail scans clean.
                    if (opts.tolerate_covered_damage) break;
                    set_fail(res, RecoveryStatus::WalCorrupt, expected, 0,
                             "corrupt record encountered during replay");
                    break;
                }
                // Entry.
                if (!replay_started) {
                    replay_started = true;
                    expected = ev.seq;
                }
                if (ev.seq > expected) {
                    // Snapshot-covered loss: the prescan verified every lost
                    // range ends at/below min_snapshot, so the missing seqs
                    // are provably inside snapshot state. Skip to the next
                    // committed entry. Uncovered → SeqGap, fail closed.
                    if (ev.seq <= min_snapshot) {
                        expected = ev.seq;
                    } else {
                        set_fail(res, RecoveryStatus::SeqGap, ev.seq, 0,
                                 "non-contiguous WAL seq — entries lost");
                        break;
                    }
                } else if (ev.seq < expected) {
                    set_fail(res, RecoveryStatus::SeqGap, ev.seq, 0,
                             "WAL seq regression — overlapping streams");
                    break;
                }
                ++expected;

                const uint32_t want = expected_payload_len(ev.type);
                if (want != 0 && ev.payload_len != want) {
                    set_fail(res, RecoveryStatus::InvariantViolated, ev.seq, 0,
                             "payload length violates pinned contract");
                    break;
                }

                // Every scanned entry advances the "consistent through"
                // cursor of every bound book — entries not targeting a book
                // are no-ops for its state, but its recovery still covered
                // that position. This is what makes recomputed_book_seq ==
                // wal_tail the post-replay invariant per spec §3.5. max()
                // because a snapshot boundary can sit ahead of covered seqs.
                for (auto& b : states) {
                    if (ev.seq + 1 > b.cursor) b.cursor = ev.seq + 1;
                }

                // Deterministic logical clock: the journal header timestamp
                // carries the engine's now_ns_ on engine-authored entries,
                // so driving every bound engine's clock with it is a
                // monotone no-op on real streams and a deterministic clock
                // on tick-free journals (expiry/trigger sweeps re-derive at
                // the journaled time — never a wall clock).
                //
                // Phase-17 Task 17.3.1 (spec §24 #318): each drive also
                // re-anchors the journal-free engine's virtual WAL cursor
                // to this entry's seq — every journal site consumed during
                // the drive then lands on the seq its live counterpart
                // stamped, keeping the L3 wal_seq stream bit-identical
                // across replay. The dispatch arms below re-anchor again
                // per row semantics.
                for (auto& b : states) {
                    b.engine->set_replay_wal_seq(ev.seq);
                    b.engine->on_time_tick(ev.timestamp_ns);
                }
                if (ev.timestamp_ns > max_ts) max_ts = ev.timestamp_ns;
                if (ev.seq > max_seq) max_seq = ev.seq;

                if (ev.type == WalEventType::PREVENTED_MATCH) {
                    // Task-2.3.18 audit row: the accompanying ORDER_CANCEL /
                    // ORDER_MODIFY entries carry the actual book mutation.
                    // Classified explicitly — never routed via payload[0].
                    ++res.audit_events;
                    continue;
                }

                if (is_shard_event(ev.type)) {
                    ++res.shard_events;
                    if (ev.type == WalEventType::TIME_TICK) {
                        WalTimeTickPayload tt;
                        std::memcpy(&tt, ev.payload, sizeof(tt));
                        for (auto& b : states) {
                            // The tick row IS this entry — anchor so the
                            // journal site consumes ev.seq itself.
                            b.engine->set_replay_wal_seq(ev.seq);
                            b.engine->on_time_tick(tt.tick_ns);
                        }
                    }
                    continue;
                }

                // --- dispatch to the target book ------------------------------
                BookState* target = nullptr;
                uint64_t order_id = 0;
                if (ev.type == WalEventType::ORDER_NEW ||
                    ev.type == WalEventType::ORDER_NEW_EX) {
                    // ORDER_NEW_EX opens with a verbatim WalOrderNewPayload —
                    // routing reads the shared head only.
                    WalOrderNewPayload p;
                    std::memcpy(&p, ev.payload, sizeof(p));
                    const auto it = by_instrument.find(p.instrument_id);
                    if (it == by_instrument.end()) {
                        ++res.foreign_entries;
                        continue;
                    }
                    target = it->second;
                } else if (ev.type == WalEventType::TRADE) {
                    WalTradePayload p;
                    std::memcpy(&p, ev.payload, sizeof(p));
                    const auto it = by_instrument.find(p.instrument_id);
                    if (it == by_instrument.end()) {
                        ++res.foreign_entries;
                        continue;
                    }
                    target = it->second;
                } else if (ev.type == WalEventType::OCO_LINK) {
                    // Phase-14 Task 14.3.1 — link carries instrument_id so
                    // it routes to exactly one book like ORDER_NEW/TRADE.
                    WalOcoLinkPayload p;
                    std::memcpy(&p, ev.payload, sizeof(p));
                    const auto it = by_instrument.find(p.instrument_id);
                    if (it == by_instrument.end()) {
                        ++res.foreign_entries;
                        continue;
                    }
                    target = it->second;
                } else if (ev.type == WalEventType::AUCTION_PHASE) {
                    // Phase-15 Tasks 15.3.6/15.3.10 — instrument-scoped via
                    // instrument_id, identical routing to ORDER_NEW/TRADE.
                    WalAuctionPhasePayload p;
                    std::memcpy(&p, ev.payload, sizeof(p));
                    const auto it = by_instrument.find(p.instrument_id);
                    if (it == by_instrument.end()) {
                        ++res.foreign_entries;
                        continue;
                    }
                    target = it->second;
                } else if (ev.type == WalEventType::ORDER_TRIGGERED ||
                           ev.type == WalEventType::PEG_REPRICE) {
                    // Phase-16 — both payloads carry instrument_id; route to
                    // exactly one book like ORDER_NEW/TRADE (the layouts
                    // differ, so decode by type).
                    uint32_t iid = 0;
                    if (ev.type == WalEventType::ORDER_TRIGGERED) {
                        WalOrderTriggeredPayload p;
                        std::memcpy(&p, ev.payload, sizeof(p));
                        iid = p.instrument_id;
                    } else {
                        WalPegRepricePayload p;
                        std::memcpy(&p, ev.payload, sizeof(p));
                        iid = p.instrument_id;
                    }
                    const auto it = by_instrument.find(iid);
                    if (it == by_instrument.end()) {
                        ++res.foreign_entries;
                        continue;
                    }
                    target = it->second;
                } else {  // ORDER_CANCEL / ORDER_MODIFY — no instrument field
                    if (ev.type == WalEventType::ORDER_CANCEL) {
                        WalOrderCancelPayload p;
                        std::memcpy(&p, ev.payload, sizeof(p));
                        order_id = p.order_id;
                    } else {
                        WalOrderModifyPayload p;
                        std::memcpy(&p, ev.payload, sizeof(p));
                        order_id = p.order_id;
                    }
                    const uint32_t owner_idx = order_owner.get(order_id);
                    if (owner_idx == 0) {
                        ++res.dedup_skips;  // unknown order — already gone
                        continue;
                    }
                    target = &states[owner_idx - 1];
                }

                BookState& bs = *target;
                if (ev.seq < bs.snapshot_seq) {
                    // Covered by the snapshot — spec §3.5 replays from
                    // snapshot_seq onward only.
                    ++res.covered_skips;
                    ++bs.covered_skips;
                    continue;
                }
                ++bs.entries_consumed;
                ++res.entries_replayed;

                // Book-mutation delta around an engine call distinguishes a
                // real admission/amend/cancel from an engine-level no-op
                // (reject, duplicate id) — replay idempotence accounting.
                const uint64_t seq_before = bs.book->book_seq();

                switch (ev.type) {
                    case WalEventType::ORDER_NEW:
                    case WalEventType::ORDER_NEW_EX: {
                        WalOrderNewPayload p;
                        std::memcpy(&p, ev.payload, sizeof(p));
                        // Phase-16 extension fields (only meaningful when
                        // the record is ORDER_NEW_EX — length-pinned above).
                        WalOrderNewExPayload px{};
                        const bool has_ex =
                            ev.type == WalEventType::ORDER_NEW_EX;
                        if (has_ex) {
                            std::memcpy(&px, ev.payload, sizeof(px));
                        }
                        OrderType otype = OrderType::LIMIT;
                        switch (p.type) {
                            case 0: otype = OrderType::MARKET; break;
                            case 1: otype = OrderType::LIMIT; break;
                            case 2: otype = OrderType::STOP; break;
                            case 3: otype = OrderType::STOP_LIMIT; break;
                            case kWalOrderTypeIceberg:
                                otype = OrderType::ICEBERG; break;
                            case kWalOrderTypeTrailingStop:
                                otype = OrderType::TRAILING_STOP; break;
                            case kWalOrderTypePeg:
                                otype = OrderType::PEG; break;
                            case kWalOrderTypeMoo:
                                otype = OrderType::MOO; break;
                            case kWalOrderTypeMoc:
                                otype = OrderType::MOC; break;
                            default:
                                set_fail(res, RecoveryStatus::ApplyFailed,
                                         ev.seq, bs.instrument_id,
                                         "unmappable wire order type");
                                break;
                        }
                        if (res.status != RecoveryStatus::Ok) break;
                        Order* o = bs.replay_pool->alloc();
                        if (o == nullptr) {
                            set_fail(res, RecoveryStatus::ApplyFailed, ev.seq,
                                     bs.instrument_id,
                                     "replay order pool exhausted");
                            break;
                        }
                        o->id = p.order_id;
                        o->account_id = p.account_id;
                        o->side = static_cast<Side>(p.side);
                        o->type = otype;
                        o->tif = static_cast<TimeInForce>(p.tif);
                        o->stp_mode = static_cast<StpMode>(p.stp_mode);
                        o->flags = p.flags;
                        o->price_ticks = p.price_ticks;
                        o->qty_units = p.qty_units;
                        o->filled_qty_units = 0;
                        // visible_qty_units on the wire is the engine-
                        // resolved slice for icebergs (register_order
                        // re-derives the same slice); non-iceberg journals
                        // carry remaining qty → display = 0 (fully visible),
                        // matching EnginePump's default.
                        o->display_qty_units =
                            (otype == OrderType::ICEBERG)
                                ? p.visible_qty_units
                                : 0;
                        o->quantity = Decimal::from_mantissa(p.qty_units);
                        // (ts, seq) priority stamps from the WAL envelope —
                        // the EnginePump stamping convention — clamped
                        // monotone vs the snapshot-restored stamp domain so
                        // level (ts, seq) chains stay ordered.
                        o->timestamp_ns = max_ts;
                        o->ingress_seq = max_seq;
                        o->next = o->prev = o->hash_next = nullptr;
                        OrderAux aux{};
                        aux.stop_price_ticks = p.stop_price_ticks;
                        aux.gtd_expiry_ns = p.gtd_expiry_ns;
                        aux.trade_group_id = p.trade_group_id;
                        aux.instrument_id = p.instrument_id;
                        if (has_ex) {
                            // Phase-16 aux verbatim from the extension
                            // record — trigger source, peg and trailing
                            // metadata replay identically to admission.
                            aux.trigger_source = px.trigger_source;
                            aux.peg_mode = px.peg_mode;
                            aux.trail_unit = px.trail_unit;
                            aux.peg_offset_ticks = px.peg_offset_ticks;
                            aux.peg_limit_ticks = px.peg_limit_ticks;
                            aux.trail_distance = px.trail_distance;
                            aux.activation_price_ticks =
                                px.activation_price_ticks;
                        }
                        // The engine owns the node from here: terminal paths
                        // free it to the replay pool, pending stops adopt it
                        // (dies with the replay engine's stop queue), and
                        // resting remainders are cloned into the book's own
                        // pool before the scratch node is freed.
                        // L3 anchor: the replayed ORDER_NEW row occupies
                        // ev.seq; derived fills/cancels consume next.
                        bs.engine->set_replay_wal_seq(ev.seq);
                        bs.engine->on_order_received_ex(o, aux);
                        order_owner.set(p.order_id, static_cast<uint32_t>(&bs - states.data()) + 1);
                        if (bs.book->book_seq() != seq_before ||
                            bs.engine->stops().pending(p.order_id)) {
                            ++res.mutations_applied;
                            ++bs.mutations_applied;
                        } else {
                            // Rejected admission (duplicate id, FOK kill,
                            // liquidity gate): the journal recorded the
                            // attempt but the book is unchanged.
                            ++res.dedup_skips;
                            ++bs.dedup_skips;
                        }
                        break;
                    }
                    case WalEventType::ORDER_CANCEL: {
                        WalOrderCancelPayload p;
                        std::memcpy(&p, ev.payload, sizeof(p));
                        const bool was_pending =
                            bs.engine->stops().pending(p.order_id);
                        bs.engine->set_replay_wal_seq(ev.seq);  // cancel row
                        bs.engine->on_cancel_received(p.order_id,
                                                    p.account_id);
                        if (bs.book->book_seq() != seq_before || was_pending) {
                            ++res.mutations_applied;
                            ++bs.mutations_applied;
                        } else {
                            // Consumed/derived cancel (IOC-FOK-STP-expiry
                            // remainder re-derived by the replayed
                            // admission, or a duplicate journal row).
                            ++res.dedup_skips;
                            ++bs.dedup_skips;
                        }
                        break;
                    }
                    case WalEventType::ORDER_MODIFY: {
                        WalOrderModifyPayload p;
                        std::memcpy(&p, ev.payload, sizeof(p));
                        const bool was_pending =
                            bs.engine->stops().pending(p.order_id);
                        // Resolved payload values + the entry seq as the
                        // §6.9 amend fence input — WAL order is strictly
                        // increasing seqs.
                        bs.engine->set_replay_wal_seq(ev.seq);  // modify row
                        bs.engine->on_amend_received(p.order_id,
                                                     p.new_price_ticks,
                                                     p.new_qty_units,
                                                     p.new_stop_price_ticks,
                                                     ev.seq);
                        if (bs.book->book_seq() != seq_before || was_pending) {
                            ++res.mutations_applied;
                            ++bs.mutations_applied;
                        } else {
                            ++res.dedup_skips;
                            ++bs.dedup_skips;
                        }
                        break;
                    }
                    case WalEventType::OCO_LINK: {
                        WalOcoLinkPayload p;
                        std::memcpy(&p, ev.payload, sizeof(p));
                        // Replays install the link into the journal-free
                        // replay engine exactly as live — the journaled
                        // row precedes both legs' ORDER_NEW, so the armed
                        // members are in place before any replayed fill
                        // re-derives the sibling cancel (or marks a doomed
                        // leg for its replayed ORDER_NEW to reject).
                        const uint64_t members_before =
                            bs.engine->oco_member_count();
                        bs.engine->set_replay_wal_seq(ev.seq);  // link row
                        bs.engine->on_oco_link_received(
                            p.link_id, p.order_id_a, p.order_id_b,
                            p.account_id, p.instrument_id);
                        if (bs.engine->oco_member_count() != members_before) {
                            ++res.mutations_applied;
                            ++bs.mutations_applied;
                        } else {
                            // Idempotent re-send or engine-level reject —
                            // the journal row is still consumed.
                            ++res.dedup_skips;
                            ++bs.dedup_skips;
                        }
                        break;
                    }
                    case WalEventType::AUCTION_PHASE: {
                        // Phase-15 — re-run the transition in the
                        // journal-free replay engine. UNCROSS re-derives
                        // the uncross from the replayed book (its journaled
                        // TRADE rows dedup through the fill ledger below);
                        // QUARANTINE restores the halt; CALL/EXTEND/CANCEL
                        // re-drive the same state machine the live engine
                        // ran. An inconsistent transition (EXTEND without
                        // CALL, engine wal_fault) fails closed.
                        WalAuctionPhasePayload p;
                        std::memcpy(&p, ev.payload, sizeof(p));
                        // L3 anchor: UNCROSS re-journals the phase row
                        // inside auction_resolve_deadline (ev.seq);
                        // verbatim-applied phases' derived rows occupied
                        // ev.seq+1 onward live.
                        bs.engine->set_replay_wal_seq(
                            ev.seq +
                            (p.phase == MatchingEngine::kAuctionPhaseUncross
                                 ? 0 : 1));
                        if (!bs.engine->on_auction_phase_replay(p)) {
                            set_fail(res, RecoveryStatus::ApplyFailed,
                                     ev.seq, bs.instrument_id,
                                     "AUCTION_PHASE replay rejected");
                            break;
                        }
                        ++res.mutations_applied;
                        ++bs.mutations_applied;
                        break;
                    }
                    case WalEventType::ORDER_TRIGGERED: {
                        // Phase-16 Task 16.3.17 — conditional activation.
                        // LAST-sourced triggers re-derive inside the
                        // replayed settle wave (the row is informational);
                        // MARK/INDEX rows are authoritative — the feedless
                        // replay engine cannot re-evaluate the oracle.
                        WalOrderTriggeredPayload p;
                        std::memcpy(&p, ev.payload, sizeof(p));
                        const bool was_pending =
                            bs.engine->stops().pending(p.order_id);
                        // The trigger row is applied verbatim — the derived
                        // fill/cancel rows it spawned live occupied ev.seq+1
                        // onward.
                        bs.engine->set_replay_wal_seq(ev.seq + 1);
                        bs.engine->on_order_triggered_replay(p);
                        if (bs.book->book_seq() != seq_before || was_pending) {
                            ++res.mutations_applied;
                            ++bs.mutations_applied;
                        } else {
                            ++res.dedup_skips;
                            ++bs.dedup_skips;
                        }
                        break;
                    }
                    case WalEventType::PEG_REPRICE: {
                        // Phase-16 Task 16.3.11 — audit row only: the
                        // replaying engine re-derives every committed
                        // reprice inside its own settle wave from the
                        // replayed book references. Consumed, never applied
                        // verbatim (a double-apply would diverge).
                        ++res.dedup_skips;
                        ++bs.dedup_skips;
                        break;
                    }
                    case WalEventType::TRADE: {
                        WalTradePayload p;
                        std::memcpy(&p, ev.payload, sizeof(p));
                        if (p.trade_id > res.max_trade_id) {
                            res.max_trade_id = p.trade_id;
                        }
                        if (applied_trades.insert(p.trade_id)) {
                            ++res.dedup_skips;
                            ++bs.dedup_skips;
                            break;  // same trade_id already consumed — no-op
                        }
                        // Resolve the resting maker leg: both resting → the
                        // leg whose price the trade prints at; exactly one
                        // resting → that leg. (A partial taker's remainder
                        // can be the resting leg — the journaled-fill ledger
                        // still decides derived-vs-orphan correctly because
                        // it accumulates every fill the journal applies to
                        // that order id, aggressor or maker.)
                        Order* buy = bs.book->find_order(p.buy_order_id);
                        Order* sell = bs.book->find_order(p.sell_order_id);
                        Order* maker = nullptr;
                        bool auction_both_resting = false;
                        if (buy != nullptr && sell != nullptr) {
                            if (buy->price_ticks == p.price_ticks &&
                                sell->price_ticks == p.price_ticks) {
                                // Phase-15 uncross fill (Task 15.3.6): in a
                                // single-price clearing BOTH legs rest at
                                // the print price — the continuous-mode
                                // single-maker rule cannot resolve this.
                                auction_both_resting = true;
                            } else if (buy->price_ticks == p.price_ticks &&
                                       sell->price_ticks != p.price_ticks) {
                                maker = buy;
                            } else if (sell->price_ticks == p.price_ticks &&
                                       buy->price_ticks != p.price_ticks) {
                                maker = sell;
                            } else {
                                set_fail(res, RecoveryStatus::ApplyFailed,
                                         ev.seq, bs.instrument_id,
                                         "TRADE maker ambiguous — suspect WAL");
                                break;
                            }
                        } else {
                            maker = buy != nullptr ? buy : sell;
                        }
                        if (auction_both_resting) {
                            // Dedup/apply per leg: the replayed UNCROSS
                            // already applied this fill to both makers —
                            // the journaled-fill ledger detects that
                            // (journaled + qty <= actual filled). An orphan
                            // row (UNCROSS covered by a snapshot) applies
                            // verbatim to BOTH resting legs.
                            bool applied_any = false;
                            bool leg_failed = false;
                            for (Order* leg : {buy, sell}) {
                                int64_t actual = leg->filled_qty_units;
                                if (const IcebergManager::Record* rec =
                                        bs.engine->icebergs().find(leg->id)) {
                                    actual = rec->filled_total_units;
                                }
                                const int64_t jr = bs.journaled_fills.get(leg->id);
                                if (jr + p.qty_units <= actual) continue;
                                if (p.qty_units > remaining_qty_units(*leg)) {
                                    set_fail(res, RecoveryStatus::ApplyFailed,
                                             ev.seq, bs.instrument_id,
                                             "auction TRADE qty exceeds "
                                             "maker remaining");
                                    leg_failed = true;
                                    break;
                                }
                                if (bs.book->apply_fill(leg, p.qty_units) !=
                                        BookError::OK) {
                                    set_fail(res, RecoveryStatus::ApplyFailed,
                                             ev.seq, bs.instrument_id,
                                             "replayed auction TRADE fill "
                                             "rejected");
                                    leg_failed = true;
                                    break;
                                }
                                applied_any = true;
                            }
                            if (leg_failed) break;
                            if (applied_any) {
                                ++res.mutations_applied;
                                ++bs.mutations_applied;
                                ++bs.trades_applied;
                                ++res.trades_applied;
                            } else {
                                ++bs.trades_derived;
                                ++res.trades_derived;
                            }
                        } else if (maker == nullptr) {
                            // Both legs consumed/absent — nothing applies.
                            // When a journaled order id participated the
                            // fill was engine-derived during an ORDER_NEW
                            // replay; otherwise the maker was consumed by
                            // earlier entries — an idempotent no-op either
                            // way.
                            if (order_owner.contains(p.buy_order_id) ||
                                order_owner.contains(p.sell_order_id)) {
                                ++bs.trades_derived;
                                ++res.trades_derived;
                            } else {
                                ++res.dedup_skips;
                                ++bs.dedup_skips;
                            }
                        } else {
                            // Applied fills on this order id — an iceberg
                            // record tracks the whole order across slice
                            // refreshes; the book node carries the live
                            // slice only.
                            int64_t actual_filled = maker->filled_qty_units;
                            if (const IcebergManager::Record* rec =
                                    bs.engine->icebergs().find(maker->id)) {
                                actual_filled = rec->filled_total_units;
                            }
                            const int64_t journaled = bs.journaled_fills.get(maker->id);
                            if (journaled + p.qty_units <= actual_filled) {
                                // The replayed aggressor's walk_match
                                // already re-derived this fill — do not
                                // re-apply (zero duplicate trades).
                                ++bs.trades_derived;
                                ++res.trades_derived;
                            } else {
                                // Orphan fill: the aggressor's ORDER_NEW
                                // lives outside the scanned stream
                                // (snapshot-covered prefix / trimmed tail)
                                // so the engine could not reproduce it.
                                // Apply to the resting maker verbatim —
                                // zero missing trades.
                                if (p.qty_units >
                                    remaining_qty_units(*maker)) {
                                    set_fail(res, RecoveryStatus::ApplyFailed,
                                             ev.seq, bs.instrument_id,
                                             "TRADE qty exceeds maker "
                                             "remaining");
                                    break;
                                }
                                const BookError e =
                                    bs.book->apply_fill(maker, p.qty_units);
                                if (e != BookError::OK) {
                                    set_fail(res, RecoveryStatus::ApplyFailed,
                                             ev.seq, bs.instrument_id,
                                             "replayed TRADE fill rejected");
                                    break;
                                }
                                ++res.mutations_applied;
                                ++bs.mutations_applied;
                                ++bs.trades_applied;
                                ++res.trades_applied;
                            }
                        }
                        // Accrue the journaled quantity on BOTH legs: an
                        // order's filled accumulates whether it filled as
                        // maker or as aggressor (a resting taker remainder
                        // is resolvable only through this ledger).
                        bs.journaled_fills.ref(p.buy_order_id) += p.qty_units;
                        bs.journaled_fills.ref(p.sell_order_id) += p.qty_units;
                        break;
                    }
                    default:
                        break;  // unreachable — shard events handled above
                }
                if (res.status != RecoveryStatus::Ok) break;
            }
            if (res.status != RecoveryStatus::Ok) break;
        }

        if (res.status == RecoveryStatus::Ok) {
            // wal_tail/stream_base/wal_entries were established by the
            // prescan (post-truncation). Replay cross-checks: `expected`
            // tracks last_seq+1 across covered gaps and must land exactly on
            // the prescanned tail.
            if (replay_started && expected != res.wal_tail) {
                set_fail(res, RecoveryStatus::SeqGap, expected, 0,
                         "seq accounting mismatch");
            }

            // --- Phase 6: boot invariant (fail-closed) -------------------------
            const bool stream_empty = res.wal_entries == 0;
            for (auto& bs : states) {
                // Snapshot claims coverage beyond the surviving log — spec
                // §18.5 forward divergence; never start with suspect state.
                // Fires even on an EMPTY stream: wal_tail=0 below a nonzero
                // snapshot cursor means every covered entry is gone, and
                // resuming journaling below the cursor would collide with
                // the snapshot-covered seq domain (latent zero-loss).
                if (bs.snapshot_seq > res.wal_tail) {
                    set_fail(res, RecoveryStatus::InvariantViolated,
                             bs.snapshot_seq, bs.instrument_id,
                             "snapshot_seq ahead of WAL tail");
                    break;
                }
                if (!stream_empty && replay_started &&
                    res.stream_base > bs.snapshot_seq) {
                    // WAL trimmed past this book's snapshot boundary —
                    // entries it needed are unverifiably gone.
                    set_fail(res, RecoveryStatus::InvariantViolated,
                             res.stream_base, bs.instrument_id,
                             "WAL trimmed beyond snapshot coverage");
                    break;
                }
                const char* violation = nullptr;
                if (!bs.book->validate(&violation)) {
                    set_fail(res, RecoveryStatus::InvariantViolated,
                             bs.cursor, bs.instrument_id,
                             violation != nullptr ? violation
                                                  : "book audit failed");
                    break;
                }
            }
            if (res.status == RecoveryStatus::Ok) {
                // Spec §3.5 invariant: recomputed book_seq == WAL tail.
                // cursor = WAL position this book's recovered state is
                // consistent through; a complete contiguous replay drives it
                // to the tail — assert the equality explicitly per book. With
                // no WAL stream the snapshot is authoritative on its own
                // integrity (CRC + structure + audit already proven).
                for (auto& bs : states) {
                    const uint64_t recomputed =
                        (res.wal_entries == 0) ? bs.snapshot_seq : bs.cursor;
                    bs.report.recomputed_book_seq = recomputed;
                    bs.report.book_seq_verified =
                        (res.wal_entries == 0) ||
                        (recomputed == res.wal_tail);
                    if (!bs.report.book_seq_verified) {
                        set_fail(res, RecoveryStatus::InvariantViolated,
                                 recomputed, bs.instrument_id,
                                 "recomputed book_seq != WAL tail");
                        break;
                    }
                }
            }
            snapshot_seq_ =
                (min_snapshot == UINT64_MAX) ? 0 : min_snapshot;
        }

        for (auto& bs : states) {
            bs.report.snapshot_loaded = bs.snapshot_loaded;
            bs.report.snapshot_seq = bs.snapshot_seq;
            bs.report.snapshot_book_seq = bs.snapshot_book_seq;
            bs.report.orders_restored = bs.orders_restored;
            bs.report.entries_consumed = bs.entries_consumed;
            bs.report.mutations_applied = bs.mutations_applied;
            bs.report.dedup_skips = bs.dedup_skips;
            bs.report.covered_skips = bs.covered_skips;
            bs.report.trades_derived = bs.trades_derived;
            bs.report.trades_applied = bs.trades_applied;
            bs.report.engine_trades_emitted =
                bs.engine != nullptr ? bs.engine->trades_emitted() : 0;
            bs.report.pending_stops =
                bs.engine != nullptr ? bs.engine->stops().size() : 0;
            res.books.push_back(bs.report);
            // Phase-15 — capture the replayed auction/lifecycle position
            // for live-engine adoption (adopt_auction_state). The replay
            // engine dies with this BookState; the parked nodes live in the
            // binding pool / retained arena and stay valid per that
            // contract.
            if (bs.engine != nullptr) {
                RecoveredAuctionState ra{};
                ra.phase = bs.engine->auction_phase();
                ra.auction_id = bs.engine->auction_id();
                ra.deadline_ns = bs.engine->auction_deadline_ns();
                ra.extensions = bs.engine->auction_extensions();
                ra.awaiting = bs.engine->auction_awaiting();
                ra.last_completed = bs.engine->last_completed_auction_id();
                ra.quarantined = bs.engine->quarantined();
                ra.quarantine_code = bs.engine->quarantine_code();
                ra.parked_head = const_cast<Order*>(
                    bs.engine->auction_parked_head());
                ra.parked_count = bs.engine->auction_parked_count();
                recovered_auctions_[bs.instrument_id] = ra;
                // v3 aux state — drain the replay engine's side tables
                // wholesale (pending nodes leave the queue; the live engine
                // re-enqueues them via adopt_aux_state). Covers both
                // snapshot-adopted rows and everything WAL replay
                // re-registered at admission.
                SnapshotAuxState aux;
                if (bs.engine->drain_aux_state(aux)) {
                    recovered_aux_[bs.instrument_id] = std::move(aux);
                }
            }
        }
        wal_tail_ = res.wal_tail;
        last_outcome_ = res.outcome();
        return res;
    } catch (...) {
        res.status = RecoveryStatus::Io;
        std::snprintf(res.detail, sizeof(res.detail),
                      "unexpected exception during recovery");
        wal_tail_ = res.wal_tail;
        last_outcome_ = res.outcome();
        return res;
    }
}

// ==== Graduated recovery ladder (Phase-04 Task 4.3.5/4.3.9) ==================

namespace {

// Canonical runbook for the level-3 halt (the alert line and the
// recovery_reports detail both carry it — Task 4.3.5 AC).
constexpr const char* kHaltRunbook =
    "docs/runbooks/wal-recovery-halt.md (exchange:replay-from-archive)";

void json_escape_to(std::string& out, std::string_view s) {
    for (const char c : s) {
        switch (c) {
            case '"':  out += "\\\""; break;
            case '\\': out += "\\\\"; break;
            case '\n': out += "\\n"; break;
            case '\r': out += "\\r"; break;
            case '\t': out += "\\t"; break;
            default:
                if (static_cast<unsigned char>(c) < 0x20) {
                    char buf[8];
                    std::snprintf(buf, sizeof(buf), "\\u%04x",
                                  static_cast<unsigned>(c));
                    out += buf;
                } else {
                    out += c;
                }
        }
    }
}

// Serialize the migration-065 recovery_reports contract as one JSON line.
// The Go consumer (services/cmd/wal-recovery persist-reports) inserts it
// verbatim; detail_json is embedded pre-encoded (callers build the object).
std::string recovery_report_json(const RecoveryReport& r,
                                 uint64_t ts_unix_ns) {
    std::string j;
    j.reserve(256 + r.detail_json.size());
    char num[32];
    j += '{';
    std::snprintf(num, sizeof(num), "%u", r.shard_id);
    j += "\"shard_id\":"; j += num;
    std::snprintf(num, sizeof(num), "%lld", (long long)r.book_seq);
    j += ",\"book_seq\":"; j += num;
    std::snprintf(num, sizeof(num), "%llu", (unsigned long long)r.wal_tail);
    j += ",\"wal_tail\":"; j += num;
    std::snprintf(num, sizeof(num), "%lld", (long long)r.last_valid_seq);
    j += ",\"last_valid_seq\":"; j += num;
    std::snprintf(num, sizeof(num), "%llu",
                  (unsigned long long)r.snapshot_seq);
    j += ",\"snapshot_seq\":"; j += num;
    std::snprintf(num, sizeof(num), "%lld",
                  (long long)r.first_divergent_seq);
    j += ",\"first_divergent_seq\":"; j += num;
    j += ",\"stage\":\""; json_escape_to(j, r.stage); j += '"';
    j += ",\"outcome\":\""; json_escape_to(j, r.outcome); j += '"';
    j += ",\"detail\":";
    j += r.detail_json.empty() ? "{}" : r.detail_json;
    std::snprintf(num, sizeof(num), "%llu", (unsigned long long)ts_unix_ns);
    j += ",\"ts_unix_ns\":"; j += num;
    j += '}';
    return j;
}

void fsync_parent_dir(const std::string& path) noexcept {
    const std::string dir =
        std::filesystem::path(path).parent_path().string();
    if (dir.empty()) return;
    const int fd = ::open(dir.c_str(), O_RDONLY | O_DIRECTORY | O_CLOEXEC);
    if (fd < 0) return;
    ::fsync(fd);
    ::close(fd);
}

// Append one JSON line to the report log (durable — fsync file + dir).
bool append_jsonl(const std::string& path, const std::string& line) noexcept {
    const int fd =
        ::open(path.c_str(), O_WRONLY | O_CREAT | O_APPEND | O_CLOEXEC, 0644);
    if (fd < 0) return false;
    const std::string buf = line + '\n';
    uint64_t off = 0;
    bool ok = true;
    while (off < buf.size()) {
        const ssize_t w =
            ::write(fd, buf.data() + off, buf.size() - off);
        if (w < 0) {
            if (errno == EINTR) continue;
            ok = false;
            break;
        }
        off += static_cast<uint64_t>(w);
    }
    if (ok && ::fsync(fd) != 0) ok = false;
    ::close(fd);
    fsync_parent_dir(path);
    return ok;
}

// Level-2 rebase primitive: write a fresh segment {marker_seq}.wal holding a
// single BOOK_SNAPSHOT marker entry at seq=marker_seq. The segment's
// filename stem is the seq floor Wal::open() resumes from, and the marker
// entry itself anchors wal_tail = marker_seq + 1 — the WAL seq space is
// thereby rebased past snapshot coverage (the lost range below marker_seq
// is snapshot-covered by definition of the divergence being resolved).
// Idempotent: an existing file is left untouched.
bool write_rebase_marker(const std::string& dir, uint16_t shard,
                         uint64_t marker_seq) noexcept {
    const std::string path = dir + "/" + std::to_string(marker_seq) + ".wal";
    std::error_code ec;
    if (std::filesystem::exists(path, ec)) return !ec;

    WalFileHeader fh{};
    fh.magic = kWalMagic;
    fh.version = kWalVersion;
    fh.shard_id = shard;

    WalBookSnapshotHeader pay{};
    pay.instrument_id = 0;      // shard-level marker, no book payload
    pay.level_count = 0;
    pay.order_count = 0;
    pay.book_seq = marker_seq;  // WAL cursor the snapshot covers

    uint8_t entry[sizeof(WalEntryHeader) + sizeof(pay) + sizeof(uint32_t)];
    const uint64_t n = wal_encode_entry(entry, marker_seq, now_ns(),
                                        WalEventType::BOOK_SNAPSHOT,
                                        &pay, sizeof(pay));

    const int fd = ::open(path.c_str(),
                          O_WRONLY | O_CREAT | O_EXCL | O_CLOEXEC, 0644);
    if (fd < 0) return errno == EEXIST;  // raced twin — already present
    bool ok = true;
    {
        const uint8_t* p = reinterpret_cast<const uint8_t*>(&fh);
        uint64_t off = 0;
        while (off < sizeof(fh)) {
            const ssize_t w = ::write(fd, p + off, sizeof(fh) - off);
            if (w < 0) { if (errno == EINTR) continue; ok = false; break; }
            off += static_cast<uint64_t>(w);
        }
    }
    if (ok) {
        uint64_t off = 0;
        while (off < n) {
            const ssize_t w = ::write(fd, entry + off, n - off);
            if (w < 0) { if (errno == EINTR) continue; ok = false; break; }
            off += static_cast<uint64_t>(w);
        }
    }
    if (ok && ::fsync(fd) != 0) ok = false;
    ::close(fd);
    fsync_parent_dir(path);
    if (!ok) {
        std::error_code rec;
        std::filesystem::remove(path, rec);
    }
    return ok;
}

}  // namespace

RecoveryLadderResult RecoveryManager::recover_ladder(
    std::string_view wal_dir,
    std::span<const RecoveryBookBinding> bindings,
    std::string_view report_log_path) noexcept {
    RecoveryLadderResult out;
    out.report_path = std::string(report_log_path);
    const std::string dir_str(wal_dir);

    // Build the detail fragment once per outcome — filled in below.
    auto fill_report = [&](const RecoveryResult& r, const char* outcome,
                           const char* extra) {
        RecoveryReport& rep = out.report;
        rep.shard_id = shard_id_;
        rep.wal_tail = r.wal_tail;
        rep.last_valid_seq = (r.wal_entries > 0)
                                 ? static_cast<int64_t>(r.last_valid_seq)
                                 : -1;
        rep.snapshot_seq = snapshot_seq_;
        rep.first_divergent_seq =
            (r.first_divergent_seq == UINT64_MAX)
                ? -1
                : static_cast<int64_t>(r.first_divergent_seq);
        rep.book_seq = -1;
        for (const auto& b : r.books) {
            if (b.book_seq_verified) {
                rep.book_seq = static_cast<int64_t>(b.recomputed_book_seq);
            }
        }
        rep.outcome = outcome;
        std::string d;
        d += "{\"status\":\""; json_escape_to(d, recovery_status_str(r.status));
        d += "\",\"detail\":\"";
        json_escape_to(d, r.detail);
        d += "\",\"instrument\":";
        char num[32];
        std::snprintf(num, sizeof(num), "%u", r.detail_instrument);
        d += num;
        std::snprintf(num, sizeof(num), "%llu",
                      (unsigned long long)r.covered_gap_seqs);
        d += ",\"covered_gap_seqs\":"; d += num;
        if (r.tail_truncated) {
            std::snprintf(num, sizeof(num), "%llu",
                          (unsigned long long)r.truncate_offset);
            d += ",\"truncate_offset\":"; d += num;
        }
        if (r.snapshot_fallback) d += ",\"snapshot_fallback\":true";
        if (out.rebase_marker_written) d += ",\"rebase_marker\":true";
        d += ",\"runbook\":\""; json_escape_to(d, kHaltRunbook); d += "\"";
        if (extra != nullptr) { d += ","; d += extra; }
        d += '}';
        rep.detail_json = std::move(d);
    };
    auto emit_report = [&]() {
        const std::string line =
            recovery_report_json(out.report, now_ns());
        if (!out.report_path.empty()) {
            out.report_written = append_jsonl(out.report_path, line);
        }
        // Alert path (spec §3.5 ladder): the fail-closed halt raises P1 —
        // the structured stderr line is scraped by the supervisor/log
        // pipeline (same convention as ENGINE_STALL in main.cpp) and the
        // NATS ops channel consumes the JSONL row. Repaired/rebased
        // recoveries still emit the row but at notice level — the service
        // is healthy again after the probe gate.
        const bool halted =
            std::strcmp(out.report.outcome, "WAL_RECOVERY_HALT") == 0;
        std::fprintf(stderr, "%s %s shard=%u wal_tail=%llu "
                             "first_divergent_seq=%lld runbook=%s\n",
                     halted ? "ALERT_P1" : "NOTICE",
                     out.report.outcome, shard_id_,
                     (unsigned long long)out.report.wal_tail,
                     (long long)out.report.first_divergent_seq,
                     kHaltRunbook);
        // Mirror the JSONL row to stderr too — when no report file is
        // configured this is the only structured copy.
        std::fprintf(stderr, "recovery_report %s\n", line.c_str());
    };

    // ---- Level 1: CRC repair + replay (strict contract) --------------------
    RecoveryResult r1 = recover(wal_dir, bindings, RecoverOptions{});
    if (r1.ok()) {
        out.result = std::move(r1);
        out.level = 1;
        out.outcome = out.result.outcome();  // CLEAN or WAL_REPAIRED
        if (out.outcome != RecoveryOutcome::CLEAN) {
            fill_report(out.result, "WAL_REPAIRED", nullptr);
            emit_report();
        }
        return out;
    }

    // Level-2 rebase requires empty bound books — it re-runs recover() in
    // place. Any level-1 failure AFTER Phase-4 (restore/replay) leaves a
    // book populated: halting is the only fail-closed answer (restart with
    // fresh books; the report records books_dirty_on_fail).
    bool books_dirty = false;
    for (const auto& b : bindings) {
        if (b.book != nullptr && b.book->live_orders() != 0) {
            books_dirty = true;
            break;
        }
    }

    RecoveryResult r2;
    char l1_status[64];
    std::snprintf(l1_status, sizeof(l1_status), "\"level1_status\":\"%s\"",
                  recovery_status_str(r1.status));

    // Forward divergence (snapshot_seq > wal_tail): peek the latest verified
    // snapshot cursor per binding — read-only, no book mutation — and write
    // the rebase-marker segment when the snapshot genuinely covers past the
    // surviving tail. This runs on EVERY level-1 failure, before the
    // dirty-book gate: the marker is directory-level and anchors the seq
    // domain so a level-3 halt's next boot never journals below the covered
    // cursor (e.g. empty-WAL + ahead-snapshot divergence).
    if (snapshots_ != nullptr) {
        uint64_t max_ss = 0;
        bool any = false;
        for (const auto& b : bindings) {
            uint64_t seq = 0;
            bool has = false;
            std::vector<uint8_t> blob;
            if (!snapshots_->load_latest(b.instrument_id, &seq, &blob,
                                         &has)) {
                continue;  // corrupt latest — fallback/genesis decides
            }
            if (has) { any = true; if (seq > max_ss) max_ss = seq; }
        }
        if (any && max_ss > r1.wal_tail) {
            if (write_rebase_marker(dir_str,
                                    static_cast<uint16_t>(shard_id_),
                                    max_ss)) {
                out.rebase_marker_written = true;
            }
        }
    }

    if (!books_dirty) {
        out.level = 2;
        RecoverOptions rebase_opts;
        rebase_opts.snapshot_fallback = true;
        rebase_opts.tolerate_covered_damage = true;
        r2 = recover(wal_dir, bindings, rebase_opts);
        if (r2.ok()) {
            out.result = std::move(r2);
            out.outcome = RecoveryOutcome::SNAPSHOT_REBASED;
            fill_report(out.result, "SNAPSHOT_REBASED", l1_status);
            emit_report();
            return out;
        }
    } else {
        out.books_dirty_on_fail = true;
    }

    // ---- Level 3: fail-closed halt (last resort) ----------------------------
    out.level = 3;
    out.outcome = RecoveryOutcome::HALTED;
    // Report the most diagnostic attempt: the level-2 result when it ran
    // (it reflects post-repair/post-rebase state), else level-1.
    out.result = books_dirty ? r1 : r2;
    std::string extra(l1_status);
    if (!books_dirty && r2.status != RecoveryStatus::Ok) {
        extra += ",\"level2_status\":\"";
        json_escape_to(extra, recovery_status_str(r2.status));
        extra += "\"";
    }
    if (books_dirty) extra += ",\"books_dirty_on_fail\":true";
    fill_report(out.result, "WAL_RECOVERY_HALT", extra.c_str());
    emit_report();
    return out;
}

}  // namespace exch
