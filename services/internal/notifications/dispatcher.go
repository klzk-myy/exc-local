package notifications

import (
	"context"
	"fmt"
	"time"
)

// RetryPolicy is the task's exponential-backoff ladder: 1s, 2s, 4s, 8s,
// 16s. The fifth failure dead-letters (MaxAttempts = len(RetryPolicy)).
var RetryPolicy = []time.Duration{
	1 * time.Second,
	2 * time.Second,
	4 * time.Second,
	8 * time.Second,
	16 * time.Second,
}

// MaxAttempts bounds send attempts per delivery leg.
const MaxAttempts = 5

// backoffFor returns the delay before the NEXT attempt after the
// attempt that just failed (attempt is the post-increment count, 1-based
// — first failure waits RetryPolicy[0]).
func backoffFor(attempt int) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	if attempt > len(RetryPolicy) {
		attempt = len(RetryPolicy)
	}
	return RetryPolicy[attempt-1]
}

// Dispatcher drains the notification queue. Run it as a goroutine next
// to the gateway; it exits on ctx cancellation (graceful shutdown —
// claimed-but-unacked items are requeued on the next boot's RequeueAll).
type Dispatcher struct {
	svc           *Service
	popTimeout    time.Duration // BLMOVE wait — bounds shutdown latency
	retryInterval time.Duration // PromoteDue cadence
	batch         int64
}

// NewDispatcher builds the worker with the repo's conservative defaults.
func (s *Service) NewDispatcher() *Dispatcher {
	return &Dispatcher{
		svc:           s,
		popTimeout:    1 * time.Second,
		retryInterval: 500 * time.Millisecond,
		batch:         100,
	}
}

// Run first requeues any claimed-but-unacked strays from a previous
// process, then loops: promote due retries → pop → handle. Shutdown is
// responsive within popTimeout.
func (d *Dispatcher) Run(ctx context.Context) {
	if n, err := d.svc.queue.RequeueAll(ctx); err != nil {
		d.svc.log("notifications: processing requeue failed: %v", err)
	} else if n > 0 {
		d.svc.log("notifications: requeued %d claimed items", n)
	}
	retry := time.NewTicker(d.retryInterval)
	defer retry.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-retry.C:
			if _, err := d.svc.queue.PromoteDue(ctx, d.svc.now(), d.batch); err != nil {
				d.svc.log("notifications: retry promote failed: %v", err)
			}
		default:
		}
		item, raw, ok, err := d.svc.queue.Pop(ctx, d.popTimeout)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			if raw != "" {
				// Undecodable queue item: park it as a dead letter —
				// it can never be dispatched and must not spin.
				d.svc.log("notifications: undecodable queue item dropped to review: %v", err)
				_ = d.svc.queue.Ack(ctx, raw) // bytes preserved in logs; DLQ needs a delivery row to reference
			} else {
				d.svc.log("notifications: pop failed: %v", err)
				select {
				case <-ctx.Done():
					return
				case <-time.After(500 * time.Millisecond):
				}
			}
			continue
		}
		if !ok {
			continue // timeout — loop (retry ticker also drains)
		}
		d.process(ctx, item, raw)
	}
}

// process handles one claimed item: preference re-check → quiet-hours
// gate → recipient/anti-phish resolution → send → outcome bookkeeping.
func (d *Dispatcher) process(ctx context.Context, item QueueItem, raw string) {
	s := d.svc
	now := s.now()

	// Preference re-check: a user who opted out between enqueue and
	// dispatch gets SUPPRESSED, not a delivery (per-event opt-out is the
	// task contract). A lookup error is transient — retry, never send on
	// an unreadable preference (fail closed).
	pref, err := s.store.GetPreferences(ctx, item.UserID)
	if err != nil {
		d.retry(ctx, item, raw, "preferences lookup: "+err.Error())
		return
	}
	if pref == nil {
		pref = DefaultPreferences(item.UserID)
	}
	if !pref.Enabled(item.Event, item.Channel) {
		_ = s.store.MarkStatus(ctx, item.DeliveryID, StatusSuppressed,
			item.Attempts, "channel disabled by user preference")
		d.ack(ctx, raw)
		return
	}

	// Quiet hours (UTC): non-critical events defer to window end;
	// CRITICAL (security_alert, liquidation_warning) bypass entirely.
	if !IsCritical(item.Event) && pref.InQuietHours(now) {
		if err := s.queue.ScheduleRetry(ctx, item, pref.QuietEndAfter(now)); err != nil {
			d.retry(ctx, item, raw, "quiet-hours defer: "+err.Error())
			return
		}
		d.ack(ctx, raw)
		return
	}

	sender, ok := s.senders[item.Channel]
	if !ok || sender == nil {
		// Sender was wired at enqueue but vanished — SUPPRESSED, not a
		// retry loop (the wiring defect is a code bug, not transient).
		_ = s.store.MarkStatus(ctx, item.DeliveryID, StatusSuppressed,
			item.Attempts, "no sender for channel "+item.Channel)
		d.ack(ctx, raw)
		return
	}

	msg, err := s.buildMessage(ctx, item)
	if err != nil {
		// Unroutable recipient is terminal (no address on file); a store
		// error is transient — buildMessage distinguishes them.
		if nerr, ok2 := err.(*unroutableError); ok2 {
			_ = s.store.MarkStatus(ctx, item.DeliveryID, StatusSuppressed,
				item.Attempts, nerr.Error())
			d.ack(ctx, raw)
			return
		}
		d.retry(ctx, item, raw, "build message: "+err.Error())
		return
	}

	// Count the attempt before sending: a crash between send and
	// bookkeeping may re-attempt, but attempts must never understate.
	item.Attempts++
	_ = s.store.RecordAttempt(ctx, item.DeliveryID, item.Attempts, "")

	if err := sender.Send(ctx, msg); err != nil {
		_ = s.store.RecordAttempt(ctx, item.DeliveryID, item.Attempts, err.Error())
		if item.Attempts >= MaxAttempts {
			if derr := s.store.DeadLetter(ctx, item.DeliveryID,
				item.Attempts, err.Error()); derr != nil {
				// Cannot record the terminal state — leave the item
				// claimed in processing; the next boot requeues it
				// rather than losing a dead letter (zero-loss, §2.7).
				s.log("notifications: dead-letter record failed for delivery %d: %v",
					item.DeliveryID, derr)
				return
			}
			d.ack(ctx, raw)
			return
		}
		if rerr := s.queue.ScheduleRetry(ctx, item,
			now.Add(backoffFor(item.Attempts))); rerr != nil {
			s.log("notifications: retry schedule failed for delivery %d: %v",
				item.DeliveryID, rerr)
			return // stays claimed → requeued on restart
		}
		d.ack(ctx, raw)
		return
	}

	if err := s.store.MarkDelivered(ctx, item.DeliveryID, item.Attempts, now); err != nil {
		// Send succeeded; tracking update failed. Leave claimed — the
		// restart requeue re-delivers (idempotent per delivery_id on the
		// receiver side; the duplicate outranks a false QUEUED row).
		s.log("notifications: delivered record failed for delivery %d: %v",
			item.DeliveryID, err)
		return
	}
	d.ack(ctx, raw)
}

// retry requeues a transiently-failed item on the backoff ladder, or
// dead-letters it once attempts are exhausted. Unrecordable terminal
// states keep the item claimed for restart recovery.
func (d *Dispatcher) retry(ctx context.Context, item QueueItem, raw, lastErr string) {
	s := d.svc
	item.Attempts++
	if item.Attempts >= MaxAttempts {
		if err := s.store.DeadLetter(ctx, item.DeliveryID, item.Attempts, lastErr); err != nil {
			s.log("notifications: dead-letter record failed for delivery %d: %v",
				item.DeliveryID, err)
			return // stays claimed → requeued on restart
		}
		d.ack(ctx, raw)
		return
	}
	if err := s.queue.ScheduleRetry(ctx, item,
		s.now().Add(backoffFor(item.Attempts))); err != nil {
		s.log("notifications: retry schedule failed for delivery %d: %v",
			item.DeliveryID, err)
		return // stays claimed → requeued on restart
	}
	d.ack(ctx, raw)
}

// ack removes the item from processing; failures are logged (the item
// stays claimed and is requeued on restart — never silently dropped).
func (d *Dispatcher) ack(ctx context.Context, raw string) {
	if err := d.svc.queue.Ack(ctx, raw); err != nil {
		d.svc.log("notifications: ack failed: %v", err)
	}
}

// unroutableError marks permanent send-impossibility (e.g. user has no
// email/phone on file) — SUPPRESSED, never retried.
type unroutableError struct{ msg string }

func (e *unroutableError) Error() string { return e.msg }

// buildMessage renders the outbound Message: recipient address,
// per-event subject and the anti-phishing banner on email/SMS bodies.
func (s *Service) buildMessage(ctx context.Context, item QueueItem) (Message, error) {
	msg := Message{
		DeliveryID: item.DeliveryID, UserID: item.UserID,
		Channel: item.Channel, Event: item.Event,
		Subject: subjectFor(item.Event), Payload: item.Payload,
		SentAt: s.now(),
	}
	// Recipient resolution for address-bound channels.
	if item.Channel == ChannelEmail || item.Channel == ChannelSMS {
		if s.dir == nil {
			return msg, fmt.Errorf("notifications: no recipient directory wired")
		}
		email, phone, err := s.dir.Recipient(ctx, item.UserID)
		if err != nil {
			return msg, err // transient — retry
		}
		to := email
		if item.Channel == ChannelSMS {
			to = phone
		}
		if to == "" {
			return msg, &unroutableError{msg: fmt.Sprintf(
				"no %s address on file for user %d", item.Channel, item.UserID)}
		}
		msg.To = to
	}
	// Anti-phishing banner on email/SMS — lookup errors degrade to the
	// unset banner (a banner outage must never block the send).
	if item.Channel == ChannelEmail || item.Channel == ChannelSMS {
		code := ""
		if s.anti != nil {
			c, err := s.anti.Code(ctx, item.UserID)
			if err != nil {
				s.log("notifications: anti-phish lookup user %d: %v", item.UserID, err)
			} else {
				code = c
			}
		}
		msg.Body = antiPhishBanner(code) + "\n\n" + s.renderBody(item)
	} else {
		msg.Body = s.renderBody(item)
	}
	return msg, nil
}

// subjectFor maps events to fixed outbound subjects.
func subjectFor(event string) string {
	switch event {
	case EventDepositConfirmed:
		return "Deposit confirmed"
	case EventWithdrawalCompleted:
		return "Withdrawal completed"
	case EventOrderFilled:
		return "Order filled"
	case EventKYCApproved:
		return "Identity verification approved"
	case EventKYCRejected:
		return "Identity verification rejected"
	case EventLiquidationWarning:
		return "Liquidation warning — margin level critical"
	case EventSecurityAlert:
		return "Security alert"
	default:
		return "Notification"
	}
}

// renderBody renders the event payload as the text body — stable JSON
// until per-channel template work lands (Phase-12 follow-on).
func (s *Service) renderBody(item QueueItem) string {
	if len(item.Payload) == 0 {
		return item.Event
	}
	return fmt.Sprintf("%s\n\n%s", item.Event, string(item.Payload))
}
