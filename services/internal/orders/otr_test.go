// Task 13.3.6 — MiFID II RTS 9 OTR gate on the service seam: new orders
// reject with OTR_LIMIT_EXCEEDED while the account's breach flag stands;
// cancels stay exempt; every event class counts in the window.
package orders

import (
	"context"
	"testing"
	"time"

	"exchange/internal/config"
	excerrors "exchange/pkg/errors"
)

// fakeOtr records counted events and answers Admission per the breach flag.
type fakeOtr struct {
	breached bool
	events   []otrEvent
}

type otrEvent struct {
	accountID int64
	tier      string
	symbol    string
}

func (f *fakeOtr) Event(_ context.Context, accountID int64, tier, symbol string) {
	f.events = append(f.events, otrEvent{accountID, tier, symbol})
}

func (f *fakeOtr) Admission(_ context.Context, accountID int64) error {
	if f.breached {
		return excerrors.New("OTR_LIMIT_EXCEEDED",
			"order-to-trade ratio limit breached — cancels only")
	}
	return nil
}

func newSvcWithOtr(t *testing.T, st *fakeStore, gate *fakeOtr) *Service {
	t.Helper()
	shards, err := config.LoadShardMap("")
	if err != nil {
		t.Fatalf("shard map: %v", err)
	}
	sub := &fakeSubmitter{}
	svc, err := NewService(Options{
		Store: st, Submitter: sub, ShardMap: shards,
		KillSwitch: openKill{}, Otr: gate, Breakers: openBreakers{},
		AckTimeout: 200 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	sub.pending = svc.Pending()
	sub.store = st
	return svc
}

func TestOtr_SubmitRejectedWhenBreached(t *testing.T) {
	st := newFakeStore()
	gate := &fakeOtr{breached: true}
	svc := newSvcWithOtr(t, st, gate)
	_, err := svc.Submit(context.Background(), st.acct, submitReq())
	if codeOf(t, err) != "OTR_LIMIT_EXCEEDED" {
		t.Fatalf("breached admission must reject OTR_LIMIT_EXCEEDED, got %v", err)
	}
	// The event still counted — a breached account cannot spam-escape
	// the window by firing rejected orders.
	if len(gate.events) != 1 {
		t.Fatalf("rejected order must still count, events=%d", len(gate.events))
	}
}

func TestOtr_CancelSurvivesBreach(t *testing.T) {
	st := newFakeStore()
	gate := &fakeOtr{}
	svc := newSvcWithOtr(t, st, gate)
	ctx := context.Background()
	ack, err := svc.Submit(ctx, st.acct, submitReq())
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	// Breach raised — cancels must still flow AND count as events.
	gate.breached = true
	cack, err := svc.Cancel(ctx, st.acct, ack.OrderID, "tester", "r1", "127.0.0.1")
	if err != nil {
		t.Fatalf("cancel under breach must succeed: %v", err)
	}
	if cack.Status != "CANCELLED" {
		t.Fatalf("cancel ack: %+v", cack)
	}
	if len(gate.events) != 2 {
		t.Fatalf("submit+cancel must both count, events=%d", len(gate.events))
	}
	if gate.events[1].symbol != "EUR/USD" {
		t.Fatalf("cancel event symbol = %q", gate.events[1].symbol)
	}
}

func TestOtr_UnsetGateSkips(t *testing.T) {
	// No gate wired — orders flow; enforcement lives at the engine-side
	// flag (SuspensionFlags) and the documented nil-disable contract.
	st := newFakeStore()
	svc := newSvc(t, st, &fakeSubmitter{})
	if _, err := svc.Submit(context.Background(), st.acct, submitReq()); err != nil {
		t.Fatalf("nil gate must not block: %v", err)
	}
}
