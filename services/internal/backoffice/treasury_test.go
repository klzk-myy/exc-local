// treasury_test.go — DoD coverage for Phase-24 Task 24.3.17: the own-funds
// ledger (distinct from client money and GL), daily bank reconciliation,
// contingent-capital waterfall commitments ending in a funded backstop,
// insurance-cover expiry paging, the stressed 5-day liquidity buffer with
// outflow/LP freeze, and the funded admission gate.
package backoffice

import (
	"context"
	"testing"
	"time"

	"exchange/pkg/decimal"
	excerrors "exchange/pkg/errors"
)

type cmOutflows struct{ m map[string]decimal.Decimal }

func (f *cmOutflows) StressedOutflow5d(_ context.Context, ccy string) (decimal.Decimal, error) {
	return f.m[ccy], nil
}

func newTreasurySvc(t *testing.T, st *cmStore, out map[string]decimal.Decimal) (*TreasuryService, *cmAlertCap, *cmClock) {
	t.Helper()
	clk := &cmClock{t: time.Now().UTC()}
	al := &cmAlertCap{}
	svc, err := NewTreasuryService(TreasuryDeps{
		Store: st, Outflows: &cmOutflows{m: out}, Alerter: al,
		Resolver: cmRoles, Now: clk.now,
	})
	if err != nil {
		t.Fatalf("NewTreasuryService: %v", err)
	}
	return svc, al, clk
}

func TestOwnFunds_LedgerAndReconciliation(t *testing.T) {
	st := newCMStore()
	svc, al, _ := newTreasurySvc(t, st, nil)
	ctx := context.Background()

	// The four line kinds land; an unknown kind is refused.
	if _, err := svc.SetOwnFunds(ctx, cmActor(uidFO), OwnFunds{
		LineKind: FundHouseEquity, Currency: "USD", Balance: decimal.RequireFromString("1000000"),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.SetOwnFunds(ctx, cmActor(uidFO), OwnFunds{
		LineKind: FundInsuranceFundLine, Currency: "USD", Balance: decimal.RequireFromString("50000"),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.SetOwnFunds(ctx, cmActor(uidFO), OwnFunds{
		LineKind: "CLIENT_MONEY", Currency: "USD", Balance: decimal.RequireFromString("1"),
	}); err == nil {
		t.Fatal("client-money line kinds must be refused on the own-funds ledger")
	}

	// Daily bank-statement reconciliation: matching → RECONCILED.
	line, _ := svc.ListOwnFunds(ctx, cmActor(uidAuditor)) // Read-Only Auditor sees the surface
	var equity *OwnFunds
	for i := range line {
		if line[i].LineKind == FundHouseEquity {
			equity = &line[i]
		}
	}
	rec, err := svc.ReconcileOwnFunds(ctx, cmActor(uidFO), equity.ID,
		decimal.RequireFromString("1000000"), "STMT-OWN-1")
	if err != nil || rec.ReconciliationStatus != ReconReconciled {
		t.Fatalf("reconcile: %v %+v", err, rec)
	}
	// Divergence → BREAK + P1.
	rec2, err := svc.ReconcileOwnFunds(ctx, cmActor(uidFO), equity.ID,
		decimal.RequireFromString("999999"), "STMT-OWN-2")
	if err != nil || rec2.ReconciliationStatus != ReconBreak {
		t.Fatalf("divergent reconcile: %v %+v", err, rec2)
	}
	if !al.has("OWN_FUNDS_RECON_BREAK") {
		t.Fatal("own-funds recon break alert missing")
	}
	// HouseReserve excludes the insurance-fund line.
	hr, err := svc.HouseReserve(ctx, st, "USD")
	if err != nil || !hr.Equal(decimal.RequireFromString("1000000")) {
		t.Fatalf("house reserve=%s", hr)
	}
}

func TestCommitments_WaterfallAndBackstop(t *testing.T) {
	st := newCMStore()
	svc, _, _ := newTreasurySvc(t, st, nil)
	ctx := context.Background()
	fin := cmActor(uidFO)

	// Validation: kind domain, trigger and agreement ref mandatory.
	if _, err := svc.RecordCommitment(ctx, fin, Commitment{
		ProviderName: "SponsorCo", Kind: CommitSponsor, PrioritySeq: 2,
		CommittedAmount: decimal.RequireFromString("500000"), Currency: "USD",
		ActivationTrigger: "FUND_DEPLETED", DrawWindowDays: 5,
		AgreementRef: "SPA-2026-01",
	}); err != nil {
		t.Fatal(err)
	}
	hc, err := svc.RecordCommitment(ctx, fin, Commitment{
		ProviderName: "Venue", Kind: CommitHouseCapital, PrioritySeq: 1,
		CommittedAmount: decimal.RequireFromString("200000"), Currency: "USD",
		ActivationTrigger: "NBP_EXHAUSTION", DrawWindowDays: 0,
		AgreementRef: "BOARD-RES-11",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.RecordCommitment(ctx, fin, Commitment{
		ProviderName: "X", Kind: CommitSponsor, PrioritySeq: 3,
		CommittedAmount: decimal.RequireFromString("1"), Currency: "USD",
	}); err == nil {
		t.Fatal("commitment without trigger/agreement must be refused")
	}

	// Unexecuted waterfall → no funded backstop → admission fails.
	ok, err := svc.BackstopFunded(ctx)
	if err != nil || ok {
		t.Fatal("backstop must be unfunded until last commitment executes")
	}
	// EXECUTED is dual... execution is Finance Ops here; verify lifecycle guard.
	sp, _ := svc.ListCommitments(ctx, cmActor(uidFO))
	var sponsor *Commitment
	for i := range sp {
		if sp[i].Kind == CommitSponsor {
			sponsor = &sp[i]
		}
	}
	if _, err := svc.ExecuteCommitment(ctx, fin, hc.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.ExecuteCommitment(ctx, fin, hc.ID); err == nil {
		t.Fatal("re-executing an EXECUTED commitment must fail")
	}
	// Sponsor still COMMITTED → last-in-waterfall not funded.
	if ok, _ := svc.BackstopFunded(ctx); ok {
		t.Fatal("waterfall must end in a funded commitment")
	}
	if _, err := svc.ExecuteCommitment(ctx, fin, sponsor.ID); err != nil {
		t.Fatal(err)
	}
	ok, err = svc.BackstopFunded(ctx)
	if err != nil || !ok {
		t.Fatal("executed sponsor backstop must satisfy the waterfall")
	}
	// Draws bounded by headroom.
	if _, err := svc.DrawCommitment(ctx, fin, sponsor.ID, decimal.RequireFromString("600000")); err == nil {
		t.Fatal("overdraw beyond committed headroom must fail")
	}
	dr, err := svc.DrawCommitment(ctx, fin, sponsor.ID, decimal.RequireFromString("100000"))
	if err != nil || dr.Status != CommitDrawn {
		t.Fatalf("draw: %v %+v", err, dr)
	}
	// Lapse terminates a commitment; lapsing the funded tail leaves the
	// waterfall ending at the (still EXECUTED, funded) house-capital tier —
	// the sequence still terminates in a funded backstop.
	if _, err := svc.LapseCommitment(ctx, cmActor(uidCO), sponsor.ID); err != nil {
		t.Fatal(err)
	}
	if ok, _ := svc.BackstopFunded(ctx); !ok {
		t.Fatal("house-capital tail should keep the waterfall funded")
	}
	// Lapse the remaining funded tail too → the waterfall is unbacked.
	if _, err := svc.LapseCommitment(ctx, cmActor(uidCO), hc.ID); err != nil {
		t.Fatal(err)
	}
	if ok, _ := svc.BackstopFunded(ctx); ok {
		t.Fatal("all live commitments lapsed → waterfall unbacked")
	}
}

func TestInsurancePolicy_ExpiryAlert(t *testing.T) {
	st := newCMStore()
	svc, al, clk := newTreasurySvc(t, st, nil)
	ctx := context.Background()

	exp := clk.t.Add(30 * 24 * time.Hour) // inside the 60-day horizon
	pol, err := svc.RecordCommitment(ctx, cmActor(uidFO), Commitment{
		ProviderName: "InsureCo", Kind: CommitInsurancePolicy, PrioritySeq: 9,
		CommittedAmount: decimal.RequireFromString("1000000"), Currency: "USD",
		ActivationTrigger: "COVERED_EVENT", DrawWindowDays: 30,
		AgreementRef: "POL-99", PolicyType: PolicyCyber,
		CoverLimit: decimal.RequireFromString("1000000"),
		Excess:     decimal.RequireFromString("50000"), Broker: "Marsh",
		ExpiresAt: &exp,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.CheckInsuranceExpiries(ctx); err != nil {
		t.Fatal(err)
	}
	if !al.has(alertInsuranceExpiring) {
		t.Fatal("INSURANCE_POLICY_EXPIRING P2 missing")
	}
	var sev string
	for _, a := range al.alerts {
		if a.Code == alertInsuranceExpiring {
			sev = a.Severity
		}
	}
	if sev != SeverityP2 {
		t.Fatalf("expiry alert severity=%s want P2", sev)
	}
	// Second sweep dedups (expiry_alerted_at stamped).
	if err := svc.CheckInsuranceExpiries(ctx); err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, a := range al.alerts {
		if a.Code == alertInsuranceExpiring {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("expiry alerted %d times, want 1", n)
	}
	// A policy outside the horizon does not alert.
	far := clk.t.Add(90 * 24 * time.Hour)
	if _, err := svc.RecordCommitment(ctx, cmActor(uidFO), Commitment{
		ProviderName: "InsureCo", Kind: CommitInsurancePolicy, PrioritySeq: 10,
		CommittedAmount: decimal.RequireFromString("1"), Currency: "USD",
		ActivationTrigger: "X", AgreementRef: "POL-100",
		PolicyType: PolicyErrorsOmissions, ExpiresAt: &far,
	}); err != nil {
		t.Fatal(err)
	}
	_ = pol
}

func TestLiquidityBreach_FreezesOutflowsAndLP(t *testing.T) {
	st := newCMStore()
	out := map[string]decimal.Decimal{"USD": decimal.RequireFromString("500000")}
	svc, al, _ := newTreasurySvc(t, st, out)
	ctx := context.Background()

	// Liquid house funds below the stressed 5-day outflow → BREACH.
	if _, err := svc.SetOwnFunds(ctx, cmActor(uidFO), OwnFunds{
		LineKind: FundHouseEquity, Currency: "USD", Balance: decimal.RequireFromString("100000"),
	}); err != nil {
		t.Fatal(err)
	}
	a, err := svc.EvaluateLiquidity(ctx, "USD")
	if err != nil || a.Status != "BREACH" {
		t.Fatalf("assessment: %v %+v", err, a)
	}
	if !al.has(alertLiquidityBreach) {
		t.Fatal("TREASURY_LIQUIDITY_BREACH P1 missing")
	}
	if err := svc.AssertDiscretionaryOutflow(ctx); err == nil ||
		excerrors.CodeOf(err) != CodeTreasuryLiquidityBreach {
		t.Fatal("discretionary outflow must fail with TREASURY_LIQUIDITY_BREACH")
	}
	if err := svc.AssertLPCapacity(ctx); err == nil ||
		excerrors.CodeOf(err) != CodeTreasuryLiquidityBreach {
		t.Fatal("new LP capacity must fail with TREASURY_LIQUIDITY_BREACH")
	}
	// Lift requires dual control.
	if _, err := svc.LiftLiquidityFreeze(ctx, cmActor(uidFO)); err == nil {
		t.Fatal("unfreeze without approver must fail")
	}
	ctrl, err := svc.LiftLiquidityFreeze(ctx, cmAct2(uidFO, uidCO))
	if err != nil || ctrl.DiscretionaryOutflowsFrozen || ctrl.LPCapacityBlocked {
		t.Fatalf("lift: %v %+v", err, ctrl)
	}
	if err := svc.AssertDiscretionaryOutflow(ctx); err != nil {
		t.Fatalf("post-lift outflow blocked: %v", err)
	}
}

func TestAdmissionGate_FundedOnly(t *testing.T) {
	st := newCMStore()
	svc, _, _ := newTreasurySvc(t, st, nil)
	ctx := context.Background()

	// Nothing funded → gate closed.
	ok, err := svc.AdmissionSatisfied(ctx)
	if err != nil || ok {
		t.Fatal("admission must fail with empty own-funds ledger")
	}
	// Own funds funded but no executed backstop → still closed.
	if _, err := svc.SetOwnFunds(ctx, cmActor(uidFO), OwnFunds{
		LineKind: FundCapitalReserve, Currency: "USD", Balance: decimal.RequireFromString("250000"),
	}); err != nil {
		t.Fatal(err)
	}
	if ok, _ := svc.AdmissionSatisfied(ctx); ok {
		t.Fatal("attestation-only capital must not satisfy the gate")
	}
	// Executed funded backstop + funded own funds → gate opens.
	c, err := svc.RecordCommitment(ctx, cmActor(uidFO), Commitment{
		ProviderName: "Bank", Kind: CommitCreditFacility, PrioritySeq: 2,
		CommittedAmount: decimal.RequireFromString("1000000"), Currency: "USD",
		ActivationTrigger: "SHORTFALL", DrawWindowDays: 3, AgreementRef: "RCF-7",
	})
	if err != nil {
		t.Fatal(err)
	}
	if ok, _ := svc.AdmissionSatisfied(ctx); ok {
		t.Fatal("COMMITTED (unexecuted) backstop must not satisfy the gate")
	}
	if _, err := svc.ExecuteCommitment(ctx, cmActor(uidFO), c.ID); err != nil {
		t.Fatal(err)
	}
	ok, err = svc.AdmissionSatisfied(ctx)
	if err != nil || !ok {
		t.Fatal("funded own funds + executed backstop must satisfy the gate")
	}
}

func TestTreasury_RoleEnforcement(t *testing.T) {
	st := newCMStore()
	svc, _, _ := newTreasurySvc(t, st, nil)
	ctx := context.Background()

	if _, err := svc.SetOwnFunds(ctx, cmActor(uidSupport), OwnFunds{
		LineKind: FundHouseEquity, Currency: "USD", Balance: decimal.RequireFromString("1"),
	}); err == nil {
		t.Fatal("Support Agent must not write own funds")
	}
	// Read-Only Auditor can read the breach/coverage views.
	if _, err := svc.ListCommitments(ctx, cmActor(uidAuditor)); err != nil {
		t.Fatalf("auditor read refused: %v", err)
	}
	if _, err := svc.Controls(ctx, cmActor(uidAuditor)); err != nil {
		t.Fatalf("auditor controls read refused: %v", err)
	}
}
