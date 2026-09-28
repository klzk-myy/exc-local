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

	"exchange/internal/config"
	"exchange/internal/db"
	"exchange/internal/ipc"
	"exchange/internal/marketdata"
	excnats "exchange/internal/nats"
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

	closeNATS func()
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
		ch, err := ipc.OpenChannel(base, sh, ipc.EndpointGateway, false, 0, 0)
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
	log *slog.Logger) *streamProducers {

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

	// --- BBO (delta tap) ---------------------------------------------
	var bboSrc marketdata.DeltaSource
	if bboTap != nil {
		bboSrc = chanDeltas(bboTap)
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
	if cfg.Postgres.DSN != "" &&
		envOr("EXC_MARKETDATA_OI", "1") != "0" {
		poolCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		pool, perr := db.NewPool(poolCtx, cfg.Postgres.DSN, cfg.Postgres.MaxConns)
		cancel()
		if perr != nil {
			log.Error("marketdata: postgres unavailable — openInterest idle",
				"err", perr)
		} else {
			oiSrc := marketdata.NewPgxOpenInterestSource(pool,
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
	}

	// --- Launch -------------------------------------------------------
	runners := map[string]func(context.Context) error{
		"trades": sp.Trades.Run, "aggTrades": sp.Agg.Run,
		"ticker": sp.Ticker.Run, "stats": sp.Stats.Run,
		"blockTrades": sp.Blocks.Run, "bbo": sp.BBO.Run,
		"liquidations": sp.Liqs.Run, "openInterest": sp.OI.Run,
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
