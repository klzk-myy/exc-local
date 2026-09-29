package orders

import (
	"testing"
	"time"

	"exchange/pkg/decimal"
	excerrors "exchange/pkg/errors"
)

func codeOf(t *testing.T, err error) string {
	t.Helper()
	if err == nil {
		t.Fatal("expected error")
	}
	var ce *excerrors.Error
	if e, ok := err.(*excerrors.Error); ok {
		ce = e
	} else {
		t.Fatalf("error %T is not a coded error: %v", err, err)
	}
	return ce.Code
}

func testInst() *Instrument {
	return &Instrument{
		ID: 1, Symbol: "EUR/USD", BaseCurrency: "EUR", QuoteCurrency: "USD",
		InstrumentType:   "SPOT",
		Status:           "ACTIVE",
		TickSize:         decimal.MustFromString("0.00001"),
		LotSize:          decimal.MustFromString("1000"),
		MinOrderQty:      decimal.MustFromString("1000"),
		MaxOrderQty:      decimal.MustFromString("10000000"),
		MinNotional:      decimal.MustFromString("100"),
		PriceBandPctUp:   decimal.MustFromString("10"),
		PriceBandPctDown: decimal.MustFromString("10"),
		MaxLeverage:      30,
	}
}

func testAcct() *Account {
	return &Account{ID: 7, Type: "SPOT", KycTier: "T1", Status: "ACTIVE"}
}

func d(s string) *decimal.Decimal { v := decimal.MustFromString(s); return &v }

func u64p(v uint64) *uint64 { return &v }

func submitReq() *SubmitRequest {
	return &SubmitRequest{
		Symbol: "EURUSD", Side: SideBuy, OrderType: TypeLimit,
		TimeInForce: TIFGTC, Quantity: d("1000"), Price: d("1.05000"),
	}
}

func TestValidateSubmitHappyPath(t *testing.T) {
	ref := decimal.MustFromString("1.05")
	if err := ValidateSubmit(submitReq(), testInst(), testAcct(), &ref, time.Now()); err != nil {
		t.Fatalf("valid submit rejected: %v", err)
	}
}

func TestValidateSubmitAccountGates(t *testing.T) {
	ref := decimal.MustFromString("1.05")
	cases := []struct {
		name string
		acct *Account
		want string
	}{
		{"nil account", nil, "UNAUTHORIZED"},
		{"frozen", &Account{ID: 1, Status: "FROZEN", KycTier: "T1"}, "ACCOUNT_FROZEN"},
		{"suspended", &Account{ID: 1, Status: "SUSPENDED", KycTier: "T1"}, "FORBIDDEN"},
		{"t0 kyc", &Account{ID: 1, Status: "ACTIVE", KycTier: "T0"}, "KYC_REQUIRED"},
	}
	for _, tc := range cases {
		if got := codeOf(t, ValidateSubmit(submitReq(), testInst(), tc.acct, &ref, time.Now())); got != tc.want {
			t.Errorf("%s: got %s want %s", tc.name, got, tc.want)
		}
	}
}

func TestValidateSubmitInstrumentStates(t *testing.T) {
	ref := decimal.MustFromString("1.05")
	for state, want := range map[string]string{
		"CANCEL_ONLY": "INSTRUMENT_CANCEL_ONLY",
		"SUSPENDED":   "INSTRUMENT_SUSPENDED",
		"DRAFT":       "INSTRUMENT_SUSPENDED",
		"HALTED":      "INSTRUMENT_HALTED",
		"DELISTED":    "INSTRUMENT_DELISTED",
		"BOGUS_STATE": "INSTRUMENT_SUSPENDED",
	} {
		inst := testInst()
		inst.Status = state
		if got := codeOf(t, ValidateSubmit(submitReq(), inst, testAcct(), &ref, time.Now())); got != want {
			t.Errorf("state %s: got %s want %s", state, got, want)
		}
	}
}

func TestValidateSubmitRestrictedLimitsOnly(t *testing.T) {
	ref := decimal.MustFromString("1.05")
	inst := testInst()
	inst.Status = "RESTRICTED"
	req := submitReq()
	req.OrderType = TypeMarket
	req.Price = nil
	if got := codeOf(t, ValidateSubmit(req, inst, testAcct(), &ref, time.Now())); got != "INSTRUMENT_RESTRICTED" {
		t.Fatalf("market on restricted: got %s", got)
	}
	// RESTRICTED still accepts limit orders.
	req = submitReq()
	if err := ValidateSubmit(req, inst, testAcct(), &ref, time.Now()); err != nil {
		t.Fatalf("limit on restricted rejected: %v", err)
	}
}

func TestValidateSubmitQuoteQuantityRules(t *testing.T) {
	ref := decimal.MustFromString("1.05")
	// Exactly one of quantity / quote_quantity on MARKET.
	req := &SubmitRequest{Symbol: "EURUSD", Side: SideBuy, OrderType: TypeMarket}
	if got := codeOf(t, ValidateSubmit(req, testInst(), testAcct(), &ref, time.Now())); got != "QUOTE_QUANTITY_INVALID" {
		t.Fatalf("neither qty: got %s", got)
	}
	req.Quantity, req.QuoteQuantity = d("1000"), d("1050")
	if got := codeOf(t, ValidateSubmit(req, testInst(), testAcct(), &ref, time.Now())); got != "QUOTE_QUANTITY_INVALID" {
		t.Fatalf("both qty: got %s", got)
	}
	req.Quantity = nil
	if err := ValidateSubmit(req, testInst(), testAcct(), &ref, time.Now()); err != nil {
		t.Fatalf("quote-only market rejected: %v", err)
	}
	// quote_quantity on a LIMIT is invalid.
	lim := submitReq()
	lim.QuoteQuantity = d("1050")
	if got := codeOf(t, ValidateSubmit(lim, testInst(), testAcct(), &ref, time.Now())); got != "QUOTE_QUANTITY_INVALID" {
		t.Fatalf("limit with quote qty: got %s", got)
	}
	// Non-positive quote_quantity.
	req.QuoteQuantity = d("0")
	if got := codeOf(t, ValidateSubmit(req, testInst(), testAcct(), &ref, time.Now())); got != "QUOTE_QUANTITY_INVALID" {
		t.Fatalf("zero quote qty: got %s", got)
	}
}

func TestValidateSubmitFilters(t *testing.T) {
	ref := decimal.MustFromString("1.05")
	// Off-lot quantity.
	req := submitReq()
	req.Quantity = d("1500")
	if got := codeOf(t, ValidateSubmit(req, testInst(), testAcct(), &ref, time.Now())); got != "INVALID_REQUEST" {
		t.Fatalf("off-lot: got %s", got)
	}
	// Below min qty.
	req = submitReq()
	req.Quantity = d("500")
	if got := codeOf(t, ValidateSubmit(req, testInst(), testAcct(), &ref, time.Now())); got != "INVALID_REQUEST" {
		t.Fatalf("min qty: got %s", got)
	}
	// Off-tick price (1.050005 is not a multiple of tick_size 0.00001).
	req = submitReq()
	req.Price = d("1.050005")
	if got := codeOf(t, ValidateSubmit(req, testInst(), testAcct(), &ref, time.Now())); got != "INVALID_REQUEST" {
		t.Fatalf("off-tick: got %s", got)
	}
	// Below min notional: 1000 × 0.05 = 50 < 100.
	req = submitReq()
	req.Price = d("0.05")
	ref2 := decimal.MustFromString("0.05")
	if got := codeOf(t, ValidateSubmit(req, testInst(), testAcct(), &ref2, time.Now())); got != "MIN_NOTIONAL_VIOLATION" {
		t.Fatalf("min notional: got %s", got)
	}
	// Outside price band: 10% up cap at 1.155.
	req = submitReq()
	req.Price = d("1.20")
	if got := codeOf(t, ValidateSubmit(req, testInst(), testAcct(), &ref, time.Now())); got != "PRICE_OUT_OF_BAND" {
		t.Fatalf("band: got %s", got)
	}
}

func TestValidateSubmitTIFRules(t *testing.T) {
	ref := decimal.MustFromString("1.05")
	req := submitReq()
	req.TimeInForce = "GTD"
	if got := codeOf(t, ValidateSubmit(req, testInst(), testAcct(), &ref, time.Now())); got != "INVALID_REQUEST" {
		t.Fatalf("GTD without expiry: got %s", got)
	}
	past := time.Now().Add(-time.Hour)
	req.GTDExpiry = &past
	if got := codeOf(t, ValidateSubmit(req, testInst(), testAcct(), &ref, time.Now())); got != "INVALID_REQUEST" {
		t.Fatalf("GTD past expiry: got %s", got)
	}
	fut := time.Now().Add(time.Hour)
	req.GTDExpiry = &fut
	if err := ValidateSubmit(req, testInst(), testAcct(), &ref, time.Now()); err != nil {
		t.Fatalf("valid GTD rejected: %v", err)
	}
}

func TestValidateSubmitTypeVocab(t *testing.T) {
	ref := decimal.MustFromString("1.05")
	req := submitReq()
	req.OrderType = "PEGGED" // Phase-16 type — fails closed on the wire enum
	if got := codeOf(t, ValidateSubmit(req, testInst(), testAcct(), &ref, time.Now())); got != "INVALID_REQUEST" {
		t.Fatalf("unsupported type: got %s", got)
	}
	req.OrderType = TypeLimit
	req.Side = "SHORT"
	if got := codeOf(t, ValidateSubmit(req, testInst(), testAcct(), &ref, time.Now())); got != "INVALID_REQUEST" {
		t.Fatalf("bad side: got %s", got)
	}
}

func TestStaleModify(t *testing.T) {
	if got := codeOf(t, StaleModify(nil, 5)); got != "INVALID_REQUEST" {
		t.Fatalf("missing seq: got %s", got)
	}
	if got := codeOf(t, StaleModify(u64p(4), 5)); got != "STALE_MODIFY" {
		t.Fatalf("stale seq: got %s", got)
	}
	if got := codeOf(t, StaleModify(u64p(9), 5)); got != "STALE_MODIFY" {
		t.Fatalf("future seq is stale too: got %s", got)
	}
	if err := StaleModify(u64p(5), 5); err != nil {
		t.Fatalf("matching seq rejected: %v", err)
	}
}

func openOrder() *Order {
	return &Order{
		ID: 42, AccountID: 7, InstrumentID: 1, Side: SideBuy,
		OrderType: TypeLimit, Quantity: decimal.MustFromString("1000"),
		Price: d("1.05000"), TimeInForce: TIFGTC, Status: "ACTIVE",
		OrderSeq: 5,
	}
}

func TestValidateModifyRules(t *testing.T) {
	inst := testInst()
	// Terminal order.
	o := openOrder()
	o.Status = "FILLED"
	mr := &ModifyRequest{Price: d("1.06"), OrderSeq: u64p(5)}
	if got := codeOf(t, ValidateModify(mr, o, inst)); got != "ORDER_NOT_FOUND" {
		t.Fatalf("terminal amend: got %s", got)
	}
	// IOC order.
	o = openOrder()
	o.TimeInForce = TIFIOC
	if got := codeOf(t, ValidateModify(mr, o, inst)); got != "ORDER_AMEND_REJECTED" {
		t.Fatalf("IOC amend: got %s", got)
	}
	// Amend TIF to IOC.
	o = openOrder()
	mr = &ModifyRequest{TimeInForce: TIFIOC, OrderSeq: u64p(5)}
	if got := codeOf(t, ValidateModify(mr, o, inst)); got != "ORDER_AMEND_REJECTED" {
		t.Fatalf("to-IOC amend: got %s", got)
	}
	// Quantity not above filled.
	o = openOrder()
	o.FilledQty = decimal.MustFromString("500")
	mr = &ModifyRequest{Quantity: d("400"), OrderSeq: u64p(5)}
	if got := codeOf(t, ValidateModify(mr, o, inst)); got != "ORDER_AMEND_REJECTED" {
		t.Fatalf("qty <= filled: got %s", got)
	}
	// No fields.
	o = openOrder()
	mr = &ModifyRequest{OrderSeq: u64p(5)}
	if got := codeOf(t, ValidateModify(mr, o, inst)); got != "INVALID_REQUEST" {
		t.Fatalf("empty amend: got %s", got)
	}
	// Price amend on MARKET order rejected.
	o = openOrder()
	o.OrderType = TypeMarket
	o.Price = nil
	mr = &ModifyRequest{Price: d("1.06"), OrderSeq: u64p(5)}
	if got := codeOf(t, ValidateModify(mr, o, inst)); got != "ORDER_AMEND_REJECTED" {
		t.Fatalf("market price amend: got %s", got)
	}
	// Valid.
	o = openOrder()
	mr = &ModifyRequest{Price: d("1.06"), OrderSeq: u64p(5)}
	if err := ValidateModify(mr, o, inst); err != nil {
		t.Fatalf("valid amend rejected: %v", err)
	}
}

func TestValidateKeepPriority(t *testing.T) {
	inst := testInst()
	inst.LotSize = decimal.MustFromString("100") // 500 is a valid decrease
	o := openOrder()
	// Equal quantity is not a decrease.
	kp := &KeepPriorityRequest{OrderSeq: u64p(5), Quantity: d("1000")}
	if got := codeOf(t, ValidateKeepPriority(kp, o, inst)); got != "ORDER_AMEND_REJECTED" {
		t.Fatalf("equal qty: got %s", got)
	}
	// Increase.
	kp.Quantity = d("2000")
	if got := codeOf(t, ValidateKeepPriority(kp, o, inst)); got != "ORDER_AMEND_REJECTED" {
		t.Fatalf("qty up: got %s", got)
	}
	// Missing quantity.
	kp.Quantity = nil
	if got := codeOf(t, ValidateKeepPriority(kp, o, inst)); got != "ORDER_AMEND_REJECTED" {
		t.Fatalf("no qty: got %s", got)
	}
	// Decrease is allowed.
	kp.Quantity = d("500")
	if err := ValidateKeepPriority(kp, o, inst); err != nil {
		t.Fatalf("qty down rejected: %v", err)
	}
	// Below filled still rejected.
	o.FilledQty = decimal.MustFromString("600")
	if got := codeOf(t, ValidateKeepPriority(kp, o, inst)); got != "ORDER_AMEND_REJECTED" {
		t.Fatalf("qty <= filled: got %s", got)
	}
}

func TestNormalizeMassCancelScope(t *testing.T) {
	s := MassCancelScope{AccountID: 7, Side: "ALL", OrderType: "ALL"}
	if err := NormalizeMassCancelScope(&s, false); err != nil {
		t.Fatalf("normalize: %v", err)
	}
	if s.Side != "" || s.OrderType != "" {
		t.Fatalf("ALL not normalized: %+v", s)
	}
	s = MassCancelScope{AccountID: 7, Side: "SHORT"}
	if got := codeOf(t, NormalizeMassCancelScope(&s, false)); got != "INVALID_REQUEST" {
		t.Fatalf("bad side: got %s", got)
	}
	s = MassCancelScope{AccountID: 7, OrderType: "PEGGED"}
	if got := codeOf(t, NormalizeMassCancelScope(&s, false)); got != "INVALID_REQUEST" {
		t.Fatalf("bad type: got %s", got)
	}
	// Non-admin without account context fails closed.
	s = MassCancelScope{}
	if got := codeOf(t, NormalizeMassCancelScope(&s, false)); got != "UNAUTHORIZED" {
		t.Fatalf("no account: got %s", got)
	}
	// Admin may pass account_id=0.
	s = MassCancelScope{}
	if err := NormalizeMassCancelScope(&s, true); err != nil {
		t.Fatalf("admin all-account scope rejected: %v", err)
	}
}

func TestSubmitHashStability(t *testing.T) {
	r1 := submitReq()
	r1.ClientOrderID = "c1"
	r2 := submitReq()
	r2.ClientOrderID = "c1"
	if submitHash(1, r1) != submitHash(1, r2) {
		t.Fatal("identical payloads hash differently")
	}
	r2.Price = d("1.06")
	if submitHash(1, r1) == submitHash(1, r2) {
		t.Fatal("different payloads hash the same")
	}
	// Hash covers instrument identity too.
	if submitHash(1, r1) == submitHash(2, r1) {
		t.Fatal("instrument not in hash")
	}
}

// --- body parsing -------------------------------------------------------------

func TestParseSubmitDecimalForms(t *testing.T) {
	req, err := ParseSubmit([]byte(`{
		"symbol":"EURUSD","side":"BUY","type":"LIMIT",
		"quantity":"1000","price":1.05,"time_in_force":"GTC"}`))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if req.Price == nil || !req.Price.Equal(decimal.MustFromString("1.05")) {
		t.Fatalf("numeric price not parsed exactly: %+v", req.Price)
	}
	// A float-unsafe payload must fail rather than round.
	_, err = ParseSubmit([]byte(`{"symbol":"EURUSD","side":"BUY","type":"LIMIT",
		"quantity":"1000","price":"abc"}`))
	if err == nil {
		t.Fatal("non-decimal price accepted")
	}
}

func TestParseModifySeqAcceptedAsStringOrNumber(t *testing.T) {
	for _, body := range []string{
		`{"order_seq":5,"price":"1.06"}`,
		`{"order_seq":"5","price":"1.06"}`,
	} {
		mr, err := ParseModify([]byte(body))
		if err != nil || mr.OrderSeq == nil || *mr.OrderSeq != 5 {
			t.Fatalf("seq parse %s: %+v err=%v", body, mr, err)
		}
	}
	// Negative / fractional rejected.
	if _, err := ParseModify([]byte(`{"order_seq":-1}`)); err == nil {
		t.Fatal("negative seq accepted")
	}
	if _, err := ParseModify([]byte(`{"order_seq":1.5}`)); err == nil {
		t.Fatal("fractional seq accepted")
	}
}
