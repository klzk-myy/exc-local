package fix

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"exchange/internal/marketdata"
	"exchange/internal/marketmaking"
	"exchange/internal/orders"
	"exchange/pkg/decimal"
	excerrors "exchange/pkg/errors"
)

// fakeQuoteSink records the distribution-facing LP quote stream.
type fakeQuoteSink struct {
	mu  sync.Mutex
	evs []LPQuoteEvent
	err error
}

func (f *fakeQuoteSink) EmitLPQuote(_ context.Context, ev LPQuoteEvent) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return f.err
	}
	f.evs = append(f.evs, ev)
	return nil
}

func (f *fakeQuoteSink) events() []LPQuoteEvent {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]LPQuoteEvent(nil), f.evs...)
}

// quoteSvcWithSink builds a QuoteService with the LP gate wired (account
// 7 → LP 5, clear) and the feed sink bound.
func quoteSvcWithSink(t *testing.T, pipe *fakePipeline) (*QuoteService, *fakeQuoteSink) {
	t.Helper()
	ent := &fakeEntitlement{programs: map[string]*marketmaking.Program{
		"7|42": activeProgram(42), "7|43": activeProgram(43),
	}}
	svc, err := NewQuoteService(pipe, ent, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	svc.WithLPGate(&fakeLPResolver{lp: map[int64]int64{7: 5}},
		&fakeLPGuard{suspended: map[string]string{}})
	sink := &fakeQuoteSink{}
	svc.WithQuoteEventSink(sink)
	return svc, sink
}

func TestLPQuoteSubject(t *testing.T) {
	subj, err := LPQuoteSubject(7, "EUR/USD")
	if err != nil {
		t.Fatalf("subject: %v", err)
	}
	if subj != "quotes.lp.7.EUR-USD" {
		t.Fatalf("subject = %q, want quotes.lp.7.EUR-USD", subj)
	}
	for _, bad := range []struct {
		lpID int64
		sym  string
	}{{0, "EUR/USD"}, {-1, "EUR/USD"}, {7, ""}, {7, "EUR.USD"}, {7, "EUR *"}} {
		if _, err := LPQuoteSubject(bad.lpID, bad.sym); err == nil {
			t.Fatalf("subject (%d, %q) must fail", bad.lpID, bad.sym)
		}
	}
}

// TestEncodeLPQuoteEventRoundTrip pins the wire contract: the fix-side
// encoder output must decode cleanly through marketdata's
// DecodeLPQuoteJSON, subject fallback included.
func TestEncodeLPQuoteEventRoundTrip(t *testing.T) {
	ts := time.Date(2026, 1, 2, 3, 4, 5, 123456789, time.UTC)
	ev := LPQuoteEvent{
		Kind: LPQuoteEventUpdate, LPID: 7, InstrumentID: 3, Symbol: "EUR/USD",
		Bids: []LPQuoteLevel{
			{Price: decimal.RequireFromString("1.08000"), Qty: decimal.RequireFromString("1000000")},
			{Price: decimal.RequireFromString("1.07990"), Qty: decimal.RequireFromString("500000")},
		},
		Asks: []LPQuoteLevel{
			{Price: decimal.RequireFromString("1.08020"), Qty: decimal.RequireFromString("2000000")},
		},
		Seq: 42, Ts: ts,
	}
	subj, err := LPQuoteSubject(ev.LPID, ev.Symbol)
	if err != nil {
		t.Fatalf("subject: %v", err)
	}
	payload, err := EncodeLPQuoteEvent(ev)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	q, err := marketdata.DecodeLPQuoteJSON(payload, subj,
		marketdata.SubjectSymbols(marketdata.MapResolver{3: "EUR/USD"}))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if q.LPID != 7 || q.InstrumentID != 3 || q.Symbol != "EUR/USD" ||
		q.Seq != 42 || len(q.Bids) != 2 || len(q.Asks) != 1 {
		t.Fatalf("decoded quote = %+v", q)
	}
	if q.Bids[0].Price.String() != "1.08" || q.Bids[0].Qty.String() != "1000000" ||
		q.Asks[0].Price.String() != "1.0802" {
		t.Fatalf("decoded levels = %+v / %+v", q.Bids, q.Asks)
	}
	if !q.Ts.Equal(ts) {
		t.Fatalf("decoded ts = %v, want %v", q.Ts, ts)
	}
}

func TestEncodeLPQuoteEventWithdraw(t *testing.T) {
	payload, err := EncodeLPQuoteEvent(LPQuoteEvent{
		Kind: LPQuoteEventWithdraw, LPID: 7, InstrumentID: 3,
		Symbol: "EUR/USD", Seq: 9, Ts: time.Now().UTC(),
	})
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	if !strings.Contains(string(payload), `"kind":"WITHDRAW"`) ||
		!strings.Contains(string(payload), `"bids":[]`) {
		t.Fatalf("withdraw payload = %s", payload)
	}
	// The defensive half: a withdraw carrying stray levels still decodes
	// to an empty book.
	q, err := marketdata.DecodeLPQuoteJSON(
		[]byte(`{"kind":"WITHDRAW","lp_id":7,"symbol":"EUR/USD",`+
			`"bids":[["9.99","1"]],"ts_ms":1767323045123}`),
		"quotes.lp.7.EUR-USD", nil)
	if err != nil {
		t.Fatalf("decode withdraw: %v", err)
	}
	if len(q.Bids) != 0 || len(q.Asks) != 0 {
		t.Fatalf("withdraw must pin empty sides: %+v", q)
	}
}

func TestQuoteServiceEmitsLPQuoteEvents(t *testing.T) {
	pipe := newFakePipeline()
	svc, sink := quoteSvcWithSink(t, pipe)
	ctx := context.Background()

	ack, err := svc.SubmitMassQuote(ctx, "sess", 7, &MassQuote{
		QuoteSetID: "s1",
		Entries: []QuoteEntry{
			twoSidedEntry("e1", "EURUSD"), twoSidedEntry("e2", "GBPUSD"),
		},
	})
	if err != nil || !ack.Accepted() {
		t.Fatalf("set must accept: %v %+v", err, ack)
	}
	evs := sink.events()
	if len(evs) != 2 {
		t.Fatalf("one UPDATE per (lp, symbol): %d events", len(evs))
	}
	bySym := map[string]LPQuoteEvent{}
	for _, ev := range evs {
		bySym[ev.Symbol] = ev
		if ev.Kind != LPQuoteEventUpdate || ev.LPID != 5 || ev.Ts.IsZero() ||
			ev.Seq == 0 {
			t.Fatalf("event = %+v", ev)
		}
	}
	eur := bySym["EUR/USD"]
	if eur.InstrumentID != 42 || len(eur.Bids) != 1 || len(eur.Asks) != 1 ||
		eur.Bids[0].Price.String() != "1.1" || eur.Asks[0].Price.String() != "1.1002" {
		t.Fatalf("eur event = %+v", eur)
	}
	if _, ok := bySym["GBP/USD"]; !ok {
		t.Fatalf("missing GBP/USD event: %v", evs)
	}
	if evs[0].Seq >= evs[1].Seq {
		t.Fatalf("event seq must be monotonic: %d → %d", evs[0].Seq, evs[1].Seq)
	}

	// 35=Z per-set cancel → WITHDRAW per affected symbol.
	if _, err := svc.CancelQuotes(ctx, "sess", 7,
		&QuoteCancel{QuoteSetID: "s1"}); err != nil {
		t.Fatal(err)
	}
	evs = sink.events()
	if len(evs) != 4 {
		t.Fatalf("2 updates + 2 withdraws: %d events", len(evs))
	}
	for _, ev := range evs[2:] {
		if ev.Kind != LPQuoteEventWithdraw || ev.LPID != 5 ||
			len(ev.Bids) != 0 || len(ev.Asks) != 0 {
			t.Fatalf("withdraw event = %+v", ev)
		}
	}
}

// Two entries on the SAME symbol aggregate into one multi-level update —
// the lpBook feed carries the LP's full ladder, not per-entry frames.
func TestQuoteServiceAggregatesPerSymbol(t *testing.T) {
	pipe := newFakePipeline()
	svc, sink := quoteSvcWithSink(t, pipe)

	ack, err := svc.SubmitMassQuote(context.Background(), "sess", 7, &MassQuote{
		QuoteSetID: "s1",
		Entries: []QuoteEntry{
			{QuoteEntryID: "e1", Symbol: "EURUSD",
				BidPx: decPtr("1.1000"), BidSize: decPtr("100000"),
				OfferPx: decPtr("1.1002"), OfferSize: decPtr("100000")},
			{QuoteEntryID: "e2", Symbol: "EURUSD",
				BidPx: decPtr("1.0995"), BidSize: decPtr("250000"),
				OfferPx: decPtr("1.1005"), OfferSize: decPtr("250000")},
		},
	})
	if err != nil || !ack.Accepted() {
		t.Fatalf("set must accept: %v %+v", err, ack)
	}
	evs := sink.events()
	if len(evs) != 1 {
		t.Fatalf("same-symbol entries must aggregate to one update: %d", len(evs))
	}
	ev := evs[0]
	if len(ev.Bids) != 2 || len(ev.Asks) != 2 ||
		ev.Bids[1].Price.String() != "1.0995" || ev.Asks[1].Qty.String() != "250000" {
		t.Fatalf("aggregated levels = %+v / %+v", ev.Bids, ev.Asks)
	}
}

func TestQuoteServiceWithdrawsOnSymbolAndAllCancels(t *testing.T) {
	pipe := newFakePipeline()
	svc, sink := quoteSvcWithSink(t, pipe)
	ctx := context.Background()
	if _, err := svc.SubmitMassQuote(ctx, "sess", 7, &MassQuote{
		QuoteSetID: "s1",
		Entries: []QuoteEntry{
			twoSidedEntry("e1", "EURUSD"), twoSidedEntry("e2", "GBPUSD"),
		},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.CancelQuotes(ctx, "sess", 7, &QuoteCancel{
		CancelType: QuoteCancelPerSymbol, Symbol: "EURUSD",
	}); err != nil {
		t.Fatal(err)
	}
	evs := sink.events()
	if len(evs) != 3 || evs[2].Kind != LPQuoteEventWithdraw ||
		evs[2].Symbol != "EUR/USD" || evs[2].InstrumentID != 42 {
		t.Fatalf("per-symbol withdraw missing: %+v", evs)
	}
	// All-quotes cancel withdraws every symbol the session tracked.
	if _, err := svc.CancelQuotes(ctx, "sess", 7,
		&QuoteCancel{CancelType: QuoteCancelAllQuotes}); err != nil {
		t.Fatal(err)
	}
	evs = sink.events()
	if len(evs) != 4 || evs[3].Kind != LPQuoteEventWithdraw ||
		evs[3].Symbol != "GBP/USD" {
		t.Fatalf("all-quotes withdraw missing: %+v", evs)
	}
}

// A same-set requote whose replacement leg fails leaves the book bare —
// the feed must withdraw the symbol's previously distributed levels.
func TestQuoteServiceWithdrawsOnFailedReplace(t *testing.T) {
	pipe := newFakePipeline()
	svc, sink := quoteSvcWithSink(t, pipe)
	ctx := context.Background()
	if _, err := svc.SubmitMassQuote(ctx, "sess", 7, &MassQuote{
		QuoteSetID: "s1", Entries: []QuoteEntry{twoSidedEntry("e1", "EURUSD")},
	}); err != nil {
		t.Fatal(err)
	}
	pipe.submitErrFor["mq:s1:2:e1:BUY"] = excerrors.New("ORDER_REJECTED", "post failed")
	ack, err := svc.SubmitMassQuote(ctx, "sess", 7, &MassQuote{
		QuoteSetID: "s1", Entries: []QuoteEntry{twoSidedEntry("e1", "EURUSD")},
	})
	if err != nil {
		t.Fatal(err)
	}
	if ack.Entries[0].Status != QuoteStatusRejected {
		t.Fatalf("entry must reject: %+v", ack.Entries[0])
	}
	evs := sink.events()
	if len(evs) != 2 || evs[1].Kind != LPQuoteEventWithdraw ||
		evs[1].Symbol != "EUR/USD" {
		t.Fatalf("failed replace must withdraw: %+v", evs)
	}
}

// Accounts without an lp_accounts binding have no lpBook channel — the
// feed stays silent even though quoting proceeds normally.
func TestQuoteServiceNoEmitForNonLP(t *testing.T) {
	pipe := newFakePipeline()
	pipe.acct2 = &orders.Account{ID: 8}
	ent := &fakeEntitlement{programs: map[string]*marketmaking.Program{
		"8|43": activeProgram(43)}}
	svc, err := NewQuoteService(pipe, ent, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	svc.WithLPGate(&fakeLPResolver{lp: map[int64]int64{}},
		&fakeLPGuard{suspended: map[string]string{}})
	sink := &fakeQuoteSink{}
	svc.WithQuoteEventSink(sink)
	ack, err := svc.SubmitMassQuote(context.Background(), "sess", 8, &MassQuote{
		QuoteSetID: "s1", Entries: []QuoteEntry{twoSidedEntry("e1", "GBPUSD")},
	})
	if err != nil || !ack.Accepted() {
		t.Fatalf("unbound account must quote: %v %+v", err, ack)
	}
	if len(sink.events()) != 0 {
		t.Fatalf("non-LP account emitted: %+v", sink.events())
	}
}

// Sink errors are logged and swallowed — distribution must never fail
// firm quote admission.
func TestQuoteServiceSinkErrorNonFatal(t *testing.T) {
	pipe := newFakePipeline()
	svc, sink := quoteSvcWithSink(t, pipe)
	sink.err = errors.New("nats down")
	ack, err := svc.SubmitMassQuote(context.Background(), "sess", 7, &MassQuote{
		QuoteSetID: "s1", Entries: []QuoteEntry{twoSidedEntry("e1", "EURUSD")},
	})
	if err != nil || !ack.Accepted() {
		t.Fatalf("sink error must not fail admission: %v %+v", err, ack)
	}
}

// Saturation evicts the OLDEST queued event (conflation) and counts it.
func TestJetStreamQuoteSinkSaturation(t *testing.T) {
	s := NewJetStreamQuoteSink(nil, QuoteSinkConfig{Buffer: 1})
	ev := LPQuoteEvent{Kind: LPQuoteEventUpdate, LPID: 7, Symbol: "EUR/USD"}
	if err := s.EmitLPQuote(context.Background(), ev); err != nil {
		t.Fatalf("first enqueue: %v", err)
	}
	if err := s.EmitLPQuote(context.Background(), ev); err != nil {
		t.Fatalf("conflation enqueue: %v", err)
	}
	if s.Dropped() != 1 {
		t.Fatalf("dropped = %d, want 1", s.Dropped())
	}
	got := <-s.in
	if got.Symbol != "EUR/USD" {
		t.Fatalf("queue holds %+v", got)
	}
}
