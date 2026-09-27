#pragma once

// PHASE-01 TASK-1.3.6 STUB — custom binary WAL: mmap(MAP_SHARED) + fsync batch
// (1ms / 100 events), O_DIRECT with 4KB-aligned posix_memalign block flushing,
// CRC32 per entry, 1GB rotation, S3 archive before trim (spec §3.4).
// Engine integration: Phase-02 Task 2.3.4. Archive/replay: Phase-04.

#include <cstdint>
#include <string>
#include <string_view>

#include "wal/WalEntry.hpp"

namespace exch {

class Wal {
public:
    Wal(std::string_view path, uint16_t shard_id);

    // PHASE-01 TASK-1.3.6: open()/append()/flush()/rotate() land there.
    [[nodiscard]] bool is_open() const noexcept { return open_; }
    [[nodiscard]] uint16_t shard_id() const noexcept { return shard_id_; }
    [[nodiscard]] uint64_t tail_seq() const noexcept { return tail_seq_; }
    [[nodiscard]] const std::string& path() const noexcept { return path_; }

private:
    std::string path_;
    uint16_t shard_id_;
    bool open_ = false;
    uint64_t tail_seq_ = 0;
};

}  // namespace exch
