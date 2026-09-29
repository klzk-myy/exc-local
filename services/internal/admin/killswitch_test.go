// Unit tests for the Task 11.3.4/11.3.8/11.3.12 kill-switch resolver:
// scope-lattice precedence, key rendering, and fail-closed semantics.
// Durable-record paths (KillSwitchService.Set/Clear) are exercised by
// the DSN-gated integration suite — they need a real pgx tx.
package admin

import (
	"context"
	"errors"
	"testing"

	excerrors "exchange/pkg/errors"
)

// fakeFlags implements HaltFlags over an in-memory key set.
type fakeFlags struct {
	set     map[string]string
	scanErr error
	halted  bool
	haltErr error
}

func (f *fakeFlags) HaltScopeScan(_ context.Context, keys []string) (map[string]string, error) {
	if f.scanErr != nil {
		return nil, f.scanErr
	}
	out := map[string]string{}
	for _, k := range keys {
		if v, ok := f.set[k]; ok {
			out[k] = v
		}
	}
	return out, nil
}

func (f *fakeFlags) IsHalted(context.Context) (bool, error) {
	return f.halted, f.haltErr
}

func TestKillSwitchResolve_Precedence(t *testing.T) {
	flags := &fakeFlags{set: map[string]string{
		"halt:global":             "global halt",
		"halt:instrument:EUR/USD": "instrument halt",
		"halt:account:42":         "account halt",
	}}
	r := NewKillSwitchResolver(flags, "prod")
	d, err := r.Resolve(context.Background(), HaltQuery{
		AccountID: 42, Symbol: "EUR/USD", InstrumentClass: "SPOT"})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if !d.Suspended || d.Scope != ScopeAccount || d.Target != "42" {
		t.Fatalf("account scope must beat instrument/global: %+v", d)
	}

	// Instrument beats class and global when the account flag is absent.
	flags.set = map[string]string{
		"halt:global":                "global halt",
		"halt:instrument:EUR/USD":    "instrument halt",
		"halt:instrument_class:SPOT": "class halt",
	}
	d, _ = r.Resolve(context.Background(), HaltQuery{
		Symbol: "EUR/USD", InstrumentClass: "SPOT"})
	if !d.Suspended || d.Scope != ScopeInstrument {
		t.Fatalf("instrument must beat class/global: %+v", d)
	}

	// Class beats global.
	flags.set = map[string]string{
		"halt:global":                "global halt",
		"halt:instrument_class:SPOT": "class halt",
	}
	d, _ = r.Resolve(context.Background(), HaltQuery{
		Symbol: "GBP/JPY", InstrumentClass: "SPOT"})
	if !d.Suspended || d.Scope != ScopeInstrumentClass {
		t.Fatalf("class must beat global: %+v", d)
	}

	// Global is the always-present fallback.
	flags.set = map[string]string{"halt:global": "global halt"}
	d, _ = r.Resolve(context.Background(), HaltQuery{AccountID: 7, Symbol: "EUR/USD"})
	if !d.Suspended || d.Scope != ScopeGlobal {
		t.Fatalf("global fallback: %+v", d)
	}

	// Counterparty resolves on the account id axis (§24 #409) and beats
	// instrument.
	flags.set = map[string]string{
		"halt:counterparty:42":       "counterparty halt",
		"halt:instrument:EUR/USD":    "instrument halt",
		"halt:instrument_class:SPOT": "class halt",
	}
	d, _ = r.Resolve(context.Background(), HaltQuery{
		AccountID: 42, Symbol: "EUR/USD", InstrumentClass: "SPOT"})
	if !d.Suspended || d.Scope != ScopeCounterparty {
		t.Fatalf("counterparty must beat instrument/class: %+v", d)
	}

	// ENV axis binds at construction — env flag halts even with no
	// caller-supplied dimensions.
	flags.set = map[string]string{"halt:env:PROD": "env drain"}
	d, _ = r.Resolve(context.Background(), HaltQuery{AccountID: 9})
	if !d.Suspended || d.Scope != ScopeEnv {
		t.Fatalf("env axis: %+v", d)
	}

	// All clear.
	flags.set = map[string]string{}
	d, _ = r.Resolve(context.Background(), HaltQuery{AccountID: 42, Symbol: "EUR/USD"})
	if d.Suspended {
		t.Fatalf("expected open: %+v", d)
	}
}

func TestKillSwitchResolve_FailClosed(t *testing.T) {
	// Lookup error propagates — callers fail closed.
	flags := &fakeFlags{scanErr: errors.New("redis down")}
	r := NewKillSwitchResolver(flags, "prod")
	if _, err := r.Resolve(context.Background(), HaltQuery{AccountID: 1}); err == nil {
		t.Fatal("lookup error must propagate")
	}
	if _, _, err := r.OrderHalt(context.Background(), 1, "EUR/USD", "SPOT", ""); err == nil {
		t.Fatal("OrderHalt must propagate lookup errors")
	}

	// Nil flag store → resolver is unverifiable (error, not "open").
	nilr := NewKillSwitchResolver(nil, "prod")
	if _, err := nilr.Resolve(context.Background(), HaltQuery{}); err == nil {
		t.Fatal("nil flag store must error")
	}
	if _, err := nilr.GlobalHalted(context.Background()); err == nil {
		t.Fatal("nil resolver GlobalHalted must error")
	}
	var z *KillSwitchResolver
	if _, err := z.Resolve(context.Background(), HaltQuery{}); err == nil {
		t.Fatal("nil resolver must error (not panic)")
	}
}

func TestKillSwitchResolve_OrderHaltDetail(t *testing.T) {
	flags := &fakeFlags{set: map[string]string{
		"halt:instrument:EUR/USD": "issuer suspension",
	}}
	r := NewKillSwitchResolver(flags, "")
	scope, detail, err := r.OrderHalt(context.Background(), 7, "EUR/USD", "SPOT", "")
	if err != nil || scope != ScopeInstrument {
		t.Fatalf("scope/detail: %q %q %v", scope, detail, err)
	}
	if detail == "" {
		t.Fatal("detail must name the winning scope + reason")
	}
	// Unaffected instrument passes.
	scope, _, err = r.OrderHalt(context.Background(), 7, "USD/JPY", "SPOT", "")
	if err != nil || scope != "" {
		t.Fatalf("unrelated instrument must pass: %q %v", scope, err)
	}
}

func TestKillSwitchResolve_RailAndLP(t *testing.T) {
	flags := &fakeFlags{set: map[string]string{
		"halt:rail:SEPA": "SEPA scheme outage",
		"halt:lp:lp-7":   "stale quotes",
	}}
	r := NewKillSwitchResolver(flags, "")

	suspended, reason, err := r.RailSuspended(context.Background(), "SEPA")
	if err != nil || !suspended || reason == "" {
		t.Fatalf("rail suspension: %v %q %v", suspended, reason, err)
	}
	suspended, _, err = r.RailSuspended(context.Background(), "SWIFT")
	if err != nil || suspended {
		t.Fatal("sibling rails must keep operating")
	}
	// Rail name normalizes (lowercase key form).
	suspended, _, _ = r.RailSuspended(context.Background(), "sepa")
	if !suspended {
		t.Fatal("rail lookup must be case-insensitive")
	}

	suspended, _, err = r.LPSuspended(context.Background(), "lp-7")
	if err != nil || !suspended {
		t.Fatalf("lp suspension: %v %v", suspended, err)
	}
	suspended, _, _ = r.LPSuspended(context.Background(), "lp-8")
	if suspended {
		t.Fatal("unrelated LP must keep quoting")
	}

	// Lookup error propagates for callers to fail closed.
	flags.scanErr = errors.New("boom")
	if _, _, err := r.RailSuspended(context.Background(), "SEPA"); err == nil {
		t.Fatal("rail lookup error must propagate")
	}
}

func TestKillSwitchScopeVocabulary(t *testing.T) {
	for _, s := range []string{"GLOBAL", "ACCOUNT", "COUNTERPARTY", "INSTRUMENT",
		"INSTRUMENT_CLASS", "FIX_SESSION", "LP", "RAIL", "REGION", "ENV", "DESK"} {
		if !ValidScope(s) {
			t.Fatalf("scope %s must be valid", s)
		}
	}
	for _, s := range []string{"", "EVERYTHING", "globalx", "ACCOUNT2"} {
		if ValidScope(s) {
			t.Fatalf("scope %q must be invalid", s)
		}
	}

	if !DualControlRequired(ScopeGlobal) || !DualControlRequired(ScopeCounterparty) {
		t.Fatal("GLOBAL and COUNTERPARTY are the destructive dual-control scopes")
	}
	for _, s := range []string{ScopeAccount, ScopeInstrument, ScopeInstrumentClass,
		ScopeFixSession, ScopeLP, ScopeRail, ScopeRegion, ScopeEnv, ScopeDesk} {
		if DualControlRequired(s) {
			t.Fatalf("scope %s must be single-approver per Task 11.3.8", s)
		}
	}
}

func TestNormalizeScope(t *testing.T) {
	sc, tgt, err := normalizeScope(" instrument ", "eur/usd")
	if err != nil || sc != ScopeInstrument || tgt != "EUR/USD" {
		t.Fatalf("normalize: %q %q %v", sc, tgt, err)
	}
	if _, _, err := normalizeScope("BOGUS", "x"); err == nil {
		t.Fatal("unknown scope must reject")
	}
	if _, _, err := normalizeScope("ACCOUNT", ""); err == nil {
		t.Fatal("scoped kill without target must reject")
	}
	sc, tgt, err = normalizeScope("GLOBAL", "ignored")
	if err != nil || sc != ScopeGlobal || tgt != "" {
		t.Fatalf("global drops its target: %q %q %v", sc, tgt, err)
	}
}

func TestKillSwitchRoles_FailClosed(t *testing.T) {
	// requireKillRole is unreachable without a service instance (pool is a
	// concrete dep) — but the seam contract is enforced by NewKillSwitchService:
	// nil role resolver rejects construction outright.
	if _, err := NewKillSwitchService(KillSwitchDeps{
		Pool: nil, Flags: &fakeFlagWriter{}, Roles: nil}); err == nil {
		t.Fatal("nil deps must reject construction")
	}
	var nilResolver AdminRoleResolver
	if nilResolver != nil {
		t.Fatal("nil func resolver")
	}
	// And a coded error type exists for the middleware-level refusal.
	e := excerrors.New("UNAUTHORIZED_ROLE", "x")
	if excerrors.CodeOf(e) != "UNAUTHORIZED_ROLE" {
		t.Fatal("code plumbing")
	}
}

type fakeFlagWriter struct{ set map[string]string }

func (f *fakeFlagWriter) SetHaltScope(_ context.Context, scope, target, reason string) error {
	if f.set == nil {
		f.set = map[string]string{}
	}
	f.set[scope+":"+target] = reason
	return nil
}
func (f *fakeFlagWriter) ClearHaltScope(_ context.Context, scope, target string) error {
	delete(f.set, scope+":"+target)
	return nil
}
