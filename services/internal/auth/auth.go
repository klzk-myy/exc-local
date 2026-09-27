// Package auth holds JWT/OAuth2 authentication for API requests.
// PHASE-05 owner: token verification, key management and middleware
// integration land with the order-gateway API (Tasks 5.3.x).
package auth

// Claims is the authenticated identity attached to a request.
// Field set is a scaffold minimum; it grows with the token format
// defined in Phase-05/12.
type Claims struct {
	Subject   string   // stable user identity (token "sub")
	AccountID int64    // trading account context, 0 = none selected
	Scopes    []string // granted scopes
}

// HasScope reports whether the claims grant the given scope.
func (c Claims) HasScope(scope string) bool {
	for _, s := range c.Scopes {
		if s == scope {
			return true
		}
	}
	return false
}
