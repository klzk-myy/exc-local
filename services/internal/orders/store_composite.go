// Phase-16 composite-order persistence (Tasks 16.3.14 bracket/OTO and
// 16.3.20 OPO/OPOCO order lists). CompositeStore is a SEPARATE seam from
// Store so the composite tables stay optional on deployments that have
// not applied migrations 075/225 — *PgStore implements it; the service
// treats a nil composite as "feature unavailable" (fail closed at
// submit, no-op on lifecycle hooks).
package orders

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"exchange/pkg/decimal"
)

// ---------------------------------------------------------------------------
// Bracket types (migration 225)
// ---------------------------------------------------------------------------

// Bracket is one bracket_orders row — the durable group record binding a
// parent entry order to its SL/TP child configuration.
type Bracket struct {
	ID            int64
	AccountID     int64
	InstrumentID  int64
	ParentOrderID int64
	ParentSide    string
	ChildSL       BracketChild
	ChildTP       BracketChild
	State         string
	PlacedQty     decimal.Decimal // cumulative child qty already placed
	ChildSeq      int             // pair counter → derived client ids
	GTDExpiry     *time.Time      // children inherit the parent's GTD
	CreatedAt     time.Time
	UpdatedAt     time.Time
}

// BracketChildRef is one bracket_children placement row — the
// fill-proportional SL/TP pair already dispatched for a bracket.
type BracketChildRef struct {
	ID         int64
	BracketID  int64
	PairIndex  int
	Qty        decimal.Decimal
	SLOrderID  int64
	TPOrderID  int64
	OcoGroupID int64
}

// ---------------------------------------------------------------------------
// Order-list types (migration 075)
// ---------------------------------------------------------------------------

// OrderList is one order_lists row — the OPO/OPOCO list state.
type OrderList struct {
	ID              int64
	AccountID       int64
	InstrumentID    int64
	ContingencyType string // OPO | OPOCO
	State           string
	WorkingOrderID  int64
	LockedProceeds  decimal.Decimal // net base received, locked as proceeds
	NetPendingQty   decimal.Decimal // lot-rounded pending-leg quantity
	ClientOrderID   string
	FailReason      string
	CreatedAt       time.Time
	UpdatedAt       time.Time
	ClosedAt        *time.Time
}

// OrderListLeg is one order_list_legs row. Params is the pending leg's
// SubmitRequest snapshot (JSONB) replayed at activation.
type OrderListLeg struct {
	ID        int64
	ListID    int64
	LegIndex  int
	Role      string // WORKING | PENDING
	OrderID   *int64 // nil until placed
	Params    []byte
	State     string
	CreatedAt time.Time
}

// CompositeStore is the Phase-16 composite persistence seam.
type CompositeStore interface {
	// ---- Brackets (Task 16.3.14) ----
	// InsertOrderBracketTx persists the parent order row AND the
	// bracket_orders group row in one transaction — a group can never
	// exist without its parent nor a parent without its group.
	InsertOrderBracketTx(ctx context.Context, p InsertParams,
		sl, tp BracketChild, gtd *time.Time) (*Order, *Bracket, error)
	BracketByParent(ctx context.Context, parentOrderID int64) (*Bracket, error)
	// OpenBrackets lists non-terminal groups — the recovery sweep set.
	OpenBrackets(ctx context.Context) ([]Bracket, error)
	// PlaceBracketChildrenTx is the atomic fill-cascade: it locks the
	// bracket row, computes delta = parent.filled_qty − placed_qty, and
	// when delta > 0 inserts the proportional SL/TP pair through the
	// SAME insert machinery as InsertOcoPairTx (dedup + oco_group_id)
	// plus a bracket_children ledger row, and bumps placed_qty/child_seq.
	// delta ≤ 0 (replay / race) returns nil,nil,nil — placement is
	// strictly proportional and never double-counts.
	// sl/tp carry the child field templates (side/type/prices/TIF);
	// their Quantity is overridden with the computed delta inside the
	// tx.
	PlaceBracketChildrenTx(ctx context.Context, bracketID, groupID int64,
		slCOID, tpCOID string, sl, tp InsertParams) (*Order, *Order, error)
	// SetBracketState CAS-transitions the group state machine.
	SetBracketState(ctx context.Context, bracketID int64,
		from []string, to string) (bool, error)
	// BracketOpenChildIDs returns the engine-live child order ids of a
	// bracket — the parent-cancel cascade set.
	BracketOpenChildIDs(ctx context.Context, bracketID int64) ([]int64, error)

	// ---- Order lists (Task 16.3.20) ----
	// InsertOrderListTx persists the working order, the order_lists row
	// and every leg row in one transaction. The working leg gets an
	// order_id immediately; pending legs persist their SubmitRequest
	// snapshot with NULL order_id until activation.
	InsertOrderListTx(ctx context.Context, list *OrderList,
		working InsertParams, legs []OrderListLeg) (*OrderList, *Order, error)
	OrderListGet(ctx context.Context, listID int64) (*OrderList, []OrderListLeg, error)
	// OrderListByWorking resolves the list owning a working-order fill.
	OrderListByWorking(ctx context.Context, workingOrderID int64) (*OrderList, []OrderListLeg, error)
	// OrderListByLegOrder resolves the list + legs containing a pending
	// leg's placed order id (cancel cascade).
	OrderListByLegOrder(ctx context.Context, orderID int64) (*OrderList, []OrderListLeg, error)
	// OrderListsPage is the §8.8 keyset-paged open/history query —
	// openOnly=false returns the closed set.
	OrderListsPage(ctx context.Context, accountID int64, openOnly bool,
		cursorAt time.Time, cursorID int64, limit int) ([]OrderList, int64, error)
	// ActivatePendingTx is the atomic net-proceeds activation: locks the
	// EXECUTING list, inserts the pending leg order(s) — OPOCO inserts a
	// shared-oco_group_id pair — stamps leg order_ids + states and flips
	// the list to ALL_DONE with locked_proceeds / net_pending_qty.
	// A non-EXECUTING list returns ok=false (replay-safe).
	ActivatePendingTx(ctx context.Context, listID int64,
		locked, netQty decimal.Decimal, ocoGroupID *int64,
		pending []InsertParams) (orders []*Order, ok bool, err error)
	// SetOrderListState transitions the list (CANCELLED/FAILED/EXPIRED)
	// — CAS on the expected-from set; fail_reason rides along.
	SetOrderListState(ctx context.Context, listID int64,
		from []string, to, failReason string) (bool, error)
	// OpenOrderLists lists EXECUTING lists — the recovery sweep set.
	OpenOrderLists(ctx context.Context) ([]OrderList, error)
	// ListLegSetState updates a leg's order link/state at activation.
	ListLegPlaced(ctx context.Context, legID, orderID int64, state string) error
}

// ---------------------------------------------------------------------------
// PgStore implementation — brackets
// ---------------------------------------------------------------------------

func scanBracket(row pgx.Row) (*Bracket, error) {
	b := &Bracket{}
	var slJSON, tpJSON []byte
	err := row.Scan(&b.ID, &b.AccountID, &b.InstrumentID, &b.ParentOrderID,
		&b.ParentSide, &slJSON, &tpJSON, &b.State, &b.PlacedQty,
		&b.ChildSeq, &b.GTDExpiry, &b.CreatedAt, &b.UpdatedAt)
	if err != nil {
		return nil, err
	}
	if err := decodeBracketChild(slJSON, &b.ChildSL); err != nil {
		return nil, err
	}
	if err := decodeBracketChild(tpJSON, &b.ChildTP); err != nil {
		return nil, err
	}
	return b, nil
}

// InsertOrderBracketTx persists the parent + group atomically (Task
// 16.3.14 item 2): the dedup/orders insert runs through the shared
// insertOrderInTx so §8.7 conflicts surface identically to a plain
// submit; the bracket_orders row commits in the same tx.
func (s *PgStore) InsertOrderBracketTx(ctx context.Context, p InsertParams,
	sl, tp BracketChild, gtd *time.Time) (*Order, *Bracket, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	o, err := insertOrderInTx(ctx, tx, p)
	if err != nil {
		return nil, nil, err
	}
	slJSON, err := encodeBracketChild(sl)
	if err != nil {
		return nil, nil, err
	}
	tpJSON, err := encodeBracketChild(tp)
	if err != nil {
		return nil, nil, err
	}
	var bracketID int64
	err = tx.QueryRow(ctx, `
		INSERT INTO bracket_orders
		    (account_id, instrument_id, parent_order_id, parent_side,
		     child_sl, child_tp, gtd_expiry)
		VALUES ($1,$2,$3,$4,$5::jsonb,$6::jsonb,$7)
		RETURNING id`,
		p.AccountID, p.InstrumentID, o.ID, p.Side, string(slJSON), string(tpJSON), gtd).
		Scan(&bracketID)
	if err != nil {
		return nil, nil, fmt.Errorf("insert bracket group: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, nil, fmt.Errorf("commit bracket submit: %w", err)
	}
	b, err := s.BracketByParent(ctx, o.ID)
	return o, b, err
}

const bracketCols = `
	id, account_id, instrument_id, parent_order_id, parent_side::text,
	child_sl, child_tp, state::text, placed_qty, child_seq,
	gtd_expiry, created_at, updated_at`

func (s *PgStore) BracketByParent(ctx context.Context, parentOrderID int64) (*Bracket, error) {
	b, err := scanBracket(s.pool.QueryRow(ctx,
		`SELECT `+bracketCols+` FROM bracket_orders WHERE parent_order_id=$1`,
		parentOrderID))
	if isNoRows(err) {
		return nil, nil
	}
	return b, err
}

func (s *PgStore) OpenBrackets(ctx context.Context) ([]Bracket, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT `+bracketCols+` FROM bracket_orders
		 WHERE state='WORKING' ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Bracket
	for rows.Next() {
		// QueryRow-style single-row scan per row via Rows.Scan is not
		// possible through scanBracket (pgx.Row vs pgx.Rows); collect
		// manually.
		var b Bracket
		var slJSON, tpJSON []byte
		if err := rows.Scan(&b.ID, &b.AccountID, &b.InstrumentID,
			&b.ParentOrderID, &b.ParentSide, &slJSON, &tpJSON, &b.State,
			&b.PlacedQty, &b.ChildSeq, &b.GTDExpiry,
			&b.CreatedAt, &b.UpdatedAt); err != nil {
			return nil, err
		}
		if err := decodeBracketChild(slJSON, &b.ChildSL); err != nil {
			return nil, err
		}
		if err := decodeBracketChild(tpJSON, &b.ChildTP); err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

// PlaceBracketChildrenTx implements the fill-proportional placement
// inside one locked transaction (Task 16.3.14 items 4/6):
//
//	SELECT ... FOR UPDATE pins the group row → delta = parent.filled_qty
//	− placed_qty → when positive, the SL/TP pair inserts via
//	insertOrderInTx (dedup included) with the shared oco_group_id, the
//	bracket_children ledger row records the pair, and placed_qty/
//	child_seq advance — a redelivered fill event sees placed_qty already
//	covered and places nothing (deterministic replay).
func (s *PgStore) PlaceBracketChildrenTx(ctx context.Context, bracketID, groupID int64,
	slCOID, tpCOID string, sl, tp InsertParams) (*Order, *Order, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var (
		placedQtyStr string
		pairIndex    int
		parentID     int64
		state        string
		filledStr    string
	)
	err = tx.QueryRow(ctx, `
		SELECT b.placed_qty::text, b.child_seq, b.parent_order_id,
		       b.state::text, o.filled_qty::text
		  FROM bracket_orders b
		  JOIN orders o ON o.id = b.parent_order_id
		 WHERE b.id=$1 FOR UPDATE OF b`, bracketID).
		Scan(&placedQtyStr, &pairIndex, &parentID, &state, &filledStr)
	if err != nil {
		return nil, nil, fmt.Errorf("lock bracket %d: %w", bracketID, err)
	}
	if state != BracketWorking {
		return nil, nil, nil // terminal group — nothing to place
	}
	placed := decimal.RequireFromString(placedQtyStr)
	filled := decimal.RequireFromString(filledStr)
	delta := filled.Sub(placed)
	if !delta.IsPositive() {
		return nil, nil, nil // replay — nothing uncovered
	}
	sl.OcoGroupID = &groupID
	tp.OcoGroupID = &groupID
	sl.ClientOrderID = slCOID
	tp.ClientOrderID = tpCOID
	sl.Quantity = delta
	tp.Quantity = delta
	slOrder, err := insertOrderInTx(ctx, tx, sl)
	if err != nil {
		return nil, nil, err
	}
	tpOrder, err := insertOrderInTx(ctx, tx, tp)
	if err != nil {
		return nil, nil, err
	}
	pairIndex++
	if _, err := tx.Exec(ctx, `
		INSERT INTO bracket_children
		    (bracket_id, pair_index, qty, sl_order_id, tp_order_id, oco_group_id)
		VALUES ($1,$2,$3::numeric,$4,$5,$6)`,
		bracketID, pairIndex, delta.String(), slOrder.ID, tpOrder.ID, groupID); err != nil {
		return nil, nil, fmt.Errorf("insert bracket child: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		UPDATE bracket_orders
		   SET placed_qty = placed_qty + $2::numeric,
		       child_seq = $3, updated_at = now()
		 WHERE id = $1`, bracketID, delta.String(), pairIndex); err != nil {
		return nil, nil, fmt.Errorf("bump bracket placed_qty: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, nil, fmt.Errorf("commit bracket children: %w", err)
	}
	return slOrder, tpOrder, nil
}

func (s *PgStore) SetBracketState(ctx context.Context, bracketID int64,
	from []string, to string) (bool, error) {
	tag, err := s.pool.Exec(ctx, `
		UPDATE bracket_orders SET state=$2::bracket_state_enum,
		    updated_at=now()
		 WHERE id=$1 AND state::text = ANY($3)`,
		bracketID, to, from)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() > 0, nil
}

func (s *PgStore) BracketOpenChildIDs(ctx context.Context, bracketID int64) ([]int64, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT c.sl_order_id, c.tp_order_id
		  FROM bracket_children c
		  JOIN orders sl ON sl.id = c.sl_order_id
		  JOIN orders tp ON tp.id = c.tp_order_id
		 WHERE c.bracket_id = $1
		   AND (sl.status = ANY($2) OR tp.status = ANY($2))`,
		bracketID, OpenStatuses)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []int64{}
	for rows.Next() {
		var slID, tpID int64
		if err := rows.Scan(&slID, &tpID); err != nil {
			return nil, err
		}
		out = append(out, slID, tpID)
	}
	return out, rows.Err()
}

// ---------------------------------------------------------------------------
// PgStore implementation — order lists
// ---------------------------------------------------------------------------

func (s *PgStore) InsertOrderListTx(ctx context.Context, list *OrderList,
	working InsertParams, legs []OrderListLeg) (*OrderList, *Order, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	o, err := insertOrderInTx(ctx, tx, working)
	if err != nil {
		return nil, nil, err
	}
	var listID int64
	err = tx.QueryRow(ctx, `
		INSERT INTO order_lists
		    (account_id, instrument_id, contingency_type, working_order_id,
		     client_order_id)
		VALUES ($1,$2,$3::contingency_type_enum,$4,NULLIF($5,''))
		RETURNING id`,
		list.AccountID, list.InstrumentID, list.ContingencyType,
		o.ID, list.ClientOrderID).Scan(&listID)
	if err != nil {
		return nil, nil, fmt.Errorf("insert order list: %w", err)
	}
	for _, leg := range legs {
		var oid any
		if leg.OrderID != nil {
			oid = *leg.OrderID
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO order_list_legs
			    (list_id, leg_index, role, order_id, params, state)
			VALUES ($1,$2,$3,$4,$5::jsonb,$6)`,
			listID, leg.LegIndex, leg.Role, oid, string(leg.Params), leg.State); err != nil {
			return nil, nil, fmt.Errorf("insert list leg %d: %w", leg.LegIndex, err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, nil, fmt.Errorf("commit order list: %w", err)
	}
	l, _, err := s.OrderListGet(ctx, listID)
	return l, o, err
}

const orderListCols = `
	id, account_id, instrument_id, contingency_type::text, state::text,
	working_order_id, locked_proceeds::text, net_pending_qty::text,
	COALESCE(client_order_id,''), COALESCE(fail_reason,''),
	created_at, updated_at, closed_at`

func scanOrderList(row pgx.Row) (*OrderList, error) {
	l := &OrderList{}
	var locked, net string
	err := row.Scan(&l.ID, &l.AccountID, &l.InstrumentID, &l.ContingencyType,
		&l.State, &l.WorkingOrderID, &locked, &net, &l.ClientOrderID,
		&l.FailReason, &l.CreatedAt, &l.UpdatedAt, &l.ClosedAt)
	if err != nil {
		return nil, err
	}
	l.LockedProceeds = decimal.RequireFromString(locked)
	l.NetPendingQty = decimal.RequireFromString(net)
	return l, nil
}

func (s *PgStore) listLegs(ctx context.Context, listID int64) ([]OrderListLeg, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id, list_id, leg_index, role, order_id, params, state, created_at
		  FROM order_list_legs WHERE list_id=$1 ORDER BY leg_index`, listID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []OrderListLeg{}
	for rows.Next() {
		var l OrderListLeg
		if err := rows.Scan(&l.ID, &l.ListID, &l.LegIndex, &l.Role,
			&l.OrderID, &l.Params, &l.State, &l.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

func (s *PgStore) OrderListGet(ctx context.Context, listID int64) (*OrderList, []OrderListLeg, error) {
	l, err := scanOrderList(s.pool.QueryRow(ctx,
		`SELECT `+orderListCols+` FROM order_lists WHERE id=$1`, listID))
	if isNoRows(err) {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, err
	}
	legs, err := s.listLegs(ctx, l.ID)
	return l, legs, err
}

func (s *PgStore) OrderListByWorking(ctx context.Context, workingOrderID int64) (*OrderList, []OrderListLeg, error) {
	l, err := scanOrderList(s.pool.QueryRow(ctx,
		`SELECT `+orderListCols+` FROM order_lists WHERE working_order_id=$1`,
		workingOrderID))
	if isNoRows(err) {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, err
	}
	legs, err := s.listLegs(ctx, l.ID)
	return l, legs, err
}

func (s *PgStore) OrderListByLegOrder(ctx context.Context, orderID int64) (*OrderList, []OrderListLeg, error) {
	var listID int64
	err := s.pool.QueryRow(ctx, `
		SELECT list_id FROM order_list_legs WHERE order_id=$1 LIMIT 1`,
		orderID).Scan(&listID)
	if isNoRows(err) {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, err
	}
	return s.OrderListGet(ctx, listID)
}

func (s *PgStore) OrderListsPage(ctx context.Context, accountID int64, openOnly bool,
	cursorAt time.Time, cursorID int64, limit int) ([]OrderList, int64, error) {
	openStates := []string{ListStateExecuting, ListStateAllDone}
	statePred := `state::text = ANY($2)`
	if !openOnly {
		statePred = `state::text <> ALL($2)`
	}
	args := []any{accountID, openStates}
	where := `account_id=$1 AND ` + statePred
	if !cursorAt.IsZero() {
		args = append(args, cursorAt, cursorID)
		where += fmt.Sprintf(` AND (created_at, id) < ($%d, $%d)`, len(args)-1, len(args))
	}
	var total int64
	if err := s.pool.QueryRow(ctx,
		`SELECT count(*) FROM order_lists WHERE `+where, args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	args = append(args, limit+1)
	rows, err := s.pool.Query(ctx,
		`SELECT `+orderListCols+` FROM order_lists WHERE `+where+
			` ORDER BY created_at DESC, id DESC LIMIT $`+fmt.Sprint(len(args)),
		args...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	out := []OrderList{}
	for rows.Next() {
		var l OrderList
		var locked, net string
		if err := rows.Scan(&l.ID, &l.AccountID, &l.InstrumentID,
			&l.ContingencyType, &l.State, &l.WorkingOrderID, &locked, &net,
			&l.ClientOrderID, &l.FailReason,
			&l.CreatedAt, &l.UpdatedAt, &l.ClosedAt); err != nil {
			return nil, 0, err
		}
		l.LockedProceeds = decimal.RequireFromString(locked)
		l.NetPendingQty = decimal.RequireFromString(net)
		out = append(out, l)
	}
	return out, total, rows.Err()
}

// ActivatePendingTx — see the interface contract. The pending order
// rows ride the SAME insertOrderInTx (dedup inside the tx); OPOCO's
// pair shares oco_group_id exactly like InsertOcoPairTx.
func (s *PgStore) ActivatePendingTx(ctx context.Context, listID int64,
	locked, netQty decimal.Decimal, ocoGroupID *int64,
	pending []InsertParams) ([]*Order, bool, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, false, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var state string
	if err := tx.QueryRow(ctx, `
		SELECT state::text FROM order_lists WHERE id=$1 FOR UPDATE`,
		listID).Scan(&state); err != nil {
		if isNoRows(err) {
			return nil, false, nil
		}
		return nil, false, err
	}
	if state != ListStateExecuting {
		return nil, false, nil // already activated/closed — replay
	}
	out := make([]*Order, 0, len(pending))
	for i := range pending {
		p := pending[i]
		p.OcoGroupID = ocoGroupID
		o, err := insertOrderInTx(ctx, tx, p)
		if err != nil {
			return nil, false, err
		}
		out = append(out, o)
	}
	// Stamp the pending leg rows (leg_index 1..N — index 0 is WORKING).
	for i, o := range out {
		oid := o.ID
		tag, err := tx.Exec(ctx, `
			UPDATE order_list_legs SET order_id=$3, state='PLACED',
			    updated_at=now()
			 WHERE list_id=$1 AND leg_index=$2`, listID, i+1, oid)
		if err != nil {
			return nil, false, fmt.Errorf("stamp leg %d: %w", i+1, err)
		}
		if tag.RowsAffected() == 0 {
			return nil, false, fmt.Errorf("list %d leg %d missing", listID, i+1)
		}
	}
	if _, err := tx.Exec(ctx, `
		UPDATE order_lists
		   SET state='ALL_DONE', locked_proceeds=$2::numeric,
		       net_pending_qty=$3::numeric, updated_at=now()
		 WHERE id=$1`, listID, locked.String(), netQty.String()); err != nil {
		return nil, false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, false, fmt.Errorf("commit pending activation: %w", err)
	}
	return out, true, nil
}

func (s *PgStore) SetOrderListState(ctx context.Context, listID int64,
	from []string, to, failReason string) (bool, error) {
	tag, err := s.pool.Exec(ctx, `
		UPDATE order_lists
		   SET state=$2::order_list_state_enum,
		       fail_reason=NULLIF($4,''), closed_at=now(), updated_at=now()
		 WHERE id=$1 AND state::text = ANY($3)`,
		listID, to, from, failReason)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() > 0, nil
}

func (s *PgStore) OpenOrderLists(ctx context.Context) ([]OrderList, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT `+orderListCols+` FROM order_lists
		 WHERE state='EXECUTING' ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []OrderList{}
	for rows.Next() {
		var l OrderList
		var locked, net string
		if err := rows.Scan(&l.ID, &l.AccountID, &l.InstrumentID,
			&l.ContingencyType, &l.State, &l.WorkingOrderID, &locked, &net,
			&l.ClientOrderID, &l.FailReason,
			&l.CreatedAt, &l.UpdatedAt, &l.ClosedAt); err != nil {
			return nil, err
		}
		l.LockedProceeds = decimal.RequireFromString(locked)
		l.NetPendingQty = decimal.RequireFromString(net)
		out = append(out, l)
	}
	return out, rows.Err()
}

func (s *PgStore) ListLegPlaced(ctx context.Context, legID, orderID int64, state string) error {
	_, err := s.pool.Exec(ctx, `
		UPDATE order_list_legs SET order_id=$2, state=$3, updated_at=now()
		 WHERE id=$1`, legID, orderID, state)
	return err
}

// bracketChildJSON encodes/decodes the trigger+limit config persisted
// as bracket_orders.child_sl / child_tp JSONB.
func encodeBracketChild(c BracketChild) ([]byte, error) {
	m := map[string]any{}
	if c.TriggerPrice != nil {
		m["trigger_price"] = c.TriggerPrice.String()
	}
	if c.LimitPrice != nil {
		m["limit_price"] = c.LimitPrice.String()
	}
	if c.ClientOrderID != "" {
		m["client_order_id"] = c.ClientOrderID
	}
	return json.Marshal(m)
}

func decodeBracketChild(raw []byte, c *BracketChild) error {
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return fmt.Errorf("bracket child decode: %w", err)
	}
	if s, ok := m["trigger_price"].(string); ok && s != "" {
		d, err := decimal.NewFromString(s)
		if err != nil {
			return fmt.Errorf("bracket child trigger_price: %w", err)
		}
		c.TriggerPrice = &d
	}
	if s, ok := m["limit_price"].(string); ok && s != "" {
		d, err := decimal.NewFromString(s)
		if err != nil {
			return fmt.Errorf("bracket child limit_price: %w", err)
		}
		c.LimitPrice = &d
	}
	if s, ok := m["client_order_id"].(string); ok {
		c.ClientOrderID = s
	}
	return nil
}
