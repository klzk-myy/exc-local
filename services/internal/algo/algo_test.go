// Phase-16 algo framework unit tests — fakes for Store/ChildExecutor
// keep these PG-free; integration coverage is in algo_integration_test.go.
package algo

import (
	"context"
	"encoding/json"
	"sort"
	"sync"
	"testing"
	"time"

	"exchange/pkg/decimal"
	excerrors "exchange/pkg/errors"
)

// ---------------------------------------------------------------------------
// fakes
// ---------------------------------------------------------------------------

type fakeStore struct {
	mu       sync.Mutex
	nextPID  int64
	nextCID  int64
	parents  map[int64]*Parent
	children map[int64]*Child // row id → child
	byCID    map[string]int64 // "acct|cid" → parent id
}

func newFakeStore() *fakeStore {
	return &fakeStore{parents: map[int64]*Parent{},
		children: map[int64]*Child{}, byCID: map[string]int64{}, nextPID: 1, nextCID: 1}
}

func cloneParent(p *Parent) *Parent { c := *p; return &c }
func cloneChild(c *Child) *Child    { d := *c; return &d }

func (s *fakeStore) InsertParent(_ context.Context, p *Parent) (*Parent, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if p.ClientOrderID != "" {
		if id, ok := s.byCID[keyOf(p.AccountID, p.ClientOrderID)]; ok {
			return cloneParent(s.parents[id]), true, nil
		}
	}
	p.ID = s.nextPID
	s.nextPID++
	p.Status = StatusNew
	p.CreatedAt = time.Now()
	p.UpdatedAt = p.CreatedAt
	s.parents[p.ID] = cloneParent(p)
	if p.ClientOrderID != "" {
		s.byCID[keyOf(p.AccountID, p.ClientOrderID)] = p.ID
	}
	return cloneParent(p), false, nil
}

func keyOf(acct int64, cid string) string { return itoa(acct) + "|" + cid }
func itoa(i int64) string                 { return decimal.NewFromInt(i).String() }

func (s *fakeStore) GetParent(_ context.Context, id int64) (*Parent, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if p, ok := s.parents[id]; ok {
		return cloneParent(p), nil
	}
	return nil, nil
}

func (s *fakeStore) CASStatus(_ context.Context, id int64, from []string,
	to, detail string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.parents[id]
	if !ok {
		return false, nil
	}
	for _, f := range from {
		if p.Status == f {
			p.Status = to
			if detail != "" {
				p.ErrorDetail = detail
			}
			return true, nil
		}
	}
	return false, nil
}

func (s *fakeStore) SaveState(_ context.Context, id int64, state []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if p, ok := s.parents[id]; ok {
		p.State = append(json.RawMessage(nil), state...)
	}
	return nil
}

func (s *fakeStore) SetFilled(_ context.Context, id int64, filled decimal.Decimal) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if p, ok := s.parents[id]; ok {
		p.FilledQty = filled
	}
	return nil
}

func (s *fakeStore) ActiveParents(_ context.Context) ([]Parent, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []Parent
	for _, p := range s.parents {
		if p.Status == StatusPending || p.Status == StatusRunning || p.Status == StatusPaused {
			out = append(out, *cloneParent(p))
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

func (s *fakeStore) PendingDue(_ context.Context, now time.Time, _ int) ([]Parent, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []Parent
	for _, p := range s.parents {
		if p.Status == StatusPending && p.StartAt != nil && !p.StartAt.After(now) {
			out = append(out, *cloneParent(p))
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

func (s *fakeStore) ListParents(_ context.Context, accountID int64,
	status string, limit int) ([]Parent, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []Parent
	for _, p := range s.parents {
		if p.AccountID != accountID {
			continue
		}
		if status != "" && p.Status != status {
			continue
		}
		out = append(out, *cloneParent(p))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID > out[j].ID })
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func (s *fakeStore) InsertChild(_ context.Context, c *Child) (*Child, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if c.ClientOrderID == "" {
		c.ClientOrderID = childCID(c.AlgoOrderID, c.Seq)
	}
	c.ID = s.nextCID
	s.nextCID++
	c.Status = ChildPending
	s.children[c.ID] = cloneChild(c)
	return cloneChild(c), nil
}

func (s *fakeStore) UpdateChild(_ context.Context, c *Child) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.children[c.ID] = cloneChild(c)
	return nil
}

func (s *fakeStore) Children(_ context.Context, parentID int64) ([]Child, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []Child
	for _, c := range s.children {
		if c.AlgoOrderID == parentID {
			out = append(out, *cloneChild(c))
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Seq < out[j].Seq })
	return out, nil
}

func (s *fakeStore) OpenChildren(_ context.Context, parentID int64) ([]Child, error) {
	all, _ := s.Children(context.Background(), parentID)
	var out []Child
	for _, c := range all {
		switch c.Status {
		case ChildPending, ChildSubmitted, ChildOpen, ChildPartial:
			out = append(out, c)
		}
	}
	return out, nil
}

// fakeExec — scripted child executor.
type fakeExec struct {
	mu        sync.Mutex
	submitted []ChildRequest
	calls     int
	nextOID   int64
	statuses  map[int64]ChildStatus
	failN     int // reject the first N submits
	failOnNth int // reject exactly the Nth submit call (1-indexed)
	fillAll   bool
	cancelled map[int64]bool
}

func newFakeExec() *fakeExec {
	return &fakeExec{nextOID: 100, statuses: map[int64]ChildStatus{},
		fillAll: true, cancelled: map[int64]bool{}}
}

func (f *fakeExec) SubmitChild(_ context.Context, _ int64, req ChildRequest) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.failN > 0 {
		f.failN--
		return 0, codeErr("INSUFFICIENT_BALANCE", "fake reject")
	}
	if f.failOnNth == f.calls {
		return 0, codeErr("INSUFFICIENT_BALANCE", "fake reject")
	}
	f.nextOID++
	oid := f.nextOID
	f.submitted = append(f.submitted, req)
	st := ChildStatus{Status: "ACTIVE"}
	if f.fillAll {
		st = ChildStatus{Status: "FILLED", FilledQty: req.Quantity}
	}
	f.statuses[oid] = st
	return oid, nil
}

func (f *fakeExec) CancelChild(_ context.Context, _, orderID int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cancelled[orderID] = true
	st := f.statuses[orderID]
	st.Status = "CANCELLED"
	f.statuses[orderID] = st
	return nil
}

func (f *fakeExec) ChildStatus(_ context.Context, orderID int64) (*ChildStatus, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	st, ok := f.statuses[orderID]
	if !ok {
		return &ChildStatus{Status: "ACTIVE"}, nil
	}
	return &st, nil
}

type fakeQuote struct{ bid, ask decimal.Decimal }

func (q fakeQuote) BestBidAsk(context.Context, string) (decimal.Decimal, decimal.Decimal, bool, error) {
	return q.bid, q.ask, true, nil
}

type fakePips struct{ p decimal.Decimal }

func (f fakePips) PipSize(context.Context, string) (decimal.Decimal, error) { return f.p, nil }

func newTestEngine(t *testing.T, fs *fakeStore, fe *fakeExec) *Engine {
	t.Helper()
	e, err := NewEngine(Options{
		Store: fs, Exec: fe,
		Quote: fakeQuote{decimal.RequireFromString("1.10"), decimal.RequireFromString("1.12")},
		Pips:  fakePips{decimal.RequireFromString("0.0001")},
	})
	if err != nil {
		t.Fatalf("engine: %v", err)
	}
	return e
}

// ---------------------------------------------------------------------------
// validation
// ---------------------------------------------------------------------------

func TestTWAPValidation(t *testing.T) {
	e := newTestEngine(t, newFakeStore(), newFakeExec())
	cases := []struct {
		name    string
		body    string
		wantErr bool
	}{
		{"ok", `{"symbol":"EUR/USD","side":"BUY","total_qty":"100","interval_secs":10,"duration_secs":60}`, false},
		{"interval below 1s", `{"symbol":"EUR/USD","side":"BUY","total_qty":"100","interval_secs":0,"duration_secs":60}`, true},
		{"interval above 1h", `{"symbol":"EUR/USD","side":"BUY","total_qty":"100","interval_secs":3601,"duration_secs":7200}`, true},
		{"bad side", `{"symbol":"EUR/USD","side":"HOLD","total_qty":"100","interval_secs":10,"duration_secs":60}`, true},
		{"no duration", `{"symbol":"EUR/USD","side":"BUY","total_qty":"100","interval_secs":10}`, true},
		{"bad discretion", `{"symbol":"EUR/USD","side":"BUY","total_qty":"100","interval_secs":10,"duration_secs":60,"discretion_pips":4}`, true},
		{"negative qty", `{"symbol":"EUR/USD","side":"BUY","total_qty":"-5","interval_secs":10,"duration_secs":60}`, true},
	}
	for _, tc := range cases {
		req, err := ParseTypedSubmit(TypeTWAP, []byte(tc.body))
		if err != nil {
			t.Fatalf("%s: parse: %v", tc.name, err)
		}
		_, err = e.Submit(context.Background(), 7, req)
		if tc.wantErr && err == nil {
			t.Fatalf("%s: expected rejection", tc.name)
		}
		if !tc.wantErr && err != nil {
			t.Fatalf("%s: unexpected %v", tc.name, err)
		}
	}
}

func TestVPValidation(t *testing.T) {
	fs, fe := newFakeStore(), newFakeExec()
	e, err := NewEngine(Options{
		Store: fs, Exec: fe,
		Quote:  fakeQuote{decimal.RequireFromString("1.10"), decimal.RequireFromString("1.12")},
		Volume: NewTradeVolumeTracker(time.Minute),
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		rate    float64
		wantErr bool
	}{
		{0.005, true}, {0.01, false}, {0.25, false}, {0.50, false}, {0.51, true},
	} {
		body, _ := json.Marshal(map[string]any{
			"symbol": "EUR/USD", "side": "BUY", "total_qty": "1000",
			"participation_rate": tc.rate, "max_duration_secs": 60})
		req, _ := ParseTypedSubmit(TypeVP, body)
		_, err := e.Submit(context.Background(), 7, req)
		if tc.wantErr && err == nil {
			t.Fatalf("rate %v: expected rejection", tc.rate)
		}
		if !tc.wantErr && err != nil {
			t.Fatalf("rate %v: unexpected %v", tc.rate, err)
		}
	}
}

// VP fails closed without a volume seam.
func TestVPNoVolumeSourceFailsClosed(t *testing.T) {
	e := newTestEngine(t, newFakeStore(), newFakeExec()) // Volume nil
	body, _ := json.Marshal(map[string]any{
		"symbol": "EUR/USD", "side": "BUY", "total_qty": "1000",
		"participation_rate": 0.1, "max_duration_secs": 60})
	req, _ := ParseTypedSubmit(TypeVP, body)
	_, err := e.Submit(context.Background(), 7, req)
	if err == nil || codeString(err) != "SERVICE_DEGRADED" {
		t.Fatalf("want SERVICE_DEGRADED, got %v", err)
	}
}

func codeString(err error) string { return excerrors.CodeOf(err) }

// ---------------------------------------------------------------------------
// plan math
// ---------------------------------------------------------------------------

func TestTWAPPlanEqualSlicesAndConservation(t *testing.T) {
	total := decimal.RequireFromString("100.5")
	r := newPRNG()
	plan, bounds := twapPlan(total, 60*time.Second, 10*time.Second, r)
	if len(plan) != 6 {
		t.Fatalf("want 6 slices, got %d", len(plan))
	}
	sum := decimal.Zero
	for _, q := range plan {
		if q.IsNegative() {
			t.Fatal("negative slice")
		}
		sum = sum.Add(q)
	}
	if !sum.Equal(total) {
		t.Fatalf("plan sum %s != total %s — conservation broken", sum, total)
	}
	for i := 1; i < len(bounds); i++ {
		if bounds[i] < bounds[i-1] {
			t.Fatalf("bounds not monotone at %d", i)
		}
	}
	// Unperturbed (maxPm=0): strictly equal slices.
	plain := make([]decimal.Decimal, 4)
	base := total.Div(decimal.NewFromInt(4))
	for i := 0; i < 3; i++ {
		plain[i] = base
	}
	plain[3] = total.Sub(base.Mul(decimal.NewFromInt(3)))
	unpert := perturbSizes(newPRNG(), plain, total, 0)
	for i := 0; i < 3; i++ {
		if !unpert[i].Equal(base) {
			t.Fatalf("slice %d changed under zero jitter", i)
		}
	}
	if !unpert[3].Equal(total.Sub(base.Mul(decimal.NewFromInt(3)))) {
		t.Fatal("final slice residual changed")
	}
}

func TestPerturbSizesNeverNegative(t *testing.T) {
	// Front-load extreme draws: n=10, total small — the 90% cap must
	// keep the final slice positive.
	total := decimal.RequireFromString("1.0")
	sched := make([]decimal.Decimal, 10)
	for i := range sched {
		sched[i] = decimal.RequireFromString("0.1")
	}
	for trial := 0; trial < 200; trial++ {
		out := perturbSizes(newPRNG(), sched, total, 150)
		sum := decimal.Zero
		for i, q := range out {
			if q.IsNegative() {
				t.Fatalf("trial %d slice %d negative", trial, i)
			}
			sum = sum.Add(q)
		}
		if !sum.Equal(total) {
			t.Fatalf("trial %d sum %s != %s", trial, sum, total)
		}
	}
}

// Task 16.3.12: two independent runs must produce different child
// sequences — statistical: 20 runs, expect at least 2 distinct plans.
func TestAntiGamingNonDeterministic(t *testing.T) {
	total := decimal.RequireFromString("100")
	sched := []decimal.Decimal{decimal.RequireFromString("25"), decimal.RequireFromString("25"),
		decimal.RequireFromString("25"), decimal.RequireFromString("25")}
	seen := map[string]bool{}
	for i := 0; i < 20; i++ {
		out := perturbSizes(newPRNG(), sched, total, 150)
		key := ""
		for _, q := range out {
			key += q.String() + ","
		}
		seen[key] = true
	}
	if len(seen) < 2 {
		t.Fatal("20 runs produced identical child sequences — anti-gaming broken")
	}
	// Same for jittered bounds.
	bounds := map[int64]bool{}
	for i := 0; i < 20; i++ {
		bounds[jitterDuration(newPRNG(), time.Minute, TimingJitterPermille).Milliseconds()] = true
	}
	if len(bounds) < 2 {
		t.Fatal("jitter produced constant bounds")
	}
}

func TestVWAPProfileWeighting(t *testing.T) {
	// 4 buckets, skewed profile — slices must track weights.
	w := []decimal.Decimal{decimal.RequireFromString("70"),
		decimal.RequireFromString("10"), decimal.RequireFromString("10"),
		decimal.RequireFromString("10")}
	total := decimal.RequireFromString("100")
	plan := weightsToPlan(w, total)
	if len(plan) != 4 || !plan[0].Equal(decimal.RequireFromString("70")) {
		t.Fatalf("skewed profile not honored: %v", plan)
	}
	sum := decimal.Zero
	for _, q := range plan {
		sum = sum.Add(q)
	}
	if !sum.Equal(total) {
		t.Fatalf("sum %s != %s", sum, total)
	}
	// Flat fallback profile → equal slices.
	flat := weightsToPlan(flatProfile(4), total)
	if !flat[0].Equal(decimal.RequireFromString("25")) {
		t.Fatalf("flat profile not equal: %v", flat)
	}
}

func TestScaledWeightsAndLevels(t *testing.T) {
	e := newTestEngine(t, newFakeStore(), newFakeExec())
	// Equal distribution: 5 levels of 100 → 20 each.
	body := []byte(`{"symbol":"EUR/USD","side":"BUY","total_qty":"100",
		"levels":5,"distribution":"EQUAL","start_price":"1.1000","spacing_pips":"10"}`)
	req, err := ParseTypedSubmit(TypeScaled, body)
	if err != nil {
		t.Fatal(err)
	}
	p, err := e.Submit(context.Background(), 7, req)
	if err != nil {
		t.Fatal(err)
	}
	// driver runs async — wait for the plan to dispatch.
	deadline := time.Now().Add(3 * time.Second)
	var children []Child
	for time.Now().Before(deadline) {
		children, _ = e.store.Children(context.Background(), p.ID)
		if len(children) == 5 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if len(children) != 5 {
		t.Fatalf("want 5 level children, got %d", len(children))
	}
	sum := decimal.Zero
	for _, c := range children {
		sum = sum.Add(c.Qty)
		if c.Price == nil {
			t.Fatal("level missing price")
		}
	}
	if !sum.Equal(decimal.RequireFromString("100")) {
		t.Fatalf("level qty sum %s != 100", sum)
	}
	// BUY ladder descends: level prices strictly decreasing.
	for i := 1; i < len(children); i++ {
		if !children[i].Price.LessThan(*children[i-1].Price) {
			t.Fatalf("BUY ladder not descending at %d: %v", i, children[i].Price)
		}
	}
	// Deterministic client_order_id derivation.
	for _, c := range children {
		if c.ClientOrderID != childCID(p.ID, c.Seq) {
			t.Fatalf("child %d cid %q != derived %q", c.Seq, c.ClientOrderID,
				childCID(p.ID, c.Seq))
		}
	}
}

func TestScaledValidation(t *testing.T) {
	e := newTestEngine(t, newFakeStore(), newFakeExec())
	cases := []struct {
		name, body string
		wantErr    bool
	}{
		{"too many levels", `{"symbol":"EUR/USD","side":"BUY","total_qty":"10","levels":21,"distribution":"EQUAL","start_price":"1.1","spacing_pips":"5"}`, true},
		{"custom wrong len", `{"symbol":"EUR/USD","side":"BUY","total_qty":"10","levels":3,"distribution":"CUSTOM","weights":["1","1"],"start_price":"1.1","spacing_pips":"5"}`, true},
		{"custom ok", `{"symbol":"EUR/USD","side":"BUY","total_qty":"10","levels":2,"distribution":"CUSTOM","weights":["3","1"],"start_price":"1.1","spacing_pips":"5"}`, false},
		{"no spacing", `{"symbol":"EUR/USD","side":"BUY","total_qty":"10","levels":3,"distribution":"EQUAL"}`, true},
		{"level prices ok", `{"symbol":"EUR/USD","side":"SELL","total_qty":"10","levels":2,"distribution":"LINEAR","level_prices":["1.10","1.11"]}`, false},
	}
	for _, tc := range cases {
		req, err := ParseTypedSubmit(TypeScaled, []byte(tc.body))
		if err != nil {
			t.Fatalf("%s: parse: %v", tc.name, err)
		}
		_, err = e.Submit(context.Background(), 7, req)
		if tc.wantErr && err == nil {
			t.Fatalf("%s: expected rejection", tc.name)
		}
		if !tc.wantErr && err != nil {
			t.Fatalf("%s: unexpected %v", tc.name, err)
		}
	}
}

// ---------------------------------------------------------------------------
// framework: lifecycle
// ---------------------------------------------------------------------------

func TestDelayedDispatchStaysPending(t *testing.T) {
	fs, fe := newFakeStore(), newFakeExec()
	e := newTestEngine(t, fs, fe)
	future := time.Now().Add(time.Hour)
	req := &SubmitRequest{
		AlgoType: TypeTWAP, Symbol: "EUR/USD", Side: "BUY",
		TotalQty: decimal.RequireFromString("10"),
		StartAt:  &future,
		Params:   json.RawMessage(`{"interval_secs":10,"duration_secs":60}`),
	}
	p, err := e.Submit(context.Background(), 7, req)
	if err != nil {
		t.Fatal(err)
	}
	stored, _ := e.store.GetParent(context.Background(), p.ID)
	if stored.Status != StatusPending {
		t.Fatalf("delayed submit should be PENDING, got %s", stored.Status)
	}
	fe.mu.Lock()
	if len(fe.submitted) != 0 {
		t.Fatal("delayed algo dispatched a child early")
	}
	fe.mu.Unlock()
}

func TestIdempotentParentReplay(t *testing.T) {
	fs, fe := newFakeStore(), newFakeExec()
	e := newTestEngine(t, fs, fe)
	req := &SubmitRequest{
		AlgoType: TypeTWAP, Symbol: "EUR/USD", Side: "BUY",
		TotalQty:      decimal.RequireFromString("10"),
		Params:        json.RawMessage(`{"interval_secs":10,"duration_secs":60}`),
		ClientOrderID: "my-algo-1",
	}
	p1, err := e.Submit(context.Background(), 7, req)
	if err != nil {
		t.Fatal(err)
	}
	p2, err := e.Submit(context.Background(), 7, req)
	if err != nil {
		t.Fatal(err)
	}
	if p1.ID != p2.ID {
		t.Fatalf("idempotent replay returned different parent %d vs %d", p2.ID, p1.ID)
	}
}

func TestPauseResumeCancel(t *testing.T) {
	fs, fe := newFakeStore(), newFakeExec()
	e := newTestEngine(t, fs, fe)
	req := &SubmitRequest{
		AlgoType: TypeTWAP, Symbol: "EUR/USD", Side: "BUY",
		TotalQty: decimal.RequireFromString("10"),
		Params:   json.RawMessage(`{"interval_secs":30,"duration_secs":300}`),
	}
	p, err := e.Submit(context.Background(), 7, req)
	if err != nil {
		t.Fatal(err)
	}
	waitStatus := func(want string) *Parent {
		deadline := time.Now().Add(3 * time.Second)
		for time.Now().Before(deadline) {
			cur, _ := e.store.GetParent(context.Background(), p.ID)
			if cur.Status == want {
				return cur
			}
			time.Sleep(10 * time.Millisecond)
		}
		cur, _ := e.store.GetParent(context.Background(), p.ID)
		t.Fatalf("status never became %s (now %s)", want, cur.Status)
		return nil
	}
	waitStatus(StatusRunning)

	if _, err := e.Pause(context.Background(), 7, p.ID); err != nil {
		t.Fatalf("pause: %v", err)
	}
	waitStatus(StatusPaused)
	if _, err := e.Resume(context.Background(), 7, p.ID); err != nil {
		t.Fatalf("resume: %v", err)
	}
	waitStatus(StatusRunning)
	if _, err := e.Cancel(context.Background(), 7, p.ID, "test"); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	waitStatus(StatusCancelled)
	// Terminal transitions must reject.
	if _, err := e.Pause(context.Background(), 7, p.ID); err == nil {
		t.Fatal("paused a cancelled parent")
	}
	// Foreign account cannot touch the parent.
	if _, err := e.Pause(context.Background(), 999, p.ID); err == nil {
		t.Fatal("foreign account paused parent")
	}
}

// TWAP end-to-end with the fake pipeline: short schedule completes and
// all slices were dispatched with derived cids at mid price.
func TestTWAPEndToEndFills(t *testing.T) {
	fs, fe := newFakeStore(), newFakeExec()
	e := newTestEngine(t, fs, fe)
	req := &SubmitRequest{
		AlgoType: TypeTWAP, Symbol: "EUR/USD", Side: "BUY",
		TotalQty: decimal.RequireFromString("3"),
		Params:   json.RawMessage(`{"interval_secs":1,"duration_secs":3}`),
	}
	p, err := e.Submit(context.Background(), 7, req)
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		cur, _ := e.store.GetParent(context.Background(), p.ID)
		if isTerminalStatus(cur.Status) {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	cur, _ := e.store.GetParent(context.Background(), p.ID)
	if cur.Status != StatusCompleted && cur.Status != StatusExpired {
		t.Fatalf("parent status %s, detail %q", cur.Status, cur.ErrorDetail)
	}
	fe.mu.Lock()
	n := len(fe.submitted)
	fe.mu.Unlock()
	if n < 1 {
		t.Fatal("no children dispatched")
	}
	// All dispatches at the fake mid 1.11 within the discretion band
	// (pip size unwired → discretion collapses to exactly mid).
	for _, r := range fe.submitted {
		if !r.Price.Equal(decimal.RequireFromString("1.11")) {
			t.Fatalf("slice price %s != mid 1.11", r.Price)
		}
	}
}

// Task 16.3.23 — GET /algo-orders (list + child progress) and
// DELETE /algo-orders (cancel-all, zero orphan slices).
func TestListAndCancelAll(t *testing.T) {
	fs, fe := newFakeStore(), newFakeExec()
	e := newTestEngine(t, fs, fe)
	mk := func(acct int64, cid string) *Parent {
		req := &SubmitRequest{
			AlgoType: TypeTWAP, Symbol: "EUR/USD", Side: "BUY",
			TotalQty:      decimal.RequireFromString("10"),
			Params:        json.RawMessage(`{"interval_secs":30,"duration_secs":300}`),
			ClientOrderID: cid,
		}
		p, err := e.Submit(context.Background(), acct, req)
		if err != nil {
			t.Fatal(err)
		}
		return p
	}
	p1, p2 := mk(7, "l1"), mk(7, "l2")
	pf := mk(99, "foreign")

	waitRunning := func(id int64) {
		deadline := time.Now().Add(3 * time.Second)
		for time.Now().Before(deadline) {
			cur, _ := e.store.GetParent(context.Background(), id)
			if cur.Status == StatusRunning {
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
		t.Fatalf("parent %d never reached RUNNING", id)
	}
	waitRunning(p1.ID)
	waitRunning(p2.ID)
	waitRunning(pf.ID)

	// List scoped to account + status filter.
	running, err := e.List(context.Background(), 7, StatusRunning, 50)
	if err != nil {
		t.Fatal(err)
	}
	if len(running) != 2 {
		t.Fatalf("want 2 RUNNING parents for acct 7, got %d", len(running))
	}
	all, err := e.List(context.Background(), 7, "", 50)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 2 {
		t.Fatalf("acct-7 list leaked foreign parent (got %d)", len(all))
	}

	// Symbol-scoped cancel hits only EUR/USD parents of the account.
	nn, err := e.CancelAll(context.Background(), 7, "USD/JPY")
	if err != nil {
		t.Fatal(err)
	}
	if nn != 0 {
		t.Fatalf("symbol-scoped cancel-all touched %d wrong-symbol parents", nn)
	}

	// CancelAll terminates both parents and drains every live child.
	n, err := e.CancelAll(context.Background(), 7, "")
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("cancel-all transitioned %d parents, want 2", n)
	}
	for _, id := range []int64{p1.ID, p2.ID} {
		cur, _ := e.store.GetParent(context.Background(), id)
		if cur.Status != StatusCancelled {
			t.Fatalf("parent %d status %s, want CANCELLED", id, cur.Status)
		}
		open, _ := e.store.OpenChildren(context.Background(), id)
		if len(open) != 0 {
			t.Fatalf("parent %d left %d orphan open children", id, len(open))
		}
	}
	// Foreign account parent untouched.
	cur, _ := e.store.GetParent(context.Background(), pf.ID)
	if cur.Status == StatusCancelled {
		t.Fatal("cancel-all crossed account boundary")
	}
}

// ---------------------------------------------------------------------------
// spread
// ---------------------------------------------------------------------------

func TestSpreadMarketMismatchRejects(t *testing.T) {
	// Leg1 mid 1.11, leg2 mid 1.11 → market spread 0.00; asking to BUY
	// the spread at -1.00 is worse than market → SPREAD_ORDER_REJECTED.
	fs, fe := newFakeStore(), newFakeExec()
	e := newTestEngine(t, fs, fe)
	body := []byte(`{"legs":[
		{"symbol":"EUR/USD","side":"BUY","quantity":"10"},
		{"symbol":"GBP/USD","side":"SELL","quantity":"10"}],
		"spread_price":"-1.00"}`)
	req, err := ParseTypedSubmit(TypeSpread, body)
	if err != nil {
		t.Fatal(err)
	}
	_, err = e.Submit(context.Background(), 7, req)
	if err == nil || codeString(err) != "SPREAD_ORDER_REJECTED" {
		t.Fatalf("want SPREAD_ORDER_REJECTED, got %v", err)
	}
	// Rejection leaves no parent row.
	parents, _ := e.store.ListParents(context.Background(), 7, "", 0)
	if len(parents) != 0 {
		t.Fatal("rejected spread persisted a parent row")
	}
}

func TestSpreadBothLegsFill(t *testing.T) {
	fs, fe := newFakeStore(), newFakeExec()
	e := newTestEngine(t, fs, fe)
	body := []byte(`{"legs":[
		{"symbol":"EUR/USD","side":"BUY","quantity":"10"},
		{"symbol":"GBP/USD","side":"SELL","quantity":"10"}],
		"spread_price":"0.10"}`)
	req, _ := ParseTypedSubmit(TypeSpread, body)
	p, err := e.Submit(context.Background(), 7, req)
	if err != nil {
		t.Fatal(err)
	}
	if p.Status != StatusCompleted {
		t.Fatalf("spread parent status %s, detail %q", p.Status, p.ErrorDetail)
	}
	children, _ := e.store.Children(context.Background(), p.ID)
	if len(children) != 2 {
		t.Fatalf("want 2 leg children, got %d", len(children))
	}
}

func TestSpreadLegAFailureStopsLegB(t *testing.T) {
	fs, fe := newFakeStore(), newFakeExec()
	fe.failN = 1 // leg A's IOC rejects at the pipeline → leg B must not dispatch
	e := newTestEngine(t, fs, fe)
	body := []byte(`{"legs":[
		{"symbol":"EUR/USD","side":"BUY","quantity":"10"},
		{"symbol":"GBP/USD","side":"SELL","quantity":"10"}],
		"spread_price":"0.10"}`)
	req, _ := ParseTypedSubmit(TypeSpread, body)
	p, err := e.Submit(context.Background(), 7, req)
	if err != nil {
		t.Fatal(err)
	}
	if p.Status != StatusFailed {
		t.Fatalf("want FAILED, got %s (%q)", p.Status, p.ErrorDetail)
	}
	fe.mu.Lock()
	n := fe.calls
	fe.mu.Unlock()
	if n != 1 {
		t.Fatalf("leg B dispatched after leg A failure (%d submit calls)", n)
	}
}

// Rollback-on-partial: leg A fills, leg B rejects → the filled leg is
// flattened by an opposite-side UNWIND child (both-or-neither).
func TestSpreadRollbackOnPartial(t *testing.T) {
	fs, fe := newFakeStore(), newFakeExec()
	fe.failOnNth = 2 // leg B rejects; leg A and the unwind succeed
	e := newTestEngine(t, fs, fe)
	body := []byte(`{"legs":[
		{"symbol":"EUR/USD","side":"BUY","quantity":"10"},
		{"symbol":"GBP/USD","side":"SELL","quantity":"10"}],
		"spread_price":"0.10"}`)
	req, _ := ParseTypedSubmit(TypeSpread, body)
	p, err := e.Submit(context.Background(), 7, req)
	if err != nil {
		t.Fatal(err)
	}
	if p.Status != StatusFailed {
		t.Fatalf("want FAILED, got %s (%q)", p.Status, p.ErrorDetail)
	}
	children, _ := e.store.Children(context.Background(), p.ID)
	var unwind *Child
	for i := range children {
		if children[i].Role == RoleUnwind {
			unwind = &children[i]
		}
	}
	if unwind == nil {
		t.Fatalf("no unwind child; rows: %+v", children)
	}
	// The unwind flattens leg A's BUY with a SELL of equal filled size.
	if unwind.Side != "SELL" || unwind.Symbol != "EUR/USD" {
		t.Fatalf("unwind leg wrong: %+v", unwind)
	}
	if !unwind.Qty.Equal(decimal.RequireFromString("10")) {
		t.Fatalf("unwind qty %s != filled 10", unwind.Qty)
	}
}
