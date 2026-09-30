// Post-mortem public archive generator — Phase-09 Task 9.3.25 AC row
// "Historical incident post-mortems publicly accessible".
//
// The status page consumes ops_incidents (migration 197) through
// GET /api/v1/system/incidents, whose rows carry postmortem_url. Until
// now nothing produced the artifact that URL points at. This package is
// that producer: it renders sanitized post-mortems into a static,
// public-consumable layout the status host can serve verbatim:
//
//	<out>/postmortems/index.html            human index, newest first
//	<out>/postmortems/index.json            machine manifest
//	<out>/postmortems/{incident-id}/index.html   one page per incident
//
// Two sources feed the archive; both funnel through the same sanitizer:
//
//   - DirSource  — the §9.3.9 authored archive (docs/incidents/INC-*.md,
//     postmortem-template.md shape). These are full post-mortems.
//   - StoreSource — ops_incidents rows (public, resolved, no postmortem
//     link yet): the auto-posted degradation notices. These render as
//     lighter "incident record" pages so every public incident reaches a
//     resolvable artifact.
//
// Sanitization is mandatory on the public path and follows the
// compliance masking conventions (internal/compliance/audit_query.go:
// v4 → /24, v6 → /48, PII-keyed values → redacted): emails, phones,
// IBANs, account/user/client/order linkage ids, auth material, person
// names in blameless metadata rows (Commander / Author(s)), and
// internal hostnames are stripped before anything is written.
//
// Nothing here adds or serves routes — the output is static files. A
// separate optional step (LinkIncidents) writes the generated URL back
// into ops_incidents.postmortem_url so GET /api/v1/system/incidents
// surfaces the link on the next read.
package ops

import (
	"context"
	"encoding/json"
	"fmt"
	"html"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// ---------------------------------------------------------------------------
// Model
// ---------------------------------------------------------------------------

// Postmortem is one publishable incident document.
type Postmortem struct {
	ID         string     `json:"id"`                    // INC-YYYYMMDD-NN or ops-{rowid}
	Kind       string     `json:"kind"`                  // "postmortem" | "incident-record"
	Title      string     `json:"title"`                 //
	Severity   string     `json:"severity,omitempty"`    // P0..P3
	Status     string     `json:"status,omitempty"`      // RESOLVED etc.
	Mode       string     `json:"mode,omitempty"`        // degradation modes traversed
	DORA       bool       `json:"dora_reportable"`       //
	StartedAt  *time.Time `json:"started_at,omitempty"`  //
	ResolvedAt *time.Time `json:"resolved_at,omitempty"` //
	Body       string     `json:"-"`                     // authored markdown (pre-sanitize)
	Source     string     `json:"-"`                     // provenance: file path or "ops_incidents#N"
}

// Source yields post-mortem documents. DirSource and StoreSource satisfy
// it; tests use fakes.
type Source interface {
	Postmortems(ctx context.Context) ([]Postmortem, error)
}

// ---------------------------------------------------------------------------
// DirSource — docs/incidents/INC-*.md (Task 9.3.9 archive)
// ---------------------------------------------------------------------------

var (
	incidentFileRe = regexp.MustCompile(`(?i)^(INC-\d{8}-\d+[A-Za-z0-9-]*)\.md$`)
	incidentIDRe   = regexp.MustCompile(`(?i)\bINC-\d{8}-\d+\b`)
	headingRe      = regexp.MustCompile(`^#\s+(.+?)\s*$`)
	metaRowRe      = regexp.MustCompile(`^\|\s*([^|]+?)\s*\|\s*([^|]*?)\s*\|\s*$`)
	dateInIDRe     = regexp.MustCompile(`^INC-(\d{4})(\d{2})(\d{2})-`)
)

// DirSource scans dir for INC-*.md post-mortem documents.
type DirSource struct {
	Dir string
}

func (s DirSource) Postmortems(ctx context.Context) ([]Postmortem, error) {
	ents, err := os.ReadDir(s.Dir)
	if err != nil {
		return nil, fmt.Errorf("postmortem dir %s: %w", s.Dir, err)
	}
	var out []Postmortem
	for _, e := range ents {
		if e.IsDir() || !incidentFileRe.MatchString(e.Name()) {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(s.Dir, e.Name()))
		if err != nil {
			return nil, fmt.Errorf("postmortem read %s: %w", e.Name(), err)
		}
		out = append(out, parsePostmortemDoc(e.Name(), string(raw)))
	}
	return out, ctx.Err()
}

// parsePostmortemDoc extracts id/title/severity/status/mode/dates from
// the template shape: first `# ` heading is the title, the `| Field |
// Value |` block is metadata.
func parsePostmortemDoc(filename, body string) Postmortem {
	p := Postmortem{Kind: "postmortem", Body: body, Source: filename}
	// Canonical id order: first INC-… token in the document (the title
	// carries it), then the filename, then the bare filename. The
	// filename may append a slug (INC-…-slug.md) — never part of the id.
	if m := incidentIDRe.FindString(body); m != "" {
		p.ID = strings.ToUpper(m)
	} else if m := incidentIDRe.FindString(filename); m != "" {
		p.ID = strings.ToUpper(m)
	} else {
		p.ID = strings.TrimSuffix(filename, filepath.Ext(filename))
	}
	meta := map[string]string{}
	for _, line := range strings.Split(body, "\n") {
		if hm := headingRe.FindStringSubmatch(line); hm != nil && p.Title == "" {
			t := hm[1]
			// "Incident Post-Mortem — INC-…" → keep just the id part if bare
			t = strings.TrimPrefix(t, "Incident Post-Mortem")
			t = strings.TrimLeft(t, " —–:")
			p.Title = strings.TrimSpace(t)
		}
		if rm := metaRowRe.FindStringSubmatch(line); rm != nil {
			key := strings.ToLower(strings.TrimSpace(rm[1]))
			val := strings.TrimSpace(rm[2])
			if _, seen := meta[key]; !seen {
				meta[key] = val
			}
		}
	}
	if p.Title == "" {
		p.Title = p.ID
	}
	if v := firstWord(meta["severity"]); v != "" {
		p.Severity = strings.ToUpper(v)
	}
	if v := firstWord(meta["status"]); v != "" {
		p.Status = v
	}
	if v := meta["degradation modes traversed"]; v != "" {
		p.Mode = v
	}
	p.DORA = strings.HasPrefix(strings.ToLower(meta["dora-reportable"]), "yes")
	for _, k := range []string{"started", "detected", "started_at"} {
		if t, ok := parseMetaTime(meta[k]); ok {
			p.StartedAt = &t
			break
		}
	}
	if p.StartedAt == nil {
		if m := dateInIDRe.FindStringSubmatch(p.ID); m != nil {
			if t, err := time.Parse("2006-01-02",
				m[1]+"-"+m[2]+"-"+m[3]); err == nil {
				p.StartedAt = &t
			}
		}
	}
	for _, k := range []string{"resolved", "recovered", "resolved_at"} {
		if t, ok := parseMetaTime(meta[k]); ok {
			p.ResolvedAt = &t
			break
		}
	}
	return p
}

func firstWord(s string) string {
	f := strings.Fields(s)
	if len(f) == 0 {
		return ""
	}
	return f[0]
}

func parseMetaTime(s string) (time.Time, bool) {
	for _, layout := range []string{time.RFC3339, "2006-01-02 15:04", "2006-01-02"} {
		if t, err := time.Parse(layout, strings.TrimSpace(s)); err == nil {
			return t, true
		}
	}
	return time.Time{}, false
}

// ---------------------------------------------------------------------------
// StoreSource — ops_incidents (migration 197)
// ---------------------------------------------------------------------------

// StoreSource reads public, resolved incident notices that do not yet
// carry a postmortem_url — rows with one are already published. These
// become "incident-record" artifacts; a fuller authored INC doc always
// wins because sources are merged with DirSource first.
type StoreSource struct {
	Pool *pgxpool.Pool
}

func (s StoreSource) Postmortems(ctx context.Context) ([]Postmortem, error) {
	if s.Pool == nil {
		return nil, nil
	}
	rows, err := s.Pool.Query(ctx, `
		SELECT id, title, severity, status, mode, started_at, resolved_at,
		       summary
		  FROM ops_incidents
		 WHERE public AND status = 'RESOLVED' AND postmortem_url IS NULL
		 ORDER BY started_at DESC`)
	if err != nil {
		return nil, fmt.Errorf("postmortem store source: %w", err)
	}
	defer rows.Close()
	var out []Postmortem
	for rows.Next() {
		var (
			id            int64
			title, sev    string
			status        string
			mode, summary *string
			started       time.Time
			resolved      *time.Time
		)
		if err := rows.Scan(&id, &title, &sev, &status, &mode, &started,
			&resolved, &summary); err != nil {
			return nil, fmt.Errorf("postmortem store scan: %w", err)
		}
		p := Postmortem{
			ID:         fmt.Sprintf("ops-%d", id),
			Kind:       "incident-record",
			Title:      title,
			Severity:   sev,
			Status:     status,
			StartedAt:  &started,
			ResolvedAt: resolved,
			Source:     fmt.Sprintf("ops_incidents#%d", id),
		}
		if mode != nil {
			p.Mode = *mode
		}
		var b strings.Builder
		fmt.Fprintf(&b, "# %s\n\n", title)
		if summary != nil && *summary != "" {
			fmt.Fprintf(&b, "## 1. Summary\n\n%s\n", *summary)
		}
		fmt.Fprintf(&b, "\n_Auto-generated incident record from the "+
			"operations incident store; the full blameless post-mortem "+
			"supersedes this page when published._\n")
		p.Body = b.String()
		// An authored INC doc always wins over the row it belongs to:
		// when the row's title/summary already carries an INC id, adopt
		// it so the merge dedupes under the doc.
		if inc := incidentIDRe.FindString(title); inc != "" {
			p.ID = strings.ToUpper(inc)
		} else if summary != nil {
			if inc := incidentIDRe.FindString(*summary); inc != "" {
				p.ID = strings.ToUpper(inc)
			}
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// ---------------------------------------------------------------------------
// Sanitizer — compliance conventions (docs/security/pii-catalog.csv
// classes + internal/compliance/audit_query.go masking).
// ---------------------------------------------------------------------------

// SanitizeReport counts redactions per rule for the build log.
type SanitizeReport struct {
	Counts map[string]int `json:"counts"`
}

func (r *SanitizeReport) add(name string, n int) {
	if n > 0 {
		if r.Counts == nil {
			r.Counts = map[string]int{}
		}
		r.Counts[name] += n
	}
}

// Total redactions applied.
func (r SanitizeReport) Total() int {
	n := 0
	for _, c := range r.Counts {
		n += c
	}
	return n
}

type scrubRule struct {
	name string
	re   *regexp.Regexp
	repl func(m []string) string
}

// Value must look like a durable identifier — contains a letter AND a
// digit (uuid/ulid/hash shaped), is a long digit run (≥6: account ids,
// iban fragments), or embeds a '-'/'_' separator. Plain counts and
// short tokens ("42", "1,240") stay — blast-radius counts are part of
// the post-mortem contract.
func looksLikeIdentifier(v string) bool {
	if len(v) < 4 {
		return false
	}
	digits, letters := 0, 0
	for _, r := range v {
		switch {
		case r >= '0' && r <= '9':
			digits++
		case (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z'):
			letters++
		}
	}
	switch {
	case digits >= 6:
		return true
	case digits >= 1 && letters >= 1:
		return true
	case strings.ContainsAny(v, "-_") && digits+letters >= 8:
		return true
	}
	return false
}

// identifierRepl keeps the key, replaces the value.
func identifierRepl(m []string) string {
	v := strings.TrimRight(m[2], ".,;)]}")
	if !looksLikeIdentifier(v) {
		return m[0]
	}
	return m[1] + "[redacted-id]"
}

var scrubRules = []scrubRule{
	{"secret", regexp.MustCompile(`(?i)\b(bearer)\s+[A-Za-z0-9._~+/-]{8,}`),
		func(m []string) string { return m[1] + " [redacted-secret]" }},
	{"secret", regexp.MustCompile(`(?i)\b((?:api[-_ ]?key|api[-_ ]?secret|access[-_ ]?token|auth[-_ ]?token|secret|token|password|passwd|pwd|passphrase|private[-_ ]?key|signing[-_ ]?key|session[-_ ]?key|hmac[-_ ]?secret|webhook[-_ ]?secret|totp)\s*[:=]\s*"?)([A-Za-z0-9._~+/-]{4,})`),
		func(m []string) string { return m[1] + "[redacted-secret]" }},
	{"email", regexp.MustCompile(`\b[A-Za-z0-9._%+-]+@[A-Za-z0-9.-]+\.[A-Za-z]{2,}\b`),
		func(m []string) string { return "[redacted-email]" }},
	{"iban", regexp.MustCompile(`\b[A-Z]{2}\d{2}(?:[\s-]?[A-Z0-9]{4}){2,7}[\s-]?[A-Z0-9]{1,4}\b`),
		func(m []string) string { return "[redacted-iban]" }},
	{"phone", regexp.MustCompile(`\+\d[\d\s().-]{7,14}\d`),
		func(m []string) string { return "[redacted-phone]" }},
	// Account/client linkage ids (PII classes LINKAGE/FREE_TEXT). Key is
	// preserved, value redacted only when it looks like an identifier —
	// "accounts affected: 1,240" survives, "account_id 882114" does not.
	{"account-id", regexp.MustCompile(`(?i)\b((?:account(?:s)?(?:[-_ ]?id|[-_ ]?number|[-_ ]?no)?|user[-_ ]?ids?|client[-_ ]?ids?|customer[-_ ]?ids?|member[-_ ]?ids?|kyc[-_ ]?doc(?:ument)?[-_ ]?ids?|order[-_ ]?ids?|client[-_ ]?order[-_ ]?ids?|trade[-_ ]?ids?|execution[-_ ]?ids?|position[-_ ]?ids?|withdrawal[-_ ]?ids?|deposit[-_ ]?ids?|transfer[-_ ]?ids?|reference[-_ ]?account|beneficiary[-_ ]?ids?)\s*(?:[:=#]|\bis\b|\bof\b)?\s*"?)([A-Za-z0-9][A-Za-z0-9.,_-]*[A-Za-z0-9])`),
		identifierRepl},
	// Name-valued fields inherently carry a person name — redact the
	// value unconditionally (single or multi word, capitalized).
	{"person", regexp.MustCompile(`(?i)\b((?:full[-_ ]?name|first[-_ ]?name|last[-_ ]?name|originator[-_ ]?name|beneficiary[-_ ]?name|contact[-_ ]?name|counterparty[-_ ]?name|customer[-_ ]?name|account[-_ ]?holder)\s*[:=#]?\s*)([A-Z][a-z]+(?:\s+[A-Z][a-z]+){0,3})`),
		func(m []string) string { return m[1] + "[redacted]" }},
	// IPs: mask host octets to the compliance convention (/24, /48).
	{"ipv4", regexp.MustCompile(`\b(\d{1,3})\.(\d{1,3})\.(\d{1,3})\.\d{1,3}\b`),
		func(m []string) string { return m[1] + "." + m[2] + "." + m[3] + ".0/24" }},
	// IPv6 needs ≥4 groups (first three hextets + ≥1 more, empty hextets
	// allowed for :: compression) — that bound also keeps HH:MM:SS
	// timestamps (exactly three groups) out of the rule.
	{"ipv6", regexp.MustCompile(`\b([0-9a-fA-F]{1,4}):([0-9a-fA-F]{1,4}):([0-9a-fA-F]{1,4})(?::[0-9a-fA-F]{0,4}){1,5}\b`),
		func(m []string) string { return m[1] + ":" + m[2] + ":" + m[3] + "::/48" }},
	// Internal hostnames disclose topology; public FQDNs survive.
	{"internal-host", regexp.MustCompile(`\b(?:[A-Za-z0-9-]+\.)+(?:internal|corp|intra|lan|local)\b`),
		func(m []string) string { return "[redacted-host]" }},
	// Blameless metadata: individual names never go public.
	{"person", regexp.MustCompile(`(?im)^(\|\s*(?:Commander|Authors?|Author\(s\)|Incident\s+Commander)\s*\|)[^|]*(\|)`),
		func(m []string) string { return m[1] + " [redacted] " + m[2] }},
	{"person", regexp.MustCompile(`(?i)\b((?:commander|author(?:\(s\)|s)?|reported\s+by|on[- ]?call\s+engineer)\s*[:=]\s*)([A-Z][a-z]+(?:\s+[A-Z][a-z]+)+)`),
		func(m []string) string { return m[1] + "[redacted]" }},
}

// Sanitize rewrites a post-mortem document for public release. Every
// rule in scrubRules is applied; the report counts redactions per rule.
func Sanitize(body string) (string, SanitizeReport) {
	rep := SanitizeReport{}
	out := body
	for _, rule := range scrubRules {
		n := 0
		out = rule.re.ReplaceAllStringFunc(out, func(match string) string {
			sub := rule.re.FindStringSubmatch(match)
			repl := rule.repl(sub)
			if repl != match {
				n++
			}
			return repl
		})
		rep.add(rule.name, n)
	}
	return out, rep
}

// ---------------------------------------------------------------------------
// Renderer — sanitized markdown → static HTML (subset: h1-h4, tables,
// lists, fences, blockquote, paragraphs, `code`, **bold**, links).
// ---------------------------------------------------------------------------

var (
	mdHeading = regexp.MustCompile(`^(#{1,4})\s+(.*)$`)
	mdTable   = regexp.MustCompile(`^\|(.+)\|\s*$`)
	mdDivider = regexp.MustCompile(`^\|[\s:|-]+\|\s*$`)
	mdList    = regexp.MustCompile(`^\s*[-*]\s+(.*)$`)
	mdNumList = regexp.MustCompile(`^\s*\d+\.\s+(.*)$`)
	mdQuote   = regexp.MustCompile(`^>\s?(.*)$`)
	mdInline  = regexp.MustCompile("`([^`]+)`|\\*\\*([^*]+)\\*\\*|\\[([^\\]]+)\\]\\(([^)]+)\\)")
)

func inlineMD(s string) string {
	return mdInline.ReplaceAllStringFunc(s, func(m string) string {
		sub := mdInline.FindStringSubmatch(m)
		switch {
		case sub[1] != "":
			return "<code>" + html.EscapeString(sub[1]) + "</code>"
		case sub[2] != "":
			return "<strong>" + html.EscapeString(sub[2]) + "</strong>"
		default:
			href := html.EscapeString(sub[4])
			if !(strings.HasPrefix(sub[4], "http://") ||
				strings.HasPrefix(sub[4], "https://") ||
				strings.HasPrefix(sub[4], "/")) {
				return html.EscapeString(sub[3])
			}
			return `<a href="` + href + `" rel="noopener">` +
				html.EscapeString(sub[3]) + `</a>`
		}
	})
}

// renderInline escapes everything except the spans inlineMD produces:
// inline constructs are swapped to placeholders first, the remainder is
// HTML-escaped, then the placeholders resolve to pre-built tags — raw
// HTML in the source can never reach the output.
func renderInline(s string) string {
	spans := map[string]string{}
	i := 0
	s = mdInline.ReplaceAllStringFunc(s, func(m string) string {
		key := fmt.Sprintf("\x00%d\x00", i)
		spans[key] = inlineMD(m)
		i++
		return key
	})
	s = html.EscapeString(s)
	for k, v := range spans {
		s = strings.ReplaceAll(s, html.EscapeString(k), v)
		s = strings.ReplaceAll(s, k, v)
	}
	return s
}

// RenderMarkdown converts the sanitized doc to a body fragment.
func RenderMarkdown(md string) string {
	var b strings.Builder
	lines := strings.Split(md, "\n")
	inCode, inTable, inList := false, false, false
	closeBlocks := func() {
		if inTable {
			b.WriteString("</tbody></table>\n")
			inTable = false
		}
		if inList {
			b.WriteString("</ul>\n")
			inList = false
		}
	}
	for i := 0; i < len(lines); i++ {
		line := strings.TrimRight(lines[i], " \t")
		switch {
		case strings.HasPrefix(line, "```"):
			if inCode {
				b.WriteString("</code></pre>\n")
			} else {
				closeBlocks()
				b.WriteString("<pre><code>")
			}
			inCode = !inCode
			continue
		case inCode:
			b.WriteString(html.EscapeString(line) + "\n")
			continue
		case line == "":
			closeBlocks()
			continue
		case mdDivider.MatchString(line):
			continue
		case mdTable.MatchString(line):
			cells := strings.Split(strings.Trim(line, "|"), "|")
			if !inTable {
				closeBlocks()
				b.WriteString("<table><thead><tr>")
				for _, c := range cells {
					b.WriteString("<th>" + renderInline(strings.TrimSpace(c)) + "</th>")
				}
				b.WriteString("</tr></thead><tbody>\n")
				inTable = true
				// first row is the header; divider row already skipped
				continue
			}
			b.WriteString("<tr>")
			for _, c := range cells {
				b.WriteString("<td>" + renderInline(strings.TrimSpace(c)) + "</td>")
			}
			b.WriteString("</tr>\n")
			continue
		}
		if m := mdHeading.FindStringSubmatch(line); m != nil {
			closeBlocks()
			lvl := len(m[1])
			fmt.Fprintf(&b, "<h%d>%s</h%d>\n", lvl, renderInline(m[2]), lvl)
			continue
		}
		if m := mdList.FindStringSubmatch(line); m != nil {
			if !inList {
				if inTable {
					b.WriteString("</tbody></table>\n")
					inTable = false
				}
				b.WriteString("<ul>\n")
				inList = true
			}
			b.WriteString("<li>" + renderInline(m[1]) + "</li>\n")
			continue
		}
		if m := mdNumList.FindStringSubmatch(line); m != nil {
			if !inList {
				if inTable {
					b.WriteString("</tbody></table>\n")
					inTable = false
				}
				b.WriteString("<ul>\n")
				inList = true
			}
			b.WriteString("<li>" + renderInline(m[1]) + "</li>\n")
			continue
		}
		if m := mdQuote.FindStringSubmatch(line); m != nil {
			closeBlocks()
			b.WriteString("<blockquote>" + renderInline(m[1]) + "</blockquote>\n")
			continue
		}
		if inList || inTable {
			closeBlocks()
		}
		b.WriteString("<p>" + renderInline(line) + "</p>\n")
	}
	closeBlocks()
	if inCode {
		b.WriteString("</code></pre>\n")
	}
	return b.String()
}

const pageCSS = `body{font-family:system-ui,sans-serif;max-width:64rem;margin:2rem auto;padding:0 1rem;color:#1c2330;line-height:1.55}
h1{border-bottom:2px solid #d5dbe5;padding-bottom:.3rem}
table{border-collapse:collapse;width:100%;margin:1rem 0}
td,th{border:1px solid #d5dbe5;padding:.4rem .6rem;text-align:left;font-size:.9rem}
code{background:#f0f3f8;padding:.1rem .3rem;border-radius:3px}
pre{background:#f0f3f8;padding:.8rem;border-radius:6px;overflow-x:auto}
blockquote{border-left:3px solid #d5dbe5;margin-left:0;padding-left:1rem;color:#556}
.meta{color:#556;font-size:.85rem;margin-bottom:1.5rem}
.badge{display:inline-block;padding:.1rem .5rem;border-radius:4px;font-size:.8rem;font-weight:600;margin-right:.4rem}
.sev-p0{background:#7f1d1d;color:#fff}.sev-p1{background:#b45309;color:#fff}
.sev-p2{background:#1d4ed8;color:#fff}.sev-p3{background:#475569;color:#fff}
.notice{background:#fef9c3;border:1px solid #eab308;border-radius:6px;padding:.6rem .9rem;font-size:.85rem}`

func pageHTML(p Postmortem, bodyFrag, generated string) string {
	var b strings.Builder
	sev := strings.ToLower(p.Severity)
	fmt.Fprintf(&b, `<!doctype html><html lang="en"><head><meta charset="utf-8">
<meta name="viewport" content="width=device-width,initial-scale=1">
<title>%s — %s</title><style>%s</style></head><body>`,
		html.EscapeString(p.ID), html.EscapeString(p.Title), pageCSS)
	b.WriteString(`<div class="meta">`)
	fmt.Fprintf(&b, `<span class="badge sev-%s">%s</span>`,
		html.EscapeString(sev), html.EscapeString(p.Severity))
	fmt.Fprintf(&b, `<span class="badge">%s</span>`,
		html.EscapeString(p.ID))
	if p.Status != "" {
		fmt.Fprintf(&b, `<span class="badge">%s</span>`,
			html.EscapeString(p.Status))
	}
	if p.StartedAt != nil {
		fmt.Fprintf(&b, ` started %s`, p.StartedAt.UTC().Format("2006-01-02"))
	}
	if p.ResolvedAt != nil {
		fmt.Fprintf(&b, ` · resolved %s`, p.ResolvedAt.UTC().Format("2006-01-02"))
	}
	if p.Mode != "" {
		fmt.Fprintf(&b, ` · modes: %s`, html.EscapeString(p.Mode))
	}
	if p.DORA {
		b.WriteString(` · DORA-reportable`)
	}
	fmt.Fprintf(&b, ` · generated %s</div>`, generated)
	b.WriteString(`<p class="notice">This page is the sanitized public ` +
		`release of an internal incident document. Account-identifying, ` +
		`personal, network-topology and credential material has been ` +
		`redacted per the venue's disclosure conventions.</p>`)
	b.WriteString(bodyFrag)
	fmt.Fprintf(&b, `<hr><p class="meta"><a href="../index.html">← incident archive</a></p></body></html>`)
	return b.String()
}

// ---------------------------------------------------------------------------
// Archive writer + generator
// ---------------------------------------------------------------------------

// Writer persists one artifact under a relative name.
type Writer interface {
	Write(ctx context.Context, name string, data []byte, contentType string) error
}

// DirWriter writes artifacts under Root — the "status/public" layout the
// status host serves or syncs to object storage.
type DirWriter struct {
	Root string
}

func (w DirWriter) Write(ctx context.Context, name string, data []byte, _ string) error {
	clean := path.Clean("/" + name)       // normalize; kills ../
	rel := strings.TrimPrefix(clean, "/") // safe relative path
	full := filepath.Join(w.Root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		return fmt.Errorf("postmortem mkdir %s: %w", filepath.Dir(full), err)
	}
	if err := os.WriteFile(full, data, 0o644); err != nil {
		return fmt.Errorf("postmortem write %s: %w", full, err)
	}
	return ctx.Err()
}

// Manifest is index.json — one entry per generated artifact.
type Manifest struct {
	GeneratedAt time.Time `json:"generated_at"`
	Entries     []Entry   `json:"entries"`
}

// Entry indexes one artifact.
type Entry struct {
	Postmortem
	Path       string         `json:"path"` // public-relative URL path
	Redactions map[string]int `json:"redactions,omitempty"`
}

// Generator renders sources into the public archive layout.
type Generator struct {
	Now func() time.Time // nil → time.Now
}

// PublicPath is the URL path of one incident artifact relative to the
// archive root — also the value LinkIncidents writes to postmortem_url.
func PublicPath(id string) string {
	return "postmortems/" + id + "/"
}

// Generate renders every postmortem from all sources, dedupes by ID
// (first source wins — order sources authoritative-first), and writes
// the index + manifest.
func (g Generator) Generate(ctx context.Context, w Writer, sources ...Source) (*Manifest, error) {
	now := time.Now
	if g.Now != nil {
		now = g.Now
	}
	seen := map[string]bool{}
	var docs []Postmortem
	for _, src := range sources {
		list, err := src.Postmortems(ctx)
		if err != nil {
			return nil, err
		}
		for _, p := range list {
			if p.ID == "" || seen[p.ID] {
				continue
			}
			seen[p.ID] = true
			docs = append(docs, p)
		}
	}
	sort.Slice(docs, func(i, j int) bool {
		a, b := time.Time{}, time.Time{}
		if docs[i].StartedAt != nil {
			a = *docs[i].StartedAt
		}
		if docs[j].StartedAt != nil {
			b = *docs[j].StartedAt
		}
		if !a.Equal(b) {
			return a.After(b) // newest first
		}
		return docs[i].ID > docs[j].ID
	})

	gen := now().UTC().Format("2006-01-02")
	m := &Manifest{GeneratedAt: now().UTC()}
	var idx strings.Builder
	idx.WriteString(`<!doctype html><html lang="en"><head><meta charset="utf-8">
<meta name="viewport" content="width=device-width,initial-scale=1">
<title>Incident Post-Mortem Archive</title><style>` + pageCSS + `</style></head><body>
<h1>Incident Post-Mortem Archive</h1>
<p class="meta">Sanitized public releases of venue incident post-mortems.
Published per the blameless post-mortem policy — generated ` + gen + `.</p>`)
	if len(docs) == 0 {
		idx.WriteString(`<p>No post-mortems published yet.</p>`)
	} else {
		idx.WriteString("<ul>\n")
	}
	for _, p := range docs {
		clean, rep := Sanitize(p.Body)
		frag := RenderMarkdown(clean)
		page := pageHTML(p, frag, gen)
		rel := PublicPath(p.ID) + "index.html"
		if err := w.Write(ctx, rel, []byte(page), "text/html; charset=utf-8"); err != nil {
			return nil, err
		}
		e := Entry{Postmortem: p, Path: PublicPath(p.ID), Redactions: rep.Counts}
		m.Entries = append(m.Entries, e)

		var metaParts []string
		if p.StartedAt != nil {
			metaParts = append(metaParts, p.StartedAt.UTC().Format("2006-01-02"))
		}
		if p.Severity != "" {
			metaParts = append(metaParts, p.Severity)
		}
		idx.WriteString(`<li><a href="` + html.EscapeString(PublicPath(p.ID)) +
			`">` + html.EscapeString(p.Title) + `</a> <span class="meta">` +
			html.EscapeString(strings.Join(metaParts, " · ")) + `</span></li>` + "\n")
	}
	if len(docs) > 0 {
		idx.WriteString("</ul>\n")
	}
	idx.WriteString(`</body></html>`)

	raw, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("postmortem manifest: %w", err)
	}
	if err := w.Write(ctx, "postmortems/index.json", raw, "application/json"); err != nil {
		return nil, err
	}
	if err := w.Write(ctx, "postmortems/index.html", []byte(idx.String()),
		"text/html; charset=utf-8"); err != nil {
		return nil, err
	}
	return m, ctx.Err()
}

// LinkIncidents points public ops_incidents rows at their generated
// artifact: rows whose title/summary already carries the incident id
// (or whose store id matches an ops-N artifact) get postmortem_url set
// — never overwriting an operator-set link.
func LinkIncidents(ctx context.Context, pool *pgxpool.Pool, baseURL string, m *Manifest) (int, error) {
	if pool == nil || m == nil {
		return 0, nil
	}
	base := strings.TrimRight(baseURL, "/")
	total := 0
	for _, e := range m.Entries {
		url := base + "/" + e.Path
		var affected int64
		// ops-N artifacts key on the row id; INC-* artifacts match the
		// incident id token inside the row's title/summary.
		if strings.HasPrefix(e.ID, "ops-") {
			var rowID int64
			if _, err := fmt.Sscanf(e.ID, "ops-%d", &rowID); err != nil {
				continue
			}
			ct, err := pool.Exec(ctx, `
				UPDATE ops_incidents SET postmortem_url = $1
				 WHERE id = $2 AND public AND postmortem_url IS NULL`,
				url, rowID)
			if err != nil {
				return total, fmt.Errorf("link incident %s: %w", e.ID, err)
			}
			affected = ct.RowsAffected()
		} else {
			ct, err := pool.Exec(ctx, `
				UPDATE ops_incidents SET postmortem_url = $1
				 WHERE public AND postmortem_url IS NULL
				   AND (title ILIKE '%' || $2 || '%'
				        OR summary ILIKE '%' || $2 || '%')`,
				url, e.ID)
			if err != nil {
				return total, fmt.Errorf("link incident %s: %w", e.ID, err)
			}
			affected = ct.RowsAffected()
		}
		total += int(affected)
	}
	return total, nil
}
