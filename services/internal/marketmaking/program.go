// Package marketmaking implements the Phase-18 Task 18.3.10 market-maker
// program (spec §5.27, §9.6, §24 #139):
//
//   - program.go        — enrollment/admin lifecycle and the quoting
//     entitlement lookup consumed by the FIX mass-quoting path
//     (internal/fix/quoting.go) plus the §9.6 OTR allowance consumed by
//     risk.OtrMonitor (Task 13.3.6).
//   - compliance.go     — per-minute quoting-obligation sampling (two-
//     sided size/spread presence) rolled into the mm_compliance daily
//     row, the rolling 3-breaches-in-7-days suspension rule, and the
//     admin alert on breach/suspension.
//   - mmp.go            — Market-Maker-Protection sliding-window fill
//     counter; on mmp_max_fills inside mmp_window_ms the tracker
//     mass-cancels that MM's quotes on the instrument and holds an
//     MMP_LOCKED_OUT lockout until an explicit reset.
//   - rebate.go         — maker-rebate accrual per fill and the monthly
//     GL posting through the JournalPoster seam (§9.6: "reconciled via
//     the double-entry GL").
//
// Persistence lives in migration 045 (mm_programs, mm_compliance,
// mm_rebate_accruals). The package never mutates balances directly —
// rebates post exclusively through the ledger path (spec §5.3 zero
// GL-bypass invariant).
package marketmaking

import (
	"context"
	"fmt"
	"sync"
	"time"

	"exchange/internal/ledger"
	"exchange/internal/observability"
	"exchange/pkg/decimal"
	excerrors "exchange/pkg/errors"
)

// Program status vocabulary (mm_program_status_enum, migration 045).
type Status string

const (
	StatusActive    Status = "ACTIVE"
	StatusSuspended Status = "SUSPENDED"
)

// Canonical rejection codes — spec §23 registry rows.
const (
	// CodeMMObligationBreach — 429; obligation/MMP breach events.
	CodeMMObligationBreach = "MM_OBLIGATION_BREACH"
	// CodeMMPLockedOut — 403; new quotes rejected while an MMP trigger
	// lockout stands (explicit reset required, §24.x MM Program row).
	CodeMMPLockedOut = "MMP_LOCKED_OUT"
	// CodeMMPTriggered — 429; the MMP mass-cancel trigger fired.
	CodeMMPTriggered = "MMP_TRIGGERED"
	// CodeQuoteRequestRejected — 400; malformed/non-firm mass quote.
	CodeQuoteRequestRejected = "QUOTE_REQUEST_REJECTED"
	// CodeMMNotEntitled — 403; account holds no ACTIVE program covering
	// the instrument (rides SESSION_NOT_ENTITLED — already registered).
	CodeSessionNotEntitled = "SESSION_NOT_ENTITLED"
	// CodeMMProgramNotFound — 404.
	CodeMMProgramNotFound = "NOT_FOUND"
	// CodeMMInvalid — 400 invalid enrollment/update payload.
	CodeMMInvalid = "INVALID_REQUEST"
	// CodeMMInternal wraps store failures.
	CodeMMInternal = "INTERNAL_ERROR"
)

// BreachSuspendWindow / BreachSuspendThreshold pin the task's rolling
// suspension rule: 3 daily compliance failures inside a rolling week
// suspends the program for Risk-Manager review.
const (
	BreachSuspendWindow    = 7 * 24 * time.Hour
	BreachSuspendThreshold = 3
)

// Program mirrors one mm_programs row. InstrumentID nil = program-wide
// (covers every instrument the account quotes); a set id binds one
// instrument (per-instrument override).
type Program struct {
	ID           int64            `json:"id"`
	AccountID    int64            `json:"account_id"`
	InstrumentID *int64           `json:"instrument_id,omitempty"`
	Symbol       string           `json:"symbol,omitempty"` // resolved on load (join); not a column
	MinQuoteSize decimal.Decimal  `json:"min_quote_size"`
	MaxSpreadBps decimal.Decimal  `json:"max_spread_bps"`
	PresencePct  decimal.Decimal  `json:"presence_pct"`
	MMPMaxFills  int              `json:"mmp_max_fills"`
	MMPWindowMs  int              `json:"mmp_window_ms"`
	RebateBps    decimal.Decimal  `json:"rebate_bps"`
	OtrAllowance *decimal.Decimal `json:"otr_allowance,omitempty"`
	Status       Status           `json:"status"`
	CreatedAt    time.Time        `json:"created_at"`
	UpdatedAt    time.Time        `json:"updated_at"`
}

// Covers reports whether the program binds instrumentID (program-wide
// rows cover everything).
func (p *Program) Covers(instrumentID int64) bool {
	return p.InstrumentID == nil || *p.InstrumentID == instrumentID
}

// MMPWindow returns the sliding MMP measurement window.
func (p *Program) MMPWindow() time.Duration {
	return time.Duration(p.MMPWindowMs) * time.Millisecond
}

// ComplianceRow mirrors one mm_compliance daily rollup row.
type ComplianceRow struct {
	ID               int64            `json:"id"`
	ProgramID        int64            `json:"program_id"`
	Day              time.Time        `json:"day"`
	SamplesTotal     int              `json:"samples_total"`
	SamplesCompliant int              `json:"samples_compliant"`
	PresencePct      *decimal.Decimal `json:"presence_pct,omitempty"`
	Breach           bool             `json:"breach"`
	BreachReason     string           `json:"breach_reason,omitempty"`
	CreatedAt        time.Time        `json:"created_at"`
	UpdatedAt        time.Time        `json:"updated_at"`
}

// RebateAccrual mirrors one mm_rebate_accruals row — a per-fill maker
// rebate held until the monthly GL posting stamps posted_journal_id.
type RebateAccrual struct {
	ID              int64           `json:"id"`
	ProgramID       int64           `json:"program_id"`
	AccountID       int64           `json:"account_id"`
	InstrumentID    int64           `json:"instrument_id"`
	FillRef         string          `json:"fill_ref"` // caller idempotency token (engine fill seq / order+qty ref)
	Day             time.Time       `json:"day"`
	Currency        string          `json:"currency"`
	Amount          decimal.Decimal `json:"amount"`
	PostedJournalID *int64          `json:"posted_journal_id,omitempty"`
	CreatedAt       time.Time       `json:"created_at"`
}

// Store is the persistence seam — *PgStore satisfies it; unit tests
// substitute a fake.
type Store interface {
	CreateProgram(ctx context.Context, p *Program) error
	UpdateProgram(ctx context.Context, p *Program) error
	ProgramByID(ctx context.Context, id int64) (*Program, error)
	// ProgramsFor returns every program row for accountID (any status)
	// covering instrumentID: the per-instrument row AND the program-wide
	// row. Callers pick per specificity/activity.
	ProgramsFor(ctx context.Context, accountID, instrumentID int64) ([]Program, error)
	ListPrograms(ctx context.Context, accountID int64, status Status) ([]Program, error)
	// InstrumentBySymbol resolves the quoting path's symbol →
	// instrument row (id + quote currency for rebate denomination).
	InstrumentBySymbol(ctx context.Context, symbol string) (id int64, quoteCcy string, err error)
	InstrumentSymbol(ctx context.Context, instrumentID int64) (string, error)
	QuoteCurrency(ctx context.Context, instrumentID int64) (string, error)

	// RecordSample upserts today's mm_compliance row for programID and
	// increments samples_total (+ samples_compliant when compliant).
	RecordSample(ctx context.Context, programID int64, day time.Time, compliant bool) error
	// FinalizeCompliance writes the rollup verdict for (program, day):
	// presence_pct and the breach flag + reason.
	FinalizeCompliance(ctx context.Context, programID int64, day time.Time,
		presence decimal.Decimal, breach bool, reason string) error
	ComplianceRows(ctx context.Context, programID int64, from, to time.Time) ([]ComplianceRow, error)
	BreachCount(ctx context.Context, programID int64, since time.Time) (int, error)

	// AccrueRebate inserts one per-fill accrual; a replayed fillRef is a
	// no-op (UNIQUE program_id+fill_ref dedup).
	AccrueRebate(ctx context.Context, a RebateAccrual) error
	UnpostedRebates(ctx context.Context) ([]RebateAccrual, error)
	MarkRebatesPosted(ctx context.Context, ids []int64, journalID int64) error
	ListRebates(ctx context.Context, programID int64, limit int) ([]RebateAccrual, error)
}

// JournalPoster is the GL posting seam — *settlement.LedgerService
// satisfies it (same shape as settlement.FeeService's poster).
type JournalPoster interface {
	Post(ctx context.Context, j ledger.Journal) (ledger.PostResult, error)
}

// Service is the market-maker program domain core.
type Service struct {
	store  Store
	poster JournalPoster      // nil → accrual-only (PostAccruedRebates refuses)
	sink   observability.Sink // optional alert sink (P2 on breach/suspension)
	logf   func(format string, args ...any)
	now    func() time.Time

	// programs is the in-memory ACTIVE snapshot feeding the OTR
	// allowance lookup — OtrAllowance is consulted on every order event,
	// so it must never hit PG inline. Load() refreshes it.
	mu       sync.RWMutex
	active   map[int64][]Program // accountID → ACTIVE programs (rows carry Symbol)
	loadedAt time.Time
}

// Options customizes construction; nil/zero fields pick defaults.
type Options struct {
	Poster JournalPoster // GL posting seam for the monthly rebate sweep
	Alerts observability.Sink
	Logger func(format string, args ...any)
	Now    func() time.Time
}

// NewService wires the service; store is required (fail-closed: an
// entitlement check that cannot resolve a program must never silently
// admit quoting).
func NewService(store Store, o Options) *Service {
	s := &Service{store: store, poster: o.Poster, sink: o.Alerts, now: o.Now, logf: o.Logger}
	if s.now == nil {
		s.now = time.Now
	}
	if s.logf == nil {
		s.logf = func(string, ...any) {}
	}
	s.active = map[int64][]Program{}
	return s
}

// Load refreshes the ACTIVE program snapshot (with instrument symbols
// resolved for the OTR allowance path). Store errors abort the load —
// the last good snapshot stays live (entitlement checks degrade to the
// stale set; a stale entitlement is fail-closed in the restrictive
// direction only when programs get stricter — suspension is read
// through ProgramsFor/Entitled against PG, never the cache).
func (s *Service) Load(ctx context.Context) error {
	rows, err := s.store.ListPrograms(ctx, 0, StatusActive)
	if err != nil {
		return fmt.Errorf("mm: load programs: %w", err)
	}
	byAcct := map[int64][]Program{}
	for _, p := range rows {
		if p.InstrumentID != nil && p.Symbol == "" {
			if sym, serr := s.store.InstrumentSymbol(ctx, *p.InstrumentID); serr == nil {
				p.Symbol = sym
			}
		}
		byAcct[p.AccountID] = append(byAcct[p.AccountID], p)
	}
	s.mu.Lock()
	s.active = byAcct
	s.loadedAt = s.now()
	s.mu.Unlock()
	return nil
}

// StartRefresher reloads the ACTIVE snapshot every interval until ctx
// is cancelled — same ticker pattern as risk.LimitsService.
func (s *Service) StartRefresher(ctx context.Context, interval time.Duration, onErr func(error)) {
	if interval <= 0 {
		interval = 60 * time.Second
	}
	go func() {
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				if err := s.Load(ctx); err != nil && onErr != nil {
					onErr(err)
				}
			}
		}
	}()
}

// OtrAllowance implements the risk.OtrMonitor MM-allowance seam (Task
// 13.3.6 / §9.6): the registered MM's configured ratio for the
// (account, symbol) pair, or the zero value when no ACTIVE program
// covers it (the monitor then applies venue/risk_limits ratios — the
// absence of an allowance can only tighten, never loosen, enforcement:
// fail-closed). Per-instrument program rows win over program-wide.
func (s *Service) OtrAllowance(_ context.Context, accountID int64, symbol string) *decimal.Decimal {
	s.mu.RLock()
	rows := append([]Program(nil), s.active[accountID]...)
	s.mu.RUnlock()
	var wide, specific *decimal.Decimal
	for i := range rows {
		p := &rows[i]
		if p.OtrAllowance == nil {
			continue
		}
		if p.InstrumentID == nil {
			if wide == nil || p.OtrAllowance.GreaterThan(*wide) {
				wide = p.OtrAllowance
			}
			continue
		}
		if p.Symbol == symbol && (specific == nil || p.OtrAllowance.GreaterThan(*specific)) {
			specific = p.OtrAllowance
		}
	}
	if specific != nil {
		return specific
	}
	return wide
}

// Entitled resolves the ACTIVE program covering (accountID,
// instrumentID) for the mass-quoting entitlement check — most specific
// row wins (per-instrument > program-wide). A nil program + nil error
// means no coverage; callers reject SESSION_NOT_ENTITLED.
func (s *Service) Entitled(ctx context.Context, accountID, instrumentID int64) (*Program, error) {
	rows, err := s.store.ProgramsFor(ctx, accountID, instrumentID)
	if err != nil {
		return nil, excerrors.Wrap(CodeMMInternal, "mm program lookup", err)
	}
	var wide, specific *Program
	for i := range rows {
		p := &rows[i]
		if p.Status != StatusActive {
			continue
		}
		if p.InstrumentID == nil {
			wide = p
		} else {
			specific = p
		}
	}
	if specific != nil {
		return specific, nil
	}
	return wide, nil
}

// ---------------------------------------------------------------------------
// Admin lifecycle — enrollment / update / suspend / resume
// ---------------------------------------------------------------------------

// Validate enforces the enrollment payload invariants (mirrors the
// migration-045 CHECKs so bad rows reject before the write).
func (p *Program) Validate() error {
	if p.AccountID <= 0 {
		return excerrors.New(CodeMMInvalid, "account_id must be positive")
	}
	if !p.MinQuoteSize.IsPositive() {
		return excerrors.New(CodeMMInvalid, "min_quote_size must be positive")
	}
	if !p.MaxSpreadBps.IsPositive() {
		return excerrors.New(CodeMMInvalid, "max_spread_bps must be positive")
	}
	if !p.PresencePct.IsPositive() || p.PresencePct.GreaterThan(decimal.NewFromInt(100)) {
		return excerrors.New(CodeMMInvalid, "presence_pct must be in (0, 100]")
	}
	if p.MMPMaxFills <= 0 {
		return excerrors.New(CodeMMInvalid, "mmp_max_fills must be positive")
	}
	// §24.x MM Program row: the sliding window is Δt ∈ [100ms, 5000ms].
	if p.MMPWindowMs < 100 || p.MMPWindowMs > 5000 {
		return excerrors.New(CodeMMInvalid,
			"mmp_window_ms must be within [100, 5000] (spec §24 MMP sliding window)")
	}
	if p.RebateBps.IsNegative() {
		return excerrors.New(CodeMMInvalid, "rebate_bps must be ≥ 0")
	}
	if p.OtrAllowance != nil && !p.OtrAllowance.IsPositive() {
		return excerrors.New(CodeMMInvalid, "otr_allowance must be positive when set")
	}
	return nil
}

// Enroll registers a new program (status ACTIVE). The unique
// (account_id, instrument_id NULLS NOT DISTINCT) index makes re-enroll
// idempotent-failing — a duplicate surfaces as a store error.
func (s *Service) Enroll(ctx context.Context, p *Program) error {
	p.Status = StatusActive
	if err := p.Validate(); err != nil {
		return err
	}
	if err := s.store.CreateProgram(ctx, p); err != nil {
		return excerrors.Wrap(CodeMMInternal, "mm program create", err)
	}
	return nil
}

// Update rewrites program parameters (obligations/MMP/rebate/allowance)
// without touching status — suspension lifecycle goes through
// Suspend/Resume.
func (s *Service) Update(ctx context.Context, p *Program) error {
	cur, err := s.store.ProgramByID(ctx, p.ID)
	if err != nil {
		return excerrors.Wrap(CodeMMInternal, "mm program read", err)
	}
	if cur == nil {
		return excerrors.New(CodeMMProgramNotFound,
			fmt.Sprintf("mm program %d not found", p.ID))
	}
	if err := p.Validate(); err != nil {
		return err
	}
	p.Status = cur.Status // status is transition-gated, not field-updated
	if err := s.store.UpdateProgram(ctx, p); err != nil {
		return excerrors.Wrap(CodeMMInternal, "mm program update", err)
	}
	return nil
}

// Get returns one program.
func (s *Service) Get(ctx context.Context, id int64) (*Program, error) {
	p, err := s.store.ProgramByID(ctx, id)
	if err != nil {
		return nil, excerrors.Wrap(CodeMMInternal, "mm program read", err)
	}
	if p == nil {
		return nil, excerrors.New(CodeMMProgramNotFound,
			fmt.Sprintf("mm program %d not found", id))
	}
	return p, nil
}

// List returns programs filtered by account (0 = all) and status
// ("" = any).
func (s *Service) List(ctx context.Context, accountID int64, status Status) ([]Program, error) {
	rows, err := s.store.ListPrograms(ctx, accountID, status)
	if err != nil {
		return nil, excerrors.Wrap(CodeMMInternal, "mm program list", err)
	}
	return rows, nil
}

// Suspend halts a program (Risk-Manager review state): quoting
// entitlement stops (Entitled skips non-ACTIVE rows) and rebate
// accrual pauses per the task text.
func (s *Service) Suspend(ctx context.Context, id int64, reason string) error {
	p, err := s.Get(ctx, id)
	if err != nil {
		return err
	}
	if p.Status == StatusSuspended {
		return nil // idempotent
	}
	p.Status = StatusSuspended
	if err := s.store.UpdateProgram(ctx, p); err != nil {
		return excerrors.Wrap(CodeMMInternal, "mm program suspend", err)
	}
	s.raise(ctx, observability.Alert{
		Rule:     "mm_program_suspended",
		Severity: observability.SeverityP2,
		Code:     CodeMMObligationBreach,
		Summary:  fmt.Sprintf("MM program %d suspended (account %d): %s", id, p.AccountID, reason),
		Status:   "firing",
		Details: map[string]string{
			"program_id": fmt.Sprint(id),
			"account_id": fmt.Sprint(p.AccountID),
			"reason":     reason,
		},
		FiredAt: s.now().UTC().Format(time.RFC3339Nano),
	})
	return nil
}

// Resume reactivates a suspended program after Risk-Manager review.
func (s *Service) Resume(ctx context.Context, id int64) error {
	p, err := s.Get(ctx, id)
	if err != nil {
		return err
	}
	if p.Status == StatusActive {
		return nil
	}
	p.Status = StatusActive
	if err := s.store.UpdateProgram(ctx, p); err != nil {
		return excerrors.Wrap(CodeMMInternal, "mm program resume", err)
	}
	return nil
}

func (s *Service) raise(ctx context.Context, a observability.Alert) {
	if s.sink == nil {
		return
	}
	if err := s.sink.Raise(ctx, a); err != nil {
		s.logf("mm: alert dispatch failed: %v", err)
	}
}
