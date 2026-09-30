// Task 24.3.7 unit tests — PB give-up reconciliation, break detection,
// KPI alerting, guarded status transitions, atomic collateral rebalance.

package backoffice

import (
	"context"
	"fmt"
	"testing"
	"time"

	"exchange/pkg/decimal"
)

// ---------------------------------------------------------------------------
// In-memory PBReconStore/PBReconTx fake
// ---------------------------------------------------------------------------

type soCollBal struct {
	avail, locked decimal.Decimal
}

type soReconStore struct {
	giveups    map[int64]*GiveUpRow // giveup id
	trades     map[int64]*TradeRow
	runs       []PBReconRun
	breaks     []PBReconBreak
	events     []string
	tolerances map[string]PBBreakTolerance
	balances   map[string]*soCollBal // "acct:ccy"
	nextID     int64
}

func newSoReconStore() *soReconStore {
	return &soReconStore{
		giveups: map[int64]*GiveUpRow{}, trades: map[int64]*TradeRow{},
		tolerances: map[string]PBBreakTolerance{}, balances: map[string]*soCollBal{},
	}
}

// seedGiveUp registers a PENDING give-up row.
func (s *soReconStore) seedGiveUp(tradeID, pbID, execAcct, clientAcct int64,
	traiana string, status string, created time.Time) int64 {
	s.nextID++
	s.giveups[s.nextID] = &GiveUpRow{ID: s.nextID, TradeID: tradeID,
		PrimeBrokerID: pbID, ExecutingAcctID: execAcct, ClientAcctID: clientAcct,
		Status: status, TraianaMessageID: traiana, CreatedAt: created}
	return s.nextID
}

func (s *soReconStore) InTx(ctx context.Context, fn func(context.Context, PBReconTx) error) error {
	return fn(ctx, s)
}

func (s *soReconStore) GiveUpsFor(_ context.Context, pbID int64, _ time.Time) ([]GiveUpRow, error) {
	var out []GiveUpRow
	for _, g := range s.giveups {
		if g.PrimeBrokerID == pbID && g.Status != "SETTLED" {
			out = append(out, *g)
		}
	}
	return out, nil
}

func (s *soReconStore) TradeByID(_ context.Context, id int64) (TradeRow, bool, error) {
	if t, ok := s.trades[id]; ok {
		return *t, true, nil
	}
	return TradeRow{}, false, nil
}

func (s *soReconStore) ToleranceFor(_ context.Context, ccy string) (PBBreakTolerance, error) {
	if tol, ok := s.tolerances[ccy]; ok {
		return tol, nil
	}
	// Same default as PgxReconStore.
	return PBBreakTolerance{Currency: ccy,
		AmountAbs:       decimal.NewFromInt(1000),
		AmountPct:       decimal.NewFromFloat(0.0001),
		RateToleranceBP: decimal.NewFromFloat(0.5)}, nil
}

func (s *soReconStore) InsertRun(_ context.Context, r PBReconRun) (PBReconRun, error) {
	cp := r
	cp.ID = int64(len(s.runs) + 1)
	cp.CreatedAt = time.Now()
	s.runs = append(s.runs, cp)
	return cp, nil
}

func (s *soReconStore) InsertBreak(_ context.Context, b PBReconBreak) (PBReconBreak, bool, error) {
	for _, e := range s.breaks {
		matchGID := (e.GiveUpTradeID == nil && b.GiveUpTradeID == nil) ||
			(e.GiveUpTradeID != nil && b.GiveUpTradeID != nil &&
				*e.GiveUpTradeID == *b.GiveUpTradeID)
		if matchGID && e.Type == b.Type &&
			(e.Status == PBBreakOpen || e.Status == PBBreakInvestigating) {
			return e, false, nil
		}
	}
	cp := b
	cp.ID = int64(len(s.breaks) + 1)
	cp.CreatedAt = time.Now()
	s.breaks = append(s.breaks, cp)
	return cp, true, nil
}

func (s *soReconStore) LockBreak(_ context.Context, id int64) (PBReconBreak, bool, error) {
	for i := range s.breaks {
		if s.breaks[i].ID == id {
			return s.breaks[i], true, nil
		}
	}
	return PBReconBreak{}, false, nil
}

func (s *soReconStore) ResolveBreak(_ context.Context, id int64, note string,
	resolverID int64, at time.Time) error {
	for i := range s.breaks {
		if s.breaks[i].ID == id {
			b := &s.breaks[i]
			if b.Status == PBBreakResolved || b.Status == PBBreakWrittenOff {
				return fmt.Errorf("conflict")
			}
			b.Status = PBBreakResolved
			b.ResolutionNote = note
			b.ResolvedBy = &resolverID
			b.ResolvedAt = &at
			return nil
		}
	}
	return fmt.Errorf("not found")
}

func (s *soReconStore) AssignBreak(_ context.Context, id, assignee int64, _ time.Time) error {
	for i := range s.breaks {
		if s.breaks[i].ID == id {
			s.breaks[i].Status = PBBreakInvestigating
			s.breaks[i].AssignedTo = &assignee
			return nil
		}
	}
	return fmt.Errorf("not found")
}

func (s *soReconStore) BreaksFor(_ context.Context, pbID int64, _ time.Time) ([]PBReconBreak, error) {
	var out []PBReconBreak
	for _, b := range s.breaks {
		if pbID > 0 && b.PrimeBrokerID != pbID {
			continue
		}
		if b.Status == PBBreakOpen || b.Status == PBBreakInvestigating {
			out = append(out, b)
		}
	}
	return out, nil
}

func (s *soReconStore) RunsFor(_ context.Context, pbID int64, day time.Time) ([]PBReconRun, error) {
	var out []PBReconRun
	for _, r := range s.runs {
		if (pbID == 0 || r.PrimeBrokerID == pbID) && r.RunDate.Equal(day) {
			out = append(out, r)
		}
	}
	return out, nil
}

func (s *soReconStore) GiveUpForUpdate(_ context.Context, id int64) (GiveUpRow, bool, error) {
	if g, ok := s.giveups[id]; ok {
		return *g, true, nil
	}
	return GiveUpRow{}, false, nil
}

var soGiveupTransitions = map[string][]string{
	"PENDING":  {"AFFIRMED", "REJECTED", "DISPUTED"},
	"AFFIRMED": {"DISPUTED", "SETTLED"},
	"REJECTED": {"DISPUTED"},
	"DISPUTED": {"AFFIRMED", "REJECTED", "SETTLED"},
	"SETTLED":  {},
}

func (s *soReconStore) SetGiveUpStatus(_ context.Context, id int64, to, reason string) error {
	g := s.giveups[id]
	if g == nil {
		return fmt.Errorf("not found")
	}
	if g.Status == to {
		return nil
	}
	for _, l := range soGiveupTransitions[g.Status] {
		if l == to {
			g.Status = to
			return nil
		}
	}
	return fmt.Errorf("conflict: %s -> %s", g.Status, to)
}

func (s *soReconStore) AppendReconEvent(_ context.Context, _ *int64, _ *int64,
	_ *int64, action string, _ map[string]any) error {
	s.events = append(s.events, action)
	return nil
}

// MoveLocked models the atomic collateral move: dest headroom check
// FIRST (abort without mutation), then clamped release + lock.
func (s *soReconStore) MoveLocked(_ context.Context, giveUpID, fromAcct, toAcct int64,
	ccy string, releaseAmt, lockAmt decimal.Decimal) error {
	key := func(a int64) string { return fmt.Sprintf("%d:%s", a, ccy) }
	dst, ok := s.balances[key(toAcct)]
	if !ok {
		dst = &soCollBal{}
		s.balances[key(toAcct)] = dst
	}
	if lockAmt.IsPositive() && dst.avail.LessThan(lockAmt) {
		return fmt.Errorf("INSUFFICIENT_MARGIN")
	}
	src, ok := s.balances[key(fromAcct)]
	if !ok {
		src = &soCollBal{}
		s.balances[key(fromAcct)] = src
	}
	release := releaseAmt
	if release.GreaterThan(src.locked) {
		release = src.locked
	}
	if release.IsPositive() {
		src.locked = src.locked.Sub(release)
		src.avail = src.avail.Add(release)
	}
	if lockAmt.IsPositive() {
		dst.locked = dst.locked.Add(lockAmt)
		dst.avail = dst.avail.Sub(lockAmt)
	}
	return nil
}

type soFeed struct{ recs []AffirmationRecord }

func (f *soFeed) Fetch(_ context.Context, _ int64, _ time.Time) ([]AffirmationRecord, error) {
	return f.recs, nil
}

func soTradeRow(id int64, symbol, price, qty string) *TradeRow {
	return &TradeRow{ID: id, Symbol: symbol,
		Price: decimal.RequireFromString(price), Quantity: decimal.RequireFromString(qty)}
}

type soAlert struct{ code, summary string }

// ---------------------------------------------------------------------------
// Reconcile
// ---------------------------------------------------------------------------

func TestPBRecon_AutoMatchAffirms(t *testing.T) {
	st := newSoReconStore()
	st.trades[5] = soTradeRow(5, "EURUSD", "1.1000", "1000000")
	gid := st.seedGiveUp(5, 2, 7, 42, "TRM-1", "PENDING", time.Now())
	svc := NewPBReconService(st, PBReconOptions{})
	run, brks, err := svc.Reconcile(context.Background(), 2,
		time.Now().Truncate(24*time.Hour), "AFFIRMATION", &soFeed{
			recs: []AffirmationRecord{{
				ExternalTradeID: "TRM-1", Pair: "EURUSD",
				Rate:     decimal.RequireFromString("1.1000"),
				Notional: decimal.RequireFromString("1000000"), Status: FeedAffirmed}},
		})
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if run.TradesScanned != 1 || run.AutoMatched != 1 ||
		!run.AutoMatchRate.Equal(decimal.NewFromInt(100)) {
		t.Fatalf("bad run %+v", run)
	}
	if len(brks) != 0 {
		t.Fatalf("unexpected breaks: %+v", brks)
	}
	if st.giveups[gid].Status != "AFFIRMED" {
		t.Fatalf("give-up not affirmed: %s", st.giveups[gid].Status)
	}
}

func TestPBRecon_RateMismatchBeyondToleranceDisputes(t *testing.T) {
	st := newSoReconStore()
	st.trades[5] = soTradeRow(5, "EURUSD", "1.1000", "1000000")
	gid := st.seedGiveUp(5, 2, 7, 42, "TRM-1", "PENDING", time.Now())
	svc := NewPBReconService(st, PBReconOptions{})
	_, brks, err := svc.Reconcile(context.Background(), 2, time.Now(), "AFFIRMATION", &soFeed{
		recs: []AffirmationRecord{{
			ExternalTradeID: "TRM-1", Pair: "EURUSD",
			Rate:     decimal.RequireFromString("1.2000"), // ~909bp off
			Notional: decimal.RequireFromString("1000000"), Status: FeedAffirmed}},
	})
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if len(brks) != 1 || brks[0].Type != BreakRateMismatch {
		t.Fatalf("want RATE_MISMATCH break, got %+v", brks)
	}
	if st.giveups[gid].Status != "DISPUTED" {
		t.Fatalf("give-up not DISPUTED: %s", st.giveups[gid].Status)
	}
}

func TestPBRecon_QuantityMismatchAndMissingTicket(t *testing.T) {
	st := newSoReconStore()
	st.trades[5] = soTradeRow(5, "EURUSD", "1.1000", "1000000")
	st.seedGiveUp(5, 2, 7, 42, "TRM-1", "PENDING", time.Now())
	st.seedGiveUp(9, 2, 7, 42, "TRM-2", "PENDING", time.Now()) // trade 9 does not exist
	svc := NewPBReconService(st, PBReconOptions{})
	_, brks, err := svc.Reconcile(context.Background(), 2, time.Now(), "AFFIRMATION", &soFeed{
		recs: []AffirmationRecord{
			{ExternalTradeID: "TRM-1", Pair: "EURUSD", Rate: decimal.RequireFromString("1.1000"),
				Notional: decimal.RequireFromString("2000000"), Status: FeedAffirmed},
			{ExternalTradeID: "TRM-2", Pair: "EURUSD", Rate: decimal.RequireFromString("1.1"),
				Notional: decimal.RequireFromString("5"), Status: FeedAffirmed},
		},
	})
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	seen := map[BreakType]bool{}
	for _, b := range brks {
		seen[b.Type] = true
	}
	if !seen[BreakQuantityMismatch] || !seen[BreakMissingTicket] {
		t.Fatalf("want QUANTITY_MISMATCH + MISSING_TICKET, got %v", seen)
	}
}

func TestPBRecon_PairMismatch(t *testing.T) {
	st := newSoReconStore()
	st.trades[5] = soTradeRow(5, "EURUSD", "1.1000", "1000000")
	st.seedGiveUp(5, 2, 7, 42, "TRM-1", "PENDING", time.Now())
	svc := NewPBReconService(st, PBReconOptions{})
	_, brks, err := svc.Reconcile(context.Background(), 2, time.Now(), "AFFIRMATION", &soFeed{
		recs: []AffirmationRecord{{
			// PB affirmed the wrong instrument entirely.
			ExternalTradeID: "TRM-1", Pair: "GBPUSD", Rate: decimal.RequireFromString("1.2700"),
			Notional: decimal.RequireFromString("1000000"), Status: FeedAffirmed}},
	})
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if len(brks) != 1 || brks[0].Type != BreakPairMismatch {
		t.Fatalf("want PAIR_MISMATCH, got %+v", brks)
	}
}

func TestPBRecon_UnaffirmedTimeoutAndMissingAtPB(t *testing.T) {
	st := newSoReconStore()
	st.trades[5] = soTradeRow(5, "EURUSD", "1.1000", "1000000")
	st.trades[6] = soTradeRow(6, "EURUSD", "1.1000", "1000000")
	// PENDING 61s ago → UNAFFIRMED_TIMEOUT (AFFIRMATION) / MISSING_AT_PB (BLOTTER).
	old := time.Now().Add(-61 * time.Second)
	st.seedGiveUp(5, 2, 7, 42, "TRM-OLD", "PENDING", old)
	// fresh PENDING → no break.
	st.seedGiveUp(6, 2, 7, 42, "TRM-NEW", "PENDING", time.Now().Add(-10*time.Second))
	svc := NewPBReconService(st, PBReconOptions{})

	_, brks, err := svc.Reconcile(context.Background(), 2, time.Now(), "AFFIRMATION", &soFeed{})
	if err != nil {
		t.Fatalf("affirmation recon: %v", err)
	}
	if len(brks) != 1 || brks[0].Type != BreakUnaffirmedTimeout {
		t.Fatalf("want 1 UNAFFIRMED_TIMEOUT, got %+v", brks)
	}

	st2 := newSoReconStore()
	st2.trades[5] = soTradeRow(5, "EURUSD", "1.1000", "1000000")
	st2.seedGiveUp(5, 2, 7, 42, "TRM-OLD", "PENDING", old)
	svc2 := NewPBReconService(st2, PBReconOptions{})
	_, brks2, err := svc2.Reconcile(context.Background(), 2, time.Now(), "BLOTTER", &soFeed{})
	if err != nil {
		t.Fatalf("blotter recon: %v", err)
	}
	if len(brks2) != 1 || brks2[0].Type != BreakMissingAtPB {
		t.Fatalf("want 1 MISSING_AT_PB, got %+v", brks2)
	}
}

func TestPBRecon_MissingLocallyAndKPIAlert(t *testing.T) {
	st := newSoReconStore()
	var alerts []soAlert
	st.trades[5] = soTradeRow(5, "EURUSD", "1.1000", "100")
	st.trades[6] = soTradeRow(6, "EURUSD", "1.1000", "100")
	st.seedGiveUp(5, 2, 7, 42, "TRM-1", "PENDING", time.Now())
	st.seedGiveUp(6, 2, 7, 42, "TRM-2", "PENDING", time.Now())
	svc := NewPBReconService(st, PBReconOptions{
		OnAlert: func(_ context.Context, code, summary string, _ map[string]any) {
			alerts = append(alerts, soAlert{code, summary})
		}})
	// 1 matched + 1 rate-mismatch + 1 orphan feed record → 2 breaks, 50% KPI.
	run, brks, err := svc.Reconcile(context.Background(), 2, time.Now(), "AFFIRMATION", &soFeed{
		recs: []AffirmationRecord{
			{ExternalTradeID: "TRM-1", Pair: "EURUSD", Rate: decimal.RequireFromString("1.1000"),
				Notional: decimal.RequireFromString("100"), Status: FeedAffirmed},
			{ExternalTradeID: "TRM-2", Pair: "EURUSD", Rate: decimal.RequireFromString("1.5000"),
				Notional: decimal.RequireFromString("100"), Status: FeedAffirmed},
			{ExternalTradeID: "TRM-REMOTE", Pair: "GBPUSD", Rate: decimal.RequireFromString("1.3"),
				Notional: decimal.RequireFromString("10"), Status: FeedAffirmed},
		},
	})
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	seen := map[BreakType]bool{}
	for _, b := range brks {
		seen[b.Type] = true
	}
	if !seen[BreakRateMismatch] || !seen[BreakMissingLocally] {
		t.Fatalf("want RATE_MISMATCH + MISSING_LOCALLY, got %v", seen)
	}
	if !run.AutoMatchRate.Equal(decimal.NewFromInt(50)) {
		t.Fatalf("auto-match rate %s", run.AutoMatchRate)
	}
	var kpi bool
	for _, a := range alerts {
		if a.code == "PB_RECON_MATCH_RATE" {
			kpi = true
		}
	}
	if !kpi {
		t.Fatalf("KPI alert not raised: %v", alerts)
	}
}

func TestPBRecon_IdempotentBreakInsert(t *testing.T) {
	st := newSoReconStore()
	st.trades[5] = soTradeRow(5, "EURUSD", "1.1000", "1000000")
	st.seedGiveUp(5, 2, 7, 42, "TRM-OLD", "PENDING", time.Now().Add(-2*time.Hour))
	svc := NewPBReconService(st, PBReconOptions{})
	feed := &soFeed{}
	for i := 0; i < 2; i++ {
		if _, _, err := svc.Reconcile(context.Background(), 2, time.Now(), "AFFIRMATION", feed); err != nil {
			t.Fatalf("recon %d: %v", i, err)
		}
	}
	if len(st.breaks) != 1 {
		t.Fatalf("idempotency broken: %d breaks", len(st.breaks))
	}
}

func TestPBRecon_NilDepsFailClosed(t *testing.T) {
	svc := NewPBReconService(nil, PBReconOptions{})
	if _, _, err := svc.Reconcile(context.Background(), 1, time.Now(), "AFFIRMATION", &soFeed{}); errCode(err) != CodeServiceDegraded {
		t.Fatalf("want SERVICE_DEGRADED, got %v", err)
	}
	svc2 := NewPBReconService(newSoReconStore(), PBReconOptions{})
	if _, _, err := svc2.Reconcile(context.Background(), 1, time.Now(), "AFFIRMATION", nil); errCode(err) != CodeServiceDegraded {
		t.Fatalf("want SERVICE_DEGRADED for nil feed, got %v", err)
	}
	if _, _, err := svc2.Reconcile(context.Background(), 1, time.Now(), "ODDSOCK", &soFeed{}); errCode(err) != "INVALID_REQUEST" {
		t.Fatalf("want INVALID_REQUEST, got %v", err)
	}
}

// ---------------------------------------------------------------------------
// Break workflow + collateral rebalance
// ---------------------------------------------------------------------------

func TestPBRecon_AssignAndResolveBreak(t *testing.T) {
	st := newSoReconStore()
	st.trades[5] = soTradeRow(5, "EURUSD", "1.1000", "100")
	gid := st.seedGiveUp(5, 2, 7, 42, "TRM-OLD", "PENDING", time.Now().Add(-2*time.Hour))
	svc := NewPBReconService(st, PBReconOptions{})
	if _, _, err := svc.Reconcile(context.Background(), 2, time.Now(), "AFFIRMATION", &soFeed{}); err != nil {
		t.Fatalf("recon: %v", err)
	}
	if len(st.breaks) != 1 {
		t.Fatalf("expected a break")
	}
	b := st.breaks[0]

	// Unknown break → error.
	if err := svc.AssignBreak(context.Background(), b.ID+99, 7); err == nil {
		t.Fatalf("assign on missing break should fail")
	}
	if err := svc.AssignBreak(context.Background(), b.ID, 77); err != nil {
		t.Fatalf("assign: %v", err)
	}
	if st.breaks[0].Status != PBBreakInvestigating || *st.breaks[0].AssignedTo != 77 {
		t.Fatalf("assign failed: %+v", st.breaks[0])
	}
	// Bad disposition.
	if err := svc.ResolveBreakDirect(context.Background(), b.ID, "BUY_IN", "", 7); errCode(err) != "INVALID_REQUEST" {
		t.Fatalf("want INVALID_REQUEST, got %v", err)
	}
	// Legal disposition — flips the give-up.
	if err := svc.ResolveBreakDirect(context.Background(), b.ID, "AFFIRMED", "pb confirmed", 7); err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if st.breaks[0].Status != PBBreakResolved {
		t.Fatalf("break not resolved")
	}
	if st.giveups[gid].Status != "AFFIRMED" {
		t.Fatalf("give-up not affirmed: %s", st.giveups[gid].Status)
	}
	// Double resolve → PB_RECON_BREAK_CONFLICT.
	if err := svc.ResolveBreakDirect(context.Background(), b.ID, "RESOLVED", "again", 7); errCode(err) != CodeBreakConflict {
		t.Fatalf("want %s, got %v", CodeBreakConflict, err)
	}
}

func TestPBRecon_AffirmMovesCollateralAtomically(t *testing.T) {
	st := newSoReconStore()
	gid := st.seedGiveUp(5, 2, 7, 42, "TRM-1", "PENDING", time.Now())
	st.balances["7:USD"] = &soCollBal{avail: decimal.RequireFromString("2000"),
		locked: decimal.RequireFromString("600")}
	st.balances["42:USD"] = &soCollBal{avail: decimal.RequireFromString("1000")}
	svc := NewPBReconService(st, PBReconOptions{})
	if err := svc.AffirmAndRebalance(context.Background(), gid, "USD",
		decimal.RequireFromString("600"), decimal.RequireFromString("600")); err != nil {
		t.Fatalf("affirm: %v", err)
	}
	if st.giveups[gid].Status != "AFFIRMED" {
		t.Fatalf("status %s", st.giveups[gid].Status)
	}
	exec := st.balances["7:USD"]
	client := st.balances["42:USD"]
	if !exec.avail.Equal(decimal.RequireFromString("2600")) || !exec.locked.IsZero() {
		t.Fatalf("exec balance not released: %+v", exec)
	}
	if !client.locked.Equal(decimal.RequireFromString("600")) ||
		!client.avail.Equal(decimal.RequireFromString("400")) {
		t.Fatalf("client margin not locked: %+v", client)
	}
}

func TestPBRecon_InsufficientMarginAbortsClean(t *testing.T) {
	st := newSoReconStore()
	gid := st.seedGiveUp(5, 2, 7, 42, "TRM-1", "PENDING", time.Now())
	st.balances["7:USD"] = &soCollBal{avail: decimal.RequireFromString("2000"),
		locked: decimal.RequireFromString("600")}
	st.balances["42:USD"] = &soCollBal{avail: decimal.RequireFromString("100")}
	svc := NewPBReconService(st, PBReconOptions{})
	err := svc.AffirmAndRebalance(context.Background(), gid, "USD",
		decimal.RequireFromString("600"), decimal.RequireFromString("600"))
	if err == nil {
		t.Fatalf("want INSUFFICIENT_MARGIN abort")
	}
	if st.giveups[gid].Status != "PENDING" {
		t.Fatalf("give-up mutated on failed rebalance")
	}
	if !st.balances["7:USD"].locked.Equal(decimal.RequireFromString("600")) {
		t.Fatalf("executing balance touched on abort")
	}
	if !st.balances["42:USD"].avail.Equal(decimal.RequireFromString("100")) {
		t.Fatalf("client balance touched on abort")
	}
}

func TestPBRecon_ReportAggregates(t *testing.T) {
	st := newSoReconStore()
	day := time.Now().UTC().Truncate(24 * time.Hour)
	st.trades[5] = soTradeRow(5, "EURUSD", "1.1000", "100")
	st.seedGiveUp(5, 2, 7, 42, "TRM-1", "PENDING", time.Now())
	svc := NewPBReconService(st, PBReconOptions{})
	if _, _, err := svc.Reconcile(context.Background(), 2, day, "AFFIRMATION", &soFeed{
		recs: []AffirmationRecord{{
			ExternalTradeID: "TRM-1", Pair: "EURUSD", Rate: decimal.RequireFromString("1.1000"),
			Notional: decimal.RequireFromString("100"), Status: FeedAffirmed}},
	}); err != nil {
		t.Fatalf("recon: %v", err)
	}
	rep, err := svc.Report(context.Background(), 2, day)
	if err != nil {
		t.Fatalf("report: %v", err)
	}
	if len(rep.Runs) != 1 || rep.AutoMatchRate != "100.0000" {
		t.Fatalf("bad report %+v", rep)
	}
}
