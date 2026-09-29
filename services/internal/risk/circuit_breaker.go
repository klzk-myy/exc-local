// Phase-13 Task 13.3.1 / 13.3.9 — five-tier circuit breaker (spec §2.6,
// §2.7, §24 #313).
//
// Scopes, triggers, holds and recovery contracts are the spec §2.6
// canonical table — never re-tuned silently:
//
//	Scope               Trigger                                          Hold    Recovery
//	INSTRUMENT          price move > instrument_price_limit (def 5%)/60s  5 min   10/10 probes in 30s
//	ACCOUNT             3+ rapid losses > 5% of equity within 5 min       30 min  manual admin reset
//	VOLUME_SPIKE        1-min volume z-score >= 4.0σ vs trailing 1h       10 min  10/10 probes in 30s
//	OPTIONS_VOLATILITY  IV spike > 200% vs 30-day average                 15 min  10/10 probes in 30s
//	MARKET_WIDE         aggregate volatility > 20% on > 2 instruments     n/a     manual admin resume
//
// State machine: CLOSED → OPEN(hold) → HALF_OPEN(probe) → CLOSED.
// Every breaker persists as a Redis HASH circuit_breaker:{scope}:{id}
// (helpers in internal/redis/client.go); this service is the in-process
// authority with read-through hydration so a restart or a flag written by
// another replica still gates admission. Every transition is audited to
// circuit_breaker_events (migration 206), counted by
// circuit_breaker_transitions_total{scope,id}, and the live state is
// exported on circuit_breaker_state{scope,id} (0=CLOSED 1=HALF_OPEN
// 2=OPEN) plus the "admin.circuit_breaker" WS admin-monitor channel.
//
// Enforcement: orders.Service consults AdmitOrder on EVERY new-order
// admission path (Submit / batch entries / modify / cancel-replace) via
// the orders.BreakerGate seam. OPEN rejects with CIRCUIT_BREAKER_OPEN
// (spec §23, 503); HALF_OPEN admits and counts up to 10 probe orders
// inside the 30s window. A nil/unreadable store fails closed — an
// unverifiable breaker is treated as tripped (spec §2.7 pessimism).
//
// Trigger feeds (honest seams — no fabricated data):
//   - ObservePrice: per-symbol trade/reference price stream. Wired at the
//     gateway to the engine out-ring fill consumer (cmd/gateway/main.go);
//     a marketdata.ReferencePriceSource adapter may drive it once the
//     Phase-19.5 oracle lands.
//   - ObserveTrade: per-symbol fill notional feeding the 1-minute volume
//     buckets the VOLUME_SPIKE z-score is computed against.
//   - ObserveAccountLoss: realized-loss feed (loss as % of account
//     equity). No realized-P&L event pipeline exists in the gateway today
//     (PositionService.RealizedPnLDelta lands with Phase-19 margin) —
//     the method + record path are complete and the feed binds there.
//   - ObserveIV / IVSource: implied-volatility seam for
//     OPTIONS_VOLATILITY. Options are Phase-22 — NullIVSource is the
//     documented dev/null feed until an IV surface exists.
//
// Cooldown interpretation (spec §2.6 item 7 + Task 13.3.9): 60s minimum
// applies between a trip (entry into OPEN) and the next automated
// recovery transition (OPEN → HALF_OPEN). Trips are NEVER delayed —
// fail-closed always wins over cooldown. HALF_OPEN → CLOSED/OPEN is
// bounded by the 30s probe window itself, which is tighter than 60s and
// cannot compose with a 60s inter-transition lock; the cooldown therefore
// pins the trip→probe gap (doubled holds included) exactly as the
// remediation #35 note intends.
//
// Flapping (Task 13.3.9): a fresh automated trip within 15 min of the
// last recovery to CLOSED doubles the hold period (capped at 120 min) and
// pages Risk Management via the FlapAlerter seam. A failed probe window
// (HALF_OPEN → OPEN before CLOSED is reached) is NOT "recovery" — the
// episode re-opens with the same effective hold, no doubling.
package risk

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"exchange/internal/config"
	"exchange/internal/observability"
	excredis "exchange/internal/redis"
	"exchange/pkg/decimal"
	excerrors "exchange/pkg/errors"
)

// CodeCircuitBreakerOpen is the spec §23 rejection emitted by the
// admission gate (503/L1 — already registered, owned by this task pair).
const CodeCircuitBreakerOpen = "CIRCUIT_BREAKER_OPEN"

// Scope tokens — spec §2.6 canonical names, also the enum values of
// circuit_breaker_scope_enum (migration 206) and the {scope} segment of
// the Redis HASH key.
const (
	ScopeInstrument        = "INSTRUMENT"
	ScopeAccount           = "ACCOUNT"
	ScopeVolumeSpike       = "VOLUME_SPIKE"
	ScopeOptionsVolatility = "OPTIONS_VOLATILITY"
	ScopeMarketWide        = "MARKET_WIDE"
)

// MarketWideID is the single instance id of the MARKET_WIDE breaker.
const MarketWideID = "MARKET"

// BreakerAdminChannel is the WS admin-monitor channel every state change
// is streamed to (Task 13.3.9 item 3). *ws.Server satisfies Publisher;
// there is no pre-existing admin monitor channel in the codebase, so this
// name is the documented seam the ops console subscribes to.
const BreakerAdminChannel = "admin.circuit_breaker"

// Canonical spec §2.6 trigger parameters (do not retune).
const (
	InstrumentMoveWindow      = 60 * time.Second
	AccountLossWindow         = 5 * time.Minute
	AccountLossMinCount       = 3
	VolumeBucket              = time.Minute
	VolumeBaselineBuckets     = 60 // trailing 1h of 1-min buckets
	VolumeBaselineMinBuckets  = 30 // fewer → baseline too thin to z-score
	MarketWideMinInstruments  = 3  // "> 2 instruments"
	ProbeCount                = 10
	ProbeWindow               = 30 * time.Second
	TransitionCooldown        = 60 * time.Second // trip → recovery min gap
	FlapWindow                = 15 * time.Minute // re-trip window for doubling
	MaxHold                   = 120 * time.Minute
	marketWideId              = MarketWideID
	defaultInstrumentLimitPct = 5.0   // percent
	accountLossEquityPct      = 5.0   // percent of equity per loss
	volumeSpikeZ              = 4.0   // σ
	optionsIVSpikePct         = 200.0 // % above the 30-day average
	marketWideMovePct         = 20.0  // % aggregate move threshold
	negCacheTTL               = 5 * time.Second
)

// scopeSpec pins the per-scope hold and recovery contract.
type scopeSpec struct {
	hold        time.Duration
	autoRecover bool // probe-window recovery permitted
}

var scopeSpecs = map[string]scopeSpec{
	ScopeInstrument:        {hold: 5 * time.Minute, autoRecover: true},
	ScopeAccount:           {hold: 30 * time.Minute, autoRecover: false},
	ScopeVolumeSpike:       {hold: 10 * time.Minute, autoRecover: true},
	ScopeOptionsVolatility: {hold: 15 * time.Minute, autoRecover: true},
	ScopeMarketWide:        {hold: 0, autoRecover: false}, // manual resume only
}

// ValidBreakerScope vets an operator-supplied scope token (fail closed on
// the unknown — never coerce, mirroring admin.ValidScope).
func ValidBreakerScope(s string) bool {
	_, ok := scopeSpecs[strings.ToUpper(strings.TrimSpace(s))]
	return ok
}

// ---------------------------------------------------------------------------
// State record
// ---------------------------------------------------------------------------

// Breaker is the live record for one (scope, id) pair. Times are UTC.
type Breaker struct {
	Scope      string            `json:"scope"`
	ID         string            `json:"id"`
	State      string            `json:"state"` // CLOSED | OPEN | HALF_OPEN
	EnteredAt  time.Time         `json:"entered_at"`
	HoldUntil  time.Time         `json:"hold_until,omitempty"` // OPEN: earliest HALF_OPEN
	WindowEnd  time.Time         `json:"window_end,omitempty"` // HALF_OPEN: probe window end
	Probes     int               `json:"probes"`               // probes admitted this window
	Reason     string            `json:"reason"`
	Trigger    map[string]string `json:"trigger,omitempty"` // trigger params at trip
	LastTrip   time.Time         `json:"last_trip,omitempty"`
	LastClosed time.Time         `json:"last_closed,omitempty"` // last recovery to CLOSED
	Flaps      int               `json:"flaps"`                 // consecutive post-recovery re-trips
	HoldMS     int64             `json:"hold_ms"`               // effective hold this episode
	Manual     bool              `json:"manual"`                // admin-forced state
	ActorID    int64             `json:"actor_id,omitempty"`
}

func (b *Breaker) key() string { return b.Scope + "|" + b.ID }

// toHash renders the Redis metadata map (state field handled by
// excredis.SetCircuitBreaker).
func (b *Breaker) toHash() excredis.CircuitBreaker {
	md := map[string]string{
		"entered_at_ms": strconv.FormatInt(b.EnteredAt.UnixMilli(), 10),
		"reason":        b.Reason,
		"probes":        strconv.Itoa(b.Probes),
		"hold_ms":       strconv.FormatInt(b.HoldMS, 10),
		"flaps":         strconv.Itoa(b.Flaps),
	}
	if !b.HoldUntil.IsZero() {
		md["hold_until_ms"] = strconv.FormatInt(b.HoldUntil.UnixMilli(), 10)
	}
	if !b.WindowEnd.IsZero() {
		md["window_end_ms"] = strconv.FormatInt(b.WindowEnd.UnixMilli(), 10)
	}
	if !b.LastTrip.IsZero() {
		md["last_trip_ms"] = strconv.FormatInt(b.LastTrip.UnixMilli(), 10)
	}
	if !b.LastClosed.IsZero() {
		md["last_closed_ms"] = strconv.FormatInt(b.LastClosed.UnixMilli(), 10)
	}
	if b.Manual {
		md["manual"] = "1"
	}
	if b.ActorID > 0 {
		md["actor_id"] = strconv.FormatInt(b.ActorID, 10)
	}
	if len(b.Trigger) > 0 {
		if raw, err := json.Marshal(b.Trigger); err == nil {
			md["trigger_json"] = string(raw)
		}
	}
	return excredis.CircuitBreaker{State: b.State, Metadata: md}
}

func msToTime(ms int64) time.Time {
	if ms <= 0 {
		return time.Time{}
	}
	return time.UnixMilli(ms).UTC()
}

func parseMS(m map[string]string, k string) time.Time {
	v, err := strconv.ParseInt(m[k], 10, 64)
	if err != nil {
		return time.Time{}
	}
	return msToTime(v)
}

func parseInt(m map[string]string, k string) int {
	v, _ := strconv.Atoi(m[k])
	return v
}

// fromHash hydrates a Breaker from the Redis record.
func breakerFromHash(scope, id string, cb *excredis.CircuitBreaker) *Breaker {
	if cb == nil {
		return nil
	}
	b := &Breaker{Scope: scope, ID: id, State: cb.State}
	if !ValidBreakerScope(scope) || !validState(cb.State) {
		return nil
	}
	m := cb.Metadata
	b.EnteredAt = parseMS(m, "entered_at_ms")
	b.HoldUntil = parseMS(m, "hold_until_ms")
	b.WindowEnd = parseMS(m, "window_end_ms")
	b.LastTrip = parseMS(m, "last_trip_ms")
	b.LastClosed = parseMS(m, "last_closed_ms")
	b.Probes = parseInt(m, "probes")
	b.Flaps = parseInt(m, "flaps")
	b.HoldMS, _ = strconv.ParseInt(m["hold_ms"], 10, 64)
	b.Reason = m["reason"]
	b.Manual = m["manual"] == "1"
	if v, err := strconv.ParseInt(m["actor_id"], 10, 64); err == nil {
		b.ActorID = v
	}
	if raw := m["trigger_json"]; raw != "" {
		var trig map[string]string
		if err := json.Unmarshal([]byte(raw), &trig); err == nil {
			b.Trigger = trig
		}
	}
	return b
}

func validState(s string) bool {
	return s == excredis.CircuitClosed || s == excredis.CircuitOpen ||
		s == excredis.CircuitHalfOpen
}

// ---------------------------------------------------------------------------
// Seams
// ---------------------------------------------------------------------------

// BreakerStore is the Redis HASH persistence seam. *excredis.Client
// satisfies the get/set pair; scan is wrapped by RedisBreakerStore.
type BreakerStore interface {
	GetCircuitBreaker(ctx context.Context, scope, id string) (*excredis.CircuitBreaker, error)
	SetCircuitBreaker(ctx context.Context, scope, id string, cb excredis.CircuitBreaker) error
	// ScanBreakers returns every persisted breaker record (boot/reconcile).
	ScanBreakers(ctx context.Context) (map[string]*excredis.CircuitBreaker, error)
}

// RedisBreakerStore adapts *excredis.Client: SCAN circuit_breaker:* +
// HGETALL per key (boot-time reconcile path, not the hot loop).
type RedisBreakerStore struct {
	C *excredis.Client
}

func (r RedisBreakerStore) GetCircuitBreaker(ctx context.Context, scope, id string) (*excredis.CircuitBreaker, error) {
	return r.C.GetCircuitBreaker(ctx, scope, id)
}

func (r RedisBreakerStore) SetCircuitBreaker(ctx context.Context, scope, id string, cb excredis.CircuitBreaker) error {
	return r.C.SetCircuitBreaker(ctx, scope, id, cb)
}

// ScanBreakers enumerates every circuit_breaker:{scope}:{id} HASH. Key
// layout is owned by internal/redis/client.go ("circuit_breaker:" prefix);
// the returned map is keyed "SCOPE|id".
func (r RedisBreakerStore) ScanBreakers(ctx context.Context) (map[string]*excredis.CircuitBreaker, error) {
	out := map[string]*excredis.CircuitBreaker{}
	var cursor uint64
	for {
		keys, next, err := r.C.Scan(ctx, cursor, "circuit_breaker:*", 200).Result()
		if err != nil {
			return nil, fmt.Errorf("redis scan circuit breakers: %w", err)
		}
		for _, k := range keys {
			rest := strings.TrimPrefix(k, "circuit_breaker:")
			scope, id, ok := strings.Cut(rest, ":")
			if !ok || scope == "" || id == "" {
				continue
			}
			m, err := r.C.HGetAll(ctx, k).Result()
			if err != nil {
				return nil, fmt.Errorf("redis read circuit breaker %s: %w", k, err)
			}
			cb := &excredis.CircuitBreaker{State: m["state"], Metadata: map[string]string{}}
			for f, v := range m {
				if f != "state" {
					cb.Metadata[f] = v
				}
			}
			out[scope+"|"+id] = cb
		}
		if next == 0 {
			return out, nil
		}
		cursor = next
	}
}

// ---------------------------------------------------------------------------
// Events / telemetry
// ---------------------------------------------------------------------------

// BreakerEvent is one state transition, persisted to
// circuit_breaker_events and streamed to the admin monitor channel.
type BreakerEvent struct {
	Scope     string            `json:"scope"`
	ID        string            `json:"id"`
	FromState string            `json:"from_state"`
	ToState   string            `json:"to_state"`
	Reason    string            `json:"reason,omitempty"`
	Trigger   map[string]string `json:"trigger,omitempty"`
	ActorID   int64             `json:"actor_id,omitempty"` // 0 = automated
	HoldMS    int64             `json:"hold_ms,omitempty"`
	Probes    int               `json:"probes,omitempty"`
	At        time.Time         `json:"at"`
}

// BreakerEventStore persists audit rows (PgBreakerEventStore over
// circuit_breaker_events). A nil store skips durable audit — the
// transition itself is never blocked by telemetry (the trip must land).
type BreakerEventStore interface {
	InsertBreakerEvent(ctx context.Context, ev BreakerEvent) error
}

// Publisher is the WS admin-monitor fanout seam — *ws.Server satisfies
// it; nil disables.
type Publisher interface {
	Publish(channel string, data any)
}

// FlapAlerter pages Risk Management when the flapping penalty doubles a
// hold. Production binds the NATS ops alerter; nil → logged only.
type FlapAlerter func(ctx context.Context, severity, code, summary string) error

// IVSource is the Phase-22 implied-volatility seam. CurrentIV returns the
// live IV and its trailing 30-day average for the symbol; implementations
// must fail closed (non-nil error) when either leg is unavailable.
type IVSource interface {
	CurrentIV(ctx context.Context, symbol string) (iv, avg30d decimal.Decimal, err error)
}

// NullIVSource is the documented dev/null feed: options are Phase-22, so
// no IV surface exists yet. It reports "no observation" — the
// OPTIONS_VOLATILITY breaker machinery is complete and binds this seam
// when the options surface lands.
type NullIVSource struct{}

// CurrentIV always reports unavailability (fail closed — never fabricate).
func (NullIVSource) CurrentIV(_ context.Context, symbol string) (decimal.Decimal, decimal.Decimal, error) {
	return decimal.Zero, decimal.Zero,
		excerrors.New("PRICE_ORACLE_UNAVAILABLE",
			"no IV feed for "+symbol+" (options land Phase-22)")
}

// PriceLimitLookup supplies the per-instrument price-move limit
// (instrument_price_limit, spec §2.6) as a percentage. nil → the 5%
// default; a missing per-instrument row → default.
type PriceLimitLookup func(ctx context.Context, symbol string) (decimal.Decimal, bool)

// ---------------------------------------------------------------------------
// Metrics
// ---------------------------------------------------------------------------

// BreakerMetrics carries the two spec §2.6 metric families. Names are the
// spec-pinned literals (circuit_breaker_state / circuit_breaker_transitions_total).
type BreakerMetrics struct {
	state       *observability.GaugeVec
	transitions *observability.CounterVec
}

// NewBreakerMetrics registers the families on reg; nil reg → nil-safe
// no-op metrics.
func NewBreakerMetrics(reg *observability.Registry) *BreakerMetrics {
	if reg == nil {
		return &BreakerMetrics{}
	}
	return &BreakerMetrics{
		state: reg.Gauge("circuit_breaker_state",
			"Five-tier circuit breaker state (0=CLOSED 1=HALF_OPEN 2=OPEN), spec §2.6"),
		transitions: reg.Counter("circuit_breaker_transitions_total",
			"Circuit breaker state transitions, spec §2.6"),
	}
}

var stateValue = map[string]float64{
	excredis.CircuitClosed:   0,
	excredis.CircuitHalfOpen: 1,
	excredis.CircuitOpen:     2,
}

func (m *BreakerMetrics) setState(scope, id, state string) {
	if m == nil || m.state == nil {
		return
	}
	m.state.With("scope", scope, "id", id).Set(stateValue[state])
}

func (m *BreakerMetrics) transition(scope, id string) {
	if m == nil || m.transitions == nil {
		return
	}
	m.transitions.With("scope", scope, "id", id).Inc()
}

// ---------------------------------------------------------------------------
// Service
// ---------------------------------------------------------------------------

type pricePoint struct {
	t time.Time
	p decimal.Decimal
}

type lossEvent struct {
	t   time.Time
	pct float64 // % of equity lost
}

// CircuitBreakerService is the five-tier breaker authority.
type CircuitBreakerService struct {
	store  BreakerStore
	events BreakerEventStore
	met    *BreakerMetrics
	pub    Publisher
	alert  FlapAlerter
	iv     IVSource
	limits PriceLimitLookup
	now    func() time.Time
	logf   func(format string, args ...any)

	mu       sync.Mutex
	breakers map[string]*Breaker                  // "SCOPE|id" → live record
	neg      map[string]time.Time                 // keys verified absent in Redis
	prices   map[string][]pricePoint              // symbol → rolling 60s window
	vols     map[string]map[int64]decimal.Decimal // symbol → minute bucket
	losses   map[int64][]lossEvent                // account → rolling 5min losses
	wide     map[string]float64                   // symbol → latest 60s move pct (MARKET_WIDE feed)
}

// BreakerDeps wires the service. Store is required — admission fails
// closed without it. Everything else degrades honestly.
type BreakerDeps struct {
	Store      BreakerStore
	Events     BreakerEventStore
	Metrics    *BreakerMetrics
	Publisher  Publisher
	Alerter    FlapAlerter
	IV         IVSource
	PriceLimit PriceLimitLookup
	Now        func() time.Time
	Logf       func(format string, args ...any)
}

// NewCircuitBreakerService builds the service. Call Load then Run.
func NewCircuitBreakerService(d BreakerDeps) (*CircuitBreakerService, error) {
	if d.Store == nil {
		return nil, fmt.Errorf("circuit-breaker: store is nil")
	}
	s := &CircuitBreakerService{
		store: d.Store, events: d.Events, met: d.Metrics,
		pub: d.Publisher, alert: d.Alerter, iv: d.IV, limits: d.PriceLimit,
		now: d.Now, logf: d.Logf,
		breakers: map[string]*Breaker{},
		neg:      map[string]time.Time{},
		prices:   map[string][]pricePoint{},
		vols:     map[string]map[int64]decimal.Decimal{},
		losses:   map[int64][]lossEvent{},
		wide:     map[string]float64{},
	}
	if s.now == nil {
		s.now = func() time.Time { return time.Now().UTC() }
	}
	if s.logf == nil {
		s.logf = func(string, ...any) {}
	}
	if s.iv == nil {
		s.iv = NullIVSource{}
	}
	return s, nil
}

// WithPublisher binds the WS admin-monitor publisher post-construction —
// cmd/gateway builds ws.Server after the order pipeline, so the breaker
// service is constructed first and the fanout attaches here.
func (s *CircuitBreakerService) WithPublisher(p Publisher) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pub = p
}

// WithAlerter binds the flapping-penalty pager post-construction (the
// NATS ops alerter is built after the breaker service in cmd/gateway).
func (s *CircuitBreakerService) WithAlerter(a FlapAlerter) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.alert = a
}

// ---------------------------------------------------------------------------
// Persistence + telemetry
// ---------------------------------------------------------------------------

// persist mirrors the breaker into Redis; a write failure is returned to
// the caller (transitions are in-memory authoritative for this process,
// but a failed persist means other replicas/sweeps won't see it — the
// caller surfaces it).
func (s *CircuitBreakerService) persist(ctx context.Context, b *Breaker) error {
	return s.store.SetCircuitBreaker(ctx, b.Scope, b.ID, b.toHash())
}

// emit writes the audit row (best-effort — a PG outage must not mask or
// veto a safety transition), bumps metrics and publishes to the admin
// monitor channel. Callers hold s.mu.
func (s *CircuitBreakerService) emit(ctx context.Context, ev BreakerEvent, b *Breaker) {
	if s.events != nil {
		if err := s.events.InsertBreakerEvent(ctx, ev); err != nil {
			s.logf("circuit-breaker: event audit failed (%s:%s %s→%s): %v",
				ev.Scope, ev.ID, ev.FromState, ev.ToState, err)
		}
	}
	s.met.transition(ev.Scope, ev.ID)
	s.met.setState(ev.Scope, ev.ID, ev.ToState)
	if s.pub != nil {
		s.pub.Publish(BreakerAdminChannel, ev)
	}
}

// transitionLocked applies a state change under s.mu: fields updated,
// Redis persisted, event emitted. Returns the persist error (the
// in-memory state is already committed — callers decide whether to
// surface it; the sweeper logs and retries on next tick).
func (s *CircuitBreakerService) transitionLocked(ctx context.Context, b *Breaker,
	to, reason string, trig map[string]string, actorID int64) error {

	from := b.State
	now := s.now()
	b.State = to
	b.EnteredAt = now
	b.Reason = reason
	b.Trigger = trig
	b.ActorID = actorID
	switch to {
	case excredis.CircuitOpen:
		b.LastTrip = now
		b.Probes = 0
		b.WindowEnd = time.Time{}
		b.HoldUntil = now.Add(time.Duration(b.HoldMS) * time.Millisecond)
	case excredis.CircuitHalfOpen:
		b.Probes = 0
		b.WindowEnd = now.Add(ProbeWindow)
	case excredis.CircuitClosed:
		b.LastClosed = now
		b.Probes = 0
		b.WindowEnd = time.Time{}
		b.HoldUntil = time.Time{}
	}
	perr := s.persist(ctx, b)
	s.emit(ctx, BreakerEvent{
		Scope: b.Scope, ID: b.ID, FromState: from, ToState: to,
		Reason: reason, Trigger: trig, ActorID: actorID,
		HoldMS: b.HoldMS, Probes: b.Probes, At: now,
	}, b)
	if perr != nil {
		return excerrors.Wrap(CodeCircuitBreakerOpen,
			fmt.Sprintf("persist breaker %s:%s", b.Scope, b.ID), perr)
	}
	return nil
}

// getLocked returns the live record for (scope,id), hydrating from Redis
// on first touch. The 5s negative cache bounds per-admission Redis reads
// for never-tripped keys while still converging on externally-written
// records (a second gateway replica's manual trip) within one TTL.
// Callers hold s.mu.
func (s *CircuitBreakerService) getLocked(ctx context.Context, scope, id string) (*Breaker, error) {
	key := scope + "|" + id
	if b, ok := s.breakers[key]; ok {
		return b, nil
	}
	if t, ok := s.neg[key]; ok && s.now().Before(t.Add(negCacheTTL)) {
		return nil, nil
	}
	cb, err := s.store.GetCircuitBreaker(ctx, scope, id)
	if err != nil {
		return nil, err
	}
	if cb == nil {
		s.neg[key] = s.now()
		return nil, nil
	}
	delete(s.neg, key)
	b := breakerFromHash(scope, id, cb)
	if b == nil {
		return nil, excerrors.New(CodeCircuitBreakerOpen,
			fmt.Sprintf("corrupt breaker record %s:%s state=%q", scope, id, cb.State))
	}
	s.breakers[key] = b
	s.met.setState(scope, id, b.State)
	return b, nil
}

// Load hydrates every persisted breaker (boot/reconcile path).
func (s *CircuitBreakerService) Load(ctx context.Context) error {
	all, err := s.store.ScanBreakers(ctx)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for key, cb := range all {
		scope, id, ok := strings.Cut(key, "|") // ScanBreakers keys are "SCOPE|id"
		if !ok {
			continue
		}
		if b := breakerFromHash(scope, id, cb); b != nil {
			s.breakers[b.key()] = b
			s.met.setState(scope, id, b.State)
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// Admission gate — orders.BreakerGate seam
// ---------------------------------------------------------------------------

// admissionTargets enumerates the breakers consulted for one order, in
// precedence order: market-wide first, then account, then the
// symbol-scoped tiers.
func admissionTargets(accountID int64, symbol string) []struct{ scope, id string } {
	out := []struct{ scope, id string }{
		{ScopeMarketWide, marketWideId},
	}
	if accountID > 0 {
		out = append(out, struct{ scope, id string }{ScopeAccount, strconv.FormatInt(accountID, 10)})
	}
	if symbol != "" {
		sym := config.CanonicalSymbol(symbol)
		out = append(out,
			struct{ scope, id string }{ScopeInstrument, sym},
			struct{ scope, id string }{ScopeVolumeSpike, sym},
			struct{ scope, id string }{ScopeOptionsVolatility, sym},
		)
	}
	return out
}

// AdmitOrder is the order-admission gate. OPEN → CIRCUIT_BREAKER_OPEN
// rejection; HALF_OPEN → admits + counts the probe (the 10th probe inside
// a live window closes the breaker — all probes admitted without the
// trigger re-firing); CLOSED/absent → admit. Store errors and missing
// store fail closed with CIRCUIT_BREAKER_OPEN.
func (s *CircuitBreakerService) AdmitOrder(ctx context.Context, accountID int64, symbol string) error {
	if s == nil || s.store == nil {
		return excerrors.New(CodeCircuitBreakerOpen,
			"circuit-breaker store unavailable — new orders rejected (fail closed)")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	if err := s.advanceLocked(ctx, now); err != nil {
		s.logf("circuit-breaker: advance failed: %v", err)
	}
	for _, t := range admissionTargets(accountID, symbol) {
		b, err := s.getLocked(ctx, t.scope, t.id)
		if err != nil {
			return excerrors.Wrap(CodeCircuitBreakerOpen,
				fmt.Sprintf("breaker lookup %s:%s failed — fail closed", t.scope, t.id), err)
		}
		if b == nil {
			continue
		}
		switch b.State {
		case excredis.CircuitOpen:
			return excerrors.New(CodeCircuitBreakerOpen,
				fmt.Sprintf("%s breaker open for %s (reason: %s)", b.Scope, b.ID, b.Reason))
		case excredis.CircuitHalfOpen:
			if !now.Before(b.WindowEnd) || b.Probes >= ProbeCount {
				return excerrors.New(CodeCircuitBreakerOpen,
					fmt.Sprintf("%s breaker probing window closed for %s", b.Scope, b.ID))
			}
			b.Probes++
			perr := s.persist(ctx, b)
			if perr != nil {
				return excerrors.Wrap(CodeCircuitBreakerOpen,
					"probe count persist failed — fail closed", perr)
			}
			if b.Probes >= ProbeCount {
				// All 10 probes admitted without a re-trip → CLOSED.
				if err := s.transitionLocked(ctx, b, excredis.CircuitClosed,
					"probe window complete: 10/10 probes admitted", nil, 0); err != nil {
					s.logf("circuit-breaker: close after probes failed: %v", err)
				}
			}
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// State advancement (sweeper + lazy)
// ---------------------------------------------------------------------------

// advanceLocked moves expired states: OPEN past hold → HALF_OPEN for
// auto-recovering scopes once the 60s post-trip cooldown has elapsed;
// HALF_OPEN past its window → CLOSED (10/10 probes) or back to OPEN
// (incomplete window — same episode, no flap penalty). Callers hold s.mu.
func (s *CircuitBreakerService) advanceLocked(ctx context.Context, now time.Time) error {
	var firstErr error
	for _, b := range s.breakers {
		switch b.State {
		case excredis.CircuitOpen:
			spec := scopeSpecs[b.Scope]
			if !spec.autoRecover || b.HoldUntil.IsZero() || now.Before(b.HoldUntil) {
				continue
			}
			// 60s cooldown between trip and recovery — holds are ≥5min so
			// this only binds edge cases (e.g. a zero-hold manual episode).
			if now.Sub(b.LastTrip) < TransitionCooldown {
				continue
			}
			if err := s.transitionLocked(ctx, b, excredis.CircuitHalfOpen,
				"hold expired — probing", nil, 0); err != nil && firstErr == nil {
				firstErr = err
			}
		case excredis.CircuitHalfOpen:
			if now.Before(b.WindowEnd) {
				continue
			}
			if b.Probes >= ProbeCount {
				if err := s.transitionLocked(ctx, b, excredis.CircuitClosed,
					"probe window complete", nil, 0); err != nil && firstErr == nil {
					firstErr = err
				}
				continue
			}
			// Incomplete probe window → re-OPEN on the same episode hold
			// (no doubling: recovery never reached CLOSED).
			b.HoldMS = effectiveHoldMS(scopeSpecs[b.Scope].hold, b.Flaps)
			if err := s.transitionLocked(ctx, b, excredis.CircuitOpen,
				fmt.Sprintf("probe window expired with %d/%d probes — re-opened",
					b.Probes, ProbeCount), nil, 0); err != nil && firstErr == nil {
				firstErr = err
			}
		}
	}
	return firstErr
}

// Run is the background sweeper: every tick it advances expired holds and
// probe windows (Task 13.3.9). interval <= 0 defaults to 1s — probe
// windows are 30s so sub-second granularity keeps the recovery boundary
// tight without hammering Redis.
func (s *CircuitBreakerService) Run(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		interval = time.Second
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.mu.Lock()
			if err := s.advanceLocked(ctx, s.now()); err != nil {
				s.logf("circuit-breaker: sweep advance failed: %v", err)
			}
			s.mu.Unlock()
		}
	}
}

// effectiveHoldMS applies the flapping multiplier: base hold doubled per
// consecutive post-recovery re-trip, capped at the 120-minute spec bound.
func effectiveHoldMS(base time.Duration, flaps int) int64 {
	ms := base.Milliseconds()
	for i := 0; i < flaps && ms < MaxHold.Milliseconds(); i++ {
		ms *= 2
		if ms > MaxHold.Milliseconds() {
			ms = MaxHold.Milliseconds()
		}
	}
	return ms
}

// ---------------------------------------------------------------------------
// Trips — automated + manual
// ---------------------------------------------------------------------------

// tripLocked transitions (or refreshes) a breaker to OPEN. Automated
// trips evaluate the flapping penalty: a re-trip within 15min of the
// last recovery to CLOSED doubles the hold (≤120min) and pages Risk
// Management. Manual trips (manual=true) skip the penalty — operator
// intent is not a flap. Callers hold s.mu.
func (s *CircuitBreakerService) tripLocked(ctx context.Context, b *Breaker,
	reason string, trig map[string]string, manual bool, actorID int64) error {

	spec := scopeSpecs[b.Scope]
	now := s.now()
	if b.State == excredis.CircuitOpen {
		return nil // already open — idempotent
	}
	flap := false
	if !manual && !b.LastClosed.IsZero() && now.Sub(b.LastClosed) < FlapWindow {
		b.Flaps++
		flap = true
	} else if b.State == excredis.CircuitClosed {
		// Recovered cleanly more than 15min ago (or never tripped):
		// reset the flap ladder.
		b.Flaps = 0
	}
	b.Manual = manual
	b.HoldMS = effectiveHoldMS(spec.hold, b.Flaps)
	trig = mergeTrigger(trig, "flap_count", strconv.Itoa(b.Flaps))
	if err := s.transitionLocked(ctx, b, excredis.CircuitOpen, reason, trig, actorID); err != nil {
		return err
	}
	if flap {
		summary := fmt.Sprintf(
			"circuit breaker %s:%s re-tripped within %s of recovery — hold doubled to %dms",
			b.Scope, b.ID, FlapWindow, b.HoldMS)
		s.logf("circuit-breaker: %s", summary)
		if s.alert != nil {
			if err := s.alert(ctx, "P1", "CIRCUIT_BREAKER_FLAPPING", summary); err != nil {
				s.logf("circuit-breaker: flap alert dispatch failed: %v", err)
			}
		}
	}
	return nil
}

func mergeTrigger(trig map[string]string, k, v string) map[string]string {
	if trig == nil {
		trig = map[string]string{}
	}
	trig[k] = v
	return trig
}

// tripByFeed is the automated-trip entry: hydrate-or-create the breaker
// and apply the trip. Feeds call this under s.mu.
func (s *CircuitBreakerService) tripByFeed(ctx context.Context, scope, id,
	reason string, trig map[string]string) error {
	b, err := s.getLocked(ctx, scope, id)
	if err != nil {
		return err
	}
	if b == nil {
		b = &Breaker{Scope: scope, ID: id, State: excredis.CircuitClosed,
			EnteredAt: s.now()}
		s.breakers[b.key()] = b
	}
	return s.tripLocked(ctx, b, reason, trig, false, 0)
}

// ---------------------------------------------------------------------------
// Admin operations (Task 13.3.1 items 8–9)
// ---------------------------------------------------------------------------

// ManualTrip forces (scope,id) OPEN — the Risk Manager manual override
// behind POST /api/v1/admin/circuit-breaker/{symbol}. Hold applies per
// scope (MARKET_WIDE stays open until manual resume).
func (s *CircuitBreakerService) ManualTrip(ctx context.Context, scope, id,
	reason string, actorID int64) error {
	scope = strings.ToUpper(strings.TrimSpace(scope))
	if !ValidBreakerScope(scope) {
		return excerrors.New("INVALID_REQUEST",
			"scope must be one of INSTRUMENT|ACCOUNT|VOLUME_SPIKE|OPTIONS_VOLATILITY|MARKET_WIDE")
	}
	id = normalizeBreakerID(scope, id)
	if id == "" && scope != ScopeMarketWide {
		return excerrors.New("INVALID_REQUEST", "target id required for scope "+scope)
	}
	if scope == ScopeMarketWide {
		id = marketWideId
	}
	reason = strings.TrimSpace(reason)
	if reason == "" {
		reason = "manual trip"
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	b, err := s.getLocked(ctx, scope, id)
	if err != nil {
		return excerrors.Wrap(CodeCircuitBreakerOpen, "breaker lookup failed", err)
	}
	if b == nil {
		b = &Breaker{Scope: scope, ID: id, State: excredis.CircuitClosed,
			EnteredAt: s.now()}
		s.breakers[b.key()] = b
	}
	return s.tripLocked(ctx, b, reason, map[string]string{"manual": "1"}, true, actorID)
}

// ManualReset forces (scope,id) CLOSED — the dual-controlled reset behind
// POST /api/v1/admin/circuit-breaker/{symbol}/reset (the four-eyes gate
// lives in the DualControlService queue; the executor calls this on
// approval). Works from OPEN or HALF_OPEN; manual recovery is the spec
// path for ACCOUNT and MARKET_WIDE.
func (s *CircuitBreakerService) ManualReset(ctx context.Context, scope, id string,
	actorID int64) error {
	scope = strings.ToUpper(strings.TrimSpace(scope))
	if !ValidBreakerScope(scope) {
		return excerrors.New("INVALID_REQUEST",
			"scope must be one of INSTRUMENT|ACCOUNT|VOLUME_SPIKE|OPTIONS_VOLATILITY|MARKET_WIDE")
	}
	if scope == ScopeMarketWide {
		id = marketWideId
	} else {
		id = normalizeBreakerID(scope, id)
		if id == "" {
			return excerrors.New("INVALID_REQUEST", "target id required for scope "+scope)
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	b, err := s.getLocked(ctx, scope, id)
	if err != nil {
		return excerrors.Wrap(CodeCircuitBreakerOpen, "breaker lookup failed", err)
	}
	if b == nil || b.State == excredis.CircuitClosed {
		return excerrors.New("NOT_FOUND",
			"no open circuit breaker for "+scope+":"+id)
	}
	b.Manual = true
	return s.transitionLocked(ctx, b, excredis.CircuitClosed,
		"manual reset (dual-control approved)",
		map[string]string{"manual": "1"}, actorID)
}

func normalizeBreakerID(scope, id string) string {
	switch scope {
	case ScopeInstrument, ScopeVolumeSpike, ScopeOptionsVolatility:
		return config.CanonicalSymbol(id)
	}
	return strings.TrimSpace(id)
}

// Status returns a copy of every live breaker record (admin status
// surface / ops monitor).
func (s *CircuitBreakerService) Status() []Breaker {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Breaker, 0, len(s.breakers))
	for _, b := range s.breakers {
		out = append(out, *b)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].key() < out[j].key() })
	return out
}

// ---------------------------------------------------------------------------
// Trigger feeds
// ---------------------------------------------------------------------------

// ObservePrice feeds one price observation for the INSTRUMENT and
// MARKET_WIDE scopes. The window measure is peak-to-trough movement over
// the trailing 60s as a percent of the window low — a move in either
// direction counts ("price move > limit", spec §2.6). Feeding while
// HALF_OPEN re-evaluates: a still-breaching move re-opens immediately
// (failed probe, same episode).
func (s *CircuitBreakerService) ObservePrice(ctx context.Context, symbol string, price decimal.Decimal) {
	sym := config.CanonicalSymbol(symbol)
	if sym == "" || !price.IsPositive() {
		return
	}
	now := s.now()
	s.mu.Lock()
	defer s.mu.Unlock()
	w := append(s.prices[sym], pricePoint{t: now, p: price})
	cut := now.Add(-InstrumentMoveWindow)
	keep := w[:0]
	for _, p := range w {
		if !p.t.Before(cut) {
			keep = append(keep, p)
		}
	}
	s.prices[sym] = keep
	move := windowMovePct(keep)
	s.wide[sym] = move

	limit := defaultInstrumentLimitPct
	if s.limits != nil {
		if v, ok := s.limits(ctx, sym); ok && v.IsPositive() {
			f, _ := v.Float64()
			if f > 0 {
				limit = f
			}
		}
	}
	if move > limit {
		if err := s.tripByFeed(ctx, ScopeInstrument, sym,
			fmt.Sprintf("price move %.2f%% exceeds limit %.2f%% within %s",
				move, limit, InstrumentMoveWindow),
			map[string]string{
				"move_pct":   strconv.FormatFloat(move, 'f', 4, 64),
				"limit_pct":  strconv.FormatFloat(limit, 'f', 4, 64),
				"window":     InstrumentMoveWindow.String(),
				"last_price": price.String(),
			}); err != nil {
			s.logf("circuit-breaker: instrument trip failed %s: %v", sym, err)
		}
	}

	// MARKET_WIDE aggregate: > 20% move on > 2 instruments.
	wide := 0
	var wideSyms []string
	for s2, m := range s.wide {
		if m > marketWideMovePct {
			wide++
			wideSyms = append(wideSyms, s2)
		}
	}
	if wide >= MarketWideMinInstruments {
		sort.Strings(wideSyms)
		if err := s.tripByFeed(ctx, ScopeMarketWide, marketWideId,
			fmt.Sprintf("aggregate volatility >%.0f%% on %d instruments (%s)",
				marketWideMovePct, wide, strings.Join(wideSyms, ",")),
			map[string]string{
				"instruments": strings.Join(wideSyms, ","),
				"count":       strconv.Itoa(wide),
				"move_pct_gt": strconv.FormatFloat(marketWideMovePct, 'f', 2, 64),
			}); err != nil {
			s.logf("circuit-breaker: market-wide trip failed: %v", err)
		}
	}
}

// windowMovePct is peak-to-trough over the window as a % of the trough.
func windowMovePct(w []pricePoint) float64 {
	if len(w) < 2 {
		return 0
	}
	lo, hi := w[0].p, w[0].p
	for _, p := range w[1:] {
		if p.p.LessThan(lo) {
			lo = p.p
		}
		if p.p.GreaterThan(hi) {
			hi = p.p
		}
	}
	if !lo.IsPositive() {
		return 0
	}
	f, _ := hi.Sub(lo).Div(lo).Mul(decimal.NewFromInt(100)).Float64()
	return f
}

// ObserveTrade feeds traded notional into the per-minute volume buckets
// the VOLUME_SPIKE z-score runs on. Buckets are in-process — the spec
// permits Redis time-bucketed counters, but the gateway is the single
// evaluator so memory holds the trailing hour; a restart rebuilds the
// baseline from subsequent fills (never fabricates history).
func (s *CircuitBreakerService) ObserveTrade(ctx context.Context, symbol string, notional decimal.Decimal) {
	sym := config.CanonicalSymbol(symbol)
	if sym == "" || !notional.IsPositive() {
		return
	}
	now := s.now()
	s.mu.Lock()
	defer s.mu.Unlock()
	m := s.vols[sym]
	if m == nil {
		m = map[int64]decimal.Decimal{}
		s.vols[sym] = m
	}
	bucket := now.Unix() / int64(VolumeBucket.Seconds())
	m[bucket] = m[bucket].Add(notional)
	// Prune buckets older than the baseline window.
	for b := range m {
		if bucket-b > VolumeBaselineBuckets {
			delete(m, b)
		}
	}
	z, cur, mean, sd, n := volumeZScore(m, bucket)
	if n < VolumeBaselineMinBuckets {
		return // baseline too thin — honest "insufficient data", no trip
	}
	if z >= volumeSpikeZ {
		if err := s.tripByFeed(ctx, ScopeVolumeSpike, sym,
			fmt.Sprintf("1-min volume z-score %.2f >= %.1f (cur=%s mean=%s sd=%s)",
				z, volumeSpikeZ, cur, mean, sd),
			map[string]string{
				"z_score":        strconv.FormatFloat(z, 'f', 4, 64),
				"z_threshold":    strconv.FormatFloat(volumeSpikeZ, 'f', 2, 64),
				"current_volume": cur.String(),
				"baseline_mean":  mean.String(),
				"baseline_sd":    sd.String(),
			}); err != nil {
			s.logf("circuit-breaker: volume-spike trip failed %s: %v", sym, err)
		}
	}
}

// volumeZScore computes the current bucket's z-score against the trailing
// VolumeBaselineBuckets (excluding the live bucket). Zero-variance
// baselines are handled explicitly: sd==0 with cur>mean is an infinite
// deviation (returns z=+Inf sentinel 1e9); sd==0 with cur<=mean is flat.
func volumeZScore(m map[int64]decimal.Decimal, curBucket int64) (
	z float64, cur, mean, sd decimal.Decimal, n int) {

	cur = m[curBucket]
	var sum, sq decimal.Decimal
	for b, v := range m {
		if b == curBucket || curBucket-b > VolumeBaselineBuckets || b > curBucket {
			continue
		}
		sum = sum.Add(v)
		sq = sq.Add(v.Mul(v))
		n++
	}
	if n == 0 {
		return 0, cur, decimal.Zero, decimal.Zero, 0
	}
	mean = sum.Div(decimal.NewFromInt(int64(n)))
	// population variance = E[x²] − E[x]², floored at 0.
	variance := sq.Div(decimal.NewFromInt(int64(n))).Sub(mean.Mul(mean))
	if variance.IsNegative() {
		variance = decimal.Zero
	}
	sdf, _ := variance.Float64()
	sd = decimal.NewFromFloat(math.Sqrt(sdf))
	if !sd.IsPositive() {
		if cur.GreaterThan(mean) {
			return 1e9, cur, mean, sd, n
		}
		return 0, cur, mean, sd, n
	}
	zf, _ := cur.Sub(mean).Div(sd).Float64()
	return zf, cur, mean, sd, n
}

// ObserveAccountLoss feeds one realized-loss event: pct is the loss as a
// percentage of the account's equity at loss time. 3+ events > 5% within
// 5min trips the ACCOUNT breaker (id = decimal account id). Feed source:
// the realized-P&L pipeline (Phase-19 PositionService.RealizedPnLDelta /
// liquidation engine) — the record path is complete; production wiring
// binds the P&L event publisher.
func (s *CircuitBreakerService) ObserveAccountLoss(ctx context.Context,
	accountID int64, lossPctOfEquity decimal.Decimal) {

	if accountID <= 0 || !lossPctOfEquity.IsPositive() {
		return
	}
	pct, _ := lossPctOfEquity.Float64()
	now := s.now()
	s.mu.Lock()
	defer s.mu.Unlock()
	id := strconv.FormatInt(accountID, 10)
	evs := append(s.losses[accountID], lossEvent{t: now, pct: pct})
	cut := now.Add(-AccountLossWindow)
	keep := evs[:0]
	rapid := 0
	for _, e := range evs {
		if !e.t.Before(cut) {
			keep = append(keep, e)
			if e.pct > accountLossEquityPct {
				rapid++
			}
		}
	}
	s.losses[accountID] = keep
	if rapid >= AccountLossMinCount {
		if err := s.tripByFeed(ctx, ScopeAccount, id,
			fmt.Sprintf("%d rapid losses >%.0f%% of equity within %s",
				rapid, accountLossEquityPct, AccountLossWindow),
			map[string]string{
				"loss_count":    strconv.Itoa(rapid),
				"min_count":     strconv.Itoa(AccountLossMinCount),
				"loss_pct_gt":   strconv.FormatFloat(accountLossEquityPct, 'f', 2, 64),
				"window":        AccountLossWindow.String(),
				"last_loss_pct": strconv.FormatFloat(pct, 'f', 4, 64),
			}); err != nil {
			s.logf("circuit-breaker: account trip failed %s: %v", id, err)
		}
	}
}

// ObserveIV evaluates one implied-volatility observation against its
// trailing 30-day average. Spec trigger: "IV spike > 200% vs 30-day
// average" — implemented as iv > avg × (1 + 200/100) = 3×avg (a >200%
// increase over the average); callers feeding ratio-style sources must
// pre-normalize. Non-positive iv/avg are ignored (no fabricated trips).
func (s *CircuitBreakerService) ObserveIV(ctx context.Context, symbol string,
	iv, avg30d decimal.Decimal) {

	sym := config.CanonicalSymbol(symbol)
	if sym == "" || !iv.IsPositive() || !avg30d.IsPositive() {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	spikePct, _ := iv.Sub(avg30d).Div(avg30d).Mul(decimal.NewFromInt(100)).Float64()
	if spikePct > optionsIVSpikePct {
		if err := s.tripByFeed(ctx, ScopeOptionsVolatility, sym,
			fmt.Sprintf("IV spike %.1f%% exceeds %.0f%% vs 30-day avg (iv=%s avg=%s)",
				spikePct, optionsIVSpikePct, iv, avg30d),
			map[string]string{
				"spike_pct":    strconv.FormatFloat(spikePct, 'f', 4, 64),
				"spike_pct_gt": strconv.FormatFloat(optionsIVSpikePct, 'f', 2, 64),
				"iv":           iv.String(),
				"iv_avg_30d":   avg30d.String(),
			}); err != nil {
			s.logf("circuit-breaker: options-volatility trip failed %s: %v", sym, err)
		}
	}
}

// PollIV evaluates the IVSource for one symbol — the production feed loop
// calls this per option underlying once Phase-22 instruments exist; a
// source error is logged (feed failure is not a trip).
func (s *CircuitBreakerService) PollIV(ctx context.Context, symbol string) {
	iv, avg, err := s.iv.CurrentIV(ctx, config.CanonicalSymbol(symbol))
	if err != nil {
		s.logf("circuit-breaker: IV source unavailable for %s: %v", symbol, err)
		return
	}
	s.ObserveIV(ctx, symbol, iv, avg)
}
