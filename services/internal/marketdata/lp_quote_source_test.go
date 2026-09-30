package marketdata

import (
	"context"
	"testing"
)

// msgSourceOf fabricates a MsgSource replaying the supplied messages
// then closing — the transport-agnostic test seam for the quote
// adapters (mirrors the decode-side tests' subject-fallback cases).
func msgSourceOf(msgs ...RawMsg) MsgSource {
	return func(context.Context) (<-chan RawMsg, error) {
		ch := make(chan RawMsg, len(msgs))
		for _, m := range msgs {
			ch <- m
		}
		close(ch)
		return ch, nil
	}
}

// The FIX sink's wire contract end-to-end at the decode seam: a
// quotes.lp.{lpID}.{symbol-token} subject + JSON payload decodes into
// LPQuote with subject fallback filling lp_id/symbol.
func TestJSONLPQuoteSourceQuotesLP(t *testing.T) {
	payload := []byte(`{"kind":"UPDATE","instrument_id":3,` +
		`"bids":[["1.08000","1000000"]],"asks":[["1.08020","500000"]],` +
		`"seq":7,"ts_ms":1767323045123}`)
	src := JSONLPQuoteSource(
		msgSourceOf(RawMsg{Subject: "quotes.lp.7.EUR-USD", Data: payload}),
		SubjectSymbols(MapResolver{3: "EUR/USD"}), nil)
	ctx := context.Background()
	ch, err := src.Quotes(ctx)
	if err != nil {
		t.Fatalf("quotes: %v", err)
	}
	q, ok := <-ch
	if !ok {
		t.Fatal("channel closed before quote")
	}
	if q.LPID != 7 || q.InstrumentID != 3 || q.Symbol != "EUR/USD" ||
		q.Seq != 7 || len(q.Bids) != 1 || len(q.Asks) != 1 {
		t.Fatalf("quote = %+v", q)
	}
	if q.Ts.UnixMilli() != 1767323045123 {
		t.Fatalf("ts = %v", q.Ts)
	}
	if _, ok := <-ch; ok {
		t.Fatal("channel must close after the replay ends")
	}
}

// kind=WITHDRAW decodes to an empty-sided quote even if the payload
// still carries levels — the withdrawal semantic is enforced at decode.
func TestDecodeLPQuoteJSONWithdrawKind(t *testing.T) {
	q, err := DecodeLPQuoteJSON(
		[]byte(`{"kind":"WITHDRAW","lp_id":7,"symbol":"EUR/USD",`+
			`"bids":[["9.99","1"]],"ts_ms":1767323045123}`),
		"", nil)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(q.Bids) != 0 || len(q.Asks) != 0 || q.LPID != 7 {
		t.Fatalf("withdraw must pin empty sides: %+v", q)
	}
	// kind "" and "UPDATE" both decode as ordinary updates.
	for _, kind := range []string{"", "UPDATE"} {
		payload := `{"instrument_id":3,"bids":[["1.1","2"]],"asks":[],"ts_ms":1767323045123}`
		if kind != "" {
			payload = `{"kind":"` + kind + `",` + payload[1:]
		}
		q, err := DecodeLPQuoteJSON([]byte(payload), "quotes.lp.7.EUR-USD",
			SubjectSymbols(MapResolver{3: "EUR/USD"}))
		if err != nil {
			t.Fatalf("kind %q decode: %v", kind, err)
		}
		if len(q.Bids) != 1 || q.Symbol != "EUR/USD" || q.LPID != 7 {
			t.Fatalf("kind %q quote = %+v", kind, q)
		}
	}
}

// A malformed payload is dropped and logged, never fatal — the feed
// survives a corrupt message (mirrors JetStreamMsgSource posture).
func TestJSONLPQuoteSourceDropsMalformed(t *testing.T) {
	src := JSONLPQuoteSource(msgSourceOf(
		RawMsg{Subject: "quotes.lp.7.EUR-USD", Data: []byte(`{bad`)},
		RawMsg{Subject: "quotes.lp.7.EUR-USD", Data: []byte(
			`{"instrument_id":3,"bids":[["1.1","2"]],"asks":[],"ts_ms":1767323045123}`)},
	), SubjectSymbols(MapResolver{3: "EUR/USD"}), nil)
	ch, err := src.Quotes(context.Background())
	if err != nil {
		t.Fatalf("quotes: %v", err)
	}
	q, ok := <-ch
	if !ok || len(q.Bids) != 1 {
		t.Fatalf("good quote must survive the malformed one: %+v", q)
	}
	// The malformed message must not poison the channel close.
	if _, ok := <-ch; ok {
		t.Fatal("channel must close after replay")
	}
}
