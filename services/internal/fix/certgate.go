// certgate.go — Task 18.3.11: production certification enforcement at
// Logon (spec §9.7 item 4, §24 #167 — "uncertified or stale-certified
// client builds cannot open production order-entry sessions").
//
// CertifiedApp wraps *App (drop-in quickfix.Application): the embedded
// authenticator runs first (CompID pair + API-key credential), then the
// certification gate. Only order-entry sessions (account_id bound) on a
// production listener are gated — drop-copy and non-production
// environments certify on their own cadence.
//
// The wrapper pattern keeps the sibling session/authentication files
// untouched: wire `quickfix.NewAcceptor(certifiedApp, …)` where a bare
// `app` would otherwise go.
package fix

import (
	"context"
	"time"

	"github.com/quickfixgo/quickfix"

	"exchange/internal/fix/certification"
)

// TagClientBuild is the venue custom Logon tag carrying the client's
// build label (e.g. "clientlib-4.2.1+gitabc"). When absent the session
// row's client_build column (migration 052) is used; when both are
// absent the gate fails closed on production.
const TagClientBuild quickfix.Tag = 20002

// CertifiedAppOptions wires the certification wrapper.
type CertifiedAppOptions struct {
	// Gate is the certification decision maker (required — nil fails
	// closed for production order-entry logons).
	Gate *certification.Gate
	// Environment is this listener's deployment label ("production"
	// gates; anything else passes through — certification still gates
	// via the SBE transport's own check where configured).
	Environment string
	// VenueSchemaVersion is the venue message-set version the gate keys
	// on (e.g. "fix-core.v1"). Empty → "fix-core.v1".
	VenueSchemaVersion string
	// DictionaryFor resolves the session's protocol dictionary —
	// defaults to the fix_sessions.protocol_version column
	// ("FIX.4.4" | "FIX.5.0SP2").
	DictionaryFor func(row *Session) string
	// Now defaults to wall clock.
	Now func() time.Time
}

// CertifiedApp is a quickfix.Application that enforces the
// certification gate on Logon for order-entry sessions.
type CertifiedApp struct {
	*App
	opt CertifiedAppOptions
}

// WrapCertification returns app guarded by the gate.
func WrapCertification(app *App, o CertifiedAppOptions) *CertifiedApp {
	if o.VenueSchemaVersion == "" {
		o.VenueSchemaVersion = "fix-core.v1"
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	return &CertifiedApp{App: app, opt: o}
}

// FromAdmin intercepts Logon: base authentication first, then the
// certification gate for order-entry sessions on production.
func (c *CertifiedApp) FromAdmin(msg *quickfix.Message,
	sessionID quickfix.SessionID) quickfix.MessageRejectError {
	if rej := c.App.FromAdmin(msg, sessionID); rej != nil {
		return rej
	}
	mt, err := msg.MsgType()
	if err != nil || mt != MsgLogon {
		return nil
	}
	if c.opt.Environment != "production" {
		return nil // non-production listeners certify separately
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	row, serr := c.App.opt.Store.SessionByID(ctx, sessionID.String())
	if serr != nil || row == nil {
		return quickfix.RejectLogon{Text: "SESSION_UNAVAILABLE"}
	}
	if row.AccountID == nil {
		return nil // drop-copy sessions carry no order entry
	}
	// Client build: Logon tag wins; else the provisioned column.
	build := ""
	if v, gerr := msg.Body.GetString(TagClientBuild); gerr == nil {
		build = v
	}
	if build == "" {
		build = rowClientBuild(row)
	}
	dict := row.ProtocolVersion
	if c.opt.DictionaryFor != nil {
		dict = c.opt.DictionaryFor(row)
	}
	if dict == "" {
		dict = sessionID.BeginString
	}
	if err := c.opt.Gate.Admit(ctx, sessionID.TargetCompID, build, dict,
		c.opt.VenueSchemaVersion); err != nil {
		code := "CERTIFICATION_REQUIRED"
		if ge, ok := err.(*certification.GateError); ok {
			code = ge.Code
		}
		return quickfix.RejectLogon{Text: code}
	}
	return nil
}

// rowClientBuild reads the migration-052 client_build column lazily —
// the sibling Session row pre-dates 052, so the value is re-queried via
// the dedicated accessor the column-aware store provides. Sessions
// provisioned before the column existed return "".
func rowClientBuild(row *Session) string {
	if b, ok := interface{}(row).(interface{ ClientBuild() string }); ok {
		return b.ClientBuild()
	}
	return ""
}
