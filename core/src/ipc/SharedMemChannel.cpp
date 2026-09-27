// Task 1.3.5 — shared-memory IPC channel: /exchange_ipc_{shard}_{in,out}
// shm_open + mmap(MAP_SHARED) SPSC rings, 64-byte cache-line-aligned slots.
// Producer = Go gateway / consumer = C++ core on _in; reversed on _out.
#include "ipc/SharedMemChannel.hpp"

#include <cstdio>

namespace exch {

SharedMemChannel::SharedMemChannel(std::string_view base, uint16_t shard,
                                   Endpoint endpoint, bool create,
                                   uint32_t capacity, uint32_t slot_payload)
    : in_name_(in_name(base, shard)),
      out_name_(out_name(base, shard)),
      endpoint_(endpoint),
      create_(create),
      capacity_(capacity),
      slot_payload_(slot_payload) {}

SharedMemChannel::~SharedMemChannel() { close(); }

std::string SharedMemChannel::in_name(std::string_view base, uint16_t shard) {
    char buf[160];
    std::snprintf(buf, sizeof(buf), "%.*s_%u_in",
                  static_cast<int>(base.size()), base.data(),
                  static_cast<unsigned>(shard));
    return { buf };
}

std::string SharedMemChannel::out_name(std::string_view base, uint16_t shard) {
    char buf[160];
    std::snprintf(buf, sizeof(buf), "%.*s_%u_out",
                  static_cast<int>(base.size()), base.data(),
                  static_cast<unsigned>(shard));
    return { buf };
}

bool SharedMemChannel::open() noexcept {
    close();
    // Roles flip by endpoint: the core consumes _in and produces _out;
    // the gateway produces _in and consumes _out.
    const bool core = (endpoint_ == Endpoint::Core);
    if (!in_.open(in_name_, core ? ShmRing::Role::Consumer : ShmRing::Role::Producer,
                  create_, capacity_, slot_payload_))
        return false;
    if (!out_.open(out_name_, core ? ShmRing::Role::Producer : ShmRing::Role::Consumer,
                   create_, capacity_, slot_payload_)) {
        in_.close();
        return false;
    }
    open_ = true;
    return true;
}

void SharedMemChannel::close() noexcept {
    in_.close();
    out_.close();
    open_ = false;
}

bool SharedMemChannel::send(const void* data, uint32_t len) noexcept {
    return send_ring().try_write(data, len);
}

int32_t SharedMemChannel::poll(void* buf, uint32_t buf_cap) noexcept {
    return recv_ring().read(buf, buf_cap);
}

const uint8_t* SharedMemChannel::poll_view(uint32_t* len_out) noexcept {
    return recv_ring().peek(len_out);
}

void SharedMemChannel::consume() noexcept { recv_ring().consume(); }

bool SharedMemChannel::producer_alive() const noexcept {
    return recv_ring().producer_alive();
}

uint64_t SharedMemChannel::producer_heartbeat_ns() const noexcept {
    return recv_ring().producer_heartbeat_ns();
}

uint64_t SharedMemChannel::occupancy() const noexcept {
    return recv_ring().occupancy();
}
uint64_t SharedMemChannel::drops() const noexcept {
    return send_ring().drops();
}

}  // namespace exch
