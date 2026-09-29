// Sender abstraction + dev implementations (Task 12.3.5 items 1/8).
//
// Production providers are seams, not code: SES/SendGrid bind behind
// Sender on the email channel, Twilio on sms, FCM/APNS on push. The
// shipped implementations are deliberately non-external:
//
//	LogSender   — slog line per send (default dev wiring; observable,
//	              zero dependencies).
//	FileSender  — appends one JSON line per send to <dir>/<channel>.jsonl
//	              (dev "mailbox" — inspect what would have gone out).
//	MemSender   — in-memory capture with a failure knob (tests).
//	WSSender    — pushes through the gateway's private:* hub via the
//	              Pusher seam (channel "private:notifications").
package notifications

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Message is the channel-agnostic outbound unit.
type Message struct {
	DeliveryID int64           `json:"delivery_id"`
	UserID     int64           `json:"user_id"`
	Channel    string          `json:"channel"`
	Event      string          `json:"event"`
	To         string          `json:"to,omitempty"`
	Subject    string          `json:"subject"`
	Body       string          `json:"body"`
	Payload    json.RawMessage `json:"payload"`
	SentAt     time.Time       `json:"sent_at"`
}

// Sender delivers one Message on one channel.
type Sender interface {
	Channel() string
	Send(ctx context.Context, msg Message) error
}

// ---------------------------------------------------------------------------
// Anti-phishing seam (task item 9)
// ---------------------------------------------------------------------------

// AntiPhishLookup resolves the user's anti_phishing_code. The users
// column + its setter/getter are owned by another Phase-12 task; this
// interface is the documented seam — wiring binds PgAntiPhish once the
// column exists, or a nil lookup (every send renders the "set your
// anti-phishing code" banner).
type AntiPhishLookup interface {
	Code(ctx context.Context, userID int64) (string, error)
}

// AntiPhishFunc adapts a function to AntiPhishLookup.
type AntiPhishFunc func(ctx context.Context, userID int64) (string, error)

// Code implements AntiPhishLookup.
func (f AntiPhishFunc) Code(ctx context.Context, userID int64) (string, error) {
	return f(ctx, userID)
}

// banner renders the anti-phishing header for email/SMS bodies. Unset
// (or unresolvable) code → the prompt banner, exactly as spec'd.
func antiPhishBanner(code string) string {
	if code == "" {
		return "You have not set an anti-phishing code. Set one under " +
			"Settings \u2192 Security \u2192 Anti-Phishing Code and verify it " +
			"appears in every official exc.local message."
	}
	return fmt.Sprintf("Anti-phishing code: %s\nIf this code does not match "+
		"the phrase you configured, this message is NOT from exc.local.", code)
}

// ---------------------------------------------------------------------------
// LogSender — default dev sender
// ---------------------------------------------------------------------------

// LogSender logs each send through slog (observable dev delivery).
type LogSender struct {
	Ch  string
	Log *slog.Logger
}

// Channel implements Sender.
func (s *LogSender) Channel() string { return s.Ch }

// Send implements Sender.
func (s *LogSender) Send(_ context.Context, msg Message) error {
	log := s.Log
	if log == nil {
		log = slog.Default()
	}
	log.Info("notification send",
		"channel", msg.Channel, "event", msg.Event,
		"user_id", msg.UserID, "delivery_id", msg.DeliveryID,
		"to", msg.To, "subject", msg.Subject)
	return nil
}

// ---------------------------------------------------------------------------
// FileSender — dev mailbox
// ---------------------------------------------------------------------------

// FileSender appends each send as a JSON line to <dir>/<channel>.jsonl.
type FileSender struct {
	Ch  string
	Dir string
	mu  sync.Mutex
}

// Channel implements Sender.
func (s *FileSender) Channel() string { return s.Ch }

// Send implements Sender.
func (s *FileSender) Send(_ context.Context, msg Message) error {
	b, err := json.Marshal(msg)
	if err != nil {
		return fmt.Errorf("notifications: file sender encode: %w", err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := os.MkdirAll(s.Dir, 0o750); err != nil {
		return fmt.Errorf("notifications: file sender dir: %w", err)
	}
	f, err := os.OpenFile(filepath.Join(s.Dir, s.Ch+".jsonl"),
		os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o640)
	if err != nil {
		return fmt.Errorf("notifications: file sender open: %w", err)
	}
	defer func() { _ = f.Close() }()
	if _, err := f.Write(append(b, '\n')); err != nil {
		return fmt.Errorf("notifications: file sender write: %w", err)
	}
	return nil
}

// ---------------------------------------------------------------------------
// MemSender — test capture
// ---------------------------------------------------------------------------

// MemSender records sends; FailErr forces Send to error (retry tests).
type MemSender struct {
	Ch      string
	FailErr error
	mu      sync.Mutex
	Sent    []Message
}

// Channel implements Sender.
func (s *MemSender) Channel() string { return s.Ch }

// Send implements Sender.
func (s *MemSender) Send(_ context.Context, msg Message) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.FailErr != nil {
		return s.FailErr
	}
	s.Sent = append(s.Sent, msg)
	return nil
}

// Messages returns the captured sends.
func (s *MemSender) Messages() []Message {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Message, len(s.Sent))
	copy(out, s.Sent)
	return out
}

// ---------------------------------------------------------------------------
// WSSender — in-platform delivery over the gateway private hub
// ---------------------------------------------------------------------------

// Pusher is the seam the gateway's ws.Server satisfies via an adapter:
// it fans a payload out to the user's account-scoped private channel
// ("private:notifications" on /ws/v1 — the Phase-10 frontend
// notification channel).
type Pusher interface {
	PushToUser(ctx context.Context, userID int64, channel string, data any) error
}

// PusherFunc adapts a function to Pusher.
type PusherFunc func(ctx context.Context, userID int64, channel string, data any) error

// PushToUser implements Pusher.
func (f PusherFunc) PushToUser(ctx context.Context, userID int64, channel string, data any) error {
	return f(ctx, userID, channel, data)
}

// WSChannel is the canonical private channel name — the Phase-10
// frontend subscribes to "private:notifications" on /ws/v1.
const WSChannel = "private:notifications"

// WSSender delivers through the in-process WS hub.
type WSSender struct {
	Push Pusher // nil fails Send (wired sender with no hub is a bug)
}

// Channel implements Sender.
func (s *WSSender) Channel() string { return ChannelWS }

// Send implements Sender.
func (s *WSSender) Send(ctx context.Context, msg Message) error {
	if s.Push == nil {
		return fmt.Errorf("notifications: ws sender has no pusher")
	}
	return s.Push.PushToUser(ctx, msg.UserID, WSChannel, map[string]any{
		"delivery_id": msg.DeliveryID,
		"event":       msg.Event,
		"subject":     msg.Subject,
		"payload":     msg.Payload,
		"sent_at":     msg.SentAt.UTC().Format(time.RFC3339Nano),
	})
}
