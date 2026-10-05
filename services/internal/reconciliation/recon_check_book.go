package reconciliation

// check_book.go — ORDERS (category 3) and TRADES (category 4): the
// PostgreSQL read models vs the core's own journal (WAL replay).
//
// Both checkers share one ruling (R2): the WAL is the authoritative
// core-side projection. An incomplete replay (coverage defect) degrades
// the whole leg to INCONCLUSIVE — a partial journal never renders a
// financial verdict. The TRADES leg adds a projection-existence rule:
// the trades table has no production writer yet, so WAL-journaled
// trades against an EMPTY projection is INCONCLUSIVE (writer seam
// absent), while a populated projection is diffed strictly.

import (
	"context"
	"fmt"

	"exchange/pkg/decimal"
)

// OrdersChecker diffs PG resting orders against the WAL-replayed book.
// A resting order journaled but absent from PG (projection lost) or a PG
// ACTIVE order with no journal record (phantom) is a MISMATCH; the halt
// resolves to the affected instrument when resolvable.
type OrdersChecker struct {
	Src  OrderSource
	Wal  walSource
	Acks AckSource // nil → every wal_vs_pg miss halts (pre-ack behavior)
}

func (OrdersChecker) Name() Category { return CatOrders }

func (c OrdersChecker) Run(ctx context.Context, s Scope) ([]Finding, error) {
	if c.Src == nil {
		return []Finding{InconclusiveFinding(CatOrders,
			string(CatOrders), "source", "no order source wired")}, nil
	}
	replay, prob := c.walReplay(ctx, s)
	if prob != nil {
		return []Finding{*prob}, nil
	}
	open, err := c.Src.OpenOrders(ctx)
	if err != nil {
		return nil, err
	}
	syms, err := c.Src.InstrumentSymbols(ctx)
	if err != nil {
		return nil, err
	}

	walOpen := map[int64]RestingOrder{}
	for _, ro := range replay.RestingOrders() {
		walOpen[int64(ro.OrderID)] = ro
	}
	pgOpen := map[int64]OpenOrder{}
	for _, o := range open {
		pgOpen[o.ID] = o
	}

	var out []Finding
	scopeFor := func(instrID int64) (string, string) {
		if sym, ok := syms[instrID]; ok && sym != "" {
			return "INSTRUMENT", sym
		}
		return "GLOBAL", ""
	}

	// Lazily loaded on the first wal_vs_pg miss — a clean run must not
	// pay for the acknowledgment tables.
	var dead map[int64]struct{}
	deadN := 0

	// WAL resting order absent / divergent in PG.
	for id, ro := range walOpen {
		pg, ok := pgOpen[id]
		walRem := decimal.NewFromScaled(ro.RemainingQty)
		if !ok {
			if c.Acks != nil {
				if dead == nil {
					dead, _ = c.Acks.DeadLetters(ctx, "order")
				}
				if _, ack := dead[int64(id)]; ack {
					deadN++
					continue
				}
			}
			sc, tg := scopeFor(int64(ro.InstrumentID))
			out = append(out, AmountFinding(CatOrders, orderSubject(id),
				"wal_vs_pg", walRem, decimal.Zero, UnitQty).
				WithDetail(map[string]any{
					"reason":        "journaled resting order missing from PG read model",
					"instrument_id": ro.InstrumentID,
				}).WithHalt(sc, tg))
			continue
		}
		if !walRem.Equal(pg.RemainingQty) {
			sc, tg := scopeFor(pg.InstrumentID)
			out = append(out, AmountFinding(CatOrders, orderSubject(id),
				"wal_vs_pg", walRem, pg.RemainingQty, UnitQty).
				WithDetail(map[string]any{
					"reason":        "resting quantity divergence",
					"instrument_id": pg.InstrumentID,
					"pg_status":     pg.Status,
				}).WithHalt(sc, tg))
		}
	}
	// PG ACTIVE order with no journal record — phantom resting order.
	// A dead-lettered order here reached a journaled terminal state the
	// read model never observed (lost cancel/fill op); it can never
	// trade again — acknowledged, not a halt.
	for id, o := range pgOpen {
		if _, ok := walOpen[id]; ok {
			continue
		}
		if c.Acks != nil {
			if dead == nil {
				dead, _ = c.Acks.DeadLetters(ctx, "order")
			}
			if _, ack := dead[id]; ack {
				deadN++
				continue
			}
		}
		sc, tg := scopeFor(o.InstrumentID)
		out = append(out, AmountFinding(CatOrders, orderSubject(id),
			"pg_vs_wal", o.RemainingQty, decimal.Zero, UnitQty).
			WithDetail(map[string]any{
				"reason":        "PG resting order has no WAL journal record",
				"instrument_id": o.InstrumentID,
				"pg_status":     o.Status,
			}).WithHalt(sc, tg))
	}
	if deadN > 0 {
		out = append(out, acknowledgedFinding(CatOrders, "dead_letters", deadN))
	}
	return out, nil
}

// acknowledgedFinding reports the count of wal_vs_pg misses absorbed by
// the acknowledgment registry — visible in the run record, never a halt.
func acknowledgedFinding(cat Category, kind string, n int) Finding {
	return Finding{
		Category: cat, Subject: string(cat), Leg: "acknowledged_gap",
		Unit: UnitCount, Severity: SevInfo,
		Detail: map[string]any{kind: n},
	}
}

// walReplay runs the journal replay and converts coverage failures into
// the single INCONCLUSIVE finding — callers return it verbatim.
func (c OrdersChecker) walReplay(ctx context.Context, s Scope) (*walReplay, *Finding) {
	return walReplayOrInconclusive(ctx, s, c.Wal, CatOrders)
}

// TradesChecker diffs the persisted trade tape against journaled TRADE
// events.
type TradesChecker struct {
	Src  TradeSource
	Wal  walSource
	Acks AckSource // nil → every wal_vs_pg miss halts (pre-ack behavior)
}

func (TradesChecker) Name() Category { return CatTrades }

func (c TradesChecker) Run(ctx context.Context, s Scope) ([]Finding, error) {
	if c.Src == nil {
		return []Finding{InconclusiveFinding(CatTrades,
			string(CatTrades), "source", "no trade source wired")}, nil
	}
	replay, prob := walReplayOrInconclusive(ctx, s, c.Wal, CatTrades)
	if prob != nil {
		return []Finding{*prob}, nil
	}
	walTrades := replay.TradeMap()
	pg, err := c.Src.PgTrades(ctx)
	if err != nil {
		return nil, err
	}
	if len(pg) == 0 && len(walTrades) > 0 {
		// Projection-existence rule: an empty tape against a journaled
		// stream means the trades projection writer is absent (no
		// production INSERT path exists as of Phase-13) — verification
		// impossible, not a divergent row.
		minTs, maxTs, entries := replay.Coverage()
		return []Finding{InconclusiveFinding(CatTrades, string(CatTrades),
			"pg_projection",
			fmt.Sprintf("%d journaled trades but the PG trades projection is "+
				"empty — projection writer not landed, leg unverifiable",
				len(walTrades))).WithDetail(map[string]any{
			"wal_trades": len(walTrades), "wal_entries": entries,
			"wal_min_ts_ns": minTs, "wal_max_ts_ns": maxTs,
		})}, nil
	}
	pgByID := map[int64]PgTrade{}
	for _, t := range pg {
		pgByID[t.ID] = t
	}
	var out []Finding
	// Lazily loaded on the first wal_vs_pg miss — a clean run must not
	// pay for the acknowledgment tables.
	var dead, settled map[int64]struct{}
	deadN, settledN := 0, 0
	for id, wt := range walTrades {
		walQty := decimal.NewFromScaled(wt.QtyUnits)
		pgRow, ok := pgByID[int64(id)]
		if !ok {
			if c.Acks != nil {
				if dead == nil {
					dead, _ = c.Acks.DeadLetters(ctx, "trade")
					settled, _ = c.Acks.ProcessedTradeIDs(ctx)
				}
				if _, ack := dead[int64(id)]; ack {
					deadN++
					continue
				}
				// The fill committed its ledger journal (dedup anchor)
				// — the missing tape row is projection damage, not a
				// settlement hole. Reported below as acknowledged; it
				// must not re-halt on what settlement already proved.
				if _, done := settled[int64(id)]; done {
					settledN++
					continue
				}
			}
			out = append(out, AmountFinding(CatTrades, tradeSubject(int64(id)),
				"wal_vs_pg", walQty, decimal.Zero, UnitQty).
				WithDetail(map[string]any{
					"reason":        "journaled trade missing from PG projection",
					"instrument_id": wt.InstrumentID,
				}).WithHalt("GLOBAL", ""))
			continue
		}
		if !walQty.Equal(pgRow.Quantity) {
			out = append(out, AmountFinding(CatTrades, tradeSubject(int64(id)),
				"wal_vs_pg", walQty, pgRow.Quantity, UnitQty).
				WithDetail(map[string]any{
					"reason":        "trade quantity divergence",
					"instrument_id": pgRow.InstrumentID,
				}).WithHalt("GLOBAL", ""))
		}
	}
	for _, t := range pg {
		if _, ok := walTrades[uint64(t.ID)]; ok {
			continue
		}
		out = append(out, AmountFinding(CatTrades, tradeSubject(t.ID),
			"pg_vs_wal", t.Quantity, decimal.Zero, UnitQty).
			WithDetail(map[string]any{
				"reason":        "PG trade has no WAL journal record",
				"instrument_id": t.InstrumentID,
			}).WithHalt("GLOBAL", ""))
	}
	if deadN+settledN > 0 {
		out = append(out, Finding{
			Category: CatTrades, Subject: string(CatTrades), Leg: "acknowledged_gap",
			Unit: UnitCount, Severity: SevInfo,
			Detail: map[string]any{
				"dead_letters":        deadN,
				"settled_unprojected": settledN,
			},
		})
	}
	return out, nil
}

// walReplayOrInconclusive replays s.WalDirs through src and renders any
// coverage failure as one INCONCLUSIVE finding for the category — nil
// dirs, scan errors and integrity problems all land here (never a pass).
func walReplayOrInconclusive(ctx context.Context, s Scope, src walSource,
	cat Category) (*walReplay, *Finding) {
	if src == nil {
		f := InconclusiveFinding(cat, string(cat), "wal_replay",
			"no WAL replay source wired")
		return nil, &f
	}
	if len(s.WalDirs) == 0 {
		f := InconclusiveFinding(cat, string(cat), "wal_replay",
			"no WAL directories configured — core journal unreachable")
		return nil, &f
	}
	replay, err := src.Replay(ctx, s.WalDirs)
	if err != nil {
		f := InconclusiveFinding(cat, string(cat), "wal_replay", err.Error())
		return nil, &f
	}
	if !replay.Complete() {
		f := InconclusiveFinding(cat, string(cat), "wal_integrity",
			"WAL coverage incomplete — partial journal cannot render a verdict").
			WithDetail(map[string]any{"problems": replay.Problems()})
		return nil, &f
	}
	return replay, nil
}
