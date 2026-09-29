// Phase-14 Task 14.3.1 — POST /api/v1/orders/oco (spec §6.2/§6.5,
// §24 #47): submit an OCO pair — two linked legs on one instrument;
// whichever reaches terminal FILLED first cancels the sibling atomically
// in the matching engine (journaled reason-7 ORDER_CANCEL). The losing
// side of the sibling race surfaces OCO_SIBLING_CANCEL_RACE (§23, 409).
package api

import (
	"net/http"

	"exchange/internal/gateway"
	"exchange/internal/orders"
)

// OrderSubmitOCO serves POST /api/v1/orders/oco. The handler is thin —
// auth, decode, service; pair semantics live in orders.SubmitOCO.
func OrderSubmitOCO(d *OrderDeps) http.HandlerFunc {
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
		req, err := orders.ParseSubmitOco(body)
		if err != nil {
			WriteError(w, "INVALID_REQUEST", err.Error(),
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		for _, leg := range req.Legs {
			leg.SessionID = c.SessionID // server-side session attribution
		}
		ack, err := d.SVC.SubmitOCO(r.Context(), acct, req)
		if err != nil {
			writeServiceErr(w, r, err)
			return
		}
		if ack.Legs[0] != nil && ack.Legs[0].Replay {
			w.Header().Set("Idempotency-Replayed", "true")
		}
		WriteJSON(w, http.StatusAccepted, ack)
	}
}
