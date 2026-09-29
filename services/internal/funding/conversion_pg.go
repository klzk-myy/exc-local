// Phase-11 Task 11.3.9 — PostgreSQL persistence for
// funding_currency_conversions (migration 198). Distinct from
// currency_conversions (migration 110) — the realized-P&L sweep audit
// table.
package funding

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"

	"exchange/pkg/decimal"
)

// PgConversionStore implements ConversionStore over the shared pgx pool.
type PgConversionStore struct {
	pool *pgxpool.Pool
}

// NewPgConversionStore wires the store to the gateway's pool.
func NewPgConversionStore(pool *pgxpool.Pool) *PgConversionStore {
	return &PgConversionStore{pool: pool}
}

const conversionColumns = `id, account_id, direction, from_currency, to_currency,
	amount_from::text, mid_rate::text, spread_bps::text, rate_applied::text,
	amount_to::text, rate_source, rate_valid_at, funding_transaction_id, created_at`

// scanConversion maps one row. Numeric columns arrive as ::text (house
// convention — the external pgtype-decimal shim is avoided); parse them.
func scanConversion(sc interface {
	Scan(...any) error
}) (*ConversionRecord, error) {
	var r ConversionRecord
	var amtFrom, mid, spread, applied, amtTo string
	if err := sc.Scan(&r.ID, &r.AccountID, &r.Direction, &r.FromCurrency,
		&r.ToCurrency, &amtFrom, &mid, &spread, &applied, &amtTo,
		&r.RateSource, &r.RateValidAt, &r.FundingTxID, &r.CreatedAt); err != nil {
		return nil, err
	}
	r.AmountFrom = decimal.RequireFromString(amtFrom)
	r.MidRate = decimal.RequireFromString(mid)
	r.SpreadBps = decimal.RequireFromString(spread)
	r.RateApplied = decimal.RequireFromString(applied)
	r.AmountTo = decimal.RequireFromString(amtTo)
	return &r, nil
}

// InsertConversion persists the indicative conversion record.
func (s *PgConversionStore) InsertConversion(ctx context.Context,
	rec ConversionRecord) (*ConversionRecord, error) {
	row := s.pool.QueryRow(ctx, `
		INSERT INTO funding_currency_conversions
		    (account_id, direction, from_currency, to_currency, amount_from,
		     mid_rate, spread_bps, rate_applied, amount_to,
		     rate_source, rate_valid_at, funding_transaction_id)
		VALUES ($1, $2, $3, $4, $5::numeric, $6::numeric, $7::numeric,
		        $8::numeric, $9::numeric, $10, $11, $12)
		RETURNING `+conversionColumns,
		rec.AccountID, rec.Direction, rec.FromCurrency, rec.ToCurrency,
		rec.AmountFrom.String(), rec.MidRate.String(), rec.SpreadBps.String(),
		rec.RateApplied.String(), rec.AmountTo.String(),
		rec.RateSource, rec.RateValidAt, rec.FundingTxID)
	out, err := scanConversion(row)
	if err != nil {
		return nil, wrapCode("INTERNAL_ERROR", "conversion insert", err)
	}
	return out, nil
}

// ConversionHistory returns the account's conversion records, newest
// first, capped at limit (default 100, max 500).
func (s *PgConversionStore) ConversionHistory(ctx context.Context, accountID int64,
	limit int) ([]ConversionRecord, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	rows, err := s.pool.Query(ctx, `
		SELECT `+conversionColumns+`
		FROM funding_currency_conversions
		WHERE account_id = $1
		ORDER BY created_at DESC, id DESC LIMIT $2`, accountID, limit)
	if err != nil {
		return nil, wrapCode("INTERNAL_ERROR", "conversion history", err)
	}
	defer rows.Close()
	out := []ConversionRecord{}
	for rows.Next() {
		r, err := scanConversion(rows)
		if err != nil {
			return nil, wrapCode("INTERNAL_ERROR", "conversion scan", err)
		}
		out = append(out, *r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("conversion history cursor: %w", err)
	}
	return out, nil
}
