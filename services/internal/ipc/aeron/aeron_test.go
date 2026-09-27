// Task 1.3.5 — cgo Aeron client tests. Starts the vendored aeronmd media
// driver and exercises Connect / AddPublication / AddSubscription / Offer /
// Poll over `aeron:ipc` with a same-process loopback image.

package aeron

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

func startDriver(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	// package dir = <root>/services/internal/ipc/aeron
	md := filepath.Clean(filepath.Join(wd, "..", "..", "..", "..",
		"core", "third_party", "aeron", "bin", "aeronmd"))
	if _, err := os.Stat(md); err != nil {
		t.Skipf("aeronmd not vendored: %v", err)
	}
	dir := filepath.Join(os.TempDir(), fmt.Sprintf("aeron-go-%d", os.Getpid()))
	cmd := exec.Command(md, "-Daeron.dir="+dir, "-Daeron.dir.delete.on.start=1")
	if err := cmd.Start(); err != nil {
		t.Skipf("aeronmd start: %v", err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		_ = os.RemoveAll(dir)
	})
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(filepath.Join(dir, "cnc.dat")); err == nil {
			return dir
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("aeronmd did not produce cnc.dat")
	return ""
}

func TestConnectPublishPoll(t *testing.T) {
	dir := startDriver(t)

	client, err := Connect(dir, 5000)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer client.Close()
	if client.ClientID() < 0 {
		t.Fatal("bad client id")
	}

	var got atomic.Int64
	var last atomic.Value
	sub, err := client.AddSubscription("aeron:ipc?alias=orders_in", 1001,
		func(buf []byte) {
			cp := make([]byte, len(buf))
			copy(cp, buf)
			last.Store(cp)
			got.Add(1)
		}, 5*time.Second)
	if err != nil {
		t.Fatalf("add subscription: %v", err)
	}
	defer sub.Close()

	pub, err := client.AddPublication("aeron:ipc?alias=orders_in", 1001,
		5*time.Second)
	if err != nil {
		t.Fatalf("add publication: %v", err)
	}

	deadline := time.Now().Add(5 * time.Second)
	for !pub.IsConnected() || !sub.IsConnected() {
		if time.Now().After(deadline) {
			t.Fatalf("pub=%v sub=%v not connected",
				pub.IsConnected(), sub.IsConnected())
		}
		time.Sleep(time.Millisecond)
	}

	payload := []byte("aeron-cgo-loopback")
	for i := 0; i < 100 && got.Load() == 0; i++ {
		if pos := pub.Offer(payload); pos <= 0 {
			continue
		}
		sub.Poll(4)
	}
	if got.Load() == 0 {
		t.Fatal("no fragment received")
	}
	if v := last.Load().([]byte); string(v) != string(payload) {
		t.Fatalf("payload mismatch: %q", v)
	}
}

func TestConnectMissingDriver(t *testing.T) {
	// A directory that will never host a CnC file must fail fast.
	c, err := Connect(filepath.Join(os.TempDir(), "aeron-nonexistent"), 300)
	if err == nil {
		c.Close()
		t.Fatal("connect should fail without driver")
	}
}
