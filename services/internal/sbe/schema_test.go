// Schema-lifecycle and negotiation tests (Task 6.3.18): accept / reject /
// sunset paths, the six-month deprecation floor, machine-readable lifecycle
// document, REST Accept and WS handshake surfaces.
package sbe

import (
	stderrors "errors"
	"net/http"
	"testing"
	"time"

	excerrors "exchange/pkg/errors"
)

var t0 = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

func codeOf(err error) string {
	var e *excerrors.Error
	if stderrors.As(err, &e) {
		return e.Code
	}
	return ""
}

func TestNegotiateActive(t *testing.T) {
	r := DefaultRegistry()
	neg, err := r.Negotiate(SchemaIDMarketData, 1, t0)
	if err != nil {
		t.Fatal(err)
	}
	if neg.Deprecated || neg.SchemaID != 1 || neg.Version != 1 {
		t.Fatalf("bad negotiation: %+v", neg)
	}
}

func TestNegotiateUnknownSchemaAndVersion(t *testing.T) {
	r := DefaultRegistry()
	for _, tc := range [][2]uint16{{99, 1}, {1, 99}} {
		_, err := r.Negotiate(tc[0], tc[1], t0)
		if codeOf(err) != CodeUnsupportedProtocolVersion {
			t.Fatalf("schema=%d ver=%d: err=%v, want UNSUPPORTED_PROTOCOL_VERSION", tc[0], tc[1], err)
		}
	}
}

func TestNegotiateDeprecatedWarnsWithinWindow(t *testing.T) {
	r := NewRegistry()
	dep := t0
	sunset := dep.Add(MinDeprecationWindow + 24*time.Hour)
	if err := r.Register(SchemaVersion{SchemaID: 1, Version: 1, State: LifecycleDeprecated,
		DeprecatedAt: dep, RetiresAt: sunset}); err != nil {
		t.Fatal(err)
	}
	if err := r.Register(SchemaVersion{SchemaID: 1, Version: 2, State: LifecycleActive}); err != nil {
		t.Fatal(err)
	}
	// Inside the window: accept with deprecation warning + sunset.
	neg, err := r.Negotiate(1, 1, dep.Add(30*24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if !neg.Deprecated || neg.Sunset.IsZero() || neg.Warning == "" {
		t.Fatalf("deprecated negotiation missing warning/sunset: %+v", neg)
	}
	// Past sunset: explicit failure, SBE_SCHEMA_RETIRED (§23, 400).
	_, err = r.Negotiate(1, 1, sunset.Add(time.Second))
	if codeOf(err) != CodeSBESchemaRetired {
		t.Fatalf("err=%v, want SBE_SCHEMA_RETIRED", err)
	}
}

func TestNegotiateRetiredFailsExplicitly(t *testing.T) {
	r := NewRegistry()
	if err := r.Register(SchemaVersion{SchemaID: 1, Version: 1, State: LifecycleRetired}); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Negotiate(1, 1, t0); codeOf(err) != CodeSBESchemaRetired {
		t.Fatalf("err=%v, want SBE_SCHEMA_RETIRED", err)
	}
}

func TestRegistryEnforcesSixMonthWindow(t *testing.T) {
	r := NewRegistry()
	err := r.Register(SchemaVersion{SchemaID: 1, Version: 1, State: LifecycleDeprecated,
		DeprecatedAt: t0, RetiresAt: t0.Add(30 * 24 * time.Hour)}) // 30d < 6mo
	if err == nil {
		t.Fatal("registry accepted a sub-six-month sunset — §24 #284 violated")
	}
}

func TestLifecycleDocumentMachineReadable(t *testing.T) {
	r := NewRegistry()
	_ = r.Register(SchemaVersion{SchemaID: 1, Version: 1, State: LifecycleDeprecated,
		DeprecatedAt: t0, RetiresAt: t0.Add(MinDeprecationWindow)})
	_ = r.Register(SchemaVersion{SchemaID: 1, Version: 2, State: LifecycleActive})
	doc := r.Document(t0.Add(24 * time.Hour))
	if len(doc.Schemas) != 2 {
		t.Fatalf("doc rows=%d, want 2", len(doc.Schemas))
	}
	if doc.Schemas[0].State != LifecycleDeprecated || doc.Schemas[1].State != LifecycleActive {
		t.Fatalf("doc states wrong: %+v", doc.Schemas)
	}
	// Past the sunset the same row reports retired — time-driven.
	doc2 := r.Document(t0.Add(MinDeprecationWindow + 48*time.Hour))
	if doc2.Schemas[0].State != LifecycleRetired {
		t.Fatalf("post-sunset state=%q, want retired", doc2.Schemas[0].State)
	}
}

func TestParseSBEAccept(t *testing.T) {
	ref, ok, err := ParseSBEAccept("application/sbe; schema=1; version=2")
	if err != nil || !ok {
		t.Fatalf("parse: ok=%v err=%v", ok, err)
	}
	if ref.SchemaID != 1 || ref.Version != 2 {
		t.Fatalf("ref=%+v", ref)
	}
	// JSON default: no SBE media range → ok=false.
	if _, ok, _ := ParseSBEAccept("application/json"); ok {
		t.Fatal("JSON accept wrongly claimed SBE")
	}
	if _, ok, _ := ParseSBEAccept("text/html, application/sbe;version=1"); !ok {
		t.Fatal("multi-range accept not parsed")
	}
	if _, _, err := ParseSBEAccept("application/sbe;schema=abc"); err == nil {
		t.Fatal("malformed param accepted")
	}
}

func TestNegotiateREST_HeadersAndErrors(t *testing.T) {
	r := NewRegistry()
	_ = r.Register(SchemaVersion{SchemaID: 1, Version: 1, State: LifecycleDeprecated,
		DeprecatedAt: t0, RetiresAt: t0.Add(MinDeprecationWindow)})
	now := t0.Add(24 * time.Hour)

	neg, ok, err := r.NegotiateREST("application/sbe;schema=1;version=1", now)
	if err != nil || !ok {
		t.Fatalf("negotiate: ok=%v err=%v", ok, err)
	}
	h := http.Header{}
	neg.ApplyHTTPHeaders(h)
	if h.Get("Content-Type") != MediaTypeSBE {
		t.Fatal("missing content-type")
	}
	if h.Get(HeaderSBESchemaID) != "1" || h.Get(HeaderSBESchemaVersion) != "1" {
		t.Fatal("missing schema echo headers")
	}
	if h.Get(HeaderSunset) == "" || h.Get(HeaderDeprecation) == "" {
		t.Fatal("deprecated schema missing Sunset/Deprecation headers")
	}

	// Unknown version → UNSUPPORTED_PROTOCOL_VERSION.
	if _, _, err := r.NegotiateREST("application/sbe;schema=1;version=9", now); codeOf(err) != CodeUnsupportedProtocolVersion {
		t.Fatalf("err=%v", err)
	}
	// JSON request → ok=false, no error.
	if _, ok, err := r.NegotiateREST("application/json", now); ok || err != nil {
		t.Fatalf("json path: ok=%v err=%v", ok, err)
	}
}

func TestNegotiateWS(t *testing.T) {
	r := DefaultRegistry()
	if _, err := r.NegotiateWS(WSNegotiateRequest{SchemaID: 1, Version: 1}, t0); err != nil {
		t.Fatal(err)
	}
	if _, err := r.NegotiateWS(WSNegotiateRequest{SchemaID: 2, Version: 1}, t0); codeOf(err) != CodeUnsupportedProtocolVersion {
		t.Fatalf("err=%v, want UNSUPPORTED_PROTOCOL_VERSION", err)
	}
}
