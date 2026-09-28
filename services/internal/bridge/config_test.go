package bridge

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestConfigDefaults(t *testing.T) {
	// No config file on the search path -> defaults apply.
	t.Setenv("EXC_CONFIG", filepath.Join(t.TempDir(), "absent.yaml"))
	// EXC_CONFIG pointing at a missing file IS an error per repo
	// convention; drop the env and run in an empty dir instead.
	os.Unsetenv("EXC_CONFIG")
	wd, _ := os.Getwd()
	defer func() { _ = os.Chdir(wd) }()
	_ = os.Chdir(t.TempDir())

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.BufferSize != 100_000 {
		t.Fatalf("buffer_size=%d want 100000 (spec §2.3.1)", cfg.BufferSize)
	}
	if cfg.HeartbeatInterval != 5*time.Second {
		t.Fatalf("heartbeat=%v want 5s", cfg.HeartbeatInterval)
	}
	if cfg.AeronChannel() != "aeron:ipc?alias=orders_out" || cfg.AeronStreamID != 1002 {
		t.Fatalf("aeron channel=%q stream=%d", cfg.AeronChannel(), cfg.AeronStreamID)
	}
	if cfg.HeartbeatSubject() != "bridge.health.0" {
		t.Fatalf("heartbeat subject %q", cfg.HeartbeatSubject())
	}
}

func TestConfigShardFormatting(t *testing.T) {
	c := Config{ShardID: 7, AeronURI: "aeron:ipc?alias=orders_out_%d"}
	if got := c.AeronChannel(); got != "aeron:ipc?alias=orders_out_7" {
		t.Fatalf("channel %q", got)
	}
	if got := c.HeartbeatSubject(); got != "bridge.health.7" {
		t.Fatalf("heartbeat %q", got)
	}
}

func TestConfigValidation(t *testing.T) {
	base := func() Config {
		return Config{
			AeronURI:          "aeron:ipc?alias=orders_out",
			AeronStreamID:     1002,
			BufferSize:        100,
			PublishTimeout:    time.Second,
			ReconnectWait:     time.Millisecond,
			HeartbeatInterval: 5 * time.Second,
			OrderIndexSize:    100,
		}
	}
	c := base()
	if err := c.Validate(); err != nil {
		t.Fatalf("valid config rejected: %v", err)
	}
	c = base()
	c.BufferSize = 0
	if err := c.Validate(); err == nil {
		t.Fatal("buffer_size=0 must be rejected")
	}
	c = base()
	c.HeartbeatInterval = 0
	if err := c.Validate(); err == nil {
		t.Fatal("heartbeat_interval=0 must be rejected")
	}
}

func TestResolverParsing(t *testing.T) {
	c := Config{Instruments: map[string]string{"3": "EUR-USD", "5": "GBP-JPY"}}
	res, err := c.Resolver()
	if err != nil {
		t.Fatalf("Resolver: %v", err)
	}
	if s, ok := res.Symbol(3); !ok || s != "EUR-USD" {
		t.Fatalf("symbol(3)=%q ok=%v", s, ok)
	}
	if _, ok := res.Symbol(9); ok {
		t.Fatal("unmapped id must return ok=false")
	}

	c = Config{Instruments: map[string]string{"x": "EUR-USD"}}
	if _, err := c.Resolver(); err == nil {
		t.Fatal("non-numeric instrument key must be rejected")
	}
	c = Config{Instruments: map[string]string{"3": " "}}
	if _, err := c.Resolver(); err == nil {
		t.Fatal("empty symbol must be rejected")
	}
}
