// Task 16.3.19 — grid bot REST handlers.
//
//	POST   /api/v1/bots/grid        create + arm a grid bot
//	GET    /api/v1/bots/grid        list account bots (live PnL + counts)
//	GET    /api/v1/bots/grid/{id}   detail + child orders
//	DELETE /api/v1/bots/grid/{id}   stop + cancel all children
//
// Auth mirrors the order surface: Bearer claims or X-API-KEY signature
// via the shared orderAuth seam; the account is resolved through the
// order store so every downstream child order carries a real accounts row.
package api

import (
	"encoding/json"
	"net/http"

	"exchange/internal/bots"
	"exchange/internal/gateway"
)

// GridBotDeps bundles the engine with the order-auth seam.
type GridBotDeps struct {
	Engine *bots.Engine
	Orders *OrderDeps
}

// GridBotCreate serves POST /api/v1/bots/grid.
func GridBotCreate(d *GridBotDeps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		body, err := readBody(r)
		if err != nil {
			WriteError(w, "INVALID_REQUEST", "request body too large",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		_, acct, ok := d.Orders.orderAuth(w, r, body, "trade")
		if !ok {
			return
		}
		var req bots.CreateRequest
		if err := json.Unmarshal(body, &req); err != nil {
			WriteError(w, "INVALID_REQUEST", "malformed JSON body",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		det, err := d.Engine.Create(r.Context(), acct.ID, req)
		if err != nil {
			writeSvcErr(w, r, err)
			return
		}
		WriteJSON(w, http.StatusCreated, det)
	}
}

// GridBotList serves GET /api/v1/bots/grid — account-owned bots with
// live PnL + filled-level accounting.
func GridBotList(d *GridBotDeps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		_, acct, ok := d.Orders.orderAuth(w, r, nil, "read")
		if !ok {
			return
		}
		list, err := d.Engine.List(r.Context(), acct.ID)
		if err != nil {
			writeSvcErr(w, r, err)
			return
		}
		WriteJSON(w, http.StatusOK, map[string]any{"bots": list})
	}
}

// GridBotGet serves GET /api/v1/bots/grid/{id}.
func GridBotGet(d *GridBotDeps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		_, acct, ok := d.Orders.orderAuth(w, r, nil, "read")
		if !ok {
			return
		}
		id, err := lpPathID(r)
		if err != nil {
			writeSvcErr(w, r, err)
			return
		}
		det, err := d.Engine.Detail(r.Context(), acct.ID, id)
		if err != nil {
			writeSvcErr(w, r, err)
			return
		}
		WriteJSON(w, http.StatusOK, det)
	}
}

// GridBotStop serves DELETE /api/v1/bots/grid/{id} — stops the bot and
// cancels every live child through the order pipeline.
func GridBotStop(d *GridBotDeps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		_, acct, ok := d.Orders.orderAuth(w, r, nil, "trade")
		if !ok {
			return
		}
		id, err := lpPathID(r)
		if err != nil {
			writeSvcErr(w, r, err)
			return
		}
		det, err := d.Engine.Stop(r.Context(), acct, id)
		if err != nil {
			writeSvcErr(w, r, err)
			return
		}
		WriteJSON(w, http.StatusOK, det)
	}
}
