#pragma once

// PHASE-02 STUB (Task 2.3.6) — 5s health probe driving ModeManager
// auto-transitions (spec §2.4). Probes: matching-loop p50 self-probe (the
// "core slow" ReadOnly trigger), Redis p50, PostgreSQL connectivity, WAL
// integrity, shard heartbeats, queue depth. Auto-recovery with 60s cooldown.

#include <cstdint>

namespace exch {

class ModeManager;

struct HealthReport {
    bool matching_loop_ok;
    bool redis_ok;
    bool postgres_ok;
    bool wal_ok;
    bool workers_ok;
    uint32_t queue_depth;
};

class HealthChecker {
public:
    explicit HealthChecker(ModeManager& modes) noexcept;

    // Stub: all-healthy report; probe wiring lands in Phase-02.
    [[nodiscard]] HealthReport check_once() const noexcept;

private:
    ModeManager& modes_;
};

}  // namespace exch
