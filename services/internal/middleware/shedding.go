// Task 9.3.10 — graceful load shedding under extreme load.
//
// Contract:
//   - queue depth > 500 → shed lowest-tier requests first;
//   - rejections are HTTP 503 CAPACITY_EXCEEDED (§23) with Retry-After;
//   - shedding is gradual: 10% → 25% → 50% as depth climbs the bands;
//   - recovery when depth < 250 — hysteresis is the 250–500 band where
//     an active stage holds (enter >500, exit <250);
//   - cancel requests are unconditionally exempt (task items 5–7):
//     DELETE /api/v1/orders*, the scoped mass-cancel routes, FIX-originated
//     OrderCancelRequest (35=F) / OrderMassCancelRequest (35=q) which the
//     bridge tags with header X-Exc-Priority: CANCEL_EXEMPT or the
//     WithCancelExempt context marker before entering this middleware;
//   - spec §2.7.3 capacity watermarks: at ≥80% of ring capacity shedding
//     is at maximum band; at ≥95% the engine emits CRITICAL_BACKPRESSURE
//     and every non-exempt request is refused ENGINE_OVERLOAD.
//
// Depth sampling is decoupled from the request path: Run(ctx) polls the
// configured DepthFunc at Interval and holds the resulting stage in an
// atomic — the hot path never blocks on a ring-buffer read.
package middleware

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"exchange/internal/ratelimit"
)

// PriorityCancelExempt is the §9.3.10 priority-lane tag: the gateway
// stamps it on cancel-shaped traffic (REST DELETE order paths carry it
// implicitly; FIX bridges set the header or context marker).
const PriorityCancelExempt = "CANCEL_EXEMPT"

// HeaderPriority carries the pre-dispatch priority tag into the shed
// middleware — FIX OrderCancelRequest/OrderMassCancelRequest arrive here
// already converted to REST-shaped calls or via direct tagging.
const HeaderPriority = "X-Exc-Priority"

type ctxKeyCancelExempt struct{}

// WithCancelExempt marks the request context as cancel-priority — the
// 100µs-lane contract from task items 6–7.
func WithCancelExempt(ctx context.Context) context.Context {
	return context.WithValue(ctx, ctxKeyCancelExempt{}, true)
}

// CancelExempt reports whether ctx carries the cancel-priority marker.
func CancelExempt(ctx context.Context) bool {
	v, _ := ctx.Value(ctxKeyCancelExempt{}).(bool)
	return v
}

// ShedBand is one ladder step: while sampled depth ≥ MinDepth (and no
// deeper band applies), Pct percent of eligible requests are shed.
type ShedBand struct {
	MinDepth int64
	Pct      int // 0..100
}

// ShedConfig parameterizes the Shedder.
type ShedConfig struct {
	// Depth samples the inbound queue depth (Aeron ingress ring
	// occupancy sum, or any queue gauge). Required.
	Depth func() int64
	// Capacity is the total ring capacity backing Depth; enables the
	// §2.7 watermarks (≥80% max-shed, ≥95% ENGINE_OVERLOAD). 0 disables
	// the watermark logic — bands still apply.
	Capacity int64
	// Bands, ascending by MinDepth. Default: >500→10%, >2000→25%,
	// >4000→50%.
	Bands []ShedBand
	// ExitDepth ends shedding when sampled depth falls below it.
	// Default 250 (task item 4).
	ExitDepth int64
	// Interval between depth samples. Default 250ms — the stage reacts
	// inside the §2.7 ≤500ms degradation-transition window.
	Interval time.Duration
	// Now overridable for tests.
	Now func() time.Time
}

func (c *ShedConfig) defaults() {
	if len(c.Bands) == 0 {
		c.Bands = []ShedBand{
			{MinDepth: 501, Pct: 10},  // task item 1: "depth > 500"
			{MinDepth: 2001, Pct: 25}, // gradual 10→25→50 (item 3)
			{MinDepth: 4001, Pct: 50},
		}
	}
	if c.ExitDepth <= 0 {
		c.ExitDepth = 250
	}
	if c.Interval <= 0 {
		c.Interval = 250 * time.Millisecond
	}
	if c.Now == nil {
		c.Now = time.Now
	}
}

// shedTierRank orders tiers lowest-first — the shed order mandated by
// "lowest-tier requests first". Institutional and Admin are never shed.
var shedRank = map[ratelimit.Tier]int{
	ratelimit.TierPublic:        0,
	ratelimit.TierBasic:         1,
	ratelimit.TierStandard:      2,
	ratelimit.TierProfessional:  3,
	ratelimit.TierInstitutional: -1, // never shed
	ratelimit.TierAdmin:         -1,
}

// stageMaxRank is which tiers each stage reaches: stage 1 sheds
// public+basic, stage 2 adds standard, stage 3 adds professional.
var stageMaxRank = map[int]int{1: 1, 2: 2, 3: 3}

// Shedder holds the live shed state.
type Shedder struct {
	cfg ShedConfig

	stage     atomic.Int32 // 0 = no shedding … 3 = max band
	overload  atomic.Bool  // §2.7 95% watermark — ENGINE_OVERLOAD
	sampled   atomic.Int64 // last observed depth
	sampledAt atomic.Int64 // unixnano
	depthErr  atomic.Bool  // sampler saw a negative/erroneous reading
	shedCtr   [4]atomic.Uint64
	totalShed atomic.Uint64
	totalPass atomic.Uint64
}

// NewShedder builds a shedder; Depth nil → fail-closed (stage never
// leaves 0 is wrong — a missing depth signal must not read as healthy;
// it forces stage 1, the mildest conservative posture).
func NewShedder(cfg ShedConfig) *Shedder {
	cfg.defaults()
	s := &Shedder{cfg: cfg}
	if cfg.Depth == nil {
		s.depthErr.Store(true)
		s.stage.Store(1)
	}
	return s
}

// Run samples depth every Interval until ctx ends. Call once per service.
func (s *Shedder) Run(ctx context.Context) {
	s.sample()
	t := time.NewTicker(s.cfg.Interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.sample()
		}
	}
}

// Sample performs one depth evaluation (exported for tests and for
// synchronous probing from health surfaces).
func (s *Shedder) Sample() { s.sample() }

func (s *Shedder) sample() {
	depth := s.cfg.Depth()
	s.sampled.Store(depth)
	s.sampledAt.Store(s.cfg.Now().UnixNano())
	if depth < 0 {
		// A depth gauge that cannot report is not "healthy": hold the
		// mildest shed stage rather than admit unbounded ingress.
		if !s.depthErr.Swap(true) {
			s.stage.CompareAndSwap(0, 1)
		}
		return
	}
	s.depthErr.Store(false)

	// §2.7 watermarks.
	if s.cfg.Capacity > 0 {
		pct100 := depth * 100 / s.cfg.Capacity
		if pct100 >= 95 {
			s.overload.Store(true)
			s.stage.Store(3)
			return
		}
		s.overload.Store(false)
		if pct100 >= 80 {
			s.stage.Store(3)
			return
		}
	} else {
		s.overload.Store(false)
	}

	cur := int(s.stage.Load())
	switch {
	case depth < s.cfg.ExitDepth:
		s.stage.Store(0) // recovery below the exit watermark
	case depth >= s.cfg.Bands[0].MinDepth:
		// hysteresis band only matters between ExitDepth and Bands[0];
		// above it the stage follows the depth ladder.
		s.stage.Store(int32(s.stageForDepth(depth)))
	default:
		// ExitDepth ≤ depth < first band: hold current stage (hysteresis).
		_ = cur
	}
}

// stageForDepth maps depth → the deepest applicable band index (1-based
// stage number; 0 = none).
func (s *Shedder) stageForDepth(depth int64) int {
	stage := 0
	for i, b := range s.cfg.Bands {
		if depth >= b.MinDepth {
			stage = i + 1
		}
	}
	return stage
}

func (s *Shedder) bandForStage(stage int) (ShedBand, bool) {
	if stage <= 0 || stage > len(s.cfg.Bands) {
		return ShedBand{}, false
	}
	return s.cfg.Bands[stage-1], true
}

// Stage reports the current shed stage (0 = off).
func (s *Shedder) Stage() int { return int(s.stage.Load()) }

// Overload reports the §2.7 95% watermark state.
func (s *Shedder) Overload() bool { return s.overload.Load() }

// Depth reports the last sampled queue depth.
func (s *Shedder) Depth() int64 { return s.sampled.Load() }

// ShedStats is the observability snapshot.
type ShedStats struct {
	Stage     int    `json:"stage"`
	Overload  bool   `json:"overload"`
	Depth     int64  `json:"depth"`
	DepthErr  bool   `json:"depth_error"`
	ShedTotal uint64 `json:"shed_total"`
	PassTotal uint64 `json:"pass_total"`
}

// Stats snapshots the shedder for /metrics and the ops health export.
func (s *Shedder) Stats() ShedStats {
	return ShedStats{
		Stage:     s.Stage(),
		Overload:  s.Overload(),
		Depth:     s.Depth(),
		DepthErr:  s.depthErr.Load(),
		ShedTotal: s.totalShed.Load(),
		PassTotal: s.totalPass.Load(),
	}
}

// sheddableTier reports whether tier is eligible for the current stage.
func (s *Shedder) sheddableTier(t ratelimit.Tier, stage int) bool {
	rank, ok := shedRank[t]
	if !ok {
		rank = 0 // unknown tier ⇒ strictest ⇒ public
	}
	if rank < 0 {
		return false
	}
	max, ok := stageMaxRank[stage]
	return ok && rank <= max
}

// ShouldShed decides for one request; exempt traffic never sheds.
func (s *Shedder) ShouldShed(r *http.Request, tier ratelimit.Tier) bool {
	if ShedExempt(r) {
		s.totalPass.Add(1)
		return false
	}
	if s.overload.Load() {
		s.totalShed.Add(1)
		return true
	}
	stage := s.Stage()
	if stage == 0 {
		s.totalPass.Add(1)
		return false
	}
	if !s.sheddableTier(tier, stage) {
		s.totalPass.Add(1)
		return false
	}
	band, ok := s.bandForStage(stage)
	if !ok {
		s.totalPass.Add(1)
		return false
	}
	// Deterministic fractional shed: of every 100 eligible requests,
	// the first Pct are dropped — reproducible under test and fair
	// across concurrent callers.
	n := s.shedCtr[stage].Add(1) - 1
	if int(n%100) < band.Pct {
		s.totalShed.Add(1)
		return true
	}
	s.totalPass.Add(1)
	return false
}

// ShedExempt reports whether a request bypasses shedding:
//   - context/header tagged CANCEL_EXEMPT (FIX 35=F / 35=q priority lane);
//   - REST cancels: DELETE under /api/v1/orders + mass-cancel endpoints;
//   - health, readiness, metrics and the public status feed — the
//     observability surface must stay up precisely while shedding;
//   - WebSocket upgrades (the ws.Server applies its own capacity caps).
func ShedExempt(r *http.Request) bool {
	if CancelExempt(r.Context()) ||
		strings.EqualFold(r.Header.Get(HeaderPriority), PriorityCancelExempt) {
		return true
	}
	p := r.URL.Path
	if r.Method == http.MethodDelete &&
		(strings.HasPrefix(p, "/api/v1/orders") ||
			strings.HasPrefix(p, "/api/v1/admin/orders")) {
		return true
	}
	if p == "/api/v1/admin/orders/mass-cancel" {
		return true
	}
	for _, pre := range shedExemptPrefixes {
		if strings.HasPrefix(p, pre) {
			return true
		}
	}
	return false
}

var shedExemptPrefixes = []string{
	"/health", "/ready", "/metrics", "/ws/",
	"/api/v1/system/", // status feed stays up while shedding
}

// ShedOptions wires the middleware.
type ShedOptions struct {
	// ResolveTier maps the request to its §8.3 tier — same seam as the
	// rate limiter. nil ⇒ public.
	ResolveTier func(r *http.Request) ratelimit.Tier
	// Emit writes the §8.7 envelope (router.WriteError). Required.
	Emit GateEmitter
	// RetryAfter on 503s; default 2s.
	RetryAfter time.Duration
}

// Shedding returns middleware that rejects shed-eligible requests with
// 503 — CAPACITY_EXCEEDED inside the graduated bands, ENGINE_OVERLOAD at
// the 95% §2.7 watermark. Both codes are registered in §23 with HTTP 503.
func Shedding(s *Shedder, opts ShedOptions) func(http.Handler) http.Handler {
	if opts.RetryAfter <= 0 {
		opts.RetryAfter = 2 * time.Second
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			tier := ratelimit.TierPublic
			if opts.ResolveTier != nil {
				tier = opts.ResolveTier(r)
			}
			if s.ShouldShed(r, tier) {
				w.Header().Set("Retry-After",
					strconv.Itoa(int(opts.RetryAfter.Seconds())))
				if s.Overload() {
					opts.Emit(w, r, "ENGINE_OVERLOAD",
						"engine ingress at 95% watermark — capacity exceeded",
						map[string]any{"shed_stage": s.Stage(), "queue_depth": s.Depth()})
				} else {
					opts.Emit(w, r, "CAPACITY_EXCEEDED",
						"load shedding active — retry later",
						map[string]any{"shed_stage": s.Stage(), "tier": string(tier)})
				}
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}
