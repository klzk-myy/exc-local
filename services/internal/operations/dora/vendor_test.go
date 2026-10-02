package dora

// Task 6 item 3 (IMP-PLAN Phase-3) — DORA Art. 28 ICT provider register
// unit coverage: create validation (Art. 28 fail-closed concentration
// rule), review cadence bookkeeping, retire, and the due sweep's three
// alert classes.

import (
	"context"
	"sort"
	"testing"
	"time"
)

type memVendorStore struct {
	nextID  int64
	rows    map[int64]*Provider
	reviews map[int64][]Review
}

func newMemVendorStore() *memVendorStore {
	return &memVendorStore{rows: map[int64]*Provider{},
		reviews: map[int64][]Review{}}
}

func (m *memVendorStore) InsertProvider(_ context.Context, p *Provider) (int64, error) {
	m.nextID++
	cp := *p
	cp.ID = m.nextID
	m.rows[cp.ID] = &cp
	return cp.ID, nil
}

func (m *memVendorStore) UpdateProvider(_ context.Context, p *Provider) (bool, error) {
	if _, ok := m.rows[p.ID]; !ok {
		return false, nil
	}
	cp := *p
	m.rows[p.ID] = &cp
	return true, nil
}

func (m *memVendorStore) SetProviderStatus(_ context.Context, id int64,
	status string, at time.Time) (bool, error) {
	p, ok := m.rows[id]
	if !ok || p.Status == status {
		return false, nil
	}
	p.Status = status
	p.UpdatedAt = at
	return true, nil
}

func (m *memVendorStore) GetProvider(_ context.Context, id int64) (*Provider, bool, error) {
	p, ok := m.rows[id]
	if !ok {
		return nil, false, nil
	}
	cp := *p
	return &cp, true, nil
}

func (m *memVendorStore) ListProviders(_ context.Context, status string) ([]Provider, error) {
	var out []Provider
	for _, p := range m.rows {
		if status == "" || p.Status == status {
			out = append(out, *p)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

func (m *memVendorStore) InsertReview(_ context.Context, r *Review) (int64, error) {
	m.nextID++
	r.ID = m.nextID
	m.reviews[r.ProviderID] = append(m.reviews[r.ProviderID], *r)
	return r.ID, nil
}

func (m *memVendorStore) ListReviews(_ context.Context, providerID int64) ([]Review, error) {
	return append([]Review(nil), m.reviews[providerID]...), nil
}

func (m *memVendorStore) SetProviderSchedule(_ context.Context, id int64,
	nextReview, lastTest *time.Time) error {
	p := m.rows[id]
	if nextReview != nil {
		p.NextReviewAt = nextReview
	}
	if lastTest != nil {
		p.LastSubstitutionTestAt = lastTest
	}
	return nil
}

func TestVendorCreateValidation(t *testing.T) {
	svc, _ := NewVendorService(newMemVendorStore())
	ctx := context.Background()

	if _, err := svc.Create(ctx, &Provider{}); err == nil {
		t.Fatal("empty provider accepted")
	}
	// Art. 28 fail-closed: HIGH/MEDIUM concentration requires an exit
	// strategy.
	if _, err := svc.Create(ctx, &Provider{Name: "X", ICTService: "svc",
		Concentration: ConcHigh}); err == nil {
		t.Fatal("HIGH concentration without exit_strategy accepted")
	}
	p, err := svc.Create(ctx, &Provider{Name: "X", ICTService: "svc",
		Concentration: ConcHigh, ExitStrategy: "failover to secondary"})
	if err != nil || p.ID == 0 || p.Status != ProviderActive {
		t.Fatalf("create: %+v err=%v", p, err)
	}
	// LOW concentration needs no exit strategy.
	if _, err := svc.Create(ctx, &Provider{Name: "Y", ICTService: "svc",
		Concentration: ConcLow}); err != nil {
		t.Fatalf("LOW without exit strategy rejected: %v", err)
	}
	if _, err := svc.Create(ctx, &Provider{Name: "Z", ICTService: "svc",
		Concentration: "CRITICAL"}); err == nil {
		t.Fatal("bogus concentration accepted")
	}
}

func TestVendorReviewCadenceAndSubstTest(t *testing.T) {
	st := newMemVendorStore()
	svc, _ := NewVendorService(st)
	now := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	svc.SetClockForTest(func() time.Time { return now })
	ctx := context.Background()

	p, _ := svc.Create(ctx, &Provider{Name: "P", ICTService: "svc",
		Concentration: ConcMedium, ExitStrategy: "insource"})
	if _, err := svc.RecordReview(ctx, &Review{
		ProviderID: p.ID, Kind: ReviewKindReview, Outcome: ReviewPass,
	}); err != nil {
		t.Fatalf("review: %v", err)
	}
	got := st.rows[p.ID].NextReviewAt
	if got == nil || !got.Equal(now.AddDate(1, 0, 0)) {
		t.Fatalf("next_review_at %v, want +1y", got)
	}
	if _, err := svc.RecordReview(ctx, &Review{
		ProviderID: p.ID, Kind: ReviewKindSubstitutionTest,
		Outcome: ReviewPass, EvidenceRef: "drill-2026-D1",
	}); err != nil {
		t.Fatalf("subst test: %v", err)
	}
	if lt := st.rows[p.ID].LastSubstitutionTestAt; lt == nil || !lt.Equal(now) {
		t.Fatalf("last_substitution_test_at %v, want %v", lt, now)
	}
	if _, err := svc.RecordReview(ctx, &Review{
		ProviderID: p.ID, Kind: "BOGUS", Outcome: ReviewPass,
	}); err == nil {
		t.Fatal("bogus review kind accepted")
	}
	if _, err := svc.RecordReview(ctx, &Review{
		ProviderID: 999, Kind: ReviewKindReview, Outcome: ReviewPass,
	}); err == nil {
		t.Fatal("review on unknown provider accepted")
	}
}

func TestVendorSweep(t *testing.T) {
	st := newMemVendorStore()
	svc, _ := NewVendorService(st)
	now := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	svc.SetClockForTest(func() time.Time { return now })
	ctx := context.Background()

	overdue := now.AddDate(0, 0, -10)
	renewSoon := now.AddDate(0, 0, 15) // inside 30d default lead
	staleTest := now.AddDate(-2, 0, 0)
	freshTest := now.AddDate(0, -1, 0)
	notice := 45
	farRenewal := now.AddDate(0, 0, 60) // beyond 45d notice

	rows := []*Provider{
		{Name: "overdue", ICTService: "a", Concentration: ConcLow,
			NextReviewAt: &overdue},
		{Name: "renewal", ICTService: "b", Concentration: ConcLow,
			RenewalAt: &renewSoon},
		{Name: "never-tested", ICTService: "c", Concentration: ConcHigh,
			ExitStrategy: "x"},
		{Name: "stale-test", ICTService: "d", Concentration: ConcMedium,
			ExitStrategy: "x", LastSubstitutionTestAt: &staleTest},
		{Name: "fresh-test", ICTService: "e", Concentration: ConcMedium,
			ExitStrategy: "x", LastSubstitutionTestAt: &freshTest},
		{Name: "far-renewal", ICTService: "f", Concentration: ConcLow,
			RenewalAt: &farRenewal, TerminationNoticeDays: &notice},
		{Name: "retired", ICTService: "g", Concentration: ConcHigh,
			ExitStrategy: "x", NextReviewAt: &overdue,
			Status: ProviderRetired},
	}
	for _, p := range rows {
		if _, err := svc.Create(ctx, p); err != nil {
			t.Fatalf("create %s: %v", p.Name, err)
		}
	}

	alerts, err := svc.Sweep(ctx)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	byCode := map[string][]int64{}
	for _, a := range alerts {
		byCode[a.Code] = append(byCode[a.Code], a.ProviderID)
	}
	if len(byCode["ICT_REVIEW_OVERDUE"]) != 1 {
		t.Fatalf("overdue alerts: %+v", byCode)
	}
	if len(byCode["ICT_RENEWAL_DUE"]) != 1 { // 45d notice row is out of runway
		t.Fatalf("renewal alerts: %+v", byCode)
	}
	if len(byCode["ICT_EXIT_TEST_STALE"]) != 2 { // never-tested + stale-test
		t.Fatalf("stale-test alerts: %+v", byCode)
	}
	for _, a := range alerts {
		if a.Name == "retired" || a.Name == "fresh-test" ||
			a.Name == "far-renewal" {
			t.Fatalf("unexpected alert for %s", a.Name)
		}
	}
}

func TestVendorRetire(t *testing.T) {
	svc, _ := NewVendorService(newMemVendorStore())
	ctx := context.Background()
	p, _ := svc.Create(ctx, &Provider{Name: "P", ICTService: "svc",
		Concentration: ConcLow})
	if err := svc.Retire(ctx, p.ID, 42); err != nil {
		t.Fatalf("retire: %v", err)
	}
	if _, err := svc.Update(ctx, &Provider{ID: p.ID, Name: "P",
		ICTService: "svc"}); err == nil {
		t.Fatal("update on RETIRED provider accepted")
	}
	if err := svc.Retire(ctx, 999, 42); err == nil {
		t.Fatal("retire on unknown provider accepted")
	}
}
