// option_margin_test.go — Phase-19 Task 19.3.25 seam coverage.
package risk

import (
	"context"
	"fmt"
	"testing"

	"exchange/pkg/decimal"
)

func TestOptionDeltaAdjustment(t *testing.T) {
	pos := []OptionPosition{
		{AccountID: 1, InstrumentID: 10, Quantity: decimal.NewFromInt(2),
			Delta: decimal.RequireFromString("0.5"), NotionalUSD: decimal.NewFromInt(1000)},
		{AccountID: 1, InstrumentID: 11, Quantity: decimal.NewFromInt(1),
			Delta: decimal.RequireFromString("-0.3"), NotionalUSD: decimal.NewFromInt(2000)},
		{AccountID: 1, InstrumentID: 12, Quantity: decimal.NewFromInt(9),
			Delta: decimal.Zero, NotionalUSD: decimal.NewFromInt(100)}, // skipped
	}
	got, err := OptionDeltaAdjustment(pos)
	if err != nil {
		t.Fatalf("adjustment: %v", err)
	}
	// 2×0.5×1000 = 1000 ; 1×(−0.3)×2000 = −600 → 400
	if !got.Equal(decimal.NewFromInt(400)) {
		t.Fatalf("delta adjustment %s, want 400", got)
	}
}

func TestOptionDeltaAdjustmentFailsClosedOnMissingNotional(t *testing.T) {
	pos := []OptionPosition{{AccountID: 1, InstrumentID: 10,
		Quantity: decimal.NewFromInt(1), Delta: decimal.RequireFromString("0.5")}}
	if _, err := OptionDeltaAdjustment(pos); err == nil {
		t.Fatal("missing notional must error (fail-closed)")
	}
}

type fakeOptionSource struct {
	pos []OptionPosition
	err error
}

func (f fakeOptionSource) OptionPositions(context.Context, int64) ([]OptionPosition, error) {
	return f.pos, f.err
}

func TestDeltaOptionMarginEvaluator(t *testing.T) {
	// Nil source → zero, never panic.
	if got, err := (DeltaOptionMarginEvaluator{}).DeltaEquityAdj(context.Background(), 7); err != nil || !got.IsZero() {
		t.Fatalf("nil-source adj %s err %v, want 0/nil", got, err)
	}
	// Source error propagates fail-closed.
	_, err := (DeltaOptionMarginEvaluator{Source: fakeOptionSource{err: fmt.Errorf("db down")}}).
		DeltaEquityAdj(context.Background(), 7)
	if err == nil {
		t.Fatal("source error must propagate")
	}
	// Null bindings contribute zero.
	if got, _ := (NullOptionDeltaSource{}).OptionPositions(context.Background(), 7); got != nil {
		t.Fatalf("null source returned %d positions", len(got))
	}
	if got, _ := (NullOptionMarginEvaluator{}).DeltaEquityAdj(context.Background(), 7); !got.IsZero() {
		t.Fatalf("null evaluator %s, want 0", got)
	}
	if got, _ := (NullSpreadOffsetSource{}).SpreadOffsets(7); got != nil {
		t.Fatalf("null spread source returned %d offsets", len(got))
	}
}

func TestSpreadOffsetBookSingleUse(t *testing.T) {
	b := NewSpreadOffsetBook()
	o := SpreadOffset{AccountID: 1, LongLegID: 11, ShortLegID: 22,
		OffsetUSD: decimal.NewFromInt(50), Strategy: "VERTICAL"}
	if got := b.Apply(o); got.String() != "50" {
		t.Fatalf("first apply %s, want 50", got)
	}
	// §15.7 double-count guard: second application grants nothing.
	if got := b.Apply(o); !got.IsZero() {
		t.Fatalf("second apply %s, want 0 (single-use)", got)
	}
	// Reversed legs must hit the same key.
	if got := b.Apply(SpreadOffset{AccountID: 1, LongLegID: 22, ShortLegID: 11,
		OffsetUSD: decimal.NewFromInt(50)}); !got.IsZero() {
		t.Fatal("reversed-leg replay must not re-grant")
	}
	// Non-positive offsets never apply.
	if got := b.Apply(SpreadOffset{AccountID: 1, LongLegID: 1, ShortLegID: 2,
		OffsetUSD: decimal.NewFromInt(-5)}); !got.IsZero() {
		t.Fatal("negative offset must be ignored")
	}
	// Fresh evaluation resets the book.
	b.Reset()
	if got := b.Apply(o); got.String() != "50" {
		t.Fatalf("post-reset apply %s, want 50", got)
	}
}

func TestSpreadOffsetTotalOffset(t *testing.T) {
	b := NewSpreadOffsetBook()
	total := b.TotalOffset([]SpreadOffset{
		{AccountID: 1, LongLegID: 1, ShortLegID: 2, OffsetUSD: decimal.NewFromInt(10)},
		{AccountID: 1, LongLegID: 3, ShortLegID: 4, OffsetUSD: decimal.NewFromInt(20)},
		{AccountID: 1, LongLegID: 1, ShortLegID: 2, OffsetUSD: decimal.NewFromInt(10)}, // dup — skipped
	})
	if !total.Equal(decimal.NewFromInt(30)) {
		t.Fatalf("total %s, want 30", total)
	}
}
