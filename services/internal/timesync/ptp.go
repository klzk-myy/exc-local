// ptp.go — PTP (IEEE 1588v2) monitoring for bare-metal matching nodes
// (Phase-09 Task 9.3.12; MiFID II RTS 25 100µs bound; spec §19.13).
//
// Guard (guard.go) is the fail-closed L0 gate on adjtimex state; this
// file is the observability half: it reads linuxptp's ptp4l/phc2sys
// state, exports the mandated Prometheus metrics
// (clock_offset_nanoseconds, ptp_sync_status), detects staleness and
// unavailability, and archives a daily maximum-divergence report for
// the regulatory audit trail.
//
// Readers (production precedence order):
//
//	StatsFileReader — the deploy/ansible/roles/ptp helper writes a
//	key=value status file (/run/ptp/status) every cycle; cheap and
//	doesn't fork from the hot host. Also the test seam.
//	PMCReader       — `pmc -u -b 0 -s /run/ptp4l 'GET TIME_STATUS_NP'`
//	plus 'GET PORT_DATA_SET portIdentity=1'; parses linuxptp's key-value
//	management responses.
//
// Absence posture: a host with no PTP (dev boxes, K8s workers) never
// crashes — reader errors surface as ptp_available 0 + a read-error
// counter, and Monitor's alert callback fires the P1 the Prometheus
// rules mirror (deploy/prometheus/rules/exchange-alerts.yml).
package timesync

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// PTPBoundNs is the MiFID II RTS 25 divergence bound: 100µs = 100_000ns.
const PTPBoundNs = 100_000

// PTPReading is one sample of the host's PTP discipline state.
type PTPReading struct {
	OffsetNs int64     // master/system offset in nanoseconds (signed)
	Synced   bool      // servo locked AND grandmaster present
	State    string    // servo/port state label (SLAVE, s2, LISTENING…)
	Source   string    // "pmc" | "ptp-status-file"
	At       time.Time // when the reading was produced (staleness basis)
}

// PTPReader samples the host's PTP state once.
type PTPReader func(ctx context.Context) (PTPReading, error)

// FallbackReader tries readers in order and returns the first success;
// the combined error reports every attempt when none succeed.
func FallbackReader(readers ...PTPReader) PTPReader {
	return func(ctx context.Context) (PTPReading, error) {
		var errs []string
		for _, r := range readers {
			rd, err := r(ctx)
			if err == nil {
				return rd, nil
			}
			errs = append(errs, err.Error())
		}
		return PTPReading{}, fmt.Errorf("ptp: all readers failed: %s",
			strings.Join(errs, "; "))
	}
}

// ---------------------------------------------------------------------------
// StatsFileReader — key=value status file written by the ptp role's
// helper (deploy/ansible/roles/ptp/files/ptp-status.sh):
//
//	offset_ns=-37
//	state=SLAVE            (or s2, LISTENING — raw servo/port state)
//	synced=1
//	read_at=2026-09-20T12:00:00Z
//
// Missing fields default conservatively (unsynced). The reader stamps
// At from the file's mtime so a dead writer reads stale, not frozen.
// ---------------------------------------------------------------------------

// StatsFileReader reads the key=value status file at Path.
type StatsFileReader struct{ Path string }

// DefaultPTPStatusPath is where the deploy role writes the status file.
const DefaultPTPStatusPath = "/run/ptp/status"

// Read parses the status file; At is the file mtime.
func (r StatsFileReader) Read(context.Context) (PTPReading, error) {
	st, err := os.Stat(r.Path)
	if err != nil {
		return PTPReading{}, fmt.Errorf("ptp: status file: %w", err)
	}
	f, err := os.Open(r.Path)
	if err != nil {
		return PTPReading{}, fmt.Errorf("ptp: status file open: %w", err)
	}
	defer f.Close()

	rd := PTPReading{Source: "ptp-status-file", At: st.ModTime()}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		switch strings.TrimSpace(k) {
		case "offset_ns":
			if n, err := strconv.ParseInt(strings.TrimSpace(v), 10, 64); err == nil {
				rd.OffsetNs = n
			}
		case "state":
			rd.State = strings.TrimSpace(v)
		case "synced":
			rd.Synced = strings.TrimSpace(v) == "1" ||
				strings.EqualFold(strings.TrimSpace(v), "true")
		case "read_at":
			if ts, err := time.Parse(time.RFC3339, strings.TrimSpace(v)); err == nil {
				rd.At = ts // explicit writer timestamp beats mtime
			}
		}
	}
	if err := sc.Err(); err != nil {
		return PTPReading{}, fmt.Errorf("ptp: status file scan: %w", err)
	}
	return rd, nil
}

// ---------------------------------------------------------------------------
// PMCReader — pmc management client against the local ptp4l UDS socket.
// ---------------------------------------------------------------------------

// Runner executes a command and returns combined output.
type Runner func(ctx context.Context, name string, args ...string) ([]byte, error)

// ExecRunner is the production Runner (2s bound — pmc is local UDS).
func ExecRunner(ctx context.Context, name string, args ...string) ([]byte, error) {
	cctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	return exec.CommandContext(cctx, name, args...).CombinedOutput()
}

// PMCReader queries linuxptp via pmc(8). Socket is the ptp4l UDS path
// (/run/ptp4l default); Run injectable for tests.
type PMCReader struct {
	Socket string
	Run    Runner
	now    func() time.Time
}

// NewPMCReader builds the production reader on the default socket.
func NewPMCReader() PMCReader {
	return PMCReader{Socket: "/run/ptp4l", Run: ExecRunner, now: time.Now}
}

// Read issues GET TIME_STATUS_NP + GET PORT_DATA_SET and folds the
// management responses into a PTPReading.
func (r PMCReader) Read(ctx context.Context) (PTPReading, error) {
	run := r.Run
	if run == nil {
		run = ExecRunner
	}
	sock := r.Socket
	if sock == "" {
		sock = "/run/ptp4l"
	}
	now := r.now
	if now == nil {
		now = time.Now
	}
	ts, err := run(ctx, "pmc", "-u", "-b", "0", "-s", sock,
		"GET TIME_STATUS_NP")
	if err != nil {
		return PTPReading{}, fmt.Errorf("ptp: pmc TIME_STATUS_NP: %w", err)
	}
	pd, err := run(ctx, "pmc", "-u", "-b", "0", "-s", sock,
		"GET PORT_DATA_SET")
	if err != nil {
		return PTPReading{}, fmt.Errorf("ptp: pmc PORT_DATA_SET: %w", err)
	}
	return parsePMC(now(), string(ts)+"\n"+string(pd))
}

// parsePMC extracts master_offset / gmPresent / port_state from pmc's
// management response bodies — linuxptp renders each datum as
// "key<tabs/spaces>value" one per line.
func parsePMC(at time.Time, out string) (PTPReading, error) {
	rd := PTPReading{Source: "pmc", At: at}
	foundOffset := false
	gmPresent := false
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		switch fields[0] {
		case "master_offset":
			if n, err := strconv.ParseInt(fields[1], 10, 64); err == nil {
				rd.OffsetNs = n
				foundOffset = true
			}
		case "gmPresent":
			gmPresent = fields[1] == "true"
		case "port_state":
			rd.State = fields[1]
		case "servo_state":
			if rd.State == "" {
				rd.State = fields[1]
			}
		}
	}
	if !foundOffset {
		return PTPReading{}, fmt.Errorf("ptp: pmc response missing master_offset")
	}
	// Synced = grandmaster present and the port is slaved to it (SLAVE for
	// an ordinary clock; MASTER on a boundary host that is itself the GM
	// is also disciplined-by-definition).
	rd.Synced = gmPresent &&
		(rd.State == "SLAVE" || rd.State == "MASTER" ||
			strings.HasPrefix(rd.State, "s2"))
	return rd, nil
}
