// FedNow adapter — Task 11.3.1: instant USD credit transfers modelled
// on the FedNow ISO 20022 customer credit transfer (pacs.008 shape)
// plus the pacs.004 payment return. USD-only, 24/7 (spec §17.2/§17.16).
package funding

// FedNowPayment is the FedNow instant credit-transfer envelope fields.
type FedNowPayment struct {
	Transfer      IsoCreditTransfer `json:"credit_transfer"`
	InstantSettle bool              `json:"instant_settle"` // always true on FedNow
}

// FedNowAdapter builds FedNow envelopes.
type FedNowAdapter struct{}

// ID returns the rail key.
func (FedNowAdapter) ID() RailID { return RailFedNow }

// BuildOutbound produces the instant pacs.008-shaped envelope.
func (FedNowAdapter) BuildOutbound(p OutboundPayment) (*WireEnvelope, error) {
	if p.Currency != "USD" {
		return nil, errf(CodeBankingRailUnavailable, "FedNow carries USD only")
	}
	e2e, err := endToEndID("FEDNOW", p.EndToEndID)
	if err != nil {
		return nil, err
	}
	msg := mkISO(p, "INSTANT", e2e, e2e)
	msg.LocalInstrument = "INST"
	return &WireEnvelope{
		Rail: string(RailFedNow), MessageType: MsgPacs008,
		EndToEndID: e2e,
		Payload:    FedNowPayment{Transfer: *msg, InstantSettle: true},
	}, nil
}

// BuildReturn produces the FedNow pacs.004 return envelope.
func (FedNowAdapter) BuildReturn(r ReturnInstruction) (*WireEnvelope, error) {
	e2e, err := endToEndID("FNRTN", "")
	if err != nil {
		return nil, err
	}
	return &WireEnvelope{
		Rail: string(RailFedNow), MessageType: MsgPacs004,
		EndToEndID: e2e, Payload: mkISOReturn(r, e2e, e2e),
	}, nil
}
