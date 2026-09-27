#pragma once

// Task 1.3.10 — dedicated low-latency Aeron media driver (aeronmd)
// configuration & launcher, per spec §2.3 / §24 #185.
//
// This TU is deliberately free of Aeron headers: it describes the driver
// profile, renders it as aeronmd `-Dname=value` argv and/or a properties
// file (config/aeron-low-latency.properties is generated from
// AeronDriverConfig::low_latency()), and fork/exec's the vendored driver
// binary (core/third_party/aeron/bin/aeronmd). Property names use Aeron's
// dotted form, which aeronmd maps to AERON_* env vars.
//
// Threading model (spec §2.3): threadingMode=DEDICATED — the driver spawns
// separate conductor, sender and receiver threads, each optionally pinned
// to an isolcpus'd core via aeron.{conductor,sender,receiver}.cpu.affinity.
// Client-side idle strategies (matching thread BusySpin, Go consumers
// Backoff) live in AeronChannel / the Go wrapper, not here.
//
// Fail-closed posture (spec §2.7): validate() rejects out-of-range values
// before launch; launch_aeronmd() only reports Started once cnc.dat appears
// (driver actually initialised), and driver death is surfaced to clients via
// Aeron's own CnC heartbeat -> client driver-timeout -> channel fail-closed.

#include <cstdint>
#include <string>
#include <string_view>
#include <utility>
#include <vector>

#include <sys/types.h>  // pid_t

namespace exch {

// -- Driver threading modes (aeron.threading.mode) ---------------------------
enum class AeronThreadingMode : uint8_t {
    Invoker,        // INVOKER        — caller drives all agents (tests only)
    Shared,         // SHARED         — one thread runs all agents
    SharedNetwork,  // SHARED_NETWORK — conductor separate, sender+receiver shared
    Dedicated,      // DEDICATED      — spec §2.3 profile (default)
};

// -- Driver agent idle strategies (aeron.*.idle.strategy) ---------------------
// Names are the C driver's lowercase spellings ("spin" == BusySpinIdleStrategy,
// "backoff" == BackoffIdleStrategy).
enum class AeronIdleStrategy : uint8_t {
    Backoff,
    BusySpin,
    Yield,
    NoOp,
    Sleeping,
};

[[nodiscard]] std::string_view to_string(AeronThreadingMode m) noexcept;
[[nodiscard]] std::string_view to_string(AeronIdleStrategy s) noexcept;

// -- Channel scaffolding (spec §2.3 "Channels") --------------------------------
// `aeron:ipc` carries core<->gateway traffic inside the NUMA node; `aeron:udp`
// is reserved for multi-node distribution (cluster replication, market data).
// Stream ids/aliases mirror AeronChannelConfig — keep in lockstep.
inline constexpr std::string_view kAeronIpcScheme = "aeron:ipc";
inline constexpr std::string_view kAeronIpcOrdersInUri = "aeron:ipc?alias=orders_in";
inline constexpr std::string_view kAeronIpcOrdersOutUri = "aeron:ipc?alias=orders_out";
inline constexpr int32_t kAeronStreamOrdersIn = 1001;   // gateway -> core
inline constexpr int32_t kAeronStreamOrdersOut = 1002;  // core -> gateway

// Build an `aeron:udp` URI for a unicast endpoint
// (aeron:udp?endpoint=host:port[|interface=name][|ttl=n][|reliable=false]).
// `interface`/`ttl` default to driver/OS behaviour when empty / < 0.
[[nodiscard]] std::string make_udp_endpoint_uri(std::string_view endpoint_host_port,
                                                std::string_view interface = {},
                                                int32_t ttl = -1,
                                                bool reliable = true);

// Build an `aeron:udp` URI for a multicast group (aeron:udp?endpoint=g:g:p|
// interface=<local if addr>). Multicast on this driver uses the control
// address form — endpoint holds the group, interface the local NIC.
[[nodiscard]] std::string make_udp_multicast_uri(std::string_view group_host_port,
                                                 std::string_view interface_addr);

// -- Driver profile -------------------------------------------------------------
struct AeronDriverConfig {
    // Directory holding cnc.dat + log buffers. Empty => driver default
    // (/dev/shm/aeron-<user>) unless $AERON_DIR is set in the child env.
    std::string aeron_dir;
    // Optional extra properties file appended as a positional arg (aeronmd
    // loads it after the -D flags below, so file values win over argv).
    std::string properties_file;

    AeronThreadingMode threading_mode = AeronThreadingMode::Dedicated;

    // spec §2.3 values — do not regress silently.
    uint64_t term_buffer_length = 134'217'728;      // aeron.term.buffer.length
    uint64_t ipc_term_buffer_length = 134'217'728;  // aeron.ipc.term.buffer.length
    uint32_t mtu_length = 1408;                     // aeron.mtu.length
    uint32_t ipc_mtu_length = 1408;                 // aeron.ipc.mtu.length
    uint64_t socket_so_rcvbuf = 16'777'216;         // aeron.socket.so.rcvbuf
    uint64_t socket_so_sndbuf = 16'777'216;         // aeron.socket.so.sndbuf

    // CPU pinning: -1 = unbound. Only set to isolcpus'd cores (see properties
    // file comments); pinning to shared cores amplifies jitter.
    int32_t conductor_cpu_affinity = -1;  // aeron.conductor.cpu.affinity
    int32_t sender_cpu_affinity = -1;     // aeron.sender.cpu.affinity
    int32_t receiver_cpu_affinity = -1;   // aeron.receiver.cpu.affinity

    AeronIdleStrategy conductor_idle = AeronIdleStrategy::Backoff;
    AeronIdleStrategy sender_idle = AeronIdleStrategy::BusySpin;
    AeronIdleStrategy receiver_idle = AeronIdleStrategy::BusySpin;

    bool term_buffer_sparse_file = true;   // aeron.term.buffer.sparse.file
    bool perform_storage_checks = true;    // aeron.perform.storage.checks
    bool dir_warn_if_exists = true;        // aeron.dir.warn.if.exists
    bool dir_delete_on_start = false;      // aeron.dir.delete.on.start
    bool dir_delete_on_shutdown = false;   // aeron.dir.delete.on.shutdown
    bool print_configuration = true;       // aeron.print.configuration
    uint64_t file_page_size = 4096;        // aeron.file.page.size (0 => omit)
    int32_t socket_multicast_ttl = -1;     // aeron.socket.multicast.ttl (<0 => omit)
    uint64_t client_liveness_timeout_ns = 0;   // aeron.client.liveness.timeout (0 => omit)
    uint64_t publication_linger_ns = 0;        // aeron.publication.linger.timeout (0 => omit)

    // The spec §2.3 profile (128MB terms, 1408 MTU, 16MB sockets, DEDICATED,
    // spin sender/receiver, backoff conductor). This is what
    // config/aeron-low-latency.properties renders.
    [[nodiscard]] static AeronDriverConfig low_latency();

    // Range/alignment checks (term power-of-2 in [64K,1G]; mtu 32B-aligned and
    // within (32,65504]; socket bufs >0; affinity >= -1). On failure fills
    // `err` and returns false — callers must refuse to launch (fail closed).
    [[nodiscard]] bool validate(std::string& err) const;

    // Ordered dotted-name/value pairs exactly as aeronmd expects them
    // (aeronmd upcases dots to underscores -> AERON_* env vars).
    [[nodiscard]] std::vector<std::pair<std::string, std::string>>
    to_properties() const;

    // Renders to_properties() as a properties file body (no comments —
    // config/aeron-low-latency.properties is this output plus docs).
    [[nodiscard]] std::string to_properties_text() const;

    // Renders to_properties() as argv for aeronmd: {"-Dname=value", ...}.
    // properties_file (when set) is appended as a trailing positional arg.
    [[nodiscard]] std::vector<std::string> to_aeronmd_argv() const;
};

// -- Launcher -------------------------------------------------------------------
enum class AeronDriverLaunchStatus : uint8_t {
    Started,          // exec'd and cnc.dat observed within the deadline
    AlreadyRunning,   // a live cnc.dat was already present in aeron_dir
    ValidateFailed,   // config failed validate()
    ExecFailed,       // fork/exec failed (child exits before writing cnc.dat
                      // also lands here via the pipe error channel)
    CncTimeout,       // exec'd but cnc.dat never appeared before deadline
};

struct AeronDriverLaunchResult {
    AeronDriverLaunchStatus status = AeronDriverLaunchStatus::ExecFailed;
    pid_t pid = -1;
    std::string aeron_dir;  // resolved dir actually used
    std::string detail;     // human-readable context on non-Started results
};

// Fork/exec `aeronmd_path` with the config's -D args, then poll for
// <aeron_dir>/cnc.dat until `cnc_timeout_ms`. Returns immediately with
// AlreadyRunning when a fresh CnC file is present. The child gets its own
// process group (setsid) so it survives the parent's terminal; SIGINT/SIGTERM
// to the child is aeronmd's graceful-stop path (see request_aeronmd_stop).
[[nodiscard]] AeronDriverLaunchResult
launch_aeronmd(const AeronDriverConfig& cfg, std::string_view aeronmd_path,
               int64_t cnc_timeout_ms = 10'000) noexcept;

// True when <aeron_dir>/cnc.dat exists and is non-empty — cheap liveness gate;
// staleness detection belongs to clients via Aeron's driver-timeout.
[[nodiscard]] bool aeron_cnc_ready(std::string_view aeron_dir) noexcept;

// kill(pid, 0) liveness for the child pid (EPERM still counts as alive).
[[nodiscard]] bool aeron_process_alive(pid_t pid) noexcept;

// Graceful driver stop: aeronmd installs SIGINT/SIGTERM handlers; we send
// SIGINT by default. Returns 0 on successful signal delivery.
[[nodiscard]] int request_aeronmd_stop(pid_t pid, int sig = 2 /*SIGINT*/) noexcept;

// Resolves cfg.aeron_dir -> $AERON_DIR -> /dev/shm/aeron-<user> fallback.
[[nodiscard]] std::string resolve_aeron_dir(std::string_view configured);

}  // namespace exch
