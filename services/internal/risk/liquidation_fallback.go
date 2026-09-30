// liquidation_fallback.go — Task 19.5.3.6 risk-side consumer: the
// stale-price liquidation ladder and flash-crash freeze.
//
// The oracle publishes oracle:fallback:{symbol} (JSON StaleReference
// mirror below) plus oracle:fallback:{symbol}:freeze (ms-epoch flash
// cooling expiry). This file reads that channel — the liquidation
// engine must NOT freeze when oracle feeds go stale (§24 #196): it
// de-rates to the tiered reference price instead, and every event row
// carries liquidation_basis=STALE_MARK (migration 236) for the
// post-incident audit.
//
//	Tier           staleness   haircut   routing
//	STALE_SHORT    5–15s       2%        normal ladder
//	STALE_MEDIUM   15–60s      5%        auction-only
//	STALE_LONG     >60s        10%       FORCE_CASH + P0 alert
//	FLASH_COOL     >5%/1s move 5s freeze (HALT), then the ladder
//
// Key-contract ownership note: oracle owns the writer contract
// (oracle/staleness_fallback.go); this file mirrors the JSON shape and
// key names — the same single-source discipline as keys.go's mirrored
// margin-level layout.
package risk

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"time"

	goredis "github.com/redis/go-redis/v9"

	"exchange/pkg/decimal"
)

// StaleFallbackState mirrors oracle.FallbackState.
type StaleFallbackState string

const (
	StaleFallbackPrice    StaleFallbackState = "STALE_PRICE"
	StaleFallbackFlash    StaleFallbackState = "FLASH_COOL"
	LiquidationBasisMark                     = "MARK"       // fresh oracle mark
	LiquidationBasisStale                    = "STALE_MARK" // stale-price fallback leg (§24 #196)
)

// StaleFallbackTier mirrors oracle.StalenessTier — the mapping is
// pinned in oracle/staleness.go (TierStaleness) and re-verified by the
// Phase-19.5 checkpoint suite.
type StaleFallbackTier int

const (
	TierStaleNone   StaleFallbackTier = 0
	TierStaleShort  StaleFallbackTier = 1 // 5–15s → 2%
	TierStaleMedium StaleFallbackTier = 2 // 15–60s → 5%, auction-only
	TierStaleLong   StaleFallbackTier = 3 // >60s → FORCE_CASH ±10%, P0
)

// Haircut returns the tier's liquidation haircut fraction.
func (t StaleFallbackTier) Haircut() decimal.Decimal {
	switch t {
	case TierStaleShort:
		return decimal.RequireFromString("0.02")
	case TierStaleMedium:
		return decimal.RequireFromString("0.05")
	case TierStaleLong:
		return decimal.RequireFromString("0.10")
	default:
		return decimal.Zero
	}
}

// StaleFallbackRef is the JSON-mirrored oracle.StaleReference.
type StaleFallbackRef struct {
	Symbol      string             `json:"Symbol"`
	State       StaleFallbackState `json:"State"`
	Tier        StaleFallbackTier  `json:"Tier"`
	LastMark    decimal.Decimal    `json:"LastMark"`
	LastAt      time.Time          `json:"LastAt"`
	SinceAt     time.Time          `json:"SinceAt"`
	FreezeUntil time.Time          `json:"FreezeUntil"`
}

// ReferencePrice de-rates the last mark by the tier haircut in the
// pessimistic direction for the liquidated side — closing a long
// prices lower, closing a short prices higher (§2.7 zero-loss).
func (r StaleFallbackRef) ReferencePrice(sideLong bool) decimal.Decimal {
	h := r.Tier.Haircut()
	if !r.LastMark.IsPositive() || !h.IsPositive() {
		return r.LastMark
	}
	if sideLong {
		return r.LastMark.Mul(decimal.One.Sub(h))
	}
	return r.LastMark.Mul(decimal.One.Add(h))
}

// AuctionOnly — the 15s+ tiers restrict closes to the auction ladder.
func (r StaleFallbackRef) AuctionOnly() bool { return r.Tier >= TierStaleMedium }

// ForceCash — the >60s tier settles immediately at the widened band.
func (r StaleFallbackRef) ForceCash() bool { return r.Tier >= TierStaleLong }

// StaleFallbackSource is the liquidation engine's read seam.
type StaleFallbackSource interface {
	// Reference returns the symbol's active fallback record — nil, nil
	// means the oracle is healthy and no de-rating applies.
	Reference(ctx context.Context, symbol string) (*StaleFallbackRef, error)
	// Frozen reports whether a flash-crash cooling freeze is active
	// (liquidation HALT — never skip, never proceed).
	Frozen(ctx context.Context, symbol string) (bool, error)
}

// RedisStaleFallbackSource implements StaleFallbackSource over the
// oracle's published keys.
type RedisStaleFallbackSource struct {
	C *goredis.Client
}

// NewRedisStaleFallbackSource wraps a go-redis client.
func NewRedisStaleFallbackSource(c *goredis.Client) *RedisStaleFallbackSource {
	return &RedisStaleFallbackSource{C: c}
}

// Reference implements StaleFallbackSource.
func (s *RedisStaleFallbackSource) Reference(ctx context.Context,
	symbol string) (*StaleFallbackRef, error) {

	v, err := s.C.Get(ctx, "oracle:fallback:"+symbol).Result()
	if err == goredis.Nil {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("stale fallback read %s: %w", symbol, err)
	}
	var ref StaleFallbackRef
	if err := json.Unmarshal([]byte(v), &ref); err != nil {
		return nil, fmt.Errorf("stale fallback %s malformed: %w", symbol, err)
	}
	return &ref, nil
}

// Frozen implements StaleFallbackSource.
func (s *RedisStaleFallbackSource) Frozen(ctx context.Context,
	symbol string) (bool, error) {

	v, err := s.C.Get(ctx, "oracle:fallback:"+symbol+":freeze").Result()
	if err == goredis.Nil {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("stale freeze read %s: %w", symbol, err)
	}
	ms, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		return false, nil // malformed freeze — treat as not frozen (expired)
	}
	return time.Now().UnixMilli() < ms, nil
}

// ---------------------------------------------------------------------------
// Engine integration
// ---------------------------------------------------------------------------

// staleFallback resolves the close's reference price and routing under
// the fallback ladder. Returns (price, ref) — ref nil means fresh.
// A FLASH_COOL freeze defers the whole close via errFlashFrozen; the
// caller requeues, it never proceeds.
func (s *LiquidationService) staleFallback(ctx context.Context,
	p LiqPosition, symbol string) (decimal.Decimal, *StaleFallbackRef, error) {

	if s.staleFb == nil {
		return p.MarkPrice, nil, nil
	}
	frozen, err := s.staleFb.Frozen(ctx, symbol)
	if err != nil {
		return p.MarkPrice, nil, err
	}
	if frozen {
		return p.MarkPrice, nil, errFlashFrozen{symbol: symbol}
	}
	ref, err := s.staleFb.Reference(ctx, symbol)
	if err != nil {
		return p.MarkPrice, nil, err
	}
	if ref == nil {
		return p.MarkPrice, nil, nil
	}
	px := ref.ReferencePrice(p.Side == "LONG")
	if !px.IsPositive() {
		return p.MarkPrice, ref, nil // last mark non-positive → position mark
	}
	return px, ref, nil
}

// errFlashFrozen marks the flash-cooling deferral — ConsumeOnce maps it
// to a timed requeue (not a failure: liquidation resumes after the 5s
// cooling window).
type errFlashFrozen struct{ symbol string }

func (e errFlashFrozen) Error() string {
	return fmt.Sprintf("liquidation frozen: flash-crash cooling on %s", e.symbol)
}
