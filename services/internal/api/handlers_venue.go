// Tasks 5.3.43 & 5.3.44 — server-time endpoint and the unified
// venue-info document.
//
//	GET /api/v1/time          — PTP-disciplined server clock (tier-exempt)
//	GET /api/v1/exchange-info — venue document: symbols, statuses, filters,
//	                            order types, rate limits, trading hours, ETag
//
// Time is real: the payload carries the UTC wall clock plus the kernel's
// adjtimex(2) sync state from internal/timesync (PTP disciplining source,
// Phase-09 Task 9.3.12 owns the PTP daemon). A client cannot fix drift —
// the endpoint only reports the synced instant and lets clients verify
// against their own clock (±5s resync guidance).
package api

import (
	"context"
	"net/http"
	"time"

	"exchange/internal/accounts"
	"exchange/internal/gateway"
	"exchange/internal/marketapi"
	"exchange/internal/timesync"
)

// ServerTime implements Task 5.3.43 / spec §8.9 item 1. src is the
// timesync clock source (timesync.KernelSource in production — the
// adjtimex(2) probe; the PTP daemon itself is Phase-09 Task 9.3.12); a
// nil src omits the sync diagnostics rather than claiming a state the
// gateway cannot substantiate. The endpoint never fails closed on an
// unsynced clock — clients still need a timestamp to compare against; the
// "synchronized" field carries the truth.
func ServerTime(src timesync.Source, now func() time.Time) http.HandlerFunc {
	if now == nil {
		now = time.Now
	}
	return func(w http.ResponseWriter, r *http.Request) {
		resp := map[string]any{
			"server_time_ms": now().UTC().UnixMilli(),
			"timezone":       "UTC",
		}
		if src != nil {
			offset, synced, err := src()
			resp["synchronized"] = err == nil && synced
			if err == nil {
				resp["offset_us"] = offset.Microseconds()
			}
		}
		WriteJSON(w, http.StatusOK, resp)
	}
}

// VenueProfileLister publishes the per-profile instrument scope
// (Phase-14 Task 14.3.13 — *accounts.ProfileService satisfies it); nil
// omits the product_profiles section (pre-095 deployments).
type VenueProfileLister interface {
	ListProfiles(ctx context.Context, includeRetired bool) ([]accounts.ProductProfile, error)
}

// VenueDeps bundles the seams behind the exchange-info document.
type VenueDeps struct {
	Store    marketapi.Store
	Cache    *marketapi.Cache
	Profiles VenueProfileLister // optional — per-profile scope section
	Now      func() time.Time
}

func (d *VenueDeps) now() time.Time {
	if d.Now != nil {
		return d.Now()
	}
	return time.Now()
}

// ExchangeInfo implements Task 5.3.44 / spec §8.9 item 2: one call
// returns the venue document a client would otherwise assemble from N
// discovery calls. Supports HTTP caching: If-None-Match → 304 (AC #379).
// The instrument list rides the 1min §10.3 cache; the ETag is a content
// hash so an instrument mutation (updated_at) flips the tag immediately
// after cache expiry — which the Phase-15/Phase-06 system.status WS
// signals (Task 15.3.8) tell clients to re-fetch.
func ExchangeInfo(d *VenueDeps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		cache := d.Cache
		if cache == nil {
			cache = marketapi.NewCache(nil)
		}
		list, err := cached(cache, "instruments:all", instrumentsCacheTTL,
			func() ([]marketapi.Instrument, error) {
				return d.Store.ListInstruments(r.Context())
			})
		if err != nil {
			WriteError(w, "SERVICE_DEGRADED",
				"instrument store unavailable",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		doc := marketapi.BuildVenueInfo(list, d.now())
		// Task 14.3.13 — publish each ACTIVE product profile's scope so
		// clients discover eligibility before order entry.
		if d.Profiles != nil {
			profs, err := d.Profiles.ListProfiles(r.Context(), false)
			if err != nil {
				WriteError(w, "SERVICE_DEGRADED",
					"product-profile read unavailable",
					gateway.RequestIDFrom(r.Context()), nil)
				return
			}
			for _, p := range profs {
				doc.ProductProfiles = append(doc.ProductProfiles, marketapi.ProfileDoc{
					Code:            p.Code,
					PricingPlan:     p.PricingPlan,
					InstrumentScope: p.InstrumentScope,
					SubunitDivisor:  p.SubunitDivisor,
					MinDeposit:      p.MinDeposit.String(),
				})
			}
		}
		etag, err := marketapi.VenueETag(doc)
		if err != nil {
			WriteError(w, "INTERNAL_ERROR",
				"venue document encoding failed",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		w.Header().Set("ETag", etag)
		if r.Header.Get("If-None-Match") == etag {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		WriteJSON(w, http.StatusOK, doc)
	}
}
