// Task 2.3.6 — transition-event serializer + IpcChannel adapter.
// See header for the JSON frame contract Phase-06 republishes as WS
// `system.status`.

#include "degradation/ModeEventPublisher.hpp"

#include <cstdio>

#include "ipc/IpcChannel.hpp"

namespace exch {

ModeEventPublisher::ModeEventPublisher(IpcChannel* channel) noexcept
    : channel_(channel) {}

std::string ModeEventPublisher::encode(DegradationMode from, DegradationMode to,
                                       std::string_view reason,
                                       int64_t entered_at_ms) {
    // Reasons are engine-authored tokens, but the frame is JSON — strip any
    // char that could break the envelope rather than escape (fail-safe and
    // branch-free enough for a control-path call).
    std::string safe_reason;
    safe_reason.reserve(reason.size());
    for (const char c : reason) {
        const bool ok = (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') ||
                        (c >= '0' && c <= '9') || c == '_' || c == '-' ||
                        c == '.' || c == ';' || c == ' ';
        safe_reason += ok ? c : '_';
    }
    char buf[512];
    const int n = std::snprintf(
        buf, sizeof(buf),
        "{\"type\":\"system.status\",\"event\":\"degradation_mode\","
        "\"mode\":\"%.*s\",\"prev\":\"%.*s\",\"reason\":\"%s\",\"ts_ms\":%lld}",
        static_cast<int>(to_string(to).size()), to_string(to).data(),
        static_cast<int>(to_string(from).size()), to_string(from).data(),
        safe_reason.c_str(), static_cast<long long>(entered_at_ms));
    if (n <= 0) return {};
    if (n >= static_cast<int>(sizeof(buf))) {
        return {buf, sizeof(buf) - 1};  // truncated frame still parses prefix
    }
    return {buf, static_cast<size_t>(n)};
}

bool ModeEventPublisher::publish(DegradationMode from, DegradationMode to,
                                 std::string_view reason,
                                 int64_t entered_at_ms) noexcept {
    if (channel_ == nullptr || !channel_->is_open()) return false;
    const std::string frame = encode(from, to, reason, entered_at_ms);
    return channel_->send(frame.data(), static_cast<uint32_t>(frame.size()));
}

void ModeEventPublisher::sink(void* ctx, DegradationMode from,
                              DegradationMode to, std::string_view reason,
                              int64_t entered_at_ms) noexcept {
    auto* self = static_cast<ModeEventPublisher*>(ctx);
    if (self != nullptr) {
        (void)self->publish(from, to, reason, entered_at_ms);
    }
}

}  // namespace exch
