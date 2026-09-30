// Package reporting implements the Phase-21 Task 21.3.14 canonical
// regulatory reporting event store — the shared EMIR REFIT / CFTC
// Parts 43/45 / MiFID II lifecycle infrastructure (spec §5.32, §14.1a;
// §24 #169/#170) that the regime adapters (Tasks 21.3.4 MiFID II,
// 21.3.5 EMIR, 21.3.9 Dodd-Frank, 21.3.16 APA/ARM) write through.
//
// Model:
//
//	regulatory_report_events      — immutable event per
//	                                (uti, regime, report_seq)
//	regulatory_report_submissions — serialized artifact journal,
//	                                one row per dispatch attempt
//	regulatory_report_acks        — immutable repository ACK/NACK log
//	regulatory_report_breaks      — reconciliation/repair queue
//	regulatory_schema_versions    — version-pinned official rulesets
//	party_identifiers             — LEI / national-ID registry
//	cftc_position_limits          — Task 21.3.9 limit store
//	regulatory_submissions        — (migration 059) transport ledger
//	                                consumed by the Task 21.3.16
//	                                APA/ARM/TR/SDR adapters
//
// Invariants:
//   - Events are append-only; corrections append CORR rows chained via
//     supersedes_event_id — never UPDATE the original.
//   - Validation is against the registry-pinned required-field ruleset
//     for (regulation, schema_name) effective at event time; failures
//     quarantine the event (status=QUARANTINED + break VALIDATION) and
//     never reach the submission journal.
//   - Every mutation that lands a store row also lands an
//     audit_hash_chain link inside the same serializable transaction
//     (audit.Append nil-payload convention — the hash preimage is
//     recomputable from the row itself).
package reporting

import (
	"encoding/json"
	"time"
)

// ---------------------------------------------------------------------------
// Enumerated vocabularies (mirror the migration 054 enum types)
// ---------------------------------------------------------------------------

// Regime is the reporting regime an event belongs to.
type Regime string

const (
	RegimeEMIRREFIT Regime = "EMIR_REFIT" // EU EMIR REFIT ISO 20022 TR reporting
	RegimeCFTCP43   Regime = "CFTC_P43"   // CFTC Part 43 real-time public dissemination
	RegimeCFTCP45   Regime = "CFTC_P45"   // CFTC Part 45 regulatory swap reporting
	RegimeMIFID2    Regime = "MIFID2"     // MiFID II RTS 22 transaction reporting
)

// ActionType is the EMIR REFIT action-type vocabulary (ISO 20022 ActnTp)
// extended with the CFTC lifecycle actions and the venue's periodic
// report type.
type ActionType string

const (
	ActionNew         ActionType = "NEWT"
	ActionModify      ActionType = "MODI"
	ActionCorrect     ActionType = "CORR"
	ActionTerminate   ActionType = "TERM"
	ActionError       ActionType = "ERRO"
	ActionRevive      ActionType = "REVI"
	ActionValuation   ActionType = "VALU"
	ActionMargin      ActionType = "MARU"
	ActionPosition    ActionType = "POSC"
	ActionAllocation  ActionType = "ALOC"
	ActionClearing    ActionType = "CLRG"
	ActionPorting     ActionType = "PORT"
	ActionLargeTrader ActionType = "LTR" // CFTC large-trader report (venue extension)
)

// EventType is the shared lifecycle event vocabulary.
type EventType string

const (
	EventTypeTrade         EventType = "TRADE"
	EventTypeModify        EventType = "MOD"
	EventTypeCorrection    EventType = "CORR"
	EventTypeValuation     EventType = "VALU"
	EventTypeMargin        EventType = "MARG"
	EventTypeCompression   EventType = "COMP"
	EventTypeAllocation    EventType = "ALOC"
	EventTypeClearing      EventType = "CLRG"
	EventTypeTermination   EventType = "TERM"
	EventTypeError         EventType = "ERRO"
	EventTypeNonActionable EventType = "NOAT"
	EventTypeReport        EventType = "RPT" // derived/periodic (large trader, EOD)
)

// EventStatus tracks one event through validate → submit → ack.
type EventStatus string

const (
	EventRecorded    EventStatus = "RECORDED"
	EventValidated   EventStatus = "VALIDATED"
	EventQuarantined EventStatus = "QUARANTINED"
	EventSubmitted   EventStatus = "SUBMITTED"
	EventAccepted    EventStatus = "ACCEPTED"
	EventRejected    EventStatus = "REJECTED"
	EventRepaired    EventStatus = "REPAIRED"
)

// Destination is the external repository class the artifact targets.
type Destination string

const (
	DestinationTR  Destination = "TR"  // EMIR trade repository
	DestinationSDR Destination = "SDR" // CFTC swap data repository
	DestinationARM Destination = "ARM" // MiFID II approved reporting mechanism
	DestinationAPA Destination = "APA" // MiFID II approved publication arrangement
)

// SubmissionStatus is the artifact journal state.
type SubmissionStatus string

const (
	SubPending     SubmissionStatus = "PENDING"
	SubSubmitted   SubmissionStatus = "SUBMITTED"
	SubAcked       SubmissionStatus = "ACKED"
	SubNacked      SubmissionStatus = "NACKED"
	SubQuarantined SubmissionStatus = "QUARANTINED"
	SubSuperseded  SubmissionStatus = "SUPERSEDED"
)

// AckStatus is the repository verdict kind.
type AckStatus string

const (
	AckAccept AckStatus = "ACK"
	AckReject AckStatus = "NACK"
	AckRecon  AckStatus = "RECON"
)

// BreakType classifies a reconciliation/repair break.
type BreakType string

const (
	BreakMissing             BreakType = "MISSING"
	BreakDuplicate           BreakType = "DUPLICATE"
	BreakStaleValuation      BreakType = "STALE_VALUATION"
	BreakStaleMargin         BreakType = "STALE_MARGIN"
	BreakIDCollision         BreakType = "ID_COLLISION"
	BreakLifecycleDivergence BreakType = "LIFECYCLE_DIVERGENCE"
	BreakNackRepair          BreakType = "NACK_REPAIR"
	BreakValidation          BreakType = "VALIDATION"
)

// BreakStatus tracks the repair queue.
type BreakStatus string

const (
	BreakOpen      BreakStatus = "OPEN"
	BreakRepairing BreakStatus = "REPAIRING"
	BreakResolved  BreakStatus = "RESOLVED"
	BreakWontFix   BreakStatus = "WONT_FIX"
)

// ---------------------------------------------------------------------------
// Rows
// ---------------------------------------------------------------------------

// Event is one regulatory_report_events row.
type Event struct {
	EventID               int64           `json:"event_id"`
	UTI                   string          `json:"uti"`
	USI                   string          `json:"usi,omitempty"`
	UPI                   string          `json:"upi,omitempty"`
	PriorUTI              string          `json:"prior_uti,omitempty"`
	PriorUSI              string          `json:"prior_usi,omitempty"`
	Regime                Regime          `json:"regime"`
	Action                ActionType      `json:"action_type"`
	EventType             EventType       `json:"event_type"`
	ReportSeq             int             `json:"report_seq"`
	TradeID               int64           `json:"trade_id,omitempty"`
	PositionID            int64           `json:"position_id,omitempty"`
	InstrumentID          int64           `json:"instrument_id,omitempty"`
	InstrumentCode        string          `json:"instrument_code"` // venue instrument code (NOT ISIN)
	InstrumentType        string          `json:"instrument_type,omitempty"`
	AccountID             int64           `json:"account_id,omitempty"`
	CounterpartyAccountID int64           `json:"counterparty_account_id,omitempty"`
	BuyerLEI              string          `json:"buyer_lei,omitempty"`
	SellerLEI             string          `json:"seller_lei,omitempty"`
	BuyerIDType           string          `json:"buyer_id_type,omitempty"`
	BuyerID               string          `json:"buyer_id,omitempty"`
	SellerIDType          string          `json:"seller_id_type,omitempty"`
	SellerID              string          `json:"seller_id,omitempty"`
	DecisionMakerType     string          `json:"decision_maker_type,omitempty"`
	DecisionMakerID       string          `json:"decision_maker_id,omitempty"`
	TraderID              string          `json:"trader_id,omitempty"`
	AlgoID                string          `json:"algo_id,omitempty"`
	VenueMIC              string          `json:"venue_mic,omitempty"`
	Jurisdiction          string          `json:"jurisdiction,omitempty"`
	DualSided             bool            `json:"dual_sided"`
	Price                 string          `json:"price,omitempty"` // decimal text (28,8)
	Quantity              string          `json:"quantity,omitempty"`
	Notional              string          `json:"notional,omitempty"`
	Currency              string          `json:"currency,omitempty"`
	Valuation             json.RawMessage `json:"valuation,omitempty"`
	Margin                json.RawMessage `json:"margin,omitempty"`
	Clearing              json.RawMessage `json:"clearing,omitempty"`
	Confirmation          json.RawMessage `json:"confirmation,omitempty"`
	Allocation            json.RawMessage `json:"allocation,omitempty"`
	Payload               json.RawMessage `json:"payload,omitempty"` // regime-serialized record
	SchemaVersion         string          `json:"schema_version,omitempty"`
	Status                EventStatus     `json:"status"`
	ValidationErrors      json.RawMessage `json:"validation_errors,omitempty"`
	DisseminationDueAt    *time.Time      `json:"dissemination_due_at,omitempty"`
	SupersedesEventID     *int64          `json:"supersedes_event_id,omitempty"`
	EventTS               time.Time       `json:"event_ts"`
	ReportedAt            *time.Time      `json:"reported_at,omitempty"`
	CreatedAt             time.Time       `json:"created_at"`
}

// Submission is one regulatory_report_submissions artifact-journal row.
type Submission struct {
	ReportSubmissionID int64            `json:"report_submission_id"`
	EventID            int64            `json:"event_id"`
	Regime             Regime           `json:"regime"`
	Destination        Destination      `json:"destination"`
	Attempt            int              `json:"attempt"`
	SchemaName         string           `json:"schema_name"`
	SchemaVersion      string           `json:"schema_version"`
	Payload            json.RawMessage  `json:"payload"`
	PayloadXML         string           `json:"payload_xml,omitempty"`
	PayloadHash        string           `json:"payload_hash"`
	Status             SubmissionStatus `json:"status"`
	ExternalRef        string           `json:"external_ref,omitempty"`
	ErrorCode          string           `json:"error_code,omitempty"`
	ErrorText          string           `json:"error_text,omitempty"`
	BatchID            string           `json:"batch_id,omitempty"`
	SubmittedAt        *time.Time       `json:"submitted_at,omitempty"`
	ResolvedAt         *time.Time       `json:"resolved_at,omitempty"`
	CreatedAt          time.Time        `json:"created_at"`
}

// Ack is one immutable regulatory_report_acks row.
type Ack struct {
	AckID              int64           `json:"ack_id"`
	ReportSubmissionID int64           `json:"report_submission_id"`
	EventID            int64           `json:"event_id"`
	AckStatus          AckStatus       `json:"ack_status"`
	AckCode            string          `json:"ack_code,omitempty"`
	AckText            string          `json:"ack_text,omitempty"`
	ExternalRef        string          `json:"external_ref,omitempty"`
	Payload            json.RawMessage `json:"payload,omitempty"`
	ReceivedAt         time.Time       `json:"received_at"`
}

// Break is one regulatory_report_breaks repair-queue row.
type Break struct {
	BreakID    int64           `json:"break_id"`
	EventID    int64           `json:"event_id,omitempty"`
	UTI        string          `json:"uti,omitempty"`
	Regime     Regime          `json:"regime,omitempty"`
	BreakType  BreakType       `json:"break_type"`
	Status     BreakStatus     `json:"status"`
	DetectedBy string          `json:"detected_by"` // reconciler|ack|validator
	Detail     json.RawMessage `json:"detail,omitempty"`
	SLADueAt   time.Time       `json:"sla_due_at"`
	DetectedAt time.Time       `json:"detected_at"`
	ResolvedAt *time.Time      `json:"resolved_at,omitempty"`
	ResolvedBy int64           `json:"resolved_by,omitempty"`
	Notes      string          `json:"notes,omitempty"`
}

// SchemaVersion is one regulatory_schema_versions registry row.
type SchemaVersion struct {
	SchemaID       int64      `json:"schema_id"`
	Regulation     string     `json:"regulation"`
	SchemaName     string     `json:"schema_name"`
	Version        string     `json:"version"`
	RulesRef       string     `json:"rules_ref"`
	RequiredFields []string   `json:"required_fields"`
	EffectiveFrom  time.Time  `json:"effective_from"`
	EffectiveTo    *time.Time `json:"effective_to,omitempty"`
	Active         bool       `json:"active"`
	CreatedAt      time.Time  `json:"created_at"`
}

// PartyIdentifiers is the party_identifiers row for one account.
type PartyIdentifiers struct {
	AccountID         int64  `json:"account_id"`
	LEI               string `json:"lei,omitempty"`
	NationalIDType    string `json:"national_id_type,omitempty"`
	NationalID        string `json:"national_id,omitempty"`
	DecisionMakerID   string `json:"decision_maker_id,omitempty"`
	DecisionMakerType string `json:"decision_maker_type,omitempty"` // LEI | NATIONAL_ID
}

// TradeContext is the resolved execution a report is built from —
// trades ⨝ instruments ⨝ orders ⨝ party identifiers.
type TradeContext struct {
	TradeID         int64
	InstrumentID    int64
	InstrumentCode  string // instruments.symbol — venue instrument code
	InstrumentType  string // SPOT|FORWARD|SWAP|NDF|OPTION
	BaseCurrency    string
	QuoteCurrency   string
	BuyOrderID      int64
	SellOrderID     int64
	BuyerAccountID  int64
	SellerAccountID int64
	BuyerUserID     int64 // execution decision-maker (order owner)
	SellerUserID    int64
	BuyerAlgoID     string // orders.algo_params->>'algo_id' when set
	SellerAlgoID    string
	Price           string // decimal text
	Quantity        string
	ExecutedAt      time.Time
	SettlementDate  *time.Time
	ShardID         int64
	TradeSeq        int64
	TradeStatus     string // COMPLETED|BUSTED|PRICE_ADJUSTED
	// Party jurisdictions resolve from each side's latest KYC
	// submission (kyc_submissions.jurisdiction, migration 203) — the
	// CFTC-nexus gate for swap reporting (US person → Parts 43/45).
	BuyerJurisdiction  string
	SellerJurisdiction string
}

// ReconcileReport summarizes one reconciler pass.
type ReconcileReport struct {
	Regime         Regime    `json:"regime"`
	StartedAt      time.Time `json:"started_at"`
	CheckedEvents  int       `json:"checked_events"`
	OpenInternal   int       `json:"open_internal"`
	BreaksOpened   []int64   `json:"breaks_opened"`
	BreaksResolved []int64   `json:"breaks_resolved"`
}
