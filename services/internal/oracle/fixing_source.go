package oracle

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	goredis "github.com/redis/go-redis/v9"

	"exchange/internal/instruments"
	"exchange/pkg/decimal"
)

// ---------------------------------------------------------------------------
// FixingMarkSource — instruments.PriceSource adapter (spec §6.4,
// Phase-16 Task 16.3.9 step 5: "official benchmark rate publication
// received via Price Oracle / Mark Price Service (Phase 19.5)")
// ---------------------------------------------------------------------------

// FixingMarkSource serves the benchmark rate the fixing scheduler
// records: the oracle mark live at the firing instant — the median of
// >=MinFeeds fresh vendor feeds after the §19.5 staleness/divergence
// gates (the venue's contracted "official published benchmark" surface).
//
// Freshness is bounded: the mark's oracle:mark:{sym}:ts stamp must sit
// within MaxAge of now (default = the canonical StaleAfter 5s gate —
// marks publish on a 1s cadence, so a live oracle is comfortably inside
// the bound). An absent, unreadable, or stale mark returns an error;
// the scheduler then writes SKIPPED — never a synthetic rate.
type FixingMarkSource struct {
	// Get is the Redis GET seam (value-or-error). Wired from the
	// coordination client's embedded go-redis Get in production.
	Get    func(ctx context.Context, key string) (string, error)
	MaxAge time.Duration    // 0 → StaleAfter
	Now    func() time.Time // nil → time.Now().UTC()
}

// NewRedisFixingMarkSource binds the source to a go-redis-compatible
// client (the services redis.Client embeds *goredis.Client, so it
// satisfies this seam directly).
func NewRedisFixingMarkSource(c interface {
	Get(ctx context.Context, key string) *goredis.StringCmd
}) *FixingMarkSource {
	return &FixingMarkSource{Get: func(ctx context.Context, k string) (string, error) {
		return c.Get(ctx, k).Result()
	}}
}

// FixingRate implements instruments.PriceSource.
func (s *FixingMarkSource) FixingRate(ctx context.Context, symbol, benchmark string,
	at time.Time) (*instruments.FixingRate, error) {
	if s == nil || s.Get == nil {
		return nil, fmt.Errorf("oracle: fixing source unwired")
	}
	raw, err := s.Get(ctx, MarkKey(symbol))
	if err != nil {
		if errors.Is(err, goredis.Nil) {
			return nil, fmt.Errorf("oracle: no mark published for %s", symbol)
		}
		return nil, fmt.Errorf("oracle: mark read %s: %w", symbol, err)
	}
	rate, err := decimal.NewFromString(raw)
	if err != nil || !rate.IsPositive() {
		return nil, fmt.Errorf("oracle: mark %s unreadable %q", symbol, raw)
	}

	tsRaw, err := s.Get(ctx, OracleMarkTsKey(symbol))
	if err != nil {
		return nil, fmt.Errorf("oracle: mark ts %s: %w", symbol, err)
	}
	ns, err := strconv.ParseInt(tsRaw, 10, 64)
	if err != nil {
		return nil, fmt.Errorf("oracle: mark ts %s unreadable %q", symbol, tsRaw)
	}
	markAt := time.Unix(0, ns)
	maxAge := s.MaxAge
	if maxAge <= 0 {
		maxAge = StaleAfter
	}
	now := time.Now().UTC()
	if s.Now != nil {
		now = s.Now()
	}
	if d := now.Sub(markAt); d > maxAge || d < -maxAge {
		return nil, fmt.Errorf("oracle: mark %s stale (age %s > %s bound)",
			symbol, d.Truncate(time.Millisecond), maxAge)
	}
	return &instruments.FixingRate{
		Rate:   rate,
		Source: "oracle-mark:" + benchmark,
	}, nil
}

// Compile-time contract check.
var _ instruments.PriceSource = (*FixingMarkSource)(nil)
