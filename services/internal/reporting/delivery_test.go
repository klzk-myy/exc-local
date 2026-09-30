package reporting

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestDispatchDue(t *testing.T) {
	// 2026-09-22 is a Tuesday.
	tue := time.Date(2026, 9, 22, 10, 0, 0, 0, time.UTC)
	wed := time.Date(2026, 9, 23, 0, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name string
		cat  string
		at   time.Time
		want bool
	}{
		{"pro immediate", "PROFESSIONAL", tue, true},
		{"ecp immediate", "ELIGIBLE_COUNTERPARTY", tue, true},
		{"retail same day waits", "RETAIL", tue.Add(3 * time.Hour), false},
		{"retail next day due", "RETAIL", wed, true},
		{"retail after weekend", "RETAIL",
			time.Date(2026, 9, 28, 0, 0, 0, 0, time.UTC), true},
		{"unknown conservative", "", tue.Add(30 * time.Hour), true},
	} {
		gen := tue
		if tc.name == "retail after weekend" {
			gen = time.Date(2026, 9, 25, 10, 0, 0, 0, time.UTC) // Friday
		}
		if got := DispatchDue(tc.cat, gen, tc.at); got != tc.want {
			t.Errorf("%s: DispatchDue(%q) = %v, want %v", tc.name, tc.cat, got, tc.want)
		}
	}
}

func TestNextBusinessDay_SkipsWeekend(t *testing.T) {
	fri := time.Date(2026, 9, 25, 15, 0, 0, 0, time.UTC) // Friday
	got := NextBusinessDay(fri)
	if got.Weekday() != time.Monday || got.Hour() != 0 {
		t.Fatalf("got %s", got)
	}
	sat := NextBusinessDay(time.Date(2026, 9, 26, 9, 0, 0, 0, time.UTC))
	if sat.Weekday() != time.Monday {
		t.Fatalf("sat → %s", sat)
	}
}

func TestDelivery_DeliverAllChannels(t *testing.T) {
	store := NewMemConfirmationStore()
	id := store.Seed(ConfirmationRecord{
		TradeID: 55, AccountID: 7, Version: 1,
		FileRef: "confirmations/7/55/v1", GeneratedAt: time.Now().UTC(),
	})
	rec, _ := store.Row(id)
	email := &MemEmailSender{}
	mt := &MemMT515Submitter{}
	d := testDelivery(store, email, mt, mapCat{7: "ELIGIBLE_COUNTERPARTY"})
	if err := d.Deliver(context.Background(), rec); err != nil {
		t.Fatal(err)
	}
	got, _ := store.Row(id)
	if got.Status != StatusDelivered {
		t.Fatalf("status %s", got.Status)
	}
	if len(email.Messages()) != 1 || len(mt.Bodies) != 1 {
		t.Fatalf("emails=%d mt515=%d", len(email.Messages()), len(mt.Bodies))
	}
}

func TestDelivery_MT515SkippedForRetail(t *testing.T) {
	store := NewMemConfirmationStore()
	id := store.Seed(ConfirmationRecord{
		TradeID: 55, AccountID: 7, Version: 1,
		FileRef: "confirmations/7/55/v1", GeneratedAt: time.Now().UTC(),
	})
	rec, _ := store.Row(id)
	email := &MemEmailSender{}
	mt := &MemMT515Submitter{}
	d := testDelivery(store, email, mt, mapCat{7: "RETAIL"})
	if err := d.Deliver(context.Background(), rec); err != nil {
		t.Fatal(err)
	}
	if len(mt.Bodies) != 0 {
		t.Fatal("MT515 emitted for retail client")
	}
	if len(email.Messages()) != 1 {
		t.Fatal("email missing for retail client")
	}
}

func TestDelivery_EmailFailureLeavesGenerated(t *testing.T) {
	store := NewMemConfirmationStore()
	id := store.Seed(ConfirmationRecord{
		TradeID: 55, AccountID: 7, Version: 1,
		FileRef: "confirmations/7/55/v1", GeneratedAt: time.Now().UTC(),
	})
	rec, _ := store.Row(id)
	email := &MemEmailSender{FailErr: errors.New("smtp down")}
	d := testDelivery(store, email, nil, mapCat{7: "PROFESSIONAL"})
	if err := d.Deliver(context.Background(), rec); err == nil {
		t.Fatal("expected send failure")
	}
	got, _ := store.Row(id)
	if got.Status != StatusGenerated {
		t.Fatalf("row must stay GENERATED for retry, got %s", got.Status)
	}
}

func TestDelivery_NoRecipientSkipsEmail(t *testing.T) {
	store := NewMemConfirmationStore()
	id := store.Seed(ConfirmationRecord{
		TradeID: 55, AccountID: 99, Version: 1,
		FileRef: "confirmations/7/55/v1", GeneratedAt: time.Now().UTC(),
	})
	rec, _ := store.Row(id)
	d := testDelivery(store, &MemEmailSender{}, nil, mapCat{99: "RETAIL"})
	d.Recipients = mapEmail{} // unroutable account
	if err := d.Deliver(context.Background(), rec); err != nil {
		t.Fatalf("unroutable email must not block delivery: %v", err)
	}
}

type memDeadLetter struct{ n int }

func (m *memDeadLetter) DeadLetter(_ context.Context, _ ConfirmationRecord, _ int, _ error) {
	m.n++
}

func TestScheduler_T1SweepAndDeadLetter(t *testing.T) {
	store := NewMemConfirmationStore()
	// Friday fill — due Monday.
	fri := time.Date(2026, 9, 25, 15, 0, 0, 0, time.UTC)
	store.Seed(ConfirmationRecord{
		TradeID: 1, AccountID: 7, Version: 1, GeneratedAt: fri,
		FileRef: "confirmations/7/55/v1",
	})
	// Institutional fill generated in the past — due immediately.
	store.Seed(ConfirmationRecord{
		TradeID: 2, AccountID: 8, Version: 1, GeneratedAt: fri,
		FileRef: "confirmations/7/55/v1",
	})
	email := &MemEmailSender{}
	d := testDelivery(store, email, nil, mapCat{7: "RETAIL", 8: "PROFESSIONAL"})
	dl := &memDeadLetter{}
	s := NewDeliveryScheduler(store, d, mapCat{7: "RETAIL", 8: "PROFESSIONAL"})
	s.DeadLetters = dl
	s.Now = func() time.Time { return fri.Add(2 * time.Hour) } // still Friday

	sent, err := s.RunDue(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if sent != 1 {
		t.Fatalf("Friday sweep: sent=%d want 1 (only institutional)", sent)
	}

	s.Now = func() time.Time { return time.Date(2026, 9, 28, 1, 0, 0, 0, time.UTC) } // Monday
	sent, err = s.RunDue(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if sent != 1 {
		t.Fatalf("Monday sweep: sent=%d want 1 (retail T+1)", sent)
	}
	if len(email.Messages()) != 2 {
		t.Fatalf("emails=%d", len(email.Messages()))
	}

	// Dead-letter path: seed a due row, force send failure, run the
	// sweep MaxDispatchAttempts times.
	store.Seed(ConfirmationRecord{
		TradeID: 3, AccountID: 8, Version: 1, GeneratedAt: fri,
		FileRef: "confirmations/7/55/v1",
	})
	email.FailErr = errors.New("smtp down")
	for i := 0; i < MaxDispatchAttempts; i++ {
		if _, err := s.RunDue(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if dl.n != 1 {
		t.Fatalf("dead-letter count=%d want 1", dl.n)
	}
	// The row stays GENERATED for ops replay (never silently dropped).
	pending, err := store.PendingGenerated(context.Background(), 10)
	if err != nil || len(pending) != 1 {
		t.Fatalf("pending=%v err=%v", pending, err)
	}
}

func TestRenderConfirmationHTML_Jurisdictions(t *testing.T) {
	doc, err := parseConfirmationJSON([]byte(testJSON))
	if err != nil {
		t.Fatal(err)
	}
	rec := ConfirmationRecord{ConfirmationID: 9, TradeID: 55, AccountID: 7,
		Version: 1, Status: StatusGenerated, GeneratedAt: time.Now().UTC()}
	for _, jur := range []string{JurisdictionEU, JurisdictionUK, JurisdictionUS, "ZZ"} {
		out, err := RenderConfirmationHTML(rec, doc, jur, "")
		if err != nil {
			t.Fatalf("%s: %v", jur, err)
		}
		s := string(out)
		if !strings.Contains(s, "EUR/USD") || !strings.Contains(s, "1.0852") {
			t.Fatalf("%s: fields missing: %q", jur, s)
		}
	}
	// Configurable disclosure overrides the jurisdiction default.
	out, err := RenderConfirmationHTML(rec, doc, JurisdictionEU, "CUSTOM DISCLOSURE")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), "CUSTOM DISCLOSURE") {
		t.Fatal("disclosure override not rendered")
	}
}

func TestMT515Doc_DeterministicAndComplete(t *testing.T) {
	doc, err := parseConfirmationJSON([]byte(testJSON))
	if err != nil {
		t.Fatal(err)
	}
	prev := int64(41)
	rec := ConfirmationRecord{ConfirmationID: 9, TradeID: 55, AccountID: 7,
		Version: 2, SupersedesID: &prev,
		GeneratedAt: time.Date(2026, 9, 22, 10, 15, 1, 0, time.UTC)}
	a, b := MT515Doc(rec, doc), MT515Doc(rec, doc)
	if a != b {
		t.Fatal("non-deterministic")
	}
	for _, want := range []string{
		":16R:GENL", ":16S:GENL", ":16R:CONFDET", ":16S:CONFDET",
		"SEME//0000000000000055-2", ":23G::NEWM",
		"PREV//CONF0000000000000041", // supersedes linkage
		":SETT//20260924", "TRAD//DATI/20260922101500",
		"EXC.LOCAL", "BUYI", "1.0852", "CHAR//USD4.00",
	} {
		if !strings.Contains(a, want) {
			t.Errorf("MT515 missing %q:\n%s", want, a)
		}
	}
}
