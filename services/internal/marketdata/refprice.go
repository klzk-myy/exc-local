// Task 6.3.17 — referencePrice@{symbol} stream (spec §10.5 channel
// registry, §24 #265 execution-rule distribution, Phase-19.5 Price
// Oracle seam).
//
// The instrument-level execution contract (limit collars and expiry
// bounds) is defined against a reference price owned by the Phase-19.5
// Price Oracle — fed by Refinitiv, Bloomberg BFIX and ECB reference
// rates, minimum two independent sources, with a 5-second staleness
// gate. This package does NOT compute references: ReferencePriceSource
// is the read-only seam the oracle publishes into; the stream below
// normalizes it onto referencePrice@{symbol} with sequence continuity
// (md:seq:referencePrice:{symbol}) and the spec's fail-closed rule —
// past the gate the frame carries no usable price/collar fields at
// all, only stale metadata, so no client can act on dead data
// (spec §2.7 fail-closed, CONDITIONAL_TRIGGER_ORACLE_STALE lineage).
package marketdata

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"exchange/pkg/decimal"
)

// OracleRef is one normalized reference observation handed up by the
// Phase-19.5 Price Oracle seam. Price uses the platform's 1e-8 wire
// scale (same as Level.Price). Source is the provenance enum the wire
// may expose — "refinitiv" | "bloomberg_bfix" | "ecb" | "composite" —
// proprietary feed internals stay upstream of this seam.
type OracleRef struct {
	Symbol  string    // display symbol ("EUR/USD")
	Price   int64     // 1e-8 scaled reference price
	Source  string    // provenance enum — never vendor internals
	ValidAt time.Time // oracle observation timestamp (staleness anchor)
}

// ReferencePriceSource is the Phase-19.5 oracle seam. Implementations
// answer the current reference for one symbol; errors mean the oracle
// itself is unreachable (distinct from a stale observation — both fail
// closed downstream).
type ReferencePriceSource interface {
	Reference(ctx context.Context, symbol string) (OracleRef, error)
}

// ReferencePriceFunc adapts a function to ReferencePriceSource.
type ReferencePriceFunc func(ctx context.Context, symbol string) (OracleRef, error)

// Reference implements ReferencePriceSource.
func (f ReferencePriceFunc) Reference(ctx context.Context, symbol string) (OracleRef, error) {
	return f(ctx, symbol)
}

// ExecutionRule is the instrument's collar set around the reference:
// orders on the buy side may not exceed ref×(1+BuyLimitBps/10⁴), sells
// may not go below ref×(1−SellLimitBps/10⁴); ExpiryMs bounds order
// lifetime under the rule (0 = no bound). The oracle/instrument service
// owns the values; the stream surfaces them verbatim.
type ExecutionRule struct {
	BuyLimitBps  int64 `json:"buy_limit_bps"`  // collar above ref for buys
	SellLimitBps int64 `json:"sell_limit_bps"` // collar below ref for sells
	ExpiryMs     int64 `json:"expiry_ms"`      // max order lifetime under rule (0 = none)
}

// ExecutionRuleSource supplies per-instrument collars. Nil in the
// stream ⇒ frames omit collar fields (clients then apply their own
// session defaults — the reference price itself is still distributed).
type ExecutionRuleSource interface {
	Rules(ctx context.Context, symbol string) (ExecutionRule, error)
}

// ExecutionRuleFunc adapts a function to ExecutionRuleSource.
type ExecutionRuleFunc func(ctx context.Context, symbol string) (ExecutionRule, error)

// Rules implements ExecutionRuleSource.
func (f ExecutionRuleFunc) Rules(ctx context.Context, symbol string) (ExecutionRule, error) {
	return f(ctx, symbol)
}

// refPriceUpdate is the referencePrice@{symbol} wire payload. On a
// FRESH observation it carries price + derived collars; on a STALE or
// unavailable observation it carries ONLY provenance/staleness
// metadata — price/buy_limit/sell_limit are omitted so a client can
// never act on dead data (fail-closed per §2.7 / Task 6.3.17).
type refPriceUpdate struct {
	Event        string `json:"event"` // "referencePrice"
	Symbol       string `json:"symbol"`
	Price        string `json:"price,omitempty"`
	BuyLimit     string `json:"buy_limit,omitempty"`  // ref × (1+bps/10⁴)
	SellLimit    string `json:"sell_limit,omitempty"` // ref × (1−bps/10⁴)
	RuleExpiryMs int64  `json:"rule_expiry_ms,omitempty"`
	Source       string `json:"source"` // provenance enum
	ValidAtMs    int64  `json:"valid_at_ms"`
	ExpiresAtMs  int64  `json:"expires_at_ms"` // ValidAt + staleness gate
	Stale        bool   `json:"stale"`
	StaleMs      int64  `json:"stale_ms,omitempty"`
	Reason       string `json:"reason,omitempty"` // "reference_stale" | "oracle_unavailable"
	Seq          uint64 `json:"seq"`
	TsMs         int64  `json:"ts_ms"`
}

// RefPriceConfig tunes RefPriceStream.
type RefPriceConfig struct {
	// PollInterval is the observation cadence — default 500ms (well
	// inside the 5s staleness gate so transitions surface promptly).
	PollInterval time.Duration
	// StalenessGate bounds observation age — default 5s (spec: oracle
	// staleness gate; CONDITIONAL_TRIGGER_ORACLE_STALE lineage).
	StalenessGate time.Duration
	Logger        *slog.Logger
	Now           func() time.Time
}

func (c *RefPriceConfig) defaults() {
	if c.PollInterval <= 0 {
		c.PollInterval = 500 * time.Millisecond
	}
	if c.StalenessGate <= 0 {
		c.StalenessGate = 5 * time.Second
	}
	if c.Logger == nil {
		c.Logger = slog.Default()
	}
	if c.Now == nil {
		c.Now = time.Now
	}
}

// RefPriceStream multiplexes oracle observations onto
// referencePrice@{symbol} channels. It emits only while subscribers
// exist for the symbol (the symbols seam reads the hub's live
// subscription index — no subscription, no oracle polling).
type RefPriceStream struct {
	cfg     RefPriceConfig
	src     ReferencePriceSource
	rules   ExecutionRuleSource
	symbols func() []string
	emit    EmitFunc
	seq     SeqStore
	journal GapJournal
	m       *Metrics

	mu      sync.Mutex
	cursors map[string]uint64
	seeded  map[string]bool
	latest  map[string]refPriceUpdate // per-symbol last frame (snapshot backing)
}

// NewRefPriceStream wires the stream. symbols/emit come from the hub
// (Server.ActiveSymbolsFor + Server.Publish); seq/journal/metrics share
// the service's durable seams. src nil renders the stream inert — Run
// returns immediately (fail-loud wiring: no oracle, no channel data).
func NewRefPriceStream(cfg RefPriceConfig, src ReferencePriceSource,
	rules ExecutionRuleSource, symbols func() []string, emit EmitFunc,
	seq SeqStore, journal GapJournal, m *Metrics) *RefPriceStream {
	cfg.defaults()
	if m == nil {
		m = NewMetrics()
	}
	return &RefPriceStream{
		cfg: cfg, src: src, rules: rules, symbols: symbols, emit: emit,
		seq: seq, journal: journal, m: m,
		cursors: map[string]uint64{}, seeded: map[string]bool{},
		latest: map[string]refPriceUpdate{},
	}
}

// Run polls subscribed symbols until ctx cancels.
func (s *RefPriceStream) Run(ctx context.Context) error {
	if s.src == nil || s.symbols == nil || s.emit == nil {
		return fmt.Errorf("marketdata: refprice stream unconfigured")
	}
	tick := time.NewTicker(s.cfg.PollInterval)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-tick.C:
			s.pollAll(ctx)
		}
	}
}

// pollAll refreshes every symbol carrying a live referencePrice@
// subscription.
func (s *RefPriceStream) pollAll(ctx context.Context) {
	for _, sym := range s.symbols() {
		if ctx.Err() != nil {
			return
		}
		s.pollOne(ctx, sym)
	}
}

// pollOne fetches and emits one symbol's reference frame. The
// staleness gate is evaluated on the ORACLE's ValidAt, not arrival
// time — a delayed-but-timestamped observation is still stale.
func (s *RefPriceStream) pollOne(ctx context.Context, symbol string) {
	now := s.cfg.Now()
	fetchCtx, cancel := context.WithTimeout(ctx, s.cfg.PollInterval)
	defer cancel()

	ref, err := s.src.Reference(fetchCtx, symbol)
	upd := refPriceUpdate{
		Event: "referencePrice", Symbol: symbol,
		Stale: true, TsMs: now.UnixMilli(),
	}
	if err != nil {
		upd.Reason = "oracle_unavailable"
		s.cfg.Logger.Warn("marketdata: reference fetch failed — emitting stale marker",
			"symbol", symbol, "err", err)
		s.emitRef(symbol, upd)
		return
	}
	if ref.Symbol != "" && ref.Symbol != symbol {
		s.cfg.Logger.Error("marketdata: oracle returned mismatched symbol — dropped",
			"want", symbol, "got", ref.Symbol)
		return
	}
	upd.Source = ref.Source
	upd.ValidAtMs = ref.ValidAt.UnixMilli()
	upd.ExpiresAtMs = ref.ValidAt.Add(s.cfg.StalenessGate).UnixMilli()

	age := now.Sub(ref.ValidAt)
	if age > s.cfg.StalenessGate || ref.Price <= 0 {
		// Fail closed: stale or unusable observations emit provenance +
		// staleness metadata only — no price, no collars (spec: "missing
		// or stale required references fail closed").
		upd.Reason = "reference_stale"
		if ref.Price <= 0 {
			upd.Reason = "reference_invalid"
		}
		if age > 0 {
			upd.StaleMs = age.Milliseconds()
		}
		s.m.RefPriceStaleEmits.Add(1)
		s.emitRef(symbol, upd)
		return
	}

	upd.Stale = false
	price := decimal.NewFromScaled(ref.Price)
	upd.Price = price.String()
	if s.rules != nil {
		if rule, rerr := s.rules.Rules(fetchCtx, symbol); rerr == nil {
			// Collars: buy limit ref×(1+bps/1e4), sell limit
			// ref×(1−bps/1e4) — decimal math, no float slop.
			scale := decimal.NewFromInt(10_000)
			if rule.BuyLimitBps > 0 {
				f := decimal.NewFromInt(10_000 + rule.BuyLimitBps).Div(scale)
				upd.BuyLimit = price.Mul(f).String()
			}
			if rule.SellLimitBps > 0 {
				f := decimal.NewFromInt(10_000 - rule.SellLimitBps).Div(scale)
				upd.SellLimit = price.Mul(f).String()
			}
			upd.RuleExpiryMs = rule.ExpiryMs
		} else {
			s.cfg.Logger.Warn("marketdata: execution rules unavailable — emitting price only",
				"symbol", symbol, "err", rerr)
		}
	}
	s.emitRef(symbol, upd)
}

// emitRef sequences and fans out one frame, refreshing the snapshot
// cache. Seq domain: "referencePrice:{symbol}" → md:seq mirror keeps
// restart continuity (Task 6.3.22).
func (s *RefPriceStream) emitRef(symbol string, upd refPriceUpdate) {
	seq := s.allocSeq(symbol)
	upd.Seq = seq
	s.mu.Lock()
	s.latest[symbol] = upd
	s.mu.Unlock()
	s.emit("referencePrice@"+symbol, seq, upd)
}

// allocSeq bumps the per-symbol reference cursor, seeding from the
// durable SeqStore on first touch (restart continues the sequence, the
// gap journal marks the boundary).
func (s *RefPriceStream) allocSeq(symbol string) uint64 {
	key := "referencePrice:" + symbol
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.seeded[key] {
		s.seeded[key] = true
		if s.seq != nil {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			v, err := s.seq.Load(ctx, key)
			cancel()
			if err != nil {
				s.cfg.Logger.Error("marketdata: refprice seq seed failed",
					"key", key, "err", err)
				noteGap(s.journal, s.cfg.Logger, key, SeqGap{
					From: 0, To: 0, Reason: GapSeedFailed,
				})
			} else {
				s.cursors[key] = v
				if v > 0 {
					noteGap(s.journal, s.cfg.Logger, key, SeqGap{
						From: v, To: v, Reason: GapRestartBoundary,
					})
				}
			}
		}
	}
	s.cursors[key]++
	cur := s.cursors[key]
	if s.seq != nil {
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			if err := s.seq.Store(ctx, key, cur); err != nil {
				s.cfg.Logger.Warn("marketdata: refprice seq mirror failed",
					"key", key, "seq", cur, "err", err)
			}
		}()
	}
	return cur
}

// Snapshot implements SnapshotSource for "referencePrice@{symbol}": the
// last emitted frame (fresh or stale — a stale snapshot correctly tells
// a resynced client the reference is not currently usable).
func (s *RefPriceStream) Snapshot(_ context.Context, channel string) (uint64, any, error) {
	ch, err := ParseChannel(channel)
	if err != nil || ch.Type != "referencePrice" {
		return 0, nil, fmt.Errorf("marketdata: no referencePrice snapshot for %q", channel)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	upd, ok := s.latest[ch.Target]
	if !ok {
		return 0, nil, fmt.Errorf("marketdata: no reference state for %q", ch.Target)
	}
	return s.cursors["referencePrice:"+ch.Target], upd, nil
}
