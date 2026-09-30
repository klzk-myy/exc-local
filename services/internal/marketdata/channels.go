// Task 6.3.1 — typed-channel subscription grammar.
//
// Canonical shape (spec §10.5, remediation #9/#35):
//
//	{type}@{target}[:{params}]   public typed channels, e.g.
//	                             book@EUR/USD, depth@EUR/USD:20:100,
//	                             kline@EUR/USD_1h, liquidations@all
//	private:{name}               auth-gated channels (ws.PrivateChannels)
//
// The type prefix selects the channel class used by the §24 #84 session
// budgets: book@/depth@ are L2-class (20 per session), l3@/l3Book@ are
// L3-class (5 per session, Phase-17 feed names), everything else is
// uncapped individually (bounded by the global 200-subscription cap).
package marketdata

import (
	"errors"
	"fmt"
	"strconv"
	"strings"

	"exchange/internal/ws"
)

// ChannelClass is the per-session budget class of a channel.
type ChannelClass int

const (
	// ClassNone carries no class budget (still bounded by MaxSubscriptions).
	ClassNone ChannelClass = iota
	// ClassL2 is the aggregated top-of-book class: book@, depth@.
	// Budget: MaxL2Subscriptions (20) per session.
	ClassL2
	// ClassL3 is the order-level class: l3@, l3Book@ (Phase-17 feed).
	// Budget: MaxL3Subscriptions (5) per session.
	ClassL3
)

func (c ChannelClass) String() string {
	switch c {
	case ClassL2:
		return "L2"
	case ClassL3:
		return "L3"
	}
	return "none"
}

// channelTypes is the registry of public channel types this endpoint
// accepts. Wave-2 producers (trades/ticker/BBO/aggTrades/liquidations/
// OI/stats/referencePrice/kline/block tape) subscribe under these names —
// a type absent from the table is rejected INVALID_REQUEST so typos never
// silently bind a subscription that can never carry data.
var channelTypes = map[string]ChannelClass{
	"book":           ClassL2,
	"depth":          ClassL2, // Task 6.3.15 parameterization (levels:cadence) lands separately
	"l3":             ClassL3,
	"l3Book":         ClassL3,
	"bbo":            ClassNone,
	"aggTrades":      ClassNone,
	"trades":         ClassNone,
	"ticker":         ClassNone,
	"kline":          ClassNone,
	"openInterest":   ClassNone,
	"referencePrice": ClassNone,
	"liquidations":   ClassNone,
	"stats":          ClassNone, // Task 6.3.20 all-market statistics
	"miniTicker":     ClassNone, // Task 6.3.20 all-symbol mini ticker
	"blockTrades":    ClassNone, // Task 6.3.20 delayed block tape
	// Phase-23 Task 23.3.3/23.3.5 premium feeds — every bind routes
	// through FeedEntitlements (premium.go): authenticated premium tier +
	// an ACTIVE per-feed subscription (migration 257). premium_l3 shares
	// the L3-class 5-subscription budget with l3@/l3Book@.
	"premium_l3": ClassL3,
	"depth_full": ClassL2,
	"auction":    ClassNone,
	"greeks":     ClassNone, // Task 23.3.5 — spec §24 #250
	// lpBook@{lpID}/{symbol} — per-LP priced book distribution
	// (Task 7.3.9 consumer, lp_pricing.go): markup/skew applied per
	// lp_instrument_configs before fanout. L2-class — it is book data.
	"lpBook": ClassL2,
}

// klineIntervals is the canonical 13-timeframe set pinned by spec §24
// #264 / §16.2 / Task 6.3.14 / migration 173 and consumed by the ohlcv
// engine's CanonicalIntervals (1s,1m,5m,15m,30m,1h,2h,4h,6h,8h,1D,1W,1M).
// Accepted case-insensitively; the frame retains the client's verbatim
// casing. (The §27.1 matrix row that once listed 3m/12h/1d was stale
// draft residue — corrected 2026-10-05, remediation #44 follow-on.)
var klineIntervals = map[string]bool{
	"1s": true, "1m": true, "5m": true, "15m": true,
	"30m": true, "1h": true, "2h": true, "4h": true, "6h": true,
	"8h": true,
	"1d": true, "1D": true,
	"1w": true, "1W": true,
	"1M": true,
}

// Channel is the parsed subscription target.
type Channel struct {
	Raw     string       // verbatim channel token
	Type    string       // "book", "kline", "private:orders", ...
	Target  string       // symbol token ("EUR/USD"), "{symbol}_{tf}", or "all"
	Params  string       // optional "{p}:{p}" tail (depth levels:cadence)
	Class   ChannelClass // budget class
	Private bool         // auth-gated ws.PrivateChannels member
	Depth   DepthVariant // resolved depth params (Type=="depth" only)
	// LPID is the lpBook@{lpID}/{symbol} LP id (Type=="lpBook" only; 0
	// otherwise). Target stays the symbol so symbol-level entitlements
	// (DenySymbols) apply to the instrument, not the "7/EUR/USD" pair
	// token.
	LPID int64
}

// DepthVariant is one supported depth@{symbol}:{levels}:{cadence_ms}
// parameterization (Task 6.3.15, spec §24 #265). Levels selects the
// top-N slice of the internal 20-level snapshot; CadenceMs bounds the
// emit frequency. The bare depth@{symbol} form is the canonical default
// 20:100 per Task 6.3.15 ("Default remains 20:100"); a partial form
// depth@{symbol}:{levels} takes the default 100ms cadence.
type DepthVariant struct {
	Levels    int `json:"levels"`
	CadenceMs int `json:"cadence_ms"`
}

// DefaultDepthVariant is the bare depth@{symbol} contract (Task 6.3.15).
var DefaultDepthVariant = DepthVariant{Levels: 20, CadenceMs: 100}

// depthLevels / depthCadences are the §24 #265 supported sets. The
// combination table is the cross product (3 levels × 3 cadences = 9
// variants); subscriptions outside it reject INVALID_DEPTH_LIMIT.
var (
	depthLevels   = map[int]bool{5: true, 10: true, 20: true}
	depthCadences = map[int]bool{100: true, 250: true, 1000: true}
)

// ChannelError carries an optional spec §23 error code so bind-time
// rejections surface the dedicated code instead of collapsing to
// INVALID_REQUEST.
type ChannelError struct {
	Code string
	Msg  string
}

func (e *ChannelError) Error() string { return e.Msg }

// channelCode extracts the §23 code from a ParseChannel error, defaulting
// to INVALID_REQUEST for malformed-grammar rejections.
func channelCode(err error) string {
	var ce *ChannelError
	if errors.As(err, &ce) && ce.Code != "" {
		return ce.Code
	}
	return "INVALID_REQUEST"
}

func depthLimitErrf(format string, args ...any) error {
	return &ChannelError{Code: "INVALID_DEPTH_LIMIT", Msg: fmt.Sprintf(format, args...)}
}

// parseDepthParams validates a depth@ params tail. Accepted forms:
// "{levels}:{cadence_ms}", "{levels}" (cadence defaults to 100), and ""
// (full default). Returns the resolved DepthVariant or a wire-safe error.
func parseDepthParams(params string) (DepthVariant, error) {
	if params == "" {
		return DefaultDepthVariant, nil
	}
	parts := strings.Split(params, ":")
	if len(parts) > 2 {
		return DepthVariant{}, depthLimitErrf(
			"depth params must be {levels}:{update_ms}, got %q", params)
	}
	levels, err := strconv.Atoi(parts[0])
	if err != nil || !depthLevels[levels] {
		return DepthVariant{}, depthLimitErrf(
			"depth levels %q unsupported — allowed: 5, 10, 20", parts[0])
	}
	v := DepthVariant{Levels: levels, CadenceMs: DefaultDepthVariant.CadenceMs}
	if len(parts) == 2 {
		cad, err := strconv.Atoi(parts[1])
		if err != nil || !depthCadences[cad] {
			return DepthVariant{}, depthLimitErrf(
				"depth cadence %q unsupported — allowed: 100, 250, 1000 ms", parts[1])
		}
		v.CadenceMs = cad
	}
	return v, nil
}

// maxChannelLen mirrors the ws package's 64-byte channel bound (spec §10.6).
const maxChannelLen = 64

// channelCharset admits the runes of the typed namespace: letters,
// digits, '_', '-', '.', '/', ':' and '@'. '/' is required for the
// canonical FX pair form "EUR/USD" (spec §10.5 examples) — the gateway's
// ws package predates the slash form; this package's grammar is the
// market-data contract.
func channelCharsetOK(s string) bool {
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '_', r == '-', r == '.', r == '/', r == ':', r == '@':
		default:
			return false
		}
	}
	return true
}

// ParseChannel validates and classifies a subscription token.
// Error strings are wire-safe (rendered into INVALID_REQUEST messages).
func ParseChannel(raw string) (Channel, error) {
	if len(raw) == 0 || len(raw) > maxChannelLen {
		return Channel{}, fmt.Errorf("channel length outside 1..%d", maxChannelLen)
	}
	if !channelCharsetOK(raw) {
		return Channel{}, fmt.Errorf("channel %q contains invalid characters", raw)
	}

	if strings.HasPrefix(raw, "private:") {
		if !ws.PrivateChannels[raw] {
			return Channel{}, fmt.Errorf("unknown private channel %q", raw)
		}
		return Channel{Raw: raw, Type: raw, Private: true}, nil
	}

	at := strings.IndexByte(raw, '@')
	if at <= 0 || at == len(raw)-1 {
		return Channel{}, fmt.Errorf("channel %q must be {type}@{target}", raw)
	}
	typ := raw[:at]
	rest := raw[at+1:]
	class, known := channelTypes[typ]
	if !known {
		return Channel{}, fmt.Errorf("unknown channel type %q", typ)
	}

	target, params := rest, ""
	if i := strings.IndexByte(rest, ':'); i >= 0 {
		target, params = rest[:i], rest[i+1:]
		if params == "" {
			return Channel{}, fmt.Errorf("channel %q has empty params", raw)
		}
	}
	if target == "" {
		return Channel{}, fmt.Errorf("channel %q has empty target", raw)
	}
	if typ == "kline" {
		// kline@{symbol}_{timeframe}: the timeframe suffix is validated so
		// a typo'd interval does not bind a subscription that never fires.
		us := strings.LastIndexByte(target, '_')
		if us <= 0 || us == len(target)-1 {
			return Channel{}, fmt.Errorf("kline channel %q must be kline@{symbol}_{timeframe}", raw)
		}
		if !klineIntervals[target[us+1:]] {
			return Channel{}, &ChannelError{Code: "INVALID_INTERVAL",
				Msg: fmt.Sprintf("kline channel %q carries an unknown timeframe", raw)}
		}
	}
	ch := Channel{Raw: raw, Type: typ, Target: target, Params: params,
		Class: class}
	if params != "" && typ != "depth" {
		// Only depth@ is parameterized (Task 6.3.15). A params tail on
		// any other type is a typo that would bind a dead subscription.
		return Channel{}, fmt.Errorf(
			"channel %q carries params, but only depth@ is parameterized", raw)
	}
	if typ == "depth" {
		// depth@{symbol}:{levels}:{update_ms} — enforce the §24 #265
		// supported-combination table at bind time so an invalid variant
		// never reserves a subscription slot that can carry no data.
		v, err := parseDepthParams(params)
		if err != nil {
			return Channel{}, err
		}
		ch.Depth = v
	}
	if typ == "lpBook" {
		// lpBook@{lpID}/{symbol}: the LP id is the first path segment —
		// a non-numeric or empty half is a typo that would bind a
		// subscription no producer can satisfy.
		lpTok, sym, ok := strings.Cut(target, "/")
		if !ok || sym == "" {
			return Channel{}, fmt.Errorf(
				"lpBook channel %q must be lpBook@{lp_id}/{symbol}", raw)
		}
		lpID, err := strconv.ParseInt(lpTok, 10, 64)
		if err != nil || lpID <= 0 {
			return Channel{}, fmt.Errorf(
				"lpBook channel %q carries an invalid lp_id %q", raw, lpTok)
		}
		ch.LPID = lpID
		ch.Target = sym // symbol-level entitlements see the instrument
	}
	return ch, nil
}
