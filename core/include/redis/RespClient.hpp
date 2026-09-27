#pragma once

// Task 2.3.5/2.3.6 — minimal synchronous RESP (REdis Serialization Protocol)
// client for the C++ matching core.
//
// Scope is deliberately narrow: blocking TCP socket, RESP2, the small command
// surface the coordination paths need (GET/SET PX/PEXPIRE/PTTL/DEL/MSET/MGET/
// EVAL/PING). There is intentionally no hiredis dependency — the core links
// nothing it does not control, and this client carries exactly the semantics
// the leader-lease and degradation-mode contracts require:
//
//   * Reconnect-capable: a transport failure drops the socket and one bounded
//     reconnect+retry is attempted per command (Config::max_attempts).
//   * Fail-closed on timeout: SO_RCVTIMEO/SO_SNDTIMEO bound every syscall; a
//     timed-out or protocol-desynced reply marks the client disconnected and
//     the command returns false. Callers must treat false as "state unknown",
//     never as a default value (spec §2.7 Strict Fail-Closed Zero-Loss
//     Pessimism).
//   * Sentinel failover note: this client pins a single address. The
//     production wiring resolves the current master via Sentinel
//     (Phase-01 Task 1.3.9 / services sentinel path); on reconnect after a
//     master switch the address may be stale — the lease/mode logic on top
//     treats a dead store as an error, never as success.
//
// All functions are noexcept: transport errors surface via return value and
// last_status(), never exceptions.

#include <cstdint>
#include <string>
#include <string_view>
#include <utility>
#include <vector>

namespace exch {

// RESP2 reply value. Bulk/simple strings share `str`; arrays share `items`;
// `$-1`/`*-1` decode as Type::Nil, `-ERR ...` as Type::Error (a transport-
// successful command-level failure — execute() still returns true).
struct RespValue {
    enum class Type : uint8_t {
        SimpleString,
        Error,
        Integer,
        BulkString,
        Array,
        Nil,
    };

    Type type = Type::Nil;
    int64_t integer = 0;
    std::string str;
    std::vector<RespValue> items;

    [[nodiscard]] bool is_nil() const noexcept { return type == Type::Nil; }
    [[nodiscard]] bool is_error() const noexcept { return type == Type::Error; }
    // "+OK" simple-string.
    [[nodiscard]] bool is_ok() const noexcept {
        return type == Type::SimpleString && str == "OK";
    }
    [[nodiscard]] bool as_int(int64_t* out) const noexcept {
        if (type != Type::Integer || out == nullptr) return false;
        *out = integer;
        return true;
    }
    // Bulk or simple string payload as a view into this value.
    [[nodiscard]] bool as_string(std::string_view* out) const noexcept {
        if (out == nullptr) return false;
        if (type == Type::BulkString || type == Type::SimpleString) {
            *out = str;
            return true;
        }
        return false;
    }
};

// Why the exchange failed at transport level (last_status()). A command that
// reached Redis and got `-ERR` is Status::ErrReply — the round-trip succeeded.
enum class RespStatus : uint8_t {
    Ok = 0,     // reply read cleanly (check RespValue::is_error for -ERR)
    ErrReply,   // transport succeeded; reply itself was a -ERR command failure
    Timeout,    // SO_RCVTIMEO/SO_SNDTIMEO/connect deadline hit
    Io,         // socket-level failure / peer closed mid-exchange
    Proto,      // reply stream desynced (fail-closed: socket dropped)
    NoConn,     // no connection could be established
};

struct RespClientConfig {
    std::string host = "127.0.0.1";
    uint16_t port = 6379;
    int connect_timeout_ms = 1'000;
    int io_timeout_ms = 1'000;   // per-syscall recv/send bound
    int max_attempts = 2;        // 1 initial try + bounded reconnect retries
};

class RespClient {
public:
    explicit RespClient(RespClientConfig cfg = {}) noexcept;
    ~RespClient() noexcept;

    RespClient(const RespClient&) = delete;
    RespClient& operator=(const RespClient&) = delete;
    RespClient(RespClient&& o) noexcept;
    RespClient& operator=(RespClient&& o) noexcept;

    // Test seam: adopt an already-connected fd (e.g. a socketpair peer) and
    // apply the configured io timeouts. Adopted clients cannot reconnect —
    // reconnect requires a configured host:port — so a dead adopted peer
    // fails closed instead of silently dialing a default address.
    bool attach(int fd) noexcept;

    // TCP connect with Config::connect_timeout_ms deadline. Safe to call on
    // an open client (drops the old socket first).
    bool connect() noexcept;
    void disconnect() noexcept;
    [[nodiscard]] bool connected() const noexcept { return fd_ >= 0; }
    [[nodiscard]] RespStatus last_status() const noexcept { return status_; }
    // Wall-clock µs of the last successful execute() round trip — the feed
    // for the HealthChecker Redis-latency probe (Task 2.3.6).
    [[nodiscard]] int64_t last_rtt_us() const noexcept { return last_rtt_us_; }

    // Send one command and read one reply. Returns true when a reply was
    // parsed (inspect `out->is_error()` for -ERR). Returns false on
    // transport/timeout/protocol failure — the socket is then dropped
    // (fail-closed) and, attempts permitting, one reconnect+retry is made.
    bool execute(const std::vector<std::string_view>& args,
                 RespValue* out) noexcept;

    // --- typed helpers (thin execute() wrappers) ----------------------------

    bool ping() noexcept;
    // out->is_nil() on a missing key.
    bool get(std::string_view key, RespValue* out) noexcept;
    // SET key val PX px_ms. Returns the transport result; check is_ok().
    bool set_px(std::string_view key, std::string_view val,
                int64_t px_ms) noexcept;
    bool pexpire(std::string_view key, int64_t px_ms) noexcept;
    // PTTL: -2 key absent, -1 no TTL, -3 transport error.
    int64_t pttl(std::string_view key) noexcept;
    // DEL: number of keys removed, -1 on transport error.
    int64_t del(std::string_view key) noexcept;
    // EVAL script nkeys <keys...> <args...> — keys_and_args carries keys first.
    bool eval(std::string_view script, uint32_t nkeys,
              const std::vector<std::string_view>& keys_and_args,
              RespValue* out) noexcept;
    // MSET k v k v ... (atomic multi-key write, spec §2.4 mode record).
    bool mset(const std::vector<std::pair<std::string_view, std::string_view>>&
                  kvs) noexcept;
    bool mget(const std::vector<std::string_view>& keys,
              RespValue* out) noexcept;

private:
    bool send_all(const char* data, size_t len) noexcept;
    bool read_byte(char* out) noexcept;
    bool refill() noexcept;
    bool read_line(std::string* out) noexcept;
    bool read_reply(RespValue* out, uint32_t depth) noexcept;
    bool read_int64(int64_t* out) noexcept;

    RespClientConfig cfg_;
    int fd_ = -1;
    RespStatus status_ = RespStatus::NoConn;
    int64_t last_rtt_us_ = 0;

    // Read buffer: commands/replies here are control-path (500ms–5s cadence),
    // so a 4KiB socket buffer is ample.
    static constexpr size_t kRBufCap = 4 * 1024;
    char rbuf_[kRBufCap] = {};
    size_t rlen_ = 0;
    size_t rpos_ = 0;
    // Serialize scratch — reused across execute() calls, no hot-path alloc.
    std::string wbuf_;
};

}  // namespace exch
