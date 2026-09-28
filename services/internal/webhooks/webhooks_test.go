// Task 5.3.17 — webhook unit tests: signature known-answer, retry
// schedule, validation. PG-backed store/dispatch tests live in
// webhooks_integration_test.go.
package webhooks

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"
)

// Sign known-answer: hex(HMAC-SHA256("s3cr3t", "1700000000.<body>")).
// The ts.body concatenation order is the wire contract — a regression
// here breaks every consumer verifier.
func TestSignKAT(t *testing.T) {
	body := []byte(`{"order_id":42,"status":"FILLED"}`)
	ts := time.Unix(1700000000, 0).UTC()
	got := Sign([]byte("s3cr3t"), ts, body)
	want := "d904256d7a153946978001853361e236e72bada79389e538d78277bf9f469f15"
	if got != want {
		t.Fatalf("signature=%s want %s", got, want)
	}
	// Timestamp is bound: a different ts must produce a different sig.
	if Sign([]byte("s3cr3t"), ts.Add(time.Second), body) == got {
		t.Fatal("timestamp not bound into signature")
	}
}

// RetryPolicy is the Task 5.3.17 schedule verbatim; backoffFor maps the
// post-claim attempt count (1-based → index attempts-1) to the delay.
func TestBackoffSchedule(t *testing.T) {
	if len(RetryPolicy) != 5 {
		t.Fatalf("policy len=%d want 5", len(RetryPolicy))
	}
	want := []time.Duration{1, 2, 4, 8, 16}
	for i, w := range want {
		if RetryPolicy[i] != w*time.Second {
			t.Fatalf("policy[%d]=%v want %ds", i, RetryPolicy[i], w)
		}
	}
	// Claimed attempt 1 fails → waits 1s; attempt 5 fails → 16s (and
	// dead-letters via attempts>=max_attempts in FailDelivery).
	for attempt, wantSec := range []int{1, 2, 4, 8, 16, 16} {
		if got := backoffFor(attempt); got != time.Duration(wantSec)*time.Second {
			t.Fatalf("backoffFor(%d)=%v want %ds", attempt, got, wantSec)
		}
	}
	if backoffFor(-1) != time.Second {
		t.Fatal("negative attempt not clamped")
	}
}

func TestValidateEvents(t *testing.T) {
	if err := validateEvents([]string{"order_filled", "deposit_confirmed"}); err != nil {
		t.Fatalf("valid events rejected: %v", err)
	}
	if err := validateEvents(nil); err == nil {
		t.Fatal("empty subscription accepted")
	}
	if err := validateEvents([]string{"price_tick"}); err == nil {
		t.Fatal("unknown event accepted")
	}
	if err := validateEvents([]string{"order_filled", "order_filled"}); err == nil {
		t.Fatal("duplicate event accepted")
	}
}

func TestValidateURL(t *testing.T) {
	for _, u := range []string{"https://hooks.example.com/x",
		"http://127.0.0.1:9090/hook"} {
		if err := validateURL(u); err != nil {
			t.Fatalf("valid url %q rejected: %v", u, err)
		}
	}
	for _, u := range []string{"", "ftp://x", "https://", "http://a b",
		"https://x\ny", strings.Repeat("https://x", 400)} {
		if err := validateURL(u); err == nil {
			t.Fatalf("url %q accepted", u)
		}
	}
}

func TestSubscribedTo(t *testing.T) {
	ep := Endpoint{Events: []string{"order_filled", "order_cancelled"}}
	if !ep.SubscribedTo("order_filled") {
		t.Fatal("subscribed event missed")
	}
	if ep.SubscribedTo("deposit_confirmed") {
		t.Fatal("unsubscribed event matched")
	}
}

// Nil-box registration must fail closed before any DB write.
func TestRegisterFailsClosed(t *testing.T) {
	if _, err := NewStore(nil, fakeBox{}); err == nil {
		t.Fatal("nil pool accepted")
	}
	s := &Store{box: nil, now: time.Now}
	if _, _, err := s.Register(context.Background(), 1, 1,
		"https://hooks.example.com/x", []string{"order_filled"}); err == nil {
		t.Fatal("nil secret box accepted")
	}
	if _, err := s.RotateSecret(context.Background(), "wh_x", 1, time.Hour); err == nil {
		t.Fatal("nil-box rotate accepted")
	}
	// Overlap bounds enforced before the store write.
	s.box = fakeBox{}
	if _, err := s.RotateSecret(context.Background(), "wh_x", 1, MaxSecretOverlap+time.Second); err == nil {
		t.Fatal("overlap >72h accepted")
	}
	if _, err := s.RotateSecret(context.Background(), "wh_x", 1, -time.Second); err == nil {
		t.Fatal("negative overlap accepted")
	}
}

func TestEventsSorted(t *testing.T) {
	ev := Events()
	if len(ev) != 4 {
		t.Fatalf("events=%v", ev)
	}
	for i := 1; i < len(ev); i++ {
		if ev[i-1] >= ev[i] {
			t.Fatalf("events unsorted: %v", ev)
		}
	}
}

// fakeBox is a test SecretBox — prefix envelope, reversible.
type fakeBox struct{}

func (fakeBox) Seal(p []byte) ([]byte, error) { return append([]byte("enc:"), p...), nil }
func (fakeBox) Open(b []byte) ([]byte, error) {
	if !bytes.HasPrefix(b, []byte("enc:")) {
		return nil, errBadBlob
	}
	return b[4:], nil
}

var errBadBlob = &stubErr{"bad blob"}

type stubErr struct{ s string }

func (e *stubErr) Error() string { return e.s }
