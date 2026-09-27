// Task 1.3.12 — severity hierarchy, §23 code constants, and RFC 7807
// problem-details envelopes (spec §2.7, §3.6).
package errors

import (
	"encoding/json"
	stderrors "errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestSeverityConstantsAndOrdering(t *testing.T) {
	// L0..L3 ordered most→least severe (spec §2.7.2).
	if !(SeverityL0 < SeverityL1 && SeverityL1 < SeverityL2 && SeverityL2 < SeverityL3) {
		t.Fatalf("severity ordering broken: %v %v %v %v",
			SeverityL0, SeverityL1, SeverityL2, SeverityL3)
	}
	for _, s := range []Severity{SeverityL0, SeverityL1, SeverityL2, SeverityL3} {
		if !s.Valid() {
			t.Fatalf("tier %v should be valid", s)
		}
	}
	if Severity(-1).Valid() || Severity(4).Valid() {
		t.Fatal("out-of-range severity reported valid")
	}
}

func TestSeverityStrings(t *testing.T) {
	cases := []struct {
		s        Severity
		name     string
		priority string
	}{
		{SeverityL0, "L0", "P0"},
		{SeverityL1, "L1", "P1"},
		{SeverityL2, "L2", "P2"},
		{SeverityL3, "L3", "P3"},
		{Severity(9), "L?", "P?"},
	}
	for _, c := range cases {
		if got := c.s.String(); got != c.name {
			t.Errorf("Severity(%d).String() = %q, want %q", int(c.s), got, c.name)
		}
		if got := c.s.Priority(); got != c.priority {
			t.Errorf("Severity(%d).Priority() = %q, want %q", int(c.s), got, c.priority)
		}
	}
}

func TestSeverityFatalOnlyL0(t *testing.T) {
	if !SeverityL0.Fatal() {
		t.Fatal("L0 must be fatal (spec §2.7.2: immediate core halt)")
	}
	for _, s := range []Severity{SeverityL1, SeverityL2, SeverityL3} {
		if s.Fatal() {
			t.Fatalf("%v must not be fatal", s)
		}
	}
}

func TestSeverityForKnownCodes(t *testing.T) {
	if got := SeverityFor(CodeTimeSyncLossHalt); got != SeverityL0 {
		t.Fatalf("TIME_SYNC_LOSS_HALT severity = %v, want L0", got)
	}
	for _, code := range []string{CodeArithmeticOverflowDetected, CodeOrderBookCapacityExceeded} {
		if got := SeverityFor(code); got != SeverityL2 {
			t.Fatalf("%s severity = %v, want L2", code, got)
		}
	}
	// Fail-closed default: unknown codes are transaction rejects, never L3.
	if got := SeverityFor("SOME_UNREGISTERED_CODE"); got != SeverityL2 {
		t.Fatalf("unknown code severity = %v, want L2", got)
	}
}

func TestSeverityOfError(t *testing.T) {
	err := Wrap(CodeOrderBookCapacityExceeded, "pool exhausted", nil)
	if got := SeverityOf(err); got != SeverityL2 {
		t.Fatalf("SeverityOf = %v, want L2", got)
	}
	if got := SeverityOf(fmt.Errorf("plain")); got != SeverityL2 {
		t.Fatalf("SeverityOf(plain) = %v, want L2", got)
	}
	// Unwraps through fmt %w chains.
	wrapped := fmt.Errorf("outer: %w", New(CodeTimeSyncLossHalt, "drift"))
	if got := SeverityOf(wrapped); got != SeverityL0 {
		t.Fatalf("SeverityOf(wrapped) = %v, want L0", got)
	}
}

func TestHTTPStatusMapping(t *testing.T) {
	cases := map[string]int{
		CodeArithmeticOverflowDetected: http.StatusBadRequest,
		CodeOrderBookCapacityExceeded:  http.StatusServiceUnavailable,
		CodeTimeSyncLossHalt:           http.StatusServiceUnavailable,
	}
	for code, want := range cases {
		if got := HTTPStatus(code); got != want {
			t.Errorf("HTTPStatus(%q) = %d, want %d", code, got, want)
		}
	}
	if got := HTTPStatus("NOPE"); got != http.StatusInternalServerError {
		t.Fatalf("unknown code status = %d, want 500", got)
	}
	if got := HTTPStatusOf(New(CodeArithmeticOverflowDetected, "x")); got != 400 {
		t.Fatalf("HTTPStatusOf = %d, want 400", got)
	}
	if got := HTTPStatusOf(stderrors.New("plain")); got != 500 {
		t.Fatalf("HTTPStatusOf(plain) = %d, want 500", got)
	}
}

func TestProblemForCodedError(t *testing.T) {
	err := New(CodeOrderBookCapacityExceeded, "order book capacity exceeded")
	p := ProblemFor(err)
	if p.Code != CodeOrderBookCapacityExceeded {
		t.Fatalf("Code = %q", p.Code)
	}
	if p.Status != http.StatusServiceUnavailable {
		t.Fatalf("Status = %d, want 503", p.Status)
	}
	if p.Type != "urn:exc:problem:"+CodeOrderBookCapacityExceeded {
		t.Fatalf("Type = %q", p.Type)
	}
	if p.Detail != "order book capacity exceeded" {
		t.Fatalf("Detail = %q", p.Detail)
	}
}

func TestProblemForPlainErrorIsOpaque(t *testing.T) {
	p := ProblemFor(stderrors.New("db connection string leaked here"))
	if p.Status != http.StatusInternalServerError || p.Code != "INTERNAL_ERROR" {
		t.Fatalf("got %+v, want generic 500", p)
	}
	if p.Detail != "" {
		t.Fatal("internal detail must not leak into the envelope")
	}
}

func TestWriteProblemRFC7807(t *testing.T) {
	rec := httptest.NewRecorder()
	WriteProblem(rec, New(CodeArithmeticOverflowDetected, "mantissa overflow"))

	res := rec.Result()
	if ct := res.Header.Get("Content-Type"); ct != ProblemMediaType {
		t.Fatalf("Content-Type = %q, want %q", ct, ProblemMediaType)
	}
	if res.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", res.StatusCode)
	}
	var body map[string]any
	if err := json.NewDecoder(res.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body["code"] != CodeArithmeticOverflowDetected {
		t.Fatalf("code = %v", body["code"])
	}
	if body["status"] != float64(400) {
		t.Fatalf("status field = %v", body["status"])
	}
	if _, ok := body["type"]; !ok {
		t.Fatal("missing RFC 7807 type member")
	}
}
