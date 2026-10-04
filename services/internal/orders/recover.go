package orders

// recover.go — boot-time replay of admitted-but-never-journaled orders
// (spec §2.7 zero-loss). An order row is written before the engine
// frame ships; a frame lost between the REST 202 and the engine's
// journal (in-ring wipe/rebuild) leaves a PG-resting order the core
// never saw — it sits bookable forever without ever trading. The WAL
// order index proves absence (journaled = engine consumed it), so each
// survivor is re-encoded through the exact live submit path
// (orderNewMsg → EncodeOrderNewEvent → Submitter.Send) into its shard's
// in-ring. Idempotent: a replayed order is journaled at engine consume,
// so the next boot's index no longer reports it. Partials replay at
// remaining qty; RESERVED stays untouched (SOR/fixing/auction lifecycles
// park there — none route through the engine book), and PENDING rows
// replay only for engine-bound types (MOO/MOC/FIXING are injector-owned).

import (
	"context"
	"fmt"
	"log/slog"

	flatbuffers "github.com/google/flatbuffers/go"

	"exchange/internal/ipc"
	"exchange/pkg/decimal"
)

// RecoverUnjournaledOrders re-submits every PG open order absent from
// the WAL order index. Call before the out-ring drain starts: replayed
// fills then ride the normal pipeline. Engine-down Send failures are
// counted, not swallowed — the orders stay journaled-absent and
// surface again on the next boot rather than being acknowledged away.
func (s *Service) RecoverUnjournaledOrders(ctx context.Context,
	journaled map[uint64]bool, log *slog.Logger) (sent, failed int, err error) {

	if s.sub == nil {
		return 0, 0, fmt.Errorf("orders: no engine submitter wired")
	}
	open, err := s.store.OpenOrders(ctx, MassCancelScope{})
	if err != nil {
		return 0, 0, err
	}
	ts := uint64(s.now().UnixNano())
	symbols := map[int64]string{}
	for i := range open {
		o := &open[i]
		switch o.Status {
		case "ACTIVE", "PARTIALLY_FILLED":
		case "PENDING":
			// Engine-bound types lost between persist and dispatch —
			// same replay class as ACTIVE. RESERVED stays untouched
			// (SOR-parked / fixing-queued / auction-parked all park
			// there), and PENDING auction/fixing rows are owned by
			// their injectors — they never reach the wire by design.
			if IsAuctionType(o.OrderType) || o.OrderType == TypeFixing {
				continue
			}
		default:
			continue
		}
		if journaled[uint64(o.ID)] {
			continue
		}
		acct, aerr := s.store.AccountByID(ctx, o.AccountID)
		if aerr != nil {
			failed++
			log.Error("order recovery: account lookup failed — "+
				"order stays journaled-absent", "order", o.ID, "err", aerr)
			continue
		}
		symbol, ok := symbols[o.InstrumentID]
		if !ok && o.ShardID == nil {
			inst, ierr := s.store.InstrumentByID(ctx, o.InstrumentID)
			if ierr != nil {
				failed++
				log.Error("order recovery: instrument lookup failed — "+
					"order stays journaled-absent", "order", o.ID, "err", ierr)
				continue
			}
			symbol, symbols[o.InstrumentID] = inst.Symbol, inst.Symbol
		}
		req := &SubmitRequest{GTDExpiry: o.GTDExpiry}
		m := orderNewMsg(o, acct, req)
		if o.Status == "PARTIALLY_FILLED" {
			rem := o.Quantity.Sub(o.FilledQty)
			if !rem.IsPositive() {
				continue // read model will converge via fill replay
			}
			m.Qty = decimal.Scaled(rem)
		}
		b := flatbuffers.NewBuilder(256)
		payload := ipc.EncodeOrderNewEvent(b, s.seq.Next(), ts, m)
		if serr := s.sub.Send(ctx, shardOf(o, s.shards, symbol), payload); serr != nil {
			failed++
			log.Error("order recovery: engine submit failed — "+
				"order stays journaled-absent", "order", o.ID, "err", serr)
			continue
		}
		sent++
	}
	if sent > 0 || failed > 0 {
		log.Warn("order recovery: replayed admitted orders absent from "+
			"the engine journal", "sent", sent, "failed", failed)
	}
	return sent, failed, nil
}
