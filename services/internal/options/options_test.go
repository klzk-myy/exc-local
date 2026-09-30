package options

import (
	stderrors "errors"
	"math"
	"testing"
	"time"

	"exchange/internal/errs"
	"exchange/internal/oracle/rates"
	"exchange/pkg/decimal"
	pkgerrors "exchange/pkg/errors"
)

// mkt builds a Market from simple continuously-compounded rates —
// DFd = e^{-rd·T}, DFf = e^{-rf·T} — the test-side counterpart of
// rates.Curve.DiscountFactor.
func mkt(spot, rd, rf, vol, t float64) Market {
	return Market{
		Spot: spot,
		DFd:  math.Exp(-rd * t),
		DFf:  math.Exp(-rf * t),
		Vol:  vol,
		T:    t,
	}
}

func mustPrice(t *testing.T, o VanillaOption, m Market) float64 {
	t.Helper()
	p, err := o.Price(m)
	if err != nil {
		t.Fatalf("Price: %v", err)
	}
	return p
}

// --- Vanilla: closed form -------------------------------------------------

// TestVanillaKnownValue pins the canonical textbook case:
// S=1.0, K=1.0, rd=5%, rf=2%, σ=20%, T=1y → F=e^{0.03}, d1=0.25, d2=0.05
// → call ≈ 0.092270 (quote-ccy per unit base).
func TestVanillaKnownValue(t *testing.T) {
	m := mkt(1.0, 0.05, 0.02, 0.20, 1.0)
	call := VanillaOption{Right: OptionCall, Strike: 1.0, Style: ExerciseEuropean}
	p := mustPrice(t, call, m)
	if math.Abs(p-0.092270) > 1e-4 {
		t.Fatalf("GK call = %v, want ≈0.092270", p)
	}
}

// TestVanillaATMBounds: call premium is bounded by the discounted forward
// intrinsic below and the discounted foreign spot above; symmetric for put.
func TestVanillaATMBounds(t *testing.T) {
	m := mkt(1.10, 0.03, 0.01, 0.10, 0.5)
	c := VanillaOption{Right: OptionCall, Strike: 1.10, Style: ExerciseEuropean}
	p := VanillaOption{Right: OptionPut, Strike: 1.10, Style: ExerciseEuropean}
	pc, pp := mustPrice(t, c, m), mustPrice(t, p, m)
	floorC, capC := m.DFd*math.Max(m.Forward()-1.10, 0), m.Spot*m.DFf
	floorP, capP := m.DFd*math.Max(1.10-m.Forward(), 0), 1.10*m.DFd
	if !(floorC <= pc && pc <= capC) {
		t.Fatalf("call %v outside [%v, %v]", pc, floorC, capC)
	}
	if !(floorP <= pp && pp <= capP) {
		t.Fatalf("put %v outside [%v, %v]", pp, floorP, capP)
	}
	if pc <= 0 || pp <= 0 {
		t.Fatalf("ATM options must price positive, got call=%v put=%v", pc, pp)
	}
}

// TestPutCallParity: C − P = DFd·(F − K) must hold essentially exactly —
// both legs share the same gkCore evaluation.
func TestPutCallParity(t *testing.T) {
	for _, k := range []float64{1.00, 1.10, 1.25} {
		m := mkt(1.10, 0.04, 0.015, 0.12, 0.75)
		c := VanillaOption{Right: OptionCall, Strike: k, Style: ExerciseEuropean}
		p := VanillaOption{Right: OptionPut, Strike: k, Style: ExerciseEuropean}
		pc, pp := mustPrice(t, c, m), mustPrice(t, p, m)
		rhs, err := PutCallParityForward(m, k)
		if err != nil {
			t.Fatalf("parity rhs: %v", err)
		}
		if math.Abs((pc-pp)-rhs) > 1e-12 {
			t.Fatalf("parity breach at K=%v: C−P=%v rhs=%v", k, pc-pp, rhs)
		}
	}
}

// TestVanillaMonotonicity: call ↑ in spot and vol, put ↓ in spot.
func TestVanillaMonotonicity(t *testing.T) {
	base := mkt(1.10, 0.03, 0.01, 0.10, 0.5)
	call := VanillaOption{Right: OptionCall, Strike: 1.12, Style: ExerciseEuropean}
	put := VanillaOption{Right: OptionPut, Strike: 1.12, Style: ExerciseEuropean}

	c0 := mustPrice(t, call, base)
	up := base
	up.Spot = 1.15
	if mustPrice(t, call, up) <= c0 {
		t.Fatal("call must increase in spot")
	}
	if mustPrice(t, put, up) >= mustPrice(t, put, base) {
		t.Fatal("put must decrease in spot")
	}
	vup := base
	vup.Vol = 0.20
	if mustPrice(t, call, vup) <= c0 || mustPrice(t, put, vup) <= mustPrice(t, put, base) {
		t.Fatal("both rights must increase in vol")
	}
}

// TestGreeksSanity — delta bounds, Δc−Δp = DFf, positive gamma/vega,
// deep-ITM call delta → DFf.
func TestGreeksSanity(t *testing.T) {
	m := mkt(1.10, 0.03, 0.01, 0.10, 0.5)
	call := VanillaOption{Right: OptionCall, Strike: 1.10, Style: ExerciseEuropean}
	put := VanillaOption{Right: OptionPut, Strike: 1.10, Style: ExerciseEuropean}
	gc, err := call.Greeks(m)
	if err != nil {
		t.Fatalf("greeks: %v", err)
	}
	gp, err := put.Greeks(m)
	if err != nil {
		t.Fatalf("greeks: %v", err)
	}
	if gc.Delta <= 0 || gc.Delta >= m.DFf {
		t.Fatalf("call delta %v outside (0, %v)", gc.Delta, m.DFf)
	}
	if gp.Delta >= 0 || gp.Delta <= -m.DFf {
		t.Fatalf("put delta %v outside (−%v, 0)", gp.Delta, m.DFf)
	}
	if math.Abs((gc.Delta-gp.Delta)-m.DFf) > 1e-12 {
		t.Fatalf("delta spread %v != DFf %v", gc.Delta-gp.Delta, m.DFf)
	}
	if gc.Gamma <= 0 || gc.Vega <= 0 {
		t.Fatalf("ATM gamma/vega must be positive, got %v / %v", gc.Gamma, gc.Vega)
	}
	if math.Abs(gc.Gamma-gp.Gamma) > 1e-15 || math.Abs(gc.Vega-gp.Vega) > 1e-15 {
		t.Fatal("call and put share gamma and vega")
	}
	// Deep ITM call → delta approaches DFf.
	deep := VanillaOption{Right: OptionCall, Strike: 0.50, Style: ExerciseEuropean}
	gd, err := deep.Greeks(m)
	if err != nil {
		t.Fatalf("greeks: %v", err)
	}
	if math.Abs(gd.Delta-m.DFf) > 1e-6 {
		t.Fatalf("deep ITM delta %v should ≈ DFf %v", gd.Delta, m.DFf)
	}
}

// TestVanillaExpired: T == 0 collapses to intrinsic, never an error.
func TestVanillaExpired(t *testing.T) {
	m := mkt(1.20, 0.03, 0.01, 0.10, 0)
	call := VanillaOption{Right: OptionCall, Strike: 1.10, Style: ExerciseEuropean}
	if p := mustPrice(t, call, m); math.Abs(p-0.10) > 1e-12 {
		t.Fatalf("expired call = %v, want 0.10", p)
	}
	put := VanillaOption{Right: OptionPut, Strike: 1.10, Style: ExerciseEuropean}
	if p := mustPrice(t, put, m); p != 0 {
		t.Fatalf("expired OTM put = %v, want 0", p)
	}
}

// TestVanillaRejectsAmerican — AMERICAN must not silently take the BSGK
// path (spec §15.2 lattice requirement; sibling-owned american.go).
func TestVanillaRejectsAmerican(t *testing.T) {
	o := VanillaOption{Right: OptionCall, Strike: 1.10, Style: ExerciseAmerican}
	_, err := o.Price(mkt(1.10, 0.03, 0.01, 0.10, 0.5))
	if err == nil {
		t.Fatal("American style must be refused")
	}
	if !stderrors.Is(err, ErrInvalidInput) {
		t.Fatalf("want ErrInvalidInput, got %v", err)
	}
	if got := pkgerrors.CodeOf(err); got != CodeOptionPricingInputInvalid {
		t.Fatalf("code = %s, want %s", got, CodeOptionPricingInputInvalid)
	}
}

// TestInvalidInputs — fail-closed input validation (spec §2.7).
func TestInvalidInputs(t *testing.T) {
	o := VanillaOption{Right: OptionCall, Strike: 1.10, Style: ExerciseEuropean}
	for name, m := range map[string]Market{
		"zero spot":  {Spot: 0, DFd: 1, DFf: 1, Vol: 0.1, T: 0.5},
		"neg spot":   {Spot: -1, DFd: 1, DFf: 1, Vol: 0.1, T: 0.5},
		"nan spot":   {Spot: math.NaN(), DFd: 1, DFf: 1, Vol: 0.1, T: 0.5},
		"inf vol":    {Spot: 1.1, DFd: 1, DFf: 1, Vol: math.Inf(1), T: 0.5},
		"zero vol":   {Spot: 1.1, DFd: 1, DFf: 1, Vol: 0, T: 0.5},
		"zero DF":    {Spot: 1.1, DFd: 0, DFf: 1, Vol: 0.1, T: 0.5},
		"negative T": {Spot: 1.1, DFd: 1, DFf: 1, Vol: 0.1, T: -1},
	} {
		if _, err := o.Price(m); !stderrors.Is(err, ErrInvalidInput) {
			t.Fatalf("%s: want ErrInvalidInput, got %v", name, err)
		}
	}
	for name, bad := range map[string]VanillaOption{
		"bad right":   {Right: "WUT", Strike: 1.1, Style: ExerciseEuropean},
		"zero strike": {Right: OptionCall, Strike: 0, Style: ExerciseEuropean},
		"nan strike":  {Right: OptionCall, Strike: math.NaN(), Style: ExerciseEuropean},
	} {
		if _, err := bad.Price(mkt(1.1, 0.03, 0.01, 0.1, 0.5)); !stderrors.Is(err, ErrInvalidInput) {
			t.Fatalf("%s: want ErrInvalidInput, got %v", name, err)
		}
	}
}

// TestNonConvergenceCode — the fail-closed gate emits
// OPTION_PRICING_CONVERGENCE_ERROR (§24 #324), never a bare NaN.
func TestNonConvergenceCode(t *testing.T) {
	_, err := checkResult("test", math.NaN())
	if err == nil {
		t.Fatal("NaN result must be refused")
	}
	if !stderrors.Is(err, ErrNonConvergence) {
		t.Fatalf("want ErrNonConvergence, got %v", err)
	}
	if got := pkgerrors.CodeOf(err); got != CodeOptionPricingConvergence {
		t.Fatalf("code = %s, want %s", got, CodeOptionPricingConvergence)
	}
}

// TestDeclaredCodesRegistered — every emitted code resolves in the §23
// registry (Task 5.3.21 startup gate semantics).
func TestDeclaredCodesRegistered(t *testing.T) {
	if err := errs.New().CheckRegistered(DeclaredCodes()...); err != nil {
		t.Fatalf("unregistered codes: %v", err)
	}
}

// --- Monte Carlo ----------------------------------------------------------

// TestMCVanillaConverges — §24 #324-adjacent numerical check: the MC
// engine must land on the closed form within statistical tolerance at
// ≥50k paths (prompt requirement); we run the spec-§15.2 100k floor.
func TestMCVanillaConverges(t *testing.T) {
	m := mkt(1.10, 0.03, 0.01, 0.10, 0.5)
	o := VanillaOption{Right: OptionCall, Strike: 1.12, Style: ExerciseEuropean}
	gk := mustPrice(t, o, m)
	res, err := NewMCPricer(MCConfig{Paths: 100_000, Steps: 1, Seed: 42}).PriceVanilla(o, m)
	if err != nil {
		t.Fatalf("MC: %v", err)
	}
	tol := math.Max(4*res.StdErr, 1e-4)
	if math.Abs(res.Value-gk) > tol {
		t.Fatalf("MC %v vs GK %v — |diff|=%v > tol %v (stderr %v)",
			res.Value, gk, math.Abs(res.Value-gk), tol, res.StdErr)
	}
}

// TestMCDeterministic — same seed ⇒ bit-identical price (replay/WAL
// determinism contract).
func TestMCDeterministic(t *testing.T) {
	m := mkt(1.10, 0.03, 0.01, 0.10, 0.5)
	o := VanillaOption{Right: OptionPut, Strike: 1.08, Style: ExerciseEuropean}
	p := NewMCPricer(MCConfig{Paths: 10_000, Steps: 1, Seed: 7})
	a, err := p.PriceVanilla(o, m)
	if err != nil {
		t.Fatalf("MC: %v", err)
	}
	b, err := p.PriceVanilla(o, m)
	if err != nil {
		t.Fatalf("MC: %v", err)
	}
	if a.Value != b.Value || a.StdErr != b.StdErr {
		t.Fatalf("non-deterministic: %v vs %v", a.Value, b.Value)
	}
	// Different seed ⇒ (almost surely) different value — guards against a
	// seed-independent stub.
	c, err := NewMCPricer(MCConfig{Paths: 10_000, Steps: 1, Seed: 8}).PriceVanilla(o, m)
	if err != nil {
		t.Fatalf("MC: %v", err)
	}
	if c.Value == a.Value {
		t.Fatal("distinct seeds produced identical prices — seed not wired")
	}
}

// TestMCConfigValidation — odd/zero/huge configs refuse (bounded-work
// guard is part of the §2.7 contract).
func TestMCConfigValidation(t *testing.T) {
	m := mkt(1.10, 0.03, 0.01, 0.10, 0.5)
	o := VanillaOption{Right: OptionCall, Strike: 1.10, Style: ExerciseEuropean}
	for name, cfg := range map[string]MCConfig{
		"odd paths":  {Paths: 101, Steps: 1, Seed: 1},
		"zero steps": {Paths: 100, Steps: 0, Seed: 1}, // zero → default, so this is actually fine
		"huge":       {Paths: 100_000_000, Steps: 1, Seed: 1},
	} {
		_, err := NewMCPricer(cfg).PriceVanilla(o, m)
		switch name {
		case "zero steps":
			if err != nil {
				t.Fatalf("%s: zero steps should take default, got %v", name, err)
			}
		default:
			if !stderrors.Is(err, ErrInvalidInput) {
				t.Fatalf("%s: want ErrInvalidInput, got %v", name, err)
			}
		}
	}
}

// --- Barrier --------------------------------------------------------------

// TestBarrierKOBoundedByVanilla — the knock-out can never exceed the
// vanilla price: pathwise dominance makes it exact on the path engine,
// and vs the closed form it must hold within MC sampling error (4σ).
func TestBarrierKOBoundedByVanilla(t *testing.T) {
	m := mkt(1.10, 0.03, 0.01, 0.10, 0.5)
	cfg := MCConfig{Paths: 50_000, Steps: 128, Seed: 99}
	p := NewMCPricer(cfg)
	gk := mustPrice(t, VanillaOption{Right: OptionCall, Strike: 1.10, Style: ExerciseEuropean}, m)
	for _, bt := range []BarrierType{BarrierUpAndOut, BarrierDownAndOut} {
		level := 1.20
		if bt == BarrierDownAndOut {
			level = 1.00
		}
		ko := BarrierOption{Right: OptionCall, Strike: 1.10, Style: ExerciseEuropean, Type: bt, Level: level}
		res, err := p.PriceBarrier(ko, m)
		if err != nil {
			t.Fatalf("%s MC: %v", bt, err)
		}
		tol := math.Max(4*res.StdErr, 1e-6)
		if res.Value > gk+tol {
			t.Fatalf("%s KO %v exceeds vanilla %v beyond 4σ (%v)", bt, res.Value, gk, tol)
		}
		if res.Value <= 0 {
			t.Fatalf("%s KO priced %v — expected positive", bt, res.Value)
		}
	}
}

// TestBarrierInOutParity — KI + KO on the same path engine and seed is
// the pathwise vanilla value to within float-association jitter: every
// simulated path's payoff splits exactly between the two contracts
// (per-path the identity is exact; the sums combine in different order
// across the three aggregates, so allow ~ulp tolerance — still 10⁵×
// tighter than MC noise). The reference is an untouchable KO (same
// engine, same seed, same paths) — NOT the terminal engine, whose
// draws differ.
func TestBarrierInOutParity(t *testing.T) {
	m := mkt(1.10, 0.03, 0.01, 0.10, 0.5)
	cfg := MCConfig{Paths: 50_000, Steps: 128, Seed: 7}
	p := NewMCPricer(cfg)
	ki := BarrierOption{Right: OptionCall, Strike: 1.10, Style: ExerciseEuropean, Type: BarrierUpAndIn, Level: 1.20}
	ko := BarrierOption{Right: OptionCall, Strike: 1.10, Style: ExerciseEuropean, Type: BarrierUpAndOut, Level: 1.20}
	// Level 3.0 is ~50σ away: never touched, so this KO equals the
	// pathwise vanilla sum on the identical path set.
	far := BarrierOption{Right: OptionCall, Strike: 1.10, Style: ExerciseEuropean, Type: BarrierUpAndOut, Level: 3.0}
	ri, err := p.PriceBarrier(ki, m)
	if err != nil {
		t.Fatalf("KI: %v", err)
	}
	ro, err := p.PriceBarrier(ko, m)
	if err != nil {
		t.Fatalf("KO: %v", err)
	}
	rf, err := p.PriceBarrier(far, m)
	if err != nil {
		t.Fatalf("far KO: %v", err)
	}
	if d := math.Abs(ri.Value + ro.Value - rf.Value); d > 1e-12 {
		t.Fatalf("in-out parity breach: %v + %v != pathwise vanilla %v (diff %v)", ri.Value, ro.Value, rf.Value, d)
	}
}

// TestBarrierFarAway — a KO barrier far outside the vol cone must
// recover the closed-form vanilla within MC sampling error (touch
// probability ~0 → pathwise payoff identical to vanilla).
func TestBarrierFarAway(t *testing.T) {
	m := mkt(1.10, 0.03, 0.01, 0.10, 0.5)
	cfg := MCConfig{Paths: 50_000, Steps: 128, Seed: 5}
	p := NewMCPricer(cfg)
	ko := BarrierOption{Right: OptionCall, Strike: 1.10, Style: ExerciseEuropean, Type: BarrierUpAndOut, Level: 3.0}
	res, err := p.PriceBarrier(ko, m)
	if err != nil {
		t.Fatalf("KO: %v", err)
	}
	gk := mustPrice(t, VanillaOption{Right: OptionCall, Strike: 1.10, Style: ExerciseEuropean}, m)
	if tol := math.Max(4*res.StdErr, 1e-4); math.Abs(res.Value-gk) > tol {
		t.Fatalf("far KO %v vs GK vanilla %v — |diff| exceeds %v", res.Value, gk, tol)
	}
}

// TestBarrierAlreadyKnocked — spot beyond an up barrier knocks at t=0:
// KO worth 0, KI worth vanilla.
func TestBarrierAlreadyKnocked(t *testing.T) {
	m := mkt(1.30, 0.03, 0.01, 0.10, 0.5)
	cfg := MCConfig{Paths: 10_000, Steps: 16, Seed: 3}
	p := NewMCPricer(cfg)
	ko := BarrierOption{Right: OptionCall, Strike: 1.10, Style: ExerciseEuropean, Type: BarrierUpAndOut, Level: 1.20}
	res, err := p.PriceBarrier(ko, m)
	if err != nil {
		t.Fatalf("KO: %v", err)
	}
	if res.Value != 0 {
		t.Fatalf("knocked-at-inception KO = %v, want 0", res.Value)
	}
}

// --- BarrierMonitor (the tick-path monitoring contract) -------------------

func TestMonitorKnockUpOut(t *testing.T) {
	mon, err := NewBarrierMonitor(BarrierUpAndOut, 1.20)
	if err != nil {
		t.Fatalf("monitor: %v", err)
	}
	base := time.Date(2026, 10, 12, 12, 0, 0, 0, time.UTC)
	for i, px := range []float64{1.10, 1.15, 1.21, 1.18} {
		at := base.Add(time.Duration(i) * time.Second)
		ev, err := mon.Observe(MarkTick{Price: px, At: at})
		if err != nil {
			t.Fatalf("tick %d: %v", i, err)
		}
		if i < 2 && ev.Knocked {
			t.Fatalf("tick %d knocked early at %v", i, px)
		}
		if i == 2 && !ev.Knocked {
			t.Fatalf("tick %d should knock at %v", i, px)
		}
		if i == 3 && ev.Knocked {
			t.Fatal("knock must fire exactly once")
		}
	}
	if !mon.Knocked() || mon.Alive() {
		t.Fatal("UP_AND_OUT should be knocked and dead")
	}
	px, at, ok := mon.KnockDetail()
	if !ok || px != 1.21 || at.IsZero() {
		t.Fatalf("knock detail = %v %v %v", px, at, ok)
	}
}

func TestMonitorKnockDownIn(t *testing.T) {
	mon, err := NewBarrierMonitor(BarrierDownAndIn, 1.05)
	if err != nil {
		t.Fatalf("monitor: %v", err)
	}
	base := time.Date(2026, 10, 12, 12, 0, 0, 0, time.UTC)
	ev, err := mon.Observe(MarkTick{Price: 1.04, At: base})
	if err != nil {
		t.Fatalf("tick: %v", err)
	}
	if !ev.Knocked || !mon.Knocked() || !mon.Alive() {
		t.Fatal("DOWN_AND_IN should knock in and activate")
	}
	if ev.Reason != BarrierReasonKnockIn {
		t.Fatalf("reason = %q, want %q", ev.Reason, BarrierReasonKnockIn)
	}
	rec := ev.Record(42, 7)
	if rec == nil || rec.Event != "KNOCK_IN" || rec.OrderID != 42 {
		t.Fatalf("record = %+v", rec)
	}
}

// TestMonitorStaleNeverFabricates — spec §15.7: a mark outside the 5s
// staleness gate (producer flags it via MarkStale → MarkTick.Stale)
// produces no evaluation and no knock, even beyond the barrier.
func TestMonitorStaleNeverFabricates(t *testing.T) {
	mon, _ := NewBarrierMonitor(BarrierUpAndOut, 1.20)
	base := time.Date(2026, 10, 12, 12, 0, 0, 0, time.UTC)
	stale := MarkTick{Price: 9.99, At: base.Add(-10 * time.Second)}
	stale.Stale = MarkStale(base, stale.At) // producer evaluates the gate
	ev, err := mon.Observe(stale)
	if err != nil {
		t.Fatalf("stale tick must not error, got %v", err)
	}
	if ev.Evaluated || ev.Knocked || mon.Knocked() || mon.TicksSeen() != 0 {
		t.Fatal("stale tick must be a complete no-op")
	}
	if ev.Reason != BarrierReasonStale {
		t.Fatalf("reason = %q, want %q", ev.Reason, BarrierReasonStale)
	}
	if mon.StaleTicksSeen() != 1 {
		t.Fatalf("stale audit counter = %d, want 1", mon.StaleTicksSeen())
	}
	// The gate is symmetric: a future-dated mark is equally suspect.
	future := MarkTick{Price: 9.99, At: base.Add(10 * time.Second)}
	future.Stale = MarkStale(base, future.At)
	ev, err = mon.Observe(future)
	if err != nil || ev.Evaluated || ev.Knocked {
		t.Fatalf("future-dated stale tick must skip: %+v err=%v", ev, err)
	}
	// EvaluateBarrierTick — the standalone contract — applies the gate
	// itself and returns the coded stale error.
	_, err = EvaluateBarrierTick(base, MarkTick{Price: 1.25, At: base.Add(-10 * time.Second)},
		BarrierKnockOutUp, 1.20)
	if pkgerrors.CodeOf(err) != CodeConditionalTriggerOracleStale {
		t.Fatalf("eval code = %v, want %s", err, CodeConditionalTriggerOracleStale)
	}
}

// TestMonitorGapFlag — first mark after a >GapThreshold silence (or one
// declared PostGap by the caller) carries the weekend-gap flag
// (spec §15.7 item 3).
func TestMonitorGapFlag(t *testing.T) {
	mon, _ := NewBarrierMonitor(BarrierUpAndIn, 1.20)
	fri := time.Date(2026, 10, 9, 22, 0, 0, 0, time.UTC)
	if _, err := mon.Observe(MarkTick{Price: 1.10, At: fri}); err != nil {
		t.Fatalf("tick: %v", err)
	}
	sun := fri.Add(71 * time.Hour) // weekend silence — inferred gap
	ev, err := mon.Observe(MarkTick{Price: 1.25, At: sun})
	if err != nil {
		t.Fatalf("post-gap tick: %v", err)
	}
	if !ev.Gap || !ev.Knocked {
		t.Fatalf("post-gap tick must flag gap and evaluate the touch: %+v", ev)
	}
	// Caller-declared gap flag also marks the event.
	mon2, _ := NewBarrierMonitor(BarrierUpAndOut, 1.20)
	ev2, err := mon2.Observe(MarkTick{Price: 1.25, At: base0(), PostGap: true})
	if err != nil || !ev2.Gap || !ev2.Knocked {
		t.Fatalf("declared PostGap must flag+knock: %+v err=%v", ev2, err)
	}
}

func base0() time.Time { return time.Date(2026, 10, 12, 12, 0, 0, 0, time.UTC) }

// TestMonitorOutOfOrder — a backward timestamp is a sequencing violation
// and fails closed.
func TestMonitorOutOfOrder(t *testing.T) {
	mon, _ := NewBarrierMonitor(BarrierUpAndOut, 1.20)
	base := time.Date(2026, 10, 12, 12, 0, 0, 0, time.UTC)
	if _, err := mon.Observe(MarkTick{Price: 1.10, At: base}); err != nil {
		t.Fatalf("tick: %v", err)
	}
	_, err := mon.Observe(MarkTick{Price: 1.11, At: base.Add(-time.Second)})
	if err == nil || !stderrors.Is(err, ErrInvalidInput) {
		t.Fatalf("out-of-order tick must fail closed, got %v", err)
	}
	if got := pkgerrors.CodeOf(err); got != CodeOptionPricingInputInvalid {
		t.Fatalf("code = %s, want %s", got, CodeOptionPricingInputInvalid)
	}
}

func TestMonitorBadTicks(t *testing.T) {
	mon, _ := NewBarrierMonitor(BarrierUpAndOut, 1.20)
	base := time.Date(2026, 10, 12, 12, 0, 0, 0, time.UTC)
	for _, px := range []float64{0, -1, math.NaN(), math.Inf(1)} {
		if _, err := mon.Observe(MarkTick{Price: px, At: base}); !stderrors.Is(err, ErrInvalidInput) {
			t.Fatalf("price %v: want ErrInvalidInput, got %v", px, err)
		}
	}
}

// TestBarrierTypeParse — canonical §5.4 strings + plan shorthand aliases.
func TestBarrierTypeParse(t *testing.T) {
	for s, want := range map[string]BarrierType{
		"UP_AND_IN": BarrierUpAndIn, "UP_IN": BarrierUpAndIn,
		"UP_AND_OUT": BarrierUpAndOut, "UP_OUT": BarrierUpAndOut,
		"DOWN_AND_IN": BarrierDownAndIn, "DOWN_IN": BarrierDownAndIn,
		"DOWN_AND_OUT": BarrierDownAndOut, "DOWN_OUT": BarrierDownAndOut,
	} {
		got, err := ParseBarrierType(s)
		if err != nil || got != want {
			t.Fatalf("ParseBarrierType(%q) = %v, %v; want %v", s, got, err, want)
		}
	}
	if _, err := ParseBarrierType("SIDEWAYS"); !stderrors.Is(err, ErrInvalidInput) {
		t.Fatalf("unknown type must fail closed, got %v", err)
	}
}

// --- Binary ---------------------------------------------------------------

func TestBinaryClosedFormBounds(t *testing.T) {
	m := mkt(1.10, 0.03, 0.01, 0.10, 0.5)
	cash := BinaryOption{Kind: BinaryCashOrNothing, Right: OptionCall, Strike: 1.12, Payout: 1.0}
	asset := BinaryOption{Kind: BinaryAssetOrNothing, Right: OptionCall, Strike: 1.12}
	pc, err := cash.PriceClosedForm(m)
	if err != nil {
		t.Fatalf("cash: %v", err)
	}
	pa, err := asset.PriceClosedForm(m)
	if err != nil {
		t.Fatalf("asset: %v", err)
	}
	if pc <= 0 || pc > m.DFd*1.0 {
		t.Fatalf("cash-or-nothing %v outside (0, %v]", pc, m.DFd)
	}
	if pa <= 0 || pa > m.Spot*m.DFf {
		t.Fatalf("asset-or-nothing %v outside (0, %v]", pa, m.Spot*m.DFf)
	}
	// Asset-or-nothing ≥ cash-or-nothing for OTM strike > 1 payout=1
	// when S_T > K > payout — always true in payoff terms.
	if pa <= pc {
		t.Fatalf("asset %v should exceed cash %v at strike 1.12 > payout 1.0", pa, pc)
	}
}

// TestBinaryMCConverges — MC vs the GK digital closed form at 100k paths.
func TestBinaryMCConverges(t *testing.T) {
	m := mkt(1.10, 0.03, 0.01, 0.10, 0.5)
	for _, o := range []BinaryOption{
		{Kind: BinaryCashOrNothing, Right: OptionCall, Strike: 1.12, Payout: 1.0},
		{Kind: BinaryAssetOrNothing, Right: OptionCall, Strike: 1.12},
		{Kind: BinaryCashOrNothing, Right: OptionPut, Strike: 1.08, Payout: 1.0},
	} {
		cf, err := o.PriceClosedForm(m)
		if err != nil {
			t.Fatalf("closed form: %v", err)
		}
		res, err := NewMCPricer(MCConfig{Paths: 100_000, Steps: 1, Seed: 11}).PriceBinary(o, m)
		if err != nil {
			t.Fatalf("MC: %v", err)
		}
		tol := math.Max(4*res.StdErr, 1e-4)
		if math.Abs(res.Value-cf) > tol {
			t.Fatalf("%s %s: MC %v vs closed-form %v (tol %v)",
				o.Kind, o.Right, res.Value, cf, tol)
		}
	}
}

func TestBinaryExpiryPayoff(t *testing.T) {
	o := BinaryOption{Kind: BinaryCashOrNothing, Right: OptionCall, Strike: 1.10, Payout: 0.50}
	if got := o.ExpiryPayoff(1.15); got != 0.50 {
		t.Fatalf("ITM binary payoff = %v, want 0.50", got)
	}
	if got := o.ExpiryPayoff(1.10); got != 0 {
		t.Fatal("exact-strike fix pays nothing (strict convention)")
	}
	a := BinaryOption{Kind: BinaryAssetOrNothing, Right: OptionPut, Strike: 1.10}
	if got := a.ExpiryPayoff(1.05); got != 1.05 {
		t.Fatalf("asset-or-nothing put pays sT, got %v", got)
	}
}

func TestBinaryInvalid(t *testing.T) {
	for name, o := range map[string]BinaryOption{
		"bad kind":       {Kind: "LADDER", Right: OptionCall, Strike: 1.1},
		"missing payout": {Kind: BinaryCashOrNothing, Right: OptionCall, Strike: 1.1, Payout: 0},
		"bad right":      {Kind: BinaryCashOrNothing, Right: "SPREAD", Strike: 1.1, Payout: 1},
	} {
		if _, err := o.PriceClosedForm(mkt(1.1, 0.03, 0.01, 0.1, 0.5)); !stderrors.Is(err, ErrInvalidInput) {
			t.Fatalf("%s: want ErrInvalidInput, got %v", name, err)
		}
	}
}

// --- Curve adapter --------------------------------------------------------

func curve(ccy string, flat float64) rates.Curve {
	rs := map[rates.Tenor]decimal.Decimal{}
	for _, tn := range rates.CanonicalTenors {
		rs[tn] = decimal.NewFromFloat(flat)
	}
	return rates.Curve{Currency: ccy, Rates: rs, AsOf: time.Now()}
}

func TestMarketFromCurves(t *testing.T) {
	m, err := MarketFromCurves(1.10, 0.10, 90, curve("EUR", 0.02), curve("USD", 0.05))
	if err != nil {
		t.Fatalf("MarketFromCurves: %v", err)
	}
	if m.T != 90.0/365.0 {
		t.Fatalf("T = %v, want 90/365", m.T)
	}
	// Flat 5% USD curve: DFd ≈ exp(−0.05·90/360) under ACT/360.
	wantDF := math.Exp(-0.05 * 90.0 / 360.0)
	if math.Abs(m.DFd-wantDF) > 1e-9 {
		t.Fatalf("DFd = %v, want ≈%v (ACT/360 USD)", m.DFd, wantDF)
	}
	if m.Forward() <= m.Spot {
		t.Fatal("quote rate > base rate ⇒ forward must exceed spot")
	}
}

func TestMarketFromCurvesFailClosed(t *testing.T) {
	stale := curve("EUR", 0.02)
	stale.Stale = true
	if _, err := MarketFromCurves(1.10, 0.10, 90, stale, curve("USD", 0.05)); !stderrors.Is(err, ErrCurveUnavailable) {
		t.Fatalf("stale curve: want ErrCurveUnavailable, got %v", err)
	}
	partial := rates.Curve{Currency: "EUR", Rates: map[rates.Tenor]decimal.Decimal{rates.Tenor1M: decimal.NewFromFloat(0.02)}}
	if _, err := MarketFromCurves(1.10, 0.10, 90, partial, curve("USD", 0.05)); !stderrors.Is(err, ErrCurveUnavailable) {
		t.Fatalf("incomplete curve: want ErrCurveUnavailable, got %v", err)
	}
	if _, err := MarketFromCurves(1.10, 0.10, 0, curve("EUR", 0.02), curve("USD", 0.05)); !stderrors.Is(err, ErrInvalidInput) {
		t.Fatalf("zero days: want ErrInvalidInput, got %v", err)
	}
}
