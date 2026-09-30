// Phase-19 margin-surface handler tests — fake seams only, no PG/Redis.
package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/shopspring/decimal"

	"exchange/internal/auth"
	"exchange/internal/errs"
	"exchange/internal/funding"
	"exchange/internal/risk"
	excerrors "exchange/pkg/errors"
)

func init() {
	// Mirror the gateway's runtime registration (cmd/gateway main): the
	// spec §23 row lands with the Phase-19 spec-sync; until then the
	// registry must know the code or NewError degrades it to a 500.
	_ = errs.Default.Register(errs.CodeDef{
		Code:        risk.CodeMarginModeBlocked,
		HTTPStatus:  http.StatusConflict,
		Description: "Margin-mode switch rejected: open positions exist",
		Owner:       "Phase-19 Task 19.3.1",
	})
}

// ---------------------------------------------------------------------------
// fakes
// ---------------------------------------------------------------------------

type fakeMarginMode struct {
	lastAcct int64
	lastReq  string
	out      *risk.MarginAccount
	err      error
}

func (f *fakeMarginMode) SetMode(_ context.Context, acct int64, req string) (*risk.MarginAccount, error) {
	f.lastAcct, f.lastReq = acct, req
	return f.out, f.err
}

type fakeLeverageSvc struct {
	in  risk.SetLeverageInput
	out *risk.SetLeverageResult
	err error
}

func (f *fakeLeverageSvc) SetLeverage(_ context.Context, in risk.SetLeverageInput) (*risk.SetLeverageResult, error) {
	f.in = in
	return f.out, f.err
}

type fakeLeverageResolver struct {
	symbol string
	view   *risk.LeverageInstrumentView
	err    error
}

func (f *fakeLeverageResolver) InstrumentLeverageViewBySymbol(_ context.Context, symbol string) (*risk.LeverageInstrumentView, error) {
	f.symbol = symbol
	return f.view, f.err
}

type fakeLiqLister struct {
	filter LiquidationListFilter
	rows   []LiquidationEventView
	total  int64
	err    error
}

func (f *fakeLiqLister) AccountLiquidations(_ context.Context, fl LiquidationListFilter) ([]LiquidationEventView, int64, error) {
	f.filter = fl
	return f.rows, f.total, f.err
}

type fakeFundReader struct {
	balances []risk.FundBalance
	history  []risk.FundTxRow
	histCcy  string
	histCur  int64
	histLim  int
	err      error
}

func (f *fakeFundReader) Balances(context.Context) ([]risk.FundBalance, error) {
	return f.balances, f.err
}

func (f *fakeFundReader) History(_ context.Context, ccy string, cursor int64, limit int) ([]risk.FundTxRow, error) {
	f.histCcy, f.histCur, f.histLim = ccy, cursor, limit
	return f.history, f.err
}

type fakeCollateralUpdater struct {
	actor    int64
	rows     []risk.CollateralScheduleEntry
	schedule []risk.CollateralScheduleEntry
	err      error
}

func (f *fakeCollateralUpdater) Schedule(context.Context) ([]risk.CollateralScheduleEntry, error) {
	return f.schedule, nil
}

func (f *fakeCollateralUpdater) UpdateSchedule(_ context.Context, actor int64, rows []risk.CollateralScheduleEntry) error {
	f.actor, f.rows = actor, rows
	return f.err
}

type fakeADL struct {
	m   map[int64]int
	err error
}

func (f fakeADL) ADLIndicators(context.Context, int64) (map[int64]int, error) {
	return f.m, f.err
}

type fakeMarginLevel struct {
	lv  *risk.MarginLevel
	err error
}

func (f fakeMarginLevel) MarginLevel(context.Context, int64) (*risk.MarginLevel, error) {
	return f.lv, f.err
}

type fakeModeReader struct {
	mode string
	err  error
}

func (f fakeModeReader) ModeFor(context.Context, int64) (risk.MarginMode, error) {
	return risk.MarginMode(f.mode), f.err
}

func (f fakeModeReader) PositionMode(context.Context, int64) (string, error) {
	return f.mode, f.err
}

type fakeLeverageEff struct {
	eff int
	err error
}

func (f fakeLeverageEff) Effective(context.Context, int64, int64) (int, error) {
	return f.eff, f.err
}

type stubPosition struct {
	id           int64
	instrumentID int64
}

type stubPositionSource struct {
	rows []stubPosition
}

func (s stubPositionSource) PositionsFor(context.Context, int64) ([]funding.PositionRow, error) {
	out := make([]funding.PositionRow, 0, len(s.rows))
	for _, r := range s.rows {
		out = append(out, funding.PositionRow{
			ID:           r.id,
			InstrumentID: r.instrumentID,
			Symbol:       "EUR/USD",
			Side:         "LONG",
			Quantity:     decimal.NewFromInt(1000),
			EntryPrice:   decimal.NewFromFloat(1.10),
		})
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

func marginReq(t *testing.T, method, path, body string, claims *auth.Claims) *http.Request {
	t.Helper()
	var r *http.Request
	if body == "" {
		r = httptest.NewRequest(method, path, nil)
	} else {
		r = httptest.NewRequest(method, path, strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
	}
	if claims != nil {
		r = r.WithContext(auth.WithClaims(r.Context(), *claims))
	}
	return r
}

func acctClaims() *auth.Claims {
	return &auth.Claims{Subject: "777", AccountID: 42, Scopes: []string{"read", "trade"}}
}

// adminClaims (Subject "9001") is shared from handlers_ops_test.go.

func riskManagerResolver(_ context.Context, _ int64) (string, error) {
	return "Risk Manager", nil
}

func auditorResolver(_ context.Context, _ int64) (string, error) {
	return "Read-Only Auditor", nil
}

func serve(t *testing.T, h http.HandlerFunc, r *http.Request) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	h(rec, r)
	return rec
}

// ---------------------------------------------------------------------------
// POST /api/v1/account/margin-mode
// ---------------------------------------------------------------------------

func TestAccountMarginMode_OK(t *testing.T) {
	fake := &fakeMarginMode{out: &risk.MarginAccount{AccountID: 42, Mode: risk.ModeCross}}
	rec := serve(t, AccountMarginMode(fake),
		marginReq(t, "POST", "/api/v1/account/margin-mode", `{"mode":"CROSS"}`, acctClaims()))
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d body %s", rec.Code, rec.Body)
	}
	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp["margin_mode"] != "CROSS" || resp["account_id"] != float64(42) {
		t.Fatalf("unexpected body %v", resp)
	}
	if fake.lastAcct != 42 || fake.lastReq != "CROSS" {
		t.Fatalf("service args acct=%d req=%q", fake.lastAcct, fake.lastReq)
	}
}

func TestAccountMarginMode_Blocked409(t *testing.T) {
	fake := &fakeMarginMode{err: excerrors.New(risk.CodeMarginModeBlocked, "2 open position(s)")}
	rec := serve(t, AccountMarginMode(fake),
		marginReq(t, "POST", "/api/v1/account/margin-mode", `{"mode":"ISOLATED"}`, acctClaims()))
	if rec.Code != http.StatusConflict {
		t.Fatalf("want 409 got %d body %s", rec.Code, rec.Body)
	}
	var env ErrorEnvelope
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatal(err)
	}
	if env.Error != risk.CodeMarginModeBlocked {
		t.Fatalf("code %q", env.Error)
	}
}

func TestAccountMarginMode_NilSvc(t *testing.T) {
	rec := serve(t, AccountMarginMode(nil),
		marginReq(t, "POST", "/api/v1/account/margin-mode", `{"mode":"CROSS"}`, acctClaims()))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("want 503 got %d", rec.Code)
	}
}

func TestAccountMarginMode_Unauthenticated(t *testing.T) {
	rec := serve(t, AccountMarginMode(&fakeMarginMode{}),
		marginReq(t, "POST", "/api/v1/account/margin-mode", `{"mode":"CROSS"}`, nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("want 401 got %d", rec.Code)
	}
}

func TestAccountMarginMode_BadBody(t *testing.T) {
	rec := serve(t, AccountMarginMode(&fakeMarginMode{}),
		marginReq(t, "POST", "/api/v1/account/margin-mode", `{oops`, acctClaims()))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("want 400 got %d", rec.Code)
	}
}

// ---------------------------------------------------------------------------
// POST /api/v1/account/leverage
// ---------------------------------------------------------------------------

func TestAccountLeverageSet_SymbolOK(t *testing.T) {
	svc := &fakeLeverageSvc{out: &risk.SetLeverageResult{
		AccountID:    42,
		InstrumentID: ptrInt64(7),
		Requested:    20,
		Effective:    &risk.LeverageDecision{AccountID: 42, InstrumentID: 7, Effective: 20},
	}}
	res := &fakeLeverageResolver{view: &risk.LeverageInstrumentView{ID: 7, Symbol: "EUR/USD"}}
	rec := serve(t, AccountLeverageSet(svc, res),
		marginReq(t, "POST", "/api/v1/account/leverage", `{"symbol":"EUR/USD","leverage":20}`, acctClaims()))
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d body %s", rec.Code, rec.Body)
	}
	if svc.in.AccountID != 42 || svc.in.InstrumentID == nil || *svc.in.InstrumentID != 7 ||
		svc.in.Requested != 20 || svc.in.UserID != 777 {
		t.Fatalf("input %+v", svc.in)
	}
	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp["leverage"] != float64(20) || resp["effective_leverage"] != float64(20) ||
		resp["symbol"] != "EUR/USD" {
		t.Fatalf("unexpected body %v", resp)
	}
}

func TestAccountLeverageSet_AccountDefault(t *testing.T) {
	svc := &fakeLeverageSvc{out: &risk.SetLeverageResult{AccountID: 42, Requested: 30}}
	rec := serve(t, AccountLeverageSet(svc, nil),
		marginReq(t, "POST", "/api/v1/account/leverage", `{"leverage":30}`, acctClaims()))
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d body %s", rec.Code, rec.Body)
	}
	if svc.in.InstrumentID != nil {
		t.Fatalf("expected account default (nil instrument), got %+v", svc.in.InstrumentID)
	}
	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if _, ok := resp["effective_leverage"]; ok {
		t.Fatalf("default-row write must not fabricate a decision: %v", resp)
	}
}

func TestAccountLeverageSet_SymbolNotFound(t *testing.T) {
	res := &fakeLeverageResolver{view: nil}
	rec := serve(t, AccountLeverageSet(&fakeLeverageSvc{}, res),
		marginReq(t, "POST", "/api/v1/account/leverage", `{"symbol":"XXX/YYY","leverage":10}`, acctClaims()))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("want 404 got %d body %s", rec.Code, rec.Body)
	}
}

func TestAccountLeverageSet_AboveCap(t *testing.T) {
	svc := &fakeLeverageSvc{err: excerrors.New("INVALID_REQUEST", "leverage 100 exceeds resolved cap 30")}
	rec := serve(t, AccountLeverageSet(svc, nil),
		marginReq(t, "POST", "/api/v1/account/leverage", `{"leverage":100}`, acctClaims()))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("want 400 got %d", rec.Code)
	}
}

func TestAccountLeverageSet_BadLeverage(t *testing.T) {
	rec := serve(t, AccountLeverageSet(&fakeLeverageSvc{}, nil),
		marginReq(t, "POST", "/api/v1/account/leverage", `{"leverage":0}`, acctClaims()))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("want 400 got %d", rec.Code)
	}
}

func TestAccountLeverageSet_NilSvc(t *testing.T) {
	rec := serve(t, AccountLeverageSet(nil, nil),
		marginReq(t, "POST", "/api/v1/account/leverage", `{"leverage":10}`, acctClaims()))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("want 503 got %d", rec.Code)
	}
}

// ---------------------------------------------------------------------------
// GET /api/v1/account/liquidations
// ---------------------------------------------------------------------------

func TestAccountLiquidations_OK(t *testing.T) {
	now := time.Now().UTC()
	lister := &fakeLiqLister{
		rows: []LiquidationEventView{
			{ID: 11, PositionID: 5, InstrumentID: 7, Symbol: "EUR/USD",
				Kind: "DIRECT_CLOSE", Side: "LONG",
				Quantity:  decimal.RequireFromString("10000"),
				Price:     decimal.RequireFromString("1.09"),
				MarkPrice: decimal.RequireFromString("1.08"),
				CreatedAt: now},
		},
		total: 1,
	}
	rec := serve(t, AccountLiquidations(lister),
		marginReq(t, "GET",
			"/api/v1/account/liquidations?symbol=EUR/USD&from=2026-01-01T00:00:00Z&to=2026-12-31T00:00:00Z",
			"", acctClaims()))
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d body %s", rec.Code, rec.Body)
	}
	var env ListEnvelope
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatal(err)
	}
	if env.Total != 1 || env.Limit != 100 {
		t.Fatalf("envelope %+v", env)
	}
	f := lister.filter
	if f.AccountID != 42 || f.Symbol != "EUR/USD" || f.From == nil || f.To == nil || f.Limit != 100 {
		t.Fatalf("filter %+v", f)
	}
}

func TestAccountLiquidations_ForeignAccount403(t *testing.T) {
	rec := serve(t, AccountLiquidations(&fakeLiqLister{}),
		marginReq(t, "GET", "/api/v1/account/liquidations?account_id=99", "", acctClaims()))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("want 403 got %d", rec.Code)
	}
}

func TestAccountLiquidations_BadCursor400(t *testing.T) {
	rec := serve(t, AccountLiquidations(&fakeLiqLister{}),
		// "AAAA" is valid base64url but decodes without a ':' — the cursor
		// shape check must reject it, not restart the stream.
		marginReq(t, "GET", "/api/v1/account/liquidations?cursor=AAAA", "", acctClaims()))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("want 400 got %d", rec.Code)
	}
}

func TestAccountLiquidations_NilLister503(t *testing.T) {
	rec := serve(t, AccountLiquidations(nil),
		marginReq(t, "GET", "/api/v1/account/liquidations", "", acctClaims()))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("want 503 got %d", rec.Code)
	}
}

func TestAccountLiquidations_KeysetPassed(t *testing.T) {
	lister := &fakeLiqLister{rows: []LiquidationEventView{}}
	cursor := EncodeCursor(Cursor{CreatedAt: time.Unix(1700000000, 0).UTC(), ID: 55})
	rec := serve(t, AccountLiquidations(lister),
		marginReq(t, "GET", "/api/v1/account/liquidations?cursor="+cursor+"&limit=5", "", acctClaims()))
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d body %s", rec.Code, rec.Body)
	}
	if lister.filter.After == nil || lister.filter.After.ID != 55 || lister.filter.Limit != 5 {
		t.Fatalf("filter %+v", lister.filter)
	}
}

// ---------------------------------------------------------------------------
// GET /api/v1/admin/insurance-fund
// ---------------------------------------------------------------------------

func TestAdminInsuranceFund_OK(t *testing.T) {
	rdr := &fakeFundReader{
		balances: []risk.FundBalance{
			{Currency: "USD", Balance: decimal.RequireFromString("500000")},
		},
		history: []risk.FundTxRow{
			{ID: 3, Currency: "USD", Direction: "CREDIT",
				Amount: decimal.RequireFromString("10"), Reason: "LIQUIDATION_PENALTY"},
		},
	}
	rec := serve(t, AdminInsuranceFund(rdr, riskManagerResolver),
		marginReq(t, "GET", "/api/v1/admin/insurance-fund?currency=USD&limit=5", "", adminClaims()))
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d body %s", rec.Code, rec.Body)
	}
	if rdr.histCcy != "USD" || rdr.histLim != 5 {
		t.Fatalf("history args ccy=%q lim=%d", rdr.histCcy, rdr.histLim)
	}
	var resp struct {
		Balances     []risk.FundBalance `json:"balances"`
		Transactions struct {
			Data       []risk.FundTxRow `json:"data"`
			NextCursor string           `json:"next_cursor"`
			Limit      int              `json:"limit"`
		} `json:"transactions"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if len(resp.Balances) != 1 || resp.Balances[0].Currency != "USD" {
		t.Fatalf("balances %+v", resp.Balances)
	}
	if len(resp.Transactions.Data) != 1 || resp.Transactions.NextCursor != "" {
		t.Fatalf("tx %+v next=%q", resp.Transactions.Data, resp.Transactions.NextCursor)
	}
}

func TestAdminInsuranceFund_NextCursor(t *testing.T) {
	rdr := &fakeFundReader{history: []risk.FundTxRow{
		{ID: 9, Currency: "USD"}, {ID: 8, Currency: "USD"},
	}}
	rec := serve(t, AdminInsuranceFund(rdr, riskManagerResolver),
		marginReq(t, "GET", "/api/v1/admin/insurance-fund?limit=2", "", adminClaims()))
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d body %s", rec.Code, rec.Body)
	}
	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	tx := resp["transactions"].(map[string]any)
	if tx["next_cursor"] != "8" {
		t.Fatalf("next_cursor %v", tx["next_cursor"])
	}
}

func TestAdminInsuranceFund_RoleDenied(t *testing.T) {
	rec := serve(t, AdminInsuranceFund(&fakeFundReader{}, auditorResolver),
		marginReq(t, "GET", "/api/v1/admin/insurance-fund", "", adminClaims()))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("want 403 got %d", rec.Code)
	}
}

func TestAdminInsuranceFund_NilResolver(t *testing.T) {
	rec := serve(t, AdminInsuranceFund(&fakeFundReader{}, nil),
		marginReq(t, "GET", "/api/v1/admin/insurance-fund", "", adminClaims()))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("want 403 got %d", rec.Code)
	}
}

func TestAdminInsuranceFund_NilReader503(t *testing.T) {
	rec := serve(t, AdminInsuranceFund(nil, riskManagerResolver),
		marginReq(t, "GET", "/api/v1/admin/insurance-fund", "", adminClaims()))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("want 503 got %d", rec.Code)
	}
}

func TestAdminInsuranceFund_Unauthenticated(t *testing.T) {
	rec := serve(t, AdminInsuranceFund(&fakeFundReader{}, riskManagerResolver),
		marginReq(t, "GET", "/api/v1/admin/insurance-fund", "", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("want 401 got %d", rec.Code)
	}
}

// ---------------------------------------------------------------------------
// PUT /api/v1/admin/collateral-schedule
// ---------------------------------------------------------------------------

func TestAdminCollateralSchedulePut_OK(t *testing.T) {
	upd := &fakeCollateralUpdater{
		schedule: []risk.CollateralScheduleEntry{
			{Currency: "EUR", Eligible: true,
				HaircutPct:          decimal.RequireFromString("5"),
				MaxConcentrationPct: decimal.RequireFromString("40")},
		},
	}
	body := `{"rows":[{"currency":"EUR","eligible":true,"haircut_pct":"5","max_concentration_pct":"40"}]}`
	rec := serve(t, AdminCollateralSchedulePut(upd, riskManagerResolver),
		marginReq(t, "PUT", "/api/v1/admin/collateral-schedule", body, adminClaims()))
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d body %s", rec.Code, rec.Body)
	}
	if upd.actor != 9001 || len(upd.rows) != 1 || upd.rows[0].Currency != "EUR" ||
		!upd.rows[0].HaircutPct.Equal(decimal.RequireFromString("5")) {
		t.Fatalf("actor=%d rows=%+v", upd.actor, upd.rows)
	}
	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp["propagation_within_seconds"] != float64(5) {
		t.Fatalf("propagation marker missing: %v", resp)
	}
	if sched, ok := resp["schedule"].([]any); !ok || len(sched) != 1 {
		t.Fatalf("schedule %v", resp["schedule"])
	}
}

func TestAdminCollateralSchedulePut_RoleDenied(t *testing.T) {
	rec := serve(t, AdminCollateralSchedulePut(&fakeCollateralUpdater{}, auditorResolver),
		marginReq(t, "PUT", "/api/v1/admin/collateral-schedule", `{"rows":[]}`, adminClaims()))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("want 403 got %d", rec.Code)
	}
}

func TestAdminCollateralSchedulePut_NilUpdater503(t *testing.T) {
	rec := serve(t, AdminCollateralSchedulePut(nil, riskManagerResolver),
		marginReq(t, "PUT", "/api/v1/admin/collateral-schedule", `{"rows":[]}`, adminClaims()))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("want 503 got %d", rec.Code)
	}
}

func TestAdminCollateralSchedulePut_ServiceErr(t *testing.T) {
	upd := &fakeCollateralUpdater{err: excerrors.New("INVALID_REQUEST", "collateral schedule update: empty row set")}
	rec := serve(t, AdminCollateralSchedulePut(upd, riskManagerResolver),
		marginReq(t, "PUT", "/api/v1/admin/collateral-schedule", `{"rows":[]}`, adminClaims()))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("want 400 got %d", rec.Code)
	}
}

// ---------------------------------------------------------------------------
// GET /api/v1/account/positions decoration
// ---------------------------------------------------------------------------

func TestAccountPositionsEnriched_FullDecoration(t *testing.T) {
	src := stubPositionSource{rows: []stubPosition{{id: 5, instrumentID: 7}}}
	now := time.Now().UTC()
	dec := &PositionsDecoration{
		ADL:          fakeADL{m: map[int64]int{5: 3}},
		MarginLevel:  fakeMarginLevel{lv: &risk.MarginLevel{AccountID: 42, Equity: decimal.NewFromInt(1000), UsedMargin: decimal.NewFromInt(200), MarginLevelPct: decimal.NewFromInt(500), Status: "NORMAL", UpdatedAt: now}},
		MarginMode:   fakeModeReader{mode: "CROSS"},
		PositionMode: fakeModeReader{mode: "NETTING"},
		Leverage:     fakeLeverageEff{eff: 20},
	}
	rec := serve(t, AccountPositionsEnriched(src, dec),
		marginReq(t, "GET", "/api/v1/account/positions", "", acctClaims()))
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d body %s", rec.Code, rec.Body)
	}
	var resp struct {
		AccountID  int64          `json:"account_id"`
		MarginMode string         `json:"margin_mode"`
		PosMode    string         `json:"position_mode"`
		Margin     map[string]any `json:"margin"`
		Positions  []struct {
			PositionID        int64 `json:"position_id"`
			ADLIndicator      *int  `json:"adl_indicator"`
			EffectiveLeverage *int  `json:"effective_leverage"`
		} `json:"positions"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.MarginMode != "CROSS" || resp.PosMode != "NETTING" {
		t.Fatalf("modes %+v", resp)
	}
	if resp.Margin == nil || resp.Margin["margin_level_pct"] == nil {
		t.Fatalf("margin block %v", resp.Margin)
	}
	if len(resp.Positions) != 1 || resp.Positions[0].ADLIndicator == nil ||
		*resp.Positions[0].ADLIndicator != 3 || resp.Positions[0].EffectiveLeverage == nil ||
		*resp.Positions[0].EffectiveLeverage != 20 {
		t.Fatalf("positions %+v", resp.Positions)
	}
}

func TestAccountPositionsEnriched_NilDec(t *testing.T) {
	src := stubPositionSource{rows: []stubPosition{{id: 5, instrumentID: 7}}}
	rec := serve(t, AccountPositionsEnriched(src, nil),
		marginReq(t, "GET", "/api/v1/account/positions", "", acctClaims()))
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d body %s", rec.Code, rec.Body)
	}
	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"margin_mode", "position_mode", "margin"} {
		if _, ok := resp[k]; ok {
			t.Fatalf("key %q present without readers: %v", k, resp)
		}
	}
	pos := resp["positions"].([]any)
	row := pos[0].(map[string]any)
	if _, ok := row["adl_indicator"]; ok {
		t.Fatalf("adl_indicator present without reader: %v", row)
	}
}

func TestAccountPositionsEnriched_ReaderErrorsOmit(t *testing.T) {
	src := stubPositionSource{rows: []stubPosition{{id: 5, instrumentID: 7}}}
	dec := &PositionsDecoration{
		ADL:          fakeADL{err: excerrors.New("INTERNAL_ERROR", "redis down")},
		MarginLevel:  fakeMarginLevel{err: excerrors.New("INTERNAL_ERROR", "redis down")},
		MarginMode:   fakeModeReader{err: excerrors.New("INTERNAL_ERROR", "db down")},
		PositionMode: fakeModeReader{err: excerrors.New("INTERNAL_ERROR", "db down")},
		Leverage:     fakeLeverageEff{err: excerrors.New("INTERNAL_ERROR", "db down")},
	}
	rec := serve(t, AccountPositionsEnriched(src, dec),
		marginReq(t, "GET", "/api/v1/account/positions", "", acctClaims()))
	if rec.Code != http.StatusOK {
		t.Fatalf("decoration errors must not fail the read: %d %s", rec.Code, rec.Body)
	}
	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if _, ok := resp["margin_mode"]; ok {
		t.Fatalf("mode present on reader error: %v", resp)
	}
}

func ptrInt64(v int64) *int64 { return &v }
