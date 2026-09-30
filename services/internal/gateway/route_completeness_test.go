package gateway

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// Task 5.3.46 — registry-vs-plan path cross-check (extends Task 5.3.7 step 6).
//
// Every HTTP/WS path declared in the phase plans or the master specification
// must appear in SeedRoutes(). Declared paths are extracted from backticked
// tokens and "METHOD /path" forms, then normalized:
//
//   - "?query" suffixes, trailing punctuation, and "[/optional]" markdown
//     segments are stripped;
//   - "{a,b}" brace alternates and "x|y" pipe alternates are expanded;
//   - "{param}", "<param>", and ":param" path parameters collapse to "{}".
//
// Declarations that are prose rather than routes (version prefixes, namespace
// references, deliberately-canonicalized aliases, and non-gateway infra probe
// paths) are excluded via declaredPathIsRoute + allowlistedExceptions.

var (
	declaredMethodPath = regexp.MustCompile(`\b(?:GET|POST|PUT|PATCH|DELETE|HEAD|WS)\s+(/[^\s` + "`" + `|]*)`)
	declaredTickedPath = regexp.MustCompile("`(/[^`]+)`")
	pathParamBrace     = regexp.MustCompile(`\{[^}]*\}`)
	pathParamAngle     = regexp.MustCompile(`<[^>]*>`)
	pathParamColon     = regexp.MustCompile(`:[a-zA-Z_][a-zA-Z0-9_]*`)
	braceAlternates    = regexp.MustCompile(`\{([^{}]*,[^{}]*)\}`)
)

// allowlistedExceptions are declared paths that are intentionally NOT distinct
// registry rows. Each entry must carry a justification.
var allowlistedExceptions = map[string]string{
	"/api/v1":                      "version prefix in prose, not an endpoint",
	"/api/v2":                      "version prefix in prose, not an endpoint",
	"/api/v3":                      "hypothetical future major in API-MIGRATION-GUIDE prose, not an endpoint",
	"/api/v{}":                     "templated version prefix in API-MIGRATION-GUIDE prose, not an endpoint",
	"/api/v{}/openapi.json":        "templated per-major spec URL in API-MIGRATION-GUIDE prose; concretes are registered",
	"/api/v1/analytics":            "namespace prose; concrete endpoints under it are registered",
	"/api/v1/market-data/ticks/{}": "deliberately canonicalized to /api/v1/history/ticks/{symbol} (Task 23.3.4)",
	"/healthz":                     "watchdogd/engine-binary probe path, not gateway client surface",
}

// declaredPathIsRoute filters declarations to the gateway-served surfaces
// (Task 5.3.46 covers §8.4/§12/§21 client routes plus ops aliases).
func declaredPathIsRoute(p string) bool {
	switch {
	case strings.HasPrefix(p, "/api/"),
		strings.HasPrefix(p, "/ws"),
		strings.HasPrefix(p, "/developer"),
		strings.HasPrefix(p, "/health"):
		return true
	case p == "/ready":
		return true
	}
	return false
}

// normalizeDeclaredPath reduces a declared path token to canonical registry
// form. Returns "" when the token is not a real route declaration.
func normalizeDeclaredPath(raw string) string {
	p := raw
	if i := strings.IndexByte(p, '?'); i >= 0 {
		p = p[:i]
	}
	if i := strings.IndexByte(p, '['); i >= 0 {
		p = p[:i]
	}
	p = strings.TrimRight(p, "/.,;:)\"'`")
	if p == "" || strings.HasSuffix(p, "*") {
		return ""
	}
	return p
}

// expandAlternates expands "{a,b}" and trailing "x|y" alternates into the set
// of concrete paths they denote.
func expandAlternates(p string) []string {
	out := []string{p}
	// {a,b} brace alternates.
	for {
		var next []string
		grew := false
		for _, s := range out {
			m := braceAlternates.FindStringSubmatchIndex(s)
			if m == nil {
				next = append(next, s)
				continue
			}
			grew = true
			for _, alt := range strings.Split(s[m[2]:m[3]], ",") {
				next = append(next, s[:m[0]]+alt+s[m[1]:])
			}
		}
		out = next
		if !grew {
			break
		}
	}
	// a|b|c in the final path segment (e.g. ".../approve|reject").
	var expanded []string
	for _, s := range out {
		last := s[strings.LastIndexByte(s, '/')+1:]
		if !strings.Contains(last, "|") {
			expanded = append(expanded, s)
			continue
		}
		base := s[:len(s)-len(last)]
		for _, alt := range strings.Split(last, "|") {
			expanded = append(expanded, base+alt)
		}
	}
	return expanded
}

// canonicalPath collapses parameter spellings to "{}" for set comparison.
func canonicalPath(p string) string {
	p = pathParamBrace.ReplaceAllString(p, "{}")
	p = pathParamAngle.ReplaceAllString(p, "{}")
	p = pathParamColon.ReplaceAllString(p, "{}")
	return strings.TrimRight(p, "/")
}

func declaredPathsFromDocs(t *testing.T) map[string]string {
	t.Helper()
	docsDir := filepath.Join("..", "..", "..", "docs")
	files, err := filepath.Glob(filepath.Join(docsDir, "*.md"))
	if err != nil || len(files) == 0 {
		t.Fatalf("phase docs not found at %s: %v", docsDir, err)
	}
	declared := map[string]string{} // canonical → first raw spelling seen
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("read %s: %v", f, err)
		}
		body := string(b)
		var raw []string
		for _, m := range declaredMethodPath.FindAllStringSubmatch(body, -1) {
			raw = append(raw, m[1])
		}
		for _, m := range declaredTickedPath.FindAllStringSubmatch(body, -1) {
			raw = append(raw, m[1])
		}
		for _, r := range raw {
			p := normalizeDeclaredPath(r)
			if p == "" || !declaredPathIsRoute(p) {
				continue
			}
			for _, e := range expandAlternates(p) {
				c := canonicalPath(e)
				if _, ok := declared[c]; !ok {
					declared[c] = filepath.Base(f) + ": " + e
				}
			}
		}
	}
	return declared
}

func TestRouteRegistryCoversDeclaredPaths(t *testing.T) {
	registered := map[string]bool{}
	for _, r := range SeedRoutes() {
		registered[canonicalPath(r.Path)] = true
	}
	declared := declaredPathsFromDocs(t)
	if len(declared) < 100 {
		t.Fatalf("only %d declared paths extracted — doc scan broken", len(declared))
	}
	var missing []string
	for canon, src := range declared {
		if registered[canon] {
			continue
		}
		if why, ok := allowlistedExceptions[canon]; ok {
			_ = why // documented exclusion
			continue
		}
		missing = append(missing, canon+" ("+src+")")
	}
	if len(missing) > 0 {
		t.Fatalf("%d declared paths not registered:\n  %s",
			len(missing), strings.Join(missing, "\n  "))
	}
}
