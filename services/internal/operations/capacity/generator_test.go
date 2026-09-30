// Quarterly capacity report generator tests — Phase-09 Task 9.3.19:
// storage runway math (avail/growth), utilization threshold projection,
// the 90/60/180-day action ladder, no-data fail-visibility, quarter
// labeling, and the sink seam.
package capacity

import (
	"context"
	"encoding/json"
	"math"
	"testing"
	"time"
)

// fakeSource returns scripted series/instant values keyed by expr.
type fakeSource struct {
	series   map[string][]Point
	instant  map[string]float64
	rangeErr map[string]error
}

func (f *fakeSource) Range(ctx context.Context, expr string,
	start, end time.Time, step time.Duration) ([]Point, error) {
	if err := f.rangeErr[expr]; err != nil {
		return nil, err
	}
	return f.series[expr], nil
}

func (f *fakeSource) Instant(ctx context.Context, expr string, at time.Time) (float64, error) {
	if v, ok := f.instant[expr]; ok {
		return v, nil
	}
	return 0, errNotFound
}

type errString string

func (e errString) Error() string { return string(e) }

const errNotFound = errString("no data")

var base = time.Date(2026, 9, 30, 0, 0, 0, 0, time.UTC) // Q3 last day

const gb = 1024 * 1024 * 1024

// ramp produces a linearly growing series: startVal at window start,
// growing growthPerDay units/day, sampled daily.
func ramp(start time.Time, days int, startVal, growthPerDay float64) []Point {
	out := make([]Point, 0, days)
	for d := 0; d < days; d++ {
		out = append(out, Point{
			At:    start.AddDate(0, 0, d),
			Value: startVal + growthPerDay*float64(d),
		})
	}
	return out
}

func newGen(t *testing.T, src MetricsSource, specs []ResourceSpec, sink Sink) *Generator {
	t.Helper()
	g, err := New(src, DefaultThresholds(), specs, sink)
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	g.SetClock(func() time.Time { return base })
	return g
}

type captureSink struct{ got *Report }

func (c *captureSink) Store(ctx context.Context, r *Report) error {
	c.got = r
	return nil
}

// Storage: 800 GB avail, measured 10 GB/day growth → 80d runway →
// PROVISION (inside 90d lead, outside 60d hardware trigger).
func TestGenerate_StorageRunwayProvision(t *testing.T) {
	th := DefaultThresholds()
	start, end := th.Window(base)
	spec := ResourceSpec{
		Name: "pg", Kind: KindStorage,
		SeriesExpr:   "pg_used",
		AvailExpr:    "pg_avail",
		CapacityExpr: "pg_cap",
		ScaleAction:  "extend volume",
	}
	src := &fakeSource{
		series: map[string][]Point{
			"pg_used": ramp(start, 90, 100*gb, 10*gb), // 100→990 GB? no — see below
		},
		instant: map[string]float64{
			"pg_avail": 800 * gb,
			"pg_cap":   2000 * gb,
		},
	}
	// used grows 10 GB/day from 200 GB → ~1090 GB used; avail 800 GB.
	src.series["pg_used"] = ramp(start, 90, 200*gb, 10*gb)

	sink := &captureSink{}
	g := newGen(t, src, []ResourceSpec{spec}, sink)
	rep, err := g.Generate(context.Background())
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	rr := rep.Resources[0]
	if rr.Action != ActionProvision {
		t.Fatalf("action = %s, want PROVISION (runway %.1fd)", rr.Action, rr.RunwayDays)
	}
	if math.Abs(rr.RunwayDays-80) > 1.5 {
		t.Fatalf("runway = %.2f, want ~80", rr.RunwayDays)
	}
	if rr.ExhaustionAt == nil {
		t.Fatal("missing exhaustion projection")
	}
	// Exhaustion ≈ end + 80d.
	wantEx := end.Add(80 * 24 * time.Hour)
	if math.Abs(rr.ExhaustionAt.Sub(wantEx).Hours()) > 36 {
		t.Fatalf("exhaustion %v, want ~%v", rr.ExhaustionAt, wantEx)
	}
	if rr.GrowthBytesPerDay < 9*gb || rr.GrowthBytesPerDay > 11*gb {
		t.Fatalf("growth %.0f B/day, want ~10GB", rr.GrowthBytesPerDay)
	}
	if sink.got == nil || sink.got.ReportID != rep.ReportID {
		t.Fatal("sink did not capture the report")
	}
	if rep.Quarter != "2026Q3" || rep.Kind != "capacity_quarterly" {
		t.Fatalf("bad artifact header: %s %s", rep.Quarter, rep.Kind)
	}
}

// Storage runway < 60d → ORDER_HARDWARE; already past disk threshold →
// EXHAUSTED.
func TestGenerate_StorageActionLadder(t *testing.T) {
	th := DefaultThresholds()
	start, _ := th.Window(base)
	mk := func(name string, avail, cap float64, growthPerDay float64) ResourceSpec {
		return ResourceSpec{
			Name: name, Kind: KindStorage,
			SeriesExpr: name + "_used", AvailExpr: name + "_avail",
			CapacityExpr: name + "_cap", ScaleAction: "act " + name,
		}
	}
	specs := []ResourceSpec{
		mk("hw", 50*gb, 1000*gb, 1*gb),    // 50d runway → ORDER_HARDWARE
		mk("hot", 200*gb, 1000*gb, 5*gb),  // 40d → also ORDER_HARDWARE… adjust below
		mk("flat", 900*gb, 1000*gb, 0),    // no growth → OK
		mk("full", 100*gb, 1000*gb, 1*gb), // used 900/1000 = 90% > 70% → EXHAUSTED
	}
	src := &fakeSource{
		series: map[string][]Point{
			"hw_used":   ramp(start, 90, 200*gb, 1*gb),
			"hot_used":  ramp(start, 90, 500*gb, 5*gb),
			"flat_used": ramp(start, 90, 100*gb, 0),
			"full_used": ramp(start, 90, 800*gb, 1*gb),
		},
		instant: map[string]float64{
			"hw_avail": 50 * gb, "hw_cap": 1000 * gb,
			"hot_avail": 200 * gb, "hot_cap": 2000 * gb,
			"flat_avail": 900 * gb, "flat_cap": 1000 * gb,
			"full_avail": 100 * gb, "full_cap": 1000 * gb,
		},
	}
	// full_used last value 800+89=889 GB → util 88.9% ≥ 70% → EXHAUSTED.
	g := newGen(t, src, specs, nil)
	rep, err := g.Generate(context.Background())
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	got := map[string]ResourceReport{}
	for _, r := range rep.Resources {
		got[r.Name] = r
	}
	if got["hw"].Action != ActionOrderHardware {
		t.Fatalf("hw action %s (runway %.1f)", got["hw"].Action, got["hw"].RunwayDays)
	}
	if got["flat"].Action != ActionOK {
		t.Fatalf("flat action %s", got["flat"].Action)
	}
	if !math.IsInf(got["flat"].RunwayDays, 1) {
		t.Fatalf("flat runway %v want +Inf", got["flat"].RunwayDays)
	}
	if got["full"].Action != ActionExhausted {
		t.Fatalf("full action %s", got["full"].Action)
	}
	if rep.WorstAction != ActionExhausted {
		t.Fatalf("worst %s", rep.WorstAction)
	}
	if rep.Actions[ActionOrderHardware] != 2 || rep.Actions[ActionOK] != 1 ||
		rep.Actions[ActionExhausted] != 1 {
		t.Fatalf("action counts %+v", rep.Actions)
	}
	if len(rep.Recommendations) != 3 {
		t.Fatalf("recommendations: %v", rep.Recommendations)
	}
}

// Utilization: CPU growing 0.1%/day from 60% → crosses the 70% alert in
// ~100d → PLAN_PROVISION (inside 180d horizon, outside 90d lead).
func TestGenerate_UtilizationProjection(t *testing.T) {
	th := DefaultThresholds()
	start, _ := th.Window(base)
	spec := ResourceSpec{
		Name: "cpu", Kind: KindUtilization,
		SeriesExpr: "cpu_util", Threshold: 0.70,
		ScaleAction: "shard split",
	}
	src := &fakeSource{series: map[string][]Point{
		"cpu_util": ramp(start, 90, 0.51, 0.001), // → 0.60 at window end
	}}
	g := newGen(t, src, []ResourceSpec{spec}, nil)
	rep, err := g.Generate(context.Background())
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	rr := rep.Resources[0]
	// slope ≈ 0.001/day; runway ≈ (0.70−0.60)/0.001 = 100d.
	if rr.Action != ActionPlanProvision {
		t.Fatalf("action %s (runway %.0fd)", rr.Action, rr.RunwayDays)
	}
	if math.Abs(rr.RunwayDays-100) > 10 {
		t.Fatalf("runway %.1f want ~100", rr.RunwayDays)
	}
	if rr.ProvisionBy == nil || rr.ProvisionBy.Before(end(th)) {
		// provision_by = exhaustion − 90d ≈ now + 10d
		t.Fatalf("provision_by %v", rr.ProvisionBy)
	}
}

// Utilization already past threshold → EXHAUSTED with zero runway.
func TestGenerate_UtilizationExhausted(t *testing.T) {
	th := DefaultThresholds()
	start, _ := th.Window(base)
	spec := ResourceSpec{Name: "mem", Kind: KindUtilization,
		SeriesExpr: "mem_util", Threshold: 0.75, ScaleAction: "add host"}
	src := &fakeSource{series: map[string][]Point{
		"mem_util": ramp(start, 90, 0.70, 0.001), // → 0.789 > 0.75
	}}
	g := newGen(t, src, []ResourceSpec{spec}, nil)
	rep, err := g.Generate(context.Background())
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	if rep.Resources[0].Action != ActionExhausted {
		t.Fatalf("action %s", rep.Resources[0].Action)
	}
	if rep.Resources[0].RunwayDays != 0 {
		t.Fatalf("runway %v", rep.Resources[0].RunwayDays)
	}
}

// A metrics outage must surface as NO_DATA on the resource — visible,
// never silently skipped.
func TestGenerate_SourceErrorNoData(t *testing.T) {
	spec := ResourceSpec{Name: "pg", Kind: KindStorage,
		SeriesExpr: "pg_used", AvailExpr: "pg_avail", ScaleAction: "extend"}
	src := &fakeSource{rangeErr: map[string]error{
		"pg_used": errString("prometheus unreachable"),
	}}
	g := newGen(t, src, []ResourceSpec{spec}, nil)
	rep, err := g.Generate(context.Background())
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	rr := rep.Resources[0]
	if rr.Action != ActionNoData || !math.IsInf(rr.RunwayDays, 1) {
		t.Fatalf("action %s runway %v", rr.Action, rr.RunwayDays)
	}
	if rep.WorstAction != ActionNoData {
		t.Fatalf("worst %s — NO_DATA must outrank provisioning", rep.WorstAction)
	}
}

// The floor growth rate keeps a modelled-growing store projected even
// when the measured series is flat (WAL 15 GB/day model floor).
func TestGenerate_StorageGrowthFloor(t *testing.T) {
	th := DefaultThresholds()
	start, _ := th.Window(base)
	spec := ResourceSpec{
		Name: "wal", Kind: KindStorage,
		SeriesExpr: "wal_used", AvailExpr: "wal_avail", CapacityExpr: "wal_cap",
		FloorGrowthBytesPerDay: 15 * gb,
		ScaleAction:            "extend WAL",
	}
	src := &fakeSource{
		series:  map[string][]Point{"wal_used": ramp(start, 90, 50*gb, 0)},
		instant: map[string]float64{"wal_avail": 300 * gb, "wal_cap": 500 * gb},
	}
	g := newGen(t, src, []ResourceSpec{spec}, nil)
	rep, err := g.Generate(context.Background())
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	rr := rep.Resources[0]
	// 300 GB / 15 GB·day⁻¹ = 20d runway → ORDER_HARDWARE.
	if rr.Action != ActionOrderHardware || math.Abs(rr.RunwayDays-20) > 1 {
		t.Fatalf("action %s runway %.1f", rr.Action, rr.RunwayDays)
	}
}

// Infinite runway serializes as null in the JSON artifact.
func TestReportJSON_InfRunwayNull(t *testing.T) {
	rr := ResourceReport{Name: "x", Kind: KindUtilization, RunwayDays: math.Inf(1), Action: ActionOK}
	b, err := json.Marshal(rr)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if v, ok := m["runway_days"]; !ok || v != nil {
		t.Fatalf("runway_days = %v want null", v)
	}
}

// NextQuarterStart lands on the first instant of the following quarter.
func TestNextQuarterStart(t *testing.T) {
	for _, tc := range []struct {
		in   time.Time
		want time.Time
	}{
		{time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)},
		{time.Date(2026, 3, 31, 23, 59, 0, 0, time.UTC), time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)},
		{time.Date(2026, 11, 30, 0, 0, 0, 0, time.UTC), time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC)},
		{time.Date(2026, 12, 31, 12, 0, 0, 0, time.UTC), time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC)},
	} {
		if got := NextQuarterStart(tc.in); !got.Equal(tc.want) {
			t.Fatalf("NextQuarterStart(%v) = %v want %v", tc.in, got, tc.want)
		}
	}
}

func end(th Thresholds) time.Time { _, e := th.Window(base); return e }
