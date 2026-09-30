// Phase-21 Tasks 21.3.2/21.3.3/21.3.6 — pure-helper unit tests (no PG).
package compliance

import (
	"context"
	"testing"
	"time"

	"github.com/shopspring/decimal"
)

func decPtr(s string) *decimal.Decimal {
	d := decimal.RequireFromString(s)
	return &d
}

// The FATF floor is INCLUSIVE ($1,000.00 applies); USD-equivalent wins
// over the native amount, and an unpriced non-USD leg is pessimistically
// in scope (§2.7 — never skip the gate for an unconvertible amount).
func TestTravelRuleInScope(t *testing.T) {
	cases := []struct {
		name string
		amt  string
		ccy  string
		usd  *decimal.Decimal
		want bool
	}{
		{"usd just below", "999.99", "USD", nil, false},
		{"usd at floor", "1000", "USD", nil, true},
		{"usd above", "1000.01", "USD", nil, true},
		{"usd exact via usd_amount", "500", "EUR", decPtr("1000"), true},
		{"eur below equivalent", "900", "EUR", decPtr("900"), false},
		{"eur unpriced pessimistic", "900", "EUR", nil, true},
		{"usd amount overrides native", "500", "USD", decPtr("400"), false},
		{"zero usd falls to native", "1200", "USD", decPtr("0"), true},
	}
	for _, c := range cases {
		got := inScope(decimal.RequireFromString(c.amt), c.ccy, c.usd)
		if got != c.want {
			t.Fatalf("%s: inScope=%v want %v", c.name, got, c.want)
		}
	}
}

// Originator mandates name+account_number+address; beneficiary mandates
// name+account_number (FATF R.16 required set).
func TestTravelRuleMissingFields(t *testing.T) {
	m := missingFields(TravelRuleParty{}, TravelRuleParty{})
	if len(m) != 5 {
		t.Fatalf("empty parties must list 5 missing fields, got %v", m)
	}
	m = missingFields(
		TravelRuleParty{Name: "Alice", AccountNumber: "acct-1",
			Address: "1 Main St"},
		TravelRuleParty{Name: "Bob", AccountNumber: "DE89"})
	if len(m) != 0 {
		t.Fatalf("complete parties must report none, got %v", m)
	}
	// Whitespace counts as absent.
	m = missingFields(
		TravelRuleParty{Name: "  ", AccountNumber: "a", Address: "x"},
		TravelRuleParty{Name: "Bob", AccountNumber: " "})
	want := map[string]bool{"originator.name": true,
		"beneficiary.account_number": true}
	if len(m) != 2 {
		t.Fatalf("whitespace must be missing, got %v", m)
	}
	for _, f := range m {
		if !want[f] {
			t.Fatalf("unexpected missing field %s", f)
		}
	}
}

// Officer-supplied (stored) fields always win over re-resolution —
// a dispatch-sweep re-run must never clobber the cured data.
func TestTravelRuleMergePartyStoredWins(t *testing.T) {
	stored := TravelRuleParty{Name: "Officer Name", AccountNumber: "S-1"}
	fresh := TravelRuleParty{Name: "Resolver Name", AccountNumber: "R-1",
		Address: "new addr", Country: "DE"}
	out := mergeParty(stored, fresh)
	if out.Name != "Officer Name" || out.AccountNumber != "S-1" {
		t.Fatalf("stored fields must win: %+v", out)
	}
	if out.Address != "new addr" || out.Country != "DE" {
		t.Fatalf("absent stored fields take fresh: %+v", out)
	}
	// Blank stored values do NOT win.
	out = mergeParty(TravelRuleParty{Name: "  "}, fresh)
	if out.Name != "Resolver Name" {
		t.Fatalf("blank stored name must not win: %+v", out)
	}
}

func TestSwiftFieldRef(t *testing.T) {
	if got := swiftFieldRef("SWIFT"); got != "MT103:50K/59" {
		t.Fatalf("swift ref=%q", got)
	}
	if got := swiftFieldRef(""); got != "MT103:50K/59" {
		t.Fatalf("empty rail defaults to MT103, got %q", got)
	}
	if got := swiftFieldRef("SEPA"); got != "pacs.008:Dbtr/Cdtr" {
		t.Fatalf("sepa ref=%q", got)
	}
}

func TestIsoWeekKey(t *testing.T) {
	// 2026-01-01 is ISO week 2026-W01.
	got := isoWeekKey(time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC))
	if got != "2026-W01" {
		t.Fatalf("isoWeekKey=%q", got)
	}
}

// Draft validation rejects before touching the pool — safe to drive
// with a nil-pool service for the INVALID_REQUEST paths.
func TestSARDraftValidation(t *testing.T) {
	s := &SARService{newToken: defaultSourceToken, now: time.Now}
	ctx := context.Background()
	acct := int64(7)

	if _, _, err := s.Draft(ctx, SARDraftInput{
		TriggerType: "BOGUS", AccountID: &acct, Description: "x"}); err == nil ||
		codeOf(t, err) != "INVALID_REQUEST" {
		t.Fatalf("bad trigger: got %v", err)
	}
	if _, _, err := s.Draft(ctx, SARDraftInput{
		TriggerType: SARTriggerManual, AccountID: &acct}); err == nil ||
		codeOf(t, err) != "INVALID_REQUEST" {
		t.Fatalf("empty description: got %v", err)
	}
	if _, _, err := s.Draft(ctx, SARDraftInput{
		TriggerType: SARTriggerManual, Description: "x"}); err == nil ||
		codeOf(t, err) != "INVALID_REQUEST" {
		t.Fatalf("no subject: got %v", err)
	}
	// subject_ref alone is a legal subject anchor.
	long := make([]byte, 161)
	for i := range long {
		long[i] = 'x'
	}
	if _, _, err := s.Draft(ctx, SARDraftInput{
		TriggerType: SARTriggerManual, SubjectRef: "acct:9",
		Description: "x", SourceRef: string(long)}); err == nil ||
		codeOf(t, err) != "INVALID_REQUEST" {
		t.Fatalf("oversize source_ref: got %v", err)
	}
}
