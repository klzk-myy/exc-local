// Request-context plumbing for authenticated claims. The auth middleware
// (Phase-05 Task 5.3.x authentication tasks) calls WithClaims after token
// verification; downstream handlers and edge middleware read via
// ClaimsFrom.
package auth

import "context"

type ctxKeyClaims struct{}

// WithClaims attaches validated claims to the request context.
func WithClaims(ctx context.Context, c Claims) context.Context {
	return context.WithValue(ctx, ctxKeyClaims{}, c)
}

// ClaimsFrom returns the claims carried by ctx, or nil when the request
// is unauthenticated (callers treat nil as 401, never as a default
// identity — fail-closed, spec §2.7).
func ClaimsFrom(ctx context.Context) *Claims {
	c, ok := ctx.Value(ctxKeyClaims{}).(Claims)
	if !ok {
		return nil
	}
	out := c
	return &out
}
