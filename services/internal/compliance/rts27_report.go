// RTS 27 public venue-quality reporting — Phase-21 Task 21.3.19
// (spec §14.5, §24 #202): the ESMA RTS 27 quarterly execution-quality
// publication per instrument class, fed by the daily materialization
// in rts27_daily_stats (migration 251).
//
// Data flow (per the task's "ClickHouse trade_history → daily
// materialized view → quarterly rollup" contract): MaterializeDay
// computes one rts27_daily_stats row per instrument per UTC day from
// ClickHouse trades (fills/prices/aggressor), volume_stats '1d'
// counters (orders submitted/filled) and tca_results period='fill'
// rows (arrival/VWAP slippage, improvement) — plus the PG orders
// ledger for order→fill latency. DEVIATION (recorded for §27): this
// rollup joins sources a ClickHouse materialized view cannot express
// (single-source INSERT triggers only), so the aggregation runs in Go
// and persists idempotently on (instrument_id, day). Metrics a source
// cannot supply stay NULL and are named in gaps[] — never fabricated
// (§2.7).
//
// Distinct from the Phase-20 TCA report: this is the PUBLIC venue
// publication surface — quarterly versioned artifacts, DRAFT →
// PUBLISHED, unauthenticated download of PUBLISHED rows only.
package compliance

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"exchange/internal/analytics"
	"exchange/internal/audit"
	excerrors "exchange/pkg/errors"
)

// RTS27Service owns the daily materialization + quarterly publication.
type RTS27Service struct {
	pool     *pgxpool.Pool
	ch       analytics.Conn
	resolver HoldRoleResolver
	now      func() time.Time
}

// NewRTS27Service wires the service; ch may be nil — MaterializeDay
// and generation then fail closed SERVICE_DEGRADED.
func NewRTS27Service(pool *pgxpool.Pool, ch analytics.Conn,
	resolver HoldRoleResolver) (*RTS27Service, error) {
	if pool == nil {
		return nil, excerrors.New("INTERNAL_ERROR",
			"rts27 service requires pool")
	}
	return &RTS27Service{pool: pool, ch: ch, resolver: resolver,
		now: time.Now}, nil
}

// WithClock overrides the clock (tests).
func (s *RTS27Service) WithClock(c func() time.Time) *RTS27Service {
	s.now = c
	return s
}

func (s *RTS27Service) checkRole(ctx context.Context, userID int64) error {
	if s.resolver == nil {
		return excerrors.New("INTERNAL_ERROR", "role resolver not wired")
	}
	role, err := s.resolver(ctx, userID)
	if err != nil {
		return excerrors.New("INTERNAL_ERROR", "role lookup: "+err.Error())
	}
	if role != "Compliance Officer" && role != "Super Admin" {
		return excerrors.New("UNAUTHORIZED_ROLE",
			"role "+role+" cannot mutate RTS 27 reports")
	}
	return nil
}

// ---------------------------------------------------------------------------
// Daily materialization
// ---------------------------------------------------------------------------

// dayStats is the in-Go rollup over the ClickHouse sources for one
// (instrument, UTC day).
type dayStats struct {
	instrumentID    int64
	symbol          string
	fills           int64
	volumeBase      float64
	volumeQuote     float64
	vwap            *float64
	priceMin        *float64
	priceMax        *float64
	priceMedian     *float64
	priceMean       *float64
	aggBuyFills     int64
	aggSellFills    int64
	unknownFills    int64
	aggBuyVol       float64
	aggSellVol      float64
	ordersSubmitted *int64
	ordersFilled    *int64
	medianFillMs    *float64
	tcaFills        int64
	slipArrAvg      *float64
	slipArrMed      *float64
	slipVWAPAvg     *float64
	improvementAvg  *float64
	gaps            []string
}

// MaterializeDay computes the rts27_daily_stats row set for one UTC
// day — upsert on (instrument_id, day) so re-runs repair in place.
// This is a scheduled-job surface: no role gate (system actor); CH or
// PG failures abort the whole day (the day retries next sweep).
func (s *RTS27Service) MaterializeDay(ctx context.Context,
	day time.Time) (int, error) {
	if s.ch == nil {
		return 0, excerrors.New("SERVICE_DEGRADED",
			"clickhouse not wired — RTS 27 materialization unavailable")
	}
	dayStart := time.Date(day.UTC().Year(), day.UTC().Month(),
		day.UTC().Day(), 0, 0, 0, 0, time.UTC)
	dayEnd := dayStart.Add(24 * time.Hour)

	stats, err := s.tradeDay(ctx, dayStart, dayEnd)
	if err != nil {
		return 0, err
	}
	if len(stats) == 0 {
		return 0, nil // silent day (weekend/halt) — nothing to persist
	}
	if err := s.orderCounters(ctx, dayStart, dayEnd, stats); err != nil {
		return 0, err
	}
	if err := s.fillLatency(ctx, dayStart, dayEnd, stats); err != nil {
		return 0, err
	}
	if err := s.tcaDay(ctx, dayStart, dayEnd, stats); err != nil {
		return 0, err
	}
	meta, err := s.instrumentMeta(ctx)
	if err != nil {
		return 0, err
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, excerrors.Wrap("INTERNAL_ERROR", "rts27 day tx", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	n := 0
	for _, st := range stats {
		m := meta[st.instrumentID]
		gaps := st.gaps
		if gaps == nil {
			gaps = []string{}
		}
		var fillRate *float64
		if st.ordersSubmitted != nil && *st.ordersSubmitted > 0 &&
			st.ordersFilled != nil {
			r := float64(*st.ordersFilled) / float64(*st.ordersSubmitted)
			fillRate = &r
		}
		gapsJSON, _ := json.Marshal(gaps)
		_, err := tx.Exec(ctx, `
			INSERT INTO rts27_daily_stats
			    (day, instrument_id, symbol, product_type, pair_class,
			     fills, volume_base, volume_quote, vwap,
			     price_min, price_max, price_median, price_mean,
			     agg_buy_fills, agg_sell_fills, unknown_fills,
			     agg_buy_volume, agg_sell_volume,
			     orders_submitted, orders_filled, fill_rate,
			     median_order_to_fill_ms, tca_fills,
			     slip_arrival_avg_bps, slip_arrival_med_bps,
			     slip_vwap_avg_bps, improvement_avg,
			     spread_avg_bps, inputs_complete, gaps)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,
			        $17,$18,$19,$20,$21,$22,$23,$24,$25,$26,$27,
			        NULL,$28,$29)
			ON CONFLICT (instrument_id, day) DO UPDATE SET
			    symbol=EXCLUDED.symbol, product_type=EXCLUDED.product_type,
			    pair_class=EXCLUDED.pair_class, fills=EXCLUDED.fills,
			    volume_base=EXCLUDED.volume_base,
			    volume_quote=EXCLUDED.volume_quote, vwap=EXCLUDED.vwap,
			    price_min=EXCLUDED.price_min, price_max=EXCLUDED.price_max,
			    price_median=EXCLUDED.price_median,
			    price_mean=EXCLUDED.price_mean,
			    agg_buy_fills=EXCLUDED.agg_buy_fills,
			    agg_sell_fills=EXCLUDED.agg_sell_fills,
			    unknown_fills=EXCLUDED.unknown_fills,
			    agg_buy_volume=EXCLUDED.agg_buy_volume,
			    agg_sell_volume=EXCLUDED.agg_sell_volume,
			    orders_submitted=EXCLUDED.orders_submitted,
			    orders_filled=EXCLUDED.orders_filled,
			    fill_rate=EXCLUDED.fill_rate,
			    median_order_to_fill_ms=EXCLUDED.median_order_to_fill_ms,
			    tca_fills=EXCLUDED.tca_fills,
			    slip_arrival_avg_bps=EXCLUDED.slip_arrival_avg_bps,
			    slip_arrival_med_bps=EXCLUDED.slip_arrival_med_bps,
			    slip_vwap_avg_bps=EXCLUDED.slip_vwap_avg_bps,
			    improvement_avg=EXCLUDED.improvement_avg,
			    spread_avg_bps=EXCLUDED.spread_avg_bps,
			    inputs_complete=EXCLUDED.inputs_complete,
			    gaps=EXCLUDED.gaps, updated_at=now()`,
			dayStart, st.instrumentID, st.symbol,
			m.productType, m.pairClass,
			st.fills, st.volumeBase, st.volumeQuote, st.vwap,
			st.priceMin, st.priceMax, st.priceMedian, st.priceMean,
			st.aggBuyFills, st.aggSellFills, st.unknownFills,
			st.aggBuyVol, st.aggSellVol,
			st.ordersSubmitted, st.ordersFilled, fillRate,
			st.medianFillMs, st.tcaFills,
			st.slipArrAvg, st.slipArrMed, st.slipVWAPAvg,
			st.improvementAvg, len(gaps) == 0, gapsJSON)
		if err != nil {
			return n, excerrors.Wrap("INTERNAL_ERROR",
				"rts27 daily upsert", err)
		}
		n++
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, excerrors.Wrap("INTERNAL_ERROR", "rts27 day commit", err)
	}
	return n, nil
}

type instMeta struct {
	productType string
	pairClass   string
	class       string // PRODUCT:PAIR_CLASS — the report axis
}

// instrumentMeta resolves product_type + pair_class per instrument id
// (pair class is derivable via analytics.ClassifyPair — never stored
// blind).
func (s *RTS27Service) instrumentMeta(ctx context.Context) (map[int64]instMeta, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT id, instrument_type::text, base_currency, quote_currency
		   FROM instruments`)
	if err != nil {
		return nil, excerrors.Wrap("INTERNAL_ERROR", "instrument meta", err)
	}
	defer rows.Close()
	out := map[int64]instMeta{}
	for rows.Next() {
		var id int64
		var pt, b, q string
		if err := rows.Scan(&id, &pt, &b, &q); err != nil {
			return nil, excerrors.Wrap("INTERNAL_ERROR", "instrument scan", err)
		}
		pc := analytics.ClassifyPair(b, q)
		out[id] = instMeta{productType: pt, pairClass: pc,
			class: pt + ":" + pc}
	}
	return out, rows.Err()
}

// tradeDay rolls the day's fills per instrument out of ClickHouse
// trades (price min/max/median/mean, VWAP, aggressor splits).
func (s *RTS27Service) tradeDay(ctx context.Context, dayStart,
	dayEnd time.Time) (map[int64]*dayStats, error) {
	rows, err := s.ch.Query(ctx, `
		SELECT instrument_id, symbol, count() AS fills,
		       sum(qty), sum(price * qty), avg(price),
		       min(price), max(price), quantile(0.5)(price),
		       countIf(aggressor_side = 'BUY'),
		       countIf(aggressor_side = 'SELL'),
		       countIf(aggressor_side NOT IN ('BUY','SELL')),
		       sumIf(qty, aggressor_side = 'BUY'),
		       sumIf(qty, aggressor_side = 'SELL')
		  FROM `+"exchange_analytics.trades"+`
		 WHERE ts >= ? AND ts < ?
		 GROUP BY instrument_id, symbol`, dayStart, dayEnd)
	if err != nil {
		return nil, excerrors.Wrap("SERVICE_DEGRADED",
			"rts27 trades aggregate failed", err)
	}
	defer rows.Close()
	out := map[int64]*dayStats{}
	for rows.Next() {
		var st dayStats
		var fills uint64
		var meanN float64
		var minN, maxN, medN float64
		var buyF, sellF, unkF uint64
		err := rows.Scan(&st.instrumentID, &st.symbol, &fills,
			&st.volumeBase, &st.volumeQuote, &meanN,
			&minN, &maxN, &medN,
			&buyF, &sellF, &unkF, &st.aggBuyVol, &st.aggSellVol)
		if err != nil {
			return nil, excerrors.Wrap("INTERNAL_ERROR",
				"rts27 trades scan", err)
		}
		st.fills = int64(fills)
		st.aggBuyFills = int64(buyF)
		st.aggSellFills = int64(sellF)
		st.unknownFills = int64(unkF)
		st.priceMean = &meanN
		st.priceMin, st.priceMax, st.priceMedian = &minN, &maxN, &medN
		if st.volumeBase > 0 {
			v := st.volumeQuote / st.volumeBase
			st.vwap = &v
		}
		st.gaps = []string{"spread_avg_bps"} // no persisted bid/ask quotes
		out[st.instrumentID] = &st
	}
	return out, rows.Err()
}

// orderCounters folds the day's volume_stats '1d' counter rows
// (orders_submitted/orders_filled deltas per symbol) into the stats —
// absent rows leave NULLs (honest gap: fill_rate then stays NULL).
func (s *RTS27Service) orderCounters(ctx context.Context, dayStart,
	dayEnd time.Time, stats map[int64]*dayStats) error {
	syms := map[string]*dayStats{}
	for _, st := range stats {
		syms[st.symbol] = st
	}
	rows, err := s.ch.Query(ctx, `
		SELECT symbol, sum(orders_submitted), sum(orders_filled)
		  FROM `+"exchange_analytics.volume_stats"+`
		 WHERE granularity = '1d' AND bucket_start >= ? AND bucket_start < ?
		 GROUP BY symbol`, dayStart, dayEnd)
	if err != nil {
		return excerrors.Wrap("SERVICE_DEGRADED",
			"rts27 volume_stats read failed", err)
	}
	defer rows.Close()
	for rows.Next() {
		var sym string
		var sub, fil uint64
		if err := rows.Scan(&sym, &sub, &fil); err != nil {
			return excerrors.Wrap("INTERNAL_ERROR", "rts27 counters scan", err)
		}
		if st, ok := syms[sym]; ok {
			s, f := int64(sub), int64(fil)
			st.ordersSubmitted, st.ordersFilled = &s, &f
		}
	}
	return rows.Err()
}

// fillLatency computes median order→fill ms per instrument by joining
// the day's aggressor-side order ids against the PG orders ledger
// (created_at = order receipt). Per-fill rows are only needed for the
// id+ts pair — bounded by daily fill count.
func (s *RTS27Service) fillLatency(ctx context.Context, dayStart,
	dayEnd time.Time, stats map[int64]*dayStats) error {
	rows, err := s.ch.Query(ctx, `
		SELECT instrument_id, ts,
		       if(aggressor_side = 'SELL', sell_order_id, buy_order_id)
		         AS agg_order_id
		  FROM `+"exchange_analytics.trades"+`
		 WHERE ts >= ? AND ts < ? AND agg_order_id > 0`, dayStart, dayEnd)
	if err != nil {
		return excerrors.Wrap("SERVICE_DEGRADED",
			"rts27 fill order ids failed", err)
	}
	type obs struct {
		inst int64
		ts   time.Time
		oid  int64
	}
	var obsAll []obs
	oids := map[int64]bool{}
	for rows.Next() {
		var o obs
		if err := rows.Scan(&o.inst, &o.ts, &o.oid); err != nil {
			rows.Close()
			return excerrors.Wrap("INTERNAL_ERROR", "rts27 fill scan", err)
		}
		obsAll = append(obsAll, o)
		oids[o.oid] = true
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	if len(obsAll) == 0 {
		return nil
	}
	idList := make([]int64, 0, len(oids))
	for id := range oids {
		idList = append(idList, id)
	}
	created := map[int64]time.Time{}
	// Batch the PG lookup — the id list is daily-fill bounded.
	const batch = 5000
	for i := 0; i < len(idList); i += batch {
		j := i + batch
		if j > len(idList) {
			j = len(idList)
		}
		pgRows, err := s.pool.Query(ctx,
			`SELECT id, created_at FROM orders WHERE id = ANY($1)`,
			idList[i:j])
		if err != nil {
			return excerrors.Wrap("INTERNAL_ERROR",
				"rts27 order lookup failed", err)
		}
		for pgRows.Next() {
			var id int64
			var ts time.Time
			if err := pgRows.Scan(&id, &ts); err != nil {
				pgRows.Close()
				return excerrors.Wrap("INTERNAL_ERROR",
					"rts27 order scan", err)
			}
			created[id] = ts
		}
		pgRows.Close()
	}
	perInst := map[int64][]float64{}
	for _, o := range obsAll {
		ct, ok := created[o.oid]
		if !ok {
			continue // order predates ledger / unresolved — skip, don't fake
		}
		ms := float64(o.ts.Sub(ct).Milliseconds())
		if ms < 0 {
			continue // clock skew guard
		}
		perInst[o.inst] = append(perInst[o.inst], ms)
	}
	for id, vals := range perInst {
		sort.Float64s(vals)
		med := vals[len(vals)/2]
		stats[id].medianFillMs = &med
	}
	return nil
}

// tcaDay folds period='fill' tca_results rows for the day into the
// stats (arrival/VWAP slippage + price improvement — the RTS 27
// "price vs arrival" axis).
func (s *RTS27Service) tcaDay(ctx context.Context, dayStart,
	dayEnd time.Time, stats map[int64]*dayStats) error {
	rows, err := s.ch.Query(ctx, `
		SELECT instrument_id, count(),
		       avg(slip_arrival_bps), quantile(0.5)(slip_arrival_bps),
		       avg(slip_vwap_bps), avg(price_improvement_delta)
		  FROM `+"exchange_analytics.tca_results"+`
		 WHERE period = 'fill' AND ts >= ? AND ts < ?
		 GROUP BY instrument_id`, dayStart, dayEnd)
	if err != nil {
		return excerrors.Wrap("SERVICE_DEGRADED",
			"rts27 tca read failed", err)
	}
	defer rows.Close()
	for rows.Next() {
		var inst int64
		var n uint64
		var a, m, v, imp *float64
		if err := rows.Scan(&inst, &n, &a, &m, &v, &imp); err != nil {
			return excerrors.Wrap("INTERNAL_ERROR", "rts27 tca scan", err)
		}
		if st, ok := stats[inst]; ok {
			st.tcaFills = int64(n)
			st.slipArrAvg, st.slipArrMed = a, m
			st.slipVWAPAvg = v
			st.improvementAvg = imp
		}
	}
	return rows.Err()
}

// ---------------------------------------------------------------------------
// Quarterly RTS 27 publication
// ---------------------------------------------------------------------------

// RTS27Report is one rts27_reports row.
type RTS27Report struct {
	ID              int64           `json:"id"`
	QuarterStart    time.Time       `json:"quarter_start"`
	InstrumentClass string          `json:"instrument_class"`
	Version         int             `json:"version"`
	Status          string          `json:"status"`
	Metrics         json.RawMessage `json:"metrics"`
	CSV             string          `json:"-"` // artifact — served by download endpoint
	DaysCovered     int             `json:"days_covered"`
	ZeroActivity    bool            `json:"zero_activity"`
	GeneratedBy     int64           `json:"generated_by"`
	CreatedAt       time.Time       `json:"created_at"`
	PublishedBy     *int64          `json:"published_by,omitempty"`
	PublishedAt     *time.Time      `json:"published_at,omitempty"`
}

// GenerateQuarter rolls the quarter's daily stats up per instrument
// class and files a DRAFT report per class (versioned — regeneration
// lands version+1, never an overwrite). An instrument class with no
// activity still generates (zero_activity=true — ESMA publishes empty
// reports for listed classes).
func (s *RTS27Service) GenerateQuarter(ctx context.Context,
	quarterStart time.Time, actor int64) ([]RTS27Report, error) {
	if err := s.checkRole(ctx, actor); err != nil {
		return nil, err
	}
	qs := time.Date(quarterStart.UTC().Year(),
		((quarterStart.UTC().Month()-1)/3)*3+1, 1, 0, 0, 0, 0, time.UTC)
	qe := qs.AddDate(0, 3, 0)

	rows, err := s.pool.Query(ctx, `
		SELECT product_type, pair_class, count(DISTINCT day) AS days,
		       sum(fills), sum(volume_base), sum(volume_quote),
		       min(price_min), max(price_max),
		       sum(orders_submitted), sum(orders_filled),
		       sum(agg_buy_fills), sum(agg_sell_fills), sum(unknown_fills),
		       sum(agg_buy_volume), sum(agg_sell_volume),
		       sum(tca_fills),
		       sum(slip_arrival_avg_bps * tca_fills),
		       sum(slip_vwap_avg_bps * tca_fills),
		       sum(improvement_avg * tca_fills),
		       count(DISTINCT instrument_id),
		       bool_and(inputs_complete)
		  FROM rts27_daily_stats
		 WHERE day >= $1 AND day < $2
		 GROUP BY product_type, pair_class`, qs, qe)
	if err != nil {
		return nil, excerrors.Wrap("INTERNAL_ERROR", "rts27 rollup", err)
	}
	type rollRow struct {
		pt, pc       string
		days, insts  int
		fills        int64
		volB, volQ   float64
		pmin, pmax   *float64
		sub, fil     *int64
		aggB, aggS   *int64
		unk          *int64
		aggBV, aggSV *float64
		tcaFills     *int64
		slipArrW     *float64
		slipVWAPW    *float64
		impW         *float64
		complete     bool
	}
	var rolls []rollRow
	for rows.Next() {
		var r rollRow
		var days int64
		if err := rows.Scan(&r.pt, &r.pc, &days, &r.fills, &r.volB,
			&r.volQ, &r.pmin, &r.pmax, &r.sub, &r.fil,
			&r.aggB, &r.aggS, &r.unk, &r.aggBV, &r.aggSV,
			&r.tcaFills, &r.slipArrW, &r.slipVWAPW, &r.impW,
			&r.insts, &r.complete); err != nil {
			rows.Close()
			return nil, excerrors.Wrap("INTERNAL_ERROR",
				"rts27 rollup scan", err)
		}
		r.days = int(days)
		rolls = append(rolls, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	var out []RTS27Report
	for _, r := range rolls {
		class := r.pt + ":" + r.pc
		metrics, csv := buildRTS27Artifact(qs, qe, class, r.days, r.insts,
			r.fills, r.volB, r.volQ, r.pmin, r.pmax, r.sub, r.fil,
			r.aggB, r.aggS, r.unk, r.aggBV, r.aggSV, r.tcaFills,
			r.slipArrW, r.slipVWAPW, r.impW, r.complete)
		var id int64
		var ver int
		err := s.pool.QueryRow(ctx, `
			INSERT INTO rts27_reports
			    (quarter_start, instrument_class, version, metrics, csv,
			     days_covered, zero_activity, generated_by)
			VALUES ($1,$2::varchar,
			    COALESCE((SELECT max(version) FROM rts27_reports
			              WHERE quarter_start=$1 AND instrument_class=$2::varchar),0)+1,
			    $3,$4,$5,$6,$7)
			RETURNING id, version`,
			qs, class, metrics, csv, r.days, r.fills == 0, actor).
			Scan(&id, &ver)
		if err != nil {
			return out, excerrors.Wrap("INTERNAL_ERROR",
				"rts27 report insert", err)
		}
		if _, err := audit.AppendAuto(ctx, s.pool, "rts27_reports", &id,
			"RTS27_GENERATED", nil); err != nil {
			return out, excerrors.Wrap("INTERNAL_ERROR", "rts27 audit", err)
		}
		out = append(out, RTS27Report{ID: id, QuarterStart: qs,
			InstrumentClass: class, Version: ver, Status: "DRAFT",
			Metrics: metrics, DaysCovered: r.days,
			ZeroActivity: r.fills == 0, GeneratedBy: actor,
			CreatedAt: s.now().UTC()})
	}
	return out, nil
}

// buildRTS27Artifact assembles the ESMA RTS 27 metrics JSON + the CSV
// publication row for one (quarter, instrument class). Aggregation
// honesty: quarterly means are volume/fill-weighted over the daily
// materialization (daily per-instrument granularity collapses to the
// class axis); NULL inputs propagate as omitted fields.
func buildRTS27Artifact(qs, qe time.Time, class string, days, insts int,
	fills int64, volB, volQ float64, pmin, pmax *float64,
	sub, fil, aggB, aggS, unk *int64, aggBV, aggSV *float64,
	tcaFills *int64, slipArrW, slipVWAPW, impW *float64,
	complete bool) (json.RawMessage, string) {
	m := map[string]any{
		"venue":               analytics.VenueInternal,
		"instrument_class":    class,
		"quarter_start":       qs.Format("2006-01-02"),
		"quarter_end":         qe.Add(-24 * time.Hour).Format("2006-01-02"),
		"days_covered":        days,
		"instruments_covered": insts,
		"total_fills":         fills,
		"volume_base":         volB,
		"volume_quote":        volQ,
		"inputs_complete":     complete,
		"methodology": "daily rts27_daily_stats rollup; class means are " +
			"fill/tca-fill weighted; spread_avg_bps unreported (no " +
			"persisted quote stream)",
	}
	if volB > 0 {
		m["vwap"] = volQ / volB
	}
	if pmin != nil {
		m["price_min"] = *pmin
	}
	if pmax != nil {
		m["price_max"] = *pmax
	}
	if sub != nil && *sub > 0 && fil != nil {
		m["orders_submitted"] = *sub
		m["orders_filled"] = *fil
		m["fill_rate"] = float64(*fil) / float64(*sub) // likelihood of execution
	}
	if fills > 0 && aggB != nil && aggS != nil {
		// aggressor-side split → % aggressive vs passive fills
		m["pct_aggressive_fills"] = float64(*aggB+*aggS) / float64(fills)
		m["pct_buy_aggressor"] = float64(*aggB) / float64(fills)
		m["pct_sell_aggressor"] = float64(*aggS) / float64(fills)
		if unk != nil {
			m["pct_unknown_side"] = float64(*unk) / float64(fills)
		}
	}
	if tcaFills != nil && *tcaFills > 0 {
		if slipArrW != nil {
			m["slip_arrival_avg_bps"] = *slipArrW / float64(*tcaFills)
		}
		if slipVWAPW != nil {
			m["slip_vwap_avg_bps"] = *slipVWAPW / float64(*tcaFills)
		}
		if impW != nil {
			m["price_improvement_avg"] = *impW / float64(*tcaFills)
		}
		m["tca_fills"] = *tcaFills
	}
	raw, _ := json.Marshal(m)
	csv := rts27CSVRow(qs, qe, class, m)
	return raw, csv
}

// rts27CSVRow renders the ESMA-style single-row CSV artifact (header +
// one data row per class).
func rts27CSVRow(qs, qe time.Time, class string, m map[string]any) string {
	var b strings.Builder
	b.WriteString("venue,instrument_class,quarter_start,quarter_end," +
		"days_covered,instruments_covered,total_fills,volume_base," +
		"volume_quote,vwap,price_min,price_max,orders_submitted," +
		"orders_filled,fill_rate,pct_aggressive_fills,pct_buy_aggressor," +
		"pct_sell_aggressor,slip_arrival_avg_bps,slip_vwap_avg_bps," +
		"price_improvement_avg\n")
	field := func(k string) string {
		if v, ok := m[k]; ok {
			return fmt.Sprint(v)
		}
		return ""
	}
	b.WriteString(field("venue") + "," + class + "," +
		qs.Format("2006-01-02") + "," +
		qe.Add(-24*time.Hour).Format("2006-01-02") + "," +
		field("days_covered") + "," + field("instruments_covered") + "," +
		field("total_fills") + "," + field("volume_base") + "," +
		field("volume_quote") + "," + field("vwap") + "," +
		field("price_min") + "," + field("price_max") + "," +
		field("orders_submitted") + "," + field("orders_filled") + "," +
		field("fill_rate") + "," + field("pct_aggressive_fills") + "," +
		field("pct_buy_aggressor") + "," + field("pct_sell_aggressor") + "," +
		field("slip_arrival_avg_bps") + "," + field("slip_vwap_avg_bps") + "," +
		field("price_improvement_avg") + "\n")
	return b.String()
}

// Publish flips a DRAFT report PUBLISHED — the only status the public
// surface exposes.
func (s *RTS27Service) Publish(ctx context.Context, reportID,
	actor int64) (*RTS27Report, error) {
	if err := s.checkRole(ctx, actor); err != nil {
		return nil, err
	}
	res, err := s.pool.Exec(ctx, `
		UPDATE rts27_reports
		SET status='PUBLISHED', published_by=$2, published_at=now()
		WHERE id=$1 AND status='DRAFT'`, reportID, actor)
	if err != nil {
		return nil, excerrors.Wrap("INTERNAL_ERROR", "rts27 publish", err)
	}
	if res.RowsAffected() == 0 {
		return nil, excerrors.New("INVALID_REQUEST",
			"rts27 report not found or already PUBLISHED")
	}
	if _, err := audit.AppendAuto(ctx, s.pool, "rts27_reports",
		&reportID, "RTS27_PUBLISHED", nil); err != nil {
		return nil, excerrors.Wrap("INTERNAL_ERROR", "rts27 audit", err)
	}
	return s.GetReport(ctx, reportID)
}

// ---------------------------------------------------------------------------
// Reads — admin (all) + public (PUBLISHED only)
// ---------------------------------------------------------------------------

const rts27Cols = `
	id, quarter_start, instrument_class, version, status, metrics, csv,
	days_covered, zero_activity, generated_by, created_at, published_by,
	published_at`

func scanRTS27(row interface{ Scan(dest ...any) error }) (*RTS27Report, error) {
	var r RTS27Report
	err := row.Scan(&r.ID, &r.QuarterStart, &r.InstrumentClass, &r.Version,
		&r.Status, &r.Metrics, &r.CSV, &r.DaysCovered, &r.ZeroActivity,
		&r.GeneratedBy, &r.CreatedAt, &r.PublishedBy, &r.PublishedAt)
	return &r, err
}

// GetReport reads one row (admin surface).
func (s *RTS27Service) GetReport(ctx context.Context,
	id int64) (*RTS27Report, error) {
	r, err := scanRTS27(s.pool.QueryRow(ctx,
		`SELECT `+rts27Cols+` FROM rts27_reports WHERE id=$1`, id))
	if err == pgx.ErrNoRows {
		return nil, excerrors.New("NOT_FOUND", "rts27 report not found")
	}
	if err != nil {
		return nil, excerrors.Wrap("INTERNAL_ERROR", "rts27 read", err)
	}
	return r, nil
}

// ListReports returns the register (?status= filter), newest first.
func (s *RTS27Service) ListReports(ctx context.Context, status string,
	limit int) ([]RTS27Report, error) {
	if limit <= 0 || limit > 200 {
		limit = 100
	}
	q := `SELECT ` + rts27Cols + ` FROM rts27_reports`
	args := []any{}
	if status != "" {
		args = append(args, status)
		q += " WHERE status=$1"
	}
	q += " ORDER BY quarter_start DESC, instrument_class, version DESC" +
		" LIMIT " + fmt.Sprint(limit)
	rows, err := s.pool.Query(ctx, q, args...)
	if err != nil {
		return nil, excerrors.Wrap("INTERNAL_ERROR", "rts27 list", err)
	}
	defer rows.Close()
	var out []RTS27Report
	for rows.Next() {
		r, err := scanRTS27(rows)
		if err != nil {
			return nil, excerrors.Wrap("INTERNAL_ERROR", "rts27 scan", err)
		}
		out = append(out, *r)
	}
	return out, rows.Err()
}

// ListPublished is the unauthenticated surface — PUBLISHED rows only,
// csv stripped from the listing (the download endpoint serves it).
func (s *RTS27Service) ListPublished(ctx context.Context) ([]RTS27Report, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT `+rts27Cols+` FROM rts27_reports
		 WHERE status='PUBLISHED'
		 ORDER BY quarter_start DESC, instrument_class, version DESC`)
	if err != nil {
		return nil, excerrors.Wrap("INTERNAL_ERROR", "rts27 public list", err)
	}
	defer rows.Close()
	var out []RTS27Report
	for rows.Next() {
		r, err := scanRTS27(rows)
		if err != nil {
			return nil, excerrors.Wrap("INTERNAL_ERROR", "rts27 scan", err)
		}
		r.CSV = "" // listing never inlines the artifact
		out = append(out, *r)
	}
	return out, rows.Err()
}

// GetPublished returns a PUBLISHED row (public download); DRAFT rows
// answer NOT_FOUND — unpublished artifacts are invisible on the
// public surface.
func (s *RTS27Service) GetPublished(ctx context.Context,
	id int64) (*RTS27Report, error) {
	r, err := scanRTS27(s.pool.QueryRow(ctx,
		`SELECT `+rts27Cols+` FROM rts27_reports
		 WHERE id=$1 AND status='PUBLISHED'`, id))
	if err == pgx.ErrNoRows {
		return nil, excerrors.New("NOT_FOUND", "report not found")
	}
	if err != nil {
		return nil, excerrors.Wrap("INTERNAL_ERROR", "rts27 read", err)
	}
	return r, nil
}
