package orders

import (
	"context"
	"errors"
	"strconv"
	"testing"
	"time"

	flatbuffers "github.com/google/flatbuffers/go"

	"exchange/internal/ipc"
	"exchange/internal/ipc/wire"
	"exchange/pkg/decimal"
)

// ---- auction fakes ---------------------------------------------------------

// openAuctionGate never freezes — pre-auction window.
type openAuctionGate struct{}

func (openAuctionGate) Frozen(context.Context, *Instrument, string, time.Time) (bool, error) {
	return false, nil
}

// frozenAuctionGate is the armed-CALL / T-30s window.
type frozenAuctionGate struct{}

func (frozenAuctionGate) Frozen(context.Context, *Instrument, string, time.Time) (bool, error) {
	return true, nil
}

// errAuctionGate models an unreadable freeze state — mutations fail
// closed per the AuctionGate contract.
type errAuctionGate struct{}

func (errAuctionGate) Frozen(context.Context, *Instrument, string, time.Time) (bool, error) {
	return false, errors.New("auction key unreadable")
}

// privNote records one PrivateNotify emission.
type privNote struct {
	acct    int64
	channel string
	data    map[string]any
}

// notifySink collects private-channel emissions for assertions.
type notifySink struct{ notes []privNote }

func (n *notifySink) fn() PrivateNotify {
	return func(a int64, ch string, data any) {
		m, _ := data.(map[string]any)
		n.notes = append(n.notes, privNote{a, ch, m})
	}
}

func (n *notifySink) has(channel string) bool {
	for _, x := range n.notes {
		if x.channel == channel {
			return true
		}
	}
	return false
}

func (n *notifySink) last(channel string) map[string]any {
	for i := len(n.notes) - 1; i >= 0; i-- {
		if n.notes[i].channel == channel {
			return n.notes[i].data
		}
	}
	return nil
}

// armedFeed fakes the Redis auction-control reads the Injector polls.
type armedFeed struct {
	armed map[string]string
	queue map[string][]int64
}

func (f *armedFeed) AuctionArmed(_ context.Context, sym string) (string, error) {
	return f.armed[sym], nil
}
func (f *armedFeed) AuctionQueue(_ context.Context, sym string) ([]int64, error) {
	return f.queue[sym], nil
}

// auctionReq builds a minimal valid MOO/MOC request (session orders are
// market-quantity orders — exactly one of quantity/quote_quantity, no
// price, TIF is normalized to GTD by validation).
func auctionReq(typ string) *SubmitRequest {
	return &SubmitRequest{
		Symbol: "EURUSD", Side: SideBuy, OrderType: typ,
		Quantity: d("2000"),
	}
}

func auctionSvc(t *testing.T, st *fakeStore, sub *fakeSubmitter,
	g AuctionGate, sink *notifySink) *Service {
	t.Helper()
	svc := newSvc(t, st, sub)
	if g != nil {
		svc.WithAuction(g)
	}
	if sink != nil {
		svc.WithNotify(sink.fn())
	}
	return svc
}

// submitErr unwraps Submit's error for codeOf assertions.
func submitErr(svc *Service, ctx context.Context, acct *Account,
	req *SubmitRequest) error {
	_, err := svc.Submit(ctx, acct, req)
	return err
}

// ---- submit path -----------------------------------------------------------

// §6.2b: a MOO/MOC submission is persisted + queued locally as
// RESERVED — it never reaches the wire at submit time, and the client
// gets an order.queued private notification.
func TestAuctionSubmitQueuesLocally(t *testing.T) {
	st := newFakeStore()
	sub := &fakeSubmitter{}
	sink := &notifySink{}
	svc := auctionSvc(t, st, sub, openAuctionGate{}, sink)
	ctx := context.Background()

	for _, typ := range []string{TypeMOO, TypeMOC} {
		req := auctionReq(typ)
		req.TimeInForce = TIFIOC // normalized to GTD by validation
		ack, err := svc.Submit(ctx, st.acct, req)
		if err != nil {
			t.Fatalf("%s submit: %v", typ, err)
		}
		if ack.Status != "RESERVED" {
			t.Fatalf("%s ack status %q, want RESERVED", typ, ack.Status)
		}
		if req.TimeInForce != TIFGTD {
			t.Fatalf("%s TIF %q, want normalized GTD", typ, req.TimeInForce)
		}
		o := st.orders[ack.OrderID]
		if o == nil || o.Status != "RESERVED" {
			t.Fatalf("%s row status: %+v", typ, o)
		}
	}
	if len(sub.sent) != 0 {
		t.Fatalf("queued auction orders hit the wire: %d sent", len(sub.sent))
	}
	if !sink.has(ChanOrderQueued) {
		t.Fatalf("no order.queued notification: %+v", sink.notes)
	}
	d := sink.last(ChanOrderQueued)
	if d["type"] != TypeMOC || d["status"] != "RESERVED" {
		t.Fatalf("order.queued payload: %+v", d)
	}
}

// The §6.2b queue contract: price/stop/post_only cannot express a
// session-uncross order; exactly one of quantity/quote_quantity.
func TestAuctionSubmitValidation(t *testing.T) {
	st := newFakeStore()
	svc := auctionSvc(t, st, &fakeSubmitter{}, openAuctionGate{}, nil)
	ctx := context.Background()

	req := auctionReq(TypeMOO)
	req.Price = d("1.05")
	if got := codeOf(t, submitErr(svc, ctx, st.acct, req)); got != "INVALID_REQUEST" {
		t.Fatalf("price on MOO: got %s", got)
	}
	req = auctionReq(TypeMOO)
	req.StopPrice = d("1.05")
	if got := codeOf(t, submitErr(svc, ctx, st.acct, req)); got != "INVALID_REQUEST" {
		t.Fatalf("stop_price on MOO: got %s", got)
	}
	req = auctionReq(TypeMOC)
	req.PostOnly = true
	if got := codeOf(t, submitErr(svc, ctx, st.acct, req)); got != "INVALID_REQUEST" {
		t.Fatalf("post_only on MOC: got %s", got)
	}
	req = auctionReq(TypeMOC)
	req.QuoteQuantity = d("2000")
	if got := codeOf(t, submitErr(svc, ctx, st.acct, req)); got != "QUOTE_QUANTITY_INVALID" {
		t.Fatalf("qty+quote on MOC: got %s", got)
	}
}

// Fail-closed admission: without the auction seam a MOO/MOC submit can
// never be freeze-gated, so it is rejected outright.
func TestAuctionSubmitFailsClosedWithoutGate(t *testing.T) {
	st := newFakeStore()
	svc := auctionSvc(t, st, &fakeSubmitter{}, nil, nil)
	if got := codeOf(t, submitErr(svc, context.Background(), st.acct, auctionReq(TypeMOO))); got != "INVALID_REQUEST" {
		t.Fatalf("nil auction gate: got %s", got)
	}
}

// ---- cancel / amend freeze gate -------------------------------------------

// Pre-freeze client cancel of a queued auction order is local-only:
// ApplyCancel + order.cancelled notification, no wire traffic.
func TestAuctionCancelPreFreeze(t *testing.T) {
	st := newFakeStore()
	sub := &fakeSubmitter{}
	sink := &notifySink{}
	svc := auctionSvc(t, st, sub, openAuctionGate{}, sink)
	ctx := context.Background()

	ack, err := svc.Submit(ctx, st.acct, auctionReq(TypeMOC))
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	ack2, err := svc.Cancel(ctx, st.acct, ack.OrderID, "user", "r1", "1.2.3.4")
	if err != nil {
		t.Fatalf("cancel: %v", err)
	}
	if ack2.Status != "CANCELLED" || st.orders[ack.OrderID].Status != "CANCELLED" {
		t.Fatalf("cancel state: ack=%+v row=%+v", ack2, st.orders[ack.OrderID])
	}
	if len(sub.sent) != 0 {
		t.Fatalf("queued cancel hit the wire: %d sent", len(sub.sent))
	}
	d := sink.last(ChanOrderCancelled)
	if d == nil || d["type"] != TypeMOC {
		t.Fatalf("no order.cancelled for queued MOC: %+v", sink.notes)
	}
}

// §6.2b item 5: once the freeze engages (armed CALL/EXTEND or T-30s)
// client cancels and amends fail with AMEND_IN_AUCTION_REJECTED; a
// gate error fails closed identically.
func TestAuctionFreezeRejectsMutation(t *testing.T) {
	ctx := context.Background()
	for name, gate := range map[string]AuctionGate{
		"frozen":     frozenAuctionGate{},
		"gate error": errAuctionGate{},
	} {
		st := newFakeStore()
		svc := auctionSvc(t, st, &fakeSubmitter{}, gate, nil)

		ack, err := svc.Submit(ctx, st.acct, auctionReq(TypeMOO))
		if err != nil {
			t.Fatalf("%s submit: %v", name, err)
		}
		if got := codeOf(t, func() error {
			_, err := svc.Cancel(ctx, st.acct, ack.OrderID, "user", "r", "ip")
			return err
		}()); got != "AMEND_IN_AUCTION_REJECTED" {
			t.Fatalf("%s cancel: got %s", name, got)
		}
		o := st.orders[ack.OrderID]
		if got := codeOf(t, func() error {
			_, err := svc.Modify(ctx, st.acct, ack.OrderID,
				&ModifyRequest{Quantity: d("3000"), OrderSeq: u64p(o.OrderSeq)},
				"user", "r", "ip")
			return err
		}()); got != "AMEND_IN_AUCTION_REJECTED" {
			t.Fatalf("%s amend: got %s", name, got)
		}
		if st.orders[ack.OrderID].Status != "RESERVED" {
			t.Fatalf("%s order mutated despite freeze: %+v", name, st.orders[ack.OrderID])
		}
	}
}

// Pre-freeze quantity amends stay local — the CAS is authoritative for
// never-injected auction rows (no wire OrderAmend exists to send).
func TestAuctionAmendPreFreeze(t *testing.T) {
	st := newFakeStore()
	sub := &fakeSubmitter{}
	svc := auctionSvc(t, st, sub, openAuctionGate{}, nil)
	ctx := context.Background()

	ack, err := svc.Submit(ctx, st.acct, auctionReq(TypeMOO))
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	o := st.orders[ack.OrderID]
	updated, err := svc.Modify(ctx, st.acct, ack.OrderID,
		&ModifyRequest{Quantity: d("4000"), OrderSeq: u64p(o.OrderSeq)},
		"user", "r", "ip")
	if err != nil {
		t.Fatalf("pre-freeze amend: %v", err)
	}
	if !updated.Quantity.Equal(decimal.MustFromString("4000")) {
		t.Fatalf("amended qty %s, want 4000", updated.Quantity)
	}
	if len(sub.sent) != 0 {
		t.Fatalf("queued amend hit the wire: %d sent", len(sub.sent))
	}
	// Price amends have no meaning for a session-uncross order.
	o = st.orders[ack.OrderID]
	if got := codeOf(t, func() error {
		_, err := svc.Modify(ctx, st.acct, ack.OrderID,
			&ModifyRequest{Price: d("1.06"), OrderSeq: u64p(o.OrderSeq)},
			"user", "r", "ip")
		return err
	}()); got != "ORDER_AMEND_REJECTED" {
		t.Fatalf("price amend: got %s", got)
	}
}

// Scoped mass cancel: queued auction orders apply locally pre-freeze
// alongside wire orders; post-freeze the queue is left to join the
// auction while wire orders still cancel.
func TestAuctionMassCancelScope(t *testing.T) {
	ctx := context.Background()

	// Pre-freeze: queued MOC cancels with the rest of the scope; a
	// scheduler AUCTION_UNFILLED_REMAINDER sweep surfaces the
	// AUCTION_CANCELLED notification reason (§6.2b).
	st := newFakeStore()
	sink := &notifySink{}
	svc := auctionSvc(t, st, &fakeSubmitter{}, openAuctionGate{}, sink)
	moc, err := svc.Submit(ctx, st.acct, auctionReq(TypeMOC))
	if err != nil {
		t.Fatalf("moc: %v", err)
	}
	lim, err := svc.Submit(ctx, st.acct, submitReq())
	if err != nil {
		t.Fatalf("limit: %v", err)
	}
	res, err := svc.MassCancel(ctx, MassCancelScope{
		AccountID: st.acct.ID, Reason: "AUCTION_UNFILLED_REMAINDER"},
		"user", "r", "ip")
	if err != nil {
		t.Fatalf("mass cancel: %v", err)
	}
	if res.Cancelled != 2 || st.orders[moc.OrderID].Status != "CANCELLED" ||
		st.orders[lim.OrderID].Status != "CANCELLED" {
		t.Fatalf("pre-freeze mass cancel: res=%+v", res)
	}
	if d := sink.last(ChanOrderCancelled); d == nil ||
		d["reason"] != ReasonAuctionCancelled {
		t.Fatalf("remainder sweep notification: %+v", sink.notes)
	}

	// Frozen: the queued order is skipped, the wire order cancels.
	st = newFakeStore()
	svc = auctionSvc(t, st, &fakeSubmitter{}, frozenAuctionGate{}, nil)
	moc, _ = svc.Submit(ctx, st.acct, auctionReq(TypeMOC))
	lim, _ = svc.Submit(ctx, st.acct, submitReq())
	res, err = svc.MassCancel(ctx, MassCancelScope{AccountID: st.acct.ID},
		"user", "r", "ip")
	if err != nil {
		t.Fatalf("frozen mass cancel: %v", err)
	}
	if res.Cancelled != 1 || st.orders[moc.OrderID].Status != "RESERVED" ||
		st.orders[lim.OrderID].Status != "CANCELLED" {
		t.Fatalf("frozen mass cancel: res=%+v moc=%+v lim=%+v",
			res, st.orders[moc.OrderID], st.orders[lim.OrderID])
	}
	// And the skipped row must not carry a fabricated CANCELLED audit.
	for _, a := range st.auditRows {
		if a.OrderID == moc.OrderID && a.NewValue == "CANCELLED" {
			t.Fatalf("frozen queued order audited as cancelled: %+v", a)
		}
	}
}

// ---- injector --------------------------------------------------------------

// decodeOrderNew unwraps a wire OrderNew event payload.
func decodeOrderNew(t *testing.T, payload []byte) *wire.OrderNew {
	t.Helper()
	ev := ipc.DecodeEvent(payload)
	if ev == nil || ev.TypeType() != wire.EventTypeOrderNew {
		t.Fatalf("not an OrderNew event")
	}
	var tbl flatbuffers.Table
	if !ev.Type(&tbl) {
		t.Fatal("OrderNew payload missing")
	}
	on := &wire.OrderNew{}
	on.Init(tbl.Bytes, tbl.Pos)
	return on
}

// The armed CALL injects exactly the scheduler-published queue ids as
// MARKET wire orders with the armed deadline stamped as the implicit
// auction GTD expiry (§6.2b); injected rows flip ACTIVE.
func TestInjectorQueueFilteredDispatch(t *testing.T) {
	st := newFakeStore()
	sub := &fakeSubmitter{}
	svc := auctionSvc(t, st, sub, openAuctionGate{}, nil)
	ctx := context.Background()

	mooAck, err := svc.Submit(ctx, st.acct, auctionReq(TypeMOO))
	if err != nil {
		t.Fatalf("moo submit: %v", err)
	}
	mocAck, err := svc.Submit(ctx, st.acct, auctionReq(TypeMOC))
	if err != nil {
		t.Fatalf("moc submit: %v", err)
	}
	if len(sub.sent) != 0 {
		t.Fatalf("submits dispatched: %d", len(sub.sent))
	}

	deadline := time.Now().Add(5 * time.Minute).UnixNano()
	feed := &armedFeed{
		armed: map[string]string{"EUR/USD": "CALL:" + strconv.FormatInt(deadline, 10)},
		queue: map[string][]int64{"EUR/USD": {mocAck.OrderID}},
	}
	in := NewInjector(svc, feed, 0, nil)
	in.sweep(ctx)

	if len(sub.sent) != 1 {
		t.Fatalf("injected %d orders, want exactly the queued id", len(sub.sent))
	}
	on := decodeOrderNew(t, sub.sent[0])
	if uint64(mocAck.OrderID) != on.OrderId() {
		t.Fatalf("injected order %d, want %d", on.OrderId(), mocAck.OrderID)
	}
	if on.Type() != wire.OrderTypeMarket {
		t.Fatalf("injected type %d, want MARKET", on.Type())
	}
	if on.Tif() != wire.TimeInForceGTD || on.GtdExpiryNs() != deadline {
		t.Fatalf("auction GTD stamp: tif=%d expiry=%d want GTD/%d",
			on.Tif(), on.GtdExpiryNs(), deadline)
	}
	if st.orders[mocAck.OrderID].Status != "ACTIVE" {
		t.Fatalf("injected row not ACTIVE: %+v", st.orders[mocAck.OrderID])
	}
	if st.orders[mooAck.OrderID].Status != "RESERVED" {
		t.Fatalf("non-queued MOO leaked into book: %+v", st.orders[mooAck.OrderID])
	}
	// Re-sweep: the injected row is engine-owned now — no re-send.
	in.sweep(ctx)
	if len(sub.sent) != 1 {
		t.Fatalf("re-sweep re-dispatched: %d sent", len(sub.sent))
	}
}

// Queue-less armed CALL (reopening after HALT/SUSPEND→ACTIVE) injects
// MOO only — MOC stays bound to the scheduled close queue per §6.2b.
func TestInjectorReopeningCallMOOOnly(t *testing.T) {
	st := newFakeStore()
	sub := &fakeSubmitter{}
	svc := auctionSvc(t, st, sub, openAuctionGate{}, nil)
	ctx := context.Background()

	mooAck, _ := svc.Submit(ctx, st.acct, auctionReq(TypeMOO))
	mocAck, _ := svc.Submit(ctx, st.acct, auctionReq(TypeMOC))

	feed := &armedFeed{
		armed: map[string]string{"EUR/USD": "CALL:" + strconv.FormatInt(time.Now().Add(time.Minute).UnixNano(), 10)},
		queue: map[string][]int64{},
	}
	NewInjector(svc, feed, 0, nil).sweep(ctx)

	if len(sub.sent) != 1 {
		t.Fatalf("reopening CALL dispatched %d, want MOO only", len(sub.sent))
	}
	on := decodeOrderNew(t, sub.sent[0])
	if on.OrderId() != uint64(mooAck.OrderID) {
		t.Fatalf("reopening CALL injected order %d, want MOO %d",
			on.OrderId(), mooAck.OrderID)
	}
	if st.orders[mocAck.OrderID].Status != "RESERVED" {
		t.Fatalf("MOC injected on queue-less CALL: %+v", st.orders[mocAck.OrderID])
	}
}

// No armed CALL → nothing injects.
func TestInjectorIdleWithoutArmedCall(t *testing.T) {
	st := newFakeStore()
	sub := &fakeSubmitter{}
	svc := auctionSvc(t, st, sub, openAuctionGate{}, nil)
	ctx := context.Background()
	if _, err := svc.Submit(ctx, st.acct, auctionReq(TypeMOC)); err != nil {
		t.Fatalf("submit: %v", err)
	}
	feed := &armedFeed{armed: map[string]string{}, queue: map[string][]int64{}}
	NewInjector(svc, feed, 0, nil).sweep(ctx)
	if len(sub.sent) != 0 {
		t.Fatalf("dispatched without armed CALL: %d", len(sub.sent))
	}
}

// ---- lifecycle notifications ------------------------------------------------

// Engine-cancelled auction orders (the scheduler's remainder sweep or a
// withdrawn CALL) surface as order.cancelled with AUCTION_CANCELLED;
// auction fills surface as order.auction_fill.
func TestAuctionLifecycleNotifications(t *testing.T) {
	st := newFakeStore()
	sink := &notifySink{}
	svc := auctionSvc(t, st, &fakeSubmitter{}, openAuctionGate{}, sink)
	ctx := context.Background()

	ack, err := svc.Submit(ctx, st.acct, auctionReq(TypeMOO))
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	o := st.orders[ack.OrderID]
	o.Status = "PARTIALLY_FILLED"
	o.FilledQty = decimal.MustFromString("1000")

	svc.OnFill(ctx, ack.OrderID,
		decimal.MustFromString("1.05"), decimal.MustFromString("1000"))
	d := sink.last(ChanOrderAuctionFill)
	if d == nil || d["order_id"] != ack.OrderID || d["price"] != "1.05" {
		t.Fatalf("auction_fill missing/malformed: %+v", sink.notes)
	}

	_ = st.ApplyCancel(ctx, ack.OrderID)
	svc.OnCancel(ctx, ack.OrderID, CancelReasonUser)
	d = sink.last(ChanOrderCancelled)
	if d == nil || d["reason"] != ReasonAuctionCancelled {
		t.Fatalf("cancelled notification: %+v", d)
	}
}
