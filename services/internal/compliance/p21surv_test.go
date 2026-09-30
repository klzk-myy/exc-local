// Phase-21 wave-2 surveillance/compliance — pure-helper unit tests
// (no PG). Covers Task 21.3.8 vocabulary + severity thresholds, Task
// 21.3.21 case state machine + STOR payload, Task 21.3.24 sensitive-
// role set, and Task 21.3.27 masking/LEI/FP-target helpers.
package compliance

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"
	"testing"
	"time"
)

// urgentEvidence: z≥3σ or confidence=HIGH in the Phase-17 evidence
// blob marks a case URGENT (4h SLA); anything weaker stays REVIEW.
func TestUrgentEvidenceThresholds(t *testing.T) {
	cases := []struct {
		name string
		ev   string
		want bool
	}{
		{"z_score 3.2", `{"z_score":3.2}`, true},
		{"z boundary", `{"z":3.0}`, true},
		{"zscore 4.9", `{"zscore":4.9}`, true},
		{"z below", `{"z_score":2.99}`, false},
		{"confidence HIGH", `{"confidence":"HIGH"}`, true},
		{"confidence MED", `{"confidence":"MEDIUM"}`, false},
		{"empty object", `{}`, false},
		{"not json", `not-json`, false},
		{"z as string", `{"z_score":"4.5"}`, false},
	}
	for _, c := range cases {
		if got := urgentEvidence(json.RawMessage(c.ev)); got != c.want {
			t.Fatalf("%s: urgentEvidence=%v want %v", c.name, got, c.want)
		}
	}
}

// Terminal dispositions are the four spec §14.9.2 outcomes — and
// nothing else (a CLOSED_* case never reopens via the same row).
func TestCaseTerminalStates(t *testing.T) {
	for _, s := range []string{
		CaseStatusClosedFP, CaseStatusEscalatedSAR,
		CaseStatusEscalatedSTR, CaseStatusEscalatedAct,
	} {
		if !isCaseTerminal(s) {
			t.Fatalf("%s must be terminal", s)
		}
	}
	for _, s := range []string{
		CaseStatusOpen, CaseStatusAssigned, CaseStatusInvestigating, "",
	} {
		if isCaseTerminal(s) {
			t.Fatalf("%s must not be terminal", s)
		}
	}
}

// SLA constants pin the spec §14.9.2 assignment deadlines.
func TestCaseSLAConstants(t *testing.T) {
	if CaseSLAUrgent != 4*time.Hour {
		t.Fatalf("urgent SLA %v want 4h", CaseSLAUrgent)
	}
	if CaseSLAReview != 24*time.Hour {
		t.Fatalf("review SLA %v want 24h", CaseSLAReview)
	}
}

// BuildSTORPayload carries the case identity into the STOR body —
// the regulator-facing snapshot must mirror the case row.
func TestSTORPayloadBuild(t *testing.T) {
	sigID := int64(77)
	acctID := int64(42)
	now := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	c := &Case{
		CaseRef: "case_abc", SignalID: &sigID, AccountID: &acctID,
		AccountHash: 991, SignalType: "SPOOFING", Symbol: "EURUSD",
		Evidence: json.RawMessage(`{"z":4.1}`),
		OpenedAt: now.Add(-2 * time.Hour),
	}
	p := BuildSTORPayload(c, "suspicious layering pattern", now)
	if p.Format != "EXC-STOR/1.0" || p.CaseRef != "case_abc" {
		t.Fatalf("stor header wrong: %+v", p)
	}
	if p.SignalID == nil || *p.SignalID != 77 {
		t.Fatalf("signal id not carried")
	}
	if p.AccountHash != "991" || p.Instrument != "EURUSD" {
		t.Fatalf("account/instrument wrong: %+v", p)
	}
	if p.ReportedAt != now || p.DetectedAt != c.OpenedAt {
		t.Fatalf("timestamps wrong")
	}
}

// Masking (Task 21.3.27 auditor export): v4 truncates to /24, v6 to
// /48, unparseable → "***".
func TestMaskIP(t *testing.T) {
	if got := maskIP("203.0.113.87"); got != "203.0.113.0/24" {
		t.Fatalf("v4 mask %q", got)
	}
	if got := maskIP("2001:db8:abcd:1234::1"); got != "2001:db8:abcd::/48" {
		t.Fatalf("v6 mask %q", got)
	}
	if got := maskIP("not-an-ip"); got != "***" {
		t.Fatalf("bad mask %q", got)
	}
}

// scrubJSON rewrites PII-keyed leaves at every depth while leaving
// operational keys untouched.
func TestScrubJSONPII(t *testing.T) {
	in := json.RawMessage(`{
		"email":"a@b.c","tin":"123","action":"freeze",
		"nested":{"ssn":"999-00","safe":7},
		"list":[{"name":"x"},1]}`)
	out := scrubJSON(in)
	var m map[string]any
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatalf("scrubbed output not JSON: %v", err)
	}
	if m["email"] != "***" || m["tin"] != "***" || m["action"] != "freeze" {
		t.Fatalf("top-level scrub wrong: %v", m)
	}
	nested := m["nested"].(map[string]any)
	if nested["ssn"] != "***" || nested["safe"] != float64(7) {
		t.Fatalf("nested scrub wrong: %v", nested)
	}
	list := m["list"].([]any)
	if list[0].(map[string]any)["name"] != "***" {
		t.Fatalf("list-element scrub wrong: %v", list)
	}
	if got := scrubJSON(json.RawMessage(`{bad`)); string(got) != `"***"` {
		t.Fatalf("invalid json → %s", got)
	}
}

// leiCheckDigits computes the ISO 7064 mod-97-10 check digits for an
// 18-char LEI body — an independent construction used to synthesise a
// valid LEI for the positive checksum case.
func leiCheckDigits(body18 string) string {
	var b strings.Builder
	for _, r := range body18 {
		if r >= '0' && r <= '9' {
			b.WriteRune(r)
		} else {
			b.WriteString(strconv.Itoa(int(r - 'A' + 10)))
		}
	}
	b.WriteString("00")
	rem := 0
	for _, r := range b.String() {
		rem = (rem*10 + int(r-'0')) % 97
	}
	check := (98 - rem) % 97
	return string(rune('0'+check/10)) + string(rune('0'+check%10))
}

// ValidateLEI: a constructed-valid LEI passes; wrong length, lowercase
// and corrupted check digits fail (spec §14.9.1 validated identifiers).
func TestValidateLEI(t *testing.T) {
	body := "549300ABCDEFGH1234" // 18 chars + 2 check digits = 20
	if len(body) != 18 {
		t.Fatalf("fixture body len=%d", len(body))
	}
	valid := body + leiCheckDigits(body)
	if err := ValidateLEI(valid); err != nil {
		t.Fatalf("constructed LEI %q must validate: %v", valid, err)
	}
	if err := ValidateLEI(body + "00"); err == nil &&
		leiCheckDigits(body) != "00" {
		t.Fatal("corrupted check digits must fail")
	}
	for _, bad := range []string{
		"", "549300AB", "549300ABCDEFGH123400EXTRA",
		strings.ToLower(valid), "549300ABCDEFGH1234!@",
	} {
		if err := ValidateLEI(bad); err == nil {
			t.Fatalf("LEI %q must reject", bad)
		}
	}
}

// Every severe signal type (the ones barred from auto-escalation past
// WARN) must carry a default FP target — the tuning register refuses
// silent zeroes.
func TestDefaultFPTargetsCoverSevere(t *testing.T) {
	for st := range severeSignalTypes {
		if DefaultFPTargets[st] <= 0 {
			t.Fatalf("severe signal %s lacks an FP target", st)
		}
	}
	if len(DefaultFPTargets) < 5 {
		t.Fatalf("FP target map suspiciously small: %v", DefaultFPTargets)
	}
}

// The sensitive-role set must name the privileged desks — an empty
// map silently disables the always-clearance gate.
func TestSensitiveRolesPopulated(t *testing.T) {
	for _, r := range []string{
		"CORE_ENGINE", "CORE_OPS", "COMPLIANCE", "LP_MANAGEMENT", "PRODUCT",
	} {
		if !SensitiveRoles[r] {
			t.Fatalf("sensitive role %s missing", r)
		}
	}
}

// Action vocabulary mirrors the migration CHECK — a drifted constant
// would surface as INSERT rejections, not compile errors.
func TestEnforcementActionVocabulary(t *testing.T) {
	for _, a := range []string{
		EnfActionWarn, EnfActionThrottle, EnfActionRestrict,
		EnfActionSuspend, EnfActionDismiss,
	} {
		if a == "" {
			t.Fatal("blank action constant")
		}
	}
	if EnfActionWarn != "WARN" || EnfActionDismiss != "DISMISS" {
		t.Fatal("vocabulary drifted from migration CHECK")
	}
}

// The audit-trail filter zero-value must be valid input (no mandatory
// fields — the auditor narrows progressively).
func TestAuditTrailFilterZeroValue(t *testing.T) {
	var f AuditTrailFilter
	if f.Mask || f.AfterID != 0 || f.Action != "" {
		t.Fatal("zero filter must be empty")
	}
}

// MonitoringFinding carries through CaseSink.OpenCase — shape sanity
// (field names feed the dedup source_ref).
func TestMonitoringFindingShape(t *testing.T) {
	f := MonitoringFinding{
		AccountID: 7, Rule: "structuring_24h", Severity: "P2",
		Summary: "x", Evidence: map[string]any{"amt": "9000"},
		DetectedAt: time.Unix(1700000000, 0).UTC(),
	}
	b, err := json.Marshal(f)
	if err != nil || !strings.Contains(string(b), "structuring_24h") {
		t.Fatalf("finding marshal: %v %s", err, b)
	}
}

// CaseService compiles as the monitoring.CaseSink implementation —
// the seam Task 21.3.21 binds over the audit fallback.
func TestCaseServiceImplementsCaseSink(t *testing.T) {
	var _ CaseSink = (*CaseService)(nil)
	var _ = context.Background // keep import honest
}
