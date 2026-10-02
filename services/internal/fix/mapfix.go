// FIX wire ↔ internal pipeline mapping (Task 18.3.2 item "FIX→internal
// map" + order status map). All conversion is total and pure — every
// unmapped/invalid input returns a coded reject, never a panic.
package fix

import (
	"fmt"
	"strings"
	"time"

	"github.com/quickfixgo/quickfix"

	"exchange/internal/config"
	"exchange/internal/orders"
	"exchange/pkg/decimal"
)

// MappingError carries a client-visible reject: code is a canonical
// registry code (SESSION_NOT_ENTITLED, INVALID_REQUEST, …) rendered in
// Text(58); rejReason is the numeric OrdRejReason(103)/BusinessReject
// companion.
type MappingError struct {
	Code      string
	Detail    string
	RefTag    *quickfix.Tag
	OrdReject int // OrdRejReason(103) for order rejects
}

func (e *MappingError) Error() string {
	if e.Detail != "" {
		return e.Code + ": " + e.Detail
	}
	return e.Code
}

func mapErr(code, format string, args ...any) *MappingError {
	return &MappingError{Code: code, Detail: fmt.Sprintf(format, args...),
		OrdReject: OrdRejReasonOther}
}

func required(msg *quickfix.Message, tag quickfix.Tag) (string, *MappingError) {
	v, err := msg.Body.GetString(tag)
	if err != nil || strings.TrimSpace(v) == "" {
		t := tag
		return "", &MappingError{Code: "INVALID_REQUEST",
			Detail:    fmt.Sprintf("required tag %d missing or empty", int(tag)),
			RefTag:    &t,
			OrdReject: OrdRejReasonOther}
	}
	return strings.TrimSpace(v), nil
}

func optionalStr(msg *quickfix.Message, tag quickfix.Tag) string {
	v, err := msg.Body.GetString(tag)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(v)
}

func optionalDec(msg *quickfix.Message, tag quickfix.Tag) (*decimal.Decimal, *MappingError) {
	v, err := msg.Body.GetString(tag)
	if err != nil {
		return nil, nil
	}
	d, derr := decimal.NewFromString(strings.TrimSpace(v))
	if derr != nil {
		t := tag
		return nil, &MappingError{Code: "INVALID_REQUEST",
			Detail:    fmt.Sprintf("tag %d value %q is not a valid decimal", int(tag), v),
			RefTag:    &t,
			OrdReject: OrdRejReasonOther}
	}
	return &d, nil
}

// MapOrderType converts OrdType(40) → internal vocabulary. Unmapped
// values reject (forex F/G/H class values are Phase-22/18.3.5 scope).
func MapOrderType(v string) (string, *MappingError) {
	switch v {
	case OrdTypeMarket:
		return orders.TypeMarket, nil
	case OrdTypeLimit:
		return orders.TypeLimit, nil
	case OrdTypeStop:
		return orders.TypeStop, nil
	case OrdTypeStopLimit:
		return orders.TypeStopLimit, nil
	}
	return "", &MappingError{Code: "INVALID_REQUEST",
		Detail:    fmt.Sprintf("OrdType(40)=%q unsupported", v),
		OrdReject: OrdRejReasonOther}
}

// MapTimeInForce converts TimeInForce(59) → internal vocabulary.
// Missing tag defaults to DAY (FIX convention: 0=Day is the absent
// default). GTD additionally requires ExpireTime(126).
func MapTimeInForce(msg *quickfix.Message) (string, *time.Time, *MappingError) {
	v, err := msg.Body.GetString(TagTimeInForce)
	if err != nil {
		return orders.TIFDAY, nil, nil
	}
	switch strings.TrimSpace(v) {
	case "", TIFDay:
		return orders.TIFDAY, nil, nil
	case TIFGTC:
		return orders.TIFGTC, nil, nil
	case TIFIOC:
		return orders.TIFIOC, nil, nil
	case TIFFOK:
		return orders.TIFFOK, nil, nil
	case TIFGTD:
		exp, gerr := msg.Body.GetTime(TagExpireTime)
		if gerr != nil {
			t := TagExpireTime
			return "", nil, &MappingError{Code: "INVALID_REQUEST",
				Detail:    "TimeInForce=GTD requires ExpireTime(126)",
				RefTag:    &t,
				OrdReject: OrdRejReasonOther}
		}
		return orders.TIFGTD, &exp, nil
	}
	return "", nil, &MappingError{Code: "INVALID_REQUEST",
		Detail:    fmt.Sprintf("TimeInForce(59)=%q unsupported", v),
		OrdReject: OrdRejReasonOther}
}

// MapSide converts Side(54) → internal vocabulary.
func MapSide(v string) (string, *MappingError) {
	switch v {
	case SideBuy:
		return orders.SideBuy, nil
	case SideSell:
		return orders.SideSell, nil
	}
	return "", &MappingError{Code: "INVALID_REQUEST",
		Detail:    fmt.Sprintf("Side(54)=%q unsupported", v),
		OrdReject: OrdRejReasonOther}
}

// MapNewOrderSingle converts an inbound 35=D into the canonical
// SubmitRequest. Symbol is canonicalized to the orders pipeline's
// form; sessionID is stamped for CoD attribution (migration 155
// session_id). Price is required for LIMIT/STOP_LIMIT; StopPx for
// STOP/STOP_LIMIT; MaxFloor(111) upgrades a limit order to ICEBERG.
func MapNewOrderSingle(msg *quickfix.Message, sessionID string) (*orders.SubmitRequest, *MappingError) {
	clOrdID, merr := required(msg, TagClOrdID)
	if merr != nil {
		return nil, merr
	}
	if len(clOrdID) > 64 {
		return nil, mapErr("INVALID_REQUEST", "ClOrdID(11) exceeds 64 chars")
	}
	symbol, merr := required(msg, TagSymbol)
	if merr != nil {
		return nil, merr
	}
	sideStr, merr := required(msg, TagSide)
	if merr != nil {
		return nil, merr
	}
	side, merr := MapSide(sideStr)
	if merr != nil {
		return nil, merr
	}
	ordTypeStr, merr := required(msg, TagOrdType)
	if merr != nil {
		return nil, merr
	}
	ordType, merr := MapOrderType(ordTypeStr)
	if merr != nil {
		return nil, merr
	}
	tif, gtd, merr := MapTimeInForce(msg)
	if merr != nil {
		return nil, merr
	}
	qty, merr := optionalDec(msg, TagOrderQty)
	if merr != nil {
		return nil, merr
	}
	if qty == nil || !qty.IsPositive() {
		t := TagOrderQty
		return nil, &MappingError{Code: "INVALID_REQUEST",
			Detail:    "OrderQty(38) missing or not positive",
			RefTag:    &t,
			OrdReject: OrdRejReasonOther}
	}
	price, merr := optionalDec(msg, TagPrice)
	if merr != nil {
		return nil, merr
	}
	stopPx, merr := optionalDec(msg, TagStopPx)
	if merr != nil {
		return nil, merr
	}
	display, merr := optionalDec(msg, TagMaxFloor)
	if merr != nil {
		return nil, merr
	}

	// Venue custom trailing/discretionary block (tags 20003–20006):
	// a trailing stop is OrdType=3 with the distance pair present; its
	// initial StopPx may be absent — the engine trails from the anchor.
	trailOff, merr := optionalDec(msg, TagTrailingOffset)
	if merr != nil {
		return nil, merr
	}
	trailUnit := ""
	if msg.Body.Has(TagTrailingOffsetUnit) {
		u, uerr := msg.Body.GetString(TagTrailingOffsetUnit)
		if uerr != nil {
			t := TagTrailingOffsetUnit
			return nil, &MappingError{Code: "INVALID_REQUEST",
				Detail:    "TrailingOffsetUnit(20004) must be a string",
				RefTag:    &t,
				OrdReject: OrdRejReasonOther}
		}
		trailUnit = strings.ToUpper(strings.TrimSpace(u))
	}
	actPx, merr := optionalDec(msg, TagActivationPrice)
	if merr != nil {
		return nil, merr
	}
	discOff, merr := optionalDec(msg, TagDiscretionaryOffPip)
	if merr != nil {
		return nil, merr
	}
	trailing := trailOff != nil || trailUnit != ""

	switch ordType {
	case orders.TypeLimit:
		if price == nil {
			t := TagPrice
			return nil, &MappingError{Code: "INVALID_REQUEST",
				Detail:    "OrdType=Limit requires Price(44)",
				RefTag:    &t,
				OrdReject: OrdRejReasonOther}
		}
	case orders.TypeStop:
		if stopPx == nil && !trailing {
			t := TagStopPx
			return nil, &MappingError{Code: "INVALID_REQUEST",
				Detail:    "OrdType=Stop requires StopPx(99) (or the 20003/20004 trailing pair)",
				RefTag:    &t,
				OrdReject: OrdRejReasonOther}
		}
	case orders.TypeStopLimit:
		if price == nil || stopPx == nil {
			return nil, mapErr("INVALID_REQUEST",
				"OrdType=StopLimit requires Price(44) and StopPx(99)")
		}
	}
	if trailing && ordType != orders.TypeStop {
		t := TagOrdType
		return nil, &MappingError{Code: "INVALID_REQUEST",
			Detail:    "trailing fields (20003/20004) require OrdType(40)=3 Stop",
			RefTag:    &t,
			OrdReject: OrdRejReasonOther}
	}
	if discOff != nil && ordType != orders.TypeLimit {
		return nil, mapErr("INVALID_REQUEST",
			"DiscretionaryOffsetPips(20006) is only valid on OrdType=Limit")
	}
	if display != nil {
		if ordType != orders.TypeLimit {
			return nil, mapErr("INVALID_REQUEST",
				"MaxFloor(111) is only valid on OrdType=Limit")
		}
		if !display.IsPositive() || display.GreaterThanOrEqual(*qty) {
			return nil, mapErr("INVALID_REQUEST",
				"MaxFloor(111) must be positive and less than OrderQty(38)")
		}
		ordType = orders.TypeIceberg
	}

	return &orders.SubmitRequest{
		Symbol:                  config.CanonicalSymbol(symbol),
		Side:                    side,
		OrderType:               ordType,
		TimeInForce:             tif,
		ClientOrderID:           clOrdID,
		Quantity:                qty,
		Price:                   price,
		StopPrice:               stopPx,
		DisplayQty:              display,
		GTDExpiry:               gtd,
		TrailingOffset:          trailOff,
		TrailingOffsetUnit:      trailUnit,
		ActivationPrice:         actPx,
		DiscretionaryOffsetPips: discOff,
		SessionID:               sessionID,
		CoDExempt:               codExempt(msg),
	}, nil
}

// codExempt reads venue tag 9510 (spec §9.9 COD_EXEMPT): truthy values
// Y/1/TRUE mark the order exempt from the session-scope
// cancel-on-disconnect sweep — every other mass-cancel reason ignores it.
func codExempt(msg *quickfix.Message) bool {
	switch strings.ToUpper(strings.TrimSpace(optionalStr(msg, TagCODExempt))) {
	case "Y", "1", "TRUE":
		return true
	default:
		return false
	}
}

// MapCancelReplace builds the internal CancelReplaceRequest from a
// 35=G. Only fields the client actually sent are set — absent fields
// keep their book value.
func MapCancelReplace(msg *quickfix.Message) (*orders.CancelReplaceRequest, *MappingError) {
	price, merr := optionalDec(msg, TagPrice)
	if merr != nil {
		return nil, merr
	}
	qty, merr := optionalDec(msg, TagOrderQty)
	if merr != nil {
		return nil, merr
	}
	stopPx, merr := optionalDec(msg, TagStopPx)
	if merr != nil {
		return nil, merr
	}
	display, merr := optionalDec(msg, TagMaxFloor)
	if merr != nil {
		return nil, merr
	}
	tif, gtd, merr := MapTimeInForce(msg)
	if merr != nil {
		return nil, merr
	}
	tifStr := ""
	if msg.Body.Has(TagTimeInForce) {
		tifStr = tif
	}
	if price == nil && qty == nil && stopPx == nil && display == nil && tifStr == "" {
		return nil, mapErr("INVALID_REQUEST",
			"OrderCancelReplaceRequest carries no mutable fields")
	}
	return &orders.CancelReplaceRequest{
		Mode: "STOP_ON_FAILURE",
		ModifyRequest: orders.ModifyRequest{
			Price:       price,
			Quantity:    qty,
			StopPrice:   stopPx,
			DisplayQty:  display,
			TimeInForce: tifStr,
			GTDExpiry:   gtd,
		},
	}, nil
}
