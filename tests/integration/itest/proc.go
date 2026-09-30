// Process lifecycle for the live-stack e2e: matching_engine shards and
// the gateway binary, spawned as real processes against the scratch
// dependencies (no docker required — same binaries, same wire paths).
package itest

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// ---------------------------------------------------------------------------
// Engine
// ---------------------------------------------------------------------------

// EngineProc is one running matching_engine shard.
type EngineProc struct {
	Shard   int
	Cmd     *exec.Cmd
	WALDir  string // <dir>/<shard>/ — wal segments + engine.lock
	SnapDir string
	LogPath string
}

// StartEngine spawns `matching_engine -shard N -instrument-id M
// -dev-all-accounts` writing under dir. dev-all-accounts is the engine's
// own dev/soak account provider (explicit opt-in flag, fails closed
// without it); the gateway's signed-request + pre-trade boundary is
// still exercised end to end.
func StartEngine(ctx context.Context, e *Env, dir string, shard, instrumentID int) (*EngineProc, error) {
	walDir := filepath.Join(dir, "wal")
	snapDir := filepath.Join(dir, "snap")
	poison := filepath.Join(dir, fmt.Sprintf("poison-%d.log", shard))
	reportLog := filepath.Join(dir, fmt.Sprintf("recovery-%d.jsonl", shard))
	logPath := filepath.Join(dir, fmt.Sprintf("engine-%d.log", shard))
	lf, err := os.Create(logPath)
	if err != nil {
		return nil, err
	}
	cmd := exec.Command(e.CoreBin,
		"-shard", strconv.Itoa(shard),
		"-instrument-id", strconv.Itoa(instrumentID),
		"-dev-all-accounts",
		"-wal-dir", walDir,
		"-snap-dir", snapDir,
		"-poison-log", poison,
		"-report-log", reportLog)
	cmd.Stdout, cmd.Stderr = lf, lf
	// Own process group: the suite can signal/kill without touching
	// unrelated engine processes on this host.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		lf.Close()
		return nil, fmt.Errorf("engine shard %d: %w", shard, err)
	}
	p := &EngineProc{Shard: shard, Cmd: cmd, WALDir: filepath.Join(walDir, strconv.Itoa(shard)), SnapDir: snapDir, LogPath: logPath}
	return p, nil
}

// WaitReady tails the engine log until "shard N ready" or timeout.
func (p *EngineProc) WaitReady(ctx context.Context, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	needle := fmt.Sprintf("shard %d ready", p.Shard)
	for time.Now().Before(deadline) {
		b, _ := os.ReadFile(p.LogPath)
		if strings.Contains(string(b), needle) {
			return nil
		}
		if err := p.Cmd.Process.Signal(syscall.Signal(0)); err != nil {
			return fmt.Errorf("engine shard %d exited before ready: %w", p.Shard, err)
		}
		time.Sleep(50 * time.Millisecond)
	}
	return fmt.Errorf("engine shard %d not ready in %s (log %s)", p.Shard, timeout, p.LogPath)
}

// Signal sends sig to the engine process group leader.
func (p *EngineProc) Signal(sig syscall.Signal) error {
	return p.Cmd.Process.Signal(sig)
}

// Stop SIGTERMs and waits briefly.
func (p *EngineProc) Stop() {
	_ = p.Cmd.Process.Signal(syscall.SIGTERM)
	done := make(chan struct{})
	go func() { _, _ = p.Cmd.Process.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		_ = p.Cmd.Process.Kill()
		<-done
	}
}

// ---------------------------------------------------------------------------
// Gateway
// ---------------------------------------------------------------------------

// GatewayProc is a running services/cmd/gateway binary.
type GatewayProc struct {
	Cmd     *exec.Cmd
	Addr    string
	LogPath string
}

// StackEnv builds the environment the gateway is spawned with. A fresh
// 32-byte HS256 key is generated per stack unless jwtKeyB64 is set —
// the suite returns it so tests can mint matching tokens.
type StackEnv struct {
	JWTKeyB64 string
}

// BuildGateway compiles cmd/gateway into dir once per suite run.
func BuildGateway(ctx context.Context, e *Env, dir string) (string, error) {
	return BuildCmd(ctx, e, "./cmd/gateway", "gateway", dir)
}

// BuildCmd compiles a services/ cmd package into dir under name.
func BuildCmd(ctx context.Context, e *Env, pkg, name, dir string) (string, error) {
	out := filepath.Join(dir, name)
	c := exec.CommandContext(ctx, "go", "build", "-o", out, pkg)
	c.Dir = e.ServicesDir
	b, err := c.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("go build %s: %w\n%s", pkg, err, tail(string(b), 30))
	}
	return out, nil
}

// StartGateway spawns the gateway binary against the scratch
// dependencies. jwtKeyB64/dataKeyB64 are generated fresh per stack so
// the suite never depends on stored secrets — dataKey seals the seeded
// api_keys rows (SecretBox wire shape matches auth.SecretBox).
func StartGateway(ctx context.Context, e *Env, bin, dir string, port int, jwtKeyB64, dataKeyB64 string) (*GatewayProc, error) {
	logPath := filepath.Join(dir, "gateway.log")
	lf, err := os.Create(logPath)
	if err != nil {
		return nil, err
	}
	// config.yaml carries secrets.data_key — viper's AutomaticEnv does
	// NOT apply to keys without a registered default on Unmarshal, so
	// EXC_SECRETS_DATA_KEY alone would be silently ignored and the
	// gateway would fall back to the deterministic dev key (orphaning
	// the suite-sealed api_keys rows). DSN/Redis/NATS envs DO have
	// defaults, so the env overrides take.
	cfgPath := filepath.Join(dir, "config.yaml")
	cfgYAML := "secrets:\n  data_key: " + dataKeyB64 + "\n"
	if err := os.WriteFile(cfgPath, []byte(cfgYAML), 0o600); err != nil {
		return nil, err
	}
	env := append(os.Environ(),
		"EXC_CONFIG="+cfgPath,
		"EXC_POSTGRES_DSN="+e.PostgresDSN,
		"EXC_POSTGRES_MAX_CONNS=8",
		"EXC_REDIS_ADDR="+e.RedisAddr,
		"EXC_REDIS_DB="+strconv.Itoa(e.RedisDB),
		"EXC_NATS_URLS="+strings.Join(e.NatsURLs, ","),
		"EXC_GATEWAY_HOST=127.0.0.1",
		"EXC_GATEWAY_PORT="+strconv.Itoa(port),
		"EXC_SHARDING_CONFIG="+filepath.Join(e.Root, "config", "sharding.yaml"),
		"EXC_JWT_HS256_KEY_B64="+jwtKeyB64,
		"EXC_SECRETS_DATA_KEY="+dataKeyB64,
		"EXC_ENVIRONMENT=development",
		"EXC_LOGGING_FORMAT=text",
		// The harness stands in for the HAProxy edge — per-test XFF
		// identities isolate public-tier rate-limit buckets so one
		// leg's deliberate abuse cannot poison every other leg.
		"EXC_TRUST_PROXY=1",
	)
	cmd := exec.Command(bin)
	cmd.Dir = dir
	cmd.Env = env
	cmd.Stdout, cmd.Stderr = lf, lf
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		lf.Close()
		return nil, fmt.Errorf("gateway spawn: %w", err)
	}
	g := &GatewayProc{Cmd: cmd, Addr: fmt.Sprintf("http://127.0.0.1:%d", port), LogPath: logPath}
	return g, nil
}

// WaitReady polls GET /health/live until 200 or timeout.
func (g *GatewayProc) WaitReady(ctx context.Context, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, g.Addr+"/health/live", nil)
		resp, err := http.DefaultClient.Do(req)
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return nil
			}
		}
		if err := g.Cmd.Process.Signal(syscall.Signal(0)); err != nil {
			b, _ := os.ReadFile(g.LogPath)
			return fmt.Errorf("gateway exited before ready: %w\n%s", err, tail(string(b), 30))
		}
		time.Sleep(100 * time.Millisecond)
	}
	b, _ := os.ReadFile(g.LogPath)
	return fmt.Errorf("gateway not ready in %s\n%s", timeout, tail(string(b), 30))
}

// WaitShardsReady polls /ready until status ok or timeout (all engine
// shards + deps up).
func (g *GatewayProc) WaitShardsReady(ctx context.Context, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, g.Addr+"/ready", nil)
		resp, err := http.DefaultClient.Do(req)
		if err == nil {
			var body struct {
				Status string `json:"status"`
			}
			_ = json.NewDecoder(resp.Body).Decode(&body)
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK && body.Status == "ok" {
				return nil
			}
		}
		time.Sleep(200 * time.Millisecond)
	}
	return fmt.Errorf("/ready did not reach ok in %s", timeout)
}

// Stop SIGTERMs the gateway.
func (g *GatewayProc) Stop() {
	_ = g.Cmd.Process.Signal(syscall.SIGTERM)
	done := make(chan struct{})
	go func() { _, _ = g.Cmd.Process.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(8 * time.Second):
		_ = g.Cmd.Process.Kill()
		<-done
	}
}

// ---------------------------------------------------------------------------
// Oracle (Phase-19.5 — required stack component: margin order admission
// fails closed on PRICE_ORACLE_UNAVAILABLE when no oracle publishes)
// ---------------------------------------------------------------------------

// OracleProc is a running services/cmd/oracle binary.
type OracleProc struct {
	Cmd     *exec.Cmd
	LogPath string
}

// StartOracle spawns the oracle in scripted-sim mode (EXC_ORACLE_SIM=1 —
// two independent SimFeeds self-driven by cmd/oracle's jitter ticker)
// publishing the contracted keyspace into the suite's scratch Redis DB.
// symbols overrides the instruments-table universe so the oracle boots
// before the fixture seeds rows.
func StartOracle(ctx context.Context, e *Env, dir string, symbols []string) (*OracleProc, error) {
	bin, err := BuildCmd(ctx, e, "./cmd/oracle", "oracle", dir)
	if err != nil {
		return nil, err
	}
	logPath := filepath.Join(dir, "oracle.log")
	lf, err := os.Create(logPath)
	if err != nil {
		return nil, err
	}
	cmd := exec.CommandContext(ctx, bin)
	cmd.Env = append(os.Environ(),
		"EXC_ORACLE_SIM=1",
		"EXC_ORACLE_SYMBOLS="+strings.Join(symbols, ","),
		"EXC_POSTGRES_DSN="+e.PostgresDSN,
		"EXC_REDIS_ADDR="+e.RedisAddr,
		"EXC_REDIS_DB="+strconv.Itoa(e.RedisDB),
		"EXC_LOGGING_FORMAT=text",
	)
	cmd.Dir = dir
	cmd.Stdout, cmd.Stderr = lf, lf
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		lf.Close()
		return nil, fmt.Errorf("oracle spawn: %w", err)
	}
	return &OracleProc{Cmd: cmd, LogPath: logPath}, nil
}

// WaitHealthy polls Redis until oracle:health:{symbol} reports OK or
// DEGRADED (fresh marks published) or timeout.
func (o *OracleProc) WaitHealthy(ctx context.Context, e *Env, symbol string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if err := o.Cmd.Process.Signal(syscall.Signal(0)); err != nil {
			b, _ := os.ReadFile(o.LogPath)
			return fmt.Errorf("oracle exited before healthy: %w\n%s", err, tail(string(b), 30))
		}
		rdb := e.RedisClient()
		if rdb != nil {
			v, err := rdb.Get(ctx, "oracle:health:"+symbol).Result()
			rdb.Close()
			if err == nil && (v == "OK" || v == "DEGRADED") {
				return nil
			}
		}
		time.Sleep(150 * time.Millisecond)
	}
	b, _ := os.ReadFile(o.LogPath)
	return fmt.Errorf("oracle:health:%s never reached OK in %s\n%s",
		symbol, timeout, tail(string(b), 30))
}

// Stop SIGTERMs the oracle.
func (o *OracleProc) Stop() {
	_ = o.Cmd.Process.Signal(syscall.SIGTERM)
	done := make(chan struct{})
	go func() { _, _ = o.Cmd.Process.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		_ = o.Cmd.Process.Kill()
		<-done
	}
}

// FreePort asks the kernel for a free TCP port.
func FreePort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port, nil
}

// GenKeyB64 returns a fresh base64-encoded 32-byte key.
func GenKeyB64() string {
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	return base64.StdEncoding.EncodeToString(b)
}

// GenKeyBytes returns fresh 32-byte key material.
func GenKeyBytes() ([]byte, error) {
	b := make([]byte, 32)
	_, err := rand.Read(b)
	return b, err
}

// B64 standard-base64-encodes b.
func B64(b []byte) string { return base64.StdEncoding.EncodeToString(b) }

// LogContains reports whether a spawned process's log contains needle.
func LogContains(path, needle string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		if strings.Contains(sc.Text(), needle) {
			return true
		}
	}
	return false
}
