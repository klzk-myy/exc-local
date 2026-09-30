package reporting

import (
	"context"
	"errors"
	"testing"
	"time"

	flatbuffers "github.com/google/flatbuffers/go"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"exchange/internal/ipc"
)

// fakeJSMsg implements jetstream.Msg for HandleMsg unit tests.
type fakeJSMsg struct {
	data   []byte
	acked  bool
	naked  bool
	ackErr error
	termD  bool
	onAck  func()
}

func (m *fakeJSMsg) Metadata() (*jetstream.MsgMetadata, error) { return nil, nil }
func (m *fakeJSMsg) Data() []byte                              { return m.data }
func (m *fakeJSMsg) Headers() nats.Header                      { return nil }
func (m *fakeJSMsg) Subject() string                           { return "trades.0.EUR-USD" }
func (m *fakeJSMsg) Reply() string                             { return "" }
func (m *fakeJSMsg) Ack() error {
	m.acked = true
	if m.onAck != nil {
		m.onAck()
	}
	return m.ackErr
}
func (m *fakeJSMsg) DoubleAck(context.Context) error { return m.Ack() }
func (m *fakeJSMsg) Nak() error                      { m.naked = true; return nil }
func (m *fakeJSMsg) NakWithDelay(time.Duration) error {
	return m.Nak()
}
func (m *fakeJSMsg) InProgress() error           { return nil }
func (m *fakeJSMsg) Term() error                 { m.termD = true; return nil }
func (m *fakeJSMsg) TermWithReason(string) error { return m.Term() }

var _ jetstream.Msg = (*fakeJSMsg)(nil)

func fillFrame(tradeID uint64, ts uint64) []byte {
	b := flatbuffers.NewBuilder(256)
	return ipc.EncodeTradeFillEvent(b, 7, ts, tradeID, 1001, 2002, 108500000, 100000, 7)
}

func TestConsumer_DecodesFillToOnFill(t *testing.T) {
	store := NewMemConfirmationStore()
	gen := &fakeGen{recs: []ConfirmationRecord{{TradeID: 55, AccountID: 7, Version: 1}}}
	svc, _ := NewConfirmationService(ServiceOptions{Generator: gen, Tracker: store})
	c, err := NewConfirmationConsumer(svc)
	if err != nil {
		t.Fatal(err)
	}
	m := &fakeJSMsg{data: fillFrame(55, uint64(time.Now().UnixNano()))}
	if err := c.HandleMsg(context.Background(), m); err != nil {
		t.Fatalf("handle: %v", err)
	}
	if gen.genCalls != 1 {
		t.Fatalf("generate calls=%d", gen.genCalls)
	}
}

func TestConsumer_PoisonAndNonFillAckAway(t *testing.T) {
	gen := &fakeGen{}
	svc, _ := NewConfirmationService(ServiceOptions{Generator: gen, Tracker: NewMemConfirmationStore()})
	c, _ := NewConfirmationConsumer(svc)
	for _, frame := range [][]byte{
		nil,                       // empty
		{1, 2, 3},                 // truncated
		[]byte("garbage-garbage"), // invalid flatbuffer
	} {
		if err := c.HandleMsg(context.Background(), &fakeJSMsg{data: frame}); err != nil {
			t.Fatalf("poison frame must ack away, got %v", err)
		}
	}
	if gen.genCalls != 0 {
		t.Fatal("poison frames triggered generation")
	}
}

func TestConsumer_GenerateErrorPropagatesForNak(t *testing.T) {
	gen := &fakeGen{generateErr: errors.New("db down")}
	svc, _ := NewConfirmationService(ServiceOptions{Generator: gen, Tracker: NewMemConfirmationStore()})
	c, _ := NewConfirmationConsumer(svc)
	err := c.HandleMsg(context.Background(),
		&fakeJSMsg{data: fillFrame(9, uint64(time.Now().UnixNano()))})
	if err == nil {
		t.Fatal("generate failure must propagate → caller NAKs")
	}
}
