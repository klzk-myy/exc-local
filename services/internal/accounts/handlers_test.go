package accounts

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"exchange/internal/errs"
	"exchange/pkg/decimal"
)

// staticIdentity resolves every request to the same caller.
func staticIdentity(id Identity) IdentityResolver {
	return func(*http.Request) *Identity { return &id }
}

func TestCountdownHandlerRejectsBadDuration(t *testing.T) {
	store := newFakeCountdownStore(time.Now)
	svc := NewDeadManService(store, &fakeDispatcher{})
	h := &Handler{DeadMan: svc,
		ResolveIdentity: staticIdentity(Identity{AccountID: 5, UserID: 50, Scopes: []string{ScopeTrade}})}

	req := httptest.NewRequest(http.MethodPost, "/api/v1/orders/countdown-cancel-all",
		strings.NewReader(`{"countdown_ms": 10}`))
	rec := httptest.NewRecorder()
	h.CountdownCancelAll(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body)
	}
	var env errorEnvelope
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if env.Error != CodeCountdownInvalid {
		t.Fatalf("code=%s, want COUNTDOWN_INVALID_DURATION", env.Error)
	}
}

func TestCountdownHandlerArmsAndReports(t *testing.T) {
	store := newFakeCountdownStore(time.Now)
	svc := NewDeadManService(store, &fakeDispatcher{})
	h := &Handler{DeadMan: svc,
		ResolveIdentity: staticIdentity(Identity{AccountID: 5, UserID: 50})}

	req := httptest.NewRequest(http.MethodPost, "/api/v1/orders/countdown-cancel-all",
		strings.NewReader(`{"countdown_ms": 30000}`))
	rec := httptest.NewRecorder()
	h.CountdownCancelAll(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body)
	}
	var ack CountdownAck
	if err := json.Unmarshal(rec.Body.Bytes(), &ack); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if ack.CountdownExpiry <= ack.ServerTime {
		t.Fatalf("expiry %d not after server_time %d", ack.CountdownExpiry, ack.ServerTime)
	}

	// countdown_ms=0 disables.
	req = httptest.NewRequest(http.MethodPost, "/api/v1/orders/countdown-cancel-all",
		strings.NewReader(`{"countdown_ms": 0}`))
	rec = httptest.NewRecorder()
	h.CountdownCancelAll(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("disable status=%d", rec.Code)
	}
}

func TestCountdownHandlerUnauthenticated(t *testing.T) {
	svc := NewDeadManService(newFakeCountdownStore(time.Now), &fakeDispatcher{})
	h := &Handler{DeadMan: svc} // no identity resolver → fail closed
	req := httptest.NewRequest(http.MethodPost, "/api/v1/orders/countdown-cancel-all",
		strings.NewReader(`{"countdown_ms": 30000}`))
	rec := httptest.NewRecorder()
	h.CountdownCancelAll(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status=%d, want 401", rec.Code)
	}
}

func TestCloseAllHandlerPartialFailureBody(t *testing.T) {
	disp := &fakeDispatcher{closeErrs: map[int64]error{101: errPlain("engine rejected")}}
	pos := &fakePositions{positions: []OpenPosition{
		{ID: 1, AccountID: 5, InstrumentID: 100, Symbol: "EURUSD", Side: "LONG",
			Quantity: decimal.NewFromInt(1), EntryPrice: decimal.NewFromInt(1)},
		{ID: 2, AccountID: 5, InstrumentID: 101, Symbol: "USDJPY", Side: "SHORT",
			Quantity: decimal.NewFromInt(1), EntryPrice: decimal.NewFromInt(1)},
	}}
	svc := NewCloseAllService(fakeGuard{}, pos, staticTOTP{testTOTPSecret}, testVerifier, disp)
	h := &Handler{CloseAll: svc,
		ResolveIdentity: staticIdentity(Identity{AccountID: 5, TwoFactorDone: true})}

	req := httptest.NewRequest(http.MethodPost, "/api/v1/positions/close-all",
		strings.NewReader(`{}`))
	rec := httptest.NewRecorder()
	h.CloseAllPositions(rec, req)
	if rec.Code != http.StatusConflict {
		t.Fatalf("status=%d body=%s, want 409", rec.Code, rec.Body)
	}
	var res CloseAllResult
	if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !res.PartialFailure || len(res.Closes) != 2 || !res.Closes[0].OK || res.Closes[1].OK {
		t.Fatalf("partial result wrong: %+v", res)
	}
}

func TestCloseAllHandlerRequires2FAHeader(t *testing.T) {
	pos := &fakePositions{}
	svc := NewCloseAllService(fakeGuard{}, pos, staticTOTP{testTOTPSecret}, testVerifier, &fakeDispatcher{})
	h := &Handler{CloseAll: svc,
		ResolveIdentity: staticIdentity(Identity{AccountID: 5})} // no AMR elevation

	req := httptest.NewRequest(http.MethodPost, "/api/v1/positions/close-all",
		strings.NewReader(`{}`))
	rec := httptest.NewRecorder()
	h.CloseAllPositions(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status=%d, want 403", rec.Code)
	}
	var env errorEnvelope
	_ = json.Unmarshal(rec.Body.Bytes(), &env)
	if env.Error != CodeTwoFactorRequired {
		t.Fatalf("code=%s, want TWO_FACTOR_REQUIRED", env.Error)
	}
}

func TestFreezeHandlerRequiresRoleAndApprover(t *testing.T) {
	// nil pool — the handler must reject before touching PG.
	svc := NewFreezeService(nil, func(context.Context, int64) (string, error) {
		return "Support Agent", nil
	})
	h := &Handler{Freeze: svc,
		ResolveIdentity: staticIdentity(Identity{UserID: 9001})}

	req := httptest.NewRequest(http.MethodPost, "/api/v1/admin/accounts/7/freeze",
		strings.NewReader(`{"reason":"hold","approver_id":9002}`))
	req.SetPathValue("id", "7")
	rec := httptest.NewRecorder()
	h.FreezeAccount(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status=%d, want 403 (role)", rec.Code)
	}
	var env errorEnvelope
	_ = json.Unmarshal(rec.Body.Bytes(), &env)
	if env.Error != CodeUnauthorizedRole {
		t.Fatalf("code=%s, want UNAUTHORIZED_ROLE", env.Error)
	}
}

// TestEmittedCodesRegistered pins every code this cluster can emit to the
// Task 5.3.21 registry — an unregistered emission must fail the startup
// gate, not ship silently.
func TestEmittedCodesRegistered(t *testing.T) {
	if err := errs.New().CheckRegistered(EmittedCodes()...); err != nil {
		t.Fatalf("unregistered emission: %v", err)
	}
}
