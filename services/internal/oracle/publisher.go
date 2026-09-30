package oracle

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"time"

	goredis "github.com/redis/go-redis/v9"

	"exchange/pkg/decimal"
)

// Publisher is the oracle's emission seam — every computed mark/index
// round flows through it. The Redis key contracts are documented in the
// package doc comment; the Aeron publication (spec §26 publisher at
// 224.0.1.1:40456) binds through the Aeron field on RedisPublisher.
type Publisher interface {
	PublishMark(ctx context.Context, r MarkResult) error
	PublishHealth(ctx context.Context, symbol string, state HealthState,
		staleSeconds float64) error
	// PublishFallback fans out the stale-liquidation reference for a
	// symbol (Task 19.5.3.6) — nil clears any prior reference.
	PublishFallback(ctx context.Context, ref *StaleReference) error
}

// Redis key contracts — see package doc.
func MarkKey(sym string) string         { return "mark:" + sym }
func MarkPriceKey(sym string) string    { return "mark_price:" + sym }
func OracleMarkKey(sym string) string   { return "oracle:mark:" + sym }
func OracleMarkTsKey(sym string) string { return "oracle:mark:" + sym + ":ts" }
func IndexPriceKey(sym string) string   { return "index_price:" + sym }
func OracleIndexKey(sym string) string  { return "oracle:index:" + sym }
func OracleIndexTsKey(sym string) string {
	return "oracle:index:" + sym + ":ts"
}
func StalenessKey(sym string) string { return "oracle:staleness:" + sym }
func HealthKey(sym string) string    { return "oracle:health:" + sym }

// HealthSummaryKey carries the service-level JSON health document.
const HealthSummaryKey = "oracle:health"

// RedisPublisher implements Publisher over the coordination Redis.
// Aeron, when non-nil, additionally fans each mark onto the configured
// Aeron channel (spec §26 multicast contract). TickSink, when non-nil,
// archives each published round (ClickHouse oracle_ticks_history tier).
type RedisPublisher struct {
	C *goredis.Client

	// Aeron publishes each mark/index round onto an Aeron channel —
	// optional; nil keeps Redis-only publication (dev/test default).
	Aeron AeronSink

	// Ticks archives each published mark round — ClickHouse
	// oracle_ticks_history in production (Task 19.5.3.1 health/
	// divergence forensics), LogTickSink in dev.
	Ticks TickSink
}

// AeronSink is the minimal publication seam — implemented by
// internal/ipc/aeron's Publication adapter in production wiring.
// Returning an error fails the publish round (logged; Redis keys still
// carry the mark so core consumers keep working off the poller).
type AeronSink interface {
	PublishMarkIndex(ctx context.Context, symbol string, mark, index decimal.Decimal,
		at time.Time) error
}

// TickSink archives one published mark round (ClickHouse
// oracle_ticks_history per §26).
type TickSink interface {
	ArchiveTick(ctx context.Context, r MarkResult) error
}

// scaleInt encodes a decimal as int64 ticks at 1e8 — the C++ contract
// (PriceOracleFeed.hpp: "decimal string" is wrong wording there; the
// parser contract is a plain scaled integer).
func scaleInt(d decimal.Decimal) string {
	return d.Mul(decimal.NewFromInt(100_000_000)).Round(0).String()
}

// PublishMark writes every contracted key atomically (MULTI) then emits
// the pub/sub delta the risk engine consumes on channel mark:{symbol}.
func (p *RedisPublisher) PublishMark(ctx context.Context, r MarkResult) error {
	ts := strconv.FormatInt(r.At.UnixNano(), 10)
	pipe := p.C.TxPipeline()
	pipe.Set(ctx, MarkKey(r.Symbol), r.Mark.String(), 0)
	pipe.Set(ctx, MarkPriceKey(r.Symbol), r.Mark.String(), 0)
	pipe.Set(ctx, OracleMarkKey(r.Symbol), scaleInt(r.Mark), 0)
	pipe.Set(ctx, OracleMarkTsKey(r.Symbol), ts, 0)
	if r.Index.IsPositive() {
		pipe.Set(ctx, IndexPriceKey(r.Symbol), r.Index.String(), 0)
		pipe.Set(ctx, OracleIndexKey(r.Symbol), scaleInt(r.Index), 0)
		pipe.Set(ctx, OracleIndexTsKey(r.Symbol), ts, 0)
	}
	// Delta on the mark channel — the markDeltaMsg shape is owned by
	// risk.PublishMarkDelta; we marshal the identical contract here
	// rather than importing the consumer package (single-writer rule).
	pipe.Publish(ctx, MarkKey(r.Symbol), markDeltaJSON(r))
	if _, err := pipe.Exec(ctx); err != nil {
		return fmt.Errorf("oracle publish %s: %w", r.Symbol, err)
	}
	if p.Aeron != nil {
		if err := p.Aeron.PublishMarkIndex(ctx, r.Symbol, r.Mark, r.Index, r.At); err != nil {
			return fmt.Errorf("oracle aeron %s: %w", r.Symbol, err)
		}
	}
	if p.Ticks != nil {
		if err := p.Ticks.ArchiveTick(ctx, r); err != nil {
			return fmt.Errorf("oracle tick archive %s: %w", r.Symbol, err)
		}
	}
	return nil
}

// markDeltaJSON mirrors risk's markDeltaMsg wire shape.
func markDeltaJSON(r MarkResult) []byte {
	b, _ := json.Marshal(struct {
		Symbol string `json:"symbol"`
		Price  string `json:"price"`
		TsMs   int64  `json:"ts_ms"`
		Source string `json:"source,omitempty"`
	}{r.Symbol, r.Mark.String(), r.At.UnixMilli(), "oracle"})
	return b
}

// PublishHealth records per-symbol health + staleness seconds.
func (p *RedisPublisher) PublishHealth(ctx context.Context, symbol string,
	state HealthState, staleSeconds float64) error {
	pipe := p.C.TxPipeline()
	pipe.Set(ctx, HealthKey(symbol), string(state), 0)
	pipe.Set(ctx, StalenessKey(symbol),
		strconv.FormatFloat(staleSeconds, 'f', 3, 64), 0)
	if _, err := pipe.Exec(ctx); err != nil {
		return fmt.Errorf("oracle health %s: %w", symbol, err)
	}
	return nil
}

// PublishFallback writes/clears the stale-liquidation reference keys.
// A nil ref deletes the fallback keys — recovery restores normal marks.
func (p *RedisPublisher) PublishFallback(ctx context.Context, ref *StaleReference) error {
	if ref == nil {
		return nil // caller deletes per-symbol when it knows the symbol
	}
	pipe := p.C.TxPipeline()
	doc, err := json.Marshal(ref)
	if err != nil {
		return err
	}
	pipe.Set(ctx, FallbackKey(ref.Symbol), doc, 0)
	if ref.State == FallbackFlashCool && !ref.FreezeUntil.IsZero() {
		pipe.Set(ctx, FallbackFreezeKey(ref.Symbol),
			strconv.FormatInt(ref.FreezeUntil.UnixMilli(), 10), 0)
	} else {
		pipe.Del(ctx, FallbackFreezeKey(ref.Symbol))
	}
	if _, err := pipe.Exec(ctx); err != nil {
		return fmt.Errorf("oracle fallback %s: %w", ref.Symbol, err)
	}
	return nil
}

// ClearFallback removes a symbol's stale-reference keys on recovery.
func (p *RedisPublisher) ClearFallback(ctx context.Context, symbol string) error {
	return p.C.Del(ctx, FallbackKey(symbol), FallbackFreezeKey(symbol)).Err()
}

// LogTickSink is the dev/test archive — production wires the ClickHouse
// HTTP writer (services/internal/oracle/store.go).
type LogTickSink struct {
	Write func(ctx context.Context, r MarkResult)
}

// ArchiveTick forwards to the configured writer.
func (l LogTickSink) ArchiveTick(ctx context.Context, r MarkResult) error {
	if l.Write == nil {
		return nil
	}
	l.Write(ctx, r)
	return nil
}
