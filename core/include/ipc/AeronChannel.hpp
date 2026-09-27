#pragma once

// Task 1.3.5 — Aeron `aeron:ipc` transport (preferred IPC path).
//
// The channel owns one Aeron client (conductor driven by an internal agent
// invoker — no extra thread), one ExclusivePublication on out_uri and one
// Subscription on in_uri:
//
//   core publishes to   aeron:ipc?alias=orders_out   (stream 1002 default)
//   core subscribes to  aeron:ipc?alias=orders_in    (stream 1001 default)
//
// Requires a media driver on the host (vendored C driver:
// core/third_party/aeron/bin/aeronmd, launched by deploy/scripts/bench_ipc.sh
// or equivalent). With EXCH_WITH_AERON=0 all calls fail closed (open()
// returns false) and the class compiles as a stub, same as Phase-01 stub.
//
// Backpressure: send() applies a bounded retry to offer(); NOT_CONNECTED /
// BACK_PRESSURED / ADMIN_ACTION spin until offer_retry_ns, then return false
// (caller's channel-full policy — ENGINE_OVERLOAD upstream in Task 2.3.7).
// Peer-death detection: publisher_connected()/subscriber_connected() reflect
// image liveness; a dead driver surfaces via DriverTimeoutException on the
// conductor, caught and mapped to is_open()==false on the next call.

#include <cstdint>
#include <memory>
#include <string>
#include <string_view>

#include "ipc/IpcChannel.hpp"

namespace exch {

struct AeronChannelConfig {
    std::string in_uri  = "aeron:ipc?alias=orders_in";   // gateway -> core
    std::string out_uri = "aeron:ipc?alias=orders_out";  // core -> gateway
    int32_t in_stream_id  = 1001;
    int32_t out_stream_id = 1002;
    // Media-driver directory (CnC file parent). Empty => $AERON_DIR env,
    // else the client default (/dev/shm/aeron-<user> on Linux).
    std::string aeron_dir;
    int64_t connect_timeout_ms = 5000;  // add pub/sub resolution deadline
    int64_t offer_retry_ns = 1'000'000; // bounded backpressure spin per send
    int32_t poll_fragment_limit = 10;   // fragments drained per poll()
};

class AeronChannel final : public IpcChannel {
public:
    AeronChannel(std::string_view in_uri, std::string_view out_uri);
    explicit AeronChannel(AeronChannelConfig cfg);
    ~AeronChannel() override;

    AeronChannel(const AeronChannel&) = delete;
    AeronChannel& operator=(const AeronChannel&) = delete;

    bool open() noexcept override;
    void close() noexcept override;
    [[nodiscard]] bool is_open() const noexcept override { return open_; }

    bool send(const void* data, uint32_t len) noexcept override;
    int32_t poll(void* buf, uint32_t buf_cap) noexcept override;

    // Zero-copy inbound read: `handler` is invoked once per assembled message
    // with a pointer directly into the driver-mapped log buffer (valid only
    // for the duration of the callback). Returns fragments consumed
    // (<= fragment_limit), 0 when drained, -1 on error.
    using fragment_sink_t = void (*)(void* ctx, const uint8_t* data, uint32_t len);
    int poll_each(fragment_sink_t handler, void* ctx, int fragment_limit) noexcept;

    // Peer liveness probes (image level).
    [[nodiscard]] bool publisher_connected() const noexcept;   // out->peer
    [[nodiscard]] bool subscriber_connected() const noexcept;  // in->peer

    [[nodiscard]] const std::string& in_uri() const noexcept { return cfg_.in_uri; }
    [[nodiscard]] const std::string& out_uri() const noexcept { return cfg_.out_uri; }

private:
    struct Impl;  // pimpl — keeps Aeron headers out of includers and lets the
                  // stub build compile cleanly when EXCH_WITH_AERON=0.
    AeronChannelConfig cfg_;
    std::unique_ptr<Impl> impl_;
    bool open_ = false;
};

}  // namespace exch
