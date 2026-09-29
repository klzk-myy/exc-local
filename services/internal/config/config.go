// Package config loads service configuration from YAML and environment.
//
// Precedence, lowest to highest:
//  1. Built-in defaults registered in setDefaults.
//  2. config.yaml — searched in "." and "./config", or an explicit file
//     named by the EXC_CONFIG environment variable.
//  3. Environment variables prefixed EXC_ (e.g. EXC_GATEWAY_PORT maps to
//     gateway.port; nested keys use "_").
//
// A missing config file is not an error: defaults still apply.
// Invalid values — including unparseable EXC_ overrides — are a startup
// error. This is deliberate fail-closed behaviour per spec §2.7: a
// misconfigured trading service must die loudly, not silently fall back.
package config

import (
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"strconv"
	"strings"

	"github.com/spf13/viper"

	"exchange/pkg/logging"
)

// Config is the root configuration shared by all Go services.
type Config struct {
	Environment string         `mapstructure:"environment"`
	Logging     LoggingConfig  `mapstructure:"logging"`
	Gateway     ServiceConfig  `mapstructure:"gateway"`
	MarketData  ServiceConfig  `mapstructure:"marketdata"`
	Fix         ServiceConfig  `mapstructure:"fix"`
	Settlement  ServiceConfig  `mapstructure:"settlement"`
	Compliance  ServiceConfig  `mapstructure:"compliance"`
	Admin       ServiceConfig  `mapstructure:"admin"`
	Postgres    PostgresConfig `mapstructure:"postgres"`
	Redis       RedisConfig    `mapstructure:"redis"`
	NATS        NATSConfig     `mapstructure:"nats"`
	Secrets     SecretsConfig  `mapstructure:"secrets"`
}

// SecretsConfig holds locally-loadable key material. The production
// source of truth is Vault/KMS (Phase-13.5 Task 13.5.3.5); the data_key
// file/env fallback exists so the platform surfaces (API-key HMAC
// secrets, webhook signing secrets — both AES-256-GCM secret_enc
// columns) can seal/unseal during development and disaster recovery.
// Production boots WITHOUT a data key fail closed (Validate).
type SecretsConfig struct {
	// DataKey is the 32-byte AES-256-GCM data key for auth.SecretBox,
	// given as base64 (preferred) or hex. EXC_SECRETS_DATA_KEY.
	DataKey string `mapstructure:"data_key"`
}

// IsProduction reports whether the environment label names a production
// deployment — every non-production keyword shares test-environment
// semantics (testenv reset gate, dev secret fallbacks). An empty or
// unrecognized label fails closed to production behaviour.
func (c *Config) IsProduction() bool {
	switch strings.ToLower(strings.TrimSpace(c.Environment)) {
	case "development", "dev", "staging", "stage", "test", "testing",
		"sandbox", "local", "ci", "testnet":
		return false
	}
	return true
}

// ServiceConfig is the uniform per-service section. Only the gateway binds
// its listener in this scaffold; the other addresses are reserved for the
// health/admin endpoints each service gains in later phases.
type ServiceConfig struct {
	Host string `mapstructure:"host"`
	Port int    `mapstructure:"port"`
}

// Addr returns the "host:port" listen address.
func (s ServiceConfig) Addr() string {
	return net.JoinHostPort(s.Host, strconv.Itoa(s.Port))
}

// PostgresConfig holds the primary OLTP connection settings (pgx).
type PostgresConfig struct {
	DSN      string `mapstructure:"dsn"`
	MaxConns int32  `mapstructure:"max_conns"`
}

// RedisConfig holds the coordination Redis client settings.
// Full key schema and the separate cache instance are Task 1.3.4.
type RedisConfig struct {
	Addr     string `mapstructure:"addr"`
	Password string `mapstructure:"password"`
	DB       int    `mapstructure:"db"`
}

// NATSConfig holds the JetStream cluster seed URLs.
type NATSConfig struct {
	// URLs is a comma-separated list, e.g.
	// "nats://127.0.0.1:4222,nats://127.0.0.1:4223,nats://127.0.0.1:4224".
	URLs string `mapstructure:"urls"`
}

// URLList splits URLs into individual server URLs.
func (n NATSConfig) URLList() []string {
	var out []string
	for _, u := range strings.Split(n.URLs, ",") {
		if u = strings.TrimSpace(u); u != "" {
			out = append(out, u)
		}
	}
	return out
}

// LoggingConfig controls slog output.
type LoggingConfig struct {
	Level  string `mapstructure:"level"`  // debug|info|warn|error
	Format string `mapstructure:"format"` // json|text
}

// Load reads configuration from defaults, config.yaml and EXC_ env vars,
// then validates the result. It never returns a partially-valid config.
func Load() (*Config, error) {
	v := viper.New()
	setDefaults(v)

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
		// No config file found on the search path: boot on defaults.
	}

	var cfg Config
	if err := v.Unmarshal(&cfg); err != nil {
		// Reached when e.g. EXC_GATEWAY_PORT=abc cannot decode to int.
		return nil, fmt.Errorf("config: decode: %w", err)
	}
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return &cfg, nil
}

func setDefaults(v *viper.Viper) {
	v.SetDefault("environment", "development")

	v.SetDefault("logging.level", "info")
	v.SetDefault("logging.format", "json")

	v.SetDefault("gateway.host", "0.0.0.0")
	v.SetDefault("gateway.port", 8080)
	v.SetDefault("marketdata.host", "0.0.0.0")
	v.SetDefault("marketdata.port", 8081)
	v.SetDefault("fix.host", "0.0.0.0")
	v.SetDefault("fix.port", 8082)
	v.SetDefault("settlement.host", "127.0.0.1")
	v.SetDefault("settlement.port", 8083)
	v.SetDefault("compliance.host", "127.0.0.1")
	v.SetDefault("compliance.port", 8084)
	v.SetDefault("admin.host", "0.0.0.0")
	v.SetDefault("admin.port", 8085)

	v.SetDefault("postgres.dsn", "postgres://exchange:exchange_dev@127.0.0.1:5433/exchange?sslmode=disable")
	v.SetDefault("postgres.max_conns", 20)

	v.SetDefault("redis.addr", "127.0.0.1:16379")
	v.SetDefault("redis.db", 0)

	v.SetDefault("nats.urls", "nats://127.0.0.1:4222,nats://127.0.0.1:4223,nats://127.0.0.1:4224")
}

// Validate rejects out-of-range or malformed settings. Every service port
// must be 1-65535, log level/format must be known, and connection strings
// must parse. Called by Load; services never see an invalid Config.
func (c *Config) Validate() error {
	for name, s := range map[string]ServiceConfig{
		"gateway":    c.Gateway,
		"marketdata": c.MarketData,
		"fix":        c.Fix,
		"settlement": c.Settlement,
		"compliance": c.Compliance,
		"admin":      c.Admin,
	} {
		if s.Port < 1 || s.Port > 65535 {
			return fmt.Errorf("config: %s.port %d out of range 1-65535 "+
				"(check config.yaml or EXC_%s_PORT)", name, s.Port,
				strings.ToUpper(name))
		}
		if _, err := net.ResolveTCPAddr("tcp", s.Addr()); err != nil {
			return fmt.Errorf("config: %s address %q: %w", name, s.Addr(), err)
		}
	}

	if _, err := logging.ParseLevel(c.Logging.Level); err != nil {
		return fmt.Errorf("config: logging.level: %w", err)
	}
	switch strings.ToLower(c.Logging.Format) {
	case "json", "text":
	default:
		return fmt.Errorf("config: logging.format %q must be json|text", c.Logging.Format)
	}

	if strings.TrimSpace(c.Postgres.DSN) == "" {
		return errors.New("config: postgres.dsn must not be empty")
	}
	if u, err := url.Parse(c.Postgres.DSN); err != nil || u.Scheme == "" {
		return fmt.Errorf("config: postgres.dsn %q is not a valid URL", c.Postgres.DSN)
	}
	if c.Postgres.MaxConns < 0 {
		return fmt.Errorf("config: postgres.max_conns %d must be >= 0", c.Postgres.MaxConns)
	}

	if strings.TrimSpace(c.Redis.Addr) == "" {
		return errors.New("config: redis.addr must not be empty")
	}
	if _, err := net.ResolveTCPAddr("tcp", c.Redis.Addr); err != nil {
		return fmt.Errorf("config: redis.addr %q: %w", c.Redis.Addr, err)
	}
	if c.Redis.DB < 0 {
		return fmt.Errorf("config: redis.db %d must be >= 0", c.Redis.DB)
	}

	if len(c.NATS.URLList()) == 0 {
		return errors.New("config: nats.urls must list at least one server")
	}
	for _, u := range c.NATS.URLList() {
		parsed, err := url.Parse(u)
		if err != nil || parsed.Scheme != "nats" || parsed.Host == "" {
			return fmt.Errorf("config: nats.urls entry %q is not a valid nats:// URL", u)
		}
	}

	// Secret material: a set data_key must decode to exactly 32 bytes
	// (AES-256-GCM); in production it must exist — a gateway that cannot
	// unseal api_keys.secret_enc / webhook secrets cannot verify
	// signatures, and a silently-keyless boot is a §2.7 violation.
	if k := strings.TrimSpace(c.Secrets.DataKey); k != "" {
		if _, err := DecodeDataKey(k); err != nil {
			return fmt.Errorf("config: secrets.data_key: %w", err)
		}
	} else if c.IsProduction() {
		return errors.New("config: secrets.data_key is required in production " +
			"(EXC_SECRETS_DATA_KEY, base64 or hex, 32 bytes)")
	}
	return nil
}

// DecodeDataKey decodes a base64- or hex-encoded 32-byte data key.
func DecodeDataKey(s string) ([]byte, error) {
	if b, err := base64.StdEncoding.DecodeString(s); err == nil && len(b) == 32 {
		return b, nil
	}
	if b, err := base64.RawURLEncoding.DecodeString(s); err == nil && len(b) == 32 {
		return b, nil
	}
	if b, err := hex.DecodeString(s); err == nil && len(b) == 32 {
		return b, nil
	}
	return nil, fmt.Errorf("must be base64 or hex encoding of exactly 32 bytes")
}
