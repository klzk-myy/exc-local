// Postgres integration coverage for Task 9.3.30 — migration 091 tables,
// env-scoped fleet actions, dual control, prod watermark, and the full
// promotion gate path (blocked + executed).
//
// Gated on EXC_PG_TEST=1; targets EXC_TEST_DSN (default: dev compose DB).
//
//	EXC_PG_TEST=1 go test -v -count=1 ./internal/fleet/
package fleet

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"exchange/internal/db"
	"exchange/internal/gateway"
)

func pgGate(t *testing.T) (*pgxpool.Pool, context.Context) {
	t.Helper()
	if os.Getenv("EXC_PG_TEST") != "1" {
		t.Skip("set EXC_PG_TEST=1 to run Postgres integration tests")
	}
	dsn := os.Getenv("EXC_TEST_DSN")
	if dsn == "" {
		dsn = "postgres://exchange:exchange_dev@127.0.0.1:5433/exchange?sslmode=disable"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	pool, err := db.NewPool(ctx, dsn, 4)
	if err != nil || pool.Ping(ctx) != nil {
		cancel()
		t.Skipf("postgres unreachable: %v", err)
	}
	t.Cleanup(func() { pool.Close(); cancel() })
	return pool, ctx
}

// mkUser inserts a minimal users row for FK satisfaction.
func mkUser(t *testing.T, pool *pgxpool.Pool, ctx context.Context, tag string) int64 {
	t.Helper()
	var id int64
	err := pool.QueryRow(ctx, `
		INSERT INTO users (email) VALUES ($1) RETURNING id`,
		fmt.Sprintf("fleet-test-%s-%d@exc.local", tag, time.Now().UnixNano())).
		Scan(&id)
	if err != nil {
		t.Fatalf("insert user: %v", err)
	}
	return id
}

// mkHost inserts a fleet host in the named env; hostname is suffixed
// with the run id so repeat runs on a shared dev DB never collide.
func mkHost(t *testing.T, pool *pgxpool.Pool, ctx context.Context, env, hostname, role string) int64 {
	t.Helper()
	hostname = fmt.Sprintf("%s-%d", hostname, time.Now().UnixNano()%100000)
	var id int64
	err := pool.QueryRow(ctx, `
		INSERT INTO fleet_hosts (environment_id, hostname, role, health)
		SELECT e.id, $2, $3, 'HEALTHY' FROM environments e WHERE e.name = $1
		RETURNING id`, env, hostname, role).Scan(&id)
	if err != nil {
		t.Fatalf("insert host %s@%s: %v", hostname, env, err)
	}
	return id
}

func saResolver(context.Context, int64) (string, error) {
	return gateway.RoleSuperAdmin, nil
}

// healthyProbes makes every production interlock pass except the real
// deploy_windows query (kept PG-backed so the window gate is genuinely
// exercised).
func healthyProbes() *Probes {
	return &Probes{
		OpenIncidents: func(context.Context) (int, string, error) {
			return 0, "test-probe", nil
		},
		DRStandbyHealthy: func(context.Context) (bool, string, error) {
			return true, "test-probe: standby streaming", nil
		},
	}
}

func auditCount(t *testing.T, pool *pgxpool.Pool, ctx context.Context, action string) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM admin_audit_log WHERE action = $1`, action).
		Scan(&n); err != nil {
		t.Fatalf("audit count %s: %v", action, err)
	}
	return n
}

func TestFleetHostActionsIntegration(t *testing.T) {
	pool, ctx := pgGate(t)
	maker := mkUser(t, pool, ctx, "maker")
	approver := mkUser(t, pool, ctx, "approver")
	svc := NewService(pool, saResolver, nil)

	devHost := mkHost(t, pool, ctx, EnvDev, "it-dev-core-1", "CORE")
	devActor := Actor{UserID: maker, Env: EnvDev, ClientIP: "203.0.113.10"}

	// drain → cordon → decommission lifecycle.
	sa, err := svc.RequestAction(ctx, devActor, devHost, ActionDrain, "deploy prep")
	if err != nil || sa.Status != "EXECUTED" {
		t.Fatalf("drain: %v %+v", err, sa)
	}
	if _, err := svc.RequestAction(ctx, devActor, devHost, ActionCordon, "drained"); err != nil {
		t.Fatalf("cordon: %v", err)
	}
	if _, err := svc.RequestAction(ctx, devActor, devHost, ActionDecommission, "retire"); err != nil {
		t.Fatalf("decommission: %v", err)
	}
	// Terminal state — no further actions.
	if _, err := svc.RequestAction(ctx, devActor, devHost, ActionDrain, "again"); err == nil {
		t.Fatal("action on DECOMMISSIONED host must fail")
	} else if codeOf(t, err) != "INVALID_LIFECYCLE_TRANSITION" {
		t.Fatalf("expected INVALID_LIFECYCLE_TRANSITION, got %v", err)
	}

	// Env isolation: a staging-scoped actor cannot touch the dev host.
	other := mkHost(t, pool, ctx, EnvDev, "it-dev-core-2", "CORE")
	stgActor := Actor{UserID: maker, Env: EnvStaging, ClientIP: "203.0.113.10"}
	if _, err := svc.RequestAction(ctx, stgActor, other, ActionDrain, "x"); err == nil ||
		codeOf(t, err) != "FORBIDDEN" {
		t.Fatalf("cross-env host action must be FORBIDDEN: %v", err)
	}

	// Production: dual control required.
	prodHost := mkHost(t, pool, ctx, EnvProduction, "it-prod-core-1", "CORE")
	prodActor := Actor{UserID: maker, Env: EnvProduction, ClientIP: "203.0.113.11"}
	if _, err := svc.RequestAction(ctx, prodActor, prodHost, ActionDrain, "no approver"); err == nil ||
		codeOf(t, err) != "DUAL_CONTROL_REQUIRED" {
		t.Fatalf("prod action without approver must be DUAL_CONTROL_REQUIRED: %v", err)
	}
	self := prodActor
	self.ApproverID = maker
	if _, err := svc.RequestAction(ctx, self, prodHost, ActionDrain, "self"); err == nil ||
		codeOf(t, err) != "DUAL_CONTROL_VIOLATION" {
		t.Fatalf("self-approval must be DUAL_CONTROL_VIOLATION: %v", err)
	}
	prodActor.ApproverID = approver
	before := auditCount(t, pool, ctx, "fleet.env_context")
	sa, err = svc.RequestAction(ctx, prodActor, prodHost, ActionDrain, "with approver")
	if err != nil {
		t.Fatalf("prod drain with approver: %v", err)
	}
	if sa.ApprovedBy == nil || *sa.ApprovedBy != approver {
		t.Fatalf("approved_by not recorded: %+v", sa)
	}
	if auditCount(t, pool, ctx, "fleet.env_context") != before+1 {
		t.Fatal("prod host action must write the env-context watermark")
	}
}

func TestPromotionIntegration(t *testing.T) {
	pool, ctx := pgGate(t)
	maker := mkUser(t, pool, ctx, "rel-maker")
	approver := mkUser(t, pool, ctx, "rel-approver")
	svc := NewService(pool, saResolver, healthyProbes())
	hash := "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

	devActor := Actor{UserID: maker, Env: EnvDev, ClientIP: "203.0.113.20"}
	rel, err := svc.RegisterRelease(ctx, devActor, Release{
		Component:    "matching-core",
		Version:      fmt.Sprintf("9.30.0-it%d", time.Now().UnixNano()%100000),
		ArtifactHash: hash, Notes: "integration test"})
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	if rel.Env != EnvDev || rel.Status != RelDeployed {
		t.Fatalf("release must land DEPLOYED in dev: %+v", rel)
	}

	// Direction: dev→production skip is FORBIDDEN.
	if _, _, err := svc.Promote(ctx, devActor, rel.ID, EnvProduction, "skip"); err == nil ||
		codeOf(t, err) != "FORBIDDEN" {
		t.Fatalf("dev→prod skip must be FORBIDDEN: %v", err)
	}

	// Session-env mismatch: staging destination requires a staging session.
	if _, _, err := svc.Promote(ctx, devActor, rel.ID, EnvStaging, "wrong env"); err == nil ||
		codeOf(t, err) != "FORBIDDEN" {
		t.Fatalf("promotion from non-destination session env must be FORBIDDEN: %v", err)
	}

	// Blocked attempt (no approver) persists a BLOCKED row with the gate
	// snapshot.
	stgActor := Actor{UserID: maker, Env: EnvStaging, ClientIP: "203.0.113.21"}
	p, gates, err := svc.Promote(ctx, stgActor, rel.ID, EnvStaging, "no approver")
	if err == nil || codeOf(t, err) != "FORBIDDEN" {
		t.Fatalf("promotion without approver must be FORBIDDEN: %v", err)
	}
	if p == nil || p.Status != PromoBlocked || len(gates) == 0 {
		t.Fatalf("blocked promotion must persist a BLOCKED row + gates: %+v", p)
	}

	// Approved dev→staging.
	stgActor.ApproverID = approver
	p, gates, err = svc.Promote(ctx, stgActor, rel.ID, EnvStaging, "approved")
	if err != nil || p == nil || p.Status != PromoExecuted {
		t.Fatalf("staging promotion: %v %+v", err, p)
	}
	var envNow string
	if err := pool.QueryRow(ctx, `SELECT env FROM releases WHERE id=$1`, rel.ID).
		Scan(&envNow); err != nil || envNow != EnvStaging {
		t.Fatalf("release must be staged: env=%q err=%v", envNow, err)
	}

	// Staging→production blocked: window gate closed (probe-forced —
	// the shared dev DB may legitimately hold open windows from other
	// runs; the real deploy_windows query is exercised below on the
	// success path).
	closedProbes := healthyProbes()
	closedProbes.DeployWindowOpen = func(context.Context) (bool, string, error) {
		return false, "no open production deploy window", nil
	}
	svcClosed := NewService(pool, saResolver, closedProbes)
	prodActor := Actor{UserID: maker, Env: EnvProduction,
		ClientIP: "203.0.113.22", ApproverID: approver}
	_, gates, err = svcClosed.Promote(ctx, prodActor, rel.ID, EnvProduction, "closed window")
	if err == nil || codeOf(t, err) != "FORBIDDEN" {
		t.Fatalf("prod promotion without a window must be FORBIDDEN: %v", err)
	}
	if gateByName(gates, "deploy_window_open").Passed {
		t.Fatal("window gate must fail when no window is open")
	}

	// Open a window + attach fresh evidence → promotion executes and the
	// prod-context watermark lands in admin_audit_log. This path uses
	// the real deploy_windows query (no DeployWindowOpen probe).
	if _, err := pool.Exec(ctx, `
		INSERT INTO deploy_windows (environment, opens_at, closes_at, status, opened_by, reason)
		VALUES ('production', now() - interval '1 hour', now() + interval '2 hours', 'OPEN', $1, 'test window')`,
		maker); err != nil {
		t.Fatalf("open deploy window: %v", err)
	}
	evidence := json.RawMessage(`{"soak":{"status":"PASS","finished_at":"` +
		time.Now().Add(-24*time.Hour).UTC().Format(time.RFC3339) +
		`"},"checkpoints":{"pending":0}}`)
	if _, err := pool.Exec(ctx,
		`UPDATE releases SET gate_evidence=$2 WHERE id=$1`, rel.ID, evidence); err != nil {
		t.Fatalf("set gate_evidence: %v", err)
	}
	before := auditCount(t, pool, ctx, "fleet.env_context")
	p, gates, err = svc.Promote(ctx, prodActor, rel.ID, EnvProduction, "all gates green")
	if err != nil || p == nil || p.Status != PromoExecuted {
		t.Fatalf("prod promotion: %v gates=%+v", err, gates)
	}
	for _, g := range gates {
		if !g.Passed {
			t.Fatalf("unexpected failing gate: %+v", g)
		}
	}
	if auditCount(t, pool, ctx, "fleet.env_context") != before+1 {
		t.Fatal("prod promotion must write the env-context watermark")
	}

	// Production is a sink — nothing promotes out of it.
	if _, _, err := svc.Promote(ctx, prodActor, rel.ID, EnvDev, "down"); err == nil ||
		codeOf(t, err) != "FORBIDDEN" {
		t.Fatalf("prod→dev must be FORBIDDEN: %v", err)
	}
}

func TestTopologyIntegration(t *testing.T) {
	pool, ctx := pgGate(t)
	svc := NewService(pool, saResolver, nil)
	mkHost(t, pool, ctx, EnvDev, "it-topo-gw", "GATEWAY")
	actor := Actor{UserID: mkUser(t, pool, ctx, "topo"), Env: EnvDev}

	tv, err := svc.Topology(ctx, actor, "")
	if err != nil || tv.Env != EnvDev {
		t.Fatalf("topology: %v %+v", err, tv)
	}
	found := false
	for _, hosts := range tv.Shards {
		for _, h := range hosts {
			if len(h.Hostname) >= 10 && h.Hostname[:10] == "it-topo-gw" {
				found = true
			}
		}
	}
	if !found {
		t.Fatal("topology must include the inserted host")
	}
	// Cross-env topology read is refused.
	if _, err := svc.Topology(ctx, actor, EnvProduction); err == nil ||
		codeOf(t, err) != "FORBIDDEN" {
		t.Fatalf("cross-env topology must be FORBIDDEN: %v", err)
	}
}
