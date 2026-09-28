// Live-cluster integration test for the JetStream DLQ. Gated on
// EXC_NATS_TEST=1 (same convention as internal/nats):
//
//	EXC_NATS_TEST=1 go test -v -count=1 ./internal/observability/ -run DLQ
//
// Optional: EXC_NATS_URLS overrides the seed list.
package observability

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/nats-io/nats.go/jetstream"

	excnats "exchange/internal/nats"
)

func dlqTestClient(t *testing.T) (*excnats.Client, context.Context) {
	t.Helper()
	if os.Getenv("EXC_NATS_TEST") != "1" {
		t.Skip("set EXC_NATS_TEST=1 to run live-cluster integration test")
	}
	urls := os.Getenv("EXC_NATS_URLS")
	if urls == "" {
		urls = "nats://127.0.0.1:4222,nats://127.0.0.1:4223,nats://127.0.0.1:4224"
	}
	cfg := excnats.DefaultConfig(strings.Split(urls, ","))
	cfg.Name = "exc-observability-itest"
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	t.Cleanup(cancel)
	c, err := excnats.Connect(ctx, cfg, slog.New(slog.NewTextHandler(os.Stderr, nil)))
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(c.Close)
	return c, ctx
}

func TestIntegrationDLQRoundTrip(t *testing.T) {
	c, ctx := dlqTestClient(t)

	store, err := NewJetStreamDLQ(ctx, c.JetStream())
	if err != nil {
		t.Fatalf("provision: %v", err)
	}
	// Re-open without provisioning — the read-path seam.
	reopened, err := OpenJetStreamDLQ(ctx, c.JetStream())
	if err != nil {
		t.Fatalf("open: %v", err)
	}

	payload := []byte(fmt.Sprintf(`{"itest":%d}`, time.Now().UnixNano()))
	seq, err := store.Put(ctx, DeadLetter{
		Stream: "trades", Consumer: "itest-consumer",
		Subject: "ops-dlq-test.origin", Reason: "simulated poison",
		Deliveries: 5, Payload: payload,
		Headers: map[string]string{"Trace": "t1"},
	})
	if err != nil {
		t.Fatalf("put: %v", err)
	}
	if seq == 0 {
		t.Fatal("seq = 0")
	}

	list, err := reopened.List(ctx, ListFilter{Stream: "trades"})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	var found bool
	for _, dl := range list {
		if dl.Seq == seq {
			found = true
			if dl.Consumer != "itest-consumer" || string(dl.Payload) != string(payload) ||
				dl.Deliveries != 5 || dl.Headers["Trace"] != "t1" {
				t.Fatalf("round-trip mismatch: %+v", dl)
			}
		}
	}
	if !found {
		t.Fatalf("entry seq %d not listed", seq)
	}

	// Replay republishes through JetStream — js.Publish expects the
	// origin subject to be ingestible by a stream (production always
	// replays into a real stream subject). Bind the synthetic origin
	// to a throwaway stream so the cluster acks it.
	if _, err := c.JetStream().CreateOrUpdateStream(ctx, jetstream.StreamConfig{
		Name:      "itest-dlq-origin",
		Subjects:  []string{"ops-dlq-test.*"},
		Retention: jetstream.LimitsPolicy,
		Storage:   jetstream.MemoryStorage,
	}); err != nil {
		t.Fatalf("origin stream: %v", err)
	}
	defer func() { _ = c.JetStream().DeleteStream(ctx, "itest-dlq-origin") }()

	// Replay republishes to the origin subject — a JetStream-ingested
	// message is captured by the bound stream, not pushed to core
	// subscriptions, so observe it through a consumer (the same way a
	// production replay would reach the origin stream's consumers).
	cons, err := c.JetStream().CreateOrUpdateConsumer(ctx, "itest-dlq-origin",
		jetstream.ConsumerConfig{
			Durable:       "itest-dlq-consumer",
			FilterSubject: "ops-dlq-test.origin",
			AckPolicy:     jetstream.AckExplicitPolicy,
		})
	if err != nil {
		t.Fatalf("origin consumer: %v", err)
	}
	if err := reopened.Replay(ctx, seq); err != nil {
		t.Fatalf("replay: %v", err)
	}
	msgs, err := cons.Fetch(1, jetstream.FetchMaxWait(10*time.Second))
	if err != nil {
		t.Fatalf("fetch replayed: %v", err)
	}
	gotMsg := false
	for m := range msgs.Messages() {
		if string(m.Data()) != string(payload) {
			t.Fatalf("replayed payload = %s", m.Data())
		}
		_ = m.Ack()
		gotMsg = true
	}
	if !gotMsg {
		t.Fatal("replayed message never arrived on origin stream")
	}
	if _, err := reopened.Get(ctx, seq); !errors.Is(err, ErrNotFound) {
		t.Fatalf("entry still present after replay: %v", err)
	}

	// Discard path: put → discard → gone.
	seq2, err := store.Put(ctx, DeadLetter{
		Stream: "trades", Subject: "ops-dlq-test.origin",
		Reason: "discard-me", Payload: []byte("x")})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Discard(ctx, seq2); err != nil {
		t.Fatalf("discard: %v", err)
	}
	if _, err := store.Get(ctx, seq2); !errors.Is(err, ErrNotFound) {
		t.Fatalf("entry still present after discard: %v", err)
	}
}
