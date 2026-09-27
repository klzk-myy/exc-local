// WAL fault scenarios (L0 tier): corrupt headers, CRC flips, torn tails,
// wrong-shard attach, sequence regression, and the kill -9 crash/recovery
// replay check.

package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
)

const walShard = 7 // arbitrary shard for fault scenarios

func walPath(e *env, scen, name string) string {
	return filepath.Join(e.workDir(scen), name)
}

// scenarioWalCorrupt drives the corrupt-WAL corpus:
//
//	crc_flip        payload byte of entry 2 flipped (no CRC update)
//	magic_corrupt   magic word destroyed -> BadHeader halt, file unmutated
//	version_corrupt version destroyed -> BadHeader halt
//	wrong_shard     valid segment opened with foreign shard id -> BadHeader
//	torn_tail       partial record appended after last commit
//	short_header    file truncated inside the 8-byte file header
//	seq_regression  explicit seq < tail / kWalPadSeq -> SeqRegression
func scenarioWalCorrupt(ctx context.Context, e *env) *Checks {
	c := &Checks{}

	mustWrite := func(name string, n int) (path string, out map[string]any, res cmdResult) {
		path = walPath(e, "wal_corrupt", name)
		out, res = e.wv(ctx, "write", path, fmt.Sprint(walShard), fmt.Sprint(n))
		c.okf(name+":write_ok", res.Err == nil && jstr(out, "status") == "Ok",
			"stdout=%q err=%v", res.Stdout, res.Err)
		return
	}
	mustScan := func(name, path string) map[string]any {
		out, res := e.wv(ctx, "scan", path)
		c.okf(name+":scan_ran", res.Err == nil && out != nil,
			"stdout=%q err=%v", res.Stdout, res.Err)
		return out
	}
	mustRecover := func(name, path string) (map[string]any, cmdResult) {
		out, res := e.wv(ctx, "recover", path, fmt.Sprint(walShard),
			walPath(e, "wal_corrupt", name+".report.json"))
		c.okf(name+":recover_ran", res.Err == nil && out != nil,
			"stdout=%q err=%v", res.Stdout, res.Err)
		return out, res
	}
	checksum := func(p string) string {
		b, err := os.ReadFile(p)
		if err != nil {
			return ""
		}
		return fmt.Sprintf("%x", sha256Of(b))
	}

	// --- crc_flip: corruption inside a committed entry ----------------------
	if p, w, _ := mustWrite("crc_flip.wal", 6); jstr(w, "status") == "Ok" {
		offsets := jarr(w, "offsets")
		c.okf("crc_flip:offsets_present", len(offsets) >= 6, "write=%v", w)
		entryOff := int64(0)
		if len(offsets) >= 6 {
			entryOff = int64(offsets[2].(float64)) // entry seq=2
		} else {
			return c
		}
		// Flip a payload byte (entry header is 21B; payload starts at +21).
		_, tr := e.wv(ctx, "tamper", p, fmt.Sprint(entryOff+21), "1", "ff")
		c.okf("crc_flip:tamper", tr.Err == nil, "stdout=%q", tr.Stdout)

		s := mustScan("crc_flip", p)
		c.ok("crc_flip:detected_not_silent", jbool(s, "corrupt"),
			fmt.Sprintf("scan=%v", s))
		c.okf("crc_flip:prefix_intact", jnum(s, "entries") == 2,
			"entries=%d want 2", jnum(s, "entries"))
		c.okf("crc_flip:corrupt_at_entry2", jnum(s, "corrupt_offset") == entryOff,
			"corrupt_offset=%d want %d", jnum(s, "corrupt_offset"), entryOff)

		rc, res := mustRecover("crc_flip", p)
		c.okf("crc_flip:recover_truncates", jstr(rc, "outcome") == "truncated_torn_tail" &&
			jnum(rc, "entries") == 2 && res.ExitCode == 0,
			"recover=%v rc=%d", rc, res.ExitCode)
		s2 := mustScan("crc_flip_post", p)
		c.okf("crc_flip:post_repair_clean", jstr(s2, "end_step") == "End" &&
			!jbool(s2, "corrupt") && jnum(s2, "entries") == 2 &&
			jbool(s2, "payload_ok"), "scan=%v", s2)
	}

	// --- magic_corrupt: fail-closed halt, zero mutation ---------------------
	if p, w, _ := mustWrite("magic_corrupt.wal", 4); jstr(w, "status") == "Ok" {
		_, _ = e.wv(ctx, "tamper", p, "0", "1", "ff")
		tampered := checksum(p) // image as the halted engine sees it
		s := mustScan("magic_corrupt", p)
		c.okf("magic_corrupt:reader_badheader", jstr(s, "open") == "BadHeader",
			"open=%q", jstr(s, "open"))
		rc, res := mustRecover("magic_corrupt", p)
		c.okf("magic_corrupt:halts_badheader",
			res.ExitCode == 2 && jstr(rc, "outcome") == "fatal_bad_header",
			"outcome=%q rc=%d", jstr(rc, "outcome"), res.ExitCode)
		// Fail-closed halt must not rewrite a byte of the segment.
		c.okf("magic_corrupt:no_mutation", checksum(p) == tampered,
			"checksum changed across halted open()")
	}

	// --- version_corrupt ----------------------------------------------------
	if p, w, _ := mustWrite("version_corrupt.wal", 3); jstr(w, "status") == "Ok" {
		_, _ = e.wv(ctx, "tamper", p, "4", "1", "aa") // version field @4..5
		s := mustScan("version_corrupt", p)
		c.okf("version_corrupt:badheader", jstr(s, "open") == "BadHeader",
			"open=%q", jstr(s, "open"))
	}

	// --- wrong_shard: valid header, foreign shard id -------------------------
	if p, w, _ := mustWrite("wrong_shard.wal", 3); jstr(w, "status") == "Ok" {
		out, res := e.wv(ctx, "recover", p, fmt.Sprint(walShard+1),
			walPath(e, "wal_corrupt", "wrong_shard.report.json"))
		c.okf("wrong_shard:rejected",
			res.ExitCode == 2 && jstr(out, "open") == "BadHeader",
			"open=%q rc=%d", jstr(out, "open"), res.ExitCode)
	}

	// --- torn_tail: partial record after last commit -------------------------
	if p, w, _ := mustWrite("torn_tail.wal", 5); jstr(w, "status") == "Ok" {
		tail := jnum(w, "tail")
		_, _ = e.wv(ctx, "tamper", p, fmt.Sprint(tail), "16") // half a 21B header
		s := mustScan("torn_tail", p)
		c.okf("torn_tail:corrupt_detected", jbool(s, "corrupt") &&
			jnum(s, "entries") == 5 && jnum(s, "corrupt_offset") == tail,
			"scan=%v", s)
		rc, _ := mustRecover("torn_tail", p)
		c.okf("torn_tail:truncated_at_tail",
			jnum(rc, "valid_end") == tail && jnum(rc, "truncated_bytes") > 0,
			"recover=%v", rc)
		s2 := mustScan("torn_tail_post", p)
		c.ok("torn_tail:post_clean", !jbool(s2, "corrupt") &&
			jnum(s2, "entries") == 5 && jbool(s2, "payload_ok"),
			fmt.Sprintf("scan=%v", s2))
	}

	// --- short_header: file truncated inside the 8-byte header ----------------
	{
		p := walPath(e, "wal_corrupt", "short_header.wal")
		if err := os.WriteFile(p, []byte{'W', 'A', 'L'}, 0o644); err == nil {
			s := mustScan("short_header", p)
			c.okf("short_header:badheader", jstr(s, "open") == "BadHeader",
				"open=%q", jstr(s, "open"))
			_, res := mustRecover("short_header", p)
			c.okf("short_header:halt", res.ExitCode == 2, "rc=%d", res.ExitCode)
		} else {
			c.ok("short_header:write_fixture", false, err.Error())
		}
	}

	// --- seq_regression: L2 boundary on the writer ---------------------------
	// NB: reopening an mmap segment truncates unused preallocated capacity
	// back to valid_end, so "no mutation" is asserted on committed content
	// (tail offset unchanged + entries still intact), never on file size.
	if p, w, _ := mustWrite("seq_regression.wal", 4); jstr(w, "status") == "Ok" {
		a, _ := e.wv(ctx, "append", p, fmt.Sprint(walShard), "0")
		c.okf("seq_regression:low_seq_rejected",
			jstr(a, "status") == "SeqRegression" &&
				jnum(a, "tail_after") == jnum(a, "tail_before"),
			"append=%v", a)
		a2, _ := e.wv(ctx, "append", p, fmt.Sprint(walShard), "18446744073709551615")
		c.okf("seq_regression:padseq_rejected", jstr(a2, "status") == "SeqRegression",
			"append=%v", a2)
		s := mustScan("seq_regression_post", p)
		c.okf("seq_regression:entries_intact",
			jnum(s, "entries") == 4 && jbool(s, "payload_ok") &&
				!jbool(s, "corrupt"),
			"scan=%v", s)
	}

	return c
}

// scenarioWalCrashRecovery is the automated recovery check (task item 3):
// a "primary" process is killed with SIGKILL after committing N entries and
// leaving a torn in-flight record; a standby open() must replay up to the
// last valid transaction with zero data loss and emit a recovery report.
func scenarioWalCrashRecovery(ctx context.Context, e *env) *Checks {
	c := &Checks{}
	p := walPath(e, "wal_crash_recovery", "seg.wal")
	repPath := walPath(e, "wal_crash_recovery", "recovery_report.json")
	const committed = 8

	res := runExec(ctx, "", e.walverify, "crash", p, fmt.Sprint(walShard),
		fmt.Sprint(committed))
	c.okf("crash:killed_by_sigkill", res.Signaled && res.Signal == "killed",
		"signaled=%v signal=%q exit=%d stderr=%q", res.Signaled, res.Signal,
		res.ExitCode, res.Stderr)
	pre := lastJSONLine(res.Stdout)
	c.okf("crash:committed_before_kill", jnum(pre, "committed") == committed,
		"stdout=%q", res.Stdout)

	// Scan the crashed image: 8 valid entries + torn tail corruption.
	s := func() map[string]any {
		out, res := e.wv(ctx, "scan", p)
		c.okf("scan_ran", res.Err == nil && out != nil, "stdout=%q err=%v",
			res.Stdout, res.Err)
		return out
	}()
	c.okf("crash:torn_tail_detected",
		jbool(s, "corrupt") && jnum(s, "entries") == committed &&
			jnum(s, "last_seq") == committed-1,
		"scan=%v", s)

	// Recovery: standby open() truncates the torn tail, writes the report.
	rc, res2 := e.wv(ctx, "recover", p, fmt.Sprint(walShard), repPath)
	c.okf("recover:repairs", res2.ExitCode == 0 &&
		jstr(rc, "outcome") == "truncated_torn_tail" &&
		jnum(rc, "last_valid_seq") == committed-1 &&
		jnum(rc, "entries") == committed,
		"recover=%v rc=%d", rc, res2.ExitCode)
	c.okf("recover:file_truncated", jnum(rc, "truncated_bytes") > 0 &&
		jnum(rc, "file_size_after") == jnum(rc, "valid_end"),
		"recover=%v", rc)

	// Recovery report file must exist and describe the repair.
	repBytes, err := os.ReadFile(repPath)
	rep := decodeJSON(string(repBytes))
	c.okf("recover:report_written", err == nil &&
		jstr(rep, "kind") == "wal_recovery_report" &&
		jstr(rep, "outcome") == "truncated_torn_tail",
		"report=%q err=%v", string(repBytes), err)

	// Replay on the standby: every committed entry survives intact.
	s2 := func() map[string]any {
		out, _ := e.wv(ctx, "scan", p)
		return out
	}()
	c.okf("replay:zero_loss",
		jnum(s2, "entries") == committed && jbool(s2, "payload_ok") &&
			jbool(s2, "seq_contiguous") && !jbool(s2, "corrupt"),
		"scan=%v", s2)

	// Appends resume at last_valid+1 with no gap — deterministic replay
	// continuity, not a seq reset.
	a, _ := e.wv(ctx, "append", p, fmt.Sprint(walShard), fmt.Sprint(committed))
	c.okf("replay:resume_at_tail", jstr(a, "status") == "Ok", "append=%v", a)
	s3, _ := e.wv(ctx, "scan", p)
	c.okf("replay:contiguous_after_resume",
		jnum(s3, "entries") == committed+1 && jbool(s3, "seq_contiguous"),
		"scan=%v", s3)

	return c
}

// scenarioOverflow runs the walverify `overflow` mode — safe_math L2
// boundary checks inside the real C++ code path.
func scenarioOverflow(ctx context.Context, e *env) *Checks {
	c := &Checks{}
	out, res := e.wv(ctx, "overflow")
	c.okf("overflow:runs", res.Err == nil && out != nil,
		"stdout=%q err=%v", res.Stdout, res.Err)
	c.okf("overflow:all_detected",
		jbool(out, "add_detected") && jbool(out, "sub_detected") &&
			jbool(out, "mul_detected") && jbool(out, "neg_detected"),
		"out=%v", out)
	c.okf("overflow:out_unmodified", jbool(out, "out_unmodified"), "out=%v", out)
	c.okf("overflow:coded_throw",
		jbool(out, "throw_ok") &&
			jstr(out, "throw_code") == "ARITHMETIC_OVERFLOW_DETECTED" &&
			jstr(out, "throw_severity") == "L2" && jnum(out, "throw_http") == 400,
		"out=%v", out)
	return c
}
