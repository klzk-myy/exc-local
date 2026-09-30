// transfer_ledger_test.go — unit tests for the §13.9 position-transfer
// journal builder (Phase-19 Task 19.3.12; spec §5.38, §5.3 zero-GL-bypass).
package accounting

import (
	"errors"
	"strings"
	"testing"

	"exchange/internal/ledger"
	"exchange/pkg/decimal"
	excerrors "exchange/pkg/errors"
)

func d(s string) decimal.Decimal { return decimal.MustFromString(s) }

func basePosting() TransferPosting {
	return TransferPosting{
		TransferID:     7,
		FromAccountID:  1001,
		ToAccountID:    1002,
		InstrumentID:   55,
		Symbol:         "EUR/USD",
		Side:           "LONG",
		Currency:       "USD",
		Quantity:       d("100000"),
		TransferPrice:  d("1.10000000"),
		SourceRelease:  d("3666.66666667"), // pro-rata margin release
		DestLock:       d("3666.66666667"), // hedge-mode dest lock
		RealizedPnL:    d("250.00"),
		AdminFee:       d("10.00"),
		PostedBy:       "position-transfer",
		IdempotencyKey: "position-transfer:7",
	}
}

// journalSums recomputes per-currency debit/credit totals for assertions.
func journalSums(t *testing.T, j ledger.Journal) (debit, credit decimal.Decimal) {
	t.Helper()
	for _, l := range j.Lines {
		if l.Currency != "USD" {
			t.Fatalf("unexpected line currency %s", l.Currency)
		}
		debit = debit.Add(l.Debit)
		credit = credit.Add(l.Credit)
	}
	return debit, credit
}

func TestTransferJournalBalancedFullEconomics(t *testing.T) {
	j, err := BuildTransferJournal(basePosting())
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	// 3 leg-pairs: release + dest lock + gain + fee = 8 lines.
	if len(j.Lines) != 8 {
		t.Fatalf("lines=%d want 8", len(j.Lines))
	}
	dr, cr := journalSums(t, j)
	if !dr.Equal(cr) {
		t.Fatalf("unbalanced: dr=%s cr=%s", dr, cr)
	}
	if j.EntryType != ledger.EntryTransfer {
		t.Fatalf("entry_type=%s want TRANSFER", j.EntryType)
	}
	if j.ReferenceID != 7 || j.IdempotencyKey != "position-transfer:7" {
		t.Fatalf("reference/idempotency not propagated: %+v", j)
	}
	// Account codes follow the canonical chart helpers.
	wantCodes := map[string]bool{
		ledger.ClientCollateral("USD"):  true,
		ledger.CustomerLiability("USD"): true,
		ledger.RetainedEarnings("USD"):  true,
		ledger.TradingFeeRevenue("USD"): true,
	}
	for _, l := range j.Lines {
		if !wantCodes[l.AccountCode] {
			t.Fatalf("unexpected account code %s", l.AccountCode)
		}
	}
	// Wallet effects: source available += release + pnl − fee, locked −= release.
	if len(j.Effects) != 2 {
		t.Fatalf("effects=%d want 2", len(j.Effects))
	}
	src, dst := j.Effects[0], j.Effects[1]
	if src.AccountID != 1001 || dst.AccountID != 1002 {
		t.Fatalf("effect account ids wrong: %+v", j.Effects)
	}
	if !src.AvailableDelta.Equal(d("3666.66666667").Add(d("250.00")).Sub(d("10.00"))) {
		t.Fatalf("src avail delta %s", src.AvailableDelta)
	}
	if !src.LockedDelta.Equal(d("-3666.66666667")) {
		t.Fatalf("src locked delta %s", src.LockedDelta)
	}
	if !dst.AvailableDelta.Equal(d("-3666.66666667")) || !dst.LockedDelta.Equal(d("3666.66666667")) {
		t.Fatalf("dst deltas %s / %s", dst.AvailableDelta, dst.LockedDelta)
	}
	// Wallet net movements: source gains (release is a reclass — net is
	// pnl − fee); destination is a pure available→locked reclass (net 0).
	if !src.Net().Equal(d("240.00")) {
		t.Fatalf("src net %s want 240", src.Net())
	}
	if !dst.Net().IsZero() {
		t.Fatalf("dst net %s want 0 (pure collateral reclass)", dst.Net())
	}
}

func TestTransferJournalNettingAbsorbFreesDestMargin(t *testing.T) {
	p := basePosting()
	p.DestLock = d("-1200.00") // NETTING dest absorbed into opposing position
	j, err := BuildTransferJournal(p)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	dr, cr := journalSums(t, j)
	if !dr.Equal(cr) {
		t.Fatalf("unbalanced: dr=%s cr=%s", dr, cr)
	}
	dst := j.Effects[1]
	// Freed margin: dest locked decreases, available increases.
	if !dst.LockedDelta.Equal(d("-1200.00")) || !dst.AvailableDelta.Equal(d("1200.00")) {
		t.Fatalf("dst deltas %s / %s", dst.AvailableDelta, dst.LockedDelta)
	}
}

func TestTransferJournalRealizedLossReverses(t *testing.T) {
	p := basePosting()
	p.RealizedPnL = d("-75.50")
	j, err := BuildTransferJournal(p)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	dr, cr := journalSums(t, j)
	if !dr.Equal(cr) {
		t.Fatalf("unbalanced: dr=%s cr=%s", dr, cr)
	}
	// Loss leg: DR customer liability / CR retained earnings.
	var lossLeg bool
	for i := 0; i+1 < len(j.Lines); i++ {
		if j.Lines[i].AccountCode == ledger.CustomerLiability("USD") &&
			j.Lines[i+1].AccountCode == ledger.RetainedEarnings("USD") &&
			j.Lines[i].Debit.Equal(d("75.50")) {
			lossLeg = true
		}
	}
	if !lossLeg {
		t.Fatal("missing DR-liability/CR-equity loss leg")
	}
	src := j.Effects[0]
	// src available: +release + (−75.50 pnl) − fee
	if !src.AvailableDelta.Equal(d("3666.66666667").Sub(d("75.50")).Sub(d("10.00"))) {
		t.Fatalf("src avail delta %s", src.AvailableDelta)
	}
}

func TestTransferJournalZeroEconomicsFailsClosed(t *testing.T) {
	p := basePosting()
	p.SourceRelease, p.DestLock, p.RealizedPnL, p.AdminFee =
		decimal.Zero, decimal.Zero, decimal.Zero, decimal.Zero
	if _, err := BuildTransferJournal(p); err == nil {
		t.Fatal("empty journal must fail (Journal.Validate requires ≥2 lines)")
	}
}

func TestTransferJournalValidation(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*TransferPosting)
	}{
		{"same account", func(p *TransferPosting) { p.ToAccountID = p.FromAccountID }},
		{"bad currency", func(p *TransferPosting) { p.Currency = "US" }},
		{"zero qty", func(p *TransferPosting) { p.Quantity = decimal.Zero }},
		{"zero price", func(p *TransferPosting) { p.TransferPrice = decimal.Zero }},
		{"negative release", func(p *TransferPosting) { p.SourceRelease = d("-1") }},
		{"negative fee", func(p *TransferPosting) { p.AdminFee = d("-0.01") }},
		{"missing transfer id", func(p *TransferPosting) { p.TransferID = 0 }},
		{"long idem key", func(p *TransferPosting) { p.IdempotencyKey = strings.Repeat("k", 129) }},
	}
	for _, tc := range cases {
		p := basePosting()
		tc.mutate(&p)
		_, err := BuildTransferJournal(p)
		if err == nil {
			t.Fatalf("%s: expected rejection", tc.name)
		}
		var e *excerrors.Error
		if !errors.As(err, &e) || e.Code != CodeTransferJournalInvalid {
			t.Fatalf("%s: code=%v want %s", tc.name, err, CodeTransferJournalInvalid)
		}
	}
}
