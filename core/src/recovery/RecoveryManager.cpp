// PHASE-02 TASK-2.3.4 — snapshot load + WAL replay + fail-closed boot
// invariant (spec §3.5/§18.1). See include/recovery/RecoveryManager.hpp for
// the sequence model and invariant contract.

#include "recovery/RecoveryManager.hpp"

#include <algorithm>
#include <cerrno>
#include <cstdio>
#include <cstring>
#include <filesystem>
#include <memory>
#include <system_error>
#include <unordered_map>
#include <unordered_set>

#include "matching/MatchingEngine.hpp"
#include "wal/Wal.hpp"

namespace exch {

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
    // Pool precedes engine so teardown frees the engine first; the pool
    // holds ingress scratch + pending-stop nodes — the book's own pool
    // keeps owning resting nodes.
    std::unique_ptr<MemoryPool<Order>> replay_pool;
    std::unique_ptr<MatchingEngine> engine;
    // Cumulative journaled fill qty per order id — the derived-vs-orphan
    // ledger for TRADE replay (seeded with filled_qty_units of
    // snapshot-restored orders; accrued on both legs of every consumed
    // journaled trade).
    std::unordered_map<uint64_t, int64_t> journaled_fills;
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
        default:                           return 0;  // BOOK_SNAPSHOT/MARGIN_*
    }
}

bool is_shard_event(WalEventType t) noexcept {
    return t == WalEventType::TIME_TICK ||
           t == WalEventType::BOOK_SNAPSHOT ||
           t == WalEventType::MARGIN_RESERVE ||
           t == WalEventType::MARGIN_RELEASE;
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

bool RecoveryManager::load_snapshot(RecoveryResult& res,
                                    BookState& bs) noexcept {
    try {
        if (snapshots_ == nullptr) return true;  // WAL-only boot

        uint64_t seq = 0;
        bool has = false;
        std::vector<uint8_t> blob;
        if (!snapshots_->load_latest(bs.instrument_id, &seq, &blob, &has)) {
            set_fail(res, RecoveryStatus::SnapshotLoadFailed, UINT64_MAX,
                     bs.instrument_id, "snapshot sink load failed");
            return false;
        }
        if (!has) return true;  // clean cold start — replay from genesis

        ParsedSnapshot parsed;
        if (!SnapshotStore::parse_book(blob.data(), blob.size(), parsed)) {
            set_fail(res, RecoveryStatus::SnapshotCorrupt, seq,
                     bs.instrument_id, "snapshot blob failed structural parse");
            return false;
        }
        if (parsed.header.instrument_id != bs.instrument_id ||
            parsed.header.book_seq != seq) {
            set_fail(res, RecoveryStatus::SnapshotCorrupt, seq,
                     bs.instrument_id,
                     "snapshot header seq/instrument mismatches container");
            return false;
        }

        if (bs.book->live_orders() != 0) {
            set_fail(res, RecoveryStatus::InvariantViolated, seq,
                     bs.instrument_id,
                     "bound book not empty at recovery start");
            return false;
        }

        // Rebuild levels+orders by re-insertion: add_order() tail-appends into
        // each level, so replaying the level-major FIFO stream order restores
        // price-time priority exactly. There is no OrderBook API to install
        // book_seq_ (pinned comment records the WAL cursor there instead), so
        // the recovered mutation counter starts at the restored order count —
        // the WAL-cursor invariant is tracked by this manager.
        for (const auto& po : parsed.orders) {
            Order* out = nullptr;
            const BookError e = bs.book->add_order(po.tmpl, &out);
            if (e != BookError::OK || out == nullptr) {
                set_fail(res, RecoveryStatus::SnapshotCorrupt, seq,
                         bs.instrument_id,
                         "snapshot order rejected by book on restore");
                return false;
            }
            ++bs.orders_restored;
        }
        const char* violation = nullptr;
        if (!bs.book->validate(&violation)) {
            set_fail(res, RecoveryStatus::SnapshotCorrupt, seq,
                     bs.instrument_id,
                     violation != nullptr ? violation
                                          : "restored book failed audit");
            return false;
        }
        bs.snapshot_seq = seq;
        bs.snapshot_book_seq = parsed.header.book_seq;
        bs.snapshot_loaded = true;
        bs.cursor = seq;
        return true;
    } catch (...) {
        set_fail(res, RecoveryStatus::Io, UINT64_MAX, bs.instrument_id,
                 "unexpected allocation failure during snapshot load");
        return false;
    }
}



RecoveryResult RecoveryManager::recover(
    std::string_view wal_dir,
    std::span<const RecoveryBookBinding> bindings) noexcept {
    RecoveryResult res;
    try {
        std::vector<BookState> states(bindings.size());
        std::unordered_map<uint32_t, BookState*> by_instrument;
        std::unordered_map<uint64_t, BookState*> order_owner;
        std::unordered_set<uint64_t> applied_trades;

        for (std::size_t i = 0; i < bindings.size(); ++i) {
            states[i].instrument_id = bindings[i].instrument_id;
            states[i].book = bindings[i].book;
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

        // --- Phase 1: snapshot restore (per bound book) ----------------------
        for (auto& bs : states) {
            if (!load_snapshot(res, bs)) {
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
                        order_owner[o->id] = &bs;
                        bs.journaled_fills[o->id] = o->filled_qty_units;
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
            std::size_t cap = bs.book->max_orders();
            if (cap == 0) cap = 1024;              // detached book floor
            if (cap > kOrderPoolCapacity) cap = kOrderPoolCapacity;
            bs.replay_pool.reset(new MemoryPool<Order>(cap));
            bs.engine.reset(new MatchingEngine(shard_id_, *bs.book,
                                               *bs.replay_pool,
                                               /*wal*/ nullptr,
                                               /*publisher*/ nullptr));
        }

        // --- Phase 2: enumerate + pre-scan WAL segments ----------------------
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
                    segs.push_back(SegmentInfo{});
                    segs.back().path = de.path().string();
                }
            }
        }
        for (auto& si : segs) {
            if (!scan_segment(si.path, static_cast<uint16_t>(shard_id_), si)) {
                set_fail(res, RecoveryStatus::WalOpenFailed, UINT64_MAX, 0,
                         "segment unreadable/bad header/foreign shard");
                wal_tail_ = res.wal_tail;
                last_outcome_ = res.outcome();
                return res;
            }
        }
        // Order segments by first entry seq (empties carry no seqs — sort last).
        std::sort(segs.begin(), segs.end(), [](const SegmentInfo& a,
                                               const SegmentInfo& b) {
            if (a.has_entries != b.has_entries) return a.has_entries;
            if (a.has_entries && b.has_entries && a.first_seq != b.first_seq)
                return a.first_seq < b.first_seq;
            return a.path < b.path;
        });

        // --- Phase 3: torn-tail repair on the tail segment -------------------
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

        // Mid-stream (non-tail) corruption is a ladder step-2/3 case — entries
        // after the corrupt record in that file are unreadable, so the seq
        // chain is broken. Report it fail-closed rather than silently halting
        // replay early.
        for (std::size_t i = 0; i < segs.size(); ++i) {
            if (i != tail_idx && segs[i].corrupt) {
                set_fail(res, RecoveryStatus::WalCorrupt, segs[i].last_seq, 0,
                         "corrupt record inside a sealed segment");
                wal_tail_ = res.wal_tail;
                last_outcome_ = res.outcome();
                return res;
            }
        }

        // --- Phase 4: replay --------------------------------------------------
        // Deterministic re-derivation: every journaled ingress/mutation
        // event is driven through the per-book replay MatchingEngine
        // (wal/publisher nullptr). The engine re-executes sweeps, trigger
        // drains, expiry sweeps, STP and amend rules — direct book mutation
        // is used ONLY for journaled orphan fills (a TRADE whose aggressor
        // ORDER_NEW lies outside the scanned stream) and snapshot restore.
        bool started = false;
        uint64_t expected = 0;
        res.stream_base = 0;

        for (const auto& si : segs) {
            if (!si.has_entries) continue;
            ++res.segments_scanned;
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
                    // Post-truncation a tail segment should scan clean; any
                    // residual corruption is a hard fail.
                    set_fail(res, RecoveryStatus::WalCorrupt, expected, 0,
                             "corrupt record encountered during replay");
                    break;
                }
                // Entry.
                if (!started) {
                    started = true;
                    res.stream_base = ev.seq;
                    expected = ev.seq;
                }
                if (ev.seq != expected) {
                    set_fail(res, RecoveryStatus::SeqGap, ev.seq, 0,
                             "non-contiguous WAL seq — entries lost");
                    break;
                }
                ++expected;
                ++res.wal_entries;

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
                for (auto& b : states) {
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
                            b.engine->on_time_tick(tt.tick_ns);
                        }
                    }
                    continue;
                }

                // --- dispatch to the target book ------------------------------
                BookState* target = nullptr;
                uint64_t order_id = 0;
                if (ev.type == WalEventType::ORDER_NEW) {
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
                    const auto it = order_owner.find(order_id);
                    if (it == order_owner.end()) {
                        ++res.dedup_skips;  // unknown order — already gone
                        continue;
                    }
                    target = it->second;
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
                    case WalEventType::ORDER_NEW: {
                        WalOrderNewPayload p;
                        std::memcpy(&p, ev.payload, sizeof(p));
                        OrderType otype = OrderType::LIMIT;
                        switch (p.type) {
                            case 0: otype = OrderType::MARKET; break;
                            case 1: otype = OrderType::LIMIT; break;
                            case 2: otype = OrderType::STOP; break;
                            case 3: otype = OrderType::STOP_LIMIT; break;
                            case kWalOrderTypeIceberg:
                                otype = OrderType::ICEBERG; break;
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
                        // The engine owns the node from here: terminal paths
                        // free it to the replay pool; pending stops and
                        // resting remainders are adopted.
                        bs.engine->on_order_received_ex(o, aux);
                        order_owner[p.order_id] = &bs;
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
                    case WalEventType::TRADE: {
                        WalTradePayload p;
                        std::memcpy(&p, ev.payload, sizeof(p));
                        if (applied_trades.count(p.trade_id) != 0) {
                            ++res.dedup_skips;
                            ++bs.dedup_skips;
                            break;  // same trade_id already consumed — no-op
                        }
                        applied_trades.insert(p.trade_id);
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
                        if (buy != nullptr && sell != nullptr) {
                            if (buy->price_ticks == p.price_ticks &&
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
                        if (maker == nullptr) {
                            // Both legs consumed/absent — nothing applies.
                            // When a journaled order id participated the
                            // fill was engine-derived during an ORDER_NEW
                            // replay; otherwise the maker was consumed by
                            // earlier entries — an idempotent no-op either
                            // way.
                            if (order_owner.count(p.buy_order_id) != 0 ||
                                order_owner.count(p.sell_order_id) != 0) {
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
                            const int64_t journaled =
                                bs.journaled_fills[maker->id];
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
                        bs.journaled_fills[p.buy_order_id] += p.qty_units;
                        bs.journaled_fills[p.sell_order_id] += p.qty_units;
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
            res.wal_tail = started ? res.stream_base + res.wal_entries : 0;
            if (started && expected != res.stream_base + res.wal_entries) {
                // Defensive: contiguity loop already guarantees this.
                set_fail(res, RecoveryStatus::SeqGap, expected, 0,
                         "seq accounting mismatch");
            }

            // --- Phase 5: boot invariant (fail-closed) -------------------------
            const bool stream_empty = res.wal_entries == 0;
            uint64_t min_snapshot = UINT64_MAX;
            for (auto& bs : states) {
                if (!stream_empty) {
                    if (bs.snapshot_seq > res.wal_tail) {
                        // Snapshot claims coverage beyond the log — spec §18.5
                        // forward divergence; never start with suspect state.
                        set_fail(res, RecoveryStatus::InvariantViolated,
                                 bs.snapshot_seq, bs.instrument_id,
                                 "snapshot_seq ahead of WAL tail");
                        break;
                    }
                    if (started && res.stream_base > bs.snapshot_seq) {
                        // WAL trimmed past this book's snapshot boundary —
                        // entries it needed are unverifiably gone.
                        set_fail(res, RecoveryStatus::InvariantViolated,
                                 res.stream_base, bs.instrument_id,
                                 "WAL trimmed beyond snapshot coverage");
                        break;
                    }
                }
                const char* violation = nullptr;
                if (!bs.book->validate(&violation)) {
                    set_fail(res, RecoveryStatus::InvariantViolated,
                             bs.cursor, bs.instrument_id,
                             violation != nullptr ? violation
                                                  : "book audit failed");
                    break;
                }
                if (bs.snapshot_loaded && bs.snapshot_seq < min_snapshot) {
                    min_snapshot = bs.snapshot_seq;
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

}  // namespace exch
