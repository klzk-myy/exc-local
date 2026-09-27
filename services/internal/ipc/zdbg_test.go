package ipc

import (
	"fmt"
	"os"
	"os/exec"
	"testing"
	"time"

	flatbuffers "github.com/google/flatbuffers/go"
	"exchange/internal/ipc/wire"
)

func TestDbgShm(t *testing.T) {
	base := fmt.Sprintf("dbg_rt_%d", os.Getpid())
	_ = os.Remove("/dev/shm/" + InName(base, 0))
	_ = os.Remove("/dev/shm/" + OutName(base, 0))
	cmd := exec.Command("/tmp/ipc_echo", "shm", base, "0", "3", "10000")
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil { t.Fatal(err) }
	defer cmd.Process.Kill()
	time.Sleep(300 * time.Millisecond)

	gw, err := OpenChannel(base, 0, EndpointGateway, false, DefaultRingCapacity, DefaultRingSlotPayload)
	if err != nil { t.Fatal(err) }
	defer gw.Close()

	b := flatbuffers.NewBuilder(256)
	for i := uint64(0); i < 3; i++ {
		msg := EncodeOrderNewEvent(b, i, uint64(time.Now().UnixNano()), OrderNewMsg{
			OrderID: 100 + i, AccountID: 1, InstrumentID: 3,
			Side: wire.SideBuy, Type: wire.OrderTypeLimit,
			Qty: 100000000, Price: 105000000, TIF: wire.TimeInForceGTC,
			ClientOrderID: "x",
		})
		cp := make([]byte, len(msg))
		copy(cp, msg)
		if !gw.Send(cp) { t.Fatalf("send %d failed", i) }
		t.Logf("sent %d, occupancy(in)=%d", i, func() uint64 {
			r, _ := OpenRing(InName(base, 0), RoleConsumer, false, DefaultRingCapacity, DefaultRingSlotPayload)
			defer r.Close()
			return r.Occupancy()
		}())
	}
	deadline := time.Now().Add(5 * time.Second)
	got := 0
	for got < 3 && time.Now().Before(deadline) {
		p := gw.Peek()
		if p == nil { continue }
		ev := DecodeEvent(p)
		t.Logf("got type=%d seq=%d", ev.TypeType(), ev.Seq())
		gw.Consume()
		got++
	}
	t.Logf("received=%d outocc=%d", got, gw.Occupancy())
	if got != 3 { t.Fatal("missing fills") }
}
