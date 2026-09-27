#pragma once

// PHASE-04 STUB (Tasks 4.3.5/4.3.9) — crash recovery per spec §3.5:
// load snapshot → replay WAL → verify book_seq == wal_tail → graduated ladder
// (CRC repair → snapshot rebase → fail-closed MarketDataOnly halt +
// recovery_report + P1 alert). Wired into boot by Phase-02 Task 2.3.4.

#include <cstdint>

namespace exch {

enum class RecoveryOutcome : uint8_t {
    CLEAN,             // invariant held — resume matching
    WAL_REPAIRED,      // truncated at last CRC-valid entry
    SNAPSHOT_REBASED,  // reload + forward replay
    HALTED,            // fail-closed halt (WAL_RECOVERY_HALT state)
};

class RecoveryManager {
public:
    explicit RecoveryManager(uint32_t shard_id) noexcept;

    // Stub: empty WAL → CLEAN.
    [[nodiscard]] RecoveryOutcome recover() noexcept;

    [[nodiscard]] uint32_t shard_id() const noexcept { return shard_id_; }
    [[nodiscard]] uint64_t snapshot_seq() const noexcept { return snapshot_seq_; }
    [[nodiscard]] uint64_t wal_tail() const noexcept { return wal_tail_; }

private:
    uint32_t shard_id_;
    uint64_t snapshot_seq_ = 0;
    uint64_t wal_tail_ = 0;
};

}  // namespace exch
