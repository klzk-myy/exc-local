// Task 20.3.15 unit tests — in-memory seams only (no PG/Redis).
// PG-gated store/source tests live in depreciation_integration_test.go.
package analytics

import (
	"context"
	"errors"
	"sort"
	"sync"
	"testing"
	"time"

	"exchange/internal/notifications"
	"exchange/internal/oracle"
	"exchange/internal/risk"
	"exchange/pkg/decimal"
)

func depD(s string) decimal.Decimal { return decimal.RequireFromString(s) }

// --- fakes ---------------------------------------------------------------

type fakeDepMarks struct {
	m   map[string]oracle.MarkView
	err error
}

func (f *fakeDepMarks) Mark(_ context.Context, symbol string) (oracle.MarkView, error) {
	if f.err != nil {
		return oracle.MarkView{}, f.err
	}
	v, ok := f.m[symbol]
	if !ok {
		return oracle.MarkView{}, nil // !Found
	}
	return v, nil
}

type fakeDepPositions struct {
	open []DepPosition
	flat []DepPosition
}

func (f *fakeDepPositions) RetailLeveraged(context.Context) ([]DepPosition, error) {
	return f.open, nil
}

func (f *fakeDepPositions) FlatSince(context.Context, time.Time) ([]DepPosition, error) {
	return f.flat, nil
}

type memDepStore struct {
	mu  sync.Mutex
	eps map[int64][]*DepreciationEpisode // position_id → episodes by seq
}

func newMemDepStore() *memDepStore { return &memDepStore{eps: map[int64][]*DepreciationEpisode{}} }

func (s *memDepStore) Latest(_ context.Context, positionID int64) (*DepreciationEpisode, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	l := s.eps[positionID]
	if len(l) == 0 {
		return nil, nil
	}
	cp := *l[len(l)-1]
	return &cp, nil
}

func (s *memDepStore) Save(_ context.Context, ep *DepreciationEpisode) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	l := s.eps[ep.PositionID]
	cp := *ep
	cp.ID = int64(len(l) + 1)
	if len(l) > 0 && l[len(l)-1].Seq == ep.Seq {
		l[len(l)-1] = &cp
	} else {
		l = append(l, &cp)
	}
	s.eps[ep.PositionID] = l
	ep.ID = cp.ID
	return nil
}

type memDepNotifier struct {
	mu    sync.Mutex
	items []notifications.Notification
	err   error
}

func (n *memDepNotifier) Notify(_ context.Context, userID int64, event string, payload map[string]any) (int, error) {
	if n.err != nil {
		return 0, n.err
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	n.items = append(n.items, notifications.Notification{UserID: userID, Event: event, Payload: payload})
	return 1, nil
}

type fakeDepMargin struct {
	lv  *risk.MarginLevel
	err error
}

func (f *fakeDepMargin) MarginLevel(context.Context, int64) (*risk.MarginLevel, error) {
	return f.lv, f.err
}

func depSvc(t *testing.T, marks *fakeDepMarks, store *memDepStore, n *memDepNotifier) *DepreciationService {
	t.Helper()
	svc, err := NewDepreciationService(DepreciationDeps{
		Marks: marks,
		Positions: &fakeDepPositions{
			open: []DepPosition{depPos(1)},
		},
		Episodes: store,
		Notifier: n,
		Margin:   &fakeDepMargin{lv: &risk.MarginLevel{MarginLevelPct: depD("150"), Status: "NORMAL"}},
		Now:      func() time.Time { return time.Date(2026, 9, 30, 14, 0, 0, 0, time.UTC) },
	})
	if err != nil {
		t.Fatal(err)
	}
	return svc
}

func depPos(id int64) DepPosition {
	return DepPosition{
		PositionID: id, AccountID: 42, UserID: 7, InstrumentID: 9,
		Symbol: "EUR/USD", QuoteCurrency: "USD", Side: "LONG",
		Quantity: depD("10000"), EntryPrice: depD("1.1000"),
		MarginUsed: depD("366.67"),
	}
}

func liveMark(px string) oracle.MarkView {
	return oracle.MarkView{Price: depD(px), Found: true, ValidAt: time.Now()}
}

// --- tests ----------------------------------------------------------------

// -10% crossing → exactly one notice; a second evaluation at the same
// level does not re-notify (episode HWM dedupe).
func TestDepreciation_CrossingNotifiesOnce(t *testing.T) {
	marks := &fakeDepMarks{m: map[string]oracle.MarkView{"EUR/USD": liveMark("0.99")}} // −10%
	store := newMemDepStore()
	n := &memDepNotifier{}
	svc := depSvc(t, marks, store, n)

	res, err := svc.HourlyJob(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if res.Notified != 1 || len(n.items) != 1 {
		t.Fatalf("notified=%d items=%d want 1", res.Notified, len(n.items))
	}
	it := n.items[0]
	if it.Event != notifications.EventPositionDepreciation || it.UserID != 7 {
		t.Fatalf("bad notice %+v", it)
	}
	if it.Payload["threshold_pct"] != "-10" || it.Payload["dep_pct"] != "-10" {
		t.Fatalf("bad threshold payload %+v", it.Payload)
	}
	if it.Payload["margin_level_pct"] != "150" || it.Payload["margin_level_status"] != "NORMAL" {
		t.Fatalf("margin level missing from payload %+v", it.Payload)
	}
	if it.Payload["depreciated_value"] != "9900" || it.Payload["open_value"] != "11000" {
		t.Fatalf("bad values %+v", it.Payload)
	}

	res, err = svc.HourlyJob(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if res.Notified != 0 || len(n.items) != 1 {
		t.Fatalf("duplicate notice: notified=%d items=%d", res.Notified, len(n.items))
	}
}

// Deeper crossing → second notice at −20; nothing at intermediate
// evaluations below the first step.
func TestDepreciation_DeeperMultipleNotifies(t *testing.T) {
	marks := &fakeDepMarks{m: map[string]oracle.MarkView{"EUR/USD": liveMark("0.99")}}
	store := newMemDepStore()
	n := &memDepNotifier{}
	svc := depSvc(t, marks, store, n)

	if _, err := svc.HourlyJob(context.Background()); err != nil {
		t.Fatal(err)
	}
	marks.m["EUR/USD"] = liveMark("0.88") // −20%
	res, err := svc.HourlyJob(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if res.Notified != 1 || len(n.items) != 2 {
		t.Fatalf("want second notice at -20: %+v", res)
	}
	if n.items[1].Payload["threshold_pct"] != "-20" {
		t.Fatalf("threshold=%v", n.items[1].Payload["threshold_pct"])
	}
	// −7%: between −5% and −10% — no notice, episode stays open.
	marks.m["EUR/USD"] = liveMark("1.023")
	res, err = svc.HourlyJob(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if res.Notified != 0 || len(n.items) != 2 {
		t.Fatalf("unexpected notice at -7%%: %+v", res)
	}
	ep, _ := store.Latest(context.Background(), 1)
	if ep == nil || ep.Status != EpisodeOpen || !ep.DeepestNotifiedPct.Equal(depD("-20")) {
		t.Fatalf("episode %+v", ep)
	}
}

// Recovery above −5% resets the episode; the next −10% crossing opens a
// new episode and re-notifies the −10% threshold.
func TestDepreciation_RecoveryResetsThenReNotifies(t *testing.T) {
	marks := &fakeDepMarks{m: map[string]oracle.MarkView{"EUR/USD": liveMark("0.99")}}
	store := newMemDepStore()
	n := &memDepNotifier{}
	svc := depSvc(t, marks, store, n)

	if _, err := svc.HourlyJob(context.Background()); err != nil {
		t.Fatal(err)
	}
	// Recover to +0.5% — episode resets.
	marks.m["EUR/USD"] = liveMark("1.1055")
	res, err := svc.HourlyJob(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if res.Resets != 1 {
		t.Fatalf("reset expected: %+v", res)
	}
	ep, _ := store.Latest(context.Background(), 1)
	if ep == nil || ep.Status != EpisodeReset {
		t.Fatalf("episode not reset: %+v", ep)
	}
	// Re-cross −10% → new episode notifies again.
	marks.m["EUR/USD"] = liveMark("0.99")
	res, err = svc.HourlyJob(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if res.Notified != 1 || len(n.items) != 2 {
		t.Fatalf("re-crossing must re-notify: %+v items=%d", res, len(n.items))
	}
	ep, _ = store.Latest(context.Background(), 1)
	if ep.Seq != 2 || ep.Status != EpisodeOpen {
		t.Fatalf("want episode seq 2 open: %+v", ep)
	}
}

// SHORT positions depreciate when the mark rises.
func TestDepreciation_ShortSide(t *testing.T) {
	pos := depPos(2)
	pos.Side = "SHORT"
	svc, err := NewDepreciationService(DepreciationDeps{
		Marks:     &fakeDepMarks{m: map[string]oracle.MarkView{"EUR/USD": liveMark("1.232")}}, // +12%
		Positions: &fakeDepPositions{open: []DepPosition{pos}},
		Episodes:  newMemDepStore(),
		Notifier:  &memDepNotifier{},
		Now:       time.Now,
	})
	if err != nil {
		t.Fatal(err)
	}
	res, err := svc.HourlyJob(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if res.Notified != 1 {
		t.Fatalf("short -12%% must notify: %+v", res)
	}
}

// Missing mark fails closed: the position records an error, no notice.
func TestDepreciation_MarkUnavailableFailsClosed(t *testing.T) {
	marks := &fakeDepMarks{m: map[string]oracle.MarkView{}}
	store := newMemDepStore()
	n := &memDepNotifier{}
	svc := depSvc(t, marks, store, n)
	res, err := svc.HourlyJob(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Errors) != 1 || res.Notified != 0 || len(n.items) != 0 {
		t.Fatalf("expected recorded error, no notice: %+v", res)
	}
}

// Notify failure does not advance the HWM — next evaluation retries.
func TestDepreciation_NotifyErrorRetries(t *testing.T) {
	marks := &fakeDepMarks{m: map[string]oracle.MarkView{"EUR/USD": liveMark("0.99")}}
	store := newMemDepStore()
	n := &memDepNotifier{err: errors.New("queue down")}
	svc := depSvc(t, marks, store, n)
	if _, err := svc.HourlyJob(context.Background()); err != nil {
		t.Fatal(err)
	}
	ep, _ := store.Latest(context.Background(), 1)
	if ep == nil || !ep.DeepestNotifiedPct.IsZero() {
		t.Fatalf("HWM advanced despite failed emit: %+v", ep)
	}
	n.err = nil
	res, err := svc.HourlyJob(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if res.Notified != 1 {
		t.Fatalf("retry must notify: %+v", res)
	}
}

// EOD sweep: a position flattened intra-day that crossed before close
// (close mark implies −11%) emits the closed-notice and ends CLOSED.
func TestDepreciation_EODClosedPositionNotifies(t *testing.T) {
	marks := &fakeDepMarks{m: map[string]oracle.MarkView{}}
	store := newMemDepStore()
	n := &memDepNotifier{}
	closeMark := depD("0.9790") // −11% vs 1.10
	flat := depPos(3)
	flat.Quantity = decimal.Zero
	flat.CloseMark = &closeMark
	svc, err := NewDepreciationService(DepreciationDeps{
		Marks:     marks,
		Positions: &fakeDepPositions{flat: []DepPosition{flat}},
		Episodes:  store,
		Notifier:  n,
		Now:       time.Now,
	})
	if err != nil {
		t.Fatal(err)
	}
	res, err := svc.EndOfDaySweep(context.Background(), time.Now().UTC().Truncate(24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if res.Notified != 1 || len(n.items) != 1 {
		t.Fatalf("closed-position crossing must notify: %+v", res)
	}
	if n.items[0].Payload["closed"] != true {
		t.Fatalf("closed flag missing: %+v", n.items[0].Payload)
	}
	ep, _ := store.Latest(context.Background(), 3)
	if ep == nil || ep.Status != EpisodeClosed {
		t.Fatalf("episode must be CLOSED: %+v", ep)
	}
}

// A flat position evaluated while still in an open episode between −5%
// and −10% (no fresh crossing) ends the episode CLOSED without notice.
func TestDepreciation_FlatWithoutCrossingClosesEpisode(t *testing.T) {
	marks := &fakeDepMarks{m: map[string]oracle.MarkView{"EUR/USD": liveMark("0.99")}}
	store := newMemDepStore()
	n := &memDepNotifier{}
	svc := depSvc(t, marks, store, n)
	if _, err := svc.HourlyJob(context.Background()); err != nil {
		t.Fatal(err)
	}
	// Close at −7%: no new threshold, episode ends CLOSED, no notice.
	closeMark := depD("1.023")
	svc.positions.(*fakeDepPositions).open = nil
	flat := depPos(1)
	flat.Quantity = decimal.Zero
	flat.CloseMark = &closeMark
	svc.positions.(*fakeDepPositions).flat = []DepPosition{flat}
	res, err := svc.EndOfDaySweep(context.Background(), time.Now().UTC().Truncate(24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if res.Notified != 0 || len(n.items) != 1 {
		t.Fatalf("no new notice expected: %+v items=%d", res, len(n.items))
	}
	ep, _ := store.Latest(context.Background(), 1)
	if ep.Status != EpisodeClosed {
		t.Fatalf("episode must be CLOSED: %+v", ep)
	}
}

// Constructor rejects missing seams.
func TestDepreciation_NilDepsFailClosed(t *testing.T) {
	if _, err := NewDepreciationService(DepreciationDeps{}); err == nil {
		t.Fatal("nil deps must fail construction")
	}
}

// Events stays sorted-ish and contains the new event — guards the
// preferences surface vocabulary.
func TestDepreciation_EventRegistered(t *testing.T) {
	found := false
	for _, e := range notifications.Events() {
		if e == notifications.EventPositionDepreciation {
			found = true
		}
	}
	if !found {
		t.Fatal("position_depreciation missing from Events()")
	}
	if !notifications.IsCritical(notifications.EventPositionDepreciation) {
		t.Fatal("position_depreciation must bypass quiet hours (statutory same-day duty)")
	}
	if !sort.StringsAreSorted(notifications.Events()) {
		t.Fatal("Events() must stay sorted")
	}
}
