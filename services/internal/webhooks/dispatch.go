package webhooks

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"time"
)

// RetryPolicy is the Task 5.3.17 schedule: exponential backoff 1s → 2s →
// 4s → 8s → 16s, five attempts total (the 5th failure dead-letters).
var RetryPolicy = []time.Duration{
	1 * time.Second,
	2 * time.Second,
	4 * time.Second,
	8 * time.Second,
	16 * time.Second,
}

// DefaultMaxAttempts matches the column default.
const DefaultMaxAttempts = 5

// Headers on every webhook POST. Signatures are
// hex(HMAC-SHA256(secret, "<unix-ts>.<body>")) — receivers recompute and
// compare constant-time; the timestamp binds the signature and gives the
// receiver a freshness check.
const (
	HeaderSignature  = "X-Webhook-Signature"
	HeaderTimestamp  = "X-Webhook-Timestamp"
	HeaderEvent      = "X-Webhook-Event"
	HeaderDeliveryID = "X-Webhook-Delivery-Id"
)

// Sign produces the canonical signature for (ts, body).
func Sign(secret []byte, ts time.Time, body []byte) string {
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(strconv.FormatInt(ts.Unix(), 10)))
	mac.Write([]byte("."))
	mac.Write(body)
	return hex.EncodeToString(mac.Sum(nil))
}

// Publisher is the domain-facing seam: emit an event for an account and
// every subscribed endpoint gets a queued delivery. Emitters (orders,
// funding, withdrawals) call Publish — they never deliver inline.
type Publisher interface {
	Publish(ctx context.Context, accountID int64, event string, payload any) (int, error)
}

// Envelope is the delivery body — a stable JSON shape so consumers can
// version on `event` and `delivery_id` is idempotent.
type Envelope struct {
	DeliveryID string `json:"delivery_id"`
	Event      string `json:"event"`
	AccountID  int64  `json:"account_id"`
	Data       any    `json:"data"`
	SentAt     string `json:"sent_at"`
}

// Dispatcher owns the delivery loop: it drains due PENDING rows and
// POSTs them. Run it as a goroutine next to the gateway; Run exits on
// ctx cancellation.
type Dispatcher struct {
	store    *Store
	client   *http.Client
	interval time.Duration
	batch    int
	now      func() time.Time
}

// NewDispatcher builds the worker. client may be nil (a bounded default
// is used). interval is the poll cadence; ≤0 defaults to 500ms.
func NewDispatcher(store *Store, client *http.Client, interval time.Duration) *Dispatcher {
	if interval <= 0 {
		interval = 500 * time.Millisecond
	}
	if client == nil {
		client = &http.Client{
			Timeout: 10 * time.Second,
			Transport: &http.Transport{
				DialContext: (&net.Dialer{Timeout: 3 * time.Second}).DialContext,
			},
		}
	}
	return &Dispatcher{store: store, client: client, interval: interval, batch: 100, now: time.Now}
}

// SetClockForTest overrides the clock; tests only.
func (d *Dispatcher) SetClockForTest(now func() time.Time) { d.now = now }

// Publish implements Publisher: enqueue one PENDING delivery per ACTIVE
// subscriber of (account, event). Payload is wrapped in the Envelope.
func (d *Dispatcher) Publish(ctx context.Context, accountID int64, event string, payload any) (int, error) {
	env := Envelope{
		Event:     event,
		AccountID: accountID,
		Data:      payload,
		SentAt:    d.now().UTC().Format(time.RFC3339Nano),
	}
	raw, err := json.Marshal(env)
	if err != nil {
		return 0, fmt.Errorf("webhooks: encode envelope: %w", err)
	}
	return d.store.Enqueue(ctx, accountID, event, raw)
}

// Run polls for due deliveries until ctx is done. Cheap partial index on
// next_attempt_at keeps the poll tiny.
func (d *Dispatcher) Run(ctx context.Context) {
	ticker := time.NewTicker(d.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			_ = d.Drain(ctx)
		}
	}
}

// Drain processes one batch; returns deliveries attempted.
func (d *Dispatcher) Drain(ctx context.Context) int {
	due, err := d.store.ClaimDue(ctx, d.batch)
	if err != nil || len(due) == 0 {
		return 0
	}
	for _, delivery := range due {
		d.deliver(ctx, delivery)
	}
	return len(due)
}

// backoffFor returns the delay before the NEXT attempt after the attempt
// that just failed. delivery.Attempts is the post-claim count (1-based),
// so callers pass Attempts-1 to index RetryPolicy 0-based — the first
// failure waits RetryPolicy[0]=1s, the fifth dead-letters via
// FailDelivery's attempts>=max_attempts check.
func backoffFor(attempt int) time.Duration {
	if attempt < 0 {
		attempt = 0
	}
	if attempt >= len(RetryPolicy) {
		attempt = len(RetryPolicy) - 1
	}
	return RetryPolicy[attempt]
}

// deliver performs one HTTP attempt for a claimed row.
func (d *Dispatcher) deliver(ctx context.Context, delivery Delivery) {
	ep, err := d.store.endpointByInternalID(ctx, delivery.EndpointID)
	if err != nil || ep.Status != StatusActive {
		// Endpoint gone or disabled between claim and attempt: fail once;
		// the retry loop will dead-letter after max_attempts, which is the
		// bounded, observable terminal state.
		_ = d.store.FailDelivery(ctx, delivery.ID, 0,
			"endpoint unavailable", d.now().Add(backoffFor(delivery.Attempts-1)))
		return
	}
	secret, prev, err := d.store.SecretsForDelivery(ctx, delivery.EndpointID)
	if err != nil {
		_ = d.store.FailDelivery(ctx, delivery.ID, 0,
			"secret unavailable", d.now().Add(backoffFor(delivery.Attempts-1)))
		return
	}

	ts := d.now().UTC()
	sig := Sign(secret, ts, delivery.Payload)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, ep.URL,
		bytes.NewReader(delivery.Payload))
	if err != nil {
		_ = d.store.FailDelivery(ctx, delivery.ID, 0, "request build: "+err.Error(),
			d.now().Add(backoffFor(delivery.Attempts-1)))
		return
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(HeaderEvent, delivery.Event)
	req.Header.Set(HeaderDeliveryID, delivery.DeliveryID)
	req.Header.Set(HeaderTimestamp, strconv.FormatInt(ts.Unix(), 10))
	req.Header.Set(HeaderSignature, sig)
	if len(prev) > 0 {
		// During rotation overlap emit the predecessor signature too so
		// consumers mid-switchover keep verifying — bounded ≤72h window.
		req.Header.Set("X-Webhook-Signature-Prev", Sign(prev, ts, delivery.Payload))
	}

	resp, err := d.client.Do(req)
	if err != nil {
		_ = d.store.FailDelivery(ctx, delivery.ID, 0,
			"transport: "+err.Error(), d.now().Add(backoffFor(delivery.Attempts-1)))
		return
	}
	io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
	resp.Body.Close()

	if resp.StatusCode >= 200 && resp.StatusCode <= 299 {
		_ = d.store.CompleteDelivery(ctx, delivery.ID, resp.StatusCode)
		return
	}
	_ = d.store.FailDelivery(ctx, delivery.ID, resp.StatusCode,
		fmt.Sprintf("http %d", resp.StatusCode),
		d.now().Add(backoffFor(delivery.Attempts-1)))
}
