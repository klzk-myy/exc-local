// Unit tests for the Phase-09 Task 9.3.29 item-4 secrets inventory —
// state machine, fail-closed validation, coverage verdict and the
// SECRET_ROTATION_OVERDUE emission contract. The PG-backed CRUD and
// audit-trail legs live in secrets_inventory_integration_test.go
// (EXC_PG_TEST-gated).
package security

import (
	"context"
	"errors"
	"testing"
	"time"

	"exchange/internal/errs"
)

func fixedNow() time.Time {
	return time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
}

func invSvcForUnit() *InventoryService {
	return NewInventoryService(nil, nil, nil, nil)
}

func TestInventoryAssessStates(t *testing.T) {
	now := fixedNow()
	rotated := now.Add(-10 * 24 * time.Hour) // 80d before a 90d deadline

	// Never rotated → UNROTATED (provisioning gap, not an SLA breach).
	e := &InventoryEntry{SecretName: "jwt-hs256-key", Class: ClassJWTSigningKey,
		TTL: 90 * 24 * time.Hour, AlertLead: 14 * 24 * time.Hour}
	if st := e.assess(now); st.State != StateUnrotated {
		t.Fatalf("unrotated: state=%s", st.State)
	}

	// Comfortably inside TTL → OK.
	e.LastRotatedAt = &rotated
	if st := e.assess(now); st.State != StateOK {
		t.Fatalf("fresh: state=%s", st.State)
	}

	// Inside the 14-day alert lead → DUE_SOON.
	rotated = now.Add(-77 * 24 * time.Hour) // deadline in 13d < 14d lead
	if st := e.assess(now); st.State != StateDueSoon {
		t.Fatalf("lead: state=%s want due_soon", st.State)
	}

	// Past TTL → OVERDUE, negative seconds-until-expiry.
	rotated = now.Add(-91 * 24 * time.Hour)
	st := e.assess(now)
	if st.State != StateOverdue || st.SecondsUntilExpiry >= 0 {
		t.Fatalf("overdue: state=%s secs=%f", st.State, st.SecondsUntilExpiry)
	}
}

func TestInventoryValidateFailClosed(t *testing.T) {
	svc := invSvcForUnit()
	ok := InventoryEntryInput{
		SecretName: "jwt-hs256-key", Class: ClassJWTSigningKey,
		Owner: "Platform / oncall", TTL: 90 * 24 * time.Hour,
	}
	if err := svc.validate(ok); err != nil {
		t.Fatalf("valid input rejected: %v", err)
	}

	cases := []struct {
		name string
		mut  func(*InventoryEntryInput)
	}{
		{"empty name", func(i *InventoryEntryInput) { i.SecretName = "" }},
		{"whitespace name", func(i *InventoryEntryInput) { i.SecretName = "jwt key" }},
		{"bad class", func(i *InventoryEntryInput) { i.Class = "nuclear_launch_codes" }},
		{"no owner", func(i *InventoryEntryInput) { i.Owner = "" }},
		{"zero ttl", func(i *InventoryEntryInput) { i.TTL = 0 }},
		// 100d TTL on a 90d-ceiling class: a weaker policy is a defect.
		{"ttl over ceiling", func(i *InventoryEntryInput) { i.TTL = 100 * 24 * time.Hour }},
		{"lead over ttl", func(i *InventoryEntryInput) {
			i.TTL = 7 * 24 * time.Hour
			i.AlertLead = 14 * 24 * time.Hour
		}},
	}
	for _, c := range cases {
		in := ok
		c.mut(&in)
		if err := svc.validate(in); err == nil {
			t.Fatalf("%s: expected rejection", c.name)
		}
	}

	// TLS certs carry a 365-day class ceiling — 200d is legal.
	in := ok
	in.Class, in.TTL = ClassTLSCertificate, 200*24*time.Hour
	if err := svc.validate(in); err != nil {
		t.Fatalf("tls 200d within class ceiling rejected: %v", err)
	}
}

func TestInventoryViewPartitions(t *testing.T) {
	svc := invSvcForUnit()
	now := fixedNow()
	fresh := now.Add(-24 * time.Hour)
	stale := now.Add(-100 * 24 * time.Hour)
	rows := []InventoryEntry{
		{SecretName: "a-ok", Class: ClassJWTSigningKey,
			TTL: 90 * 24 * time.Hour, LastRotatedAt: &fresh},
		{SecretName: "b-overdue", Class: ClassBankingAPIKey,
			TTL: 90 * 24 * time.Hour, AlertLead: 14 * 24 * time.Hour,
			LastRotatedAt: &stale},
		{SecretName: "c-unrotated", Class: ClassDBCredential,
			TTL: 90 * 24 * time.Hour},
	}
	v := svc.view(rows, now)
	if len(v.Rows) != 3 {
		t.Fatalf("rows=%d", len(v.Rows))
	}
	if len(v.Overdue) != 1 || v.Overdue[0] != "b-overdue" {
		t.Fatalf("overdue=%v", v.Overdue)
	}
	if len(v.Unrotated) != 1 || v.Unrotated[0] != "c-unrotated" {
		t.Fatalf("unrotated=%v", v.Unrotated)
	}
}

func TestCoverageReportFailClosed(t *testing.T) {
	svc := invSvcForUnit()
	now := fixedNow()

	// Empty inventory → every DefaultRegistry class missing.
	rep := svc.coverageReport(nil, nil)
	if rep.OK || len(rep.MissingClasses) != 7 {
		t.Fatalf("empty coverage: ok=%v missing=%v", rep.OK, rep.MissingClasses)
	}

	// One row per class + the canonical refs → OK.
	var rows []InventoryEntry
	for _, c := range []SecretClass{
		ClassJWTSigningKey, ClassAPIKeyMaterial, ClassDBCredential,
		ClassRedisPassword, ClassAeronToken, ClassTLSCertificate,
		ClassBankingAPIKey,
	} {
		rows = append(rows, InventoryEntry{
			SecretName: "s-" + string(c), Class: c,
			TTL: 30 * 24 * time.Hour, LastRotatedAt: &now,
		})
	}
	refs := []SecretRef{
		{Name: "s-jwt_signing_key", Required: true},
		{Name: "s-banking_api_key", Required: true},
		{Name: "undeclared-secret", Required: false},
	}
	rep = svc.coverageReport(rows, refs)
	if rep.OK || len(rep.MissingSecrets) != 1 || rep.MissingSecrets[0] != "undeclared-secret" {
		t.Fatalf("coverage with missing ref: %+v", rep)
	}
	rep = svc.coverageReport(rows, refs[:2])
	if !rep.OK {
		t.Fatalf("full coverage should pass: %+v", rep)
	}
}

func TestInventoryMarkRotatedValidation(t *testing.T) {
	superAdmin := RoleResolver(func(context.Context, int64) (string, error) {
		return "Super Admin", nil
	})
	svc := NewInventoryService(nil, superAdmin, nil, nil)

	// Emergency marks must carry the incident record.
	_, err := svc.MarkRotated(context.Background(), 7, "jwt-hs256-key",
		RotationMark{Emergency: true}, "127.0.0.1")
	requireCode(t, err, "INVALID_REQUEST")

	// Break-glass executions must carry the Task 7.3.12 grant id.
	_, err = svc.MarkRotated(context.Background(), 7, "jwt-hs256-key",
		RotationMark{BreakGlass: true}, "127.0.0.1")
	requireCode(t, err, "INVALID_REQUEST")
}

func TestInventoryRoleGate(t *testing.T) {
	// Doc §5: inventory reads/writes are Security + SRE leads — Super
	// Admin in the §8.2 canon. Every other role is refused.
	deny := RoleResolver(func(context.Context, int64) (string, error) {
		return "Support Agent", nil
	})
	svc := NewInventoryService(nil, deny, nil, nil)
	_, err := svc.List(context.Background(), 7)
	requireCode(t, err, "UNAUTHORIZED_ROLE")
	_, err = svc.UpsertEntry(context.Background(), 7, InventoryEntryInput{}, "")
	requireCode(t, err, "UNAUTHORIZED_ROLE")

	// Nil resolver fails closed, never open.
	svc = NewInventoryService(nil, nil, nil, nil)
	_, err = svc.List(context.Background(), 7)
	requireCode(t, err, "UNAUTHORIZED_ROLE")

	// Resolver failure is INTERNAL_ERROR, not a silent grant.
	svc = NewInventoryService(nil, RoleResolver(func(context.Context, int64) (string, error) {
		return "", errors.New("rbac store down")
	}), nil, nil)
	_, err = svc.List(context.Background(), 7)
	requireCode(t, err, "INTERNAL_ERROR")
}

// TestOverdueCodeRegistered pins the emission contract: the code the
// spec pins (§23 row, HTTP 503) must resolve through the registry —
// WriteError degrades unregistered codes to INTERNAL_ERROR, which would
// silently break the 503 surface.
func TestOverdueCodeRegistered(t *testing.T) {
	d, ok := errs.Default.Lookup(CodeSecretRotationOverdue)
	if !ok {
		t.Fatalf("%s not in the §23 registry", CodeSecretRotationOverdue)
	}
	if d.HTTPStatus != 503 {
		t.Fatalf("%s status=%d want 503", d.Code, d.HTTPStatus)
	}
}
