package orders

// Phase-3 Task 5 (IMP-PLAN) — PB credit gate coverage: headroom reserves
// post-insert, releases on cancel/failure paths, re-sizes on amend.

import (
	"context"
	"errors"
	"testing"

	"exchange/pkg/decimal"
)

type stubPBGate struct {
	reserved  []int64
	released  []int64
	adjusted  []int64
	reserveE  error
	gotCcy    string
	gotNotion decimal.Decimal
}

func (s *stubPBGate) ReserveHeadroom(_ context.Context, orderID, _ int64,
	_, ccy string, notional decimal.Decimal) error {
	if s.reserveE != nil {
		return s.reserveE
	}
	s.reserved = append(s.reserved, orderID)
	s.gotCcy, s.gotNotion = ccy, notional
	return nil
}
func (s *stubPBGate) ReleaseHeadroom(_ context.Context, orderID int64) error {
	s.released = append(s.released, orderID)
	return nil
}
func (s *stubPBGate) AdjustHeadroom(_ context.Context, orderID int64,
	_, _ string, _ decimal.Decimal) error {
	s.adjusted = append(s.adjusted, orderID)
	return nil
}

func TestPBGateReserveOnSubmit(t *testing.T) {
	st := newFakeStore()
	sub := &fakeSubmitter{}
	pb := &stubPBGate{}
	svc := newSvc(t, st, sub)
	svc.WithPB(pb)

	ack, err := svc.Submit(context.Background(), st.acct, submitReq())
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	if len(pb.reserved) != 1 || pb.reserved[0] != ack.OrderID {
		t.Fatalf("reservation missing: %+v", pb.reserved)
	}
	// EUR/USD order → quote currency USD; notional = qty × limit price.
	if pb.gotCcy != "USD" {
		t.Fatalf("quote ccy %q, want USD", pb.gotCcy)
	}
	want := submitReq().Quantity.Mul(*submitReq().Price)
	if !pb.gotNotion.Equal(want) {
		t.Fatalf("notional %s, want %s", pb.gotNotion, want)
	}
}

func TestPBGateBreachRejectsRow(t *testing.T) {
	st := newFakeStore()
	sub := &fakeSubmitter{}
	svc := newSvc(t, st, sub)
	svc.WithPB(&stubPBGate{reserveE: errors.New("PB_NOP_LIMIT_EXCEEDED")})

	_, err := svc.Submit(context.Background(), st.acct, submitReq())
	if err == nil {
		t.Fatal("breach must surface")
	}
	for _, o := range st.orders {
		if o.Status != "REJECTED" {
			t.Fatalf("order left status %s after PB breach", o.Status)
		}
	}
	if len(sub.sent) != 0 {
		t.Fatal("breached order must not dispatch")
	}
}

func TestPBGateReleaseOnCancel(t *testing.T) {
	st := newFakeStore()
	sub := &fakeSubmitter{}
	pb := &stubPBGate{}
	svc := newSvc(t, st, sub)
	svc.WithPB(pb)

	ack, err := svc.Submit(context.Background(), st.acct, submitReq())
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	if _, err := svc.Cancel(context.Background(), st.acct, ack.OrderID,
		"user", "", ""); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	if len(pb.released) != 1 || pb.released[0] != ack.OrderID {
		t.Fatalf("release missing on cancel: %+v", pb.released)
	}
}

func TestPBGateAdjustOnQtyAmend(t *testing.T) {
	st := newFakeStore()
	sub := &fakeSubmitter{}
	pb := &stubPBGate{}
	svc := newSvc(t, st, sub)
	svc.WithPB(pb)

	ack, err := svc.Submit(context.Background(), st.acct, submitReq())
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	bigger := submitReq().Quantity.Mul(decimal.NewFromInt(2))
	seq := ack.OrderSeq
	_, err = svc.Modify(context.Background(), st.acct, ack.OrderID,
		&ModifyRequest{Quantity: &bigger, OrderSeq: &seq}, "t", "", "")
	if err != nil {
		t.Fatalf("modify: %v", err)
	}
	if len(pb.adjusted) != 1 || pb.adjusted[0] != ack.OrderID {
		t.Fatalf("amend re-size missing: %+v", pb.adjusted)
	}
}
