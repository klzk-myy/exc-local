// Unit tests for Tasks 7.3.13/7.3.14 — fake packStore / PackSource /
// PackDeliverySink / DualControlApprover; PG coverage lives in
// admin_integration_test.go (EXC_PG_TEST=1).
package admin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	excerrors "exchange/pkg/errors"
)

// ---------------------------------------------------------------------------
// Fakes
// ---------------------------------------------------------------------------

type fakePackStore struct {
	packs       map[int64]*GovernancePack
	order       []int64
	nextID      int64
	deliveries  map[int64][]PackDelivery
	releasedCnt int
	releaseErr  error
}

func newFakePackStore() *fakePackStore {
	return &fakePackStore{packs: map[int64]*GovernancePack{},
		nextID: 1, deliveries: map[int64][]PackDelivery{}}
}

func (f *fakePackStore) insertPack(_ context.Context, p GovernancePack) (*GovernancePack, error) {
	p.PackID = f.nextID
	f.nextID++
	p.GeneratedAt = time.Now().UTC()
	if p.Deliveries == nil {
		p.Deliveries = json.RawMessage(`[]`)
	}
	cp := p
	f.packs[p.PackID] = &cp
	f.order = append(f.order, p.PackID)
	return &cp, nil
}

func (f *fakePackStore) getPack(_ context.Context, id int64) (*GovernancePack, error) {
	p, ok := f.packs[id]
	if !ok {
		return nil, excerrors.New("NOT_FOUND", "pack not found")
	}
	return p, nil
}

func (f *fakePackStore) listPacks(_ context.Context, kind string, releasedOnly bool, _ int) ([]GovernancePack, error) {
	out := []GovernancePack{}
	for i := len(f.order) - 1; i >= 0; i-- {
		p := f.packs[f.order[i]]
		if kind != "" && p.Kind != kind {
			continue
		}
		if releasedOnly && p.Status != PackReleased {
			continue
		}
		out = append(out, *p)
	}
	return out, nil
}

func (f *fakePackStore) latestPackHash(_ context.Context, kind string) (string, error) {
	for i := len(f.order) - 1; i >= 0; i-- {
		p := f.packs[f.order[i]]
		if p.Kind == kind {
			return p.ContentHash, nil
		}
	}
	return "", nil
}

func (f *fakePackStore) recordDelivery(_ context.Context, packID int64, d PackDelivery) error {
	f.deliveries[packID] = append(f.deliveries[packID], d)
	return nil
}

// releasePack mirrors pgPackStore semantics: lock, GENERATED gate,
// kind gate, immutability via a released flag.
func (f *fakePackStore) releasePack(_ context.Context, packID int64,
	initiator, approver int64, reason, _ string) (*GovernancePack, error) {
	if f.releaseErr != nil {
		return nil, f.releaseErr
	}
	p, ok := f.packs[packID]
	if !ok {
		return nil, excerrors.New("NOT_FOUND", "pack not found")
	}
	if p.Kind == PackKindCEODaily {
		return nil, excerrors.New("INVALID_REQUEST", "CEO_DAILY packs are generated-only")
	}
	if p.Status != PackGenerated {
		return nil, excerrors.New("INVALID_REQUEST", "pack is not GENERATED")
	}
	now := time.Now().UTC()
	p.Status = PackReleased
	p.ReleaseInitiatedBy = &initiator
	p.ReleaseInitiatedAt = &now
	p.ReleasedBy = &approver
	p.ReleasedAt = &now
	p.ReleaseReason = reason
	f.releasedCnt++
	return p, nil
}

type fakeSource struct {
	name, owner, ver string
	data             map[string]any
	err              error
}

func (s fakeSource) Section() string { return s.name }
func (s fakeSource) Owner() string   { return s.owner }
func (s fakeSource) Collect(context.Context, PackPeriod) (map[string]any, string, error) {
	if s.err != nil {
		return nil, "", s.err
	}
	return s.data, s.ver, nil
}

type fakeDelivery struct {
	got []GovernancePack
	err error
}

func (d *fakeDelivery) DeliverPack(_ context.Context, p GovernancePack) (string, error) {
	d.got = append(d.got, p)
	return "dashboard", d.err
}

func saRole(ids ...int64) AdminRoleResolver {
	return lpRoles(mapFrom(ids, "Super Admin"))
}

func mapFrom(ids []int64, role string) map[int64]string {
	m := map[int64]string{}
	for _, id := range ids {
		m[id] = role
	}
	return m
}

// sectionByName finds a rendered section in the stored pack content.
func sectionByName(t *testing.T, p *GovernancePack, name string) PackSection {
	t.Helper()
	var c packContent
	if err := json.Unmarshal(p.Content, &c); err != nil {
		t.Fatalf("decode pack content: %v", err)
	}
	for _, s := range c.Sections {
		if s.Name == name {
			return s
		}
	}
	t.Fatalf("section %q not found in pack", name)
	return PackSection{}
}

// ---------------------------------------------------------------------------
// Period helpers
// ---------------------------------------------------------------------------

func TestPackPeriods(t *testing.T) {
	d := DailyPeriod(time.Date(2026, 3, 4, 15, 30, 0, 0, time.UTC))
	if d.Label != "2026-03-04" || d.End.Sub(d.Start) != 24*time.Hour {
		t.Fatalf("bad daily period: %+v", d)
	}
	q, err := QuarterlyPeriod(2026, 1)
	if err != nil {
		t.Fatalf("quarterly: %v", err)
	}
	if q.Label != "2026Q1" || q.Start.Month() != 1 || q.End.Month() != 4 {
		t.Fatalf("bad quarterly period: %+v", q)
	}
	if _, err := QuarterlyPeriod(2026, 5); err == nil {
		t.Fatal("quarter 5 must fail")
	}
	a, err := AdhocPeriod("emergency-2026-03")
	if err != nil || a.Label != "emergency-2026-03" {
		t.Fatalf("adhoc: %+v err=%v", a, err)
	}
	if _, err := AdhocPeriod("   "); err == nil {
		t.Fatal("blank ad-hoc label must fail")
	}
}

// ---------------------------------------------------------------------------
// Generation: role gate, assembly, hashing
// ---------------------------------------------------------------------------

func TestGenerateRoleGate(t *testing.T) {
	store := newFakePackStore()
	svc := newGovernancePackService(store, nil, nil, nil, nil)
	if _, err := svc.Generate(context.Background(), AdminActor{UserID: 1},
		PackKindCEODaily, DailyPeriod(time.Now())); err == nil {
		t.Fatal("nil resolver must fail closed")
	} else if codeOf(t, err) != "UNAUTHORIZED_ROLE" {
		t.Fatalf("want UNAUTHORIZED_ROLE, got %s", eCode(err))
	}

	svc = newGovernancePackService(store, lpRoles(map[int64]string{1: "Risk Manager"}), nil, nil, nil)
	if _, err := svc.Generate(context.Background(), AdminActor{UserID: 1},
		PackKindCEODaily, DailyPeriod(time.Now())); err == nil {
		t.Fatal("Risk Manager must not generate packs")
	} else if codeOf(t, err) != "UNAUTHORIZED_ROLE" {
		t.Fatalf("want UNAUTHORIZED_ROLE, got %s", eCode(err))
	}

	svc = newGovernancePackService(store, saRole(1), nil, nil, nil)
	if _, err := svc.Generate(context.Background(), AdminActor{},
		PackKindCEODaily, DailyPeriod(time.Now())); err == nil {
		t.Fatal("anonymous must be rejected")
	} else if codeOf(t, err) != "UNAUTHORIZED" {
		t.Fatalf("want UNAUTHORIZED, got %s", eCode(err))
	}
	if _, err := svc.Generate(context.Background(), AdminActor{UserID: 1},
		"BOGUS", DailyPeriod(time.Now())); err == nil {
		t.Fatal("invalid kind must fail")
	}
}

func TestGenerateSectionsHashAndVersions(t *testing.T) {
	store := newFakePackStore()
	svc := newGovernancePackService(store, saRole(1),
		[]PackSource{
			fakeSource{"trade_volume", "Phase-03", "pg:trades@max_id=42",
				map[string]any{"trades": float64(7)}, nil},
			fakeSource{"incidents", "Phase-09", "",
				nil, errors.New("clickhouse timeout")},
		}, nil, nil)

	p, err := svc.Generate(context.Background(), AdminActor{UserID: 1},
		PackKindCEODaily, DailyPeriod(time.Date(2026, 3, 4, 0, 0, 0, 0, time.UTC)))
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	if p.Status != PackGenerated || p.Kind != PackKindCEODaily {
		t.Fatalf("pack state: %+v", p)
	}
	if p.ContentHash != PackContentHash(p.Content) {
		t.Fatal("stored hash must equal sha256 of the rendered content")
	}
	if !VerifyHash(p) {
		t.Fatal("VerifyHash must pass on a fresh pack")
	}
	ok := sectionByName(t, p, "trade_volume")
	if ok.Status != SectionOK || ok.SourceVersion != "pg:trades@max_id=42" {
		t.Fatalf("live section: %+v", ok)
	}
	bad := sectionByName(t, p, "incidents")
	if bad.Status != SectionStale || bad.Error == "" {
		t.Fatalf("failed source must mark STALE with error: %+v", bad)
	}
	// Source versions persisted for reproducibility.
	var ver map[string]string
	if err := json.Unmarshal(p.SourceVersions, &ver); err != nil {
		t.Fatalf("source_versions: %v", err)
	}
	if ver["trade_volume"] != "pg:trades@max_id=42" {
		t.Fatalf("source_versions missing live section: %v", ver)
	}
	// CEO pack delivery: nil sink records a QUEUED delivery.
	if got := store.deliveries[p.PackID]; len(got) != 1 || got[0].Channel != "queued" {
		t.Fatalf("want one QUEUED delivery record, got %+v", got)
	}
}

func TestGenerateHashChain(t *testing.T) {
	store := newFakePackStore()
	src := []PackSource{fakeSource{"s", "o", "v1", map[string]any{"x": 1}, nil}}
	svc := newGovernancePackService(store, saRole(1), src, nil, nil)

	p1, _ := svc.GenerateScheduled(context.Background(), PackKindCEODaily,
		DailyPeriod(time.Date(2026, 3, 4, 0, 0, 0, 0, time.UTC)))
	if p1.PrevPackHash != "" {
		t.Fatal("genesis pack must have empty prev_pack_hash")
	}
	p2, _ := svc.GenerateScheduled(context.Background(), PackKindCEODaily,
		DailyPeriod(time.Date(2026, 3, 5, 0, 0, 0, 0, time.UTC)))
	if p2.PrevPackHash != p1.ContentHash {
		t.Fatalf("hash chain broken: prev=%q want %q", p2.PrevPackHash, p1.ContentHash)
	}
	// Board packs chain independently of CEO packs.
	svc2 := newGovernancePackService(store, saRole(1), nil,
		[]PackSource{fakeSource{"s", "o", "v1", map[string]any{"x": 1}, nil}}, nil)
	b, err := svc2.GenerateBoard(context.Background(), AdminActor{UserID: 1},
		PackKindBoardQuarterly, mustQ(t, 2026, 1))
	if err != nil {
		t.Fatalf("board generate: %v", err)
	}
	if b.PrevPackHash == p2.ContentHash {
		t.Fatal("board pack chain must not link to the CEO chain")
	}
}

func mustQ(t *testing.T, year, q int) PackPeriod {
	t.Helper()
	p, err := QuarterlyPeriod(year, q)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestBoardAbsentMarking(t *testing.T) {
	store := newFakePackStore()
	svc := newGovernancePackService(store, saRole(1), nil,
		[]PackSource{
			fakeSource{"cco_report", "Phase-21 Task 21.3.15", "", nil,
				fmt.Errorf("%w: table cco_reports", errSourceAbsent)},
			fakeSource{"incident_rca_log", "Phase-09", "", nil,
				errors.New("pg connection refused")},
		}, nil)

	p, err := svc.GenerateBoard(context.Background(), AdminActor{UserID: 1},
		PackKindBoardQuarterly, mustQ(t, 2026, 1))
	if err != nil {
		t.Fatalf("board generate: %v", err)
	}
	abs := sectionByName(t, p, "cco_report")
	if abs.Status != SectionAbsent || abs.Owner != "Phase-21 Task 21.3.15" || abs.DueDate == "" {
		t.Fatalf("unprovisioned source must mark ABSENT with owner+due: %+v", abs)
	}
	st := sectionByName(t, p, "incident_rca_log")
	if st.Status != SectionStale {
		t.Fatalf("errored source must mark STALE: %+v", st)
	}

	// CEO packs mark absent sources STALE (daily roll-up never waits).
	svcCEO := newGovernancePackService(store, saRole(1),
		[]PackSource{fakeSource{"x", "o", "", nil,
			fmt.Errorf("%w: no table", errSourceAbsent)}}, nil, nil)
	cp, err := svcCEO.Generate(context.Background(), AdminActor{UserID: 1},
		PackKindCEODaily, DailyPeriod(time.Now()))
	if err != nil {
		t.Fatalf("ceo generate: %v", err)
	}
	if s := sectionByName(t, cp, "x"); s.Status != SectionStale {
		t.Fatalf("CEO pack must mark absent source STALE, got %s", s.Status)
	}
}

func TestGenerateDeliverySink(t *testing.T) {
	store := newFakePackStore()
	deliv := &fakeDelivery{}
	svc := newGovernancePackService(store, saRole(1),
		[]PackSource{fakeSource{"s", "o", "v", map[string]any{}, nil}}, nil, deliv)
	p, err := svc.Generate(context.Background(), AdminActor{UserID: 1},
		PackKindCEODaily, DailyPeriod(time.Now()))
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	if len(deliv.got) != 1 {
		t.Fatal("delivery sink must be invoked")
	}
	if got := store.deliveries[p.PackID]; len(got) != 1 || got[0].Channel != "dashboard" || !got[0].OK {
		t.Fatalf("want dashboard delivery recorded, got %+v", got)
	}
}

// ---------------------------------------------------------------------------
// Release: dual control, immutability surface
// ---------------------------------------------------------------------------

func TestReleaseDualControl(t *testing.T) {
	store := newFakePackStore()
	svc := newGovernancePackService(store, lpRoles(map[int64]string{
		1: "Super Admin", 2: "Super Admin", 3: "Risk Manager",
	}), nil, []PackSource{
		fakeSource{"s", "o", "v", map[string]any{}, nil},
	}, nil)

	p, err := svc.GenerateBoard(context.Background(), AdminActor{UserID: 1},
		PackKindBoardQuarterly, mustQ(t, 2026, 1))
	if err != nil {
		t.Fatalf("generate: %v", err)
	}

	// Missing second approver.
	if _, err := svc.Release(context.Background(),
		AdminActor{UserID: 1}, p.PackID, "q1"); err == nil {
		t.Fatal("release without approver must fail")
	} else if codeOf(t, err) != "DUAL_CONTROL_REQUIRED" {
		t.Fatalf("want DUAL_CONTROL_REQUIRED, got %s", eCode(err))
	}
	// Maker == checker.
	if _, err := svc.Release(context.Background(),
		AdminActor{UserID: 1, ApproverID: 1}, p.PackID, "q1"); err == nil {
		t.Fatal("same-id approver must fail")
	} else if codeOf(t, err) != "DUAL_CONTROL_REQUIRED" {
		t.Fatalf("want DUAL_CONTROL_REQUIRED, got %s", eCode(err))
	}
	// Approver lacks the release role.
	if _, err := svc.Release(context.Background(),
		AdminActor{UserID: 1, ApproverID: 3}, p.PackID, "q1"); err == nil {
		t.Fatal("ineligible approver must fail")
	} else if codeOf(t, err) != "UNAUTHORIZED_ROLE" {
		t.Fatalf("want UNAUTHORIZED_ROLE, got %s", eCode(err))
	}
	// Maker lacks the release role.
	if _, err := svc.Release(context.Background(),
		AdminActor{UserID: 3, ApproverID: 2}, p.PackID, "q1"); err == nil {
		t.Fatal("non-Super-Admin initiator must fail")
	} else if codeOf(t, err) != "UNAUTHORIZED_ROLE" {
		t.Fatalf("want UNAUTHORIZED_ROLE, got %s", eCode(err))
	}

	// Happy path: distinct eligible approver.
	out, err := svc.Release(context.Background(),
		AdminActor{UserID: 1, ApproverID: 2}, p.PackID, "board sign-off")
	if err != nil {
		t.Fatalf("release: %v", err)
	}
	if out.Status != PackReleased || out.ReleasedBy == nil || *out.ReleasedBy != 2 {
		t.Fatalf("released state: %+v", out)
	}
	if out.ReleaseInitiatedBy == nil || *out.ReleaseInitiatedBy != 1 {
		t.Fatalf("maker must be recorded: %+v", out)
	}

	// Second release of the same pack must fail (immutable lifecycle).
	if _, err := svc.Release(context.Background(),
		AdminActor{UserID: 2, ApproverID: 1}, p.PackID, "again"); err == nil {
		t.Fatal("re-release must fail")
	}
}

func TestReleaseCEODailyRejected(t *testing.T) {
	store := newFakePackStore()
	svc := newGovernancePackService(store, saRole(1, 2),
		[]PackSource{fakeSource{"s", "o", "v", map[string]any{}, nil}}, nil, nil)
	p, err := svc.Generate(context.Background(), AdminActor{UserID: 1},
		PackKindCEODaily, DailyPeriod(time.Now()))
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	if _, err := svc.Release(context.Background(),
		AdminActor{UserID: 1, ApproverID: 2}, p.PackID, ""); err == nil {
		t.Fatal("CEO_DAILY release must be rejected")
	} else if codeOf(t, err) != "INVALID_REQUEST" {
		t.Fatalf("want INVALID_REQUEST, got %s", eCode(err))
	}
}

// ---------------------------------------------------------------------------
// Auditor read gate
// ---------------------------------------------------------------------------

func TestPackReadGate(t *testing.T) {
	store := newFakePackStore()
	svc := newGovernancePackService(store, lpRoles(map[int64]string{
		1: "Super Admin", 9: "Read-Only Auditor", 4: "Support Agent", 2: "Super Admin",
	}), []PackSource{fakeSource{"s", "o", "v", map[string]any{}, nil}},
		[]PackSource{fakeSource{"s", "o", "v", map[string]any{}, nil}}, nil)

	board, _ := svc.GenerateBoard(context.Background(), AdminActor{UserID: 1},
		PackKindBoardQuarterly, mustQ(t, 2026, 1))
	ceo, _ := svc.Generate(context.Background(), AdminActor{UserID: 1},
		PackKindCEODaily, DailyPeriod(time.Now()))
	auditor := AdminActor{UserID: 9}

	// GENERATED board pack is not auditor-visible.
	if _, err := svc.Get(context.Background(), auditor, board.PackID); err == nil {
		t.Fatal("draft board pack must not be auditor-readable")
	} else if codeOf(t, err) != "FORBIDDEN" {
		t.Fatalf("want FORBIDDEN, got %s", eCode(err))
	}
	// CEO roll-ups are auditor-readable (record of what execs saw).
	if _, err := svc.Get(context.Background(), auditor, ceo.PackID); err != nil {
		t.Fatalf("auditor CEO read: %v", err)
	}
	// List hides the draft board pack from the auditor.
	l, err := svc.List(context.Background(), auditor, "", 0)
	if err != nil {
		t.Fatalf("auditor list: %v", err)
	}
	for _, p := range l {
		if p.PackID == board.PackID {
			t.Fatal("draft board pack leaked into auditor list")
		}
	}
	// After release the pack becomes auditor-readable.
	if _, err := svc.Release(context.Background(),
		AdminActor{UserID: 1, ApproverID: 2}, board.PackID, "q1"); err != nil {
		t.Fatalf("release: %v", err)
	}
	if _, err := svc.Get(context.Background(), auditor, board.PackID); err != nil {
		t.Fatalf("released pack must be auditor-readable: %v", err)
	}
	l, _ = svc.List(context.Background(), auditor, "", 0)
	found := false
	for _, p := range l {
		if p.PackID == board.PackID {
			found = true
		}
	}
	if !found {
		t.Fatal("released board pack missing from auditor list")
	}
	// Unrelated roles cannot read packs at all.
	if _, err := svc.Get(context.Background(), AdminActor{UserID: 4}, ceo.PackID); err == nil {
		t.Fatal("Support Agent must not read packs")
	} else if codeOf(t, err) != "UNAUTHORIZED_ROLE" {
		t.Fatalf("want UNAUTHORIZED_ROLE, got %s", eCode(err))
	}
}

func TestVerifyHashTamper(t *testing.T) {
	store := newFakePackStore()
	svc := newGovernancePackService(store, saRole(1),
		[]PackSource{fakeSource{"s", "o", "v", map[string]any{}, nil}}, nil, nil)
	p, _ := svc.Generate(context.Background(), AdminActor{UserID: 1},
		PackKindCEODaily, DailyPeriod(time.Now()))
	p.Content = json.RawMessage(`{"kind":"CEO_DAILY","tampered":true}`)
	if VerifyHash(p) {
		t.Fatal("tampered content must fail hash verification")
	}
	if VerifyHash(nil) {
		t.Fatal("nil pack must fail")
	}
}

func TestGenerateBoardKindGuard(t *testing.T) {
	svc := newGovernancePackService(newFakePackStore(), saRole(1), nil, nil, nil)
	if _, err := svc.GenerateBoard(context.Background(), AdminActor{UserID: 1},
		PackKindCEODaily, DailyPeriod(time.Now())); err == nil {
		t.Fatal("GenerateBoard must reject CEO_DAILY")
	}
	// Adhoc period works.
	if _, err := svc.GenerateBoard(context.Background(), AdminActor{UserID: 1},
		PackKindBoardAdhoc, mustAdhoc(t, "ecb-flash-review")); err != nil {
		t.Fatalf("adhoc board: %v", err)
	}
}

func mustAdhoc(t *testing.T, label string) PackPeriod {
	t.Helper()
	p, err := AdhocPeriod(label)
	if err != nil {
		t.Fatal(err)
	}
	return p
}
