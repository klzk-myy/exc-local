// Phase-23 Task 23.3.5 — real-time Greeks market data feed
// (spec §15.2, §24 #250; premium bundle member of Task 23.3.3).
//
// greeks@{symbol} publishes the full option sensitivity matrix —
// {delta, gamma, vega, theta, rho} per strike per expiry — for one
// underlying FX pair at a 100ms cadence (spec §24 #250). Computation is
// the Phase-22 pricing engine itself: European contracts take the
// analytic Garman-Kohlhagen Greeks (options.VanillaOption.Greeks,
// model "GK"); American contracts take finite-difference Greeks over
// the Kamrad–Ritchken trinomial lattice (options.PriceAmerican,
// model "LATTICE") — the feed labels every row with its model so no
// consumer can mistake the method.
//
// Fail-closed staleness contract (the task's "age/staleness flag" +
// the input gates' propagation rule): the published frame carries
// age_ms (age of the mark input) and a stale flag. Any of — stale mark
// (> GreeksStaleGate = the canonical 5s oracle gate), an upstream
// Stale flag, incomplete/stale yield curves, an IV resolver failure, a
// contract-universe failure with no cache — freezes the frame at its
// last computed matrix with stale:true. The feed NEVER fabricates
// Greeks from stale inputs: frozen rows are explicitly labelled and
// fresh ticks resume automatically when inputs recover.
//
// Distribution: each tick emits on greeks@{symbol} via the EmitFunc
// (Server.Publish) fan-out and optionally republishes the identical
// marshalled frame onto the JetStream backbone under the
// "{stream}.{shard}.{symbol}" subject convention — subject
// "analytics.{shard}.greeks-{SYMBOL}" (the greeks- token prefix keeps
// JSON rows disjoint from the bridge's wire-event payloads on the same
// stream). Historical snapshots land in ClickHouse
// exchange_analytics.greeks_snapshots (deploy/clickhouse/schema/008)
// through the GreeksSnapshotStore seam at the HistoryEvery cadence.
//
// Entitlement: greeks@ is a premium-bundle channel — binds route
// through FeedEntitlements (premium.go): authenticated, tier ≥
// Professional, ACTIVE feed subscription (migration 257).
package marketdata

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"math"
	"sync"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
	"github.com/jackc/pgx/v5/pgxpool"

	excnats "exchange/internal/nats"
	"exchange/internal/options"
	"exchange/internal/oracle/rates"
	"exchange/pkg/decimal"
)

// Spec-pinned cadence + staleness constants (§24 #250, §2.7).
const (
	// GreeksFeedCadence is the 100ms publication interval of §24 #250.
	GreeksFeedCadence = 100 * time.Millisecond
	// GreeksStaleGate is the canonical 5s oracle staleness gate — a mark
	// older than this freezes publication (never fabricate Greeks off a
	// stale mark/IV, spec §15.2 task text).
	GreeksStaleGate = 5 * time.Second
)

// ---------------------------------------------------------------------------
// Contract universe + market inputs — the seams production wires.
// ---------------------------------------------------------------------------

// OptionContract is one active option series on an underlying — the
// terms row the Greeks matrix is computed over. Sourced from the
// booked-terms store (option_positions, migration 255 — the derivative
// contract store of migration 254 covers FORWARD/SWAP/NDF, which carry
// no strike/expiry; option terms live here). A dedicated listed-series
// table is a Phase-22 follow-up; the DISTINCT terms set covers every
// contract currently carrying open interest.
type OptionContract struct {
	Symbol     string                // option instrument symbol
	Underlying string                // canonical pair "EUR/USD"
	Right      options.OptionRight   // CALL|PUT
	Style      options.ExerciseStyle // EUROPEAN|AMERICAN
	Strike     float64               // quote per base
	Expiry     time.Time             // expiry instant
}

// OptionSeriesSource enumerates the active contracts of one underlying.
// Implementations are read-only and fail loudly — a feed that cannot
// enumerate its universe must freeze, not guess.
type OptionSeriesSource interface {
	Contracts(ctx context.Context, underlying string) ([]OptionContract, error)
}

// OptionSeriesSourceFunc adapts a function to OptionSeriesSource.
type OptionSeriesSourceFunc func(ctx context.Context, underlying string) ([]OptionContract, error)

// Contracts implements OptionSeriesSource.
func (f OptionSeriesSourceFunc) Contracts(ctx context.Context, underlying string) ([]OptionContract, error) {
	return f(ctx, underlying)
}

// PgxOptionSeriesSource derives the contract universe from the booked
// option terms (option_positions, migration 255): every OPEN row's
// (option_type, exercise_style, strike, expiry) is a live series. The
// underlying resolves through underlying_instrument_id → instruments
// (physical delivery), falling back to the option instrument's own
// base/quote for CASH-settled series where the column is NULL.
type PgxOptionSeriesSource struct {
	pool *pgxpool.Pool
}

// NewPgxOptionSeriesSource binds the option-terms store.
func NewPgxOptionSeriesSource(pool *pgxpool.Pool) *PgxOptionSeriesSource {
	return &PgxOptionSeriesSource{pool: pool}
}

// Contracts implements OptionSeriesSource.
func (s *PgxOptionSeriesSource) Contracts(ctx context.Context, underlying string) ([]OptionContract, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT DISTINCT i.symbol, o.option_type, o.exercise_style,
		       o.strike::text, o.expiry_at,
		       COALESCE(u.symbol, i.base_currency || '/' || i.quote_currency)
		FROM option_positions o
		JOIN instruments i  ON i.id = o.instrument_id
		LEFT JOIN instruments u ON u.id = o.underlying_instrument_id
		WHERE o.status = 'OPEN'
		  AND COALESCE(u.symbol, i.base_currency || '/' || i.quote_currency) = $1
		ORDER BY 6, 4`, underlying)
	if err != nil {
		return nil, fmt.Errorf("marketdata: option series query: %w", err)
	}
	defer rows.Close()
	var out []OptionContract
	for rows.Next() {
		var (
			c         OptionContract
			strikeTxt string
		)
		if err := rows.Scan(&c.Symbol, &c.Right, &c.Style,
			&strikeTxt, &c.Expiry, &c.Underlying); err != nil {
			return nil, fmt.Errorf("marketdata: option series row: %w", err)
		}
		k, err := decimal.NewFromString(strikeTxt)
		if err != nil {
			return nil, fmt.Errorf("marketdata: option strike parse: %w", err)
		}
		c.Strike = k.InexactFloat64()
		out = append(out, c)
	}
	return out, rows.Err()
}

// ---------------------------------------------------------------------------
// Market inputs
// ---------------------------------------------------------------------------

// VolFunc resolves the annualized implied vol for one (strike, tenor)
// pair — production wiring binds the Phase-22 IVSurface.Vol surface.
// An error is an input failure: the frame freezes (stale:true), the
// feed never substitutes a guessed vol.
type VolFunc func(strike, tYears float64) (float64, error)

// GreeksInput is one tick of market inputs for one underlying.
type GreeksInput struct {
	// Mark is the current underlying mark/spot.
	Mark float64
	// MarkAt is the mark's observation time — age_ms and the staleness
	// gate derive from it. A zero MarkAt is itself stale.
	MarkAt time.Time
	// Stale is any upstream gate already tripped (oracle mark staleness,
	// IV-surface freshness) — propagated verbatim.
	Stale bool
	// Base/Quote are the foreign/domestic discount curves
	// (oracle/rates, Task 19.5.3.5); MarketFromCurves re-validates
	// completeness + staleness per expiry.
	Base  rates.Curve
	Quote rates.Curve
	// Vol resolves per-contract implied vol (nil → freeze).
	Vol VolFunc
}

// GreeksInputSource supplies the market inputs per underlying — the
// production implementation reads the mark oracle + curve snapshot +
// IV surface (Phase-19.5/22 seams); tests inject fakes.
type GreeksInputSource interface {
	Snapshot(ctx context.Context, underlying string) (GreeksInput, error)
}

// GreeksInputSourceFunc adapts a function to GreeksInputSource.
type GreeksInputSourceFunc func(ctx context.Context, underlying string) (GreeksInput, error)

// Snapshot implements GreeksInputSource.
func (f GreeksInputSourceFunc) Snapshot(ctx context.Context, underlying string) (GreeksInput, error) {
	return f(ctx, underlying)
}

// ---------------------------------------------------------------------------
// Greeks computation — GK closed form (EUROPEAN) / lattice FD (AMERICAN).
// ---------------------------------------------------------------------------

// GreeksPricer computes one contract's sensitivity bundle; the returned
// model label ("GK"|"LATTICE") is carried on the wire row.
type GreeksPricer interface {
	Greeks(c OptionContract, m options.Market) (options.Greeks, string, error)
}

// OptionsGreeksPricer is the production pricer: analytic GK Greeks for
// European contracts, finite differences over the trinomial lattice for
// American (the options package exposes a lattice price, not analytic
// American Greeks — bump-and-reprice is the honest method, and the row
// says so).
type OptionsGreeksPricer struct {
	// Lattice bounds the American leg's refinement loop. Zero → the
	// feed default FeedLatticeConfig — deliberately cheaper than
	// options.DefaultLatticeConfig: this is an informational market-data
	// product at 10Hz, not a margin/risk number; the "LATTICE" label on
	// the row discloses the method.
	Lattice options.LatticeConfig
}

// FeedLatticeConfig is the default lattice budget for feed Greeks —
// tighter than the §15.2 pricing-engine floor (100 steps / 1e-4) because
// a 10Hz broadcast cannot afford per-contract convergence to that
// tolerance; risk/margin consumers still use the full config.
var FeedLatticeConfig = options.LatticeConfig{MinSteps: 64, MaxSteps: 512, Tolerance: 1e-3}

func (p *OptionsGreeksPricer) latticeCfg() options.LatticeConfig {
	if p.Lattice.MinSteps > 0 {
		return p.Lattice
	}
	return FeedLatticeConfig
}

// Greeks implements GreeksPricer.
func (p *OptionsGreeksPricer) Greeks(c OptionContract, m options.Market) (options.Greeks, string, error) {
	switch c.Style {
	case options.ExerciseEuropean:
		g, err := options.VanillaOption{
			Right: c.Right, Strike: c.Strike, Style: c.Style,
		}.Greeks(m)
		return g, "GK", err
	case options.ExerciseAmerican:
		g, err := p.latticeGreeks(c, m)
		return g, "LATTICE", err
	}
	return options.Greeks{}, "", fmt.Errorf(
		"marketdata: unsupported exercise style %q", c.Style)
}

// latticePrice is one American evaluation at market m.
func (p *OptionsGreeksPricer) latticePrice(c OptionContract, m options.Market) (float64, error) {
	res, err := options.PriceAmerican(options.VanillaOption{
		Right: c.Right, Strike: c.Strike, Style: options.ExerciseAmerican,
	}, m, p.latticeCfg())
	if err != nil {
		return 0, err
	}
	return res.Price, nil
}

// latticeGreeks computes bump-and-reprice Greeks over the lattice:
// central spot/vol differences, a forward time-decay difference
// (per year, matching the GK theta convention), and discount-factor
// rate bumps rd/rf → DF·e^{−εT} for rho_domestic/rho_foreign. Every
// evaluation must return finite — any non-finite leg fails the row
// closed (§2.7, §24 #324).
func (p *OptionsGreeksPricer) latticeGreeks(c OptionContract, m options.Market) (options.Greeks, error) {
	v0, err := p.latticePrice(c, m)
	if err != nil {
		return options.Greeks{}, err
	}
	var g options.Greeks
	finiteOK := func(v float64) bool { return !math.IsNaN(v) && !math.IsInf(v, 0) }

	// Spot: delta + gamma (central difference).
	h := math.Max(m.Spot*1e-4, 1e-7)
	up, dn := m, m
	up.Spot += h
	dn.Spot -= h
	vUp, err := p.latticePrice(c, up)
	if err != nil {
		return options.Greeks{}, err
	}
	vDn, err := p.latticePrice(c, dn)
	if err != nil {
		return options.Greeks{}, err
	}
	g.Delta = (vUp - vDn) / (2 * h)
	g.Gamma = (vUp - 2*v0 + vDn) / (h * h)

	// Vol: vega per unit absolute vol (central; one-sided near σ→0).
	const ve = 1e-4
	if m.Vol-ve > 0 {
		vu, vd := m, m
		vu.Vol += ve
		vd.Vol -= ve
		a, err := p.latticePrice(c, vu)
		if err != nil {
			return options.Greeks{}, err
		}
		b, err := p.latticePrice(c, vd)
		if err != nil {
			return options.Greeks{}, err
		}
		g.Vega = (a - b) / (2 * ve)
	} else {
		vu := m
		vu.Vol += ve
		a, err := p.latticePrice(c, vu)
		if err != nil {
			return options.Greeks{}, err
		}
		g.Vega = (a - v0) / ve
	}

	// Theta per year: (V(T−δ) − V(T))/δ — same decay-sign convention as
	// the GK analytic theta.
	dt := math.Min(1.0/365.0, m.T/2)
	if dt > 0 {
		tm := m
		tm.T -= dt
		vT, err := p.latticePrice(c, tm)
		if err != nil {
			return options.Greeks{}, err
		}
		g.Theta = (vT - v0) / dt
	}

	// Rates: rho per unit absolute rate via DF bumps.
	const re = 1e-4
	dfd := m
	dfd.DFd *= math.Exp(-re * m.T)
	vd, err := p.latticePrice(c, dfd)
	if err != nil {
		return options.Greeks{}, err
	}
	g.RhoDomestic = (vd - v0) / re
	dff := m
	dff.DFf *= math.Exp(-re * m.T)
	vf, err := p.latticePrice(c, dff)
	if err != nil {
		return options.Greeks{}, err
	}
	g.RhoForeign = (vf - v0) / re

	for _, v := range []float64{g.Delta, g.Gamma, g.Vega, g.Theta, g.RhoDomestic, g.RhoForeign} {
		if !finiteOK(v) {
			return options.Greeks{}, fmt.Errorf(
				"marketdata: non-finite lattice greek for %s K=%v", c.Symbol, c.Strike)
		}
	}
	return g, nil
}

// ---------------------------------------------------------------------------
// Wire payload
// ---------------------------------------------------------------------------

// greeksRow is one contract's sensitivity line. Money/rate quantities
// render as decimal strings (the package convention); expiry is
// RFC3339 UTC.
type greeksRow struct {
	Contract   string `json:"contract"` // option instrument symbol
	Right      string `json:"right"`    // CALL|PUT
	Style      string `json:"style"`    // EUROPEAN|AMERICAN
	Expiry     string `json:"expiry"`   // RFC3339
	Strike     string `json:"strike"`   // decimal string
	Model      string `json:"model"`    // "GK" | "LATTICE"
	Delta      string `json:"delta"`
	Gamma      string `json:"gamma"`
	Vega       string `json:"vega"`
	Theta      string `json:"theta"`
	Rho        string `json:"rho"`         // domestic (quote-ccy) rate sensitivity
	RhoForeign string `json:"rho_foreign"` // foreign (base-ccy) rate sensitivity
}

// greeksData is the emitted frame payload. On stale inputs Rows carries
// the last computed matrix verbatim with Stale=true — "frozen", never
// recomputed; a feed that has never computed emits Rows empty.
type greeksData struct {
	Event  string      `json:"event"`  // "greeks"
	Symbol string      `json:"symbol"` // underlying pair
	Rows   []greeksRow `json:"rows"`
	Stale  bool        `json:"stale"`
	AgeMs  int64       `json:"age_ms"` // mark-input age; -1 when no timestamp
	Trunc  bool        `json:"truncated,omitempty"`
	TsMs   int64       `json:"ts_ms"`
}

func f64decStr(v float64) string { return decimal.NewFromFloat(v).String() }

// ---------------------------------------------------------------------------
// Distribution seams — NATS republish + ClickHouse history.
// ---------------------------------------------------------------------------

// GreeksPublisher republishes a marshalled greeks frame onto the event
// backbone (NATS "{stream}.{shard}.{symbol}" convention). Nil disables
// the hop — the WS fan-out is unaffected.
type GreeksPublisher interface {
	PublishGreeks(ctx context.Context, underlying string, frame []byte) error
}

// GreeksPublisherFunc adapts a function to GreeksPublisher.
type GreeksPublisherFunc func(ctx context.Context, underlying string, frame []byte) error

// PublishGreeks implements GreeksPublisher.
func (f GreeksPublisherFunc) PublishGreeks(ctx context.Context, underlying string, frame []byte) error {
	return f(ctx, underlying, frame)
}

// jetStreamGreeksPublisher is the JetStream adapter: subject
// "analytics.{shard}.greeks-{SYMBOL}" — the analytics stream is the
// canonical market-data republish backbone; the greeks- token prefix
// keeps these JSON frames disjoint from the bridge's FlatBuffers
// wire-event subjects on the same stream (consumers filter subjects,
// never payload-sniff).
type jetStreamGreeksPublisher struct {
	nc     *excnats.Client
	stream string
	shard  uint32
}

// JetStreamGreeksPublisher wires the republish seam. stream "" selects
// the canonical "analytics" stream; shardID defaults to 0 for Go-side
// producers (the shard router owns the engine's sharding domain — a
// republished derived product rides shard 0).
func JetStreamGreeksPublisher(nc *excnats.Client, stream string, shardID uint32) GreeksPublisher {
	if stream == "" {
		stream = "analytics"
	}
	return &jetStreamGreeksPublisher{nc: nc, stream: stream, shard: shardID}
}

// PublishGreeks implements GreeksPublisher.
func (p *jetStreamGreeksPublisher) PublishGreeks(ctx context.Context, underlying string, frame []byte) error {
	token := excnats.SymbolToken(underlying)
	_, err := p.nc.Publish(ctx, p.stream, p.shard, "greeks-"+token, frame)
	return err
}

// GreeksSnapshotRow is one ClickHouse greeks_snapshots row — the
// historical backtesting record (schema 008).
type GreeksSnapshotRow struct {
	Symbol     string // option instrument symbol
	Underlying string
	Right      string
	Style      string
	Model      string
	Expiry     time.Time
	Strike     float64
	Delta      float64
	Gamma      float64
	Vega       float64
	Theta      float64
	Rho        float64
	RhoForeign float64
	Ts         time.Time // publish tick time
}

// GreeksSnapshotStore is the historical sink seam — the ClickHouse
// ingest path. Insert errors are logged and counted, never fatal to
// the feed (market data must not stall on the analytics lane).
type GreeksSnapshotStore interface {
	InsertGreeksSnapshots(ctx context.Context, rows []GreeksSnapshotRow) error
}

// GreeksTable is the schema-008 table in exchange_analytics (the
// TickStore convention — unqualified name, the Conn carries the db).
const GreeksTable = "greeks_snapshots"

const greeksColumns = "ts, symbol, underlying, right, style, model, " +
	"expiry, strike, delta, gamma, vega, theta, rho, rho_foreign"

// CHBatchConn is the narrow columnar-batch surface the sink needs —
// internal/analytics.Conn (ch.go) satisfies it implicitly. marketdata
// cannot import analytics directly (analytics → funding → marketdata
// is an import cycle), so the identical PrepareBatch signature is
// declared here — the seam IS the analytics store path.
type CHBatchConn interface {
	PrepareBatch(ctx context.Context, query string, opts ...driver.PrepareBatchOption) (driver.Batch, error)
}

// CHGreeksStore implements GreeksSnapshotStore over the shared
// analytics.Conn-compatible seam (same PrepareBatch columnar path as
// analytics.TickStore).
type CHGreeksStore struct {
	conn CHBatchConn
}

// NewCHGreeksStore binds the sink.
func NewCHGreeksStore(conn CHBatchConn) *CHGreeksStore {
	return &CHGreeksStore{conn: conn}
}

// InsertGreeksSnapshots implements GreeksSnapshotStore.
func (s *CHGreeksStore) InsertGreeksSnapshots(ctx context.Context, rows []GreeksSnapshotRow) error {
	if len(rows) == 0 {
		return nil
	}
	batch, err := s.conn.PrepareBatch(ctx,
		"INSERT INTO "+GreeksTable+" ("+greeksColumns+")")
	if err != nil {
		return fmt.Errorf("greeks snapshot batch prepare: %w", err)
	}
	for i := range rows {
		r := rows[i]
		if err := batch.Append(r.Ts.UTC(), r.Symbol, r.Underlying,
			r.Right, r.Style, r.Model, r.Expiry.UTC(),
			r.Strike, r.Delta, r.Gamma, r.Vega, r.Theta,
			r.Rho, r.RhoForeign); err != nil {
			_ = batch.Abort()
			return fmt.Errorf("greeks snapshot append %s: %w", r.Symbol, err)
		}
	}
	if err := batch.Send(); err != nil {
		return fmt.Errorf("greeks snapshot send (%d rows): %w", len(rows), err)
	}
	return nil
}

// ---------------------------------------------------------------------------
// The feed
// ---------------------------------------------------------------------------

// GreeksFeedConfig tunes GreeksFeed. Zero values pick spec defaults.
type GreeksFeedConfig struct {
	// Cadence is the publication interval — default 100ms (§24 #250).
	// Injectable for tests (tick-driven semantics live on Tick()).
	Cadence time.Duration
	// StaleAfter is the mark-input staleness gate — floored at the
	// canonical 5s oracle gate; a tighter configured gate is a bug, so
	// the constructor clamps UP, never down.
	StaleAfter time.Duration
	// ContractRefresh is the contract-universe re-enumeration interval —
	// default 60s (terms change on booking/lifecycle timescales, not
	// tick timescales). A failed refresh keeps the cached universe.
	ContractRefresh time.Duration
	// MaxContracts bounds the per-underlying matrix (default 500) —
	// beyond it the frame truncates with truncated:true.
	MaxContracts int
	// HistoryEvery is the ClickHouse snapshot interval per symbol —
	// default 1s (every 10th tick). Requires Sink non-nil to store at
	// all; a nil Sink disables history entirely.
	HistoryEvery time.Duration
	// HistoryTimeout bounds one sink write (default 2s).
	HistoryTimeout time.Duration

	// Symbols returns the underlyings with live greeks@ subscribers —
	// production wiring is Server.ActiveSymbolsFor(FeedGreeks): the feed
	// computes only what clients consume. Nil ⇒ no symbols (inert feed).
	Symbols func() []string
	// Contracts is the contract universe source (required).
	Contracts OptionSeriesSource
	// Inputs is the market-input source (required).
	Inputs GreeksInputSource
	// Pricer is the Greeks engine — nil selects OptionsGreeksPricer
	// (GK closed form + lattice FD).
	Pricer GreeksPricer
	// Publisher republishes frames to NATS — optional.
	Publisher GreeksPublisher
	// Sink stores historical snapshots — optional.
	Sink GreeksSnapshotStore

	Logger *slog.Logger
	Now    func() time.Time
}

func (c *GreeksFeedConfig) defaults() {
	if c.Cadence <= 0 {
		c.Cadence = GreeksFeedCadence
	}
	// §2.7 floor — a tighter configured gate would weaken staleness, so
	// the clamp goes UP to the canonical 5s, never down.
	if c.StaleAfter < GreeksStaleGate {
		c.StaleAfter = GreeksStaleGate
	}
	if c.ContractRefresh <= 0 {
		c.ContractRefresh = time.Minute
	}
	if c.MaxContracts <= 0 {
		c.MaxContracts = 500
	}
	if c.HistoryEvery <= 0 {
		c.HistoryEvery = time.Second // default: every 10th tick
	}
	if c.HistoryTimeout <= 0 {
		c.HistoryTimeout = 2 * time.Second
	}
	if c.Pricer == nil {
		c.Pricer = &OptionsGreeksPricer{}
	}
	if c.Now == nil {
		c.Now = time.Now
	}
	if c.Logger == nil {
		c.Logger = slog.Default()
	}
}

type cachedContracts struct {
	list []OptionContract
	at   time.Time
}

// GreeksFeed publishes the options Greeks matrix per underlying on the
// configured cadence. Run is the ticker loop; Tick performs one
// synchronous sweep — tests drive Tick directly so cadence is fully
// deterministic.
type GreeksFeed struct {
	cfg  GreeksFeedConfig
	emit EmitFunc
	seq  *seqAllocator

	mu        sync.Mutex
	contracts map[string]cachedContracts
	lastRows  map[string][]greeksRow // frozen matrix per underlying
	lastHist  map[string]time.Time   // last CH snapshot per underlying
}

// NewGreeksFeed wires the feed; emit is Server.Publish.
func NewGreeksFeed(cfg GreeksFeedConfig, emit EmitFunc) *GreeksFeed {
	cfg.defaults()
	return &GreeksFeed{
		cfg: cfg, emit: emit, seq: newSeqAllocator(),
		contracts: map[string]cachedContracts{},
		lastRows:  map[string][]greeksRow{},
		lastHist:  map[string]time.Time{},
	}
}

// symbols resolves the active subscription set.
func (f *GreeksFeed) symbols() []string {
	if f.cfg.Symbols == nil {
		return nil
	}
	return f.cfg.Symbols()
}

// contractsFor returns the contract universe for one underlying,
// refreshing at ContractRefresh and falling back to cache on fetch
// failure (an enumerable universe of slightly-old terms is not an
// input-staleness failure — only a never-fetched universe freezes).
func (f *GreeksFeed) contractsFor(ctx context.Context, underlying string, now time.Time) ([]OptionContract, bool) {
	f.mu.Lock()
	c, ok := f.contracts[underlying]
	f.mu.Unlock()
	if ok && now.Sub(c.at) < f.cfg.ContractRefresh {
		return c.list, true
	}
	if f.cfg.Contracts == nil {
		return nil, false // unwired source → frozen frame, never a panic
	}
	list, err := f.cfg.Contracts.Contracts(ctx, underlying)
	if err != nil {
		f.cfg.Logger.Error("marketdata: greeks contract universe fetch failed",
			"underlying", underlying, "err", err)
		if ok {
			return c.list, true // serve the cached universe
		}
		return nil, false
	}
	if len(list) > f.cfg.MaxContracts {
		list = list[:f.cfg.MaxContracts]
	}
	f.mu.Lock()
	f.contracts[underlying] = cachedContracts{list: list, at: now}
	f.mu.Unlock()
	return list, true
}

// frozenRows snapshots the last computed matrix (stale re-emission).
func (f *GreeksFeed) frozenRows(underlying string) []greeksRow {
	f.mu.Lock()
	defer f.mu.Unlock()
	rows := f.lastRows[underlying]
	if rows == nil {
		rows = []greeksRow{} // wire emits [] never null
	}
	return rows
}

// freeze publishes the stale frame: last computed rows (if any) +
// stale:true + the input age. Rows are NEVER recomputed off stale
// inputs — that is the §2.7 no-fabrication line.
func (f *GreeksFeed) freeze(underlying string, ageMs int64) {
	channel := FeedGreeks + "@" + underlying
	f.emit(channel, f.seq.next(channel), greeksData{
		Event: "greeks", Symbol: underlying,
		Rows: f.frozenRows(underlying), Stale: true,
		AgeMs: ageMs, TsMs: f.cfg.Now().UnixMilli(),
	})
}

// publishSymbol computes and emits one underlying's matrix for one tick.
func (f *GreeksFeed) publishSymbol(ctx context.Context, underlying string, now time.Time) {
	if f.cfg.Inputs == nil {
		f.freeze(underlying, -1) // unwired source → frozen, never a panic
		return
	}
	in, err := f.cfg.Inputs.Snapshot(ctx, underlying)
	if err != nil {
		f.cfg.Logger.Warn("marketdata: greeks input snapshot failed",
			"underlying", underlying, "err", err)
		f.freeze(underlying, -1)
		return
	}
	var ageMs int64 = -1
	if !in.MarkAt.IsZero() {
		ageMs = now.Sub(in.MarkAt).Milliseconds()
	}
	// Staleness gates propagate: upstream flag, mark older than the 5s
	// oracle gate (or absurdly future — a corrupted timestamp is also
	// stale), missing mark time, or incomplete/stale curves.
	age := now.Sub(in.MarkAt)
	if in.Stale || in.MarkAt.IsZero() ||
		age > f.cfg.StaleAfter || age < -f.cfg.StaleAfter ||
		!in.Base.Complete() || !in.Quote.Complete() ||
		in.Base.Stale || in.Quote.Stale || in.Vol == nil {
		f.freeze(underlying, ageMs)
		return
	}

	contracts, ok := f.contractsFor(ctx, underlying, now)
	if !ok {
		f.freeze(underlying, ageMs)
		return
	}

	rows := make([]greeksRow, 0, len(contracts))
	snap := make([]GreeksSnapshotRow, 0, len(contracts))
	truncated := len(contracts) >= f.cfg.MaxContracts
	for _, c := range contracts {
		// Expired contracts leave the matrix — time-to-expiry ≤ 0 is
		// a lifecycle state, not a data failure.
		days := int(math.Ceil(c.Expiry.Sub(now).Hours() / 24))
		if days <= 0 {
			continue
		}
		tYears := c.Expiry.Sub(now).Hours() / (24 * 365)
		vol, verr := in.Vol(c.Strike, tYears)
		if verr != nil {
			// IV failure freezes the whole frame — a partial matrix
			// silently drops rows a risk consumer may depend on.
			f.cfg.Logger.Warn("marketdata: greeks IV resolution failed",
				"underlying", underlying, "contract", c.Symbol, "err", verr)
			f.freeze(underlying, ageMs)
			return
		}
		mk, merr := options.MarketFromCurves(in.Mark, vol, days, in.Base, in.Quote)
		if merr != nil {
			f.cfg.Logger.Warn("marketdata: greeks market build failed",
				"underlying", underlying, "contract", c.Symbol, "err", merr)
			f.freeze(underlying, ageMs)
			return
		}
		g, model, gerr := f.cfg.Pricer.Greeks(c, mk)
		if gerr != nil {
			f.cfg.Logger.Warn("marketdata: greeks pricing failed",
				"underlying", underlying, "contract", c.Symbol, "err", gerr)
			f.freeze(underlying, ageMs)
			return
		}
		rows = append(rows, greeksRow{
			Contract: c.Symbol, Right: string(c.Right), Style: string(c.Style),
			Expiry: c.Expiry.UTC().Format(time.RFC3339),
			Strike: f64decStr(c.Strike), Model: model,
			Delta: f64decStr(g.Delta), Gamma: f64decStr(g.Gamma),
			Vega: f64decStr(g.Vega), Theta: f64decStr(g.Theta),
			Rho: f64decStr(g.RhoDomestic), RhoForeign: f64decStr(g.RhoForeign),
		})
		snap = append(snap, GreeksSnapshotRow{
			Symbol: c.Symbol, Underlying: underlying,
			Right: string(c.Right), Style: string(c.Style), Model: model,
			Expiry: c.Expiry, Strike: c.Strike,
			Delta: g.Delta, Gamma: g.Gamma, Vega: g.Vega, Theta: g.Theta,
			Rho: g.RhoDomestic, RhoForeign: g.RhoForeign, Ts: now,
		})
	}

	channel := FeedGreeks + "@" + underlying
	data := greeksData{
		Event: "greeks", Symbol: underlying, Rows: rows,
		AgeMs: ageMs, Trunc: truncated, TsMs: now.UnixMilli(),
	}
	f.mu.Lock()
	f.lastRows[underlying] = rows
	f.mu.Unlock()
	f.emit(channel, f.seq.next(channel), data)

	// Optional NATS republish — the identical marshalled payload.
	if f.cfg.Publisher != nil {
		if b, merr := json.Marshal(data); merr == nil {
			pctx, cancel := context.WithTimeout(ctx, time.Second)
			if perr := f.cfg.Publisher.PublishGreeks(pctx, underlying, b); perr != nil {
				f.cfg.Logger.Warn("marketdata: greeks NATS republish failed",
					"underlying", underlying, "err", perr)
			}
			cancel()
		}
	}

	// Optional ClickHouse history at the HistoryEvery cadence — fresh
	// (non-stale) rows only; frozen frames never write history (the
	// store would accumulate duplicated stale snapshots).
	if f.cfg.Sink != nil && len(snap) > 0 {
		f.mu.Lock()
		last := f.lastHist[underlying]
		due := last.IsZero() || now.Sub(last) >= f.cfg.HistoryEvery
		if due {
			f.lastHist[underlying] = now
		}
		f.mu.Unlock()
		if due {
			hctx, cancel := context.WithTimeout(ctx, f.cfg.HistoryTimeout)
			if herr := f.cfg.Sink.InsertGreeksSnapshots(hctx, snap); herr != nil {
				f.cfg.Logger.Error("marketdata: greeks snapshot store failed",
					"underlying", underlying, "err", herr)
			}
			cancel()
		}
	}
}

// Tick performs one synchronous publish sweep over the active-symbol
// set — the unit tests' deterministic driver.
func (f *GreeksFeed) Tick(ctx context.Context) {
	now := f.cfg.Now()
	for _, sym := range f.symbols() {
		f.publishSymbol(ctx, sym, now)
	}
}

// Run drives the feed on the configured cadence until ctx is cancelled.
func (f *GreeksFeed) Run(ctx context.Context) error {
	tick := time.NewTicker(f.cfg.Cadence)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-tick.C:
			f.Tick(ctx)
		}
	}
}
