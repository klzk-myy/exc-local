// app_quoting.go — Task 18.3.7 wire half (session-layer tag framing the
// task's DoD names its sibling surface) + Task 11.3.12's SCOPE_LP gate
// consumer: MassQuote(35=i) and QuoteCancel(35=Z) are parsed here and
// dispatched to the transport-agnostic *QuoteService, which returns the
// per-entry acks this file renders as MassQuoteAck(35=b).
//
// Ordering of gates per spec §9.4/§11 kill-switch §17.16:
//  1. session account binding (drop-copy sessions never quote);
//  2. session allowed_instruments entitlement per entry symbol — an
//     out-of-scope entry rejects WITHOUT reaching the service, while in-
//     scope siblings still apply (per-entry semantics of 35=b);
//  3. QuoteService admission: LP kill-switch gate (SCOPE_LP — suspended
//     LP's whole set rejects TRADING_HALTED while firm CLOB trading
//     continues), firm-liquidity check, mm_programs entitlement, MMP
//     lockout, then canonical orders.Submit.
package fix

import (
	"context"
	"fmt"
	"strings"
	"time"

	"exchange/pkg/decimal"

	"github.com/quickfixgo/quickfix"
)

// MsgType additions (35=i / 35=b / 35=Z per spec §9.4).
const (
	MsgMassQuote    = "i"
	MsgMassQuoteAck = "b"
	MsgQuoteCancel  = "Z"
)

// Mass-quote tag block. Tag 20010 is the venue's bilateral last-look /
// hold-time probe (FIX defines no hold-time tag on MassQuote — see the
// MassQuote doc in quoting.go): a non-zero value rejects the whole set
// under the 100% firm-liquidity rule.
const (
	TagQuoteID            quickfix.Tag = 117
	TagBidPx              quickfix.Tag = 132
	TagOfferPx            quickfix.Tag = 133
	TagBidSize            quickfix.Tag = 134
	TagOfferSize          quickfix.Tag = 135
	TagNoQuoteEntries     quickfix.Tag = 295
	TagQuoteAckStatus     quickfix.Tag = 297
	TagQuoteCancelType    quickfix.Tag = 298
	TagQuoteEntryID       quickfix.Tag = 299
	TagQuoteRejectReason  quickfix.Tag = 300
	TagQuoteSetID         quickfix.Tag = 302
	TagQuoteHoldTimeProbe quickfix.Tag = 20010
)

// parseMassQuote frames one inbound 35=i into the service struct.
// Missing QuoteSetID or a malformed group entry is a business-level
// reject, never a session-level reject (§2.7: the peer's malformed
// field aborts the message, not the session).
func parseMassQuote(msg *quickfix.Message) (*MassQuote, error) {
	mq := &MassQuote{QuoteSetID: optionalStr(msg, TagQuoteSetID)}
	if mq.QuoteSetID == "" {
		return nil, quickfix.RequiredTagMissing(TagQuoteSetID)
	}
	if v, err := msg.Body.GetInt(TagQuoteHoldTimeProbe); err == nil && v > 0 {
		mq.HoldTime = time.Duration(v) * time.Millisecond
	}
	if msg.Body.Has(TagNoQuoteEntries) {
		grp := quickfix.NewRepeatingGroup(TagNoQuoteEntries, quickfix.GroupTemplate{
			quickfix.GroupElement(TagQuoteEntryID),
			quickfix.GroupElement(TagSymbol),
			quickfix.GroupElement(TagBidPx),
			quickfix.GroupElement(TagBidSize),
			quickfix.GroupElement(TagOfferPx),
			quickfix.GroupElement(TagOfferSize),
		})
		if err := msg.Body.GetGroup(grp); err != nil {
			return nil, err
		}
		for i := 0; i < grp.Len(); i++ {
			ge := grp.Get(i)
			e := QuoteEntry{}
			if v, err := ge.GetString(TagQuoteEntryID); err == nil {
				e.QuoteEntryID = strings.TrimSpace(v)
			}
			if v, err := ge.GetString(TagSymbol); err == nil {
				e.Symbol = strings.ToUpper(strings.TrimSpace(v))
			}
			var perr error
			e.BidPx, perr = groupDec(ge, TagBidPx)
			if perr != nil {
				return nil, perr
			}
			e.BidSize, perr = groupDec(ge, TagBidSize)
			if perr != nil {
				return nil, perr
			}
			e.OfferPx, perr = groupDec(ge, TagOfferPx)
			if perr != nil {
				return nil, perr
			}
			e.OfferSize, perr = groupDec(ge, TagOfferSize)
			if perr != nil {
				return nil, perr
			}
			mq.Entries = append(mq.Entries, e)
		}
	}
	return mq, nil
}

// groupDec parses an optional decimal inside a repeating-group entry.
func groupDec(ge *quickfix.Group, tag quickfix.Tag) (*decimal.Decimal, error) {
	v, err := ge.GetString(tag)
	if err != nil {
		return nil, nil
	}
	d, derr := decimal.NewFromString(strings.TrimSpace(v))
	if derr != nil {
		return nil, quickfix.IncorrectDataFormatForValue(tag)
	}
	return &d, nil
}

// parseQuoteCancel frames one inbound 35=Z.
func parseQuoteCancel(msg *quickfix.Message) (*QuoteCancel, error) {
	ct, err := msg.Body.GetInt(TagQuoteCancelType)
	if err != nil {
		return nil, quickfix.RequiredTagMissing(TagQuoteCancelType)
	}
	return &QuoteCancel{
		QuoteSetID: optionalStr(msg, TagQuoteSetID),
		CancelType: ct,
		Symbol:     strings.ToUpper(optionalStr(msg, TagSymbol)),
	}, nil
}

// buildMassQuoteAck renders the service's per-entry outcome as 35=b.
// Overall QuoteAckStatus(297) is 0 only when every entry accepted —
// FIX 4.4 carries no partial-accept enum, so any rejected entry makes
// the set-level status 5 with the per-entry lines carrying the truth.
func buildMassQuoteAck(ack *MassQuoteAck) *quickfix.Message {
	m := quickfix.NewMessage()
	m.Header.SetString(TagMsgType, MsgMassQuoteAck)
	m.Body.SetString(TagQuoteSetID, ack.QuoteSetID)
	if ack.Accepted() {
		m.Body.SetInt(TagQuoteAckStatus, QuoteStatusAccepted)
	} else {
		m.Body.SetInt(TagQuoteAckStatus, QuoteStatusRejected)
	}
	grp := quickfix.NewRepeatingGroup(TagNoQuoteEntries, quickfix.GroupTemplate{
		quickfix.GroupElement(TagQuoteEntryID),
		quickfix.GroupElement(TagQuoteAckStatus),
		quickfix.GroupElement(TagQuoteRejectReason),
		quickfix.GroupElement(TagSymbol),
		quickfix.GroupElement(TagText),
	})
	for _, e := range ack.Entries {
		ge := grp.Add()
		ge.SetString(TagQuoteEntryID, e.QuoteEntryID)
		ge.SetInt(TagQuoteAckStatus, e.Status)
		if e.Status != QuoteStatusAccepted {
			ge.SetInt(TagQuoteRejectReason, e.RejectReason)
		}
		if e.Symbol != "" {
			ge.SetString(TagSymbol, e.Symbol)
		}
		if e.Text != "" {
			ge.SetString(TagText, e.Text)
		}
	}
	m.Body.SetGroup(grp)
	return m
}

// buildQuoteCancelAck answers 35=Z with the same 35=b carrier: the set
// count cancelled rides Text(58); an error surfaces as status 5.
func buildQuoteCancelAck(setID string, cancelled int, err error) *quickfix.Message {
	m := quickfix.NewMessage()
	m.Header.SetString(TagMsgType, MsgMassQuoteAck)
	m.Body.SetString(TagQuoteSetID, setID)
	if err != nil {
		m.Body.SetInt(TagQuoteAckStatus, QuoteStatusRejected)
		m.Body.SetString(TagText, err.Error())
		return m
	}
	m.Body.SetInt(TagQuoteAckStatus, QuoteStatusAccepted)
	m.Body.SetString(TagText, fmt.Sprintf("cancelled %d quote entries", cancelled))
	return m
}

// onMassQuote — 35=i → QuoteService.SubmitMassQuote. Session gates run
// first: unbound (drop-copy) sessions may never quote; entries for
// symbols outside the session's allowed_instruments reject locally —
// per-entry semantics mean one out-of-scope symbol never strands the
// set's in-scope entries.
func (a *App) onMassQuote(ctx context.Context, msg *quickfix.Message,
	sessionID quickfix.SessionID, row *Session) {
	if a.opt.QS == nil {
		a.emit(sessionID, businessReject(MsgMassQuote, "",
			"MSGTYPE_UNSUPPORTED", BusinessRejectReasonOther))
		return
	}
	if row.AccountID == nil {
		a.emit(sessionID, businessReject(MsgMassQuote, "",
			"SESSION_NOT_ENTITLED", BusinessRejectReasonNotEntitled))
		return
	}
	mq, perr := parseMassQuote(msg)
	if perr != nil {
		a.emit(sessionID, businessReject(MsgMassQuote, "",
			perr.Error(), BusinessRejectReasonOther))
		return
	}
	// Session entitlement split: out-of-scope entries reject here; the
	// rest ride the service (LP gate → firm-liquidity → mm entitlement →
	// MMP → canonical pipeline).
	var forward []QuoteEntry
	local := map[string]QuoteAckEntry{}
	for _, e := range mq.Entries {
		if e.Symbol != "" && !row.Entitled(e.Symbol) {
			local[e.QuoteEntryID] = QuoteAckEntry{
				QuoteEntryID: e.QuoteEntryID, Symbol: e.Symbol,
				Status: QuoteStatusRejected, RejectReason: QuoteRejectReasonOther,
				Text: "SESSION_NOT_ENTITLED: symbol outside session allowed_instruments",
			}
			continue
		}
		forward = append(forward, e)
	}
	ack := &MassQuoteAck{QuoteSetID: mq.QuoteSetID}
	if len(forward) > 0 {
		svcAck, err := a.opt.QS.SubmitMassQuote(ctx, sessionID.String(),
			*row.AccountID, &MassQuote{
				QuoteSetID: mq.QuoteSetID, Entries: forward, HoldTime: mq.HoldTime,
			})
		if err != nil && svcAck == nil {
			a.emit(sessionID, businessReject(MsgMassQuote, "",
				err.Error(), BusinessRejectReasonOther))
			return
		}
		if svcAck != nil {
			ack.Entries = append(ack.Entries, svcAck.Entries...)
		}
	}
	// Merge the locally-rejected entries back (deterministic order:
	// service lines first, locals after — both keyed by QuoteEntryID
	// which the peer reconciles).
	for _, e := range mq.Entries {
		if la, ok := local[e.QuoteEntryID]; ok {
			ack.Entries = append(ack.Entries, la)
		}
	}
	a.emit(sessionID, buildMassQuoteAck(ack))
}

// onQuoteCancel — 35=Z → QuoteService.CancelQuotes.
func (a *App) onQuoteCancel(ctx context.Context, msg *quickfix.Message,
	sessionID quickfix.SessionID, row *Session) {
	if a.opt.QS == nil {
		a.emit(sessionID, businessReject(MsgQuoteCancel, "",
			"MSGTYPE_UNSUPPORTED", BusinessRejectReasonOther))
		return
	}
	if row.AccountID == nil {
		a.emit(sessionID, businessReject(MsgQuoteCancel, "",
			"SESSION_NOT_ENTITLED", BusinessRejectReasonNotEntitled))
		return
	}
	c, perr := parseQuoteCancel(msg)
	if perr != nil {
		a.emit(sessionID, businessReject(MsgQuoteCancel, "",
			perr.Error(), BusinessRejectReasonOther))
		return
	}
	n, err := a.opt.QS.CancelQuotes(ctx, sessionID.String(), *row.AccountID, c)
	a.emit(sessionID, buildQuoteCancelAck(c.QuoteSetID, n, err))
}
