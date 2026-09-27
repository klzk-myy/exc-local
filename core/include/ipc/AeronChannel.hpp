#pragma once

// PHASE-01 TASK-1.3.5 STUB — Aeron `aeron:ipc` transport (orders_in /
// orders_out aliases). SharedMemChannel fallback is also Task 1.3.5.
// Compiled against third_party/aeron when EXCH_WITH_AERON=1; open() returns
// false on stub builds.

#include <cstdint>
#include <string>
#include <string_view>

#include "ipc/IpcChannel.hpp"

namespace exch {

class AeronChannel final : public IpcChannel {
public:
    AeronChannel(std::string_view in_uri, std::string_view out_uri);
    ~AeronChannel() override;

    bool open() noexcept override;
    void close() noexcept override;
    [[nodiscard]] bool is_open() const noexcept override { return open_; }

    bool send(const void* data, uint32_t len) noexcept override;
    int32_t poll(void* buf, uint32_t buf_cap) noexcept override;

    [[nodiscard]] const std::string& in_uri() const noexcept { return in_uri_; }
    [[nodiscard]] const std::string& out_uri() const noexcept { return out_uri_; }

private:
    std::string in_uri_;
    std::string out_uri_;
    bool open_ = false;
};

}  // namespace exch
