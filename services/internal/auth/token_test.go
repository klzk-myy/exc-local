// Unit tests for the API-token IP allowlist (Task 5.3.9).
package auth

import (
	"net/http"
	"testing"
)

func TestIPAllowlistEmptyPermitsAll(t *testing.T) {
	al, err := ParseIPAllowlist(nil)
	if err != nil {
		t.Fatal(err)
	}
	if !al.Allows("198.51.100.7") || !al.Allows("2001:db8::1") {
		t.Fatal("empty allowlist must be unrestricted")
	}
}

func TestIPAllowlistIPv4AndCIDR(t *testing.T) {
	al, err := ParseIPAllowlist([]string{"192.0.2.10", "10.0.0.0/8"})
	if err != nil {
		t.Fatal(err)
	}
	if !al.Allows("192.0.2.10") || !al.Allows("10.44.0.9") {
		t.Fatal("expected allow hits")
	}
	if al.Allows("192.0.2.11") || al.Allows("11.0.0.1") {
		t.Fatal("non-matching IPv4 must be rejected")
	}
	if err := al.Check("11.0.0.1"); !codeIs(err, CodeTokenIPForbidden) {
		t.Fatalf("want TOKEN_IP_FORBIDDEN, got %v", err)
	}
}

func TestIPAllowlistIPv6(t *testing.T) {
	al, err := ParseIPAllowlist([]string{"2001:db8::/32", "::1"})
	if err != nil {
		t.Fatal(err)
	}
	if !al.Allows("2001:db8:abcd::1") || !al.Allows("::1") {
		t.Fatal("expected IPv6 hits")
	}
	if al.Allows("2001:dead::1") || al.Allows("8.8.8.8") {
		t.Fatal("non-matching IPv6/IPv4 must be rejected")
	}
	// IPv4-mapped IPv6 forms are normalized.
	al4, _ := ParseIPAllowlist([]string{"192.0.2.5"})
	if !al4.Allows("::ffff:192.0.2.5") {
		t.Fatal("IPv4-mapped IPv6 must match the IPv4 entry")
	}
}

func TestIPAllowlistMalformedFailsClosed(t *testing.T) {
	if _, err := ParseIPAllowlist([]string{"192.0.2.0/33"}); err == nil {
		t.Fatal("bad CIDR must reject the whole list")
	}
	if _, err := ParseIPAllowlist([]string{"not-an-ip"}); err == nil {
		t.Fatal("garbage entry must reject")
	}
	al, _ := ParseIPAllowlist([]string{"10.0.0.1"})
	if al.Allows("garbage") || al.Allows("") {
		t.Fatal("unparseable client IP must never match")
	}
}

func TestRequestClientIPTrustRules(t *testing.T) {
	h := http.Header{}
	h.Set("X-Forwarded-For", "203.0.113.9, 10.1.2.3")
	// Untrusted edge: XFF ignored, RemoteAddr wins.
	if ip := RequestClientIP("198.51.100.1:5555", h, false); ip != "198.51.100.1" {
		t.Fatalf("untrusted proxy must use RemoteAddr, got %s", ip)
	}
	// Trusted edge: leftmost XFF entry wins.
	if ip := RequestClientIP("198.51.100.1:5555", h, true); ip != "203.0.113.9" {
		t.Fatalf("trusted proxy must use leftmost XFF, got %s", ip)
	}
	// Bare RemoteAddr without port.
	if ip := RequestClientIP("192.0.2.77", nil, false); ip != "192.0.2.77" {
		t.Fatalf("bare addr: %s", ip)
	}
}
