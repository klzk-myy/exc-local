// pack.go — the versioned certification pack (spec §9.7 item 3,
// Task 18.3.17 item 4): a fixed, ordered case catalog executed against
// a venue conformance environment; the transcript hashes into the
// persisted evidence_hash.
package certification

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
	"time"
)

// PackVersion pins the case catalog below. Bump on any case
// add/remove/semantics change — a certification is only meaningful
// against the pack version that ran it.
const PackVersion = "1.0.0"

// Case is one conformance scenario. IDs are stable wire tokens — never
// renumbered; retired cases stay in the catalog flagged Retired so an
// old evidence hash remains interpretable.
type Case struct {
	ID      string
	Name    string
	Retired bool
}

// Cases is the certification catalog (spec §9.7 + Task 18.3.17 item 4
// "tag-value↔SBE equivalence, schema upgrade/retirement, maintenance
// reconnect, duplicate suppression").
var Cases = []Case{
	{ID: "logon", Name: "Logon/Logout handshake and sequence reset"},
	{ID: "heartbeat", Name: "Heartbeat and TestRequest response"},
	{ID: "resend_gapfill", Name: "ResendRequest gap-fill and SequenceReset"},
	{ID: "possdup", Name: "PossDup duplicate-order suppression"},
	{ID: "cancel_replace", Name: "OrderCancelRequest / OrderCancelReplaceRequest"},
	{ID: "session_reject", Name: "Session Reject (35=3) on malformed/invalid tags"},
	{ID: "business_reject", Name: "BusinessMessageReject (35=j) on entitlement/throttle"},
	{ID: "malformed", Name: "Malformed frame handling (bad checksum/length)"},
	{ID: "entitlement", Name: "SESSION_NOT_ENTITLED account/instrument gate"},
	{ID: "throttle", Name: "max_msgs_per_sec throttle rejection"},
	{ID: "cod", Name: "Cancel-on-disconnect mass-cancel on socket drop"},
	{ID: "dropcopy", Name: "Drop-copy replication of all executions"},
	{ID: "recovery", Name: "Reconnect + sequence resynchronization"},
	// Task 18.3.17 production transport cases.
	{ID: "sbe_equiv", Name: "Tag-value ↔ SBE semantic equivalence"},
	{ID: "schema_upgrade", Name: "Schema version upgrade/retirement negotiation"},
	{ID: "news_drain", Name: "Maintenance News drain + reconnect to replacement endpoint"},
	{ID: "dup_suppress_transport", Name: "Cross-transport duplicate suppression"},
}

// Probe executes one case against the venue's conformance environment.
// A case error is a case failure; the harness records it and continues.
type Probe interface {
	RunCase(ctx context.Context, c Case) error
}

// ProbeFunc adapts a function to Probe.
type ProbeFunc func(ctx context.Context, c Case) error

// RunCase implements Probe.
func (f ProbeFunc) RunCase(ctx context.Context, c Case) error { return f(ctx, c) }

// Transcript is the ordered per-case outcome log.
type Transcript struct {
	PackVersion string
	StartedAt   time.Time
	FinishedAt  time.Time
	Results     []CaseResult
}

// CaseResult is one executed case.
type CaseResult struct {
	CaseID   string
	Pass     bool
	Detail   string
	Duration time.Duration
}

// Run executes every non-retired case in catalog order. Overall PASS
// requires every case to pass.
func Run(ctx context.Context, probe Probe, now time.Time) *Transcript {
	t := &Transcript{PackVersion: PackVersion, StartedAt: now}
	for _, c := range Cases {
		if c.Retired {
			continue
		}
		start := time.Now()
		err := probe.RunCase(ctx, c)
		cr := CaseResult{CaseID: c.ID, Pass: err == nil, Duration: time.Since(start)}
		if err != nil {
			cr.Detail = err.Error()
		}
		t.Results = append(t.Results, cr)
	}
	t.FinishedAt = time.Now()
	return t
}

// Passed reports whether every executed case passed.
func (t *Transcript) Passed() bool {
	for _, r := range t.Results {
		if !r.Pass {
			return false
		}
	}
	return len(t.Results) > 0
}

// EvidenceHash is the sha256 over the canonical transcript rendering —
// persisted as fix_certifications.evidence_hash.
func (t *Transcript) EvidenceHash() string {
	h := sha256.New()
	fmt.Fprintf(h, "pack=%s\nstarted=%s\n", t.PackVersion,
		t.StartedAt.UTC().Format(time.RFC3339Nano))
	for _, r := range t.Results {
		fmt.Fprintf(h, "%s|%v|%s\n", r.CaseID, r.Pass, r.Detail)
	}
	return hex.EncodeToString(h.Sum(nil))
}

// ToRecord folds a transcript into a persistable Record for the tuple.
func (t *Transcript) ToRecord(participant, build, dict, schema, env string,
	ttl time.Duration) *Record {
	res := ResultFail
	if t.Passed() {
		res = ResultPass
	}
	return &Record{
		ParticipantID:      participant,
		ClientBuild:        build,
		DictionaryVersion:  dict,
		VenueSchemaVersion: schema,
		Environment:        env,
		PackVersion:        t.PackVersion,
		Result:             res,
		EvidenceHash:       t.EvidenceHash(),
		CertifiedAt:        t.FinishedAt,
		ExpiresAt:          t.FinishedAt.Add(ttl),
		Status:             StatusActive,
	}
}

// Catalog renders the case list for ops inspection.
func Catalog() string {
	var b strings.Builder
	fmt.Fprintf(&b, "certification pack %s:\n", PackVersion)
	ids := append([]Case(nil), Cases...)
	sort.Slice(ids, func(i, j int) bool { return ids[i].ID < ids[j].ID })
	for _, c := range ids {
		mark := " "
		if c.Retired {
			mark = "-"
		}
		fmt.Fprintf(&b, " %s %s — %s\n", mark, c.ID, c.Name)
	}
	return b.String()
}
