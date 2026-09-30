// Banking rails abstraction — Phase-11 Task 11.3.1.
//
// A Rail is one fiat payment rail (SWIFT, SEPA, FedNow, ACH, CHAPS,
// TARGET2 — the bank_method_enum domain of spec §5.6 minus the WIRE and
// INTERNAL fallbacks, which are not rails). The capability matrix is the
// static canonical table of spec §17.2 (rail/currency/speed) refined by
// §17.16's cut-off timetable; a DB-driven banking_rail_schedules table
// (migration 107) lands with Phase-24 Task 24.3.20 and overrides these
// statics at the composition root, never inside the adapters.
//
// Selection is currency + amount-cap + availability + account-eligibility
// filtered and fails closed: no eligible rail → BANKING_RAIL_UNAVAILABLE
// (503), a same-day-only request past cut-off → RAIL_CUTOFF_EXCEEDED
// (422). Adapters build typed message envelopes (MT103, pain.001,
// pacs.008 family, NACHA) — there is no live bank connectivity in this
// cluster; InitiatePayment persists the envelope as a rail_payments row
// in PREPARED state and only a wired RailTransport can advance it to
// DISPATCHED. A nil transport therefore never lies about delivery.
package funding

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"exchange/pkg/decimal"
)

// RailID is one of the six production banking rails.
type RailID string

const (
	RailSWIFT   RailID = "SWIFT"
	RailSEPA    RailID = "SEPA"
	RailFedNow  RailID = "FEDNOW"
	RailACH     RailID = "ACH"
	RailCHAPS   RailID = "CHAPS"
	RailTARGET2 RailID = "TARGET2"
)

// AllRails lists the rails in deterministic selection order: instant/
// same-day domestic rails before batch/international rails.
var AllRails = []RailID{
	RailFedNow, RailCHAPS, RailTARGET2, RailSEPA, RailACH, RailSWIFT,
}

// Rail-payment lifecycle (rail_payment_status_enum, migration 108).
const (
	RailPaymentPrepared     = "PREPARED"
	RailPaymentDispatched   = "DISPATCHED"
	RailPaymentAcknowledged = "ACKNOWLEDGED"
	RailPaymentSettled      = "SETTLED"
	RailPaymentReturned     = "RETURNED"
	RailPaymentFailed       = "FAILED"
	RailPaymentRejected     = "REJECTED"
)

// Rail payment direction + message-type vocabulary persisted on
// rail_payments (migration 108).
const (
	RailDirectionOutbound = "OUTBOUND"
	RailDirectionReturn   = "RETURN"

	MsgMT103     = "MT103"
	MsgMT202     = "MT202"
	MsgMT199     = "MT199"
	MsgPain001   = "PAIN001"
	MsgPacs008   = "PACS008"
	MsgPacs004   = "PACS004"
	MsgNachaFile = "NACHA_FILE"
)

// CodeBankingRailUnavailable is emitted when no eligible rail can carry
// an instruction — every candidate failed currency, amount-cap,
// availability (scoped kill-switch) or account-eligibility screening.
// Fail-closed: a payment is never silently parked without a rail.
const CodeBankingRailUnavailable = "BANKING_RAIL_UNAVAILABLE"

// ---------------------------------------------------------------------------
// Capability matrix (spec §17.2 rail table + §17.16 cut-off schedule)
// ---------------------------------------------------------------------------

// RailCapability is one row of the rail matrix. CutOffUTC is the daily
// instruction deadline in minutes since 00:00 UTC; nil means 24/7.
type RailCapability struct {
	Rail              RailID   `json:"rail"`
	Name              string   `json:"name"`
	MessageTypes      []string `json:"message_types"`
	AllCurrencies     bool     `json:"all_currencies"`
	Currencies        []string `json:"currencies,omitempty"`
	CutOffUTC         *int     `json:"cutoff_utc,omitempty"` // minutes since 00:00 UTC
	CutOffLabel       string   `json:"cutoff_label"`
	InstantCapable    bool     `json:"instant_capable"`
	MaxAmount         *string  `json:"max_amount,omitempty"`       // instant/eligibility cap (decimal text)
	CapInstantOnly    bool     `json:"cap_instant_only,omitempty"` // cap bounds the instant scheme, not the rail
	SettlementLag     string   `json:"settlement_lag"`             // T+0 | T+1
	WeekendProcessing bool     `json:"weekend_processing"`
}

// RailMatrix is the canonical static matrix. §17.16 is authoritative for
// cut-offs (FedNow 21:00 UTC, TARGET2 16:00 UTC, CHAPS 15:00 UTC, SEPA
// Instant 24/7/365 with the €100k cap, SWIFT 15:00 correspondent
// cut-off); the Phase-11 11.3.7 note supplies SEPA's 16:30 CET ≈ 15:30
// UTC SCT cut-off and ACH's same-day window is pinned to 16:45 ET ≈
// 20:45 UTC.
func RailMatrix() map[RailID]RailCapability {
	sepaInstantCap := "100000"
	return map[RailID]RailCapability{
		RailSWIFT: {
			Rail: RailSWIFT, Name: "SWIFT",
			MessageTypes:      []string{MsgMT103, MsgMT202, MsgMT199},
			AllCurrencies:     true,
			CutOffUTC:         intPtr(15 * 60), // 15:00 UTC correspondent cut-off (§17.16)
			CutOffLabel:       "15:00 UTC",
			InstantCapable:    false,
			SettlementLag:     "T+1",
			WeekendProcessing: false,
		},
		RailSEPA: {
			Rail: RailSEPA, Name: "SEPA (SCT / SCT Instant)",
			MessageTypes:      []string{MsgPain001, MsgPacs008, MsgPacs004},
			Currencies:        []string{"EUR"},
			CutOffUTC:         intPtr(15*60 + 30), // SCT 16:30 CET = 15:30 UTC (Task 11.3.7)
			CutOffLabel:       "15:30 UTC (SCT); SCT Instant 24/7",
			InstantCapable:    true, // SCT Inst, ≤ €100k (§17.16)
			MaxAmount:         &sepaInstantCap,
			CapInstantOnly:    true, // >€100k still rides standard SCT
			SettlementLag:     "T+0/T+1",
			WeekendProcessing: true, // SCT Inst runs 24/7/365
		},
		RailFedNow: {
			Rail: RailFedNow, Name: "FedNow",
			MessageTypes:      []string{MsgPacs008, MsgPacs004},
			Currencies:        []string{"USD"},
			CutOffUTC:         intPtr(21 * 60), // 17:00 ET = 21:00 UTC (§17.16)
			CutOffLabel:       "21:00 UTC",
			InstantCapable:    true,
			SettlementLag:     "T+0",
			WeekendProcessing: true, // instant rail runs 24/7/365
		},
		RailACH: {
			Rail: RailACH, Name: "ACH (NACHA)",
			MessageTypes:      []string{MsgNachaFile},
			Currencies:        []string{"USD"},
			CutOffUTC:         intPtr(20*60 + 45), // same-day ACH 16:45 ET = 20:45 UTC
			CutOffLabel:       "20:45 UTC",
			InstantCapable:    false,
			SettlementLag:     "T+1",
			WeekendProcessing: false,
		},
		RailCHAPS: {
			Rail: RailCHAPS, Name: "CHAPS",
			MessageTypes:      []string{MsgPacs008, MsgPacs004},
			Currencies:        []string{"GBP"},
			CutOffUTC:         intPtr(15 * 60), // 16:00 UK = 15:00 UTC (§17.16)
			CutOffLabel:       "15:00 UTC",
			InstantCapable:    true,
			SettlementLag:     "T+0",
			WeekendProcessing: false,
		},
		RailTARGET2: {
			Rail: RailTARGET2, Name: "TARGET2",
			MessageTypes:      []string{MsgPacs008, MsgPacs004},
			Currencies:        []string{"EUR"},
			CutOffUTC:         intPtr(16 * 60), // 17:00 CET = 16:00 UTC (§17.16)
			CutOffLabel:       "16:00 UTC",
			InstantCapable:    true,
			SettlementLag:     "T+0",
			WeekendProcessing: false,
		},
	}
}

func intPtr(v int) *int { return &v }

// Supports reports whether the rail can carry currency.
func (c RailCapability) Supports(currency string) bool {
	if c.AllCurrencies {
		return true
	}
	for _, cc := range c.Currencies {
		if cc == currency {
			return true
		}
	}
	return false
}

// WithinAmountCap reports whether amount fits the rail's per-payment
// ceiling (nil cap = uncapped).
func (c RailCapability) WithinAmountCap(amount decimal.Decimal) bool {
	if c.MaxAmount == nil {
		return true
	}
	lim, err := decimal.NewFromString(*c.MaxAmount)
	if err != nil {
		return false // an unparsable cap fails closed
	}
	return amount.LessThanOrEqual(lim)
}

// cutoffPassed reports whether now is strictly after the daily cut-off.
// A rail without a cut-off never reports one.
func (c RailCapability) cutoffPassed(now time.Time) bool {
	if c.CutOffUTC == nil {
		return false
	}
	now = now.UTC()
	return now.Hour()*60+now.Minute() >= *c.CutOffUTC
}

// NextValueDate computes the value date for an instruction submitted at
// now: today when before the cut-off, otherwise the next business day.
// Non-weekend-processing rails additionally roll Sat/Sun → Monday.
func (c RailCapability) NextValueDate(now time.Time) time.Time {
	d := now.UTC().Truncate(24 * time.Hour)
	if c.cutoffPassed(now) {
		d = d.Add(24 * time.Hour)
	}
	if !c.WeekendProcessing {
		for d.Weekday() == time.Saturday || d.Weekday() == time.Sunday {
			d = d.Add(24 * time.Hour)
		}
	}
	return d
}

// QueuedAfterCutoff reports whether an instruction submitted at now must
// queue for the next value date rather than execute today.
func (c RailCapability) QueuedAfterCutoff(now time.Time) bool {
	return !c.NextValueDate(now).Equal(now.UTC().Truncate(24 * time.Hour))
}

// ---------------------------------------------------------------------------
// Service seams
// ---------------------------------------------------------------------------

// RailGate reports whether a rail is currently operational — the seam
// Task 11.3.12's SCOPE_RAIL kill-switch binds (Redis flag
// halt:rail:{id}). A nil gate means "no scoped control wired" — every
// rail is treated as available.
type RailGate interface {
	Available(ctx context.Context, rail RailID) (bool, string, error)
}

// EligibilityChecker reports whether an account may use a rail — the
// seam the verified-beneficiary registry (Task 11.3.7) and KYC-tier
// restrictions bind. A nil checker means "no additional gate".
type EligibilityChecker interface {
	Eligible(ctx context.Context, accountID int64, rail RailID) (bool, string, error)
}

// RailTransport is the live bank-side transport seam — SWIFT Alliance,
// FedNow API, ECB TARGET2 access, partner ACH/CHAPS APIs. None is wired
// in this cluster; every adapter still produces canonical envelopes.
type RailTransport interface {
	Submit(ctx context.Context, env *WireEnvelope) error
	Status(ctx context.Context, endToEndID string) (string, error)
	ConfirmReceipt(ctx context.Context, endToEndID string) (bool, error)
}

// RailAdapter builds the per-rail typed message envelope — no I/O.
type RailAdapter interface {
	ID() RailID
	// BuildOutbound produces the canonical wire message for an outbound
	// customer payment (MT103 / pain.001 / pacs.008 / NACHA file row).
	BuildOutbound(p OutboundPayment) (*WireEnvelope, error)
	// BuildReturn produces the automated return-wire message
	// (pacs.004 / MT199+narrative / NACHA return entry).
	BuildReturn(r ReturnInstruction) (*WireEnvelope, error)
}

// TravelRuleGate is the Phase-21 Task 21.3.2 FATF Recommendation-16
// outbound gate — implemented by compliance.TravelRuleService and bound
// to DispatchService via WithTravelRule (compliance imports funding for
// these types; funding never imports compliance).
//
// EnforceOutbound persists/updates the travel_rule_records row for the
// withdrawal inside the caller's tx and returns:
//   - the (possibly originator-enriched) payment to hand to the rail
//     adapter,
//   - missing — the absent required R.16 fields; non-empty means the
//     wire MUST NOT dispatch (caller queues HELD,
//     reason QueueReasonTravelRuleMissing),
//   - a non-nil error aborts dispatch outright — tx rolls back and the
//     withdrawal stays CONFIRMED for the next sweep (fail-closed).
type TravelRuleGate interface {
	EnforceOutbound(ctx context.Context, tx pgx.Tx,
		w *DispatchableWithdrawal, p OutboundPayment) (OutboundPayment, []string, error)
}

// InboundTravelRuleChecker is the Phase-21 Task 21.3.2 inbound leg —
// DepositService calls it at dual-source resolution inside the ingest
// tx. The returned slice lists absent required fields; non-empty parks
// the deposit in PENDING_REVIEW (the record row lands inside the same
// tx for the officer supply-info flow).
type InboundTravelRuleChecker interface {
	CheckInbound(ctx context.Context, tx pgx.Tx, row *FundingTxRow,
		confs []DepositConfirmationRow) (missing []string, err error)
}

// WireEnvelope is the serialisable wire instruction. Payload is a typed
// per-rail struct persisted verbatim to rail_payments.envelope (JSONB).
type WireEnvelope struct {
	Rail        string `json:"rail"`
	MessageType string `json:"message_type"`
	EndToEndID  string `json:"end_to_end_id"`
	UETR        string `json:"uetr,omitempty"`
	Payload     any    `json:"payload"`
}

// OutboundPayment is the rail-agnostic withdrawal/settlement instruction.
type OutboundPayment struct {
	FundingTxID   int64
	SuspenseID    *int64
	AccountID     int64
	Rail          RailID
	Currency      string
	Amount        decimal.Decimal
	DebtorName    string // exchange nostro holder name
	DebtorAccount string // nostro IBAN / account number
	DebtorBIC     string
	CreditorName  string // beneficiary legal name
	CreditorIBAN  string
	CreditorBIC   string
	// Originator* carry the FATF R.16 ordering customer when the client
	// ordering the payment differs from the debtor (the exchange nostro
	// holder). The Phase-21 Task 21.3.2 travel-rule gate populates them;
	// SwiftAdapter maps them onto MT103 field 50K (name/account/address).
	// Empty → adapters fall back to the Debtor* fields (unchanged legacy
	// envelope for in-house wires).
	OriginatorName    string
	OriginatorAccount string
	OriginatorAddress string
	RemittanceInfo    string
	EndToEndID        string // caller-supplied or generated
	Charges           string // OUR|SHA|BEN
	ValueDate         time.Time
}

// ReturnInstruction is the automated return-wire request (spec §17.12.2
// — funds returned to sender minus banking processing fees).
type ReturnInstruction struct {
	OriginalEndToEndID string
	OriginalBankTxID   string
	Rail               RailID
	Currency           string
	Amount             decimal.Decimal
	ReturnReasonCode   string // ISO 20022 RsnCd or ACH R-code
	ReturnReason       string
	DebtorName         string // exchange returning the funds
	DebtorAccount      string
	DebtorBIC          string
	CreditorName       string // original originator
	CreditorAccount    string
	CreditorBIC        string
}

// ---------------------------------------------------------------------------
// RailService — selection, dispatch, status
// ---------------------------------------------------------------------------

// SelectionRequest asks the matrix for a rail.
type SelectionRequest struct {
	AccountID      int64
	Currency       string
	Amount         decimal.Decimal
	Preferred      RailID    // "" = automatic
	RequireSameDay bool      // strict intraday value date
	At             time.Time // submission time (clock() when zero)
}

// Selection is the chosen rail + computed value date.
type Selection struct {
	Rail          RailID         `json:"rail"`
	Capability    RailCapability `json:"capability"`
	ValueDate     time.Time      `json:"value_date"`
	QueuedNextDay bool           `json:"queued_next_day"`
	RejectedRails []string       `json:"rejected_rails,omitempty"`
}

// RailService owns the matrix, adapters and rail_payments persistence.
type RailService struct {
	store     Store
	poster    JournalPoster // compensating journals on returns
	gate      RailGate
	elig      EligibilityChecker
	transport RailTransport
	alerter   OpsAlerter
	adapters  map[RailID]RailAdapter
	matrix    map[RailID]RailCapability
	clock     func() time.Time
	logf      func(format string, args ...any)
}

// NewRailService wires the service; store + at least one adapter are
// required (fail closed). poster may be nil — ApplyReturn then refuses
// compensating journals fail-closed instead of skipping them.
func NewRailService(store Store, poster JournalPoster) (*RailService, error) {
	if store == nil {
		return nil, fmt.Errorf("funding: rail service requires store")
	}
	s := &RailService{
		store: store, poster: poster,
		adapters: map[RailID]RailAdapter{}, matrix: RailMatrix(),
		clock: time.Now,
	}
	// Default adapters: one per rail, pure envelope builders.
	for _, a := range []RailAdapter{
		SwiftAdapter{}, SepaAdapter{}, FedNowAdapter{},
		ACHAdapter{}, ChapsAdapter{}, Target2Adapter{},
	} {
		s.adapters[a.ID()] = a
	}
	return s, nil
}

// WithGate binds the scoped rail-availability check (halt:rail:{id}).
func (s *RailService) WithGate(g RailGate) *RailService { s.gate = g; return s }

// WithEligibility binds the account-per-rail eligibility check.
func (s *RailService) WithEligibility(e EligibilityChecker) *RailService {
	s.elig = e
	return s
}

// WithTransport binds the live rail transport. Production wiring lands
// with the correspondent connectors; nil keeps every instruction in
// PREPARED — never fabricate delivery.
func (s *RailService) WithTransport(t RailTransport) *RailService {
	s.transport = t
	return s
}

// WithAlerter binds the ops alerter (P1 on unknown return codes /
// quarantine, P1 on failed compensating journals).
func (s *RailService) WithAlerter(a OpsAlerter) *RailService {
	s.alerter = a
	return s
}

// WithMatrix replaces the static matrix (Phase-24 migration-107 rows).
func (s *RailService) WithMatrix(m map[RailID]RailCapability) *RailService {
	if m != nil && len(m) > 0 {
		s.matrix = m
	}
	return s
}

// WithClock overrides the clock (tests).
func (s *RailService) WithClock(c func() time.Time) *RailService { s.clock = c; return s }

// WithLogger wires a log sink for best-effort diagnostics.
func (s *RailService) WithLogger(f func(format string, args ...any)) *RailService {
	s.logf = f
	return s
}

// Capabilities exposes the matrix for the public rails endpoint.
func (s *RailService) Capabilities() []RailCapability {
	out := make([]RailCapability, 0, len(AllRails))
	for _, r := range AllRails {
		out = append(out, s.matrix[r])
	}
	return out
}

// Select chooses the cheapest-capable rail honouring the preferred rail
// first, then the deterministic AllRails order. Every rejected candidate
// is recorded; zero candidates → BANKING_RAIL_UNAVAILABLE.
func (s *RailService) Select(ctx context.Context, req SelectionRequest) (*Selection, error) {
	ccy, err := normalizeCurrency(req.Currency)
	if err != nil {
		return nil, err
	}
	if !req.Amount.IsPositive() {
		return nil, errCode("INVALID_REQUEST", "amount must be positive")
	}
	at := req.At
	if at.IsZero() {
		at = s.clock()
	}
	at = at.UTC()

	order := AllRails
	if req.Preferred != "" {
		if _, ok := s.matrix[req.Preferred]; !ok {
			return nil, errf("INVALID_REQUEST", "rail %q is not supported", req.Preferred)
		}
		order = append([]RailID{req.Preferred}, order...) // preferred first; dup harmless
	}

	var rejected []string
	for _, id := range order {
		capab, ok := s.matrix[id]
		if !ok {
			continue
		}
		if !capab.Supports(ccy) {
			rejected = append(rejected, fmt.Sprintf("%s:currency", id))
			continue
		}
		// Amount-cap rejection only applies when the cap bounds the whole
		// rail (e.g. a per-payment network ceiling). An instant-scheme cap
		// (SEPA's €100k) merely downgrades the instruction to the
		// standard scheme — the adapter picks the envelope.
		if !capab.WithinAmountCap(req.Amount) && !capab.CapInstantOnly {
			rejected = append(rejected, fmt.Sprintf("%s:amount_cap", id))
			continue
		}
		if s.gate != nil {
			okAvail, reason, gerr := s.gate.Available(ctx, id)
			if gerr != nil {
				// Gate failure fails closed for the rail, not for the call —
				// other rails may still serve the instruction.
				s.log("funding: rail gate check %s failed (rail skipped): %v", id, gerr)
				rejected = append(rejected, fmt.Sprintf("%s:gate_error", id))
				continue
			}
			if !okAvail {
				rejected = append(rejected, fmt.Sprintf("%s:unavailable:%s", id, reason))
				continue
			}
		}
		if req.AccountID > 0 && s.elig != nil {
			okElig, reason, eerr := s.elig.Eligible(ctx, req.AccountID, id)
			if eerr != nil {
				return nil, wrapCode("INTERNAL_ERROR", "rail eligibility check", eerr)
			}
			if !okElig {
				rejected = append(rejected, fmt.Sprintf("%s:ineligible:%s", id, reason))
				continue
			}
		}
		// Candidate found — evaluate the value-date rule.
		vd := capab.NextValueDate(at)
		queued := !vd.Equal(at.Truncate(24 * time.Hour))
		if queued && req.RequireSameDay {
			return nil, errf("RAIL_CUTOFF_EXCEEDED",
				"rail %s cut-off passed (%s) — same-day value date unavailable", id, capab.CutOffLabel)
		}
		return &Selection{
			Rail: id, Capability: capab, ValueDate: vd,
			QueuedNextDay: queued, RejectedRails: rejected,
		}, nil
	}
	return nil, errf(CodeBankingRailUnavailable,
		"no banking rail available for %s %s (rejected: %s)",
		req.Amount.String(), ccy, strings.Join(rejected, ","))
}

// Dispatch builds the outbound envelope via the rail adapter and
// persists it inside tx as a rail_payments row in PREPARED state.
// Delivery is a separate transport step — Dispatch never claims more
// than "envelope built + persisted". The caller commits tx first, then
// calls Submit to advance the row to DISPATCHED.
func (s *RailService) Dispatch(ctx context.Context, tx pgx.Tx, p OutboundPayment) (*RailPaymentRow, error) {
	if err := s.validatePayment(p); err != nil {
		return nil, err
	}
	a, ok := s.adapters[p.Rail]
	if !ok {
		return nil, errf(CodeBankingRailUnavailable, "no adapter for rail %s", p.Rail)
	}
	capab := s.matrix[p.Rail]
	if !capab.Supports(p.Currency) {
		return nil, errf(CodeBankingRailUnavailable,
			"rail %s does not support %s", p.Rail, p.Currency)
	}
	if s.gate != nil {
		okAvail, reason, gerr := s.gate.Available(ctx, p.Rail)
		if gerr != nil {
			return nil, wrapCode("INTERNAL_ERROR", "rail gate check", gerr)
		}
		if !okAvail {
			return nil, errf(CodeBankingRailUnavailable,
				"rail %s suspended: %s", p.Rail, reason)
		}
	}
	env, err := a.BuildOutbound(p)
	if err != nil {
		return nil, err
	}
	body, err := json.Marshal(env)
	if err != nil {
		return nil, wrapCode("INTERNAL_ERROR", "rail envelope marshal", err)
	}
	vd := p.ValueDate
	if vd.IsZero() {
		vd = capab.NextValueDate(s.clock())
	}
	var uetr *string
	if env.UETR != "" {
		uetr = &env.UETR
	}
	var ftx *int64
	if p.FundingTxID > 0 {
		ftx = &p.FundingTxID
	}
	row, err := s.store.InsertRailPayment(ctx, tx, RailPaymentRow{
		FundingTransactionID: ftx,
		SuspenseMappingID:    p.SuspenseID,
		Direction:            RailDirectionOutbound,
		Rail:                 string(p.Rail),
		MessageType:          env.MessageType,
		EndToEndID:           env.EndToEndID,
		UETR:                 uetr,
		Envelope:             body,
		Status:               RailPaymentPrepared,
		ValueDate:            &vd,
	})
	if err != nil {
		return nil, err
	}
	return row, nil
}

// Submit advances a PREPARED instruction through the wired transport to
// DISPATCHED. A nil transport is a fail-closed
// BANKING_RAIL_UNAVAILABLE — the row stays PREPARED for a later wired
// dispatcher, never fabricated as sent.
func (s *RailService) Submit(ctx context.Context, tx pgx.Tx, paymentID int64) (*RailPaymentRow, error) {
	row, err := s.store.RailPaymentForUpdate(ctx, tx, paymentID)
	if err != nil {
		return nil, err
	}
	if row.Status != RailPaymentPrepared {
		// Idempotent re-submit: already progressed states return the row.
		return row, nil
	}
	if s.transport == nil {
		return nil, errf(CodeBankingRailUnavailable,
			"rail transport not wired for %s — instruction remains PREPARED", row.Rail)
	}
	var env WireEnvelope
	if err := json.Unmarshal(row.Envelope, &env); err != nil {
		return nil, wrapCode("INTERNAL_ERROR", "rail envelope decode", err)
	}
	if err := s.transport.Submit(ctx, &env); err != nil {
		now := s.clock().UTC()
		reason := "submit failed"
		_ = s.store.SetRailPaymentStatus(ctx, tx, row.ID, RailPaymentFailed,
			nil, &reason, nil, &now)
		return nil, wrapCode("SETTLEMENT_RAIL_REJECTED",
			"rail submit rejected", err)
	}
	now := s.clock().UTC()
	if err := s.store.SetRailPaymentStatus(ctx, tx, row.ID, RailPaymentDispatched,
		nil, nil, &now, nil); err != nil {
		return nil, err
	}
	row.Status = RailPaymentDispatched
	row.DispatchedAt = &now
	return row, nil
}

// GetStatus asks the transport for the wire's current state. Nil
// transport → BANKING_RAIL_UNAVAILABLE (no fabricated status).
func (s *RailService) GetStatus(ctx context.Context, endToEndID string) (string, error) {
	if strings.TrimSpace(endToEndID) == "" {
		return "", errCode("INVALID_REQUEST", "end_to_end_id required")
	}
	if s.transport == nil {
		return "", errf(CodeBankingRailUnavailable,
			"rail transport not wired — status unavailable")
	}
	st, err := s.transport.Status(ctx, endToEndID)
	if err != nil {
		return "", wrapCode("SETTLEMENT_RAIL_REJECTED", "rail status query", err)
	}
	return st, nil
}

// ConfirmReceipt records receipt acknowledgement for a dispatched wire
// through the transport; nil transport → BANKING_RAIL_UNAVAILABLE.
func (s *RailService) ConfirmReceipt(ctx context.Context, endToEndID string) (bool, error) {
	if strings.TrimSpace(endToEndID) == "" {
		return false, errCode("INVALID_REQUEST", "end_to_end_id required")
	}
	if s.transport == nil {
		return false, errf(CodeBankingRailUnavailable,
			"rail transport not wired — receipt unconfirmable")
	}
	return s.transport.ConfirmReceipt(ctx, endToEndID)
}

// BuildReturnWire constructs + persists the automated return payment
// (pacs.004 / NACHA return / MT199) against a suspense mapping — the
// spec §17.12.2 funds-returned-to-source record. Persisted inside tx in
// PREPARED state; the suspense row is backlinked by the caller.
func (s *RailService) BuildReturnWire(ctx context.Context, tx pgx.Tx, r ReturnInstruction) (*RailPaymentRow, *WireEnvelope, error) {
	a, ok := s.adapters[r.Rail]
	if !ok {
		return nil, nil, errf(CodeBankingRailUnavailable, "no adapter for rail %s", r.Rail)
	}
	env, err := a.BuildReturn(r)
	if err != nil {
		return nil, nil, err
	}
	body, err := json.Marshal(env)
	if err != nil {
		return nil, nil, wrapCode("INTERNAL_ERROR", "return envelope marshal", err)
	}
	var uetr *string
	if env.UETR != "" {
		uetr = &env.UETR
	}
	rc := r.ReturnReasonCode
	row, err := s.store.InsertRailPayment(ctx, tx, RailPaymentRow{
		Direction:   RailDirectionReturn,
		Rail:        string(r.Rail),
		MessageType: env.MessageType,
		EndToEndID:  env.EndToEndID,
		UETR:        uetr,
		Envelope:    body,
		Status:      RailPaymentPrepared,
		ReturnCode:  &rc,
	})
	if err != nil {
		return nil, nil, err
	}
	return row, env, nil
}

// validatePayment enforces the outbound instruction invariants before an
// envelope is built — fail closed on any missing canonical field.
func (s *RailService) validatePayment(p OutboundPayment) error {
	if p.Rail == "" {
		return errCode("INVALID_REQUEST", "rail required")
	}
	if _, err := normalizeCurrency(p.Currency); err != nil {
		return err
	}
	if !p.Amount.IsPositive() {
		return errCode("INVALID_REQUEST", "amount must be positive")
	}
	if !p.Amount.Round(8).Equal(p.Amount) {
		return errf("INVALID_REQUEST", "amount %s exceeds the 8-decimal quantum", p.Amount.String())
	}
	if strings.TrimSpace(p.CreditorName) == "" {
		return errCode("INVALID_REQUEST", "creditor name required")
	}
	if strings.TrimSpace(p.CreditorIBAN) == "" && strings.TrimSpace(p.CreditorBIC) == "" {
		return errCode("INVALID_REQUEST", "creditor account or BIC required")
	}
	return nil
}

func (s *RailService) log(format string, args ...any) {
	if s.logf != nil {
		s.logf(format, args...)
	}
}
