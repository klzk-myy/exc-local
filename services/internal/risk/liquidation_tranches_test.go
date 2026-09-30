// liquidation_tranches_test.go — unit coverage for the §13.4a /
// Task-19.3.16-item-6a ADV slicing rule in closeTranches: the 5%-of-ADV
// trigger, the 10%-of-ADV tranche cap, early halt on margin recovery,
// and the unsliced fallbacks for unprovisioned/missing/erroring ADV.
package risk

import (
	"context"
	"fmt"
	"testing"
	"time"

	"exchange/pkg/decimal"

	"github.com/jackc/pgx/v5"
)

// ---------------------------------------------------------------------------
// Fakes
// ---------------------------------------------------------------------------

// trancheStoreFake satisfies LiquidationStore — closeTranches only
// reaches RecordLiquidationEvent (directClose's audit row); the rest
// are honest no-ops.
type trancheStoreFake struct {
	events []LiquidationEventRow
}

func (f *trancheStoreFake) OpenPositions(context.Context, int64) ([]LiqPosition, error) {
	return nil, nil
}
func (f *trancheStoreFake) OpenInterest(context.Context, int64) (decimal.Decimal, error) {
	return decimal.Zero, nil
}
func (f *trancheStoreFake) MarginMode(context.Context, int64) (string, error) {
	return "CROSS", nil
}
func (f *trancheStoreFake) SetMarginAccountStatus(context.Context, int64, string) error {
	return nil
}
func (f *trancheStoreFake) RecordLiquidationEvent(_ context.Context, ev LiquidationEventRow) (int64, error) {
	f.events = append(f.events, ev)
	return int64(len(f.events)), nil
}
func (f *trancheStoreFake) MarkPositionClosed(context.Context, pgx.Tx, int64,
	decimal.Decimal, decimal.Decimal) error {
	return nil
}
func (f *trancheStoreFake) InsertAuction(context.Context, AuctionRow) (int64, error) {
	return 0, nil
}
func (f *trancheStoreFake) AuctionByID(context.Context, int64) (*AuctionRow, error) {
	return nil, nil
}
func (f *trancheStoreFake) UpdateAuctionPhase(context.Context, int64, string,
	decimal.Decimal, time.Time, time.Time) error {
	return nil
}
func (f *trancheStoreFake) RecordAuctionFill(context.Context, int64,
	decimal.Decimal, decimal.Decimal, decimal.Decimal) error {
	return nil
}
func (f *trancheStoreFake) ActiveAuctions(context.Context) ([]AuctionRow, error) {
	return nil, nil
}
func (f *trancheStoreFake) AccountsForScan(context.Context) ([]int64, error) {
	return nil, nil
}
func (f *trancheStoreFake) IsolatedBreaches(context.Context) ([]LiqPosition, error) {
	return nil, nil
}

var _ LiquidationStore = (*trancheStoreFake)(nil)

// trancheLevelFake is the MarginLevelReader stub — a nil level or a
// level ≤ 100 keeps the ladder going; >100 halts it.
type trancheLevelFake struct {
	lv *MarginLevel
}

func (f *trancheLevelFake) MarginLevel(context.Context, int64) (*MarginLevel, error) {
	return f.lv, nil
}

var _ MarginLevelReader = (*trancheLevelFake)(nil)

// newTrancheSvc builds the service with the seams closeTranches
// consumes — direct construction keeps pool/redis/queue nil (the
// constructor's mandatory deps protect the full worker path, not this
// unit surface).
func newTrancheSvc(adv ADVSource, lv *MarginLevel, disp *fakeADLDispatch,
	store *trancheStoreFake) *LiquidationService {

	return &LiquidationService{
		adv:         adv,
		levels:      &trancheLevelFake{lv: lv},
		dispatch:    disp,
		store:       store,
		sliceDelay:  time.Millisecond,
		slippageBps: 200,
		now:         func() time.Time { return time.Now().UTC() },
		logf:        func(string, ...any) {},
	}
}

// tranchePos is the fixture: 10k EUR/USD long at mark 1.00 → notional
// 10000 USD.
func tranchePos() LiqPosition {
	return LiqPosition{
		ID: 11, AccountID: 7, InstrumentID: 5, Symbol: "EUR/USD",
		Side: "LONG", Quantity: d("10000"), MarkPrice: d("1.0"),
	}
}

// ---------------------------------------------------------------------------
// Tests
// ---------------------------------------------------------------------------

func TestCloseTranchesNilADVUnsliced(t *testing.T) {
	disp := &fakeADLDispatch{accept: true}
	store := &trancheStoreFake{}
	svc := newTrancheSvc(nil, nil, disp, store)

	if err := svc.closeTranches(context.Background(), tranchePos(), "STOP_OUT", nil); err != nil {
		t.Fatalf("closeTranches: %v", err)
	}
	if len(disp.calls) != 1 || !disp.calls[0].Quantity.Equal(d("10000")) {
		t.Fatalf("nil ADV must close whole position in one dispatch: %+v", disp.calls)
	}
	if len(store.events) != 1 {
		t.Fatalf("events = %d, want 1", len(store.events))
	}
}

func TestCloseTranchesBelowTriggerUnsliced(t *testing.T) {
	disp := &fakeADLDispatch{accept: true}
	store := &trancheStoreFake{}
	// ADV 1,000,000 → 5% trigger = 50,000 ≥ notional 10,000 → unsliced.
	svc := newTrancheSvc(staticADV{v: d("1000000")}, nil, disp, store)

	if err := svc.closeTranches(context.Background(), tranchePos(), "STOP_OUT", nil); err != nil {
		t.Fatalf("closeTranches: %v", err)
	}
	if len(disp.calls) != 1 || !disp.calls[0].Quantity.Equal(d("10000")) {
		t.Fatalf("below-trigger must close in one dispatch: %+v", disp.calls)
	}
}

func TestCloseTranchesMissingADVFallsBack(t *testing.T) {
	disp := &fakeADLDispatch{accept: true}
	store := &trancheStoreFake{}
	// Provisioned source, no data — liquidation must never stall on
	// missing analytics (§2.7 pessimism lives in the margin add-on).
	svc := newTrancheSvc(staticADV{v: decimal.Zero}, nil, disp, store)

	if err := svc.closeTranches(context.Background(), tranchePos(), "STOP_OUT", nil); err != nil {
		t.Fatalf("closeTranches: %v", err)
	}
	if len(disp.calls) != 1 || !disp.calls[0].Quantity.Equal(d("10000")) {
		t.Fatalf("missing ADV must close whole position: %+v", disp.calls)
	}
}

func TestCloseTranchesSlicedAtTenPercentADV(t *testing.T) {
	disp := &fakeADLDispatch{accept: true}
	store := &trancheStoreFake{}
	// ADV 30,000 → trigger 1,500 < notional 10,000; slice qty =
	// 10% × 30000 / mark 1.0 = 3000 → tranches 3000, 3000, 3000, 1000.
	svc := newTrancheSvc(staticADV{v: d("30000")},
		&MarginLevel{MarginLevelPct: d("40")}, disp, store)

	if err := svc.closeTranches(context.Background(), tranchePos(), "STOP_OUT", nil); err != nil {
		t.Fatalf("closeTranches: %v", err)
	}
	want := []string{"3000", "3000", "3000", "1000"}
	if len(disp.calls) != len(want) {
		t.Fatalf("tranches = %d, want %d: %+v", len(disp.calls), len(want), disp.calls)
	}
	for i, w := range want {
		if !disp.calls[i].Quantity.Equal(d(w)) {
			t.Fatalf("tranche %d qty = %s, want %s", i, disp.calls[i].Quantity, w)
		}
	}
	if len(store.events) != len(want) {
		t.Fatalf("events = %d, want %d", len(store.events), len(want))
	}
}

func TestCloseTranchesHaltsOnMarginRecovery(t *testing.T) {
	disp := &fakeADLDispatch{accept: true}
	store := &trancheStoreFake{}
	// Level recovered above 100% — the ladder halts after the first
	// tranche even though position remainder exists.
	svc := newTrancheSvc(staticADV{v: d("30000")},
		&MarginLevel{MarginLevelPct: d("150")}, disp, store)

	if err := svc.closeTranches(context.Background(), tranchePos(), "STOP_OUT", nil); err != nil {
		t.Fatalf("closeTranches: %v", err)
	}
	if len(disp.calls) != 1 || !disp.calls[0].Quantity.Equal(d("3000")) {
		t.Fatalf("recovery halt must stop after first tranche: %+v", disp.calls)
	}
}

func TestCloseTranchesADVErrorFallsBack(t *testing.T) {
	disp := &fakeADLDispatch{accept: true}
	store := &trancheStoreFake{}
	svc := newTrancheSvc(staticADV{err: fmt.Errorf("adv feed down")}, nil, disp, store)

	if err := svc.closeTranches(context.Background(), tranchePos(), "STOP_OUT", nil); err != nil {
		t.Fatalf("closeTranches: %v", err)
	}
	if len(disp.calls) != 1 || !disp.calls[0].Quantity.Equal(d("10000")) {
		t.Fatalf("ADV error must close whole position: %+v", disp.calls)
	}
}
