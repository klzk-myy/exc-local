// Unit tests for Phase-13 Task 13.3.6 — MiFID II RTS 9 OTR monitor.
// The fake Cmdable emulates otrRecordScript's observable behavior
// (sliding-window counts, breach flag + index); the REAL Lua script is
// exercised by the gated integration test at the bottom
// (EXC_REDIS_TEST=1).
package risk

import (
	"context"
	stderrors "errors"
	"os"
	"sort"
	"strconv"
	"sync"
	"testing"
	"time"

	goredis "github.com/redis/go-redis/v9"

	"exchange/internal/observability"
	excredis "exchange/internal/redis"
	"exchange/pkg/decimal"
	excerrors "exchange/pkg/errors"
)

// ---------------------------------------------------------------------------
// Fakes
// ---------------------------------------------------------------------------

// fakeOtrRedis emulates the Lua contract: per-pair sliding-window member
// lists, the account breach flag, the breach index, and the
// active-symbols bookkeeping set.
type fakeOtrRedis struct {
	goredis.Cmdable // embedded nil — any un-overridden method panics
	mu              sync.Mutex
	nowMs           int64
	events          map[string][]int64
	trades          map[string][]int64
	breach          map[string]bool
	index           map[string]bool
	active          map[string]map[string]bool
	evalErr         error
	existsErr       error
	smembersErr     error
}

func newFakeOtrRedis() *fakeOtrRedis {
	return &fakeOtrRedis{
		events: map[string][]int64{}, trades: map[string][]int64{},
		breach: map[string]bool{}, index: map[string]bool{},
		active: map[string]map[string]bool{},
	}
}

func argI64(v any) int64 {
	switch n := v.(type) {
	case int64:
		return n
	case int:
		return int64(n)
	case string:
		x, _ := strconv.ParseInt(n, 10, 64)
		return x
	default:
		return 0
	}
}

func trimWindow(list []int64, cutoff int64) []int64 {
	keep := list[:0]
	for _, ts := range list {
		if ts > cutoff {
			keep = append(keep, ts)
		}
	}
	return keep
}

// Eval emulates otrRecordScript (same key/argv layout as evalOne).
func (f *fakeOtrRedis) Eval(_ context.Context, _ string,
	keys []string, args ...interface{}) *goredis.Cmd {
	if f.evalErr != nil {
		return goredis.NewCmdResult(nil, f.evalErr)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	now := argI64(args[0])
	win := argI64(args[1])
	ratio, _ := decimal.NewFromString(args[2].(string))
	symbol := args[4].(string)
	incr := args[5].(string)
	acct := args[6].(string)
	cutoff := now - win
	f.events[keys[0]] = trimWindow(f.events[keys[0]], cutoff)
	f.trades[keys[1]] = trimWindow(f.trades[keys[1]], cutoff)
	if incr == "events" {
		f.events[keys[0]] = append(f.events[keys[0]], now)
	} else if incr == "trades" {
		f.trades[keys[1]] = append(f.trades[keys[1]], now)
	}
	if f.active[keys[4]] == nil {
		f.active[keys[4]] = map[string]bool{}
	}
	f.active[keys[4]][symbol] = true
	e := int64(len(f.events[keys[0]]))
	tr := int64(len(f.trades[keys[1]]))
	den := tr
	if den < 1 {
		den = 1
	}
	breached := decimal.NewFromInt(e).GreaterThan(
		ratio.Mul(decimal.NewFromInt(den)))
	if breached {
		f.breach[keys[2]] = true
		f.index[acct] = true
	}
	var b int64
	if breached {
		b = 1
	}
	return goredis.NewCmdResult([]interface{}{e, tr, b}, nil)
}

func (f *fakeOtrRedis) Exists(_ context.Context, keys ...string) *goredis.IntCmd {
	if f.existsErr != nil {
		return goredis.NewIntResult(0, f.existsErr)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	var n int64
	for _, k := range keys {
		if f.breach[k] {
			n++
		}
	}
	return goredis.NewIntResult(n, nil)
}

func (f *fakeOtrRedis) SMembers(_ context.Context, key string) *goredis.StringSliceCmd {
	if f.smembersErr != nil {
		return goredis.NewStringSliceResult(nil, f.smembersErr)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	if key == otrBreachIndexKey {
		for m := range f.index {
			out = append(out, m)
		}
	} else if set, ok := f.active[key]; ok {
		for m := range set {
			out = append(out, m)
		}
	}
	sort.Strings(out)
	return goredis.NewStringSliceResult(out, nil)
}

func (f *fakeOtrRedis) SRem(_ context.Context, key string,
	members ...interface{}) *goredis.IntCmd {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, m := range members {
		s, _ := m.(string)
		if key == otrBreachIndexKey {
			delete(f.index, s)
		} else if set, ok := f.active[key]; ok {
			delete(set, s)
		}
	}
	return goredis.NewIntResult(int64(len(members)), nil)
}

func (f *fakeOtrRedis) Del(_ context.Context, keys ...string) *goredis.IntCmd {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, k := range keys {
		delete(f.breach, k)
	}
	return goredis.NewIntResult(int64(len(keys)), nil)
}

func (f *fakeOtrRedis) TxPipeline() goredis.Pipeliner {
	return &fakeOtrPipe{rig: f}
}

type fakeOtrPipe struct {
	goredis.Pipeliner
	rig  *fakeOtrRedis
	dels []string
	rems [][2]string // (key, member)
}

func (p *fakeOtrPipe) Del(_ context.Context, keys ...string) *goredis.IntCmd {
	p.dels = append(p.dels, keys...)
	return goredis.NewIntResult(0, nil)
}

func (p *fakeOtrPipe) SRem(_ context.Context, key string,
	members ...interface{}) *goredis.IntCmd {
	for _, m := range members {
		if s, ok := m.(string); ok {
			p.rems = append(p.rems, [2]string{key, s})
		}
	}
	return goredis.NewIntResult(0, nil)
}

func (p *fakeOtrPipe) Exec(ctx context.Context) ([]goredis.Cmder, error) {
	p.rig.Del(ctx, p.dels...)
	for _, r := range p.rems {
		p.rig.SRem(ctx, r[0], r[1])
	}
	return nil, nil
}

// fakeOtrLimits resolves a fixed ratio + window for every scope.
type fakeOtrLimits struct {
	ratio  decimal.Decimal
	window time.Duration
}

func (f fakeOtrLimits) EffectiveLimits(int64, string, string) EffectiveLimits {
	r := f.ratio
	return EffectiveLimits{MaxOrderToTradeRatio: &r, OtrWindow: f.window}
}

type fakeSink struct {
	mu     sync.Mutex
	alerts []observability.Alert
}

func (s *fakeSink) Raise(_ context.Context, a observability.Alert) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.alerts = append(s.alerts, a)
	return nil
}

func (s *fakeSink) last() observability.Alert {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.alerts[len(s.alerts)-1]
}

func newOtrRig(ratio string, window time.Duration) (*fakeOtrRedis, *OtrMonitor, *fakeSink) {
	fx := newFakeOtrRedis()
	sink := &fakeSink{}
	m := NewOtrMonitor(fx, fakeOtrLimits{
		ratio:  decimal.RequireFromString(ratio),
		window: window,
	}).WithAlerts(sink)
	m.now = func() time.Time { return time.UnixMilli(fx.nowMs) }
	return fx, m, sink
}

func codedCode(t *testing.T, err error) string {
	t.Helper()
	var e *excerrors.Error
	if !stderrors.As(err, &e) {
		t.Fatalf("want coded error, got %T: %v", err, err)
	}
	return e.Code
}

// ---------------------------------------------------------------------------
// Tests
// ---------------------------------------------------------------------------

func TestOtrKeyLayout(t *testing.T) {
	if got := OtrEventsKey(7, "EUR/USD"); got != "otr:events:7:EUR/USD" {
		t.Fatalf("events key: %s", got)
	}
	if got := OtrTradesKey(7, "EUR/USD"); got != "otr:trades:7:EUR/USD" {
		t.Fatalf("trades key: %s", got)
	}
	if got := OtrBreachKey(7); got != "otr:breach:7" {
		t.Fatalf("breach key: %s", got)
	}
}

func TestOtrBreachRejectsNewOrders(t *testing.T) {
	fx, m, sink := newOtrRig("2", 60*time.Second)
	ctx := context.Background()
	// events > ratio × max(trades,1): third event with 0 trades breaches
	// (3 > 2×1).
	m.Event(ctx, 7, "T1", "EUR/USD")
	m.Event(ctx, 7, "T1", "EUR/USD")
	if err := m.Admission(ctx, 7); err != nil {
		t.Fatalf("under limit must admit: %v", err)
	}
	m.Event(ctx, 7, "T1", "EUR/USD")
	if !fx.breach[OtrBreachKey(7)] {
		t.Fatal("breach flag not raised")
	}
	if code := codedCode(t, m.Admission(ctx, 7)); code != CodeOtrLimitExceeded {
		t.Fatalf("want %s, got %s", CodeOtrLimitExceeded, code)
	}
	// Exactly one P2 firing alert; further events keep the flag but do
	// not re-page.
	m.Event(ctx, 7, "T1", "EUR/USD")
	if len(sink.alerts) != 1 {
		t.Fatalf("alerts: %+v", sink.alerts)
	}
	a := sink.last()
	if a.Severity != observability.SeverityP2 || a.Status != "firing" ||
		a.Code != CodeOtrLimitExceeded {
		t.Fatalf("alert: %+v", a)
	}
	if m.m.breaches.Load() != 1 {
		t.Fatalf("breaches counter = %d", m.m.breaches.Load())
	}
}

func TestOtrFillClearsBreach(t *testing.T) {
	fx, m, sink := newOtrRig("2", 60*time.Second)
	ctx := context.Background()
	for i := 0; i < 3; i++ {
		m.Event(ctx, 7, "T1", "EUR/USD")
	}
	if !fx.breach[OtrBreachKey(7)] {
		t.Fatal("setup: breach expected")
	}
	// Two fills: events=3 <= 2×max(2,1)=4 — under the limit; the
	// all-pairs re-evaluation clears the flag.
	m.Fill(ctx, 7, "T1", "EUR/USD")
	if !fx.breach[OtrBreachKey(7)] {
		t.Fatal("1 fill: 3 > 2×1 still breaches")
	}
	m.Fill(ctx, 7, "T1", "EUR/USD")
	if fx.breach[OtrBreachKey(7)] {
		t.Fatal("2 fills: 3 <= 2×2 — flag must clear")
	}
	if err := m.Admission(ctx, 7); err != nil {
		t.Fatalf("post-clear admission: %v", err)
	}
	if len(sink.alerts) != 2 || sink.last().Status != "resolved" {
		t.Fatalf("want firing+resolved alerts, got %+v", sink.alerts)
	}
	if fx.index["7"] {
		t.Fatal("breach index entry must be removed on clear")
	}
}

func TestOtrSweepClearsDecayedBreach(t *testing.T) {
	fx, m, _ := newOtrRig("2", 60*time.Second)
	ctx := context.Background()
	for i := 0; i < 3; i++ {
		m.Event(ctx, 7, "T1", "EUR/USD")
	}
	if !fx.breach[OtrBreachKey(7)] {
		t.Fatal("setup: breach expected")
	}
	// Age every event out of the 60s window; the sweeper re-evaluates
	// indexed accounts and clears the flag with no new traffic.
	fx.nowMs += 61_000
	m.sweep(ctx)
	if fx.breach[OtrBreachKey(7)] {
		t.Fatal("decayed breach must clear on sweep")
	}
	if err := m.Admission(ctx, 7); err != nil {
		t.Fatalf("post-decay admission: %v", err)
	}
}

func TestOtrAdmissionFailsClosedOnRedisError(t *testing.T) {
	fx, m, _ := newOtrRig("500", 60*time.Second)
	fx.existsErr = stderrors.New("redis down")
	if code := codedCode(t, m.Admission(context.Background(), 7)); code != "SERVICE_DEGRADED" {
		t.Fatalf("want SERVICE_DEGRADED, got %s", code)
	}
}

func TestOtrCountFailureIsBestEffortButCounted(t *testing.T) {
	fx, m, _ := newOtrRig("500", 60*time.Second)
	fx.evalErr = stderrors.New("redis down")
	m.Event(context.Background(), 7, "T1", "EUR/USD") // must not panic/return
	if m.m.countErrs.Load() != 1 {
		t.Fatalf("count errors = %d", m.m.countErrs.Load())
	}
}

func TestOtrMarketMakerScopedRatio(t *testing.T) {
	// The §9.6 MM allowance: an account+symbol-scoped row with a higher
	// ratio resolves through the existing lattice — prove the monitor
	// honors whatever EffectiveLimits returns per symbol.
	fx := newFakeOtrRedis()
	mmRatio := decimal.NewFromInt(10000)
	lim := &scopedFakeLimits{
		perSymbol: map[string]EffectiveLimits{
			"EUR/USD": {MaxOrderToTradeRatio: &mmRatio,
				OtrWindow: 60 * time.Second},
		},
		fallback: EffectiveLimits{
			MaxOrderToTradeRatio: func() *decimal.Decimal {
				d := decimal.NewFromInt(2)
				return &d
			}(),
			OtrWindow: 60 * time.Second,
		},
	}
	m := NewOtrMonitor(fx, lim)
	m.now = func() time.Time { return time.UnixMilli(fx.nowMs) }
	ctx := context.Background()
	// 3 events on the MM-symbol (ratio 10000) — under; 3 events on
	// another symbol (ratio 2) — breach.
	for i := 0; i < 3; i++ {
		m.Event(ctx, 7, "T1", "EUR/USD")
	}
	if fx.breach[OtrBreachKey(7)] {
		t.Fatal("MM-scoped ratio must not breach at 3 events")
	}
	for i := 0; i < 3; i++ {
		m.Event(ctx, 7, "T1", "GBP/USD")
	}
	if !fx.breach[OtrBreachKey(7)] {
		t.Fatal("default-scope ratio 2 must breach at 3 events")
	}
}

type scopedFakeLimits struct {
	perSymbol map[string]EffectiveLimits
	fallback  EffectiveLimits
}

func (f *scopedFakeLimits) EffectiveLimits(_ int64, _, symbol string) EffectiveLimits {
	if v, ok := f.perSymbol[symbol]; ok {
		return v
	}
	return f.fallback
}

// ---------------------------------------------------------------------------
// Gated integration — real Redis exercises the real Lua script.
//   EXC_REDIS_TEST=1 go test ./internal/risk/ -run TestOtrMonitorRedis -v
// ---------------------------------------------------------------------------

func TestOtrMonitorRedisIntegration(t *testing.T) {
	if testing.Short() || os.Getenv("EXC_REDIS_TEST") != "1" {
		t.Skip("EXC_REDIS_TEST not set")
	}
	addr := os.Getenv("EXC_REDIS_TEST_ADDR")
	if addr == "" {
		addr = "127.0.0.1:16379"
	}
	rdb := excredis.New(addr, os.Getenv("EXC_REDIS_TEST_PASSWORD"), 14).Client
	ctx := context.Background()
	if err := rdb.Ping(ctx).Err(); err != nil {
		t.Fatalf("redis ping: %v", err)
	}
	if err := rdb.FlushDB(ctx).Err(); err != nil {
		t.Fatalf("redis flushdb: %v", err)
	}
	t.Cleanup(func() { _ = rdb.Close() })

	ratio := decimal.NewFromInt(2)
	m := NewOtrMonitor(rdb, fakeOtrLimits{ratio: ratio, window: 60 * time.Second})
	for i := 0; i < 3; i++ {
		m.Event(ctx, 7, "T1", "EUR/USD")
	}
	exists, err := rdb.Exists(ctx, OtrBreachKey(7)).Result()
	if err != nil || exists != 1 {
		t.Fatalf("breach flag: n=%d err=%v", exists, err)
	}
	if err := m.Admission(ctx, 7); err == nil {
		t.Fatal("breached account must reject")
	} else if code := codedCode(t, err); code != CodeOtrLimitExceeded {
		t.Fatalf("want %s, got %s", CodeOtrLimitExceeded, code)
	}
	// Window counters are real zsets.
	if n, _ := rdb.ZCard(ctx, OtrEventsKey(7, "EUR/USD")).Result(); n != 3 {
		t.Fatalf("events zcard = %d", n)
	}
	m.Fill(ctx, 7, "T1", "EUR/USD")
	m.Fill(ctx, 7, "T1", "EUR/USD")
	if exists, _ := rdb.Exists(ctx, OtrBreachKey(7)).Result(); exists != 0 {
		t.Fatal("flag must clear once under the ratio")
	}
	if err := m.Admission(ctx, 7); err != nil {
		t.Fatalf("post-clear admission: %v", err)
	}
}
