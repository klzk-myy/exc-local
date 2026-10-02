package decimal

import "testing"

func TestScaledRoundTrip(t *testing.T) {
	// Wire values are int64 counts of 10^-8 units (spec §5.3): a Decimal
	// ↔ int64 round-trip must be exact for on-scale values.
	for _, scaled := range []int64{0, 1, -1, 110_000_000, 123_456_789, -9_876_543_210} {
		got := Scaled(NewFromScaled(scaled))
		if got != scaled {
			t.Fatalf("Scaled(NewFromScaled(%d)) = %d", scaled, got)
		}
	}
}

func TestScaledTruncatesBeyondEightDecimals(t *testing.T) {
	// Truncate toward zero, never round — fractional pipettes drop.
	d := MustFromString("1.234567891") // 9th decimal
	if got := Scaled(d); got != 123_456_789 {
		t.Fatalf("Scaled(1.234567891) = %d, want 123456789", got)
	}
	neg := MustFromString("-1.234567891")
	if got := Scaled(neg); got != -123_456_789 {
		t.Fatalf("Scaled(-1.234567891) = %d, want -123456789 (toward zero)", got)
	}
}

func TestScaleFactorMatchesScale(t *testing.T) {
	if ScaleFactor != 100_000_000 {
		t.Fatalf("ScaleFactor = %d, want 10^8", ScaleFactor)
	}
	one := NewFromScaled(ScaleFactor)
	if !one.Equal(One) {
		t.Fatalf("NewFromScaled(10^8) = %s, want 1", one)
	}
}
