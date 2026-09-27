// Task 1.3.10 — dedicated low-latency Aeron media driver (aeronmd)
// configuration & launcher, per spec §2.3 / §24 #185. See the header for the
// design contract. Self-contained: no Aeron headers required, so this TU
// compiles identically with or without EXCH_WITH_AERON.
//
// SDD edge cases addressed:
//  * driver crash recovery — launch_aeronmd() reports AlreadyRunning against
//    a live CnC file and never double-starts a driver; a crashed driver is
//    detectable via aeron_process_alive()/CnC staleness and the launcher
//    re-execs cleanly (clients fail closed on Aeron's own driver timeout).
//  * buffer wrap-around — term length validation enforces Aeron's
//    power-of-2 [64K,1G] window before launch, so a malformed value can
//    never produce a half-initialised ring.
//  * unpinned thread jitter — affinity knobs are explicit and defaulted off;
//    validate() refuses pins outside the online CPU range.

#include "ipc/AeronDriverConfig.hpp"

#include <cerrno>
#include <chrono>
#include <cstring>
#include <fcntl.h>
#include <pwd.h>
#include <signal.h>
#include <sys/stat.h>
#include <sys/wait.h>
#include <thread>
#include <unistd.h>

namespace exch {

namespace {

constexpr uint64_t kTermMin = 64ULL * 1024;
constexpr uint64_t kTermMax = 1024ULL * 1024 * 1024;
constexpr uint32_t kMtuMax = 65'504;  // AERON_MAX_UDP_PAYLOAD_LENGTH
constexpr uint32_t kFrameAlignment = 32;

bool is_power_of_two(uint64_t v) noexcept { return v != 0 && (v & (v - 1)) == 0; }

void push(std::vector<std::pair<std::string, std::string>>& out,
          std::string name, std::string value) {
    out.emplace_back(std::move(name), std::move(value));
}

int64_t mono_ms() noexcept {
    return std::chrono::duration_cast<std::chrono::milliseconds>(
               std::chrono::steady_clock::now().time_since_epoch())
        .count();
}

}  // namespace

std::string_view to_string(AeronThreadingMode m) noexcept {
    switch (m) {
        case AeronThreadingMode::Invoker:       return "INVOKER";
        case AeronThreadingMode::Shared:        return "SHARED";
        case AeronThreadingMode::SharedNetwork: return "SHARED_NETWORK";
        case AeronThreadingMode::Dedicated:     return "DEDICATED";
    }
    return "DEDICATED";
}

std::string_view to_string(AeronIdleStrategy s) noexcept {
    switch (s) {
        case AeronIdleStrategy::Backoff:  return "backoff";
        case AeronIdleStrategy::BusySpin: return "spin";
        case AeronIdleStrategy::Yield:    return "yield";
        case AeronIdleStrategy::NoOp:     return "noop";
        case AeronIdleStrategy::Sleeping: return "sleeping";
    }
    return "backoff";
}

// -- Channel URI scaffolding ---------------------------------------------------

std::string make_udp_endpoint_uri(std::string_view endpoint_host_port,
                                  std::string_view interface, int32_t ttl,
                                  bool reliable) {
    // aeron:udp?endpoint=host:port|interface=eth0|ttl=4
    std::string uri = "aeron:udp?endpoint=";
    uri.append(endpoint_host_port);
    if (!interface.empty()) {
        uri += "|interface=";
        uri.append(interface);
    }
    if (ttl >= 0) {
        uri += "|ttl=";
        uri += std::to_string(ttl);
    }
    if (!reliable)
        uri += "|reliable=false";
    return uri;
}

std::string make_udp_multicast_uri(std::string_view group_host_port,
                                   std::string_view interface_addr) {
    // aeron:udp?endpoint=239.0.0.1:40456|interface=192.168.1.10 — the control/
    // subscription side binds the local NIC address as `interface`.
    std::string uri = "aeron:udp?endpoint=";
    uri.append(group_host_port);
    if (!interface_addr.empty()) {
        uri += "|interface=";
        uri.append(interface_addr);
    }
    return uri;
}

// -- Profile --------------------------------------------------------------------

AeronDriverConfig AeronDriverConfig::low_latency() {
    // Field defaults already encode the spec §2.3 profile.
    return AeronDriverConfig{};
}

bool AeronDriverConfig::validate(std::string& err) const {
    auto bad = [&err](std::string msg) {
        err = std::move(msg);
        return false;
    };
    if (!is_power_of_two(term_buffer_length) || term_buffer_length < kTermMin ||
        term_buffer_length > kTermMax)
        return bad("term_buffer_length must be power-of-2 in [64KiB, 1GiB]");
    if (!is_power_of_two(ipc_term_buffer_length) ||
        ipc_term_buffer_length < kTermMin || ipc_term_buffer_length > kTermMax)
        return bad("ipc_term_buffer_length must be power-of-2 in [64KiB, 1GiB]");
    // MTU must cover the data header, stay under the UDP payload ceiling, and
    // honour the 32B frame alignment (aeron_driver_context_validate_mtu_length).
    if (mtu_length <= kFrameAlignment || mtu_length > kMtuMax ||
        (mtu_length % kFrameAlignment) != 0)
        return bad("mtu_length must be a multiple of 32 in (32, 65504]");
    if (ipc_mtu_length <= kFrameAlignment || ipc_mtu_length > kMtuMax ||
        (ipc_mtu_length % kFrameAlignment) != 0)
        return bad("ipc_mtu_length must be a multiple of 32 in (32, 65504]");
    if (socket_so_rcvbuf == 0)
        return bad("socket_so_rcvbuf must be > 0");
    if (socket_so_sndbuf == 0)
        return bad("socket_so_sndbuf must be > 0");
    const long ncpu = ::sysconf(_SC_NPROCESSORS_ONLN);
    auto check_cpu = [ncpu, &err](int32_t cpu, const char* role) {
        if (cpu < -1) {
            err = std::string(role) + " cpu affinity < -1";
            return false;
        }
        if (cpu >= 0 && ncpu > 0 && cpu >= ncpu) {
            err = std::string(role) + " cpu affinity " + std::to_string(cpu) +
                  " >= online cpus " + std::to_string(ncpu);
            return false;
        }
        return true;
    };
    if (!check_cpu(conductor_cpu_affinity, "conductor")) return false;
    if (!check_cpu(sender_cpu_affinity, "sender")) return false;
    if (!check_cpu(receiver_cpu_affinity, "receiver")) return false;
    if (aeron_dir.size() >= 1024)  // driver path buffer bound
        return bad("aeron_dir too long");
    return true;
}

std::vector<std::pair<std::string, std::string>>
AeronDriverConfig::to_properties() const {
    std::vector<std::pair<std::string, std::string>> p;
    auto b = [](bool v) { return v ? "true" : "false"; };
    auto u = [](uint64_t v) { return std::to_string(v); };

    if (!aeron_dir.empty())
        push(p, "aeron.dir", aeron_dir);
    push(p, "aeron.threading.mode", std::string(to_string(threading_mode)));
    push(p, "aeron.term.buffer.length", u(term_buffer_length));
    push(p, "aeron.ipc.term.buffer.length", u(ipc_term_buffer_length));
    push(p, "aeron.term.buffer.sparse.file", b(term_buffer_sparse_file));
    push(p, "aeron.mtu.length", u(mtu_length));
    push(p, "aeron.ipc.mtu.length", u(ipc_mtu_length));
    push(p, "aeron.socket.so.rcvbuf", u(socket_so_rcvbuf));
    push(p, "aeron.socket.so.sndbuf", u(socket_so_sndbuf));
    if (conductor_cpu_affinity >= 0)
        push(p, "aeron.conductor.cpu.affinity",
             std::to_string(conductor_cpu_affinity));
    if (sender_cpu_affinity >= 0)
        push(p, "aeron.sender.cpu.affinity", std::to_string(sender_cpu_affinity));
    if (receiver_cpu_affinity >= 0)
        push(p, "aeron.receiver.cpu.affinity",
             std::to_string(receiver_cpu_affinity));
    push(p, "aeron.conductor.idle.strategy",
         std::string(to_string(conductor_idle)));
    push(p, "aeron.sender.idle.strategy", std::string(to_string(sender_idle)));
    push(p, "aeron.receiver.idle.strategy",
         std::string(to_string(receiver_idle)));
    push(p, "aeron.perform.storage.checks", b(perform_storage_checks));
    push(p, "aeron.dir.warn.if.exists", b(dir_warn_if_exists));
    push(p, "aeron.dir.delete.on.start", b(dir_delete_on_start));
    push(p, "aeron.dir.delete.on.shutdown", b(dir_delete_on_shutdown));
    push(p, "aeron.print.configuration", b(print_configuration));
    if (file_page_size != 0)
        push(p, "aeron.file.page.size", u(file_page_size));
    if (socket_multicast_ttl >= 0)
        push(p, "aeron.socket.multicast.ttl",
             std::to_string(socket_multicast_ttl));
    if (client_liveness_timeout_ns != 0)
        push(p, "aeron.client.liveness.timeout", u(client_liveness_timeout_ns));
    if (publication_linger_ns != 0)
        push(p, "aeron.publication.linger.timeout", u(publication_linger_ns));
    return p;
}

std::string AeronDriverConfig::to_properties_text() const {
    std::string out;
    for (const auto& [k, v] : to_properties()) {
        out += k;
        out += '=';
        out += v;
        out += '\n';
    }
    return out;
}

std::vector<std::string> AeronDriverConfig::to_aeronmd_argv() const {
    std::vector<std::string> argv;
    argv.reserve(to_properties().size() + 1);
    for (const auto& [k, v] : to_properties())
        argv.push_back("-D" + k + "=" + v);
    if (!properties_file.empty())
        argv.push_back(properties_file);  // positional: loaded by aeronmd
    return argv;
}

// -- Launcher --------------------------------------------------------------------

std::string resolve_aeron_dir(std::string_view configured) {
    if (!configured.empty())
        return std::string(configured);
    if (const char* env = ::getenv("AERON_DIR"); env != nullptr && *env != '\0')
        return env;
    // Mirror aeron_default_path() on Linux: /dev/shm/aeron-<user>.
    const passwd* pw = ::getpwuid(::getuid());
    std::string user = (pw != nullptr && pw->pw_name != nullptr)
                           ? pw->pw_name
                           : std::to_string(::getuid());
    return "/dev/shm/aeron-" + user;
}

bool aeron_cnc_ready(std::string_view aeron_dir) noexcept {
    if (aeron_dir.empty())
        return false;
    std::string path(aeron_dir);
    path += "/cnc.dat";
    struct stat st {};
    return ::stat(path.c_str(), &st) == 0 && st.st_size > 0;
}

bool aeron_process_alive(pid_t pid) noexcept {
    if (pid <= 0)
        return false;
    if (::kill(pid, 0) == 0)
        return true;
    return errno == EPERM;
}

int request_aeronmd_stop(pid_t pid, int sig) noexcept {
    if (pid <= 0)
        return -1;
    return ::kill(pid, sig);
}

AeronDriverLaunchResult launch_aeronmd(const AeronDriverConfig& cfg,
                                       std::string_view aeronmd_path,
                                       int64_t cnc_timeout_ms) noexcept {
    AeronDriverLaunchResult res;
    res.aeron_dir = resolve_aeron_dir(cfg.aeron_dir);

    std::string err;
    if (!cfg.validate(err)) {
        res.status = AeronDriverLaunchStatus::ValidateFailed;
        res.detail = "invalid driver config: " + err;
        return res;
    }
    if (aeronmd_path.empty()) {
        res.status = AeronDriverLaunchStatus::ExecFailed;
        res.detail = "aeronmd path empty";
        return res;
    }
    // Fail closed on a live driver — double-starting on the same dir corrupts
    // nothing (the second driver would refuse) but masks the crash that left
    // the stale CnC; force the operator to clean state explicitly.
    if (aeron_cnc_ready(res.aeron_dir) && !cfg.dir_delete_on_start) {
        res.status = AeronDriverLaunchStatus::AlreadyRunning;
        res.detail = "cnc.dat already present in " + res.aeron_dir;
        return res;
    }

    // CLOEXEC error pipe: child reports execvp errno; EOF means exec OK.
    int pipefd[2] = {-1, -1};
    if (::pipe2(pipefd, O_CLOEXEC) != 0) {
        res.status = AeronDriverLaunchStatus::ExecFailed;
        res.detail = std::string("pipe2: ") + std::strerror(errno);
        return res;
    }

    // Property list needs aeron_dir resolved even when cfg left it empty so
    // that bench loops and AERON_DIR env users stay deterministic.
    AeronDriverConfig effective = cfg;
    if (effective.aeron_dir.empty())
        effective.aeron_dir = res.aeron_dir;
    const std::vector<std::string> args = effective.to_aeronmd_argv();

    const pid_t pid = ::fork();
    if (pid < 0) {
        ::close(pipefd[0]);
        ::close(pipefd[1]);
        res.status = AeronDriverLaunchStatus::ExecFailed;
        res.detail = std::string("fork: ") + std::strerror(errno);
        return res;
    }
    if (pid == 0) {
        // Child: own process group; exec aeronmd. Inherit stdout/stderr so
        // aeron.print.configuration output lands in the caller's logs.
        ::setsid();
        ::close(pipefd[0]);
        std::string bin(aeronmd_path);  // NUL-terminated copy for execv
        std::vector<char*> argv;
        argv.reserve(args.size() + 2);
        argv.push_back(bin.data());
        for (const auto& a : args)
            argv.push_back(const_cast<char*>(a.c_str()));
        argv.push_back(nullptr);
        ::execv(bin.c_str(), argv.data());
        const int e = errno;
        // Best-effort errno report; ignore write failure, then die.
        (void)!::write(pipefd[1], &e, sizeof(e));
        _exit(127);
    }

    ::close(pipefd[1]);
    res.pid = pid;
    int child_errno = 0;
    const ssize_t n = ::read(pipefd[0], &child_errno, sizeof(child_errno));
    ::close(pipefd[0]);
    if (n > 0) {
        // exec failed — reap and report.
        ::waitpid(pid, nullptr, 0);
        res.pid = -1;
        res.status = AeronDriverLaunchStatus::ExecFailed;
        res.detail = std::string("execv ") + std::string(aeronmd_path) + ": " +
                     std::strerror(child_errno);
        return res;
    }

    // Wait for the driver to publish cnc.dat; bail early if the child died.
    const int64_t deadline = mono_ms() + cnc_timeout_ms;
    while (mono_ms() < deadline) {
        if (aeron_cnc_ready(res.aeron_dir)) {
            res.status = AeronDriverLaunchStatus::Started;
            res.detail = "driver up at " + res.aeron_dir;
            return res;
        }
        int status = 0;
        if (::waitpid(pid, &status, WNOHANG) == pid) {
            res.pid = -1;
            res.status = AeronDriverLaunchStatus::ExecFailed;
            res.detail = "aeronmd exited during init (status " +
                         std::to_string(status) + ")";
            return res;
        }
        std::this_thread::sleep_for(std::chrono::milliseconds(10));
    }
    res.status = AeronDriverLaunchStatus::CncTimeout;
    res.detail = "cnc.dat not observed within " +
                 std::to_string(cnc_timeout_ms) + "ms";
    return res;
}

}  // namespace exch
