// nbp_test.go — Retail Negative-Balance Protection tests (Phase-19
// Task 19.3.9 + Task 19.3.20 NBP-restitution half; spec §13.6c, §13.11
// item 3, §24 #133/#228/#320).
//
// Coverage map (task DoD):
//   - retail equity floored to 0 post-liquidation; deficit → insurance
//     fund debit (NBP_RESTITUTION movement + journal id on the row)
//   - house-P&L fallback when the fund cannot cover (+P0 page,
//     DR 5200_NBP_RESTITUTION_EXPENSE / CR 2010_CUSTOMER_LIABILITY)
//   - professional/ECP accounts remain liable — no write-off
//   - recurring hits flag the account (nbp:review Redis key + alert)
//   - nbp_events persistence: minted-before-movement, FAILED on error,
//     retryable-row reuse, admin read surface
//   - sweep semantics: retail-only, per-account failure isolation
//   - edge: equity<0 with clean balances (positional — liquidation owns),
//     locked>0 deficit rows routing house, positive-equity negative legs
//     deferred to the auto-exchange lane
//
// Gated legs: EXC_REDIS_TEST=1 (real flag write) and EXC_PG_TEST=1
// (migration-230 round-trip), same pattern as auto_halt_test.go.
package risk

import (
	"context"
	"fmt"
	"os"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"exchange/internal/ledger"
	excredis "exchange/internal/redis"
	"exchange/pkg/decimal"
)

// ---------------------------------------------------------------------------
// Test doubles
// ---------------------------------------------------------------------------

type nbpCat struct {
	cat string
	nbp bool
}

// memNBPStore is the in-memory NBPStore with error injection.
type memNBPStore struct {
	mu      sync.Mutex
	cats    map[int64]nbpCat
	bals    map[int64][]BalanceAmount
	equity  map[int64]decimal.Decimal // absent ⇒ no margin snapshot
	deficit []int64
	events  []NBPEvent
	nextID  int64
	now     func() time.Time
}

func newMemNBPStore(now func() time.Time) *memNBPStore {
	return &memNBPStore{
		cats:   map[int64]nbpCat{},
		bals:   map[int64][]BalanceAmount{},
		equity: map[int64]decimal.Decimal{},
		now:    now,
	}
}

func (m *memNBPStore) AccountCategory(_ context.Context, accountID int64) (string, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	c, ok := m.cats[accountID]
	if !ok {
		return "", false, fmt.Errorf("account %d not found", accountID)
	}
	return c.cat, c.nbp, nil
}

func (m *memNBPStore) Balances(_ context.Context, accountID int64) ([]BalanceAmount, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]BalanceAmount(nil), m.bals[accountID]...), nil
}

func (m *memNBPStore) MarginEquity(_ context.Context, accountID int64) (*decimal.Decimal, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if e, ok := m.equity[accountID]; ok {
		cp := e
		return &cp, nil
	}
	return nil, nil
}

func (m *memNBPStore) RetailDeficitAccounts(_ context.Context) ([]int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]int64(nil), m.deficit...), nil
}

func (m *memNBPStore) InsertNBPEvent(_ context.Context, ev NBPEvent) (int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.nextID++
	ev.ID = m.nextID
	ev.CreatedAt = m.now()
	m.events = append(m.events, ev)
	return ev.ID, nil
}

func (m *memNBPStore) ReusableNBPEvent(_ context.Context, accountID int64,
	ccy string, shortfall decimal.Decimal) (*NBPEvent, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for i := len(m.events) - 1; i >= 0; i-- {
		ev := m.events[i]
		if ev.AccountID == accountID && ev.Currency == ccy &&
			ev.Shortfall.Equal(shortfall) &&
			(ev.Status == NBPStatusFailed || ev.JournalEntryID == nil) {
			cp := ev
			return &cp, nil
		}
	}
	return nil, nil
}

func (m *memNBPStore) CompleteNBPEvent(_ context.Context, id, journalID int64, funding string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for i := range m.events {
		if m.events[i].ID == id {
			jid := journalID
			m.events[i].JournalEntryID = &jid
			m.events[i].FundingSource = funding
			m.events[i].Status = NBPStatusPosted
			return nil
		}
	}
	return fmt.Errorf("event %d not found", id)
}

func (m *memNBPStore) FailNBPEvent(_ context.Context, id int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for i := range m.events {
		if m.events[i].ID == id {
			m.events[i].Status = NBPStatusFailed
			return nil
		}
	}
	return fmt.Errorf("event %d not found", id)
}

func (m *memNBPStore) CountNBPEvents(_ context.Context, accountID int64, since time.Time) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := 0
	for _, ev := range m.events {
		if ev.AccountID == accountID && ev.Status == NBPStatusPosted &&
			!ev.CreatedAt.Before(since) {
			n++
		}
	}
	return n, nil
}

func (m *memNBPStore) NBPEvents(_ context.Context, accountID int64, limit int) ([]NBPEvent, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := []NBPEvent{}
	for _, ev := range m.events {
		if accountID == 0 || ev.AccountID == accountID {
			out = append(out, ev)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID > out[j].ID })
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func (m *memNBPStore) creditWallet(accountID int64, ccy string, amt decimal.Decimal) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for i := range m.bals[accountID] {
		if m.bals[accountID][i].Currency == ccy {
			m.bals[accountID][i].Available = m.bals[accountID][i].Available.Add(amt)
		}
	}
}

func (m *memNBPStore) total(accountID int64, ccy string) decimal.Decimal {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, b := range m.bals[accountID] {
		if b.Currency == ccy {
			return b.Total()
		}
	}
	return decimal.Zero
}

// fakeNBPFunder records Debit calls and tracks a per-currency fund row.
type fakeNBPFunder struct {
	mu       sync.Mutex
	bals     map[string]decimal.Decimal
	calls    []FundMovement
	balErr   error
	debitErr error
	nextJ    int64
	fixedJID int64 // >0: return this journal id (FK-safe in PG tests)
	apply    func(m FundMovement)
}

func (f *fakeNBPFunder) Balance(_ context.Context, ccy string) (*FundBalance, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.balErr != nil {
		return nil, f.balErr
	}
	b, ok := f.bals[ccy]
	if !ok {
		return nil, nil
	}
	return &FundBalance{Currency: ccy, Balance: b}, nil
}

func (f *fakeNBPFunder) Debit(_ context.Context, m FundMovement) (*FundMovementResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, m)
	if f.debitErr != nil {
		return nil, f.debitErr
	}
	if f.apply != nil {
		f.apply(m)
	}
	bal := f.bals[m.Currency].Sub(m.Amount)
	f.bals[m.Currency] = bal
	f.nextJ++
	jid := f.fixedJID
	if jid == 0 {
		jid = 9000 + f.nextJ
	}
	return &FundMovementResult{BalanceAfter: bal, JournalID: jid}, nil
}

// fakeNBPPoster records posted journals (the LedgerService seam).
type fakeNBPPoster struct {
	mu       sync.Mutex
	calls    []ledger.Journal
	err      error
	nextJ    int64
	fixedJID int64 // >0: return this journal id (FK-safe in PG tests)
	apply    func(j ledger.Journal)
}

func (p *fakeNBPPoster) Post(_ context.Context, j ledger.Journal) (ledger.PostResult, error) {
	if err := j.Validate(); err != nil {
		return ledger.PostResult{}, err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls = append(p.calls, j)
	if p.err != nil {
		return ledger.PostResult{}, p.err
	}
	if p.apply != nil {
		p.apply(j)
	}
	p.nextJ++
	jid := p.fixedJID
	if jid == 0 {
		jid = 8000 + p.nextJ
	}
	return ledger.PostResult{JournalID: jid, Committed: true}, nil
}

func (p *fakeNBPPoster) PostJournal(_ context.Context, _ pgx.Tx, j ledger.Journal) (ledger.PostResult, error) {
	return p.Post(context.Background(), j)
}

// nbpAlertSpy captures OpsAlert pages.
type nbpAlertSpy struct {
	mu    sync.Mutex
	calls []OpsAlert
}

func (a *nbpAlertSpy) Raise(_ context.Context, al OpsAlert) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.calls = append(a.calls, al)
	return nil
}

func (a *nbpAlertSpy) codes() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	var out []string
	for _, c := range a.calls {
		out = append(out, c.Code)
	}
	return out
}

func (a *nbpAlertSpy) find(code string) *OpsAlert {
	a.mu.Lock()
	defer a.mu.Unlock()
	for i := range a.calls {
		if a.calls[i].Code == code {
			return &a.calls[i]
		}
	}
	return nil
}

// staticLevels is a fixture MarginLevelReader.
type staticLevels map[int64]*MarginLevel

func (m staticLevels) MarginLevel(_ context.Context, accountID int64) (*MarginLevel, error) {
	return m[accountID], nil
}

// ---------------------------------------------------------------------------
// Rig
// ---------------------------------------------------------------------------

type nbpRig struct {
	svc    *NBPService
	store  *memNBPStore
	fund   *fakeNBPFunder
	poster *fakeNBPPoster
	alerts *nbpAlertSpy
	clock  *fakeClock
	levels staticLevels
}

func newNBPRig(t *testing.T) *nbpRig {
	t.Helper()
	r := &nbpRig{
		clock:  newFakeClock(),
		fund:   &fakeNBPFunder{bals: map[string]decimal.Decimal{}},
		poster: &fakeNBPPoster{},
		alerts: &nbpAlertSpy{},
		levels: staticLevels{},
	}
	r.store = newMemNBPStore(r.clock.Now)
	// Wallet application: the fake fund/poster credit the mem store's
	// balances row so tests can assert the equity floor physically.
	r.fund.apply = func(m FundMovement) {
		r.store.creditWallet(m.AccountID, m.Currency, m.Amount)
	}
	r.poster.apply = func(j ledger.Journal) {
		for _, e := range j.Effects {
			r.store.creditWallet(e.AccountID, e.Currency, e.AvailableDelta)
		}
	}
	svc, err := NewNBPService(NBPDeps{
		Store: r.store, Fund: r.fund, Poster: r.poster,
		Levels: r.levels, Alerter: r.alerts, Now: r.clock.Now,
	})
	if err != nil {
		t.Fatalf("nbp service: %v", err)
	}
	r.svc = svc
	return r
}

func (r *nbpRig) retail(accountID int64) {
	r.store.cats[accountID] = nbpCat{cat: "RETAIL", nbp: true}
}

// ---------------------------------------------------------------------------
// Construction
// ---------------------------------------------------------------------------

func TestNBPConstructionRequiresSeams(t *testing.T) {
	if _, err := NewNBPService(NBPDeps{}); err == nil {
		t.Fatal("nil store must fail construction")
	}
	st := newMemNBPStore(time.Now)
	if _, err := NewNBPService(NBPDeps{Store: st}); err == nil {
		t.Fatal("nil fund must fail construction")
	}
	if _, err := NewNBPService(NBPDeps{Store: st, Fund: &fakeNBPFunder{}}); err == nil {
		t.Fatal("nil poster must fail construction")
	}
}

// ---------------------------------------------------------------------------
// Core restitution paths
// ---------------------------------------------------------------------------

// §24 #133/#228: retail equity floored at 0; shortfall debited from the
// insurance fund; balanced journal + audit row.
func TestNBPRetailDeficitRestitutedFromFund(t *testing.T) {
	r := newNBPRig(t)
	ctx := context.Background()
	r.retail(7)
	r.store.bals[7] = []BalanceAmount{
		{Currency: "USD", Available: d("-125.5"), Locked: decimal.Zero},
	}
	r.levels[7] = &MarginLevel{AccountID: 7, Equity: d("-125.5")}
	r.fund.bals["USD"] = d("1000")

	out, err := r.svc.EvaluateAccount(ctx, 7)
	if err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	if !out.Applied || len(out.Events) != 1 {
		t.Fatalf("expected 1 restitution, got %+v", out)
	}
	ev := out.Events[0]
	if ev.FundingSource != NBPSourceInsuranceFund || ev.Status != NBPStatusPosted {
		t.Fatalf("event wrong: %+v", ev)
	}
	if ev.JournalEntryID == nil || *ev.JournalEntryID != 9001 {
		t.Fatalf("journal id must land on the row: %+v", ev)
	}
	if !ev.Shortfall.Equal(d("125.5")) {
		t.Fatalf("shortfall %s, want 125.5", ev.Shortfall)
	}
	if ev.DeficitEquity == nil || !ev.DeficitEquity.Equal(d("-125.5")) {
		t.Fatalf("deficit_equity must record equity at trigger: %+v", ev)
	}

	// The fund movement is the NBP vocabulary entry — reason, ref type
	// 'nbp', wallet effect on the account, event-seeded idempotency.
	if len(r.fund.calls) != 1 {
		t.Fatalf("expected 1 fund debit, got %d", len(r.fund.calls))
	}
	mv := r.fund.calls[0]
	if mv.Reason != FundReasonNBPRestitution || mv.ReferenceType != "nbp" ||
		mv.ReferenceID != ev.ID || mv.AccountID != 7 ||
		mv.IdempotencyKey != fmt.Sprintf("nbp:%d", ev.ID) ||
		!mv.Amount.Equal(d("125.5")) {
		t.Fatalf("fund movement wrong: %+v", mv)
	}

	// Equity floored to 0 — the wallet credit zeroes the negative total.
	if got := r.store.total(7, "USD"); !got.IsZero() {
		t.Fatalf("post-NBP USD total %s, want 0", got)
	}
	// The L1 deficit-trigger event fired.
	if a := r.alerts.find(CodeNBPDeficitTriggered); a == nil || a.Severity != SeverityP1 {
		t.Fatalf("expected P1 %s alert, got %v", CodeNBPDeficitTriggered, r.alerts.codes())
	}
}

// Task item 10: professional/ECP accounts remain liable — no write-off,
// no event, collection flow continues untouched.
func TestNBPProfessionalAndECPRemainLiable(t *testing.T) {
	r := newNBPRig(t)
	ctx := context.Background()
	r.store.cats[11] = nbpCat{cat: "PROFESSIONAL", nbp: false}
	r.store.cats[12] = nbpCat{cat: "ELIGIBLE_COUNTERPARTY", nbp: false}
	for _, id := range []int64{11, 12} {
		r.store.bals[id] = []BalanceAmount{{Currency: "USD", Available: d("-50"), Locked: decimal.Zero}}
		r.levels[id] = &MarginLevel{AccountID: id, Equity: d("-50")}
		out, err := r.svc.EvaluateAccount(ctx, id)
		if err != nil {
			t.Fatalf("evaluate %d: %v", id, err)
		}
		if out.Applied {
			t.Fatalf("professional/ECP acct %d must not be restituted", id)
		}
	}
	if len(r.fund.calls) != 0 || len(r.poster.calls) != 0 || len(r.store.events) != 0 {
		t.Fatal("liable accounts must see no movement and no audit row")
	}
	if got := r.store.total(11, "USD"); !got.Equal(d("-50")) {
		t.Fatalf("professional balance must stay liable: %s", got)
	}
}

// Task item 8 + §13.6c item 2: fund < shortfall → house P&L + P0 alert.
// GL pair per the insurance_fund.go convention: DR 5200_NBP_RESTITUTION_EXPENSE
// / CR 2010_CUSTOMER_LIABILITY.
func TestNBPHousePnLFallbackWhenFundCannotCover(t *testing.T) {
	r := newNBPRig(t)
	ctx := context.Background()
	r.retail(7)
	r.store.bals[7] = []BalanceAmount{
		{Currency: "USD", Available: d("-200"), Locked: decimal.Zero},
	}
	r.levels[7] = &MarginLevel{AccountID: 7, Equity: d("-200")}
	r.fund.bals["USD"] = d("50") // fund cannot cover 200

	out, err := r.svc.EvaluateAccount(ctx, 7)
	if err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	if !out.Applied || len(out.Events) != 1 {
		t.Fatalf("expected restitution, got %+v", out)
	}
	ev := out.Events[0]
	if ev.FundingSource != NBPSourceHousePnL {
		t.Fatalf("expected HOUSE_PNL, got %s", ev.FundingSource)
	}
	if len(r.fund.calls) != 0 {
		t.Fatal("fund must not be touched when it cannot cover")
	}
	if len(r.poster.calls) != 1 {
		t.Fatalf("expected 1 house journal, got %d", len(r.poster.calls))
	}
	j := r.poster.calls[0]
	if j.EntryType != ledger.EntryAdjustment {
		t.Fatalf("entry type %s, want ADJUSTMENT", j.EntryType)
	}
	if j.IdempotencyKey != fmt.Sprintf("nbp:%d", ev.ID) {
		t.Fatalf("idempotency key %q must seed from the event id", j.IdempotencyKey)
	}
	var dr, cr *ledger.Line
	for i := range j.Lines {
		l := &j.Lines[i]
		switch {
		case l.Debit.IsPositive():
			dr = l
		case l.Credit.IsPositive():
			cr = l
		}
	}
	if dr == nil || cr == nil ||
		dr.AccountCode != "5200_NBP_RESTITUTION_EXPENSE_USD" ||
		cr.AccountCode != "2010_CUSTOMER_LIABILITY_USD" ||
		!dr.Debit.Equal(d("200")) || !cr.Credit.Equal(d("200")) {
		t.Fatalf("house journal lines wrong: %+v", j.Lines)
	}
	if len(j.Effects) != 1 || j.Effects[0].AccountID != 7 ||
		!j.Effects[0].AvailableDelta.Equal(d("200")) || !j.Effects[0].AllowNegative {
		t.Fatalf("wallet effect wrong: %+v", j.Effects)
	}
	// P0 page with the reserve-deficit code.
	a := r.alerts.find(CodeNBPRestitutionReserveDeficit)
	if a == nil || a.Severity != SeverityP0 {
		t.Fatalf("expected P0 %s, got %v", CodeNBPRestitutionReserveDeficit, r.alerts.codes())
	}
	if got := r.store.total(7, "USD"); !got.IsZero() {
		t.Fatalf("post-NBP USD total %s, want 0", got)
	}
}

func TestNBPNoFundRowFallsBackToHouse(t *testing.T) {
	r := newNBPRig(t)
	ctx := context.Background()
	r.retail(7)
	r.store.bals[7] = []BalanceAmount{
		{Currency: "USD", Available: d("-75"), Locked: decimal.Zero},
	}
	r.levels[7] = &MarginLevel{AccountID: 7, Equity: d("-75")}
	// no fund row at all for USD
	out, err := r.svc.EvaluateAccount(ctx, 7)
	if err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	if len(out.Events) != 1 || out.Events[0].FundingSource != NBPSourceHousePnL {
		t.Fatalf("absent fund row must route house: %+v", out.Events)
	}
}

// Edge: a deficit row carrying locked collateral (available+shortfall
// stays negative) would trip the fund movement's non-negative-available
// guard — route house, whose journal permits it.
func TestNBPLockedDeficitRowRoutesHouse(t *testing.T) {
	r := newNBPRig(t)
	ctx := context.Background()
	r.retail(7)
	r.store.bals[7] = []BalanceAmount{
		{Currency: "USD", Available: d("-150"), Locked: d("100")}, // total -50
	}
	r.levels[7] = &MarginLevel{AccountID: 7, Equity: d("-50")}
	r.fund.bals["USD"] = d("10000")
	out, err := r.svc.EvaluateAccount(ctx, 7)
	if err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	if len(out.Events) != 1 || out.Events[0].FundingSource != NBPSourceHousePnL {
		t.Fatalf("locked-edge deficit must route house: %+v", out.Events)
	}
	if !out.Events[0].Shortfall.Equal(d("50")) {
		t.Fatalf("shortfall must be |total|=50, got %s", out.Events[0].Shortfall)
	}
	if got := r.store.total(7, "USD"); !got.IsZero() {
		t.Fatalf("total must floor at 0, got %s", got)
	}
}

func TestNBPMultiCurrencyDeficitRestitutesEachLeg(t *testing.T) {
	r := newNBPRig(t)
	ctx := context.Background()
	r.retail(7)
	r.store.bals[7] = []BalanceAmount{
		{Currency: "USD", Available: d("-100"), Locked: decimal.Zero},
		{Currency: "EUR", Available: d("-30"), Locked: decimal.Zero},
		{Currency: "GBP", Available: d("80"), Locked: decimal.Zero},
	}
	// no equity view — pessimistic trigger on negative balances
	r.fund.bals["USD"] = d("500")
	r.fund.bals["EUR"] = d("500")
	out, err := r.svc.EvaluateAccount(ctx, 7)
	if err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	if len(out.Events) != 2 {
		t.Fatalf("expected 2 restitutions (USD+EUR), got %+v", out.Events)
	}
	if got := r.store.total(7, "USD"); !got.IsZero() {
		t.Fatalf("USD total %s", got)
	}
	if got := r.store.total(7, "EUR"); !got.IsZero() {
		t.Fatalf("EUR total %s", got)
	}
	if got := r.store.total(7, "GBP"); !got.Equal(d("80")) {
		t.Fatalf("positive GBP leg is client property, got %s", got)
	}
}

// ---------------------------------------------------------------------------
// Trigger semantics
// ---------------------------------------------------------------------------

func TestNBPPositiveEquityLeavesLegToAutoExchange(t *testing.T) {
	r := newNBPRig(t)
	ctx := context.Background()
	r.retail(7)
	r.store.bals[7] = []BalanceAmount{
		{Currency: "EUR", Available: d("-50"), Locked: decimal.Zero},
		{Currency: "USD", Available: d("9000"), Locked: decimal.Zero},
	}
	r.levels[7] = &MarginLevel{AccountID: 7, Equity: d("8000")} // net positive
	out, err := r.svc.EvaluateAccount(ctx, 7)
	if err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	if out.Applied || len(r.store.events) != 0 || len(r.fund.calls) != 0 {
		t.Fatal("equity≥0: negative legs are the auto-exchange lane, not NBP")
	}
}

func TestNBPNoDeficitNoOp(t *testing.T) {
	r := newNBPRig(t)
	ctx := context.Background()
	r.retail(7)
	r.store.bals[7] = []BalanceAmount{
		{Currency: "USD", Available: d("100"), Locked: d("50")},
	}
	r.levels[7] = &MarginLevel{AccountID: 7, Equity: d("150")}
	out, err := r.svc.EvaluateAccount(ctx, 7)
	if err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	if out.Applied || len(r.store.events) != 0 {
		t.Fatal("healthy account must be a no-op")
	}
}

// Equity<0 with clean balances means the deficit is positional
// (open-position bleed) — the liquidation engine owns it; NBP pages L1.
func TestNBPPositionalDeficitPagesLiquidationPath(t *testing.T) {
	r := newNBPRig(t)
	ctx := context.Background()
	r.retail(7)
	r.store.bals[7] = []BalanceAmount{
		{Currency: "USD", Available: d("100"), Locked: decimal.Zero},
	}
	r.levels[7] = &MarginLevel{AccountID: 7, Equity: d("-10")}
	out, err := r.svc.EvaluateAccount(ctx, 7)
	if err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	if out.Applied || len(r.store.events) != 0 {
		t.Fatal("positional deficit must not post an nbp event")
	}
	if a := r.alerts.find(CodeNBPDeficitTriggered); a == nil {
		t.Fatalf("expected %s page, got %v", CodeNBPDeficitTriggered, r.alerts.codes())
	}
}

// ---------------------------------------------------------------------------
// Event lifecycle / idempotency
// ---------------------------------------------------------------------------

func TestNBPFundDebitErrorMarksEventFailed(t *testing.T) {
	r := newNBPRig(t)
	ctx := context.Background()
	r.retail(7)
	r.store.bals[7] = []BalanceAmount{
		{Currency: "USD", Available: d("-50"), Locked: decimal.Zero},
	}
	r.fund.bals["USD"] = d("1000")
	r.fund.debitErr = fmt.Errorf("fund tx conflict")
	_, err := r.svc.EvaluateAccount(ctx, 7)
	if err == nil {
		t.Fatal("movement failure must surface")
	}
	if len(r.store.events) != 1 || r.store.events[0].Status != NBPStatusFailed {
		t.Fatalf("event must be FAILED: %+v", r.store.events)
	}
	// Retry reuses the same row (idempotent idempotency key) — count
	// stays 1, and the reused row completes on success.
	r.fund.debitErr = nil
	out, err := r.svc.EvaluateAccount(ctx, 7)
	if err != nil {
		t.Fatalf("retry: %v", err)
	}
	if len(r.store.events) != 1 {
		t.Fatalf("retry must reuse the FAILED row, got %d rows", len(r.store.events))
	}
	if !out.Applied || r.store.events[0].Status != NBPStatusPosted ||
		r.store.events[0].JournalEntryID == nil {
		t.Fatalf("reused event must complete POSTED with journal: %+v", r.store.events[0])
	}
	if r.fund.calls[len(r.fund.calls)-1].ReferenceID != r.store.events[0].ID {
		t.Fatal("reused event id must seed the retried idempotency key")
	}
}

func TestNBPStoreReadFailurePropagates(t *testing.T) {
	r := newNBPRig(t)
	ctx := context.Background()
	r.retail(7)
	r.store.bals[7] = []BalanceAmount{
		{Currency: "USD", Available: d("-50"), Locked: decimal.Zero},
	}
	r.fund.balErr = fmt.Errorf("pg down")
	if _, err := r.svc.EvaluateAccount(ctx, 7); err == nil {
		t.Fatal("unreadable fund balance is an outage, not 'cannot cover' — must fail")
	}
	if len(r.poster.calls) != 0 {
		t.Fatal("house path must not run on a fund read error")
	}
}

// ---------------------------------------------------------------------------
// Abuse detection — recurring hits flag review
// ---------------------------------------------------------------------------

func TestNBPRecurringHitsFlagReview(t *testing.T) {
	r := newNBPRig(t)
	ctx := context.Background()
	r.retail(7)
	r.store.bals[7] = []BalanceAmount{
		{Currency: "USD", Available: d("-40"), Locked: decimal.Zero},
	}
	r.fund.bals["USD"] = d("10000")
	// One prior POSTED hit inside the 90-day window.
	if _, err := r.store.InsertNBPEvent(ctx, NBPEvent{
		AccountID: 7, Currency: "USD", Shortfall: d("10"),
		FundingSource: NBPSourceInsuranceFund, Status: NBPStatusPosted,
	}); err != nil {
		t.Fatal(err)
	}
	jid := int64(700)
	if err := r.store.CompleteNBPEvent(ctx, 1, jid, NBPSourceInsuranceFund); err != nil {
		t.Fatal(err)
	}

	out, err := r.svc.EvaluateAccount(ctx, 7)
	if err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	if !out.ReviewFlagged {
		t.Fatal("second hit inside 90d must flag the account for review")
	}
	if a := r.alerts.find(codeNBPAbuseReview); a == nil || a.Severity != SeverityP1 {
		t.Fatalf("expected P1 %s, got %v", codeNBPAbuseReview, r.alerts.codes())
	}
}

func TestNBPSingleHitNoFlag(t *testing.T) {
	r := newNBPRig(t)
	ctx := context.Background()
	r.retail(7)
	r.store.bals[7] = []BalanceAmount{
		{Currency: "USD", Available: d("-40"), Locked: decimal.Zero},
	}
	r.fund.bals["USD"] = d("10000")
	out, err := r.svc.EvaluateAccount(ctx, 7)
	if err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	if out.ReviewFlagged || r.alerts.find(codeNBPAbuseReview) != nil {
		t.Fatal("a single hit must not flag review")
	}
}

// A hit older than the window does not count toward the flag.
func TestNBPAncientHitDoesNotCount(t *testing.T) {
	r := newNBPRig(t)
	ctx := context.Background()
	r.retail(7)
	r.store.bals[7] = []BalanceAmount{
		{Currency: "USD", Available: d("-40"), Locked: decimal.Zero},
	}
	r.fund.bals["USD"] = d("10000")
	r.clock.Add(-120 * 24 * time.Hour)
	if _, err := r.store.InsertNBPEvent(ctx, NBPEvent{
		AccountID: 7, Currency: "USD", Shortfall: d("10"),
		FundingSource: NBPSourceInsuranceFund, Status: NBPStatusPosted,
	}); err != nil {
		t.Fatal(err)
	}
	r.clock.Add(120 * 24 * time.Hour) // back to the present
	out, err := r.svc.EvaluateAccount(ctx, 7)
	if err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	if out.ReviewFlagged {
		t.Fatal("a 120-day-old hit must not count toward the 90-day window")
	}
}

// ---------------------------------------------------------------------------
// Sweep — the daily session-boundary pass
// ---------------------------------------------------------------------------

func TestNBPSweepRestitutesOnlyRetail(t *testing.T) {
	r := newNBPRig(t)
	ctx := context.Background()
	r.retail(7)
	r.store.cats[11] = nbpCat{cat: "PROFESSIONAL", nbp: false}
	r.store.bals[7] = []BalanceAmount{
		{Currency: "USD", Available: d("-60"), Locked: decimal.Zero},
	}
	r.store.bals[11] = []BalanceAmount{
		{Currency: "USD", Available: d("-60"), Locked: decimal.Zero},
	}
	r.levels[7] = &MarginLevel{AccountID: 7, Equity: d("-60")}
	r.levels[11] = &MarginLevel{AccountID: 11, Equity: d("-60")}
	r.store.deficit = []int64{7, 11}
	r.fund.bals["USD"] = d("10000")

	n, err := r.svc.SweepOnce(ctx)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if n != 1 {
		t.Fatalf("expected 1 restituted account, got %d", n)
	}
	if got := r.store.total(7, "USD"); !got.IsZero() {
		t.Fatalf("retail acct total %s", got)
	}
	if got := r.store.total(11, "USD"); !got.Equal(d("-60")) {
		t.Fatalf("professional acct must stay liable: %s", got)
	}
}

// ---------------------------------------------------------------------------
// Admin read surface
// ---------------------------------------------------------------------------

func TestNBPEventsAdminRead(t *testing.T) {
	r := newNBPRig(t)
	ctx := context.Background()
	for i, ev := range []NBPEvent{
		{AccountID: 7, Currency: "USD", Shortfall: d("10"),
			FundingSource: NBPSourceInsuranceFund, Status: NBPStatusPosted},
		{AccountID: 7, Currency: "EUR", Shortfall: d("5"),
			FundingSource: NBPSourceHousePnL, Status: NBPStatusPosted},
		{AccountID: 9, Currency: "USD", Shortfall: d("99"),
			FundingSource: NBPSourceInsuranceFund, Status: NBPStatusFailed},
	} {
		if _, err := r.store.InsertNBPEvent(ctx, ev); err != nil {
			t.Fatalf("seed %d: %v", i, err)
		}
	}
	all, err := r.svc.NBPEvents(ctx, 0, 0)
	if err != nil || len(all) != 3 {
		t.Fatalf("all-account read: %v %v", len(all), err)
	}
	acct, err := r.svc.NBPEvents(ctx, 7, 0)
	if err != nil || len(acct) != 2 {
		t.Fatalf("account filter: %v %v", len(acct), err)
	}
	if acct[0].ID < acct[1].ID {
		t.Fatal("events must be newest-first")
	}
	lim, err := r.svc.NBPEvents(ctx, 0, 1)
	if err != nil || len(lim) != 1 {
		t.Fatalf("limit: %v %v", len(lim), err)
	}
}

// ---------------------------------------------------------------------------
// Integration — Redis (EXC_REDIS_TEST=1) and Postgres (EXC_PG_TEST=1)
// ---------------------------------------------------------------------------

// TestRedisNBPReviewFlag proves the abuse flag lands on the real
// nbp:review:{account_id} key after the second hit. Run:
// EXC_REDIS_TEST=1 go test ./internal/risk/ -run TestRedisNBP -v
func TestRedisNBPReviewFlag(t *testing.T) {
	if os.Getenv("EXC_REDIS_TEST") != "1" {
		t.Skip("EXC_REDIS_TEST not set")
	}
	addr := os.Getenv("EXC_REDIS_TEST_ADDR")
	if addr == "" {
		addr = "127.0.0.1:16379"
	}
	rdb := excredis.New(addr, os.Getenv("EXC_REDIS_TEST_PASSWORD"), 14)
	ctx := context.Background()
	if err := rdb.Ping(ctx); err != nil {
		t.Fatalf("redis ping: %v", err)
	}
	if err := rdb.FlushDB(ctx).Err(); err != nil {
		t.Fatalf("redis flushdb: %v", err)
	}
	t.Cleanup(func() { _ = rdb.Close() })

	clock := newFakeClock()
	store := newMemNBPStore(clock.Now)
	fund := &fakeNBPFunder{bals: map[string]decimal.Decimal{"USD": d("10000")}}
	fund.apply = func(m FundMovement) { store.creditWallet(m.AccountID, m.Currency, m.Amount) }
	svc, err := NewNBPService(NBPDeps{
		Store: store, Redis: rdb, Fund: fund, Poster: &fakeNBPPoster{},
		Now: clock.Now,
	})
	if err != nil {
		t.Fatal(err)
	}
	store.cats[7] = nbpCat{cat: "RETAIL", nbp: true}
	// Prior POSTED hit inside the window.
	evID, err := store.InsertNBPEvent(ctx, NBPEvent{
		AccountID: 7, Currency: "USD", Shortfall: d("10"),
		FundingSource: NBPSourceInsuranceFund, Status: NBPStatusPosted})
	if err != nil {
		t.Fatal(err)
	}
	jid := int64(42)
	if err := store.CompleteNBPEvent(ctx, evID, jid, NBPSourceInsuranceFund); err != nil {
		t.Fatal(err)
	}
	store.bals[7] = []BalanceAmount{
		{Currency: "USD", Available: d("-25"), Locked: decimal.Zero},
	}
	out, err := svc.EvaluateAccount(ctx, 7)
	if err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	if !out.ReviewFlagged {
		t.Fatal("second hit must flag review")
	}
	got, err := rdb.Get(ctx, NBPReviewKey(7)).Result()
	if err != nil || got == "" {
		t.Fatalf("nbp:review:7 must exist in redis: %v %q", err, got)
	}
}

// TestPgNBPStoreIntegration round-trips the full service against real
// migration-230 schema: account + negative balance + margin row →
// EvaluateAccount posts through fake funding seams but every nbp_events
// read/write is real PG. Run:
// EXC_PG_TEST=1 go test ./internal/risk/ -run TestPgNBP -v
func TestPgNBPStoreIntegration(t *testing.T) {
	if os.Getenv("EXC_PG_TEST") != "1" {
		t.Skip("EXC_PG_TEST not set")
	}
	dsn := os.Getenv("EXC_PG_DSN")
	if dsn == "" {
		dsn = "postgres://exchange:exchange_dev@localhost:5433/exchange?sslmode=disable"
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("pg pool: %v", err)
	}
	defer pool.Close()
	var has bool
	if err := pool.QueryRow(ctx,
		`SELECT to_regclass('nbp_events') IS NOT NULL
		   AND to_regclass('margin_accounts') IS NOT NULL`).Scan(&has); err != nil {
		t.Fatalf("regclass: %v", err)
	}
	if !has {
		t.Skip("migration 230/013 not applied to this database")
	}

	var userID, acctID int64
	if err := pool.QueryRow(ctx,
		`INSERT INTO users (email) VALUES ($1) RETURNING id`,
		fmt.Sprintf("nbp-itest-%d@example.test", time.Now().UnixNano())).Scan(&userID); err != nil {
		t.Fatalf("user seed: %v", err)
	}
	if err := pool.QueryRow(ctx, `
		INSERT INTO accounts (user_id, account_type, client_category, nbp, base_currency)
		VALUES ($1,'MARGIN','RETAIL',true,'USD') RETURNING id`, userID).Scan(&acctID); err != nil {
		t.Fatalf("account seed: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO balances (account_id, currency, available, locked)
		VALUES ($1,'USD',-25.5,0)`, acctID); err != nil {
		t.Fatalf("balance seed: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO margin_accounts (account_id, margin_mode, equity, used_margin)
		VALUES ($1,'CROSS',-25.5,0)`, acctID); err != nil {
		t.Fatalf("margin seed: %v", err)
	}
	t.Cleanup(func() {
		c := context.Background()
		_, _ = pool.Exec(c, `DELETE FROM nbp_events WHERE account_id=$1`, acctID)
		_, _ = pool.Exec(c, `DELETE FROM margin_accounts WHERE account_id=$1`, acctID)
		_, _ = pool.Exec(c, `DELETE FROM balances WHERE account_id=$1`, acctID)
		_, _ = pool.Exec(c, `DELETE FROM accounts WHERE id=$1`, acctID)
		_, _ = pool.Exec(c, `DELETE FROM users WHERE id=$1`, userID)
	})

	st, err := NewPgNBPStore(pool)
	if err != nil {
		t.Fatal(err)
	}
	cat, nbp, err := st.AccountCategory(ctx, acctID)
	if err != nil || cat != "RETAIL" || !nbp {
		t.Fatalf("category: %q %v %v", cat, nbp, err)
	}
	eq, err := st.MarginEquity(ctx, acctID)
	if err != nil || eq == nil || !eq.Equal(d("-25.5")) {
		t.Fatalf("margin equity: %v %v", eq, err)
	}
	deficit, err := st.RetailDeficitAccounts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, id := range deficit {
		if id == acctID {
			found = true
		}
	}
	if !found {
		t.Fatalf("seeded retail deficit acct %d not in sweep set %v", acctID, deficit)
	}

	// Full service pass against the real store — funding seams faked so
	// the wallet side of the movement is not executed here (the real
	// fund service posts it in production); the nbp_events lifecycle is
	// what this leg proves. A real journal_entries row is seeded because
	// nbp_events.journal_entry_id is FK-constrained.
	var jid int64
	if err := pool.QueryRow(ctx, `
		INSERT INTO journal_entries (entry_type, description, posted_by)
		VALUES ('ADJUSTMENT','nbp itest journal','nbp-itest') RETURNING id`).Scan(&jid); err != nil {
		t.Fatalf("journal seed: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM journal_entries WHERE id=$1`, jid)
	})
	spy := &nbpAlertSpy{}
	fund := &fakeNBPFunder{bals: map[string]decimal.Decimal{"USD": d("500")}, fixedJID: jid}
	svc, err := NewNBPService(NBPDeps{
		Store: st, Fund: fund, Poster: &fakeNBPPoster{fixedJID: jid}, Alerter: spy})
	if err != nil {
		t.Fatal(err)
	}
	out, err := svc.EvaluateAccount(ctx, acctID)
	if err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	if !out.Applied || len(out.Events) != 1 {
		t.Fatalf("restitution expected: %+v", out)
	}
	ev := out.Events[0]
	if ev.FundingSource != NBPSourceInsuranceFund || ev.JournalEntryID == nil ||
		*ev.JournalEntryID != jid {
		t.Fatalf("event row: %+v", ev)
	}
	// Read-back proves the persisted row, not just the in-memory return.
	rows, err := svc.NBPEvents(ctx, acctID, 10)
	if err != nil || len(rows) != 1 {
		t.Fatalf("admin read: %v %v", len(rows), err)
	}
	if rows[0].JournalEntryID == nil || rows[0].Status != NBPStatusPosted ||
		!rows[0].Shortfall.Equal(d("25.5")) || rows[0].DeficitEquity == nil {
		t.Fatalf("persisted row wrong: %+v", rows[0])
	}
	// Second pass on the still-negative row mints a second event → the
	// 90-day count crosses the threshold and the review alert fires.
	out2, err := svc.EvaluateAccount(ctx, acctID)
	if err != nil {
		t.Fatalf("second evaluate: %v", err)
	}
	if !out2.ReviewFlagged {
		t.Fatal("second POSTED hit inside 90d must flag review")
	}
	n, err := st.CountNBPEvents(ctx, acctID, time.Now().AddDate(0, 0, -90))
	if err != nil || n != 2 {
		t.Fatalf("count: %d %v", n, err)
	}
}
