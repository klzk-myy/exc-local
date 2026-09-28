// Command wal-recovery is the offline WAL recovery CLI (Task 4.3.9, spec
// §3.5 ladder). It wraps the same graduated semantics RecoveryManager runs
// at boot — the Go tool performs the FILE-level ladder (the C++ engine owns
// book replay; this tool repairs/verifies the journal and snapshot files
// and emits the migration-065 recovery_reports rows):
//
//	scan             CRC-scan every {seq}.wal segment, report tail/gaps
//	repair           level 1: truncate a corrupt/torn tail at the last
//	                 CRC-valid record (preserves all confirmed txns)
//	rebase           level 2: verify the latest snapshot and write the
//	                 {snapshot_seq}.wal rebase-marker segment so the seq
//	                 space resumes past snapshot coverage
//	ladder           scan → repair → rebase → else emit WAL_RECOVERY_HALT
//	persist-reports  drain a recovery_report.jsonl file (C++ emitter) into
//	                 PostgreSQL recovery_reports
//	reconcile        daily ledger_entries → balances reconciliation
//
// Exit codes: 0 success/clean · 1 tool error · 2 usage · 3 fail-closed halt
// (the WAL_RECOVERY_HALT state — report emitted, operator action required).
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"time"

	"exchange/internal/config"
	"exchange/internal/db"
	excnats "exchange/internal/nats"
	"exchange/internal/recovery"
	"exchange/pkg/logging"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		var exitErr interface{ ExitCode() int }
		if errors.As(err, &exitErr) {
			fmt.Fprintf(os.Stderr, "wal-recovery: %v\n", err)
			os.Exit(exitErr.ExitCode())
		}
		fmt.Fprintf(os.Stderr, "wal-recovery: %v\n", err)
		os.Exit(1)
	}
}

type exitErr struct {
	code int
	msg  string
}

func (e *exitErr) Error() string { return e.msg }
func (e *exitErr) ExitCode() int { return e.code }
func errExit(code int, format string, a ...any) error {
	return &exitErr{code: code, msg: fmt.Sprintf(format, a...)}
}

func printJSON(v any) error {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

func run(args []string) error {
	if len(args) == 0 {
		return errExit(2, "usage: wal-recovery <scan|repair|rebase|ladder|persist-reports|reconcile> [flags]")
	}
	switch args[0] {
	case "scan":
		return cmdScan(args[1:])
	case "repair":
		return cmdRepair(args[1:])
	case "rebase":
		return cmdRebase(args[1:])
	case "ladder":
		return cmdLadder(args[1:])
	case "persist-reports":
		return cmdPersistReports(args[1:])
	case "reconcile":
		return cmdReconcile(args[1:])
	default:
		return errExit(2, "unknown subcommand %q", args[0])
	}
}

// --- shared flags -----------------------------------------------------------

type walFlags struct {
	dir   string
	shard int
}

func bindWalFlags(fs *flag.FlagSet) *walFlags {
	f := &walFlags{}
	fs.StringVar(&f.dir, "wal-dir", "", "shard WAL segment directory (required)")
	fs.IntVar(&f.shard, "shard", -1, "shard id to enforce (default: accept header)")
	return f
}

func (f *walFlags) prescan() (*recovery.RecWalPrescan, error) {
	if f.dir == "" {
		return nil, errExit(2, "--wal-dir is required")
	}
	return recovery.RecScanWalDir(f.dir, f.shard)
}

// --- scan --------------------------------------------------------------------

func cmdScan(args []string) error {
	fs := flag.NewFlagSet("scan", flag.ContinueOnError)
	wf := bindWalFlags(fs)
	if err := fs.Parse(args); err != nil {
		return errExit(2, "%v", err)
	}
	p, err := wf.prescan()
	if err != nil {
		return err
	}
	if err := printJSON(p); err != nil {
		return err
	}
	switch {
	case p.Divergence == "overlap":
		return errExit(3, "overlapping seq ranges — divergent journal copies")
	case p.SealedDamage:
		return errExit(3, "corrupt record inside a sealed segment")
	case p.TailCorrupt:
		return errExit(2, "corrupt tail — run `wal-recovery repair`")
	case p.Divergence == "seq_gap":
		return errExit(3, "non-contiguous WAL seq — entries lost")
	}
	return nil
}

// --- repair (level 1) ---------------------------------------------------------

func cmdRepair(args []string) error {
	fs := flag.NewFlagSet("repair", flag.ContinueOnError)
	wf := bindWalFlags(fs)
	if err := fs.Parse(args); err != nil {
		return errExit(2, "%v", err)
	}
	p, err := wf.prescan()
	if err != nil {
		return err
	}
	out := map[string]any{"wal_tail": p.WalTail, "repaired": false}
	if p.TailCorrupt {
		repaired, rerr := recovery.RecRepairTail(p.TailPath, p.TailValidEnd)
		if rerr != nil {
			return rerr
		}
		out["repaired"] = repaired
		out["truncated_path"] = p.TailPath
		out["truncate_offset"] = p.TailValidEnd
		// Re-verify post-repair (fail-closed — never claim a clean log
		// without rescanning the bytes that were just cut).
		p2, perr := wf.prescan()
		if perr != nil {
			return perr
		}
		out["post_repair"] = p2
		if p2.TailCorrupt || p2.SealedDamage || p2.Divergence != "" {
			_ = printJSON(out)
			return errExit(3, "tail repair did not restore a clean log — run `wal-recovery ladder` for the rebase/halt escalation")
		}
	} else {
		out["note"] = "tail already clean"
	}
	return printJSON(out)
}

// --- rebase (level 2) ---------------------------------------------------------

func cmdRebase(args []string) error {
	fs := flag.NewFlagSet("rebase", flag.ContinueOnError)
	wf := bindWalFlags(fs)
	snapRoot := fs.String("snap-root", "", "snapshot sink root (required)")
	instrument := fs.Uint("instrument-id", 0, "bound instrument id (required)")
	if err := fs.Parse(args); err != nil {
		return errExit(2, "%v", err)
	}
	if *snapRoot == "" || *instrument == 0 {
		return errExit(2, "--snap-root and --instrument-id are required")
	}
	p, err := wf.prescan()
	if err != nil {
		return err
	}
	snap, fellBack, err := recovery.RecLatestVerifiedSnapshot(*snapRoot, uint32(*instrument))
	if err != nil {
		return errExit(3, "no verified snapshot available: %v", err)
	}
	if snap == nil {
		return errExit(3, "no snapshot under %s — genesis replay is a boot decision, not an offline rebase", *snapRoot)
	}
	out := map[string]any{
		"snapshot_seq":     snap.Seq,
		"snapshot_path":    snap.Path,
		"snapshot_crc_ok":  true,
		"prior_generation": fellBack,
		"wal_tail":         p.WalTail,
		"marker_written":   false,
	}
	if snap.Seq > p.WalTail {
		// Forward divergence — the snapshot covers past the surviving log;
		// write the rebase marker so the seq space resumes at snapshot_seq.
		path, wrote, merr := recovery.RecWriteRebaseMarker(wf.dir,
			uint16(snap.ShardID), snap.Seq)
		if merr != nil {
			return merr
		}
		out["marker_written"] = wrote
		out["marker_path"] = path
		p2, perr := wf.prescan()
		if perr != nil {
			return perr
		}
		out["post_rebase"] = p2
		// The marker covers the divergence: every remaining lost range must
		// end at or below snapshot_seq (snapshot-covered loss).
		for _, lr := range p2.LostRanges {
			if lr.End > snap.Seq {
				_ = printJSON(out)
				return errExit(3, "rebase incomplete — uncovered seq range [%d,%d) above snapshot %d", lr.Begin, lr.End, snap.Seq)
			}
		}
	} else {
		out["note"] = "snapshot at or below WAL tail — no rebase needed"
	}
	return printJSON(out)
}

// --- ladder (all three levels, fail-closed) ------------------------------------

func cmdLadder(args []string) error {
	fs := flag.NewFlagSet("ladder", flag.ContinueOnError)
	wf := bindWalFlags(fs)
	snapRoot := fs.String("snap-root", "", "snapshot sink root (optional — level 2 skipped without it)")
	instrument := fs.Uint("instrument-id", 0, "bound instrument id")
	reportOut := fs.String("report-out", "", "append recovery_reports JSONL rows here")
	pg := fs.Bool("pg", false, "also insert report rows into PostgreSQL (config Postgres.DSN)")
	natsAlert := fs.Bool("nats", false, "publish report rows to the ops.alerts.recovery NATS subject")
	if err := fs.Parse(args); err != nil {
		return errExit(2, "%v", err)
	}

	ctx := context.Background()

	// NATS alert seam: core (non-JetStream) publish on ops.alerts.recovery —
	// ops bridges/alertmanager subscribe there; the WAL halts page P1 while
	// repaired/rebased outcomes ride the same subject for dashboards.
	var alertPub func(payload []byte)
	if *natsAlert {
		cfg, cerr := config.Load()
		if cerr != nil {
			return cerr
		}
		level, _ := logging.ParseLevel(cfg.Logging.Level)
		log, lerr := logging.New(level, cfg.Logging.Format)
		if lerr != nil {
			return lerr
		}
		ncfg := excnats.DefaultConfig(cfg.NATS.URLList())
		ncfg.Name = "wal-recovery"
		cli, nerr := excnats.Connect(ctx, ncfg, log)
		if nerr != nil {
			return fmt.Errorf("nats alert seam: %w", nerr)
		}
		defer cli.Close()
		alertPub = func(payload []byte) {
			if perr := cli.Conn().Publish("ops.alerts.recovery", payload); perr != nil {
				fmt.Fprintf(os.Stderr, "nats alert publish failed: %v\n", perr)
			}
		}
	}
	report := &recovery.RecReport{
		ShardID:           int16(wf.shard),
		BookSeq:           -1,
		LastValidSeq:      -1,
		FirstDivergentSeq: -1,
		Stage:             recovery.RecStageOfflineRepair,
	}
	emit := func() error {
		line, merr := json.Marshal(report)
		if merr != nil {
			return merr
		}
		fmt.Println(string(line))
		if alertPub != nil {
			alertPub(line)
		}
		if *reportOut != "" {
			f, oerr := os.OpenFile(*reportOut, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
			if oerr != nil {
				return oerr
			}
			_, werr := f.Write(append(line, '\n'))
			_ = f.Sync()
			_ = f.Close()
			if werr != nil {
				return werr
			}
		}
		if *pg {
			cfg, cerr := config.Load()
			if cerr != nil {
				return cerr
			}
			pool, perr := db.NewPool(ctx, cfg.Postgres.DSN, 2)
			if perr != nil {
				return perr
			}
			defer pool.Close()
			if _, ierr := recovery.RecInsertReport(ctx, pool, report); ierr != nil {
				return ierr
			}
		}
		return nil
	}

	// ---- Level 1: CRC repair of a torn/corrupt tail -------------------------
	p, err := wf.prescan()
	if err != nil {
		report.Outcome = recovery.RecOutcomeHalt
		report.Detail = json.RawMessage(
			fmt.Sprintf(`{"error":%q,"runbook":%q}`, err.Error(), recovery.RecHaltRunbook))
		_ = emit()
		return errExit(3, "%v", err)
	}
	report.WalTail = int64(p.WalTail)
	report.LastValidSeq = p.LastValidSeq
	repaired := false
	if p.TailCorrupt {
		if _, err := recovery.RecRepairTail(p.TailPath, p.TailValidEnd); err != nil {
			return err
		}
		repaired = true
		if p2, perr := wf.prescan(); perr == nil {
			p = p2
			report.WalTail = int64(p2.WalTail)
			report.LastValidSeq = p2.LastValidSeq
		} else {
			return perr
		}
	}

	halt := func(why string, detailKV map[string]any) error {
		report.Outcome = recovery.RecOutcomeHalt
		d := map[string]any{"reason": why, "runbook": recovery.RecHaltRunbook}
		for k, v := range detailKV {
			d[k] = v
		}
		raw, _ := json.Marshal(d)
		report.Detail = raw
		if eerr := emit(); eerr != nil {
			return eerr
		}
		fmt.Fprintf(os.Stderr,
			"ALERT_P1 WAL_RECOVERY_HALT shard=%d wal_tail=%d runbook=%s\n",
			report.ShardID, report.WalTail, recovery.RecHaltRunbook)
		return errExit(3, "%s", why)
	}

	if p.Divergence == "overlap" {
		report.FirstDivergentSeq = int64(p.StreamBase)
		return halt("overlapping WAL seq ranges — divergent journal copies",
			map[string]any{"divergence": p.Divergence})
	}

	// Snapshot coverage bound: the level-2 input — the newest VERIFIED
	// snapshot cursor (0 when no snapshot exists → genesis-only coverage).
	var snapshotSeq uint64
	var snap *recovery.OrchSnapshotInfo
	fellBack := false
	if *snapRoot != "" && *instrument != 0 {
		snap, fellBack, err = recovery.RecLatestVerifiedSnapshot(*snapRoot, uint32(*instrument))
		if err == nil && snap != nil {
			snapshotSeq = snap.Seq
		}
	}
	report.SnapshotSeq = int64(snapshotSeq)

	// Forward divergence (spec §18.5): the snapshot claims coverage beyond
	// the surviving log tail — the same early check the C++ ladder runs.
	fwdDiv := snap != nil && snapshotSeq > p.WalTail
	if fwdDiv {
		report.FirstDivergentSeq = int64(snapshotSeq)
	}

	// Coverage check: every lost range must end at/below snapshot_seq —
	// the snapshot is provably authoritative through its cursor, so a
	// covered range means the lost bytes carried only redundant journal.
	uncovered := false
	for _, lr := range p.LostRanges {
		if lr.End > snapshotSeq {
			report.FirstDivergentSeq = int64(lr.End)
			uncovered = true
			break
		}
	}

	switch {
	case uncovered:
		// Level 3 — no snapshot can prove the lost seqs.
		return halt("uncovered seq loss — WAL and snapshot cannot prove the lost range",
			map[string]any{"lost_ranges": p.LostRanges, "snapshot_seq": snapshotSeq})

	case fwdDiv:
		// Level 2 rebase: anchor the seq space at snapshot_seq with the
		// {snapshot_seq}.wal marker segment (identical bytes to the C++
		// write_rebase_marker) so replay resumes past snapshot coverage.
		path, _, merr := recovery.RecWriteRebaseMarker(wf.dir,
			uint16(snap.ShardID), snapshotSeq)
		if merr != nil {
			return merr
		}
		p2, perr := wf.prescan()
		if perr != nil {
			return perr
		}
		p = p2
		report.WalTail = int64(p2.WalTail)
		report.LastValidSeq = p2.LastValidSeq
		for _, lr := range p2.LostRanges {
			if lr.End > snapshotSeq {
				return halt("rebase marker did not cover the lost range",
					map[string]any{"marker_path": path, "lost_ranges": p2.LostRanges})
			}
		}
		report.Outcome = recovery.RecOutcomeSnapshotRebased
		report.BookSeq = int64(p.WalTail)
		report.Detail = json.RawMessage(fmt.Sprintf(
			`{"level":2,"marker_path":%q,"snapshot_seq":%d,"prior_generation":%v,"repaired_tail":%v}`,
			path, snapshotSeq, fellBack, repaired))
		return emit()

	case p.SealedDamage:
		// Sealed-segment damage whose lost range is empty (the next segment
		// resumes contiguously — the damaged bytes provably held nothing
		// valid) or fully snapshot-covered is tolerated at level 2.
		report.Outcome = recovery.RecOutcomeWalRepaired
		report.BookSeq = int64(p.WalTail)
		report.Detail = json.RawMessage(fmt.Sprintf(
			`{"level":2,"tolerated_sealed_damage":true,"snapshot_seq":%d,"covered_ranges":%d}`,
			snapshotSeq, len(p.LostRanges)))
		return emit()

	default:
		// Clean (or level-1 repaired) — done.
		report.Outcome = recovery.RecOutcomeClean
		if repaired {
			report.Outcome = recovery.RecOutcomeWalRepaired
		}
		report.BookSeq = int64(p.WalTail)
		report.Detail = json.RawMessage(
			fmt.Sprintf(`{"level":1,"repaired":%v,"wal_entries":%d}`, repaired, p.WalEntries))
		if repaired {
			if err := emit(); err != nil {
				return err
			}
		}
		return nil
	}
}

// --- persist-reports ------------------------------------------------------------

func cmdPersistReports(args []string) error {
	fs := flag.NewFlagSet("persist-reports", flag.ContinueOnError)
	file := fs.String("file", "", "recovery_report.jsonl path (required)")
	if err := fs.Parse(args); err != nil {
		return errExit(2, "%v", err)
	}
	if *file == "" {
		return errExit(2, "--file is required")
	}
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	pool, err := db.NewPool(ctx, cfg.Postgres.DSN, 2)
	if err != nil {
		return err
	}
	defer pool.Close()
	n, err := recovery.RecPersistJSONL(ctx, pool, *file)
	if err != nil {
		return err
	}
	return printJSON(map[string]any{"file": *file, "rows_inserted": n})
}

// --- reconcile (daily job) ------------------------------------------------------

func cmdReconcile(args []string) error {
	fs := flag.NewFlagSet("reconcile", flag.ContinueOnError)
	if err := fs.Parse(args); err != nil {
		return errExit(2, "%v", err)
	}
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	pool, err := db.NewPool(ctx, cfg.Postgres.DSN, 4)
	if err != nil {
		return err
	}
	defer pool.Close()
	res, err := recovery.RecReconcile(ctx, pool, nil)
	if err != nil {
		return err
	}
	if jerr := printJSON(res); jerr != nil {
		return jerr
	}
	if len(res.Mismatches) > 0 {
		return errExit(3, "%d wallet mismatches — recovery_reports row %d written",
			len(res.Mismatches), res.ReportRowID)
	}
	return nil
}
