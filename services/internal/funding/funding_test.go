// Pure unit tests for the funding package — no infrastructure.
// Integration coverage (Postgres + Redis + real LedgerService) lives in
// funding_integration_test.go, gated by EXC_PG_TEST=1.
package funding

import (
	"context"
	stderrors "errors"
	"fmt"
	"testing"
	"time"

	"exchange/internal/accounts"
	"exchange/internal/ledger"
	"exchange/pkg/decimal"
	excerrors "exchange/pkg/errors"
)

// ---------------------------------------------------------------------------
// Fakes
// ---------------------------------------------------------------------------

// fakeStore embeds the Store interface — unstubbed methods panic with nil
// derefs if a test path unexpectedly reaches them (fail loud, not silent).
type fakeStore struct {
	Store
	meta       map[int64]*AccountMeta
	userAccts  map[int64][]int64
	transfers  map[int64]*TransferRow // by id
	byIdem     map[string]*TransferRow
	insertErr  error
	lastInsert *TransferRow
	markCalls  []markCall
}

type markCall struct {
	ID      int64
	Status  string
	Journal *int64
	Failure *string
}

func (f *fakeStore) AccountMeta(_ context.Context, id int64) (*AccountMeta, error) {
	if m, ok := f.meta[id]; ok {
		return m, nil
	}
	return nil, errf("NOT_FOUND", "account %d not found", id)
}

func (f *fakeStore) UserAccountIDs(_ context.Context, userID int64) ([]int64, error) {
	return f.userAccts[userID], nil
}

func (f *fakeStore) InsertTransfer(_ context.Context, t TransferRow) (*TransferRow, error) {
	if f.insertErr != nil {
		return nil, f.insertErr
	}
	t.ID = int64(len(f.transfers) + 1)
	t.Status = TransferPending
	cp := t
	f.transfers[t.ID] = &cp
	f.lastInsert = &cp
	if t.IdempotencyKey != nil {
		f.byIdem[*t.IdempotencyKey] = &cp
	}
	return &cp, nil
}

func (f *fakeStore) TransferByIdemKey(_ context.Context, accountID int64, key string) (*TransferRow, error) {
	if t, ok := f.byIdem[key]; ok && t.AccountID == accountID {
		return t, nil
	}
	return nil, errCode("NOT_FOUND", "transfer not found")
}

func (f *fakeStore) SetTransferResult(_ context.Context, id int64, status string,
	journalID *int64, failure *string) error {
	f.markCalls = append(f.markCalls, markCall{id, status, journalID, failure})
	if t, ok := f.transfers[id]; ok {
		t.Status = status
		t.JournalEntryID = journalID
		t.FailureReason = failure
	}
	return nil
}

// fakePoster records journals + can be programmed with an error.
type fakePoster struct {
	err       error
	committed bool
	journals  []ledger.Journal
	nextID    int64
}

func (f *fakePoster) Post(_ context.Context, j ledger.Journal) (ledger.PostResult, error) {
	if err := j.Validate(); err != nil {
		return ledger.PostResult{}, err
	}
	f.journals = append(f.journals, j)
	f.nextID++
	if f.err != nil {
		return ledger.PostResult{JournalID: f.nextID, Committed: f.committed}, f.err
	}
	return ledger.PostResult{JournalID: f.nextID, Committed: true}, nil
}

// fakeChecker is the AssertMutable seam — status map per account.
type fakeChecker struct{ status map[int64]string }

func (f fakeChecker) AssertMutable(_ context.Context, id int64) error {
	st, ok := f.status[id]
	if !ok {
		return errf("NOT_FOUND", "account %d not found", id)
	}
	if st == "FROZEN" {
		return errf("ACCOUNT_FROZEN", "account %d is FROZEN", id)
	}
	if st != "ACTIVE" {
		return errf("FORBIDDEN", "account %d is %s", id, st)
	}
	return nil
}

func requireErrCode(t *testing.T, err error, want string) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected %s, got nil", want)
	}
	var e *excerrors.Error
	if !stderrors.As(err, &e) || e.Code != want {
		t.Fatalf("expected code %s, got %v", want, err)
	}
}

// ---------------------------------------------------------------------------
// Pure helpers
// ---------------------------------------------------------------------------

func TestTierForUSD(t *testing.T) {
	cases := []struct {
		usd  *decimal.Decimal
		want string
	}{
		{nil, ReviewTierPendingReview}, // fail closed
		{ptr(decimal.NewFromInt(9999)), ReviewTierAuto},
		{ptr(decimal.NewFromInt(10_000)), ReviewTierStandard},
		{ptr(decimal.NewFromInt(50_000)), ReviewTierStandard},
		{ptr(decimal.NewFromInt(50_001)), ReviewTierPendingReview},
	}
	for _, c := range cases {
		if got := TierForUSD(c.usd); got != c.want {
			t.Fatalf("usd=%v: got %s want %s", c.usd, got, c.want)
		}
	}
}

func ptr(d decimal.Decimal) *decimal.Decimal { return &d }

func TestParseMoney(t *testing.T) {
	if _, err := parseMoney("0"); err == nil {
		t.Fatal("zero accepted")
	}
	if _, err := parseMoney("-5"); err == nil {
		t.Fatal("negative accepted")
	}
	if _, err := parseMoney("1.000000001"); err == nil {
		t.Fatal("sub-quantum accepted")
	}
	d, err := parseMoney(" 12.5 ")
	if err != nil || !d.Equal(decimal.MustFromString("12.5")) {
		t.Fatalf("12.5 → %v %v", d, err)
	}
}

func TestNormalizeCurrency(t *testing.T) {
	if c, err := normalizeCurrency("usd"); err != nil || c != "USD" {
		t.Fatalf("usd → %q %v", c, err)
	}
	if _, err := normalizeCurrency("US"); err == nil {
		t.Fatal("2-letter accepted")
	}
	if _, err := normalizeCurrency("U1D"); err == nil {
		t.Fatal("non-alpha accepted")
	}
}

func TestCursorRoundTrip(t *testing.T) {
	now := time.Now().UTC()
	c := encodeCursor(now, 42)
	ts, id, err := decodeCursor(c)
	if err != nil || id != 42 || !ts.Equal(now.Truncate(0)) {
		t.Fatalf("round trip: %v %d %v", ts, id, err)
	}
	for _, bad := range []string{"", "!!!", "AAAA", "aGVsbG8"} {
		if _, _, err := decodeCursor(bad); err == nil {
			// "aGVsbG8" decodes to "hello" — missing separator must fail
			t.Fatalf("cursor %q unexpectedly accepted", bad)
		}
	}
}

func TestPayloadHashSensitive(t *testing.T) {
	a := payloadHash("transfer", "1", "2", "USD", "10")
	b := payloadHash("transfer", "1", "2", "USD", "11")
	if a == b {
		t.Fatal("payload hash insensitive to amount")
	}
}

// ---------------------------------------------------------------------------
// TransferService.Create — validation, ownership, state gates, posting
// ---------------------------------------------------------------------------

func xferFixture(t *testing.T) (*TransferService, *fakeStore, *fakePoster) {
	fs := &fakeStore{
		meta: map[int64]*AccountMeta{
			1: {ID: 1, UserID: 100, Status: "ACTIVE"},
			2: {ID: 2, UserID: 100, Status: "ACTIVE"}, // same user — master↔sub eligible
			3: {ID: 3, UserID: 200, Status: "ACTIVE"}, // foreign user
			4: {ID: 4, UserID: 100, Status: "FROZEN"},
		},
		userAccts: map[int64][]int64{100: {1, 2, 4}, 200: {3}},
		transfers: map[int64]*TransferRow{},
		byIdem:    map[string]*TransferRow{},
	}
	fp := &fakePoster{}
	svc, err := NewTransferService(fs, fp, fakeChecker{status: map[int64]string{
		1: "ACTIVE", 2: "ACTIVE", 3: "ACTIVE", 4: "FROZEN",
	}})
	if err != nil {
		t.Fatalf("service: %v", err)
	}
	return svc, fs, fp
}

func TestTransferValidation(t *testing.T) {
	svc, _, _ := xferFixture(t)
	ctx := context.Background()

	// same account
	_, err := svc.Create(ctx, CreateTransferRequest{CallerAccountID: 1, FromAccountID: 1, ToAccountID: 1, Currency: "USD", Amount: "10"})
	requireErrCode(t, err, "INVALID_REQUEST")
	// bad currency
	_, err = svc.Create(ctx, CreateTransferRequest{CallerAccountID: 1, FromAccountID: 1, ToAccountID: 2, Currency: "US", Amount: "10"})
	requireErrCode(t, err, "INVALID_REQUEST")
	// bad amount
	_, err = svc.Create(ctx, CreateTransferRequest{CallerAccountID: 1, FromAccountID: 1, ToAccountID: 2, Currency: "USD", Amount: "abc"})
	requireErrCode(t, err, "INVALID_REQUEST")
	// over-quantum amount
	_, err = svc.Create(ctx, CreateTransferRequest{CallerAccountID: 1, FromAccountID: 1, ToAccountID: 2, Currency: "USD", Amount: "0.000000001"})
	requireErrCode(t, err, "INVALID_REQUEST")
	// oversized idempotency key
	big := make([]byte, 129)
	for i := range big {
		big[i] = 'k'
	}
	_, err = svc.Create(ctx, CreateTransferRequest{CallerAccountID: 1, FromAccountID: 1, ToAccountID: 2, Currency: "USD", Amount: "10", IdempotencyKey: string(big)})
	requireErrCode(t, err, "INVALID_REQUEST")
}

func TestTransferCrossUserRejected(t *testing.T) {
	svc, _, _ := xferFixture(t)
	_, err := svc.Create(context.Background(), CreateTransferRequest{
		CallerAccountID: 1, FromAccountID: 1, ToAccountID: 3,
		Currency: "USD", Amount: "10",
	})
	requireErrCode(t, err, "FORBIDDEN")
}

func TestTransferFrozenRejected(t *testing.T) {
	svc, _, _ := xferFixture(t)
	_, err := svc.Create(context.Background(), CreateTransferRequest{
		CallerAccountID: 1, FromAccountID: 4, ToAccountID: 2,
		Currency: "USD", Amount: "10",
	})
	requireErrCode(t, err, "ACCOUNT_FROZEN")
}

func TestTransferHappyPath(t *testing.T) {
	svc, fs, fp := xferFixture(t)
	res, err := svc.Create(context.Background(), CreateTransferRequest{
		CallerAccountID: 1, FromAccountID: 1, ToAccountID: 2,
		Currency: "usd", Amount: "10.5", IdempotencyKey: "k1",
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if res.Transfer.Status != TransferCompleted || res.Transfer.JournalEntryID == nil {
		t.Fatalf("expected COMPLETED+journal, got %+v", res.Transfer)
	}
	if len(fp.journals) != 1 {
		t.Fatalf("expected 1 journal, got %d", len(fp.journals))
	}
	j := fp.journals[0]
	if j.EntryType != ledger.EntryTransfer {
		t.Fatalf("entry type %s", j.EntryType)
	}
	if len(j.Effects) != 2 {
		t.Fatalf("expected 2 effects, got %d", len(j.Effects))
	}
	if !j.Effects[0].AvailableDelta.Equal(decimal.MustFromString("-10.5")) ||
		!j.Effects[1].AvailableDelta.Equal(decimal.MustFromString("10.5")) {
		t.Fatalf("bad effects: %+v", j.Effects)
	}
	if j.IdempotencyKey != fmt.Sprintf("transfer:%d", res.Transfer.ID) {
		t.Fatalf("journal key %q", j.IdempotencyKey)
	}
	_ = fs // store verified via markCalls below
	if len(fs.markCalls) != 1 || fs.markCalls[0].Status != TransferCompleted {
		t.Fatalf("markCalls %+v", fs.markCalls)
	}
}

func TestTransferInsufficientBalancePropagates(t *testing.T) {
	svc, fs, fp := xferFixture(t)
	fp.err = excerrors.New(ledger.CodeInsufficientBalance, "insufficient")
	fp.committed = false
	_, err := svc.Create(context.Background(), CreateTransferRequest{
		CallerAccountID: 1, FromAccountID: 1, ToAccountID: 2,
		Currency: "USD", Amount: "9999",
	})
	requireErrCode(t, err, ledger.CodeInsufficientBalance)
	if len(fs.markCalls) != 1 || fs.markCalls[0].Status != TransferFailed {
		t.Fatalf("expected FAILED mark, got %+v", fs.markCalls)
	}
}

func TestTransferCommittedDispatchFailure(t *testing.T) {
	svc, _, fp := xferFixture(t)
	fp.err = excerrors.New(ledger.CodeBalanceDispatchFailed, "no pub")
	fp.committed = true // journal committed — must not repost or fail the client
	res, err := svc.Create(context.Background(), CreateTransferRequest{
		CallerAccountID: 1, FromAccountID: 1, ToAccountID: 2,
		Currency: "USD", Amount: "10",
	})
	if err != nil {
		t.Fatalf("committed-dispatch-failed should still succeed: %v", err)
	}
	if !res.DispatchPending || res.Transfer.Status != TransferCompleted {
		t.Fatalf("expected dispatch_pending COMPLETED, got %+v", res)
	}
}

func TestTransferIdempotentReplay(t *testing.T) {
	svc, fs, _ := xferFixture(t)
	fs.insertErr = ErrIdemConflict
	ph := payloadHash("transfer", "1", "2", "USD", "10")
	stored := &TransferRow{ID: 9, AccountID: 1, FromAccountID: 1, ToAccountID: 2,
		Currency: "USD", Amount: decimal.NewFromInt(10), Status: TransferCompleted,
		PayloadSHA256: ph}
	fs.byIdem["dup"] = stored

	res, err := svc.Create(context.Background(), CreateTransferRequest{
		CallerAccountID: 1, FromAccountID: 1, ToAccountID: 2,
		Currency: "USD", Amount: "10", IdempotencyKey: "dup",
	})
	if err != nil || !res.Replayed || res.Transfer.ID != 9 {
		t.Fatalf("replay: %+v %v", res, err)
	}
}

func TestTransferIdempotencyMismatch(t *testing.T) {
	svc, fs, _ := xferFixture(t)
	fs.insertErr = ErrIdemConflict
	stored := &TransferRow{ID: 9, AccountID: 1, PayloadSHA256: payloadHash("transfer", "1", "2", "USD", "10")}
	fs.byIdem["dup"] = stored
	_, err := svc.Create(context.Background(), CreateTransferRequest{
		CallerAccountID: 1, FromAccountID: 1, ToAccountID: 2,
		Currency: "USD", Amount: "11", IdempotencyKey: "dup", // amount changed
	})
	requireErrCode(t, err, "IDEMPOTENCY_KEY_MISMATCH")
}

// ---------------------------------------------------------------------------
// WithdrawalService — pure validation paths (the tx paths land in the
// integration test; fake pgx.Tx is not worth the surface).
// ---------------------------------------------------------------------------

func wdFixture(t *testing.T) (*WithdrawalService, *fakeStore, *fakePoster) {
	fs := &fakeStore{
		meta: map[int64]*AccountMeta{1: {ID: 1, UserID: 100, Status: "ACTIVE", KYCTier: "T1"}},
	}
	fp := &fakePoster{}
	svc, err := NewWithdrawalService(fs, fp, fakeChecker{status: map[int64]string{1: "ACTIVE"}})
	if err != nil {
		t.Fatalf("service: %v", err)
	}
	return svc, fs, fp
}

func TestWithdrawalValidation(t *testing.T) {
	svc, _, _ := wdFixture(t)
	ctx := context.Background()
	base := CreateWithdrawalRequest{AccountID: 1, UserID: 100, Currency: "USD",
		Amount: "10", ReferenceAccount: "IBAN-1"}

	bad := base
	bad.Currency = "XX"
	if _, err := svc.Create(ctx, bad); err == nil {
		t.Fatal("bad currency accepted")
	}
	bad = base
	bad.Amount = "0"
	requireErrCode(t, mustErr(svc.Create(ctx, bad)), "INVALID_REQUEST")
	bad = base
	bad.ReferenceAccount = ""
	requireErrCode(t, mustErr(svc.Create(ctx, bad)), "INVALID_REQUEST")
	bad = base
	bad.BankMethod = "PIGEON"
	requireErrCode(t, mustErr(svc.Create(ctx, bad)), "INVALID_REQUEST")
	bad = base
	bad.ConfirmMethod = "fax"
	requireErrCode(t, mustErr(svc.Create(ctx, bad)), "INVALID_REQUEST")
	// frozen gate
	frozen := fakeChecker{status: map[int64]string{1: "FROZEN"}}
	svcF, _ := NewWithdrawalService(&fakeStore{}, &fakePoster{}, frozen)
	requireErrCode(t, mustErr(svcF.Create(ctx, base)), "ACCOUNT_FROZEN")
}

func TestWithdrawalNilDepsFailClosed(t *testing.T) {
	if _, err := NewWithdrawalService(nil, &fakePoster{}, fakeChecker{}); err == nil {
		t.Fatal("nil store accepted")
	}
	if _, err := NewWithdrawalService(&fakeStore{}, nil, fakeChecker{}); err == nil {
		t.Fatal("nil poster accepted")
	}
	if _, err := NewTransferService(&fakeStore{}, &fakePoster{}, nil); err == nil {
		t.Fatal("nil checker accepted")
	}
}

// ---------------------------------------------------------------------------
// ChargebackService — validation + guards (tx paths → integration)
// ---------------------------------------------------------------------------

func TestChargebackValidation(t *testing.T) {
	store := &fakeStore{meta: map[int64]*AccountMeta{
		1: {ID: 1, UserID: 100, Status: "ACTIVE"},
		5: {ID: 5, UserID: 100, Status: "CLOSED"},
	}}
	svc, err := NewChargebackService(store, nil)
	if err != nil {
		t.Fatalf("service: %v", err)
	}
	ctx := context.Background()
	base := CreateChargebackRequest{AccountID: 1, Currency: "USD", Amount: "100", Reason: "fraud claim"}

	bad := base
	bad.AccountID = 0
	requireErrCode(t, mustErr(svc.Create(ctx, 7, bad)), "INVALID_REQUEST")
	bad = base
	bad.Currency = "US"
	requireErrCode(t, mustErr(svc.Create(ctx, 7, bad)), "INVALID_REQUEST")
	bad = base
	bad.Amount = "0"
	requireErrCode(t, mustErr(svc.Create(ctx, 7, bad)), "INVALID_REQUEST")
	bad = base
	bad.Reason = ""
	requireErrCode(t, mustErr(svc.Create(ctx, 7, bad)), "INVALID_REQUEST")
	bad = base
	bad.AccountID = 5 // CLOSED
	requireErrCode(t, mustErr(svc.Create(ctx, 7, bad)), "INVALID_REQUEST")
	// freeze without approver → dual control
	bad = base
	bad.FreezeAccount = true
	requireErrCode(t, mustErr(svc.Create(ctx, 7, bad)), "DUAL_CONTROL_REQUIRED")
	// freeze with self-approver → dual control
	bad.ApproverUserID = 7
	requireErrCode(t, mustErr(svc.Create(ctx, 7, bad)), "DUAL_CONTROL_REQUIRED")
	// freeze requested but no freezer wired → SERVICE_DEGRADED
	bad.ApproverUserID = 9
	requireErrCode(t, mustErr(svc.Create(ctx, 7, bad)), "SERVICE_DEGRADED")
}

func TestChargebackFreezePropagates(t *testing.T) {
	store := &fakeStore{meta: map[int64]*AccountMeta{1: {ID: 1, UserID: 100, Status: "ACTIVE"}}}
	ff := &fakeFreezer{err: excerrors.New(accounts.CodeUnauthorizedRole, "no role")}
	svc, _ := NewChargebackService(store, ff)
	_, err := svc.Create(context.Background(), 7, CreateChargebackRequest{
		AccountID: 1, Currency: "USD", Amount: "10", Reason: "r",
		FreezeAccount: true, ApproverUserID: 9,
	})
	requireErrCode(t, err, accounts.CodeUnauthorizedRole)
}

type fakeFreezer struct {
	err   error
	calls []accounts.AdminActor
}

func (f *fakeFreezer) Freeze(_ context.Context, a accounts.AdminActor, id int64, reason, ip string) error {
	f.calls = append(f.calls, a)
	return f.err
}

func mustErr(_ any, err error) error { return err }
