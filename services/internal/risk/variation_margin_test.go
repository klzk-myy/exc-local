package risk

import (
	"context"
	"testing"
	"time"

	"exchange/internal/ledger"
	"exchange/pkg/decimal"
)

// ---- fixtures ---------------------------------------------------------

type vmRow struct {
	id        int64
	settledAt *time.Time
	vm        decimal.Decimal
	mtm       decimal.Decimal
	journalID *int64
	shortfall bool
}

type fakeVMStore struct {
	subjects []VMSubject
	rows     map[string]*vmRow
	nextID   int64
	failPrev bool
}

func newFakeVMStore(subjects ...VMSubject) *fakeVMStore {
	return &fakeVMStore{subjects: subjects, rows: map[string]*vmRow{}, nextID: 1}
}

func vmKey(kind string, ref int64, day time.Time) string {
	return kind + ":" + decimal.NewFromInt(ref).String() + ":" + day.Format("2006-01-02")
}

func (s *fakeVMStore) DerivativeSubjects(context.Context) ([]VMSubject, error) {
	return s.subjects, nil
}

func (s *fakeVMStore) ClaimSettlement(_ context.Context, subj VMSubject, day time.Time) (int64, bool, error) {
	k := vmKey(subj.Kind, subj.RefID, day)
	if r, ok := s.rows[k]; ok {
		return r.id, r.settledAt != nil, nil
	}
	r := &vmRow{id: s.nextID}
	s.nextID++
	s.rows[k] = r
	return r.id, false, nil
}

func (s *fakeVMStore) PrevMTM(_ context.Context, subj VMSubject, beforeDay time.Time) (decimal.Decimal, error) {
	if s.failPrev {
		return decimal.Zero, errTestStore
	}
	var best *vmRow
	for k, r := range s.rows {
		_ = k
		if r.settledAt == nil {
			continue
		}
		if best == nil || r.settledAt.After(*best.settledAt) {
			best = r
		}
	}
	if best == nil {
		return decimal.Zero, nil
	}
	return best.mtm, nil
}

var errTestStore = excErrFixture

func (s *fakeVMStore) CompleteSettlement(_ context.Context, rowID int64, journalID *int64,
	vm, mtm, rate decimal.Decimal, shortfall bool, at time.Time) error {
	for _, r := range s.rows {
		if r.id == rowID {
			if r.settledAt != nil {
				return errTestStore
			}
			t := at
			r.settledAt = &t
			r.vm, r.mtm, r.journalID, r.shortfall = vm, mtm, journalID, shortfall
			return nil
		}
	}
	return errTestStore
}

var excErrFixture = errFixture("store failure")

type errFixture string

func (e errFixture) Error() string { return string(e) }

type fakeVMPoster struct {
	posted []ledger.Journal
	negBal bool // simulate a post-commit negative-available event
}

func (p *fakeVMPoster) Post(_ context.Context, j ledger.Journal) (ledger.PostResult, error) {
	if err := j.Validate(); err != nil {
		return ledger.PostResult{}, err
	}
	p.posted = append(p.posted, j)
	res := ledger.PostResult{JournalID: int64(len(p.posted)), Committed: true}
	if p.negBal {
		res.Events = []ledger.BalanceEvent{{
			AccountID: j.Effects[0].AccountID, Currency: j.Effects[0].Currency,
			Available: "-10", Locked: "0", Total: "-10"}}
	}
	return res, nil
}

func testSubject(kind string, ref, acct, instr int64, mtm string) VMSubject {
	return VMSubject{
		Kind: kind, RefID: ref, AccountID: acct, InstrumentID: instr,
		Symbol: "EUR/USD-FWD", Currency: "USD",
		MTM: decimal.RequireFromString(mtm), MarkRate: decimal.RequireFromString("1.10"),
	}
}

// ---- tests ------------------------------------------------------------

func TestVMSweep_PositiveVMPostsGL(t *testing.T) {
	store := newFakeVMStore(testSubject(VMSubjectPosition, 7, 42, 9, "150"))
	poster := &fakeVMPoster{}
	svc, err := NewVariationMarginService(store, poster, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	day := time.Date(2026, 9, 20, 0, 0, 0, 0, time.UTC)
	rep, err := svc.Sweep(context.Background(), day)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Settled != 1 || rep.Posted != 1 || rep.Skipped != 0 {
		t.Fatalf("report %+v", rep)
	}
	if len(poster.posted) != 1 {
		t.Fatalf("expected 1 journal, got %d", len(poster.posted))
	}
	j := poster.posted[0]
	// VM +150: DR house equity / CR customer liability, +150 to the wallet.
	if j.Lines[0].AccountCode != ledger.HouseEquity("USD") ||
		j.Lines[1].AccountCode != ledger.CustomerLiability("USD") {
		t.Fatalf("wrong account codes: %+v", j.Lines)
	}
	if !j.Effects[0].AvailableDelta.Equal(decimal.RequireFromString("150")) {
		t.Fatalf("effect delta %s", j.Effects[0].AvailableDelta)
	}
	if j.IdempotencyKey == "" {
		t.Fatal("missing idempotency key")
	}
	// Row stamped.
	r := store.rows[vmKey(VMSubjectPosition, 7, day)]
	if r.settledAt == nil || !r.vm.Equal(decimal.RequireFromString("150")) {
		t.Fatalf("row not settled: %+v", r)
	}
}

func TestVMSweep_NegativeVMCollects(t *testing.T) {
	store := newFakeVMStore(testSubject(VMSubjectPosition, 7, 42, 9, "-80"))
	poster := &fakeVMPoster{}
	svc, _ := NewVariationMarginService(store, poster, nil, nil, nil)
	day := time.Date(2026, 9, 20, 0, 0, 0, 0, time.UTC)
	if _, err := svc.Sweep(context.Background(), day); err != nil {
		t.Fatal(err)
	}
	j := poster.posted[0]
	if j.Lines[0].AccountCode != ledger.CustomerLiability("USD") ||
		j.Lines[1].AccountCode != ledger.HouseEquity("USD") {
		t.Fatalf("negative VM lines wrong: %+v", j.Lines)
	}
	if !j.Effects[0].AvailableDelta.Equal(decimal.RequireFromString("-80")) ||
		!j.Effects[0].AllowNegative {
		t.Fatalf("effect %+v", j.Effects[0])
	}
}

func TestVMSweep_ZeroVMNoJournal(t *testing.T) {
	store := newFakeVMStore(testSubject(VMSubjectPosition, 7, 42, 9, "0"))
	poster := &fakeVMPoster{}
	svc, _ := NewVariationMarginService(store, poster, nil, nil, nil)
	day := time.Date(2026, 9, 20, 0, 0, 0, 0, time.UTC)
	rep, err := svc.Sweep(context.Background(), day)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Settled != 1 || rep.Posted != 0 || len(poster.posted) != 0 {
		t.Fatalf("zero-VM should settle without posting: %+v, journals=%d", rep, len(poster.posted))
	}
	if store.rows[vmKey(VMSubjectPosition, 7, day)].settledAt == nil {
		t.Fatal("watermark not stamped")
	}
}

func TestVMSweep_RerunSameDayNoDoublePost(t *testing.T) {
	store := newFakeVMStore(testSubject(VMSubjectPosition, 7, 42, 9, "25"))
	poster := &fakeVMPoster{}
	svc, _ := NewVariationMarginService(store, poster, nil, nil, nil)
	day := time.Date(2026, 9, 20, 0, 0, 0, 0, time.UTC)
	if _, err := svc.Sweep(context.Background(), day); err != nil {
		t.Fatal(err)
	}
	rep, err := svc.Sweep(context.Background(), day)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Skipped != 1 || rep.Posted != 0 || len(poster.posted) != 1 {
		t.Fatalf("rerun double-posted: %+v journals=%d", rep, len(poster.posted))
	}
}

func TestVMSweep_SecondDayUsesPrevWatermark(t *testing.T) {
	store := newFakeVMStore(testSubject(VMSubjectPosition, 7, 42, 9, "100"))
	poster := &fakeVMPoster{}
	svc, _ := NewVariationMarginService(store, poster, nil, nil, nil)
	d1 := time.Date(2026, 9, 20, 0, 0, 0, 0, time.UTC)
	d2 := d1.AddDate(0, 0, 1)
	if _, err := svc.Sweep(context.Background(), d1); err != nil {
		t.Fatal(err)
	}
	// Day 2: MTM moved to 130 → VM = +30.
	store.subjects[0].MTM = decimal.RequireFromString("130")
	if _, err := svc.Sweep(context.Background(), d2); err != nil {
		t.Fatal(err)
	}
	if len(poster.posted) != 2 {
		t.Fatalf("expected 2 journals, got %d", len(poster.posted))
	}
	if !poster.posted[1].Effects[0].AvailableDelta.Equal(decimal.RequireFromString("30")) {
		t.Fatalf("day-2 VM %s", poster.posted[1].Effects[0].AvailableDelta)
	}
}

func TestVMSweep_AllSubjectKindsAndTypes(t *testing.T) {
	// All four derivative instrument classes ride positions rows; booked
	// OTC contracts ride CONTRACT subjects. The sweep treats them alike.
	subs := []VMSubject{
		testSubject(VMSubjectPosition, 1, 1, 101, "10"),  // FORWARD pos
		testSubject(VMSubjectPosition, 2, 1, 102, "-5"),  // SWAP pos
		testSubject(VMSubjectPosition, 3, 2, 103, "7"),   // NDF pos
		testSubject(VMSubjectPosition, 4, 2, 104, "3"),   // OPTION pos
		testSubject(VMSubjectContract, 55, 3, 105, "-2"), // booked contract
	}
	store := newFakeVMStore(subs...)
	poster := &fakeVMPoster{}
	svc, _ := NewVariationMarginService(store, poster, nil, nil, nil)
	day := time.Date(2026, 9, 20, 0, 0, 0, 0, time.UTC)
	rep, err := svc.Sweep(context.Background(), day)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Settled != 5 || rep.Posted != 5 || len(rep.Errors) != 0 {
		t.Fatalf("report %+v", rep)
	}
}

func TestVMSweep_ShortfallFlagged(t *testing.T) {
	store := newFakeVMStore(testSubject(VMSubjectPosition, 7, 42, 9, "-80"))
	poster := &fakeVMPoster{negBal: true}
	svc, _ := NewVariationMarginService(store, poster, nil, nil, nil)
	day := time.Date(2026, 9, 20, 0, 0, 0, 0, time.UTC)
	rep, err := svc.Sweep(context.Background(), day)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Shortfalls != 1 {
		t.Fatalf("expected shortfall flag, got %+v", rep)
	}
	if !store.rows[vmKey(VMSubjectPosition, 7, day)].shortfall {
		t.Fatal("row not flagged shortfall")
	}
}

func TestVMSweep_ResumesUnsettledClaim(t *testing.T) {
	store := newFakeVMStore(testSubject(VMSubjectPosition, 7, 42, 9, "40"))
	poster := &fakeVMPoster{}
	svc, _ := NewVariationMarginService(store, poster, nil, nil, nil)
	day := time.Date(2026, 9, 20, 0, 0, 0, 0, time.UTC)
	// Simulate a crashed sweep: claim exists, settled_at NULL.
	id, done, err := store.ClaimSettlement(context.Background(), store.subjects[0], day)
	if err != nil || done {
		t.Fatalf("claim: %v done=%v", err, done)
	}
	rep, err := svc.Sweep(context.Background(), day)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Settled != 1 || len(poster.posted) != 1 {
		t.Fatalf("resume failed: %+v", rep)
	}
	// Reused the same claim row (same journal idempotency key).
	if poster.posted[0].ReferenceID != id {
		t.Fatalf("journal ref %d != claim %d", poster.posted[0].ReferenceID, id)
	}
}

func TestVMSweep_NilDepsFailClosed(t *testing.T) {
	if _, err := NewVariationMarginService(nil, &fakeVMPoster{}, nil, nil, nil); err == nil {
		t.Fatal("nil store accepted")
	}
	if _, err := NewVariationMarginService(newFakeVMStore(), nil, nil, nil, nil); err == nil {
		t.Fatal("nil poster accepted")
	}
}

func TestVMSweep_StoreErrorPropagates(t *testing.T) {
	store := newFakeVMStore(testSubject(VMSubjectPosition, 7, 42, 9, "10"))
	store.failPrev = true
	poster := &fakeVMPoster{}
	svc, _ := NewVariationMarginService(store, poster, nil, nil, nil)
	rep, err := svc.Sweep(context.Background(), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Errors) != 1 || len(poster.posted) != 0 {
		t.Fatalf("expected one subject error, no posts: %+v", rep)
	}
}

// ---- ExerciseShortfallLiquidator ---------------------------------------

func TestExerciseShortfallLiquidator_NilQueueFailsClosed(t *testing.T) {
	if _, err := NewExerciseShortfallLiquidator(nil, nil, nil, nil); err == nil {
		t.Fatal("nil queue accepted")
	}
}
