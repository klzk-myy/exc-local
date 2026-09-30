package nats

// Live-cluster integration test for Task 1.3.11. Gated on EXC_NATS_TEST=1
// so unit runs stay hermetic; run against the dev cluster with:
//
//	EXC_NATS_TEST=1 go test -v -count=1 ./internal/nats/...
//
// Optional: EXC_NATS_URLS overrides the seed list (comma-separated).
// The reconnect subtest needs `sudo -n docker` and is skipped without it.

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/nats-io/nats.go/jetstream"
)

const defaultTestURLs = "nats://127.0.0.1:4222,nats://127.0.0.1:4223,nats://127.0.0.1:4224"

// natsTestClient connects to the dev cluster or skips.
func natsTestClient(t *testing.T) (*Client, context.Context) {
	t.Helper()
	if os.Getenv("EXC_NATS_TEST") != "1" {
		t.Skip("set EXC_NATS_TEST=1 to run live-cluster integration test")
	}
	urls := os.Getenv("EXC_NATS_URLS")
	if urls == "" {
		urls = defaultTestURLs
	}
	cfg := DefaultConfig(strings.Split(urls, ","))
	cfg.Name = "exc-nats-itest"
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	t.Cleanup(cancel)
	c, err := Connect(ctx, cfg, slog.New(slog.NewTextHandler(os.Stderr, nil)))
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(c.Close)
	return c, ctx
}

func TestIntegrationEnsureStreams(t *testing.T) {
	c, ctx := natsTestClient(t)

	// Run twice: creation must be idempotent (reconcile, not error).
	for i := 0; i < 2; i++ {
		infos, err := c.EnsureStreams(ctx)
		if err != nil {
			t.Fatalf("EnsureStreams pass %d: %v", i+1, err)
		}
		if len(infos) != len(Streams) {
			t.Fatalf("EnsureStreams returned %d streams, want %d", len(infos), len(Streams))
		}
		for _, info := range infos {
			name := info.Config.Name
			if info.Config.Replicas != 3 {
				t.Errorf("%s: replicas=%d, want 3", name, info.Config.Replicas)
			}
			// LimitsPolicy, not WorkQueue — spec §2.3.1 requires
			// independent durable consumer groups per service (see
			// streamConfig doc).
			if info.Config.Retention != jetstream.LimitsPolicy {
				t.Errorf("%s: retention=%s, want Limits", name, info.Config.Retention)
			}
			if info.Config.MaxAge != StreamMaxAge {
				t.Errorf("%s: max_age=%v, want %v", name, info.Config.MaxAge, StreamMaxAge)
			}
			if info.Config.Storage != jetstream.FileStorage {
				t.Errorf("%s: storage=%s, want File", name, info.Config.Storage)
			}
			if info.Cluster == nil {
				t.Fatalf("%s: no cluster info — not replicated?", name)
			}
			if n := len(info.Cluster.Replicas); n != 2 {
				t.Errorf("%s: %d follower replicas, want 2 (leader + 2 = R3)", name, n)
			}
		}
	}
}

func TestIntegrationPublishFetchAck(t *testing.T) {
	c, ctx := natsTestClient(t)
	if _, err := c.EnsureStreams(ctx); err != nil {
		t.Fatalf("EnsureStreams: %v", err)
	}

	// Unique symbol per run: under LimitsPolicy the stream retains history,
	// so a reused subject would replay stale copies from prior runs into
	// this consumer's backlog.
	sym := fmt.Sprintf("ITEST-%d", time.Now().UnixNano())
	cons, err := c.EnsureConsumer(ctx, "trades", "itest-ack",
		WithFilterSubject("trades.9."+sym))
	if err != nil {
		t.Fatalf("EnsureConsumer: %v", err)
	}
	defer func() {
		s, _ := c.JetStream().Stream(context.Background(), "trades")
		if s != nil {
			_ = s.DeleteConsumer(context.Background(), "itest-ack")
		}
	}()

	payload := []byte(`{"test":"ack","pair":"` + sym + `"}`)
	ack, err := c.Publish(ctx, "trades", 9, sym, payload)
	if err != nil {
		t.Fatalf("Publish: %v", err)
	}
	if ack.Stream != "trades" || ack.Sequence == 0 {
		t.Fatalf("PubAck = %+v, want stream=trades seq>0", ack)
	}
	if ack.Duplicate {
		t.Fatal("PubAck flagged duplicate on first publish")
	}

	msgs, err := Fetch(ctx, cons, 1, 10*time.Second)
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if len(msgs) != 1 {
		t.Fatalf("Fetch got %d msgs, want 1", len(msgs))
	}
	m := msgs[0]
	if m.Subject() != "trades.9."+sym {
		t.Fatalf("subject = %q, want trades.9.%s", m.Subject(), sym)
	}
	if string(m.Data()) != string(payload) {
		t.Fatalf("payload = %q, want %q", m.Data(), payload)
	}

	// Explicit ack → at-least-once satisfied; must NOT be redelivered.
	if err := m.Ack(); err != nil {
		t.Fatalf("Ack: %v", err)
	}
	time.Sleep(500 * time.Millisecond)
	again, err := Fetch(ctx, cons, 1, 3*time.Second)
	if err != nil {
		t.Fatalf("Fetch after ack: %v", err)
	}
	if len(again) != 0 {
		t.Fatalf("acked message redelivered (%d msgs) — at-least-once broken", len(again))
	}
}

func TestIntegrationRedelivery(t *testing.T) {
	c, ctx := natsTestClient(t)
	if _, err := c.EnsureStreams(ctx); err != nil {
		t.Fatalf("EnsureStreams: %v", err)
	}

	// Short AckWait so the test doesn't sit through the 30s default —
	// the production template value is covered by config assertions.
	// Unique symbol per run: LimitsPolicy retains history, so a reused
	// subject would replay stale copies into this consumer's backlog.
	sym := fmt.Sprintf("ITEST-%d", time.Now().UnixNano())
	cons, err := c.EnsureConsumer(ctx, "trades", "itest-redeliver",
		WithFilterSubject("trades.8."+sym),
		WithAckWait(2*time.Second),
		WithMaxDeliver(ConsumerMaxDeliver))
	if err != nil {
		t.Fatalf("EnsureConsumer: %v", err)
	}
	defer func() {
		s, _ := c.JetStream().Stream(context.Background(), "trades")
		if s != nil {
			_ = s.DeleteConsumer(context.Background(), "itest-redeliver")
		}
	}()

	if _, err := c.Publish(ctx, "trades", 8, sym, []byte(`{"test":"redelivery"}`)); err != nil {
		t.Fatalf("Publish: %v", err)
	}

	first, err := Fetch(ctx, cons, 1, 10*time.Second)
	if err != nil || len(first) != 1 {
		t.Fatalf("first fetch: msgs=%d err=%v", len(first), err)
	}
	// No ack — the message must come back after AckWait.
	meta1, err := first[0].Metadata()
	if err != nil {
		t.Fatalf("metadata: %v", err)
	}
	time.Sleep(3 * time.Second)

	second, err := Fetch(ctx, cons, 1, 5*time.Second)
	if err != nil || len(second) != 1 {
		t.Fatalf("redelivery fetch: msgs=%d err=%v", len(second), err)
	}
	meta2, err := second[0].Metadata()
	if err != nil {
		t.Fatalf("metadata: %v", err)
	}
	if meta2.Sequence.Stream != meta1.Sequence.Stream {
		t.Fatalf("redelivered different message: seq %d → %d",
			meta1.Sequence.Stream, meta2.Sequence.Stream)
	}
	if meta2.NumDelivered < 2 {
		t.Fatalf("NumDelivered=%d, want >=2", meta2.NumDelivered)
	}
	if err := second[0].Ack(); err != nil {
		t.Fatalf("Ack redelivered: %v", err)
	}
	t.Logf("redelivery verified: stream seq %d delivered %d times",
		meta2.Sequence.Stream, meta2.NumDelivered)
}

// TestIntegrationReconnect bounces whichever cluster node the client is
// attached to and verifies the connection re-establishes and publishing
// resumes. Requires passwordless-sudo docker; skipped otherwise.
func TestIntegrationReconnect(t *testing.T) {
	c, ctx := natsTestClient(t)
	if _, err := c.EnsureStreams(ctx); err != nil {
		t.Fatalf("EnsureStreams: %v", err)
	}

	// Map the connected port to its compose container name.
	portMap := map[string]string{
		"4222": "exc-dev-nats-1-1",
		"4223": "exc-dev-nats-2-1",
		"4224": "exc-dev-nats-3-1",
	}
	url := c.ConnectedURL()
	var container string
	for port, name := range portMap {
		if strings.HasSuffix(url, ":"+port) {
			container = name
		}
	}
	if container == "" {
		t.Skipf("connected URL %q is not a known dev node", url)
	}
	if err := exec.Command("sudo", "-n", "docker", "inspect", "-f", "{{.State.Running}}", container).Run(); err != nil {
		t.Skipf("docker not usable (sudo -n docker inspect failed): %v", err)
	}

	t.Logf("restarting %s (client on %s)", container, url)
	if out, err := exec.Command("sudo", "-n", "docker", "restart", container).CombinedOutput(); err != nil {
		t.Skipf("docker restart failed: %v (%s)", err, out)
	}

	// Client has MaxReconnects=-1; publish should succeed again shortly.
	deadline := time.Now().Add(45 * time.Second)
	var lastErr error
	for time.Now().Before(deadline) {
		pctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		_, lastErr = c.Publish(pctx, "trades", 2, "USD-JPY", []byte(`{"test":"reconnect"}`))
		cancel()
		if lastErr == nil {
			t.Logf("reconnected to %s, publish succeeded", c.ConnectedURL())
			return
		}
		time.Sleep(500 * time.Millisecond)
	}
	t.Fatalf("publish never recovered after node restart: %v", lastErr)
}

func TestIntegrationHealth(t *testing.T) {
	c, ctx := natsTestClient(t)
	if _, err := c.EnsureStreams(ctx); err != nil {
		t.Fatalf("EnsureStreams: %v", err)
	}
	rep, err := c.JetStreamHealth(ctx)
	if err != nil {
		t.Fatalf("JetStreamHealth: %v", err)
	}
	if rep.Account.Streams < len(Streams) {
		t.Fatalf("account streams = %d, want >= %d", rep.Account.Streams, len(Streams))
	}
	if len(rep.MissingStream) != 0 {
		t.Fatalf("missing streams: %v", rep.MissingStream)
	}
	if len(rep.Streams) != len(Streams) {
		t.Fatalf("report covers %d streams, want %d", len(rep.Streams), len(Streams))
	}
	for _, s := range rep.Streams {
		fmt.Fprintf(os.Stderr, "  %-14s msgs=%-4d replicas=%d consumers=%d\n",
			s.Name, s.Messages, s.Replicas, len(s.Consumers))
	}
}
