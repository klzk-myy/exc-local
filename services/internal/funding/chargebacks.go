// Chargeback service — Task 5.3.18, Phase-05 AC #31.
//
// Admin (Finance Ops per route metadata) opens a dispute against a
// funding transaction: POST /api/v1/admin/chargebacks. The record goes
// DISPUTE_OPENED → EVIDENCE_COLLECTED (automated bundle: disputed funding
// row, account trades in the window, freeze/audit history — each row
// sha256-hashed JSONB) → SUBMITTED → RESOLVED_WON | RESOLVED_LOST.
// Transitions are guarded by INVALID_LIFECYCLE_TRANSITION and every step
// writes admin_audit_log (spec §5.9). An optional freeze at open runs
// through accounts.FreezeService — dual control (distinct approver) is
// required by that service and propagates DUAL_CONTROL_REQUIRED.
//
// Balance impact: Phase-05 does NOT debit the client on RESOLVED_LOST —
// the clawback posting is a Phase-11/24 rail action; the dispute record +
// audit trail are the Phase-05 surface.
package funding

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"exchange/internal/accounts"
)

// ChargebackService wires the dispute lifecycle to the store + freezer.
type ChargebackService struct {
	store   Store
	freezer Freezer // optional — required only when freeze is requested
	clock   func() time.Time
	logf    func(format string, args ...any)
}

// NewChargebackService wires the service.
func NewChargebackService(store Store, freezer Freezer) (*ChargebackService, error) {
	if store == nil {
		return nil, fmt.Errorf("funding: chargeback service requires a store")
	}
	return &ChargebackService{store: store, freezer: freezer, clock: time.Now}, nil
}

// WithLogger wires a diagnostics sink.
func (s *ChargebackService) WithLogger(f func(format string, args ...any)) *ChargebackService {
	s.logf = f
	return s
}

// ---------------------------------------------------------------------------
// Create
// ---------------------------------------------------------------------------

// CreateChargebackRequest is the POST /api/v1/admin/chargebacks payload.
type CreateChargebackRequest struct {
	AccountID            int64
	FundingTransactionID *int64 // disputed deposit/funding row (optional)
	CardNetwork          string // visa | mastercard | … (optional)
	Currency             string
	Amount               string
	Reason               string
	FreezeAccount        bool  // freeze the account while the dispute runs
	ApproverUserID       int64 // dual-control approver — required iff FreezeAccount
	ClientIP             string
}

// CreateChargebackResult reports the opened dispute.
type CreateChargebackResult struct {
	Chargeback    ChargebackRow `json:"chargeback"`
	EvidenceCount int           `json:"evidence_count"`
	AccountFrozen bool          `json:"account_frozen"`
}

// Create validates, optionally freezes the account (dual control), then
// persists the dispute + auto-collected evidence bundle inside one
// SERIALIZABLE tx and mirrors the action into admin_audit_log.
func (s *ChargebackService) Create(ctx context.Context, adminID int64,
	req CreateChargebackRequest) (*CreateChargebackResult, error) {
	if req.AccountID <= 0 {
		return nil, errCode("INVALID_REQUEST", "account_id required")
	}
	ccy, err := normalizeCurrency(req.Currency)
	if err != nil {
		return nil, err
	}
	amount, err := parseMoney(req.Amount)
	if err != nil {
		return nil, err
	}
	if req.Reason == "" || len(req.Reason) > 2000 {
		return nil, errCode("INVALID_REQUEST", "reason is required (≤2000 chars)")
	}
	meta, err := s.store.AccountMeta(ctx, req.AccountID)
	if err != nil {
		return nil, err
	}
	if meta.Status == "CLOSED" {
		return nil, errf("INVALID_REQUEST", "account %d is CLOSED", req.AccountID)
	}
	var netw *string
	if req.CardNetwork != "" {
		if len(req.CardNetwork) > 32 {
			return nil, errCode("INVALID_REQUEST", "card_network too long")
		}
		v := req.CardNetwork
		netw = &v
	}

	// Verify the disputed funding row exists + belongs to the account —
	// a fat-fingered reference must not silently open a bad dispute.
	var fundingSnap []byte
	if req.FundingTransactionID != nil {
		snap, err := s.store.FundingTxSnapshot(ctx, *req.FundingTransactionID)
		if err != nil {
			return nil, errf("INVALID_REQUEST",
				"funding_transaction_id %d not found", *req.FundingTransactionID)
		}
		fundingSnap = snap
		var check struct {
			AccountID int64 `json:"account_id"`
		}
		if json.Unmarshal(snap, &check) == nil && check.AccountID != req.AccountID {
			return nil, errf("INVALID_REQUEST",
				"funding transaction %d belongs to account %d, not %d",
				*req.FundingTransactionID, check.AccountID, req.AccountID)
		}
	}

	frozen := false
	if req.FreezeAccount {
		if req.ApproverUserID == 0 || req.ApproverUserID == adminID {
			return nil, errCode("DUAL_CONTROL_REQUIRED",
				"freezing during a dispute requires a distinct approver_user_id")
		}
		if s.freezer == nil {
			return nil, errCode("SERVICE_DEGRADED", "account freeze service unavailable")
		}
		if err := s.freezer.Freeze(ctx, accounts.AdminActor{
			UserID:     adminID,
			Role:       accounts.RoleComplianceOfficer,
			ApproverID: req.ApproverUserID,
		}, req.AccountID, fmt.Sprintf("chargeback dispute (admin %d)", adminID), req.ClientIP); err != nil {
			return nil, err
		}
		frozen = true
	}

	cb := ChargebackRow{
		AccountID:            req.AccountID,
		FundingTransactionID: req.FundingTransactionID,
		CardNetwork:          netw,
		Currency:             ccy,
		Amount:               amount,
		Reason:               req.Reason,
		OpenedBy:             adminID,
	}

	tx, err := s.store.BeginTx(ctx)
	if err != nil {
		return nil, wrapCode("INTERNAL_ERROR", "chargeback tx", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	ins, err := s.store.InsertChargeback(ctx, tx, cb)
	if err != nil {
		return nil, err
	}
	cb = *ins

	// Automated evidence collection (DoD: trade records, communications,
	// timestamps). Every row carries a sha256 payload digest.
	evCount, err := s.collectEvidence(ctx, tx, &cb, fundingSnap, &adminID)
	if err != nil {
		return nil, err
	}
	if err := s.store.UpdateChargebackStatus(ctx, tx, cb.ID,
		"EVIDENCE_COLLECTED", nil, nil, nil); err != nil {
		return nil, err
	}
	cb.Status = "EVIDENCE_COLLECTED"
	if err := tx.Commit(ctx); err != nil {
		return nil, wrapCode("INTERNAL_ERROR", "chargeback commit", err)
	}

	after, _ := json.Marshal(map[string]any{
		"status": cb.Status, "evidence_rows": evCount, "account_frozen": frozen,
	})
	if err := s.store.AdminAudit(ctx, adminID, "chargeback.open", "chargeback",
		cb.ID, nil, after, req.ClientIP); err != nil {
		s.log("funding: chargeback audit for %d: %v", cb.ID, err)
	}

	return &CreateChargebackResult{
		Chargeback:    cb,
		EvidenceCount: evCount,
		AccountFrozen: frozen,
	}, nil
}

// collectEvidence gathers the automated bundle inside the dispute tx:
// the disputed funding row, the account's trades in the 90-day window
// before the dispute, the legal-hold history, plus a NOTE row carrying
// the collection timestamp.
func (s *ChargebackService) collectEvidence(ctx context.Context, tx pgx.Tx,
	cb *ChargebackRow, fundingSnap []byte, collectedBy *int64) (int, error) {
	n := 0
	put := func(kind string, payload []byte) error {
		if err := s.store.InsertEvidence(ctx, tx, EvidenceRow{
			ChargebackID: cb.ID, Kind: kind,
			Payload: payload, PayloadSHA: sha256Hex(payload),
			CollectedBy: collectedBy,
		}); err != nil {
			return err
		}
		n++
		return nil
	}
	if len(fundingSnap) > 0 {
		if err := put("FUNDING_TX", fundingSnap); err != nil {
			return 0, err
		}
	}
	since := cb.OpenedAt.Add(-90 * 24 * time.Hour)
	trades, err := s.store.TradesForEvidence(ctx, cb.AccountID, since, cb.OpenedAt, 100)
	if err != nil {
		return 0, err
	}
	for _, tr := range trades {
		if err := put("TRADE", tr.Payload); err != nil {
			return 0, err
		}
	}
	freezes, err := s.store.FreezeEventsForEvidence(ctx, cb.AccountID, 50)
	if err != nil {
		return 0, err
	}
	for _, fe := range freezes {
		if err := put("FREEZE_EVENT", fe.Payload); err != nil {
			return 0, err
		}
	}
	note, _ := json.Marshal(map[string]any{
		"note":          "automated evidence collection at dispute open",
		"chargeback_id": cb.ID,
		"account_id":    cb.AccountID,
		"collected_at":  s.clock().UTC().Format(time.RFC3339Nano),
		"window_from":   since.Format(time.RFC3339),
		"window_to":     cb.OpenedAt.Format(time.RFC3339),
		"trades":        len(trades),
		"freeze_events": len(freezes),
	})
	if err := put("NOTE", note); err != nil {
		return 0, err
	}
	return n, nil
}

// ---------------------------------------------------------------------------
// Lifecycle transitions
// ---------------------------------------------------------------------------

// ChargebackDetail is the GET {id} payload: dispute + evidence bundle.
type ChargebackDetail struct {
	Chargeback ChargebackRow `json:"chargeback"`
	Evidence   []EvidenceRow `json:"evidence"`
}

// Detail returns the dispute plus its evidence bundle.
func (s *ChargebackService) Detail(ctx context.Context, id int64) (*ChargebackDetail, error) {
	cb, err := s.store.Chargeback(ctx, id)
	if err != nil {
		return nil, err
	}
	ev, err := s.store.EvidenceFor(ctx, id)
	if err != nil {
		return nil, err
	}
	return &ChargebackDetail{Chargeback: *cb, Evidence: ev}, nil
}

// List pages the dispute journal for GET /api/v1/admin/chargebacks.
func (s *ChargebackService) List(ctx context.Context, f ChargebackFilter) ([]ChargebackRow, string, int64, error) {
	if f.Limit <= 0 || f.Limit > 500 {
		f.Limit = 50
	}
	rows, total, err := s.store.ListChargebacks(ctx, f)
	if err != nil {
		return nil, "", 0, err
	}
	var next string
	if len(rows) > f.Limit {
		last := rows[f.Limit-1]
		next = encodeCursor(last.OpenedAt, last.ID)
		rows = rows[:f.Limit]
	}
	return rows, next, total, nil
}

// Submit transitions EVIDENCE_COLLECTED → SUBMITTED. Requires a
// non-empty evidence bundle (fail closed — a bare dispute never reaches
// the processor).
func (s *ChargebackService) Submit(ctx context.Context, adminID int64, id int64, clientIP string) (*ChargebackRow, error) {
	tx, err := s.store.BeginTx(ctx)
	if err != nil {
		return nil, wrapCode("INTERNAL_ERROR", "submit tx", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	cb, err := s.store.ChargebackForUpdate(ctx, tx, id)
	if err != nil {
		return nil, err
	}
	if cb.Status != "EVIDENCE_COLLECTED" {
		return nil, errf("INVALID_LIFECYCLE_TRANSITION",
			"chargeback %d is %s — only EVIDENCE_COLLECTED disputes submit", id, cb.Status)
	}
	count, err := s.store.EvidenceCount(ctx, tx, id)
	if err != nil {
		return nil, err
	}
	if count == 0 {
		return nil, errf("INVALID_REQUEST",
			"chargeback %d has no evidence — collect evidence before submitting", id)
	}
	now := s.clock().UTC()
	if err := s.store.UpdateChargebackStatus(ctx, tx, id, "SUBMITTED", &now, nil, nil); err != nil {
		return nil, err
	}
	cb.Status = "SUBMITTED"
	cb.SubmittedAt = &now
	if err := tx.Commit(ctx); err != nil {
		return nil, wrapCode("INTERNAL_ERROR", "submit commit", err)
	}
	s.audit(ctx, adminID, "chargeback.submit", id, clientIP,
		map[string]any{"status": "SUBMITTED", "evidence_rows": count})
	return cb, nil
}

// Resolve transitions SUBMITTED → RESOLVED_WON | RESOLVED_LOST.
func (s *ChargebackService) Resolve(ctx context.Context, adminID int64, id int64,
	outcome, note, clientIP string) (*ChargebackRow, error) {
	var status string
	switch outcome {
	case "WON":
		status = "RESOLVED_WON"
	case "LOST":
		status = "RESOLVED_LOST"
	default:
		return nil, errCode("INVALID_REQUEST", "outcome must be WON or LOST")
	}

	tx, err := s.store.BeginTx(ctx)
	if err != nil {
		return nil, wrapCode("INTERNAL_ERROR", "resolve tx", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	cb, err := s.store.ChargebackForUpdate(ctx, tx, id)
	if err != nil {
		return nil, err
	}
	if cb.Status != "SUBMITTED" {
		return nil, errf("INVALID_LIFECYCLE_TRANSITION",
			"chargeback %d is %s — only SUBMITTED disputes resolve", id, cb.Status)
	}
	now := s.clock().UTC()
	var notePtr *string
	if note != "" {
		notePtr = &note
	}
	if err := s.store.UpdateChargebackStatus(ctx, tx, id, status, nil, &now, notePtr); err != nil {
		return nil, err
	}
	cb.Status = status
	cb.ResolvedAt = &now
	if err := tx.Commit(ctx); err != nil {
		return nil, wrapCode("INTERNAL_ERROR", "resolve commit", err)
	}
	s.audit(ctx, adminID, "chargeback.resolve", id, clientIP,
		map[string]any{"status": status, "note": note})
	return cb, nil
}

func (s *ChargebackService) audit(ctx context.Context, adminID int64, action string,
	id int64, ip string, after map[string]any) {
	payload, _ := json.Marshal(after)
	if err := s.store.AdminAudit(ctx, adminID, action, "chargeback", id, nil, payload, ip); err != nil {
		s.log("funding: %s audit for %d: %v", action, id, err)
	}
}

func (s *ChargebackService) log(format string, args ...any) {
	if s.logf != nil {
		s.logf(format, args...)
	}
}
