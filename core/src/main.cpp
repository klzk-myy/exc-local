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
                 "          [-poison-log <path>] [-idle-sleep-ns <ns>]\n",
                 argv0);
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
    std::string ipc_base{exch::SharedMemChannel::kDefaultBase};
    std::string wal_dir = "wal";
    std::string poison_path = "poison_pill.log";
    int64_t idle_sleep_ns = 0;

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

    // --- WAL: wal/{shard}/0.wal (spec §3.4 layout) ----------------------------
    // Fail-closed: no journal, no engine (zero-loss invariant, spec §2.7).
    std::error_code ec;
    const auto shard_dir = std::filesystem::path(wal_dir) / std::to_string(shard);
    std::filesystem::create_directories(shard_dir, ec);
    const std::string wal_path = (shard_dir / "0.wal").string();
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
    exch::PreTradeChecker risk;
    exch::EngineRiskBinding risk_binding{&risk, book.instrument(),
                                         engine.now_ns_ptr()};
    engine.set_risk_hook(&exch::engine_risk_check, &risk_binding);

    exch::ModeManager modes;
    exch::HealthChecker health(modes);
    exch::LeaderElection election(shard);
    exch::RecoveryManager recovery(shard);
    (void)health;
    (void)election;
    (void)recovery;

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
    (void)wal.flush();
    wal.close();
    core_chan.close();
    if (poison_fp != nullptr) std::fclose(poison_fp);
    std::fprintf(stderr, "shard %" PRIu32 " stopped\n", shard);
    return 0;
}
