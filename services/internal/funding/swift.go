// SWIFT adapter — Task 11.3.1: MT103 customer credit transfers, MT202
// bank-to-bank transfers, and MT199 free-format return messages. Typed
// field structs mirror the MT message layout; no live Alliance
// connectivity — envelopes persist to rail_payments.envelope (JSONB).
package funding

import (
	"fmt"
	"strings"
)

// MT103 is the SWIFT customer credit transfer (series 100).
type MT103 struct {
	SenderBIC            string `json:"sender_bic"`
	ReceiverBIC          string `json:"receiver_bic"`
	UETR                 string `json:"uetr"` // field 121 (gpi)
	SenderReference      string `json:"f20_sender_ref"`
	BankOperationCode    string `json:"f23b"` // CRED
	ValueDateCcyAmount   string `json:"f32a"` // YYMMDD + CCY + amount
	OrderingCustomerName string `json:"f50k_name"`
	OrderingCustomerAcct string `json:"f50k_account"`
	// OrderingCustomerAddr is the field 50K address line(s) — FATF R.16
	// originator data populated by the Phase-21 Task 21.3.2 travel-rule
	// gate when the ordering customer differs from the debtor.
	OrderingCustomerAddr string `json:"f50k_address,omitempty"`
	OrderingInstitution  string `json:"f52a_bic,omitempty"`
	AccountWithInst      string `json:"f57a_bic,omitempty"`
	BeneficiaryAcct      string `json:"f59_account"`
	BeneficiaryName      string `json:"f59_name"`
	RemittanceInfo       string `json:"f70,omitempty"`
	DetailsOfCharges     string `json:"f71a"` // OUR|SHA|BEN
}

// MT202 is the SWIFT bank-to-bank transfer (series 200).
type MT202 struct {
	SenderBIC           string `json:"sender_bic"`
	ReceiverBIC         string `json:"receiver_bic"`
	TransactionRef      string `json:"f20"`
	RelatedRef          string `json:"f21,omitempty"`
	ValueDateCcyAmount  string `json:"f32a"`
	OrderingInstitution string `json:"f52a_bic,omitempty"`
	BeneficiaryInst     string `json:"f58a_bic"`
}

// MT199 is the free-format message used for SWIFT returns/queries
// (field 79 carries the return reason + original reference).
type MT199 struct {
	SenderBIC      string `json:"sender_bic"`
	ReceiverBIC    string `json:"receiver_bic"`
	TransactionRef string `json:"f20"`
	RelatedRef     string `json:"f21"` // original wire reference
	Narrative      string `json:"f79"` // return reason text
}

// SwiftAdapter builds MT envelopes.
type SwiftAdapter struct{}

// ID returns the rail key.
func (SwiftAdapter) ID() RailID { return RailSWIFT }

// BuildOutbound produces an MT103 (customer payment) or MT202 when the
// creditor leg is itself a financial institution (BIC-only, no IBAN —
// bank-to-bank shape).
func (SwiftAdapter) BuildOutbound(p OutboundPayment) (*WireEnvelope, error) {
	e2e, err := endToEndID("SWIFT", p.EndToEndID)
	if err != nil {
		return nil, err
	}
	uetr := swiftUETR(e2e)
	f32a := p.ValueDate.UTC().Format("060102") + p.Currency + p.Amount.String()
	if p.CreditorIBAN == "" && p.CreditorBIC != "" {
		// Bank-to-bank leg → MT202.
		env := &WireEnvelope{
			Rail: string(RailSWIFT), MessageType: MsgMT202,
			EndToEndID: e2e, UETR: uetr,
			Payload: MT202{
				SenderBIC:           p.DebtorBIC,
				ReceiverBIC:         p.CreditorBIC,
				TransactionRef:      e2e,
				ValueDateCcyAmount:  f32a,
				OrderingInstitution: p.DebtorBIC,
				BeneficiaryInst:     p.CreditorBIC,
			},
		}
		return env, nil
	}
	// Field 50K ordering customer: the FATF originator when the Phase-21
	// travel-rule gate populated Originator*; otherwise the debtor
	// (exchange nostro holder) — unchanged legacy behaviour.
	ordName, ordAcct, ordAddr := p.OriginatorName, p.OriginatorAccount,
		p.OriginatorAddress
	if ordName == "" {
		ordName = p.DebtorName
	}
	if ordAcct == "" {
		ordAcct = p.DebtorAccount
	}
	env := &WireEnvelope{
		Rail: string(RailSWIFT), MessageType: MsgMT103,
		EndToEndID: e2e, UETR: uetr,
		Payload: MT103{
			SenderBIC:            p.DebtorBIC,
			ReceiverBIC:          p.CreditorBIC,
			UETR:                 uetr,
			SenderReference:      e2e,
			BankOperationCode:    "CRED",
			ValueDateCcyAmount:   f32a,
			OrderingCustomerName: ordName,
			OrderingCustomerAcct: ordAcct,
			OrderingCustomerAddr: ordAddr,
			OrderingInstitution:  p.DebtorBIC,
			AccountWithInst:      p.CreditorBIC,
			BeneficiaryAcct:      p.CreditorIBAN,
			BeneficiaryName:      p.CreditorName,
			RemittanceInfo:       p.RemittanceInfo,
			DetailsOfCharges:     swiftCharges(p.Charges),
		},
	}
	return env, nil
}

// BuildReturn produces an MT199 narrative return — the SWIFT MT world
// has no pacs.004 equivalent; the return reason rides field 79 with the
// original wire reference in field 21.
func (SwiftAdapter) BuildReturn(r ReturnInstruction) (*WireEnvelope, error) {
	e2e, err := endToEndID("SWIFTRTN", "")
	if err != nil {
		return nil, err
	}
	narrative := fmt.Sprintf("/RETN/%s/RREF/%s/REAS/%s %s",
		r.OriginalEndToEndID, r.OriginalBankTxID,
		r.ReturnReasonCode, r.ReturnReason)
	if len(narrative) > 35*50 {
		narrative = narrative[:35*50] // MT199 field 79 is 35×50
	}
	return &WireEnvelope{
		Rail: string(RailSWIFT), MessageType: MsgMT199,
		EndToEndID: e2e, UETR: swiftUETR(e2e),
		Payload: MT199{
			SenderBIC:      r.DebtorBIC,
			ReceiverBIC:    r.CreditorBIC,
			TransactionRef: e2e,
			RelatedRef:     r.OriginalEndToEndID,
			Narrative:      narrative,
		},
	}, nil
}

// swiftUETR derives a deterministic UETR-shaped reference (36 chars)
// from the end-to-end id — gpi tracks the same payment end to end.
func swiftUETR(e2e string) string {
	sum := sha256Hex([]byte("UETR:" + e2e))
	return fmt.Sprintf("%s-%s-%s-%s-%s",
		sum[0:8], sum[8:12], sum[12:16], sum[16:20], sum[20:32])
}

func swiftCharges(c string) string {
	switch strings.ToUpper(strings.TrimSpace(c)) {
	case "OUR", "SHA", "BEN":
		return strings.ToUpper(strings.TrimSpace(c))
	default:
		return "SHA"
	}
}
