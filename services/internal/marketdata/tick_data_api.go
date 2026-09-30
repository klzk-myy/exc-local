// Phase-23 Task 23.3.4 — historical tick data REST contract (spec §10,
// §14, §24 #236):
//
//	GET /api/v1/history/ticks/{symbol}?from=&to=&limit=&cursor=
//
// (canonical path — remediation #10 supersedes the
// /api/v1/market-data/ticks/{instrument} variant; the endpoint itself is
// served by internal/api/handlers_history.go — this file owns the
// task's market-data-domain rules: access tiers, the free-tier window +
// publication delay, content negotiation, and the CSV/FIX renderers.)
//
// Tiering (remediation #35 — supersedes Task 23.3.1's 100/1000-per-min
// scheme and the divergent retail/institutional numbers in this task's
// item 5):
//
//	free      → rate tiers Public/Basic:   last-30-days window AND a
//	             minimum 15-minute publication delay (regulatory delay
//	             for unpaid tape).
//	premium   → rate tiers Professional/Institutional: full history,
//	             real-time.
//	staff     → compliance/staff roles (operator identity): exempt from
//	             window and delay.
//
// Resolution is a seam (HistoryTierResolver) injected by the wiring
// layer: a nil resolver degrades to free — the SAFE default, since
// under-entitlement is never a correctness violation — while a resolver
// ERROR resolves free + degraded so a lookup outage can never mint
// premium access.
package marketdata

import (
	"context"
	"fmt"
	"io"
	"net/textproto"
	"strings"
	"time"

	"exchange/internal/auth"
	"exchange/internal/middleware"
	"exchange/internal/ratelimit"
	"exchange/pkg/decimal"
)

// HistoryFreeWindowDays is the free tier's lookback ceiling
// (Task 23.3.4 item 6: "free tier (last 30 days)").
const HistoryFreeWindowDays = 30

// HistoryFreeDelay is the free tier's minimum publication delay
// (Task 23.3.4 item 7: "minimum 15-minute delay for free tier").
const HistoryFreeDelay = 15 * time.Minute

// HistoryAccess is the caller's historical-data entitlement class.
type HistoryAccess string

const (
	HistoryAccessFree    HistoryAccess = "free"    // Public/Basic — 30d window + 15min delay
	HistoryAccessPremium HistoryAccess = "premium" // Professional/Institutional — full history, real-time
	HistoryAccessStaff   HistoryAccess = "staff"   // compliance/staff — exempt
)

// HistoryTierResolver maps the request identity to a HistoryAccess.
// claims is nil for anonymous traffic (the history routes ride
// authPublic/authRead). The resolver itself must stay cheap — it runs
// once per history request.
//
// Contract: resolve as narrowly as the identity supports — when in
// doubt return HistoryAccessFree, never Premium; a resolver error means
// the handler falls back to free + degraded (fail-closed to the
// restrictive tier, §2.7). Staff detection (compliance/operator
// identity) is a wiring concern: the production resolver checks the
// admin-session binding; RateTierHistoryResolver covers the claims-tier
// half.
type HistoryTierResolver func(ctx context.Context, claims *auth.Claims) (HistoryAccess, error)

// HistoryAccessForRateTier is the canonical rate-tier → access mapping
// (remediation #35). Standard and Demo collapse into Free — neither was
// granted premium history by the task; unknown/empty tiers are Free
// (fail-closed). TierAdmin is operator identity → staff exemption.
func HistoryAccessForRateTier(t ratelimit.Tier) HistoryAccess {
	switch t {
	case ratelimit.TierProfessional, ratelimit.TierInstitutional:
		return HistoryAccessPremium
	case ratelimit.TierAdmin:
		return HistoryAccessStaff
	default:
		return HistoryAccessFree // Public, Basic, Standard, Demo, unknown
	}
}

// RateTierHistoryResolver adapts the platform's claims→rate-limit-tier
// resolver (middleware.TierResolver — the same seam the edge limiter
// and the introspection endpoint use) to HistoryTierResolver. A nil
// claims or a Public resolution still yields Free — authenticated-basic
// is indistinguishable from anonymous for history purposes. This
// adapter never reports errors; deployments that wire a fallible
// identity lookup get the free+degraded fail-closed behavior from the
// HistoryTierResolver contract itself.
func RateTierHistoryResolver(tr middleware.TierResolver) HistoryTierResolver {
	return func(ctx context.Context, claims *auth.Claims) (HistoryAccess, error) {
		if claims == nil || tr == nil {
			return HistoryAccessFree, nil
		}
		return HistoryAccessForRateTier(tr(ctx, claims)), nil
	}
}

// HistoryWindow is the effective [From, To) query bounds after tier
// clamping. A zero bound is unbounded.
type HistoryWindow struct {
	From time.Time // inclusive (zero = unbounded)
	To   time.Time // exclusive (zero = unbounded)
}

// Empty reports whether the window can contain no row — a clamped
// range whose bounds meet or cross. Callers answer an empty page
// WITHOUT touching the store (legitimately empty, not a failure).
func (w HistoryWindow) Empty() bool {
	return !w.From.IsZero() && !w.To.IsZero() && !w.From.Before(w.To)
}

// ResolveHistoryWindow clamps the caller's [from, to) bounds to the
// access tier's entitlement:
//
//	free:    From ≥ now−30d, To ≤ now−15min — the last-30-days window
//	         AND the minimum publication delay both apply to whatever
//	         the client asked for (a wider request narrows, never 403s).
//	premium/staff: bounds pass through unclamped.
//
// now is injectable so the delay horizon is deterministic under test.
func ResolveHistoryWindow(access HistoryAccess, from, to time.Time, now time.Time) HistoryWindow {
	w := HistoryWindow{From: from, To: to}
	if access != HistoryAccessFree {
		return w
	}
	floor := now.UTC().AddDate(0, 0, -HistoryFreeWindowDays)
	if w.From.IsZero() || w.From.Before(floor) {
		w.From = floor
	}
	ceil := now.UTC().Add(-HistoryFreeDelay)
	if w.To.IsZero() || w.To.After(ceil) {
		w.To = ceil
	}
	return w
}

// HistoryDelayFor returns the tier's publication delay — the horizon
// that decides "closed interval" for the response cache and stamps the
// delayed flag. Free: 15min; everyone else: real-time.
func HistoryDelayFor(access HistoryAccess) time.Duration {
	if access == HistoryAccessFree {
		return HistoryFreeDelay
	}
	return 0
}

// ---------------------------------------------------------------------------
// Content negotiation (Task 23.3.4 item 4)
// ---------------------------------------------------------------------------

// HistoryFormat is the negotiated response representation.
type HistoryFormat string

const (
	HistoryFormatJSON HistoryFormat = "json" // §8.8 envelope (default)
	HistoryFormatCSV  HistoryFormat = "csv"  // Accept: text/csv
	HistoryFormatFIX  HistoryFormat = "fix"  // Accept: application/x-fix — drop-copy lines
)

// ContentType maps a format to its response media type.
func (f HistoryFormat) ContentType() string {
	switch f {
	case HistoryFormatCSV:
		return "text/csv"
	case HistoryFormatFIX:
		return "application/x-fix"
	default:
		return "application/json"
	}
}

// HistoryFormatFor negotiates the Accept header — first matching media
// type wins (q-values are honored implicitly by order only when the
// client lists several; json is the default for absent/`*/*`/unmatched
// headers). Anything-but-JSON is opt-in: a browser default
// (`text/html,...`) still gets the JSON envelope.
func HistoryFormatFor(accept string) HistoryFormat {
	if accept == "" {
		return HistoryFormatJSON
	}
	best, bestQ := HistoryFormatJSON, -1.0
	for _, part := range strings.Split(accept, ",") {
		mt := textproto.TrimString(part)
		semi := strings.IndexByte(mt, ';')
		media := mt
		if semi >= 0 {
			media = mt[:semi]
		}
		media = strings.ToLower(textproto.TrimString(media))
		var f HistoryFormat
		switch media {
		case "text/csv":
			f = HistoryFormatCSV
		case "application/x-fix":
			f = HistoryFormatFIX
		default:
			continue
		}
		q := 1.0
		if semi >= 0 {
			for _, p := range strings.Split(mt[semi+1:], ";") {
				p = textproto.TrimString(p)
				if strings.HasPrefix(p, "q=") {
					var v float64
					if _, err := fmt.Sscanf(p[2:], "%g", &v); err == nil {
						q = v
					}
				}
			}
		}
		if q > bestQ {
			best, bestQ = f, q
		}
	}
	return best
}

// ---------------------------------------------------------------------------
// Renderers — page-capped bodies (Task 23.3.2's 1M export ceiling does
// NOT apply here; these emit the single keyset page the query returned)
// ---------------------------------------------------------------------------

// TickExportRow is the renderers' input row — the api layer projects
// analytics.Tick onto it (that package cannot be imported here:
// analytics → funding → marketdata would close an import cycle).
// Field-for-field identical to the tape row; keep them aligned.
type TickExportRow struct {
	TradeID  uint64
	EventSeq uint64
	ShardID  uint32
	Symbol   string
	Price    decimal.Decimal
	Qty      decimal.Decimal
	Side     string
	Ts       time.Time
}

// WriteTicksCSV streams ticks as RFC-4180-style rows:
//
//	trade_id,event_seq,shard_id,time,price,quantity,side
//
// Money fields stay decimal strings (StringFixed(8)) — never float.
func WriteTicksCSV(w io.Writer, ticks []TickExportRow) error {
	if _, err := io.WriteString(w,
		"trade_id,event_seq,shard_id,time,price,quantity,side\n"); err != nil {
		return err
	}
	for _, t := range ticks {
		if _, err := fmt.Fprintf(w, "%d,%d,%d,%s,%s,%s,%s\n",
			t.TradeID, t.EventSeq, t.ShardID,
			t.Ts.UTC().Format("2006-01-02T15:04:05.000Z"),
			t.Price.StringFixed(8), t.Qty.StringFixed(8), t.Side); err != nil {
			return err
		}
	}
	return nil
}

// fixSide renders the FIX tag-54 side convention (1=Buy, 2=Sell, 0=unknown).
func fixSide(side string) string {
	switch side {
	case "BUY":
		return "1"
	case "SELL":
		return "2"
	default:
		return "0"
	}
}

// WriteTicksFIXDropCopy streams one FIX tag=value line per tick — the
// minimal drop-copy shape the task pins:
//
//	11={trade_id} 17={exec_id} 55={symbol} 31={price} 32={qty} 54={side} 60={ts}
//
// Fields are SOH-delimited per FIX wire convention and each row ends
// with SOH+LF so a text consumer can split lines while a FIX consumer
// can re-frame on SOH. 17 carries event_seq (the per-symbol execution
// sequence — the venue's ExecID axis); 60 is FIX UTCTIMESTAMP.
func WriteTicksFIXDropCopy(w io.Writer, ticks []TickExportRow) error {
	const soh = '\x01'
	for _, t := range ticks {
		if _, err := fmt.Fprintf(w,
			"11=%d%c17=%d%c55=%s%c31=%s%c32=%s%c54=%s%c60=%s%c\n",
			t.TradeID, soh,
			t.EventSeq, soh,
			t.Symbol, soh,
			t.Price.StringFixed(8), soh,
			t.Qty.StringFixed(8), soh,
			fixSide(t.Side), soh,
			t.Ts.UTC().Format("20060102-15:04:05.000"), soh); err != nil {
			return err
		}
	}
	return nil
}
