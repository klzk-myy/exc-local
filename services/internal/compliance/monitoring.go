// Phase-21 Task 21.3.11 — ongoing transaction monitoring (spec §14.3:
// structuring/smurfing, velocity anomalies, dormant-account
// reactivation). Rules evaluate funding/trade flow events against the
// account's recent history; findings open compliance cases (the SAR /
// surveillance-case workflow is sibling Task 21.3.21's — this service
// feeds it through the CaseSink seam) and page the ops channel at the
// finding's severity. Findings NEVER auto-clear flags and never mutate
// account state directly — dispositions belong to officers.
package compliance

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"exchange/pkg/decimal"
)

// FlowKind classifies a monitored funding/trade event.
type FlowKind string

const (
	FlowDeposit    FlowKind = "DEPOSIT"
	FlowWithdrawal FlowKind = "WITHDRAWAL"
	FlowTrade      FlowKind = "TRADE"
	FlowTransfer   FlowKind = "TRANSFER"
)

// FlowEvent is one monitored value movement — minor-unit amounts keep
// the rule arithmetic integer (funding convention).
type FlowEvent struct {
	AccountID int64     `json:"account_id"`
	Kind      FlowKind  `json:"kind"`
	Amount    int64     `json:"amount_minor"` // minor units
	Currency  string    `json:"currency"`     // ISO 4217
	At        time.Time `json:"at"`
	Ref       string    `json:"ref,omitempty"` // funding/transfer id for evidence
}

// MonitoringFinding is one rule breach — the case-system intake row.
type MonitoringFinding struct {
	AccountID  int64     `json:"account_id"`
	Rule       string    `json:"rule"`     // structuring_24h etc.
	Severity   string    `json:"severity"` // P1..P3
	Summary    string    `json:"summary"`
	Evidence   any       `json:"evidence"` // structured detail for the case
	DetectedAt time.Time `json:"detected_at"`
}

// ActivitySource is the read seam over flow history — funding +
// ledger history implement it; tests inject fixtures. Queries are
// bounded (window + per-account) so a monitoring sweep never scans
// the whole ledger.
type ActivitySource interface {
	// RecentFlows returns the account's funding/trade flows inside the
	// window (newest first). Window is inclusive of `now`.
	RecentFlows(ctx context.Context, accountID int64,
		window time.Duration, now time.Time) ([]FlowEvent, error)
	// LastActivityAt returns the most recent flow timestamp before
	// `before` (dormant-account detection) — zero time = no history.
	LastActivityAt(ctx context.Context, accountID int64,
		before time.Time) (time.Time, error)
	// ActiveAccounts returns account ids with any flow inside window —
	// the sweep's work set.
	ActiveAccounts(ctx context.Context, window time.Duration,
		now time.Time, limit int) ([]int64, error)
}

// CaseSink is the surveillance-case intake seam — Task 21.3.21's case
// management binds it; AuditCaseSink (below) is the built-in fallback
// that preserves findings in admin_audit_log so they are never lost.
type CaseSink interface {
	OpenCase(ctx context.Context, f MonitoringFinding) (caseID int64, err error)
}

// AuditCaseSink preserves findings as hash-chained audit rows when no
// dedicated case store is bound — providerless-verifiable.
type AuditCaseSink struct {
	Auditor AuditSink
}

// OpenCase writes the finding as an audit record; the returned id is 0
// (audit rows are keyed by their chain sequence, not a case id).
func (a AuditCaseSink) OpenCase(ctx context.Context,
	f MonitoringFinding) (int64, error) {
	if a.Auditor == nil {
		return 0, nil
	}
	return 0, a.Auditor.Record(ctx, f.AccountID,
		"monitoring.finding", "account", f.AccountID, f)
}

// MonitoringRule is one detection rule. Evaluate sees the triggering
// event plus the account's recent history; it returns zero or more
// findings (usually 0|1) — never mutates state.
type MonitoringRule interface {
	Name() string
	Evaluate(ctx context.Context, ev FlowEvent,
		history []FlowEvent, now time.Time) []MonitoringFinding
}

// Rule thresholds — conservative fiat defaults in minor units (cents);
// operators tune via the rule structs (kept as fields, not env globals,
// so tests pin them).
const (
	// CTR-structure window: sub-threshold deposits bunching over 24h.
	structuringWindow    = 24 * time.Hour
	defaultCTRMinor      = int64(1_000_000) // $10,000.00 in cents
	structuringBandMinor = int64(50_000)    // band: [$9,500, $10,000)
	structuringMinCount  = 3
	velocityWindow       = time.Hour
	velocityMaxCount     = 10
	velocitySpikeFactor  = 5 // >5× account mean flow → anomaly
	dormantThreshold     = 90 * 24 * time.Hour
	roundAmountStep      = int64(100_000) // $1,000 multiples
	roundAmountMinCount  = 3
)

// ---------------------------------------------------------------------------
// Rules
// ---------------------------------------------------------------------------

// StructuringRule flags bunching of just-below-reporting-threshold
// deposits over 24h (smurfing): ≥N deposits each below 95% of the CTR
// threshold whose sum exceeds the threshold.
type StructuringRule struct {
	ThresholdMinor int64
	SubThreshold   int64
	MinCount       int
	Window         time.Duration
}

// NewStructuringRule applies the defaults.
func NewStructuringRule() *StructuringRule {
	return &StructuringRule{ThresholdMinor: defaultCTRMinor,
		SubThreshold: structuringBandMinor, MinCount: structuringMinCount,
		Window: structuringWindow}
}

// Name identifies the rule in findings.
func (r *StructuringRule) Name() string { return "structuring_24h" }

// Evaluate counts qualifying deposits in-window.
func (r *StructuringRule) Evaluate(_ context.Context, ev FlowEvent,
	history []FlowEvent, now time.Time) []MonitoringFinding {
	if ev.Kind != FlowDeposit {
		return nil
	}
	var sum int64
	var evs []FlowEvent
	cut := now.Add(-r.Window)
	for _, h := range history {
		if h.Kind != FlowDeposit || h.At.Before(cut) {
			continue
		}
		if h.Amount < r.ThresholdMinor && h.Amount > 0 {
			sum += h.Amount
			evs = append(evs, h)
		}
	}
	if len(evs) < r.MinCount || sum < r.ThresholdMinor {
		return nil
	}
	// Bunching signature: the count of events landing in the
	// sub-threshold band [subBand, threshold).
	banded := 0
	low := r.ThresholdMinor - r.SubThreshold
	for _, h := range evs {
		if h.Amount >= low && h.Amount < r.ThresholdMinor {
			banded++
		}
	}
	if banded < r.MinCount {
		return nil
	}
	return []MonitoringFinding{{
		AccountID: ev.AccountID, Rule: r.Name(), Severity: "P1",
		Summary: fmt.Sprintf(
			"%d sub-threshold deposits in %v totalling %d minor units — possible structuring",
			banded, r.Window, sum),
		Evidence:   map[string]any{"events": evs, "window": r.Window.String()},
		DetectedAt: now,
	}}
}

// VelocityRule flags flow-count bursts and amount spikes vs the
// account's own baseline (avg daily flow over the window's history).
type VelocityRule struct {
	MaxPerHour  int
	SpikeFactor int64 // amount > spikeFactor × daily average
}

// NewVelocityRule applies defaults.
func NewVelocityRule() *VelocityRule {
	return &VelocityRule{MaxPerHour: velocityMaxCount,
		SpikeFactor: velocitySpikeFactor}
}

// Name identifies the rule.
func (r *VelocityRule) Name() string { return "velocity_burst" }

// Evaluate checks hour-window count + amount-vs-baseline.
func (r *VelocityRule) Evaluate(_ context.Context, ev FlowEvent,
	history []FlowEvent, now time.Time) []MonitoringFinding {
	hourCut := now.Add(-velocityWindow)
	count := 0
	for _, h := range history {
		if !h.At.Before(hourCut) {
			count++
		}
	}
	var findings []MonitoringFinding
	if count > r.MaxPerHour {
		findings = append(findings, MonitoringFinding{
			AccountID: ev.AccountID, Rule: r.Name(), Severity: "P2",
			Summary: fmt.Sprintf("%d flows in the last hour exceeds %d",
				count, r.MaxPerHour),
			Evidence:   map[string]any{"count": count, "window": "1h"},
			DetectedAt: now})
	}
	// Amount spike: event > spikeFactor × mean of prior daily totals.
	// Baseline needs ≥5 prior flows to be meaningful (else noise).
	var prior []int64
	for _, h := range history {
		if h.Ref != ev.Ref { // exclude the triggering event itself
			prior = append(prior, h.Amount)
		}
	}
	if len(prior) >= 5 && ev.Amount > 0 {
		var sum int64
		for _, a := range prior {
			sum += a
		}
		mean := sum / int64(len(prior))
		if mean > 0 && ev.Amount > mean*r.SpikeFactor {
			findings = append(findings, MonitoringFinding{
				AccountID: ev.AccountID, Rule: "velocity_amount_spike",
				Severity: "P2",
				Summary: fmt.Sprintf(
					"flow amount %d exceeds %d× account baseline (%d)",
					ev.Amount, r.SpikeFactor, mean),
				Evidence: map[string]any{"amount": ev.Amount,
					"baseline_mean": mean, "sample": len(prior)},
				DetectedAt: now})
		}
	}
	return findings
}

// RoundAmountRule flags repeated suspiciously round funding amounts
// (structuring tell).
type RoundAmountRule struct {
	StepMinor int64
	MinCount  int
	Window    time.Duration
}

// NewRoundAmountRule applies defaults (7d window).
func NewRoundAmountRule() *RoundAmountRule {
	return &RoundAmountRule{StepMinor: roundAmountStep,
		MinCount: roundAmountMinCount, Window: 7 * 24 * time.Hour}
}

// Name identifies the rule.
func (r *RoundAmountRule) Name() string { return "round_amount_cluster" }

// Evaluate counts round-step-multiple funding events in-window.
func (r *RoundAmountRule) Evaluate(_ context.Context, ev FlowEvent,
	history []FlowEvent, now time.Time) []MonitoringFinding {
	if ev.Kind == FlowTrade {
		return nil // trade notional rounding is normal market behavior
	}
	cut := now.Add(-r.Window)
	n := 0
	for _, h := range history {
		if h.Kind == FlowTrade || h.At.Before(cut) || h.Amount <= 0 {
			continue
		}
		if h.Amount%r.StepMinor == 0 && h.Amount >= r.StepMinor {
			n++
		}
	}
	if n < r.MinCount {
		return nil
	}
	return []MonitoringFinding{{
		AccountID: ev.AccountID, Rule: r.Name(), Severity: "P3",
		Summary: fmt.Sprintf(
			"%d round-amount funding events (multiples of %d minor units) in %v",
			n, r.StepMinor, r.Window),
		Evidence:   map[string]any{"count": n, "step": r.StepMinor},
		DetectedAt: now}}
}

// ---------------------------------------------------------------------------
// Service
// ---------------------------------------------------------------------------

// MonitoringService evaluates incoming flow events against the rules
// and sweeps for dormant reactivation. Findings go to the CaseSink +
// ops alerter + audit; nothing here touches account state.
type MonitoringService struct {
	src     ActivitySource
	rules   []MonitoringRule
	cases   CaseSink
	alerter Alerter
	now     func() time.Time

	dormant time.Duration // dormant-account threshold
	mu      sync.Mutex
	evals   int64
	hits    int64
}

// MonitoringOptions wires the service. Src + Cases are mandatory
// (AuditCaseSink is the honest fallback); Rules nil → defaults.
type MonitoringOptions struct {
	Src     ActivitySource
	Rules   []MonitoringRule
	Cases   CaseSink
	Alerter Alerter
	Now     func() time.Time
	Dormant time.Duration // 0 → 90d
}

// NewMonitoringService validates + binds.
func NewMonitoringService(o MonitoringOptions) (*MonitoringService, error) {
	if o.Src == nil {
		return nil, fmt.Errorf("compliance: monitoring source is nil")
	}
	if o.Cases == nil {
		return nil, fmt.Errorf("compliance: monitoring case sink is nil")
	}
	s := &MonitoringService{
		src: o.Src, rules: o.Rules, cases: o.Cases,
		alerter: o.Alerter, now: o.Now,
		dormant: o.Dormant,
	}
	if len(s.rules) == 0 {
		s.rules = []MonitoringRule{NewStructuringRule(), NewVelocityRule(),
			NewRoundAmountRule()}
	}
	if s.now == nil {
		s.now = func() time.Time { return time.Now().UTC() }
	}
	if s.dormant <= 0 {
		s.dormant = dormantThreshold
	}
	return s, nil
}

// WithCaseSink rebinds the finding sink post-construction — Task
// 21.3.21's CaseService is built after the SAR service it escalates
// into, which lands after the monitoring service in cmd/gateway, so
// the seam attaches here (the WithX pattern the codebase uses for
// forward references). Nil keeps the existing sink.
func (s *MonitoringService) WithCaseSink(c CaseSink) *MonitoringService {
	if c != nil {
		s.cases = c
	}
	return s
}

// EvaluateFlow runs every rule against one observed event — called by
// the funding pipeline hook (funding publishes settled flows) and by
// the sweep for replay. Returns the findings produced.
func (s *MonitoringService) EvaluateFlow(ctx context.Context,
	ev FlowEvent) ([]MonitoringFinding, error) {
	now := s.now()
	hist, err := s.src.RecentFlows(ctx, ev.AccountID, 7*24*time.Hour, now)
	if err != nil {
		return nil, fmt.Errorf("compliance: monitor history: %w", err)
	}
	// The triggering event participates in its own history — prepend if
	// the source doesn't already include it.
	found := false
	for _, h := range hist {
		if h.Ref != "" && h.Ref == ev.Ref {
			found = true
			break
		}
	}
	if !found {
		hist = append([]FlowEvent{ev}, hist...)
	}
	var out []MonitoringFinding
	for _, r := range s.rules {
		out = append(out, r.Evaluate(ctx, ev, hist, now)...)
	}
	s.mu.Lock()
	s.evals++
	s.mu.Unlock()
	for _, f := range out {
		f.DetectedAt = now
		if _, err := s.cases.OpenCase(ctx, f); err != nil {
			s.raise(ctx, "P2", "MONITORING_CASE_FAILED",
				fmt.Sprintf("case open failed for account %d rule %s: %v",
					f.AccountID, f.Rule, err))
		}
		s.raise(ctx, f.Severity, "MONITORING_"+strings.ToUpper(f.Rule),
			f.Summary)
	}
	if len(out) > 0 {
		s.mu.Lock()
		s.hits += int64(len(out))
		s.mu.Unlock()
	}
	return out, nil
}

// SweepDormant finds accounts whose last activity was ≥ dormant ago
// and which show a flow now — the dormant-reactivation pattern. The
// sweep is hourly-scale (same cadence class as the lifecycle
// reverify sweeps).
func (s *MonitoringService) SweepDormant(ctx context.Context,
	limit int) (int, error) {
	now := s.now()
	// Accounts active in the last dormant window-edge slice: we ask the
	// source for recently-active ids, then test each for a dormancy gap.
	recent, err := s.src.ActiveAccounts(ctx, 24*time.Hour, now, limit)
	if err != nil {
		return 0, fmt.Errorf("compliance: dormant sweep active set: %w", err)
	}
	hits := 0
	for _, id := range recent {
		prior, err := s.src.LastActivityAt(ctx, id, now.Add(-24*time.Hour))
		if err != nil {
			continue // one bad row never aborts the sweep
		}
		if prior.IsZero() || now.Sub(prior) <= s.dormant {
			continue
		}
		hits++
		f := MonitoringFinding{
			AccountID: id, Rule: "dormant_reactivation", Severity: "P2",
			Summary: fmt.Sprintf(
				"account reactivated after %.0f days dormant", now.Sub(prior).Hours()/24),
			Evidence:   map[string]any{"last_prior_activity": prior},
			DetectedAt: now}
		if _, err := s.cases.OpenCase(ctx, f); err != nil {
			s.raise(ctx, "P2", "MONITORING_CASE_FAILED",
				fmt.Sprintf("dormant case open failed account %d: %v", id, err))
		}
		s.raise(ctx, "P2", "MONITORING_DORMANT_REACTIVATION", f.Summary)
	}
	return hits, nil
}

// Stats exposes the eval/finding counters for the health surface.
func (s *MonitoringService) Stats() (evals, findings int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.evals, s.hits
}

func (s *MonitoringService) raise(ctx context.Context, sev, code, summary string) {
	if s.alerter != nil {
		_ = s.alerter(ctx, sev, code, summary)
	}
}

// ObserveFundingNotification interprets a funding Notifier emission
// (deposit_confirmed / withdrawal_completed — post-commit, best-effort)
// into a FlowEvent and evaluates it. Non-terminal events and unparsable
// payloads are ignored; evaluation failures alert, never propagate —
// the funding notification path must not see monitoring errors.
func (s *MonitoringService) ObserveFundingNotification(ctx context.Context,
	accountID int64, event string, payload map[string]any) {
	var kind FlowKind
	var ref string
	switch event {
	case "deposit_confirmed":
		kind, ref = FlowDeposit, fmt.Sprintf("deposit:%v", payload["deposit_id"])
	case "withdrawal_completed":
		kind, ref = FlowWithdrawal,
			fmt.Sprintf("withdrawal:%v", payload["withdrawal_id"])
	default:
		return // non-terminal event — not a monitored flow
	}
	amt, err := decimal.NewFromString(fmt.Sprintf("%v", payload["amount"]))
	if err != nil || !amt.IsPositive() {
		return
	}
	cur, _ := payload["currency"].(string)
	ev := FlowEvent{
		AccountID: accountID, Kind: kind,
		// major-unit decimal → fiat minor units (cents)
		Amount:   amt.Shift(2).Truncate(0).IntPart(),
		Currency: cur, At: s.now(), Ref: ref,
	}
	if _, err := s.EvaluateFlow(ctx, ev); err != nil {
		s.raise(ctx, "P2", "MONITORING_EVAL_FAILED",
			fmt.Sprintf("flow evaluation failed account %d: %v",
				accountID, err))
	}
}

// ---------------------------------------------------------------------------
// PgActivitySource — production ActivitySource over funding flows
// ---------------------------------------------------------------------------

// PgActivitySource reads the money-movement surface — settled
// funding_transactions (migration 007) plus completed internal
// transfers (migration 161) — read-only. Monitoring consumes
// CONFIRMED/COMPLETED rows only: pending/review rows are not flows.
type PgActivitySource struct {
	pool *pgxpool.Pool
}

// NewPgActivitySource binds the OLTP pool.
func NewPgActivitySource(pool *pgxpool.Pool) *PgActivitySource {
	return &PgActivitySource{pool: pool}
}

// flowRows returns funding_transactions + transfers inside the window
// as FlowEvents (amount converted to minor units in SQL).
const recentFlowsSQL = `
	SELECT account_id, type::text, ROUND(amount * 100)::bigint, currency,
	       COALESCE(completed_at, confirmed_at, created_at), 'ftx:' || id::text
	  FROM funding_transactions
	 WHERE account_id = $1 AND created_at >= $2
	   AND status IN ('CONFIRMED','COMPLETED')
	UNION ALL
	SELECT from_account_id, 'TRANSFER', ROUND(amount * 100)::bigint, currency,
	       COALESCE(completed_at, created_at), 'xfer:' || id::text
	  FROM transfers
	 WHERE from_account_id = $1 AND created_at >= $2 AND status = 'COMPLETED'
	 ORDER BY 5 DESC
	 LIMIT 500`

// RecentFlows implements ActivitySource.
func (p *PgActivitySource) RecentFlows(ctx context.Context, accountID int64,
	window time.Duration, now time.Time) ([]FlowEvent, error) {
	rows, err := p.pool.Query(ctx, recentFlowsSQL, accountID,
		now.Add(-window))
	if err != nil {
		return nil, fmt.Errorf("compliance: recent flows: %w", err)
	}
	defer rows.Close()
	var out []FlowEvent
	for rows.Next() {
		var f FlowEvent
		var kind string
		if err := rows.Scan(&f.AccountID, &kind, &f.Amount, &f.Currency,
			&f.At, &f.Ref); err != nil {
			return nil, err
		}
		f.Kind = FlowKind(kind)
		out = append(out, f)
	}
	return out, rows.Err()
}

// LastActivityAt implements ActivitySource — most recent settled flow
// strictly before `before`.
func (p *PgActivitySource) LastActivityAt(ctx context.Context, accountID int64,
	before time.Time) (time.Time, error) {
	var at *time.Time
	err := p.pool.QueryRow(ctx, `
		SELECT max(t) FROM (
			SELECT COALESCE(completed_at, confirmed_at, created_at) AS t
			  FROM funding_transactions
			 WHERE account_id = $1 AND created_at < $2
			   AND status IN ('CONFIRMED','COMPLETED')
			UNION ALL
			SELECT COALESCE(completed_at, created_at) FROM transfers
			 WHERE from_account_id = $1 AND created_at < $2
			   AND status = 'COMPLETED'
		) s`, accountID, before).Scan(&at)
	if err != nil {
		return time.Time{}, fmt.Errorf("compliance: last activity: %w", err)
	}
	if at == nil {
		return time.Time{}, nil
	}
	return *at, nil
}

// ActiveAccounts implements ActivitySource — accounts with any settled
// flow inside the window.
func (p *PgActivitySource) ActiveAccounts(ctx context.Context,
	window time.Duration, now time.Time, limit int) ([]int64, error) {
	if limit <= 0 {
		limit = 500
	}
	rows, err := p.pool.Query(ctx, `
		SELECT DISTINCT account_id FROM (
			SELECT account_id FROM funding_transactions
			 WHERE created_at >= $1
			   AND status IN ('CONFIRMED','COMPLETED')
			UNION
			SELECT from_account_id FROM transfers
			 WHERE created_at >= $1 AND status = 'COMPLETED'
		) s LIMIT $2`, now.Add(-window), limit)
	if err != nil {
		return nil, fmt.Errorf("compliance: active accounts: %w", err)
	}
	defer rows.Close()
	var out []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}
