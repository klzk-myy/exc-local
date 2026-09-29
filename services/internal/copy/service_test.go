package copy

import (
	"context"
	"testing"
	"time"

	"exchange/internal/ledger"
	"exchange/pkg/decimal"
)

func d(s string) decimal.Decimal { return decimal.RequireFromString(s) }

// --- fakes -----------------------------------------------------------------

type fakeStore struct {
	strategies map[int64]*Strategy
	follows    map[int64]*Follow
	children   []ChildOrder
	hwms       map[int64]*HighWaterMark
	accruals   []Accrual
	nextID     int64
	minQty     decimal.Decimal
	fills      []ManagerFill
}

func newFakeStore() *fakeStore {
	return &fakeStore{
		strategies: map[int64]*Strategy{},
		follows:    map[int64]*Follow{},
		hwms:       map[int64]*HighWaterMark{},
		minQty:     d("1000"),
	}
}

func (f *fakeStore) CreateStrategy(ctx context.Context, s *Strategy) (*Strategy, error) {
	f.nextID++
	cp := *s
	cp.StrategyID = f.nextID
	cp.Status = StatusIncubating
	cp.IncubatingSince = time.Now().UTC()
	f.strategies[cp.StrategyID] = &cp
	return &cp, nil
}
func (f *fakeStore) StrategyByID(ctx context.Context, id int64) (*Strategy, error) {
	if s, ok := f.strategies[id]; ok {
		cp := *s
		return &cp, nil
	}
	return nil, nil
}
func (f *fakeStore) StrategyForUpdate(ctx context.Context, id int64) (*Strategy, error) {
	return f.StrategyByID(ctx, id)
}
func (f *fakeStore) StrategiesByStatus(ctx context.Context, st StrategyStatus) ([]Strategy, error) {
	var out []Strategy
	for _, s := range f.strategies {
		if s.Status == st {
			out = append(out, *s)
		}
	}
	return out, nil
}
func (f *fakeStore) StrategiesByManager(ctx context.Context, mgr int64) ([]Strategy, error) {
	var out []Strategy
	for _, s := range f.strategies {
		if s.ManagerAccountID == mgr {
			out = append(out, *s)
		}
	}
	return out, nil
}
func (f *fakeStore) ListStrategy(ctx context.Context, id int64) error {
	s := f.strategies[id]
	if s == nil || s.Status != StatusIncubating {
		return errorf(CodeForbidden, "not incubating")
	}
	s.Status = StatusListed
	now := time.Now().UTC()
	s.ListedAt = &now
	return nil
}
func (f *fakeStore) SuspendStrategy(ctx context.Context, id int64, reason string) error {
	s := f.strategies[id]
	if s == nil {
		return errorf(CodeNotFound, "gone")
	}
	s.Status = StatusSuspended
	now := time.Now().UTC()
	s.SuspendedAt = &now
	s.SuspendReason = reason
	return nil
}
func (f *fakeStore) CreateFollow(ctx context.Context, fl *Follow) (*Follow, error) {
	f.nextID++
	cp := *fl
	cp.FollowID = f.nextID
	cp.Status = FollowActive
	f.follows[cp.FollowID] = &cp
	return &cp, nil
}
func (f *fakeStore) FollowByID(ctx context.Context, id int64) (*Follow, error) {
	if fl, ok := f.follows[id]; ok {
		cp := *fl
		return &cp, nil
	}
	return nil, nil
}
func (f *fakeStore) ActiveFollowsForStrategy(ctx context.Context, sid int64) ([]Follow, error) {
	var out []Follow
	for _, fl := range f.follows {
		if fl.StrategyID == sid && fl.Status == FollowActive {
			out = append(out, *fl)
		}
	}
	return out, nil
}
func (f *fakeStore) Unfollow(ctx context.Context, id int64) (*Follow, error) {
	fl := f.follows[id]
	if fl == nil || fl.Status != FollowActive {
		return nil, errorf(CodeNotFound, "not active")
	}
	fl.Status = FollowUnfollowed
	now := time.Now().UTC()
	fl.UnfollowedAt = &now
	cp := *fl
	return &cp, nil
}
func (f *fakeStore) InsertChildOrders(ctx context.Context, rows []ChildOrder) ([]ChildOrder, error) {
	var out []ChildOrder
	for _, c := range rows {
		dup := false
		for _, e := range f.children {
			if e.MasterTradeID == c.MasterTradeID && e.FollowID == c.FollowID {
				dup = true
			}
		}
		if dup {
			continue
		}
		f.nextID++
		c.ChildID = f.nextID
		f.children = append(f.children, c)
		out = append(out, c)
	}
	return out, nil
}
func (f *fakeStore) CancelPendingChildren(ctx context.Context, fid int64) ([]ChildOrder, error) {
	var out []ChildOrder
	for i, c := range f.children {
		if c.FollowID == fid && c.Status == ChildPending {
			f.children[i].Status = ChildCancelled
			out = append(out, f.children[i])
		}
	}
	return out, nil
}
func (f *fakeStore) UpdateChildStatus(ctx context.Context, id int64, st ChildStatus, oid *int64, notice string) error {
	for i := range f.children {
		if f.children[i].ChildID == id {
			f.children[i].Status = st
			f.children[i].ChildOrderID = oid
			f.children[i].Notice = notice
		}
	}
	return nil
}
func (f *fakeStore) HWMForUpdate(ctx context.Context, fid int64) (*HighWaterMark, error) {
	if h, ok := f.hwms[fid]; ok {
		cp := *h
		return &cp, nil
	}
	h := &HighWaterMark{FollowID: fid, WatermarkPnL: decimal.Zero}
	f.hwms[fid] = h
	return h, nil
}
func (f *fakeStore) RatchetHWM(ctx context.Context, fid int64, wm decimal.Decimal, at time.Time) error {
	h := f.hwms[fid]
	if h != nil && wm.GreaterThan(h.WatermarkPnL) {
		h.WatermarkPnL = wm
		h.LastSettledAt = &at
	}
	return nil
}
func (f *fakeStore) InsertAccrual(ctx context.Context, a *Accrual) (*Accrual, error) {
	for _, e := range f.accruals {
		if e.FollowID == a.FollowID && e.PeriodEnd.Equal(a.PeriodEnd) {
			return nil, errorf(CodeIdempotencyCollision, "dupe")
		}
	}
	f.nextID++
	a.AccrualID = f.nextID
	a.Status = AccrualAccrued
	f.accruals = append(f.accruals, *a)
	return a, nil
}
func (f *fakeStore) ManagerTrades(ctx context.Context, mgr int64) ([]ManagerFill, error) {
	return f.fills, nil
}
func (f *fakeStore) FollowerStats(ctx context.Context, sid int64) (int64, decimal.Decimal, error) {
	var n int64
	sum := decimal.Zero
	for _, fl := range f.follows {
		if fl.StrategyID == sid && fl.Status == FollowActive {
			n++
			sum = sum.Add(fl.AllocationNotional)
		}
	}
	return n, sum, nil
}
func (f *fakeStore) InstrumentMinQty(ctx context.Context, id int64) (decimal.Decimal, error) {
	return f.minQty, nil
}

type fakeApprov struct{ err error }

func (a fakeApprov) Appropriateness(context.Context, int64, string) error { return a.err }

type fakeChecker struct{ frozen map[int64]bool }

func (c fakeChecker) AssertMutable(_ context.Context, id int64) error {
	if c.frozen[id] {
		return errorf(CodeForbidden, "account %d frozen", id)
	}
	return nil
}

type fakeRates struct{}

func (fakeRates) IndexRate(_ context.Context, base, quote string) (decimal.Decimal, error) {
	if base == quote {
		return decimal.One, nil
	}
	return decimal.Zero, errorf(CodeServiceDegraded, "no rate %s→%s", base, quote)
}

type fakeAuditor struct{ calls int }

func (a *fakeAuditor) LogSuspension(context.Context, int64, int64, string, string) error {
	a.calls++
	return nil
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

type fakeSubledger struct{ rows []SubledgerRow }

func (s *fakeSubledger) InsertSubledger(_ context.Context, rows []SubledgerRow) error {
	s.rows = append(s.rows, rows...)
	return nil
}

func newTestService(fs *fakeStore) (*Service, *fakePoster, *fakeSubledger, *fakeAuditor) {
	poster := &fakePoster{}
	sub := &fakeSubledger{}
	aud := &fakeAuditor{}
	svc, err := NewService(fs, fakeApprov{}, fakeChecker{}, fakeRates{}, aud,
		WithJournalPoster(poster), WithSubledgerWriter(sub))
	if err != nil {
		panic(err)
	}
	return svc, poster, sub, aud
}

// --- LISTED gating ----------------------------------------------------------

// AC: LISTED requires ≥30 days INCUBATING — a fresh profile rejects.
func TestListGate_Incubation(t *testing.T) {
	fs := newFakeStore()
	svc, _, _, _ := newTestService(fs)
	st, err := svc.CreateStrategy(context.Background(), CreateStrategyInput{
		ManagerAccountID: 10, DisplayName: "alpha", Currency: "USD",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.List(context.Background(), st.StrategyID, 10); err == nil {
		t.Fatal("listed before 30 days")
	}
	// Age it past the gate.
	fs.strategies[st.StrategyID].IncubatingSince = time.Now().Add(-31 * 24 * time.Hour)
	got, err := svc.List(context.Background(), st.StrategyID, 10)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != StatusListed {
		t.Fatalf("status %s", got.Status)
	}
}

// AC: LISTED requires appropriateness PASS — a failing checker blocks.
func TestListGate_Appropriateness(t *testing.T) {
	fs := newFakeStore()
	svc, err := NewService(fs,
		fakeApprov{err: errorf(CodeProductNotPermitted, "assessment required")},
		fakeChecker{}, fakeRates{}, &fakeAuditor{})
	if err != nil {
		t.Fatal(err)
	}
	st, _ := svc.CreateStrategy(context.Background(), CreateStrategyInput{
		ManagerAccountID: 10, DisplayName: "alpha", Currency: "USD",
		InstrumentClass: "FORWARD",
	})
	fs.strategies[st.StrategyID].IncubatingSince = time.Now().Add(-31 * 24 * time.Hour)
	if _, err := svc.List(context.Background(), st.StrategyID, 10); err == nil {
		t.Fatal("listed without appropriateness PASS")
	}
	if fs.strategies[st.StrategyID].Status != StatusIncubating {
		t.Fatal("status flipped despite failed gate")
	}
}

// AC: only the manager can request listing.
func TestListGate_ManagerOnly(t *testing.T) {
	fs := newFakeStore()
	svc, _, _, _ := newTestService(fs)
	st, _ := svc.CreateStrategy(context.Background(), CreateStrategyInput{
		ManagerAccountID: 10, DisplayName: "a", Currency: "USD"})
	fs.strategies[st.StrategyID].IncubatingSince = time.Now().Add(-31 * 24 * time.Hour)
	if _, err := svc.List(context.Background(), st.StrategyID, 99); err == nil {
		t.Fatal("non-manager listed the strategy")
	}
}

// --- Follow / suspend --------------------------------------------------------

func listedStrategy(t *testing.T, svc *Service, fs *fakeStore, pct string) *Strategy {
	t.Helper()
	st, err := svc.CreateStrategy(context.Background(), CreateStrategyInput{
		ManagerAccountID: 10, DisplayName: "alpha", Currency: "USD",
		ProfitSharePct: pct,
	})
	if err != nil {
		t.Fatal(err)
	}
	fs.strategies[st.StrategyID].IncubatingSince = time.Now().Add(-31 * 24 * time.Hour)
	if _, err := svc.List(context.Background(), st.StrategyID, 10); err != nil {
		t.Fatal(err)
	}
	return st
}

// AC: SUSPENDED strategies reject new follows but existing follows are
// untouched; the suspension is audit-logged.
func TestSuspendBlocksNewFollows(t *testing.T) {
	fs := newFakeStore()
	svc, _, _, aud := newTestService(fs)
	st := listedStrategy(t, svc, fs, "20")
	f, err := svc.Follow(context.Background(), FollowInput{
		InvestorAccountID: 42, StrategyID: st.StrategyID,
		AllocationNotional: "5000"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Suspend(context.Background(), st.StrategyID, 7, "10.0.0.1", "scope breach"); err != nil {
		t.Fatal(err)
	}
	if aud.calls != 1 {
		t.Fatalf("audit not logged (%d)", aud.calls)
	}
	if _, err := svc.Follow(context.Background(), FollowInput{
		InvestorAccountID: 43, StrategyID: st.StrategyID,
		AllocationNotional: "1000"}); err == nil {
		t.Fatal("new follow accepted on SUSPENDED strategy")
	}
	// Existing follow unaffected.
	got, _ := fs.FollowByID(context.Background(), f.FollowID)
	if got.Status != FollowActive {
		t.Fatalf("existing follow changed to %s", got.Status)
	}
}

// INCUBATING strategies never accept follows.
func TestFollowRequiresListed(t *testing.T) {
	fs := newFakeStore()
	svc, _, _, _ := newTestService(fs)
	st, _ := svc.CreateStrategy(context.Background(), CreateStrategyInput{
		ManagerAccountID: 10, DisplayName: "a", Currency: "USD"})
	if _, err := svc.Follow(context.Background(), FollowInput{
		InvestorAccountID: 42, StrategyID: st.StrategyID,
		AllocationNotional: "1000"}); err == nil {
		t.Fatal("follow on INCUBATING accepted")
	}
}

// Unfollow cancels pending children and keeps open positions with the
// investor (disclosure returned).
func TestUnfollowCancelsPending(t *testing.T) {
	fs := newFakeStore()
	svc, _, _, _ := newTestService(fs)
	st := listedStrategy(t, svc, fs, "0")
	f, err := svc.Follow(context.Background(), FollowInput{
		InvestorAccountID: 42, StrategyID: st.StrategyID,
		AllocationNotional: "5000"})
	if err != nil {
		t.Fatal(err)
	}
	// Seed a pending child manually.
	fs.children = append(fs.children, ChildOrder{
		ChildID: 900, FollowID: f.FollowID, MasterTradeID: 1,
		InstrumentID: 1, Side: "BUY", Quantity: d("100"), MasterPrice: d("1.1"),
		Status: ChildPending})
	res, err := svc.Unfollow(context.Background(), f.FollowID, 42)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.CancelledChildren) != 1 {
		t.Fatalf("expected 1 cancelled child, got %d", len(res.CancelledChildren))
	}
	if res.Disclosure == "" {
		t.Fatal("disclosure missing")
	}
	if _, err := svc.Unfollow(context.Background(), f.FollowID, 42); err == nil {
		t.Fatal("double unfollow accepted")
	}
}

// --- Profit share / HWM -----------------------------------------------------

func settledEnv(t *testing.T, pct string) (*Service, *fakeStore, *fakePoster, *fakeSubledger, *Follow) {
	fs := newFakeStore()
	svc, poster, sub, _ := newTestService(fs)
	st := listedStrategy(t, svc, fs, pct)
	f, err := svc.Follow(context.Background(), FollowInput{
		InvestorAccountID: 42, StrategyID: st.StrategyID,
		AllocationNotional: "5000"})
	if err != nil {
		t.Fatal(err)
	}
	return svc, fs, poster, sub, f
}

// AC: accrual only above HWM, HWM ratchets on payout.
func TestProfitShare_RatchetsHWM(t *testing.T) {
	svc, fs, poster, sub, f := settledEnv(t, "20")
	end := time.Now().UTC()
	start := end.AddDate(0, -1, 0)
	res, err := svc.SettleProfitShare(context.Background(), f.FollowID, start, end, d("1000"))
	if err != nil {
		t.Fatal(err)
	}
	// accrued = 1000 × 20% = 200
	if !res.Accrued.Equal(d("200")) {
		t.Fatalf("accrued %s", res.Accrued)
	}
	if !res.WatermarkAfter.Equal(d("1000")) {
		t.Fatalf("hwm %s", res.WatermarkAfter)
	}
	if !fs.hwms[f.FollowID].WatermarkPnL.Equal(d("1000")) {
		t.Fatal("store hwm not ratcheted")
	}
	if len(poster.journals) != 1 {
		t.Fatalf("expected 1 journal, got %d", len(poster.journals))
	}
	if len(sub.rows) != 2 || sub.rows[0].TxnType != "PAMM_FEE_PERF" {
		t.Fatalf("subledger rows %+v", sub.rows)
	}
}

// AC: loss month — no accrual, HWM holds (never resets).
func TestProfitShare_LossMonthHoldsHWM(t *testing.T) {
	svc, fs, poster, _, f := settledEnv(t, "20")
	end := time.Now().UTC()
	start := end.AddDate(0, -1, 0)
	if _, err := svc.SettleProfitShare(context.Background(), f.FollowID, start, end, d("1000")); err != nil {
		t.Fatal(err)
	}
	res, err := svc.SettleProfitShare(context.Background(), f.FollowID,
		end, end.AddDate(0, 1, 0), d("-400"))
	if err != nil {
		t.Fatal(err)
	}
	if !res.Accrued.IsZero() {
		t.Fatalf("accrued %s on a loss month", res.Accrued)
	}
	if !fs.hwms[f.FollowID].WatermarkPnL.Equal(d("1000")) {
		t.Fatalf("hwm reset on loss: %s", fs.hwms[f.FollowID].WatermarkPnL)
	}
	if len(poster.journals) != 1 {
		t.Fatal("journal posted on a loss month")
	}
}

// AC: partial recovery accrues only on the excess above the mark.
func TestProfitShare_AccruesOnlyAboveHWM(t *testing.T) {
	svc, _, _, _, f := settledEnv(t, "20")
	end := time.Now().UTC()
	start := end.AddDate(0, -1, 0)
	if _, err := svc.SettleProfitShare(context.Background(), f.FollowID, start, end, d("1000")); err != nil {
		t.Fatal(err)
	}
	// Next period cumulative P&L 1200 → above-HWM excess 200 × 20% = 40.
	res, err := svc.SettleProfitShare(context.Background(), f.FollowID,
		end, end.AddDate(0, 1, 0), d("1200"))
	if err != nil {
		t.Fatal(err)
	}
	if !res.Accrued.Equal(d("40")) {
		t.Fatalf("accrued %s want 40", res.Accrued)
	}
	if !res.WatermarkAfter.Equal(d("1200")) {
		t.Fatalf("hwm %s", res.WatermarkAfter)
	}
}

// AC: the profit-share journal is a BALANCED GL posting — investor P&L
// liability → manager revenue, zero-sum per currency.
func TestProfitShare_BalancedGL(t *testing.T) {
	svc, _, poster, _, f := settledEnv(t, "50")
	end := time.Now().UTC()
	if _, err := svc.SettleProfitShare(context.Background(), f.FollowID,
		end.AddDate(0, -1, 0), end, d("250")); err != nil {
		t.Fatal(err)
	}
	if len(poster.journals) != 1 {
		t.Fatalf("expected journal, got %d", len(poster.journals))
	}
	j := poster.journals[0]
	debit, credit := decimal.Zero, decimal.Zero
	for _, l := range j.Lines {
		debit = debit.Add(l.Debit)
		credit = credit.Add(l.Credit)
	}
	if !debit.Equal(credit) || !debit.Equal(d("125")) {
		t.Fatalf("unbalanced: debit %s credit %s", debit, credit)
	}
	if j.EntryType != ledger.EntryTransfer {
		t.Fatalf("entry type %s — PAMM movements never use DEPOSIT/WITHDRAWAL", j.EntryType)
	}
	// Effects mirror: investor -125, manager +125.
	var sum decimal.Decimal
	for _, e := range j.Effects {
		sum = sum.Add(e.AvailableDelta)
	}
	if !sum.IsZero() {
		t.Fatalf("effects not zero-sum: %s", sum)
	}
}

// Re-settling the same period is idempotent (no double pay).
func TestProfitShare_IdempotentPeriod(t *testing.T) {
	svc, _, poster, _, f := settledEnv(t, "20")
	end := time.Now().UTC()
	start := end.AddDate(0, -1, 0)
	if _, err := svc.SettleProfitShare(context.Background(), f.FollowID, start, end, d("100")); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.SettleProfitShare(context.Background(), f.FollowID, start, end, d("100")); err == nil {
		t.Fatal("re-settlement double-paid")
	}
	// pnl == ratcheted HWM → accrued zero → no second journal; the
	// accrual-row UNIQUE(follow_id, period_end) is the idempotent fence.
	if len(poster.journals) != 1 {
		t.Fatalf("journals=%d", len(poster.journals))
	}
}
