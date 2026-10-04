package main

// fill_recover.go — boot-time settlement gap repair (spec §2.7
// zero-loss). The engine WAL is the authoritative journal of every fill;
// frames lost between journal and settlement commit — unread out-ring
// events stranded by a /dev/shm wipe or ring rebuild — are re-injected
// into the settlement queue as wire-identical TradeFill frames. The
// FillConsumer pipeline (decode → resolve → dedup → commit → republish)
// treats them exactly like live frames, so no parallel settle path is
// built: processed_trades dedup makes overlap with in-flight delivery
// a no-op, and reconciliation's TradesChecker remains the independent
// detector for anything this misses.

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	flatbuffers "github.com/google/flatbuffers/go"
	"github.com/jackc/pgx/v5/pgxpool"

	"exchange/internal/ipc"
	"exchange/internal/recovery"
)

// recoverJournaledFills scans the shard's WAL dirs for TRADE entries
// absent from processed_trades and enqueues each as a fresh
// TradeFill frame. Returns the trade_ids re-injected. Errors are
// returned — the caller logs them; a failed scan must not silently
// pretend the journal had nothing to recover.
func recoverJournaledFills(ctx context.Context, pool *pgxpool.Pool,
	dirs []string, shard uint16, q chan<- []byte, log *slog.Logger) ([]int64, error) {

	var journaled []recovery.JournaledTrade
	for _, dir := range dirs {
		ts, err := recovery.ScanTrades(dir)
		if err != nil {
			return nil, fmt.Errorf("wal trade scan %s: %w", dir, err)
		}
		for _, t := range ts {
			if t.Shard == shard {
				journaled = append(journaled, t)
			}
		}
	}
	if len(journaled) == 0 {
		return nil, nil
	}

	// Diff journaled trade_ids against processed_trades in one query —
	// holes below max(trade_id) count too (an in-flight window dropped
	// mid-shutdown leaves gaps, not just a trailing tail). Fills whose
	// orders no longer exist can never resolve — injecting them would
	// wedge the consumer on TRADE_FILL_UNRESOLVABLE (a poison frame is
	// re-halted every restart), so they are recorded as dead letters
	// instead: reconciliation's TRADES leg downgrades acknowledged dead
	// letters from halt-firing mismatches to a counted INFO summary.
	ids := make([]int64, len(journaled))
	bids := make([]int64, len(journaled))
	sids := make([]int64, len(journaled))
	for i, t := range journaled {
		ids[i] = int64(t.TradeID)
		bids[i] = int64(t.BuyOrderID)
		sids[i] = int64(t.SellOrderID)
	}
	rows, err := pool.Query(ctx, `
		SELECT t.tid,
		       EXISTS (SELECT 1 FROM orders o WHERE o.id = t.bid) AS bex,
		       EXISTS (SELECT 1 FROM orders o WHERE o.id = t.sid) AS sex
		 FROM unnest($1::bigint[], $2::bigint[], $3::bigint[]) AS t(tid, bid, sid)
		 WHERE NOT EXISTS (
		   SELECT 1 FROM processed_trades pt WHERE pt.trade_id = t.tid)`, ids, bids, sids)
	if err != nil {
		return nil, fmt.Errorf("processed_trades diff: %w", err)
	}
	missing := map[uint64]struct{}{}
	var deadIDs []int64
	for rows.Next() {
		var id int64
		var bex, sex bool
		if err := rows.Scan(&id, &bex, &sex); err != nil {
			rows.Close()
			return nil, fmt.Errorf("processed_trades diff row: %w", err)
		}
		if bex && sex {
			missing[uint64(id)] = struct{}{}
		} else {
			deadIDs = append(deadIDs, id)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if nd, derr := recordDeadLetters(ctx, pool, "trade", deadIDs,
		"unresolvable_orders"); derr != nil {
		return nil, fmt.Errorf("record dead-letter fills: %w", derr)
	} else if nd > 0 {
		log.Warn("settlement: journaled fills are permanently unresolvable "+
			"— recorded as dead letters (recon counts, no re-halt)",
			"shard", shard, "fills", nd)
	}
	if len(missing) == 0 {
		return nil, nil
	}

	var injected []int64
	for _, t := range journaled {
		if _, ok := missing[t.TradeID]; !ok {
			continue
		}
		// WAL PriceTicks/QtyUnits are already the 1e8-scaled wire values.
		// The wire engine_seq is not journaled; the per-shard journal seq
		// is the same monotone authority — unique per fill, so republish
		// msgIDs (s{shard}-{seq}) stay collision-free against live seqs.
		b := flatbuffers.NewBuilder(256)
		frame := ipc.EncodeTradeFillEvent(b, t.Seq, t.TsNs, t.TradeID,
			t.BuyOrderID, t.SellOrderID, t.PriceTicks, t.QtyUnits, int64(t.Seq))
		select {
		case q <- frame:
			injected = append(injected, int64(t.TradeID))
		case <-ctx.Done():
			return injected, ctx.Err()
		}
	}
	return injected, nil
}

// waitRecoveredSettlements polls processed_trades until every injected
// trade_id has committed (or the budget expires). Runs during boot
// before the out-ring drain starts, so the only commits possible are
// recovered fills — a shortfall means a poison frame wedged the
// consumer; the caller logs it loudly and recon's TradesChecker flags
// the residual gap.
func waitRecoveredSettlements(ctx context.Context, pool *pgxpool.Pool,
	ids []int64) (int, error) {
	const budget = 3 * time.Minute
	deadline := time.Now().Add(budget)
	for {
		var got int64
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM processed_trades
			WHERE trade_id = ANY($1)`, ids).Scan(&got); err != nil {
			return int(got), err
		}
		if int(got) >= len(ids) || time.Now().After(deadline) {
			return int(got), nil
		}
		select {
		case <-ctx.Done():
			return int(got), ctx.Err()
		case <-time.After(250 * time.Millisecond):
		}
	}
}

// healSettledAheadOrders re-projects orders.filled_qty / avg_fill_price
// / status from the committed tape for every order whose read-model
// fill diverges from the settled legs. A recovered fill commits through
// the settlement queue — its read-model op was stranded with the lost
// frame, so injecting the trade alone would leave orders.filled_qty
// permanently behind; a redelivered op on the other side double-counts
// (applyFillSQL is additive, not idempotent). Incremental op replay is
// unsafe, but a full re-projection from trades is exact — and race-free
// here because this runs before the out-ring drain starts: no live fill
// ops can be in flight. Both directions heal: the committed tape is the
// authority, so any divergence is a read-model defect by definition.
// Terminal non-fill statuses are preserved: a CANCELLED order with late
// fills keeps its CANCELLED status, only the quantity fields converge.
func healSettledAheadOrders(ctx context.Context, pool *pgxpool.Pool) (int64, error) {
	tag, err := pool.Exec(ctx, `
		UPDATE orders o SET
		    filled_qty = s.qty,
		    avg_fill_price = s.vwap,
		    status = CASE
		        WHEN s.qty >= o.quantity THEN 'FILLED'::order_status_enum
		        WHEN o.status IN ('CANCELLED','REJECTED','EXPIRED') THEN o.status
		        ELSE 'PARTIALLY_FILLED'::order_status_enum END,
		    updated_at = now()
		FROM (
		    SELECT order_id, sum(quantity) AS qty,
		           sum(price * quantity) / NULLIF(sum(quantity), 0) AS vwap
		    FROM (
		        SELECT buy_order_id AS order_id, quantity, price FROM trades
		        UNION ALL
		        SELECT sell_order_id, quantity, price FROM trades) legs
		    GROUP BY order_id) s
		WHERE o.id = s.order_id
		  AND s.qty <> o.filled_qty`)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

// healPendingRestedOrders flips orders stuck PENDING/RESERVED whose ids
// the WAL order index reports as live on the engine book: the MarkActive
// op rode a frame that never reached the read model. journaled+resting
// is engine truth, so the status re-projects straight to ACTIVE. Orders
// never journaled (or already terminal in the journal) stay untouched —
// the unjournaled-order replay owns the former, dead-letters the latter.
func healPendingRestedOrders(ctx context.Context, pool *pgxpool.Pool,
	resting map[uint64]bool) (int64, error) {

	if len(resting) == 0 {
		return 0, nil
	}
	ids := make([]int64, 0, len(resting))
	for id := range resting {
		ids = append(ids, int64(id))
	}
	tag, err := pool.Exec(ctx, `
		UPDATE orders SET status='ACTIVE', updated_at=now()
		WHERE status IN ('PENDING','RESERVED') AND id = ANY($1::bigint[])`, ids)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

// recoverDeadOrders dead-letters the two permanently-unresolvable order
// classes the WAL diff exposes:
//
//   - journaled resting order with no PG row: live on the engine's book
//     (WAL replay restores it at engine boot) but any fill it takes is an
//     unresolvable TRADE_FILL poison frame — no path recreates a row.
//   - PG resting order journaled but not resting in the WAL: the engine
//     reached a terminal state (filled-out or cancelled) whose op never
//     survived to the read model — the order rests in PG forever while
//     the engine holds nothing.
//
// Neither can be repaired by re-injection, so they are recorded once and
// the ORDERS reconciliation leg counts them instead of re-halting. The
// caller supplies the WAL order index (shared with the unjournaled-order
// replay so boot scans the journal once).
func recoverDeadOrders(ctx context.Context, pool *pgxpool.Pool,
	journaled, resting map[uint64]bool, log *slog.Logger) (int, error) {

	total := 0
	// Class 1: WAL-resting ids absent from orders.
	if len(resting) > 0 {
		ids := make([]int64, 0, len(resting))
		for id := range resting {
			ids = append(ids, int64(id))
		}
		rows, err := pool.Query(ctx, `
			SELECT t.oid FROM unnest($1::bigint[]) AS t(oid)
			 WHERE NOT EXISTS (SELECT 1 FROM orders o WHERE o.id = t.oid)`, ids)
		if err != nil {
			return total, fmt.Errorf("resting-order diff: %w", err)
		}
		var missing []int64
		for rows.Next() {
			var id int64
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				return total, err
			}
			missing = append(missing, id)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return total, err
		}
		n, err := recordDeadLetters(ctx, pool, "order", missing,
			"missing_projection_row")
		if err != nil {
			return total, err
		}
		total += n
	}
	// Class 2: PG-resting ids the journal already terminated.
	rows, err := pool.Query(ctx,
		`SELECT id FROM orders WHERE status IN
			('PENDING','RESERVED','ACTIVE','PARTIALLY_FILLED')`)
	if err != nil {
		return total, fmt.Errorf("pg resting scan: %w", err)
	}
	var stuck []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return total, err
		}
		if journaled[uint64(id)] && !resting[uint64(id)] {
			stuck = append(stuck, id)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return total, err
	}
	n, err := recordDeadLetters(ctx, pool, "order", stuck,
		"terminal_in_journal")
	if err != nil {
		return total, err
	}
	total += n
	if total > 0 {
		log.Warn("settlement: unresolvable orders recorded as dead letters "+
			"(recon counts, no re-halt)", "orders", total,
			"wal_resting", len(resting), "terminal_in_journal", len(stuck))
	}
	return total, nil
}

// recordDeadLetters bulk-inserts acknowledged permanent gaps, chunked so
// a large WAL diff stays within statement/parameter limits. ON CONFLICT
// keeps repeat boots idempotent — a dead letter once recorded stays
// acknowledged (its projection rows are gone by definition).
func recordDeadLetters(ctx context.Context, pool *pgxpool.Pool,
	entity string, ids []int64, reason string) (int, error) {

	n := 0
	for i := 0; i < len(ids); i += 10000 {
		end := min(i+10000, len(ids))
		tag, err := pool.Exec(ctx, `
			INSERT INTO recon_dead_letters (entity, entity_id, reason)
			SELECT $1, unnest($2::bigint[]), $3
			ON CONFLICT (entity, entity_id) DO NOTHING`,
			entity, ids[i:end], reason)
		if err != nil {
			return n, err
		}
		n += int(tag.RowsAffected())
	}
	return n, nil
}
