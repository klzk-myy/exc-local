// auction_test.go — unit coverage for the §13.4 auction ladder math and
// the merged liquidation-queue producer contract (Task 19.3.3/19.3.26).
package risk

import (
	"context"
	"errors"
	"testing"
	"time"

	"exchange/pkg/decimal"
	excerrors "exchange/pkg/errors"
)

func TestFloorFor(t *testing.T) {
	p := LiqPosition{Side: "LONG", LiquidationPrice: decimal.RequireFromString("100"),
		MarkPrice: decimal.RequireFromString("105")}
	if got := FloorFor(p); got.String() != "98" {
		t.Fatalf("long floor = %s, want 98 (liq×0.98)", got)
	}
	p.Side = "SHORT"
	if got := FloorFor(p); got.String() != "102" {
		t.Fatalf("short cap = %s, want 102 (liq×1.02)", got)
	}
	// Missing liquidation_price falls back to mark.
	p.Side, p.LiquidationPrice = "LONG", decimal.Zero
	if got := FloorFor(p); got.String() != "102.9" {
		t.Fatalf("mark fallback floor = %s, want 102.9 (105×0.98)", got)
	}
	p.MarkPrice = decimal.Zero
	if got := FloorFor(p); got.IsPositive() {
		t.Fatalf("no basis must yield non-positive floor, got %s", got)
	}
}

func TestDecayFloor(t *testing.T) {
	floor := decimal.RequireFromString("100")
	// Sells: each EXTEND step lowers the floor 0.5%.
	if got := decayFloor(floor, "LONG", 2); !got.Equal(decimal.RequireFromString("99.0025")) {
		t.Fatalf("sell decayed floor = %s, want 99.0025 (100×0.995²)", got)
	}
	// Buys: each step raises the cap 0.5%.
	if got := decayFloor(floor, "SHORT", 1); !got.Equal(decimal.RequireFromString("100.5")) {
		t.Fatalf("buy decayed cap = %s, want 100.5", got)
	}
	if got := decayFloor(floor, "LONG", 0); !got.Equal(floor) {
		t.Fatalf("zero steps must return the floor unchanged, got %s", got)
	}
}

func TestForceCashCap(t *testing.T) {
	mark := decimal.RequireFromString("200")
	if got := forceCashCap(mark, "LONG"); got.String() != "190" {
		t.Fatalf("long force-cash = %s, want 190 (mark×0.95)", got)
	}
	if got := forceCashCap(mark, "SHORT"); got.String() != "210" {
		t.Fatalf("short force-cash = %s, want 210 (mark×1.05)", got)
	}
}

func TestDedupKeyScoping(t *testing.T) {
	acct := dedupKeyFor(LiquidationJob{AccountID: 42})
	if acct != LiquidationDedupKey(42) {
		t.Fatalf("account dedup key = %q, want %q", acct, LiquidationDedupKey(42))
	}
	scoped := dedupKeyFor(LiquidationJob{AccountID: 42, PositionID: 7})
	want := LiquidationDedupKey(42) + ":pos:7"
	if scoped != want {
		t.Fatalf("position-scoped key = %q, want %q", scoped, want)
	}
	if scoped == acct {
		t.Fatal("position-scoped key must not equal the account-level key")
	}
}

func TestNewLiquidationQueueNilRedis(t *testing.T) {
	q, err := NewLiquidationQueue(nil, nil)
	if err == nil || q != nil {
		t.Fatal("nil redis must be rejected fail-closed")
	}
}

func TestPenaltyAndDeficiency(t *testing.T) {
	// LONG sold above liquidation_price → fund penalty (surplus).
	pen, def := PenaltyAndDeficiency("LONG", decimal.NewFromInt(10),
		decimal.RequireFromString("102"), decimal.RequireFromString("101"),
		decimal.RequireFromString("100"))
	if !pen.Equal(decimal.NewFromInt(20)) || !def.IsZero() {
		t.Fatalf("pen=%s def=%s, want 20/0", pen, def)
	}
	// LONG sold below liquidation_price → deficiency the fund covers.
	pen, def = PenaltyAndDeficiency("LONG", decimal.NewFromInt(10),
		decimal.RequireFromString("98"), decimal.RequireFromString("99"),
		decimal.RequireFromString("100"))
	if !pen.IsZero() || !def.Equal(decimal.NewFromInt(20)) {
		t.Fatalf("pen=%s def=%s, want 0/20", pen, def)
	}
	// SHORT bought back below liquidation_price → penalty.
	pen, def = PenaltyAndDeficiency("SHORT", decimal.NewFromInt(5),
		decimal.RequireFromString("98"), decimal.RequireFromString("99"),
		decimal.RequireFromString("100"))
	if !pen.Equal(decimal.NewFromInt(10)) || !def.IsZero() {
		t.Fatalf("short pen=%s def=%s, want 10/0", pen, def)
	}
	// No basis at all → zero/zero, never panic.
	pen, def = PenaltyAndDeficiency("LONG", decimal.NewFromInt(1),
		decimal.RequireFromString("1"), decimal.Zero, decimal.Zero)
	if !pen.IsZero() || !def.IsZero() {
		t.Fatalf("zero-basis pen=%s def=%s, want 0/0", pen, def)
	}
}

func TestIsParkable(t *testing.T) {
	halted := excerrors.New("TRADING_HALTED", "trading suspended (account)")
	// Terminal codes park — even through the CodeLiquidationFailed wrap
	// submitLeg/resubmit apply to dispatch failures.
	for _, err := range []error{
		halted,
		excerrors.Wrap(CodeLiquidationFailed, "auction leg dispatch", halted),
		excerrors.Wrap("INTERNAL_ERROR", "outer", excerrors.New("ORDER_NOT_FOUND", "account gone")),
		excerrors.New("ACCOUNT_NOT_FOUND", "account 7 not found"),
	} {
		if !isParkable(err) {
			t.Fatalf("isParkable(%v) = false, want true", err)
		}
	}
	// Liquidity, transient I/O and generic failures stay retryable.
	for _, err := range []error{
		nil,
		excerrors.New("ORDER_REJECTED_NO_LIQUIDITY", "no ref price"),
		excerrors.Wrap(CodeLiquidationFailed, "leg dispatch", errors.New("conn refused")),
		errors.New("plain failure"),
	} {
		if isParkable(err) {
			t.Fatalf("isParkable(%v) = true, want false", err)
		}
	}
}

var _ = context.Background
var _ = time.Now
