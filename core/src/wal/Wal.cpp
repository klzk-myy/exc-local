// PHASE-01 TASK-1.3.6 STUB — mmap/fsync/CRC32 implementation lands there.
#include "wal/Wal.hpp"

namespace exch {

Wal::Wal(std::string_view path, uint16_t shard_id)
    : path_(path), shard_id_(shard_id) {}

}  // namespace exch
