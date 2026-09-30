// Task 24.3.1 tests — account registry + the movement poster.
// PG-gated coverage lives at the bottom (EXC_PG_TEST).
package backoffice

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"exchange/pkg/decimal"
	excerrors "exchange/pkg/errors"
)

// ---------------------------------------------------------------------------
// In-memory NostroStore fake.
// ---------------------------------------------------------------------------

type fakeNostroStore struct {
	mu        sync.Mutex
	accounts  map[int64]*NostroAccount
	movements map[int64]*NostroMovement
	alerts    []OpsAlertRow
	nextID    int64
}

func newFakeNostroStore() *fakeNostroStore {
	return &fakeNostroStore{
		accounts:  map[int64]*NostroAccount{},
		movements: map[int64]*NostroMovement{},
		nextID:    1,
	}
}

func (f *fakeNostroStore) InsertAccount(_ context.Context, a *NostroAccount) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, ex := range f.accounts {
		if ex.Currency == a.Currency && ex.BankCode == a.BankCode &&
			ex.AccountNumber == a.AccountNumber && ex.IBAN == a.IBAN {
			return excerrors.New(CodeNostroAccountExists, "dup")
		}
	}
	a.ID = f.nextID
	f.nextID++
	a.CreatedAt = time.Now().UTC()
	a.UpdatedAt = a.CreatedAt
	f.accounts[a.ID] = a
	return nil
}

func (f *fakeNostroStore) ListAccounts(_ context.Context, flt AccountFilter) ([]NostroAccount, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []NostroAccount
	for _, a := range f.accounts {
		if flt.Currency != "" && a.Currency != flt.Currency {
			continue
		}
		if flt.Role != "" && a.Role != flt.Role {
			continue
		}
		if flt.Status != "" && a.Status != flt.Status {
			continue
		}
		out = append(out, *a)
	}
	return out, nil
}

func (f *fakeNostroStore) PendingMovements(_ context.Context, limit int) ([]NostroMovement, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []NostroMovement
	for _, m := range f.movements {
		if m.Status == "PENDING" {
			out = append(out, *m)
		}
	}
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func (f *fakeNostroStore) MovementForInstruction(_ context.Context, instructionID int64) (*NostroMovement, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, m := range f.movements {
		if m.SettlementInstructionID == instructionID {
			cp := *m
			return &cp, nil
		}
	}
	return nil, nil
}

func (f *fakeNostroStore) addMovement(m NostroMovement) *NostroMovement {
	f.mu.Lock()
	defer f.mu.Unlock()
	m.ID = f.nextID
	f.nextID++
	if m.CreatedAt.IsZero() {
		m.CreatedAt = time.Now().UTC()
	}
	f.movements[m.ID] = &m
	return &m
}

// --- recon seam (fakeNostroStore also satisfies NostroReconStore) ---

func (f *fakeNostroStore) HasOpenAlert(_ context.Context, code, dedupKey string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, a := range f.alerts {
		if a.Code == code && strings.Contains(string(a.Detail), dedupKey) {
			return true, nil
		}
	}
	return false, nil
}

func (f *fakeNostroStore) UpsertStatementEntries(_ context.Context, entries []NostroStatementEntry) (int, error) {
	return 0, errors.New("fake store: statement upsert not wired")
}

func (f *fakeNostroStore) StatementEntriesFor(_ context.Context, accountID int64, day time.Time) ([]NostroStatementEntry, error) {
	return nil, errors.New("fake store: statement entries not wired")
}

func (f *fakeNostroStore) MovementsInWindow(_ context.Context, accountID int64, from, to time.Time) ([]NostroMovement, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []NostroMovement
	for _, m := range f.movements {
		if m.NostroAccountID == accountID &&
			!m.CreatedAt.Before(from) && m.CreatedAt.Before(to) {
			out = append(out, *m)
		}
	}
	return out, nil
}

func (f *fakeNostroStore) InsertReconRun(context.Context, *NostroReconRun) error {
	return errors.New("fake store: recon runs not wired")
}
func (f *fakeNostroStore) LatestRunsForDate(context.Context, time.Time) ([]NostroReconRun, error) {
	return nil, errors.New("fake store: recon runs not wired")
}
func (f *fakeNostroStore) InsertBreak(context.Context, *NostroReconBreak) error {
	return errors.New("fake store: breaks not wired")
}
func (f *fakeNostroStore) BreaksForDate(context.Context, time.Time) ([]NostroReconBreak, error) {
	return nil, errors.New("fake store: breaks not wired")
}
func (f *fakeNostroStore) OpenBreaksFor(context.Context, int64, time.Time) ([]NostroReconBreak, error) {
	return nil, errors.New("fake store: breaks not wired")
}
func (f *fakeNostroStore) ResolveBreak(context.Context, int64, string, int64, time.Time) (bool, error) {
	return false, errors.New("fake store: breaks not wired")
}
func (f *fakeNostroStore) AssignBreak(context.Context, int64, int64, time.Time) (bool, error) {
	return false, errors.New("fake store: breaks not wired")
}
func (f *fakeNostroStore) AutoResolveForMovement(context.Context, int64, time.Time) (int, error) {
	return 0, nil
}

func (f *fakeNostroStore) InTx(ctx context.Context, fn func(ctx context.Context, tx NostroTx) error) error {
	return fn(ctx, fakeNostroTx{f: f})
}

type fakeNostroTx struct{ f *fakeNostroStore }

func (t fakeNostroTx) MovementForUpdate(_ context.Context, id int64) (*NostroMovement, error) {
	t.f.mu.Lock()
	defer t.f.mu.Unlock()
	if m, ok := t.f.movements[id]; ok {
		cp := *m
		return &cp, nil
	}
	return nil, nil
}

func (t fakeNostroTx) AccountForUpdate(_ context.Context, id int64) (*NostroAccount, error) {
	t.f.mu.Lock()
	defer t.f.mu.Unlock()
	if a, ok := t.f.accounts[id]; ok {
		cp := *a
		return &cp, nil
	}
	return nil, nil
}

func (t fakeNostroTx) ApplyBalance(_ context.Context, accountID int64, delta decimal.Decimal) (decimal.Decimal, error) {
	t.f.mu.Lock()
	defer t.f.mu.Unlock()
	a := t.f.accounts[accountID]
	if a == nil {
		return decimal.Zero, errors.New("account gone")
	}
	a.Balance = a.Balance.Add(delta)
	return a.Balance, nil
}

func (t fakeNostroTx) MarkMovementPosted(_ context.Context, id int64, at time.Time) (bool, error) {
	t.f.mu.Lock()
	defer t.f.mu.Unlock()
	m := t.f.movements[id]
	if m == nil || m.Status != "PENDING" {
		return false, nil
	}
	m.Status = "POSTED"
	m.PostedAt = &at
	return true, nil
}

func (t fakeNostroTx) InsertAlert(_ context.Context, a OpsAlertRow) (int64, error) {
	t.f.mu.Lock()
	defer t.f.mu.Unlock()
	t.f.alerts = append(t.f.alerts, a)
	return int64(len(t.f.alerts)), nil
}

// ---------------------------------------------------------------------------
// 24.3.1 — account management
// ---------------------------------------------------------------------------

func TestCreateAccount_NostroAndVostro(t *testing.T) {
	svc, err := NewNostroService(newFakeNostroStore())
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	// NOSTRO — our USD account held at the correspondent bank.
	n, err := svc.CreateAccount(ctx, CreateAccountInput{
		Currency: "usd", BankName: "Deutsche Bank",
		BankCode: "DEUTDEFF", AccountNumber: "DE-NOSTRO-1",
	})
	if err != nil {
		t.Fatalf("nostro create: %v", err)
	}
	if n.Role != RoleNostro || n.Status != "ACTIVE" || n.Currency != "USD" {
		t.Fatalf("unexpected account: %+v", n)
	}
	// VOSTRO — the correspondent's EUR account held with us.
	v, err := svc.CreateAccount(ctx, CreateAccountInput{
		Currency: "EUR", BankName: "Deutsche Bank",
		BankCode: "DEUTDEFF", IBAN: "DE89370400440532013000",
		Role: RoleVostro,
	})
	if err != nil {
		t.Fatalf("vostro create: %v", err)
	}
	if v.Role != RoleVostro {
		t.Fatalf("role = %q, want VOSTRO", v.Role)
	}
}

func TestCreateAccount_Validation(t *testing.T) {
	svc, _ := NewNostroService(newFakeNostroStore())
	cases := []CreateAccountInput{
		{Currency: "US", BankName: "B", IBAN: "X"},             // bad ccy
		{Currency: "USD", BankName: "", IBAN: "X"},             // no bank
		{Currency: "USD", BankName: "B"},                       // no locator
		{Currency: "USD", BankName: "B", IBAN: "X", Role: "X"}, // bad role
	}
	for i, in := range cases {
		if _, err := svc.CreateAccount(context.Background(), in); err == nil {
			t.Fatalf("case %d: expected INVALID_REQUEST", i)
		}
	}
}

func TestCreateAccount_DuplicateRejected(t *testing.T) {
	store := newFakeNostroStore()
	svc, _ := NewNostroService(store)
	in := CreateAccountInput{Currency: "USD", BankName: "B",
		BankCode: "BK", AccountNumber: "1"}
	if _, err := svc.CreateAccount(context.Background(), in); err != nil {
		t.Fatal(err)
	}
	_, err := svc.CreateAccount(context.Background(), in)
	if err == nil || excerrors.CodeOf(err) != CodeNostroAccountExists {
		t.Fatalf("duplicate create: %v", err)
	}
}

func TestListAccounts_Filters(t *testing.T) {
	store := newFakeNostroStore()
	svc, _ := NewNostroService(store)
	ctx := context.Background()
	mk := func(ccy, bank string, role AccountRole) {
		if _, err := svc.CreateAccount(ctx, CreateAccountInput{
			Currency: ccy, BankName: bank, IBAN: ccy + "-" + bank,
			Role: role}); err != nil {
			t.Fatal(err)
		}
	}
	mk("USD", "B1", RoleNostro)
	mk("EUR", "B1", RoleNostro)
	mk("USD", "B2", RoleVostro)

	all, _ := svc.ListAccounts(ctx, AccountFilter{})
	if len(all) != 3 {
		t.Fatalf("all = %d", len(all))
	}
	usd, _ := svc.ListAccounts(ctx, AccountFilter{Currency: "USD"})
	if len(usd) != 2 {
		t.Fatalf("usd = %d", len(usd))
	}
	vostro, _ := svc.ListAccounts(ctx, AccountFilter{Role: RoleVostro})
	if len(vostro) != 1 || vostro[0].BankName != "B2" {
		t.Fatalf("vostro filter: %+v", vostro)
	}
	// Empty result is a legit empty list (service non-nil ⇒ real data).
	none, _ := svc.ListAccounts(ctx, AccountFilter{Currency: "JPY"})
	if none == nil || len(none) != 0 {
		t.Fatalf("empty filter result: %+v", none)
	}
}

func TestNostroDecimalStrings_JSON(t *testing.T) {
	store := newFakeNostroStore()
	svc, _ := NewNostroService(store)
	a, err := svc.CreateAccount(context.Background(), CreateAccountInput{
		Currency: "USD", BankName: "B", IBAN: "X"})
	if err != nil {
		t.Fatal(err)
	}
	a.Balance = decimal.RequireFromString("1234.56780000")
	body, err := json.Marshal(a)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), `"balance":"1234.5678"`) {
		t.Fatalf("balance must serialize as a decimal string: %s", body)
	}
}

// ---------------------------------------------------------------------------
// 24.3.1 — movement poster: real-time balance tracking, idempotency,
// overdraft alerting.
// ---------------------------------------------------------------------------

func TestPostMovement_CreditAndDebit(t *testing.T) {
	store := newFakeNostroStore()
	svc, _ := NewNostroService(store)
	ctx := context.Background()
	acct, _ := svc.CreateAccount(ctx, CreateAccountInput{
		Currency: "USD", BankName: "B", IBAN: "X"})
	acct.Balance = decimal.NewFromInt(1000)

	cr := store.addMovement(NostroMovement{NostroAccountID: acct.ID,
		Currency: "USD", Amount: decimal.NewFromInt(250), Direction: "CREDIT",
		Status: "PENDING"})
	res, err := svc.PostMovement(ctx, cr.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Posted || res.BalanceAfter.String() != "1250" {
		t.Fatalf("credit post: %+v", res)
	}

	db := store.addMovement(NostroMovement{NostroAccountID: acct.ID,
		Currency: "USD", Amount: decimal.NewFromInt(400), Direction: "DEBIT",
		Status: "PENDING"})
	res, err = svc.PostMovement(ctx, db.ID)
	if err != nil {
		t.Fatal(err)
	}
	if res.BalanceAfter.String() != "850" || res.Overdrawn {
		t.Fatalf("debit post: %+v", res)
	}
	if got := store.accounts[acct.ID].Balance.String(); got != "850" {
		t.Fatalf("account balance = %s", got)
	}
}

func TestPostMovement_IdempotentReplay(t *testing.T) {
	store := newFakeNostroStore()
	svc, _ := NewNostroService(store)
	ctx := context.Background()
	acct, _ := svc.CreateAccount(ctx, CreateAccountInput{
		Currency: "USD", BankName: "B", IBAN: "X"})
	acct.Balance = decimal.NewFromInt(100)
	m := store.addMovement(NostroMovement{NostroAccountID: acct.ID,
		Currency: "USD", Amount: decimal.NewFromInt(50), Direction: "DEBIT",
		Status: "PENDING"})
	if _, err := svc.PostMovement(ctx, m.ID); err != nil {
		t.Fatal(err)
	}
	res, err := svc.PostMovement(ctx, m.ID) // replay
	if err != nil {
		t.Fatal(err)
	}
	if res.Posted {
		t.Fatal("replayed post must report Posted=false")
	}
	if got := store.accounts[acct.ID].Balance.String(); got != "50" {
		t.Fatalf("double-applied balance: %s", got)
	}
}

func TestPostMovement_OverdrawnAlerts(t *testing.T) {
	store := newFakeNostroStore()
	svc, _ := NewNostroService(store)
	ctx := context.Background()
	acct, _ := svc.CreateAccount(ctx, CreateAccountInput{
		Currency: "USD", BankName: "B", IBAN: "X"})
	acct.Balance = decimal.NewFromInt(100)
	m := store.addMovement(NostroMovement{NostroAccountID: acct.ID,
		Currency: "USD", Amount: decimal.NewFromInt(400), Direction: "DEBIT",
		Status: "PENDING"})
	res, err := svc.PostMovement(ctx, m.ID)
	if err != nil {
		t.Fatal(err)
	}
	// The payment already happened at the correspondent — it POSTS,
	// but the P1 NOSTRO_OVERDRAWN alert lands on the durable trail.
	if !res.Posted || !res.Overdrawn {
		t.Fatalf("overdrawn post: %+v", res)
	}
	if len(store.alerts) != 1 || store.alerts[0].Code != CodeNostroOverdrawn ||
		store.alerts[0].Severity != "P1" {
		t.Fatalf("alerts: %+v", store.alerts)
	}
}

func TestPostMovement_MissingRows(t *testing.T) {
	svc, _ := NewNostroService(newFakeNostroStore())
	if _, err := svc.PostMovement(context.Background(), 99); err == nil {
		t.Fatal("missing movement must fail")
	}
}

func TestPostMovementForInstruction_NoMovementIsNoop(t *testing.T) {
	svc, _ := NewNostroService(newFakeNostroStore())
	res, err := svc.PostMovementForInstruction(context.Background(), 77)
	if err != nil || res.Posted {
		t.Fatalf("no-movement post: %+v %v", res, err)
	}
}

func TestPostPendingMovements_Drains(t *testing.T) {
	store := newFakeNostroStore()
	svc, _ := NewNostroService(store)
	ctx := context.Background()
	acct, _ := svc.CreateAccount(ctx, CreateAccountInput{
		Currency: "EUR", BankName: "B", IBAN: "X"})
	for i := 0; i < 3; i++ {
		store.addMovement(NostroMovement{NostroAccountID: acct.ID,
			Currency: "EUR", Amount: decimal.NewFromInt(10),
			Direction: "CREDIT", Status: "PENDING"})
	}
	posted, errs, err := svc.PostPendingMovements(ctx, 10)
	if err != nil || len(errs) != 0 || posted != 3 {
		t.Fatalf("drain: posted=%d errs=%v err=%v", posted, errs, err)
	}
	if got := store.accounts[acct.ID].Balance.String(); got != "30" {
		t.Fatalf("balance = %s", got)
	}
}
