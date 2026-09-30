// Wave-2 stream-producer wiring (Tasks 6.3.3, 6.3.4, 6.3.11, 6.3.12,
// 6.3.13, 6.3.20, 6.3.23) — additive companion to main.go. All logic
// lives here; main.go only calls the two helpers.
//
// Feed topology:
//
//   - Book deltas: the _out IPC rings are SPSC — exactly one consumer
//     per process. The L2 conflator claims them via buildDeltaSource, so
//     BBO rides a TeeDeltaSource tap installed by the additive block in
//     main.go (never a second ring attach).
//   - Trade events: "auto" mode resolves to JetStream when the delta
//     source holds the rings (delta=ipc → trades=jetstream), and to the
//     IPC ring itself when the delta source is jetstream/none (the ring
//     is then unclaimed). An explicit EXC_MARKETDATA_TRADES_SOURCE=ipc
//     alongside delta=ipc is refused — two consumers on one SPSC ring
//     would corrupt both feeds.
//   - JetStream trades consume the bridge's republished stream
//     (trades.{shard}.{symbol}); the symbol comes from the SUBJECT. The
//     optional auxiliary order feed (default stream "analytics") rebuilds
//     the order→instrument admission index so taker side resolves;
//     EXC_MARKETDATA_TRADES_ORDER_FEED=none disables it (taker then
//     stays UNKNOWN — never guessed).
//   - Liquidations consume "margin-events" (Phase-19 transport shell).
//   - Open interest polls the positions table via PgxOpenInterestSource
//     (spec §5.13 is the authoritative OI source).
//
// Env knobs (additive):
//
//	EXC_MARKETDATA_TRADES_SOURCE      auto|ipc|jetstream|none (default auto)
//	EXC_MARKETDATA_TRADES_STREAM      JetStream stream (default "trades")
//	EXC_MARKETDATA_TRADES_ORDER_FEED  aux OrderNew stream (default
//	                                  "analytics"; "none" disables)
//	EXC_MARKETDATA_LIQ_STREAM         liquidation stream (default
//	                                  "margin-events"; "none" disables)
//	EXC_MARKETDATA_BLOCK_DELAY        block tape deferral (default 15m)
//	EXC_MARKETDATA_BLOCK_MIN_USD      block threshold (default 1000000)
//	EXC_MARKETDATA_OI                 "1" on / "0" off (default: on when
//	                                  Postgres DSN configured)
//	EXC_MARKETDATA_SENTIMENT          "1" on / "0" off (default on —
//	                                  sentiment@ producer, Task 23.3.6)
//	EXC_MARKETDATA_GREEKS             "1" on / "0" off (default on —
//	                                  greeks@ producer, Task 23.3.5)
//	EXC_MARKETDATA_PREMIUM            "1" on / "0" off (default on —
//	                                  premium bundle, Task 23.3.3)
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	goredis "github.com/redis/go-redis/v9"

	"exchange/internal/analytics"
	"exchange/internal/config"
	"exchange/internal/db"
	"exchange/internal/ipc"
	"exchange/internal/marketdata"
	excnats "exchange/internal/nats"
	"exchange/internal/oracle"
	"exchange/internal/oracle/rates"
	"exchange/internal/ws"
	"exchange/pkg/decimal"
)

// streamProducers owns every Wave-2 producer for shutdown/metrics.
type streamProducers struct {
	Trades *marketdata.TradesProducer
	Agg    *marketdata.AggTradesProducer
	BBO    *marketdata.BBOProducer
	Ticker *marketdata.TickerProducer
	Stats  *marketdata.StatsProducer
	Blocks *marketdata.BlockTapeProducer
	Liqs   *marketdata.LiquidationsProducer
	OI     *marketdata.OIProducer
	// Sentiment is the Phase-23 Task 23.3.6 sentiment@{symbol} producer
	// (30s cadence, 5m-delayed cohort + taker flow).
	Sentiment *marketdata.SentimentProducer
	// Greeks is the Phase-23 Task 23.3.5 greeks@{underlying} producer
	// (100ms matrix, premium-bundle entitlement enforced at subscribe).
	Greeks *marketdata.GreeksFeed
	// Premium feeds — Task 23.3.3 bundle: premium_l3@ (order-level),
	// full_depth@ (raw book deltas), auctions@ (delayed auction tape).
	PremiumL3 *marketdata.PremiumL3Producer
	FullDepth *marketdata.FullDepthProducer
	Auctions  *marketdata.AuctionsProducer

	closeNATS func()
}

// greeksInputSource resolves the per-underlying market inputs from the
// shared Redis seams — the oracle mark (oracle:mark:{sym}) and the
// Task 19.5.3.5 discount curves (curve:{ccy}). The VolFunc seam stays
// unwired: no IV-surface publisher exists yet, and the feed's contract
// freezes a contract frame (stale:true) rather than substitute a
// guessed vol. Curve misses degrade identically — MarketFromCurves
// re-validates completeness before a single greek is computed.
type greeksInputSource struct {
	marks  *oracle.Provider
	curves *rates.Store
}

func (s greeksInputSource) Snapshot(ctx context.Context,
	underlying string) (marketdata.GreeksInput, error) {
	mv, err := s.marks.Mark(ctx, underlying)
	if err != nil {
		return marketdata.GreeksInput{}, err
	}
	if !mv.Found {
		return marketdata.GreeksInput{},
			fmt.Errorf("greeks input: no mark for %s", underlying)
	}
	base, quote, _ := strings.Cut(underlying, "/")
	var bc, qc rates.Curve
	if c, cerr := s.curves.GetCurve(ctx, base); cerr == nil {
		bc = c
	}
	if c, cerr := s.curves.GetCurve(ctx, quote); cerr == nil {
		qc = c
	}
	return marketdata.GreeksInput{
		Mark:   mv.Price.InexactFloat64(),
		MarkAt: mv.ValidAt,
		Stale:  mv.Stale,
		Base:   bc,
		Quote:  qc,
	}, nil
}

// msgToByteSource drops the routing subject — WireL3Source consumes
// raw wire payloads and the subject is unused on the "l3" stream (the
// resolver, not the route token, resolves instrument symbols).
func msgToByteSource(src marketdata.MsgSource) marketdata.ByteSource {
	return func(ctx context.Context) (<-chan []byte, error) {
		in, err := src(ctx)
		if err != nil {
			return nil, err
		}
		out := make(chan []byte, 4096)
		go func() {
			defer close(out)
			for m := range in {
				out <- m.Data
			}
		}()
		return out, nil
	}
}

// chanTrades adapts a FanOut tap channel to a TradeSource.
func chanTrades(ch <-chan marketdata.TradeEvent) marketdata.TradeSource {
	return marketdata.TradeSourceFunc(
		func(context.Context) (<-chan marketdata.TradeEvent, error) {
			return ch, nil
		})
}

// chanDeltas adapts a BookDelta tap channel to a DeltaSource.
func chanDeltas(ch <-chan marketdata.BookDelta) marketdata.DeltaSource {
	return marketdata.DeltaSourceFunc(
		func(context.Context) (<-chan marketdata.BookDelta, error) {
			return ch, nil
		})
}

// connectNATS opens a dedicated producer-side NATS connection (the
// delta-source JetStream path uses its own in main.go — keep them
// independent so a consumer stall on one never wedges the other).
func connectNATS(ctx context.Context, cfg *config.Config, log *slog.Logger) (*excnats.Client, error) {
	urls := cfg.NATS.URLList()
	if len(urls) == 0 {
		return nil, errors.New("nats urls not configured")
	}
	return excnats.Connect(ctx, excnats.DefaultConfig(urls), log)
}

// buildTradeSource selects the engine trade feed per
// EXC_MARKETDATA_TRADES_SOURCE (see file header for the mode rules).
func buildTradeSource(ctx context.Context, cfg *config.Config,
	deltaMode string, res marketdata.InstrumentResolver,
	log *slog.Logger) (marketdata.TradeSource, func(), error) {

	mode := strings.ToLower(strings.TrimSpace(
		envOr("EXC_MARKETDATA_TRADES_SOURCE", "auto")))
	switch mode {
	case "none":
		return nil, nil, nil
	case "auto":
		if deltaMode == "ipc" {
			mode = "jetstream" // rings already claimed by the delta path
		} else {
			mode = "ipc" // rings unclaimed — we may attach
		}
	case "ipc", "jetstream":
	default:
		return nil, nil, fmt.Errorf("unknown trades source %q", mode)
	}

	if mode == "ipc" {
		if deltaMode == "ipc" {
			// SPSC — a second consumer corrupts both feeds. Refuse.
			return nil, nil, errors.New(
				"trades source 'ipc' conflicts with ipc delta source (SPSC ring)")
		}
		return ipcTradeSource(res, log)
	}
	return jetstreamTradeSource(ctx, cfg, res, log)
}

// ipcTradeSource attaches each shard's _out ring and decodes the full
// event stream (OrderNew echoes + TradeFill) — the high-fidelity path.
func ipcTradeSource(res marketdata.InstrumentResolver,
	log *slog.Logger) (marketdata.TradeSource, func(), error) {
	base := envOr("EXC_MARKETDATA_IPC_BASE", ipc.DefaultShmBase)
	shards, err := shardList(envOr("EXC_MARKETDATA_IPC_SHARDS", "0"))
	if err != nil {
		return nil, nil, err
	}
	var sources []marketdata.TradeSource
	var firstErr error
	for _, sh := range shards {
		// Same attach-geometry fix as ipcSource: defaults are validated but
		// overridden by the existing ring's header; 0 fails the precheck.
		ch, err := ipc.OpenChannel(base, sh, ipc.EndpointGateway, false,
			ipc.DefaultRingCapacity, ipc.DefaultRingSlotPayload)
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			log.Error("marketdata: ipc ring attach failed (trades)",
				"base", base, "shard", sh, "err", err)
			continue
		}
		sources = append(sources,
			marketdata.NewWireTradeSource(marketdata.IPCRingBytes(ch), res, log))
	}
	if len(sources) == 0 {
		return nil, nil, fmt.Errorf("no ipc rings attached: %w", firstErr)
	}
	return marketdata.FanInTradeSource(sources...), func() {}, nil
}

// jetstreamTradeSource consumes the bridge's republished trades stream
// plus the optional auxiliary order feed for taker resolution.
func jetstreamTradeSource(ctx context.Context, cfg *config.Config,
	res marketdata.InstrumentResolver,
	log *slog.Logger) (marketdata.TradeSource, func(), error) {
	nc, err := connectNATS(ctx, cfg, log)
	if err != nil {
		return nil, nil, err
	}
	closer := func() { nc.Close() }

	stream := envOr("EXC_MARKETDATA_TRADES_STREAM", "trades")
	src := marketdata.NewJetStreamTradeSource(
		marketdata.JetStreamMsgSource(nc, stream, "marketdata-trades",
			stream+".*.*", log),
		marketdata.SubjectSymbols(res), res, log)

	feed := strings.ToLower(envOr("EXC_MARKETDATA_TRADES_ORDER_FEED", "analytics"))
	if feed != "none" && feed != "" {
		src.WithOrderFeed(marketdata.JetStreamMsgSource(nc, feed,
			"marketdata-orderidx", feed+".*.*", log))
	}
	return src, closer, nil
}

// buildLiquidationSource wires the Phase-19 margin-events transport
// shell (Task 6.3.13). "none" leaves the producer Push-only.
func buildLiquidationSource(ctx context.Context, cfg *config.Config,
	res marketdata.InstrumentResolver,
	log *slog.Logger) (marketdata.LiquidationSource, func(), error) {
	stream := envOr("EXC_MARKETDATA_LIQ_STREAM", "margin-events")
	if stream == "none" || stream == "" {
		return nil, nil, nil
	}
	nc, err := connectNATS(ctx, cfg, log)
	if err != nil {
		return nil, nil, err
	}
	return marketdata.JetStreamLiquidationSource(
		marketdata.JetStreamMsgSource(nc, stream, "marketdata-liq",
			stream+".*.*", log),
		marketdata.SubjectSymbols(res), log), func() { nc.Close() }, nil
}

// instrumentIDMap inverts the wire resolver to instrument_id → symbol
// for the PG position aggregate (positions.instrument_id is BIGINT).
func instrumentIDMap(res marketdata.InstrumentResolver) map[int64]string {
	out := map[int64]string{}
	if mr, ok := res.(marketdata.MapResolver); ok {
		for id, sym := range mr {
			out[int64(id)] = sym
		}
	}
	return out
}

// oiHistoryHandler serves {"action":"request","method":"openInterest.history",
// "params":{"symbol":"EUR/USD","interval":"5m","limit":100}} — the
// in-memory derivation until Phase-23 lands the ClickHouse read model.
func oiHistoryHandler(oi *marketdata.OIProducer) marketdata.MethodHandler {
	return func(ctx context.Context, sess *ws.Session,
		params json.RawMessage) (any, error) {
		_ = sess
		var p struct {
			Symbol   string `json:"symbol"`
			Interval string `json:"interval"`
			Limit    int    `json:"limit"`
		}
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, err
		}
		secs, err := oiIntervalSeconds(p.Interval)
		if err != nil {
			return nil, err
		}
		candles, err := oi.History(p.Symbol, secs, p.Limit)
		if err != nil {
			return nil, err
		}
		return map[string]any{
			"symbol":   p.Symbol,
			"interval": p.Interval,
			"candles":  candles,
		}, nil
	}
}

// oiIntervalSeconds maps kline-style interval tokens to seconds
// (minute-granularity minimum — the history ring buckets per minute).
func oiIntervalSeconds(iv string) (int, error) {
	if iv == "" {
		return 60, nil
	}
	mult := int64(1)
	unit := iv[len(iv)-1]
	num := strings.TrimSuffix(iv, string(unit))
	n, err := strconv.ParseInt(num, 10, 32)
	if err != nil || n <= 0 {
		return 0, fmt.Errorf("invalid oi interval %q", iv)
	}
	switch unit {
	case 'm':
		mult = 60
	case 'h':
		mult = 3600
	case 'd', 'D':
		mult = 86400
	default:
		return 0, fmt.Errorf("unsupported oi interval %q", iv)
	}
	return int(n * mult), nil
}

// startStreamProducers builds and launches every Wave-2 producer.
// bboTap mirrors the engine book-delta stream (installed by the additive
// block in main.go); a nil channel leaves BBO Push-only. All producers
// fail-loud but never block startup — the WS surface stays up.
func startStreamProducers(ctx context.Context, cfg *config.Config,
	srv *marketdata.Server, bboTap <-chan marketdata.BookDelta,
	rdb *goredis.Client, log *slog.Logger) *streamProducers {

	res := instrumentResolver()
	sp := &streamProducers{}

	deltaMode := strings.ToLower(strings.TrimSpace(
		envOr("EXC_MARKETDATA_SOURCE", "ipc")))

	// --- Trade event fan-out hub -------------------------------------
	tradeSrc, closeTrades, err := buildTradeSource(ctx, cfg, deltaMode, res, log)
	if err != nil {
		log.Error("marketdata: trades source unavailable — tape producers idle",
			"err", err)
	}
	if closeTrades != nil {
		sp.closeNATS = closeTrades
	}

	var tradeHub *marketdata.FanOut[marketdata.TradeEvent]
	taps := map[string]<-chan marketdata.TradeEvent{}
	if tradeSrc != nil {
		if up, terr := tradeSrc.Trades(ctx); terr == nil {
			tradeHub = marketdata.NewFanOut(up)
			for _, name := range []string{"trades", "agg", "ticker", "stats", "blocks"} {
				taps[name] = tradeHub.Subscribe(8192)
			}
			go func() {
				if err := tradeHub.Run(ctx); err != nil &&
					!errors.Is(err, context.Canceled) {
					log.Error("marketdata: trade fanout exited", "err", err)
				}
			}()
		} else {
			log.Error("marketdata: trades source open failed", "err", terr)
		}
	}

	srcFor := func(name string) marketdata.TradeSource {
		if ch, ok := taps[name]; ok {
			return chanTrades(ch)
		}
		return nil
	}

	sp.Trades = marketdata.NewTradesProducer(
		marketdata.TradesProducerConfig{Logger: log}, srcFor("trades"), srv.Publish)
	sp.Agg = marketdata.NewAggTradesProducer(
		marketdata.AggTradesProducerConfig{Logger: log}, srcFor("agg"), srv.Publish)
	sp.Ticker = marketdata.NewTickerProducer(
		marketdata.TickerProducerConfig{Logger: log}, srcFor("ticker"), srv.Publish)
	sp.Stats = marketdata.NewStatsProducer(
		marketdata.StatsProducerConfig{Logger: log}, srcFor("stats"), srv.Publish)

	blockDelay, _ := time.ParseDuration(envOr("EXC_MARKETDATA_BLOCK_DELAY", ""))
	blockMin, _ := decimal.NewFromString(envOr("EXC_MARKETDATA_BLOCK_MIN_USD", ""))
	sp.Blocks = marketdata.NewBlockTapeProducer(marketdata.BlockTapeProducerConfig{
		Logger: log, Delay: blockDelay, ThresholdUSD: blockMin,
	}, srcFor("blocks"), srv.Publish)

	// --- BBO + full-depth (delta tap) ---------------------------------
	// The raw pre-conflation stream feeds both the BBO conflator input
	// and the Phase-23 Task 23.3.3 full_depth@ premium channel — a
	// FanOut mirror keeps them independent (same trade-hub pattern).
	var bboSrc, depthSrc marketdata.DeltaSource
	if bboTap != nil {
		deltaHub := marketdata.NewFanOut(bboTap)
		bboSrc = chanDeltas(deltaHub.Subscribe(8192))
		depthSrc = chanDeltas(deltaHub.Subscribe(8192))
		go func() {
			if err := deltaHub.Run(ctx); err != nil &&
				!errors.Is(err, context.Canceled) {
				log.Error("marketdata: delta fanout exited", "err", err)
			}
		}()
	}
	sp.BBO = marketdata.NewBBOProducer(
		marketdata.BBOProducerConfig{Logger: log}, bboSrc, srv.Publish)

	// --- Liquidations (Phase-19 transport shell) ----------------------
	liqSrc, closeLiq, lerr := buildLiquidationSource(ctx, cfg, res, log)
	if lerr != nil {
		log.Error("marketdata: liquidation source unavailable — feed idle",
			"err", lerr)
	}
	if closeLiq != nil {
		prev := sp.closeNATS
		sp.closeNATS = func() {
			closeLiq()
			if prev != nil {
				prev()
			}
		}
	}
	sp.Liqs = marketdata.NewLiquidationsProducer(
		marketdata.LiquidationsProducerConfig{Logger: log}, liqSrc, srv.Publish)

	// --- Open interest (positions aggregate) --------------------------
	sp.OI = marketdata.NewOIProducer(marketdata.OIProducerConfig{Logger: log},
		nil, srv.Publish)

	// Shared positions-table pool for the position-derived producers —
	// open interest (above) and the Phase-23 sentiment cohort (below)
	// both read §5.13 `positions`. A dial failure leaves every
	// position-derived feed in its suppressed/idle mode rather than
	// blocking startup.
	var posPool *pgxpool.Pool
	if cfg.Postgres.DSN != "" {
		poolCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		p, perr := db.NewPool(poolCtx, cfg.Postgres.DSN, cfg.Postgres.MaxConns)
		cancel()
		if perr != nil {
			log.Error("marketdata: postgres unavailable — position-derived feeds idle",
				"err", perr)
		} else {
			posPool = p
		}
	}
	if posPool != nil && envOr("EXC_MARKETDATA_OI", "1") != "0" {
		oiSrc := marketdata.NewPgxOpenInterestSource(posPool,
			instrumentIDMap(res), nil)
		syms := make([]string, 0, len(instrumentIDMap(res)))
		for _, s := range instrumentIDMap(res) {
			syms = append(syms, s)
		}
		sp.OI = marketdata.NewOIProducer(marketdata.OIProducerConfig{
			Logger: log, Symbols: syms,
		}, oiSrc, srv.Publish)
		srv.SetSnapshotSource("openInterest", sp.OI)
		if err := srv.RegisterMethod("openInterest.history",
			oiHistoryHandler(sp.OI)); err != nil {
			log.Error("marketdata: openInterest.history register failed",
				"err", err)
		}
	}

	// --- Sentiment & positioning (Phase-23 Task 23.3.6, spec §10.8) ----
	// sentiment@{symbol} pushes the 5m-delayed long/short cohort plus a
	// trailing taker-flow window every 30s (public channel — the delay
	// is structural). Cohort = same PG positions pool as OI; flow =
	// ClickHouse `trades` projection. Either source missing suppresses
	// only its frame section — the producer degrades per-source.
	sp.Sentiment = marketdata.NewSentimentProducer(
		marketdata.SentimentProducerConfig{Logger: log},
		nil, nil, srv.Publish)
	if envOr("EXC_MARKETDATA_SENTIMENT", "1") != "0" {
		var posSrc marketdata.PositionCohortSource
		if posPool != nil {
			// Cohort query is keyed on instrument_id — invert the
			// resolver's id→symbol map.
			symToID := map[string]int64{}
			for id, sym := range instrumentIDMap(res) {
				symToID[sym] = id
			}
			posSrc = marketdata.NewPgxPositionCohortSource(posPool,
				symToID, nil)
		}
		var flowSrc marketdata.TakerFlowSource
		if ch, derr := analytics.Dial(ctx, analytics.ConfigFromEnv()); derr != nil {
			log.Error("marketdata: clickhouse unavailable — sentiment taker flow idle",
				"err", derr)
		} else {
			flowSrc = marketdata.NewTakerFlowStore(ch)
			prev := sp.closeNATS
			sp.closeNATS = func() {
				_ = ch.Close()
				if prev != nil {
					prev()
				}
			}
		}
		syms := make([]string, 0, len(instrumentIDMap(res)))
		for _, s := range instrumentIDMap(res) {
			syms = append(syms, s)
		}
		sp.Sentiment = marketdata.NewSentimentProducer(
			marketdata.SentimentProducerConfig{Logger: log, Symbols: syms},
			posSrc, flowSrc, srv.Publish).WithSymbolSource(
			func() []string { return srv.ActiveSymbolsFor("sentiment") })
		srv.SetSnapshotSource("sentiment", sp.Sentiment)
	}

	// --- Greeks feed (Phase-23 Task 23.3.5, spec §24 #250) ------------
	// greeks@{underlying} emits the full option matrix every 100ms to
	// premium-bundle subscribers. Contracts read the derivative-
	// instruments table (positions pool); inputs ride the Redis oracle
	// mark + Task 19.5.3.5 curves via greeksInputSource; frames
	// republish to JetStream and snapshot to ClickHouse 008. A missing
	// vol surface freezes contract frames (stale:true) — never a guess.
	sp.Greeks = marketdata.NewGreeksFeed(marketdata.GreeksFeedConfig{
		Logger: log,
		Symbols: func() []string {
			return srv.ActiveSymbolsFor(marketdata.FeedGreeks)
		},
	}, srv.Publish)
	if envOr("EXC_MARKETDATA_GREEKS", "1") != "0" {
		gcfg := marketdata.GreeksFeedConfig{
			Logger: log,
			Symbols: func() []string {
				return srv.ActiveSymbolsFor(marketdata.FeedGreeks)
			},
		}
		if posPool != nil {
			gcfg.Contracts = marketdata.NewPgxOptionSeriesSource(posPool)
		}
		gcfg.Inputs = greeksInputSource{
			marks:  oracle.NewProvider(rdb),
			curves: rates.NewStore(rdb),
		}
		if ch, derr := analytics.Dial(ctx, analytics.ConfigFromEnv()); derr != nil {
			log.Error("marketdata: clickhouse unavailable — greeks history sink idle",
				"err", derr)
		} else {
			gcfg.Sink = marketdata.NewCHGreeksStore(ch)
			prev := sp.closeNATS
			sp.closeNATS = func() {
				_ = ch.Close()
				if prev != nil {
					prev()
				}
			}
		}
		if nc, nerr := connectNATS(ctx, cfg, log); nerr != nil {
			log.Error("marketdata: nats unavailable — greeks republish idle",
				"err", nerr)
		} else {
			gcfg.Publisher = marketdata.JetStreamGreeksPublisher(nc, "analytics", 0)
			prev := sp.closeNATS
			sp.closeNATS = func() {
				nc.Close()
				if prev != nil {
					prev()
				}
			}
		}
		sp.Greeks = marketdata.NewGreeksFeed(gcfg, srv.Publish)
	}

	// --- Premium feeds (Phase-23 Task 23.3.3) --------------------------
	// premium_l3@{sym} relays engine L3 rows off the dedicated "l3"
	// JetStream stream (bridge route.go owns the routing); full_depth@
	// rides the raw delta mirror opened above; auctions@ re-emits
	// delayed liquidation-auction events off a second margin-events
	// consumer. Entitlement enforcement lives on the server bind —
	// producers publish frames, never policy.
	sp.PremiumL3 = marketdata.NewPremiumL3Producer(nil, srv.Publish, log)
	sp.FullDepth = marketdata.NewFullDepthProducer(depthSrc, srv.Publish, nil, log)
	sp.Auctions = marketdata.NewAuctionsProducer(
		marketdata.LiquidationsProducerConfig{Logger: log}, nil, srv.Publish)
	if envOr("EXC_MARKETDATA_PREMIUM", "1") != "0" {
		if nc, nerr := connectNATS(ctx, cfg, log); nerr != nil {
			log.Error("marketdata: nats unavailable — premium_l3 feed idle",
				"err", nerr)
		} else {
			l3src := marketdata.NewWireL3Source(
				msgToByteSource(marketdata.JetStreamMsgSource(nc, "l3",
					"marketdata-premium-l3", "l3.*.*", log)),
				res, log)
			sp.PremiumL3 = marketdata.NewPremiumL3Producer(
				l3src, srv.Publish, log)
			prev := sp.closeNATS
			sp.closeNATS = func() {
				nc.Close()
				if prev != nil {
					prev()
				}
			}
		}
		sp.FullDepth = marketdata.NewFullDepthProducer(
			depthSrc, srv.Publish, nil, log)
		aucSrc, closeAuc, aerr := buildLiquidationSource(ctx, cfg, res, log)
		if aerr != nil {
			log.Error("marketdata: auctions source unavailable — feed idle",
				"err", aerr)
		} else {
			prev := sp.closeNATS
			sp.closeNATS = func() {
				closeAuc()
				if prev != nil {
					prev()
				}
			}
		}
		sp.Auctions = marketdata.NewAuctionsProducer(
			marketdata.LiquidationsProducerConfig{Logger: log},
			aucSrc, srv.Publish)
	}

	// --- Launch -------------------------------------------------------
	runners := map[string]func(context.Context) error{
		"trades": sp.Trades.Run, "aggTrades": sp.Agg.Run,
		"ticker": sp.Ticker.Run, "stats": sp.Stats.Run,
		"blockTrades": sp.Blocks.Run, "bbo": sp.BBO.Run,
		"liquidations": sp.Liqs.Run, "openInterest": sp.OI.Run,
		"sentiment": sp.Sentiment.Run, "greeks": sp.Greeks.Run,
		"premiumL3": sp.PremiumL3.Run, "fullDepth": sp.FullDepth.Run,
		"auctions": sp.Auctions.Run,
	}
	for name, run := range runners {
		go func(name string, run func(context.Context) error) {
			if err := run(ctx); err != nil && !errors.Is(err, context.Canceled) {
				log.Error("marketdata: producer exited", "producer", name, "err", err)
			}
		}(name, run)
	}
	log.Info("marketdata: wave-2 stream producers started")
	return sp
}
