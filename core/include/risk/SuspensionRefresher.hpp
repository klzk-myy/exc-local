#pragma once

// Phase-11 Task 11.3.4/11.3.12 — control-path poller for the Redis
// `halt:*` kill-switch flag space. The caller owns the poll cadence
// (a control thread in main.cpp, default 50ms); the matching thread
// only ever reads the SuspensionFlags snapshot, never the socket.

#include <memory>

#include "redis/RespClient.hpp"
#include "risk/SuspensionFlags.hpp"

namespace exch {

class SuspensionRefresher {
public:
    // Non-owning: client may be nullptr (the refresher then always
    // marks unverifiable — bound-but-unwired still fails closed).
    explicit SuspensionRefresher(RespClient* client) noexcept;

    // One poll round: KEYS halt:* -> snapshot -> apply. Returns false on
    // any transport/parse failure AFTER marking flags unverifiable.
    bool refresh(SuspensionFlags* flags) noexcept;

private:
    RespClient* client_;
};

}  // namespace exch
