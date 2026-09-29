// Task 5.3.42 item 1 — the unified list envelope.
//
// Every paginated GET returns:
//
//	{"data": [...], "next_cursor": "...", "limit": 100, "total": N}
//
// with cursor pagination over the stable (created_at, id) keyset —
// OFFSET paging drifts under concurrent inserts; keyset does not. The
// per-endpoint default/max limits and sort/filter matrices are tabulated
// in ListSpecs and published at GET /api/v1/meta/pagination (the task
// text wants them on the /api/v1/routes dump; the Route struct is frozen
// by another cluster's ownership so the matrix gets its own meta route —
// drift gap reported to the spec owners).
package api

import (
	"encoding/base64"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// ListSpec is one endpoint's pagination contract.
type ListSpec struct {
	Path       string   `json:"path"`
	Default    int      `json:"default_limit"`
	Max        int      `json:"max_limit"`
	Sortable   []string `json:"sortable"`   // whitelist; canonical order (created_at,id)
	Filterable []string `json:"filterable"` // query-param whitelist
}

// ListSpecs is the §8.8 item 1 tabulation. "limit=500/1500 vs limit=100"
// drift is reconciled by declaring every list endpoint here — handlers
// MUST source default/max from this table, never hardcode.
var ListSpecs = []ListSpec{
	{Path: "/api/v1/orders", Default: 100, Max: 1000,
		Sortable: []string{"created_at", "id"},
		Filterable: []string{
			"symbol", "status", "side", "type", "client_order_id", "from", "to"}},
	{Path: "/api/v1/trades", Default: 100, Max: 1000,
		Sortable:   []string{"created_at", "id"},
		Filterable: []string{"symbol", "side", "from", "to"}},
	{Path: "/api/v1/funding", Default: 100, Max: 500,
		Sortable:   []string{"created_at", "id"},
		Filterable: []string{"currency", "type", "status", "from", "to"}},
	{Path: "/api/v1/positions", Default: 100, Max: 500,
		Sortable:   []string{"created_at", "id"},
		Filterable: []string{"symbol", "side"}},
	{Path: "/api/v1/account/login-history", Default: 50, Max: 200,
		Sortable:   []string{"created_at", "id"},
		Filterable: []string{"from", "to"}},
	{Path: "/api/v1/account/sessions", Default: 50, Max: 200,
		Sortable:   []string{"created_at", "id"},
		Filterable: []string{"status"}},
	{Path: "/api/v1/klines/{symbol}", Default: 500, Max: 1500,
		Sortable:   []string{"open_time"},
		Filterable: []string{"interval", "start_time", "end_time"}},
	{Path: "/api/v1/history/ticks/{symbol}", Default: 100, Max: 1000,
		Sortable:   []string{"ts"},
		Filterable: []string{"from", "to"}},
	{Path: "/api/v1/transfers", Default: 100, Max: 500,
		Sortable:   []string{"created_at", "id"},
		Filterable: []string{"currency", "direction", "from", "to"}},
	// Phase-07 Task 7.3.7 support surfaces.
	{Path: "/api/v1/support/tickets", Default: 50, Max: 200,
		Sortable:   []string{"created_at", "id"},
		Filterable: []string{"status"}},
	{Path: "/api/v1/admin/support/tickets", Default: 100, Max: 500,
		Sortable: []string{"created_at", "id"},
		Filterable: []string{
			"status", "category", "type", "queue", "assignee",
			"account_id", "unassigned", "breached"}},
	{Path: "/api/v1/admin/support/complaints/register", Default: 100, Max: 500,
		Sortable:   []string{"created_at", "id"},
		Filterable: []string{"status"}},
	// Phase-13.5 Task 13.5.3.8 VDP register.
	{Path: "/api/v1/admin/security/disclosures", Default: 100, Max: 500,
		Sortable: []string{"created_at", "id"},
		Filterable: []string{
			"status", "severity", "source", "bulletin",
			"assignee", "unassigned", "breached"}},
	// Phase-07 Task 7.3.3 audit query (both paths share the handler).
	{Path: "/api/v1/admin/audit-log", Default: 100, Max: 1000,
		Sortable: []string{"created_at", "id"},
		Filterable: []string{
			"admin_user_id", "action", "action_prefix",
			"target_type", "target_id", "from", "to"}},
	{Path: "/api/v1/admin/audit", Default: 100, Max: 1000,
		Sortable: []string{"created_at", "id"},
		Filterable: []string{
			"admin_user_id", "action", "action_prefix",
			"target_type", "target_id", "from", "to"}},
}

// ListSpecFor resolves the spec row for a route path ("" key tolerated).
func ListSpecFor(path string) *ListSpec {
	for i := range ListSpecs {
		if ListSpecs[i].Path == path {
			return &ListSpecs[i]
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// Cursor — opaque keyset token over (created_at, id).
// Wire form: base64url("ts_nanos:id") — intentionally NOT self-describing
// JSON so clients treat it as opaque (§8.8: cursor is an opaque token).
// ---------------------------------------------------------------------------

// Cursor is the decoded (created_at, id) keyset position.
type Cursor struct {
	CreatedAt time.Time
	ID        int64
}

// EncodeCursor renders the opaque token.
func EncodeCursor(c Cursor) string {
	raw := fmt.Sprintf("%d:%d", c.CreatedAt.UTC().UnixNano(), c.ID)
	return base64.RawURLEncoding.EncodeToString([]byte(raw))
}

// DecodeCursor parses the opaque token. Any deviation — bad base64, bad
// shape, non-numeric fields — is an error; callers map it to
// INVALID_REQUEST rather than silently restarting the page stream.
func DecodeCursor(s string) (Cursor, error) {
	raw, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return Cursor{}, fmt.Errorf("cursor not base64url")
	}
	parts := strings.SplitN(string(raw), ":", 2)
	if len(parts) != 2 {
		return Cursor{}, fmt.Errorf("cursor malformed")
	}
	ns, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil {
		return Cursor{}, fmt.Errorf("cursor timestamp malformed")
	}
	id, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil {
		return Cursor{}, fmt.Errorf("cursor id malformed")
	}
	return Cursor{CreatedAt: time.Unix(0, ns).UTC(), ID: id}, nil
}

// ListParams carries the parsed pagination inputs for one request.
type ListParams struct {
	Limit   int       `json:"limit"`
	Cursor  string    `json:"cursor,omitempty"` // raw client token
	Decoded *Cursor   `json:"-"`                // set when Cursor parses
	Spec    *ListSpec `json:"-"`
}

// ParseListParams validates ?limit=&cursor= against the endpoint's
// ListSpec. Errors return (nil, message) — handlers emit
// INVALID_REQUEST 400 with it. spec==nil uses the 100/1000 baseline.
func ParseListParams(r *http.Request, spec *ListSpec) (*ListParams, error) {
	if spec == nil {
		spec = &ListSpec{Default: 100, Max: 1000}
	}
	p := &ListParams{Limit: spec.Default, Spec: spec}
	q := r.URL.Query()

	if raw := q.Get("limit"); raw != "" {
		v, err := strconv.Atoi(raw)
		if err != nil || v < 1 {
			return nil, fmt.Errorf("limit must be a positive integer")
		}
		if v > spec.Max {
			return nil, fmt.Errorf("limit %d exceeds max %d", v, spec.Max)
		}
		p.Limit = v
	}
	if raw := q.Get("cursor"); raw != "" {
		c, err := DecodeCursor(raw)
		if err != nil {
			return nil, fmt.Errorf("invalid cursor: %v", err)
		}
		p.Cursor = raw
		p.Decoded = &c
	}
	return p, nil
}

// CursorClause returns the SQL predicate + args for the keyset page —
// "WHERE (created_at, id) < ($n, $n+1)" for DESC streams. argStart is the
// next free $n position. Callers append ORDER BY created_at DESC, id DESC.
func (p *ListParams) CursorClause(argStart int) (clause string, args []any) {
	if p.Decoded == nil {
		return "", nil
	}
	return fmt.Sprintf(" AND (created_at, id) < ($%d, $%d)", argStart, argStart+1),
		[]any{p.Decoded.CreatedAt, p.Decoded.ID}
}

// ---------------------------------------------------------------------------
// Envelope
// ---------------------------------------------------------------------------

// ListEnvelope is the wire shape every paginated GET emits.
type ListEnvelope struct {
	Data       any    `json:"data"`
	NextCursor string `json:"next_cursor"`
	Limit      int    `json:"limit"`
	Total      int64  `json:"total"`
}

// NewListEnvelope builds the envelope; nextCursor is "" when the page is
// terminal (empty string rather than absent keeps the contract stable).
func NewListEnvelope(data any, p *ListParams, rows []Cursor, total int64) ListEnvelope {
	env := ListEnvelope{Data: data, Limit: p.Limit, Total: total}
	if len(rows) == p.Limit {
		// A full page implies a possible successor — the cursor is the
		// last row's keyset position.
		env.NextCursor = EncodeCursor(rows[len(rows)-1])
	}
	return env
}

// PageCursors extracts (created_at,id) per row via the accessor.
func PageCursors[T any](rows []T, f func(T) (time.Time, int64)) []Cursor {
	out := make([]Cursor, len(rows))
	for i, r := range rows {
		ca, id := f(r)
		out[i] = Cursor{CreatedAt: ca, ID: id}
	}
	return out
}

// ListMetaHandler serves GET /api/v1/meta/pagination — the published
// sort/filter matrix + limit tabulation the OpenAPI document consumes.
func ListMetaHandler(w http.ResponseWriter, _ *http.Request) {
	WriteJSON(w, http.StatusOK, map[string]any{
		"envelope": map[string]any{
			"shape":         "{data, next_cursor, limit, total}",
			"cursor_order":  "(created_at, id) DESC",
			"cursor_opaque": true,
		},
		"endpoints": ListSpecs,
	})
}
