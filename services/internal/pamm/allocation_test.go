package pamm

import (
	"testing"

	"exchange/pkg/decimal"
)

func d(s string) decimal.Decimal { return decimal.RequireFromString(s) }

func shares(ws ...string) []Share {
	out := make([]Share, len(ws))
	for i, w := range ws {
		out[i] = Share{ID: int64(i + 1), Weight: d(w)}
	}
	return out
}

// AC: pro-rata totals are conserved exactly — Σ child quantities == the
// master quantity, to the quantum.
func TestAllocateProRata_ConservesTotal(t *testing.T) {
	cases := []struct {
		total string
		ws    []string
	}{
		{"100", []string{"1", "1", "1"}},
		{"0.00000001", []string{"1", "1", "1"}},
		{"12345.67891234", []string{"7", "3", "11", "2"}},
		{"1", []string{"0.00000001", "999999"}},
	}
	for _, c := range cases {
		out, err := AllocateProRata(d(c.total), shares(c.ws...))
		if err != nil {
			t.Fatalf("total=%s: %v", c.total, err)
		}
		sum := decimal.Zero
		for _, a := range out {
			sum = sum.Add(a.Quantity)
		}
		if !sum.Equal(d(c.total)) {
			t.Fatalf("total=%s: sum %s != %s", c.total, sum, c.total)
		}
	}
}

// AC: deterministic remainder — equal shares, indivisible total: the
// leftover quantum goes to the lowest share ID (ascending-ID tiebreak).
func TestAllocateProRata_RemainderToLowestID(t *testing.T) {
	out, err := AllocateProRata(d("100"), shares("1", "1", "1"))
	if err != nil {
		t.Fatal(err)
	}
	// 100/3 → 33.33333333 floor each; one 1e-8 leftover → share ID 1.
	want := []string{"33.33333334", "33.33333333", "33.33333333"}
	for i, a := range out {
		if !a.Quantity.Equal(d(want[i])) {
			t.Fatalf("share %d: got %s want %s", a.ID, a.Quantity, want[i])
		}
	}
}

// AC: determinism — the same inputs always produce the same outputs
// (map iteration never influences distribution).
func TestAllocateProRata_Deterministic(t *testing.T) {
	ws := shares("10", "20", "30", "40")
	first, err := AllocateProRata(d("777.12345678"), ws)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 64; i++ {
		got, err := AllocateProRata(d("777.12345678"), ws)
		if err != nil {
			t.Fatal(err)
		}
		for j := range got {
			if !got[j].Quantity.Equal(first[j].Quantity) {
				t.Fatalf("iter %d share %d: %s != %s", i, j, got[j].Quantity, first[j].Quantity)
			}
		}
	}
}

// Largest remainder wins: weights 2,1,1 on total 3 → raw 1.5, .75, .75 →
// floors 1,0,0, leftover 2 quanta → both .75-remainder shares get one
// each.
func TestAllocateProRata_LargestRemainder(t *testing.T) {
	out, err := AllocateProRata(d("3"), shares("2", "1", "1"))
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"1.5", "0.75", "0.75"}
	for i, a := range out {
		if !a.Quantity.Equal(d(want[i])) {
			t.Fatalf("share %d: got %s want %s", a.ID, a.Quantity, want[i])
		}
	}
}

// Zero-weight shares receive exactly zero; a zero total is a no-op, never
// an error.
func TestAllocateProRata_ZeroWeightAndZeroTotal(t *testing.T) {
	out, err := AllocateProRata(d("10"), shares("5", "0", "5"))
	if err != nil {
		t.Fatal(err)
	}
	if !out[1].Quantity.IsZero() {
		t.Fatalf("zero-weight share got %s", out[1].Quantity)
	}
	out, err = AllocateProRata(decimal.Zero, shares("5", "5"))
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range out {
		if !a.Quantity.IsZero() {
			t.Fatalf("zero-total share got %s", a.Quantity)
		}
	}
}

// Fail closed: empty share set, negative total, negative weight and an
// all-zero weight set are all coded INVALID_REQUEST — never a silent
// misallocation.
func TestAllocateProRata_FailClosed(t *testing.T) {
	if _, err := AllocateProRata(d("10"), nil); err == nil {
		t.Fatal("empty share set accepted")
	}
	if _, err := AllocateProRata(d("-1"), shares("1")); err == nil {
		t.Fatal("negative total accepted")
	}
	if _, err := AllocateProRata(d("1"), []Share{{ID: 1, Weight: d("-1")}}); err == nil {
		t.Fatal("negative weight accepted")
	}
	if _, err := AllocateProRata(d("1"), shares("0", "0")); err == nil {
		t.Fatal("all-zero weights accepted")
	}
}
