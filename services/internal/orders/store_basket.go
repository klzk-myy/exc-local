// store_basket.go — migration-283 basket ledger persistence on PgStore.
// The narrowing interface lives in basket.go (basketStore); this file is
// the PG implementation.

package orders

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// InsertBasket persists the op row + leg rows in ONE transaction — a
// basket with partial legs must never exist in the ledger.
func (s *PgStore) InsertBasket(ctx context.Context, b basketRow,
	legs []basketLegRow) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("basket tx begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := tx.Exec(ctx, `
		INSERT INTO baskets (op_id_hi, op_id_lo, account_id, status, code,
		                     leg_count, legs_filled, legs_unwound,
		                     slippage_ticks)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)
		ON CONFLICT (op_id_hi, op_id_lo) DO NOTHING`,
		int64(b.OpHi), int64(b.OpLo), b.AccountID, b.Status, b.Code,
		b.LegCount, b.LegsFilled, b.LegsUnwound, b.SlippageTicks); err != nil {
		return fmt.Errorf("baskets insert: %w", err)
	}
	for _, l := range legs {
		if _, err := tx.Exec(ctx, `
			INSERT INTO basket_legs (op_id_hi, op_id_lo, leg_index, order_id,
			                         instrument_id, shard_id)
			VALUES ($1,$2,$3,$4,$5,$6)
			ON CONFLICT (op_id_hi, op_id_lo, leg_index) DO NOTHING`,
			int64(b.OpHi), int64(b.OpLo), l.LegIndex, l.OrderID,
			l.InstrumentID, l.ShardID); err != nil {
			return fmt.Errorf("basket_legs insert: %w", err)
		}
	}
	return tx.Commit(ctx)
}

// BasketByOp returns the op row plus legs joined to their order status;
// nil row when the op is unknown.
func (s *PgStore) BasketByOp(ctx context.Context, opHi, opLo uint64) (
	*basketRow, []basketLegRow, error) {
	var b basketRow
	err := s.pool.QueryRow(ctx, `
		SELECT op_id_hi, op_id_lo, account_id, status, code, leg_count,
		       legs_filled, legs_unwound, slippage_ticks
		  FROM baskets WHERE op_id_hi=$1 AND op_id_lo=$2`,
		int64(opHi), int64(opLo)).
		Scan(&b.OpHi, &b.OpLo, &b.AccountID, &b.Status, &b.Code,
			&b.LegCount, &b.LegsFilled, &b.LegsUnwound, &b.SlippageTicks)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, err
	}
	rows, err := s.pool.Query(ctx, `
		SELECT l.leg_index, l.order_id, l.instrument_id, l.shard_id,
		       o.status::text, o.filled_qty::text
		  FROM basket_legs l
		  JOIN orders o ON o.id = l.order_id
		 WHERE l.op_id_hi=$1 AND l.op_id_lo=$2
		 ORDER BY l.leg_index`,
		int64(opHi), int64(opLo))
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	var legs []basketLegRow
	for rows.Next() {
		var l basketLegRow
		if err := rows.Scan(&l.LegIndex, &l.OrderID, &l.InstrumentID,
			&l.ShardID, &l.OrderStatus, &l.FilledQty); err != nil {
			return nil, nil, err
		}
		legs = append(legs, l)
	}
	return &b, legs, rows.Err()
}

// UpdateBasketResult stamps the terminal OptResult onto the op row —
// driven by the bridge's BasketResult consumer (the engine's journal is
// authoritative; this is the queryable projection).
func (s *PgStore) UpdateBasketResult(ctx context.Context, opHi, opLo uint64,
	status string, code, legsFilled, legsUnwound int, slippage int64) error {
	_, err := s.pool.Exec(ctx, `
		UPDATE baskets SET status=$3, code=$4, legs_filled=$5,
		       legs_unwound=$6, slippage_ticks=$7, updated_at=now()
		 WHERE op_id_hi=$1 AND op_id_lo=$2`,
		int64(opHi), int64(opLo), status, code, legsFilled, legsUnwound,
		slippage)
	return err
}
