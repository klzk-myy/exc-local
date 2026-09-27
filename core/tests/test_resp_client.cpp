// Task 2.3.5/2.3.6 — RespClient tests.
//
// Two layers:
//   * hermetic socketpair tests — a scripted RESP peer verifies exact wire
//     encoding and feeds canned replies (no Redis needed);
//   * live tests gated on EXC_REDIS_TEST_ADDR (host:port of the dev
//     coordination instance, e.g. 127.0.0.1:16379) — skipped when unset.

#include <gtest/gtest.h>

#include <atomic>
#include <cerrno>
#include <cstdint>
#include <cstdlib>
#include <cstring>
#include <deque>
#include <memory>
#include <string>
#include <sys/socket.h>
#include <thread>
#include <unistd.h>
#include <vector>

#include "redis/RespClient.hpp"

namespace {

// --- scripted RESP peer -------------------------------------------------------
//
// Minimal server-side RESP request parser: reads `*N\r\n$len\r\narg\r\n…`
// frames off the socket and replies with the canned response for that round
// (queue order). A reply entry of "@sleep:ms" sleeps instead — used to force
// client-side timeouts. "@close" closes the connection.

struct FakePeer {
    int fd = -1;
    std::thread th;
    std::atomic<bool> stop{false};
    std::deque<std::string> replies;
    std::vector<std::vector<std::string>> requests;  // parsed args per command

    bool start(int peer_fd) {
        fd = peer_fd;
        th = std::thread([this] { serve(); });
        return true;
    }

    void serve() {
        std::string in;
        char buf[1024];
        while (!stop.load()) {
            const ssize_t n = ::recv(fd, buf, sizeof(buf), MSG_DONTWAIT);
            if (n == 0) return;              // peer closed
            if (n < 0) {
                if (errno == EAGAIN || errno == EWOULDBLOCK) {
                    // Idle: drain any complete frame that has landed (records
                    // the request even with no reply queued), else wait.
                    if (!try_drain(in)) ::usleep(1000);
                    continue;
                }
                return;
            }
            in.append(buf, static_cast<size_t>(n));
            try_drain(in);
        }
    }

    // Parse one complete RESP command frame from `in`; on success pops it,
    // records args, and sends the queued reply. Returns true if a command was
    // consumed.
    bool try_drain(std::string& in) {
        if (in.empty() || in[0] != '*') return false;
        // Parse *N
        const size_t h_end = in.find("\r\n");
        if (h_end == std::string::npos) return false;
        const int n_args = std::atoi(in.substr(1, h_end - 1).c_str());
        if (n_args <= 0) return false;
        size_t pos = h_end + 2;
        std::vector<std::string> args;
        for (int i = 0; i < n_args; ++i) {
            if (pos >= in.size() || in[pos] != '$') return false;
            const size_t l_end = in.find("\r\n", pos);
            if (l_end == std::string::npos) return false;
            const int len =
                std::atoi(in.substr(pos + 1, l_end - pos - 1).c_str());
            const size_t data = l_end + 2;
            if (in.size() < data + static_cast<size_t>(len) + 2) {
                return false;  // incomplete frame — wait for more bytes
            }
            args.push_back(in.substr(data, static_cast<size_t>(len)));
            pos = data + static_cast<size_t>(len) + 2;
        }
        in.erase(0, pos);
        requests.push_back(args);
        if (replies.empty()) return true;
        std::string rep = replies.front();
        replies.pop_front();
        if (rep == "@close") {
            ::shutdown(fd, SHUT_RDWR);
            return true;
        }
        if (rep.rfind("@sleep:", 0) == 0) {
            ::usleep(static_cast<unsigned>(
                         std::atoi(rep.c_str() + 7)) * 1000);
            return true;
        }
        ::send(fd, rep.data(), rep.size(), MSG_NOSIGNAL);
        return true;
    }

    void join() {
        stop.store(true);
        if (fd >= 0) ::shutdown(fd, SHUT_RDWR);
        if (th.joinable()) th.join();
    }
};

struct Pair {
    int cli = -1;
    int srv = -1;
    ~Pair() {
        if (cli >= 0) ::close(cli);
        if (srv >= 0) ::close(srv);
    }
};

Pair socket_pair() {
    Pair p;
    int fds[2] = {-1, -1};
    if (::socketpair(AF_UNIX, SOCK_STREAM, 0, fds) == 0) {
        p.cli = fds[0];
        p.srv = fds[1];
    }
    return p;
}

exch::RespClientConfig test_cfg() {
    exch::RespClientConfig cfg;
    cfg.host.clear();  // adopted fd — no reconnect target
    cfg.io_timeout_ms = 150;
    cfg.max_attempts = 1;
    return cfg;
}

std::string redis_addr() {
    const char* env = std::getenv("EXC_REDIS_TEST_ADDR");
    return env != nullptr ? std::string(env) : std::string();
}

void split_addr(const std::string& addr, std::string* host, uint16_t* port) {
    const auto colon = addr.rfind(':');
    *host = addr.substr(0, colon);
    *port = static_cast<uint16_t>(
        std::atoi(addr.substr(colon + 1).c_str()));
}

// --- wire-level tests ----------------------------------------------------------

TEST(RespClient, EncodesAndReadsSimpleString) {
    Pair p = socket_pair();
    ASSERT_GE(p.cli, 0);
    FakePeer peer;
    peer.replies = {"+PONG\r\n"};
    ASSERT_TRUE(peer.start(p.srv));

    exch::RespClient c(test_cfg());
    ASSERT_TRUE(c.attach(p.cli));
    p.cli = -1;  // client owns the fd now

    EXPECT_TRUE(c.ping());
    EXPECT_EQ(c.last_status(), exch::RespStatus::Ok);
    peer.join();
    ASSERT_EQ(peer.requests.size(), 1u);
    EXPECT_EQ(peer.requests[0], std::vector<std::string>{"PING"});
}

TEST(RespClient, BulkStringNilIntegerAndError) {
    Pair p = socket_pair();
    ASSERT_GE(p.cli, 0);
    FakePeer peer;
    peer.replies = {
        "$5\r\nhello\r\n",   // GET present
        "$-1\r\n",           // GET missing  → nil
        ":2000\r\n",         // PTTL
        "-ERR broken\r\n",   // command-level error
    };
    ASSERT_TRUE(peer.start(p.srv));

    exch::RespClient c(test_cfg());
    ASSERT_TRUE(c.attach(p.cli));
    p.cli = -1;

    exch::RespValue v;
    ASSERT_TRUE(c.get("k1", &v));
    std::string_view sv;
    ASSERT_TRUE(v.as_string(&sv));
    EXPECT_EQ(sv, "hello");

    ASSERT_TRUE(c.get("missing", &v));
    EXPECT_TRUE(v.is_nil());

    EXPECT_EQ(c.pttl("k1"), 2000);

    ASSERT_TRUE(c.get("k1", &v));  // transport ok, reply is -ERR
    EXPECT_TRUE(v.is_error());
    EXPECT_EQ(v.str, "ERR broken");
    EXPECT_EQ(c.last_status(), exch::RespStatus::ErrReply);

    peer.join();
}

TEST(RespClient, EvalSendsKeysAndArgsAndParsesArray) {
    Pair p = socket_pair();
    ASSERT_GE(p.cli, 0);
    FakePeer peer;
    peer.replies = {"*2\r\n:1\r\n:7\r\n"};
    ASSERT_TRUE(peer.start(p.srv));

    exch::RespClient c(test_cfg());
    ASSERT_TRUE(c.attach(p.cli));
    p.cli = -1;

    exch::RespValue v;
    ASSERT_TRUE(c.eval("return {1,7}", 2, {"k1", "k2", "arg1"}, &v));
    ASSERT_EQ(v.type, exch::RespValue::Type::Array);
    ASSERT_EQ(v.items.size(), 2u);
    int64_t a = 0, b = 0;
    ASSERT_TRUE(v.items[0].as_int(&a));
    ASSERT_TRUE(v.items[1].as_int(&b));
    EXPECT_EQ(a, 1);
    EXPECT_EQ(b, 7);

    peer.join();
    ASSERT_EQ(peer.requests.size(), 1u);
    // EVAL <script> 2 k1 k2 arg1
    const auto& req = peer.requests[0];
    ASSERT_EQ(req.size(), 6u);
    EXPECT_EQ(req[0], "EVAL");
    EXPECT_EQ(req[1], "return {1,7}");
    EXPECT_EQ(req[2], "2");
    EXPECT_EQ(req[3], "k1");
    EXPECT_EQ(req[4], "k2");
    EXPECT_EQ(req[5], "arg1");
}

TEST(RespClient, MsetEncodesKeyValuePairs) {
    Pair p = socket_pair();
    ASSERT_GE(p.cli, 0);
    FakePeer peer;
    peer.replies = {"+OK\r\n"};
    ASSERT_TRUE(peer.start(p.srv));

    exch::RespClient c(test_cfg());
    ASSERT_TRUE(c.attach(p.cli));
    p.cli = -1;

    EXPECT_TRUE(c.mset({{"a", "1"}, {"b", "two"}}));
    peer.join();
    ASSERT_EQ(peer.requests.size(), 1u);
    EXPECT_EQ(peer.requests[0],
              (std::vector<std::string>{"MSET", "a", "1", "b", "two"}));
}

TEST(RespClient, TimeoutIsFailClosed) {
    Pair p = socket_pair();
    ASSERT_GE(p.cli, 0);
    FakePeer peer;
    peer.replies = {"@sleep:500"};  // peer never answers in time
    ASSERT_TRUE(peer.start(p.srv));

    exch::RespClientConfig cfg = test_cfg();
    cfg.io_timeout_ms = 50;
    cfg.max_attempts = 1;
    exch::RespClient c(cfg);
    ASSERT_TRUE(c.attach(p.cli));
    p.cli = -1;

    exch::RespValue v;
    EXPECT_FALSE(c.get("k", &v));              // timed out
    EXPECT_EQ(c.last_status(), exch::RespStatus::Timeout);
    EXPECT_FALSE(c.connected());               // fail-closed: socket dropped
    peer.join();
}

TEST(RespClient, PeerCloseFailsClosed) {
    Pair p = socket_pair();
    ASSERT_GE(p.cli, 0);
    FakePeer peer;
    peer.replies = {"+PONG\r\n", "@close"};
    ASSERT_TRUE(peer.start(p.srv));

    exch::RespClient c(test_cfg());
    ASSERT_TRUE(c.attach(p.cli));
    p.cli = -1;

    EXPECT_TRUE(c.ping());
    exch::RespValue v;
    EXPECT_FALSE(c.get("k", &v));  // peer closed mid-exchange
    EXPECT_FALSE(c.connected());
    // Adopted fd has no reconnect target: a second attempt must fail fast.
    EXPECT_FALSE(c.get("k", &v));
    peer.join();
}

// --- live tests (gated) ---------------------------------------------------------

class LiveResp : public ::testing::Test {
protected:
    void SetUp() override {
        addr_ = redis_addr();
        if (addr_.empty()) {
            GTEST_SKIP() << "set EXC_REDIS_TEST_ADDR=host:port";
        }
        split_addr(addr_, &cfg_.host, &cfg_.port);
        cfg_.connect_timeout_ms = 1000;
        cfg_.io_timeout_ms = 1000;
        client_ = std::make_unique<exch::RespClient>(cfg_);
        ASSERT_TRUE(client_->connect()) << "redis unreachable at " << addr_;
        ASSERT_TRUE(client_->ping());
    }
    void TearDown() override {
        if (client_) client_->disconnect();
    }
    std::string addr_;
    exch::RespClientConfig cfg_;
    std::unique_ptr<exch::RespClient> client_;
};

TEST_F(LiveResp, SetGetPttlDelRoundTrip) {
    const std::string key = "test:resp:" + std::to_string(::getpid());
    ASSERT_TRUE(client_->del(key) >= 0);
    ASSERT_TRUE(client_->set_px(key, "v42", 60'000));

    exch::RespValue v;
    ASSERT_TRUE(client_->get(key, &v));
    std::string_view sv;
    ASSERT_TRUE(v.as_string(&sv));
    EXPECT_EQ(sv, "v42");

    const int64_t ttl = client_->pttl(key);
    EXPECT_GT(ttl, 0);
    EXPECT_LE(ttl, 60'000);

    EXPECT_EQ(client_->del(key), 1);
    ASSERT_TRUE(client_->get(key, &v));
    EXPECT_TRUE(v.is_nil());
}

TEST_F(LiveResp, EvalExecutesLuaAtomically) {
    const std::string key = "test:resp:eval:" + std::to_string(::getpid());
    ASSERT_TRUE(client_->del(key) >= 0);
    exch::RespValue v;
    ASSERT_TRUE(client_->eval(
        "redis.call('SET', KEYS[1], ARGV[1]); return redis.call('GET', KEYS[1])",
        1, {key, "lua-wrote"}, &v));
    std::string_view sv;
    ASSERT_TRUE(v.as_string(&sv));
    EXPECT_EQ(sv, "lua-wrote");
    EXPECT_EQ(client_->del(key), 1);
}

TEST_F(LiveResp, ReconnectsAfterPeerDrop) {
    ASSERT_TRUE(client_->ping());
    client_->disconnect();
    EXPECT_FALSE(client_->connected());
    // execute() reconnects automatically (max_attempts >= 2).
    EXPECT_TRUE(client_->ping());
    EXPECT_TRUE(client_->connected());
}

}  // namespace
