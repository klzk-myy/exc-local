// fix50sp2_test.go — Task 18.3.5: derivative tag mapping onto
// algo_params, per-contract required fields, ExecutionReport echo.
package fix

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/quickfixgo/quickfix"
)

// frame assembles a raw FIXT.1.1 frame body (fields after the head
// triple; checksum/length computed for realism though the parser is
// structural).
func frame(fields string) []byte {
	body := fields
	payload := fmt.Sprintf("35=D\x01%s", body)
	head := fmt.Sprintf("8=FIXT.1.1\x019=%d\x01", len(payload))
	msg := head + payload + "10=000\x01"
	return []byte(msg)
}

func baseOrder() string {
	return "11=ord-1\x0155=EURUSD\x0154=1\x0138=1000000\x0140=2\x0144=1.1000\x0159=1\x01"
}

func algoParams(t *testing.T, raw []byte) (*DerivativeParams, string) {
	t.Helper()
	req, merr := MapNewOrderSingleSP2(raw, "sess-1")
	if merr != nil {
		t.Fatalf("map: %v", merr)
	}
	if req.AlgoType != AlgoTypeFXDerivative {
		t.Fatalf("algo_type %q", req.AlgoType)
	}
	var dp DerivativeParams
	if err := json.Unmarshal(req.AlgoParams, &dp); err != nil {
		t.Fatalf("algo_params not valid JSON: %v", err)
	}
	return &dp, string(req.AlgoParams)
}

func mapFails(t *testing.T, raw []byte, wantSub string) *MappingError {
	t.Helper()
	_, merr := MapNewOrderSingleSP2(raw, "sess-1")
	if merr == nil {
		t.Fatalf("expected mapping failure %q", wantSub)
	}
	if wantSub != "" && !strings.Contains(merr.Detail, wantSub) {
		t.Fatalf("error %q missing %q", merr.Detail, wantSub)
	}
	return merr
}

func TestSpotPassthrough(t *testing.T) {
	// No SecurityType → plain spot order, no algo_params.
	req, merr := MapNewOrderSingleSP2(frame(baseOrder()), "s1")
	if merr != nil {
		t.Fatalf("map: %v", merr)
	}
	if req.AlgoType != "" || len(req.AlgoParams) != 0 {
		t.Fatalf("spot order gained derivative params: %s", req.AlgoParams)
	}
	if req.Symbol != "EUR/USD" || req.Side != "BUY" || req.OrderType != "LIMIT" {
		t.Fatalf("spot mapping: %+v", req)
	}
}

func TestForwardMapping(t *testing.T) {
	dp, blob := algoParams(t, frame(baseOrder()+
		"167=FORWARD\x019502=20251015\x019501=T+1\x0115=USD\x01"))
	if dp.SecurityType != SecTypeForward || dp.ValueDate != "2025-10-15" ||
		dp.SettlementType != "T+1" || dp.Currency != "USD" {
		t.Fatalf("forward params: %+v", dp)
	}
	if !strings.Contains(blob, `"value_date":"2025-10-15"`) {
		t.Fatalf("blob: %s", blob)
	}
}

func TestNDFRequiresFixingDate(t *testing.T) {
	// Missing FixingDate(9503) → reject.
	merr := mapFails(t, frame(baseOrder()+
		"167=NDF\x019502=20251015\x01"), "FixingDate")
	if merr.Code != "INVALID_REQUEST" {
		t.Fatalf("code %s", merr.Code)
	}
	// Complete NDF maps.
	dp, _ := algoParams(t, frame(baseOrder()+
		"167=NDF\x019502=20251015\x019503=20251013\x01"))
	if dp.FixingDate != "2025-10-13" {
		t.Fatalf("fixing date %q", dp.FixingDate)
	}
}

func TestSwapLegDates(t *testing.T) {
	// Missing far leg → reject.
	mapFails(t, frame(baseOrder()+
		"167=SWAP\x019508=20251015\x01"), "9508/9509")
	dp, _ := algoParams(t, frame(baseOrder()+
		"167=SWAP\x019508=20251015\x019509=20251115\x01"))
	if dp.NearLegValueDate != "2025-10-15" || dp.FarLegValueDate != "2025-11-15" {
		t.Fatalf("swap legs: %+v", dp)
	}
}

func TestOptionContract(t *testing.T) {
	// OPT missing strike/PutOrCall/maturity → each rejects.
	mapFails(t, frame(baseOrder()+"167=OPT\x01"), "StrikePrice")
	mapFails(t, frame(baseOrder()+
		"167=OPT\x01202=1.10\x01"), "PutOrCall")
	mapFails(t, frame(baseOrder()+
		"167=OPT\x01202=1.10\x01201=1\x01"), "MaturityDate")
	dp, _ := algoParams(t, frame(baseOrder()+
		"167=OPT\x01202=1.10\x01201=1\x01541=20251215\x01"+
		"9506=EUROPEAN\x019507=0.005\x019504=1.15\x019505=UP_AND_OUT\x01"))
	if dp.OptionType != "CALL" || dp.Strike == nil || *dp.Strike != "1.1" ||
		dp.ExpiryAt != "2025-12-15" || dp.ExerciseStyle != "EUROPEAN" ||
		dp.Premium == nil || *dp.Premium != "0.005" ||
		dp.BarrierLevel == nil || *dp.BarrierLevel != "1.15" ||
		dp.BarrierType != "UP_AND_OUT" {
		t.Fatalf("option params: %+v", dp)
	}
}

func TestSettlementDateStandardTag(t *testing.T) {
	// SettlDate(64) serves as the FORWARD value date.
	dp, _ := algoParams(t, frame(baseOrder()+
		"167=FORWARD\x0164=20251020\x01"))
	if dp.SettlementDate != "2025-10-20" {
		t.Fatalf("settlement date %q", dp.SettlementDate)
	}
}

func TestPartyGroupParse(t *testing.T) {
	dp, _ := algoParams(t, frame(baseOrder()+
		"167=FORWARD\x019502=20251015\x01"+
		"9018=2\x019020=FIRMA\x019019=D\x01452=1\x019020=CLEARBANK\x019019=B\x01452=13\x01"))
	if len(dp.Parties) != 2 {
		t.Fatalf("parties: %+v", dp.Parties)
	}
	if dp.Parties[0].ID != "FIRMA" || dp.Parties[0].Role != "1" {
		t.Fatalf("party0: %+v", dp.Parties[0])
	}
	if dp.Parties[1].ID != "CLEARBANK" || dp.Parties[1].Source != "B" ||
		dp.Parties[1].Role != "13" {
		t.Fatalf("party1: %+v", dp.Parties[1])
	}
}

func TestMalformedAndBadValues(t *testing.T) {
	// Missing ClOrdID.
	mapFails(t, frame("55=EURUSD\x0154=1\x0138=1\x0140=1\x01"), "11")
	// Wrong MsgType.
	_, merr := MapNewOrderSingleSP2(
		[]byte("8=FIXT.1.1\x019=5\x0135=W\x0110=000\x01"), "s")
	if merr == nil || !strings.Contains(merr.Detail, "expected D") {
		t.Fatalf("msgtype check: %v", merr)
	}
	// Wrong BeginString.
	_, merr = MapNewOrderSingleSP2(
		[]byte("8=FIX.4.4\x019=30\x0135=D\x0111=x\x0155=EURUSD\x0154=1\x0138=1\x0140=1\x0110=000\x01"), "s")
	if merr == nil || !strings.Contains(merr.Detail, "FIXT.1.1") {
		t.Fatalf("beginstring check: %v", merr)
	}
	// Bad decimal tag.
	mapFails(t, frame(baseOrder()+"167=OPT\x01202=abc\x01"), "not a valid decimal")
	// Bad date tag.
	mapFails(t, frame(baseOrder()+"167=FORWARD\x019502=notadate\x01"), "not a valid date")
	// Unsupported SecurityType.
	mapFails(t, frame(baseOrder()+"167=CRYPTO\x01"), "unsupported")
	// Unsupported SettlementType.
	mapFails(t, frame(baseOrder()+"167=FORWARD\x019502=20251015\x019501=T+30\x01"), "9501")
	// Non-positive qty.
	mapFails(t, frame("11=x\x0155=EURUSD\x0154=1\x0138=0\x0140=1\x01"), "OrderQty")
	// GTD without ExpireTime.
	mapFails(t, frame("11=x\x0155=EURUSD\x0154=1\x0138=1\x0140=1\x0159=6\x01"), "ExpireTime")
}

// TestSP2ExecutionReportEcho — derivative contract fields round-trip
// onto the 5.0 SP2 ExecutionReport.
func TestSP2ExecutionReportEcho(t *testing.T) {
	dp := DerivativeParams{
		SecurityType:   SecTypeNDF,
		ValueDate:      "2025-10-15",
		FixingDate:     "2025-10-13",
		SettlementType: "T+1",
		Currency:       "USD",
		Parties:        []FXParty{{ID: "FIRMA", Source: "D", Role: "1"}},
	}
	blob, _ := json.Marshal(dp)
	m := quickfix.NewMessage()
	DerivativeEcho(m, blob)
	sec, err := m.Body.GetString(TagSecurityType)
	if err != nil || sec != "NDF" {
		t.Fatalf("SecurityType echo: %q %v", sec, err)
	}
	fix, _ := m.Body.GetString(TagFXFixingDate)
	if fix != "20251013" {
		t.Fatalf("fixing date echo %q", fix)
	}
	st, _ := m.Body.GetString(TagFXSettlementType)
	if st != "T+1" {
		t.Fatalf("settlement type echo %q", st)
	}
	// Party block echoed.
	n, err := m.Body.GetInt(TagFXNoPartyIDs)
	if err != nil || n != 1 {
		t.Fatalf("party count echo: %d %v", n, err)
	}
	// Empty params → no panic, no fields.
	m2 := quickfix.NewMessage()
	DerivativeEcho(m2, nil)
	if _, err := m2.Body.GetString(TagSecurityType); err == nil {
		t.Fatal("empty echo wrote SecurityType")
	}
}

// TestApplVerStamp — SP2 reports carry ApplVerID(1128)=9.
func TestApplVerStamp(t *testing.T) {
	m := SP2ApplVer(quickfix.NewMessage())
	v, err := m.Header.GetString(TagApplVerID)
	if err != nil || v != "9" {
		t.Fatalf("ApplVerID: %q %v", v, err)
	}
}
