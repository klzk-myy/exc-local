package accounts

import (
	"context"
	"errors"
	"sort"
	"sync"
	"testing"
	"time"

	excerrors "exchange/pkg/errors"
)

// ---- in-memory CountdownStore fake ----

type fakeCountdownStore struct {
	mu    sync.Mutex
	timer map[int64]struct{ deadline, ttl int64 } // accountID -> armed
	now   func() time.Time
	// simulated expiry: arm entries whose deadline <= now are "expired"
	// at PopExpired time (mirrors real TTL eviction).
}

func newFakeCountdownStore(now func() time.Time) *fakeCountdownStore {
	return &fakeCountdownStore{timer: map[int64]struct{ deadline, ttl int64 }{}, now: now}
}

func (f *fakeCountdownStore) Arm(_ context.Context, id int64, deadlineMs int64,
	ttl time.Duration, onlyIfAbsent bool) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if onlyIfAbsent {
		if e, ok := f.timer[id]; ok && e.deadline > f.now().UnixMilli() {
			return false, nil
		}
	}
	f.timer[id] = struct{ deadline, ttl int64 }{deadlineMs, ttl.Milliseconds()}
	return true, nil
}

func (f *fakeCountdownStore) Disarm(_ context.Context, id int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.timer, id)
	return nil
}

func (f *fakeCountdownStore) Deadline(_ context.Context, id int64) (int64, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	e, ok := f.timer[id]
	if !ok || e.deadline <= f.now().UnixMilli() {
		return 0, false, nil
	}
	return e.deadline, true, nil
}

func (f *fakeCountdownStore) PopExpired(_ context.Context, nowMs int64, limit int) ([]int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []int64
	for id, e := range f.timer {
		if e.deadline <= nowMs {
			out = append(out, id)
			delete(f.timer, id)
			if len(out) >= limit {
				break
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out, nil
}

// ---- fake dispatcher ----

type fakeDispatcher struct {
	mu        sync.Mutex
	cancels   []MassCancelScope
	closes    []CloseOrderRequest
	cancelErr error
	closeErrs map[int64]error // instrumentID -> err
	acceptAll bool
}

func (f *fakeDispatcher) MassCancel(_ context.Context, scope MassCancelScope) (*MassCancelResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cancels = append(f.cancels, scope)
	if f.cancelErr != nil {
		return nil, f.cancelErr
	}
	return &MassCancelResult{Cancelled: 7}, nil
}

func (f *fakeDispatcher) SubmitClose(_ context.Context, req CloseOrderRequest) (*OrderAck, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closes = append(f.closes, req)
	if err := f.closeErrs[req.InstrumentID]; err != nil {
		return nil, err
	}
	return &OrderAck{OrderID: int64(9000 + len(f.closes)), ClientOrderID: req.ClientOrderID, Accepted: true}, nil
}

func requireCode(t *testing.T, err error, want string) {
	t.Helper()
	var e *excerrors.Error
	if err == nil {
		t.Fatalf("expected coded error %s, got nil", want)
	}
	if !errors.As(err, &e) || e.Code != want {
		t.Fatalf("expected code %s, got %v", want, err)
	}
}

// ---- unit tests ----

func TestCountdownSetValidatesRange(t *testing.T) {
	store := newFakeCountdownStore(time.Now)
	svc := NewDeadManService(store, &fakeDispatcher{})

	for _, ms := range []int64{-1, 1, 999, 300001, 1 << 40} {
		if _, err := svc.Set(context.Background(), 42, ms, true); err == nil {
			t.Fatalf("countdown_ms=%d accepted, want COUNTDOWN_INVALID_DURATION", ms)
		} else {
			requireCode(t, err, CodeCountdownInvalid)
		}
	}
	ack, err := svc.Set(context.Background(), 42, 30000, true)
	if err != nil {
		t.Fatalf("valid arm rejected: %v", err)
	}
	if ack.CountdownExpiry <= ack.ServerTime {
		t.Fatalf("expiry %d not in the future (server_time %d)", ack.CountdownExpiry, ack.ServerTime)
	}
	if ack.CountdownExpiry-ack.ServerTime < 29000 {
		t.Fatalf("expiry too close: delta=%dms", ack.CountdownExpiry-ack.ServerTime)
	}
}

func TestCountdownStrictStartRejectsActive(t *testing.T) {
	store := newFakeCountdownStore(time.Now)
	svc := NewDeadManService(store, &fakeDispatcher{})
	ctx := context.Background()

	if _, err := svc.Set(ctx, 7, 10000, false); err != nil {
		t.Fatalf("first arm: %v", err)
	}
	if _, err := svc.Set(ctx, 7, 10000, false); err == nil {
		t.Fatal("duplicate strict start must reject")
	} else {
		requireCode(t, err, CodeCountdownAlreadyActive)
	}
	// Heartbeat renewal must succeed.
	if _, err := svc.Set(ctx, 7, 10000, true); err != nil {
		t.Fatalf("renewal must succeed: %v", err)
	}
}

func TestCountdownDisableClearsTimer(t *testing.T) {
	store := newFakeCountdownStore(time.Now)
	svc := NewDeadManService(store, &fakeDispatcher{})
	ctx := context.Background()

	if _, err := svc.Set(ctx, 9, 10000, true); err != nil {
		t.Fatalf("arm: %v", err)
	}
	ack, err := svc.Set(ctx, 9, 0, true) // countdown_ms=0 disables
	if err != nil {
		t.Fatalf("disable: %v", err)
	}
	if ack.CountdownExpiry != 0 {
		t.Fatalf("disabled ack expiry = %d, want 0", ack.CountdownExpiry)
	}
	if _, ok, _ := store.Deadline(ctx, 9); ok {
		t.Fatal("timer still armed after disable")
	}
}

func TestCountdownExpiryTriggersMassCancel(t *testing.T) {
	cur := time.Now()
	store := newFakeCountdownStore(func() time.Time { return cur })
	disp := &fakeDispatcher{}
	svc := NewDeadManService(store, disp)
	svc.now = func() time.Time { return cur }
	ctx := context.Background()

	if _, err := svc.Set(ctx, 55, 5000, true); err != nil {
		t.Fatalf("arm: %v", err)
	}
	// Not yet expired.
	if n, err := svc.SweepOnce(ctx); err != nil || n != 0 {
		t.Fatalf("early sweep cancelled %d (err %v)", n, err)
	}
	if len(disp.cancels) != 0 {
		t.Fatal("cancel fired before expiry")
	}
	// Advance past expiry.
	cur = cur.Add(6 * time.Second)
	if n, err := svc.SweepOnce(ctx); err != nil || n != 1 {
		t.Fatalf("sweep cancelled %d, want 1 (err %v)", n, err)
	}
	if len(disp.cancels) != 1 || disp.cancels[0].AccountID != 55 || disp.cancels[0].Reason != "deadman" {
		t.Fatalf("mass cancel scope wrong: %+v", disp.cancels)
	}
	// Timer consumed — a second sweep fires nothing.
	if n, err := svc.SweepOnce(ctx); err != nil || n != 0 {
		t.Fatalf("second sweep fired %d", n)
	}
}

func TestCountdownCancelFailureReArms(t *testing.T) {
	cur := time.Now()
	store := newFakeCountdownStore(func() time.Time { return cur })
	disp := &fakeDispatcher{cancelErr: errPlain("engine unreachable")}
	svc := NewDeadManService(store, disp)
	svc.now = func() time.Time { return cur }
	ctx := context.Background()

	if _, err := svc.Set(ctx, 66, 2000, true); err != nil {
		t.Fatalf("arm: %v", err)
	}
	cur = cur.Add(3 * time.Second)
	if _, err := svc.SweepOnce(ctx); err == nil {
		t.Fatal("cancel failure must surface (fail-closed)")
	}
	// Re-armed for retry — next sweep fires again.
	disp.cancelErr = nil
	cur = cur.Add(1500 * time.Millisecond)
	if n, err := svc.SweepOnce(ctx); err != nil || n != 1 {
		t.Fatalf("retry sweep cancelled %d, want 1 (err %v)", n, err)
	}
	if len(disp.cancels) != 2 {
		t.Fatalf("cancels=%d, want 2 (initial fail + retry)", len(disp.cancels))
	}
}

type errPlain string

func (e errPlain) Error() string { return string(e) }
