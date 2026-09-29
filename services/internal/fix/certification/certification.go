// Package certification implements the Phase-18 Task 18.3.11 versioned
// client-certification register and the production session gate (spec
// §9.7, §24 #167): every client build must pass the venue certification
// pack before its production order-entry sessions open, and material
// protocol changes revoke certification until re-tested.
//
// Records are keyed by the exact compatibility tuple —
// (participant_id, client_build, dictionary_version,
// venue_schema_version, environment) — persisted by migration 052
// (fix_certifications). The Gate fails closed: no current ACTIVE PASS
// row → the session's Logon is refused.
package certification

import (
	"context"
	"fmt"
	"time"
)

// Record is one fix_certifications row — a single certification run's
// outcome, keyed by the compatibility tuple.
type Record struct {
	ID                 int64
	ParticipantID      string // client SenderCompID / firm id
	ClientBuild        string // versioned client build label
	DictionaryVersion  string // 'FIX.4.4' | 'FIX.5.0SP2'
	VenueSchemaVersion string // venue message-set version
	Environment        string // production | staging | sandbox | development
	PackVersion        string // certification pack that ran (e.g. "1.0.0")
	Result             string // PASS | FAIL
	EvidenceHash       string // sha256 hex of the run transcript
	CertifiedAt        time.Time
	ExpiresAt          time.Time
	Status             Status
	RevokedReason      string
}

// Status is the fix_certifications lifecycle state.
type Status string

const (
	StatusActive     Status = "ACTIVE"
	StatusRevoked    Status = "REVOKED"
	StatusExpired    Status = "EXPIRED"
	StatusSuperseded Status = "SUPERSEDED"
)

// Result values.
const (
	ResultPass = "PASS"
	ResultFail = "FAIL"
)

// Store is the certification persistence seam — PgStore in production,
// MemoryStore in tests.
type Store interface {
	// Record persists a run's outcome; supersession of prior ACTIVE rows
	// for the same tuple is the caller's (Register's) business.
	Record(ctx context.Context, r *Record) (*Record, error)
	// Latest returns the newest ACTIVE row for the exact tuple
	// (participant, build, dictionary, schema, env) regardless of result.
	Latest(ctx context.Context, participant, build, dict, schema, env string) (*Record, error)
	// Revoke flips a row to REVOKED with a reason (material protocol
	// change, incident finding).
	Revoke(ctx context.Context, id int64, reason string) error
	// RevokeSchema mass-revokes ACTIVE rows on a venue schema version —
	// the "material protocol changes revoke certification" rule.
	RevokeSchema(ctx context.Context, schemaVersion, reason string) (int64, error)
}

// Gate decides whether a client build may open a production order-entry
// session. Constructed per listener environment.
type Gate struct {
	store Store
	env   string
	now   func() time.Time
}

// NewGate builds the gate for one environment (e.g. "production").
func NewGate(store Store, environment string, now func() time.Time) *Gate {
	if now == nil {
		now = time.Now
	}
	return &Gate{store: store, env: environment, now: now}
}

// GateError is a coded gate rejection — rendered into the Logon reject
// Text(58) by the session layer.
type GateError struct {
	Code   string // CERTIFICATION_REQUIRED | CERTIFICATION_STALE | CERTIFICATION_UNAVAILABLE
	Detail string
}

func (e *GateError) Error() string { return e.Code + ": " + e.Detail }

// Admit returns nil when a current ACTIVE PASS certification covers the
// exact tuple (participant, build, dictionary, schema) in this gate's
// environment. Any miss, FAIL, expired or revoked row — or a store
// error — fails closed.
func (g *Gate) Admit(ctx context.Context, participant, build, dict, schema string) error {
	if g == nil || g.store == nil {
		return &GateError{Code: "CERTIFICATION_UNAVAILABLE",
			Detail: "certification store not wired"}
	}
	rec, err := g.store.Latest(ctx, participant, build, dict, schema, g.env)
	if err != nil {
		return &GateError{Code: "CERTIFICATION_UNAVAILABLE",
			Detail: fmt.Sprintf("certification lookup failed: %v", err)}
	}
	if rec == nil {
		return &GateError{Code: "CERTIFICATION_REQUIRED",
			Detail: fmt.Sprintf("no certification for build %q on %s/%s in %s",
				build, dict, schema, g.env)}
	}
	if rec.Status != StatusActive {
		return &GateError{Code: "CERTIFICATION_STALE",
			Detail: fmt.Sprintf("certification %d status %s", rec.ID, rec.Status)}
	}
	if rec.Result != ResultPass {
		return &GateError{Code: "CERTIFICATION_REQUIRED",
			Detail: fmt.Sprintf("latest certification run for build %q did not pass", build)}
	}
	if !g.now().Before(rec.ExpiresAt) {
		return &GateError{Code: "CERTIFICATION_STALE",
			Detail: fmt.Sprintf("certification %d expired %s", rec.ID,
				rec.ExpiresAt.UTC().Format(time.RFC3339))}
	}
	return nil
}

// Register records a certification run outcome. A PASS supersedes prior
// ACTIVE rows for the tuple (they are not deleted — superseded rows
// remain queryable history and stop satisfying the gate via Latest,
// which always reads the newest row for the tuple).
func Register(ctx context.Context, s Store, r *Record) (*Record, error) {
	if r.Result != ResultPass && r.Result != ResultFail {
		return nil, fmt.Errorf("certification: result must be PASS or FAIL")
	}
	if r.Status == "" {
		r.Status = StatusActive
	}
	if !r.ExpiresAt.After(r.CertifiedAt) {
		return nil, fmt.Errorf("certification: expires_at must be after certified_at")
	}
	return s.Record(ctx, r)
}
