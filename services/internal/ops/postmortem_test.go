package ops

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// fixtures
// ---------------------------------------------------------------------------

const sampleDoc = `# Incident Post-Mortem — INC-20260920-01

| Field | Value |
|---|---|
| Incident ID | INC-20260920-01 |
| Severity | P1 |
| Status | Closed |
| Commander | Jane Okafor |
| Author(s) | Jane Okafor, Priya Nair |
| Degradation modes traversed | Normal → ReadOnly → Normal |
| DORA-reportable | no |

## 1. Summary

The matching engine shard 3 leader lease expired after account_id 8821134
submitted a burst; on-call paged jane.okafor@exchange-corp.example and the
runbook operator used api_key: sk_live_9f8e7d6c5b4a from the bastion
ops-bastion-01.dc1.internal at 10.32.8.114 to revoke Authorization: Bearer
eyJhbGciOiJIUzI1NiJ9.payload.sig. Client contact was +44 20 7946 0958 and
settlement reference GB29 NWBK 6016 1331 9268 19.

## 2. Impact

| Dimension | Value |
|---|---|
| Client-visible impact | orders rejected: 2,340 (DEGRADED_MODE) |
| Blast radius | accounts affected: 1,240 across shards 3-4 |

## 3. Timeline (UTC, one row per fact)

| Time | Event | Source |
|---|---|---|
| 02:00:01 | leader lease lost | coordination Redis at 2001:db8:ac10:fe01::42 |
`

func writeDoc(t *testing.T, dir, name, body string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// ---------------------------------------------------------------------------
// DirSource
// ---------------------------------------------------------------------------

func TestDirSourceParsesTemplate(t *testing.T) {
	dir := t.TempDir()
	writeDoc(t, dir, "INC-20260920-01-lease-loss.md", sampleDoc)
	writeDoc(t, dir, "README.md", "# not an incident doc")

	list, err := DirSource{Dir: dir}.Postmortems(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 {
		t.Fatalf("expected 1 doc, got %d", len(list))
	}
	p := list[0]
	if p.ID != "INC-20260920-01" || p.Kind != "postmortem" {
		t.Fatalf("id/kind = %q/%q", p.ID, p.Kind)
	}
	if p.Title != "INC-20260920-01" {
		t.Fatalf("title = %q", p.Title)
	}
	if p.Severity != "P1" || p.Status != "Closed" {
		t.Fatalf("severity/status = %q/%q", p.Severity, p.Status)
	}
	if !strings.Contains(p.Mode, "ReadOnly") {
		t.Fatalf("mode = %q", p.Mode)
	}
	if p.DORA {
		t.Fatal("dora should be false")
	}
	if p.StartedAt == nil || p.StartedAt.Format("2006-01-02") != "2026-09-20" {
		t.Fatalf("started_at = %v", p.StartedAt)
	}
}

func TestDirSourceMissingDir(t *testing.T) {
	_, err := DirSource{Dir: filepath.Join(t.TempDir(), "nope")}.
		Postmortems(context.Background())
	if err == nil {
		t.Fatal("missing dir must error")
	}
}

// ---------------------------------------------------------------------------
// Sanitizer
// ---------------------------------------------------------------------------

func TestSanitizeRedacts(t *testing.T) {
	clean, rep := Sanitize(sampleDoc)
	for _, forbidden := range []string{
		"jane.okafor@exchange-corp.example", // email
		"sk_live_9f8e7d6c5b4a",              // api key
		"eyJhbGciOiJIUzI1NiJ9.payload.sig",  // bearer
		"8821134",                           // account id
		"10.32.8.114",                       // ipv4
		"fe01::42",                          // ipv6 tail
		"ops-bastion-01.dc1.internal",       // internal host
		"+44 20 7946 0958",                  // phone
		"GB29 NWBK 6016 1331 9268 19",       // iban
		"Jane Okafor", "Priya Nair",         // blameless names
	} {
		if strings.Contains(clean, forbidden) {
			t.Fatalf("sanitized doc still contains %q\n%s", forbidden, clean)
		}
	}
	if !strings.Contains(clean, "10.32.8.0/24") {
		t.Fatalf("ipv4 not masked to /24:\n%s", clean)
	}
	if !strings.Contains(clean, "2001:db8:ac10::/48") {
		t.Fatalf("ipv6 not masked to /48:\n%s", clean)
	}
	if rep.Total() == 0 {
		t.Fatal("expected redactions")
	}
	for _, rule := range []string{"email", "secret", "ipv4", "ipv6",
		"internal-host", "phone", "iban", "account-id", "person"} {
		if rep.Counts[rule] == 0 {
			t.Fatalf("rule %s fired 0 times (report %+v)", rule, rep.Counts)
		}
	}
}

// Blast-radius counts and incident ids survive — the post-mortem
// contract requires them.
func TestSanitizePreservesOperationalFacts(t *testing.T) {
	clean, _ := Sanitize(sampleDoc)
	for _, keep := range []string{
		"INC-20260920-01", "orders rejected: 2,340", "DEGRADED_MODE",
		"accounts affected: 1,240", "shards 3-4",
	} {
		if !strings.Contains(clean, keep) {
			t.Fatalf("sanitizer destroyed %q\n%s", keep, clean)
		}
	}
}

func TestLooksLikeIdentifier(t *testing.T) {
	yes := []string{"8821134", "9f8e7d6c5b4a", "acct-8821134-x",
		"550e8400-e29b-41d4-a716-446655440000"}
	no := []string{"42", "1,240", "x", "Normal", "ReadOnly", "3-4"}
	for _, v := range yes {
		if !looksLikeIdentifier(v) {
			t.Fatalf("%q should look like an identifier", v)
		}
	}
	for _, v := range no {
		if looksLikeIdentifier(v) {
			t.Fatalf("%q should not look like an identifier", v)
		}
	}
}

// ---------------------------------------------------------------------------
// Generator
// ---------------------------------------------------------------------------

type memWriter struct {
	files map[string][]byte
}

func (w *memWriter) Write(_ context.Context, name string, data []byte, _ string) error {
	if w.files == nil {
		w.files = map[string][]byte{}
	}
	w.files[name] = data
	return nil
}

type fakeSource struct{ docs []Postmortem }

func (s fakeSource) Postmortems(context.Context) ([]Postmortem, error) {
	return s.docs, nil
}

func TestGenerateWritesArchive(t *testing.T) {
	dir := t.TempDir()
	writeDoc(t, dir, "INC-20260920-01-lease-loss.md", sampleDoc)

	w := &memWriter{}
	m, err := Generator{}.Generate(context.Background(), w,
		DirSource{Dir: dir})
	if err != nil {
		t.Fatal(err)
	}
	if len(m.Entries) != 1 || m.Entries[0].ID != "INC-20260920-01" {
		t.Fatalf("manifest entries = %+v", m.Entries)
	}
	page, ok := w.files["postmortems/INC-20260920-01/index.html"]
	if !ok {
		t.Fatalf("artifact missing; wrote %v", keysOf(w.files))
	}
	body := string(page)
	if !strings.Contains(body, "Incident Post-Mortem Archive") &&
		!strings.Contains(body, "lease") {
		t.Fatal("page missing rendered content")
	}
	if strings.Contains(body, "Jane Okafor") ||
		strings.Contains(body, "sk_live_") ||
		strings.Contains(body, "10.32.8.114") {
		t.Fatal("artifact leaked sanitized content")
	}
	if !strings.Contains(body, "sanitized public") {
		t.Fatal("artifact missing sanitization notice")
	}
	idx := string(w.files["postmortems/index.html"])
	if !strings.Contains(idx, "INC-20260920-01") {
		t.Fatal("index does not link the artifact")
	}
	var parsed Manifest
	if err := json.Unmarshal(w.files["postmortems/index.json"], &parsed); err != nil {
		t.Fatalf("index.json: %v", err)
	}
	if parsed.Entries[0].Path != "postmortems/INC-20260920-01/" {
		t.Fatalf("entry path = %q", parsed.Entries[0].Path)
	}
	if parsed.Entries[0].Redactions["email"] == 0 {
		t.Fatal("manifest missing redaction counts")
	}
}

func keysOf(m map[string][]byte) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// First source wins on ID collision — the authored doc supersedes the
// auto-generated incident-record page.
func TestGenerateDedupesByID(t *testing.T) {
	authored := fakeSource{docs: []Postmortem{{
		ID: "INC-20260920-01", Kind: "postmortem",
		Title: "authored", Severity: "P1", Body: "# authored\n",
	}}}
	stub := fakeSource{docs: []Postmortem{{
		ID: "INC-20260920-01", Kind: "incident-record",
		Title: "auto", Body: "# auto\n",
	}}}
	w := &memWriter{}
	m, err := Generator{}.Generate(context.Background(), w, authored, stub)
	if err != nil {
		t.Fatal(err)
	}
	if len(m.Entries) != 1 || m.Entries[0].Title != "authored" {
		t.Fatalf("dedupe failed: %+v", m.Entries)
	}
}

func TestGenerateEmptyArchive(t *testing.T) {
	w := &memWriter{}
	m, err := Generator{}.Generate(context.Background(), w,
		fakeSource{docs: nil})
	if err != nil {
		t.Fatal(err)
	}
	if len(m.Entries) != 0 {
		t.Fatal("expected empty manifest")
	}
	if !strings.Contains(string(w.files["postmortems/index.html"]),
		"No post-mortems published yet") {
		t.Fatal("empty index missing placeholder")
	}
}

// Newest first ordering.
func TestGenerateOrdersNewestFirst(t *testing.T) {
	d1 := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	d2 := time.Date(2026, 9, 20, 0, 0, 0, 0, time.UTC)
	src := fakeSource{docs: []Postmortem{
		{ID: "INC-20260901-01", Title: "old", StartedAt: &d1, Body: "# old"},
		{ID: "INC-20260920-01", Title: "new", StartedAt: &d2, Body: "# new"},
	}}
	w := &memWriter{}
	m, err := Generator{}.Generate(context.Background(), w, src)
	if err != nil {
		t.Fatal(err)
	}
	if m.Entries[0].ID != "INC-20260920-01" {
		t.Fatalf("order = %v", []string{m.Entries[0].ID, m.Entries[1].ID})
	}
}

// ---------------------------------------------------------------------------
// DirWriter — traversal safety
// ---------------------------------------------------------------------------

func TestDirWriterStaysUnderRoot(t *testing.T) {
	root := t.TempDir()
	w := DirWriter{Root: root}
	err := w.Write(context.Background(), "../../etc/evil.txt",
		[]byte("x"), "text/plain")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "etc", "evil.txt")); err != nil {
		t.Fatal("expected traversal-proof write under root")
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(root), "evil.txt")); !os.IsNotExist(err) {
		t.Fatal("write escaped the root")
	}
}

// ---------------------------------------------------------------------------
// Renderer — raw HTML in the source can never reach the artifact.
// ---------------------------------------------------------------------------

func TestRenderEscapesRawHTML(t *testing.T) {
	out := RenderMarkdown("# title\n\n<script>alert(1)</script>\n")
	if strings.Contains(out, "<script>") {
		t.Fatal("raw HTML reached the artifact")
	}
	if !strings.Contains(out, "&lt;script&gt;") {
		t.Fatal("expected escaped script tag")
	}
}

func TestRenderTableAndInline(t *testing.T) {
	out := RenderMarkdown("| A | B |\n|---|---|\n| `x` | **y** |\n")
	if !strings.Contains(out, "<table>") || !strings.Contains(out, "<code>x</code>") ||
		!strings.Contains(out, "<strong>y</strong>") {
		t.Fatalf("render = %s", out)
	}
}
