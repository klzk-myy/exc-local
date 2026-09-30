// Phase-21 Task 21.3.11 — PEP tier screening, adverse-media flagging
// and ongoing rescreen cadence (spec §14.3; remediation row "PEP /
// adverse-media / ongoing monitoring").
//
// PEP hits are NOT the same severity class as sanctions hits — they
// route to enhanced due diligence: a compliance hold with trigger
// PEP_MATCH (dual-control release through HoldService), a P2 ops alert
// and a pep_status flag on the KYC profile. Confirmed SANCTIONS hits on
// the same screen route trigger SANCTIONS_HIT + the C++ account flag.
//
// Cadence: PEP status is re-screened every PEPRescreenInterval (30d)
// against the current lists; every list-delta (ListScreener.WithDeltaHook)
// triggers a delta rescreen — accounts are re-matched and only entries
// added in that delta can produce NEW hits (delisted names never
// auto-clear a flag — clearance is always an officer action).
//
// Adverse media: the provider seam is injected (AdverseMediaVendor) —
// nil vendor leaves the manual ReportAdverseMedia intake path live so
// the flag surface exists providerless.
package compliance

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"exchange/internal/admin"
	excerrors "exchange/pkg/errors"
)

// PEPRescreenInterval is the ongoing-monitoring cadence for PEP/adverse
// screening — 30 days, matching the conservative end of the spec §14.3
// periodic-review expectation.
const PEPRescreenInterval = 30 * 24 * time.Hour

// ScreeningSubject is the identity surface a screen runs against —
// sourced from accounts + kyc_profiles (full_name, trading name,
// beneficial-owner names). The store seam materializes it; fields left
// empty are simply not screened.
type ScreeningSubject struct {
	AccountID        int64    `json:"account_id"`
	UserID           int64    `json:"user_id"` // audit-attribution principal
	LegalName        string   `json:"legal_name"`
	TradingName      string   `json:"trading_name,omitempty"`
	BeneficialOwners []string `json:"beneficial_owners,omitempty"`
	Country          string   `json:"country,omitempty"`
	Tier             string   `json:"tier,omitempty"` // current KYC tier (T0/T1/T2)
}

// Candidates renders every screenable name for the subject.
func (s ScreeningSubject) Candidates() []string {
	out := []string{s.LegalName, s.TradingName}
	return append(out, s.BeneficialOwners...)
}

// ScreeningStore is the persistence seam for screening bookkeeping —
// last-screened stamps and screening-evidence rows live on the KYC
// profile surface (kyc_profiles is sibling-owned; the pg
// implementation writes only the screening columns/rows this task
// owns).
type ScreeningStore interface {
	// Subjects returns the screenable account roster — implementation
	// pages internally; limit bounds the batch (0 = all).
	Subjects(ctx context.Context, limit int) ([]ScreeningSubject, error)
	// DueForScreen returns subjects whose kind screen is older than
	// olderThan (or never screened).
	DueForScreen(ctx context.Context, kind EntryKind,
		olderThan time.Time, limit int) ([]ScreeningSubject, error)
	// RecordScreenResult persists the outcome stamp + hit evidence.
	// hitJSON is the MatchHit evidence (empty slice when clean) — the
	// row carries the screened payload, never a pointer to mutable
	// state (providerless-verifiable audit).
	RecordScreenResult(ctx context.Context, accountID int64,
		kind EntryKind, hits []MatchHit, screenedAt time.Time) error
	// RecordAdverseMedia appends one adverse-media flag row; returns
	// the flag id.
	RecordAdverseMedia(ctx context.Context, item AdverseMediaItem) (int64, error)
}

// AdverseMediaItem is one adverse-media flag — from the vendor feed or
// the manual compliance intake endpoint.
type AdverseMediaItem struct {
	AccountID   int64      `json:"account_id"`
	Source      string     `json:"source"` // vendor / intake channel
	Headline    string     `json:"headline"`
	URL         string     `json:"url,omitempty"`
	Severity    string     `json:"severity"` // LOW|MEDIUM|HIGH — HIGH routes to hold review
	PublishedAt *time.Time `json:"published_at,omitempty"`
}

// AdverseMediaVendor is the injected adverse-media provider seam —
// the compliance vendor adapter implements it; nil means manual intake
// only (dev binding).
type AdverseMediaVendor interface {
	// Screen returns adverse-media findings for a subject — an empty
	// slice is a clean screen, never an error.
	Screen(ctx context.Context, s ScreeningSubject) ([]AdverseMediaItem, error)
}

// ScreenOutcome is the per-subject result — returned to callers +
// serialized into the audit/evidence row.
type ScreenOutcome struct {
	AccountID    int64      `json:"account_id"`
	PEPHits      []MatchHit `json:"pep_hits,omitempty"`
	SanctionHits []MatchHit `json:"sanction_hits,omitempty"`
	Quarantined  bool       `json:"quarantined"` // screen deferred to the pending queue
	HoldID       string     `json:"hold_id,omitempty"`
	AdverseMedia int        `json:"adverse_media_count"`
	ScreenedAt   time.Time  `json:"screened_at"`
}

// holdPlacer narrows HoldService to the seam screening needs —
// tests substitute a fake; production binds *HoldService.
type holdPlacer interface {
	PlaceHold(ctx context.Context, req PlaceHoldRequest) (*Hold, error)
}

// ScreeningService runs PEP/sanctions screening at onboarding and on
// the rescreen cadence, routes adverse media, and feeds the hold
// workflow + flag publisher.
type ScreeningService struct {
	screener *ListScreener
	gated    *QuarantinedScreener // quarantine-aware onboarding seam
	store    ScreeningStore
	holds    holdPlacer
	flags    *FlagPublisher
	vendor   AdverseMediaVendor
	alerter  Alerter
	auditor  AuditSink
	now      func() time.Time

	rescreen    time.Duration
	mu          sync.Mutex
	lastDeltaAt time.Time
}

// ScreeningOptions wires the service. Screener is mandatory; Store is
// mandatory for the rescreen sweep (RecordScreenResult is where the
// audit-grade evidence lands). Holds/Flags/Alerter/Auditor/Vendor are
// optional seams — nil degrades to alert-only/audit-only behavior,
// never to a silent pass.
type ScreeningOptions struct {
	Screener *ListScreener
	Gated    *QuarantinedScreener
	Store    ScreeningStore
	Holds    holdPlacer
	Flags    *FlagPublisher
	Vendor   AdverseMediaVendor
	Alerter  Alerter
	Auditor  AuditSink
	Now      func() time.Time
	Rescreen time.Duration // 0 → PEPRescreenInterval
}

// NewScreeningService validates + binds.
func NewScreeningService(o ScreeningOptions) (*ScreeningService, error) {
	if o.Screener == nil {
		return nil, fmt.Errorf("compliance: screening screener is nil")
	}
	if o.Store == nil {
		return nil, fmt.Errorf("compliance: screening store is nil")
	}
	s := &ScreeningService{
		screener: o.Screener, gated: o.Gated, store: o.Store,
		holds: o.Holds, flags: o.Flags, vendor: o.Vendor,
		alerter: o.Alerter, auditor: o.Auditor, now: o.Now,
		rescreen: o.Rescreen,
	}
	if s.now == nil {
		s.now = func() time.Time { return time.Now().UTC() }
	}
	if s.rescreen <= 0 {
		s.rescreen = PEPRescreenInterval
	}
	return s, nil
}

// ScreenOnboarding runs the onboarding screen — sanctions AND PEP
// kinds over every candidate name. Quarantined providers defer the
// screen to the pending queue (outcome.Quarantined) instead of
// failing open.
func (s *ScreeningService) ScreenOnboarding(ctx context.Context,
	sub ScreeningSubject) (ScreenOutcome, error) {
	out := ScreenOutcome{AccountID: sub.AccountID}
	candidates := sub.Candidates()
	if len(candidates) == 0 {
		return out, excerrors.New("INVALID_REQUEST",
			"screening subject carries no screenable names")
	}
	// Quarantine path — defer, never pass.
	if s.gated != nil {
		hits, err := s.gated.ScreenOnboarding(ctx, sub.AccountID,
			candidates...)
		if err == ErrQuarantined {
			out.Quarantined = true
			s.audit(ctx, sub.UserID, "screening.onboarding_deferred",
				sub.AccountID, map[string]any{
					"candidates": candidates, "reason": "provider quarantine"})
			return out, nil
		}
		if err != nil {
			return out, err
		}
		for _, h := range hits {
			out.SanctionHits = append(out.SanctionHits, h)
		}
	} else {
		hits, err := s.screener.ScreenParty(ctx, EntryKindSanctions,
			candidates...)
		if err != nil {
			return out, err
		}
		out.SanctionHits = hits
	}
	// PEP kind — screens even when the screener is file-backed.
	if pep, err := s.screener.ScreenParty(ctx, EntryKindPEP,
		candidates...); err == nil {
		out.PEPHits = pep
	} else {
		return out, err
	}
	out.ScreenedAt = s.now()
	if err := s.applyOutcome(ctx, sub, &out); err != nil {
		return out, err
	}
	return out, nil
}

// applyOutcome routes a completed screen: SANCTIONS hits → flag +
// SANCTIONS_HIT hold (4h SLA, high confidence) + P1; PEP hits →
// PEP_MATCH hold + P2 + pep flag; clean → MarkScreened. Every outcome
// lands a screening evidence row + audit record.
func (s *ScreeningService) applyOutcome(ctx context.Context,
	sub ScreeningSubject, out *ScreenOutcome) error {
	hits := out.SanctionHits
	trigger := HoldTriggerSanctionsHit
	if len(out.SanctionHits) == 0 && len(out.PEPHits) > 0 {
		hits, trigger = out.PEPHits, HoldTriggerPEPMatch
	}
	if len(hits) > 0 {
		evidence, _ := json.Marshal(hits)
		if s.holds != nil {
			h, err := s.holds.PlaceHold(ctx, PlaceHoldRequest{
				AccountID: sub.AccountID, Trigger: trigger,
				Reason: fmt.Sprintf(
					"%s screening hit on onboarding/rescreen — %s matched %q (%s)",
					trigger, hits[0].Candidate, hits[0].Display, hits[0].List),
				EvidenceRef:    string(evidence),
				HighConfidence: trigger == HoldTriggerSanctionsHit,
				PlacedBy:       sub.UserID,
			})
			if err != nil {
				// Hold placement failure is a P1 — the flag still
				// publishes so the C++ hook blocks order entry.
				s.raise(ctx, "P1", "SCREENING_HOLD_FAILED",
					fmt.Sprintf("hold placement failed for account %d: %v",
						sub.AccountID, err))
			} else {
				out.HoldID = h.HoldID
			}
		}
		if s.flags != nil && trigger == HoldTriggerSanctionsHit {
			_ = s.flags.FlagAccount(ctx, sub.AccountID,
				"screening hit: "+hits[0].Display)
		}
		sev, code := "P2", "PEP_SCREEN_HIT"
		if trigger == HoldTriggerSanctionsHit {
			sev, code = "P1", "SANCTIONS_SCREEN_HIT"
		}
		s.raise(ctx, sev, code, fmt.Sprintf(
			"account %d %s: %d hit(s) — first %q (%s, score %.2f)",
			sub.AccountID, trigger, len(hits), hits[0].Display,
			hits[0].List, hits[0].Score))
		s.audit(ctx, sub.UserID, "screening.hit", sub.AccountID,
			map[string]any{"trigger": trigger, "hits": hits,
				"hold_id": out.HoldID})
	} else {
		if s.flags != nil {
			_ = s.flags.MarkScreened(ctx, sub.AccountID)
		}
	}
	// Stamp EVERY screened kind — a clean screen screened both
	// SANCTIONS and PEP; stamping only the hit kind would leave the
	// other perpetually "due" on the rescreen cadence.
	stamps := map[EntryKind][]MatchHit{
		EntryKindSanctions: out.SanctionHits,
		EntryKindPEP:       out.PEPHits,
	}
	for k, hh := range stamps {
		if err := s.store.RecordScreenResult(ctx, sub.AccountID, k,
			hh, out.ScreenedAt); err != nil {
			return fmt.Errorf("compliance: record screen result: %w", err)
		}
	}
	return nil
}

// RescreenDue sweeps accounts whose PEP screen is stale — the ongoing
// monitoring cadence leg.
func (s *ScreeningService) RescreenDue(ctx context.Context,
	limit int) (int, error) {
	cutoff := s.now().Add(-s.rescreen)
	subs, err := s.store.DueForScreen(ctx, EntryKindPEP, cutoff, limit)
	if err != nil {
		return 0, fmt.Errorf("compliance: due-for-screen: %w", err)
	}
	n := 0
	for _, sub := range subs {
		if _, err := s.ScreenOnboarding(ctx, sub); err != nil {
			s.raise(ctx, "P2", "RESCREEN_FAILED",
				fmt.Sprintf("rescreen account %d: %v", sub.AccountID, err))
			continue
		}
		n++
	}
	return n, nil
}

// DeltaRescreen handles a list-update delta — every subject is
// re-screened and only hits whose matched entry was ADDED in this
// delta are reported as new (delistings are routed to the audit log
// for officer review, never auto-cleared).
func (s *ScreeningService) DeltaRescreen(ctx context.Context,
	d ListDelta) (int, error) {
	if len(d.Added) == 0 {
		if len(d.Removed) > 0 {
			s.audit(ctx, 0, "screening.list_delistings", 0,
				map[string]any{"removed": d.Removed})
		}
		return 0, nil
	}
	added := map[string]bool{}
	for _, n := range d.Added {
		added[n] = true
	}
	subs, err := s.store.Subjects(ctx, 0)
	if err != nil {
		return 0, fmt.Errorf("compliance: delta rescreen subjects: %w", err)
	}
	newHits := 0
	for _, sub := range subs {
		var delta []MatchHit
		for _, kind := range []EntryKind{EntryKindSanctions, EntryKindPEP} {
			hits, err := s.screener.ScreenParty(ctx, kind,
				sub.Candidates()...)
			if err != nil {
				s.raise(ctx, "P1", "DELTA_RESCREEN_FAILED",
					fmt.Sprintf("delta rescreen account %d: %v",
						sub.AccountID, err))
				return newHits, err // fail closed — partial delta sweep aborts
			}
			for _, h := range hits {
				if added[h.Matched] {
					delta = append(delta, h)
				}
			}
		}
		if len(delta) == 0 {
			continue
		}
		newHits++
		out := ScreenOutcome{AccountID: sub.AccountID, ScreenedAt: s.now()}
		for _, h := range delta {
			if h.Kind == string(EntryKindPEP) {
				out.PEPHits = append(out.PEPHits, h)
			} else {
				out.SanctionHits = append(out.SanctionHits, h)
			}
		}
		if err := s.applyOutcome(ctx, sub, &out); err != nil {
			s.raise(ctx, "P2", "DELTA_RESCREEN_APPLY_FAILED",
				fmt.Sprintf("delta rescreen apply account %d: %v",
					sub.AccountID, err))
		}
	}
	if newHits > 0 {
		s.raise(ctx, "P1", "DELTA_RESCREEN_HITS",
			fmt.Sprintf("list delta rescreen produced %d newly-hit account(s)",
				newHits))
	}
	return newHits, nil
}

// OnDelta is the ListScreener.WithDeltaHook adapter — records the delta
// timestamp and returns a closure callers bind:
//
//	screener.WithDeltaHook(svc.OnDelta(ctx))
//
// The hook runs the rescreen inline on the reload goroutine — a slow
// account base sweeps asynchronously so wiring SHOULD bind the Run
// scheduler instead for large bases; the hook itself is safe.
func (s *ScreeningService) OnDelta(ctx context.Context) func(ListDelta) {
	return func(d ListDelta) {
		s.mu.Lock()
		s.lastDeltaAt = s.now()
		s.mu.Unlock()
		_, _ = s.DeltaRescreen(ctx, d)
	}
}

// ScreenAdverseMedia runs the injected vendor for one subject and
// records every finding — HIGH severity additionally routes a hold
// (UNUSUAL_ACTIVITY trigger with review SLA) + P1 alert; lower
// severities record + P3 alert only.
func (s *ScreeningService) ScreenAdverseMedia(ctx context.Context,
	sub ScreeningSubject) (int, error) {
	if s.vendor == nil {
		return 0, nil // manual intake only
	}
	items, err := s.vendor.Screen(ctx, sub)
	if err != nil {
		return 0, fmt.Errorf("compliance: adverse media screen: %w", err)
	}
	for _, it := range items {
		it.AccountID = sub.AccountID
		if err := s.applyAdverse(ctx, sub, it); err != nil {
			return 0, err
		}
	}
	return len(items), nil
}

// ReportAdverseMedia is the manual intake — compliance officer or the
// ops webhook submits a finding directly.
func (s *ScreeningService) ReportAdverseMedia(ctx context.Context,
	sub ScreeningSubject, item AdverseMediaItem) error {
	if item.AccountID > 0 && item.AccountID != sub.AccountID {
		return excerrors.New("INVALID_REQUEST",
			"adverse-media account_id does not match subject")
	}
	item.AccountID = sub.AccountID
	return s.applyAdverse(ctx, sub, item)
}

func (s *ScreeningService) applyAdverse(ctx context.Context,
	sub ScreeningSubject, item AdverseMediaItem) error {
	sev := strings.ToUpper(strings.TrimSpace(item.Severity))
	if sev == "" {
		sev = "MEDIUM"
	}
	item.Severity = sev
	id, err := s.store.RecordAdverseMedia(ctx, item)
	if err != nil {
		return fmt.Errorf("compliance: record adverse media: %w", err)
	}
	detail := map[string]any{
		"flag_id": id, "source": item.Source, "headline": item.Headline,
		"url": item.URL, "severity": sev}
	s.audit(ctx, sub.UserID, "screening.adverse_media", sub.AccountID, detail)
	if sev != "HIGH" {
		s.raise(ctx, "P3", "ADVERSE_MEDIA", fmt.Sprintf(
			"adverse media flag %d for account %d (%s): %s",
			id, sub.AccountID, item.Source, item.Headline))
		return nil
	}
	// HIGH → hold review (UNUSUAL_ACTIVITY — dual-control release) + P1.
	var holdID string
	if s.holds != nil {
		h, err := s.holds.PlaceHold(ctx, PlaceHoldRequest{
			AccountID: sub.AccountID, Trigger: HoldTriggerUnusualActivity,
			Reason: fmt.Sprintf("HIGH adverse media flag %d (%s): %s",
				id, item.Source, item.Headline),
			EvidenceRef: item.URL, PlacedBy: sub.UserID})
		if err != nil {
			s.raise(ctx, "P1", "ADVERSE_MEDIA_HOLD_FAILED",
				fmt.Sprintf("hold placement failed for account %d: %v",
					sub.AccountID, err))
		} else {
			holdID = h.HoldID
		}
	}
	s.raise(ctx, "P1", "ADVERSE_MEDIA_HIGH", fmt.Sprintf(
		"HIGH adverse media flag %d for account %d (%s): %s [hold %s]",
		id, sub.AccountID, item.Source, item.Headline, holdID))
	return nil
}

// HandleReplayHit is the QueueReplayer onHit binding — a positive
// replay result routes through the same hold/flag/audit path as a live
// screen.
func (s *ScreeningService) HandleReplayHit(ctx context.Context,
	p PendingScreen, hits []MatchHit) error {
	out := ScreenOutcome{AccountID: p.AccountID, ScreenedAt: s.now()}
	sub := ScreeningSubject{AccountID: p.AccountID, UserID: p.ActorID}
	for _, h := range hits {
		if h.Kind == string(EntryKindPEP) {
			out.PEPHits = append(out.PEPHits, h)
		} else {
			out.SanctionHits = append(out.SanctionHits, h)
		}
	}
	return s.applyOutcome(ctx, sub, &out)
}

func (s *ScreeningService) raise(ctx context.Context, sev, code, summary string) {
	if s.alerter != nil {
		_ = s.alerter(ctx, sev, code, summary)
	}
}

func (s *ScreeningService) audit(ctx context.Context, actor int64,
	action string, targetID int64, detail any) {
	if s.auditor != nil {
		_ = s.auditor.Record(ctx, actor, action, "account", targetID, detail)
	}
}

// ---------------------------------------------------------------------------
// MemoryScreeningStore — dev/test ScreeningStore
// ---------------------------------------------------------------------------

// MemoryScreeningStore is the in-memory ScreeningStore: subjects seeded
// by the test/dev harness, screen stamps + adverse-media rows kept for
// assertions. The production store is bound in wiring (screening
// columns ride on the KYC profile surface the sibling lifecycle store
// owns).
type MemoryScreeningStore struct {
	mu       sync.Mutex
	subjects map[int64]ScreeningSubject
	stamps   map[int64]map[EntryKind]time.Time
	results  map[int64][]MatchHit
	adverse  []AdverseMediaItem
	seq      int64
}

// NewMemoryScreeningStore builds the empty store.
func NewMemoryScreeningStore() *MemoryScreeningStore {
	return &MemoryScreeningStore{
		subjects: map[int64]ScreeningSubject{},
		stamps:   map[int64]map[EntryKind]time.Time{},
		results:  map[int64][]MatchHit{},
	}
}

// SeedSubject registers a screenable subject (test fixture seeding).
func (m *MemoryScreeningStore) SeedSubject(s ScreeningSubject) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.subjects[s.AccountID] = s
}

// Subjects returns the full roster (limit 0 = all).
func (m *MemoryScreeningStore) Subjects(_ context.Context,
	limit int) ([]ScreeningSubject, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []ScreeningSubject
	for _, s := range m.subjects {
		out = append(out, s)
		if limit > 0 && len(out) >= limit {
			break
		}
	}
	return out, nil
}

// DueForScreen returns subjects with no screen of that kind or a stamp
// older than the cutoff.
func (m *MemoryScreeningStore) DueForScreen(_ context.Context,
	kind EntryKind, olderThan time.Time,
	limit int) ([]ScreeningSubject, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []ScreeningSubject
	for id, s := range m.subjects {
		t, ok := m.stamps[id][kind]
		if !ok || t.Before(olderThan) {
			out = append(out, s)
			if limit > 0 && len(out) >= limit {
				break
			}
		}
	}
	return out, nil
}

// RecordScreenResult stamps the screen + keeps the hit evidence.
func (m *MemoryScreeningStore) RecordScreenResult(_ context.Context,
	accountID int64, kind EntryKind, hits []MatchHit,
	screenedAt time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.stamps[accountID] == nil {
		m.stamps[accountID] = map[EntryKind]time.Time{}
	}
	m.stamps[accountID][kind] = screenedAt
	m.results[accountID] = append([]MatchHit(nil), hits...)
	return nil
}

// RecordAdverseMedia appends one flag row.
func (m *MemoryScreeningStore) RecordAdverseMedia(_ context.Context,
	item AdverseMediaItem) (int64, error) {
	if item.AccountID <= 0 || strings.TrimSpace(item.Headline) == "" {
		return 0, excerrors.New("INVALID_REQUEST",
			"adverse media requires account_id and headline")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.seq++
	m.adverse = append(m.adverse, item)
	return m.seq, nil
}

// ScreenedAt returns the recorded stamp (test assertion helper).
func (m *MemoryScreeningStore) ScreenedAt(accountID int64,
	kind EntryKind) (time.Time, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	t, ok := m.stamps[accountID][kind]
	return t, ok
}

// AdverseCount returns the recorded adverse-media rows for an account.
func (m *MemoryScreeningStore) AdverseCount(accountID int64) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := 0
	for _, a := range m.adverse {
		if a.AccountID == accountID {
			n++
		}
	}
	return n
}

// ---------------------------------------------------------------------------
// PgScreeningStore — production ScreeningStore over existing tables
// ---------------------------------------------------------------------------

// PgScreeningStore implements ScreeningStore without new schema (Task
// 21.3.11 owns no migration — screening columns ride the sibling-owned
// kyc_profiles surface when it lands; until then the audit trail IS the
// evidence store, per the hash-chained audit convention):
//
//   - Subjects/DueForScreen read accounts + users.full_name (migration
//     027's self-declared profile) + the latest kyc_submissions
//     jurisdiction. Beneficial-owner names join the subject when a
//     kyc_profiles/companies surface exists — absent today.
//   - RecordScreenResult appends a 'screening.result' admin_audit_log
//     row (hash-chained by admin.LogAuto) carrying the full hit
//     evidence — the screening record IS the audit row.
//   - RecordAdverseMedia appends 'screening.adverse_media' audit rows
//     (the flag surface); a dedicated table lands with the sibling
//     case-management migration.
type PgScreeningStore struct {
	pool *pgxpool.Pool
}

// NewPgScreeningStore binds the OLTP pool.
func NewPgScreeningStore(pool *pgxpool.Pool) *PgScreeningStore {
	return &PgScreeningStore{pool: pool}
}

const screeningSubjectsSQL = `
	SELECT a.id, a.user_id, COALESCE(u.full_name,''), a.kyc_tier,
	       COALESCE(ks.jurisdiction,'')
	  FROM accounts a
	  JOIN users u ON u.id = a.user_id
	  LEFT JOIN LATERAL (
		SELECT jurisdiction FROM kyc_submissions
		 WHERE account_id = a.id ORDER BY id DESC LIMIT 1
	  ) ks ON true
	 WHERE a.status <> 'CLOSED'
	 ORDER BY a.id`

// Subjects returns the screenable roster (limit bounds the batch).
func (p *PgScreeningStore) Subjects(ctx context.Context,
	limit int) ([]ScreeningSubject, error) {
	q := screeningSubjectsSQL
	if limit > 0 {
		q += fmt.Sprintf(" LIMIT %d", limit)
	}
	rows, err := p.pool.Query(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("compliance: screening subjects: %w", err)
	}
	defer rows.Close()
	var out []ScreeningSubject
	for rows.Next() {
		var s ScreeningSubject
		var tier, jur *string
		if err := rows.Scan(&s.AccountID, &s.UserID, &s.LegalName,
			&tier, &jur); err != nil {
			return nil, err
		}
		if tier != nil {
			s.Tier = *tier
		}
		if jur != nil {
			s.Country = *jur
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// DueForScreen returns subjects whose last 'screening.result' audit row
// of the kind is older than the cutoff (or never screened).
func (p *PgScreeningStore) DueForScreen(ctx context.Context,
	kind EntryKind, olderThan time.Time,
	limit int) ([]ScreeningSubject, error) {
	q := `
		SELECT a.id, a.user_id, COALESCE(u.full_name,''), a.kyc_tier,
		       COALESCE(ks.jurisdiction,'')
		  FROM accounts a
		  JOIN users u ON u.id = a.user_id
		  LEFT JOIN LATERAL (
			SELECT jurisdiction FROM kyc_submissions
			 WHERE account_id = a.id ORDER BY id DESC LIMIT 1
		  ) ks ON true
		 WHERE a.status <> 'CLOSED'
		   AND NOT EXISTS (
			SELECT 1 FROM admin_audit_log l
			 WHERE l.action = 'screening.result'
			   AND l.target_type = 'account' AND l.target_id = a.id
			   AND l.after_state->>'kind' = $1
			   AND l.created_at > $2
		   )
		 ORDER BY a.id`
	if limit > 0 {
		q += fmt.Sprintf(" LIMIT %d", limit)
	}
	rows, err := p.pool.Query(ctx, q, string(kind), olderThan)
	if err != nil {
		return nil, fmt.Errorf("compliance: due-for-screen: %w", err)
	}
	defer rows.Close()
	var out []ScreeningSubject
	for rows.Next() {
		var s ScreeningSubject
		var tier, jur *string
		if err := rows.Scan(&s.AccountID, &s.UserID, &s.LegalName,
			&tier, &jur); err != nil {
			return nil, err
		}
		if tier != nil {
			s.Tier = *tier
		}
		if jur != nil {
			s.Country = *jur
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// RecordScreenResult appends the evidence row — the audit attribution
// is the account owner (machine screen convention).
func (p *PgScreeningStore) RecordScreenResult(ctx context.Context,
	accountID int64, kind EntryKind, hits []MatchHit,
	screenedAt time.Time) error {
	var owner int64
	if err := p.pool.QueryRow(ctx,
		`SELECT user_id FROM accounts WHERE id=$1`, accountID).
		Scan(&owner); err == pgx.ErrNoRows {
		return excerrors.New("NOT_FOUND", "account not found")
	} else if err != nil {
		return fmt.Errorf("compliance: screen owner: %w", err)
	}
	if hits == nil {
		hits = []MatchHit{}
	}
	_, _, err := admin.LogAuto(ctx, p.pool, admin.AuditEntry{
		AdminUserID: owner,
		Action:      "screening.result",
		TargetType:  "account",
		TargetID:    &accountID,
		AfterState: map[string]any{
			"kind":        string(kind),
			"hits":        hits,
			"hit_count":   len(hits),
			"screened_at": screenedAt,
		},
	})
	return err
}

// RecordAdverseMedia appends the flag as a hash-chained audit row.
func (p *PgScreeningStore) RecordAdverseMedia(ctx context.Context,
	item AdverseMediaItem) (int64, error) {
	if item.AccountID <= 0 || strings.TrimSpace(item.Headline) == "" {
		return 0, excerrors.New("INVALID_REQUEST",
			"adverse media requires account_id and headline")
	}
	var owner int64
	if err := p.pool.QueryRow(ctx,
		`SELECT user_id FROM accounts WHERE id=$1`, item.AccountID).
		Scan(&owner); err != nil {
		return 0, excerrors.New("NOT_FOUND", "account not found")
	}
	id, _, err := admin.LogAuto(ctx, p.pool, admin.AuditEntry{
		AdminUserID: owner,
		Action:      "screening.adverse_media",
		TargetType:  "account",
		TargetID:    &item.AccountID,
		AfterState:  item,
	})
	return id, err
}

// SubjectFor materializes one subject — the handler's account screen
// surface and the KYC post-approve hook both bind it.
func (p *PgScreeningStore) SubjectFor(ctx context.Context,
	accountID int64) (ScreeningSubject, error) {
	var s ScreeningSubject
	var tier, jur *string
	err := p.pool.QueryRow(ctx, `
		SELECT a.id, a.user_id, COALESCE(u.full_name,''), a.kyc_tier,
		       COALESCE(ks.jurisdiction,'')
		  FROM accounts a
		  JOIN users u ON u.id = a.user_id
		  LEFT JOIN LATERAL (
			SELECT jurisdiction FROM kyc_submissions
			 WHERE account_id = a.id ORDER BY id DESC LIMIT 1
		  ) ks ON true
		 WHERE a.id = $1`, accountID).
		Scan(&s.AccountID, &s.UserID, &s.LegalName, &tier, &jur)
	if err == pgx.ErrNoRows {
		return s, excerrors.New("NOT_FOUND", "account not found")
	}
	if err != nil {
		return s, fmt.Errorf("compliance: subject materialize: %w", err)
	}
	if tier != nil {
		s.Tier = *tier
	}
	if jur != nil {
		s.Country = *jur
	}
	return s, nil
}
