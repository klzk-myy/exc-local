// Phase-20 Tasks 20.3.4 / 20.3.5 — ClickHouse-backed analytics surface
// (spec §16):
//
//	GET /api/v1/analytics/pnl?from=&to=&format=json|csv|pdf
//	    — per-account P&L report (auth required; account from claims,
//	      ?account_id= must match). Rows aggregate per
//	      (account, symbol, day) — symbol is the instrument identity in
//	      the CH projection; money fields serialize as decimal strings
//	      per the §5.3 wire convention.
//	GET /api/v1/analytics/volume?from=&to=&symbol=&granularity=1h|1d&limit=
//	    — public per-symbol volume buckets (hourly stored buckets; "1d"
//	      is a toStartOfDay rollup of them).
//	GET /api/v1/analytics/stats?from=&to=
//	    — public venue statistics: per-symbol fill rate
//	      (orders_filled/orders_submitted counters fed into volume_stats
//	      via VolumeStatsStore.SyncOrdersCount) + trade count per
//	      symbol/client_category tier.
//
// Window contract: from/to accept RFC3339 or YYYY-MM-DD; the range is
// half-open [from, to), so `to=2026-09-21` includes the 20th. Defaults:
// to = now UTC, from = to − 30 days. Windows wider than 397 days are
// rejected — these endpoints are reports, not table dumps (the Task
// 23.3.4 history/export surface owns bulk extraction).
package api

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"exchange/internal/analytics"
	"exchange/internal/gateway"
	"exchange/pkg/decimal"
)

// PnLReporter is the ClickHouse P&L read seam (*analytics.PnLStore).
type PnLReporter interface {
	Report(ctx context.Context, accountID int64, from, to time.Time) ([]analytics.PnLRow, error)
}

// StatsReporter is the ClickHouse volume/stats read seam
// (*analytics.VolumeStatsStore).
type StatsReporter interface {
	Volume(ctx context.Context, from, to time.Time, symbol, granularity string, limit int) ([]analytics.VolumeRow, error)
	FillRates(ctx context.Context, from, to time.Time) ([]analytics.FillRate, error)
	TradesPerTier(ctx context.Context, from, to time.Time) ([]analytics.TierTradeRow, error)
}

// AnalyticsDeps bundles the reporting seams. A nil PnL/Stats fails closed
// SERVICE_DEGRADED on the corresponding endpoint — the route registry
// stays complete even when this binary hosts no ClickHouse client.
type AnalyticsDeps struct {
	PnL   PnLReporter
	Stats StatsReporter
	Now   func() time.Time // injectable clock; UTC when nil
}

func (d *AnalyticsDeps) now() time.Time {
	if d != nil && d.Now != nil {
		return d.Now().UTC()
	}
	return time.Now().UTC()
}

const (
	// analyticsMaxWindowDays caps the report window — reports, not dumps.
	analyticsMaxWindowDays = 397
	// analyticsDefaultWindowDays applies when the caller omits from.
	analyticsDefaultWindowDays = 30
	// analyticsVolumeMaxLimit bounds bucket rows on /analytics/volume.
	analyticsVolumeMaxLimit = 20000
)

// parseAnalyticsTime accepts RFC3339 or a bare YYYY-MM-DD date (UTC
// midnight). Empty → nil.
func parseAnalyticsTime(raw string) (*time.Time, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}
	if t, err := time.Parse(time.RFC3339, raw); err == nil {
		u := t.UTC()
		return &u, nil
	}
	if t, err := time.Parse("2006-01-02", raw); err == nil {
		return &t, nil
	}
	return nil, fmt.Errorf("must be RFC3339 or YYYY-MM-DD")
}

// analyticsWindow resolves the [from, to) half-open range with defaults
// and bounds. ok=false means the handler already emitted the 400.
func analyticsWindow(w http.ResponseWriter, r *http.Request, now time.Time) (time.Time, time.Time, bool) {
	q := r.URL.Query()
	from, err := parseAnalyticsTime(q.Get("from"))
	if err != nil {
		WriteError(w, "INVALID_REQUEST", "from: "+err.Error(),
			gateway.RequestIDFrom(r.Context()), nil)
		return time.Time{}, time.Time{}, false
	}
	to, err := parseAnalyticsTime(q.Get("to"))
	if err != nil {
		WriteError(w, "INVALID_REQUEST", "to: "+err.Error(),
			gateway.RequestIDFrom(r.Context()), nil)
		return time.Time{}, time.Time{}, false
	}
	toV := now
	if to != nil {
		toV = to.UTC()
	}
	fromV := toV.AddDate(0, 0, -analyticsDefaultWindowDays)
	if from != nil {
		fromV = *from
	}
	if !fromV.Before(toV) {
		WriteError(w, "INVALID_REQUEST", "from must be before to",
			gateway.RequestIDFrom(r.Context()), nil)
		return time.Time{}, time.Time{}, false
	}
	if toV.Sub(fromV) > time.Duration(analyticsMaxWindowDays)*24*time.Hour {
		WriteError(w, "INVALID_REQUEST",
			fmt.Sprintf("window exceeds %d days", analyticsMaxWindowDays),
			gateway.RequestIDFrom(r.Context()), nil)
		return time.Time{}, time.Time{}, false
	}
	return fromV, toV, true
}

// analyticsFormat resolves the export format: explicit ?format= wins;
// otherwise Accept: text/csv|application/pdf negotiates; default JSON.
func analyticsFormat(r *http.Request) string {
	if f := r.URL.Query().Get("format"); f != "" {
		return f
	}
	accept := r.Header.Get("Accept")
	switch {
	case strings.Contains(accept, "text/csv"):
		return "csv"
	case strings.Contains(accept, "application/pdf"):
		return "pdf"
	default:
		return "json"
	}
}

// ---------------------------------------------------------------------------
// GET /api/v1/analytics/pnl
// ---------------------------------------------------------------------------

// pnlRowJSON is the wire shape — decimals as fixed-point strings.
type pnlRowJSON struct {
	AccountID    int64  `json:"account_id"`
	InstrumentID int64  `json:"instrument_id"`
	Symbol       string `json:"symbol"`
	Day          string `json:"day"`
	Realized     string `json:"realized"`
	Unrealized   string `json:"unrealized"`
	Fees         string `json:"fees"`
	Net          string `json:"net"`
}

// AnalyticsPnL serves the per-account P&L report. Account scoping follows
// the account.go convention: claims carry the account; a foreign
// ?account_id= is 403.
func AnalyticsPnL(d *AnalyticsDeps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		accountID, claims, ok := claimsAccount(w, r)
		if !ok {
			return
		}
		if rejectForeignAccount(w, r, claims, r.URL.Query().Get("account_id")) {
			return
		}
		if d == nil || d.PnL == nil {
			WriteError(w, "SERVICE_DEGRADED",
				"analytics store unavailable", gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		from, to, ok := analyticsWindow(w, r, d.now())
		if !ok {
			return
		}
		format := analyticsFormat(r)
		if format != "json" && format != "csv" && format != "pdf" {
			WriteError(w, "INVALID_REQUEST",
				"format must be json|csv|pdf", gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		rows, err := d.PnL.Report(r.Context(), accountID, from, to)
		if err != nil {
			WriteError(w, "SERVICE_DEGRADED",
				"pnl report store unavailable", gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		if rows == nil {
			rows = []analytics.PnLRow{}
		}
		rep := pnlReportJSON(accountID, from, to, d.now(), rows)
		switch format {
		case "csv":
			w.Header().Set("Content-Type", "text/csv; charset=utf-8")
			w.Header().Set("Content-Disposition",
				fmt.Sprintf(`attachment; filename="pnl-report-%d-%s.csv"`,
					accountID, to.Format("20060102")))
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(renderPnLCSV(accountID, from, to, d.now(), rows))
		case "pdf":
			w.Header().Set("Content-Type", "application/pdf")
			w.Header().Set("Content-Disposition",
				fmt.Sprintf(`attachment; filename="pnl-report-%d-%s.pdf"`,
					accountID, to.Format("20060102")))
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(renderPnLPDF(accountID, from, to, d.now(), rows))
		default:
			WriteJSON(w, http.StatusOK, rep)
		}
	}
}

// pnlReportJSON shapes the report payload: header + per-bucket rows +
// totals. Decimals render via String() (exact, no float rounding).
func pnlReportJSON(accountID int64, from, to, generated time.Time,
	rows []analytics.PnLRow) map[string]any {
	out := make([]pnlRowJSON, 0, len(rows))
	totalR, totalU, totalF := decimal.Zero, decimal.Zero, decimal.Zero
	for _, r := range rows {
		out = append(out, pnlRowJSON{
			AccountID:    r.AccountID,
			InstrumentID: r.InstrumentID,
			Symbol:       r.Symbol,
			Day:          r.Day.UTC().Format("2006-01-02"),
			Realized:     r.Realized.String(),
			Unrealized:   r.Unrealized.String(),
			Fees:         r.Fees.String(),
			Net:          r.Net().String(),
		})
		totalR = totalR.Add(r.Realized)
		totalU = totalU.Add(r.Unrealized)
		totalF = totalF.Add(r.Fees)
	}
	return map[string]any{
		"account_id":   accountID,
		"from":         from.UTC().Format(time.RFC3339),
		"to":           to.UTC().Format(time.RFC3339),
		"generated_at": generated.UTC().Format(time.RFC3339),
		"rows":         out,
		"totals": map[string]any{
			"realized":   totalR.String(),
			"unrealized": totalU.String(),
			"fees":       totalF.String(),
			"net":        totalR.Add(totalU).Sub(totalF).String(),
		},
	}
}

// renderPnLCSV produces the export: header block, one row per bucket,
// totals block. Decimals are String() — never float (§5.3).
func renderPnLCSV(accountID int64, from, to, generated time.Time,
	rows []analytics.PnLRow) []byte {
	var b bytes.Buffer
	fmt.Fprintf(&b, "account_id,%d\n", accountID)
	fmt.Fprintf(&b, "from,%s\n", from.UTC().Format(time.RFC3339))
	fmt.Fprintf(&b, "to,%s\n", to.UTC().Format(time.RFC3339))
	fmt.Fprintf(&b, "generated_at,%s\n", generated.UTC().Format(time.RFC3339))
	b.WriteString("\naccount_id,instrument_id,symbol,day,realized,unrealized,fees,net\n")
	totalR, totalU, totalF := decimal.Zero, decimal.Zero, decimal.Zero
	for _, r := range rows {
		fmt.Fprintf(&b, "%d,%d,%s,%s,%s,%s,%s,%s\n",
			r.AccountID, r.InstrumentID, analyticsCSVField(r.Symbol),
			r.Day.UTC().Format("2006-01-02"),
			r.Realized.String(), r.Unrealized.String(),
			r.Fees.String(), r.Net().String())
		totalR = totalR.Add(r.Realized)
		totalU = totalU.Add(r.Unrealized)
		totalF = totalF.Add(r.Fees)
	}
	b.WriteString("\nrealized,unrealized,fees,net\n")
	fmt.Fprintf(&b, "%s,%s,%s,%s\n",
		totalR.String(), totalU.String(), totalF.String(),
		totalR.Add(totalU).Sub(totalF).String())
	return b.Bytes()
}

func analyticsCSVField(s string) string {
	if strings.ContainsAny(s, ",\"\n") {
		return `"` + strings.ReplaceAll(s, `"`, `""`) + `"`
	}
	return s
}

// renderPnLPDF builds the one-font tabular export via the shared minimal
// PDF writer.
func renderPnLPDF(accountID int64, from, to, generated time.Time,
	rows []analytics.PnLRow) []byte {
	lines := []PDFLine{
		{Text: fmt.Sprintf("Account %d   Window %s .. %s (UTC)",
			accountID, from.UTC().Format("2006-01-02"), to.UTC().Format("2006-01-02")), Size: 11},
		{Text: fmt.Sprintf("Generated %s UTC   (amounts in each instrument's quote currency)",
			generated.UTC().Format("2006-01-02 15:04:05")), Size: 9},
		{Text: " ", Size: 8},
		{Text: fmt.Sprintf("%-10s %-12s %14s %14s %12s %14s",
			"Day", "Symbol", "Realized", "Unrealized", "Fees", "Net"), Size: 9},
	}
	totalR, totalU, totalF := decimal.Zero, decimal.Zero, decimal.Zero
	for _, r := range rows {
		lines = append(lines, PDFLine{Text: fmt.Sprintf(
			"%-10s %-12s %14s %14s %12s %14s",
			r.Day.UTC().Format("2006-01-02"),
			pdfTrunc(r.Symbol, 12),
			r.Realized.StringFixed(4), r.Unrealized.StringFixed(4),
			r.Fees.StringFixed(4), r.Net().StringFixed(4)), Size: 9})
		totalR = totalR.Add(r.Realized)
		totalU = totalU.Add(r.Unrealized)
		totalF = totalF.Add(r.Fees)
	}
	lines = append(lines,
		PDFLine{Text: " ", Size: 8},
		PDFLine{Text: "Totals are additive only within one currency — mixed-currency", Size: 9},
		PDFLine{Text: "accounts must read per-instrument rows, not the cross-ccy sum.", Size: 9},
		PDFLine{Text: fmt.Sprintf("%-24s %14s %14s %12s %14s",
			"Totals", totalR.StringFixed(4), totalU.StringFixed(4),
			totalF.StringFixed(4),
			totalR.Add(totalU).Sub(totalF).StringFixed(4)), Size: 10})
	return RenderPDFDoc("Exchange - Account P&L Report", lines)
}

func pdfTrunc(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max]
}

// ---------------------------------------------------------------------------
// GET /api/v1/analytics/volume
// ---------------------------------------------------------------------------

type volumeRowJSON struct {
	Symbol      string `json:"symbol"`
	BucketStart string `json:"bucket_start"`
	Granularity string `json:"granularity"`
	Volume      string `json:"volume"`
	QuoteVolume string `json:"quote_volume"`
	TradeCount  int64  `json:"trade_count"`
}

// AnalyticsVolume serves per-symbol volume buckets. Public endpoint —
// numbers are venue aggregates, no account data. granularity=1h returns
// the stored hourly buckets; 1d rolls them up per UTC day.
func AnalyticsVolume(d *AnalyticsDeps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if d == nil || d.Stats == nil {
			WriteError(w, "SERVICE_DEGRADED",
				"analytics store unavailable", gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		from, to, ok := analyticsWindow(w, r, d.now())
		if !ok {
			return
		}
		q := r.URL.Query()
		granularity := q.Get("granularity")
		if granularity != "" && granularity != analytics.GranularityHour &&
			granularity != analytics.GranularityDay {
			WriteError(w, "INVALID_REQUEST",
				"granularity must be 1h|1d", gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		limit, ok := intParam(q.Get("limit"), 5000, 1, analyticsVolumeMaxLimit)
		if !ok {
			WriteError(w, "INVALID_REQUEST",
				fmt.Sprintf("limit must be 1..%d", analyticsVolumeMaxLimit),
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		rows, err := d.Stats.Volume(r.Context(), from, to,
			q.Get("symbol"), granularity, limit)
		if err != nil {
			WriteError(w, "SERVICE_DEGRADED",
				"volume store unavailable", gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		out := make([]volumeRowJSON, 0, len(rows))
		for _, v := range rows {
			out = append(out, volumeRowJSON{
				Symbol:      v.Symbol,
				BucketStart: v.BucketStart.UTC().Format(time.RFC3339),
				Granularity: v.Granularity,
				Volume:      v.Volume.String(),
				QuoteVolume: v.QuoteVolume.String(),
				TradeCount:  v.TradeCount,
			})
		}
		WriteJSON(w, http.StatusOK, map[string]any{
			"from":         from.UTC().Format(time.RFC3339),
			"to":           to.UTC().Format(time.RFC3339),
			"generated_at": d.now().Format(time.RFC3339),
			"count":        len(out),
			"limit":        limit,
			"truncated":    len(out) == limit,
			"rows":         out,
		})
	}
}

// ---------------------------------------------------------------------------
// GET /api/v1/analytics/stats
// ---------------------------------------------------------------------------

// AnalyticsStats serves venue trading statistics: per-symbol fill rate
// plus trade count per (symbol, client_category tier). Public — venue
// aggregates only.
func AnalyticsStats(d *AnalyticsDeps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if d == nil || d.Stats == nil {
			WriteError(w, "SERVICE_DEGRADED",
				"analytics store unavailable", gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		from, to, ok := analyticsWindow(w, r, d.now())
		if !ok {
			return
		}
		fills, err := d.Stats.FillRates(r.Context(), from, to)
		if err != nil {
			WriteError(w, "SERVICE_DEGRADED",
				"stats store unavailable", gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		tiers, err := d.Stats.TradesPerTier(r.Context(), from, to)
		if err != nil {
			WriteError(w, "SERVICE_DEGRADED",
				"stats store unavailable", gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		fillJSON := make([]map[string]any, 0, len(fills))
		var totSub, totFilled int64
		for _, f := range fills {
			totSub += f.OrdersSubmitted
			totFilled += f.OrdersFilled
			row := map[string]any{
				"symbol":           f.Symbol,
				"orders_submitted": f.OrdersSubmitted,
				"orders_filled":    f.OrdersFilled,
				"fill_rate":        nil,
			}
			if rate := f.Rate(); rate != nil {
				row["fill_rate"] = rate.String()
			}
			fillJSON = append(fillJSON, row)
		}
		tierJSON := make([]map[string]any, 0, len(tiers))
		var totParticipations int64
		totVol := decimal.Zero
		for _, t := range tiers {
			totParticipations += t.TradeCount
			totVol = totVol.Add(t.Volume)
			tierJSON = append(tierJSON, map[string]any{
				"symbol":      t.Symbol,
				"tier":        t.Tier,
				"trade_count": t.TradeCount,
				"volume":      t.Volume.String(),
			})
		}
		var totRate any
		if totSub > 0 {
			totRate = decimal.NewFromInt(totFilled).
				Div(decimal.NewFromInt(totSub)).String()
		}
		WriteJSON(w, http.StatusOK, map[string]any{
			"from":         from.UTC().Format(time.RFC3339),
			"to":           to.UTC().Format(time.RFC3339),
			"generated_at": d.now().Format(time.RFC3339),
			"fill_rates":   fillJSON,
			// The 'tier' axis is accounts.client_category (MiFID II
			// RETAIL|PROFESSIONAL|ELIGIBLE_COUNTERPARTY) — documented on
			// analytics.TierTradeRow; counts are side-participations
			// (buy leg + sell leg), ≈ 2× executions venue-wide.
			"trade_counts_by_tier": tierJSON,
			"totals": map[string]any{
				"orders_submitted":     totSub,
				"orders_filled":        totFilled,
				"fill_rate":            totRate,
				"trade_participations": totParticipations,
				"base_volume":          totVol.String(),
			},
		})
	}
}
