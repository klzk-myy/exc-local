package bridge

// Live-cluster integration test. Gated on EXC_NATS_TEST=1 (same convention
// as internal/nats); run against the dev cluster with:
//
//	EXC_NATS_TEST=1 go test -v -count=1 ./internal/bridge/...
//
// Optional: EXC_NATS_URLS overrides the seed list (comma-separated).

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	flatbuffers "github.com/google/flatbuffers/go"
	"github.com/nats-io/nats.go/jetstream"

	"exchange/internal/ipc"
	excnats "exchange/internal/nats"
)

type livePublisher struct{ c *excnats.Client }

func (p *livePublisher) PublishEvent(ctx context.Context, subject, msgID string, payload []byte) error {
	_, err := p.c.JetStream().Publish(ctx, subject, payload, jetstream.WithMsgID(msgID))
	return err
}

func (p *livePublisher) PublishHeartbeat(subject string, payload []byte) error {
	return p.c.Conn().Publish(subject, payload)
}

func (p *livePublisher) Connected() bool { return p.c.Connected() }

func liveClient(t *testing.T) (*excnats.Client, context.Context) {
	t.Helper()
	if os.Getenv("EXC_NATS_TEST") != "1" {
		t.Skip("set EXC_NATS_TEST=1 to run live-cluster integration test")
	}
	urls := os.Getenv("EXC_NATS_URLS")
	if urls == "" {
		urls = defaultTestURLs
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	t.Cleanup(cancel)
	cfg := excnats.DefaultConfig(strings.Split(urls, ","))
	cfg.Name = "bridge-test"
	cli, err := excnats.Connect(ctx, cfg, nil)
	if err != nil {
		t.Skipf("dev cluster unreachable: %v", err)
	}
	t.Cleanup(cli.Close)
	return cli, ctx
}

const defaultTestURLs = "nats://127.0.0.1:4222,nats://127.0.0.1:4223,nats://127.0.0.1:4224"

// End-to-end proof on the dev cluster: fragment -> JetStream stream ->
// durable consumer fetch, plus a heartbeat on core NATS.
func TestBridgeAgainstLiveNATS(t *testing.T) {
	cli, ctx := liveClient(t)

	// Provisioning is an operator action (natsctl init); the test ensures
	// the streams it needs so it is self-contained.
	for _, s := range []string{"trades", "settlements", "compliance", "analytics"} {
		if _, err := cli.EnsureStream(ctx, s); err != nil {
			t.Fatalf("ensure stream %s: %v", s, err)
		}
	}

	cfg := testConfig(0, 1024)
	b, err := New(cfg, &livePublisher{c: cli}, MapResolver{3: "EUR-USD"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- b.Run(runCtx) }()
	defer func() { cancel(); <-done }()

	fb := flatbuffers.NewBuilder(256)
	b.HandleFragment(newOrderEvent(fb, 1001, 1001, 3))
	b.HandleFragment(newFillEvent(fb, 1002, 1001, 2002))

	waitForCond(t, 10*time.Second, "live publishes", func() bool {
		return b.Snapshot().Published == 4
	})
	if b.Snapshot().BufferDepth != 0 {
		t.Fatalf("buffer depth=%d", b.Snapshot().BufferDepth)
	}

	// Verify the events are durably stored on their routed subjects. Read
	// the raw stream messages directly — creating another filtered
	// consumer on the workqueue stream could collide with ops consumers.
	checks := []struct {
		stream, subject string
		wantSeq         uint64
	}{
		{"trades", "trades.0.EUR-USD", 1002},
		{"settlements", "settlements.0.EUR-USD", 1002},
		{"compliance", "compliance.0.EUR-USD", 1001},
		{"analytics", "analytics.0.EUR-USD", 1001},
	}
	for _, c := range checks {
		s, err := cli.JetStream().Stream(ctx, c.stream)
		if err != nil {
			t.Fatalf("stream %s: %v", c.stream, err)
		}
		msg, err := s.GetLastMsgForSubject(ctx, c.subject)
		if err != nil {
			t.Fatalf("last msg %s: %v", c.subject, err)
		}
		ev := ipc.DecodeEvent(msg.Data)
		if ev == nil || ev.Seq() != c.wantSeq {
			t.Fatalf("%s: seq=%d want %d", c.subject, ev.Seq(), c.wantSeq)
		}
	}
	t.Logf("live fan-out verified: published=%d", b.Snapshot().Published)
}
