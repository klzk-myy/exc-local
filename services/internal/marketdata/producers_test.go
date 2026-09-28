package marketdata

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	flatbuffers "github.com/google/flatbuffers/go"

	"exchange/internal/ipc"
	"exchange/internal/ipc/wire"
	"exchange/pkg/decimal"
)

// ---------------------------------------------------------------------------
// Test harness — emitter capture + source fixtures shared across the Wave-2
// producer tests (Tasks 6.3.3/6.3.4/6.3.11–13/6.3.20/6.3.23).
// ---------------------------------------------------------------------------

type capturedEmit struct {
	Channel string
	Seq     uint64
	Data    any
}

// emitter captures every Publish call; thread-safe for producer goroutines.
type emitter struct {
	mu   sync.Mutex
	evts []capturedEmit
}

func (e *emitter) fn() EmitFunc {
	return func(ch string, seq uint64, data any) {
		e.mu.Lock()
		e.evts = append(e.evts, capturedEmit{ch, seq, data})
		e.mu.Unlock()
	}
}

// payload renders the emitted struct to its wire-map form via JSON —
// the same view a WS client sees (spec §10.5 frame data object).
func payload(t *testing.T, e capturedEmit) map[string]any {
	t.Helper()
	raw, err := json.Marshal(e.Data)
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("unmarshal payload: %v", err)
	}
	return m
}

func (e *emitter) all() []capturedEmit {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make([]capturedEmit, len(e.evts))
	copy(out, e.evts)
	return out
}

// waitFor polls until n events are captured or the deadline passes.
// Returns the count reached — callers assert on it.
func (e *emitter) waitFor(n int, deadline time.Duration) int {
	end := time.Now().Add(deadline)
	for time.Now().Before(end) {
		e.mu.Lock()
		got := len(e.evts)
		e.mu.Unlock()
		if got >= n {
			return got
		}
		time.Sleep(5 * time.Millisecond)
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	return len(e.evts)
}

// forChannel filters captured events by exact channel.
func forChannel(evts []capturedEmit, ch string) []capturedEmit {
	var out []capturedEmit
	for _, e := range evts {
		if e.Channel == ch {
			out = append(out, e)
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// Source fixtures
// ---------------------------------------------------------------------------

// tradeChanSource feeds a fixed slice then closes (context-cancellable).
type tradeChanSource struct{ evts []TradeEvent }

func (s tradeChanSource) Trades(ctx context.Context) (<-chan TradeEvent, error) {
	ch := make(chan TradeEvent, len(s.evts)+1)
	go func() {
		defer close(ch)
		for _, e := range s.evts {
			select {
			case ch <- e:
			case <-ctx.Done():
				return
			}
		}
	}()
	return ch, nil
}

// deltaChanSource feeds BookDeltas then closes.
type deltaChanSource struct{ deltas []BookDelta }

func (s deltaChanSource) Deltas(ctx context.Context) (<-chan BookDelta, error) {
	ch := make(chan BookDelta, len(s.deltas)+1)
	go func() {
		defer close(ch)
		for _, d := range s.deltas {
			select {
			case ch <- d:
			case <-ctx.Done():
				return
			}
		}
	}()
	return ch, nil
}

// byteChanSource feeds encoded wire events then closes.
type byteChanSource struct{ bufs [][]byte }

func (s byteChanSource) Bytes(ctx context.Context) (<-chan []byte, error) {
	ch := make(chan []byte, len(s.bufs)+1)
	go func() {
		defer close(ch)
		for _, b := range s.bufs {
			cp := make([]byte, len(b))
			copy(cp, b)
			select {
			case ch <- cp:
			case <-ctx.Done():
				return
			}
		}
	}()
	return ch, nil
}

// ---------------------------------------------------------------------------
// Wire event encoders
// ---------------------------------------------------------------------------

func encodeOrderNew(seq, ts uint64, orderID, accountID uint64, instrument uint32,
	side wire.Side, qty, price int64) []byte {
	b := flatbuffers.NewBuilder(256)
	return ipc.EncodeOrderNewEvent(b, seq, ts, ipc.OrderNewMsg{
		OrderID: orderID, AccountID: accountID, InstrumentID: instrument,
		Side: side, Type: wire.OrderTypeLimit,
		Qty: qty, Price: price, TIF: wire.TimeInForceGTC,
		ClientOrderID: "t",
	})
}

func encodeTradeFill(seq, ts uint64, tradeID, buyID, sellID uint64,
	price, qty, engineSeq int64) []byte {
	b := flatbuffers.NewBuilder(256)
	return ipc.EncodeTradeFillEvent(b, seq, ts, tradeID, buyID, sellID,
		price, qty, engineSeq)
}

// dec renders a fixed-point 1e8 int to a decimal for assertions.
func dec(t *testing.T, raw int64) decimal.Decimal {
	t.Helper()
	return decimal.NewFromScaled(raw)
}

func mustDec(t *testing.T, s string) decimal.Decimal {
	t.Helper()
	d, err := decimal.NewFromString(s)
	if err != nil {
		t.Fatalf("decimal %q: %v", s, err)
	}
	return d
}

// decField extracts a wire string field and parses it as decimal.
func decField(t *testing.T, m map[string]any, key string) decimal.Decimal {
	t.Helper()
	v, ok := m[key].(string)
	if !ok {
		t.Fatalf("field %q missing or not a string: %v", key, m[key])
	}
	d, err := decimal.NewFromString(v)
	if err != nil {
		t.Fatalf("field %q not decimal: %q", key, v)
	}
	return d
}

const (
	testSym  = "EUR/USD"
	testInst = uint32(1)
	testNs   = uint64(1_700_000_000_000_000_000) // fixed epoch ns for events
)

var testResolver = MapResolver{testInst: testSym}
