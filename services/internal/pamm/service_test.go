package pamm

import (
	"context"
	"testing"
	"time"

	"exchange/internal/ledger"
	"exchange/pkg/decimal"
)

// --- fakes -----------------------------------------------------------------

type fakeStore struct {
	pools     map[int64]*Pool
	byAcct    map[int64]*Pool
	allocs    map[string]*Allocation // "pool:investor"
	subledger []SubledgerEntry
	fills     []FillAllocation
	accounts  map[int64]*AccountMeta
	nextID    int64
}

func newFakeStore() *fakeStore {
	return &fakeStore{
		pools:    map[int64]*Pool{},
		byAcct:   map[int64]*Pool{},
		allocs:   map[string]*Allocation{},
		accounts: map[int64]*AccountMeta{},
	}
}

func (f *fakeStore) AccountMeta(_ context.Context, id int64) (*AccountMeta, error) {
	if m, ok := f.accounts[id]; ok {
		cp := *m
		return &cp, nil
	}
	return nil, errorf(CodeNotFound, "account %d", id)
}
func (f *fakeStore) CreatePool(_ context.Context, mgr int64, name, ccy string, min decimal.Decimal) (*Pool, error) {
	m, ok := f.accounts[mgr]
	if !ok {
		return nil, errorf(CodeNotFound, "manager %d", mgr)
	}
	if m.ParentID != nil {
		return nil, errorf(CodeForbidden, "sub-account cannot manage")
	}
	f.nextID++
	poolAcct := f.nextID
	f.nextID++
	p := &Pool{PoolID: f.nextID, ManagerAccountID: mgr, PoolAccountID: poolAcct,
		Name: name, Currency: ccy, MinInvestment: min, Status: PoolActive,
		CreatedAt: time.Now()}
	f.pools[p.PoolID] = p
	f.byAcct[poolAcct] = p
	f.accounts[poolAcct] = &AccountMeta{ID: poolAcct, UserID: m.UserID, Status: "ACTIVE"}
	return p, nil
}
func (f *fakeStore) PoolByID(_ context.Context, id int64) (*Pool, error) {
	if p, ok := f.pools[id]; ok {
		cp := *p
		return &cp, nil
	}
	return nil, nil
}
func (f *fakeStore) PoolByAccountID(_ context.Context, id int64) (*Pool, error) {
	if p, ok := f.byAcct[id]; ok {
		cp := *p
		return &cp, nil
	}
	return nil, nil
}
func (f *fakeStore) AllocationForUpdate(_ context.Context, poolID, inv int64) (*Allocation, error) {
	if a, ok := f.allocs[allocKey(poolID, inv)]; ok {
		cp := *a
		return &cp, nil
	}
	return nil, nil
}
func allocKey(poolID, inv int64) string {
	return decimal.NewFromInt(poolID).String() + ":" + decimal.NewFromInt(inv).String()
}
func (f *fakeStore) BumpInvested(_ context.Context, poolID, inv int64, ccy string, delta decimal.Decimal) (*Allocation, error) {
	k := allocKey(poolID, inv)
	a, ok := f.allocs[k]
	if !ok {
		a = &Allocation{AllocationID: int64(len(f.allocs) + 1), PoolID: poolID,
			InvestorAccountID: inv, Currency: ccy, Status: AllocActive,
			CreatedAt: time.Now()}
		f.allocs[k] = a
	}
	if a.Invested.Add(delta).IsNegative() {
		return nil, errorf(CodeInsufficientBalance, "redeem exceeds invested")
	}
	a.Invested = a.Invested.Add(delta)
	cp := *a
	return &cp, nil
}
func (f *fakeStore) InsertSubledger(_ context.Context, rows []SubledgerEntry) error {
	for _, r := range rows {
		if r.IdempotencyKey != "" {
			for _, e := range f.subledger {
				if e.IdempotencyKey == r.IdempotencyKey {
					return errIdempotentReplay
				}
			}
		}
		f.subledger = append(f.subledger, r)
	}
	return nil
}
func (f *fakeStore) SubledgerByKey(_ context.Context, key string) (*SubledgerEntry, error) {
	for _, e := range f.subledger {
		if e.IdempotencyKey == key {
			cp := e
			return &cp, nil
		}
	}
	return nil, nil
}
func (f *fakeStore) ActiveAllocations(_ context.Context, poolID int64) ([]Allocation, error) {
	var out []Allocation
	for _, a := range f.allocs {
		if a.PoolID == poolID && a.Status == AllocActive && a.Invested.IsPositive() {
			out = append(out, *a)
		}
	}
	return out, nil
}
func (f *fakeStore) InsertFillAllocations(_ context.Context, rows []FillAllocation) (int, error) {
	applied := 0
	for _, r := range rows {
		dup := false
		for _, e := range f.fills {
			if e.MasterTradeID == r.MasterTradeID && e.AllocationID == r.AllocationID {
				dup = true
			}
		}
		if !dup {
			f.fills = append(f.fills, r)
			applied++
		}
	}
	return applied, nil
}
func (f *fakeStore) PoolAccountIDs(_ context.Context) ([]int64, error) {
	var out []int64
	for _, p := range f.pools {
		out = append(out, p.PoolAccountID)
	}
	return out, nil
}

type fakePoster struct {
	journals []ledger.Journal
	nextID   int64
}

func (p *fakePoster) Post(_ context.Context, j ledger.Journal) (ledger.PostResult, error) {
	if err := j.Validate(); err != nil {
		return ledger.PostResult{}, err
	}
	p.nextID++
	p.journals = append(p.journals, j)
	return ledger.PostResult{Committed: true, JournalID: p.nextID}, nil
}

type fakeChecker struct{ frozen map[int64]bool }

func (c fakeChecker) AssertMutable(_ context.Context, id int64) error {
	if c.frozen[id] {
		return errorf(CodeForbidden, "account %d frozen", id)
	}
	return nil
}

func svcEnv(t *testing.T) (*Service, *fakeStore, *fakePoster) {
	fs := newFakeStore()
	fs.accounts[10] = &AccountMeta{ID: 10, UserID: 1, Status: "ACTIVE", KycTier: "T2"}
	fs.accounts[42] = &AccountMeta{ID: 42, UserID: 2, Status: "ACTIVE", KycTier: "T2"}
	poster := &fakePoster{}
	svc, err := NewService(fs, poster, fakeChecker{})
	if err != nil {
		t.Fatal(err)
	}
	return svc, fs, poster
}

// --- tests ------------------------------------------------------------------

func TestCreatePool_Hierarchy(t *testing.T) {
	svc, fs, _ := svcEnv(t)
	p, err := svc.CreatePool(context.Background(), 10, "alpha pool", "USD", "100")
	if err != nil {
		t.Fatal(err)
	}
	if p.PoolAccountID == 0 || p.Status != PoolActive {
		t.Fatalf("pool %+v", p)
	}
	// A pool account is a sub-account; it cannot manage its own pool.
	pid := int64(999)
	fs.accounts[p.PoolAccountID].ParentID = &pid
	if _, err := svc.CreatePool(context.Background(), p.PoolAccountID, "nested", "USD", "0"); err == nil {
		t.Fatal("pool account managed a pool")
	}
}

// AC (fiat-cap non-deduction): invest posts a TRANSFER journal 2010 →
// 2170_PAMM_POOL_LIABILITY — never DEPOSIT/WITHDRAWAL, and touches only
// the investor + pool wallets (no funding_transactions, no daily counter
// write — the withdrawal-cap path is never invoked).
func TestInvest_TaxonomyAndCapIsolation(t *testing.T) {
	svc, fs, poster := svcEnv(t)
	p, _ := svc.CreatePool(context.Background(), 10, "a", "USD", "100")
	res, err := svc.Invest(context.Background(), MovementRequest{
		PoolID: p.PoolID, InvestorAccountID: 42, Amount: "500"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Invested != "500" {
		t.Fatalf("invested %s", res.Invested)
	}
	j := poster.journals[0]
	if j.EntryType != ledger.EntryTransfer {
		t.Fatalf("entry type %s — PAMM movements are never DEPOSIT/WITHDRAWAL", j.EntryType)
	}
	// GL: 2010 → 2170.
	if j.Lines[0].AccountCode != "2010_CUSTOMER_LIABILITY_USD" ||
		j.Lines[1].AccountCode != "2170_PAMM_POOL_LIABILITY_USD" {
		t.Fatalf("GL codes %+v", j.Lines)
	}
	// Subledger taxonomy is the dedicated type.
	if fs.subledger[0].TxnType != TxnInvest || fs.subledger[1].TxnType != TxnInvest {
		t.Fatalf("txn types %+v", fs.subledger)
	}
	a := fs.allocs[allocKey(p.PoolID, 42)]
	if a == nil || !a.Invested.Equal(d("500")) {
		t.Fatal("allocation not bumped")
	}
}

func TestInvest_BelowMinimum(t *testing.T) {
	svc, _, _ := svcEnv(t)
	p, _ := svc.CreatePool(context.Background(), 10, "a", "USD", "100")
	if _, err := svc.Invest(context.Background(), MovementRequest{
		PoolID: p.PoolID, InvestorAccountID: 42, Amount: "50"}); err == nil {
		t.Fatal("below-minimum invest accepted")
	}
}

func TestRedeem_OverInvested(t *testing.T) {
	svc, _, _ := svcEnv(t)
	p, _ := svc.CreatePool(context.Background(), 10, "a", "USD", "0")
	if _, err := svc.Invest(context.Background(), MovementRequest{
		PoolID: p.PoolID, InvestorAccountID: 42, Amount: "500"}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Redeem(context.Background(), MovementRequest{
		PoolID: p.PoolID, InvestorAccountID: 42, Amount: "600"}); err == nil {
		t.Fatal("over-redemption accepted")
	}
	res, err := svc.Redeem(context.Background(), MovementRequest{
		PoolID: p.PoolID, InvestorAccountID: 42, Amount: "200"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Invested != "300" {
		t.Fatalf("invested %s", res.Invested)
	}
	if res.TxnType != TxnRedeem {
		t.Fatalf("txn %s", res.TxnType)
	}
}

// Idempotent replay: same key + same payload resolves to the stored
// movement without a second journal; a changed payload under the same
// key is a collision.
func TestInvest_Idempotency(t *testing.T) {
	svc, fs, poster := svcEnv(t)
	p, _ := svc.CreatePool(context.Background(), 10, "a", "USD", "0")
	req := MovementRequest{PoolID: p.PoolID, InvestorAccountID: 42,
		Amount: "500", IdempotencyKey: "k1"}
	if _, err := svc.Invest(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	re, err := svc.Invest(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if !re.Replayed || len(poster.journals) != 1 {
		t.Fatalf("replay double-posted: %+v journals=%d", re, len(poster.journals))
	}
	req.Amount = "600"
	if _, err := svc.Invest(context.Background(), req); err == nil {
		t.Fatal("payload collision accepted")
	}
	if len(fs.subledger) != 2 {
		t.Fatalf("collision wrote subledger rows: %d", len(fs.subledger))
	}
}
