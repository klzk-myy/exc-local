package settlement

import (
	"context"
	stderrors "errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/shopspring/decimal"

	"exchange/internal/ledger"
	excerrors "exchange/pkg/errors"
)

// ---------------------------------------------------------------------------
// In-memory fake store — SettlementStore + SettlementTx
// ---------------------------------------------------------------------------

type fakeSettlementStore struct {
	instruments map[int64]InstrumentRef
	nostros     map[string]NostroAccount // per-currency ACTIVE primary
	nextID      int64
	legs        []*SettlementInstruction
	movements   []NostroMovement
	// PD fee-confirm seam fixtures
	feeSpecs    map[int64]TradeFeeSpec
	postedKeys  map[string]bool
	journals    []ledger.Journal
	postErr     error
	feeSpecErr  error
	feeDedupErr error
}

func newFakeSettlementStore() *fakeSettlementStore {
	return &fakeSettlementStore{
		instruments: map[int64]InstrumentRef{},
		nostros:     map[string]NostroAccount{},
		feeSpecs:    map[int64]TradeFeeSpec{},
		postedKeys:  map[string]bool{},
		nextID:      1,
	}
}

func (s *fakeSettlementStore) addInstrument(id int64, symbol, base, quote string, cycle int) {
	s.instruments[id] = InstrumentRef{
		ID: id, Symbol: symbol, BaseCurrency: base, QuoteCurrency: quote, SettlementCycle: cycle,
	}
}

func (s *fakeSettlementStore) addNostro(id int64, ccy, bank, bic, acct, iban string) {
	s.nostros[ccy] = NostroAccount{
		ID: id, Currency: ccy, BankName: bank, BankCode: bic, AccountNumber: acct, IBAN: iban,
	}
}

func (s *fakeSettlementStore) nostroByID(id int64) NostroAccount {
	for _, n := range s.nostros {
		if n.ID == id {
			return n
		}
	}
	return NostroAccount{}
}

func (s *fakeSettlementStore) Instrument(_ context.Context, id int64) (InstrumentRef, error) {
	inst, ok := s.instruments[id]
	if !ok {
		return InstrumentRef{}, fmt.Errorf("instrument %d not found", id)
	}
	return inst, nil
}

func (s *fakeSettlementStore) ActiveNostroFor(_ context.Context, ccy string) (NostroAccount, error) {
	n, ok := s.nostros[ccy]
	if !ok {
		return NostroAccount{}, fmt.Errorf("no ACTIVE nostro for %s", ccy)
	}
	return n, nil
}

func legKey(tid, aid int64, ccy string, d SettlementDirection) string {
	return fmt.Sprintf("%d/%d/%s/%s", tid, aid, ccy, d)
}

func (s *fakeSettlementStore) InsertInstructions(_ context.Context, legs []SettlementInstruction) (int, error) {
	have := map[string]bool{}
	for _, l := range s.legs {
		have[legKey(l.TradeID, l.AccountID, l.Currency, l.Direction)] = true
	}
	inserted := 0
	for _, l := range legs {
		if have[legKey(l.TradeID, l.AccountID, l.Currency, l.Direction)] {
			continue
		}
		cp := l
		cp.ID = s.nextID
		s.nextID++
		cp.CreatedAt = time.Now().UTC()
		s.legs = append(s.legs, &cp)
		have[legKey(l.TradeID, l.AccountID, l.Currency, l.Direction)] = true
		inserted++
	}
	return inserted, nil
}

func (s *fakeSettlementStore) DueInstructions(_ context.Context, day time.Time) ([]DueLeg, error) {
	var out []DueLeg
	for _, l := range s.legs {
		if l.Status != SettlePending || l.SwiftMessageID != nil {
			continue
		}
		if normalizeDay(l.SettlementDate).After(normalizeDay(day)) {
			continue
		}
		var n NostroAccount
		if l.NostroAccountID != nil {
			n = s.nostroByID(*l.NostroAccountID)
		}
		out = append(out, DueLeg{Instruction: *l, Nostro: n})
	}
	return out, nil
}

func (s *fakeSettlementStore) MarkDispatched(_ context.Context, id int64, msgID string, format MessageFormat, payload string, _ time.Time) (bool, error) {
	for _, l := range s.legs {
		if l.ID == id {
			if l.SwiftMessageID != nil {
				return false, nil
			}
			l.SwiftMessageID = &msgID
			l.MessageFormat = &format
			l.MessagePayload = &payload
			return true, nil
		}
	}
	return false, fmt.Errorf("instruction %d not found", id)
}

func (s *fakeSettlementStore) QueueForNextCycle(_ context.Context, id int64, newDate time.Time, at time.Time) (bool, error) {
	for _, l := range s.legs {
		if l.ID == id {
			if l.Status != SettlePending || l.SwiftMessageID != nil {
				return false, nil
			}
			l.SettlementDate = newDate
			l.Status = SettleQueuedNextCycle
			_ = at
			return true, nil
		}
	}
	return false, fmt.Errorf("instruction %d not found", id)
}

func (s *fakeSettlementStore) ReleaseQueued(_ context.Context, day time.Time) (int, error) {
	n := 0
	for _, l := range s.legs {
		if l.Status == SettleQueuedNextCycle && !normalizeDay(l.SettlementDate).After(normalizeDay(day)) {
			l.Status = SettlePending
			n++
		}
	}
	return n, nil
}

func (s *fakeSettlementStore) InTx(ctx context.Context, fn func(ctx context.Context, tx SettlementTx) error) error {
	// Snapshot state so a failed fn rolls back — the production tx
	// guarantees SETTLED + nostro intent + fee journals are atomic; the
	// fake must prove the same or the atomicity test is hollow.
	saved := map[*SettlementInstruction]SettlementInstruction{}
	for _, l := range s.legs {
		saved[l] = *l
	}
	mLen, jLen := len(s.movements), len(s.journals)
	savedKeys := map[string]bool{}
	for k, v := range s.postedKeys {
		savedKeys[k] = v
	}
	err := fn(ctx, fakeSettlementTx{s})
	if err != nil {
		for p, v := range saved {
			*p = v
		}
		s.movements = s.movements[:mLen]
		s.journals = s.journals[:jLen]
		s.postedKeys = savedKeys
	}
	return err
}

func (s *fakeSettlementStore) leg(id int64) *SettlementInstruction {
	for _, l := range s.legs {
		if l.ID == id {
			return l
		}
	}
	return nil
}

type fakeSettlementTx struct{ s *fakeSettlementStore }

func (t fakeSettlementTx) LockInstruction(_ context.Context, id int64) (SettlementInstruction, bool, error) {
	l := t.s.leg(id)
	if l == nil {
		return SettlementInstruction{}, false, nil
	}
	return *l, true, nil
}

func (t fakeSettlementTx) SetSettled(_ context.Context, id int64, settledAt time.Time, ref string) error {
	l := t.s.leg(id)
	if l == nil || l.Status != SettlePending {
		return fmt.Errorf("instruction %d not pending", id)
	}
	l.Status = SettleSettled
	l.SettledAt = &settledAt
	if ref != "" {
		l.ConfirmationRef = &ref
	}
	return nil
}

func (t fakeSettlementTx) RecordNostroMovement(_ context.Context, m NostroMovement) error {
	t.s.movements = append(t.s.movements, m)
	return nil
}

func (t fakeSettlementTx) TradeFeeSpec(_ context.Context, tradeID int64) (TradeFeeSpec, bool, error) {
	if t.s.feeSpecErr != nil {
		return TradeFeeSpec{}, false, t.s.feeSpecErr
	}
	spec, ok := t.s.feeSpecs[tradeID]
	return spec, ok, nil
}

func (t fakeSettlementTx) FeeJournalPosted(_ context.Context, key string) (bool, error) {
	if t.s.feeDedupErr != nil {
		return false, t.s.feeDedupErr
	}
	return t.s.postedKeys[key], nil
}

func (t fakeSettlementTx) PostJournal(_ context.Context, j ledger.Journal) (ledger.PostResult, error) {
	if t.s.postErr != nil {
		return ledger.PostResult{}, t.s.postErr
	}
	if err := j.Validate(); err != nil {
		return ledger.PostResult{}, err
	}
	t.s.postedKeys[j.IdempotencyKey] = true
	t.s.journals = append(t.s.journals, j)
	return ledger.PostResult{JournalID: int64(len(t.s.journals))}, nil
}

// ---------------------------------------------------------------------------
// Fixtures
// ---------------------------------------------------------------------------

// newTestSettlementService builds a service over the shared holiday
// fixture with USD + EUR + GBP + JPY + CAD + MXN nostros and instruments
// mirroring the migration-001 seed cycles.
func newTestSettlementService(t *testing.T) (*SettlementService, *fakeSettlementStore, *NullDispatcher) {
	t.Helper()
	store := newFakeSettlementStore()
	// instruments mirroring migration 001 seeds
	store.addInstrument(1, "EUR/USD", "EUR", "USD", 1)
	store.addInstrument(2, "USD/CAD", "USD", "CAD", 0)
	store.addInstrument(3, "USD/MXN", "USD", "MXN", 0)
	store.addInstrument(4, "USD/JPY", "USD", "JPY", 1)
	store.addInstrument(5, "USD/JPY-T2", "USD", "JPY", 2) // T+2 exotic stand-in
	// nostros per settlement currency
	store.addNostro(101, "USD", "JPMorgan Chase", "CHASUS33", "US000111", "")
	store.addNostro(102, "EUR", "Deutsche Bank", "DEUTDEFF", "", "DE89370400440532013000")
	store.addNostro(103, "JPY", "MUFG Bank", "BOTKJPJT", "JP0222", "")
	store.addNostro(104, "CAD", "RBC", "ROYCCAT2", "CA0333", "")
	store.addNostro(105, "MXN", "Banxico", "BDMXMXMM", "MX0444", "")
	store.addNostro(106, "GBP", "Barclays", "BARCGB22", "GB0555", "")

	disp := &NullDispatcher{}
	svc, err := NewSettlementService(store, testCalendar(t), SettlementOptions{
		SenderBIC:  "EXCHGB2L",
		Dispatcher: disp,
	})
	if err != nil {
		t.Fatalf("NewSettlementService: %v", err)
	}
	return svc, store, disp
}

func eurusdFill(tradeID int64, tradeDate time.Time) SettlementFill {
	return SettlementFill{
		TradeID: tradeID, InstrumentID: 1,
		BuyerAccountID: 11, SellerAccountID: 22,
		Price: decimal.RequireFromString("1.0850"), Quantity: decimal.NewFromInt(100_000),
		TradeDate: tradeDate,
	}
}

// ---------------------------------------------------------------------------
// Task 3.3.3 — instruction generation
// ---------------------------------------------------------------------------

// T+1: EUR/USD trade on Monday settles Tuesday (spec §6.3).
func TestGenerateInstructionsT1MondayToTuesday(t *testing.T) {
	svc, _, _ := newTestSettlementService(t)
	res, err := svc.GenerateInstructions(context.Background(),
		eurusdFill(42, day(t, "2025-11-24"))) // Monday
	if err != nil {
		t.Fatalf("GenerateInstructions: %v", err)
	}
	if got := res.SettlementDate.Format("2006-01-02"); got != "2025-11-25" {
		t.Fatalf("settlement_date = %s, want 2025-11-25 (Tuesday)", got)
	}
	if len(res.Legs) != 4 || res.Inserted != 4 {
		t.Fatalf("legs=%d inserted=%d, want 4/4", len(res.Legs), res.Inserted)
	}
	// Leg semantics: buyer RECEIVE base / PAY quote; seller PAY base /
	// RECEIVE quote; every leg carries the currency's nostro account.
	type want struct {
		acct int64
		ccy  string
		dir  SettlementDirection
		amt  string
		nID  int64
	}
	wants := []want{
		{11, "EUR", DirectionReceive, "100000", 102},
		{11, "USD", DirectionPay, "108500", 101},
		{22, "EUR", DirectionPay, "100000", 102},
		{22, "USD", DirectionReceive, "108500", 101},
	}
	for i, w := range wants {
		l := res.Legs[i]
		if l.AccountID != w.acct || l.Currency != w.ccy || l.Direction != w.dir {
			t.Fatalf("leg %d = %d/%s/%s, want %d/%s/%s", i, l.AccountID, l.Currency, l.Direction, w.acct, w.ccy, w.dir)
		}
		if !l.Amount.Equal(decimal.RequireFromString(w.amt)) {
			t.Fatalf("leg %d amount = %s, want %s", i, l.Amount, w.amt)
		}
		if l.NostroAccountID == nil || *l.NostroAccountID != w.nID {
			t.Fatalf("leg %d nostro = %v, want %d", i, l.NostroAccountID, w.nID)
		}
		if l.Status != SettlePending {
			t.Fatalf("leg %d status = %s, want PENDING", i, l.Status)
		}
	}
}

// Same-day: USD/CAD settles on the trade date (spec §6.3).
func TestGenerateInstructionsSameDayUSDCAD(t *testing.T) {
	svc, _, _ := newTestSettlementService(t)
	f := eurusdFill(43, day(t, "2025-11-24"))
	f.InstrumentID = 2 // USD/CAD, settlement_cycle 0
	res, err := svc.GenerateInstructions(context.Background(), f)
	if err != nil {
		t.Fatalf("GenerateInstructions: %v", err)
	}
	if got := res.SettlementDate.Format("2006-01-02"); got != "2025-11-24" {
		t.Fatalf("USD/CAD settlement_date = %s, want same-day 2025-11-24", got)
	}
}

// Same-day USD/MXN (canonical spec §6.3 pair, cycle 0 in seed data).
func TestGenerateInstructionsSameDayUSDMXN(t *testing.T) {
	svc, _, _ := newTestSettlementService(t)
	f := eurusdFill(44, day(t, "2025-11-24"))
	f.InstrumentID = 3
	res, err := svc.GenerateInstructions(context.Background(), f)
	if err != nil {
		t.Fatalf("GenerateInstructions: %v", err)
	}
	if got := res.SettlementDate.Format("2006-01-02"); got != "2025-11-24" {
		t.Fatalf("USD/MXN settlement_date = %s, want 2025-11-24", got)
	}
}

// T+2 exotic: Monday trade settles Wednesday when both days are mutual
// business days.
func TestGenerateInstructionsT2(t *testing.T) {
	svc, _, _ := newTestSettlementService(t)
	f := eurusdFill(45, day(t, "2025-11-24"))
	f.InstrumentID = 5 // USD/JPY with cycle 2
	res, err := svc.GenerateInstructions(context.Background(), f)
	if err != nil {
		t.Fatalf("GenerateInstructions: %v", err)
	}
	if got := res.SettlementDate.Format("2006-01-02"); got != "2025-11-26" {
		t.Fatalf("T+2 settlement_date = %s, want 2025-11-26 (Wednesday)", got)
	}
}

// Weekend shift: EUR/USD Friday trade T+1 rolls to Monday.
func TestGenerateInstructionsWeekendShift(t *testing.T) {
	svc, _, _ := newTestSettlementService(t)
	res, err := svc.GenerateInstructions(context.Background(),
		eurusdFill(46, day(t, "2025-11-21"))) // Friday
	if err != nil {
		t.Fatalf("GenerateInstructions: %v", err)
	}
	if got := res.SettlementDate.Format("2006-01-02"); got != "2025-11-24" {
		t.Fatalf("Friday T+1 settlement_date = %s, want Monday 2025-11-24", got)
	}
}

// Holiday shift: USD/JPY trade Wed 2025-11-26 — nominal T+1 Thu 11-27 is
// US Thanksgiving → settlement Friday 11-28.
func TestGenerateInstructionsHolidayShift(t *testing.T) {
	svc, _, _ := newTestSettlementService(t)
	f := eurusdFill(47, day(t, "2025-11-26"))
	f.InstrumentID = 4 // USD/JPY T+1
	res, err := svc.GenerateInstructions(context.Background(), f)
	if err != nil {
		t.Fatalf("GenerateInstructions: %v", err)
	}
	if got := res.SettlementDate.Format("2006-01-02"); got != "2025-11-28" {
		t.Fatalf("holiday-shifted settlement_date = %s, want 2025-11-28", got)
	}
}

// Same-day Saturday trade rolls forward to Monday (Modified Following).
func TestGenerateInstructionsSameDayWeekendRoll(t *testing.T) {
	svc, _, _ := newTestSettlementService(t)
	f := eurusdFill(48, day(t, "2025-11-22")) // Saturday
	f.InstrumentID = 2
	res, err := svc.GenerateInstructions(context.Background(), f)
	if err != nil {
		t.Fatalf("GenerateInstructions: %v", err)
	}
	if got := res.SettlementDate.Format("2006-01-02"); got != "2025-11-24" {
		t.Fatalf("Saturday same-day settlement_date = %s, want Monday 2025-11-24", got)
	}
}

// Idempotent fill replay: a repeated fill inserts nothing and yields the
// same four legs.
func TestGenerateInstructionsIdempotentReplay(t *testing.T) {
	svc, store, _ := newTestSettlementService(t)
	ctx := context.Background()
	first, err := svc.GenerateInstructions(ctx, eurusdFill(42, day(t, "2025-11-24")))
	if err != nil {
		t.Fatalf("first generate: %v", err)
	}
	second, err := svc.GenerateInstructions(ctx, eurusdFill(42, day(t, "2025-11-24")))
	if err != nil {
		t.Fatalf("replay generate: %v", err)
	}
	if second.Inserted != 0 {
		t.Fatalf("replay inserted %d rows, want 0", second.Inserted)
	}
	if len(store.legs) != 4 {
		t.Fatalf("store holds %d legs after replay, want 4", len(store.legs))
	}
	if first.SettlementDate != second.SettlementDate {
		t.Fatalf("replay settlement date drifted: %s vs %s",
			first.SettlementDate, second.SettlementDate)
	}
}

// Fail-closed: no ACTIVE nostro for a settlement currency is an error,
// never a leg with NULL nostro.
func TestGenerateInstructionsMissingNostroFailsClosed(t *testing.T) {
	svc, store, _ := newTestSettlementService(t)
	delete(store.nostros, "USD")
	_, err := svc.GenerateInstructions(context.Background(),
		eurusdFill(49, day(t, "2025-11-24")))
	var ce *excerrors.Error
	if !stderrors.As(err, &ce) || ce.Code != CodeSettlementNostroMissing {
		t.Fatalf("err = %v, want code %s", err, CodeSettlementNostroMissing)
	}
	if len(store.legs) != 0 {
		t.Fatalf("wrote %d legs despite missing nostro", len(store.legs))
	}
}

// ---------------------------------------------------------------------------
// Dispatch — SWIFT MT202 / pacs.009 payloads (Phase-11 stub seam)
// ---------------------------------------------------------------------------

func TestDispatchDueMT202(t *testing.T) {
	svc, store, disp := newTestSettlementService(t)
	ctx := context.Background()
	if _, err := svc.GenerateInstructions(ctx, eurusdFill(42, day(t, "2025-11-24"))); err != nil {
		t.Fatalf("generate: %v", err)
	}
	rep, err := svc.DispatchDue(ctx, day(t, "2025-11-25")) // settlement date
	if err != nil {
		t.Fatalf("DispatchDue: %v", err)
	}
	if rep.Due != 4 || rep.Claimed != 4 || rep.Dispatched != 4 || len(rep.Errors) != 0 {
		t.Fatalf("report = %+v, want due/claimed/dispatched 4/4/4 no errors", rep)
	}
	// Inspect the USD PAY leg payload (instruction → message matched by id).
	var payLeg *SettlementInstruction
	for _, l := range store.legs {
		if l.Currency == "USD" && l.Direction == DirectionPay {
			payLeg = l
		}
	}
	var pay *OutboundMessage
	for i := range disp.Sent {
		m := &disp.Sent[i]
		if payLeg != nil && m.InstructionID == payLeg.ID {
			pay = m
		}
	}
	if pay == nil || !strings.Contains(pay.Payload, ":32A:251125USD108500\n") {
		t.Fatalf("no USD PAY MT202 payload with :32A:251125USD108500, among %+v", disp.Sent)
	}
	if !strings.Contains(pay.Payload, ":20:"+pay.MessageID) {
		t.Fatalf("payload missing :20: ref %q:\n%s", pay.MessageID, pay.Payload)
	}
	if !strings.Contains(pay.Payload, "{1:F01EXCHGB2LXXX") {
		t.Fatalf("payload missing sender BIC block:\n%s", pay.Payload)
	}
	if !strings.Contains(pay.Payload, "{2:I202CHASUS33XXX") {
		t.Fatalf("payload missing receiver BIC block:\n%s", pay.Payload)
	}
	if !strings.Contains(pay.Payload, ":21:TRD") {
		t.Fatalf("payload missing :21: trade reference:\n%s", pay.Payload)
	}
	if !strings.Contains(pay.Payload, ":52A:EXCHGB2LXXX") || !strings.Contains(pay.Payload, ":58A:") {
		t.Fatalf("payload missing :52A:/:58A: fields:\n%s", pay.Payload)
	}
	// Row claimed: message id + payload persisted, still PENDING until
	// bank confirmation.
	claimedLeg := payLeg
	if claimedLeg.SwiftMessageID == nil || claimedLeg.MessagePayload == nil {
		t.Fatalf("leg not marked dispatched: %+v", claimedLeg)
	}
	if *claimedLeg.SwiftMessageID != pay.MessageID {
		t.Fatalf("swift_message_id %q != message id %q", *claimedLeg.SwiftMessageID, pay.MessageID)
	}
	if claimedLeg.Status != SettlePending {
		t.Fatalf("dispatched leg status = %s, want PENDING until confirmation", claimedLeg.Status)
	}
	// Second pass: nothing left due.
	rep2, err := svc.DispatchDue(ctx, day(t, "2025-11-25"))
	if err != nil {
		t.Fatalf("DispatchDue 2: %v", err)
	}
	if rep2.Due != 0 {
		t.Fatalf("second dispatch due=%d, want 0 (idempotent claim)", rep2.Due)
	}
}

// Instructions not yet due stay pending/undispatched.
func TestDispatchDueFutureDateSkipped(t *testing.T) {
	svc, _, disp := newTestSettlementService(t)
	ctx := context.Background()
	if _, err := svc.GenerateInstructions(ctx, eurusdFill(42, day(t, "2025-11-24"))); err != nil {
		t.Fatalf("generate: %v", err)
	}
	rep, err := svc.DispatchDue(ctx, day(t, "2025-11-24")) // trade date — T+1 not yet due
	if err != nil {
		t.Fatalf("DispatchDue: %v", err)
	}
	if rep.Due != 0 || len(disp.Sent) != 0 {
		t.Fatalf("due=%d sent=%d, want 0/0 before settlement date", rep.Due, len(disp.Sent))
	}
}

// ISO 20022 pacs.009 rendering of the same leg.
func TestDispatchDuePacs009(t *testing.T) {
	svc, _, disp := newTestSettlementService(t)
	svc.format = FormatPacs009
	ctx := context.Background()
	if _, err := svc.GenerateInstructions(ctx, eurusdFill(42, day(t, "2025-11-24"))); err != nil {
		t.Fatalf("generate: %v", err)
	}
	if _, err := svc.DispatchDue(ctx, day(t, "2025-11-25")); err != nil {
		t.Fatalf("DispatchDue: %v", err)
	}
	if len(disp.Sent) != 4 {
		t.Fatalf("sent %d messages, want 4", len(disp.Sent))
	}
	var usd *OutboundMessage
	for i := range disp.Sent {
		if disp.Sent[i].Currency == "USD" {
			usd = &disp.Sent[i]
		}
	}
	if usd == nil {
		t.Fatal("no USD pacs.009 message")
	}
	p := usd.Payload
	for _, want := range []string{
		"pacs.009.001.08", "<MsgId>" + usd.MessageID + "</MsgId>",
		`<IntrBkSttlmAmt Ccy="USD">108500</IntrBkSttlmAmt>`,
		"<IntrBkSttlmDt>2025-11-25</IntrBkSttlmDt>",
		"<BICFI>CHASUS33XXX</BICFI>", "<BICFI>EXCHGB2LXXX</BICFI>",
	} {
		if !strings.Contains(p, want) {
			t.Fatalf("pacs.009 payload missing %q:\n%s", want, p)
		}
	}
}

// ---------------------------------------------------------------------------
// Confirmation → SETTLED + nostro movement intent
// ---------------------------------------------------------------------------

func TestConfirmSettlement(t *testing.T) {
	svc, store, _ := newTestSettlementService(t)
	ctx := context.Background()
	if _, err := svc.GenerateInstructions(ctx, eurusdFill(42, day(t, "2025-11-24"))); err != nil {
		t.Fatalf("generate: %v", err)
	}
	// Stored legs carry ids — find the buyer's USD PAY leg.
	var leg *SettlementInstruction
	for _, l := range store.legs {
		if l.Direction == DirectionPay && l.Currency == "USD" && l.AccountID == 11 {
			leg = l
		}
	}
	if leg == nil {
		t.Fatal("buyer USD PAY leg not found")
	}
	conf, err := svc.ConfirmSettlement(ctx, leg.ID, "CONF-ABC123")
	if err != nil {
		t.Fatalf("ConfirmSettlement: %v", err)
	}
	if conf.Status != SettleSettled || conf.SettledAt == nil {
		t.Fatalf("confirmed leg = status %s settled_at %v", conf.Status, conf.SettledAt)
	}
	if len(store.movements) != 1 {
		t.Fatalf("movements = %d, want 1", len(store.movements))
	}
	mv := store.movements[0]
	if mv.Direction != NostroDebit || mv.Status != MovementPending {
		t.Fatalf("PAY leg movement = %s/%s, want DEBIT/PENDING", mv.Direction, mv.Status)
	}
	if mv.NostroAccountID != 101 || mv.Currency != "USD" ||
		!mv.Amount.Equal(decimal.RequireFromString("108500")) || mv.ConfirmationRef != "CONF-ABC123" {
		t.Fatalf("movement = %+v", mv)
	}
	// RECEIVE leg → CREDIT movement.
	var recv *SettlementInstruction
	for _, l := range store.legs {
		if l.Direction == DirectionReceive && l.Currency == "USD" && l.AccountID == 22 {
			recv = l
		}
	}
	if _, err := svc.ConfirmSettlement(ctx, recv.ID, ""); err != nil {
		t.Fatalf("confirm receive: %v", err)
	}
	if store.movements[1].Direction != NostroCredit {
		t.Fatalf("RECEIVE leg movement = %s, want CREDIT", store.movements[1].Direction)
	}
	// Idempotent replay: re-confirm the settled leg, no new movement.
	again, err := svc.ConfirmSettlement(ctx, leg.ID, "CONF-ABC123")
	if err != nil {
		t.Fatalf("re-confirm: %v", err)
	}
	if again.Status != SettleSettled || len(store.movements) != 2 {
		t.Fatalf("re-confirm: status=%s movements=%d", again.Status, len(store.movements))
	}
}

func TestConfirmSettlementNotFound(t *testing.T) {
	svc, _, _ := newTestSettlementService(t)
	_, err := svc.ConfirmSettlement(context.Background(), 9999, "")
	var ce *excerrors.Error
	if !stderrors.As(err, &ce) || ce.Code != CodeSettlementNotFound {
		t.Fatalf("err = %v, want %s", err, CodeSettlementNotFound)
	}
}

func TestConfirmSettlementFailedLegConflicts(t *testing.T) {
	svc, store, _ := newTestSettlementService(t)
	ctx := context.Background()
	if _, err := svc.GenerateInstructions(ctx, eurusdFill(42, day(t, "2025-11-24"))); err != nil {
		t.Fatalf("generate: %v", err)
	}
	leg := store.legs[0]
	leg.Status = SettleFailed
	_, err := svc.ConfirmSettlement(ctx, leg.ID, "")
	var ce *excerrors.Error
	if !stderrors.As(err, &ce) || ce.Code != CodeSettlementStateConflict {
		t.Fatalf("err = %v, want %s", err, CodeSettlementStateConflict)
	}
}

// ---------------------------------------------------------------------------
// MT202 marshalling unit checks
// ---------------------------------------------------------------------------

func TestSwiftMT202MarshalFields(t *testing.T) {
	payload, err := SwiftMT202{
		SenderBIC:              "EXCHGB2L",
		ReceiverBIC:            "CHASUS33",
		TransactionRef:         "SI0000000000001",
		RelatedRef:             "TRD0000000000042",
		ValueDate:              day(t, "2025-11-25"),
		Currency:               "USD",
		Amount:                 decimal.RequireFromString("108500.25"),
		OrderingInstitutionBIC: "EXCHGB2L",
		BeneficiaryBIC:         "CHASUS33",
		BeneficiaryAccount:     "US000111",
	}.Marshal()
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	for _, want := range []string{
		"{1:F01EXCHGB2LXXX", "{2:I202CHASUS33XXXN}",
		":20:SI0000000000001", ":21:TRD0000000000042",
		":32A:251125USD108500,25", ":52A:EXCHGB2LXXX",
		":58A:/US000111\nCHASUS33XXX", "-}",
	} {
		if !strings.Contains(payload, want) {
			t.Fatalf("MT202 missing %q:\n%s", want, payload)
		}
	}
}

func TestSwiftMT202RejectsBadInput(t *testing.T) {
	if _, err := (SwiftMT202{
		SenderBIC: "bad", ReceiverBIC: "CHASUS33", TransactionRef: "X",
		Currency: "USD", Amount: decimal.NewFromInt(1), ValueDate: day(t, "2025-11-25"),
	}).Marshal(); err == nil {
		t.Fatal("expected invalid BIC error")
	}
	if _, err := (SwiftMT202{
		SenderBIC: "EXCHGB2L", ReceiverBIC: "CHASUS33", TransactionRef: "X",
		Currency: "USD", Amount: decimal.NewFromInt(-1), ValueDate: day(t, "2025-11-25"),
	}).Marshal(); err == nil {
		t.Fatal("expected negative amount error")
	}
}

// ---------------------------------------------------------------------------
// PD fee recognition at settlement confirm (spec §5.45.2)
// ---------------------------------------------------------------------------

// pdSpec is a physical-delivery trade 42 spec: buyer 11 pays a 0.5 EUR
// maker fee, seller 22 pays a 1.085 USD taker fee.
func pdSpec() TradeFeeSpec {
	return TradeFeeSpec{
		TradeID: 42, PhysicalDelivery: true,
		BuyerAccountID: 11, SellerAccountID: 22,
		BaseCurrency: "EUR", QuoteCurrency: "USD",
		BuyerFee:  decimal.RequireFromString("0.5"),
		SellerFee: decimal.RequireFromString("1.085"),
		BuyerRole: RoleMaker, SellerRole: RoleTaker,
	}
}

// confirmFirstLeg generates trade 42's four legs and confirms the first
// PENDING one, returning the leg.
func confirmFirstLeg(t *testing.T, svc *SettlementService, store *fakeSettlementStore) *SettlementInstruction {
	t.Helper()
	ctx := context.Background()
	if _, err := svc.GenerateInstructions(ctx, eurusdFill(42, day(t, "2025-11-24"))); err != nil {
		t.Fatalf("generate: %v", err)
	}
	leg := store.legs[0]
	if _, err := svc.ConfirmSettlement(ctx, leg.ID, "CONF-1"); err != nil {
		t.Fatalf("confirm leg %d: %v", leg.ID, err)
	}
	return leg
}

func TestConfirmSettlementPostsDeliveryFees(t *testing.T) {
	svc, store, _ := newTestSettlementService(t)
	store.feeSpecs[42] = pdSpec()
	leg := confirmFirstLeg(t, svc, store)

	if len(store.journals) != 2 {
		t.Fatalf("fee journals = %d, want 2", len(store.journals))
	}
	buyer := store.journals[0]
	if buyer.EntryType != ledger.EntryFee || buyer.PostedBy != "settlement-confirm" ||
		buyer.IdempotencyKey != "fee:42:11:MAKER" {
		t.Fatalf("buyer journal = %+v", buyer)
	}
	if len(buyer.Effects) != 1 || buyer.Effects[0].AccountID != 11 ||
		buyer.Effects[0].Currency != "EUR" ||
		!buyer.Effects[0].AvailableDelta.Equal(decimal.RequireFromString("-0.5")) ||
		!buyer.Effects[0].AllowNegative {
		t.Fatalf("buyer fee effect = %+v", buyer.Effects)
	}
	seller := store.journals[1]
	if seller.IdempotencyKey != "fee:42:22:TAKER" ||
		seller.Effects[0].AccountID != 22 || seller.Effects[0].Currency != "USD" ||
		!seller.Effects[0].AvailableDelta.Equal(decimal.RequireFromString("-1.085")) {
		t.Fatalf("seller journal = %+v", seller)
	}
	if leg.Status != SettleSettled || len(store.movements) != 1 {
		t.Fatalf("leg status %s, movements %d", leg.Status, len(store.movements))
	}
}

func TestConfirmSettlementFeesDedupAcrossLegs(t *testing.T) {
	svc, store, _ := newTestSettlementService(t)
	ctx := context.Background()
	store.feeSpecs[42] = pdSpec()
	confirmFirstLeg(t, svc, store)
	// Confirm the remaining three legs — the fee keys dedupe, so no
	// additional journals post.
	for _, l := range store.legs[1:] {
		if _, err := svc.ConfirmSettlement(ctx, l.ID, "CONF-N"); err != nil {
			t.Fatalf("confirm leg %d: %v", l.ID, err)
		}
	}
	if len(store.journals) != 2 {
		t.Fatalf("fee journals = %d, want 2 (sibling legs deduped)", len(store.journals))
	}
	if len(store.movements) != 4 {
		t.Fatalf("movements = %d, want 4 (one per leg)", len(store.movements))
	}
}

func TestConfirmSettlementRollingMarginSkipsFees(t *testing.T) {
	svc, store, _ := newTestSettlementService(t)
	spec := pdSpec()
	spec.PhysicalDelivery = false // RM fill — fees settled at execution
	store.feeSpecs[42] = spec
	confirmFirstLeg(t, svc, store)
	if len(store.journals) != 0 {
		t.Fatalf("RM confirm posted %d fee journals, want 0", len(store.journals))
	}
}

func TestConfirmSettlementZeroFeesPostNothing(t *testing.T) {
	svc, store, _ := newTestSettlementService(t)
	spec := pdSpec()
	spec.BuyerFee = decimal.Zero
	spec.SellerFee = decimal.Zero
	store.feeSpecs[42] = spec
	confirmFirstLeg(t, svc, store)
	if len(store.journals) != 0 {
		t.Fatalf("zero-fee confirm posted %d journals, want 0", len(store.journals))
	}
}

func TestConfirmSettlementFeePostFailureRollsBack(t *testing.T) {
	svc, store, _ := newTestSettlementService(t)
	ctx := context.Background()
	store.feeSpecs[42] = pdSpec()
	store.postErr = fmt.Errorf("ledger: insert journal_entries: boom")
	if _, err := svc.GenerateInstructions(ctx, eurusdFill(42, day(t, "2025-11-24"))); err != nil {
		t.Fatalf("generate: %v", err)
	}
	leg := store.legs[0]
	if _, err := svc.ConfirmSettlement(ctx, leg.ID, "CONF-1"); err == nil {
		t.Fatal("confirm with failing poster should error")
	}
	// Atomicity: the SETTLED flip and nostro intent rolled back with the
	// failed fee post.
	if leg.Status != SettlePending {
		t.Fatalf("leg status = %s, want PENDING (rollback)", leg.Status)
	}
	if len(store.movements) != 0 {
		t.Fatalf("movements = %d after rollback, want 0", len(store.movements))
	}
	// Retry with the poster healthy — confirm completes and fees book.
	store.postErr = nil
	if _, err := svc.ConfirmSettlement(ctx, leg.ID, "CONF-2"); err != nil {
		t.Fatalf("retry confirm: %v", err)
	}
	if leg.Status != SettleSettled || len(store.journals) != 2 {
		t.Fatalf("retry: status %s journals %d", leg.Status, len(store.journals))
	}
}

func TestConfirmSettlementDerivativeLegSkipsFees(t *testing.T) {
	svc, store, _ := newTestSettlementService(t)
	ctx := context.Background()
	store.feeSpecs[42] = pdSpec()
	if _, err := svc.GenerateInstructions(ctx, eurusdFill(42, day(t, "2025-11-24"))); err != nil {
		t.Fatalf("generate: %v", err)
	}
	cid := int64(77)
	store.legs[0].DerivativeContractID = &cid
	if _, err := svc.ConfirmSettlement(ctx, store.legs[0].ID, "CONF-1"); err != nil {
		t.Fatalf("confirm: %v", err)
	}
	if len(store.journals) != 0 {
		t.Fatalf("derivative leg posted %d fee journals, want 0", len(store.journals))
	}
}

func TestConfirmSettlementUnknownTradeSkipsFees(t *testing.T) {
	svc, store, _ := newTestSettlementService(t)
	// No feeSpec for trade 42 — the instruction still confirms (manual /
	// non-spot legs carry no fee model); recon owns the missing-trade
	// detection, the confirm must not brick on it.
	leg := confirmFirstLeg(t, svc, store)
	if leg.Status != SettleSettled || len(store.journals) != 0 {
		t.Fatalf("status %s journals %d", leg.Status, len(store.journals))
	}
}

// ---------------------------------------------------------------------------
// PostgreSQL integration (EXC_PG_TEST=1) — the PD fee-confirm seam against
// the real schema: partitioned trades FOR UPDATE, journal posting, and the
// SETTLED↔fee atomicity recon keys on.
// ---------------------------------------------------------------------------

func TestIntegrationConfirmSettlementDeliveryFees(t *testing.T) {
	pool := grossNetPool(t)
	ctx := context.Background()

	// Fixture: two test_scoped accounts (recon-invisible — the committed
	// ledger residue must never trip ExpectedFees/CollectedFees/
	// FeeRevenue), a SPOT instrument, one ACTIVE nostro, a PD trade
	// (settlement_date set) with per-side fees, and two PENDING legs.
	// The committed fixture is deliberately left in place: ledger rows are
	// append-only (FK-chained), and a coherent trade+legs+journals set is
	// the recon-clean residue — deleting the trade would orphan the
	// revenue legs into a fee_revenue_orphan finding.
	var buyer, seller, instrID int64
	if err := pool.QueryRow(ctx, `
		SELECT min(id), max(id) FROM accounts WHERE test_scoped`).Scan(&buyer, &seller); err != nil {
		t.Skipf("test-scoped accounts empty: %v", err)
	}
	if buyer == seller {
		t.Skip("need two distinct test-scoped accounts")
	}
	if err := pool.QueryRow(ctx,
		`SELECT id FROM instruments WHERE instrument_type='SPOT' LIMIT 1`).Scan(&instrID); err != nil {
		t.Skipf("instruments empty: %v", err)
	}
	var nostroID int64
	if err := pool.QueryRow(ctx, `
		INSERT INTO nostro_accounts (currency, bank_name, bank_code, account_number, status)
		VALUES ('USD','ITEST FeeBank','ITESTUS33','FEESEAM-TEST','ACTIVE')
		RETURNING id`).Scan(&nostroID); err != nil {
		t.Skipf("nostro fixture: %v", err)
	}
	var tradeID int64
	if err := pool.QueryRow(ctx, `
		INSERT INTO trades (instrument_id, buy_order_id, sell_order_id,
		                    buyer_account_id, seller_account_id, price, quantity,
		                    buyer_fee, seller_fee, settlement_date)
		VALUES ($1, 0, 0, $2, $3, 1.0850, 100000, 0.5, 1.085, CURRENT_DATE + 1)
		RETURNING id`, instrID, buyer, seller).Scan(&tradeID); err != nil {
		t.Skipf("trade fixture: %v", err)
	}
	var leg1, leg2 int64
	for i, ccy := range []string{"USD", "EUR"} {
		dir, acct := "PAY", buyer
		if i == 1 {
			dir, acct = "RECEIVE", seller
		}
		var id int64
		if err := pool.QueryRow(ctx, `
			INSERT INTO settlement_instructions
			    (trade_id, account_id, currency, amount, direction,
			     settlement_date, nostro_account_id, status)
			VALUES ($1,$2,$3,$4::numeric,$5::settlement_direction_enum,
			        CURRENT_DATE + 1, $6, 'PENDING') RETURNING id`,
			tradeID, acct, ccy, "1.0", dir, nostroID).Scan(&id); err != nil {
			t.Skipf("instruction fixture: %v", err)
		}
		if i == 0 {
			leg1 = id
		} else {
			leg2 = id
		}
	}
	// Cleanup removes the operational rows (instructions, nostro intent,
	// nostro) but deliberately leaves the trade + committed ledger rows:
	// ledger rows are append-only and FK-chained, and the trade must stay
	// for FeeRevenue's test_scoped exclusion to apply — deleting it would
	// orphan the revenue legs into a fee_revenue_orphan finding.
	t.Cleanup(func() {
		pool.Exec(ctx, `DELETE FROM nostro_movements WHERE settlement_instruction_id = ANY($1)`, []int64{leg1, leg2})
		pool.Exec(ctx, `DELETE FROM settlement_instructions WHERE id = ANY($1)`, []int64{leg1, leg2})
		pool.Exec(ctx, `DELETE FROM nostro_accounts WHERE id = $1`, nostroID)
	})

	store := NewPgxSettlementStore(pool)
	ledgerSvc, err := NewLedgerService(pool, nil, nil)
	if err != nil {
		t.Fatalf("ledger: %v", err)
	}
	store.Poster = ledgerSvc
	svc, err := NewSettlementService(store, testCalendar(t),
		SettlementOptions{SenderBIC: "EXCHGB2L"})
	if err != nil {
		t.Fatalf("service: %v", err)
	}

	var availBefore string
	if err := pool.QueryRow(ctx, `
		SELECT COALESCE((SELECT available::text FROM balances
		 WHERE account_id=$1 AND currency='EUR'), '0')`, buyer).Scan(&availBefore); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.ConfirmSettlement(ctx, leg1, "ICONF-1"); err != nil {
		t.Fatalf("confirm leg1: %v", err)
	}
	// Both fee journals committed with the SETTLED flip — recon's
	// ExpectedFees EXISTS-clause and collected legs see them atomically.
	var n int
	if err := pool.QueryRow(ctx, `
		SELECT count(*) FROM journal_entries
		 WHERE idempotency_key LIKE $1||':%'`, fmt.Sprintf("fee:%d", tradeID)).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("fee journals = %d, want 2", n)
	}
	// The debit hit the wallet: buyer owes 0.5 EUR (AllowNegative — PD
	// deliverable went off-exchange, the fee is a booked receivable).
	var avail string
	if err := pool.QueryRow(ctx, `
		SELECT available::text FROM balances WHERE account_id=$1 AND currency='EUR'`,
		buyer).Scan(&avail); err != nil {
		t.Fatal(err)
	}
	got := decimal.RequireFromString(avail)
	want := decimal.RequireFromString(availBefore).Sub(decimal.RequireFromString("0.5"))
	if !got.Equal(want) {
		t.Fatalf("buyer EUR available = %s, want %s (before %s − 0.5)", avail, want, availBefore)
	}
	// Second leg confirms: same-trade dedup on the fee keys.
	if _, err := svc.ConfirmSettlement(ctx, leg2, "ICONF-2"); err != nil {
		t.Fatalf("confirm leg2: %v", err)
	}
	if err := pool.QueryRow(ctx, `
		SELECT count(*) FROM journal_entries
		 WHERE idempotency_key LIKE $1||':%'`, fmt.Sprintf("fee:%d", tradeID)).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("after leg2: fee journals = %d, want 2", n)
	}
	// Re-confirm leg1 — idempotent, no duplicate movement or journal.
	if _, err := svc.ConfirmSettlement(ctx, leg1, "ICONF-1"); err != nil {
		t.Fatalf("re-confirm: %v", err)
	}
}

// TestIntegrationBatchTapeCarriesSettlementDate proves the production
// batch tape path persists the PD value date — recon's ExpectedFees and
// the confirm-time TradeFeeSpec both gate on settlement_date IS NOT NULL,
// so a batch-committed PD fill without it would never collect its fees.
func TestIntegrationBatchTapeCarriesSettlementDate(t *testing.T) {
	pool := grossNetPool(t)
	ctx := context.Background()

	var buyer, seller, instrID int64
	if err := pool.QueryRow(ctx, `
		SELECT min(id), max(id) FROM accounts WHERE test_scoped`).Scan(&buyer, &seller); err != nil {
		t.Skipf("test-scoped accounts empty: %v", err)
	}
	if buyer == seller {
		t.Skip("need two distinct test-scoped accounts")
	}
	if err := pool.QueryRow(ctx,
		`SELECT id FROM instruments WHERE instrument_type='SPOT' LIMIT 1`).Scan(&instrID); err != nil {
		t.Skipf("instruments empty: %v", err)
	}

	// Fund both deliverable wallets through real deposit journals — the
	// delivery lock debits available, and journal_sums/balances must stay
	// consistent on the shared dev DB.
	ledgerSvc, err := NewLedgerService(pool, nil, nil)
	if err != nil {
		t.Fatalf("ledger: %v", err)
	}
	qty := decimal.NewFromInt(100)
	px := decimal.RequireFromString("1.0850")
	qa := qty.Mul(px).Round(8)
	tradeID := int64(987654001)
	sd := time.Now().UTC().Truncate(24*time.Hour).AddDate(0, 0, 1)
	for _, dep := range []struct {
		acct int64
		ccy  string
		amt  decimal.Decimal
	}{{buyer, "USD", qa}, {seller, "EUR", qty}} {
		j := ledger.Journal{
			EntryType:      ledger.EntryDeposit,
			ReferenceID:    tradeID,
			Description:    "itest batch-tape funding",
			PostedBy:       "itest",
			IdempotencyKey: fmt.Sprintf("itest-dep:%d:%d", tradeID, dep.acct),
			Lines: []ledger.Line{
				ledger.DebitLine(ledger.Nostro(dep.ccy), dep.ccy, dep.amt, "deposit in"),
				ledger.CreditLine(ledger.CustomerLiability(dep.ccy), dep.ccy, dep.amt, "deposit"),
			},
			Effects: []ledger.AccountEffect{{
				AccountID: dep.acct, Currency: dep.ccy, AvailableDelta: dep.amt,
			}},
		}
		dtx, err := pool.Begin(ctx)
		if err != nil {
			t.Skipf("deposit tx: %v", err)
		}
		if _, err := ledgerSvc.PostJournal(ctx, dtx, j); err != nil {
			_ = dtx.Rollback(ctx)
			t.Skipf("deposit fixture: %v", err)
		}
		if err := dtx.Commit(ctx); err != nil {
			t.Skipf("deposit commit: %v", err)
		}
	}

	rt := ResolvedTrade{
		Fill: EngineFill{TradeID: uint64(tradeID), BuyOrderID: 9001, SellOrderID: 9002,
			Price: px, Qty: qty, ShardID: 0, EngineSeq: 1},
		InstrumentID:   instrID,
		BuyerAccountID: buyer, SellerAccountID: seller,
		BaseCurrency: "EUR", QuoteCurrency: "USD",
		BuyerFee:    decimal.RequireFromString("0.5"),
		SellerFee:   decimal.RequireFromString("0.1"),
		BuyerIntent: IntentPhysicalDelivery, SellerIntent: IntentPhysicalDelivery,
		SettlementDate: &sd,
	}
	fj, err := buildFillJournal(rt, "itest")
	if err != nil {
		t.Fatalf("buildFillJournal: %v", err)
	}
	svc := &BalanceService{}
	btx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	outcomes, _, err := svc.commitBatchSet(ctx, btx, []ResolvedTrade{rt}, [][]ledger.Journal{{fj}})
	if err != nil {
		_ = btx.Rollback(ctx)
		t.Fatalf("commitBatchSet: %v", err)
	}
	if err := btx.Commit(ctx); err != nil {
		t.Fatalf("batch commit: %v", err)
	}
	if len(outcomes) != 1 || !outcomes[0].Applied {
		t.Fatalf("outcome = %+v", outcomes[0])
	}
	var got time.Time
	if err := pool.QueryRow(ctx,
		`SELECT settlement_date FROM trades WHERE id=$1`, tradeID).Scan(&got); err != nil {
		t.Fatalf("read tape row: %v", err)
	}
	if !got.Equal(sd) {
		t.Fatalf("settlement_date = %v, want %v", got, sd)
	}
	// fees landed on the tape for the confirm-time collector
	var bf, sf string
	if err := pool.QueryRow(ctx,
		`SELECT buyer_fee::text, seller_fee::text FROM trades WHERE id=$1`, tradeID).
		Scan(&bf, &sf); err != nil {
		t.Fatal(err)
	}
	if bf != "0.50000000" || sf != "0.10000000" {
		t.Fatalf("taped fees = %s/%s, want 0.50000000/0.10000000", bf, sf)
	}
}
