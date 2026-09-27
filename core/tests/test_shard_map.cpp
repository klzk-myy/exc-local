// Tests for Task 1.3.7 ShardMap (spec §2.2). The canonical table is the
// real repo-root config/sharding.yaml, located via the EXC_SHARDING_YAML
// compile definition CMake bakes in; temp files cover malformed inputs,
// elastic hashing and reload semantics.

#include <gtest/gtest.h>

#include <cstdint>
#include <filesystem>
#include <fstream>
#include <string>

#include "utils/ShardMap.hpp"

namespace {

std::string writeTempYaml(const std::string& name, const std::string& body) {
    const auto dir = std::filesystem::temp_directory_path() / "exch_shardmap_tests";
    std::filesystem::create_directories(dir);
    const auto path = dir / name;
    std::ofstream out(path);
    out << body;
    out.close();
    return path.string();
}

}  // namespace

TEST(ShardMap, CanonicalRepoFile) {
    const auto m = exch::ShardMap::loadFromFile(EXC_SHARDING_YAML);
    // spec §2.2 table, verbatim.
    const std::pair<const char*, uint16_t> want[] = {
        {"EUR/USD", 0}, {"GBP/USD", 0}, {"USD/CHF", 0},
        {"USD/JPY", 1}, {"AUD/USD", 1}, {"NZD/USD", 1},
        {"USD/CAD", 2}, {"USD/MXN", 2}, {"USD/BRL", 2},
        {"EUR/GBP", 3}, {"EUR/JPY", 3}, {"EUR/CHF", 3},
    };
    for (const auto& [sym, shard] : want)
        EXPECT_EQ(m.getShard(sym), shard) << sym;
    EXPECT_EQ(m.numStatic(), 12u);
    EXPECT_EQ(m.elasticBase(), 4);
    EXPECT_GE(m.elasticCount(), 1);
}

TEST(ShardMap, Canonicalize) {
    EXPECT_EQ(exch::ShardMap::canonicalize("EUR/USD"), "EUR/USD");
    EXPECT_EQ(exch::ShardMap::canonicalize("EURUSD"), "EUR/USD");
    EXPECT_EQ(exch::ShardMap::canonicalize("eur-usd"), "EUR/USD");
    EXPECT_EQ(exch::ShardMap::canonicalize("USD/BRL-1W"), "USDBRL1W");
    EXPECT_EQ(exch::ShardMap::canonicalize(""), "");
}

// Locks the hash algorithm so the Go and C++ elastic policies cannot
// silently diverge (both must agree on the shard for an unmapped symbol).
TEST(ShardMap, Fnv1a32KnownVectors) {
    EXPECT_EQ(exch::ShardMap::fnv1a32(""), 2166136261u);
    EXPECT_EQ(exch::ShardMap::fnv1a32("a"), 0xE40C292Cu);       // 3826002220
    EXPECT_EQ(exch::ShardMap::fnv1a32("EUR/USD"), 0xE1DE52F8u); // recorded vector
}

TEST(ShardMap, BlockListAndScalarForms) {
    const auto path = writeTempYaml("block.yaml", R"yaml(
version: 1
elastic:
  base_shard: 10
  num_shards: 2
shards:
  0:
    - EUR/USD
    - GBP/USD
  1: USD/JPY
  2: []
)yaml");
    const auto m = exch::ShardMap::loadFromFile(path);
    EXPECT_EQ(m.getShard("EUR/USD"), 0);
    EXPECT_EQ(m.getShard("GBP/USD"), 0);
    EXPECT_EQ(m.getShard("USD/JPY"), 1);
    EXPECT_EQ(m.getShard("usd.jpy"), 1);  // normalization across spellings
    EXPECT_EQ(m.numStatic(), 3u);
    EXPECT_EQ(m.elasticBase(), 10);
    EXPECT_EQ(m.elasticCount(), 2);
}

// SDD edge case: unknown symbol → deterministic shard in the elastic
// range; identical inputs must always land on the same shard.
TEST(ShardMap, UnknownSymbolIsElasticAndDeterministic) {
    const auto m = exch::ShardMap::loadFromFile(EXC_SHARDING_YAML);
    for (const char* sym : {"USD/TRY", "USD/ZAR", "EURUSD-1W"}) {
        const uint16_t got = m.getShard(sym);
        EXPECT_GE(got, m.elasticBase()) << sym;
        EXPECT_LT(got, m.elasticBase() + m.elasticCount()) << sym;
        EXPECT_EQ(m.getShard(sym), got) << sym << " not deterministic";
    }
}

TEST(ShardMap, ParseErrorsFailClosed) {
    EXPECT_THROW(exch::ShardMap::loadFromFile("/nonexistent/sharding.yaml"),
                 std::runtime_error);
    const auto dup = writeTempYaml("dup.yaml",
                                   "shards:\n  0: [EUR/USD]\n  1: [EURUSD]\n");
    EXPECT_THROW(exch::ShardMap::loadFromFile(dup), std::runtime_error);
    const auto empty = writeTempYaml("empty.yaml", "version: 1\n");
    EXPECT_THROW(exch::ShardMap::loadFromFile(empty), std::runtime_error);
    const auto bad = writeTempYaml("bad.yaml",
                                   "shards:\n  x: [EUR/USD]\n");
    EXPECT_THROW(exch::ShardMap::loadFromFile(bad), std::runtime_error);
}

// SDD edge case: config reload swaps atomically; a broken reload keeps the
// previous table.
TEST(ShardMap, ReloadSwapsAndKeepsOnError) {
    const auto path = writeTempYaml("reload.yaml", R"yaml(
elastic:
  base_shard: 4
  num_shards: 2
shards:
  0: [EUR/USD]
)yaml");
    auto m = exch::ShardMap::loadFromFile(path);
    EXPECT_EQ(m.getShard("EUR/USD"), 0);

    {   std::ofstream out(path);
        out << "elastic:\n  base_shard: 8\n  num_shards: 2\nshards:\n  7: [EUR/USD]\n"; }
    ASSERT_NO_THROW(m.reload());
    EXPECT_EQ(m.getShard("EUR/USD"), 7);
    EXPECT_EQ(m.elasticBase(), 8);

    {   std::ofstream out(path);
        out << "shards:\n  0: [EUR/USD]\n  1: [EUR/USD]\n"; }
    EXPECT_THROW(m.reload(), std::runtime_error);
    EXPECT_EQ(m.getShard("EUR/USD"), 7);  // old table retained
}
