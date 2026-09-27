#pragma once

// ShardMap (Task 1.3.7, spec §2.2): assigns a currency-pair symbol to its
// engine shard id. Loads the same repo-root config/sharding.yaml the Go
// services use — the file is intentionally flat (top-level scalars plus
// `shard_id: [SYM, ...]` flow lists or `- SYM` block entries) so a minimal
// parser suffices; no yaml dependency is added to the core.
//
// Symbol resolution contract (mirrored in services/internal/config/
// sharding.go — keep bit-identical):
//   1. Canonicalize: keep [A-Za-z0-9], uppercase; a 6-char result becomes
//      "BASE/QUOTE"; anything else stays compact (e.g. "EURUSD-1W" →
//      "EURUSD1W").
//   2. Static table lookup.
//   3. Elastic (spec §2.2 "4+"): elastic_base + fnv1a32(canonical) %
//      elastic_count — deterministic across Go and C++ without Redis.
//
// Reload re-parses the file and swaps the table atomically (the hot path
// snapshots an immutable std::shared_ptr<const Table> under a shared lock).

#include <cstdint>
#include <memory>
#include <shared_mutex>
#include <string>
#include <string_view>
#include <unordered_map>
#include <vector>

namespace exch {

class ShardMap {
public:
    ShardMap() = default;  // empty map: everything resolves via elastic defaults
    ShardMap(ShardMap&& o) noexcept;
    ShardMap& operator=(ShardMap&& o) noexcept;
    ShardMap(const ShardMap&) = delete;
    ShardMap& operator=(const ShardMap&) = delete;

    // One parsed snapshot. Kept as a struct so the hot path can hold an
    // immutable shared_ptr and never lock.
    struct Table {
        std::unordered_map<std::string, uint16_t> by_symbol;
        uint16_t elastic_base = 4;
        uint16_t elastic_count = 4;
    };

    // Parse path (config/sharding.yaml). Throws std::runtime_error on any
    // IO/parse/validation failure — fail-closed per spec §2.7: a wrong
    // shard map is worse than refusing to start.
    static ShardMap loadFromFile(const std::string& path);

    // Search order: $EXC_SHARDING_CONFIG (explicit; missing = throw), then
    // ./config/sharding.yaml, ./sharding.yaml, ../config/sharding.yaml,
    // ../../config/sharding.yaml, ../../../config/sharding.yaml.
    static ShardMap loadDefault();

    // Shard for a symbol. noexcept: static lookup or FNV-1a elastic hash.
    [[nodiscard]] uint16_t getShard(const char* symbol) const noexcept;
    [[nodiscard]] uint16_t getShard(std::string_view symbol) const noexcept;

    // Re-parse the backing file and atomically swap the table (SDD edge
    // case: config reload). Throws and keeps the current table on failure.
    void reload();

    // --- introspection (tests, ops logging) ------------------------------
    [[nodiscard]] size_t numStatic() const noexcept;
    [[nodiscard]] uint16_t elasticBase() const noexcept;
    [[nodiscard]] uint16_t elasticCount() const noexcept;
    [[nodiscard]] const std::string& path() const noexcept { return path_; }

    // Canonicalization, exposed for tests and for parity checks against
    // the Go implementation.
    static std::string canonicalize(std::string_view symbol);
    static uint32_t fnv1a32(std::string_view bytes) noexcept;

private:
    std::string path_;
    // Always non-null: a default-constructed map resolves every symbol via
    // the default elastic range (base 4, count 4) instead of crashing.
    // getShard snapshots the immutable table under a shared lock; reload
    // swaps it under an exclusive lock.
    mutable std::shared_mutex mu_;
    std::shared_ptr<const Table> table_ = std::make_shared<Table>();
};

}  // namespace exch
