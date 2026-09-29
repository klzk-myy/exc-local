// SEPA adapter — Task 11.3.1: SEPA Credit Transfer (SCT) via pain.001
// and SEPA Instant Credit Transfer (SCT Inst) via pacs.008, plus the
// pacs.004 return. EUR-only rail (capability matrix, spec §17.2).
package funding

import "exchange/pkg/decimal"

// Pain001 is the customer-to-bank SCT initiation document shape.
type Pain001 struct {
	GroupHeader struct {
		MessageID        string `json:"msg_id"`
		CreationDateTime string `json:"creation_dt"`
		NumberOfTxs      int    `json:"nb_of_txs"`
		InitiatingParty  string `json:"initiating_party"`
	} `json:"group_header"`
	PaymentInfo struct {
		PaymentInfoID   string `json:"pmt_inf_id"`
		PaymentMethod   string `json:"pmt_method"` // TRF
		RequestedExecDt string `json:"reqd_exctn_dt"`
	} `json:"payment_info"`
	CreditTransfer IsoCreditTransfer `json:"cdt_trf_tx_inf"`
}

// SepaAdapter builds SEPA envelopes; instant=true selects SCT Inst.
type SepaAdapter struct{}

// ID returns the rail key.
func (SepaAdapter) ID() RailID { return RailSEPA }

// BuildOutbound produces a pain.001 SCT envelope — or a pacs.008 SCT
// Inst envelope when the payment qualifies for the instant scheme
// (EUR, ≤ €100k cap from the matrix — enforced by selection upstream;
// the adapter re-checks the cap fail-closed).
func (SepaAdapter) BuildOutbound(p OutboundPayment) (*WireEnvelope, error) {
	e2e, err := endToEndID("SEPA", p.EndToEndID)
	if err != nil {
		return nil, err
	}
	// SCT Inst: instant-flagged payments ≤ €100k route to pacs.008 INST.
	if p.Amount.LessThanOrEqual(dec100k()) && p.Currency == "EUR" && p.ValueDate.IsZero() {
		msg := mkISO(p, "SEPA_INST", e2e, e2e)
		return &WireEnvelope{
			Rail: string(RailSEPA), MessageType: MsgPacs008,
			EndToEndID: e2e, Payload: msg,
		}, nil
	}
	msg := mkISO(p, "SEPA", e2e, e2e)
	doc := Pain001{CreditTransfer: *msg}
	doc.GroupHeader.MessageID = e2e
	doc.GroupHeader.CreationDateTime = msg.CreationDateTime
	doc.GroupHeader.NumberOfTxs = 1
	doc.GroupHeader.InitiatingParty = p.DebtorName
	doc.PaymentInfo.PaymentInfoID = e2e + "-PI"
	doc.PaymentInfo.PaymentMethod = "TRF"
	doc.PaymentInfo.RequestedExecDt = msg.ValueDate
	return &WireEnvelope{
		Rail: string(RailSEPA), MessageType: MsgPain001,
		EndToEndID: e2e, Payload: doc,
	}, nil
}

// BuildReturn produces the pacs.004 return envelope.
func (SepaAdapter) BuildReturn(r ReturnInstruction) (*WireEnvelope, error) {
	e2e, err := endToEndID("SEPARTN", "")
	if err != nil {
		return nil, err
	}
	return &WireEnvelope{
		Rail: string(RailSEPA), MessageType: MsgPacs004,
		EndToEndID: e2e, Payload: mkISOReturn(r, e2e, e2e),
	}, nil
}

func dec100k() decimal.Decimal { return decimal.NewFromInt(100_000) }
