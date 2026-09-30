// Pure unit tests for the Task 21.3.15 venue-governance package —
// LEI/dossier validation, the admission gate's fail-closed posture and
// the case lifecycle transition map. DB-backed behavior lives in
// venue_pg_test.go (EXC_PG_TEST=1).
package venue

import (
	"context"
	"strconv"
	"strings"
	"testing"

	excerrors "exchange/pkg/errors"
)

// leiDigits computes the ISO 17442 mod-97 check digits for an 18-char
// body — mirrors the helper in internal/compliance tests.
func leiDigits(body18 string) string {
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

func validLEI() string {
	return "549300ABCDEFGH1234" + leiDigits("549300ABCDEFGH1234")
}

func TestMemberInputValidate(t *testing.T) {
	good := MemberInput{LegalName: "Acme Liquidity LP", LEI: validLEI(),
		AccessModel: AccessDEA}
	if err := good.validate(); err != nil {
		t.Fatalf("valid dossier rejected: %v", err)
	}
	// LEI is normalized (upper+trim) before checksum — lowercase input
	// carrying valid digits passes.
	lc := good
	lc.LEI = strings.ToLower(validLEI())
	if err := lc.validate(); err != nil {
		t.Fatalf("lowercase LEI must normalize+pass: %v", err)
	}
	// Corrupted check digit must fail — the register never stores a
	// malformed LEI.
	badLEI := good
	badLEI.LEI = "549300ABCDEFGH1234" + "00"
	if leiDigits("549300ABCDEFGH1234") != "00" {
		if err := badLEI.validate(); err == nil {
			t.Fatal("bad LEI check digits must reject")
		}
	}
	noName := good
	noName.LegalName = "  "
	if err := noName.validate(); excerrors.CodeOf(err) != "INVALID_REQUEST" {
		t.Fatalf("missing legal_name: %v", err)
	}
	badModel := good
	badModel.AccessModel = "RETAIL"
	if err := badModel.validate(); excerrors.CodeOf(err) != "INVALID_REQUEST" {
		t.Fatalf("bad access_model: %v", err)
	}
}

// The admission gate fails closed SERVICE_DEGRADED when the member
// service is unwired — an order can never slip past a missing
// dependency (spec §2.7 zero-loss pessimism).
func TestAdmissionGate_NilServiceFailsClosed(t *testing.T) {
	g := &AdmissionGate{}
	err := g.AdmitOrder(context.Background(), 7, "EURUSD", "SPOT", false)
	if excerrors.CodeOf(err) != "SERVICE_DEGRADED" {
		t.Fatalf("nil member service must fail closed, got %v", err)
	}
}

// The case lifecycle map is the §14.1b disciplinary workflow: every
// status resolves, terminal states (SANCTIONED/DISMISSED/CLOSED) have
// no onward path except the documented closes, and nothing transitions
// back to OPEN.
func TestCaseTransitions_LifecycleShape(t *testing.T) {
	if len(caseTransitions[CaseStatusClosed]) != 0 {
		t.Fatal("CLOSED must be terminal")
	}
	allowed := func(from, to string) bool {
		for _, v := range caseTransitions[from] {
			if v == to {
				return true
			}
		}
		return false
	}
	if !allowed(CaseStatusOpen, CaseStatusInvestigating) {
		t.Fatal("OPEN → INVESTIGATING must be legal")
	}
	if allowed(CaseStatusOpen, CaseStatusSanctioned) {
		t.Fatal("OPEN → SANCTIONED skips the investigation/charge path")
	}
	for from, tos := range caseTransitions {
		for _, to := range tos {
			if to == CaseStatusOpen {
				t.Fatalf("%s → OPEN reopen not permitted", from)
			}
		}
	}
	if !allowed(CaseStatusSanctioned, CaseStatusClosed) ||
		!allowed(CaseStatusDismissed, CaseStatusClosed) {
		t.Fatal("sanctioned/dismissed cases must close")
	}
}
