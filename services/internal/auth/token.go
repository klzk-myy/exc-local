// Task 5.3.9 — per-token IP allowlists, enforced at request time on every
// API-key-authenticated surface (REST signed requests, WS authenticate,
// FIX logon per spec §8.8 item 3).
//
// Semantics:
//   - empty allowlist  => unrestricted (allowlist is optional)
//   - non-empty        => request IP must match an entry; else
//     TOKEN_IP_FORBIDDEN (HTTP 403, spec §23)
//   - entries accept bare IPs and CIDR prefixes, IPv4 and IPv6
package auth

import (
	"net"
	"net/netip"
	"strings"
)

// IPAllowlist is a parsed set of permitted source prefixes.
type IPAllowlist []netip.Prefix

// ParseIPAllowlist converts raw entries (bare IPs or CIDRs, IPv4/IPv6)
// into an allowlist. Any malformed entry rejects the whole list —
// a partially-applied allowlist is a security control failure
// (fail-closed, spec §2.7).
func ParseIPAllowlist(entries []string) (IPAllowlist, error) {
	out := make(IPAllowlist, 0, len(entries))
	for _, raw := range entries {
		e := strings.TrimSpace(raw)
		if e == "" {
			continue
		}
		if strings.Contains(e, "/") {
			p, err := netip.ParsePrefix(e)
			if err != nil {
				return nil, wrapError(CodeAPIKeyInvalid, "invalid CIDR in IP allowlist: "+raw, err)
			}
			out = append(out, p.Masked())
			continue
		}
		addr, err := parseIPAddr(e)
		if err != nil {
			return nil, wrapError(CodeAPIKeyInvalid, "invalid IP in allowlist: "+raw, err)
		}
		out = append(out, netip.PrefixFrom(addr, addr.BitLen()))
	}
	return out, nil
}

// parseIPAddr parses a bare IP, tolerating a zone suffix on IPv6
// literals ("%eth0") by stripping it — allowlist semantics are
// address-based, not interface-based.
func parseIPAddr(s string) (netip.Addr, error) {
	addr, err := netip.ParseAddr(s)
	if err == nil {
		return addr.Unmap(), nil
	}
	if i := strings.LastIndex(s, "%"); i > 0 {
		if addr, err2 := netip.ParseAddr(s[:i]); err2 == nil {
			return addr.Unmap(), nil
		}
	}
	return netip.Addr{}, err
}

// Allows reports whether clientIP is permitted. An empty allowlist
// permits everything (optional control); a malformed clientIP never
// matches (fail-closed).
func (l IPAllowlist) Allows(clientIP string) bool {
	if len(l) == 0 {
		return true
	}
	addr, err := parseIPAddr(strings.TrimSpace(clientIP))
	if err != nil {
		return false
	}
	for _, p := range l {
		if p.Contains(addr) {
			return true
		}
	}
	return false
}

// Check is Allows lifted to a coded error for the gateway path.
func (l IPAllowlist) Check(clientIP string) error {
	if l.Allows(clientIP) {
		return nil
	}
	return newError(CodeTokenIPForbidden, "request IP is not on the API token allowlist")
}

// RequestClientIP extracts the client source address from the request:
// RemoteAddr is authoritative — X-Forwarded-For/X-Real-IP are consulted
// only when trustedProxy is true (edge deployments that terminate at the
// HAProxy LB; spoofed client headers are never trusted by default).
func RequestClientIP(remoteAddr string, headers map[string][]string, trustedProxy bool) string {
	if trustedProxy {
		if xff := firstHeader(headers, "X-Forwarded-For"); xff != "" {
			// leftmost entry = original client per de-facto standard
			if i := strings.Index(xff, ","); i >= 0 {
				xff = xff[:i]
			}
			if ip := strings.TrimSpace(xff); ip != "" {
				return ip
			}
		}
		if xrip := strings.TrimSpace(firstHeader(headers, "X-Real-IP")); xrip != "" {
			return xrip
		}
	}
	host, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		return remoteAddr // may already be a bare IP
	}
	return host
}

func firstHeader(h map[string][]string, key string) string {
	for k, vs := range h {
		if strings.EqualFold(k, key) && len(vs) > 0 {
			return vs[0]
		}
	}
	return ""
}
