package ws

import (
	"context"
	"encoding/json"
	"testing"

	"exchange/internal/orders"
)

// fakeOrderSvc records invocations for the adapter tests.
type fakeOrderSvc struct {
	cancelID     int64
	cancelActor  string
	cancelReqID  string
	cancelIP     string
	submitSessID string
	getID        int64
}

func (f *fakeOrderSvc) Submit(_ context.Context, _ *orders.Account, req *orders.SubmitRequest) (*orders.Ack, error) {
	f.submitSessID = req.SessionID
	return &orders.Ack{}, nil
}
func (f *fakeOrderSvc) Cancel(_ context.Context, _ *orders.Account, orderID int64, actor, requestID, ip string) (*orders.Ack, error) {
	f.cancelID, f.cancelActor, f.cancelReqID, f.cancelIP = orderID, actor, requestID, ip
	return &orders.Ack{}, nil
}
func (f *fakeOrderSvc) Modify(_ context.Context, _ *orders.Account, _ int64, _ *orders.ModifyRequest, _, _, _ string) (*orders.Order, error) {
	return &orders.Order{ID: 7}, nil
}
func (f *fakeOrderSvc) CancelReplace(_ context.Context, _ *orders.Account, _ int64, _ *orders.CancelReplaceRequest, _, _, _ string) (*orders.Order, error) {
	return &orders.Order{ID: 8}, nil
}
func (f *fakeOrderSvc) AmendKeepPriority(_ context.Context, _ *orders.Account, _ int64, _ *orders.KeepPriorityRequest, _, _, _ string) (*orders.Order, error) {
	return &orders.Order{ID: 9}, nil
}
func (f *fakeOrderSvc) BatchSubmit(_ context.Context, _ *orders.Account, reqs []*orders.SubmitRequest, _, _ string) ([]orders.BatchResult, error) {
	return make([]orders.BatchResult, len(reqs)), nil
}
func (f *fakeOrderSvc) DryRun(_ context.Context, _ *orders.Account, _ *orders.SubmitRequest) (*orders.Preview, error) {
	return &orders.Preview{}, nil
}
func (f *fakeOrderSvc) GetOrder(_ context.Context, _ *orders.Account, orderID int64) (*orders.Order, error) {
	f.getID = orderID
	return &orders.Order{ID: orderID}, nil
}

func okLookup(*orders.Account) AccountLookup {
	return func(context.Context, int64) (*orders.Account, error) {
		return &orders.Account{ID: 42, Status: "ACTIVE"}, nil
	}
}

func tradeSess() *Session {
	return &Session{Authenticated: true, AccountID: 42, Subject: "u-1",
		SessionID: "sess-9", RemoteIP: "203.0.113.7"}
}

func TestAdapterCancelRoutesAndStampsIdentity(t *testing.T) {
	svc := &fakeOrderSvc{}
	d := NewOrdersDispatcher(svc, okLookup(nil))
	res, err := d.Dispatch(withRequestID(context.Background(), "rid-1"),
		tradeSess(), "order.cancel", json.RawMessage(`{"order_id":55}`))
	if err != nil || res == nil || res.Status != "ACK" {
		t.Fatalf("dispatch: %v %+v", err, res)
	}
	if svc.cancelID != 55 {
		t.Fatalf("order_id: %d", svc.cancelID)
	}
	if svc.cancelActor != "account:42" || svc.cancelReqID != "rid-1" ||
		svc.cancelIP != "203.0.113.7" {
		t.Fatalf("audit fields: %+v", svc)
	}
}

func TestAdapterOrderIDFlexForms(t *testing.T) {
	for _, raw := range []string{
		`{"order_id":55}`, `{"order_id":"55"}`} {
		id, err := payloadOrderID(json.RawMessage(raw))
		if err != nil || id != 55 {
			t.Fatalf("parse %s: %d %v", raw, id, err)
		}
	}
	for _, raw := range []string{
		`{}`, `{"order_id":"abc"}`, `{"order_id":-1}`, `not json`} {
		if _, err := payloadOrderID(json.RawMessage(raw)); err == nil {
			t.Fatalf("bad order_id accepted: %s", raw)
		}
	}
}

func TestAdapterRequiresAccountBoundSession(t *testing.T) {
	svc := &fakeOrderSvc{}
	d := NewOrdersDispatcher(svc, okLookup(nil))
	_, err := d.Dispatch(context.Background(),
		&Session{Authenticated: true}, // AccountID 0
		"order.status", json.RawMessage(`{"order_id":1}`))
	if codeOf(err) != "UNAUTHORIZED" {
		t.Fatalf("anon-account order: %v", err)
	}
}

func TestAdapterFailsClosedWhenUnwired(t *testing.T) {
	d := NewOrdersDispatcher(nil, nil)
	_, err := d.Dispatch(context.Background(), tradeSess(),
		"order.status", json.RawMessage(`{"order_id":1}`))
	if codeOf(err) != "NOT_IMPLEMENTED" {
		t.Fatalf("unwired dispatch: %v", err)
	}
}

func TestAdapterAccountNotFound(t *testing.T) {
	d := NewOrdersDispatcher(&fakeOrderSvc{},
		func(context.Context, int64) (*orders.Account, error) { return nil, nil })
	_, err := d.Dispatch(context.Background(), tradeSess(),
		"order.status", json.RawMessage(`{"order_id":1}`))
	if codeOf(err) != "ACCOUNT_NOT_FOUND" {
		t.Fatalf("missing account: %v", err)
	}
}

func TestAdapterStatusAndBatch(t *testing.T) {
	svc := &fakeOrderSvc{}
	d := NewOrdersDispatcher(svc, okLookup(nil))

	res, err := d.Dispatch(context.Background(), tradeSess(),
		"order.status", json.RawMessage(`{"order_id":77}`))
	if err != nil || res.Status != "ACK" || svc.getID != 77 {
		t.Fatalf("status: %v %+v", err, res)
	}

	res, err = d.Dispatch(context.Background(), tradeSess(),
		"order.batch", json.RawMessage(
			`{"orders":[{"symbol":"EURUSD"},{"symbol":"USDJPY"}]}`))
	if err != nil || res.Status != "ACK" {
		t.Fatalf("batch: %v %+v", err, res)
	}
	// Empty batch array rejected.
	if _, err := d.Dispatch(context.Background(), tradeSess(),
		"order.batch", json.RawMessage(`{"orders":[]}`)); err == nil {
		t.Fatal("empty batch accepted")
	}
}
