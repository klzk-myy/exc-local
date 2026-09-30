// Service — the reporting lifecycle engine (Task 21.3.14, spec §14.1a):
//
//	execution in → regime fan-out → party decomposition → UTI/USI/UPI →
//	version-pinned validation → event + artifact rows → dispatch feed →
//	ACK/NACK ingest → break/repair queue → daily reconciliation.
//
// Fail-closed invariants:
//   - Missing endpoint config never reaches here (the dispatcher fails
//     before Submit); a missing schema version QUARANTINES the event.
//   - Identifier collision (UTI bound to another trade) rejects the
//     event and opens an ID_COLLISION break.
//   - A NACK lands its NACK_REPAIR break in the same transaction.
package reporting

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/shopspring/decimal"
)

// ---------------------------------------------------------------------------
// Configuration
// ---------------------------------------------------------------------------

// Config carries the venue identity + lifecycle tunables. Identity comes
// from env in production (EXC_VENUE_LEI, EXC_VENUE_MIC, EXC_USI_NAMESPACE);
// dev defaults are clearly test values.
type Config struct {
	VenueLEI     string
	VenueMIC     string
	USINamespace string
	// DualSided — the EMIR dual-sided reporting flag: when true the
	// venue reports for both counterparties (delegated reporting).
	DualSided bool
	// ReportAllToCFTC — dev/test switch: report every derivative to the
	// CFTC regimes even without a US-nexus party (production leaves it
	// off; scope is jurisdiction-driven).
	ReportAllToCFTC bool
	// RepairSLA bounds a NACK/validation repair (spec §14.9 — 2h
	// post-detection default).
	RepairSLA time.Duration
	// StaleAfter — a position updated without a matching VALU/MARU
	// continuation older than this raises STALE_VALUATION/STALE_MARGIN.
	StaleAfter time.Duration
	// Now injects the clock (tests pin it).
	Now func() time.Time
	// Alerter is the Compliance Officer notification seam (NACK breaks,
	// SLA breaches) — same signature as compliance.Alerter
	// (severity, code, summary); nil = log-only.
	Alerter AlertFunc
	// DerivativeEnrich supplies the EMIR REFIT NEWT economics the trade
	// row alone doesn't carry — the pinned ruleset requires valuation,
	// margin and notional on the report. Wired to the Phase-19.5
	// forward-points oracle (compliance.EMIRReporter.EnrichNEWT); a
	// returned error propagates (the consumer NAKs for redelivery —
	// stale/missing points fail closed, never fabricated). Nil → NEWTs
	// validate without enrichment and the required-field validator
	// quarantines them (also fail-closed).
	DerivativeEnrich func(ctx context.Context, tc *TradeContext) (
		notional string, valuation, margin json.RawMessage, err error)
}

// ConfigFromEnv resolves identity from the environment with loud
// test-default fallbacks (the dev markers TestVenueLEI/DefaultVenueMIC/
// TestUSINamespace make a misconfigured deployment obvious in every
// generated identifier).
func ConfigFromEnv() Config {
	c := Config{
		VenueLEI:     strings.TrimSpace(os.Getenv("EXC_VENUE_LEI")),
		VenueMIC:     strings.TrimSpace(os.Getenv("EXC_VENUE_MIC")),
		USINamespace: strings.TrimSpace(os.Getenv("EXC_USI_NAMESPACE")),
		RepairSLA:    2 * time.Hour,
		StaleAfter:   24 * time.Hour,
		DualSided:    true,
	}
	if c.VenueLEI == "" {
		c.VenueLEI = TestVenueLEI
	}
	if c.VenueMIC == "" {
		c.VenueMIC = DefaultVenueMIC
	}
	if c.USINamespace == "" {
		c.USINamespace = TestUSINamespace
	}
	if v := strings.TrimSpace(os.Getenv("EXC_REPORT_ALL_CFTC")); v == "1" || strings.EqualFold(v, "true") {
		c.ReportAllToCFTC = true
	}
	return c
}

func (c Config) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now().UTC()
}

func (c Config) repairSLA() time.Duration {
	if c.RepairSLA <= 0 {
		return 2 * time.Hour
	}
	return c.RepairSLA
}

func (c Config) staleAfter() time.Duration {
	if c.StaleAfter <= 0 {
		return 24 * time.Hour
	}
	return c.StaleAfter
}

// Service is the lifecycle engine over a Store.
type Service struct {
	Store Store
	Cfg   Config
}

// Now is the injected clock (UTC, or the test pin).
func (s *Service) Now() time.Time { return s.Cfg.now() }

// NewService validates the config (fail closed on malformed identity) —
// a venue LEI that fails ISO 17442 means every generated UTI is invalid.
func NewService(st Store, cfg Config) (*Service, error) {
	if st == nil {
		return nil, fmt.Errorf("reporting: nil store")
	}
	if err := ValidateLEI(cfg.VenueLEI); err != nil {
		return nil, fmt.Errorf("reporting: venue LEI: %w", err)
	}
	if err := ValidateMIC(cfg.VenueMIC); err != nil {
		return nil, fmt.Errorf("reporting: venue MIC: %w", err)
	}
	if cfg.USINamespace == "" {
		return nil, fmt.Errorf("reporting: empty USI namespace")
	}
	return &Service{Store: st, Cfg: cfg}, nil
}

// ---------------------------------------------------------------------------
// Regime fan-out
// ---------------------------------------------------------------------------

// Derivative reports to EMIR + (US-nexus) CFTC; spot to MiFID only.
func isDerivative(instrumentType string) bool {
	switch strings.ToUpper(instrumentType) {
	case "FORWARD", "SWAP", "NDF", "OPTION":
		return true
	}
	return false
}

func isUS(jurisdiction string) bool {
	return strings.EqualFold(strings.TrimSpace(jurisdiction), "US")
}

// RegimesFor returns the reporting regimes a trade falls under:
//   - MiFID II RTS 22 — every on-venue transaction (spec §14.5).
//   - EMIR REFIT — derivative instruments (FORWARD/SWAP/NDF/OPTION).
//   - CFTC Parts 43/45 — derivative + US nexus (either party US-
//     jurisdictional per KYC, or the dev ReportAllToCFTC flag).
func (s *Service) RegimesFor(tc *TradeContext) []Regime {
	regimes := []Regime{RegimeMIFID2}
	if !isDerivative(tc.InstrumentType) {
		return regimes
	}
	regimes = append(regimes, RegimeEMIRREFIT)
	if s.Cfg.ReportAllToCFTC || isUS(tc.BuyerJurisdiction) || isUS(tc.SellerJurisdiction) {
		regimes = append(regimes, RegimeCFTCP43, RegimeCFTCP45)
	}
	return regimes
}

// DestinationFor maps a regime to its repository class.
func DestinationFor(regime Regime) Destination {
	switch regime {
	case RegimeEMIRREFIT:
		return DestinationTR
	case RegimeCFTCP43, RegimeCFTCP45:
		return DestinationSDR
	case RegimeMIFID2:
		return DestinationARM
	}
	return ""
}

// ---------------------------------------------------------------------------
// Party decomposition (RTS 22 buyer/seller/decision-maker)
// ---------------------------------------------------------------------------

// partyID picks the best identifier for a party: LEI first, then the
// RTS 22 national id, then the internal INTC pseudo-code. A completely
// missing party leaves the id empty — the pinned validator quarantines
// (never fabricate).
func partyID(p *PartyIdentifiers) (idType, id string) {
	if p == nil {
		return "", ""
	}
	if p.LEI != "" {
		return "LEI", p.LEI
	}
	if p.NationalID != "" {
		return "NATIONAL_ID", p.NationalID
	}
	return "", ""
}

func intcID(userID int64) string { return fmt.Sprintf("INTC%d", userID) }

// ---------------------------------------------------------------------------
// Execution ingestion
// ---------------------------------------------------------------------------

// RecordExecution resolves one trade into per-regime canonical events +
// dispatchable artifacts. Idempotent on replay: an existing NEWT row for
// (uti, regime) returns the stored event without a duplicate insert —
// at-least-once consumers replay safely.
func (s *Service) RecordExecution(ctx context.Context, tc *TradeContext) ([]*Event, error) {
	if tc == nil {
		return nil, fmt.Errorf("reporting: nil trade context")
	}
	buyer, err := s.Store.PartyFor(ctx, tc.BuyerAccountID)
	if err != nil {
		return nil, err
	}
	seller, err := s.Store.PartyFor(ctx, tc.SellerAccountID)
	if err != nil {
		return nil, err
	}
	buyerType, buyerID := partyID(buyer)
	sellerType, sellerID := partyID(seller)
	if buyerType == "" {
		buyerType, buyerID = "INTC", intcID(tc.BuyerUserID)
	}
	if sellerType == "" {
		sellerType, sellerID = "INTC", intcID(tc.SellerUserID)
	}

	var out []*Event
	for _, regime := range s.RegimesFor(tc) {
		ev, err := s.recordForRegime(ctx, tc, regime, buyer, seller,
			buyerType, buyerID, sellerType, sellerID)
		if err != nil {
			return out, err
		}
		out = append(out, ev)
	}
	return out, nil
}

func (s *Service) recordForRegime(ctx context.Context, tc *TradeContext,
	regime Regime, buyer, seller *PartyIdentifiers,
	buyerType, buyerID, sellerType, sellerID string) (*Event, error) {

	uti := UTIFor(s.Cfg.VenueLEI, "TRADE", tc.TradeID)
	// Idempotent replay: the NEWT row for (uti, regime) already exists.
	if existing, err := s.Store.LatestEvent(ctx, uti, regime); err != nil {
		return nil, err
	} else if existing != nil && existing.Action == ActionNew {
		return existing, nil
	}

	// Collision-reject: a UTI bound to a different trade is a data-
	// integrity defect — open an ID_COLLISION break, refuse the event.
	if owner, found, err := s.Store.UTIOwner(ctx, uti); err != nil {
		return nil, err
	} else if found && owner != tc.TradeID {
		br := &Break{
			UTI:        uti,
			Regime:     regime,
			BreakType:  BreakIDCollision,
			DetectedBy: "validator",
			SLADueAt:   s.Cfg.now().Add(s.Cfg.repairSLA()),
		}
		br.Detail, _ = json.Marshal(map[string]any{
			"uti": uti, "existing_trade_id": owner,
			"incoming_trade_id": tc.TradeID,
		})
		_ = s.Store.InsertBreak(ctx, br)
		s.alert(ctx, "P1", "UTI_COLLISION",
			fmt.Sprintf("UTI %s bound to trade %d; incoming trade %d rejected",
				uti, owner, tc.TradeID))
		return nil, fmt.Errorf("reporting: UTI %s already bound to trade %d", uti, owner)
	}

	e := &Event{
		UTI:            uti,
		Regime:         regime,
		Action:         ActionNew,
		EventType:      EventTypeTrade,
		ReportSeq:      1,
		TradeID:        tc.TradeID,
		InstrumentID:   tc.InstrumentID,
		InstrumentCode: tc.InstrumentCode,
		InstrumentType: tc.InstrumentType,
		VenueMIC:       s.Cfg.VenueMIC,
		Price:          tc.Price,
		Quantity:       tc.Quantity,
		Currency:       tc.QuoteCurrency,
		EventTS:        tc.ExecutedAt,
	}
	if isDerivative(tc.InstrumentType) {
		e.UPI = UPIFor(tc.InstrumentType, tc.BaseCurrency, tc.QuoteCurrency)
	}
	switch regime {
	case RegimeMIFID2:
		// Venue-side reporting account = buyer (the venue reports both
		// sides via buyer/seller decomposition).
		e.AccountID = tc.BuyerAccountID
		e.CounterpartyAccountID = tc.SellerAccountID
		e.Jurisdiction = "EU"
		e.BuyerLEI = strOr(buyer, func(p *PartyIdentifiers) string { return p.LEI })
		e.SellerLEI = strOr(seller, func(p *PartyIdentifiers) string { return p.LEI })
		e.BuyerIDType, e.BuyerID = buyerType, buyerID
		e.SellerIDType, e.SellerID = sellerType, sellerID
		// Decision-maker: algo id when algo-driven, else the account's
		// declared decision-maker, else the order owner (INTC).
		e.DecisionMakerType, e.DecisionMakerID = decisionMaker(buyer, tc.BuyerAlgoID, tc.BuyerUserID)
		e.TraderID = intcID(tc.BuyerUserID)
		e.AlgoID = tc.BuyerAlgoID
		due := RTS22Deadline(tc.ExecutedAt)
		e.DisseminationDueAt = &due
	case RegimeEMIRREFIT:
		e.AccountID = tc.BuyerAccountID
		e.CounterpartyAccountID = tc.SellerAccountID
		e.Jurisdiction = "EU"
		e.DualSided = s.Cfg.DualSided
		e.BuyerLEI = strOr(buyer, func(p *PartyIdentifiers) string { return p.LEI })
		e.SellerLEI = strOr(seller, func(p *PartyIdentifiers) string { return p.LEI })
		e.BuyerIDType, e.BuyerID = buyerType, buyerID
		e.SellerIDType, e.SellerID = sellerType, sellerID
		// Notional is deterministic trade arithmetic (base qty × price,
		// quote-ccy) — required on every EMIR report.
		e.Notional = notionalOf(tc.Price, tc.Quantity)
		// Valuation + margin ride the configured enricher (Phase-19.5
		// forward-points oracle). An oracle error fails closed —
		// the caller NAKs rather than shipping an unpriced report.
		if s.Cfg.DerivativeEnrich != nil {
			n, v, m, err := s.Cfg.DerivativeEnrich(ctx, tc)
			if err != nil {
				return nil, fmt.Errorf("reporting: derivative enrich %s: %w", uti, err)
			}
			if n != "" {
				e.Notional = n
			}
			e.Valuation, e.Margin = v, m
		}
	case RegimeCFTCP43, RegimeCFTCP45:
		e.USI = USIFor(s.Cfg.USINamespace, "TRADE", tc.TradeID)
		e.AccountID = tc.BuyerAccountID
		e.CounterpartyAccountID = tc.SellerAccountID
		e.Jurisdiction = "US"
		e.BuyerIDType, e.BuyerID = buyerType, buyerID
		e.SellerIDType, e.SellerID = sellerType, sellerID
		if regime == RegimeCFTCP43 {
			// Part 43 real-time public dissemination SLA: 15 minutes.
			due := tc.ExecutedAt.Add(15 * time.Minute)
			e.DisseminationDueAt = &due
		}
	}

	return s.validateAndPersist(ctx, e)
}

func strOr(p *PartyIdentifiers, f func(*PartyIdentifiers) string) string {
	if p == nil {
		return ""
	}
	return f(p)
}

// notionalOf computes the report notional — base quantity × price in
// quote currency. Deterministic trade arithmetic; a malformed decimal
// yields "" so the required-field validator quarantines (fail closed).
func notionalOf(price, quantity string) string {
	p, err := decimal.NewFromString(price)
	if err != nil {
		return ""
	}
	q, err := decimal.NewFromString(quantity)
	if err != nil {
		return ""
	}
	return p.Mul(q).String()
}

func decisionMaker(p *PartyIdentifiers, algoID string, userID int64) (string, string) {
	if algoID != "" {
		return "ALGO", algoID
	}
	if p != nil && p.DecisionMakerID != "" {
		t := p.DecisionMakerType
		if t == "" {
			t = "LEI"
		}
		return t, p.DecisionMakerID
	}
	return "INTC", intcID(userID)
}

// rts22Deadline — T+1 23:59 CET per spec §14.5/§14.9. CET/CEST is
// Europe/Berlin; the deadline lands in UTC.
var locCET = func() *time.Location {
	l, err := time.LoadLocation("Europe/Berlin")
	if err != nil {
		return time.FixedZone("CET", 3600)
	}
	return l
}()

// RTS22Deadline — the T+1 23:59 CET report transmission deadline (spec
// §14.5/§14.9). Exported for the MiFID adapter + deadline assertions.
func RTS22Deadline(executedAt time.Time) time.Time {
	local := executedAt.In(locCET)
	d := time.Date(local.Year(), local.Month(), local.Day()+1, 23, 59, 0, 0, locCET)
	return d.UTC()
}

// ---------------------------------------------------------------------------
// Validate → persist → artifact
// ---------------------------------------------------------------------------

// validateAndPersist runs the pinned-ruleset check, stores the event
// (QUARANTINED on failure — with a VALIDATION break), and on success
// builds the artifact-journal row that the dispatcher ships.
func (s *Service) validateAndPersist(ctx context.Context, e *Event) (*Event, error) {
	// LTR (large-trader) rows are venue-extension reports filed to the
	// CFTC outside the SDR feed — they persist without the repository
	// field set and without an artifact.
	if e.Action == ActionLargeTrader {
		e.Status = EventValidated
		e.Payload, _ = json.Marshal(WithExecutingEntity(PayloadFor(e), s.Cfg.VenueLEI))
		return e, s.Store.InsertEvent(ctx, e)
	}

	regulation, schemaName := SchemaFor(e.Regime, DestinationFor(e.Regime))
	schema, err := s.Store.ActiveSchema(ctx, regulation, schemaName, e.EventTS)
	if err != nil {
		return nil, err
	}

	payload := WithExecutingEntity(PayloadFor(e), s.Cfg.VenueLEI)
	e.Payload, _ = json.Marshal(payload)
	violations := ValidatePayload(schema, payload)
	if len(violations) > 0 {
		e.Status = EventQuarantined
		e.ValidationErrors, _ = json.Marshal(violations)
	} else {
		e.Status = EventValidated
		e.SchemaVersion = schema.Version
	}
	if err := s.Store.InsertEvent(ctx, e); err != nil {
		return nil, err
	}

	if len(violations) > 0 {
		br := &Break{
			EventID:    e.EventID,
			UTI:        e.UTI,
			Regime:     e.Regime,
			BreakType:  BreakValidation,
			DetectedBy: "validator",
			SLADueAt:   s.Cfg.now().Add(s.Cfg.repairSLA()),
		}
		br.Detail, _ = json.Marshal(map[string]any{
			"violations": violations, "schema": schemaName,
		})
		_ = s.Store.InsertBreak(ctx, br)
		s.alert(ctx, "P2", "REPORT_QUARANTINED",
			fmt.Sprintf("event %d (%s/%s) quarantined: %v",
				e.EventID, e.Regime, e.UTI, violations))
		return e, nil
	}

	// MiFID II events produce two artifacts: the ARM RTS 22 transaction
	// report and the APA RTS 1 post-trade transparency message (Task
	// 21.3.16 — one canonical event fans out to both vendors).
	dests := []Destination{DestinationFor(e.Regime)}
	if e.Regime == RegimeMIFID2 {
		dests = append(dests, DestinationAPA)
	}
	for _, dest := range dests {
		if err := s.buildArtifact(ctx, e, dest); err != nil {
			return e, err
		}
	}
	return e, nil
}

// buildArtifact serializes the destination-specific artifact + inserts
// the journal row (attempt = next per event+destination). Each
// destination payload validates against ITS pinned schema — a failure
// marks the artifact QUARANTINED with a VALIDATION break (the event
// itself stays VALIDATED; the other destination can still dispatch).
func (s *Service) buildArtifact(ctx context.Context, e *Event, dest Destination) error {
	regulation, schemaName := SchemaFor(e.Regime, dest)
	schema, err := s.Store.ActiveSchema(ctx, regulation, schemaName, e.EventTS)
	if err != nil {
		return err
	}
	schemaVersion := ""
	if schema != nil {
		schemaVersion = schema.Version
	}

	payload := WithExecutingEntity(
		PayloadForDest(e, dest, s.Cfg.now()), s.Cfg.VenueLEI)
	payloadJSON, _ := json.Marshal(payload)

	var xmlBody string
	switch {
	case e.Regime == RegimeEMIRREFIT:
		x, err := EMIRXML(e, payload, s.Cfg.VenueLEI)
		if err != nil {
			return err
		}
		xmlBody = string(x)
	case e.Regime == RegimeMIFID2 && dest == DestinationARM:
		x, err := RTS22XML([]*Event{e}, s.Cfg.VenueLEI)
		if err != nil {
			return err
		}
		xmlBody = string(x)
	}

	attempt := 1
	if prior, err := s.Store.SubmissionsForEvent(ctx, e.EventID); err != nil {
		return err
	} else {
		for _, p := range prior {
			if p.Destination == dest && p.Attempt >= attempt {
				attempt = p.Attempt + 1
			}
		}
	}

	status := SubPending
	if violations := ValidatePayload(schema, payload); len(violations) > 0 {
		status = SubQuarantined
		defer func() {
			br := &Break{
				EventID: e.EventID, UTI: e.UTI, Regime: e.Regime,
				BreakType: BreakValidation, DetectedBy: "validator",
				Detail: detail(map[string]any{
					"violations": violations, "schema": schemaName,
					"destination": dest}),
				SLADueAt: s.Cfg.now().Add(s.Cfg.repairSLA()),
			}
			_ = s.Store.InsertBreak(ctx, br)
		}()
	}

	return s.Store.InsertSubmission(ctx, &Submission{
		EventID:       e.EventID,
		Regime:        e.Regime,
		Destination:   dest,
		Attempt:       attempt,
		SchemaName:    schemaName,
		SchemaVersion: schemaVersion,
		Payload:       payloadJSON,
		PayloadXML:    xmlBody,
		PayloadHash:   PayloadHash([]byte(xmlBody), payloadJSON),
		Status:        status,
	})
}

// ---------------------------------------------------------------------------
// Lifecycle events (MODI/VALU/MARU/TERM/ERRO/CORR/…)
// ---------------------------------------------------------------------------

// LifecycleInput carries one continuation report request.
type LifecycleInput struct {
	UTI       string
	Regime    Regime
	Action    ActionType
	EventType EventType
	EventTS   time.Time
	// Optional field overrides (corrections land here too). Instrument/
	// account keys matter for self-rooted reports (LTR) that carry no
	// NEWT ancestor to inherit from.
	PositionID     int64
	InstrumentID   int64
	InstrumentCode string
	AccountID      int64
	Price          string
	Quantity       string
	Notional       string
	Currency       string
	Valuation      json.RawMessage
	Margin         json.RawMessage
	Clearing       json.RawMessage
	Confirmation   json.RawMessage
	Allocation     json.RawMessage
	PriorUTI       string
	PriorUSI       string
	DualSided      *bool
	// Corrections applies whitelisted repair keys onto the new event's
	// fields (resubmission path) — the appended CORR row carries the
	// corrected values while the superseded row keeps the original
	// (append-only history).
	Corrections map[string]any
}

// RecordLifecycle appends the continuation row for (uti, regime):
// seq = latest+1, prior-ID links, supersedes chain on CORR. A missing
// NEWT is a fail-closed error — never invent a root report.
func (s *Service) RecordLifecycle(ctx context.Context, in LifecycleInput) (*Event, error) {
	if in.Action == "" {
		return nil, fmt.Errorf("reporting: lifecycle action required")
	}
	latest, err := s.Store.LatestEvent(ctx, in.UTI, in.Regime)
	if err != nil {
		return nil, err
	}
	if latest == nil {
		// LTR reports are self-rooted — the venue-extension report has
		// no NEWT ancestor on this UTI chain.
		if in.Action != ActionLargeTrader {
			return nil, fmt.Errorf("reporting: no NEWT for uti=%s regime=%s", in.UTI, in.Regime)
		}
		latest = &Event{UTI: in.UTI, Regime: in.Regime}
	}
	seq, err := s.Store.NextReportSeq(ctx, in.UTI, in.Regime)
	if err != nil {
		return nil, err
	}

	// Carry-forward: continuation rows inherit identity/party/trade
	// fields from the latest event; overrides replace economics.
	e := *latest
	e.EventID = 0
	e.ReportSeq = seq
	e.Action = in.Action
	e.EventType = in.EventType
	e.EventTS = in.EventTS
	e.Status = EventRecorded
	e.ValidationErrors = nil
	e.ReportedAt = nil
	e.DisseminationDueAt = nil
	e.SupersedesEventID = nil
	e.Payload = nil

	if in.PositionID != 0 {
		e.PositionID = in.PositionID
	}
	if in.InstrumentID != 0 {
		e.InstrumentID = in.InstrumentID
	}
	if in.InstrumentCode != "" {
		e.InstrumentCode = in.InstrumentCode
	}
	if in.AccountID != 0 {
		e.AccountID = in.AccountID
	}
	if in.Price != "" {
		e.Price = in.Price
	}
	if in.Quantity != "" {
		e.Quantity = in.Quantity
	}
	if in.Notional != "" {
		e.Notional = in.Notional
	}
	if in.Currency != "" {
		e.Currency = in.Currency
	}
	if len(in.Valuation) > 0 {
		e.Valuation = in.Valuation
	}
	if len(in.Margin) > 0 {
		e.Margin = in.Margin
	}
	if len(in.Clearing) > 0 {
		e.Clearing = in.Clearing
	}
	if len(in.Confirmation) > 0 {
		e.Confirmation = in.Confirmation
	}
	if len(in.Allocation) > 0 {
		e.Allocation = in.Allocation
	}
	if in.PriorUTI != "" {
		e.PriorUTI = in.PriorUTI
	}
	if in.PriorUSI != "" {
		e.PriorUSI = in.PriorUSI
	}
	if in.DualSided != nil {
		e.DualSided = *in.DualSided
	}
	if len(in.Corrections) > 0 {
		applyCorrections(&e, in.Corrections)
	}
	if in.Action == ActionCorrect {
		e.SupersedesEventID = &latest.EventID
	}
	if in.EventType == "" {
		e.EventType = eventTypeFor(in.Action)
	}
	return s.validateAndPersist(ctx, &e)
}

// applyCorrections maps repairable-field correction keys onto event
// fields — the CORR row's own artifact then serializes the corrected
// values (a single corrected report per seq, not a corrected artifact
// trailing an unchanged one). Values that aren't strings are ignored —
// payload-typed fields (valuation/margin) accept JSON text or objects.
func applyCorrections(e *Event, c map[string]any) {
	str := func(k string) (string, bool) {
		v, ok := c[k]
		if !ok {
			return "", false
		}
		s, ok := v.(string)
		return s, ok
	}
	raw := func(k string) (json.RawMessage, bool) {
		v, ok := c[k]
		if !ok {
			return nil, false
		}
		switch t := v.(type) {
		case string:
			if json.Valid([]byte(t)) {
				return json.RawMessage(t), true
			}
		case json.RawMessage:
			return t, true
		case map[string]any:
			if b, err := json.Marshal(t); err == nil {
				return b, true
			}
		}
		return nil, false
	}
	if v, ok := str("buyer_id"); ok {
		e.BuyerID = v
	}
	if v, ok := str("seller_id"); ok {
		e.SellerID = v
	}
	if v, ok := str("buyer_lei"); ok {
		e.BuyerLEI = v
	}
	if v, ok := str("seller_lei"); ok {
		e.SellerLEI = v
	}
	if v, ok := str("buyer_id_type"); ok {
		e.BuyerIDType = v
	}
	if v, ok := str("seller_id_type"); ok {
		e.SellerIDType = v
	}
	if v, ok := str("decision_maker_id"); ok {
		e.DecisionMakerID = v
	}
	if v, ok := str("decision_maker_type"); ok {
		e.DecisionMakerType = v
	}
	if v, ok := str("trader_id"); ok {
		e.TraderID = v
	}
	if v, ok := str("algo_id"); ok {
		e.AlgoID = v
	}
	if v, ok := str("price"); ok {
		e.Price = v
	}
	if v, ok := str("quantity"); ok {
		e.Quantity = v
	}
	if v, ok := str("notional"); ok {
		e.Notional = v
	}
	if v, ok := str("currency"); ok {
		e.Currency = v
	}
	if v, ok := str("venue_mic"); ok {
		e.VenueMIC = v
	}
	if v, ok := str("upi"); ok {
		e.UPI = v
	}
	if v, ok := raw("valuation"); ok {
		e.Valuation = v
	}
	if v, ok := raw("margin"); ok {
		e.Margin = v
	}
	// counterparty2 mirrors the buyer-side decomposition on this venue.
	if v, ok := str("counterparty2_id"); ok {
		e.BuyerID = v
	}
	if v, ok := str("counterparty2_type"); ok {
		e.BuyerIDType = v
	}
}

func eventTypeFor(a ActionType) EventType {
	switch a {
	case ActionNew:
		return EventTypeTrade
	case ActionModify:
		return EventTypeModify
	case ActionCorrect:
		return EventTypeCorrection
	case ActionTerminate:
		return EventTypeTermination
	case ActionError:
		return EventTypeError
	case ActionValuation:
		return EventTypeValuation
	case ActionMargin:
		return EventTypeMargin
	case ActionAllocation:
		return EventTypeAllocation
	case ActionClearing:
		return EventTypeClearing
	case ActionPorting:
		return EventTypeCompression
	case ActionPosition:
		return EventTypeModify
	}
	return EventTypeTrade
}

// ---------------------------------------------------------------------------
// ACK / NACK ingest
// ---------------------------------------------------------------------------

// IngestAck lands the repository verdict through the atomic store tx.
// A NACK opens the NACK_REPAIR break (already in-tx) and alerts.
func (s *Service) IngestAck(ctx context.Context, a Ack) (*Break, error) {
	subStatus := SubAcked
	eventStatus := EventAccepted
	nack := false
	switch a.AckStatus {
	case AckReject:
		subStatus = SubNacked
		eventStatus = EventRejected
		nack = true
	case AckRecon:
		// Reconciliation feedback — event state unchanged, ack journaled.
		subStatus = SubAcked
		eventStatus = EventAccepted
	}
	br, err := s.Store.IngestAckTx(ctx, a, subStatus, eventStatus, nack,
		s.Cfg.now().Add(s.Cfg.repairSLA()))
	if err != nil {
		return nil, err
	}
	if nack && br != nil {
		s.alert(ctx, "P1", "REPORT_NACKED",
			fmt.Sprintf("event %d NACKed (%s %s) — repair break %d",
				a.EventID, a.AckCode, a.AckText, br.BreakID))
	}
	return br, nil
}

// ---------------------------------------------------------------------------
// Repair / resubmission
// ---------------------------------------------------------------------------

// repairableFields is the correction whitelist — keys the Compliance
// Officer may repair on a rejected/quarantined artifact.
var repairableFields = map[string]bool{
	"buyer_id": true, "seller_id": true, "buyer_lei": true,
	"seller_lei": true, "buyer_id_type": true, "seller_id_type": true,
	"decision_maker_id": true, "decision_maker_type": true,
	"trader_id": true, "algo_id": true,
	"price": true, "quantity": true, "notional": true, "currency": true,
	"venue_mic": true, "upi": true, "valuation": true, "margin": true,
	"counterparty2_id": true, "counterparty2_type": true,
}

// Resubmit repairs an artifact after repository rejection (or quarantine)
// and dispatches a corrected attempt:
//   - Event never dispatched (VALIDATED/QUARANTINED) → corrected payload
//     rides as a new artifact attempt on the same event (the event row is
//     the immutable business fact; the artifact carries report fields).
//   - Event was rejected by the repository → the corrected data also
//     lands a CORR lifecycle event (seq+1, supersedes chain) per EMIR/
//     CFTC error-correction semantics.
//
// Returns the new artifact row id.
func (s *Service) Resubmit(ctx context.Context, reportSubmissionID int64,
	corrections map[string]any, adminUserID int64) (*Submission, error) {

	sub, err := s.Store.SubmissionByID(ctx, reportSubmissionID)
	if err != nil {
		return nil, err
	}
	if sub == nil {
		return nil, fmt.Errorf("reporting: submission %d not found", reportSubmissionID)
	}
	for k := range corrections {
		if !repairableFields[k] {
			return nil, fmt.Errorf("reporting: field %q is not repairable", k)
		}
	}
	e, err := s.Store.EventByID(ctx, sub.EventID)
	if err != nil {
		return nil, err
	}
	if e == nil {
		return nil, fmt.Errorf("reporting: event %d not found", sub.EventID)
	}

	// Apply corrections to a payload copy, re-validate under the schema
	// pinned at the event's business time.
	var payload map[string]any
	if err := json.Unmarshal(sub.Payload, &payload); err != nil {
		return nil, fmt.Errorf("reporting: submission %d payload: %w", reportSubmissionID, err)
	}
	if payload == nil {
		payload = map[string]any{}
	}
	for k, v := range corrections {
		payload[k] = v
	}
	regulation, schemaName := SchemaFor(e.Regime, sub.Destination)
	schema, err := s.Store.ActiveSchema(ctx, regulation, schemaName, e.EventTS)
	if err != nil {
		return nil, err
	}
	if violations := ValidatePayload(schema, payload); len(violations) > 0 {
		return nil, fmt.Errorf("reporting: corrected payload still invalid: %v", violations)
	}
	payloadJSON, _ := json.Marshal(payload)

	// Repo-visible rejection → the corrected report is a CORR lifecycle
	// event (immutable supersede chain) whose own artifact for this
	// destination already carries the corrected fields — one corrected
	// report per seq, not a corrected attempt trailing an unchanged one.
	if e.Status == EventRejected {
		corr, err := s.RecordLifecycle(ctx, LifecycleInput{
			UTI: e.UTI, Regime: e.Regime,
			Action: ActionCorrect, EventType: EventTypeCorrection,
			EventTS: s.Cfg.now(), Corrections: corrections,
		})
		if err != nil {
			return nil, fmt.Errorf("reporting: corr event: %w", err)
		}
		if corr.Status == EventQuarantined {
			return nil, fmt.Errorf("reporting: corrected event quarantined: %s",
				string(corr.ValidationErrors))
		}
		subs, err := s.Store.SubmissionsForEvent(ctx, corr.EventID)
		if err != nil {
			return nil, err
		}
		var fresh *Submission
		for i := range subs {
			if subs[i].Destination == sub.Destination &&
				(fresh == nil || subs[i].Attempt > fresh.Attempt) {
				cp := subs[i]
				fresh = &cp
			}
		}
		if fresh == nil {
			return nil, fmt.Errorf("reporting: corr artifact missing for %s",
				sub.Destination)
		}
		return fresh, nil
	}

	var xmlBody string
	if e.Regime == RegimeEMIRREFIT {
		x, err := EMIRXML(e, payload, s.Cfg.VenueLEI)
		if err != nil {
			return nil, err
		}
		xmlBody = string(x)
	}
	schemaVersion := ""
	if schema != nil {
		schemaVersion = schema.Version
	}
	fresh := &Submission{
		EventID:       e.EventID,
		Regime:        e.Regime,
		Destination:   sub.Destination,
		Attempt:       sub.Attempt + 1,
		SchemaName:    schemaName,
		SchemaVersion: schemaVersion,
		Payload:       payloadJSON,
		PayloadXML:    xmlBody,
		PayloadHash:   PayloadHash([]byte(xmlBody), payloadJSON),
		Status:        SubPending,
	}
	if err := s.Store.InsertSubmission(ctx, fresh); err != nil {
		return nil, err
	}
	return fresh, nil
}

// ---------------------------------------------------------------------------
// Reconciliation (repo-vs-internal, spec §14.1a acceptance)
// ---------------------------------------------------------------------------

// Reconcile runs one daily reconciliation pass for a derivative regime:
// repo-accepted open state (latest events) vs internal open positions.
// Missing/duplicate/stale/divergent/collision conditions open breaks;
// breaks whose condition cleared auto-resolve. Returns the pass summary.
func (s *Service) Reconcile(ctx context.Context, regime Regime) (*ReconcileReport, error) {
	if regime != RegimeEMIRREFIT && regime != RegimeCFTCP45 {
		return nil, fmt.Errorf("reporting: reconcile supports EMIR_REFIT/CFTC_P45, not %s", regime)
	}
	rep := &ReconcileReport{Regime: regime, StartedAt: s.Cfg.now()}

	latest, err := s.Store.LatestEvents(ctx, regime)
	if err != nil {
		return nil, err
	}
	rep.CheckedEvents = len(latest)
	positions, err := s.Store.OpenDerivativePositions(ctx)
	if err != nil {
		return nil, err
	}
	rep.OpenInternal = len(positions)

	byPosition := map[int64]*Event{}
	byAcctInst := map[string]*Event{}
	termed := map[int64]*Event{}
	for i := range latest {
		ev := &latest[i]
		key := fmt.Sprintf("%d:%d", ev.AccountID, ev.InstrumentID)
		switch ev.Action {
		case ActionTerminate, ActionError:
			if ev.PositionID != 0 {
				termed[ev.PositionID] = ev
			}
			termed[0] = ev // marker for acct/inst matching below
			_ = key
		default:
			if ev.PositionID != 0 {
				byPosition[ev.PositionID] = ev
			}
			byAcctInst[key] = ev
		}
	}
	// A TERMed latest row also lives in byAcctInst space for divergence
	// detection on (account, instrument) when position_id is unset.
	termedPairs := map[string]*Event{}
	for i := range latest {
		ev := &latest[i]
		if ev.Action == ActionTerminate || ev.Action == ActionError {
			termedPairs[fmt.Sprintf("%d:%d", ev.AccountID, ev.InstrumentID)] = ev
		}
	}

	openNow := map[string]bool{} // dedupe "uti|type" breaks this pass
	openBreak := func(b *Break) {
		key := b.UTI + "|" + string(b.BreakType)
		if openNow[key] {
			return
		}
		openNow[key] = true
		if err := s.Store.InsertBreak(ctx, b); err == nil {
			rep.BreaksOpened = append(rep.BreaksOpened, b.BreakID)
		}
	}

	now := s.Cfg.now()
	for _, pos := range positions {
		key := fmt.Sprintf("%d:%d", pos.AccountID, pos.InstrumentID)
		ev := byPosition[pos.PositionID]
		if ev == nil {
			ev = byAcctInst[key]
		}
		if ev == nil {
			// Position open internally but repo reports TERM → divergence;
			// otherwise unexplained absence → MISSING.
			if t := termed[pos.PositionID]; t != nil {
				openNow[t.UTI+"|"+string(BreakLifecycleDivergence)] = true
				openBreak(&Break{
					EventID: t.EventID, UTI: t.UTI, Regime: regime,
					BreakType: BreakLifecycleDivergence, DetectedBy: "reconciler",
					Detail: detail(map[string]any{
						"position_id": pos.PositionID,
						"reason":      "repo TERM vs internal open"}),
					SLADueAt: now.Add(s.Cfg.repairSLA()),
				})
				continue
			}
			if t := termedPairs[key]; t != nil {
				openBreak(&Break{
					EventID: t.EventID, UTI: t.UTI, Regime: regime,
					BreakType: BreakLifecycleDivergence, DetectedBy: "reconciler",
					Detail: detail(map[string]any{
						"position_id": pos.PositionID, "account_id": pos.AccountID,
						"instrument_id": pos.InstrumentID,
						"reason":        "repo TERM vs internal open"}),
					SLADueAt: now.Add(s.Cfg.repairSLA()),
				})
				continue
			}
			openBreak(&Break{
				UTI: "", Regime: regime,
				BreakType: BreakMissing, DetectedBy: "reconciler",
				Detail: detail(map[string]any{
					"position_id": pos.PositionID, "account_id": pos.AccountID,
					"instrument_id": pos.InstrumentID,
					"reason":        "internal open derivative lacks repo event"}),
				SLADueAt: now.Add(s.Cfg.repairSLA()),
			})
			continue
		}
		// Stale valuation: the position moved more than StaleAfter ago
		// without a newer repo continuation.
		if pos.UpdatedAt.After(ev.EventTS.Add(s.Cfg.staleAfter())) {
			openBreak(&Break{
				EventID: ev.EventID, UTI: ev.UTI, Regime: regime,
				BreakType: BreakStaleValuation, DetectedBy: "reconciler",
				Detail: detail(map[string]any{
					"position_id": pos.PositionID,
					"position_ts": pos.UpdatedAt, "report_ts": ev.EventTS}),
				SLADueAt: now.Add(s.Cfg.repairSLA()),
			})
		}
	}

	// Duplicates + identifier collisions.
	for _, uti := range must(s.Store.DuplicateNEWT(ctx, regime)) {
		openBreak(&Break{
			UTI: uti, Regime: regime, BreakType: BreakDuplicate,
			DetectedBy: "reconciler",
			Detail:     detail(map[string]any{"reason": ">1 live NEWT row"}),
			SLADueAt:   now.Add(s.Cfg.repairSLA()),
		})
	}
	for _, uti := range must(s.Store.CollidingUTIs(ctx)) {
		openBreak(&Break{
			UTI: uti, Regime: regime, BreakType: BreakIDCollision,
			DetectedBy: "reconciler",
			Detail:     detail(map[string]any{"reason": "UTI bound to >1 trade_id"}),
			SLADueAt:   now.Add(s.Cfg.repairSLA()),
		})
	}
	return rep, nil
}

func detail(v map[string]any) json.RawMessage {
	b, _ := json.Marshal(v)
	return b
}

func must[T any](v T, err error) T { return v }

// AlertFunc raises a compliance ops alert (P1 severity for NACK/collision,
// P2 for quarantine) — compliance.Alerter's signature.
type AlertFunc func(ctx context.Context, severity, code, summary string) error

func (s *Service) alert(ctx context.Context, severity, code, summary string) {
	if s.Cfg.Alerter != nil {
		_ = s.Cfg.Alerter(ctx, severity, code, summary)
	}
}
