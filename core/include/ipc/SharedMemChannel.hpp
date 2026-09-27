#pragma once

// Task 1.3.5 — shared-memory fallback transport. A channel is a pair of
// shm_open'd SPSC rings on the same host:
//
//   /exchange_ipc_{shard}_in    gateway -> core   (OrderNew / OrderCancel)
//   /exchange_ipc_{shard}_out   core -> gateway   (TradeFill / BookSnapshot)
//
// Endpoint::Core     = matching engine: consumes _in, produces _out
// Endpoint::Gateway  = Go services side: produces _in, consumes _out
//
// Zero-copy reads via poll_view()/consume() — the FlatBuffers Event is read
// directly out of the ring slot (spec §2.3). Backpressure: send() returns
// false when the outbound ring is full; drops()/occupancy() expose metrics.

#include <cstdint>
#include <string>
#include <string_view>

#include "ipc/IpcChannel.hpp"
#include "ipc/ShmRing.hpp"

namespace exch {

class SharedMemChannel final : public IpcChannel {
public:
    enum class Endpoint : uint8_t { Core, Gateway };

    static constexpr std::string_view kDefaultBase = "exchange_ipc";

    // shm names are "<base>_<shard>_in" / "<base>_<shard>_out" under /dev/shm.
    SharedMemChannel(std::string_view base, uint16_t shard, Endpoint endpoint,
                     bool create,
                     uint32_t capacity = ShmRing::kDefaultCapacity,
                     uint32_t slot_payload = ShmRing::kDefaultSlotPayload);
    ~SharedMemChannel() override;

    SharedMemChannel(const SharedMemChannel&) = delete;
    SharedMemChannel& operator=(const SharedMemChannel&) = delete;

    bool open() noexcept override;
    void close() noexcept override;
    [[nodiscard]] bool is_open() const noexcept override { return open_; }

    // Publish outbound message. false = ring full -> backpressure
    // (drops() increments; caller applies ENGINE_OVERLOAD policy upstream).
    bool send(const void* data, uint32_t len) noexcept override;

    // Copy next inbound message into buf (direct slot->caller copy, no
    // intermediate staging). Bytes copied, 0 drained, <0 error.
    int32_t poll(void* buf, uint32_t buf_cap) noexcept override;

    // Zero-copy inbound read: returns pointer into the ring slot (valid until
    // consume()) and sets *len_out. nullptr when drained.
    const uint8_t* poll_view(uint32_t* len_out) noexcept;
    void consume() noexcept;

    // Liveness of the far-end producer (kill(pid,0) on the stamped pid) plus
    // the nanosecond heartbeat the producer refreshes on every write batch.
    [[nodiscard]] bool producer_alive() const noexcept;
    [[nodiscard]] uint64_t producer_heartbeat_ns() const noexcept;

    [[nodiscard]] uint64_t occupancy() const noexcept;   // inbound ring fill
    [[nodiscard]] uint64_t drops() const noexcept;       // outbound full-drops

    ShmRing& inbound() noexcept { return in_; }
    ShmRing& outbound() noexcept { return out_; }

    // Canonical shm object names (no leading slash — shm_open adds it).
    static std::string in_name(std::string_view base, uint16_t shard);
    static std::string out_name(std::string_view base, uint16_t shard);

private:
    // Direction from *this endpoint's* perspective: the core sends on _out
    // and receives on _in; the gateway sends on _in and receives on _out.
    ShmRing& send_ring() noexcept {
        return endpoint_ == Endpoint::Core ? out_ : in_;
    }
    const ShmRing& send_ring() const noexcept {
        return endpoint_ == Endpoint::Core ? out_ : in_;
    }
    ShmRing& recv_ring() noexcept {
        return endpoint_ == Endpoint::Core ? in_ : out_;
    }
    const ShmRing& recv_ring() const noexcept {
        return endpoint_ == Endpoint::Core ? in_ : out_;
    }

    std::string in_name_;    // shm object "<base>_<shard>_in"  (gw -> core)
    std::string out_name_;   // shm object "<base>_<shard>_out" (core -> gw)
    Endpoint endpoint_;
    bool create_;
    uint32_t capacity_;
    uint32_t slot_payload_;
    ShmRing in_;
    ShmRing out_;
    bool open_ = false;
};

}  // namespace exch
