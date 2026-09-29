// Handler tests — Phase-12 Task 12.3.6 (notification preferences).
package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"exchange/internal/auth"
	"exchange/internal/notifications"
	excerrors "exchange/pkg/errors"
)

// fakePrefStore satisfies prefStore without PostgreSQL.
type fakePrefStore struct {
	prefs map[int64]*notifications.Preferences
	err   error
}

func newFakePrefStore() *fakePrefStore {
	return &fakePrefStore{prefs: map[int64]*notifications.Preferences{}}
}

func (f *fakePrefStore) GetPreferences(_ context.Context, userID int64) (*notifications.Preferences, error) {
	if f.err != nil {
		return nil, f.err
	}
	if p, ok := f.prefs[userID]; ok {
		return p, nil
	}
	return nil, nil
}

func (f *fakePrefStore) PutPreferences(_ context.Context, p *notifications.Preferences) (*notifications.Preferences, error) {
	if f.err != nil {
		return nil, f.err
	}
	if err := p.Validate(); err != nil {
		return nil, err
	}
	p.UpdatedAt = time.Now().UTC()
	f.prefs[p.UserID] = p
	return p, nil
}

func prefReq(r *http.Request, userSub string) *http.Request {
	return r.WithContext(auth.WithClaims(r.Context(),
		auth.Claims{Subject: userSub, Scopes: []string{"read"}}))
}

func TestNotificationPreferencesGetDefaults(t *testing.T) {
	st := newFakePrefStore()
	rec := httptest.NewRecorder()
	req := userCtx(httptest.NewRequest(http.MethodGet,
		"/api/v1/account/notifications/preferences", nil), 0)
	NotificationPreferencesGet(st)(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body)
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body["user_id"].(float64) != 100 {
		t.Fatalf("user_id=%v want 100 (numeric sub)", body["user_id"])
	}
	// Event vocabulary: 11 user-facing tokens (trading_halt via Phase-14
	// Task 14.3.2; kyc_tier_downgraded via Task 14.3.4; trade_busted /
	// trade_price_adjusted via Phase-15 Task 15.3.5; copy_child_skipped
	// is internal-only and not user-preference-addressable).
	if len(body["events"].([]any)) != 11 {
		t.Fatalf("events=%v", body["events"])
	}
}

func TestNotificationPreferencesPutThenGet(t *testing.T) {
	st := newFakePrefStore()
	payload := `{"matrix":{"deposit_confirmed":{"email":false,"sms":true}},
		"quiet_hours":{"enabled":true,"start":"22:00","end":"07:00"}}`
	rec := httptest.NewRecorder()
	req := userCtx(httptest.NewRequest(http.MethodPut,
		"/api/v1/account/notifications/preferences", strings.NewReader(payload)), 0)
	NotificationPreferencesPut(st)(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("put status=%d body=%s", rec.Code, rec.Body)
	}
	got := st.prefs[100]
	if got == nil {
		t.Fatal("prefs not stored")
	}
	if got.Enabled("deposit_confirmed", "email") {
		t.Fatal("opt-out lost")
	}
	if !got.Enabled("deposit_confirmed", "sms") {
		t.Fatal("opt-in lost")
	}
	if !got.Quiet.Enabled || got.Quiet.Start != "22:00" {
		t.Fatalf("quiet hours lost: %+v", got.Quiet)
	}
}

func TestNotificationPreferencesPutValidation(t *testing.T) {
	st := newFakePrefStore()
	cases := []struct {
		name string
		body string
	}{
		{"malformed", `{`},
		{"unknown event", `{"matrix":{"nonsense":{"email":true}}}`},
		{"unknown channel", `{"matrix":{"order_filled":{"pigeon":true}}}`},
		{"bad quiet window", `{"quiet_hours":{"enabled":true,"start":"99:99","end":"07:00"}}`},
		{"quiet without enable", `{"quiet_hours":{"enabled":false,"start":"22:00","end":"07:00"}}`},
		{"empty window", `{"quiet_hours":{"enabled":true,"start":"08:00","end":"08:00"}}`},
	}
	for _, tc := range cases {
		rec := httptest.NewRecorder()
		req := userCtx(httptest.NewRequest(http.MethodPut,
			"/api/v1/account/notifications/preferences", strings.NewReader(tc.body)), 0)
		NotificationPreferencesPut(st)(rec, req)
		env := decodeErr(t, rec)
		if env.Error != "INVALID_REQUEST" {
			t.Fatalf("%s: code=%q status=%d want INVALID_REQUEST", tc.name, env.Error, rec.Code)
		}
	}
}

func TestNotificationPreferencesUnauthenticated(t *testing.T) {
	st := newFakePrefStore()
	// No claims at all → UNAUTHORIZED.
	rec := httptest.NewRecorder()
	NotificationPreferencesGet(st)(rec,
		httptest.NewRequest(http.MethodGet, "/x", nil))
	if decodeErr(t, rec).Error != "UNAUTHORIZED" {
		t.Fatalf("anonymous: %s", rec.Body)
	}
	// Non-numeric subject (oauth2 client grant) → UNAUTHORIZED.
	rec = httptest.NewRecorder()
	req := prefReq(httptest.NewRequest(http.MethodGet, "/x", nil), "oauth2:svc")
	NotificationPreferencesGet(st)(rec, req)
	if decodeErr(t, rec).Error != "UNAUTHORIZED" {
		t.Fatalf("non-user subject: %s", rec.Body)
	}
}

func TestNotificationPreferencesStoreDown(t *testing.T) {
	st := newFakePrefStore()
	st.err = excerrors.New("SERVICE_DEGRADED", "pg down")
	rec := httptest.NewRecorder()
	req := userCtx(httptest.NewRequest(http.MethodGet, "/x", nil), 0)
	NotificationPreferencesGet(st)(rec, req)
	if rec.Code == http.StatusOK {
		t.Fatal("store error surfaced as 200")
	}
}
