package notifications

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// Fakes
// ---------------------------------------------------------------------------

type memQueue struct {
	items      []QueueItem
	acked      []string
	retries    []QueueItem
	retryTimes []time.Time
	requeued   int
	enqueueErr error
}

func (q *memQueue) Enqueue(_ context.Context, item QueueItem) error {
	if q.enqueueErr != nil {
		return q.enqueueErr
	}
	q.items = append(q.items, item)
	return nil
}

func (q *memQueue) Pop(_ context.Context, _ time.Duration) (QueueItem, string, bool, error) {
	return QueueItem{}, "", false, nil
}

func (q *memQueue) Ack(_ context.Context, raw string) error {
	q.acked = append(q.acked, raw)
	return nil
}

func (q *memQueue) RequeueAll(_ context.Context) (int, error) { return q.requeued, nil }

func (q *memQueue) ScheduleRetry(_ context.Context, item QueueItem, at time.Time) error {
	q.retries = append(q.retries, item)
	q.retryTimes = append(q.retryTimes, at)
	return nil
}

func (q *memQueue) PromoteDue(_ context.Context, _ time.Time, _ int64) (int, error) {
	return 0, nil
}

type fakeStore struct {
	prefs       map[int64]*Preferences
	deliveries  map[int64]*Delivery
	nextID      int64
	deadLetters int
	getErr      error
}

func newFakeStore() *fakeStore {
	return &fakeStore{prefs: map[int64]*Preferences{}, deliveries: map[int64]*Delivery{}, nextID: 1}
}

func (s *fakeStore) InsertDelivery(_ context.Context, d Delivery) (int64, error) {
	d.ID = s.nextID
	s.nextID++
	cp := d
	s.deliveries[d.ID] = &cp
	return d.ID, nil
}

func (s *fakeStore) RecordAttempt(_ context.Context, id int64, attempts int, lastErr string) error {
	d := s.deliveries[id]
	if d == nil {
		return nil
	}
	d.Attempts = attempts
	d.LastError = &lastErr
	return nil
}

func (s *fakeStore) MarkDelivered(_ context.Context, id int64, attempts int, at time.Time) error {
	d := s.deliveries[id]
	if d == nil {
		return nil
	}
	d.Status = StatusDelivered
	d.Attempts = attempts
	d.DeliveredAt = &at
	return nil
}

func (s *fakeStore) MarkStatus(_ context.Context, id int64, status string, attempts int, lastErr string) error {
	d := s.deliveries[id]
	if d == nil {
		return nil
	}
	d.Status = status
	d.Attempts = attempts
	d.LastError = &lastErr
	return nil
}

func (s *fakeStore) DeadLetter(_ context.Context, id int64, attempts int, lastErr string) error {
	d := s.deliveries[id]
	if d != nil {
		d.Status = StatusDeadLettered
		d.Attempts = attempts
		d.LastError = &lastErr
	}
	s.deadLetters++
	return nil
}

func (s *fakeStore) GetPreferences(_ context.Context, userID int64) (*Preferences, error) {
	if s.getErr != nil {
		return nil, s.getErr
	}
	if p, ok := s.prefs[userID]; ok {
		return p, nil
	}
	return nil, nil
}

func (s *fakeStore) PutPreferences(_ context.Context, p *Preferences) (*Preferences, error) {
	if err := p.Validate(); err != nil {
		return nil, err
	}
	p.UpdatedAt = time.Now().UTC()
	s.prefs[p.UserID] = p
	return p, nil
}

func (s *fakeStore) RecentDeliveries(_ context.Context, userID int64, _ int) ([]Delivery, error) {
	var out []Delivery
	for _, d := range s.deliveries {
		if d.UserID == userID {
			out = append(out, *d)
		}
	}
	return out, nil
}

type fakeDir struct{ email, phone string }

func (d fakeDir) Recipient(_ context.Context, _ int64) (string, string, error) {
	return d.email, d.phone, nil
}

type errDir struct{}

func (errDir) Recipient(_ context.Context, _ int64) (string, string, error) {
	return "", "", fmt.Errorf("dir down")
}

func testService(st Store, q Queuer, senders ...Sender) *Service {
	s, err := NewService(Options{
		Store: st, Queue: q, Senders: senders,
		Dir: fakeDir{email: "u@x.test", phone: "+155501"},
	})
	if err != nil {
		panic(err)
	}
	return s
}

// ---------------------------------------------------------------------------
// Preferences model
// ---------------------------------------------------------------------------

func TestPreferencesDefaults(t *testing.T) {
	p := DefaultPreferences(7)
	for _, ev := range Events() {
		if !p.Enabled(ev, ChannelEmail) || !p.Enabled(ev, ChannelWS) {
			t.Fatalf("default %s should enable email+ws", ev)
		}
		if p.Enabled(ev, ChannelSMS) || p.Enabled(ev, ChannelPush) {
			t.Fatalf("default %s should disable sms+push", ev)
		}
	}
}

func TestPreferencesMatrixOverride(t *testing.T) {
	p := DefaultPreferences(7)
	p.Matrix[EventDepositConfirmed] = map[string]bool{ChannelEmail: false, ChannelSMS: true}
	if p.Enabled(EventDepositConfirmed, ChannelEmail) {
		t.Fatal("matrix disable ignored")
	}
	if !p.Enabled(EventDepositConfirmed, ChannelSMS) {
		t.Fatal("matrix enable ignored")
	}
	// Untouched events keep defaults.
	if !p.Enabled(EventOrderFilled, ChannelEmail) {
		t.Fatal("untouched event lost default")
	}
}

func TestPreferencesValidate(t *testing.T) {
	p := DefaultPreferences(7)
	p.Matrix["bogus_event"] = map[string]bool{ChannelEmail: true}
	if err := p.Validate(); err == nil {
		t.Fatal("unknown event accepted")
	}
	p = DefaultPreferences(7)
	p.Matrix[EventOrderFilled] = map[string]bool{"pigeon": true}
	if err := p.Validate(); err == nil {
		t.Fatal("unknown channel accepted")
	}
	p = DefaultPreferences(7)
	p.Quiet = QuietHours{Enabled: true, Start: "25:00", End: "07:00"}
	if err := p.Validate(); err == nil {
		t.Fatal("bad quiet start accepted")
	}
	p.Quiet = QuietHours{Enabled: true, Start: "22:00", End: "22:00"}
	if err := p.Validate(); err == nil {
		t.Fatal("empty quiet window accepted")
	}
	p.Quiet = QuietHours{Enabled: false, Start: "22:00", End: "07:00"}
	if err := p.Validate(); err == nil {
		t.Fatal("quiet window without enable accepted")
	}
	p.Quiet = QuietHours{Enabled: true, Start: "22:00", End: "07:00"}
	if err := p.Validate(); err != nil {
		t.Fatalf("valid quiet window rejected: %v", err)
	}
}

func TestQuietHours(t *testing.T) {
	p := &Preferences{Quiet: QuietHours{Enabled: true, Start: "22:00", End: "07:00"}}
	inside := time.Date(2026, 1, 5, 23, 30, 0, 0, time.UTC)
	if !p.InQuietHours(inside) {
		t.Fatal("23:30 should be inside 22:00→07:00 window")
	}
	early := time.Date(2026, 1, 5, 3, 0, 0, 0, time.UTC)
	if !p.InQuietHours(early) {
		t.Fatal("03:00 should be inside wrap window")
	}
	outside := time.Date(2026, 1, 5, 12, 0, 0, 0, time.UTC)
	if p.InQuietHours(outside) {
		t.Fatal("12:00 should be outside window")
	}
	// QuietEndAfter: pre-midnight wraps to next day; post-midnight is same-day.
	end := p.QuietEndAfter(inside)
	want := time.Date(2026, 1, 6, 7, 0, 0, 0, time.UTC)
	if !end.Equal(want) {
		t.Fatalf("QuietEndAfter=%v want %v", end, want)
	}
	end = p.QuietEndAfter(early)
	want = time.Date(2026, 1, 5, 7, 0, 0, 0, time.UTC)
	if !end.Equal(want) {
		t.Fatalf("QuietEndAfter=%v want %v", end, want)
	}
	// Non-wrap window.
	p2 := &Preferences{Quiet: QuietHours{Enabled: true, Start: "12:00", End: "14:00"}}
	if !p2.InQuietHours(time.Date(2026, 1, 5, 13, 0, 0, 0, time.UTC)) {
		t.Fatal("13:00 inside 12:00→14:00")
	}
	if p2.InQuietHours(time.Date(2026, 1, 5, 15, 0, 0, 0, time.UTC)) {
		t.Fatal("15:00 outside 12:00→14:00")
	}
}

// ---------------------------------------------------------------------------
// Notify / Emit
// ---------------------------------------------------------------------------

func TestNotifyFansOutEnabledChannels(t *testing.T) {
	st := newFakeStore()
	q := &memQueue{}
	svc := testService(st, q,
		&MemSender{Ch: ChannelEmail}, &MemSender{Ch: ChannelWS})
	n, err := svc.Notify(context.Background(), 7, EventDepositConfirmed,
		map[string]any{"amount": "100.00", "currency": "USD"})
	if err != nil {
		t.Fatalf("Notify: %v", err)
	}
	if n != 2 {
		t.Fatalf("queued=%d want 2 (email+ws defaults)", n)
	}
	if len(q.items) != 2 || len(st.deliveries) != 2 {
		t.Fatalf("queue=%d deliveries=%d want 2/2", len(q.items), len(st.deliveries))
	}
	for _, d := range st.deliveries {
		if d.Status != StatusQueued || d.MaxAttempts != MaxAttempts {
			t.Fatalf("delivery %+v not QUEUED/max-5", d)
		}
	}
}

func TestNotifyRejectsUnknownEventAndUser(t *testing.T) {
	svc := testService(newFakeStore(), &memQueue{})
	if _, err := svc.Notify(context.Background(), 7, "made_up", nil); err == nil {
		t.Fatal("unknown event accepted")
	}
	if _, err := svc.Notify(context.Background(), 0, EventOrderFilled, nil); err == nil {
		t.Fatal("zero user accepted")
	}
}

func TestNotifyHonorsPreferenceOptOut(t *testing.T) {
	st := newFakeStore()
	st.prefs[7] = &Preferences{
		UserID: 7,
		Matrix: map[string]map[string]bool{
			EventDepositConfirmed: {ChannelEmail: false, ChannelWS: false, ChannelSMS: true},
		},
	}
	q := &memQueue{}
	svc := testService(st, q,
		&MemSender{Ch: ChannelEmail}, &MemSender{Ch: ChannelWS}, &MemSender{Ch: ChannelSMS})
	n, err := svc.Notify(context.Background(), 7, EventDepositConfirmed, nil)
	if err != nil || n != 1 {
		t.Fatalf("queued=%d err=%v want 1 (sms only)", n, err)
	}
	if q.items[0].Channel != ChannelSMS {
		t.Fatalf("queued channel %s want sms", q.items[0].Channel)
	}
}

// ---------------------------------------------------------------------------
// Dispatcher paths
// ---------------------------------------------------------------------------

func dispatchOne(t *testing.T, svc *Service, item QueueItem) {
	t.Helper()
	svc.NewDispatcher().process(context.Background(), item, "raw-member")
}

func TestDispatchDelivers(t *testing.T) {
	st := newFakeStore()
	q := &memQueue{}
	ms := &MemSender{Ch: ChannelEmail}
	svc := testService(st, q, ms)
	id, _ := st.InsertDelivery(context.Background(), Delivery{
		UserID: 7, Channel: ChannelEmail, Event: EventDepositConfirmed, Status: StatusQueued})
	dispatchOne(t, svc, QueueItem{DeliveryID: id, UserID: 7, Channel: ChannelEmail, Event: EventDepositConfirmed})
	if len(ms.Messages()) != 1 {
		t.Fatalf("sent=%d want 1", len(ms.Messages()))
	}
	d := st.deliveries[id]
	if d.Status != StatusDelivered || d.Attempts != 1 || d.DeliveredAt == nil {
		t.Fatalf("delivery %+v not DELIVERED@1", d)
	}
	if len(q.acked) != 1 {
		t.Fatalf("acked=%d want 1", len(q.acked))
	}
}

func TestDispatchSuppressesOptedOutChannel(t *testing.T) {
	st := newFakeStore()
	st.prefs[7] = &Preferences{
		UserID: 7,
		Matrix: map[string]map[string]bool{EventOrderFilled: {ChannelEmail: false}},
	}
	q := &memQueue{}
	ms := &MemSender{Ch: ChannelEmail}
	svc := testService(st, q, ms)
	id, _ := st.InsertDelivery(context.Background(), Delivery{
		UserID: 7, Channel: ChannelEmail, Event: EventOrderFilled, Status: StatusQueued})
	dispatchOne(t, svc, QueueItem{DeliveryID: id, UserID: 7, Channel: ChannelEmail, Event: EventOrderFilled})
	if len(ms.Messages()) != 0 {
		t.Fatal("send fired despite opt-out")
	}
	if st.deliveries[id].Status != StatusSuppressed {
		t.Fatalf("status=%s want SUPPRESSED", st.deliveries[id].Status)
	}
}

func TestDispatchQuietHoursDefersNonCritical(t *testing.T) {
	st := newFakeStore()
	st.prefs[7] = &Preferences{
		UserID: 7, Quiet: QuietHours{Enabled: true, Start: "00:00", End: "23:59"},
	}
	q := &memQueue{}
	ms := &MemSender{Ch: ChannelEmail}
	svc := testService(st, q, ms)
	svc.now = func() time.Time { return time.Date(2026, 1, 5, 12, 0, 0, 0, time.UTC) }
	id, _ := st.InsertDelivery(context.Background(), Delivery{
		UserID: 7, Channel: ChannelEmail, Event: EventOrderFilled, Status: StatusQueued})
	dispatchOne(t, svc, QueueItem{DeliveryID: id, UserID: 7, Channel: ChannelEmail, Event: EventOrderFilled})
	if len(ms.Messages()) != 0 {
		t.Fatal("send fired inside quiet hours")
	}
	if len(q.retries) != 1 {
		t.Fatalf("retries=%d want 1 (deferred)", len(q.retries))
	}
	if q.retries[0].Attempts != 0 {
		t.Fatal("deferred item consumed an attempt")
	}
	if st.deliveries[id].Status != StatusQueued {
		t.Fatalf("status=%s want QUEUED", st.deliveries[id].Status)
	}
}

func TestDispatchCriticalBypassesQuietHours(t *testing.T) {
	st := newFakeStore()
	st.prefs[7] = &Preferences{
		UserID: 7, Quiet: QuietHours{Enabled: true, Start: "00:00", End: "23:59"},
	}
	q := &memQueue{}
	ms := &MemSender{Ch: ChannelEmail}
	svc := testService(st, q, ms)
	svc.now = func() time.Time { return time.Date(2026, 1, 5, 12, 0, 0, 0, time.UTC) }
	for _, ev := range []string{EventSecurityAlert, EventLiquidationWarning} {
		id, _ := st.InsertDelivery(context.Background(), Delivery{
			UserID: 7, Channel: ChannelEmail, Event: ev, Status: StatusQueued})
		dispatchOne(t, svc, QueueItem{DeliveryID: id, UserID: 7, Channel: ChannelEmail, Event: ev})
	}
	if len(ms.Messages()) != 2 {
		t.Fatalf("critical sends=%d want 2 (quiet-hours bypass)", len(ms.Messages()))
	}
}

func TestDispatchRetriesThenDeadLetters(t *testing.T) {
	st := newFakeStore()
	q := &memQueue{}
	ms := &MemSender{Ch: ChannelEmail, FailErr: errors.New("provider down")}
	svc := testService(st, q, ms)
	fixedNow := time.Date(2026, 1, 5, 12, 0, 0, 0, time.UTC)
	svc.now = func() time.Time { return fixedNow }
	id, _ := st.InsertDelivery(context.Background(), Delivery{
		UserID: 7, Channel: ChannelEmail, Event: EventOrderFilled, Status: StatusQueued})
	item := QueueItem{DeliveryID: id, UserID: 7, Channel: ChannelEmail, Event: EventOrderFilled}
	// Attempts 1..4 → retry scheduled on the backoff ladder.
	for i := 1; i <= 4; i++ {
		dispatchOne(t, svc, item)
		item = q.retries[len(q.retries)-1]
		if item.Attempts != i {
			t.Fatalf("attempt %d: item.Attempts=%d", i, item.Attempts)
		}
		wantDelay := backoffFor(i)
		gotDelay := q.retryTimes[len(q.retryTimes)-1].Sub(svc.now())
		if gotDelay != wantDelay {
			t.Fatalf("attempt %d delay=%v want %v", i, gotDelay, wantDelay)
		}
	}
	// Attempt 5 → dead letter, no more retries.
	dispatchOne(t, svc, item)
	if st.deliveries[id].Status != StatusDeadLettered {
		t.Fatalf("status=%s want DEAD_LETTERED", st.deliveries[id].Status)
	}
	if st.deadLetters != 1 {
		t.Fatalf("dead letters=%d want 1", st.deadLetters)
	}
	if st.deliveries[id].Attempts != MaxAttempts {
		t.Fatalf("attempts=%d want %d", st.deliveries[id].Attempts, MaxAttempts)
	}
}

func TestDispatchUnroutableSuppresses(t *testing.T) {
	st := newFakeStore()
	q := &memQueue{}
	ms := &MemSender{Ch: ChannelSMS}
	svc, err := NewService(Options{
		Store: st, Queue: q, Senders: []Sender{ms},
		Dir: fakeDir{email: "u@x.test", phone: ""}, // no phone on file
	})
	if err != nil {
		t.Fatal(err)
	}
	id, _ := st.InsertDelivery(context.Background(), Delivery{
		UserID: 7, Channel: ChannelSMS, Event: EventOrderFilled, Status: StatusQueued})
	dispatchOne(t, svc, QueueItem{DeliveryID: id, UserID: 7, Channel: ChannelSMS, Event: EventOrderFilled})
	if st.deliveries[id].Status != StatusSuppressed {
		t.Fatalf("status=%s want SUPPRESSED (no phone)", st.deliveries[id].Status)
	}
	if len(q.retries) != 0 {
		t.Fatal("unroutable item was retried")
	}
}

func TestDispatchTransientErrorRetriesBounded(t *testing.T) {
	st := newFakeStore()
	st.getErr = errors.New("pg down")
	q := &memQueue{}
	svc := testService(st, q, &MemSender{Ch: ChannelEmail})
	item := QueueItem{DeliveryID: 1, UserID: 7, Channel: ChannelEmail, Event: EventSecurityAlert}
	for i := 0; i < MaxAttempts-1; i++ {
		dispatchOne(t, svc, item)
		item = q.retries[len(q.retries)-1]
	}
	// Final failure (getErr still set) → dead-letters at max attempts.
	dispatchOne(t, svc, item)
	if st.deadLetters != 1 {
		t.Fatalf("dead letters=%d want 1", st.deadLetters)
	}
}

// ---------------------------------------------------------------------------
// Anti-phishing banner
// ---------------------------------------------------------------------------

func TestAntiPhishBanner(t *testing.T) {
	st := newFakeStore()
	q := &memQueue{}
	ms := &MemSender{Ch: ChannelEmail}
	svc, err := NewService(Options{
		Store: st, Queue: q, Senders: []Sender{ms},
		Dir: fakeDir{email: "u@x.test"},
		Anti: AntiPhishFunc(func(_ context.Context, _ int64) (string, error) {
			return "BLUE FALCON", nil
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	msg, err := svc.buildMessage(context.Background(), QueueItem{
		UserID: 7, Channel: ChannelEmail, Event: EventSecurityAlert,
		Payload: []byte(`{"ip":"1.2.3.4"}`)})
	if err != nil {
		t.Fatalf("buildMessage: %v", err)
	}
	if msg.To != "u@x.test" {
		t.Fatalf("to=%q", msg.To)
	}
	if got := msg.Body; !strings.HasPrefix(got, "Anti-phishing code:") {
		t.Fatalf("body missing anti-phish header: %q", got)
	}
	if want := "BLUE FALCON"; !strings.Contains(msg.Body, want) {
		t.Fatalf("body missing code %q: %q", want, msg.Body)
	}
}

func TestAntiPhishUnsetBanner(t *testing.T) {
	st := newFakeStore()
	ms := &MemSender{Ch: ChannelEmail}
	svc, err := NewService(Options{
		Store: st, Queue: &memQueue{}, Senders: []Sender{ms},
		Dir: fakeDir{email: "u@x.test"},
		// nil Anti — lookup absent → prompt banner.
	})
	if err != nil {
		t.Fatal(err)
	}
	msg, err := svc.buildMessage(context.Background(), QueueItem{
		UserID: 7, Channel: ChannelEmail, Event: EventDepositConfirmed})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(msg.Body, "have not set an anti-phishing code") {
		t.Fatalf("expected unset-code banner: %q", msg.Body)
	}
}
