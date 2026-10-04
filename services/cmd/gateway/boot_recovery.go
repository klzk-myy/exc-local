package main

// boot_recovery.go — the post-consumer boot sweep that heals journal gaps
// before live traffic resumes. Runs inline in run() after the per-shard
// FillConsumers are launched and recoverJournaledFills has re-injected
// stranded WAL frames, strictly before the out-ring drain starts, so the
// heal sees no in-flight read-model ops (spec §2.7 zero-loss ordering).

import (
	"context"
	"log/slog"

	"github.com/jackc/pgx/v5/pgxpool"

	"exchange/internal/config"
	"exchange/internal/orders"
	"exchange/internal/reconciliation"
)

func bootRecoverySweep(sweepCtx context.Context, pool *pgxpool.Pool,
	shardMap *config.ShardMap, orderSvc *orders.Service,
	recoveredTradeIDs []int64, log *slog.Logger) {
	// Wait for injected fills to commit, then re-project the orders read
	// model from the tape. A recovered fill has no read-model op — the
	// frame that carried it was lost — so without this heal every
	// recovered trade leaves orders.filled_qty permanently behind.
	// Strictly before the out-ring drain starts: with no live ops in
	// flight the set-based re-projection cannot double-count a pending
	// applyFill.
	if len(recoveredTradeIDs) > 0 {
		got, werr := waitRecoveredSettlements(sweepCtx, pool, recoveredTradeIDs)
		switch {
		case werr != nil:
			log.Error("settlement: recovery commit wait interrupted — "+
				"residual fills surface via recon TradesChecker",
				"committed", got, "injected", len(recoveredTradeIDs), "err", werr)
		case got < len(recoveredTradeIDs):
			log.Error("settlement: recovered fills still uncommitted after "+
				"wait budget — residual fills surface via recon TradesChecker",
				"committed", got, "injected", len(recoveredTradeIDs))
		}
	}
	if healed, herr := healSettledAheadOrders(sweepCtx, pool); herr != nil {
		log.Error("settlement: read-model heal failed — orders lagging "+
			"committed legs stay stale until next restart", "err", herr)
	} else if healed > 0 {
		log.Warn("settlement: re-projected orders whose committed tape "+
			"outran the read model", "orders", healed)
	}
	// WAL order index — replayed once, shared by the dead-letter
	// scan and the unjournaled-order replay below.
	journaled, walResting, ierr := reconciliation.ReplayOrderIndex(
		sweepCtx, reconciliationWalDirs(shardMap))
	if ierr != nil {
		log.Error("settlement: wal order replay failed — "+
			"dead-letter scan + order replay skipped this boot", "err", ierr)
	} else {
		// Dead-letter resting orders whose PG rows are gone — they
		// remain live on the engine's book but can never resolve
		// downstream, so any fill they take becomes an unresolvable
		// poison frame. Recording them lets recon count the gap once
		// instead of re-halting forever.
		if _, derr := recoverDeadOrders(sweepCtx, pool,
			journaled, walResting, log); derr != nil {
			log.Error("settlement: dead-order scan failed — "+
				"recon keeps halting on unresolvable resting orders", "err", derr)
		}
		// Replay admitted-but-never-journaled orders through the live
		// submit path — PG-resting rows the engine never saw (in-ring
		// loss after REST persist) ride the exact OrderNew wire frame
		// onto their shard's book. Still before the out-ring drain:
		// replayed fills ride the normal pipeline.
		if sent, failed, rerr := orderSvc.RecoverUnjournaledOrders(
			sweepCtx, journaled, log); rerr != nil {
			log.Error("settlement: unjournaled-order replay failed — "+
				"recon keeps halting on pg_vs_wal order gaps", "err", rerr)
		} else if sent > 0 || failed > 0 {
			log.Warn("settlement: unjournaled orders replayed to engine",
				"sent", sent, "failed", failed)
		}
		// Orders journaled+resting but stuck PENDING/RESERVED in PG —
		// the MarkActive op was lost with its frame. Engine truth is
		// the journal, so the status re-projects straight to ACTIVE.
		if healedP, herr := healPendingRestedOrders(sweepCtx, pool,
			walResting); herr != nil {
			log.Error("settlement: pending-order heal failed — "+
				"journaled resting orders stay PENDING until next boot",
				"err", herr)
		} else if healedP > 0 {
			log.Warn("settlement: re-activated orders journaled resting "+
				"but stuck PENDING", "orders", healedP)
		}
	}
}
