// trace_ipc_test.go — Task 9.3.11 Go half: producer injects the 64B
// EXCTRACE block onto the engine-bound frame; the consumer strips it
// before decode and continues the trace with a remote-parented span.
package orders

import (
	"context"
	"testing"

	flatbuffers "github.com/google/flatbuffers/go"

	"exchange/internal/tracing"
)

type recExporter struct{ spans []tracing.SpanData }

func (r *recExporter) ExportSpans(_ context.Context, s []tracing.SpanData) error {
	r.spans = append(r.spans, s...)
	return nil
}
func (r *recExporter) Shutdown(context.Context) error { return nil }

// tracedFrame composes [EXCTRACE block][Event payload] the way
// ShmSubmitter.Send does when ctx carries a span.
func tracedFrame(ctx context.Context, payload []byte) []byte {
	frame := make([]byte, tracing.AeronTraceHeaderLen+len(payload))
	tracing.InjectAeronTrace(ctx, frame[:tracing.AeronTraceHeaderLen])
	copy(frame[tracing.AeronTraceHeaderLen:], payload)
	return frame
}

func TestConsumer_StripsTraceBlockAndContinuesSpan(t *testing.T) {
	rec := &recExporter{}
	tr := tracing.NewTracer("test", rec, &tracing.Options{SampleAll: true})
	st := newFakeStore()
	st.orders[4242] = &Order{Status: "ACTIVE"}
	con := NewConsumer(&fakeSubmitter{store: st}, st, newPendingConfirms()).
		WithTracer(tr)

	parentCtx, parent := tr.Start(context.Background(), "http.submit",
		tracing.KindServer)

	b := flatbuffers.NewBuilder(256)
	payload := EncodeCancelEvent(b, 7, 1, 4242, 11)
	con.HandleFragment(tracedFrame(parentCtx, payload))

	if st.orders[4242].Status != "CANCELLED" {
		t.Fatal("traced frame did not decode/apply — strip failed")
	}
	var got *tracing.SpanData
	for i := range rec.spans {
		if rec.spans[i].Name == "orders.consume" {
			got = &rec.spans[i]
		}
	}
	if got == nil {
		t.Fatalf("no orders.consume span exported (%d spans)", len(rec.spans))
	}
	pc := parent.Context()
	if got.TraceID != pc.TraceID || got.ParentSpanID != pc.SpanID {
		t.Fatalf("span not remote-parented: trace=%s parent=%s want %s/%s",
			got.TraceID, got.ParentSpanID, pc.TraceID, pc.SpanID)
	}
}

func TestConsumer_UntracedFrameStillDecodes(t *testing.T) {
	st := newFakeStore()
	st.orders[777] = &Order{Status: "ACTIVE"}
	con := NewConsumer(&fakeSubmitter{store: st}, st, newPendingConfirms())
	b := flatbuffers.NewBuilder(256)
	con.HandleFragment(EncodeCancelEvent(b, 7, 1, 777, 11))
	if st.orders[777].Status != "CANCELLED" {
		t.Fatal("legacy untraced frame broke — strip must be a no-op")
	}
}
