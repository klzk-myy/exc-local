// Unit coverage for the pure parts of the fleet/promotion service —
// direction lattice, secret-path isolation, four-eyes approval, gate
// evaluation with injected probes. Live-PG coverage is in
// fleet_integration_test.go (EXC_PG_TEST=1).
package fleet

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"

	"exchange/internal/admin"
	"exchange/internal/gateway"

	excerrors "exchange/pkg/errors"
)

func codeOf(t *testing.T, err error) string {
	t.Helper()
	var e *excerrors.Error
	if !errors.As(err, &e) {
		t.Fatalf("expected *errors.Error, got %v", err)
	}
	return e.Code
}

// --- environment context ----------------------------------------------------

func TestRequireEnv(t *testing.T) {
	if _, err := requireEnv(Actor{}); err == nil ||
		codeOf(t, err) != "FORBIDDEN" {
		t.Fatalf("empty env context must fail FORBIDDEN, got %v", err)
	}
	if got, err := requireEnv(Actor{Env: "prod"}); err != nil || got != EnvProduction {
		t.Fatalf("prod alias should normalize to production, got %q %v", got, err)
	}
}

// --- per-env secret path isolation (§19.16.3) --------------------------------

func TestSecretPath(t *testing.T) {
	p, err := SecretPath("staging", "postgres_dsn")
	if err != nil || p != "secret/data/staging/postgres_dsn" {
		t.Fatalf("unexpected path %q %v", p, err)
	}
	for _, bad := range []string{"../prod/db", "prod/db", "/abs", "a b", ""} {
		if _, err := SecretPath("dev", bad); err == nil {
			t.Fatalf("secret name %q must be rejected", bad)
		}
	}
	if _, err := SecretPath("", "x"); err == nil {
		t.Fatal("secret resolution requires an env context")
	}
	// The env prefix comes from the session, never the name — a caller
	// cannot reach production's namespace from a dev session.
	p, _ = SecretPath("dev", "db")
	if !strings.HasPrefix(p, "secret/data/dev/") {
		t.Fatalf("dev session resolved outside dev namespace: %q", p)
	}
}

// --- four-eyes approval ------------------------------------------------------

func TestRequireApprover(t *testing.T) {
	ctx := context.Background()
	saRole := func(context.Context, int64) (string, error) {
		return gateway.RoleSuperAdmin, nil
	}

	// No approver → DUAL_CONTROL_REQUIRED.
	svc := NewService(nil, saRole, nil)
	if err := svc.requireApprover(ctx, Actor{UserID: 1},
		gateway.RoleSuperAdmin); err == nil ||
		codeOf(t, err) != "DUAL_CONTROL_REQUIRED" {
		t.Fatalf("missing approver: %v", err)
	}
	// Self-approval → DUAL_CONTROL_VIOLATION.
	if err := svc.requireApprover(ctx,
		Actor{UserID: 1, ApproverID: 1},
		gateway.RoleSuperAdmin); err == nil ||
		codeOf(t, err) != "DUAL_CONTROL_VIOLATION" {
		t.Fatalf("self-approval: %v", err)
	}
	// Nil resolver fails closed.
	svcNoRes := NewService(nil, nil, nil)
	if err := svcNoRes.requireApprover(ctx,
		Actor{UserID: 1, ApproverID: 2},
		gateway.RoleSuperAdmin); err == nil ||
		codeOf(t, err) != "UNAUTHORIZED_ROLE" {
		t.Fatalf("nil resolver: %v", err)
	}
	// Wrong role.
	svcWeak := NewService(nil,
		func(context.Context, int64) (string, error) {
			return gateway.RoleSupportAgent, nil
		}, nil)
	if err := svcWeak.requireApprover(ctx,
		Actor{UserID: 1, ApproverID: 2},
		gateway.RoleSuperAdmin); err == nil ||
		codeOf(t, err) != "UNAUTHORIZED_ROLE" {
		t.Fatalf("weak approver: %v", err)
	}
	// Distinct Super Admin approver passes.
	if err := svc.requireApprover(ctx,
		Actor{UserID: 1, ApproverID: 2},
		gateway.RoleSuperAdmin); err != nil {
		t.Fatalf("valid approver rejected: %v", err)
	}
}

// --- host state transitions --------------------------------------------------

func TestActionTransitions(t *testing.T) {
	cases := []struct {
		action, from string
		ok           bool
		to           string
	}{
		{ActionDrain, HostActive, true, HostDraining},
		{ActionDrain, HostDraining, false, ""},
		{ActionDrain, HostMaintenance, false, ""},
		{ActionDrain, HostDecommissioned, false, ""},
		{ActionCordon, HostActive, true, HostMaintenance},
		{ActionCordon, HostDraining, true, HostMaintenance},
		{ActionReboot, HostActive, true, ""}, // state preserved
		{ActionReboot, HostMaintenance, true, ""},
		{ActionReboot, HostDecommissioned, false, ""},
		{ActionDecommission, HostActive, true, HostDecommissioned},
		{ActionDecommission, HostMaintenance, true, HostDecommissioned},
		{ActionDecommission, HostDecommissioned, false, ""},
	}
	for _, c := range cases {
		spec, ok := actionTransition[c.action]
		if !ok {
			t.Fatalf("action %s missing", c.action)
		}
		if got := spec.from[c.from]; got != c.ok {
			t.Errorf("%s from %s: legal=%v want %v", c.action, c.from, got, c.ok)
		}
		if c.ok && spec.to != c.to {
			t.Errorf("%s from %s: to=%q want %q", c.action, c.from, spec.to, c.to)
		}
	}
	if _, ok := actionTransition["SHUTDOWN"]; ok {
		t.Fatal("unknown action must not have a transition")
	}
}

// --- promotion direction lattice ----------------------------------------------

func TestPromotionDirectionLattice(t *testing.T) {
	if nextEnv[EnvDev] != EnvStaging || nextEnv[EnvStaging] != EnvProduction {
		t.Fatalf("lattice must be dev→staging→production, got %v", nextEnv)
	}
	if _, ok := nextEnv[EnvProduction]; ok {
		t.Fatal("production is a sink — it must have no promotion target")
	}
	// Anything not in the lattice (prod→dev, staging→dev, dev→prod skip)
	// fails the map lookup in Promote.
	for _, from := range []string{EnvProduction} {
		for _, to := range []string{EnvDev, EnvStaging} {
			if next, ok := nextEnv[from]; ok && next == to {
				t.Fatalf("downward promotion %s→%s must be unreachable", from, to)
			}
		}
	}
	if nextEnv[EnvDev] == EnvProduction {
		t.Fatal("dev→production must not skip staging")
	}
}

// --- gate evaluation with injected probes -------------------------------------

func evidenceBundle(soakStatus, finishedAt string, pending int) json.RawMessage {
	return json.RawMessage(`{"soak":{"status":"` + soakStatus +
		`","finished_at":"` + finishedAt + `"},"checkpoints":{"pending":` +
		strconv.Itoa(pending) + `}}`)
}

func probeSvc(t *testing.T, probes *Probes) *Service {
	t.Helper()
	svc := NewService(nil, func(context.Context, int64) (string, error) {
		return gateway.RoleSuperAdmin, nil
	}, probes)
	svc.SetClockForTest(func() time.Time {
		return time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC)
	})
	return svc
}

func gateByName(gates []GateResult, name string) GateResult {
	for _, g := range gates {
		if g.Name == name {
			return g
		}
	}
	return GateResult{Name: name, Detail: "absent"}
}

func TestEvaluateGatesStagingOnlyNeedsApproval(t *testing.T) {
	svc := probeSvc(t, nil)
	rel := &Release{Env: EnvDev, Status: RelDeployed}

	// No approver → approval gate fails.
	gates := svc.evaluateGates(context.Background(), rel,
		Actor{UserID: 1, Env: EnvStaging}, EnvStaging)
	if len(gates) != 1 || gates[0].Name != "approval" || gates[0].Passed {
		t.Fatalf("staging must evaluate only the approval gate: %+v", gates)
	}

	// Distinct Super Admin approver → passes; no infra probes consulted
	// for a non-production target.
	gates = svc.evaluateGates(context.Background(), rel,
		Actor{UserID: 1, Env: EnvStaging, ApproverID: 2}, EnvStaging)
	if len(gates) != 1 || !gates[0].Passed {
		t.Fatalf("staging promotion with approver should pass: %+v", gates)
	}
}

func TestEvaluateGatesProductionInterlocks(t *testing.T) {
	fresh := time.Date(2026, 9, 18, 0, 0, 0, 0, time.UTC).Format(time.RFC3339)
	probes := &Probes{
		DeployWindowOpen: func(context.Context) (bool, string, error) {
			return true, "window OPEN", nil
		},
		OpenIncidents: func(context.Context) (int, string, error) {
			return 0, "test", nil
		},
		DRStandbyHealthy: func(context.Context) (bool, string, error) {
			return true, "standby streaming", nil
		},
	}
	svc := probeSvc(t, probes)
	rel := &Release{
		Env: EnvStaging, Status: RelDeployed,
		GateEvidence: evidenceBundle("PASS", fresh, 0),
	}
	gates := svc.evaluateGates(context.Background(), rel,
		Actor{UserID: 1, Env: EnvProduction, ApproverID: 2}, EnvProduction)
	for _, g := range gates {
		if !g.Passed {
			t.Fatalf("gate %s should pass: %+v", g.Name, gates)
		}
	}
	names := map[string]bool{}
	for _, g := range gates {
		names[g.Name] = true
	}
	for _, want := range []string{"approval", "deploy_window_open",
		"no_p0_p1_incidents", "dr_standby_healthy",
		"soak_fresh", "checkpoints_clear"} {
		if !names[want] {
			t.Fatalf("production gate %s missing from %v", want, names)
		}
	}
}

func TestEvaluateGatesFailClosed(t *testing.T) {
	fresh := time.Date(2026, 9, 18, 0, 0, 0, 0, time.UTC).Format(time.RFC3339)

	// Open P1 incident blocks.
	svc := probeSvc(t, &Probes{
		DeployWindowOpen: func(context.Context) (bool, string, error) {
			return true, "open", nil
		},
		OpenIncidents: func(context.Context) (int, string, error) {
			return 1, "test", nil
		},
		DRStandbyHealthy: func(context.Context) (bool, string, error) {
			return true, "ok", nil
		},
	})
	rel := &Release{Env: EnvStaging, Status: RelDeployed,
		GateEvidence: evidenceBundle("PASS", fresh, 0)}
	gates := svc.evaluateGates(context.Background(), rel,
		Actor{UserID: 1, Env: EnvProduction, ApproverID: 2}, EnvProduction)
	if gateByName(gates, "no_p0_p1_incidents").Passed {
		t.Fatal("open P1 must block production promotion")
	}

	// Closed deploy window blocks.
	svc = probeSvc(t, &Probes{
		DeployWindowOpen: func(context.Context) (bool, string, error) {
			return false, "no open window", nil
		},
		OpenIncidents: func(context.Context) (int, string, error) {
			return 0, "test", nil
		},
		DRStandbyHealthy: func(context.Context) (bool, string, error) {
			return true, "ok", nil
		},
	})
	gates = svc.evaluateGates(context.Background(), rel,
		Actor{UserID: 1, Env: EnvProduction, ApproverID: 2}, EnvProduction)
	if gateByName(gates, "deploy_window_open").Passed {
		t.Fatal("closed deploy window must block")
	}

	// DR standby unhealthy blocks.
	svc = probeSvc(t, &Probes{
		DeployWindowOpen: func(context.Context) (bool, string, error) {
			return true, "open", nil
		},
		OpenIncidents: func(context.Context) (int, string, error) {
			return 0, "test", nil
		},
		DRStandbyHealthy: func(context.Context) (bool, string, error) {
			return false, "lag 40s", nil
		},
	})
	gates = svc.evaluateGates(context.Background(), rel,
		Actor{UserID: 1, Env: EnvProduction, ApproverID: 2}, EnvProduction)
	if gateByName(gates, "dr_standby_healthy").Passed {
		t.Fatal("unhealthy DR standby must block")
	}

	// Unavailable source fails closed (spec §2.7).
	svc = probeSvc(t, &Probes{
		DeployWindowOpen: func(context.Context) (bool, string, error) {
			return true, "open", nil
		},
		OpenIncidents: func(context.Context) (int, string, error) {
			return 0, "test", nil
		},
		DRStandbyHealthy: func(context.Context) (bool, string, error) {
			return false, "", errors.New("pg_stat_replication unreachable")
		},
	})
	gates = svc.evaluateGates(context.Background(), rel,
		Actor{UserID: 1, Env: EnvProduction, ApproverID: 2}, EnvProduction)
	if g := gateByName(gates, "dr_standby_healthy"); g.Passed ||
		!strings.Contains(g.Detail, "unavailable") {
		t.Fatalf("unavailable DR source must fail closed: %+v", g)
	}
}

func TestGateEvidenceRules(t *testing.T) {
	probes := &Probes{
		DeployWindowOpen: func(context.Context) (bool, string, error) {
			return true, "open", nil
		},
		OpenIncidents: func(context.Context) (int, string, error) {
			return 0, "t", nil
		},
		DRStandbyHealthy: func(context.Context) (bool, string, error) {
			return true, "ok", nil
		},
	}
	svc := probeSvc(t, probes)
	fresh := time.Date(2026, 9, 18, 0, 0, 0, 0, time.UTC).Format(time.RFC3339)
	stale := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC).Format(time.RFC3339)
	actor := Actor{UserID: 1, Env: EnvProduction, ApproverID: 2}
	ctx := context.Background()

	// Stale soak → soak_fresh fails.
	rel := &Release{Env: EnvStaging, Status: RelDeployed,
		GateEvidence: evidenceBundle("PASS", stale, 0)}
	gates := svc.evaluateGates(ctx, rel, actor, EnvProduction)
	if gateByName(gates, "soak_fresh").Passed {
		t.Fatal("stale soak evidence must block")
	}
	// Pending checkpoints → checkpoints_clear fails.
	rel.GateEvidence = evidenceBundle("PASS", fresh, 2)
	gates = svc.evaluateGates(ctx, rel, actor, EnvProduction)
	if gateByName(gates, "checkpoints_clear").Passed {
		t.Fatal("pending checkpoints must block")
	}
	// Unparseable bundle → both evidence gates fail.
	rel.GateEvidence = json.RawMessage(`{not json`)
	gates = svc.evaluateGates(ctx, rel, actor, EnvProduction)
	if gateByName(gates, "soak_fresh").Passed ||
		gateByName(gates, "checkpoints_clear").Passed {
		t.Fatal("unparseable gate_evidence must fail both evidence gates")
	}
	// Failed soak status → fails even when fresh.
	rel.GateEvidence = evidenceBundle("FAIL", fresh, 0)
	gates = svc.evaluateGates(ctx, rel, actor, EnvProduction)
	if gateByName(gates, "soak_fresh").Passed {
		t.Fatal("non-PASS soak must block")
	}
}

func TestGateFailuresRendersFailedNames(t *testing.T) {
	got := gateFailures([]GateResult{
		{Name: "approval", Passed: true},
		{Name: "deploy_window_open", Passed: false},
		{Name: "dr_standby_healthy", Passed: false},
	})
	if got != "deploy_window_open, dr_standby_healthy" {
		t.Fatalf("gateFailures: %q", got)
	}
}

// --- misc --------------------------------------------------------------------

func TestLower(t *testing.T) {
	if lower("DECOMMISSION") != "decommission" {
		t.Fatal("lower must fold ASCII uppercase")
	}
	if lower("") != "" {
		t.Fatal("lower(\"\")")
	}
}

// admin import retained for documentation of the env vocabulary seam.
var _ = admin.NormalizeEnv
