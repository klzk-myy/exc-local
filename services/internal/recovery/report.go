package recovery

// report.go — Task 4.3.5/4.3.9 recovery_reports row model and the JSONL
// drain. The C++ recovery ladder (RecoveryManager::recover_ladder) cannot
// speak PostgreSQL, so it appends one JSON line per non-clean run to
// -report-log (recovery_report.jsonl); RecParseReportLine /
// RecPersistJSONL read that stream and RecInsertReport writes the exact
// migration-065 column contract:
//
//	(shard_id, book_seq, wal_tail, last_valid_seq, snapshot_seq,
//	 first_divergent_seq, stage, outcome, detail JSONB, created_at)
//
// The daily ledger reconciliation job (reconcile.go) writes rows through
// the same RecInsertReport path. All exported names are Rec-prefixed —
// the orchestrator's OrchPgReportSink owns its own writer for the same
// table (same columns, different caller namespace).

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Canonical outcome vocabulary (spec §3.5 ladder).
const (
	RecOutcomeClean           = "CLEAN"
	RecOutcomeWalRepaired     = "WAL_REPAIRED"
	RecOutcomeSnapshotRebased = "SNAPSHOT_REBASED"
	RecOutcomeHalt            = "WAL_RECOVERY_HALT"
)

// Canonical stage values.
const (
	RecStageBootLadder     = "boot_ladder"
	RecStageOfflineRepair  = "offline_wal_repair"
	RecStageDailyReconcile = "daily_reconcile"
)

// The runbook a halted shard is parked under (mirrors kHaltRunbook in
// RecoveryManager.cpp — keep in sync).
const RecHaltRunbook = "docs/runbooks/wal-recovery-halt.md (exchange:replay-from-archive)"

// RecReport mirrors the recovery_reports row (migration 065) and the C++
// JSONL emitter's field names 1:1.
type RecReport struct {
	ShardID           int16           `json:"shard_id"`
	BookSeq           int64           `json:"book_seq"`            // -1 = unknown
	WalTail           int64           `json:"wal_tail"`            // last valid seq + 1
	LastValidSeq      int64           `json:"last_valid_seq"`      // -1 = none
	SnapshotSeq       int64           `json:"snapshot_seq"`        // WAL cursor covered
	FirstDivergentSeq int64           `json:"first_divergent_seq"` // -1 = none
	Stage             string          `json:"stage"`
	Outcome           string          `json:"outcome"`
	Detail            json.RawMessage `json:"detail"`
	TsUnixNs          int64           `json:"ts_unix_ns,omitempty"` // emitter stamp (detail carries no clock)
}

// RecParseReportLine decodes one JSONL row emitted by the C++ ladder.
// Unknown fields are ignored (forward-compatible); detail must be a JSON
// object (the emitter writes a pre-encoded object literal).
func RecParseReportLine(line []byte) (*RecReport, error) {
	var r RecReport
	if err := json.Unmarshal(line, &r); err != nil {
		return nil, fmt.Errorf("recovery_report parse: %w", err)
	}
	if r.Outcome == "" || r.Stage == "" {
		return nil, fmt.Errorf("recovery_report parse: outcome/stage missing")
	}
	if len(r.Detail) == 0 {
		r.Detail = json.RawMessage(`{}`)
	}
	if !json.Valid(r.Detail) {
		return nil, fmt.Errorf("recovery_report parse: detail is not valid JSON")
	}
	return &r, nil
}

// RecInsertReport writes one row. Returns the generated id.
func RecInsertReport(ctx context.Context, pool *pgxpool.Pool, r *RecReport) (int64, error) {
	if pool == nil {
		return 0, fmt.Errorf("recovery_reports insert: nil pool")
	}
	detail := r.Detail
	if len(detail) == 0 {
		detail = json.RawMessage(`{}`)
	}
	var id int64
	err := pool.QueryRow(ctx, `
		INSERT INTO recovery_reports
		    (shard_id, book_seq, wal_tail, last_valid_seq, snapshot_seq,
		     first_divergent_seq, stage, outcome, detail)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)
		RETURNING id`,
		r.ShardID, r.BookSeq, r.WalTail, r.LastValidSeq,
		r.SnapshotSeq, r.FirstDivergentSeq, r.Stage, r.Outcome, detail).
		Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("recovery_reports insert: %w", err)
	}
	return id, nil
}

// RecPersistJSONL drains a C++ recovery_report.jsonl file into
// recovery_reports. Each line is parsed and inserted; on a fully clean
// drain the file is renamed to {path}.consumed-{unix} so a re-run cannot
// double-insert (rows are append-only so duplicates are only noise, but
// consuming the file keeps operator dashboards accurate). Partial failure:
// the file is left in place for retry.
func RecPersistJSONL(ctx context.Context, pool *pgxpool.Pool, path string) (int, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, fmt.Errorf("persist %s: %w", path, err)
	}
	var rows []*RecReport
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), 4<<20)
	lineNo := 0
	for sc.Scan() {
		lineNo++
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		r, perr := RecParseReportLine(line)
		if perr != nil {
			_ = f.Close()
			return 0, fmt.Errorf("persist %s:%d: %w", path, lineNo, perr)
		}
		rows = append(rows, r)
	}
	if err := sc.Err(); err != nil {
		_ = f.Close()
		return 0, fmt.Errorf("persist %s: %w", path, err)
	}
	_ = f.Close()

	for _, r := range rows {
		if _, err := RecInsertReport(ctx, pool, r); err != nil {
			return 0, err // partial drain — file left in place for retry
		}
	}
	if len(rows) > 0 {
		done := fmt.Sprintf("%s.consumed-%d", path, time.Now().Unix())
		if err := os.Rename(path, done); err != nil {
			return len(rows), fmt.Errorf("persist %s: rows inserted but rename failed: %w", path, err)
		}
	}
	return len(rows), nil
}
