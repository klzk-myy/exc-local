// Shared ISO 20022 typed message fields — the canonical wire payload
// every XML-family rail adapter serialises (SEPA pain.001/pacs.008,
// FedNow pacs.008, CHAPS, TARGET2 pacs.009-ish credit transfers, and the
// pacs.004 payment return used by every rail's automated return wire).
//
// These structs model the wire *fields* (JSON persist of the envelope) —
// transport-layer XML serialisation belongs to the connector, not here.
package funding

import (
	crand "crypto/rand"
	"encoding/hex"
	"fmt"
	"strings"
	"time"
)

// IsoParty identifies one payment party.
type IsoParty struct {
	Name    string `json:"name,omitempty"`
	IBAN    string `json:"iban,omitempty"`
	Account string `json:"account,omitempty"` // non-IBAN account number
	BIC     string `json:"bic,omitempty"`
}

// IsoAmount is the InstdAmt shape — value is decimal text (DECIMAL(28,8)
// quantum enforced upstream; the wire never sees float rounding).
type IsoAmount struct {
	Currency string `json:"ccy"`
	Value    string `json:"value"`
}

// IsoCreditTransfer is the pacs.008/pain.001 credit-transfer payload —
// one debtor → one creditor (single-payment files; batching is a
// transport concern).
type IsoCreditTransfer struct {
	MessageID              string    `json:"msg_id"`
	CreationDateTime       string    `json:"creation_dt"` // RFC3339
	EndToEndID             string    `json:"end_to_end_id"`
	UETR                   string    `json:"uetr,omitempty"`
	ServiceLevel           string    `json:"service_level,omitempty"` // SEPA|SEPA_INST|SDVA|URGP
	LocalInstrument        string    `json:"local_instrument,omitempty"`
	SettlementMethod       string    `json:"settlement_method,omitempty"` // CLRG|INDA|INGA
	Amount                 IsoAmount `json:"amount"`
	ChargeBearer           string    `json:"charge_bearer,omitempty"` // SLEV|OUR|SHA|BEN
	ValueDate              string    `json:"value_date,omitempty"`    // YYYY-MM-DD
	Debtor                 IsoParty  `json:"debtor"`
	DebtorAgentBIC         string    `json:"debtor_agent_bic,omitempty"`
	CreditorAgentBIC       string    `json:"creditor_agent_bic,omitempty"`
	Creditor               IsoParty  `json:"creditor"`
	RemittanceUnstructured string    `json:"remittance,omitempty"`
}

// IsoPaymentReturn is the pacs.004 payment-return payload.
type IsoPaymentReturn struct {
	MessageID          string    `json:"msg_id"`
	CreationDateTime   string    `json:"creation_dt"`
	OriginalMessageID  string    `json:"original_msg_id,omitempty"`
	OriginalEndToEndID string    `json:"original_end_to_end_id"`
	OriginalBankTxID   string    `json:"original_bank_tx_id,omitempty"`
	ReturnedAmount     IsoAmount `json:"returned_amount"`
	ReturnReasonCode   string    `json:"return_reason_code"` // RsnCd e.g. AC01/AM04/RR04
	ReturnReason       string    `json:"return_reason,omitempty"`
	ChargeBearer       string    `json:"charge_bearer,omitempty"` // SLEV on returns (fees deducted)
	Debtor             IsoParty  `json:"debtor"`
	DebtorAgentBIC     string    `json:"debtor_agent_bic,omitempty"`
	Creditor           IsoParty  `json:"creditor"`
	CreditorAgentBIC   string    `json:"creditor_agent_bic,omitempty"`
}

// endToEndID returns the caller-supplied wire reference or mints one —
// E2E-{prefix}-{20 hex chars} from a fresh random source, ISO-width safe.
func endToEndID(prefix, seed string) (string, error) {
	seed = strings.TrimSpace(seed)
	if seed != "" {
		if len(seed) > 64 {
			return "", errCode("INVALID_REQUEST", "end_to_end_id exceeds 64 chars")
		}
		return seed, nil
	}
	var b [10]byte
	if _, err := crand.Read(b[:]); err != nil {
		return "", wrapCode("INTERNAL_ERROR", "end-to-end id", err)
	}
	return fmt.Sprintf("E2E-%s-%s", prefix, hex.EncodeToString(b[:])), nil
}

// mkISO converts an OutboundPayment to the shared credit-transfer shape.
func mkISO(p OutboundPayment, svcLevel, msgID, endToEnd string) *IsoCreditTransfer {
	vd := ""
	if !p.ValueDate.IsZero() {
		vd = p.ValueDate.UTC().Format("2006-01-02")
	}
	return &IsoCreditTransfer{
		MessageID:        msgID,
		CreationDateTime: time.Now().UTC().Format("2006-01-02T15:04:05Z"),
		EndToEndID:       endToEnd,
		ServiceLevel:     svcLevel,
		Amount:           IsoAmount{Currency: p.Currency, Value: p.Amount.String()},
		ChargeBearer:     p.Charges,
		ValueDate:        vd,
		Debtor: IsoParty{
			Name: p.DebtorName, IBAN: p.DebtorAccount, BIC: p.DebtorBIC,
		},
		Creditor: IsoParty{
			Name: p.CreditorName, IBAN: p.CreditorIBAN, BIC: p.CreditorBIC,
		},
		DebtorAgentBIC:         p.DebtorBIC,
		CreditorAgentBIC:       p.CreditorBIC,
		RemittanceUnstructured: p.RemittanceInfo,
	}
}

// mkISOReturn converts a ReturnInstruction to the shared pacs.004 shape.
func mkISOReturn(r ReturnInstruction, msgID, endToEnd string) *IsoPaymentReturn {
	return &IsoPaymentReturn{
		MessageID:          msgID,
		CreationDateTime:   time.Now().UTC().Format("2006-01-02T15:04:05Z"),
		OriginalEndToEndID: r.OriginalEndToEndID,
		OriginalBankTxID:   r.OriginalBankTxID,
		ReturnedAmount:     IsoAmount{Currency: r.Currency, Value: r.Amount.String()},
		ReturnReasonCode:   r.ReturnReasonCode,
		ReturnReason:       r.ReturnReason,
		ChargeBearer:       "SLEV", // fees deducted per §17.12.2
		Debtor: IsoParty{
			Name: r.DebtorName, IBAN: r.DebtorAccount, BIC: r.DebtorBIC,
		},
		Creditor: IsoParty{
			Name: r.CreditorName, IBAN: r.CreditorAccount, BIC: r.CreditorBIC,
		},
		DebtorAgentBIC:   r.DebtorBIC,
		CreditorAgentBIC: r.CreditorBIC,
	}
}
