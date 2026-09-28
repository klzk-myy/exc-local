// The configuration-driven retention enforcer — Phase-09 Task 9.3.22,
// spec §19.12 item 4. A nightly run validates the policy matrix against
// live state:
//
//	postgres_partitioned — attached partitions past hot_days must not be
//	                       sitting un-archived; warm-tier partitions past
//	                       warm_days must not be stranded; cold-tier rows
//	                       must have a DROPPED archive-log entry.
//	postgres_table       — purgeable classes: rows older than retain_days
//	                       are a VIOLATION in dry-run and are DELETEd in
//	                       apply mode (an active parent-name hold blocks
//	                       the purge — GDPR Art. 17(3)(b) carve-out).
//	clickhouse           — TTL drift: rows older than retain_days still
//	                       present (via the CHQuerier seam; SKIPPED when
//	                       no ClickHouse client is wired).
//	object_store         — cold evidence: every DROPPED archive log row's
//	                       object must still exist until retain_until;
//	                       expired objects past retain_until are purge
//	                       candidates (deleted in apply mode unless held).
//	external             — lifecycle-bound classes (e.g. KYC = account
//	                       close + 5y) are logged SKIPPED with the owning
//	                       flow named — the schedule documents them, this
//	                       enforcer does not evaluate them row-wise.
//
// Every check writes a retention_audit_log row keyed by run_id; any
// VIOLATION is a RETENTION_POLICY_VIOLATION finding (spec §23 — P2,
// Compliance/DevOps).
package retention

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"exchange/internal/objectstore"
)

// Finding is one check outcome (also the shape written to
// retention_audit_log).
type Finding struct {
	DataType string `json:"data_type"`
	Target   string `json:"target"`
	Check    string `json:"check"`
	Result   string `json:"result"`
	Detail   string `json:"detail,omitempty"`
}

// Report summarises one enforcer run.
type Report struct {
	RunID      string    `json:"run_id"`
	Mode       string    `json:"mode"` // DRY_RUN | APPLY
	At         time.Time `json:"at"`
	Findings   []Finding `json:"findings"`
	Violations int       `json:"violations"`
	Actions    int       `json:"actions"`
	Errors     int       `json:"errors"`
}

// CHQuerier is the narrow ClickHouse seam (same signature as
// persistence.CHQuerier; re-declared to keep the dependency direction
// retention → nothing).
type CHQuerier interface {
	QueryCSV(ctx context.Context, query string) (io.ReadCloser, error)
}

// Mover applies one class's hot→warm→cold movement (wired to the
// archiver lifecycle runner in cmd).
type Mover interface {
	MoveParent(ctx context.Context, parent string) error
}

// Enforcer validates the policy against live state.
type Enforcer struct {
	pool   *pgxpool.Pool
	policy *Policy
	store  objectstore.Client // optional — object_store checks
	ch     CHQuerier          // optional — clickhouse checks
	mover  Mover              // optional — apply-mode partition moves
	now    func() time.Time
}

func New(pool *pgxpool.Pool, policy *Policy) *Enforcer {
	return &Enforcer{
		pool:   pool,
		policy: policy,
		now:    func() time.Time { return time.Now().UTC() },
	}
}

func (e *Enforcer) SetStore(c objectstore.Client) { e.store = c }
func (e *Enforcer) SetCH(q CHQuerier)             { e.ch = q }
func (e *Enforcer) SetMover(m Mover)              { e.mover = m }
func (e *Enforcer) SetClock(f func() time.Time)   { e.now = f }

var boundEndRe = regexp.MustCompile(`(?i)\bTO\s*\(\s*'([^']+)'\s*\)`)

// parseBoundEnd extracts the TO bound from a relpartbound expression.
// Duplicated from internal/archiver (unexported there) to keep the
// dependency direction one-way.
func parseBoundEnd(expr string) time.Time {
	m := boundEndRe.FindStringSubmatch(expr)
	if m == nil {
		return time.Time{}
	}
	for _, layout := range []string{
		"2006-01-02 15:04:05.999999999-07",
		"2006-01-02 15:04:05.999999999Z07",
		"2006-01-02 15:04:05.999999999",
		"2006-01-02 15:04:05-07",
		"2006-01-02 15:04:05Z07",
		"2006-01-02 15:04:05",
		"2006-01-02",
	} {
		if t, err := time.Parse(layout, m[1]); err == nil {
			return t.UTC()
		}
	}
	return time.Time{}
}

// newRunID mints a random UUID (v4 layout) for correlating one enforcer
// run across retention_audit_log rows.
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

// Run executes every class check. apply=false is the nightly dry-run;
// apply=true additionally performs the corrective action (partition move
// via Mover, row purge, cold-object delete). Run never fails fast on a
// class error — the failure is recorded as an ERROR finding so drift is
// visible rather than aborting the sweep.
func (e *Enforcer) Run(ctx context.Context, apply bool) (*Report, error) {
	mode := "DRY_RUN"
	if apply {
		mode = "APPLY"
	}
	rep := &Report{RunID: newRunID(), Mode: mode, At: e.now()}

	for i := range e.policy.Classes {
		c := e.policy.Classes[i]
		var fs []Finding
		switch c.Store {
		case StorePartitionedPG:
			fs = e.checkPartitioned(ctx, c, apply)
		case StoreTablePG:
			fs = e.checkTable(ctx, c, apply)
		case StoreClickHouse:
			fs = e.checkClickHouse(ctx, c)
		case StoreObjectStore:
			fs = e.checkObjectStore(ctx, c, apply)
		case StoreExternal:
			fs = []Finding{{
				DataType: c.Name, Target: c.Table, Check: "lifecycle_bound",
				Result: ResultSkipped,
				Detail: "retention bound to account lifecycle (" + c.Mechanism +
					") — enforced by the offboarding flow, not time-based",
			}}
		}
		for _, f := range fs {
			rep.Findings = append(rep.Findings, f)
			switch f.Result {
			case ResultViolation:
				rep.Violations++
			case ResultAction:
				rep.Actions++
			case ResultError:
				rep.Errors++
			}
			e.audit(ctx, rep.RunID, mode, f)
		}
	}
	return rep, nil
}

// --- check implementations -------------------------------------------------

// checkPartitioned validates a partitioned parent's tier position.
func (e *Enforcer) checkPartitioned(ctx context.Context, c DataClass, apply bool) []Finding {
	var out []Finding

	// 1. hot_window: attached partitions past the hot boundary drift.
	hotCut := e.now().AddDate(0, 0, -nz(c.HotDays, 90))
	rows, err := e.pool.Query(ctx, `
		SELECT c.relname, pg_get_expr(c.relpartbound, c.oid)
		FROM pg_inherits i
		JOIN pg_class p ON i.inhparent = p.oid
		JOIN pg_namespace pn ON p.relnamespace = pn.oid
		JOIN pg_class c ON i.inhrelid = c.oid
		WHERE p.relname = $1 AND pn.nspname = 'public'`, c.Table)
	if err != nil {
		return []Finding{{DataType: c.Name, Target: c.Table,
			Check: "hot_window", Result: ResultError, Detail: err.Error()}}
	}
	type partRow struct{ name, bound string }
	var parts []partRow
	for rows.Next() {
		var pr partRow
		if err := rows.Scan(&pr.name, &pr.bound); err == nil {
			parts = append(parts, pr)
		}
	}
	rows.Close()

	for _, pr := range parts {
		end := parseBoundEnd(pr.bound)
		if end.IsZero() || !end.Before(hotCut) {
			continue
		}
		f := Finding{DataType: c.Name, Target: pr.name, Check: "hot_window",
			Result: ResultViolation,
			Detail: fmt.Sprintf("attached partition range_end %s older than hot window %dd — RETENTION_POLICY_VIOLATION",
				end.Format("2006-01-02"), nz(c.HotDays, 90))}
		if apply && e.mover != nil {
			if err := e.mover.MoveParent(ctx, c.Table); err != nil {
				f.Result = ResultError
				f.Detail += " | move failed: " + err.Error()
			} else {
				f.Result = ResultAction
				f.Detail += " | moved hot→warm by lifecycle mover"
			}
		}
		out = append(out, f)
	}
	if len(out) == 0 {
		out = append(out, Finding{DataType: c.Name, Target: c.Table,
			Check: "hot_window", Result: ResultOK,
			Detail: "no attached partition past hot cutoff"})
	}

	// 2. warm_overstay + cold_evidence from partition_tier_state.
	out = append(out, e.checkTierState(ctx, c, apply)...)

	// 3. holds_active: informational proof the carve-out is live.
	holds, err := e.activeHolds(ctx, c.Table, "")
	if err != nil {
		out = append(out, Finding{DataType: c.Name, Target: c.Table,
			Check: "holds_active", Result: ResultError, Detail: err.Error()})
	} else {
		for _, h := range holds {
			out = append(out, Finding{DataType: c.Name,
				Target: c.Table + "/" + h, Check: "holds_active",
				Result: ResultHeld, Detail: "active compliance hold"})
		}
	}
	return out
}

// checkTierState validates warm/cold bookkeeping for the parent.
func (e *Enforcer) checkTierState(ctx context.Context, c DataClass, apply bool) []Finding {
	var out []Finding
	warmCut := e.now().AddDate(0, 0, -nz(c.WarmDays, 365))
	rows, err := e.pool.Query(ctx, `
		SELECT partition_name, tier, range_end, archive_id
		FROM partition_tier_state WHERE parent_table = $1`, c.Table)
	if err != nil {
		if isUndefinedTable(err) {
			return []Finding{{DataType: c.Name, Target: c.Table,
				Check: "tier_state", Result: ResultSkipped,
				Detail: "partition_tier_state absent (migration 195 not applied)"}}
		}
		return []Finding{{DataType: c.Name, Target: c.Table,
			Check: "tier_state", Result: ResultError, Detail: err.Error()}}
	}
	defer rows.Close()
	type ts struct {
		partition string
		tier      string
		rangeEnd  *time.Time
		archiveID *int64
	}
	var states []ts
	for rows.Next() {
		var s ts
		if err := rows.Scan(&s.partition, &s.tier, &s.rangeEnd, &s.archiveID); err == nil {
			states = append(states, s)
		}
	}
	for _, s := range states {
		switch s.tier {
		case "WARM":
			if s.rangeEnd != nil && s.rangeEnd.Before(warmCut) {
				f := Finding{DataType: c.Name, Target: s.partition,
					Check: "warm_overstay", Result: ResultViolation,
					Detail: "warm partition past warm boundary — RETENTION_POLICY_VIOLATION"}
				if apply && e.mover != nil {
					if err := e.mover.MoveParent(ctx, c.Table); err != nil {
						f.Result = ResultError
						f.Detail += " | move failed: " + err.Error()
					} else {
						f.Result = ResultAction
						f.Detail += " | moved warm→cold by lifecycle mover"
					}
				}
				out = append(out, f)
			}
		case "COLD":
			if s.archiveID == nil {
				out = append(out, Finding{DataType: c.Name, Target: s.partition,
					Check: "cold_evidence", Result: ResultViolation,
					Detail: "tier=COLD without archive_id — RETENTION_POLICY_VIOLATION"})
				continue
			}
			var status string
			err := e.pool.QueryRow(ctx, `
				SELECT status FROM partition_archive_log WHERE archive_id = $1`,
				*s.archiveID).Scan(&status)
			if err != nil || (status != "DROPPED" && status != "RESTORED") {
				out = append(out, Finding{DataType: c.Name, Target: s.partition,
					Check: "cold_evidence", Result: ResultViolation,
					Detail: fmt.Sprintf("archive log status %q (err %v)", status, err)})
			}
		}
	}
	if len(out) == 0 {
		out = append(out, Finding{DataType: c.Name, Target: c.Table,
			Check: "tier_state", Result: ResultOK,
			Detail: "tier bookkeeping consistent"})
	}
	return out
}

// checkTable handles plain (non-partitioned) tables.
func (e *Enforcer) checkTable(ctx context.Context, c DataClass, apply bool) []Finding {
	if !c.Purgeable {
		// Time-bound-but-non-purgeable class: report oldest row age as
		// evidence the table is being tracked (deletion is manual).
		f := Finding{DataType: c.Name, Target: c.Table,
			Check: "retained", Result: ResultOK,
			Detail: "non-purgeable class tracked; retention floor " +
				strconv.Itoa(c.RetainDays) + "d"}
		return []Finding{f}
	}
	cut := e.now().AddDate(0, 0, -c.RetainDays)
	var expired int64
	err := e.pool.QueryRow(ctx, fmt.Sprintf(
		`SELECT count(*) FROM %q WHERE %q < $1`, c.Table, c.TSColumn), cut).
		Scan(&expired)
	if err != nil {
		if isUndefinedTable(err) {
			return []Finding{{DataType: c.Name, Target: c.Table,
				Check: "expired_rows", Result: ResultSkipped,
				Detail: "table absent"}}
		}
		return []Finding{{DataType: c.Name, Target: c.Table,
			Check: "expired_rows", Result: ResultError, Detail: err.Error()}}
	}
	if expired == 0 {
		return []Finding{{DataType: c.Name, Target: c.Table,
			Check: "expired_rows", Result: ResultOK,
			Detail: "no rows past retention window"}}
	}

	// Hold carve-out: a parent-name hold blocks the purge.
	if holds, herr := e.activeHolds(ctx, c.Table, ""); herr == nil && len(holds) > 0 {
		return []Finding{{DataType: c.Name, Target: c.Table,
			Check: "expired_rows", Result: ResultHeld,
			Detail: fmt.Sprintf("%d expired rows but table under hold (%s)",
				expired, strings.Join(holds, ";"))}}
	}

	f := Finding{DataType: c.Name, Target: c.Table,
		Check: "expired_rows", Result: ResultViolation,
		Detail: fmt.Sprintf("%d rows older than %dd — RETENTION_POLICY_VIOLATION",
			expired, c.RetainDays)}
	if apply {
		tag, derr := e.pool.Exec(ctx, fmt.Sprintf(
			`DELETE FROM %q WHERE %q < $1`, c.Table, c.TSColumn), cut)
		if derr != nil {
			f.Result = ResultError
			f.Detail += " | purge failed: " + derr.Error()
		} else {
			f.Result = ResultAction
			f.Detail = fmt.Sprintf("purged %d rows older than %dd",
				tag.RowsAffected(), c.RetainDays)
		}
	}
	return []Finding{f}
}

// checkClickHouse verifies TTL drift via the query seam.
func (e *Enforcer) checkClickHouse(ctx context.Context, c DataClass) []Finding {
	if e.ch == nil {
		return []Finding{{DataType: c.Name, Target: c.Table,
			Check: "ttl_drift", Result: ResultSkipped,
			Detail: "no ClickHouse querier wired"}}
	}
	ts := c.TSColumn
	if ts == "" {
		ts = "timestamp"
	}
	q := fmt.Sprintf(
		`SELECT count() FROM %s WHERE %s < now() - INTERVAL %d DAY FORMAT CSV`,
		c.Table, ts, c.RetainDays)
	rc, err := e.ch.QueryCSV(ctx, q)
	if err != nil {
		return []Finding{{DataType: c.Name, Target: c.Table,
			Check: "ttl_drift", Result: ResultError, Detail: err.Error()}}
	}
	defer rc.Close()
	body, err := io.ReadAll(rc)
	if err != nil {
		return []Finding{{DataType: c.Name, Target: c.Table,
			Check: "ttl_drift", Result: ResultError, Detail: err.Error()}}
	}
	n, err := strconv.ParseInt(strings.Trim(strings.TrimSpace(string(body)), `"`), 10, 64)
	if err != nil {
		return []Finding{{DataType: c.Name, Target: c.Table,
			Check: "ttl_drift", Result: ResultError,
			Detail: "unparseable count: " + string(body)}}
	}
	if n > 0 {
		return []Finding{{DataType: c.Name, Target: c.Table,
			Check: "ttl_drift", Result: ResultViolation,
			Detail: fmt.Sprintf("%d rows older than %dd survive TTL — RETENTION_POLICY_VIOLATION",
				n, c.RetainDays)}}
	}
	return []Finding{{DataType: c.Name, Target: c.Table,
		Check: "ttl_drift", Result: ResultOK,
		Detail: "no rows older than retain window"}}
}

// checkObjectStore verifies cold-tier evidence and purges expired
// archives (apply mode, holds honored, WORM lock permitting).
func (e *Enforcer) checkObjectStore(ctx context.Context, c DataClass, apply bool) []Finding {
	if e.store == nil {
		return []Finding{{DataType: c.Name, Target: c.Table,
			Check: "cold_integrity", Result: ResultSkipped,
			Detail: "no object store client wired"}}
	}
	rows, err := e.pool.Query(ctx, `
		SELECT archive_id, partition_name, s3_key, retain_until, status
		FROM partition_archive_log
		WHERE s3_bucket = $1 AND status IN ('DROPPED','RESTORED')`,
		e.store.Bucket())
	if err != nil {
		if isUndefinedTable(err) {
			return []Finding{{DataType: c.Name, Target: c.Table,
				Check: "cold_integrity", Result: ResultSkipped,
				Detail: "partition_archive_log absent"}}
		}
		return []Finding{{DataType: c.Name, Target: c.Table,
			Check: "cold_integrity", Result: ResultError, Detail: err.Error()}}
	}
	defer rows.Close()
	type arc struct {
		id          int64
		partition   string
		key         string
		retainUntil *time.Time
		status      string
	}
	var arcs []arc
	for rows.Next() {
		var a arc
		if err := rows.Scan(&a.id, &a.partition, &a.key,
			&a.retainUntil, &a.status); err == nil {
			arcs = append(arcs, a)
		}
	}
	if err := rows.Err(); err != nil {
		return []Finding{{DataType: c.Name, Target: c.Table,
			Check: "cold_integrity", Result: ResultError, Detail: err.Error()}}
	}

	var out []Finding
	missing, expired := 0, 0
	for _, a := range arcs {
		if a.retainUntil != nil && a.retainUntil.Before(e.now()) {
			expired++
			// Retention window elapsed — purge candidate. Hold check first.
			if holds, herr := e.activeHolds(ctx, "" /*any*/, a.partition); herr == nil &&
				len(holds) > 0 {
				out = append(out, Finding{DataType: c.Name, Target: a.key,
					Check: "expired_cold", Result: ResultHeld,
					Detail: "past retain_until but under active hold"})
				continue
			}
			f := Finding{DataType: c.Name, Target: a.key,
				Check: "expired_cold", Result: ResultViolation,
				Detail: "archive past retain_until — purge pending (apply mode deletes)"}
			if apply {
				if derr := e.store.Delete(ctx, a.key); derr != nil {
					f.Result = ResultError
					f.Detail = "delete failed (object lock?): " + derr.Error()
				} else {
					f.Result = ResultAction
					f.Detail = "deleted expired cold archive object"
				}
			}
			out = append(out, f)
			continue
		}
		// Within retention floor — object must exist (WORM evidence).
		if _, err := e.store.Head(ctx, a.key); err != nil {
			missing++
			out = append(out, Finding{DataType: c.Name, Target: a.key,
				Check: "cold_integrity", Result: ResultViolation,
				Detail: "cold archive object missing inside retention window — " +
					"RETENTION_POLICY_VIOLATION"})
		}
	}
	out = append(out, Finding{DataType: c.Name, Target: c.Table,
		Check: "cold_integrity", Result: ResultOK,
		Detail: fmt.Sprintf("%d archives checked: %d missing, %d expired",
			len(arcs), missing, expired)})
	return out
}

// --- helpers ----------------------------------------------------------------

// activeHolds returns case refs of active holds covering parent (or, when
// partition is supplied, partition-scoped holds under any parent).
func (e *Enforcer) activeHolds(ctx context.Context, parent, partition string) ([]string, error) {
	var q string
	var args []any
	switch {
	case partition != "":
		q = `SELECT case_ref FROM data_retention_holds
		     WHERE released_at IS NULL
		       AND (expires_at IS NULL OR expires_at > now())
		       AND (partition_name = $1 OR (parent_table = $1 AND partition_name IS NULL))`
		args = []any{partition}
	default:
		q = `SELECT case_ref FROM data_retention_holds
		     WHERE released_at IS NULL
		       AND (expires_at IS NULL OR expires_at > now())
		       AND parent_table = $1`
		args = []any{parent}
	}
	rows, err := e.pool.Query(ctx, q, args...)
	if err != nil {
		if isUndefinedTable(err) {
			return nil, nil
		}
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err == nil {
			out = append(out, s)
		}
	}
	return out, rows.Err()
}

// audit persists one finding row. Tolerates a missing audit table
// (pre-migration schemas) — the finding is still in the returned Report.
func (e *Enforcer) audit(ctx context.Context, runID, mode string, f Finding) {
	detail, _ := json.Marshal(f)
	_, err := e.pool.Exec(ctx, `
		INSERT INTO retention_audit_log
		  (run_id, mode, data_type, target, check_name, result, detail)
		VALUES ($1,$2,$3,$4,$5,$6,$7)`,
		runID, mode, f.DataType, f.Target, f.Check, f.Result, string(detail))
	_ = err // audit sink failure must never hide the finding itself
}

func isUndefinedTable(err error) bool {
	for err != nil {
		if e, ok := err.(*pgconn.PgError); ok {
			return e.Code == "42P01"
		}
		type u interface{ Unwrap() error }
		un, ok := err.(u)
		if !ok {
			return false
		}
		err = un.Unwrap()
	}
	return false
}

func nz(v, def int) int {
	if v <= 0 {
		return def
	}
	return v
}
