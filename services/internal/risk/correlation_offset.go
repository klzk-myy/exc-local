// correlation_offset.go — 90-day correlation matrix & margin offset
// provider (Phase-19 Task 19.3.18; spec §13.6g, §24 #232).
//
// The CorrelationMatrix is the production binding of margin.go's
// CorrelationProvider seam: it computes Pearson correlation between FX
// instrument return series over a 90-day rolling window, serves
// Offset(a,b) in basis points (ρ × 10⁴) for the PORTFOLIO-mode
// portfolioMargin path, and optionally supplies per-pair offset factors
// via the CorrelationFactorProvider contract (default 0.5, clamp ≤ 0.8).
//
// The engine applies the §13.6g rules inside margin.go — |ρ_pos| > 0.70
// gate on the side-adjusted correlation (sign-aware natural hedges:
// long EUR/USD + long USD/CHF hedge because the INSTRUMENT pair is
// negatively correlated), offset = min(m1,m2) × |ρ_pos| × factor, the
// 80%-of-gross credit cap and the 20%-of-gross regulatory floor. This
// file owns the ESTIMATES: matrix computation, storage, factors, audit.
//
// Storage: Redis-only. No dedicated correlation-snapshot table exists in
// the migration set (audited 2026-10-XX) — per the task contract the
// matrix persists as margin:corr:{pairA}:{pairB} STRING keys (rho as a
// decimal string) plus a margin:corr:computed_at watermark; per-pair
// factors live in margin:corr:factor:{pairA}:{pairB}. The in-memory map
// is the read path (Offset is on the hot margin path); LoadPersisted
// warms it on restart.
//
// Audit trail (§24 #232 "log every offset computation"): every Offset
// consumption that returns a value is appended to the CorrelationAuditSink
// — production binds RedisCorrelationAudit (LPUSH + LTRIM-bounded
// margin:corr:audit LIST). Since ρ is constant between daily refreshes,
// repeats of the SAME computation within a matrix epoch are
// deduplicated per (pairA, pairB, computedAt) — the trail records every
// distinct computation while bounding write volume.
//
// Groups: pairs with |ρ| > 0.7 form offset groups (connected components
// — handles the circular-correlation edge case); Groups() exposes them
// for the admin surface.
package risk

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strings"
	"sync"
	"time"

	goredis "github.com/redis/go-redis/v9"
)

// corrLookbackDays is the §13.6g canonical 90-day rolling window.
const corrLookbackDays = 90

// corrAuditMaxLen bounds the margin:corr:audit append-only trail.
const corrAuditMaxLen = 10000

// corrGateAbs is the §13.6g |ρ| > 0.70 offset-group gate.
const corrGateAbs = 0.70

// CorrelationAuditKey is the append-only Redis LIST carrying the offset
// audit trail (bounded by corrAuditMaxLen).
const CorrelationAuditKey = "margin:corr:audit"

// CorrelationComputedAtKey is the matrix-refresh watermark.
const CorrelationComputedAtKey = "margin:corr:computed_at"

// CorrelationKey is the margin:corr:{pairA}:{pairB} STRING holding ρ for
// one canonically-ordered instrument pair. (Key builders live here, not
// keys.go — the correlation family is self-contained in this file.)
func CorrelationKey(pairA, pairB string) string {
	a, b := canonicalCorrPair(pairA, pairB)
	return fmt.Sprintf("margin:corr:%s:%s", a, b)
}

// CorrelationFactorKey is the per-pair offset-factor override key.
func CorrelationFactorKey(pairA, pairB string) string {
	a, b := canonicalCorrPair(pairA, pairB)
	return fmt.Sprintf("margin:corr:factor:%s:%s", a, b)
}

// canonicalCorrPair normalizes a symbol pair to its canonical key form:
// uppercase, separators stripped ("EUR/USD"→"EURUSD"), lexicographic
// order — so Offset("EUR/USD","GBP/USD") and ("GBPUSD","EURUSD") agree.
func canonicalCorrPair(a, b string) (string, string) {
	an, bn := normalizeCorrSymbol(a), normalizeCorrSymbol(b)
	if bn < an {
		an, bn = bn, an
	}
	return an, bn
}

// normalizeCorrSymbol strips every separator the display form carries.
func normalizeCorrSymbol(s string) string {
	s = strings.ToUpper(s)
	s = strings.ReplaceAll(s, "/", "")
	s = strings.ReplaceAll(s, "-", "")
	s = strings.ReplaceAll(s, "_", "")
	return s
}

// ---------------------------------------------------------------------------
// Source + audit seams
// ---------------------------------------------------------------------------

// ReturnSeriesSource supplies daily close-to-close returns per
// instrument — production binds ClickHouse (90-day window); tests
// substitute a fake. Returns are fractional (+0.0012 = +12bp),
// oldest→newest; a shorter history is acceptable, len<2 means no
// correlation estimate.
type ReturnSeriesSource interface {
	DailyReturns(ctx context.Context, symbol string, days int) ([]float64, error)
}

// ReturnSeriesFunc adapts a function to ReturnSeriesSource.
type ReturnSeriesFunc func(ctx context.Context, symbol string, days int) ([]float64, error)

// DailyReturns implements ReturnSeriesSource.
func (f ReturnSeriesFunc) DailyReturns(ctx context.Context, symbol string, days int) ([]float64, error) {
	return f(ctx, symbol, days)
}

// CorrelationAuditEvent is one append-only audit entry — the value the
// margin engine consumed for a pair within a matrix epoch.
type CorrelationAuditEvent struct {
	PairA      string    `json:"pair_a"`
	PairB      string    `json:"pair_b"`
	Rho        float64   `json:"rho"`
	RhoBps     int64     `json:"rho_bps"`
	ComputedAt time.Time `json:"computed_at"`
	ConsumedAt time.Time `json:"consumed_at"`
}

// CorrelationAuditSink is the audit-write seam.
type CorrelationAuditSink interface {
	AuditOffset(ctx context.Context, ev CorrelationAuditEvent) error
}

// CorrelationAuditFunc adapts a function to CorrelationAuditSink.
type CorrelationAuditFunc func(ctx context.Context, ev CorrelationAuditEvent) error

// AuditOffset implements CorrelationAuditSink.
func (f CorrelationAuditFunc) AuditOffset(ctx context.Context, ev CorrelationAuditEvent) error {
	return f(ctx, ev)
}

// RedisCorrelationAudit appends audit events to the bounded
// margin:corr:audit LIST (LPUSH+LTRIM in one pipeline).
type RedisCorrelationAudit struct {
	C *goredis.Client
}

// NewRedisCorrelationAudit wraps a go-redis client (excredis.Client
// embeds one — pass rdb.Client).
func NewRedisCorrelationAudit(c *goredis.Client) *RedisCorrelationAudit {
	return &RedisCorrelationAudit{C: c}
}

// AuditOffset implements CorrelationAuditSink.
func (a *RedisCorrelationAudit) AuditOffset(ctx context.Context, ev CorrelationAuditEvent) error {
	payload, err := json.Marshal(ev)
	if err != nil {
		return err
	}
	pipe := a.C.TxPipeline()
	pipe.LPush(ctx, CorrelationAuditKey, payload)
	pipe.LTrim(ctx, CorrelationAuditKey, 0, corrAuditMaxLen-1)
	_, err = pipe.Exec(ctx)
	return err
}

// ---------------------------------------------------------------------------
// CorrelationMatrix — computation + serving
// ---------------------------------------------------------------------------

// CorrelationMatrix implements margin.go's CorrelationProvider (and the
// optional CorrelationFactorProvider extension) over the daily-refreshed
// 90-day matrix. Offset reads are lock-free map lookups; Refresh swaps
// the whole matrix atomically.
type CorrelationMatrix struct {
	src   ReturnSeriesSource
	rdb   *goredis.Client
	audit CorrelationAuditSink
	now   func() time.Time
	logf  func(format string, args ...any)
	days  int

	defaultFactor float64

	mu         sync.RWMutex
	corr       map[string]float64 // "A|B" canonical → ρ
	factors    map[string]float64 // "A|B" canonical → offset factor
	computedAt time.Time
	audited    map[string]time.Time // dedupe: pair → audited epoch
}

// CorrelationMatrixDeps wires the matrix.
type CorrelationMatrixDeps struct {
	// Source is required to REFRESH the matrix; nil still serves a
	// persisted/seeded matrix (Offset works, Refresh errors).
	Source ReturnSeriesSource
	// Redis persists the matrix + watermark for restart warmup;
	// optional (nil ⇒ memory-only).
	Redis *goredis.Client
	// Audit receives every distinct offset computation; nil ⇒ no audit.
	Audit CorrelationAuditSink
	// Days overrides the 90-day window; <=0 = default.
	Days int
	// DefaultFactor is the offset factor when no per-pair override
	// exists; <=0 or >0.8 ⇒ 0.5 (the §13.6g default; >0.8 rejected).
	DefaultFactor float64
	// Factors seeds per-pair offset-factor overrides (canonical or
	// display-form keys accepted, clamped to (0, 0.8]).
	Factors map[string]float64
	Now     func() time.Time
	Logf    func(format string, args ...any)
}

// NewCorrelationMatrix builds the matrix.
func NewCorrelationMatrix(d CorrelationMatrixDeps) *CorrelationMatrix {
	m := &CorrelationMatrix{
		src: d.Source, rdb: d.Redis, audit: d.Audit, now: d.Now, logf: d.Logf,
		days: corrLookbackDays, defaultFactor: 0.5,
		corr: map[string]float64{}, factors: map[string]float64{},
		audited: map[string]time.Time{},
	}
	if d.Days > 0 {
		m.days = d.Days
	}
	if d.DefaultFactor > 0 && d.DefaultFactor <= 0.8 {
		m.defaultFactor = d.DefaultFactor
	}
	for k, v := range d.Factors {
		a, b := canonicalCorrPair(splitCorrKey(k))
		m.factors[a+"|"+b] = clampFactor(v)
	}
	if m.now == nil {
		m.now = func() time.Time { return time.Now().UTC() }
	}
	if m.logf == nil {
		m.logf = func(string, ...any) {}
	}
	return m
}

// splitCorrKey splits "A|B", "A:B", or "A/B" seed keys into (a,b);
// a single token is paired with itself (self-correlation seed — legal).
func splitCorrKey(k string) (string, string) {
	for _, sep := range []string{"|", ":", "/"} {
		if i := strings.Index(k, sep); i > 0 {
			return k[:i], k[i+len(sep):]
		}
	}
	return k, k
}

// clampFactor enforces the §13.6g factor ceiling (≤0.8).
func clampFactor(f float64) float64 {
	if f < 0 {
		return 0
	}
	if f > 0.8 {
		return 0.8
	}
	return f
}

// Refresh recomputes the full pairwise matrix for the symbol universe
// from the 90-day return series, persists it to Redis (when bound) and
// atomically swaps the serving map. Pairwise alignment uses the common
// tail (the shorter history truncates the longer).
func (m *CorrelationMatrix) Refresh(ctx context.Context, symbols []string) error {
	if m.src == nil {
		return fmt.Errorf("correlation matrix: no return-series source bound")
	}
	series := make(map[string][]float64, len(symbols))
	for _, sym := range symbols {
		key := normalizeCorrSymbol(sym)
		if key == "" {
			continue
		}
		r, err := m.src.DailyReturns(ctx, sym, m.days)
		if err != nil {
			return fmt.Errorf("correlation matrix: returns %s: %w", sym, err)
		}
		series[key] = r
	}
	next := map[string]float64{}
	keys := make([]string, 0, len(series))
	for k := range series {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for i := 0; i < len(keys); i++ {
		for j := i + 1; j < len(keys); j++ {
			rho, ok := pearson(series[keys[i]], series[keys[j]])
			if !ok {
				continue // <2 aligned observations ⇒ no estimate
			}
			next[keys[i]+"|"+keys[j]] = rho
		}
	}
	at := m.now().UTC()
	if m.rdb != nil {
		if err := m.persist(ctx, next, at); err != nil {
			return fmt.Errorf("correlation matrix persist: %w", err)
		}
	}
	m.mu.Lock()
	m.corr = next
	m.computedAt = at
	m.audited = map[string]time.Time{} // new epoch — re-audit first uses
	m.mu.Unlock()
	return nil
}

// persist writes every pair key + the computed_at watermark in one
// pipeline (no TTL — the matrix is durable state; staleness is governed
// by the daily refresh cadence, not expiry).
func (m *CorrelationMatrix) persist(ctx context.Context, corr map[string]float64, at time.Time) error {
	pipe := m.rdb.Pipeline()
	for k, rho := range corr {
		a, b := canonicalCorrPair(splitCorrKey(k))
		pipe.Set(ctx, CorrelationKey(a, b), fmt.Sprintf("%.6f", rho), 0)
	}
	pipe.Set(ctx, CorrelationComputedAtKey, at.Format(time.RFC3339Nano), 0)
	_, err := pipe.Exec(ctx)
	return err
}

// LoadPersisted warms the serving map from the Redis keys — the restart
// path so a matrix survives process restarts between daily refreshes.
// Pair keys are scanned under margin:corr:*; factor keys under
// margin:corr:factor:*.
func (m *CorrelationMatrix) LoadPersisted(ctx context.Context) error {
	if m.rdb == nil {
		return fmt.Errorf("correlation matrix: no redis bound")
	}
	var cursor uint64
	next := map[string]float64{}
	factors := map[string]float64{}
	var computedAt time.Time
	for {
		keys, cur, err := m.rdb.Scan(ctx, cursor, "margin:corr:*", 500).Result()
		if err != nil {
			return fmt.Errorf("correlation matrix scan: %w", err)
		}
		cursor = cur
		for _, key := range keys {
			if key == CorrelationAuditKey || key == CorrelationComputedAtKey {
				continue
			}
			if rest, ok := strings.CutPrefix(key, "margin:corr:factor:"); ok {
				v, err := m.rdb.Get(ctx, key).Float64()
				if err == nil {
					a, b := canonicalCorrPair(splitCorrKey(rest))
					factors[a+"|"+b] = clampFactor(v)
				}
				continue
			}
			pair, ok := strings.CutPrefix(key, "margin:corr:")
			if !ok || strings.Count(pair, ":") != 1 {
				continue
			}
			v, err := m.rdb.Get(ctx, key).Float64()
			if err != nil {
				return fmt.Errorf("correlation matrix read %s: %w", key, err)
			}
			a, b := canonicalCorrPair(splitCorrKey(pair))
			next[a+"|"+b] = v
		}
		if cursor == 0 {
			break
		}
	}
	if ts, err := m.rdb.Get(ctx, CorrelationComputedAtKey).Result(); err == nil {
		if t, terr := time.Parse(time.RFC3339Nano, ts); terr == nil {
			computedAt = t
		}
	}
	m.mu.Lock()
	m.corr = next
	for k, v := range factors {
		m.factors[k] = v
	}
	m.computedAt = computedAt
	m.mu.Unlock()
	return nil
}

// Offset implements margin.go's CorrelationProvider: ρ in basis points
// (ρ × 10⁴) for the canonically-ordered pair; ok=false means no
// estimate (treated as ρ=0 by the engine). Consumed values are audited
// once per matrix epoch per pair — identical repeated computations add
// no information and would drown the bounded trail.
func (m *CorrelationMatrix) Offset(a, b string) (float64, bool) {
	an, bn := canonicalCorrPair(a, b)
	key := an + "|" + bn
	m.mu.RLock()
	rho, ok := m.corr[key]
	epoch := m.computedAt
	_, audited := m.audited[key]
	m.mu.RUnlock()
	if !ok {
		return 0, false
	}
	if !audited && m.audit != nil {
		m.mu.Lock()
		if _, done := m.audited[key]; !done {
			m.audited[key] = epoch
			// Release before the sink call — an audit stall must never
			// block the margin read path.
			m.mu.Unlock()
			ev := CorrelationAuditEvent{
				PairA: an, PairB: bn, Rho: rho,
				RhoBps:     int64(math.Round(rho * 10000)),
				ComputedAt: epoch, ConsumedAt: m.now().UTC(),
			}
			if err := m.audit.AuditOffset(context.Background(), ev); err != nil {
				m.logf("correlation audit write %s|%s: %v", an, bn, err)
			}
		} else {
			m.mu.Unlock()
		}
	}
	return rho * 10000, true
}

// OffsetFactor implements CorrelationFactorProvider — the optional
// per-pair factor consult margin.go performs before falling back to the
// §13.6g default. Overrides are clamped to the 0.8 ceiling.
func (m *CorrelationMatrix) OffsetFactor(a, b string) (float64, bool) {
	an, bn := canonicalCorrPair(a, b)
	m.mu.RLock()
	f, ok := m.factors[an+"|"+bn]
	def := m.defaultFactor
	m.mu.RUnlock()
	if !ok || f <= 0 {
		return def, true
	}
	return clampFactor(f), true
}

// SetFactor installs a per-pair offset-factor override (Risk Manager
// knob; values outside (0, 0.8] are rejected — a zero factor silently
// disables the hedge benefit and >0.8 breaches the §13.6g ceiling).
// Persists to Redis when bound.
func (m *CorrelationMatrix) SetFactor(ctx context.Context, pairA, pairB string, factor float64) error {
	if factor <= 0 || factor > 0.8 {
		return fmt.Errorf("correlation factor %v outside (0,0.8]", factor)
	}
	an, bn := canonicalCorrPair(pairA, pairB)
	if m.rdb != nil {
		if err := m.rdb.Set(ctx, CorrelationFactorKey(an, bn),
			fmt.Sprintf("%.6f", factor), 0).Err(); err != nil {
			return fmt.Errorf("correlation factor persist: %w", err)
		}
	}
	m.mu.Lock()
	m.factors[an+"|"+bn] = factor
	m.mu.Unlock()
	return nil
}

// CorrelationEntry is one matrix row for the admin read surface.
type CorrelationEntry struct {
	PairA string  `json:"pair_a"`
	PairB string  `json:"pair_b"`
	Rho   float64 `json:"rho"`
}

// Matrix returns the current matrix + refresh watermark for the admin
// read seam (GET surface is the route cluster's job).
func (m *CorrelationMatrix) Matrix(ctx context.Context) ([]CorrelationEntry, time.Time) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]CorrelationEntry, 0, len(m.corr))
	for k, rho := range m.corr {
		a, b := splitCorrKey(k)
		out = append(out, CorrelationEntry{PairA: a, PairB: b, Rho: rho})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].PairA != out[j].PairA {
			return out[i].PairA < out[j].PairA
		}
		return out[i].PairB < out[j].PairB
	})
	return out, m.computedAt
}

// Groups returns the offset groups — connected components of the
// |ρ| > 0.7 graph (circular groupings collapse to one component; the
// engine still prices pairwise offsets — grouping is an admin risk
// view, not a second offset pass).
func (m *CorrelationMatrix) Groups() [][]string {
	m.mu.RLock()
	parent := map[string]string{}
	var find func(x string) string
	find = func(x string) string {
		if parent[x] != x {
			parent[x] = find(parent[x])
		}
		return parent[x]
	}
	union := func(a, b string) {
		ra, rb := find(a), find(b)
		if ra != rb {
			parent[ra] = rb
		}
	}
	seen := map[string]bool{}
	for k, rho := range m.corr {
		a, b := splitCorrKey(k)
		if math.Abs(rho) <= corrGateAbs {
			continue
		}
		for _, s := range []string{a, b} {
			if !seen[s] {
				seen[s] = true
				parent[s] = s
			}
		}
		union(a, b)
	}
	m.mu.RUnlock()
	comp := map[string][]string{}
	for s := range seen {
		r := find(s)
		comp[r] = append(comp[r], s)
	}
	out := make([][]string, 0, len(comp))
	for _, members := range comp {
		sort.Strings(members)
		out = append(out, members)
	}
	sort.Slice(out, func(i, j int) bool { return out[i][0] < out[j][0] })
	return out
}

// pearson computes the Pearson product-moment correlation over the
// common tail of two return series (the shorter history truncates the
// longer). ok=false means fewer than two aligned observations or a
// degenerate (zero-variance) series.
func pearson(a, b []float64) (float64, bool) {
	n := len(a)
	if len(b) < n {
		n = len(b)
	}
	if n < 2 {
		return 0, false
	}
	a, b = a[len(a)-n:], b[len(b)-n:]
	var ma, mb float64
	for i := 0; i < n; i++ {
		ma += a[i]
		mb += b[i]
	}
	ma, mb = ma/float64(n), mb/float64(n)
	var sab, saa, sbb float64
	for i := 0; i < n; i++ {
		da, db := a[i]-ma, b[i]-mb
		sab += da * db
		saa += da * da
		sbb += db * db
	}
	if saa <= 0 || sbb <= 0 {
		return 0, false
	}
	return sab / math.Sqrt(saa*sbb), true
}

// compile-time seam assertions.
var (
	_ CorrelationProvider       = (*CorrelationMatrix)(nil)
	_ CorrelationFactorProvider = (*CorrelationMatrix)(nil)
	_ CorrelationAuditSink      = (*RedisCorrelationAudit)(nil)
	_ ReturnSeriesSource        = ReturnSeriesFunc(nil)
)
