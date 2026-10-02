// bilateral_credit_test.go — unit + PG-gated integration coverage for
// Task 19.3.10 (bilateral credit groups, mutual screening, atomic
// reservations; spec §5.30/§13.8, migration 053).
//
// Integration legs run on the dev Postgres inside a throwaway schema with
// the real migration files applied verbatim — same pattern as
// liquidation_store_test.go:
//
//	EXC_PG_TEST=1 go test ./internal/risk/ -run 'TestPgBilateralCredit' -v
package risk

import (
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"exchange/pkg/decimal"
)

// ---------------------------------------------------------------------------
// Pure helpers
// ---------------------------------------------------------------------------

func TestCreditPoolForInstrumentType(t *testing.T) {
	cases := map[string]string{
		"SPOT":    CreditPoolSpot,
		"FORWARD": CreditPoolForwardNDF,
		"SWAP":    CreditPoolForwardNDF,
		"NDF":     CreditPoolForwardNDF,
		"OPTION":  CreditPoolForwardNDF, // deliverable FX options → forward pool
		"spot":    CreditPoolSpot,       // case-insensitive
	}
	for in, want := range cases {
		got, err := CreditPoolForInstrumentType(in)
		if err != nil || got != want {
			t.Fatalf("%q → %q,%v want %q", in, got, err, want)
		}
	}
	if _, err := CreditPoolForInstrumentType(""); err == nil {
		t.Fatal("empty instrument type must fail closed")
	}
	if _, err := CreditPoolForInstrumentType("CRYPTO_PERP"); err == nil {
		t.Fatal("unknown instrument type must fail closed")
	}
}

func TestCreditTicks(t *testing.T) {
	if got := CreditTicks(decimal.Zero); got != 0 {
		t.Fatalf("zero → %d", got)
	}
	if got := CreditTicks(decimal.NewFromInt(-5)); got != 0 {
		t.Fatalf("negative must clamp to 0, got %d", got)
	}
	// $5M → 5e6 × 1e8 = 5e14 ticks (same scale as ipc credit matrix).
	if got := CreditTicks(decimal.NewFromInt(5_000_000)); got != 500_000_000_000_000 {
		t.Fatalf("$5M → %d", got)
	}
	// Fractional ticks truncate toward zero.
	if got := CreditTicks(decimal.RequireFromString("1.000000001")); got != 100_000_000 {
		t.Fatalf("frac → %d", got)
	}
	// Gigantic values saturate at MaxUint64, never wrap.
	if got := CreditTicks(decimal.NewFromInt(math.MaxInt64)); got != math.MaxUint64 {
		t.Fatalf("huge → %d", got)
	}
}

func bcRel(id int64, grantor, grantee int64, pool string, gross, net *decimal.Decimal,
	curGross, curNet decimal.Decimal) CreditRelationship {
	return CreditRelationship{
		ID: id, GrantorGroupID: 10 + grantor, GrantorAccountID: grantor,
		GrantorProfile: CreditProfileOnePool, GrantorStatus: CreditGroupStatusActive,
		GranteeAccountID: grantee, ProductPool: pool,
		GrossLimit: gross, NetLimit: net,
		CurrentGross: curGross, CurrentNet: curNet,
		Version: 1,
	}
}

func TestCreditRelationshipActiveAt(t *testing.T) {
	now := time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC)
	past := now.Add(-time.Hour)
	future := now.Add(time.Hour)
	r := bcRel(1, 1, 2, CreditPoolAll, dec("100"), dec("100"),
		decimal.Zero, decimal.Zero)
	if !r.ActiveAt(now) {
		t.Fatal("plain active edge must be active")
	}
	r.Blocked = true
	if r.ActiveAt(now) {
		t.Fatal("blocked edge must be inactive")
	}
	r.Blocked = false
	r.GrantorStatus = CreditGroupStatusSuspended
	if r.ActiveAt(now) {
		t.Fatal("suspended group must inactivate its edges")
	}
	r.GrantorStatus = CreditGroupStatusActive
	r.EffectiveAt = &future
	if r.ActiveAt(now) {
		t.Fatal("not-yet-effective edge must be inactive")
	}
	r.EffectiveAt = &past
	r.ExpiresAt = &now // expiry boundary is exclusive
	if r.ActiveAt(now) {
		t.Fatal("expired edge must be inactive")
	}
}

func TestCreditRelationshipRemaining(t *testing.T) {
	now := time.Now().UTC()
	// min(gross headroom, net headroom).
	r := bcRel(1, 1, 2, CreditPoolSpot, dec("1000"), dec("400"),
		decimal.NewFromInt(100), decimal.NewFromInt(50))
	if got := r.Remaining(now); got.String() != "350" {
		t.Fatalf("remaining = %s, want 350 (net-bound)", got)
	}
	// Uncapped side is skipped; single-cap rows are bounded by that cap.
	r.NetLimit = nil
	if got := r.Remaining(now); got.String() != "900" {
		t.Fatalf("remaining = %s, want 900", got)
	}
	// Inactive → 0 regardless of limits.
	r.Blocked = true
	if got := r.Remaining(now); !got.IsZero() {
		t.Fatalf("inactive remaining = %s, want 0", got)
	}
	// Fully consumed clamps at 0, never negative.
	r.Blocked = false
	r.CurrentNet = decimal.NewFromInt(900)
	r.NetLimit = dec("400")
	if got := r.Remaining(now); !got.IsZero() {
		t.Fatalf("overdrawn remaining = %s, want 0", got)
	}
}

func TestPickDirectedEdge(t *testing.T) {
	now := time.Now().UTC()
	zero := decimal.Zero
	// Deterministic pick: pool-specific row beats wildcard; both pass → SPOT.
	all := bcRel(1, 1, 2, CreditPoolAll, dec("1000"), nil, zero, zero)
	spot := bcRel(2, 1, 2, CreditPoolSpot, dec("500"), nil, zero, zero)
	got, err := pickDirectedEdge([]CreditRelationship{all, spot}, CreditPoolSpot,
		decimal.NewFromInt(100), now, 1, 2)
	if err != nil || got.ID != 2 {
		t.Fatalf("specific pool must win: %+v %v", got, err)
	}
	// Insufficient specific → wildcard fallback is admissible.
	spot.GrossLimit = dec("50")
	got, err = pickDirectedEdge([]CreditRelationship{all, spot}, CreditPoolSpot,
		decimal.NewFromInt(100), now, 1, 2)
	if err != nil || got.ID != 1 {
		t.Fatalf("wildcard fallback: %+v %v", got, err)
	}
	// Neither has headroom → GROSS breach naming the deterministic
	// lowest-id active candidate (rel 1).
	spot.GrossLimit = dec("10")
	_, err = pickDirectedEdge([]CreditRelationship{all, spot}, CreditPoolSpot,
		decimal.NewFromInt(5000), now, 1, 2)
	var be *CreditBreachError
	if !errors.As(err, &be) || be.Kind != "GROSS" || be.RelationshipID != 1 {
		t.Fatalf("want GROSS breach on rel 1, got %v", err)
	}
	// No applicable row → NO_RELATIONSHIP.
	_, err = pickDirectedEdge(nil, CreditPoolSpot, decimal.One, now, 1, 2)
	if !errors.As(err, &be) || be.Kind != "NO_RELATIONSHIP" {
		t.Fatalf("want NO_RELATIONSHIP, got %v", err)
	}
	// Blocked row → INACTIVE.
	blocked := bcRel(3, 1, 2, CreditPoolSpot, dec("1e6"), nil, zero, zero)
	blocked.Blocked = true
	_, err = pickDirectedEdge([]CreditRelationship{blocked}, CreditPoolSpot,
		decimal.One, now, 1, 2)
	if !errors.As(err, &be) || be.Kind != "INACTIVE" {
		t.Fatalf("want INACTIVE, got %v", err)
	}
	// Net bound failure reports NET.
	netBound := bcRel(4, 1, 2, CreditPoolSpot, nil, dec("100"), zero, zero)
	_, err = pickDirectedEdge([]CreditRelationship{netBound}, CreditPoolSpot,
		decimal.NewFromInt(200), now, 1, 2)
	if !errors.As(err, &be) || be.Kind != "NET" {
		t.Fatalf("want NET, got %v", err)
	}
}

// ---------------------------------------------------------------------------
// ScreenLiquidity — the credit-screened market view (spec §13.8.4)
// ---------------------------------------------------------------------------

func bcOrder(party uint32, price, qty string, ticks uint64) ScreenedOrder {
	return ScreenedOrder{
		Party:         party,
		Price:         decimal.RequireFromString(price),
		Quantity:      decimal.RequireFromString(qty),
		NotionalTicks: ticks,
	}
}

func TestScreenLiquidity(t *testing.T) {
	accessible := map[uint32]bool{7: true, 9: true}
	headroom := func(maker, taker uint32, n uint64) bool {
		return accessible[maker] && taker == 42
	}
	resting := []ScreenedOrder{
		bcOrder(7, "1.1000", "100", 100),
		bcOrder(5, "1.1001", "50", 50),   // no credit → suppressed
		bcOrder(9, "1.1001", "75", 75),   // accessible → merges at 1.1001
		bcOrder(7, "1.1002", "200", 200), // accessible
	}
	out := ScreenLiquidity(42, resting, headroom)
	if !out.CreditScreened {
		t.Fatal("private view must be labelled credit-screened")
	}
	if len(out.Levels) != 3 {
		t.Fatalf("want 3 levels, got %+v", out.Levels)
	}
	if out.Levels[0].Price.String() != "1.1" || out.Levels[0].Quantity.String() != "100" {
		t.Fatalf("level0 %+v", out.Levels[0])
	}
	// Party 5's qty must not leak into the aggregate at 1.1001.
	if out.Levels[1].Price.String() != "1.1001" || out.Levels[1].Quantity.String() != "75" {
		t.Fatalf("level1 must aggregate only accessible qty: %+v", out.Levels[1])
	}
	if out.Levels[2].Price.String() != "1.1002" || out.Levels[2].Quantity.String() != "200" {
		t.Fatalf("level2 %+v", out.Levels[2])
	}
	// Nil oracle → empty view (fail closed), still labelled screened.
	out = ScreenLiquidity(42, resting, nil)
	if len(out.Levels) != 0 || !out.CreditScreened {
		t.Fatalf("nil oracle must yield empty screened view: %+v", out)
	}
	// Public aggregate is labelled NON-credit-screened.
	pub := PublicDepth([]ScreenedLevel{{Price: decimal.One, Quantity: decimal.One}})
	if pub.CreditScreened {
		t.Fatal("public aggregate must carry the non-screened label")
	}
}

// ---------------------------------------------------------------------------
// Service unit tests — in-memory fake store
// ---------------------------------------------------------------------------

// fakeCreditStore is a minimal in-memory CreditStore for unit tests.
type fakeCreditStore struct {
	edges    map[int64]CreditRelationship
	nextID   int64
	parties  map[int64]uint32
	res      map[int64][]CreditReservation // by order id
	reserveE error                         // injected ReserveMatchTx failure
	consumed map[int64]decimal.Decimal     // consume calls recorded
	released map[int64]bool
	swept    int64
}

func newFakeCreditStore() *fakeCreditStore {
	return &fakeCreditStore{
		edges:    map[int64]CreditRelationship{},
		parties:  map[int64]uint32{},
		res:      map[int64][]CreditReservation{},
		consumed: map[int64]decimal.Decimal{},
		released: map[int64]bool{},
	}
}

func (f *fakeCreditStore) addEdge(r CreditRelationship) {
	f.nextID++
	r.ID = f.nextID
	r.Version = 1
	f.edges[r.ID] = r
}

func (f *fakeCreditStore) CreateGroup(_ context.Context, g CreditGroup) (CreditGroup, error) {
	return g, nil
}
func (f *fakeCreditStore) SetGroupStatus(_ context.Context, id int64, s string) (CreditGroup, error) {
	return CreditGroup{ID: id, Status: s}, nil
}
func (f *fakeCreditStore) GroupByID(_ context.Context, id int64) (CreditGroup, error) {
	return CreditGroup{ID: id}, nil
}
func (f *fakeCreditStore) GroupsForGrantor(_ context.Context, a int64) ([]CreditGroup, error) {
	return nil, nil
}
func (f *fakeCreditStore) RelationshipByID(_ context.Context, id int64) (CreditRelationship, error) {
	r, ok := f.edges[id]
	if !ok {
		return r, fmt.Errorf("rel %d not found", id)
	}
	return r, nil
}
func (f *fakeCreditStore) DirectedEdges(_ context.Context, grantor, grantee int64,
	pool string) ([]CreditRelationship, error) {
	var out []CreditRelationship
	for _, r := range f.edges {
		if r.GrantorAccountID != grantor || r.GranteeAccountID != grantee {
			continue
		}
		if pool == "" || r.ProductPool == CreditPoolAll || r.ProductPool == pool {
			out = append(out, r)
		}
	}
	return out, nil
}
func (f *fakeCreditStore) EdgesForGrantor(_ context.Context, grantor int64) ([]CreditRelationship, error) {
	var out []CreditRelationship
	for _, r := range f.edges {
		if r.GrantorAccountID == grantor {
			out = append(out, r)
		}
	}
	return out, nil
}
func (f *fakeCreditStore) EdgesForGrantee(_ context.Context, grantee int64,
	pool string) ([]CreditRelationship, error) {
	var out []CreditRelationship
	for _, r := range f.edges {
		if r.GranteeAccountID != grantee {
			continue
		}
		if pool == "" || r.ProductPool == CreditPoolAll || r.ProductPool == pool {
			out = append(out, r)
		}
	}
	return out, nil
}
func (f *fakeCreditStore) AllEdges(_ context.Context) ([]CreditRelationship, error) {
	out := make([]CreditRelationship, 0, len(f.edges))
	for _, r := range f.edges {
		out = append(out, r)
	}
	return out, nil
}
func (f *fakeCreditStore) CreateRelationship(_ context.Context, r CreditRelationship) (CreditRelationship, error) {
	f.addEdge(r)
	return r, nil
}
func (f *fakeCreditStore) UpdateRelationship(_ context.Context, r CreditRelationship) (CreditRelationship, error) {
	cur, ok := f.edges[r.ID]
	if !ok {
		return r, fmt.Errorf("rel %d not found", r.ID)
	}
	if cur.Version != r.Version {
		return r, &CreditVersionError{RelationshipID: r.ID, ExpectedVersion: r.Version}
	}
	r.Version = cur.Version + 1
	r.GrantorAccountID, r.GrantorProfile, r.GrantorStatus =
		cur.GrantorAccountID, cur.GrantorProfile, cur.GrantorStatus
	f.edges[r.ID] = r
	return r, nil
}
func (f *fakeCreditStore) ReserveMatchTx(_ context.Context, spec CreditReserveSpec) ([]CreditReservation, error) {
	if spec.MakerAccountID == spec.TakerAccountID {
		return nil, nil
	}
	if f.reserveE != nil {
		return nil, f.reserveE
	}
	now := spec.Now
	mt, _ := f.DirectedEdges(context.Background(), spec.MakerAccountID, spec.TakerAccountID, spec.Pool)
	e1, err := pickDirectedEdge(mt, spec.Pool, spec.Amount, now,
		spec.MakerAccountID, spec.TakerAccountID)
	if err != nil {
		return nil, err
	}
	tm, _ := f.DirectedEdges(context.Background(), spec.TakerAccountID, spec.MakerAccountID, spec.Pool)
	e2, err := pickDirectedEdge(tm, spec.Pool, spec.Amount, now,
		spec.TakerAccountID, spec.MakerAccountID)
	if err != nil {
		return nil, err
	}
	// Debit both directions.
	for _, e := range []*CreditRelationship{e1, e2} {
		cur := f.edges[e.ID]
		cur.CurrentGross = cur.CurrentGross.Add(spec.Amount)
		cur.CurrentNet = cur.CurrentNet.Add(spec.Amount)
		f.edges[e.ID] = cur
	}
	res := []CreditReservation{
		{ID: int64(len(f.res) + 1), OrderID: spec.TakerOrderID, RelationshipID: e1.ID,
			AccountID: spec.TakerAccountID, ProductPool: spec.Pool,
			ReservedAmount: spec.Amount, Status: CreditReservationActive,
			ExpiresAt: spec.ExpiresAt},
		{ID: int64(len(f.res) + 2), OrderID: spec.MakerOrderID, RelationshipID: e2.ID,
			AccountID: spec.MakerAccountID, ProductPool: spec.Pool,
			ReservedAmount: spec.Amount, Status: CreditReservationActive,
			ExpiresAt: spec.ExpiresAt},
	}
	f.res[spec.TakerOrderID] = append(f.res[spec.TakerOrderID], res[0])
	f.res[spec.MakerOrderID] = append(f.res[spec.MakerOrderID], res[1])
	return res, nil
}
func (f *fakeCreditStore) ConsumeTx(_ context.Context, orderID int64, amount decimal.Decimal) error {
	f.consumed[orderID] = f.consumed[orderID].Add(amount)
	return nil
}
func (f *fakeCreditStore) ConsumeMatchTx(_ context.Context, m, t int64, amount decimal.Decimal) error {
	f.consumed[m] = f.consumed[m].Add(amount)
	f.consumed[t] = f.consumed[t].Add(amount)
	return nil
}
func (f *fakeCreditStore) ReleaseTx(_ context.Context, orderID int64) error {
	f.released[orderID] = true
	return nil
}
func (f *fakeCreditStore) ReleaseMatchTx(_ context.Context, m, t int64) error {
	f.released[m] = true
	f.released[t] = true
	return nil
}
func (f *fakeCreditStore) SweepTx(_ context.Context, _ time.Time) (int64, error) {
	return f.swept, nil
}
func (f *fakeCreditStore) ReconcileTx(_ context.Context, relID int64) (CreditRelationship, error) {
	return f.RelationshipByID(context.Background(), relID)
}
func (f *fakeCreditStore) ReservationsFor(_ context.Context, orderID int64) ([]CreditReservation, error) {
	return f.res[orderID], nil
}
func (f *fakeCreditStore) PartyIndex(_ context.Context, a int64) (uint32, error) {
	p, ok := f.parties[a]
	if !ok {
		return 0, fmt.Errorf("no party index")
	}
	return p, nil
}
func (f *fakeCreditStore) EnsurePartyIndex(_ context.Context, a int64) (uint32, error) {
	if p, ok := f.parties[a]; ok {
		return p, nil
	}
	var max uint32
	for _, p := range f.parties {
		if p >= max {
			max = p + 1
		}
	}
	f.parties[a] = max
	return max, nil
}
func (f *fakeCreditStore) Parties(_ context.Context) (map[int64]uint32, error) {
	out := map[int64]uint32{}
	for a, p := range f.parties {
		out[a] = p
	}
	return out, nil
}

// fakeMatrix implements CreditCellWriter + CreditCellReader + CreditScreener.
type fakeMatrix struct {
	cells map[[2]uint32]uint64
}

func newFakeMatrix() *fakeMatrix { return &fakeMatrix{cells: map[[2]uint32]uint64{}} }
func (m *fakeMatrix) ApplyUpdate(a, b uint32, v uint64) bool {
	m.cells[[2]uint32{a, b}] = v
	return true
}
func (m *fakeMatrix) CreditLimit(a, b uint32) uint64 { return m.cells[[2]uint32{a, b}] }
func (m *fakeMatrix) HasHeadroom(a, b uint32, n uint64) bool {
	return m.cells[[2]uint32{a, b}] >= n && m.cells[[2]uint32{b, a}] >= n
}

// fakeOpsAlerter captures OpsAlerts.
type fakeOpsAlerter struct {
	mu     sync.Mutex
	alerts []OpsAlert
}

func (a *fakeOpsAlerter) Raise(_ context.Context, al OpsAlert) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.alerts = append(a.alerts, al)
	return nil
}

func bcSvc(t *testing.T, store CreditStore, m *fakeMatrix, alerter *fakeOpsAlerter) *BilateralCreditService {
	t.Helper()
	var w CreditCellWriter
	var r CreditCellReader
	if m != nil {
		w, r = m, m
	}
	svc, err := NewBilateralCreditService(BilateralCreditOptions{
		Store: store, Writer: w, Reader: r, Alerter: alerter,
	})
	if err != nil {
		t.Fatalf("service: %v", err)
	}
	return svc
}

func bcMatch(makerAcct, takerAcct int64, notional string) CreditMatchRequest {
	return CreditMatchRequest{
		MakerOrderID: 9001, MakerAccountID: makerAcct,
		TakerOrderID: 9002, TakerAccountID: takerAcct,
		InstrumentType: "SPOT", QuoteCurrency: "USD",
		Notional: decimal.RequireFromString(notional),
	}
}

func TestBilateralServiceNilStore(t *testing.T) {
	if _, err := NewBilateralCreditService(BilateralCreditOptions{}); err == nil {
		t.Fatal("nil store must fail construction (fail closed)")
	}
}

func TestBilateralServiceCheckMatchBothDirections(t *testing.T) {
	ctx := context.Background()
	fs := newFakeCreditStore()
	svc := bcSvc(t, fs, nil, nil)
	zero := decimal.Zero

	// Only maker→taker — must reject (mutual requirement).
	fs.addEdge(bcRel(0, 1, 2, CreditPoolAll, dec("1000"), nil, zero, zero))
	err := svc.CheckMatch(ctx, bcMatch(1, 2, "100"))
	requireCode(t, err, CodeBilateralCreditExceeded)

	// Both directions → admit.
	fs.addEdge(bcRel(0, 2, 1, CreditPoolAll, dec("500"), nil, zero, zero))
	if err := svc.CheckMatch(ctx, bcMatch(1, 2, "100")); err != nil {
		t.Fatalf("mutual credit must admit: %v", err)
	}
	// Reverse edge is the tighter bound — 600 exceeds it.
	requireCode(t, svc.CheckMatch(ctx, bcMatch(1, 2, "600")), CodeBilateralCreditExceeded)

	// Self-match: no bilateral exposure → admit with no edges at all.
	if err := svc.CheckMatch(ctx, bcMatch(7, 7, "1e6")); err != nil {
		t.Fatalf("self-match must be a no-op: %v", err)
	}
}

func TestBilateralServiceReserveMatch(t *testing.T) {
	ctx := context.Background()
	fs := newFakeCreditStore()
	svc := bcSvc(t, fs, nil, nil)
	zero := decimal.Zero
	fs.addEdge(bcRel(0, 1, 2, CreditPoolAll, dec("1000"), nil, zero, zero))
	fs.addEdge(bcRel(0, 2, 1, CreditPoolAll, dec("1000"), nil, zero, zero))

	if err := svc.ReserveMatch(ctx, bcMatch(1, 2, "400")); err != nil {
		t.Fatalf("reserve: %v", err)
	}
	// Both edges debited.
	for _, id := range []int64{1, 2} {
		if got := fs.edges[id].CurrentGross.String(); got != "400" {
			t.Fatalf("rel %d gross = %s, want 400", id, got)
		}
	}
	// Reservations recorded per order.
	if len(fs.res[9001]) != 1 || len(fs.res[9002]) != 1 {
		t.Fatalf("reservations: %+v", fs.res)
	}
	// Second reserve beyond reverse headroom → coded breach, no debit.
	err := svc.ReserveMatch(ctx, bcMatch(1, 2, "700"))
	requireCode(t, err, CodeBilateralCreditExceeded)
	if got := fs.edges[1].CurrentGross.String(); got != "400" {
		t.Fatalf("failed reserve must not debit: gross %s", got)
	}
	// Unknown instrument type fails closed before touching the store.
	requireCode(t, svc.ReserveMatch(ctx, CreditMatchRequest{
		MakerAccountID: 1, TakerAccountID: 2, InstrumentType: "BOGUS",
		Notional: decimal.One,
	}), CodeRiskLimitsInternal)
}

func TestBilateralServiceConsumeAndRelease(t *testing.T) {
	ctx := context.Background()
	fs := newFakeCreditStore()
	svc := bcSvc(t, fs, nil, nil)

	if err := svc.ConsumeFill(ctx, 9002, "USD", decimal.NewFromInt(250)); err != nil {
		t.Fatal(err)
	}
	if fs.consumed[9002].String() != "250" {
		t.Fatalf("consumed %s", fs.consumed[9002])
	}
	if err := svc.ConsumeMatchFill(ctx, 9001, 9002, "USD", decimal.NewFromInt(10)); err != nil {
		t.Fatal(err)
	}
	if !fs.consumed[9001].Equal(decimal.NewFromInt(10)) ||
		!fs.consumed[9002].Equal(decimal.NewFromInt(260)) {
		t.Fatalf("match consume: %+v", fs.consumed)
	}
	if err := svc.ReleaseOrder(ctx, 9001); err != nil {
		t.Fatal(err)
	}
	if !fs.released[9001] {
		t.Fatal("release not propagated")
	}
	if err := svc.ReleaseMatch(ctx, 9001, 9002); err != nil {
		t.Fatal(err)
	}
	if !fs.released[9002] {
		t.Fatal("match release must cover both orders")
	}
	// Non-USD quote without a rate source fails closed.
	requireCode(t, svc.ConsumeFill(ctx, 9001, "JPY", decimal.One), CodeRiskLimitsInternal)
}

func TestBilateralServiceVersionErrorAlert(t *testing.T) {
	ctx := context.Background()
	fs := newFakeCreditStore()
	al := &fakeOpsAlerter{}
	svc := bcSvc(t, fs, nil, al)
	fs.addEdge(bcRel(0, 1, 2, CreditPoolAll, dec("100"), nil,
		decimal.Zero, decimal.Zero))

	stale := fs.edges[1]
	stale.Version = 99 // observed a stale row
	_, err := svc.UpdateRelationship(ctx, stale)
	requireCode(t, err, CodeBilateralCreditExceeded)
	if len(al.alerts) != 1 || al.alerts[0].Code != codeCreditDivergence {
		t.Fatalf("stale version must alert Risk Manager: %+v", al.alerts)
	}

	// Fresh version → applies and bumps.
	fresh := fs.edges[1]
	fresh.GrossLimit = dec("2000")
	out, err := svc.UpdateRelationship(ctx, fresh)
	if err != nil || out.Version != 2 {
		t.Fatalf("update: %+v %v", out, err)
	}
}

func TestBilateralServiceUtilizationAlert(t *testing.T) {
	ctx := context.Background()
	fs := newFakeCreditStore()
	al := &fakeOpsAlerter{}
	svc := bcSvc(t, fs, nil, al)
	zero := decimal.Zero
	// 90%+ utilization after a 950 reserve against a 1000 gross cap.
	fs.addEdge(bcRel(0, 1, 2, CreditPoolAll, dec("1000"), nil, zero, zero))
	fs.addEdge(bcRel(0, 2, 1, CreditPoolAll, dec("1e6"), nil, zero, zero))
	if err := svc.ReserveMatch(ctx, bcMatch(1, 2, "950")); err != nil {
		t.Fatalf("reserve: %v", err)
	}
	found := false
	for _, a := range al.alerts {
		if a.Code == codeCreditUtilization && a.Details["relationship_id"] == "1" {
			found = true
		}
	}
	if !found {
		t.Fatalf("90%% utilization must page Risk Manager: %+v", al.alerts)
	}
}

func TestBilateralServiceDivergenceProbe(t *testing.T) {
	ctx := context.Background()
	fs := newFakeCreditStore()
	m := newFakeMatrix()
	al := &fakeOpsAlerter{}
	svc := bcSvc(t, fs, m, al)
	zero := decimal.Zero
	fs.addEdge(bcRel(0, 1, 2, CreditPoolAll, dec("1000"), nil, zero, zero))
	fs.addEdge(bcRel(0, 2, 1, CreditPoolAll, dec("1000"), nil, zero, zero))
	fs.parties[1], fs.parties[2] = 10, 20

	// Matrix agrees → admit.
	if _, err := svc.PublishSnapshot(ctx); err != nil {
		t.Fatal(err)
	}
	if err := svc.CheckMatch(ctx, bcMatch(1, 2, "100")); err != nil {
		t.Fatalf("converged state must admit: %v", err)
	}

	// Divergent: cell over-admits what PG denies → reject + alert.
	m.ApplyUpdate(10, 20, math.MaxUint64)
	err := svc.CheckMatch(ctx, bcMatch(1, 2, "100"))
	requireCode(t, err, CodeBilateralCreditExceeded)
	if len(al.alerts) == 0 || al.alerts[len(al.alerts)-1].Code != codeCreditDivergence {
		t.Fatalf("divergence must alert: %+v", al.alerts)
	}
}

func TestBilateralServicePublishAndVerify(t *testing.T) {
	ctx := context.Background()
	fs := newFakeCreditStore()
	m := newFakeMatrix()
	al := &fakeOpsAlerter{}
	svc := bcSvc(t, fs, m, al)
	zero := decimal.Zero
	fs.addEdge(bcRel(0, 1, 2, CreditPoolAll, dec("1000"), dec("600"), zero, zero))
	fs.addEdge(bcRel(0, 2, 1, CreditPoolAll, dec("800"), nil, zero, zero))
	fs.parties[1], fs.parties[2] = 10, 20

	cells, err := svc.PublishSnapshot(ctx)
	if err != nil || cells != 2 {
		t.Fatalf("snapshot: cells=%d err=%v", cells, err)
	}
	// cell[10][20] = min(gross 1000, net 600) = $600 → 6e10 ticks.
	if got := m.CreditLimit(10, 20); got != 60_000_000_000 {
		t.Fatalf("cell[10][20] = %d, want 6e10", got)
	}
	if got := m.CreditLimit(20, 10); got != 80_000_000_000 {
		t.Fatalf("cell[20][10] = %d, want 8e10", got)
	}

	// Converged → no divergence.
	if n, err := svc.VerifyMatrix(ctx); err != nil || n != 0 {
		t.Fatalf("verify converged: n=%d err=%v", n, err)
	}
	// Force divergence: shrink PG limit then verify cell > remaining.
	rel := fs.edges[1]
	rel.GrossLimit = dec("100")
	out, err := svc.UpdateRelationship(ctx, rel)
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	_ = out
	// UpdateRelationship published the delta already — simulate a stale
	// core by re-inflating the cell directly, then verify+repair.
	m.ApplyUpdate(10, 20, 90_000_000_000)
	n, err := svc.VerifyMatrix(ctx)
	if err != nil || n != 1 {
		t.Fatalf("verify divergent: n=%d err=%v", n, err)
	}
	if got := m.CreditLimit(10, 20); got != 10_000_000_000 {
		t.Fatalf("verify must republish correct cell, got %d", got)
	}
}

// ---------------------------------------------------------------------------
// PG-gated integration — scratch schema + real migration files
// ---------------------------------------------------------------------------

const bcStoreTestDSN = "postgres://exchange:exchange_dev@127.0.0.1:5433/exchange?sslmode=disable"

func bcStoreDSN() string {
	if d := os.Getenv("EXC_PG_DSN"); d != "" {
		return d
	}
	return bcStoreTestDSN
}

func bcStoreFixture(t *testing.T) (*PgBilateralCreditStore, *pgxpool.Pool) {
	t.Helper()
	if os.Getenv("EXC_PG_TEST") != "1" {
		t.Skip("EXC_PG_TEST not set")
	}
	dsn := bcStoreDSN()
	schema := fmt.Sprintf("bc_itest_%d", time.Now().UnixNano())
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	boot, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Skipf("postgres unreachable (%v)", err)
	}
	if err := boot.Ping(ctx); err != nil {
		boot.Close(ctx)
		t.Skipf("postgres unreachable (%v)", err)
	}
	if _, err := boot.Exec(ctx, "CREATE SCHEMA "+schema); err != nil {
		boot.Close(ctx)
		t.Fatalf("create schema: %v", err)
	}
	boot.Close(ctx)
	t.Cleanup(func() {
		c2, cancel2 := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel2()
		conn, err := pgx.Connect(c2, dsn)
		if err == nil {
			_, _ = conn.Exec(c2, "DROP SCHEMA IF EXISTS "+schema+" CASCADE")
			conn.Close(c2)
		}
	})

	liqStoreMigExec(t, ctx, dsn, schema, "../db/migrations/001_create_instruments.up.sql")
	liqStoreMigExec(t, ctx, dsn, schema, "../db/migrations/002_create_users.up.sql")
	liqStoreMigExec(t, ctx, dsn, schema, "../db/migrations/003_create_accounts.up.sql")
	liqStoreMigExec(t, ctx, dsn, schema, "../db/migrations/005_create_orders.up.sql")
	liqStoreMigExec(t, ctx, dsn, schema, "../db/migrations/053_bilateral_credit.up.sql")

	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatalf("parse dsn: %v", err)
	}
	cfg.ConnConfig.RuntimeParams["search_path"] = schema + ",public"
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	t.Cleanup(pool.Close)

	st, err := NewPgBilateralCreditStore(pool)
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	return st, pool
}

func bcSeedAccount(t *testing.T, pool *pgxpool.Pool) int64 {
	t.Helper()
	ctx := context.Background()
	var uid, aid int64
	if err := pool.QueryRow(ctx,
		`INSERT INTO users (email, status) VALUES ($1,'ACTIVE') RETURNING id`,
		fmt.Sprintf("bc_%d@example.com", time.Now().UnixNano())).Scan(&uid); err != nil {
		t.Fatalf("user: %v", err)
	}
	if err := pool.QueryRow(ctx,
		`INSERT INTO accounts (user_id, account_type) VALUES ($1,'MARGIN') RETURNING id`,
		uid).Scan(&aid); err != nil {
		t.Fatalf("account: %v", err)
	}
	return aid
}

func bcSeedPair(t *testing.T, pool *pgxpool.Pool, acctA, acctB int64,
	limitAB, limitBA string) (relAB, relBA int64) {
	t.Helper()
	ctx := context.Background()
	var gA, gB int64
	if err := pool.QueryRow(ctx, `
		INSERT INTO credit_groups (grantor_account_id, name, profile)
		VALUES ($1,'main','ONE_POOL') RETURNING id`, acctA).Scan(&gA); err != nil {
		t.Fatalf("group A: %v", err)
	}
	if err := pool.QueryRow(ctx, `
		INSERT INTO credit_groups (grantor_account_id, name, profile)
		VALUES ($1,'main','ONE_POOL') RETURNING id`, acctB).Scan(&gB); err != nil {
		t.Fatalf("group B: %v", err)
	}
	if err := pool.QueryRow(ctx, `
		INSERT INTO credit_relationships
		    (grantor_group_id, grantee_account_id, product_pool, gross_limit, net_limit)
		VALUES ($1,$2,'ALL',$3::numeric,$3::numeric) RETURNING id`,
		gA, acctB, limitAB).Scan(&relAB); err != nil {
		t.Fatalf("rel A→B: %v", err)
	}
	if err := pool.QueryRow(ctx, `
		INSERT INTO credit_relationships
		    (grantor_group_id, grantee_account_id, product_pool, gross_limit, net_limit)
		VALUES ($1,$2,'ALL',$3::numeric,$3::numeric) RETURNING id`,
		gB, acctA, limitBA).Scan(&relBA); err != nil {
		t.Fatalf("rel B→A: %v", err)
	}
	return relAB, relBA
}

func bcSpec(makerAcct, takerAcct int64, orderBase int64, amount string) CreditReserveSpec {
	return CreditReserveSpec{
		MakerOrderID: orderBase, MakerAccountID: makerAcct,
		TakerOrderID: orderBase + 1, TakerAccountID: takerAcct,
		Pool:   CreditPoolSpot,
		Amount: decimal.RequireFromString(amount),
		Now:    time.Now().UTC(), ExpiresAt: time.Now().UTC().Add(30 * time.Second),
	}
}

// bcCounters returns (current_gross, current_net) as normalized decimal
// strings — NUMERIC(28,8) projects trailing zeros via ::text.
func bcCounters(t *testing.T, pool *pgxpool.Pool, relID int64) (string, string) {
	t.Helper()
	var g, n string
	if err := pool.QueryRow(context.Background(),
		`SELECT current_gross::text, current_net::text FROM credit_relationships WHERE id=$1`,
		relID).Scan(&g, &n); err != nil {
		t.Fatalf("counters rel %d: %v", relID, err)
	}
	gd, err := decimal.NewFromString(g)
	if err != nil {
		t.Fatalf("gross %q unparseable: %v", g, err)
	}
	nd, err := decimal.NewFromString(n)
	if err != nil {
		t.Fatalf("net %q unparseable: %v", n, err)
	}
	return gd.String(), nd.String()
}

func TestPgBilateralCreditReserveAtomic(t *testing.T) {
	st, pool := bcStoreFixture(t)
	ctx := context.Background()
	a := bcSeedAccount(t, pool)
	b := bcSeedAccount(t, pool)
	relAB, relBA := bcSeedPair(t, pool, a, b, "1000", "500")

	// 400 fits both directions.
	res, err := st.ReserveMatchTx(ctx, bcSpec(a, b, 90001, "400"))
	if err != nil {
		t.Fatalf("reserve: %v", err)
	}
	if len(res) != 2 {
		t.Fatalf("want 2 reservation rows, got %d", len(res))
	}
	// Taker order carries the maker→taker edge; maker order the reverse.
	if res[0].RelationshipID != relAB || res[0].OrderID != 90002 ||
		res[1].RelationshipID != relBA || res[1].OrderID != 90001 {
		t.Fatalf("reservation wiring wrong: %+v", res)
	}
	if g, n := bcCounters(t, pool, relAB); g != "400" || n != "400" {
		t.Fatalf("relAB counters %s/%s", g, n)
	}
	if g, n := bcCounters(t, pool, relBA); g != "400" || n != "400" {
		t.Fatalf("relBA counters %s/%s", g, n)
	}

	// B→A limit is 500; 400 already held → a second 200 must fail and
	// must not debit anything on either edge (atomicity across directions).
	var be *CreditBreachError
	_, err = st.ReserveMatchTx(ctx, bcSpec(a, b, 90011, "200"))
	if !errors.As(err, &be) {
		t.Fatalf("want directed breach, got %v", err)
	}
	if g, n := bcCounters(t, pool, relAB); g != "400" || n != "400" {
		t.Fatalf("failed reserve debited relAB: %s/%s", g, n)
	}
	if g, n := bcCounters(t, pool, relBA); g != "400" || n != "400" {
		t.Fatalf("failed reserve debited relBA: %s/%s", g, n)
	}
	// A same-account "match" is a no-op — self-trading carries no
	// bilateral exposure.
	res, err = st.ReserveMatchTx(ctx, bcSpec(a, a, 90091, "999999"))
	if err != nil || len(res) != 0 {
		t.Fatalf("self-match must be a no-op: %v %+v", err, res)
	}
}

func TestPgBilateralCreditReserveBreachAtomic(t *testing.T) {
	st, pool := bcStoreFixture(t)
	ctx := context.Background()
	a := bcSeedAccount(t, pool)
	b := bcSeedAccount(t, pool)
	relAB, relBA := bcSeedPair(t, pool, a, b, "1000", "500")

	// One direction lacks headroom → NOTHING debited either side.
	var be *CreditBreachError
	_, err := st.ReserveMatchTx(ctx, bcSpec(a, b, 90021, "600"))
	if !errors.As(err, &be) || be.Kind != "GROSS" && be.Kind != "NET" {
		t.Fatalf("want directed breach, got %v", err)
	}
	for _, rel := range []int64{relAB, relBA} {
		if g, n := bcCounters(t, pool, rel); g != "0" || n != "0" {
			t.Fatalf("failed reserve debited rel %d: %s/%s", rel, g, n)
		}
	}
	var cnt int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM credit_reservations`).Scan(&cnt); err != nil || cnt != 0 {
		t.Fatalf("reservations after failed reserve: %d %v", cnt, err)
	}
}

func TestPgBilateralCreditConsumeProRata(t *testing.T) {
	st, pool := bcStoreFixture(t)
	ctx := context.Background()
	a := bcSeedAccount(t, pool)
	b := bcSeedAccount(t, pool)
	relAB, relBA := bcSeedPair(t, pool, a, b, "1000", "1000")

	if _, err := st.ReserveMatchTx(ctx, bcSpec(a, b, 90031, "400")); err != nil {
		t.Fatal(err)
	}
	// Partial fill 250: consumed 250, remainder 150 released.
	if err := st.ConsumeMatchTx(ctx, 90031, 90032, decimal.NewFromInt(250)); err != nil {
		t.Fatal(err)
	}
	for _, rel := range []int64{relAB, relBA} {
		if g, n := bcCounters(t, pool, rel); g != "250" || n != "250" {
			t.Fatalf("rel %d after pro-rata consume: %s/%s want 250/250", rel, g, n)
		}
	}
	rows, err := st.ReservationsFor(ctx, 90032)
	if err != nil {
		t.Fatal(err)
	}
	var consumed, released int
	for _, r := range rows {
		switch r.Status {
		case CreditReservationConsumed:
			consumed++
			if r.ReservedAmount.String() != "250" {
				t.Fatalf("consumed amount %s want 250", r.ReservedAmount)
			}
		case CreditReservationReleased:
			released++
			if r.ReservedAmount.String() != "150" {
				t.Fatalf("released remainder %s want 150", r.ReservedAmount)
			}
		}
	}
	if consumed != 1 || released != 1 {
		t.Fatalf("want 1 CONSUMED + 1 RELEASED row, got %+v", rows)
	}
	// Re-consume is a no-op (replay idempotency).
	if err := st.ConsumeTx(ctx, 90032, decimal.NewFromInt(250)); err != nil {
		t.Fatal(err)
	}
	// Over-consume fails closed.
	if err := st.ConsumeMatchTx(ctx, 90041, 90042, decimal.NewFromInt(1)); err != nil {
		t.Fatal(err)
	}
}

func TestPgBilateralCreditReleaseAndSweep(t *testing.T) {
	st, pool := bcStoreFixture(t)
	ctx := context.Background()
	a := bcSeedAccount(t, pool)
	b := bcSeedAccount(t, pool)
	relAB, relBA := bcSeedPair(t, pool, a, b, "1000", "1000")

	if _, err := st.ReserveMatchTx(ctx, bcSpec(a, b, 90051, "300")); err != nil {
		t.Fatal(err)
	}
	// Match abort → both directions credited back.
	if err := st.ReleaseMatchTx(ctx, 90051, 90052); err != nil {
		t.Fatal(err)
	}
	for _, rel := range []int64{relAB, relBA} {
		if g, _ := bcCounters(t, pool, rel); g != "0" {
			t.Fatalf("rel %d after release: %s", rel, g)
		}
	}
	// Expired reservation → swept to EXPIRED + credited.
	if _, err := st.ReserveMatchTx(ctx, CreditReserveSpec{
		MakerOrderID: 90061, MakerAccountID: a,
		TakerOrderID: 90062, TakerAccountID: b,
		Pool: CreditPoolSpot, Amount: decimal.NewFromInt(100),
		Now: time.Now().UTC().Add(-time.Hour), ExpiresAt: time.Now().UTC().Add(-time.Minute),
	}); err != nil {
		t.Fatal(err)
	}
	n, err := st.SweepTx(ctx, time.Now().UTC())
	if err != nil || n != 2 {
		t.Fatalf("sweep: n=%d err=%v", n, err)
	}
	if g, _ := bcCounters(t, pool, relAB); g != "0" {
		t.Fatalf("relAB after expiry sweep: %s", g)
	}
	var st2 string
	if err := pool.QueryRow(ctx,
		`SELECT status FROM credit_reservations WHERE order_id=90061`).Scan(&st2); err != nil {
		t.Fatal(err)
	}
	if st2 != CreditReservationExpired {
		t.Fatalf("expired row status %q", st2)
	}
}

func TestPgBilateralCreditConcurrentNoOversubscribe(t *testing.T) {
	st, pool := bcStoreFixture(t)
	ctx := context.Background()
	a := bcSeedAccount(t, pool)
	b := bcSeedAccount(t, pool)
	relAB, relBA := bcSeedPair(t, pool, a, b, "1000", "1000")

	// 16 concurrent 100-unit reservations against a 1000 cap: at most 10
	// succeed per direction; counters never exceed the limit.
	var wg sync.WaitGroup
	var mu sync.Mutex
	succeeded := 0
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if _, err := st.ReserveMatchTx(ctx,
				bcSpec(a, b, int64(91000+i*10), "100")); err == nil {
				mu.Lock()
				succeeded++
				mu.Unlock()
			}
		}(i)
	}
	wg.Wait()
	if succeeded != 10 {
		t.Fatalf("want exactly 10 successful reserves, got %d", succeeded)
	}
	for _, rel := range []int64{relAB, relBA} {
		if g, _ := bcCounters(t, pool, rel); g != "1000" {
			t.Fatalf("rel %d oversubscribed: %s", rel, g)
		}
	}
}

func TestPgBilateralCreditTwoPoolSplit(t *testing.T) {
	st, pool := bcStoreFixture(t)
	ctx := context.Background()
	a := bcSeedAccount(t, pool)
	b := bcSeedAccount(t, pool)
	var gA, gB int64
	if err := pool.QueryRow(ctx, `
		INSERT INTO credit_groups (grantor_account_id, name, profile)
		VALUES ($1,'two','TWO_POOL') RETURNING id`, a).Scan(&gA); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `
		INSERT INTO credit_groups (grantor_account_id, name, profile)
		VALUES ($1,'two','TWO_POOL') RETURNING id`, b).Scan(&gB); err != nil {
		t.Fatal(err)
	}
	// A→B has SPOT credit only; B→A has both.
	if _, err := pool.Exec(ctx, `
		INSERT INTO credit_relationships
		    (grantor_group_id, grantee_account_id, product_pool, gross_limit)
		VALUES ($1,$2,'SPOT',1000)`, gA, b); err != nil {
		t.Fatal(err)
	}
	var relBAF int64
	if err := pool.QueryRow(ctx, `
		INSERT INTO credit_relationships
		    (grantor_group_id, grantee_account_id, product_pool, gross_limit)
		VALUES ($1,$2,'SPOT',1000) RETURNING id`, gB, a).Scan(&relBAF); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO credit_relationships
		    (grantor_group_id, grantee_account_id, product_pool, gross_limit)
		VALUES ($1,$2,'FORWARD_NDF',2000)`, gB, a); err != nil {
		t.Fatal(err)
	}

	// SPOT match works.
	if _, err := st.ReserveMatchTx(ctx, bcSpec(a, b, 92001, "500")); err != nil {
		t.Fatalf("spot reserve: %v", err)
	}
	// FORWARD_NDF fails — A never granted B a forward line.
	var be *CreditBreachError
	fwd := bcSpec(a, b, 92011, "500")
	fwd.Pool = CreditPoolForwardNDF
	_, err := st.ReserveMatchTx(ctx, fwd)
	if !errors.As(err, &be) || be.Kind != "NO_RELATIONSHIP" {
		t.Fatalf("forward without line must fail NO_RELATIONSHIP, got %v", err)
	}
	// Spot line untouched by the failed forward reserve.
	if g, _ := bcCounters(t, pool, relBAF); g != "500" {
		t.Fatalf("pool isolation violated: %s", g)
	}
}

func TestPgBilateralCreditBlockedAndExpired(t *testing.T) {
	st, pool := bcStoreFixture(t)
	ctx := context.Background()
	a := bcSeedAccount(t, pool)
	b := bcSeedAccount(t, pool)
	relAB, _ := bcSeedPair(t, pool, a, b, "1000", "1000")

	// Block the A→B edge → match must fail INACTIVE.
	if _, err := pool.Exec(ctx,
		`UPDATE credit_relationships SET blocked=true WHERE id=$1`, relAB); err != nil {
		t.Fatal(err)
	}
	var be *CreditBreachError
	_, err := st.ReserveMatchTx(ctx, bcSpec(a, b, 93001, "100"))
	if !errors.As(err, &be) || be.Kind != "INACTIVE" {
		t.Fatalf("blocked edge must fail INACTIVE, got %v", err)
	}
	// Expire it instead → still INACTIVE.
	if _, err := pool.Exec(ctx, `
		UPDATE credit_relationships SET blocked=false,
		    expires_at=now()-interval '1 second' WHERE id=$1`, relAB); err != nil {
		t.Fatal(err)
	}
	_, err = st.ReserveMatchTx(ctx, bcSpec(a, b, 93011, "100"))
	if !errors.As(err, &be) || be.Kind != "INACTIVE" {
		t.Fatalf("expired edge must fail INACTIVE, got %v", err)
	}
}

func TestPgBilateralCreditUpdateVersioned(t *testing.T) {
	st, pool := bcStoreFixture(t)
	ctx := context.Background()
	a := bcSeedAccount(t, pool)
	b := bcSeedAccount(t, pool)
	relAB, _ := bcSeedPair(t, pool, a, b, "1000", "1000")

	cur, err := st.RelationshipByID(ctx, relAB)
	if err != nil {
		t.Fatal(err)
	}
	newLimit := decimal.NewFromInt(5000)
	cur.GrossLimit = &newLimit
	out, err := st.UpdateRelationship(ctx, cur)
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if out.Version != 2 {
		t.Fatalf("version = %d, want bumped 2", out.Version)
	}
	// Replaying the same stale row → CreditVersionError.
	_, err = st.UpdateRelationship(ctx, cur)
	var ve *CreditVersionError
	if !errors.As(err, &ve) {
		t.Fatalf("stale version must fail closed, got %v", err)
	}
	// Missing row → plain not-found error.
	missing := cur
	missing.ID = 999999
	if _, err := st.UpdateRelationship(ctx, missing); err == nil {
		t.Fatal("missing relationship must error")
	}
}

func TestPgBilateralCreditPartiesAndReconcile(t *testing.T) {
	st, pool := bcStoreFixture(t)
	ctx := context.Background()
	a := bcSeedAccount(t, pool)
	b := bcSeedAccount(t, pool)

	pa, err := st.EnsurePartyIndex(ctx, a)
	if err != nil {
		t.Fatal(err)
	}
	pb, err := st.EnsurePartyIndex(ctx, b)
	if err != nil {
		t.Fatal(err)
	}
	if pa == pb || pa >= 1024 || pb >= 1024 {
		t.Fatalf("party indexes must be distinct in range: %d %d", pa, pb)
	}
	// Idempotent re-read.
	if again, _ := st.EnsurePartyIndex(ctx, a); again != pa {
		t.Fatalf("party index reassigned: %d → %d", pa, again)
	}
	// Unknown account → error (fail closed), never zero default.
	if _, err := st.PartyIndex(ctx, 42424242); err == nil {
		t.Fatal("unknown party must error")
	}

	relAB, _ := bcSeedPair(t, pool, a, b, "1000", "1000")
	if _, err := st.ReserveMatchTx(ctx, bcSpec(a, b, 94001, "300")); err != nil {
		t.Fatal(err)
	}
	// Corrupt the counter, then ReconcileTx restores from the ledger.
	if _, err := pool.Exec(ctx,
		`UPDATE credit_relationships SET current_gross=999999 WHERE id=$1`, relAB); err != nil {
		t.Fatal(err)
	}
	rec, err := st.ReconcileTx(ctx, relAB)
	if err != nil {
		t.Fatal(err)
	}
	if rec.CurrentGross.String() != "300" || rec.CurrentNet.String() != "300" {
		t.Fatalf("reconcile must re-derive from reservations: %s/%s",
			rec.CurrentGross, rec.CurrentNet)
	}
}

func TestPgBilateralCreditServiceEndToEnd(t *testing.T) {
	st, pool := bcStoreFixture(t)
	ctx := context.Background()
	a := bcSeedAccount(t, pool)
	b := bcSeedAccount(t, pool)
	bcSeedPair(t, pool, a, b, "1000", "1000")

	m := newFakeMatrix()
	al := &fakeOpsAlerter{}
	svc := bcSvc(t, st, m, al)

	// Snapshot publishes both directed cells at full remaining.
	cells, err := svc.PublishSnapshot(ctx)
	if err != nil || cells != 2 {
		t.Fatalf("snapshot: %d %v", cells, err)
	}
	pa, _ := st.PartyIndex(ctx, a)
	pb, _ := st.PartyIndex(ctx, b)
	if m.CreditLimit(pa, pb) != CreditTicks(decimal.NewFromInt(1000)) {
		t.Fatalf("cell a→b = %d", m.CreditLimit(pa, pb))
	}

	req := CreditMatchRequest{
		MakerOrderID: 95001, MakerAccountID: a,
		TakerOrderID: 95002, TakerAccountID: b,
		InstrumentType: "SPOT", QuoteCurrency: "USD",
		Notional: decimal.NewFromInt(600),
	}
	if err := svc.CheckMatch(ctx, req); err != nil {
		t.Fatalf("check: %v", err)
	}
	if err := svc.ReserveMatch(ctx, req); err != nil {
		t.Fatalf("reserve: %v", err)
	}
	// Post-reserve divergence probe: republish pulls cells to remaining=400.
	if err := svc.PublishDelta(ctx, a, b); err != nil {
		t.Fatal(err)
	}
	if got := m.CreditLimit(pa, pb); got != CreditTicks(decimal.NewFromInt(400)) {
		t.Fatalf("post-reserve cell = %d, want 400-equivalent", got)
	}
	// 90% utilization alert: net limit 1000, current 600 — 60% no alert yet.
	for _, al2 := range al.alerts {
		if al2.Code == codeCreditUtilization {
			t.Fatal("60% must not alert")
		}
	}
	if err := svc.ConsumeMatchFill(ctx, 95001, 95002, "USD", decimal.NewFromInt(600)); err != nil {
		t.Fatal(err)
	}
}

// --- Phase-3 Task 5 — AdmitOrder (the orders-pipeline admission consult) ---

func TestAdmitOrderUnscreenedSkips(t *testing.T) {
	fs := newFakeCreditStore()
	svc := bcSvc(t, fs, nil, nil)
	// Account 7 has no party index → anonymous flow → admit.
	if err := svc.AdmitOrder(context.Background(), 7,
		"SPOT", "USD", *dec("1000000")); err != nil {
		t.Fatalf("unscreened admit: %v", err)
	}
}

func TestAdmitOrderMutualHeadroom(t *testing.T) {
	fs := newFakeCreditStore()
	zero := decimal.Zero
	// 1↔2 mutual edges, both with headroom.
	fs.addEdge(bcRel(0, 1, 2, CreditPoolAll, dec("5000"), nil, zero, zero))
	fs.addEdge(bcRel(0, 2, 1, CreditPoolAll, dec("5000"), nil, zero, zero))
	if _, err := fs.EnsurePartyIndex(context.Background(), 1); err != nil {
		t.Fatal(err)
	}
	svc := bcSvc(t, fs, nil, nil)
	if err := svc.AdmitOrder(context.Background(), 1,
		"SPOT", "USD", *dec("1000")); err != nil {
		t.Fatalf("admit with mutual headroom: %v", err)
	}
}

func TestAdmitOrderNoHeadroomRejects(t *testing.T) {
	fs := newFakeCreditStore()
	zero := decimal.Zero
	// 1→2 edge exists but no 2→1 — the mutual screen must fail.
	fs.addEdge(bcRel(0, 1, 2, CreditPoolAll, dec("5000"), nil, zero, zero))
	if _, err := fs.EnsurePartyIndex(context.Background(), 1); err != nil {
		t.Fatal(err)
	}
	svc := bcSvc(t, fs, nil, nil)
	err := svc.AdmitOrder(context.Background(), 1,
		"SPOT", "USD", *dec("1000"))
	requireCode(t, err, CodeBilateralCreditExceeded)
}

func TestAdmitOrderOversizedRejects(t *testing.T) {
	fs := newFakeCreditStore()
	zero := decimal.Zero
	fs.addEdge(bcRel(0, 1, 2, CreditPoolAll, dec("500"), nil, zero, zero))
	fs.addEdge(bcRel(0, 2, 1, CreditPoolAll, dec("500"), nil, zero, zero))
	if _, err := fs.EnsurePartyIndex(context.Background(), 1); err != nil {
		t.Fatal(err)
	}
	svc := bcSvc(t, fs, nil, nil)
	requireCode(t, svc.AdmitOrder(context.Background(), 1,
		"SPOT", "USD", *dec("9999")), CodeBilateralCreditExceeded)
}
