// Handler tests — Phase-05 Wave-2 Cluster B (Tasks 5.3.4/5.3.6/5.3.18/
// 5.3.23/5.3.45). httptest + fake service seams; money paths are covered
// by the funding package's integration tests.
package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"exchange/internal/auth"
	"exchange/internal/funding"
	"exchange/pkg/decimal"
	excerrors "exchange/pkg/errors"
)

func userCtx(r *http.Request, accountID int64) *http.Request {
	return r.WithContext(auth.WithClaims(r.Context(),
		auth.Claims{Subject: "100", AccountID: accountID, Scopes: []string{"read", "transfer"}}))
}

func adminCtxB(r *http.Request) *http.Request {
	return r.WithContext(auth.WithClaims(r.Context(),
		auth.Claims{Subject: "500", AccountID: 9, Scopes: []string{"admin"}}))
}

func decodeErr(t *testing.T, rec *httptest.ResponseRecorder) ErrorEnvelope {
	t.Helper()
	var env ErrorEnvelope
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("envelope: %v", err)
	}
	return env
}

// --- fakes --------------------------------------------------------------

type fakeBalanceSrc struct {
	rows []funding.BalanceRow
	err  error
}

func (f fakeBalanceSrc) BalancesFor(context.Context, int64) ([]funding.BalanceRow, error) {
	return f.rows, f.err
}

type fakeNostroSrc struct {
	accts []funding.NostroAccount
	err   error
}

func (f fakeNostroSrc) NostroAccounts(context.Context, string) ([]funding.NostroAccount, error) {
	return f.accts, f.err
}

type fakeWithdrawalSvc struct {
	createRes  *funding.WithdrawalResult
	createErr  error
	confirmRes *funding.WithdrawalResult
	confirmErr error
	lastCreate funding.CreateWithdrawalRequest
}

func (f *fakeWithdrawalSvc) Create(_ context.Context, req funding.CreateWithdrawalRequest) (*funding.WithdrawalResult, error) {
	f.lastCreate = req
	return f.createRes, f.createErr
}

func (f *fakeWithdrawalSvc) Confirm(_ context.Context, req funding.ConfirmWithdrawalRequest) (*funding.WithdrawalResult, error) {
	return f.confirmRes, f.confirmErr
}

type fakeTransferSvc struct {
	res *funding.TransferResult
	err error
}

func (f *fakeTransferSvc) Create(_ context.Context, req funding.CreateTransferRequest) (*funding.TransferResult, error) {
	return f.res, f.err
}

type fakeTransferHist struct {
	rows  []funding.TransferRow
	next  string
	total int64
	err   error
	gotF  funding.TransferFilter
}

func (f *fakeTransferHist) Transfers(_ context.Context, _ int64, fl funding.TransferFilter) ([]funding.TransferRow, string, int64, error) {
	f.gotF = fl
	return f.rows, f.next, f.total, f.err
}

type fakeFundingHist struct {
	rows  []funding.FundingTxRow
	next  string
	total int64
	err   error
}

func (f *fakeFundingHist) Funding(context.Context, int64, funding.FundingFilter) ([]funding.FundingTxRow, string, int64, error) {
	return f.rows, f.next, f.total, f.err
}

type fakeChargebackSvc struct {
	createRes *funding.CreateChargebackResult
	err       error
	lastReq   funding.CreateChargebackRequest
	lastAdmin int64
}

func (f *fakeChargebackSvc) Create(_ context.Context, adminID int64, req funding.CreateChargebackRequest) (*funding.CreateChargebackResult, error) {
	f.lastAdmin, f.lastReq = adminID, req
	return f.createRes, f.err
}
func (f *fakeChargebackSvc) List(context.Context, funding.ChargebackFilter) ([]funding.ChargebackRow, string, int64, error) {
	return nil, "", 0, nil
}
func (f *fakeChargebackSvc) Detail(context.Context, int64) (*funding.ChargebackDetail, error) {
	return &funding.ChargebackDetail{}, nil
}
func (f *fakeChargebackSvc) Submit(context.Context, int64, int64, string) (*funding.ChargebackRow, error) {
	return &funding.ChargebackRow{Status: "SUBMITTED"}, nil
}
func (f *fakeChargebackSvc) Resolve(context.Context, int64, int64, string, string, string) (*funding.ChargebackRow, error) {
	return &funding.ChargebackRow{Status: "RESOLVED_WON"}, nil
}

// --- tests ---------------------------------------------------------------

func TestClusterB_Unauthenticated(t *testing.T) {
	handlers := map[string]http.HandlerFunc{
		"balances":   AccountBalances(fakeBalanceSrc{}),
		"positions":  AccountPositions(fakeBalanceSrc2{}),
		"deposits":   DepositInstructions(fakeNostroSrc{}),
		"wd-create":  CreateWithdrawal(&fakeWithdrawalSvc{}),
		"wd-confirm": ConfirmWithdrawal(&fakeWithdrawalSvc{}),
		"funding":    FundingHistory(&fakeFundingHist{}),
		"transfer":   CreateTransfer(&fakeTransferSvc{}),
		"tx-history": TransferHistory(&fakeTransferHist{}),
		"cb-create":  AdminChargebackCreate(&fakeChargebackSvc{}),
	}
	for name, h := range handlers {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/x", strings.NewReader("{}"))
		h.ServeHTTP(rec, req)
		env := decodeErr(t, rec)
		if rec.Code != http.StatusUnauthorized || env.Error != "UNAUTHORIZED" {
			t.Fatalf("%s: got %d %s", name, rec.Code, env.Error)
		}
	}
}

type fakeBalanceSrc2 struct{}

func (fakeBalanceSrc2) PositionsFor(context.Context, int64) ([]funding.PositionRow, error) {
	return nil, nil
}

func TestAccountBalances_ForeignAccountRejected(t *testing.T) {
	rec := httptest.NewRecorder()
	req := userCtx(httptest.NewRequest(http.MethodGet,
		"/api/v1/account/balances?account_id=999", nil), 7)
	AccountBalances(fakeBalanceSrc{}).ServeHTTP(rec, req)
	env := decodeErr(t, rec)
	if rec.Code != http.StatusForbidden || env.Error != "FORBIDDEN" {
		t.Fatalf("got %d %s", rec.Code, env.Error)
	}
}

func TestAccountBalances_Happy(t *testing.T) {
	rec := httptest.NewRecorder()
	req := userCtx(httptest.NewRequest(http.MethodGet, "/api/v1/account/balances", nil), 7)
	src := fakeBalanceSrc{rows: []funding.BalanceRow{
		{Currency: "USD", Available: decimal.NewFromInt(100), Locked: decimal.NewFromInt(5),
			Total: decimal.NewFromInt(105)},
	}}
	AccountBalances(src).ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d body %s", rec.Code, rec.Body.String())
	}
	var body struct {
		AccountID int64 `json:"account_id"`
		Balances  []struct {
			Currency  string `json:"currency"`
			Available string `json:"available"`
		} `json:"balances"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.AccountID != 7 || len(body.Balances) != 1 || body.Balances[0].Available != "100" {
		t.Fatalf("body %+v", body)
	}
}

func TestDepositInstructions(t *testing.T) {
	h := DepositInstructions(fakeNostroSrc{accts: []funding.NostroAccount{
		{ID: 1, Currency: "USD", BankName: "Testbank", IBAN: "DE00"},
	}})
	// bad currency
	rec := httptest.NewRecorder()
	req := userCtx(httptest.NewRequest(http.MethodGet, "/api/v1/deposits/US", nil), 7)
	req.SetPathValue("currency", "US")
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("bad ccy: %d", rec.Code)
	}
	// happy
	rec = httptest.NewRecorder()
	req = userCtx(httptest.NewRequest(http.MethodGet, "/api/v1/deposits/USD", nil), 7)
	req.SetPathValue("currency", "usd")
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("happy: %d %s", rec.Code, rec.Body.String())
	}
	var body struct {
		Currency  string `json:"currency"`
		Reference string `json:"reference"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Currency != "USD" || !strings.HasPrefix(body.Reference, "EXC") {
		t.Fatalf("body %+v", body)
	}
	// no nostro → 404
	rec = httptest.NewRecorder()
	req = userCtx(httptest.NewRequest(http.MethodGet, "/api/v1/deposits/ZZZ", nil), 7)
	req.SetPathValue("currency", "ZZZ")
	DepositInstructions(fakeNostroSrc{}).ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("empty nostro: %d", rec.Code)
	}
}

func TestCreateWithdrawal_Handler(t *testing.T) {
	svc := &fakeWithdrawalSvc{createRes: &funding.WithdrawalResult{
		WithdrawalID: 42, Status: "PENDING", ConfirmToken: "tok",
	}}
	// malformed body
	rec := httptest.NewRecorder()
	req := userCtx(httptest.NewRequest(http.MethodPost, "/api/v1/withdrawals",
		strings.NewReader("{bad")), 7)
	CreateWithdrawal(svc).ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("malformed: %d", rec.Code)
	}
	// happy — idempotency key forwarded
	rec = httptest.NewRecorder()
	req = userCtx(httptest.NewRequest(http.MethodPost, "/api/v1/withdrawals",
		strings.NewReader(`{"currency":"usd","amount":"10","reference_account":"IBAN"}`)), 7)
	req.Header.Set("Idempotency-Key", "k-1")
	CreateWithdrawal(svc).ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", rec.Code, rec.Body.String())
	}
	if svc.lastCreate.IdempotencyKey != "k-1" || svc.lastCreate.AccountID != 7 {
		t.Fatalf("req %+v", svc.lastCreate)
	}
	// coded service error → mapped status (ACCOUNT_FROZEN → 403)
	svc.createErr = excerrors.New("ACCOUNT_FROZEN", "frozen")
	svc.createRes = nil
	rec = httptest.NewRecorder()
	req = userCtx(httptest.NewRequest(http.MethodPost, "/api/v1/withdrawals",
		strings.NewReader(`{"currency":"USD","amount":"10","reference_account":"I"}`)), 7)
	CreateWithdrawal(svc).ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("coded err: %d %s", rec.Code, rec.Body.String())
	}
}

func TestConfirmWithdrawal_BadID(t *testing.T) {
	rec := httptest.NewRecorder()
	req := userCtx(httptest.NewRequest(http.MethodPost, "/api/v1/withdrawals/x/confirm",
		strings.NewReader(`{"token":"t"}`)), 7)
	req.SetPathValue("id", "x")
	ConfirmWithdrawal(&fakeWithdrawalSvc{}).ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("bad id: %d", rec.Code)
	}
}

func TestTransferHistory_FilterMapping(t *testing.T) {
	svc := &fakeTransferHist{rows: []funding.TransferRow{{ID: 1}}, next: "", total: 1}
	rec := httptest.NewRecorder()
	req := userCtx(httptest.NewRequest(http.MethodGet,
		"/api/v1/transfers?direction=in&currency=eur&sub_account_id=7&limit=10", nil), 7)
	TransferHistory(svc).ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d %s", rec.Code, rec.Body.String())
	}
	if svc.gotF.Direction != "IN" || svc.gotF.Currency != "EUR" ||
		svc.gotF.PerspectiveID != 7 || svc.gotF.Limit != 10 {
		t.Fatalf("filter %+v", svc.gotF)
	}
	// malformed cursor → 400
	rec = httptest.NewRecorder()
	req = userCtx(httptest.NewRequest(http.MethodGet, "/api/v1/transfers?cursor=!!!", nil), 7)
	TransferHistory(svc).ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("bad cursor: %d", rec.Code)
	}
}

func TestAdminChargeback_ScopeGate(t *testing.T) {
	// non-admin claims → 403
	rec := httptest.NewRecorder()
	req := userCtx(httptest.NewRequest(http.MethodPost, "/api/v1/admin/chargebacks",
		strings.NewReader("{}")), 7)
	AdminChargebackCreate(&fakeChargebackSvc{}).ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("non-admin: %d", rec.Code)
	}
}

func TestAdminChargeback_CreateHappy(t *testing.T) {
	svc := &fakeChargebackSvc{createRes: &funding.CreateChargebackResult{
		Chargeback:    funding.ChargebackRow{ID: 5, Status: "EVIDENCE_COLLECTED"},
		AccountFrozen: true,
	}}
	rec := httptest.NewRecorder()
	req := adminCtxB(httptest.NewRequest(http.MethodPost, "/api/v1/admin/chargebacks",
		strings.NewReader(`{"account_id":1,"currency":"USD","amount":"10","reason":"r","freeze_account":true,"approver_user_id":600}`)))
	AdminChargebackCreate(svc).ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", rec.Code, rec.Body.String())
	}
	if svc.lastAdmin != 500 || !svc.lastReq.FreezeAccount || svc.lastReq.ApproverUserID != 600 {
		t.Fatalf("req %+v admin=%d", svc.lastReq, svc.lastAdmin)
	}
}

func TestClusterB_DecimalSerialization(t *testing.T) {
	// Money leaves the handler as decimal strings — regression guard
	// against float drift on the wire.
	rec := httptest.NewRecorder()
	req := userCtx(httptest.NewRequest(http.MethodGet, "/api/v1/account/balances", nil), 7)
	src := fakeBalanceSrc{rows: []funding.BalanceRow{
		{Currency: "EUR", Available: decimal.MustFromString("1234.56789012"),
			Locked: decimal.Zero, Total: decimal.MustFromString("1234.56789012")},
	}}
	AccountBalances(src).ServeHTTP(rec, req)
	if !strings.Contains(rec.Body.String(), `"1234.56789012"`) {
		t.Fatalf("decimal not serialized as string: %s", rec.Body.String())
	}
}
