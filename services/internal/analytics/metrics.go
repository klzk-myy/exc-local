package analytics

import (
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

// IngestMetrics is the analytics ingest panel (spec §24 #68 — the
// dashboard feed for latency, throughput, fill rate and P&L projection
// lag). Same convention as internal/bridge/metrics.go: no Prometheus
// client dependency — atomic counters plus a hand-rolled v0.0.4 text
// exposition a standard scrape config can consume.
type IngestMetrics struct {
	rowsInserted  atomic.Uint64 // CH-inserted rows (all tables)
	batchInserted atomic.Uint64 // successful PrepareBatch sends
	insertErrors  atomic.Uint64 // failed CH insert attempts (pre-spool)
	insertNanos   atomic.Uint64 // sum of insert wall time
	insertCalls   atomic.Uint64 // insert wall-time observation count

	spooledBatches atomic.Uint64 // batches diverted to the disk spool
	spooledRows    atomic.Uint64
	spoolEntries   atomic.Int64  // live spool depth (gauge)
	spoolBytes     atomic.Int64  // live spool disk usage (gauge)
	spoolDropped   atomic.Uint64 // oldest-first evictions past MaxBytes (§16.6 F15)

	drainedBatches atomic.Uint64 // spool batches re-inserted on recovery
	drainedRows    atomic.Uint64
	recoveries     atomic.Uint64 // down->up Ping transitions

	msgsFetched   atomic.Uint64 // JetStream messages pulled
	msgsDecoded   atomic.Uint64 // flatbuffers decodes that produced rows
	msgsMalformed atomic.Uint64 // undecodable payloads (counted, Nak'd? no: acked-and-counted)
	msgsAcked     atomic.Uint64
	msgsNakd      atomic.Uint64

	ordersSubmitted atomic.Uint64 // OrderNew seen on `analytics` stream
	fills           atomic.Uint64 // TradeFill rows ingested (fill-rate feed)

	incomeRows atomic.Uint64 // income_ledger rows projected
	incomeLag  atomic.Int64  // nanoseconds: now - newest projected posted_at

	lastInsertUnix atomic.Int64 // last successful insert wall time (unix nano)
}

// NewIngestMetrics returns a zeroed panel.
func NewIngestMetrics() *IngestMetrics { return &IngestMetrics{} }

func (m *IngestMetrics) incRowsInserted(n int) { m.rowsInserted.Add(uint64(n)) }
func (m *IngestMetrics) incBatchInserted()     { m.batchInserted.Add(1) }
func (m *IngestMetrics) incInsertErrors()      { m.insertErrors.Add(1) }
func (m *IngestMetrics) incSpooled(n int)      { m.spooledBatches.Add(1); m.spooledRows.Add(uint64(n)) }
func (m *IngestMetrics) incDrained(batches, rows int) {
	m.drainedBatches.Add(uint64(batches))
	m.drainedRows.Add(uint64(rows))
}
func (m *IngestMetrics) incRecoveries()        { m.recoveries.Add(1) }
func (m *IngestMetrics) incMsgsFetched(n int)  { m.msgsFetched.Add(uint64(n)) }
func (m *IngestMetrics) incDecoded(n int)      { m.msgsDecoded.Add(uint64(n)) }
func (m *IngestMetrics) incMalformed()         { m.msgsMalformed.Add(1) }
func (m *IngestMetrics) incAcked(n int)        { m.msgsAcked.Add(uint64(n)) }
func (m *IngestMetrics) incNakd(n int)         { m.msgsNakd.Add(uint64(n)) }
func (m *IngestMetrics) incOrders()            { m.ordersSubmitted.Add(1) }
func (m *IngestMetrics) incFills(n int)        { m.fills.Add(uint64(n)) }
func (m *IngestMetrics) incIncomeRows(n int)   { m.incomeRows.Add(uint64(n)) }
func (m *IngestMetrics) incSpoolDropped(n int) { m.spoolDropped.Add(uint64(n)) }

func (m *IngestMetrics) observeInsertNanos(d time.Duration) {
	m.insertNanos.Add(uint64(d))
	m.insertCalls.Add(1)
	m.lastInsertUnix.Store(time.Now().UnixNano())
}

// SetSpoolGauges mirrors the live spool depth/bytes onto the gauges
// (Prometheus `analytics_spool_size_bytes`, alert threshold 80GB per
// §16.6 F15).
func (m *IngestMetrics) SetSpoolGauges(entries, bytes int64) {
	m.spoolEntries.Store(entries)
	m.spoolBytes.Store(bytes)
}

// SetIncomeLag records the projection lag (now − newest posted_at).
func (m *IngestMetrics) SetIncomeLag(d time.Duration) { m.incomeLag.Store(int64(d)) }

// Snapshot is a point-in-time copy for tests and health payloads.
type IngestSnapshot struct {
	RowsInserted    uint64 `json:"rows_inserted_total"`
	BatchInserted   uint64 `json:"batches_inserted_total"`
	InsertErrors    uint64 `json:"insert_errors_total"`
	SpooledBatches  uint64 `json:"spooled_batches_total"`
	SpooledRows     uint64 `json:"spooled_rows_total"`
	SpoolEntries    int64  `json:"spool_entries"`
	SpoolBytes      int64  `json:"spool_bytes"`
	SpoolDropped    uint64 `json:"spool_dropped_total"`
	DrainedBatches  uint64 `json:"drained_batches_total"`
	DrainedRows     uint64 `json:"drained_rows_total"`
	Recoveries      uint64 `json:"recoveries_total"`
	MsgsFetched     uint64 `json:"msgs_fetched_total"`
	MsgsDecoded     uint64 `json:"msgs_decoded_total"`
	MsgsMalformed   uint64 `json:"msgs_malformed_total"`
	MsgsAcked       uint64 `json:"msgs_acked_total"`
	MsgsNakd        uint64 `json:"msgs_nakd_total"`
	OrdersSubmitted uint64 `json:"orders_submitted_total"`
	Fills           uint64 `json:"fills_total"`
	IncomeRows      uint64 `json:"income_rows_total"`
	IncomeLagNanos  int64  `json:"income_lag_nanos"`
}

// Snapshot copies the counters.
func (m *IngestMetrics) Snapshot() IngestSnapshot {
	return IngestSnapshot{
		RowsInserted:    m.rowsInserted.Load(),
		BatchInserted:   m.batchInserted.Load(),
		InsertErrors:    m.insertErrors.Load(),
		SpooledBatches:  m.spooledBatches.Load(),
		SpooledRows:     m.spooledRows.Load(),
		SpoolEntries:    m.spoolEntries.Load(),
		SpoolBytes:      m.spoolBytes.Load(),
		SpoolDropped:    m.spoolDropped.Load(),
		DrainedBatches:  m.drainedBatches.Load(),
		DrainedRows:     m.drainedRows.Load(),
		Recoveries:      m.recoveries.Load(),
		MsgsFetched:     m.msgsFetched.Load(),
		MsgsDecoded:     m.msgsDecoded.Load(),
		MsgsMalformed:   m.msgsMalformed.Load(),
		MsgsAcked:       m.msgsAcked.Load(),
		MsgsNakd:        m.msgsNakd.Load(),
		OrdersSubmitted: m.ordersSubmitted.Load(),
		Fills:           m.fills.Load(),
		IncomeRows:      m.incomeRows.Load(),
		IncomeLagNanos:  m.incomeLag.Load(),
	}
}

// exposition series; emission order is deterministic.
type ingestMetricDef struct {
	name  string
	help  string
	typ   string // counter|gauge
	value func() float64
}

func (m *IngestMetrics) defs() []ingestMetricDef {
	return []ingestMetricDef{
		{"analytics_ch_rows_inserted_total", "Rows committed to ClickHouse (all tables).", "counter",
			func() float64 { return float64(m.rowsInserted.Load()) }},
		{"analytics_ch_batches_inserted_total", "Successful PrepareBatch sends.", "counter",
			func() float64 { return float64(m.batchInserted.Load()) }},
		{"analytics_ch_insert_errors_total", "Failed ClickHouse inserts (rows were diverted to the spool).", "counter",
			func() float64 { return float64(m.insertErrors.Load()) }},
		{"analytics_ch_insert_latency_seconds_sum", "Insert wall-time sum (divide by _count for mean).", "counter",
			func() float64 { return float64(m.insertNanos.Load()) / 1e9 }},
		{"analytics_ch_insert_latency_seconds_count", "Insert wall-time observation count.", "counter",
			func() float64 { return float64(m.insertCalls.Load()) }},
		{"analytics_spooled_batches_total", "Insert batches diverted to the local disk spool.", "counter",
			func() float64 { return float64(m.spooledBatches.Load()) }},
		{"analytics_spooled_rows_total", "Rows diverted to the local disk spool.", "counter",
			func() float64 { return float64(m.spooledRows.Load()) }},
		{"analytics_spool_entries", "Pending spool entries awaiting drain.", "gauge",
			func() float64 { return float64(m.spoolEntries.Load()) }},
		{"analytics_spool_size_bytes", "On-disk spool size; alert at 80GB (§16.6 F15).", "gauge",
			func() float64 { return float64(m.spoolBytes.Load()) }},
		{"analytics_spool_dropped_total", "Oldest-first spool evictions past the 100GB bound.", "counter",
			func() float64 { return float64(m.spoolDropped.Load()) }},
		{"analytics_drained_batches_total", "Spool batches re-inserted after ClickHouse recovery.", "counter",
			func() float64 { return float64(m.drainedBatches.Load()) }},
		{"analytics_drained_rows_total", "Spool rows re-inserted after ClickHouse recovery.", "counter",
			func() float64 { return float64(m.drainedRows.Load()) }},
		{"analytics_ch_recoveries_total", "ClickHouse down->up Ping transitions observed by Recover.", "counter",
			func() float64 { return float64(m.recoveries.Load()) }},
		{"analytics_nats_msgs_fetched_total", "JetStream messages pulled by the ETL consumers.", "counter",
			func() float64 { return float64(m.msgsFetched.Load()) }},
		{"analytics_nats_msgs_decoded_total", "Messages decoded into insertable rows.", "counter",
			func() float64 { return float64(m.msgsDecoded.Load()) }},
		{"analytics_nats_msgs_malformed_total", "Undecodable wire payloads.", "counter",
			func() float64 { return float64(m.msgsMalformed.Load()) }},
		{"analytics_nats_msgs_acked_total", "JetStream acks after durable landing (CH or spool).", "counter",
			func() float64 { return float64(m.msgsAcked.Load()) }},
		{"analytics_nats_msgs_nakd_total", "JetStream naks (nothing durable — redelivery expected).", "counter",
			func() float64 { return float64(m.msgsNakd.Load()) }},
		{"analytics_orders_submitted_total", "OrderNew events seen on the analytics stream (fill-rate denominator).", "counter",
			func() float64 { return float64(m.ordersSubmitted.Load()) }},
		{"analytics_fills_total", "TradeFill rows ingested (fill-rate numerator).", "counter",
			func() float64 { return float64(m.fills.Load()) }},
		{"analytics_income_rows_total", "income_ledger rows projected from the PG ledger.", "counter",
			func() float64 { return float64(m.incomeRows.Load()) }},
		{"analytics_income_projection_lag_seconds", "Lag between wall clock and newest projected ledger entry (P&L projection lag).", "gauge",
			func() float64 { return float64(m.incomeLag.Load()) / 1e9 }},
		{"analytics_last_insert_unix_seconds", "Wall time of the last successful ClickHouse insert.", "gauge",
			func() float64 { return float64(m.lastInsertUnix.Load()) / 1e9 }},
	}
}

// writeText renders the Prometheus v0.0.4 exposition body.
func (m *IngestMetrics) writeText(w io.Writer) {
	for _, d := range m.defs() {
		fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s %s\n%s %s\n",
			d.name, d.help, d.name, d.typ, d.name,
			strconv.FormatFloat(d.value(), 'g', -1, 64))
	}
}

// String renders the exposition body — used by unit tests without an HTTP
// round-trip.
func (m *IngestMetrics) String() string {
	var sb strings.Builder
	m.writeText(&sb)
	return sb.String()
}

// Handler serves GET /metrics in Prometheus text exposition format.
func (m *IngestMetrics) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/metrics" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		m.writeText(w)
	})
}
