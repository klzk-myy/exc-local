// margin_level_reader.go — the margin:level:{account_id} read contract
// (Phase-19 cluster boundary: the margin-level engine — Tasks 19.3.1/
// 19.3.16 sibling — WRITES the hash; the liquidation cluster READS it).
//
// Canonical HASH shape (contract consumed here):
//
//	margin:level:{account_id} = {
//	  equity:           DECIMAL string   -- haircut-adjusted equity (USD numeraire)
//	  used_margin:      DECIMAL string   -- Σ required margin, USD normalized
//	  margin_level_pct: DECIMAL string   -- equity/used_margin × 100
//	  status:           NORMAL | MARGIN_CALL | LIQUIDATING
//	  updated_at:       RFC3339 | epoch millis
//	}
//
// Fail-closed (§2.7): a read error propagates; a missing hash returns
// (nil, nil) — no computed level means "no live margin view", which
// callers treat as "cannot evaluate", never "healthy".
package risk

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	goredis "github.com/redis/go-redis/v9"

	excredis "exchange/internal/redis"
	"exchange/pkg/decimal"
)

// MarginLevel is the decoded margin:level:{account_id} hash.
type MarginLevel struct {
	AccountID      int64
	Equity         decimal.Decimal
	UsedMargin     decimal.Decimal
	MarginLevelPct decimal.Decimal // percent; 0 when used_margin = 0
	Status         string          // NORMAL | MARGIN_CALL | LIQUIDATING
	UpdatedAt      time.Time
}

// MarginLevelReader is the seam the liquidation engine consumes.
// Production binds RedisMarginLevelReader; tests substitute fixtures.
type MarginLevelReader interface {
	// MarginLevel returns the live level for accountID, or (nil, nil)
	// when the account has no computed level hash.
	MarginLevel(ctx context.Context, accountID int64) (*MarginLevel, error)
}

// RedisMarginLevelReader reads the canonical margin:level hash.
type RedisMarginLevelReader struct {
	C *excredis.Client
}

// MarginLevel implements MarginLevelReader over Redis HGETALL.
func (r RedisMarginLevelReader) MarginLevel(ctx context.Context, accountID int64) (*MarginLevel, error) {
	if r.C == nil {
		return nil, fmt.Errorf("margin-level reader: nil redis client")
	}
	m, err := r.C.HGetAll(ctx, MarginLevelKey(accountID)).Result()
	if err != nil {
		return nil, fmt.Errorf("margin:level read acct %d: %w", accountID, err)
	}
	if len(m) == 0 {
		return nil, nil
	}
	lv := &MarginLevel{AccountID: accountID, Status: m["status"], UpdatedAt: time.Now().UTC()}
	for field, dst := range map[string]*decimal.Decimal{
		"equity":           &lv.Equity,
		"used_margin":      &lv.UsedMargin,
		"margin_level_pct": &lv.MarginLevelPct,
	} {
		raw, ok := m[field]
		if !ok || raw == "" {
			continue
		}
		d, err := decimal.NewFromString(raw)
		if err != nil {
			return nil, fmt.Errorf("margin:level acct %d field %s unreadable %q: %w",
				accountID, field, raw, err)
		}
		*dst = d
	}
	if raw := m["updated_at"]; raw != "" {
		if ts, err := time.Parse(time.RFC3339Nano, raw); err == nil {
			lv.UpdatedAt = ts
		} else if ms, err := strconv.ParseInt(raw, 10, 64); err == nil {
			lv.UpdatedAt = time.UnixMilli(ms).UTC()
		}
	}
	return lv, nil
}

// MarginLevelWriter is the test/dev twin of the reader contract — the
// cluster-B engine owns production writes; tests and the document-fixture
// path use this to stage hashes.
type MarginLevelWriter interface {
	SetMarginLevel(ctx context.Context, lv MarginLevel) error
}

// SetMarginLevel writes the canonical hash shape (test/helper path).
func (r RedisMarginLevelReader) SetMarginLevel(ctx context.Context, lv MarginLevel) error {
	if r.C == nil {
		return fmt.Errorf("margin-level writer: nil redis client")
	}
	pipe := r.C.TxPipeline()
	pipe.HSet(ctx, MarginLevelKey(lv.AccountID), map[string]any{
		"equity":           lv.Equity.String(),
		"used_margin":      lv.UsedMargin.String(),
		"margin_level_pct": lv.MarginLevelPct.String(),
		"status":           lv.Status,
		"updated_at":       lv.UpdatedAt.UTC().Format(time.RFC3339Nano),
	})
	_, err := pipe.Exec(ctx)
	if err != nil {
		return fmt.Errorf("margin:level write acct %d: %w", lv.AccountID, err)
	}
	return nil
}

// IsMarginCallLevel reports whether the level sits at-or-below the §13.3
// margin-call threshold (canonical 111.1%).
func (l *MarginLevel) IsMarginCallLevel(thresholdPct decimal.Decimal) bool {
	return l != nil && l.UsedMargin.IsPositive() &&
		l.MarginLevelPct.LessThanOrEqual(thresholdPct)
}

// IsStopOutLevel reports whether the level is at-or-below the stop-out
// threshold for the account tier (§13.6d — 50% retail default).
func (l *MarginLevel) IsStopOutLevel(stopOutPct decimal.Decimal) bool {
	return l != nil && l.UsedMargin.IsPositive() &&
		l.MarginLevelPct.LessThanOrEqual(stopOutPct)
}

// isRedisNil is a small wrapper so callers don't import go-redis.
func isRedisNil(err error) bool { return errors.Is(err, goredis.Nil) }
