// Package retention implements Phase-09 Task 9.3.22 — the unified data
// retention policy enforcer (spec §19.12, §24 #212).
//
// The policy is configuration-driven: a YAML document
// (infrastructure/data-tiering/tiering_policy.yaml) lists every data
// class with its retention period, archival mechanism, regulatory basis
// and GDPR interaction. The enforcer validates live PostgreSQL partition
// state, purgeable row windows and cold-tier evidence against that matrix
// each night, writes one retention_audit_log row per check, and raises
// RETENTION_POLICY_VIOLATION findings on drift.
//
// The same YAML drives the hot→warm→cold mover (cmd/archiver lifecycle);
// class fields hot_days/warm_days/retain_days are the shared contract.
package retention

import (
	"fmt"
	"os"
	"regexp"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Store kinds recognised by the enforcer.
const (
	StorePartitionedPG = "postgres_partitioned" // pg_partman/declarative parents
	StoreTablePG       = "postgres_table"       // plain tables with a ts column
	StoreClickHouse    = "clickhouse"           // MergeTree TTL-governed tables
	StoreObjectStore   = "object_store"         // cold WORM archives
	StoreExternal      = "external"             // lifecycle-bound (e.g. KYC = account+5y)
)

// Check results written to retention_audit_log.result.
const (
	ResultOK        = "OK"
	ResultViolation = "VIOLATION"
	ResultAction    = "ACTION" // apply-mode mutation performed
	ResultHeld      = "HELD"
	ResultSkipped   = "SKIPPED"
	ResultError     = "ERROR"
)

// DataClass is one row of the retention matrix.
type DataClass struct {
	Name        string `yaml:"name" json:"name"`               // e.g. "trades"
	Description string `yaml:"description" json:"description"` // human summary
	Store       string `yaml:"store" json:"store"`             // Store* const
	Table       string `yaml:"table" json:"table"`             // parent or table name
	TSColumn    string `yaml:"ts_column" json:"ts_column"`     // for postgres_table checks
	HotDays     int    `yaml:"hot_days" json:"hot_days"`       // attached-queryable window
	WarmDays    int    `yaml:"warm_days" json:"warm_days"`     // warm-tier exit boundary
	RetainDays  int    `yaml:"retain_days" json:"retain_days"` // legal retention floor
	Purgeable   bool   `yaml:"purgeable" json:"purgeable"`     // apply-mode row delete allowed
	Mechanism   string `yaml:"mechanism" json:"mechanism"`     // archival mechanism prose
	Basis       string `yaml:"regulatory_basis" json:"basis"`  // MiFID RTS 6, GDPR, ...
	GDPR        string `yaml:"gdpr_interaction" json:"gdpr"`   // Art. 17(3)(b) notes
}

// Policy is the parsed tiering_policy.yaml document.
type Policy struct {
	Version int         `yaml:"version"`
	Classes []DataClass `yaml:"classes"`
}

var identRe = regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9_]*$`)

// Validate sanity-checks a policy (fail-closed: a malformed matrix is a
// config error, not a skipped class).
func (p *Policy) Validate() error {
	if len(p.Classes) == 0 {
		return fmt.Errorf("retention: policy has no classes")
	}
	seen := map[string]bool{}
	for _, c := range p.Classes {
		if strings.TrimSpace(c.Name) == "" {
			return fmt.Errorf("retention: class with empty name")
		}
		if seen[c.Name] {
			return fmt.Errorf("retention: duplicate class %q", c.Name)
		}
		seen[c.Name] = true
		switch c.Store {
		case StorePartitionedPG, StoreTablePG:
			if !identRe.MatchString(c.Table) {
				return fmt.Errorf("retention: class %q unsafe table %q", c.Name, c.Table)
			}
			if c.Store == StoreTablePG && c.Purgeable && !identRe.MatchString(c.TSColumn) {
				return fmt.Errorf("retention: class %q unsafe ts_column %q", c.Name, c.TSColumn)
			}
		case StoreClickHouse:
			if c.Table == "" {
				return fmt.Errorf("retention: class %q missing clickhouse table", c.Name)
			}
		case StoreObjectStore, StoreExternal:
			// no live-PG identifier to check
		default:
			return fmt.Errorf("retention: class %q unknown store %q", c.Name, c.Store)
		}
		if c.RetainDays <= 0 {
			return fmt.Errorf("retention: class %q needs retain_days > 0", c.Name)
		}
	}
	return nil
}

// LoadYAML reads and validates a policy file.
func LoadYAML(path string) (*Policy, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("retention: read %s: %w", path, err)
	}
	var p Policy
	if err := yaml.Unmarshal(raw, &p); err != nil {
		return nil, fmt.Errorf("retention: parse %s: %w", path, err)
	}
	if err := p.Validate(); err != nil {
		return nil, fmt.Errorf("retention: %s: %w", path, err)
	}
	return &p, nil
}

// Class returns the named class or nil.
func (p *Policy) Class(name string) *DataClass {
	for i := range p.Classes {
		if p.Classes[i].Name == name {
			return &p.Classes[i]
		}
	}
	return nil
}

// BuiltinPolicy mirrors spec §19.12's canonical schedule — used when no
// YAML is configured and as the test oracle for the YAML file itself.
func BuiltinPolicy() *Policy {
	return &Policy{
		Version: 1,
		Classes: []DataClass{
			{Name: "orders", Store: StorePartitionedPG, Table: "orders",
				HotDays: 90, WarmDays: 365, RetainDays: 1825,
				Mechanism: "pg detach→warm schema→parquet+zstd WORM S3",
				Basis:     "MiFID II RTS 6 — order records 5y",
				GDPR:      "Art. 17(3)(b) legal-obligation override"},
			{Name: "trades", Store: StorePartitionedPG, Table: "trades",
				HotDays: 90, WarmDays: 365, RetainDays: 1825,
				Mechanism: "pg detach→warm schema→parquet+zstd WORM S3",
				Basis:     "MiFID II RTS 6 — trade records 5y",
				GDPR:      "Art. 17(3)(b) legal-obligation override"},
			{Name: "order_audit", Store: StorePartitionedPG, Table: "order_audit",
				HotDays: 90, WarmDays: 365, RetainDays: 1825,
				Mechanism: "pg detach→warm schema→parquet+zstd WORM S3",
				Basis:     "MiFID II RTS 6 — order lifecycle audit 5y",
				GDPR:      "Art. 17(3)(b) legal-obligation override"},
			{Name: "ledger", Store: StorePartitionedPG, Table: "ledger_lines",
				HotDays: 90, WarmDays: 365, RetainDays: 2555,
				Mechanism: "pg detach→warm schema→parquet+zstd WORM S3",
				Basis:     "finance records 7y (§19.12)",
				GDPR:      "Art. 17(3)(b) legal-obligation override"},
			{Name: "audit_hash_chain", Store: StorePartitionedPG, Table: "audit_hash_chain",
				HotDays: 90, WarmDays: 365, RetainDays: 2555,
				Mechanism: "pg detach→warm schema→parquet+zstd WORM S3",
				Basis:     "audit logs 7y (§19.12)",
				GDPR:      "tamper-evident chain — never purgeable"},
			{Name: "tick_history", Store: StoreClickHouse, Table: "tick_history",
				TSColumn: "timestamp", RetainDays: 90,
				Mechanism: "MergeTree TTL 90d → aggregates",
				Basis:     "§19.12 raw ticks 90d"},
			{Name: "ohlcv_aggregates", Store: StoreClickHouse, Table: "ohlcv_1m",
				TSColumn: "window_start", RetainDays: 1825,
				Mechanism: "SummingMergeTree TTL 5y",
				Basis:     "§19.12 OHLCV aggregates 5y"},
			{Name: "comms_recordings", Store: StoreObjectStore, Table: "comms_recordings",
				RetainDays: 1825,
				Mechanism:  "WORM object store",
				Basis:      "MiFID II Art. 16(7) — taping 5y"},
			{Name: "kyc_documents", Store: StoreExternal, Table: "kyc_documents",
				RetainDays: 1825,
				Mechanism:  "account-close +5y lifecycle (Task 14.3.9)",
				Basis:      "AML/KYC — account lifetime + 5y",
				GDPR:       "Art. 17 erasure honoured on account close + retention floor"},
			{Name: "surveillance_signals", Store: StoreTablePG, Table: "surveillance_signals",
				TSColumn: "created_at", RetainDays: 1825,
				Mechanism: "in-table 5y; cold review via export",
				Basis:     "§19.12 surveillance 5y"},
			{Name: "client_order_id_dedup", Store: StoreTablePG, Table: "client_order_id_dedup",
				TSColumn: "created_at", RetainDays: 7, Purgeable: true,
				Mechanism: "enforcer row purge (24h semantic window + margin)",
				Basis:     "internal idempotency window (migration 154)"},
			{Name: "idempotency_keys", Store: StoreTablePG, Table: "idempotency_keys",
				TSColumn: "created_at", RetainDays: 7, Purgeable: true,
				Mechanism: "enforcer row purge",
				Basis:     "internal idempotency window (migration 154)"},
			{Name: "webhook_deliveries", Store: StoreTablePG, Table: "webhook_deliveries",
				TSColumn: "created_at", RetainDays: 90, Purgeable: true,
				Mechanism: "enforcer row purge",
				Basis:     "operational delivery log"},
		},
	}
}

// RetainUntil computes the legal-retention floor for a class row written
// at t.
func (c DataClass) RetainUntil(t time.Time) time.Time {
	return t.AddDate(0, 0, c.RetainDays)
}
