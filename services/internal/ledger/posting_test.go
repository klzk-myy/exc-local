package ledger

import (
	stderrors "errors"
	"testing"

	"github.com/shopspring/decimal"

	excerrors "exchange/pkg/errors"
)

func d(s string) decimal.Decimal { return decimal.RequireFromString(s) }

// requireCode fails unless err carries the expected machine code.
func requireCode(t *testing.T, err error, code string) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected error %s, got nil", code)
	}
	var e *excerrors.Error
	if !stderrors.As(err, &e) || e.Code != code {
		t.Fatalf("expected code %s, got %v", code, err)
	}
}

func depositJournal() Journal {
	return Journal{
		EntryType:   EntryDeposit,
		ReferenceID: 42,
		Description: "test deposit",
		PostedBy:    "test",
		Lines: []Line{
			DebitLine(Nostro("USD"), "USD", d("100"), "nostro in"),
			CreditLine(CustomerLiability("USD"), "USD", d("100"), "client credit"),
		},
		Effects: []AccountEffect{{
			AccountID:      7,
			Currency:       "USD",
			AvailableDelta: d("100"),
		}},
	}
}

func TestValidateBalancedJournal(t *testing.T) {
	j := depositJournal()
	if err := j.Validate(); err != nil {
		t.Fatalf("balanced journal rejected: %v", err)
	}
	if err := j.ValidateAccounts(DefaultChart()); err != nil {
		t.Fatalf("seeded accounts rejected: %v", err)
	}
}

func TestValidateImbalanceAborts(t *testing.T) {
	j := depositJournal()
	j.Lines[1].Credit = d("99.99999999") // one quantum short
	requireCode(t, j.Validate(), CodeLedgerImbalanceAbort)
}

func TestValidateImbalancePerCurrency(t *testing.T) {
	// USD leg balanced, EUR leg unbalanced — per-currency rule catches it.
	j := depositJournal()
	j.Lines = append(j.Lines,
		DebitLine(Nostro("EUR"), "EUR", d("5"), ""),
		CreditLine(CustomerLiability("EUR"), "EUR", d("4"), ""),
	)
	requireCode(t, j.Validate(), CodeLedgerImbalanceAbort)
}

func TestValidateRejectsMalformedLines(t *testing.T) {
	j := depositJournal()
	cases := []Line{
		{AccountCode: Nostro("USD"), Currency: "USD"},                                // both zero
		{AccountCode: Nostro("USD"), Currency: "USD", Debit: d("1"), Credit: d("1")}, // two-sided
		{AccountCode: Nostro("USD"), Currency: "USD", Debit: d("-1")},                // negative
		{AccountCode: "", Currency: "USD", Debit: d("1")},                            // no account
		{AccountCode: Nostro("USD"), Currency: "usd", Debit: d("1")},                 // bad ccy
		{AccountCode: Nostro("USD"), Currency: "USD", Debit: d("1.000000001")},       // >8dp
	}
	for i, l := range cases {
		jj := j
		jj.Lines = append(append([]Line{}, j.Lines...), l)
		if err := jj.Validate(); err == nil {
			t.Fatalf("case %d: malformed line accepted: %+v", i, l)
		}
	}
}

func TestValidateRequiresTwoLines(t *testing.T) {
	j := depositJournal()
	j.Lines = j.Lines[:1]
	requireCode(t, j.Validate(), CodeLedgerInvalidJournal)
}

func TestValidateRejectsBadEntryType(t *testing.T) {
	j := depositJournal()
	j.EntryType = "MOONBEAM"
	requireCode(t, j.Validate(), CodeLedgerInvalidJournal)
}

func TestValidateAccountsUnknownAborts(t *testing.T) {
	j := depositJournal()
	j.Lines[0].AccountCode = "9999_NOT_AN_ACCOUNT_USD"
	requireCode(t, j.ValidateAccounts(DefaultChart()), CodeLedgerUnknownAccount)
}

func TestValidateAccountsCurrencyMismatch(t *testing.T) {
	j := depositJournal()
	j.Lines[0].Currency = "EUR" // 1010_NOSTRO_USD is USD-denominated
	if err := j.ValidateAccounts(DefaultChart()); err == nil {
		t.Fatal("currency-mismatched line accepted")
	}
}

func TestValidateAccountsNilChartFailsClosed(t *testing.T) {
	requireCode(t, depositJournal().ValidateAccounts(nil), CodeLedgerInvalidJournal)
}

func TestAffectedAccountsSortedUnique(t *testing.T) {
	j := depositJournal()
	j.Effects = append(j.Effects,
		AccountEffect{AccountID: 3, Currency: "EUR", AvailableDelta: d("-1")},
		AccountEffect{AccountID: 1, Currency: "USD", AvailableDelta: d("1")},
	)
	got := j.AffectedAccounts()
	want := []int64{1, 3, 7}
	if len(got) != len(want) {
		t.Fatalf("got %v want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v want %v", got, want)
		}
	}
}

func TestLedgerEntryTypeMapping(t *testing.T) {
	if EntryEODRollover.LedgerEntryType() != "ROLLOVER" {
		t.Fatal("EOD_ROLLOVER must map to ledger_entries 'ROLLOVER' (§5.3 enum)")
	}
	if EntryTransfer.LedgerEntryType() != "TRANSFER" {
		t.Fatal("TRANSFER must map 1:1")
	}
}

func TestBalanceChangedSubject(t *testing.T) {
	if got := BalanceChangedSubject(123); got != "account.balance.changed.123" {
		t.Fatalf("subject %q", got)
	}
}
