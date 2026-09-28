package ws

import (
	"context"
	"testing"
	"time"
)

func TestPayloadHashAndDedupKeyShape(t *testing.T) {
	a := PayloadHash("order.place", []byte(`{"x":1}`))
	b := PayloadHash("order.place", []byte(`{"x":1}`))
	if a != b {
		t.Fatal("payload hash not deterministic")
	}
	if PayloadHash("order.place", []byte(`{"x":2}`)) == a {
		t.Fatal("payload hash ignores params")
	}
	if PayloadHash("order.cancel", []byte(`{"x":1}`)) == a {
		t.Fatal("payload hash ignores action")
	}
	if got := DedupKey("a:42", "req-1"); got != "idem:ws:a:42:req-1" {
		t.Fatalf("dedup key shape: %q", got)
	}
}

func TestMemDedupStoreStates(t *testing.T) {
	ctx := context.Background()
	s := NewMemDedupStore()
	key, ph := DedupKey("a:1", "r1"), "ph1"

	res, err := s.Begin(ctx, key, ph, time.Minute)
	if err != nil || res.State != DedupProceed {
		t.Fatalf("first begin: %+v %v", res, err)
	}
	// Same key+payload while pending → in-flight.
	res, err = s.Begin(ctx, key, ph, time.Minute)
	if err != nil || res.State != DedupInFlight {
		t.Fatalf("in-flight begin: %+v %v", res, err)
	}
	// Same key, different payload → mismatch.
	res, err = s.Begin(ctx, key, "ph2", time.Minute)
	if err != nil || res.State != DedupMismatch {
		t.Fatalf("mismatch begin: %+v %v", res, err)
	}
	// Completing with a frame stores it for verbatim replay.
	if err := s.Complete(ctx, key, ph, []byte(`{"type":"response"}`)); err != nil {
		t.Fatalf("complete: %v", err)
	}
	res, err = s.Begin(ctx, key, ph, time.Minute)
	if err != nil || res.State != DedupReplay {
		t.Fatalf("replay begin: %+v %v", res, err)
	}
	if string(res.Replay) != `{"type":"response"}` {
		t.Fatalf("replay body: %q", res.Replay)
	}
}

func TestMemDedupStoreNilCompleteReleasesClaim(t *testing.T) {
	ctx := context.Background()
	s := NewMemDedupStore()
	key, ph := DedupKey("a:1", "r2"), "ph"

	if _, err := s.Begin(ctx, key, ph, time.Minute); err != nil {
		t.Fatal(err)
	}
	// nil frame = transient failure → slot released so retry proceeds.
	if err := s.Complete(ctx, key, ph, nil); err != nil {
		t.Fatal(err)
	}
	res, err := s.Begin(ctx, key, ph, time.Minute)
	if err != nil || res.State != DedupProceed {
		t.Fatalf("retry after release: %+v %v", res, err)
	}
	// Completing a released/missing claim errors.
	if err := s.Complete(ctx, DedupKey("a:1", "nope"), ph, nil); err == nil {
		t.Fatal("complete on missing claim should fail")
	}
}

func TestMemDedupStoreExpiry(t *testing.T) {
	ctx := context.Background()
	s := NewMemDedupStore()
	now := time.Now()
	s.now = func() time.Time { return now }
	key, ph := DedupKey("a:1", "r3"), "ph"

	if _, err := s.Begin(ctx, key, ph, time.Second); err != nil {
		t.Fatal(err)
	}
	now = now.Add(2 * time.Second) // past TTL
	res, err := s.Begin(ctx, key, "ph2", time.Minute)
	if err != nil || res.State != DedupProceed {
		t.Fatalf("expired claim should proceed: %+v %v", res, err)
	}
}

func TestSessionScopeMatrix(t *testing.T) {
	// Full user JWT (no explicit scopes, not via API key) → all scopes.
	jwt := Session{Authenticated: true}
	if !jwt.hasScope("trade") || !jwt.hasScope("read") {
		t.Fatal("scope-less JWT session should be full-access")
	}
	// API-key session with empty scope list → nothing.
	ak := Session{Authenticated: true, ViaAPIKey: true}
	if ak.hasScope("trade") {
		t.Fatal("API-key session with no scopes must not trade")
	}
	// Explicit scope list is strictly bounded.
	s := Session{Authenticated: true, ViaAPIKey: true, Scopes: []string{"read"}}
	if !s.hasScope("read") || s.hasScope("trade") {
		t.Fatal("explicit scope list not enforced")
	}
}

func TestDedupNamespaceAccountScoped(t *testing.T) {
	if got := (&Session{AccountID: 7, Subject: "u"}).dedupNamespace(); got != "a:7" {
		t.Fatalf("account namespace: %q", got)
	}
	if got := (&Session{Subject: "u-1"}).dedupNamespace(); got != "u:u-1" {
		t.Fatalf("subject namespace: %q", got)
	}
}

func TestParseFrameAndChannelList(t *testing.T) {
	f, err := parseFrame([]byte(`{"action":"subscribe","channels":["public:ticker"]}`))
	if err != nil {
		t.Fatal(err)
	}
	ch, err := f.channelList()
	if err != nil || len(ch) != 1 || ch[0] != "public:ticker" {
		t.Fatalf("channels list: %v %v", ch, err)
	}
	// params-as-array variant
	f, _ = parseFrame([]byte(`{"action":"subscribe","params":["a","b"]}`))
	ch, err = f.channelList()
	if err != nil || len(ch) != 2 {
		t.Fatalf("params array: %v %v", ch, err)
	}
	// params-as-object variant
	f, _ = parseFrame([]byte(`{"action":"subscribe","params":{"channel":"c1"}}`))
	ch, err = f.channelList()
	if err != nil || len(ch) != 1 || ch[0] != "c1" {
		t.Fatalf("params object: %v %v", ch, err)
	}
	// malformed JSON
	if _, err := parseFrame([]byte(`{`)); err == nil {
		t.Fatal("bad JSON must fail")
	}
}

func TestChannelNameValidation(t *testing.T) {
	for _, ok := range []string{"public:ticker@EURUSD", "private:orders", "a-b_c.d"} {
		if !channelNameOK(ok) {
			t.Fatalf("valid channel rejected: %q", ok)
		}
	}
	for _, bad := range []string{"", "has space", "semi;colon", string(make([]byte, 65))} {
		if channelNameOK(bad) {
			t.Fatalf("invalid channel accepted: %q", bad)
		}
	}
}
