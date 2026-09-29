// Unit tests for the Phase-11 rails+returns cluster (Tasks 11.3.1 and
// 11.3.11): capability matrix + selection, per-rail envelope builders,
// return-code mapping + ApplyReturn, Jaro-Winkler, and the deposit guard.
// Pure unit level — a stub pgx.Tx drives the tx-shaped store calls;
// live-DB coverage lives in the integration test (EXC_PG_TEST gated).
package funding

import (
	"context"
	stderrors "errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"exchange/internal/ledger"
	"exchange/pkg/decimal"
	excerrors "exchange/pkg/errors"
)

// ---------------------------------------------------------------------------
// pgx.Tx stub — the repo convention defers tx-path tests to integration;
// the rails cluster has enough tx-path logic (quarantine + return-wire
// persistence) that a ~40-line stub earns its keep.
// ---------------------------------------------------------------------------

type stubTx struct{ committed, rolledBack bool }

func (t *stubTx) Begin(ctx context.Context) (pgx.Tx, error) { return t, nil }
func (t *stubTx) Commit(ctx context.Context) error {
	t.committed = true
	return nil
}
func (t *stubTx) Rollback(ctx context.Context) error {
	t.rolledBack = true
	return nil
}
func (t *stubTx) CopyFrom(ctx context.Context, tableName pgx.Identifier,
	columnNames []string, rowSrc pgx.CopyFromSource) (int64, error) {
	return 0, stderrors.New("stubTx: CopyFrom unsupported")
}
func (t *stubTx) SendBatch(ctx context.Context, b *pgx.Batch) pgx.BatchResults {
	return nil
}
func (t *stubTx) LargeObjects() pgx.LargeObjects { return pgx.LargeObjects{} }
func (t *stubTx) Prepare(ctx context.Context, name, sql string) (*pgconn.StatementDescription, error) {
	return nil, stderrors.New("stubTx: Prepare unsupported")
}
func (t *stubTx) Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	return pgconn.CommandTag{}, stderrors.New("stubTx: Exec unsupported")
}
func (t *stubTx) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	return nil, stderrors.New("stubTx: Query unsupported")
}
func (t *stubTx) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	return errRow{err: stderrors.New("stubTx: QueryRow unsupported")}
}
func (t *stubTx) Conn() *pgx.Conn { return nil }

type errRow struct{ err error }

func (r errRow) Scan(dest ...any) error { return r.err }

// ---------------------------------------------------------------------------
// Extended fake store for the Phase-11 paths
// ---------------------------------------------------------------------------

type p11Store struct {
	fakeStore
	tx             *stubTx
	railPayments   map[int64]*RailPaymentRow
	byEndToEnd     map[string]*RailPaymentRow
	suspense       map[int64]*SuspenseRow
	byBankTx       map[string]*SuspenseRow
	fundingTx      map[int64]*FundingTxRow
	deposits       map[int64]*DepositRow
	nextID         int64
	suspenseStatus []string
	railStatus     []string
}

func newP11Store() *p11Store {
	return &p11Store{
		fakeStore:    fakeStore{meta: map[int64]*AccountMeta{}},
		tx:           &stubTx{},
		railPayments: map[int64]*RailPaymentRow{},
		byEndToEnd:   map[string]*RailPaymentRow{},
		suspense:     map[int64]*SuspenseRow{},
		byBankTx:     map[string]*SuspenseRow{},
		fundingTx:    map[int64]*FundingTxRow{},
		deposits:     map[int64]*DepositRow{},
	}
}

func (s *p11Store) BeginTx(context.Context) (pgx.Tx, error) { return s.tx, nil }

func (s *p11Store) InsertRailPayment(_ context.Context, _ pgx.Tx, p RailPaymentRow) (*RailPaymentRow, error) {
	if _, dup := s.byEndToEnd[p.EndToEndID]; dup {
		return nil, ErrIdemConflict
	}
	s.nextID++
	cp := p
	cp.ID = s.nextID
	s.railPayments[cp.ID] = &cp
	s.byEndToEnd[cp.EndToEndID] = &cp
	return &cp, nil
}

func (s *p11Store) RailPaymentForUpdate(_ context.Context, _ pgx.Tx, id int64) (*RailPaymentRow, error) {
	if p, ok := s.railPayments[id]; ok {
		return p, nil
	}
	return nil, errCode("NOT_FOUND", "rail payment not found")
}

func (s *p11Store) RailPaymentByEndToEndID(_ context.Context, e2e string) (*RailPaymentRow, error) {
	if p, ok := s.byEndToEnd[e2e]; ok {
		return p, nil
	}
	return nil, errCode("NOT_FOUND", "rail payment not found")
}

func (s *p11Store) SetRailPaymentStatus(_ context.Context, _ pgx.Tx, id int64,
	status string, returnCode, returnReason *string,
	dispatchedAt, settledAt *time.Time) error {
	p, ok := s.railPayments[id]
	if !ok {
		return errCode("NOT_FOUND", "rail payment not found")
	}
	p.Status = status
	p.ReturnCode = returnCode
	p.ReturnReason = returnReason
	p.DispatchedAt = dispatchedAt
	p.SettledAt = settledAt
	s.railStatus = append(s.railStatus, status)
	return nil
}

func (s *p11Store) InsertSuspenseMapping(_ context.Context, _ pgx.Tx, m SuspenseRow) (*SuspenseRow, error) {
	if _, dup := s.byBankTx[m.BankTxID]; dup {
		return nil, ErrIdemConflict
	}
	s.nextID++
	cp := m
	cp.ID = s.nextID
	cp.QuarantinedAt = time.Now().UTC()
	s.suspense[cp.ID] = &cp
	s.byBankTx[cp.BankTxID] = &cp
	return &cp, nil
}

func (s *p11Store) SuspenseByBankTx(_ context.Context, bankTxID string) (*SuspenseRow, error) {
	if m, ok := s.byBankTx[bankTxID]; ok {
		return m, nil
	}
	return nil, errCode("NOT_FOUND", "suspense mapping not found")
}

func (s *p11Store) SuspenseForUpdate(_ context.Context, _ pgx.Tx, id int64) (*SuspenseRow, error) {
	if m, ok := s.suspense[id]; ok {
		return m, nil
	}
	return nil, errCode("NOT_FOUND", "suspense mapping not found")
}

func (s *p11Store) SetSuspenseStatus(_ context.Context, _ pgx.Tx, id int64, status string,
	investigatorID *int64, notes *string, resolvedAt *time.Time) error {
	m, ok := s.suspense[id]
	if !ok {
		return errCode("NOT_FOUND", "suspense mapping not found")
	}
	m.QuarantineStatus = status
	m.AssignedInvestigator = investigatorID
	m.ResolutionNotes = notes
	m.ResolvedAt = resolvedAt
	s.suspenseStatus = append(s.suspenseStatus, status)
	return nil
}

func (s *p11Store) SetSuspenseLinks(_ context.Context, _ pgx.Tx, id int64,
	journalID, returnPaymentID *int64) error {
	m, ok := s.suspense[id]
	if !ok {
		return errCode("NOT_FOUND", "suspense mapping not found")
	}
	if journalID != nil {
		m.JournalEntryID = journalID
	}
	if returnPaymentID != nil {
		m.ReturnPaymentID = returnPaymentID
	}
	return nil
}

func (s *p11Store) ListSuspense(_ context.Context, f SuspenseFilter) ([]SuspenseRow, int64, error) {
	var out []SuspenseRow
	for _, m := range s.suspense {
		if f.Status != "" && m.QuarantineStatus != f.Status {
			continue
		}
		out = append(out, *m)
	}
	return out, int64(len(out)), nil
}

func (s *p11Store) InsertDepositPending(_ context.Context, _ pgx.Tx, d DepositRow) (*DepositRow, error) {
	if d.IdempotencyKey != nil {
		for _, x := range s.deposits {
			if x.IdempotencyKey != nil && *x.IdempotencyKey == *d.IdempotencyKey {
				return nil, ErrIdemConflict
			}
		}
	}
	s.nextID++
	cp := d
	cp.ID = s.nextID
	s.deposits[cp.ID] = &cp
	s.fundingTx[cp.ID] = &FundingTxRow{
		ID: cp.ID, AccountID: cp.AccountID, Currency: cp.Currency,
		Type: "DEPOSIT", Amount: cp.Amount, Status: cp.Status,
	}
	return &cp, nil
}

func (s *p11Store) FundingTxForUpdate(_ context.Context, _ pgx.Tx, id int64) (*FundingTxRow, error) {
	if f, ok := s.fundingTx[id]; ok {
		return f, nil
	}
	return nil, errCode("NOT_FOUND", "funding transaction not found")
}

func (s *p11Store) SetFundingTxStatus(_ context.Context, _ pgx.Tx, id int64,
	status string, completedAt *time.Time) error {
	if f, ok := s.fundingTx[id]; ok {
		f.Status = status
		return nil
	}
	return errCode("NOT_FOUND", "funding transaction not found")
}

type fakeGate struct{ halted map[RailID]string }

func (g fakeGate) Available(_ context.Context, r RailID) (bool, string, error) {
	if reason, ok := g.halted[r]; ok {
		return false, reason, nil
	}
	return true, "", nil
}

type fakeEligibility struct {
	deny map[RailID]string
	err  error
}

func (e fakeEligibility) Eligible(_ context.Context, _ int64, r RailID) (bool, string, error) {
	if e.err != nil {
		return false, "", e.err
	}
	if reason, ok := e.deny[r]; ok {
		return false, reason, nil
	}
	return true, "", nil
}

type fakeAlerter struct{ alerts []OpsAlert }

func (a *fakeAlerter) Raise(_ context.Context, al OpsAlert) error {
	a.alerts = append(a.alerts, al)
	return nil
}

type fakeNames struct{ names map[int64]string }

func (n fakeNames) LegalName(_ context.Context, accountID int64) (string, error) {
	return n.names[accountID], nil
}

func dec(s string) decimal.Decimal { return decimal.RequireFromString(s) }

func newRailSvc(t *testing.T, store *p11Store) (*RailService, *fakePoster, *fakeAlerter) {
	t.Helper()
	fp := &fakePoster{}
	fa := &fakeAlerter{}
	svc, err := NewRailService(store, fp)
	if err != nil {
		t.Fatalf("NewRailService: %v", err)
	}
	svc.WithAlerter(fa)
	return svc, fp, fa
}

// ---------------------------------------------------------------------------
// Capability matrix
// ---------------------------------------------------------------------------

func TestRailMatrixComplete(t *testing.T) {
	m := RailMatrix()
	if len(m) != 6 {
		t.Fatalf("matrix must carry 6 rails, got %d", len(m))
	}
	for _, r := range AllRails {
		c, ok := m[r]
		if !ok {
			t.Fatalf("rail %s missing from matrix", r)
		}
		if c.Rail != r {
			t.Fatalf("matrix[%s].Rail = %s", r, c.Rail)
		}
	}
	// Currency coverage per spec §17.2.
	if !m[RailSWIFT].AllCurrencies {
		t.Fatal("SWIFT must be all-currencies")
	}
	for _, tc := range []struct {
		rail RailID
		ccy  string
	}{
		{RailSEPA, "EUR"}, {RailFedNow, "USD"}, {RailACH, "USD"},
		{RailCHAPS, "GBP"}, {RailTARGET2, "EUR"},
	} {
		if !m[tc.rail].Supports(tc.ccy) {
			t.Fatalf("%s must support %s", tc.rail, tc.ccy)
		}
	}
	if m[RailFedNow].Supports("EUR") || m[RailACH].Supports("GBP") {
		t.Fatal("USD rails must not support foreign currencies")
	}
}

func TestRailSelectionByCurrency(t *testing.T) {
	svc, _, _ := newRailSvc(t, newP11Store())
	at := time.Date(2026, 11, 9, 10, 0, 0, 0, time.UTC) // Monday 10:00 UTC

	cases := []struct {
		ccy  string
		want RailID
	}{
		{"USD", RailFedNow},  // instant USD before batch ACH/SWIFT
		{"EUR", RailTARGET2}, // RTGS before SEPA
		{"GBP", RailCHAPS},
		{"JPY", RailSWIFT}, // only SWIFT is all-currencies
	}
	for _, tc := range cases {
		sel, err := svc.Select(context.Background(), SelectionRequest{
			Currency: tc.ccy, Amount: dec("1000"), At: at,
		})
		if err != nil {
			t.Fatalf("select %s: %v", tc.ccy, err)
		}
		if sel.Rail != tc.want {
			t.Fatalf("ccy %s → %s, want %s", tc.ccy, sel.Rail, tc.want)
		}
	}
}

func TestRailSelectionPreferredAndUnavailable(t *testing.T) {
	svc, _, _ := newRailSvc(t, newP11Store())
	at := time.Date(2026, 11, 9, 10, 0, 0, 0, time.UTC)

	// Preferred ACH for USD beats the FedNow default.
	sel, err := svc.Select(context.Background(), SelectionRequest{
		Currency: "USD", Amount: dec("500"), Preferred: RailACH, At: at,
	})
	if err != nil || sel.Rail != RailACH {
		t.Fatalf("preferred ACH: sel=%+v err=%v", sel, err)
	}

	// Halt every rail — fail closed.
	svc.WithGate(fakeGate{halted: map[RailID]string{
		RailSWIFT: "maintenance", RailSEPA: "halt", RailFedNow: "halt",
		RailACH: "halt", RailCHAPS: "halt", RailTARGET2: "halt",
	}})
	_, err = svc.Select(context.Background(), SelectionRequest{
		Currency: "USD", Amount: dec("500"), At: at,
	})
	if codeOf(err) != CodeBankingRailUnavailable {
		t.Fatalf("want BANKING_RAIL_UNAVAILABLE, got %v (%v)", codeOf(err), err)
	}
}

func TestRailSelectionGateSingleHaltFallsThrough(t *testing.T) {
	svc, _, _ := newRailSvc(t, newP11Store())
	// Halt only FedNow — USD must fall through to ACH.
	svc.WithGate(fakeGate{halted: map[RailID]string{RailFedNow: "instant outage"}})
	sel, err := svc.Select(context.Background(), SelectionRequest{
		Currency: "USD", Amount: dec("100"), At: time.Date(2026, 11, 9, 10, 0, 0, 0, time.UTC),
	})
	if err != nil || sel.Rail != RailACH {
		t.Fatalf("want ACH fallback, got %+v err=%v", sel, err)
	}
}

func TestRailSelectionEligibility(t *testing.T) {
	svc, _, _ := newRailSvc(t, newP11Store())
	svc.WithEligibility(fakeEligibility{deny: map[RailID]string{RailFedNow: "account not FedNow-enabled"}})
	sel, err := svc.Select(context.Background(), SelectionRequest{
		AccountID: 42, Currency: "USD", Amount: dec("100"),
		At: time.Date(2026, 11, 9, 10, 0, 0, 0, time.UTC),
	})
	if err != nil || sel.Rail != RailACH {
		t.Fatalf("want ACH after eligibility deny, got %+v err=%v", sel, err)
	}

	// Deny everything → BANKING_RAIL_UNAVAILABLE.
	svc.WithEligibility(fakeEligibility{deny: map[RailID]string{
		RailSWIFT: "x", RailSEPA: "x", RailFedNow: "x",
		RailACH: "x", RailCHAPS: "x", RailTARGET2: "x",
	}})
	_, err = svc.Select(context.Background(), SelectionRequest{
		AccountID: 42, Currency: "USD", Amount: dec("100"),
		At: time.Date(2026, 11, 9, 10, 0, 0, 0, time.UTC),
	})
	if codeOf(err) != CodeBankingRailUnavailable {
		t.Fatalf("want BANKING_RAIL_UNAVAILABLE, got %v", codeOf(err))
	}
}

func TestRailCutoffSameDay(t *testing.T) {
	svc, _, _ := newRailSvc(t, newP11Store())
	// 22:00 UTC Monday — past every cut-off except weekend rails.
	late := time.Date(2026, 11, 9, 22, 0, 0, 0, time.UTC)
	sel, err := svc.Select(context.Background(), SelectionRequest{
		Currency: "GBP", Amount: dec("1000"), At: late,
	})
	if err != nil {
		t.Fatalf("CHAPS after cutoff should queue, got err %v", err)
	}
	if sel.Rail != RailCHAPS || !sel.QueuedNextDay {
		t.Fatalf("CHAPS should queue next day, got %+v", sel)
	}
	if sel.ValueDate.Equal(late.Truncate(24 * time.Hour)) {
		t.Fatal("value date must roll to next business day")
	}

	// require_same_day → RAIL_CUTOFF_EXCEEDED.
	_, err = svc.Select(context.Background(), SelectionRequest{
		Currency: "GBP", Amount: dec("1000"), At: late, RequireSameDay: true,
	})
	if codeOf(err) != "RAIL_CUTOFF_EXCEEDED" {
		t.Fatalf("want RAIL_CUTOFF_EXCEEDED, got %v", codeOf(err))
	}
}

func TestRailCutoffWeekendRoll(t *testing.T) {
	// Friday 18:00 UTC — after CHAPS 15:00 cut-off → next business day is
	// Monday (CHAPS does not process weekends).
	c := RailMatrix()[RailCHAPS]
	friday := time.Date(2026, 11, 6, 18, 0, 0, 0, time.UTC) // a Friday
	vd := c.NextValueDate(friday)
	if vd.Weekday() == time.Saturday || vd.Weekday() == time.Sunday {
		t.Fatalf("weekend-processing=false rail rolled to %v", vd)
	}
	if vd.Weekday() != time.Monday {
		t.Fatalf("want Monday value date, got %v", vd)
	}
	// FedNow processes weekends — Friday 18:00 is before 21:00 cutoff.
	fed := RailMatrix()[RailFedNow]
	if !fed.NextValueDate(friday).Equal(friday.Truncate(24 * time.Hour)) {
		t.Fatal("FedNow 18:00 Fri should settle same-day")
	}
}

func TestRailSelectionSepaOverInstantCap(t *testing.T) {
	svc, _, _ := newRailSvc(t, newP11Store())
	// €150k — above the SCT-Inst cap but the rail must still serve via
	// standard SCT (cap is instant-scheme-only).
	sel, err := svc.Select(context.Background(), SelectionRequest{
		Currency: "EUR", Amount: dec("150000"),
		At: time.Date(2026, 11, 9, 10, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatalf("EUR 150k must not fail: %v", err)
	}
	if sel.Rail != RailTARGET2 && sel.Rail != RailSEPA {
		t.Fatalf("EUR rail expected, got %s", sel.Rail)
	}
}

func TestRailSelectionRejectsBadInput(t *testing.T) {
	svc, _, _ := newRailSvc(t, newP11Store())
	if _, err := svc.Select(context.Background(), SelectionRequest{
		Currency: "USD", Amount: dec("0")}); codeOf(err) != "INVALID_REQUEST" {
		t.Fatalf("zero amount: %v", codeOf(err))
	}
	if _, err := svc.Select(context.Background(), SelectionRequest{
		Currency: "XX", Amount: dec("1")}); codeOf(err) != "INVALID_REQUEST" {
		t.Fatalf("bad ccy: %v", codeOf(err))
	}
	if _, err := svc.Select(context.Background(), SelectionRequest{
		Currency: "USD", Amount: dec("1"), Preferred: "BOGUS"}); codeOf(err) != "INVALID_REQUEST" {
		t.Fatalf("bad rail: %v", codeOf(err))
	}
}

// ---------------------------------------------------------------------------
// Envelope builders
// ---------------------------------------------------------------------------

func baseOutbound(rail RailID, ccy string) OutboundPayment {
	return OutboundPayment{
		FundingTxID: 1, AccountID: 7, Rail: rail, Currency: ccy,
		Amount:     dec("1234.50"),
		DebtorName: "Exchange Ops", DebtorAccount: "DE89370400440532013000",
		DebtorBIC:    "EXCHGB2L",
		CreditorName: "Jane Trader", CreditorIBAN: "GB29NWBK60161331926819",
		CreditorBIC:    "NWBKGB2L",
		RemittanceInfo: "withdrawal 1",
		Charges:        "SHA",
		ValueDate:      time.Date(2026, 11, 9, 0, 0, 0, 0, time.UTC),
	}
}

func TestSwiftMT103Envelope(t *testing.T) {
	env, err := SwiftAdapter{}.BuildOutbound(baseOutbound(RailSWIFT, "USD"))
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if env.MessageType != MsgMT103 || env.Rail != "SWIFT" {
		t.Fatalf("envelope meta wrong: %+v", env)
	}
	if env.UETR == "" || len(env.UETR) != 36 {
		t.Fatalf("UETR shape wrong: %q", env.UETR)
	}
	mt, ok := env.Payload.(MT103)
	if !ok {
		t.Fatalf("payload type %T, want MT103", env.Payload)
	}
	if mt.SenderBIC != "EXCHGB2L" || mt.ReceiverBIC != "NWBKGB2L" {
		t.Fatalf("BICs wrong: %+v", mt)
	}
	if mt.BeneficiaryAcct != "GB29NWBK60161331926819" || mt.BeneficiaryName != "Jane Trader" {
		t.Fatalf("beneficiary wrong: %+v", mt)
	}
	if mt.DetailsOfCharges != "SHA" || mt.BankOperationCode != "CRED" {
		t.Fatalf("charge/opcode wrong: %+v", mt)
	}
	if !strings.Contains(mt.ValueDateCcyAmount, "USD1234.5") {
		t.Fatalf("32A wrong: %q", mt.ValueDateCcyAmount)
	}
}

func TestSwiftMT202BankToBank(t *testing.T) {
	p := baseOutbound(RailSWIFT, "USD")
	p.CreditorIBAN = "" // BIC-only creditor → bank-to-bank leg
	env, err := SwiftAdapter{}.BuildOutbound(p)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if env.MessageType != MsgMT202 {
		t.Fatalf("want MT202, got %s", env.MessageType)
	}
}

func TestSwiftReturnMT199(t *testing.T) {
	env, err := SwiftAdapter{}.BuildReturn(ReturnInstruction{
		OriginalEndToEndID: "E2E-ORIG", OriginalBankTxID: "BANK-1",
		Rail: RailSWIFT, Currency: "USD", Amount: dec("100"),
		ReturnReasonCode: "AC04", ReturnReason: "closed",
		DebtorBIC: "EXCHGB2L", CreditorBIC: "NWBKGB2L",
	})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if env.MessageType != MsgMT199 {
		t.Fatalf("want MT199, got %s", env.MessageType)
	}
	n := env.Payload.(MT199).Narrative
	if !strings.Contains(n, "E2E-ORIG") || !strings.Contains(n, "AC04") {
		t.Fatalf("narrative missing return refs: %q", n)
	}
}

func TestSepaEnvelopes(t *testing.T) {
	a := SepaAdapter{}
	// ≤ €100k + no explicit value date → SCT Inst (pacs.008 INST).
	p := baseOutbound(RailSEPA, "EUR")
	p.ValueDate = time.Time{} // unflagged instant → pacs.008
	env, err := a.BuildOutbound(p)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if env.MessageType != MsgPacs008 {
		t.Fatalf("small EUR should ride SCT Inst, got %s", env.MessageType)
	}
	// > €100k → standard SCT pain.001.
	p2 := baseOutbound(RailSEPA, "EUR")
	p2.Amount = dec("150000")
	env2, err := a.BuildOutbound(p2)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if env2.MessageType != MsgPain001 {
		t.Fatalf("€150k should downgrade to SCT, got %s", env2.MessageType)
	}
	doc := env2.Payload.(Pain001)
	if doc.GroupHeader.NumberOfTxs != 1 || doc.PaymentInfo.PaymentMethod != "TRF" {
		t.Fatalf("pain.001 header wrong: %+v", doc)
	}
	if doc.CreditTransfer.Amount.Currency != "EUR" || doc.CreditTransfer.Amount.Value != "150000" {
		t.Fatalf("SCT amount wrong: %+v", doc.CreditTransfer.Amount)
	}
	// Return → pacs.004.
	env3, err := a.BuildReturn(ReturnInstruction{
		OriginalEndToEndID: "E2E-1", Rail: RailSEPA, Currency: "EUR",
		Amount: dec("10"), ReturnReasonCode: "AC01",
	})
	if err != nil || env3.MessageType != MsgPacs004 {
		t.Fatalf("SEPA return: %v %s", err, env3.MessageType)
	}
}

func TestFedNowEnvelopeUSDOnly(t *testing.T) {
	a := FedNowAdapter{}
	env, err := a.BuildOutbound(baseOutbound(RailFedNow, "USD"))
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if env.MessageType != MsgPacs008 || !env.Payload.(FedNowPayment).InstantSettle {
		t.Fatalf("FedNow envelope wrong: %+v", env)
	}
	if env.Payload.(FedNowPayment).Transfer.LocalInstrument != "INST" {
		t.Fatal("INST local instrument flag missing")
	}
	if _, err := a.BuildOutbound(baseOutbound(RailFedNow, "EUR")); err == nil ||
		codeOf(err) != CodeBankingRailUnavailable {
		t.Fatalf("EUR on FedNow must fail closed, got %v", err)
	}
}

func TestACHEnvelopeCents(t *testing.T) {
	a := ACHAdapter{ImmediateOrigin: "123456789", OriginatingDFI: "02100002"}
	env, err := a.BuildOutbound(baseOutbound(RailACH, "USD"))
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if env.MessageType != MsgNachaFile {
		t.Fatalf("want NACHA_FILE, got %s", env.MessageType)
	}
	f := env.Payload.(NACHAFile)
	if f.Entry.AmountCents != 123450 {
		t.Fatalf("amount cents: %d", f.Entry.AmountCents)
	}
	if f.Batch.ServiceClassCode != 220 || f.Batch.SECCode != "PPD" {
		t.Fatalf("batch header wrong: %+v", f.Batch)
	}
	if len(f.Entry.TraceNumber) > 16 {
		t.Fatalf("trace too long: %q", f.Entry.TraceNumber)
	}
	// Sub-cent precision fails closed.
	p := baseOutbound(RailACH, "USD")
	p.Amount = dec("1.005")
	if _, err := a.BuildOutbound(p); codeOf(err) != "INVALID_REQUEST" {
		t.Fatalf("sub-cent must fail, got %v", codeOf(err))
	}
	// Non-USD fails.
	if _, err := a.BuildOutbound(baseOutbound(RailACH, "EUR")); err == nil {
		t.Fatal("EUR on ACH must fail")
	}
}

func TestChapsTarget2Envelopes(t *testing.T) {
	env, err := ChapsAdapter{}.BuildOutbound(baseOutbound(RailCHAPS, "GBP"))
	if err != nil || env.MessageType != MsgPacs008 {
		t.Fatalf("CHAPS: %v %v", err, env)
	}
	if env.Payload.(ChapsPayment).Transfer.ServiceLevel != "SDVA" {
		t.Fatal("SDVA service level expected")
	}
	if _, err := (ChapsAdapter{}).BuildOutbound(baseOutbound(RailCHAPS, "USD")); err == nil {
		t.Fatal("USD on CHAPS must fail")
	}
	env2, err := Target2Adapter{}.BuildOutbound(baseOutbound(RailTARGET2, "EUR"))
	if err != nil || env2.MessageType != MsgPacs008 {
		t.Fatalf("TARGET2: %v", err)
	}
	if env2.Payload.(Target2Payment).SettlementMethod != "CLRG" {
		t.Fatal("CLRG settlement method expected")
	}
	if _, err := (Target2Adapter{}).BuildOutbound(baseOutbound(RailTARGET2, "GBP")); err == nil {
		t.Fatal("GBP on TARGET2 must fail")
	}
}

// ---------------------------------------------------------------------------
// Return-code mapping
// ---------------------------------------------------------------------------

func TestReturnCodeMappingSpecPins(t *testing.T) {
	cases := []struct {
		rail    RailID
		raw     string
		surface string
		canon   string
	}{
		{RailSEPA, "AC01", "SETTLEMENT_ACCOUNT_CLOSED", RetCanonAccountInvalid},
		{RailSEPA, "AM04", "SETTLEMENT_RAIL_REJECTED", RetCanonInsufficient},
		{RailSWIFT, "RR04", "SETTLEMENT_RAIL_REJECTED", RetCanonRegulatory}, // NOT SANCTIONS_SERVICE_UNAVAILABLE
		{RailSEPA, "TM01", "RAIL_CUTOFF_EXCEEDED", RetCanonCutoff},
		{RailACH, "R02", "SETTLEMENT_ACCOUNT_CLOSED", RetCanonAccountClosed},
		{RailACH, "R01", "SETTLEMENT_RAIL_REJECTED", RetCanonInsufficient},
		{RailACH, "R24", "SETTLEMENT_RAIL_REJECTED", RetCanonDuplicate},
		{RailFedNow, "FOCR", "SETTLEMENT_RAIL_REJECTED", RetCanonReturnAccepted},
	}
	for _, tc := range cases {
		m := MapReturnCode(tc.rail, tc.raw)
		if m.SurfaceCode != tc.surface || m.Canonical != tc.canon {
			t.Fatalf("%s %s → (%s,%s), want (%s,%s)",
				tc.rail, tc.raw, m.SurfaceCode, m.Canonical, tc.surface, tc.canon)
		}
	}
	// Spec pin: RR04 must never surface SANCTIONS_SERVICE_UNAVAILABLE.
	if m := MapReturnCode(RailSWIFT, "RR04"); m.SurfaceCode == "SANCTIONS_SERVICE_UNAVAILABLE" {
		t.Fatal("RR04 mapped to SANCTIONS_SERVICE_UNAVAILABLE — spec pin violated")
	}
}

func TestReturnCodeUnknownQuarantines(t *testing.T) {
	m := MapReturnCode(RailSEPA, "ZZ99")
	if !m.Quarantine || !m.Alert || m.Canonical != RetCanonUnknown {
		t.Fatalf("unknown code must quarantine+alert: %+v", m)
	}
	if m.FundingStatus != FundingPendingReview {
		t.Fatalf("unknown funding status %s, want PENDING_REVIEW", m.FundingStatus)
	}
	// Empty code also quarantines.
	m2 := MapReturnCode(RailSWIFT, "")
	if !m2.Quarantine {
		t.Fatal("empty code must quarantine")
	}
	// MS02/MS03/NARR quarantine (reason unspecified on the wire).
	for _, c := range []string{"MS02", "MS03", "NARR"} {
		if !MapReturnCode(RailSEPA, c).Quarantine {
			t.Fatalf("%s must quarantine", c)
		}
	}
}

func TestApplyReturnCompensating(t *testing.T) {
	st := newP11Store()
	svc, fp, fa := newRailSvc(t, st)

	// Seed a DISPATCHED outbound instruction linked to a CONFIRMED
	// withdrawal funding row.
	st.fundingTx[11] = &FundingTxRow{
		ID: 11, AccountID: 7, Currency: "USD", Type: "WITHDRAWAL",
		Amount: dec("500"), Status: FundingConfirmed,
	}
	ftx := int64(11)
	if _, err := st.InsertRailPayment(context.Background(), nil, RailPaymentRow{
		FundingTransactionID: &ftx, Direction: RailDirectionOutbound,
		Rail: "SEPA", MessageType: MsgPacs008, EndToEndID: "E2E-TEST-1",
		Status: RailPaymentDispatched,
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	out, err := svc.ApplyReturn(context.Background(), "E2E-TEST-1", "AC01", "incorrect account")
	if err != nil {
		t.Fatalf("ApplyReturn: %v", err)
	}
	if out.Payment.Status != RailPaymentReturned {
		t.Fatalf("status %s, want RETURNED", out.Payment.Status)
	}
	if out.FundingStatus != FundingFailed {
		t.Fatalf("funding status %s, want FAILED", out.FundingStatus)
	}
	if st.fundingTx[11].Status != FundingFailed {
		t.Fatalf("store funding status %s", st.fundingTx[11].Status)
	}
	// Compensating journal: ClearingTransit → CustomerLiability,
	// wallet locked → available.
	if len(fp.journals) != 1 {
		t.Fatalf("want 1 compensation journal, got %d", len(fp.journals))
	}
	j := fp.journals[0]
	if j.IdempotencyKey != "rail-return:E2E-TEST-1" {
		t.Fatalf("idempotency key wrong: %s", j.IdempotencyKey)
	}
	eff := j.Effects[0]
	if !eff.AvailableDelta.Equal(dec("500")) || !eff.LockedDelta.Equal(dec("-500")) {
		t.Fatalf("wallet effect wrong: %+v", eff)
	}
	if j.Lines[0].AccountCode != ledger.ClearingTransit("USD") ||
		j.Lines[1].AccountCode != ledger.CustomerLiability("USD") {
		t.Fatalf("GL legs wrong: %+v", j.Lines)
	}
	if len(fa.alerts) != 0 {
		t.Fatalf("known code must not alert: %+v", fa.alerts)
	}

	// Idempotent replay — no second journal.
	out2, err := svc.ApplyReturn(context.Background(), "E2E-TEST-1", "AC01", "incorrect account")
	if err != nil || len(fp.journals) != 1 {
		t.Fatalf("replay: err=%v journals=%d", err, len(fp.journals))
	}
	if out2.Payment.Status != RailPaymentReturned {
		t.Fatal("replay must return recorded state")
	}
}

func TestApplyReturnUnknownQuarantinesAndAlerts(t *testing.T) {
	st := newP11Store()
	svc, fp, fa := newRailSvc(t, st)
	st.fundingTx[12] = &FundingTxRow{
		ID: 12, AccountID: 7, Currency: "USD", Type: "WITHDRAWAL",
		Amount: dec("500"), Status: FundingConfirmed,
	}
	ftx := int64(12)
	if _, err := st.InsertRailPayment(context.Background(), nil, RailPaymentRow{
		FundingTransactionID: &ftx, Direction: RailDirectionOutbound,
		Rail: "ACH", MessageType: MsgNachaFile, EndToEndID: "E2E-UNK",
		Status: RailPaymentDispatched,
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	out, err := svc.ApplyReturn(context.Background(), "E2E-UNK", "R99", "mystery")
	if err != nil {
		t.Fatalf("ApplyReturn: %v", err)
	}
	if !out.Quarantined || out.FundingStatus != FundingPendingReview {
		t.Fatalf("unknown must quarantine: %+v", out)
	}
	if len(fp.journals) != 0 {
		t.Fatal("quarantine path must not post compensation")
	}
	if len(fa.alerts) != 1 || fa.alerts[0].Severity != "P1" || fa.alerts[0].Code != "RAIL_RETURN_UNKNOWN" {
		t.Fatalf("want P1 RAIL_RETURN_UNKNOWN alert, got %+v", fa.alerts)
	}
	// Funds stay locked — funding row is PENDING_REVIEW not FAILED.
	if st.fundingTx[12].Status != FundingPendingReview {
		t.Fatalf("funding status %s", st.fundingTx[12].Status)
	}
}

func TestApplyReturnNotFound(t *testing.T) {
	svc, _, _ := newRailSvc(t, newP11Store())
	_, err := svc.ApplyReturn(context.Background(), "NOPE", "AC01", "x")
	if codeOf(err) != "NOT_FOUND" {
		t.Fatalf("want NOT_FOUND, got %v", codeOf(err))
	}
}

// ---------------------------------------------------------------------------
// Jaro-Winkler
// ---------------------------------------------------------------------------

func TestJaroWinklerKnownVectors(t *testing.T) {
	// Canonical vector: MARTHA/MARHTA → Jaro-Winkler ≈ 0.9611.
	got := JaroWinkler("MARTHA", "MARHTA")
	if got < 0.95 || got > 0.97 {
		t.Fatalf("MARTHA/MARHTA = %.4f, want ≈0.961", got)
	}
	// Identical / empty edges.
	if JaroWinkler("ABC", "ABC") != 1 {
		t.Fatal("identical must be 1.0")
	}
	if JaroWinkler("", "ABC") != 0 || JaroWinkler("ABC", "") != 0 {
		t.Fatal("empty must be 0")
	}
	// DWAYNE/DUANE ≈ 0.840.
	if got := JaroWinkler("DWAYNE", "DUANE"); got < 0.83 || got > 0.85 {
		t.Fatalf("DWAYNE/DUANE = %.4f, want ≈0.840", got)
	}
	// Clearly different names score low.
	if JaroWinkler("JOHN SMITH", "XWVIQZ KBRTM") > 0.55 {
		t.Fatal("unrelated names must score low")
	}
}

func TestNormalizeLegalName(t *testing.T) {
	cases := []struct{ in, want string }{
		{"Jane Trader LLC", "JANE TRADER"},
		{"jane  trader,  inc.", "JANE TRADER"},
		{"Smith & Jones GmbH", "SMITH AND JONES"},
		{"ACME CORP", "ACME"},
		{"Öztürk Trading Ltd", "ÖZTÜRK TRADING"},
		{"Solo Name", "SOLO NAME"}, // single word keeps suffix-less name
	}
	for _, tc := range cases {
		if got := NormalizeLegalName(tc.in); got != tc.want {
			t.Fatalf("Normalize(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestNameMatchThresholdDirection(t *testing.T) {
	// The spec pin: similarity >= 0.85 accepts, < 0.85 rejects.
	same := JaroWinkler(NormalizeLegalName("Jane Trader"),
		NormalizeLegalName("JANE TRADER"))
	if same < NameMatchThreshold {
		t.Fatalf("identical normalized names scored %.3f < %.2f", same, NameMatchThreshold)
	}
	mismatch := JaroWinkler(NormalizeLegalName("John Smith"),
		NormalizeLegalName("Jane Trader"))
	if mismatch >= NameMatchThreshold {
		t.Fatalf("unrelated names scored %.3f >= %.2f", mismatch, NameMatchThreshold)
	}
	// Score is always bounded [0,1].
	for _, pair := range [][2]string{
		{"A", "A"}, {"", ""}, {"ABC", "XYZ"}, {"MARTHA", "MARHTA"},
	} {
		s := JaroWinkler(pair[0], pair[1])
		if s < 0 || s > 1 {
			t.Fatalf("score %v out of [0,1] for %v", s, pair)
		}
	}
}

// ---------------------------------------------------------------------------
// Deposit guard
// ---------------------------------------------------------------------------

func guardFixture(t *testing.T) (*DepositGuard, *p11Store, *fakePoster, *fakeAlerter) {
	st := newP11Store()
	st.meta[7] = &AccountMeta{ID: 7, UserID: 1, Status: "ACTIVE", KYCTier: "T1", BaseCurrency: "USD"}
	svc, fp, fa := newRailSvc(t, st)
	g, err := NewDepositGuard(st, fp, svc)
	if err != nil {
		t.Fatalf("guard: %v", err)
	}
	g.WithLegalNames(fakeNames{names: map[int64]string{7: "JANE TRADER"}})
	g.WithAlerter(fa)
	return g, st, fp, fa
}

func TestDepositGuardAccepts(t *testing.T) {
	g, st, fp, fa := guardFixture(t)
	res, err := g.ScreenInbound(context.Background(), InboundWire{
		BankTxID: "BANK-TX-1", Rail: "SEPA", Currency: "EUR",
		Amount: dec("1000"), OriginatorName: "Jane Trader",
		OriginatorAccount: "DE111", Reference: "EXC00000007-EUR",
	})
	if err != nil {
		t.Fatalf("accept path errored: %v", err)
	}
	if res.Disposition != DispositionAccepted || res.Deposit == nil {
		t.Fatalf("want ACCEPTED+deposit, got %+v", res)
	}
	if res.Deposit.Status != FundingPending {
		t.Fatalf("deposit status %s, want PENDING (locked landing)", res.Deposit.Status)
	}
	if len(fp.journals) != 1 {
		t.Fatalf("want nostro→liability journal, got %d", len(fp.journals))
	}
	j := fp.journals[0]
	if j.Lines[0].AccountCode != ledger.Nostro("EUR") ||
		j.Lines[1].AccountCode != ledger.CustomerLiability("EUR") {
		t.Fatalf("journal legs wrong: %+v", j.Lines)
	}
	if !j.Effects[0].LockedDelta.Equal(dec("1000")) || !j.Effects[0].AvailableDelta.IsZero() {
		t.Fatalf("wallet must lock, not credit: %+v", j.Effects[0])
	}
	if len(fa.alerts) != 0 {
		t.Fatalf("accepted wire must not alert: %+v", fa.alerts)
	}
	if _, ok := st.deposits[res.Deposit.ID]; !ok {
		t.Fatal("deposit row not persisted")
	}
}

func TestDepositGuardNameMismatchQuarantines(t *testing.T) {
	g, st, fp, fa := guardFixture(t)
	res, err := g.ScreenInbound(context.Background(), InboundWire{
		BankTxID: "BANK-TX-2", Rail: "SEPA", Currency: "EUR",
		Amount: dec("2000"), OriginatorName: "XWVIQZ KBRTM",
		OriginatorAccount: "DE999", Reference: "EXC00000007-EUR",
	})
	// THIRD_PARTY_DEPOSIT_REJECTED is the API surface code (422).
	if codeOf(err) != CodeThirdPartyDepositRejected {
		t.Fatalf("want THIRD_PARTY_DEPOSIT_REJECTED, got %v (err %v)", codeOf(err), err)
	}
	if res == nil || res.Disposition != DispositionQuarantined || res.Suspense == nil {
		t.Fatalf("want quarantine result with suspense row, got %+v", res)
	}
	s := res.Suspense
	if s.UnmatchedReason != "NAME_MISMATCH" || s.GLAccount != "2150" {
		t.Fatalf("suspense row wrong: %+v", s)
	}
	if s.NameMatchScore == nil || *s.NameMatchScore >= NameMatchThreshold {
		t.Fatalf("score must record <%.2f: %+v", NameMatchThreshold, s.NameMatchScore)
	}
	if s.AccountID == nil || *s.AccountID != 7 {
		t.Fatal("attributed suspense must carry account_id")
	}
	if s.FundingTransactionID == nil {
		t.Fatal("attributed wire must carry a PENDING_REVIEW funding row")
	}
	if ft := st.fundingTx[*s.FundingTransactionID]; ft.Status != FundingPendingReview {
		t.Fatalf("funding status %s, want PENDING_REVIEW", ft.Status)
	}
	if s.SLAExpiresAt.Before(time.Now().Add(47 * time.Hour)) {
		t.Fatal("48h SLA missing")
	}
	// Return wire persisted (pacs.004 for SEPA).
	if res.ReturnWire == nil || res.ReturnWire.MessageType != MsgPacs004 {
		t.Fatalf("want pacs.004 return wire, got %+v", res.ReturnWire)
	}
	if s.ReturnPaymentID == nil {
		t.Fatal("suspense must backlink the return payment")
	}
	// Suspense journal: nostro → 2150.
	if len(fp.journals) != 1 || fp.journals[0].Lines[1].AccountCode != ledger.SuspenseDeposits("EUR") {
		t.Fatalf("want suspense GL journal, got %+v", fp.journals)
	}
	if len(fp.journals[0].Effects) != 0 {
		t.Fatal("quarantined funds must never reach the client wallet")
	}
	// P1 alert raised.
	if len(fa.alerts) != 1 || fa.alerts[0].Code != "DEPOSIT_QUARANTINED" || fa.alerts[0].Severity != "P1" {
		t.Fatalf("want P1 quarantine alert, got %+v", fa.alerts)
	}
}

func TestDepositGuardMissingReference(t *testing.T) {
	g, st, _, fa := guardFixture(t)
	res, err := g.ScreenInbound(context.Background(), InboundWire{
		BankTxID: "BANK-TX-3", Rail: "SWIFT", Currency: "USD",
		Amount: dec("50"), OriginatorName: "Who Knows",
		OriginatorAccount: "XX", RemittanceInfo: "gift",
	})
	if err != nil {
		t.Fatalf("unattributable wire must quarantine, not error: %v", err)
	}
	if res.Disposition != DispositionQuarantined || res.Suspense.UnmatchedReason != "MISSING_REFERENCE" {
		t.Fatalf("got %+v", res)
	}
	if res.Suspense.AccountID != nil || res.Suspense.FundingTransactionID != nil {
		t.Fatal("unattributable wire must carry NULL account/funding links")
	}
	if len(st.deposits) != 0 {
		t.Fatal("no funding row for unreferenced wire")
	}
	if len(fa.alerts) != 1 {
		t.Fatal("quarantine must alert")
	}
}

func TestDepositGuardDedup(t *testing.T) {
	g, _, _, _ := guardFixture(t)
	ctx := context.Background()

	// Accepted-wire replay → idempotent ACCEPTED (idempotency_key dedup).
	w := InboundWire{
		BankTxID: "BANK-DUP-A", Rail: "SEPA", Currency: "EUR",
		Amount: dec("10"), OriginatorName: "Jane Trader",
		OriginatorAccount: "DE1", Reference: "EXC00000007-EUR",
	}
	if _, err := g.ScreenInbound(ctx, w); err != nil {
		t.Fatalf("first accept: %v", err)
	}
	r2, err := g.ScreenInbound(ctx, w)
	if err != nil || !r2.Idempotent || r2.Disposition != DispositionAccepted {
		t.Fatalf("accepted dup must replay idempotently: err=%v res=%+v", err, r2)
	}

	// Quarantined-wire replay → idempotent QUARANTINED (bank_tx_id guard).
	bad := InboundWire{
		BankTxID: "BANK-DUP-Q", Rail: "SWIFT", Currency: "USD",
		Amount: dec("5"), OriginatorName: "NOPE", OriginatorAccount: "X",
		Reference: "EXC00000007-USD",
	}
	if _, err := g.ScreenInbound(ctx, bad); codeOf(err) != CodeThirdPartyDepositRejected {
		t.Fatalf("first mismatch: %v", codeOf(err))
	}
	r4, err := g.ScreenInbound(ctx, bad)
	if err != nil || !r4.Idempotent || r4.Disposition != DispositionQuarantined {
		t.Fatalf("quarantined dup must replay idempotently: err=%v res=%+v", err, r4)
	}
}

func TestDepositGuardResolverUnwiredFailsClosed(t *testing.T) {
	st := newP11Store()
	st.meta[7] = &AccountMeta{ID: 7, Status: "ACTIVE"}
	svc, _, _ := newRailSvc(t, st)
	g, _ := NewDepositGuard(st, &fakePoster{}, svc)
	// No WithLegalNames — attribution must refuse, not quarantine blindly.
	_, err := g.ScreenInbound(context.Background(), InboundWire{
		BankTxID: "B1", Rail: "SEPA", Currency: "EUR", Amount: dec("1"),
		Reference: "EXC00000007-EUR",
	})
	if codeOf(err) != "INTERNAL_ERROR" {
		t.Fatalf("unwired resolver must error fail-closed, got %v", codeOf(err))
	}
}

func TestResolveSuspenseReleaseAndReturn(t *testing.T) {
	g, st, fp, _ := guardFixture(t)
	ctx := context.Background()

	// Seed a quarantined, attributed wire.
	res, err := g.ScreenInbound(ctx, InboundWire{
		BankTxID: "BANK-TX-R1", Rail: "SEPA", Currency: "EUR",
		Amount: dec("300"), OriginatorName: "NO MATCH AT ALL",
		OriginatorAccount: "DE1", Reference: "EXC00000007-EUR",
	})
	if codeOf(err) != CodeThirdPartyDepositRejected {
		t.Fatalf("seed mismatch: %v", codeOf(err))
	}
	sid := res.Suspense.ID

	// RELEASE_TO_CLIENT → RESOLVED + funding COMPLETED + wallet credit.
	out, err := g.ResolveSuspense(ctx, sid,
		ResolveReleaseToClient, 99, "verified manually")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if out.Status != "RESOLVED" {
		t.Fatalf("status %s", out.Status)
	}
	if st.fundingTx[*res.Suspense.FundingTransactionID].Status != FundingCompleted {
		t.Fatal("funding row must complete on release")
	}
	last := fp.journals[len(fp.journals)-1]
	if last.Lines[0].AccountCode != ledger.SuspenseDeposits("EUR") ||
		last.Lines[1].AccountCode != ledger.CustomerLiability("EUR") {
		t.Fatalf("release journal legs wrong: %+v", last.Lines)
	}
	if !last.Effects[0].AvailableDelta.Equal(dec("300")) {
		t.Fatal("release must credit available balance")
	}

	// Second resolution attempt rejected (terminal state).
	if _, err := g.ResolveSuspense(ctx, sid,
		ResolveReturnToSource, 99, "again"); codeOf(err) != "INVALID_REQUEST" {
		t.Fatalf("re-resolve must fail: %v", codeOf(err))
	}

	// RETURN_TO_SOURCE path on a second wire.
	res3, _ := g.ScreenInbound(ctx, InboundWire{
		BankTxID: "BANK-TX-R4", Rail: "SEPA", Currency: "EUR",
		Amount: dec("50"), OriginatorName: "NOT MATCHING", OriginatorAccount: "D",
		Reference: "EXC00000007-EUR",
	})
	out2, err := g.ResolveSuspense(ctx, res3.Suspense.ID,
		ResolveReturnToSource, 99, "returning")
	if err != nil {
		t.Fatalf("return resolve: %v", err)
	}
	if out2.Status != "RETURNED_TO_SOURCE" {
		t.Fatalf("status %s", out2.Status)
	}
	if st.fundingTx[*res3.Suspense.FundingTransactionID].Status != FundingFailed {
		t.Fatal("funding row must fail on return")
	}
	last2 := fp.journals[len(fp.journals)-1]
	if last2.Lines[1].AccountCode != ledger.ClearingTransit("EUR") {
		t.Fatalf("return must move liability to transit, got %+v", last2.Lines)
	}
	if len(last2.Effects) != 0 {
		t.Fatal("return must not touch client wallet")
	}
}

func TestResolveSuspenseUnattributableReleaseFails(t *testing.T) {
	g, _, _, _ := guardFixture(t)
	res, err := g.ScreenInbound(context.Background(), InboundWire{
		BankTxID: "BANK-TX-UNREF", Rail: "SWIFT", Currency: "USD",
		Amount: dec("9"), OriginatorName: "?", OriginatorAccount: "x",
	})
	if err != nil {
		t.Fatalf("quarantine seed: %v", err)
	}
	if _, err := g.ResolveSuspense(context.Background(), res.Suspense.ID,
		ResolveReleaseToClient, 99, "release it"); codeOf(err) != "INVALID_REQUEST" {
		t.Fatalf("release of unattributed wire must fail: %v", codeOf(err))
	}
}

// codeOf extracts the coded error code or "".
func codeOf(err error) string {
	var e *excerrors.Error
	if stderrors.As(err, &e) {
		return e.Code
	}
	return ""
}
