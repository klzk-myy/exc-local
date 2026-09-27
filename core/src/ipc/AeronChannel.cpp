// PHASE-01 TASK-1.3.5 STUB — Aeron connect/publish/subscribe lands there.
#include "ipc/AeronChannel.hpp"

#if EXCH_WITH_AERON
#include <Aeron.h>
#endif

namespace exch {

AeronChannel::AeronChannel(std::string_view in_uri, std::string_view out_uri)
    : in_uri_(in_uri), out_uri_(out_uri) {}

AeronChannel::~AeronChannel() = default;

bool AeronChannel::open() noexcept {
#if EXCH_WITH_AERON
    // Link proof only; media-driver attach + publications are Task 1.3.5.
    (void)aeron::Aeron::version();
#endif
    return false;
}

void AeronChannel::close() noexcept { open_ = false; }

bool AeronChannel::send(const void* /*data*/, uint32_t /*len*/) noexcept {
    return false;
}

int32_t AeronChannel::poll(void* /*buf*/, uint32_t /*buf_cap*/) noexcept {
    return 0;
}

}  // namespace exch
