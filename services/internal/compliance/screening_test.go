package compliance

// Phase-21 Task 21.3.11 — ScreeningService behavior: PEP onboarding
// hits, sanctions hits → hold routing, quarantined provider → queue
// deferral, rescreen cadence, delta rescreening, adverse-media intake
// and replay-hit routing.

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// fakeHoldPlacer records PlaceHold calls.
type fakeHoldPlacer struct {
	reqs []PlaceHoldRequest
	err  error
}

func (f *fakeHoldPlacer) PlaceHold(_ context.Context,
	r PlaceHoldRequest) (*Hold, error) {
	f.reqs = append(f.reqs, r)
	if f.err != nil {
		return nil, f.err
	}
	return &Hold{HoldID: "H-FAKE", AccountID: r.AccountID}, nil
}

// spyAlerts collects (severity, code, summary) tuples.
type spyAlerts struct{ rows [][3]string }

func (s *spyAlerts) raise(_ context.Context, sev, code, summary string) error {
	s.rows = append(s.rows, [3]string{sev, code, summary})
	return nil
}

func (s *spyAlerts) has(code string) bool {
	for _, r := range s.rows {
		if r[1] == code {
			return true
		}
	}
	return false
}

// spyAuditor records audit actions.
type spyAuditor struct{ actions []string }

func (s *spyAuditor) Record(_ context.Context, _ int64, action, _ string,
	_ int64, _ any) error {
	s.actions = append(s.actions, action)
	return nil
}

func (s *spyAuditor) has(action string) bool {
	for _, a := range s.actions {
		if a == action {
			return true
		}
	}
	return false
}

// screenerWith builds a screener over the given fixture files.
func screenerWith(t *testing.T, files map[string]string) *ListScreener {
	t.Helper()
	s, err := NewListScreener(fixtureDir(t, files))
	if err != nil {
		t.Fatalf("screener: %v", err)
	}
	return s
}

func TestScreenOnboardingSanctionsHit(t *testing.T) {
	s := screenerWith(t, map[string]string{
		"dev.txt": "Blocked Beneficiary Trading\n"})
	store := NewMemoryScreeningStore()
	holds := &fakeHoldPlacer{}
	alerts := &spyAlerts{}
	aud := &spyAuditor{}
	svc, err := NewScreeningService(ScreeningOptions{
		Screener: s, Store: store, Holds: holds,
		Alerter: alerts.raise, Auditor: aud})
	if err != nil {
		t.Fatalf("service: %v", err)
	}
	out, err := svc.ScreenOnboarding(context.Background(), ScreeningSubject{
		AccountID: 42, UserID: 7,
		LegalName: "Blocked Beneficiary Trading"})
	if err != nil {
		t.Fatalf("screen: %v", err)
	}
	if len(out.SanctionHits) != 1 || len(out.PEPHits) != 0 {
		t.Fatalf("outcome: %+v", out)
	}
	if len(holds.reqs) != 1 || holds.reqs[0].Trigger != HoldTriggerSanctionsHit {
		t.Fatalf("hold routing: %+v", holds.reqs)
	}
	if !holds.reqs[0].HighConfidence {
		t.Fatal("sanctions hit must carry high-confidence (4h SLA)")
	}
	if !alerts.has("SANCTIONS_SCREEN_HIT") {
		t.Fatalf("alerts: %+v", alerts.rows)
	}
	if !aud.has("screening.hit") {
		t.Fatalf("audit: %+v", aud.actions)
	}
	if _, ok := store.ScreenedAt(42, EntryKindSanctions); !ok {
		t.Fatal("screen stamp not recorded")
	}
}

func TestScreenOnboardingPEPHitRoutesToReview(t *testing.T) {
	s := screenerWith(t, map[string]string{
		"dev.txt": "Blocked Beneficiary Trading\n",
		"pep.txt": "Senator Fixture Example\n"})
	store := NewMemoryScreeningStore()
	holds := &fakeHoldPlacer{}
	svc, err := NewScreeningService(ScreeningOptions{
		Screener: s, Store: store, Holds: holds})
	if err != nil {
		t.Fatalf("service: %v", err)
	}
	out, err := svc.ScreenOnboarding(context.Background(), ScreeningSubject{
		AccountID: 42, LegalName: "Senator Fixture Example"})
	if err != nil {
		t.Fatalf("screen: %v", err)
	}
	if len(out.PEPHits) != 1 || len(out.SanctionHits) != 0 {
		t.Fatalf("outcome: %+v", out)
	}
	if len(holds.reqs) != 1 || holds.reqs[0].Trigger != HoldTriggerPEPMatch {
		t.Fatalf("PEP hit must place a PEP_MATCH hold: %+v", holds.reqs)
	}
	if holds.reqs[0].HighConfidence {
		t.Fatal("PEP review hold must not carry the 4h sanctions SLA")
	}
}

func TestScreenOnboardingQuarantinedDefers(t *testing.T) {
	s := screenerWith(t, map[string]string{"dev.txt": "Blocked Beneficiary Trading\n"})
	store := NewMemoryScreeningStore()
	queue := NewMemoryScreenQueue()
	gate := NewProviderGate([]string{"vendorA"})
	gate.ReportResult(context.Background(), "vendorA", errTest())
	gated := NewQuarantinedScreener(s, gate, queue)
	svc, err := NewScreeningService(ScreeningOptions{
		Screener: s, Gated: gated, Store: store})
	if err != nil {
		t.Fatalf("service: %v", err)
	}
	out, err := svc.ScreenOnboarding(context.Background(), ScreeningSubject{
		AccountID: 42, LegalName: "Totally Clean Name"})
	if err != nil {
		t.Fatalf("quarantined screen must not error the caller: %v", err)
	}
	if !out.Quarantined {
		t.Fatal("outcome not marked quarantined")
	}
	if d, _ := queue.Depth(context.Background()); d != 1 {
		t.Fatalf("queue depth: %d", d)
	}
}

func TestScreenOnboardingCleanStamps(t *testing.T) {
	s := screenerWith(t, map[string]string{"dev.txt": "Blocked Beneficiary Trading\n"})
	store := NewMemoryScreeningStore()
	svc, err := NewScreeningService(ScreeningOptions{
		Screener: s, Store: store})
	if err != nil {
		t.Fatalf("service: %v", err)
	}
	out, err := svc.ScreenOnboarding(context.Background(), ScreeningSubject{
		AccountID: 9, LegalName: "Alice Example"})
	if err != nil {
		t.Fatalf("screen: %v", err)
	}
	if len(out.SanctionHits)+len(out.PEPHits) != 0 || out.Quarantined {
		t.Fatalf("clean outcome polluted: %+v", out)
	}
	if _, ok := store.ScreenedAt(9, EntryKindSanctions); !ok {
		t.Fatal("clean screen stamp missing")
	}
}

func TestRescreenDuePicksOnlyStale(t *testing.T) {
	s := screenerWith(t, map[string]string{"dev.txt": "Blocked Beneficiary Trading\n"})
	store := NewMemoryScreeningStore()
	store.SeedSubject(ScreeningSubject{AccountID: 1, LegalName: "Alice"})
	store.SeedSubject(ScreeningSubject{AccountID: 2, LegalName: "Bob"})
	svc, err := NewScreeningService(ScreeningOptions{
		Screener: s, Store: store, Rescreen: time.Minute})
	if err != nil {
		t.Fatalf("service: %v", err)
	}
	ctx := context.Background()
	// Screen account 1 — fresh stamp; 2 remains unscreened.
	if _, err := svc.ScreenOnboarding(ctx, ScreeningSubject{
		AccountID: 1, LegalName: "Alice"}); err != nil {
		t.Fatalf("screen: %v", err)
	}
	n, err := svc.RescreenDue(ctx, 10)
	if err != nil {
		t.Fatalf("rescreen: %v", err)
	}
	if n != 1 {
		t.Fatalf("expected 1 stale rescreen, got %d", n)
	}
	// Both stamped now — a second sweep finds none due.
	if n, _ := svc.RescreenDue(ctx, 10); n != 0 {
		t.Fatalf("rescreen not idempotent: %d", n)
	}
}

func TestDeltaRescreenOnlyNewlyAdded(t *testing.T) {
	dir := fixtureDir(t, map[string]string{
		"dev.txt": "Blocked Beneficiary Trading\n"})
	s, err := NewListScreener(dir)
	if err != nil {
		t.Fatalf("screener: %v", err)
	}
	store := NewMemoryScreeningStore()
	store.SeedSubject(ScreeningSubject{AccountID: 1,
		LegalName: "Blocked Beneficiary Trading"}) // matches pre-existing entry
	store.SeedSubject(ScreeningSubject{AccountID: 2,
		LegalName: "Newly Listed Villain"})
	holds := &fakeHoldPlacer{}
	svc, err := NewScreeningService(ScreeningOptions{
		Screener: s, Store: store, Holds: holds})
	if err != nil {
		t.Fatalf("service: %v", err)
	}
	ctx := context.Background()
	// Grow the list via a real reload so delta.Added carries the
	// normalized newly-listed name.
	if err := os.WriteFile(filepath.Join(dir, "dev.txt"), []byte(
		"Blocked Beneficiary Trading\nNewly Listed Villain\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := s.Reload(ctx); err != nil {
		t.Fatalf("reload: %v", err)
	}
	n, err := svc.DeltaRescreen(ctx, s.LastDelta())
	if err != nil {
		t.Fatalf("delta rescreen: %v", err)
	}
	if n != 1 {
		t.Fatalf("expected exactly 1 newly-hit subject, got %d", n)
	}
	if len(holds.reqs) != 1 || holds.reqs[0].AccountID != 2 {
		t.Fatalf("delta hit routed hold to wrong account: %+v", holds.reqs)
	}
	// A delta containing only removals never auto-clears nor holds.
	n, err = svc.DeltaRescreen(ctx, ListDelta{Removed: []string{"X Y"}})
	if err != nil || n != 0 {
		t.Fatalf("removal delta: n=%d err=%v", n, err)
	}
	if len(holds.reqs) != 1 {
		t.Fatal("removal delta placed a hold")
	}
}

func TestAdverseMediaSeverityRouting(t *testing.T) {
	s := screenerWith(t, map[string]string{"dev.txt": "Blocked Beneficiary Trading\n"})
	store := NewMemoryScreeningStore()
	holds := &fakeHoldPlacer{}
	alerts := &spyAlerts{}
	svc, err := NewScreeningService(ScreeningOptions{
		Screener: s, Store: store, Holds: holds, Alerter: alerts.raise})
	if err != nil {
		t.Fatalf("service: %v", err)
	}
	ctx := context.Background()
	sub := ScreeningSubject{AccountID: 5, UserID: 9, LegalName: "Alice"}
	// MEDIUM → recorded + P3 alert, no hold.
	if err := svc.ReportAdverseMedia(ctx, sub, AdverseMediaItem{
		Source: "vendor", Headline: "Fixture faces probe", Severity: "MEDIUM"}); err != nil {
		t.Fatalf("medium intake: %v", err)
	}
	if len(holds.reqs) != 0 {
		t.Fatal("medium severity must not place a hold")
	}
	if !alerts.has("ADVERSE_MEDIA") {
		t.Fatal("medium severity must alert P3")
	}
	// HIGH → recorded + UNUSUAL_ACTIVITY hold + P1.
	if err := svc.ReportAdverseMedia(ctx, sub, AdverseMediaItem{
		Source: "vendor", Headline: "Fixture indicted", Severity: "HIGH"}); err != nil {
		t.Fatalf("high intake: %v", err)
	}
	if len(holds.reqs) != 1 || holds.reqs[0].Trigger != HoldTriggerUnusualActivity {
		t.Fatalf("HIGH severity must place UNUSUAL_ACTIVITY hold: %+v", holds.reqs)
	}
	if !alerts.has("ADVERSE_MEDIA_HIGH") {
		t.Fatal("HIGH severity must page P1")
	}
	if store.AdverseCount(5) != 2 {
		t.Fatalf("adverse rows: %d", store.AdverseCount(5))
	}
	// Account-mismatch is rejected.
	if err := svc.ReportAdverseMedia(ctx, sub, AdverseMediaItem{
		AccountID: 999, Headline: "x"}); err == nil {
		t.Fatal("mismatched account_id accepted")
	}
}

func TestHandleReplayHitRoutesHold(t *testing.T) {
	s := screenerWith(t, map[string]string{"dev.txt": "Blocked Beneficiary Trading\n"})
	store := NewMemoryScreeningStore()
	holds := &fakeHoldPlacer{}
	svc, err := NewScreeningService(ScreeningOptions{
		Screener: s, Store: store, Holds: holds})
	if err != nil {
		t.Fatalf("service: %v", err)
	}
	hits, err := s.ScreenParty(context.Background(), "",
		"Blocked Beneficiary Trading")
	if err != nil || len(hits) != 1 {
		t.Fatalf("screen setup: %v %v", hits, err)
	}
	err = svc.HandleReplayHit(context.Background(), PendingScreen{
		ID: 1, AccountID: 77, ActorID: 8, Flow: ScreenFlowWithdrawal,
		Candidates: []string{"Blocked Beneficiary Trading"}}, hits)
	if err != nil {
		t.Fatalf("replay hit: %v", err)
	}
	if len(holds.reqs) != 1 || holds.reqs[0].AccountID != 77 ||
		holds.reqs[0].Trigger != HoldTriggerSanctionsHit {
		t.Fatalf("replay hit must place a SANCTIONS_HIT hold on 77: %+v", holds.reqs)
	}
}
