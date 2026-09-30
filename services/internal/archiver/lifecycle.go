// Hot→warm→cold lifecycle scheduler — Phase-09 Tasks 9.3.17 & 9.3.24,
// spec §19.7 (tiers) and §19.12 (schedule, hold carve-outs).
//
// One RunLifecycle pass per configured class policy:
//
//	hot→warm: attached partitions whose range ended > HotDays ago are
//	          DETACHed from the live parent and moved into the warm
//	          schema (ALTER TABLE ... SET SCHEMA) — off the OLTP path,
//	          still re-attachable on demand. Row-count + relation-size
//	          parity is recorded as move evidence (the move is
//	          metadata-only DDL; the data file never changes).
//	warm→cold: warm-schema partitions whose range ended > WarmDays ago
//	           are exported (parquet+zstd), uploaded to the WORM bucket,
//	           verified (ETag + manifest sha256 + COMPLIANCE lock via
//	           HEAD), logged to partition_archive_log, then dropped.
//	           The sha256 is the checksum evidence for the move.
//
// Compliance holds (data_retention_holds) block BOTH transitions — a
// held partition is never detached, exported, or dropped. Every decision
// (MOVED / HELD / SKIPPED / ERROR) is written to partition_tier_log and
// the current tier to partition_tier_state.
//
// ClickHouse warm copy (tick/OHLCV classes) is ingested continuously by
// the analytics pipeline — this scheduler owns only the PostgreSQL-side
// tier movement; CH TTLs are validated by the retention enforcer.
package archiver

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"time"
)

// newRunID mints a random UUID (v4 layout) for correlating one lifecycle
// or drill pass across partition_tier_log / retention_audit_log.
func newRunID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("run-%d", time.Now().UnixNano())
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	h := hex.EncodeToString(b[:])
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:32]
}

// TierMove is one transition (or planned transition) in a lifecycle pass.
type TierMove struct {
	Parent    string `json:"parent"`
	Partition string `json:"partition"`
	FromTier  string `json:"from_tier"` // HOT|WARM|"" (planned)
	ToTier    string `json:"to_tier"`   // WARM|COLD
	Result    string `json:"result"`    // MOVED|HELD|SKIPPED|ERROR|PLANNED
	RowCount  int64  `json:"row_count"`
	Checksum  string `json:"checksum,omitempty"`
	ArchiveID int64  `json:"archive_id,omitempty"`
	Detail    string `json:"detail,omitempty"`
}

// LifecycleReport is the outcome of one scheduler pass.
type LifecycleReport struct {
	RunID  string     `json:"run_id"`
	DryRun bool       `json:"dry_run"`
	At     time.Time  `json:"at"`
	Moves  []TierMove `json:"moves"`
}

// RunLifecycle executes (or plans, when dryRun) one full tier pass across
// all class policies. Fail-closed per partition: an error on one
// partition is logged ERROR and does not abort the pass — a partial move
// can never strand data because detach leaves rows reachable and the
// warm→cold path drops only after verified upload.
func (a *Archiver) RunLifecycle(ctx context.Context, dryRun bool) (*LifecycleReport, error) {
	rep := &LifecycleReport{
		RunID:  newRunID(),
		DryRun: dryRun,
		At:     a.now(),
	}
	for _, pol := range a.policies {
		pol = normalizePolicy(pol)
		if err := a.lifecycleParent(ctx, rep, pol, dryRun); err != nil {
			return rep, err
		}
	}
	return rep, nil
}

// RunLifecycleParent applies due tier transitions for a single parent —
// the Mover seam the retention enforcer calls in apply mode.
func (a *Archiver) RunLifecycleParent(ctx context.Context, parent string) error {
	rep := &LifecycleReport{RunID: newRunID(), At: a.now()}
	return a.lifecycleParent(ctx, rep, normalizePolicy(a.policyFor(parent)), false)
}

// lifecycleParent runs both stages for one class policy.
func (a *Archiver) lifecycleParent(ctx context.Context, rep *LifecycleReport,
	pol ClassPolicy, dryRun bool) error {
	// --- stage 1: hot → warm -----------------------------------------
	parts, err := a.listParentPartitions(ctx, pol.Parent)
	if err != nil {
		return err
	}
	hotCutoff := a.now().AddDate(0, 0, -pol.HotDays)
	for _, p := range parts {
		if p.RangeEnd.IsZero() || !p.RangeEnd.Before(hotCutoff) {
			continue
		}
		mv := TierMove{Parent: p.Parent, Partition: p.Name,
			FromTier: "HOT", ToTier: "WARM"}
		if dryRun {
			mv.Result = "PLANNED"
			rep.Moves = append(rep.Moves, mv)
			continue
		}
		mv = a.moveHotToWarm(ctx, rep.RunID, p, pol)
		rep.Moves = append(rep.Moves, mv)
	}

	// --- stage 2: warm → cold ----------------------------------------
	warmCutoff := a.now().AddDate(0, 0, -pol.WarmDays)
	warmParts, err := a.warmPartitions(ctx, pol)
	if err != nil {
		return err
	}
	for _, wp := range warmParts {
		mv := TierMove{Parent: wp.Parent, Partition: wp.Name,
			FromTier: "WARM", ToTier: "COLD"}
		if wp.RangeEnd.IsZero() {
			mv.Result = "SKIPPED"
			mv.Detail = "range_end unknown — adopted warm table ages from adoption date"
		} else if wp.RangeEnd.Before(warmCutoff) {
			if dryRun {
				mv.Result = "PLANNED"
			} else {
				mv = a.moveWarmToCold(ctx, rep.RunID, wp, pol)
			}
		} else {
			continue // still inside the warm window
		}
		rep.Moves = append(rep.Moves, mv)
	}
	return nil
}

// listParentPartitions lists attached children of a single parent.
func (a *Archiver) listParentPartitions(ctx context.Context, parent string) ([]Partition, error) {
	if !identRe.MatchString(parent) {
		return nil, errUnsafePolicy(parent)
	}
	rows, err := a.pool.Query(ctx, `
		SELECT pn.nspname, p.relname, cn.nspname, c.relname,
		       pg_get_expr(c.relpartbound, c.oid)
		FROM pg_inherits i
		JOIN pg_class p   ON i.inhparent = p.oid
		JOIN pg_namespace pn ON p.relnamespace = pn.oid
		JOIN pg_class c   ON i.inhrelid = c.oid
		JOIN pg_namespace cn ON c.relnamespace = cn.oid
		WHERE p.relname = $1 AND pn.nspname = 'public'
		ORDER BY c.relname`, parent)
	if err != nil {
		return nil, fmt.Errorf("archiver: lifecycle list %s: %w", parent, err)
	}
	defer rows.Close()
	var out []Partition
	for rows.Next() {
		var p Partition
		if err := rows.Scan(&p.ParentSchema, &p.Parent, &p.Schema,
			&p.Name, &p.BoundExpr); err != nil {
			return nil, err
		}
		p.RangeEnd = parseBoundEnd(p.BoundExpr)
		out = append(out, p)
	}
	return out, rows.Err()
}

// warmPartition is a detached table resident in the warm schema.
type warmPartition struct {
	Parent   string
	Name     string
	RangeEnd time.Time
}

// warmPartitions discovers class partitions currently in the warm tier:
// rows in partition_tier_state, plus warm-schema tables not yet tracked
// (adopted: they age from adoption — never auto-cold without a bound).
func (a *Archiver) warmPartitions(ctx context.Context, pol ClassPolicy) ([]warmPartition, error) {
	tracked := map[string]warmPartition{}
	rows, err := a.pool.Query(ctx, `
		SELECT partition_name, range_end, warm_schema
		FROM partition_tier_state
		WHERE parent_table = $1 AND tier = 'WARM'`, pol.Parent)
	if err != nil {
		if isUndefinedTable(err) {
			return nil, fmt.Errorf("archiver: partition_tier_state missing " +
				"(apply migration 195)")
		}
		return nil, fmt.Errorf("archiver: warm state %s: %w", pol.Parent, err)
	}
	defer rows.Close()
	for rows.Next() {
		var wp warmPartition
		var ws *string
		var re *time.Time
		if err := rows.Scan(&wp.Name, &re, &ws); err != nil {
			return nil, err
		}
		wp.Parent = pol.Parent
		if re != nil {
			wp.RangeEnd = *re
		}
		tracked[wp.Name] = wp
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	// Adopt untracked tables physically present in the warm schema that
	// look like partitions of this parent (<parent>_p* naming).
	if identRe.MatchString(pol.WarmSchema) {
		arows, err := a.pool.Query(ctx, `
			SELECT c.relname FROM pg_class c
			JOIN pg_namespace n ON c.relnamespace = n.oid
			WHERE n.nspname = $1 AND c.relkind = 'r'
			  AND c.relname LIKE $2`, pol.WarmSchema, pol.Parent+"\\_p%")
		if err != nil {
			return nil, err
		}
		defer arows.Close()
		for arows.Next() {
			var name string
			if err := arows.Scan(&name); err != nil {
				return nil, err
			}
			if _, ok := tracked[name]; !ok {
				tracked[name] = warmPartition{Parent: pol.Parent, Name: name}
			}
		}
		if err := arows.Err(); err != nil {
			return nil, err
		}
	}
	out := make([]warmPartition, 0, len(tracked))
	for _, wp := range tracked {
		out = append(out, wp)
	}
	return out, nil
}

// moveHotToWarm detaches one expired-hot partition into the warm schema
// and records the verified move.
func (a *Archiver) moveHotToWarm(ctx context.Context, runID string,
	p Partition, pol ClassPolicy) TierMove {
	mv := TierMove{Parent: p.Parent, Partition: p.Name,
		FromTier: "HOT", ToTier: "WARM"}

	held, holds, err := a.partitionHeld(ctx, p.Parent, p.Name)
	if err != nil {
		mv.Result, mv.Detail = "ERROR", err.Error()
		_ = a.logTierMove(ctx, runID, mv)
		return mv
	}
	if held {
		mv.Result = "HELD"
		mv.Detail = (&HeldError{Parent: p.Parent, Partition: p.Name, Holds: holds}).Error()
		_ = a.logTierMove(ctx, runID, mv)
		return mv
	}

	qs, _ := quoteIdent(p.Schema)
	qn, _ := quoteIdent(p.Name)
	qw, _ := quoteIdent(pol.WarmSchema)

	// Evidence: row count + relation size before the move.
	var rowsBefore, sizeBefore int64
	if err := a.pool.QueryRow(ctx, fmt.Sprintf(
		`SELECT count(*), pg_relation_size('%s.%s') FROM %s.%s`,
		p.Schema, p.Name, qs, qn)).Scan(&rowsBefore, &sizeBefore); err != nil {
		mv.Result, mv.Detail = "ERROR", fmt.Sprintf("pre-count: %v", err)
		_ = a.logTierMove(ctx, runID, mv)
		return mv
	}

	if _, err := a.pool.Exec(ctx,
		fmt.Sprintf(`CREATE SCHEMA IF NOT EXISTS %s`, qw)); err != nil {
		mv.Result, mv.Detail = "ERROR", fmt.Sprintf("create warm schema: %v", err)
		_ = a.logTierMove(ctx, runID, mv)
		return mv
	}
	if err := a.detach(ctx, p); err != nil {
		mv.Result, mv.Detail = "ERROR", fmt.Sprintf("detach: %v", err)
		_ = a.logTierMove(ctx, runID, mv)
		return mv
	}
	if _, err := a.pool.Exec(ctx, fmt.Sprintf(
		`ALTER TABLE %s.%s SET SCHEMA %s`, qs, qn, qw)); err != nil {
		// Compensate: reattach before returning so the partition is
		// never stranded detached-but-unmoved.
		rerr := a.reattach(ctx, p)
		mv.Result, mv.Detail = "ERROR",
			fmt.Sprintf("set schema: %v (reattach: %v)", err, rerr)
		_ = a.logTierMove(ctx, runID, mv)
		return mv
	}

	// Post-move parity: same row count on the warm-schema table.
	var rowsAfter int64
	if err := a.pool.QueryRow(ctx, fmt.Sprintf(
		`SELECT count(*) FROM %s.%s`, qw, qn)).Scan(&rowsAfter); err != nil {
		mv.Result, mv.Detail = "ERROR", fmt.Sprintf("post-count: %v", err)
		_ = a.logTierMove(ctx, runID, mv)
		return mv
	}
	mv.RowCount = rowsAfter
	if rowsAfter != rowsBefore {
		mv.Result = "ERROR"
		mv.Detail = fmt.Sprintf("row-count mismatch %d→%d", rowsBefore, rowsAfter)
		_ = a.logTierMove(ctx, runID, mv)
		return mv
	}

	mv.Result = "MOVED"
	mv.Detail = fmt.Sprintf("detached to %s.%s rows=%d size=%d",
		pol.WarmSchema, p.Name, rowsAfter, sizeBefore)
	if err := a.setTier(ctx, p.Parent, p.Name, "WARM", p.RangeEnd,
		p.BoundExpr, pol.WarmSchema, 0, rowsAfter, ""); err != nil {
		mv.Result, mv.Detail = "ERROR",
			fmt.Sprintf("moved but tier_state write failed: %v", err)
	}
	_ = a.logTierMove(ctx, runID, mv)
	return mv
}

// moveWarmToCold exports a warm-tier partition to the WORM bucket and
// drops it, reusing the verified archiveDetached pipeline.
func (a *Archiver) moveWarmToCold(ctx context.Context, runID string,
	wp warmPartition, pol ClassPolicy) TierMove {
	mv := TierMove{Parent: wp.Parent, Partition: wp.Name,
		FromTier: "WARM", ToTier: "COLD"}

	held, holds, err := a.partitionHeld(ctx, wp.Parent, wp.Name)
	if err != nil {
		mv.Result, mv.Detail = "ERROR", err.Error()
		_ = a.logTierMove(ctx, runID, mv)
		return mv
	}
	if held {
		mv.Result = "HELD"
		mv.Detail = (&HeldError{Parent: wp.Parent, Partition: wp.Name, Holds: holds}).Error()
		_ = a.logTierMove(ctx, runID, mv)
		return mv
	}

	p := Partition{Parent: wp.Parent, Schema: pol.WarmSchema, Name: wp.Name}
	le, err := a.archiveDetached(ctx, p)
	if err != nil {
		mv.Result, mv.Detail = "ERROR", err.Error()
		_ = a.logTierMove(ctx, runID, mv)
		return mv
	}
	mv.Result = "MOVED"
	mv.ArchiveID = le.ArchiveID
	mv.Detail = fmt.Sprintf("exported to %s (log #%d)", le.S3Key, le.ArchiveID)

	// Recover the evidence from the archive log row.
	var rowCount int64
	var sha string
	if err := a.pool.QueryRow(ctx, `
		SELECT row_count, archive_sha256 FROM partition_archive_log
		WHERE archive_id = $1`, le.ArchiveID).Scan(&rowCount, &sha); err == nil {
		mv.RowCount = rowCount
		mv.Checksum = sha
	}
	if err := a.setTier(ctx, wp.Parent, wp.Name, "COLD", wp.RangeEnd,
		"", "", le.ArchiveID, mv.RowCount, mv.Checksum); err != nil {
		mv.Result, mv.Detail = "ERROR",
			fmt.Sprintf("archived but tier_state write failed: %v", err)
	}
	_ = a.logTierMove(ctx, runID, mv)
	return mv
}

// setTier upserts partition_tier_state.
func (a *Archiver) setTier(ctx context.Context, parent, partition, tier string,
	rangeEnd time.Time, boundExpr, warmSchema string, archiveID int64,
	rowCount int64, checksum string) error {
	var re, be, ws any
	if !rangeEnd.IsZero() {
		re = rangeEnd
	}
	if boundExpr != "" {
		be = boundExpr
	}
	if warmSchema != "" {
		ws = warmSchema
	}
	var arch any
	if archiveID > 0 {
		arch = archiveID
	}
	var ck any
	if checksum != "" {
		ck = checksum
	}
	_, err := a.pool.Exec(ctx, `
		INSERT INTO partition_tier_state
		  (parent_table, partition_name, tier, tier_since, range_end,
		   bound_expr, warm_schema, archive_id, row_count, checksum, updated_at)
		VALUES ($1,$2,$3,now(),$4,$5,$6,$7,$8,$9,now())
		ON CONFLICT (parent_table, partition_name) DO UPDATE SET
		  tier = EXCLUDED.tier, tier_since = now(), range_end = EXCLUDED.range_end,
		  bound_expr = EXCLUDED.bound_expr, warm_schema = EXCLUDED.warm_schema,
		  archive_id = EXCLUDED.archive_id, row_count = EXCLUDED.row_count,
		  checksum = EXCLUDED.checksum, updated_at = now()`,
		parent, partition, tier, re, be, ws, arch, rowCount, ck)
	return err
}

// logTierMove appends to partition_tier_log (best-effort — callers ignore
// the error because the tier_state row is the authoritative record).
func (a *Archiver) logTierMove(ctx context.Context, runID string, mv TierMove) error {
	var from any
	if mv.FromTier != "" {
		from = mv.FromTier
	}
	var ck any
	if mv.Checksum != "" {
		ck = mv.Checksum
	}
	var arch any
	if mv.ArchiveID > 0 {
		arch = mv.ArchiveID
	}
	_, err := a.pool.Exec(ctx, `
		INSERT INTO partition_tier_log
		  (run_id, parent_table, partition_name, from_tier, to_tier,
		   result, row_count, checksum, detail)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)`,
		runID, mv.Parent, mv.Partition, from, mv.ToTier,
		mv.Result, mv.RowCount, ck, map[string]string{
			"detail": mv.Detail, "archive_id": fmt.Sprint(arch),
		})
	return err
}
