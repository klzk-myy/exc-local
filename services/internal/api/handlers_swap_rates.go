// Phase-23 Task 23.3.9 — daily swap-rate history API (spec §17.15,
// §24 #358):
//
//	GET /api/v1/history/swap-rates?symbol=&from=&to=
//
// Read model over the Task-3.3.11 published sheet + accrual journal
// (marketdata.SwapRateHistorySource / PgSwapRateHistory): long/short
// interbank points per effective date, the markup split the journal
// actually applied, the rollover day-count, and an explicit `triple`
// flag for Wednesday rolls. Same Task-23.3.8 guards as tick history:
// 10s query timeout → HISTORICAL_QUERY_TIMEOUT, 60s Redis cache for
// closed intervals, free tier → last-30-days + ≥15-minute-old (on
// ingested_at), premium/staff → real-time. Fail-closed: unknown symbol
// → NOT_FOUND, store outage → SERVICE_DEGRADED.
package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"time"

	"exchange/internal/gateway"
	"exchange/internal/marketdata"
)

// SwapRateHistoryDeps bundles the seams — same shape as
// BlockTapeHistoryDeps; the hist() adapter feeds the shared Phase-23
// helpers without widening HistoryDeps.
type SwapRateHistoryDeps struct {
	History     marketdata.SwapRateHistorySource
	Instruments InstrumentResolver
	// Tiers resolves the caller's access tier; nil → free (fail-closed).
	Tiers marketdata.HistoryTierResolver
	// Guard carries the 10s-timeout/60s-cache parameters.
	Guard marketdata.HistoryQueryGuard
	// Cache is the best-effort response store; nil disables caching.
	Cache marketdata.HistoryKV
}

func (d *SwapRateHistoryDeps) hist() *HistoryDeps {
	if d == nil {
		return &HistoryDeps{}
	}
	return &HistoryDeps{Tiers: d.Tiers, Guard: d.Guard, Cache: d.Cache}
}

// historySwapRatesSpec is the pagination contract — declared locally
// like historyKlinesSpec; daily rows are small (≤366/instrument/year).
var historySwapRatesSpec = &ListSpec{
	Path: "/api/v1/history/swap-rates", Default: 100, Max: 500,
	Sortable:   []string{"effective_date"},
	Filterable: []string{"symbol", "from", "to"},
}

// swapRateDayDoc is the wire projection of one effective-dated sheet
// row reconciled against the accrual journal (§24 #358).
type swapRateDayDoc struct {
	InstrumentID   int64  `json:"instrument_id"`
	Symbol         string `json:"symbol"`
	EffectiveDate  string `json:"effective_date"` // YYYY-MM-DD, NY roll civil date
	LongPoints     string `json:"long_points"`
	ShortPoints    string `json:"short_points"`
	LongMarkupBps  string `json:"long_markup_bps"`
	ShortMarkupBps string `json:"short_markup_bps"`
	DaysApplied    int    `json:"days_applied"`  // journal rollover day-count (0 = no accruals)
	Triple         bool   `json:"triple"`        // Wednesday 3-day financing roll
	AccrualCount   int64  `json:"accrual_count"` // journal rows reconciled into the day
	Source         string `json:"source"`
	IngestedAtMs   int64  `json:"ingested_at_ms"`
	IngestedAt     string `json:"ingested_at"`
}

func swapRateDayDocOf(d marketdata.SwapRateDay) swapRateDayDoc {
	return swapRateDayDoc{
		InstrumentID:   d.InstrumentID,
		Symbol:         d.Symbol,
		EffectiveDate:  d.EffectiveDate.UTC().Format("2006-01-02"),
		LongPoints:     d.LongPoints.StringFixed(8),
		ShortPoints:    d.ShortPoints.StringFixed(8),
		LongMarkupBps:  d.LongMarkupBps.String(),
		ShortMarkupBps: d.ShortMarkupBps.String(),
		DaysApplied:    d.DaysApplied,
		Triple:         d.Triple,
		AccrualCount:   d.AccrualCount,
		Source:         d.Source,
		IngestedAtMs:   d.IngestedAt.UTC().UnixMilli(),
		IngestedAt:     d.IngestedAt.UTC().Format(time.RFC3339),
	}
}

// HistorySwapRates serves effective-dated swap history newest-first on
// (effective_date, instrument_id) with an opaque §8.8 keyset cursor.
// JSON default; Accept: text/csv renders the CSV leg.
func HistorySwapRates(d *SwapRateHistoryDeps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if d == nil || d.History == nil {
			WriteError(w, "SERVICE_DEGRADED",
				"swap-rate history store not configured",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		p, err := ParseListParams(r, historySwapRatesSpec)
		if err != nil {
			WriteError(w, "INVALID_REQUEST", err.Error(),
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		q := r.URL.Query()
		symbol := q.Get("symbol")
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
		if symbol != "" && !resolveInstrument(w, r, d.Instruments, symbol) {
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
			format = marketdata.HistoryFormatJSON
		}

		render := func(days []marketdata.SwapRateDay) []byte {
			if format == marketdata.HistoryFormatCSV {
				var buf bytes.Buffer
				_ = marketdata.WriteSwapRateDaysCSV(&buf, days)
				return buf.Bytes()
			}
			docs := make([]swapRateDayDoc, 0, len(days))
			for _, day := range days {
				docs = append(docs, swapRateDayDocOf(day))
			}
			env := historyEnvelope{
				ListEnvelope: NewListEnvelope(docs, p,
					PageCursors(days,
						func(day marketdata.SwapRateDay) (time.Time, int64) {
							return day.EffectiveDate, day.InstrumentID
						}), int64(len(days))),
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

		sq := marketdata.SwapRateHistoryQuery{
			Symbol: symbol, From: win.From, To: win.To, Limit: p.Limit,
		}
		// Free tier: only sheets published ≥15min ago — the delay
		// horizon binds ingested_at (the publication instant), not the
		// effective date.
		if delay := marketdata.HistoryDelayFor(access); delay > 0 {
			sq.PublishedBefore = d.Guard.Clock().Add(-delay)
		}
		if p.Decoded != nil {
			sq.After = &marketdata.SwapRateHistoryCursor{
				Date:         p.Decoded.CreatedAt,
				InstrumentID: p.Decoded.ID,
			}
		}
		qctx, cancel := d.Guard.QueryContext(r.Context())
		days, qerr := d.History.Query(qctx, sq)
		cancel()
		if qerr != nil {
			if historyTimedOut(qctx, qerr) {
				WriteError(w, "HISTORICAL_QUERY_TIMEOUT",
					"swap-rate history query exceeded the query timeout",
					gateway.RequestIDFrom(r.Context()), map[string]any{
						"hint": "narrow the from/to range",
					})
				return
			}
			WriteError(w, "SERVICE_DEGRADED",
				"swap-rate history store unavailable",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		body := render(days)
		if key != "" {
			d.Guard.CachePut(r.Context(), d.Cache, key, body)
		}
		writeHistoryBody(w, format, body, false)
	}
}
