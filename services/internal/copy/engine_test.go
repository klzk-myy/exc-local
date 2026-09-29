package copy

import (
	"context"
	"testing"
	"time"

	"exchange/pkg/decimal"
)

type fakeSubmitter struct {
	calls []ChildOrderRequest
	fail  bool
}

func (f *fakeSubmitter) SubmitChild(_ context.Context, _ int64, req ChildOrderRequest) (int64, error) {
	if f.fail {
		return 0, errorf(CodeInternalError, "submit failed")
	}
	f.calls = append(f.calls, req)
	return int64(1000 + len(f.calls)), nil
}

type fakeNotifier struct{ notices []string }

func (n *fakeNotifier) NotifyChildSkipped(_ context.Context, _ int64, c ChildOrder, detail string) error {
	n.notices = append(n.notices, detail)
	return nil
}

func engineEnv(t *testing.T, mode SafetyMode) (*Engine, *fakeStore, *fakeSubmitter, *fakeNotifier, *Follow) {
	fs := newFakeStore()
	svc, _, _, _ := newTestService(fs)
	st := listedStrategy(t, svc, fs, "0")
	f, err := svc.Follow(context.Background(), FollowInput{
		InvestorAccountID: 42, StrategyID: st.StrategyID,
		AllocationNotional: "10000", SafetyMode: string(mode)})
	if err != nil {
		t.Fatal(err)
	}
	sub := &fakeSubmitter{}
	notif := &fakeNotifier{}
	eng, err := NewEngine(fs, WithChildSubmitter(sub), WithSkipNotifier(notif))
	if err != nil {
		t.Fatal(err)
	}
	return eng, fs, sub, notif, f
}

// AC: FULL mode — pro-rata child quantity equals the master qty (single
// follower), submitted through the order seam.
func TestEngine_FullMode(t *testing.T) {
	eng, fs, sub, _, f := engineEnv(t, ModeFull)
	res, err := eng.OnManagerFill(context.Background(), EngineFill{
		StrategyID: f.StrategyID, ManagerAcctID: 10, TradeID: 7,
		InstrumentID: 1, Side: "BUY", Quantity: "5000", Price: "1.10"})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Children) != 1 || len(res.Skipped) != 0 {
		t.Fatalf("children=%d skipped=%d", len(res.Children), len(res.Skipped))
	}
	c := res.Children[0]
	if !c.Quantity.Equal(d("5000")) {
		t.Fatalf("child qty %s", c.Quantity)
	}
	if c.Status != ChildSubmitted || c.ChildOrderID == nil {
		t.Fatalf("child not submitted: %+v", c)
	}
	if len(sub.calls) != 1 || sub.calls[0].ClientOrderID != "copy:7:"+itoa(f.FollowID) {
		t.Fatalf("submit calls %+v", sub.calls)
	}
	_ = fs
}

// AC: HALF_RISK scales the child quantity ×0.5 BEFORE the min-notional
// check.
func TestEngine_HalfRiskScaling(t *testing.T) {
	eng, _, sub, _, f := engineEnv(t, ModeHalfRisk)
	res, err := eng.OnManagerFill(context.Background(), EngineFill{
		StrategyID: f.StrategyID, ManagerAcctID: 10, TradeID: 9,
		InstrumentID: 1, Side: "SELL", Quantity: "4000", Price: "1.10"})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Children) != 1 {
		t.Fatalf("children=%d", len(res.Children))
	}
	if !res.Children[0].Quantity.Equal(d("2000")) {
		t.Fatalf("half-risk child qty %s want 2000", res.Children[0].Quantity)
	}
	if len(sub.calls) != 1 || !sub.calls[0].Quantity.Equal(d("2000")) {
		t.Fatalf("submitted qty %+v", sub.calls)
	}
}

// AC: scaled quantity below instrument min → SKIPPED_MIN_NOTIONAL with a
// durable notice + notifier invocation — never silently dropped, never
// submitted.
func TestEngine_MinNotionalSkipNotice(t *testing.T) {
	eng, _, sub, notif, f := engineEnv(t, ModeHalfRisk)
	// min_qty 1000; master qty 1000 → half-risk child 500 < 1000.
	res, err := eng.OnManagerFill(context.Background(), EngineFill{
		StrategyID: f.StrategyID, ManagerAcctID: 10, TradeID: 11,
		InstrumentID: 1, Side: "BUY", Quantity: "1000", Price: "1.10"})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Skipped) != 1 || len(res.Children) != 0 {
		t.Fatalf("children=%d skipped=%d", len(res.Children), len(res.Skipped))
	}
	sk := res.Skipped[0]
	if sk.Status != ChildSkippedMinNotional || sk.Notice == "" {
		t.Fatalf("skip row malformed: %+v", sk)
	}
	if len(notif.notices) != 1 {
		t.Fatalf("no notice emitted")
	}
	if len(sub.calls) != 0 {
		t.Fatal("sub-min child was submitted")
	}
}

// AC: replay — the same master trade fans out once; a redelivery is a
// Duplicate, children neither re-inserted nor re-submitted.
func TestEngine_ReplayDedup(t *testing.T) {
	eng, fs, sub, _, f := engineEnv(t, ModeFull)
	mk := func() EngineFill {
		return EngineFill{StrategyID: f.StrategyID, ManagerAcctID: 10,
			TradeID: 33, InstrumentID: 1, Side: "BUY",
			Quantity: "2000", Price: "1.10"}
	}
	if _, err := eng.OnManagerFill(context.Background(), mk()); err != nil {
		t.Fatal(err)
	}
	res, err := eng.OnManagerFill(context.Background(), mk())
	if err != nil {
		t.Fatal(err)
	}
	if !res.Duplicate {
		t.Fatal("replay not deduped")
	}
	if len(sub.calls) != 1 {
		t.Fatalf("child double-submitted (%d calls)", len(sub.calls))
	}
	if len(fs.children) != 1 {
		t.Fatalf("child double-persisted (%d)", len(fs.children))
	}
}

// Multi-follower pro-rata: two follows split the master fill by notional.
func TestEngine_ProRataAcrossFollows(t *testing.T) {
	eng, fs, sub, _, f := engineEnv(t, ModeFull)
	svc, _, _, _ := newTestService(fs)
	if _, err := svc.Follow(context.Background(), FollowInput{
		InvestorAccountID: 77, StrategyID: f.StrategyID,
		AllocationNotional: "30000"}); err != nil {
		t.Fatal(err)
	}
	res, err := eng.OnManagerFill(context.Background(), EngineFill{
		StrategyID: f.StrategyID, ManagerAcctID: 10, TradeID: 41,
		InstrumentID: 1, Side: "BUY", Quantity: "8000", Price: "1.10"})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Children) != 2 {
		t.Fatalf("children=%d", len(res.Children))
	}
	// 10k : 30k → 2000 : 6000.
	byFollow := map[int64]decimal.Decimal{}
	for _, c := range res.Children {
		byFollow[c.FollowID] = c.Quantity
	}
	if !byFollow[f.FollowID].Equal(d("2000")) {
		t.Fatalf("f1 qty %s", byFollow[f.FollowID])
	}
	var other decimal.Decimal
	for fid, q := range byFollow {
		if fid != f.FollowID {
			other = q
		}
	}
	if !other.Equal(d("6000")) {
		t.Fatalf("f2 qty %s", other)
	}
	if len(sub.calls) != 2 {
		t.Fatalf("submits=%d", len(sub.calls))
	}
	_ = time.Now
}

func itoa(v int64) string {
	return decimal.NewFromInt(v).String()
}
