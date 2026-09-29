// fix50sp2.go — Task 18.3.5: FIX 5.0 SP2 protocol surface for FX
// derivatives and institutional flow (spec §9.1/§9.2, §24 #55/#56).
//
// FIX 5.0 SP2 runs over the FIXT.1.1 session layer: wire frames carry
// BeginString(8)=FIXT.1.1 with DefaultApplVerID(1137)/ApplVerID(1128)=9.
// Session mechanics (heartbeat, sequence persistence, TLS 1.3) are the
// same machinery as FIX 4.4 — this file owns the application-layer
// differences: FX SecurityType(167) derivatives and the venue custom
// derivative tag block.
//
// Field persistence: derivative fields map onto orders.algo_params
// (migration 038 JSONB) with algo_type="FX_DERIVATIVE" until Phase-22
// lands the dedicated migration-039 derivative columns; the JSONB keys
// already use the 039 column vocabulary (strike, option_type,
// exercise_style, expiry_at, barrier_type, barrier_level, value_date,
// near_leg_value_date, far_leg_value_date, premium, fixing_date) so the
// Phase-22 cutover is a mechanical column lift.
//
// Repeating groups (NoPartyIDs) are parsed from RAW wire bytes —
// quickfixgo's FieldMap collapses repeated tags when no data dictionary
// is loaded, so ParseRawFields preserves occurrence order.
package fix

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/quickfixgo/quickfix"

	"exchange/internal/config"
	"exchange/internal/orders"
	"exchange/pkg/decimal"
)

// BeginString / ApplVerID for the 5.0 SP2 session layer.
const (
	BeginStringFIXT11 = "FIXT.1.1"
	ApplVerIDFIX50SP2 = "9"
)

// Additional tags for the derivatives surface (standard) + the venue
// custom block per spec §9.2 (9501–9509) and the FX party block
// (9018–9020).
const (
	TagPutOrCall        quickfix.Tag = 201
	TagMaturityDate     quickfix.Tag = 541
	TagMaturityTime     quickfix.Tag = 1079
	TagApplVerID        quickfix.Tag = 1128
	TagDefaultApplVerID quickfix.Tag = 1137

	// Spec §9.2 FX party block lives in dropcopy.go (9018/9019/9020 +
	// standard 453/447/448/452 — both accepted on parse).

	// Spec §9.2 + Task 18.3.5 derivative fields. (TagFXSettlementType
	// 9501 / TagFXSettlementDate 9502 are declared in dropcopy.go.)
	TagFXFixingDate       quickfix.Tag = 9503 // NDF fixing date
	TagFXBarrierLevel     quickfix.Tag = 9504
	TagFXBarrierType      quickfix.Tag = 9505
	TagFXExerciseStyle    quickfix.Tag = 9506 // EUROPEAN | AMERICAN
	TagFXPremium          quickfix.Tag = 9507
	TagFXNearLegValueDate quickfix.Tag = 9508 // swap near leg
	TagFXFarLegValueDate  quickfix.Tag = 9509 // swap far leg
)

// SecurityType(167) FX derivative values (spec §9.2).
const (
	SecTypeForward = "FORWARD"
	SecTypeSwap    = "SWAP"
	SecTypeNDF     = "NDF"
	SecTypeOption  = "OPT"
)

// SettlementType(9501) values (spec §6.3 vocabulary).
const (
	SettleTypeT1      = "T+1"
	SettleTypeT2      = "T+2"
	SettleTypeSameDay = "SAME_DAY"
)

// AlgoTypeFXDerivative is the algo_params discriminator carrying the
// derivative parameter blob until Phase-22's dedicated columns land.
// Unknown to the Phase-16 schema validators → size cap only (deliberate:
// sibling tasks own their strategy contracts).
const AlgoTypeFXDerivative = "FX_DERIVATIVE"

// PutOrCall(201) → option_type (migration-039 vocabulary).
func putOrCallToOptionType(v string) (string, *MappingError) {
	switch strings.ToUpper(strings.TrimSpace(v)) {
	case "0", "PUT":
		return "PUT", nil
	case "1", "CALL":
		return "CALL", nil
	}
	return "", &MappingError{Code: "INVALID_REQUEST",
		Detail:    fmt.Sprintf("PutOrCall(201)=%q unsupported", v),
		OrdReject: OrdRejReasonOther}
}

// ---------------------------------------------------------------------------
// Ordered raw tag-value scan — preserves repeated tags for group parsing.
// ---------------------------------------------------------------------------

// RawField is one tag=value pair in wire order.
type RawField struct {
	Tag   int
	Value string
}

// ParseRawFields tokenizes a complete FIX frame into ordered fields.
// It validates BeginString/BodyLength/MsgType presence and the trailing
// CheckSum value (not the checksum arithmetic — quickfix's transport
// layer already verified it when the message came off a session; raw
// callers outside a session must still validate transport integrity).
func ParseRawFields(raw []byte) ([]RawField, error) {
	var out []RawField
	for _, seg := range bytes.Split(raw, []byte{0x01}) {
		if len(seg) == 0 {
			continue
		}
		eq := bytes.IndexByte(seg, '=')
		if eq <= 0 {
			return nil, fmt.Errorf("fix: malformed field %q", string(seg))
		}
		tag, err := strconv.Atoi(string(seg[:eq]))
		if err != nil || tag <= 0 {
			return nil, fmt.Errorf("fix: bad tag %q", string(seg[:eq]))
		}
		out = append(out, RawField{Tag: tag, Value: string(seg[eq+1:])})
	}
	if len(out) < 3 || out[0].Tag != int(TagBeginString) ||
		out[1].Tag != int(TagBodyLength) || out[2].Tag != int(TagMsgType) {
		return nil, fmt.Errorf("fix: frame lacks BeginString/BodyLength/MsgType head")
	}
	return out, nil
}

// first returns the first occurrence of tag ("" absent).
func first(fs []RawField, tag quickfix.Tag) (string, bool) {
	t := int(tag)
	for _, f := range fs {
		if f.Tag == t {
			return f.Value, true
		}
	}
	return "", false
}

func firstRequired(fs []RawField, tag quickfix.Tag) (string, *MappingError) {
	if v, ok := first(fs, tag); ok && strings.TrimSpace(v) != "" {
		return strings.TrimSpace(v), nil
	}
	t := tag
	return "", &MappingError{Code: "INVALID_REQUEST",
		Detail: fmt.Sprintf("required tag %d missing or empty", int(tag)),
		RefTag: &t, OrdReject: OrdRejReasonOther}
}

func decField(fs []RawField, tag quickfix.Tag) (*decimal.Decimal, *MappingError) {
	v, ok := first(fs, tag)
	if !ok || strings.TrimSpace(v) == "" {
		return nil, nil
	}
	d, err := decimal.NewFromString(strings.TrimSpace(v))
	if err != nil {
		t := tag
		return nil, &MappingError{Code: "INVALID_REQUEST",
			Detail: fmt.Sprintf("tag %d value %q is not a valid decimal", int(tag), v),
			RefTag: &t, OrdReject: OrdRejReasonOther}
	}
	return &d, nil
}

// parseDate8 parses FIX LocalMktDate (YYYYMMDD) or full UTC timestamps.
func parseDate8(v string) (string, bool) {
	v = strings.TrimSpace(v)
	if len(v) >= 8 {
		if _, err := time.Parse("20060102", v[:8]); err == nil {
			return v[:4] + "-" + v[4:6] + "-" + v[6:8], true
		}
	}
	if _, err := time.Parse("2006-01-02", v); err == nil {
		return v, true
	}
	return "", false
}

// FXParty is one identified settlement/trading party (FIX 5.0 SP2 wire-parse shape).
type FXParty struct {
	ID     string `json:"id"`
	Source string `json:"source,omitempty"` // PartyIDSource (B=BIC, D=custom…)
	Role   string `json:"role,omitempty"`   // PartyRole numeric
}

// parseParties extracts the NoPartyIDs group. Both tag blocks are
// accepted: the spec §9.2 custom block (9018/9019/9020 + role 452) and
// the standard Parties group (453/448/447/452).
func parseParties(fs []RawField) []FXParty {
	var parties []FXParty
	cur := -1
	for _, f := range fs {
		switch f.Tag {
		case int(TagFXPartyID), 448:
			if len(parties) > 0 && parties[len(parties)-1].ID == "" {
				parties[len(parties)-1].ID = f.Value
			} else {
				parties = append(parties, FXParty{ID: f.Value})
			}
			cur = len(parties) - 1
		case int(TagFXPartyIDSource), 447:
			if cur >= 0 {
				parties[cur].Source = f.Value
			}
		case int(TagPartyRole):
			if cur >= 0 {
				parties[cur].Role = f.Value
			}
		}
	}
	return parties
}

// ---------------------------------------------------------------------------
// FIX 5.0 SP2 NewOrderSingle → SubmitRequest (+ derivative params)
// ---------------------------------------------------------------------------

// DerivativeParams is the algo_params JSONB payload for a derivative
// order (migration-039 column vocabulary).
type DerivativeParams struct {
	SecurityType     string    `json:"security_type"` // FORWARD|SWAP|NDF|OPT
	ValueDate        string    `json:"value_date,omitempty"`
	FixingDate       string    `json:"fixing_date,omitempty"`
	SettlementType   string    `json:"settlement_type,omitempty"`
	SettlementDate   string    `json:"settlement_date,omitempty"`
	Strike           *string   `json:"strike,omitempty"`
	OptionType       string    `json:"option_type,omitempty"`    // PUT|CALL
	ExerciseStyle    string    `json:"exercise_style,omitempty"` // EUROPEAN|AMERICAN
	ExpiryAt         string    `json:"expiry_at,omitempty"`      // MaturityDate
	BarrierLevel     *string   `json:"barrier_level,omitempty"`
	BarrierType      string    `json:"barrier_type,omitempty"`
	NearLegValueDate string    `json:"near_leg_value_date,omitempty"`
	FarLegValueDate  string    `json:"far_leg_value_date,omitempty"`
	Premium          *string   `json:"premium,omitempty"`
	Currency         string    `json:"currency,omitempty"` // Currency(15)
	DeliverToCompID  string    `json:"deliver_to_comp_id,omitempty"`
	Parties          []FXParty `json:"parties,omitempty"`
}

// MapNewOrderSingleSP2 converts a raw FIXT.1.1/FIX.5.0SP2 35=D frame
// into a SubmitRequest. Derivative orders (SecurityType present) carry
// their contract terms in AlgoParams under algo_type FX_DERIVATIVE.
// Spot fields validate exactly like the 4.4 path — MapSide/MapOrderType/
// MapTimeInForce semantics are reproduced on the raw field set so both
// protocol versions share one admission contract.
func MapNewOrderSingleSP2(raw []byte, sessionID string) (*orders.SubmitRequest, *MappingError) {
	fs, err := ParseRawFields(raw)
	if err != nil {
		return nil, &MappingError{Code: "INVALID_REQUEST", Detail: err.Error(),
			OrdReject: OrdRejReasonOther}
	}
	mt, _ := first(fs, TagMsgType)
	if mt != MsgNewOrderSingle {
		return nil, mapErr("INVALID_REQUEST", "MsgType(35)=%q, expected D", mt)
	}
	bs, _ := first(fs, TagBeginString)
	if bs != BeginStringFIXT11 && bs != "FIX.5.0SP2" {
		return nil, mapErr("INVALID_REQUEST",
			"BeginString(8)=%q — FIX 5.0 SP2 requires FIXT.1.1", bs)
	}

	clOrdID, merr := firstRequired(fs, TagClOrdID)
	if merr != nil {
		return nil, merr
	}
	if len(clOrdID) > 64 {
		return nil, mapErr("INVALID_REQUEST", "ClOrdID(11) exceeds 64 chars")
	}
	symbol, merr := firstRequired(fs, TagSymbol)
	if merr != nil {
		return nil, merr
	}
	sideStr, merr := firstRequired(fs, TagSide)
	if merr != nil {
		return nil, merr
	}
	var side string
	switch sideStr {
	case SideBuy:
		side = orders.SideBuy
	case SideSell:
		side = orders.SideSell
	default:
		return nil, mapErr("INVALID_REQUEST", "Side(54)=%q unsupported", sideStr)
	}
	ordTypeStr, merr := firstRequired(fs, TagOrdType)
	if merr != nil {
		return nil, merr
	}
	var ordType string
	switch ordTypeStr {
	case OrdTypeMarket:
		ordType = orders.TypeMarket
	case OrdTypeLimit:
		ordType = orders.TypeLimit
	case OrdTypeStop:
		ordType = orders.TypeStop
	case OrdTypeStopLimit:
		ordType = orders.TypeStopLimit
	default:
		return nil, mapErr("INVALID_REQUEST", "OrdType(40)=%q unsupported", ordTypeStr)
	}
	qty, merr := decField(fs, TagOrderQty)
	if merr != nil {
		return nil, merr
	}
	if qty == nil || !qty.IsPositive() {
		t := TagOrderQty
		return nil, &MappingError{Code: "INVALID_REQUEST",
			Detail: "OrderQty(38) missing or not positive",
			RefTag: &t, OrdReject: OrdRejReasonOther}
	}
	price, merr := decField(fs, TagPrice)
	if merr != nil {
		return nil, merr
	}
	tif := orders.TIFDAY
	var gtd *time.Time
	if v, ok := first(fs, TagTimeInForce); ok {
		switch strings.TrimSpace(v) {
		case "", TIFDay:
			tif = orders.TIFDAY
		case TIFGTC:
			tif = orders.TIFGTC
		case TIFIOC:
			tif = orders.TIFIOC
		case TIFFOK:
			tif = orders.TIFFOK
		case TIFGTD:
			tif = orders.TIFGTD
			ev, ok := first(fs, TagExpireTime)
			if !ok {
				t := TagExpireTime
				return nil, &MappingError{Code: "INVALID_REQUEST",
					Detail: "TimeInForce=GTD requires ExpireTime(126)",
					RefTag: &t, OrdReject: OrdRejReasonOther}
			}
			if parsed, perr := time.Parse("20060102-15:04:05", strings.TrimSpace(ev)); perr == nil {
				gtd = &parsed
			} else if parsed, perr := time.Parse("20060102-15:04:05.000", strings.TrimSpace(ev)); perr == nil {
				gtd = &parsed
			} else {
				return nil, mapErr("INVALID_REQUEST", "ExpireTime(126) %q unparsable", ev)
			}
		default:
			return nil, mapErr("INVALID_REQUEST", "TimeInForce(59)=%q unsupported", v)
		}
	}
	req := &orders.SubmitRequest{
		Symbol:        config.CanonicalSymbol(symbol),
		Side:          side,
		OrderType:     ordType,
		TimeInForce:   tif,
		ClientOrderID: clOrdID,
		Quantity:      qty,
		Price:         price,
		GTDExpiry:     gtd,
		SessionID:     sessionID,
	}

	// Derivative surface — SecurityType(167) presence selects the
	// derivative parameter path (spot orders pass through unchanged).
	secType, hasSec := first(fs, TagSecurityType)
	if !hasSec {
		return req, nil
	}
	dp, merr := mapDerivativeFields(fs, strings.ToUpper(strings.TrimSpace(secType)))
	if merr != nil {
		return nil, merr
	}
	blob, jerr := json.Marshal(dp)
	if jerr != nil {
		return nil, mapErr("INVALID_REQUEST", "derivative params encode: %v", jerr)
	}
	req.AlgoType = AlgoTypeFXDerivative
	req.AlgoParams = json.RawMessage(blob)
	return req, nil
}

// mapDerivativeFields validates the derivative tag block and builds the
// JSONB params. Per-contract required-field rules (spec §15.1):
// FORWARD needs a value date; NDF needs value + fixing dates; SWAP needs
// near+far leg value dates; OPT needs strike, option type and expiry.
func mapDerivativeFields(fs []RawField, secType string) (*DerivativeParams, *MappingError) {
	switch secType {
	case SecTypeForward, SecTypeSwap, SecTypeNDF, SecTypeOption:
	default:
		return nil, mapErr("INVALID_REQUEST",
			"SecurityType(167)=%q unsupported — FORWARD|SWAP|NDF|OPT", secType)
	}
	dp := &DerivativeParams{SecurityType: secType}
	dp.Parties = parseParties(fs)

	dateField := func(tag quickfix.Tag, dst *string) *MappingError {
		if v, ok := first(fs, tag); ok && strings.TrimSpace(v) != "" {
			norm, ok := parseDate8(v)
			if !ok {
				t := tag
				return &MappingError{Code: "INVALID_REQUEST",
					Detail: fmt.Sprintf("tag %d value %q is not a valid date", int(tag), v),
					RefTag: &t, OrdReject: OrdRejReasonOther}
			}
			*dst = norm
		}
		return nil
	}
	decFieldStr := func(tag quickfix.Tag) (*string, *MappingError) {
		d, merr := decField(fs, tag)
		if merr != nil {
			return nil, merr
		}
		if d == nil {
			return nil, nil
		}
		s := d.String()
		return &s, nil
	}
	strField := func(tag quickfix.Tag, dst *string) *MappingError {
		if v, ok := first(fs, tag); ok {
			*dst = strings.ToUpper(strings.TrimSpace(v))
		}
		return nil
	}

	for _, step := range []func() *MappingError{
		func() *MappingError { return dateField(TagFXSettlementDate, &dp.ValueDate) },
		func() *MappingError { return dateField(TagSettlDate, &dp.SettlementDate) },
		func() *MappingError { return dateField(TagFXFixingDate, &dp.FixingDate) },
		func() *MappingError { return dateField(TagFXNearLegValueDate, &dp.NearLegValueDate) },
		func() *MappingError { return dateField(TagFXFarLegValueDate, &dp.FarLegValueDate) },
		func() *MappingError { return dateField(TagMaturityDate, &dp.ExpiryAt) },
		func() *MappingError { return strField(TagFXSettlementType, &dp.SettlementType) },
		func() *MappingError { return strField(TagFXBarrierType, &dp.BarrierType) },
		func() *MappingError { return strField(TagFXExerciseStyle, &dp.ExerciseStyle) },
		func() *MappingError { return strField(TagCurrency, &dp.Currency) },
	} {
		if merr := step(); merr != nil {
			return nil, merr
		}
	}
	// DeliverToCompID(128) is a header field per FIX — accept from the
	// raw stream regardless of section.
	if v, ok := first(fs, TagDeliverToCompID); ok {
		dp.DeliverToCompID = strings.TrimSpace(v)
	}
	var merr *MappingError
	if dp.Strike, merr = decFieldStr(TagStrikePrice); merr != nil {
		return nil, merr
	}
	if dp.BarrierLevel, merr = decFieldStr(TagFXBarrierLevel); merr != nil {
		return nil, merr
	}
	if dp.Premium, merr = decFieldStr(TagFXPremium); merr != nil {
		return nil, merr
	}
	if v, ok := first(fs, TagPutOrCall); ok {
		pc, merr := putOrCallToOptionType(v)
		if merr != nil {
			return nil, merr
		}
		dp.OptionType = pc
	}
	if dp.SettlementType != "" {
		switch dp.SettlementType {
		case SettleTypeT1, SettleTypeT2, SettleTypeSameDay:
		default:
			return nil, mapErr("INVALID_REQUEST",
				"SettlementType(9501)=%q — expected T+1|T+2|SAME_DAY", dp.SettlementType)
		}
	}

	// Per-contract required fields.
	require := func(cond bool, detail string) *MappingError {
		if cond {
			return nil
		}
		return mapErr("INVALID_REQUEST", "%s", detail)
	}
	switch secType {
	case SecTypeForward:
		if merr := require(dp.ValueDate != "" || dp.SettlementDate != "",
			"FORWARD requires value date (9502 or 64)"); merr != nil {
			return nil, merr
		}
	case SecTypeNDF:
		if merr := require(dp.ValueDate != "" || dp.SettlementDate != "",
			"NDF requires value date (9502 or 64)"); merr != nil {
			return nil, merr
		}
		if merr := require(dp.FixingDate != "",
			"NDF requires FixingDate(9503)"); merr != nil {
			return nil, merr
		}
	case SecTypeSwap:
		if merr := require(dp.NearLegValueDate != "" && dp.FarLegValueDate != "",
			"SWAP requires near+far leg value dates (9508/9509)"); merr != nil {
			return nil, merr
		}
	case SecTypeOption:
		if merr := require(dp.Strike != nil,
			"OPT requires StrikePrice(202)"); merr != nil {
			return nil, merr
		}
		if merr := require(dp.OptionType != "",
			"OPT requires PutOrCall(201)"); merr != nil {
			return nil, merr
		}
		if merr := require(dp.ExpiryAt != "",
			"OPT requires MaturityDate(541)"); merr != nil {
			return nil, merr
		}
	}
	return dp, nil
}

// ---------------------------------------------------------------------------
// FIX 5.0 SP2 ExecutionReport — same report shape as 4.4 plus the
// derivative echo block.
// ---------------------------------------------------------------------------

// DerivativeEcho re-emits the derivative contract fields on the
// ExecutionReport (§24 #55: exec reports for derivative orders via
// 5.0 SP2). Built from the stored order's algo_params blob.
func DerivativeEcho(m *quickfix.Message, algoParams json.RawMessage) {
	if len(algoParams) == 0 {
		return
	}
	var dp DerivativeParams
	if err := json.Unmarshal(algoParams, &dp); err != nil {
		return
	}
	setEcho := func(tag quickfix.Tag, v string) {
		if v != "" {
			m.Body.SetString(tag, v)
		}
	}
	setEcho(TagSecurityType, dp.SecurityType)
	setEcho(TagFXSettlementType, dp.SettlementType)
	setEcho(TagFXSettlementDate, compactDate(dp.ValueDate))
	setEcho(TagFXFixingDate, compactDate(dp.FixingDate))
	setEcho(TagFXExerciseStyle, dp.ExerciseStyle)
	setEcho(TagFXBarrierType, dp.BarrierType)
	setEcho(TagMaturityDate, compactDate(dp.ExpiryAt))
	setEcho(TagFXNearLegValueDate, compactDate(dp.NearLegValueDate))
	setEcho(TagFXFarLegValueDate, compactDate(dp.FarLegValueDate))
	setEcho(TagCurrency, dp.Currency)
	if dp.Strike != nil {
		m.Body.SetString(TagStrikePrice, *dp.Strike)
	}
	if dp.BarrierLevel != nil {
		m.Body.SetString(TagFXBarrierLevel, *dp.BarrierLevel)
	}
	if dp.Premium != nil {
		m.Body.SetString(TagFXPremium, *dp.Premium)
	}
	switch dp.OptionType {
	case "PUT":
		m.Body.SetString(TagPutOrCall, "0")
	case "CALL":
		m.Body.SetString(TagPutOrCall, "1")
	}
	// Spec §9.2 custom party block echo.
	if len(dp.Parties) > 0 {
		m.Body.SetInt(TagFXNoPartyIDs, len(dp.Parties))
		grp := quickfix.NewRepeatingGroup(TagFXNoPartyIDs,
			quickfix.GroupTemplate{
				quickfix.GroupElement(TagFXPartyID),
				quickfix.GroupElement(TagFXPartyIDSource),
				quickfix.GroupElement(TagPartyRole),
			})
		for _, p := range dp.Parties {
			g := grp.Add()
			g.SetString(TagFXPartyID, p.ID)
			if p.Source != "" {
				g.SetString(TagFXPartyIDSource, p.Source)
			}
			if p.Role != "" {
				g.SetString(TagPartyRole, p.Role)
			}
		}
		m.Body.SetGroup(grp)
	}
}

// compactDate renders "YYYY-MM-DD" back to FIX LocalMktDate YYYYMMDD.
func compactDate(v string) string {
	return strings.ReplaceAll(v, "-", "")
}

// SP2ApplVer stamps ApplVerID(1128)=9 on an outbound message — the
// FIXT.1.1 convention for FIX 5.0 SP2 application-level framing.
func SP2ApplVer(m *quickfix.Message) *quickfix.Message {
	m.Header.SetString(TagApplVerID, ApplVerIDFIX50SP2)
	return m
}

// isSP2Session reports whether the quickfix session runs the FIXT.1.1
// transport (the 5.0 SP2 acceptor). FIXT.1.1 sessions route through the
// raw-bytes mapper + SP2-flavoured reports; FIX.4.4 sessions use the
// FieldMap path unchanged.
func isSP2Session(sessionID quickfix.SessionID) bool {
	return sessionID.BeginString == BeginStringFIXT11
}

// reportAcceptedFor returns the session-appropriate accept report:
// SP2 (ApplVerID + derivative echo) on FIXT.1.1, plain 4.4 otherwise.
func reportAcceptedFor(sessionID quickfix.SessionID, o *orders.Order,
	ack *orders.Ack) *quickfix.Message {
	if isSP2Session(sessionID) {
		return ReportAcceptedSP2(o, ack)
	}
	return reportAccepted(o, ack)
}

// reportRejectedFor is the matching reject report.
func reportRejectedFor(sessionID quickfix.SessionID, clOrdID, symbol,
	side string, err error) *quickfix.Message {
	if isSP2Session(sessionID) {
		return ReportRejectedSP2(clOrdID, symbol, side, err)
	}
	return reportRejected(clOrdID, symbol, side, err)
}

// reportCanceledFor is the matching cancel confirmation.
func reportCanceledFor(sessionID quickfix.SessionID, o *orders.Order,
	origClOrdID string) *quickfix.Message {
	if isSP2Session(sessionID) {
		return ReportCanceledSP2(o, origClOrdID)
	}
	return reportCanceled(o, origClOrdID)
}

// reportReplacedFor is the matching replace confirmation.
func reportReplacedFor(sessionID quickfix.SessionID, o *orders.Order,
	origClOrdID string) *quickfix.Message {
	m := reportReplaced(o, origClOrdID)
	if isSP2Session(sessionID) {
		SP2ApplVer(m)
		if o != nil {
			DerivativeEcho(m, o.AlgoParams)
		}
	}
	return m
}

// ReportAcceptedSP2 wraps reportAccepted with the SP2 application
// version + derivative echo — the 5.0 SP2 accept-callback used by the
// order-entry path (Task 18.3.5 AC).
func ReportAcceptedSP2(o *orders.Order, ack *orders.Ack) *quickfix.Message {
	m := SP2ApplVer(reportAccepted(o, ack))
	if o != nil {
		DerivativeEcho(m, o.AlgoParams)
	}
	return m
}

// ReportFillSP2 is the SP2 fill report with derivative echo.
func ReportFillSP2(o *orders.Order, lastPx, lastQty decimal.Decimal) *quickfix.Message {
	m := SP2ApplVer(reportFill(o, lastPx, lastQty))
	if o != nil {
		DerivativeEcho(m, o.AlgoParams)
	}
	return m
}

// ReportRejectedSP2 is the SP2 order rejection.
func ReportRejectedSP2(clOrdID, symbol, side string, err error) *quickfix.Message {
	return SP2ApplVer(reportRejected(clOrdID, symbol, side, err))
}

// ReportCanceledSP2 is the SP2 cancel confirmation.
func ReportCanceledSP2(o *orders.Order, origClOrdID string) *quickfix.Message {
	m := SP2ApplVer(reportCanceled(o, origClOrdID))
	if o != nil {
		DerivativeEcho(m, o.AlgoParams)
	}
	return m
}
