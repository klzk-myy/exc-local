// Package reporting implements the Phase-20 Task 20.3.8 delivery engine
// — client reporting portal backing + multi-channel trade-confirmation
// delivery (MiFID II Art. 25, spec §8.4 / §24 traceability).
//
// Ownership split (coordinated with the analytics sibling):
//
//   - internal/analytics OWNS generation: ConfirmationService.Generate /
//     MarkAdjusted render the contract note (JSON + PDF), archive it
//     under file_ref stems "confirmations/{acct}/{trade}/v{n}" and write
//     trade_confirmations rows (status GENERATED, version, content_sha256,
//     supersedes_id — migration 049 shape).
//   - THIS package owns delivery: the fill-trigger adapter
//     (ConfirmationService.OnFill / ConfirmationConsumer on the JetStream
//     `trades` stream) invokes generation inside the ≤60s
//     contract-note window, then dispatches GENERATED rows over the
//     configured channels — secure portal (status DELIVERED + file_ref
//     backing GET /api/v1/account/confirmations/{trade_id}), email with
//     the PDF attachment (EmailSender seam — the notifications package
//     carries no attachment field, so confirmations own a dedicated
//     channel), and optional SWIFT MT515 for institutional categories
//     (PHASE-24 SEAM — LogMT515 only until Phase-24 binds the network).
//
// Timing is regulatory: RETAIL dispatches no later than the first
// business day after execution (Art. 25(6) — DeliveryScheduler sweeps
// GENERATED rows); PROFESSIONAL / ELIGIBLE_COUNTERPARTY dispatch
// immediately on the fill path. Dispatch failures retry through the
// sweep with a per-row attempt counter, dead-lettering after
// MaxDispatchAttempts via the DeadLetterSink.
//
// Amendments (trade bust / price-adjust, spec §5.29): the generator
// issues a new version under a NEW file_ref (the sent object is never
// mutated) and the new GENERATED row dispatches immediately regardless
// of client category.
package reporting

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"html/template"
	"sync/atomic"
	"time"
)

// GenerationSLA bounds trade-event → confirmation-generation latency
// (the orchestrator alerts on M.SLAViolations; MiFID contract-note
// window).
const GenerationSLA = 60 * time.Second

// Jurisdiction template keys — embedded under templates/.
const (
	JurisdictionEU = "EU" // MiFID II (default)
	JurisdictionUK = "UK" // FCA COBS 16 post-Brexit variant
	JurisdictionUS = "US" // CFTC/FinCEN retail-forex wording
)

// Confirmation statuses — the migration-049 CHECK constraint vocabulary
// (the sibling generation path owns GENERATED; delivery flips to
// DELIVERED; adjustments mark the superseded rows ADJUSTED).
const (
	StatusGenerated = "GENERATED"
	StatusDelivered = "DELIVERED"
	StatusAdjusted  = "ADJUSTED"
	StatusFailed    = "FAILED"
)

// FillNotice is the fill-event contract for the delivery engine — the
// trade id is all generation needs (the sibling service joins trades ⨝
// instruments for the document); ExecutedAt feeds the SLA timer.
type FillNotice struct {
	TradeID    int64
	ExecutedAt time.Time // engine event timestamp
}

// ConfirmationRecord mirrors the sibling-owned trade_confirmations row.
// Field set per migration 049: confirmation_id, trade_id, account_id,
// version, status, file_ref (extension-less key stem — "<ref>.pdf" and
// "<ref>.json" both exist), content_sha256, generated_at, delivered_at,
// supersedes_id.
type ConfirmationRecord struct {
	ConfirmationID int64
	TradeID        int64
	AccountID      int64
	Version        int
	Status         string
	FileRef        string
	ContentSHA256  string
	GeneratedAt    time.Time
	DeliveredAt    *time.Time
	SupersedesID   *int64
}

// ConfirmationGenerator is the sibling generation seam — bound to
// analytics.ConfirmationService by a thin adapter in the orchestrator
// (that package's row type differs from this mirror, so the adapter
// converts; keeping the iface local avoids importing a churning sibling).
type ConfirmationGenerator interface {
	// Generate issues the v1 contract notes for both legs of a trade.
	Generate(ctx context.Context, tradeID int64) ([]ConfirmationRecord, error)
	// MarkAdjusted voids standing rows (ADJUSTED) and issues version+1
	// notes — returns the newly GENERATED records.
	MarkAdjusted(ctx context.Context, tradeID int64) ([]ConfirmationRecord, error)
}

// ConfirmationTracker is the delivery-side read/write seam over
// trade_confirmations (PG impl in store_pg.go; MemConfirmationStore for
// tests).
type ConfirmationTracker interface {
	// PendingGenerated lists GENERATED rows oldest-first — the T+1 sweep
	// input. (New adjusted issues also land GENERATED, so corrections
	// ride the same sweep.)
	PendingGenerated(ctx context.Context, limit int) ([]ConfirmationRecord, error)
	// MarkDelivered stamps delivered_at and flips GENERATED→DELIVERED
	// (the sibling's MarkDelivered semantic — portal fetch alone does
	// NOT count as delivery; the engine owns that transition).
	MarkDelivered(ctx context.Context, confirmationID int64, at time.Time) error
	// LatestByTrade returns the newest row for a trade regardless of
	// account (handler ownership check happens above this).
	LatestByTrade(ctx context.Context, tradeID int64) (*ConfirmationRecord, error)
}

// DocumentStore fetches archived confirmation objects by file_ref —
// satisfied by analytics.S3FileStore / MemFileStore (structural match)
// or any objectstore adapter the orchestrator binds.
type DocumentStore interface {
	Get(ctx context.Context, ref string) ([]byte, error)
}

// Metrics is the plain-counter telemetry surface.
type Metrics struct {
	Generated     atomic.Int64 // fills that triggered generation
	Adjusted      atomic.Int64 // amendment replays
	Delivered     atomic.Int64
	SLAViolations atomic.Int64
	Errors        atomic.Int64
	EmailFailures atomic.Int64
	DeadLettered  atomic.Int64
}

// ConfirmationService is the delivery engine: OnFill → Generate →
// dispatch; amendments → MarkAdjusted → immediate dispatch.
type ConfirmationService struct {
	gen   ConfirmationGenerator
	track ConfirmationTracker
	deliv *Delivery
	cats  CategorySource
	now   func() time.Time
	logf  func(string, ...any)
	M     Metrics
}

// ServiceOptions wires ConfirmationService. Generator + Tracker are
// mandatory (fail closed — a delivery engine that cannot generate or
// track must never start); Delivery may be nil (records stay GENERATED
// for the sweep).
type ServiceOptions struct {
	Generator  ConfirmationGenerator
	Tracker    ConfirmationTracker
	Delivery   *Delivery
	Categories CategorySource
	Now        func() time.Time
	Logf       func(string, ...any)
}

// NewConfirmationService wires the engine.
func NewConfirmationService(o ServiceOptions) (*ConfirmationService, error) {
	if o.Generator == nil {
		return nil, fmt.Errorf("reporting: confirmation generator required")
	}
	if o.Tracker == nil {
		return nil, fmt.Errorf("reporting: confirmation tracker required")
	}
	now := o.Now
	if now == nil {
		now = time.Now
	}
	return &ConfirmationService{
		gen: o.Generator, track: o.Tracker, deliv: o.Delivery,
		cats: o.Categories, now: now, logf: o.Logf,
	}, nil
}

func (s *ConfirmationService) log(format string, args ...any) {
	if s.logf != nil {
		s.logf(format, args...)
	}
}

// OnFill triggers contract-note generation for a fill and dispatches
// each leg's record per its client-category timing. Idempotent — the
// generator's own version bookkeeping absorbs JetStream redelivery.
func (s *ConfirmationService) OnFill(ctx context.Context, f FillNotice) ([]ConfirmationRecord, error) {
	recs, err := s.gen.Generate(ctx, f.TradeID)
	if err != nil {
		s.M.Errors.Add(1)
		return nil, fmt.Errorf("reporting: generate confirmations trade %d: %w", f.TradeID, err)
	}
	s.M.Generated.Add(1)
	now := s.now().UTC()
	if !f.ExecutedAt.IsZero() && now.Sub(f.ExecutedAt) > GenerationSLA {
		s.M.SLAViolations.Add(1)
		s.log("reporting: confirmation generation exceeded %s SLA (trade %d filled %s)",
			GenerationSLA, f.TradeID, f.ExecutedAt.Format(time.RFC3339Nano))
	}
	for _, rec := range recs {
		s.maybeDispatch(ctx, rec, now)
	}
	return recs, nil
}

// OnTradeAmended regenerates notes after a post-trade event (spec §5.29
// bust/price-adjust) and dispatches the new versions immediately — a
// correction outranks the retail T+1 window.
func (s *ConfirmationService) OnTradeAmended(ctx context.Context, tradeID int64) ([]ConfirmationRecord, error) {
	recs, err := s.gen.MarkAdjusted(ctx, tradeID)
	if err != nil {
		s.M.Errors.Add(1)
		return nil, fmt.Errorf("reporting: adjust confirmations trade %d: %w", tradeID, err)
	}
	s.M.Adjusted.Add(1)
	for _, rec := range recs {
		if s.deliv == nil {
			continue
		}
		if err := s.deliv.Deliver(ctx, rec); err != nil {
			s.M.EmailFailures.Add(1)
			s.log("reporting: amended dispatch failed confirmation %d: %v",
				rec.ConfirmationID, err)
		} else {
			s.M.Delivered.Add(1)
		}
	}
	return recs, nil
}

// maybeDispatch delivers immediately when the record's category is due
// now (institutional categories; RETAIL waits for the T+1 sweep).
func (s *ConfirmationService) maybeDispatch(ctx context.Context, rec ConfirmationRecord, now time.Time) {
	if s.deliv == nil {
		return
	}
	cat := "RETAIL"
	if s.cats != nil {
		if c, err := s.cats.Category(ctx, rec.AccountID); err == nil && c != "" {
			cat = c
		}
	}
	if !DispatchDue(cat, rec.GeneratedAt, now) {
		return
	}
	if err := s.deliv.Deliver(ctx, rec); err != nil {
		// Dispatch failure never rolls back generation — the row stays
		// GENERATED and the scheduler sweep retries.
		s.M.EmailFailures.Add(1)
		s.log("reporting: immediate dispatch failed confirmation %d: %v",
			rec.ConfirmationID, err)
		return
	}
	s.M.Delivered.Add(1)
}

// ---------------------------------------------------------------------------
// Stored-document parsing + jurisdiction rendering
// ---------------------------------------------------------------------------

// confirmationJSON is the documented stored-document shape the sibling's
// renderConfirmationJSON emits ("<file_ref>.json"). The struct is a
// tolerant projection — unknown fields ignored.
type confirmationJSON struct {
	Type           string `json:"type"`
	Version        int    `json:"version"`
	Adjusted       bool   `json:"adjusted"`
	TradeID        int64  `json:"trade_id"`
	AccountID      int64  `json:"account_id"`
	Symbol         string `json:"symbol"`
	Side           string `json:"side"`
	Price          string `json:"price"`
	Quantity       string `json:"quantity"`
	Fee            string `json:"fee"`
	FeeCurrency    string `json:"fee_currency"`
	SettlementDate string `json:"settlement_date"`
	ExecutedAt     string `json:"executed_at"`
	GeneratedAt    string `json:"generated_at"`
	Venue          string `json:"venue"`
}

func parseConfirmationJSON(body []byte) (*confirmationJSON, error) {
	var d confirmationJSON
	if err := json.Unmarshal(body, &d); err != nil {
		return nil, fmt.Errorf("reporting: parse confirmation doc: %w", err)
	}
	if d.Type != "trade_confirmation" {
		return nil, fmt.Errorf("reporting: unexpected doc type %q", d.Type)
	}
	return &d, nil
}

// ConfirmationDoc is the template payload for the jurisdiction-rendered
// HTML (email body / portal preview).
type ConfirmationDoc struct {
	ConfirmationID int64
	Version        int
	Status         string
	Adjusted       bool
	Venue          string
	Jurisdiction   string
	Disclosure     string
	GeneratedAt    time.Time
	TradeID        int64
	AccountID      int64
	Symbol         string
	Side           string
	Quantity       string
	Price          string
	Fee            string
	FeeCurrency    string
	SettlementDate string
	ExecutedAt     string
}

// RenderConfirmationHTML renders the jurisdiction template for a stored
// confirmation — used for the HTML email body. An unknown jurisdiction
// falls back to EU wording rather than failing.
func RenderConfirmationHTML(rec ConfirmationRecord, doc *confirmationJSON, jurisdiction, disclosure string) ([]byte, error) {
	d := ConfirmationDoc{
		ConfirmationID: rec.ConfirmationID, Version: rec.Version,
		Status: rec.Status, Adjusted: doc.Adjusted,
		Venue: doc.Venue, Jurisdiction: jurisdiction,
		Disclosure: disclosure, GeneratedAt: rec.GeneratedAt,
		TradeID: doc.TradeID, AccountID: doc.AccountID,
		Symbol: doc.Symbol, Side: doc.Side,
		Quantity: doc.Quantity, Price: doc.Price,
		Fee: doc.Fee, FeeCurrency: doc.FeeCurrency,
		SettlementDate: doc.SettlementDate, ExecutedAt: doc.ExecutedAt,
	}
	if d.Venue == "" {
		d.Venue = "EXCHANGE"
	}
	if d.Jurisdiction == "" {
		d.Jurisdiction = JurisdictionEU
	}
	if d.Disclosure == "" {
		d.Disclosure = defaultDisclosure(d.Jurisdiction)
	}
	name := "confirmation_" + d.Jurisdiction + ".html.tmpl"
	t := templates.Lookup(name)
	if t == nil {
		t = templates.Lookup("confirmation_EU.html.tmpl")
	}
	var b bytes.Buffer
	if err := t.Execute(&b, d); err != nil {
		return nil, fmt.Errorf("reporting: render %s: %w", name, err)
	}
	return b.Bytes(), nil
}

func defaultDisclosure(jur string) string {
	switch jur {
	case JurisdictionUK:
		return "This confirmation is issued under the FCA Conduct of Business " +
			"Sourcebook (COBS 16). It is a record of an executed transaction " +
			"and does not constitute investment advice."
	case JurisdictionUS:
		return "This confirmation is furnished pursuant to CFTC/FinCEN " +
			"record-keeping obligations for retail forex transactions. " +
			"It is a record of an executed transaction only."
	default:
		return "This confirmation is provided in accordance with MiFID II " +
			"Article 25(6) and the firm's order execution policy. It is a " +
			"record of an executed transaction and not investment advice."
	}
}

var templates = template.Must(template.New("root").Funcs(template.FuncMap{
	"fmtTime": func(t time.Time) string { return t.UTC().Format("2006-01-02 15:04:05.000 UTC") },
	"fmtDate": func(t time.Time) string { return t.UTC().Format("2006-01-02") },
}).ParseFS(templateFS, "templates/*.tmpl"))
