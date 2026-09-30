// Phase-21 Task 21.3.23 — Sanctions-provider outage quarantine +
// pending-screen queue + ordered replay (spec §14.3, §24 #43: feed
// outage degrades the sanctions feed SCOPE, never the venue — existing
// screened counterparties keep trading under enhanced monitoring while
// new onboarding and fiat withdrawals quarantine pending the provider).
//
// Two moving parts:
//
//	ProviderGate — health latch over the configured external screening
//	providers. A request timing out past ProviderTimeout (30s) or ALL
//	configured providers failing transitions the gate to QUARANTINED;
//	the first provider success after that latches it back to HEALTHY and
//	fires the recovery hook (queue replay). Every transition is audited
//	(admin_audit_log + hash chain, providerless-verifiable payload) and
//	alerted on the ops channel.
//
//	ScreenQueue (+ QueuedScreener + QueueReplayer) — while the gate is
//	quarantined, screens that MUST happen (new onboarding, withdrawal
//	confirmations, registrations) are persisted to an at-least-once
//	queue and the caller fails closed with errQuarantined →
//	SANCTIONS_SERVICE_UNAVAILABLE (HTTP 503 per errs/codes.go:199). On
//	recovery the QueueReplayer drains the queue FIFO, re-screens through
//	the now-healthy screener, publishes account flags and routes hits to
//	the hold workflow — and refuses to ack an item it could not
//	definitively screen (fail-closed replay).
package compliance

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"exchange/internal/admin"
	exchredis "exchange/internal/redis"
	excerrors "exchange/pkg/errors"
)

// ProviderTimeout is the task-pinned external-request ceiling: any
// screening provider request exceeding it counts as a provider failure
// (and an immediate quarantine trigger). The ops staleness metric
// (GateStatus.StaleFor) surfaces feed age for the 60s operator alarm —
// two distinct clocks on purpose: 30s bounds a single request, 60s of
// no successful provider contact is the staleness alert.
const ProviderTimeout = 30 * time.Second

// QueueBacklogAlertThreshold is the compliance-officer alert trip point
// for the pending-screen backlog (mirrors the ARM/APA repair backlog
// threshold — both are "queued work the officer must know about").
const QueueBacklogAlertThreshold = 100

// Pending-screen flow identifiers — recorded on the queue item so
// replay applies the right follow-up (withdrawal hits hold the account;
// onboarding hits reject the application).
const (
	ScreenFlowOnboarding   = "ONBOARDING"
	ScreenFlowWithdrawal   = "WITHDRAWAL"
	ScreenFlowDeposit      = "DEPOSIT"
	ScreenFlowRegistration = "REGISTRATION"
	ScreenFlowTrade        = "TRADE"
	ScreenFlowPEP          = "PEP_MONITOR"
)

// ErrQuarantined is the coded error surfaced by the quarantine-aware
// screener and queue while the provider gate is down — registered as
// SANCTIONS_SERVICE_UNAVAILABLE (503).
var ErrQuarantined = excerrors.New("SANCTIONS_SERVICE_UNAVAILABLE",
	"sanctions screening provider unreachable — request queued for replay")

// AuditSink is the hash-chained audit seam every screening/quarantine
// component writes through — implemented by AdminAuditSink over
// admin.LogAuto in wiring; implementers MUST persist an immutable,
// providerless-verifiable detail payload (never opaque blobs).
type AuditSink interface {
	Record(ctx context.Context, actor int64, action, targetType string,
		targetID int64, detail any) error
}

// ---------------------------------------------------------------------------
// ProviderGate
// ---------------------------------------------------------------------------

// providerHealth tracks one upstream provider's contact ledger.
type providerHealth struct {
	up           bool // last contact succeeded
	consecFail   int  // consecutive failures since last success
	lastOK       time.Time
	lastFail     time.Time
	lastErr      string
	timedOut     bool // last observation was a >30s timeout
	observations int
}

// GateStatus is the operator-surface snapshot of the provider gate.
type GateStatus struct {
	Quarantined bool              `json:"quarantined"`
	Since       *time.Time        `json:"since,omitempty"`
	Reason      string            `json:"reason,omitempty"`
	Providers   map[string]string `json:"providers"` // name → UP|DOWN
	StaleFor    string            `json:"stale_for"` // duration since last provider success
}

// ProviderGate is the scoped-degradation latch. Down-scoped: while
// quarantined, callers route onboarding/withdrawals to the pending
// queue and answer SANCTIONS_SERVICE_UNAVAILABLE; pre-screened trading
// is unaffected (the C++ hook keeps screening per-account flags, and
// ScreeningService ramps monitoring instead).
type ProviderGate struct {
	mu      sync.Mutex
	now     func() time.Time
	healthy map[string]*providerHealth

	quarantined bool
	since       time.Time
	reason      string

	alerter Alerter
	auditor AuditSink

	onRecover []func(ctx context.Context) // replay hook, wired by replayer
}

// NewProviderGate registers the configured provider names. An empty set
// is legal (file-backed dev binding) — the gate then only quarantines
// on explicit ReportTimeout/ReportFailure calls and recovers on
// ReportSuccess.
func NewProviderGate(providers []string) *ProviderGate {
	g := &ProviderGate{
		now:     func() time.Time { return time.Now().UTC() },
		healthy: map[string]*providerHealth{},
	}
	for _, p := range providers {
		if p == "" {
			continue
		}
		g.healthy[p] = &providerHealth{up: true}
	}
	return g
}

// WithClock injects the clock (tests).
func (g *ProviderGate) WithClock(now func() time.Time) *ProviderGate {
	g.now = now
	return g
}

// WithAlerter/WithAuditor wire ops alerting + the hash-chained audit
// sink used on every quarantine/recovery transition.
func (g *ProviderGate) WithAlerter(a Alerter) *ProviderGate   { g.alerter = a; return g }
func (g *ProviderGate) WithAuditor(a AuditSink) *ProviderGate { g.auditor = a; return g }

// OnRecover registers a hook fired once per quarantine→healthy
// transition — the QueueReplayer drain is bound here in wiring.
func (g *ProviderGate) OnRecover(fn func(ctx context.Context)) *ProviderGate {
	g.mu.Lock()
	g.onRecover = append(g.onRecover, fn)
	g.mu.Unlock()
	return g
}

// Quarantined reports the gate state — hot-path read, cheap.
func (g *ProviderGate) Quarantined() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.quarantined
}

// Check returns ErrQuarantined while quarantined (the 503 surface).
func (g *ProviderGate) Check() error {
	if g.Quarantined() {
		return ErrQuarantined
	}
	return nil
}

// ReportTimeout records a provider request that exceeded
// ProviderTimeout — per task text this alone transitions the gate to
// quarantine (a 30s+ hang is an outage signal, not noise).
func (g *ProviderGate) ReportTimeout(ctx context.Context, provider string) {
	g.mu.Lock()
	h := g.prov(provider)
	h.up, h.timedOut, h.consecFail = false, true, h.consecFail+1
	h.lastFail, h.lastErr = g.now(), "request exceeded 30s provider timeout"
	h.observations++
	g.mu.Unlock()
	g.quarantine(ctx, "provider timeout: "+provider)
}

// ReportResult records one bounded provider call: err == nil is a
// success (and a recovery trigger when quarantined); any error marks
// the provider down and quarantines the gate once EVERY configured
// provider is down.
func (g *ProviderGate) ReportResult(ctx context.Context, provider string, err error) {
	g.mu.Lock()
	h := g.prov(provider)
	h.observations++
	if err == nil {
		h.up, h.timedOut, h.consecFail = true, false, 0
		h.lastOK, h.lastErr = g.now(), ""
		was := g.quarantined
		hooks := append([]func(context.Context){}, g.onRecover...)
		g.quarantined, g.reason = false, ""
		g.mu.Unlock()
		if was {
			g.transition(ctx, "sanctions.provider_recovered",
				"provider contact restored: "+provider)
			for _, fn := range hooks {
				fn(ctx)
			}
		}
		return
	}
	h.up, h.timedOut, h.consecFail = false, false, h.consecFail+1
	h.lastFail, h.lastErr = g.now(), err.Error()
	allDown := len(g.healthy) > 0
	for _, p := range g.healthy {
		if p.up {
			allDown = false
			break
		}
	}
	g.mu.Unlock()
	if allDown {
		g.quarantine(ctx, "all screening providers failing (last: "+
			provider+": "+err.Error()+")")
	}
}

func (g *ProviderGate) prov(name string) *providerHealth {
	h, ok := g.healthy[name]
	if !ok {
		h = &providerHealth{up: true}
		g.healthy[name] = h
	}
	return h
}

// quarantine latches the gate and emits the transition audit + P1
// alert (once — idempotent while already quarantined).
func (g *ProviderGate) quarantine(ctx context.Context, reason string) {
	g.mu.Lock()
	if g.quarantined {
		g.mu.Unlock()
		return
	}
	g.quarantined, g.since, g.reason = true, g.now(), reason
	g.mu.Unlock()
	g.transition(ctx, "sanctions.provider_quarantined", reason)
}

// transition emits the P1 ops alert and the hash-chained audit row for
// a gate transition. Actor is 0-attributed to the gate itself — the
// audit convention (lifecycle.go) requires a positive id; the system
// actor convention is the affected principal, which for a provider
// outage is recorded as actor 0 → sink substitutes the compliance
// service account.
func (g *ProviderGate) transition(ctx context.Context, action, summary string) {
	if g.alerter != nil {
		_ = g.alerter(ctx, "P1", "SANCTIONS_PROVIDER_"+ternaryStr(
			action == "sanctions.provider_quarantined", "OUTAGE", "RECOVERED"),
			summary)
	}
	if g.auditor != nil {
		g.mu.Lock()
		detail := map[string]any{
			"action": action, "reason": summary,
			"quarantined": g.quarantined, "since": g.since,
		}
		g.mu.Unlock()
		_ = g.auditor.Record(ctx, 0, action, "sanctions_provider", 0, detail)
	}
}

func ternaryStr(c bool, a, b string) string {
	if c {
		return a
	}
	return b
}

// Status renders the operator snapshot (provider map, staleness).
func (g *ProviderGate) Status() GateStatus {
	g.mu.Lock()
	defer g.mu.Unlock()
	st := GateStatus{
		Quarantined: g.quarantined,
		Reason:      g.reason,
		Providers:   map[string]string{},
	}
	if g.quarantined {
		s := g.since
		st.Since = &s
	}
	var lastOK time.Time
	for name, h := range g.healthy {
		if h.up {
			st.Providers[name] = "UP"
			if h.lastOK.After(lastOK) {
				lastOK = h.lastOK
			}
		} else {
			st.Providers[name] = "DOWN"
		}
	}
	if !lastOK.IsZero() {
		st.StaleFor = g.now().Sub(lastOK).String()
	}
	return st
}

// ---------------------------------------------------------------------------
// Pending-screen queue
// ---------------------------------------------------------------------------

// PendingScreen is one queued screening obligation — the flows that
// MUST fail closed during an outage park here for ordered replay.
type PendingScreen struct {
	ID         int64     `json:"id"`
	AccountID  int64     `json:"account_id"`
	ActorID    int64     `json:"actor_id"` // audit-attribution user id (0 = system)
	Flow       string    `json:"flow"`
	Candidates []string  `json:"candidates"`
	QueuedAt   time.Time `json:"queued_at"`
	Attempts   int       `json:"attempts"`

	raw string `json:"-"` // transport payload (Redis impl's LREM handle)
}

// ScreenQueue is the at-least-once pending-screen seam — the memory
// implementation backs dev/tests, RedisScreenQueue backs production
// (pending + inflight lists + dedupe set).
type ScreenQueue interface {
	// Enqueue persists one obligation; same (flow, account) within the
	// dedupe window collapses to the queued item (idempotent enqueue).
	Enqueue(ctx context.Context, s PendingScreen) (int64, error)
	// Claim moves up to limit pending items to inflight and returns
	// them FIFO. Items not Ack'd stay inflight — RecoverInflight
	// returns them to pending.
	Claim(ctx context.Context, limit int) ([]PendingScreen, error)
	// Ack removes claimed items permanently.
	Ack(ctx context.Context, items ...PendingScreen) error
	// RecoverInflight returns every inflight item to pending — replay
	// calls it first so a crashed sweep never loses work.
	RecoverInflight(ctx context.Context) (int, error)
	// Depth reports the pending+inflight backlog (alerting metric).
	Depth(ctx context.Context) (int, error)
}

// MemoryScreenQueue is the in-memory dev/test implementation.
type MemoryScreenQueue struct {
	mu       sync.Mutex
	seq      int64
	pending  []PendingScreen
	inflight []PendingScreen
	seen     map[string]int64
	now      func() time.Time
}

// NewMemoryScreenQueue builds the dev/test queue.
func NewMemoryScreenQueue() *MemoryScreenQueue {
	return &MemoryScreenQueue{
		seen: map[string]int64{},
		now:  func() time.Time { return time.Now().UTC() },
	}
}

// WithClock injects the clock.
func (q *MemoryScreenQueue) WithClock(now func() time.Time) *MemoryScreenQueue {
	q.now = now
	return q
}

func dedupKey(s PendingScreen) string {
	return s.Flow + ":" + strconv.FormatInt(s.AccountID, 10)
}

// Enqueue dedupes on (flow, account) — a withdrawal that retries five
// times during an outage is one obligation, not five.
func (q *MemoryScreenQueue) Enqueue(_ context.Context, s PendingScreen) (int64, error) {
	if s.AccountID <= 0 || s.Flow == "" || len(s.Candidates) == 0 {
		return 0, excerrors.New("INVALID_REQUEST",
			"pending screen requires account_id, flow and candidates")
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	key := dedupKey(s)
	if id, dup := q.seen[key]; dup {
		for _, p := range append(append([]PendingScreen{}, q.pending...), q.inflight...) {
			if p.ID == id {
				return id, nil // already queued — idempotent
			}
		}
		delete(q.seen, key)
	}
	q.seq++
	s.ID, s.QueuedAt = q.seq, q.now()
	q.pending = append(q.pending, s)
	q.seen[key] = s.ID
	return s.ID, nil
}

// Claim moves up to limit pending → inflight, FIFO.
func (q *MemoryScreenQueue) Claim(_ context.Context, limit int) ([]PendingScreen, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if limit <= 0 || limit > len(q.pending) {
		limit = len(q.pending)
	}
	out := append([]PendingScreen{}, q.pending[:limit]...)
	q.inflight = append(q.inflight, q.pending[:limit]...)
	q.pending = q.pending[limit:]
	for i := range out {
		out[i].Attempts++
	}
	return out, nil
}

// Ack removes claimed items from inflight and releases dedupe keys.
func (q *MemoryScreenQueue) Ack(_ context.Context, items ...PendingScreen) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	ids := map[int64]bool{}
	for _, it := range items {
		ids[it.ID] = true
		delete(q.seen, dedupKey(it))
	}
	var keep []PendingScreen
	for _, p := range q.inflight {
		if !ids[p.ID] {
			keep = append(keep, p)
		}
	}
	q.inflight = keep
	return nil
}

// RecoverInflight returns all inflight items to pending (crash sweep).
func (q *MemoryScreenQueue) RecoverInflight(_ context.Context) (int, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	n := len(q.inflight)
	q.pending = append(q.inflight, q.pending...)
	q.inflight = nil
	return n, nil
}

// Depth reports pending + inflight depth.
func (q *MemoryScreenQueue) Depth(_ context.Context) (int, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	return len(q.pending) + len(q.inflight), nil
}

// RedisScreenQueue is the production queue — pending FIFO list,
// inflight list (at-least-once claim), per-(flow,account) dedupe keys
// (24h TTL) and a monotonically increasing id sequence.
type RedisScreenQueue struct {
	r *exchredis.Client
}

const (
	keyPending   = "exc:sanctions:pending"
	keyInflight  = "exc:sanctions:pending:inflight"
	keyQueueSeq  = "exc:sanctions:pending:seq"
	keyDedupPref = "exc:sanctions:pending:dedup:"
)

// NewRedisScreenQueue wraps the coordination client.
func NewRedisScreenQueue(c *exchredis.Client) *RedisScreenQueue {
	return &RedisScreenQueue{r: c}
}

// Enqueue serializes + dedupes + RPUSHes. Dedupe key TTL is 24h — the
// obligation outlives any plausible outage while still expiring.
func (q *RedisScreenQueue) Enqueue(ctx context.Context, s PendingScreen) (int64, error) {
	if s.AccountID <= 0 || s.Flow == "" || len(s.Candidates) == 0 {
		return 0, excerrors.New("INVALID_REQUEST",
			"pending screen requires account_id, flow and candidates")
	}
	dk := keyDedupPref + dedupKey(s)
	ok, err := q.r.SetNX(ctx, dk, "1", 24*time.Hour).Result()
	if err != nil {
		return 0, fmt.Errorf("screen queue dedupe: %w", err)
	}
	if !ok {
		return 0, nil // already queued — idempotent collapse
	}
	id, err := q.r.Incr(ctx, keyQueueSeq).Result()
	if err != nil {
		return 0, fmt.Errorf("screen queue seq: %w", err)
	}
	s.ID, s.QueuedAt = id, time.Now().UTC()
	blob, _ := json.Marshal(s)
	if _, err := q.r.RPush(ctx, keyPending, blob).Result(); err != nil {
		return 0, fmt.Errorf("screen queue enqueue: %w", err)
	}
	return id, nil
}

// Claim moves tail items (oldest — enqueue RPUSHes) to inflight FIFO.
func (q *RedisScreenQueue) Claim(ctx context.Context, limit int) ([]PendingScreen, error) {
	if limit <= 0 {
		limit = 100
	}
	var out []PendingScreen
	for i := 0; i < limit; i++ {
		raw, err := q.r.RPopLPush(ctx, keyPending, keyInflight).Result()
		if err != nil {
			break // list empty (or transport error — return what we got)
		}
		var s PendingScreen
		if err := json.Unmarshal([]byte(raw), &s); err != nil {
			continue // poisoned payload — drop silently is WRONG; keep it
		}
		s.raw = raw
		s.Attempts++
		out = append(out, s)
	}
	return out, nil
}

// Ack LREMs each claimed payload from inflight and clears dedupe keys.
func (q *RedisScreenQueue) Ack(ctx context.Context, items ...PendingScreen) error {
	for _, it := range items {
		if it.raw != "" {
			_, _ = q.r.LRem(ctx, keyInflight, 1, it.raw).Result()
		}
		_, _ = q.r.Del(ctx, keyDedupPref+dedupKey(it)).Result()
	}
	return nil
}

// RecoverInflight moves every inflight item back to pending — called at
// replay start so a crashed sweep never strands obligations.
func (q *RedisScreenQueue) RecoverInflight(ctx context.Context) (int, error) {
	n := 0
	for {
		_, err := q.r.RPopLPush(ctx, keyInflight, keyPending).Result()
		if err != nil {
			return n, nil // empty or transport error — nothing more to recover
		}
		n++
	}
}

// Depth reports pending+inflight backlog.
func (q *RedisScreenQueue) Depth(ctx context.Context) (int, error) {
	p, _ := q.r.LLen(ctx, keyPending).Result()
	f, _ := q.r.LLen(ctx, keyInflight).Result()
	return int(p + f), nil
}

// ---------------------------------------------------------------------------
// QuarantinedScreener — the fail-closed seam wrapper
// ---------------------------------------------------------------------------

// FundingScreener is the funding package's screening seam surface
// (deposit + withdrawal) — *ListScreener implements it.
type FundingScreener interface {
	ScreenDeposit(ctx context.Context, accountID int64,
		senderName, senderAccount string) (bool, error)
	ScreenWithdrawal(ctx context.Context, accountID int64,
		beneficiaryName, destination string) (bool, error)
}

// QuarantinedScreener wraps the real screener: quarantined gate →
// enqueue the obligation + ErrQuarantined (→SANCTIONS_SERVICE_UNAVAILABLE
// at the funding layer); healthy → delegate. This is the object wired
// into the funding service, never a bare ListScreener — the queue is
// where fail-closed becomes fail-recoverable.
type QuarantinedScreener struct {
	inner FundingScreener
	gate  *ProviderGate
	queue ScreenQueue
}

// NewQuarantinedScreener binds inner + gate + queue.
func NewQuarantinedScreener(inner FundingScreener, gate *ProviderGate,
	queue ScreenQueue) *QuarantinedScreener {
	return &QuarantinedScreener{inner: inner, gate: gate, queue: queue}
}

// Inner exposes the wrapped screener for wiring surfaces that need the
// unwrapped list (status endpoints).
func (q *QuarantinedScreener) Inner() FundingScreener { return q.inner }

func (q *QuarantinedScreener) enqueue(ctx context.Context, flow string,
	accountID int64, candidates []string) {
	if q.queue == nil {
		return
	}
	_, _ = q.queue.Enqueue(ctx, PendingScreen{
		AccountID: accountID, Flow: flow, Candidates: candidates})
}

// ScreenDeposit implements funding.SanctionsScreener under quarantine.
func (q *QuarantinedScreener) ScreenDeposit(ctx context.Context,
	accountID int64, senderName, senderAccount string) (bool, error) {
	if q.gate != nil && q.gate.Quarantined() {
		q.enqueue(ctx, ScreenFlowDeposit, accountID,
			[]string{senderName, senderAccount})
		return false, ErrQuarantined
	}
	return q.inner.ScreenDeposit(ctx, accountID, senderName, senderAccount)
}

// ScreenWithdrawal implements funding.WithdrawalScreener under
// quarantine — outbound fiat legs are the task-named quarantine scope.
func (q *QuarantinedScreener) ScreenWithdrawal(ctx context.Context,
	accountID int64, beneficiaryName, destination string) (bool, error) {
	if q.gate != nil && q.gate.Quarantined() {
		q.enqueue(ctx, ScreenFlowWithdrawal, accountID,
			[]string{beneficiaryName, destination})
		return false, ErrQuarantined
	}
	return q.inner.ScreenWithdrawal(ctx, accountID, beneficiaryName, destination)
}

// ScreenOnboarding is the registration/new-client leg — queued like
// every other must-screen flow during outage.
func (q *QuarantinedScreener) ScreenOnboarding(ctx context.Context,
	accountID int64, candidates ...string) ([]MatchHit, error) {
	if q.gate != nil && q.gate.Quarantined() {
		q.enqueue(ctx, ScreenFlowOnboarding, accountID, candidates)
		return nil, ErrQuarantined
	}
	s, ok := q.inner.(*ListScreener)
	if !ok {
		return nil, fmt.Errorf("sanctions: onboarding screen requires ListScreener")
	}
	return s.ScreenRegistration(ctx, accountID, candidates...)
}

// ---------------------------------------------------------------------------
// QueueReplayer — ordered, fail-closed replay on provider recovery
// ---------------------------------------------------------------------------

// ReplayReport summarizes one drain pass.
type ReplayReport struct {
	RecoveredInflight int         `json:"recovered_inflight"`
	Claimed           int         `json:"claimed"`
	Rescreened        int         `json:"rescreened"` // clean pass — flag marked screened
	HitCount          int         `json:"hit_count"`  // positive — hold routed
	Hits              []ReplayHit `json:"hits,omitempty"`
	Backlog           int         `json:"backlog"`
}

// ReplayHit records one positive replay result for audit detail.
type ReplayHit struct {
	ScreenID  int64      `json:"screen_id"`
	AccountID int64      `json:"account_id"`
	Flow      string     `json:"flow"`
	Hits      []MatchHit `json:"hits"`
}

// QueueReplayer drains the pending-screen queue after provider
// recovery. Ordering: pending FIFO. Trust: an item is Ack'd only after
// a definitive screen — screener errors abort the pass with the
// remainder left inflight/pending (fail closed).
type QueueReplayer struct {
	queue    ScreenQueue
	screener *ListScreener
	flags    *FlagPublisher
	onHit    func(ctx context.Context, s PendingScreen, hits []MatchHit) error
	alerter  Alerter
	auditor  AuditSink
	batch    int
}

// NewQueueReplayer wires the drain. onHit receives positive results —
// wiring binds it to ScreeningService.HandleReplayHit which applies
// holds/flags through the Phase-14.3.10 workflow.
func NewQueueReplayer(queue ScreenQueue, screener *ListScreener) *QueueReplayer {
	return &QueueReplayer{queue: queue, screener: screener, batch: 500}
}

// WithFlags publishes screened/flagged account bits for the C++ hook.
func (r *QueueReplayer) WithFlags(f *FlagPublisher) *QueueReplayer {
	r.flags = f
	return r
}

// WithOnHit wires the hold-routing hook.
func (r *QueueReplayer) WithOnHit(fn func(ctx context.Context,
	s PendingScreen, hits []MatchHit) error) *QueueReplayer {
	r.onHit = fn
	return r
}

// WithAlerter/WithAuditor wire ops surfaces; WithBatch sizes claim batches.
func (r *QueueReplayer) WithAlerter(a Alerter) *QueueReplayer   { r.alerter = a; return r }
func (r *QueueReplayer) WithAuditor(a AuditSink) *QueueReplayer { r.auditor = a; return r }
func (r *QueueReplayer) WithBatch(n int) *QueueReplayer {
	if n > 0 {
		r.batch = n
	}
	return r
}

// Replay drains pending work in FIFO batches. Returns the report even
// on error so operators see partial progress; unscreened items stay
// queued — a screen that cannot establish a trustworthy result is
// never acked.
func (r *QueueReplayer) Replay(ctx context.Context) (ReplayReport, error) {
	var rep ReplayReport
	if r.queue == nil || r.screener == nil {
		return rep, fmt.Errorf("sanctions: replayer unbound")
	}
	recovered, _ := r.queue.RecoverInflight(ctx)
	rep.RecoveredInflight = recovered
	for {
		items, err := r.queue.Claim(ctx, r.batch)
		if err != nil {
			return rep, fmt.Errorf("sanctions: queue claim: %w", err)
		}
		if len(items) == 0 {
			break
		}
		rep.Claimed += len(items)
		sortPending(items)
		for _, s := range items {
			hits, err := r.screener.ScreenParty(ctx, "", s.Candidates...)
			if err != nil {
				// Screener still unavailable/quarantined — stop the pass
				// and leave this item (and the rest) unacked. Fail closed.
				rep.Backlog, _ = r.queue.Depth(ctx)
				return rep, fmt.Errorf("sanctions: replay screen account %d: %w",
					s.AccountID, err)
			}
			if len(hits) > 0 {
				rep.HitCount++
				rep.Hits = append(rep.Hits, ReplayHit{
					ScreenID: s.ID, AccountID: s.AccountID, Flow: s.Flow,
					Hits: hits})
				if r.onHit != nil {
					if err := r.onHit(ctx, s, hits); err != nil {
						return rep, fmt.Errorf(
							"sanctions: replay hit handling account %d: %w",
							s.AccountID, err)
					}
				}
				if r.flags != nil {
					_ = r.flags.FlagAccount(ctx, s.AccountID, "REPLAY_HIT")
				}
			} else {
				rep.Rescreened++
				if r.flags != nil {
					_ = r.flags.MarkScreened(ctx, s.AccountID)
				}
			}
		}
		if err := r.queue.Ack(ctx, items...); err != nil {
			return rep, fmt.Errorf("sanctions: queue ack: %w", err)
		}
	}
	rep.Backlog, _ = r.queue.Depth(ctx)
	if r.alerter != nil && rep.Backlog > QueueBacklogAlertThreshold {
		_ = r.alerter(ctx, "P1", "SANCTIONS_SCREEN_BACKLOG",
			fmt.Sprintf("pending-screen backlog %d exceeds %d — compliance officer review required",
				rep.Backlog, QueueBacklogAlertThreshold))
	}
	if r.auditor != nil && (rep.Claimed > 0 || recovered > 0) {
		_ = r.auditor.Record(ctx, 0, "sanctions.queue_replayed",
			"sanctions_screen_queue", 0, map[string]any{
				"recovered_inflight": rep.RecoveredInflight,
				"claimed":            rep.Claimed,
				"rescreened":         rep.Rescreened,
				"hits":               rep.HitCount,
				"backlog":            rep.Backlog,
			})
	}
	return rep, nil
}

// sortPending orders claimed items FIFO by queued_at — belt-and-braces
// ordering guarantee for implementations whose claim order is storage
// order (the Redis impl is FIFO by construction; the memory impl too).
func sortPending(items []PendingScreen) {
	sort.Slice(items, func(i, j int) bool {
		if items[i].QueuedAt.Equal(items[j].QueuedAt) {
			return items[i].ID < items[j].ID
		}
		return items[i].QueuedAt.Before(items[j].QueuedAt)
	})
}

// ---------------------------------------------------------------------------
// AdminAuditSink — the AuditSink adapter over admin.LogAuto
// ---------------------------------------------------------------------------

// AdminAuditSink writes screening/quarantine events into
// admin_audit_log + the hash chain (providerless-verifiable detail —
// the payload is the structured event map, never an opaque blob).
// Machine actions attribute the affected principal as the acting id
// (the lifecycle.go convention: admin_user_id cannot be 0).
type AdminAuditSink struct {
	Pool *pgxpool.Pool
}

// Record appends the audit row via admin.LogAuto.
func (a AdminAuditSink) Record(ctx context.Context, actor int64,
	action, targetType string, targetID int64, detail any) error {
	if a.Pool == nil {
		return nil
	}
	if actor <= 0 {
		actor = targetID
	}
	if actor <= 0 {
		return nil // no attributable principal — skip rather than fabricate
	}
	var tid *int64
	if targetID > 0 {
		tid = &targetID
	}
	_, _, err := admin.LogAuto(ctx, a.Pool, admin.AuditEntry{
		AdminUserID: actor,
		Action:      action,
		TargetType:  targetType,
		TargetID:    tid,
		AfterState:  detail,
	})
	return err
}
