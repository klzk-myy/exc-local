// PHASE-04 STUB (Tasks 4.3.5/4.3.9) — graduated recovery ladder lands there.
#include "recovery/RecoveryManager.hpp"

namespace exch {

RecoveryManager::RecoveryManager(uint32_t shard_id) noexcept : shard_id_(shard_id) {}

RecoveryOutcome RecoveryManager::recover() noexcept {
    return RecoveryOutcome::CLEAN;
}

}  // namespace exch
