// Task 24.3.14 unit tests — PB credit restitution on SETTLEMENT_FAILED:
// DSL restore + NOP reversal inside the settlement tx, one-time-per-event
// idempotency, replaced/allocated guards, post-commit deliveries.

package backoffice

import (
	"context"
	"fmt"
	"testing"
	"time"

	"exchange/pkg/decimal"
)

// ---------------------------------------------------------------------------

type soRestitStore struct {
	byInstr   map[int64]*Restitution
	giveups   map[int64]*GiveUpRow // tradeID
	replaced  map[int64]bool       // tradeID
	allocated map[int64]bool       // tradeID
	journals  []soPostedJournal
	delivered []Restitution
	nextID    int64
}

func newSoRestitStore() *soRestitStore {
	return &soRestitStore{
		byInstr: map[int64]*Restitution{}, giveups: map[int64]*GiveUpRow{},
		replaced: map[int64]bool{}, allocated: map[int64]bool{},
	}
}

func (s *soRestitStore) InTx(ctx context.Context,
	fn func(context.Context, RestitutionTx, Querier) error) error {
	return fn(ctx, s, nil) // raw Querier unused by fake adjusters
}

func (s *soRestitStore) RestitutionByInstruction(_ context.Context,
	id int64) (Restitution, bool, error) {
	if r, ok := s.byInstr[id]; ok {
		return *r, true, nil
	}
	return Restitution{}, false, nil
}

func (s *soRestitStore) MarkDelivered(_ context.Context, id int64, pb, margin bool) error {
	for _, r := range s.byInstr {
		if r.ID == id {
			r.PBNotified, r.MarginQueued = pb, margin
			s.delivered = append(s.delivered, *r)
			return nil
		}
	}
	return fmt.Errorf("not found")
}

func (s *soRestitStore) GiveUpForTrade(_ context.Context, tradeID int64) (GiveUpRow, bool, error) {
	if g, ok := s.giveups[tradeID]; ok {
		return *g, true, nil
	}
	return GiveUpRow{}, false, nil
}

func (s *soRestitStore) TradeReplaced(_ context.Context, tradeID, _ int64) (bool, error) {
	return s.replaced[tradeID], nil
}

func (s *soRestitStore) TradeAllocated(_ context.Context, tradeID int64) (bool, error) {
	return s.allocated[tradeID], nil
}

func (s *soRestitStore) InsertRestitution(_ context.Context, r Restitution) (Restitution, error) {
	if ex, ok := s.byInstr[r.InstructionID]; ok {
		return *ex, nil // concurrent insert dedupe
	}
	cp := r
	s.nextID++
	cp.ID = s.nextID
	cp.CreatedAt = time.Now()
	s.byInstr[cp.InstructionID] = &cp
	return cp, nil
}

func (s *soRestitStore) PostJournal(_ context.Context, entryType, description,
	postedBy, idemKey string, referenceID int64, lines []JournalLine) (int64, error) {
	s.journals = append(s.journals, soPostedJournal{
		entryType: entryType, description: description, idemKey: idemKey, lines: lines})
	return int64(len(s.journals)), nil
}

// ---------------------------------------------------------------------------

type soAdjCall struct {
	clientID int64
	pair     string
	dsl, nop decimal.Decimal
}

type soAdjuster struct{ calls []soAdjCall }

func (a *soAdjuster) RestituteInTx(_ context.Context, _ any, clientID int64,
	pair string, dsl, nop decimal.Decimal) (int, error) {
	a.calls = append(a.calls, soAdjCall{clientID: clientID, pair: pair, dsl: dsl, nop: nop})
	return 1, nil
}

type soMargin struct{ calls []int64 }

func (m *soMargin) Recalculate(_ context.Context, accountID int64) error {
	m.calls = append(m.calls, accountID)
	return nil
}

type soNotifier struct{ calls []int64 }

func (n *soNotifier) NotifyRestitution(_ context.Context, pbID, _ int64,
	_ map[string]string) error {
	n.calls = append(n.calls, pbID)
	return nil
}

type soRestitAlerter struct{ alerts []RestitutionAlert }

func (a *soRestitAlerter) RaiseRestitutionAlert(_ context.Context, al RestitutionAlert) error {
	a.alerts = append(a.alerts, al)
	return nil
}

func soRestitSvc(st *soRestitStore) (*RestitutionService, *soAdjuster, *soMargin, *soNotifier, *soRestitAlerter) {
	adj := &soAdjuster{}
	m := &soMargin{}
	n := &soNotifier{}
	al := &soRestitAlerter{}
	svc, err := NewRestitutionService(st, adj)
	if err != nil {
		panic(err)
	}
	svc.WithMargin(m).WithNotifier(n).WithAlerter(al)
	return svc, adj, m, n, al
}

func soFailEvent(instr, trade, client int64, dsl, nop string) SettlementFailedEvent {
	return SettlementFailedEvent{
		InstructionID: instr, TradeID: trade, ClientID: client,
		CurrencyPair: "EURUSD", Currency: "USD",
		DSLConsumedUSD: decimal.RequireFromString(dsl),
		NOPConsumedUSD: decimal.RequireFromString(nop),
		DetectedBy:     "exception-service",
	}
}

// ---------------------------------------------------------------------------

func TestRestitution_AppliesCountersJournalAndDeliveries(t *testing.T) {
	st := newSoRestitStore()
	st.giveups[55] = &GiveUpRow{ID: 9, TradeID: 55, PrimeBrokerID: 2,
		ClientAcctID: 42, Status: "AFFIRMED"}
	svc, adj, margin, notifier, alerter := soRestitSvc(st)

	rest, err := svc.HandleSettlementFailed(context.Background(),
		soFailEvent(10, 55, 0, "250000", "1000000"))
	if err != nil {
		t.Fatalf("handle: %v", err)
	}
	if rest.Status != "APPLIED" || rest.ClientID != 42 || rest.PrimeBrokerID == nil || *rest.PrimeBrokerID != 2 {
		t.Fatalf("bad restitution %+v", rest)
	}
	if rest.Reason != RestitutionReason {
		t.Fatalf("reason %s", rest.Reason)
	}
	// Counters adjusted inside the tx — DSL credit + NOP reversal.
	if len(adj.calls) != 1 || adj.calls[0].clientID != 42 ||
		adj.calls[0].dsl.String() != "250000" ||
		adj.calls[0].nop.String() != "1000000" {
		t.Fatalf("adjuster calls %+v", adj.calls)
	}
	// DSL memo journal landed.
	if len(st.journals) != 1 || st.journals[0].lines[0].AccountCode != "1090_SETTLEMENT_FAIL_MEMO_USD" {
		t.Fatalf("journal %+v", st.journals)
	}
	// Post-commit: margin recalc, PB drop-copy notice, admin alert,
	// delivery flags stamped.
	if len(margin.calls) != 1 || margin.calls[0] != 42 {
		t.Fatalf("margin not recalculated")
	}
	if len(notifier.calls) != 1 || notifier.calls[0] != 2 {
		t.Fatalf("pb not notified")
	}
	if len(alerter.alerts) != 1 || alerter.alerts[0].Code != CodeSettlementFailed {
		t.Fatalf("admin alert %+v", alerter.alerts)
	}
	if !rest.PBNotified || !rest.MarginQueued {
		t.Fatalf("delivery flags not stamped: %+v", rest)
	}
}

func TestRestitution_ReplayIsIdempotent(t *testing.T) {
	st := newSoRestitStore()
	svc, adj, _, _, _ := soRestitSvc(st)
	ev := soFailEvent(10, 55, 42, "100", "200")
	r1, err := svc.HandleSettlementFailed(context.Background(), ev)
	if err != nil {
		t.Fatalf("first: %v", err)
	}
	r2, err := svc.HandleSettlementFailed(context.Background(), ev)
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	if r2.ID != r1.ID || len(adj.calls) != 1 {
		t.Fatalf("restitution applied twice: %+v", adj.calls)
	}
	if len(st.journals) != 1 {
		t.Fatalf("journal duplicated")
	}
}

func TestRestitution_BlockedWhenReplacedOrAllocated(t *testing.T) {
	st := newSoRestitStore()
	st.replaced[56] = true
	st.allocated[57] = true
	svc, adj, _, _, _ := soRestitSvc(st)

	r, err := svc.HandleSettlementFailed(context.Background(), soFailEvent(10, 56, 42, "1", "1"))
	if errCode(err) != CodeRestitutionBlocked || r == nil || r.Status != "BLOCKED" {
		t.Fatalf("replaced: want BLOCKED + %s, got %v %+v", CodeRestitutionBlocked, err, r)
	}
	r, err = svc.HandleSettlementFailed(context.Background(), soFailEvent(11, 57, 42, "1", "1"))
	if errCode(err) != CodeRestitutionBlocked || r == nil || r.Status != "BLOCKED" {
		t.Fatalf("allocated: want BLOCKED + %s, got %v %+v", CodeRestitutionBlocked, err, r)
	}
	if len(adj.calls) != 0 {
		t.Fatalf("counters touched on blocked restitution")
	}
}

func TestRestitution_RequiresClientScope(t *testing.T) {
	st := newSoRestitStore()
	svc, _, _, _, _ := soRestitSvc(st)
	// No give-up row, no client on the event → cannot scope the counters.
	if _, err := svc.HandleSettlementFailed(context.Background(),
		soFailEvent(10, 55, 0, "1", "1")); errCode(err) != "INVALID_REQUEST" {
		t.Fatalf("want INVALID_REQUEST, got %v", err)
	}
	// No instruction/trade id → invalid.
	if _, err := svc.HandleSettlementFailed(context.Background(),
		SettlementFailedEvent{}); errCode(err) != "INVALID_REQUEST" {
		t.Fatalf("want INVALID_REQUEST, got %v", err)
	}
}

func TestRestitution_ConstructionFailsClosed(t *testing.T) {
	if _, err := NewRestitutionService(nil, &soAdjuster{}); errCode(err) != CodeServiceDegraded {
		t.Fatalf("nil store: want SERVICE_DEGRADED, got %v", err)
	}
	if _, err := NewRestitutionService(newSoRestitStore(), nil); errCode(err) != CodeServiceDegraded {
		t.Fatalf("nil adjuster: want SERVICE_DEGRADED, got %v", err)
	}
}
