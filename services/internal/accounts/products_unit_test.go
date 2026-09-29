// Unit coverage for the Phase-14 product-governance value objects —
// no PostgreSQL needed (validation/canonicalization only).
package accounts

import (
	"testing"
	"time"
)

func TestProfileInputValidation(t *testing.T) {
	cases := []struct {
		name string
		in   ProfileInput
		ok   bool
	}{
		{"valid", ProfileInput{Code: "vip", PricingPlan: "raw_spread_commission",
			InstrumentScope: []string{"spot", "forward"}, SubunitDivisor: 1}, true},
		{"bad pricing plan", ProfileInput{Code: "x", PricingPlan: "FREE",
			InstrumentScope: []string{"SPOT"}}, false},
		{"empty scope", ProfileInput{Code: "x", PricingPlan: "SPREAD_MARKUP"}, false},
		{"unknown class", ProfileInput{Code: "x", PricingPlan: "SPREAD_MARKUP",
			InstrumentScope: []string{"SPOT", "FUTURES"}}, false},
		{"bad divisor", ProfileInput{Code: "x", PricingPlan: "SPREAD_MARKUP",
			InstrumentScope: []string{"SPOT"}, SubunitDivisor: 50}, false},
		{"negative deposit", ProfileInput{Code: "x", PricingPlan: "SPREAD_MARKUP",
			InstrumentScope: []string{"SPOT"}, MinDeposit: "-1"}, false},
		{"missing code", ProfileInput{PricingPlan: "SPREAD_MARKUP",
			InstrumentScope: []string{"SPOT"}}, false},
	}
	for _, tc := range cases {
		err := tc.in.validate(false)
		if tc.ok && err != nil {
			t.Fatalf("%s: unexpected rejection: %v", tc.name, err)
		}
		if !tc.ok && err == nil {
			t.Fatalf("%s: expected rejection, got nil", tc.name)
		}
	}
	// Canonicalization: lowercase/dupes fold to a sorted class set.
	in := ProfileInput{Code: "X", PricingPlan: "SPREAD_MARKUP",
		InstrumentScope: []string{"spot", "SPOT", "forward"}}
	if err := in.validate(false); err != nil {
		t.Fatalf("canonicalization: %v", err)
	}
	if len(in.InstrumentScope) != 2 || in.InstrumentScope[0] != "FORWARD" ||
		in.InstrumentScope[1] != "SPOT" {
		t.Fatalf("scope not canonicalized: %v", in.InstrumentScope)
	}
	if in.Code != "X" {
		t.Fatalf("code not upper-cased: %q", in.Code)
	}
	// Update path accepts the status vocabulary only.
	up := ProfileInput{PricingPlan: "SPREAD_MARKUP",
		InstrumentScope: []string{"SPOT"}, Status: "RETIRED"}
	if err := up.validate(true); err != nil {
		t.Fatalf("update validate: %v", err)
	}
	up.Status = "BROKEN"
	if err := up.validate(true); err == nil {
		t.Fatal("invalid status accepted on update")
	}
}

func TestTargetMarketInputValidation(t *testing.T) {
	now := time.Now().UTC()
	base := func() TargetMarketInput {
		return TargetMarketInput{
			ProfileID: 1, ClientCategory: "RETAIL",
			PositiveClasses: []string{"SPOT"},
			ReviewDueAt:     now.Add(30 * 24 * time.Hour).Format(time.RFC3339),
		}
	}
	valid := base()
	if err := valid.validate(now); err != nil {
		t.Fatalf("valid input rejected: %v", err)
	}
	// >12-month review horizon must reject.
	in := base()
	in.ReviewDueAt = now.Add(ReviewHorizon + time.Hour).Format(time.RFC3339)
	if err := in.validate(now); err == nil {
		t.Fatal("review_due_at > 12 months accepted")
	}
	// Past due date must reject.
	in = base()
	in.ReviewDueAt = now.Add(-time.Hour).Format(time.RFC3339)
	if err := in.validate(now); err == nil {
		t.Fatal("past review_due_at accepted")
	}
	// A class cannot be both positive and negative.
	in = base()
	in.NegativeClasses = []string{"SPOT"}
	if err := in.validate(now); err == nil {
		t.Fatal("positive∩negative overlap accepted")
	}
	// Unknown category must reject.
	in = base()
	in.ClientCategory = "ALIEN"
	if err := in.validate(now); err == nil {
		t.Fatal("unknown client_category accepted")
	}
}

func TestTargetMarketOverdue(t *testing.T) {
	now := time.Now()
	// APPROVED within the window admits.
	tm := &TargetMarket{Status: "APPROVED",
		ReviewDueAt: now.Add(time.Hour)}
	if tm.Overdue(now) {
		t.Fatal("fresh APPROVED row reported overdue")
	}
	// Swept REVIEW_OVERDUE always reports overdue regardless of due date.
	tm.Status = "REVIEW_OVERDUE"
	if !tm.Overdue(now) {
		t.Fatal("REVIEW_OVERDUE row not overdue")
	}
	// APPROVED but past the review instant reports overdue lazily (the
	// gate cannot admit on an unswept row).
	tm.Status = "APPROVED"
	tm.ReviewDueAt = now.Add(-time.Minute)
	if !tm.Overdue(now) {
		t.Fatal("elapsed review_due_at not overdue")
	}
}

func TestProductProfileInScope(t *testing.T) {
	p := &ProductProfile{InstrumentScope: []string{"SPOT", "NDF"}}
	if !p.InScope("spot") || !p.InScope("NDF") {
		t.Fatal("in-scope class rejected")
	}
	if p.InScope("OPTION") || p.InScope("") {
		t.Fatal("out-of-scope class admitted")
	}
}
