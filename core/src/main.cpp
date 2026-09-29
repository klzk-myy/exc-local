// matching_engine — one process per shard (DESIGN.md: dedicated bare metal,
// single matching thread per shard). Task 1.3.1 scaffold + Task 2.3.7/2.3.19
// wiring: argv -> shm IPC channels -> WAL -> order pool/book -> engine ->
// EnginePump -> EngineLoop (watchdog armed) -> SIGTERM-driven idle loop.
//
// argv: matching_engine [-shard <n>] [-ipc-base <name>] [-wal-dir <dir>]
//                       [-poison-log <path>] [-idle-sleep-ns <ns>]

#include <errno.h>
#include <fcntl.h>
#include <signal.h>
#include <sys/file.h>
#include <unistd.h>

#include <atomic>
#include <chrono>
#include <cinttypes>
#include <cstdio>
#include <cstdlib>
#include <cstring>
#include <filesystem>
#include <memory>
#include <thread>
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
#include "recovery/SnapshotManager.hpp"
#include "recovery/SnapshotStore.hpp"
#include "redis/RespClient.hpp"
#include "risk/EngineRiskAdapter.hpp"
#include "risk/InstrumentFeed.hpp"
#include "risk/InstrumentFeedRefresher.hpp"
#include "risk/PriceOracleFeed.hpp"
#include "risk/PreTradeChecker.hpp"
#include "risk/SuspensionFlags.hpp"
#include "risk/SuspensionRefresher.hpp"
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
                 "          [-snapshot-interval-s <s>] [-follower]\n"
                 "          [-report-log <path>]\n"
                 "          [-redis <host:port>] [-halt-poll-ms <ms>]\n"
                 "          [-symbol <SYM>] [-feed-poll-ms <ms>]\n",
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
    exch::SnapshotManager* mgr;   // Task 4.3.1 — PG persist notify + WAL trim
    uint32_t instrument_id;
    uint64_t last_trades = 0;
    bool store_failed = false;
    bool serialize_failed = false;
};

void on_snapshot_tick(void* raw, uint64_t now_ns,
                      uint64_t trades_emitted) noexcept {
    auto* c = static_cast<SnapshotCtx*>(raw);
    const uint64_t delta = trades_emitted - c->last_trades;
    c->last_trades = trades_emitted;
    const exch::SnapshotOutcome oc = c->store->maybe_snapshot(
        *c->book, c->instrument_id, c->wal->tail_seq(), now_ns, delta);
    // Task 4.3.1: a stored snapshot is handed to the Go recovery service
    // (compact SnapReadyMsg on the shm ring — the file is the transport),
    // and confirmed snapshots trim sealed WAL segments. Both are cold-path,
    // non-blocking, and their failures degrade safely (dir scan catches
    // missed notifies; un-acked WAL simply stays untrimmed).
    if (oc == exch::SnapshotOutcome::Taken && c->mgr != nullptr) {
        (void)c->mgr->notify_stored(c->instrument_id,
                                    c->store->last_snapshot_seq());
    }
    if (c->mgr != nullptr) {
        c->mgr->poll_acks(*c->wal);
    }
    // Snapshot failure must not kill the matching loop (journaling is the
    // durability guarantee; the snapshot is a recovery accelerator) — but a
    // persistent failure MUST be observable: report on the transition only,
    // the next cadence window retries.
    if (oc == exch::SnapshotOutcome::StoreFailed && !c->store_failed) {
        std::fprintf(stderr, "snapshot store failed — cadence retry pending\n");
    }
    if (oc == exch::SnapshotOutcome::SerializeFailed && !c->serialize_failed) {
        std::fprintf(stderr,
                     "snapshot serialize failed — book exceeds encodable "
                     "shape; recovery degrades to full replay\n");
    }
    c->store_failed = oc == exch::SnapshotOutcome::StoreFailed;
    c->serialize_failed = oc == exch::SnapshotOutcome::SerializeFailed;
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
    bool follower = false;             // warm standby: skip snapshot/replay
    // recovery_reports JSONL sink (migration 065; the Go wal-recovery CLI's
    // persist-reports drains it into PostgreSQL — the core never speaks PG).
    std::string report_log;
    // Phase-11 kill-switch (Tasks 11.3.4/11.3.8/11.3.12): `-redis` binds
    // the halt-flag lattice to a control-path Redis poll; absent = the
    // suspension seam stays unbound and enforcement lives on the Go
    // admission gates only.
    std::string redis_addr;
    uint32_t halt_poll_ms = 50;        // Redis halt:* refresh cadence
    // Phase-15 (Tasks 15.3.3/15.3.4/15.3.6/15.3.10): `-symbol` names the
    // served instrument for the per-symbol control keys
    // (instrument:status/auction:{symbol}); with -redis it binds the
    // InstrumentFeed — the matching thread then enforces lifecycle status,
    // 24/5 market hours and the reopening CALL armed key. Without -symbol
    // the feed stays unbound and enforcement lives on the Go gates only
    // (same unwired-seam convention as the halt lattice).
    std::string symbol;
    uint32_t feed_poll_ms = 250;       // control-key refresh cadence

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
        } else if (std::strcmp(argv[i], "-follower") == 0) {
            follower = true;
        } else if (std::strcmp(argv[i], "-report-log") == 0) {
            if (++i >= argc) {
                usage(argv[0]);
                return 2;
            }
            report_log = argv[i];
        } else if (std::strcmp(argv[i], "-redis") == 0) {
            if (++i >= argc) {
                usage(argv[0]);
                return 2;
            }
            redis_addr = argv[i];
        } else if (std::strcmp(argv[i], "-halt-poll-ms") == 0) {
            if (++i >= argc || !parse_u32(argv[i], &halt_poll_ms)) {
                usage(argv[0]);
                return 2;
            }
            if (halt_poll_ms == 0) halt_poll_ms = 50;
        } else if (std::strcmp(argv[i], "-symbol") == 0) {
            if (++i >= argc) {
                usage(argv[0]);
                return 2;
            }
            symbol = argv[i];
        } else if (std::strcmp(argv[i], "-feed-poll-ms") == 0) {
            if (++i >= argc || !parse_u32(argv[i], &feed_poll_ms)) {
                usage(argv[0]);
                return 2;
            }
            if (feed_poll_ms == 0) feed_poll_ms = 250;
        } else if (std::strcmp(argv[i], "-h") == 0 || std::strcmp(argv[i], "--help") == 0) {
            usage(argv[0]);
            return 0;
        } else {
            std::fprintf(stderr, "unknown argument: %s\n", argv[i]);
            usage(argv[0]);
            return 2;
        }
    }

    // --- Single-writer shard lock (Task 4.5.3.1 chaos finding, spec §2.7) ---
    // Two engine processes on the same shard WAL dir / shm rings would
    // dual-write the journal (neither shm_open nor Wal::open carried an
    // exclusivity guard — second-instance startup was a silent split-brain).
    // Fail closed BEFORE touching either resource: flock(LOCK_EX|LOCK_NB) on
    // <wal-dir>/<shard>/engine.lock. The kernel releases the lock on process
    // death, so a crashed leader's lock is never sticky; flock (not an
    // O_EXCL pidfile) is the authority — the pid payload is diagnostic only.
    const auto shard_dir = std::filesystem::path(wal_dir) / std::to_string(shard);
    {
        std::error_code mk_ec;
        std::filesystem::create_directories(shard_dir, mk_ec);
    }
    const std::string lock_path = (shard_dir / "engine.lock").string();
    const int lock_fd =
        ::open(lock_path.c_str(), O_RDWR | O_CREAT | O_CLOEXEC, 0644);
    if (lock_fd < 0 ||
        ::flock(lock_fd, LOCK_EX | LOCK_NB) != 0) {
        std::fprintf(stderr,
                     "FATAL: shard lock unavailable path=%s errno=%d — "
                     "another engine owns this shard WAL dir; refusing to "
                     "start (fail-closed single-writer guard)\n",
                     lock_path.c_str(), errno);
        return 1;
    }
    // Record the owner for operators (pid + ipc base). Not the guard itself —
    // the held flock is; this file's contents may be stale-read only.
    {
        if (::ftruncate(lock_fd, 0) == 0) {
            char owner[160];
            const int n =
                std::snprintf(owner, sizeof(owner),
                              "pid=%d ipc_base=%s shard=%u\n",
                              static_cast<int>(::getpid()), ipc_base.c_str(),
                              shard);
            if (n > 0 &&
                ::write(lock_fd, owner, static_cast<std::size_t>(n)) < 0) {
                // Diagnostic write only — the held flock is the guard.
            }
        }
    }
    // lock_fd intentionally stays open for the process lifetime — the kernel
    // drops the flock on exit/crash.

    // --- Transport: shm channel pair (Core endpoint) ------------------------
    // open() is authoritative; a transport that cannot attach fails closed.
    // Phase-08 Task 8.3.3 — 2048B slots: a spec-§10.2 top-20/side
    // BookSnapshot serializes to ~1.3-1.5KB, so the 1024B default could
    // never carry a conforming L2 frame (every deep-book publish dropped
    // at send). The geometry applies to both rings; the extra stride on
    // _in is harmless (orders are ~100B) and attaching Go endpoints adopt
    // the size from the ring header.
    exch::SharedMemChannel core_chan(ipc_base, static_cast<uint16_t>(shard),
                                     exch::SharedMemChannel::Endpoint::Core,
                                     /*create=*/true,
                                     exch::ShmRing::kDefaultCapacity,
                                     /*slot_payload=*/2048);
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
    // shard_dir was created + locked above (single-writer guard).
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
    if (!symbol.empty()) {
        std::snprintf(served.symbol, sizeof(served.symbol), "%s",
                      symbol.c_str());
    }
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

    // --- Kill-switch suspension lattice (Task 11.3.4 step 4; 11.3.8/12) --
    // `-redis host:port` binds check 0: a control thread polls `halt:*`
    // every -halt-poll-ms (default 50ms) into an immutable snapshot the
    // matching thread reads per order. Bound-but-unverifiable rejects
    // TRADING_HALTED (fail closed); an unwired seam leaves enforcement
    // to the Go admission layer.
    exch::SuspensionFlags susp_flags;
    std::unique_ptr<exch::RespClient> susp_redis;
    std::unique_ptr<exch::SuspensionRefresher> susp_refresh;
    std::atomic<bool> susp_stop{false};
    std::thread susp_thread;
    if (!redis_addr.empty()) {
        exch::RespClientConfig rcfg{};
        const std::size_t colon = redis_addr.rfind(':');
        if (colon == std::string::npos || colon == 0) {
            std::fprintf(stderr,
                         "FATAL: -redis expects host:port (got %s)\n",
                         redis_addr.c_str());
            wal.close();
            core_chan.close();
            return 1;
        }
        rcfg.host = redis_addr.substr(0, colon);
        rcfg.port = static_cast<uint16_t>(
            std::strtoul(redis_addr.c_str() + colon + 1, nullptr, 10));
        susp_redis = std::make_unique<exch::RespClient>(rcfg);
        susp_refresh =
            std::make_unique<exch::SuspensionRefresher>(susp_redis.get());
        risk.bind_suspensions(&susp_flags);
        // Synchronous first poll before the ingress ring opens — orders
        // admitted after "ready" see a verified flag state, not the
        // fail-closed startup default, when Redis is reachable.
        if (!susp_redis->connect() || !susp_refresh->refresh(&susp_flags)) {
            std::fprintf(stderr,
                         "WARN: halt:* flag poll unreachable at boot — "
                         "check 0 fails closed (TRADING_HALTED) until the "
                         "poll thread lands a clean read\n");
        }
        exch::SuspensionRefresher* refresher = susp_refresh.get();
        exch::SuspensionFlags* flags = &susp_flags;
        std::atomic<bool>* stop = &susp_stop;
        const auto cadence = std::chrono::milliseconds(halt_poll_ms);
        susp_thread = std::thread([refresher, flags, stop, cadence]() {
            while (!stop->load(std::memory_order_acquire)) {
                refresher->refresh(flags);
                std::this_thread::sleep_for(cadence);
            }
        });
    }

    // --- Phase-15 instrument feed (Tasks 15.3.3/15.3.4/15.3.6/15.3.10) ----
    // With -redis + -symbol the engine binds a dedicated control poll of
    // instrument:status/auction:{symbol} + market:hours. The matching
    // thread reads the immutable snapshot only — every Redis round trip is
    // on this thread; a dead/unparseable poll marks the feed unverifiable
    // and admission fails closed until a clean poll lands.
    exch::InstrumentFeed instr_feed;
    std::unique_ptr<exch::RespClient> feed_redis;
    std::unique_ptr<exch::InstrumentFeedRefresher> feed_refresh;
    std::atomic<bool> feed_stop{false};
    std::thread feed_thread;
    if (!redis_addr.empty() && !symbol.empty()) {
        exch::RespClientConfig fcfg{};
        const std::size_t colon = redis_addr.rfind(':');
        fcfg.host = redis_addr.substr(0, colon);
        fcfg.port = static_cast<uint16_t>(
            std::strtoul(redis_addr.c_str() + colon + 1, nullptr, 10));
        feed_redis = std::make_unique<exch::RespClient>(fcfg);
        feed_refresh =
            std::make_unique<exch::InstrumentFeedRefresher>(
                feed_redis.get(), symbol);
        engine.bind_instrument_feed(&instr_feed);
        // Synchronous first poll — bound-but-never-verified fails closed;
        // one clean poll up front makes the boot's verdict useful.
        if (!feed_redis->connect() ||
            !feed_refresh->refresh(&instr_feed)) {
            std::fprintf(stderr,
                         "WARN: instrument feed poll unreachable at boot — "
                         "lifecycle/hours gates fail closed until the poll "
                         "thread lands a clean read\n");
        }
        exch::InstrumentFeedRefresher* fr = feed_refresh.get();
        exch::InstrumentFeed* feed = &instr_feed;
        std::atomic<bool>* fstop = &feed_stop;
        const auto cadence = std::chrono::milliseconds(feed_poll_ms);
        feed_thread = std::thread([fr, feed, fstop, cadence]() {
            while (!fstop->load(std::memory_order_acquire)) {
                (void)fr->refresh(feed);
                std::this_thread::sleep_for(cadence);
            }
        });
    } else if (!symbol.empty()) {
        std::fprintf(stderr,
                     "WARN: -symbol given without -redis — instrument feed "
                     "unbound (lifecycle/hours enforced by the Go gates)\n");
    }

    // --- Phase-16 mark/index oracle feed (Tasks 16.3.17/16.3.22) ----------
    // With -redis + -symbol the engine binds a dedicated control poll of
    // oracle:mark|index:{symbol}[:ts]. MARK_PRICE/INDEX_PRICE conditional
    // triggers evaluate only against this snapshot plus the 5s staleness
    // gate (MatchingEngine::kOracleStaleNs); unbound or unverifiable means
    // those sources freeze fail-closed while LAST_PRICE stays live.
    exch::PriceOracleFeed oracle_feed;
    std::unique_ptr<exch::RespClient> oracle_redis;
    std::unique_ptr<exch::PriceOracleFeedRefresher> oracle_refresh;
    std::atomic<bool> oracle_stop{false};
    std::thread oracle_thread;
    if (!redis_addr.empty() && !symbol.empty()) {
        exch::RespClientConfig ocfg{};
        const std::size_t colon = redis_addr.rfind(':');
        ocfg.host = redis_addr.substr(0, colon);
        ocfg.port = static_cast<uint16_t>(
            std::strtoul(redis_addr.c_str() + colon + 1, nullptr, 10));
        oracle_redis = std::make_unique<exch::RespClient>(ocfg);
        std::string oracle_sym;
        oracle_refresh =
            std::make_unique<exch::PriceOracleFeedRefresher>(
                oracle_redis.get(), symbol, &oracle_sym);
        engine.bind_oracle_feed(&oracle_feed);
        if (!oracle_redis->connect() ||
            !oracle_refresh->refresh(&oracle_feed)) {
            std::fprintf(stderr,
                         "WARN: oracle feed poll unreachable at boot — "
                         "MARK/INDEX conditional sources freeze until the "
                         "poll thread lands a clean read\n");
        }
        exch::PriceOracleFeedRefresher* orf = oracle_refresh.get();
        exch::PriceOracleFeed* ofeed = &oracle_feed;
        std::atomic<bool>* ostop = &oracle_stop;
        const auto ocadence = std::chrono::milliseconds(feed_poll_ms);
        oracle_thread = std::thread([orf, ofeed, ostop, ocadence]() {
            while (!ostop->load(std::memory_order_acquire)) {
                (void)orf->refresh(ofeed);
                std::this_thread::sleep_for(ocadence);
            }
        });
    }

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

    // Task 4.3.1 — PostgreSQL snapshot persistence seam. The snap files
    // remain the bulk transport (shared dir); the ring pair carries only
    // the compact ready/ack descriptors:
    //   "{base}_{shard}_snap"      core -> Go   SnapReadyMsg
    //   "{base}_{shard}_snap_ack"  Go -> core   SnapAckMsg (trim gate)
    exch::SnapshotManager snap_mgr(static_cast<uint16_t>(shard), snap_sink);
    snap_mgr.open(exch::snap_ready_name(ipc_base,
                                        static_cast<uint16_t>(shard)),
                  exch::snap_ack_name(ipc_base, static_cast<uint16_t>(shard)));
    if (!snap_mgr.notify_open() || !snap_mgr.ack_open()) {
        std::fprintf(stderr,
                     "WARN: snapshot PG seam degraded (notify=%d ack=%d) — "
                     "Go recovery dir-scan still persists snapshots; "
                     "WAL trim idles until ack ring attaches\n",
                     snap_mgr.notify_open() ? 1 : 0,
                     snap_mgr.ack_open() ? 1 : 0);
    }
    SnapshotCtx snap_ctx{&snap_store, &book, &wal, &snap_mgr, instrument_id, 0};
    engine.set_snapshot_hook(&on_snapshot_tick, &snap_ctx);

    // --- Boot recovery (Task 2.3.4 / Phase-02.5 failover benchmark) --------
    // Replay snapshot + WAL tail into the book BEFORE the pump opens the
    // ingress ring: a restarted shard resumes with the exact pre-crash state.
    // Fail-closed — an unverifiable journal halts the boot (spec §2.7/§3.5).
    // Task 4.3.5 graduated ladder: Level-1 torn-tail CRC repair + replay →
    // Level-2 snapshot rebase → Level-3 fail-closed halt (recovery_reports
    // JSONL row + ALERT_P1, MarketDataOnly pinned).
    exch::ModeManager modes;
    if (follower) {
        // Warm-standby follower (Task 4.3.5): never treats its local WAL as
        // authoritative — no snapshot load, no replay; book starts empty
        // and leader output feeds it via the Phase-06 transport seam.
        std::fprintf(stderr,
                     "exchange_engine: follower mode — snapshot load and WAL "
                     "replay skipped; book is empty until the leader-output "
                     "seam binds (no trading traffic before promotion)\n");
    } else {
        exch::RecoveryManager recovery(shard, snap_sink);
        const exch::RecoveryLadderResult lad = recovery.recover_ladder(
            shard_dir.string(), {{instrument_id, &book, &orders}}, report_log);
        const exch::RecoveryResult& rr = lad.result;
        if (lad.outcome == exch::RecoveryOutcome::HALTED) {
            // Level 3 — the ladder already appended the WAL_RECOVERY_HALT
            // row and emitted ALERT_P1. Pin MarketDataOnly and stop before
            // any traffic can reach the book (fail-closed, spec §2.7).
            modes.set_mode(exch::DegradationMode::MarketDataOnly,
                           "wal recovery halt — unrecoverable divergence");
            std::fprintf(stderr,
                         "FATAL: WAL_RECOVERY_HALT status=%d "
                         "first_divergent_seq=%llu instrument=%u "
                         "detail=%s runbook=docs/runbooks/wal-recovery-halt.md\n",
                         static_cast<int>(rr.status),
                         (unsigned long long)rr.first_divergent_seq,
                         rr.detail_instrument, rr.detail);
            wal.close();
            core_chan.close();
            return 1;
        }
        if (lad.outcome != exch::RecoveryOutcome::CLEAN) {
            // Repaired or rebased — Maintenance-resume probe gate (spec §3.5
            // step a): prove the recovered engine executes a full order
            // lifecycle on a synthetic order before traffic may reopen.
            modes.set_mode(exch::DegradationMode::Maintenance,
                           "recovery repair — synthetic probe gate");
            const uint64_t base_orders = book.live_orders();
            const char* violation = nullptr;
            bool probe_ok = book.validate(&violation);
            if (probe_ok) {
                exch::Order* p = orders.alloc();
                if (p != nullptr) {
                    *p = exch::Order{};
                    p->id = 0xFFFFFFFFFFFFFE00ULL;  // probe sentinel id
                    p->account_id = 1;
                    p->side = exch::Side::BUY;
                    p->type = exch::OrderType::LIMIT;
                    p->tif = exch::TimeInForce::GTC;
                    p->price_ticks = 1;  // far off any real touch
                    p->qty_units = 1;
                    p->timestamp_ns = exch::now_ns();
                    exch::OrderAux aux{};
                    engine.on_order_received(p, aux);
                    engine.on_cancel_received(p->id, p->account_id);
                    const char* v2 = nullptr;
                    probe_ok = book.validate(&v2) &&
                               book.live_orders() == base_orders;
                    if (!probe_ok && v2 != nullptr) violation = v2;
                } else {
                    probe_ok = false;
                }
            }
            if (!probe_ok) {
                modes.set_mode(exch::DegradationMode::MarketDataOnly,
                               "recovery probe failed");
                std::fprintf(stderr,
                             "ALERT_P1 WAL_RECOVERY_HALT shard=%" PRIu32
                             " detail=\"post-repair probe failed: %s\" "
                             "runbook=docs/runbooks/wal-recovery-halt.md\n",
                             shard,
                             violation != nullptr ? violation
                                                  : "probe alloc/exhausted");
                wal.close();
                core_chan.close();
                return 1;
            }
            modes.set_mode(exch::DegradationMode::Normal,
                           "recovery probe passed — traffic reopened");
        }
        if (rr.max_trade_id > 0) {
            // Post-recovery fills must not reuse journaled trade ids — the
            // dedup ledger would silently drop them on the next restart.
            engine.seed_trade_id(rr.max_trade_id + 1);
        }
        // Phase-15 — adopt the replayed auction/lifecycle position into the
        // live engine BEFORE the ingress ring opens: an armed CALL resumes
        // accumulating toward its journaled deadline, a journaled
        // quarantine keeps the book halted, and the consumed-auction
        // dedupe ledger blocks re-entry into a completed CALL. Parked
        // nodes live in `orders` (the binding pool — shared with the live
        // engine), so adoption moves ownership, not memory.
        if (const auto* ra =
                recovery.recovered_auction_state(instrument_id)) {
            engine.adopt_auction_state(ra->phase, ra->auction_id,
                                       ra->deadline_ns, ra->extensions,
                                       ra->awaiting, ra->last_completed,
                                       ra->quarantined, ra->quarantine_code,
                                       ra->parked_head, ra->parked_count);
            if (ra->phase != exch::MatchingEngine::kAuctionPhaseNone ||
                ra->quarantined) {
                std::fprintf(stderr,
                             "recovery: auction state adopted — phase=%u "
                             "auction_id=%lld deadline=%lld ext=%u "
                             "quarantined=%d parked=%u\n",
                             static_cast<unsigned>(ra->phase),
                             (long long)ra->auction_id,
                             (long long)ra->deadline_ns,
                             static_cast<unsigned>(ra->extensions),
                             ra->quarantined ? 1 : 0, ra->parked_count);
            }
        }
        std::fprintf(stderr,
                     "recovery: level=%u entries=%llu applied=%llu "
                     "derived=%llu dedup=%llu covered_gap_seqs=%llu "
                     "tail=%llu%s%s\n",
                     static_cast<unsigned>(lad.level),
                     (unsigned long long)rr.entries_replayed,
                     (unsigned long long)rr.mutations_applied,
                     (unsigned long long)rr.trades_derived,
                     (unsigned long long)rr.dedup_skips,
                     (unsigned long long)rr.covered_gap_seqs,
                     (unsigned long long)rr.wal_tail,
                     rr.tail_truncated ? " (torn tail truncated)" : "",
                     rr.snapshot_fallback ? " (snapshot fallback)" : "");
    }

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

    // Stop the control-path poll threads before their targets leave scope.
    susp_stop.store(true, std::memory_order_release);
    feed_stop.store(true, std::memory_order_release);
    oracle_stop.store(true, std::memory_order_release);
    if (susp_thread.joinable()) susp_thread.join();
    if (feed_thread.joinable()) feed_thread.join();
    if (oracle_thread.joinable()) oracle_thread.join();

    loop.stop_watchdog();
    // Drain snapshot: a graceful stop leaves a fresh base so the next boot
    // replays only the post-snapshot tail instead of the whole journal.
    if (snap_store.force_snapshot(book, instrument_id, wal.tail_seq()) ==
        exch::SnapshotOutcome::Taken) {
        std::fprintf(stderr, "snapshot stored at seq=%llu\n",
                     (unsigned long long)wal.tail_seq());
        (void)snap_mgr.notify_stored(instrument_id,
                                     snap_store.last_snapshot_seq());
    }
    // Last ack drain before the WAL closes: a confirmation that arrived
    // during the final snapshot still gets its trim.
    (void)snap_mgr.poll_acks(wal);
    snap_mgr.close();
    (void)wal.flush();
    wal.close();
    core_chan.close();
    if (poison_fp != nullptr) std::fclose(poison_fp);
    std::fprintf(stderr, "shard %" PRIu32 " stopped\n", shard);
    return 0;
}
