// product_gate.go — order-entry product gate (Phase-14 Tasks 14.3.13
// instrument_scope + 14.3.16 retail target market; spec §5.41, §14.12,
// §24 #369/#376).
//
// ProductGateService satisfies orders.ProductGate and is consulted on
// every new-order admission path (Submit / Batch entries / Modify /
// CancelReplace — cancels and qty-down keep-priority amends never reach
// it). The gate runs, in order:
//
//  1. instrument_scope — the class must be allowlisted by the account's
//     product profile; a narrowed scope rejects new opens but reduce_only
//     flow still exits (close-only posture for holders grandfathered
//     below the narrowed scope).
//  2. RETAIL target market — when the Categorizer seam is wired, RETAIL
//     accounts additionally need an APPROVED, non-overdue
//     product_target_markets row whose positive_classes cover the class
//     and whose negative_classes do not; missing row, SUSPENDED status
//     or an elapsed review_due_at all fail closed (the missing/overdue
//     cases still admit reduce_only exits — a client must always be
//     able to flatten, but no new retail open slips through unreviewed
//     governance).
//  3. appropriateness — enforced separately: orders.checkAdmission runs
//     the Task 14.3.7 AppropriatenessGate immediately after this gate,
//     so both the product check and the knowledge check must pass
//     (§14.12). This gate never re-evaluates it.
//
// The nil-Categorizer arm skips step 2: the seam exists so the gate
// operates pre-042 (client categorization not yet migrated) — wiring
// passes the real adapter once available.
package accounts

import (
	"context"
	"strings"
	"time"
)

// Categorizer is the minimal local seam over the Task 14.3.7 client
// categorization contract — the sibling cluster owns the canonical
// implementation (compliance.CategorizationService.Category, adapted in
// the wiring layer); Category returns the §14.2 enum name and a read
// error fails closed (SERVICE_DEGRADED).
type Categorizer interface {
	Category(ctx context.Context, accountID int64) (string, error)
}

// ProductGateService evaluates profile scope + retail target market for
// one admission decision.
type ProductGateService struct {
	profiles *ProfileService
	markets  *TargetMarketService
	cat      Categorizer
	now      func() time.Time
}

// NewProductGateService wires the gate; cat may be nil (pre-042
// deployments — scope enforcement still applies).
func NewProductGateService(profiles *ProfileService, markets *TargetMarketService, cat Categorizer) *ProductGateService {
	return &ProductGateService{profiles: profiles, markets: markets, cat: cat, now: time.Now}
}

// AdmitOrder implements the orders.ProductGate seam. Returns nil when the
// account may open on this instrument class; a coded rejection
// (PRODUCT_NOT_PERMITTED / SERVICE_DEGRADED) otherwise. reduceOnly marks
// closing flow — it bypasses scope/target-market restrictions (close-only
// posture) but never the appropriateness verdict, which is the sibling
// cluster's absolute contract.
func (g *ProductGateService) AdmitOrder(ctx context.Context, accountID int64,
	symbol, instrumentClass string, reduceOnly bool) error {

	class := strings.ToUpper(strings.TrimSpace(instrumentClass))
	prof, err := g.profiles.AccountProfile(ctx, accountID)
	if err != nil {
		return err
	}
	if !prof.InScope(class) {
		if reduceOnly {
			// Narrowed scope below held positions: close-only per the
			// 14.3.13 edge case — exits always pass.
		} else {
			return errorf(CodeProductNotPermitted,
				"%s (%s) is outside product profile %s scope — open rejected",
				symbol, class, prof.Code)
		}
	}
	if g.cat == nil {
		return nil // categorization seam unwired — scope gate stands alone
	}
	// The account join already carried client_category — a second read
	// would return the same row, so only the category-less paths (the
	// STANDARD default or a code/id-resolved profile) consult the
	// categorizer seam.
	cat := prof.accountCategory
	if cat == "" {
		var err error
		cat, err = g.cat.Category(ctx, accountID)
		if err != nil {
			return err // SERVICE_DEGRADED — unresolvable category never admits
		}
	}
	if cat == "RETAIL" {
		if err := g.retailTargetCheck(ctx, prof, class, symbol, reduceOnly); err != nil {
			return err
		}
	}
	// Appropriateness (Task 14.3.7) runs as a distinct checkAdmission
	// seam after this gate — both must pass; nothing to re-check here.
	return nil
}

// retailTargetCheck enforces the profile's RETAIL target-market row.
func (g *ProductGateService) retailTargetCheck(ctx context.Context,
	prof *ProductProfile, class, symbol string, reduceOnly bool) error {

	if g.markets == nil {
		return errorf(CodeServiceDegraded,
			"target-market store not configured — retail admission rejected")
	}
	tm, err := g.markets.ForProfile(ctx, prof.ProfileID, CategoryRetail)
	if err != nil {
		return err
	}
	if tm == nil {
		if reduceOnly {
			return nil // config gap — exits still permitted
		}
		return errorf(CodeProductNotPermitted,
			"no approved retail target market for profile %s — %s open rejected (fail closed)",
			prof.Code, symbol)
	}
	if reduceOnly {
		return nil // close-only: exits always pass the product check
	}
	switch {
	case tm.Status == TargetStatusSuspended:
		return errorf(CodeProductNotPermitted,
			"retail distribution suspended for profile %s — %s open rejected",
			prof.Code, symbol)
	case tm.Overdue(g.now()):
		return errorf(CodeProductNotPermitted,
			"target-market review overdue for profile %s — close-only for RETAIL opens on %s",
			prof.Code, symbol)
	case tm.NegativeClass(class):
		reason := tm.NegativeTarget
		if reason == "" {
			reason = "negative target market"
		}
		return errorf(CodeProductNotPermitted,
			"%s is a negative-target-market class for RETAIL on profile %s (%s)",
			class, prof.Code, reason)
	case !tm.PositiveClass(class):
		return errorf(CodeProductNotPermitted,
			"%s is outside the RETAIL positive target market for profile %s",
			class, prof.Code)
	}
	return nil
}

// CategoryRetail mirrors the §14.2 enum literal — kept as a plain string
// so this package never depends on the sibling cluster's type.
const CategoryRetail = "RETAIL"

// TargetStatus enumerates product_target_markets.status.
const (
	TargetStatusApproved      = "APPROVED"
	TargetStatusReviewOverdue = "REVIEW_OVERDUE"
	TargetStatusSuspended     = "SUSPENDED"
)
