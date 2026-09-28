package api

import (
	"net/http/httptest"
	"testing"
	"time"
)

func TestCursorRoundTrip(t *testing.T) {
	c := Cursor{CreatedAt: time.Unix(1700000000, 123456789), ID: 98765}
	tok := EncodeCursor(c)
	got, err := DecodeCursor(tok)
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != c.ID || !got.CreatedAt.Equal(c.CreatedAt.UTC()) {
		t.Fatalf("round trip: %+v vs %+v", got, c)
	}
}

func TestDecodeCursorRejectsGarbage(t *testing.T) {
	for _, bad := range []string{
		"", "!!!not-base64", "aGVsbG8", // "hello" — no colon
		"MToxOmV4dHJh", // "1:1:extra" tolerated? parts[1] "1:extra" fails int parse
		"YWJjOjEyMw",   // "abc:123" bad ts
		"MTIzOmFiYw",   // "123:abc" bad id
	} {
		if _, err := DecodeCursor(bad); err == nil {
			t.Fatalf("accepted bad cursor %q", bad)
		}
	}
}

func TestParseListParamsDefaultsAndBounds(t *testing.T) {
	spec := &ListSpec{Path: "/x", Default: 100, Max: 500}

	r := httptest.NewRequest("GET", "/x", nil)
	p, err := ParseListParams(r, spec)
	if err != nil || p.Limit != 100 {
		t.Fatalf("default limit: %+v %v", p, err)
	}

	r = httptest.NewRequest("GET", "/x?limit=42", nil)
	p, err = ParseListParams(r, spec)
	if err != nil || p.Limit != 42 {
		t.Fatalf("explicit limit: %+v %v", p, err)
	}

	for _, q := range []string{"/x?limit=0", "/x?limit=-1", "/x?limit=501", "/x?limit=abc"} {
		r = httptest.NewRequest("GET", q, nil)
		if _, err := ParseListParams(r, spec); err == nil {
			t.Fatalf("limit accepted: %s", q)
		}
	}
}

func TestParseListParamsCursor(t *testing.T) {
	tok := EncodeCursor(Cursor{CreatedAt: time.Now(), ID: 5})
	r := httptest.NewRequest("GET", "/x?cursor="+tok, nil)
	p, err := ParseListParams(r, nil) // nil spec → 100/1000 baseline
	if err != nil || p.Decoded == nil || p.Decoded.ID != 5 {
		t.Fatalf("cursor: %+v %v", p, err)
	}

	r = httptest.NewRequest("GET", "/x?cursor=!!!", nil)
	if _, err := ParseListParams(r, nil); err == nil {
		t.Fatal("bad cursor accepted")
	}
}

func TestCursorClause(t *testing.T) {
	p := &ListParams{Limit: 10}
	clause, args := p.CursorClause(3)
	if clause != "" || args != nil {
		t.Fatal("no cursor → no clause")
	}
	p.Decoded = &Cursor{CreatedAt: time.Now(), ID: 9}
	clause, args = p.CursorClause(3)
	if clause != " AND (created_at, id) < ($3, $4)" || len(args) != 2 || args[1] != int64(9) {
		t.Fatalf("clause %q args %v", clause, args)
	}
}

func TestListEnvelopeNextCursor(t *testing.T) {
	p := &ListParams{Limit: 2}
	rows := []Cursor{
		{CreatedAt: time.Now(), ID: 3},
		{CreatedAt: time.Now(), ID: 2},
	}
	env := NewListEnvelope([]int{1, 2}, p, rows, 50)
	if env.Limit != 2 || env.Total != 50 {
		t.Fatalf("envelope: %+v", env)
	}
	if env.NextCursor == "" {
		t.Fatal("full page must emit next_cursor")
	}
	got, err := DecodeCursor(env.NextCursor)
	if err != nil || got.ID != 2 {
		t.Fatalf("next cursor decodes to last row: %v %+v", err, got)
	}

	// Short page → terminal, empty cursor.
	env = NewListEnvelope([]int{1}, p, rows[:1], 1)
	if env.NextCursor != "" {
		t.Fatalf("short page must not emit cursor: %q", env.NextCursor)
	}
}

func TestListSpecFor(t *testing.T) {
	if s := ListSpecFor("/api/v1/orders"); s == nil || s.Max != 1000 {
		t.Fatalf("orders spec: %+v", s)
	}
	if ListSpecFor("/nope") != nil {
		t.Fatal("unknown path should resolve nil")
	}
}
