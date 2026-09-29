package copy

import (
	"context"
	"strings"

	"exchange/pkg/decimal"
)

// FollowInput is the POST /copy/follows body.
type FollowInput struct {
	InvestorAccountID  int64
	StrategyID         int64
	AllocationNotional string // decimal text >0, ≤8dp, strategy currency
	SafetyMode         string // FULL | HALF_RISK (default FULL)
	StopLossCap        string // optional decimal — investor-side cap
	DisclosedPositions bool   // investor acknowledged the disclosure below
}

// FollowDisclosure is the mandatory follow-time disclosure (spec §12.9):
// unfollow cancels pending children; already-copied OPEN positions stay
// with the investor and are never auto-closed by the unfollow itself.
const FollowDisclosure = "Unfollowing cancels all pending copied orders. " +
	"Copied positions already open remain yours and are NOT closed " +
	"automatically — manage or close them explicitly."

// Follow binds investor → strategy. Gating: strategy must be LISTED
// (SUSPENDED/INCUBATING never accept new follows), investor account must
// be mutable, allocation currency must equal the strategy denomination
// (conversion at follow time is a deliberate non-feature — the investor
// states their capital in the strategy's own ccy), and the ACTIVE unique
// index enforces one active follow per (investor, strategy).
func (s *Service) Follow(ctx context.Context, in FollowInput) (*Follow, error) {
	if in.InvestorAccountID <= 0 || in.StrategyID <= 0 {
		return nil, errorf(CodeInvalidRequest, "investor_account_id and strategy_id required")
	}
	notional, err := decimal.NewFromString(in.AllocationNotional)
	if err != nil || !notional.IsPositive() {
		return nil, errorf(CodeInvalidRequest,
			"allocation_notional %q must be a positive decimal", in.AllocationNotional)
	}
	if !notional.Round(8).Equal(notional) {
		return nil, errorf(CodeInvalidRequest,
			"allocation_notional %s exceeds the DECIMAL(28,8) quantum", notional)
	}
	mode := SafetyMode(strings.ToUpper(strings.TrimSpace(in.SafetyMode)))
	if mode == "" {
		mode = ModeFull
	}
	if mode != ModeFull && mode != ModeHalfRisk {
		return nil, errorf(CodeInvalidRequest, "safety_mode %q must be FULL or HALF_RISK", in.SafetyMode)
	}
	var slc *decimal.Decimal
	if strings.TrimSpace(in.StopLossCap) != "" {
		d, err := decimal.NewFromString(strings.TrimSpace(in.StopLossCap))
		if err != nil || !d.IsPositive() {
			return nil, errorf(CodeInvalidRequest, "stop_loss_cap %q must be a positive decimal", in.StopLossCap)
		}
		slc = &d
	}

	st, err := s.store.StrategyByID(ctx, in.StrategyID)
	if err != nil {
		return nil, err
	}
	if st == nil {
		return nil, errorf(CodeNotFound, "strategy %d not found", in.StrategyID)
	}
	if st.Status == StatusSuspended {
		return nil, errorf(CodeForbidden,
			"strategy %d is SUSPENDED — no new follows (existing follows continue)", in.StrategyID)
	}
	if st.Status != StatusListed {
		return nil, errorf(CodeForbidden,
			"strategy %d is %s — only LISTED strategies accept follows", in.StrategyID, st.Status)
	}
	if in.InvestorAccountID == st.ManagerAccountID {
		return nil, errorf(CodeInvalidRequest, "a manager cannot follow its own strategy")
	}
	if err := s.checker.AssertMutable(ctx, in.InvestorAccountID); err != nil {
		return nil, err
	}
	f, err := s.store.CreateFollow(ctx, &Follow{
		InvestorAccountID:  in.InvestorAccountID,
		StrategyID:         in.StrategyID,
		AllocationNotional: notional,
		Currency:           st.Currency,
		SafetyMode:         mode,
		StopLossCap:        slc,
	})
	if err != nil {
		return nil, err
	}
	// A follow creates its zero-baseline HWM eagerly — the ratchet read is
	// then always present at first settlement (loss months keep 0 forever
	// until a profit above it pays out).
	if _, err := s.store.HWMForUpdate(ctx, f.FollowID); err != nil {
		return nil, err
	}
	return f, nil
}

// UnfollowResult reports the unfollow outcome — pending children
// cancelled, open copied positions untouched (disclosed).
type UnfollowResult struct {
	Follow            *Follow      `json:"follow"`
	CancelledChildren []ChildOrder `json:"cancelled_children"`
	Disclosure        string       `json:"disclosure"`
}

// Unfollow detaches the investor: status → UNFOLLOWED, every PENDING
// child row is CANCELLED (submitted children are the order pipeline's —
// the engine's cancel seam handles them where bound). Open positions
// stay with the investor; the disclosure is echoed in the response.
func (s *Service) Unfollow(ctx context.Context, followID, actorAccountID int64) (*UnfollowResult, error) {
	if followID <= 0 {
		return nil, errorf(CodeInvalidRequest, "follow_id required")
	}
	f, err := s.store.FollowByID(ctx, followID)
	if err != nil {
		return nil, err
	}
	if f == nil {
		return nil, errorf(CodeNotFound, "follow %d not found", followID)
	}
	if f.Status != FollowActive {
		return nil, errorf(CodeInvalidRequest, "follow %d is %s", followID, f.Status)
	}
	if actorAccountID > 0 && actorAccountID != f.InvestorAccountID {
		// Admin unfollow is a later RBAC seam — for now only the investor
		// may detach (fail closed rather than guess at a role claim).
		return nil, errorf(CodeForbidden, "only the investor may unfollow %d", followID)
	}
	uf, err := s.store.Unfollow(ctx, followID)
	if err != nil {
		return nil, err
	}
	cancelled, err := s.store.CancelPendingChildren(ctx, followID)
	if err != nil {
		return nil, err
	}
	return &UnfollowResult{Follow: uf, CancelledChildren: cancelled,
		Disclosure: FollowDisclosure}, nil
}
