// Unit tests for the Phase-11 Task 11.3.9 fee engine + admin service.
// PostgreSQL coverage lives in fee_schedule_integration_test.go
// (EXC_PG_TEST=1); these exercise the pure resolution/charge semantics
// against in-memory fakes.
package funding

import (
	"context"
	"fmt"
	"sort"
	"testing"
	"time"

	"exchange/internal/ledger"
	"exchange/pkg/decimal"
)

// fakeFeeStore implements FeeScheduleStore with the documented
// resolution semantics: exact (currency, tier) rows beat '*' wildcards
// per key, then newest effective_date ≤ at among non-retired rows.
type fakeFeeStore struct {
	tiers []FundingFeeTier
	usage map[string]int
	err   error
}

func (f *fakeFeeStore) FeeTierAt(_ context.Context, rail, ccy, dir, tier string,
	at time.Time) (*FundingFeeTier, error) {
	if f.err != nil {
		return nil, f.err
	}
	var cand []FundingFeeTier
	for _, t := range f.tiers {
		if t.Rail != rail || t.Direction != dir {
			continue
		}
		if t.Currency != ccy && t.Currency != FeeTierWildcard {
			continue
		}
		if t.AccountTier != tier && t.AccountTier != FeeTierWildcard {
			continue
		}
		if t.RetiredAt != nil || t.EffectiveDate.After(at) {
			continue
		}
		cand = append(cand, t)
	}
	if len(cand) == 0 {
		return nil, nil
	}
	sort.Slice(cand, func(i, j int) bool {
		a, b := cand[i], cand[j]
		if (a.Currency == ccy) != (b.Currency == ccy) {
			return a.Currency == ccy // exact currency beats '*'
		}
		if (a.AccountTier == tier) != (b.AccountTier == tier) {
			return a.AccountTier == tier
		}
		if !a.EffectiveDate.Equal(b.EffectiveDate) {
			return a.EffectiveDate.After(b.EffectiveDate)
		}
		return a.Version > b.Version
	})
	t := cand[0]
	return &t, nil
}

func (f *fakeFeeStore) FreeUsageCount(_ context.Context, acct int64, dir string,
	month time.Time) (int, error) {
	if f.err != nil {
		return 0, f.err
	}
	return f.usage[fmt.Sprintf("%d|%s|%s", acct, dir, month.Format("2006-01"))], nil
}

func (f *fakeFeeStore) RecordFreeUsage(_ context.Context, acct int64, dir string,
	month time.Time) error {
	if f.err != nil {
		return f.err
	}
	k := fmt.Sprintf("%d|%s|%s", acct, dir, month.Format("2006-01"))
	f.usage[k]++
	return nil
}

type fakeAccounts struct{ metas map[int64]*AccountMeta }

func (f fakeAccounts) AccountMeta(_ context.Context, id int64) (*AccountMeta, error) {
	m, ok := f.metas[id]
	if !ok {
		return nil, errf("NOT_FOUND", "account %d not found", id)
	}
	return m, nil
}

// codeOfT is the strict variant of the package-shared codeOf — a missing
// code is a test failure, not a silent "".
func codeOfT(t *testing.T, err error) string {
	t.Helper()
	c := codeOf(err)
	if c == "" {
		t.Fatalf("expected coded error, got %v", err)
	}
	return c
}

func tier(flat, bps, minF, maxF string, free int) FundingFeeTier {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	t := FundingFeeTier{
		ID: 1, Rail: "SWIFT", Currency: "USD", Direction: "WITHDRAWAL",
		AccountTier: "T2",
		FlatFee:     decimal.MustFromString(flat), PercentageBps: decimal.MustFromString(bps),
		MinFee:               decimal.MustFromString(minF),
		FreeTierMonthlyCount: free, EffectiveDate: now, Version: 1,
	}
	if maxF != "" {
		m := decimal.MustFromString(maxF)
		t.MaxFee = &m
	}
	return t
}

// TestFeeTierFormula covers the flat+pct + min/max clamp arithmetic.
func TestFeeTierFormula(t *testing.T) {
	// flat 5 + 10bps of 10000 → 5 + 10 = 15 (within [0, uncapped]).
	tr := tier("5", "10", "0", "", 0)
	if got := tr.Fee(decimal.NewFromInt(10_000)); got.String() != "15" {
		t.Fatalf("fee=%s want 15", got)
	}
	// min_fee floor: flat 0 + 1bps of 100 → 0.01 → clamped to 1.50.
	tr = tier("0", "1", "1.5", "", 0)
	if got := tr.Fee(decimal.NewFromInt(100)); got.String() != "1.5" {
		t.Fatalf("min clamp fee=%s want 1.5", got)
	}
	// max_fee cap: flat 0 + 500bps of 1,000,000 → 50000 → capped 25.
	tr = tier("0", "500", "0", "25", 0)
	if got := tr.Fee(decimal.NewFromInt(1_000_000)); got.String() != "25" {
		t.Fatalf("max cap fee=%s want 25", got)
	}
}

func feeSvc(t *testing.T, tiers []FundingFeeTier, used int, poster *fakePoster) *FeeService {
	st := &fakeFeeStore{tiers: tiers, usage: map[string]int{}}
	now := time.Now().UTC()
	if used > 0 {
		st.usage[fmt.Sprintf("1|WITHDRAWAL|%s", monthStart(now).Format("2006-01"))] = used
	}
	var jp JournalPoster // keep a nil interface when poster is nil —
	if poster != nil {   // a typed-nil *fakePoster would defeat the
		jp = poster // fail-closed poster==nil check.
	}
	svc, err := NewFeeService(st, fakeAccounts{metas: map[int64]*AccountMeta{
		1: {ID: 1, UserID: 100, Status: "ACTIVE", KYCTier: "T2", BaseCurrency: "USD"},
	}}, jp)
	if err != nil {
		t.Fatal(err)
	}
	return svc
}

func TestFeeEstimateHappy(t *testing.T) {
	svc := feeSvc(t, []FundingFeeTier{tier("5", "10", "0", "", 0)}, 0, nil)
	est, err := svc.Estimate(context.Background(), FeeEstimateRequest{
		AccountID: 1, Rail: "swift", Currency: "usd",
		Direction: "withdrawal", Amount: decimal.NewFromInt(10_000)})
	if err != nil {
		t.Fatalf("estimate: %v", err)
	}
	if est.Fee.String() != "15" || est.NetAmount.String() != "9985" ||
		est.AccountTier != "T2" || est.FreeTier != nil {
		t.Fatalf("estimate=%+v", est)
	}
}

func TestFeeEstimateFreeTier(t *testing.T) {
	// 5 free withdrawals/month on the schedule; 3 used → this one is free.
	svc := feeSvc(t, []FundingFeeTier{tier("5", "0", "0", "", 5)}, 3, nil)
	est, err := svc.Estimate(context.Background(), FeeEstimateRequest{
		AccountID: 1, Rail: "SWIFT", Currency: "USD",
		Direction: "WITHDRAWAL", Amount: decimal.NewFromInt(100)})
	if err != nil {
		t.Fatalf("estimate: %v", err)
	}
	if !est.FreeTier.Applies || !est.Fee.IsZero() || est.ScheduledFee.String() != "5" ||
		est.FreeTier.Remaining != 2 {
		t.Fatalf("free estimate=%+v", est)
	}
	// Exhausted allowance → full fee.
	svc2 := feeSvc(t, []FundingFeeTier{tier("5", "0", "0", "", 5)}, 5, nil)
	est, err = svc2.Estimate(context.Background(), FeeEstimateRequest{
		AccountID: 1, Rail: "SWIFT", Currency: "USD",
		Direction: "WITHDRAWAL", Amount: decimal.NewFromInt(100)})
	if err != nil || est.Fee.String() != "5" || est.FreeTier.Applies {
		t.Fatalf("exhausted estimate=%+v err=%v", est, err)
	}
}

func TestFeeEstimateNotFound(t *testing.T) {
	svc := feeSvc(t, nil, 0, nil)
	_, err := svc.Estimate(context.Background(), FeeEstimateRequest{
		AccountID: 1, Rail: "SWIFT", Currency: "USD",
		Direction: "WITHDRAWAL", Amount: decimal.NewFromInt(100)})
	if codeOfT(t, err) != "FEE_TIER_NOT_FOUND" {
		t.Fatalf("code=%s", codeOfT(t, err))
	}
}

func TestFeeEstimateValidation(t *testing.T) {
	svc := feeSvc(t, []FundingFeeTier{tier("0", "0", "0", "", 0)}, 0, nil)
	for _, tc := range []struct {
		req  FeeEstimateRequest
		code string
	}{
		{FeeEstimateRequest{AccountID: 1, Rail: "BOGUS", Currency: "USD",
			Direction: "WITHDRAWAL", Amount: decimal.One}, "FEE_INVALID_INPUT"},
		{FeeEstimateRequest{AccountID: 1, Rail: "SWIFT", Currency: "*",
			Direction: "WITHDRAWAL", Amount: decimal.One}, "FEE_INVALID_INPUT"},
		{FeeEstimateRequest{AccountID: 1, Rail: "SWIFT", Currency: "USD",
			Direction: "BOGUS", Amount: decimal.One}, "FEE_INVALID_INPUT"},
		{FeeEstimateRequest{AccountID: 1, Rail: "SWIFT", Currency: "USD",
			Direction: "WITHDRAWAL", Amount: decimal.Zero}, "FEE_INVALID_INPUT"},
		{FeeEstimateRequest{AccountID: 1, Rail: "SWIFT", Currency: "USD",
			Direction: "WITHDRAWAL",
			Amount:    decimal.RequireFromString("1.123456789")}, "FEE_INVALID_INPUT"},
		{FeeEstimateRequest{AccountID: 99, Rail: "SWIFT", Currency: "USD",
			Direction: "WITHDRAWAL", Amount: decimal.One}, "NOT_FOUND"},
	} {
		if _, err := svc.Estimate(context.Background(), tc.req); err == nil {
			t.Fatalf("expected error for %+v", tc.req)
		} else if c := codeOfT(t, err); c != tc.code {
			t.Fatalf("req=%+v code=%s want %s", tc.req, c, tc.code)
		}
	}
}

func TestFeeEstimateExceedsAmount(t *testing.T) {
	// Flat 10 on a 5-unit withdrawal → fee ≥ amount → 422 code.
	svc := feeSvc(t, []FundingFeeTier{tier("10", "0", "0", "", 0)}, 0, nil)
	_, err := svc.Estimate(context.Background(), FeeEstimateRequest{
		AccountID: 1, Rail: "SWIFT", Currency: "USD",
		Direction: "WITHDRAWAL", Amount: decimal.NewFromInt(5)})
	if codeOfT(t, err) != CodeFundingFeeExceedsAmount {
		t.Fatalf("code=%s want %s", codeOfT(t, err), CodeFundingFeeExceedsAmount)
	}
}

func TestFeeChargePostsJournal(t *testing.T) {
	poster := &fakePoster{}
	svc := feeSvc(t, []FundingFeeTier{tier("5", "0", "0", "", 0)}, 0, poster)
	out, err := svc.Charge(context.Background(), FeeCharge{
		AccountID: 1, UserID: 100, Rail: "SWIFT", Currency: "USD",
		Direction: "WITHDRAWAL", Amount: decimal.NewFromInt(100),
		FundingTxID: 42})
	if err != nil {
		t.Fatalf("charge: %v", err)
	}
	if out.Fee.String() != "5" || out.JournalID != 1 {
		t.Fatalf("charge=%+v", out)
	}
	if len(poster.journals) != 1 {
		t.Fatalf("journals=%d", len(poster.journals))
	}
	j := poster.journals[0]
	if j.EntryType != ledger.EntryFee || j.ReferenceID != 42 {
		t.Fatalf("journal=%+v", j)
	}
	if len(j.Lines) != 2 || len(j.Effects) != 1 {
		t.Fatalf("lines=%v effects=%v", j.Lines, j.Effects)
	}
	// DR customer liability / CR funding fee revenue; wallet effect −fee.
	if j.Lines[0].AccountCode != ledger.CustomerLiability("USD") ||
		j.Lines[1].AccountCode != ledger.FundingFeeRevenue("USD") {
		t.Fatalf("lines=%+v", j.Lines)
	}
	if !j.Effects[0].AvailableDelta.Equal(decimal.NewFromInt(-5)) {
		t.Fatalf("effect=%+v", j.Effects[0])
	}
}

func TestFeeChargeNilPosterFailsClosed(t *testing.T) {
	svc := feeSvc(t, []FundingFeeTier{tier("5", "0", "0", "", 0)}, 0, nil)
	_, err := svc.Charge(context.Background(), FeeCharge{
		AccountID: 1, Rail: "SWIFT", Currency: "USD",
		Direction: "WITHDRAWAL", Amount: decimal.NewFromInt(100),
		FundingTxID: 1})
	if codeOfT(t, err) != "SERVICE_DEGRADED" {
		t.Fatalf("code=%s want SERVICE_DEGRADED", codeOfT(t, err))
	}
}

func TestFeeChargeFreeConsumesNoPost(t *testing.T) {
	poster := &fakePoster{}
	store := &fakeFeeStore{tiers: []FundingFeeTier{tier("5", "0", "0", "", 2)},
		usage: map[string]int{}}
	svc, err := NewFeeService(store, fakeAccounts{metas: map[int64]*AccountMeta{
		1: {ID: 1, UserID: 100, Status: "ACTIVE", KYCTier: "T2"},
	}}, poster)
	if err != nil {
		t.Fatal(err)
	}
	out, err := svc.Charge(context.Background(), FeeCharge{
		AccountID: 1, Rail: "SWIFT", Currency: "USD",
		Direction: "WITHDRAWAL", Amount: decimal.NewFromInt(100),
		FundingTxID: 1})
	if err != nil {
		t.Fatalf("charge: %v", err)
	}
	if !out.FreeApplied || !out.Fee.IsZero() || out.JournalID != 0 {
		t.Fatalf("free charge=%+v", out)
	}
	if len(poster.journals) != 0 {
		t.Fatal("free movement must not post a journal")
	}
	month := monthStart(time.Now().UTC())
	if n, _ := store.FreeUsageCount(context.Background(), 1, "WITHDRAWAL", month); n != 1 {
		t.Fatalf("usage=%d want 1", n)
	}
}

// ---------------------------------------------------------------------------
// Admin service role gate + payload validation (store seam faked).
// ---------------------------------------------------------------------------

type fakeAdminStore struct {
	fakeFeeStore
	created []FundingFeeTier
	byID    map[int64]*FundingFeeTier
	nextID  int64
}

func newFakeAdminStore(tiers ...FundingFeeTier) *fakeAdminStore {
	f := &fakeAdminStore{byID: map[int64]*FundingFeeTier{}, nextID: 1}
	for i := range tiers {
		f.tiers = append(f.tiers, tiers[i])
		c := tiers[i]
		f.byID[c.ID] = &c
	}
	return f
}

func (f *fakeAdminStore) ListFeeTiers(_ context.Context, flt FeeTierFilter) ([]FundingFeeTier, error) {
	return f.tiers, nil
}

func (f *fakeAdminStore) FeeTierByID(_ context.Context, id int64) (*FundingFeeTier, error) {
	return f.byID[id], nil
}

func (f *fakeAdminStore) FeeTierVersionChain(_ context.Context, id int64) ([]FundingFeeTier, error) {
	base := f.byID[id]
	if base == nil {
		return nil, nil
	}
	return []FundingFeeTier{*base}, nil
}

func (f *fakeAdminStore) CreateFeeTier(_ context.Context, t FundingFeeTier,
	_ FeeAdminActor) (*FundingFeeTier, error) {
	t.ID = f.nextID
	f.nextID++
	f.created = append(f.created, t)
	f.byID[t.ID] = &t
	return &t, nil
}

func (f *fakeAdminStore) CreateFeeTierVersion(_ context.Context, id int64,
	t FundingFeeTier, _ FeeAdminActor) (*FundingFeeTier, error) {
	if f.byID[id] == nil {
		return nil, errf("FEE_TIER_NOT_FOUND", "not found")
	}
	t.ID = f.nextID
	f.nextID++
	f.byID[t.ID] = &t
	return &t, nil
}

func (f *fakeAdminStore) RetireFeeTier(_ context.Context, id int64,
	_ FeeAdminActor) (*FundingFeeTier, error) {
	t := f.byID[id]
	if t == nil {
		return nil, nil
	}
	now := time.Now()
	t.RetiredAt = &now
	return t, nil
}

func adminSvc(resolver RoleResolver, tiers ...FundingFeeTier) (*FeeScheduleService, *fakeAdminStore) {
	st := newFakeAdminStore(tiers...)
	svc, err := NewFeeScheduleService(st, resolver)
	if err != nil {
		panic(err)
	}
	return svc, st
}

func TestFeeScheduleRoleGate(t *testing.T) {
	actor := FeeAdminActor{UserID: 500, ClientIP: "10.0.0.1"}
	// Nil resolver fails closed.
	svc, _ := adminSvc(nil, tier("1", "0", "0", "", 0))
	if _, err := svc.List(context.Background(), actor, FeeTierFilter{}); err == nil ||
		codeOfT(t, err) != "UNAUTHORIZED_ROLE" {
		t.Fatalf("nil resolver: %v", err)
	}
	// Resolver error fails closed.
	svc, _ = adminSvc(func(context.Context, int64) (string, error) {
		return "", fmt.Errorf("store down")
	}, tier("1", "0", "0", "", 0))
	if _, err := svc.List(context.Background(), actor, FeeTierFilter{}); err == nil ||
		codeOfT(t, err) != "UNAUTHORIZED_ROLE" {
		t.Fatalf("resolver err: %v", err)
	}
	// Wrong role refused.
	svc, _ = adminSvc(func(context.Context, int64) (string, error) {
		return "Support Agent", nil
	}, tier("1", "0", "0", "", 0))
	if _, err := svc.List(context.Background(), actor, FeeTierFilter{}); err == nil ||
		codeOfT(t, err) != "UNAUTHORIZED_ROLE" {
		t.Fatalf("support role: %v", err)
	}
	// Finance Ops passes.
	svc, _ = adminSvc(func(context.Context, int64) (string, error) {
		return "Finance Ops", nil
	}, tier("1", "0", "0", "", 0))
	rows, err := svc.List(context.Background(), actor, FeeTierFilter{})
	if err != nil || len(rows) != 1 {
		t.Fatalf("finance ops list: %v len=%d", err, len(rows))
	}
}

func TestFeeScheduleCreateUpdateRetire(t *testing.T) {
	svc, st := adminSvc(func(context.Context, int64) (string, error) {
		return "Finance Ops", nil
	})
	actor := FeeAdminActor{UserID: 500}
	bad := svc.Create
	if _, err := bad(context.Background(), actor, FeeTierCreate{
		Rail: "SWIFT", Currency: "USD", Direction: "WITHDRAWAL",
		AccountTier: "T2", FlatFee: "5", PercentageBps: "10", MinFee: "0",
	}); err != nil {
		t.Fatalf("create: %v", err)
	}
	// Validation failures stay coded 400s.
	for _, req := range []FeeTierCreate{
		{Rail: "NOPE", Currency: "USD", Direction: "WITHDRAWAL",
			FlatFee: "5", PercentageBps: "10", MinFee: "0"},
		{Rail: "SWIFT", Currency: "USD", Direction: "WITHDRAWAL",
			FlatFee: "-1", PercentageBps: "10", MinFee: "0"},
		{Rail: "SWIFT", Currency: "USD", Direction: "WITHDRAWAL",
			FlatFee: "5", PercentageBps: "20000", MinFee: "0"}, // >100%
		{Rail: "SWIFT", Currency: "USD", Direction: "WITHDRAWAL",
			FlatFee: "5", PercentageBps: "10", MinFee: "0",
			AccountTier: "VIP"}, // tier outside domain
	} {
		if _, err := svc.Create(context.Background(), actor, req); err == nil ||
			codeOfT(t, err) != "FEE_INVALID_INPUT" {
			t.Fatalf("create %+v: %v", req, err)
		}
	}
	if len(st.created) != 1 || st.created[0].Version != 1 || st.created[0].CreatedBy != 500 {
		t.Fatalf("created=%+v", st.created)
	}
	// Update inserts a successor version carrying the group identity.
	upd, err := svc.Update(context.Background(), actor, 1, FeeTierUpdate{
		FlatFee: "7", PercentageBps: "10", MinFee: "0"})
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if upd.Version != 2 || upd.FlatFee.String() != "7" ||
		upd.Rail != "SWIFT" || upd.AccountTier != "T2" {
		t.Fatalf("update=%+v", upd)
	}
	// Unknown / retired rows refuse cleanly.
	if _, err := svc.Update(context.Background(), actor, 999, FeeTierUpdate{
		FlatFee: "1", PercentageBps: "0", MinFee: "0"}); err == nil ||
		codeOfT(t, err) != "FEE_TIER_NOT_FOUND" {
		t.Fatalf("update unknown: %v", err)
	}
	ret, err := svc.Retire(context.Background(), actor, 1)
	if err != nil || ret.RetiredAt == nil {
		t.Fatalf("retire: %v %+v", err, ret)
	}
	if _, err := svc.Retire(context.Background(), actor, 999); err == nil ||
		codeOfT(t, err) != "FEE_TIER_NOT_FOUND" {
		t.Fatalf("retire unknown: %v", err)
	}
}
