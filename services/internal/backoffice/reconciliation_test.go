// Task 24.3.2 tests — daily reconciliation, §24 #21 thresholds, break
// categories, auto-resolution, the investigation workflow, P1 alerting.
package backoffice

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"exchange/pkg/decimal"
)

// fakeReconStore is a working in-memory NostroReconStore.
type fakeReconStore struct {
	fakeNostroStore
	mu2     sync.Mutex
	entries map[int64][]NostroStatementEntry // per account
	runs    []NostroReconRun
	breaks  []NostroReconBreak
	nextRID int64
	nextBID int64
}

func newFakeReconStore() *fakeReconStore {
	return &fakeReconStore{
		fakeNostroStore: *newFakeNostroStore(),
		entries:         map[int64][]NostroStatementEntry{},
		nextRID:         1, nextBID: 1,
	}
}

func (f *fakeReconStore) UpsertStatementEntries(_ context.Context, entries []NostroStatementEntry) (int, error) {
	f.mu2.Lock()
	defer f.mu2.Unlock()
	n := 0
	for _, e := range entries {
		dup := false
		for _, ex := range f.entries[e.NostroAccountID] {
			if ex.SwiftReference == e.SwiftReference &&
				ex.Direction == e.Direction && ex.Amount.Equal(e.Amount) {
				dup = true
				break
			}
		}
		if dup {
			continue
		}
		e.ID = int64(len(f.entries[e.NostroAccountID]) + 1)
		f.entries[e.NostroAccountID] = append(f.entries[e.NostroAccountID], e)
		n++
	}
	return n, nil
}

func (f *fakeReconStore) StatementEntriesFor(_ context.Context, accountID int64, day time.Time) ([]NostroStatementEntry, error) {
	f.mu2.Lock()
	defer f.mu2.Unlock()
	var out []NostroStatementEntry
	for _, e := range f.entries[accountID] {
		if e.StatementDate.Equal(day) {
			out = append(out, e)
		}
	}
	return out, nil
}

func (f *fakeReconStore) InsertReconRun(_ context.Context, r *NostroReconRun) error {
	f.mu2.Lock()
	defer f.mu2.Unlock()
	r.ID = f.nextRID
	f.nextRID++
	f.runs = append(f.runs, *r)
	return nil
}

func (f *fakeReconStore) LatestRunsForDate(_ context.Context, day time.Time) ([]NostroReconRun, error) {
	f.mu2.Lock()
	defer f.mu2.Unlock()
	latest := map[int64]NostroReconRun{}
	for _, r := range f.runs {
		if !r.ReconDate.Equal(day) {
			continue
		}
		if ex, ok := latest[r.NostroAccountID]; !ok || r.ID > ex.ID {
			latest[r.NostroAccountID] = r
		}
	}
	out := []NostroReconRun{}
	for _, r := range latest {
		out = append(out, r)
	}
	return out, nil
}

func (f *fakeReconStore) InsertBreak(_ context.Context, b *NostroReconBreak) error {
	f.mu2.Lock()
	defer f.mu2.Unlock()
	b.ID = f.nextBID
	f.nextBID++
	f.breaks = append(f.breaks, *b)
	return nil
}

func (f *fakeReconStore) BreaksForDate(_ context.Context, day time.Time) ([]NostroReconBreak, error) {
	f.mu2.Lock()
	defer f.mu2.Unlock()
	var out []NostroReconBreak
	for _, b := range f.breaks {
		if b.ReconDate.Equal(day) {
			out = append(out, b)
		}
	}
	return out, nil
}

func (f *fakeReconStore) OpenBreaksFor(_ context.Context, accountID int64, day time.Time) ([]NostroReconBreak, error) {
	f.mu2.Lock()
	defer f.mu2.Unlock()
	var out []NostroReconBreak
	for _, b := range f.breaks {
		if b.NostroAccountID == accountID && b.ReconDate.Equal(day) &&
			(b.Status == NostroBreakOpen || b.Status == NostroBreakInvestigating) {
			out = append(out, b)
		}
	}
	return out, nil
}

func (f *fakeReconStore) ResolveBreak(_ context.Context, breakID int64, notes string, by int64, at time.Time) (bool, error) {
	f.mu2.Lock()
	defer f.mu2.Unlock()
	for i := range f.breaks {
		b := &f.breaks[i]
		if b.ID == breakID && (b.Status == NostroBreakOpen || b.Status == NostroBreakInvestigating) {
			b.Status = NostroBreakResolved
			b.ResolutionNotes = notes
			b.ResolvedBy = &by
			b.ResolvedAt = &at
			return true, nil
		}
	}
	return false, nil
}

func (f *fakeReconStore) AssignBreak(_ context.Context, breakID, actor int64, at time.Time) (bool, error) {
	f.mu2.Lock()
	defer f.mu2.Unlock()
	for i := range f.breaks {
		b := &f.breaks[i]
		if b.ID == breakID && b.Status == NostroBreakOpen {
			b.Status = NostroBreakInvestigating
			b.AssignedTo = &actor
			return true, nil
		}
	}
	return false, nil
}

func (f *fakeReconStore) AutoResolveForMovement(_ context.Context, movementID int64, at time.Time) (int, error) {
	f.mu2.Lock()
	defer f.mu2.Unlock()
	n := 0
	for i := range f.breaks {
		b := &f.breaks[i]
		if b.MovementID != nil && *b.MovementID == movementID &&
			(b.Status == NostroBreakOpen || b.Status == NostroBreakInvestigating) {
			b.Status = NostroBreakAutoResolved
			b.ResolvedAt = &at
			n++
		}
	}
	return n, nil
}

func boDay(v string) time.Time {
	t, _ := time.Parse("2006-01-02", v)
	return t
}

// ---------------------------------------------------------------------------
// §24 #21 threshold logic
// ---------------------------------------------------------------------------

func TestNostroThreshold_Rules(t *testing.T) {
	cases := []struct {
		diff, exp string
		want      bool
		note      string
	}{
		// Absolute arm — expected is large enough (20M → 0.01% = 2,000)
		// that only the $1,000 boundary discriminates.
		{"999", "20000000", false, "below $1,000"},
		{"1000", "20000000", false, "exactly $1,000 is not > $1,000"},
		{"1000.01", "20000000", true, "over $1,000"},
		{"-1001", "20000000", true, "abs() — negative breaches too"},
		// Percentage arm — expected small enough that 0.01% discriminates.
		{"11", "100000", true, "11 > 0.01% of 100,000 (=10)"},
		{"10", "100000", false, "exactly 0.01% boundary is not >"},
		{"0", "100000", false, "no diff"},
	}
	for i, c := range cases {
		got := nostroThresholdExceeded(decimal.RequireFromString(c.diff),
			decimal.RequireFromString(c.exp))
		if got != c.want {
			t.Fatalf("case %d %s: diff %s → %v, want %v",
				i, c.note, c.diff, got, c.want)
		}
	}
}

// ---------------------------------------------------------------------------
// Daily run: match categories, auto-resolution, P1 alert.
// ---------------------------------------------------------------------------

type reconFixture struct {
	store *fakeReconStore
	svc   *NostroReconService
	acct  *NostroAccount
	day   time.Time
}

func newReconFixture(t *testing.T) *reconFixture {
	t.Helper()
	store := newFakeReconStore()
	ns, err := NewNostroService(store)
	if err != nil {
		t.Fatal(err)
	}
	svc, err := NewNostroReconService(store, ns)
	if err != nil {
		t.Fatal(err)
	}
	acct, err := ns.CreateAccount(context.Background(), CreateAccountInput{
		Currency: "USD", BankName: "B", IBAN: "X"})
	if err != nil {
		t.Fatal(err)
	}
	return &reconFixture{store: store, svc: svc, acct: acct,
		day: boDay("2026-09-28")}
}

func (fx *reconFixture) entry(ref, dir, amt, narrative string) NostroStatementEntry {
	return NostroStatementEntry{
		NostroAccountID: fx.acct.ID, StatementDate: fx.day,
		SwiftReference: ref, Direction: dir,
		Amount: decimal.RequireFromString(amt), Currency: "USD",
		Narrative: narrative, Source: "MT940"}
}

func TestReconDaily_CleanRun(t *testing.T) {
	fx := newReconFixture(t)
	ctx := context.Background()
	// Movement + matching statement entry (ref+amount+date) → CLEAN.
	m := fx.store.addMovement(NostroMovement{
		SettlementInstructionID: 1, NostroAccountID: fx.acct.ID,
		Currency: "USD", Amount: decimal.NewFromInt(500),
		Direction: "CREDIT", Status: "POSTED",
		ConfirmationRef: "CONF-1", CreatedAt: fx.day})
	fx.store.entries[fx.acct.ID] = []NostroStatementEntry{
		fx.entry("CONF-1", "CREDIT", "500", "")}
	rep, err := fx.svc.RunDaily(ctx, fx.day, nil)
	if err != nil {
		t.Fatal(err)
	}
	if rep.AccountsReconciled != 1 || rep.Runs[0].Status != "CLEAN" ||
		rep.OpenBreaks != 0 || rep.ThresholdBreaches != 0 {
		t.Fatalf("clean run: %+v", rep)
	}
	_ = m
}

func TestReconDaily_AutoResolvePostsPendingMovement(t *testing.T) {
	fx := newReconFixture(t)
	ctx := context.Background()
	// PENDING movement (intent written, bank confirms) → the statement
	// match posts the balance and flips the movement POSTED.
	fx.acct.Balance = decimal.NewFromInt(1000)
	m := fx.store.addMovement(NostroMovement{
		SettlementInstructionID: 2, NostroAccountID: fx.acct.ID,
		Currency: "USD", Amount: decimal.NewFromInt(200),
		Direction: "DEBIT", Status: "PENDING",
		SwiftMessageID: "LEG-2", CreatedAt: fx.day})
	fx.store.entries[fx.acct.ID] = []NostroStatementEntry{
		fx.entry("LEG-2", "DEBIT", "200", "")}
	rep, err := fx.svc.RunDaily(ctx, fx.day, nil)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Runs[0].Status != "CLEAN" {
		t.Fatalf("auto-resolved run should be CLEAN: %+v", rep.Runs[0])
	}
	if fx.store.movements[m.ID].Status != "POSTED" {
		t.Fatal("matched movement must post")
	}
	if got := fx.store.accounts[fx.acct.ID].Balance.String(); got != "800" {
		t.Fatalf("balance = %s", got)
	}
}

func TestReconDaily_BreakCategories(t *testing.T) {
	fx := newReconFixture(t)
	ctx := context.Background()
	// Our side: two movements the bank never reported.
	fx.store.addMovement(NostroMovement{
		SettlementInstructionID: 10, NostroAccountID: fx.acct.ID,
		Currency: "USD", Amount: decimal.NewFromInt(100),
		Direction: "DEBIT", Status: "POSTED",
		SwiftMessageID: "LEG-A", CreatedAt: fx.day})
	// Bank side: fee charge (FEES), unreferenced credit (UNMATCHED),
	// an entry matching LEG-A's ref but a different amount
	// (AMOUNT_MISMATCH) and an entry matching LEG-B's ref with the same
	// amount but created on the adjacent day (TIMING).
	fx.store.addMovement(NostroMovement{
		SettlementInstructionID: 11, NostroAccountID: fx.acct.ID,
		Currency: "USD", Amount: decimal.NewFromInt(60),
		Direction: "CREDIT", Status: "POSTED",
		SwiftMessageID: "LEG-B", CreatedAt: fx.day.Add(-24 * time.Hour)})
	fx.store.entries[fx.acct.ID] = []NostroStatementEntry{
		fx.entry("FEE-SEP", "DEBIT", "25", "monthly commission charge"),
		fx.entry("", "CREDIT", "77", "unsolicited inflow"),
		fx.entry("LEG-A", "DEBIT", "95", ""),
		fx.entry("LEG-B", "CREDIT", "60", ""),
	}
	rep, err := fx.svc.RunDaily(ctx, fx.day, nil)
	if err != nil {
		t.Fatal(err)
	}
	cats := map[NostroBreakCategory]int{}
	for _, b := range rep.Breaks {
		cats[b.Category]++
	}
	if cats[NostroBreakFees] != 1 {
		t.Fatalf("FEES missing: %v", cats)
	}
	if cats[NostroBreakUnmatched] != 1 {
		t.Fatalf("UNMATCHED missing: %v", cats)
	}
	if cats[NostroBreakAmountMismatch] != 1 {
		t.Fatalf("AMOUNT_MISMATCH missing: %v", cats)
	}
	if cats[NostroBreakTiming] != 1 {
		t.Fatalf("TIMING missing (adjacent-day ref match): %v", cats)
	}
	if rep.Runs[0].Status == "CLEAN" {
		t.Fatal("breaks must mark the run non-clean")
	}
	// Every mismatch lands the P1.
	if len(fx.store.alerts) == 0 ||
		fx.store.alerts[len(fx.store.alerts)-1].Code != CodeNostroReconMismatch {
		t.Fatalf("P1 recon alert missing: %+v", fx.store.alerts)
	}
}

func TestReconDaily_MissingConfirmationBreak(t *testing.T) {
	fx := newReconFixture(t)
	ctx := context.Background()
	// We dispatched (movement exists); the bank statement has no trace.
	fx.store.addMovement(NostroMovement{
		SettlementInstructionID: 20, NostroAccountID: fx.acct.ID,
		Currency: "USD", Amount: decimal.NewFromInt(300),
		Direction: "DEBIT", Status: "POSTED",
		SwiftMessageID: "LEG-MISS", CreatedAt: fx.day})
	rep, err := fx.svc.RunDaily(ctx, fx.day, nil)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, b := range rep.Breaks {
		if b.Category == NostroBreakMissingConfirmation &&
			b.SwiftReference == "LEG-MISS" {
			found = true
		}
	}
	if !found {
		t.Fatalf("MISSING_CONFIRMATION break absent: %+v", rep.Breaks)
	}
}

func TestReconDaily_ThresholdBreachFlag(t *testing.T) {
	fx := newReconFixture(t)
	ctx := context.Background()
	// Our net 0; statement shows a 5000 credit we never recorded →
	// |diff|=5000 > 1000 → DISCREPANCY + threshold_breach.
	fx.store.entries[fx.acct.ID] = []NostroStatementEntry{
		fx.entry("GHOST", "CREDIT", "5000", "")}
	rep, err := fx.svc.RunDaily(ctx, fx.day, nil)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Runs[0].Status != "DISCREPANCY" || !rep.Runs[0].ThresholdBreach ||
		rep.ThresholdBreaches != 1 {
		t.Fatalf("threshold run: %+v", rep.Runs[0])
	}
}

func TestReconDaily_StatementSourcePolls(t *testing.T) {
	fx := newReconFixture(t)
	ctx := context.Background()
	polled := false
	fx.svc.WithStatements(NostroStatementSourceFunc(
		func(_ context.Context, acctID int64, d time.Time) ([]NostroStatementEntry, error) {
			polled = true
			if acctID != fx.acct.ID {
				return nil, fmt.Errorf("wrong acct %d", acctID)
			}
			return []NostroStatementEntry{
				fx.entry("P-1", "CREDIT", "10", "polled")}, nil
		}))
	if _, err := fx.svc.RunDaily(ctx, fx.day, nil); err != nil {
		t.Fatal(err)
	}
	if !polled {
		t.Fatal("statement source never polled")
	}
	if n := len(fx.store.entries[fx.acct.ID]); n != 1 {
		t.Fatalf("polled entries not ingested: %d", n)
	}
}

// NostroStatementSourceFunc adapts a func to the polling seam (tests).
type NostroStatementSourceFunc func(ctx context.Context, nostroAccountID int64, day time.Time) ([]NostroStatementEntry, error)

func (f NostroStatementSourceFunc) Poll(ctx context.Context, id int64, d time.Time) ([]NostroStatementEntry, error) {
	return f(ctx, id, d)
}

func TestReconReport_LatestRunAndBreaks(t *testing.T) {
	fx := newReconFixture(t)
	ctx := context.Background()
	fx.store.entries[fx.acct.ID] = []NostroStatementEntry{
		fx.entry("X", "DEBIT", "42", "")}
	if _, err := fx.svc.RunDaily(ctx, fx.day, nil); err != nil {
		t.Fatal(err)
	}
	rep, err := fx.svc.Report(ctx, fx.day)
	if err != nil {
		t.Fatal(err)
	}
	if rep.AccountsReconciled != 1 || len(rep.Breaks) != 1 ||
		rep.OpenBreaks != 1 {
		t.Fatalf("report: %+v", rep)
	}
}

func TestReconBreak_Workflow(t *testing.T) {
	fx := newReconFixture(t)
	ctx := context.Background()
	fx.store.entries[fx.acct.ID] = []NostroStatementEntry{
		fx.entry("X", "DEBIT", "42", "")}
	rep, err := fx.svc.RunDaily(ctx, fx.day, nil)
	if err != nil {
		t.Fatal(err)
	}
	brk := rep.Breaks[0]
	// OPEN → INVESTIGATING.
	got, err := fx.svc.AssignBreak(ctx, brk.ID, 42)
	if err != nil || got.Status != NostroBreakInvestigating {
		t.Fatalf("assign: %+v %v", got, err)
	}
	// INVESTIGATING → RESOLVED with notes.
	got, err = fx.svc.ResolveBreak(ctx, brk.ID, "bank fee — agreed", 42)
	if err != nil || got.Status != NostroBreakResolved {
		t.Fatalf("resolve: %+v %v", got, err)
	}
	// Terminal rows reject further transitions.
	if _, err := fx.svc.ResolveBreak(ctx, brk.ID, "again", 42); err == nil {
		t.Fatal("terminal break must reject resolve")
	}
	if _, err := fx.svc.ResolveBreak(ctx, 9999, "x", 42); err == nil {
		t.Fatal("unknown break must reject resolve")
	}
}

func TestReconService_FailClosedCtor(t *testing.T) {
	if _, err := NewNostroReconService(nil, nil); err == nil {
		t.Fatal("nil store/poster must fail closed")
	}
}

// ---------------------------------------------------------------------------
// PG-gated — recon tables + SQL.
// ---------------------------------------------------------------------------

func TestPgReconStore_Integration(t *testing.T) {
	ctx, pool := boTestPool(t)
	boSchema(t, ctx, pool)
	store := NewPgNostroStore(pool)
	ns, _ := NewNostroService(store)
	svc, err := NewNostroReconService(store, ns)
	if err != nil {
		t.Fatal(err)
	}
	acct, _ := ns.CreateAccount(ctx, CreateAccountInput{
		Currency: "EUR", BankName: "B", IBAN: "DE001"})
	d := boDay("2026-09-28")

	// Idempotent statement ingest.
	e := NostroStatementEntry{NostroAccountID: acct.ID, StatementDate: d,
		SwiftReference: "S-1", Direction: "CREDIT",
		Amount: decimal.NewFromInt(50), Currency: "EUR", Source: "POLL"}
	n, err := store.UpsertStatementEntries(ctx, []NostroStatementEntry{e, e})
	if err != nil || n != 1 {
		t.Fatalf("upsert n=%d err=%v", n, err)
	}
	rows, err := store.StatementEntriesFor(ctx, acct.ID, d)
	if err != nil || len(rows) != 1 {
		t.Fatalf("entries: %+v %v", rows, err)
	}

	// Matching movement → auto-resolved clean run.
	_, movID := seedLeg(t, ctx, pool, acct.ID, "EUR", "50", "CREDIT", "S-1")
	if _, err := pool.Exec(ctx,
		`UPDATE nostro_movements
		    SET confirmation_ref = 'S-1', created_at = $1 WHERE id = $2`,
		d, movID); err != nil {
		t.Fatal(err)
	}
	rep, err := svc.RunDaily(ctx, d, nil)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Runs[0].Status != "CLEAN" {
		t.Fatalf("pg recon run: %+v", rep.Runs[0])
	}
	var movStatus string
	if err := pool.QueryRow(ctx,
		`SELECT status::text FROM nostro_movements WHERE id = $1`,
		movID).Scan(&movStatus); err != nil || movStatus != "POSTED" {
		t.Fatalf("movement must POST on auto-resolution: %q %v", movStatus, err)
	}

	// A ghost statement debit → break + P1 row on the durable trail.
	if _, err := store.UpsertStatementEntries(ctx, []NostroStatementEntry{{
		NostroAccountID: acct.ID, StatementDate: d, SwiftReference: "G",
		Direction: "DEBIT", Amount: decimal.NewFromInt(2000),
		Currency: "EUR", Source: "MT940"}}); err != nil {
		t.Fatal(err)
	}
	rep, err = svc.RunDaily(ctx, d, nil)
	if err != nil {
		t.Fatal(err)
	}
	var run NostroReconRun
	for _, r := range rep.Runs {
		run = r
	}
	if run.Status != "DISCREPANCY" || !run.ThresholdBreach {
		t.Fatalf("run: %+v", run)
	}
	var code string
	if err := pool.QueryRow(ctx, `
		SELECT code FROM funding_ops_alerts
		 WHERE code='NOSTRO_RECON_MISMATCH' AND severity='P1'
		 ORDER BY id DESC LIMIT 1`).Scan(&code); err != nil {
		t.Fatalf("P1 recon alert missing: %v", err)
	}
	var status string
	if err := pool.QueryRow(ctx, `
		SELECT status::text FROM nostro_recon_breaks
		 WHERE nostro_account_id = $1 ORDER BY id DESC LIMIT 1`,
		acct.ID).Scan(&status); err != nil || status != "OPEN" {
		t.Fatalf("break: status=%q err=%v", status, err)
	}
}
