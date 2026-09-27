// Audit hash-chain tamper scenario (L0 tier): mutating a stored row must be
// detected by both the in-process verifier (audit.VerifyThrough) and the
// operator CLI (`exchange verify-audit` exit code 2 — the AUDIT_HASH_CORRUPTION
// / AUDIT_CHAIN_BROKEN halt path of spec §5.8). The probe restores the row
// afterwards so the dev chain stays clean.

package main

import (
	"context"
	"strings"
	"time"

	"exchange/internal/audit"
	"exchange/internal/db"
)

func scenarioAuditTamper(ctx context.Context, e *env) *Checks {
	c := &Checks{}

	pool, err := db.NewPool(ctx, e.pgDSN, 4)
	if err != nil {
		c.ok("audit:connect", false, err.Error())
		return c
	}
	defer pool.Close()
	if err := pool.Ping(ctx); err != nil {
		c.ok("audit:ping", false, err.Error())
		return c
	}
	verify := func() (audit.VerifyReport, error) {
		return audit.VerifyThrough(ctx, pool, time.Now().UTC().AddDate(0, 0, 1), nil)
	}

	// Baseline: chain must currently verify (otherwise probes would be
	// meaningless — a dirty baseline is a harness failure, not a pass).
	rep, err := verify()
	c.okf("audit:baseline_clean", err == nil && rep.OK(),
		"violations=%v err=%v", rep.Violations, err)
	if err != nil || !rep.OK() {
		return c
	}

	// Two probe rows — tampering row N must break N's payload_hash AND the
	// prev_hash link of N+1.
	e1, err := audit.AppendAuto(ctx, pool, "fault_probe_tamper", nil, "INSERT", nil)
	if err != nil {
		c.ok("audit:probe_append_1", false, err.Error())
		return c
	}
	if _, err := audit.AppendAuto(ctx, pool, "fault_probe_tamper", nil, "INSERT", nil); err != nil {
		c.ok("audit:probe_append_2", false, err.Error())
		return c
	}
	rep, err = verify()
	c.okf("audit:probes_verify", err == nil && rep.OK(),
		"violations=%v", rep.Violations)

	// Tamper: overwrite the probe row's payload_hash with a constant.
	var origHash string
	err = pool.QueryRow(ctx,
		"SELECT payload_hash FROM audit_hash_chain WHERE sequence_num=$1",
		e1.SequenceNum).Scan(&origHash)
	if err != nil {
		c.ok("audit:read_probe", false, err.Error())
		return c
	}
	_, err = pool.Exec(ctx,
		"UPDATE audit_hash_chain SET payload_hash=$1 WHERE sequence_num=$2",
		strings.Repeat("0", 64), e1.SequenceNum)
	if err != nil {
		c.ok("audit:tamper", false, err.Error())
		return c
	}

	rep, err = verify()
	c.okf("audit:tamper_detected", err == nil && !rep.OK() && len(rep.Violations) > 0,
		"violations=%v err=%v", rep.Violations, err)

	// CLI path: verify-audit must exit 2 with integrity-violation output.
	// The child resolves its DSN via EXC_POSTGRES_DSN — pin it to the
	// harness's -pg-dsn so both see the same database.
	res := runExecEnv(ctx, "", []string{"EXC_POSTGRES_DSN=" + e.pgDSN},
		e.exchange, "verify-audit", "--date", time.Now().UTC().Format("2006-01-02"))
	c.okf("audit:cli_exit_2", res.Err == nil && res.ExitCode == 2 &&
		strings.Contains(res.Stdout, "FAILED"),
		"exit=%d stdout=%q stderr=%q err=%v", res.ExitCode, res.Stdout, res.Stderr, res.Err)

	// Restore the row and re-verify — the probe leaves the chain whole.
	_, err = pool.Exec(ctx,
		"UPDATE audit_hash_chain SET payload_hash=$1 WHERE sequence_num=$2",
		origHash, e1.SequenceNum)
	c.okf("audit:restored", err == nil, "err=%v", err)
	rep, err = verify()
	c.okf("audit:restored_verifies", err == nil && rep.OK(),
		"violations=%v err=%v", rep.Violations, err)
	res = runExecEnv(ctx, "", []string{"EXC_POSTGRES_DSN=" + e.pgDSN},
		e.exchange, "verify-audit", "--date", time.Now().UTC().Format("2006-01-02"))
	c.okf("audit:cli_exit_0_clean", res.Err == nil && res.ExitCode == 0,
		"exit=%d stdout=%q", res.ExitCode, res.Stdout)

	return c
}
