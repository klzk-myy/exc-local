#pragma once

// IMP-PLAN Phase-3 Task 4 — CtlDemux: protocol demultiplexer over ONE
// cross-shard control channel.
//
// CrossShardCoordinator (Task 2.3.8, kBasketCtlMagic frames) and
// OptimisticShardCoordinator (Task 2.3.25, kOptCtlMagic frames) share the
// CrossShardCtlHeader layout — designed so "one Aeron stream pair can
// carry both protocols" (CrossShardCoordinator.hpp). But each
// coordinator's on_time_tick() drains its bound IpcChannel *wholly*: two
// coordinators sharing a raw channel would eat each other's frames. This
// demux restores the documented one-channel topology: the pump drains the
// physical channel once per loop pass and routes each frame by header
// magic into a per-protocol queue; each coordinator is constructed with
// its queue's IpcChannel view, so its unchanged on_time_tick() sees only
// its own frames.
//
// Queues are fixed-capacity (kQueueFrames x kFrameBytes), zero-alloc.
// Overflow drops the NEWEST frame and bumps drops_ — a flooded control
// lane degrades loudly (metrics) rather than unbounded-buffering on the
// hot path. Frames > kFrameBytes or with unknown magic are dropped and
// counted (fail-closed: never enqueue bytes a coordinator can't parse).

#include <cstdint>
#include <cstring>

#include "ipc/IpcChannel.hpp"
#include "matching/CrossShardCoordinator.hpp"    // kBasketCtlMagic
#include "matching/OptimisticShardCoordinator.hpp"  // kOptCtlMagic

namespace exch {

class CtlQueueChannel final : public IpcChannel {
public:
    CtlQueueChannel() = default;

    void bind(IpcChannel* phys) noexcept { phys_ = phys; }

    bool open() noexcept override { return phys_ != nullptr && phys_->open(); }
    void close() noexcept override {}  // physical owner closes
    [[nodiscard]] bool is_open() const noexcept override {
        return phys_ != nullptr && phys_->is_open();
    }

    // Coordinator-originated frames share the one physical channel —
    // dst_shard in the shared header is the relay's routing key.
    bool send(const void* data, uint32_t len) noexcept override {
        return phys_ != nullptr && phys_->send(data, len);
    }

    int32_t poll(void* buf, uint32_t buf_cap) noexcept override {
        if (head_ == tail_) return 0;
        const Slot& s = q_[head_];
        if (s.len > buf_cap) {
            // Frame too big for the caller's buffer — drop it and surface
            // an error so the caller's bad-frames counter moves.
            head_ = (head_ + 1) % kQueueFrames;
            ++drops_;
            return -1;
        }
        std::memcpy(buf, s.bytes, s.len);
        head_ = (head_ + 1) % kQueueFrames;
        return static_cast<int32_t>(s.len);
    }

    // Demux-side feed (matching thread only — same single-writer
    // discipline as every other engine component).
    void push(const void* data, uint32_t len) noexcept {
        const uint32_t next = (tail_ + 1) % kQueueFrames;
        if (next == head_) {  // full — drop newest, count loudly
            ++drops_;
            return;
        }
        Slot& s = q_[tail_];
        s.len = static_cast<uint16_t>(len);
        std::memcpy(s.bytes, data, len);
        tail_ = next;
    }

    [[nodiscard]] uint64_t drops() const noexcept { return drops_; }

    static constexpr uint32_t kFrameBytes = 128;    // > kOptCtlMaxFrame (80)
    static constexpr uint32_t kQueueFrames = 256;

private:
    struct Slot {
        uint16_t len;
        uint8_t bytes[kFrameBytes];
    };
    IpcChannel* phys_ = nullptr;
    Slot q_[kQueueFrames]{};
    uint32_t head_ = 0;
    uint32_t tail_ = 0;
    uint64_t drops_ = 0;
};

class CtlDemux final {
public:
    explicit CtlDemux(IpcChannel* phys) noexcept : phys_(phys) {
        basket_q_.bind(phys);
        opt_q_.bind(phys);
    }

    // One queue view per protocol — hand these to the coordinators as
    // their `ctl` channel.
    IpcChannel* basket_channel() noexcept { return &basket_q_; }
    IpcChannel* opt_channel() noexcept { return &opt_q_; }

    // Drain the physical channel once (bounded), routing each frame by
    // the magic at header offset 0 (CrossShardCtlHeader::magic).
    // Returns frames routed this pass.
    uint32_t drain() noexcept {
        uint32_t routed = 0;
        if (phys_ == nullptr || !phys_->is_open()) return 0;
        for (uint32_t i = 0; i < 64; ++i) {  // same bound as the coordinators
            uint8_t buf[CtlQueueChannel::kFrameBytes];
            const int32_t n = phys_->poll(buf, sizeof(buf));
            if (n <= 0) {
                if (n < 0) ++bad_frames_;
                break;
            }
            if (n < 16) {  // no full header — can't route safely
                ++bad_frames_;
                continue;
            }
            uint32_t magic = 0;
            std::memcpy(&magic, buf, sizeof(magic));
            if (magic == kBasketCtlMagic) {
                basket_q_.push(buf, static_cast<uint32_t>(n));
            } else if (magic == kOptCtlMagic) {
                opt_q_.push(buf, static_cast<uint32_t>(n));
            } else {
                ++bad_frames_;  // foreign protocol — drop, count, never guess
                continue;
            }
            ++routed;
        }
        return routed;
    }

    [[nodiscard]] uint64_t bad_frames() const noexcept { return bad_frames_; }
    [[nodiscard]] uint64_t queue_drops() const noexcept {
        return basket_q_.drops() + opt_q_.drops();
    }

private:
    IpcChannel* phys_;
    CtlQueueChannel basket_q_;
    CtlQueueChannel opt_q_;
    uint64_t bad_frames_ = 0;
};

}  // namespace exch
