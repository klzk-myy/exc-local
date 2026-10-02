// exec_resolver.go — Phase-3 Task 3 (IMP-PLAN): production
// ExecQuantityResolver + Tag-17 ExecID resolution for 35=J
// AllocationInstructions (Task 18.3.13).
//
// Tag-17 ExecIDs are per-process mints (report.go nextExecID — "EX{n}"),
// never persisted. The ExecRegistry records execID → orderID as reports
// are emitted, so a same-process 35=J referencing Tag 17 resolves to the
// owning order's cumulative fill state. After a restart a referenced
// exec id is unresolvable and the instruction fails closed
// (ALLOCATION_REJECTED leg status) — never a guessed quantity (§2.7).
package fix

import (
	"context"
	"fmt"
	"sync"

	"github.com/jackc/pgx/v5/pgxpool"

	"exchange/pkg/decimal"
)

// execRegistryCap bounds the in-memory exec map — one entry per emitted
// ExecutionReport; far beyond any realistic allocation window.
const execRegistryCap = 1 << 18

// ExecRegistry maps emitted ExecID(17) values to the order they report.
// Register it on the ReportBus tap chain: app.Report().WithTap(reg.Tap()).
type ExecRegistry struct {
	mu     sync.Mutex
	seq    []string         // FIFO eviction order
	orders map[string]int64 // execID → orders.id
}

// NewExecRegistry builds an empty registry.
func NewExecRegistry() *ExecRegistry {
	return &ExecRegistry{orders: map[string]int64{}}
}

// Note records one emitted report (execID → owning order).
func (r *ExecRegistry) Note(execID string, orderID int64) {
	if execID == "" || orderID == 0 {
		return
	}
	r.mu.Lock()
	if len(r.seq) >= execRegistryCap {
		old := r.seq[0]
		r.seq = r.seq[1:]
		delete(r.orders, old)
	}
	if _, dup := r.orders[execID]; !dup {
		r.seq = append(r.seq, execID)
	}
	r.orders[execID] = orderID
	r.mu.Unlock()
}

// OrderOf resolves an exec id to its order id.
func (r *ExecRegistry) OrderOf(execID string) (int64, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	id, ok := r.orders[execID]
	return id, ok
}

// Tap adapts the registry to the ReportBus tap chain — ExecID(17) is
// read off the emitted report body.
func (r *ExecRegistry) Tap() ReportTap {
	return func(ev ReportEvent) {
		if ev.Msg == nil {
			return
		}
		if id, err := ev.Msg.Body.GetString(TagExecID); err == nil {
			r.Note(id, ev.OrderID)
		}
	}
}

// PgExecResolver resolves allocation exec/order references against the
// orders + instruments tables.
type PgExecResolver struct {
	pool *pgxpool.Pool
	reg  *ExecRegistry // nil → Exec() always unresolvable (fail closed)
}

// NewPgExecResolver builds the production resolver. reg is the
// ExecRegistry bound to the app's ReportBus.
func NewPgExecResolver(pool *pgxpool.Pool, reg *ExecRegistry) *PgExecResolver {
	return &PgExecResolver{pool: pool, reg: reg}
}

// OrderCum resolves OrderID(37) to the order's cumulative fill state.
func (r *PgExecResolver) OrderCum(ctx context.Context, orderID string) (ExecRef, error) {
	var id int64
	if _, err := fmt.Sscan(orderID, &id); err != nil || id <= 0 {
		return ExecRef{}, allocErr(CodeAllocationInvalid,
			"OrderID(37) %q is not a venue order id", orderID)
	}
	var ref ExecRef
	var side string
	var avg *decimal.Decimal
	err := r.pool.QueryRow(ctx, `
		SELECT o.filled_qty, o.avg_fill_price, i.symbol,
		       o.side::text, o.account_id
		  FROM orders o JOIN instruments i ON i.id = o.instrument_id
		 WHERE o.id = $1`, id).
		Scan(&ref.Qty, &avg, &ref.Symbol, &side, &ref.AccountID)
	if err != nil {
		return ExecRef{}, allocErr(CodeAllocationInvalid,
			"OrderID(37) %q unresolvable", orderID)
	}
	if avg != nil {
		ref.AvgPx = *avg
	}
	if side == "SELL" {
		ref.Side = '2'
	} else {
		ref.Side = '1'
	}
	if ref.Qty.IsZero() {
		return ExecRef{}, allocErr(CodeAllocationInvalid,
			"order %d has no executed quantity to allocate", id)
	}
	return ref, nil
}

// Exec resolves ExecID(17) through the in-process registry → the owning
// order's cumulative fill state (OrderCum).
func (r *PgExecResolver) Exec(ctx context.Context, execID string) (ExecRef, error) {
	if r.reg == nil {
		return ExecRef{}, allocErr(CodeAllocationInvalid,
			"exec-id resolution unavailable")
	}
	orderID, ok := r.reg.OrderOf(execID)
	if !ok {
		return ExecRef{}, allocErr(CodeAllocationInvalid,
			"ExecID(17) %q unresolvable (per-process mint or expired)", execID)
	}
	return r.OrderCum(ctx, fmt.Sprint(orderID))
}

// compile-time guard — the allocation service's contract.
var _ ExecQuantityResolver = (*PgExecResolver)(nil)
