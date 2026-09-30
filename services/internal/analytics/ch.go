// Package analytics is the Phase-20 ClickHouse analytics & reporting
// layer (spec §16). It owns the native-protocol ingest path used by the
// JetStream ETL consumer, the disk spool that covers ClickHouse outages,
// and the read-side projections that back the REST reporting surface.
//
// Schema lives under deploy/clickhouse/schema/ (numbered DDL applied via
// clickhouse-client); this package reads/writes through the single
// Conn seam below so every consumer shares one connection shape and the
// in-memory fakes used by unit tests stay honest.
package analytics

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2"
	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
)

// Config holds the ClickHouse connection settings. Values map 1:1 onto
// clickhouse.Options — native protocol port (9000) because the §24 #65
// 50k-inserts/sec ingest contract needs the columnar binary format, not
// HTTP JSONEachRow.
type Config struct {
	Addr     string // host:port, native protocol (e.g. 127.0.0.1:9000)
	Database string
	User     string
	Password string
	// DialTimeout bounds the connect handshake; ingest calls use the
	// caller's context deadline (ETL caps batch flush at 5s per §16.6).
	DialTimeout time.Duration
}

// ConfigFromEnv mirrors the cmd/s3-market-data-exporter convention:
// EXC_CH_URL may carry either the HTTP (:8123) or native (:9000) form —
// only the native form is used here; when the env carries an http:// URL
// the :8123 port is rewritten to :9000 so one env block serves both
// clients.
func ConfigFromEnv() Config {
	addr := strings.TrimSpace(os.Getenv("EXC_CH_NATIVE_URL"))
	if addr == "" {
		raw := strings.TrimSpace(os.Getenv("EXC_CH_URL"))
		raw = strings.TrimPrefix(raw, "http://")
		raw = strings.TrimPrefix(raw, "https://")
		addr = raw
	}
	if addr == "" {
		addr = "127.0.0.1:9000"
	}
	addr = strings.Replace(addr, ":8123", ":9000", 1)
	db := os.Getenv("EXC_CH_DATABASE")
	if db == "" {
		db = "exchange_analytics"
	}
	return Config{
		Addr:        addr,
		Database:    db,
		User:        os.Getenv("EXC_CH_USER"),
		Password:    os.Getenv("EXC_CH_PASSWORD"),
		DialTimeout: 10 * time.Second,
	}
}

// Conn is the narrow driver surface the package depends on. driver.Conn
// satisfies it; tests substitute fakes.
type Conn interface {
	Exec(ctx context.Context, query string, args ...any) error
	Query(ctx context.Context, query string, args ...any) (driver.Rows, error)
	PrepareBatch(ctx context.Context, query string, opts ...driver.PrepareBatchOption) (driver.Batch, error)
	Ping(ctx context.Context) error
	Close() error
}

// Dial opens a native-protocol connection. The ping happens inside the
// dial so callers fail fast on a dead cluster — ingest callers still
// degrade to the spool when writes fail later.
func Dial(ctx context.Context, cfg Config) (Conn, error) {
	if cfg.DialTimeout <= 0 {
		cfg.DialTimeout = 10 * time.Second
	}
	conn, err := clickhouse.Open(&clickhouse.Options{
		Addr:        []string{cfg.Addr},
		DialTimeout: cfg.DialTimeout,
		Auth: clickhouse.Auth{
			Database: cfg.Database,
			Username: cfg.User,
			Password: cfg.Password,
		},
		Settings: clickhouse.Settings{
			// async_insert + wait_for_async_insert keeps acks cheap on the
			// tick path while still surfacing hard failures.
			"async_insert":                 1,
			"wait_for_async_insert":        1,
			"async_insert_busy_timeout_ms": 200,
		},
	})
	if err != nil {
		return nil, fmt.Errorf("clickhouse dial %s: %w", cfg.Addr, err)
	}
	if err := conn.Ping(ctx); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("clickhouse ping %s: %w", cfg.Addr, err)
	}
	return conn, nil
}
