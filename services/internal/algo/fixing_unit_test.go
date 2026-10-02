// fixing_unit_test.go — admission-gate coverage for the §27.1 error
// codes (no DB: the calendar seam drives all cases).
package algo

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"exchange/internal/instruments"
	"exchange/internal/ledger"
	"exchange/internal/orders"
	excerrors "exchange/pkg/errors"
)

type stubPoster struct{}

func (stubPoster) Post(context.Context, ledger.Journal) (ledger.PostResult, error) {
	return ledger.PostResult{}, nil
}
func (stubPoster) PostJournal(context.Context, pgx.Tx, ledger.Journal) (ledger.PostResult, error) {
	return ledger.PostResult{}, nil
}

// emptyCal satisfies CalendarSource with zero rows — every fixing
// benchmark is closed.
type emptyCal struct{}

func (emptyCal) CalendarFor(context.Context, string) ([]instruments.CalendarEntry, error) {
	return nil, nil
}

func fixingSvcForTest(t *testing.T, cal CalendarSource) *FixingService {
	t.Helper()
	pool, err := pgxpool.New(context.Background(),
		"postgres://unused:unused@127.0.0.1:1/x")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	p := stubPoster{}
	svc, err := NewFixingService(FixingDeps{
		Pool: pool, Poster: p, TxPoster: p, Calendar: cal,
	})
	if err != nil {
		t.Fatalf("service: %v", err)
	}
	return svc
}

func codeIs(t *testing.T, err error, want string) {
	t.Helper()
	if err == nil {
		t.Fatalf("want %s, got nil", want)
	}
	e, ok := err.(*excerrors.Error)
	if !ok || e.Code != want {
		t.Fatalf("want %s, got %v", want, err)
	}
}

// Disabled/absent fixing windows on the auction calendar →
// FIXING_WINDOW_CLOSED (400, spec §27.1 MTF/Fair-Value row), not a
// generic invalid request.
func TestFixingWindowClosedWhenNoEnabledWindow(t *testing.T) {
	svc := fixingSvcForTest(t, emptyCal{})
	inst := &orders.Instrument{ID: 7, Symbol: "EURUSD"}
	req := &orders.SubmitRequest{FixingBenchmark: orders.FixingBenchmarkWMR4PM}
	codeIs(t, svc.AdmitSubmit(context.Background(), inst, req,
		time.Now().UTC()), "FIXING_WINDOW_CLOSED")
}

// A disabled calendar row is identical to an absent one.
func TestFixingWindowClosedWhenRowDisabled(t *testing.T) {
	cal := calWith(instruments.CalendarEntry{
		Symbol: "EURUSD", AuctionType: instruments.AuctionFixing,
		Benchmark: instruments.BenchWMLondon, Enabled: false,
	})
	svc := fixingSvcForTest(t, cal)
	inst := &orders.Instrument{ID: 7, Symbol: "EURUSD"}
	req := &orders.SubmitRequest{FixingBenchmark: orders.FixingBenchmarkWMR4PM}
	codeIs(t, svc.AdmitSubmit(context.Background(), inst, req,
		time.Now().UTC()), "FIXING_WINDOW_CLOSED")
}

type fixedCal []instruments.CalendarEntry

func (c fixedCal) CalendarFor(context.Context, string) ([]instruments.CalendarEntry, error) {
	return c, nil
}

func calWith(e instruments.CalendarEntry) fixedCal { return fixedCal{e} }
