package algo

// Phase-16 Task 16.3.22 — deterministic parent/child cancellation-race
// reconciliation + conditional-trigger oracle guards (Go half).
//
// The engine's WAL/event order is the authoritative sequencer: a child
// fill committed BEFORE the parent's cancel CAS keeps its fill (first
// match wins, per the OCO_SIBLING_CANCEL_RACE registry semantics); a
// cancel committed first means no later fill may exist — the engine
// rejects it. This package reconciles the Go read model against that
// authoritative truth and flags violations without ever rewriting a
// committed fill (undoing a settled fill is fabrication; ops sees the
// audit trail instead).
//
// Part 2/3 of the task are admission guards:
//   - conditional triggers on MARK_PRICE/INDEX_PRICE require the feed
//     staleness ≤ 5s — an unavailable feed fails closed with
//     CONDITIONAL_TRIGGER_ORACLE_STALE;
//   - pegged orders require a usable BBO at admission — crossed or
//     missing book state rejects with PEGGED_PRICING_UNAVAILABLE.

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"exchange/internal/orders"
	excerrors "exchange/pkg/errors"
)

// TriggerStaleLimit is the spec §24 #317 staleness bound for conditional
// trigger evaluation.
const TriggerStaleLimit = 5 * time.Second

// ChildRace outcome tags — persisted in algo_order_children.detail and
// the child order's audit trail.
const (
	// RaceFillWon — the fill sequenced before the parent cancel; the
	// child is FILLED and contributes to the parent's filled_qty.
	RaceFillWon = "FILL_WON"
	// RaceCancelWon — no fill committed before the cancel; the child
	// ends CANCELLED (or REJECTED/EXPIRED — still a cancel-side win).
	RaceCancelWon = "CANCEL_WON"
	// RaceViolation — authoritative state shows a fill committed AFTER
	// the parent cancel. WAL order says this cannot happen; the audit
	// trail records OCO_SIBLING_CANCEL_RACE and the read model keeps
	// the (committed) fill — reconciling money is never silently undone.
	RaceViolation = "RACE_VIOLATION"
)

// RaceGuard reconciles algo parent/child state after a cancellation —
// bound to the shared pool; all reads are authoritative (orders table,
// never in-flight request results).
type RaceGuard struct {
	pool *pgxpool.Pool
	now  func() time.Time
}

// NewRaceGuard binds the guard. now may be nil (time.Now).
func NewRaceGuard(pool *pgxpool.Pool, now func() time.Time) *RaceGuard {
	if now == nil {
		now = time.Now
	}
	return &RaceGuard{pool: pool, now: now}
}

// ChildRace is the deterministic verdict for one child order.
type ChildRace struct {
	ChildID     int64
	OrderID     int64
	Outcome     string // RaceFillWon | RaceCancelWon | RaceViolation
	OrderStatus string
	ChildSeq    int
	FillAuditAt *time.Time
	Detail      string
}

// ReconcileParent settles every child row of a cancelled parent against
// the authoritative orders table. Idempotent — a second pass converges
// to the same rows (terminal states are fixed points; the audit marker
// dedups repeats).
//
// Returns the per-child verdicts for callers that surface detail (the
// Engine's Cancel path attaches them to child rows).
func (g *RaceGuard) ReconcileParent(ctx context.Context, parentID int64) ([]ChildRace, error) {
	var parentStatus string
	var parentCancelAt *time.Time
	err := g.pool.QueryRow(ctx, `
		SELECT status,
		       CASE WHEN status='CANCELLED' THEN updated_at END
		FROM algo_orders WHERE id=$1`, parentID).
		Scan(&parentStatus, &parentCancelAt)
	if err != nil {
		return nil, fmt.Errorf("raceguard parent %d: %w", parentID, err)
	}
	if parentStatus != "CANCELLED" && parentStatus != "PAUSED" {
		return nil, nil // nothing to reconcile mid-flight
	}

	rows, err := g.pool.Query(ctx, `
		SELECT c.id, c.seq, c.order_id, c.status, c.filled_qty::text,
		       o.status::text, o.filled_qty::text
		FROM algo_order_children c
		LEFT JOIN orders o ON o.id = c.order_id
		WHERE c.algo_order_id=$1
		ORDER BY c.seq`, parentID)
	if err != nil {
		return nil, fmt.Errorf("raceguard children %d: %w", parentID, err)
	}
	defer rows.Close()

	type raw struct {
		childID int64
		seq     int
		orderID *int64
		cStatus string
		cFilled string
		oStatus *string
		oFilled *string
	}
	var list []raw
	for rows.Next() {
		var r raw
		if err := rows.Scan(&r.childID, &r.seq, &r.orderID, &r.cStatus,
			&r.cFilled, &r.oStatus, &r.oFilled); err != nil {
			return nil, err
		}
		list = append(list, r)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	var verdicts []ChildRace
	for _, r := range list {
		v := ChildRace{ChildID: r.childID, ChildSeq: r.seq}
		if r.orderID == nil {
			// Never dispatched — cancel side wins trivially.
			v.Outcome, v.Detail = RaceCancelWon, "never dispatched"
			verdicts = append(verdicts, v)
			_ = g.settleChild(ctx, r.childID, "CANCELLED",
				r.cFilled, "never dispatched — parent "+parentStatus)
			continue
		}
		v.OrderID = *r.orderID
		if r.oStatus == nil {
			v.Outcome, v.Detail = RaceCancelWon, "order row missing"
			verdicts = append(verdicts, v)
			continue
		}
		v.OrderStatus = *r.oStatus
		switch *r.oStatus {
		case "FILLED":
			v.Outcome = RaceFillWon
			v.Detail = "child fill sequenced before parent cancel — kept"
			// Ordering evidence: the fill's audit timestamp vs the
			// parent's terminal transition. A fill audit row that
			// post-dates the cancel is a WAL-order violation — flagged,
			// never undone.
			var fillAt *time.Time
			_ = g.pool.QueryRow(ctx, `
				SELECT MAX(modified_at) FROM order_audit
				WHERE order_id=$1 AND operation IN ('FILL','ENGINE_FILL','FILLED')`,
				r.orderID).Scan(&fillAt)
			v.FillAuditAt = fillAt
			if parentCancelAt != nil && fillAt != nil && fillAt.After(*parentCancelAt) {
				v.Outcome = RaceViolation
				v.Detail = "OCO_SIBLING_CANCEL_RACE: fill audit post-dates parent cancel"
				_ = g.audit(ctx, *r.orderID, "OCO_SIBLING_CANCEL_RACE",
					"race", "", fmt.Sprintf("parent %d cancelled %s; fill audit %s — committed fill kept, ops review",
						parentID, parentCancelAt.UTC().Format(time.RFC3339Nano),
						fillAt.UTC().Format(time.RFC3339Nano)))
			}
			_ = g.settleChild(ctx, r.childID, "FILLED", *r.oFilled, v.Detail)
		case "CANCELLED", "EXPIRED", "REJECTED":
			v.Outcome = RaceCancelWon
			v.Detail = "child terminal " + *r.oStatus
			_ = g.settleChild(ctx, r.childID, "CANCELLED",
				r.cFilled, v.Detail)
		default:
			// Order still live — the cancel request raced a late
			// dispatch; the authoritative row is what matters, mark the
			// child for another cancel pass (caller retries CancelChild).
			v.Outcome = RaceCancelWon
			v.Detail = "child order " + *r.oStatus + " — cancel pending"
			verdicts = append(verdicts, v)
			continue
		}
		verdicts = append(verdicts, v)
	}
	return verdicts, nil
}

// settleChild updates the algo_order_children row deterministically
// (status + resolved_at + detail) — the orders row is never touched
// (the engine owns it).
func (g *RaceGuard) settleChild(ctx context.Context, childID int64,
	status, filled, detail string) error {
	_, err := g.pool.Exec(ctx, `
		UPDATE algo_order_children
		SET status=$2, filled_qty=$3::numeric, detail=$4, resolved_at=now()
		WHERE id=$1 AND status NOT IN ('FILLED','CANCELLED','REJECTED')`,
		childID, status, filled, detail)
	return err
}

// audit writes the race verdict onto the child order's order_audit row
// — deduped per (order, operation) so retries stay idempotent.
func (g *RaceGuard) audit(ctx context.Context, orderID int64,
	operation, field, oldValue, newValue string) error {
	var exists bool
	err := g.pool.QueryRow(ctx, `
		SELECT EXISTS(SELECT 1 FROM order_audit
		              WHERE order_id=$1 AND operation=$2)`,
		orderID, operation).Scan(&exists)
	if err != nil || exists {
		return err
	}
	_, err = g.pool.Exec(ctx, `
		INSERT INTO order_audit (order_id, account_id, operation,
		    field_name, old_value, new_value, modified_by)
		SELECT $1, account_id, $2, $3, NULLIF($4,''), NULLIF($5,''), 'algo-raceguard'
		FROM orders WHERE id=$1`,
		orderID, operation, field, oldValue, newValue)
	return err
}

// ---------------------------------------------------------------------------
// Conditional-trigger oracle gate (Task 16.3.22 parts 2-3)
// ---------------------------------------------------------------------------

// FeedFreshness resolves a feed's last-publication timestamp for one
// instrument — the Phase-19.5 oracle binds this seam for MARK_PRICE /
// INDEX_PRICE. A nil source for a gated source fails closed.
type FeedFreshness func(ctx context.Context, instrumentID int64,
	source string) (*time.Time, error)

// TriggerGuard implements orders.ConditionalGate — admission-side
// staleness and pegged-book guards. The engine re-evaluates the same
// rules at trigger time; this gate keeps unschedulable orders out.
type TriggerGuard struct {
	pool      *pgxpool.Pool
	freshness FeedFreshness // nil → MARK/INDEX feeds unavailable (fail closed)
	top       *PgTopOfBook
	now       func() time.Time
}

// NewTriggerGuard wires the gate. freshness may be nil until the
// Phase-19.5 oracle lands — MARK/INDEX triggers then reject at
// admission with CONDITIONAL_TRIGGER_ORACLE_STALE (fail closed, spec
// §2.7), never silently accepted.
func NewTriggerGuard(pool *pgxpool.Pool, freshness FeedFreshness,
	now func() time.Time) *TriggerGuard {
	if now == nil {
		now = time.Now
	}
	return &TriggerGuard{pool: pool, freshness: freshness,
		top: NewPgTopOfBook(pool), now: now}
}

// AdmitConditional is bound into orders.Service admission for orders
// carrying a trigger_source or a peg_mode — the two gated surfaces.
func (g *TriggerGuard) AdmitConditional(ctx context.Context,
	inst *orders.Instrument, req *orders.SubmitRequest) error {
	// Part 2 — conditional triggers: the trigger source's feed must be
	// fresh at admission; a stale/disconnected feed suspends evaluation
	// rather than risking a false trigger.
	switch req.TriggerSource {
	case "MARK_PRICE", "INDEX_PRICE":
		stale, err := g.stale(ctx, inst.ID, req.TriggerSource)
		if err != nil {
			return fmt.Errorf("trigger freshness %s: %w", req.TriggerSource, err)
		}
		if stale {
			return excerrors.New("CONDITIONAL_TRIGGER_ORACLE_STALE",
				fmt.Sprintf("%s feed on %s stale or unavailable (>5s) — trigger evaluation suspended",
					req.TriggerSource, inst.Symbol))
		}
	case "LAST_PRICE", "":
		// LAST_PRICE is exchange-generated (the trades tape) — it is
		// never oracle-dependent; an empty book simply means no triggers
		// yet, which is legal.
	}
	// Part 3 — pegged orders need a viable book reference at admission.
	if req.PegMode != "" {
		bid, ask, ok, err := g.top.BestBidAsk(ctx, inst.Symbol)
		if err != nil {
			return fmt.Errorf("peg bbo: %w", err)
		}
		if !ok || !bid.IsPositive() || !ask.IsPositive() || bid.Cmp(ask) >= 0 {
			return excerrors.New("PEGGED_PRICING_UNAVAILABLE",
				fmt.Sprintf("no viable BBO for pegged %s on %s — re-pricing source unviable",
					req.PegMode, inst.Symbol))
		}
	}
	return nil
}

// stale reports whether the named feed is stale (>5s) or unavailable.
func (g *TriggerGuard) stale(ctx context.Context, instrumentID int64,
	source string) (bool, error) {
	var last *time.Time
	var err error
	switch source {
	case "LAST_PRICE":
		err = g.pool.QueryRow(ctx, `
			SELECT MAX(created_at) FROM trades WHERE instrument_id=$1`,
			instrumentID).Scan(&last)
	default:
		if g.freshness == nil {
			return true, nil // feed seam unwired → treat as unavailable
		}
		last, err = g.freshness(ctx, instrumentID, source)
	}
	if err != nil {
		return false, err
	}
	if last == nil || g.now().Sub(*last) > TriggerStaleLimit {
		return true, nil
	}
	return false, nil
}
