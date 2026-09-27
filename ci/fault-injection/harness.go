// Negative assertion engine helpers for the fault-injection harness
// (Task 1.5.3.5, spec §2.7 fail-closed invariants).

package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	stderrors "errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"

	excerrors "exchange/pkg/errors"
)

// Check is one assertion inside a scenario.
type Check struct {
	Name   string `json:"name"`
	Pass   bool   `json:"pass"`
	Detail string `json:"detail,omitempty"`
}

// Checks accumulates assertions for one scenario run.
type Checks struct{ list []Check }

func (c *Checks) ok(name string, cond bool, detail ...string) {
	c.list = append(c.list, Check{Name: name, Pass: cond, Detail: strings.Join(detail, " ")})
}

// okf is ok with a printf-style detail (only evaluated for the message).
func (c *Checks) okf(name string, cond bool, format string, args ...any) {
	c.ok(name, cond, fmt.Sprintf(format, args...))
}

func (c *Checks) info(name, detail string) {
	c.list = append(c.list, Check{Name: name, Pass: true, Detail: detail})
}

// allPass reports whether every recorded check passed.
func (c Checks) allPass() bool {
	for _, ch := range c.list {
		if !ch.Pass {
			return false
		}
	}
	return len(c.list) > 0
}

// --- coded-error assertions ---------------------------------------------------

// errCode extracts the *errors.Error code, or "".
func errCode(err error) string {
	var e *excerrors.Error
	if stderrors.As(err, &e) {
		return e.Code
	}
	return ""
}

// pgSQLSTATE extracts the Postgres SQLSTATE from an error chain, or "".
func pgSQLSTATE(err error) string {
	type sqlStater interface{ SQLState() string }
	for e := err; e != nil; e = stderrors.Unwrap(e) {
		if s, ok := e.(sqlStater); ok {
			return s.SQLState()
		}
	}
	// pgconn.PgError exposes Code (not SQLState()); handled by caller via
	// errors.As — kept here for wrapped fmt.Errorf chains.
	var pgErr interface{ SQLState() string }
	_ = pgErr
	return ""
}

// --- subprocess runner ----------------------------------------------------------

// cmdResult captures one subprocess invocation.
type cmdResult struct {
	Stdout   string
	Stderr   string
	ExitCode int
	Signaled bool
	Signal   string
	Dur      time.Duration
	Err      error // launch/Wait failure itself
}

// runExec runs argv; nonzero exit codes are DATA (the harness asserts on
// them), never harness failures.
func runExec(ctx context.Context, dir string, argv ...string) cmdResult {
	return runExecEnv(ctx, dir, nil, argv...)
}

// runExecEnv is runExec with extra environment entries appended to the
// inherited environment.
func runExecEnv(ctx context.Context, dir string, extraEnv []string, argv ...string) cmdResult {
	start := time.Now()
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	if dir != "" {
		cmd.Dir = dir
	}
	if len(extraEnv) > 0 {
		cmd.Env = append(os.Environ(), extraEnv...)
	}
	var so, se bytes.Buffer
	cmd.Stdout, cmd.Stderr = &so, &se
	err := cmd.Run()
	res := cmdResult{
		Stdout: strings.TrimSpace(so.String()),
		Stderr: strings.TrimSpace(se.String()),
		Dur:    time.Since(start),
	}
	if err != nil {
		res.ExitCode = -1
		var ee *exec.ExitError
		if stderrors.As(err, &ee) {
			res.ExitCode = ee.ExitCode()
		} else {
			res.Err = err
		}
	}
	if cmd.ProcessState != nil {
		if ws, ok := cmd.ProcessState.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
			res.Signaled = true
			res.Signal = ws.Signal().String()
		}
	}
	return res
}

// decodeJSON unmarshals a JSON object; returns nil on failure.
func decodeJSON(s string) map[string]any {
	var m map[string]any
	if err := json.Unmarshal([]byte(s), &m); err != nil {
		return nil
	}
	return m
}

// jstr/jnum/jbool are tolerant JSON-map accessors for walverify output.
func jstr(m map[string]any, k string) string {
	if m == nil {
		return ""
	}
	if v, ok := m[k].(string); ok {
		return v
	}
	return ""
}

func jnum(m map[string]any, k string) int64 {
	if m == nil {
		return -1
	}
	if v, ok := m[k].(float64); ok {
		return int64(v)
	}
	return -1
}

func jbool(m map[string]any, k string) bool {
	if m == nil {
		return false
	}
	v, _ := m[k].(bool)
	return v
}

func jarr(m map[string]any, k string) []any {
	if m == nil {
		return nil
	}
	v, _ := m[k].([]any)
	return v
}

// sha256Of returns the hex SHA-256 of b — used for file-image mutation checks.
func sha256Of(b []byte) [32]byte { return sha256.Sum256(b) }

// lastJSONLine parses the last non-empty stdout line into a map — the
// walverify contract is exactly one JSON object per invocation.
func lastJSONLine(stdout string) map[string]any {
	line := strings.TrimSpace(stdout)
	if i := strings.LastIndexByte(line, '\n'); i >= 0 {
		line = line[i+1:]
	}
	return decodeJSON(line)
}

// panicGuard runs fn and converts an escaped panic into a check failure
// string instead of letting it crash the harness process.
func panicGuard(fn func() error) (err error, panicked any) {
	defer func() {
		if r := recover(); r != nil {
			panicked = r
		}
	}()
	err = fn()
	return
}
