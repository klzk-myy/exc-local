// Pure unit tests for the Task 21.3.19 RTS 27/28 artifact builders —
// weighted aggregation, CSV rendering and gap honesty (NULL inputs
// propagate as omitted fields; nothing is fabricated).
package compliance

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func f64(v float64) *float64 { return &v }
func i64(v int64) *int64     { return &v }

// Full-input artifact: VWAP = Σquote/Σbase, fill_rate = filled/submitted,
// aggressor percentages split over total fills and the TCA axes are
// fill-weighted means over the TCA-covered subset.
func TestBuildRTS27Artifact_FullInputs(t *testing.T) {
	qs := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	qe := time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)
	raw, csv := buildRTS27Artifact(qs, qe, "SPOT:FX_MAJOR", 63, 12,
		1000, 2000, 2600.0, f64(1.05), f64(1.42),
		i64(500), i64(400), i64(600), i64(300), i64(100),
		f64(1200), f64(600), i64(800),
		f64(1600.0), f64(2400.0), f64(80.0), true)
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("metrics JSON: %v", err)
	}
	if m["vwap"] != 1.3 {
		t.Fatalf("vwap = %v, want 2600/2000 = 1.3", m["vwap"])
	}
	if m["fill_rate"] != 0.8 {
		t.Fatalf("fill_rate = %v, want 400/500", m["fill_rate"])
	}
	if m["pct_buy_aggressor"] != 0.6 || m["pct_sell_aggressor"] != 0.3 ||
		m["pct_unknown_side"] != 0.1 {
		t.Fatalf("aggressor split wrong: %+v", m)
	}
	// tca-weighted means divide by tca_fills (800).
	if m["slip_arrival_avg_bps"] != 2.0 {
		t.Fatalf("slip_arrival = %v, want 1600/800 = 2", m["slip_arrival_avg_bps"])
	}
	if m["slip_vwap_avg_bps"] != 3.0 || m["price_improvement_avg"] != 0.1 {
		t.Fatalf("vwap/improvement means wrong: %+v", m)
	}
	if m["inputs_complete"] != true {
		t.Fatal("complete input must mark inputs_complete")
	}
	if m["quarter_end"] != "2026-03-31" {
		t.Fatalf("quarter_end = %v", m["quarter_end"])
	}
	if !strings.Contains(csv, "SPOT:FX_MAJOR") ||
		!strings.Contains(csv, "2026-03-31") {
		t.Fatalf("csv artifact missing row content: %q", csv)
	}
	if strings.Count(csv, "\n") != 2 {
		t.Fatalf("csv must be header + one row: %q", csv)
	}
}

// Gap honesty: NULL counters stay NULL — no fill_rate, no slippage and
// no aggressor percentages are invented when the sources have no data.
func TestBuildRTS27Artifact_GapsPropagate(t *testing.T) {
	qs := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	qe := qs.AddDate(0, 3, 0)
	raw, _ := buildRTS27Artifact(qs, qe, "NDF:FX_EXOTIC", 10, 3,
		40, 100, 99.5, nil, nil,
		nil, nil, nil, nil, nil, nil, nil, nil,
		nil, nil, nil, false)
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("metrics JSON: %v", err)
	}
	for _, absent := range []string{
		"price_min", "price_max",
		"orders_submitted", "orders_filled", "fill_rate",
		"pct_aggressive_fills", "pct_buy_aggressor", "pct_sell_aggressor",
		"slip_arrival_avg_bps", "slip_vwap_avg_bps",
		"price_improvement_avg", "tca_fills",
	} {
		if _, ok := m[absent]; ok {
			t.Fatalf("key %s must be omitted on NULL input", absent)
		}
	}
	if m["inputs_complete"] != false {
		t.Fatal("incomplete inputs must record inputs_complete=false")
	}
	// vwap still derives from real volumes — volume presence is not a gap.
	if m["vwap"] != 0.995 {
		t.Fatalf("vwap = %v, want 99.5/100", m["vwap"])
	}
}

// Zero submitted → fill_rate omitted (division guard), but the counts
// themselves are still reported.
func TestBuildRTS27Artifact_ZeroSubmitted(t *testing.T) {
	qs := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	raw, _ := buildRTS27Artifact(qs, qs.AddDate(0, 3, 0), "SPOT:FX_MINOR",
		5, 1, 10, 50, 60, nil, nil,
		i64(0), i64(0), i64(5), i64(5), i64(0),
		f64(25), f64(25), i64(0), nil, nil, nil, true)
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("metrics JSON: %v", err)
	}
	if _, ok := m["fill_rate"]; ok {
		t.Fatal("0 submitted orders must omit fill_rate, not emit 0 or NaN")
	}
	if _, ok := m["pct_buy_aggressor"]; !ok {
		t.Fatal("aggressor split should still report with real fills")
	}
	if _, ok := m["slip_arrival_avg_bps"]; ok {
		t.Fatal("zero TCA fills must omit slippage means")
	}
}

// rts28CSV renders one row per ranked venue per category, ordered by the
// fixed category sequence, with NULL percentages serialized empty.
func TestRTS28CSV(t *testing.T) {
	pv, po := 100.0, 100.0
	cats := map[string]CategoryTable{
		CatRetail: {Category: CatRetail, TotalVolume: 1000, TotalOrders: 50,
			Venues: []VenueRankRow{{
				Rank: 1, Venue: "EXC.LOCAL", PctVolume: &pv,
				PctOrders: &po, VolumeBase: 1000, Orders: 50,
				Counterparties: 3,
			}}},
	}
	csv := rts28CSV(2026, "SPOT:FX_MAJOR", cats)
	lines := strings.Split(strings.TrimSpace(csv), "\n")
	if len(lines) != 2 {
		t.Fatalf("want header + 1 row, got %d lines", len(lines))
	}
	row := lines[1]
	if !strings.Contains(row, "2026,SPOT:FX_MAJOR,RETAIL,1,EXC.LOCAL") {
		t.Fatalf("row prefix wrong: %q", row)
	}
	// Passive/aggressive were nil → empty fields between pct_orders and
	// volume.
	if !strings.Contains(row, "100.000000,100.000000,,,1000.00000000,50,3") {
		t.Fatalf("nil pct must serialize empty: %q", row)
	}
}
