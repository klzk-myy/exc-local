// certification_test.go — Task 18.3.11 gate matrix + pack runner +
// register/supersede/revoke semantics.
package certification

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"
)

func rec(participant, build, dict, schema, env string, at, exp time.Time) *Record {
	return &Record{
		ParticipantID:      participant,
		ClientBuild:        build,
		DictionaryVersion:  dict,
		VenueSchemaVersion: schema,
		Environment:        env,
		PackVersion:        PackVersion,
		Result:             ResultPass,
		EvidenceHash:       "ab12",
		CertifiedAt:        at,
		ExpiresAt:          exp,
		Status:             StatusActive,
	}
}

func gateErr(t *testing.T, err error) *GateError {
	t.Helper()
	var ge *GateError
	if !errors.As(err, &ge) {
		t.Fatalf("expected GateError, got %v", err)
	}
	return ge
}

// TestGateMatrix — the certification admission decision across the
// full tuple/lifecycle space (spec §9.7: uncertified or stale-certified
// builds cannot open production order-entry sessions).
func TestGateMatrix(t *testing.T) {
	now := time.Now()
	ctx := context.Background()
	store := NewMemoryStore()
	gate := NewGate(store, "production", func() time.Time { return now })

	tuple := func() (string, string, string, string) {
		return "firmA", "clientlib-4.2.1", "FIX.4.4", "fix-core.v1"
	}
	p, b, dict, schema := tuple()

	// 1. No row → REQUIRED.
	if err := gate.Admit(ctx, p, b, dict, schema); gateErr(t, err).Code != "CERTIFICATION_REQUIRED" {
		t.Fatal("uncertified build admitted")
	}

	// 2. Latest run FAILED → REQUIRED.
	bad := rec(p, b, dict, schema, "production", now.Add(-time.Hour), now.Add(24*time.Hour))
	bad.Result = ResultFail
	if _, err := Register(ctx, store, bad); err != nil {
		t.Fatal(err)
	}
	if err := gate.Admit(ctx, p, b, dict, schema); gateErr(t, err).Code != "CERTIFICATION_REQUIRED" {
		t.Fatal("failed certification admitted")
	}

	// 3. PASS in staging → production still REQUIRED (environment is
	// part of the tuple).
	if _, err := Register(ctx, store, rec(p, b, dict, schema, "staging",
		now.Add(-time.Hour), now.Add(24*time.Hour))); err != nil {
		t.Fatal(err)
	}
	if err := gate.Admit(ctx, p, b, dict, schema); gateErr(t, err).Code != "CERTIFICATION_REQUIRED" {
		t.Fatal("staging certification admitted to production")
	}

	// 4. PASS in production → admitted.
	good := rec(p, b, dict, schema, "production", now.Add(-time.Hour), now.Add(24*time.Hour))
	good.CertifiedAt = now.Add(-time.Minute) // newest row for the tuple
	if _, err := Register(ctx, store, good); err != nil {
		t.Fatal(err)
	}
	if err := gate.Admit(ctx, p, b, dict, schema); err != nil {
		t.Fatalf("certified build rejected: %v", err)
	}

	// 5. Expired certification → STALE.
	old := rec(p, "old-build", dict, schema, "production",
		now.Add(-90*24*time.Hour), now.Add(-time.Hour))
	if _, err := Register(ctx, store, old); err != nil {
		t.Fatal(err)
	}
	if err := gate.Admit(ctx, p, "old-build", dict, schema); gateErr(t, err).Code != "CERTIFICATION_STALE" {
		t.Fatal("expired certification admitted")
	}

	// 6. Revoked certification → STALE.
	rev := rec(p, "revoked-build", dict, schema, "production",
		now.Add(-time.Hour), now.Add(24*time.Hour))
	saved, err := Register(ctx, store, rev)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Revoke(ctx, saved.ID, "material protocol change"); err != nil {
		t.Fatal(err)
	}
	if err := gate.Admit(ctx, p, "revoked-build", dict, schema); gateErr(t, err).Code != "CERTIFICATION_STALE" {
		t.Fatal("revoked certification admitted")
	}

	// 7. Tuple discipline: different schema version is a different key —
	// the material-change rule forces re-certification.
	if err := gate.Admit(ctx, p, b, dict, "fix-core.v2"); gateErr(t, err).Code != "CERTIFICATION_REQUIRED" {
		t.Fatal("certification carried across schema versions")
	}
	// Different dictionary likewise.
	if err := gate.Admit(ctx, p, b, "FIX.5.0SP2", schema); gateErr(t, err).Code != "CERTIFICATION_REQUIRED" {
		t.Fatal("certification carried across dictionaries")
	}

	// 8. Nil store fails closed.
	if err := NewGate(nil, "production", nil).Admit(ctx, p, b, dict, schema); gateErr(t, err).Code != "CERTIFICATION_UNAVAILABLE" {
		t.Fatal("nil store not fail-closed")
	}
}

// TestRevokeSchema — a material protocol change revokes every ACTIVE
// certification bound to the old schema version.
func TestRevokeSchema(t *testing.T) {
	now := time.Now()
	ctx := context.Background()
	store := NewMemoryStore()
	_, _ = Register(ctx, store, rec("a", "b1", "FIX.4.4", "v1", "production", now, now.Add(time.Hour)))
	_, _ = Register(ctx, store, rec("b", "b2", "FIX.4.4", "v1", "production", now, now.Add(time.Hour)))
	_, _ = Register(ctx, store, rec("c", "b3", "FIX.4.4", "v2", "production", now, now.Add(time.Hour)))
	n, err := store.RevokeSchema(ctx, "v1", "dictionary update 2026-09")
	if err != nil || n != 2 {
		t.Fatalf("revoked %d rows, want 2 (%v)", n, err)
	}
	gate := NewGate(store, "production", nil)
	if err := gate.Admit(ctx, "a", "b1", "FIX.4.4", "v1"); gateErr(t, err).Code != "CERTIFICATION_STALE" {
		t.Fatal("schema-revoked certification admitted")
	}
	if err := gate.Admit(ctx, "c", "b3", "FIX.4.4", "v2"); err != nil {
		t.Fatal("other-schema certification wrongly revoked")
	}
}

// TestRegisterValidation — records must carry a result and a sane
// validity window.
func TestRegisterValidation(t *testing.T) {
	now := time.Now()
	store := NewMemoryStore()
	r := rec("p", "b", "FIX.4.4", "v1", "production", now, now.Add(time.Hour))
	r.Result = "MAYBE"
	if _, err := Register(context.Background(), store, r); err == nil {
		t.Fatal("invalid result persisted")
	}
	r2 := rec("p", "b", "FIX.4.4", "v1", "production", now, now.Add(-time.Hour))
	if _, err := Register(context.Background(), store, r2); err == nil {
		t.Fatal("backwards validity window persisted")
	}
}

// TestPackRunner — all-passing probe → PASS + stable evidence hash; a
// failing case → FAIL; catalog covers the required §9.7 cases.
func TestPackRunner(t *testing.T) {
	pass := ProbeFunc(func(_ context.Context, _ Case) error { return nil })
	tr := Run(context.Background(), pass, time.Now())
	if !tr.Passed() {
		t.Fatal("all-pass probe should pass")
	}
	if tr.EvidenceHash() == "" {
		t.Fatal("missing evidence hash")
	}
	recd := tr.ToRecord("firm", "build", "FIX.4.4", "v1", "staging", 90*24*time.Hour)
	if recd.Result != ResultPass || recd.PackVersion != PackVersion {
		t.Fatalf("record: %+v", recd)
	}

	fail := ProbeFunc(func(_ context.Context, c Case) error {
		if c.ID == "cod" {
			return fmt.Errorf("cancel-on-disconnect never fired")
		}
		return nil
	})
	tr2 := Run(context.Background(), fail, time.Now())
	if tr2.Passed() {
		t.Fatal("failing case should fail the run")
	}

	// Required coverage (spec §9.7 + Task 18.3.17).
	want := map[string]bool{
		"logon": false, "heartbeat": false, "resend_gapfill": false,
		"possdup": false, "cancel_replace": false, "session_reject": false,
		"business_reject": false, "malformed": false, "entitlement": false,
		"throttle": false, "cod": false, "recovery": false,
		"sbe_equiv": false, "schema_upgrade": false, "news_drain": false,
	}
	for _, c := range Cases {
		if _, ok := want[c.ID]; ok && !c.Retired {
			want[c.ID] = true
		}
	}
	for id, seen := range want {
		if !seen {
			t.Fatalf("certification pack missing required case %q", id)
		}
	}
}
