// Task 18.3.16 — dead-man switch via FIX UserRequest (35=BE). The FIX
// command shares the canonical Task 5.3.33 account-level countdown
// timer (accounts.DeadManService / Redis countdown:{account}) with the
// REST and WS surfaces — one timer, one expiry, one atomic mass-cancel.
package fix

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/quickfixgo/quickfix"

	"exchange/internal/accounts"
	excerrors "exchange/pkg/errors"
)

// FIX-side countdown bounds per the task contract: CountdownMs(20001)
// 1000..60000, 0 disables. The canonical service admits up to 300000 —
// the FIX surface deliberately keeps the tighter institutional bound.
const (
	fixCountdownMinMs = 1000
	fixCountdownMaxMs = 60000
)

// onUserRequest handles 35=BE: UserRequestType(924)=4 sets/refreshes
// the countdown (or disables at 0); anything else is an unknown
// request answered with UserResponse(35=BF) UserStatus=8.
func (a *App) onUserRequest(ctx context.Context, msg *quickfix.Message,
	sessionID quickfix.SessionID, row *Session) {
	reqID := optionalStr(msg, TagUserRequestID)
	reqType := optionalStr(msg, TagUserRequestType)

	respond := func(status, text string) {
		m := quickfix.NewMessage()
		m.Header.SetField(TagMsgType, quickfix.FIXString(MsgUserResponse))
		if reqID != "" {
			m.Body.SetString(TagUserRequestID, reqID)
		}
		m.Body.SetString(TagUserRequestType, reqType)
		m.Body.SetString(TagUserStatus, status)
		if text != "" {
			m.Body.SetString(TagUserStatusText, text)
		}
		a.emit(sessionID, m)
	}

	if reqType != UserRequestTypeCountdown {
		respond("8", "USERREQUEST_UNSUPPORTED")
		return
	}
	if a.opt.DeadMan == nil {
		respond("8", "DEAD_MAN_UNAVAILABLE")
		return
	}
	if row.AccountID == nil {
		respond("8", "SESSION_NOT_ENTITLED")
		return
	}

	msStr, rerr := required(msg, TagCountdownMs)
	if rerr != nil {
		respond("8", "INVALID_TIMEOUT")
		return
	}
	ms, perr := strconv.ParseInt(strings.TrimSpace(msStr), 10, 64)
	if perr != nil || ms < 0 || (ms != 0 && (ms < fixCountdownMinMs || ms > fixCountdownMaxMs)) {
		respond("8", "INVALID_TIMEOUT")
		return
	}

	var ack *accounts.CountdownAck
	var err error
	if ms == 0 {
		ack, err = a.opt.DeadMan.Disable(ctx, *row.AccountID)
	} else {
		// renew=true — every UserRequest is a heartbeat refresh of the
		// shared account timer (the REST surface's semantics).
		ack, err = a.opt.DeadMan.Set(ctx, *row.AccountID, ms, true)
	}
	if err != nil {
		respond("8", excerrors.CodeOf(err))
		return
	}

	m := quickfix.NewMessage()
	m.Header.SetField(TagMsgType, quickfix.FIXString(MsgUserResponse))
	if reqID != "" {
		m.Body.SetString(TagUserRequestID, reqID)
	}
	m.Body.SetString(TagUserRequestType, reqType)
	m.Body.SetString(TagUserStatus, "1") // armed/disabled confirmed
	m.Body.SetString(TagCountdownMs, fmt.Sprint(ms))
	if ack != nil && ack.CountdownExpiry > 0 {
		m.Body.SetString(TagUserStatusText,
			fmt.Sprintf("countdown_expiry_ms=%d", ack.CountdownExpiry))
	} else {
		m.Body.SetString(TagUserStatusText, "countdown_disabled")
	}
	a.emit(sessionID, m)
}
