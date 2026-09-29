// Phase-11 Task 11.3.9 — PostgreSQL implementation of the funding fee
// schedule seams (migration 198: funding_fee_tiers + free usage).
//
// All mutations open a SERIALIZABLE transaction and write the
// admin_audit_log row + §5.8 audit_hash_chain link via admin.Log inside
// it — record and audit anchor commit or fail together.
package funding

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"exchange/internal/admin"
	"exchange/pkg/decimal"
)

// PgFeeScheduleStore implements FeeScheduleStore + FeeScheduleAdminStore
// over the shared pgx pool.
type PgFeeScheduleStore struct {
	pool *pgxpool.Pool
}

// NewPgFeeScheduleStore wires the store to the gateway's pool.
func NewPgFeeScheduleStore(pool *pgxpool.Pool) *PgFeeScheduleStore {
	return &PgFeeScheduleStore{pool: pool}
}

const feeTierColumns = `id, rail::text, currency, direction, account_tier,
	flat_fee::text, percentage_bps::text, min_fee::text, max_fee::text,
	free_tier_monthly_count, effective_date, version, supersedes_id,
	retired_at, retired_by, created_by, created_at`

func scanFeeTier(row pgx.Row) (*FundingFeeTier, error) {
	var t FundingFeeTier
	var flat, bps, minF string
	var maxF *string
	err := row.Scan(&t.ID, &t.Rail, &t.Currency, &t.Direction, &t.AccountTier,
		&flat, &bps, &minF, &maxF,
		&t.FreeTierMonthlyCount, &t.EffectiveDate, &t.Version,
		&t.SupersedesID, &t.RetiredAt, &t.RetiredBy, &t.CreatedBy, &t.CreatedAt)
	if err != nil {
		return nil, err
	}
	t.FlatFee = decimal.RequireFromString(flat)
	t.PercentageBps = decimal.RequireFromString(bps)
	t.MinFee = decimal.RequireFromString(minF)
	if maxF != nil {
		d := decimal.RequireFromString(*maxF)
		t.MaxFee = &d
	}
	return &t, nil
}

// FeeTierAt resolves the live schedule row for
// (rail, currency, direction, accountTier) at time at: concrete rows
// beat '*' wildcards per key, then the newest effective_date wins.
// (nil, nil) when nothing resolves.
func (s *PgFeeScheduleStore) FeeTierAt(ctx context.Context, rail, currency,
	direction, accountTier string, at time.Time) (*FundingFeeTier, error) {
	row := s.pool.QueryRow(ctx, `
		SELECT `+feeTierColumns+`
		FROM funding_fee_tiers
		WHERE rail = $1::bank_method_enum
		  AND direction = $2
		  AND currency IN ($3, '*')
		  AND account_tier IN ($4, '*')
		  AND effective_date <= $5
		  AND retired_at IS NULL
		ORDER BY (currency = $3) DESC, (account_tier = $4) DESC,
		         effective_date DESC, version DESC
		LIMIT 1`, rail, direction, currency, accountTier, at)
	t, err := scanFeeTier(row)
	if err == pgx.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, wrapCode("FUNDING_RATE_ERROR", "fee tier resolve", err)
	}
	return t, nil
}

// FreeUsageCount reads funding_fee_free_usage for the period.
func (s *PgFeeScheduleStore) FreeUsageCount(ctx context.Context, accountID int64,
	direction string, periodMonth time.Time) (int, error) {
	var n int
	err := s.pool.QueryRow(ctx, `
		SELECT used_count FROM funding_fee_free_usage
		WHERE account_id = $1 AND direction = $2 AND period_month = $3`,
		accountID, direction, periodMonth.Format("2006-01-02")).Scan(&n)
	if err == pgx.ErrNoRows {
		return 0, nil
	}
	if err != nil {
		return 0, wrapCode("FUNDING_RATE_ERROR", "free usage read", err)
	}
	return n, nil
}

// RecordFreeUsage consumes one free movement for the period.
func (s *PgFeeScheduleStore) RecordFreeUsage(ctx context.Context, accountID int64,
	direction string, periodMonth time.Time) error {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO funding_fee_free_usage (account_id, direction, period_month, used_count)
		VALUES ($1, $2, $3, 1)
		ON CONFLICT (account_id, direction, period_month)
		DO UPDATE SET used_count = funding_fee_free_usage.used_count + 1,
		              updated_at = now()`,
		accountID, direction, periodMonth.Format("2006-01-02"))
	if err != nil {
		return wrapCode("FUNDING_RATE_ERROR", "free usage increment", err)
	}
	return nil
}

// auditFeeTier writes the admin_audit_log + hash-chain rows inside tx.
func auditFeeTier(ctx context.Context, tx pgx.Tx, actor FeeAdminActor,
	action string, targetID int64, before, after any) error {
	_, _, err := admin.Log(ctx, tx, admin.AuditEntry{
		AdminUserID: actor.UserID,
		Action:      action,
		TargetType:  "funding_fee_tier",
		TargetID:    &targetID,
		BeforeState: before,
		AfterState:  after,
		IPAddress:   actor.ClientIP,
	})
	if err != nil {
		return wrapCode("INTERNAL_ERROR", "admin audit", err)
	}
	return nil
}

// ListFeeTiers returns schedule rows per filter — newest effective first.
func (s *PgFeeScheduleStore) ListFeeTiers(ctx context.Context,
	f FeeTierFilter) ([]FundingFeeTier, error) {
	where := []string{"TRUE"}
	args := []any{}
	n := 0
	add := func(clause string, v any) {
		n++
		where = append(where, fmt.Sprintf(clause, n))
		args = append(args, v)
	}
	if !f.IncludeAll {
		where = append(where, "retired_at IS NULL")
	}
	if f.Rail != "" {
		add("rail = $%d::bank_method_enum", f.Rail)
	}
	if f.Currency != "" {
		add("currency = $%d", f.Currency)
	}
	if f.Direction != "" {
		add("direction = $%d", f.Direction)
	}
	if f.Tier != "" {
		add("account_tier = $%d", f.Tier)
	}
	limit := f.Limit
	if limit <= 0 || limit > 500 {
		limit = 200
	}
	n++
	q := fmt.Sprintf(`
		SELECT %s FROM funding_fee_tiers
		WHERE %s
		ORDER BY rail, currency, direction, account_tier,
		         effective_date DESC, version DESC
		LIMIT $%d`, feeTierColumns, strings.Join(where, " AND "), n)
	args = append(args, limit)

	rows, err := s.pool.Query(ctx, q, args...)
	if err != nil {
		return nil, wrapCode("INTERNAL_ERROR", "fee tier list", err)
	}
	defer rows.Close()
	out := []FundingFeeTier{}
	for rows.Next() {
		t, err := scanFeeTier(rows)
		if err != nil {
			return nil, wrapCode("INTERNAL_ERROR", "fee tier scan", err)
		}
		out = append(out, *t)
	}
	return out, rows.Err()
}

// FeeTierByID reads one schedule version. (nil, nil) for unknown ids.
func (s *PgFeeScheduleStore) FeeTierByID(ctx context.Context, id int64) (*FundingFeeTier, error) {
	t, err := scanFeeTier(s.pool.QueryRow(ctx,
		`SELECT `+feeTierColumns+` FROM funding_fee_tiers WHERE id = $1`, id))
	if err == pgx.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, wrapCode("INTERNAL_ERROR", "fee tier read", err)
	}
	return t, nil
}

// feeTierByIDTx is the in-transaction variant (SELECT … FOR UPDATE for
// version chains).
func feeTierByIDTx(ctx context.Context, tx pgx.Tx, id int64, lock bool) (*FundingFeeTier, error) {
	q := `SELECT ` + feeTierColumns + ` FROM funding_fee_tiers WHERE id = $1`
	if lock {
		q += ` FOR UPDATE`
	}
	t, err := scanFeeTier(tx.QueryRow(ctx, q, id))
	if err == pgx.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, wrapCode("INTERNAL_ERROR", "fee tier read", err)
	}
	return t, nil
}

// FeeTierVersionChain returns every version of the row's schedule group,
// newest version first. Empty slice when id is unknown.
func (s *PgFeeScheduleStore) FeeTierVersionChain(ctx context.Context, id int64) ([]FundingFeeTier, error) {
	base, err := s.FeeTierByID(ctx, id)
	if err != nil {
		return nil, err
	}
	if base == nil {
		return []FundingFeeTier{}, nil
	}
	rows, err := s.pool.Query(ctx, `
		SELECT `+feeTierColumns+`
		FROM funding_fee_tiers
		WHERE rail = $1::bank_method_enum AND currency = $2
		  AND direction = $3 AND account_tier = $4
		ORDER BY version DESC, id DESC`,
		base.Rail, base.Currency, base.Direction, base.AccountTier)
	if err != nil {
		return nil, wrapCode("INTERNAL_ERROR", "fee tier chain", err)
	}
	defer rows.Close()
	out := []FundingFeeTier{}
	for rows.Next() {
		t, err := scanFeeTier(rows)
		if err != nil {
			return nil, wrapCode("INTERNAL_ERROR", "fee tier chain scan", err)
		}
		out = append(out, *t)
	}
	return out, rows.Err()
}

func insertFeeTier(ctx context.Context, tx pgx.Tx, t FundingFeeTier) (*FundingFeeTier, error) {
	var maxF *string
	if t.MaxFee != nil {
		s := t.MaxFee.String()
		maxF = &s
	}
	row := tx.QueryRow(ctx, `
		INSERT INTO funding_fee_tiers
		    (rail, currency, direction, account_tier, flat_fee,
		     percentage_bps, min_fee, max_fee, free_tier_monthly_count,
		     effective_date, version, supersedes_id, created_by)
		VALUES ($1::bank_method_enum, $2, $3, $4, $5::numeric, $6::numeric,
		        $7::numeric, $8::numeric, $9, $10, $11, $12, $13)
		RETURNING `+feeTierColumns,
		t.Rail, t.Currency, t.Direction, t.AccountTier, t.FlatFee.String(),
		t.PercentageBps.String(), t.MinFee.String(), maxF,
		t.FreeTierMonthlyCount, t.EffectiveDate, t.Version, t.SupersedesID,
		t.CreatedBy)
	created, err := scanFeeTier(row)
	if err != nil {
		if isUniqueViolation(err) {
			return nil, errCode("FEE_INVALID_INPUT",
				"a schedule version already exists for this rail/currency/direction/tier at that effective_date")
		}
		return nil, wrapCode("INTERNAL_ERROR", "fee tier insert", err)
	}
	return created, nil
}

// CreateFeeTier inserts version 1 of a schedule group + audit atomically.
func (s *PgFeeScheduleStore) CreateFeeTier(ctx context.Context, t FundingFeeTier,
	actor FeeAdminActor) (*FundingFeeTier, error) {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return nil, wrapCode("INTERNAL_ERROR", "fee tier tx", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	created, err := insertFeeTier(ctx, tx, t)
	if err != nil {
		return nil, err
	}
	if err := auditFeeTier(ctx, tx, actor, "funding_fee_tier.create",
		created.ID, nil, created); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, wrapCode("INTERNAL_ERROR", "fee tier commit", err)
	}
	return created, nil
}

// CreateFeeTierVersion inserts the successor version under the superseded
// row's group + audit atomically.
func (s *PgFeeScheduleStore) CreateFeeTierVersion(ctx context.Context, id int64,
	t FundingFeeTier, actor FeeAdminActor) (*FundingFeeTier, error) {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return nil, wrapCode("INTERNAL_ERROR", "fee tier tx", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	prev, err := feeTierByIDTx(ctx, tx, id, true)
	if err != nil {
		return nil, err
	}
	if prev == nil {
		return nil, errf("FEE_TIER_NOT_FOUND", "funding fee schedule %d not found", id)
	}
	if prev.RetiredAt != nil {
		return nil, errf("FEE_INVALID_INPUT",
			"funding fee schedule %d is retired — version a live row", id)
	}
	created, err := insertFeeTier(ctx, tx, t)
	if err != nil {
		return nil, err
	}
	if err := auditFeeTier(ctx, tx, actor, "funding_fee_tier.update",
		created.ID, prev, created); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, wrapCode("INTERNAL_ERROR", "fee tier commit", err)
	}
	return created, nil
}

// RetireFeeTier sets retired_at/retired_by + audit atomically.
// (nil, nil) for unknown ids; a repeat retire is a coded conflict.
func (s *PgFeeScheduleStore) RetireFeeTier(ctx context.Context, id int64,
	actor FeeAdminActor) (*FundingFeeTier, error) {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return nil, wrapCode("INTERNAL_ERROR", "fee tier tx", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	prev, err := feeTierByIDTx(ctx, tx, id, true)
	if err != nil {
		return nil, err
	}
	if prev == nil {
		return nil, nil
	}
	if prev.RetiredAt != nil {
		return nil, errf("FEE_INVALID_INPUT",
			"funding fee schedule %d already retired", id)
	}
	row := tx.QueryRow(ctx, `
		UPDATE funding_fee_tiers
		SET retired_at = now(), retired_by = $2
		WHERE id = $1
		RETURNING `+feeTierColumns, id, actor.UserID)
	updated, err := scanFeeTier(row)
	if err != nil {
		return nil, wrapCode("INTERNAL_ERROR", "fee tier retire", err)
	}
	if err := auditFeeTier(ctx, tx, actor, "funding_fee_tier.retire",
		id, prev, updated); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, wrapCode("INTERNAL_ERROR", "fee tier commit", err)
	}
	return updated, nil
}
