// Live-stack leg of Task 8.5.3.3 — errblast against the REAL gateway
// binary on the dev topology (docker-compose.dev.yml: PG 5433, Redis
// primary 16379, NATS 4222; the same stack tests/integration spawns).
//
// Gate: EXC_ERRBLAST_LIVE=1 (plus reachable dev infra — unreachable
// dependencies report SKIP, never a fabricated PASS; Phase-08 rule).
//
// Leg A (plan step 2 — degradation transition observable surface):
// write the Phase-02 ModeManager's Redis mode record
// (system:degradation:{mode,entered_at,reason}) as ReadOnly on the
// gateway's coordination DB, then verify on the wire that order
// placement rejects 503 DEGRADED_MODE while market-data reads and the
// X-Degradation-Mode header keep flowing; clearing the record restores
// order-path evaluation.
//
// Leg B (plan step 1 scaled to dev): a real errblast burst —
// paced invalid-order fire across every defect class with rotating
// minted JWTs and rotating X-Forwarded-For identities — asserting the
// rejection path stays clean (verdict 0) and the process stays alive.
package main

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"io"
	mrand "math/rand"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// Minimal RESP client — just enough to write/read the mode record on the
// scratch DB (keeps this module dependency-free; production readers use
// services/internal/redis).
// ---------------------------------------------------------------------------

func respArgs(args ...string) []byte {
	var b strings.Builder
	fmt.Fprintf(&b, "*%d\r\n", len(args))
	for _, a := range args {
		fmt.Fprintf(&b, "$%d\r\n%s\r\n", len(a), a)
	}
	return []byte(b.String())
}

// respOne reads one RESP reply; returns the string/int/simple form.
func respOne(r *bufio.Reader) (string, error) {
	line, err := r.ReadString('\n')
	if err != nil {
		return "", err
	}
	if len(line) < 3 {
		return "", fmt.Errorf("short RESP reply %q", line)
	}
	switch line[0] {
	case '+', ':':
		return strings.TrimRight(line[1:], "\r\n"), nil
	case '-':
		return "", fmt.Errorf("redis error: %s", strings.TrimRight(line[1:], "\r\n"))
	case '$':
		n, _ := strconv.Atoi(strings.TrimRight(line[1:], "\r\n"))
		if n < 0 {
			return "", nil // nil bulk
		}
		buf := make([]byte, n+2)
		if _, err := io.ReadFull(r, buf); err != nil {
			return "", err
		}
		return string(buf[:n]), nil
	case '*':
		n, _ := strconv.Atoi(strings.TrimRight(line[1:], "\r\n"))
		for i := 0; i < n; i++ {
			if _, err := respOne(r); err != nil {
				return "", err
			}
		}
		return fmt.Sprintf("<array %d>", n), nil
	}
	return "", fmt.Errorf("unexpected RESP type %q", line[0])
}

// redisCmd runs one command on the scratch DB (SELECT first when db != 0).
func redisCmd(t *testing.T, addr string, db int, args ...string) string {
	t.Helper()
	conn, err := net.DialTimeout("tcp", addr, 3*time.Second)
	if err != nil {
		t.Fatalf("redis dial %s: %v", addr, err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	r := bufio.NewReader(conn)
	if db != 0 {
		if _, err := conn.Write(respArgs("SELECT", strconv.Itoa(db))); err != nil {
			t.Fatalf("redis SELECT: %v", err)
		}
		if _, err := respOne(r); err != nil {
			t.Fatalf("redis SELECT %d: %v", db, err)
		}
	}
	if _, err := conn.Write(respArgs(args...)); err != nil {
		t.Fatalf("redis %v: %v", args, err)
	}
	v, err := respOne(r)
	if err != nil {
		t.Fatalf("redis %v: %v", args, err)
	}
	return v
}

func redisPing(t *testing.T, addr string) bool {
	conn, err := net.DialTimeout("tcp", addr, 2*time.Second)
	if err != nil {
		return false
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(3 * time.Second))
	if _, err := conn.Write(respArgs("PING")); err != nil {
		return false
	}
	v, err := respOne(bufio.NewReader(conn))
	return err == nil && v == "PONG"
}

// haltFlags SCANs one logical DB for every halt:* flag — scoped flags
// (halt:account:N, halt:env:…, …) matter to order admission just as
// much as halt:global. Returns the raw key list.
func haltFlags(t *testing.T, addr string, db int) []string {
	t.Helper()
	conn, err := net.DialTimeout("tcp", addr, 3*time.Second)
	if err != nil {
		t.Fatalf("redis dial for halt scan: %v", err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
	r := bufio.NewReader(conn)
	if _, err := conn.Write(respArgs("SELECT", strconv.Itoa(db))); err != nil {
		t.Fatalf("SELECT: %v", err)
	}
	if _, err := respOne(r); err != nil {
		t.Fatalf("SELECT %d: %v", db, err)
	}
	var keys []string
	cursor := "0"
	for {
		if _, err := conn.Write(respArgs("SCAN", cursor, "MATCH", "halt:*", "COUNT", "100")); err != nil {
			t.Fatalf("SCAN: %v", err)
		}
		line, err := r.ReadString('\n')
		if err != nil || len(line) == 0 || line[0] != '*' {
			t.Fatalf("SCAN reply: %v %q", err, line)
		}
		cursorHdr, _ := r.ReadString('\n') // $cursor bulk header
		cn, _ := strconv.Atoi(strings.TrimRight(cursorHdr[1:], "\r\n"))
		buf := make([]byte, cn+2)
		if _, err := io.ReadFull(r, buf); err != nil {
			t.Fatalf("SCAN cursor: %v", err)
		}
		cursor = string(buf[:cn])
		keysHdr, _ := r.ReadString('\n') // *keys array header
		kn, _ := strconv.Atoi(strings.TrimRight(keysHdr[1:], "\r\n"))
		for i := 0; i < kn; i++ {
			v, err := respOne(r)
			if err != nil {
				t.Fatalf("SCAN key: %v", err)
			}
			keys = append(keys, v)
		}
		if cursor == "0" {
			break
		}
	}
	return keys
}

// waitReconSweep blocks until the gateway's boot reconciliation sweep
// finishes — runLogged always writes a "reconciliation: run …" line
// (success AND failure paths). Without this, the test would race the
// sweep's fail-closed halt:* emissions on the dirty dev fixture data.
func waitReconSweep(t *testing.T, logPath string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		b, _ := os.ReadFile(logPath)
		if strings.Contains(string(b), "reconciliation: run") {
			return
		}
		time.Sleep(500 * time.Millisecond)
	}
	b, _ := os.ReadFile(logPath)
	t.Fatalf("boot reconciliation sweep never logged completion in %v\n"+
		"gateway log tail:\n%s", timeout, tailLog(string(b)))
}

// assertHaltStable verifies no halt:* flag exists on the scratch DB and
// none reappears for the window — catches an active writer (a second
// recon sweep, an auto-halt trip) before the legs run, instead of
// letting it silently re-collapse the order path mid-blast.
func assertHaltStable(t *testing.T, addr string, db int, window time.Duration) {
	t.Helper()
	deadline := time.Now().Add(window)
	for time.Now().Before(deadline) {
		if got := haltFlags(t, addr, db); len(got) > 0 {
			t.Fatalf("halt flags reappeared on db%d after the boot sweep "+
				"cleared: %v — another writer is active", db, got)
		}
		time.Sleep(150 * time.Millisecond)
	}
}

// ---------------------------------------------------------------------------
// Live gateway process.
// ---------------------------------------------------------------------------

func repoRoot(t *testing.T) string {
	t.Helper()
	if r := os.Getenv("EXC_REPO_ROOT"); r != "" {
		return r
	}
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for d := wd; ; d = filepath.Dir(d) {
		if _, err := os.Stat(filepath.Join(d, "AGENTS.md")); err == nil {
			if _, err := os.Stat(filepath.Join(d, "services", "go.mod")); err == nil {
				return d
			}
		}
		if filepath.Dir(d) == d {
			t.Fatal("repo root not found")
		}
	}
}

func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("free port: %v", err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

func b64Key(t *testing.T) string {
	t.Helper()
	k := make([]byte, 32)
	if _, err := rand.Read(k); err != nil {
		t.Fatalf("rand: %v", err)
	}
	return base64.StdEncoding.EncodeToString(k)
}

// startLiveGateway builds services/cmd/gateway and spawns it against the
// dev topology on a scratch Redis DB — env mirrors itest.StartGateway.
func startLiveGateway(t *testing.T, root, pgDSN, redisAddr string, redisDB int,
	natsURLs string, jwtKeyB64 string) (addr string, cmd *exec.Cmd, logPath string) {
	t.Helper()
	dir := t.TempDir()
	bin := filepath.Join(dir, "gateway")
	build := exec.Command("go", "build", "-o", bin, "./cmd/gateway")
	build.Dir = filepath.Join(root, "services")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("go build gateway: %v\n%s", err, out)
	}

	logPath = filepath.Join(dir, "gateway.log")
	lf, err := os.Create(logPath)
	if err != nil {
		t.Fatal(err)
	}
	cfgPath := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(cfgPath,
		[]byte("secrets:\n  data_key: "+b64Key(t)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	port := freePort(t)
	cmd = exec.Command(bin)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"EXC_CONFIG="+cfgPath,
		"EXC_POSTGRES_DSN="+pgDSN,
		"EXC_POSTGRES_MAX_CONNS=8",
		"EXC_REDIS_ADDR="+redisAddr,
		"EXC_REDIS_DB="+strconv.Itoa(redisDB),
		"EXC_NATS_URLS="+natsURLs,
		"EXC_GATEWAY_HOST=127.0.0.1",
		"EXC_GATEWAY_PORT="+strconv.Itoa(port),
		"EXC_SHARDING_CONFIG="+filepath.Join(root, "config", "sharding.yaml"),
		"EXC_JWT_HS256_KEY_B64="+jwtKeyB64,
		"EXC_DOCS_SECRET=errblast-doc-secret",
		"EXC_ENVIRONMENT=development",
		"EXC_LOGGING_FORMAT=text",
		// Stand in for the HAProxy edge so per-IP rate-limit/ban buckets
		// honor the blast's rotating X-Forwarded-For identities.
		"EXC_TRUST_PROXY=1",
	)
	cmd.Stdout, cmd.Stderr = lf, lf
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		lf.Close()
		t.Fatalf("gateway spawn: %v", err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Signal(syscall.SIGTERM)
		done := make(chan struct{})
		go func() { _, _ = cmd.Process.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(8 * time.Second):
			_ = cmd.Process.Kill()
			<-done
		}
		lf.Close()
	})
	return fmt.Sprintf("http://127.0.0.1:%d", port), cmd, logPath
}

// startLiveOracle builds cmd/oracle and spawns it in scripted-sim mode
// (EXC_ORACLE_SIM=1 — two independent SimFeeds), publishing the oracle
// health/mark keyspace into the scratch Redis DB. Required because order
// admission fail-closes PRICE_ORACLE_UNAVAILABLE when no fresh marks
// exist (spec §19.5; itest boots one for the same reason).
func startLiveOracle(t *testing.T, root, pgDSN, redisAddr string, redisDB int,
	symbols []string) {
	t.Helper()
	dir := t.TempDir()
	bin := filepath.Join(dir, "oracle")
	build := exec.Command("go", "build", "-o", bin, "./cmd/oracle")
	build.Dir = filepath.Join(root, "services")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("go build oracle: %v\n%s", err, out)
	}
	logPath := filepath.Join(dir, "oracle.log")
	lf, err := os.Create(logPath)
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(bin)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"EXC_ORACLE_SIM=1",
		"EXC_ORACLE_SYMBOLS="+strings.Join(symbols, ","),
		"EXC_POSTGRES_DSN="+pgDSN,
		"EXC_REDIS_ADDR="+redisAddr,
		"EXC_REDIS_DB="+strconv.Itoa(redisDB),
		"EXC_LOGGING_FORMAT=text",
	)
	cmd.Stdout, cmd.Stderr = lf, lf
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		lf.Close()
		t.Fatalf("oracle spawn: %v", err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Signal(syscall.SIGTERM)
		done := make(chan struct{})
		go func() { _, _ = cmd.Process.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			_ = cmd.Process.Kill()
			<-done
		}
		lf.Close()
	})

	// Wait for fresh marks: oracle:health:{symbol} = OK|DEGRADED.
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if err := cmd.Process.Signal(syscall.Signal(0)); err != nil {
			b, _ := os.ReadFile(logPath)
			t.Fatalf("oracle exited before healthy: %v\n%s", err, tailLog(string(b)))
		}
		v := redisCmd(t, redisAddr, redisDB, "GET", "oracle:health:"+symbols[0])
		if v == "OK" || v == "DEGRADED" {
			return
		}
		time.Sleep(250 * time.Millisecond)
	}
	b, _ := os.ReadFile(logPath)
	t.Fatalf("oracle never healthy for %s\n%s", symbols[0], tailLog(string(b)))
}

func waitLive(t *testing.T, addr string, cmd *exec.Cmd, logPath string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		resp, err := http.Get(addr + "/health/live")
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return
			}
		}
		if err := cmd.Process.Signal(syscall.Signal(0)); err != nil {
			b, _ := os.ReadFile(logPath)
			t.Fatalf("gateway exited before ready: %v\n%s", err, tailLog(string(b)))
		}
		time.Sleep(150 * time.Millisecond)
	}
	b, _ := os.ReadFile(logPath)
	t.Fatalf("gateway not ready in %s\n%s", timeout, tailLog(string(b)))
}

func tailLog(s string) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) > 40 {
		lines = lines[len(lines)-40:]
	}
	return strings.Join(lines, "\n")
}

// ---------------------------------------------------------------------------
// The live test.
// ---------------------------------------------------------------------------

func TestLiveGatewayErrorBlast(t *testing.T) {
	if os.Getenv("EXC_ERRBLAST_LIVE") != "1" {
		t.Skip("set EXC_ERRBLAST_LIVE=1 to run the live-gateway blast " +
			"(needs the dev topology: PG 5433, Redis 16379, NATS 4222)")
	}
	root := repoRoot(t)
	redisAddr := os.Getenv("EXC_REDIS_TEST_ADDR")
	if redisAddr == "" {
		redisAddr = "127.0.0.1:16379"
	}
	pgDSN := os.Getenv("EXC_PG_DSN")
	if pgDSN == "" {
		pgDSN = "postgres://exchange:exchange_dev@127.0.0.1:5433/exchange?sslmode=disable"
	}
	natsURLs := os.Getenv("EXC_NATS_URLS")
	if natsURLs == "" {
		natsURLs = "nats://127.0.0.1:4222"
	}
	if !redisPing(t, redisAddr) {
		t.Skipf("dev Redis unreachable at %s", redisAddr)
	}
	if c, err := net.DialTimeout("tcp", "127.0.0.1:5433", 2*time.Second); err != nil {
		t.Skipf("dev Postgres unreachable at 127.0.0.1:5433: %v", err)
	} else {
		c.Close()
	}

	// Scratch DB 14 — the integration suite's convention is DB 9, and a
	// stale halt:* flag or system:degradation:* record there fail-closes
	// every order admission (TRADING_HALTED/DEGRADED_MODE), collapsing
	// every defect class onto the same 503 path. A less-shared index
	// plus an explicit sweep of the enforcement keys keeps the blast on
	// the validation paths it targets; teardown clears them again.
	const redisDB = 14
	jwtKey := b64Key(t)
	symbols := []string{"EUR/USD", "GBP/USD", "USD/JPY", "AUD/USD"}

	clearHalt := func() {
		if stale := haltFlags(t, redisAddr, redisDB); len(stale) > 0 {
			args := append([]string{"DEL"}, stale...)
			redisCmd(t, redisAddr, redisDB, args...)
			t.Logf("cleared %d stale halt flag(s) on db%d: %v",
				len(stale), redisDB, stale)
		}
		redisCmd(t, redisAddr, redisDB, "DEL",
			"system:degradation:mode", "system:degradation:entered_at",
			"system:degradation:reason")
	}
	clearHalt()

	startLiveOracle(t, root, pgDSN, redisAddr, redisDB, symbols)
	addr, gw, logPath := startLiveGateway(t, root, pgDSN, redisAddr, redisDB, natsURLs, jwtKey)
	waitLive(t, addr, gw, logPath, 60*time.Second)
	t.Cleanup(clearHalt)

	// The gateway's Phase-13 reconciliation engine runs its first sweep
	// AT BOOT (RunScheduler → runLogged before the first tick). On the
	// dev fixture data it legitimately finds mismatches and emits
	// halt:* flags + ACTIVE trading_suspensions rows — a REAL
	// fail-closed halt that collapses every order-admission request
	// onto 503 TRADING_HALTED and hides the defect classes the blast
	// exercises. Wait for that sweep to finish (its runLogged line hits
	// the log), clear the emitted flags once, then assert they stay
	// clear — the next scheduler tick is ~1h out, so a re-raise inside
	// the stability window means another writer is active.
	waitReconSweep(t, logPath, 90*time.Second)
	clearHalt()
	assertHaltStable(t, redisAddr, redisDB, 3*time.Second)

	// ---- Leg A: mode record → wire-visible enforcement ---------------------
	token, err := mintJWT(jwtKey, "errblast-live", 1, []string{"trade", "read"})
	if err != nil {
		t.Fatalf("mintJWT: %v", err)
	}
	validOrder := orderJSON("EUR/USD", "BUY", "1.08500", "1000")

	redisCmd(t, redisAddr, redisDB, "MSET",
		"system:degradation:mode", "ReadOnly",
		"system:degradation:entered_at", strconv.FormatInt(time.Now().UnixMilli(), 10),
		"system:degradation:reason", "errblast_live_leg")

	// Order placement rejects DEGRADED_MODE while the mode holds.
	req, _ := http.NewRequest(http.MethodPost, addr+"/api/v1/orders",
		strings.NewReader(string(validOrder)))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Idempotency-Key", newIDemKey(mrand.New(mrand.NewSource(1))))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST /orders under ReadOnly: %v", err)
	}
	body := make([]byte, 2048)
	n, _ := resp.Body.Read(body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("ReadOnly POST /orders status = %d, want 503 (body %s)",
			resp.StatusCode, body[:n])
	}
	if got := resp.Header.Get("X-Degradation-Mode"); got != "ReadOnly" {
		t.Fatalf("X-Degradation-Mode = %q, want ReadOnly", got)
	}

	// Market-data reads keep flowing (spec §2.4 ReadOnly column: reads,
	// book queries, market data continue).
	for _, path := range []string{"/api/v1/time", "/api/v1/instruments"} {
		r2, err := http.Get(addr + path)
		if err != nil {
			t.Fatalf("GET %s under ReadOnly: %v", path, err)
		}
		r2.Body.Close()
		if r2.StatusCode == http.StatusServiceUnavailable {
			t.Fatalf("GET %s = 503 under ReadOnly — reads must continue", path)
		}
	}

	// Clearing the record restores write-path evaluation (the POST is
	// still rejected — auth/validation — but NOT by the mode gate).
	redisCmd(t, redisAddr, redisDB, "DEL",
		"system:degradation:mode", "system:degradation:entered_at",
		"system:degradation:reason")
	req, _ = http.NewRequest(http.MethodPost, addr+"/api/v1/orders",
		strings.NewReader(`{"symbol":"EUR/USD"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Idempotency-Key", newIDemKey(mrand.New(mrand.NewSource(2))))
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST /orders after clear: %v", err)
	}
	body = make([]byte, 4096)
	n, _ = resp.Body.Read(body)
	resp.Body.Close()
	if resp.StatusCode == http.StatusServiceUnavailable &&
		(strings.Contains(string(body[:n]), "DEGRADED_MODE") ||
			strings.Contains(string(body[:n]), "MAINTENANCE_MODE")) {
		t.Fatalf("mode gate still rejecting after the record cleared: %s",
			body[:n])
	}
	// Any other status (400/401/403/409/422 — or a fail-closed 503 from a
	// deeper gate, e.g. PRICE_ORACLE_UNAVAILABLE) is a clean rejection:
	// the mode-gate leg only asserts the degradation record's effect.
	t.Logf("POST /orders after clear: status=%d body=%.200s",
		resp.StatusCode, body[:n])

	// ---- Leg B: the real burst ----------------------------------------------
	// A genuinely executed error-injection run against the live binary —
	// dev-scale rate (the staging 10k/s gate is the orchestrator's), same
	// defect mix, same verdict policy.
	cfg := Config{
		Addr:         addr,
		Rate:         1_500,
		Duration:     4 * time.Second,
		Workers:      32,
		ControlRate:  10,
		ReqTimeout:   3 * time.Second,
		StartupGrace: 0, // gateway already proven live
		Max5xxPct:    1.0,
		JWTKeyB64:    jwtKey,
		JWTAccounts:  512,
		XFFPool:      512,
		Seed:         99,
	}
	rep, code := Run(context.Background(), cfg)
	t.Logf("blast: sent=%d 4xx=%d 429=%d 5xx=%d timeout=%d transport=%d "+
		"control ok=%d failed=%d | statuses=%v modes=%v",
		rep.Totals.Sent, rep.Totals.R4xx, rep.Totals.R429, rep.Totals.R5xx,
		rep.Totals.Timeouts, rep.Totals.Transport,
		rep.Control.OK, rep.Control.Failed+rep.Control.NonOK,
		rep.Statuses, rep.Modes)

	// Process still alive after the storm (no crash, no wedged listener).
	if err := gw.Process.Signal(syscall.Signal(0)); err != nil {
		t.Fatalf("gateway dead after blast: %v", err)
	}
	if rep.Totals.Sent < 1_000 {
		t.Fatalf("blast produced too little traffic: %d", rep.Totals.Sent)
	}
	if code != 0 {
		b, _ := os.ReadFile(logPath)
		t.Fatalf("blast verdict = %d (%v)\ngateway log tail:\n%s",
			code, rep.Reasons, tailLog(string(b)))
	}
	// The mode-reporting surface stayed live through the storm.
	if rep.Modes["Normal"] == 0 {
		t.Fatalf("X-Degradation-Mode header missing/never Normal: %v", rep.Modes)
	}
}
