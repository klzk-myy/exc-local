// Package admin hosts the Phase-07 admin & monitoring domain services:
// the append-only admin audit log (Task 7.3.3, this file) and the
// read-only support view (Task 7.3.7, support_view.go). RBAC, dual
// control, data scopes and role lifecycle land in sibling files
// (rbac.go / scopes.go / lifecycle.go — Tasks 7.3.1/7.3.2/7.3.11/7.3.12).
package admin

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"exchange/internal/audit"

	excerrors "exchange/pkg/errors"
)

// ---------------------------------------------------------------------------
// Task 7.3.3 — admin audit log writer (spec §5.9, migration 010).
//
// Every privileged mutation funnels through Log: one admin_audit_log row
// with before/after state plus the audit_hash_chain link (spec §5.8/
// §14.11 remediation #38 F11) inside the SAME transaction, so the audited
// action and its tamper-evident anchor commit or fail together.
//
// Hash-chain binding: the chain row records
// (table_name='admin_audit_log', record_id=admin_audit_log.id) with the
// chain action 'INSERT'. admin_audit_log has no prev_checksum column —
// the 010 schema predates F11 — so the cryptographic anchor is the
// shared audit_hash_chain row, exactly as the manual-liquidation path
// already does. The payload argument to audit.Append stays nil so the
// chain row remains self-verifiable by `exchange verify-audit` without a
// payload provider (audit.Append doc contract); a content-binding
// checksum column is a schema amendment for a later remediation.
// ---------------------------------------------------------------------------

// AuditEntry is the write-side record for one admin action.
type AuditEntry struct {
	AdminUserID int64  // acting admin (users.id)
	Action      string // dotted action, e.g. "liquidation.manual", "support.ticket.assign"
	TargetType  string // e.g. "account", "support_ticket"; "" = no target
	TargetID    *int64
	BeforeState any // marshaled to jsonb; nil → NULL
	AfterState  any
	IPAddress   string // "" → NULL
}

// validate fails closed on the columns the schema can't hold.
func (e AuditEntry) validate() error {
	if e.AdminUserID <= 0 {
		return excerrors.New("INVALID_REQUEST", "audit entry requires an admin user id")
	}
	if e.Action == "" || len(e.Action) > 128 {
		return excerrors.New("INVALID_REQUEST", "audit action empty or > 128 chars")
	}
	if len(e.TargetType) > 64 {
		return excerrors.New("INVALID_REQUEST", "audit target_type > 64 chars")
	}
	return nil
}

// marshalState renders before/after state for the JSONB columns. nil
// state → nil (SQL NULL via Exec).
func marshalState(v any) (any, error) {
	if v == nil {
		return nil, nil
	}
	b, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	return json.RawMessage(b), nil
}

// Log appends one admin_audit_log row plus its audit_hash_chain link
// inside the caller's transaction. Returns the audit row id and the
// chain sequence number.
func Log(ctx context.Context, tx pgx.Tx, e AuditEntry) (auditID, chainSeq int64, err error) {
	if err := e.validate(); err != nil {
		return 0, 0, err
	}
	before, err := marshalState(e.BeforeState)
	if err != nil {
		return 0, 0, excerrors.Wrap("INVALID_REQUEST", "before_state not JSON-marshalable", err)
	}
	after, err := marshalState(e.AfterState)
	if err != nil {
		return 0, 0, excerrors.Wrap("INVALID_REQUEST", "after_state not JSON-marshalable", err)
	}

	var targetType any
	if e.TargetType != "" {
		targetType = e.TargetType
	}
	var targetID any
	if e.TargetID != nil {
		targetID = *e.TargetID
	}
	if err := tx.QueryRow(ctx, `
		INSERT INTO admin_audit_log
		    (admin_user_id, action, target_type, target_id,
		     before_state, after_state, ip_address)
		VALUES ($1, $2, $3, $4, $5, $6, NULLIF($7, '')::inet)
		RETURNING id`,
		e.AdminUserID, e.Action, targetType, targetID, before, after,
		e.IPAddress).Scan(&auditID); err != nil {
		return 0, 0, fmt.Errorf("admin audit insert: %w", err)
	}

	// Chain link in the same transaction — the audit row cannot commit
	// without its tamper-evident anchor (spec §5.40.3 fail-closed).
	chain, err := audit.Append(ctx, tx, "admin_audit_log", &auditID, "INSERT", nil)
	if err != nil {
		return 0, 0, fmt.Errorf("audit chain append: %w", err)
	}
	return auditID, chain.SequenceNum, nil
}

// retryableSQLSTATEs mirrors audit.isRetryable (unexported): unique
// violation on the chain sequence, serialization failure, deadlock —
// spec §5.40 whole-transaction retry classes.
var retryableSQLSTATEs = map[string]bool{
	"23505": true,
	"40001": true,
	"40P01": true,
}

func isRetryable(err error) bool {
	var pgErr *pgconn.PgError
	for err != nil {
		if e, ok := err.(*pgconn.PgError); ok {
			pgErr = e
			break
		}
		u, ok := err.(interface{ Unwrap() error })
		if !ok {
			break
		}
		err = u.Unwrap()
	}
	return pgErr != nil && retryableSQLSTATEs[pgErr.Code]
}

// LogAuto is the pool-based convenience form: runs Log in its own
// SERIALIZABLE transaction and retries the whole transaction on
// retryable conflicts (max 3 attempts, spec §5.40). For callers whose
// audited mutation already runs inside a transaction, use Log so the
// action and its audit row commit atomically — LogAuto only suits
// standalone audit events (e.g. support-view reads).
func LogAuto(ctx context.Context, pool *pgxpool.Pool, e AuditEntry) (auditID, chainSeq int64, err error) {
	var lastErr error
	for attempt := 1; attempt <= 3; attempt++ {
		tx, err := pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
		if err != nil {
			return 0, 0, fmt.Errorf("admin audit: begin tx: %w", err)
		}
		auditID, chainSeq, err = Log(ctx, tx, e)
		if err == nil {
			if err = tx.Commit(ctx); err == nil {
				return auditID, chainSeq, nil
			}
		}
		_ = tx.Rollback(ctx)
		lastErr = err
		if !isRetryable(err) {
			return 0, 0, err
		}
	}
	return 0, 0, excerrors.Wrap("TRANSACTION_CONFLICT_RETRY_EXHAUSTED",
		"admin audit append failed after 3 attempts", lastErr)
}

// ---------------------------------------------------------------------------
// Query surface — GET /api/v1/admin/audit-log (+/audit alias), Task 7.3.3
// item 3 / spec §14.11.3.
// ---------------------------------------------------------------------------

// AuditFilter narrows the admin_audit_log scan. Zero values match all.
type AuditFilter struct {
	AdminUserID  *int64
	Action       string // exact match
	ActionPrefix string // e.g. "account." — prefix match
	TargetType   string
	TargetID     *int64
	From         *time.Time // created_at >= From
	To           *time.Time // created_at < To
}

// AuditRow is one query result, joined with its hash-chain anchor.
type AuditRow struct {
	ID          int64           `json:"id"`
	AdminUserID int64           `json:"admin_user_id"`
	Action      string          `json:"action"`
	TargetType  string          `json:"target_type,omitempty"`
	TargetID    *int64          `json:"target_id,omitempty"`
	BeforeState json.RawMessage `json:"before_state,omitempty"`
	AfterState  json.RawMessage `json:"after_state,omitempty"`
	IPAddress   string          `json:"ip_address,omitempty"`
	CreatedAt   time.Time       `json:"created_at"`
	// AuditSeq is the row's audit_hash_chain sequence number — NULL when
	// a legacy row predates chain linking (tamper-evident gap).
	AuditSeq *int64 `json:"audit_seq,omitempty"`
}

// AuditPage carries the query result plus the keyset cursor for the next
// page (canonical (created_at,id) DESC order, spec §8.8).
type AuditPage struct {
	Rows     []AuditRow
	Total    int64
	HasMore  bool
	LastID   int64     // last row's id — feeds the next cursor
	LastTime time.Time // last row's created_at
}

// QueryAudit runs the filtered keyset scan over admin_audit_log. after is
// the decoded cursor position (nil = first page); the caller owns limit
// clamping (api.ListSpec).
func QueryAudit(ctx context.Context, pool *pgxpool.Pool, f AuditFilter,
	after *struct {
		Time time.Time
		ID   int64
	}, limit int) (*AuditPage, error) {

	where := "WHERE 1=1"
	args := []any{}
	next := func(v any) string {
		args = append(args, v)
		return fmt.Sprintf("$%d", len(args))
	}
	if f.AdminUserID != nil {
		where += " AND l.admin_user_id = " + next(*f.AdminUserID)
	}
	if f.Action != "" {
		where += " AND l.action = " + next(f.Action)
	}
	if f.ActionPrefix != "" {
		where += " AND l.action LIKE " + next(f.ActionPrefix+"%")
	}
	if f.TargetType != "" {
		where += " AND l.target_type = " + next(f.TargetType)
	}
	if f.TargetID != nil {
		where += " AND l.target_id = " + next(*f.TargetID)
	}
	if f.From != nil {
		where += " AND l.created_at >= " + next(*f.From)
	}
	if f.To != nil {
		where += " AND l.created_at < " + next(*f.To)
	}
	if after != nil {
		where += " AND (l.created_at, l.id) < (" + next(after.Time) + ", " + next(after.ID) + ")"
	}

	page := &AuditPage{Rows: []AuditRow{}}
	if err := pool.QueryRow(ctx,
		"SELECT count(*) FROM admin_audit_log l "+where, args...).Scan(&page.Total); err != nil {
		return nil, fmt.Errorf("admin audit count: %w", err)
	}

	q := `
		SELECT l.id, l.admin_user_id, l.action,
		       COALESCE(l.target_type,''), l.target_id,
		       l.before_state, l.after_state,
		       COALESCE(host(l.ip_address),''), l.created_at,
		       c.sequence_num
		  FROM admin_audit_log l
		  LEFT JOIN audit_hash_chain c
		         ON c.table_name = 'admin_audit_log' AND c.record_id = l.id
		 ` + where + `
		 ORDER BY l.created_at DESC, l.id DESC
		 LIMIT ` + fmt.Sprintf("%d", limit+1)

	rows, err := pool.Query(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("admin audit query: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var r AuditRow
		if err := rows.Scan(&r.ID, &r.AdminUserID, &r.Action, &r.TargetType,
			&r.TargetID, &r.BeforeState, &r.AfterState, &r.IPAddress,
			&r.CreatedAt, &r.AuditSeq); err != nil {
			return nil, fmt.Errorf("admin audit scan: %w", err)
		}
		page.Rows = append(page.Rows, r)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(page.Rows) > limit {
		page.HasMore = true
		page.Rows = page.Rows[:limit]
	}
	if n := len(page.Rows); n > 0 {
		page.LastID = page.Rows[n-1].ID
		page.LastTime = page.Rows[n-1].CreatedAt
	}
	return page, nil
}

// ---------------------------------------------------------------------------
// One-click integrity proof — GET /api/v1/admin/audit/verify?date=
// (Task 7.3.3 amended, remediation #26). Runs the same recomputation the
// `exchange verify-audit` CLI performs and returns the report.
// ---------------------------------------------------------------------------

// VerifyReport is the endpoint's report payload.
type VerifyReport struct {
	Date        string            `json:"date"`
	RowsChecked int               `json:"rows_checked"`
	OK          bool              `json:"ok"`
	Violations  []audit.Violation `json:"violations"`
	Merkle      VerifyMerkle      `json:"merkle"`
	CheckedAt   time.Time         `json:"checked_at"`
}

// VerifyMerkle reports the day-level Merkle anchor check.
type VerifyMerkle struct {
	StoredRoot bool   `json:"stored_root"`
	Verified   bool   `json:"verified"`
	Recomputed string `json:"recomputed_root,omitempty"`
	Violation  string `json:"violation,omitempty"`
}

// VerifyDay replays the chain from genesis through the end of date
// (UTC) and checks the stored daily Merkle root when one exists.
func VerifyDay(ctx context.Context, pool *pgxpool.Pool, date time.Time) (*VerifyReport, error) {
	date = date.UTC().Truncate(24 * time.Hour)
	rep := &VerifyReport{Date: date.Format("2006-01-02"),
		Violations: []audit.Violation{}, CheckedAt: time.Now().UTC()}

	chain, err := audit.VerifyThrough(ctx, pool, date.AddDate(0, 0, 1), nil)
	if err != nil {
		return nil, fmt.Errorf("chain verify: %w", err)
	}
	rep.RowsChecked = chain.RowsChecked
	rep.Violations = chain.Violations

	mv, recomputed, stored, err := audit.VerifyDayMerkle(ctx, pool, date)
	if err != nil {
		return nil, fmt.Errorf("merkle verify: %w", err)
	}
	rep.Merkle = VerifyMerkle{StoredRoot: stored, Recomputed: recomputed,
		Verified: stored && mv == nil}
	if mv != nil {
		rep.Merkle.Violation = mv.String()
	}

	rep.OK = chain.OK() && mv == nil
	return rep, nil
}
