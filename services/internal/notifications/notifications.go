package notifications

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	excerrors "exchange/pkg/errors"
)

// Channel vocabulary — the four Task 12.3.5 delivery channels. The
// notification_deliveries CHECK constraint mirrors this set.
const (
	ChannelEmail = "email"
	ChannelSMS   = "sms"
	ChannelPush  = "push"
	ChannelWS    = "ws"
)

// Event vocabulary — the task's event list. Emitters must use these
// tokens; unknown events reject INVALID_REQUEST at the Notify boundary
// (fail closed — a typo'd event must not silently deliver nowhere).
const (
	EventDepositConfirmed    = "deposit_confirmed"
	EventWithdrawalCompleted = "withdrawal_completed"
	EventOrderFilled         = "order_filled"
	EventKYCApproved         = "kyc_approved"
	EventKYCRejected         = "kyc_rejected"
	EventLiquidationWarning  = "liquidation_warning"
	EventSecurityAlert       = "security_alert"
	// EventTradingHalt (Phase-14 Task 14.3.2): anomaly-driven auto-halt —
	// instrument suspended by the circuit breaker; users holding an open
	// position or working order on the symbol are notified.
	EventTradingHalt = "trading_halt"
	// EventKYCTierDowngraded (Phase-14 Task 14.3.4): the hourly
	// re-verification sweep auto-downgraded an overdue T2 account to T1
	// (an INSTITUTIONAL lapse additionally reverts client_category).
	EventKYCTierDowngraded = "kyc_tier_downgraded"
	// EventCopyChildSkipped (Phase-14 Task 14.3.14): a copied child order
	// was skipped — safety-mode scaling pushed the pro-rata quantity
	// below the instrument minimum. Never silently dropped: the durable
	// copy_child_orders row is written first, this notice explains it.
	EventCopyChildSkipped = "copy_child_skipped"
	// EventTradeBusted / EventTradePriceAdjusted (Phase-15 Task 15.3.5):
	// an executed obvious-error correction — both counterparties of the
	// corrected trade are notified (spec §5.29/§24 #138).
	EventTradeBusted        = "trade_busted"
	EventTradePriceAdjusted = "trade_price_adjusted"
	// EventPositionDepreciation (Phase-20 Task 20.3.15): a RETAIL
	// leveraged position depreciated past a −10% multiple (MiFID II
	// 10%-rule, spec §16.9/§24 #375). Payload carries position_id,
	// symbol, threshold_pct, dep_pct, depreciated_value and
	// margin_level_pct.
	EventPositionDepreciation = "position_depreciation"
)

var validChannels = map[string]bool{
	ChannelEmail: true, ChannelSMS: true, ChannelPush: true, ChannelWS: true,
}

var validEvents = map[string]bool{
	EventDepositConfirmed:     true,
	EventWithdrawalCompleted:  true,
	EventOrderFilled:          true,
	EventKYCApproved:          true,
	EventKYCRejected:          true,
	EventLiquidationWarning:   true,
	EventSecurityAlert:        true,
	EventTradingHalt:          true,
	EventKYCTierDowngraded:    true,
	EventCopyChildSkipped:     true,
	EventTradeBusted:          true,
	EventTradePriceAdjusted:   true,
	EventPositionDepreciation: true,
}

// criticalEvents bypass quiet hours (Task 12.3.6 item 4 ruling):
// security alerts, margin-safety warnings and trading-halt notices are
// never deferred — the harm of a delayed credential-theft, liquidation
// or market-suspension notice outweighs a do-not-disturb preference.
// All other events defer to window end.
var criticalEvents = map[string]bool{
	EventSecurityAlert:      true,
	EventLiquidationWarning: true,
	EventTradingHalt:        true,
	// A busted/repriced trade moved money — never defer that notice.
	EventTradeBusted:        true,
	EventTradePriceAdjusted: true,
	// The MiFID II 10%-rule notice is a statutory same-BUSINESS-DAY duty
	// (spec §16.9) — deferring it past quiet-hours end could push
	// delivery past the deadline, so it bypasses quiet hours like the
	// other money-safety events.
	EventPositionDepreciation: true,
}

// ValidChannel reports whether c is a known channel token.
func ValidChannel(c string) bool { return validChannels[c] }

// ValidEvent reports whether e is a known event token.
func ValidEvent(e string) bool { return validEvents[e] }

// IsCritical reports whether event bypasses quiet hours.
func IsCritical(event string) bool { return criticalEvents[event] }

// Events returns the canonical event list (sorted) — the preferences
// payload and tests iterate it.
func Events() []string {
	return []string{
		EventDepositConfirmed, EventKYCApproved, EventKYCRejected,
		EventKYCTierDowngraded, EventLiquidationWarning, EventOrderFilled,
		EventPositionDepreciation, EventSecurityAlert, EventTradeBusted,
		EventTradePriceAdjusted, EventTradingHalt, EventWithdrawalCompleted,
	}
}

// Channels returns the canonical channel list (sorted).
func Channels() []string {
	return []string{ChannelEmail, ChannelPush, ChannelSMS, ChannelWS}
}

// Delivery status lattice (mirrors the migration 028 CHECK).
const (
	StatusQueued       = "QUEUED"
	StatusDelivered    = "DELIVERED"
	StatusSuppressed   = "SUPPRESSED"
	StatusDeadLettered = "DEAD_LETTERED"
)

// Notification is the unit emitters hand to the service.
type Notification struct {
	UserID  int64          `json:"user_id"`
	Event   string         `json:"event"`
	Payload map[string]any `json:"payload"`
}

// QueueItem is the Redis-queue wire shape: one per (notification,
// channel) leg. Attempts is the retry-authority — the PG column mirrors
// it so tracking stays truthful even while PG is unreachable.
type QueueItem struct {
	DeliveryID int64           `json:"delivery_id"`
	UserID     int64           `json:"user_id"`
	Channel    string          `json:"channel"`
	Event      string          `json:"event"`
	Payload    json.RawMessage `json:"payload"`
	Attempts   int             `json:"attempts"`
}

// Queuer is the queue seam — *Queue (Redis) in production, in-memory
// fake in unit tests.
type Queuer interface {
	Enqueue(ctx context.Context, item QueueItem) error
	Pop(ctx context.Context, timeout time.Duration) (QueueItem, string, bool, error)
	Ack(ctx context.Context, raw string) error
	RequeueAll(ctx context.Context) (int, error)
	ScheduleRetry(ctx context.Context, item QueueItem, at time.Time) error
	PromoteDue(ctx context.Context, now time.Time, limit int64) (int, error)
}

// Service owns the emit API + delivery bookkeeping. Construct via
// NewService; run the worker via NewDispatcher().Run(ctx).
type Service struct {
	store   Store
	queue   Queuer
	senders map[string]Sender
	anti    AntiPhishLookup
	dir     RecipientDirectory
	now     func() time.Time
	logf    func(format string, args ...any)
}

// Options wires Service. Store and Queue are mandatory — §24 #100 makes
// delivery tracking non-optional, and a notification service without a
// queue is a no-op (fail closed at construction).
type Options struct {
	Store   Store
	Queue   Queuer
	Senders []Sender // one per channel; duplicates/unknown rejected
	Anti    AntiPhishLookup
	Dir     RecipientDirectory
	Logf    func(format string, args ...any)
	Now     func() time.Time // tests only
}

// NewService validates wiring and builds the service.
func NewService(o Options) (*Service, error) {
	if o.Store == nil {
		return nil, fmt.Errorf("notifications: store required (§24 #100 tracking)")
	}
	if o.Queue == nil {
		return nil, fmt.Errorf("notifications: queue required")
	}
	s := &Service{
		store: o.Store, queue: o.Queue,
		senders: map[string]Sender{}, anti: o.Anti, dir: o.Dir,
		now: time.Now, logf: o.Logf,
	}
	if o.Now != nil {
		s.now = o.Now
	}
	for _, snd := range o.Senders {
		if snd == nil {
			continue
		}
		ch := snd.Channel()
		if !ValidChannel(ch) {
			return nil, fmt.Errorf("notifications: sender channel %q unknown", ch)
		}
		if _, dup := s.senders[ch]; dup {
			return nil, fmt.Errorf("notifications: duplicate sender for channel %q", ch)
		}
		s.senders[ch] = snd
	}
	return s, nil
}

// log emits to the wired sink when present — nil sink means drop
// (same convention as the funding services).
func (s *Service) log(format string, args ...any) {
	if s.logf != nil {
		s.logf(format, args...)
	}
}

// Notify emits one user event: it expands the event over the user's
// enabled channels (preference matrix, defaults when no row exists),
// records one notification_deliveries row per leg, and enqueues each
// leg on notifications:pending. Returns the number of queued legs.
//
// Legs are independent: a channel the user disabled is skipped; a
// channel with no registered sender is skipped (dev partial wiring).
// If the queue enqueue fails after the delivery row was written, the
// row is marked DEAD_LETTERED with the enqueue error — a QUEUED row
// that was never enqueued is a tracking lie (fail closed, §2.7).
func (s *Service) Notify(ctx context.Context, userID int64, event string, payload map[string]any) (int, error) {
	return s.Emit(ctx, Notification{UserID: userID, Event: event, Payload: payload})
}

// Emit is the struct-shaped twin of Notify for emitters that already
// hold a Notification value.
func (s *Service) Emit(ctx context.Context, n Notification) (int, error) {
	if n.UserID <= 0 {
		return 0, excerrors.New("INVALID_REQUEST", "notifications: user_id required")
	}
	if !ValidEvent(n.Event) {
		return 0, excerrors.New("INVALID_REQUEST",
			fmt.Sprintf("notifications: unknown event %q", n.Event))
	}
	raw, err := json.Marshal(n.Payload)
	if err != nil {
		return 0, excerrors.Wrap("INTERNAL_ERROR", "notifications: encode payload", err)
	}
	pref, err := s.store.GetPreferences(ctx, n.UserID)
	if err != nil {
		return 0, excerrors.Wrap("INTERNAL_ERROR", "notifications: preferences", err)
	}
	if pref == nil {
		pref = DefaultPreferences(n.UserID)
	}
	queued := 0
	for _, ch := range Channels() {
		if !pref.Enabled(n.Event, ch) {
			continue
		}
		if _, ok := s.senders[ch]; !ok {
			continue // no sender wired for this channel — unroutable, skip
		}
		deliveryID, err := s.store.InsertDelivery(ctx, Delivery{
			UserID: n.UserID, Channel: ch, Event: n.Event,
			Payload: raw, Status: StatusQueued, MaxAttempts: MaxAttempts,
		})
		if err != nil {
			return queued, excerrors.Wrap("INTERNAL_ERROR", "notifications: record delivery", err)
		}
		item := QueueItem{
			DeliveryID: deliveryID, UserID: n.UserID, Channel: ch,
			Event: n.Event, Payload: raw,
		}
		if err := s.queue.Enqueue(ctx, item); err != nil {
			_ = s.store.MarkStatus(ctx, deliveryID, StatusDeadLettered,
				0, "queue enqueue: "+err.Error())
			return queued, excerrors.Wrap("INTERNAL_ERROR", "notifications: enqueue", err)
		}
		queued++
	}
	return queued, nil
}
