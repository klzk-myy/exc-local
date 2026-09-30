// Quarterly capacity report generator — Phase-09 Task 9.3.19,
// spec §19.9, §24 #191, capacity-planning.md §5.
//
// Generate pulls each ResourceSpec's trailing-window series through the
// MetricsSource seam, fits a least-squares trend, projects exhaustion
// per the sizing model's runway formulas, grades the resource against
// the 90d/60d provisioning leads and the 180-day horizon, and persists
// the report artifact through the Sink seam (capacity_reports,
// migration 274). A resource whose series is unavailable surfaces as
// ActionNoData — visible, never silently skipped (§2.7).
package capacity

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"time"

	excerrors "exchange/pkg/errors"
)

// Step is the range-query sampling step — hourly samples over the 90d
// window keep the regression cheap and the trend intact.
const Step = time.Hour

// ResourceReport is one resource's section of the quarterly report.
type ResourceReport struct {
	Name        string `json:"name"`
	Kind        string `json:"kind"`
	Samples     int    `json:"samples"`
	ScaleAction string `json:"scale_action"`

	// Utilization resources: utilization ratios (0..1).
	// Storage resources: Current is the used fraction of capacity.
	Current   float64 `json:"current"`
	Peak      float64 `json:"peak"`
	Threshold float64 `json:"threshold,omitempty"`

	// Storage resources.
	UsedBytes         int64   `json:"used_bytes,omitempty"`
	AvailBytes        int64   `json:"avail_bytes,omitempty"`
	CapacityBytes     int64   `json:"capacity_bytes,omitempty"`
	GrowthBytesPerDay float64 `json:"growth_bytes_per_day,omitempty"`

	// GrowthPerDay is the fitted daily slope — bytes/day for storage,
	// utilization-ratio/day for utilization resources.
	GrowthPerDay float64    `json:"growth_per_day"`
	RunwayDays   float64    `json:"runway_days"`             // +Inf serialized as null below
	ExhaustionAt *time.Time `json:"exhaustion_at,omitempty"` // projected threshold crossing
	ProvisionBy  *time.Time `json:"provision_by,omitempty"`  // exhaustion − ProvisionLeadDays
	Action       string     `json:"action"`                  // Action* constants
	Detail       string     `json:"detail,omitempty"`
}

// MarshalJSON renders +Inf runways as null (JSON has no infinity).
func (r ResourceReport) MarshalJSON() ([]byte, error) {
	type alias ResourceReport
	a := struct {
		alias
		RunwayDays *float64 `json:"runway_days"`
	}{alias: alias(r)}
	if !math.IsInf(r.RunwayDays, 1) {
		v := r.RunwayDays
		a.RunwayDays = &v
	}
	return json.Marshal(a)
}

// Report is the quarterly artifact (retention.Report-style JSON shape:
// id + period + per-resource findings + action counts).
type Report struct {
	ReportID        string           `json:"report_id"`
	Kind            string           `json:"kind"` // "capacity_quarterly"
	GeneratedAt     time.Time        `json:"generated_at"`
	PeriodStart     time.Time        `json:"period_start"`
	PeriodEnd       time.Time        `json:"period_end"`
	Quarter         string           `json:"quarter"` // e.g. "2026Q3" (calendar quarter of PeriodEnd)
	HorizonDays     int              `json:"horizon_days"`
	WindowDays      int              `json:"window_days"`
	Resources       []ResourceReport `json:"resources"`
	Actions         map[string]int   `json:"actions"` // Action* → count
	WorstAction     string           `json:"worst_action"`
	Recommendations []string         `json:"recommendations"`
}

// Sink persists/emits the generated artifact. PgSink writes
// capacity_reports (migration 274); callers may compose a file/object
// sink alongside.
type Sink interface {
	Store(ctx context.Context, r *Report) error
}

// Generator produces quarterly capacity reports.
type Generator struct {
	src       MetricsSource
	th        Thresholds
	resources []ResourceSpec
	sink      Sink
	now       func() time.Time
	newID     func() string
}

// New wires the generator. sink may be nil (report returned but not
// persisted — the caller owns emission).
func New(src MetricsSource, th Thresholds, resources []ResourceSpec, sink Sink) (*Generator, error) {
	if src == nil {
		return nil, excerrors.New("INTERNAL_ERROR", "capacity: metrics source not wired")
	}
	if len(resources) == 0 {
		return nil, excerrors.New("INTERNAL_ERROR", "capacity: no resource specs")
	}
	return &Generator{
		src: src, th: th, resources: resources, sink: sink,
		now:   func() time.Time { return time.Now().UTC() },
		newID: newRunID,
	}, nil
}

// SetClock overrides the clock (tests).
func (g *Generator) SetClock(f func() time.Time) { g.now = f }

// Generate runs the report over the trailing WindowDays window ending
// at now, persists via the sink when wired, and returns the artifact.
// Fail-visible, never fail-silent: a source error for one resource
// yields a NO_DATA row; a source-wide outage yields NO_DATA on every
// row and the report still emits.
func (g *Generator) Generate(ctx context.Context) (*Report, error) {
	now := g.now()
	start, end := g.th.Window(now)
	rep := &Report{
		ReportID:    g.newID(),
		Kind:        "capacity_quarterly",
		GeneratedAt: now,
		PeriodStart: start, PeriodEnd: end,
		Quarter:     quarterLabel(end),
		HorizonDays: g.th.HorizonDays,
		WindowDays:  g.th.WindowDays,
		Actions:     map[string]int{},
	}
	worst := ActionOK
	for _, spec := range g.resources {
		rr := g.evaluate(ctx, spec, start, end)
		rep.Resources = append(rep.Resources, rr)
		rep.Actions[rr.Action]++
		if actionRank(rr.Action) > actionRank(worst) {
			worst = rr.Action
		}
		if rr.Action != ActionOK {
			rep.Recommendations = append(rep.Recommendations,
				fmt.Sprintf("%s: %s — %s", rr.Name, rr.Action, rr.ScaleAction))
		}
	}
	rep.WorstAction = worst
	sort.Strings(rep.Recommendations)

	if g.sink != nil {
		if err := g.sink.Store(ctx, rep); err != nil {
			return rep, excerrors.Wrap("INTERNAL_ERROR",
				"capacity report sink", err)
		}
	}
	return rep, nil
}

// evaluate computes one resource's runway section.
func (g *Generator) evaluate(ctx context.Context, spec ResourceSpec,
	start, end time.Time) ResourceReport {
	rr := ResourceReport{
		Name: spec.Name, Kind: spec.Kind, ScaleAction: spec.ScaleAction,
		Threshold: spec.Threshold,
	}
	series, err := g.src.Range(ctx, spec.SeriesExpr, start, end, Step)
	if err != nil {
		rr.Action = ActionNoData
		rr.RunwayDays = math.Inf(1)
		rr.Detail = "series unavailable: " + err.Error()
		return rr
	}
	rr.Samples = len(series)
	if len(series) == 0 {
		rr.Action = ActionNoData
		rr.RunwayDays = math.Inf(1)
		rr.Detail = "empty series"
		return rr
	}

	slopePerSec, _ := regress(series, start)
	slopePerDay := slopePerSec * 86400

	switch spec.Kind {
	case KindStorage:
		return g.evalStorage(ctx, spec, series, rr, slopePerDay, end)
	case KindUtilization:
		return g.evalUtilization(spec, series, rr, slopePerDay, end)
	default:
		rr.Action = ActionNoData
		rr.RunwayDays = math.Inf(1)
		rr.Detail = "unknown resource kind " + spec.Kind
		return rr
	}
}

// evalStorage applies runway_days = avail_bytes / growth_bytes_per_day
// (capacity-planning.md §3). Growth is the measured 90d regression
// slope; FloorGrowthBytesPerDay (when configured) acts as the model
// floor so a flat-but-modelled-growing store still gets projected.
func (g *Generator) evalStorage(ctx context.Context, spec ResourceSpec,
	series []Point, rr ResourceReport, slopePerDay float64, end time.Time) ResourceReport {
	avail, err := g.src.Instant(ctx, spec.AvailExpr, end)
	if err != nil {
		rr.Action = ActionNoData
		rr.RunwayDays = math.Inf(1)
		rr.Detail = "avail query failed: " + err.Error()
		return rr
	}
	rr.AvailBytes = int64(math.Max(avail, 0))

	if spec.CapacityExpr != "" {
		if capBytes, cerr := g.src.Instant(ctx, spec.CapacityExpr, end); cerr == nil && capBytes > 0 {
			rr.CapacityBytes = int64(capBytes)
		}
	}
	used := series[len(series)-1].Value
	rr.UsedBytes = int64(math.Max(used, 0))
	if rr.CapacityBytes == 0 {
		rr.CapacityBytes = rr.UsedBytes + rr.AvailBytes
	}
	rr.Peak = maxPoint(series)
	if rr.CapacityBytes > 0 {
		rr.Current = float64(rr.UsedBytes) / float64(rr.CapacityBytes)
		rr.Threshold = g.th.DiskAlert
	}

	growth := slopePerDay
	if growth < spec.FloorGrowthBytesPerDay {
		growth = spec.FloorGrowthBytesPerDay
	}
	rr.GrowthBytesPerDay = growth
	rr.GrowthPerDay = growth

	rr.RunwayDays, rr.ExhaustionAt, rr.ProvisionBy = runway(
		float64(rr.AvailBytes), growth, g.th, end)
	rr.Action = gradeStorage(rr, g.th)
	if growth <= 0 && rr.Action == ActionOK {
		rr.Detail = "no measured growth and no configured floor — runway open-ended"
	}
	return rr
}

// evalUtilization projects days until the series crosses the alert
// threshold: runway = (threshold − current) / slope_per_day. A flat or
// declining series is an open-ended runway (OK).
func (g *Generator) evalUtilization(spec ResourceSpec, series []Point,
	rr ResourceReport, slopePerDay float64, end time.Time) ResourceReport {
	cur := series[len(series)-1].Value
	rr.Current = cur
	rr.Peak = maxPoint(series)
	rr.GrowthPerDay = slopePerDay

	if cur >= spec.Threshold {
		rr.RunwayDays = 0
		now := end
		rr.ExhaustionAt = &now
		rr.ProvisionBy = &now
		rr.Action = ActionExhausted
		rr.Detail = fmt.Sprintf("utilization %.1f%% already past %.0f%% threshold",
			cur*100, spec.Threshold*100)
		return rr
	}
	if slopePerDay <= 0 {
		rr.RunwayDays = math.Inf(1)
		rr.Action = ActionOK
		rr.Detail = "non-growing trend — no threshold crossing projected"
		return rr
	}
	runway := (spec.Threshold - cur) / slopePerDay
	rr.RunwayDays = runway
	ex := end.Add(time.Duration(runway * float64(24*time.Hour)))
	rr.ExhaustionAt = &ex
	pb := ex.AddDate(0, 0, -g.th.ProvisionLeadDays)
	rr.ProvisionBy = &pb
	rr.Action = gradeRunway(runway, g.th)
	return rr
}

// runway converts remaining headroom + daily growth into days, plus the
// exhaustion/provision timestamps. Non-positive growth → +Inf.
func runway(headroom, growthPerDay float64, th Thresholds, end time.Time) (float64, *time.Time, *time.Time) {
	if growthPerDay <= 0 {
		return math.Inf(1), nil, nil
	}
	r := headroom / growthPerDay
	if r < 0 {
		r = 0
	}
	ex := end.Add(time.Duration(r * float64(24*time.Hour)))
	pb := ex.AddDate(0, 0, -th.ProvisionLeadDays)
	return r, &ex, &pb
}

// gradeStorage maps a storage resource to its action level.
func gradeStorage(rr ResourceReport, th Thresholds) string {
	if rr.CapacityBytes > 0 && rr.Current >= th.DiskAlert {
		return ActionExhausted // already inside the 30%-headroom breach zone
	}
	return gradeRunway(rr.RunwayDays, th)
}

// gradeRunway is the shared 90/60/180-day ladder (capacity-planning.md §3).
func gradeRunway(runwayDays float64, th Thresholds) string {
	switch {
	case math.IsInf(runwayDays, 1):
		return ActionOK
	case runwayDays <= 0:
		return ActionExhausted
	case runwayDays < float64(th.OrderLeadDays):
		return ActionOrderHardware
	case runwayDays < float64(th.ProvisionLeadDays):
		return ActionProvision
	case runwayDays < float64(th.HorizonDays):
		return ActionPlanProvision
	default:
		return ActionOK
	}
}

// actionRank orders actions worst-first (higher = worse).
func actionRank(a string) int {
	switch a {
	case ActionNoData:
		return 4 // visible-not-OK: a blind spot outranks provisioning work
	case ActionExhausted:
		return 3
	case ActionOrderHardware:
		return 2
	case ActionProvision:
		return 1
	case ActionPlanProvision:
		return 0
	default:
		return -1
	}
}

// maxPoint returns the largest sampled value.
func maxPoint(series []Point) float64 {
	m := series[0].Value
	for _, p := range series[1:] {
		if p.Value > m {
			m = p.Value
		}
	}
	return m
}

// regress fits value = intercept + slope*(seconds since base) by least
// squares over the series.
func regress(series []Point, base time.Time) (slopePerSec, intercept float64) {
	var sx, sy, sxx, sxy float64
	n := float64(len(series))
	for _, p := range series {
		x := p.At.Sub(base).Seconds()
		sx += x
		sy += p.Value
		sxx += x * x
		sxy += x * p.Value
	}
	denom := n*sxx - sx*sx
	if denom == 0 || n == 0 {
		return 0, sy / math.Max(n, 1)
	}
	slopePerSec = (n*sxy - sx*sy) / denom
	intercept = (sy - slopePerSec*sx) / n
	return slopePerSec, intercept
}

// quarterLabel returns e.g. "2026Q3" for the calendar quarter holding t.
func quarterLabel(t time.Time) string {
	u := t.UTC()
	q := (int(u.Month())-1)/3 + 1
	return fmt.Sprintf("%dQ%d", u.Year(), q)
}

// newRunID mints a random UUID (v4 layout) — the report correlation id
// (same convention as the retention enforcer's run_id).
func newRunID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("cap-%d", time.Now().UnixNano())
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	h := hex.EncodeToString(b[:])
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:32]
}
