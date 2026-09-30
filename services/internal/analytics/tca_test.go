package analytics

import (
	"context"
	"io"
	"testing"
	"time"

	"exchange/internal/objectstore"
	"exchange/pkg/decimal"
)

func dec(s string) decimal.Decimal {
	d, err := decimal.NewFromString(s)
	if err != nil {
		panic(err)
	}
	return d
}

func fill() TCAFill {
	return TCAFill{
		FillID: 100, OrderID: 42, AccountID: 7, InstrumentID: 3,
		Symbol: "EUR/USD", Side: "BUY",
		ExecPrice: dec("1.0852"), Qty: dec("100000"),
		ReceivedAt: time.Date(2026, 9, 22, 10, 0, 0, 0, time.UTC),
		ExecutedAt: time.Date(2026, 9, 22, 10, 15, 0, 0, time.UTC),
	}
}

func newEngine(t *testing.T) (*TCAEngine, *MemTCASink, *MemArrivalSource, *MemSessionVWAP, *MemFixSource) {
	t.Helper()
	sink := &MemTCASink{}
	ar := NewMemArrivalSource()
	vw := NewMemSessionVWAP()
	fx := NewMemFixSource()
	e, err := NewTCAEngine(sink, ar, vw, fx)
	if err != nil {
		t.Fatal(err)
	}
	return e, sink, ar, vw, fx
}

func TestTCA_ArrivalSlippageBuy(t *testing.T) {
	e, sink, ar, _, _ := newEngine(t)
	ar.Set("EUR/USD", dec("1.0850")) // exec 1.0852 → 1.8435... bps worse
	rec, err := e.OnFill(context.Background(), fill())
	if err != nil {
		t.Fatal(err)
	}
	if rec.ArrivalPrice == nil || rec.SlipArrivalBps == nil {
		t.Fatal("arrival benchmark missing")
	}
	want := dec("1.0852").Sub(dec("1.0850")).Div(dec("1.0850")).Mul(decimal.NewFromInt(10000))
	if !rec.SlipArrivalBps.Equal(want) {
		t.Fatalf("slip %s want %s", rec.SlipArrivalBps, want)
	}
	if !rec.SlipArrivalBps.IsPositive() {
		t.Fatal("BUY above arrival must be positive slip")
	}
	if len(sink.Rows) != 1 {
		t.Fatal("row not persisted")
	}
}

func TestTCA_ArrivalSlippageSellSign(t *testing.T) {
	e, _, ar, _, _ := newEngine(t)
	ar.Set("EUR/USD", dec("1.0854"))
	f := fill()
	f.Side = "SELL" // exec 1.0852 below arrival 1.0854 → positive slip
	rec, err := e.OnFill(context.Background(), f)
	if err != nil {
		t.Fatal(err)
	}
	if rec.SlipArrivalBps == nil || !rec.SlipArrivalBps.IsPositive() {
		t.Fatalf("sell slip sign wrong: %v", rec.SlipArrivalBps)
	}
	want := dec("1.0854").Sub(dec("1.0852")).Div(dec("1.0854")).Mul(decimal.NewFromInt(10000))
	if !rec.SlipArrivalBps.Equal(want) {
		t.Fatalf("slip %s want %s", rec.SlipArrivalBps, want)
	}
}

func TestTCA_VWAPSlippage(t *testing.T) {
	e, _, _, vw, _ := newEngine(t)
	vw.Set("EUR/USD", dec("1.0840"))
	rec, err := e.OnFill(context.Background(), fill())
	if err != nil {
		t.Fatal(err)
	}
	if rec.SessionVWAP == nil || rec.SlipVWAPBps == nil {
		t.Fatal("vwap benchmark missing")
	}
	want := dec("1.0852").Sub(dec("1.0840")).Div(dec("1.0840")).Mul(decimal.NewFromInt(10000))
	if !rec.SlipVWAPBps.Equal(want) {
		t.Fatalf("slip %s want %s", rec.SlipVWAPBps, want)
	}
}

func TestTCA_ECBFixSlippage(t *testing.T) {
	e, _, _, _, fx := newEngine(t)
	fx.Set("EUR/USD", dec("1.0860"))
	rec, err := e.OnFill(context.Background(), fill())
	if err != nil {
		t.Fatal(err)
	}
	if rec.ECBFix == nil || rec.SlipFixBps == nil || rec.FixAbsent {
		t.Fatalf("fix fields wrong: %+v", rec)
	}
	want := dec("1.0852").Sub(dec("1.0860")).Div(dec("1.0860")).Mul(decimal.NewFromInt(10000))
	if !rec.SlipFixBps.Equal(want) {
		t.Fatalf("fix slip %s want %s", rec.SlipFixBps, want)
	}
	if !rec.SlipFixBps.IsNegative() {
		t.Fatal("BUY below the fix = negative (better) slip")
	}
}

func TestTCA_MissingFixRecordedNotFabricated(t *testing.T) {
	e, sink, _, _, _ := newEngine(t) // no fix published
	rec, err := e.OnFill(context.Background(), fill())
	if err != nil {
		t.Fatal(err)
	}
	if !rec.FixAbsent {
		t.Fatal("fix_absent must be set")
	}
	if rec.ECBFix != nil || rec.SlipFixBps != nil {
		t.Fatal("absent fix must serialize as NULL, never a fabricated value")
	}
	got := sink.Rows[0]
	if got.ECBFix != nil {
		t.Fatal("persisted a fabricated fix")
	}
}

func TestTCA_PriceImprovementFromPreventionEvent(t *testing.T) {
	e, _, _, _, _ := newEngine(t)
	e.OnPriceImprovement(PriceImprovementEvent{
		OrderID: 42, Symbol: "EUR/USD",
		LimitPrice: dec("1.0855"), ExecPrice: dec("1.0852"),
		Ts: time.Now(),
	})
	rec, err := e.OnFill(context.Background(), fill())
	if err != nil {
		t.Fatal(err)
	}
	if rec.PriceImprovement == nil || !rec.PriceImprovement.Equal(dec("0.0003")) {
		t.Fatalf("improvement %v want 0.0003 (limit−exec)", rec.PriceImprovement)
	}
}

func TestTCA_PriceImprovementFromLimitFallback(t *testing.T) {
	e, _, _, _, _ := newEngine(t)
	f := fill()
	limit := dec("1.0850")
	f.LimitPrice = &limit // exec 1.0852 > limit → delta negative = adverse selection
	rec, err := e.OnFill(context.Background(), f)
	if err != nil {
		t.Fatal(err)
	}
	if rec.PriceImprovement == nil {
		t.Fatal("delta missing")
	}
	want := dec("1.0850").Sub(dec("1.0852"))
	if !rec.PriceImprovement.Equal(want) || !rec.PriceImprovement.IsNegative() {
		t.Fatalf("adverse selection not recorded verbatim: %v", rec.PriceImprovement)
	}
}

func TestTCA_ImprovementZeroDeltaAndTTL(t *testing.T) {
	e, _, _, _, _ := newEngine(t)
	e.OnPriceImprovement(PriceImprovementEvent{
		OrderID: 42, LimitPrice: dec("1.0852"), ExecPrice: dec("1.0852"),
	})
	rec, err := e.OnFill(context.Background(), fill())
	if err != nil {
		t.Fatal(err)
	}
	if rec.PriceImprovement == nil || !rec.PriceImprovement.IsZero() {
		t.Fatalf("zero delta must be recorded faithfully: %v", rec.PriceImprovement)
	}
	// Consumed — a second fill on the same order sees no delta.
	rec2, err := e.OnFill(context.Background(), fill())
	if err != nil {
		t.Fatal(err)
	}
	if rec2.PriceImprovement != nil {
		t.Fatal("delta must be consumed once")
	}
}

func TestTCA_AbsentSourcesYieldNullRow(t *testing.T) {
	sink := &MemTCASink{}
	e, err := NewTCAEngine(sink, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	rec, err := e.OnFill(context.Background(), fill())
	if err != nil {
		t.Fatal(err)
	}
	if rec.ArrivalPrice != nil || rec.SessionVWAP != nil || rec.ECBFix != nil {
		t.Fatal("absent sources must leave NULLs")
	}
	if len(sink.Rows) != 1 || sink.Rows[0].Period != "fill" {
		t.Fatal("fill row must persist even with no benchmarks")
	}
}

func TestTCA_SinkFailurePropagates(t *testing.T) {
	e, sink, _, _, _ := newEngine(t)
	sink.Err = context.DeadlineExceeded
	if _, err := e.OnFill(context.Background(), fill()); err == nil {
		t.Fatal("sink failure must propagate for NAK")
	}
}

// ---------------------------------------------------------------------------
// Aggregation
// ---------------------------------------------------------------------------

func TestMemReportStore_PeriodAggregation(t *testing.T) {
	s1 := dec("10")
	s2 := dec("20")
	store := &MemReportStore{Fills: []TCARecord{
		{AccountID: 7, InstrumentID: 3, Symbol: "EUR/USD", Period: "fill",
			Ts: time.Date(2026, 9, 22, 10, 0, 0, 0, time.UTC), SlipArrivalBps: &s1},
		{AccountID: 7, InstrumentID: 3, Symbol: "EUR/USD", Period: "fill",
			Ts: time.Date(2026, 9, 23, 10, 0, 0, 0, time.UTC), SlipArrivalBps: &s2},
		{AccountID: 8, InstrumentID: 3, Symbol: "EUR/USD", Period: "fill",
			Ts: time.Date(2026, 9, 22, 11, 0, 0, 0, time.UTC), SlipArrivalBps: &s1},
	}}
	// daily → two buckets for account 7
	rows, err := store.Aggregate(context.Background(), ReportFilter{AccountID: 7, Period: "daily"})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Fatalf("daily buckets=%d want 2", len(rows))
	}
	// monthly → one bucket averaging (10+20)/2 = 15
	rows, err = store.Aggregate(context.Background(), ReportFilter{AccountID: 7, Period: "monthly"})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Fills != 2 {
		t.Fatalf("monthly rows=%v", rows)
	}
	if rows[0].AvgSlipArrivalBps == nil || !rows[0].AvgSlipArrivalBps.Equal(dec("15")) {
		t.Fatalf("avg %v want 15", rows[0].AvgSlipArrivalBps)
	}
	// quarterly → single bucket
	rows, err = store.Aggregate(context.Background(), ReportFilter{AccountID: 7, Period: "quarterly"})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("quarterly rows=%d", len(rows))
	}
	// instrument filter
	rows, err = store.Aggregate(context.Background(), ReportFilter{
		AccountID: 7, Period: "monthly", InstrumentIDs: []int64{999}})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 0 {
		t.Fatal("instrument filter must exclude non-matching")
	}
}

func TestClassifyPair(t *testing.T) {
	for _, tc := range []struct{ b, q, want string }{
		{"EUR", "USD", ClassFXMajor},
		{"USD", "JPY", ClassFXMajor},
		{"EUR", "GBP", ClassFXMinor},
		{"USD", "TRY", ClassFXExotic},
		{"TRY", "ZAR", ClassFXExotic},
	} {
		if got := ClassifyPair(tc.b, tc.q); got != tc.want {
			t.Errorf("%s/%s = %s want %s", tc.b, tc.q, got, tc.want)
		}
	}
}

func TestSessionWindow_Boundary(t *testing.T) {
	// Fill at Wednesday 10:00 UTC → session opened Tuesday 22:00.
	start, end := SessionWindow(time.Date(2026, 9, 23, 10, 0, 0, 0, time.UTC))
	if start != time.Date(2026, 9, 22, 22, 0, 0, 0, time.UTC) {
		t.Fatalf("session start %s", start)
	}
	if end != time.Date(2026, 9, 23, 10, 0, 0, 0, time.UTC) {
		t.Fatalf("session end %s", end)
	}
	// Fill at 23:30 UTC → session opened same day at 22:00.
	start, _ = SessionWindow(time.Date(2026, 9, 23, 23, 30, 0, 0, time.UTC))
	if start != time.Date(2026, 9, 23, 22, 0, 0, 0, time.UTC) {
		t.Fatalf("post-22:00 session start %s", start)
	}
}

// ---------------------------------------------------------------------------
// RTS 28 job
// ---------------------------------------------------------------------------

// fakeObjStore implements objectstore.Client for the RTS28 test.
type fakeObjStore struct {
	m   map[string][]byte
	put []objectstore.PutInput
}

func (o *fakeObjStore) Put(_ context.Context, in objectstore.PutInput) (objectstore.Object, error) {
	b, err := io.ReadAll(in.Body)
	if err != nil {
		return objectstore.Object{}, err
	}
	o.m[in.Key] = b
	o.put = append(o.put, in)
	return objectstore.Object{Key: in.Key, Size: int64(len(b))}, nil
}

func (o *fakeObjStore) Get(_ context.Context, key string) ([]byte, objectstore.Object, error) {
	b, ok := o.m[key]
	if !ok {
		return nil, objectstore.Object{}, &objectstore.NotFoundError{Key: key}
	}
	return b, objectstore.Object{Key: key}, nil
}

func (o *fakeObjStore) Head(_ context.Context, key string) (objectstore.Object, error) {
	if _, ok := o.m[key]; !ok {
		return objectstore.Object{}, &objectstore.NotFoundError{Key: key}
	}
	return objectstore.Object{Key: key}, nil
}

func (o *fakeObjStore) List(context.Context, objectstore.ListInput) (objectstore.ListOutput, error) {
	return objectstore.ListOutput{}, nil
}

func (o *fakeObjStore) Delete(_ context.Context, key string) error {
	delete(o.m, key)
	return nil
}

func (o *fakeObjStore) Bucket() string { return "test" }

func TestRTS28Job_PersistsRollupAndArchives(t *testing.T) {
	s1 := dec("10")
	store := &MemReportStore{Fills: []TCARecord{
		{AccountID: 7, InstrumentID: 3, Symbol: "EUR/USD", Period: "fill",
			Ts: time.Date(2026, 8, 10, 10, 0, 0, 0, time.UTC), SlipArrivalBps: &s1},
		{AccountID: 7, InstrumentID: 3, Symbol: "EUR/USD", Period: "fill",
			Ts: time.Date(2026, 8, 20, 10, 0, 0, 0, time.UTC), SlipArrivalBps: &s1},
		// out-of-quarter fill must be excluded
		{AccountID: 7, InstrumentID: 3, Symbol: "EUR/USD", Period: "fill",
			Ts: time.Date(2026, 10, 2, 10, 0, 0, 0, time.UTC), SlipArrivalBps: &s1},
	}}
	sink := &MemTCASink{}
	objs := &fakeObjStore{m: map[string][]byte{}}
	job, err := NewRTS28SummaryJob(store, sink, objs)
	if err != nil {
		t.Fatal(err)
	}
	n, err := job.RunQuarter(context.Background(), time.Date(2026, 8, 15, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 || len(sink.Rows) != 1 {
		t.Fatalf("rollup rows=%d sink=%d", n, len(sink.Rows))
	}
	rec := sink.Rows[0]
	if rec.Period != "quarterly" || rec.AccountID != 7 {
		t.Fatalf("rollup record wrong: %+v", rec)
	}
	if rec.PeriodStart != time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC) {
		t.Fatalf("quarter start %s", rec.PeriodStart)
	}
	if len(objs.m) != 1 {
		t.Fatalf("archived docs=%d", len(objs.m))
	}
	for key, body := range objs.m {
		if len(body) < 100 || string(body[:5]) != "%PDF-" {
			t.Fatalf("archived %s is not a pdf", key)
		}
		if key != "rts28/2026Q3/7.pdf" {
			t.Fatalf("archive key %s", key)
		}
	}
	if len(objs.put) != 1 {
		t.Fatal("missing PutInput capture")
	}
	in := objs.put[0]
	if in.Metadata["retention-class"] != "7y" {
		t.Fatalf("retention metadata missing: %v", in.Metadata)
	}
	if in.ObjectLockMode != "COMPLIANCE" || in.ObjectLockRetainUntil.Before(now7y()) {
		t.Fatal("7-year object lock not applied")
	}
}

func now7y() time.Time { return time.Now().UTC().AddDate(6, 11, 30) }

func TestRenderRTS28PDF_VenueSection(t *testing.T) {
	s := dec("12.5")
	pdf := RenderRTS28PDF(7, time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC), []AggregateRow{
		{AccountID: 7, Symbol: "EUR/USD", InstrumentID: 3, Fills: 40,
			AvgSlipArrivalBps: &s},
	})
	if string(pdf[:5]) != "%PDF-" {
		t.Fatal("not a pdf")
	}
}
