// Negotiation handshakes for Task 6.3.18 items 1–2:
//
//	REST: client sends `Accept: application/sbe; schema=<id>; version=<n>`
//	      (absence → JSON default — spec §22.4/§24 #284 "JSON remains
//	      default"). Response carries Content-Type: application/sbe plus
//	      X-SBE-Schema-Id / X-SBE-Schema-Version echoes; deprecated schemas
//	      additionally carry Deprecation + RFC 8594 Sunset headers
//	      (consistent with middleware/versioning.go).
//	WS:   during the §10.5 authenticate handshake the client may include a
//	      "sbe": {"schema_id": n, "version": n} object selecting SBE
//	      encoding for public, interactive-trading and private event
//	      payloads. Failures render as the standard WS error envelope
//	      {"type":"error","error":"<CODE>",...} (§10.5 item 4).
//
// Both surfaces resolve through Registry.Negotiate so the accept/deprecate/
// reject semantics are identical everywhere.
package sbe

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	excerrors "exchange/pkg/errors"
)

// MediaTypeSBE is the negotiated content type.
const MediaTypeSBE = "application/sbe"

// Negotiated-response headers.
const (
	HeaderSBESchemaID      = "X-SBE-Schema-Id"
	HeaderSBESchemaVersion = "X-SBE-Schema-Version"
	HeaderDeprecation      = "Deprecation" // HTTP-date per Phase-05 Task 5.3.20
	HeaderSunset           = "Sunset"      // RFC 8594 HTTP-date
	HeaderWarning          = "Warning"     // RFC 9110 warning header
)

// SchemaRef is a requested {schemaID, version} pair.
type SchemaRef struct {
	SchemaID uint16 `json:"schema_id"`
	Version  uint16 `json:"version"`
}

// ParseSBEAccept extracts a schema ref from an HTTP Accept header.
// Returns ok=false when the header does not request SBE (JSON default);
// a malformed SBE media-range returns an error.
func ParseSBEAccept(accept string) (SchemaRef, bool, error) {
	var ref SchemaRef
	for _, part := range strings.Split(accept, ",") {
		part = strings.TrimSpace(part)
		segs := strings.Split(part, ";")
		media := strings.TrimSpace(segs[0])
		if !strings.EqualFold(media, MediaTypeSBE) {
			continue
		}
		for _, p := range segs[1:] {
			kv := strings.SplitN(strings.TrimSpace(p), "=", 2)
			if len(kv) != 2 {
				return ref, false, fmt.Errorf("sbe: malformed accept param %q", p)
			}
			n, err := strconv.ParseUint(strings.TrimSpace(kv[1]), 10, 16)
			if err != nil {
				return ref, false, fmt.Errorf("sbe: malformed accept param %q: %w", p, err)
			}
			switch strings.ToLower(strings.TrimSpace(kv[0])) {
			case "schema":
				ref.SchemaID = uint16(n)
			case "version":
				ref.Version = uint16(n)
			}
		}
		return ref, true, nil
	}
	return ref, false, nil
}

// NegotiateREST resolves an HTTP Accept header against the registry.
// (ok=false, nil) means the client did not ask for SBE — fall back to JSON.
func (r *Registry) NegotiateREST(accept string, now time.Time) (neg *Negotiation, ok bool, err error) {
	ref, wants, err := ParseSBEAccept(accept)
	if err != nil {
		return nil, false, excerrors.New(CodeUnsupportedProtocolVersion, err.Error())
	}
	if !wants {
		return nil, false, nil
	}
	neg, err = r.Negotiate(ref.SchemaID, ref.Version, now)
	if err != nil {
		return nil, true, err
	}
	return neg, true, nil
}

// WSNegotiateRequest is the SBE block of the §10.5 authenticate frame:
// {"action":"authenticate", ..., "sbe": {"schema_id":1,"version":1}}.
type WSNegotiateRequest struct {
	SchemaID uint16 `json:"schema_id"`
	Version  uint16 `json:"version"`
}

// NegotiateWS resolves a WS handshake request. The caller renders errors
// into the §10.5 envelope — UNSUPPORTED_PROTOCOL_VERSION or
// SBE_SCHEMA_RETIRED.
func (r *Registry) NegotiateWS(req WSNegotiateRequest, now time.Time) (*Negotiation, error) {
	return r.Negotiate(req.SchemaID, req.Version, now)
}

// ApplyHTTPHeaders writes the negotiated encoding headers onto h
// (Content-Type, X-SBE-Schema-Id/Version, Deprecation/Sunset/Warning).
func (n *Negotiation) ApplyHTTPHeaders(h http.Header) {
	h.Set("Content-Type", MediaTypeSBE)
	h.Set(HeaderSBESchemaID, strconv.Itoa(int(n.SchemaID)))
	h.Set(HeaderSBESchemaVersion, strconv.Itoa(int(n.Version)))
	if n.Deprecated {
		httpDate := n.Sunset.UTC().Format(http.TimeFormat)
		h.Set(HeaderDeprecation, httpDate)
		h.Set(HeaderSunset, httpDate)
		if n.Warning != "" {
			h.Set(HeaderWarning, `299 - "`+n.Warning+`"`)
		}
	}
}
