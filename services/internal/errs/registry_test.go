package errs

import (
	"net/http"
	"testing"

	pkgerrors "exchange/pkg/errors"
)

func TestSpecTableSizeAndUniqueness(t *testing.T) {
	// 200 = 185 + 6 concurrent Phase-16 sibling registrations +
	// GSLO_EXPOSURE_EXCEEDED (Phase-16 Task 16.3.16 — §24 matrix code
	// tabled 2026-09-29 during implementation) + 3 Phase-18 quoting/MMP
	// codes (Tasks 18.3.7/18.3.10: QUOTE_REQUEST_REJECTED, MMP_TRIGGERED,
	// MMP_LOCKED_OUT) + 2 Phase-18 allocation codes (Task 18.3.13:
	// ALLOCATION_SUM_MISMATCH, ALLOCATION_INVALID) + 1 Phase-18 SOR code
	// (Task 18.3.14: ROUTING_REJECTED) + 2 Phase-19 margin codes
	// (Task 19.3.1/19.3.23: MARGIN_MODE_SWITCH_BLOCKED; Task 19.3.3
	// §13.6d order-entry block: MARGIN_CALL_EXCEEDED) + 1 baseline =
	// 200; + 5 Phase-19.5 oracle codes (Tasks 19.5.3.2–3.7:
	// ORACLE_FEED_STALE, ORACLE_DIVERGENCE_EXCEEDED, MARK_PRICE_STALE,
	// MARK_PRICE_OUT_OF_BOUNDS, STALE_FORWARD_POINTS).
	// + 1 Phase-21 Task 21.3.8 (ENFORCEMENT_ACTION_EXISTS, registered
	// with the task landing).
	if got := len(specCodes); got != 207 {
		t.Fatalf("spec §23 table must carry 207 codes, got %d", got)
	}
	seen := map[string]bool{}
	for _, d := range specCodes {
		if seen[d.Code] {
			t.Fatalf("duplicate code in spec table: %s", d.Code)
		}
		seen[d.Code] = true
		if d.HTTPStatus < 400 || d.HTTPStatus > 599 {
			t.Errorf("%s: implausible HTTP status %d", d.Code, d.HTTPStatus)
		}
		if d.Description == "" {
			t.Errorf("%s: empty description", d.Code)
		}
	}
}

func TestRegistryLoad(t *testing.T) {
	r := New()
	if got, want := r.Len(), len(specCodes)+len(localCodes); got != want {
		t.Fatalf("registry size %d, want %d", got, want)
	}
	d, ok := r.Lookup("INSUFFICIENT_BALANCE")
	if !ok || d.HTTPStatus != http.StatusBadRequest {
		t.Fatalf("INSUFFICIENT_BALANCE lookup: %+v ok=%v", d, ok)
	}
	if got := r.HTTPStatus("GATEWAY_TIMEOUT_MATCHING_ENGINE"); got != http.StatusGatewayTimeout {
		t.Fatalf("GATEWAY_TIMEOUT_MATCHING_ENGINE status %d, want 504", got)
	}
	if got := r.HTTPStatus("NO_SUCH_CODE"); got != http.StatusInternalServerError {
		t.Fatalf("unknown code status %d, want 500 (fail-closed)", got)
	}
}

func TestOwnerExtraction(t *testing.T) {
	r := New()
	cases := map[string]string{
		"INSUFFICIENT_BALANCE":            "Phase-02 Task 2.3.3",
		"GATEWAY_TIMEOUT_MATCHING_ENGINE": "", // no Phase citation — resolvable via phase-plan token scan
		"SERVICE_DEGRADED":                "Phase-05 Task 5.3.29",
	}
	for code, wantOwner := range cases {
		d, ok := r.Lookup(code)
		if !ok {
			t.Fatalf("%s not registered", code)
		}
		if d.Owner != wantOwner {
			t.Errorf("%s owner %q, want %q", code, d.Owner, wantOwner)
		}
	}
}

func TestMarkers(t *testing.T) {
	r := New()
	if d, _ := r.Lookup("CORPORATE_ACTION_SCHEDULED"); !d.Reserved {
		t.Error("CORPORATE_ACTION_SCHEDULED must be marked reserved")
	}
	if d, _ := r.Lookup("ADDRESS_NOT_ALLOWLISTED"); d.SupersededBy != "BANK_ACCOUNT_NOT_VERIFIED" {
		t.Errorf("ADDRESS_NOT_ALLOWLISTED superseded_by %q, want BANK_ACCOUNT_NOT_VERIFIED", d.SupersededBy)
	}
	if d, _ := r.Lookup("MARKET_SLIPPAGE_EXCEEDED"); d.AliasOf != "SLIPPAGE_EXCEEDED" {
		t.Errorf("MARKET_SLIPPAGE_EXCEEDED alias_of %q, want SLIPPAGE_EXCEEDED", d.AliasOf)
	}
	// Alias + target must coexist (spec: clients treat them interchangeably).
	if _, ok := r.Lookup("SLIPPAGE_EXCEEDED"); !ok {
		t.Error("alias target SLIPPAGE_EXCEEDED missing")
	}
}

func TestEmissionGate(t *testing.T) {
	r := New()
	e := r.NewError("INSUFFICIENT_BALANCE", "nope")
	if e.Code != "INSUFFICIENT_BALANCE" {
		t.Fatalf("registered code rewritten: %s", e.Code)
	}
	bad := r.NewError("TOTALLY_MADE_UP", "x")
	if bad.Code != CodeInternalError {
		t.Fatalf("unregistered emission must degrade to INTERNAL_ERROR, got %s", bad.Code)
	}
	v := r.Violations()
	if len(v) != 1 || v[0] != "TOTALLY_MADE_UP" {
		t.Fatalf("violations %v, want [TOTALLY_MADE_UP]", v)
	}
	if err := r.ValidateEmissions(); err == nil {
		t.Fatal("ValidateEmissions must fail after an unregistered emission")
	}
	if got := r.Emitted(); len(got) != 2 {
		t.Fatalf("emitted %v", got)
	}
}

func TestCheckRegistered(t *testing.T) {
	r := New()
	if err := r.CheckRegistered("INVALID_REQUEST", "NOT_FOUND"); err != nil {
		t.Fatalf("registered codes rejected: %v", err)
	}
	if err := r.CheckRegistered("INVALID_REQUEST", "MISSING_CODE"); err == nil {
		t.Fatal("unregistered code accepted")
	}
}

func TestRegisterConflict(t *testing.T) {
	r := New()
	d := CodeDef{Code: "X_TEST", HTTPStatus: 400, Description: "test", Owner: "Phase-05 Task 5.3.7"}
	if err := r.Register(d); err != nil {
		t.Fatalf("register: %v", err)
	}
	if err := r.Register(d); err != nil {
		t.Fatalf("idempotent re-register: %v", err)
	}
	conflict := d
	conflict.HTTPStatus = 500
	if err := r.Register(conflict); err == nil {
		t.Fatal("conflicting re-registration accepted")
	}
	if err := r.Register(CodeDef{Code: "BAD", HTTPStatus: 99}); err == nil {
		t.Fatal("invalid status accepted")
	}
}

func TestUnresolvedOwners(t *testing.T) {
	r := New()
	un := r.UnresolvedOwners()
	// Codes with no Phase citation are listed for the CI phase-plan grep;
	// known examples: FORBIDDEN, RATE_LIMIT_TIER_EXCEEDED, STALE_MODIFY.
	found := map[string]bool{}
	for _, d := range un {
		found[d.Code] = true
	}
	for _, code := range []string{"FORBIDDEN", "RATE_LIMIT_TIER_EXCEEDED", "STALE_MODIFY"} {
		if !found[code] {
			t.Errorf("%s should be listed as owner-unresolved (no §23 citation)", code)
		}
	}
	// Reserved/cited codes must not appear.
	if found["CORPORATE_ACTION_SCHEDULED"] || found["INSUFFICIENT_BALANCE"] {
		t.Error("reserved or cited code listed as unresolved")
	}
}

func TestSeverityDelegation(t *testing.T) {
	r := New()
	if got := r.Severity("TIME_SYNC_LOSS_HALT"); got != pkgerrors.SeverityL0 {
		t.Errorf("TIME_SYNC_LOSS_HALT severity %v, want L0", got)
	}
	if got := r.Severity("INSUFFICIENT_BALANCE"); got != pkgerrors.SeverityL2 {
		t.Errorf("INSUFFICIENT_BALANCE severity %v, want L2 (default)", got)
	}
}

func TestAllSorted(t *testing.T) {
	all := New().All()
	for i := 1; i < len(all); i++ {
		if all[i-1].Code >= all[i].Code {
			t.Fatalf("All() not sorted at %d: %q >= %q", i, all[i-1].Code, all[i].Code)
		}
	}
}
