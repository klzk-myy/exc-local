package ledger

import (
	"fmt"
	"strconv"
	"strings"
)

// AccountType mirrors gl_account_type_enum (spec §5.21).
type AccountType string

const (
	AccountAsset     AccountType = "ASSET"
	AccountLiability AccountType = "LIABILITY"
	AccountEquity    AccountType = "EQUITY"
	AccountRevenue   AccountType = "REVENUE"
	AccountExpense   AccountType = "EXPENSE"
)

// Account is one chart_of_accounts row: a numbered code plus a
// per-currency sub-account suffix ("1010_NOSTRO_USD").
type Account struct {
	Code     string
	Name     string
	Type     AccountType
	Currency string
}

// Segregation is the structural client-vs-house split of the numbering
// plan (spec §5.21a — segregation lives in the number ranges, never in a
// query filter or flag column):
//
//	1100–1149  client-segregated money assets      → SegregationClient
//	2000–2199  client liabilities & transit        → SegregationClient
//	everything else (house assets/equity/revenue/expense,
//	1150+ restricted assets, 2200+ house liabilities) → SegregationHouse
type Segregation int

const (
	// SegregationClient — balances held for clients: client liabilities and
	// segregated client-money bank accounts (spec §17.9 safeguarding side).
	SegregationClient Segregation = iota
	// SegregationHouse — own funds: operating nostro, equity, revenue,
	// expense, insurance-fund commitments (§17.13 — insurance fund is own
	// capital committed to client protection, structurally NOT client money).
	SegregationHouse
)

// String labels the bucket for logs/reporting.
func (s Segregation) String() string {
	if s == SegregationClient {
		return "CLIENT"
	}
	return "HOUSE"
}

// SegregationOf classifies an account code by its numeric prefix. Unknown
// or malformed codes classify HOUSE — a mislabelled client-side line would
// fail a safeguarding reconciliation LOUDLY rather than silently passing
// for client money (fail-closed classification).
func SegregationOf(code string) Segregation {
	num, _, ok := parseCode(code)
	if !ok {
		return SegregationHouse
	}
	switch {
	case num >= 1100 && num <= 1149:
		return SegregationClient // client money assets
	case num >= 2000 && num <= 2199:
		return SegregationClient // client liabilities / suspense / transit
	default:
		return SegregationHouse
	}
}

// IsClient reports whether the account sits on the client side of the
// safeguarding boundary.
func IsClient(code string) bool { return SegregationOf(code) == SegregationClient }

// parseCode splits "2010_CUSTOMER_LIABILITY_USD" into (2010,
// "CUSTOMER_LIABILITY_USD", true). Codes are "{NNNN}_{NAME}_{CCY}".
func parseCode(code string) (int, string, bool) {
	head, rest, found := strings.Cut(code, "_")
	if !found {
		return 0, "", false
	}
	num, err := strconv.Atoi(head)
	if err != nil || num < 1000 || num > 9999 {
		return 0, "", false
	}
	return num, rest, true
}

// code builds a per-currency sub-account code.
func code(prefix, ccy string) string { return prefix + "_" + ccy }

// ---------------------------------------------------------------------------
// Canonical code builders — the single naming source for every poster.
// Keep in sync with the seeds in migrations 036/088.
// ---------------------------------------------------------------------------

// House assets (1xxx).
func Nostro(ccy string) string         { return code("1010_NOSTRO", ccy) }
func NostroClearing(ccy string) string { return code("1020_NOSTRO_CLEARING", ccy) }
func MultiCcyClearing(ccy string) string { // Task 3.3.14 "1200-MULTI-CURRENCY-CLEARING"
	return code("1200_MULTI_CURRENCY_CLEARING", ccy)
}

// Client money assets (1100–1149, SegregationClient per §17.9) and
// restricted house assets (1150+, SegregationHouse).
func ClientMoneySegregated(ccy string) string { return code("1110_CLIENT_MONEY_SEGREGATED", ccy) }
func InsuranceFundNostro(ccy string) string   { return code("1150_INSURANCE_FUND_NOSTRO", ccy) }

// Client liabilities (2000–2199).
func CustomerLiability(ccy string) string { return code("2010_CUSTOMER_LIABILITY", ccy) }
func PendingSettlementDelivery(ccy string) string { // Task 3.3.22 physical-delivery lock
	return code("2011_PENDING_SETTLEMENT_DELIVERY", ccy)
}
func ClientCollateral(ccy string) string { // Task 3.3.14 "2100-CLIENT-COLLATERAL"
	return code("2100_CLIENT_COLLATERAL", ccy)
}
func SuspenseDeposits(ccy string) string { // §5.46 default gl_account '2150'
	return code("2150_SUSPENSE_DEPOSITS", ccy)
}
func ClearingTransit(ccy string) string { return code("2160_CLEARING_TRANSIT", ccy) }

// PAMMPoolLiability is the pooled-capital obligation of internal PAMM
// investment accounts — the GL mirror of the dedicated PAMM sub-ledger
// (Task 14.3.8). It stays inside the 2000–2199 client-liability range
// because pooled capital remains client money; a distinct code keeps
// invested capital OUT of 2010 so daily fiat withdrawal-cap accounting
// never sees it.
func PAMMPoolLiability(ccy string) string { return code("2170_PAMM_POOL_LIABILITY", ccy) }

// House liabilities (2200+).
func InsuranceFundLiability(ccy string) string {
	return code("2210_INSURANCE_FUND_LIABILITY", ccy)
}

// House equity (3xxx).
func HouseEquity(ccy string) string      { return code("3010_HOUSE_EQUITY", ccy) }
func RetainedEarnings(ccy string) string { return code("3020_RETAINED_EARNINGS", ccy) }

// House revenue (4xxx). NOTE: spec §5.45.3 cites "GL 4300" for swap-free
// admin fees while Phase-03 Task 3.3.23 cites 4020_SWAPFREE_ADMIN_REVENUE —
// BOTH codes are seeded until the contradiction is ruled on (spec §27).
func TradingFeeRevenue(ccy string) string    { return code("4010_TRADING_FEE_REVENUE", ccy) }
func SwapfreeAdminRevenue(ccy string) string { return code("4020_SWAPFREE_ADMIN_REVENUE", ccy) }
func CommissionRevenue(ccy string) string    { return code("4030_COMMISSION_REVENUE", ccy) }
func SwapRolloverRevenue(ccy string) string  { return code("4100_SWAP_ROLLOVER_REVENUE", ccy) }
func SwapMarkupRevenue(ccy string) string    { return code("4110_SWAP_MARKUP_REVENUE", ccy) }
func FundingFeeRevenue(ccy string) string    { return code("4200_FUNDING_FEE_REVENUE", ccy) }
func SwapfreeAdminFee4300(ccy string) string { return code("4300_SWAPFREE_ADMIN_FEE", ccy) }
func ConversionSpreadRevenue(ccy string) string {
	return code("4400_CONVERSION_SPREAD_REVENUE", ccy)
}
func InactivityFeeRevenue(ccy string) string {
	return code("4500_INACTIVITY_FEE_REVENUE", ccy)
}

// House expense (5xxx).
func LiquidationPenalty(ccy string) string     { return code("5010_LIQUIDATION_PENALTY", ccy) }
func LiquidityRebateExpense(ccy string) string { return code("5100_LIQUIDITY_REBATE_EXPENSE", ccy) }
func NBPRestitutionExpense(ccy string) string  { return code("5200_NBP_RESTITUTION_EXPENSE", ccy) }
func BankRailFeeExpense(ccy string) string     { return code("5300_BANK_RAIL_FEE_EXPENSE", ccy) }

// ---------------------------------------------------------------------------
// Seed descriptors — mirror of migrations 036 + 088 (+ 216 for
// 2170_PAMM_POOL_LIABILITY) INSERT sets. Anything
// posted through these builders must resolve to a seeded row; DefaultChart
// is built from this list and is the offline validation fixture.
// ---------------------------------------------------------------------------

// accountSeed is one per-currency account template.
type accountSeed struct {
	Prefix string
	Name   string
	Type   AccountType
}

// seed036 is the Task 3.3.6 baseline (spec §5.21 examples) — a strict
// subset of the Task 3.3.19 production chart per §5.21a.
var seed036 = []accountSeed{
	{"1010_NOSTRO", "Nostro operating account", AccountAsset},
	{"2010_CUSTOMER_LIABILITY", "Customer balance liability", AccountLiability},
	{"4010_TRADING_FEE_REVENUE", "Trading fee revenue", AccountRevenue},
	{"5010_LIQUIDATION_PENALTY", "Liquidation penalty clearing", AccountExpense},
}

// seed088 is the Task 3.3.19 production chart (spec §5.21a): client
// liabilities, nostro clearing, suspense/clearing-transit, swap/rollover
// revenue, commission revenue, funding-fee revenue, liquidity-rebate
// expense, insurance-fund liability, house equity/retained earnings.
var seed088 = []accountSeed{
	// house assets
	{"1010_NOSTRO", "Nostro operating account", AccountAsset},
	{"1020_NOSTRO_CLEARING", "Nostro clearing / in-transit", AccountAsset},
	{"1200_MULTI_CURRENCY_CLEARING", "Multi-currency clearing (auto-exchange)", AccountAsset},
	// client-segregated money assets
	{"1110_CLIENT_MONEY_SEGREGATED", "Segregated client-money bank account", AccountAsset},
	// restricted house assets
	{"1150_INSURANCE_FUND_NOSTRO", "Insurance fund segregated nostro", AccountAsset},
	// client liabilities & transit
	{"2010_CUSTOMER_LIABILITY", "Customer balance liability", AccountLiability},
	{"2011_PENDING_SETTLEMENT_DELIVERY", "Physical-delivery pending settlement", AccountLiability},
	{"2100_CLIENT_COLLATERAL", "Client collateral held", AccountLiability},
	{"2150_SUSPENSE_DEPOSITS", "Suspense — unmatched deposits", AccountLiability},
	{"2160_CLEARING_TRANSIT", "Suspense — clearing transit", AccountLiability},
	{"2170_PAMM_POOL_LIABILITY", "PAMM pooled-investment liability", AccountLiability},
	// house liabilities
	{"2210_INSURANCE_FUND_LIABILITY", "Insurance fund liability", AccountLiability},
	// house equity
	{"3010_HOUSE_EQUITY", "House equity", AccountEquity},
	{"3020_RETAINED_EARNINGS", "Retained earnings", AccountEquity},
	// house revenue
	{"4010_TRADING_FEE_REVENUE", "Trading fee revenue", AccountRevenue},
	{"4020_SWAPFREE_ADMIN_REVENUE", "Swap-free admin fee revenue (Task 3.3.23)", AccountRevenue},
	{"4030_COMMISSION_REVENUE", "Commission revenue (raw-spread model)", AccountRevenue},
	{"4100_SWAP_ROLLOVER_REVENUE", "Swap/rollover interbank financing revenue", AccountRevenue},
	{"4110_SWAP_MARKUP_REVENUE", "Swap admin markup revenue", AccountRevenue},
	{"4200_FUNDING_FEE_REVENUE", "Funding fee revenue (deposits/withdrawals)", AccountRevenue},
	{"4300_SWAPFREE_ADMIN_FEE", "Swap-free admin fee revenue (§5.45 GL 4300)", AccountRevenue},
	{"4400_CONVERSION_SPREAD_REVENUE", "Currency conversion spread revenue", AccountRevenue},
	{"4500_INACTIVITY_FEE_REVENUE", "Inactivity/dormancy fee revenue", AccountRevenue},
	// house expense
	{"5010_LIQUIDATION_PENALTY", "Liquidation penalty clearing", AccountExpense},
	{"5100_LIQUIDITY_REBATE_EXPENSE", "Liquidity-provider rebate expense", AccountExpense},
	{"5200_NBP_RESTITUTION_EXPENSE", "Retail NBP restitution expense", AccountExpense},
	{"5300_BANK_RAIL_FEE_EXPENSE", "Banking-rail fee expense", AccountExpense},
}

// SeedCurrencies are the fiat currencies the CoA seeds per spec §5.1
// instrument coverage (superset of seed instruments, migration 001).
var SeedCurrencies = []string{"USD", "EUR", "GBP", "JPY", "AUD", "CAD", "CHF", "NZD", "MXN"}

// Seed036Accounts expands the Task 3.3.6 seed set over SeedCurrencies.
func Seed036Accounts() []Account { return expandSeeds(seed036) }

// Seed088Accounts expands the full Task 3.3.19 chart over SeedCurrencies
// (contains the 036 set as a subset).
func Seed088Accounts() []Account { return expandSeeds(seed088) }

func expandSeeds(seeds []accountSeed) []Account {
	out := make([]Account, 0, len(seeds)*len(SeedCurrencies))
	for _, s := range seeds {
		for _, ccy := range SeedCurrencies {
			out = append(out, Account{
				Code:     code(s.Prefix, ccy),
				Name:     fmt.Sprintf("%s (%s)", s.Name, ccy),
				Type:     s.Type,
				Currency: ccy,
			})
		}
	}
	return out
}

// Chart is an immutable account_code → Account index used to resolve
// postings before commit. The production instance is loaded from
// chart_of_accounts inside the posting transaction; DefaultChart is the
// offline/test fixture mirroring the migration seeds.
type Chart struct {
	byCode map[string]Account
}

// NewChart indexes accs; later duplicates overwrite earlier rows (the
// seed lists are duplicate-free — 036 is a strict subset of 088).
func NewChart(accs []Account) *Chart {
	c := &Chart{byCode: make(map[string]Account, len(accs))}
	for _, a := range accs {
		c.byCode[a.Code] = a
	}
	return c
}

// Lookup resolves an account code.
func (c *Chart) Lookup(code string) (Account, bool) {
	a, ok := c.byCode[code]
	return a, ok
}

// Len reports the number of accounts in the chart.
func (c *Chart) Len() int { return len(c.byCode) }

// DefaultChart returns the full production seed chart (036 ∪ 088 — the
// 088 set already contains the 036 subset).
func DefaultChart() *Chart { return NewChart(Seed088Accounts()) }

// DefaultChart036 returns only the Task 3.3.6 baseline seed set.
func DefaultChart036() *Chart { return NewChart(Seed036Accounts()) }
