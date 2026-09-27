// Tests for Task 1.3.7 shard mapping. The canonical config is the real
// repo-root config/sharding.yaml (resolved via the standard search order
// from this package's working directory); temp files cover malformed
// inputs and reload. Redis paths are gated on EXC_REDIS_TEST=1 against
// the dev coordination primary (same convention as internal/redis tests).
package config

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"exchange/internal/redis"
)

func loadCanonical(t *testing.T) *ShardMap {
	t.Helper()
	m, err := LoadShardMap("")
	if err != nil {
		t.Fatalf("LoadShardMap: %v", err)
	}
	return m
}

func TestShardMapCanonicalTable(t *testing.T) {
	m := loadCanonical(t)
	// spec §2.2 table, verbatim.
	want := map[string]int{
		"EUR/USD": 0, "GBP/USD": 0, "USD/CHF": 0,
		"USD/JPY": 1, "AUD/USD": 1, "NZD/USD": 1,
		"USD/CAD": 2, "USD/MXN": 2, "USD/BRL": 2,
		"EUR/GBP": 3, "EUR/JPY": 3, "EUR/CHF": 3,
	}
	for sym, shard := range want {
		if got := m.GetShard(sym); got != shard {
			t.Errorf("GetShard(%q) = %d, want %d", sym, got, shard)
		}
	}
	if n := len(m.Entries()); n != len(want) {
		t.Errorf("static entries = %d, want %d", n, len(want))
	}
	if m.ElasticBase() != DefaultElasticBase || m.ElasticCount() < 1 {
		t.Errorf("elastic range = %d+%d, want base %d count>=1",
			m.ElasticBase(), m.ElasticCount(), DefaultElasticBase)
	}
}

func TestShardMapAcceptanceProbes(t *testing.T) {
	m := loadCanonical(t)
	if got := m.GetShard("EUR/USD"); got != 0 {
		t.Fatalf(`GetShard("EUR/USD") = %d, want 0`, got)
	}
	if got := m.GetShard("USD/JPY"); got != 1 {
		t.Fatalf(`GetShard("USD/JPY") = %d, want 1`, got)
	}
}

func TestCanonicalSymbol(t *testing.T) {
	for in, want := range map[string]string{
		"EUR/USD":    "EUR/USD",
		"EURUSD":     "EUR/USD",
		"eurusd":     "EUR/USD",
		"eur/usd":    "EUR/USD",
		"EUR-USD":    "EUR/USD",
		" EUR/USD ":  "EUR/USD",
		"USD/BRL-1W": "USDBRL1W", // non-6-char: stays compact, still deterministic
		"":           "",
		"///":        "",
	} {
		if got := CanonicalSymbol(in); got != want {
			t.Errorf("CanonicalSymbol(%q) = %q, want %q", in, got, want)
		}
	}
}

// SDD edge case: unknown symbol → deterministic elastic shard >= base,
// identical to what the C++ FNV-1a implementation produces.
func TestShardMapUnknownSymbolElastic(t *testing.T) {
	m := loadCanonical(t)
	base, count := m.ElasticBase(), m.ElasticCount()
	for _, sym := range []string{"USD/TRY", "USD/ZAR", "EURUSD-1W", "XAGUSD1W"} {
		got := m.GetShard(sym)
		if got < base || got >= base+count {
			t.Fatalf("GetShard(%q) = %d outside elastic range [%d,%d)",
				sym, got, base, base+count)
		}
		if again := m.GetShard(sym); again != got {
			t.Fatalf("GetShard(%q) not deterministic: %d then %d", sym, got, again)
		}
	}
	// Empty symbol still resolves (hashes the empty string).
	if got := m.GetShard(""); got < base || got >= base+count {
		t.Fatalf("GetShard(\"\") = %d outside elastic range", got)
	}
}

func writeTempShardFile(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "sharding.yaml")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatalf("write temp sharding.yaml: %v", err)
	}
	return p
}

func TestLoadShardMapValidation(t *testing.T) {
	for name, body := range map[string]string{
		"duplicate symbol": "shards:\n  0: [EUR/USD]\n  1: [EURUSD]\n", // canonicalizes identically
		"empty shards":     "version: 1\n",
		"bad shard id":     "shards:\n  x: [EUR/USD]\n",
		"negative shard":   "shards:\n  -1: [EUR/USD]\n",
		"zero elastic":     "elastic:\n  num_shards: 0\nshards:\n  0: [EUR/USD]\n",
	} {
		p := writeTempShardFile(t, body)
		if _, err := LoadShardMap(p); err == nil {
			t.Errorf("%s: expected error, got nil", name)
		}
	}
	if _, err := LoadShardMap("/nonexistent/sharding.yaml"); err == nil {
		t.Error("missing explicit file: expected error, got nil")
	}
}

// SDD edge case: config reload — a good file swaps atomically, a broken
// reload keeps the previous table.
func TestShardMapReload(t *testing.T) {
	p := writeTempShardFile(t,
		"elastic:\n  base_shard: 4\n  num_shards: 2\nshards:\n  0: [EUR/USD]\n")
	m, err := LoadShardMap(p)
	if err != nil {
		t.Fatalf("LoadShardMap: %v", err)
	}
	if got := m.GetShard("USD/JPY"); got < 4 || got > 5 {
		t.Fatalf("pre-reload elastic shard %d not in [4,6)", got)
	}
	if err := os.WriteFile(p, []byte(
		"elastic:\n  base_shard: 8\n  num_shards: 2\nshards:\n  7: [EUR/USD]\n"), 0o600); err != nil {
		t.Fatalf("rewrite: %v", err)
	}
	if err := m.Reload(); err != nil {
		t.Fatalf("Reload: %v", err)
	}
	if got := m.GetShard("EUR/USD"); got != 7 {
		t.Fatalf("post-reload GetShard(EUR/USD) = %d, want 7", got)
	}
	if m.ElasticBase() != 8 {
		t.Fatalf("post-reload elastic base = %d, want 8", m.ElasticBase())
	}
	// Broken file → error, old table retained.
	if err := os.WriteFile(p, []byte("shards:\n  0: [EUR/USD]\n  1: [EUR/USD]\n"), 0o600); err != nil {
		t.Fatalf("rewrite2: %v", err)
	}
	if err := m.Reload(); err == nil {
		t.Fatal("Reload accepted duplicate symbol")
	}
	if got := m.GetShard("EUR/USD"); got != 7 {
		t.Fatalf("broken reload clobbered table: GetShard = %d, want 7", got)
	}
}

// --- Redis paths (integration-gated) ---------------------------------------

func shardTestClient(t *testing.T) *redis.Client {
	t.Helper()
	if os.Getenv("EXC_REDIS_TEST") != "1" {
		t.Skip("set EXC_REDIS_TEST=1 to run Redis integration tests")
	}
	addr := os.Getenv("EXC_REDIS_TEST_ADDR")
	if addr == "" {
		addr = "127.0.0.1:16379"
	}
	c := redis.New(addr, "", 0)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := c.Ping(ctx); err != nil {
		c.Close()
		t.Skipf("redis coordination instance unreachable at %s: %v", addr, err)
	}
	t.Cleanup(func() {
		c.Del(context.Background(), ShardMapRedisKey)
		c.Close()
	})
	return c
}

func TestShardMapRedisRoundTrip(t *testing.T) {
	c := shardTestClient(t)
	ctx := context.Background()
	c.Del(ctx, ShardMapRedisKey) // may already exist from cache-shard-map runs

	// SDD edge case: cache miss → ErrShardMapCacheMiss.
	if _, err := LoadShardMapFromRedis(ctx, c); err != ErrShardMapCacheMiss {
		t.Fatalf("miss: got %v, want ErrShardMapCacheMiss", err)
	}
	// Cache miss falls back to file in LoadShardMapForService.
	m, src, err := LoadShardMapForService(ctx, c)
	if err != nil || src != ShardMapSourceFile {
		t.Fatalf("miss fallback: src=%v err=%v, want file", src, err)
	}
	if m.GetShard("EUR/USD") != 0 {
		t.Fatal("file-fallback map wrong")
	}

	// Write then read back: identical resolution, Redis source.
	if err := m.WriteToRedis(ctx, c); err != nil {
		t.Fatalf("WriteToRedis: %v", err)
	}
	rm, err := LoadShardMapFromRedis(ctx, c)
	if err != nil {
		t.Fatalf("LoadShardMapFromRedis: %v", err)
	}
	for sym, shard := range m.Entries() {
		if rm.GetShard(sym) != shard {
			t.Fatalf("redis map diverged on %s: %d != %d", sym, rm.GetShard(sym), shard)
		}
	}
	// Elastic params ride in the hash: unknown symbol resolves the same.
	if rm.GetShard("USD/TRY") != m.GetShard("USD/TRY") {
		t.Fatal("redis map elastic resolution diverged")
	}
	m2, src, err := LoadShardMapForService(ctx, c)
	if err != nil || src != ShardMapSourceRedis {
		t.Fatalf("hit: src=%v err=%v, want redis", src, err)
	}
	if m2.GetShard("USD/JPY") != 1 {
		t.Fatal("redis-sourced map wrong")
	}
}
