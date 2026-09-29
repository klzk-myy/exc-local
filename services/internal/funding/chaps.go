// CHAPS adapter — Task 11.3.1: same-day GBP payments over the Bank of
// England CHAPS scheme, modelled on the ISO 20022 credit transfer
// (pacs.008-family fields) plus the pacs.004 return. GBP-only (§17.2).
package funding

// ChapsPayment is the CHAPS same-day payment envelope.
type ChapsPayment struct {
	Transfer      IsoCreditTransfer `json:"credit_transfer"`
	SameDaySettle bool              `json:"same_day_settle"` // always true on CHAPS
}

// ChapsAdapter builds CHAPS envelopes.
type ChapsAdapter struct{}

// ID returns the rail key.
func (ChapsAdapter) ID() RailID { return RailCHAPS }

// BuildOutbound produces the CHAPS envelope (service level SDVA —
// same-day value).
func (ChapsAdapter) BuildOutbound(p OutboundPayment) (*WireEnvelope, error) {
	if p.Currency != "GBP" {
		return nil, errf(CodeBankingRailUnavailable, "CHAPS carries GBP only")
	}
	e2e, err := endToEndID("CHAPS", p.EndToEndID)
	if err != nil {
		return nil, err
	}
	msg := mkISO(p, "SDVA", e2e, e2e)
	return &WireEnvelope{
		Rail: string(RailCHAPS), MessageType: MsgPacs008,
		EndToEndID: e2e,
		Payload:    ChapsPayment{Transfer: *msg, SameDaySettle: true},
	}, nil
}

// BuildReturn produces the CHAPS pacs.004 return envelope.
func (ChapsAdapter) BuildReturn(r ReturnInstruction) (*WireEnvelope, error) {
	if r.Currency != "GBP" {
		return nil, errf(CodeBankingRailUnavailable, "CHAPS carries GBP only")
	}
	e2e, err := endToEndID("CHRTN", "")
	if err != nil {
		return nil, err
	}
	return &WireEnvelope{
		Rail: string(RailCHAPS), MessageType: MsgPacs004,
		EndToEndID: e2e, Payload: mkISOReturn(r, e2e, e2e),
	}, nil
}
