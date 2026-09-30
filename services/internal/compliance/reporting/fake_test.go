// fakeStore — an in-memory reporting.Store for unit tests. Preserves
// the append-only + unique-key semantics the pg store enforces. The
// compliance package's tests use reporting/reporttest.FakeStore (same
// shape, exported) — this copy stays in-package because test-package
// reporting can't import reporttest (import cycle).
package reporting

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"time"
)

type fakeStore struct {
	events    []*Event
	subs      []*Submission
	acks      []*Ack
	breaks    []*Break
	parties   map[int64]*PartyIdentifiers
	trades    map[int64]*TradeContext
	schemas   map[string]*SchemaVersion
	positions []PositionSnapshot
	limits    []*CFTCLimit
	eventSeq  int64
	subSeq    int64
	ackSeq    int64
	brkSeq    int64
	now       time.Time
}

func newFakeStore(now time.Time) *fakeStore {
	return &fakeStore{
		parties: map[int64]*PartyIdentifiers{},
		trades:  map[int64]*TradeContext{},
		schemas: map[string]*SchemaVersion{},
		now:     now,
	}
}

func schemaKey(regulation, schemaName string) string { return regulation + "|" + schemaName }

// seedSchemas registers the pinned rulesets mirroring migration 054.
func (f *fakeStore) seedSchemas() {
	seed := func(reg, name, version string, fields ...string) {
		f.schemas[schemaKey(reg, name)] = &SchemaVersion{
			SchemaID: int64(len(f.schemas) + 1), Regulation: reg,
			SchemaName: name, Version: version, RulesRef: "test",
			RequiredFields: fields, EffectiveFrom: f.now.Add(-365 * 24 * time.Hour),
			Active: true, CreatedAt: f.now,
		}
	}
	seed(RegEMIR, SchemaEMIR, "2024-EMIR-REFIT",
		"uti", "upi", "counterparty1_lei", "counterparty2_id",
		"action_type", "event_type", "event_ts", "instrument_code",
		"price", "quantity", "notional", "currency", "venue_mic",
		"valuation", "margin")
	seed(RegCFTCP43, SchemaCFTCP43, "2022-PART43",
		"usi", "uti", "asset_class", "instrument_code", "price",
		"quantity", "currency", "event_ts", "venue_mic")
	seed(RegCFTCP45, SchemaCFTCP45, "2022-PART45",
		"uti", "usi", "upi", "prior_uti", "action_type", "event_type",
		"event_ts", "instrument_code", "price", "quantity", "currency",
		"counterparty1_lei", "counterparty2_id")
	seed(RegMIFID22, SchemaMIFID22, "RTS22-2017",
		"tvtc", "executing_entity_lei", "buyer_id", "seller_id",
		"trading_datetime", "venue_mic", "instrument_code", "price",
		"quantity", "currency", "decision_maker_id", "trader_id")
	seed(RegMIFIDAPA, SchemaMIFIDAPA, "RTS12-2017",
		"trade_id", "instrument_code", "price", "quantity", "currency",
		"trading_datetime", "venue_mic", "publication_datetime")
}

func (f *fakeStore) ResolveTrade(_ context.Context, id int64) (*TradeContext, error) {
	return f.trades[id], nil
}
func (f *fakeStore) PartyFor(_ context.Context, acct int64) (*PartyIdentifiers, error) {
	return f.parties[acct], nil
}
func (f *fakeStore) UpsertParty(_ context.Context, p PartyIdentifiers, _ int64, _ string) error {
	cp := p
	f.parties[p.AccountID] = &cp
	return nil
}
func (f *fakeStore) InsertEvent(_ context.Context, e *Event) error {
	for _, x := range f.events {
		if x.UTI == e.UTI && x.Regime == e.Regime && x.ReportSeq == e.ReportSeq {
			return fmt.Errorf("unique (uti,regime,seq) violation")
		}
	}
	f.eventSeq++
	cp := *e
	cp.EventID = f.eventSeq
	cp.CreatedAt = f.now
	f.events = append(f.events, &cp)
	e.EventID = cp.EventID
	e.CreatedAt = cp.CreatedAt
	return nil
}
func (f *fakeStore) EventByID(_ context.Context, id int64) (*Event, error) {
	for _, e := range f.events {
		if e.EventID == id {
			return e, nil
		}
	}
	return nil, nil
}
func (f *fakeStore) LatestEvent(_ context.Context, uti string, regime Regime) (*Event, error) {
	var best *Event
	for _, e := range f.events {
		if e.UTI == uti && e.Regime == regime && (best == nil || e.ReportSeq > best.ReportSeq) {
			best = e
		}
	}
	return best, nil
}
func (f *fakeStore) NextReportSeq(ctx context.Context, uti string, regime Regime) (int, error) {
	l, err := f.LatestEvent(ctx, uti, regime)
	if err != nil || l == nil {
		return 1, err
	}
	return l.ReportSeq + 1, nil
}
func (f *fakeStore) UTIOwner(_ context.Context, uti string) (int64, bool, error) {
	for _, e := range f.events {
		if e.UTI == uti && e.TradeID != 0 {
			return e.TradeID, true, nil
		}
	}
	return 0, false, nil
}
func (f *fakeStore) SetEventStatus(_ context.Context, id int64, st EventStatus, verr json.RawMessage) error {
	for _, e := range f.events {
		if e.EventID == id {
			e.Status = st
			e.ValidationErrors = verr
			return nil
		}
	}
	return fmt.Errorf("event %d not found", id)
}
func (f *fakeStore) ExportEvents(_ context.Context, regime Regime, from, to *time.Time, limit int) ([]Event, error) {
	var out []Event
	for _, e := range f.events {
		if e.Regime != regime {
			continue
		}
		if from != nil && e.EventTS.Before(*from) {
			continue
		}
		if to != nil && !e.EventTS.Before(*to) {
			continue
		}
		out = append(out, *e)
	}
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}
func (f *fakeStore) LatestEvents(_ context.Context, regime Regime) ([]Event, error) {
	byUTI := map[string]*Event{}
	for _, e := range f.events {
		if e.Regime != regime {
			continue
		}
		if cur := byUTI[e.UTI]; cur == nil || e.ReportSeq > cur.ReportSeq {
			byUTI[e.UTI] = e
		}
	}
	var out []Event
	for _, e := range byUTI {
		out = append(out, *e)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].EventID < out[j].EventID })
	return out, nil
}
func (f *fakeStore) DuplicateNEWT(_ context.Context, regime Regime) ([]string, error) {
	n := map[string]int{}
	for _, e := range f.events {
		if e.Regime == regime && e.Action == ActionNew {
			n[e.UTI]++
		}
	}
	var out []string
	for u, c := range n {
		if c > 1 {
			out = append(out, u)
		}
	}
	return out, nil
}
func (f *fakeStore) CollidingUTIs(_ context.Context) ([]string, error) {
	m := map[string]map[int64]bool{}
	for _, e := range f.events {
		if e.TradeID == 0 {
			continue
		}
		if m[e.UTI] == nil {
			m[e.UTI] = map[int64]bool{}
		}
		m[e.UTI][e.TradeID] = true
	}
	var out []string
	for u, ts := range m {
		if len(ts) > 1 {
			out = append(out, u)
		}
	}
	return out, nil
}
func (f *fakeStore) InsertSubmission(_ context.Context, s *Submission) error {
	for _, x := range f.subs {
		if x.EventID == s.EventID && x.Destination == s.Destination && x.Attempt == s.Attempt {
			return fmt.Errorf("unique (event,dest,attempt) violation")
		}
	}
	f.subSeq++
	cp := *s
	cp.ReportSubmissionID = f.subSeq
	cp.CreatedAt = f.now
	f.subs = append(f.subs, &cp)
	s.ReportSubmissionID = cp.ReportSubmissionID
	s.CreatedAt = cp.CreatedAt
	return nil
}
func (f *fakeStore) SubmissionByID(_ context.Context, id int64) (*Submission, error) {
	for _, s := range f.subs {
		if s.ReportSubmissionID == id {
			return s, nil
		}
	}
	return nil, nil
}
func (f *fakeStore) SubmissionsForEvent(_ context.Context, eventID int64) ([]Submission, error) {
	var out []Submission
	for _, s := range f.subs {
		if s.EventID == eventID {
			out = append(out, *s)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Attempt < out[j].Attempt })
	return out, nil
}
func (f *fakeStore) PendingSubmissions(ctx context.Context, limit int) ([]Submission, error) {
	var out []Submission
	for _, s := range f.subs {
		if s.Status != SubPending {
			continue
		}
		e, _ := f.EventByID(ctx, s.EventID)
		if e == nil || (e.Status != EventValidated && e.Status != EventSubmitted) {
			continue
		}
		out = append(out, *s)
	}
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}
func (f *fakeStore) MarkSubmissionDispatched(_ context.Context, id int64, at time.Time) error {
	for _, s := range f.subs {
		if s.ReportSubmissionID == id {
			if s.Status != SubPending {
				return fmt.Errorf("submission %d not PENDING", id)
			}
			s.Status = SubSubmitted
			s.SubmittedAt = &at
			return nil
		}
	}
	return fmt.Errorf("submission %d not found", id)
}
func (f *fakeStore) IngestAckTx(_ context.Context, a Ack, subStatus SubmissionStatus,
	eventStatus EventStatus, nackBreak bool, slaDueAt time.Time) (*Break, error) {
	f.ackSeq++
	a.AckID = f.ackSeq
	a.ReceivedAt = f.now
	f.acks = append(f.acks, &a)
	for _, s := range f.subs {
		if s.ReportSubmissionID == a.ReportSubmissionID {
			s.Status = subStatus
			s.ResolvedAt = &a.ReceivedAt
		}
	}
	for _, e := range f.events {
		if e.EventID == a.EventID {
			e.Status = eventStatus
		}
	}
	if !nackBreak {
		return nil, nil
	}
	f.brkSeq++
	b := &Break{BreakID: f.brkSeq, EventID: a.EventID,
		BreakType: BreakNackRepair, Status: BreakOpen, DetectedBy: "ack",
		SLADueAt: slaDueAt, DetectedAt: f.now}
	f.breaks = append(f.breaks, b)
	return b, nil
}
func (f *fakeStore) AcksForEvent(_ context.Context, eventID int64) ([]Ack, error) {
	var out []Ack
	for _, a := range f.acks {
		if a.EventID == eventID {
			out = append(out, *a)
		}
	}
	return out, nil
}
func (f *fakeStore) InsertBreak(_ context.Context, b *Break) error {
	f.brkSeq++
	b.BreakID = f.brkSeq
	b.Status = BreakOpen
	b.DetectedAt = f.now
	f.breaks = append(f.breaks, b)
	return nil
}
func (f *fakeStore) OpenBreaks(_ context.Context, regime Regime, limit int) ([]Break, error) {
	var out []Break
	for _, b := range f.breaks {
		if b.Status != BreakOpen && b.Status != BreakRepairing {
			continue
		}
		if regime != "" && b.Regime != regime {
			continue
		}
		out = append(out, *b)
	}
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}
func (f *fakeStore) ResolveBreakTx(_ context.Context, id int64, status BreakStatus,
	by int64, notes string, at time.Time, _ string) (bool, error) {
	for _, b := range f.breaks {
		if b.BreakID == id {
			if b.Status == BreakResolved || b.Status == BreakWontFix {
				return false, nil
			}
			b.Status = status
			b.ResolvedAt = &at
			b.ResolvedBy = by
			b.Notes = notes
			return true, nil
		}
	}
	return false, fmt.Errorf("break %d not found", id)
}
func (f *fakeStore) ActiveSchema(_ context.Context, regulation, schemaName string, at time.Time) (*SchemaVersion, error) {
	sv := f.schemas[schemaKey(regulation, schemaName)]
	if sv == nil || !sv.Active || sv.EffectiveFrom.After(at) {
		return nil, nil
	}
	return sv, nil
}
func (f *fakeStore) OpenDerivativePositions(_ context.Context) ([]PositionSnapshot, error) {
	return f.positions, nil
}
func (f *fakeStore) ActiveLimitFor(_ context.Context, instrumentID int64, instrumentType, _ string, at time.Time) (*CFTCLimit, error) {
	for _, l := range f.limits {
		if l.InstrumentType != instrumentType {
			continue
		}
		if l.InstrumentID != nil && *l.InstrumentID != instrumentID {
			continue
		}
		if l.EffectiveFrom.After(at) || (l.EffectiveTo != nil && l.EffectiveTo.Before(at)) {
			continue
		}
		return l, nil
	}
	return nil, nil
}
func (f *fakeStore) OpenQuantityFor(_ context.Context, accountID, instrumentID int64) (string, error) {
	for _, p := range f.positions {
		if p.AccountID == accountID && p.InstrumentID == instrumentID {
			return p.Quantity, nil
		}
	}
	return "0", nil
}
