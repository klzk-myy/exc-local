// ShardMap (Task 1.3.7, spec §2.2). See include/utils/ShardMap.hpp for the
// contract. The parser below intentionally supports only the flat subset
// of YAML used by config/sharding.yaml: top-level `key: scalar` lines,
// `section:` blocks, `shard_id: [a, b]` flow lists, `- item` block list
// entries and `#` comments. Anything else is a parse error — the file is
// ours and a malformed shard map must fail closed, not silently parse
// differently from the Go side.

#include "utils/ShardMap.hpp"

#include <cctype>
#include <charconv>
#include <cstdlib>
#include <fstream>
#include <mutex>
#include <sstream>
#include <stdexcept>
#include <system_error>

namespace exch {
namespace {

[[noreturn]] void fail(const std::string& path, size_t line, const std::string& what) {
    std::ostringstream oss;
    oss << "shard map: " << path << ":" << line << ": " << what;
    throw std::runtime_error(oss.str());
}

std::string_view trim(std::string_view s) noexcept {
    while (!s.empty() && (s.front() == ' ' || s.front() == '\t' || s.front() == '\r'))
        s.remove_prefix(1);
    while (!s.empty() && (s.back() == ' ' || s.back() == '\t' || s.back() == '\r'))
        s.remove_suffix(1);
    return s;
}

std::string unquote(std::string_view s) {
    s = trim(s);
    if (s.size() >= 2 &&
        ((s.front() == '"' && s.back() == '"') || (s.front() == '\'' && s.back() == '\'')))
        s = s.substr(1, s.size() - 2);
    return std::string(s);
}

// Strict unsigned parse — no signs, no whitespace, full consumption.
uint64_t parseUint(std::string_view s, const std::string& path, size_t line,
                   const char* what) {
    s = trim(s);
    if (s.empty())
        fail(path, line, std::string("empty ") + what);
    uint64_t v = 0;
    const auto* first = s.data();
    const auto* last = s.data() + s.size();
    auto [ptr, ec] = std::from_chars(first, last, v);
    if (ec != std::errc{} || ptr != last)
        fail(path, line, std::string("invalid ") + what + " '" + std::string(s) + "'");
    return v;
}

void addSymbol(ShardMap::Table& t, uint16_t shard, std::string_view raw,
               const std::string& path, size_t line) {
    const std::string canon = ShardMap::canonicalize(raw);
    if (canon.empty())
        fail(path, line, "invalid symbol '" + std::string(raw) + "'");
    auto [it, inserted] = t.by_symbol.emplace(canon, shard);
    if (!inserted)
        fail(path, line, "symbol " + canon + " assigned to shards " +
                             std::to_string(it->second) + " and " + std::to_string(shard));
}

// Parses a `shard_id: [a, b]` flow list, a `shard_id: SYM` scalar, or a
// bare `shard_id:` (block list continues via `- item` lines).
void parseShardLine(ShardMap::Table& t, std::string_view key, std::string_view value,
                    const std::string& path, size_t line, int& cur_shard) {
    const uint64_t id = parseUint(key, path, line, "shard id");
    if (id > 1023)
        fail(path, line, "shard id " + std::to_string(id) + " out of range 0-1023");
    cur_shard = static_cast<int>(id);
    value = trim(value);
    if (value.empty())
        return;  // block-list form: subsequent "- SYM" lines append
    if (value.front() == '[') {
        if (value.back() != ']')
            fail(path, line, "unterminated flow list");
        value = value.substr(1, value.size() - 2);
        size_t pos = 0;
        while (pos <= value.size()) {
            const size_t comma = value.find(',', pos);
            const auto item = trim(value.substr(
                pos, comma == std::string_view::npos ? comma : comma - pos));
            if (!item.empty())
                addSymbol(t, cur_shard, item, path, line);
            if (comma == std::string_view::npos)
                break;
            pos = comma + 1;
        }
        return;
    }
    addSymbol(t, cur_shard, value, path, line);  // bare scalar: `3: EUR/GBP`
}

std::shared_ptr<ShardMap::Table> parseFile(const std::string& path) {
    std::ifstream in(path);
    if (!in)
        throw std::runtime_error("shard map: cannot open " + path);

    auto table = std::make_shared<ShardMap::Table>();
    bool have_base = false, have_count = false, have_shard = false;
    std::string section;
    int cur_shard = -1;

    std::string line;
    for (size_t lineno = 1; std::getline(in, line); ++lineno) {
        // Comment: '#' at start or after whitespace (symbols never carry '#').
        for (size_t i = 0; i < line.size(); ++i) {
            if (line[i] == '#' && (i == 0 || line[i - 1] == ' ' || line[i - 1] == '\t')) {
                line.resize(i);
                break;
            }
        }
        const size_t first = line.find_first_not_of(" \t\r");
        if (first == std::string::npos)
            continue;
        const size_t indent = first;
        std::string_view sv(line.data() + first, line.size() - first);
        sv = trim(sv);
        if (sv.empty())
            continue;

        if (sv.front() == '-') {  // block list item
            if (section != "shards" || cur_shard < 0)
                fail(path, lineno, "list item outside a shard list");
            addSymbol(*table, static_cast<uint16_t>(cur_shard), unquote(sv.substr(1)),
                      path, lineno);
            have_shard = true;
            continue;
        }

        const size_t colon = sv.find(':');
        if (colon == std::string_view::npos)
            fail(path, lineno, "expected 'key: value'");
        const std::string_view key = trim(sv.substr(0, colon));
        const std::string_view value = trim(sv.substr(colon + 1));

        if (indent == 0) {
            if (value.empty()) {  // section header: `shards:` / `elastic:`
                section = std::string(key);
                cur_shard = -1;
                continue;
            }
            if (key == "version") {
                const uint64_t ver = parseUint(value, path, lineno, "version");
                if (ver < 1)
                    fail(path, lineno, "version must be >= 1");
                continue;
            }
            continue;  // forward-compatible: ignore unknown top-level scalars
        }

        if (section == "shards") {
            parseShardLine(*table, key, value, path, lineno, cur_shard);
            have_shard = true;
        } else if (section == "elastic") {
            const uint64_t v = parseUint(value, path, lineno, "elastic parameter");
            if (key == "base_shard") {
                table->elastic_base = static_cast<uint16_t>(v);
                have_base = true;
            } else if (key == "num_shards") {
                table->elastic_count = static_cast<uint16_t>(v);
                have_count = true;
            } else {
                fail(path, lineno, "unknown elastic key '" + std::string(key) + "'");
            }
        } else {
            fail(path, lineno, "entry in unknown section '" + section + "'");
        }
    }

    if (!have_shard)
        throw std::runtime_error("shard map: " + path + ": no shard entries");
    if (table->elastic_count == 0)
        throw std::runtime_error("shard map: " + path + ": elastic.num_shards must be >= 1");
    // uint16 range check for base + count - 1.
    if (static_cast<uint32_t>(table->elastic_base) + table->elastic_count - 1 > 65535)
        throw std::runtime_error("shard map: " + path +
                                 ": elastic range exceeds uint16 shard ids");
    (void)have_base;
    (void)have_count;  // absent → defaults (4, 4) already in Table
    return table;
}

}  // namespace

std::string ShardMap::canonicalize(std::string_view symbol) {
    std::string out;
    out.reserve(symbol.size() + 1);
    for (const char c : symbol) {
        if (c >= 'a' && c <= 'z')
            out.push_back(static_cast<char>(c - 'a' + 'A'));
        else if ((c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9'))
            out.push_back(c);
    }
    if (out.size() == 6)
        out.insert(3, 1, '/');
    return out;
}

uint32_t ShardMap::fnv1a32(std::string_view bytes) noexcept {
    uint32_t h = 2166136261u;
    for (const char c : bytes)
        h = (h ^ static_cast<uint8_t>(c)) * 16777619u;
    return h;
}

ShardMap::ShardMap(ShardMap&& o) noexcept {
    std::unique_lock lock(o.mu_);
    path_ = std::move(o.path_);
    table_ = std::move(o.table_);
    // Keep the moved-from object usable (getShard never sees null).
    o.table_ = std::make_shared<Table>();
}

ShardMap& ShardMap::operator=(ShardMap&& o) noexcept {
    if (this == &o)
        return *this;
    std::scoped_lock lock(mu_, o.mu_);
    path_ = std::move(o.path_);
    table_ = std::move(o.table_);
    o.table_ = std::make_shared<Table>();
    return *this;
}

ShardMap ShardMap::loadFromFile(const std::string& path) {
    ShardMap m;
    m.path_ = path;
    m.table_ = parseFile(path);
    return m;
}

ShardMap ShardMap::loadDefault() {
    if (const char* env = std::getenv("EXC_SHARDING_CONFIG"); env && *env)
        return loadFromFile(env);
    static const char* kCandidates[] = {
        "./config/sharding.yaml", "./sharding.yaml", "../config/sharding.yaml",
        "../../config/sharding.yaml", "../../../config/sharding.yaml",
    };
    for (const char* c : kCandidates) {
        std::ifstream probe(c);
        if (probe.good())
            return loadFromFile(c);
    }
    throw std::runtime_error(
        "shard map: no sharding.yaml found (searched ./config, ../config, "
        "../../config, ../../../config; set EXC_SHARDING_CONFIG)");
}

uint16_t ShardMap::getShard(std::string_view symbol) const noexcept {
    std::shared_ptr<const Table> table;
    try {
        std::shared_lock lock(mu_);
        table = table_;
        std::string canon = canonicalize(symbol);
        lock.unlock();
        if (const auto it = table->by_symbol.find(canon); it != table->by_symbol.end())
            return it->second;
        return static_cast<uint16_t>(table->elastic_base +
                                     fnv1a32(canon) % table->elastic_count);
    } catch (...) {
        // shared_lock/canonicalize can only fail on resource exhaustion —
        // hash the raw view so lookups stay total + noexcept.
        const auto t = std::make_shared<Table>();
        return static_cast<uint16_t>(t->elastic_base +
                                     fnv1a32(symbol) % t->elastic_count);
    }
}

uint16_t ShardMap::getShard(const char* symbol) const noexcept {
    return getShard(std::string_view(symbol ? symbol : ""));
}

void ShardMap::reload() {
    if (path_.empty())
        throw std::runtime_error("shard map: reload requires a file-backed map");
    // parseFile throws before the swap: the previous table stays live.
    auto next = parseFile(path_);
    std::unique_lock lock(mu_);
    table_ = std::move(next);
}

size_t ShardMap::numStatic() const noexcept {
    std::shared_lock lock(mu_);
    return table_->by_symbol.size();
}

uint16_t ShardMap::elasticBase() const noexcept {
    std::shared_lock lock(mu_);
    return table_->elastic_base;
}

uint16_t ShardMap::elasticCount() const noexcept {
    std::shared_lock lock(mu_);
    return table_->elastic_count;
}

}  // namespace exch
