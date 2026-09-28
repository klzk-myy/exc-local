// Task 6.3.1 — typed-channel grammar tests.
package marketdata

import (
	"strings"
	"testing"
)

func TestParseChannelGrammar(t *testing.T) {
	good := []struct {
		raw    string
		typ    string
		target string
		class  ChannelClass
	}{
		{"book@EUR/USD", "book", "EUR/USD", ClassL2},
		{"depth@EUR/USD", "depth", "EUR/USD", ClassL2},
		{"depth@EUR/USD:20:100", "depth", "EUR/USD", ClassL2},
		{"bbo@GBP/USD", "bbo", "GBP/USD", ClassNone},
		{"aggTrades@USD/JPY", "aggTrades", "USD/JPY", ClassNone},
		{"trades@EUR/USD", "trades", "EUR/USD", ClassNone},
		{"ticker@EUR/USD", "ticker", "EUR/USD", ClassNone},
		{"kline@EUR/USD_1h", "kline", "EUR/USD_1h", ClassNone},
		{"kline@EUR/USD_1s", "kline", "EUR/USD_1s", ClassNone},
		{"openInterest@EUR/USD", "openInterest", "EUR/USD", ClassNone},
		{"referencePrice@EUR/USD", "referencePrice", "EUR/USD", ClassNone},
		{"liquidations@all", "liquidations", "all", ClassNone},
		{"liquidations@EUR/USD", "liquidations", "EUR/USD", ClassNone},
		{"stats@all", "stats", "all", ClassNone},
		{"l3@EUR/USD", "l3", "EUR/USD", ClassL3},
		{"l3Book@EUR/USD", "l3Book", "EUR/USD", ClassL3},
	}
	for _, tc := range good {
		ch, err := ParseChannel(tc.raw)
		if err != nil {
			t.Errorf("ParseChannel(%q) unexpected error: %v", tc.raw, err)
			continue
		}
		if ch.Type != tc.typ || ch.Target != tc.target || ch.Class != tc.class {
			t.Errorf("ParseChannel(%q) = %+v, want type=%s target=%s class=%s",
				tc.raw, ch, tc.typ, tc.target, tc.class)
		}
		if ch.Private {
			t.Errorf("ParseChannel(%q) marked private", tc.raw)
		}
	}
}

func TestParseChannelPrivate(t *testing.T) {
	// private:* channels resolve through ws.PrivateChannels — "private:orders"
	// is a canonical member.
	ch, err := ParseChannel("private:orders")
	if err != nil {
		t.Fatalf("ParseChannel(private:orders): %v", err)
	}
	if !ch.Private {
		t.Fatal("private:orders not marked private")
	}
	if _, err := ParseChannel("private:bogus"); err == nil {
		t.Fatal("unknown private channel accepted")
	}
}

func TestParseChannelRejects(t *testing.T) {
	bad := []string{
		"",                  // empty
		"book",              // no @
		"book@",             // empty target
		"@EUR/USD",          // empty type
		"wat@EUR/USD",       // unknown type
		"book@EUR/USD:",     // empty params
		"kline@EUR/USD",     // missing timeframe
		"kline@EUR/USD_7h",  // unknown timeframe
		"book@EUR USD",      // space
		"book@EUR/USD;drop", // metachar
		"book@\x01",         // control char
		"private:orders@x",  // private prefix is whole-token
	}
	for _, raw := range bad {
		if _, err := ParseChannel(raw); err == nil {
			t.Errorf("ParseChannel(%q) accepted", raw)
		}
	}
}

func TestParseChannelLengthBound(t *testing.T) {
	long := "book@" + strings.Repeat("A", 100)
	if _, err := ParseChannel(long); err == nil {
		t.Fatal("over-length channel accepted")
	}
}
