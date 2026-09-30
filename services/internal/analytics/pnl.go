// P&L projection store — Task 20.3.4.
//
// ClickHouse `account_pnl` (deploy/clickhouse/schema/005_account_pnl.sql)
// is a ReplacingMergeTree(ver) ordered by (account_id, instrument_id,
// day). Writers Upsert the FULL latest bucket values per
// (account_id, instrument_id, day) — writes are replacements, never
// deltas; a re-write of the same key with a higher `ver` collapses to the
// newest row on merge. Reads use FINAL then sum so they are correct even
// before merges run.
//
// `realized`/`unrealized`/`fees` are denominated in the instrument's
// quote currency (spec §16.4); account base-currency conversion is a
// read-side/projection concern resolved at report time — this store
// keeps the native figures.
//
// All money columns are Decimal(38,8) — every value stays a
// decimal.Decimal end-to-end; no float ever touches a financial figure.
package analytics

import (
	"context"
	"errors"
	"fmt"
	"time"

	"exchange/pkg/decimal"
)

// TableAccountPnL is the ClickHouse projection for per-account P&L.
const TableAccountPnL = "account_pnl"

// PnLRow is one account_pnl bucket: the latest known P&L state for one
// (account, instrument, UTC day). Ver is the ReplacingMergeTree version
// — higher wins; Upsert derives it from Now (unix nanos) so later writes
// always replace earlier ones. Ts is the wall-clock time the snapshot
// was taken (informational; not part of the dedup key).
type PnLRow struct {
	Day          time.Time       // UTC calendar day (clickhouse Date)
	AccountID    int64           // accounts.id
	InstrumentID int64           // instruments.id (0 allowed)
	Symbol       string          // denormalized e.g. "EUR/USD"
	Realized     decimal.Decimal // realized P&L, quote ccy (Decimal(38,8))
	Unrealized   decimal.Decimal // unrealized P&L, quote ccy
	Fees         decimal.Decimal // total fees paid, quote ccy
	Ts           time.Time       // snapshot timestamp (UTC)
	Ver          uint64          // ReplacingMergeTree version
}

// Net returns realized + unrealized - fees for the bucket.
func (r PnLRow) Net() decimal.Decimal {
	return r.Realized.Add(r.Unrealized).Sub(r.Fees)
}

// PnLStore reads and replaces account_pnl buckets. The zero Now defaults
// to time.Now; tests inject a fixed clock to assert exact ver values.
type PnLStore struct {
	ch  Conn
	Now func() time.Time // injectable clock — drives Ts and Ver
}

// NewPnLStore wires the store; a nil conn is legal (every method then
// returns ErrNoClickHouse so callers fail closed instead of panicking).
func NewPnLStore(ch Conn) *PnLStore {
	return &PnLStore{ch: ch, Now: time.Now}
}

// Upsert replaces the bucket for each row's (account_id, instrument_id,
// day) in one batch. Day is truncated to the UTC calendar day; Ver is
// overwritten with Now().UnixNano() — callers supply the VALUES, the
// store owns versioning so two writers can't race a stale version.
// No rows is a no-op.
func (s *PnLStore) Upsert(ctx context.Context, rows ...PnLRow) error {
	if s == nil || s.ch == nil {
		return ErrNoClickHouse
	}
	if len(rows) == 0 {
		return nil
	}
	now := time.Now().UTC()
	if s.Now != nil {
		now = s.Now().UTC()
	}
	ver := uint64(now.UnixNano())
	batch, err := s.ch.PrepareBatch(ctx, "INSERT INTO "+TableAccountPnL+
		" (account_id, instrument_id, symbol, day, realized, unrealized, fees, ts, ver)")
	if err != nil {
		return fmt.Errorf("analytics: pnl prepare batch: %w", err)
	}
	for _, r := range rows {
		ts := r.Ts
		if ts.IsZero() {
			ts = now
		}
		if err := batch.Append(
			r.AccountID,
			r.InstrumentID,
			r.Symbol,
			dayOnlyUTC(r.Day),
			r.Realized,
			r.Unrealized,
			r.Fees,
			ts.UTC(),
			ver,
		); err != nil {
			_ = batch.Abort()
			return fmt.Errorf("analytics: pnl batch append: %w", err)
		}
	}
	if err := batch.Send(); err != nil {
		return fmt.Errorf("analytics: pnl batch send: %w", err)
	}
	return nil
}

// Report returns the per-(instrument, day) P&L rollup for one account
// over the half-open window [from, to). Days are UTC calendar days
// (matching the Date column). Results are ordered by day then symbol.
// FINAL collapses ReplacingMergeTree versions before summing, so
// in-flight rewrites never double-count.
func (s *PnLStore) Report(ctx context.Context, accountID int64,
	from, to time.Time) ([]PnLRow, error) {
	if s == nil || s.ch == nil {
		return nil, ErrNoClickHouse
	}
	if accountID <= 0 {
		return nil, fmt.Errorf("analytics: pnl report needs a positive account id, got %d", accountID)
	}
	const q = `
SELECT account_id, instrument_id, symbol, day,
       sum(realized)   AS realized,
       sum(unrealized) AS unrealized,
       sum(fees)       AS fees,
       max(ts)         AS ts,
       max(ver)        AS ver
FROM ` + TableAccountPnL + ` FINAL
WHERE account_id = ?
  AND day >= toDate(?)
  AND day <  toDate(?)
GROUP BY account_id, instrument_id, symbol, day
ORDER BY day ASC, symbol ASC`
	chRows, err := s.ch.Query(ctx, q, accountID,
		dayOnlyUTC(from), dayOnlyUTC(to))
	if err != nil {
		return nil, fmt.Errorf("analytics: pnl report query: %w", err)
	}
	defer chRows.Close()
	out := make([]PnLRow, 0, 64)
	for chRows.Next() {
		var r PnLRow
		if err := chRows.Scan(&r.AccountID, &r.InstrumentID, &r.Symbol,
			&r.Day, &r.Realized, &r.Unrealized, &r.Fees, &r.Ts, &r.Ver); err != nil {
			return nil, fmt.Errorf("analytics: pnl report scan: %w", err)
		}
		out = append(out, r)
	}
	if err := chRows.Err(); err != nil {
		return nil, fmt.Errorf("analytics: pnl report rows: %w", err)
	}
	return out, nil
}

// ErrNoClickHouse is returned by every store method built on a nil Conn —
// construction is allowed to be lazy (the orchestrator wires the real
// conn when ClickHouse is reachable) but calls fail closed.
var ErrNoClickHouse = errors.New("analytics: clickhouse connection not configured")

// dayOnlyUTC truncates t to the UTC calendar day — the Date column
// semantic. Date inputs from API parameters arrive already UTC; trading
// timestamps are normalized here so a 23:30-UTC fill lands on the same
// day the trade reports under.
func dayOnlyUTC(t time.Time) time.Time {
	u := t.UTC()
	return time.Date(u.Year(), u.Month(), u.Day(), 0, 0, 0, 0, time.UTC)
}
