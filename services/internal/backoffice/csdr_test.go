// Task 24.3.13 unit tests — ISD+1 fail detection, regime separation,
// CSDR Art. 7 bilateral penalties (CSD scope only), CLS exemption,
// auto-resolve on settlement, daily/monthly reports, buy-in lifecycle.

package backoffice

import (
	"context"
	"fmt"
	"testing"
	"time"

	"exchange/pkg/decimal"
)

// ---------------------------------------------------------------------------
// In-memory FailStore/FailTx + BuyInStore/BuyInTx fakes
// ---------------------------------------------------------------------------

type soFailStore struct {
	legs       map[int64]*ExceptionLeg
	flagged    map[int64]bool
	fails      map[int64]*SettlementFail
	byInstr    map[int64]int64
	penalties  []SettlementPenalty
	buyIns     []BuyInEvent
	journals   []soPostedJournal
	tradePrice map[int64]decimal.Decimal
	nextID     int64
}

func newSoFailStore() *soFailStore {
	return &soFailStore{
		legs: map[int64]*ExceptionLeg{}, flagged: map[int64]bool{},
		fails: map[int64]*SettlementFail{}, byInstr: map[int64]int64{},
		tradePrice: map[int64]decimal.Decimal{},
	}
}

func (s *soFailStore) InTx(ctx context.Context, fn func(context.Context, FailTx) error) error {
	return fn(ctx, s)
}

func (s *soFailStore) LateInstructions(_ context.Context, day time.Time) ([]ExceptionLeg, error) {
	var out []ExceptionLeg
	for _, l := range s.legs {
		// Mirrors PgxFailStore.LateInstructions: PENDING *and* FAILED legs
		// late at ISD+1 are detected once (fail_flag idempotency guard).
		if (l.Status == "PENDING" || l.Status == "FAILED") &&
			l.SettlementDate.Before(day.Truncate(24*time.Hour)) && !s.flagged[l.ID] {
			out = append(out, *l)
		}
	}
	return out, nil
}

func (s *soFailStore) FlagInstruction(_ context.Context, id int64) error {
	s.flagged[id] = true
	return nil
}

func (s *soFailStore) InsertFail(_ context.Context, f SettlementFail) (SettlementFail, bool, error) {
	if id, ok := s.byInstr[f.InstructionID]; ok {
		return *s.fails[id], false, nil
	}
	s.nextID++
	cp := f
	cp.ID = s.nextID
	s.fails[cp.ID] = &cp
	s.byInstr[cp.InstructionID] = cp.ID
	return cp, true, nil
}

func (s *soFailStore) SetFailStatus(_ context.Context, id int64, st FailStatus, at time.Time) error {
	f := s.fails[id]
	if f.Status == st {
		return nil
	}
	f.Status = st
	switch st {
	case FailResolved:
		f.ResolvedAt = &at
	case FailClosedOut:
		f.ClosedOutAt = &at
	case FailBuyInNotified:
		f.BuyInNotifiedAt = &at
	}
	return nil
}

func (s *soFailStore) InstructionSettled(_ context.Context, id int64) (bool, error) {
	l := s.legs[id]
	return l != nil && (l.Status == "SETTLED" || l.Status == "RECONCILED"), nil
}

func (s *soFailStore) AccruePenalty(_ context.Context, p SettlementPenalty) (bool, error) {
	for _, e := range s.penalties {
		if e.FailID == p.FailID && e.AccrualDate.Equal(p.AccrualDate) && e.Direction == p.Direction {
			return false, nil
		}
	}
	s.penalties = append(s.penalties, p)
	return true, nil
}

func (s *soFailStore) openFailsTx(_ context.Context, regime FailRegime) ([]SettlementFail, error) {
	return s.OpenFails(context.Background(), &regime)
}

func (s *soFailStore) FailByID(_ context.Context, id int64) (SettlementFail, bool, error) {
	if f, ok := s.fails[id]; ok {
		return *f, true, nil
	}
	return SettlementFail{}, false, nil
}

func (s *soFailStore) OpenFails(_ context.Context, regime *FailRegime) ([]SettlementFail, error) {
	var out []SettlementFail
	for _, f := range s.fails {
		if f.Status != FailOpen && f.Status != FailBuyInNotified {
			continue
		}
		if regime != nil && f.Regime != *regime {
			continue
		}
		out = append(out, *f)
	}
	return out, nil
}

func (s *soFailStore) FailsInRange(_ context.Context, _, to time.Time) ([]SettlementFail, error) {
	var out []SettlementFail
	for _, f := range s.fails {
		if f.DetectedAt.Before(to) {
			out = append(out, *f)
		}
	}
	return out, nil
}

func (s *soFailStore) PenaltiesInRange(_ context.Context, from, to time.Time) ([]SettlementPenalty, error) {
	var out []SettlementPenalty
	for _, p := range s.penalties {
		if !p.AccrualDate.Before(from) && p.AccrualDate.Before(to) {
			out = append(out, p)
		}
	}
	return out, nil
}

// BuyInStore/BuyInTx — same backing data.

func (s *soFailStore) InBuyInTx(ctx context.Context, fn func(context.Context, BuyInTx) error) error {
	return fn(ctx, s)
}

func (s *soFailStore) CSDRFailsDue(_ context.Context, isdBefore time.Time,
	states []FailStatus) ([]SettlementFail, error) {
	want := map[FailStatus]bool{}
	for _, st := range states {
		want[st] = true
	}
	var out []SettlementFail
	for _, f := range s.fails {
		if f.Regime == RegimeCSDR && !f.ISD.After(isdBefore) && want[f.Status] {
			out = append(out, *f)
		}
	}
	return out, nil
}

func (s *soFailStore) InsertBuyIn(_ context.Context, e BuyInEvent) (BuyInEvent, bool, error) {
	for _, ex := range s.buyIns {
		if ex.FailID == e.FailID && ex.Kind == e.Kind {
			return ex, false, nil
		}
	}
	cp := e
	cp.ID = int64(len(s.buyIns) + 1)
	s.buyIns = append(s.buyIns, cp)
	return cp, true, nil
}

func (s *soFailStore) TradePrice(_ context.Context, tradeID int64) (decimal.Decimal, bool, error) {
	p, ok := s.tradePrice[tradeID]
	return p, ok, nil
}

func (s *soFailStore) PostJournal(_ context.Context, entryType, description,
	postedBy, idemKey string, referenceID int64, lines []JournalLine) (int64, error) {
	s.journals = append(s.journals, soPostedJournal{
		entryType: entryType, description: description, idemKey: idemKey, lines: lines})
	return int64(len(s.journals)), nil
}

// buyInStoreAdapter — buyin.go's BuyInStore has its own InTx signature.
type soBuyInStore struct{ *soFailStore }

func (s soBuyInStore) InTx(ctx context.Context, fn func(context.Context, BuyInTx) error) error {
	return fn(ctx, s.soFailStore)
}

type soClassifier struct{ byLeg map[int64]LegClassification }

func (c soClassifier) Classify(_ context.Context, leg ExceptionLeg) (LegClassification, error) {
	if cl, ok := c.byLeg[leg.ID]; ok {
		return cl, nil
	}
	return LegClassification{Regime: RegimeFXCloseout, Liquidity: LiquidityLiquid}, nil
}

type soPricer struct{ marks map[int64]decimal.Decimal }

func (p soPricer) MarkPrice(_ context.Context, tradeID int64) (decimal.Decimal, error) {
	if m, ok := p.marks[tradeID]; ok {
		return m, nil
	}
	return decimal.Zero, fmt.Errorf("no mark for trade %d", tradeID)
}

func soLateLeg(id, tradeID, acct int64, ccy, amt string, isd time.Time) *ExceptionLeg {
	return &ExceptionLeg{ID: id, TradeID: tradeID, AccountID: acct, Currency: ccy,
		Amount: decimal.RequireFromString(amt), Direction: "RECEIVE",
		SettlementDate: isd, Status: "PENDING"}
}

// ---------------------------------------------------------------------------
// Fail detection (ISD+1 scan) + regime separation
// ---------------------------------------------------------------------------

func TestCSDR_DetectFailsFlagsAndClassifies(t *testing.T) {
	st := newSoFailStore()
	day := time.Now().UTC().Truncate(24 * time.Hour)
	isd := day.Add(-24 * time.Hour) // yesterday → ISD+1 passed
	st.legs[10] = soLateLeg(10, 55, 7, "USD", "1000000", isd)
	st.legs[11] = soLateLeg(11, 56, 7, "EUR", "900000", isd)
	st.legs[12] = soLateLeg(12, 57, 7, "JPY", "500000", day) // not late yet
	st.legs[13] = soLateLeg(13, 58, 7, "GBP", "100", isd)
	st.legs[13].Status = "SETTLED" // settled legs are skipped
	st.legs[14] = soLateLeg(14, 59, 7, "USD", "250000", isd)
	st.legs[14].Status = "FAILED" // already-FAILED legs are still detected at ISD+1

	svc := NewCSDRService(st, soClassifier{byLeg: map[int64]LegClassification{
		11: {Regime: RegimeCSDR, Liquidity: LiquidityIlliquid},
	}})

	fails, err := svc.DetectFails(context.Background(), day)
	if err != nil {
		t.Fatalf("detect: %v", err)
	}
	if len(fails) != 3 {
		t.Fatalf("want 3 fails, got %d", len(fails))
	}
	if !st.flagged[10] || !st.flagged[11] || !st.flagged[14] || st.flagged[12] {
		t.Fatalf("fail_flag stamping wrong: %v", st.flagged)
	}
	var fx, csdr, failedLeg *SettlementFail
	for i := range st.fails {
		f := st.fails[i]
		switch f.InstructionID {
		case 10:
			fx = f
		case 11:
			csdr = f
		case 14:
			failedLeg = f
		}
	}
	if fx.Regime != RegimeFXCloseout {
		t.Fatalf("FX leg regime = %s, want FX_CLOSEOUT", fx.Regime)
	}
	if csdr.Regime != RegimeCSDR || csdr.Liquidity != LiquidityIlliquid {
		t.Fatalf("securities leg regime = %+v", csdr)
	}
	if failedLeg == nil || failedLeg.Regime != RegimeFXCloseout {
		t.Fatalf("already-FAILED leg not detected/classified: %+v", failedLeg)
	}
	// Rescan is idempotent — flagged legs are out of the late set and the
	// unique instruction key dedupes any race.
	fails2, err := svc.DetectFails(context.Background(), day)
	if err != nil {
		t.Fatalf("rescan: %v", err)
	}
	if len(fails2) != 0 || len(st.fails) != 3 {
		t.Fatalf("rescan duplicated fails: %d/%d", len(fails2), len(st.fails))
	}
}

func TestCSDR_NilStoreFailsClosed(t *testing.T) {
	svc := NewCSDRService(nil, nil)
	ctx := context.Background()
	if _, err := svc.DetectFails(ctx, time.Now()); errCode(err) != CodeServiceDegraded {
		t.Fatalf("detect: want SERVICE_DEGRADED got %v", err)
	}
	if _, err := svc.AccruePenalties(ctx, time.Now()); errCode(err) != CodeServiceDegraded {
		t.Fatalf("accrue: want SERVICE_DEGRADED got %v", err)
	}
	if _, err := svc.DailyReport(ctx, time.Now()); errCode(err) != CodeServiceDegraded {
		t.Fatalf("daily: want SERVICE_DEGRADED got %v", err)
	}
	if err := svc.ResolveFail(ctx, 1); errCode(err) != CodeServiceDegraded {
		t.Fatalf("resolve: want SERVICE_DEGRADED got %v", err)
	}
}

// ---------------------------------------------------------------------------
// Penalty accrual — CSDR scope only, bilateral, CLS exempt
// ---------------------------------------------------------------------------

func seedFail(st *soFailStore, id int64, regime FailRegime, liq LiquidityClass,
	cls bool, ccy, amt string) {
	st.fails[id] = &SettlementFail{ID: id, InstructionID: 100 + id, TradeID: 55,
		AccountID: 7, Currency: ccy, Amount: decimal.RequireFromString(amt),
		ISD: time.Now().Add(-48 * time.Hour), DetectedAt: time.Now().Add(-24 * time.Hour),
		Regime: regime, Liquidity: liq, CLSSettled: cls, Status: FailOpen}
	st.byInstr[100+id] = id
	st.legs[100+id] = soLateLeg(100+id, 55, 7, ccy, amt, time.Now().Add(-48*time.Hour))
}

func TestCSDR_PenaltiesBilateralCSDRScopeOnly(t *testing.T) {
	st := newSoFailStore()
	day := time.Now().UTC().Truncate(24 * time.Hour)
	seedFail(st, 1, RegimeCSDR, LiquidityLiquid, false, "EUR", "1000000")
	seedFail(st, 2, RegimeCSDR, LiquidityIlliquid, false, "EUR", "2000000")
	seedFail(st, 3, RegimeFXCloseout, LiquidityLiquid, false, "USD", "500000") // FX: exempt
	seedFail(st, 4, RegimeCSDR, LiquidityLiquid, true, "EUR", "500000")        // CLS: exempt

	svc := NewCSDRService(st, nil)
	n, err := svc.AccruePenalties(context.Background(), day)
	if err != nil {
		t.Fatalf("accrue: %v", err)
	}
	if n != 2 {
		t.Fatalf("want 2 accrued fails, got %d", n)
	}
	// Two fails × two directions = 4 rows.
	if len(st.penalties) != 4 {
		t.Fatalf("want 4 penalty rows, got %d", len(st.penalties))
	}
	var liquidPen, illiqPen decimal.Decimal
	dirs := map[string]bool{}
	perFail := map[int64]int{}
	for _, p := range st.penalties {
		dirs[p.Direction] = true
		perFail[p.FailID]++
		if p.FailID == 1 && p.Direction == "PAYABLE" {
			liquidPen = p.PenaltyAmount
			if !p.RateBP.Equal(decimal.RequireFromString("1")) {
				t.Fatalf("liquid rate %s", p.RateBP)
			}
		}
		if p.FailID == 2 && p.Direction == "PAYABLE" {
			illiqPen = p.PenaltyAmount
		}
	}
	// 1bp on €1,000,000 = 100.00 ; 0.5bp on €2,000,000 = 100.00.
	if liquidPen.String() != "100" {
		t.Fatalf("liquid penalty want 100 got %s", liquidPen)
	}
	if illiqPen.String() != "100" {
		t.Fatalf("illiquid penalty want 100 got %s", illiqPen)
	}
	if !dirs["PAYABLE"] || !dirs["RECEIVABLE"] {
		t.Fatalf("bilateral legs missing: %v", dirs)
	}
	if perFail[1] != 2 || perFail[2] != 2 {
		t.Fatalf("each fail needs PAYABLE+RECEIVABLE rows: %v", perFail)
	}
	// Replay-safe: second accrual same day inserts nothing new.
	if _, err := svc.AccruePenalties(context.Background(), day); err != nil {
		t.Fatalf("re-accrue: %v", err)
	}
	if len(st.penalties) != 4 {
		t.Fatalf("accrual replay duplicated: %d", len(st.penalties))
	}
}

func TestCSDR_SettledLegAutoResolves(t *testing.T) {
	st := newSoFailStore()
	seedFail(st, 1, RegimeCSDR, LiquidityLiquid, false, "EUR", "1000000")
	st.legs[101].Status = "SETTLED" // arrived since the scan
	svc := NewCSDRService(st, nil)
	n, err := svc.AccruePenalties(context.Background(), time.Now())
	if err != nil {
		t.Fatalf("accrue: %v", err)
	}
	if n != 0 {
		t.Fatalf("settled fail accrued a penalty")
	}
	if st.fails[1].Status != FailResolved || st.fails[1].ResolvedAt == nil {
		t.Fatalf("fail not auto-resolved: %+v", st.fails[1])
	}
}

func TestCSDR_Reports(t *testing.T) {
	st := newSoFailStore()
	today := time.Now().UTC().Truncate(24 * time.Hour)
	svc := NewCSDRService(st, nil)
	svc.SetClockForTest(func() time.Time { return today.Add(12 * time.Hour) })

	seedFail(st, 1, RegimeCSDR, LiquidityLiquid, false, "EUR", "1000000")
	seedFail(st, 2, RegimeCSDR, LiquidityLiquid, false, "EUR", "500000")
	st.fails[2].Status = FailResolved
	st.fails[1].DetectedAt = today.Add(-48 * time.Hour) // detected before today
	st.fails[2].DetectedAt = today.Add(1 * time.Hour)   // detected today

	if _, err := svc.AccruePenalties(context.Background(), today); err != nil {
		t.Fatalf("accrue: %v", err)
	}
	rep, err := svc.DailyReport(context.Background(), today)
	if err != nil {
		t.Fatalf("daily report: %v", err)
	}
	if rep.FailsDetected != 1 || rep.FailsOpen != 1 || rep.FailsResolved != 1 {
		t.Fatalf("daily counts %+v", rep)
	}
	// Only fail 1 was open at accrual: 1bp × €1,000,000 = €100 payable
	// (PenaltyRows counts both bilateral legs → 2).
	if rep.PenaltyRows != 2 || rep.PenaltyByCcy["EUR"] != "100" {
		t.Fatalf("daily penalties %+v", rep)
	}
	mrep, err := svc.MonthlyReport(context.Background(), today.Year(), today.Month())
	if err != nil {
		t.Fatalf("monthly: %v", err)
	}
	if mrep.FailsDetected < rep.FailsDetected {
		t.Fatalf("monthly report not an aggregate of daily")
	}
}

// ---------------------------------------------------------------------------
// Buy-in lifecycle — CSDR scope only
// ---------------------------------------------------------------------------

func TestBuyIn_NotifyAtISD4ExecuteAtISD7(t *testing.T) {
	st := newSoFailStore()
	isd := time.Now().UTC().Truncate(24*time.Hour).AddDate(0, 0, -8) // ISD 8 days ago
	st.fails[1] = &SettlementFail{ID: 1, InstructionID: 10, TradeID: 55,
		AccountID: 7, Currency: "EUR", Amount: decimal.RequireFromString("1000000"),
		ISD: isd, DetectedAt: isd.Add(24 * time.Hour),
		Regime: RegimeCSDR, Liquidity: LiquidityLiquid, Status: FailOpen}
	st.byInstr[10] = 1
	// FX_CLOSEOUT fail at the same age — must never reach buy-in.
	st.fails[2] = &SettlementFail{ID: 2, InstructionID: 11, TradeID: 56,
		AccountID: 7, Currency: "USD", Amount: decimal.RequireFromString("500000"),
		ISD: isd, DetectedAt: isd.Add(24 * time.Hour),
		Regime: RegimeFXCloseout, Liquidity: LiquidityLiquid, Status: FailOpen}
	st.byInstr[11] = 2
	st.tradePrice[55] = decimal.RequireFromString("1.1000")

	var alerts []string
	svc := NewBuyInService(soBuyInStore{st}, soPricer{
		marks: map[int64]decimal.Decimal{55: decimal.RequireFromString("1.1200")}},
		func(_ context.Context, code, summary string, _ map[string]any) {
			alerts = append(alerts, code)
		})

	// ISD+4 notify.
	n, err := svc.NotifyDue(context.Background(), isd.AddDate(0, 0, BuyInNotifyDays))
	if err != nil {
		t.Fatalf("notify: %v", err)
	}
	if n != 1 {
		t.Fatalf("want 1 notification, got %d", n)
	}
	if st.fails[1].Status != FailBuyInNotified || st.fails[1].BuyInNotifiedAt == nil {
		t.Fatalf("fail not BUYIN_NOTIFIED: %+v", st.fails[1])
	}
	if st.fails[2].Status != FailOpen {
		t.Fatalf("FX fail touched by buy-in machinery")
	}
	// Notify replay is idempotent.
	if n, err := svc.NotifyDue(context.Background(), isd.AddDate(0, 0, BuyInNotifyDays)); err != nil || n != 0 {
		t.Fatalf("notify replay n=%d err=%v", n, err)
	}

	// ISD+7 execute: |1.1200−1.1000| × €1,000,000 = €20,000 differential.
	n, err = svc.ExecuteDue(context.Background(), isd.AddDate(0, 0, BuyInExecuteDays))
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if n != 1 {
		t.Fatalf("want 1 execution, got %d", n)
	}
	var ev *BuyInEvent
	for i := range st.buyIns {
		if st.buyIns[i].Kind == "EXECUTION" {
			ev = &st.buyIns[i]
		}
	}
	if ev == nil || ev.PriceDifferential.String() != "20000" {
		t.Fatalf("bad execution event %+v", ev)
	}
	if st.fails[1].Status != FailBoughtIn {
		t.Fatalf("fail not BOUGHT_IN")
	}
	if len(st.journals) != 1 {
		t.Fatalf("want 1 differential journal, got %d", len(st.journals))
	}
	var dr, cr decimal.Decimal
	for _, l := range st.journals[0].lines {
		dr = dr.Add(l.Debit)
		cr = cr.Add(l.Credit)
	}
	if !dr.Equal(cr) || dr.String() != "20000" {
		t.Fatalf("unbalanced/wrong journal dr=%s cr=%s", dr, cr)
	}
	// Execute replay is idempotent.
	if n, err := svc.ExecuteDue(context.Background(), isd.AddDate(0, 0, BuyInExecuteDays)); err != nil || n != 0 {
		t.Fatalf("execute replay n=%d err=%v", n, err)
	}
	var buyInAlert bool
	for _, a := range alerts {
		if a == CodeBuyInTriggered {
			buyInAlert = true
		}
	}
	if !buyInAlert {
		t.Fatalf("no BUY_IN_TRIGGERED alert: %v", alerts)
	}
}

func TestBuyIn_NilDepsFailClosed(t *testing.T) {
	svc := NewBuyInService(nil, nil, nil)
	ctx := context.Background()
	if _, err := svc.NotifyDue(ctx, time.Now()); errCode(err) != CodeServiceDegraded {
		t.Fatalf("want SERVICE_DEGRADED got %v", err)
	}
	svc2 := NewBuyInService(soBuyInStore{newSoFailStore()}, nil, nil)
	if _, err := svc2.ExecuteDue(ctx, time.Now()); errCode(err) != CodeServiceDegraded {
		t.Fatalf("nil pricer: want SERVICE_DEGRADED got %v", err)
	}
}
