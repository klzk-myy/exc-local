// Unit tests for Task 3.3.21 — cent-denominated sub-unit ledger accounting.
package ledger

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/shopspring/decimal"
)

func centProfile() ProductProfile {
	return ProductProfile{ProfileID: 2, Code: ProfileCodeCent,
		SubunitDivisor: SubunitDivisorCent, Status: "ACTIVE"}
}

// Minor-unit journal: amounts in cents still satisfy the per-currency
// zero-sum invariant (Journal.Validate is unit-agnostic).
func TestMinorUnitZeroSumInvariant(t *testing.T) {
	j := Journal{
		EntryType:   EntryDeposit,
		Description: "cent deposit",
		PostedBy:    "test",
		Lines: []Line{
			DebitLine(Nostro("USD"), "USD", d("10000"), ""), // $100.00 in cents
			CreditLine(CustomerLiability("USD"), "USD", d("10000"), ""),
		},
	}
	if err := VerifyMinorUnitZeroSum(j); err != nil {
		t.Fatalf("minor-unit journal must validate: %v", err)
	}
	if err := j.ValidateAccounts(DefaultChart()); err != nil {
		t.Fatalf("minor-unit lines resolve to the same CoA: %v", err)
	}

	// Imbalanced minor-unit journal still aborts.
	j.Lines[1].Credit = d("9999")
	if err := VerifyMinorUnitZeroSum(j); err == nil {
		t.Fatalf("imbalanced minor-unit journal must reject")
	} else {
		requireCode(t, err, CodeLedgerImbalanceAbort)
	}
}

// DisplayAmount divides by the account profile divisor — the single helper
// every read boundary shares.
func TestDisplayAmount(t *testing.T) {
	ctx := context.Background()
	cent := StaticProfileProvider{Profile: centProfile()}

	got, err := DisplayAmount(ctx, cent, 1, d("12345"))
	if err != nil {
		t.Fatalf("display: %v", err)
	}
	if want := d("123.45"); !got.Equal(want) {
		t.Fatalf("cent display %s != %s", got, want)
	}

	std := StaticProfileProvider{Profile: StandardProfile}
	got, err = DisplayAmount(ctx, std, 1, d("123.45"))
	if err != nil {
		t.Fatalf("display: %v", err)
	}
	if !got.Equal(d("123.45")) {
		t.Fatalf("standard display must be identity, got %s", got)
	}
}

// StorageAmount multiplies by the divisor — inverse of DisplayAmount.
func TestStorageAmount(t *testing.T) {
	got, err := StorageAmount(d("123.45"), SubunitDivisorCent)
	if err != nil {
		t.Fatalf("storage: %v", err)
	}
	if !got.Equal(d("12345")) {
		t.Fatalf("cent storage %s != 12345", got)
	}
	if _, err := StorageAmount(d("1"), 42); err == nil {
		t.Fatalf("divisor 42 must fail closed")
	} else {
		requireCode(t, err, CodeSubunitDivisorInvalid)
	}
}

// Profile switch STANDARD→CENT with any non-zero balance is rejected;
// all-zero is permitted; same-divisor is a no-op.
func TestProfileSwitchZeroBalanceGuard(t *testing.T) {
	std := StandardProfile
	cent := centProfile()

	// non-zero balance → INVALID_REQUEST
	err := ValidateProfileSwitch(std, cent, []SubunitBalance{
		{Currency: "USD", Total: decimal.Zero},
		{Currency: "EUR", Total: d("0.01")},
	})
	if err == nil {
		t.Fatalf("non-zero balance switch must reject")
	} else {
		requireCode(t, err, CodeInvalidRequest)
	}

	// all-zero balances → allowed
	if err := ValidateProfileSwitch(std, cent, []SubunitBalance{
		{Currency: "USD", Total: decimal.Zero},
		{Currency: "EUR", Total: decimal.Zero},
	}); err != nil {
		t.Fatalf("zero-balance switch must pass: %v", err)
	}

	// same divisor → no-op even with balances
	if err := ValidateProfileSwitch(std, std, []SubunitBalance{
		{Currency: "USD", Total: d("100")},
	}); err != nil {
		t.Fatalf("same-profile switch must be a no-op: %v", err)
	}

	// invalid target divisor → fail closed
	bad := ProductProfile{Code: "MILLI", SubunitDivisor: 1000}
	if err := ValidateProfileSwitch(std, bad, nil); err == nil {
		t.Fatalf("divisor 1000 profile must fail closed")
	} else {
		requireCode(t, err, CodeSubunitDivisorInvalid)
	}
}

// An unvalidated profile surfaces through DisplayAmount too — a corrupt
// divisor must never silently convert client-visible amounts.
func TestDisplayAmountRejectsBadDivisor(t *testing.T) {
	bad := StaticProfileProvider{Profile: ProductProfile{Code: "X", SubunitDivisor: 7}}
	if _, err := DisplayAmount(context.Background(), bad, 1, d("1")); err == nil {
		t.Fatalf("bad divisor must fail closed")
	} else {
		requireCode(t, err, CodeSubunitDivisorInvalid)
	}
}

// Sweep-date helper sanity (dust sweep day anchor — shared file context).
func TestDayAnchorUTC(t *testing.T) {
	ts := time.Date(2026, 9, 30, 23, 59, 59, 0, time.FixedZone("X", -5*3600))
	// 23:59:59 at UTC-5 = 04:59:59 UTC Oct 1 → day must be Oct 1 UTC.
	got := dayStartUTC(ts)
	if got.Format("2006-01-02") != "2026-10-01" {
		t.Fatalf("day anchor %s", got)
	}
	if fmt.Sprint(got.Location()) != "UTC" {
		t.Fatalf("day anchor must be UTC")
	}
}
