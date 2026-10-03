// Phase-05 Wave-2 cluster A — order pipeline REST surface.
//
// Tasks 5.3.3 / 5.3.22 / 5.3.24 / 5.3.25 / 5.3.32 / 5.3.37 / 5.3.39.
// Handlers are thin: auth claim resolution (JWT claims or the §8.1
// HMAC-signed API-key path), scope gates, body decode, service call,
// envelope emit. All business semantics live in internal/orders.
package api

import (
	"context"
	"encoding/json"
	stderrors "errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"exchange/internal/auth"
	"exchange/internal/gateway"
	"exchange/internal/middleware"
	"exchange/internal/orders"
	excerrors "exchange/pkg/errors"
)

// OrderDeps bundles the handler's seams. SVC is the pipeline service;
// Verifier enables the HMAC API-key path (nil = Bearer-only, signature
// headers are not honored); RoleResolver resolves admin roles for the
// audit/mass-cancel admin surfaces — nil fails closed per the Phase-07
// boundary convention.
type OrderDeps struct {
	SVC          *orders.Service
	Verifier     *auth.SignatureVerifier
	TrustProxy   bool
	RoleResolver func(ctx context.Context, adminUserID int64) (string, error)
	// SymbolFor resolves instrument_id → canonical symbol for order
	// views — the wire contract private:orders and the UI's order
	// parser both key on. Nil leaves the view's instrument_id only.
	SymbolFor func(instrumentID int64) (string, bool)
}

// orderView renders o per the §5.3 wire contract, decorating the
// canonical symbol clients key on (the REST row only carries
// instrument_id; OrderEvent.Symbol is the private-stream shape).
func (d *OrderDeps) orderView(o *orders.Order) map[string]any {
	v := o.View()
	if d.SymbolFor != nil {
		if sym, ok := d.SymbolFor(o.InstrumentID); ok && sym != "" {
			v["symbol"] = sym
		}
	}
	return v
}

// actor renders the audit actor string from the auth context.
func actorOf(c *auth.Claims) string {
	if c.Subject != "" {
		return c.Subject
	}
	return "account:" + strconv.FormatInt(c.AccountID, 10)
}

// orderAuth resolves the request identity: Bearer claims first; when
// absent but X-API-KEY signing headers are present, the HMAC verifier
// (Task 5.3.24) runs — signature → synthesized claims bound to the
// key's account + scopes. Need is the required §8.8 scope ("read" or
// "trade"). Any failure writes the envelope and returns false.
func (d *OrderDeps) orderAuth(w http.ResponseWriter, r *http.Request,
	body []byte, need string) (*auth.Claims, *orders.Account, bool) {
	reqID := gateway.RequestIDFrom(r.Context())
	c := auth.ClaimsFrom(r.Context())
	if c == nil && d.Verifier != nil && r.Header.Get("X-API-KEY") != "" {
		key, err := d.Verifier.Verify(r.Context(),
			auth.SignedRequestFromHTTP(r, body, d.TrustProxy))
		if err != nil {
			writeServiceErr(w, r, err)
			return nil, nil, false
		}
		c = &auth.Claims{
			Subject:   "apikey:" + key.KeyID,
			AccountID: key.AccountID,
			Scopes:    key.Scopes,
			KeyID:     key.KeyID,
		}
	}
	if c == nil || c.AccountID == 0 {
		WriteError(w, "UNAUTHORIZED", "authentication required", reqID, nil)
		return nil, nil, false
	}
	if !c.HasScope(need) && !c.HasScope(gateway.ScopeAdmin) {
		WriteError(w, "INSUFFICIENT_SCOPE",
			"endpoint requires scope "+need, reqID, nil)
		return nil, nil, false
	}
	acct, err := d.SVC.AccountByID(r.Context(), c.AccountID)
	if err != nil {
		writeServiceErr(w, r, err)
		return nil, nil, false
	}
	if acct == nil {
		WriteError(w, "ACCOUNT_NOT_FOUND", "account not found", reqID, nil)
		return nil, nil, false
	}
	return c, acct, true
}

func readBody(r *http.Request) ([]byte, error) {
	return io.ReadAll(io.LimitReader(r.Body, 1<<20+1))
}

func orderPathID(w http.ResponseWriter, r *http.Request) (int64, bool) {
	id, ok := parsePathID(r.PathValue("id"))
	if !ok {
		WriteError(w, "INVALID_REQUEST", "order id must be a positive integer",
			gateway.RequestIDFrom(r.Context()), nil)
		return 0, false
	}
	return id, true
}

// ---------------------------------------------------------------------------
// POST /api/v1/orders — submit (Tasks 5.3.3/5.3.24/5.3.39)
// ---------------------------------------------------------------------------

// OrderSubmit validates → persists → dispatches → acks. The
// client_order_id dedup contract (§8.1 idempotent submission) lives in
// the service; this handler only transports.
func OrderSubmit(d *OrderDeps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		body, err := readBody(r)
		if err != nil {
			WriteError(w, "INVALID_REQUEST", "request body unreadable",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		c, acct, ok := d.orderAuth(w, r, body, gateway.ScopeTrade)
		if !ok {
			return
		}
		req, err := orders.ParseSubmit(body)
		if err != nil {
			WriteError(w, "INVALID_REQUEST", err.Error(),
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		req.SessionID = c.SessionID // server-side session attribution (CoD)
		ack, err := d.SVC.Submit(r.Context(), acct, req)
		if err != nil {
			writeServiceErr(w, r, err)
			return
		}
		if ack.Replay {
			w.Header().Set("Idempotency-Replayed", "true")
		}
		WriteJSON(w, http.StatusAccepted, ack)
	}
}

// ---------------------------------------------------------------------------
// POST /api/v1/orders/basket — cross-shard basket submit (Phase-3 Task 4,
// spec §2.2a). Same auth/account pipeline as OrderSubmit; legs execute via
// the OptimisticShardCoordinator's TRY_MATCH path, terminal outcome lands
// as a BasketResult event (bridge → baskets table → BasketGet projection).
// ---------------------------------------------------------------------------

// BasketSubmit validates → persists legs + op rows → dispatches to the
// coordinator shard → acks MATCHING with the 128-bit op id.
func BasketSubmit(d *OrderDeps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		body, err := readBody(r)
		if err != nil {
			WriteError(w, "INVALID_REQUEST", "request body unreadable",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		c, acct, ok := d.orderAuth(w, r, body, gateway.ScopeTrade)
		if !ok {
			return
		}
		_ = c
		req, err := orders.ParseBasketSubmit(body)
		if err != nil {
			WriteError(w, "INVALID_REQUEST", err.Error(),
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		ack, err := d.SVC.SubmitBasket(r.Context(), acct, req)
		if err != nil {
			writeServiceErr(w, r, err)
			return
		}
		WriteJSON(w, http.StatusAccepted, ack)
	}
}

// BasketGet — GET /api/v1/baskets/{op_id}: the op row + leg order
// statuses (migration 283).
func BasketGet(d *OrderDeps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		c, acct, ok := d.orderAuth(w, r, nil, gateway.ScopeRead)
		if !ok {
			return
		}
		_ = c
		st, err := d.SVC.BasketStatus(r.Context(), acct,
			r.PathValue("op_id"))
		if err != nil {
			writeServiceErr(w, r, err)
			return
		}
		WriteJSON(w, http.StatusOK, st)
	}
}

// ---------------------------------------------------------------------------
// PUT /api/v1/orders/{id} — modify (Tasks 5.3.3/5.3.22)
// ---------------------------------------------------------------------------

func OrderModify(d *OrderDeps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, ok := orderPathID(w, r)
		if !ok {
			return
		}
		body, err := readBody(r)
		if err != nil {
			WriteError(w, "INVALID_REQUEST", "request body unreadable",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		c, acct, ok := d.orderAuth(w, r, body, gateway.ScopeTrade)
		if !ok {
			return
		}
		req, err := orders.ParseModify(body)
		if err != nil {
			WriteError(w, "INVALID_REQUEST", err.Error(),
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		o, err := d.SVC.Modify(r.Context(), acct, id, req,
			actorOf(c), gateway.RequestIDFrom(r.Context()),
			middleware.ClientIP(r, d.TrustProxy))
		if err != nil {
			writeServiceErr(w, r, err)
			return
		}
		WriteJSON(w, http.StatusOK, d.orderView(o))
	}
}

// ---------------------------------------------------------------------------
// DELETE /api/v1/orders/{id} — cancel (Task 5.3.3)
// ---------------------------------------------------------------------------

func OrderCancel(d *OrderDeps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, ok := orderPathID(w, r)
		if !ok {
			return
		}
		body, _ := readBody(r) // DELETE may carry a signed empty body
		c, acct, ok := d.orderAuth(w, r, body, gateway.ScopeTrade)
		if !ok {
			return
		}
		ack, err := d.SVC.Cancel(r.Context(), acct, id,
			actorOf(c), gateway.RequestIDFrom(r.Context()),
			middleware.ClientIP(r, d.TrustProxy))
		if err != nil {
			writeServiceErr(w, r, err)
			return
		}
		WriteJSON(w, http.StatusOK, ack)
	}
}

// ---------------------------------------------------------------------------
// DELETE /api/v1/orders/all + /api/v1/orders?symbol= — mass cancel
// (Tasks 5.3.3/5.3.25)
// ---------------------------------------------------------------------------

func (d *OrderDeps) massCancel(w http.ResponseWriter, r *http.Request, allInstruments bool) {
	body, _ := readBody(r)
	c, acct, ok := d.orderAuth(w, r, body, gateway.ScopeTrade)
	if !ok {
		return
	}
	q := r.URL.Query()
	scope := orders.MassCancelScope{
		AccountID: acct.ID,
		Reason:    "client",
		Side:      strings.ToUpper(strings.TrimSpace(q.Get("side"))),
		OrderType: strings.ToUpper(strings.TrimSpace(q.Get("type"))),
	}
	if !allInstruments {
		sym := strings.TrimSpace(q.Get("symbol"))
		if sym == "" {
			WriteError(w, "INVALID_REQUEST",
				"symbol query parameter is required for scoped mass cancel",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		inst, err := d.SVC.InstrumentBySymbol(r.Context(), sym)
		if err != nil {
			writeServiceErr(w, r, err)
			return
		}
		if inst == nil {
			WriteError(w, "INVALID_REQUEST", "unknown symbol "+strconv.Quote(sym),
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		scope.InstrumentID = inst.ID
	}
	res, err := d.SVC.MassCancel(r.Context(), scope,
		actorOf(c), gateway.RequestIDFrom(r.Context()),
		middleware.ClientIP(r, d.TrustProxy))
	if err != nil {
		writeServiceErr(w, r, err)
		return
	}
	WriteJSON(w, http.StatusOK, map[string]any{
		"cancelled":  res.Cancelled,
		"per_symbol": res.PerSymbol,
	})
}

// OrderCancelAll serves DELETE /api/v1/orders/all — every open order of
// the authenticated account.
func OrderCancelAll(d *OrderDeps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		d.massCancel(w, r, true)
	}
}

// OrderMassCancel serves DELETE /api/v1/orders — the scoped variant
// (?symbol= required; side= and type= optional filters).
func OrderMassCancel(d *OrderDeps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		d.massCancel(w, r, false)
	}
}

// ---------------------------------------------------------------------------
// GET /api/v1/orders + /api/v1/orders/{id} — reads (Task 5.3.3)
// ---------------------------------------------------------------------------

// OrderList is cursor-paged order history over the (created_at,id)
// keyset, per the shared ListSpec envelope.
func OrderList(d *OrderDeps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		c, acct, ok := d.orderAuth(w, r, nil, gateway.ScopeRead)
		if !ok {
			return
		}
		_ = c
		p, err := ParseListParams(r, ListSpecFor("/api/v1/orders"))
		if err != nil {
			WriteError(w, "INVALID_REQUEST", err.Error(),
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		q := r.URL.Query()
		lq := orders.ListQuery{Limit: p.Limit} // store fetches limit+1 internally
		if p.Decoded != nil {
			lq.CursorAt = p.Decoded.CreatedAt
			lq.CursorID = p.Decoded.ID
		}
		lq.Status = strings.ToUpper(strings.TrimSpace(q.Get("status")))
		lq.Side = strings.ToUpper(strings.TrimSpace(q.Get("side")))
		lq.OrderType = strings.ToUpper(strings.TrimSpace(q.Get("type")))
		lq.ClientOrderID = strings.TrimSpace(q.Get("client_order_id"))
		if sym := strings.TrimSpace(q.Get("symbol")); sym != "" {
			inst, ierr := d.SVC.InstrumentBySymbol(r.Context(), sym)
			if ierr != nil {
				writeServiceErr(w, r, ierr)
				return
			}
			if inst == nil {
				WriteError(w, "INVALID_REQUEST", "unknown symbol "+strconv.Quote(sym),
					gateway.RequestIDFrom(r.Context()), nil)
				return
			}
			lq.InstrumentID = inst.ID
		}
		if t, terr := parseTimeQuery(q.Get("from")); terr != nil {
			WriteError(w, "INVALID_REQUEST", "from must be RFC3339",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		} else {
			lq.From = t
		}
		if t, terr := parseTimeQuery(q.Get("to")); terr != nil {
			WriteError(w, "INVALID_REQUEST", "to must be RFC3339",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		} else {
			lq.To = t
		}
		rows, total, err := d.SVC.ListOrders(r.Context(), acct, lq)
		if err != nil {
			writeServiceErr(w, r, err)
			return
		}
		page := rows
		if len(page) > p.Limit {
			page = page[:p.Limit]
		}
		views := make([]map[string]any, 0, len(page))
		for i := range page {
			views = append(views, d.orderView(&page[i]))
		}
		env := NewListEnvelope(views, p,
			PageCursors(page, func(o orders.Order) (time.Time, int64) {
				return o.CreatedAt, o.ID
			}), total)
		WriteJSON(w, http.StatusOK, env)
	}
}

// ---------------------------------------------------------------------------
// GET /api/v1/orders/{id}
// ---------------------------------------------------------------------------

func OrderGet(d *OrderDeps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, ok := orderPathID(w, r)
		if !ok {
			return
		}
		_, acct, ok := d.orderAuth(w, r, nil, gateway.ScopeRead)
		if !ok {
			return
		}
		o, err := d.SVC.GetOrder(r.Context(), acct, id)
		if err != nil {
			writeServiceErr(w, r, err)
			return
		}
		WriteJSON(w, http.StatusOK, d.orderView(o))
	}
}

// ---------------------------------------------------------------------------
// POST /api/v1/orders/batch + DELETE /api/v1/orders/batch (Task 5.3.32)
// ---------------------------------------------------------------------------

// batchSubmitPayload is the documented {"orders":[...]} envelope.
type batchSubmitPayload struct {
	Orders []json.RawMessage `json:"orders"`
}

// OrderBatchSubmit is strictly atomic per the spec rule "batch
// operations never partially apply" — the service validates all
// entries before any dispatch; on failure the error envelope carries
// details.results with the per-entry verdicts.
func OrderBatchSubmit(d *OrderDeps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		body, err := readBody(r)
		if err != nil {
			WriteError(w, "INVALID_REQUEST", "request body unreadable",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		c, acct, ok := d.orderAuth(w, r, body, gateway.ScopeTrade)
		if !ok {
			return
		}
		var env batchSubmitPayload
		if err := json.Unmarshal(body, &env); err != nil || env.Orders == nil {
			WriteError(w, "INVALID_REQUEST",
				"body must be {\"orders\":[...]}", gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		reqs := make([]*orders.SubmitRequest, len(env.Orders))
		for i, raw := range env.Orders {
			req, perr := orders.ParseSubmit(raw)
			if perr != nil {
				WriteError(w, "INVALID_REQUEST",
					"orders["+strconv.Itoa(i)+"]: "+perr.Error(),
					gateway.RequestIDFrom(r.Context()), nil)
				return
			}
			req.SessionID = c.SessionID
			reqs[i] = req
		}
		results, err := d.SVC.BatchSubmit(r.Context(), acct, reqs,
			gateway.RequestIDFrom(r.Context()),
			middleware.ClientIP(r, d.TrustProxy))
		if err != nil {
			var bf *orders.BatchSubmitFailure
			if stderrors.As(err, &bf) {
				writeServiceErrDetails(w, r, bf.Err,
					map[string]any{"results": bf.Results})
				return
			}
			writeServiceErr(w, r, err)
			return
		}
		WriteJSON(w, http.StatusOK, map[string]any{"results": results})
	}
}

// batchCancelPayload accepts order_ids and/or client_order_ids (spec
// Task 5.3.32 item 2 — totals cap at 20 references). order_ids accepts
// JSON numbers or digit strings.
type batchCancelPayload struct {
	OrderIDs       []json.RawMessage `json:"order_ids"`
	ClientOrderIDs []string          `json:"client_order_ids"`
}

// OrderBatchCancel resolves every reference first (a single unknown or
// foreign id aborts the batch — atomicity) then dispatches per-order
// cancels through the engine-confirmation path.
func OrderBatchCancel(d *OrderDeps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		body, err := readBody(r)
		if err != nil {
			WriteError(w, "INVALID_REQUEST", "request body unreadable",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		c, acct, ok := d.orderAuth(w, r, body, gateway.ScopeTrade)
		if !ok {
			return
		}
		var env batchCancelPayload
		if err := json.Unmarshal(body, &env); err != nil {
			WriteError(w, "INVALID_REQUEST",
				"body must be {\"order_ids\":[...], \"client_order_ids\":[...]}",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		ids := make([]int64, 0, len(env.OrderIDs))
		for _, v := range env.OrderIDs {
			var id int64
			var sid string
			if json.Unmarshal(v, &id) != nil {
				if json.Unmarshal(v, &sid) != nil {
					WriteError(w, "INVALID_REQUEST",
						"order_ids entries must be positive integers",
						gateway.RequestIDFrom(r.Context()), nil)
					return
				}
				parsed, perr := strconv.ParseInt(sid, 10, 64)
				if perr != nil {
					WriteError(w, "INVALID_REQUEST",
						"order_ids entries must be positive integers",
						gateway.RequestIDFrom(r.Context()), nil)
					return
				}
				id = parsed
			}
			if id <= 0 {
				WriteError(w, "INVALID_REQUEST",
					"order_ids entries must be positive integers",
					gateway.RequestIDFrom(r.Context()), nil)
				return
			}
			ids = append(ids, id)
		}
		results, err := d.SVC.BatchCancel(r.Context(), acct, ids,
			env.ClientOrderIDs,
			actorOf(c), gateway.RequestIDFrom(r.Context()),
			middleware.ClientIP(r, d.TrustProxy))
		if err != nil {
			writeServiceErrDetails(w, r, err,
				map[string]any{"results": results})
			return
		}
		WriteJSON(w, http.StatusOK, map[string]any{"results": results})
	}
}

// ---------------------------------------------------------------------------
// POST /api/v1/orders/{id}/cancel-replace + PUT .../amend/keep-priority
// + GET .../amendments (Task 5.3.37)
// ---------------------------------------------------------------------------

func OrderCancelReplace(d *OrderDeps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, ok := orderPathID(w, r)
		if !ok {
			return
		}
		body, err := readBody(r)
		if err != nil {
			WriteError(w, "INVALID_REQUEST", "request body unreadable",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		c, acct, ok := d.orderAuth(w, r, body, gateway.ScopeTrade)
		if !ok {
			return
		}
		req, err := orders.ParseCancelReplace(body)
		if err != nil {
			WriteError(w, "INVALID_REQUEST", err.Error(),
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		o, err := d.SVC.CancelReplace(r.Context(), acct, id, req,
			actorOf(c), gateway.RequestIDFrom(r.Context()),
			middleware.ClientIP(r, d.TrustProxy))
		if err != nil {
			// §24 #281: ALLOW_FAILURE reports distinct leg outcomes; the
			// amend is atomic so a failed replace rolls back fully.
			if req.Mode == "ALLOW_FAILURE" {
				var ce *excerrors.Error
				code := "INTERNAL_ERROR"
				if stderrors.As(err, &ce) {
					code = ce.Code
				}
				WriteError(w, "CANCEL_REPLACE_PARTIAL_FAILURE",
					"cancel-replace failed atomically: "+err.Error(),
					gateway.RequestIDFrom(r.Context()), map[string]any{
						"order_id":       id,
						"mode":           req.Mode,
						"cancel_outcome": "ROLLED_BACK",
						"new_outcome":    "NOT_APPLIED",
						"cause":          code,
					})
				return
			}
			writeServiceErr(w, r, err)
			return
		}
		WriteJSON(w, http.StatusOK, map[string]any{
			"order_id":       o.ID,
			"mode":           req.Mode,
			"cancel_outcome": "SUPERSEDED", // single atomic replace — no split legs
			"new_outcome":    "APPLIED",
			"order_seq":      o.OrderSeq,
			"order":          d.orderView(o),
		})
	}
}

func OrderKeepPriority(d *OrderDeps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, ok := orderPathID(w, r)
		if !ok {
			return
		}
		body, err := readBody(r)
		if err != nil {
			WriteError(w, "INVALID_REQUEST", "request body unreadable",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		c, acct, ok := d.orderAuth(w, r, body, gateway.ScopeTrade)
		if !ok {
			return
		}
		req, err := orders.ParseKeepPriority(body)
		if err != nil {
			WriteError(w, "INVALID_REQUEST", err.Error(),
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		o, err := d.SVC.AmendKeepPriority(r.Context(), acct, id, req,
			actorOf(c), gateway.RequestIDFrom(r.Context()),
			middleware.ClientIP(r, d.TrustProxy))
		if err != nil {
			writeServiceErr(w, r, err)
			return
		}
		WriteJSON(w, http.StatusOK, d.orderView(o))
	}
}

// OrderAmendments is the client-visible modification history for one
// owned order (Task 5.3.37 item 7).
func OrderAmendments(d *OrderDeps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, ok := orderPathID(w, r)
		if !ok {
			return
		}
		_, acct, ok := d.orderAuth(w, r, nil, gateway.ScopeRead)
		if !ok {
			return
		}
		rows, err := d.SVC.Amendments(r.Context(), acct, id)
		if err != nil {
			writeServiceErr(w, r, err)
			return
		}
		if rows == nil {
			rows = []orders.AuditEntry{}
		}
		WriteJSON(w, http.StatusOK, map[string]any{
			"order_id": id, "amendments": rows,
		})
	}
}

// ---------------------------------------------------------------------------
// POST /api/v1/orders/test — dry-run preview (Task 5.3.39)
// ---------------------------------------------------------------------------

// OrderTest runs the full validation/filter stack with ZERO side
// effects — no order row, no reservation, no dispatch, no audit write.
func OrderTest(d *OrderDeps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		body, err := readBody(r)
		if err != nil {
			WriteError(w, "INVALID_REQUEST", "request body unreadable",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		_, acct, ok := d.orderAuth(w, r, body, gateway.ScopeTrade)
		if !ok {
			return
		}
		req, err := orders.ParseSubmit(body)
		if err != nil {
			WriteError(w, "INVALID_REQUEST", err.Error(),
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		p, err := d.SVC.DryRun(r.Context(), acct, req)
		if err != nil {
			writeServiceErr(w, r, err)
			return
		}
		WriteJSON(w, http.StatusOK, p)
	}
}

// ---------------------------------------------------------------------------
// GET /api/v1/admin/orders/{id}/audit — Compliance Officer+ (Task 5.3.22)
// ---------------------------------------------------------------------------

var orderAuditRoles = map[string]bool{
	gateway.RoleSuperAdmin:        true,
	gateway.RoleComplianceOfficer: true,
	gateway.RoleRiskManager:       true,
}

// AdminOrderAudit serves the full order_audit history. Role is resolved
// through the Phase-07 RBAC seam; a nil resolver fails closed — audit
// data is never served without a role check.
func AdminOrderAudit(d *OrderDeps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, ok := orderPathID(w, r)
		if !ok {
			return
		}
		c := auth.ClaimsFrom(r.Context())
		if c == nil {
			WriteError(w, "UNAUTHORIZED", "authentication required",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		// The resolver keys on the admin USER id (subject), not the
		// selected trading account — admins may have no account bound.
		adminID, perr := strconv.ParseInt(c.Subject, 10, 64)
		if perr != nil || adminID <= 0 {
			WriteError(w, "UNAUTHORIZED", "admin identity unresolvable",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		if d.RoleResolver == nil {
			WriteError(w, "UNAUTHORIZED_ROLE",
				"role resolver not configured", gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		role, err := d.RoleResolver(r.Context(), adminID)
		if err != nil {
			writeServiceErr(w, r, err)
			return
		}
		if !orderAuditRoles[role] {
			WriteError(w, "UNAUTHORIZED_ROLE",
				"order audit requires Compliance Officer or higher",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		rows, err := d.SVC.AuditTrail(r.Context(), id)
		if err != nil {
			writeServiceErr(w, r, err)
			return
		}
		if rows == nil {
			rows = []orders.AuditEntry{}
		}
		WriteJSON(w, http.StatusOK, map[string]any{
			"order_id": id, "audit": rows,
		})
	}
}

// ---------------------------------------------------------------------------
// POST /api/v1/admin/orders/mass-cancel — Risk Manager (Task 5.3.24/25)
// ---------------------------------------------------------------------------

// AdminMassCancel is the cross-account mass-cancel surface. account_id
// is OPTIONAL here (0 = all accounts) — unlike the client endpoints
// where the account is always pinned to the authenticated identity.
func AdminMassCancel(d *OrderDeps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		body, err := readBody(r)
		if err != nil {
			WriteError(w, "INVALID_REQUEST", "request body unreadable",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		c := auth.ClaimsFrom(r.Context())
		if c == nil {
			WriteError(w, "UNAUTHORIZED", "authentication required",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		// Resolver keys on the admin USER id (subject), not AccountID.
		adminID, perr := strconv.ParseInt(c.Subject, 10, 64)
		if perr != nil || adminID <= 0 {
			WriteError(w, "UNAUTHORIZED", "admin identity unresolvable",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		if d.RoleResolver == nil {
			WriteError(w, "UNAUTHORIZED_ROLE",
				"role resolver not configured", gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		role, err := d.RoleResolver(r.Context(), adminID)
		if err != nil {
			writeServiceErr(w, r, err)
			return
		}
		if role != gateway.RoleRiskManager && role != gateway.RoleSuperAdmin {
			WriteError(w, "UNAUTHORIZED_ROLE",
				"admin mass cancel requires Risk Manager",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		obj, err := decodeJSONObj(body)
		if err != nil {
			WriteError(w, "INVALID_REQUEST", err.Error(),
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		scope := orders.MassCancelScope{Reason: "admin"}
		if v, ok := obj["account_id"]; ok && string(v) != "null" {
			var raw any
			if err := json.Unmarshal(v, &raw); err != nil {
				WriteError(w, "INVALID_REQUEST", "account_id malformed",
					gateway.RequestIDFrom(r.Context()), nil)
				return
			}
			switch n := raw.(type) {
			case float64:
				scope.AccountID = int64(n)
			case string:
				parsed, perr := strconv.ParseInt(n, 10, 64)
				if perr != nil {
					WriteError(w, "INVALID_REQUEST", "account_id malformed",
						gateway.RequestIDFrom(r.Context()), nil)
					return
				}
				scope.AccountID = parsed
			}
		}
		if s, ok := obj["symbol"]; ok {
			var sym string
			if err := json.Unmarshal(s, &sym); err == nil && sym != "" {
				inst, ierr := d.SVC.InstrumentBySymbol(r.Context(), sym)
				if ierr != nil {
					writeServiceErr(w, r, ierr)
					return
				}
				if inst == nil {
					WriteError(w, "INVALID_REQUEST", "unknown symbol "+strconv.Quote(sym),
						gateway.RequestIDFrom(r.Context()), nil)
					return
				}
				scope.InstrumentID = inst.ID
			}
		}
		if s, ok := obj["side"]; ok {
			_ = json.Unmarshal(s, &scope.Side)
		}
		if s, ok := obj["order_type"]; ok {
			_ = json.Unmarshal(s, &scope.OrderType)
		}
		res, err := d.SVC.AdminMassCancel(r.Context(), scope,
			"admin:"+actorOf(c), gateway.RequestIDFrom(r.Context()),
			middleware.ClientIP(r, d.TrustProxy))
		if err != nil {
			writeServiceErr(w, r, err)
			return
		}
		WriteJSON(w, http.StatusOK, map[string]any{
			"cancelled":  res.Cancelled,
			"per_symbol": res.PerSymbol,
		})
	}
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

// decodeJSONObj unmarshals a JSON object body for the admin surface.
func decodeJSONObj(body []byte) (map[string]json.RawMessage, error) {
	obj := map[string]json.RawMessage{}
	if len(body) == 0 {
		return obj, nil
	}
	if err := json.Unmarshal(body, &obj); err != nil {
		return nil, err
	}
	return obj, nil
}

// writeServiceErrDetails is writeServiceErr with an envelope details
// payload — the batch result array rides inside details.results.
func writeServiceErrDetails(w http.ResponseWriter, r *http.Request,
	err error, details map[string]any) {
	var e *excerrors.Error
	if stderrors.As(err, &e) {
		WriteError(w, e.Code, e.Message, gateway.RequestIDFrom(r.Context()), details)
		return
	}
	WriteError(w, "INTERNAL_ERROR", "internal error",
		gateway.RequestIDFrom(r.Context()), details)
}
