#pragma once

// Task 2.3.6 — Redis binding of the ModeStore seam: the spec §4.2
// coordination keys `system:degradation:mode` / `:entered_at` / `:reason`,
// written as one atomic MSET per transition (same key space the Go
// coordination client reads — services/internal/redis/client.go
// GetDegradationMode — so the Go X-Degradation-Mode middleware sees C++-
// authored transitions with zero impedance).

#include "degradation/ModeManager.hpp"

namespace exch {

class RespClient;

// Non-owning: the client must outlive the store.
class RedisModeStore final : public ModeStore {
public:
    explicit RedisModeStore(RespClient* client) noexcept;

    bool write(const ModeRecord& rec) noexcept override;
    bool read(ModeRecord* out) noexcept override;

private:
    RespClient* client_;
};

}  // namespace exch
