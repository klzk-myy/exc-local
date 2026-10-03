// quote_events.go — Task 7.3.9 feed seam, publisher half.
//
// Accepted FIX mass quotes (35=i) and quote withdrawals (35=Z) fan out
// as JSON events on the dedicated "quotes" JetStream stream under
// subject quotes.lp.{lp_id}.{symbol-token} (canonical "EUR/USD" →
// "EUR-USD", same token convention as the trades stream). The
// marketdata service consumes them via JetStreamLPQuoteSource →
// DecodeLPQuoteJSON — the shared wire schema lives in
// internal/marketdata/lp_pricing.go (lpQuoteJSON) — then applies the
// per-LP pricing config and fans lpBook@{lpID}/{symbol} onto the WS
// distribution.
//
// The sink is deliberately asynchronous: EmitLPQuote only enqueues —
// JetStream publish (an R3-quorum PubAck round trip) must never sit on
// the 35=i admission path. Run drains the queue on its own goroutine
// with a bounded per-publish timeout. Saturation evicts the OLDEST
// queued event (quote distribution conflates — the newest level is the
// truth) and counts via Dropped; publish failures likewise drop loudly
// rather than retry-loop, because the next quote or the downstream
// stale sweep repairs the feed.
package fix

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync/atomic"
	"time"

	"github.com/nats-io/nats.go/jetstream"

	excnats "exchange/internal/nats"
)

const (
	// LPQuoteStream is the canonical JetStream stream carrying venue-side
	// LP quote events — declared in internal/nats Streams (binds the
	// wildcard "quotes.>"). Mirrored marketdata-side as
	// marketdata.LPQuoteStream (the layering rule forbids the import —
	// internal/fix already depends on internal/marketdata).
	LPQuoteStream = "quotes"
	// LPQuoteSubjectPrefix scopes the per-(lp, symbol) ordering subjects:
	// quotes.lp.{lp_id}.{symbol-token}. Consumers filter
	// LPQuoteSubjectPrefix + ">" ("quotes.lp.>").
	LPQuoteSubjectPrefix = "quotes.lp."

	// defaultQuoteSinkBuffer bounds the pending-publish queue.
	defaultQuoteSinkBuffer = 8192
	// lpQuotePublishTimeout bounds each JetStream publish so a stalled
	// cluster cannot wedge the drain loop (or shutdown).
	lpQuotePublishTimeout = 3 * time.Second
)

// QuoteSinkConfig tunes JetStreamQuoteSink.
type QuoteSinkConfig struct {
	// Stream overrides the JetStream stream name; "" → LPQuoteStream.
	Stream string
	// Buffer is the pending-publish queue depth; ≤0 → defaultQuoteSinkBuffer.
	Buffer int
	// Logger is the operator log line; nil → no-op.
	Logger func(format string, args ...any)
}

// JetStreamQuoteSink is the production QuoteEventSink: it serializes
// LPQuoteEvents to the lpQuoteJSON wire shape and publishes them to
// "quotes" via the JetStream context (durable PubAck, Nats-Msg-Id dedup
// on the per-event sequence).
type JetStreamQuoteSink struct {
	nc     *excnats.Client
	stream string
	logf   func(string, ...any)
	in     chan LPQuoteEvent

	dropped atomic.Int64
}

// NewJetStreamQuoteSink binds the sink to a connected client. Run must
// be started by the caller (it owns stream provisioning).
func NewJetStreamQuoteSink(nc *excnats.Client, cfg QuoteSinkConfig) *JetStreamQuoteSink {
	if cfg.Stream == "" {
		cfg.Stream = LPQuoteStream
	}
	if cfg.Buffer <= 0 {
		cfg.Buffer = defaultQuoteSinkBuffer
	}
	if cfg.Logger == nil {
		cfg.Logger = func(string, ...any) {}
	}
	return &JetStreamQuoteSink{
		nc: nc, stream: cfg.Stream, logf: cfg.Logger,
		in: make(chan LPQuoteEvent, cfg.Buffer),
	}
}

// EmitLPQuote implements QuoteEventSink — enqueue only, never blocks on
// the broker. On a saturated queue the oldest pending event is evicted
// (conflation: a superseded quote level is worthless to distribute);
// the fresh event is then queued. Returns an error only when the event
// itself could not be queued after the eviction attempt.
func (s *JetStreamQuoteSink) EmitLPQuote(_ context.Context, ev LPQuoteEvent) error {
	select {
	case s.in <- ev:
		return nil
	default:
	}
	select {
	case <-s.in:
		s.dropped.Add(1)
		s.logf("fix quoting: lp quote sink saturated — oldest event evicted")
	default:
	}
	select {
	case s.in <- ev:
		return nil
	default:
		s.dropped.Add(1)
		return fmt.Errorf("fix quoting: lp quote sink queue full")
	}
}

// Dropped reports the number of quote events lost to saturation or
// publish failure — the fail-loud seam for metrics/alerts.
func (s *JetStreamQuoteSink) Dropped() int64 { return s.dropped.Load() }

// Run drains the queue until ctx is cancelled. The "quotes" stream is
// ensured first (idempotent — natsctl's EnsureStreams is the canonical
// provisioner, but a dev topology that skipped it still works); an
// ensure failure is logged and the loop continues — publishes surface
// their own errors.
func (s *JetStreamQuoteSink) Run(ctx context.Context) error {
	if _, err := s.nc.EnsureStream(ctx, s.stream); err != nil {
		s.logf("fix quoting: ensure stream %q failed: %v", s.stream, err)
	}
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case ev := <-s.in:
			if err := s.publish(ctx, ev); err != nil {
				if ctx.Err() != nil {
					return ctx.Err()
				}
				s.dropped.Add(1)
				s.logf("fix quoting: lp quote publish failed (lp=%d %s %s): %v",
					ev.LPID, ev.Kind, ev.Symbol, err)
			}
		}
	}
}

// publish serializes + publishes one event with the bounded timeout and
// a deterministic Nats-Msg-Id (dedup window covers publisher retries).
func (s *JetStreamQuoteSink) publish(ctx context.Context, ev LPQuoteEvent) error {
	subj, err := LPQuoteSubject(ev.LPID, ev.Symbol)
	if err != nil {
		return err
	}
	payload, err := EncodeLPQuoteEvent(ev)
	if err != nil {
		return err
	}
	pctx, cancel := context.WithTimeout(ctx, lpQuotePublishTimeout)
	defer cancel()
	_, err = s.nc.JetStream().Publish(pctx, subj, payload,
		jetstream.WithMsgID(fmt.Sprintf("lpq:%d:%s:%d", ev.LPID, ev.Symbol, ev.Seq)))
	if err != nil {
		return fmt.Errorf("nats: publish %q: %w", subj, err)
	}
	return nil
}

// LPQuoteSubject renders quotes.lp.{lp_id}.{symbol-token}. Tokens are
// validated against the same rule as excnats.Subject — a stray dot or
// wildcard would silently misroute the event.
func LPQuoteSubject(lpID int64, symbol string) (string, error) {
	if lpID <= 0 {
		return "", fmt.Errorf("fix quoting: lp_id %d invalid for quote subject", lpID)
	}
	tok := excnats.SymbolToken(symbol)
	if tok == "" || strings.ContainsAny(tok, ". *>\t\n\r") {
		return "", fmt.Errorf("fix quoting: symbol %q is not a valid subject token", symbol)
	}
	return fmt.Sprintf("%s%d.%s", LPQuoteSubjectPrefix, lpID, tok), nil
}

// lpQuoteEventJSON is the wire shape — field-for-field the lpQuoteJSON
// schema decoded by marketdata.DecodeLPQuoteJSON plus the "kind"
// discriminator ("UPDATE" | "WITHDRAW"; absent ≡ UPDATE for
// backward-compatible producers).
type lpQuoteEventJSON struct {
	Kind         string      `json:"kind"`
	LPID         int64       `json:"lp_id"`
	InstrumentID int64       `json:"instrument_id"`
	Symbol       string      `json:"symbol"`
	Bids         [][2]string `json:"bids"` // [price, qty] decimal strings
	Asks         [][2]string `json:"asks"`
	Seq          uint64      `json:"seq"`
	Ts           string      `json:"ts"` // RFC3339Nano — anchors the consumer staleness gate
}

// EncodeLPQuoteEvent serializes one event. Empty kind normalizes to
// UPDATE; WITHDRAW always encodes empty side arrays (the withdrawal
// semantic is also enforced on decode). Sides marshal as [] — never
// null — so downstream decoders see an explicit empty book.
func EncodeLPQuoteEvent(ev LPQuoteEvent) ([]byte, error) {
	kind := ev.Kind
	if kind == "" {
		kind = LPQuoteEventUpdate
	}
	w := lpQuoteEventJSON{
		Kind:         string(kind),
		LPID:         ev.LPID,
		InstrumentID: ev.InstrumentID,
		Symbol:       ev.Symbol,
		Seq:          ev.Seq,
		Ts:           ev.Ts.UTC().Format(time.RFC3339Nano),
		Bids:         [][2]string{},
		Asks:         [][2]string{},
	}
	if kind != LPQuoteEventWithdraw {
		for _, l := range ev.Bids {
			w.Bids = append(w.Bids, [2]string{l.Price.String(), l.Qty.String()})
		}
		for _, l := range ev.Asks {
			w.Asks = append(w.Asks, [2]string{l.Price.String(), l.Qty.String()})
		}
	}
	return json.Marshal(w)
}
