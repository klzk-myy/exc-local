#pragma once

// PHASE-02 STUB (Task 2.3.5) — per-shard leader election:
// SETNX engine:leader:{shardId} (10s TTL, value = pid:instanceId), heartbeat
// EXPIRE every 3s + leader:heartbeat:{shardId} 15s TTL, 5s startup settle,
// split-brain -> both stop matching + P1 + ReadOnly.

#include <cstdint>

namespace exch {

enum class LeaderRole : uint8_t { FOLLOWER, LEADER };

class LeaderElection {
public:
    explicit LeaderElection(uint32_t shard_id) noexcept;

    // Stub: no Redis client yet — acquire() stays FOLLOWER.
    bool acquire() noexcept;
    void heartbeat() noexcept;

    [[nodiscard]] uint32_t shard_id() const noexcept { return shard_id_; }
    [[nodiscard]] LeaderRole role() const noexcept { return role_; }
    [[nodiscard]] bool is_leader() const noexcept { return role_ == LeaderRole::LEADER; }

private:
    uint32_t shard_id_;
    LeaderRole role_ = LeaderRole::FOLLOWER;
};

}  // namespace exch
