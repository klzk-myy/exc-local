// Task 18.3.6 — Electronic trade affirmation (Traiana Harmony / MarkitSERV)
// and two-way give-up status synchronization (spec §5.22, §9.2, §24 #125).
//
// Outbound: PBDropCopy submits each recorded give-up through registered
// AffirmationSubmitter implementations (HTTPAffirmationExporter ships the
// Traiana Harmony / MarkitSERV JSON envelopes). Inbound: affirmation
// responses land on AffirmationSync.Apply which moves pb_giveup_trades
// PENDING -> AFFIRMED/REJECTED via PBStore's guarded transition. The
// AffirmationMonitor sweep flags trades still PENDING past the configured
// window (default 60s) as middle-office breaks — never silently un-affirmed.
package fix

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"
)

// AffirmationVenue names the external affirmation provider.
type AffirmationVenue string

const (
	VenueTraiana    AffirmationVenue = "TRAIANA_HARMONY"
	VenueMarkitSERV AffirmationVenue = "MARKITSERV"
)

// affirmationRequest is the wire envelope POSTed to a venue endpoint. Field
// names are the venue-normalized canonical set; per-venue dialect mapping
// (Traiana Harmony block/alloc schemas, MarkitSERV TEA/matching payloads) is
// the adapter's body-encode seam — see Encode.
type affirmationRequest struct {
	Venue            AffirmationVenue `json:"venue"`
	VenueRef         string           `json:"venue_ref"` // pb.traiana_code participant id
	GiveUpID         int64            `json:"giveup_id"`
	TradeID          int64            `json:"trade_id"`
	ExecutingAccount int64            `json:"executing_broker_account_id"`
	ClientAccount    int64            `json:"client_account_id"`
	SubmittedAt      time.Time        `json:"submitted_at"`
}

// affirmationResponse is the decoded venue acknowledgement.
type affirmationResponse struct {
	MessageID string `json:"message_id"`
	Status    string `json:"status"` // venue-specific; normalized by NormalizeAffirmStatus
	Reason    string `json:"reason"`
}

// ErrAffirmationNotConfigured fails closed when a give-up exists but no
// venue endpoint is configured — better a loud PENDING break than a silent
// skip (spec §2.7).
var ErrAffirmationNotConfigured = errors.New("fix: affirmation venue endpoint not configured")

// HTTPAffirmationExporter is the production affirmation adapter for the
// Traiana Harmony and MarkitSERV gateways: JSON POST of the give-up record,
// external reference captured from the ack. Production credentials arrive
// via the Vault/KMS secrets path (Phase-13.5 Task 13.5.3.5); this type holds
// a static token for tests/dev.
type HTTPAffirmationExporter struct {
	venue    AffirmationVenue
	endpoint string
	token    string
	client   *http.Client
}

// NewAffirmationExporter builds an exporter for one venue. Empty endpoint is
// legal — Export then returns ErrAffirmationNotConfigured (fail closed).
func NewAffirmationExporter(venue AffirmationVenue, endpoint, token string, client *http.Client) *HTTPAffirmationExporter {
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second}
	}
	return &HTTPAffirmationExporter{venue: venue, endpoint: endpoint, token: token, client: client}
}

// Submit implements AffirmationSubmitter.
func (e *HTTPAffirmationExporter) Submit(ctx context.Context, trade GiveUpTrade, pb PrimeBroker) (string, error) {
	if e.endpoint == "" {
		return "", ErrAffirmationNotConfigured
	}
	req := affirmationRequest{
		Venue:            e.venue,
		VenueRef:         pb.TraianaCode,
		GiveUpID:         trade.ID,
		TradeID:          trade.TradeID,
		ExecutingAccount: trade.ExecutingBrokerAcctID,
		ClientAccount:    trade.ClientAccountID,
		SubmittedAt:      time.Now().UTC(),
	}
	body, err := json.Marshal(req)
	if err != nil {
		return "", err
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, e.endpoint, bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if e.token != "" {
		httpReq.Header.Set("Authorization", "Bearer "+e.token)
	}
	resp, err := e.client.Do(httpReq)
	if err != nil {
		return "", fmt.Errorf("fix: %s affirmation submit: %w", e.venue, err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", err
	}
	if resp.StatusCode/100 != 2 {
		return "", fmt.Errorf("fix: %s affirmation HTTP %d: %s", e.venue, resp.StatusCode, string(raw))
	}
	var ack affirmationResponse
	if err := json.Unmarshal(raw, &ack); err != nil {
		return "", fmt.Errorf("fix: %s affirmation ack decode: %w", e.venue, err)
	}
	return ack.MessageID, nil
}

// ---------------------------------------------------------------------------
// Two-way status synchronization (Task 18.3.6 step 4).
// ---------------------------------------------------------------------------

// NormalizeAffirmStatus maps venue response vocabulary onto the
// pb_giveup_trades enum. Unknown/malformed statuses return ok=false — the
// caller must never guess a terminal state.
func NormalizeAffirmStatus(s string) (GiveUpStatus, bool) {
	switch s {
	case "AFFIRMED", "AFFIRM", "MATCHED", "CONFIRMED", "ACK", "ACCEPTED":
		return GiveUpAffirmed, true
	case "REJECTED", "REJECT", "DK", "DISAFFIRMED", "NACK":
		return GiveUpRejected, true
	case "DISPUTED", "BREAK":
		return GiveUpDisputed, true
	default:
		return "", false
	}
}

// giveUpStatusWriter is the persistence seam for inbound sync — *PBStore
// satisfies it; tests use a fake.
type giveUpStatusWriter interface {
	UpdateStatusByTraianaID(ctx context.Context, msgID string, to GiveUpStatus, reason string) error
}

// AffirmationSync applies inbound affirmation responses to pb_giveup_trades.
type AffirmationSync struct{ store giveUpStatusWriter }

// NewAffirmationSync binds the sync handler.
func NewAffirmationSync(store giveUpStatusWriter) *AffirmationSync {
	return &AffirmationSync{store: store}
}

// Apply records an affirmation response: PENDING -> AFFIRMED/REJECTED (or
// DISPUTED from Phase-24's vocabulary). Unknown statuses and unknown
// external refs fail loudly — the give-up stays PENDING and ages into the
// timeout monitor's break list.
func (s *AffirmationSync) Apply(ctx context.Context, externalRef, rawStatus, reason string) error {
	st, ok := NormalizeAffirmStatus(rawStatus)
	if !ok {
		return fmt.Errorf("fix: unknown affirmation status %q", rawStatus)
	}
	return s.store.UpdateStatusByTraianaID(ctx, externalRef, st, reason)
}

// ---------------------------------------------------------------------------
// Affirmation timeout monitor (Task 18.3.6 step 5).
// ---------------------------------------------------------------------------

// DefaultAffirmationTimeout is the spec window: trades un-affirmed after
// 60s are flagged for middle-office break alerts.
const DefaultAffirmationTimeout = 60 * time.Second

// GiveUpBreak is a middle-office break record emitted by the monitor.
type GiveUpBreak struct {
	Trade GiveUpTrade
	Age   time.Duration
}

// BreakAlerter is the alert seam — production wires it to the Phase-07 alert
// taxonomy / NATS "compliance" stream; tests use a recorder.
type BreakAlerter interface {
	AlertBreak(ctx context.Context, b GiveUpBreak) error
}

// LogAlerter is the default alerter: structured slog, no external deps.
type LogAlerter struct{ Log *slog.Logger }

// AlertBreak logs the break at warn level.
func (l LogAlerter) AlertBreak(ctx context.Context, b GiveUpBreak) error {
	l.Log.WarnContext(ctx, "pb give-up affirmation timeout",
		"giveup_id", b.Trade.ID, "trade_id", b.Trade.TradeID,
		"pb_id", b.Trade.PrimeBrokerID, "age", b.Age.String())
	return nil
}

// pendingGiveUpLister is the monitor's persistence seam — *PBStore
// satisfies it; tests use a fake.
type pendingGiveUpLister interface {
	PendingOlderThan(ctx context.Context, cutoff time.Time) ([]GiveUpTrade, error)
}

// AffirmationMonitor periodically sweeps for give-ups stuck in PENDING past
// the affirmation window and raises a middle-office break alert per trade.
type AffirmationMonitor struct {
	store    pendingGiveUpLister
	timeout  time.Duration
	interval time.Duration
	alerter  BreakAlerter
	clock    func() time.Time
	// seen guards one-alert-per-trade without a schema change: a trade that
	// has already alerted is not re-alerted on the next sweep (Phase-24's
	// break workflow owns escalation cadence).
	seen map[int64]struct{}
}

// NewAffirmationMonitor builds the monitor. interval is the sweep cadence
// (callers pass the scanner tick, e.g. 5s); timeout defaults to 60s.
func NewAffirmationMonitor(store pendingGiveUpLister, timeout, interval time.Duration, alerter BreakAlerter) *AffirmationMonitor {
	if timeout <= 0 {
		timeout = DefaultAffirmationTimeout
	}
	if interval <= 0 {
		interval = 5 * time.Second
	}
	if alerter == nil {
		alerter = LogAlerter{Log: slog.Default()}
	}
	return &AffirmationMonitor{
		store: store, timeout: timeout, interval: interval,
		alerter: alerter, clock: time.Now, seen: map[int64]struct{}{},
	}
}

// SetClockForTest overrides the clock — tests only.
func (m *AffirmationMonitor) SetClockForTest(now func() time.Time) { m.clock = now }

// ScanOnce performs one sweep — the unit-testable core of the monitor.
func (m *AffirmationMonitor) ScanOnce(ctx context.Context) (int, error) {
	pend, err := m.store.PendingOlderThan(ctx, m.clock().Add(-m.timeout))
	if err != nil {
		return 0, err
	}
	alerts := 0
	var errs []error
	for _, g := range pend {
		if _, dup := m.seen[g.ID]; dup {
			continue
		}
		if err := m.alerter.AlertBreak(ctx, GiveUpBreak{Trade: g, Age: m.clock().Sub(g.CreatedAt)}); err != nil {
			errs = append(errs, fmt.Errorf("fix: break alert give-up %d: %w", g.ID, err))
			continue // retry next sweep — never mark seen on failure
		}
		m.seen[g.ID] = struct{}{}
		alerts++
	}
	return alerts, errors.Join(errs...)
}

// Run sweeps on the interval until ctx is cancelled.
func (m *AffirmationMonitor) Run(ctx context.Context) {
	t := time.NewTicker(m.interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if _, err := m.ScanOnce(ctx); err != nil {
				slog.Default().WarnContext(ctx, "pb affirmation sweep", "err", err)
			}
		}
	}
}
