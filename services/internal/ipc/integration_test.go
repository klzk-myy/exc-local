// Task 1.3.5 — cross-language integration tests: Go endpoint <-> C++ helper
// binary (core/src/ipc/bench/ipc_echo_main.cpp) over both transports.
//
//   shm:   Go produces Event{OrderNew} on _in; the C++ helper (Endpoint::Core)
//          echoes Event{TradeFill} on _out. Proves the "Go writes 1000, C++
//          reads all 1000 zero-loss" criterion and measures RTT.
//   aeron: same over `aeron:ipc` against a live aeronmd media driver started
//          by the test — proves both sides connect and exchange messages.
//
// The helper is compiled once per `go test` run with the same flags the
// bench script uses (requires g++; skipped if absent).

package ipc_test

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"sync"
	"testing"
	"time"

	flatbuffers "github.com/google/flatbuffers/go"

	"exchange/internal/ipc"
	ipcaeron "exchange/internal/ipc/aeron"
	"exchange/internal/ipc/wire"
)

func repoRoot(t *testing.T) string {
	t.Helper()
	// package dir = <root>/services/internal/ipc
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	return filepath.Clean(filepath.Join(wd, "..", "..", ".."))
}

var (
	echoOnce    sync.Once
	echoBin     string
	echoBuildEr error
)

// echoBinary compiles the C++ echo helper (same command as bench_ipc.sh).
func echoBinary(t *testing.T) string {
	t.Helper()
	echoOnce.Do(func() {
		root := repoRoot(t)
		gpp, err := exec.LookPath("g++")
		if err != nil {
			echoBuildEr = fmt.Errorf("g++ not found: %w", err)
			return
		}
		out := filepath.Join(os.TempDir(),
			fmt.Sprintf("ipc_echo_test_%d", os.Getpid()))
		core := filepath.Join(root, "core")
		cmd := exec.Command(gpp,
			"-std=c++20", "-O2", "-DEXCH_WITH_AERON=1",
			"-I"+filepath.Join(core, "include"),
			"-I"+filepath.Join(core, "proto", "gen"),
			"-I"+filepath.Join(core, "third_party", "aeron", "include", "cpp"),
			filepath.Join(core, "src", "ipc", "bench", "ipc_echo_main.cpp"),
			filepath.Join(core, "src", "ipc", "SharedMemChannel.cpp"),
			filepath.Join(core, "src", "ipc", "AeronChannel.cpp"),
			filepath.Join(core, "src", "ipc", "IpcChannel.cpp"),
			filepath.Join(core, "third_party", "aeron", "lib", "libaeron_client.a"),
			"-lpthread", "-lrt",
			"-o", out)
		if b, err := cmd.CombinedOutput(); err != nil {
			echoBuildEr = fmt.Errorf("helper build: %v\n%s", err, b)
			return
		}
		echoBin = out
	})
	if echoBuildEr != nil {
		t.Skipf("cannot build C++ helper: %v", echoBuildEr)
	}
	t.Cleanup(func() {})
	return echoBin
}

// waitFor polls fn until it returns true or the deadline passes.
func waitFor(d time.Duration, fn func() bool) bool {
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if fn() {
			return true
		}
	}
	return false
}

func percentile(sorted []int64, p float64) int64 {
	if len(sorted) == 0 {
		return 0
	}
	i := int(float64(len(sorted)-1) * p)
	return sorted[i]
}

func reportLatency(t *testing.T, name string, lats []int64) {
	t.Helper()
	sort.Slice(lats, func(i, j int) bool { return lats[i] < lats[j] })
	var sum int64
	for _, v := range lats {
		sum += v
	}
	avg := sum / int64(len(lats))
	p50 := percentile(lats, 0.50)
	p99 := percentile(lats, 0.99)
	max := lats[len(lats)-1]
	t.Logf("%s: n=%d avg=%dns p50=%dns p99=%dns max=%dns",
		name, len(lats), avg, p50, p99, max)
	// Task 1.3.5 AC: < 50µs end-to-end (Go -> C++ -> Go).
	if p99 > 50_000 {
		t.Errorf("%s p99 %dns exceeds 50us budget", name, p99)
	}
}

// -- shm transport ----------------------------------------------------------

// DoD: Go writes 1000 messages, C++ reads all 1000 with zero loss — plus a
// 10k ping-pong RTT measurement under the 50µs end-to-end budget.
func TestShmRoundTripCpp(t *testing.T) {
	bin := echoBinary(t)
	base := fmt.Sprintf("exchange_ipc_it%d", os.Getpid())
	_ = os.Remove("/dev/shm/" + ipc.InName(base, 0))
	_ = os.Remove("/dev/shm/" + ipc.OutName(base, 0))
	defer func() {
		_ = os.Remove("/dev/shm/" + ipc.InName(base, 0))
		_ = os.Remove("/dev/shm/" + ipc.OutName(base, 0))
	}()

	const burst = 1000 // zero-loss criterion count
	const pingpong = 10000
	total := burst + pingpong

	cmd := exec.Command(bin, "shm", base, "0",
		fmt.Sprint(total), "30000")
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("start helper: %v", err)
	}
	defer cmd.Process.Kill()

	// Attach as the gateway end; the helper creates the images.
	gw, err := ipc.OpenChannel(base, 0, ipc.EndpointGateway, false,
		ipc.DefaultRingCapacity, ipc.DefaultRingSlotPayload)
	if err != nil {
		t.Fatalf("channel attach: %v", err)
	}
	defer gw.Close()

	b := flatbuffers.NewBuilder(256)
	order := ipc.OrderNewMsg{
		OrderID: 1, AccountID: 77, InstrumentID: 3,
		Side: wire.SideBuy, Type: wire.OrderTypeLimit,
		Qty: 100000000, Price: 105000000, TIF: wire.TimeInForceGTC,
		ClientOrderID: "it",
	}
	send := func(seq uint64) bool {
		b.Reset()
		msg := ipc.EncodeOrderNewEvent(b, seq, uint64(time.Now().UnixNano()), order)
		cp := make([]byte, len(msg))
		copy(cp, msg)
		deadline := time.Now().Add(5 * time.Second)
		for !gw.Send(cp) {
			if time.Now().After(deadline) {
				return false
			}
		}
		return true
	}
	recvFill := func() (seq uint64, ts uint64, ok bool) {
		p := gw.Peek()
		if p == nil {
			return 0, 0, false
		}
		ev := ipc.DecodeEvent(p)
		tf := ipc.EventTradeFill(ev)
		if tf == nil {
			t.Fatalf("non-fill reply type=%d", ev.TypeType())
		}
		seq, ts = ev.Seq(), ev.Ts()
		gw.Consume()
		return seq, ts, true
	}

	// Phase 1 — pipelined burst: all 1000 writes first, then 1000 reads.
	// Proves zero-loss delivery even when the producer outruns the consumer.
	for i := uint64(0); i < burst; i++ {
		if !send(i) {
			t.Fatalf("burst send %d failed", i)
		}
	}
	got := uint64(0)
	deadline := time.Now().Add(15 * time.Second)
	for got < burst && time.Now().Before(deadline) {
		if s, _, ok := recvFill(); ok {
			if s != got {
				t.Fatalf("out-of-order fill: got seq %d want %d", s, got)
			}
			got++
		}
	}
	if got != burst {
		t.Fatalf("zero-loss violated: %d/%d fills", got, burst)
	}

	// Phase 2 — ping-pong RTT, 10k messages.
	lats := make([]int64, 0, pingpong)
	for i := uint64(0); i < pingpong; i++ {
		seq := burst + i
		if !send(seq) {
			t.Fatalf("send %d failed", seq)
		}
		deadline := time.Now().Add(5 * time.Second)
		for {
			if s, ts, ok := recvFill(); ok {
				if s != seq {
					t.Fatalf("fill seq %d != %d", s, seq)
				}
				lats = append(lats, time.Now().UnixNano()-int64(ts))
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("timeout waiting fill for seq %d", seq)
			}
		}
	}
	reportLatency(t, "shm round-trip", lats)

	if err := cmd.Wait(); err != nil {
		t.Fatalf("helper exit: %v", err)
	}
}

// -- aeron transport ----------------------------------------------------------

func startAeronDriver(t *testing.T) (dir string) {
	t.Helper()
	root := repoRoot(t)
	md := filepath.Join(root, "core", "third_party", "aeron", "bin", "aeronmd")
	if _, err := os.Stat(md); err != nil {
		t.Skipf("aeronmd not vendored: %v", err)
	}
	dir = filepath.Join(os.TempDir(), fmt.Sprintf("aeron-exc-it%d", os.Getpid()))
	cmd := exec.Command(md, "-Daeron.dir="+dir, "-Daeron.dir.delete.on.start=1")
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("start aeronmd: %v", err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		_ = os.RemoveAll(dir)
	})
	if !waitFor(5*time.Second, func() bool {
		_, err := os.Stat(filepath.Join(dir, "cnc.dat"))
		return err == nil
	}) {
		t.Fatal("aeronmd did not create cnc.dat")
	}
	return dir
}

// DoD: media driver starts; C++ and Go (cgo) both connect to IPC channels;
// end-to-end RTT measured.
func TestAeronRoundTripCpp(t *testing.T) {
	dir := startAeronDriver(t)
	bin := echoBinary(t)

	const n = 10000
	cmd := exec.Command(bin, "aeron", dir, fmt.Sprint(n), "30000")
	cmd.Stderr = os.Stderr
	cmd.Stdout = os.Stdout
	if err := cmd.Start(); err != nil {
		t.Fatalf("start helper: %v", err)
	}
	defer cmd.Process.Kill()

	client, err := ipcaeron.Connect(dir, 5000)
	if err != nil {
		t.Fatalf("aeron connect: %v", err)
	}
	defer client.Close()

	pub, err := client.AddPublication("aeron:ipc?alias=orders_in", 1001, 5*time.Second)
	if err != nil {
		t.Fatalf("add publication: %v", err)
	}
	type fillMsg struct {
		seq uint64
		ts  uint64
	}
	fills := make(chan fillMsg, 64)
	sub, err := client.AddSubscription("aeron:ipc?alias=orders_out", 1002,
		func(buf []byte) {
			ev := ipc.DecodeEvent(buf)
			if tf := ipc.EventTradeFill(ev); tf != nil {
				fills <- fillMsg{seq: ev.Seq(), ts: ev.Ts()}
			}
		}, 5*time.Second)
	if err != nil {
		t.Fatalf("add subscription: %v", err)
	}
	defer sub.Close()

	if !waitFor(5*time.Second, pub.IsConnected) {
		t.Fatal("publication never connected")
	}
	if !waitFor(5*time.Second, sub.IsConnected) {
		t.Fatal("subscription never connected")
	}

	b := flatbuffers.NewBuilder(256)
	order := ipc.OrderNewMsg{
		OrderID: 9, AccountID: 55, InstrumentID: 3,
		Side: wire.SideSell, Type: wire.OrderTypeLimit,
		Qty: 250000000, Price: 106000000, TIF: wire.TimeInForceIOC,
		ClientOrderID: "aeron-it",
	}
	lats := make([]int64, 0, n)
	for i := uint64(0); i < n; i++ {
		b.Reset()
		msg := ipc.EncodeOrderNewEvent(b, i, uint64(time.Now().UnixNano()), order)
		cp := make([]byte, len(msg))
		copy(cp, msg)
		deadline := time.Now().Add(5 * time.Second)
		for pub.Offer(cp) <= 0 {
			if time.Now().After(deadline) {
				t.Fatalf("offer seq %d stuck", i)
			}
		}
		for {
			if sub.Poll(4) < 0 {
				t.Fatalf("poll error at seq %d", i)
			}
			select {
			case f := <-fills:
				if f.seq != i {
					t.Fatalf("fill seq %d != %d", f.seq, i)
				}
				lats = append(lats, time.Now().UnixNano()-int64(f.ts))
				goto next
			default:
			}
			if time.Now().After(deadline) {
				t.Fatalf("timeout waiting fill %d", i)
			}
		}
	next:
	}
	reportLatency(t, "aeron round-trip", lats)

	if err := cmd.Wait(); err != nil {
		t.Fatalf("helper exit: %v", err)
	}
}
