// Phase-21 Task 21.3.27 (part 3) — audit-trail query service.
//
// GET /api/v1/admin/audit/chain + /audit/export (Read-Only Auditor)
// search BOTH admin_audit_log (the action journal) and
// audit_hash_chain (the tamper-evident anchor), joined on
// (table_name='admin_audit_log', record_id=admin_audit_log.id) — the
// join produces the proof-bearing row: who did what, plus the chain
// sequence/hash pinning it.
//
// Masked export (mask=1) is the auditor-shareable view: ip_address is
// truncated to a /24 and PII-shaped keys inside before/after_state
// (email|name|tin|address|phone|document) collapse to "***" — the
// compliance officer sees raw rows; the auditor export is shareable.
package compliance

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"regexp"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	excerrors "exchange/pkg/errors"
)

// AuditTrailRow is one joined audit entry + chain anchor.
type AuditTrailRow struct {
	ID          int64           `json:"id"`
	AdminUserID int64           `json:"admin_user_id"`
	Action      string          `json:"action"`
	TargetType  string          `json:"target_type,omitempty"`
	TargetID    *int64          `json:"target_id,omitempty"`
	BeforeState json.RawMessage `json:"before_state,omitempty"`
	AfterState  json.RawMessage `json:"after_state,omitempty"`
	IPAddress   *string         `json:"ip_address,omitempty"`
	CreatedAt   time.Time       `json:"created_at"`
	// Chain anchor — NULL when the row predates the chain binding.
	ChainSeq     *int64     `json:"chain_seq,omitempty"`
	ChainHash    *string    `json:"chain_hash,omitempty"`
	ChainPrev    *string    `json:"chain_prev,omitempty"`
	ChainCreated *time.Time `json:"chain_created_at,omitempty"`
}

// AuditTrailFilter bounds the query (all optional; keyset by id DESC).
type AuditTrailFilter struct {
	ActorID    int64
	Action     string // exact or prefix ("liquidation.%")
	TargetType string
	TargetID   int64
	From       time.Time
	To         time.Time
	AfterID    int64 // keyset: rows with id < AfterID
	Limit      int
	Mask       bool // truncate ip + scrub PII keys in state blobs
}

// AuditTrailQuery serves the auditor surface over admin_audit_log ⨝
// audit_hash_chain.
type AuditTrailQuery struct {
	pool *pgxpool.Pool
}

// NewAuditTrailQuery binds the read service.
func NewAuditTrailQuery(pool *pgxpool.Pool) *AuditTrailQuery {
	return &AuditTrailQuery{pool: pool}
}

// Search runs the joined query. Chain coverage is LEFT — audit rows
// that predate the chain binding still surface (with null anchors),
// which is itself auditable signal.
func (s *AuditTrailQuery) Search(ctx context.Context,
	f AuditTrailFilter) ([]AuditTrailRow, error) {
	limit := f.Limit
	if limit <= 0 || limit > 500 {
		limit = 200
	}
	q := `
		SELECT a.id, a.admin_user_id, a.action, a.target_type,
		       a.target_id, a.before_state, a.after_state,
		       a.ip_address::text, a.created_at,
		       c.sequence_num, c.payload_hash, c.prev_hash, c.created_at
		  FROM admin_audit_log a
		  LEFT JOIN audit_hash_chain c
		    ON c.table_name = 'admin_audit_log' AND c.record_id = a.id
		 WHERE 1=1`
	args := []any{}
	// The placeholder ordinal is the arg count AFTER append — build
	// the condition inside add so $n always matches args[n-1].
	add := func(v any, condFmt string) {
		args = append(args, v)
		q += " AND " + fmt.Sprintf(condFmt, len(args))
	}
	if f.ActorID > 0 {
		add(f.ActorID, "a.admin_user_id = $%d")
	}
	if f.Action != "" {
		if strings.HasSuffix(f.Action, "%") || strings.HasSuffix(f.Action, ".") {
			add(strings.TrimSuffix(f.Action, "%")+"%", "a.action LIKE $%d")
		} else {
			add(f.Action, "a.action = $%d")
		}
	}
	if f.TargetType != "" {
		add(f.TargetType, "a.target_type = $%d")
	}
	if f.TargetID > 0 {
		add(f.TargetID, "a.target_id = $%d")
	}
	if !f.From.IsZero() {
		add(f.From.UTC(), "a.created_at >= $%d")
	}
	if !f.To.IsZero() {
		add(f.To.UTC(), "a.created_at < $%d")
	}
	if f.AfterID > 0 {
		add(f.AfterID, "a.id < $%d")
	}
	q += " ORDER BY a.id DESC LIMIT " + itoa(limit)

	rows, err := s.pool.Query(ctx, q, args...)
	if err != nil {
		return nil, excerrors.New("INTERNAL_ERROR",
			"audit trail query: "+err.Error())
	}
	defer rows.Close()
	var out []AuditTrailRow
	for rows.Next() {
		var r AuditTrailRow
		if err := rows.Scan(&r.ID, &r.AdminUserID, &r.Action,
			&r.TargetType, &r.TargetID, &r.BeforeState, &r.AfterState,
			&r.IPAddress, &r.CreatedAt, &r.ChainSeq, &r.ChainHash,
			&r.ChainPrev, &r.ChainCreated); err != nil {
			return nil, excerrors.New("INTERNAL_ERROR",
				"audit trail row: "+err.Error())
		}
		if f.Mask {
			r.mask()
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// ChainSlice returns raw audit_hash_chain rows — the auditor's
// chain-integrity lane (table/action/window filters; keyset by
// sequence_num DESC).
func (s *AuditTrailQuery) ChainSlice(ctx context.Context, tableName,
	action string, from, to time.Time, afterSeq int64,
	limit int) ([]map[string]any, error) {
	if limit <= 0 || limit > 500 {
		limit = 200
	}
	q := `SELECT id, sequence_num, table_name, record_id, action,
	             payload_hash, prev_hash, created_at
	        FROM audit_hash_chain WHERE 1=1`
	args := []any{}
	add := func(v any, condFmt string) {
		args = append(args, v)
		q += " AND " + fmt.Sprintf(condFmt, len(args))
	}
	if tableName != "" {
		add(tableName, "table_name = $%d")
	}
	if action != "" {
		add(action, "action = $%d")
	}
	if !from.IsZero() {
		add(from.UTC(), "created_at >= $%d")
	}
	if !to.IsZero() {
		add(to.UTC(), "created_at < $%d")
	}
	if afterSeq > 0 {
		add(afterSeq, "sequence_num < $%d")
	}
	q += " ORDER BY sequence_num DESC LIMIT " + itoa(limit)
	rows, err := s.pool.Query(ctx, q, args...)
	if err != nil {
		return nil, excerrors.New("INTERNAL_ERROR", "chain slice: "+err.Error())
	}
	defer rows.Close()
	var out []map[string]any
	for rows.Next() {
		var (
			id, seq, recID int64
			table, act     string
			hash, prev     []byte
			created        time.Time
		)
		if err := rows.Scan(&id, &seq, &table, &recID, &act,
			&hash, &prev, &created); err != nil {
			return nil, excerrors.New("INTERNAL_ERROR",
				"chain row: "+err.Error())
		}
		out = append(out, map[string]any{
			"id": id, "sequence_num": seq, "table_name": table,
			"record_id": recID, "action": act,
			"payload_hash": fmt.Sprintf("%x", hash),
			"prev_hash":    fmt.Sprintf("%x", prev),
			"created_at":   created,
		})
	}
	return out, rows.Err()
}

// ---------------------------------------------------------------------------
// Masking — auditor-shareable export
// ---------------------------------------------------------------------------

// piiKey matches state keys that carry personal data.
var piiKey = regexp.MustCompile(`(?i)(email|tin|ssn|name|address|phone|passport|document|dob|birth)`)

// mask truncates the IP and scrub-walks before/after_state.
func (r *AuditTrailRow) mask() {
	if r.IPAddress != nil && *r.IPAddress != "" {
		*r.IPAddress = maskIP(*r.IPAddress)
	}
	if len(r.BeforeState) > 0 {
		r.BeforeState = scrubJSON(r.BeforeState)
	}
	if len(r.AfterState) > 0 {
		r.AfterState = scrubJSON(r.AfterState)
	}
}

// maskIP zeroes the host octets — "203.0.113.7" → "203.0.113.0/24",
// v6 → first 3 hextets. Unparseable → "***".
func maskIP(ip string) string {
	parsed := net.ParseIP(ip)
	if parsed == nil {
		return "***"
	}
	if v4 := parsed.To4(); v4 != nil {
		return net.IPv4(v4[0], v4[1], v4[2], 0).String() + "/24"
	}
	segs := strings.Split(ip, ":")
	if len(segs) > 3 {
		return strings.Join(segs[:3], ":") + "::/48"
	}
	return "***"
}

// scrubJSON rewrites PII-keyed leaves to "***" recursively.
func scrubJSON(raw json.RawMessage) json.RawMessage {
	var v any
	if json.Unmarshal(raw, &v) != nil {
		return json.RawMessage(`"***"`)
	}
	out, err := json.Marshal(scrubValue(v))
	if err != nil {
		return json.RawMessage(`"***"`)
	}
	return out
}

func scrubValue(v any) any {
	switch t := v.(type) {
	case map[string]any:
		for k, val := range t {
			if piiKey.MatchString(k) {
				t[k] = "***"
			} else {
				t[k] = scrubValue(val)
			}
		}
		return t
	case []any:
		for i, val := range t {
			t[i] = scrubValue(val)
		}
		return t
	default:
		return v
	}
}

func itoa(n int) string {
	const digits = "0123456789"
	if n == 0 {
		return "0"
	}
	var b [8]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = digits[n%10]
		n /= 10
	}
	return string(b[i:])
}
