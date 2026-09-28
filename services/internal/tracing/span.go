// span.go — the OTel-compatible span model (Task 9.3.11).
//
// Field names and OTLP encoding (exporter.go) follow the OTLP/HTTP JSON
// mapping so spans are importable by Jaeger, Tempo and the OTel
// collector unchanged — the "SDK" here is the model plus the wire
// format, which is all the collector contract requires.
package tracing

import (
	"sync"
	"time"
)

// Kind mirrors OTLP SpanKind enum values.
type Kind int32

const (
	KindInternal Kind = 1
	KindServer   Kind = 2
	KindClient   Kind = 3
	KindProducer Kind = 4
	KindConsumer Kind = 5
)

// StatusCode mirrors OTLP Status_code enum values.
type StatusCode int32

const (
	StatusUnset StatusCode = 0
	StatusOK    StatusCode = 1
	StatusError StatusCode = 2
)

// Attr is a span attribute; Value must be string, int64, float64 or
// bool (the OTLP scalar set). Anything else renders via fmt at encode.
type Attr struct {
	Key   string
	Value any
}

// Str returns a string attribute.
func Str(k, v string) Attr { return Attr{Key: k, Value: v} }

// Int returns an int64 attribute.
func Int(k string, v int64) Attr { return Attr{Key: k, Value: v} }

// Float returns a float64 attribute.
func Float(k string, v float64) Attr { return Attr{Key: k, Value: v} }

// Bool returns a bool attribute.
func Bool(k string, v bool) Attr { return Attr{Key: k, Value: v} }

// Event is a timestamped span annotation.
type Event struct {
	Name  string
	At    time.Time
	Attrs []Attr
}

// SpanData is the immutable snapshot handed to exporters.
type SpanData struct {
	TraceID       string
	SpanID        string
	ParentSpanID  string
	Name          string
	Kind          Kind
	Start         time.Time
	End           time.Time
	Attrs         []Attr
	Events        []Event
	Status        StatusCode
	StatusMessage string
	ServiceName   string
}

// Span is the in-flight operation record. Not for concurrent use beyond
// the internal mutex (handlers do touch spans across goroutines on the
// WS path — guard everything).
type Span struct {
	ctx       SpanContext
	tracer    *Tracer
	name      string
	kind      Kind
	start     time.Time
	clock     func() time.Time
	mu        sync.Mutex
	attrs     []Attr
	events    []Event
	status    StatusCode
	statusMsg string
	finished  bool
}

// Context returns the span's propagatable context.
func (s *Span) Context() SpanContext { return s.ctx }

// SetAttr appends attributes; duplicate keys are last-write-wins at the
// backend, same as OTel.
func (s *Span) SetAttr(attrs ...Attr) {
	s.mu.Lock()
	s.attrs = append(s.attrs, attrs...)
	s.mu.Unlock()
}

// AddEvent appends a timestamped annotation.
func (s *Span) AddEvent(name string, attrs ...Attr) {
	s.mu.Lock()
	s.events = append(s.events, Event{Name: name, At: s.clock(), Attrs: attrs})
	s.mu.Unlock()
}

// SetStatus records OK or Error (Unset is the zero value and never
// exported deliberately).
func (s *Span) SetStatus(code StatusCode, msg string) {
	s.mu.Lock()
	s.status, s.statusMsg = code, msg
	s.mu.Unlock()
}

// RecordError marks the span errored and adds an exception event —
// tail-sampling-relevant: errored spans always export.
func (s *Span) RecordError(err error) {
	if err == nil {
		return
	}
	s.AddEvent("exception",
		Str("exception.type", "error"),
		Str("exception.message", err.Error()))
	s.SetStatus(StatusError, err.Error())
}

// Finish completes the span and hands the snapshot to the tracer for
// sampling/export. Idempotent — a second Finish is a no-op.
func (s *Span) Finish() {
	s.mu.Lock()
	if s.finished {
		s.mu.Unlock()
		return
	}
	s.finished = true
	s.mu.Unlock()
	s.tracer.finish(s)
}

// snapshot renders the export view; called once at Finish.
func (s *Span) snapshot(end time.Time) SpanData {
	s.mu.Lock()
	defer s.mu.Unlock()
	return SpanData{
		TraceID:       s.ctx.TraceID,
		SpanID:        s.ctx.SpanID,
		ParentSpanID:  s.ctx.ParentSpanID,
		Name:          s.name,
		Kind:          s.kind,
		Start:         s.start,
		End:           end,
		Attrs:         append([]Attr(nil), s.attrs...),
		Events:        append([]Event(nil), s.events...),
		Status:        s.status,
		StatusMessage: s.statusMsg,
		ServiceName:   s.tracer.service,
	}
}

// IsRecording reports whether this span will export — callers can skip
// expensive attribute computation on the unsampled hot path.
func (s *Span) IsRecording() bool { return s.tracer != nil }
