// roll_test.go — unit coverage for Task 22.3.8 roll management:
// request validation, close+open atomicity (copy-on-write rollback),
// roll pricing (roll_price = open−close), idempotent replay, the
// per-account auto-roll config + sweep, and the HTTP handler seam.
package derivatives

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"exchange/internal/auth"
	"exchange/internal/oracle"
	"exchange/internal/oracle/rates"
	"exchange/pkg/decimal"
	excerrors "exchange/pkg/errors"
)

// withTestClaims attaches an authenticated trading-account context.
func withTestClaims(ctx context.Context, accountID int64) context.Context {
	return auth.WithClaims(ctx, auth.Claims{AccountID: accountID, Scopes: []string{"trade"}})
}

// ---------------------------------------------------------------------------
// In-memory RollStore — mirrors pgxRollTx semantics with copy-on-write
// rollback so tests can prove close+open atomicity.
// ---------------------------------------------------------------------------

type memRollStore struct {
	mu        sync.Mutex
	nextID    int64
	contracts map[int64]*Contract
	byKey     map[string]int64
	legs      map[int64][]PersistedLeg
	legSeq    int64
	rolls     map[int64]*ContractRoll
	rollByKey map[string]int64
	rollSeq   int64
	cfgs      map[int64]*AutoRollConfig
	audits    []string
	cycleDays map[int64]int
	failAt    string // test hook: method name → injected error
}

func newMemRollStore() *memRollStore {
	return &memRollStore{
		nextID: 100, rollSeq: 1,
		contracts: map[int64]*Contract{}, byKey: map[string]int64{},
		legs:  map[int64][]PersistedLeg{},
		rolls: map[int64]*ContractRoll{}, rollByKey: map[string]int64{},
		cfgs: map[int64]*AutoRollConfig{}, cycleDays: map[int64]int{},
	}
}

func (s *memRollStore) clone() *memRollStore {
	c := newMemRollStore()
	c.nextID, c.rollSeq, c.legSeq = s.nextID, s.rollSeq, s.legSeq
	for k, v := range s.contracts {
		cp := *v
		c.contracts[k] = &cp
	}
	for k, v := range s.byKey {
		c.byKey[k] = v
	}
	for k, v := range s.legs {
		c.legs[k] = append([]PersistedLeg(nil), v...)
	}
	for k, v := range s.rolls {
		cp := *v
		c.rolls[k] = &cp
	}
	for k, v := range s.rollByKey {
		c.rollByKey[k] = v
	}
	for k, v := range s.cfgs {
		cp := *v
		c.cfgs[k] = &cp
	}
	c.audits = append([]string(nil), s.audits...)
	for k, v := range s.cycleDays {
		c.cycleDays[k] = v
	}
	c.failAt = s.failAt
	return c
}

// InTx runs fn against a scratch clone; an error discards the clone —
// the observable rollback contract.
func (s *memRollStore) InTx(ctx context.Context, fn func(context.Context, RollTx) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	work := s.clone()
	if err := fn(ctx, &memRollTx{s: work}); err != nil {
		return err
	}
	// Commit: copy the mutable fields, not the mutex.
	s.nextID, s.rollSeq, s.legSeq = work.nextID, work.rollSeq, work.legSeq
	s.contracts, s.byKey = work.contracts, work.byKey
	s.legs, s.rolls, s.rollByKey = work.legs, work.rolls, work.rollByKey
	s.cfgs, s.audits, s.cycleDays = work.cfgs, work.audits, work.cycleDays
	return nil
}

func (s *memRollStore) ReadTx(ctx context.Context, fn func(context.Context, RollTx) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return fn(ctx, &memRollTx{s: s})
}

type memRollTx struct{ s *memRollStore }

func (t *memRollTx) maybeFail(at string) error {
	if t.s.failAt == at {
		return fmt.Errorf("injected failure at %s", at)
	}
	return nil
}

func (t *memRollTx) ContractForUpdate(_ context.Context, id int64) (*Contract, error) {
	if err := t.maybeFail("ContractForUpdate"); err != nil {
		return nil, err
	}
	c, ok := t.s.contracts[id]
	if !ok {
		return nil, excerrors.New(CodeNotFound, fmt.Sprintf("derivative contract %d not found", id))
	}
	cp := *c
	return &cp, nil
}

func (t *memRollTx) InsertContract(_ context.Context, c *Contract) (int64, bool, error) {
	if err := t.maybeFail("InsertContract"); err != nil {
		return 0, false, err
	}
	if c.IdempotencyKey != "" {
		if id, ok := t.s.byKey[c.IdempotencyKey]; ok {
			return id, false, nil
		}
	}
	cp := *c
	cp.ID = t.s.nextID
	t.s.nextID++
	cp.Status = StatusOpen
	t.s.contracts[cp.ID] = &cp
	if cp.IdempotencyKey != "" {
		t.s.byKey[cp.IdempotencyKey] = cp.ID
	}
	return cp.ID, true, nil
}

func (t *memRollTx) InsertLegs(ctx context.Context, legs []SettlementLeg) (int, error) {
	ids, err := t.InsertLegsReturningIDs(ctx, legs)
	return len(ids), err
}

func (t *memRollTx) InsertLegsReturningIDs(_ context.Context, legs []SettlementLeg) ([]int64, error) {
	if err := t.maybeFail("InsertLegs"); err != nil {
		return nil, err
	}
	ids := make([]int64, 0, len(legs))
	for _, l := range legs {
		t.s.legSeq++
		t.s.legs[l.ContractID] = append(t.s.legs[l.ContractID], PersistedLeg{
			ID: t.s.legSeq, Direction: l.Direction, Currency: l.Currency,
			Amount: l.Amount, ValueDate: l.ValueDate, Status: "PENDING",
		})
		ids = append(ids, t.s.legSeq)
	}
	return ids, nil
}

func (t *memRollTx) UpdateContractStatus(_ context.Context, id int64, st ContractStatus, settledAt *time.Time) error {
	if err := t.maybeFail("UpdateContractStatus"); err != nil {
		return err
	}
	c, ok := t.s.contracts[id]
	if !ok {
		return excNotFound(id)
	}
	c.Status = st
	c.SettledAt = settledAt
	return nil
}

func (t *memRollTx) SettlementCycleDays(_ context.Context, instrumentID int64) (int, error) {
	if d, ok := t.s.cycleDays[instrumentID]; ok {
		return d, nil
	}
	return 1, nil
}

func (t *memRollTx) InsertRoll(_ context.Context, r *ContractRoll) (int64, bool, error) {
	if err := t.maybeFail("InsertRoll"); err != nil {
		return 0, false, err
	}
	if r.IdempotencyKey != "" {
		if id, ok := t.s.rollByKey[r.IdempotencyKey]; ok {
			return id, false, nil
		}
	}
	cp := *r
	cp.ID = t.s.rollSeq
	t.s.rollSeq++
	cp.CreatedAt = time.Now().UTC()
	t.s.rolls[cp.ID] = &cp
	if cp.IdempotencyKey != "" {
		t.s.rollByKey[cp.IdempotencyKey] = cp.ID
	}
	return cp.ID, true, nil
}

func (t *memRollTx) CompleteRoll(_ context.Context, r *ContractRoll) error {
	if err := t.maybeFail("CompleteRoll"); err != nil {
		return err
	}
	got, ok := t.s.rolls[r.ID]
	if !ok {
		return fmt.Errorf("roll %d not found", r.ID)
	}
	*got = *r
	now := time.Now().UTC()
	got.CompletedAt = &now
	return nil
}

func (t *memRollTx) LoadRollByKey(_ context.Context, key string) (*ContractRoll, error) {
	id, ok := t.s.rollByKey[key]
	if !ok {
		return nil, nil
	}
	cp := *t.s.rolls[id]
	return &cp, nil
}

func (t *memRollTx) PutAutoRollConfig(_ context.Context, cfg AutoRollConfig) error {
	t.s.cfgs[cfg.AccountID] = &cfg
	return nil
}

func (t *memRollTx) GetAutoRollConfig(_ context.Context, accountID int64) (*AutoRollConfig, error) {
	c, ok := t.s.cfgs[accountID]
	if !ok {
		return nil, nil
	}
	cp := *c
	return &cp, nil
}

func (t *memRollTx) DueAutoRollContracts(_ context.Context) ([]AutoRollDue, error) {
	var out []AutoRollDue
	for _, c := range t.s.contracts {
		cfg, ok := t.s.cfgs[c.AccountID]
		if !ok || !cfg.Enabled {
			continue
		}
		if c.Status != StatusOpen && c.Status != StatusPartiallySettled {
			continue
		}
		if c.Kind != KindForward && c.Kind != KindSwap {
			continue
		}
		out = append(out, AutoRollDue{
			ContractID: c.ID, AccountID: c.AccountID,
			LeadDays: cfg.LeadDays, Tenor: cfg.Tenor, ValueDate: c.ValueDate,
		})
	}
	return out, nil
}

func (t *memRollTx) AuditAppend(_ context.Context, table string, recordID *int64, action string) error {
	t.s.audits = append(t.s.audits, fmt.Sprintf("%s:%d:%s", table, *recordID, action))
	return nil
}

// ---------------------------------------------------------------------------
// Fixtures
// ---------------------------------------------------------------------------

func rollSvc(t *testing.T) (*RollService, *memRollStore) {
	t.Helper()
	store := newMemRollStore()
	cal := testCalendar(t)
	d := NewDates(cal)
	now := time.Now().UTC()
	curves := fakeCurves{m: map[string]rates.Curve{
		"EUR": completeCurve("EUR", 0.04, now),
		"USD": completeCurve("USD", 0.05, now),
	}}
	spot := fakeSpot{m: map[string]oracle.MarkView{
		"EUR/USD": {Price: decimal.RequireFromString("1.10")},
	}}
	p := NewPricer(curves, spot)
	return NewRollService(store, d, p), store
}

func fwdContract(id, acct int64, vd time.Time) *Contract {
	pair, _ := NewPair("EUR", "USD")
	return &Contract{
		ID: id, TradeID: id * 10, AccountID: acct, InstrumentID: 42,
		Kind: KindForward, Side: SideBuy, Pair: pair,
		Notional:      decimal.RequireFromString("1000000"),
		SpotRate:      decimal.RequireFromString("1.09"),
		ForwardRate:   decimal.RequireFromString("1.095"),
		SwapPoints:    decimal.RequireFromString("0.005"),
		SpotValueDate: day(2026, 1, 9), ValueDate: vd,
		Status: StatusOpen, BookedAt: day(2026, 1, 7),
	}
}

// ---------------------------------------------------------------------------
// Validation
// ---------------------------------------------------------------------------

func TestRollValidation(t *testing.T) {
	svc, store := rollSvc(t)
	ctx := context.Background()
	c := fwdContract(1, 7, day(2026, 2, 10))
	store.contracts[1] = c

	cases := []struct {
		name string
		req  RollRequest
		code string
	}{
		{"no contract", RollRequest{AccountID: 7, Tenor: "1M"}, "INVALID_REQUEST"},
		{"no target", RollRequest{AccountID: 7, ContractID: 1}, "INVALID_REQUEST"},
		{"both targets", RollRequest{AccountID: 7, ContractID: 1,
			Tenor: "1M", NewValueDate: ptrTime(day(2026, 3, 10))}, "INVALID_REQUEST"},
		{"bad key", RollRequest{AccountID: 7, ContractID: 1, Tenor: "1M",
			IdempotencyKey: string(make([]byte, 200))}, "INVALID_REQUEST"},
		{"bad source", RollRequest{AccountID: 7, ContractID: 1, Tenor: "1M",
			Source: "BOGUS"}, "INVALID_REQUEST"},
		{"unknown contract", RollRequest{AccountID: 7, ContractID: 99, Tenor: "1M"}, "NOT_FOUND"},
		{"wrong account", RollRequest{AccountID: 8, ContractID: 1, Tenor: "1M"}, "ROLL_NOT_PERMITTED"},
	}
	for _, tc := range cases {
		_, err := svc.Roll(ctx, tc.req)
		assertCode(t, err, tc.code, tc.name)
	}
}

func ptrTime(t time.Time) *time.Time { return &t }

func assertCode(t *testing.T, err error, want, name string) {
	t.Helper()
	if err == nil {
		t.Fatalf("%s: expected %s, got nil", name, want)
	}
	var e *excerrors.Error
	if !errorAs(err, &e) || e.Code != want {
		t.Fatalf("%s: expected code %s, got %v", name, want, err)
	}
}

func errorAs(err error, target **excerrors.Error) bool {
	for err != nil {
		if e, ok := err.(*excerrors.Error); ok {
			*target = e
			return true
		}
		type unwrapper interface{ Unwrap() error }
		u, ok := err.(unwrapper)
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}

// ---------------------------------------------------------------------------
// Happy path + pricing
// ---------------------------------------------------------------------------

func TestRollForwardHappyPath(t *testing.T) {
	svc, store := rollSvc(t)
	svc.SetNowFunc(func() time.Time { return day(2026, 2, 6).Add(10 * time.Hour) })
	ctx := context.Background()
	src := fwdContract(1, 7, day(2026, 2, 10))
	store.contracts[1] = src

	res, err := svc.Roll(ctx, RollRequest{
		AccountID: 7, ContractID: 1, Tenor: "1M",
		IdempotencyKey: "roll-req-1",
	})
	if err != nil {
		t.Fatalf("roll: %v", err)
	}
	if res.Replayed || res.Status != "COMPLETED" {
		t.Fatalf("unexpected result %+v", res)
	}
	// source → ROLLED, target → new OPEN contract with same kind/side/notional
	if got := store.contracts[1].Status; got != StatusRolled {
		t.Fatalf("source status %s, want ROLLED", got)
	}
	tgt := store.contracts[res.TargetContractID]
	if tgt == nil || tgt.Status != StatusOpen || tgt.Kind != KindForward ||
		tgt.Side != src.Side || !tgt.Notional.Equal(src.Notional) {
		t.Fatalf("replacement contract malformed: %+v", tgt)
	}
	if tgt.ValueDate.Format("2006-01-02") != res.TargetValueDate {
		t.Fatalf("target value date mismatch")
	}
	// roll_price = open_rate − close_rate; both priced off the oracle +
	// curves (USD rate > EUR rate ⇒ forward trades below spot).
	if !res.RollPrice.Equal(res.OpenRate.Sub(res.CloseRate)) {
		t.Fatalf("roll_price %s != open-close", res.RollPrice)
	}
	if !res.OpenRate.IsPositive() || !res.CloseRate.IsPositive() {
		t.Fatalf("rates not priced: %+v", res)
	}
	// Closing P&L leg lands on the source contract (quote currency).
	srcLegs := store.legs[src.ID]
	if len(srcLegs) != 1 || srcLegs[0].Currency != "USD" {
		t.Fatalf("expected one close-PnL USD leg, got %+v", srcLegs)
	}
	wantPnL := res.CloseRate.Sub(src.ForwardRate).Mul(src.Notional).Round(8)
	if !srcLegs[0].Amount.Equal(wantPnL.Abs()) {
		t.Fatalf("close P&L leg %s, want %s", srcLegs[0].Amount, wantPnL.Abs())
	}
	// New contract carries its delivery legs.
	if got := len(store.legs[tgt.ID]); got != 2 {
		t.Fatalf("replacement legs %d, want 2", got)
	}
	// Roll row committed.
	roll := store.rolls[res.RollID]
	if roll == nil || roll.Status != "COMPLETED" || roll.TargetContractID != tgt.ID {
		t.Fatalf("roll record %+v", roll)
	}
	// Audit chain appends recorded.
	if len(store.audits) < 3 {
		t.Fatalf("expected ≥3 audit appends, got %v", store.audits)
	}
}

func TestRollSwapReLegs(t *testing.T) {
	svc, store := rollSvc(t)
	svc.SetNowFunc(func() time.Time { return day(2026, 2, 6) })
	ctx := context.Background()
	src := fwdContract(1, 7, day(2026, 2, 10))
	src.Kind = KindSwap
	near := day(2026, 1, 12)
	src.NearLegValueDate = &near
	store.contracts[1] = src

	res, err := svc.Roll(ctx, RollRequest{
		AccountID: 7, ContractID: 1, NewValueDate: ptrTime(day(2026, 3, 12)),
	})
	if err != nil {
		t.Fatalf("swap roll: %v", err)
	}
	tgt := store.contracts[res.TargetContractID]
	if tgt.NearLegValueDate == nil ||
		!tgt.NearLegValueDate.Equal(day(2026, 2, 10)) {
		t.Fatalf("rolled swap near leg must anchor the expiring maturity, got %+v",
			tgt.NearLegValueDate)
	}
	if got := len(store.legs[tgt.ID]); got != 4 {
		t.Fatalf("swap roll legs %d, want 4", got)
	}
}

// ---------------------------------------------------------------------------
// Atomicity
// ---------------------------------------------------------------------------

func TestRollAtomicRollback(t *testing.T) {
	for _, failAt := range []string{"InsertRoll", "InsertContract", "InsertLegs", "UpdateContractStatus", "CompleteRoll"} {
		svc, store := rollSvc(t)
		svc.SetNowFunc(func() time.Time { return day(2026, 2, 6) })
		ctx := context.Background()
		store.contracts[1] = fwdContract(1, 7, day(2026, 2, 10))
		store.failAt = failAt

		_, err := svc.Roll(ctx, RollRequest{AccountID: 7, ContractID: 1, Tenor: "1M"})
		if err == nil {
			t.Fatalf("%s: expected injected failure", failAt)
		}
		if got := store.contracts[1].Status; got != StatusOpen {
			t.Fatalf("%s: source status mutated to %s despite rollback", failAt, got)
		}
		if len(store.contracts) != 1 || len(store.rolls) != 0 || len(store.legs) != 0 {
			t.Fatalf("%s: partial writes leaked past rollback (contracts=%d rolls=%d legs=%d)",
				failAt, len(store.contracts), len(store.rolls), len(store.legs))
		}
	}
}

// ---------------------------------------------------------------------------
// Idempotent replay
// ---------------------------------------------------------------------------

func TestRollIdempotentReplay(t *testing.T) {
	svc, store := rollSvc(t)
	svc.SetNowFunc(func() time.Time { return day(2026, 2, 6) })
	ctx := context.Background()
	store.contracts[1] = fwdContract(1, 7, day(2026, 2, 10))

	req := RollRequest{AccountID: 7, ContractID: 1, Tenor: "1M", IdempotencyKey: "r1"}
	res1, err := svc.Roll(ctx, req)
	if err != nil {
		t.Fatalf("roll: %v", err)
	}
	// Replaying a committed key resolves to the original without a second
	// contract — even though the source is now ROLLED.
	store.contracts[res1.SourceContractID].Status = StatusOpen // pretend re-check
	res2, err := svc.Roll(ctx, req)
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	if !res2.Replayed || res2.RollID != res1.RollID || res2.TargetContractID != res1.TargetContractID {
		t.Fatalf("replay mismatch: %+v vs %+v", res2, res1)
	}
	if n := len(store.contracts); n != 2 {
		t.Fatalf("replay created extra contracts: %d", n)
	}
}

// ---------------------------------------------------------------------------
// Non-rollable classes + tolerance
// ---------------------------------------------------------------------------

func TestRollRejectsTerminalAndNDF(t *testing.T) {
	svc, store := rollSvc(t)
	svc.SetNowFunc(func() time.Time { return day(2026, 2, 6) })
	ctx := context.Background()

	settled := fwdContract(2, 7, day(2026, 2, 10))
	settled.Status = StatusSettled
	store.contracts[2] = settled
	_, err := svc.Roll(ctx, RollRequest{AccountID: 7, ContractID: 2, Tenor: "1M"})
	assertCode(t, err, CodeRollNotPermitted, "settled contract")

	ndf := fwdContract(3, 7, day(2026, 2, 10))
	ndf.Kind = KindNDF
	fix := day(2026, 2, 10)
	ndf.NdfFixingDate = &fix
	ndf.NdfFixingSource = "CENTRAL_BANK"
	ndf.SettlementCurrency = "USD"
	store.contracts[3] = ndf
	_, err = svc.Roll(ctx, RollRequest{AccountID: 7, ContractID: 3, Tenor: "1M"})
	assertCode(t, err, CodeRollNotPermitted, "NDF")
}

func TestRollTargetBeforeSourceRejected(t *testing.T) {
	svc, store := rollSvc(t)
	svc.SetNowFunc(func() time.Time { return day(2026, 2, 6) })
	ctx := context.Background()
	store.contracts[1] = fwdContract(1, 7, day(2026, 6, 10))
	_, err := svc.Roll(ctx, RollRequest{
		AccountID: 7, ContractID: 1, NewValueDate: ptrTime(day(2026, 3, 10)),
	})
	assertCode(t, err, CodeRollTargetInvalid, "backward roll")
}

func TestRollSpreadTolerance(t *testing.T) {
	svc, store := rollSvc(t)
	svc.SetNowFunc(func() time.Time { return day(2026, 2, 6) })
	ctx := context.Background()
	store.contracts[1] = fwdContract(1, 7, day(2026, 2, 10))
	tol := decimal.RequireFromString("1") // 1 bps — the 1M roll spread exceeds it
	_, err := svc.Roll(ctx, RollRequest{
		AccountID: 7, ContractID: 1, Tenor: "1M", MaxRollPriceBps: &tol,
	})
	assertCode(t, err, CodeRollSpreadTolerance, "tolerance")
}

func TestRollStaleMarkFailsClosed(t *testing.T) {
	svc, store := rollSvc(t)
	svc.SetNowFunc(func() time.Time { return day(2026, 2, 6) })
	// Replace the pricer's spot with a stale mark.
	stale := fakeSpot{m: map[string]oracle.MarkView{
		"EUR/USD": {Price: decimal.RequireFromString("1.10"), Stale: true},
	}}
	svc.Pricer = NewPricer(svc.Pricer.Curves, stale)
	store.contracts[1] = fwdContract(1, 7, day(2026, 2, 10))
	_, err := svc.Roll(context.Background(), RollRequest{
		AccountID: 7, ContractID: 1, Tenor: "1M",
	})
	assertCode(t, err, CodePriceOracleUnavailable, "stale mark")
	if got := store.contracts[1].Status; got != StatusOpen {
		t.Fatalf("status mutated on stale mark: %s", got)
	}
}

// ---------------------------------------------------------------------------
// Auto-roll config + sweep
// ---------------------------------------------------------------------------

func TestAutoRollConfigAndSweep(t *testing.T) {
	svc, store := rollSvc(t)
	svc.SetNowFunc(func() time.Time { return day(2026, 2, 6) })
	ctx := context.Background()

	if err := svc.SetAutoRoll(ctx, AutoRollConfig{AccountID: 7, Enabled: true, LeadDays: 3, Tenor: "3M"}); err != nil {
		t.Fatalf("set config: %v", err)
	}
	cfg, err := svc.AutoRoll(ctx, 7)
	if err != nil || cfg == nil || !cfg.Enabled || cfg.Tenor != "3M" || cfg.LeadDays != 3 {
		t.Fatalf("config round trip: %+v err=%v", cfg, err)
	}
	if c, _ := svc.AutoRoll(ctx, 99); c != nil {
		t.Fatalf("unset account must return nil config")
	}
	// invalid configs
	for _, bad := range []AutoRollConfig{
		{AccountID: 0, Enabled: true, Tenor: "1M"},
		{AccountID: 7, Enabled: true, LeadDays: 99, Tenor: "1M"},
		{AccountID: 7, Enabled: true, Tenor: "42D"},
	} {
		if err := svc.SetAutoRoll(ctx, bad); err == nil {
			t.Fatalf("bad config accepted: %+v", bad)
		}
	}

	// due contract (value date within lead window) + not-due contract
	store.contracts[1] = fwdContract(1, 7, day(2026, 2, 8)) // due: today+3 ≥ vd
	store.contracts[2] = fwdContract(2, 7, day(2026, 9, 1)) // not due
	store.contracts[3] = fwdContract(3, 8, day(2026, 2, 8)) // no config — skipped
	rep, err := svc.RunAutoRollSweep(ctx, day(2026, 2, 6))
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if rep.Evaluated != 1 || rep.Rolled != 1 || len(rep.Errors) != 0 {
		t.Fatalf("sweep report %+v", rep)
	}
	if got := store.contracts[1].Status; got != StatusRolled {
		t.Fatalf("due contract not rolled: %s", got)
	}
	if got := store.contracts[2].Status; got != StatusOpen {
		t.Fatalf("future contract rolled early: %s", got)
	}
	// Replay-safe: second sweep rolls nothing.
	rep2, _ := svc.RunAutoRollSweep(ctx, day(2026, 2, 6))
	if rep2.Rolled != 0 {
		t.Fatalf("re-sweep double-rolled: %+v", rep2)
	}
}

// ---------------------------------------------------------------------------
// HTTP handler seam
// ---------------------------------------------------------------------------

func TestRollHandler(t *testing.T) {
	svc, store := rollSvc(t)
	svc.SetNowFunc(func() time.Time { return day(2026, 2, 6) })
	store.contracts[1] = fwdContract(1, 7, day(2026, 2, 10))
	h := RollHandler(svc)

	// unauthenticated
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/orders/roll",
		bytes.NewBufferString(`{"contract_id":1,"tenor":"1M"}`))
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("unauth → %d", rec.Code)
	}

	// authenticated happy path
	body, _ := json.Marshal(map[string]any{
		"contract_id": 1, "tenor": "1M", "account_id": 7, "idempotency_key": "k1",
	})
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPost, "/api/v1/orders/roll", bytes.NewBuffer(body))
	req = req.WithContext(withTestClaims(req.Context(), 7))
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("roll → %d body %s", rec.Code, rec.Body.String())
	}
	var res RollResult
	if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if res.RollID == 0 || res.Status != "COMPLETED" {
		t.Fatalf("bad response %+v", res)
	}

	// cross-account body rejected
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPost, "/api/v1/orders/roll",
		bytes.NewBufferString(`{"contract_id":1,"tenor":"1M","account_id":9}`))
	req = req.WithContext(withTestClaims(req.Context(), 7))
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("cross-account → %d", rec.Code)
	}
}
