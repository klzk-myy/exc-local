// Phase-11 Task 11.3.7 — beneficiary bank-account registry surface.
//
// Client routes (account-scoped, authUser/authRead):
//
//	POST   /api/v1/funding/bank-accounts    — register (KYC T1+, ACTIVE)
//	GET    /api/v1/funding/bank-accounts    — list own beneficiaries
//	DELETE /api/v1/funding/bank-accounts?id= — remove own beneficiary
//
// Admin routes (Finance Ops+; verification is dual-controlled):
//
//	GET  /api/v1/admin/funding/bank-accounts            — ?status=&limit=
//	POST /api/v1/admin/funding/bank-accounts/{id}/verify {approver_id, method}
//	POST /api/v1/admin/funding/bank-accounts/{id}/reject {reason}
//
// There is deliberately NO client verify path — verification is
// bank-statement review / micro-deposit evidence judged by ops; a user
// cannot self-verify (task text).
package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"

	"exchange/internal/admin"
	"exchange/internal/funding"
	"exchange/internal/gateway"
	"exchange/internal/middleware"
)

// beneficiaryService is the narrow handler seam —
// *funding.BankAccountService satisfies it; tests substitute a fake.
type beneficiaryService interface {
	Register(ctx context.Context, accountID int64, in funding.BankAccountInput) (*funding.BankAccount, error)
	ListMine(ctx context.Context, accountID int64) ([]funding.BankAccount, error)
	DeleteMine(ctx context.Context, accountID, id int64) error
	AdminList(ctx context.Context, status string, limit int) ([]funding.BankAccount, error)
	Verify(ctx context.Context, adminID, approverID, id int64, method, ip string) (*funding.BankAccount, error)
	Reject(ctx context.Context, adminID, id int64, reason, ip string) (*funding.BankAccount, error)
}

// FundingBankAccountCreate — POST /api/v1/funding/bank-accounts.
func FundingBankAccountCreate(svc beneficiaryService) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		accountID, _, ok := claimsAccount(w, r)
		if !ok {
			return
		}
		var body funding.BankAccountInput
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			WriteError(w, "INVALID_REQUEST", "malformed JSON body",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		b, err := svc.Register(r.Context(), accountID, body)
		if err != nil {
			writeServiceErr(w, r, err)
			return
		}
		WriteJSON(w, http.StatusCreated, b)
	}
}

// FundingBankAccountList — GET /api/v1/funding/bank-accounts.
func FundingBankAccountList(svc beneficiaryService) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		accountID, _, ok := claimsAccount(w, r)
		if !ok {
			return
		}
		rows, err := svc.ListMine(r.Context(), accountID)
		if err != nil {
			writeServiceErr(w, r, err)
			return
		}
		WriteJSON(w, http.StatusOK, map[string]any{"bank_accounts": rows})
	}
}

// FundingBankAccountDelete — DELETE /api/v1/funding/bank-accounts?id=.
// Owner-scoped: the id must resolve under the caller's account.
func FundingBankAccountDelete(svc beneficiaryService) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		accountID, _, ok := claimsAccount(w, r)
		if !ok {
			return
		}
		id, err := strconv.ParseInt(r.URL.Query().Get("id"), 10, 64)
		if err != nil || id <= 0 {
			WriteError(w, "INVALID_REQUEST", "query param id is required",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		if err := svc.DeleteMine(r.Context(), accountID, id); err != nil {
			writeServiceErr(w, r, err)
			return
		}
		WriteJSON(w, http.StatusOK, map[string]any{"deleted": id})
	}
}

// ---------------------------------------------------------------------------
// Admin surface
// ---------------------------------------------------------------------------

// AdminBankAccountList — GET /api/v1/admin/funding/bank-accounts
// (?status=PENDING_VERIFICATION|VERIFIED|REJECTED&limit=).
func AdminBankAccountList(svc beneficiaryService) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		rows, err := svc.AdminList(r.Context(),
			r.URL.Query().Get("status"),
			parseLimitQuery(r.URL.Query().Get("limit"), 200, 500))
		if err != nil {
			writeServiceErr(w, r, err)
			return
		}
		WriteJSON(w, http.StatusOK, map[string]any{"bank_accounts": rows})
	}
}

// adminBankAccountID parses the {id} path segment; the admin routes
// live behind the RBAC middleware so the identity is already verified —
// handlers only re-derive the actor for audit/dual-control.
func adminBankAccountActor(w http.ResponseWriter, r *http.Request,
	trustProxy bool) (adminID int64, id int64, ip string, ok bool) {
	ident := admin.IdentityFrom(r.Context())
	if ident == nil || ident.UserID <= 0 {
		WriteError(w, "UNAUTHORIZED", "admin identity required",
			gateway.RequestIDFrom(r.Context()), nil)
		return 0, 0, "", false
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		WriteError(w, "INVALID_REQUEST", "invalid beneficiary id",
			gateway.RequestIDFrom(r.Context()), nil)
		return 0, 0, "", false
	}
	return ident.UserID, id, middleware.ClientIP(r, trustProxy), true
}

// AdminBankAccountVerify — POST /api/v1/admin/funding/bank-accounts/{id}/verify
// {approver_id, method?: BANK_STATEMENT|MICRO_DEPOSIT}. Dual control:
// the approver must be a distinct eligible admin (task step 2).
func AdminBankAccountVerify(svc beneficiaryService, trustProxy bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		adminID, id, ip, ok := adminBankAccountActor(w, r, trustProxy)
		if !ok {
			return
		}
		var body struct {
			ApproverID json.RawMessage `json:"approver_id"`
			Method     string          `json:"method"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			WriteError(w, "INVALID_REQUEST", "malformed JSON body",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		approver, err := rawInt64(body.ApproverID)
		if err != nil {
			WriteError(w, "INVALID_REQUEST", "approver_id must be an integer",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		row, err := svc.Verify(r.Context(), adminID, approver, id, body.Method, ip)
		if err != nil {
			writeServiceErr(w, r, err)
			return
		}
		WriteJSON(w, http.StatusOK, row)
	}
}

// AdminBankAccountReject — POST /api/v1/admin/funding/bank-accounts/{id}/reject
// {reason}. Single-approver: rejection releases no funds.
func AdminBankAccountReject(svc beneficiaryService, trustProxy bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		adminID, id, ip, ok := adminBankAccountActor(w, r, trustProxy)
		if !ok {
			return
		}
		var body struct {
			Reason string `json:"reason"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			WriteError(w, "INVALID_REQUEST", "malformed JSON body",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		row, err := svc.Reject(r.Context(), adminID, id, body.Reason, ip)
		if err != nil {
			writeServiceErr(w, r, err)
			return
		}
		WriteJSON(w, http.StatusOK, row)
	}
}
