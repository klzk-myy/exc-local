// Task 15.3.5 — Trade Bust & Price-Adjust Workflow (obvious-error
// policy), spec §5.29 (trade_busts), §7.2 (Risk Manager + dual control),
// §7.3.4 (15-minute claim deadline / irrevocable dispatch →
// TRADE_ALREADY_SETTLED) and §24 #138.
//
// Shape of the workflow:
//
//	RequestBust(actor, in)                       — Risk Manager+ initiates.
//	  - in.ApproverID = 0 → durable trade_busts row PENDING_APPROVAL
//	    (the trade is under review — settlement legs are held while this
//	    row exists; the settlement dispatcher consults HasPendingBust /
//	    PendingBustTradeIDs before claiming a trade's instructions).
//	  - in.ApproverID > 0 → the §8.2 synchronous "two principals, one
//	    request" path (same convention as FreezeService / kill-switch —
//	    blessed by dualcontrol.go's header for ops that cannot wait on a
//	    queue): both roles validated, execution runs atomically.
//	DecideBust(approver, bustID, approve, note)  — a DISTINCT second
//	  Risk Manager+ approver completes (or turns down) a pending review.
//
// Execution is one SERIALIZABLE transaction holding the §5.3 Redis account
// mutexes: eligibility re-check → GL reversal/adjustment journal via the
// landed LedgerService.PostJournal contract → trades.status flag →
// settlement leg void/amend → position aggregate rebuild → audit.
// BalanceChanged events dispatch post-commit through the wired Publisher
// seam (account.balance.changed.{id}); counterparty notifications ride
// the notifications seam (trade_busted / trade_price_adjusted).
//
// GL reversal (BUST — the mirror image of the Task 3.3.1 four-legged
// fill, spec §5.3/§5.21 zero-sum per currency):
//
//	QUOTE:  CR 2010 qa            (buyer's consumed reservation restored)
//	        DR 2010 (qa−sellerFee) (seller proceeds clawed back)
//	        DR 4010 sellerFee     (fee revenue reversed)
//	BASE:   CR 2010 q             (seller's consumed reservation restored)
//	        DR 2010 (q−buyerFee)  (buyer proceeds clawed back)
//	        DR 4010 buyerFee      (fee revenue reversed)
//
// PRICE_ADJUST posts the signed quote-currency delta only (buyer pays
// q·p′ instead of q·p; seller mirrors). Fees are NOT recomputed — the
// original fee schedule applied at the fill stands (recorded deviation:
// §5.29 does not prescribe fee re-rating).
//
// PHYSICAL_DELIVERY fills (orders.settlement_intent, migration 104) reverse
// the pending-delivery lock instead of wallet proceeds: the 2011_PENDING_
// SETTLEMENT_DELIVERY reclass is unwound and locked deliverables released.
//
// Position side: positions are a projection of position_fills — a bust
// rebuilds the aggregate by replaying the account's fills for the
// instrument minus the busted trade (PRICE_ADJUST replays with the
// adjusted price, after updating position_fills.price). The mirror of
// settlement.applyFill lives here to keep the mutation inside the bust
// transaction without exporting sibling internals.
package admin

import (
	"context"
	"encoding/json"
	stderrors "errors"
	"fmt"
	"sort"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"exchange/internal/ledger"
	"exchange/pkg/decimal"
	excerrors "exchange/pkg/errors"
)

// ---------------------------------------------------------------------------
// Vocabulary (spec §5.29 / §5.5 status enums, migration 051).
// ---------------------------------------------------------------------------

const (
	BustActionBust        = "BUST"
	BustActionPriceAdjust = "PRICE_ADJUST"

	BustStatusPending  = "PENDING_APPROVAL"
	BustStatusExecuted = "EXECUTED"
	BustStatusRejected = "REJECTED"

	TradeStatusCompleted     = "COMPLETED"
	TradeStatusBusted        = "BUSTED"
	TradeStatusPriceAdjusted = "PRICE_ADJUSTED"
)

// BustWindow is the canonical obvious-error claim deadline — spec §7.3.4
// (remediation #35 pinned 15 minutes, superseding the draft's 1h).
const BustWindow = 15 * time.Minute

// BustReviewTTL mirrors the §8.2 four-eyes window: a pending review lapses
// after 15 minutes (ExpirePending marks it REJECTED).
const BustReviewTTL = ApprovalWindow

// Settlement leg statuses we must refuse to disturb (§7.3.4 — dispatched
// or settled is irrevocable; 'VOID' is the 051 terminal state for
// undispatched legs).
var bustSettledStatuses = []string{"SETTLED", "RECONCILED"}

// Notification event tokens (notifications.go vocabulary — Task 12.3.5
// registry extended for this task; both are critical (bypass quiet hours)).
const (
	BustEventBusted        = "trade_busted"
	BustEventPriceAdjusted = "trade_price_adjusted"
)

// bustRoles may initiate or approve an obvious-error correction — spec
// §7.2 "Trade bust / price adjust: Risk Manager+, dual control".
var bustRoles = map[string]bool{
	RoleRiskManager: true,
	RoleSuperAdmin:  true,
}

// ---------------------------------------------------------------------------
// Seams
// ---------------------------------------------------------------------------

// BustJournalPoster is the tx-scoped §5.3 posting contract —
// *settlement.LedgerService satisfies it (PostJournal).
type BustJournalPoster interface {
	PostJournal(ctx context.Context, tx pgx.Tx, j ledger.Journal) (ledger.PostResult, error)
}

// BustAccountLocker is the §5.3 Redis mutex surface — *excredis.Client
// satisfies it (TryLockAccount/UnlockAccount).
type BustAccountLocker interface {
	TryLockAccount(ctx context.Context, accountID, token string, ttl time.Duration) (bool, error)
	UnlockAccount(ctx context.Context, accountID, token string) (bool, error)
}

// BustEventPublisher dispatches committed BalanceChanged events to
// account.balance.changed.{account_id} — the settlement.Publisher shape
// (NATS JetStream in production; nil → events are recorded but not fanned
// out — a dispatch failure after commit must be resynced, never reposted).
type BustEventPublisher interface {
	Publish(ctx context.Context, subject string, payload []byte) error
}

// BustNotifier is the counterparty-notification seam —
// *notifications.Service satisfies it (Notify).
type BustNotifier interface {
	Notify(ctx context.Context, userID int64, event string, payload map[string]any) (int, error)
}

// BustReferencePricer resolves the market reference price the obvious-error
// deviation is measured against (spec §7.3.4 — "market at fill time").
// The default (pgxTradeRefPricer) uses the most recent COMPLETED trade on
// the instrument strictly before the fill; callers may inject the Phase-19.5
// oracle/backfill seam. before is the fill's created_at.
type BustReferencePricer func(ctx context.Context, tx pgx.Tx, instrumentID int64, before time.Time) (decimal.Decimal, error)

// AdminActor / AdminRoleResolver are the shared admin seams declared in
// lp.go (AdminActor{UserID, ApproverID, ClientIP}).

// ---------------------------------------------------------------------------
// Service
// ---------------------------------------------------------------------------

// TradeBustService is the obvious-error correction control plane.
type TradeBustService struct {
	pool    *pgxpool.Pool
	poster  BustJournalPoster
	locks   BustAccountLocker
	pub     BustEventPublisher
	roles   AdminRoleResolver
	notify  BustNotifier
	ref     BustReferencePricer
	bandMul decimal.Decimal // obvious-error multiplier over the instrument band
	now     func() time.Time
	logf    func(format string, args ...any)
}

// TradeBustDeps wires the service. Pool, Poster, Locks and Roles are
// mandatory — a bust that cannot hold the §5.3 mutexes or post a balanced
// journal must fail closed, never record a half-correction.
type TradeBustDeps struct {
	Pool      *pgxpool.Pool
	Poster    BustJournalPoster
	Locks     BustAccountLocker
	Publisher BustEventPublisher // nil → no post-commit fan-out (dev)
	Roles     AdminRoleResolver
	Notifier  BustNotifier        // nil → counterparty notices skipped (logged)
	RefPrice  BustReferencePricer // nil → prior-trade default
	// BandMultiplier is the obvious-error band multiplier (default 2.0 —
	// price deviating > 2× the instrument's price band from market at fill,
	// spec §7.3.4 / Phase-15 task text).
	BandMultiplier decimal.Decimal
	Logf           func(format string, args ...any)
	Now            func() time.Time
}

// NewTradeBustService wires the service; missing mandatory deps fail closed.
func NewTradeBustService(d TradeBustDeps) (*TradeBustService, error) {
	if d.Pool == nil {
		return nil, fmt.Errorf("trade-bust: pgx pool is nil")
	}
	if d.Poster == nil {
		return nil, fmt.Errorf("trade-bust: journal poster is nil (§5.3 posting contract required)")
	}
	if d.Locks == nil {
		return nil, fmt.Errorf("trade-bust: account mutex locker is nil (§5.3 locking protocol required)")
	}
	if d.Roles == nil {
		return nil, fmt.Errorf("trade-bust: role resolver is nil")
	}
	s := &TradeBustService{
		pool: d.Pool, poster: d.Poster, locks: d.Locks, pub: d.Publisher,
		roles: d.Roles, notify: d.Notifier, ref: d.RefPrice,
		bandMul: d.BandMultiplier, now: d.Now, logf: d.Logf,
	}
	if s.bandMul.IsZero() || s.bandMul.IsNegative() {
		s.bandMul = decimal.NewFromInt(2)
	}
	if s.ref == nil {
		s.ref = pgxTradeRefPricer
	}
	if s.now == nil {
		s.now = time.Now
	}
	if s.logf == nil {
		s.logf = func(string, ...any) {}
	}
	return s, nil
}

// ---------------------------------------------------------------------------
// Types
// ---------------------------------------------------------------------------

// TradeBust is one trade_busts row (spec §5.29 + the reference_price
// evidence column).
type TradeBust struct {
	ID             int64            `json:"id"`
	TradeID        int64            `json:"trade_id"`
	Action         string           `json:"action"`
	AdjustedPrice  *decimal.Decimal `json:"adjusted_price,omitempty"`
	Reason         string           `json:"reason"`
	ReferencePrice *decimal.Decimal `json:"reference_price,omitempty"`
	InitiatedBy    int64            `json:"initiated_by"`
	ApprovedBy     *int64           `json:"approved_by,omitempty"`
	Status         string           `json:"status"`
	CreatedAt      time.Time        `json:"created_at"`
	ExecutedAt     *time.Time       `json:"executed_at,omitempty"`
}

// BustRequest is the POST /api/v1/admin/trades/{id}/bust payload.
type BustRequest struct {
	TradeID       int64           `json:"-"` // path parameter
	Action        string          `json:"action"`
	AdjustedPrice decimal.Decimal `json:"adjusted_price"` // required for PRICE_ADJUST
	Reason        string          `json:"reason"`
	// ApproverID is the second principal (§8.2 synchronous dual control);
	// 0 → the request lands PENDING_APPROVAL for DecideBust.
	ApproverID int64 `json:"approver_id"`
	// ReferencePrice optionally overrides the at-fill market reference
	// (evidence attachment when no prior trade exists on the instrument).
	ReferencePrice *decimal.Decimal `json:"reference_price,omitempty"`
}

// BustOutcome reports a correction request. For the queued path it
// carries the PENDING_APPROVAL row (Settlement "PENDING"); for the
// executed path it carries the EXECUTED row + journal/settlement detail.
type BustOutcome struct {
	Bust       TradeBust `json:"bust"`
	JournalID  int64     `json:"journal_id,omitempty"`
	Settlement string    `json:"settlement"` // "PENDING" | "VOIDED" | "AMENDED" | "NONE"
	Dispatched bool      `json:"dispatched"` // BalanceChanged events published post-commit
}

// tradeRow is the locked trades row plus the resolution context the
// reversal journal needs.
type tradeRow struct {
	id          int64
	instrument  int64
	symbol      string
	base, quote string
	buyOrder    int64
	sellOrder   int64
	buyer       int64
	seller      int64
	price       decimal.Decimal
	qty         decimal.Decimal
	buyerFee    decimal.Decimal
	sellerFee   decimal.Decimal
	intent      string // orders.settlement_intent (both legs must agree)
	status      string
	createdAt   time.Time
	bandPct     decimal.Decimal // max(price_band_pct_up, price_band_pct_down)
}

// ---------------------------------------------------------------------------
// Authorization (spec §7.2: Risk Manager+, dual control — two distinct
// principals, both eligible).
// ---------------------------------------------------------------------------

func (s *TradeBustService) requireBustRole(ctx context.Context, userID int64) error {
	if s.roles == nil {
		return excerrors.New("UNAUTHORIZED_ROLE",
			"trade bust requires an admin role — resolver unavailable")
	}
	role, err := s.roles(ctx, userID)
	if err != nil {
		return excerrors.New("INTERNAL_ERROR",
			"trade bust role resolution failed: "+err.Error())
	}
	if !bustRoles[role] {
		return excerrors.New("UNAUTHORIZED_ROLE",
			"trade bust requires Risk Manager or stronger (spec §7.2)")
	}
	return nil
}

// ---------------------------------------------------------------------------
// Request — eligibility + PENDING_APPROVAL (or synchronous EXECUTED when
// approver_id rides along).
// ---------------------------------------------------------------------------

// RequestBust opens (or, with approver_id, completes) an obvious-error
// review on a trade. approver_id = 0 → PENDING_APPROVAL review (the
// settlement hold attaches until decided); approver_id = a distinct,
// role-eligible admin → §8.2 synchronous "two principals, one request"
// execution (the §7.3.4 window is too short to depend on a queue).
func (s *TradeBustService) RequestBust(ctx context.Context, actor AdminActor, in BustRequest) (*BustOutcome, error) {
	if err := s.requireBustRole(ctx, actor.UserID); err != nil {
		return nil, err
	}
	if err := s.validateRequest(in); err != nil {
		return nil, err
	}

	// Cheap pre-read for the lock set (account ids) — re-validated under
	// FOR UPDATE inside the transaction.
	pre, err := s.loadTrade(ctx, nil, in.TradeID)
	if err != nil {
		return nil, err
	}

	if in.ApproverID == 0 {
		// Queued review: durable PENDING_APPROVAL row; settlement dispatch
		// is held on this trade until the review decides (HasPendingBust).
		b, err := s.insertPending(ctx, actor, pre, in)
		if err != nil {
			return nil, err
		}
		return &BustOutcome{Bust: *b, Settlement: "PENDING"}, nil
	}

	// Synchronous two-principal completion.
	if err := s.requireApprover(ctx, actor.UserID, in.ApproverID); err != nil {
		return nil, err
	}
	return s.execute(ctx, pre, nil, actor.UserID, in.ApproverID, in, actor.ClientIP)
}

func (s *TradeBustService) validateRequest(in BustRequest) error {
	if in.TradeID <= 0 {
		return excerrors.New("INVALID_REQUEST", "trade id required")
	}
	switch in.Action {
	case BustActionBust:
	case BustActionPriceAdjust:
		if !in.AdjustedPrice.IsPositive() {
			return excerrors.New("INVALID_REQUEST",
				"price-adjust requires a positive adjusted_price")
		}
		if !in.AdjustedPrice.Round(8).Equal(in.AdjustedPrice) {
			return excerrors.New("INVALID_REQUEST",
				"adjusted_price exceeds the DECIMAL(20,8) quantum")
		}
	default:
		return excerrors.New("INVALID_REQUEST",
			fmt.Sprintf("action %q must be BUST or PRICE_ADJUST", in.Action))
	}
	if len(in.Reason) == 0 || len(in.Reason) > 255 {
		return excerrors.New("INVALID_REQUEST",
			"reason is required (≤255 chars) — the §5.29 review record carries it")
	}
	return nil
}

// requireApprover enforces the four-eyes leg: a distinct, role-eligible
// second principal (same convention as KillSwitchService.requireApprover).
func (s *TradeBustService) requireApprover(ctx context.Context, actor, approver int64) error {
	if approver <= 0 || approver == actor {
		return excerrors.New("DUAL_CONTROL_REQUIRED",
			"trade bust requires a second distinct authorizer (§7.2/§8.2)")
	}
	if err := s.requireBustRole(ctx, approver); err != nil {
		return err
	}
	return nil
}

// ---------------------------------------------------------------------------
// Eligibility (spec §7.3.4 + §5.29)
// ---------------------------------------------------------------------------

// checkEligibility runs the obvious-error gates against a FOR-UPDATE-locked
// trade row. Called inside the transaction. excludeBustID exempts the
// pending review row being approved (it would otherwise trip the
// TRADE_BUST_PENDING gate on itself).
func (s *TradeBustService) checkEligibility(ctx context.Context, tx pgx.Tx, t *tradeRow, in *BustRequest, excludeBustID int64) (decimal.Decimal, error) {
	var zero decimal.Decimal
	// Post-trade lifecycle: only a COMPLETED trade is correctable.
	switch t.status {
	case TradeStatusCompleted:
	case TradeStatusBusted:
		return zero, excerrors.New("INVALID_REQUEST",
			fmt.Sprintf("trade %d already BUSTED — busted trades are terminal (never re-busted, never deleted)", t.id))
	case TradeStatusPriceAdjusted:
		return zero, excerrors.New("INVALID_REQUEST",
			fmt.Sprintf("trade %d already PRICE_ADJUSTED — one executed correction per trade", t.id))
	default:
		return zero, excerrors.New("INVALID_REQUEST",
			fmt.Sprintf("trade %d has unknown status %q", t.id, t.status))
	}
	// §7.3.4 deadline: claims submitted after the 15-minute window reject
	// with TRADE_ALREADY_SETTLED (the spec's canonical code for late busts).
	if s.now().After(t.createdAt.Add(BustWindow)) {
		return zero, excerrors.New("TRADE_ALREADY_SETTLED",
			fmt.Sprintf("trade %d is past the %s obvious-error window (filled %s)",
				t.id, BustWindow, t.createdAt.UTC().Format(time.RFC3339)))
	}
	// Second review on the same trade → TRADE_BUST_PENDING (spec §23).
	var pending int
	if err := tx.QueryRow(ctx,
		`SELECT count(*) FROM trade_busts
		  WHERE trade_id=$1 AND status='PENDING_APPROVAL' AND id <> $2`,
		t.id, excludeBustID).Scan(&pending); err != nil {
		return zero, excerrors.Wrap("INTERNAL_ERROR", "pending bust probe", err)
	}
	if pending > 0 {
		return zero, excerrors.New("TRADE_BUST_PENDING",
			fmt.Sprintf("trade %d already under obvious-error review", t.id))
	}
	// Irrevocable settlement: any leg dispatched or terminal → refuse.
	var settled int
	if err := tx.QueryRow(ctx, `
		SELECT count(*) FROM settlement_instructions
		 WHERE trade_id=$1 AND (dispatched_at IS NOT NULL OR status = ANY($2))`,
		t.id, bustSettledStatuses).Scan(&settled); err != nil {
		return zero, excerrors.Wrap("INTERNAL_ERROR", "settlement state probe", err)
	}
	if settled > 0 {
		return zero, excerrors.New("TRADE_ALREADY_SETTLED",
			fmt.Sprintf("trade %d has dispatched/settled settlement legs — only bilateral compensation remains (§7.3.4)", t.id))
	}

	// Obvious-error band: |fill − ref| > bandMul × bandPct × ref.
	ref, err := s.referencePrice(ctx, tx, t, in)
	if err != nil {
		return zero, err
	}
	dev := t.price.Sub(ref).Abs()
	bound := ref.Mul(t.bandPct.Div(decimal.NewFromInt(100))).Mul(s.bandMul)
	if !dev.GreaterThan(bound) {
		return zero, excerrors.New("INVALID_REQUEST", fmt.Sprintf(
			"trade %d price %s deviates %s from reference %s — inside the obvious-error band (%s×%s%%)",
			t.id, t.price, dev, ref, s.bandMul, t.bandPct))
	}
	return ref, nil
}

// referencePrice resolves the market-at-fill reference: caller-supplied
// evidence wins (documented override), else the priced seam (default: the
// most recent COMPLETED trade before the fill on the same instrument).
func (s *TradeBustService) referencePrice(ctx context.Context, tx pgx.Tx, t *tradeRow, in *BustRequest) (decimal.Decimal, error) {
	if in.ReferencePrice != nil && in.ReferencePrice.IsPositive() {
		return *in.ReferencePrice, nil
	}
	ref, err := s.ref(ctx, tx, t.instrument, t.createdAt)
	if err == nil && ref.IsPositive() {
		return ref, nil
	}
	return decimal.Zero, excerrors.New("INVALID_REQUEST", fmt.Sprintf(
		"trade %d: no market reference price available — attach reference_price evidence", t.id))
}

// pgxTradeRefPricer is the default BustReferencePricer: the latest
// COMPLETED trade on the instrument strictly before the fill (excludes
// BUSTED/PRICE_ADJUSTED rows so a corrected print never anchors a review).
func pgxTradeRefPricer(ctx context.Context, tx pgx.Tx, instrumentID int64, before time.Time) (decimal.Decimal, error) {
	var p decimal.Decimal
	err := tx.QueryRow(ctx, `
		SELECT price FROM trades
		 WHERE instrument_id=$1 AND created_at < $2
		   AND status = 'COMPLETED'
		 ORDER BY created_at DESC, id DESC LIMIT 1`,
		instrumentID, before).Scan(&p)
	if err != nil {
		return decimal.Zero, err
	}
	return p, nil
}

// ---------------------------------------------------------------------------
// Pending review lifecycle
// ---------------------------------------------------------------------------

// insertPending validates + records the PENDING_APPROVAL review row.
func (s *TradeBustService) insertPending(ctx context.Context, actor AdminActor, pre *tradeRow, in BustRequest) (*TradeBust, error) {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return nil, excerrors.Wrap("INTERNAL_ERROR", "bust request tx", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	locked, err := s.loadTrade(ctx, tx, in.TradeID)
	if err != nil {
		return nil, err
	}
	ref, err := s.checkEligibility(ctx, tx, locked, &in, 0)
	if err != nil {
		return nil, err
	}

	b, err := s.insertBustRow(ctx, tx, locked, in, actor.UserID, 0, BustStatusPending, ref, nil)
	if err != nil {
		return nil, err
	}
	if _, _, err := Log(ctx, tx, AuditEntry{
		AdminUserID: actor.UserID,
		Action:      "trade.bust.request",
		TargetType:  "trade",
		TargetID:    &in.TradeID,
		AfterState: map[string]any{
			"bust_id": b.ID, "action": in.Action, "status": b.Status,
			"adjusted_price":  decPtrString(in.AdjustedPrice, in.Action == BustActionPriceAdjust),
			"reference_price": ref.String(), "reason": in.Reason,
		},
		IPAddress: actor.ClientIP,
	}); err != nil {
		return nil, excerrors.Wrap("INTERNAL_ERROR", "audit log", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, excerrors.Wrap("INTERNAL_ERROR", "bust request commit", err)
	}
	return b, nil
}

// ApproveBust completes a pending review: a distinct Risk Manager+
// approver executes the correction atomically. The §7.3.4 window is
// measured at CLAIM time (the pending row), not approval — but a review
// still lapses after the 15-minute four-eyes window (BustReviewTTL);
// lapsed rows are marked REJECTED and refuse approval.
func (s *TradeBustService) ApproveBust(ctx context.Context, approver AdminActor, bustID int64) (*BustOutcome, error) {
	b, err := s.pendingForDecision(ctx, approver, bustID)
	if err != nil {
		return nil, err
	}
	pre, err := s.loadTrade(ctx, nil, b.TradeID)
	if err != nil {
		return nil, err
	}
	in := BustRequest{
		TradeID: b.TradeID, Action: b.Action, Reason: b.Reason,
	}
	if b.AdjustedPrice != nil {
		in.AdjustedPrice = *b.AdjustedPrice
	}
	if b.ReferencePrice != nil {
		in.ReferencePrice = b.ReferencePrice
	}
	outcome, err := s.execute(ctx, pre, b, b.InitiatedBy, approver.UserID, in, approver.ClientIP)
	if err != nil {
		return nil, err
	}
	return outcome, nil
}

// RejectBust turns down a pending review — distinct approver, same gate.
func (s *TradeBustService) RejectBust(ctx context.Context, approver AdminActor, bustID int64, note string) (*TradeBust, error) {
	b, err := s.pendingForDecision(ctx, approver, bustID)
	if err != nil {
		return nil, err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, excerrors.Wrap("INTERNAL_ERROR", "bust reject tx", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	err = tx.QueryRow(ctx, `
		UPDATE trade_busts SET status='REJECTED'
		 WHERE id=$1 AND status='PENDING_APPROVAL'
		RETURNING id`, bustID).Scan(&b.ID)
	if err == pgx.ErrNoRows {
		return nil, excerrors.New("INVALID_REQUEST",
			fmt.Sprintf("bust %d no longer pending", bustID))
	}
	if err != nil {
		return nil, excerrors.Wrap("INTERNAL_ERROR", "reject bust", err)
	}
	if _, _, err := Log(ctx, tx, AuditEntry{
		AdminUserID: approver.UserID,
		Action:      "trade.bust.reject",
		TargetType:  "trade",
		TargetID:    &b.TradeID,
		AfterState:  map[string]any{"bust_id": bustID, "status": "REJECTED", "note": note},
		IPAddress:   approver.ClientIP,
	}); err != nil {
		return nil, excerrors.Wrap("INTERNAL_ERROR", "audit log", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, excerrors.Wrap("INTERNAL_ERROR", "bust reject commit", err)
	}
	b.Status = BustStatusRejected
	return b, nil
}

// pendingForDecision loads a PENDING_APPROVAL bust FOR the decision:
// approver identity, distinctness and the review TTL are enforced here.
func (s *TradeBustService) pendingForDecision(ctx context.Context, approver AdminActor, bustID int64) (*TradeBust, error) {
	if err := s.requireBustRole(ctx, approver.UserID); err != nil {
		return nil, err
	}
	b, err := s.getBust(ctx, bustID)
	if err != nil {
		return nil, err
	}
	if b.Status != BustStatusPending {
		return nil, excerrors.New("INVALID_REQUEST",
			fmt.Sprintf("bust %d is %s — not decidable", bustID, b.Status))
	}
	if approver.UserID == b.InitiatedBy {
		return nil, excerrors.New("DUAL_CONTROL_VIOLATION",
			"the initiating admin cannot decide their own bust request (§8.2)")
	}
	if s.now().After(b.CreatedAt.Add(BustReviewTTL)) {
		// Lapse the row so the settlement hold releases.
		if _, err := s.pool.Exec(ctx,
			`UPDATE trade_busts SET status='REJECTED'
			  WHERE id=$1 AND status='PENDING_APPROVAL'`, bustID); err != nil {
			return nil, excerrors.Wrap("INTERNAL_ERROR", "lapse bust", err)
		}
		return nil, excerrors.New("DUAL_CONTROL_REQUIRED",
			"bust review window elapsed — the request must be re-submitted inside the §7.3.4 deadline")
	}
	return b, nil
}

// ExpirePending lapses PENDING_APPROVAL rows older than BustReviewTTL —
// a rejected-by-timeout review releases the settlement hold. Sweep entry
// (housekeeping ticker, same cadence as DualControlService.ExpireDue).
func (s *TradeBustService) ExpirePending(ctx context.Context) (int64, error) {
	tag, err := s.pool.Exec(ctx, `
		UPDATE trade_busts SET status='REJECTED'
		 WHERE status='PENDING_APPROVAL'
		   AND created_at <= now() - interval '15 minutes'`)
	if err != nil {
		return 0, excerrors.Wrap("INTERNAL_ERROR", "expire pending busts", err)
	}
	return tag.RowsAffected(), nil
}

// HasPendingBust reports whether a trade is under obvious-error review —
// the settlement-hold seam: the dispatch job MUST consult this before
// claiming a trade's settlement_instructions (spec §5.29 "settlement is
// held while review is pending").
func (s *TradeBustService) HasPendingBust(ctx context.Context, tradeID int64) (bool, error) {
	var n int
	if err := s.pool.QueryRow(ctx,
		`SELECT count(*) FROM trade_busts
		  WHERE trade_id=$1 AND status='PENDING_APPROVAL'`, tradeID).Scan(&n); err != nil {
		return false, excerrors.Wrap("INTERNAL_ERROR", "pending bust probe", err)
	}
	return n > 0, nil
}

// PendingBustTradeIDs is the batch form of the settlement-hold seam: the
// dispatch job passes its candidate trade_id set and gets back the subset
// under review — one probe per claim batch instead of one per instruction.
func (s *TradeBustService) PendingBustTradeIDs(ctx context.Context, tradeIDs []int64) (map[int64]bool, error) {
	held := make(map[int64]bool, len(tradeIDs))
	if len(tradeIDs) == 0 {
		return held, nil
	}
	rows, err := s.pool.Query(ctx,
		`SELECT trade_id FROM trade_busts
		  WHERE trade_id = ANY($1) AND status='PENDING_APPROVAL'`, tradeIDs)
	if err != nil {
		return nil, excerrors.Wrap("INTERNAL_ERROR", "pending bust batch probe", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, excerrors.Wrap("INTERNAL_ERROR", "scan pending bust", err)
		}
		held[id] = true
	}
	return held, rows.Err()
}

// GetBust loads one trade_busts row.
func (s *TradeBustService) GetBust(ctx context.Context, id int64) (*TradeBust, error) {
	return s.getBust(ctx, id)
}

func (s *TradeBustService) getBust(ctx context.Context, id int64) (*TradeBust, error) {
	var b TradeBust
	err := s.pool.QueryRow(ctx, `
		SELECT id, trade_id, action, adjusted_price, reason, reference_price,
		       initiated_by, approved_by, status, created_at, executed_at
		  FROM trade_busts WHERE id=$1`, id).
		Scan(&b.ID, &b.TradeID, &b.Action, &b.AdjustedPrice, &b.Reason,
			&b.ReferencePrice, &b.InitiatedBy, &b.ApprovedBy, &b.Status,
			&b.CreatedAt, &b.ExecutedAt)
	if err == pgx.ErrNoRows {
		return nil, excerrors.New("NOT_FOUND", fmt.Sprintf("trade bust %d not found", id))
	}
	if err != nil {
		return nil, excerrors.Wrap("INTERNAL_ERROR", "load bust", err)
	}
	return &b, nil
}

// ---------------------------------------------------------------------------
// Execution — the atomic correction.
// ---------------------------------------------------------------------------

// execute runs the correction inside one SERIALIZABLE transaction holding
// the counterparty account mutexes. bustRow nil → the synchronous
// request+approve path inserts the row EXECUTED; non-nil → the pending
// review row is flipped.
func (s *TradeBustService) execute(ctx context.Context, pre *tradeRow, bustRow *TradeBust,
	initiator, approver int64, in BustRequest, clientIP string) (*BustOutcome, error) {

	accounts := []int64{pre.buyer, pre.seller}
	sort.Slice(accounts, func(a, b int) bool { return accounts[a] < accounts[b] })
	token := fmt.Sprintf("bust-%d-%d", pre.id, s.now().UnixNano())
	held, err := s.lockAccounts(ctx, accounts, token)
	if err != nil {
		return nil, err
	}
	defer s.unlockAccounts(held, token)

	var outcome *BustOutcome
	var events []ledger.BalanceEvent
	var bustID int64
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return nil, excerrors.Wrap("INTERNAL_ERROR", "bust execute tx", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	t, err := s.loadTrade(ctx, tx, in.TradeID)
	if err != nil {
		return nil, err
	}
	var excludeBust int64
	if bustRow != nil {
		excludeBust = bustRow.ID // the row being decided must not trip the pending gate
	}
	ref, err := s.checkEligibility(ctx, tx, t, &in, excludeBust)
	if err != nil {
		return nil, err
	}
	// On the approve path the pending row itself is the review — re-verify
	// it's still decidable under the row lock (double-decision race).
	if bustRow != nil {
		// Re-validate the pending row inside the lock (double decision race).
		var st string
		if err := tx.QueryRow(ctx,
			`SELECT status FROM trade_busts WHERE id=$1 FOR UPDATE`, bustRow.ID).Scan(&st); err != nil {
			return nil, excerrors.Wrap("INTERNAL_ERROR", "lock bust row", err)
		}
		if st != BustStatusPending {
			return nil, excerrors.New("INVALID_REQUEST",
				fmt.Sprintf("bust %d is %s — not decidable", bustRow.ID, st))
		}
	}

	// 1. Journal — balanced reversal (BUST) or quote-delta (PRICE_ADJUST).
	j, err := s.buildJournal(t, in, initiator)
	if err != nil {
		return nil, err
	}
	if bustRow != nil {
		j.IdempotencyKey = fmt.Sprintf("trade-bust:%d", bustRow.ID)
	} else {
		j.IdempotencyKey = fmt.Sprintf("trade-bust:sync:%d:%d", t.id, s.now().UnixNano())
	}
	res, err := s.poster.PostJournal(ctx, tx, j)
	if err != nil {
		return nil, err
	}
	events = res.Events

	// 2. Post-trade lifecycle flag (migration 051 column) — flagged, never
	//    deleted (MiFID record-keeping, Task DoD).
	newStatus := TradeStatusBusted
	if in.Action == BustActionPriceAdjust {
		newStatus = TradeStatusPriceAdjusted
	}
	if tag, err := tx.Exec(ctx,
		`UPDATE trades SET status=$2 WHERE id=$1 AND status='COMPLETED'`,
		t.id, newStatus); err != nil {
		return nil, excerrors.Wrap("INTERNAL_ERROR", "flag trade", err)
	} else if tag.RowsAffected() != 1 {
		return nil, excerrors.New("INVALID_REQUEST",
			fmt.Sprintf("trade %d no longer COMPLETED", t.id))
	}

	// 3. Settlement legs: void (BUST) or amend quote legs (PRICE_ADJUST) —
	//    only PENDING + undispatched legs are touchable (§7.3.4 gate ran
	//    above; the predicate repeats it for defence in depth).
	settlement, err := s.applySettlement(ctx, tx, t, in)
	if err != nil {
		return nil, err
	}

	// 4. Positions: rebuild each counterparty's aggregate from
	//    position_fills — BUST excludes the trade's fills, PRICE_ADJUST
	//    rewrites their price first.
	if err := s.rebuildPositions(ctx, tx, t, in); err != nil {
		return nil, err
	}

	// 5. The §5.29 review record.
	var b *TradeBust
	if bustRow == nil {
		now := s.now().UTC()
		b, err = s.insertBustRow(ctx, tx, t, in, initiator, approver, BustStatusExecuted, ref, &now)
	} else {
		b, err = s.completeBustRow(ctx, tx, bustRow.ID, approver, ref)
	}
	if err != nil {
		return nil, err
	}
	bustID = b.ID

	// 6. Audit — both principals on the row (§8.2/§24 #138 evidence).
	if _, _, err := Log(ctx, tx, AuditEntry{
		AdminUserID: approver,
		Action:      "trade.bust.execute",
		TargetType:  "trade",
		TargetID:    &t.id,
		BeforeState: map[string]any{
			"status": "COMPLETED", "price": t.price.String(), "quantity": t.qty.String(),
		},
		AfterState: map[string]any{
			"bust_id": bustID, "action": in.Action, "status": newStatus,
			"journal_id": res.JournalID, "reference_price": ref.String(),
			"initiated_by": initiator, "approved_by": approver,
			"adjusted_price": decPtrString(in.AdjustedPrice, in.Action == BustActionPriceAdjust),
			"settlement":     settlement,
		},
		IPAddress: clientIP,
	}); err != nil {
		return nil, excerrors.Wrap("INTERNAL_ERROR", "audit log", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, excerrors.Wrap("INTERNAL_ERROR", "bust commit", err)
	}

	outcome = &BustOutcome{Bust: *b, JournalID: res.JournalID, Settlement: settlement}

	// Post-commit fan-out — funds are final; dispatch failure demands
	// resync downstream (documented contract, mirrors ledger.Post).
	if err := s.dispatchBalanceEvents(ctx, events); err != nil {
		outcome.Dispatched = false
		return outcome, err
	}
	outcome.Dispatched = true
	s.notifyCounterparties(ctx, t, in, b)
	return outcome, nil
}

func (s *TradeBustService) lockAccounts(ctx context.Context, ids []int64, token string) ([]int64, error) {
	held := make([]int64, 0, len(ids))
	for _, id := range ids {
		ok, err := s.locks.TryLockAccount(ctx, fmt.Sprint(id), token, 10*time.Second)
		if err != nil {
			s.unlockAccounts(held, token)
			return nil, excerrors.Wrap(ledger.CodeLedgerLockUnavailable,
				fmt.Sprintf("bust: lock account %d", id), err)
		}
		if !ok {
			s.unlockAccounts(held, token)
			return nil, excerrors.New(ledger.CodeAccountBusy,
				fmt.Sprintf("bust: account %d locked by another operation", id))
		}
		held = append(held, id)
	}
	return held, nil
}

func (s *TradeBustService) unlockAccounts(ids []int64, token string) {
	for _, id := range ids {
		_, _ = s.locks.UnlockAccount(context.Background(), fmt.Sprint(id), token)
	}
}

// dispatchBalanceEvents publishes committed BalanceChanged payloads to
// account.balance.changed.{account_id} (spec §5.3 invariant 4 — dispatch
// after commit is a MUST; a nil publisher is a dev-no-op).
func (s *TradeBustService) dispatchBalanceEvents(ctx context.Context, events []ledger.BalanceEvent) error {
	if s.pub == nil || len(events) == 0 {
		return nil
	}
	for _, ev := range events {
		payload, err := json.Marshal(ev)
		if err != nil {
			return excerrors.Wrap(ledger.CodeBalanceDispatchFailed,
				"bust: marshal balance event", err)
		}
		if err := s.pub.Publish(ctx, ledger.BalanceChangedSubject(ev.AccountID), payload); err != nil {
			return excerrors.Wrap(ledger.CodeBalanceDispatchFailed,
				fmt.Sprintf("bust: dispatch account %d event (funds committed — resync downstream)", ev.AccountID), err)
		}
	}
	return nil
}

// notifyCounterparties tells BOTH sides of the corrected trade — the
// Task-15.3.5 DoD. Notification failure never masks a committed bust.
func (s *TradeBustService) notifyCounterparties(ctx context.Context, t *tradeRow, in BustRequest, b *TradeBust) {
	if s.notify == nil {
		s.logf("trade-bust: notifier not wired — counterparties of trade %d not notified", t.id)
		return
	}
	event := BustEventBusted
	if in.Action == BustActionPriceAdjust {
		event = BustEventPriceAdjusted
	}
	payload := map[string]any{
		"trade_id": t.id, "bust_id": b.ID, "symbol": t.symbol,
		"action": in.Action, "price": t.price.String(), "quantity": t.qty.String(),
		"reason": in.Reason,
	}
	if in.Action == BustActionPriceAdjust {
		payload["adjusted_price"] = in.AdjustedPrice.String()
	}
	// account → user resolution for the notifications seam.
	rows, err := s.pool.Query(ctx,
		`SELECT id, user_id FROM accounts WHERE id = ANY($1)`,
		[]int64{t.buyer, t.seller})
	if err != nil {
		s.logf("trade-bust: counterparty lookup failed: %v", err)
		return
	}
	defer rows.Close()
	notified := map[int64]bool{}
	for rows.Next() {
		var acctID, userID int64
		if err := rows.Scan(&acctID, &userID); err != nil {
			s.logf("trade-bust: counterparty scan: %v", err)
			return
		}
		if notified[userID] {
			continue // self-trade — one notice
		}
		notified[userID] = true
		if _, err := s.notify.Notify(ctx, userID, event, payload); err != nil {
			s.logf("trade-bust: notify user %d failed: %v", userID, err)
		}
	}
}

// ---------------------------------------------------------------------------
// Row helpers
// ---------------------------------------------------------------------------

// loadTrade reads the trade with its instrument/order context. tx nil →
// pool read (pre-scan); non-nil → FOR UPDATE inside the caller's tx.
func (s *TradeBustService) loadTrade(ctx context.Context, tx pgx.Tx, tradeID int64) (*tradeRow, error) {
	q := `
		SELECT t.id, t.instrument_id, i.symbol, i.base_currency, i.quote_currency,
		       t.buy_order_id, t.sell_order_id, t.buyer_account_id, t.seller_account_id,
		       t.price, t.quantity,
		       COALESCE(t.buyer_fee,0), COALESCE(t.seller_fee,0),
		       t.status::text, t.created_at,
		       GREATEST(i.price_band_pct_up, i.price_band_pct_down),
		       COALESCE(bo.settlement_intent::text, 'ROLLING_MARGIN'),
		       COALESCE(so.settlement_intent::text, 'ROLLING_MARGIN')
		  FROM trades t
		  JOIN instruments i ON i.id = t.instrument_id
		  JOIN orders bo ON bo.id = t.buy_order_id
		  JOIN orders so ON so.id = t.sell_order_id
		 WHERE t.id = $1`
	if tx != nil {
		q += " FOR UPDATE OF t"
	}
	var (
		tr         tradeRow
		intB, intS string
	)
	qr := func(ctx context.Context, q string, args ...any) pgx.Row {
		if tx != nil {
			return tx.QueryRow(ctx, q, args...)
		}
		return s.pool.QueryRow(ctx, q, args...)
	}
	err := qr(ctx, q, tradeID).Scan(
		&tr.id, &tr.instrument, &tr.symbol, &tr.base, &tr.quote,
		&tr.buyOrder, &tr.sellOrder, &tr.buyer, &tr.seller,
		&tr.price, &tr.qty, &tr.buyerFee, &tr.sellerFee,
		&tr.status, &tr.createdAt, &tr.bandPct, &intB, &intS)
	if err == pgx.ErrNoRows {
		return nil, excerrors.New("NOT_FOUND", fmt.Sprintf("trade %d not found", tradeID))
	}
	if err != nil {
		return nil, excerrors.Wrap("INTERNAL_ERROR", fmt.Sprintf("load trade %d", tradeID), err)
	}
	if intB != intS {
		return nil, excerrors.New("SETTLEMENT_INTENT_MISMATCH", fmt.Sprintf(
			"trade %d legs disagree on settlement intent (%s vs %s) — matching anomaly",
			tr.id, intB, intS))
	}
	tr.intent = intB
	return &tr, nil
}

// insertBustRow writes the §5.29 record (PENDING or EXECUTED).
func (s *TradeBustService) insertBustRow(ctx context.Context, tx pgx.Tx, t *tradeRow,
	in BustRequest, initiator, approver int64, status string, ref decimal.Decimal,
	executedAt *time.Time) (*TradeBust, error) {

	var adj any
	if in.Action == BustActionPriceAdjust {
		adj = in.AdjustedPrice.String()
	}
	var appr any
	if approver > 0 {
		appr = approver
	}
	var b TradeBust
	err := tx.QueryRow(ctx, `
		INSERT INTO trade_busts
		    (trade_id, action, adjusted_price, reason, reference_price,
		     initiated_by, approved_by, status, executed_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)
		RETURNING id, trade_id, action, adjusted_price, reason, reference_price,
		          initiated_by, approved_by, status, created_at, executed_at`,
		t.id, in.Action, adj, in.Reason, ref.String(), initiator, appr,
		status, executedAt).
		Scan(&b.ID, &b.TradeID, &b.Action, &b.AdjustedPrice, &b.Reason,
			&b.ReferencePrice, &b.InitiatedBy, &b.ApprovedBy, &b.Status,
			&b.CreatedAt, &b.ExecutedAt)
	if err != nil {
		var pgErr *pgconn.PgError
		if stderrors.As(err, &pgErr) && pgErr.Code == "23505" {
			return nil, excerrors.New("TRADE_BUST_PENDING",
				fmt.Sprintf("trade %d already has a pending or executed correction", t.id))
		}
		return nil, excerrors.Wrap("INTERNAL_ERROR", "insert bust row", err)
	}
	return &b, nil
}

// completeBustRow flips a PENDING_APPROVAL row to EXECUTED.
func (s *TradeBustService) completeBustRow(ctx context.Context, tx pgx.Tx,
	bustID, approver int64, ref decimal.Decimal) (*TradeBust, error) {

	var b TradeBust
	err := tx.QueryRow(ctx, `
		UPDATE trade_busts
		   SET status='EXECUTED', approved_by=$2, executed_at=now(),
		       reference_price=$3
		 WHERE id=$1 AND status='PENDING_APPROVAL'
		RETURNING id, trade_id, action, adjusted_price, reason, reference_price,
		          initiated_by, approved_by, status, created_at, executed_at`,
		bustID, approver, ref.String()).
		Scan(&b.ID, &b.TradeID, &b.Action, &b.AdjustedPrice, &b.Reason,
			&b.ReferencePrice, &b.InitiatedBy, &b.ApprovedBy, &b.Status,
			&b.CreatedAt, &b.ExecutedAt)
	if err == pgx.ErrNoRows {
		return nil, excerrors.New("INVALID_REQUEST",
			fmt.Sprintf("bust %d no longer pending", bustID))
	}
	if err != nil {
		return nil, excerrors.Wrap("INTERNAL_ERROR", "complete bust row", err)
	}
	return &b, nil
}

// ---------------------------------------------------------------------------
// Journal construction (pure — unit-tested; mirrors the fill contract).
// ---------------------------------------------------------------------------

// buildJournal assembles the balanced correction journal. ReferenceID is
// the trade id; the idempotency key is set by the caller.
func (s *TradeBustService) buildJournal(t *tradeRow, in BustRequest, initiator int64) (ledger.Journal, error) {
	if in.Action == BustActionPriceAdjust {
		return s.buildAdjustJournal(t, in, initiator)
	}
	return s.buildBustJournal(t, in, initiator)
}

// buildBustJournal is the BUST full reversal — the mirror image of
// buildRollingJournal / buildDeliveryJournal (settlement/balance_service.go).
func (s *TradeBustService) buildBustJournal(t *tradeRow, in BustRequest, initiator int64) (ledger.Journal, error) {
	q := t.qty
	qa := t.qty.Mul(t.price).Round(8)
	buyerCredit := q.Sub(t.buyerFee)
	sellerCredit := qa.Sub(t.sellerFee)
	if buyerCredit.IsNegative() || sellerCredit.IsNegative() {
		return ledger.Journal{}, excerrors.New("FEE_EXCEEDS_PROCEEDS",
			fmt.Sprintf("trade %d fee exceeds delivered leg — cannot reverse", t.id))
	}
	postedBy := fmt.Sprintf("admin-bust:%d", initiator)

	if t.intent == "PHYSICAL_DELIVERY" {
		// PD fills never touched wallet proceeds — the deliverables moved
		// available→locked and were reclassified 2010→2011. Unwind both.
		j := ledger.Journal{
			EntryType:   ledger.EntryAdjustment,
			ReferenceID: t.id,
			Description: trunc255(fmt.Sprintf("bust trade %d %s — release pending delivery", t.id, t.symbol)),
			PostedBy:    postedBy,
			Lines: []ledger.Line{
				ledger.DebitLine(ledger.PendingSettlementDelivery(t.quote), t.quote, qa,
					fmt.Sprintf("unwind pending delivery quote leg trade %d", t.id)),
				ledger.CreditLine(ledger.CustomerLiability(t.quote), t.quote, qa,
					fmt.Sprintf("buyer %d quote liability restored", t.buyer)),
				ledger.DebitLine(ledger.PendingSettlementDelivery(t.base), t.base, q,
					fmt.Sprintf("unwind pending delivery base leg trade %d", t.id)),
				ledger.CreditLine(ledger.CustomerLiability(t.base), t.base, q,
					fmt.Sprintf("seller %d base liability restored", t.seller)),
			},
			Effects: []ledger.AccountEffect{
				{AccountID: t.buyer, Currency: t.quote,
					AvailableDelta: qa, LockedDelta: qa.Neg()},
				{AccountID: t.seller, Currency: t.base,
					AvailableDelta: q, LockedDelta: q.Neg()},
			},
		}
		return j, j.Validate()
	}

	// ROLLING_MARGIN reversal.
	j := ledger.Journal{
		EntryType:   ledger.EntryAdjustment,
		ReferenceID: t.id,
		Description: trunc255(fmt.Sprintf("bust trade %d %s full reversal", t.id, t.symbol)),
		PostedBy:    postedBy,
	}
	// QUOTE: re-credit buyer's consumed reservation; claw back seller's
	// net proceeds; reverse the seller-fee revenue.
	j.Lines = append(j.Lines,
		ledger.CreditLine(ledger.CustomerLiability(t.quote), t.quote, qa,
			fmt.Sprintf("buyer %d quote restored (bust)", t.buyer)),
		ledger.DebitLine(ledger.CustomerLiability(t.quote), t.quote, sellerCredit,
			fmt.Sprintf("seller %d quote proceeds clawed back", t.seller)),
	)
	if t.sellerFee.IsPositive() {
		j.Lines = append(j.Lines,
			ledger.DebitLine(ledger.TradingFeeRevenue(t.quote), t.quote, t.sellerFee,
				fmt.Sprintf("seller %d fee reversal", t.seller)))
	}
	// BASE symmetric.
	j.Lines = append(j.Lines,
		ledger.CreditLine(ledger.CustomerLiability(t.base), t.base, q,
			fmt.Sprintf("seller %d base restored (bust)", t.seller)),
		ledger.DebitLine(ledger.CustomerLiability(t.base), t.base, buyerCredit,
			fmt.Sprintf("buyer %d base proceeds clawed back", t.buyer)),
	)
	if t.buyerFee.IsPositive() {
		j.Lines = append(j.Lines,
			ledger.DebitLine(ledger.TradingFeeRevenue(t.base), t.base, t.buyerFee,
				fmt.Sprintf("buyer %d fee reversal", t.buyer)))
	}
	j.Effects = []ledger.AccountEffect{
		{AccountID: t.buyer, Currency: t.quote, AvailableDelta: qa},
		{AccountID: t.buyer, Currency: t.base,
			AvailableDelta: buyerCredit.Neg(), AllowNegative: true},
		{AccountID: t.seller, Currency: t.base, AvailableDelta: q},
		{AccountID: t.seller, Currency: t.quote,
			AvailableDelta: sellerCredit.Neg(), AllowNegative: true},
	}
	return j, j.Validate()
}

// buildAdjustJournal posts the signed quote-currency delta of repricing
// the fill to adjusted_price (fees stand; the base leg is unchanged).
func (s *TradeBustService) buildAdjustJournal(t *tradeRow, in BustRequest, initiator int64) (ledger.Journal, error) {
	qaOld := t.qty.Mul(t.price).Round(8)
	qaNew := t.qty.Mul(in.AdjustedPrice).Round(8)
	delta := qaNew.Sub(qaOld)
	if delta.IsZero() {
		return ledger.Journal{}, excerrors.New("INVALID_REQUEST",
			"adjusted_price equals the fill price — nothing to correct")
	}
	postedBy := fmt.Sprintf("admin-bust:%d", initiator)
	d := delta.Abs()

	if t.intent == "PHYSICAL_DELIVERY" {
		// Only the buyer's quote obligation moves (the seller's deliverable
		// is the base leg — quantity unchanged).
		var lines []ledger.Line
		var eff []ledger.AccountEffect
		if delta.IsPositive() {
			lines = []ledger.Line{
				ledger.DebitLine(ledger.CustomerLiability(t.quote), t.quote, d,
					fmt.Sprintf("buyer %d additional quote obligation", t.buyer)),
				ledger.CreditLine(ledger.PendingSettlementDelivery(t.quote), t.quote, d,
					fmt.Sprintf("pending delivery top-up trade %d", t.id)),
			}
			eff = []ledger.AccountEffect{{
				AccountID: t.buyer, Currency: t.quote,
				AvailableDelta: d.Neg(), LockedDelta: d, AllowNegative: true}}
		} else {
			lines = []ledger.Line{
				ledger.DebitLine(ledger.PendingSettlementDelivery(t.quote), t.quote, d,
					fmt.Sprintf("pending delivery reduction trade %d", t.id)),
				ledger.CreditLine(ledger.CustomerLiability(t.quote), t.quote, d,
					fmt.Sprintf("buyer %d quote obligation reduced", t.buyer)),
			}
			eff = []ledger.AccountEffect{{
				AccountID: t.buyer, Currency: t.quote,
				AvailableDelta: d, LockedDelta: d.Neg()}}
		}
		j := ledger.Journal{
			EntryType:   ledger.EntryAdjustment,
			ReferenceID: t.id,
			Description: trunc255(fmt.Sprintf("price-adjust trade %d %s → %s", t.id, t.symbol, in.AdjustedPrice)),
			PostedBy:    postedBy, Lines: lines, Effects: eff,
		}
		return j, j.Validate()
	}

	// ROLLING_MARGIN: buyer pays delta more (or less); seller receives it.
	var lines []ledger.Line
	var eff []ledger.AccountEffect
	if delta.IsPositive() {
		lines = []ledger.Line{
			ledger.DebitLine(ledger.CustomerLiability(t.quote), t.quote, d,
				fmt.Sprintf("buyer %d quote delta on reprice", t.buyer)),
			ledger.CreditLine(ledger.CustomerLiability(t.quote), t.quote, d,
				fmt.Sprintf("seller %d quote delta on reprice", t.seller)),
		}
		eff = []ledger.AccountEffect{
			{AccountID: t.buyer, Currency: t.quote,
				AvailableDelta: d.Neg(), AllowNegative: true},
			{AccountID: t.seller, Currency: t.quote, AvailableDelta: d},
		}
	} else {
		lines = []ledger.Line{
			ledger.DebitLine(ledger.CustomerLiability(t.quote), t.quote, d,
				fmt.Sprintf("seller %d quote refund on reprice", t.seller)),
			ledger.CreditLine(ledger.CustomerLiability(t.quote), t.quote, d,
				fmt.Sprintf("buyer %d quote refund on reprice", t.buyer)),
		}
		eff = []ledger.AccountEffect{
			{AccountID: t.buyer, Currency: t.quote, AvailableDelta: d},
			{AccountID: t.seller, Currency: t.quote,
				AvailableDelta: d.Neg(), AllowNegative: true},
		}
	}
	j := ledger.Journal{
		EntryType:   ledger.EntryAdjustment,
		ReferenceID: t.id,
		Description: trunc255(fmt.Sprintf("price-adjust trade %d %s → %s", t.id, t.symbol, in.AdjustedPrice)),
		PostedBy:    postedBy, Lines: lines, Effects: eff,
	}
	return j, j.Validate()
}

// ---------------------------------------------------------------------------
// Settlement legs + position rebuild
// ---------------------------------------------------------------------------

// applySettlement voids (BUST) or amends (PRICE_ADJUST) the trade's
// settlement legs. Returns the outcome label for the audit trail.
func (s *TradeBustService) applySettlement(ctx context.Context, tx pgx.Tx,
	t *tradeRow, in BustRequest) (string, error) {

	if in.Action == BustActionPriceAdjust {
		qaNew := t.qty.Mul(in.AdjustedPrice).Round(8)
		tag, err := tx.Exec(ctx, `
			UPDATE settlement_instructions
			   SET amount=$2, updated_at=now()
			 WHERE trade_id=$1 AND currency=$3
			   AND status='PENDING' AND dispatched_at IS NULL`,
			t.id, qaNew.String(), t.quote)
		if err != nil {
			return "", excerrors.Wrap("INTERNAL_ERROR", "amend settlement legs", err)
		}
		if tag.RowsAffected() == 0 {
			return "NONE", nil
		}
		return "AMENDED", nil
	}
	tag, err := tx.Exec(ctx, `
		UPDATE settlement_instructions
		   SET status='VOID', updated_at=now()
		 WHERE trade_id=$1 AND status='PENDING' AND dispatched_at IS NULL`,
		t.id)
	if err != nil {
		return "", excerrors.Wrap("INTERNAL_ERROR", "void settlement legs", err)
	}
	if tag.RowsAffected() == 0 {
		return "NONE", nil
	}
	return "VOIDED", nil
}

// rebuildPositions recomputes each counterparty's position aggregate from
// position_fills — the fills table is the position's source of truth, so
// a bust removes the trade's fills and a price-adjust reprices them, then
// the (account, instrument) position is replayed from zero. Deterministic
// and exact regardless of what came before/after (partial fills, later
// hedges — the SDD edge cases).
func (s *TradeBustService) rebuildPositions(ctx context.Context, tx pgx.Tx,
	t *tradeRow, in BustRequest) error {

	for _, accountID := range []int64{t.buyer, t.seller} {
		if in.Action == BustActionPriceAdjust {
			// Reprice the persisted fill records first so the replay is a
			// plain forward pass (audit of the change lives on trade_busts +
			// admin_audit_log; trades.price stays the immutable original).
			if _, err := tx.Exec(ctx, `
				UPDATE position_fills SET price=$3
				 WHERE trade_id=$1 AND account_id=$2`,
				t.id, accountID, in.AdjustedPrice.String()); err != nil {
				return excerrors.Wrap("INTERNAL_ERROR", "reprice position fills", err)
			}
		}
		if err := s.replayPosition(ctx, tx, accountID, t.instrument,
			map[int64]bool{t.id: in.Action == BustActionBust}); err != nil {
			return err
		}
	}
	return nil
}

// replayPosition recomputes the (account, instrument) position from its
// position_fills in applied order, skipping busted trades. The apply
// rules mirror settlement.applyFill (Phase-03 Task 3.3.2) — kept local so
// the correction rides the bust transaction, not the sibling package's
// unexported internals.
func (s *TradeBustService) replayPosition(ctx context.Context, tx pgx.Tx,
	accountID, instrumentID int64, skip map[int64]bool) error {

	// Lock the position row first (create-if-absent not needed — a flat
	// result simply zeroes the row).
	var (
		posID      int64
		qty, entry decimal.Decimal
		side       string
		mark       decimal.Decimal
		hasMark    bool
		realized   decimal.Decimal
		rowExists  bool
	)
	err := tx.QueryRow(ctx, `
		SELECT id, side::text, quantity, entry_price,
		       COALESCE(mark_price,0), realized_pnl
		  FROM positions
		 WHERE account_id=$1 AND instrument_id=$2
		 FOR UPDATE`, accountID, instrumentID).
		Scan(&posID, &side, &qty, &entry, &mark, &realized)
	switch {
	case err == pgx.ErrNoRows:
		rowExists = false
	case err != nil:
		return excerrors.Wrap("INTERNAL_ERROR", "lock position", err)
	default:
		rowExists = true
		hasMark = mark.IsPositive()
	}

	rows, err := tx.Query(ctx, `
		SELECT trade_id, side, quantity, price
		  FROM position_fills
		 WHERE account_id=$1 AND instrument_id=$2
		 ORDER BY applied_at, trade_id`, accountID, instrumentID)
	if err != nil {
		return excerrors.Wrap("INTERNAL_ERROR", "load position fills", err)
	}
	defer rows.Close()

	// Replay from flat.
	signed := decimal.Zero
	entry = decimal.Zero
	realizedTotal := decimal.Zero
	curSide := "LONG"
	for rows.Next() {
		var fid int64
		var fSide string
		var fq, fp decimal.Decimal
		if err := rows.Scan(&fid, &fSide, &fq, &fp); err != nil {
			return excerrors.Wrap("INTERNAL_ERROR", "scan position fill", err)
		}
		if skip[fid] {
			continue
		}
		if fSide == "SELL" {
			fq = fq.Neg()
		}
		realizedDelta := decimal.Zero
		newSigned := signed.Add(fq)
		switch {
		case signed.IsZero() || signed.Sign() == fq.Sign():
			// Open / increase: VWAP entry.
			total := signed.Abs().Add(fq.Abs())
			if signed.IsZero() {
				entry = fp
			} else {
				entry = signed.Abs().Mul(entry).Add(fq.Abs().Mul(fp)).Div(total)
			}
			signed = newSigned
			if signed.IsNegative() {
				curSide = "SHORT"
			} else {
				curSide = "LONG"
			}
		case fq.Abs().LessThan(signed.Abs()):
			// Partial close.
			closed := fq.Abs()
			realizedDelta = fp.Sub(entry).Mul(closed).Mul(decimal.NewFromInt(int64(signed.Sign())))
			signed = newSigned
		default:
			// Full close / reversal.
			realizedDelta = fp.Sub(entry).Mul(signed.Abs()).Mul(decimal.NewFromInt(int64(signed.Sign())))
			signed = newSigned
			if signed.IsZero() {
				entry = decimal.Zero
			} else {
				entry = fp
				if signed.IsNegative() {
					curSide = "SHORT"
				} else {
					curSide = "LONG"
				}
			}
		}
		realizedTotal = realizedTotal.Add(realizedDelta)
	}
	if err := rows.Err(); err != nil {
		return excerrors.Wrap("INTERNAL_ERROR", "position fills iterate", err)
	}

	newQty := signed.Abs()
	unrealized := decimal.Zero
	if hasMark && !newQty.IsZero() {
		unrealized = mark.Sub(entry).Mul(signed)
	}
	if !rowExists {
		if newQty.IsZero() && realizedTotal.IsZero() {
			return nil // nothing ever existed
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO positions
			    (account_id, instrument_id, side, quantity, entry_price,
			     mark_price, unrealized_pnl, realized_pnl, updated_at)
			VALUES ($1,$2,$3,$4,$5,NULLIF($6,0),$7,$8,now())`,
			accountID, instrumentID, curSide, newQty.String(), entry.String(),
			mark.String(), unrealized.String(), realizedTotal.String()); err != nil {
			return excerrors.Wrap("INTERNAL_ERROR", "rebuild position insert", err)
		}
		return nil
	}
	if _, err := tx.Exec(ctx, `
		UPDATE positions
		   SET side=$3, quantity=$4, entry_price=$5,
		       unrealized_pnl=$6, realized_pnl=$7, updated_at=now()
		 WHERE id=$1 AND account_id=$2`,
		posID, accountID, curSide, newQty.String(), entry.String(),
		unrealized.String(), realizedTotal.String()); err != nil {
		return excerrors.Wrap("INTERNAL_ERROR", "rebuild position update", err)
	}
	return nil
}

// decPtrString renders the adjusted price for audit JSON ("" when absent).
func decPtrString(d decimal.Decimal, present bool) string {
	if !present {
		return ""
	}
	return d.String()
}
