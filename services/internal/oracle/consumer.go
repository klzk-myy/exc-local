package oracle

import (
	"context"
	"fmt"
	"strconv"
	"time"

	goredis "github.com/redis/go-redis/v9"

	"exchange/pkg/decimal"
	excerrors "exchange/pkg/errors"
)

// ErrOracleUnavailable is the coded fail-closed rejection for
// margin-increasing order admission while the oracle is UNAVAILABLE
// (Task 19.5.3.7 step 2; registered code PRICE_ORACLE_UNAVAILABLE, 503).
var ErrOracleUnavailable = excerrors.New("PRICE_ORACLE_UNAVAILABLE",
	"fewer than 2 independent oracle feeds fresh — margin orders halted")

// ---------------------------------------------------------------------------
// Consumer seam (Task 19.5.3.4) — the single oracle every consumer reads.
//
// Provider implements risk.MarkPriceProvider over the published Redis
// keyspace so Phase-19 margin machinery reads oracle marks with zero
// caller changes; it is bound in cmd/gateway ahead of the last-trade
// stub (stub stays as the no-oracle dev fallback).
//
// HealthGate is the fail-closed admission seam: margin-increasing order
// admission checks it and rejects PRICE_ORACLE_UNAVAILABLE (503) when
// the oracle reports UNAVAILABLE (Task 19.5.3.7 step 2).
// ---------------------------------------------------------------------------

// Provider reads published oracle marks. stalenessGate bounds how old a
// mark may be before it reports ErrMarkNotFound (consumers treat that
// as a data condition; the HEALTH gate separately blocks admission).
type Provider struct {
	C           *goredis.Client
	StaleAfter  time.Duration // default 5s
	HealthKeyFn func(symbol string) string
	now         func() time.Time
}

// NewProvider wires the Redis reader.
func NewProvider(c *goredis.Client) *Provider {
	return &Provider{C: c, StaleAfter: StaleAfter, now: time.Now,
		HealthKeyFn: HealthKey}
}

// MarkView is the provider's read result (mirrors risk.MarkPrice's
// provenance contract without importing the consumer package).
type MarkView struct {
	Price   decimal.Decimal
	ValidAt time.Time
	Stale   bool
	Found   bool
}

// Mark reads oracle:mark:{sym} + :ts — the C++-shared 1e8 contract.
func (p *Provider) Mark(ctx context.Context, symbol string) (MarkView, error) {
	vals, err := p.C.MGet(ctx, OracleMarkKey(symbol), OracleMarkTsKey(symbol)).Result()
	if err != nil {
		return MarkView{}, fmt.Errorf("oracle mark read %s: %w", symbol, err)
	}
	s, ok := vals[0].(string)
	if !ok || s == "" {
		return MarkView{}, nil // absent — data condition, not an error
	}
	ticks, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return MarkView{}, fmt.Errorf("oracle mark %s malformed %q", symbol, s)
	}
	v := MarkView{
		Price: decimal.NewFromInt(ticks).Div(decimal.NewFromInt(100_000_000)),
		Found: true,
	}
	if ts, ok := vals[1].(string); ok {
		if ns, err := strconv.ParseInt(ts, 10, 64); err == nil {
			v.ValidAt = time.Unix(0, ns)
			v.Stale = p.now().Sub(v.ValidAt) > p.StaleAfter
		}
	}
	return v, nil
}

// SymbolHealth reads oracle:health:{sym} — "" means never published.
func (p *Provider) SymbolHealth(ctx context.Context, symbol string) (HealthState, error) {
	v, err := p.C.Get(ctx, p.HealthKeyFn(symbol)).Result()
	if err == goredis.Nil {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return HealthState(v), nil
}

// GateOrderAdmission is the orders-side check (Task 19.5.3.7): a symbol
// whose oracle health is UNAVAILABLE fails closed — position-increasing
// orders must be rejected with PRICE_ORACLE_UNAVAILABLE. Empty health
// (oracle service down/never published) is ALSO unavailable — a silent
// oracle must not silently pass.
func (p *Provider) GateOrderAdmission(ctx context.Context, symbol string) error {
	h, err := p.SymbolHealth(ctx, symbol)
	if err != nil {
		return fmt.Errorf("oracle health read %s: %w", symbol, err)
	}
	if h != HealthOK && h != HealthDegraded {
		return ErrOracleUnavailable
	}
	return nil
}

// MarkPriceProvider impl — satisfies risk.MarkPriceProvider's contract
// (GetMarkPrice / GetMarkPriceWithProvenance shapes) through the
// Adapter below so the oracle package never imports its consumer.

// MarkReader is the narrow seam risk.Provider needs — implemented by
// *Provider. Defined interface-side so risk stays the contract owner.
type MarkReader interface {
	Mark(ctx context.Context, symbol string) (MarkView, error)
}
