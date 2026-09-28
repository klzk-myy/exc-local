// Schema registry & versioned lifecycle (Task 6.3.18 item 3, spec §8.6 SBE
// row, §24 #284): every schema {ID, version} has a machine-readable
// lifecycle state. Deprecated schemas remain compatible for AT LEAST six
// months and emit warnings; retired schemas fail explicitly with
// SBE_SCHEMA_RETIRED (§23, 400); unknown IDs/versions fail with
// UNSUPPORTED_PROTOCOL_VERSION (§23, 400 — same code family the WS
// auth-frame version check uses per §10.5).
package sbe

import (
	"fmt"
	"sort"
	"time"

	excerrors "exchange/pkg/errors"
)

// §23 error codes emitted by negotiation (registered: Phase-05 Task 5.3.28
// and Phase-06 Task 6.3.18).
const (
	CodeUnsupportedProtocolVersion = "UNSUPPORTED_PROTOCOL_VERSION" // 400
	CodeSBESchemaRetired           = "SBE_SCHEMA_RETIRED"           // 400
)

// Lifecycle is the machine-readable schema state of Task 6.3.18 item 3.
type Lifecycle string

const (
	LifecycleActive     Lifecycle = "active"
	LifecycleDeprecated Lifecycle = "deprecated"
	LifecycleRetired    Lifecycle = "retired"
)

// MinDeprecationWindow is the §24 #284 six-month floor expressed as a
// conservative duration (183 days ≥ any calendar six-month span). The
// registry enforces the calendar rule — RetiresAt ≥ DeprecatedAt + 6
// calendar months — via AddDate(0, 6, 0); the constant is exported for
// callers/tests that need a duration form.
const MinDeprecationWindow = 183 * 24 * time.Hour

// SchemaVersion is one registered schema line.
type SchemaVersion struct {
	SchemaID uint16
	Version  uint16
	State    Lifecycle
	// DeprecatedAt is when the schema entered deprecation (zero if active).
	DeprecatedAt time.Time
	// RetiresAt is the sunset instant — at/after it the schema is retired
	// even if State is still "deprecated" (time-driven retirement).
	RetiresAt time.Time
	// Note is free-form provenance (e.g. "superseded by v2 field append").
	Note string
}

type schemaKey struct {
	id      uint16
	version uint16
}

// Registry maps template schema IDs/versions with lifecycle + sunset dates.
// Immutable after construction in the hot path — rebuild and swap rather
// than mutating under load. Safe for concurrent reads.
type Registry struct {
	m map[schemaKey]SchemaVersion
}

// NewRegistry returns an empty registry.
func NewRegistry() *Registry {
	return &Registry{m: make(map[schemaKey]SchemaVersion)}
}

// Register adds (or replaces) a schema row. When State is Deprecated,
// RetiresAt MUST be ≥ DeprecatedAt+MinDeprecationWindow — the six-month
// rule is enforced here so no registered row can violate §24 #284.
func (r *Registry) Register(v SchemaVersion) error {
	switch v.State {
	case LifecycleActive:
		// ok
	case LifecycleDeprecated:
		if v.DeprecatedAt.IsZero() {
			return fmt.Errorf("sbe: schema %d v%d deprecated without DeprecatedAt", v.SchemaID, v.Version)
		}
		if v.RetiresAt.Before(v.DeprecatedAt.AddDate(0, 6, 0)) {
			return fmt.Errorf("sbe: schema %d v%d sunset %s violates six-month deprecation window (deprecated %s)",
				v.SchemaID, v.Version, v.RetiresAt, v.DeprecatedAt)
		}
	case LifecycleRetired:
		// ok — explicitly dead
	default:
		return fmt.Errorf("sbe: schema %d v%d unknown lifecycle %q", v.SchemaID, v.Version, v.State)
	}
	r.m[schemaKey{v.SchemaID, v.Version}] = v
	return nil
}

// Lookup returns the row for an exact {schemaID, version}.
func (r *Registry) Lookup(schemaID, version uint16) (SchemaVersion, bool) {
	v, ok := r.m[schemaKey{schemaID, version}]
	return v, ok
}

// Latest returns the highest registered version of schemaID.
func (r *Registry) Latest(schemaID uint16) (SchemaVersion, bool) {
	var best SchemaVersion
	found := false
	for k, v := range r.m {
		if k.id == schemaID && (!found || k.version > best.Version) {
			best, found = v, true
		}
	}
	return best, found
}

// Negotiation is the outcome of a successful schema handshake.
type Negotiation struct {
	SchemaID   uint16
	Version    uint16
	Deprecated bool
	// Sunset is the retirement instant for deprecated schemas (zero when
	// active) — surfaced as RFC 8594 `Sunset` on REST responses.
	Sunset time.Time
	// Warning is the human-readable deprecation advisory; empty when active.
	Warning string
}

// Negotiate resolves a requested {schemaID, version} at instant now:
//   - unknown schema or version → UNSUPPORTED_PROTOCOL_VERSION (400)
//   - retired, or deprecated past its sunset → SBE_SCHEMA_RETIRED (400)
//   - deprecated within its window → accept + Warning + Sunset
//   - active → accept
func (r *Registry) Negotiate(schemaID, version uint16, now time.Time) (*Negotiation, error) {
	v, ok := r.Lookup(schemaID, version)
	if !ok {
		return nil, excerrors.New(CodeUnsupportedProtocolVersion,
			fmt.Sprintf("unknown SBE schema_id=%d version=%d", schemaID, version))
	}
	eff := v.State
	if eff == LifecycleDeprecated && !v.RetiresAt.IsZero() && !now.Before(v.RetiresAt) {
		eff = LifecycleRetired // sunset elapsed
	}
	switch eff {
	case LifecycleRetired:
		return nil, excerrors.New(CodeSBESchemaRetired,
			fmt.Sprintf("SBE schema_id=%d version=%d is retired", schemaID, version))
	case LifecycleDeprecated:
		return &Negotiation{
			SchemaID:   schemaID,
			Version:    version,
			Deprecated: true,
			Sunset:     v.RetiresAt,
			Warning: fmt.Sprintf("SBE schema %d v%d is deprecated; sunset %s",
				schemaID, version, v.RetiresAt.UTC().Format(time.RFC3339)),
		}, nil
	default:
		return &Negotiation{SchemaID: schemaID, Version: version}, nil
	}
}

// SchemaInfo is one row of the machine-readable lifecycle document
// (Task 6.3.18 item 3) served to clients for proactive migration.
type SchemaInfo struct {
	SchemaID     uint16    `json:"schema_id"`
	Version      uint16    `json:"version"`
	State        Lifecycle `json:"state"`
	DeprecatedAt time.Time `json:"deprecated_at,omitempty"`
	RetiresAt    time.Time `json:"retires_at,omitempty"`
	Note         string    `json:"note,omitempty"`
}

// LifecycleDoc is the machine-readable registry export — the "schema
// lifecycle" surface of §24 #284. Render via encoding/json at the endpoint.
type LifecycleDoc struct {
	GeneratedAt time.Time    `json:"generated_at"`
	MinWindow   string       `json:"min_deprecation_window"`
	Schemas     []SchemaInfo `json:"schemas"`
}

// Document returns the machine-readable lifecycle view at instant now —
// deprecated-but-past-sunset rows report as retired.
func (r *Registry) Document(now time.Time) LifecycleDoc {
	doc := LifecycleDoc{
		GeneratedAt: now.UTC(),
		MinWindow:   "6 months (spec §24 #284)",
	}
	for _, v := range r.m {
		eff := v.State
		if eff == LifecycleDeprecated && !v.RetiresAt.IsZero() && !now.Before(v.RetiresAt) {
			eff = LifecycleRetired
		}
		doc.Schemas = append(doc.Schemas, SchemaInfo{
			SchemaID:     v.SchemaID,
			Version:      v.Version,
			State:        eff,
			DeprecatedAt: v.DeprecatedAt,
			RetiresAt:    v.RetiresAt,
			Note:         v.Note,
		})
	}
	sort.Slice(doc.Schemas, func(i, j int) bool {
		if doc.Schemas[i].SchemaID != doc.Schemas[j].SchemaID {
			return doc.Schemas[i].SchemaID < doc.Schemas[j].SchemaID
		}
		return doc.Schemas[i].Version < doc.Schemas[j].Version
	})
	return doc
}

// DefaultRegistry registers schema 1 (market data) version 1 as active —
// the bootstrap state before lifecycle administration lands.
func DefaultRegistry() *Registry {
	r := NewRegistry()
	_ = r.Register(SchemaVersion{
		SchemaID: SchemaIDMarketData,
		Version:  SchemaVersionCurrent,
		State:    LifecycleActive,
		Note:     "marketdata schema: heartbeat/book_update/trade/snapshot_marker/security_status/security_definition",
	})
	return r
}
