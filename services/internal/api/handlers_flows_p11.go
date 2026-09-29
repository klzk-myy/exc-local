// Phase-11 funding handlers — flows cluster (Tasks 11.3.2, 11.3.3,
// 11.3.6, 11.3.10). Kept in their own file per the phase's file-map
// contract; they extend — never replace — the Phase-05 funding surface
// in funding.go (which remains untouched).
//
//	POST /api/v1/withdrawals/{id}/confirm          2FA step-up + 15-min window (wraps Phase-05 confirm)
//	POST /api/v1/admin/withdrawals/{id}/approve    PENDING_REVIEW approve → dispatch (four-eyes)
//	POST /api/v1/admin/withdrawals/{id}/reject     PENDING_REVIEW reject → compensating release (four-eyes)
//	POST /api/v1/deposits                          client deposit intent (Idempotency-Key required)
//	POST /api/v1/admin/funding/deposits            statement/webhook detection ingest
//	POST /api/v1/admin/funding/deposits/{id}/confirm   second-source confirmation
//	POST /api/v1/admin/funding/deposits/{id}/review    PENDING_REVIEW approve/reject (four-eyes)
//	GET  /api/v1/funding/withdrawal-whitelist      whitelist mode + effective beneficiaries
//	POST /api/v1/funding/withdrawal-whitelist/enable   WHITELIST_ONLY (rate-limited re-enable)
//	POST /api/v1/funding/withdrawal-whitelist/disable  ALLOW_ALL + account-scoped 24h lock
//	GET  /api/v1/admin/funding/nostro              nostro balances + coverage
//	POST /api/v1/admin/funding/nostro/replenishments   reserve→nostro request (dual control)
//	POST /api/v1/admin/funding/nostro/replenishments/{id}/decide  approve→execute / reject
//	GET  /api/v1/admin/funding/ops-alerts          durable funding alert trail
package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"

	"exchange/internal/funding"
	"exchange/internal/gateway"
	"exchange/pkg/decimal"
)

// ---------------------------------------------------------------------------
// POST /api/v1/withdrawals — Phase-11 gated create (11.3.2)
// ---------------------------------------------------------------------------

// withdrawalCreator is the FlowService create seam (whitelist mode +
// account egress lock + beneficiary timelock + same-destination
// cooldown + unverified-destination hold before the Phase-05 create).
type withdrawalCreator interface {
	Create(ctx context.Context, req funding.CreateWithdrawalRequest) (*funding.WithdrawalResult, error)
}

// CreateWithdrawalFlow serves the canonical create route through the
// Phase-11 gate wrapper — the request/response contract is identical
// to CreateWithdrawal (funding.go); only the service seam narrows to
// create-only so the 2FA-gated confirm path can never be mounted here
// by accident.
func CreateWithdrawalFlow(svc withdrawalCreator) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		accountID, claims, ok := claimsAccount(w, r)
		if !ok {
			return
		}
		var body createWithdrawalBody
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			WriteError(w, "INVALID_REQUEST", "malformed JSON body",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		userID, _ := strconv.ParseInt(claims.Subject, 10, 64)
		res, err := svc.Create(r.Context(), funding.CreateWithdrawalRequest{
			AccountID:        accountID,
			UserID:           userID,
			Currency:         body.Currency,
			Amount:           body.Amount,
			ReferenceAccount: body.ReferenceAccount,
			BankMethod:       body.BankMethod,
			ConfirmMethod:    body.ConfirmMethod,
			IdempotencyKey:   r.Header.Get("Idempotency-Key"),
		})
		if err != nil {
			writeServiceErr(w, r, err)
			return
		}
		WriteJSON(w, http.StatusCreated, res)
	}
}

// ---------------------------------------------------------------------------
// POST /api/v1/withdrawals/{id}/confirm — TOTP step-up wrapper (11.3.2)
// ---------------------------------------------------------------------------

// stepUpConfirmer is the FlowService confirm seam.
type stepUpConfirmer interface {
	ConfirmStepUp(ctx context.Context, req funding.StepUpConfirmRequest) (*funding.WithdrawalResult, error)
}

type confirmWithdrawalStepUpBody struct {
	Token  string `json:"token"`
	Method string `json:"method,omitempty"`
}

// ConfirmWithdrawalStepUp serves the canonical confirm route: the
// X-2FA-Token header (or an AMR-elevated session) satisfies the §12.6
// step-up before the Phase-05 15-minute window/token checks run.
// Idempotent: re-confirming a progressed withdrawal replays the result.
func ConfirmWithdrawalStepUp(svc stepUpConfirmer) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		accountID, claims, ok := claimsAccount(w, r)
		if !ok {
			return
		}
		id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
		if err != nil || id <= 0 {
			WriteError(w, "INVALID_REQUEST", "withdrawal id must be a positive integer",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		var body confirmWithdrawalStepUpBody
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			WriteError(w, "INVALID_REQUEST", "malformed JSON body",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		userID, _ := strconv.ParseInt(claims.Subject, 10, 64)
		res, err := svc.ConfirmStepUp(r.Context(), funding.StepUpConfirmRequest{
			ConfirmWithdrawalRequest: funding.ConfirmWithdrawalRequest{
				WithdrawalID: id,
				AccountID:    accountID,
				UserID:       userID,
				Token:        body.Token,
				Method:       body.Method,
			},
			TOTPToken:        r.Header.Get("X-2FA-Token"),
			SessionTwoFactor: claims.TwoFactorVerified(),
		})
		if err != nil {
			writeServiceErr(w, r, err)
			return
		}
		WriteJSON(w, http.StatusOK, res)
	}
}

// ---------------------------------------------------------------------------
// POST /api/v1/admin/withdrawals/{id}/approve|reject — 11.3.2 review
// ---------------------------------------------------------------------------

// withdrawalReviewer is the FlowService admin-review seam.
type withdrawalReviewer interface {
	AdminReview(ctx context.Context, adminID, approverID, withdrawalID int64,
		approve bool, note string) (*funding.WithdrawalResult, error)
}

type withdrawalReviewBody struct {
	ApproverID int64  `json:"approver_id"`
	Note       string `json:"note,omitempty"`
}

// AdminWithdrawalReview returns the approve/reject handler — the
// actionable leg of the >$50K PENDING_REVIEW tier (route registry rows
// are DualControl; approver_id must differ from the acting admin).
func AdminWithdrawalReview(svc withdrawalReviewer, approve bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		actor, ok := adminActor(w, r)
		if !ok {
			return
		}
		id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
		if err != nil || id <= 0 {
			WriteError(w, "INVALID_REQUEST", "withdrawal id must be a positive integer",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		var body withdrawalReviewBody
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			WriteError(w, "INVALID_REQUEST", "malformed JSON body",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		res, err := svc.AdminReview(r.Context(), actor, body.ApproverID, id, approve, body.Note)
		if err != nil {
			writeServiceErr(w, r, err)
			return
		}
		WriteJSON(w, http.StatusOK, res)
	}
}

// ---------------------------------------------------------------------------
// POST /api/v1/deposits — client deposit intent (11.3.3)
// ---------------------------------------------------------------------------

// depositIntenter is the DepositService intent seam.
type depositIntenter interface {
	CreateIntent(ctx context.Context, req funding.CreateDepositIntentRequest) (*funding.DepositResult, error)
}

type createDepositBody struct {
	Currency   string `json:"currency"`
	Amount     string `json:"amount"`
	Reference  string `json:"reference,omitempty"`
	BankMethod string `json:"bank_method,omitempty"`
}

// CreateDepositIntent registers a client-declared inbound wire — the
// detection poller adopts it by reference. Account-scoped idempotency:
// Idempotency-Key is required; replays return the stored row.
func CreateDepositIntent(svc depositIntenter) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		accountID, claims, ok := claimsAccount(w, r)
		if !ok {
			return
		}
		var body createDepositBody
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			WriteError(w, "INVALID_REQUEST", "malformed JSON body",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		userID, _ := strconv.ParseInt(claims.Subject, 10, 64)
		res, err := svc.CreateIntent(r.Context(), funding.CreateDepositIntentRequest{
			AccountID:      accountID,
			UserID:         userID,
			Currency:       body.Currency,
			Amount:         body.Amount,
			Reference:      body.Reference,
			BankMethod:     body.BankMethod,
			IdempotencyKey: r.Header.Get("Idempotency-Key"),
		})
		if err != nil {
			writeServiceErr(w, r, err)
			return
		}
		status := http.StatusCreated
		if res.Replayed {
			status = http.StatusOK
		}
		WriteJSON(w, status, res)
	}
}

// ---------------------------------------------------------------------------
// POST /api/v1/admin/funding/deposits — detection ingest (11.3.3)
// ---------------------------------------------------------------------------

// depositIngester is the detection seam (statement poll / webhook).
type depositIngester interface {
	IngestDetected(ctx context.Context, req funding.IngestDepositRequest) (*funding.DepositResult, error)
}

type ingestDepositBody struct {
	AccountID         int64  `json:"account_id"`
	Currency          string `json:"currency"`
	Amount            string `json:"amount"`
	Reference         string `json:"reference"`
	BankMethod        string `json:"bank_method,omitempty"`
	OriginatorName    string `json:"originator_name,omitempty"`
	OriginatorAccount string `json:"originator_account,omitempty"`
	Source            string `json:"source"`
	IntentReference   string `json:"intent_reference,omitempty"`
}

// AdminDepositIngest persists one detected deposit: PENDING row +
// first source confirmation + locked hold.
func AdminDepositIngest(svc depositIngester) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		actor, ok := adminActor(w, r)
		if !ok {
			return
		}
		var body ingestDepositBody
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			WriteError(w, "INVALID_REQUEST", "malformed JSON body",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		res, err := svc.IngestDetected(r.Context(), funding.IngestDepositRequest{
			AccountID:         body.AccountID,
			Currency:          body.Currency,
			Amount:            body.Amount,
			Reference:         body.Reference,
			BankMethod:        body.BankMethod,
			OriginatorName:    body.OriginatorName,
			OriginatorAccount: body.OriginatorAccount,
			Source:            body.Source,
			IntentReference:   body.IntentReference,
			IdempotencyKey:    r.Header.Get("Idempotency-Key"),
			ReceivedBy:        actor,
		})
		if err != nil {
			writeServiceErr(w, r, err)
			return
		}
		WriteJSON(w, http.StatusCreated, res)
	}
}

// ---------------------------------------------------------------------------
// POST /api/v1/admin/funding/deposits/{id}/confirm — dual-source (11.3.3)
// ---------------------------------------------------------------------------

// depositConfirmer is the source-confirmation seam.
type depositConfirmer interface {
	Confirm(ctx context.Context, req funding.DepositConfirmRequest) (*funding.DepositResult, error)
}

type depositConfirmBody struct {
	Source        string `json:"source"`
	SenderName    string `json:"sender_name,omitempty"`
	SenderAccount string `json:"sender_account,omitempty"`
	PayloadSHA    string `json:"payload_sha256,omitempty"`
}

// AdminDepositConfirm records an independent bank-source confirmation;
// the second distinct source resolves the anti-fraud tier.
func AdminDepositConfirm(svc depositConfirmer) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		actor, ok := adminActor(w, r)
		if !ok {
			return
		}
		id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
		if err != nil || id <= 0 {
			WriteError(w, "INVALID_REQUEST", "deposit id must be a positive integer",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		var body depositConfirmBody
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			WriteError(w, "INVALID_REQUEST", "malformed JSON body",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		res, err := svc.Confirm(r.Context(), funding.DepositConfirmRequest{
			DepositID:     id,
			Source:        body.Source,
			SenderName:    body.SenderName,
			SenderAccount: body.SenderAccount,
			PayloadSHA:    body.PayloadSHA,
			ReceivedBy:    actor,
		})
		if err != nil {
			writeServiceErr(w, r, err)
			return
		}
		WriteJSON(w, http.StatusOK, res)
	}
}

// ---------------------------------------------------------------------------
// POST /api/v1/admin/funding/deposits/{id}/review — PENDING_REVIEW (11.3.3)
// ---------------------------------------------------------------------------

// depositReviewer is the deposit review seam.
type depositReviewer interface {
	AdminReview(ctx context.Context, adminID, approverID, depositID int64,
		approve bool, note string) (*funding.DepositResult, error)
}

type depositReviewBody struct {
	Action     string `json:"action"` // APPROVE | REJECT
	ApproverID int64  `json:"approver_id"`
	Note       string `json:"note,omitempty"`
}

// AdminDepositReview resolves a PENDING_REVIEW deposit (four-eyes).
func AdminDepositReview(svc depositReviewer) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		actor, ok := adminActor(w, r)
		if !ok {
			return
		}
		id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
		if err != nil || id <= 0 {
			WriteError(w, "INVALID_REQUEST", "deposit id must be a positive integer",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		var body depositReviewBody
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			WriteError(w, "INVALID_REQUEST", "malformed JSON body",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		var approve bool
		switch strings.ToUpper(strings.TrimSpace(body.Action)) {
		case "APPROVE":
			approve = true
		case "REJECT":
			approve = false
		default:
			WriteError(w, "INVALID_REQUEST", "action must be APPROVE or REJECT",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		res, err := svc.AdminReview(r.Context(), actor, body.ApproverID, id, approve, body.Note)
		if err != nil {
			writeServiceErr(w, r, err)
			return
		}
		WriteJSON(w, http.StatusOK, res)
	}
}

// ---------------------------------------------------------------------------
// Whitelist mode — GET /api/v1/funding/withdrawal-whitelist (+ enable/disable)
// (Task 11.3.10; destination registry itself lives under
// /funding/bank-accounts — Task 11.3.7).
// ---------------------------------------------------------------------------

// whitelistManager is the WhitelistService seam.
type whitelistManager interface {
	Get(ctx context.Context, accountID int64) (*funding.WhitelistView, error)
	Enable(ctx context.Context, accountID, userID int64) (*funding.WhitelistView, error)
	Disable(ctx context.Context, accountID, userID int64) (*funding.WhitelistView, error)
}

// GetWithdrawalWhitelist serves the effective whitelist state +
// VERIFIED beneficiary membership.
func GetWithdrawalWhitelist(svc whitelistManager) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		accountID, _, ok := claimsAccount(w, r)
		if !ok {
			return
		}
		v, err := svc.Get(r.Context(), accountID)
		if err != nil {
			writeServiceErr(w, r, err)
			return
		}
		WriteJSON(w, http.StatusOK, v)
	}
}

// SetWithdrawalWhitelist returns the enable/disable mode handler.
// Enabling is refused while the re-enable latch stands
// (WHITELIST_CHANGE_LOCKED); disabling latches the account-scoped 24h
// egress lock — both latched by the service.
func SetWithdrawalWhitelist(svc whitelistManager, enable bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		accountID, claims, ok := claimsAccount(w, r)
		if !ok {
			return
		}
		userID, _ := strconv.ParseInt(claims.Subject, 10, 64)
		var (
			v   *funding.WhitelistView
			err error
		)
		if enable {
			v, err = svc.Enable(r.Context(), accountID, userID)
		} else {
			v, err = svc.Disable(r.Context(), accountID, userID)
		}
		if err != nil {
			writeServiceErr(w, r, err)
			return
		}
		WriteJSON(w, http.StatusOK, v)
	}
}

// ---------------------------------------------------------------------------
// Nostro coverage + replenishment (Task 11.3.6)
// ---------------------------------------------------------------------------

// nostroViewer is the DispatchService admin-read seam.
type nostroViewer interface {
	Coverage(ctx context.Context) ([]funding.NostroCoverageRow, error)
	OpsAlerts(ctx context.Context, limit int) ([]funding.FundingOpsAlertRow, error)
	Replenishments(ctx context.Context, limit int) ([]funding.ReplenishmentRow, error)
	RequestReplenishment(ctx context.Context, adminID int64, currency string,
		amount decimal.Decimal, sourceID, targetID int64) (*funding.ReplenishmentRow, error)
	DecideReplenishment(ctx context.Context, adminID, requestID int64,
		approve bool, note string) (*funding.ReplenishmentRow, error)
}

// AdminNostroCoverage lists nostro balances per currency with the
// pending-withdrawal load and deficit flags.
func AdminNostroCoverage(svc nostroViewer) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if _, ok := adminActor(w, r); !ok {
			return
		}
		rows, err := svc.Coverage(r.Context())
		if err != nil {
			writeServiceErr(w, r, err)
			return
		}
		WriteJSON(w, http.StatusOK, map[string]any{"currencies": rows})
	}
}

// AdminFundingOpsAlerts lists the durable funding alert trail.
func AdminFundingOpsAlerts(svc nostroViewer) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if _, ok := adminActor(w, r); !ok {
			return
		}
		limit := parseLimitQuery(r.URL.Query().Get("limit"), 50, 200)
		rows, err := svc.OpsAlerts(r.Context(), limit)
		if err != nil {
			writeServiceErr(w, r, err)
			return
		}
		WriteJSON(w, http.StatusOK, map[string]any{"items": rows})
	}
}

type replenishmentCreateBody struct {
	Currency     string `json:"currency"`
	Amount       string `json:"amount"`
	SourceNostro int64  `json:"source_nostro_id"`
	TargetNostro int64  `json:"target_nostro_id"`
}

// AdminReplenishmentCreate opens a PENDING_APPROVAL reserve→nostro
// replenishment request.
func AdminReplenishmentCreate(svc nostroViewer) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		actor, ok := adminActor(w, r)
		if !ok {
			return
		}
		var body replenishmentCreateBody
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			WriteError(w, "INVALID_REQUEST", "malformed JSON body",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		amount, err := decimal.NewFromString(strings.TrimSpace(body.Amount))
		if err != nil || !amount.IsPositive() {
			WriteError(w, "INVALID_REQUEST", "amount must be a positive decimal",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		row, err := svc.RequestReplenishment(r.Context(), actor,
			strings.ToUpper(strings.TrimSpace(body.Currency)), amount,
			body.SourceNostro, body.TargetNostro)
		if err != nil {
			writeServiceErr(w, r, err)
			return
		}
		WriteJSON(w, http.StatusCreated, row)
	}
}

// AdminReplenishmentList returns recent requests.
func AdminReplenishmentList(svc nostroViewer) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if _, ok := adminActor(w, r); !ok {
			return
		}
		limit := parseLimitQuery(r.URL.Query().Get("limit"), 50, 200)
		rows, err := svc.Replenishments(r.Context(), limit)
		if err != nil {
			writeServiceErr(w, r, err)
			return
		}
		WriteJSON(w, http.StatusOK, map[string]any{"items": rows})
	}
}

type replenishmentDecideBody struct {
	Action string `json:"action"` // APPROVE | REJECT
	Note   string `json:"note,omitempty"`
}

// AdminReplenishmentDecide applies the four-eyes decision — APPROVE
// executes the two-sided balance movement inside the service tx.
func AdminReplenishmentDecide(svc nostroViewer) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		actor, ok := adminActor(w, r)
		if !ok {
			return
		}
		id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
		if err != nil || id <= 0 {
			WriteError(w, "INVALID_REQUEST", "replenishment id must be a positive integer",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		var body replenishmentDecideBody
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			WriteError(w, "INVALID_REQUEST", "malformed JSON body",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		var approve bool
		switch strings.ToUpper(strings.TrimSpace(body.Action)) {
		case "APPROVE":
			approve = true
		case "REJECT":
			approve = false
		default:
			WriteError(w, "INVALID_REQUEST", "action must be APPROVE or REJECT",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		row, err := svc.DecideReplenishment(r.Context(), actor, id, approve, body.Note)
		if err != nil {
			writeServiceErr(w, r, err)
			return
		}
		WriteJSON(w, http.StatusOK, row)
	}
}
