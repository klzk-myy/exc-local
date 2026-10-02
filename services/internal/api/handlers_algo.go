// Phase-16 algo order REST surface.
//
// Tasks 16.3.1/16.3.2/16.3.6/16.3.7/16.3.8/16.3.12/16.3.18/16.3.21.
// Handlers stay thin — claims + scope gates reuse the OrderDeps
// convention (Bearer JWT or HMAC-signed API key), bodies decode through
// the algo package parsers, coded errors map via writeServiceErr.
package api

import (
	"net/http"
	"sort"
	"strings"
	"time"

	"exchange/internal/algo"
	"exchange/internal/bots"
	"exchange/internal/gateway"
	"exchange/internal/orders"
)

// AlgoDeps bundles the algo handlers' seams. Engine is the Phase-16
// framework service (nil → all algo routes fail closed SERVICE_DEGRADED);
// OrderDeps supplies the shared auth path. Bots is the Task 16.3.19
// grid engine — nil excludes GRID entries from the unified list and
// leaves them untouched by cancel-all (Task 16.3.23).
type AlgoDeps struct {
	OrderDeps
	Engine *algo.Engine
	Bots   *bots.Engine
}

// algoOK fails closed when the engine is unwired.
func (d *AlgoDeps) algoOK(w http.ResponseWriter, r *http.Request) bool {
	if d.Engine == nil {
		WriteError(w, "SERVICE_DEGRADED", "algo engine unwired",
			gateway.RequestIDFrom(r.Context()), nil)
		return false
	}
	return true
}

func algoParentView(p *algo.Parent, children []algo.Child) map[string]any {
	v := p.View()
	if children != nil {
		cv := make([]map[string]any, 0, len(children))
		for i := range children {
			cv = append(cv, children[i].View())
		}
		v["children"] = cv
	}
	return v
}

// ---------------------------------------------------------------------------
// POST /api/v1/orders/algo — generic submit (Task 16.3.8)
// POST /api/v1/orders/{twap,vwap,scaled,spread} — typed submits
// ---------------------------------------------------------------------------

// AlgoSubmit handles POST /orders/algo {"algo_type","symbol","side",
// "total_qty","params","start_at","client_order_id"}.
func AlgoSubmit(d *AlgoDeps) http.HandlerFunc {
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
		req, err := algo.ParseAlgoSubmit(body)
		if err != nil {
			WriteError(w, "INVALID_REQUEST", err.Error(),
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		req.SessionID = c.SessionID
		// TRAILING_STOP is an engine-side order type (Task 16.3.15), not a
		// slicing strategy — route it through the order pipeline so its
		// params reach the OrderNew wire fields. The same body decodes via
		// orders.ParseSubmit (algo_type/algo_params are SubmitRequest
		// fields; total_qty needs the manual fallback).
		if req.AlgoType == "TRAILING_STOP" {
			oreq, oerr := orders.ParseSubmit(body)
			if oerr != nil {
				WriteError(w, "INVALID_REQUEST", oerr.Error(),
					gateway.RequestIDFrom(r.Context()), nil)
				return
			}
			// The algo-surface body has no `type` key — a trailing stop is
			// a STOP order with the distance pair (the engine derives
			// TRAILING_STOP from StopMarket + trailing_offset_unit).
			oreq.OrderType = orders.TypeStop
			if oreq.Quantity == nil && !req.TotalQty.IsZero() {
				q := req.TotalQty
				oreq.Quantity = &q
			}
			oreq.SessionID = c.SessionID
			ack, serr := d.SVC.Submit(r.Context(), acct, oreq)
			if serr != nil {
				writeServiceErr(w, r, serr)
				return
			}
			WriteJSON(w, http.StatusAccepted, ack)
			return
		}
		if !d.algoOK(w, r) {
			return
		}
		p, err := d.Engine.Submit(r.Context(), acct.ID, req)
		if err != nil {
			writeServiceErr(w, r, err)
			return
		}
		_, children, _ := d.Engine.Get(r.Context(), acct.ID, p.ID)
		WriteJSON(w, http.StatusAccepted, algoParentView(p, children))
	}
}

// AlgoSubmitTyped builds the typed submitters — the flat body carries
// symbol/side/total_qty plus the strategy params inline.
func AlgoSubmitTyped(d *AlgoDeps, algoType string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !d.algoOK(w, r) {
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
		req, err := algo.ParseTypedSubmit(algoType, body)
		if err != nil {
			WriteError(w, "INVALID_REQUEST", err.Error(),
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		req.SessionID = c.SessionID
		p, err := d.Engine.Submit(r.Context(), acct.ID, req)
		if err != nil {
			writeServiceErr(w, r, err)
			return
		}
		_, children, _ := d.Engine.Get(r.Context(), acct.ID, p.ID)
		WriteJSON(w, http.StatusAccepted, algoParentView(p, children))
	}
}

// ---------------------------------------------------------------------------
// pause / resume / cancel
// ---------------------------------------------------------------------------

func AlgoPause(d *AlgoDeps) http.HandlerFunc {
	return algoTransition(d, func(r *http.Request, accountID, id int64) (*algo.Parent, error) {
		return d.Engine.Pause(r.Context(), accountID, id)
	})
}

func AlgoResume(d *AlgoDeps) http.HandlerFunc {
	return algoTransition(d, func(r *http.Request, accountID, id int64) (*algo.Parent, error) {
		return d.Engine.Resume(r.Context(), accountID, id)
	})
}

// AlgoCancel — DELETE /orders/algo/{id}: cancel parent + children.
func AlgoCancel(d *AlgoDeps) http.HandlerFunc {
	return algoTransition(d, func(r *http.Request, accountID, id int64) (*algo.Parent, error) {
		return d.Engine.Cancel(r.Context(), accountID, id, "client cancel")
	})
}

func algoTransition(d *AlgoDeps,
	op func(*http.Request, int64, int64) (*algo.Parent, error)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !d.algoOK(w, r) {
			return
		}
		id, ok := orderPathID(w, r)
		if !ok {
			return
		}
		body, _ := readBody(r)
		_, acct, ok := d.orderAuth(w, r, body, gateway.ScopeTrade)
		if !ok {
			return
		}
		p, err := op(r, acct.ID, id)
		if err != nil {
			writeServiceErr(w, r, err)
			return
		}
		_, children, _ := d.Engine.Get(r.Context(), acct.ID, p.ID)
		WriteJSON(w, http.StatusOK, algoParentView(p, children))
	}
}

// ---------------------------------------------------------------------------
// GET /api/v1/algo-orders — unified list + child progress (Task 16.3.23)
// DELETE /api/v1/algo-orders — cancel all running algos (zero orphans)
// ---------------------------------------------------------------------------

// unifiedAlgoEntry is one merged row of the algo + grid status surface.
type unifiedAlgoEntry struct {
	CreatedAt time.Time
	ID        int64
	Kind      string // algo_type | "GRID"
	View      map[string]any
}

// AlgoList is the Task 16.3.23 unified status surface: TWAP/VWAP/VP/
// SCALE/SPREAD parents from the algo engine plus grid bots when the
// bots engine is wired. Filters: type, symbol, status; the Task 5.3.42
// envelope paginates over (created_at,id) DESC.
func AlgoList(d *AlgoDeps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !d.algoOK(w, r) {
			return
		}
		_, acct, ok := d.orderAuth(w, r, nil, gateway.ScopeRead)
		if !ok {
			return
		}
		p, err := ParseListParams(r, ListSpecFor("/api/v1/algo-orders"))
		if err != nil {
			WriteError(w, "INVALID_REQUEST", err.Error(),
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		q := r.URL.Query()
		typ := strings.ToUpper(strings.TrimSpace(q.Get("type")))
		symbol := strings.TrimSpace(q.Get("symbol"))
		status := strings.ToUpper(strings.TrimSpace(q.Get("status")))

		var entries []unifiedAlgoEntry
		// Algo parents — the store applies the status filter; type,
		// symbol and the cursor keyset filter merge-side.
		if typ == "" || typ != "GRID" {
			parents, lerr := d.Engine.List(r.Context(), acct.ID, status, 1000)
			if lerr != nil {
				writeServiceErr(w, r, lerr)
				return
			}
			for i := range parents {
				par := &parents[i]
				if typ != "" && par.Type != typ {
					continue
				}
				if symbol != "" && par.Symbol != symbol {
					continue
				}
				if p.Decoded != nil &&
					!par.CreatedAt.Before(p.Decoded.CreatedAt) &&
					!(par.CreatedAt.Equal(p.Decoded.CreatedAt) && par.ID < p.Decoded.ID) {
					continue // keyset: keep only rows strictly after the cursor
				}
				_, children, _ := d.Engine.Get(r.Context(), acct.ID, par.ID)
				v := algoParentView(par, children)
				v["kind"] = "ALGO"
				entries = append(entries, unifiedAlgoEntry{
					CreatedAt: par.CreatedAt, ID: par.ID, Kind: par.Type, View: v})
			}
		}
		// Grid bots — status filter maps onto the bot lifecycle.
		if d.Bots != nil && (typ == "" || typ == "GRID") {
			bl, berr := d.Bots.List(r.Context(), acct.ID)
			if berr != nil {
				writeServiceErr(w, r, berr)
				return
			}
			for i := range bl {
				b := &bl[i]
				if status != "" && b.Status != status {
					continue
				}
				if symbol != "" && b.Symbol != symbol {
					continue
				}
				if p.Decoded != nil &&
					!b.CreatedAt.Before(p.Decoded.CreatedAt) &&
					!(b.CreatedAt.Equal(p.Decoded.CreatedAt) && b.BotID < p.Decoded.ID) {
					continue
				}
				det, _ := d.Bots.Detail(r.Context(), acct.ID, b.BotID)
				filled := 0
				if det != nil {
					for _, c := range det.Children {
						if c.Status == "FILLED" {
							filled++
						}
					}
				}
				entries = append(entries, unifiedAlgoEntry{
					CreatedAt: b.CreatedAt, ID: b.BotID, Kind: "GRID",
					View: map[string]any{
						"bot_id":        b.BotID,
						"account_id":    b.AccountID,
						"algo_type":     "GRID",
						"symbol":        b.Symbol,
						"status":        b.Status,
						"filled_levels": filled,
						"fills_count":   b.FillsCount,
						"realized_pnl":  b.RealizedPnL.String(),
						"created_at":    b.CreatedAt.UTC().Format(time.RFC3339Nano),
						"updated_at":    b.UpdatedAt.UTC().Format(time.RFC3339Nano),
						"kind":          "GRID",
					}})
			}
		}
		sort.Slice(entries, func(i, j int) bool {
			if entries[i].CreatedAt.Equal(entries[j].CreatedAt) {
				return entries[i].ID > entries[j].ID
			}
			return entries[i].CreatedAt.After(entries[j].CreatedAt)
		})
		total := int64(len(entries))
		page := entries
		if len(page) > p.Limit {
			page = page[:p.Limit]
		}
		views := make([]map[string]any, 0, len(page))
		for _, e := range page {
			views = append(views, e.View)
		}
		env := NewListEnvelope(views, p,
			PageCursors(page, func(e unifiedAlgoEntry) (time.Time, int64) {
				return e.CreatedAt, e.ID
			}), total)
		WriteJSON(w, http.StatusOK, env)
	}
}

// AlgoCancelAll — DELETE /algo-orders: every non-terminal parent of the
// account is cancelled (children terminated via the pipeline with the
// Task 16.3.22 race reconcile), plus every live (RUNNING or PAUSED —
// paused children still rest on the book) grid bot when the bots engine
// is wired — zero orphan slices per §24 #365. ?symbol=
// scopes the sweep to one instrument.
func AlgoCancelAll(d *AlgoDeps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !d.algoOK(w, r) {
			return
		}
		body, _ := readBody(r)
		_, acct, ok := d.orderAuth(w, r, body, gateway.ScopeTrade)
		if !ok {
			return
		}
		symbol := strings.TrimSpace(r.URL.Query().Get("symbol"))
		n, err := d.Engine.CancelAll(r.Context(), acct.ID, symbol)
		if err != nil {
			writeServiceErr(w, r, err)
			return
		}
		stopped := 0
		if d.Bots != nil {
			bl, berr := d.Bots.List(r.Context(), acct.ID)
			if berr != nil {
				writeServiceErr(w, r, berr)
				return
			}
			for i := range bl {
				if bl[i].Status != bots.StatusRunning &&
					bl[i].Status != bots.StatusPaused {
					continue
				}
				if symbol != "" && bl[i].Symbol != symbol {
					continue
				}
				if _, serr := d.Bots.Stop(r.Context(), acct, bl[i].BotID); serr != nil {
					continue // surfaced via the bot's own state; sweep continues
				}
				stopped++
			}
		}
		WriteJSON(w, http.StatusOK, map[string]any{
			"cancelled":    n,
			"bots_stopped": stopped,
		})
	}
}
