// Task 5.3.28 — API versioning middleware (spec §8.6, §24 #208).
//
// REST contract: major version lives in the path (/api/v{n}/...); the
// response carries the semantic build version in X-API-Version, and
// deprecated endpoints additionally emit `Deprecation` and RFC 8594
// `Sunset` headers matching the 6-month deprecation policy (Task 5.3.20;
// major versions get the doubled 12-month window per §8.6).
//
// Unknown major versions are rejected with UNSUPPORTED_PROTOCOL_VERSION
// (§23, 400) — the same code the WS auth frame uses for an unknown
// protocol_version, surfaced identically on REST for one client-facing
// contract.
package middleware

import (
	"context"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	excerrors "exchange/pkg/errors"
)

// VersionHeader carries the semantic API version on every response.
const VersionHeader = "X-API-Version"

// DeprecatedFieldHeader flags fields scheduled for removal (§8.6).
const DeprecatedFieldHeader = "X-Deprecated-Field"

// SunsetHeader is the RFC 8594 header for deprecated endpoints.
const SunsetHeader = "Sunset"

// DeprecationHeader marks a response as deprecated (HTTP-date form per
// Task 5.3.20's "Deprecation header with date" wording).
const DeprecationHeader = "Deprecation"

// CodeUnsupportedProtocolVersion is the §23 code for unknown versions
// (registered: Phase-05 Task 5.3.28).
const CodeUnsupportedProtocolVersion = "UNSUPPORTED_PROTOCOL_VERSION" // 400

type ctxKeyAPIVersion struct{}

// DeprecatedRoute marks a path prefix as sunset at a fixed instant.
type DeprecatedRoute struct {
	PathPrefix string    // e.g. "/api/v1/legacy"
	Sunset     time.Time // RFC 8594 sunset instant (HTTP-date rendered)
}

// VersionConfig configures APIVersion middleware.
type VersionConfig struct {
	// Versions maps supported major → semantic version string emitted in
	// X-API-Version (e.g. {1: "1.2.3"}). Requests for other majors are
	// rejected UNSUPPORTED_PROTOCOL_VERSION (400). An empty map rejects
	// nothing and emits no header (pre-versioned scaffold).
	Versions map[int]string
	// Deprecated lists sunset routes; matching is by path prefix.
	Deprecated []DeprecatedRoute
	// Emit writes rejection envelopes — gateway.Router.WriteError wired
	// in production; nil falls back to the local writer.
	Emit ErrorEmitter
}

var apiPathRe = regexp.MustCompile(`^/api/v(\d+)(/|$)`)

// APIVersion returns middleware that negotiates the path major version,
// emits X-API-Version, and stamps Sunset/Deprecation on sunset routes.
func APIVersion(cfg VersionConfig) func(http.Handler) http.Handler {
	emit := cfg.Emit
	if emit == nil {
		emit = defaultEmit
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if m := apiPathRe.FindStringSubmatch(r.URL.Path); m != nil {
				major, _ := strconv.Atoi(m[1])
				sem, ok := cfg.Versions[major]
				if len(cfg.Versions) > 0 && !ok {
					emit(w, r, CodeUnsupportedProtocolVersion,
						"unsupported api version v"+m[1],
						map[string]any{"supported": supportedMajors(cfg.Versions)})
					return
				}
				if ok {
					w.Header().Set(VersionHeader, sem)
				}
				r = r.WithContext(context.WithValue(r.Context(), ctxKeyAPIVersion{}, major))
				for _, d := range cfg.Deprecated {
					if strings.HasPrefix(r.URL.Path, d.PathPrefix) {
						httpDate := d.Sunset.UTC().Format(http.TimeFormat)
						w.Header().Set(SunsetHeader, httpDate)
						w.Header().Set(DeprecationHeader, httpDate)
					}
				}
			}
			next.ServeHTTP(w, r)
		})
	}
}

func supportedMajors(v map[int]string) []int {
	out := make([]int, 0, len(v))
	for k := range v {
		out = append(out, k)
	}
	for i := 0; i < len(out); i++ {
		for j := i + 1; j < len(out); j++ {
			if out[j] < out[i] {
				out[i], out[j] = out[j], out[i]
			}
		}
	}
	return out
}

// APIVersionFrom returns the negotiated major version, or 0 when the
// request carried no /api/v{n}/ path.
func APIVersionFrom(ctx context.Context) int {
	v, _ := ctx.Value(ctxKeyAPIVersion{}).(int)
	return v
}

// MarkDeprecatedField emits X-Deprecated-Field for a field scheduled for
// removal within the same major version (§8.6 backward-compat contract).
func MarkDeprecatedField(w http.ResponseWriter, field string) {
	w.Header().Add(DeprecatedFieldHeader, field)
}

// WSProtocolVersion is the negotiated WS protocol major (spec §8.6).
const WSProtocolVersion = 1

// CheckWSProtocolVersion validates the auth-frame protocol_version
// (spec §8.6 WS row, §10.5 auth frame). Unknown versions return a coded
// error the WS layer renders as
// {"type":"error","error":"UNSUPPORTED_PROTOCOL_VERSION"}.
func CheckWSProtocolVersion(v int) error {
	if v != WSProtocolVersion {
		return excerrors.New(CodeUnsupportedProtocolVersion,
			"unsupported protocol_version")
	}
	return nil
}
