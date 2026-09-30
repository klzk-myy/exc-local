// Package reporttest provides an in-memory reporting.Store for unit
// tests in reporting and the compliance adapters. It preserves the
// append-only + unique-key semantics the pg store enforces:
// (uti, regime, report_seq) and (event_id, destination, attempt) collide.
// Only test binaries link this package — production code never imports it.
package reporttest

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"time"

	"exchange/internal/compliance/reporting"
)

// FakeStore is an in-memory reporting.Store.
type FakeStore struct {
	Events    []*reporting.Event
	Subs      []*reporting.Submission
	Acks      []*reporting.Ack
	Breaks    []*reporting.Break
	Parties   map[int64]*reporting.PartyIdentifiers
	Trades    map[int64]*reporting.TradeContext
	Schemas   map[string]*reporting.SchemaVersion
	Positions []reporting.PositionSnapshot
	Limits    []*reporting.CFTCLimit

	eventSeq int64
	subSeq   int64
	ackSeq   int64
	brkSeq   int64
	now      time.Time
}

// NewFakeStore constructs the fake pinned at `now`.
func NewFakeStore(now time.Time) *FakeStore {
	return &FakeStore{
		Parties: map[int64]*reporting.PartyIdentifiers{},
		Trades:  map[int64]*reporting.TradeContext{},
		Schemas: map[string]*reporting.SchemaVersion{},
		now:     now,
	}
}

func schemaKey(regulation, schemaName string) string {
	return regulation + "|" + schemaName
}

// SeedSchemas registers the pinned rulesets mirroring migration 054.
func (f *FakeStore) SeedSchemas() {
	seed := func(reg, name, version string, fields ...string) {
		f.Schemas[schemaKey(reg, name)] = &reporting.SchemaVersion{
			SchemaID: int64(len(f.Schemas) + 1), Regulation: reg,
			SchemaName: name, Version: version, RulesRef: "test",
			RequiredFields: fields, EffectiveFrom: f.now.Add(-365 * 24 * time.Hour),
			Active: true, CreatedAt: f.now,
		}
	}
	seed(reporting.RegEMIR, reporting.SchemaEMIR, "2024-EMIR-REFIT",
		"uti", "upi", "counterparty1_lei", "counterparty2_id",
		"action_type", "event_type", "event_ts", "instrument_code",
		"price", "quantity", "notional", "currency", "venue_mic",
		"valuation", "margin")
	seed(reporting.RegCFTCP43, reporting.SchemaCFTCP43, "2022-PART43",
		"usi", "uti", "asset_class", "instrument_code", "price",
		"quantity", "currency", "event_ts", "venue_mic")
	seed(reporting.RegCFTCP45, reporting.SchemaCFTCP45, "2022-PART45",
		"uti", "usi", "upi", "prior_uti", "action_type", "event_type",
		"event_ts", "instrument_code", "price", "quantity", "currency",
		"counterparty1_lei", "counterparty2_id")
	seed(reporting.RegMIFID22, reporting.SchemaMIFID22, "RTS22-2017",
		"tvtc", "executing_entity_lei", "buyer_id", "seller_id",
		"trading_datetime", "venue_mic", "instrument_code", "price",
		"quantity", "currency", "decision_maker_id", "trader_id")
	seed(reporting.RegMIFIDAPA, reporting.SchemaMIFIDAPA, "RTS12-2017",
		"trade_id", "instrument_code", "price", "quantity", "currency",
		"trading_datetime", "venue_mic", "publication_datetime")
}

// OpenBreakTypes returns the set of open break types (assertion helper).
func (f *FakeStore) OpenBreakTypes() map[reporting.BreakType]bool {
	out := map[reporting.BreakType]bool{}
	for _, b := range f.Breaks {
		if b.Status == reporting.BreakOpen || b.Status == reporting.BreakRepairing {
			out[b.BreakType] = true
		}
	}
	return out
}

func (f *FakeStore) ResolveTrade(_ context.Context, id int64) (*reporting.TradeContext, error) {
	return f.Trades[id], nil
}
func (f *FakeStore) PartyFor(_ context.Context, acct int64) (*reporting.PartyIdentifiers, error) {
	return f.Parties[acct], nil
}
func (f *FakeStore) UpsertParty(_ context.Context, p reporting.PartyIdentifiers, _ int64, _ string) error {
	cp := p
	f.Parties[p.AccountID] = &cp
	return nil
}
func (f *FakeStore) InsertEvent(_ context.Context, e *reporting.Event) error {
	for _, x := range f.Events {
		if x.UTI == e.UTI && x.Regime == e.Regime && x.ReportSeq == e.ReportSeq {
			return fmt.Errorf("unique (uti,regime,seq) violation")
		}
	}
	f.eventSeq++
	cp := *e
	cp.EventID = f.eventSeq
	cp.CreatedAt = f.now
	f.Events = append(f.Events, &cp)
	e.EventID = cp.EventID
	e.CreatedAt = cp.CreatedAt
	return nil
}
func (f *FakeStore) EventByID(_ context.Context, id int64) (*reporting.Event, error) {
	for _, e := range f.Events {
		if e.EventID == id {
			return e, nil
		}
	}
	return nil, nil
}
func (f *FakeStore) LatestEvent(_ context.Context, uti string, regime reporting.Regime) (*reporting.Event, error) {
	var best *reporting.Event
	for _, e := range f.Events {
		if e.UTI == uti && e.Regime == regime && (best == nil || e.ReportSeq > best.ReportSeq) {
			best = e
		}
	}
	return best, nil
}
func (f *FakeStore) NextReportSeq(ctx context.Context, uti string, regime reporting.Regime) (int, error) {
	l, err := f.LatestEvent(ctx, uti, regime)
	if err != nil || l == nil {
		return 1, err
	}
	return l.ReportSeq + 1, nil
}
func (f *FakeStore) UTIOwner(_ context.Context, uti string) (int64, bool, error) {
	for _, e := range f.Events {
		if e.UTI == uti && e.TradeID != 0 {
			return e.TradeID, true, nil
		}
	}
	return 0, false, nil
}
func (f *FakeStore) SetEventStatus(_ context.Context, id int64, st reporting.EventStatus, verr json.RawMessage) error {
	for _, e := range f.Events {
		if e.EventID == id {
			e.Status = st
			e.ValidationErrors = verr
			return nil
		}
	}
	return fmt.Errorf("event %d not found", id)
}
func (f *FakeStore) ExportEvents(_ context.Context, regime reporting.Regime, from, to *time.Time, limit int) ([]reporting.Event, error) {
	var out []reporting.Event
	for _, e := range f.Events {
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
func (f *FakeStore) LatestEvents(_ context.Context, regime reporting.Regime) ([]reporting.Event, error) {
	byUTI := map[string]*reporting.Event{}
	for _, e := range f.Events {
		if e.Regime != regime {
			continue
		}
		if cur := byUTI[e.UTI]; cur == nil || e.ReportSeq > cur.ReportSeq {
			byUTI[e.UTI] = e
		}
	}
	var out []reporting.Event
	for _, e := range byUTI {
		out = append(out, *e)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].EventID < out[j].EventID })
	return out, nil
}
func (f *FakeStore) DuplicateNEWT(_ context.Context, regime reporting.Regime) ([]string, error) {
	n := map[string]int{}
	for _, e := range f.Events {
		if e.Regime == regime && e.Action == reporting.ActionNew {
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
func (f *FakeStore) CollidingUTIs(_ context.Context) ([]string, error) {
	m := map[string]map[int64]bool{}
	for _, e := range f.Events {
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
func (f *FakeStore) InsertSubmission(_ context.Context, s *reporting.Submission) error {
	for _, x := range f.Subs {
		if x.EventID == s.EventID && x.Destination == s.Destination && x.Attempt == s.Attempt {
			return fmt.Errorf("unique (event,dest,attempt) violation")
		}
	}
	f.subSeq++
	cp := *s
	cp.ReportSubmissionID = f.subSeq
	cp.CreatedAt = f.now
	f.Subs = append(f.Subs, &cp)
	s.ReportSubmissionID = cp.ReportSubmissionID
	s.CreatedAt = cp.CreatedAt
	return nil
}
func (f *FakeStore) SubmissionByID(_ context.Context, id int64) (*reporting.Submission, error) {
	for _, s := range f.Subs {
		if s.ReportSubmissionID == id {
			return s, nil
		}
	}
	return nil, nil
}
func (f *FakeStore) SubmissionsForEvent(_ context.Context, eventID int64) ([]reporting.Submission, error) {
	var out []reporting.Submission
	for _, s := range f.Subs {
		if s.EventID == eventID {
			out = append(out, *s)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Attempt < out[j].Attempt })
	return out, nil
}
func (f *FakeStore) PendingSubmissions(ctx context.Context, limit int) ([]reporting.Submission, error) {
	var out []reporting.Submission
	for _, s := range f.Subs {
		if s.Status != reporting.SubPending {
			continue
		}
		e, _ := f.EventByID(ctx, s.EventID)
		if e == nil || (e.Status != reporting.EventValidated && e.Status != reporting.EventSubmitted) {
			continue
		}
		out = append(out, *s)
	}
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}
func (f *FakeStore) MarkSubmissionDispatched(_ context.Context, id int64, at time.Time) error {
	for _, s := range f.Subs {
		if s.ReportSubmissionID == id {
			if s.Status != reporting.SubPending {
				return fmt.Errorf("submission %d not PENDING", id)
			}
			s.Status = reporting.SubSubmitted
			s.SubmittedAt = &at
			return nil
		}
	}
	return fmt.Errorf("submission %d not found", id)
}
func (f *FakeStore) IngestAckTx(_ context.Context, a reporting.Ack, subStatus reporting.SubmissionStatus,
	eventStatus reporting.EventStatus, nackBreak bool, slaDueAt time.Time) (*reporting.Break, error) {
	f.ackSeq++
	a.AckID = f.ackSeq
	a.ReceivedAt = f.now
	f.Acks = append(f.Acks, &a)
	for _, s := range f.Subs {
		if s.ReportSubmissionID == a.ReportSubmissionID {
			s.Status = subStatus
			s.ResolvedAt = &a.ReceivedAt
		}
	}
	for _, e := range f.Events {
		if e.EventID == a.EventID {
			e.Status = eventStatus
		}
	}
	if !nackBreak {
		return nil, nil
	}
	f.brkSeq++
	b := &reporting.Break{BreakID: f.brkSeq, EventID: a.EventID,
		BreakType: reporting.BreakNackRepair, Status: reporting.BreakOpen,
		DetectedBy: "ack", SLADueAt: slaDueAt, DetectedAt: f.now}
	f.Breaks = append(f.Breaks, b)
	return b, nil
}
func (f *FakeStore) AcksForEvent(_ context.Context, eventID int64) ([]reporting.Ack, error) {
	var out []reporting.Ack
	for _, a := range f.Acks {
		if a.EventID == eventID {
			out = append(out, *a)
		}
	}
	return out, nil
}
func (f *FakeStore) InsertBreak(_ context.Context, b *reporting.Break) error {
	f.brkSeq++
	b.BreakID = f.brkSeq
	b.Status = reporting.BreakOpen
	b.DetectedAt = f.now
	f.Breaks = append(f.Breaks, b)
	return nil
}
func (f *FakeStore) OpenBreaks(_ context.Context, regime reporting.Regime, limit int) ([]reporting.Break, error) {
	var out []reporting.Break
	for _, b := range f.Breaks {
		if b.Status != reporting.BreakOpen && b.Status != reporting.BreakRepairing {
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
func (f *FakeStore) ResolveBreakTx(_ context.Context, id int64, status reporting.BreakStatus,
	by int64, notes string, at time.Time, _ string) (bool, error) {
	for _, b := range f.Breaks {
		if b.BreakID == id {
			if b.Status == reporting.BreakResolved || b.Status == reporting.BreakWontFix {
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
func (f *FakeStore) ActiveSchema(_ context.Context, regulation, schemaName string, at time.Time) (*reporting.SchemaVersion, error) {
	sv := f.Schemas[schemaKey(regulation, schemaName)]
	if sv == nil || !sv.Active || sv.EffectiveFrom.After(at) {
		return nil, nil
	}
	return sv, nil
}
func (f *FakeStore) OpenDerivativePositions(_ context.Context) ([]reporting.PositionSnapshot, error) {
	return f.Positions, nil
}
func (f *FakeStore) ActiveLimitFor(_ context.Context, instrumentID int64, instrumentType, _ string, at time.Time) (*reporting.CFTCLimit, error) {
	for _, l := range f.Limits {
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
func (f *FakeStore) OpenQuantityFor(_ context.Context, accountID, instrumentID int64) (string, error) {
	for _, p := range f.Positions {
		if p.AccountID == accountID && p.InstrumentID == instrumentID {
			return p.Quantity, nil
		}
	}
	return "0", nil
}

var _ reporting.Store = (*FakeStore)(nil)
