// matching_engine — one process per shard (DESIGN.md: dedicated bare metal,
// single matching thread per shard). Task 1.3.1 scaffold + Task 2.3.7/2.3.19
// wiring: argv -> shm IPC channels -> WAL -> order pool/book -> engine ->
// EnginePump -> EngineLoop (watchdog armed) -> SIGTERM-driven idle loop.
//
// argv: matching_engine [-shard <n>] [-ipc-base <name>] [-wal-dir <dir>]
//                       [-poison-log <path>] [-idle-sleep-ns <ns>]

#include <signal.h>

#include <atomic>
#include <cinttypes>
#include <cstdio>
#include <cstdlib>
#include <cstring>
#include <filesystem>
#include <memory>
#include <type_traits>

#include "book/Order.hpp"
#include "book/OrderBook.hpp"
#include "degradation/ModeManager.hpp"
#include "election/LeaderElection.hpp"
#include "health/HealthChecker.hpp"
#include "ipc/EnginePump.hpp"
#include "ipc/SharedMemChannel.hpp"
#include "matching/EngineLoop.hpp"
#include "matching/IpcPublisher.hpp"
#include "matching/MatchingEngine.hpp"
#include "matching/WalWriter.hpp"
#include "recovery/RecoveryManager.hpp"
#include "recovery/SnapshotStore.hpp"
#include "risk/EngineRiskAdapter.hpp"
#include "risk/PreTradeChecker.hpp"
#include "utils/MemoryPool.hpp"
#include "utils/TimeUtils.hpp"
#include "wal/Wal.hpp"

namespace {

std::atomic<bool> g_stop{false};

void on_term_sig(int /*sig*/) { g_stop.store(true, std::memory_order_release); }

void usage(const char* argv0) {
    std::fprintf(stderr,
                 "usage: %s [-shard <n>] [-ipc-base <name>] [-wal-dir <dir>]\n"
                 "          [-poison-log <path>] [-idle-sleep-ns <ns>]\n"
                 "          [-instrument-id <n>] [-dev-all-accounts]\n"
                 "          [-snap-dir <dir>] [-snapshot-trades <n>]\n"
                 "          [-snapshot-interval-s <s>]\n",
                 argv0);
}

// Dev/soak account provider (Phase-02.5): every account reads ACTIVE with
// effectively unlimited balance, ECP category and T2 KYC. Wired ONLY under
// the explicit -dev-all-accounts flag — production boots fail closed on an
// unbound account store (spec §2.7), which remains the default.
class DevAccountState final : public exch::IAccountState {
public:
    [[nodiscard]] exch::AccountStatus status(
        uint64_t /*account_id*/) const noexcept override {
        return exch::AccountStatus::ACTIVE;
    }
    [[nodiscard]] int64_t available_balance(
        uint64_t /*account_id*/, uint64_t /*instrument_id*/,
        exch::BalanceUnit /*unit*/) const noexcept override {
        return INT64_MAX / 4;
    }
    [[nodiscard]] uint32_t open_position_count(
        uint64_t /*account_id*/) const noexcept override {
        return 0;
    }
    [[nodiscard]] exch::StpMode default_stp_mode(
        uint64_t /*account_id*/) const noexcept override {
        return static_cast<exch::StpMode>(exch::kStpModeUnset);
    }
    [[nodiscard]] exch::ClientCategory client_category(
        uint64_t /*account_id*/) const noexcept override {
        return exch::ClientCategory::ELIGIBLE_COUNTERPARTY;
    }
    [[nodiscard]] exch::KycTier kyc_tier(
        uint64_t /*account_id*/) const noexcept override {
        return exch::KycTier::T2;
    }
};

// Snapshot cadence binding (Phase-02.5 soak finding): the engine's
// on_time_tick tail invokes the hook on the matching thread; the closure
// feeds SnapshotStore::maybe_snapshot the WAL tail cursor + the trades
// delta since the previous tick. Book serialization runs between events
// only when the cadence threshold is met — steady-state cost is two
// compares per tick.
struct SnapshotCtx {
    exch::SnapshotStore* store;
    exch::OrderBook* book;
    exch::Wal* wal;
    uint32_t instrument_id;
    uint64_t last_trades = 0;
    bool store_failed = false;
};

void on_snapshot_tick(void* raw, uint64_t now_ns,
                      uint64_t trades_emitted) noexcept {
    auto* c = static_cast<SnapshotCtx*>(raw);
    const uint64_t delta = trades_emitted - c->last_trades;
    c->last_trades = trades_emitted;
    const exch::SnapshotOutcome oc = c->store->maybe_snapshot(
        *c->book, c->instrument_id, c->wal->tail_seq(), now_ns, delta);
    // Sink I/O failure must not kill the matching loop (journaling is the
    // durability guarantee; the snapshot is a recovery accelerator) — report
    // on the transition only, the next cadence window retries.
    if (oc == exch::SnapshotOutcome::StoreFailed && !c->store_failed) {
        std::fprintf(stderr, "snapshot store failed — cadence retry pending\n");
    }
    c->store_failed = oc == exch::SnapshotOutcome::StoreFailed;
}

bool parse_u32(const char* s, uint32_t* out) {
    if (s == nullptr || *s == '\0' || *s == '-') return false;
    char* end = nullptr;
    const unsigned long v = std::strtoul(s, &end, 10);
    if (end == s || *end != '\0' || v > 0xFFFFul) return false;
    *out = static_cast<uint32_t>(v);
    return true;
}

bool parse_i64(const char* s, int64_t* out) {
    if (s == nullptr || *s == '\0') return false;
    char* end = nullptr;
    const long long v = std::strtoll(s, &end, 10);
    if (end == s || *end != '\0') return false;
    *out = static_cast<int64_t>(v);
    return true;
}

// Structured alert line — sink for EnginePump/EngineLoop reports.
void stderr_alert(void* /*ctx*/, const char* code, const char* detail) {
    std::fprintf(stderr, "[P1] %s: %s\n", code, detail != nullptr ? detail : "");
}

// Poison-pill quarantine log (Task 2.3.19): one hex line per rejected frame,
// append mode. ctx is a FILE* (nullptr => stderr fallback inside the fn).
void file_poison_sink(void* ctx, const uint8_t* data, uint32_t len, const char* why) {
    std::FILE* f = static_cast<std::FILE*>(ctx != nullptr ? ctx : stderr);
    const uint32_t n = len < 64 ? len : 64;
    std::fprintf(f, "poison len=%u why=%s head=", len, why != nullptr ? why : "?");
    for (uint32_t i = 0; i < n; ++i) std::fprintf(f, "%02x", data[i]);
    std::fprintf(f, "\n");
    std::fflush(f);
}

// Watchdog STALL/WARN report — runs on the watchdog thread, so it must not
// touch the WAL or the book (single-writer ownership, wal/Wal.hpp). It only
// emits the operational alert; the L0 response (dirty-flush -> lease release
// -> halt for hot-standby promotion, Task 2.3.19) is wired by the supervisor
// facing this line — systemd sd_notify("WATCHDOG=1") petting goes through the
// service unit rather than this non-systemd build.
void watchdog_report(void* /*ctx*/, exch::Watchdog::Level level, int64_t stale_ns, uint64_t beat) {
    std::fprintf(stderr, "[%s] ENGINE_%s stale_ns=%" PRId64 " beat=%" PRIu64 "\n",
                 level == exch::Watchdog::Level::Stall ? "P0" : "P1",
                 exch::Watchdog::level_name(level), stale_ns, beat);
}

}  // namespace

int main(int argc, char** argv) {
    uint32_t shard = 0;
    uint32_t instrument_id = 7;  // served book instrument (dev soak default)
    std::string ipc_base{exch::SharedMemChannel::kDefaultBase};
    std::string wal_dir = "wal";
    std::string poison_path = "poison_pill.log";
    int64_t idle_sleep_ns = 0;
    bool dev_all_accounts = false;
    std::string snap_dir = "snapshots";
    uint32_t snapshot_trades = 0;      // 0 -> SnapshotPolicy default
    uint32_t snapshot_interval_s = 0;  // 0 -> SnapshotPolicy default

    for (int i = 1; i < argc; ++i) {
        if (std::strcmp(argv[i], "-shard") == 0) {
            if (++i >= argc || !parse_u32(argv[i], &shard)) {
                usage(argv[0]);
                return 2;
            }
        } else if (std::strcmp(argv[i], "-ipc-base") == 0) {
            if (++i >= argc) {
                usage(argv[0]);
                return 2;
            }
            ipc_base = argv[i];
        } else if (std::strcmp(argv[i], "-wal-dir") == 0) {
            if (++i >= argc) {
                usage(argv[0]);
                return 2;
            }
            wal_dir = argv[i];
        } else if (std::strcmp(argv[i], "-poison-log") == 0) {
            if (++i >= argc) {
                usage(argv[0]);
                return 2;
            }
            poison_path = argv[i];
        } else if (std::strcmp(argv[i], "-idle-sleep-ns") == 0) {
            if (++i >= argc || !parse_i64(argv[i], &idle_sleep_ns)) {
                usage(argv[0]);
                return 2;
            }
        } else if (std::strcmp(argv[i], "-instrument-id") == 0) {
            if (++i >= argc || !parse_u32(argv[i], &instrument_id)) {
                usage(argv[0]);
                return 2;
            }
        } else if (std::strcmp(argv[i], "-snap-dir") == 0) {
            if (++i >= argc) {
                usage(argv[0]);
                return 2;
            }
            snap_dir = argv[i];
        } else if (std::strcmp(argv[i], "-snapshot-trades") == 0) {
            if (++i >= argc || !parse_u32(argv[i], &snapshot_trades)) {
                usage(argv[0]);
                return 2;
            }
        } else if (std::strcmp(argv[i], "-snapshot-interval-s") == 0) {
            if (++i >= argc || !parse_u32(argv[i], &snapshot_interval_s)) {
                usage(argv[0]);
                return 2;
            }
        } else if (std::strcmp(argv[i], "-dev-all-accounts") == 0) {
            dev_all_accounts = true;
        } else if (std::strcmp(argv[i], "-h") == 0 || std::strcmp(argv[i], "--help") == 0) {
            usage(argv[0]);
            return 0;
        } else {
            std::fprintf(stderr, "unknown argument: %s\n", argv[i]);
            usage(argv[0]);
            return 2;
        }
    }

    // --- Transport: shm channel pair (Core endpoint) ------------------------
    // open() is authoritative; a transport that cannot attach fails closed.
    exch::SharedMemChannel core_chan(ipc_base, static_cast<uint16_t>(shard),
                                     exch::SharedMemChannel::Endpoint::Core,
                                     /*create=*/true);
    if (!core_chan.open()) {
        std::fprintf(stderr, "FATAL: ipc channel open failed (base=%s shard=%" PRIu32 ")\n",
                     ipc_base.c_str(), shard);
        return 1;
    }

    // --- WAL: wal/{shard}/{seq_base}.wal (spec §3.4 layout) -------------------
    // Fail-closed: no journal, no engine (zero-loss invariant, spec §2.7).
    // Resume at the NEWEST segment — rotation renames to {tail_seq}.wal, so
    // always reopening 0.wal would rewind next_seq into an already-journaled
    // range and trip the next boot's SeqGap check.
    std::error_code ec;
    const auto shard_dir = std::filesystem::path(wal_dir) / std::to_string(shard);
    std::filesystem::create_directories(shard_dir, ec);
    uint64_t resume_base = 0;
    bool have_segment = false;
    for (const auto& de : std::filesystem::directory_iterator(shard_dir, ec)) {
        if (!de.is_regular_file() || de.path().extension() != ".wal") continue;
        const std::string stem = de.path().stem().string();
        char* end = nullptr;
        const unsigned long long base = std::strtoull(stem.c_str(), &end, 10);
        if (end == stem.c_str() || *end != '\0') continue;
        if (!have_segment || base > resume_base) {
            resume_base = base;
            have_segment = true;
        }
    }
    const std::string wal_path =
        (shard_dir / (std::to_string(resume_base) + ".wal")).string();
    exch::Wal wal(wal_path, static_cast<uint16_t>(shard));
    if (wal.open() != exch::WalStatus::Ok) {
        std::fprintf(stderr, "FATAL: wal open failed path=%s errno=%d\n", wal_path.c_str(),
                     wal.last_errno());
        core_chan.close();
        return 1;
    }

    // --- Pre-allocated state: no heap traffic past this point (spec §3.6) ----
    exch::MemoryPool<exch::Order> orders(exch::kOrderPoolCapacity);
    exch::OrderBook book(orders);
    // The shard's served instrument binds the book before any ingress —
    // recovery routes journaled rows through instrument_id, so the binding
    // must precede recover() or every row reads as foreign.
    exch::Instrument served{};
    served.instrument_id = instrument_id;
    served.pip_factor = 10;
    served.pip_size_ticks = 10'000;
    served.tick_size_ticks = 1'000;
    served.lot_size_units = 1;
    served.settlement_cycle = 1;  // T+1 major
    book.set_instrument(served);

    // ==== ENGINE WIRING (Tasks 2.3.2/2.3.3/2.3.4) ====
    // Sinks outlive the engine (declaration order = reverse destruction).
    exch::WalWriter wal_writer(&wal);            // ORDER_*/TRADE journal
    exch::IpcPublisher publisher(&core_chan);    // fills/book fan-out
    // Outbound is journal-first; publisher drops are channel backpressure —
    // EnginePump::note_outbound_drop accounting is wired when the pump owns
    // the outbound path (Phase-03 bridge consumes the same channel).
    exch::MatchingEngine engine(shard, book, orders, &wal_writer, &publisher);

    // In-process pre-trade risk (Task 2.3.3): 14 checks, <10µs, codes surface
    // verbatim as engine last_reject(). now_ns_source reads the engine's
    // TIME_TICK clock — determinism preserved end to end.
    // Explicit dev/soak opt-in: bound provider reports ACTIVE for every
    // account, and the per-account order-rate collar is lifted — soak
    // ingress (50k/s) would otherwise trip the 50/s default. The flag is
    // non-default precisely so production boots keep §2.7 fail-closed.
    exch::RiskConfig risk_cfg{};
    if (dev_all_accounts) {
        risk_cfg.order_rate_per_sec = 10'000'000;
        risk_cfg.order_rate_burst = 10'000'000;
    }
    exch::PreTradeChecker risk{risk_cfg};
    DevAccountState dev_accounts;
    if (dev_all_accounts) {
        risk.bind_accounts(&dev_accounts);
        std::fprintf(stderr, "WARN: -dev-all-accounts active — account "
                             "checks permissive (soak/dev only)\n");
    }
    exch::EngineRiskBinding risk_binding{&risk, book.instrument(),
                                         engine.now_ns_ptr()};
    engine.set_risk_hook(&exch::engine_risk_check, &risk_binding);

    // --- Snapshot sink + cadence (Task 2.3.4; Phase-02.5 finding) -----------
    // Without periodic snapshots recovery replays the WHOLE journal —
    // measured 65s at 3h/21GB on the soak bench, over the <10s AC. The
    // FileSnapshotSink is the Phase-02 durable home (PostgreSQL
    // book_snapshots sink is Phase-04 scope, same interface).
    const auto snap_root =
        std::filesystem::path(snap_dir) / std::to_string(shard);
    exch::FileSnapshotSink snap_sink(snap_root.string(),
                                     static_cast<uint16_t>(shard));
    exch::SnapshotPolicy snap_policy{};
    if (snapshot_trades != 0) snap_policy.trade_interval = snapshot_trades;
    if (snapshot_interval_s != 0) {
        snap_policy.interval_ns =
            static_cast<uint64_t>(snapshot_interval_s) * 1'000'000'000ull;
    }
    exch::SnapshotStore snap_store(snap_sink, snap_policy);
    SnapshotCtx snap_ctx{&snap_store, &book, &wal, instrument_id, 0};
    engine.set_snapshot_hook(&on_snapshot_tick, &snap_ctx);

    // --- Boot recovery (Task 2.3.4 / Phase-02.5 failover benchmark) --------
    // Replay snapshot + WAL tail into the book BEFORE the pump opens the
    // ingress ring: a restarted shard resumes with the exact pre-crash state.
    // Fail-closed — an unverifiable journal halts the boot (spec §2.7/§3.5).
    exch::RecoveryManager recovery(shard, snap_sink);
    {
        const exch::RecoveryResult rr =
            recovery.recover(shard_dir.string(),
                             {{instrument_id, &book, &orders}});
        if (!rr.ok()) {
            std::fprintf(stderr,
                         "FATAL: wal recovery failed status=%d detail=%s\n",
                         static_cast<int>(rr.status), rr.detail);
            wal.close();
            core_chan.close();
            return 1;
        }
        if (rr.max_trade_id > 0) {
            // Post-recovery fills must not reuse journaled trade ids — the
            // dedup ledger would silently drop them on the next restart.
            engine.seed_trade_id(rr.max_trade_id + 1);
        }
        std::fprintf(stderr,
                     "recovery: entries=%llu applied=%llu derived=%llu "
                     "dedup=%llu tail=%llu%s\n",
                     (unsigned long long)rr.entries_replayed,
                     (unsigned long long)rr.mutations_applied,
                     (unsigned long long)rr.trades_derived,
                     (unsigned long long)rr.dedup_skips,
                     (unsigned long long)rr.wal_tail,
                     rr.tail_truncated ? " (torn tail truncated)" : "");
    }

    exch::ModeManager modes;
    exch::HealthChecker health(modes);
    exch::LeaderElection election(shard);
    (void)health;
    (void)election;

    // --- Pump + loop ---------------------------------------------------------
    exch::MatchingEngineIngress ingress(&engine);
    exch::EnginePump pump(&core_chan, &core_chan, &ingress, &orders, &wal);
    pump.set_report_sink(stderr_alert, nullptr);

    std::FILE* poison_fp = std::fopen(poison_path.c_str(), "a");
    pump.set_poison_sink(file_poison_sink, poison_fp);

    exch::EngineLoopConfig lcfg;
    lcfg.idle_sleep_ns = idle_sleep_ns;
    exch::EngineLoop loop(&pump, lcfg);
    loop.set_report_sink(stderr_alert, nullptr);

    ::signal(SIGTERM, on_term_sig);
    ::signal(SIGINT, on_term_sig);

    (void)loop.start_watchdog(watchdog_report, nullptr);

    std::printf("shard %" PRIu32 " ready at %" PRIu64 " ns (ipc=%s wal=%s)\n", shard,
                exch::now_ns(), ipc_base.c_str(), wal_path.c_str());
    std::fflush(stdout);  // readiness line must land even when piped

    loop.run(&g_stop);  // matching thread = main thread; SIGTERM exits cleanly.

    loop.stop_watchdog();
    // Drain snapshot: a graceful stop leaves a fresh base so the next boot
    // replays only the post-snapshot tail instead of the whole journal.
    if (snap_store.force_snapshot(book, instrument_id, wal.tail_seq()) ==
        exch::SnapshotOutcome::Taken) {
        std::fprintf(stderr, "snapshot stored at seq=%llu\n",
                     (unsigned long long)wal.tail_seq());
    }
    (void)wal.flush();
    wal.close();
    core_chan.close();
    if (poison_fp != nullptr) std::fclose(poison_fp);
    std::fprintf(stderr, "shard %" PRIu32 " stopped\n", shard);
    return 0;
}
