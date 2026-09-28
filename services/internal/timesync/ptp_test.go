// Coverage for Task 9.3.12 — pmc parsing, status-file reading, staleness,
// availability-on-absent-PTP, bound violation, daily divergence archive.
package timesync

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"exchange/internal/observability"
)

// --- pmc parsing ----------------------------------------------------------

const pmcTimeStatus = `	sending: GET TIME_STATUS_NP
	80f402.fffe.123456-1 seq 0 RESPONSE MANAGEMENT TIME_STATUS_NP
		master_offset              41
		ingress_time               1758432000000000041
		cumulativeScaledRateOffset +1.000000000
		scaledLastGmPhaseChange    0
		gmTimeBaseIndicator        0
		lastGmPhaseChange          0x0000'0000000000000000
		gmPresent                  true
		gmIdentity                 80f402.fffe.123456
`

const pmcPortData = `	sending: GET PORT_DATA_SET
	80f402.fffe.123456-1 seq 0 RESPONSE MANAGEMENT PORT_DATA_SET
		portIdentity               000c29.fffe.4d9cb1-1
		port_state                 SLAVE
		logMinDelayReqInterval     0
	`

func TestPMCReaderParsesLockedSlave(t *testing.T) {
	r := PMCReader{Run: func(ctx context.Context, name string, args ...string) ([]byte, error) {
		for _, a := range args {
			if strings.Contains(a, "TIME_STATUS_NP") {
				return []byte(pmcTimeStatus), nil
			}
			if strings.Contains(a, "PORT_DATA_SET") {
				return []byte(pmcPortData), nil
			}
		}
		return nil, nil
	}}
	rd, err := r.Read(context.Background())
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if rd.OffsetNs != 41 || !rd.Synced || rd.State != "SLAVE" || rd.Source != "pmc" {
		t.Fatalf("bad reading: %+v", rd)
	}
}

func TestPMCReaderGMMissingNotSynced(t *testing.T) {
	out := strings.Replace(pmcTimeStatus, "gmPresent                  true",
		"gmPresent                  false", 1)
	r := PMCReader{Run: func(context.Context, string, ...string) ([]byte, error) {
		return []byte(out + pmcPortData), nil
	}}
	rd, err := r.Read(context.Background())
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if rd.Synced {
		t.Fatal("gmPresent=false must not report synced")
	}
}

func TestPMCReaderExecFailure(t *testing.T) {
	r := PMCReader{Run: func(context.Context, string, ...string) ([]byte, error) {
		return nil, os.ErrNotExist // pmc binary absent
	}}
	if _, err := r.Read(context.Background()); err == nil {
		t.Fatal("absent pmc must error (monitor maps it to availability=0)")
	}
}

// --- status file ----------------------------------------------------------

func TestStatsFileReader(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "status")
	ts := time.Now().UTC().Format(time.RFC3339)
	if err := os.WriteFile(path, []byte(
		"# ptp4l/phc2sys status\noffset_ns=-37\nstate=SLAVE\nsynced=1\nread_at="+ts+"\n"),
		0o640); err != nil {
		t.Fatal(err)
	}
	rd, err := (StatsFileReader{Path: path}).Read(context.Background())
	if err != nil || rd.OffsetNs != -37 || !rd.Synced || rd.State != "SLAVE" {
		t.Fatalf("bad file reading: %+v %v", rd, err)
	}
	if rd.At.Format(time.RFC3339) != ts {
		t.Fatalf("read_at must win over mtime: %v", rd.At)
	}
	// Missing file → error, never panic.
	if _, err := (StatsFileReader{Path: filepath.Join(dir, "nope")}).
		Read(context.Background()); err == nil {
		t.Fatal("missing status file must error")
	}
}

// --- monitor --------------------------------------------------------------

type fakeReader struct {
	rd  PTPReading
	err error
	n   int
}

func (f *fakeReader) Read(context.Context) (PTPReading, error) {
	f.n++
	return f.rd, f.err
}

func newTestMonitor(t *testing.T, expected bool) (*PTPMonitor, *observability.Registry, *fakeReader, *[]PTPAlert) {
	t.Helper()
	reg := observability.New()
	fr := &fakeReader{rd: PTPReading{
		OffsetNs: 42, Synced: true, State: "SLAVE",
		Source: "test", At: time.Now()}}
	var alerts []PTPAlert
	m := NewPTPMonitor(reg, fr.Read, expected)
	m.OnAlert = func(a PTPAlert) { alerts = append(alerts, a) }
	return m, reg, fr, &alerts
}

func TestMonitorHealthySample(t *testing.T) {
	m, reg, _, alerts := newTestMonitor(t, true)
	if err := m.SampleOnce(context.Background()); err != nil {
		t.Fatalf("sample: %v", err)
	}
	out := reg.String()
	for _, want := range []string{
		"clock_offset_nanoseconds 42",
		"ptp_sync_status 1",
		"ptp_available 1",
		"ptp_last_read_age_seconds",
		"clock_divergence_daily_max_nanoseconds 42",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("metrics missing %q in:\n%s", want, out)
		}
	}
	if len(*alerts) != 0 {
		t.Fatalf("healthy sample must not alert: %+v", *alerts)
	}
}

func TestMonitorUnavailableOnExpectedHost(t *testing.T) {
	m, reg, fr, alerts := newTestMonitor(t, true)
	fr.err = os.ErrNotExist // no ptp4l, no status file
	if err := m.SampleOnce(context.Background()); err == nil {
		t.Fatal("read error must propagate")
	}
	out := reg.String()
	if !strings.Contains(out, "ptp_available 0") ||
		!strings.Contains(out, "ptp_read_errors_total 1") {
		t.Fatalf("absent PTP must surface as available=0 + error counter:\n%s", out)
	}
	if len(*alerts) != 1 || (*alerts)[0].Kind != AlertUnavailable {
		t.Fatalf("expected P1 ptp_unavailable: %+v", *alerts)
	}
	// Host not expected → metric still flips, no page.
	m2, _, fr2, alerts2 := newTestMonitor(t, false)
	fr2.err = os.ErrNotExist
	_ = m2.SampleOnce(context.Background())
	if len(*alerts2) != 0 {
		t.Fatalf("non-expected host must not page: %+v", *alerts2)
	}
	_ = fr2
}

func TestMonitorStaleness(t *testing.T) {
	reg := observability.New()
	old := time.Now().Add(-2 * time.Minute)
	fr := &fakeReader{rd: PTPReading{
		OffsetNs: 10, Synced: true, State: "SLAVE", Source: "t", At: old}}
	var alerts []PTPAlert
	m := NewPTPMonitor(reg, fr.Read, true)
	m.StaleAfter = 30 * time.Second
	m.OnAlert = func(a PTPAlert) { alerts = append(alerts, a) }
	_ = m.SampleOnce(context.Background())
	if len(alerts) == 0 || alerts[0].Kind != AlertStale {
		t.Fatalf("stale reading must raise P1: %+v", alerts)
	}
}

func TestMonitorBoundViolation(t *testing.T) {
	m, _, fr, alerts := newTestMonitor(t, true)
	fr.rd.OffsetNs = 150_000 // 150µs > 100µs RTS-25 bound
	_ = m.SampleOnce(context.Background())
	if len(*alerts) != 1 || (*alerts)[0].Kind != AlertOffsetExceeded {
		t.Fatalf("over-bound offset must raise clock_offset_exceeded: %+v", *alerts)
	}
}

func TestMonitorNotSynced(t *testing.T) {
	m, _, fr, alerts := newTestMonitor(t, true)
	fr.rd.Synced = false
	fr.rd.State = "LISTENING"
	_ = m.SampleOnce(context.Background())
	if len(*alerts) != 1 || (*alerts)[0].Kind != AlertNotSynced {
		t.Fatalf("unslaved port must raise ptp_not_synced: %+v", *alerts)
	}
}

func TestDailyDivergenceReport(t *testing.T) {
	dir := t.TempDir()
	rep := filepath.Join(dir, "clock-divergence.jsonl")
	reg := observability.New()
	now := time.Date(2026, 9, 20, 23, 0, 0, 0, time.UTC)
	fr := &fakeReader{}
	m := NewPTPMonitor(reg, fr.Read, true)
	m.DailyReportPath = rep
	m.Now = func() time.Time { return now }

	// Day 1: worst |offset| = 80µs.
	fr.rd = PTPReading{OffsetNs: -80_000, Synced: true, State: "SLAVE", At: now}
	_ = m.SampleOnce(context.Background())
	fr.rd.OffsetNs = 10_000
	_ = m.SampleOnce(context.Background())

	// Roll into day 2: report for day 1 must be appended.
	now = now.Add(2 * time.Hour)
	fr.rd = PTPReading{OffsetNs: 5_000, Synced: true, State: "SLAVE", At: now}
	_ = m.SampleOnce(context.Background())

	b, err := os.ReadFile(rep)
	if err != nil {
		t.Fatalf("daily report: %v", err)
	}
	var rec DailyReport
	if err := json.Unmarshal([]byte(strings.TrimSpace(string(b))), &rec); err != nil {
		t.Fatalf("report line not JSON: %v", err)
	}
	if rec.Date != "2026-09-20" || rec.MaxOffsetNs != 80_000 ||
		rec.Samples != 2 || !rec.HostExpectedPTP {
		t.Fatalf("bad daily report: %+v", rec)
	}
	// Gauge now tracks day 2's max.
	if !strings.Contains(reg.String(),
		"clock_divergence_daily_max_nanoseconds 5000") {
		t.Fatalf("day rollover must reset the gauge:\n%s", reg.String())
	}
}
