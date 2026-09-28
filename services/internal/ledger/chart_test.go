package ledger

import (
	"strings"
	"testing"
)

// The chart is the resolution contract: every code builder must produce a
// seeded code, and every seed must fit chart_of_accounts.account_code
// VARCHAR(48) and carry a currency.
func TestAllBuildersResolveInDefaultChart(t *testing.T) {
	ch := DefaultChart()
	builders := map[string]func(string) string{
		"Nostro":                    Nostro,
		"NostroClearing":            NostroClearing,
		"MultiCcyClearing":          MultiCcyClearing,
		"ClientMoneySegregated":     ClientMoneySegregated,
		"InsuranceFundNostro":       InsuranceFundNostro,
		"CustomerLiability":         CustomerLiability,
		"PendingSettlementDelivery": PendingSettlementDelivery,
		"ClientCollateral":          ClientCollateral,
		"SuspenseDeposits":          SuspenseDeposits,
		"ClearingTransit":           ClearingTransit,
		"InsuranceFundLiability":    InsuranceFundLiability,
		"HouseEquity":               HouseEquity,
		"RetainedEarnings":          RetainedEarnings,
		"TradingFeeRevenue":         TradingFeeRevenue,
		"SwapfreeAdminRevenue":      SwapfreeAdminRevenue,
		"CommissionRevenue":         CommissionRevenue,
		"SwapRolloverRevenue":       SwapRolloverRevenue,
		"SwapMarkupRevenue":         SwapMarkupRevenue,
		"FundingFeeRevenue":         FundingFeeRevenue,
		"SwapfreeAdminFee4300":      SwapfreeAdminFee4300,
		"ConversionSpreadRevenue":   ConversionSpreadRevenue,
		"InactivityFeeRevenue":      InactivityFeeRevenue,
		"LiquidationPenalty":        LiquidationPenalty,
		"LiquidityRebateExpense":    LiquidityRebateExpense,
		"NBPRestitutionExpense":     NBPRestitutionExpense,
		"BankRailFeeExpense":        BankRailFeeExpense,
	}
	for name, fn := range builders {
		for _, ccy := range SeedCurrencies {
			code := fn(ccy)
			if len(code) > 48 {
				t.Fatalf("%s(%s) = %q exceeds VARCHAR(48)", name, ccy, code)
			}
			a, ok := ch.Lookup(code)
			if !ok {
				t.Fatalf("%s(%s) = %q not seeded in chart", name, ccy, code)
			}
			if a.Currency != ccy {
				t.Fatalf("%s(%s): seeded currency %s", name, ccy, a.Currency)
			}
		}
	}
}

// Structural client-vs-house segregation (§5.21a): the number block is the
// boundary, never a query filter.
func TestSegregationBoundaries(t *testing.T) {
	client := []string{
		CustomerLiability("USD"), PendingSettlementDelivery("USD"),
		ClientCollateral("USD"), SuspenseDeposits("USD"), ClearingTransit("USD"),
		ClientMoneySegregated("USD"),
	}
	house := []string{
		Nostro("USD"), NostroClearing("USD"), MultiCcyClearing("USD"),
		InsuranceFundNostro("USD"), InsuranceFundLiability("USD"),
		HouseEquity("USD"), TradingFeeRevenue("USD"), SwapMarkupRevenue("USD"),
		LiquidityRebateExpense("USD"),
	}
	for _, c := range client {
		if !IsClient(c) {
			t.Fatalf("%s must classify CLIENT", c)
		}
	}
	for _, c := range house {
		if IsClient(c) {
			t.Fatalf("%s must classify HOUSE", c)
		}
	}
	// Malformed code fails closed to HOUSE.
	if IsClient("garbage") {
		t.Fatal("malformed code classified as client")
	}
}

// The 036 seed set is a strict subset of the production chart (§5.21a).
func Test036SeedSubsetOf088(t *testing.T) {
	prod := DefaultChart()
	for _, a := range Seed036Accounts() {
		got, ok := prod.Lookup(a.Code)
		if !ok {
			t.Fatalf("036 seed %q missing from production chart", a.Code)
		}
		if got.Type != a.Type {
			t.Fatalf("%s type drifted: 036=%s 088=%s", a.Code, a.Type, got.Type)
		}
	}
}

// Codes follow "{NNNN}_{NAME}_{CCY}".
func TestCodeShape(t *testing.T) {
	for _, a := range Seed088Accounts() {
		num, rest, ok := parseCode(a.Code)
		if !ok || num < 1000 || num > 9999 {
			t.Fatalf("bad code shape %q", a.Code)
		}
		if !strings.HasSuffix(rest, "_"+a.Currency) {
			t.Fatalf("%q missing currency suffix", a.Code)
		}
	}
}
