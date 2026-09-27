#pragma once

// Task 2.3.6 — thin IPC seam for degradation-mode transitions.
//
// ModeManager::set_transition_sink broadcasts every applied transition;
// ModeEventPublisher is the adapter that serializes the transition onto an
// IpcChannel (Aeron in production, SharedMemChannel fallback — Task 1.3.5).
// Phase-06 subscribes on the market-data/WS side and republishes the payload
// verbatim as the WS `system.status` event (spec §2.4 "Client Visibility").
//
// Payload contract (JSON, self-describing for the Phase-06 republisher):
//   {"type":"system.status","event":"degradation_mode",
//    "mode":"<To>","prev":"<From>","reason":"<r>","ts_ms":<epoch_ms>}
//
// Publish failure never blocks or fails the transition — Redis
// (system:degradation:*) is the authoritative record; the stream is
// observability. The sink signature matches ModeManager::transition_fn so it
// can be wired as `&ModeEventPublisher::sink` with `this` as ctx.

#include <cstdint>
#include <string>
#include <string_view>

#include "degradation/ModeManager.hpp"

namespace exch {

class IpcChannel;

class ModeEventPublisher {
public:
    // `channel` is non-owning; nullptr channel degrades publish() to false.
    explicit ModeEventPublisher(IpcChannel* channel) noexcept;

    // Serialize + offer one transition frame on the channel. false = channel
    // absent/closed/full — the caller (ModeManager sink path) ignores the
    // result deliberately.
    bool publish(DegradationMode from, DegradationMode to,
                 std::string_view reason, int64_t entered_at_ms) noexcept;

    // ModeManager::transition_fn adapter.
    static void sink(void* ctx, DegradationMode from, DegradationMode to,
                     std::string_view reason, int64_t entered_at_ms) noexcept;

    // The serialized frame — factored out so tests assert the contract
    // without a channel.
    [[nodiscard]] static std::string encode(DegradationMode from,
                                            DegradationMode to,
                                            std::string_view reason,
                                            int64_t entered_at_ms);

private:
    IpcChannel* channel_;
};

}  // namespace exch
