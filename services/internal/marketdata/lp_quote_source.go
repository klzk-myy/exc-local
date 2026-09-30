// lp_quote_source.go — Task 7.3.9 feed seam, consumer half.
//
// The FIX mass-quote path (internal/fix QuoteService → JetStreamQuoteSink,
// Task 18.3.7) publishes venue-side LP quote events as JSON on the
// dedicated "quotes" JetStream stream under
// quotes.lp.{lp_id}.{symbol-token} ("EUR/USD" → "EUR-USD" — the same
// subject-token convention as the trades stream). This file binds that
// stream to the LPBookProducer: a durable pull consumer filtered on
// "quotes.lp.>" decoded by the shared lpQuoteJSON wire schema
// (DecodeLPQuoteJSON in lp_pricing.go — lp_id rides the second-to-last
// subject token, the symbol the last).
//
// Stream/durable constant naming mirrors fix.LPQuoteStream /
// fix.LPQuoteSubjectPrefix — duplicated per the layering rule
// (internal/marketdata must not import internal/fix; fix already
// depends on marketdata for the 35=V feed).
package marketdata

import (
	"log/slog"

	excnats "exchange/internal/nats"
)

const (
	// LPQuoteStream is the canonical JetStream stream for LP quote
	// events (bound wildcard "quotes.>").
	LPQuoteStream = "quotes"
	// LPQuoteSubjectFilter covers every per-(lp, symbol) quote subject
	// published by the FIX path.
	LPQuoteSubjectFilter = "quotes.lp.>"
	// LPQuoteDurable is the marketdata consumer's stable durable name —
	// restart/redeploy resumes at the durable's ack floor
	// (at-least-once; a redelivered quote simply re-applies the filter).
	LPQuoteDurable = "marketdata-lp-quotes"
)

// JetStreamLPQuoteSource binds the "quotes" stream to LPQuoteSource:
// JetStreamMsgSource supplies the durable pull consumer with the
// canonical ack discipline (Ack only after channel handoff — a
// saturated consumer leaves the message for AckWait redelivery), and
// JSONLPQuoteSource decodes payloads with subject fallback for
// lp_id/symbol. stream/durable "" select the canonical values.
func JetStreamLPQuoteSource(nc *excnats.Client, stream, durable string,
	symbols SubjectSymbol, log *slog.Logger) LPQuoteSource {
	if stream == "" {
		stream = LPQuoteStream
	}
	if durable == "" {
		durable = LPQuoteDurable
	}
	return JSONLPQuoteSource(
		JetStreamMsgSource(nc, stream, durable, LPQuoteSubjectFilter, log),
		symbols, log)
}
