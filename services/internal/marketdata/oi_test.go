package marketdata

import (
	"context"
	"errors"
	"os"
	"sync"
	"testing"
	"time"

	"exchange/internal/db"
)

// fakeOISource returns queued Aggregate results — each call shifts one
// scripted outcome ({samples} or {err}).
type fakeOISource struct {
	mu      sync.Mutex
	results []struct {
		samples []OISample
		err     error
	}
	calls int
}

func (f *fakeOISource) enqueue(samples []OISample, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.results = append(f.results, struct {
		samples []OISample
		err     error
	}{samples, err})
}

func (f *fakeOISource) Aggregate(context.Context) ([]OISample, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if len(f.results) == 0 {
		return nil, nil
	}
	r := f.results[0]
	f.results = f.results[1:]
	return r.samples, r.err
}

// TestOI_EmitsPositionAggregate — Task 6.3.23 / §24 #357 + spec §10.8:
// openInterest@ carries the position-aggregate fields verbatim.
func TestOI_EmitsPositionAggregate(t *testing.T) {
	src := &fakeOISource{}
	src.enqueue([]OISample{{
		Symbol: testSym, InstrumentID: int64(testInst),
		OpenInterest: mustDec(t, "42000000"),
		Notional:     mustDec(t, "46200000"),
		Positions:    137, AsOf: time.Now(),
	}}, nil)

	em := &emitter{}
	p := NewOIProducer(OIProducerConfig{Tick: 25 * time.Millisecond},
		src, em.fn())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go p.Run(ctx)

	if em.waitFor(1, 2*time.Second) != 1 {
		t.Fatal("no OI frame")
	}
	e := em.all()[0]
	if e.Channel != "openInterest@"+testSym {
		t.Fatalf("channel=%q", e.Channel)
	}
	m := payload(t, e)
	if m["event"] != "openInterest" || m["symbol"] != testSym {
		t.Fatalf("envelope: %v", m)
	}
	if m["open_interest"] != "42000000" ||
		m["open_interest_notional"] != "46200000" {
		t.Fatalf("oi fields: %v", m)
	}
	if m["positions"] != float64(137) {
		t.Fatalf("positions=%v", m["positions"])
	}
	if m["stale"] != false {
		t.Fatalf("fresh sample must not be stale: %v", m)
	}
}

// TestOI_PollFailureMarkedStale — a failed aggregate poll re-emits the
// last known value flagged stale=true; it must never interpolate.
func TestOI_PollFailureMarkedStale(t *testing.T) {
	src := &fakeOISource{}
	src.enqueue([]OISample{{
		Symbol: testSym, OpenInterest: mustDec(t, "100"),
		Notional: mustDec(t, "110"), Positions: 3, AsOf: time.Now(),
	}}, nil)
	src.enqueue(nil, errors.New("db unreachable"))
	src.enqueue(nil, errors.New("db unreachable"))

	em := &emitter{}
	p := NewOIProducer(OIProducerConfig{Tick: 20 * time.Millisecond},
		src, em.fn())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go p.Run(ctx)

	if em.waitFor(3, 2*time.Second) != 3 {
		t.Fatalf("emitted %d", em.waitFor(0, 0))
	}
	all := em.all()
	if payload(t, all[0])["stale"] != false {
		t.Fatal("first frame should be fresh")
	}
	for i := 1; i < 3; i++ {
		m := payload(t, all[i])
		if m["stale"] != true {
			t.Fatalf("frame %d must be stale on poll failure", i)
		}
		if m["open_interest"] != "100" {
			t.Fatalf("frame %d value changed under failure: %v", i, m)
		}
	}
}

// TestOI_ConfirmedZeroIsNotStale — a successful poll that omits a known
// symbol reports OI=0, stale=false (positions genuinely closed — not
// a data outage).
func TestOI_ConfirmedZeroIsNotStale(t *testing.T) {
	src := &fakeOISource{}
	src.enqueue(nil, nil) // empty success
	src.enqueue(nil, nil)

	em := &emitter{}
	p := NewOIProducer(OIProducerConfig{
		Tick: 20 * time.Millisecond, Symbols: []string{testSym},
	}, src, em.fn())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go p.Run(ctx)

	if em.waitFor(1, 2*time.Second) != 1 {
		t.Fatal("no frame")
	}
	m := payload(t, em.all()[0])
	if m["open_interest"] != "0" || m["positions"] != float64(0) {
		t.Fatalf("zero OI wrong: %v", m)
	}
	if m["stale"] != false {
		t.Fatal("confirmed zero must not be stale")
	}
}

// TestOI_SnapshotSource — Snapshot serves the resume/cold-start path
// with the latest known sample and its emitted seq.
func TestOI_SnapshotSource(t *testing.T) {
	src := &fakeOISource{}
	src.enqueue([]OISample{{
		Symbol: testSym, OpenInterest: mustDec(t, "55"),
		Notional: mustDec(t, "60"), Positions: 2, AsOf: time.Now(),
	}}, nil)

	em := &emitter{}
	p := NewOIProducer(OIProducerConfig{Tick: 20 * time.Millisecond},
		src, em.fn())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go p.Run(ctx)

	if em.waitFor(1, 2*time.Second) != 1 {
		t.Fatal("no frame")
	}
	seq, data, err := p.Snapshot(context.Background(), "openInterest@"+testSym)
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if seq != 1 {
		t.Fatalf("snapshot seq=%d", seq)
	}
	raw := data.(openInterestData)
	if raw.OpenInterest != "55" {
		t.Fatalf("snapshot oi=%v", raw.OpenInterest)
	}
	if _, _, err := p.Snapshot(context.Background(), "openInterest@NOPE"); err == nil {
		t.Fatal("unknown symbol must error, not fabricate")
	}
}

// TestOI_History — the 1m bucket ring rolls up into candles; bad
// intervals are rejected.
func TestOI_History(t *testing.T) {
	p := NewOIProducer(OIProducerConfig{}, nil,
		func(string, uint64, any) {})
	now := time.Now()
	p.Push(OISample{Symbol: testSym, OpenInterest: mustDec(t, "10"),
		AsOf: now})
	p.tick(now)
	p.Push(OISample{Symbol: testSym, OpenInterest: mustDec(t, "20"),
		AsOf: now.Add(time.Second)})
	p.tick(now.Add(time.Second))

	candles, err := p.History(testSym, 60, 0)
	if err != nil {
		t.Fatalf("History: %v", err)
	}
	if len(candles) != 1 {
		t.Fatalf("candles=%d", len(candles))
	}
	c := candles[0]
	if c.Open != "10" || c.Close != "20" || c.High != "20" ||
		c.Low != "10" || c.Samples != 2 {
		t.Fatalf("candle wrong: %+v", c)
	}
	if _, err := p.History(testSym, 45, 0); err == nil {
		t.Fatal("non-minute interval must error")
	}
}

// TestOI_PgxSource — real PostgreSQL aggregate query; gated per the
// integration-test convention (EXC_PG_TEST=1 + EXC_PG_DSN).
func TestOI_PgxSource(t *testing.T) {
	if os.Getenv("EXC_PG_TEST") != "1" {
		t.Skip("set EXC_PG_TEST=1 to run the PostgreSQL-backed OI test")
	}
	dsn := os.Getenv("EXC_PG_DSN")
	if dsn == "" {
		t.Skip("EXC_PG_DSN not set")
	}
	pool, err := db.NewPool(context.Background(), dsn, 4)
	if err != nil {
		t.Fatalf("pg pool: %v", err)
	}
	defer pool.Close()

	src := NewPgxOpenInterestSource(pool,
		map[int64]string{int64(testInst): testSym}, nil)
	samples, err := src.Aggregate(context.Background())
	if err != nil {
		t.Fatalf("Aggregate: %v", err)
	}
	for _, s := range samples {
		if s.Symbol == "" || s.Positions < 0 || s.AsOf.IsZero() {
			t.Fatalf("malformed sample: %+v", s)
		}
	}
}
