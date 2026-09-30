// Phase-21 Task 21.3.1 — scheduled vendor-list refresh with delta
// ingestion. The dev fixture directory proves the file-backed path;
// production pulls the official OFAC/EU/UN/UK-HMT (+ PEP vendor) lists
// on a daily cadence through the Fetcher seam, stages each new body
// under <dir>/.staging, validates it parses to ≥1 usable entry, swaps
// it atomically into the managed directory, then Reloads the screener —
// Reload computes the entry-set delta which drives Task 21.3.11's
// rescreen hook.
//
// Delta semantics: a body whose SHA-256 matches the last-applied
// content is skipped entirely (no rewrite, no reload). A body that
// parses to zero usable entries is REJECTED — last good copy retained,
// P1 alert raised. A fetch/transport failure likewise retains last
// good. The screener therefore never loads a truncated vendor file:
// zero usable entries stays an error end-to-end (spec §2.7).
//
// Feed outcomes are reported into the ProviderGate (Task 21.3.23) so a
// vendor outage feeds the same scoped-degradation latch as live
// screening provider failures.
package compliance

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// RefreshInterval is the task-pinned "lists updated daily" cadence.
const RefreshInterval = 24 * time.Hour

// ListFeed is one upstream list source — the URL is fetcher-interpreted
// (https:// → HTTPFetcher, file:///path or bare path → FileFetcher);
// Filename is the name written into the managed list directory, which
// drives classifyFile's format/kind/provenance detection. A Required
// feed's failure raises P1; optional feeds (e.g. a supplemental PEP
// vendor) warn only.
type ListFeed struct {
	Name     string `json:"name"`               // operator label ("ofac-sdn")
	URL      string `json:"url"`                // fetch source
	Filename string `json:"filename"`           // managed-dir file name
	Required bool   `json:"required,omitempty"` // failure → P1 + gate report
}

// Fetcher is the transport seam — unit tests inject fixtures, wiring
// binds HTTPFetcher (bounded body) or FileFetcher for mounted vendor
// drops.
type Fetcher interface {
	Fetch(ctx context.Context, url string) ([]byte, error)
}

// HTTPFetcher fetches over HTTP with a hard response cap (vendor lists
// are a few hundred MB max; 512MB is the safety ceiling).
type HTTPFetcher struct {
	Client   *http.Client
	MaxBytes int64
}

// Fetch GETs the URL with the client timeout, requiring 200 and
// bounding the body at MaxBytes (default 512MB).
func (h *HTTPFetcher) Fetch(ctx context.Context, url string) ([]byte, error) {
	c := h.Client
	if c == nil {
		c = &http.Client{Timeout: ProviderTimeout} // 30s — same ceiling as
		// the provider gate so a hanging vendor feed trips quarantine
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("fetch %s: HTTP %d", url, resp.StatusCode)
	}
	max := h.MaxBytes
	if max <= 0 {
		max = 512 << 20
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, max+1))
	if err != nil {
		return nil, err
	}
	if int64(len(body)) > max {
		return nil, fmt.Errorf("fetch %s: body exceeds %d-byte cap", url, max)
	}
	return body, nil
}

// FileFetcher reads a local path — mounted vendor drops and tests.
// "file://" prefixes are accepted.
type FileFetcher struct{}

// Fetch reads the (file://-stripped) path.
func (FileFetcher) Fetch(_ context.Context, url string) ([]byte, error) {
	path := strings.TrimPrefix(url, "file://")
	return os.ReadFile(path)
}

// FeedResult records one feed's refresh outcome for the report + audit.
type FeedResult struct {
	Name     string `json:"name"`
	Filename string `json:"filename"`
	SHA256   string `json:"sha256,omitempty"`
	Changed  bool   `json:"changed"`
	Entries  int    `json:"entries,omitempty"`
	Error    string `json:"error,omitempty"`
}

// RefreshReport summarizes one refresh pass — audited wholesale.
type RefreshReport struct {
	StartedAt    time.Time    `json:"started_at"`
	FinishedAt   time.Time    `json:"finished_at"`
	Feeds        []FeedResult `json:"feeds"`
	Reloaded     bool         `json:"reloaded"`
	DeltaAdded   int          `json:"delta_added"`
	DeltaRemoved int          `json:"delta_removed"`
}

// VendorRefresher owns the scheduled pull. It is deliberately the ONLY
// writer of managed-dir list bodies — operator file drops still work
// (Reload reads the whole directory) but the provenance hashes it
// reports reflect the managed files.
type VendorRefresher struct {
	screener *ListScreener
	dir      string
	feeds    []ListFeed
	fetch    Fetcher
	gate     *ProviderGate
	alerter  Alerter
	auditor  AuditSink
	interval time.Duration
	now      func() time.Time

	mu         sync.Mutex
	hashes     map[string]string // feed name → last applied sha256
	lastRun    time.Time
	lastOK     time.Time
	lastReport RefreshReport
}

// NewVendorRefresher binds the managed screener + feed inventory. dir
// must exist (the screener was constructed over it). A nil fetcher
// selects HTTPFetcher defaults.
func NewVendorRefresher(screener *ListScreener, dir string,
	feeds []ListFeed, fetch Fetcher) (*VendorRefresher, error) {
	if screener == nil {
		return nil, fmt.Errorf("sanctions refresh: screener required")
	}
	if strings.TrimSpace(dir) == "" {
		return nil, fmt.Errorf("sanctions refresh: managed dir required")
	}
	if fetch == nil {
		fetch = &HTTPFetcher{}
	}
	v := &VendorRefresher{
		screener: screener, dir: dir, feeds: feeds, fetch: fetch,
		interval: RefreshInterval, hashes: map[string]string{},
		now: func() time.Time { return time.Now().UTC() },
	}
	return v, nil
}

// WithGate/WithAlerter/WithAuditor/WithInterval/WithClock — wiring seams.
func (v *VendorRefresher) WithGate(g *ProviderGate) *VendorRefresher {
	v.gate = g
	return v
}
func (v *VendorRefresher) WithAlerter(a Alerter) *VendorRefresher {
	v.alerter = a
	return v
}
func (v *VendorRefresher) WithAuditor(a AuditSink) *VendorRefresher {
	v.auditor = a
	return v
}
func (v *VendorRefresher) WithInterval(d time.Duration) *VendorRefresher {
	if d > 0 {
		v.interval = d
	}
	return v
}
func (v *VendorRefresher) WithClock(now func() time.Time) *VendorRefresher {
	v.now = now
	return v
}

// RefreshOnce runs one fetch→validate→stage→swap→reload pass. Report
// is returned even on error so the operator surface sees per-feed
// outcomes. A pass where ANY required feed failed returns a non-nil
// error AND retains all last-good files — the screener keeps running
// on the previous list set.
func (v *VendorRefresher) RefreshOnce(ctx context.Context) (RefreshReport, error) {
	rep := RefreshReport{StartedAt: v.now()}
	staging := filepath.Join(v.dir, ".staging")
	if err := os.MkdirAll(staging, 0o750); err != nil {
		return rep, fmt.Errorf("sanctions refresh: staging dir: %w", err)
	}
	var firstErr error
	changedAny := false
	for _, f := range v.feeds {
		res := FeedResult{Name: f.Name, Filename: f.Filename}
		body, err := v.fetch.Fetch(ctx, f.URL)
		if err != nil {
			res.Error = err.Error()
			v.feedFailed(ctx, f, err)
			rep.Feeds = append(rep.Feeds, res)
			if f.Required && firstErr == nil {
				firstErr = fmt.Errorf("required feed %s: %w", f.Name, err)
			}
			continue
		}
		sum := sha256.Sum256(body)
		res.SHA256 = hex.EncodeToString(sum[:])
		v.mu.Lock()
		dup := v.hashes[f.Name] == res.SHA256
		v.mu.Unlock()
		if dup {
			// Identical body — no rewrite, no reload. Still counts as
			// a provider success (the feed answered).
			v.feedOK(ctx, f)
			rep.Feeds = append(rep.Feeds, res)
			continue
		}
		// Validate before swapping: stage, parse, demand ≥1 usable
		// entry. A zero-entry body never displaces last good.
		staged := filepath.Join(staging, f.Filename)
		if err := os.WriteFile(staged, body, 0o640); err != nil {
			res.Error = err.Error()
			rep.Feeds = append(rep.Feeds, res)
			if firstErr == nil {
				firstErr = fmt.Errorf("stage feed %s: %w", f.Name, err)
			}
			continue
		}
		n, perr := validateListFile(staged, f.Filename)
		if perr != nil || n == 0 {
			if perr == nil {
				perr = fmt.Errorf("no usable entries")
			}
			res.Error = perr.Error()
			v.feedFailed(ctx, f, perr)
			rep.Feeds = append(rep.Feeds, res)
			if f.Required && firstErr == nil {
				firstErr = fmt.Errorf("required feed %s invalid: %w",
					f.Name, perr)
			}
			continue
		}
		res.Entries = n
		res.Changed = true
		changedAny = true
		final := filepath.Join(v.dir, f.Filename)
		if err := os.Rename(staged, final); err != nil {
			res.Error = err.Error()
			res.Changed = false
			rep.Feeds = append(rep.Feeds, res)
			if firstErr == nil {
				firstErr = fmt.Errorf("swap feed %s: %w", f.Name, err)
			}
			continue
		}
		v.mu.Lock()
		v.hashes[f.Name] = res.SHA256
		v.mu.Unlock()
		v.feedOK(ctx, f)
		rep.Feeds = append(rep.Feeds, res)
	}
	if changedAny {
		if err := v.screener.Reload(ctx); err != nil {
			// Reload keeps the previous set on error — but the staged
			// files ARE now the dir contents. Surface loudly: the
			// screener may be running a set that parses but was flagged
			// bad by a later file's failure. Return the error; ops alert.
			if v.alerter != nil {
				_ = v.alerter(ctx, "P1", "SANCTIONS_RELOAD_FAILED",
					"vendor refresh applied but reload failed: "+err.Error())
			}
			rep.FinishedAt = v.now()
			return rep, fmt.Errorf("sanctions refresh: reload: %w", err)
		}
		rep.Reloaded = true
		d := v.screener.LastDelta()
		rep.DeltaAdded, rep.DeltaRemoved = len(d.Added), len(d.Removed)
	}
	rep.FinishedAt = v.now()
	v.mu.Lock()
	v.lastRun = rep.FinishedAt
	if firstErr == nil {
		v.lastOK = rep.FinishedAt
	}
	v.lastReport = rep
	v.mu.Unlock()
	if v.auditor != nil && (changedAny || firstErr != nil) {
		_ = v.auditor.Record(ctx, 0, "sanctions.vendor_refresh",
			"sanctions_lists", 0, rep)
	}
	return rep, firstErr
}

// feedOK/feedFailed report per-feed outcomes into the provider gate
// (Task 21.3.23) — feed names are the gate's provider registry.
func (v *VendorRefresher) feedOK(ctx context.Context, f ListFeed) {
	if v.gate != nil {
		v.gate.ReportResult(ctx, "feed:"+f.Name, nil)
	}
}

func (v *VendorRefresher) feedFailed(ctx context.Context, f ListFeed, err error) {
	if v.gate != nil {
		v.gate.ReportResult(ctx, "feed:"+f.Name, err)
	}
	if v.alerter != nil {
		sev := "P2"
		if f.Required {
			sev = "P1"
		}
		_ = v.alerter(ctx, sev, "SANCTIONS_FEED_FAILED",
			fmt.Sprintf("list feed %s failed: %v (last good retained)",
				f.Name, err))
	}
}

// Run executes the daily loop (immediate first pass, then on interval)
// until ctx ends.
func (v *VendorRefresher) Run(ctx context.Context) {
	if _, err := v.RefreshOnce(ctx); err != nil {
		if v.alerter != nil {
			_ = v.alerter(ctx, "P1", "SANCTIONS_REFRESH_FAILED", err.Error())
		}
	}
	t := time.NewTicker(v.interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if _, err := v.RefreshOnce(ctx); err != nil && v.alerter != nil {
				_ = v.alerter(ctx, "P1", "SANCTIONS_REFRESH_FAILED", err.Error())
			}
		}
	}
}

// Status reports refresh health for the operator surface.
func (v *VendorRefresher) Status() RefreshReport {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.lastReport
}

// validateListFile parses a staged body through the same parser the
// screener would use for that filename, returning usable raw names.
// Kept in lockstep with Reload's dispatch (extension + classifyFile).
func validateListFile(path, name string) (int, error) {
	_, source, aliasFile := classifyFile(name)
	var got []rawName
	var err error
	switch strings.ToLower(filepath.Ext(name)) {
	case extOFACCSV:
		if aliasFile {
			got, err = parseOFACAlt(path)
		} else {
			got, _, err = parseCSVNames(path, source)
		}
	case extXML:
		got, _, err = parseConsolidatedXML(path)
	case extPlain, extList:
		got, err = parsePlainList(path)
	default:
		return 0, fmt.Errorf("unknown list extension for %s", name)
	}
	if err != nil {
		return 0, err
	}
	return len(got), nil
}
