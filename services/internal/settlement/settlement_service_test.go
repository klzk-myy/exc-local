package settlement

import (
	"context"
	stderrors "errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/shopspring/decimal"

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
}

func newFakeSettlementStore() *fakeSettlementStore {
	return &fakeSettlementStore{
		instruments: map[int64]InstrumentRef{},
		nostros:     map[string]NostroAccount{},
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

func (s *fakeSettlementStore) InTx(ctx context.Context, fn func(ctx context.Context, tx SettlementTx) error) error {
	return fn(ctx, fakeSettlementTx{s})
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
