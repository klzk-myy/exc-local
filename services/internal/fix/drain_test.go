// Drain-half of Task 18.3.17 — maintenance drain semantics: orderly
// Logout(35=5 "Scheduled maintenance") fan-out, News(35=B) advisory,
// ≤10s bounded peer-Logout wait, forced close on stragglers, and new-
// Logon refusal while draining (§24 #289; Task 9.3.23 item 3).
package fix

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/quickfixgo/quickfix"
)

// fakeDrainSession records the outbound stream and controls when its
// peer-Logout channel closes.
type fakeDrainSession struct {
	id      string
	mu      sync.Mutex
	sent    []*quickfix.Message
	done    chan struct{}
	closed  bool
	sendErr error
}

func newFakeDrainSession(id string) *fakeDrainSession {
	return &fakeDrainSession{id: id, done: make(chan struct{})}
}

func (f *fakeDrainSession) ID() string { return f.id }

func (f *fakeDrainSession) Send(_ context.Context, m *quickfix.Message) error {
	if f.sendErr != nil {
		return f.sendErr
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sent = append(f.sent, m)
	return nil
}

func (f *fakeDrainSession) LogoutDone() <-chan struct{} { return f.done }

func (f *fakeDrainSession) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closed = true
	select {
	case <-f.done:
	default:
		close(f.done)
	}
	return nil
}

func (f *fakeDrainSession) types(t *testing.T) []string {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, m := range f.sent {
		mt, err := m.MsgType()
		if err != nil {
			t.Fatalf("sent msgtype: %v", err)
		}
		out = append(out, mt)
	}
	return out
}

type fakeDrainRegistry struct{ sessions []DrainSession }

func (r fakeDrainRegistry) ActiveSessions() []DrainSession { return r.sessions }

func TestDrainRefusesNewLogons(t *testing.T) {
	d := NewDrainer(fakeDrainRegistry{}, DrainConfig{}, nil, nil)
	if err := d.AdmitLogon(); err != nil {
		t.Fatalf("logon before drain: %v", err)
	}
	if err := d.Drain(context.Background()); err != nil {
		t.Fatalf("drain: %v", err)
	}
	if err := d.AdmitLogon(); !errors.Is(err, ErrDraining) {
		t.Fatalf("logon during drain: err=%v, want ErrDraining", err)
	}
	// The on-wire refusal is Logout(35=5) with session-not-available text.
	m := d.LogonRefusal()
	if mt, _ := m.MsgType(); mt != MsgLogout {
		t.Fatal("logon refusal is not 35=5")
	}
	if txt, _ := m.Body.GetString(TagText); !strings.Contains(txt, "SESSION_NOT_AVAILABLE") {
		t.Fatalf("refusal text = %q", txt)
	}
}

func TestDrainSendsNewsThenLogoutAndWaits(t *testing.T) {
	s1 := newFakeDrainSession("FIX.4.4:VENUE->A")
	s2 := newFakeDrainSession("FIX.4.4:VENUE->B")
	reg := fakeDrainRegistry{sessions: []DrainSession{s1, s2}}
	d := NewDrainer(reg, DrainConfig{
		LogoutWait:          500 * time.Millisecond,
		ReplacementEndpoint: "fix2.venue:9800",
	}, nil, nil)

	// Peers answer Logout quickly — close the done channels after 20ms.
	go func() {
		time.Sleep(20 * time.Millisecond)
		_ = s1.Close()
		_ = s2.Close()
	}()
	start := time.Now()
	if err := d.Drain(context.Background()); err != nil {
		t.Fatalf("drain: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 400*time.Millisecond {
		t.Fatalf("drain took %v — should return as soon as peers logout", elapsed)
	}
	for _, s := range []*fakeDrainSession{s1, s2} {
		types := s.types(t)
		if len(types) != 2 || types[0] != MsgNews || types[1] != MsgLogout {
			t.Fatalf("%s outbound = %v, want [B, 5]", s.id, types)
		}
		s.mu.Lock()
		headline, _ := s.sent[0].Body.GetString(TagHeadline)
		s.mu.Unlock()
		if !strings.Contains(headline, "fix2.venue:9800") {
			t.Fatalf("news headline missing replacement endpoint: %q", headline)
		}
		s.mu.Lock()
		ltxt, _ := s.sent[1].Body.GetString(TagText)
		s.mu.Unlock()
		if ltxt != "Scheduled maintenance" {
			t.Fatalf("logout text = %q", ltxt)
		}
	}
}

func TestDrainForceClosesStraggler(t *testing.T) {
	slow := newFakeDrainSession("FIX.4.4:VENUE->SLOW") // done never closes by itself
	reg := fakeDrainRegistry{sessions: []DrainSession{slow}}
	d := NewDrainer(reg, DrainConfig{LogoutWait: 60 * time.Millisecond}, nil, nil)

	start := time.Now()
	if err := d.Drain(context.Background()); err != nil {
		t.Fatalf("drain: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 500*time.Millisecond {
		t.Fatalf("drain overran wait: %v", elapsed)
	}
	slow.mu.Lock()
	defer slow.mu.Unlock()
	if !slow.closed {
		t.Fatal("straggler was not force-closed after the wait")
	}
}

func TestDrainNilRegistryIsNoop(t *testing.T) {
	d := NewDrainer(nil, DrainConfig{}, nil, nil)
	if err := d.Drain(context.Background()); err != nil {
		t.Fatalf("nil-registry drain: %v", err)
	}
	if !d.Draining() {
		t.Fatal("draining flag not latched")
	}
	// Idempotent — second drain returns immediately.
	if err := d.Drain(context.Background()); err != nil {
		t.Fatalf("second drain: %v", err)
	}
}

func TestDrainContextAbort(t *testing.T) {
	slow := newFakeDrainSession("FIX.4.4:VENUE->SLOW")
	reg := fakeDrainRegistry{sessions: []DrainSession{slow}}
	d := NewDrainer(reg, DrainConfig{LogoutWait: 10 * time.Second}, nil, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	err := d.Drain(ctx)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("drain err = %v, want context deadline", err)
	}
}
