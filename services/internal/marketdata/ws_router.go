// Task 6.3.10 (frame routing) + Task 6.3.1 (subscription control path).
//
// Inbound frame router: inspects the §10.5 `action` discriminator and
// separates control frames (subscribe/unsubscribe/ping/resume/
// authenticate/refresh_token), generic request frames (request.method),
// and interactive trading frames (order.*) so a burst of market-data
// writes never blocks the dispatch path and vice versa.
package marketdata

import (
	"context"
	"strings"

	"exchange/internal/auth"
	"exchange/internal/middleware"
	"exchange/internal/ratelimit"
	"exchange/internal/ws"
)

// handleMessage decodes and routes one inbound frame. Returning
// errCloseRead ends the read loop (used after terminal closes).
func (c *Conn) handleMessage(msg []byte) error {
	f, err := parseFrame(msg)
	if err != nil {
		c.sendError("", "", "INVALID_REQUEST", "frame is not valid JSON", 0)
		return nil
	}
	switch f.Action {
	case "authenticate":
		c.handleAuthenticate(f)
	case "refresh_token":
		c.handleRefresh(f)
	case "subscribe", "unsubscribe":
		c.handleSubscribe(f, f.Action == "unsubscribe")
	case "ping":
		b, _ := marshalFrame(pongFrame{Type: "pong", TsMs: c.srv.cfg.Now().UnixMilli()})
		c.enqueue(b)
	case "resume":
		c.handleResume(f)
	case "resync":
		c.handleResync(f) // §10.9 mid-stream gap repair (Task 6.3.24)
	case "request":
		c.handleRequest(f)
	default:
		if strings.HasPrefix(f.Action, "order.") {
			return c.handleOrder(f)
		}
		if f.Action == "" {
			c.sendError(f.rid(), "", "INVALID_REQUEST", "missing action", 0)
			return nil
		}
		c.sendError(f.rid(), f.Action, "INVALID_REQUEST", "unknown action", 0)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Subscriptions — typed-channel grammar (channels.go), private auth gate,
// class budgets (§24 #84), global cap, churn guard, acks.
// ---------------------------------------------------------------------------

// handleSubscribe applies one subscribe/unsubscribe frame. Per-channel
// verdicts are emitted as error frames for the rejected entries only —
// a partially-invalid batch still binds the valid channels (same
// semantics as the gateway surface).
func (c *Conn) handleSubscribe(f clientFrame, unsub bool) {
	channels, err := f.channelList()
	if err != nil || len(channels) == 0 {
		c.sendError(f.rid(), f.Action, "INVALID_REQUEST",
			"subscribe requires a non-empty channel list", 0)
		return
	}
	if c.noteChurn() {
		c.sendError(f.rid(), f.Action, "WS_ABUSE_DETECTED",
			"subscription churn exceeded", 0)
		c.closeConn(CloseAbuse, "WS_ABUSE_DETECTED")
		return
	}

	sess := c.session()
	var accepted []string
	for _, raw := range channels {
		ch, perr := ParseChannel(raw)
		if perr != nil {
			c.sendError(f.rid(), f.Action, channelCode(perr), perr.Error(), 0)
			continue
		}
		if !unsub {
			// Bind-side gates only — an unsubscribe is always honored
			// (a session that lost auth mid-stream must still be able
			// to drop channels cleanly).
			if code, reason := c.gateChannel(sess, ch); code != "" {
				c.sendError(f.rid(), f.Action, code, reason, 0)
				continue
			}
		}

		c.subMu.Lock()
		if unsub {
			if sub, ok := c.subs[ch.Raw]; ok {
				delete(c.subs, ch.Raw)
				switch sub.class {
				case ClassL2:
					c.l2n--
				case ClassL3:
					c.l3n--
				}
				c.srv.unsubscribeLocked(c, ch)
				accepted = append(accepted, ch.Raw)
			}
			c.subMu.Unlock()
			continue
		}
		if _, dup := c.subs[ch.Raw]; !dup {
			var code, why string
			switch ch.Class {
			case ClassL2:
				if c.l2n >= c.srv.cfg.MaxL2Subscriptions {
					code, why = "WS_MAX_SUBSCRIPTIONS_EXCEEDED",
						"L2 subscription budget (20 channels) exceeded"
				}
			case ClassL3:
				if c.l3n >= c.srv.cfg.MaxL3Subscriptions {
					code, why = "WS_MAX_SUBSCRIPTIONS_EXCEEDED",
						"L3 subscription budget (5 channels) exceeded"
				}
			}
			if code == "" && len(c.subs) >= c.srv.cfg.MaxSubscriptions {
				code, why = "WS_MAX_SUBSCRIPTIONS_EXCEEDED",
					"subscription limit reached"
			}
			if code != "" {
				c.subMu.Unlock()
				c.sendError(f.rid(), f.Action, code, why, 0)
				continue
			}
			c.subs[ch.Raw] = &subscription{ch: ch, class: ch.Class}
			switch ch.Class {
			case ClassL2:
				c.l2n++
			case ClassL3:
				c.l3n++
			}
			c.srv.subscribe(c, ch)
			accepted = append(accepted, ch.Raw)
		} else {
			accepted = append(accepted, ch.Raw) // idempotent subscribe
		}
		c.subMu.Unlock()
	}
	if len(accepted) > 0 {
		c.subMu.Lock()
		total := len(c.subs)
		c.subMu.Unlock()
		typ := "subscribed"
		if unsub {
			typ = "unsubscribed"
		}
		b, _ := marshalFrame(subscribedFrame{
			Type: typ, Channels: accepted, Total: total,
			TsMs: c.srv.cfg.Now().UnixMilli(),
		})
		c.enqueue(b)
	}
}

// gateChannel applies every bind-side admission check shared by the
// subscribe, resume and resync paths (one gate, three entrances):
//
//   - private:* channels require an authenticated session with "read"
//     scope (§10.5 item 2);
//   - the /ws/v1/orders private endpoint serves private:* channels only
//     (Task 6.3.5 — public feeds stay on /ws/v1/marketdata);
//   - the §10.7 entitlement check (Task 6.3.22) — symbol-level access
//     control mirroring the FIX SESSION_NOT_ENTITLED model; a denial
//     surfaces ENTITLEMENT_REQUIRED.
//
// Returns ("", "") when the bind is admitted; otherwise the §23 code and
// a wire-safe reason for the error frame.
func (c *Conn) gateChannel(sess ws.Session, ch Channel) (code, reason string) {
	if ch.Private {
		if !sess.Authenticated {
			return "UNAUTHORIZED", "authentication required for " + ch.Raw
		}
		if !hasScope(sess, "read") {
			return "INSUFFICIENT_SCOPE", "read scope required for " + ch.Raw
		}
	} else if c.onPrivateEndpoint() {
		return "INVALID_REQUEST",
			"public channel " + ch.Raw + " is served on /ws/v1/marketdata — this endpoint carries private:* channels only"
	}
	if ok, why := c.srv.checkEntitlement(&sess, ch); !ok {
		if why == "" {
			why = "channel not entitled for this session"
		}
		return "ENTITLEMENT_REQUIRED", why
	}
	return "", ""
}

// ---------------------------------------------------------------------------
// authenticate / refresh_token — same contract as the gateway surface
// (§10.5 items 1–3), reusing auth.Issuer / auth.SignatureVerifier.
// ---------------------------------------------------------------------------

// handleAuthenticate elevates the connection per §10.5 item 1. The frame:
//
//	{"action":"authenticate","token":"<jwt|ak_*>","signature":"<opt>",
//	 "timestamp":<epoch_s>,"protocol_version":1}
//
// API-key tokens carry the ak_ prefix and REQUIRE signature — the signed
// surface is CanonicalRequest(timestamp, "WS", upgrade path, token).
func (c *Conn) handleAuthenticate(f clientFrame) {
	if f.ProtocolVersion == nil {
		c.sendError(f.rid(), "authenticate", "INVALID_REQUEST",
			"protocol_version is required", 0)
		return
	}
	if err := middleware.CheckWSProtocolVersion(*f.ProtocolVersion); err != nil {
		c.sendError(f.rid(), "authenticate", "UNSUPPORTED_PROTOCOL_VERSION",
			"protocol_version not supported", 0)
		return
	}
	if f.Token == "" {
		c.sendError(f.rid(), "authenticate", "INVALID_REQUEST",
			"token is required", 0)
		return
	}

	var sess ws.Session
	sess.ProtocolVer = *f.ProtocolVersion
	if strings.HasPrefix(f.Token, "ak_") {
		if c.srv.cfg.Verifier == nil {
			c.sendError(f.rid(), "authenticate", "UNAUTHORIZED",
				"api-key authentication not configured", 0)
			return
		}
		if f.Signature == "" || len(f.Timestamp) == 0 {
			c.sendError(f.rid(), "authenticate", "INVALID_REQUEST",
				"api-key auth requires signature and timestamp", 0)
			return
		}
		key, err := c.srv.cfg.Verifier.Verify(context.Background(), auth.SignedRequest{
			KeyID:     f.Token,
			Timestamp: f.tsString(),
			Signature: f.Signature,
			Method:    "WS",
			Path:      c.path,
			Body:      []byte(f.Token),
			RemoteIP:  c.remoteIP,
		})
		if err != nil {
			c.sendError(f.rid(), "authenticate", codeOf(err),
				"api-key authentication failed", 0)
			return
		}
		sess.Authenticated = true
		sess.ViaAPIKey = true
		sess.Subject = "apikey:" + key.KeyID
		sess.AccountID = key.AccountID
		sess.Scopes = key.Scopes
		sess.Tier = key.RateLimitTier
		sess.KeyID = key.KeyID
	} else {
		if c.srv.cfg.Issuer == nil {
			c.sendError(f.rid(), "authenticate", "UNAUTHORIZED",
				"jwt authentication not configured", 0)
			return
		}
		claims, err := c.srv.cfg.Issuer.Parse(f.Token)
		if err != nil {
			c.sendError(f.rid(), "authenticate", "UNAUTHORIZED",
				"token validation failed", 0)
			return
		}
		sess.Authenticated = true
		sess.Subject = claims.Subject
		sess.AccountID = claims.AccountID
		sess.Scopes = claims.Scopes
		sess.KeyID = claims.KeyID
		sess.SessionID = claims.SessionID
		sess.ExpiresAt = claims.ExpiresAt
	}

	// Per-account concurrent-WS cap (spec §8.3 WS-connections column).
	if !c.srv.accountCapAdmit(c, &sess) {
		c.sendError(f.rid(), "authenticate", "CAPACITY_EXCEEDED",
			"per-account WebSocket connection cap reached for tier", 0)
		return
	}

	sess.RemoteIP = c.remoteIP
	prev := c.session()
	if prev.Authenticated && prev.AccountID != sess.AccountID {
		c.teardownPrivate() // re-auth under a different account must not leak
	}
	c.setSession(sess)

	data := map[string]any{
		"subject":          sess.Subject,
		"account_id":       sess.AccountID,
		"scopes":           sess.Scopes,
		"protocol_version": sess.ProtocolVer,
	}
	if !sess.ExpiresAt.IsZero() {
		data["expires_at_ms"] = sess.ExpiresAt.UnixMilli()
	}
	c.sendResponse(f.rid(), "authenticate", "ACK", data)
}

// accountCapAdmit checks and reserves the per-account slot. Caller must
// only call this for an authenticated session.
func (s *Server) accountCapAdmit(c *Conn, sess *ws.Session) bool {
	tier := ratelimit.ParseTier(sess.Tier)
	if s.cfg.TierResolver != nil {
		tier = s.cfg.TierResolver(context.Background(), sess)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	// Release a prior account binding on this conn (re-auth flow).
	if prev := c.session(); prev.Authenticated && prev.AccountID != 0 &&
		prev.AccountID != sess.AccountID {
		if s.byAccount[prev.AccountID]--; s.byAccount[prev.AccountID] <= 0 {
			delete(s.byAccount, prev.AccountID)
		}
	}
	if prev := c.session(); prev.Authenticated && prev.AccountID == sess.AccountID {
		return true // already counted — re-auth/refresh under same account
	}
	if s.byAccount[sess.AccountID] >= ws.WSConnCap(tier) {
		return false
	}
	s.byAccount[sess.AccountID]++
	return true
}

// handleRefresh implements the §10.5 item 3 in-flight renewal:
// {"action":"refresh_token","token":"<new_jwt>"} — the socket,
// subscriptions and in-flight messages survive untouched.
func (c *Conn) handleRefresh(f clientFrame) {
	sess := c.session()
	if !sess.Authenticated {
		c.sendError(f.rid(), "refresh_token", "UNAUTHORIZED",
			"authenticate first", 0)
		return
	}
	if sess.ViaAPIKey {
		c.sendError(f.rid(), "refresh_token", "INVALID_REQUEST",
			"api-key sessions do not expire", 0)
		return
	}
	if f.Token == "" {
		c.sendError(f.rid(), "refresh_token", "INVALID_REQUEST",
			"token is required", 0)
		return
	}
	if c.srv.cfg.Issuer == nil {
		c.sendError(f.rid(), "refresh_token", "UNAUTHORIZED",
			"jwt authentication not configured", 0)
		return
	}
	claims, err := c.srv.cfg.Issuer.Parse(f.Token)
	if err != nil {
		c.sendError(f.rid(), "refresh_token", "UNAUTHORIZED",
			"token validation failed", 0)
		return
	}
	// Renewal binds to the existing session: a different identity is a
	// re-authenticate, not a renewal (fail closed on token swap).
	if claims.Subject != sess.Subject || claims.AccountID != sess.AccountID {
		c.sendError(f.rid(), "refresh_token", "UNAUTHORIZED",
			"renewal token identity mismatch", 0)
		return
	}
	sess.ExpiresAt = claims.ExpiresAt
	sess.KeyID = claims.KeyID
	c.setSession(sess)
	c.sendResponse(f.rid(), "refresh_token", "ACK", map[string]any{
		"expires_at_ms": claims.ExpiresAt.UnixMilli(),
	})
}

// teardownPrivate removes every private:* subscription (auth-expiry and
// cross-account re-auth paths).
func (c *Conn) teardownPrivate() {
	c.subMu.Lock()
	defer c.subMu.Unlock()
	for ch, sub := range c.subs {
		if sub.ch.Private {
			c.srv.unsubscribeLocked(c, sub.ch)
			delete(c.subs, ch)
		}
	}
}

// hasScope checks the §8.8 scope matrix. A session carrying NO scopes at
// all is a full user JWT — scope enforcement is the API-key contract, so
// a scope-less session is treated as full-access (mirrors
// ws.Session.hasScope semantics).
func hasScope(s ws.Session, scope string) bool {
	if len(s.Scopes) == 0 {
		return !s.ViaAPIKey // API-key sessions always carry a scope list
	}
	for _, v := range s.Scopes {
		if v == scope {
			return true
		}
	}
	return false
}
