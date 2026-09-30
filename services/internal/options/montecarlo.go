package options

import (
	"fmt"
	"math"
	"math/rand/v2"
)

// ---------------------------------------------------------------------------
// MCPricer — deterministic seeded Monte Carlo with antithetic variates
// (spec §15.2 "Monte Carlo for barrier/binary options"; path-dependent
// exotics run ≥100,000 simulations). Phase-22 Tasks 22.3.5/22.3.6.
//
// Determinism contract: identical (config, option, market) inputs produce
// bit-identical results — paths come from a seeded PCG generator
// (math/rand/v2, stdlib — no external RNG dependency) and Box-Muller
// normals with a clamped uniform, so the sequence is platform-stable.
// ---------------------------------------------------------------------------

// MCConfig tunes a Monte Carlo run. Paths counts antithetic pairs ×2 —
// i.e. the total number of simulated paths, always even.
type MCConfig struct {
	// Paths is the total path count including antithetic mirrors.
	// Spec §15.2 floor for exotics is 100,000 — DefaultMCConfig meets it.
	Paths int
	// Steps is the number of monitoring intervals per path — the discrete
	// barrier-monitoring analogue of spec §15.7's mark-tick evaluation.
	Steps int
	// Seed anchors the PCG stream. Any value is valid; the same seed
	// replays the identical path set (deterministic pricing/replay).
	Seed uint64
}

// DefaultMCConfig is the spec-conforming default: 100,000 paths
// (50k antithetic pairs), 252 monitoring steps (~1 trading year), fixed
// seed so an unconfigured pricer is still deterministic.
var DefaultMCConfig = MCConfig{Paths: 100_000, Steps: 252, Seed: 0x5EC0FFEE}

func (c MCConfig) withDefaults() MCConfig {
	if c.Paths == 0 {
		c.Paths = DefaultMCConfig.Paths
	}
	if c.Steps == 0 {
		c.Steps = DefaultMCConfig.Steps
	}
	if c.Seed == 0 {
		c.Seed = DefaultMCConfig.Seed
	}
	return c
}

// validate enforces the fail-closed config contract.
func (c MCConfig) validate(op string) error {
	switch {
	case c.Paths < 2 || c.Paths%2 != 0:
		return invalidInput(op, fmt.Sprintf("paths must be a positive even number, got %d", c.Paths))
	case c.Steps < 1:
		return invalidInput(op, fmt.Sprintf("steps must be >= 1, got %d", c.Steps))
	case c.Paths > 50_000_000:
		// Bounded-work guard: runaway path counts are a CPU DoS, not a
		// price — refuse (L2 transaction-boundary pessimism, §2.7).
		return invalidInput(op, fmt.Sprintf("paths %d exceeds the 50M safety bound", c.Paths))
	case c.Steps > 1_000_000:
		return invalidInput(op, fmt.Sprintf("steps %d exceeds the 1M safety bound", c.Steps))
	}
	return nil
}

// MCResult is one Monte Carlo price estimate.
type MCResult struct {
	Value  float64 `json:"value"`   // discounted mean payoff
	StdErr float64 `json:"std_err"` // standard error of the mean
	Paths  int     `json:"paths"`   // total paths simulated
	Steps  int     `json:"steps"`   // monitoring steps per path
	Seed   uint64  `json:"seed"`    // seed that generated the run (replay key)
}

// MCPricer is a configured Monte Carlo engine. It is stateless per call —
// every Price* method re-seeds, so results never depend on call order.
type MCPricer struct {
	cfg MCConfig
}

// NewMCPricer returns a pricer; zero-valued config fields take
// DefaultMCConfig values.
func NewMCPricer(cfg MCConfig) *MCPricer {
	return &MCPricer{cfg: cfg.withDefaults()}
}

// normalSource draws standard normals from a seeded PCG stream via
// Box-Muller — fully specified, so path sequences are reproducible
// across hosts and Go releases (no library ziggurat dependence).
type normalSource struct {
	rng     *rand.Rand
	spare   float64
	hasSpar bool
}

func newNormalSource(seed uint64) *normalSource {
	// PCG takes two uint64s; derive the second deterministically from the
	// seed so a single Seed field fully pins the stream.
	return &normalSource{rng: rand.New(rand.NewPCG(seed, seed^0x9E3779B97F4A7C15))}
}

func (n *normalSource) next() float64 {
	if n.hasSpar {
		n.hasSpar = false
		return n.spare
	}
	u1 := n.rng.Float64()
	if u1 <= 0 {
		// Clamp rather than retry: termination is unconditional and the
		// clamp point is fixed for a given stream — determinism kept.
		u1 = math.SmallestNonzeroFloat64
	}
	u2 := n.rng.Float64()
	r := math.Sqrt(-2 * math.Log(u1))
	n.spare = r * math.Sin(2*math.Pi*u2)
	n.hasSpar = true
	return r * math.Cos(2*math.Pi*u2)
}

// terminal simulates Paths terminal spot values (single-step GBM) and
// averages payoff(sT) — for European payoffs (vanilla MC reference,
// binary digitals) where intermediate monitoring is meaningless.
//
// Exact GBM step under domestic risk-neutral measure:
//
//	S_T = S·exp((rd−rf−σ²/2)·T + σ√T·z),  rd−rf = ln(DFf/DFd)/T
func (p *MCPricer) terminal(m Market, payoff func(sT float64) float64) (MCResult, error) {
	const op = "MCPricer.terminal"
	if err := m.validate(op); err != nil {
		return MCResult{}, err
	}
	cfg := p.cfg.withDefaults()
	if err := cfg.validate(op); err != nil {
		return MCResult{}, err
	}
	src := newNormalSource(cfg.Seed)
	drift := math.Log(m.DFf/m.DFd) - 0.5*m.Vol*m.Vol*m.T // (rd−rf−σ²/2)·T
	diff := m.Vol * math.Sqrt(m.T)
	pairs := cfg.Paths / 2
	var sum, sumSq float64
	for i := 0; i < pairs; i++ {
		z := src.next()
		xp := payoff(m.Spot * math.Exp(drift+diff*z)) // antithetic +
		xm := payoff(m.Spot * math.Exp(drift-diff*z)) // antithetic −
		x := (xp + xm) * 0.5
		sum += x
		sumSq += x * x
	}
	return mcAggregate(op, cfg, sum, sumSq, pairs)
}

// pathDependent simulates Steps-interval GBM paths with discrete barrier
// monitoring — each step boundary is a mark-tick observation, matching
// the spec §15.7 knock-on-discrete-ticks contract (and the production
// BarrierMonitor semantics: same inequality, same per-tick evaluation).
// payoff receives (sT, touched).
func (p *MCPricer) pathDependent(m Market, barrier BarrierType, level float64,
	payoff func(sT float64, touched bool) float64) (MCResult, error) {
	const op = "MCPricer.pathDependent"
	if err := m.validate(op); err != nil {
		return MCResult{}, err
	}
	cfg := p.cfg.withDefaults()
	if err := cfg.validate(op); err != nil {
		return MCResult{}, err
	}
	src := newNormalSource(cfg.Seed)
	dt := m.T / float64(cfg.Steps)
	mu := math.Log(m.DFf/m.DFd) / m.T // rd−rf
	incr := (mu - 0.5*m.Vol*m.Vol) * dt
	diff := m.Vol * math.Sqrt(dt)
	up := barrier.IsUp()
	pairs := cfg.Paths / 2
	var sum, sumSq float64
	for i := 0; i < pairs; i++ {
		sp, sm := m.Spot, m.Spot
		// A barrier at-or-inside the current market is knocked at t=0 —
		// consistent with BarrierMonitor's first-tick evaluation.
		tp := up && sp >= level || !up && sp <= level
		tm := up && sm >= level || !up && sm <= level
		for s := 0; s < cfg.Steps; s++ {
			z := src.next()
			sp *= math.Exp(incr + diff*z)
			sm *= math.Exp(incr - diff*z)
			if up {
				tp = tp || sp >= level
				tm = tm || sm >= level
			} else {
				tp = tp || sp <= level
				tm = tm || sm <= level
			}
		}
		x := (payoff(sp, tp) + payoff(sm, tm)) * 0.5
		sum += x
		sumSq += x * x
	}
	return mcAggregate(op, cfg, sum, sumSq, pairs)
}

// mcAggregate discounts the antithetic-pair mean and computes the
// standard error — the fail-closed gate rejects non-finite or
// negative-variance aggregates as ErrNonConvergence.
func mcAggregate(op string, cfg MCConfig, sum, sumSq float64, pairs int) (MCResult, error) {
	n := float64(pairs)
	mean := sum / n
	variance := sumSq/n - mean*mean
	if variance < 0 {
		variance = 0 // float jitter on constant payoffs
	}
	stdErr := math.Sqrt(variance / n)
	value := mean // payoff functions already discount (DFd applied there)
	res := MCResult{Value: value, StdErr: stdErr, Paths: cfg.Paths, Steps: cfg.Steps, Seed: cfg.Seed}
	if !finite(res.Value) || !finite(res.StdErr) {
		return MCResult{}, nonConvergence(op, "non-finite Monte Carlo aggregate — refusing to emit")
	}
	return res, nil
}

// PriceVanilla prices a European vanilla option on the terminal-only
// engine — the convergence control for the closed form (§24 #324 tests
// compare this against VanillaOption.Price).
func (p *MCPricer) PriceVanilla(o VanillaOption, m Market) (MCResult, error) {
	const op = "MCPricer.PriceVanilla"
	if err := o.validate(op); err != nil {
		return MCResult{}, err
	}
	if m.T == 0 {
		v, err := checkResult(op, o.ExpiryPayoff(m.Spot))
		return MCResult{Value: v, Steps: 1, Seed: p.cfg.withDefaults().Seed}, err
	}
	return p.terminal(m, func(sT float64) float64 {
		return m.DFd * o.ExpiryPayoff(sT)
	})
}

// PriceBarrier prices a knock-in/knock-out option on the path-dependent
// engine (Task 22.3.5 step 5 — MC per spec §15.2). The payoff applies
// the knock rule: knock-out touched → 0; knock-in untouched → 0.
func (p *MCPricer) PriceBarrier(o BarrierOption, m Market) (MCResult, error) {
	const op = "MCPricer.PriceBarrier"
	if err := o.validate(op); err != nil {
		return MCResult{}, err
	}
	if m.T == 0 {
		// Expired: the barrier state is whatever the monitor recorded;
		// pricing is the residual intrinsic — an unmonitored expiry takes
		// the untouched branch (pessimistic zero for knock-in).
		v, err := checkResult(op, o.ExpiryPayoff(m.Spot, false))
		return MCResult{Value: v, Steps: p.cfg.withDefaults().Steps, Seed: p.cfg.withDefaults().Seed}, err
	}
	return p.pathDependent(m, o.Type, o.Level, func(sT float64, touched bool) float64 {
		return m.DFd * o.ExpiryPayoff(sT, touched)
	})
}

// PriceBinary prices a digital on the terminal-only engine — European
// binaries pay on S_T vs K, no path dependence (Task 22.3.6 step 3).
func (p *MCPricer) PriceBinary(o BinaryOption, m Market) (MCResult, error) {
	const op = "MCPricer.PriceBinary"
	if err := o.validate(op); err != nil {
		return MCResult{}, err
	}
	if m.T == 0 {
		v, err := checkResult(op, o.ExpiryPayoff(m.Spot))
		return MCResult{Value: v, Steps: 1, Seed: p.cfg.withDefaults().Seed}, err
	}
	return p.terminal(m, func(sT float64) float64 {
		return m.DFd * o.ExpiryPayoff(sT)
	})
}
