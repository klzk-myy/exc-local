package bridge

import (
	"errors"
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/viper"
)

// Config holds the Bridge Service settings (config.yaml `bridge:` section
// or EXC_BRIDGE_* environment overrides). The shared loader
// (internal/config) only models the common sections; this loader applies
// the same precedence — defaults < config.yaml < env — for the bridge
// subtree.
type Config struct {
	// ShardID is the matching-engine shard this instance fans out. One
	// Bridge per shard, NUMA-colocated with the engine.
	ShardID uint32 `mapstructure:"shard_id"`

	// Aeron subscription parameters. The engine publishes outbound events
	// on `aeron:ipc?alias=orders_out` (stream 1002, see
	// core/include/ipc/AeronDriverConfig.hpp). URI may contain a single
	// "%d" verb which is formatted with ShardID (multi-shard alias scheme).
	AeronDir           string        `mapstructure:"aeron_dir"`
	AeronURI           string        `mapstructure:"aeron_uri"`
	AeronStreamID      int32         `mapstructure:"aeron_stream_id"`
	AeronDriverTimeout time.Duration `mapstructure:"aeron_driver_timeout"`
	AeronPollIdleWait  time.Duration `mapstructure:"aeron_poll_idle_wait"`

	// BufferSize bounds the in-memory NATS-outage ring (spec §2.3.1: 100k).
	BufferSize int `mapstructure:"buffer_size"`

	// PublishTimeout bounds each JetStream publish attempt.
	PublishTimeout time.Duration `mapstructure:"publish_timeout"`
	// ReconnectWait is the backoff between head-of-line retries while NATS
	// is unavailable.
	ReconnectWait time.Duration `mapstructure:"reconnect_wait"`

	// HeartbeatInterval is the cadence for bridge.health.<shard> (spec:
	// every 5s; absence triggers P1 alert).
	HeartbeatInterval time.Duration `mapstructure:"heartbeat_interval"`

	// MetricsAddr is the listen address for the /metrics + /healthz HTTP
	// endpoint. Empty disables the listener. Run one bridge per shard —
	// give each a distinct port (e.g. 9100+shard).
	MetricsAddr string `mapstructure:"metrics_addr"`

	// NATSURLs optionally overrides the global nats.urls seed list (empty
	// = use the shared config value).
	NATSURLs string `mapstructure:"nats_urls"`

	// Instruments maps wire instrument_id -> symbol token. Keys are the
	// decimal instrument ids ("3": "EUR-USD"). Unmapped ids fall back to
	// "instr-<id>" so ordering domains remain per-instrument.
	Instruments map[string]string `mapstructure:"instruments"`

	// OrderIndexSize bounds the order_id -> symbol index used to route
	// fills/cancels/amends that carry no instrument_id.
	OrderIndexSize int `mapstructure:"order_index_size"`
}

// Resolver returns the configured instrument map as a Resolver.
func (c *Config) Resolver() (MapResolver, error) {
	m := make(MapResolver, len(c.Instruments))
	for k, v := range c.Instruments {
		id, err := strconv.ParseUint(strings.TrimSpace(k), 10, 32)
		if err != nil {
			return nil, fmt.Errorf("bridge: instruments key %q is not a uint32 instrument id", k)
		}
		sym := strings.TrimSpace(v)
		if sym == "" {
			return nil, fmt.Errorf("bridge: instruments[%q] must not be empty", k)
		}
		m[uint32(id)] = sym
	}
	return m, nil
}

// AeronChannel resolves the subscription URI, formatting a "%d" verb with
// the shard id if present.
func (c *Config) AeronChannel() string {
	if strings.Contains(c.AeronURI, "%d") {
		return fmt.Sprintf(c.AeronURI, c.ShardID)
	}
	return c.AeronURI
}

// HeartbeatSubject is "bridge.health.<shard_id>".
func (c *Config) HeartbeatSubject() string {
	return fmt.Sprintf("bridge.health.%d", c.ShardID)
}

// Validate rejects out-of-range settings (fail-closed per spec §2.7).
func (c *Config) Validate() error {
	if c.AeronURI == "" {
		return errors.New("config: bridge.aeron_uri must not be empty")
	}
	if c.AeronStreamID <= 0 {
		return fmt.Errorf("config: bridge.aeron_stream_id %d must be > 0", c.AeronStreamID)
	}
	if c.BufferSize < 1 {
		return fmt.Errorf("config: bridge.buffer_size %d must be >= 1", c.BufferSize)
	}
	if c.PublishTimeout <= 0 {
		return errors.New("config: bridge.publish_timeout must be > 0")
	}
	if c.ReconnectWait <= 0 {
		return errors.New("config: bridge.reconnect_wait must be > 0")
	}
	if c.HeartbeatInterval <= 0 {
		return errors.New("config: bridge.heartbeat_interval must be > 0")
	}
	if c.OrderIndexSize < 1 {
		return fmt.Errorf("config: bridge.order_index_size %d must be >= 1", c.OrderIndexSize)
	}
	if c.MetricsAddr != "" {
		if _, err := net.ResolveTCPAddr("tcp", c.MetricsAddr); err != nil {
			return fmt.Errorf("config: bridge.metrics_addr %q: %w", c.MetricsAddr, err)
		}
	}
	return nil
}

// Load reads the `bridge:` subtree with the same lookup semantics as
// internal/config.Load: defaults < config.yaml (. / ./config / $EXC_CONFIG)
// < EXC_BRIDGE_* env vars (nested keys use "_").
func Load() (*Config, error) {
	v := viper.New()

	v.SetDefault("bridge.shard_id", 0)
	v.SetDefault("bridge.aeron_dir", "")
	v.SetDefault("bridge.aeron_uri", "aeron:ipc?alias=orders_out")
	v.SetDefault("bridge.aeron_stream_id", 1002)
	v.SetDefault("bridge.aeron_driver_timeout", "5s")
	v.SetDefault("bridge.aeron_poll_idle_wait", "100us")
	v.SetDefault("bridge.buffer_size", 100_000)
	v.SetDefault("bridge.publish_timeout", "2s")
	v.SetDefault("bridge.reconnect_wait", "250ms")
	v.SetDefault("bridge.heartbeat_interval", "5s")
	v.SetDefault("bridge.metrics_addr", "127.0.0.1:9100")
	v.SetDefault("bridge.nats_urls", "")
	v.SetDefault("bridge.order_index_size", 1_000_000)

	v.SetConfigName("config")
	v.SetConfigType("yaml")
	v.AddConfigPath(".")
	v.AddConfigPath("./config")
	if p := os.Getenv("EXC_CONFIG"); strings.TrimSpace(p) != "" {
		v.SetConfigFile(p) // explicit path: a missing file IS an error
	}

	v.SetEnvPrefix("EXC")
	v.SetEnvKeyReplacer(strings.NewReplacer(".", "_"))
	v.AutomaticEnv()

	if err := v.ReadInConfig(); err != nil {
		var notFound viper.ConfigFileNotFoundError
		if !errors.As(err, &notFound) {
			return nil, fmt.Errorf("config: %w", err)
		}
	}

	var cfg Config
	if err := v.UnmarshalKey("bridge", &cfg); err != nil {
		return nil, fmt.Errorf("config: decode bridge: %w", err)
	}
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return &cfg, nil
}
