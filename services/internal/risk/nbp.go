// nbp.go — Retail Negative-Balance Protection (Phase-19 Task 19.3.9 +
// the NBP-restitution half of Task 19.3.20; spec §13.6c, §13.4, §13.11
// item 3, §24 #133/#228/#320).
//
// DEVIATION RECORD (phase-file vs landed location): Task 19.3.9
// prescribes services/internal/margin/nbp.go — the Phase-19 margin/liquidation
// code lives in internal/risk (margin.go, liquidation.go, insurance_fund.go),
// so this file lands here. Recorded for the §27 convention.
//
// Semantics (spec §13.6c, verbatim task items mapped):
//
//	applies to   accounts.client_category='RETAIL' AND accounts.nbp
//	             (migration 042 keeps nbp=(category='RETAIL')); PROFESSIONAL /
//	             ELIGIBLE_COUNTERPARTY stay fully liable — this service is a
//	             no-op for them, the standard margin-call/collection flow owns.
//
//	triggers     (a) EvaluateAccount — bound post-liquidation by the
//	             liquidation cluster's wiring; (b) SweepOnce — the daily
//	             session-boundary (17:00 ET rollover) sweep over every
//	             retail account whose equity or any balance is negative.
//
//	reset        each negative-total balances row is floored to 0 by
//	             crediting |total| to available. Per-currency flooring is
//	             deliberate: the GL zero-sum invariant is per-currency, and
//	             "equity floored at 0" means ≥0 — positive legs in other
//	             currencies remain client property. When the resolved
//	             account equity is KNOWN non-negative, negative single-currency
//	             legs are left to the §24 #415 auto-exchange lane (cross-
//	             currency cover), not NBP restitution.
//
//	funding      shortfall debits the insurance fund via
//	             InsuranceFundService.Debit (FundReasonNBPRestitution —
//	             mig-230 enum member; one atomic tx: fund balance +
//	             insurance_fund_transactions audit + balanced GL journal
//	             DR 2210_INSURANCE_FUND_LIABILITY / CR 2010_CUSTOMER_LIABILITY
//	             + wallet credit). When the fund CANNOT cover — no fund row
//	             or balance < shortfall — the house P&L leg posts directly
//	             through the JournalPoster seam:
//	             DR 5200_NBP_RESTITUTION_EXPENSE / CR 2010_CUSTOMER_LIABILITY
//	             (the "NBP beyond fund depth" pair documented in
//	             insurance_fund.go), paged NBP_RESERVE_DEFICIT at SeverityP0.
//	             Edge: a deficit row carrying locked>0 would leave available
//	             negative after the fund's credit (its AccountEffect forbids
//	             negative available) — that shape also routes HOUSE_PNL,
//	             whose journal carries AllowNegative.
//
//	audit        every attempt mints an nbp_events row FIRST (mig 230:
//	             account_id, currency, shortfall, funding_source
//	             INSURANCE_FUND|HOUSE_PNL, journal_entry_id, status
//	             POSTED|FAILED, deficit_equity) — the row id seeds the
//	             journal IdempotencyKey "nbp:{event_id}". A movement failure
//	             flips the row to FAILED; a retry reuses the latest FAILED or
//	             journal-less row for the same (account, currency, shortfall)
//	             so replays never mint duplicate journals. The committed
//	             journal_entry_id lands back on the row — Phase-21 regulatory
//	             reporting + the admin report surface (NBPEvents).
//
//	abuse flag   ≥ NBPReviewThreshold POSTED events inside
//	             NBPReviewWindowDays sets the review flag. No
//	             accounts.review_flag column or risk_flags table exists
//	             (checked migrations 001–236) — the flag is the Redis key
//	             nbp:review:{account_id} (builder defined HERE, not keys.go:
//	             keys.go is the liquidation cluster's shared file and other
//	             Phase-19 agents edit it concurrently) plus a P1
//	             NBP_ABUSE_REVIEW alert. nbp_events is the durable truth —
//	             the flag is recomputable if Redis is lost.
//
//	What this file deliberately does NOT do: margin_accounts.equity is
//	never written — it is cluster-B's recomputed snapshot; flooring it by
//	hand would fabricate equity while positions/unrealized legs remain.
//	Post-liquidation the next evaluation recomputes ≥0 honestly from the
//	restituted balances.
package risk

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"exchange/internal/ledger"
	excredis "exchange/internal/redis"
	"exchange/pkg/decimal"
	excerrors "exchange/pkg/errors"
)

// ---------------------------------------------------------------------------
// Canonical NBP vocabulary
// ---------------------------------------------------------------------------

// nbp_events.funding_source / .status (migration 230 CHECK domains).
const (
	NBPSourceInsuranceFund = "INSURANCE_FUND"
	NBPSourceHousePnL      = "HOUSE_PNL"

	NBPStatusPosted = "POSTED"
	NBPStatusFailed = "FAILED"
)

// Abuse-detection defaults (Task 19.3.9 item 4 — "recurring NBP hits
// auto-review flag"): ≥2 POSTED events inside a rolling 90-day window.
const (
	NBPReviewWindowDays = 90
	NBPReviewThreshold  = 2
)

// codeNBPAbuseReview is an internal-only ops alert code (§23 internal-only
// list). Defined locally per this task's write scope — codes.go is edited
// by other agents concurrently.
const codeNBPAbuseReview = "NBP_ABUSE_REVIEW"

// NBPReviewKey is the abuse-review flag key. Defined locally rather than
// in keys.go (cluster-owned, concurrently edited) — documented in the
// file header.
func NBPReviewKey(accountID int64) string {
	return fmt.Sprintf("nbp:review:%d", accountID)
}

// ---------------------------------------------------------------------------
// Store seam + row type
// ---------------------------------------------------------------------------

// NBPEvent is one nbp_events row (migration 230) — the regulatory
// NBP write-off log consumed by Phase-21 reporting and the admin surface.
type NBPEvent struct {
	ID             int64            `json:"id"`
	AccountID      int64            `json:"account_id"`
	Currency       string           `json:"currency"`
	Shortfall      decimal.Decimal  `json:"shortfall"`
	FundingSource  string           `json:"funding_source"` // INSURANCE_FUND | HOUSE_PNL
	JournalEntryID *int64           `json:"journal_entry_id,omitempty"`
	Status         string           `json:"status"` // POSTED | FAILED
	DeficitEquity  *decimal.Decimal `json:"deficit_equity,omitempty"`
	CreatedAt      time.Time        `json:"created_at"`
}

// NBPStore is the Postgres seam for the NBP service; PgNBPStore is the
// production implementation, tests substitute fakes.
type NBPStore interface {
	// AccountCategory returns accounts.client_category + accounts.nbp —
	// the retail-classification input (migration 042).
	AccountCategory(ctx context.Context, accountID int64) (category string, nbp bool, err error)
	// Balances returns every balances row for the account (margin.go's
	// BalanceAmount; Total() = available+locked is the deficit test).
	Balances(ctx context.Context, accountID int64) ([]BalanceAmount, error)
	// MarginEquity returns the persisted margin_accounts.equity snapshot,
	// or (nil, nil) when no margin row exists. The live margin:level hash
	// (Levels seam) wins when bound.
	MarginEquity(ctx context.Context, accountID int64) (*decimal.Decimal, error)
	// RetailDeficitAccounts lists retail+nbp account ids whose persisted
	// margin equity is negative OR that hold any negative-total balance —
	// the 17:00 ET session-boundary sweep set.
	RetailDeficitAccounts(ctx context.Context) ([]int64, error)
	// InsertNBPEvent mints the audit row (journal_entry_id NULL, status
	// POSTED) and returns its id — minted BEFORE the funding movement so
	// the id can seed the journal idempotency key.
	InsertNBPEvent(ctx context.Context, ev NBPEvent) (int64, error)
	// ReusableNBPEvent returns the newest retryable row for
	// (account, currency, shortfall): status FAILED or POSTED with no
	// journal (crashed mid-flight). nil ⇒ mint a fresh row.
	ReusableNBPEvent(ctx context.Context, accountID int64, ccy string,
		shortfall decimal.Decimal) (*NBPEvent, error)
	// CompleteNBPEvent lands the committed journal id + funding source and
	// marks the row POSTED.
	CompleteNBPEvent(ctx context.Context, id, journalID int64, fundingSource string) error
	// FailNBPEvent marks the row FAILED after a funding-movement error.
	FailNBPEvent(ctx context.Context, id int64) error
	// CountNBPEvents counts POSTED events for the account since `since`
	// (the abuse-detection window).
	CountNBPEvents(ctx context.Context, accountID int64, since time.Time) (int, error)
	// NBPEvents returns events newest-first; accountID=0 reports all
	// accounts (admin surface).
	NBPEvents(ctx context.Context, accountID int64, limit int) ([]NBPEvent, error)
}

// ---------------------------------------------------------------------------
// Funding seams
// ---------------------------------------------------------------------------

// NBPFunder is the insurance-fund seam the service needs — the narrow
// subset of *InsuranceFundService (which satisfies it in production);
// tests substitute fakes.
type NBPFunder interface {
	// Debit moves Amount out of the fund with its balanced GL journal +
	// wallet credit + insurance_fund_transactions audit row.
	Debit(ctx context.Context, m FundMovement) (*FundMovementResult, error)
	// Balance returns the per-currency fund row, or (nil, nil) when the
	// currency has never been funded (balance zero).
	Balance(ctx context.Context, ccy string) (*FundBalance, error)
}

// ---------------------------------------------------------------------------
// NBPService
// ---------------------------------------------------------------------------

// NBPService executes §13.6c restitution. Construct via NewNBPService.
type NBPService struct {
	store   NBPStore
	rdb     *excredis.Client // review-flag writes; nil ⇒ flag skipped (alert still fires)
	levels  MarginLevelReader
	fund    NBPFunder
	poster  JournalPoster
	alerter OpsAlerter
	cfg     NBPConfig
	now     func() time.Time
	logf    func(format string, args ...any)
}

// NBPConfig tunes the service; zero fields take canonical defaults.
type NBPConfig struct {
	ReviewWindowDays int    // abuse window (default 90)
	ReviewThreshold  int    // POSTED events in window to flag (default 2)
	PostedBy         string // house-fallback journal posted_by identity
}

func (c NBPConfig) normalize() NBPConfig {
	if c.ReviewWindowDays <= 0 {
		c.ReviewWindowDays = NBPReviewWindowDays
	}
	if c.ReviewThreshold <= 0 {
		c.ReviewThreshold = NBPReviewThreshold
	}
	if c.PostedBy == "" {
		c.PostedBy = "risk-nbp"
	}
	return c
}

// NBPDeps wires the service. Store, Fund and Poster are mandatory —
// a service that cannot reach the fund or post the house fallback is a
// regulatory-invariant breach (fail-closed construction).
type NBPDeps struct {
	Store   NBPStore
	Redis   *excredis.Client // optional — review flags; absence is logged
	Levels  MarginLevelReader
	Fund    NBPFunder
	Poster  JournalPoster // *settlement.LedgerService — managed Post path
	Alerter OpsAlerter
	Config  NBPConfig
	Now     func() time.Time
	Logf    func(format string, args ...any)
}

// NewNBPService builds the service.
func NewNBPService(d NBPDeps) (*NBPService, error) {
	if d.Store == nil {
		return nil, fmt.Errorf("nbp: nil store")
	}
	if d.Fund == nil {
		return nil, fmt.Errorf("nbp: nil insurance fund — restitution has no funding source")
	}
	if d.Poster == nil {
		return nil, fmt.Errorf("nbp: nil journal poster — house-P&L fallback needs the GL seam (spec §5.3)")
	}
	s := &NBPService{
		store: d.Store, rdb: d.Redis, levels: d.Levels, fund: d.Fund,
		poster: d.Poster, alerter: d.Alerter, cfg: d.Config.normalize(),
		now: d.Now, logf: d.Logf,
	}
	if s.now == nil {
		s.now = func() time.Time { return time.Now().UTC() }
	}
	if s.logf == nil {
		s.logf = func(string, ...any) {}
	}
	if s.rdb == nil {
		s.logf("nbp: nil redis — nbp:review abuse flags will alert only")
	}
	return s, nil
}

// NBPOutcome reports one EvaluateAccount pass.
type NBPOutcome struct {
	AccountID     int64
	Applied       bool             // ≥1 restitution executed
	Events        []NBPEvent       // committed POSTED rows this pass
	EquityTrigger *decimal.Decimal // resolved equity at trigger (nil = unknown)
	ReviewFlagged bool             // abuse flag raised this pass
}

// ---------------------------------------------------------------------------
// EvaluateAccount — the per-account restitution pass
// ---------------------------------------------------------------------------

// EvaluateAccount runs §13.6c for one account. Called post-liquidation
// (the liquidation cluster binds it) and per sweep row. Non-retail /
// nbp=false accounts return an unapplied outcome — professional/ECP stay
// liable under the standard margin-call/collection flow (task item 10).
func (s *NBPService) EvaluateAccount(ctx context.Context, accountID int64) (*NBPOutcome, error) {
	cat, nbp, err := s.store.AccountCategory(ctx, accountID)
	if err != nil {
		return nil, excerrors.Wrap("INTERNAL_ERROR", "nbp: account category read", err)
	}
	out := &NBPOutcome{AccountID: accountID}
	if !nbp || cat != "RETAIL" {
		return out, nil // liable for negative balances — no write-off
	}

	balances, err := s.store.Balances(ctx, accountID)
	if err != nil {
		return nil, excerrors.Wrap("INTERNAL_ERROR", "nbp: balances read", err)
	}
	equity := s.equity(ctx, accountID)
	out.EquityTrigger = equity

	// Equity-known-non-negative ⇒ any single-currency negative leg is
	// covered cross-currency — the §24 #415 auto-exchange lane, not NBP.
	if equity != nil && !equity.IsNegative() {
		return out, nil
	}

	var deficits []BalanceAmount
	for _, b := range balances {
		if b.Total().IsNegative() {
			deficits = append(deficits, b)
		}
	}
	if len(deficits) == 0 {
		// Equity < 0 with clean balances: the deficit lives in open
		// positions/unrealized P&L — that is the liquidation engine's
		// miss, never a silent NBP skip. Page it.
		if equity != nil && equity.IsNegative() {
			s.raiseAlert(ctx, SeverityP1, CodeNBPDeficitTriggered, fmt.Sprintf(
				"retail account %d equity %s < 0 with no negative balance — deficit is positional; liquidation owns it",
				accountID, equity), map[string]string{
				"account_id": fmt.Sprint(accountID), "equity": equity.String()})
		}
		return out, nil
	}

	// Genuine retail deficit — the L1 trigger event (spec error map
	// labels this code for the NBP trigger).
	s.raiseAlert(ctx, SeverityP1, CodeNBPDeficitTriggered, fmt.Sprintf(
		"retail account %d negative-balance deficit across %d currenc(ies) — executing §13.6c restitution",
		accountID, len(deficits)), map[string]string{
		"account_id": fmt.Sprint(accountID), "deficit_currencies": fmt.Sprint(len(deficits))})

	for _, b := range deficits {
		ev, err := s.restitute(ctx, accountID, b, equity)
		if err != nil {
			return out, err
		}
		out.Events = append(out.Events, *ev)
		out.Applied = true
	}

	// Abuse detection: ≥ threshold POSTED hits inside the window flags
	// the account for review (task item 4).
	since := s.now().AddDate(0, 0, -s.cfg.ReviewWindowDays)
	count, err := s.store.CountNBPEvents(ctx, accountID, since)
	if err != nil {
		// The restitution is committed; a count read failure must not
		// fail the pass — flagging is post-commit duty.
		s.logf("nbp: event count acct %d failed: %v", accountID, err)
	} else if count >= s.cfg.ReviewThreshold {
		s.flagReview(ctx, accountID, count)
		out.ReviewFlagged = true
	}
	return out, nil
}

// restitute floors one negative-total balance row at zero: the
// insurance fund absorbs when it can cover, else house P&L (P0).
// The nbp_events row is minted before the movement so its id seeds the
// journal idempotency key; retries reuse a FAILED/journal-less row.
func (s *NBPService) restitute(ctx context.Context, accountID int64,
	b BalanceAmount, equity *decimal.Decimal) (*NBPEvent, error) {

	shortfall := b.Total().Neg()

	// Coverage pre-check (fail-closed on read error — an unknown fund
	// balance is not "cannot cover", it is an outage to retry).
	fb, err := s.fund.Balance(ctx, b.Currency)
	if err != nil {
		return nil, excerrors.Wrap("INTERNAL_ERROR",
			fmt.Sprintf("nbp: fund balance read %s", b.Currency), err)
	}
	covered := fb != nil && !fb.Balance.LessThan(shortfall) &&
		!b.Available.Add(shortfall).IsNegative()
	// The second conjunct is the locked>0 edge: crediting |total| then
	// leaves available = -locked < 0, which the fund's AccountEffect
	// (no AllowNegative) would reject — route house instead.

	funding := NBPSourceInsuranceFund
	if !covered {
		funding = NBPSourceHousePnL
	}

	ev, err := s.store.ReusableNBPEvent(ctx, accountID, b.Currency, shortfall)
	if err != nil {
		return nil, excerrors.Wrap("INTERNAL_ERROR", "nbp: reusable event read", err)
	}
	if ev == nil {
		id, err := s.store.InsertNBPEvent(ctx, NBPEvent{
			AccountID:     accountID,
			Currency:      b.Currency,
			Shortfall:     shortfall,
			FundingSource: funding,
			Status:        NBPStatusPosted,
			DeficitEquity: equity,
		})
		if err != nil {
			return nil, excerrors.Wrap("INTERNAL_ERROR", "nbp: event insert", err)
		}
		ev = &NBPEvent{ID: id, AccountID: accountID, Currency: b.Currency,
			Shortfall: shortfall, FundingSource: funding, Status: NBPStatusPosted,
			DeficitEquity: equity}
	}

	journalID, err := s.fundRestitution(ctx, ev, funding, fb)
	if err != nil {
		if ferr := s.store.FailNBPEvent(ctx, ev.ID); ferr != nil {
			s.logf("nbp: FAIL mark on event %d failed: %v", ev.ID, ferr)
		}
		return nil, err
	}
	if err := s.store.CompleteNBPEvent(ctx, ev.ID, journalID, funding); err != nil {
		// The wallet+GL committed — a row-completion failure is
		// reconcileable (journal_entry_id stays NULL → retryable).
		return nil, excerrors.Wrap("INTERNAL_ERROR",
			fmt.Sprintf("nbp: event %d completion (journal %d committed)", ev.ID, journalID), err)
	}
	ev.FundingSource = funding
	ev.JournalEntryID = &journalID
	ev.Status = NBPStatusPosted
	return ev, nil
}

// fundRestitution executes the money movement and returns the committed
// journal id.
func (s *NBPService) fundRestitution(ctx context.Context, ev *NBPEvent,
	funding string, fb *FundBalance) (int64, error) {

	idem := fmt.Sprintf("nbp:%d", ev.ID)
	if funding == NBPSourceInsuranceFund {
		res, err := s.fund.Debit(ctx, FundMovement{
			Reason:         FundReasonNBPRestitution,
			Currency:       ev.Currency,
			Amount:         ev.Shortfall,
			ReferenceType:  "nbp",
			ReferenceID:    ev.ID,
			AccountID:      ev.AccountID,
			IdempotencyKey: idem,
			Narrative: fmt.Sprintf("retail NBP restitution acct %d %s %s",
				ev.AccountID, ev.Currency, ev.Shortfall),
		})
		if err != nil {
			return 0, excerrors.Wrap(CodeNBPRestitutionReserveDeficit,
				fmt.Sprintf("nbp: fund debit event %d failed", ev.ID), err)
		}
		return res.JournalID, nil
	}

	// HOUSE_PNL — the fund cannot cover. P0 page FIRST in the details,
	// then the managed posting (locks + retry + BalanceChanged dispatch
	// are the LedgerService's own machinery).
	fundBal := "no fund row"
	if fb != nil {
		fundBal = fb.Balance.String()
	}
	s.raiseAlert(ctx, SeverityP0, CodeNBPRestitutionReserveDeficit, fmt.Sprintf(
		"NBP_RESERVE_DEFICIT: retail acct %d shortfall %s %s exceeds insurance fund balance %s — house P&L absorbs (NBP_RESTITUTION_POSTED)",
		ev.AccountID, ev.Shortfall, ev.Currency, fundBal), map[string]string{
		"account_id": fmt.Sprint(ev.AccountID), "currency": ev.Currency,
		"shortfall": ev.Shortfall.String(), "fund_balance": fundBal,
		"nbp_event_id": fmt.Sprint(ev.ID)})

	j := ledger.Journal{
		EntryType:      ledger.EntryAdjustment,
		ReferenceID:    ev.ID,
		Description:    fmt.Sprintf("NBP_RESTITUTION_POSTED house P&L acct %d %s %s", ev.AccountID, ev.Currency, ev.Shortfall),
		PostedBy:       s.cfg.PostedBy,
		IdempotencyKey: idem,
		Lines: []ledger.Line{
			ledger.DebitLine(ledger.NBPRestitutionExpense(ev.Currency), ev.Currency,
				ev.Shortfall, "retail NBP restitution — house P&L absorbs (fund cannot cover)"),
			ledger.CreditLine(ledger.CustomerLiability(ev.Currency), ev.Currency,
				ev.Shortfall, fmt.Sprintf("NBP restitution to client %d", ev.AccountID)),
		},
		Effects: []ledger.AccountEffect{{
			AccountID: ev.AccountID, Currency: ev.Currency,
			AvailableDelta: ev.Shortfall, AllowNegative: true,
		}},
	}
	pr, err := s.poster.Post(ctx, j)
	if err != nil {
		return 0, excerrors.Wrap(CodeNBPRestitutionReserveDeficit,
			fmt.Sprintf("nbp: house-P&L journal event %d failed", ev.ID), err)
	}
	return pr.JournalID, nil
}

// ---------------------------------------------------------------------------
// Sweep — the 17:00 ET session-boundary pass (spec §13.6c item 1)
// ---------------------------------------------------------------------------

// SweepOnce evaluates every retail account flagged by
// RetailDeficitAccounts (persisted negative margin equity OR any
// negative-total balance). Production binds this to the daily rollover
// cadence; per-account failures are logged and the sweep continues —
// one poisoned account never strands the rest. Returns the count of
// accounts restituted.
func (s *NBPService) SweepOnce(ctx context.Context) (int, error) {
	ids, err := s.store.RetailDeficitAccounts(ctx)
	if err != nil {
		return 0, excerrors.Wrap("INTERNAL_ERROR", "nbp: deficit account scan", err)
	}
	n := 0
	for _, id := range ids {
		out, err := s.EvaluateAccount(ctx, id)
		if err != nil {
			s.logf("nbp: sweep acct %d: %v", id, err)
			continue
		}
		if out != nil && out.Applied {
			n++
		}
	}
	return n, nil
}

// ---------------------------------------------------------------------------
// Admin report surface (Task 19.3.9 item 4 — Phase-19 API wiring consumes)
// ---------------------------------------------------------------------------

// NBPEvents returns the regulatory write-off log newest-first;
// accountID=0 spans all accounts.
func (s *NBPService) NBPEvents(ctx context.Context, accountID int64, limit int) ([]NBPEvent, error) {
	return s.store.NBPEvents(ctx, accountID, limit)
}

// ---------------------------------------------------------------------------
// Internals
// ---------------------------------------------------------------------------

// equity resolves the account's equity at trigger: the live margin:level
// hash first, then the persisted margin_accounts snapshot, else nil
// (unknown — callers then pessimistically trigger on negative balances).
func (s *NBPService) equity(ctx context.Context, accountID int64) *decimal.Decimal {
	if s.levels != nil {
		if lv, err := s.levels.MarginLevel(ctx, accountID); err == nil && lv != nil {
			e := lv.Equity
			return &e
		} else if err != nil {
			s.logf("nbp: margin level read acct %d failed (fallback to snapshot): %v", accountID, err)
		}
	}
	if e, err := s.store.MarginEquity(ctx, accountID); err == nil && e != nil {
		return e
	} else if err != nil {
		s.logf("nbp: margin equity read acct %d failed: %v", accountID, err)
	}
	return nil
}

// flagReview sets the nbp:review:{account_id} flag and pages the
// abuse-review alert. The durable evidence is nbp_events; the flag is a
// recomputable pointer for ops tooling.
func (s *NBPService) flagReview(ctx context.Context, accountID int64, count int) {
	payload, _ := json.Marshal(map[string]any{
		"account_id":   accountID,
		"event_count":  count,
		"window_days":  s.cfg.ReviewWindowDays,
		"threshold":    s.cfg.ReviewThreshold,
		"flagged_at":   s.now().UTC().Format(time.RFC3339Nano),
		"recomputable": "nbp_events",
	})
	if s.rdb != nil {
		if err := s.rdb.Set(ctx, NBPReviewKey(accountID), payload, 0).Err(); err != nil {
			s.logf("nbp: review flag write acct %d failed: %v", accountID, err)
		}
	}
	s.raiseAlert(ctx, SeverityP1, codeNBPAbuseReview, fmt.Sprintf(
		"retail account %d hit %d NBP restitutions inside %d days — flagged for abuse review",
		accountID, count, s.cfg.ReviewWindowDays), map[string]string{
		"account_id": fmt.Sprint(accountID), "event_count": fmt.Sprint(count),
		"window_days": fmt.Sprint(s.cfg.ReviewWindowDays)})
}

// raiseAlert delivers an ops alert on a bounded fresh context (the
// caller's ctx is often spent post-commit).
func (s *NBPService) raiseAlert(ctx context.Context, severity, code, summary string, details map[string]string) {
	if s.alerter == nil {
		s.logf("nbp: alert %s undeliverable (no alerter): %s", code, summary)
		return
	}
	actx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := s.alerter.Raise(actx, OpsAlert{
		Severity: severity, Code: code, Summary: summary, Details: details,
	}); err != nil {
		s.logf("nbp: alert %s dispatch failed: %v", code, err)
	}
}

// ---------------------------------------------------------------------------
// PgNBPStore — production store (migration-230 schema)
// ---------------------------------------------------------------------------

// PgNBPStore implements NBPStore over pgx. Monetary columns scan as
// ::text into the decimal facade — never float64.
type PgNBPStore struct {
	Pool *pgxpool.Pool
}

// NewPgNBPStore binds the pool.
func NewPgNBPStore(pool *pgxpool.Pool) (*PgNBPStore, error) {
	if pool == nil {
		return nil, fmt.Errorf("nbp store: nil pgx pool")
	}
	return &PgNBPStore{Pool: pool}, nil
}

// AccountCategory implements NBPStore — accounts.client_category + nbp
// (migration 042). A missing account is an error, never a silent default.
func (s *PgNBPStore) AccountCategory(ctx context.Context, accountID int64) (string, bool, error) {
	var cat string
	var nbp bool
	err := s.Pool.QueryRow(ctx, `
		SELECT client_category::text, nbp FROM accounts WHERE id = $1`, accountID).
		Scan(&cat, &nbp)
	if err == pgx.ErrNoRows {
		return "", false, fmt.Errorf("nbp: account %d not found", accountID)
	}
	if err != nil {
		return "", false, fmt.Errorf("nbp: account category %d: %w", accountID, err)
	}
	return cat, nbp, nil
}

// Balances implements NBPStore.
func (s *PgNBPStore) Balances(ctx context.Context, accountID int64) ([]BalanceAmount, error) {
	rows, err := s.Pool.Query(ctx, `
		SELECT currency, available::text, locked::text
		FROM balances WHERE account_id = $1`, accountID)
	if err != nil {
		return nil, fmt.Errorf("nbp: balances acct %d: %w", accountID, err)
	}
	defer rows.Close()
	var out []BalanceAmount
	for rows.Next() {
		var b BalanceAmount
		var avail, locked string
		if err := rows.Scan(&b.Currency, &avail, &locked); err != nil {
			return nil, fmt.Errorf("nbp: balances acct %d scan: %w", accountID, err)
		}
		if b.Available, err = decimal.NewFromString(avail); err != nil {
			return nil, fmt.Errorf("nbp: balance %q: %w", avail, err)
		}
		if b.Locked, err = decimal.NewFromString(locked); err != nil {
			return nil, fmt.Errorf("nbp: locked %q: %w", locked, err)
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

// MarginEquity implements NBPStore — (nil, nil) when the account has no
// margin_accounts row.
func (s *PgNBPStore) MarginEquity(ctx context.Context, accountID int64) (*decimal.Decimal, error) {
	var txt string
	err := s.Pool.QueryRow(ctx, `
		SELECT equity::text FROM margin_accounts WHERE account_id = $1`, accountID).Scan(&txt)
	if err == pgx.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("nbp: margin equity acct %d: %w", accountID, err)
	}
	d, err := decimal.NewFromString(txt)
	if err != nil {
		return nil, fmt.Errorf("nbp: margin equity %q: %w", txt, err)
	}
	return &d, nil
}

// RetailDeficitAccounts implements NBPStore — the sweep set: retail+nbp
// accounts with persisted negative margin equity OR any negative-total
// balance row.
func (s *PgNBPStore) RetailDeficitAccounts(ctx context.Context) ([]int64, error) {
	rows, err := s.Pool.Query(ctx, `
		SELECT DISTINCT a.id FROM accounts a
		WHERE a.client_category = 'RETAIL' AND a.nbp
		  AND ( EXISTS (SELECT 1 FROM margin_accounts m
		                WHERE m.account_id = a.id AND m.equity < 0)
		     OR EXISTS (SELECT 1 FROM balances b
		                WHERE b.account_id = a.id AND b.total < 0) )
		ORDER BY a.id`)
	if err != nil {
		return nil, fmt.Errorf("nbp: deficit account scan: %w", err)
	}
	defer rows.Close()
	var out []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("nbp: deficit scan: %w", err)
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// InsertNBPEvent implements NBPStore.
func (s *PgNBPStore) InsertNBPEvent(ctx context.Context, ev NBPEvent) (int64, error) {
	var deficit *string
	if ev.DeficitEquity != nil {
		str := ev.DeficitEquity.String()
		deficit = &str
	}
	var id int64
	err := s.Pool.QueryRow(ctx, `
		INSERT INTO nbp_events
		    (account_id, currency, shortfall, funding_source, status, deficit_equity)
		VALUES ($1,$2,$3,$4,$5,$6) RETURNING id`,
		ev.AccountID, ev.Currency, ev.Shortfall.String(),
		ev.FundingSource, ev.Status, deficit).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("nbp: event insert acct %d: %w", ev.AccountID, err)
	}
	return id, nil
}

// ReusableNBPEvent implements NBPStore — the retryable-row lookup
// (FAILED, or POSTED with a NULL journal: a crashed mid-flight attempt).
func (s *PgNBPStore) ReusableNBPEvent(ctx context.Context, accountID int64,
	ccy string, shortfall decimal.Decimal) (*NBPEvent, error) {

	rows, err := s.Pool.Query(ctx, `
		SELECT id, account_id, currency, shortfall::text, funding_source,
		       journal_entry_id, status, deficit_equity::text, created_at
		FROM nbp_events
		WHERE account_id = $1 AND currency = $2 AND shortfall = $3
		  AND (status = 'FAILED' OR journal_entry_id IS NULL)
		ORDER BY id DESC LIMIT 1`,
		accountID, ccy, shortfall.String())
	if err != nil {
		return nil, fmt.Errorf("nbp: reusable event read: %w", err)
	}
	defer rows.Close()
	return scanOneNBPEvent(rows)
}

// CompleteNBPEvent implements NBPStore.
func (s *PgNBPStore) CompleteNBPEvent(ctx context.Context, id, journalID int64, fundingSource string) error {
	tag, err := s.Pool.Exec(ctx, `
		UPDATE nbp_events SET journal_entry_id = $2, funding_source = $3,
		       status = 'POSTED'
		WHERE id = $1`, id, journalID, fundingSource)
	if err != nil {
		return fmt.Errorf("nbp: event %d complete: %w", id, err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("nbp: event %d complete: no row", id)
	}
	return nil
}

// FailNBPEvent implements NBPStore.
func (s *PgNBPStore) FailNBPEvent(ctx context.Context, id int64) error {
	_, err := s.Pool.Exec(ctx,
		`UPDATE nbp_events SET status = 'FAILED' WHERE id = $1`, id)
	if err != nil {
		return fmt.Errorf("nbp: event %d fail-mark: %w", id, err)
	}
	return nil
}

// CountNBPEvents implements NBPStore — POSTED events inside the window.
func (s *PgNBPStore) CountNBPEvents(ctx context.Context, accountID int64, since time.Time) (int, error) {
	var n int
	if err := s.Pool.QueryRow(ctx, `
		SELECT count(*) FROM nbp_events
		WHERE account_id = $1 AND status = 'POSTED' AND created_at >= $2`,
		accountID, since).Scan(&n); err != nil {
		return 0, fmt.Errorf("nbp: event count acct %d: %w", accountID, err)
	}
	return n, nil
}

// NBPEvents implements NBPStore — newest-first admin read.
func (s *PgNBPStore) NBPEvents(ctx context.Context, accountID int64, limit int) ([]NBPEvent, error) {
	if limit <= 0 {
		limit = 100
	}
	q := `SELECT id, account_id, currency, shortfall::text, funding_source,
	             journal_entry_id, status, deficit_equity::text, created_at
	      FROM nbp_events`
	args := []any{}
	if accountID > 0 {
		q += ` WHERE account_id = $1`
		args = append(args, accountID)
	}
	q += fmt.Sprintf(` ORDER BY id DESC LIMIT $%d`, len(args)+1)
	args = append(args, limit)
	rows, err := s.Pool.Query(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("nbp: events read: %w", err)
	}
	defer rows.Close()
	var out []NBPEvent
	for rows.Next() {
		ev, err := scanNBPEventRow(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *ev)
	}
	return out, rows.Err()
}

func scanOneNBPEvent(rows pgx.Rows) (*NBPEvent, error) {
	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return nil, fmt.Errorf("nbp: event row: %w", err)
		}
		return nil, nil
	}
	return scanNBPEventRow(rows)
}

func scanNBPEventRow(rows pgx.Rows) (*NBPEvent, error) {
	var ev NBPEvent
	var short, deficit *string
	if err := rows.Scan(&ev.ID, &ev.AccountID, &ev.Currency, &short,
		&ev.FundingSource, &ev.JournalEntryID, &ev.Status, &deficit,
		&ev.CreatedAt); err != nil {
		return nil, fmt.Errorf("nbp: event scan: %w", err)
	}
	if short != nil {
		d, err := decimal.NewFromString(*short)
		if err != nil {
			return nil, fmt.Errorf("nbp: shortfall %q: %w", *short, err)
		}
		ev.Shortfall = d
	}
	if deficit != nil {
		d, err := decimal.NewFromString(*deficit)
		if err != nil {
			return nil, fmt.Errorf("nbp: deficit_equity %q: %w", *deficit, err)
		}
		ev.DeficitEquity = &d
	}
	return &ev, nil
}
