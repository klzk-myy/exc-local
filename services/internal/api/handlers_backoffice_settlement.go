// handlers_backoffice_settlement.go — Phase-24 Backoffice & Settlement
// REST surface for Tasks 24.3.8 (CLS PvP), 24.3.9 (SSI + bilateral
// netting), 24.3.12 (bank statement ingestion), 24.3.20 (rail cut-off)
// and 24.3.21 (suspense routing). Routes registered by the composition
// root (routes_v1.go is sibling-owned):
//
//	POST /api/v1/admin/settlement/statements                 statement ingest (Finance Ops)
//	GET  /api/v1/admin/settlement/statements                 statement journal
//	GET  /api/v1/admin/settlement/statements/{id}/entries    normalized entries
//	POST /api/v1/admin/settlement/cls/instructions           submit paired CLS instruction
//	POST /api/v1/admin/settlement/cls/instructions/{ref}/dispatch
//	POST /api/v1/admin/settlement/cls/instructions/{ref}/amend
//	POST /api/v1/admin/settlement/cls/instructions/{ref}/rescind
//	POST /api/v1/admin/settlement/cls/instructions/{ref}/pay-in
//	POST /api/v1/admin/settlement/cls/instructions/{ref}/finality  authenticated member finality
//	POST /api/v1/admin/settlement/cls/instructions/{ref}/status    member status notification
//	GET  /api/v1/admin/settlement/ssi                        list SSIs (?account_id=)
//	POST /api/v1/admin/settlement/ssi                        register SSI (verified vs bank_accounts)
//	POST /api/v1/admin/settlement/ssi/{id}/revoke            revoke SSI
//	POST /api/v1/admin/settlement/netting/run                run netting cycle
//	GET  /api/v1/admin/settlement/netting/batches            list batches (?status=)
//	GET  /api/v1/admin/settlement/netting/batches/{id}/lines batch lines
//	POST /api/v1/admin/settlement/netting/batches/{id}/dispatch
//	POST /api/v1/admin/settlement/netting/batches/{id}/settle
//	POST /api/v1/admin/settlement/netting/batches/{id}/bust  release busted trades
//	GET  /api/v1/admin/settlement/rail-schedules             banking_rail_schedules view
//	POST /api/v1/admin/settlement/rail-schedules/evaluate    cut-off evaluation preview
//	POST /api/v1/admin/settlement/instructions/{id}/roll     manual roll past cut-off
//	POST /api/v1/admin/settlement/suspense/route             route unmatched credit (DepositGuard)
//	POST /api/v1/admin/settlement/suspense/{id}/resolve      four-eyes suspense resolution
package api

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"

	"exchange/internal/funding"
	"exchange/internal/gateway"
	"exchange/internal/settlement"
	"exchange/pkg/decimal"
)

// ---------------------------------------------------------------------------
// Funding → settlement adapter (import-cycle boundary — settlement cannot
// import funding; the api package composes both).
// ---------------------------------------------------------------------------

// depositGuardSettlementAdapter exposes *funding.DepositGuard through the
// settlement.DepositScreener seam (Task 24.3.21).
type depositGuardSettlementAdapter struct{ g *funding.DepositGuard }

// NewDepositGuardSettlementAdapter wires the adapter.
func NewDepositGuardSettlementAdapter(g *funding.DepositGuard) settlement.DepositScreener {
	return depositGuardSettlementAdapter{g: g}
}

func (a depositGuardSettlementAdapter) ScreenInbound(ctx context.Context, w settlement.SuspenseInbound) (*settlement.SuspenseScreenResult, error) {
	res, err := a.g.ScreenInbound(ctx, funding.InboundWire{
		BankTxID:          w.BankTxID,
		Rail:              w.Rail,
		Currency:          w.Currency,
		Amount:            w.Amount,
		OriginatorName:    w.OriginatorName,
		OriginatorAccount: w.OriginatorAccount,
		OriginatorBIC:     w.OriginatorBIC,
		Reference:         w.Reference,
		RemittanceInfo:    w.RemittanceInfo,
		ReceivedAt:        w.ReceivedAt,
	})
	if err != nil {
		return nil, err
	}
	out := &settlement.SuspenseScreenResult{
		Disposition: res.Disposition, Reason: res.Reason, Idempotent: res.Idempotent,
	}
	if res.Suspense != nil {
		out.SuspenseID = res.Suspense.ID
	}
	return out, nil
}

func (a depositGuardSettlementAdapter) ResolveSuspense(ctx context.Context, suspenseID int64,
	action string, investigatorID int64, notes string) (*settlement.SuspenseResolveOutcome, error) {
	out, err := a.g.ResolveSuspense(ctx, suspenseID, action, investigatorID, notes)
	if err != nil {
		return nil, err
	}
	return &settlement.SuspenseResolveOutcome{
		SuspenseID: out.SuspenseID, Action: out.Action,
		Status: out.Status, JournalID: out.JournalID,
	}, nil
}

// ---------------------------------------------------------------------------
// POST /api/v1/admin/settlement/statements — ingest (Task 24.3.12)
// ---------------------------------------------------------------------------

// statementIngester is the ingestion seam (*settlement.StatementIngestionService).
type statementIngester interface {
	Ingest(ctx context.Context, req settlement.IngestRequest) (*settlement.IngestResult, error)
}

type statementIngestRequest struct {
	NostroAccountID  int64  `json:"nostro_account_id"`
	Format           string `json:"format"`            // MT940|MT942|CAMT053
	ContentBase64    string `json:"content_base64"`    // raw file, base64
	Content          string `json:"content,omitempty"` // raw file, verbatim
	Source           string `json:"source,omitempty"`
	ExpectedChecksum string `json:"checksum,omitempty"` // sha256 hex
}

// AdminIngestStatement ingests one bank statement file — checksum-dedup,
// IBAN/BIC validation, sequence + balance-continuity checks, UETR → ref
// → amount matching and break routing in one transaction.
func AdminIngestStatement(svc statementIngester) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if _, ok := adminActor(w, r); !ok {
			return
		}
		var req statementIngestRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			WriteError(w, "INVALID_REQUEST", "malformed JSON body",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		var raw []byte
		var err error
		if req.ContentBase64 != "" {
			raw, err = base64.StdEncoding.DecodeString(req.ContentBase64)
			if err != nil {
				WriteError(w, "INVALID_REQUEST", "content_base64 is not valid base64",
					gateway.RequestIDFrom(r.Context()), nil)
				return
			}
		} else {
			raw = []byte(req.Content)
		}
		if len(raw) == 0 {
			WriteError(w, "INVALID_REQUEST", "statement content required",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		res, err := svc.Ingest(r.Context(), settlement.IngestRequest{
			NostroAccountID:  req.NostroAccountID,
			Format:           req.Format,
			Raw:              raw,
			Source:           req.Source,
			ExpectedChecksum: req.ExpectedChecksum,
		})
		if err != nil {
			writeServiceErr(w, r, err)
			return
		}
		WriteJSON(w, http.StatusOK, res)
	}
}

// ---------------------------------------------------------------------------
// GET /api/v1/admin/settlement/statements (+ /{id}/entries)
// ---------------------------------------------------------------------------

// statementReader is the read seam (PgxStatementStore).
type statementReader interface {
	ListStatements(ctx context.Context, nostroID int64, limit int) ([]settlement.BankStatement, error)
	ListEntries(ctx context.Context, statementID int64) ([]settlement.StatementEntryRow, error)
}

// AdminListStatements pages the statement journal.
func AdminListStatements(store statementReader) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if _, ok := adminActor(w, r); !ok {
			return
		}
		var nostroID int64
		if v := strings.TrimSpace(r.URL.Query().Get("nostro_account_id")); v != "" {
			var err error
			nostroID, err = strconv.ParseInt(v, 10, 64)
			if err != nil {
				WriteError(w, "INVALID_REQUEST", "bad nostro_account_id",
					gateway.RequestIDFrom(r.Context()), nil)
				return
			}
		}
		rows, err := store.ListStatements(r.Context(), nostroID,
			parseLimitQuery(r.URL.Query().Get("limit"), 100, 500))
		if err != nil {
			writeServiceErr(w, r, err)
			return
		}
		WriteJSON(w, http.StatusOK, map[string]any{"statements": rows})
	}
}

// AdminStatementEntries returns one statement's normalized entries.
func AdminStatementEntries(store statementReader) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if _, ok := adminActor(w, r); !ok {
			return
		}
		id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
		if err != nil || id <= 0 {
			WriteError(w, "INVALID_REQUEST", "bad statement id",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		rows, err := store.ListEntries(r.Context(), id)
		if err != nil {
			writeServiceErr(w, r, err)
			return
		}
		WriteJSON(w, http.StatusOK, map[string]any{"entries": rows})
	}
}

// ---------------------------------------------------------------------------
// CLS PvP (Task 24.3.8)
// ---------------------------------------------------------------------------

// clsLifecycle is the CLS seam (*settlement.ClsPvpService).
type clsLifecycle interface {
	SubmitInstruction(ctx context.Context, in settlement.ClsNewInstruction) (*settlement.ClsInstruction, error)
	Dispatch(ctx context.Context, instructionRef string) (*settlement.ClsInstruction, error)
	AmendInstruction(ctx context.Context, ref string, am settlement.ClsAmend) (*settlement.ClsInstruction, error)
	RescindInstruction(ctx context.Context, ref, reason string) (*settlement.ClsInstruction, error)
	ApplyMemberStatus(ctx context.Context, ref string, to settlement.ClsStatus, memberRef, detail string) (*settlement.ClsInstruction, error)
	RecordFinality(ctx context.Context, ref, memberRef string, authenticated bool) (*settlement.ClsInstruction, error)
	MarkPayIn(ctx context.Context, ref string) (*settlement.ClsInstruction, error)
	RouteIneligible(ctx context.Context, instructionID int64) (*settlement.ClsInstruction, error)
}

type clsSubmitRequest struct {
	InstructionRef        string `json:"instruction_ref,omitempty"`
	CounterpartyAccountID int64  `json:"counterparty_account_id"`
	MemberBIC             string `json:"member_bic"`
	Product               string `json:"product"`
	BuyCurrency           string `json:"buy_currency"`
	BuyAmount             string `json:"buy_amount"`
	SellCurrency          string `json:"sell_currency"`
	SellAmount            string `json:"sell_amount"`
	ValueDate             string `json:"value_date"` // YYYY-MM-DD
	TradeID               *int64 `json:"trade_id,omitempty"`
}

// AdminClsSubmit creates + validates a paired CLS instruction.
func AdminClsSubmit(svc clsLifecycle) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if _, ok := adminActor(w, r); !ok {
			return
		}
		var req clsSubmitRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			WriteError(w, "INVALID_REQUEST", "malformed JSON body",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		buy, err := decimal.NewFromString(strings.TrimSpace(req.BuyAmount))
		if err != nil || !buy.IsPositive() {
			WriteError(w, "INVALID_REQUEST", "buy_amount must be a positive decimal",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		sell, err := decimal.NewFromString(strings.TrimSpace(req.SellAmount))
		if err != nil || !sell.IsPositive() {
			WriteError(w, "INVALID_REQUEST", "sell_amount must be a positive decimal",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		vd, err := time.Parse("2006-01-02", strings.TrimSpace(req.ValueDate))
		if err != nil {
			WriteError(w, "INVALID_REQUEST", "value_date must be YYYY-MM-DD",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		out, err := svc.SubmitInstruction(r.Context(), settlement.ClsNewInstruction{
			InstructionRef:        req.InstructionRef,
			CounterpartyAccountID: req.CounterpartyAccountID,
			MemberBIC:             req.MemberBIC,
			Product:               req.Product,
			BuyCurrency:           req.BuyCurrency,
			BuyAmount:             buy,
			SellCurrency:          req.SellCurrency,
			SellAmount:            sell,
			ValueDate:             vd,
			TradeID:               req.TradeID,
		})
		if err != nil {
			writeServiceErr(w, r, err)
			return
		}
		WriteJSON(w, http.StatusCreated, out)
	}
}

// AdminClsDispatch submits a VALIDATED instruction to the CLS member.
func AdminClsDispatch(svc clsLifecycle) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if _, ok := adminActor(w, r); !ok {
			return
		}
		out, err := svc.Dispatch(r.Context(), r.PathValue("ref"))
		if err != nil {
			writeServiceErr(w, r, err)
			return
		}
		WriteJSON(w, http.StatusOK, out)
	}
}

type clsAmendRequest struct {
	BuyAmount  *string `json:"buy_amount,omitempty"`
	SellAmount *string `json:"sell_amount,omitempty"`
	ValueDate  *string `json:"value_date,omitempty"`
}

// AdminClsAmend amends a live instruction through the member's amend verb.
func AdminClsAmend(svc clsLifecycle) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if _, ok := adminActor(w, r); !ok {
			return
		}
		var req clsAmendRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			WriteError(w, "INVALID_REQUEST", "malformed JSON body",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		var am settlement.ClsAmend
		if req.BuyAmount != nil {
			d, err := decimal.NewFromString(strings.TrimSpace(*req.BuyAmount))
			if err != nil || !d.IsPositive() {
				WriteError(w, "INVALID_REQUEST", "buy_amount must be a positive decimal",
					gateway.RequestIDFrom(r.Context()), nil)
				return
			}
			am.BuyAmount = &d
		}
		if req.SellAmount != nil {
			d, err := decimal.NewFromString(strings.TrimSpace(*req.SellAmount))
			if err != nil || !d.IsPositive() {
				WriteError(w, "INVALID_REQUEST", "sell_amount must be a positive decimal",
					gateway.RequestIDFrom(r.Context()), nil)
				return
			}
			am.SellAmount = &d
		}
		if req.ValueDate != nil {
			vd, err := time.Parse("2006-01-02", strings.TrimSpace(*req.ValueDate))
			if err != nil {
				WriteError(w, "INVALID_REQUEST", "value_date must be YYYY-MM-DD",
					gateway.RequestIDFrom(r.Context()), nil)
				return
			}
			am.ValueDate = &vd
		}
		out, err := svc.AmendInstruction(r.Context(), r.PathValue("ref"), am)
		if err != nil {
			writeServiceErr(w, r, err)
			return
		}
		WriteJSON(w, http.StatusOK, out)
	}
}

type clsReasonRequest struct {
	Reason string `json:"reason"`
}

// AdminClsRescind rescinds a live instruction pre-finality.
func AdminClsRescind(svc clsLifecycle) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if _, ok := adminActor(w, r); !ok {
			return
		}
		var req clsReasonRequest
		_ = json.NewDecoder(r.Body).Decode(&req) // reason optional
		out, err := svc.RescindInstruction(r.Context(), r.PathValue("ref"), req.Reason)
		if err != nil {
			writeServiceErr(w, r, err)
			return
		}
		WriteJSON(w, http.StatusOK, out)
	}
}

// AdminClsPayIn records pay-in for an ELIGIBLE instruction.
func AdminClsPayIn(svc clsLifecycle) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if _, ok := adminActor(w, r); !ok {
			return
		}
		out, err := svc.MarkPayIn(r.Context(), r.PathValue("ref"))
		if err != nil {
			writeServiceErr(w, r, err)
			return
		}
		WriteJSON(w, http.StatusOK, out)
	}
}

type clsFinalityRequest struct {
	MemberRef     string `json:"member_ref"`
	Authenticated bool   `json:"authenticated"` // caller asserts member-authenticated notification
}

// AdminClsFinality posts GL/nostro finality from an authenticated member
// notification. Unauthenticated requests are refused — CLS finality in
// central-bank money is the only settlement trigger (spec §17.6 step 4).
func AdminClsFinality(svc clsLifecycle) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if _, ok := adminActor(w, r); !ok {
			return
		}
		var req clsFinalityRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			WriteError(w, "INVALID_REQUEST", "malformed JSON body",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		out, err := svc.RecordFinality(r.Context(), r.PathValue("ref"),
			req.MemberRef, req.Authenticated)
		if err != nil {
			writeServiceErr(w, r, err)
			return
		}
		WriteJSON(w, http.StatusOK, out)
	}
}

type clsStatusRequest struct {
	To        settlement.ClsStatus `json:"to"`
	MemberRef string               `json:"member_ref,omitempty"`
	Detail    string               `json:"detail,omitempty"`
}

// AdminClsStatus applies an inbound member status notification
// (unmatched/rejected transitions open breaks — spec §17.6 step 3).
// SETTLED is refused here — finality goes through /finality only.
func AdminClsStatus(svc clsLifecycle) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if _, ok := adminActor(w, r); !ok {
			return
		}
		var req clsStatusRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			WriteError(w, "INVALID_REQUEST", "malformed JSON body",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		out, err := svc.ApplyMemberStatus(r.Context(), r.PathValue("ref"),
			req.To, req.MemberRef, req.Detail)
		if err != nil {
			writeServiceErr(w, r, err)
			return
		}
		WriteJSON(w, http.StatusOK, out)
	}
}

// ---------------------------------------------------------------------------
// SSI (Task 24.3.9)
// ---------------------------------------------------------------------------

// ssiManager is the SSI seam (*settlement.SsiService).
type ssiManager interface {
	RegisterSSI(ctx context.Context, accountID, bankAccountID int64, currency,
		ref, bic string, isDefault bool) (*settlement.StandingSettlementInstruction, error)
	RevokeSSI(ctx context.Context, id int64) (*settlement.StandingSettlementInstruction, error)
	List(ctx context.Context, accountID int64) ([]settlement.StandingSettlementInstruction, error)
}

type ssiRegisterRequest struct {
	AccountID     int64  `json:"account_id"`
	BankAccountID int64  `json:"bank_account_id"`
	Currency      string `json:"currency"`
	Ref           string `json:"nostro_or_beneficiary_ref"`
	BIC           string `json:"bic,omitempty"`
	IsDefault     bool   `json:"is_default,omitempty"`
}

// AdminSsiRegister verifies the beneficiary against bank_accounts and
// registers the SSI (spec §17.7).
func AdminSsiRegister(svc ssiManager) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if _, ok := adminActor(w, r); !ok {
			return
		}
		var req ssiRegisterRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			WriteError(w, "INVALID_REQUEST", "malformed JSON body",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		out, err := svc.RegisterSSI(r.Context(), req.AccountID, req.BankAccountID,
			req.Currency, req.Ref, req.BIC, req.IsDefault)
		if err != nil {
			writeServiceErr(w, r, err)
			return
		}
		WriteJSON(w, http.StatusCreated, out)
	}
}

// AdminSsiList returns an account's SSIs.
func AdminSsiList(svc ssiManager) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if _, ok := adminActor(w, r); !ok {
			return
		}
		accountID, err := strconv.ParseInt(strings.TrimSpace(r.URL.Query().Get("account_id")), 10, 64)
		if err != nil || accountID <= 0 {
			WriteError(w, "INVALID_REQUEST", "account_id query parameter required",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		rows, err := svc.List(r.Context(), accountID)
		if err != nil {
			writeServiceErr(w, r, err)
			return
		}
		WriteJSON(w, http.StatusOK, map[string]any{"ssis": rows})
	}
}

// AdminSsiRevoke revokes one SSI.
func AdminSsiRevoke(svc ssiManager) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if _, ok := adminActor(w, r); !ok {
			return
		}
		id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
		if err != nil || id <= 0 {
			WriteError(w, "INVALID_REQUEST", "bad ssi id",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		out, err := svc.RevokeSSI(r.Context(), id)
		if err != nil {
			writeServiceErr(w, r, err)
			return
		}
		WriteJSON(w, http.StatusOK, out)
	}
}

// ---------------------------------------------------------------------------
// Bilateral netting (Task 24.3.9)
// ---------------------------------------------------------------------------

// nettingRunner is the netting seam (*settlement.NettingService).
type nettingRunner interface {
	RunNetting(ctx context.Context, scope settlement.NettingScope) (*settlement.NettingReport, error)
	DispatchBatch(ctx context.Context, batchID int64, rail string) (*settlement.NettingBatch, error)
	SettleBatch(ctx context.Context, batchID int64, confirmationRef string) (*settlement.NettingBatch, error)
	ReopenBatch(ctx context.Context, batchID int64, instructionIDs []int64, reason string) (*settlement.NettingBatch, error)
	ListBatches(ctx context.Context, status string, limit int) ([]settlement.NettingBatch, error)
	BatchLines(ctx context.Context, batchID int64) ([]settlement.NettingLine, error)
}

type nettingRunRequest struct {
	CounterpartyAccountID int64  `json:"counterparty_account_id"`
	Currency              string `json:"currency"`
	ValueDate             string `json:"value_date"` // YYYY-MM-DD
}

// AdminNettingRun executes one netting cycle for the scoped
// (counterparty, currency, value date) — CLS-eligible obligations are
// excluded by the service (spec §17.7).
func AdminNettingRun(svc nettingRunner) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if _, ok := adminActor(w, r); !ok {
			return
		}
		var req nettingRunRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			WriteError(w, "INVALID_REQUEST", "malformed JSON body",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		vd, err := time.Parse("2006-01-02", strings.TrimSpace(req.ValueDate))
		if err != nil {
			WriteError(w, "INVALID_REQUEST", "value_date must be YYYY-MM-DD",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		out, err := svc.RunNetting(r.Context(), settlement.NettingScope{
			CounterpartyAccountID: req.CounterpartyAccountID,
			Currency:              req.Currency,
			ValueDate:             vd,
		})
		if err != nil {
			writeServiceErr(w, r, err)
			return
		}
		WriteJSON(w, http.StatusOK, out)
	}
}

// AdminNettingBatches lists batches (?status=).
func AdminNettingBatches(svc nettingRunner) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if _, ok := adminActor(w, r); !ok {
			return
		}
		rows, err := svc.ListBatches(r.Context(),
			strings.ToUpper(strings.TrimSpace(r.URL.Query().Get("status"))),
			parseLimitQuery(r.URL.Query().Get("limit"), 100, 500))
		if err != nil {
			writeServiceErr(w, r, err)
			return
		}
		WriteJSON(w, http.StatusOK, map[string]any{"batches": rows})
	}
}

// AdminNettingBatchLines returns one batch's constituent lines.
func AdminNettingBatchLines(svc nettingRunner) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if _, ok := adminActor(w, r); !ok {
			return
		}
		id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
		if err != nil || id <= 0 {
			WriteError(w, "INVALID_REQUEST", "bad batch id",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		rows, err := svc.BatchLines(r.Context(), id)
		if err != nil {
			writeServiceErr(w, r, err)
			return
		}
		WriteJSON(w, http.StatusOK, map[string]any{"lines": rows})
	}
}

type nettingDispatchRequest struct {
	Rail string `json:"rail"`
}

// AdminNettingDispatch releases a NETTED batch to its rail.
func AdminNettingDispatch(svc nettingRunner) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if _, ok := adminActor(w, r); !ok {
			return
		}
		id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
		if err != nil || id <= 0 {
			WriteError(w, "INVALID_REQUEST", "bad batch id",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		var req nettingDispatchRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		out, err := svc.DispatchBatch(r.Context(), id, req.Rail)
		if err != nil {
			writeServiceErr(w, r, err)
			return
		}
		WriteJSON(w, http.StatusOK, out)
	}
}

type nettingSettleRequest struct {
	ConfirmationRef string `json:"confirmation_ref"`
}

// AdminNettingSettle confirms the net payment settled.
func AdminNettingSettle(svc nettingRunner) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if _, ok := adminActor(w, r); !ok {
			return
		}
		id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
		if err != nil || id <= 0 {
			WriteError(w, "INVALID_REQUEST", "bad batch id",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		var req nettingSettleRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		out, err := svc.SettleBatch(r.Context(), id, req.ConfirmationRef)
		if err != nil {
			writeServiceErr(w, r, err)
			return
		}
		WriteJSON(w, http.StatusOK, out)
	}
}

type nettingBustRequest struct {
	InstructionIDs []int64 `json:"instruction_ids"`
	Reason         string  `json:"reason"`
}

// AdminNettingBust releases busted trade legs from a batch (re-opens the
// aggregate and recomputes the net — spec §17.7 edge case).
func AdminNettingBust(svc nettingRunner) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if _, ok := adminActor(w, r); !ok {
			return
		}
		id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
		if err != nil || id <= 0 {
			WriteError(w, "INVALID_REQUEST", "bad batch id",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		var req nettingBustRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			WriteError(w, "INVALID_REQUEST", "malformed JSON body",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		out, err := svc.ReopenBatch(r.Context(), id, req.InstructionIDs, req.Reason)
		if err != nil {
			writeServiceErr(w, r, err)
			return
		}
		WriteJSON(w, http.StatusOK, out)
	}
}

// ---------------------------------------------------------------------------
// Rail cut-off (Task 24.3.20)
// ---------------------------------------------------------------------------

// railCutoffSource is the schedule seam (*settlement.RailCutoffService).
type railCutoffSource interface {
	Schedules() []settlement.RailSchedule
	Evaluate(rail, currency string, at time.Time) (settlement.CutoffDecision, error)
}

// AdminRailSchedules returns the DB-driven cut-off schedule snapshot.
func AdminRailSchedules(svc railCutoffSource) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if _, ok := adminActor(w, r); !ok {
			return
		}
		WriteJSON(w, http.StatusOK, map[string]any{"schedules": svc.Schedules()})
	}
}

type railEvalRequest struct {
	Rail     string `json:"rail"`
	Currency string `json:"currency"`
	At       string `json:"at,omitempty"` // RFC3339; empty = now
}

// AdminRailEvaluate previews a cut-off decision (which window applies +
// rolled value date) without touching any instruction.
func AdminRailEvaluate(svc railCutoffSource) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if _, ok := adminActor(w, r); !ok {
			return
		}
		var req railEvalRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			WriteError(w, "INVALID_REQUEST", "malformed JSON body",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		var at time.Time
		if req.At != "" {
			var err error
			at, err = time.Parse(time.RFC3339, req.At)
			if err != nil {
				WriteError(w, "INVALID_REQUEST", "at must be RFC3339",
					gateway.RequestIDFrom(r.Context()), nil)
				return
			}
		}
		out, err := svc.Evaluate(req.Rail, req.Currency, at)
		if err != nil {
			writeServiceErr(w, r, err)
			return
		}
		WriteJSON(w, http.StatusOK, out)
	}
}

// instructionRoller is the manual-roll seam (*settlement.SettlementService).
type instructionRoller interface {
	RollPastCutoff(ctx context.Context, instructionID int64, at time.Time) (*settlement.SettlementInstruction, error)
}

// AdminRollInstruction manually rolls a PENDING instruction past its
// rail cut-off to the next business day (QUEUED_FOR_NEXT_CYCLE).
func AdminRollInstruction(svc instructionRoller) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if _, ok := adminActor(w, r); !ok {
			return
		}
		id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
		if err != nil || id <= 0 {
			WriteError(w, "INVALID_REQUEST", "bad instruction id",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		out, err := svc.RollPastCutoff(r.Context(), id, time.Time{})
		if err != nil {
			writeServiceErr(w, r, err)
			return
		}
		WriteJSON(w, http.StatusOK, out)
	}
}

// ---------------------------------------------------------------------------
// Suspense routing (Task 24.3.21)
// ---------------------------------------------------------------------------

// suspenseRouter is the suspense seam (*settlement.SuspenseService over
// the DepositGuard adapter).
type suspenseRouter interface {
	RouteCredit(ctx context.Context, w settlement.SuspenseInbound) (*settlement.SuspenseScreenResult, error)
	Resolve(ctx context.Context, suspenseID int64, action string,
		investigatorID int64, notes string) (*settlement.SuspenseResolveOutcome, error)
}

type suspenseRouteRequest struct {
	BankTxID          string `json:"bank_tx_id"`
	Rail              string `json:"rail,omitempty"`
	Currency          string `json:"currency"`
	Amount            string `json:"amount"`
	OriginatorName    string `json:"originator_name"`
	OriginatorAccount string `json:"originator_account"`
	Reference         string `json:"reference,omitempty"`
	RemittanceInfo    string `json:"remittance_info,omitempty"`
}

// AdminSuspenseRoute manually routes an unmatched credit into the
// suspense pipeline — same DepositGuard engine as the automated
// statement-ingest path (Dr 1010 → Cr 2150, quarantine record, alert).
func AdminSuspenseRoute(svc suspenseRouter) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if _, ok := adminActor(w, r); !ok {
			return
		}
		var req suspenseRouteRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			WriteError(w, "INVALID_REQUEST", "malformed JSON body",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		amount, err := decimal.NewFromString(strings.TrimSpace(req.Amount))
		if err != nil || !amount.IsPositive() {
			WriteError(w, "INVALID_REQUEST", "amount must be a positive decimal",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		res, err := svc.RouteCredit(r.Context(), settlement.SuspenseInbound{
			BankTxID:          req.BankTxID,
			Rail:              req.Rail,
			Currency:          req.Currency,
			Amount:            amount,
			OriginatorName:    req.OriginatorName,
			OriginatorAccount: req.OriginatorAccount,
			Reference:         req.Reference,
			RemittanceInfo:    req.RemittanceInfo,
			ReceivedAt:        time.Now().UTC(),
		})
		if err != nil {
			writeServiceErr(w, r, err)
			return
		}
		WriteJSON(w, http.StatusOK, res)
	}
}

type suspenseResolveRequest struct {
	Action string `json:"action"` // RELEASE_TO_CLIENT | RETURN_TO_SOURCE
	Notes  string `json:"notes,omitempty"`
}

// AdminSuspenseResolve applies a four-eyes quarantine resolution through
// the guard (the actor id goes in as investigator; the dual-control
// approver check happens at the route/RBAC layer per Task 11.3.8).
func AdminSuspenseResolve(svc suspenseRouter) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		actor, ok := adminActor(w, r)
		if !ok {
			return
		}
		id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
		if err != nil || id <= 0 {
			WriteError(w, "INVALID_REQUEST", "bad suspense id",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		var req suspenseResolveRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			WriteError(w, "INVALID_REQUEST", "malformed JSON body",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		out, err := svc.Resolve(r.Context(), id, req.Action, actor, req.Notes)
		if err != nil {
			writeServiceErr(w, r, err)
			return
		}
		WriteJSON(w, http.StatusOK, out)
	}
}
