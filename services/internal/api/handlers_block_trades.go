// Phase-23 Task 23.3.7 — historical block-trade API (spec §10.8/§16.1,
// §24 #291; §28.1 route matrix row):
//
//	GET /api/v1/history/block-trades/{symbol}?from=&to=&limit=&cursor=
//
// Read model over the ClickHouse `block_trades_tape` table populated by
// the Phase-06 Task 6.3.20 producer's published-print + correction
// events (marketdata.BlockTapeStore). Every Task-23.3.8 guard applies:
// 10s query timeout → HISTORICAL_QUERY_TIMEOUT, 60s closed-interval
// Redis cache, free tier → last-30-days + ≥15-minute-old (on pub_ts —
// the publication axis — in addition to the window's exec_ts clamp);
// premium/staff → real-time. Correction/bust lineage rides the row
// annotations (corrected_by / bust / supersedes). Participant identity
// is structurally absent — the read model carries none.
package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"time"

	"exchange/internal/gateway"
	"exchange/internal/marketdata"
)

// BlockTapeHistoryDeps bundles the seams the block-tape history
// handler needs — same shape as HistoryDeps; the hist() adapter feeds
// the shared Phase-23 helpers (resolveHistoryAccess,
// historyCacheAttempt) a compatible view without widening
// HistoryDeps itself.
type BlockTapeHistoryDeps struct {
	Tape        marketdata.BlockTapeReader
	Instruments InstrumentResolver
	// Tiers resolves the caller's access tier; nil → free (fail-closed).
	Tiers marketdata.HistoryTierResolver
	// Guard carries the 10s-timeout/60s-cache parameters.
	Guard marketdata.HistoryQueryGuard
	// Cache is the best-effort response store; nil disables caching.
	Cache marketdata.HistoryKV
}

func (d *BlockTapeHistoryDeps) hist() *HistoryDeps {
	if d == nil {
		return &HistoryDeps{}
	}
	return &HistoryDeps{Tiers: d.Tiers, Guard: d.Guard, Cache: d.Cache}
}

// historyBlockTradesSpec is the pagination contract for the block-tape
// route — same shape as historyKlinesSpec; declared locally because the
// published ListSpecs table is owned by another task's file.
var historyBlockTradesSpec = &ListSpec{
	Path: "/api/v1/history/block-trades/{symbol}", Default: 500, Max: 1500,
	Sortable:   []string{"pub_ts"},
	Filterable: []string{"from", "to"},
}

// blockTapeDoc is the wire projection of one tape entry. The lineage
// trio is explicit: `bust`/`corrected_by` annotate a superseded PRINT,
// `supersedes` links a CORRECTION/BUST row back to the print it amends.
// No side, no account ids, no order ids — the tape schema carries none.
type blockTapeDoc struct {
	EntryID           uint64   `json:"entry_id"`
	Kind              string   `json:"kind"` // PRINT | CORRECTION | BUST
	BlockTradeID      uint64   `json:"block_trade_id"`
	Symbol            string   `json:"symbol"`
	Price             string   `json:"price,omitempty"`
	Quantity          string   `json:"quantity,omitempty"`
	NotionalUSD       string   `json:"notional_usd,omitempty"`
	ExecTsMs          int64    `json:"exec_ts_ms"`
	ExecTs            string   `json:"exec_ts"`
	PubTsMs           int64    `json:"pub_ts_ms"`
	PubTs             string   `json:"pub_ts"`
	DelayMs           int64    `json:"delay_ms"`
	VenueFlags        []string `json:"venue_flags"`
	Bust              bool     `json:"bust"`                   // print superseded by a BUST / row IS a bust
	CorrectedBy       uint64   `json:"corrected_by,omitempty"` // print: entry_id of superseding correction
	Supersedes        uint64   `json:"supersedes,omitempty"`   // correction: block_trade_id superseded
	OriginalTradeID   uint64   `json:"original_trade_id,omitempty"`
	CorrectedPrice    string   `json:"corrected_price,omitempty"`
	CorrectedQuantity string   `json:"corrected_quantity,omitempty"`
}

func blockTapeDocOf(e *marketdata.BlockTapeEntry) blockTapeDoc {
	doc := blockTapeDoc{
		EntryID:      e.EntryID,
		Kind:         string(e.Kind),
		BlockTradeID: e.BlockTradeID,
		Symbol:       e.Symbol,
		ExecTsMs:     e.ExecTs.UTC().UnixMilli(),
		ExecTs:       e.ExecTs.UTC().Format(time.RFC3339),
		PubTsMs:      e.PubTs.UTC().UnixMilli(),
		PubTs:        e.PubTs.UTC().Format(time.RFC3339),
		DelayMs:      e.DelayMs,
		VenueFlags:   e.VenueFlags,
		Bust:         e.Bust || e.Kind == marketdata.BlockTapeBust,
	}
	if e.VenueFlags == nil {
		doc.VenueFlags = []string{}
	}
	if e.Kind == marketdata.BlockTapePrint {
		doc.Price = e.Price.StringFixed(8)
		doc.Quantity = e.Quantity.StringFixed(8)
		doc.NotionalUSD = e.NotionalUSD.StringFixed(8)
		doc.CorrectedBy = e.CorrectedBy
		return doc
	}
	doc.Supersedes = e.BlockTradeID
	doc.OriginalTradeID = e.OriginalTradeID
	if e.CorrectedPrice != nil {
		doc.CorrectedPrice = e.CorrectedPrice.StringFixed(8)
	}
	if e.CorrectedQuantity != nil {
		doc.CorrectedQuantity = e.CorrectedQuantity.StringFixed(8)
	}
	return doc
}

// HistoryBlockTrades serves the published anonymous block tape for one
// symbol, newest-first on (pub_ts, entry_id) with an opaque §8.8
// keyset cursor. JSON default; Accept: text/csv renders the CSV leg.
// Fail-closed: unknown symbol → NOT_FOUND; store outage →
// SERVICE_DEGRADED; deadline → HISTORICAL_QUERY_TIMEOUT (504).
func HistoryBlockTrades(d *BlockTapeHistoryDeps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if d == nil || d.Tape == nil {
			WriteError(w, "SERVICE_DEGRADED",
				"block-tape history store not configured",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		symbol := r.PathValue("symbol")
		if symbol == "" {
			WriteError(w, "INVALID_REQUEST", "symbol required",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		p, err := ParseListParams(r, historyBlockTradesSpec)
		if err != nil {
			WriteError(w, "INVALID_REQUEST", err.Error(),
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		q := r.URL.Query()
		from, err := parseTimeQuery(q.Get("from"))
		if err != nil {
			WriteError(w, "INVALID_REQUEST", "from must be RFC3339",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		to, err := parseTimeQuery(q.Get("to"))
		if err != nil {
			WriteError(w, "INVALID_REQUEST", "to must be RFC3339",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		if !resolveInstrument(w, r, d.Instruments, symbol) {
			return
		}

		h := d.hist()
		access, degraded := resolveHistoryAccess(h, r)
		var fromV, toV time.Time
		if from != nil {
			fromV = *from
		}
		if to != nil {
			toV = *to
		}
		win := marketdata.ResolveHistoryWindow(access, fromV, toV, d.Guard.Clock())
		format := marketdata.HistoryFormatFor(r.Header.Get("Accept"))
		if format == marketdata.HistoryFormatFIX {
			format = marketdata.HistoryFormatJSON // no drop-copy leg on the tape
		}

		render := func(entries []*marketdata.BlockTapeEntry) []byte {
			if format == marketdata.HistoryFormatCSV {
				var buf bytes.Buffer
				_ = marketdata.WriteBlockTapeCSV(&buf, entries)
				return buf.Bytes()
			}
			docs := make([]blockTapeDoc, 0, len(entries))
			for _, e := range entries {
				docs = append(docs, blockTapeDocOf(e))
			}
			env := historyEnvelope{
				ListEnvelope: NewListEnvelope(docs, p,
					PageCursors(entries,
						func(e *marketdata.BlockTapeEntry) (time.Time, int64) {
							return e.PubTs, int64(e.EntryID)
						}), int64(len(entries))),
				AccessTier: string(access),
				Delayed:    marketdata.HistoryDelayFor(access) > 0,
				Degraded:   degraded,
			}
			body, _ := json.Marshal(env)
			return body
		}

		if win.Empty() {
			writeHistoryBody(w, format, render(nil), false)
			return
		}

		key, cached := historyCacheAttempt(h, r, access, format, win.To)
		if cached != nil {
			writeHistoryBody(w, format, cached, true)
			return
		}

		bq := marketdata.BlockTapeQuery{
			Symbol: symbol, From: win.From, To: win.To, Limit: p.Limit,
		}
		// The 15-minute free delay binds pub_ts — the axis the venue
		// actually disclosed on. ResolveHistoryWindow already clamps
		// exec_ts; this bound covers corrections whose exec_ts ≈
		// publication time.
		if delay := marketdata.HistoryDelayFor(access); delay > 0 {
			bq.PublishedBefore = d.Guard.Clock().Add(-delay)
		}
		if p.Decoded != nil {
			bq.After = &marketdata.BlockTapeCursor{
				Ts:      p.Decoded.CreatedAt,
				EntryID: uint64(p.Decoded.ID),
			}
		}
		qctx, cancel := d.Guard.QueryContext(r.Context())
		entries, qerr := d.Tape.Query(qctx, bq)
		cancel()
		if qerr != nil {
			if historyTimedOut(qctx, qerr) {
				WriteError(w, "HISTORICAL_QUERY_TIMEOUT",
					"block-tape query exceeded the query timeout",
					gateway.RequestIDFrom(r.Context()), map[string]any{
						"hint": "narrow the from/to range or reduce limit",
					})
				return
			}
			WriteError(w, "SERVICE_DEGRADED",
				"block-tape history store unavailable",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		body := render(entries)
		if key != "" {
			d.Guard.CachePut(r.Context(), d.Cache, key, body)
		}
		writeHistoryBody(w, format, body, false)
	}
}
