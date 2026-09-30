package reporting

import (
	"context"
	"encoding/json"
	"time"
)

// ---------------------------------------------------------------------------
// Store seam — PgStore implements it; unit tests fake it.
// ---------------------------------------------------------------------------

// PositionSnapshot is the internal open-derivative state the reconciler
// compares against accepted repository state (spec §14.1a).
type PositionSnapshot struct {
	PositionID     int64     `json:"position_id"`
	AccountID      int64     `json:"account_id"`
	InstrumentID   int64     `json:"instrument_id"`
	Side           string    `json:"side"`
	Quantity       string    `json:"quantity"`
	MarkPrice      string    `json:"mark_price"`
	MarginUsed     string    `json:"margin_used"`
	InstrumentCode string    `json:"instrument_code"`
	UpdatedAt      time.Time `json:"updated_at"`
}

// CFTCLimit is one active cftc_position_limits row.
type CFTCLimit struct {
	LimitID              int64      `json:"limit_id"`
	InstrumentID         *int64     `json:"instrument_id,omitempty"`
	InstrumentType       string     `json:"instrument_type"`
	CurrencyPair         string     `json:"currency_pair,omitempty"`
	SpotMonthLimit       *string    `json:"spot_month_limit,omitempty"`
	AllMonthsLimit       *string    `json:"all_months_limit,omitempty"`
	LargeTraderThreshold string     `json:"large_trader_threshold"`
	Currency             string     `json:"currency"`
	EffectiveFrom        time.Time  `json:"effective_from"`
	EffectiveTo          *time.Time `json:"effective_to,omitempty"`
}

// Store is the persistence seam for the canonical reporting model.
// Mutations carry their audit_hash_chain link inside the same
// transaction (audit.Append nil-payload convention); read methods are
// plain queries. All implementers must preserve append-only semantics
// for events/acks/log rows.
type Store interface {
	// --- source resolution ---
	ResolveTrade(ctx context.Context, tradeID int64) (*TradeContext, error)
	PartyFor(ctx context.Context, accountID int64) (*PartyIdentifiers, error)
	// UpsertParty records/repairs the regulatory identifiers for an
	// account (Compliance Officer repair path — LEI checksum-validated
	// by the service before the write). Returns the stored row.
	UpsertParty(ctx context.Context, p PartyIdentifiers, adminUserID int64, clientIP string) error

	// --- events ---
	// InsertEvent appends one immutable event row + chain link.
	InsertEvent(ctx context.Context, e *Event) error
	EventByID(ctx context.Context, eventID int64) (*Event, error)
	// LatestEvent returns the highest report_seq row for (uti, regime),
	// or nil — lifecycle events chain off it.
	LatestEvent(ctx context.Context, uti string, regime Regime) (*Event, error)
	// NextReportSeq = LatestEvent.report_seq+1 (1 when no prior row).
	NextReportSeq(ctx context.Context, uti string, regime Regime) (int, error)
	// UTIOwner returns the trade_id the uti was first minted for —
	// collision-reject compares it against the incoming trade (a
	// different trade_id on the same UTI is an ID_COLLISION).
	UTIOwner(ctx context.Context, uti string) (tradeID int64, found bool, err error)
	// SetEventStatus moves the lifecycle state machine; errs are the
	// pinned-rule validation misses recorded on QUARANTINED.
	SetEventStatus(ctx context.Context, eventID int64, status EventStatus, validationErrors json.RawMessage) error
	// ExportEvents is the report export feed (from/to on event_ts).
	ExportEvents(ctx context.Context, regime Regime, from, to *time.Time, limit int) ([]Event, error)
	// LatestEvents returns the newest event row per uti for the regime
	// (any action/status) — the reconciler's repository-side view.
	LatestEvents(ctx context.Context, regime Regime) ([]Event, error)
	// DuplicateNEWT returns utis carrying >1 live NEWT row in the
	// regime (DUPLICATE break feed).
	DuplicateNEWT(ctx context.Context, regime Regime) ([]string, error)
	// CollidingUTIs returns utis bound to >1 distinct trade_id
	// (ID_COLLISION sweep).
	CollidingUTIs(ctx context.Context) ([]string, error)

	// --- artifact journal ---
	InsertSubmission(ctx context.Context, s *Submission) error
	SubmissionByID(ctx context.Context, reportSubmissionID int64) (*Submission, error)
	SubmissionsForEvent(ctx context.Context, eventID int64) ([]Submission, error)
	// PendingSubmissions feeds the dispatcher — status PENDING and
	// event VALIDATED, oldest first.
	PendingSubmissions(ctx context.Context, limit int) ([]Submission, error)
	// MarkSubmissionDispatched transitions PENDING→SUBMITTED (wire
	// attempt logged in regulatory_submissions by the dispatcher).
	MarkSubmissionDispatched(ctx context.Context, reportSubmissionID int64, at time.Time) error
	// IngestAckTx lands the ack row + submission transition + event
	// transition + (NACK) break row in one serializable transaction.
	IngestAckTx(ctx context.Context, a Ack, submissionStatus SubmissionStatus,
		eventStatus EventStatus, nackBreak bool, slaDueAt time.Time) (*Break, error)
	// AcksForEvent lists the immutable ack log for one event (detail
	// view — repository feedback history).
	AcksForEvent(ctx context.Context, eventID int64) ([]Ack, error)

	// --- breaks ---
	InsertBreak(ctx context.Context, b *Break) error
	OpenBreaks(ctx context.Context, regime Regime, limit int) ([]Break, error)
	// ResolveBreakTx marks OPEN/REPAIRING → RESOLVED (or WONT_FIX with
	// notes) + admin audit row. Idempotent on already-resolved rows.
	ResolveBreakTx(ctx context.Context, breakID int64, status BreakStatus,
		resolvedBy int64, notes string, at time.Time, clientIP string) (bool, error)

	// --- schema registry ---
	// ActiveSchema returns the active, currently-effective pinned ruleset
	// for (regulation, schemaName) — nil when none is registered.
	ActiveSchema(ctx context.Context, regulation, schemaName string, at time.Time) (*SchemaVersion, error)

	// --- reconciliation reads ---
	// OpenDerivativePositions is the internal side of the daily
	// repo-vs-internal reconciliation.
	OpenDerivativePositions(ctx context.Context) ([]PositionSnapshot, error)

	// --- CFTC limits (Task 21.3.9) ---
	// ActiveLimitFor resolves the in-force limit row for an instrument
	// (instrument-specific row wins over the instrument_type default).
	ActiveLimitFor(ctx context.Context, instrumentID int64, instrumentType string, pair string, at time.Time) (*CFTCLimit, error)
	// OpenQuantityFor sums the account's open quantity on an instrument.
	OpenQuantityFor(ctx context.Context, accountID, instrumentID int64) (string, error)
}
