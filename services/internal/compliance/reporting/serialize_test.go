package reporting

import (
	"bytes"
	"encoding/json"
	"encoding/xml"
	"strings"
	"testing"
)

func testEvent() *Event {
	return &Event{
		UTI: "213800TESTTEST000042T0001", UPI: "JEURUSDTEST",
		Regime: RegimeMIFID2, Action: ActionNew, EventType: EventTypeTrade,
		ReportSeq: 1, TradeID: 42,
		InstrumentCode: "EUR/USD", InstrumentType: "SPOT",
		BuyerLEI: "213800TESTTEST000042", BuyerID: "INTC501",
		SellerID: "INTC502", DecisionMakerID: "ALGO-9", TraderID: "INTC501",
		AlgoID: "ALGO-9", VenueMIC: "XEXC",
		Price: "1.0850", Quantity: "1000000", Currency: "USD",
		EventTS: testNow,
	}
}

func TestRTS22XML_VenueCodeNotISIN(t *testing.T) {
	e := testEvent()
	out, err := RTS22XML([]*Event{e}, "213800TESTTEST000042")
	if err != nil {
		t.Fatal(err)
	}
	if err := xml.Unmarshal(out, &struct {
		XMLName xml.Name `xml:"Document"`
	}{}); err != nil {
		t.Fatalf("malformed xml: %v", err)
	}
	s := string(out)
	if !strings.Contains(s, "auth.016.001.05") {
		t.Fatal("auth.016 namespace missing")
	}
	if !strings.Contains(s, "EUR/USD") {
		t.Fatal("venue instrument code must serialize (not ISIN)")
	}
	for _, want := range []string{"<TxId>42</TxId>", "INTC501", "INTC502",
		"ALGO-9", "XEXC", "1.0850"} {
		if !strings.Contains(s, want) {
			t.Fatalf("xml missing %q: %s", want, s)
		}
	}
}

func TestEMIRXML_WellFormed(t *testing.T) {
	e := testEvent()
	e.Regime = RegimeEMIRREFIT
	e.Notional = "1085000"
	e.Valuation = []byte(`{"mark_price":"1.0850"}`)
	out, err := EMIRXML(e, PayloadFor(e), "213800TESTTEST000042")
	if err != nil {
		t.Fatal(err)
	}
	if err := xml.Unmarshal(out, &struct {
		XMLName xml.Name `xml:"Document"`
	}{}); err != nil {
		t.Fatalf("malformed xml: %v", err)
	}
	s := string(out)
	for _, want := range []string{"auth.030.001.05", e.UTI, "NEWT", "TRADE",
		"213800TESTTEST000042", "INTC501", "EUR/USD", "1085000"} {
		if !strings.Contains(s, want) {
			t.Fatalf("emir xml missing %q: %s", want, s)
		}
	}
}

func TestPayloadForDest_APAVariant(t *testing.T) {
	e := testEvent()
	arm := PayloadForDest(e, DestinationARM, testNow)
	apa := PayloadForDest(e, DestinationAPA, testNow)
	if _, ok := apa["publication_datetime"]; !ok {
		t.Fatal("APA payload needs publication_datetime")
	}
	// RTS 1 strips party decomposition.
	for _, k := range []string{"buyer_lei", "seller_lei", "decision_maker_id"} {
		if _, ok := apa[k]; ok {
			t.Fatalf("APA payload must not carry %s", k)
		}
	}
	// ARM keeps decomposition + RTS 22 fields.
	if arm["buyer_id"] != "INTC501" || arm["tvtc"] != "42" ||
		arm["trading_datetime"] == "" {
		t.Fatalf("ARM payload: %v", arm)
	}
}

func TestValidatePayload(t *testing.T) {
	// Nil schema is itself a violation (fail closed).
	if v := ValidatePayload(nil, map[string]any{"a": 1}); len(v) == 0 {
		t.Fatal("nil schema must violate")
	}
	sv := &SchemaVersion{RequiredFields: []string{"a", "b", "c", "d", "e"}}
	v := ValidatePayload(sv, map[string]any{
		"a": "x", "b": "", "c": json.RawMessage(`{}`),
		"d": map[string]any{"k": 1}})
	if len(v) != 3 { // b empty, c {} empty, e absent
		t.Fatalf("violations: %v", v)
	}
	for _, f := range []string{"b", "c", "e"} {
		found := false
		for _, x := range v {
			if x == f {
				found = true
			}
		}
		if !found {
			t.Fatalf("expected violation for %s: %v", f, v)
		}
	}
}

func TestPayloadHash_Deterministic(t *testing.T) {
	h1 := PayloadHash([]byte("<x/>"), json.RawMessage(`{"a":1}`))
	h2 := PayloadHash([]byte("<x/>"), json.RawMessage(`{"a":1}`))
	if !bytes.Equal([]byte(h1), []byte(h2)) || h1 == "" {
		t.Fatal("hash must be deterministic")
	}
	if PayloadHash(nil, json.RawMessage(`{"a":2}`)) == h1 {
		t.Fatal("payload change must change hash")
	}
}
