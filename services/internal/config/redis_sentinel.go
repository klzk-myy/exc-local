// Sentinel failover wiring for the coordination Redis (Task 1.3.9,
// spec §4.5). LoadRedisSentinelConfig reads config/redis-sentinel.yaml
// (master name, sentinel addrs, pool size, timeouts, resolve mode);
// LoadRedisFailover turns it into a live redis.FailoverClient.
//
// Environment overrides (applied after the YAML file, highest precedence):
//
//	EXC_SENTINEL_ADDRS        comma-separated host:port list
//	EXC_SENTINEL_MASTER       monitor name (default "mymaster")
//	EXC_SENTINEL_RESOLVE_MODE as_announced | announce_map | host_probe
//	EXC_SENTINEL_HOST_ADDRS   comma-separated host:port candidates for host_probe
//	EXC_SENTINEL_PASSWORD     data-node password
//
// Resolve modes (redis.SentinelResolveMode): production and in-network
// containers use as_announced — the address Sentinel announces is
// routable. Dev hosts use host_probe (or announce_map) because the
// compose network's announced addrs (172.18.x.x:6379 / redis-*:6379) do
// not resolve from the host; the probe maps each announced node to the
// matching host port in host_addrs by run_id.
package config

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/spf13/viper"

	"exchange/internal/redis"
)

// RedisSentinelConfig mirrors config/redis-sentinel.yaml. Timeout fields
// are duration strings ("1s", "500ms"); empty means "use the client
// defaults" (5s dial, 3s read/write, 4s pool — same as redis.New).
type RedisSentinelConfig struct {
	MasterName    string            `mapstructure:"master_name"`
	SentinelAddrs []string          `mapstructure:"sentinel_addrs"`
	Password      string            `mapstructure:"password"`
	DB            int               `mapstructure:"db"`
	PoolSize      int               `mapstructure:"pool_size"`
	MinIdleConns  int               `mapstructure:"min_idle_conns"`
	MaxRetries    int               `mapstructure:"max_retries"`
	DialTimeout   string            `mapstructure:"dial_timeout"`
	ReadTimeout   string            `mapstructure:"read_timeout"`
	WriteTimeout  string            `mapstructure:"write_timeout"`
	PoolTimeout   string            `mapstructure:"pool_timeout"`
	ResolveMode   string            `mapstructure:"resolve_mode"`
	AnnounceMap   map[string]string `mapstructure:"announce_map"`
	HostAddrs     []string          `mapstructure:"host_addrs"`
}

// LoadRedisSentinelConfig reads the failover config file. path may be
// empty, in which case ./config/redis-sentinel.yaml and
// ../config/redis-sentinel.yaml are searched (repo-root file vs
// services/-relative working dirs). An explicit missing or unparseable
// file is an error — fail-closed per spec §2.7.
func LoadRedisSentinelConfig(path string) (*RedisSentinelConfig, error) {
	// "::" key delimiter: announce_map keys are host:port addresses like
	// "10.99.0.11:6379" — viper's default "." delimiter would split them
	// into bogus nested maps. No legit key contains "::".
	v := viper.NewWithOptions(viper.KeyDelimiter("::"))
	v.SetConfigType("yaml")
	if path != "" {
		v.SetConfigFile(path)
	} else {
		v.SetConfigName("redis-sentinel")
		v.AddConfigPath("./config")
		v.AddConfigPath("../config")
	}
	if err := v.ReadInConfig(); err != nil {
		return nil, fmt.Errorf("config: redis-sentinel: %w", err)
	}

	var cfg RedisSentinelConfig
	if err := v.Unmarshal(&cfg); err != nil {
		return nil, fmt.Errorf("config: redis-sentinel decode: %w", err)
	}
	applySentinelEnv(&cfg)
	if err := cfg.applyDefaultsAndValidate(); err != nil {
		return nil, err
	}
	return &cfg, nil
}

// applySentinelEnv lets operators point at a different topology without
// editing the file (container-vs-host dev, CI drills).
func applySentinelEnv(cfg *RedisSentinelConfig) {
	if v := strings.TrimSpace(os.Getenv("EXC_SENTINEL_ADDRS")); v != "" {
		cfg.SentinelAddrs = splitCSV(v)
	}
	if v := strings.TrimSpace(os.Getenv("EXC_SENTINEL_MASTER")); v != "" {
		cfg.MasterName = v
	}
	if v := strings.TrimSpace(os.Getenv("EXC_SENTINEL_RESOLVE_MODE")); v != "" {
		cfg.ResolveMode = v
	}
	if v := strings.TrimSpace(os.Getenv("EXC_SENTINEL_HOST_ADDRS")); v != "" {
		cfg.HostAddrs = splitCSV(v)
	}
	if v := os.Getenv("EXC_SENTINEL_PASSWORD"); v != "" {
		cfg.Password = v
	}
}

func splitCSV(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func (c *RedisSentinelConfig) applyDefaultsAndValidate() error {
	if strings.TrimSpace(c.MasterName) == "" {
		c.MasterName = "mymaster"
	}
	if len(c.SentinelAddrs) == 0 {
		return errors.New("config: redis-sentinel sentinel_addrs must list at least one sentinel")
	}
	if c.ResolveMode == "" {
		c.ResolveMode = string(redis.ResolveAsAnnounced)
	}
	return nil
}

// failoverConfig converts the YAML/env shape into the typed
// redis.FailoverConfig, parsing duration strings. Any bad value is a
// startup error (spec §2.7 fail-closed).
func (c *RedisSentinelConfig) failoverConfig() (redis.FailoverConfig, error) {
	fc := redis.FailoverConfig{
		MasterName:    c.MasterName,
		SentinelAddrs: c.SentinelAddrs,
		Password:      c.Password,
		DB:            c.DB,
		PoolSize:      c.PoolSize,
		MinIdleConns:  c.MinIdleConns,
		MaxRetries:    c.MaxRetries,
		ResolveMode:   redis.SentinelResolveMode(c.ResolveMode),
		AnnounceMap:   c.AnnounceMap,
		HostAddrs:     c.HostAddrs,
	}
	for _, d := range []struct {
		name string
		raw  string
		out  *time.Duration
	}{
		{"dial_timeout", c.DialTimeout, &fc.DialTimeout},
		{"read_timeout", c.ReadTimeout, &fc.ReadTimeout},
		{"write_timeout", c.WriteTimeout, &fc.WriteTimeout},
		{"pool_timeout", c.PoolTimeout, &fc.PoolTimeout},
	} {
		if d.raw == "" {
			continue
		}
		v, err := time.ParseDuration(d.raw)
		if err != nil || v <= 0 {
			return fc, fmt.Errorf("config: redis-sentinel %s %q is not a positive duration", d.name, d.raw)
		}
		*d.out = v
	}
	return fc, nil
}

// LoadRedisFailover builds the sentinel-aware coordination client from an
// already-loaded RedisSentinelConfig. Construction is lazy: sentinel
// discovery happens on the first command; callers that need readiness at
// boot should Ping.
func LoadRedisFailover(cfg *RedisSentinelConfig) (*redis.FailoverClient, error) {
	if cfg == nil {
		return nil, errors.New("config: LoadRedisFailover requires a non-nil RedisSentinelConfig")
	}
	fc, err := cfg.failoverConfig()
	if err != nil {
		return nil, err
	}
	client, err := redis.NewFailoverClient(fc)
	if err != nil {
		return nil, fmt.Errorf("config: redis-sentinel: %w", err)
	}
	return client, nil
}

// LoadRedisFailoverFromFile is the one-call convenience form:
// file -> config -> client.
func LoadRedisFailoverFromFile(path string) (*redis.FailoverClient, error) {
	cfg, err := LoadRedisSentinelConfig(path)
	if err != nil {
		return nil, err
	}
	return LoadRedisFailover(cfg)
}
