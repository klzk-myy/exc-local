// Task 23.3.5 tests — greeks@ channel: registration, cadence semantics,
// staleness freeze (never fabricate), GK/LATTICE models, snapshot sink.
package marketdata

import (
	"context"
	"strconv"
	"sync"
	"testing"
	"time"

	"exchange/internal/options"
	"exchange/internal/oracle/rates"
	"exchange/pkg/decimal"
)

// completeCurve builds a full non-stale discount curve at rate r.
func completeCurve(ccy string, r float64, asOf time.Time) rates.Curve {
	m := map[rates.Tenor]decimal.Decimal{}
	for _, t := range rates.CanonicalTenors {
		m[t] = decimal.NewFromFloat(r)
	}
	return rates.Curve{
		Currency: ccy, Rates: m, AsOf: asOf,
		SourceFeeds: []string{"ref-a", "ref-b"},
	}
}

type fakeSeries struct {
	list []OptionContract
	err  error
}

func (s fakeSeries) Contracts(_ context.Context, _ string) ([]OptionContract, error) {
	return s.list, s.err
}

type fakeInputs struct {
	in  GreeksInput
	err error
}

func (s fakeInputs) Snapshot(_ context.Context, _ string) (GreeksInput, error) {
	return s.in, s.err
}

type fakeSink struct {
	mu   sync.Mutex
	rows []GreeksSnapshotRow
}

func (s *fakeSink) InsertGreeksSnapshots(_ context.Context, rows []GreeksSnapshotRow) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rows = append(s.rows, rows...)
	return nil
}

var greeksNow = time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)

func greeksContract(style options.ExerciseStyle) OptionContract {
	return OptionContract{
		Symbol: "EUR/USD-OPT-1.10-C-30D", Underlying: testSym,
		Right: options.OptionCall, Style: style,
		Strike: 1.10, Expiry: greeksNow.Add(30 * 24 * time.Hour),
	}
}

func freshInput(now time.Time) GreeksInput {
	return GreeksInput{
		Mark: 1.09, MarkAt: now,
		Base:  completeCurve("EUR", 0.025, now),
		Quote: completeCurve("USD", 0.045, now),
		Vol:   func(strike, tYears float64) (float64, error) { return 0.10, nil },
	}
}

func newTestGreeksFeed(em *emitter, series OptionSeriesSource,
	in GreeksInputSource, sink GreeksSnapshotStore,
	now func() time.Time) *GreeksFeed {
	return NewGreeksFeed(GreeksFeedConfig{
		Symbols:   func() []string { return []string{testSym} },
		Contracts: series,
		Inputs:    in,
		Sink:      sink,
		Now:       now,
	}, em.fn())
}

func TestGreeksFeed_PublishesGKMatrix(t *testing.T) {
	em := &emitter{}
	feed := newTestGreeksFeed(em,
		fakeSeries{list: []OptionContract{greeksContract(options.ExerciseEuropean)}},
		fakeInputs{in: freshInput(greeksNow)}, nil,
		func() time.Time { return greeksNow })

	feed.Tick(context.Background())
	got := forChannel(em.all(), "greeks@"+testSym)
	if len(got) != 1 {
		t.Fatalf("emitted %d frames, want 1", len(got))
	}
	m := payload(t, got[0])
	if m["event"] != "greeks" || m["symbol"] != testSym || m["stale"] != false {
		t.Fatalf("frame envelope wrong: %v", m)
	}
	if m["age_ms"] != float64(0) {
		t.Fatalf("age_ms = %v, want 0", m["age_ms"])
	}
	rows := m["rows"].([]any)
	if len(rows) != 1 {
		t.Fatalf("rows = %d, want 1", len(rows))
	}
	row := rows[0].(map[string]any)
	if row["model"] != "GK" || row["right"] != "CALL" || row["strike"] != "1.1" {
		t.Fatalf("row wrong: %v", row)
	}
	delta, err := strconv.ParseFloat(row["delta"].(string), 64)
	if err != nil || delta <= 0 || delta >= 1 {
		t.Fatalf("call delta %q out of (0,1)", row["delta"])
	}
	for _, k := range []string{"gamma", "vega", "theta", "rho", "rho_foreign"} {
		if _, ok := row[k].(string); !ok {
			t.Fatalf("row missing %s: %v", k, row)
		}
	}
}

func TestGreeksFeed_StaleFreezesNeverFabricates(t *testing.T) {
	em := &emitter{}
	feed := newTestGreeksFeed(em,
		fakeSeries{list: []OptionContract{greeksContract(options.ExerciseEuropean)}},
		fakeInputs{in: freshInput(greeksNow)}, nil,
		func() time.Time { return greeksNow })

	// 1. Fresh tick → computed row.
	feed.Tick(context.Background())
	got := forChannel(em.all(), "greeks@"+testSym)
	if len(got) != 1 || len(payload(t, got[0])["rows"].([]any)) != 1 {
		t.Fatalf("fresh tick emitted %d", len(got))
	}

	// 2. Stale mark (10s > 5s gate) → frozen frame: stale:true, rows are
	//    the LAST computed matrix verbatim — no recomputation.
	staleIn := freshInput(greeksNow)
	staleIn.MarkAt = greeksNow.Add(-10 * time.Second)
	feed.cfg.Inputs = fakeInputs{in: staleIn}
	feed.Tick(context.Background())
	got = forChannel(em.all(), "greeks@"+testSym)
	if len(got) != 2 {
		t.Fatalf("emitted %d, want 2", len(got))
	}
	m := payload(t, got[1])
	if m["stale"] != true {
		t.Fatalf("stale mark published fresh frame: %v", m)
	}
	if m["age_ms"] != float64(10_000) {
		t.Fatalf("age_ms = %v, want 10000", m["age_ms"])
	}
	if n := len(m["rows"].([]any)); n != 1 {
		t.Fatalf("frozen rows = %d, want last matrix (1 row)", n)
	}

	// 3. Stale BEFORE any computation → no fabricated rows at all.
	em2 := &emitter{}
	feed2 := newTestGreeksFeed(em2,
		fakeSeries{list: []OptionContract{greeksContract(options.ExerciseEuropean)}},
		fakeInputs{in: staleIn}, nil,
		func() time.Time { return greeksNow })
	feed2.Tick(context.Background())
	m2 := payload(t, forChannel(em2.all(), "greeks@"+testSym)[0])
	if m2["stale"] != true || len(m2["rows"].([]any)) != 0 {
		t.Fatalf("never-computed feed emitted rows on stale inputs: %v", m2)
	}

	// 4. IV failure freezes too — no partial matrix.
	em3 := &emitter{}
	badVol := freshInput(greeksNow)
	badVol.Vol = func(float64, float64) (float64, error) {
		return 0, options.ErrNonConvergence
	}
	feed3 := newTestGreeksFeed(em3,
		fakeSeries{list: []OptionContract{greeksContract(options.ExerciseEuropean)}},
		fakeInputs{in: badVol}, nil,
		func() time.Time { return greeksNow })
	feed3.Tick(context.Background())
	m3 := payload(t, forChannel(em3.all(), "greeks@"+testSym)[0])
	if m3["stale"] != true {
		t.Fatal("IV failure emitted fresh frame")
	}
}

func TestGreeksFeed_LatticeModelForAmerican(t *testing.T) {
	em := &emitter{}
	feed := newTestGreeksFeed(em,
		fakeSeries{list: []OptionContract{greeksContract(options.ExerciseAmerican)}},
		fakeInputs{in: freshInput(greeksNow)}, nil,
		func() time.Time { return greeksNow })
	// Cheap lattice for the test — the FD machinery is what is under test.
	feed.cfg.Pricer = &OptionsGreeksPricer{Lattice: options.LatticeConfig{
		MinSteps: 8, MaxSteps: 128, Tolerance: 2e-2}}

	feed.Tick(context.Background())
	got := forChannel(em.all(), "greeks@"+testSym)
	if len(got) != 1 {
		t.Fatalf("emitted %d", len(got))
	}
	row := payload(t, got[0])["rows"].([]any)[0].(map[string]any)
	if row["model"] != "LATTICE" {
		t.Fatalf("American row model = %v, want LATTICE", row["model"])
	}
	delta, _ := strconv.ParseFloat(row["delta"].(string), 64)
	if delta <= 0 || delta >= 1 {
		t.Fatalf("lattice call delta %q out of (0,1)", row["delta"])
	}
}

func TestGreeksFeed_SnapshotStoreRowShape(t *testing.T) {
	em := &emitter{}
	sink := &fakeSink{}
	feed := newTestGreeksFeed(em,
		fakeSeries{list: []OptionContract{greeksContract(options.ExerciseEuropean)}},
		fakeInputs{in: freshInput(greeksNow)}, sink,
		func() time.Time { return greeksNow })

	feed.Tick(context.Background())
	sink.mu.Lock()
	defer sink.mu.Unlock()
	if len(sink.rows) != 1 {
		t.Fatalf("sink rows = %d, want 1", len(sink.rows))
	}
	r := sink.rows[0]
	if r.Symbol != "EUR/USD-OPT-1.10-C-30D" || r.Underlying != testSym ||
		r.Right != "CALL" || r.Style != "EUROPEAN" || r.Model != "GK" ||
		r.Strike != 1.10 || !r.Expiry.Equal(greeksNow.Add(30*24*time.Hour)) ||
		!r.Ts.Equal(greeksNow) {
		t.Fatalf("snapshot row shape wrong: %+v", r)
	}
	if r.Delta <= 0 || r.Delta >= 1 || r.Gamma <= 0 || r.Vega <= 0 {
		t.Fatalf("snapshot greeks implausible: %+v", r)
	}

	// Second tick inside HistoryEvery must not duplicate the snapshot.
	feed.Tick(context.Background())
	if len(sink.rows) != 1 {
		t.Fatalf("sink rows = %d after second tick — HistoryEvery broken", len(sink.rows))
	}
}

func TestGreeksFeed_CadenceAndGateDefaults(t *testing.T) {
	feed := NewGreeksFeed(GreeksFeedConfig{}, func(string, uint64, any) {})
	if feed.cfg.Cadence != GreeksFeedCadence {
		t.Fatalf("cadence = %v, want %v", feed.cfg.Cadence, GreeksFeedCadence)
	}
	if feed.cfg.StaleAfter != GreeksStaleGate {
		t.Fatalf("stale gate = %v, want %v", feed.cfg.StaleAfter, GreeksStaleGate)
	}
	// A tighter configured gate clamps UP — the 5s floor is not optional.
	feed2 := NewGreeksFeed(GreeksFeedConfig{StaleAfter: time.Second},
		func(string, uint64, any) {})
	if feed2.cfg.StaleAfter != GreeksStaleGate {
		t.Fatalf("sub-floor gate accepted: %v", feed2.cfg.StaleAfter)
	}
}

func TestGreeksFeed_NoContractsPublishesEmptyMatrix(t *testing.T) {
	em := &emitter{}
	feed := newTestGreeksFeed(em,
		fakeSeries{list: nil},
		fakeInputs{in: freshInput(greeksNow)}, nil,
		func() time.Time { return greeksNow })
	feed.Tick(context.Background())
	m := payload(t, forChannel(em.all(), "greeks@"+testSym)[0])
	if m["stale"] != false || len(m["rows"].([]any)) != 0 {
		t.Fatalf("empty universe emitted wrong frame: %v", m)
	}
}
