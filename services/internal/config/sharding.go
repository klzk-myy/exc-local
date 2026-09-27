// Shard mapping (Task 1.3.7, spec §2.2).
//
// The canonical symbol → shard table lives in config/sharding.yaml at the
// repo root; the same file is parsed by the C++ engine
// (core/src/utils/ShardMap.cpp) with its own minimal parser, so the YAML
// structure is intentionally flat: top-level `version`, an `elastic`
// section (`base_shard`, `num_shards`) and a `shards` map of
// shard-id → list of canonical "BASE/QUOTE" symbols.
//
// Runtime contract:
//   - `exchange cache-shard-map` writes the file's map to the Redis
//     `shard:map` HASH (one field per symbol plus meta:* fields).
//   - Services call LoadShardMapForService at startup: Redis first,
//     config file fallback on cache miss or Redis outage (SDD edge case:
//     "Redis cache miss"). The file remains the source of truth; Redis is
//     a distribution cache for processes that cannot see the repo file.
//   - Symbols absent from the static table are elastic (spec §2.2 "4+"):
//     base_shard + fnv1a32(CanonicalSymbol) % num_shards. FNV-1a is
//     implemented bit-for-bit identically in Go and C++ so every process
//     derives the same shard for an unmapped/exotic symbol without
//     consulting Redis.
package config

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"

	goredis "github.com/redis/go-redis/v9"
	"github.com/spf13/viper"
)

// ShardMapRedisKey is the HASH every service reads at startup.
const ShardMapRedisKey = "shard:map"

// Meta fields inside the shard:map hash carry the elastic policy so
// Redis-only readers resolve unknown symbols identically to file readers.
// Symbol fields always contain "/" in canonical form; meta fields are the
// only keys beginning "meta:".
const (
	shardMapMetaVersion = "meta:version"
	shardMapMetaBase    = "meta:elastic_base"
	shardMapMetaCount   = "meta:elastic_count"
)

// Elastic defaults applied when the `elastic` section is absent. Spec §2.2
// reserves shards 0-3 for the static groups and "4+" for elastic.
const (
	DefaultElasticBase  = 4
	DefaultElasticCount = 4
)

// ErrShardMapCacheMiss distinguishes "key absent/empty" (fall back to the
// config file) from a real Redis failure.
var ErrShardMapCacheMiss = errors.New("shard map: redis shard:map empty or absent")

// ShardMapSource records where a successfully loaded ShardMap came from.
type ShardMapSource int

const (
	ShardMapSourceFile ShardMapSource = iota
	ShardMapSourceRedis
)

func (s ShardMapSource) String() string {
	switch s {
	case ShardMapSourceRedis:
		return "redis"
	default:
		return "file"
	}
}

// ShardMap maps a currency-pair symbol to its engine shard id. The static
// table is immutable after load except via Reload, which swaps it
// atomically; GetShard is safe for concurrent use.
type ShardMap struct {
	path         string // file it was loaded from ("" when loaded from Redis)
	mu           sync.RWMutex
	bySymbol     map[string]int
	elasticBase  int
	elasticCount int
}

// shardFile mirrors config/sharding.yaml.
type shardFile struct {
	Version int `mapstructure:"version"`
	Elastic struct {
		BaseShard int `mapstructure:"base_shard"`
		NumShards int `mapstructure:"num_shards"`
	} `mapstructure:"elastic"`
	Shards map[int][]string `mapstructure:"shards"`
}

// resolveShardConfigPath implements the search order documented in
// sharding.yaml: EXC_SHARDING_CONFIG wins (and must exist), otherwise the
// first existing candidate relative to the working directory. The walk-up
// entries cover CWDs like services/, services/cmd/exchange and
// services/internal/config (tests).
func resolveShardConfigPath() (string, error) {
	if p := strings.TrimSpace(os.Getenv("EXC_SHARDING_CONFIG")); p != "" {
		if _, err := os.Stat(p); err != nil {
			return "", fmt.Errorf("shard map: EXC_SHARDING_CONFIG %q: %w", p, err)
		}
		return p, nil
	}
	candidates := []string{
		"./config/sharding.yaml",
		"./sharding.yaml",
		"../config/sharding.yaml",
		"../../config/sharding.yaml",
		"../../../config/sharding.yaml",
	}
	for _, c := range candidates {
		if _, err := os.Stat(c); err == nil {
			abs, err := filepath.Abs(c)
			if err != nil {
				return c, nil
			}
			return abs, nil
		}
	}
	return "", fmt.Errorf("shard map: no sharding.yaml found (searched %s; set EXC_SHARDING_CONFIG)",
		strings.Join(candidates, ", "))
}

// LoadShardMap parses config/sharding.yaml. An empty path triggers the
// standard search order; an explicit missing/unparseable file is an
// error — fail-closed per spec §2.7 (a wrong shard map routes orders to
// the wrong engine, which is worse than refusing to start).
func LoadShardMap(path string) (*ShardMap, error) {
	if path == "" {
		var err error
		path, err = resolveShardConfigPath()
		if err != nil {
			return nil, err
		}
	}
	table, err := parseShardFile(path)
	if err != nil {
		return nil, err
	}
	return &ShardMap{
		path:         path,
		bySymbol:     table.bySymbol,
		elasticBase:  table.elasticBase,
		elasticCount: table.elasticCount,
	}, nil
}

// shardTable is the validated result of one parse/read.
type shardTable struct {
	bySymbol     map[string]int
	elasticBase  int
	elasticCount int
}

func parseShardFile(path string) (*shardTable, error) {
	v := viper.New()
	v.SetConfigFile(path)
	if err := v.ReadInConfig(); err != nil {
		return nil, fmt.Errorf("shard map: %s: %w", path, err)
	}
	var f shardFile
	if err := v.Unmarshal(&f); err != nil {
		return nil, fmt.Errorf("shard map: %s decode: %w", path, err)
	}
	base, count := f.Elastic.BaseShard, f.Elastic.NumShards
	if !v.IsSet("elastic") {
		// Section absent → defaults. A present-but-zero num_shards is a
		// config error and reaches validateShardTable (fail-closed).
		base, count = DefaultElasticBase, DefaultElasticCount
	}
	return validateShardTable(f.Shards, base, count)
}

// validateShardTable checks a symbol→shard assignment assembled from any
// source (file or Redis hash) and returns it in canonical form.
// Rejections: no entries, negative shard ids, duplicate/invalid symbols,
// non-positive elastic range.
func validateShardTable(shards map[int][]string, elasticBase, elasticCount int) (*shardTable, error) {
	if len(shards) == 0 {
		return nil, errors.New("shard map: no shard entries")
	}
	if elasticBase < 0 || elasticCount < 1 {
		return nil, fmt.Errorf("shard map: invalid elastic range base=%d num_shards=%d",
			elasticBase, elasticCount)
	}
	bySymbol := make(map[string]int, 16)
	for shard, symbols := range shards {
		if shard < 0 || shard > 1023 {
			return nil, fmt.Errorf("shard map: shard id %d out of range 0-1023", shard)
		}
		for _, raw := range symbols {
			sym := CanonicalSymbol(raw)
			if sym == "" {
				return nil, fmt.Errorf("shard map: shard %d has empty/invalid symbol %q", shard, raw)
			}
			if prev, dup := bySymbol[sym]; dup {
				return nil, fmt.Errorf("shard map: symbol %s assigned to shards %d and %d",
					sym, prev, shard)
			}
			bySymbol[sym] = shard
		}
	}
	return &shardTable{
		bySymbol:     bySymbol,
		elasticBase:  elasticBase,
		elasticCount: elasticCount,
	}, nil
}

// CanonicalSymbol normalizes a symbol for lookup and hashing. Rule (must
// stay identical to core/src/utils/ShardMap.cpp): keep [A-Za-z0-9],
// uppercase; a 6-character result is rendered "BASE/QUOTE", anything else
// stays compact. Thus "EUR/USD", "EURUSD", "eur-usd" all → "EUR/USD",
// while derivative symbols like "EURUSD-1W" → "EURUSD1W".
func CanonicalSymbol(symbol string) string {
	var b strings.Builder
	b.Grow(len(symbol) + 1)
	for _, r := range symbol {
		switch {
		case r >= 'a' && r <= 'z':
			b.WriteRune(r - 'a' + 'A')
		case r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			b.WriteRune(r)
		}
	}
	s := b.String()
	if len(s) == 6 {
		return s[:3] + "/" + s[3:]
	}
	return s
}

// fnv1a32 is the cross-language elastic hash (FNV-1a 32-bit over the
// canonical symbol bytes). Mirrored in C++ — do not change the algorithm.
func fnv1a32(s string) uint32 {
	const offset, prime = 2166136261, 16777619
	var h uint32 = offset
	for i := 0; i < len(s); i++ {
		h = (h ^ uint32(s[i])) * prime
	}
	return h
}

// GetShard returns the engine shard id for a symbol. Static entries win;
// anything unmapped (exotics, derivatives) is hashed into the elastic
// range [elasticBase, elasticBase+elasticCount).
func (m *ShardMap) GetShard(symbol string) int {
	sym := CanonicalSymbol(symbol)
	m.mu.RLock()
	shard, ok := m.bySymbol[sym]
	base, count := m.elasticBase, m.elasticCount
	m.mu.RUnlock()
	if ok {
		return shard
	}
	return base + int(fnv1a32(sym)%uint32(count))
}

// Entries returns a copy of the static symbol→shard table (excludes the
// elastic range — elastic assignments are derived, not listed).
func (m *ShardMap) Entries() map[string]int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make(map[string]int, len(m.bySymbol))
	for k, v := range m.bySymbol {
		out[k] = v
	}
	return out
}

// ElasticBase is the first shard id usable for hashed/exotic symbols.
func (m *ShardMap) ElasticBase() int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.elasticBase
}

// ElasticCount is the number of elastic shards (ids base..base+count-1).
func (m *ShardMap) ElasticCount() int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.elasticCount
}

// Path is the file the map was loaded from ("" when loaded from Redis).
func (m *ShardMap) Path() string { return m.path }

// Reload re-parses the source file and atomically swaps the table (SDD
// edge case: config reload). A parse/validation failure keeps the current
// table — operators get the error, services keep a known-good map.
func (m *ShardMap) Reload() error {
	if m.path == "" {
		return errors.New("shard map: reload unavailable — loaded from Redis, not a file")
	}
	table, err := parseShardFile(m.path)
	if err != nil {
		return err
	}
	m.mu.Lock()
	m.bySymbol = table.bySymbol
	m.elasticBase = table.elasticBase
	m.elasticCount = table.elasticCount
	m.mu.Unlock()
	return nil
}

// ---------------------------------------------------------------------------
// Redis distribution cache — shard:map HASH
// ---------------------------------------------------------------------------

// ShardMapStore is the narrow Redis surface shard-map I/O needs. Both
// *redis.Client and *redis.FailoverClient satisfy it via their embedded
// *goredis.Client (UniversalClient doesn't fit: internal/redis.Client
// redefines Ping with a different signature for probe ergonomics).
type ShardMapStore interface {
	HGetAll(ctx context.Context, key string) *goredis.MapStringStringCmd
	TxPipeline() goredis.Pipeliner
}

// WriteToRedis replaces the shard:map HASH atomically (DEL+HSET inside
// MULTI, same pattern as SetCircuitBreaker): fields are
// canonical-symbol → decimal shard id plus the meta:* elastic params.
// Works on *redis.Client, *redis.FailoverClient or *goredis.Client.
func (m *ShardMap) WriteToRedis(ctx context.Context, c ShardMapStore) error {
	if c == nil {
		return errors.New("shard map: WriteToRedis requires a non-nil client")
	}
	m.mu.RLock()
	fields := make(map[string]any, len(m.bySymbol)+3)
	for sym, shard := range m.bySymbol {
		fields[sym] = strconv.Itoa(shard)
	}
	fields[shardMapMetaVersion] = "1"
	fields[shardMapMetaBase] = strconv.Itoa(m.elasticBase)
	fields[shardMapMetaCount] = strconv.Itoa(m.elasticCount)
	m.mu.RUnlock()

	pipe := c.TxPipeline()
	pipe.Del(ctx, ShardMapRedisKey)
	pipe.HSet(ctx, ShardMapRedisKey, fields)
	if _, err := pipe.Exec(ctx); err != nil {
		return fmt.Errorf("shard map: write %s: %w", ShardMapRedisKey, err)
	}
	return nil
}

// LoadShardMapFromRedis reads the shard:map HASH into a ShardMap. An
// absent/empty hash returns ErrShardMapCacheMiss; a malformed hash is a
// hard error (a corrupt distribution cache must fail closed, not silently
// produce a partial map). The result has no file path — call Reload only
// on file-sourced maps.
func LoadShardMapFromRedis(ctx context.Context, c ShardMapStore) (*ShardMap, error) {
	if c == nil {
		return nil, errors.New("shard map: LoadShardMapFromRedis requires a non-nil client")
	}
	raw, err := c.HGetAll(ctx, ShardMapRedisKey).Result()
	if err != nil {
		return nil, fmt.Errorf("shard map: read %s: %w", ShardMapRedisKey, err)
	}
	if len(raw) == 0 {
		return nil, ErrShardMapCacheMiss
	}

	shards := map[int][]string{}
	base, count := DefaultElasticBase, DefaultElasticCount
	for field, value := range raw {
		switch field {
		case shardMapMetaVersion:
			// informational; only version 1 exists
		case shardMapMetaBase:
			v, err := strconv.Atoi(value)
			if err != nil || v < 0 {
				return nil, fmt.Errorf("shard map: %s field %s = %q invalid", ShardMapRedisKey, field, value)
			}
			base = v
		case shardMapMetaCount:
			v, err := strconv.Atoi(value)
			if err != nil || v < 1 {
				return nil, fmt.Errorf("shard map: %s field %s = %q invalid", ShardMapRedisKey, field, value)
			}
			count = v
		default:
			if strings.HasPrefix(field, "meta:") {
				continue // forward-compatible: ignore unknown meta fields
			}
			shard, err := strconv.Atoi(value)
			if err != nil || shard < 0 {
				return nil, fmt.Errorf("shard map: %s field %s = %q invalid", ShardMapRedisKey, field, value)
			}
			shards[shard] = append(shards[shard], field)
		}
	}
	table, err := validateShardTable(shards, base, count)
	if err != nil {
		return nil, fmt.Errorf("shard map: %s: %w", ShardMapRedisKey, err)
	}
	return &ShardMap{
		bySymbol:     table.bySymbol,
		elasticBase:  table.elasticBase,
		elasticCount: table.elasticCount,
	}, nil
}

// LoadShardMapForService is the startup path every service uses (spec
// task: "all services read shard map from Redis at startup"): try the
// shard:map HASH first; on cache miss or Redis error fall back to
// config/sharding.yaml. The returned ShardMapSource tells callers which
// side won so they can log/alert on unexpected file fallback. The error
// is non-nil only when both sources fail — a file miss after a Redis
// outage is fatal to routing and must stop startup.
//
// Example (cmd/gateway):
//
//	sm, src, err := config.LoadShardMapForService(ctx, rdb)
//	if err != nil { return err }
//	log.Info("shard map loaded", "source", src.String(), "entries", len(sm.Entries()))
func LoadShardMapForService(ctx context.Context, c ShardMapStore) (*ShardMap, ShardMapSource, error) {
	if c != nil {
		m, err := LoadShardMapFromRedis(ctx, c)
		if err == nil {
			return m, ShardMapSourceRedis, nil
		}
		if !errors.Is(err, ErrShardMapCacheMiss) {
			// Real Redis failure — still fall back: the file is the source
			// of truth, and a service that CAN route correctly should not
			// refuse to boot. Callers observe ShardMapSourceFile.
			m, ferr := LoadShardMap("")
			if ferr != nil {
				return nil, 0, fmt.Errorf("shard map: redis read failed (%w) and file fallback failed: %v", err, ferr)
			}
			return m, ShardMapSourceFile, nil
		}
	}
	m, err := LoadShardMap("")
	if err != nil {
		return nil, 0, err
	}
	return m, ShardMapSourceFile, nil
}
