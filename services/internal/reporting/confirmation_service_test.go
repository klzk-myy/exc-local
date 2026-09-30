package reporting

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// fakes
// ---------------------------------------------------------------------------

type fakeGen struct {
	generateErr error
	genCalls    int
	adjCalls    int
	recs        []ConfirmationRecord
}

func (f *fakeGen) Generate(_ context.Context, tradeID int64) ([]ConfirmationRecord, error) {
	f.genCalls++
	if f.generateErr != nil {
		return nil, f.generateErr
	}
	out := []ConfirmationRecord{}
	for _, r := range f.recs {
		r.TradeID = tradeID
		out = append(out, r)
	}
	return out, nil
}

func (f *fakeGen) MarkAdjusted(_ context.Context, tradeID int64) ([]ConfirmationRecord, error) {
	f.adjCalls++
	out := []ConfirmationRecord{}
	for _, r := range f.recs {
		r.TradeID = tradeID
		r.SupersedesID = &r.ConfirmationID
		r.Version = 2
		out = append(out, r)
	}
	return out, nil
}

type mapCat map[int64]string

func (m mapCat) Category(_ context.Context, a int64) (string, error) { return m[a], nil }

type mapEmail map[int64]string

func (m mapEmail) Email(_ context.Context, a int64) (string, error) { return m[a], nil }

type memDocs struct{ m map[string][]byte }

func (d memDocs) Get(_ context.Context, ref string) ([]byte, error) {
	b, ok := d.m[ref]
	if !ok {
		return nil, errors.New("missing object " + ref)
	}
	return b, nil
}

const testJSON = `{"type":"trade_confirmation","version":1,"trade_id":55,"account_id":7,` +
	`"symbol":"EUR/USD","side":"BUY","price":"1.0852","quantity":"100000",` +
	`"fee":"4.00","fee_currency":"USD","settlement_date":"2026-09-24",` +
	`"executed_at":"2026-09-22T10:15:00Z","generated_at":"2026-09-22T10:15:01Z","venue":"EXC.LOCAL"}`

func testDelivery(store *MemConfirmationStore, email *MemEmailSender, mt *MemMT515Submitter,
	cats CategorySource) *Delivery {
	d := &Delivery{
		Tracker: store, Docs: memDocs{m: map[string][]byte{
			"confirmations/7/55/v1.pdf":  []byte("%PDF-fake"),
			"confirmations/7/55/v1.json": []byte(testJSON),
		}},
		Email: email, Recipients: mapEmail{7: "c@example.com", 8: "p@example.com"},
		Categories: cats,
	}
	if mt != nil { // avoid a typed-nil interface
		d.MT515 = mt
	}
	return d
}

// ---------------------------------------------------------------------------
// OnFill / dispatch timing
// ---------------------------------------------------------------------------

func TestOnFill_GeneratesAndDispatchesInstitutional(t *testing.T) {
	store := NewMemConfirmationStore()
	id := store.Seed(ConfirmationRecord{
		TradeID: 55, AccountID: 7, Version: 1,
		FileRef: "confirmations/7/55/v1", GeneratedAt: time.Now().UTC().Add(-time.Second),
	})
	rec, _ := store.Row(id)
	email := &MemEmailSender{}
	mt := &MemMT515Submitter{}
	gen := &fakeGen{recs: []ConfirmationRecord{rec}}
	svc, err := NewConfirmationService(ServiceOptions{
		Generator: gen, Tracker: store,
		Delivery:   testDelivery(store, email, mt, mapCat{7: "PROFESSIONAL"}),
		Categories: mapCat{7: "PROFESSIONAL"},
	})
	if err != nil {
		t.Fatal(err)
	}
	recs, err := svc.OnFill(context.Background(), FillNotice{TradeID: 55, ExecutedAt: time.Now().UTC().Add(-2 * time.Second)})
	if err != nil {
		t.Fatalf("onfill: %v", err)
	}
	if len(recs) != 1 || gen.genCalls != 1 {
		t.Fatalf("gen calls=%d recs=%d", gen.genCalls, len(recs))
	}
	got, _ := store.Row(id)
	if got.Status != StatusDelivered || got.DeliveredAt == nil {
		t.Fatalf("expected DELIVERED, got %+v", got)
	}
	if len(email.Messages()) != 1 {
		t.Fatalf("emails=%d", len(email.Messages()))
	}
	em := email.Messages()[0]
	if em.AttachmentName != "confirmation-55-v1.pdf" || len(em.Attachment) == 0 {
		t.Fatalf("email attachment missing: %+v", em)
	}
	if !strings.Contains(em.BodyHTML, "Trade Confirmation") || !strings.Contains(em.BodyHTML, "EUR/USD") {
		t.Fatalf("email HTML not jurisdiction-rendered: %q", em.BodyHTML)
	}
	if len(mt.Bodies) != 1 {
		t.Fatalf("mt515 bodies=%d (institutional must emit)", len(mt.Bodies))
	}
	if svc.M.Delivered.Load() != 1 || svc.M.Generated.Load() != 1 {
		t.Fatalf("metrics delivered=%d generated=%d",
			svc.M.Delivered.Load(), svc.M.Generated.Load())
	}
}

func TestOnFill_RetailWaitsForT1(t *testing.T) {
	store := NewMemConfirmationStore()
	id := store.Seed(ConfirmationRecord{
		TradeID: 55, AccountID: 7, Version: 1, FileRef: "confirmations/7/55/v1",
		GeneratedAt: time.Now().UTC(),
	})
	rec, _ := store.Row(id)
	email := &MemEmailSender{}
	gen := &fakeGen{recs: []ConfirmationRecord{rec}}
	svc, _ := NewConfirmationService(ServiceOptions{
		Generator: gen, Tracker: store,
		Delivery:   testDelivery(store, email, nil, mapCat{7: "RETAIL"}),
		Categories: mapCat{7: "RETAIL"},
	})
	if _, err := svc.OnFill(context.Background(), FillNotice{TradeID: 55}); err != nil {
		t.Fatal(err)
	}
	got, _ := store.Row(id)
	if got.Status != StatusGenerated {
		t.Fatalf("retail must not immediate-deliver, got %s", got.Status)
	}
	if len(email.Messages()) != 0 {
		t.Fatal("email sent for retail before T+1")
	}
}

func TestOnFill_GenerationErrorPropagates(t *testing.T) {
	store := NewMemConfirmationStore()
	gen := &fakeGen{generateErr: errors.New("boom")}
	svc, _ := NewConfirmationService(ServiceOptions{Generator: gen, Tracker: store})
	if _, err := svc.OnFill(context.Background(), FillNotice{TradeID: 1}); err == nil {
		t.Fatal("expected error")
	}
	if svc.M.Errors.Load() != 1 {
		t.Fatal("error metric missing")
	}
}

func TestOnFill_SLAViolationCounted(t *testing.T) {
	store := NewMemConfirmationStore()
	gen := &fakeGen{}
	svc, _ := NewConfirmationService(ServiceOptions{Generator: gen, Tracker: store})
	old := time.Now().UTC().Add(-2 * time.Minute)
	if _, err := svc.OnFill(context.Background(), FillNotice{TradeID: 1, ExecutedAt: old}); err != nil {
		t.Fatal(err)
	}
	if svc.M.SLAViolations.Load() != 1 {
		t.Fatal("sla violation not counted")
	}
}

func TestOnTradeAmended_DispatchesImmediately(t *testing.T) {
	store := NewMemConfirmationStore()
	id := store.Seed(ConfirmationRecord{
		TradeID: 55, AccountID: 7, Version: 2, FileRef: "confirmations/7/55/v1",
		GeneratedAt: time.Now().UTC(),
	})
	rec, _ := store.Row(id)
	email := &MemEmailSender{}
	gen := &fakeGen{recs: []ConfirmationRecord{rec}}
	svc, _ := NewConfirmationService(ServiceOptions{
		Generator: gen, Tracker: store,
		// amendment must dispatch even for RETAIL — correction outranks T+1
		Delivery:   testDelivery(store, email, nil, mapCat{7: "RETAIL"}),
		Categories: mapCat{7: "RETAIL"},
	})
	recs, err := svc.OnTradeAmended(context.Background(), 55)
	if err != nil {
		t.Fatal(err)
	}
	if gen.adjCalls != 1 || len(recs) != 1 || *recs[0].SupersedesID == 0 {
		t.Fatalf("amend chain broken: %+v", recs)
	}
	if len(email.Messages()) != 1 {
		t.Fatal("amended confirmation not dispatched")
	}
	if svc.M.Adjusted.Load() != 1 {
		t.Fatal("adjusted metric missing")
	}
}

func TestNewConfirmationService_RequiresDeps(t *testing.T) {
	if _, err := NewConfirmationService(ServiceOptions{}); err == nil {
		t.Fatal("nil generator accepted")
	}
	if _, err := NewConfirmationService(ServiceOptions{Generator: &fakeGen{}}); err == nil {
		t.Fatal("nil tracker accepted")
	}
}
