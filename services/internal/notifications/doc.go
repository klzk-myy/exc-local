// Package notifications implements the Phase-12 Task 12.3.5 notification
// service: multi-channel user notifications with bounded retry,
// dead-lettering and §24 #100 delivery tracking.
//
// Channels: email, SMS, push and in-platform WebSocket. Production
// providers (SES/SendGrid, Twilio, FCM/APNS) are interface seams — the
// shipped implementations are dev senders (slog / JSONL file / in-memory
// test capture) plus a WS sender over the gateway's private:* hub
// (channel "private:notifications", Phase-10 frontend consumer).
//
// Pipeline:
//
//	Notify(userID, event, payload)
//	  → preference matrix expands the event into per-channel deliveries
//	  → notification_deliveries row per channel (PG, §24 #100 tracking)
//	  → queue item LPUSH'ed onto Redis list notifications:pending
//	Dispatcher (goroutine, graceful shutdown via ctx)
//	  → BLMOVE pending → notifications:processing (reliable-queue claim)
//	  → preference re-check + quiet-hours gate (UTC; CRITICAL events
//	    — security_alert, liquidation_warning — bypass)
//	  → channel Sender.Send; attempts recorded on the delivery row
//	  → failure: ZADD notifications:retry at now+backoff
//	    (1s,2s,4s,8s,16s); after max 5 attempts the delivery is marked
//	    DEAD_LETTERED and copied into notification_dead_letters
//	    (migration 028) — one transaction, never silently dropped.
//	  → shutdown: an in-flight (claimed) item stays in
//	    notifications:processing; Run requeues it on next boot
//	    (RequeueAll) so a restart never loses a claimed notification.
//
// Anti-phishing (Task 12.3.5 item 9): every outbound email/SMS renders
// the user's anti_phishing_code in a header banner. The users column is
// owned by another task — this package defines the seam
// (AntiPhishLookup; PgAntiPhish binds users.anti_phishing_code). An
// unset code renders a "set your anti-phishing code" banner instead;
// a lookup error also renders the banner and is recorded on the
// delivery — degraded banner, never a blocked security alert.
//
// Wiring (cmd/gateway/main.go): PgStore over the shared pool, Queue
// over the coordination Redis, dev senders on all four channels
// (WSSender → ws.Server.PublishPrivate per user account), PgAntiPhish,
// dispatcher goroutine sharing the sweep lifecycle. Funding emits
// deposit_confirmed / withdrawal_completed through the optional
// funding.Notifier seam; the orders consumer emits order_filled via
// WithFillHook.
package notifications
