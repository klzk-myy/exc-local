// dlq_test.go — MemoryDLQ store semantics, DeadLetterMsg consumer seam,
// admin HTTP handlers. Live JetStream coverage is the EXC_NATS_TEST-gated
// integration test (dlq_integration_test.go).
package observability

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	gonats "github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
)

func TestMemoryDLQLifecycle(t *testing.T) {
	ctx := context.Background()
	d := NewMemoryDLQ()

	seq1, err := d.Put(ctx, DeadLetter{
		Stream: "trades", Consumer: "ingest", Subject: "trades.0.EUR-USD",
		Reason: "decode", Deliveries: 5, Payload: []byte("bad-msg")})
	if err != nil {
		t.Fatal(err)
	}
	_, err = d.Put(ctx, DeadLetter{
		Stream: "funding", Consumer: "recon", Subject: "funding.1.USD",
		Reason: "pg down", Payload: []byte("x")})
	if err != nil {
		t.Fatal(err)
	}

	// List all → both; filter by stream → one.
	list, err := d.List(ctx, ListFilter{})
	if err != nil || len(list) != 2 {
		t.Fatalf("list = %d %v", len(list), err)
	}
	list, err = d.List(ctx, ListFilter{Stream: "trades"})
	if err != nil || len(list) != 1 || list[0].Subject != "trades.0.EUR-USD" {
		t.Fatalf("filtered list = %+v", list)
	}

	// Get returns full detail.
	dl, err := d.Get(ctx, seq1)
	if err != nil {
		t.Fatal(err)
	}
	if dl.Reason != "decode" || dl.Deliveries != 5 || string(dl.Payload) != "bad-msg" {
		t.Fatalf("get = %+v", dl)
	}
	if _, err := d.Get(ctx, 999); !errors.Is(err, ErrNotFound) {
		t.Fatalf("get missing = %v", err)
	}

	// Replay redelivers to origin and removes the entry.
	if err := d.Replay(ctx, seq1); err != nil {
		t.Fatal(err)
	}
	if len(d.Replayed) != 1 || d.Replayed[0].Subject != "trades.0.EUR-USD" ||
		string(d.Replayed[0].Payload) != "bad-msg" {
		t.Fatalf("replayed = %+v", d.Replayed)
	}
	if _, err := d.Get(ctx, seq1); !errors.Is(err, ErrNotFound) {
		t.Fatal("entry still present after replay")
	}

	// Replay a missing entry errors; Discard removes without redrive.
	if err := d.Replay(ctx, seq1); !errors.Is(err, ErrNotFound) {
		t.Fatalf("replay missing = %v", err)
	}
	list, _ = d.List(ctx, ListFilter{})
	if err := d.Discard(ctx, list[0].Seq); err != nil {
		t.Fatal(err)
	}
	if len(d.Replayed) != 1 {
		t.Fatal("discard must not redeliver")
	}
	if out, _ := d.List(ctx, ListFilter{}); len(out) != 0 {
		t.Fatalf("entries remain: %+v", out)
	}
}

// fakeMsg implements jetstream.Msg for the DeadLetterMsg seam test.
type fakeMsg struct {
	subject    string
	data       []byte
	headers    gonats.Header
	meta       *jetstream.MsgMetadata
	terminated bool
}

func (m *fakeMsg) Metadata() (*jetstream.MsgMetadata, error) { return m.meta, nil }
func (m *fakeMsg) Data() []byte                              { return m.data }
func (m *fakeMsg) Headers() gonats.Header                    { return m.headers }
func (m *fakeMsg) Subject() string                           { return m.subject }
func (m *fakeMsg) Reply() string                             { return "" }
func (m *fakeMsg) Ack() error                                { return nil }
func (m *fakeMsg) DoubleAck(context.Context) error           { return nil }
func (m *fakeMsg) Nak() error                                { return nil }
func (m *fakeMsg) NakWithDelay(time.Duration) error          { return nil }
func (m *fakeMsg) InProgress() error                         { return nil }
func (m *fakeMsg) Term() error                               { m.terminated = true; return nil }
func (m *fakeMsg) TermWithReason(string) error               { m.terminated = true; return nil }

func TestDeadLetterMsgTerminates(t *testing.T) {
	ctx := context.Background()
	store := NewMemoryDLQ()
	msg := &fakeMsg{
		subject: "compliance.2.EUR-USD",
		data:    []byte(`{"poison":true}`),
		headers: gonats.Header{"X-Trace": []string{"abc"}},
		meta: &jetstream.MsgMetadata{
			Stream: "compliance", Consumer: "aml-scan", NumDelivered: 5,
			Timestamp: time.Now().UTC(),
		},
	}
	if err := DeadLetterMsg(ctx, store, msg, "max deliveries exceeded"); err != nil {
		t.Fatal(err)
	}
	if !msg.terminated {
		t.Fatal("message not Term()'d after DLQ write")
	}
	list, _ := store.List(ctx, ListFilter{})
	if len(list) != 1 {
		t.Fatalf("dlq len = %d", len(list))
	}
	dl := list[0]
	if dl.Stream != "compliance" || dl.Consumer != "aml-scan" ||
		dl.Deliveries != 5 || dl.Reason != "max deliveries exceeded" {
		t.Fatalf("dlq entry = %+v", dl)
	}
	if dl.Headers["X-Trace"] != "abc" {
		t.Fatalf("headers = %v", dl.Headers)
	}
}

func TestDeadLetterMsgStoreFailureLeavesMessage(t *testing.T) {
	store := &failStore{}
	msg := &fakeMsg{subject: "s", data: []byte("x")}
	if err := DeadLetterMsg(context.Background(), store, msg, "x"); err == nil {
		t.Fatal("store failure must propagate")
	}
	if msg.terminated {
		t.Fatal("message terminated despite failed DLQ write")
	}
}

type failStore struct{ Store }

func (failStore) Put(context.Context, DeadLetter) (uint64, error) {
	return 0, fmt.Errorf("store down")
}

func TestDLQHandlers(t *testing.T) {
	d := NewMemoryDLQ()
	seq, _ := d.Put(context.Background(), DeadLetter{
		Stream: "trades", Consumer: "ingest", Subject: "trades.0.EUR-USD",
		Reason: "boom", Payload: []byte("p")})

	h := DLQHandler(d)
	// list
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/api/v1/admin/dlq", nil))
	if rec.Code != 200 {
		t.Fatalf("list status = %d body=%s", rec.Code, rec.Body)
	}
	var listResp struct {
		Entries []DeadLetter `json:"entries"`
		Count   int          `json:"count"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &listResp); err != nil {
		t.Fatal(err)
	}
	if listResp.Count != 1 || listResp.Entries[0].Seq != seq {
		t.Fatalf("list = %+v", listResp)
	}
	// detail
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET",
		fmt.Sprintf("/api/v1/admin/dlq?seq=%d", seq), nil))
	if rec.Code != 200 {
		t.Fatalf("detail status = %d", rec.Code)
	}
	// missing seq
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/api/v1/admin/dlq?seq=999", nil))
	if rec.Code != 404 {
		t.Fatalf("missing seq status = %d", rec.Code)
	}

	// replay via action handler
	rec = httptest.NewRecorder()
	DLQActionHandler(d, "replay").ServeHTTP(rec, httptest.NewRequest("POST",
		fmt.Sprintf("/api/v1/admin/dlq/replay?seq=%d", seq), nil))
	if rec.Code != 200 {
		t.Fatalf("replay status = %d body=%s", rec.Code, rec.Body)
	}
	if len(d.Replayed) != 1 {
		t.Fatal("replay did not redeliver")
	}

	// nil store → 503 (fail-closed surface when NATS absent)
	rec = httptest.NewRecorder()
	DLQHandler(nil).ServeHTTP(rec, httptest.NewRequest("GET", "/api/v1/admin/dlq", nil))
	if rec.Code != 503 {
		t.Fatalf("nil store status = %d", rec.Code)
	}
}

func TestDLQSubjectSanitize(t *testing.T) {
	got := dlqSubject("margin-events", "consumer.one")
	if strings.Contains(got, "*") || strings.Contains(got, ">") {
		t.Fatalf("unsafe subject %q", got)
	}
	if got != "ops-dlq.margin-events.consumer-one" {
		t.Fatalf("subject = %q", got)
	}
}
