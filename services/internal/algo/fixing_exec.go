package algo

// Phase-16 Task 16.3.9 — fix-time executor internals: the atomic cross
// settlement and the residual-imbalance evidence rows.

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/jackc/pgx/v5"

	"exchange/internal/ledger"
	"exchange/pkg/decimal"
)

// crossResult reports one executeCross pass: Qty is the executed amount
// at the fix rate (0 = no fill); BuyDone/SellDone tell the walker to
// retire that side (filled, rejected on shortfall, or cancelled by a
// racing path).
type crossResult struct {
	Qty      decimal.Decimal
	BuyDone  bool
	SellDone bool
}

// executeCross settles one buy↔sell cross at the published fix rate —
// atomically inside one SERIALIZABLE transaction:
//
//  1. lock both order rows FOR UPDATE, re-verify open (racing cancels win)
//  2. verify reservation coverage (shortfall → REJECTED + release, no fill)
//  3. post the settlement journal in-tx (transit → client liability)
//  4. update filled_qty/avg_fill_price/status + reservation consumption
//  5. release terminal orders' leftover reservation
//  6. insert the §5.5 trades row + FIXING_FILL audit rows
//
// Idempotency: journal keys are per (fixing, pair); a crashed-and-
// restarted tick re-runs only consume genuine residuals because
// filled_qty persists the truth.
func (s *FixingService) executeCross(ctx context.Context, f *fixingRow,
	buyID, sellID int64) (res crossResult, _ error) {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return res, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	buy, err := s.lockOrder(ctx, tx, buyID)
	if err != nil {
		return res, err
	}
	sell, err := s.lockOrder(ctx, tx, sellID)
	if err != nil {
		return res, err
	}
	var events []ledger.BalanceEvent
	if buy == nil || sell == nil ||
		!fixingOpen(buy.Status) || !fixingOpen(sell.Status) {
		// Racing cancel/reject committed first — nothing to do.
		res.BuyDone = buy == nil || !fixingOpen(buy.Status)
		res.SellDone = sell == nil || !fixingOpen(sell.Status)
		return res, nil
	}

	q := buy.remaining()
	if sr := sell.remaining(); sr.LessThan(q) {
		q = sr
	}
	if !q.IsPositive() {
		res.BuyDone, res.SellDone = true, true
		return res, nil
	}
	qa := q.Mul(f.Rate).Round(8)

	bres, err := reservationOf(buy.AlgoParams)
	if err != nil {
		return res, err
	}
	sres, err := reservationOf(sell.AlgoParams)
	if err != nil {
		return res, err
	}

	// Shortfall handling — honest, never fabricated: an order whose
	// reservation cannot cover its fix-time obligation is rejected and
	// its lock released; the counterparty rolls to the next order in
	// the queue.
	if sres == nil || sres.remaining().LessThan(q) {
		evs, err := s.rejectShortfall(ctx, tx, f, sell, sres, "base reservation exhausted")
		if err != nil {
			return res, err
		}
		events = append(events, evs...)
		res.SellDone = true
		if err := tx.Commit(ctx); err != nil {
			return res, err
		}
		s.dispatch(events)
		return res, nil
	}
	if bres == nil || bres.remaining().LessThan(qa) {
		evs, err := s.rejectShortfall(ctx, tx, f, buy, bres,
			"quote reservation below fix-time obligation")
		if err != nil {
			return res, err
		}
		events = append(events, evs...)
		res.BuyDone = true
		if err := tx.Commit(ctx); err != nil {
			return res, err
		}
		s.dispatch(events)
		return res, nil
	}

	base, quote, err := instrumentCurrencies(ctx, tx, buy.InstrumentID)
	if err != nil {
		return res, err
	}

	// Settlement journal — consumed locked flows out of
	// 2160_CLEARING_TRANSIT; value received lands on
	// 2010_CUSTOMER_LIABILITY (same convention as the rolling-margin
	// fill journal).
	j := ledger.Journal{
		EntryType:      ledger.EntryTradeFill,
		ReferenceID:    f.ID,
		Description:    truncate255(fmt.Sprintf("FIXING %s cross %d<->%d at %s", f.Benchmark, buyID, sellID, f.Rate)),
		PostedBy:       "algo-fixing",
		IdempotencyKey: fmt.Sprintf("fixing-fill:%d:%d-%d", f.ID, buyID, sellID),
		Lines: []ledger.Line{
			ledger.DebitLine(ledger.ClearingTransit(quote), quote, qa,
				"buy-side reservation consumed at fix"),
			ledger.CreditLine(ledger.CustomerLiability(quote), quote, qa,
				"sell-side quote proceeds at fix"),
			ledger.DebitLine(ledger.ClearingTransit(base), base, q,
				"sell-side reservation consumed at fix"),
			ledger.CreditLine(ledger.CustomerLiability(base), base, q,
				"buy-side base received at fix"),
		},
		Effects: []ledger.AccountEffect{
			{AccountID: buy.AccountID, Currency: quote,
				LockedDelta: qa.Neg()},
			{AccountID: buy.AccountID, Currency: base,
				AvailableDelta: q},
			{AccountID: sell.AccountID, Currency: base,
				LockedDelta: q.Neg()},
			{AccountID: sell.AccountID, Currency: quote,
				AvailableDelta: qa},
		},
	}
	jr, err := s.txPost.PostJournal(ctx, tx, j)
	if err != nil {
		return res, err
	}
	events = append(events, jr.Events...)

	// Read-model updates — same shapes as orders.ApplyFill, plus the
	// reservation consumed counter.
	newBuyFilled := buy.FilledQty.Add(q)
	newSellFilled := sell.FilledQty.Add(q)
	buyFilled := newBuyFilled.Cmp(buy.Quantity) >= 0
	sellFilled := newSellFilled.Cmp(sell.Quantity) >= 0
	if err := s.applyFixFill(ctx, tx, buy.ID, q, f.Rate, buyFilled,
		&fixingReservation{Currency: bres.Currency, Amount: bres.Amount,
			Consumed: bres.consumed().Add(qa).Round(8).String()}); err != nil {
		return res, err
	}
	if err := s.applyFixFill(ctx, tx, sell.ID, q, f.Rate, sellFilled,
		&fixingReservation{Currency: sres.Currency, Amount: sres.Amount,
			Consumed: sres.consumed().Add(q).Round(8).String()}); err != nil {
		return res, err
	}

	// Terminal orders release whatever reservation remains — the lock's
	// job is done.
	if buyFilled {
		rem := bres.amount().Sub(bres.consumed().Add(qa).Round(8))
		if rem.IsPositive() {
			jr, err := s.txPost.PostJournal(ctx, tx, releaseJournal(
				buy.ID, buy.AccountID, bres.Currency, rem,
				fmt.Sprintf("fixing-release:%d:fix%d", buy.ID, f.ID),
				fmt.Sprintf("FIXING order %d terminal release %.8s %s", buy.ID, rem, bres.Currency)))
			if err != nil {
				return res, err
			}
			events = append(events, jr.Events...)
		}
	}
	if sellFilled {
		rem := sres.amount().Sub(sres.consumed().Add(q).Round(8))
		if rem.IsPositive() {
			jr, err := s.txPost.PostJournal(ctx, tx, releaseJournal(
				sell.ID, sell.AccountID, sres.Currency, rem,
				fmt.Sprintf("fixing-release:%d:fix%d", sell.ID, f.ID),
				fmt.Sprintf("FIXING order %d terminal release %.8s %s", sell.ID, rem, sres.Currency)))
			if err != nil {
				return res, err
			}
			events = append(events, jr.Events...)
		}
	}

	if err := s.insertFixingTrade(ctx, tx, f, buy, sell, q, base, quote); err != nil {
		return res, err
	}
	if err := s.insertFixAudits(ctx, tx, f, buy, sell, q, qa, quote); err != nil {
		return res, err
	}
	if err := tx.Commit(ctx); err != nil {
		return res, fmt.Errorf("fixing cross commit: %w", err)
	}
	res.Qty = q
	res.BuyDone, res.SellDone = buyFilled, sellFilled
	s.dispatch(events)
	return res, nil
}

func fixingOpen(status string) bool {
	return status == "RESERVED" || status == "PARTIALLY_FILLED"
}

// lockOrder reads one order FOR UPDATE inside the cross tx.
func (s *FixingService) lockOrder(ctx context.Context, tx pgx.Tx, id int64) (*fixingOrder, error) {
	var o fixingOrder
	var qty, filled, status string
	err := tx.QueryRow(ctx, `
		SELECT id, account_id, instrument_id, side::text, quantity::text,
		       filled_qty::text, status::text, algo_params, shard_id
		FROM orders WHERE id = $1 FOR UPDATE`, id).
		Scan(&o.ID, &o.AccountID, &o.InstrumentID, &o.Side,
			&qty, &filled, &status, &o.AlgoParams, &o.ShardID)
	if err != nil {
		if err == pgx.ErrNoRows {
			return nil, nil
		}
		return nil, err
	}
	o.Quantity = decimal.RequireFromString(qty)
	o.FilledQty = decimal.RequireFromString(filled)
	o.Status = status
	return &o, nil
}

// rejectShortfall cancels one order inside the cross tx: status REJECTED,
// reservation released, audit row recording the reason. The counterparty
// is untouched (it re-queues by staying open). Returned events dispatch
// post-commit via the caller.
func (s *FixingService) rejectShortfall(ctx context.Context, tx pgx.Tx, f *fixingRow,
	o *fixingOrder, rsv *fixingReservation, reason string) ([]ledger.BalanceEvent, error) {
	var events []ledger.BalanceEvent
	if rsv != nil {
		if rem := rsv.remaining(); rem.IsPositive() {
			jr, err := s.txPost.PostJournal(ctx, tx, releaseJournal(
				o.ID, o.AccountID, rsv.Currency, rem,
				fmt.Sprintf("fixing-release:%d:fix%d", o.ID, f.ID),
				fmt.Sprintf("FIXING order %d shortfall release %.8s %s", o.ID, rem, rsv.Currency)))
			if err != nil {
				return nil, err
			}
			events = jr.Events
		}
	}
	tag, err := tx.Exec(ctx, `
		UPDATE orders SET status='REJECTED', updated_at=now()
		WHERE id=$1 AND status IN ('RESERVED','PARTIALLY_FILLED')`, o.ID)
	if err != nil {
		return nil, err
	}
	if tag.RowsAffected() == 0 {
		return nil, nil // racing terminal transition won
	}
	if err := s.insertAudit(ctx, tx, o.ID, o.AccountID, "FIXING_SHORTFALL",
		"status", "REJECTED", reason, "algo-fixing"); err != nil {
		return nil, err
	}
	return events, nil
}

// applyFixFill mirrors orders.ApplyFill's read-model update and stamps
// the full reservation record back into algo_params (the || merge is
// top-level only — a partial object would drop currency/amount).
func (s *FixingService) applyFixFill(ctx context.Context, tx pgx.Tx, orderID int64,
	q, price decimal.Decimal, filled bool, rsv *fixingReservation) error {
	newStatus := "PARTIALLY_FILLED"
	if filled {
		newStatus = "FILLED"
	}
	resBlob, _ := json.Marshal(map[string]any{"fixing_reservation": rsv})
	tag, err := tx.Exec(ctx, `
		UPDATE orders SET
		    filled_qty = filled_qty + $2::numeric,
		    avg_fill_price = (
		        (COALESCE(avg_fill_price,0) * filled_qty + $3::numeric * $2::numeric)
		        / NULLIF(filled_qty + $2::numeric, 0)),
		    status = $4::order_status_enum,
		    algo_params = COALESCE(algo_params,'{}'::jsonb) || $5::jsonb,
		    updated_at = now()
		WHERE id = $1 AND status IN ('RESERVED','PARTIALLY_FILLED')`,
		orderID, q.String(), price.String(), newStatus, resBlob)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("order %d closed during fixing cross (race)", orderID)
	}
	return nil
}

// insertFixingTrade writes the §5.5 trades row for one fix cross. Fees
// are recorded as 0 — the commission engine is a separate seam (the fix
// cross is a balanced transfer; fee assessment on fixing executions is
// the documented residual).
func (s *FixingService) insertFixingTrade(ctx context.Context, tx pgx.Tx, f *fixingRow,
	buy, sell *fixingOrder, q decimal.Decimal, base, quote string) error {
	settle := f.ScheduledAt.AddDate(0, 0, 1) // T+1 convention fallback
	if s.valueAt != nil {
		if d, err := s.valueAt(base, quote, f.ScheduledAt); err == nil && !d.IsZero() {
			settle = d
		}
	}
	var shard any
	if buy.ShardID != nil {
		shard = *buy.ShardID
	}
	_, err := tx.Exec(ctx, `
		INSERT INTO trades (instrument_id, buy_order_id, sell_order_id,
		    buyer_account_id, seller_account_id, price, quantity,
		    buyer_fee, seller_fee, settlement_date, shard_id, trade_seq)
		VALUES ($1,$2,$3,$4,$5,$6::numeric,$7::numeric,0,0,$8,$9,$10)`,
		buy.InstrumentID, buy.ID, sell.ID, buy.AccountID, sell.AccountID,
		f.Rate.String(), q.String(), settle, shard, f.ScheduledAt.UnixNano())
	return err
}

func (s *FixingService) insertFixAudits(ctx context.Context, tx pgx.Tx, f *fixingRow,
	buy, sell *fixingOrder, q, qa decimal.Decimal, quote string) error {
	for _, o := range []*fixingOrder{buy, sell} {
		if err := s.insertAudit(ctx, tx, o.ID, o.AccountID, "FIXING_FILL",
			"benchmark_fixing_id", fmt.Sprint(f.ID),
			fmt.Sprintf("qty %s at fix %s (quote %s %s)", q, f.Rate, qa, quote),
			"algo-fixing"); err != nil {
			return err
		}
	}
	return nil
}

// insertAudit writes one order_audit row inside the caller's tx — same
// column shape orders.WriteAudit emits.
func (s *FixingService) insertAudit(ctx context.Context, tx pgx.Tx,
	orderID, accountID int64, operation, field, oldValue, newValue, actor string) error {
	_, err := tx.Exec(ctx, `
		INSERT INTO order_audit (order_id, account_id, operation,
		    field_name, old_value, new_value, modified_by)
		VALUES ($1,$2,$3,NULLIF($4,''),NULLIF($5,''),$6,$7)`,
		orderID, accountID, operation, field, oldValue, newValue, actor)
	return err
}

// markImbalance records the unmatched residual for one order at one
// fixing — once per (order, fixing) via the audit-marker dedup.
func (s *FixingService) markImbalance(ctx context.Context, f *fixingRow, o *fixingOrder) error {
	var exists bool
	err := s.pool.QueryRow(ctx, `
		SELECT EXISTS(
		    SELECT 1 FROM order_audit
		    WHERE order_id=$1 AND operation='FIXING_IMBALANCE'
		      AND field_name='benchmark_fixing_id' AND new_value=$2)`,
		o.ID, fmt.Sprint(f.ID)).Scan(&exists)
	if err != nil {
		return err
	}
	if exists {
		return nil
	}
	_, err = s.pool.Exec(ctx, `
		INSERT INTO order_audit (order_id, account_id, operation,
		    field_name, old_value, new_value, modified_by)
		VALUES ($1,$2,'FIXING_IMBALANCE','benchmark_fixing_id','',$3,'algo-fixing')`,
		o.ID, o.AccountID, fmt.Sprintf("%d residual %s", f.ID, o.remaining()))
	return err
}

// instrumentCurrencies resolves base/quote inside the cross tx.
func instrumentCurrencies(ctx context.Context, tx pgx.Tx, instrumentID int64) (base, quote string, err error) {
	err = tx.QueryRow(ctx, `
		SELECT base_currency, quote_currency FROM instruments WHERE id=$1`,
		instrumentID).Scan(&base, &quote)
	if err != nil {
		return "", "", fmt.Errorf("instrument %d currencies: %w", instrumentID, err)
	}
	return base, quote, nil
}

// dispatch emits the collected BalanceChanged events post-commit —
// same subject contract as the ledger service
// (account.balance.changed.{id}); nil-publisher tolerated.
func (s *FixingService) dispatch(events []ledger.BalanceEvent) {
	if s.pub == nil {
		return
	}
	for _, ev := range events {
		payload, err := json.Marshal(ev)
		if err != nil {
			continue
		}
		_ = s.pub.Publish(context.Background(),
			ledger.BalanceChangedSubject(ev.AccountID), payload)
	}
}

func truncate255(s string) string {
	if len(s) <= 255 {
		return s
	}
	return s[:255]
}
