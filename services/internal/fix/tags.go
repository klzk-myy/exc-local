// FIX tag / MsgType / enum constants used by the gateway. The venue
// speaks FIX 4.4 on the tag-value wire; generated quickfixgo message
// packages are deliberately not a dependency — every message is built
// and read through the raw quickfix.Message/FieldMap API so the module
// graph stays at github.com/quickfixgo/quickfix alone.
package fix

import "github.com/quickfixgo/quickfix"

// MsgType (tag 35) values the gateway produces/consumes.
const (
	MsgHeartbeat            = "0"
	MsgTestRequest          = "1"
	MsgResendRequest        = "2"
	MsgReject               = "3"
	MsgSequenceReset        = "4"
	MsgLogout               = "5"
	MsgExecutionReport      = "8"
	MsgOrderCancelReject    = "9"
	MsgLogon                = "A"
	MsgNews                 = "B"
	MsgNewOrderSingle       = "D"
	MsgOrderCancelRequest   = "F"
	MsgOrderCancelReplace   = "G"
	MsgOrderStatusRequest   = "H"
	MsgBusinessReject       = "j"
	MsgUserRequest          = "BE"
	MsgUserResponse         = "BF"
	MsgTradingSessionStatus = "h"
)

// FIX 4.4 tags referenced by the gateway.
const (
	TagAccount              quickfix.Tag = 1
	TagAvgPx                quickfix.Tag = 6
	TagBeginSeqNo           quickfix.Tag = 7
	TagBeginString          quickfix.Tag = 8
	TagBodyLength           quickfix.Tag = 9
	TagCheckSum             quickfix.Tag = 10
	TagClOrdID              quickfix.Tag = 11
	TagCumQty               quickfix.Tag = 14
	TagCurrency             quickfix.Tag = 15
	TagEndSeqNo             quickfix.Tag = 16
	TagExecID               quickfix.Tag = 17
	TagExecInst             quickfix.Tag = 18
	TagExecRefID            quickfix.Tag = 19
	TagHandlInst            quickfix.Tag = 21
	TagLastPx               quickfix.Tag = 31
	TagLastQty              quickfix.Tag = 32
	TagMsgSeqNum            quickfix.Tag = 34
	TagMsgType              quickfix.Tag = 35
	TagOrderID              quickfix.Tag = 37
	TagOrderQty             quickfix.Tag = 38
	TagOrdStatus            quickfix.Tag = 39
	TagOrdType              quickfix.Tag = 40
	TagOrigClOrdID          quickfix.Tag = 41
	TagPossDupFlag          quickfix.Tag = 43
	TagPrice                quickfix.Tag = 44
	TagRefSeqNum            quickfix.Tag = 45
	TagSecurityID           quickfix.Tag = 48
	TagSenderCompID         quickfix.Tag = 49
	TagSenderSubID          quickfix.Tag = 50
	TagSendingTime          quickfix.Tag = 52
	TagSide                 quickfix.Tag = 54
	TagSymbol               quickfix.Tag = 55
	TagTargetCompID         quickfix.Tag = 56
	TagTargetSubID          quickfix.Tag = 57
	TagText                 quickfix.Tag = 58
	TagTimeInForce          quickfix.Tag = 59
	TagTransactTime         quickfix.Tag = 60
	TagSettlDate            quickfix.Tag = 64
	TagStopPx               quickfix.Tag = 99
	TagOrdRejReason         quickfix.Tag = 103
	TagMaxFloor             quickfix.Tag = 111
	TagMinQty               quickfix.Tag = 110
	TagCxlRejReason         quickfix.Tag = 102
	TagCxlRejResponseTo     quickfix.Tag = 434
	TagOrigSendingTime      quickfix.Tag = 122
	TagGapFillFlag          quickfix.Tag = 123
	TagExpireTime           quickfix.Tag = 126
	TagDeliverToCompID      quickfix.Tag = 128
	TagResetSeqNumFlag      quickfix.Tag = 141
	TagExecType             quickfix.Tag = 150
	TagLeavesQty            quickfix.Tag = 151
	TagSecurityType         quickfix.Tag = 167
	TagStrikePrice          quickfix.Tag = 202
	TagNoPartyIDs           quickfix.Tag = 453
	TagUsername             quickfix.Tag = 553
	TagPassword             quickfix.Tag = 554
	TagUserRequestID        quickfix.Tag = 923
	TagUserRequestType      quickfix.Tag = 924
	TagUserStatus           quickfix.Tag = 926
	TagUserStatusText       quickfix.Tag = 927
	TagBusinessRejectRefID  quickfix.Tag = 379
	TagBusinessRejectReason quickfix.Tag = 380
	TagRefMsgType           quickfix.Tag = 372
	TagRefTagID             quickfix.Tag = 371
	TagSessionRejectReason  quickfix.Tag = 373
	TagTradingSessionID     quickfix.Tag = 336
	TagTradSesStatus        quickfix.Tag = 340
	TagTradSesStatusRejReas quickfix.Tag = 567
	TagSecondaryClOrdID     quickfix.Tag = 526

	// TagCountdownMs is the venue custom tag carrying the dead-man
	// countdown inside UserRequest 35=BE (Phase-18 Task 18.3.16 —
	// "custom tag 20001, min 1000, max 60000").
	TagCountdownMs quickfix.Tag = 20001
)

// UserRequestType(924) values: venue custom countdown-cancel-all is 4
// per Task 18.3.16 (FIX reserves 1..4 for logon/status flows; the plan
// pins 4 as the venue countdown command).
const (
	UserRequestTypeCountdown = "4"
)

// Side(54)
const (
	SideBuy  = "1"
	SideSell = "2"
)

// OrdType(40) — FIX 4.4 values this gateway accepts today. Forex-
// specific values (E=FXPrevQuote, F=FXSwap, G=FXForward, H=FXSpot)
// map onto the same spot pipeline: H behaves as Market, F/G are
// rejected pending the Phase-22 derivative layer (Task 18.3.5 owns
// the FIX 5.0 SP2 protocol for those).
const (
	OrdTypeMarket    = "1"
	OrdTypeLimit     = "2"
	OrdTypeStop      = "3"
	OrdTypeStopLimit = "4"
)

// TimeInForce(59)
const (
	TIFDay = "0"
	TIFGTC = "1"
	TIFIOC = "3"
	TIFFOK = "4"
	TIFGTD = "6"
)

// OrdStatus(39) / ExecType(150)
const (
	OrdStatusNew             = "0"
	OrdStatusPartiallyFilled = "1"
	OrdStatusFilled          = "2"
	OrdStatusDoneForDay      = "3"
	OrdStatusCanceled        = "4"
	OrdStatusReplaced        = "5"
	OrdStatusPendingCancel   = "6"
	OrdStatusStopped         = "7"
	OrdStatusRejected        = "8"
	OrdStatusPendingNew      = "A"
	OrdStatusExpired         = "C"
)

const (
	ExecTypeNew           = "0"
	ExecTypePartialFill   = "1" // FIX 4.4: 1=PartialFill
	ExecTypeFill          = "2" // FIX 4.4: 2=Fill
	ExecTypeTrade         = "F" // FIX 4.4+: F=Trade
	ExecTypeDoneForDay    = "3"
	ExecTypeCanceled      = "4"
	ExecTypeReplaced      = "5"
	ExecTypePendingCancel = "6"
	ExecTypeStopped       = "7"
	ExecTypeRejected      = "8"
	ExecTypePendingNew    = "A"
	ExecTypeExpired       = "C"
	ExecTypeOrderStatus   = "I"
)

// CxlRejResponseTo(434)
const (
	CxlRejResponseToCancel        = "1"
	CxlRejResponseToCancelReplace = "2"
)

// OrdRejReason(103) — standard FIX 4.4 subset we emit.
const (
	OrdRejReasonBrokerCredit   = 0
	OrdRejReasonUnknownSymbol  = 1
	OrdRejReasonExchangeClosed = 2
	OrdRejReasonExceedsLimit   = 3 // order exceeds limit
	OrdRejReasonTooLate        = 4
	OrdRejReasonUnknownOrder   = 5
	OrdRejReasonDuplicateOrder = 6
	OrdRejReasonOther          = 99
)

// CxlRejReason(102)
const (
	CxlRejReasonTooLate        = 0
	CxlRejReasonUnknownOrder   = 1
	CxlRejReasonBrokerOption   = 2
	CxlRejReasonAlreadyPending = 3
)

// BusinessRejectReason(380) — spec §9.9 custom space.
const (
	BusinessRejectReasonOther       = 0
	BusinessRejectReasonUnknownID   = 1
	BusinessRejectReasonNotEntitled = 6 // venue custom: SESSION_NOT_ENTITLED
	BusinessRejectReasonThrottled   = 7 // venue custom: SESSION_THROTTLED
)

// Internal order_status → FIX OrdStatus — Task 18.3.2 item 5, spec
// §5.4 vocabulary → §9.9 wire values.
func ordStatusFIX(internal string) string {
	switch internal {
	case "PENDING":
		return OrdStatusPendingNew
	case "RESERVED":
		return OrdStatusNew // queued fixing/auction order rests as New
	case "ACTIVE":
		return OrdStatusNew
	case "PARTIALLY_FILLED":
		return OrdStatusPartiallyFilled
	case "FILLED":
		return OrdStatusFilled
	case "CANCELLED":
		return OrdStatusCanceled
	case "REJECTED":
		return OrdStatusRejected
	case "EXPIRED":
		return OrdStatusExpired
	}
	return OrdStatusNew
}

// ordRejReason maps canonical gateway error codes onto FIX
// OrdRejReason(103). Text(58) always carries the code itself — the
// numeric reason is secondary per spec §9.9.
func ordRejReason(code string) int {
	switch code {
	case "ORDER_NOT_FOUND":
		return OrdRejReasonUnknownOrder
	case "INVALID_REQUEST", "VALIDATION_FAILED", "QUOTE_QUANTITY_INVALID",
		"INVALID_PRICE", "INVALID_QUANTITY", "MIN_NOTIONAL",
		"PRICE_BAND_BREACH", "INSTRUMENT_DELISTED":
		return OrdRejReasonOther
	case "UNKNOWN_SYMBOL", "INSTRUMENT_NOT_FOUND":
		return OrdRejReasonUnknownSymbol
	case "TRADING_HALTED", "CIRCUIT_BREAKER_OPEN", "MARKET_CLOSED",
		"WEEKEND_HALT_ACTIVE", "INSTRUMENT_HALTED":
		return OrdRejReasonExchangeClosed
	case "INSUFFICIENT_BALANCE", "RISK_LIMIT_EXCEEDED", "MARGIN_INSUFFICIENT",
		"EXPOSURE_LIMIT_EXCEEDED":
		return OrdRejReasonExceedsLimit
	case "IDEMPOTENCY_KEY_COLLISION", "DUPLICATE_ORDER":
		return OrdRejReasonDuplicateOrder
	case "GATEWAY_TIMEOUT_MATCHING_ENGINE", "SERVICE_DEGRADED",
		"ENGINE_OVERLOAD":
		return OrdRejReasonTooLate
	case "STALE_MODIFY":
		return OrdRejReasonTooLate
	}
	return OrdRejReasonOther
}
