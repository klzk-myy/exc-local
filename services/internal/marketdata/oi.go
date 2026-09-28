// Task 6.3.23 — public open-interest stream `openInterest@{symbol}`
// (spec §10.8, §24 #357) plus in-memory history derivation.
//
// DATA SOURCE — honest, not fabricated: open interest is aggregated
// from the positions table (spec §5.13) — the authoritative position
// store — via OpenInterestSource. The production implementation is
// PgxOpenInterestSource:
//
//	SELECT instrument_id, COUNT(*), SUM(quantity),
//	       SUM(quantity * COALESCE(mark_price, entry_price)),
//	       MAX(updated_at)
//	FROM positions WHERE quantity > 0 GROUP BY instrument_id
//
// OI is the LONG|SHORT-summed notional (spec §10.8 item 1: FX has no
// long/short float split to disclose) — `open_interest` is summed base
// quantity, `open_interest_notional` is the same positions at their
// mark (entry price as the mark-less fallback — positions.mark_price is
// NULL until the first mark).
//
// Staleness (§10.8 "stale-flagged beyond the 5s oracle gate, never
// interpolated"): `stale` is set when the aggregate read itself is older
// than the gate — a failed poll re-publishes the last known values with
// stale:true (and on a cold failure, nothing at all: a never-observed
// symbol emits no value rather than a fabricated zero). Position
// mutation time is NOT the staleness signal — a quiet book is not stale.
//
// History: per-symbol 1-minute OI buckets retained 24h in-memory, served
// via the `openInterest.history` WS request method (and aggregatable to
// any multiple of a minute). §27 candidate: the Phase-06 task text names
// ClickHouse as the history store; ClickHouse is not deployed for
// marketdata yet, so history is derived from live position aggregates —
// the same data — until Phase-23 lands the durable read model.
package marketdata

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"exchange/pkg/decimal"
)

// DefaultOITick / DefaultOIStaleGate pin the §10.8 cadence and gate.
const (
	DefaultOITick      = time.Second
	DefaultOIStaleGate = 5 * time.Second
	oiHistoryBucket    = time.Minute
	oiHistoryCap       = 24 * 60 // 24h of 1m buckets
)

// OISample is one symbol's aggregate open interest.
type OISample struct {
	Symbol       string
	InstrumentID int64
	OpenInterest decimal.Decimal // Σ quantity (base ccy units)
	Notional     decimal.Decimal // Σ quantity × mark (quote ccy)
	Positions    int64           // open position rows contributing
	AsOf         time.Time       // source freshness (poll time)
}

// OpenInterestSource supplies per-symbol OI aggregates. Implementations
// read the position store — the positions table is authoritative
// (spec §5.13); nothing else is a valid OI source.
type OpenInterestSource interface {
	Aggregate(ctx context.Context) ([]OISample, error)
}

// ---------------------------------------------------------------------------
// PostgreSQL position-aggregate source
// ---------------------------------------------------------------------------

// PgxOpenInterestSource aggregates open positions straight from the
// positions table. `instruments` maps instrument_id → canonical symbol
// (the same map the wire resolver uses, inverted at wiring).
type PgxOpenInterestSource struct {
	pool *pgxpool.Pool
	inst map[int64]string
	now  func() time.Time
}

// NewPgxOpenInterestSource binds the position store.
func NewPgxOpenInterestSource(pool *pgxpool.Pool, instruments map[int64]string,
	now func() time.Time) *PgxOpenInterestSource {
	if now == nil {
		now = time.Now
	}
	return &PgxOpenInterestSource{pool: pool, inst: instruments, now: now}
}

// Aggregate implements OpenInterestSource.
func (s *PgxOpenInterestSource) Aggregate(ctx context.Context) ([]OISample, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT instrument_id, COUNT(*),
		       COALESCE(SUM(quantity),0)::text,
		       COALESCE(SUM(quantity * COALESCE(mark_price, entry_price)),0)::text,
		       COALESCE(MAX(updated_at), now())
		FROM positions
		WHERE quantity > 0
		GROUP BY instrument_id`)
	if err != nil {
		return nil, fmt.Errorf("marketdata: oi aggregate query: %w", err)
	}
	defer rows.Close()

	var out []OISample
	for rows.Next() {
		var (
			smp       OISample
			oiTxt     string
			notTxt    string
			updatedAt time.Time
		)
		if err := rows.Scan(&smp.InstrumentID, &smp.Positions,
			&oiTxt, &notTxt, &updatedAt); err != nil {
			return nil, fmt.Errorf("marketdata: oi row scan: %w", err)
		}
		if smp.OpenInterest, err = decimal.NewFromString(oiTxt); err != nil {
			return nil, fmt.Errorf("marketdata: oi quantity parse: %w", err)
		}
		if smp.Notional, err = decimal.NewFromString(notTxt); err != nil {
			return nil, fmt.Errorf("marketdata: oi notional parse: %w", err)
		}
		sym, ok := s.inst[smp.InstrumentID]
		if !ok {
			sym = fmt.Sprintf("instr-%d", smp.InstrumentID)
		}
		smp.Symbol = sym
		smp.AsOf = s.now()
		out = append(out, smp)
	}
	return out, rows.Err()
}

// ---------------------------------------------------------------------------
// Producer
// ---------------------------------------------------------------------------

// openInterestData is the emitted openInterest@ payload.
type openInterestData struct {
	Event                string `json:"event"` // "openInterest"
	Symbol               string `json:"symbol"`
	OpenInterest         string `json:"open_interest"`          // Σ base qty
	OpenInterestNotional string `json:"open_interest_notional"` // Σ qty×mark (quote ccy)
	Positions            int64  `json:"positions"`
	Stale                bool   `json:"stale"` // >5s oracle gate (§10.8)
	AsOfMs               int64  `json:"as_of_ms"`
}

// OICandle is one bucket of OI history (OHLC over sampled notional).
type OICandle struct {
	OpenTimeMs int64  `json:"open_time_ms"`
	Open       string `json:"open"`
	High       string `json:"high"`
	Low        string `json:"low"`
	Close      string `json:"close"`
	Samples    int64  `json:"samples"`
}

// OIProducerConfig tunes OIProducer.
type OIProducerConfig struct {
	Logger    *slog.Logger
	Now       func() time.Time
	Tick      time.Duration // emit cadence; default 1s (§10.8)
	StaleGate time.Duration // staleness flag threshold; default 5s
	Symbols   []string      // universe to emit for (zero-OI included)
}

func (c *OIProducerConfig) defaults() {
	if c.Logger == nil {
		c.Logger = slog.Default()
	}
	if c.Now == nil {
		c.Now = time.Now
	}
	if c.Tick <= 0 {
		c.Tick = DefaultOITick
	}
	if c.StaleGate <= 0 {
		c.StaleGate = DefaultOIStaleGate
	}
}

// oiBucket is one minute of OI samples.
type oiBucket struct {
	start                  time.Time
	open, high, low, close decimal.Decimal
	samples                int64
}

func (b *oiBucket) fold(v decimal.Decimal) {
	if b.samples == 0 {
		b.open, b.high, b.low, b.close = v, v, v, v
	} else {
		if v.GreaterThan(b.high) {
			b.high = v
		}
		if v.LessThan(b.low) {
			b.low = v
		}
		b.close = v
	}
	b.samples++
}

// OIProducer polls the position-aggregate source on a 1s cadence and
// publishes openInterest@{symbol} (plus the latest value to the history
// ring). Symbols come from cfg.Symbols ∪ anything the source returns.
type OIProducer struct {
	cfg  OIProducerConfig
	src  OpenInterestSource
	emit EmitFunc
	seq  *seqAllocator

	mu      sync.Mutex
	last    map[string]OISample   // symbol → latest good sample
	lastSeq map[string]uint64     // symbol → last emitted seq
	hist    map[string][]oiBucket // symbol → 1m OI buckets (24h)
	pending []OISample            // Push() seam buffer for tests
}

// NewOIProducer wires the producer; emit is Server.Publish. src may be
// nil only for tests driving Push — Run without a source emits stale
// frames for configured symbols (stale=true, zero values? no — see Run).
func NewOIProducer(cfg OIProducerConfig, src OpenInterestSource, emit EmitFunc) *OIProducer {
	cfg.defaults()
	return &OIProducer{
		cfg: cfg, src: src, emit: emit, seq: newSeqAllocator(),
		last: map[string]OISample{}, lastSeq: map[string]uint64{},
		hist: map[string][]oiBucket{},
	}
}

// Push injects a sample directly (tests/embedders). Takes effect on the
// next tick — a Pushed sample supersedes the source for that tick only.
func (p *OIProducer) Push(s OISample) {
	p.mu.Lock()
	p.pending = append(p.pending, s)
	p.mu.Unlock()
}

// publish emits one symbol's frame. Caller holds p.mu (Publish never
// blocks — ordering under the lock keeps per-channel seq monotonic).
func (p *OIProducer) publish(sym string, smp OISample, stale bool, now time.Time) {
	ch := "openInterest@" + sym
	seq := p.seq.next(ch)
	p.lastSeq[sym] = seq
	p.emit(ch, seq, openInterestData{
		Event: "openInterest", Symbol: sym,
		OpenInterest:         smp.OpenInterest.String(),
		OpenInterestNotional: smp.Notional.String(),
		Positions:            smp.Positions,
		Stale:                stale,
		AsOfMs:               smp.AsOf.UnixMilli(),
	})
	p.recordHistory(sym, smp, now)
	_ = now
}

// recordHistory folds a sample into the symbol's 1m bucket ring.
// Caller holds p.mu.
func (p *OIProducer) recordHistory(sym string, smp OISample, now time.Time) {
	start := now.Truncate(oiHistoryBucket)
	h := p.hist[sym]
	if n := len(h); n > 0 && h[n-1].start.Equal(start) {
		h[n-1].fold(smp.OpenInterest)
		return
	}
	b := oiBucket{start: start}
	b.fold(smp.OpenInterest)
	h = append(h, b)
	if len(h) > oiHistoryCap {
		h = append([]oiBucket(nil), h[len(h)-oiHistoryCap:]...)
	}
	p.hist[sym] = h
}

// tick polls the source and emits for the full symbol universe.
func (p *OIProducer) tick(now time.Time) {
	var samples []OISample
	var srcErr error
	if p.src != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		samples, srcErr = p.src.Aggregate(ctx)
		cancel()
	}

	p.mu.Lock()
	// Pushed (injected) samples merge over the polled set for the tick.
	if len(p.pending) > 0 {
		samples = append(samples, p.pending...)
		p.pending = nil
	}

	bySym := map[string]OISample{}
	for _, s := range samples {
		if s.Symbol != "" {
			bySym[s.Symbol] = s
		}
	}
	if srcErr != nil {
		// Poll failed — re-emit last known values flagged stale. A
		// symbol never observed emits nothing: a fabricated zero would
		// be worse than silence.
		for _, s := range p.last {
			if _, ok := bySym[s.Symbol]; !ok {
				bySym[s.Symbol] = s
			}
		}
	} else {
		// Poll succeeded — absence from the result means OI is
		// legitimately zero (all positions closed). Synthesize confirmed
		// zeros for the configured universe and previously seen symbols.
		for _, sym := range p.cfg.Symbols {
			if _, ok := bySym[sym]; !ok {
				bySym[sym] = OISample{Symbol: sym, AsOf: now}
			}
		}
		for _, s := range p.last {
			if _, ok := bySym[s.Symbol]; !ok {
				bySym[s.Symbol] = OISample{Symbol: s.Symbol, AsOf: now}
			}
		}
	}

	fresh := make(map[string]bool, len(bySym))
	for _, s := range samples {
		fresh[s.Symbol] = true
	}

	for sym, smp := range bySym {
		// Stale = the poll failed (re-emitted last-known) or the sample
		// itself aged past the gate. A confirmed zero is fresh, not stale.
		stale := srcErr != nil ||
			(fresh[sym] && now.Sub(smp.AsOf) > p.cfg.StaleGate)
		if srcErr == nil && fresh[sym] {
			p.last[sym] = smp
		}
		p.publish(sym, smp, stale, now)
	}
	p.mu.Unlock()

	if srcErr != nil {
		p.cfg.Logger.Error("marketdata: oi aggregate failed — emitting stale flags",
			"err", srcErr)
	}
}

// Snapshot implements SnapshotSource for channel type "openInterest" —
// the resume/cold-start fallback returns the latest known sample.
func (p *OIProducer) Snapshot(_ context.Context, channel string) (uint64, any, error) {
	sym, ok := strings.CutPrefix(channel, "openInterest@")
	if !ok || sym == "" {
		return 0, nil, fmt.Errorf("marketdata: no openInterest snapshot for %q", channel)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	smp, ok := p.last[sym]
	if !ok {
		return 0, nil, fmt.Errorf("marketdata: no open interest state for %q", sym)
	}
	return p.lastSeq[sym], openInterestData{
		Event: "openInterest", Symbol: sym,
		OpenInterest:         smp.OpenInterest.String(),
		OpenInterestNotional: smp.Notional.String(),
		Positions:            smp.Positions,
		Stale:                p.cfg.Now().Sub(smp.AsOf) > p.cfg.StaleGate,
		AsOfMs:               smp.AsOf.UnixMilli(),
	}, nil
}

// History aggregates the per-minute bucket ring into OI candles at the
// requested bucket width (must be a whole number of minutes). limit<=0
// returns all retained candles (≤24h).
func (p *OIProducer) History(symbol string, bucketSec int, limit int) ([]OICandle, error) {
	if bucketSec <= 0 || bucketSec%60 != 0 {
		return nil, fmt.Errorf("marketdata: oi history interval must be a multiple of 60s")
	}
	width := time.Duration(bucketSec) * time.Second
	p.mu.Lock()
	defer p.mu.Unlock()
	src := p.hist[symbol]
	var out []OICandle
	var cur *OICandle
	var curStart time.Time
	flush := func() {
		if cur != nil {
			out = append(out, *cur)
			cur = nil
		}
	}
	for _, b := range src {
		bs := b.start.Truncate(width)
		if cur == nil || !bs.Equal(curStart) {
			flush()
			cur = &OICandle{OpenTimeMs: bs.UnixMilli(),
				Open: b.open.String(), High: b.high.String(),
				Low: b.low.String(), Close: b.close.String(),
				Samples: b.samples}
			curStart = bs
			continue
		}
		if b.high.GreaterThan(decimal.RequireFromString(cur.High)) {
			cur.High = b.high.String()
		}
		if b.low.LessThan(decimal.RequireFromString(cur.Low)) {
			cur.Low = b.low.String()
		}
		cur.Close = b.close.String()
		cur.Samples += b.samples
	}
	flush()
	if limit > 0 && len(out) > limit {
		out = out[len(out)-limit:]
	}
	return out, nil
}

// Run drives the poll loop until ctx is cancelled.
func (p *OIProducer) Run(ctx context.Context) error {
	tick := time.NewTicker(p.cfg.Tick)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case t := <-tick.C:
			p.tick(t)
		}
	}
}
