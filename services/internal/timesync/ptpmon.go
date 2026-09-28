// ptpmon.go — the sampling loop that turns PTPReader output into the
// mandated metrics, staleness detection, P1 alerts and the daily
// divergence archive (Phase-09 Task 9.3.12, spec §19.13, MiFID II RTS 25).
//
// Metric surface (exact names per task text):
//
//	clock_offset_nanoseconds                 gauge  signed offset, last sample
//	ptp_sync_status                          gauge  1=disciplined/slaved
//	ptp_available                            gauge  1=a reader produced data
//	ptp_last_read_age_seconds                gauge  staleness detector input
//	ptp_read_errors_total                    counter
//	clock_divergence_daily_max_nanoseconds   gauge  UTC-day max |offset|
//	ptp_violations_total{kind}               counter P1 alert breadcrumbs
//
// Alert contract: on every sample the monitor evaluates
//   - |offset| > 100_000ns       → P1 "clock_offset_exceeded"
//   - reading stale (>StaleAfter) → P1 "ptp_stale"
//   - Expected && !available     → P1 "ptp_unavailable" (the
//     aeron_driver_up-shaped host-absence alert)
//   - available && !Synced       → P1 "ptp_not_synced"
//
// Mirrored as Prometheus rules in
// deploy/prometheus/rules/exchange-alerts.yml so paging doesn't depend
// on the in-process callback being wired.
package timesync

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sync"
	"time"

	"exchange/internal/observability"
)

// PTPAlertKind enumerates the P1 conditions the monitor raises.
const (
	AlertOffsetExceeded = "clock_offset_exceeded"
	AlertStale          = "ptp_stale"
	AlertUnavailable    = "ptp_unavailable"
	AlertNotSynced      = "ptp_not_synced"
)

// PTPAlert is one evaluated violation, delivered to OnAlert.
type PTPAlert struct {
	Kind   string    `json:"kind"`
	Detail string    `json:"detail"`
	At     time.Time `json:"at"`
}

// PTPMonitor samples a PTPReader and owns the metric state.
type PTPMonitor struct {
	// Reader produces one sample per tick (default: PMCReader on
	// /run/ptp4l, falling back to the /run/ptp/status file).
	Reader PTPReader
	// Expected marks hosts that MUST run PTP (bare-metal matching
	// nodes). When false, unavailability still flips ptp_available but
	// raises no alert (dev boxes don't page).
	Expected bool
	// Interval between samples (default 5s — the P1 response must be
	// sub-scrape-interval, not sub-second).
	Interval time.Duration
	// StaleAfter bounds reading age (default 30s).
	StaleAfter time.Duration
	// BoundNs is the divergence bound (default PTPBoundNs = 100µs).
	BoundNs int64
	// DailyReportPath, when set, receives one JSONL record per UTC day
	// boundary carrying the day's max |offset| — the regulatory audit
	// artifact (log-shipped to the archive bucket).
	DailyReportPath string
	// OnAlert is invoked synchronously per violation per sample —
	// keep it cheap (the Prometheus rules are the paging surface; this
	// is the in-process breadcrumb / watchdog input).
	OnAlert func(PTPAlert)
	// Now overrides time.Now (tests).
	Now func() time.Time

	offset     *observability.GaugeVec
	synced     *observability.GaugeVec
	avail      *observability.GaugeVec
	readAge    *observability.GaugeVec
	readErr    *observability.CounterVec
	dayMax     *observability.GaugeVec
	violations *observability.CounterVec

	mu         sync.Mutex
	day        string // UTC YYYY-MM-DD the accumulator belongs to
	dayMaxNs   int64
	daySamples int64     // good readings this UTC day
	dayErrs    int64     // read failures this UTC day
	lastAt     time.Time // last successful reading's At
	hadSample  bool
}

// NewPTPMonitor wires the monitor onto the registry. reader nil selects
// the production fallback chain (status file → pmc).
func NewPTPMonitor(reg *observability.Registry, reader PTPReader, expected bool) *PTPMonitor {
	if reader == nil {
		reader = FallbackReader(
			StatsFileReader{Path: DefaultPTPStatusPath}.Read,
			NewPMCReader().Read,
		)
	}
	m := &PTPMonitor{
		Reader:     reader,
		Expected:   expected,
		Interval:   5 * time.Second,
		StaleAfter: 30 * time.Second,
		BoundNs:    PTPBoundNs,
		Now:        time.Now,
		offset: reg.Gauge("clock_offset_nanoseconds",
			"PTP/phc2sys clock offset vs grandmaster (signed ns); RTS 25 bound ±100000."),
		synced: reg.Gauge("ptp_sync_status",
			"1 when ptp4l/phc2sys report a locked servo with a present grandmaster."),
		avail: reg.Gauge("ptp_available",
			"1 when a PTP state source produced a reading on the last sample."),
		readAge: reg.Gauge("ptp_last_read_age_seconds",
			"Age of the most recent PTP reading (staleness detector)."),
		readErr: reg.Counter("ptp_read_errors_total",
			"PTP state source read failures."),
		dayMax: reg.Gauge("clock_divergence_daily_max_nanoseconds",
			"Maximum |clock_offset| observed in the current UTC day (RTS 25 audit)."),
		violations: reg.Counter("ptp_violations_total",
			"P1 PTP violations evaluated, by kind."),
	}
	return m
}

// now is the injectable clock.
func (m *PTPMonitor) now() time.Time {
	if m.Now != nil {
		return m.Now()
	}
	return time.Now()
}

// SampleOnce reads the PTP source, updates every metric and evaluates
// the alert set. Returns the read error (availability bookkeeping has
// already run — callers may log and continue).
func (m *PTPMonitor) SampleOnce(ctx context.Context) error {
	now := m.now()
	rd, err := m.Reader(ctx)

	m.mu.Lock()
	if err != nil {
		m.rollDayLocked(now) // error days still roll the report boundary
		m.dayErrs++
		m.readErr.With().Inc()
		m.avail.With().Set(0)
		m.synced.With().Set(0)
	} else {
		m.avail.With().Set(1)
		m.offset.With().Set(float64(rd.OffsetNs))
		synced := 0.0
		if rd.Synced {
			synced = 1
		}
		m.synced.With().Set(synced)
		m.lastAt = rd.At
		m.accumulate(rd)
	}
	age := -1.0
	if m.hadSample || !m.lastAt.IsZero() {
		m.hadSample = true
		age = now.Sub(m.lastAt).Seconds()
		if age < 0 {
			age = 0
		}
	}
	m.readAge.With().Set(age)
	stale := age < 0 || age > m.StaleAfter.Seconds()
	m.mu.Unlock()

	m.evaluate(now, rd, err, age, stale)
	return err
}

// rollDayLocked closes the current UTC day's accumulator — writing the
// daily report — and opens the new day. Caller holds m.mu.
func (m *PTPMonitor) rollDayLocked(at time.Time) {
	day := at.UTC().Format("2006-01-02")
	if m.day == day {
		return
	}
	if m.day != "" {
		m.writeDailyReportLocked()
	}
	m.day = day
	m.dayMaxNs = 0
	m.daySamples = 0
	m.dayErrs = 0
}

// accumulate folds a good reading into the UTC-day max and rolls the
// daily report file at the boundary.
func (m *PTPMonitor) accumulate(rd PTPReading) {
	// caller holds m.mu
	m.rollDayLocked(rd.At)
	m.daySamples++
	abs := rd.OffsetNs
	if abs < 0 {
		abs = -abs
	}
	if abs > m.dayMaxNs {
		m.dayMaxNs = abs
	}
	m.dayMax.With().Set(float64(m.dayMaxNs))
}

// DailyReport is one archived UTC-day line — the RTS 25 audit artifact.
type DailyReport struct {
	Date            string `json:"date"`
	MaxOffsetNs     int64  `json:"max_offset_ns"`
	BoundNs         int64  `json:"bound_ns"`
	Samples         int64  `json:"samples"`
	ReadErrors      int64  `json:"read_errors"`
	ExceededBound   bool   `json:"exceeded_bound"`
	HostExpectedPTP bool   `json:"host_expected_ptp"`
	ClosedAt        string `json:"closed_at"` // UTC
}

// writeDailyReportLocked appends the closing day's record. Best-effort:
// a write failure must not crash the monitor (metric state already holds
// the value); callers see it through the read-error counter.
func (m *PTPMonitor) writeDailyReportLocked() {
	if m.DailyReportPath == "" {
		return
	}
	rec := DailyReport{
		Date:            m.day,
		MaxOffsetNs:     m.dayMaxNs,
		BoundNs:         m.BoundNs,
		Samples:         m.daySamples,
		ReadErrors:      m.dayErrs,
		ExceededBound:   m.dayMaxNs > m.BoundNs,
		HostExpectedPTP: m.Expected,
		ClosedAt:        m.now().UTC().Format(time.RFC3339),
	}
	b, err := json.Marshal(rec)
	if err != nil {
		return
	}
	f, err := os.OpenFile(m.DailyReportPath,
		os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o640)
	if err != nil {
		return
	}
	defer f.Close()
	_, _ = f.Write(append(b, '\n'))
}

// evaluate runs the P1 alert set over the fresh sample state.
func (m *PTPMonitor) evaluate(now time.Time, rd PTPReading, readErr error, age float64, stale bool) {
	fire := func(kind, detail string) {
		m.violations.With("kind", kind).Inc()
		if m.OnAlert != nil {
			m.OnAlert(PTPAlert{Kind: kind, Detail: detail, At: now})
		}
	}
	switch {
	case readErr != nil && m.Expected:
		fire(AlertUnavailable,
			fmt.Sprintf("no PTP source on PTP-expected host: %v", readErr))
	case readErr != nil:
		// unexpected host: availability metric only — no page.
	case stale:
		fire(AlertStale,
			fmt.Sprintf("last PTP reading %.0fs old (>%.0fs)", age, m.StaleAfter.Seconds()))
	case !rd.Synced:
		fire(AlertNotSynced,
			fmt.Sprintf("ptp4l not slaved (state %q)", rd.State))
	case rd.OffsetNs > m.BoundNs || rd.OffsetNs < -m.BoundNs:
		fire(AlertOffsetExceeded,
			fmt.Sprintf("|offset| %dns exceeds %dns (RTS 25 100µs)",
				rd.OffsetNs, m.BoundNs))
	}
}

// Run samples until ctx is cancelled; the first sample is immediate so a
// broken host cannot hide for a whole interval.
func (m *PTPMonitor) Run(ctx context.Context) {
	_ = m.SampleOnce(ctx)
	t := time.NewTicker(m.Interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			_ = m.SampleOnce(ctx)
		}
	}
}
