// Phase-19 margin-engine shared contracts — mark price provider, Redis
// mark cache (batch MGET), mark delta source, and the margin-level hash
// reader siblings consume.
//
// THE CONTRACT (spec §13.1/§13.15, Phase-19.5 Task 19.5.3.1):
//
//	MarkPriceProvider is the seam every Phase-19 consumer calls. The
//	Phase-19 landing binds StubMarkPriceProvider (last-trade price from
//	the matching engine — the same placeholder settlement.PositionService
//	and risk.PnlService use). Phase-19.5 replaces it with
//	OracleMarkPriceProvider (Refinitiv / Bloomberg BFIX / ECB, ≥2
//	independent sources, 5s staleness gate) WITHOUT changing these
//	signatures — the interface is the contract.
//
// Consumers needing batch loading (margin evaluation, spec §13.1 "no
// N+1") use MarkCache.BatchMarks — a single MGET across mark:{symbol}
// keys — rather than calling GetMarkPrice per symbol.
package risk

import (
	"context"
	"encoding/json"
	stderrors "errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	goredis "github.com/redis/go-redis/v9"

	"exchange/pkg/decimal"
	excerrors "exchange/pkg/errors"
)

// ErrMarkNotFound is returned when a symbol has no usable mark —
// neither a live oracle value nor a last trade. Callers decide the
// fallback (stored mark → entry price); it is a data condition, not a
// service failure.
var ErrMarkNotFound = stderrors.New("mark price not found")

// MarkPriceSource enumerates mark provenance for provenance-stamped
// consumers (contract field on MarkPrice).
const (
	MarkSourceLastTrade  = "last_trade"  // stub — engine last traded price
	MarkSourceOracle     = "oracle"      // Phase-19.5 composite oracle
	MarkSourceStoredMark = "stored_mark" // positions.mark_price fallback
	MarkSourceEntry      = "entry"       // cost-basis last resort
)

// MarkPrice is a mark observation plus its provenance — the payload
// GetMarkPriceWithProvenance returns.
type MarkPrice struct {
	Symbol  string
	Price   decimal.Decimal
	Source  string    // MarkSource* enum
	ValidAt time.Time // observation time (staleness anchor)
	Stale   bool      // past the consumer's staleness gate when true
}

// MarkPriceProvider is the Phase-19 mark-price contract
// (docs/Phase-19-Multi-Asset-Margin.md Task 19.3.3 note, added
// 2026-09-27 functional cluster review F8). Signatures are frozen —
// Phase-19.5 implements them without touching Phase-19 callers.
type MarkPriceProvider interface {
	// GetMarkPrice returns the current mark for symbol (display form,
	// e.g. "EUR/USD"). ErrMarkNotFound means no mark exists at all —
	// NOT an outage.
	GetMarkPrice(symbol string) (decimal.Decimal, error)
	// GetMarkPriceWithProvenance returns the mark plus source/staleness
	// metadata for audit and staleness-gated consumers.
	GetMarkPriceWithProvenance(symbol string) (MarkPrice, error)
}

// ---------------------------------------------------------------------------
// StubMarkPriceProvider — last-trade placeholder until Phase-19.5
// ---------------------------------------------------------------------------

// StubMarkPriceProvider is the Phase-19 binding of MarkPriceProvider:
// the instrument's last traded price, fed by Observe (engine fill/tick
// fan-in) and backed by an optional synchronous fallback (e.g.
// PgLastTradeFallback for cold-start / missed ticks).
//
// This is the same placeholder semantics as settlement's
// LastTradeMarkPriceProvider, keyed by SYMBOL (not instrument_id) so the
// Redis mark:{symbol} contract and the provider agree on one key shape.
type StubMarkPriceProvider struct {
	mu       sync.RWMutex
	last     map[string]decimal.Decimal
	seenAt   map[string]time.Time
	now      func() time.Time
	fallback func(symbol string) (decimal.Decimal, bool) // optional; no ctx — contract is synchronous
}

// NewStubMarkPriceProvider returns an empty last-trade cache. Feed it
// via Observe (the margin engine calls Observe for every mark tick it
// consumes; the fill pipeline can call it on every trade).
func NewStubMarkPriceProvider() *StubMarkPriceProvider {
	return &StubMarkPriceProvider{
		last:   map[string]decimal.Decimal{},
		seenAt: map[string]time.Time{},
		now:    time.Now,
	}
}

// WithFallback binds a synchronous cold lookup (e.g. PgLastTradeFallback)
// consulted when the in-memory cache has no entry. The result is cached
// so a second call does not re-hit the store.
func (p *StubMarkPriceProvider) WithFallback(fn func(symbol string) (decimal.Decimal, bool)) *StubMarkPriceProvider {
	p.fallback = fn
	return p
}

// WithClock injects the clock (tests).
func (p *StubMarkPriceProvider) WithClock(now func() time.Time) *StubMarkPriceProvider {
	p.now = now
	return p
}

// Observe records the latest traded price for symbol. Non-positive prices
// are ignored — a corrupt feed can never plant a mark.
func (p *StubMarkPriceProvider) Observe(symbol string, price decimal.Decimal, at time.Time) {
	if !price.IsPositive() {
		return
	}
	p.mu.Lock()
	p.last[symbol] = price
	p.seenAt[symbol] = at
	p.mu.Unlock()
}

// GetMarkPrice implements MarkPriceProvider.
func (p *StubMarkPriceProvider) GetMarkPrice(symbol string) (decimal.Decimal, error) {
	mp, err := p.GetMarkPriceWithProvenance(symbol)
	if err != nil {
		return decimal.Zero, err
	}
	return mp.Price, nil
}

// GetMarkPriceWithProvenance implements MarkPriceProvider.
func (p *StubMarkPriceProvider) GetMarkPriceWithProvenance(symbol string) (MarkPrice, error) {
	p.mu.RLock()
	px, ok := p.last[symbol]
	at := p.seenAt[symbol]
	p.mu.RUnlock()
	if !ok && p.fallback != nil {
		if px2, found := p.fallback(symbol); found && px2.IsPositive() {
			p.Observe(symbol, px2, p.now())
			px, at, ok = px2, p.now(), true
		}
	}
	if !ok || !px.IsPositive() {
		return MarkPrice{}, ErrMarkNotFound
	}
	return MarkPrice{Symbol: symbol, Price: px, Source: MarkSourceLastTrade,
		ValidAt: at}, nil
}

// ---------------------------------------------------------------------------
// ChainedMarkPriceProvider — oracle-primary / last-trade fallback
// ---------------------------------------------------------------------------

// ChainedMarkPriceProvider is the Phase-19.5 wiring: the oracle adapter
// is primary; when it has no mark for a symbol (cold start, symbol
// outside the oracle universe) the last-trade stub serves. Errors and
// ErrMarkNotFound on the primary fall through — a degraded oracle must
// not blind the margin path (provenance still reports the real source).
//
// Observe delegates to the fallback stub — the fill hook keeps feeding
// the last-trade book regardless of which provider answered.
type ChainedMarkPriceProvider struct {
	Primary  MarkPriceProvider
	Fallback *StubMarkPriceProvider
}

// NewChainedMarkPriceProvider wires primary-over-fallback.
func NewChainedMarkPriceProvider(primary MarkPriceProvider,
	fallback *StubMarkPriceProvider) *ChainedMarkPriceProvider {
	return &ChainedMarkPriceProvider{Primary: primary, Fallback: fallback}
}

// GetMarkPrice implements MarkPriceProvider.
func (c *ChainedMarkPriceProvider) GetMarkPrice(symbol string) (decimal.Decimal, error) {
	mp, err := c.GetMarkPriceWithProvenance(symbol)
	if err != nil {
		return decimal.Zero, err
	}
	return mp.Price, nil
}

// GetMarkPriceWithProvenance implements MarkPriceProvider — primary
// first, stub fallback. A stale primary mark is returned AS stale
// (Stale=true carries the signal; callers apply their own gate).
func (c *ChainedMarkPriceProvider) GetMarkPriceWithProvenance(symbol string) (MarkPrice, error) {
	if c.Primary != nil {
		if mp, err := c.Primary.GetMarkPriceWithProvenance(symbol); err == nil {
			return mp, nil
		}
	}
	if c.Fallback == nil {
		return MarkPrice{}, ErrMarkNotFound
	}
	return c.Fallback.GetMarkPriceWithProvenance(symbol)
}

// Observe feeds the last-trade stub — the fill hook's writer path is
// unchanged by the oracle binding (MarkObserver seam compatibility).
func (c *ChainedMarkPriceProvider) Observe(symbol string, price decimal.Decimal, at time.Time) {
	if c.Fallback != nil {
		c.Fallback.Observe(symbol, price, at)
	}
}

// PgLastTradeFallback builds the synchronous PostgreSQL last-trade lookup
// used as the stub's cold path: latest trades.price for the symbol.
// Absent instrument or zero trades → ok=false. Errors collapse to
// ok=false (the caller treats it as no-mark); the margin evaluator keeps
// its own fail-closed discipline on top.
func PgLastTradeFallback(pool *pgxpool.Pool) func(symbol string) (decimal.Decimal, bool) {
	return func(symbol string) (decimal.Decimal, bool) {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		var px *string
		err := pool.QueryRow(ctx, `
			SELECT t.price::text FROM trades t
			JOIN instruments i ON i.id = t.instrument_id
			WHERE i.symbol = $1 ORDER BY t.id DESC LIMIT 1`, symbol).Scan(&px)
		if err != nil || px == nil {
			return decimal.Zero, false
		}
		d, err := decimal.NewFromString(*px)
		if err != nil {
			return decimal.Zero, false
		}
		return d, true
	}
}

// ---------------------------------------------------------------------------
// RedisMarkCache — single-MGET batch mark loading (spec §13.1 no-N+1)
// ---------------------------------------------------------------------------

// MarkCache is the batch read seam: one MGET across mark:{symbol} keys.
// The margin evaluator MUST use it for every valuation pass — per-symbol
// GETs are an N+1 defect under spec §13.1.
type MarkCache interface {
	// BatchMarks returns symbol → mark for every mark:{symbol} key that
	// exists; absent symbols are simply missing from the map.
	BatchMarks(ctx context.Context, symbols []string) (map[string]decimal.Decimal, error)
}

// RedisMarkCache implements MarkCache over the coordination Redis.
// It is also the dev/test write path: PublishMarkDelta SETs the key and
// PUBLISHes the delta on the per-symbol channel in one MULTI, exactly
// what the Phase-19.5 oracle writer does.
type RedisMarkCache struct {
	C *goredis.Client
}

// NewRedisMarkCache wraps a go-redis client (excredis.Client embeds one —
// pass rdb.Client).
func NewRedisMarkCache(c *goredis.Client) *RedisMarkCache { return &RedisMarkCache{C: c} }

// BatchMarks implements MarkCache — ONE MGet round-trip for all symbols.
func (m *RedisMarkCache) BatchMarks(ctx context.Context, symbols []string) (map[string]decimal.Decimal, error) {
	out := make(map[string]decimal.Decimal, len(symbols))
	if len(symbols) == 0 {
		return out, nil
	}
	keys := make([]string, 0, len(symbols))
	seen := map[string]bool{}
	for _, s := range symbols {
		if s == "" || seen[s] {
			continue
		}
		seen[s] = true
		keys = append(keys, MarkPriceKey(s))
	}
	vals, err := m.C.MGet(ctx, keys...).Result()
	if err != nil {
		return nil, fmt.Errorf("redis mark mget: %w", err)
	}
	for i, v := range vals {
		if v == nil {
			continue
		}
		s, isStr := v.(string)
		if !isStr {
			return nil, fmt.Errorf("redis mark mget: %s non-string payload %T", keys[i], v)
		}
		d, err := decimal.NewFromString(s)
		if err != nil || !d.IsPositive() {
			return nil, fmt.Errorf("redis mark mget: %s malformed value %q", keys[i], s)
		}
		// map key order == symbol order == requested order (deduped)
		out[strings.TrimPrefix(keys[i], "mark:")] = d
	}
	return out, nil
}

// markDeltaMsg is the pub/sub payload on channel mark:{symbol}.
type markDeltaMsg struct {
	Symbol string `json:"symbol"`
	Price  string `json:"price"`
	TsMs   int64  `json:"ts_ms"`
	Source string `json:"source,omitempty"`
}

// PublishMarkDelta writes mark:{symbol} and publishes the delta on the
// per-symbol channel atomically (MULTI) — the writer contract the
// Phase-19.5 oracle implements; used today by dev feeders and tests.
func (m *RedisMarkCache) PublishMarkDelta(ctx context.Context, symbol string,
	price decimal.Decimal, at time.Time) error {
	payload, err := json.Marshal(markDeltaMsg{
		Symbol: symbol, Price: price.String(), TsMs: at.UnixMilli(),
		Source: MarkSourceLastTrade,
	})
	if err != nil {
		return err
	}
	pipe := m.C.TxPipeline()
	pipe.Set(ctx, MarkPriceKey(symbol), price.String(), 0)
	pipe.Publish(ctx, MarkPriceKey(symbol), payload)
	_, err = pipe.Exec(ctx)
	return err
}

// ---------------------------------------------------------------------------
// RedisMarkSource — pub/sub delta feed (same seam shape as fix/mdata.go's
// DeltaSource: a channel-yielding source the engine consumes, with a
// direct PushMark hook as the test/inline path)
// ---------------------------------------------------------------------------

// MarkTick is one mark-price delta consumed by the margin engine.
type MarkTick struct {
	Symbol string
	Price  decimal.Decimal
	Source string // MarkSource* enum
	Ts     time.Time
}

// MarkDeltaSource yields mark ticks until ctx ends (closed channel on
// termination) — same adapter contract as marketdata.DeltaSource.
type MarkDeltaSource interface {
	Marks(ctx context.Context) (<-chan MarkTick, error)
}

// MarkSourceFunc adapts a function to MarkDeltaSource.
type MarkSourceFunc func(ctx context.Context) (<-chan MarkTick, error)

// Marks implements MarkDeltaSource.
func (f MarkSourceFunc) Marks(ctx context.Context) (<-chan MarkTick, error) { return f(ctx) }

// RedisMarkSource PSubscribes MarkChannelPattern (mark:*) and decodes
// markDeltaMsg payloads. Malformed frames are dropped and counted via
// OnDrop — a corrupt tick must never kill the margin feed (same
// discipline as WireDeltaSource).
type RedisMarkSource struct {
	C   *goredis.Client
	Log *slog.Logger
	// OnDrop observes skipped payloads ("malformed"); nil is fine.
	OnDrop func(reason string)
}

// NewRedisMarkSource wraps a go-redis client.
func NewRedisMarkSource(c *goredis.Client, log *slog.Logger) *RedisMarkSource {
	if log == nil {
		log = slog.Default()
	}
	return &RedisMarkSource{C: c, Log: log}
}

// Marks implements MarkDeltaSource.
func (s *RedisMarkSource) Marks(ctx context.Context) (<-chan MarkTick, error) {
	sub := s.C.PSubscribe(ctx, MarkChannelPattern)
	if _, err := sub.Receive(ctx); err != nil {
		_ = sub.Close()
		return nil, fmt.Errorf("redis mark psubscribe: %w", err)
	}
	out := make(chan MarkTick, 1024)
	go func() {
		defer close(out)
		defer func() { _ = sub.Close() }()
		for {
			select {
			case <-ctx.Done():
				return
			case msg, ok := <-sub.Channel():
				if !ok {
					return
				}
				var m markDeltaMsg
				if err := json.Unmarshal([]byte(msg.Payload), &m); err != nil {
					s.Log.Warn("risk: malformed mark delta", "channel", msg.Channel, "err", err)
					if s.OnDrop != nil {
						s.OnDrop("malformed")
					}
					continue
				}
				sym := m.Symbol
				if sym == "" {
					sym = strings.TrimPrefix(msg.Channel, "mark:")
				}
				px, err := decimal.NewFromString(m.Price)
				if err != nil || !px.IsPositive() || sym == "" {
					s.Log.Warn("risk: malformed mark delta payload",
						"channel", msg.Channel, "price", m.Price)
					if s.OnDrop != nil {
						s.OnDrop("malformed")
					}
					continue
				}
				tick := MarkTick{Symbol: sym, Price: px,
					Source: m.Source, Ts: time.UnixMilli(m.TsMs)}
				if tick.Source == "" {
					tick.Source = MarkSourceOracle
				}
				select {
				case out <- tick:
				case <-ctx.Done():
					return
				}
			}
		}
	}()
	return out, nil
}

// NOTE: the canonical margin:level:{account_id} read contract
// (MarginLevel / MarginLevelReader / RedisMarginLevelReader) lives in
// margin_level_reader.go — the earlier inline draft was superseded by
// the interface form the liquidation cluster consumes.

// ADVKey is the Redis mirror for an instrument's average daily traded
// notional (quote ccy) — the §13.12 liquidity add-on and §13.4a/6a
// liquidation-slicing yardstick. The analytics rollup owns the writer;
// an absent key reads as ADV 0 (the §2.7 pessimistic legs apply).
func ADVKey(instrumentID int64) string {
	return fmt.Sprintf("instrument:adv:%d", instrumentID)
}

// RedisADVSource implements ADVSource over the coordination Redis.
// Missing keys and malformed/non-positive payloads read as ADV 0 —
// never an error that stalls an evaluation (the LiquidityAddon cap
// already carries the pessimism).
type RedisADVSource struct {
	C *goredis.Client
}

// NewRedisADVSource wraps a go-redis client (excredis.Client embeds
// one — pass rdb.Client).
func NewRedisADVSource(c *goredis.Client) *RedisADVSource {
	return &RedisADVSource{C: c}
}

// ADV implements ADVSource.
func (a *RedisADVSource) ADV(ctx context.Context, instrumentID int64) (decimal.Decimal, error) {
	v, err := a.C.Get(ctx, ADVKey(instrumentID)).Result()
	if err == goredis.Nil {
		return decimal.Zero, nil
	}
	if err != nil {
		return decimal.Zero, fmt.Errorf("redis adv %d: %w", instrumentID, err)
	}
	d, err := decimal.NewFromString(v)
	if err != nil || !d.IsPositive() {
		return decimal.Zero, nil
	}
	return d, nil
}

// compile-time seam assertions
var (
	_ MarkPriceProvider = (*StubMarkPriceProvider)(nil)
	_ MarkCache         = (*RedisMarkCache)(nil)
	_ MarkDeltaSource   = (*RedisMarkSource)(nil)
	_ MarkDeltaSource   = MarkSourceFunc(nil)
	_ ADVSource         = (*RedisADVSource)(nil)
)

// errCode re-wraps an error with a registered §23 code — internal helper
// shared by the margin files.
func errCode(code, op string, err error) error {
	return excerrors.Wrap(code, op, err)
}
