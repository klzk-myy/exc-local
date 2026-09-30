package fix

// Live-cluster integration test for the Task 7.3.9 feed seam. Gated on
// EXC_NATS_TEST=1 (repo convention — see internal/nats
// integration_test.go); run against the dev cluster with:
//
//	EXC_NATS_TEST=1 go test -v -count=1 ./internal/fix/ -run IntegrationLPQuoteFeed
//
// Optional: EXC_NATS_URLS overrides the seed list (comma-separated).

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"

	"exchange/internal/marketdata"
	excnats "exchange/internal/nats"
	"exchange/pkg/decimal"
)

// TestIntegrationLPQuoteFeed runs the real end-to-end: JetStreamQuoteSink
// publishes onto the "quotes" stream; a marketdata-side consumer
// (JetStreamMsgSource + JSONLPQuoteSource — the exact composition
// JetStreamLPQuoteSource binds, here with a per-run subject filter for
// history isolation under LimitsPolicy) decodes it back into an LPQuote.
func TestIntegrationLPQuoteFeed(t *testing.T) {
	if os.Getenv("EXC_NATS_TEST") != "1" {
		t.Skip("set EXC_NATS_TEST=1 to run live-cluster integration test")
	}
	urls := os.Getenv("EXC_NATS_URLS")
	if urls == "" {
		urls = "nats://127.0.0.1:4222,nats://127.0.0.1:4223,nats://127.0.0.1:4224"
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	nc, err := excnats.Connect(ctx, excnats.DefaultConfig(
		strings.Split(urls, ",")), log)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer nc.Close()
	if _, err := nc.EnsureStream(ctx, LPQuoteStream); err != nil {
		t.Fatalf("ensure stream %q: %v", LPQuoteStream, err)
	}

	// Unique lp_id per run — LimitsPolicy retains history, so a reused
	// subject would replay stale copies into the consumer's backlog.
	lpID := time.Now().UnixNano()%900000 + 100000
	durable := fmt.Sprintf("itest-lpq-%d", lpID)
	cons, err := nc.EnsureConsumer(ctx, LPQuoteStream, durable,
		excnats.WithFilterSubject(fmt.Sprintf("%s%d.>", LPQuoteSubjectPrefix, lpID)))
	if err != nil {
		t.Fatalf("ensure consumer: %v", err)
	}
	defer func() {
		if s, _ := nc.JetStream().Stream(context.Background(), LPQuoteStream); s != nil {
			_ = s.DeleteConsumer(context.Background(), durable)
		}
	}()
	src := marketdata.JSONLPQuoteSource(
		marketdata.JetStreamMsgSource(nc, LPQuoteStream, durable,
			fmt.Sprintf("%s%d.>", LPQuoteSubjectPrefix, lpID), log),
		marketdata.SubjectSymbols(marketdata.MapResolver{3: "EUR/USD"}), log)
	quotes, err := src.Quotes(ctx)
	if err != nil {
		t.Fatalf("quotes source: %v", err)
	}
	_ = cons // the MsgSource owns its own consumer handle

	sink := NewJetStreamQuoteSink(nc, QuoteSinkConfig{})
	go func() {
		if err := sink.Run(ctx); err != nil && ctx.Err() == nil {
			t.Logf("sink run: %v", err)
		}
	}()

	want := LPQuoteEvent{
		Kind: LPQuoteEventUpdate, LPID: lpID, InstrumentID: 3, Symbol: "EUR/USD",
		Bids: []LPQuoteLevel{{Price: decimal.RequireFromString("1.08000"),
			Qty: decimal.RequireFromString("1000000")}},
		Asks: []LPQuoteLevel{{Price: decimal.RequireFromString("1.08020"),
			Qty: decimal.RequireFromString("2000000")}},
		Seq: 1, Ts: time.Now().UTC(),
	}
	if err := sink.EmitLPQuote(ctx, want); err != nil {
		t.Fatalf("emit: %v", err)
	}
	if err := sink.EmitLPQuote(ctx, LPQuoteEvent{
		Kind: LPQuoteEventWithdraw, LPID: lpID, InstrumentID: 3,
		Symbol: "EUR/USD", Seq: 2, Ts: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("emit withdraw: %v", err)
	}

	deadline := time.After(30 * time.Second)
	var got []marketdata.LPQuote
	for len(got) < 2 {
		select {
		case <-deadline:
			t.Fatalf("timed out waiting for quote events (got %d)", len(got))
		case q, ok := <-quotes:
			if !ok {
				t.Fatal("quote channel closed early")
			}
			got = append(got, q)
		}
	}
	if got[0].LPID != lpID || got[0].Symbol != "EUR/USD" ||
		got[0].InstrumentID != 3 || len(got[0].Bids) != 1 ||
		len(got[0].Asks) != 1 || got[0].Seq != 1 {
		t.Fatalf("update = %+v", got[0])
	}
	if len(got[1].Bids) != 0 || len(got[1].Asks) != 0 {
		t.Fatalf("withdraw must decode empty-sided: %+v", got[1])
	}
}
