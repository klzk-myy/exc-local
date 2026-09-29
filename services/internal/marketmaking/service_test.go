package marketmaking

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"exchange/internal/ledger"
	"exchange/internal/observability"
	"exchange/pkg/decimal"
	excerrors "exchange/pkg/errors"
)

// fakeStore is the in-memory Store substitute for unit tests.
type fakeStore struct {
	mu         sync.Mutex
	nextID     int64
	programs   map[int64]*Program
	samples    map[string]*ComplianceRow // "programID|day"
	accruals   map[int64]*RebateAccrual
	nextAccID  int64
	symToID    map[string]int64
	idToSym    map[int64]string
	idToQuoteC map[int64]string
	postErr    error
}

func newFakeStore() *fakeStore {
	return &fakeStore{
		nextID: 1, nextAccID: 1,
		programs:   map[int64]*Program{},
		samples:    map[string]*ComplianceRow{},
		accruals:   map[int64]*RebateAccrual{},
		symToID:    map[string]int64{"EURUSD": 42, "GBPUSD": 43},
		idToSym:    map[int64]string{42: "EURUSD", 43: "GBPUSD"},
		idToQuoteC: map[int64]string{42: "USD", 43: "USD"},
	}
}

func (f *fakeStore) CreateProgram(_ context.Context, p *Program) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, q := range f.programs {
		sameScope := (q.InstrumentID == nil && p.InstrumentID == nil) ||
			(q.InstrumentID != nil && p.InstrumentID != nil && *q.InstrumentID == *p.InstrumentID)
		if q.AccountID == p.AccountID && sameScope {
			return fmt.Errorf("duplicate program for account/instrument")
		}
	}
	p.ID = f.nextID
	f.nextID++
	p.CreatedAt = time.Now()
	p.UpdatedAt = p.CreatedAt
	cp := *p
	f.programs[p.ID] = &cp
	return nil
}

func (f *fakeStore) UpdateProgram(_ context.Context, p *Program) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.programs[p.ID]; !ok {
		return fmt.Errorf("program %d not found", p.ID)
	}
	cp := *p
	f.programs[p.ID] = &cp
	return nil
}

func (f *fakeStore) ProgramByID(_ context.Context, id int64) (*Program, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	p, ok := f.programs[id]
	if !ok {
		return nil, nil
	}
	cp := *p
	return &cp, nil
}

func (f *fakeStore) ProgramsFor(_ context.Context, accountID, instrumentID int64) ([]Program, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []Program
	for _, p := range f.programs {
		if p.AccountID != accountID {
			continue
		}
		if !p.Covers(instrumentID) {
			continue
		}
		out = append(out, *p)
	}
	return out, nil
}

func (f *fakeStore) ListPrograms(_ context.Context, accountID int64, status Status) ([]Program, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []Program
	for _, p := range f.programs {
		if accountID > 0 && p.AccountID != accountID {
			continue
		}
		if status != "" && p.Status != status {
			continue
		}
		out = append(out, *p)
	}
	return out, nil
}

func (f *fakeStore) InstrumentBySymbol(_ context.Context, symbol string) (int64, string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	id, ok := f.symToID[symbol]
	if !ok {
		return 0, "", nil
	}
	return id, f.idToQuoteC[id], nil
}

func (f *fakeStore) InstrumentSymbol(_ context.Context, id int64) (string, error) {
	return f.idToSym[id], nil
}

func (f *fakeStore) QuoteCurrency(_ context.Context, id int64) (string, error) {
	return f.idToQuoteC[id], nil
}

func dayKey(programID int64, day time.Time) string {
	return fmt.Sprintf("%d|%s", programID, day.Format("2006-01-02"))
}

func (f *fakeStore) RecordSample(_ context.Context, programID int64, day time.Time, compliant bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	k := dayKey(programID, day)
	row := f.samples[k]
	if row == nil {
		row = &ComplianceRow{ProgramID: programID, Day: day}
		f.samples[k] = row
	}
	row.SamplesTotal++
	if compliant {
		row.SamplesCompliant++
	}
	return nil
}

func (f *fakeStore) FinalizeCompliance(_ context.Context, programID int64, day time.Time,
	presence decimal.Decimal, breach bool, reason string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	k := dayKey(programID, day)
	row := f.samples[k]
	if row == nil {
		row = &ComplianceRow{ProgramID: programID, Day: day}
		f.samples[k] = row
	}
	v := presence
	row.PresencePct = &v
	row.Breach = breach
	row.BreachReason = reason
	return nil
}

func (f *fakeStore) ComplianceRows(_ context.Context, programID int64, from, to time.Time) ([]ComplianceRow, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []ComplianceRow
	for _, r := range f.samples {
		if r.ProgramID != programID {
			continue
		}
		if r.Day.Before(from) || r.Day.After(to) {
			continue
		}
		out = append(out, *r)
	}
	return out, nil
}

func (f *fakeStore) BreachCount(_ context.Context, programID int64, since time.Time) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, r := range f.samples {
		if r.ProgramID == programID && r.Breach && !r.Day.Before(since.Truncate(24*time.Hour)) {
			n++
		}
	}
	return n, nil
}

func (f *fakeStore) AccrueRebate(_ context.Context, a RebateAccrual) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, x := range f.accruals {
		if x.ProgramID == a.ProgramID && x.FillRef == a.FillRef {
			return nil // idempotent dedup
		}
	}
	a.ID = f.nextAccID
	f.nextAccID++
	f.accruals[a.ID] = &a
	return nil
}

func (f *fakeStore) UnpostedRebates(_ context.Context) ([]RebateAccrual, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []RebateAccrual
	for _, a := range f.accruals {
		if a.PostedJournalID == nil {
			out = append(out, *a)
		}
	}
	return out, nil
}

func (f *fakeStore) MarkRebatesPosted(_ context.Context, ids []int64, journalID int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.postErr != nil {
		return f.postErr
	}
	for _, id := range ids {
		if a, ok := f.accruals[id]; ok {
			jid := journalID
			a.PostedJournalID = &jid
		}
	}
	return nil
}

func (f *fakeStore) ListRebates(_ context.Context, programID int64, _ int) ([]RebateAccrual, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []RebateAccrual
	for _, a := range f.accruals {
		if a.ProgramID == programID {
			out = append(out, *a)
		}
	}
	return out, nil
}

// capSink captures raised alerts.
type capSink struct {
	mu     sync.Mutex
	alerts []observability.Alert
}

func (c *capSink) Raise(_ context.Context, a observability.Alert) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.alerts = append(c.alerts, a)
	return nil
}

func (c *capSink) count(rule string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := 0
	for _, a := range c.alerts {
		if a.Rule == rule {
			n++
		}
	}
	return n
}

func mustDec(t *testing.T, s string) decimal.Decimal {
	t.Helper()
	d, err := decimal.NewFromString(s)
	if err != nil {
		t.Fatalf("dec %q: %v", s, err)
	}
	return d
}

func progFixture(t *testing.T, fs *fakeStore, instID *int64) *Program {
	t.Helper()
	p := &Program{
		AccountID:    7,
		InstrumentID: instID,
		MinQuoteSize: mustDec(t, "100000"),
		MaxSpreadBps: mustDec(t, "5"),
		PresencePct:  mustDec(t, "80"),
		MMPMaxFills:  3,
		MMPWindowMs:  1000,
		RebateBps:    mustDec(t, "0.5"),
	}
	return p
}

// --- Entitlement -------------------------------------------------------------

func TestEntitledPrefersSpecificOverWide(t *testing.T) {
	fs := newFakeStore()
	svc := NewService(fs, Options{})
	ctx := context.Background()

	wide := progFixture(t, fs, nil)
	inst := int64(42)
	spec := progFixture(t, fs, &inst)
	spec.MaxSpreadBps = mustDec(t, "2")
	if err := svc.Enroll(ctx, wide); err != nil {
		t.Fatalf("enroll wide: %v", err)
	}
	if err := svc.Enroll(ctx, spec); err != nil {
		t.Fatalf("enroll spec: %v", err)
	}
	got, err := svc.Entitled(ctx, 7, 42)
	if err != nil || got == nil {
		t.Fatalf("entitled: %v %v", got, err)
	}
	if got.ID != spec.ID {
		t.Fatalf("specific row must win: got program %d want %d", got.ID, spec.ID)
	}
	// Instrument only covered by the wide row → wide wins.
	got, err = svc.Entitled(ctx, 7, 43)
	if err != nil || got == nil || got.ID != wide.ID {
		t.Fatalf("wide coverage failed: %+v err %v", got, err)
	}
	// No coverage → nil.
	got, err = svc.Entitled(ctx, 99, 42)
	if err != nil || got != nil {
		t.Fatalf("unregistered account must not be entitled: %+v", got)
	}
}

func TestSuspendedProgramNotEntitled(t *testing.T) {
	fs := newFakeStore()
	svc := NewService(fs, Options{})
	ctx := context.Background()
	p := progFixture(t, fs, nil)
	if err := svc.Enroll(ctx, p); err != nil {
		t.Fatalf("enroll: %v", err)
	}
	if err := svc.Suspend(ctx, p.ID, "test"); err != nil {
		t.Fatalf("suspend: %v", err)
	}
	got, err := svc.Entitled(ctx, 7, 42)
	if err != nil || got != nil {
		t.Fatalf("suspended program must not entitle: %+v err %v", got, err)
	}
	if err := svc.Resume(ctx, p.ID); err != nil {
		t.Fatalf("resume: %v", err)
	}
	got, _ = svc.Entitled(ctx, 7, 42)
	if got == nil {
		t.Fatal("resumed program must entitle again")
	}
}

func TestOtrAllowanceResolution(t *testing.T) {
	fs := newFakeStore()
	svc := NewService(fs, Options{})
	ctx := context.Background()
	inst := int64(42)
	wide := progFixture(t, fs, nil)
	w := mustDec(t, "1500")
	wide.OtrAllowance = &w
	spec := progFixture(t, fs, &inst)
	s := mustDec(t, "3000")
	spec.OtrAllowance = &s
	if err := svc.Enroll(ctx, wide); err != nil {
		t.Fatal(err)
	}
	if err := svc.Enroll(ctx, spec); err != nil {
		t.Fatal(err)
	}
	if err := svc.Load(ctx); err != nil {
		t.Fatalf("load: %v", err)
	}
	got := svc.OtrAllowance(ctx, 7, "EURUSD")
	if got == nil || !got.Equal(mustDec(t, "3000")) {
		t.Fatalf("per-instrument allowance must win: %v", got)
	}
	got = svc.OtrAllowance(ctx, 7, "GBPUSD")
	if got == nil || !got.Equal(mustDec(t, "1500")) {
		t.Fatalf("program-wide allowance: %v", got)
	}
	if got := svc.OtrAllowance(ctx, 99, "EURUSD"); got != nil {
		t.Fatalf("no program → nil allowance, got %v", got)
	}
}

// --- Compliance --------------------------------------------------------------

func TestEvaluateObligation(t *testing.T) {
	p := &Program{MinQuoteSize: mustDec(t, "100"), MaxSpreadBps: mustDec(t, "10")}
	bidPx := mustDec(t, "1.1000")
	askPx := mustDec(t, "1.1005")
	qty := mustDec(t, "100")
	v := EvaluateObligation(p, &bidPx, &qty, &askPx, &qty)
	if !v.Compliant() {
		t.Fatalf("tight two-sided quote must comply: %+v", v)
	}
	small := mustDec(t, "50")
	v = EvaluateObligation(p, &bidPx, &small, &askPx, &qty)
	if v.Compliant() || v.SizeOK {
		t.Fatalf("under-size side must fail size: %+v", v)
	}
	wide := mustDec(t, "1.1020") // ~18.2bps vs 10bps floor
	v = EvaluateObligation(p, &bidPx, &qty, &wide, &qty)
	if v.Compliant() || v.SpreadOK {
		t.Fatalf("wide spread must fail spread: %+v", v)
	}
	v = EvaluateObligation(p, nil, &qty, &askPx, &qty)
	if v.Compliant() {
		t.Fatal("one-sided quote is never compliant")
	}
}

func TestRollupBreachSuspendsAfterThreshold(t *testing.T) {
	fs := newFakeStore()
	sink := &capSink{}
	now := time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC)
	svc := NewService(fs, Options{Alerts: sink, Now: func() time.Time { return now }})
	ctx := context.Background()
	p := progFixture(t, fs, nil)
	if err := svc.Enroll(ctx, p); err != nil {
		t.Fatal(err)
	}
	// 4 samples, 1 compliant → 25% presence vs 80% floor → breach.
	day := now.UTC().Truncate(24 * time.Hour)
	for i := 0; i < 4; i++ {
		if err := fs.RecordSample(ctx, p.ID, day, i == 0); err != nil {
			t.Fatal(err)
		}
	}
	row, err := svc.RollupDay(ctx, p.ID, day)
	if err != nil {
		t.Fatal(err)
	}
	if !row.Breach {
		t.Fatalf("expected breach at 25%% presence vs 80%% floor: %+v", row)
	}
	if row.PresencePct == nil || !row.PresencePct.Equal(mustDec(t, "25")) {
		t.Fatalf("presence rollup wrong: %v", row.PresencePct)
	}
	if sink.count("mm_obligation_breach") != 1 {
		t.Fatalf("breach must alert admins: %d", sink.count("mm_obligation_breach"))
	}
	// Two more breach days inside the rolling week → suspension.
	for _, d := range []time.Time{day.Add(-24 * time.Hour), day.Add(-48 * time.Hour)} {
		if err := fs.RecordSample(ctx, p.ID, d, false); err != nil {
			t.Fatal(err)
		}
		if _, err := svc.RollupDay(ctx, p.ID, d); err != nil {
			t.Fatal(err)
		}
	}
	got, _ := svc.Get(ctx, p.ID)
	if got.Status != StatusSuspended {
		t.Fatalf("3 breaches in rolling week must suspend: %s", got.Status)
	}
	if sink.count("mm_program_suspended") != 1 {
		t.Fatalf("suspension must alert: %d", sink.count("mm_program_suspended"))
	}
}

func TestObserveQuoteSamplesBothPrograms(t *testing.T) {
	fs := newFakeStore()
	svc := NewService(fs, Options{})
	ctx := context.Background()
	inst := int64(42)
	wide := progFixture(t, fs, nil)
	spec := progFixture(t, fs, &inst)
	if err := svc.Enroll(ctx, wide); err != nil {
		t.Fatal(err)
	}
	if err := svc.Enroll(ctx, spec); err != nil {
		t.Fatal(err)
	}
	bid := mustDec(t, "1.1")
	ask := mustDec(t, "1.10001")
	qty := mustDec(t, "200000")
	if err := svc.ObserveQuote(ctx, 7, 42, &bid, &qty, &ask, &qty); err != nil {
		t.Fatal(err)
	}
	if fs.samples[dayKey(wide.ID, svc.todayUTC())].SamplesTotal != 1 {
		t.Fatal("program-wide row must sample")
	}
	if fs.samples[dayKey(spec.ID, svc.todayUTC())].SamplesCompliant != 1 {
		t.Fatal("compliant quote must increment compliant count")
	}
}

// --- MMP ---------------------------------------------------------------------

func TestMMPTriggerMassCancelsAndLocks(t *testing.T) {
	fs := newFakeStore()
	svc := NewService(fs, Options{})
	ctx := context.Background()
	inst := int64(42)
	p := progFixture(t, fs, &inst) // 3 fills / 1000ms
	if err := svc.Enroll(ctx, p); err != nil {
		t.Fatal(err)
	}
	var cancelCalls []struct {
		programID int64
		instID    int64
	}
	tracker := NewMMPTracker(svc, func(_ context.Context, got *Program, iid int64) (int, error) {
		cancelCalls = append(cancelCalls, struct {
			programID int64
			instID    int64
		}{got.ID, iid})
		return 5, nil
	}).WithClock(func() time.Time { return time.Now() })

	for i := 0; i < 3; i++ {
		trig, err := tracker.OnFill(ctx, 7, 42)
		if err != nil || trig {
			t.Fatalf("fill %d must not trigger: trig=%v err=%v", i+1, trig, err)
		}
	}
	trig, err := tracker.OnFill(ctx, 7, 42) // 4th fill > mmp_max_fills(3)
	if err != nil || !trig {
		t.Fatalf("4th fill must trigger MMP: trig=%v err=%v", trig, err)
	}
	if len(cancelCalls) != 1 || cancelCalls[0].instID != 42 {
		t.Fatalf("mass-cancel must be scoped to the fill instrument: %+v", cancelCalls)
	}
	if !tracker.MMPLocked(ctx, 7, 42) {
		t.Fatal("lockout must hold after trigger")
	}
	if err := tracker.ResetMMP(ctx, 7, 42); err != nil {
		t.Fatal(err)
	}
	if tracker.MMPLocked(ctx, 7, 42) {
		t.Fatal("reset must clear lockout")
	}
}

func TestMMPWindowSlides(t *testing.T) {
	fs := newFakeStore()
	svc := NewService(fs, Options{})
	ctx := context.Background()
	inst := int64(42)
	p := progFixture(t, fs, &inst)
	if err := svc.Enroll(ctx, p); err != nil {
		t.Fatal(err)
	}
	base := time.Now()
	cur := base
	tracker := NewMMPTracker(svc, nil).WithClock(func() time.Time { return cur })
	// 4 fills spread over 4s — window is 1000ms so each fill sees ≤1 in
	// window; never triggers.
	for i := 0; i < 4; i++ {
		cur = base.Add(time.Duration(i) * 2 * time.Second)
		trig, err := tracker.OnFill(ctx, 7, 42)
		if err != nil || trig {
			t.Fatalf("stale-window fills must not trigger: trig=%v", trig)
		}
	}
}

func TestMMPNoProgramNoTrigger(t *testing.T) {
	fs := newFakeStore()
	svc := NewService(fs, Options{})
	tracker := NewMMPTracker(svc, nil)
	trig, err := tracker.OnFill(context.Background(), 99, 42)
	if trig || err != nil {
		t.Fatalf("unregistered account fills are ignored: trig=%v err=%v", trig, err)
	}
}

// --- Rebates -----------------------------------------------------------------

func TestAccrueRebateAmountAndIdempotency(t *testing.T) {
	fs := newFakeStore()
	svc := NewService(fs, Options{})
	ctx := context.Background()
	inst := int64(42)
	p := progFixture(t, fs, &inst) // rebate 0.5bps
	if err := svc.Enroll(ctx, p); err != nil {
		t.Fatal(err)
	}
	amt, err := svc.AccrueRebate(ctx, 7, 42, "fill-1",
		mustDec(t, "100000"), mustDec(t, "1.10"))
	if err != nil {
		t.Fatal(err)
	}
	// 100000 × 1.10 × 0.5 / 10000 = 5.5 USD
	if !amt.Equal(mustDec(t, "5.5")) {
		t.Fatalf("rebate math: got %s want 5.5", amt)
	}
	// Replayed fillRef → dedup no-op.
	if _, err := svc.AccrueRebate(ctx, 7, 42, "fill-1",
		mustDec(t, "100000"), mustDec(t, "1.10")); err != nil {
		t.Fatal(err)
	}
	rows, _ := fs.ListRebates(ctx, p.ID, 10)
	if len(rows) != 1 {
		t.Fatalf("idempotent accrual must persist once: %d rows", len(rows))
	}
	if rows[0].Currency != "USD" {
		t.Fatalf("rebate denominates in quote ccy: %s", rows[0].Currency)
	}
}

func TestAccrueRebatePausedWhenSuspended(t *testing.T) {
	fs := newFakeStore()
	svc := NewService(fs, Options{})
	ctx := context.Background()
	p := progFixture(t, fs, nil)
	if err := svc.Enroll(ctx, p); err != nil {
		t.Fatal(err)
	}
	if err := svc.Suspend(ctx, p.ID, "test"); err != nil {
		t.Fatal(err)
	}
	amt, err := svc.AccrueRebate(ctx, 7, 42, "fill-x",
		mustDec(t, "100000"), mustDec(t, "1.1"))
	if err != nil {
		t.Fatal(err)
	}
	if !amt.IsZero() {
		t.Fatalf("suspended program must not accrue: %s", amt)
	}
}

type fakePoster struct {
	posted   []ledger.Journal
	nextID   int64
	postedAt map[string]bool
}

func (f *fakePoster) Post(_ context.Context, j ledger.Journal) (ledger.PostResult, error) {
	f.nextID++
	f.posted = append(f.posted, j)
	return ledger.PostResult{JournalID: f.nextID}, nil
}

func TestPostAccruedRebatesGroupsAndMarks(t *testing.T) {
	fs := newFakeStore()
	poster := &fakePoster{}
	svc := NewService(fs, Options{Poster: poster})
	ctx := context.Background()
	p := progFixture(t, fs, nil)
	if err := svc.Enroll(ctx, p); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if _, err := svc.AccrueRebate(ctx, 7, 42,
			fmt.Sprintf("f%d", i), mustDec(t, "1000"), mustDec(t, "1.1")); err != nil {
			t.Fatal(err)
		}
	}
	posted, err := svc.PostAccruedRebates(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(posted) != 1 {
		t.Fatalf("one (account,ccy) group expected: %v", posted)
	}
	if len(poster.posted) != 1 {
		t.Fatalf("one journal expected: %d", len(poster.posted))
	}
	j := poster.posted[0]
	if len(j.Lines) != 2 {
		t.Fatalf("balanced DR/CR pair required: %d lines", len(j.Lines))
	}
	if j.IdempotencyKey == "" {
		t.Fatal("journal must carry an idempotency key")
	}
	unposted, _ := fs.UnpostedRebates(ctx)
	if len(unposted) != 0 {
		t.Fatalf("posted rows must be stamped: %d unposted", len(unposted))
	}
	// Re-sweep is a no-op (idempotent monthly run).
	posted, err = svc.PostAccruedRebates(ctx)
	if err != nil || len(posted) != 0 {
		t.Fatalf("re-sweep must be empty: %v %v", posted, err)
	}
}

func TestPostAccruedRebatesNeedsPoster(t *testing.T) {
	fs := newFakeStore()
	svc := NewService(fs, Options{})
	if _, err := svc.PostAccruedRebates(context.Background()); err == nil ||
		excerrors.CodeOf(err) != CodeMMInternal {
		t.Fatalf("unwired poster must refuse: %v", err)
	}
}
