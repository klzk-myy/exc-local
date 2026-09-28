package ws

import (
	"context"
	"errors"
	"time"
)

// Session is the per-connection authenticated state (spec §10.5 items
// 1–3). A connection starts anonymous (public market data) and elevates
// via the authenticate frame; refresh_token swaps the expiry without
// dropping subscriptions.
type Session struct {
	Authenticated bool
	ViaAPIKey     bool   // true when the "ak_" path authenticated the conn
	Subject       string // JWT sub, or "apikey:{key_id}"
	AccountID     int64
	Scopes        []string  // API-key scope matrix (§8.8 item 3)
	Tier          string    // §8.3 tier label for per-account conn caps
	KeyID         string    // JWT kid / API-key id (rotation forensics)
	SessionID     string    // JWT sid binding
	ExpiresAt     time.Time // zero for API-key sessions (no token expiry)
	ProtocolVer   int
	RemoteIP      string // peer address at auth time (order_audit forensics)
}

// hasScope checks the §8.8 scope matrix. A session carrying NO scopes at
// all is a full user JWT (password+2FA login) — scope enforcement is the
// API-key contract, so a scope-less session is treated as full-access.
// Sessions with an explicit scope list are strictly bounded by it.
func (s *Session) hasScope(scope string) bool {
	if len(s.Scopes) == 0 {
		return !s.ViaAPIKey // API-key sessions always carry a scope list
	}
	for _, v := range s.Scopes {
		if v == scope {
			return true
		}
	}
	return false
}

// dedupNamespace scopes the request_id dedup window per Task 5.3.42
// (account-scoped keys, global unscoped keys prohibited).
func (s *Session) dedupNamespace() string {
	if s.AccountID != 0 {
		return "a:" + int64str(s.AccountID)
	}
	return "u:" + s.Subject
}

var errBadParams = errors.New("ws: params must be a channel array or object")

// CountdownController is the seam into the Task 5.3.33 dead-man service.
// *accounts.DeadManService is adapted at command wiring (see adapters.go
// in cmd/gateway); tests substitute a fake.
type CountdownController interface {
	Set(ctx context.Context, accountID int64, countdownMs int64, renew bool) (serverTime, expiry int64, err error)
}

func int64str(v int64) string {
	const digits = "0123456789"
	if v == 0 {
		return "0"
	}
	neg := v < 0
	if neg {
		v = -v
	}
	var buf [20]byte
	pos := len(buf)
	for v > 0 {
		pos--
		buf[pos] = digits[v%10]
		v /= 10
	}
	if neg {
		pos--
		buf[pos] = '-'
	}
	return string(buf[pos:])
}
