// Task 1.3.5 — Go half of the IPC round-trip benchmark.
//
// Sends N FlatBuffers Event{OrderNew} messages to the C++ echo helper
// (core/src/ipc/bench/ipc_echo_main.cpp), reads the echoed
// Event{TradeFill} replies, and reports RTT percentiles. One run covers one
// transport; deploy/scripts/bench_ipc.sh drives both and applies the
// <50µs end-to-end acceptance gate.
//
//	bench_ipc_go shm   <base> <shard> <n> <timeout_ms>
//	bench_ipc_go aeron <aeron_dir> <n> <timeout_ms>
//
// Exit codes: 0 ok · 1 setup failure · 2 message loss/timeout.
package main

import (
	"fmt"
	"os"
	"sort"
	"strconv"
	"time"

	flatbuffers "github.com/google/flatbuffers/go"

	"exchange/internal/ipc"
	ipcaeron "exchange/internal/ipc/aeron"
	"exchange/internal/ipc/wire"
)

type transport interface {
	send(p []byte) bool
	// recv returns (seq, ts, ok); ok=false means "nothing right now".
	recv() (uint64, uint64, bool)
	close()
}

// -- shared-memory transport -------------------------------------------------

type shmT struct{ ch *ipc.Channel }

func (t *shmT) send(p []byte) bool { return t.ch.Send(p) }
func (t *shmT) recv() (uint64, uint64, bool) {
	p := t.ch.Peek()
	if p == nil {
		return 0, 0, false
	}
	ev := ipc.DecodeEvent(p)
	seq, ts := ev.Seq(), ev.Ts()
	t.ch.Consume()
	return seq, ts, true
}
func (t *shmT) close() { _ = t.ch.Close() }

// -- aeron transport ---------------------------------------------------------

type aeronT struct {
	client *ipcaeron.Client
	pub    *ipcaeron.Publication
	sub    *ipcaeron.Subscription
	fills  chan [2]uint64
}

func (t *aeronT) send(p []byte) bool { return t.pub.Offer(p) > 0 }
func (t *aeronT) recv() (uint64, uint64, bool) {
	t.sub.Poll(16)
	select {
	case f := <-t.fills:
		return f[0], f[1], true
	default:
		return 0, 0, false
	}
}
func (t *aeronT) close() {
	t.sub.Close()
	t.client.Close()
}

func dialShm(base string, shard uint16) (transport, error) {
	ch, err := ipc.OpenChannel(base, shard, ipc.EndpointGateway, false,
		ipc.DefaultRingCapacity, ipc.DefaultRingSlotPayload)
	if err != nil {
		return nil, err
	}
	return &shmT{ch: ch}, nil
}

func dialAeron(dir string) (transport, error) {
	client, err := ipcaeron.Connect(dir, 5000)
	if err != nil {
		return nil, err
	}
	pub, err := client.AddPublication("aeron:ipc?alias=orders_in", 1001, 5*time.Second)
	if err != nil {
		client.Close()
		return nil, err
	}
	t := &aeronT{client: client, pub: pub, fills: make(chan [2]uint64, 256)}
	sub, err := client.AddSubscription("aeron:ipc?alias=orders_out", 1002,
		func(buf []byte) {
			ev := ipc.DecodeEvent(buf)
			if ipc.EventTradeFill(ev) != nil {
				t.fills <- [2]uint64{ev.Seq(), ev.Ts()}
			}
		}, 5*time.Second)
	if err != nil {
		client.Close()
		return nil, err
	}
	t.sub = sub

	// Wait for both directions to connect (bounded).
	deadline := time.Now().Add(5 * time.Second)
	for !pub.IsConnected() || !sub.IsConnected() {
		if time.Now().After(deadline) {
			t.close()
			return nil, fmt.Errorf("aeron: connect timeout")
		}
		time.Sleep(time.Millisecond)
	}
	return t, nil
}

func main() {
	if len(os.Args) < 3 {
		fmt.Fprintf(os.Stderr,
			"usage: %s shm <base> <shard> <n> <timeout_ms>\n"+
				"       %s aeron <dir> <n> <timeout_ms>\n",
			os.Args[0], os.Args[0])
		os.Exit(64)
	}
	mode := os.Args[1]
	var tr transport
	var err error
	var n uint64
	var timeoutMs int64

	switch mode {
	case "shm":
		if len(os.Args) < 6 {
			os.Exit(64)
		}
		shard, _ := strconv.ParseUint(os.Args[3], 10, 16)
		n, _ = strconv.ParseUint(os.Args[4], 10, 64)
		timeoutMs, _ = strconv.ParseInt(os.Args[5], 10, 64)
		tr, err = dialShm(os.Args[2], uint16(shard))
	case "aeron":
		if len(os.Args) < 5 {
			os.Exit(64)
		}
		n, _ = strconv.ParseUint(os.Args[3], 10, 64)
		timeoutMs, _ = strconv.ParseInt(os.Args[4], 10, 64)
		tr, err = dialAeron(os.Args[2])
	default:
		os.Exit(64)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s: connect: %v\n", mode, err)
		os.Exit(1)
	}
	defer tr.close()

	b := flatbuffers.NewBuilder(256)
	order := ipc.OrderNewMsg{
		OrderID: 1, AccountID: 77, InstrumentID: 3,
		Side: wire.SideBuy, Type: wire.OrderTypeLimit,
		Qty: 100000000, Price: 105000000, TIF: wire.TimeInForceGTC,
		ClientOrderID: "bench",
	}
	deadline := time.Now().Add(time.Duration(timeoutMs) * time.Millisecond)
	lats := make([]int64, 0, n)

	for i := uint64(0); i < n; i++ {
		b.Reset()
		msg := ipc.EncodeOrderNewEvent(b, i, uint64(time.Now().UnixNano()), order)
		cp := make([]byte, len(msg))
		copy(cp, msg)
		for !tr.send(cp) { // bounded-spin on backpressure
			if time.Now().After(deadline) {
				fmt.Fprintf(os.Stderr, "%s: send stuck at seq %d\n", mode, i)
				os.Exit(2)
			}
		}
		for {
			seq, ts, ok := tr.recv()
			if ok {
				if seq != i {
					fmt.Fprintf(os.Stderr, "%s: seq mismatch got=%d want=%d\n",
						mode, seq, i)
					os.Exit(2)
				}
				lats = append(lats, time.Now().UnixNano()-int64(ts))
				break
			}
			if time.Now().After(deadline) {
				fmt.Fprintf(os.Stderr, "%s: timeout at seq %d (got %d/%d)\n",
					mode, i, len(lats), n)
				os.Exit(2)
			}
		}
	}

	sort.Slice(lats, func(i, j int) bool { return lats[i] < lats[j] })
	var sum int64
	for _, v := range lats {
		sum += v
	}
	pct := func(p float64) int64 {
		return lats[int(float64(len(lats)-1)*p)]
	}
	fmt.Printf("%s: n=%d avg=%dns p50=%dns p99=%dns max=%dns\n",
		mode, len(lats), sum/int64(len(lats)), pct(0.50), pct(0.99),
		lats[len(lats)-1])
	if pct(0.99) > 50_000 {
		os.Exit(2) // exceeds the <50µs end-to-end budget
	}
}
