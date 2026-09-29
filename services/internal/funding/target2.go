// TARGET2 adapter — Task 11.3.1: real-time gross settlement EUR
// payments over the ECB TARGET2/T2 scheme (ISO 20022 pacs.008-family
// credit transfer fields) plus the pacs.004 return. EUR-only (§17.2).
package funding

// Target2Payment is the TARGET2 RTGS envelope — settlement method CLRG
// (central clearing), settlement in central-bank money.
type Target2Payment struct {
	Transfer         IsoCreditTransfer `json:"credit_transfer"`
	SettlementMethod string            `json:"settlement_method"` // CLRG
	RealtimeSettle   bool              `json:"realtime_settle"`   // always true on T2
}

// Target2Adapter builds TARGET2 envelopes.
type Target2Adapter struct{}

// ID returns the rail key.
func (Target2Adapter) ID() RailID { return RailTARGET2 }

// BuildOutbound produces the TARGET2 envelope.
func (Target2Adapter) BuildOutbound(p OutboundPayment) (*WireEnvelope, error) {
	if p.Currency != "EUR" {
		return nil, errf(CodeBankingRailUnavailable, "TARGET2 carries EUR only")
	}
	e2e, err := endToEndID("T2", p.EndToEndID)
	if err != nil {
		return nil, err
	}
	msg := mkISO(p, "URGP", e2e, e2e)
	msg.SettlementMethod = "CLRG"
	return &WireEnvelope{
		Rail: string(RailTARGET2), MessageType: MsgPacs008,
		EndToEndID: e2e,
		Payload:    Target2Payment{Transfer: *msg, SettlementMethod: "CLRG", RealtimeSettle: true},
	}, nil
}

// BuildReturn produces the TARGET2 pacs.004 return envelope.
func (Target2Adapter) BuildReturn(r ReturnInstruction) (*WireEnvelope, error) {
	if r.Currency != "EUR" {
		return nil, errf(CodeBankingRailUnavailable, "TARGET2 carries EUR only")
	}
	e2e, err := endToEndID("T2RTN", "")
	if err != nil {
		return nil, err
	}
	return &WireEnvelope{
		Rail: string(RailTARGET2), MessageType: MsgPacs004,
		EndToEndID: e2e, Payload: mkISOReturn(r, e2e, e2e),
	}, nil
}
