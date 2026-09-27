// Task 2.3.5/2.3.6 — minimal synchronous RESP client. See header for the
// fail-closed / bounded-timeout contract (spec §2.7).

#include "redis/RespClient.hpp"

#include <cerrno>
#include <chrono>
#include <cstdio>
#include <cstring>
#include <arpa/inet.h>
#include <fcntl.h>
#include <netdb.h>
#include <netinet/in.h>
#include <netinet/tcp.h>
#include <poll.h>
#include <sys/socket.h>
#include <unistd.h>

namespace exch {
namespace {

int64_t mono_us_now() noexcept {
    return std::chrono::duration_cast<std::chrono::microseconds>(
               std::chrono::steady_clock::now().time_since_epoch())
        .count();
}

// Milliseconds → timeval for SO_RCVTIMEO/SO_SNDTIMEO.
timeval ms_to_tv(int ms) noexcept {
    timeval tv{};
    tv.tv_sec = ms / 1000;
    tv.tv_usec = (ms % 1000) * 1000;
    if (tv.tv_sec == 0 && tv.tv_usec == 0) tv.tv_usec = 1000;  // floor 1ms
    return tv;
}

}  // namespace

RespClient::RespClient(RespClientConfig cfg) noexcept : cfg_(std::move(cfg)) {}

RespClient::~RespClient() noexcept { disconnect(); }

RespClient::RespClient(RespClient&& o) noexcept
    : cfg_(std::move(o.cfg_)),
      fd_(o.fd_),
      status_(o.status_),
      last_rtt_us_(o.last_rtt_us_),
      rlen_(o.rlen_),
      rpos_(o.rpos_),
      wbuf_(std::move(o.wbuf_)) {
    std::memcpy(rbuf_, o.rbuf_, rlen_);
    o.fd_ = -1;
    o.rlen_ = o.rpos_ = 0;
}

RespClient& RespClient::operator=(RespClient&& o) noexcept {
    if (this != &o) {
        disconnect();
        cfg_ = std::move(o.cfg_);
        fd_ = o.fd_;
        status_ = o.status_;
        last_rtt_us_ = o.last_rtt_us_;
        rlen_ = o.rlen_;
        rpos_ = o.rpos_;
        std::memcpy(rbuf_, o.rbuf_, rlen_);
        wbuf_ = std::move(o.wbuf_);
        o.fd_ = -1;
        o.rlen_ = o.rpos_ = 0;
    }
    return *this;
}

bool RespClient::attach(int fd) noexcept {
    disconnect();
    if (fd < 0) return false;
    const timeval tv = ms_to_tv(cfg_.io_timeout_ms);
    ::setsockopt(fd, SOL_SOCKET, SO_RCVTIMEO, &tv, sizeof(tv));
    ::setsockopt(fd, SOL_SOCKET, SO_SNDTIMEO, &tv, sizeof(tv));
    fd_ = fd;
    rlen_ = rpos_ = 0;
    status_ = RespStatus::Ok;
    return true;
}

bool RespClient::connect() noexcept {
    disconnect();
    if (cfg_.host.empty() || cfg_.port == 0) {
        status_ = RespStatus::NoConn;
        return false;
    }

    // Resolve. Fast path is a numeric IPv4 literal (the only form production
    // config should carry); a hostname falls back to getaddrinfo — blocking
    // DNS is acceptable at connect time but never on the IO path.
    sockaddr_in addr{};
    addr.sin_family = AF_INET;
    addr.sin_port = htons(cfg_.port);
    if (::inet_pton(AF_INET, cfg_.host.c_str(), &addr.sin_addr) != 1) {
        addrinfo hints{};
        hints.ai_family = AF_INET;
        hints.ai_socktype = SOCK_STREAM;
        char port_str[8];
        std::snprintf(port_str, sizeof(port_str), "%u", cfg_.port);
        addrinfo* res = nullptr;
        if (::getaddrinfo(cfg_.host.c_str(), port_str, &hints, &res) != 0 ||
            res == nullptr) {
            status_ = RespStatus::NoConn;
            return false;
        }
        addr = *reinterpret_cast<sockaddr_in*>(res->ai_addr);
        ::freeaddrinfo(res);
    }

    int fd = ::socket(AF_INET, SOCK_STREAM | SOCK_NONBLOCK | SOCK_CLOEXEC, 0);
    if (fd < 0) {
        status_ = RespStatus::NoConn;
        return false;
    }

    int rc = ::connect(fd, reinterpret_cast<sockaddr*>(&addr), sizeof(addr));
    if (rc != 0 && errno == EINPROGRESS) {
        pollfd pfd{fd, POLLOUT, 0};
        do {
            rc = ::poll(&pfd, 1, cfg_.connect_timeout_ms);
        } while (rc < 0 && errno == EINTR);
        if (rc == 0) status_ = RespStatus::Timeout;
        if (rc <= 0) {
            ::close(fd);
            if (status_ != RespStatus::Timeout) status_ = RespStatus::NoConn;
            return false;
        }
        int err = 0;
        socklen_t len = sizeof(err);
        if (::getsockopt(fd, SOL_SOCKET, SO_ERROR, &err, &len) != 0 || err != 0) {
            ::close(fd);
            status_ = RespStatus::NoConn;
            return false;
        }
    } else if (rc != 0) {
        ::close(fd);
        status_ = RespStatus::NoConn;
        return false;
    }

    // Back to blocking mode; per-syscall deadlines via SO_RCVTIMEO/SO_SNDTIMEO.
    int flags = ::fcntl(fd, F_GETFL, 0);
    ::fcntl(fd, F_SETFL, flags & ~O_NONBLOCK);
    const timeval tv = ms_to_tv(cfg_.io_timeout_ms);
    ::setsockopt(fd, SOL_SOCKET, SO_RCVTIMEO, &tv, sizeof(tv));
    ::setsockopt(fd, SOL_SOCKET, SO_SNDTIMEO, &tv, sizeof(tv));
    int one = 1;
    ::setsockopt(fd, IPPROTO_TCP, TCP_NODELAY, &one, sizeof(one));

    fd_ = fd;
    rlen_ = rpos_ = 0;
    status_ = RespStatus::Ok;
    return true;
}

void RespClient::disconnect() noexcept {
    if (fd_ >= 0) {
        ::close(fd_);
        fd_ = -1;
    }
    rlen_ = rpos_ = 0;
}

bool RespClient::send_all(const char* data, size_t len) noexcept {
    size_t off = 0;
    while (off < len) {
        const ssize_t n = ::send(fd_, data + off, len - off, MSG_NOSIGNAL);
        if (n > 0) {
            off += static_cast<size_t>(n);
            continue;
        }
        if (n < 0 && errno == EINTR) continue;
        status_ = (n < 0 && (errno == EAGAIN || errno == EWOULDBLOCK))
                      ? RespStatus::Timeout
                      : RespStatus::Io;
        return false;
    }
    return true;
}

bool RespClient::refill() noexcept {
    rpos_ = 0;
    rlen_ = 0;
    for (;;) {
        const ssize_t n = ::recv(fd_, rbuf_, kRBufCap, 0);
        if (n > 0) {
            rlen_ = static_cast<size_t>(n);
            return true;
        }
        if (n < 0 && errno == EINTR) continue;
        status_ = (n < 0 && (errno == EAGAIN || errno == EWOULDBLOCK))
                      ? RespStatus::Timeout
                      : RespStatus::Io;
        return false;
    }
}

bool RespClient::read_byte(char* out) noexcept {
    if (rpos_ >= rlen_ && !refill()) return false;
    *out = rbuf_[rpos_++];
    return true;
}

bool RespClient::read_line(std::string* out) noexcept {
    out->clear();
    // A RESP line is short; cap at 1MiB anyway so a hostile/desynced peer
    // cannot grow this unbounded before the protocol error trips.
    for (size_t i = 0; i < (1u << 20); ++i) {
        char c = 0;
        if (!read_byte(&c)) return false;
        if (c == '\r') {
            char lf = 0;
            if (!read_byte(&lf)) return false;
            if (lf != '\n') {
                status_ = RespStatus::Proto;
                return false;
            }
            return true;
        }
        out->push_back(c);
    }
    status_ = RespStatus::Proto;
    return false;
}

bool RespClient::read_int64(int64_t* out) noexcept {
    std::string line;
    if (!read_line(&line)) return false;
    char* end = nullptr;
    errno = 0;
    const long long v = std::strtoll(line.c_str(), &end, 10);
    if (errno != 0 || end == line.c_str() || *end != '\0') {
        status_ = RespStatus::Proto;
        return false;
    }
    *out = v;
    return true;
}

bool RespClient::read_reply(RespValue* out, uint32_t depth) noexcept {
    if (depth > 16) {  // pathological nesting — desync guard
        status_ = RespStatus::Proto;
        return false;
    }
    char t = 0;
    if (!read_byte(&t)) return false;
    switch (t) {
        case '+':
            out->type = RespValue::Type::SimpleString;
            return read_line(&out->str);
        case '-':
            out->type = RespValue::Type::Error;
            return read_line(&out->str);
        case ':':
            out->type = RespValue::Type::Integer;
            return read_int64(&out->integer);
        case '$': {
            int64_t len = 0;
            if (!read_int64(&len)) return false;
            if (len < 0) {
                out->type = RespValue::Type::Nil;
                return true;
            }
            if (len > (64 << 20)) {  // 64MiB bulk cap — desync guard
                status_ = RespStatus::Proto;
                return false;
            }
            out->type = RespValue::Type::BulkString;
            out->str.resize(static_cast<size_t>(len));
            size_t got = 0;
            while (got < static_cast<size_t>(len)) {
                if (rpos_ >= rlen_ && !refill()) return false;
                const size_t avail = rlen_ - rpos_;
                const size_t want = static_cast<size_t>(len) - got;
                const size_t take = avail < want ? avail : want;
                std::memcpy(&out->str[got], rbuf_ + rpos_, take);
                rpos_ += take;
                got += take;
            }
            // Trailing CRLF.
            char cr = 0, lf = 0;
            if (!read_byte(&cr) || !read_byte(&lf) || cr != '\r' || lf != '\n') {
                status_ = RespStatus::Proto;
                return false;
            }
            return true;
        }
        case '*': {
            int64_t n = 0;
            if (!read_int64(&n)) return false;
            if (n < 0) {
                out->type = RespValue::Type::Nil;
                return true;
            }
            if (n > (1 << 20)) {  // desync guard
                status_ = RespStatus::Proto;
                return false;
            }
            out->type = RespValue::Type::Array;
            out->items.resize(static_cast<size_t>(n));
            for (int64_t i = 0; i < n; ++i) {
                if (!read_reply(&out->items[static_cast<size_t>(i)], depth + 1))
                    return false;
            }
            return true;
        }
        default:
            status_ = RespStatus::Proto;
            return false;
    }
}

bool RespClient::execute(const std::vector<std::string_view>& args,
                         RespValue* out) noexcept {
    if (args.empty() || args.size() > 512) {
        status_ = RespStatus::Proto;
        return false;
    }
    // Serialize: *N\r\n$len\r\narg\r\n ...
    wbuf_.clear();
    wbuf_ += '*';
    wbuf_ += std::to_string(args.size());
    wbuf_ += "\r\n";
    for (const std::string_view a : args) {
        wbuf_ += '$';
        wbuf_ += std::to_string(a.size());
        wbuf_ += "\r\n";
        wbuf_.append(a.data(), a.size());
        wbuf_ += "\r\n";
    }

    const int attempts = cfg_.max_attempts > 0 ? cfg_.max_attempts : 1;
    for (int attempt = 0; attempt < attempts; ++attempt) {
        if (fd_ < 0 && !connect()) continue;  // status_ set inside connect()
        if (!send_all(wbuf_.data(), wbuf_.size())) {
            disconnect();
            continue;  // status_ = Timeout | Io
        }
        const int64_t t0 = mono_us_now();
        out->type = RespValue::Type::Nil;
        out->str.clear();
        out->items.clear();
        if (!read_reply(out, 0)) {
            disconnect();  // fail-closed: never reuse a desynced stream
            if (status_ == RespStatus::Proto) return false;  // retry is futile
            continue;
        }
        last_rtt_us_ = mono_us_now() - t0;
        status_ = out->is_error() ? RespStatus::ErrReply : RespStatus::Ok;
        return true;
    }
    if (fd_ >= 0) disconnect();
    if (status_ == RespStatus::Ok) status_ = RespStatus::NoConn;
    return false;
}

// --- typed helpers ----------------------------------------------------------

bool RespClient::ping() noexcept {
    RespValue out;
    if (!execute({"PING"}, &out)) return false;
    return out.type == RespValue::Type::SimpleString && out.str == "PONG";
}

bool RespClient::get(std::string_view key, RespValue* out) noexcept {
    return execute({"GET", key}, out);
}

bool RespClient::set_px(std::string_view key, std::string_view val,
                        int64_t px_ms) noexcept {
    std::string px = std::to_string(px_ms);
    RespValue out;
    return execute({"SET", key, val, "PX", px}, &out) && out.is_ok();
}

bool RespClient::pexpire(std::string_view key, int64_t px_ms) noexcept {
    std::string px = std::to_string(px_ms);
    RespValue out;
    int64_t n = 0;
    return execute({"PEXPIRE", key, px}, &out) && out.as_int(&n) && n == 1;
}

int64_t RespClient::pttl(std::string_view key) noexcept {
    RespValue out;
    int64_t v = -3;
    if (!execute({"PTTL", key}, &out) || !out.as_int(&v)) return -3;
    return v;
}

int64_t RespClient::del(std::string_view key) noexcept {
    RespValue out;
    int64_t v = -1;
    if (!execute({"DEL", key}, &out) || !out.as_int(&v)) return -1;
    return v;
}

bool RespClient::eval(std::string_view script, uint32_t nkeys,
                      const std::vector<std::string_view>& keys_and_args,
                      RespValue* out) noexcept {
    // nk is a local std::string; its string_view in args stays valid for the
    // whole synchronous execute() call.
    const std::string nk = std::to_string(nkeys);
    std::vector<std::string_view> args;
    args.reserve(keys_and_args.size() + 3);
    args.push_back("EVAL");
    args.push_back(script);
    args.push_back(nk);
    args.insert(args.end(), keys_and_args.begin(), keys_and_args.end());
    return execute(args, out);
}

bool RespClient::mset(
    const std::vector<std::pair<std::string_view, std::string_view>>& kvs)
    noexcept {
    std::vector<std::string_view> args;
    args.reserve(1 + kvs.size() * 2);
    args.push_back("MSET");
    for (const auto& kv : kvs) {
        args.push_back(kv.first);
        args.push_back(kv.second);
    }
    RespValue out;
    return execute(args, &out) && out.is_ok();
}

bool RespClient::mget(const std::vector<std::string_view>& keys,
                      RespValue* out) noexcept {
    std::vector<std::string_view> args;
    args.reserve(1 + keys.size());
    args.push_back("MGET");
    args.insert(args.end(), keys.begin(), keys.end());
    return execute(args, out);
}

}  // namespace exch
