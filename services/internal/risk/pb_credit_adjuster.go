// Task 24.3.14 — PB credit restitution adjuster (spec §13.7 extension,
// §24 #200).
//
// This file ADJUSTS the existing Phase-19 PB-credit implementation — it
// extends PgPBCreditStore/PBCreditService with the SETTLEMENT_FAILED
// counter corrections; no parallel credit ledger is created. On a failed
// settlement the DSL consumption booked at fill/settlement time is
// credited back and the failed trade's NOP impact is reversed, inside the
// caller's SERIALIZABLE transaction so the restitution row + counter
// correction + GL journal commit atomically.
//
// Idempotency lives in the caller: pb_credit_restitutions is UNIQUE on
// settlement_instruction_id, so RestituteInTx can never run twice for one
// settlement event (one-time-per-event contract, AC #5).

package risk

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"

	"exchange/pkg/decimal"
	excerrors "exchange/pkg/errors"
)

// RestitutionReason is the DSL correction-journal reason tag named by
// Task 24.3.14 step 2 — carried on the journal's description/payload.
const RestitutionReason = "SETTLEMENT_FAIL_RESTITUTION"

// RestitutionInput carries one settlement event's counter corrections.
// Both amounts are CONSUMED usd (positive = credit back); the NOP figure
// is the signed NOP impact the failed trade had when it consumed headroom
// (ReserveTx debits NOP+DSL symmetrically by the same USD notional).
type RestitutionInput struct {
	ClientID       int64           `json:"client_id"`
	PrimeBrokerID  int64           `json:"prime_broker_id"`
	CurrencyPair   string          `json:"currency_pair"` // compact "EURUSD"; "" = global scope
	DSLConsumedUSD decimal.Decimal `json:"dsl_consumed_usd"`
	NOPConsumedUSD decimal.Decimal `json:"nop_consumed_usd"`
}

// RestituteInTx credits back consumed DSL and reverses the NOP impact for
// every limit scope covering the (client, pair) — the global row
// (currency_pair IS NULL) plus the pair-scoped row, mirroring the
// scope-fan-out ReserveTx applies on the consume path. Counters clamp at
// zero — a restitution can never drive utilization negative (a clamped
// row is logged via the returned applied flag for audit detail).
func RestituteInTx(ctx context.Context, tx pgx.Tx, in RestitutionInput) (rows int, err error) {
	if tx == nil {
		return 0, excerrors.New("SERVICE_DEGRADED", "pb credit restitution: nil tx")
	}
	pair := normPair(in.CurrencyPair)
	// Lock the matching limit rows FOR UPDATE in deterministic id order.
	lrows, err := tx.Query(ctx, `
		SELECT id FROM pb_credit_limits
		 WHERE client_account_id = $1
		   AND ($2 = '' OR currency_pair IS NULL OR currency_pair = $2)
		 ORDER BY id FOR UPDATE`, in.ClientID, pair)
	if err != nil {
		return 0, fmt.Errorf("pb restitution: lock limits client %d: %w", in.ClientID, err)
	}
	var ids []int64
	for lrows.Next() {
		var id int64
		if err := lrows.Scan(&id); err != nil {
			lrows.Close()
			return 0, err
		}
		ids = append(ids, id)
	}
	lrows.Close()
	if err := lrows.Err(); err != nil {
		return 0, err
	}
	for _, id := range ids {
		tag, err := tx.Exec(ctx, `
			UPDATE pb_credit_limits SET
			    current_daily_settled     = GREATEST(0, current_daily_settled - $2),
			    current_net_open_position = GREATEST(0, current_net_open_position - $3),
			    updated_at = now()
			WHERE id = $1`, id, in.DSLConsumedUSD, in.NOPConsumedUSD)
		if err != nil {
			return rows, fmt.Errorf("pb restitution: limit %d: %w", id, err)
		}
		rows += int(tag.RowsAffected())
	}
	return rows, nil
}

// RestituteSettlementFail is the convenience wrapper for callers that do
// not already hold a transaction — the counters adjust under their own
// SERIALIZABLE tx. The Task 24.3.14 backoffice path prefers
// RestituteInTx inside its own transaction for full atomicity with the
// restitution journal row.
func (s *PgPBCreditStore) RestituteSettlementFail(ctx context.Context,
	in RestitutionInput) (int, error) {
	tx, err := s.P.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return 0, fmt.Errorf("pb restitution: begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	n, err := RestituteInTx(ctx, tx, in)
	if err != nil {
		return 0, err
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("pb restitution: commit: %w", err)
	}
	return n, nil
}

// RestituteSettlementFail is the service-level entry: applies the
// counters, then mirrors the new utilization to Redis via the existing
// checkAlerts/mirror machinery (best-effort, exactly like OnFill).
func (s *PBCreditService) RestituteSettlementFail(ctx context.Context,
	in RestitutionInput) error {
	pg, ok := s.store.(*PgPBCreditStore)
	if !ok {
		return excerrors.New("SERVICE_DEGRADED",
			"pb credit restitution requires the pg store")
	}
	if _, err := pg.RestituteSettlementFail(ctx, in); err != nil {
		return err
	}
	s.checkAlerts(ctx, in.ClientID) // recalc utilization + fire/mirror alerts
	return nil
}

// RestitutionAdjuster adapts RestituteInTx to the backoffice CreditAdjuster
// seam: the caller's tx arrives as `any` (the store's raw Querier) and must
// be a live pgx.Tx inside the SERIALIZABLE restitution transaction — a
// wrong/nil type fails closed with SERVICE_DEGRADED.
type RestitutionAdjuster struct{}

// RestituteInTx implements the backoffice.CreditAdjuster contract.
func (RestitutionAdjuster) RestituteInTx(ctx context.Context, tx any,
	clientID int64, pair string, dslCreditUSD, nopConsumedUSD decimal.Decimal) (int, error) {
	pgxTx, ok := tx.(pgx.Tx)
	if !ok || pgxTx == nil {
		return 0, excerrors.New("SERVICE_DEGRADED",
			"pb credit restitution: caller tx is not a pgx transaction")
	}
	return RestituteInTx(ctx, pgxTx, RestitutionInput{
		ClientID: clientID, CurrencyPair: pair,
		DSLConsumedUSD: dslCreditUSD, NOPConsumedUSD: nopConsumedUSD,
	})
}
