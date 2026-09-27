// Task 1.3.5 — Go-side tests for the shared-memory SPSC ring + channel.

package ipc

import (
	"bytes"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	flatbuffers "github.com/google/flatbuffers/go"

	"exchange/internal/ipc/wire"
)

func uniqBase(t *testing.T) string {
	t.Helper()
	base := fmt.Sprintf("exch_gotest_%s_%d", t.Name(), os.Getpid())
	// Best-effort cleanup of stale images.
	_ = os.Remove("/dev/shm/" + InName(base, 0))
	_ = os.Remove("/dev/shm/" + OutName(base, 0))
	return base
}

func openPair(t *testing.T, base string) (prod, cons *Ring) {
	t.Helper()
	p, err := OpenRing(base+"_ring", RoleProducer, true, 4096, 1024)
	if err != nil {
		t.Fatalf("producer open: %v", err)
	}
	c, err := OpenRing(base+"_ring", RoleConsumer, false, 4096, 1024)
	if err != nil {
		t.Fatalf("consumer open: %v", err)
	}
	t.Cleanup(func() {
		p.Close()
		c.Close()
		_ = os.Remove("/dev/shm/" + base + "_ring")
	})
	return p, c
}

func TestRingZeroLoss1000(t *testing.T) {
	base := uniqBase(t)
	prod, cons := openPair(t, base)

	b := flatbuffers.NewBuilder(256)
	var wg sync.WaitGroup
	wg.Add(1)
	bad := false
	done := make(chan struct{})
	go func() {
		defer wg.Done()
		defer close(done)
		want := uint64(0)
		deadline := time.Now().Add(10 * time.Second)
		for want < 1000 && time.Now().Before(deadline) {
			p := cons.Peek()
			if p == nil {
				continue
			}
			ev := DecodeEvent(p)
			on := EventOrderNew(ev)
			if on == nil || ev.Seq() != want || on.OrderId() != 5000+want {
				bad = true
			}
			cons.Consume()
			want++
		}
	}()

	for i := uint64(0); i < 1000; i++ {
		b.Reset()
		msg := EncodeOrderNewEvent(b, i, uint64(time.Now().UnixNano()), OrderNewMsg{
			OrderID:       5000 + i,
			AccountID:     1000,
			InstrumentID:  3,
			Side:          wire.SideBuy,
			Type:          wire.OrderTypeLimit,
			Qty:           100000000,
			Price:         105000000,
			TIF:           wire.TimeInForceGTC,
			ClientOrderID: fmt.Sprintf("g-%d", i),
		})
		cp := make([]byte, len(msg))
		copy(cp, msg)
		if !prod.WriteWait(cp, 5*time.Second) {
			t.Fatalf("write %d failed", i)
		}
	}
	wg.Wait()
	if bad {
		t.Fatal("message content/sequence mismatch")
	}
	if prod.Drops() != 0 {
		t.Fatalf("unexpected drops %d", prod.Drops())
	}
}

func TestRingBackpressure(t *testing.T) {
	base := uniqBase(t)
	p, err := OpenRing(base+"_ring", RoleProducer, true, 8, 1024)
	if err != nil {
		t.Fatalf("producer open: %v", err)
	}
	c, err := OpenRing(base+"_ring", RoleConsumer, false, 8, 1024)
	if err != nil {
		t.Fatalf("consumer open: %v", err)
	}
	defer func() {
		p.Close()
		c.Close()
		_ = os.Remove("/dev/shm/" + base + "_ring")
	}()

	msg := []byte("0123456789abcdef")
	for i := 0; i < 8; i++ {
		if !p.TryWrite(msg) {
			t.Fatalf("write %d should fit", i)
		}
	}
	if p.TryWrite(msg) {
		t.Fatal("write to full ring should fail")
	}
	if p.Drops() == 0 {
		t.Fatal("drop counter should have incremented")
	}
	if p.Occupancy() != 8 {
		t.Fatalf("occupancy=%d want 8", p.Occupancy())
	}
	if c.Peek() == nil {
		t.Fatal("consumer sees empty")
	}
	c.Consume()
	if !p.TryWrite(msg) {
		t.Fatal("write after drain should succeed")
	}
}

func TestRingProducerLiveness(t *testing.T) {
	base := uniqBase(t)
	// This test process is the producer; consumer checks kill(pid,0).
	prod, cons := openPair(t, base)
	if !cons.ProducerAlive() {
		t.Fatal("live producer should probe alive")
	}
	if cons.ProducerPid() != uint64(os.Getpid()) {
		t.Fatalf("producer pid %d != %d", cons.ProducerPid(), os.Getpid())
	}
	if cons.ProducerHeartbeatNs() == 0 {
		t.Fatal("heartbeat never stamped")
	}
	prod.Close()
	// pid still ours & alive — semantics verified cross-process in the C++
	// test (ShmRing.ProducerDeathDetection) via fork+exit.
	_ = cons
}

func TestChannelLoopback(t *testing.T) {
	base := uniqBase(t)
	core, err := OpenChannel(base, 0, EndpointCore, true, 256, 1024)
	if err != nil {
		t.Fatalf("core open: %v", err)
	}
	gw, err := OpenChannel(base, 0, EndpointGateway, false, 256, 1024)
	if err != nil {
		t.Fatalf("gw open: %v", err)
	}
	defer func() {
		core.Close()
		gw.Close()
	}()

	b := flatbuffers.NewBuilder(256)
	msg := EncodeOrderNewEvent(b, 42, uint64(time.Now().UnixNano()), OrderNewMsg{
		OrderID: 4242, AccountID: 1, InstrumentID: 3,
		Side: wire.SideSell, Type: wire.OrderTypeMarket,
		Qty: 5, Price: 0, TIF: wire.TimeInForceIOC,
		ClientOrderID: "loop",
	})
	cp := make([]byte, len(msg))
	copy(cp, msg)
	if !gw.Send(cp) {
		t.Fatal("gateway send failed")
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		p := core.Peek()
		if p != nil {
			ev := DecodeEvent(p)
			on := EventOrderNew(ev)
			if ev.Seq() != 42 || on == nil || on.OrderId() != 4242 ||
				on.Side() != wire.SideSell {
				t.Fatalf("bad inbound event seq=%d", ev.Seq())
			}
			core.Consume()
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("core never received")
		}
	}

	// core -> gw
	out := EncodeTradeFillEvent(b, 42, uint64(time.Now().UnixNano()),
		9001, 4242, 4243, 105000000, 5, 42)
	if !core.Send(out) {
		t.Fatal("core send failed")
	}
	for {
		p := gw.Peek()
		if p != nil {
			ev := DecodeEvent(p)
			tf := EventTradeFill(ev)
			if tf == nil || tf.TradeId() != 9001 || tf.BuyOrderId() != 4242 {
				t.Fatal("bad fill")
			}
			gw.Consume()
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("gw never received fill")
		}
	}
	if core.Drops() != 0 || gw.Drops() != 0 {
		t.Fatal("unexpected drops")
	}
}

func TestEncodeDecodeIdentities(t *testing.T) {
	b := flatbuffers.NewBuilder(512)
	msg := EncodeOrderNewEvent(b, 7, 12345, OrderNewMsg{
		OrderID: 99, AccountID: 55, InstrumentID: 11,
		Side: wire.SideBuy, Type: wire.OrderTypeStopLimit,
		Qty: 123, Price: 456, TIF: wire.TimeInForceGTD,
		ClientOrderID: "x-9",
	})
	cp := bytes.Clone(msg)
	ev := DecodeEvent(cp)
	if ev.Seq() != 7 || ev.Ts() != 12345 {
		t.Fatal("envelope fields")
	}
	on := EventOrderNew(ev)
	if on == nil {
		t.Fatal("not an OrderNew")
	}
	if on.OrderId() != 99 || on.AccountId() != 55 || on.InstrumentId() != 11 ||
		on.Side() != wire.SideBuy || on.Type() != wire.OrderTypeStopLimit ||
		on.Qty() != 123 || on.Price() != 456 || on.Tif() != wire.TimeInForceGTD ||
		string(on.ClientOrderId()) != "x-9" {
		t.Fatal("field round-trip mismatch")
	}
}
