package analytics

// JetStream -> ClickHouse consumer (Task 20.3.1 item 3, spec §2.3.1/§16.6).
//
// One Consumer goroutine per stream (the daemon runs two: durable
// `ch-etl` on `trades` and `ch-etl` on `analytics`). Pulls via the
// canonical durable pull consumer (explicit ack, AckWait 30s,
// MaxDeliver 5), decodes the flatbuffers Event envelope, and inserts:
//
//	trades.{shard}.{symbol}   TradeFill -> ticks + trades
//	analytics.{shard}.{symbol} OrderNew -> fill-rate denominator metric
//	                           BookSnapshot/OrderAmend/OrderCancel -> counted
//
// Ack discipline (at-least-once): a fetched batch is acked only after
// every decoded row is durable — either committed to ClickHouse or synced
// to the disk spool. If InsertWithSpool reports total failure the whole
// batch is Nak'd for redelivery; we never ack an unpersisted row.

import (
	"context"
	"io"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/nats-io/nats.go/jetstream"

	"exchange/internal/ipc"
	"exchange/internal/ipc/wire"
	excnats "exchange/internal/nats"
	"exchange/internal/tracing"
)

// ConsumerOptions tunes the pull loop.
type ConsumerOptions struct {
	// FetchBatch caps one pull (default 1024). Combined with
	// FetchMaxWait this bounds tick latency: a batch inserts as soon as
	// the fetch returns, so worst-case enqueue->insert is ~maxWait +
	// insert — well under the 1s real-time contract.
	FetchBatch int
	// FetchMaxWait bounds one pull's wait (default 500ms).
	FetchMaxWait time.Duration
	// OnTradeFill, when set, receives every decoded TradeFill as
	// (canonical symbol, tradeID, seq, priceE8, qtyE8, ts) — prices and
	// quantities arrive int64 scaled 1e8 (wire convention). It runs
	// synchronously inside batch decode, before the batch is counted
	// durable, so it must be cheap — the candle engine's in-memory
	// aggregate is the intended consumer. The wire fill carries no
	// taker-side flag; consumers needing side resolve it through
	// OrderNew state themselves (this consumer never emits a guessed
	// side).
	OnTradeFill func(symbol string, tradeID, seq uint64, priceE8, qtyE8 int64, ts time.Time)
}

// orderMeta is the OrderNew-fed enrichment record: fills carry only order
// ids, so the trades-table account/instrument columns resolve through
// this index (same pattern as the Bridge's order index in route.go).
type orderMeta struct {
	accountID    uint64
	instrumentID uint32
}

// orderIndex is shared across this daemon's stream consumers — the
// `analytics` stream sees OrderNew, the `trades` stream sees fills; a
// single mutex-guarded map joins them. Bounded by FIFO eviction like the
// Bridge's index (engine order ids are monotonic, so evicting oldest
// approximates LRU).
type orderIndex struct {
	mu    sync.Mutex
	m     map[uint64]orderMeta
	fifo  []uint64
	start int
	cap   int
}

func newOrderIndex(capacity int) *orderIndex {
	if capacity < 1 {
		capacity = 1
	}
	return &orderIndex{m: make(map[uint64]orderMeta, 1024), cap: capacity}
}

func (o *orderIndex) put(orderID uint64, meta orderMeta) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if _, exists := o.m[orderID]; exists {
		o.m[orderID] = meta
		return
	}
	for len(o.m) >= o.cap {
		old := o.fifo[o.start]
		o.start++
		delete(o.m, old)
	}
	if o.start > 0 && o.start*2 >= len(o.fifo) {
		o.fifo = append([]uint64(nil), o.fifo[o.start:]...)
		o.start = 0
	}
	o.m[orderID] = meta
	o.fifo = append(o.fifo, orderID)
}

func (o *orderIndex) get(orderID uint64) (orderMeta, bool) {
	o.mu.Lock()
	defer o.mu.Unlock()
	v, ok := o.m[orderID]
	return v, ok
}

// dayCounters accumulates per-(symbol, day) order-event counters within
// one fetch batch; flushed as '1d' counters-only volume_stats delta rows
// (same shape as VolumeStatsStore.SyncOrdersCount — volume columns zero
// so the '1h' volume axes are untouched).
type dayCounters struct {
	submitted uint64 // OrderNew events
	filled    uint64 // TradeFill events
}

// Consumer owns one durable pull subscription against one stream.
type Consumer struct {
	nats *excnats.Client
	ing  *Ingester
	opts ConsumerOptions
	m    *IngestMetrics
	log  *slog.Logger
	// idx is shared by every stream consumer this daemon runs — OrderNew
	// events (analytics stream) populate it; TradeFill events (trades
	// stream) resolve account/instrument ids through it.
	idx *orderIndex
}

// NewConsumer wires the pull loop.
func NewConsumer(nc *excnats.Client, ing *Ingester, opts ConsumerOptions, m *IngestMetrics, log *slog.Logger) *Consumer {
	if opts.FetchBatch <= 0 {
		opts.FetchBatch = 1024
	}
	if opts.FetchMaxWait <= 0 {
		opts.FetchMaxWait = 500 * time.Millisecond
	}
	if m == nil {
		m = ing.Metrics()
	}
	if log == nil {
		log = slog.Default()
	}
	return &Consumer{nats: nc, ing: ing, opts: opts, m: m, log: log,
		idx: newOrderIndex(1 << 20)}
}

// RunStream consumes `stream` under durable `durable` until ctx cancels.
// The durable is created (idempotent EnsureConsumer) filtered to the
// stream's full subject space — one consumer owns the whole stream
// (work-queue semantics forbid overlapping filters anyway).
func (c *Consumer) RunStream(ctx context.Context, stream, durable string) error {
	cons, err := c.nats.EnsureConsumer(ctx, stream, durable,
		excnats.WithFilterSubject(stream+".>"))
	if err != nil {
		return err
	}
	c.log.Info("analytics consumer started",
		"stream", stream, "durable", durable,
		"fetch_batch", c.opts.FetchBatch, "max_wait", c.opts.FetchMaxWait)

	for ctx.Err() == nil {
		msgs, err := c.nats.Fetch(ctx, cons, c.opts.FetchBatch, c.opts.FetchMaxWait)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			// Transient fetch failure (leadership change, reconnect) —
			// back off briefly; durable state survives.
			c.log.Warn("nats fetch failed", "stream", stream, "err", err)
			select {
			case <-ctx.Done():
			case <-time.After(time.Second):
			}
			continue
		}
		if len(msgs) == 0 {
			continue
		}
		c.m.incMsgsFetched(len(msgs))
		if err := c.process(ctx, stream, msgs); err != nil {
			if ctx.Err() != nil {
				return nil
			}
			c.log.Warn("batch not durable; nacking", "stream", stream,
				"msgs", len(msgs), "err", err)
		}
	}
	return nil
}

// parseSubject splits "{stream}.{shard}.{symbol}" — symbols are single
// tokens (guaranteed by nats.Subject), shard parses to uint32.
func parseSubject(subj string) (shard uint32, symbol string, ok bool) {
	parts := strings.Split(subj, ".")
	if len(parts) != 3 {
		return 0, "", false
	}
	v, err := strconv.ParseUint(parts[1], 10, 32)
	if err != nil {
		return 0, "", false
	}
	// The subject token is the NATS-safe form ("EUR-USD"); every
	// ClickHouse projection stores the canonical instrument symbol
	// ("EUR/USD") — the same mapping marketdata.SubjectSymbol applies —
	// so REST queries, CHSessionVWAP and the candle engine all key on
	// one convention.
	return uint32(v), strings.Replace(parts[2], "-", "/", 1), true
}

// process decodes one fetch batch, inserts the resulting rows, and acks
// (or naks on total failure). Malformed payloads are acked AND counted —
// they are unrecoverable poison, not a transient fault; redelivery would
// loop forever against MaxDeliver.
func (c *Consumer) process(ctx context.Context, stream string, msgs []jetstream.Msg) error {
	var tickRows, tradeRows [][]any
	// Per-(symbol, day) order counters -> '1d' volume_stats delta rows.
	type statKey struct {
		symbol string
		day    time.Time
	}
	stats := map[statKey]*dayCounters{}
	bump := func(symbol string, ts time.Time) *dayCounters {
		k := statKey{symbol, dayOnlyUTC(ts)}
		b := stats[k]
		if b == nil {
			b = &dayCounters{}
			stats[k] = b
		}
		return b
	}

	for _, msg := range msgs {
		shard, symbol, ok := parseSubject(msg.Subject())
		if !ok {
			// A malformed subject keeps its ordering domain explicit rather
			// than dropping the row (fail-closed §2.7).
			shard, symbol = 0, "UNKNOWN"
		}
		c.handleMsg(msg, shard, symbol, &tickRows, &tradeRows, bump)
	}

	// '1d' counters-only delta rows: the volume projection itself is the
	// ticks MV's job ('1h' rows); these carry only orders_submitted /
	// orders_filled so FillRates has a durable feed (Task 20.3.5 contract).
	var statRows [][]any
	for k, b := range stats {
		if b.filled == 0 && b.submitted == 0 {
			continue
		}
		statRows = append(statRows, VolumeStatCounters(k.symbol, k.day, b.submitted, b.filled))
	}

	// Fail-closed ordering: ticks first, then trades, then stat deltas; a
	// failure leaves the batch un-acked and Nak'd below.
	if err := c.ing.InsertWithSpool(ctx, "ticks", tickRows); err != nil {
		c.nak(msgs)
		return err
	}
	if err := c.ing.InsertWithSpool(ctx, "trades", tradeRows); err != nil {
		c.nak(msgs)
		return err
	}
	if err := c.ing.InsertWithSpool(ctx, "volume_stats", statRows); err != nil {
		c.nak(msgs)
		return err
	}
	c.m.incFills(len(tickRows))
	c.ack(msgs)
	return nil
}

func (c *Consumer) ack(msgs []jetstream.Msg) {
	for _, m := range msgs {
		if err := m.Ack(); err != nil {
			c.log.Warn("nats ack failed (redelivery expected)", "err", err)
			continue
		}
	}
	c.m.incAcked(len(msgs))
}

func (c *Consumer) nak(msgs []jetstream.Msg) {
	for _, m := range msgs {
		if err := m.Nak(); err != nil {
			c.log.Warn("nats nak failed", "err", err)
		}
	}
	c.m.incNakd(len(msgs))
}

// handleMsg decodes one JetStream message and appends its rows.
// flatbuffers accessors panic on malformed buffers — the deferred
// recover degrades any such panic to "malformed" (counted, acked by the
// batch) instead of crashing the consumer; redelivery can never fix a
// poison frame. Same convention as internal/reporting's consumer.
func (c *Consumer) handleMsg(msg jetstream.Msg, shard uint32, symbol string,
	tickRows, tradeRows *[][]any, bump func(string, time.Time) *dayCounters) {
	defer func() {
		if recover() != nil {
			c.m.incMalformed()
		}
	}()

	data, err := msgData(msg)
	if err != nil || len(data) == 0 {
		c.m.incMalformed()
		return
	}
	body, _, _ := tracing.StripAeronTrace(data)
	ev := ipc.DecodeEvent(body)
	if ev == nil {
		c.m.incMalformed()
		return
	}
	switch ev.TypeType() {
	case wire.EventTypeTradeFill:
		tf := ipc.EventTradeFill(ev)
		if tf == nil {
			c.m.incMalformed()
			return
		}
		ts := time.Unix(0, int64(ev.Ts())).UTC()
		// Enrich via the OrderNew-fed index: buy/sell order ids ->
		// account ids + instrument id. 0 = unresolved (never guessed).
		var instrumentID uint32
		var makerAcct, takerAcct int64
		if meta, hit := c.idx.get(tf.BuyOrderId()); hit {
			makerAcct = int64(meta.accountID)
			instrumentID = meta.instrumentID
		}
		if meta, hit := c.idx.get(tf.SellOrderId()); hit {
			takerAcct = int64(meta.accountID)
			if instrumentID == 0 {
				instrumentID = meta.instrumentID
			}
		}
		*tickRows = append(*tickRows, TickRowValues(
			ts, symbol, tf.Price(), tf.Qty(), "UNKNOWN",
			tf.TradeId(), tf.Seq(), shard))
		*tradeRows = append(*tradeRows, TradeRowValues(
			ts, symbol, tf.TradeId(), instrumentID,
			makerAcct, takerAcct, tf.BuyOrderId(), tf.SellOrderId(),
			tf.Price(), tf.Qty(), "UNKNOWN", tf.Seq(), shard))
		bump(symbol, ts).filled++
		// The "UNKNOWN" fallback (unparseable subject) is a CH ordering
		// marker, not a symbol — never feed it to downstream projections.
		if c.opts.OnTradeFill != nil && strings.Contains(symbol, "/") {
			c.opts.OnTradeFill(symbol, tf.TradeId(), tf.Seq(),
				tf.Price(), tf.Qty(), ts)
		}
		c.m.incDecoded(1)
	case wire.EventTypeOrderNew:
		on := ipc.EventOrderNew(ev)
		if on != nil {
			c.idx.put(on.OrderId(), orderMeta{
				accountID:    on.AccountId(),
				instrumentID: on.InstrumentId(),
			})
		}
		bump(symbol, time.Unix(0, int64(ev.Ts())).UTC()).submitted++
		c.m.incOrders() // fill-rate denominator (§24 #68)
		c.m.incDecoded(1)
	case wire.EventTypeOrderAmend, wire.EventTypeOrderCancel,
		wire.EventTypeBookSnapshot:
		// Decoded but not projected into tick tables — book depth and
		// order lifecycle projections are later-phase reads.
		c.m.incDecoded(1)
	default:
		c.m.incDecoded(1)
	}
}

// msgData extracts the payload; jetstream.Msg.Data() never errors but the
// defensive check keeps decode call-sites honest if the API shifts.
func msgData(m jetstream.Msg) ([]byte, error) {
	d := m.Data()
	if len(d) == 0 {
		return nil, io.ErrUnexpectedEOF
	}
	return d, nil
}
