#pragma once

// PHASE-02 STUB (Task 2.3.7) — IPC seam: Go gateway <-> matching core
// (Task 1.3.5 transport, spec §2.3). SPSC queues both directions, zero-copy
// FlatBuffers payloads; backpressure maps to ENGINE_OVERLOAD (Task 2.3.7).

#include <cstdint>

namespace exch {

class IpcChannel {
public:
    virtual ~IpcChannel() = default;

    virtual bool open() noexcept = 0;
    virtual void close() noexcept = 0;
    [[nodiscard]] virtual bool is_open() const noexcept = 0;

    // Publish one framed outbound message. false = channel full/backpressure.
    virtual bool send(const void* data, uint32_t len) noexcept = 0;

    // Copy next inbound message into buf. Returns bytes written, 0 when the
    // channel is drained, <0 on error.
    virtual int32_t poll(void* buf, uint32_t buf_cap) noexcept = 0;
};

}  // namespace exch
