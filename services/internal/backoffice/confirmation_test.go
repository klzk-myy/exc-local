// Task 24.3.3 tests — MT900/MT910 confirmation processing, the
// SETTLED transition + balance post, idempotent replay, and the
// two-business-day timeout sweep.
package backoffice

import (
	"context"
	"testing"
	"time"

	"exchange/pkg/decimal"
	excerrors "exchange/pkg/errors"
)

// fakeConfirmer records ConfirmSettlement calls and returns a settled
// projection.
type fakeConfirmer struct {
	calls  []int64
	status string
	err    error
}

func (f *fakeConfirmer) ConfirmSettlement(_ context.Context, id int64, ref string) (*ConfirmedInstruction, error) {
	f.calls = append(f.calls, id)
	if f.err != nil {
		return nil, f.err
	}
	st := f.status
	if st == "" {
		st = "SETTLED"
	}
	return &ConfirmedInstruction{Status: st}, nil
}

// fakeRecorder captures journal writes (SwiftRecorder seam).
type fakeRecorder struct{ msgs []SwiftMessage }

func (f *fakeRecorder) Record(_ context.Context, m SwiftMessage) (*SwiftMessage, error) {
	m.ID = int64(len(f.msgs) + 1)
	m.CreatedAt = time.Now().UTC()
	f.msgs = append(f.msgs, m)
	return &m, nil
}

// fakeAlerter captures OpsAlert pages.
type fakeAlerter struct{ pages []OpsAlert }

func (f *fakeAlerter) Raise(_ context.Context, a OpsAlert) error {
	f.pages = append(f.pages, a)
	return nil
}

// fakeConfStore is an in-memory ConfirmationStore.
type fakeConfStore struct {
	fakeNostroStore
	byRef map[string]InstructionRow
}

func newFakeConfStore() *fakeConfStore {
	return &fakeConfStore{
		fakeNostroStore: *newFakeNostroStore(),
		byRef:           map[string]InstructionRow{},
	}
}

func (f *fakeConfStore) InstructionBySwiftRef(_ context.Context, ref string) (*InstructionRow, error) {
	if r, ok := f.byRef[ref]; ok {
		cp := r
		return &cp, nil
	}
	return nil, nil
}

func (f *fakeConfStore) StaleDispatched(_ context.Context, before time.Time, limit int) ([]InstructionRow, error) {
	var out []InstructionRow
	for _, r := range f.byRef {
		if r.Status == "PENDING" && r.DispatchedAt != nil &&
			!r.DispatchedAt.After(before) {
			out = append(out, r)
		}
	}
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func confFixture(t *testing.T) (*fakeConfStore, *ConfirmationService,
	*fakeConfirmer, *fakeRecorder, *fakeAlerter) {
	t.Helper()
	store := newFakeConfStore()
	ns, err := NewNostroService(store)
	if err != nil {
		t.Fatal(err)
	}
	conf := &fakeConfirmer{}
	rec := &fakeRecorder{}
	al := &fakeAlerter{}
	svc, err := NewConfirmationService(store, conf, ns)
	if err != nil {
		t.Fatal(err)
	}
	svc.WithRecorder(rec).WithAlerter(al)
	return store, svc, conf, rec, al
}

func mtConf(ref, ccy, amt string) SwiftConfirmation {
	return SwiftConfirmation{
		MessageType: MsgMT910, Reference: "BANK-" + ref, RelatedRef: ref,
		Currency: ccy, Amount: decimal.RequireFromString(amt),
		ValueDate: boDay("2026-09-28"), ReceivedAt: time.Now().UTC()}
}

func TestConfirmation_MT910_SettlesAndPosts(t *testing.T) {
	store, svc, conf, rec, _ := confFixture(t)
	ctx := context.Background()
	ns, _ := NewNostroService(store)
	acct, _ := ns.CreateAccount(ctx, CreateAccountInput{
		Currency: "USD", BankName: "B", IBAN: "X"})
	store.byRef["LEG-9"] = InstructionRow{
		ID: 9, Currency: "USD", Amount: decimal.NewFromInt(150),
		Direction: "RECEIVE", Status: "PENDING",
		SwiftMessageID: strptr("LEG-9")}
	// The Phase-03 intent row the poster applies.
	m := store.addMovement(NostroMovement{
		SettlementInstructionID: 9, NostroAccountID: acct.ID,
		Currency: "USD", Amount: decimal.NewFromInt(150),
		Direction: "CREDIT", Status: "PENDING"})

	res, err := svc.ProcessConfirmation(ctx, mtConf("LEG-9", "USD", "150"))
	if err != nil {
		t.Fatal(err)
	}
	if res.InstructionID != 9 || res.Status != "SETTLED" ||
		!res.MovementPosted || res.AlreadySettled {
		t.Fatalf("result: %+v", res)
	}
	if res.BalanceAfter == nil || *res.BalanceAfter != "150" {
		t.Fatalf("balance_after: %+v", res.BalanceAfter)
	}
	if len(conf.calls) != 1 || conf.calls[0] != 9 {
		t.Fatalf("confirmer calls: %v", conf.calls)
	}
	// Journal wrote the inbound MT910.
	if len(rec.msgs) != 1 || rec.msgs[0].MessageType != "MT910" ||
		rec.msgs[0].Direction != SwiftIn {
		t.Fatalf("journal: %+v", rec.msgs)
	}
	if store.movements[m.ID].Status != "POSTED" {
		t.Fatal("movement must be POSTED")
	}
}

func TestConfirmation_MT900_DebitPath(t *testing.T) {
	store, svc, conf, _, _ := confFixture(t)
	ctx := context.Background()
	ns, _ := NewNostroService(store)
	acct, _ := ns.CreateAccount(ctx, CreateAccountInput{
		Currency: "EUR", BankName: "B", IBAN: "X"})
	acct.Balance = decimal.NewFromInt(500)
	store.byRef["LEG-D"] = InstructionRow{
		ID: 12, Currency: "EUR", Amount: decimal.NewFromInt(200),
		Direction: "PAY", Status: "PENDING",
		SwiftMessageID: strptr("LEG-D")}
	store.addMovement(NostroMovement{
		SettlementInstructionID: 12, NostroAccountID: acct.ID,
		Currency: "EUR", Amount: decimal.NewFromInt(200),
		Direction: "DEBIT", Status: "PENDING"})

	c := mtConf("LEG-D", "EUR", "200")
	c.MessageType = MsgMT900
	res, err := svc.ProcessConfirmation(ctx, c)
	if err != nil {
		t.Fatal(err)
	}
	if *res.BalanceAfter != "300" {
		t.Fatalf("debit post balance: %v", *res.BalanceAfter)
	}
	if len(conf.calls) != 1 {
		t.Fatal("confirmer not invoked")
	}
}

func TestConfirmation_ReplayIsIdempotent(t *testing.T) {
	store, svc, conf, _, _ := confFixture(t)
	ctx := context.Background()
	ns, _ := NewNostroService(store)
	acct, _ := ns.CreateAccount(ctx, CreateAccountInput{
		Currency: "USD", BankName: "B", IBAN: "X"})
	store.byRef["LEG-7"] = InstructionRow{
		ID: 7, Currency: "USD", Amount: decimal.NewFromInt(80),
		Direction: "RECEIVE", Status: "SETTLED", // bank re-sent the MT910
		SwiftMessageID: strptr("LEG-7")}
	m := store.addMovement(NostroMovement{
		SettlementInstructionID: 7, NostroAccountID: acct.ID,
		Currency: "USD", Amount: decimal.NewFromInt(80),
		Direction: "CREDIT", Status: "POSTED"})
	acct.Balance = decimal.NewFromInt(80)

	res, err := svc.ProcessConfirmation(ctx, mtConf("LEG-7", "USD", "80"))
	if err != nil {
		t.Fatal(err)
	}
	if !res.AlreadySettled || res.MovementPosted {
		t.Fatalf("replay must not re-apply: %+v", res)
	}
	if got := store.accounts[acct.ID].Balance.String(); got != "80" {
		t.Fatalf("double-applied: %s", got)
	}
	_ = conf
	_ = m
}

func TestConfirmation_Validation(t *testing.T) {
	_, svc, _, _, _ := confFixture(t)
	ctx := context.Background()
	bad := []SwiftConfirmation{
		{MessageType: "MT103", RelatedRef: "X", Currency: "USD",
			Amount: decimal.NewFromInt(1)}, // wrong type
		{MessageType: MsgMT910, Currency: "USD",
			Amount: decimal.NewFromInt(1)}, // no ref at all
		{MessageType: MsgMT910, RelatedRef: "R", Currency: "usd!",
			Amount: decimal.NewFromInt(1)}, // bad ccy
		{MessageType: MsgMT910, RelatedRef: "R", Currency: "USD",
			Amount: decimal.Zero}, // non-positive
	}
	for i, c := range bad {
		if _, err := svc.ProcessConfirmation(ctx, c); err == nil ||
			excerrors.CodeOf(err) != "INVALID_REQUEST" {
			t.Fatalf("case %d: %v", i, err)
		}
	}
}

func TestConfirmation_UnknownRef(t *testing.T) {
	_, svc, _, _, _ := confFixture(t)
	_, err := svc.ProcessConfirmation(context.Background(),
		mtConf("NOPE", "USD", "10"))
	if err == nil || excerrors.CodeOf(err) != "NOT_FOUND" {
		t.Fatalf("unknown ref: %v", err)
	}
}

func TestConfirmation_CurrencyMismatch(t *testing.T) {
	store, svc, _, _, _ := confFixture(t)
	store.byRef["LEG-X"] = InstructionRow{
		ID: 3, Currency: "EUR", Amount: decimal.NewFromInt(10),
		Status: "PENDING", SwiftMessageID: strptr("LEG-X")}
	_, err := svc.ProcessConfirmation(context.Background(),
		mtConf("LEG-X", "USD", "10"))
	if err == nil || excerrors.CodeOf(err) != "INVALID_REQUEST" {
		t.Fatalf("ccy mismatch: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Timeout sweep — >2 business days unconfirmed → deduplicated P2.
// ---------------------------------------------------------------------------

// 2026-09-25 is a Friday; two business days later is Tuesday 2026-09-29
// (Sat/Sun skipped, weekends-only fallback calendar).
var (
	fri = time.Date(2026, 9, 25, 10, 0, 0, 0, time.UTC) // dispatch
	mon = time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC) // 1 biz day in
	wed = time.Date(2026, 9, 30, 0, 0, 1, 0, time.UTC)  // past deadline
)

func TestSweepOverdue_TwoBusinessDays(t *testing.T) {
	store, svc, _, _, al := confFixture(t)
	store.byRef["LEG-O"] = InstructionRow{
		ID: 5, TradeID: 5, Currency: "USD",
		Amount: decimal.NewFromInt(1000), Direction: "PAY",
		Status: "PENDING", SwiftMessageID: strptr("LEG-O"),
		DispatchedAt: &fri}
	ctx := context.Background()

	// Monday noon — still inside the window (deadline Tue EOD).
	svc.WithClock(func() time.Time { return mon })
	rep, err := svc.SweepOverdue(ctx, 100)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Overdue != 0 || rep.Alerted != 0 {
		t.Fatalf("inside window: %+v", rep)
	}

	// Wednesday — past the two-business-day deadline → P2, dedup-keyed.
	svc.WithClock(func() time.Time { return wed })
	rep, err = svc.SweepOverdue(ctx, 100)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Overdue != 1 || rep.Alerted != 1 || len(store.alerts) != 1 ||
		store.alerts[0].Code != CodeConfirmationOverdue ||
		store.alerts[0].Severity != "P2" {
		t.Fatalf("overdue sweep: %+v alerts=%+v", rep, store.alerts)
	}
	if len(al.pages) != 1 || al.pages[0].Severity != "P2" {
		t.Fatalf("page: %+v", al.pages)
	}

	// Re-sweep — the OPEN dedup-keyed alert blocks a duplicate.
	rep, err = svc.SweepOverdue(ctx, 100)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Alerted != 0 || rep.AlreadyUp != 1 || len(store.alerts) != 1 {
		t.Fatalf("dedup resweep: %+v alerts=%d", rep, len(store.alerts))
	}
}

func TestSweepOverdue_HolidayCalendarWired(t *testing.T) {
	store, svc, _, _, _ := confFixture(t)
	// Holiday-aware calendar: Monday 2026-09-28 is a USD holiday → the
	// second business day lands Wednesday → still inside on Wed.
	svc.WithCalendar(bizCalFunc(func(ccy string, d time.Time) bool {
		if d.Equal(boDay("2026-09-28")) {
			return false // holiday
		}
		wd := d.Weekday()
		return wd != time.Saturday && wd != time.Sunday
	}))
	store.byRef["LEG-H"] = InstructionRow{
		ID: 6, Currency: "USD", Amount: decimal.NewFromInt(10),
		Direction: "PAY", Status: "PENDING",
		SwiftMessageID: strptr("LEG-H"), DispatchedAt: &fri}
	svc.WithClock(func() time.Time { return wed })
	rep, err := svc.SweepOverdue(context.Background(), 100)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Overdue != 0 {
		t.Fatalf("holiday-aware deadline: %+v", rep)
	}
}

type bizCalFunc func(ccy string, d time.Time) bool

func (f bizCalFunc) IsBusinessDay(ccy string, d time.Time) bool { return f(ccy, d) }

func TestConfirmationService_FailClosedCtor(t *testing.T) {
	if _, err := NewConfirmationService(nil, nil, nil); err == nil {
		t.Fatal("nil deps must fail closed")
	}
}

func strptr(s string) *string { return &s }

// ---------------------------------------------------------------------------
// PG-gated — instruction resolution + stale scan.
// ---------------------------------------------------------------------------

func TestPgConfirmationStore_Integration(t *testing.T) {
	ctx, pool := boTestPool(t)
	boSchema(t, ctx, pool)
	store := NewPgNostroStore(pool)
	ns, _ := NewNostroService(store)
	acct, _ := ns.CreateAccount(ctx, CreateAccountInput{
		Currency: "USD", BankName: "B", IBAN: "X"})
	legID, _ := seedLeg(t, ctx, pool, acct.ID, "USD", "75", "CREDIT", "REF-C1")

	leg, err := store.InstructionBySwiftRef(ctx, "REF-C1")
	if err != nil || leg == nil || leg.ID != legID {
		t.Fatalf("resolve: %+v %v", leg, err)
	}
	miss, err := store.InstructionBySwiftRef(ctx, "NOPE")
	if err != nil || miss != nil {
		t.Fatalf("miss: %+v %v", miss, err)
	}

	stale, err := store.StaleDispatched(ctx, time.Now().UTC().Add(time.Hour), 10)
	if err != nil || len(stale) != 1 {
		t.Fatalf("stale: %+v %v", stale, err)
	}

	// Full confirm path on real PG through the fake confirmer seam.
	conf := &fakeConfirmer{}
	tracker, err := NewSwiftTracker(store)
	if err != nil {
		t.Fatal(err)
	}
	svc, err := NewConfirmationService(store, conf, ns)
	if err != nil {
		t.Fatal(err)
	}
	svc.WithRecorder(tracker)
	res, err := svc.ProcessConfirmation(ctx, mtConf("REF-C1", "USD", "75"))
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != "SETTLED" || !res.MovementPosted ||
		res.BalanceAfter == nil || *res.BalanceAfter != "75" {
		t.Fatalf("pg confirm: %+v", res)
	}
	// Idempotent on the store side too.
	if _, err := svc.ProcessConfirmation(ctx,
		mtConf("REF-C1", "USD", "75")); err != nil {
		t.Fatal(err)
	}
	var bal string
	if err := pool.QueryRow(ctx,
		`SELECT balance::text FROM nostro_accounts WHERE id=$1`,
		acct.ID).Scan(&bal); err != nil {
		t.Fatal(err)
	}
	if d := decimal.RequireFromString(bal); !d.Equal(decimal.NewFromInt(75)) {
		t.Fatalf("double-applied replay: %s", bal)
	}
	var n int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM swift_messages WHERE message_type='MT910'`).
		Scan(&n); err != nil || n != 2 {
		t.Fatalf("journal rows=%d err=%v", n, err)
	}
}
