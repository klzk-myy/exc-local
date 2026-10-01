// Phase-08 Task 8.3.1 — end-to-end integration suite.
//
// Layout: this file owns TestMain (registry + report emission) and the
// shared live-stack fixture. stack_e2e_test.go holds the live-stack
// legs, delegated_test.go the services/ctest legs, engine_wal_test.go
// the binary-level crash/WAL legs, contracts_test.go the registry
// integrity checks.
//
// Status vocabulary: PASS | FAIL | EXECUTABLE | PLANNED | BLOCKED —
// BLOCKED means a bound leg could not execute on this host (reason is
// recorded verbatim). Nothing here fabricates a pass.
package integration

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"exchange-integration/itest"
)

var (
	env  = itest.DefaultEnv()
	reg  *itest.Registry
	rows []itest.Contract
)

// stack is the lazily-booted live topology shared by e2e legs.
type stack struct {
	once    sync.Once
	err     error
	dir     string
	engines []*itest.EngineProc
	oracle  *itest.OracleProc
	gw      *itest.GatewayProc
	jwtKey  string
	dataKey []byte
	fx      *itest.Fixture
	http    *itest.Client
	runTag  string
}

var stk stack

// getStack boots (once): 8 matching_engine shards (sharding.yaml's
// 0-7) + the real gateway binary against the scratch PG/Redis/NATS.
// Every leg runs real binaries over real wire paths — the only
// substitutions are environment (scratch DB, dev JWT/data keys).
func getStack(t *testing.T) *stack {
	t.Helper()
	stk.once.Do(func() {
		ctx := context.Background()
		stk.err = bootStack(ctx)
	})
	if stk.err != nil {
		t.Skipf("stack unavailable: %v", stk.err)
	}
	return &stk
}

func bootStack(ctx context.Context) error {
	if r := env.GatePG(ctx); r != "" {
		return fmt.Errorf("%s", r)
	}
	if r := env.GateRedis(ctx); r != "" {
		return fmt.Errorf("%s", r)
	}
	if r := env.GateEngine(); r != "" {
		return fmt.Errorf("%s", r)
	}
	// The suite owns logical DB 9 of the scratch Redis (env.go) — flush
	// it so stale ip_ban/rl429/idempotency keys from earlier runs can't
	// contaminate this stack (a stale 127.0.0.1 ban previously
	// cascade-failed every leg).
	rdb := env.RedisClient()
	if err := rdb.FlushDB(ctx).Err(); err != nil {
		rdb.Close()
		return fmt.Errorf("scratch redis flush: %w", err)
	}
	rdb.Close()

	dir, err := os.MkdirTemp("", "exc-itest-stack-*")
	if err != nil {
		return err
	}
	stk.dir = dir
	stk.runTag = fmt.Sprintf("%d", time.Now().Unix()%100000)
	stk.jwtKey = itest.GenKeyB64()
	dk, err := itest.GenKeyBytes()
	if err != nil {
		return err
	}
	stk.dataKey = dk

	// Engines: one process per shard (the deployment model). Instrument
	// 1 (EUR/USD) is the traded book on shard 0; other shards serve the
	// same instrument id purely for liveness — the suite only trades
	// EUR/USD, which sharding.yaml pins to shard 0.
	for _, s := range env.Shards {
		p, err := itest.StartEngine(ctx, env, dir, s, 1)
		if err != nil {
			return fmt.Errorf("engine shard %d: %w", s, err)
		}
		stk.engines = append(stk.engines, p)
	}
	for _, p := range stk.engines {
		if err := p.WaitReady(ctx, 10*time.Second); err != nil {
			return err
		}
	}

	// Phase-19.5: the price oracle is a required stack component — the
	// order admission gate fails closed (PRICE_ORACLE_UNAVAILABLE) for
	// margin instruments without published oracle health. Boot the
	// scripted-sim oracle against the scratch Redis DB before the
	// gateway so the gate sees a live oracle.
	orcl, err := itest.StartOracle(ctx, env, dir, []string{
		"EUR/USD", "GBP/USD", "USD/JPY", "USD/CHF",
		"AUD/USD", "NZD/USD", "USD/CAD", "USD/MXN",
		"USD/BRL", "EUR/GBP", "EUR/JPY", "EUR/CHF",
	})
	if err != nil {
		return fmt.Errorf("oracle: %w", err)
	}
	stk.oracle = orcl
	if err := orcl.WaitHealthy(ctx, env, "EUR/USD", 10*time.Second); err != nil {
		return err
	}

	bin, err := itest.BuildGateway(ctx, env, dir)
	if err != nil {
		return err
	}
	port, err := itest.FreePort()
	if err != nil {
		return err
	}
	dataKeyB64 := itest.B64(stk.dataKey)
	gw, err := itest.StartGateway(ctx, env, bin, dir, port, stk.jwtKey, dataKeyB64)
	if err != nil {
		return err
	}
	stk.gw = gw
	if err := gw.WaitReady(ctx, 20*time.Second); err != nil {
		return err
	}
	if err := gw.WaitShardsReady(ctx, 20*time.Second); err != nil {
		return err
	}
	stk.http = itest.NewClient(gw.Addr, "")

	pool, err := env.Pool(ctx)
	if err != nil {
		return err
	}
	defer pool.Close()
	// The scratch DB persists across suite runs — clear residue owned by
	// the delegated RBAC lifecycle test (fixed principal IDs 9200xx,
	// IT-* campaign labels) so its re-runs stay idempotent.
	itest.ScrubDelegatedResidue(ctx, pool)
	fx, err := itest.SeedFixture(ctx, pool, stk.dataKey, stk.runTag)
	if err != nil {
		return fmt.Errorf("seed fixture: %w", err)
	}
	stk.fx = fx
	return nil
}

// stopStack tears the stack down (SIGTERM engines + gateway).
func stopStack() {
	if stk.gw != nil {
		stk.gw.Stop()
	}
	if stk.oracle != nil {
		stk.oracle.Stop()
	}
	for _, p := range stk.engines {
		p.Stop()
	}
}

// cli returns an HTTP client with a unique per-test X-Forwarded-For
// identity in TEST-NET-3 (203.0.113.0/24). The gateway's edge rate
// limiter runs BEFORE auth resolves, so every request is billed to the
// caller's public IP bucket (5/s + burst 10): per-test IPs keep one
// noisy leg from starving or banning the rest. The .99 address is
// reserved for the ban-escalation leg.
func cli(t *testing.T) *itest.Client {
	return itest.NewClient(stk.gw.Addr, testIP(t))
}

// cliAlt returns a client on the test's Nth alternate source IP
// (TEST-NET-2) — legs that intentionally exhaust a public bucket still
// need a fresh identity for subsequent assertions in the same test.
func cliAlt(t *testing.T, n int) *itest.Client {
	h := uint32(2166136261)
	for _, b := range []byte(t.Name()) {
		h ^= uint32(b)
		h *= 16777619
	}
	return itest.NewClient(stk.gw.Addr, fmt.Sprintf("198.51.100.%d", 1+((int(h)+n)%200)))
}

// testIP derives the per-test TEST-NET-3 source identity shared by the
// REST client and the WS dialer.
func testIP(t *testing.T) string {
	h := uint32(2166136261)
	for _, b := range []byte(t.Name()) {
		h ^= uint32(b)
		h *= 16777619
	}
	return fmt.Sprintf("203.0.113.%d", 1+(h%90))
}

// record is the suite-wide criterion outcome sink.
func record(id int, leg, status, detail string) {
	reg.Recordf(id, leg, status, "%s", detail)
}

func recordLeg(id int, leg string, start time.Time, ok bool, detail string) {
	st := "PASS"
	if !ok {
		st = "FAIL"
	}
	reg.Record(id, leg, st, detail, time.Since(start))
}

func recordBlocked(id int, leg, reason string) {
	reg.Record(id, leg, "BLOCKED", reason, 0)
}

// loadContracts parses the committed registry; TestMain falls back to a
// fresh projection so the suite still runs when contracts.json is stale
// (the integrity test flags the staleness separately).
func loadContracts() []itest.Contract {
	b, err := os.ReadFile(env.Root + "/tests/integration/contracts.json")
	if err == nil {
		var f struct {
			Contracts []itest.Contract `json:"contracts"`
		}
		if json.Unmarshal(b, &f) == nil && len(f.Contracts) > 0 {
			return f.Contracts
		}
	}
	tf, terr := itest.LoadTrace(env.TracePath)
	if terr != nil {
		return nil
	}
	return itest.Contracts(tf, itest.CriterionBindings)
}

func TestMain(m *testing.M) {
	rows = loadContracts()
	reg = itest.NewRegistry(rows)
	code := m.Run()
	stopStack()
	if !reg.HasLegs() {
		fmt.Println("no bound legs executed; coverage report not overwritten")
	} else if rep, err := reg.Emit(env.ReportPath); err == nil {
		fmt.Println(rep.SummaryLine())
		fmt.Println("report:", env.ReportPath)
	} else {
		fmt.Println("report emit failed:", err)
	}
	os.Exit(code)
}
