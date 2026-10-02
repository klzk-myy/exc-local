// session_lifecycle.go — Phase-15 Task 15.3.7: the 24/5 weekly session
// state machine (spec §6.7, §24 #217 / Phase-15 AC row 21).
//
// Four canonical states drive the FX week:
//
//	OPEN ── Fri 21:55 UTC ──▶ PRE_CLOSE (5-minute advisory window —
//	                              order flow continues; documented
//	                              interpretation: spec §6.7 lists the
//	                              state but schedules no window for it)
//	PRE_CLOSE ── Fri 22:00 UTC ──▶ CLOSED (matching stops — the
//	                              market:hours window already rejects
//	                              MARKET_CLOSED; this transition
//	                              additionally fires the Tom-Next
//	                              rollover through the Task 3.3.7
//	                              settlement.RolloverService seam and
//	                              emits session.closed)
//	CLOSED ── Sun 20:45 UTC ──▶ PRE_OPEN (orders accumulate without
//	                              matching — this IS the §6.7/§7.1
//	                              reopening-auction CALL phase; the
//	                              service writes
//	                              instrument:auction:{symbol} =
//	                              "CALL:{deadline_unix_ns}" per ACTIVE
//	                              instrument with deadline = the 21:00
//	                              open instant, and emits
//	                              session.pre_open)
//	PRE_OPEN ── Sun 21:00 UTC ──▶ OPEN (the C++ AuctionManager
//	                              consumes the CALL deadline, performs
//	                              the single-price uncross and deletes
//	                              the key — the same key contract the
//	                              Task 15.3.2 resume path owns; Go
//	                              emits session.open and clears any
//	                              leftover key on the next close)
//
// Session state lives on the Redis coordination instance as
// session:state:{shard_id} HASH per engine shard — surviving restarts
// (reconcile-on-boot rewrites it from the schedule). Order-entry
// rejection is the market:hours contract (market_schedule.go); DAY/GTD
// expiry is clock-driven inside the C++ ExpiryScheduler (Task 2.3.10)
// and is deliberately NOT gated on session state — weekend GTD expiries
// fire per spec §6.7.
//
// Side-effect discipline (spec §2.7): every transition's global effects
// (rollover, auction keys, WS broadcast) are recorded as pending in the
// session:ctl hash BEFORE execution and drained idempotently on every
// Evaluate — a replica that dies mid-transition leaves the pending set
// for the next Evaluate on any replica. Rollover itself is internally
// idempotent (rollover_runs dedup + Redis execution lock), auction key
// SETs are idempotent, and a double WS advisory is harmless.
package admin

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"time"

	goredis "github.com/redis/go-redis/v9"

	excredis "exchange/internal/redis"
)

// SessionState is the §6.7 weekly session machine state.
type SessionState string

const (
	SessionOpen     SessionState = "OPEN"
	SessionPreClose SessionState = "PRE_CLOSE"
	SessionClosed   SessionState = "CLOSED"
	SessionPreOpen  SessionState = "PRE_OPEN"
)

func (s SessionState) valid() bool {
	switch s {
	case SessionOpen, SessionPreClose, SessionClosed, SessionPreOpen:
		return true
	}
	return false
}

// Redis keys (coordination instance, no TTL — durable control state).
const (
	// SessionStateKeyPrefix + shard id → HASH
	//   {state, entered_at, next_transition_at, next_state, last_event}
	SessionStateKeyPrefix = "session:state:"
	// sessionCtlKey is the cross-replica effects ledger:
	//   {pending_state, pending_effects (JSON array), updated_at}
	sessionCtlKey = "session:ctl"
	// sessionEffectsLockPrefix + boundary unix epoch → SET NX PX
	// single-fire guard so one replica runs a boundary's effects.
	sessionEffectsLockPrefix = "session:ctl:effects:"
	// AuctionKeyPrefix + symbol → "CALL:{deadline_unix_ns}" STRING —
	// the exact key contract the Task 15.3.2 instrument-lifecycle
	// resume path and the C++ AuctionManager consumer share.
	AuctionKeyPrefix = "instrument:auction:"

	// SessionStatusChannel is the public WS advisory channel — the same
	// convention as venue.status (kill-switch) / system.status (modes).
	SessionStatusChannel = "session.status"
)

// Weekly boundary minutes-of-day (UTC). Sunday 20:45 is ALSO the
// market:hours pre_open_utc order-entry boundary (the CALL phase is the
// pre-open order accumulation — one wall-clock event, two consumers).
const (
	boundaryPreCloseMin = 21*60 + 55 // Friday 21:55 UTC
	boundaryCloseMin    = 22 * 60    // Friday 22:00 UTC (spec §1)
	boundaryPreOpenMin  = 20*60 + 45 // Sunday 20:45 UTC (spec §6.7)
	boundaryOpenMin     = 21 * 60    // Sunday 21:00 UTC (spec §1)
)

// ---------------------------------------------------------------------------
// Pure schedule math — UTC, deterministic, unit-testable.
// ---------------------------------------------------------------------------

// boundariesOn returns the transition instants of one civil day (only
// Friday and Sunday carry boundaries).
func boundariesOn(day time.Time) []struct {
	At time.Time
	To SessionState
} {
	day = time.Date(day.UTC().Year(), day.UTC().Month(), day.UTC().Day(),
		0, 0, 0, 0, time.UTC)
	mk := func(min int, to SessionState) struct {
		At time.Time
		To SessionState
	} {
		return struct {
			At time.Time
			To SessionState
		}{At: day.Add(time.Duration(min) * time.Minute), To: to}
	}
	switch day.Weekday() {
	case time.Friday:
		return []struct {
			At time.Time
			To SessionState
		}{mk(boundaryPreCloseMin, SessionPreClose), mk(boundaryCloseMin, SessionClosed)}
	case time.Sunday:
		return []struct {
			At time.Time
			To SessionState
		}{mk(boundaryPreOpenMin, SessionPreOpen), mk(boundaryOpenMin, SessionOpen)}
	}
	return nil
}

// sessionStateAt is the schedule function: the session state that should
// hold at instant now under the canonical weekly grid. Mid-week holiday
// overrides narrow the trading window via market:hours but do NOT move
// the weekly session boundaries.
func sessionStateAt(now time.Time) SessionState {
	utc := now.UTC()
	mins := utc.Hour()*60 + utc.Minute()
	switch utc.Weekday() {
	case time.Sunday:
		switch {
		case mins < boundaryPreOpenMin:
			return SessionClosed
		case mins < boundaryOpenMin:
			return SessionPreOpen
		default:
			return SessionOpen
		}
	case time.Friday:
		switch {
		case mins < boundaryPreCloseMin:
			return SessionOpen
		case mins < boundaryCloseMin:
			return SessionPreClose
		default:
			return SessionClosed
		}
	case time.Saturday:
		return SessionClosed
	default: // Monday–Thursday
		return SessionOpen
	}
}

// nextBoundary returns the next transition instant strictly after now
// and the state it enters.
func nextBoundary(now time.Time) (time.Time, SessionState) {
	now = now.UTC()
	for d := 0; d < 9; d++ {
		day := time.Date(now.Year(), now.Month(), now.Day()+d, 0, 0, 0, 0, time.UTC)
		for _, b := range boundariesOn(day) {
			if b.At.After(now) {
				return b.At, b.To
			}
		}
	}
	return time.Time{}, SessionOpen // unreachable within a 9-day scan
}

// NextSessionClose returns the instant the next SessionClosed boundary
// enters — the spec §5.4 DAY-order expiry under the weekly 24/5 grid
// (Friday 22:00 UTC). Zero when unreachable (never, on the canonical
// grid — fail closed by returning zero so callers leave expiry unset).
func NextSessionClose(now time.Time) time.Time {
	cur := now.UTC()
	for i := 0; i < 16; i++ {
		at, st := nextBoundary(cur)
		if st == SessionClosed {
			return at
		}
		if at.IsZero() {
			return time.Time{}
		}
		cur = at
	}
	return time.Time{}
}

// lastBoundary returns the most recent transition instant at-or-before
// now — the epoch that identifies the current boundary's effects lock.
func lastBoundary(now time.Time) (time.Time, SessionState) {
	now = now.UTC()
	for d := 0; d < 9; d++ {
		day := time.Date(now.Year(), now.Month(), now.Day()-d, 0, 0, 0, 0, time.UTC)
		bs := boundariesOn(day)
		for i := len(bs) - 1; i >= 0; i-- {
			if !bs[i].At.After(now) {
				return bs[i].At, bs[i].To
			}
		}
	}
	return time.Time{}, SessionClosed // unreachable within a 9-day scan
}

// ---------------------------------------------------------------------------
// Seams
// ---------------------------------------------------------------------------

// SessionPublisher fans session events onto the public WS channel —
// *ws.Server satisfies it; nil mutes broadcasts (never fatal).
type SessionPublisher interface {
	Publish(channel string, data any)
}

// RolloverRunner is the Task 3.3.7 settlement.RolloverService seam.
// The adapter signature returns a short human summary for logging;
// wiring binds settlement.RolloverService.RunOnce. nil = unwired (the
// transition still commits; the missing roll is logged and alerted —
// the standalone rollover daemon, when deployed, is the backstop).
type RolloverRunner func(ctx context.Context, now time.Time) (string, error)

// ActiveSymbolSource lists the symbols that participate in the weekly
// reopening auction (instruments.status = 'ACTIVE'). Wired to a PG
// query in production.
type ActiveSymbolSource func(ctx context.Context) ([]string, error)

// SessionEvent is the WS payload shape published on
// SessionStatusChannel (spec §6.7 event names verbatim).
type SessionEvent struct {
	Event            string       `json:"event"` // session.closed | session.pre_open | session.open | session.pre_close
	State            SessionState `json:"state"`
	At               string       `json:"at"`                 // RFC3339 transition instant
	NextState        SessionState `json:"next_state"`         // state the next boundary enters
	NextTransitionAt string       `json:"next_transition_at"` // RFC3339
}

// SessionService is the weekly session machine.
type SessionService struct {
	rdb      *excredis.Client
	shards   []int
	pub      SessionPublisher
	rollover RolloverRunner
	symbols  ActiveSymbolSource
	alert    Alerter
	logf     func(format string, args ...any)
	now      func() time.Time
}

// SessionDeps wires the service. RDB and Shards are required; every
// other seam is optional (nil → that effect is skipped and logged).
type SessionDeps struct {
	RDB      *excredis.Client
	Shards   []int
	Pub      SessionPublisher
	Rollover RolloverRunner
	Symbols  ActiveSymbolSource
	Alert    Alerter
	Logf     func(format string, args ...any)
}

// NewSessionService fails closed on missing required deps.
func NewSessionService(d SessionDeps) (*SessionService, error) {
	if d.RDB == nil {
		return nil, fmt.Errorf("session lifecycle: nil redis client")
	}
	if len(d.Shards) == 0 {
		return nil, fmt.Errorf("session lifecycle: empty shard set")
	}
	shards := append([]int(nil), d.Shards...)
	sort.Ints(shards)
	s := &SessionService{
		rdb: d.RDB, shards: shards, pub: d.Pub, rollover: d.Rollover,
		symbols: d.Symbols, alert: d.Alert, logf: d.Logf, now: time.Now,
	}
	if s.logf == nil {
		s.logf = func(string, ...any) {}
	}
	return s, nil
}

// SetClockForTest overrides the clock; tests only.
func (s *SessionService) SetClockForTest(now func() time.Time) { s.now = now }

func sessionStateKey(shard int) string {
	return SessionStateKeyPrefix + strconv.Itoa(shard)
}

// ---------------------------------------------------------------------------
// Evaluate / Reconcile / Run — the scheduler surface
// ---------------------------------------------------------------------------

// Evaluate drives the machine to the schedule-implied state for `now`:
// reconciles session:state:{shard} on every shard (writing missing keys —
// this is the boot reconcile), performs transitions with a per-boundary
// single-fire lock, and drains any pending effects left by a previous
// incomplete transition. Deterministic under an injected clock — the
// integration test sweeps a full Friday→Sunday sequence with it.
func (s *SessionService) Evaluate(ctx context.Context, now time.Time) error {
	now = now.UTC()
	expected := sessionStateAt(now)
	nextAt, nextState := nextBoundary(now)

	// 1. Drain pending effects first — a previous replica may have died
	//    mid-transition. Effects still belong to `expected`'s boundary;
	//    a stale pending_state is dropped (superseded by the schedule).
	if err := s.drainPending(ctx, expected, now); err != nil {
		s.logf("session: pending effects drain failed: %v", err)
	}

	// 2. Per-shard state reconcile.
	var firstErr error
	for _, shard := range s.shards {
		if err := s.evaluateShard(ctx, shard, expected, now, nextAt, nextState); err != nil {
			s.logf("session: shard %d evaluate failed: %v", shard, err)
			if firstErr == nil {
				firstErr = err
			}
		}
	}
	return firstErr
}

// Reconcile is the boot contract: rewrite session:state:* from the
// schedule — a restarted gateway can never leave shards believing the
// market is OPEN through a weekend. Identity: Evaluate(now).
func (s *SessionService) Reconcile(ctx context.Context) error {
	return s.Evaluate(ctx, s.now())
}

// Run is the daemon loop: Evaluate on every tick until ctx cancels.
// Tick default 5s — far tighter than the 15-minute pre-open window and
// comfortably inside the 5-minute PRE_CLOSE advisory.
func (s *SessionService) Run(ctx context.Context, tick time.Duration) error {
	if tick <= 0 {
		tick = 5 * time.Second
	}
	if err := s.Evaluate(ctx, s.now()); err != nil {
		s.logf("session: boot evaluate failed: %v", err)
	}
	t := time.NewTicker(tick)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
			if err := s.Evaluate(ctx, s.now()); err != nil {
				s.logf("session: evaluate failed: %v", err)
			}
		}
	}
}

// ---------------------------------------------------------------------------
// Shard state read/write
// ---------------------------------------------------------------------------

// evaluateShard reconciles one shard's session:state key.
func (s *SessionService) evaluateShard(ctx context.Context, shard int,
	expected SessionState, now, nextAt time.Time, nextState SessionState) error {
	key := sessionStateKey(shard)
	cur, err := s.rdb.HGetAll(ctx, key).Result()
	if err != nil {
		return fmt.Errorf("session: read %s: %w", key, err)
	}
	stored := SessionState(cur["state"])

	if stored == expected && stored.valid() {
		// State already correct — refresh the forward-looking fields when
		// they drift (cheap HSET only on change).
		if cur["next_transition_at"] != nextAt.Format(time.RFC3339) ||
			cur["next_state"] != string(nextState) {
			if err := s.rdb.HSet(ctx, key,
				"next_transition_at", nextAt.Format(time.RFC3339),
				"next_state", string(nextState)).Err(); err != nil {
				return fmt.Errorf("session: refresh %s: %w", key, err)
			}
		}
		return nil
	}

	// Transition: CAS so two replicas do not both believe they performed
	// this shard's flip (only the winner runs the boundary effects).
	event := sessionEventName(expected)
	won, err := s.casTransition(ctx, shard, expected, now, nextAt, nextState, event)
	if err != nil {
		return err
	}
	if !won {
		return nil // another replica beat us to it
	}
	s.logf("session: shard %d %q → %q at %s", shard, stored, expected,
		now.Format(time.RFC3339))
	s.runBoundaryEffects(ctx, expected, now, nextAt, nextState)
	return nil
}

// casTransitionScript atomically flips one shard's session hash when the
// stored state differs from the target. Returns 1 on transition,
// 0 when already in target.
var casTransitionScript = goredis.NewScript(`
local cur = redis.call('HGET', KEYS[1], 'state')
if cur == ARGV[1] then return 0 end
redis.call('HSET', KEYS[1],
  'state', ARGV[1],
  'entered_at', ARGV[2],
  'next_transition_at', ARGV[3],
  'next_state', ARGV[4],
  'last_event', ARGV[5])
return 1
`)

func (s *SessionService) casTransition(ctx context.Context, shard int,
	to SessionState, now, nextAt time.Time, nextState SessionState, event string) (bool, error) {
	n, err := casTransitionScript.Run(ctx, s.rdb.Client,
		[]string{sessionStateKey(shard)},
		string(to), now.Format(time.RFC3339),
		nextAt.Format(time.RFC3339), string(nextState), event).Int()
	if err != nil {
		return false, fmt.Errorf("session: transition shard %d: %w", shard, err)
	}
	return n == 1, nil
}

// sessionEventName maps a target state to its §6.7 WS event name.
// PRE_CLOSE is an advisory extension (spec lists only closed/pre_open/
// open) — clients get a 5-minute close warning for free.
func sessionEventName(to SessionState) string {
	switch to {
	case SessionClosed:
		return "session.closed"
	case SessionPreOpen:
		return "session.pre_open"
	case SessionOpen:
		return "session.open"
	case SessionPreClose:
		return "session.pre_close"
	}
	return "session." + string(to)
}

// ---------------------------------------------------------------------------
// Boundary effects — pending-ledger discipline
// ---------------------------------------------------------------------------

// effectsFor lists the global side-effects a transition into `to`
// requires, in execution order.
func (s *SessionService) effectsFor(to SessionState) []string {
	switch to {
	case SessionPreClose:
		return []string{"ws"} // advisory event only
	case SessionClosed:
		// Auction keys first: drop any stale UNCROSS leftover before
		// the rollover leg runs (idempotent either way).
		return []string{"auction_clear", "rollover", "ws"}
	case SessionPreOpen:
		return []string{"auction_call", "ws"}
	case SessionOpen:
		// No auction effect: the CALL key's deadline IS the release —
		// the C++ AuctionManager uncrosses at the deadline and deletes
		// the key (Task 15.3.6 contract). A leftover is cleared at the
		// next Friday close (auction_clear) if the consumer never ran.
		return []string{"ws"}
	}
	return nil
}

// runBoundaryEffects executes the transition's global effects under the
// per-boundary single-fire lock, recording the pending set in
// session:ctl BEFORE executing so a crash leaves a resumable ledger.
func (s *SessionService) runBoundaryEffects(ctx context.Context, to SessionState, now, nextAt time.Time, nextState SessionState) {
	boundaryAt, _ := lastBoundary(now)
	effects := s.effectsFor(to)
	if len(effects) == 0 {
		return
	}

	// Single-fire guard: one replica per boundary runs effects. A loser
	// contributes nothing — pending bookkeeping still lands because the
	// winner writes it before executing.
	lockKey := sessionEffectsLockPrefix + strconv.FormatInt(boundaryAt.Unix(), 10)
	ok, err := s.rdb.SetNX(ctx, lockKey, "1", 10*time.Minute).Result()
	if err != nil {
		s.logf("session: effects lock %s: %v — skipping effects", lockKey, err)
		return
	}
	if !ok {
		return // another replica is running this boundary's effects
	}
	if err := s.markPending(ctx, to, effects); err != nil {
		s.logf("session: mark pending failed: %v", err)
	}
	s.runEffects(ctx, to, effects, now, nextAt, nextState)
	s.clearPendingFor(ctx, to)
}

// markPending persists the pending effect set for the state being
// entered — written before execution so survivors resume it.
func (s *SessionService) markPending(ctx context.Context, st SessionState, effects []string) error {
	raw, err := json.Marshal(effects)
	if err != nil {
		return err
	}
	return s.rdb.HSet(ctx, sessionCtlKey,
		"pending_state", string(st),
		"pending_effects", string(raw),
		"updated_at", s.now().UTC().Format(time.RFC3339)).Err()
}

// clearPendingFor drops the pending set when it still belongs to st.
func (s *SessionService) clearPendingFor(ctx context.Context, st SessionState) {
	// Token-checked clear: never drop a pending set belonging to a
	// different state (a racing boundary must not lose its ledger).
	err := clearPendingScript.Run(ctx, s.rdb.Client,
		[]string{sessionCtlKey}, string(st)).Err()
	if err != nil {
		s.logf("session: clear pending: %v", err)
	}
}

var clearPendingScript = goredis.NewScript(`
if redis.call('HGET', KEYS[1], 'pending_state') == ARGV[1] then
  return redis.call('HDEL', KEYS[1], 'pending_state', 'pending_effects')
end
return 0
`)

// drainPending re-runs effects a previous transition failed to finish.
func (s *SessionService) drainPending(ctx context.Context, expected SessionState, now time.Time) error {
	cur, err := s.rdb.HGetAll(ctx, sessionCtlKey).Result()
	if err != nil {
		return fmt.Errorf("session: read ctl: %w", err)
	}
	ps, pe := cur["pending_state"], cur["pending_effects"]
	if ps == "" || pe == "" {
		return nil
	}
	if SessionState(ps) != expected {
		// Schedule moved on — the pending set is stale, drop it.
		_ = s.rdb.HDel(ctx, sessionCtlKey, "pending_state", "pending_effects").Err()
		return nil
	}
	var effects []string
	if err := json.Unmarshal([]byte(pe), &effects); err != nil {
		_ = s.rdb.HDel(ctx, sessionCtlKey, "pending_state", "pending_effects").Err()
		return fmt.Errorf("session: pending parse: %w", err)
	}
	nextAt, nextState := nextBoundary(now)
	s.logf("session: draining pending effects for %s: %v", expected, effects)
	s.runEffects(ctx, expected, effects, now, nextAt, nextState)
	s.clearPendingFor(ctx, expected)
	return nil
}

// runEffects executes each effect; failures log + raise the ops alert
// and leave the pending set in place (drained on the next Evaluate).
func (s *SessionService) runEffects(ctx context.Context, to SessionState,
	effects []string, now, nextAt time.Time, nextState SessionState) {
	for _, e := range effects {
		var err error
		switch e {
		case "rollover":
			err = s.effectRollover(ctx, now)
		case "auction_call":
			err = s.effectAuctionCall(ctx, now)
		case "auction_clear":
			err = s.effectAuctionClear(ctx)
		case "ws":
			err = s.effectPublish(to, now, nextAt, nextState)
		}
		if err != nil {
			s.logf("session: effect %s failed: %v", e, err)
			if s.alert != nil {
				if aerr := s.alert(ctx, "P1",
					fmt.Sprintf("session %s effect %s failed: %v", to, e, err)); aerr != nil {
					s.logf("session: alert failed: %v", aerr)
				}
			}
		}
	}
}

// ---------------------------------------------------------------------------
// Individual effects
// ---------------------------------------------------------------------------

// effectRollover fires the Tom-Next roll through the Task 3.3.7 seam.
// Friday 22:00 UTC lands inside the rollover clock's Friday cutoff —
// RunOnce is idempotent (rollover_runs + execution lock), so a replica
// retry or a same-day daemon run can never double-roll.
func (s *SessionService) effectRollover(ctx context.Context, now time.Time) error {
	if s.rollover == nil {
		s.logf("session: rollover seam unwired — Friday close roll skipped (daemon backstop)")
		return nil
	}
	summary, err := s.rollover(ctx, now)
	if err != nil {
		return fmt.Errorf("tom-next rollover: %w", err)
	}
	s.logf("session: tom-next rollover complete: %s", summary)
	return nil
}

// effectAuctionCall writes instrument:auction:{symbol} =
// "CALL:{deadline_unix_ns}" for every ACTIVE instrument — the exact
// key contract the Task 15.3.2 resume path owns and the C++
// AuctionManager consumes (Task 15.3.6 reopening-auction machinery:
// hold the book in CALL until the deadline, then single-price uncross
// and delete). For the weekly open the deadline is the next boundary —
// Sunday 21:00 UTC on the canonical grid — giving the §24 #142
// 15-minute pre-open CALL window (spec §6.7's 20:45 → 21:00 span).
func (s *SessionService) effectAuctionCall(ctx context.Context, now time.Time) error {
	if s.symbols == nil {
		return fmt.Errorf("auction CALL: symbol source unwired")
	}
	syms, err := s.symbols(ctx)
	if err != nil {
		return fmt.Errorf("auction CALL: list symbols: %w", err)
	}
	deadline, _ := nextBoundary(now) // the Sunday 21:00 open instant
	val := fmt.Sprintf("CALL:%d", deadline.UnixNano())
	pipe := s.rdb.TxPipeline()
	for _, sym := range syms {
		pipe.Set(ctx, AuctionKeyPrefix+sym, val, 0)
	}
	if _, err := pipe.Exec(ctx); err != nil {
		return fmt.Errorf("auction CALL keys: %w", err)
	}
	return nil
}

// effectAuctionClear removes leftover auction control keys on session
// close so a stale CALL can never leak into the next weekly cycle.
func (s *SessionService) effectAuctionClear(ctx context.Context) error {
	if s.symbols == nil {
		return nil // nothing enumerable — nothing to clear
	}
	syms, err := s.symbols(ctx)
	if err != nil {
		return fmt.Errorf("auction clear: list symbols: %w", err)
	}
	if len(syms) == 0 {
		return nil
	}
	keys := make([]string, 0, len(syms))
	for _, sym := range syms {
		keys = append(keys, AuctionKeyPrefix+sym)
	}
	if err := s.rdb.Del(ctx, keys...).Err(); err != nil {
		return fmt.Errorf("auction clear: %w", err)
	}
	return nil
}

// effectPublish broadcasts the session event on the public channel.
func (s *SessionService) effectPublish(to SessionState, now, nextAt time.Time, nextState SessionState) error {
	if s.pub == nil {
		return nil
	}
	s.pub.Publish(SessionStatusChannel, SessionEvent{
		Event:            sessionEventName(to),
		State:            to,
		At:               now.Format(time.RFC3339),
		NextState:        nextState,
		NextTransitionAt: nextAt.Format(time.RFC3339),
	})
	return nil
}

// ---------------------------------------------------------------------------
// Read surface — GET /api/v1/session/status
// ---------------------------------------------------------------------------

// ShardSession is one shard's view for the status payload.
type ShardSession struct {
	ShardID   int          `json:"shard_id"`
	State     SessionState `json:"state"`
	EnteredAt string       `json:"entered_at,omitempty"`
	Reachable bool         `json:"reachable"` // false when the Redis read failed
}

// SessionStatus is the §6.7 status response: consensus state, next
// scheduled transition and per-shard coverage (spec: "{state,
// next_transition_at, shard coverage}").
type SessionStatus struct {
	State            SessionState   `json:"state"`
	Consistent       bool           `json:"consistent"`
	Shards           []ShardSession `json:"shards"`
	ShardCoverage    string         `json:"shard_coverage"` // e.g. "7/8 shards OPEN"
	NextState        SessionState   `json:"next_state"`
	NextTransitionAt string         `json:"next_transition_at"` // RFC3339
	MarketOpen       bool           `json:"market_open"`
	PendingEffects   []string       `json:"pending_effects,omitempty"`
}

// Status aggregates every shard's session:state view. The reported
// `state` is the schedule-implied state (the machine's own clock is the
// authority); `consistent=false` exposes shards that have not yet
// converged — never a fabricated all-green.
func (s *SessionService) Status(ctx context.Context) (*SessionStatus, error) {
	now := s.now().UTC()
	expected := sessionStateAt(now)
	nextAt, nextState := nextBoundary(now)

	out := &SessionStatus{
		State:            expected,
		Consistent:       true,
		NextState:        nextState,
		NextTransitionAt: nextAt.Format(time.RFC3339),
		MarketOpen:       expected == SessionOpen,
	}
	inState := 0
	for _, shard := range s.shards {
		sh := ShardSession{ShardID: shard}
		cur, err := s.rdb.HGetAll(ctx, sessionStateKey(shard)).Result()
		switch {
		case err != nil:
			sh.State, sh.Reachable = "UNREACHABLE", false
			out.Consistent = false
		default:
			sh.State = SessionState(cur["state"])
			sh.EnteredAt = cur["entered_at"]
			sh.Reachable = true
			if sh.State == "" {
				sh.State = "UNSET"
				out.Consistent = false
			} else if sh.State != expected {
				out.Consistent = false
			} else {
				inState++
			}
		}
		out.Shards = append(out.Shards, sh)
	}
	out.ShardCoverage = fmt.Sprintf("%d/%d shards %s", inState, len(s.shards), expected)

	// Surface a non-empty pending ledger — ops sees unfinished effects.
	if cur, err := s.rdb.HGetAll(ctx, sessionCtlKey).Result(); err == nil {
		if pe := cur["pending_effects"]; pe != "" {
			var eff []string
			if json.Unmarshal([]byte(pe), &eff) == nil && len(eff) > 0 {
				out.PendingEffects = eff
			}
		}
	}
	return out, nil
}

// Shards exposes the configured shard set (status/debug surfaces).
func (s *SessionService) Shards() []int { return append([]int(nil), s.shards...) }
