// subunit.go — Cent-Denominated Sub-Unit Ledger Accounting
// (Phase-03 Task 3.3.21; spec §5.41, §24 #370; migration 096 —
// convention-only, no tables).
//
// Accounts whose product profile (account_product_profiles, migration 095 —
// Phase-14 Task 14.3.13) carries subunit_divisor = 100 (seed code 'CENT')
// store balances/ledger_lines amounts in MINOR units (cents). STANDARD
// profiles (divisor 1) store major units. The two never mix within an
// account, and the CoA is unchanged — minor-unit postings resolve to the
// same chart_of_accounts codes.
//
// This file owns the three rules:
//
//  1. Minor-unit posting — the per-currency zero-sum invariant
//     (Journal.Validate + gl_journal_zero_sum_chk) is unit-agnostic and is
//     asserted to hold on minor-unit journals exactly as on major-unit
//     ones (VerifyMinorUnitZeroSum).
//  2. STANDARD↔CENT profile switching is permitted only when EVERY balance
//     row of the account is zero — else INVALID_REQUEST (dust-convert,
//     Task 3.3.20, is the pre-switch path for stranded minors).
//  3. One shared display helper — DisplayAmount divides stored amounts by
//     the account's profile divisor at every read boundary (statements,
//     snapshots, tax tool, margin equity); no per-consumer conversion
//     logic may exist.
package ledger

import (
	"context"
	"fmt"

	"github.com/shopspring/decimal"

	excerrors "exchange/pkg/errors"
)

// Scaffold code — register in Phase-05 Task 5.3.21 (spec §23 INVALID_REQUEST
// is the canonical rejection for non-zero-balance switches; this code marks
// divisor-domain violations).
const CodeSubunitDivisorInvalid = "SUBUNIT_DIVISOR_INVALID"

// Canonical product-profile divisors (spec §5.41: "subunit_divisor (1
// standard, 100 cent)").
const (
	SubunitDivisorStandard int64 = 1
	SubunitDivisorCent     int64 = 100
)

// ProfileCode enumerates the seeded profile codes (migration 095).
const (
	ProfileCodeStandard = "STANDARD"
	ProfileCodeCent     = "CENT"
)

// ProductProfile is the engine's view of one account_product_profiles row.
type ProductProfile struct {
	ProfileID      int64
	Code           string // STANDARD | CENT (seeds; further codes allowed)
	SubunitDivisor int64  // storage divisor for balances/ledger_lines
	PricingPlan    string // §5.41 — consumed by the Task 3.3.13 engine
	Status         string
}

// ValidDivisor reports whether d is in the sub-unit domain {1, 100}.
func ValidDivisor(d int64) bool {
	return d == SubunitDivisorStandard || d == SubunitDivisorCent
}

// Validate enforces the profile's divisor domain — fail-closed: an unknown
// divisor means no reader can trust a single stored amount.
func (p ProductProfile) Validate() error {
	if !ValidDivisor(p.SubunitDivisor) {
		return excerrors.New(CodeSubunitDivisorInvalid, fmt.Sprintf(
			"profile %d (%s) subunit_divisor=%d not in {1,100}",
			p.ProfileID, p.Code, p.SubunitDivisor))
	}
	return nil
}

// IsCent reports whether the profile stores minor units.
func (p ProductProfile) IsCent() bool { return p.SubunitDivisor == SubunitDivisorCent }

// ProductProfileProvider resolves an account's product profile — the seam
// over accounts.product_profile_id → account_product_profiles (migration
// 095). Implementations must return a validated profile or an error; a
// missing profile row defaults nowhere silently — callers use
// StandardProfile for the documented pre-095 default.
type ProductProfileProvider interface {
	ProfileForAccount(ctx context.Context, accountID int64) (ProductProfile, error)
}

// StandardProfile is the canonical default: every pre-095 account is
// STANDARD (accounts.product_profile_id defaults to it per §5.41).
var StandardProfile = ProductProfile{
	Code: ProfileCodeStandard, SubunitDivisor: SubunitDivisorStandard,
	PricingPlan: "SPREAD_MARKUP", Status: "ACTIVE",
}

// StaticProfileProvider returns one profile for every account — test
// fixture and the pre-095 all-STANDARD default.
type StaticProfileProvider struct{ Profile ProductProfile }

// ProfileForAccount implements ProductProfileProvider.
func (s StaticProfileProvider) ProfileForAccount(context.Context, int64) (ProductProfile, error) {
	return s.Profile, s.Profile.Validate()
}

// ---------------------------------------------------------------------------
// Read/write unit conversion — the single helper every boundary uses.
// ---------------------------------------------------------------------------

// DisplayAmount converts a STORED amount to major units for presentation:
// stored / divisor. This is the only place that division happens —
// statements (Task 20.3.6), snapshots (Task 20.3.13), the tax tool
// (Task 20.3.10) and margin equity (Task 19.3.8) all read through it.
func DisplayAmount(ctx context.Context, profiles ProductProfileProvider, accountID int64, stored decimal.Decimal) (decimal.Decimal, error) {
	if profiles == nil {
		return decimal.Zero, excerrors.New(CodeSubunitDivisorInvalid,
			"display conversion requires a profile provider")
	}
	p, err := profiles.ProfileForAccount(ctx, accountID)
	if err != nil {
		return decimal.Zero, err
	}
	if err := p.Validate(); err != nil {
		return decimal.Zero, err
	}
	return stored.Div(decimal.NewFromInt(p.SubunitDivisor)), nil
}

// StorageAmount converts a MAJOR-unit display amount into the account's
// storage units: display × divisor. Writers call this before building
// effects/journal lines so postings stay in the profile's units.
func StorageAmount(display decimal.Decimal, divisor int64) (decimal.Decimal, error) {
	if !ValidDivisor(divisor) {
		return decimal.Zero, excerrors.New(CodeSubunitDivisorInvalid,
			fmt.Sprintf("subunit_divisor=%d not in {1,100}", divisor))
	}
	return display.Mul(decimal.NewFromInt(divisor)), nil
}

// ---------------------------------------------------------------------------
// Minor-unit invariant + profile switching
// ---------------------------------------------------------------------------

// VerifyMinorUnitZeroSum asserts the §5.21 per-currency zero-sum invariant
// on a journal whose lines are denominated in MINOR units — the invariant
// is unit-agnostic, so this is Journal.Validate under an explicit name so
// tests and auditors can state the requirement directly (Task 3.3.21 AC:
// "the invariant holds in minor units").
func VerifyMinorUnitZeroSum(j Journal) error {
	if err := j.Validate(); err != nil {
		return err
	}
	return nil
}

// SubunitBalance is the (currency, total) pair the switch guard inspects —
// total = balances.total in the account's CURRENT storage units.
type SubunitBalance struct {
	Currency string
	Total    decimal.Decimal
}

// ValidateProfileSwitch gates STANDARD↔CENT transitions (spec §5.41.2):
// permitted only when every balance row is zero — a non-zero total in ANY
// currency rejects with INVALID_REQUEST, since a mid-flight unit change
// would corrupt the zero-sum ledger. Switching to the profile the account
// already holds is a no-op and always allowed.
func ValidateProfileSwitch(current, target ProductProfile, balances []SubunitBalance) error {
	if err := current.Validate(); err != nil {
		return err
	}
	if err := target.Validate(); err != nil {
		return err
	}
	if current.SubunitDivisor == target.SubunitDivisor {
		return nil // same unit convention — nothing to convert
	}
	for _, b := range balances {
		if !b.Total.IsZero() {
			return excerrors.New(CodeInvalidRequest, fmt.Sprintf(
				"profile switch %s→%s requires zero balances: %s has total %s",
				current.Code, target.Code, b.Currency, b.Total.String()))
		}
	}
	return nil
}
