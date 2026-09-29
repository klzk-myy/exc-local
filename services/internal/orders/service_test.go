package orders

import (
	"context"
	"errors"
	"testing"
	"time"

	flatbuffers "github.com/google/flatbuffers/go"

	"exchange/internal/config"
	"exchange/internal/ipc"
	"exchange/internal/ipc/wire"
	"exchange/pkg/decimal"
	excerrors "exchange/pkg/errors"
)

// ---- fakes -----------------------------------------------------------------

type dedupKey struct {
	acct int64
	coid string
}

// fakeStore is an in-memory Store — enough semantics to exercise the
// service-level ordering rules (dedup conflict, CAS miss, open-set
// scoping) without PostgreSQL.
type fakeStore struct {
	acct      *Account
	inst      *Instrument
	bal       decimal.Decimal
	orders    map[int64]*Order
	dedup     map[dedupKey]*DedupRow
	auditRows []AuditEntry
	nextID    int64
	refPrice  *decimal.Decimal
}

func newFakeStore() *fakeStore {
	ref := decimal.MustFromString("1.05")
	return &fakeStore{
		acct:     testAcct(),
		inst:     testInst(),
		bal:      decimal.MustFromString("100000"),
		orders:   map[int64]*Order{},
		dedup:    map[dedupKey]*DedupRow{},
		nextID:   100,
		refPrice: &ref,
	}
}

func (s *fakeStore) InstrumentBySymbol(_ context.Context, sym string) (*Instrument, error) {
	if s.inst == nil || s.inst.Symbol != config.CanonicalSymbol(sym) {
		return nil, nil
	}
	return s.inst, nil
}
func (s *fakeStore) InstrumentByID(_ context.Context, id int64) (*Instrument, error) {
	if s.inst == nil || s.inst.ID != id {
		return nil, nil
	}
	return s.inst, nil
}
func (s *fakeStore) AccountByID(_ context.Context, id int64) (*Account, error) {
	if s.acct == nil || s.acct.ID != id {
		return nil, nil
	}
	return s.acct, nil
}
func (s *fakeStore) ReferencePrice(context.Context, int64) (*decimal.Decimal, error) {
	return s.refPrice, nil
}
func (s *fakeStore) AvailableBalance(_ context.Context, _ int64, _ string) (*decimal.Decimal, error) {
	return &s.bal, nil
}
func (s *fakeStore) DedupLookup(_ context.Context, a int64, c string) (*DedupRow, error) {
	return s.dedup[dedupKey{a, c}], nil
}
func (s *fakeStore) InsertOrderTx(_ context.Context, p InsertParams) (*Order, *DedupRow, error) {
	if p.ClientOrderID != "" {
		if row, ok := s.dedup[dedupKey{p.AccountID, p.ClientOrderID}]; ok {
			return nil, row, &dedupConflict{row: row}
		}
	}
	s.nextID++
	o := &Order{
		ID: s.nextID, AccountID: p.AccountID, InstrumentID: p.InstrumentID,
		ClientOrderID: p.ClientOrderID, Side: p.Side, OrderType: p.OrderType,
		Quantity: p.Quantity, QuoteQuantity: p.QuoteQuantity,
		Price: p.Price, StopPrice: p.StopPrice, DisplayQty: p.DisplayQty,
		TimeInForce: p.TimeInForce, Status: "PENDING",
		OrderSeq: p.OrderSeq, PostOnly: p.PostOnly, ReduceOnly: p.ReduceOnly,
		STPMode: p.STPMode, SessionID: p.SessionID,
		ShardID:   &p.ShardID,
		CreatedAt: time.Now(), UpdatedAt: time.Now(),
	}
	s.orders[o.ID] = o
	if p.ClientOrderID != "" {
		s.dedup[dedupKey{p.AccountID, p.ClientOrderID}] = &DedupRow{
			AccountID: p.AccountID, ClientOrderID: p.ClientOrderID,
			OrderID: o.ID, RequestHash: p.RequestHash,
		}
	}
	return o, nil, nil
}
func (s *fakeStore) GetOrder(_ context.Context, id int64) (*Order, error) {
	if o := s.orders[id]; o != nil {
		cp := *o // copy: RevertAmend must see pre-CAS values
		return &cp, nil
	}
	return nil, nil
}
func (s *fakeStore) ListOrders(_ context.Context, q ListQuery) ([]Order, int64, error) {
	var out []Order
	for _, o := range s.orders {
		if o.AccountID != q.AccountID {
			continue
		}
		out = append(out, *o)
	}
	return out, int64(len(out)), nil
}
func (s *fakeStore) OpenOrders(_ context.Context, sc MassCancelScope) ([]Order, error) {
	var out []Order
	for _, o := range s.orders {
		if !isTerminal(o.Status) && isOpen(o.Status) {
			if sc.AccountID != 0 && o.AccountID != sc.AccountID {
				continue
			}
			if sc.InstrumentID != 0 && o.InstrumentID != sc.InstrumentID {
				continue
			}
			if sc.Side != "" && o.Side != sc.Side {
				continue
			}
			if sc.OrderType != "" && o.OrderType != sc.OrderType {
				continue
			}
			if sc.SessionID != "" && o.SessionID != sc.SessionID {
				continue
			}
			out = append(out, *o)
		}
	}
	return out, nil
}
func isOpen(st string) bool {
	for _, x := range OpenStatuses {
		if x == st {
			return true
		}
	}
	return false
}
func (s *fakeStore) ApplyCancel(_ context.Context, id int64) error {
	if o := s.orders[id]; o != nil && isOpen(o.Status) {
		o.Status = "CANCELLED"
	}
	return nil
}
func (s *fakeStore) ApplyFill(_ context.Context, id int64, px, qty decimal.Decimal) error {
	o := s.orders[id]
	if o == nil || o.Status == "FILLED" {
		return nil
	}
	total := o.FilledQty.Add(qty)
	if o.AvgFillPrice == nil {
		v := px
		o.AvgFillPrice = &v
	} else {
		v := (o.AvgFillPrice.Mul(o.FilledQty).Add(px.Mul(qty))).Div(total)
		o.AvgFillPrice = &v
	}
	o.FilledQty = total
	if !total.LessThan(o.Quantity) {
		o.Status = "FILLED"
	} else {
		o.Status = "PARTIALLY_FILLED"
	}
	return nil
}
func (s *fakeStore) MarkActive(_ context.Context, id int64) error {
	if o := s.orders[id]; o != nil {
		o.Status = "ACTIVE"
	}
	return nil
}
func (s *fakeStore) MarkRejected(_ context.Context, id int64) error {
	if o := s.orders[id]; o != nil && isOpen(o.Status) {
		o.Status = "REJECTED"
	}
	return nil
}
func (s *fakeStore) AmendCAS(_ context.Context, id int64, expectedSeq, newSeq uint64,
	f AmendFields, audits []AuditEntry) (*Order, bool, error) {
	o := s.orders[id]
	if o == nil || o.OrderSeq != expectedSeq {
		return nil, false, nil
	}
	o.OrderSeq = newSeq
	if f.Price != nil {
		o.Price = f.Price
	}
	if f.Quantity != nil {
		o.Quantity = *f.Quantity
	}
	if f.StopPrice != nil {
		o.StopPrice = f.StopPrice
	}
	if f.DisplayQty != nil {
		o.DisplayQty = f.DisplayQty
	}
	if f.TimeInForce != "" {
		o.TimeInForce = f.TimeInForce
	}
	s.auditRows = append(s.auditRows, audits...)
	cp := *o
	return &cp, true, nil
}
func (s *fakeStore) RevertAmend(_ context.Context, prev *Order) error {
	s.orders[prev.ID] = prev
	return nil
}
func (s *fakeStore) WriteAudit(_ context.Context, entries []AuditEntry) error {
	s.auditRows = append(s.auditRows, entries...)
	return nil
}
func (s *fakeStore) AuditTrail(_ context.Context, id int64) ([]AuditEntry, error) {
	var out []AuditEntry
	for _, e := range s.auditRows {
		if e.OrderID == id {
			out = append(out, e)
		}
	}
	return out, nil
}
func (s *fakeStore) Amendments(ctx context.Context, id int64) ([]AuditEntry, error) {
	return s.AuditTrail(ctx, id)
}
func (s *fakeStore) IdemLookup(context.Context, int64, string) (*IdemRow, error) {
	return nil, nil
}
func (s *fakeStore) IdemStore(context.Context, int64, string, string, string, int, []byte) error {
	return nil
}
func (s *fakeStore) seed(o *Order) {
	if o.ID == 0 {
		s.nextID++
		o.ID = s.nextID
	}
	s.orders[o.ID] = o
}

// fakeSubmitter emulates the engine end-to-end for cancel events: it
// decodes the payload (covering the FlatBuffers encoders) and resolves
// the pending confirmation + applies the cancel on the store, exactly
// as the outbound Consumer does in production. fail=true simulates a
// transport failure.
type fakeSubmitter struct {
	sent    [][]byte
	fail    bool
	pending *pendingConfirms
	store   *fakeStore
}

func (f *fakeSubmitter) Send(_ context.Context, _ uint16, payload []byte) error {
	if f.fail {
		return excerrors.New("SERVICE_DEGRADED", "engine image unavailable")
	}
	f.sent = append(f.sent, payload)
	ev := ipc.DecodeEvent(payload)
	if ev != nil && ev.TypeType() == wire.EventTypeOrderCancel {
		var t flatbuffers.Table
		if ev.Type(&t) {
			oc := &wire.OrderCancel{}
			oc.Init(t.Bytes, t.Pos)
			_ = f.store.ApplyCancel(context.Background(), int64(oc.OrderId()))
			if f.pending != nil {
				f.pending.resolve(oc.OrderId())
			}
		}
	}
	return nil
}
func (f *fakeSubmitter) Channel(uint16) (*ipc.Channel, error) {
	return nil, errors.New("no channel in tests")
}

type fakeBatchRL struct{ allow bool }

func (f fakeBatchRL) AllowBatch(context.Context, int64) (bool, error) { return f.allow, nil }

// newSvc wires a service against the canonical sharding.yaml (found via
// the standard walk-up from services/internal/orders).
// openKill is the always-open kill-switch fake — the seam fails closed
// when nil, so tests that exercise happy paths must opt out explicitly.
type openKill struct{}

func (openKill) OrderHalt(context.Context, int64, string, string, string) (string, string, error) {
	return "", "", nil
}

func newSvc(t *testing.T, st *fakeStore, sub *fakeSubmitter) *Service {
	t.Helper()
	shards, err := config.LoadShardMap("")
	if err != nil {
		t.Fatalf("shard map: %v", err)
	}
	svc, err := NewService(Options{
		Store: st, Submitter: sub, ShardMap: shards,
		KillSwitch: openKill{},
		AckTimeout: 200 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	if sub != nil {
		sub.pending = svc.Pending()
		sub.store = st
	}
	return svc
}

// ---- tests -----------------------------------------------------------------

func TestSubmitHappyPathAndDedup(t *testing.T) {
	st := newFakeStore()
	sub := &fakeSubmitter{}
	svc := newSvc(t, st, sub)
	ctx := context.Background()

	req := submitReq()
	req.ClientOrderID = "c1"
	ack, err := svc.Submit(ctx, st.acct, req)
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	if ack.Status != "ACTIVE" || len(sub.sent) != 1 {
		t.Fatalf("expected ACTIVE + 1 dispatch, got %+v / %d", ack, len(sub.sent))
	}
	if st.orders[ack.OrderID].Status != "ACTIVE" {
		t.Fatalf("read model not activated: %s", st.orders[ack.OrderID].Status)
	}

	// Replay returns the stored ack — flagged, no second dispatch.
	ack2, err := svc.Submit(ctx, st.acct, submitReq2("c1"))
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	if !ack2.Replay || ack2.OrderID != ack.OrderID || len(sub.sent) != 1 {
		t.Fatalf("replay semantics broken: %+v sent=%d", ack2, len(sub.sent))
	}

	// Same client_order_id, different payload → 409 collision.
	bad := submitReq2("c1")
	bad.Price = d("1.06")
	_, err = svc.Submit(ctx, st.acct, bad)
	if codeOf(t, err) != "IDEMPOTENCY_KEY_COLLISION" {
		t.Fatalf("collision: got %v", err)
	}
	if len(sub.sent) != 1 {
		t.Fatalf("collision dispatched: %d", len(sub.sent))
	}
}

func submitReq2(coid string) *SubmitRequest {
	r := submitReq()
	r.ClientOrderID = coid
	return r
}

func TestSubmitInsufficientBalance(t *testing.T) {
	st := newFakeStore()
	st.bal = decimal.MustFromString("10") // needs ~1155 USD quote
	svc := newSvc(t, st, &fakeSubmitter{})
	_, err := svc.Submit(context.Background(), st.acct, submitReq())
	if codeOf(t, err) != "INSUFFICIENT_BALANCE" {
		t.Fatalf("got %v", err)
	}
}

func TestSubmitMarksRejectedOnDispatchFailure(t *testing.T) {
	st := newFakeStore()
	svc := newSvc(t, st, &fakeSubmitter{fail: true})
	_, err := svc.Submit(context.Background(), st.acct, submitReq())
	if err == nil {
		t.Fatal("expected dispatch error")
	}
	for _, o := range st.orders {
		if o.Status != "REJECTED" {
			t.Fatalf("order not marked rejected: %s", o.Status)
		}
	}
}

func TestSubmitQuoteMarketConvertsToBase(t *testing.T) {
	st := newFakeStore()
	sub := &fakeSubmitter{}
	svc := newSvc(t, st, sub)
	req := &SubmitRequest{
		Symbol: "EURUSD", Side: SideBuy, OrderType: TypeMarket,
		QuoteQuantity: d("1575"), // 1575/1.05 = 1500 → lot-rounds to 1000
	}
	ack, err := svc.Submit(context.Background(), st.acct, req)
	if err != nil {
		t.Fatalf("quote market submit: %v", err)
	}
	o := st.orders[ack.OrderID]
	if !o.Quantity.Equal(decimal.MustFromString("1000")) {
		t.Fatalf("lot-rounded base qty wrong: %s", o.Quantity)
	}
}

func TestCancelOwnershipAndConfirm(t *testing.T) {
	st := newFakeStore()
	sub := &fakeSubmitter{}
	svc := newSvc(t, st, sub)
	ord := openOrder()
	st.seed(ord)

	// Foreign account → ORDER_NOT_FOUND.
	other := &Account{ID: 99, Status: "ACTIVE", KycTier: "T1"}
	_, err := svc.Cancel(context.Background(), other, 42, "u", "", "")
	if codeOf(t, err) != "ORDER_NOT_FOUND" {
		t.Fatalf("foreign cancel: got %v", err)
	}
	ack, err := svc.Cancel(context.Background(), st.acct, 42, "u", "", "")
	if err != nil || ack.Status != "CANCELLED" {
		t.Fatalf("cancel: ack=%+v err=%v", ack, err)
	}
	if len(sub.sent) != 1 {
		t.Fatalf("cancel not dispatched: %d", len(sub.sent))
	}
	// Idempotent retry: already-cancelled returns without resend.
	ack2, err := svc.Cancel(context.Background(), st.acct, 42, "u", "", "")
	if err != nil || ack2.Status != "CANCELLED" || len(sub.sent) != 1 {
		t.Fatalf("cancel retry: %+v sent=%d err=%v", ack2, len(sub.sent), err)
	}
}

func TestCancelTimesOutWithoutEcho(t *testing.T) {
	st := newFakeStore()
	// Submitter that never resolves the pending confirm.
	sub := &fakeSubmitter{}
	svc := newSvc(t, st, sub)
	sub.pending = nil // emulate echo never arriving
	ord := openOrder()
	st.seed(ord)
	_, err := svc.Cancel(context.Background(), st.acct, 42, "u", "", "")
	if codeOf(t, err) != "GATEWAY_TIMEOUT_MATCHING_ENGINE" {
		t.Fatalf("timeout: got %v", err)
	}
}

func TestModifyStaleAndCAS(t *testing.T) {
	st := newFakeStore()
	svc := newSvc(t, st, &fakeSubmitter{})
	st.seed(openOrder())

	// Wrong seq → STALE_MODIFY.
	mr := &ModifyRequest{Price: d("1.06"), OrderSeq: u64p(99)}
	_, err := svc.Modify(context.Background(), st.acct, 42, mr, "u", "r1", "127.0.0.1")
	if codeOf(t, err) != "STALE_MODIFY" {
		t.Fatalf("stale: got %v", err)
	}
	// Right seq → amend dispatched, seq bumped, audit row written.
	mr.OrderSeq = u64p(5)
	updated, err := svc.Modify(context.Background(), st.acct, 42, mr, "u", "r1", "127.0.0.1")
	if err != nil {
		t.Fatalf("modify: %v", err)
	}
	if updated.OrderSeq == 5 || !updated.Price.Equal(decimal.MustFromString("1.06")) {
		t.Fatalf("amend not applied: %+v", updated)
	}
	if len(st.auditRows) != 1 || st.auditRows[0].Operation != "MODIFY" ||
		st.auditRows[0].FieldName != "price" || st.auditRows[0].RequestID != "r1" {
		t.Fatalf("audit row wrong: %+v", st.auditRows)
	}
}

func TestModifyRollbackOnDispatchFailure(t *testing.T) {
	st := newFakeStore()
	svc := newSvc(t, st, &fakeSubmitter{fail: true})
	st.seed(openOrder())
	mr := &ModifyRequest{Price: d("1.06"), OrderSeq: u64p(5)}
	_, err := svc.Modify(context.Background(), st.acct, 42, mr, "u", "", "")
	if err == nil {
		t.Fatal("expected dispatch error")
	}
	// CAS reverted — original values restored.
	o := st.orders[42]
	if o.OrderSeq != 5 || !o.Price.Equal(decimal.MustFromString("1.05")) {
		t.Fatalf("amend not reverted: %+v", o)
	}
}

func TestAmendKeepPriorityService(t *testing.T) {
	st := newFakeStore()
	st.inst.LotSize = decimal.MustFromString("100") // qty-down to 500 is on-lot
	svc := newSvc(t, st, &fakeSubmitter{})
	st.seed(openOrder())

	// Quantity increase rejected before dispatch.
	kp := &KeepPriorityRequest{OrderSeq: u64p(5), Quantity: d("2000")}
	if _, err := svc.AmendKeepPriority(context.Background(), st.acct, 42, kp, "u", "", ""); codeOf(t, err) != "ORDER_AMEND_REJECTED" {
		t.Fatalf("qty up: got %v", err)
	}
	// Decrease preserves the order id and dispatches an amend.
	kp.Quantity = d("500")
	updated, err := svc.AmendKeepPriority(context.Background(), st.acct, 42, kp, "u", "r2", "")
	if err != nil {
		t.Fatalf("keep-priority: %v", err)
	}
	if updated.ID != 42 || !updated.Quantity.Equal(decimal.MustFromString("500")) {
		t.Fatalf("keep-priority result wrong: %+v", updated)
	}
	if st.auditRows[len(st.auditRows)-1].Operation != "AMEND" {
		t.Fatalf("audit op wrong: %+v", st.auditRows[len(st.auditRows)-1])
	}
}

func TestCancelReplaceAtomicAmend(t *testing.T) {
	st := newFakeStore()
	sub := &fakeSubmitter{}
	svc := newSvc(t, st, sub)
	st.seed(openOrder())

	cr := &CancelReplaceRequest{
		Mode:          "STOP_ON_FAILURE",
		ModifyRequest: ModifyRequest{OrderSeq: u64p(5), Price: d("1.06")},
	}
	replaced, err := svc.CancelReplace(context.Background(), st.acct, 42, cr, "u", "r3", "")
	if err != nil {
		t.Fatalf("cancel-replace: %v", err)
	}
	// Atomic amend: same order id, seq bumped, no cancel event on the wire.
	if replaced.ID != 42 || replaced.OrderSeq == 5 {
		t.Fatalf("replace result wrong: %+v", replaced)
	}
	for _, p := range sub.sent {
		ev := ipc.DecodeEvent(p)
		if ev != nil && ev.TypeType() == wire.EventTypeOrderCancel {
			t.Fatal("cancel-replace emitted a standalone OrderCancel")
		}
	}
	if st.auditRows[len(st.auditRows)-1].Operation != "CANCEL_REPLACE" {
		t.Fatalf("audit op wrong: %+v", st.auditRows[len(st.auditRows)-1])
	}
	// Invalid mode fails closed.
	cr.Mode = "BOGUS"
	if _, err := svc.CancelReplace(context.Background(), st.acct, 42, cr, "u", "", ""); codeOf(t, err) != "INVALID_REQUEST" {
		t.Fatalf("bad mode: got %v", err)
	}
}

func TestMassCancelScoped(t *testing.T) {
	st := newFakeStore()
	sub := &fakeSubmitter{}
	svc := newSvc(t, st, sub)
	a := openOrder() // BUY LIMIT, acct 7
	b := openOrder() // SELL LIMIT — outside scope
	b.ID, b.Side = 0, SideSell
	c := openOrder() // BUY MARKET — outside scope
	c.ID, c.OrderType = 0, TypeMarket
	c.Price = nil
	st.seed(a)
	st.seed(b)
	st.seed(c)

	res, err := svc.MassCancel(context.Background(),
		MassCancelScope{AccountID: 7, Side: SideBuy, OrderType: TypeLimit},
		"u", "r4", "")
	if err != nil {
		t.Fatalf("mass cancel: %v", err)
	}
	if res.Cancelled != 1 {
		t.Fatalf("cancelled %d, want 1", res.Cancelled)
	}
	if st.orders[a.ID].Status != "CANCELLED" {
		t.Fatalf("scoped order still %s", st.orders[a.ID].Status)
	}
	if st.orders[b.ID].Status == "CANCELLED" || st.orders[c.ID].Status == "CANCELLED" {
		t.Fatal("out-of-scope orders cancelled")
	}
	// MASS_CANCEL audit tag.
	found := false
	for _, e := range st.auditRows {
		if e.Operation == "MASS_CANCEL" && e.OrderID == a.ID {
			found = true
		}
	}
	if !found {
		t.Fatal("no MASS_CANCEL audit row")
	}
}

func TestBatchSubmitAtomicity(t *testing.T) {
	st := newFakeStore()
	sub := &fakeSubmitter{}
	svc := newSvc(t, st, sub)
	ctx := context.Background()

	res, err := svc.BatchSubmit(ctx, st.acct, []*SubmitRequest{
		{Symbol: "EURUSD", Side: SideBuy, OrderType: TypeLimit, TimeInForce: TIFGTC, Quantity: d("1000"), Price: d("1.05"), ClientOrderID: "b1"},
		{Symbol: "EURUSD", Side: SideBuy, OrderType: TypeLimit, TimeInForce: TIFGTC, Quantity: d("1000"), Price: d("1.04"), ClientOrderID: "b2"},
	}, "r5", "")
	if err != nil {
		t.Fatalf("batch: %v", err)
	}
	if len(res) != 2 || res[0].Index != 0 || res[1].Index != 1 || len(sub.sent) != 2 {
		t.Fatalf("batch results/dispatch wrong: %+v sent=%d", res, len(sub.sent))
	}

	// Any validation failure aborts the whole batch — zero dispatch.
	sub.sent = nil
	_, err = svc.BatchSubmit(ctx, st.acct, []*SubmitRequest{
		{Symbol: "EURUSD", Side: SideBuy, OrderType: TypeLimit, TimeInForce: TIFGTC, Quantity: d("1000"), Price: d("1.05")},
		{Symbol: "EURUSD", Side: "SHORT", OrderType: TypeLimit, TimeInForce: TIFGTC, Quantity: d("1000"), Price: d("1.05")},
	}, "r6", "")
	var bf *BatchSubmitFailure
	if !errors.As(err, &bf) {
		t.Fatalf("batch failure type %T: %v", err, err)
	}
	if bf.Results[0].Status != "ACCEPTED" || bf.Results[1].Status != "REJECTED" {
		t.Fatalf("index-mapped results wrong: %+v", bf.Results)
	}
	if len(sub.sent) != 0 || len(st.orders) != 2 {
		t.Fatalf("partial dispatch/persist happened: sent=%d orders=%d",
			len(sub.sent), len(st.orders))
	}

	// Duplicate client_order_id inside the batch aborts too.
	_, err = svc.BatchSubmit(ctx, st.acct, []*SubmitRequest{
		submitReq2("dup"), submitReq2("dup"),
	}, "r7", "")
	var ce *excerrors.Error
	if !errors.As(err, &ce) || ce.Code != "IDEMPOTENCY_KEY_COLLISION" {
		t.Fatalf("in-batch dup: got %v", err)
	}

	// Over the ceiling.
	overs := make([]*SubmitRequest, BatchSubmitMax+1)
	for i := range overs {
		overs[i] = submitReq()
	}
	if _, err := svc.BatchSubmit(ctx, st.acct, overs, "", ""); codeOf(t, err) != "BATCH_SIZE_EXCEEDED" {
		t.Fatalf("oversize batch: got %v", err)
	}
}

func TestBatchRateLimitGate(t *testing.T) {
	st := newFakeStore()
	sub := &fakeSubmitter{}
	shards, err := config.LoadShardMap("")
	if err != nil {
		t.Fatalf("shard map: %v", err)
	}
	svc, err := NewService(Options{
		Store: st, Submitter: sub, ShardMap: shards,
		KillSwitch: openKill{},
		BatchRL:    fakeBatchRL{allow: false},
	})
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	_, err = svc.BatchSubmit(context.Background(), st.acct,
		[]*SubmitRequest{submitReq()}, "", "")
	if codeOf(t, err) != "RATE_LIMIT_TIER_EXCEEDED" {
		t.Fatalf("batch RL: got %v", err)
	}
}

func TestBatchCancelResolvesClientIDs(t *testing.T) {
	st := newFakeStore()
	sub := &fakeSubmitter{}
	svc := newSvc(t, st, sub)
	o := openOrder()
	o.ClientOrderID = "cli-1"
	st.seed(o)
	st.dedup[dedupKey{7, "cli-1"}] = &DedupRow{AccountID: 7, ClientOrderID: "cli-1", OrderID: o.ID}

	res, err := svc.BatchCancel(context.Background(), st.acct,
		nil, []string{"cli-1"}, "u", "", "")
	if err != nil || len(res) != 1 || res[0].Status != "CANCELLED" {
		t.Fatalf("batch cancel: %+v err=%v", res, err)
	}
	// Foreign order id aborts the batch.
	other := openOrder()
	other.ID, other.AccountID = 0, 99
	st.seed(other)
	_, err = svc.BatchCancel(context.Background(), st.acct,
		[]int64{o.ID + 9000, other.ID}, nil, "u", "", "")
	if codeOf(t, err) != "ORDER_NOT_FOUND" {
		t.Fatalf("foreign/unknown batch cancel: got %v", err)
	}
}

func TestDryRunNoSideEffects(t *testing.T) {
	st := newFakeStore()
	sub := &fakeSubmitter{}
	svc := newSvc(t, st, sub)
	rep, err := svc.DryRun(context.Background(), st.acct, submitReq())
	if err != nil {
		t.Fatalf("dry run: %v", err)
	}
	if rep.Binding || rep.EstimatedBaseQty != "1000" {
		t.Fatalf("preview wrong: %+v", rep)
	}
	if len(st.orders) != 0 || len(sub.sent) != 0 {
		t.Fatalf("dry run mutated state: orders=%d sent=%d", len(st.orders), len(sub.sent))
	}

	// Insufficient funds surface as HIGH risk + warning, not an error.
	st.bal = decimal.MustFromString("1")
	rep, err = svc.DryRun(context.Background(), st.acct, submitReq())
	if err != nil {
		t.Fatalf("insufficient dry run should not error: %v", err)
	}
	if rep.RiskLevel != "HIGH" || len(rep.Warnings) == 0 {
		t.Fatalf("insufficient funds not predicted: %+v", rep)
	}

	// Quote-denominated market preview.
	st.bal = decimal.MustFromString("100000")
	rep, err = svc.DryRun(context.Background(), st.acct, &SubmitRequest{
		Symbol: "EURUSD", Side: SideBuy, OrderType: TypeMarket,
		QuoteQuantity: d("1050"),
	})
	if err != nil || rep.EstimatedBaseQty != "1000" {
		t.Fatalf("quote preview: %+v err=%v", rep, err)
	}
}

func TestEncodeCancelAmendRoundTrip(t *testing.T) {
	b := flatbuffers.NewBuilder(128)
	payload := EncodeCancelEvent(b, 77, 99, 123, 456)
	ev := ipc.DecodeEvent(payload)
	if ev == nil || ev.TypeType() != wire.EventTypeOrderCancel || ev.Seq() != 77 || ev.Ts() != 99 {
		t.Fatalf("cancel event decode wrong: %+v", ev)
	}
	var tb flatbuffers.Table
	if !ev.Type(&tb) {
		t.Fatal("cancel union missing")
	}
	oc := &wire.OrderCancel{}
	oc.Init(tb.Bytes, tb.Pos)
	if oc.OrderId() != 123 || oc.AccountId() != 456 {
		t.Fatalf("cancel fields wrong: id=%d acct=%d", oc.OrderId(), oc.AccountId())
	}

	b.Reset()
	payload = EncodeAmendEvent(b, 78, 100, 123, 9,
		decimal.Scaled(decimal.MustFromString("1.06")),
		decimal.Scaled(decimal.MustFromString("2000")), 0, 0)
	ev = ipc.DecodeEvent(payload)
	if ev == nil || ev.TypeType() != wire.EventTypeOrderAmend {
		t.Fatalf("amend event decode wrong: %+v", ev)
	}
	if !ev.Type(&tb) {
		t.Fatal("amend union missing")
	}
	oa := &wire.OrderAmend{}
	oa.Init(tb.Bytes, tb.Pos)
	if oa.OrderId() != 123 || oa.OrderSeq() != 9 ||
		oa.Price() != 106_000_000 || oa.Qty() != 200_000_000_000 {
		t.Fatalf("amend fields wrong: id=%d seq=%d px=%d qty=%d",
			oa.OrderId(), oa.OrderSeq(), oa.Price(), oa.Qty())
	}
}
