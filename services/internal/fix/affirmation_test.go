// Unit tests for Task 18.3.6 — Traiana/MarkitSERV affirmation export,
// two-way status sync and the pending-affirmation timeout monitor.
package fix

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestNormalizeAffirmStatus(t *testing.T) {
	for s, want := range map[string]GiveUpStatus{
		"AFFIRMED": GiveUpAffirmed, "MATCHED": GiveUpAffirmed,
		"REJECTED": GiveUpRejected, "DK": GiveUpRejected,
		"DISPUTED": GiveUpDisputed, "BREAK": GiveUpDisputed,
	} {
		got, ok := NormalizeAffirmStatus(s)
		if !ok || got != want {
			t.Fatalf("%s -> %v,%v want %s", s, got, ok, want)
		}
	}
	if _, ok := NormalizeAffirmStatus("GIBBERISH"); ok {
		t.Fatal("unknown status must not normalize")
	}
}

func TestHTTPAffirmationExporter(t *testing.T) {
	var gotBody affirmationRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer tok" {
			t.Error("missing auth header")
		}
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"message_id":"TRX-991","status":"RECEIVED"}`))
	}))
	defer srv.Close()

	exp := NewAffirmationExporter(VenueTraiana, srv.URL, "tok", srv.Client())
	ref, err := exp.Submit(context.Background(),
		GiveUpTrade{ID: 5, TradeID: 77},
		PrimeBroker{TraianaCode: "PB-PART-1"})
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	if ref != "TRX-991" {
		t.Fatalf("ref %q", ref)
	}
	if gotBody.TradeID != 77 || gotBody.Venue != VenueTraiana || gotBody.VenueRef != "PB-PART-1" {
		t.Fatalf("wire body: %+v", gotBody)
	}
}

func TestHTTPAffirmationExporter_NotConfigured(t *testing.T) {
	exp := NewAffirmationExporter(VenueTraiana, "", "", nil)
	if _, err := exp.Submit(context.Background(), GiveUpTrade{}, PrimeBroker{}); !errors.Is(err, ErrAffirmationNotConfigured) {
		t.Fatalf("want ErrAffirmationNotConfigured, got %v", err)
	}
}

func TestHTTPAffirmationExporter_HTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer srv.Close()
	exp := NewAffirmationExporter(VenueMarkitSERV, srv.URL, "", srv.Client())
	if _, err := exp.Submit(context.Background(), GiveUpTrade{}, PrimeBroker{}); err == nil {
		t.Fatal("non-2xx must fail")
	}
}

// fakeGiveUpWriter records UpdateStatusByTraianaID calls.
type fakeGiveUpWriter struct {
	calls []struct {
		msgID  string
		status GiveUpStatus
		reason string
	}
	err error
}

func (f *fakeGiveUpWriter) UpdateStatusByTraianaID(_ context.Context, msgID string, to GiveUpStatus, reason string) error {
	if f.err != nil {
		return f.err
	}
	f.calls = append(f.calls, struct {
		msgID  string
		status GiveUpStatus
		reason string
	}{msgID, to, reason})
	return nil
}

func TestAffirmationSync_Apply(t *testing.T) {
	w := &fakeGiveUpWriter{}
	sync := NewAffirmationSync(w)
	if err := sync.Apply(context.Background(), "TRX-1", "AFFIRMED", ""); err != nil {
		t.Fatalf("apply: %v", err)
	}
	if err := sync.Apply(context.Background(), "TRX-2", "DK", "wrong account"); err != nil {
		t.Fatalf("apply: %v", err)
	}
	if len(w.calls) != 2 || w.calls[0].status != GiveUpAffirmed || w.calls[1].status != GiveUpRejected {
		t.Fatalf("calls: %+v", w.calls)
	}
	if w.calls[1].reason != "wrong account" {
		t.Fatal("reject reason must propagate")
	}
	if err := sync.Apply(context.Background(), "TRX-3", "WAT", ""); err == nil {
		t.Fatal("unknown status must fail loudly")
	}
}

// fakePendingLister returns canned PENDING give-ups.
type fakePendingLister struct{ rows []GiveUpTrade }

func (f fakePendingLister) PendingOlderThan(_ context.Context, _ time.Time) ([]GiveUpTrade, error) {
	return f.rows, nil
}

type breakRecorder struct{ breaks []GiveUpBreak }

func (b *breakRecorder) AlertBreak(_ context.Context, br GiveUpBreak) error {
	b.breaks = append(b.breaks, br)
	return nil
}

func TestAffirmationMonitor_ScanOnce(t *testing.T) {
	lister := fakePendingLister{rows: []GiveUpTrade{
		{ID: 1, TradeID: 100, CreatedAt: time.Now().Add(-2 * time.Hour)},
		{ID: 2, TradeID: 101, CreatedAt: time.Now().Add(-3 * time.Hour)},
	}}
	rec := &breakRecorder{}
	mon := NewAffirmationMonitor(lister, time.Hour, time.Minute, rec)
	n, err := mon.ScanOnce(context.Background())
	if err != nil || n != 2 {
		t.Fatalf("scan: n=%d err=%v", n, err)
	}
	if len(rec.breaks) != 2 {
		t.Fatalf("want 2 breaks, got %d", len(rec.breaks))
	}
	// Second sweep dedupes — already-alerted give-ups do not re-alert.
	n, _ = mon.ScanOnce(context.Background())
	if n != 0 || len(rec.breaks) != 2 {
		t.Fatalf("dedupe broken: n=%d breaks=%d", n, len(rec.breaks))
	}
}

func TestAffirmationMonitor_Defaults(t *testing.T) {
	mon := NewAffirmationMonitor(fakePendingLister{}, 0, 0, nil)
	if mon.timeout != DefaultAffirmationTimeout {
		t.Fatalf("timeout %v", mon.timeout)
	}
}
