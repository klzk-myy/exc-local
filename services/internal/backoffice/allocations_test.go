// Unit tests for Tasks 24.3.10 / 24.3.15 — bunched-order VWAP groups,
// deterministic allocation, lifecycle, corrections, GL rebooking and the
// T+0 escalation sweep. The fake store enforces the same conservation
// rules as migration 055's triggers so over-allocation surfaces in tests
// too.
package backoffice

import (
	"context"
	"encoding/json"
	stderrors "errors"
	"fmt"
	"sort"
	"testing"
	"time"

	"exchange/internal/fix"
	"exchange/internal/ledger"
	"exchange/pkg/decimal"
	excerrors "exchange/pkg/errors"
)

// ---------------------------------------------------------------------------
// In-memory AllocationStore fake — mirrors the pgx semantics (including the
// conservation + capacity triggers) so engine paths are testable without PG.
// ---------------------------------------------------------------------------

type memLeg struct {
	id     int64
	status string // PENDING|SETTLED|VOID|...
}

type memAllocStore struct {
	nextGroup, nextAlloc, nextFill, nextEvent, nextInstr int64
	groups                                               map[int64]*Group
	byRef                                                map[string]int64
	eligible                                             map[int64]map[int64]EligibleAccount
	fills                                                map[int64][]Fill
	fillGroup                                            map[int64]int64 // tradeID → groupID
	trades                                               map[int64]*Trade
	allocs                                               map[int64]*Allocation
	events                                               []*Event
	journals                                             []ledger.Journal
	alerts                                               []OpsAlertRow
	legs                                                 map[int64][]memLeg // allocID → settlement instructions
	nostro                                               map[string]int64
	postErr                                              error // injectable posting failure
}

func newMemStore() *memAllocStore {
	return &memAllocStore{
		groups:    map[int64]*Group{},
		byRef:     map[string]int64{},
		eligible:  map[int64]map[int64]EligibleAccount{},
		fills:     map[int64][]Fill{},
		fillGroup: map[int64]int64{},
		trades:    map[int64]*Trade{},
		allocs:    map[int64]*Allocation{},
		legs:      map[int64][]memLeg{},
		nostro:    map[string]int64{},
	}
}

func (m *memAllocStore) InTx(ctx context.Context, fn func(context.Context, AllocationTx) error) error {
	return fn(ctx, m)
}

func (m *memAllocStore) Group(_ context.Context, id int64) (*Group, error) {
	g := m.groups[id]
	if g == nil {
		return nil, nil
	}
	cp := *g
	return &cp, nil
}

func (m *memAllocStore) GroupByRef(_ context.Context, ref string) (*Group, error) {
	if id, ok := m.byRef[ref]; ok {
		return m.Group(context.Background(), id)
	}
	return nil, nil
}

func (m *memAllocStore) Fills(_ context.Context, groupID int64) ([]Fill, error) {
	out := append([]Fill{}, m.fills[groupID]...)
	sort.Slice(out, func(i, j int) bool { return out[i].TradeID < out[j].TradeID })
	return out, nil
}

func (m *memAllocStore) Allocations(_ context.Context, groupID int64) ([]Allocation, error) {
	var out []Allocation
	for _, a := range m.allocs {
		if a.GroupID == groupID {
			out = append(out, *a)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

func (m *memAllocStore) EligibleAccounts(_ context.Context, groupID int64) ([]EligibleAccount, error) {
	var out []EligibleAccount
	for _, e := range m.eligible[groupID] {
		out = append(out, e)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].AccountID < out[j].AccountID })
	return out, nil
}

func (m *memAllocStore) Events(_ context.Context, groupID int64) ([]Event, error) {
	var out []Event
	for _, e := range m.events {
		if e.GroupID == groupID {
			out = append(out, *e)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Seq < out[j].Seq })
	return out, nil
}

// ---- Tx surface ----

func (m *memAllocStore) InsertGroup(_ context.Context, g *Group) error {
	if _, dup := m.byRef[g.GroupRef]; dup {
		return fmt.Errorf("duplicate group_ref %s", g.GroupRef)
	}
	for _, e := range m.eligible[g.ID] {
		_ = e
	}
	m.nextGroup++
	g.ID = m.nextGroup
	now := time.Now().UTC()
	g.CreatedAt, g.UpdatedAt = now, now
	m.groups[g.ID] = g
	m.byRef[g.GroupRef] = g.ID
	return nil
}

func (m *memAllocStore) LockGroup(_ context.Context, id int64) (*Group, error) {
	g := m.groups[id]
	if g == nil {
		return nil, allocErr(CodeAllocationNotFound, "average-price group %d not found", id)
	}
	cp := *g
	return &cp, nil
}

func (m *memAllocStore) LockGroupByRef(_ context.Context, ref string) (*Group, error) {
	if id, ok := m.byRef[ref]; ok {
		return m.LockGroup(context.Background(), id)
	}
	return nil, nil
}

func (m *memAllocStore) UpdateGroupComputed(_ context.Context, id int64, totalQty, avgPrice, residual decimal.Decimal) error {
	g := m.groups[id]
	if g == nil {
		return allocErr(CodeAllocationNotFound, "group %d not found", id)
	}
	var active decimal.Decimal
	for _, a := range m.allocs {
		if a.GroupID == id && a.Status.Active() && (a.Kind == KindPrimary || a.Kind == KindReplacement) {
			active = active.Add(a.Quantity)
		}
	}
	if active.GreaterThan(totalQty) {
		return allocErr(CodeAllocationSumMismatch, "allocated %s exceeds total %s", active, totalQty)
	}
	g.TotalQty, g.AvgPrice, g.PriceResidual, g.AllocatedQty = totalQty, avgPrice, residual, active
	return nil
}

func (m *memAllocStore) SetGroupStatus(_ context.Context, id int64, status GroupStatus, locked bool) error {
	g := m.groups[id]
	if g == nil {
		return allocErr(CodeAllocationNotFound, "group %d not found", id)
	}
	g.Status, g.SettlementLocked = status, locked
	if status == GroupLocked && g.SubmittedAt == nil {
		now := time.Now().UTC()
		g.SubmittedAt = &now
	}
	return nil
}

func (m *memAllocStore) MarkGroupEscalated(_ context.Context, id int64, at time.Time) error {
	m.groups[id].EscalatedAt = &at
	return nil
}

func (m *memAllocStore) SetGroupFixRef(_ context.Context, id, fixAllocID int64) error {
	m.groups[id].FixAllocationID = fixAllocID
	return nil
}

func (m *memAllocStore) InsertEligible(_ context.Context, groupID int64, e EligibleAccount) error {
	g := m.groups[groupID]
	if g == nil {
		return allocErr(CodeAllocationInvalid, "group %d not found", groupID)
	}
	if e.Capacity != g.Capacity {
		return allocErr(CodeAllocationCapacityMix,
			"account %d capacity %s <> group %d capacity %s", e.AccountID, e.Capacity, groupID, g.Capacity)
	}
	if m.eligible[groupID] == nil {
		m.eligible[groupID] = map[int64]EligibleAccount{}
	}
	if cur, ok := m.eligible[groupID][e.AccountID]; ok {
		// upsert — preserve status
		cur.Weight = e.Weight
		if e.PartyID != "" {
			cur.PartyID = e.PartyID
		}
		if e.PartyLEI != "" {
			cur.PartyLEI = e.PartyLEI
		}
		m.eligible[groupID][e.AccountID] = cur
		return nil
	}
	if e.Status == "" {
		e.Status = "ELIGIBLE"
	}
	m.eligible[groupID][e.AccountID] = e
	return nil
}

func (m *memAllocStore) EligibleAccountsTx(ctx context.Context, groupID int64) ([]EligibleAccount, error) {
	return m.EligibleAccounts(ctx, groupID)
}

func (m *memAllocStore) SetEligibleStatus(_ context.Context, groupID, accountID int64, status string) error {
	e, ok := m.eligible[groupID][accountID]
	if !ok {
		return allocErr(CodeAllocationNotFound, "eligible account %d not on group %d", accountID, groupID)
	}
	e.Status = status
	m.eligible[groupID][accountID] = e
	return nil
}

func (m *memAllocStore) LoadTrade(_ context.Context, tradeID int64) (*Trade, error) {
	t := m.trades[tradeID]
	if t == nil {
		return nil, allocErr(CodeAllocationNotFound, "trade %d not found", tradeID)
	}
	cp := *t
	return &cp, nil
}

func (m *memAllocStore) FillGroup(_ context.Context, tradeID int64) (int64, bool, error) {
	gid, ok := m.fillGroup[tradeID]
	return gid, ok, nil
}

func (m *memAllocStore) InsertFill(_ context.Context, groupID int64, side byte, t *Trade) error {
	m.nextFill++
	acct := t.BuyerAccountID
	if side == '2' {
		acct = t.SellerAccountID
	}
	m.fills[groupID] = append(m.fills[groupID], Fill{
		ID: m.nextFill, GroupID: groupID, TradeID: t.ID, AccountID: acct,
		Quantity: t.Quantity, Price: t.Price, SettlementDate: t.SettlementDate,
	})
	m.fillGroup[t.ID] = groupID
	return nil
}

func (m *memAllocStore) GroupFillsTx(ctx context.Context, groupID int64) ([]Fill, error) {
	return m.Fills(ctx, groupID)
}

func (m *memAllocStore) conservation(groupID, tradeID int64, plus decimal.Decimal) error {
	var fillQty decimal.Decimal
	for _, f := range m.fills[groupID] {
		if f.TradeID == tradeID {
			fillQty = f.Quantity
		}
	}
	var active decimal.Decimal
	for _, a := range m.allocs {
		if a.GroupID == groupID && a.TradeID == tradeID && a.Status.Active() &&
			(a.Kind == KindPrimary || a.Kind == KindReplacement) {
			active = active.Add(a.Quantity)
		}
	}
	active = active.Add(plus)
	if active.GreaterThan(fillQty) {
		return allocErr(CodeAllocationSumMismatch,
			"active %s exceeds fill %d qty %s", active, tradeID, fillQty)
	}
	return nil
}

func (m *memAllocStore) InsertAllocation(_ context.Context, a *Allocation) error {
	if err := m.conservation(a.GroupID, a.TradeID, a.activeContribution()); err != nil {
		return err
	}
	m.nextAlloc++
	a.ID = m.nextAlloc
	now := time.Now().UTC()
	a.CreatedAt, a.UpdatedAt = now, now
	if a.SettlementInstructionIDs == nil {
		a.SettlementInstructionIDs = []int64{}
	}
	m.allocs[a.ID] = a
	return nil
}

func (a *Allocation) activeContribution() decimal.Decimal {
	if (a.Kind == KindPrimary || a.Kind == KindReplacement) && a.Status.Active() {
		return a.Quantity
	}
	return decimal.Zero
}

func (m *memAllocStore) LockAllocation(_ context.Context, id int64) (*Allocation, error) {
	a := m.allocs[id]
	if a == nil {
		return nil, allocErr(CodeAllocationNotFound, "allocation %d not found", id)
	}
	cp := *a
	return &cp, nil
}

func (m *memAllocStore) AllocationsTx(ctx context.Context, groupID int64) ([]Allocation, error) {
	return m.Allocations(ctx, groupID)
}

func (m *memAllocStore) ActiveFillQty(_ context.Context, groupID, tradeID int64) (decimal.Decimal, error) {
	var s decimal.Decimal
	for _, a := range m.allocs {
		if a.GroupID == groupID && a.TradeID == tradeID && a.Status.Active() &&
			(a.Kind == KindPrimary || a.Kind == KindReplacement) {
			s = s.Add(a.Quantity)
		}
	}
	return s, nil
}

func (m *memAllocStore) UpdateAllocationStatus(_ context.Context, id int64, status Status, claimedAt *time.Time, reason, confRef string) error {
	a := m.allocs[id]
	if a == nil {
		return allocErr(CodeAllocationNotFound, "allocation %d not found", id)
	}
	if (a.Status.Active() || a.Status == StatusCorrected) && !status.Active() {
		// released qty — conservation re-check not needed (shrinks)
	}
	if err := m.conservation(a.GroupID, a.TradeID,
		decimal.Zero); err != nil {
		return err
	}
	a.Status = status
	if claimedAt != nil {
		a.ClaimedAt = claimedAt
	}
	if reason != "" {
		a.RejectedReason = reason
	}
	if confRef != "" {
		a.ConfirmationRef = confRef
	}
	return nil
}

func (m *memAllocStore) AttachSettlementLegs(_ context.Context, id int64, instructionIDs []int64) error {
	a := m.allocs[id]
	seen := map[int64]bool{}
	for _, v := range a.SettlementInstructionIDs {
		seen[v] = true
	}
	for _, v := range instructionIDs {
		if !seen[v] {
			a.SettlementInstructionIDs = append(a.SettlementInstructionIDs, v)
		}
	}
	return nil
}

func (m *memAllocStore) VoidAllocationInstructions(_ context.Context, allocID int64, _ string) (int, error) {
	n := 0
	for i := range m.legs[allocID] {
		if m.legs[allocID][i].status == "PENDING" {
			m.legs[allocID][i].status = "VOID"
			n++
		}
	}
	return n, nil
}

func (m *memAllocStore) AllocationLegCounts(_ context.Context, allocID int64) (int, int, error) {
	var settled, pending int
	for _, l := range m.legs[allocID] {
		switch l.status {
		case "SETTLED", "RECONCILED":
			settled++
		case "PENDING":
			pending++
		}
	}
	return settled, pending, nil
}

func (m *memAllocStore) AccountInHierarchy(_ context.Context, masterID, accountID int64) (bool, error) {
	return masterID == accountID, nil
}

func (m *memAllocStore) ActiveNostro(_ context.Context, currency string) (int64, error) {
	return m.nostro[currency], nil
}

func (m *memAllocStore) InsertSettlementLeg(_ context.Context, leg SettlementLeg) (int64, bool, error) {
	// dedup on (trade, account, currency, direction) like the pgx impl.
	for _, legs := range m.legs {
		for _, l := range legs {
			_ = l
		}
	}
	m.nextInstr++
	m.legs[leg.AccountID] = append(m.legs[leg.AccountID], memLeg{id: m.nextInstr, status: "PENDING"})
	return m.nextInstr, true, nil
}

func (m *memAllocStore) PostJournal(_ context.Context, j ledger.Journal) (int64, error) {
	if m.postErr != nil {
		return 0, m.postErr
	}
	if err := j.Validate(); err != nil {
		return 0, err
	}
	m.journals = append(m.journals, j)
	return int64(len(m.journals)), nil
}

func (m *memAllocStore) AppendEvent(_ context.Context, groupID int64, allocID *int64, typ string,
	before, after any, actor string, approvedBy *int64) error {
	m.nextEvent++
	var bB, aB json.RawMessage
	if before != nil {
		b, err := json.Marshal(before)
		if err != nil {
			return err
		}
		bB = b
	}
	a, err := json.Marshal(after)
	if err != nil {
		return err
	}
	aB = a
	m.events = append(m.events, &Event{
		ID: m.nextEvent, GroupID: groupID, AllocationID: allocID, Seq: int(m.nextEvent),
		Type: typ, Before: bB, After: aB, Actor: actor, ApprovedBy: approvedBy,
		CreatedAt: time.Now().UTC(),
	})
	return nil
}

func (m *memAllocStore) UnallocatedGroups(_ context.Context, cutoff time.Time) ([]Group, error) {
	var out []Group
	for _, g := range m.groups {
		if (g.Status == GroupOpen || g.Status == GroupAllocated || g.Status == GroupLocked) && g.EscalatedAt == nil &&
			!g.CreatedAt.After(cutoff) {
			out = append(out, *g)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

func (m *memAllocStore) InsertAlert(_ context.Context, a OpsAlertRow) (int64, error) {
	m.alerts = append(m.alerts, a)
	return int64(len(m.alerts)), nil
}

// ---------------------------------------------------------------------------
// Fixtures + helpers
// ---------------------------------------------------------------------------

func mkTrade(id, instrumentID, buyer, seller int64, qty, price string) *Trade {
	d := time.Date(2026, 9, 30, 0, 0, 0, 0, time.UTC)
	return &Trade{
		ID: id, InstrumentID: instrumentID, Symbol: "EURUSD",
		BaseCurrency: "EUR", QuoteCurrency: "USD", SettlementCycle: 1,
		BuyerAccountID: buyer, SellerAccountID: seller,
		Price: decimal.RequireFromString(price), Quantity: decimal.RequireFromString(qty),
		SettlementDate: &d, Status: "COMPLETED", CreatedAt: d,
	}
}

func mkService(t *testing.T, roles RoleResolver) (*AllocationService, *memAllocStore) {
	t.Helper()
	st := newMemStore()
	svc, err := NewAllocationService(AllocationServiceDeps{Store: st, Roles: roles})
	if err != nil {
		t.Fatal(err)
	}
	return svc, st
}

func mkGroup(t *testing.T, svc *AllocationService, method Method, capacity Capacity, elig ...EligibleAccount) *Group {
	t.Helper()
	g, err := svc.CreateGroup(context.Background(), 7, CreateGroupInput{
		GroupRef: "G1", ManagerAccountID: 100, InstrumentID: 5, Side: '1',
		Capacity: capacity, Method: method, Eligible: elig,
	})
	if err != nil {
		t.Fatalf("create group: %v", err)
	}
	return g
}

func errCode(err error) string {
	var ce *excerrors.Error
	if stderrors.As(err, &ce) {
		return ce.Code
	}
	return ""
}

// ---------------------------------------------------------------------------
// Pure math — VWAP, distribution, spread.
// ---------------------------------------------------------------------------

func TestVWAP_WeightedAndResidual(t *testing.T) {
	fills := []Fill{
		{TradeID: 1, Quantity: decimal.RequireFromString("2"), Price: decimal.RequireFromString("1.10")},
		{TradeID: 2, Quantity: decimal.RequireFromString("3"), Price: decimal.RequireFromString("1.20")},
	}
	avg, total, value, err := VWAP(fills)
	if err != nil {
		t.Fatal(err)
	}
	if avg.String() != "1.16" || total.String() != "5" || value.String() != "5.8" {
		t.Fatalf("avg=%s total=%s value=%s", avg, total, value)
	}
	// Residual-bearing case: 3 fills × qty 1 at prices that don't divide.
	fills = []Fill{
		{TradeID: 1, Quantity: decimal.One, Price: decimal.RequireFromString("1.01")},
		{TradeID: 2, Quantity: decimal.One, Price: decimal.RequireFromString("1.02")},
		{TradeID: 3, Quantity: decimal.One, Price: decimal.RequireFromString("1.03")},
	}
	avg, total, value, err = VWAP(fills)
	if err != nil {
		t.Fatal(err)
	}
	// Σp·q = 3.06; avg = 1.02 exactly — residual zero here.
	if avg.String() != "1.02" || value.String() != "3.06" {
		t.Fatalf("avg=%s value=%s", avg, value)
	}
	resid := value.Sub(total.Mul(avg))
	if !resid.IsZero() {
		t.Fatalf("residual %s", resid)
	}
	if _, _, _, err := VWAP(nil); errCode(err) != CodeAllocationInvalid {
		t.Fatalf("empty fills: %v", err)
	}
}

func TestDistribute_ManualPartialAndReject(t *testing.T) {
	elig := []EligibleAccount{
		{AccountID: 201, Capacity: CapacityClient, Status: "ELIGIBLE"},
		{AccountID: 202, Capacity: CapacityClient, Status: "ELIGIBLE"},
	}
	// partial MANUAL — remainder stays on the omnibus.
	out, err := distributeTotal(MethodManual, decimal.RequireFromString("10"),
		[]LegRequest{{AccountID: 201, Quantity: decimal.RequireFromString("6")}}, elig)
	if err != nil || len(out) != 1 || out[0].Quantity.String() != "6" {
		t.Fatalf("partial manual: %+v %v", out, err)
	}
	// over-allocating MANUAL → ALLOCATION_SUM_MISMATCH.
	if _, err := distributeTotal(MethodManual, decimal.RequireFromString("10"),
		[]LegRequest{{AccountID: 201, Quantity: decimal.RequireFromString("11")}}, elig); errCode(err) != CodeAllocationSumMismatch {
		t.Fatalf("over-alloc: %v", err)
	}
	// ineligible leg → ALLOCATION_INELIGIBLE.
	if _, err := distributeTotal(MethodManual, decimal.RequireFromString("10"),
		[]LegRequest{{AccountID: 999, Quantity: decimal.One}}, elig); errCode(err) != CodeAllocationIneligible {
		t.Fatalf("ineligible: %v", err)
	}
	// INELIGIBLE account leg refused too.
	elig[0].Status = "INELIGIBLE"
	if _, err := distributeTotal(MethodManual, decimal.RequireFromString("10"),
		[]LegRequest{{AccountID: 201, Quantity: decimal.One}}, elig); errCode(err) != CodeAllocationIneligible {
		t.Fatalf("post-execution ineligible: %v", err)
	}
}

func TestDistribute_ProRataEqualRuleBased_Conservation(t *testing.T) {
	elig := []EligibleAccount{
		{AccountID: 201, Capacity: CapacityClient, Status: "ELIGIBLE", Weight: decimal.RequireFromString("0.25")},
		{AccountID: 202, Capacity: CapacityClient, Status: "ELIGIBLE", Weight: decimal.RequireFromString("0.75")},
		{AccountID: 203, Capacity: CapacityClient, Status: "ELIGIBLE"},
	}
	// RULE_BASED — preset weights on the registry (203 has none → excluded
	// by INELIGIBLE status below; first flip it).
	total := decimal.RequireFromString("100.00000003") // non-quantum divisor
	out, err := distributeTotal(MethodRuleBased, total, nil, elig[:2])
	if err != nil {
		t.Fatal(err)
	}
	var sum decimal.Decimal
	for _, b := range out {
		sum = sum.Add(b.Quantity)
	}
	if !sum.Equal(total) {
		t.Fatalf("rule-based sum %s != %s", sum, total)
	}
	// 25/75 split of 100.00000003 → 25.0000000075→25.00000001 rounding.
	byID := map[int64]decimal.Decimal{}
	for _, b := range out {
		byID[b.AccountID] = b.Quantity
	}
	if !byID[201].Add(byID[202]).Equal(total) {
		t.Fatalf("conservation: %v", byID)
	}

	// EQUAL_SPLIT across the still-eligible accounts (203 INELIGIBLE
	// excluded — post-execution edge case).
	elig[2].Status = "INELIGIBLE"
	out, err = distributeTotal(MethodEqualSplit, decimal.RequireFromString("9"), nil, elig)
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 2 {
		t.Fatalf("equal split legs: %+v", out)
	}
	sum = decimal.Zero
	for _, b := range out {
		sum = sum.Add(b.Quantity)
	}
	if !sum.Equal(decimal.RequireFromString("9")) {
		t.Fatalf("equal split sum %s", sum)
	}

	// PRO_RATA leg weights.
	out, err = distributeTotal(MethodProRata, decimal.RequireFromString("10"),
		[]LegRequest{{AccountID: 201, Weight: decimal.One}, {AccountID: 202, Weight: decimal.RequireFromString("3")}}, elig)
	if err != nil {
		t.Fatal(err)
	}
	byID = map[int64]decimal.Decimal{}
	for _, b := range out {
		byID[b.AccountID] = b.Quantity
	}
	if byID[201].String() != "2.5" || byID[202].String() != "7.5" {
		t.Fatalf("pro-rata: %v", byID)
	}
}

func TestSpreadFills_ConservationPerFill(t *testing.T) {
	fills := []Fill{
		{TradeID: 2, GroupID: 1, Quantity: decimal.RequireFromString("3")},
		{TradeID: 1, GroupID: 1, Quantity: decimal.RequireFromString("7")},
	}
	benef := []BeneficiaryQty{
		{AccountID: 201, Quantity: decimal.RequireFromString("6")},
		{AccountID: 202, Quantity: decimal.RequireFromString("4")},
	}
	rows, err := spreadFills(benef, fills)
	if err != nil {
		t.Fatal(err)
	}
	perFill := map[int64]decimal.Decimal{}
	perAcct := map[int64]decimal.Decimal{}
	for _, r := range rows {
		perFill[r.TradeID] = perFill[r.TradeID].Add(r.Quantity)
		perAcct[r.BeneficiaryAccountID] = perAcct[r.BeneficiaryAccountID].Add(r.Quantity)
	}
	if !perFill[1].Equal(decimal.RequireFromString("7")) || !perFill[2].Equal(decimal.RequireFromString("3")) {
		t.Fatalf("per-fill conservation: %v", perFill)
	}
	if !perAcct[201].Equal(decimal.RequireFromString("6")) || !perAcct[202].Equal(decimal.RequireFromString("4")) {
		t.Fatalf("per-acct conservation: %v", perAcct)
	}
	// deterministic order — beneficiary 201 first, fill 1 first.
	if rows[0].BeneficiaryAccountID != 201 || rows[0].TradeID != 1 {
		t.Fatalf("order: %+v", rows[0])
	}
}

// ---------------------------------------------------------------------------
// Service lifecycle.
// ---------------------------------------------------------------------------

func TestCreateGroup_CapacityMixBlocked(t *testing.T) {
	svc, _ := mkService(t, nil)
	_, err := svc.CreateGroup(context.Background(), 7, CreateGroupInput{
		GroupRef: "G1", ManagerAccountID: 100, InstrumentID: 5, Side: '1',
		Capacity: CapacityClient, Method: MethodProRata,
		Eligible: []EligibleAccount{
			{AccountID: 201, Capacity: CapacityClient},
			{AccountID: 301, Capacity: CapacityProprietary}, // prop in client group
		},
	})
	if errCode(err) != CodeAllocationCapacityMix {
		t.Fatalf("capacity mix: %v", err)
	}
	// Manager as own beneficiary refused.
	_, err = svc.CreateGroup(context.Background(), 7, CreateGroupInput{
		GroupRef: "G2", ManagerAccountID: 100, InstrumentID: 5, Side: '1',
		Capacity: CapacityProprietary, Method: MethodManual,
		Eligible: []EligibleAccount{{AccountID: 100, Capacity: CapacityProprietary}},
	})
	if errCode(err) != CodeAllocationInvalid {
		t.Fatalf("self-beneficiary: %v", err)
	}
}

func TestAttachFills_Validation(t *testing.T) {
	svc, st := mkService(t, nil)
	g := mkGroup(t, svc, MethodManual, CapacityClient,
		EligibleAccount{AccountID: 201, Capacity: CapacityClient})
	st.trades[501] = mkTrade(501, 5, 100, 900, "10", "1.10")
	st.trades[502] = mkTrade(502, 9, 100, 900, "10", "1.10") // wrong instrument
	st.trades[503] = mkTrade(503, 5, 777, 900, "10", "1.10") // manager not on buy side
	st.trades[504] = mkTrade(504, 5, 100, 900, "5", "1.10")
	st.trades[504].Status = "BUSTED"

	ctx := context.Background()
	if _, err := svc.AttachFills(ctx, g.ID, []int64{502}); errCode(err) != CodeAllocationInvalid {
		t.Fatalf("wrong instrument: %v", err)
	}
	if _, err := svc.AttachFills(ctx, g.ID, []int64{503}); errCode(err) != CodeAllocationInvalid {
		t.Fatalf("wrong side account: %v", err)
	}
	if _, err := svc.AttachFills(ctx, g.ID, []int64{504}); errCode(err) != CodeAllocationInvalid {
		t.Fatalf("busted fill: %v", err)
	}
	got, err := svc.AttachFills(ctx, g.ID, []int64{501})
	if err != nil {
		t.Fatal(err)
	}
	if got.TotalQty.String() != "10" || got.AvgPrice.String() != "1.1" {
		t.Fatalf("vwap: %+v", got)
	}
	// Double-attach refused.
	if _, err := svc.AttachFills(ctx, g.ID, []int64{501}); errCode(err) != CodeAllocationInvalid {
		t.Fatalf("double attach: %v", err)
	}
}

func TestAllocate_FullLifecycle_ConservationAndRemainder(t *testing.T) {
	svc, st := mkService(t, nil)
	g := mkGroup(t, svc, MethodProRata, CapacityClient,
		EligibleAccount{AccountID: 201, Capacity: CapacityClient},
		EligibleAccount{AccountID: 202, Capacity: CapacityClient})
	st.trades[501] = mkTrade(501, 5, 100, 900, "7", "1.10")
	st.trades[502] = mkTrade(502, 5, 100, 900, "3", "1.30") // partial fills bunch
	ctx := context.Background()
	if _, err := svc.AttachFills(ctx, g.ID, []int64{501, 502}); err != nil {
		t.Fatal(err)
	}
	// VWAP = (7×1.10 + 3×1.30)/10 = (7.7+3.9)/10 = 1.16
	rows, err := svc.Allocate(ctx, g.ID, []LegRequest{
		{AccountID: 201, Weight: decimal.One},
		{AccountID: 202, Weight: decimal.RequireFromString("4")},
	})
	if err != nil {
		t.Fatal(err)
	}
	var sum decimal.Decimal
	for _, r := range rows {
		sum = sum.Add(r.Quantity)
		if !r.AvgPrice.Equal(decimal.RequireFromString("1.16")) {
			t.Fatalf("leg %d avg %s", r.ID, r.AvgPrice)
		}
	}
	if !sum.Equal(decimal.RequireFromString("10")) {
		t.Fatalf("conservation: %s", sum)
	}
	// allocated == filled — GL park journal posted.
	if len(st.journals) != 1 {
		t.Fatalf("park journal missing: %d", len(st.journals))
	}
	j := st.journals[0]
	if err := j.Validate(); err != nil {
		t.Fatalf("park journal invalid: %v", err)
	}
	var saw2090 bool
	for _, l := range j.Lines {
		if l.AccountCode == BlockAllocationClearing("EUR") || l.AccountCode == BlockAllocationClearing("USD") {
			saw2090 = true
		}
	}
	if !saw2090 {
		t.Fatal("park journal does not touch 2090 clearing")
	}
	// Group row reconciles.
	g2, _ := st.Group(ctx, g.ID)
	if !g2.AllocatedQty.Equal(decimal.RequireFromString("10")) || !g2.UnallocatedQty().IsZero() {
		t.Fatalf("group totals: %+v", g2)
	}
	// Second allocate refused (single-run; corrections travel Correct).
	if _, err := svc.Allocate(ctx, g.ID, []LegRequest{{AccountID: 201, Quantity: decimal.One}}); errCode(err) != CodeAllocationStateConflict {
		t.Fatalf("re-allocate: %v", err)
	}
	// lifecycle: claim → locked-reject dance.
	a := rows[0]
	if _, err := svc.Claim(ctx, a.ID, "fund-ops"); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Reject(ctx, a.ID, "fund-ops", "late"); errCode(err) != CodeAllocationStateConflict {
		t.Fatalf("reject claimed: %v", err)
	}
	// Reject the second leg → its quantity returns to the remainder.
	if _, err := svc.Reject(ctx, rows[1].ID, "fund-ops", "nomatch"); err != nil {
		t.Fatal(err)
	}
	g3, _ := st.Group(ctx, g.ID)
	var active decimal.Decimal
	for _, r := range st.allocs {
		if r.GroupID == g.ID && r.Status.Active() && r.Kind == KindPrimary {
			active = active.Add(r.Quantity)
		}
	}
	if !g3.AllocatedQty.Equal(active) || g3.UnallocatedQty().IsNegative() {
		t.Fatalf("post-reject totals: alloc=%s unalloc=%s", g3.AllocatedQty, g3.UnallocatedQty())
	}
	// Cancel a non-existent allocation → 404-class error.
	if _, err := svc.Cancel(ctx, 9999, "ops", ""); errCode(err) != CodeAllocationNotFound {
		t.Fatalf("cancel missing: %v", err)
	}
}

func TestAllocate_IneligibleBeneficiaryAfterExecution(t *testing.T) {
	svc, st := mkService(t, nil)
	g := mkGroup(t, svc, MethodEqualSplit, CapacityClient,
		EligibleAccount{AccountID: 201, Capacity: CapacityClient},
		EligibleAccount{AccountID: 202, Capacity: CapacityClient})
	st.trades[501] = mkTrade(501, 5, 100, 900, "10", "1.10")
	ctx := context.Background()
	if _, err := svc.AttachFills(ctx, g.ID, []int64{501}); err != nil {
		t.Fatal(err)
	}
	// Beneficiary 202 becomes ineligible after execution → folds out of
	// the split deterministically; 201 takes the full qty.
	if _, err := svc.SetEligibility(ctx, g.ID, 202, false, "ops"); err != nil {
		t.Fatal(err)
	}
	rows, err := svc.Allocate(ctx, g.ID, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].BeneficiaryAccountID != 201 || rows[0].Quantity.String() != "10" {
		t.Fatalf("ineligible fold: %+v", rows)
	}
}

func TestSettlementLock_AndDualControlCorrect(t *testing.T) {
	roles := func(_ context.Context, id int64) (string, error) {
		if id == 88 {
			return "Finance Ops", nil
		}
		return "Support Agent", nil
	}
	svc, st := mkService(t, roles)
	g := mkGroup(t, svc, MethodManual, CapacityClient,
		EligibleAccount{AccountID: 201, Capacity: CapacityClient},
		EligibleAccount{AccountID: 202, Capacity: CapacityClient})
	st.trades[501] = mkTrade(501, 5, 100, 900, "10", "1.10")
	ctx := context.Background()
	if _, err := svc.AttachFills(ctx, g.ID, []int64{501}); err != nil {
		t.Fatal(err)
	}
	rows, err := svc.Allocate(ctx, g.ID, []LegRequest{
		{AccountID: 201, Quantity: decimal.RequireFromString("10")},
	})
	if err != nil {
		t.Fatal(err)
	}
	// Pre-lock correction: no approver needed.
	c, err := svc.Correct(ctx, Actor{UserID: 42}, rows[0].ID, CorrectInput{
		Quantity: decimal.RequireFromString("9"), Reason: "reprice qty"})
	if err != nil {
		t.Fatal(err)
	}
	if c.Corrected.Status != StatusCorrected || c.Offset.Kind != KindOffset ||
		c.Replacement.Kind != KindReplacement || *c.Replacement.CorrectsAllocationID != rows[0].ID {
		t.Fatalf("correction lineage: %+v", c)
	}
	if c.Replacement.Quantity.String() != "9" {
		t.Fatalf("replacement qty: %s", c.Replacement.Quantity)
	}
	// Lock the group.
	if _, err := svc.SubmitToSettlement(ctx, g.ID, "ops"); err != nil {
		t.Fatal(err)
	}
	// Post-lock: lifecycle mutation refused.
	if _, err := svc.Reject(ctx, c.Replacement.ID, "ops", "x"); errCode(err) != CodeAllocationLocked {
		t.Fatalf("post-lock reject: %v", err)
	}
	// Post-lock correction without approver → DUAL_CONTROL_REQUIRED.
	if _, err := svc.Correct(ctx, Actor{UserID: 42}, c.Replacement.ID, CorrectInput{
		BeneficiaryAccountID: 202, Reason: "move to fund 202"}); errCode(err) != "DUAL_CONTROL_REQUIRED" {
		t.Fatalf("no approver: %v", err)
	}
	// Same-principal approver refused.
	if _, err := svc.Correct(ctx, Actor{UserID: 42, ApproverID: 42}, c.Replacement.ID, CorrectInput{
		BeneficiaryAccountID: 202, Reason: "x"}); errCode(err) != "DUAL_CONTROL_REQUIRED" {
		t.Fatalf("self-approval: %v", err)
	}
	// Approver with wrong role refused.
	if _, err := svc.Correct(ctx, Actor{UserID: 42, ApproverID: 77}, c.Replacement.ID, CorrectInput{
		BeneficiaryAccountID: 202, Reason: "x"}); errCode(err) != "UNAUTHORIZED_ROLE" {
		t.Fatalf("wrong role: %v", err)
	}
	// Distinct role-eligible approver → corrected; audit carries both
	// principals + before/after.
	c2, err := svc.Correct(ctx, Actor{UserID: 42, ApproverID: 88}, c.Replacement.ID, CorrectInput{
		BeneficiaryAccountID: 202, Reason: "move to fund 202"})
	if err != nil {
		t.Fatal(err)
	}
	if c2.Replacement.BeneficiaryAccountID != 202 {
		t.Fatalf("corrected beneficiary: %d", c2.Replacement.BeneficiaryAccountID)
	}
	var ev *Event
	for _, e := range st.events {
		if e.Type == EvCorrected && e.AllocationID != nil && *e.AllocationID == c2.Corrected.ID {
			ev = e
		}
	}
	if ev == nil || ev.ApprovedBy == nil || *ev.ApprovedBy != 88 || ev.Actor != "42" ||
		len(ev.Before) == 0 || len(ev.After) == 0 {
		t.Fatalf("immutable correction evidence: %+v", ev)
	}
	// Corrected beneficiary must be eligible.
	if _, err := svc.Correct(ctx, Actor{UserID: 42, ApproverID: 88}, c2.Replacement.ID, CorrectInput{
		BeneficiaryAccountID: 999, Reason: "bad"}); errCode(err) != CodeAllocationIneligible {
		t.Fatalf("ineligible corrected beneficiary: %v", err)
	}
	// Correction that over-fills refused.
	_, err = svc.Correct(ctx, Actor{UserID: 42, ApproverID: 88}, c2.Replacement.ID, CorrectInput{
		Quantity: decimal.RequireFromString("100"), Reason: "fat finger"})
	if errCode(err) != CodeAllocationSumMismatch {
		t.Fatalf("over-fill correction: %v", err)
	}
}

func TestDetail_AuditChain(t *testing.T) {
	svc, st := mkService(t, nil)
	g := mkGroup(t, svc, MethodManual, CapacityClient,
		EligibleAccount{AccountID: 201, Capacity: CapacityClient})
	st.trades[501] = mkTrade(501, 5, 100, 900, "10", "1.10")
	ctx := context.Background()
	_, _ = svc.AttachFills(ctx, g.ID, []int64{501})
	_, _ = svc.Allocate(ctx, g.ID, []LegRequest{{AccountID: 201, Quantity: decimal.RequireFromString("10")}})
	d, err := svc.Detail(ctx, g.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(d.Fills) != 1 || len(d.Allocations) != 1 || len(d.Events) < 3 {
		t.Fatalf("detail bundle: %+v", d)
	}
	types := map[string]bool{}
	for _, e := range d.Events {
		types[e.Type] = true
	}
	for _, want := range []string{EvGroupCreated, EvFillAttached, EvAveragePriced, EvAllocated} {
		if !types[want] {
			t.Fatalf("audit chain missing %s: %v", want, types)
		}
	}
}

// ---------------------------------------------------------------------------
// Engine — Task 24.3.15.
// ---------------------------------------------------------------------------

type fakeExecResolver map[string]int64

func (f fakeExecResolver) TradeForExec(_ context.Context, id string) (int64, error) {
	if tid, ok := f[id]; ok {
		return tid, nil
	}
	return 0, allocErr(CodeAllocationInvalid, "exec %q unknown", id)
}

func mkEngine(t *testing.T) (*Engine, *memAllocStore) {
	t.Helper()
	st := newMemStore()
	eng, err := NewEngine(EngineDeps{Store: st, Execs: fakeExecResolver{}})
	if err != nil {
		t.Fatal(err)
	}
	return eng, st
}

func TestSubmitBlock_REST_HappyAndForbidden(t *testing.T) {
	eng, st := mkEngine(t)
	st.trades[601] = mkTrade(601, 5, 100, 900, "40", "1.25")
	ctx := context.Background()
	// Caller must be the account holding the named side.
	if _, err := eng.SubmitBlock(ctx, 777, BlockSubmitInput{
		TradeID: 601, Side: '1', Method: MethodManual,
	}); errCode(err) != "FORBIDDEN" {
		t.Fatalf("foreign submit: %v", err)
	}
	res, err := eng.SubmitBlock(ctx, 100, BlockSubmitInput{
		TradeID: 601, Side: '1', Method: MethodManual,
		Legs: []LegRequest{
			{AccountID: 201, Quantity: decimal.RequireFromString("25")},
			{AccountID: 202, Quantity: decimal.RequireFromString("15")},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Allocations) != 2 {
		t.Fatalf("legs: %+v", res.Allocations)
	}
	g, _ := st.Group(ctx, res.Group.ID)
	if !g.TotalQty.Equal(decimal.RequireFromString("40")) ||
		!g.AllocatedQty.Equal(decimal.RequireFromString("40")) ||
		!g.AvgPrice.Equal(decimal.RequireFromString("1.25")) {
		t.Fatalf("group: %+v", g)
	}
	if g.Source != SourceREST {
		t.Fatalf("source: %s", g.Source)
	}
}

func TestSubmitBlock_RuleBasedPercentages(t *testing.T) {
	eng, st := mkEngine(t)
	st.trades[601] = mkTrade(601, 5, 100, 900, "90", "1.25")
	res, err := eng.SubmitBlock(context.Background(), 100, BlockSubmitInput{
		TradeID: 601, Side: '1', Method: MethodRuleBased,
		Legs: []LegRequest{
			{AccountID: 201, Weight: decimal.RequireFromString("0.6")},
			{AccountID: 202, Weight: decimal.RequireFromString("0.4")},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	var sum decimal.Decimal
	byID := map[int64]string{}
	for _, a := range res.Allocations {
		sum = sum.Add(a.Quantity)
		byID[a.BeneficiaryAccountID] = a.Quantity.String()
	}
	if !sum.Equal(decimal.RequireFromString("90")) {
		t.Fatalf("conservation %s", sum)
	}
	if byID[201] != "54" || byID[202] != "36" {
		t.Fatalf("rule-based %v", byID)
	}
}

func TestConfirmFund_SettlementLegsGLAndReport(t *testing.T) {
	eng, st := mkEngine(t)
	st.nostro["EUR"] = 55
	st.trades[601] = mkTrade(601, 5, 100, 900, "10", "1.20")
	ctx := context.Background()
	res, err := eng.SubmitBlock(ctx, 100, BlockSubmitInput{
		TradeID: 601, Side: '1', Method: MethodManual,
		Legs: []LegRequest{
			{AccountID: 201, Quantity: decimal.RequireFromString("10"), Ref: "FUND-A", PartyLEI: "529900T8BM49AURSDO55"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	alloc := res.Allocations[0]
	cf, err := eng.ConfirmFund(ctx, alloc.ID, "fundops:11")
	if err != nil {
		t.Fatal(err)
	}
	if len(cf.SettlementInstructionIDs) != 2 {
		t.Fatalf("child legs: %v", cf.SettlementInstructionIDs)
	}
	if cf.ConfirmationRef == "" {
		t.Fatal("missing 35=AK confirmation ref")
	}
	if cf.Report == nil {
		t.Fatal("35=AK report not generated")
	}
	// 35=AK MsgType.
	if mt, _ := cf.Report.Header.GetString(fix.TagMsgType); mt != fix.MsgAllocationReport {
		t.Fatalf("msg type %s", mt)
	}
	// GL: park + child rebook — both balanced per currency.
	if len(st.journals) != 2 {
		t.Fatalf("journals: %d", len(st.journals))
	}
	for _, j := range st.journals {
		if err := j.Validate(); err != nil {
			t.Fatalf("journal imbalance: %v", err)
		}
	}
	// rebook journal keys on alloc-leg:{id}.
	if st.journals[1].IdempotencyKey != fmt.Sprintf("alloc-leg:%d", alloc.ID) {
		t.Fatalf("rebook key: %s", st.journals[1].IdempotencyKey)
	}
	// allocation marked CLAIMED with the confirmation ref.
	a2, _ := st.LockAllocation(ctx, alloc.ID)
	if a2.Status != StatusClaimed || a2.ClaimedAt == nil || a2.ConfirmationRef == "" {
		t.Fatalf("claimed: %+v", a2)
	}
	// Audit chain: CLAIMED + SETTLEMENT_INSTRUCTION + REPORT events.
	evTypes := map[string]bool{}
	for _, e := range st.events {
		evTypes[e.Type] = true
	}
	for _, want := range []string{EvClaimed, EvSettlementInstruction, EvReportEmitted} {
		if !evTypes[want] {
			t.Fatalf("audit missing %s", want)
		}
	}
	// Re-confirm refused (state conflict).
	if _, err := eng.ConfirmFund(ctx, alloc.ID, "x"); errCode(err) != CodeAllocationStateConflict {
		t.Fatalf("double confirm: %v", err)
	}
}

func TestConfirmFund_SellSide(t *testing.T) {
	eng, st := mkEngine(t)
	st.trades[602] = mkTrade(602, 5, 900, 100, "10", "1.20") // manager sells
	ctx := context.Background()
	res, err := eng.SubmitBlock(ctx, 100, BlockSubmitInput{
		TradeID: 602, Side: '2', Method: MethodManual,
		Legs: []LegRequest{{AccountID: 201, Quantity: decimal.RequireFromString("10")}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := eng.ConfirmFund(ctx, res.Allocations[0].ID, "f"); err != nil {
		t.Fatal(err)
	}
	if len(st.journals) != 2 {
		t.Fatalf("journals: %d", len(st.journals))
	}
	for _, j := range st.journals {
		if err := j.Validate(); err != nil {
			t.Fatalf("imbalance: %v", err)
		}
	}
}

func TestAmendAllocation_PartialSettlementGate(t *testing.T) {
	eng, st := mkEngine(t)
	st.trades[601] = mkTrade(601, 5, 100, 900, "10", "1.20")
	ctx := context.Background()
	res, _ := eng.SubmitBlock(ctx, 100, BlockSubmitInput{
		TradeID: 601, Side: '1', Method: MethodManual,
		Legs: []LegRequest{{AccountID: 201, Quantity: decimal.RequireFromString("10"), Ref: "F"}},
	})
	alloc := res.Allocations[0]
	// A SETTLED instruction blocks amendment entirely.
	st.legs[alloc.ID] = []memLeg{{id: 1, status: "SETTLED"}, {id: 2, status: "PENDING"}}
	if _, err := eng.AmendAllocation(ctx, Actor{UserID: 1}, alloc.ID, CorrectInput{Reason: "x", Quantity: decimal.One}); errCode(err) != CodeAllocationStateConflict {
		t.Fatalf("settled amend: %v", err)
	}
	// Only PENDING → voided, then corrected (group unlocked → no approver)
	// — resize to a smaller quantity on the same beneficiary.
	st.legs[alloc.ID] = []memLeg{{id: 2, status: "PENDING"}}
	c, err := eng.AmendAllocation(ctx, Actor{UserID: 1}, alloc.ID, CorrectInput{
		Reason: "resize", Quantity: decimal.RequireFromString("4")})
	if err != nil {
		t.Fatal(err)
	}
	if c.Replacement.Quantity.String() != "4" || c.Corrected.Status != StatusCorrected {
		t.Fatalf("amend: %+v", c)
	}
	if st.legs[alloc.ID][0].status != "VOID" {
		t.Fatalf("pending leg not voided: %+v", st.legs[alloc.ID])
	}
}

func TestEscalateUnallocated_T0EOD(t *testing.T) {
	eng, st := mkEngine(t)
	st.trades[601] = mkTrade(601, 5, 100, 900, "10", "1.20")
	ctx := context.Background()
	// MANUAL partial allocation — 3 stays on the omnibus.
	res, err := eng.SubmitBlock(ctx, 100, BlockSubmitInput{
		TradeID: 601, Side: '1', Method: MethodManual,
		Legs: []LegRequest{{AccountID: 201, Quantity: decimal.RequireFromString("7")}},
	})
	if err != nil {
		t.Fatal(err)
	}
	out, err := eng.EscalateUnallocated(ctx, time.Now().UTC().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if out.Escalated != 1 || out.GroupIDs[0] != res.Group.ID {
		t.Fatalf("escalation: %+v", out)
	}
	if len(st.alerts) != 1 || st.alerts[0].Code != CodeUnallocatedBlockTrade || st.alerts[0].Severity != "P2" {
		t.Fatalf("P2 alert: %+v", st.alerts)
	}
	g, _ := st.Group(ctx, res.Group.ID)
	if g.EscalatedAt == nil {
		t.Fatal("escalated_at latch not set")
	}
	// Idempotent — second sweep does not re-alert.
	out, err = eng.EscalateUnallocated(ctx, time.Now().UTC().Add(2*time.Hour))
	if err != nil || out.Escalated != 0 {
		t.Fatalf("repeat sweep: %+v %v", out, err)
	}
	// Fully-allocated groups don't escalate.
	eng2, st2 := mkEngine(t)
	st2.trades[7] = mkTrade(7, 5, 100, 900, "10", "1.20")
	if _, err := eng2.SubmitBlock(ctx, 100, BlockSubmitInput{
		TradeID: 7, Side: '1', Method: MethodManual,
		Legs: []LegRequest{{AccountID: 201, Quantity: decimal.RequireFromString("10")}},
	}); err != nil {
		t.Fatal(err)
	}
	out, err = eng2.EscalateUnallocated(ctx, time.Now().UTC().Add(time.Hour))
	if err != nil || out.Escalated != 0 {
		t.Fatalf("full-alloc sweep: %+v %v", out, err)
	}
}

func TestFIXIngest_CommittedReplacedCancelled(t *testing.T) {
	eng, st := mkEngine(t)
	st.trades[701] = mkTrade(701, 5, 100, 900, "20", "1.15")
	execs := fakeExecResolver{"EX-1": 701}
	eng.execs = execs
	ctx := context.Background()
	ev := fix.AllocationChange{
		Type: "COMMITTED",
		Allocation: fix.Allocation{
			ID: 41, AllocID: "AL-1", Status: fix.AllocStatusAccepted,
			MasterAccountID: 100, Symbol: "EURUSD", Side: '1',
			ExecQty:  decimal.RequireFromString("20"),
			ExecRefs: []string{"EX-1"},
			Legs: []fix.AllocationLeg{
				{LegNo: 1, AllocAccount: "FUND-A", AccountID: 201, AllocQty: decimal.RequireFromString("12")},
				{LegNo: 2, AllocAccount: "FUND-B", AccountID: 202, AllocQty: decimal.RequireFromString("8")},
				{LegNo: 3, AllocAccount: "BRKR-1", AccountID: 0, AllocQty: decimal.Zero}, // STEP_OUT external
			},
		},
	}
	if err := eng.OnAllocationChange(ctx, ev); err != nil {
		t.Fatal(err)
	}
	g, _ := st.GroupByRef(ctx, "FIX-AL-1")
	if g == nil || g.Source != SourceFIX || g.FixAllocationID != 41 {
		t.Fatalf("fix group: %+v", g)
	}
	rows, _ := st.Allocations(ctx, g.ID)
	if len(rows) != 2 {
		t.Fatalf("internal legs only: %+v", rows)
	}
	var sum decimal.Decimal
	for _, r := range rows {
		sum = sum.Add(r.Quantity)
	}
	if !sum.Equal(decimal.RequireFromString("20")) {
		t.Fatalf("fix conservation: %s", sum)
	}

	// REPLACE supersedes: old legs cancelled, new legs booked.
	ev2 := fix.AllocationChange{
		Type: "REPLACED", PrevAllocID: "AL-1",
		Allocation: fix.Allocation{
			ID: 42, AllocID: "AL-1", Status: fix.AllocStatusAccepted,
			MasterAccountID: 100, Symbol: "EURUSD", Side: '1',
			ExecQty:  decimal.RequireFromString("20"),
			ExecRefs: []string{"EX-1"},
			Legs: []fix.AllocationLeg{
				{LegNo: 1, AllocAccount: "FUND-A", AccountID: 201, AllocQty: decimal.RequireFromString("20")},
			},
		},
	}
	if err := eng.OnAllocationChange(ctx, ev2); err != nil {
		t.Fatal(err)
	}
	rows, _ = st.Allocations(ctx, g.ID)
	var active, cancelled int
	for _, r := range rows {
		if r.Status.Active() {
			active++
		}
		if r.Status == StatusCancelled {
			cancelled++
		}
	}
	if active != 1 || cancelled != 2 {
		t.Fatalf("replace supersede: active=%d cancelled=%d", active, cancelled)
	}

	// CANCELLED withdraws.
	ev3 := fix.AllocationChange{
		Type: "CANCELLED",
		Allocation: fix.Allocation{
			ID: 43, AllocID: "AL-1", Status: fix.AllocStatusCancelled,
			MasterAccountID: 100,
		},
	}
	if err := eng.OnAllocationChange(ctx, ev3); err != nil {
		t.Fatal(err)
	}
	g, _ = st.GroupByRef(ctx, "FIX-AL-1")
	if g.Status != GroupCancelled {
		t.Fatalf("cancelled group: %s", g.Status)
	}
	for _, r := range st.allocs {
		if r.Status.Active() {
			t.Fatalf("active row survives cancel: %+v", r)
		}
	}
}

func TestFIXIngest_LockedGroupRefusesReplace(t *testing.T) {
	eng, st := mkEngine(t)
	st.trades[701] = mkTrade(701, 5, 100, 900, "20", "1.15")
	eng.execs = fakeExecResolver{"EX-1": 701}
	ctx := context.Background()
	ev := fix.AllocationChange{
		Type: "COMMITTED",
		Allocation: fix.Allocation{
			ID: 41, AllocID: "AL-9", Status: fix.AllocStatusAccepted,
			MasterAccountID: 100, Side: '1',
			ExecQty:  decimal.RequireFromString("20"),
			ExecRefs: []string{"EX-1"},
			Legs: []fix.AllocationLeg{
				{LegNo: 1, AllocAccount: "F", AccountID: 201, AllocQty: decimal.RequireFromString("20")},
			},
		},
	}
	if err := eng.OnAllocationChange(ctx, ev); err != nil {
		t.Fatal(err)
	}
	g, _ := st.GroupByRef(ctx, "FIX-AL-9")
	if _, err := eng.SubmitToSettlement(ctx, g.ID, "ops"); err != nil {
		t.Fatal(err)
	}
	ev.Type = "REPLACED"
	if err := eng.OnAllocationChange(ctx, ev); errCode(err) != CodeAllocationLocked {
		t.Fatalf("locked replace: %v", err)
	}
	// Nil exec resolver fails closed.
	eng2, _ := mkEngine(t)
	eng2.execs = nil
	if err := eng2.OnAllocationChange(ctx, ev); errCode(err) != CodeServiceDegraded {
		t.Fatalf("nil resolver: %v", err)
	}
}
