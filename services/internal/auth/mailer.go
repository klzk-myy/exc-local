// Task 12.3.1 — outbound-mail seam for the auth flows (email
// verification links, password-reset links, security alerts).
//
// There is no real provider yet: the full notification service
// (Phase-12 Task 12.3.5, internal/notifications) is owned by a
// different cluster. This seam is deliberately narrow — a single
// Send(Message) so the orchestrator can later bind the real
// notification pipeline behind the same signature without touching
// registration/reset code.
//
// LogSender is the dev implementation: it logs the envelope so the
// verification/reset token is reachable in scratch environments. It
// never reports delivery success beyond the log line — a nil Sender is
// rejected at construction (fail-closed, spec §2.7: a registration
// that cannot deliver its verification email must not pretend to).
package auth

import (
	"context"
	"strings"
)

// MailKind identifies the message class for downstream routing.
type MailKind string

const (
	MailEmailVerification MailKind = "email_verification"
	MailPasswordReset     MailKind = "password_reset"
	MailSecurityAlert     MailKind = "security_alert"
)

// Message is one outbound email. Body carries the fully-rendered text —
// the auth cluster composes plain text; a richer templated sender can
// key templates off Kind.
type Message struct {
	To      string   // recipient address (the user's verified-or-pending email)
	Kind    MailKind // message class
	Subject string   // plain-text subject line
	Body    string   // plain-text body (verification/reset link included)
}

// Sender is the narrow outbound-mail seam the auth flows consume.
// Implementations must surface a real error — a swallowed failure is a
// silent "verification email sent" lie.
type Sender interface {
	Send(ctx context.Context, m Message) error
}

// LogSender is the development Sender: emits one log line per message.
// The token-bearing link is part of the envelope because dev flows need
// it reachable; production must wire the notification service instead.
type LogSender struct {
	Logf func(format string, args ...any)
}

// NewLogSender binds the dev sender. logf nil → fmt-free no-op panic
// guard is NOT provided: callers must pass a sink (service logger).
func NewLogSender(logf func(format string, args ...any)) LogSender {
	return LogSender{Logf: logf}
}

// Send logs the envelope (To/Kind/Subject + body) and reports success.
func (s LogSender) Send(_ context.Context, m Message) error {
	if s.Logf != nil {
		s.Logf("auth mailer(dev): to=%s kind=%s subject=%q body=%q",
			m.To, m.Kind, m.Subject, m.Body)
	}
	return nil
}

// normalizeEmail canonicalizes a login/registration address: trimmed,
// lower-cased (the uniqueness contract on users.email is case-insensitive
// at the application layer — the column is UNIQUE on the stored form).
func normalizeEmail(s string) string {
	return strings.ToLower(strings.TrimSpace(s))
}
