#pragma once

// PHASE-01 TASK-1.3.6 STUB — binary WAL wire format per spec §3.4.
// Frame: [WalFileHeader] then repeated entries:
//   WalEntryHeader | payload[payload_len] | crc32 u32

#include <cstdint>

namespace exch {

inline constexpr uint32_t kWalMagic = 0x57414C00;
inline constexpr uint16_t kWalVersion = 1;

enum class WalEventType : uint8_t {
    ORDER_NEW,
    ORDER_CANCEL,
    ORDER_MODIFY,
    TRADE,
    TIME_TICK,      // deterministic clock tick for GTD/DAY replay (Task 2.3.10)
    BOOK_SNAPSHOT,
};

#pragma pack(push, 1)
struct WalFileHeader {
    uint32_t magic;     // kWalMagic
    uint16_t version;   // kWalVersion
    uint16_t shard_id;
};

struct WalEntryHeader {
    uint64_t seq;          // monotonic sequence
    uint64_t timestamp_ns;
    uint8_t event_type;    // WalEventType
    uint32_t payload_len;  // FlatBuffers payload follows; crc32 u32 trailer after it
};
#pragma pack(pop)

static_assert(sizeof(WalFileHeader) == 8);
static_assert(sizeof(WalEntryHeader) == 21);

}  // namespace exch
