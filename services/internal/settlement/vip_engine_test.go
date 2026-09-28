// Unit tests for Task 3.3.16 VIP 0–9 tier engine. Persistence and Redis
// are faked; the pure matrix (TierFor) and the family orchestration
// (RunOnce) are exercised without a database.
package settlement

import (
	"context"
	stderrors "errors"
	"fmt"
	"sort"
	"testing"
	"time"

	"exchange/pkg/decimal"
)

// ---------------------------------------------------------------------------
// Fakes
// ---------------------------------------------------------------------------

type fakeVipStore struct {
	accounts  []VipAccount
	schedule  []TierRule
	volumes   map[string]decimal.Decimal // "id1,id2,..." → usd — matched via key of sorted ids
	equities  map[int64]decimal.Decimal  // per-account USD equity, summed per family
	snapshots map[string]decimal.Decimal // "acct|YYYY-MM-DD"
	history   []histRow
	tiers     map[int64]int // recorded SetAccountTier calls
	volErr    error         // injected: FamilyVolumeUSD fails for all
	volErrIDs map[string]bool
}

type histRow struct {
	accountID    int64
	previousTier int
	newTier      int
	volume       decimal.Decimal
	avgEquity    decimal.Decimal
}

func newFakeVipStore() *fakeVipStore {
	return &fakeVipStore{
		volumes:   map[string]decimal.Decimal{},
		equities:  map[int64]decimal.Decimal{},
		snapshots: map[string]decimal.Decimal{},
		tiers:     map[int64]int{},
		volErrIDs: map[string]bool{},
	}
}

func idsKey(ids []int64) string {
	s := append([]int64(nil), ids...)
	sort.Slice(s, func(i, j int) bool { return s[i] < s[j] })
	return fmt.Sprint(s)
}

func (f *fakeVipStore) ActiveAccounts(context.Context) ([]VipAccount, error) {
	return f.accounts, nil
}

func (f *fakeVipStore) LoadSchedule(context.Context) ([]TierRule, error) {
	return f.schedule, nil
}

func (f *fakeVipStore) FamilyVolumeUSD(_ context.Context, ids []int64, _ time.Time) (decimal.Decimal, error) {
	k := idsKey(ids)
	if f.volErr != nil || f.volErrIDs[k] {
		return decimal.Zero, stderrors.New("volume query failed")
	}
	return f.volumes[k], nil
}

func (f *fakeVipStore) FamilyEquityUSD(_ context.Context, ids []int64) (decimal.Decimal, error) {
	total := decimal.Zero
	for _, id := range ids {
		total = total.Add(f.equities[id])
	}
	return total, nil
}

func (f *fakeVipStore) UpsertEquitySnapshot(_ context.Context, accountID int64, day time.Time, equityUSD decimal.Decimal) error {
	f.snapshots[fmt.Sprintf("%d|%s", accountID, day.Format("2006-01-02"))] = equityUSD
	return nil
}

func (f *fakeVipStore) AvgEquityUSD(_ context.Context, accountID int64, sinceDay time.Time) (decimal.Decimal, bool, error) {
	var sum decimal.Decimal
	var n int
	for k, v := range f.snapshots {
		var id int64
		var ds string
		fmt.Sscanf(k, "%d|%s", &id, &ds)
		d, err := time.Parse("2006-01-02", ds)
		if err != nil || id != accountID || d.Before(sinceDay) {
			continue
		}
		sum = sum.Add(v)
		n++
	}
	if n == 0 {
		return decimal.Zero, false, nil
	}
	return sum.Div(decimal.NewFromInt(int64(n))), true, nil
}

func (f *fakeVipStore) PersistTier(_ context.Context, accountID int64, previousTier, newTier int, volumeUSD, avgEquityUSD decimal.Decimal, _ time.Time) error {
	f.history = append(f.history, histRow{accountID, previousTier, newTier, volumeUSD, avgEquityUSD})
	for i := range f.accounts {
		if f.accounts[i].ID == accountID {
			f.accounts[i].VipTier = newTier
		}
	}
	return nil
}

func (f *fakeVipStore) SetAccountTier(_ context.Context, accountID int64, tier int) error {
	f.tiers[accountID] = tier
	for i := range f.accounts {
		if f.accounts[i].ID == accountID {
			f.accounts[i].VipTier = tier
		}
	}
	return nil
}

type fakeVipCache struct {
	records map[int64]VipCacheRecord
}

func (c *fakeVipCache) SetVipTier(_ context.Context, accountID int64, rec VipCacheRecord) error {
	c.records[accountID] = rec
	return nil
}

// testSchedule mirrors migration 086 seed data (thresholds only relevant
// columns used by TierFor).
var testSchedule = []TierRule{
	{Tier: 0, MinVolumeUSD: decimal.Zero, MinEquityUSD: decimal.Zero,
		MakerBps: decimal.MustFromString("1.0"), TakerBps: decimal.MustFromString("1.5")},
	{Tier: 1, MinVolumeUSD: decimal.NewFromInt(1_000_000), MinEquityUSD: decimal.NewFromInt(25_000),
		MakerBps: decimal.MustFromString("0.9"), TakerBps: decimal.MustFromString("1.4")},
	{Tier: 4, MinVolumeUSD: decimal.NewFromInt(100_000_000), MinEquityUSD: decimal.NewFromInt(1_000_000),
		MakerBps: decimal.MustFromString("-0.05"), TakerBps: decimal.MustFromString("1.0")},
	{Tier: 9, MinVolumeUSD: decimal.NewFromInt(5_000_000_000), MinEquityUSD: decimal.NewFromInt(50_000_000),
		MakerBps: decimal.MustFromString("-0.5"), TakerBps: decimal.MustFromString("0.5")},
}

// ---------------------------------------------------------------------------
// TierFor — qualification matrix edges
// ---------------------------------------------------------------------------

func TestTierForBoundaries(t *testing.T) {
	m := decimal.NewFromInt
	cases := []struct {
		name    string
		vol, eq decimal.Decimal
		want    int
	}{
		{"zero activity → VIP0", m(0), m(0), 0},
		{"volume just below VIP1 → VIP0", m(999_999), m(0), 0},
		{"volume at VIP1 boundary → VIP1", m(1_000_000), m(0), 1},
		{"equity alone qualifies VIP1", m(0), m(25_000), 1},
		{"equity just below VIP1 → VIP0", m(0), m(24_999), 0},
		{"OR logic: big volume, tiny equity", m(100_000_000), m(0), 4},
		{"OR logic: big equity, tiny volume", m(0), m(1_000_000), 4},
		{"VIP9 by volume", m(5_000_000_000), m(0), 9},
		{"VIP9 by equity", m(0), m(50_000_000), 9},
		{"above max stays 9", m(9_000_000_000), m(0), 9},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := TierFor(testSchedule, tc.vol, tc.eq); got != tc.want {
				t.Fatalf("TierFor(vol=%s, eq=%s) = %d, want %d", tc.vol, tc.eq, got, tc.want)
			}
		})
	}
}

func TestTierForEmptySchedule(t *testing.T) {
	if got := TierFor(nil, decimal.NewFromInt(1<<40), decimal.NewFromInt(1<<40)); got != 0 {
		t.Fatalf("nil schedule = %d, want 0", got)
	}
	// Unsorted schedule still resolves the highest qualified tier.
	unsorted := []TierRule{testSchedule[3], testSchedule[0], testSchedule[1]}
	if got := TierFor(unsorted, decimal.NewFromInt(1_000_000), decimal.Zero); got != 1 {
		t.Fatalf("unsorted schedule = %d, want 1", got)
	}
}

func TestNextUTCMidnight(t *testing.T) {
	// Just after midnight → next midnight is tomorrow.
	now := time.Date(2026, 9, 30, 0, 0, 1, 0, time.UTC)
	next := NextUTCMidnight(now)
	if next.Format("2006-01-02 15:04") != "2026-10-01 00:00" {
		t.Fatalf("NextUTCMidnight = %v", next)
	}
	// Mid-day → next midnight.
	now = time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)
	if next := NextUTCMidnight(now); next.Day() != 1 || next.Hour() != 0 {
		t.Fatalf("NextUTCMidnight = %v", next)
	}
	// Non-UTC input normalises to UTC.
	loc := time.FixedZone("EST", -5*3600)
	now = time.Date(2026, 9, 30, 18, 30, 0, 0, loc) // 23:30 UTC
	if next := NextUTCMidnight(now); next.Format("2006-01-02") != "2026-10-01" {
		t.Fatalf("NextUTCMidnight non-UTC = %v", next)
	}
}

// ---------------------------------------------------------------------------
// RunOnce orchestration
// ---------------------------------------------------------------------------

func TestRunOnceFamilyTierAndHistory(t *testing.T) {
	fs := newFakeVipStore()
	fs.schedule = testSchedule
	fs.accounts = []VipAccount{
		{ID: 11, VipTier: 0},                        // master
		{ID: 12, ParentID: &masterID11, VipTier: 0}, // sub of 11
		{ID: 21, VipTier: 2},                        // standalone master
	}
	fs.volumes[idsKey([]int64{11, 12})] = decimal.NewFromInt(100_000_000) // VIP4 by volume
	fs.volumes[idsKey([]int64{21})] = decimal.NewFromInt(0)
	fs.equities[11], fs.equities[12] = decimal.NewFromInt(300_000), decimal.NewFromInt(900_000)
	fs.equities[21] = decimal.NewFromInt(60_000) // VIP1 by equity (>25k, <1M)

	now := time.Date(2026, 9, 30, 0, 0, 0, 0, time.UTC)
	cache := &fakeVipCache{records: map[int64]VipCacheRecord{}}
	e := NewVipEngine(fs, cache, func() time.Time { return now })

	rep, err := e.RunOnce(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if rep.Masters != 2 || rep.Members != 3 {
		t.Fatalf("report = %+v", rep)
	}
	if rep.Changed != 2 {
		t.Fatalf("Changed = %d, want 2 (11→4, 21→1)", rep.Changed)
	}
	if len(rep.Errors) != 0 {
		t.Fatalf("unexpected errors: %v", rep.Errors)
	}

	// History audit rows appended for both masters with correct inputs.
	if len(fs.history) != 2 {
		t.Fatalf("history rows = %d, want 2", len(fs.history))
	}
	for _, h := range fs.history {
		switch h.accountID {
		case 11:
			if h.previousTier != 0 || h.newTier != 4 {
				t.Fatalf("master 11 history %+v", h)
			}
			if h.volume.String() != "100000000" {
				t.Fatalf("master 11 volume = %s", h.volume)
			}
			// Family equity 1.2M over window [today] → avg 1.2M.
			if h.avgEquity.String() != "1200000" {
				t.Fatalf("master 11 avgEquity = %s", h.avgEquity)
			}
		case 21:
			if h.previousTier != 2 || h.newTier != 1 {
				t.Fatalf("master 21 history %+v", h)
			}
		}
	}

	// Sub-account inherits family tier; cache written for every member.
	if fs.tiers[12] != 4 {
		t.Fatalf("sub-account tier = %d, want 4", fs.tiers[12])
	}
	for _, id := range []int64{11, 12, 21} {
		rec, ok := cache.records[id]
		if !ok {
			t.Fatalf("missing cache record for account %d", id)
		}
		if id != 21 && rec.Tier != 4 {
			t.Fatalf("cache tier for %d = %d, want 4", id, rec.Tier)
		}
	}
	if cache.records[21].Tier != 1 {
		t.Fatalf("cache tier 21 = %d, want 1", cache.records[21].Tier)
	}
	// Effective fee schedule synced: VIP4 maker is a rebate (-0.05 bps).
	if cache.records[11].MakerBps.String() != "-0.05" {
		t.Fatalf("maker_bps = %s, want -0.05", cache.records[11].MakerBps)
	}
	// Equity snapshot persisted for masters only.
	if _, ok := fs.snapshots["11|2026-09-30"]; !ok {
		t.Fatal("missing equity snapshot for master 11")
	}
	if _, ok := fs.snapshots["12|2026-09-30"]; ok {
		t.Fatal("sub-account must not get its own equity snapshot")
	}
}

var masterID11 = int64(11)

func TestRunOnceFailureIsolation(t *testing.T) {
	fs := newFakeVipStore()
	fs.schedule = testSchedule
	fs.accounts = []VipAccount{
		{ID: 11, VipTier: 0},
		{ID: 21, VipTier: 1},
	}
	fs.volErrIDs[idsKey([]int64{11})] = true
	fs.volumes[idsKey([]int64{21})] = decimal.NewFromInt(5_000_000_000)
	fs.equities[21] = decimal.Zero

	e := NewVipEngine(fs, nil, func() time.Time {
		return time.Date(2026, 9, 30, 0, 0, 0, 0, time.UTC)
	})
	rep, err := e.RunOnce(context.Background())
	if err != nil {
		t.Fatal(err) // per-account failure must not abort the run
	}
	if len(rep.Errors) != 1 || rep.Errors[0].AccountID != 11 {
		t.Fatalf("errors = %+v, want single failure on 11", rep.Errors)
	}
	// Failed family keeps previous tier and gets no history row.
	for _, h := range fs.history {
		if h.accountID == 11 {
			t.Fatal("failed family must not persist tier")
		}
	}
	// Healthy family still upgraded to VIP9.
	var found bool
	for _, h := range fs.history {
		if h.accountID == 21 && h.newTier == 9 {
			found = true
		}
	}
	if !found {
		t.Fatal("family 21 must upgrade to VIP9")
	}
}

func TestRunOnceUnchangedTierStillAudits(t *testing.T) {
	fs := newFakeVipStore()
	fs.schedule = testSchedule
	fs.accounts = []VipAccount{{ID: 11, VipTier: 1}}
	fs.volumes[idsKey([]int64{11})] = decimal.NewFromInt(1_500_000) // still VIP1
	fs.equities[11] = decimal.NewFromInt(30_000)

	e := NewVipEngine(fs, nil, func() time.Time {
		return time.Date(2026, 9, 30, 0, 0, 0, 0, time.UTC)
	})
	rep, err := e.RunOnce(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if rep.Changed != 0 {
		t.Fatalf("Changed = %d, want 0", rep.Changed)
	}
	// Audit row appended even when the tier is unchanged — the daily
	// recalculation itself is the audited event.
	if len(fs.history) != 1 || fs.history[0].newTier != 1 || fs.history[0].previousTier != 1 {
		t.Fatalf("history = %+v", fs.history)
	}
}

func TestRunOnceInactiveMasterSkipsFamily(t *testing.T) {
	fs := newFakeVipStore()
	fs.schedule = testSchedule
	fs.accounts = []VipAccount{{ID: 12, ParentID: &masterID11, VipTier: 0}}
	// Master 11 absent from the ACTIVE set → family frozen.
	e := NewVipEngine(fs, nil, func() time.Time {
		return time.Date(2026, 9, 30, 0, 0, 0, 0, time.UTC)
	})
	rep, err := e.RunOnce(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if rep.Masters != 0 || len(fs.history) != 0 || len(fs.tiers) != 0 {
		t.Fatalf("orphaned family must be skipped: %+v", rep)
	}
}
