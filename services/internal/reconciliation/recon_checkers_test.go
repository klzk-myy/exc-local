package reconciliation

// recon_checkers_test.go — per-category unit tests against fake sources
// (no PostgreSQL). Each checker is exercised for: divergence → MISMATCH
// with the right halt scope; unreachable/partial inputs → INCONCLUSIVE.

import (
	"context"
	"errors"
	"testing"
	"time"

	"exchange/internal/recovery"
	"exchange/pkg/decimal"
)

func dec(t *testing.T, s string) decimal.Decimal {
	t.Helper()
	return decimal.RequireFromString(s)
}

// ---------------------------------------------------------------------------
// BALANCES
// ---------------------------------------------------------------------------

type fakeBalancesSrc struct{ res *recovery.RecReconcileResult }

func (f fakeBalancesSrc) WalletDiff(context.Context) (*recovery.RecReconcileResult, error) {
	return f.res, nil
}

func TestBalancesChecker(t *testing.T) {
	res := &recovery.RecReconcileResult{
		WalletsChecked: 2, LedgerRows: 10,
		Mismatches: []recovery.RecWalletMismatch{
			{AccountID: 7, Currency: "USD", LedgerNet: "100", LiveTotal: "90",
				EntryCount: 5, Kind: "amount"},
			{AccountID: 8, Currency: "EUR", LedgerNet: "50", LiveTotal: "50",
				JournalSumsNet: "49", Kind: "journal_sums_drift"},
			{AccountID: 9, Currency: "USD", LedgerNet: "0", LiveTotal: "25",
				Kind: "no_ledger_stream"},
		},
	}
	fs, err := BalancesChecker{Src: fakeBalancesSrc{res}}.Run(
		context.Background(), Scope{Now: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	var mism, incon int
	for _, f := range fs {
		switch f.Severity {
		case SevMismatch:
			mism++
			if f.HaltScope != "ACCOUNT" {
				t.Fatalf("money mismatch without account halt: %+v", f)
			}
			if f.Delta == nil || f.Expected == nil {
				t.Fatalf("missing fixed-point triple: %+v", f)
			}
		case SevInconclusive:
			incon++
		}
	}
	if mism != 2 || incon != 1 {
		t.Fatalf("mismatch=%d inconclusive=%d: %+v", mism, incon, fs)
	}
	// journal_sums_drift is INCONCLUSIVE — the money verifies; the cache
	// drifted. Halting on it would stop trading over a repairable cache.
	for _, f := range fs {
		if f.Detail["kind"] == "journal_sums_drift" && f.Severity != SevInconclusive {
			t.Fatal("journal_sums_drift must be inconclusive, never a halt")
		}
	}
}

func TestBalancesCheckerNilSource(t *testing.T) {
	fs, _ := BalancesChecker{}.Run(context.Background(), Scope{})
	if len(fs) != 1 || fs[0].Severity != SevInconclusive {
		t.Fatalf("nil source must report inconclusive: %+v", fs)
	}
}

// ---------------------------------------------------------------------------
// GENERAL_LEDGER
// ---------------------------------------------------------------------------

type fakeGL struct {
	sums []GLCurrencySum
	imbs []GLJournalImbalance
}

func (f fakeGL) CurrencySums(context.Context) ([]GLCurrencySum, error) {
	return f.sums, nil
}
func (f fakeGL) JournalImbalances(context.Context) ([]GLJournalImbalance, error) {
	return f.imbs, nil
}

func TestGeneralLedgerClean(t *testing.T) {
	src := fakeGL{sums: []GLCurrencySum{
		{Currency: "USD", Debits: dec(t, "100"), Credits: dec(t, "100")},
		{Currency: "EUR", Debits: dec(t, "7.5"), Credits: dec(t, "7.5")},
	}}
	fs, err := GeneralLedgerChecker{Src: src}.Run(context.Background(), Scope{})
	if err != nil {
		t.Fatal(err)
	}
	if len(fs) != 0 {
		t.Fatalf("balanced GL must be clean: %+v", fs)
	}
}

func TestGeneralLedgerImbalance(t *testing.T) {
	src := fakeGL{
		sums: []GLCurrencySum{
			{Currency: "USD", Debits: dec(t, "101"), Credits: dec(t, "100")},
		},
		imbs: []GLJournalImbalance{
			{JournalEntryID: 42, Currency: "USD",
				Debits: dec(t, "5"), Credits: dec(t, "4")},
		},
	}
	fs, err := GeneralLedgerChecker{Src: src}.Run(context.Background(), Scope{})
	if err != nil {
		t.Fatal(err)
	}
	if len(fs) != 2 {
		t.Fatalf("expected currency + journal findings: %+v", fs)
	}
	for _, f := range fs {
		if f.Severity != SevMismatch || f.HaltScope != "GLOBAL" {
			t.Fatalf("GL divergence must be MISMATCH+GLOBAL halt: %+v", f)
		}
		if f.Delta == nil {
			t.Fatalf("GL finding must carry fixed-point delta: %+v", f)
		}
	}
}

// ---------------------------------------------------------------------------
// POSITIONS / PNL
// ---------------------------------------------------------------------------

type fakePositions struct {
	pos   []PgPosition
	fills []FillNet
	err   error
}

func (f fakePositions) Positions(context.Context) ([]PgPosition, error) {
	return f.pos, f.err
}
func (f fakePositions) FillNets(context.Context) ([]FillNet, error) {
	return f.fills, f.err
}

func TestPositionsChecker(t *testing.T) {
	src := fakePositions{
		pos: []PgPosition{
			{AccountID: 5, InstrumentID: 1, Side: "LONG",
				Quantity: dec(t, "10"), EntryPrice: dec(t, "1.1")},
		},
		fills: []FillNet{
			{AccountID: 5, InstrumentID: 1, NetQty: dec(t, "9")},
			{AccountID: 6, InstrumentID: 2, NetQty: dec(t, "3")},
		},
	}
	fs, err := PositionsChecker{Src: src}.Run(context.Background(), Scope{})
	if err != nil {
		t.Fatal(err)
	}
	var mism, incon int
	for _, f := range fs {
		switch f.Severity {
		case SevMismatch:
			mism++
			if f.HaltScope != "ACCOUNT" {
				t.Fatalf("position divergence must halt account: %+v", f)
			}
		case SevInconclusive:
			incon++
			if f.Leg != "core_state" {
				t.Fatalf("unexpected inconclusive leg %+v", f)
			}
		}
	}
	if mism != 2 || incon != 1 {
		t.Fatalf("mismatch=%d inconclusive=%d: %+v", mism, incon, fs)
	}
}

func TestPnLChecker(t *testing.T) {
	mark := dec(t, "1.2")
	src := fakePositions{
		pos: []PgPosition{
			// Unrealized: (1.2-1.1)*10 = 1.0 — stored says 2.0 → mismatch.
			{AccountID: 5, InstrumentID: 1, Side: "LONG",
				Quantity: dec(t, "10"), EntryPrice: dec(t, "1.1"),
				MarkPrice:     &mark,
				UnrealizedPnL: dec(t, "2"), RealizedPnL: dec(t, "3")},
		},
		fills: []FillNet{
			{AccountID: 5, InstrumentID: 1, RealizedPnL: dec(t, "2.5")},
			{AccountID: 9, InstrumentID: 3, RealizedPnL: dec(t, "7")},
		},
	}
	fs, err := PnLChecker{Src: src}.Run(context.Background(), Scope{})
	if err != nil {
		t.Fatal(err)
	}
	var mism, incon int
	for _, f := range fs {
		switch f.Severity {
		case SevMismatch:
			mism++
		case SevInconclusive:
			incon++
		}
	}
	// realized 3 vs 2.5, unrealized 2 vs 1.0, orphan fill realized 7.
	if mism != 3 || incon != 0 {
		t.Fatalf("mismatch=%d inconclusive=%d: %+v", mism, incon, fs)
	}
}

func TestPnLNilMarkIsInconclusive(t *testing.T) {
	src := fakePositions{
		pos: []PgPosition{
			{AccountID: 5, InstrumentID: 1, Side: "LONG",
				Quantity: dec(t, "10"), EntryPrice: dec(t, "1.1")},
		},
	}
	fs, err := PnLChecker{Src: src}.Run(context.Background(), Scope{})
	if err != nil {
		t.Fatal(err)
	}
	if len(fs) != 1 || fs[0].Severity != SevInconclusive {
		t.Fatalf("nil mark with open qty must be inconclusive: %+v", fs)
	}
}

// ---------------------------------------------------------------------------
// ORDERS / TRADES (fake walSource over a hand-built walReplay)
// ---------------------------------------------------------------------------

type fakeWal struct {
	replay *walReplay
	err    error
}

func (f fakeWal) Replay(context.Context, []string) (*walReplay, error) {
	return f.replay, f.err
}

type fakeOrders struct {
	open []OpenOrder
	syms map[int64]string
}

func (f fakeOrders) OpenOrders(context.Context) ([]OpenOrder, error) {
	return f.open, nil
}
func (f fakeOrders) InstrumentSymbols(context.Context) (map[int64]string, error) {
	return f.syms, nil
}

func mkReplay() *walReplay {
	return &walReplay{
		origQty: map[uint64]int64{}, filledQty: map[uint64]int64{},
		live: map[uint64]bool{}, orderInstr: map[uint64]uint32{},
		trades: map[uint64]walTrade{}, complete: true,
	}
}

func TestOrdersChecker(t *testing.T) {
	r := mkReplay()
	// WAL: order 100 resting 5 units (500000000 scaled), instrument 1.
	r.origQty[100] = 500000000
	r.live[100] = true
	r.orderInstr[100] = 1
	src := fakeOrders{
		open: []OpenOrder{
			// PG: order 100 remaining differs (4) + phantom order 200.
			{ID: 100, AccountID: 7, InstrumentID: 1,
				RemainingQty: dec(t, "4"), Status: "PARTIALLY_FILLED"},
			{ID: 200, AccountID: 8, InstrumentID: 2,
				RemainingQty: dec(t, "1"), Status: "ACTIVE"},
		},
		syms: map[int64]string{1: "EUR/USD", 2: "GBP/USD"},
	}
	c := OrdersChecker{Src: src, Wal: fakeWal{replay: r}}
	fs, err := c.Run(context.Background(),
		Scope{WalDirs: []string{"/wal/0"}, Now: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	if len(fs) != 2 {
		t.Fatalf("want qty-divergence + phantom findings: %+v", fs)
	}
	for _, f := range fs {
		if f.Severity != SevMismatch || f.HaltScope != "INSTRUMENT" {
			t.Fatalf("order divergence must halt the instrument: %+v", f)
		}
	}
}

func TestOrdersIncompleteWalIsInconclusive(t *testing.T) {
	r := mkReplay()
	r.complete = false
	r.problems = []string{"lost seq range"}
	c := OrdersChecker{Src: fakeOrders{}, Wal: fakeWal{replay: r}}
	fs, err := c.Run(context.Background(), Scope{WalDirs: []string{"/wal/0"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(fs) != 1 || fs[0].Severity != SevInconclusive {
		t.Fatalf("partial journal must yield inconclusive: %+v", fs)
	}
}

func TestOrdersNoWalDirs(t *testing.T) {
	c := OrdersChecker{Src: fakeOrders{}, Wal: fakeWal{}}
	fs, _ := c.Run(context.Background(), Scope{})
	if len(fs) != 1 || fs[0].Severity != SevInconclusive {
		t.Fatalf("missing WAL dirs must be inconclusive: %+v", fs)
	}
}

type fakeTradesSrc struct{ trades []PgTrade }

func (f fakeTradesSrc) PgTrades(context.Context) ([]PgTrade, error) {
	return f.trades, nil
}

func TestTradesCheckerEmptyProjection(t *testing.T) {
	r := mkReplay()
	r.trades[55] = walTrade{TradeID: 55, QtyUnits: 100000000, InstrumentID: 1}
	c := TradesChecker{Src: fakeTradesSrc{}, Wal: fakeWal{replay: r}}
	fs, err := c.Run(context.Background(), Scope{WalDirs: []string{"/x"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(fs) != 1 || fs[0].Severity != SevInconclusive {
		t.Fatalf("empty projection + journaled trades = inconclusive: %+v", fs)
	}
}

func TestTradesCheckerDiff(t *testing.T) {
	r := mkReplay()
	r.trades[55] = walTrade{TradeID: 55, QtyUnits: 100000000, InstrumentID: 1}
	r.trades[56] = walTrade{TradeID: 56, QtyUnits: 200000000, InstrumentID: 1}
	src := fakeTradesSrc{trades: []PgTrade{
		{ID: 55, InstrumentID: 1, Quantity: dec(t, "1"), Price: dec(t, "1.1")},
		{ID: 99, InstrumentID: 1, Quantity: dec(t, "0.5"), Price: dec(t, "1.1")},
	}}
	c := TradesChecker{Src: src, Wal: fakeWal{replay: r}}
	fs, err := c.Run(context.Background(), Scope{WalDirs: []string{"/x"}})
	if err != nil {
		t.Fatal(err)
	}
	// trade 56 missing in PG + phantom 99.
	if len(fs) != 2 {
		t.Fatalf("want 2 mismatches: %+v", fs)
	}
	for _, f := range fs {
		if f.Severity != SevMismatch || f.HaltScope != "GLOBAL" {
			t.Fatalf("trade divergence must mismatch+GLOBAL: %+v", f)
		}
	}
}

// ---------------------------------------------------------------------------
// FUNDING
// ---------------------------------------------------------------------------

type fakeFunding struct {
	w []WithdrawalLeg
	q []QuarantineLeg
	d *DepositLeg
}

func (f fakeFunding) CompletedWithdrawals(context.Context) ([]WithdrawalLeg, error) {
	return f.w, nil
}
func (f fakeFunding) QuarantinedDeposits(context.Context) ([]QuarantineLeg, error) {
	return f.q, nil
}
func (f fakeFunding) DepositForBankTx(context.Context, string) (*DepositLeg, error) {
	return f.d, nil
}

func TestFundingChecker(t *testing.T) {
	rpID := int64(44)
	src := fakeFunding{
		w: []WithdrawalLeg{
			{FundingID: 1, AccountID: 7, Currency: "USD",
				Amount: dec(t, "500"), BankMethod: "SWIFT"},
			{FundingID: 2, AccountID: 8, Currency: "EUR",
				Amount: dec(t, "100"), BankMethod: "SEPA",
				RailPaymentID: &rpID, RailStatus: "RETURNED"},
			{FundingID: 3, AccountID: 8, Currency: "EUR",
				Amount: dec(t, "60"), BankMethod: "SEPA",
				RailPaymentID: &rpID, RailStatus: "DISPATCHED"},
		},
		q: []QuarantineLeg{
			{FundingID: 9, MappingID: 3, AccountID: 8, Currency: "EUR",
				Amount: dec(t, "250"), Status: "COMPLETED",
				QuarantineStatus: "QUARANTINED"},
		},
	}
	fs, err := FundingChecker{Src: src}.Run(context.Background(), Scope{Now: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	var mism, incon int
	var railHalts, acctHalts int
	for _, f := range fs {
		switch f.Severity {
		case SevMismatch:
			mism++
			switch f.HaltScope {
			case "RAIL":
				railHalts++
			case "ACCOUNT":
				acctHalts++
			}
		case SevInconclusive:
			incon++
		}
	}
	// withdrawal 1 (no rail) + 2 (RETURNED) + quarantined 9 = 3 mismatches;
	// withdrawal 3 in-flight + missing statement source = 2 inconclusive.
	if mism != 3 || incon != 2 || railHalts != 2 || acctHalts != 1 {
		t.Fatalf("mism=%d incon=%d rail=%d acct=%d: %+v",
			mism, incon, railHalts, acctHalts, fs)
	}
}

func TestFundingStatementLeg(t *testing.T) {
	src := fakeFunding{d: &DepositLeg{
		FundingID: 10, AccountID: 7, Currency: "USD",
		Amount: dec(t, "100"), Status: "COMPLETED"}}
	fc := FundingChecker{Src: src, Statements: stmtFunc(
		func(context.Context, time.Time) ([]StatementLine, error) {
			return []StatementLine{
				{BankTxID: "B1", Currency: "USD", Amount: dec(t, "100")},
				{BankTxID: "B2", Currency: "USD", Amount: dec(t, "99")},
			}, nil
		})}
	fs, err := fc.Run(context.Background(), Scope{Now: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	var mism int
	for _, f := range fs {
		if f.Severity == SevMismatch {
			mism++
		}
	}
	// B1 matches exactly (clean); B2 amount-diverges (mismatch).
	if mism != 1 {
		t.Fatalf("statement leg mismatches=%d: %+v", mism, fs)
	}
}

type stmtFunc func(context.Context, time.Time) ([]StatementLine, error)

func (f stmtFunc) StatementLines(ctx context.Context, since time.Time) ([]StatementLine, error) {
	return f(ctx, since)
}

// ---------------------------------------------------------------------------
// SETTLEMENT
// ---------------------------------------------------------------------------

type fakeSettlement struct {
	legs    []SettlementLeg
	orphans []NostroOrphan
	overdue []int64
}

func (f fakeSettlement) SettledLegs(context.Context) ([]SettlementLeg, error) {
	return f.legs, nil
}
func (f fakeSettlement) PostedOrphans(context.Context) ([]NostroOrphan, error) {
	return f.orphans, nil
}
func (f fakeSettlement) OverduePending(context.Context, time.Time) ([]int64, error) {
	return f.overdue, nil
}

func TestSettlementChecker(t *testing.T) {
	mvID := int64(9)
	src := fakeSettlement{
		legs: []SettlementLeg{
			{InstructionID: 1, Currency: "USD", Amount: dec(t, "10"),
				Direction: "PAY"}, // no movement
			{InstructionID: 2, Currency: "USD", Amount: dec(t, "10"),
				Direction: "PAY", MovementID: &mvID,
				MovementAmount: dec(t, "10"), MovementDir: "CREDIT",
				MovementStatus: "POSTED"}, // inverted direction
			{InstructionID: 3, Currency: "EUR", Amount: dec(t, "5"),
				Direction: "RECEIVE", MovementID: &mvID,
				MovementAmount: dec(t, "5"), MovementDir: "CREDIT",
				MovementStatus: "POSTED"}, // clean
			{InstructionID: 4, Currency: "EUR", Amount: dec(t, "7"),
				Direction: "RECEIVE", MovementID: &mvID,
				MovementAmount: dec(t, "7"), MovementDir: "CREDIT",
				MovementStatus: "PENDING"}, // poster lag → inconclusive
		},
		orphans: []NostroOrphan{
			{MovementID: 8, InstructionID: 20, InstructionStat: "PENDING",
				Currency: "USD", Amount: dec(t, "3")},
		},
		overdue: []int64{30},
	}
	fs, err := SettlementChecker{Src: src}.Run(context.Background(), Scope{Now: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	var mism, incon int
	for _, f := range fs {
		switch f.Severity {
		case SevMismatch:
			mism++
			if f.HaltScope != "GLOBAL" {
				t.Fatalf("settlement divergence must halt GLOBAL: %+v", f)
			}
		case SevInconclusive:
			incon++
		}
	}
	// legs 1+2 + orphan = 3 mismatches; leg 4 + overdue = 2 inconclusive.
	if mism != 3 || incon != 2 {
		t.Fatalf("mism=%d incon=%d: %+v", mism, incon, fs)
	}
}

// ---------------------------------------------------------------------------
// FEES
// ---------------------------------------------------------------------------

type fakeFees struct {
	exp []ExpectedFee
	col []CollectedFee
	rev []FeeRevenueRow
}

func (f fakeFees) ExpectedFees(context.Context) ([]ExpectedFee, error) {
	return f.exp, nil
}
func (f fakeFees) CollectedFees(context.Context) ([]CollectedFee, error) {
	return f.col, nil
}
func (f fakeFees) FeeRevenue(context.Context) ([]FeeRevenueRow, error) {
	return f.rev, nil
}

func TestFeesChecker(t *testing.T) {
	src := fakeFees{
		exp: []ExpectedFee{
			{TradeID: 1, BaseCurrency: "EUR", QuoteCurrency: "USD",
				BuyerID: 7, SellerID: 8,
				BuyerFee: dec(t, "0.001"), SellerFee: dec(t, "0.002")},
		},
		col: []CollectedFee{
			{TradeID: 1, AccountID: 7, Currency: "EUR",
				Outflow: dec(t, "0.001")}, // buyer leg collected exactly
			// seller leg missing entirely
			{TradeID: 2, AccountID: 9, Currency: "USD",
				Outflow: dec(t, "0.005")}, // orphan ledger row
		},
		rev: []FeeRevenueRow{
			{TradeID: 1, Currency: "USD", Revenue: dec(t, "0.001")},
			// EUR revenue for trade 1 missing; USD revenue diverges
			// (expected 0.002).
		},
	}
	fs, err := FeesChecker{Src: src}.Run(context.Background(), Scope{})
	if err != nil {
		t.Fatal(err)
	}
	if len(fs) == 0 {
		t.Fatal("expected fee findings")
	}
	for _, f := range fs {
		if f.Severity != SevMismatch {
			t.Fatalf("fee divergence must mismatch: %+v", f)
		}
	}
}

func TestFeesCheckerEmptyIsClean(t *testing.T) {
	fs, err := FeesChecker{Src: fakeFees{}}.Run(context.Background(), Scope{})
	if err != nil {
		t.Fatal(err)
	}
	if len(fs) != 0 {
		t.Fatalf("no trades and no fee rows is a valid clean state: %+v", fs)
	}
}

// ---------------------------------------------------------------------------
// Checker error propagation
// ---------------------------------------------------------------------------

type errBalances struct{}

func (errBalances) WalletDiff(context.Context) (*recovery.RecReconcileResult, error) {
	return nil, errors.New("pg down")
}

func TestCheckerErrorPropagates(t *testing.T) {
	_, err := BalancesChecker{Src: errBalances{}}.Run(context.Background(), Scope{})
	if err == nil {
		t.Fatal("source error must propagate — engine records inconclusive")
	}
}
