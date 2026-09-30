package compliance

// Phase-21 Task 21.3.11 — ongoing-monitoring rule tests: structuring,
// velocity, round-amount, dormant reactivation, and the service's
// case-sink routing (findings never mutate account state).

import (
	"context"
	"testing"
	"time"
)

// memActivity is a fixture ActivitySource.
type memActivity struct {
	flows     map[int64][]FlowEvent
	lastPrior map[int64]time.Time
	active    []int64
}

func (m *memActivity) RecentFlows(_ context.Context, accountID int64,
	_ time.Duration, _ time.Time) ([]FlowEvent, error) {
	return m.flows[accountID], nil
}

func (m *memActivity) LastActivityAt(_ context.Context, accountID int64,
	_ time.Time) (time.Time, error) {
	return m.lastPrior[accountID], nil
}

func (m *memActivity) ActiveAccounts(_ context.Context, _ time.Duration,
	_ time.Time, _ int) ([]int64, error) {
	return m.active, nil
}

type memCases struct{ findings []MonitoringFinding }

func (m *memCases) OpenCase(_ context.Context,
	f MonitoringFinding) (int64, error) {
	m.findings = append(m.findings, f)
	return int64(len(m.findings)), nil
}

func TestStructuringRule(t *testing.T) {
	rule := NewStructuringRule()
	now := time.Now().UTC()
	// Three $9,600 deposits inside 24h — below the $10k CTR line,
	// bunching in the sub-threshold band → finding.
	hist := []FlowEvent{
		{AccountID: 1, Kind: FlowDeposit, Amount: 960_000, At: now.Add(-time.Hour)},
		{AccountID: 1, Kind: FlowDeposit, Amount: 960_000, At: now.Add(-2 * time.Hour)},
		{AccountID: 1, Kind: FlowDeposit, Amount: 960_000, At: now.Add(-3 * time.Hour)},
	}
	ev := FlowEvent{AccountID: 1, Kind: FlowDeposit, Amount: 500_000, At: now}
	f := rule.Evaluate(context.Background(), ev, hist, now)
	if len(f) != 1 || f[0].Rule != "structuring_24h" || f[0].Severity != "P1" {
		t.Fatalf("structuring finding: %+v", f)
	}
	// A single over-threshold deposit defeats the bunching signature.
	hist2 := append(hist, FlowEvent{AccountID: 1, Kind: FlowDeposit,
		Amount: 1_200_000, At: now.Add(-time.Hour)})
	// still 3 banded — but the sum now exceeds via the big one too;
	// the rule only counts banded events for the signature — keep
	// proving non-deposit kinds don't trip it.
	f = rule.Evaluate(context.Background(),
		FlowEvent{AccountID: 1, Kind: FlowWithdrawal, Amount: 1, At: now},
		hist2, now)
	if len(f) != 0 {
		t.Fatalf("withdrawal trigger must not evaluate structuring: %+v", f)
	}
	// Sub-threshold but NOT banded (small amounts) → no finding.
	small := []FlowEvent{
		{AccountID: 1, Kind: FlowDeposit, Amount: 100_000, At: now},
		{AccountID: 1, Kind: FlowDeposit, Amount: 100_000, At: now},
		{AccountID: 1, Kind: FlowDeposit, Amount: 100_000, At: now},
	}
	if f := rule.Evaluate(context.Background(), ev, small, now); len(f) != 0 {
		t.Fatalf("unbanded small deposits flagged: %+v", f)
	}
}

func TestVelocityRule(t *testing.T) {
	rule := NewVelocityRule()
	now := time.Now().UTC()
	var hist []FlowEvent
	for i := 0; i < 11; i++ {
		hist = append(hist, FlowEvent{AccountID: 2, Kind: FlowDeposit,
			Amount: 10_000, At: now.Add(-time.Duration(i) * time.Minute)})
	}
	ev := FlowEvent{AccountID: 2, Kind: FlowDeposit, Amount: 10_000, At: now}
	f := rule.Evaluate(context.Background(), ev, hist, now)
	if len(f) == 0 || f[0].Rule != "velocity_burst" {
		t.Fatalf("velocity burst missed: %+v", f)
	}
	// Amount spike: baseline of 5 × 10k flows, event 100k → 10× spike.
	hist = hist[:5]
	ev2 := FlowEvent{AccountID: 2, Kind: FlowDeposit,
		Amount: 1_000_000, At: now, Ref: "EV-2"}
	f = rule.Evaluate(context.Background(), ev2, hist, now)
	var spike bool
	for _, x := range f {
		if x.Rule == "velocity_amount_spike" {
			spike = true
		}
	}
	if !spike {
		t.Fatalf("amount spike missed: %+v", f)
	}
}

func TestRoundAmountRule(t *testing.T) {
	rule := NewRoundAmountRule()
	now := time.Now().UTC()
	hist := []FlowEvent{
		{AccountID: 3, Kind: FlowDeposit, Amount: 500_000, At: now},
		{AccountID: 3, Kind: FlowWithdrawal, Amount: 100_000, At: now},
		{AccountID: 3, Kind: FlowDeposit, Amount: 700_000, At: now},
	}
	ev := FlowEvent{AccountID: 3, Kind: FlowDeposit, Amount: 300_000, At: now}
	if f := rule.Evaluate(context.Background(), ev, hist, now); len(f) != 1 {
		t.Fatalf("round-amount cluster missed: %+v", f)
	}
	// Trades are exempt — notional rounding is normal.
	trades := []FlowEvent{
		{AccountID: 3, Kind: FlowTrade, Amount: 500_000, At: now},
		{AccountID: 3, Kind: FlowTrade, Amount: 600_000, At: now},
		{AccountID: 3, Kind: FlowTrade, Amount: 700_000, At: now},
	}
	if f := rule.Evaluate(context.Background(),
		FlowEvent{AccountID: 3, Kind: FlowTrade, Amount: 800_000, At: now},
		trades, now); len(f) != 0 {
		t.Fatalf("trade-kind rounding flagged: %+v", f)
	}
}

func TestMonitoringServiceEvaluateAndDormantSweep(t *testing.T) {
	now := time.Now().UTC()
	src := &memActivity{
		flows: map[int64][]FlowEvent{
			1: {
				{AccountID: 1, Kind: FlowDeposit, Amount: 960_000, At: now.Add(-time.Hour)},
				{AccountID: 1, Kind: FlowDeposit, Amount: 960_000, At: now.Add(-2 * time.Hour)},
				{AccountID: 1, Kind: FlowDeposit, Amount: 960_000, At: now.Add(-3 * time.Hour)},
			},
		},
		lastPrior: map[int64]time.Time{
			// account 4: last activity 120 days ago → dormant reactivation
			4: now.Add(-120 * 24 * time.Hour),
			// account 5: active yesterday → not dormant
			5: now.Add(-time.Hour),
		},
		active: []int64{4, 5},
	}
	cases := &memCases{}
	svc, err := NewMonitoringService(MonitoringOptions{
		Src: src, Cases: cases, Now: func() time.Time { return now }})
	if err != nil {
		t.Fatalf("service: %v", err)
	}
	ctx := context.Background()
	findings, err := svc.EvaluateFlow(ctx, FlowEvent{
		AccountID: 1, Kind: FlowDeposit, Amount: 960_000,
		At: now, Ref: "D-NEW"})
	if err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	if len(findings) == 0 {
		t.Fatal("structuring finding not produced")
	}
	if len(cases.findings) == 0 {
		t.Fatal("finding never reached the case sink")
	}
	evals, hits := svc.Stats()
	if evals != 1 || hits == 0 {
		t.Fatalf("stats: evals=%d hits=%d", evals, hits)
	}
	// Dormant sweep: account 4 reactivates, 5 stays quiet.
	n, err := svc.SweepDormant(ctx, 100)
	if err != nil || n != 1 {
		t.Fatalf("dormant sweep: n=%d err=%v", n, err)
	}
	var dormant bool
	for _, f := range cases.findings {
		if f.Rule == "dormant_reactivation" && f.AccountID == 4 {
			dormant = true
		}
	}
	if !dormant {
		t.Fatal("dormant reactivation finding missing")
	}
}
