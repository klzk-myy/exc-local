// Smoke-level coverage; mmap/CRC32/rotation land with Phase-01 Task 1.3.6.
#include <gtest/gtest.h>

#include <cstdint>

#include "wal/Wal.hpp"
#include "wal/WalEntry.hpp"

using namespace exch;

TEST(WalSmoke, WireFormatConstants) {
    EXPECT_EQ(kWalMagic, 0x57414C00u);
    EXPECT_EQ(kWalVersion, 1u);
    static_assert(sizeof(WalFileHeader) == 8);
    static_assert(sizeof(WalEntryHeader) == 21);
    SUCCEED();
}

TEST(WalSmoke, EventTypesPresent) {
    EXPECT_EQ(static_cast<uint8_t>(WalEventType::ORDER_NEW), 0);
    EXPECT_EQ(static_cast<uint8_t>(WalEventType::TIME_TICK), 4);
    EXPECT_EQ(static_cast<uint8_t>(WalEventType::BOOK_SNAPSHOT), 5);
}

TEST(WalSmoke, ConstructNotOpen) {
    Wal wal("var/wal/shard2.wal", 2);
    EXPECT_EQ(wal.shard_id(), 2u);
    EXPECT_FALSE(wal.is_open());
    EXPECT_EQ(wal.tail_seq(), 0u);
    EXPECT_EQ(wal.path(), "var/wal/shard2.wal");
}
