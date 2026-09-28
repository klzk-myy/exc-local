// dlq.go — dead-letter queue for failed event consumers (Task 7.3.10
// item 3, spec §2.7, §24 #306).
//
// Production store: a dedicated `ops-dlq` JetStream stream with
// LimitsPolicy retention (NOT the canonical WorkQueuePolicy — DLQ entries
// must survive for inspection and controlled redrive, so the consumer
// ownership semantics of work-queue streams are wrong here). Entries are
// stored with X-DLQ-* headers carrying origin stream/subject/consumer and
// failure metadata; replay republishes the original payload to the origin
// subject then removes the DLQ copy; discard removes it.
//
// Consumers dead-letter via DeadLetterMsg(): put + Term() (terminal ack)
// — a poison message leaves the delivery loop and lands in DLQ instead of
// cycling through MaxDeliver forever.
//
// MemoryDLQ backs unit tests and the no-NATS fallback.
package observability

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	gonats "github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
)

// DLQStream is the dead-letter stream name; subjects are
// "ops-dlq.<origin-stream>.<origin-consumer>".
const DLQStream = "ops-dlq"

// DLQSubjectPrefix is the wildcard root the stream binds.
const DLQSubjectPrefix = "ops-dlq"

// Header keys on DLQ messages.
const (
	hdrStream     = "X-DLQ-Stream"
	hdrConsumer   = "X-DLQ-Consumer"
	hdrSubject    = "X-DLQ-Subject"
	hdrReason     = "X-DLQ-Reason"
	hdrFailedAt   = "X-DLQ-Failed-At"
	hdrDeliveries = "X-DLQ-Deliveries"
)

// DeadLetter is one failed event retained for inspection/redrive.
type DeadLetter struct {
	Seq        uint64            `json:"seq"`               // DLQ stream sequence
	Stream     string            `json:"stream"`            // origin JetStream stream
	Consumer   string            `json:"consumer"`          // origin durable consumer
	Subject    string            `json:"subject"`           // original publish subject
	Reason     string            `json:"reason"`            // terminal failure cause
	FailedAt   time.Time         `json:"failed_at"`         // dead-letter timestamp
	Deliveries uint64            `json:"deliveries"`        // origin delivery attempts
	Payload    []byte            `json:"payload"`           // original message bytes
	Headers    map[string]string `json:"headers,omitempty"` // original msg headers
}

// ListFilter narrows List output.
type ListFilter struct {
	Stream   string // "" = all
	Consumer string // "" = all
	Limit    int    // 0 → default 100, capped at 1000
}

// Store is the DLQ abstraction — JetStreamDLQ in production, MemoryDLQ
// in tests / NATS-less operation.
type Store interface {
	Put(ctx context.Context, dl DeadLetter) (seq uint64, err error)
	List(ctx context.Context, f ListFilter) ([]DeadLetter, error)
	Get(ctx context.Context, seq uint64) (*DeadLetter, error)
	// Replay republishes the payload to the origin subject and removes the
	// DLQ entry — the controlled redrive.
	Replay(ctx context.Context, seq uint64) error
	// Discard removes the entry without republishing.
	Discard(ctx context.Context, seq uint64) error
}

// ErrNotFound is returned by Get/Replay/Discard for an unknown seq.
var ErrNotFound = fmt.Errorf("dlq: entry not found")

// ---------------------------------------------------------------------------
// JetStream-backed store
// ---------------------------------------------------------------------------

// JetStreamDLQ stores dead letters in the ops-dlq stream.
type JetStreamDLQ struct {
	js     jetstream.JetStream
	stream jetstream.Stream
}

// NewJetStreamDLQ provisions (idempotent) and opens the ops-dlq stream.
// LimitsPolicy retention + FileStorage + R3 + 7-day MaxAge mirrors the
// canonical streams' durability while keeping entries browsable via
// direct GetMsg.
func NewJetStreamDLQ(ctx context.Context, js jetstream.JetStream) (*JetStreamDLQ, error) {
	if js == nil {
		return nil, fmt.Errorf("dlq: nil jetstream context")
	}
	s, err := js.CreateOrUpdateStream(ctx, jetstream.StreamConfig{
		Name:      DLQStream,
		Subjects:  []string{DLQSubjectPrefix + ".>"},
		Retention: jetstream.LimitsPolicy,
		Storage:   jetstream.FileStorage,
		Replicas:  3,
		MaxAge:    7 * 24 * time.Hour,
		Discard:   jetstream.DiscardOld,
	})
	if err != nil {
		return nil, fmt.Errorf("dlq: ensure stream %q: %w", DLQStream, err)
	}
	return &JetStreamDLQ{js: js, stream: s}, nil
}

// OpenJetStreamDLQ binds the ops-dlq stream without creating it — the
// read/inspect surface convention: stream provisioning is an operator
// action (`natsctl dlq init`), not a service boot side effect (same rule
// as the canonical streams). A missing stream returns an error; callers
// degrade their DLQ surface to 503 rather than fail boot.
func OpenJetStreamDLQ(ctx context.Context, js jetstream.JetStream) (*JetStreamDLQ, error) {
	if js == nil {
		return nil, fmt.Errorf("dlq: nil jetstream context")
	}
	s, err := js.Stream(ctx, DLQStream)
	if err != nil {
		return nil, fmt.Errorf("dlq: open stream %q: %w", DLQStream, err)
	}
	return &JetStreamDLQ{js: js, stream: s}, nil
}

// dlqSubject builds the per-origin subject leaf.
func dlqSubject(stream, consumer string) string {
	san := func(s string) string {
		s = strings.Map(func(r rune) rune {
			if r == '.' || r == '*' || r == '>' || r == ' ' {
				return '-'
			}
			return r
		}, s)
		if s == "" {
			return "unknown"
		}
		return s
	}
	return DLQSubjectPrefix + "." + san(stream) + "." + san(consumer)
}

// Put stores the dead letter.
func (d *JetStreamDLQ) Put(ctx context.Context, dl DeadLetter) (uint64, error) {
	msg := gonats.NewMsg(dlqSubject(dl.Stream, dl.Consumer))
	msg.Data = dl.Payload
	msg.Header.Set(hdrStream, dl.Stream)
	msg.Header.Set(hdrSubject, dl.Subject)
	msg.Header.Set(hdrConsumer, dl.Consumer)
	msg.Header.Set(hdrReason, dl.Reason)
	if dl.FailedAt.IsZero() {
		dl.FailedAt = time.Now().UTC()
	}
	msg.Header.Set(hdrFailedAt, dl.FailedAt.Format(time.RFC3339Nano))
	msg.Header.Set(hdrDeliveries, strconv.FormatUint(dl.Deliveries, 10))
	for k, v := range dl.Headers {
		msg.Header.Set("X-DLQ-Orig-"+k, v)
	}
	ack, err := d.js.PublishMsg(ctx, msg)
	if err != nil {
		return 0, fmt.Errorf("dlq: publish: %w", err)
	}
	return ack.Sequence, nil
}

func decodeDeadLetter(seq uint64, m *jetstream.RawStreamMsg) DeadLetter {
	h := m.Header
	dl := DeadLetter{
		Seq:      seq,
		Stream:   h.Get(hdrStream),
		Consumer: h.Get(hdrConsumer),
		Subject:  h.Get(hdrSubject),
		Reason:   h.Get(hdrReason),
		Payload:  append([]byte(nil), m.Data...),
	}
	if ts, err := time.Parse(time.RFC3339Nano, h.Get(hdrFailedAt)); err == nil {
		dl.FailedAt = ts
	}
	dl.Deliveries, _ = strconv.ParseUint(h.Get(hdrDeliveries), 10, 64)
	for k := range h {
		if strings.HasPrefix(k, "X-DLQ-Orig-") {
			if dl.Headers == nil {
				dl.Headers = map[string]string{}
			}
			dl.Headers[strings.TrimPrefix(k, "X-DLQ-Orig-")] = h.Get(k)
		}
	}
	return dl
}

// List walks the stream's sequence range with direct GetMsg — newest
// last. Only entries matching f are returned; f.Limit caps the scan
// result (default 100, max 1000).
func (d *JetStreamDLQ) List(ctx context.Context, f ListFilter) ([]DeadLetter, error) {
	limit := f.Limit
	if limit <= 0 {
		limit = 100
	}
	if limit > 1000 {
		limit = 1000
	}
	info, err := d.stream.Info(ctx)
	if err != nil {
		return nil, fmt.Errorf("dlq: stream info: %w", err)
	}
	var out []DeadLetter
	for seq := info.State.FirstSeq; seq <= info.State.LastSeq; seq++ {
		m, err := d.stream.GetMsg(ctx, seq)
		if err != nil {
			continue // gap: deleted/discarded seq, or torn read — skip
		}
		dl := decodeDeadLetter(seq, m)
		if f.Stream != "" && dl.Stream != f.Stream {
			continue
		}
		if f.Consumer != "" && dl.Consumer != f.Consumer {
			continue
		}
		out = append(out, dl)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Seq < out[j].Seq })
	if len(out) > limit {
		out = out[len(out)-limit:]
	}
	return out, nil
}

// Get returns one entry by stream sequence.
func (d *JetStreamDLQ) Get(ctx context.Context, seq uint64) (*DeadLetter, error) {
	m, err := d.stream.GetMsg(ctx, seq)
	if err != nil {
		return nil, ErrNotFound
	}
	dl := decodeDeadLetter(seq, m)
	return &dl, nil
}

// Replay republishes the payload to the origin subject, then removes the
// DLQ entry. If the republish fails the entry is retained — redrive is
// atomic from the operator's view (the entry is either replayed+gone or
// untouched).
func (d *JetStreamDLQ) Replay(ctx context.Context, seq uint64) error {
	dl, err := d.Get(ctx, seq)
	if err != nil {
		return err
	}
	if dl.Subject == "" {
		return fmt.Errorf("dlq: entry %d has no origin subject", seq)
	}
	if _, err := d.js.Publish(ctx, dl.Subject, dl.Payload); err != nil {
		return fmt.Errorf("dlq: replay publish %q: %w", dl.Subject, err)
	}
	if err := d.stream.DeleteMsg(ctx, seq); err != nil {
		return fmt.Errorf("dlq: replay delete seq %d: %w", seq, err)
	}
	return nil
}

// Discard removes the entry without republishing.
func (d *JetStreamDLQ) Discard(ctx context.Context, seq uint64) error {
	if _, err := d.Get(ctx, seq); err != nil {
		return err
	}
	if err := d.stream.DeleteMsg(ctx, seq); err != nil {
		return fmt.Errorf("dlq: delete seq %d: %w", seq, err)
	}
	return nil
}

// ---------------------------------------------------------------------------
// consumer dead-letter helper
// ---------------------------------------------------------------------------

// DeadLetterMsg dead-letters a poison JetStream message: it records the
// message in store with origin metadata then Term()s it so the delivery
// loop moves on. A store failure leaves the message to be redelivered —
// the DLQ write is the authoritative record, so fail toward redelivery,
// not silent ack.
func DeadLetterMsg(ctx context.Context, store Store, m jetstream.Msg, reason string) error {
	if store == nil {
		return fmt.Errorf("dlq: nil store")
	}
	dl := DeadLetter{
		Subject:  m.Subject(),
		Payload:  append([]byte(nil), m.Data()...),
		Reason:   reason,
		FailedAt: time.Now().UTC(),
	}
	if md, err := m.Metadata(); err == nil && md != nil {
		dl.Stream = md.Stream
		dl.Consumer = md.Consumer
		dl.Deliveries = md.NumDelivered
		dl.FailedAt = md.Timestamp
	}
	for k := range m.Headers() {
		if dl.Headers == nil {
			dl.Headers = map[string]string{}
		}
		dl.Headers[k] = m.Headers().Get(k)
	}
	if _, err := store.Put(ctx, dl); err != nil {
		return err
	}
	return m.Term()
}

// ---------------------------------------------------------------------------
// in-memory store (tests + no-NATS fallback)
// ---------------------------------------------------------------------------

// MemoryDLQ is the in-memory Store: deterministic for tests and as the
// no-NATS fallback. Replay hands the payload to Redeliver (set by the
// owner — production binds a publisher; tests capture).
type MemoryDLQ struct {
	mu      sync.Mutex
	seq     uint64
	entries map[uint64]DeadLetter
	// Redeliver is invoked by Replay before the entry is removed.
	// Nil → replay records onto Replayed for assertion.
	Redeliver func(ctx context.Context, dl DeadLetter) error
	Replayed  []DeadLetter // entries redriven (when Redeliver is nil)
}

// NewMemoryDLQ builds the store.
func NewMemoryDLQ() *MemoryDLQ {
	return &MemoryDLQ{entries: map[uint64]DeadLetter{}}
}

// Put stores the entry.
func (d *MemoryDLQ) Put(_ context.Context, dl DeadLetter) (uint64, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.seq++
	dl.Seq = d.seq
	if dl.FailedAt.IsZero() {
		dl.FailedAt = time.Now().UTC()
	}
	d.entries[d.seq] = dl
	return d.seq, nil
}

// List returns entries matching f in seq order.
func (d *MemoryDLQ) List(_ context.Context, f ListFilter) ([]DeadLetter, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	var out []DeadLetter
	for _, dl := range d.entries {
		if f.Stream != "" && dl.Stream != f.Stream {
			continue
		}
		if f.Consumer != "" && dl.Consumer != f.Consumer {
			continue
		}
		out = append(out, dl)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Seq < out[j].Seq })
	limit := f.Limit
	if limit <= 0 {
		limit = 100
	}
	if len(out) > limit {
		out = out[len(out)-limit:]
	}
	return out, nil
}

// Get returns the entry for seq.
func (d *MemoryDLQ) Get(_ context.Context, seq uint64) (*DeadLetter, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	dl, ok := d.entries[seq]
	if !ok {
		return nil, ErrNotFound
	}
	return &dl, nil
}

// Replay redelivers then removes the entry.
func (d *MemoryDLQ) Replay(ctx context.Context, seq uint64) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	dl, ok := d.entries[seq]
	if !ok {
		return ErrNotFound
	}
	if d.Redeliver != nil {
		if err := d.Redeliver(ctx, dl); err != nil {
			return err
		}
	} else {
		d.Replayed = append(d.Replayed, dl)
	}
	delete(d.entries, seq)
	return nil
}

// Discard removes the entry without redelivery.
func (d *MemoryDLQ) Discard(_ context.Context, seq uint64) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if _, ok := d.entries[seq]; !ok {
		return ErrNotFound
	}
	delete(d.entries, seq)
	return nil
}

// ---------------------------------------------------------------------------
// admin HTTP surface
// ---------------------------------------------------------------------------

// DLQHandler serves the admin DLQ inspection endpoints. Mount on the
// admin service mux (and the gateway's live-handler table for the
// registered GET /api/v1/admin/dlq route — Phase-05 Task 5.3.7 registry).
//
//	GET  /api/v1/admin/dlq               — list (query: stream, consumer, limit)
//	GET  /api/v1/admin/dlq?seq=N         — single entry
//	POST /api/v1/admin/dlq/replay?seq=N  — redrive to origin subject
//	POST /api/v1/admin/dlq/discard?seq=N — remove without redrive
//
// Replay/discard mutations live on explicit POST paths; the route
// registry currently declares only the GET review surface, so the POSTs
// are served by the admin service mux until Phase-05 registers them.
func DLQHandler(store Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if store == nil {
			writeJSON(w, http.StatusServiceUnavailable,
				map[string]any{"type": "error", "error": "SERVICE_DEGRADED",
					"message": "DLQ store unavailable (NATS not connected)"})
			return
		}
		if r.Method != http.MethodGet {
			writeJSON(w, http.StatusMethodNotAllowed,
				map[string]any{"type": "error", "error": "METHOD_NOT_ALLOWED"})
			return
		}
		q := r.URL.Query()
		if seqStr := q.Get("seq"); seqStr != "" {
			seq, err := strconv.ParseUint(seqStr, 10, 64)
			if err != nil {
				writeJSON(w, http.StatusBadRequest,
					map[string]any{"type": "error", "error": "INVALID_PARAMETER",
						"message": "seq must be an unsigned integer"})
				return
			}
			dl, err := store.Get(r.Context(), seq)
			if err != nil {
				writeJSON(w, http.StatusNotFound,
					map[string]any{"type": "error", "error": "DLQ_ENTRY_NOT_FOUND"})
				return
			}
			writeJSON(w, http.StatusOK, dl)
			return
		}
		limit, _ := strconv.Atoi(q.Get("limit"))
		list, err := store.List(r.Context(), ListFilter{
			Stream:   q.Get("stream"),
			Consumer: q.Get("consumer"),
			Limit:    limit,
		})
		if err != nil {
			writeJSON(w, http.StatusServiceUnavailable,
				map[string]any{"type": "error", "error": "SERVICE_DEGRADED",
					"message": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"entries": list, "count": len(list)})
	}
}

// DLQActionHandler serves replay/discard POSTs (admin service mux).
func DLQActionHandler(store Store, action string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if store == nil {
			writeJSON(w, http.StatusServiceUnavailable,
				map[string]any{"type": "error", "error": "SERVICE_DEGRADED"})
			return
		}
		if r.Method != http.MethodPost {
			writeJSON(w, http.StatusMethodNotAllowed,
				map[string]any{"type": "error", "error": "METHOD_NOT_ALLOWED"})
			return
		}
		seq, err := strconv.ParseUint(r.URL.Query().Get("seq"), 10, 64)
		if err != nil {
			writeJSON(w, http.StatusBadRequest,
				map[string]any{"type": "error", "error": "INVALID_PARAMETER",
					"message": "seq must be an unsigned integer"})
			return
		}
		switch action {
		case "replay":
			err = store.Replay(r.Context(), seq)
		case "discard":
			err = store.Discard(r.Context(), seq)
		default:
			err = fmt.Errorf("unknown action %q", action)
		}
		if err != nil {
			status := http.StatusInternalServerError
			if err == ErrNotFound {
				status = http.StatusNotFound
			}
			writeJSON(w, status, map[string]any{
				"type": "error", "error": "DLQ_ACTION_FAILED", "message": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"seq": seq, "action": action, "status": "ok"})
	}
}

// writeJSON is the package-local JSON responder — the handler is used
// both inside the gateway (whose api.WriteJSON envelope lives in
// internal/api) and standalone in the admin service.
func writeJSON(w http.ResponseWriter, status int, v any) {
	body, err := json.Marshal(v)
	if err != nil {
		body = []byte(`{"type":"error","error":"INTERNAL_ERROR"}`)
		status = http.StatusInternalServerError
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(body)
}
