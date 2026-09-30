package reporting

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// ---------------------------------------------------------------------------
// Email channel (Task 20.3.8 item 2b)
//
// The notifications package Message carries no attachment field and its
// channels are preference-driven — trade confirmations are regulatory
// artifacts with a mandatory PDF attachment, so this package owns a
// dedicated EmailSender seam. Production binds SMTP/SES behind it
// (config seam: EXC_CONFIRM_EMAIL_* env in the orchestrator); dev uses
// Log/File, tests use Mem.
// ---------------------------------------------------------------------------

// Email is one outbound confirmation message with its document
// attachment.
type Email struct {
	To             string
	Subject        string
	Body           string // plain text
	BodyHTML       string // jurisdiction-rendered HTML (optional)
	AttachmentName string // e.g. "confirmation-42-v1.pdf"
	Attachment     []byte
	ContentType    string // attachment MIME; default application/pdf
}

// EmailSender delivers one Email. Implementations MUST NOT retain the
// attachment bytes beyond SendEmail's return.
type EmailSender interface {
	SendEmail(ctx context.Context, m Email) error
}

// LogEmailSender logs each send via slog (attachment bytes are never
// logged — size + name only).
type LogEmailSender struct{ Log *slog.Logger }

// SendEmail implements EmailSender.
func (s LogEmailSender) SendEmail(_ context.Context, m Email) error {
	l := s.Log
	if l == nil {
		l = slog.Default()
	}
	l.Info("confirmation email", "to", maskTo(m.To), "subject", m.Subject,
		"attachment", m.AttachmentName, "attachment_bytes", len(m.Attachment))
	return nil
}

func maskTo(to string) string {
	if i := strings.LastIndex(to, "@"); i > 0 {
		return "***@" + to[i+1:]
	}
	return "***"
}

// FileEmailSender appends each send as a JSON line (attachment recorded
// as name+size only — a dev mailbox must not hoard document bodies).
type FileEmailSender struct {
	Dir string
	mu  sync.Mutex
}

// SendEmail implements EmailSender.
func (s *FileEmailSender) SendEmail(_ context.Context, m Email) error {
	rec := map[string]any{
		"to": m.To, "subject": m.Subject, "body": m.Body,
		"attachment_name":  m.AttachmentName,
		"attachment_bytes": len(m.Attachment),
		"sent_at":          time.Now().UTC().Format(time.RFC3339Nano),
	}
	b, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := os.MkdirAll(s.Dir, 0o750); err != nil {
		return err
	}
	f, err := os.OpenFile(filepath.Join(s.Dir, "confirmations.jsonl"),
		os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o640)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	_, err = f.Write(append(b, '\n'))
	return err
}

// MemEmailSender captures sends for tests; FailErr forces failures.
type MemEmailSender struct {
	mu      sync.Mutex
	Sent    []Email
	FailErr error
}

// SendEmail implements EmailSender.
func (s *MemEmailSender) SendEmail(_ context.Context, m Email) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.FailErr != nil {
		return s.FailErr
	}
	s.Sent = append(s.Sent, m)
	return nil
}

// Messages returns a copy of captured sends.
func (s *MemEmailSender) Messages() []Email {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Email, len(s.Sent))
	copy(out, s.Sent)
	return out
}

// ---------------------------------------------------------------------------
// Delivery — the multi-channel dispatcher
// ---------------------------------------------------------------------------

// RecipientSource resolves the account's email address. PG impl binds
// accounts ⨝ users.email (store_pg.go).
type RecipientSource interface {
	Email(ctx context.Context, accountID int64) (string, error)
}

// CategorySource resolves accounts.client_category (migration 042).
// A missing account resolves to RETAIL — the most conservative timing.
type CategorySource interface {
	Category(ctx context.Context, accountID int64) (string, error)
}

// DeadLetterSink is called when a confirmation exhausts its dispatch
// attempts — mirrors the notifications dead-letter pattern (the store
// row stays GENERATED for ops replay; the sink is the alerting seam).
type DeadLetterSink interface {
	DeadLetter(ctx context.Context, rec ConfirmationRecord, attempts int, err error)
}

// LogDeadLetter is the default sink.
type LogDeadLetter struct{ Log *slog.Logger }

// DeadLetter implements DeadLetterSink.
func (l LogDeadLetter) DeadLetter(_ context.Context, rec ConfirmationRecord, attempts int, err error) {
	lg := l.Log
	if lg == nil {
		lg = slog.Default()
	}
	lg.Error("confirmation dispatch dead-lettered",
		"confirmation_id", rec.ConfirmationID, "trade_id", rec.TradeID,
		"account_id", rec.AccountID, "attempts", attempts, "err", err)
}

// Delivery fans one GENERATED confirmation out over the configured
// channels: portal (MarkDelivered is the portal-publish step — the
// file_ref makes the doc retrievable via the endpoint), email with the
// PDF attachment, and MT515 for institutional categories.
type Delivery struct {
	Tracker    ConfirmationTracker
	Docs       DocumentStore   // archived "<ref>.pdf" / "<ref>.json"
	Email      EmailSender     // nil → email channel skipped
	Recipients RecipientSource // required when Email != nil
	Categories CategorySource  // MT515 gate + email jurisdiction context
	// Jurisdiction maps account → template key (EU default). Nil → EU.
	Jurisdiction func(ctx context.Context, accountID int64) (string, error)
	// Disclosures overrides per-jurisdiction regulatory text.
	Disclosures map[string]string
	MT515       MT515Submitter // nil → MT515 channel skipped
	Now         func() time.Time
	Logf        func(string, ...any)

	mu       sync.Mutex
	attempts map[int64]int // confirmation_id → dispatch attempts (in-process)
}

// MaxDispatchAttempts bounds sweep retries before dead-lettering.
const MaxDispatchAttempts = 5

func (d *Delivery) log(format string, args ...any) {
	if d.Logf != nil {
		d.Logf(format, args...)
	}
}

func (d *Delivery) now() time.Time {
	if d.Now != nil {
		return d.Now().UTC()
	}
	return time.Now().UTC()
}

// Deliver dispatches one confirmation record and marks it DELIVERED.
// Any channel failure returns an error — the row stays GENERATED and
// the caller (immediate path / T+1 sweep) retries with its attempt
// bookkeeping.
func (d *Delivery) Deliver(ctx context.Context, rec ConfirmationRecord) error {
	var pdf []byte
	var doc *confirmationJSON
	if d.Docs != nil && rec.FileRef != "" {
		body, err := d.Docs.Get(ctx, rec.FileRef+".pdf")
		if err != nil {
			return fmt.Errorf("reporting: fetch %q.pdf: %w", rec.FileRef, err)
		}
		pdf = body
		if jbody, err := d.Docs.Get(ctx, rec.FileRef+".json"); err == nil {
			if parsed, perr := parseConfirmationJSON(jbody); perr == nil {
				doc = parsed
			}
		}
	}

	cat := "RETAIL"
	if d.Categories != nil {
		c, err := d.Categories.Category(ctx, rec.AccountID)
		if err != nil {
			return fmt.Errorf("reporting: category acct %d: %w", rec.AccountID, err)
		}
		if c != "" {
			cat = strings.ToUpper(c)
		}
	}

	if d.Email != nil && d.Recipients != nil {
		to, err := d.Recipients.Email(ctx, rec.AccountID)
		if err != nil {
			return fmt.Errorf("reporting: recipient acct %d: %w", rec.AccountID, err)
		}
		if to != "" {
			em := d.emailFor(rec, doc, pdf, to)
			if err := d.Email.SendEmail(ctx, em); err != nil {
				return fmt.Errorf("reporting: email confirmation %d: %w",
					rec.ConfirmationID, err)
			}
		}
	}
	if d.MT515 != nil && isInstitutional(cat) {
		if err := d.MT515.SubmitMT515(ctx, rec, doc); err != nil {
			return fmt.Errorf("reporting: MT515 confirmation %d: %w",
				rec.ConfirmationID, err)
		}
	}
	if err := d.Tracker.MarkDelivered(ctx, rec.ConfirmationID, d.now()); err != nil {
		return fmt.Errorf("reporting: mark delivered %d: %w", rec.ConfirmationID, err)
	}
	return nil
}

// emailFor builds the Email — HTML body is the jurisdiction-rendered
// confirmation (falls back to a text summary when the stored JSON is
// absent or unparsable).
func (d *Delivery) emailFor(rec ConfirmationRecord, doc *confirmationJSON, pdf []byte, to string) Email {
	em := Email{
		To:             to,
		Subject:        fmt.Sprintf("Trade confirmation — trade %d (v%d)", rec.TradeID, rec.Version),
		AttachmentName: fmt.Sprintf("confirmation-%d-v%d.pdf", rec.TradeID, rec.Version),
		Attachment:     pdf,
		ContentType:    "application/pdf",
	}
	em.Body = fmt.Sprintf(
		"Please find attached the trade confirmation for trade %d (version %d, generated %s).\n"+
			"The document is also available in your account portal under Reports → Confirmations.",
		rec.TradeID, rec.Version, rec.GeneratedAt.UTC().Format(time.RFC3339))
	if doc != nil {
		jur := ""
		if d.Jurisdiction != nil {
			if j, err := d.Jurisdiction(context.Background(), rec.AccountID); err == nil {
				jur = j
			}
		}
		disc := ""
		if d.Disclosures != nil {
			disc = d.Disclosures[strings.ToUpper(jur)]
		}
		if html, err := RenderConfirmationHTML(rec, doc, strings.ToUpper(jur), disc); err == nil {
			em.BodyHTML = string(html)
		}
	}
	return em
}

func isInstitutional(cat string) bool {
	c := strings.ToUpper(cat)
	return c == "PROFESSIONAL" || c == "ELIGIBLE_COUNTERPARTY"
}

// bumpAttempt records one failed dispatch and reports the count.
func (d *Delivery) bumpAttempt(id int64) int {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.attempts == nil {
		d.attempts = map[int64]int{}
	}
	d.attempts[id]++
	return d.attempts[id]
}

func (d *Delivery) clearAttempt(id int64) {
	d.mu.Lock()
	defer d.mu.Unlock()
	delete(d.attempts, id)
}

// ---------------------------------------------------------------------------
// DeliveryScheduler — MiFID II Art. 25(6) T+1 retail dispatch
// ---------------------------------------------------------------------------

// DispatchDue reports whether a confirmation for the given client
// category is due at `at` relative to its generation timestamp:
// institutional categories dispatch immediately; RETAIL dispatches at
// the start of the next business day after the generation day (T+1 —
// the Art. 25(6) outer bound, honoured at day start rather than end).
func DispatchDue(clientCategory string, generatedAt, at time.Time) bool {
	switch strings.ToUpper(clientCategory) {
	case "PROFESSIONAL", "ELIGIBLE_COUNTERPARTY":
		return !at.Before(generatedAt)
	default: // RETAIL + unknown → conservative T+1
		return !at.Before(NextBusinessDay(generatedAt))
	}
}

// NextBusinessDay returns the start (00:00 UTC) of the next weekday
// after t's UTC calendar day. Currency-holiday refinement is the
// IsBusinessDay seam on DeliveryScheduler.
func NextBusinessDay(t time.Time) time.Time {
	d := t.UTC().Truncate(24 * time.Hour).Add(24 * time.Hour)
	for d.Weekday() == time.Saturday || d.Weekday() == time.Sunday {
		d = d.Add(24 * time.Hour)
	}
	return d
}

// DeliveryScheduler sweeps GENERATED confirmations and dispatches the
// ones whose category-specific due time has passed (retail T+1; a still-
// GENERATED institutional row — e.g. a failed immediate dispatch — is
// due now). Run it on a short cadence (the orchestrator binds Run).
type DeliveryScheduler struct {
	Tracker    ConfirmationTracker
	Deliver    *Delivery
	Categories CategorySource
	// IsBusinessDay optionally refines the due-date computation
	// (currency-holiday calendar — Phase-03 settlement.HolidayCalendar
	// adapter). Nil → weekday-only.
	IsBusinessDay func(time.Time) bool
	Batch         int
	Now           func() time.Time
	Logf          func(string, ...any)
	DeadLetters   DeadLetterSink
}

// NewDeliveryScheduler wires the sweep with sane defaults.
func NewDeliveryScheduler(t ConfirmationTracker, d *Delivery, cats CategorySource) *DeliveryScheduler {
	return &DeliveryScheduler{
		Tracker: t, Deliver: d, Categories: cats,
		Batch: 200, Now: time.Now, DeadLetters: LogDeadLetter{},
	}
}

func (s *DeliveryScheduler) log(format string, args ...any) {
	if s.Logf != nil {
		s.Logf(format, args...)
	}
}

func (s *DeliveryScheduler) now() time.Time {
	if s.Now != nil {
		return s.Now().UTC()
	}
	return time.Now().UTC()
}

// dueAt computes the dispatch deadline for a record given its category.
func (s *DeliveryScheduler) dueAt(cat string, generated time.Time) time.Time {
	if strings.EqualFold(cat, "PROFESSIONAL") || strings.EqualFold(cat, "ELIGIBLE_COUNTERPARTY") {
		return generated
	}
	if s.IsBusinessDay == nil {
		return NextBusinessDay(generated)
	}
	d := generated.UTC().Truncate(24 * time.Hour).Add(24 * time.Hour)
	for !s.IsBusinessDay(d) {
		d = d.Add(24 * time.Hour)
	}
	return d
}

// RunDue processes up to Batch generated rows; returns the dispatch
// count. A row whose category lookup fails is treated RETAIL
// (conservative — later, never earlier). Dispatch failures bump the
// attempt counter; after MaxDispatchAttempts the row dead-letters via
// DeadLetters and stays GENERATED for ops replay.
func (s *DeliveryScheduler) RunDue(ctx context.Context) (int, error) {
	if s.Tracker == nil || s.Deliver == nil {
		return 0, fmt.Errorf("reporting: scheduler requires tracker+delivery")
	}
	var now = s.now()
	limit := s.Batch
	if limit <= 0 {
		limit = 200
	}
	rows, err := s.Tracker.PendingGenerated(ctx, limit)
	if err != nil {
		return 0, fmt.Errorf("reporting: pending confirmations: %w", err)
	}
	sent := 0
	for _, rec := range rows {
		cat := "RETAIL"
		if s.Categories != nil {
			c, cerr := s.Categories.Category(ctx, rec.AccountID)
			if cerr != nil {
				s.log("reporting: category lookup acct %d failed (%v) — treating as RETAIL",
					rec.AccountID, cerr)
			} else if c != "" {
				cat = strings.ToUpper(c)
			}
		}
		if now.Before(s.dueAt(cat, rec.GeneratedAt)) {
			continue // not yet due — retail T+1
		}
		if err := s.Deliver.Deliver(ctx, rec); err != nil {
			n := s.Deliver.bumpAttempt(rec.ConfirmationID)
			if n >= MaxDispatchAttempts {
				sink := s.DeadLetters
				if sink == nil {
					sink = LogDeadLetter{}
				}
				sink.DeadLetter(ctx, rec, n, err)
				s.Deliver.clearAttempt(rec.ConfirmationID)
			}
			s.log("reporting: dispatch confirmation %d attempt %d failed: %v",
				rec.ConfirmationID, n, err)
			continue
		}
		s.Deliver.clearAttempt(rec.ConfirmationID)
		sent++
	}
	return sent, nil
}

// Run loops RunDue on the given cadence until ctx cancels.
func (s *DeliveryScheduler) Run(ctx context.Context, cadence time.Duration) {
	if cadence <= 0 {
		cadence = time.Minute
	}
	t := time.NewTicker(cadence)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if _, err := s.RunDue(ctx); err != nil {
				s.log("reporting: due sweep failed: %v", err)
			}
		}
	}
}
