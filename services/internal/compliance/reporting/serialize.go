// Regime payload builders + version-pinned validation + ISO 20022 XML
// serialization (Task 21.3.14, spec §14.1a).
//
// The canonical payload is a JSON object whose field names match the
// pinned required_fields registry rows — the pre-flight validator checks
// presence/non-empty against the active ruleset (the "version-pinned
// official rules" seam); the repositories themselves still run the
// official XSD/schema validation, and their verdicts ingest through the
// ACK/NACK path.
//
// ISO 20022 XML: EMIR REFIT uses auth.030.001.05 (DerivativesTradeReport);
// RTS 22 ARM batches use auth.016.001.05 (TransactionReport). The
// element structure is the regulator-defined envelope with the mandatory
// content — vendors perform full schema conformance on ingest.
package reporting

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"time"
)

// Schema names keyed in regulatory_schema_versions.
const (
	SchemaEMIR     = "auth.030.001.05"
	SchemaCFTCP43  = "CFTC-P43-DISSEMINATION"
	SchemaCFTCP45  = "CFTC-P45-SDR"
	SchemaMIFID22  = "auth.016.001.05"
	SchemaMIFIDAPA = "RTS1-POST-TRADE"
)

// Regulation names in the schema registry (finer than Regime — MiFID has
// two report classes).
const (
	RegEMIR     = "EMIR_REFIT"
	RegCFTCP43  = "CFTC_P43"
	RegCFTCP45  = "CFTC_P45"
	RegMIFID22  = "MIFID2_RTS22"
	RegMIFIDAPA = "MIFID2_RTS1"
)

// SchemaFor maps a regime (+ destination for MiFID's two messages) to
// the (regulation, schema_name) registry key.
func SchemaFor(regime Regime, dest Destination) (regulation, schemaName string) {
	switch regime {
	case RegimeEMIRREFIT:
		return RegEMIR, SchemaEMIR
	case RegimeCFTCP43:
		return RegCFTCP43, SchemaCFTCP43
	case RegimeCFTCP45:
		return RegCFTCP45, SchemaCFTCP45
	case RegimeMIFID2:
		if dest == DestinationAPA {
			return RegMIFIDAPA, SchemaMIFIDAPA
		}
		return RegMIFID22, SchemaMIFID22
	}
	return "", ""
}

// ---------------------------------------------------------------------------
// Canonical payloads
// ---------------------------------------------------------------------------

func put(m map[string]any, k string, v any) {
	switch t := v.(type) {
	case string:
		if t != "" {
			m[k] = t
		}
	case int64:
		if t != 0 {
			m[k] = t
		}
	default:
		if v != nil {
			m[k] = v
		}
	}
}

func rawOrNil(v json.RawMessage) any {
	if len(v) == 0 {
		return nil
	}
	var m map[string]any
	if err := json.Unmarshal(v, &m); err == nil {
		return m
	}
	return nil
}

// PayloadFor builds the canonical regime record for one event — the
// JSONB stored on the event row and embedded in the artifact payload.
func PayloadFor(e *Event) map[string]any {
	m := map[string]any{}
	put(m, "uti", e.UTI)
	put(m, "usi", e.USI)
	put(m, "upi", e.UPI)
	put(m, "prior_uti", e.PriorUTI)
	put(m, "prior_usi", e.PriorUSI)
	m["action_type"] = string(e.Action)
	m["event_type"] = string(e.EventType)
	put(m, "report_seq", int64(e.ReportSeq))
	put(m, "trade_id", e.TradeID)
	put(m, "position_id", e.PositionID)
	put(m, "instrument_id", e.InstrumentID)
	put(m, "instrument_code", e.InstrumentCode)
	put(m, "instrument_type", e.InstrumentType)
	put(m, "account_id", e.AccountID)
	put(m, "counterparty_account_id", e.CounterpartyAccountID)

	// Counterparty decomposition — the RTS 22 buyer/seller/decision-
	// maker split and the EMIR/CFTC counterparty fields share the same
	// resolved identifiers.
	put(m, "buyer_lei", e.BuyerLEI)
	put(m, "seller_lei", e.SellerLEI)
	put(m, "buyer_id", e.BuyerID)
	put(m, "seller_id", e.SellerID)
	put(m, "buyer_id_type", e.BuyerIDType)
	put(m, "seller_id_type", e.SellerIDType)
	put(m, "decision_maker_id", e.DecisionMakerID)
	put(m, "decision_maker_type", e.DecisionMakerType)
	put(m, "trader_id", e.TraderID)
	put(m, "algo_id", e.AlgoID)
	// EMIR/CFTC counterparty naming: counterparty1_lei (the reporting
	// entity = venue LEI) is stamped by WithExecutingEntity at artifact
	// build time; the client side rides as counterparty2.
	put(m, "counterparty2_id", e.BuyerID)
	put(m, "counterparty2_type", e.BuyerIDType)

	put(m, "venue_mic", e.VenueMIC)
	put(m, "jurisdiction", e.Jurisdiction)
	if e.DualSided {
		m["dual_sided"] = true
	}
	put(m, "price", e.Price)
	put(m, "quantity", e.Quantity)
	put(m, "notional", e.Notional)
	put(m, "currency", e.Currency)
	put(m, "valuation", rawOrNil(e.Valuation))
	put(m, "margin", rawOrNil(e.Margin))
	put(m, "clearing", rawOrNil(e.Clearing))
	put(m, "confirmation", rawOrNil(e.Confirmation))
	put(m, "allocation", rawOrNil(e.Allocation))
	m["event_ts"] = e.EventTS.UTC().Format(time.RFC3339Nano)

	// Regime-specific extras.
	switch e.Regime {
	case RegimeMIFID2:
		// RTS 22 field names (executing_entity_lei lands via
		// WithExecutingEntity at artifact build time).
		put(m, "tvtc", fmt.Sprintf("%d", e.TradeID)) // trading venue tx id
		m["trading_datetime"] = e.EventTS.UTC().Format(time.RFC3339Nano)
	case RegimeCFTCP43:
		m["asset_class"] = "FX"
		m["dissemination_datetime"] = e.EventTS.UTC().Format(time.RFC3339Nano)
	}
	return m
}

// PayloadForDest — the destination-specific artifact payload. MiFID II
// events fan out to ARM (RTS 22) and APA (RTS 1) — same canonical event,
// different mandatory field sets; the APA variant carries the RTS 1
// post-trade transparency shape (publication_datetime = submission time).
func PayloadForDest(e *Event, dest Destination, at time.Time) map[string]any {
	m := PayloadFor(e)
	if e.Regime == RegimeMIFID2 && dest == DestinationAPA {
		m["publication_datetime"] = at.UTC().Format(time.RFC3339Nano)
		// RTS 1 doesn't carry party decomposition.
		delete(m, "buyer_lei")
		delete(m, "seller_lei")
		delete(m, "decision_maker_id")
		delete(m, "decision_maker_type")
	}
	return m
}

// WithExecutingEntity stamps the venue LEI into payload fields that name
// the reporting/executing entity (kept out of the Event row — it is a
// venue constant, not per-event data).
func WithExecutingEntity(payload map[string]any, venueLEI string) map[string]any {
	if venueLEI != "" {
		payload["executing_entity_lei"] = venueLEI
		payload["reporting_entity_lei"] = venueLEI
		// The venue is counterparty 1 (the reporting/central side) for
		// client trades on a matched-principal venue.
		payload["counterparty1_lei"] = venueLEI
	}
	return payload
}

// ---------------------------------------------------------------------------
// Version-pinned validation (the "official rules" pre-flight)
// ---------------------------------------------------------------------------

// ValidatePayload enforces the registry-pinned required_fields against
// the serialized payload; missing/empty keys are returned as violations.
// Fail-closed: a nil schema (no active pinned ruleset) is itself a
// violation — reports never ship against an unregistered schema.
func ValidatePayload(schema *SchemaVersion, payload map[string]any) []string {
	if schema == nil {
		return []string{"no active schema version registered for regulation"}
	}
	var missing []string
	for _, f := range schema.RequiredFields {
		v, ok := payload[f]
		if !ok || v == nil {
			missing = append(missing, f)
			continue
		}
		switch t := v.(type) {
		case string:
			if t == "" {
				missing = append(missing, f)
			}
		case json.RawMessage:
			if len(t) == 0 || string(t) == "{}" {
				missing = append(missing, f)
			}
		case map[string]any:
			if len(t) == 0 {
				missing = append(missing, f)
			}
		}
	}
	return missing
}

// PayloadHash is the sha256 anchor stored on artifact + transport rows.
func PayloadHash(xml []byte, payload json.RawMessage) string {
	h := sha256.New()
	h.Write(xml)
	h.Write(payload)
	return hex.EncodeToString(h.Sum(nil))
}

// ---------------------------------------------------------------------------
// ISO 20022 XML serializers
// ---------------------------------------------------------------------------

// emirXML is the auth.030.001.05 DerivativesTradeReport subset. Element
// names follow the ISO 20022 EMIR REFIT vocabulary; the struct carries
// the mandatory content the registry's required_fields pin.
type emirXML struct {
	XMLName xml.Name `xml:"Document"`
	Xmlns   string   `xml:"xmlns,attr"`
	Report  emirRpt  `xml:"DerivsTradRpt>TradData>Rpt"`
}

type emirRpt struct {
	UTI            string `xml:"TechRcrdId>Id,omitempty"`
	UPI            string `xml:"CtrPtySpcficData>Deriv>UPI,omitempty"`
	ActionType     string `xml:"CtrPtySpcficData>Deriv>TxData>ActnTp"`
	EventType      string `xml:"CtrPtySpcficData>Deriv>TxData>EvntTp"`
	EventTs        string `xml:"CtrPtySpcficData>Deriv>TxData>EvtTmstmp"`
	RptgCtrPtyLEI  string `xml:"CtrPtySpcficData>CtrPty>RptgCtrPty>Id>LEI"`
	OthrCtrPtyId   string `xml:"CtrPtySpcficData>CtrPty>OthrCtrPty>Id>Othr>Id"`
	InstrumentCode string `xml:"TradData>CmonTradData>TradData>Plc>Instrm>Id"`
	VenueMIC       string `xml:"TradData>CmonTradData>TradData>Plc>Ven>MIC"`
	Price          string `xml:"TradData>CmonTradData>Pric>Pric"`
	Quantity       string `xml:"TradData>CmonTradData>Qty>Qty"`
	Notional       string `xml:"TradData>CmonTradData>Ntnl>Ntnl"`
	Currency       string `xml:"TradData>CmonTradData>Ccy>Ccy"`
	Valuation      string `xml:"TradData>Vltn>Val,omitempty"`
	Margin         string `xml:"TradData>Mrgn>Im,omitempty"`
	PriorUTI       string `xml:"CtrPtySpcficData>Deriv>PrTxId,omitempty"`
}

// EMIRXML renders one event as an auth.030.001.05 report.
func EMIRXML(e *Event, payload map[string]any, venueLEI string) ([]byte, error) {
	other := e.BuyerID
	if other == "" {
		other = e.BuyerLEI
	}
	if other == "" && e.CounterpartyAccountID != 0 {
		other = fmt.Sprintf("INTC%d", e.CounterpartyAccountID)
	}
	val := ""
	if len(e.Valuation) > 0 {
		val = string(e.Valuation)
	}
	mrg := ""
	if len(e.Margin) > 0 {
		mrg = string(e.Margin)
	}
	doc := emirXML{
		Xmlns: "urn:iso:std:iso:20022:tech:xsd:auth.030.001.05",
		Report: emirRpt{
			UTI:            e.UTI,
			UPI:            e.UPI,
			ActionType:     string(e.Action),
			EventType:      string(e.EventType),
			EventTs:        e.EventTS.UTC().Format(time.RFC3339Nano),
			RptgCtrPtyLEI:  venueLEI,
			OthrCtrPtyId:   other,
			InstrumentCode: e.InstrumentCode,
			VenueMIC:       e.VenueMIC,
			Price:          e.Price,
			Quantity:       e.Quantity,
			Notional:       e.Notional,
			Currency:       e.Currency,
			Valuation:      val,
			Margin:         mrg,
			PriorUTI:       e.PriorUTI,
		},
	}
	var b bytes.Buffer
	b.WriteString(xml.Header)
	enc := xml.NewEncoder(&b)
	enc.Indent("", "  ")
	if err := enc.Encode(doc); err != nil {
		return nil, fmt.Errorf("reporting: auth.030 marshal: %w", err)
	}
	return b.Bytes(), nil
}

// RTS22Tx is one auth.016.001.05 transaction line (RTS 22 field subset —
// venue instrument code per the Task 21.3.4 correction, not ISIN).
type RTS22Tx struct {
	TVTC             string `xml:"TxId"`
	ExecutingEntity  string `xml:"ExctgPty>LEI"`
	BuyerID          string `xml:"Buyr>Id>Othr>Id"`
	SellerID         string `xml:"Sellr>Id>Othr>Id"`
	DecisionMaker    string `xml:"InvstmtDcsnPty>Id>Othr>Id,omitempty"`
	TraderID         string `xml:"ExctgDcsnPty>Id>Othr>Id,omitempty"`
	AlgoID           string `xml:"AlgoId,omitempty"`
	TradingDateTime  string `xml:"TxDtTm"`
	VenueMIC         string `xml:"PlcOfTx>Ven>MIC"`
	InstrumentCode   string `xml:"Instrm>Id"`
	Price            string `xml:"Tx>Pric>Pric"`
	Quantity         string `xml:"Tx>Qty>Qty"`
	Currency         string `xml:"Tx>Qty>Ccy"`
	TransmissionFlag string `xml:"Tx>Trnsmssn,omitempty"` // 'XMIT' when venue transmits on behalf
}

// RTS22XML renders an auth.016.001.05 ARM batch for a T+1 RTS 22
// transmission — one <Tx> per event.
func RTS22XML(events []*Event, venueLEI string) ([]byte, error) {
	type rpt struct {
		XMLName xml.Name  `xml:"Document"`
		Xmlns   string    `xml:"xmlns,attr"`
		Txs     []RTS22Tx `xml:"FinInstrmRptgTxRpt>Tx"`
	}
	doc := rpt{Xmlns: "urn:iso:std:iso:20022:tech:xsd:auth.016.001.05"}
	for _, e := range events {
		tx := RTS22Tx{
			TVTC:            fmt.Sprintf("%d", e.TradeID),
			ExecutingEntity: venueLEI,
			BuyerID:         e.BuyerID,
			SellerID:        e.SellerID,
			DecisionMaker:   e.DecisionMakerID,
			TraderID:        e.TraderID,
			AlgoID:          e.AlgoID,
			TradingDateTime: e.EventTS.UTC().Format(time.RFC3339Nano),
			VenueMIC:        e.VenueMIC,
			InstrumentCode:  e.InstrumentCode,
			Price:           e.Price,
			Quantity:        e.Quantity,
			Currency:        e.Currency,
		}
		doc.Txs = append(doc.Txs, tx)
	}
	var b bytes.Buffer
	b.WriteString(xml.Header)
	enc := xml.NewEncoder(&b)
	enc.Indent("", "  ")
	if err := enc.Encode(doc); err != nil {
		return nil, fmt.Errorf("reporting: auth.016 marshal: %w", err)
	}
	return b.Bytes(), nil
}
