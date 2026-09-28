// VIP 0–9 tier recalculation engine — Phase-03 Task 3.3.16
// (spec §8.5, §24 #290, migration 086).
//
// A daily 00:00 UTC job recomputes, per master account (spec §8.5: volume
// scope includes all sub-accounts):
//
//  1. 30-day trailing notional trading volume in USD equivalent
//     (trades × instruments.quote_currency, converted via UsdConverter).
//  2. 30-day average equity in USD equivalent (daily equity snapshots in
//     account_equity_snapshots, upserted by this job before averaging).
//
// A family qualifies for the highest tier whose min_30d_volume_usd OR
// min_30d_avg_equity_usd threshold is met. The result is persisted to
// accounts.vip_tier, audited in account_vip_history (one row per run per
// master), inherited by sub-accounts, and synced to Redis
// vip_tier:{account_id} for zero-latency gateway reads.
//
// Fail-closed (spec §2.7): a per-account failure skips that account and
// is collected in RunReport.Errors — the previous tier stands; a missing
// USD conversion rate is an error, never an implicit 1.0.
package settlement

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	excredis "exchange/internal/redis"
	"exchange/pkg/decimal"
	excerrors "exchange/pkg/errors"
)

// CodeVipEngineInternal is a scaffold error code pending canonical
// registration in Phase-05 Task 5.3.21.
const CodeVipEngineInternal = "VIP_ENGINE_INTERNAL"

// VipWindowDays is the spec §8.5 rolling volume/equity window.
const VipWindowDays = 30

// ---------------------------------------------------------------------------
// Qualification matrix — pure logic
// ---------------------------------------------------------------------------

// TierRule is one vip_tier_schedule row.
type TierRule struct {
	Tier         int
	MinVolumeUSD decimal.Decimal // trailing 30d notional, USD equiv
	MinEquityUSD decimal.Decimal // 30d average equity, USD equiv
	MakerBps     decimal.Decimal // negative = rebate (VIP 4+, Task 3.3.17)
	TakerBps     decimal.Decimal
}

// TierFor returns the highest tier (0–9) whose volume OR equity threshold
// is met. An empty or unsorted schedule is still handled correctly;
// a nil schedule yields VIP 0.
func TierFor(schedule []TierRule, volumeUSD, avgEquityUSD decimal.Decimal) int {
	tier := 0
	for _, r := range schedule {
		if r.Tier < 0 || r.Tier > 9 {
			continue // defensive: CHECK constraint already guarantees 0–9
		}
		if volumeUSD.GreaterThanOrEqual(r.MinVolumeUSD) ||
			avgEquityUSD.GreaterThanOrEqual(r.MinEquityUSD) {
			if r.Tier > tier {
				tier = r.Tier
			}
		}
	}
	return tier
}

// RuleFor returns the schedule row for tier; the second return is false
// when the schedule has no such row.
func RuleFor(schedule []TierRule, tier int) (TierRule, bool) {
	for _, r := range schedule {
		if r.Tier == tier {
			return r, true
		}
	}
	return TierRule{}, false
}

// ---------------------------------------------------------------------------
// Engine
// ---------------------------------------------------------------------------

// VipAccount is the engine's view of an accounts row.
type VipAccount struct {
	ID       int64
	ParentID *int64 // nil = master account
	VipTier  int
}

// VipCacheRecord is the vip_tier:{account_id} Redis hash payload.
type VipCacheRecord struct {
	Tier            int
	MakerBps        decimal.Decimal
	TakerBps        decimal.Decimal
	Volume30dUSD    decimal.Decimal
	AvgEquity30dUSD decimal.Decimal
	CalculatedAt    time.Time
}

// VipStore is the persistence seam; PgVipStore implements it over pgx.
type VipStore interface {
	// ActiveAccounts returns every status='ACTIVE' account.
	ActiveAccounts(ctx context.Context) ([]VipAccount, error)
	// LoadSchedule returns the vip_tier_schedule matrix.
	LoadSchedule(ctx context.Context) ([]TierRule, error)
	// FamilyVolumeUSD sums 30-day trailing notional (USD equiv) over the
	// account family (master + sub-accounts).
	FamilyVolumeUSD(ctx context.Context, accountIDs []int64, since time.Time) (decimal.Decimal, error)
	// FamilyEquityUSD sums current balances (USD equiv) over the family.
	FamilyEquityUSD(ctx context.Context, accountIDs []int64) (decimal.Decimal, error)
	// UpsertEquitySnapshot writes today's equity row (idempotent on
	// (account_id, snapshot_date)) — masters only.
	UpsertEquitySnapshot(ctx context.Context, accountID int64, day time.Time, equityUSD decimal.Decimal) error
	// AvgEquityUSD averages snapshots on/after sinceDay; found=false when
	// no snapshot exists in the window.
	AvgEquityUSD(ctx context.Context, accountID int64, sinceDay time.Time) (avg decimal.Decimal, found bool, err error)
	// PersistTier updates accounts.vip_tier and appends the
	// account_vip_history audit row in one transaction.
	PersistTier(ctx context.Context, accountID int64, previousTier, newTier int, volumeUSD, avgEquityUSD decimal.Decimal, calculatedAt time.Time) error
	// SetAccountTier syncs accounts.vip_tier for a sub-account without a
	// history row — the audit trail lives on the master.
	SetAccountTier(ctx context.Context, accountID int64, tier int) error
}

// VipCache syncs the active tier + effective fee schedule to Redis.
type VipCache interface {
	SetVipTier(ctx context.Context, accountID int64, rec VipCacheRecord) error
}

// AccountError records one account's skipped recalculation. The account
// keeps its previous tier (fail-closed: unverified accounts get no new
// tier, in either direction).
type AccountError struct {
	AccountID int64
	Err       error
}

// RunReport summarises one recalculation pass.
type RunReport struct {
	StartedAt  time.Time
	FinishedAt time.Time
	Masters    int // master-account families evaluated
	Members    int // total accounts synced (masters + subs)
	Changed    int // master tier transitions
	Errors     []AccountError
}

// VipEngine runs the daily recalculation.
type VipEngine struct {
	store VipStore
	cache VipCache // may be nil
	clock func() time.Time
}

// NewVipEngine builds the engine. clock may be nil (defaults time.Now) —
// injected for tests.
func NewVipEngine(store VipStore, cache VipCache, clock func() time.Time) *VipEngine {
	if clock == nil {
		clock = time.Now
	}
	return &VipEngine{store: store, cache: cache, clock: clock}
}

// NextUTCMidnight returns the next 00:00 UTC strictly after now.
func NextUTCMidnight(now time.Time) time.Time {
	n := now.UTC()
	return time.Date(n.Year(), n.Month(), n.Day()+1, 0, 0, 0, 0, time.UTC)
}

// RunOnce performs one full recalculation pass. Store-level failures
// (schedule/accounts load) abort with error; per-account failures are
// collected in the report — the remaining accounts still complete.
func (e *VipEngine) RunOnce(ctx context.Context) (*RunReport, error) {
	rep := &RunReport{StartedAt: e.clock().UTC()}
	now := rep.StartedAt
	since := now.AddDate(0, 0, -VipWindowDays)
	sinceDay := time.Date(since.Year(), since.Month(), since.Day(), 0, 0, 0, 0, time.UTC)
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)

	schedule, err := e.store.LoadSchedule(ctx)
	if err != nil {
		return nil, excerrors.Wrap(CodeVipEngineInternal, "load vip_tier_schedule", err)
	}
	accounts, err := e.store.ActiveAccounts(ctx)
	if err != nil {
		return nil, excerrors.Wrap(CodeVipEngineInternal, "load active accounts", err)
	}

	// Group family members under each master (parent_account_id nil or
	// self-referential safety: a parent not in the active set still maps
	// to itself as master via parentID).
	families := make(map[int64][]VipAccount)
	var order []int64 // deterministic master order
	for _, a := range accounts {
		master := a.ID
		if a.ParentID != nil {
			master = *a.ParentID
		}
		if _, seen := families[master]; !seen {
			order = append(order, master)
		}
		families[master] = append(families[master], a)
	}

	for _, masterID := range order {
		if ctx.Err() != nil {
			return rep, ctx.Err()
		}
		members := families[masterID]
		ids := make([]int64, 0, len(members))
		var master *VipAccount
		for i := range members {
			ids = append(ids, members[i].ID)
			if members[i].ID == masterID {
				master = &members[i]
			}
		}
		if master == nil {
			// Master is not ACTIVE (suspended/closed): skip — family
			// members' tiers freeze until the master is active again.
			continue
		}

		changed, err := e.recalcFamily(ctx, master, ids, schedule, now, today, sinceDay)
		if err != nil {
			rep.Errors = append(rep.Errors, AccountError{AccountID: masterID, Err: err})
			continue
		}
		rep.Masters++
		rep.Members += len(members)
		if changed {
			rep.Changed++
		}
	}
	rep.FinishedAt = e.clock().UTC()
	return rep, nil
}

// recalcFamily evaluates one account family and persists + caches the
// result; the bool reports whether the master's tier changed. An error
// skips the family unchanged (fail-closed: previous tier stands).
func (e *VipEngine) recalcFamily(ctx context.Context, master *VipAccount, ids []int64, schedule []TierRule, now, today, sinceDay time.Time) (bool, error) {
	vol, err := e.store.FamilyVolumeUSD(ctx, ids, sinceDay)
	if err != nil {
		return false, fmt.Errorf("family volume: %w", err)
	}
	eq, err := e.store.FamilyEquityUSD(ctx, ids)
	if err != nil {
		return false, fmt.Errorf("family equity: %w", err)
	}
	if err := e.store.UpsertEquitySnapshot(ctx, master.ID, today, eq); err != nil {
		return false, fmt.Errorf("equity snapshot: %w", err)
	}
	avg, found, err := e.store.AvgEquityUSD(ctx, master.ID, sinceDay)
	if err != nil {
		return false, fmt.Errorf("avg equity: %w", err)
	}
	if !found {
		avg = eq // first run: today's snapshot is the whole window
	}

	tier := TierFor(schedule, vol, avg)
	if err := e.store.PersistTier(ctx, master.ID, master.VipTier, tier, vol, avg, now); err != nil {
		return false, fmt.Errorf("persist tier: %w", err)
	}

	rule, _ := RuleFor(schedule, tier)
	rec := VipCacheRecord{
		Tier:            tier,
		MakerBps:        rule.MakerBps,
		TakerBps:        rule.TakerBps,
		Volume30dUSD:    vol,
		AvgEquity30dUSD: avg,
		CalculatedAt:    now,
	}

	// Sync every family member's column + cache entry to the family tier.
	for _, id := range ids {
		if id != master.ID {
			if err := e.store.SetAccountTier(ctx, id, tier); err != nil {
				return false, fmt.Errorf("sync sub-account %d: %w", id, err)
			}
		}
		if e.cache != nil {
			if err := e.cache.SetVipTier(ctx, id, rec); err != nil {
				return false, fmt.Errorf("cache tier account %d: %w", id, err)
			}
		}
	}
	return tier != master.VipTier, nil
}

// RunDaily fires RunOnce at each 00:00 UTC until ctx is cancelled —
// the simple ticker pattern (utils.SignalContext cancels the parent).
// Per-run failures are reported through onErr and do not stop the loop;
// the engine retries at the next midnight.
func (e *VipEngine) RunDaily(ctx context.Context, onErr func(error)) error {
	for {
		next := NextUTCMidnight(e.clock())
		timer := time.NewTimer(time.Until(next))
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
			if _, err := e.RunOnce(ctx); err != nil && onErr != nil {
				onErr(err)
			}
		}
	}
}

// ---------------------------------------------------------------------------
// PgVipStore — PostgreSQL implementation
// ---------------------------------------------------------------------------

// UsdConverter converts a currency amount to USD equivalent.
// Implementations must fail closed: a missing rate is an error, never an
// implicit 1.0 — an unpriced currency must not fabricate VIP volume.
type UsdConverter interface {
	ToUSD(ctx context.Context, currency string, amount decimal.Decimal) (decimal.Decimal, error)
}

// StaticUsdConverter converts via an explicit rate table mapping each
// currency to the USD value of one unit (USD itself is always 1.0).
type StaticUsdConverter struct {
	Rates map[string]decimal.Decimal
}

// ToUSD implements UsdConverter.
func (c StaticUsdConverter) ToUSD(_ context.Context, currency string, amount decimal.Decimal) (decimal.Decimal, error) {
	if currency == "USD" {
		return amount, nil
	}
	r, ok := c.Rates[currency]
	if !ok {
		return decimal.Zero, fmt.Errorf("no USD rate for %s", currency)
	}
	return amount.Mul(r), nil
}

// RedisUsdConverter reads fx:rate:{CCY}USD (USD value of 1 CCY) from the
// coordination Redis — populated by the market-data pipeline.
type RedisUsdConverter struct {
	Rdb *excredis.Client
}

// ToUSD implements UsdConverter.
func (c *RedisUsdConverter) ToUSD(ctx context.Context, currency string, amount decimal.Decimal) (decimal.Decimal, error) {
	if currency == "USD" {
		return amount, nil
	}
	txt, err := c.Rdb.Get(ctx, fmt.Sprintf("fx:rate:%sUSD", currency)).Result()
	if err != nil {
		return decimal.Zero, fmt.Errorf("usd rate %s: %w", currency, err)
	}
	r, err := decimal.NewFromString(txt)
	if err != nil || !r.IsPositive() {
		return decimal.Zero, fmt.Errorf("usd rate %s: invalid %q", currency, txt)
	}
	return amount.Mul(r), nil
}

// PgVipStore implements VipStore over pgx. Numerics cross the wire as
// text (DECIMAL → ::text on read, string → ::numeric on write) to avoid
// the external pgtype-decimal shim.
type PgVipStore struct {
	pool *pgxpool.Pool
	conv UsdConverter
}

// NewPgVipStore wires the store. conv is required — pass
// StaticUsdConverter{Rates:...} or a Redis-backed converter.
func NewPgVipStore(pool *pgxpool.Pool, conv UsdConverter) *PgVipStore {
	return &PgVipStore{pool: pool, conv: conv}
}

// ActiveAccounts returns all ACTIVE accounts.
func (s *PgVipStore) ActiveAccounts(ctx context.Context) ([]VipAccount, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id, parent_account_id, vip_tier
		FROM accounts WHERE status = 'ACTIVE' ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []VipAccount
	for rows.Next() {
		var a VipAccount
		if err := rows.Scan(&a.ID, &a.ParentID, &a.VipTier); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// LoadSchedule reads the vip_tier_schedule matrix.
func (s *PgVipStore) LoadSchedule(ctx context.Context) ([]TierRule, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT vip_tier, min_30d_volume_usd::text, min_30d_avg_equity_usd::text,
		       maker_bps::text, taker_bps::text
		FROM vip_tier_schedule ORDER BY vip_tier`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []TierRule
	for rows.Next() {
		var r TierRule
		var mv, me, mk, tk string
		if err := rows.Scan(&r.Tier, &mv, &me, &mk, &tk); err != nil {
			return nil, err
		}
		var err error
		if r.MinVolumeUSD, err = decimal.NewFromString(mv); err != nil {
			return nil, err
		}
		if r.MinEquityUSD, err = decimal.NewFromString(me); err != nil {
			return nil, err
		}
		if r.MakerBps, err = decimal.NewFromString(mk); err != nil {
			return nil, err
		}
		if r.TakerBps, err = decimal.NewFromString(tk); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// FamilyVolumeUSD sums qty×price per quote currency over the family's
// trades in the window, converts each bucket to USD, and returns the
// total. Volume scope: per master account including sub-accounts (§8.5).
func (s *PgVipStore) FamilyVolumeUSD(ctx context.Context, accountIDs []int64, since time.Time) (decimal.Decimal, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT i.quote_currency, COALESCE(SUM(t.quantity * t.price), 0)::text
		FROM trades t
		JOIN instruments i ON i.id = t.instrument_id
		WHERE t.created_at >= $1
		  AND (t.buyer_account_id = ANY($2) OR t.seller_account_id = ANY($2))
		GROUP BY i.quote_currency`, since, accountIDs)
	if err != nil {
		return decimal.Zero, err
	}
	defer rows.Close()
	total := decimal.Zero
	for rows.Next() {
		var ccy, amt string
		if err := rows.Scan(&ccy, &amt); err != nil {
			return decimal.Zero, err
		}
		d, err := decimal.NewFromString(amt)
		if err != nil {
			return decimal.Zero, fmt.Errorf("parse volume %q: %w", amt, err)
		}
		usd, err := s.conv.ToUSD(ctx, ccy, d)
		if err != nil {
			return decimal.Zero, err
		}
		total = total.Add(usd)
	}
	return total, rows.Err()
}

// FamilyEquityUSD sums balances.total across the family, converted to USD.
func (s *PgVipStore) FamilyEquityUSD(ctx context.Context, accountIDs []int64) (decimal.Decimal, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT currency, COALESCE(SUM(total), 0)::text
		FROM balances WHERE account_id = ANY($1)
		GROUP BY currency`, accountIDs)
	if err != nil {
		return decimal.Zero, err
	}
	defer rows.Close()
	total := decimal.Zero
	for rows.Next() {
		var ccy, amt string
		if err := rows.Scan(&ccy, &amt); err != nil {
			return decimal.Zero, err
		}
		d, err := decimal.NewFromString(amt)
		if err != nil {
			return decimal.Zero, fmt.Errorf("parse balance %q: %w", amt, err)
		}
		usd, err := s.conv.ToUSD(ctx, ccy, d)
		if err != nil {
			return decimal.Zero, err
		}
		total = total.Add(usd)
	}
	return total, rows.Err()
}

// UpsertEquitySnapshot writes today's equity point for the master.
func (s *PgVipStore) UpsertEquitySnapshot(ctx context.Context, accountID int64, day time.Time, equityUSD decimal.Decimal) error {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO account_equity_snapshots (account_id, snapshot_date, equity_usd)
		VALUES ($1, $2, $3::numeric)
		ON CONFLICT (account_id, snapshot_date)
		DO UPDATE SET equity_usd = EXCLUDED.equity_usd`,
		accountID, day, equityUSD.String())
	return err
}

// AvgEquityUSD averages snapshots in the 30-day window.
func (s *PgVipStore) AvgEquityUSD(ctx context.Context, accountID int64, sinceDay time.Time) (decimal.Decimal, bool, error) {
	var txt *string
	err := s.pool.QueryRow(ctx, `
		SELECT AVG(equity_usd)::text FROM account_equity_snapshots
		WHERE account_id = $1 AND snapshot_date >= $2`,
		accountID, sinceDay).Scan(&txt)
	if err != nil {
		return decimal.Zero, false, err
	}
	if txt == nil {
		return decimal.Zero, false, nil
	}
	d, err := decimal.NewFromString(*txt)
	if err != nil {
		return decimal.Zero, false, fmt.Errorf("parse avg equity %q: %w", *txt, err)
	}
	return d, true, nil
}

// PersistTier updates the master's vip_tier and appends the audit row in
// one transaction — tier state and its history can never diverge.
func (s *PgVipStore) PersistTier(ctx context.Context, accountID int64, previousTier, newTier int, volumeUSD, avgEquityUSD decimal.Decimal, calculatedAt time.Time) error {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := tx.Exec(ctx, `
		UPDATE accounts SET vip_tier = $2, updated_at = now() WHERE id = $1`,
		accountID, newTier); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO account_vip_history
		    (account_id, previous_tier, vip_tier, volume_30d_usd,
		     avg_equity_30d_usd, source, calculated_at)
		VALUES ($1, $2, $3, $4::numeric, $5::numeric, 'DAILY_JOB', $6)`,
		accountID, previousTier, newTier, volumeUSD.String(),
		avgEquityUSD.String(), calculatedAt); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// SetAccountTier syncs a sub-account's column to the family tier.
func (s *PgVipStore) SetAccountTier(ctx context.Context, accountID int64, tier int) error {
	_, err := s.pool.Exec(ctx, `
		UPDATE accounts SET vip_tier = $2, updated_at = now() WHERE id = $1`,
		accountID, tier)
	return err
}

// ---------------------------------------------------------------------------
// RedisVipCache — vip_tier:{account_id} hash
// ---------------------------------------------------------------------------

// RedisVipCache writes vip_tier:{account_id} HASH records for zero-latency
// gateway reads. No TTL: the daily job is the authoritative writer.
type RedisVipCache struct {
	rdb *excredis.Client
}

// NewRedisVipCache wraps the coordination Redis client.
func NewRedisVipCache(rdb *excredis.Client) *RedisVipCache {
	return &RedisVipCache{rdb: rdb}
}

// VipCacheKey is the canonical key builder: vip_tier:{account_id}.
func VipCacheKey(accountID int64) string {
	return fmt.Sprintf("vip_tier:%d", accountID)
}

// SetVipTier replaces the account's tier record (DEL+HSET inside MULTI so
// no stale fields survive a schedule change).
func (c *RedisVipCache) SetVipTier(ctx context.Context, accountID int64, rec VipCacheRecord) error {
	key := VipCacheKey(accountID)
	pipe := c.rdb.TxPipeline()
	pipe.Del(ctx, key)
	pipe.HSet(ctx, key, map[string]any{
		"tier":               rec.Tier,
		"maker_bps":          rec.MakerBps.String(),
		"taker_bps":          rec.TakerBps.String(),
		"volume_30d_usd":     rec.Volume30dUSD.String(),
		"avg_equity_30d_usd": rec.AvgEquity30dUSD.String(),
		"calculated_at":      rec.CalculatedAt.UTC().Format(time.RFC3339Nano),
	})
	if _, err := pipe.Exec(ctx); err != nil {
		return fmt.Errorf("redis set %s: %w", key, err)
	}
	return nil
}
