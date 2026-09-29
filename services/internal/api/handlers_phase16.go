// Phase-16 composite-order REST surface.
//
// Tasks 16.3.14 (bracket/OTO submit), 16.3.20 (OPO/OPOCO submit +
// cancel), 16.3.24 (composite-list query endpoints), 16.3.23
// extensions live in handlers_algo.go. Handlers stay thin: claims +
// scope gates reuse the OrderDeps convention; bodies decode through
// the orders package parsers; coded errors map via writeServiceErr;
// list endpoints emit the Task 5.3.42 envelope.
package api

import (
	"net/http"
	"time"

	"exchange/internal/gateway"
	"exchange/internal/middleware"
	"exchange/internal/orders"
)

// ---------------------------------------------------------------------------
// POST /api/v1/orders/bracket — bracket/OTO submit (Task 16.3.14)
// ---------------------------------------------------------------------------

// OrderBracketSubmit validates all three legs, persists the parent +
// bracket group atomically and dispatches only the parent — children
// materialize on the first fill through the consumer's composite hook.
func OrderBracketSubmit(d *OrderDeps) http.HandlerFunc {
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
		req, err := orders.ParseSubmitBracket(body)
		if err != nil {
			WriteError(w, "INVALID_REQUEST", err.Error(),
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		req.Parent.SessionID = c.SessionID
		ack, err := d.SVC.SubmitBracket(r.Context(), acct, req)
		if err != nil {
			writeServiceErr(w, r, err)
			return
		}
		if ack.Parent != nil && ack.Parent.Replay {
			w.Header().Set("Idempotency-Replayed", "true")
		}
		WriteJSON(w, http.StatusAccepted, ack)
	}
}

// ---------------------------------------------------------------------------
// POST /api/v1/order-lists + DELETE /api/v1/order-lists/{id}
// (Task 16.3.20 — OPO/OPOCO list lifecycle)
// ---------------------------------------------------------------------------

// OrderListSubmit validates the working leg + pending-leg structure,
// persists list row + legs + working order in one transaction and
// dispatches only the working leg (pending legs activate on fill).
func OrderListSubmit(d *OrderDeps) http.HandlerFunc {
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
		req, err := orders.ParseSubmitOrderList(body)
		if err != nil {
			WriteError(w, "INVALID_REQUEST", err.Error(),
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		req.Working.SessionID = c.SessionID
		ack, err := d.SVC.SubmitOrderList(r.Context(), acct, req)
		if err != nil {
			writeServiceErr(w, r, err)
			return
		}
		if ack.Working != nil && ack.Working.Replay {
			w.Header().Set("Idempotency-Replayed", "true")
		}
		WriteJSON(w, http.StatusAccepted, ack)
	}
}

// OrderListCancel is DELETE /order-lists/{id}: the working leg cancels
// while open, live pending legs cancel, the list closes CANCELLED —
// idempotent on terminal lists.
func OrderListCancel(d *OrderDeps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, ok := orderPathID(w, r)
		if !ok {
			return
		}
		body, _ := readBody(r)
		c, acct, ok := d.orderAuth(w, r, body, gateway.ScopeTrade)
		if !ok {
			return
		}
		l, err := d.SVC.CancelOrderList(r.Context(), acct, id,
			actorOf(c), gateway.RequestIDFrom(r.Context()),
			middleware.ClientIP(r, d.TrustProxy))
		if err != nil {
			writeServiceErr(w, r, err)
			return
		}
		WriteJSON(w, http.StatusOK, orders.OrderListView(l))
	}
}

// ---------------------------------------------------------------------------
// GET /api/v1/order-lists (open) + /api/v1/order-lists/history (closed)
// + GET /api/v1/order-lists/{id} — Task 16.3.24 query endpoints
// ---------------------------------------------------------------------------

// orderListsPage serves both list endpoints; openOnly selects the open
// vs closed-state slice of order_lists.
func orderListsPage(d *OrderDeps, openOnly bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		_, acct, ok := d.orderAuth(w, r, nil, gateway.ScopeRead)
		if !ok {
			return
		}
		path := "/api/v1/order-lists"
		if !openOnly {
			path += "/history"
		}
		p, err := ParseListParams(r, ListSpecFor(path))
		if err != nil {
			WriteError(w, "INVALID_REQUEST", err.Error(),
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		var cursorAt time.Time
		var cursorID int64
		if p.Decoded != nil {
			cursorAt = p.Decoded.CreatedAt
			cursorID = p.Decoded.ID
		}
		rows, total, err := d.SVC.OrderLists(r.Context(), acct.ID, openOnly,
			cursorAt, cursorID, p.Limit)
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
			views = append(views, orders.OrderListView(&page[i]))
		}
		env := NewListEnvelope(views, p,
			PageCursors(page, func(l orders.OrderList) (time.Time, int64) {
				return l.CreatedAt, l.ID
			}), total)
		WriteJSON(w, http.StatusOK, env)
	}
}

// OrderListsOpen serves GET /order-lists — non-terminal lists only.
func OrderListsOpen(d *OrderDeps) http.HandlerFunc {
	return orderListsPage(d, true)
}

// OrderListsHistory serves GET /order-lists/history — terminal lists.
func OrderListsHistory(d *OrderDeps) http.HandlerFunc {
	return orderListsPage(d, false)
}

// OrderListGet serves GET /order-lists/{id} — list + leg detail,
// account-scoped (foreign ids map to ORDER_NOT_FOUND, never 403 leaks).
func OrderListGet(d *OrderDeps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, ok := orderPathID(w, r)
		if !ok {
			return
		}
		_, acct, ok := d.orderAuth(w, r, nil, gateway.ScopeRead)
		if !ok {
			return
		}
		l, legs, err := d.SVC.OrderListDetail(r.Context(), acct, id)
		if err != nil {
			writeServiceErr(w, r, err)
			return
		}
		v := orders.OrderListView(l)
		legViews := make([]map[string]any, 0, len(legs))
		for i := range legs {
			legViews = append(legViews, orders.OrderListLegView(&legs[i]))
		}
		v["legs"] = legViews
		WriteJSON(w, http.StatusOK, v)
	}
}
