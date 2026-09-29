// Return-code mapping — Phase-11 Task 11.3.11(a).
//
// Every rail delivers a rejected/returned payment with a rail-native
// reason code: ISO 20022 pacs.004 RsnCd values (SEPA, FedNow, CHAPS,
// TARGET2, and the pacs.* legs of SWIFT flows), SWIFT MT narrative codes
// (MT199/MT299 field-79 tokens), and ACH NACHA R-codes. This file holds
// the full static tables — every known code maps to a canonical internal
// reason, the funding_transactions status the return drives, and the
// §23 surface code the API reports.
//
// Spec-pinned mappings (Task 11.3.11): AC01 → SETTLEMENT_ACCOUNT_CLOSED,
// AM04 → SETTLEMENT_RAIL_REJECTED, RR04 → SETTLEMENT_RAIL_REJECTED
// (NOT SANCTIONS_SERVICE_UNAVAILABLE — that code is reserved for
// provider outages). Any code absent from the tables is a fail-closed
// quarantine: rail_payments records the raw code, the linked funding row
// parks at PENDING_REVIEW, and a P1 ops alert fires — never a silent
// drop, never a guessed classification.
package funding

import (
	"context"
	"fmt"
	"strings"

	"exchange/internal/ledger"
)

// Canonical internal return reasons (the ReturnMapping.Canonical domain
// — stable vocabulary independent of any rail's wire format).
const (
	RetCanonAccountClosed  = "ACCOUNT_CLOSED"
	RetCanonAccountInvalid = "ACCOUNT_INVALID"
	RetCanonInsufficient   = "INSUFFICIENT_FUNDS"
	RetCanonDuplicate      = "DUPLICATE"
	RetCanonUnauthorized   = "UNAUTHORIZED"
	RetCanonRegulatory     = "REGULATORY_HOLD"
	RetCanonFormat         = "INVALID_FORMAT"
	RetCanonAmount         = "INVALID_AMOUNT"
	RetCanonDeceased       = "BENEFICIARY_DECEASED"
	RetCanonAccountFrozen  = "ACCOUNT_FROZEN"
	RetCanonCutoff         = "CUT_OFF_MISSED"
	RetCanonRejected       = "RAIL_REJECTED"
	RetCanonMisrouted      = "MISROUTED"
	RetCanonReturnAccepted = "RETURN_ACCEPTED"
	RetCanonUnknown        = "UNMAPPED"
)

// ReturnMapping is one row of the return-code tables.
type ReturnMapping struct {
	RawCode       string `json:"raw_code"`
	Canonical     string `json:"canonical"`
	SurfaceCode   string `json:"surface_code"`   // §23 code emitted API-side
	FundingStatus string `json:"funding_status"` // status applied to linked funding row
	Quarantine    bool   `json:"quarantine"`     // true → P1 alert + manual triage
	Alert         bool   `json:"alert"`          // compliance/ops attention flag
	Retryable     bool   `json:"retryable"`      // a corrected resubmit may succeed
	Description   string `json:"description"`
}

// isoReturnTable is the shared pacs.004/pacs.002 RsnCd map — SEPA,
// FedNow, CHAPS, TARGET2 and ISO-flavoured SWIFT returns all draw on it.
var isoReturnTable = map[string]ReturnMapping{
	"AC01": {RawCode: "AC01", Canonical: RetCanonAccountInvalid, SurfaceCode: "SETTLEMENT_ACCOUNT_CLOSED", FundingStatus: FundingFailed, Description: "incorrect account number"},
	"AC03": {RawCode: "AC03", Canonical: RetCanonAccountInvalid, SurfaceCode: "SETTLEMENT_ACCOUNT_CLOSED", FundingStatus: FundingFailed, Retryable: true, Description: "invalid creditor account number"},
	"AC04": {RawCode: "AC04", Canonical: RetCanonAccountClosed, SurfaceCode: "SETTLEMENT_ACCOUNT_CLOSED", FundingStatus: FundingFailed, Description: "closed account"},
	"AC06": {RawCode: "AC06", Canonical: RetCanonAccountFrozen, SurfaceCode: "SETTLEMENT_RAIL_REJECTED", FundingStatus: FundingFailed, Alert: true, Description: "blocked account"},
	"AG01": {RawCode: "AG01", Canonical: RetCanonUnauthorized, SurfaceCode: "SETTLEMENT_RAIL_REJECTED", FundingStatus: FundingFailed, Alert: true, Description: "transaction forbidden"},
	"AG02": {RawCode: "AG02", Canonical: RetCanonFormat, SurfaceCode: "SETTLEMENT_RAIL_REJECTED", FundingStatus: FundingFailed, Retryable: true, Description: "invalid bank operation code"},
	"AM01": {RawCode: "AM01", Canonical: RetCanonAmount, SurfaceCode: "SETTLEMENT_RAIL_REJECTED", FundingStatus: FundingFailed, Description: "zero amount"},
	"AM02": {RawCode: "AM02", Canonical: RetCanonAmount, SurfaceCode: "SETTLEMENT_RAIL_REJECTED", FundingStatus: FundingFailed, Retryable: true, Description: "amount exceeds limit"},
	"AM03": {RawCode: "AM03", Canonical: RetCanonFormat, SurfaceCode: "SETTLEMENT_RAIL_REJECTED", FundingStatus: FundingFailed, Description: "currency not allowed"},
	"AM04": {RawCode: "AM04", Canonical: RetCanonInsufficient, SurfaceCode: "SETTLEMENT_RAIL_REJECTED", FundingStatus: FundingFailed, Description: "insufficient funds"},
	"AM05": {RawCode: "AM05", Canonical: RetCanonDuplicate, SurfaceCode: "SETTLEMENT_RAIL_REJECTED", FundingStatus: FundingFailed, Description: "duplicate payment"},
	"AM07": {RawCode: "AM07", Canonical: RetCanonAccountFrozen, SurfaceCode: "SETTLEMENT_RAIL_REJECTED", FundingStatus: FundingFailed, Alert: true, Description: "blocked amount"},
	"AM09": {RawCode: "AM09", Canonical: RetCanonAmount, SurfaceCode: "SETTLEMENT_RAIL_REJECTED", FundingStatus: FundingFailed, Description: "wrong amount"},
	"AM10": {RawCode: "AM10", Canonical: RetCanonFormat, SurfaceCode: "SETTLEMENT_RAIL_REJECTED", FundingStatus: FundingFailed, Retryable: true, Description: "invalid control sum"},
	"BE01": {RawCode: "BE01", Canonical: RetCanonFormat, SurfaceCode: "SETTLEMENT_RAIL_REJECTED", FundingStatus: FundingFailed, Description: "inconsistent with end customer"},
	"BE04": {RawCode: "BE04", Canonical: RetCanonFormat, SurfaceCode: "SETTLEMENT_RAIL_REJECTED", FundingStatus: FundingFailed, Retryable: true, Description: "missing creditor address"},
	"BE05": {RawCode: "BE05", Canonical: RetCanonUnauthorized, SurfaceCode: "SETTLEMENT_RAIL_REJECTED", FundingStatus: FundingFailed, Alert: true, Description: "unrecognised initiating party"},
	"BE06": {RawCode: "BE06", Canonical: RetCanonAccountInvalid, SurfaceCode: "SETTLEMENT_ACCOUNT_CLOSED", FundingStatus: FundingFailed, Description: "unknown end customer"},
	"BE07": {RawCode: "BE07", Canonical: RetCanonFormat, SurfaceCode: "SETTLEMENT_RAIL_REJECTED", FundingStatus: FundingFailed, Retryable: true, Description: "missing debtor address"},
	"BE08": {RawCode: "BE08", Canonical: RetCanonRejected, SurfaceCode: "SETTLEMENT_RAIL_REJECTED", FundingStatus: FundingFailed, Retryable: true, Description: "bank error"},
	"BE10": {RawCode: "BE10", Canonical: RetCanonFormat, SurfaceCode: "SETTLEMENT_RAIL_REJECTED", FundingStatus: FundingFailed, Retryable: true, Description: "invalid debtor agent identification"},
	"BE11": {RawCode: "BE11", Canonical: RetCanonFormat, SurfaceCode: "SETTLEMENT_RAIL_REJECTED", FundingStatus: FundingFailed, Retryable: true, Description: "invalid creditor agent identification"},
	"BE13": {RawCode: "BE13", Canonical: RetCanonFormat, SurfaceCode: "SETTLEMENT_RAIL_REJECTED", FundingStatus: FundingFailed, Description: "unknown debtor country"},
	"BE14": {RawCode: "BE14", Canonical: RetCanonFormat, SurfaceCode: "SETTLEMENT_RAIL_REJECTED", FundingStatus: FundingFailed, Description: "unknown creditor country"},
	"BE15": {RawCode: "BE15", Canonical: RetCanonAccountClosed, SurfaceCode: "SETTLEMENT_ACCOUNT_CLOSED", FundingStatus: FundingFailed, Description: "not an active account"},
	"BE16": {RawCode: "BE16", Canonical: RetCanonFormat, SurfaceCode: "SETTLEMENT_RAIL_REJECTED", FundingStatus: FundingFailed, Retryable: true, Description: "invalid debtor identification"},
	"BE17": {RawCode: "BE17", Canonical: RetCanonFormat, SurfaceCode: "SETTLEMENT_RAIL_REJECTED", FundingStatus: FundingFailed, Retryable: true, Description: "invalid creditor identification"},
	"BE19": {RawCode: "BE19", Canonical: RetCanonFormat, SurfaceCode: "SETTLEMENT_RAIL_REJECTED", FundingStatus: FundingFailed, Retryable: true, Description: "invalid charge bearer"},
	"BE20": {RawCode: "BE20", Canonical: RetCanonFormat, SurfaceCode: "SETTLEMENT_RAIL_REJECTED", FundingStatus: FundingFailed, Retryable: true, Description: "invalid name length"},
	"CNOR": {RawCode: "CNOR", Canonical: RetCanonRejected, SurfaceCode: "SETTLEMENT_RAIL_REJECTED", FundingStatus: FundingFailed, Description: "creditor bank not registered"},
	"DNOR": {RawCode: "DNOR", Canonical: RetCanonRejected, SurfaceCode: "SETTLEMENT_RAIL_REJECTED", FundingStatus: FundingFailed, Description: "debtor bank not registered"},
	"DT01": {RawCode: "DT01", Canonical: RetCanonFormat, SurfaceCode: "SETTLEMENT_RAIL_REJECTED", FundingStatus: FundingFailed, Retryable: true, Description: "invalid date"},
	"DT05": {RawCode: "DT05", Canonical: RetCanonFormat, SurfaceCode: "SETTLEMENT_RAIL_REJECTED", FundingStatus: FundingFailed, Retryable: true, Description: "invalid creation date"},
	"FF01": {RawCode: "FF01", Canonical: RetCanonFormat, SurfaceCode: "SETTLEMENT_RAIL_REJECTED", FundingStatus: FundingFailed, Retryable: true, Description: "invalid file format"},
	"FOCR": {RawCode: "FOCR", Canonical: RetCanonReturnAccepted, SurfaceCode: "SETTLEMENT_RAIL_REJECTED", FundingStatus: FundingFailed, Description: "return following cancellation request"},
	"MD07": {RawCode: "MD07", Canonical: RetCanonDeceased, SurfaceCode: "SETTLEMENT_RAIL_REJECTED", FundingStatus: FundingFailed, Alert: true, Description: "end customer deceased"},
	"MS02": {RawCode: "MS02", Canonical: RetCanonUnknown, SurfaceCode: "SETTLEMENT_RAIL_REJECTED", FundingStatus: FundingPendingReview, Quarantine: true, Alert: true, Description: "unspecified reason (customer-generated)"},
	"MS03": {RawCode: "MS03", Canonical: RetCanonUnknown, SurfaceCode: "SETTLEMENT_RAIL_REJECTED", FundingStatus: FundingPendingReview, Quarantine: true, Alert: true, Description: "unspecified reason (agent-generated)"},
	"NARR": {RawCode: "NARR", Canonical: RetCanonUnknown, SurfaceCode: "SETTLEMENT_RAIL_REJECTED", FundingStatus: FundingPendingReview, Quarantine: true, Alert: true, Description: "narrative — reason in free text"},
	"RC01": {RawCode: "RC01", Canonical: RetCanonFormat, SurfaceCode: "SETTLEMENT_RAIL_REJECTED", FundingStatus: FundingFailed, Retryable: true, Description: "bank identifier incorrect"},
	"RR01": {RawCode: "RR01", Canonical: RetCanonRegulatory, SurfaceCode: "SETTLEMENT_RAIL_REJECTED", FundingStatus: FundingFailed, Retryable: true, Description: "missing debtor account or identification"},
	"RR02": {RawCode: "RR02", Canonical: RetCanonRegulatory, SurfaceCode: "SETTLEMENT_RAIL_REJECTED", FundingStatus: FundingFailed, Retryable: true, Description: "missing debtor name or address"},
	"RR03": {RawCode: "RR03", Canonical: RetCanonRegulatory, SurfaceCode: "SETTLEMENT_RAIL_REJECTED", FundingStatus: FundingFailed, Retryable: true, Description: "missing creditor name or address"},
	"RR04": {RawCode: "RR04", Canonical: RetCanonRegulatory, SurfaceCode: "SETTLEMENT_RAIL_REJECTED", FundingStatus: FundingFailed, Alert: true, Description: "regulatory reason"},
	"RR05": {RawCode: "RR05", Canonical: RetCanonRegulatory, SurfaceCode: "SETTLEMENT_RAIL_REJECTED", FundingStatus: FundingFailed, Retryable: true, Description: "regulatory information invalid"},
	"RR06": {RawCode: "RR06", Canonical: RetCanonRegulatory, SurfaceCode: "SETTLEMENT_RAIL_REJECTED", FundingStatus: FundingFailed, Retryable: true, Description: "regulatory information invalid"},
	"RR07": {RawCode: "RR07", Canonical: RetCanonRegulatory, SurfaceCode: "SETTLEMENT_RAIL_REJECTED", FundingStatus: FundingFailed, Retryable: true, Description: "regulatory information invalid"},
	"RR08": {RawCode: "RR08", Canonical: RetCanonRegulatory, SurfaceCode: "SETTLEMENT_RAIL_REJECTED", FundingStatus: FundingFailed, Retryable: true, Description: "regulatory information invalid"},
	"RR09": {RawCode: "RR09", Canonical: RetCanonRegulatory, SurfaceCode: "SETTLEMENT_RAIL_REJECTED", FundingStatus: FundingFailed, Retryable: true, Description: "regulatory information invalid"},
	"RR10": {RawCode: "RR10", Canonical: RetCanonRegulatory, SurfaceCode: "SETTLEMENT_RAIL_REJECTED", FundingStatus: FundingFailed, Retryable: true, Description: "regulatory information invalid"},
	"RR11": {RawCode: "RR11", Canonical: RetCanonRegulatory, SurfaceCode: "SETTLEMENT_RAIL_REJECTED", FundingStatus: FundingFailed, Retryable: true, Description: "regulatory information invalid"},
	"RR12": {RawCode: "RR12", Canonical: RetCanonRegulatory, SurfaceCode: "SETTLEMENT_RAIL_REJECTED", FundingStatus: FundingFailed, Retryable: true, Description: "regulatory information invalid"},
	"TM01": {RawCode: "TM01", Canonical: RetCanonCutoff, SurfaceCode: "RAIL_CUTOFF_EXCEEDED", FundingStatus: FundingFailed, Retryable: true, Description: "instruction received after cut-off"},
}

// achReturnTable is the NACHA R-code map (USD rail — keyed separately so
// an "R04" never collides with ISO "RR04").
var achReturnTable = map[string]ReturnMapping{
	"R01": {RawCode: "R01", Canonical: RetCanonInsufficient, SurfaceCode: "SETTLEMENT_RAIL_REJECTED", FundingStatus: FundingFailed, Description: "insufficient funds"},
	"R02": {RawCode: "R02", Canonical: RetCanonAccountClosed, SurfaceCode: "SETTLEMENT_ACCOUNT_CLOSED", FundingStatus: FundingFailed, Description: "account closed"},
	"R03": {RawCode: "R03", Canonical: RetCanonAccountClosed, SurfaceCode: "SETTLEMENT_ACCOUNT_CLOSED", FundingStatus: FundingFailed, Description: "no account / unable to locate"},
	"R04": {RawCode: "R04", Canonical: RetCanonAccountInvalid, SurfaceCode: "SETTLEMENT_ACCOUNT_CLOSED", FundingStatus: FundingFailed, Description: "invalid account number"},
	"R05": {RawCode: "R05", Canonical: RetCanonUnauthorized, SurfaceCode: "SETTLEMENT_RAIL_REJECTED", FundingStatus: FundingFailed, Alert: true, Description: "unauthorized debit to consumer account"},
	"R06": {RawCode: "R06", Canonical: RetCanonRejected, SurfaceCode: "SETTLEMENT_RAIL_REJECTED", FundingStatus: FundingFailed, Description: "returned per ODFI request"},
	"R07": {RawCode: "R07", Canonical: RetCanonUnauthorized, SurfaceCode: "SETTLEMENT_RAIL_REJECTED", FundingStatus: FundingFailed, Alert: true, Description: "authorization revoked by customer"},
	"R08": {RawCode: "R08", Canonical: RetCanonRejected, SurfaceCode: "SETTLEMENT_RAIL_REJECTED", FundingStatus: FundingFailed, Description: "payment stopped"},
	"R09": {RawCode: "R09", Canonical: RetCanonInsufficient, SurfaceCode: "SETTLEMENT_RAIL_REJECTED", FundingStatus: FundingFailed, Description: "uncollected funds"},
	"R10": {RawCode: "R10", Canonical: RetCanonUnauthorized, SurfaceCode: "SETTLEMENT_RAIL_REJECTED", FundingStatus: FundingFailed, Alert: true, Description: "customer advises not authorized"},
	"R11": {RawCode: "R11", Canonical: RetCanonRejected, SurfaceCode: "SETTLEMENT_RAIL_REJECTED", FundingStatus: FundingFailed, Description: "check truncation entry return"},
	"R12": {RawCode: "R12", Canonical: RetCanonAccountClosed, SurfaceCode: "SETTLEMENT_ACCOUNT_CLOSED", FundingStatus: FundingFailed, Description: "account sold to another DFI"},
	"R13": {RawCode: "R13", Canonical: RetCanonFormat, SurfaceCode: "SETTLEMENT_RAIL_REJECTED", FundingStatus: FundingFailed, Retryable: true, Description: "invalid ACH routing number"},
	"R14": {RawCode: "R14", Canonical: RetCanonDeceased, SurfaceCode: "SETTLEMENT_RAIL_REJECTED", FundingStatus: FundingFailed, Alert: true, Description: "representative payee deceased"},
	"R15": {RawCode: "R15", Canonical: RetCanonDeceased, SurfaceCode: "SETTLEMENT_RAIL_REJECTED", FundingStatus: FundingFailed, Alert: true, Description: "beneficiary deceased"},
	"R16": {RawCode: "R16", Canonical: RetCanonAccountFrozen, SurfaceCode: "SETTLEMENT_RAIL_REJECTED", FundingStatus: FundingFailed, Alert: true, Description: "account frozen / returned per OFAC"},
	"R17": {RawCode: "R17", Canonical: RetCanonFormat, SurfaceCode: "SETTLEMENT_RAIL_REJECTED", FundingStatus: FundingFailed, Retryable: true, Description: "file record edit criteria"},
	"R20": {RawCode: "R20", Canonical: RetCanonFormat, SurfaceCode: "SETTLEMENT_RAIL_REJECTED", FundingStatus: FundingFailed, Description: "non-transaction account"},
	"R21": {RawCode: "R21", Canonical: RetCanonFormat, SurfaceCode: "SETTLEMENT_RAIL_REJECTED", FundingStatus: FundingFailed, Retryable: true, Description: "invalid company identification"},
	"R22": {RawCode: "R22", Canonical: RetCanonFormat, SurfaceCode: "SETTLEMENT_RAIL_REJECTED", FundingStatus: FundingFailed, Retryable: true, Description: "invalid individual ID number"},
	"R23": {RawCode: "R23", Canonical: RetCanonRejected, SurfaceCode: "SETTLEMENT_RAIL_REJECTED", FundingStatus: FundingFailed, Description: "credit entry refused by receiver"},
	"R24": {RawCode: "R24", Canonical: RetCanonDuplicate, SurfaceCode: "SETTLEMENT_RAIL_REJECTED", FundingStatus: FundingFailed, Description: "duplicate entry"},
	"R29": {RawCode: "R29", Canonical: RetCanonUnauthorized, SurfaceCode: "SETTLEMENT_RAIL_REJECTED", FundingStatus: FundingFailed, Alert: true, Description: "corporate customer advises not authorized"},
	"R31": {RawCode: "R31", Canonical: RetCanonRejected, SurfaceCode: "SETTLEMENT_RAIL_REJECTED", FundingStatus: FundingFailed, Retryable: true, Description: "permissible return entry"},
	"R33": {RawCode: "R33", Canonical: RetCanonRejected, SurfaceCode: "SETTLEMENT_RAIL_REJECTED", FundingStatus: FundingFailed, Description: "return of XCK entry"},
	"R34": {RawCode: "R34", Canonical: RetCanonRejected, SurfaceCode: "SETTLEMENT_RAIL_REJECTED", FundingStatus: FundingFailed, Description: "limited participation DFI"},
	"R35": {RawCode: "R35", Canonical: RetCanonRejected, SurfaceCode: "SETTLEMENT_RAIL_REJECTED", FundingStatus: FundingFailed, Description: "return of improper debit entry"},
	"R36": {RawCode: "R36", Canonical: RetCanonRejected, SurfaceCode: "SETTLEMENT_RAIL_REJECTED", FundingStatus: FundingFailed, Description: "return of improper credit entry"},
	"R37": {RawCode: "R37", Canonical: RetCanonRejected, SurfaceCode: "SETTLEMENT_RAIL_REJECTED", FundingStatus: FundingFailed, Description: "source document presented for payment"},
	"R38": {RawCode: "R38", Canonical: RetCanonRejected, SurfaceCode: "SETTLEMENT_RAIL_REJECTED", FundingStatus: FundingFailed, Description: "stop payment on source document"},
	"R39": {RawCode: "R39", Canonical: RetCanonFormat, SurfaceCode: "SETTLEMENT_RAIL_REJECTED", FundingStatus: FundingFailed, Description: "improper source document"},
	"R40": {RawCode: "R40", Canonical: RetCanonRejected, SurfaceCode: "SETTLEMENT_RAIL_REJECTED", FundingStatus: FundingFailed, Description: "non-participant in ENR program"},
	"R45": {RawCode: "R45", Canonical: RetCanonFormat, SurfaceCode: "SETTLEMENT_RAIL_REJECTED", FundingStatus: FundingFailed, Description: "invalid individual name / company name"},
	"R61": {RawCode: "R61", Canonical: RetCanonMisrouted, SurfaceCode: "SETTLEMENT_RAIL_REJECTED", FundingStatus: FundingPendingReview, Quarantine: true, Alert: true, Description: "misrouted return"},
	"R62": {RawCode: "R62", Canonical: RetCanonMisrouted, SurfaceCode: "SETTLEMENT_RAIL_REJECTED", FundingStatus: FundingPendingReview, Quarantine: true, Alert: true, Description: "incorrect trace number"},
	"R68": {RawCode: "R68", Canonical: RetCanonRejected, SurfaceCode: "SETTLEMENT_RAIL_REJECTED", FundingStatus: FundingFailed, Alert: true, Description: "untimely return"},
	"R69": {RawCode: "R69", Canonical: RetCanonFormat, SurfaceCode: "SETTLEMENT_RAIL_REJECTED", FundingStatus: FundingFailed, Retryable: true, Description: "field error"},
	"R70": {RawCode: "R70", Canonical: RetCanonRejected, SurfaceCode: "SETTLEMENT_RAIL_REJECTED", FundingStatus: FundingFailed, Description: "permissible return not accepted"},
	"R75": {RawCode: "R75", Canonical: RetCanonRejected, SurfaceCode: "SETTLEMENT_RAIL_REJECTED", FundingStatus: FundingFailed, Description: "original return not a duplicate"},
	"R80": {RawCode: "R80", Canonical: RetCanonFormat, SurfaceCode: "SETTLEMENT_RAIL_REJECTED", FundingStatus: FundingFailed, Description: "cross-border payment coding error"},
}

// swiftReturnTable covers the narrative codes seen in MT199/MT299 field
// 79 return narratives that have no ISO equivalent; shared ISO codes
// resolve via the fallback in MapReturnCode.
var swiftReturnTable = map[string]ReturnMapping{
	"ACCTCLOSED":    {RawCode: "ACCTCLOSED", Canonical: RetCanonAccountClosed, SurfaceCode: "SETTLEMENT_ACCOUNT_CLOSED", FundingStatus: FundingFailed, Description: "beneficiary account closed"},
	"NOSTROVOSTRO":  {RawCode: "NOSTROVOSTRO", Canonical: RetCanonRejected, SurfaceCode: "SETTLEMENT_RAIL_REJECTED", FundingStatus: FundingFailed, Description: "nostro/vostro accounting issue"},
	"UNABLETOAPPLY": {RawCode: "UNABLETOAPPLY", Canonical: RetCanonRejected, SurfaceCode: "SETTLEMENT_RAIL_REJECTED", FundingStatus: FundingFailed, Description: "unable to apply funds"},
	"REGHOLD":       {RawCode: "REGHOLD", Canonical: RetCanonRegulatory, SurfaceCode: "SETTLEMENT_RAIL_REJECTED", FundingStatus: FundingFailed, Alert: true, Description: "regulatory hold (narrative)"},
}

// MapReturnCode resolves a rail-native code to its canonical mapping.
// Lookup order: rail-specific table → shared ISO table → fail-closed
// unknown mapping (Quarantine+Alert — the task's "no silent unknown").
func MapReturnCode(rail RailID, raw string) ReturnMapping {
	raw = strings.ToUpper(strings.TrimSpace(raw))
	if raw == "" {
		return unknownReturnMapping("")
	}
	switch rail {
	case RailACH:
		if m, ok := achReturnTable[raw]; ok {
			return m
		}
	case RailSWIFT:
		if m, ok := swiftReturnTable[raw]; ok {
			return m
		}
	}
	if m, ok := isoReturnTable[raw]; ok {
		return m
	}
	return unknownReturnMapping(raw)
}

// unknownReturnMapping is the fail-closed bucket for codes not in any
// table: quarantine-class (PENDING_REVIEW on the funding row), P1 alert.
func unknownReturnMapping(raw string) ReturnMapping {
	disp := raw
	if disp == "" {
		disp = "(empty)"
	}
	return ReturnMapping{
		RawCode:       raw,
		Canonical:     RetCanonUnknown,
		SurfaceCode:   "SETTLEMENT_RAIL_REJECTED",
		FundingStatus: FundingPendingReview,
		Quarantine:    true,
		Alert:         true,
		Description:   fmt.Sprintf("unmapped rail return code %s — manual triage required", disp),
	}
}

// ---------------------------------------------------------------------------
// ApplyReturn — row transition + compensating journal
// ---------------------------------------------------------------------------

// ReturnOutcome reports what ApplyReturn did.
type ReturnOutcome struct {
	Payment       *RailPaymentRow `json:"payment"`
	Mapping       ReturnMapping   `json:"mapping"`
	FundingStatus string          `json:"funding_status,omitempty"`
	JournalID     *int64          `json:"journal_entry_id,omitempty"`
	Quarantined   bool            `json:"quarantined"`
}

// ApplyReturn records a bank-side return/reject against a dispatched
// rail instruction: map the code, mark rail_payments RETURNED (or
// REJECTED for pre-dispatch rejects), and — when the instruction backs a
// client withdrawal whose hold is still locked — post the compensating
// journal (ClearingTransit → CustomerLiability, locked → available) and
// fail the funding row. Unknown codes park the funding row at
// PENDING_REVIEW and fire a P1 alert — the funds stay locked until ops
// resolves the classification (fail-closed).
//
// Idempotent on (end_to_end_id, raw_code): replaying the same bank
// notification returns the recorded outcome without double-compensating.
func (s *RailService) ApplyReturn(ctx context.Context, endToEndID, rawCode, reason string) (*ReturnOutcome, error) {
	e2e := strings.TrimSpace(endToEndID)
	if e2e == "" {
		return nil, errCode("INVALID_REQUEST", "end_to_end_id required")
	}
	// Locate the instruction — the rail keys the lookup table.
	pay, err := s.store.RailPaymentByEndToEndID(ctx, e2e)
	if err != nil {
		return nil, err
	}
	mapping := MapReturnCode(RailID(pay.Rail), rawCode)

	// Idempotent replay: same code already recorded.
	if pay.ReturnCode != nil && strings.EqualFold(*pay.ReturnCode, strings.ToUpper(strings.TrimSpace(rawCode))) &&
		(pay.Status == RailPaymentReturned || pay.Status == RailPaymentRejected) {
		return &ReturnOutcome{Payment: pay, Mapping: mapping, Quarantined: mapping.Quarantine}, nil
	}

	tx, err := s.store.BeginTx(ctx)
	if err != nil {
		return nil, wrapCode("INTERNAL_ERROR", "rail return tx", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	locked, err := s.store.RailPaymentForUpdate(ctx, tx, pay.ID)
	if err != nil {
		return nil, err
	}
	rc := strings.ToUpper(strings.TrimSpace(rawCode))
	newStatus := RailPaymentReturned
	if locked.Status == RailPaymentPrepared {
		newStatus = RailPaymentRejected
	}
	if err := s.store.SetRailPaymentStatus(ctx, tx, locked.ID, newStatus,
		&rc, &reason, nil, nil); err != nil {
		return nil, err
	}

	// Linked funding row: definitive rejections release the withdrawal
	// hold and mark FAILED; quarantine-class codes park at
	// PENDING_REVIEW for manual triage.
	var fundingStatus string
	if locked.FundingTransactionID != nil {
		ftx, ferr := s.store.FundingTxForUpdate(ctx, tx, *locked.FundingTransactionID)
		if ferr != nil {
			return nil, ferr
		}
		if mapping.Quarantine {
			if ftx.Status != FundingPendingReview {
				if uerr := s.store.SetFundingTxStatus(ctx, tx, ftx.ID,
					FundingPendingReview, nil); uerr != nil {
					return nil, uerr
				}
			}
			fundingStatus = FundingPendingReview
		} else if ftx.Status == FundingConfirmed || ftx.Status == FundingPending ||
			ftx.Status == FundingPendingReview {
			if uerr := s.store.SetFundingTxStatus(ctx, tx, ftx.ID,
				FundingFailed, nil); uerr != nil {
				return nil, uerr
			}
			fundingStatus = FundingFailed
		} else {
			fundingStatus = ftx.Status // terminal already — leave as-is
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, wrapCode("INTERNAL_ERROR", "rail return commit", err)
	}

	locked.Status = newStatus
	locked.ReturnCode = &rc
	locked.ReturnReason = &reason
	out := &ReturnOutcome{
		Payment: locked, Mapping: mapping,
		FundingStatus: fundingStatus, Quarantined: mapping.Quarantine,
	}

	// Compensating journal — only for definitive rejections on a client
	// withdrawal that still holds locked funds. Posted outside the row tx
	// (LedgerService owns its own tx); failure raises P1 — the hold stays
	// locked for ops replay, never silently dropped.
	if fundingStatus == FundingFailed && s.poster != nil && locked.FundingTransactionID != nil {
		fresh, ferr := s.fundingTxByID(ctx, *locked.FundingTransactionID)
		if ferr == nil && fresh.Type == "WITHDRAWAL" {
			res, perr := s.poster.Post(ctx, ledger.Journal{
				EntryType:      ledger.EntryWithdrawal,
				ReferenceID:    fresh.ID,
				Description:    fmt.Sprintf("withdrawal %d rail return %s — hold release", fresh.ID, rc),
				PostedBy:       "funding:rail-returns",
				IdempotencyKey: fmt.Sprintf("rail-return:%s", e2e),
				Lines: []ledger.Line{
					ledger.DebitLine(ledger.ClearingTransit(fresh.Currency), fresh.Currency, fresh.Amount,
						fmt.Sprintf("rail %s return %s — transit liability released", locked.Rail, rc)),
					ledger.CreditLine(ledger.CustomerLiability(fresh.Currency), fresh.Currency, fresh.Amount,
						"client balance restored"),
				},
				Effects: []ledger.AccountEffect{{
					AccountID:      fresh.AccountID,
					Currency:       fresh.Currency,
					AvailableDelta: fresh.Amount,
					LockedDelta:    fresh.Amount.Neg(),
				}},
			})
			if perr != nil && !res.Committed {
				s.log("funding: rail-return compensation for tx %d failed: %v", fresh.ID, perr)
				if s.alerter != nil {
					_ = s.alerter.Raise(ctx, OpsAlert{
						Severity: "P1",
						Code:     "RAIL_RETURN_COMPENSATION_FAILED",
						Summary:  fmt.Sprintf("rail return %s on funding tx %d — hold release failed, ops replay required", e2e, fresh.ID),
						Err:      perr.Error(),
					})
				}
			} else if res.JournalID != 0 {
				jid := res.JournalID
				out.JournalID = &jid
			}
		}
	}

	// Quarantine-class codes always page ops — unmapped reasons never
	// resolve themselves.
	if mapping.Quarantine || mapping.Alert {
		if s.alerter != nil {
			sev := "P2"
			code := "RAIL_RETURN_COMPLIANCE"
			if mapping.Quarantine {
				sev = "P1"
				code = "RAIL_RETURN_UNKNOWN"
			}
			_ = s.alerter.Raise(ctx, OpsAlert{
				Severity: sev,
				Code:     code,
				Summary: fmt.Sprintf("rail %s return %s on %s: %s",
					locked.Rail, rc, e2e, mapping.Description),
			})
		}
	}
	return out, nil
}

// fundingTxByID reads a funding row without a lock (post-commit state).
func (s *RailService) fundingTxByID(ctx context.Context, id int64) (*FundingTxRow, error) {
	tx, err := s.store.BeginTx(ctx)
	if err != nil {
		return nil, wrapCode("INTERNAL_ERROR", "funding tx read", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	return s.store.FundingTxForUpdate(ctx, tx, id)
}
