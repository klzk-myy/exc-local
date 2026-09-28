// Task 5.3.20 — API deprecation policy surface.
//
//	POST /api/v1/admin/api-deprecations — announce a deprecation
//	    {method?, path, match_prefix?, sunset_at, replacement?, notice?}
//	    sunset_at ≥ announced_at + 6 months (policy CHECK, migration 182)
//	GET  /api/v1/admin/api-deprecations — list rules
//	GET  /developer/migration           — public migration guide
//
// Enforcement middleware lives in internal/deprecation (wired around the
// mux in cmd/gateway): announced endpoints carry Deprecation + Sunset +
// Link headers; past-sunset endpoints answer 410 ENDPOINT_GONE.
package api

import (
	"encoding/json"
	"fmt"
	"html"
	"net/http"
	"strings"
	"time"

	"exchange/internal/deprecation"
	"exchange/internal/gateway"
)

// AdminDeprecations returns announce/list handlers for the policy store.
func AdminDeprecations(store *deprecation.Store) (announce, list http.HandlerFunc) {
	announce = func(w http.ResponseWriter, r *http.Request) {
		claims := requireAdmin(w, r)
		if claims == nil {
			return
		}
		var body struct {
			Method      *string `json:"method"`
			Path        string  `json:"path"`
			MatchPrefix bool    `json:"match_prefix"`
			AnnouncedAt *string `json:"announced_at"`
			SunsetAt    string  `json:"sunset_at"`
			Replacement *string `json:"replacement"`
			Notice      *string `json:"notice"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			WriteError(w, "INVALID_REQUEST",
				"malformed body", gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		sunset, err := time.Parse(time.RFC3339, body.SunsetAt)
		if err != nil {
			WriteError(w, "INVALID_REQUEST",
				"sunset_at must be RFC3339", gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		var announced time.Time
		if body.AnnouncedAt != nil {
			announced, err = time.Parse(time.RFC3339, *body.AnnouncedAt)
			if err != nil {
				WriteError(w, "INVALID_REQUEST",
					"announced_at must be RFC3339", gateway.RequestIDFrom(r.Context()), nil)
				return
			}
		}
		actor := claims.AccountID
		if n, perr := parseSubjectID(claims.Subject); perr == nil && n > 0 {
			actor = n
		}
		rule, err := store.Announce(r.Context(), deprecation.Rule{
			Method: body.Method, Path: body.Path, MatchPrefix: body.MatchPrefix,
			AnnouncedAt: announced, SunsetAt: sunset,
			Replacement: body.Replacement, Notice: body.Notice,
			CreatedBy: actor,
		})
		switch {
		case err != nil && strings.HasPrefix(err.Error(), "deprecation:"):
			WriteError(w, "INVALID_REQUEST",
				err.Error(), gateway.RequestIDFrom(r.Context()), nil)
		case err != nil:
			WriteError(w, "SERVICE_DEGRADED",
				"deprecation store unavailable", gateway.RequestIDFrom(r.Context()), nil)
		default:
			WriteJSON(w, http.StatusCreated, rule)
		}
	}
	list = func(w http.ResponseWriter, r *http.Request) {
		if requireAdmin(w, r) == nil {
			return
		}
		rules, err := store.List(r.Context())
		if err != nil {
			WriteError(w, "SERVICE_DEGRADED",
				"deprecation store unavailable", gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		WriteJSON(w, http.StatusOK, map[string]any{"deprecations": rules})
	}
	return announce, list
}

// ---------------------------------------------------------------------------
// GET /developer/migration — the public migration guide.
// ---------------------------------------------------------------------------

// MigrationGuide serves the deprecation migration guide: policy text +
// the live rule table (announced → sunset per endpoint, replacement).
// src may be nil (static policy text only).
func MigrationGuide(src deprecation.Rules) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var b strings.Builder
		b.WriteString(`<!doctype html><html lang="en"><head><meta charset="utf-8">
<title>API Deprecation &amp; Migration Guide</title>
<style>body{font-family:system-ui,sans-serif;max-width:60rem;margin:2rem auto;padding:0 1rem;color:#1c2330}
table{border-collapse:collapse;width:100%}td,th{border:1px solid #d5dbe5;padding:.4rem .6rem;text-align:left;font-size:.9rem}
code{background:#f0f3f8;padding:.1rem .3rem;border-radius:3px}</style></head><body>
<h1>API Deprecation &amp; Migration Guide</h1>
<h2>Policy (Task 5.3.20, spec §8.6)</h2>
<ul>
<li><strong>Six-month notice.</strong> An endpoint announced for deprecation
    remains fully supported for at least six months.</li>
<li><strong>Headers.</strong> Deprecated endpoints answer with
    <code>Deprecation</code> (announce timestamp), <code>Sunset</code>
    (HTTP-date, RFC 8594) and <code>Link: &lt;/developer/migration&gt;;
    rel="deprecation"</code>.</li>
<li><strong>Sunset.</strong> Once <code>Sunset</code> passes the endpoint
    deterministically answers <code>410 ENDPOINT_GONE</code>. Migrate before
    that date.</li>
<li><strong>Major versions.</strong> A new major API version runs in
    parallel; <code>/api/v1</code> stays available for the whole notice
    window.</li>
</ul>
`)
		if src != nil {
			rules, err := src.Active(r.Context())
			if err == nil && len(rules) > 0 {
				b.WriteString("<h2>Announced deprecations</h2><table><tr><th>Method</th><th>Path</th><th>Announced</th><th>Sunset</th><th>Replacement</th><th>Notice</th></tr>\n")
				for _, rule := range rules {
					m := "*"
					if rule.Method != nil {
						m = *rule.Method
					}
					repl := ""
					if rule.Replacement != nil {
						repl = *rule.Replacement
					}
					notice := ""
					if rule.Notice != nil {
						notice = *rule.Notice
					}
					fmt.Fprintf(&b, "<tr><td>%s</td><td><code>%s</code></td><td>%s</td><td>%s</td><td><code>%s</code></td><td>%s</td></tr>\n",
						html.EscapeString(m), html.EscapeString(rule.Path),
						rule.AnnouncedAt.UTC().Format("2006-01-02"),
						rule.SunsetAt.UTC().Format("2006-01-02"),
						html.EscapeString(repl), html.EscapeString(notice))
				}
				b.WriteString("</table>\n")
			}
		}
		b.WriteString(`<h2>What to do</h2>
<p>Watch for the <code>Deprecation</code>/<code>Sunset</code> headers on
responses, fetch <code>GET /api/v1/openapi.json</code> for the current
contract, and file a support ticket if no replacement is listed.</p>
</body></html>`)
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(b.String()))
	}
}
