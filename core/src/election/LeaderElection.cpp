// PHASE-02 STUB (Task 2.3.5) — Redis SETNX election lands there.
#include "election/LeaderElection.hpp"

namespace exch {

LeaderElection::LeaderElection(uint32_t shard_id) noexcept : shard_id_(shard_id) {}

bool LeaderElection::acquire() noexcept { return false; }

void LeaderElection::heartbeat() noexcept {}

}  // namespace exch
