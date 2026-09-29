// ACH adapter — Task 11.3.1: NACHA file/entry structs for USD domestic
// transfers and the NACHA return entry (R-code) for automated returns.
// USD-only, batch rail (spec §17.2).
package funding

import (
	"fmt"
	"strings"
)

// NACHAFileHeader is the file header record (type "1").
type NACHAFileHeader struct {
	RecordType           string `json:"record_type"` // "1"
	PriorityCode         string `json:"priority_code"`
	ImmediateDestination string `json:"immediate_destination"` // RDFI, 9 digits + leading space
	ImmediateOrigin      string `json:"immediate_origin"`      // EIN/company id
	FileCreationDate     string `json:"file_creation_date"`    // YYMMDD
	FileCreationTime     string `json:"file_creation_time"`    // HHMM
	FileIDModifier       string `json:"file_id_modifier"`      // "A"
	RecordSize           int    `json:"record_size"`           // 94
	BlockingFactor       int    `json:"blocking_factor"`       // 10
	FormatCode           int    `json:"format_code"`           // 1
	DestinationName      string `json:"destination_name"`
	OriginName           string `json:"origin_name"`
}

// NACHABatchHeader is the company/batch header record (type "5").
type NACHABatchHeader struct {
	RecordType         string `json:"record_type"`        // "5"
	ServiceClassCode   int    `json:"service_class_code"` // 220 credits
	CompanyName        string `json:"company_name"`
	CompanyID          string `json:"company_id"`
	SECCode            string `json:"sec_code"` // PPD|CCD
	EntryDescription   string `json:"entry_description"`
	EffectiveEntryDate string `json:"effective_entry_date"` // YYMMDD — value date
	OriginatingDFI     string `json:"originating_dfi"`      // 8-digit
}

// NACHAEntry is the entry detail record (type "6") — credit to the
// receiver. Amount is integer minor units (USD cents — ACH has no
// decimal field; quantized at insert).
type NACHAEntry struct {
	RecordType       string `json:"record_type"`      // "6"
	TransactionCode  int    `json:"transaction_code"` // 22 = checking credit, 32 = savings credit
	RDFI             string `json:"rdfi"`             // 8-digit receiving DFI
	CheckDigit       string `json:"check_digit"`
	AccountNumber    string `json:"account_number"`
	AmountCents      int64  `json:"amount_cents"`
	ReceiverName     string `json:"receiver_name"`
	Discretionary    string `json:"discretionary,omitempty"`
	TraceNumber      string `json:"trace_number"`                 // ODFI (8) + sequence (7)
	ReturnReasonCode string `json:"return_reason_code,omitempty"` // R-code on returns
}

// NACHAFile is one ACH file with a single batch (one payment per file —
// batching is a transport concern).
type NACHAFile struct {
	Header NACHAFileHeader  `json:"file_header"`
	Batch  NACHABatchHeader `json:"batch_header"`
	Entry  NACHAEntry       `json:"entry"`
}

// ACHAdapter builds NACHA envelopes.
type ACHAdapter struct {
	// ImmediateOrigin / ImmediateDestination / CompanyID / OriginatingDFI
	// are wired at composition root (exchange's ACH origination profile).
	ImmediateOrigin      string
	ImmediateDestination string
	CompanyID            string
	OriginatingDFI       string
}

// ID returns the rail key.
func (ACHAdapter) ID() RailID { return RailACH }

// BuildOutbound produces the NACHA file envelope. The rail carries
// integer cents — an amount that isn't cent-quantized fails closed.
func (a ACHAdapter) BuildOutbound(p OutboundPayment) (*WireEnvelope, error) {
	if p.Currency != "USD" {
		return nil, errf(CodeBankingRailUnavailable, "ACH carries USD only")
	}
	cents, ok := achCents(p.Amount.String())
	if !ok {
		return nil, errf("INVALID_REQUEST",
			"amount %s is not cent-quantized for ACH", p.Amount.String())
	}
	e2e, err := endToEndID("ACH", p.EndToEndID)
	if err != nil {
		return nil, err
	}
	eff := ""
	if !p.ValueDate.IsZero() {
		eff = p.ValueDate.UTC().Format("060102")
	}
	rdfi, chk := splitRouting(p.CreditorIBAN)
	f := NACHAFile{
		Header: NACHAFileHeader{
			RecordType:           "1",
			PriorityCode:         "01",
			ImmediateDestination: achPadded9(a.ImmediateDestination),
			ImmediateOrigin:      achPadded9(a.ImmediateOrigin),
			FileIDModifier:       "A",
			RecordSize:           94, BlockingFactor: 10, FormatCode: 1,
			DestinationName: "RDFI", OriginName: p.DebtorName,
		},
		Batch: NACHABatchHeader{
			RecordType:         "5",
			ServiceClassCode:   220,
			CompanyName:        p.DebtorName,
			CompanyID:          a.CompanyID,
			SECCode:            "PPD",
			EntryDescription:   "WITHDRAWAL",
			EffectiveEntryDate: eff,
			OriginatingDFI:     a.OriginatingDFI,
		},
		Entry: NACHAEntry{
			RecordType:      "6",
			TransactionCode: 22,
			RDFI:            rdfi,
			CheckDigit:      chk,
			AccountNumber:   p.CreditorIBAN,
			AmountCents:     cents,
			ReceiverName:    p.CreditorName,
			TraceNumber:     achTrace(a.OriginatingDFI, e2e),
			Discretionary:   p.RemittanceInfo,
		},
	}
	return &WireEnvelope{
		Rail: string(RailACH), MessageType: MsgNachaFile,
		EndToEndID: e2e, Payload: f,
	}, nil
}

// BuildReturn produces the NACHA return entry (type "6" with the R-code
// in the return reason field — the ACH return mechanism).
func (a ACHAdapter) BuildReturn(r ReturnInstruction) (*WireEnvelope, error) {
	if r.Currency != "USD" {
		return nil, errf(CodeBankingRailUnavailable, "ACH carries USD only")
	}
	cents, ok := achCents(r.Amount.String())
	if !ok {
		return nil, errf("INVALID_REQUEST",
			"amount %s is not cent-quantized for ACH", r.Amount.String())
	}
	e2e, err := endToEndID("ACHRTN", "")
	if err != nil {
		return nil, err
	}
	code := r.ReturnReasonCode
	if !strings.HasPrefix(code, "R") || len(code) != 3 {
		code = "R01" // unmapped reason fails closed to the generic return code
	}
	rdfi, chk := splitRouting(r.CreditorAccount)
	entry := NACHAEntry{
		RecordType:       "6",
		TransactionCode:  21, // checking account return/credit return
		RDFI:             rdfi,
		CheckDigit:       chk,
		AccountNumber:    r.CreditorAccount,
		AmountCents:      cents,
		ReceiverName:     r.CreditorName,
		TraceNumber:      achTrace(a.OriginatingDFI, e2e),
		ReturnReasonCode: code,
	}
	return &WireEnvelope{
		Rail: string(RailACH), MessageType: MsgNachaFile,
		EndToEndID: e2e,
		Payload: NACHAFile{
			Batch: NACHABatchHeader{RecordType: "5", ServiceClassCode: 225,
				CompanyName: r.DebtorName, CompanyID: a.CompanyID,
				SECCode: "PPD", EntryDescription: "RETURN",
				OriginatingDFI: a.OriginatingDFI},
			Entry: entry,
		},
	}, nil
}

// achCents parses a decimal string into integer cents; the second return
// is false when the amount has sub-cent precision.
func achCents(s string) (int64, bool) {
	neg := strings.HasPrefix(s, "-")
	s = strings.TrimPrefix(s, "-")
	parts := strings.SplitN(s, ".", 2)
	var whole, frac string
	whole = parts[0]
	if len(parts) == 2 {
		frac = parts[1]
	}
	if len(frac) > 2 {
		if strings.TrimRight(frac[2:], "0") != "" {
			return 0, false // sub-cent precision
		}
		frac = frac[:2]
	}
	for len(frac) < 2 {
		frac += "0"
	}
	var w int64
	for _, r := range whole {
		if r < '0' || r > '9' {
			return 0, false
		}
		w = w*10 + int64(r-'0')
	}
	var f int64
	for _, r := range frac {
		f = f*10 + int64(r-'0')
	}
	cents := w*100 + f
	if neg || cents <= 0 {
		return 0, false
	}
	return cents, true
}

// splitRouting splits a US ABA routing number into RDFI + check digit;
// non-9-digit input returns the value as-is (fail-open on shape — the
// store's beneficiary registry validates format upstream).
func splitRouting(rtn string) (rdfi, chk string) {
	rtn = strings.TrimSpace(rtn)
	if len(rtn) == 9 && isDigits(rtn) {
		return rtn[:8], rtn[8:]
	}
	return rtn, ""
}

func isDigits(s string) bool {
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return len(s) > 0
}

func achPadded9(s string) string {
	if len(s) >= 10 {
		return s[:10]
	}
	return fmt.Sprintf("%-10s", s) // NACHA pads destination/origin left-blank? fields are 10 wide
}

// achTrace derives a 15-char trace: 8-digit ODFI + zero-padded sequence
// from the end-to-end hash tail.
func achTrace(odfi, e2e string) string {
	seq := 0
	for _, r := range sha256Hex([]byte(e2e))[:7] {
		seq = (seq*16 + int(r)) % 10_000_000
	}
	return fmt.Sprintf("%-8.8s%07d", odfi, seq)
}
