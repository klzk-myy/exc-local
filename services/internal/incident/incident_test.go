package incident

import (
	"context"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"exchange/internal/observability"
)

// fakePager captures raised alerts; fail=true makes every Raise error.
type fakePager struct {
	mu     chan struct{}
	alerts []observability.Alert
	fail   atomic.Bool
}

func newFakePager() *fakePager { return &fakePager{mu: make(chan struct{}, 1)} }

func (f *fakePager) Raise(_ context.Context, a observability.Alert) error {
	f.mu <- struct{}{}
	defer func() { <-f.mu }()
	f.alerts = append(f.alerts, a)
	if f.fail.Load() {
		return fmt.Errorf("fake pager: provider down")
	}
	return nil
}

func (f *fakePager) codes() []string {
	f.mu <- struct{}{}
	defer func() { <-f.mu }()
	out := make([]string, len(f.alerts))
	for i, a := range f.alerts {
		out[i] = a.Code + ":" + a.Status
	}
	return out
}

// clock is a manually-advanced clock for the escalation/cadence tests.
type clock struct{ t time.Time }

func (c *clock) now() time.Time      { return c.t }
func (c *clock) add(d time.Duration) { c.t = c.t.Add(d) }

func newTestManager(t *testing.T) (*Manager, *MemStore, *fakePager, *FileChatOps, *clock) {
	t.Helper()
	clk := &clock{t: time.Date(2026, 9, 27, 12, 0, 0, 0, time.UTC)}
	st := NewMemStore()
	pg := newFakePager()
	chat := NewFileChatOps(t.TempDir())
	m, err := NewManager(Options{Store: st, Pager: pg, Chat: chat, Now: clk.now})
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	return m, st, pg, chat, clk
}

func TestNewManagerFailClosed(t *testing.T) {
	st, pg, chat := NewMemStore(), newFakePager(), NewFileChatOps(t.TempDir())
	cases := []Options{
		{Store: nil, Pager: pg, Chat: chat},
		{Store: st, Pager: nil, Chat: chat},
		{Store: st, Pager: pg, Chat: nil},
	}
	for i, o := range cases {
		if _, err := NewManager(o); err == nil {
			t.Fatalf("case %d: expected construction error on missing seam", i)
		}
	}
}

func TestDeclareP0FullPlaybook(t *testing.T) {
	m, st, pg, chat, _ := newTestManager(t)
	inc, err := m.Declare(context.Background(), Declaration{
		Severity: "P0", Title: "matching engine halted", Summary: "shard 0 stalled",
		By: "oncall-sre", Source: "manual",
	})
	if err != nil {
		t.Fatalf("declare: %v", err)
	}
	if inc.ID != "INC-20260927-0001" {
		t.Fatalf("id = %q", inc.ID)
	}
	if inc.Status != StatusOpen {
		t.Fatalf("status = %q", inc.Status)
	}
	if inc.WarRoom != "inc-20260927-0001" {
		t.Fatalf("war room = %q", inc.WarRoom)
	}
	if inc.Bridge == "" {
		t.Fatal("bridge not provisioned")
	}
	if inc.Commander != "oncall-sre" {
		t.Fatalf("commander = %q", inc.Commander)
	}
	// Page: single firing edge naming every P0 target.
	codes := pg.codes()
	if len(codes) != 1 || codes[0] != "INCIDENT_DECLARED:firing" {
		t.Fatalf("pages = %v", codes)
	}
	msgs, err := chat.Messages(inc.WarRoom)
	if err != nil || len(msgs) != 1 {
		t.Fatalf("room messages = %v err=%v", msgs, err)
	}
	if !strings.Contains(msgs[0], "commander: oncall-sre") {
		t.Fatalf("opening post = %q", msgs[0])
	}
	// Timeline carries declared → war_room → page edges.
	tl, _ := st.Timeline(context.Background(), inc.ID)
	var kinds []string
	for _, e := range tl {
		kinds = append(kinds, e.Kind)
	}
	want := []string{TlDeclared, TlWarRoom, TlWarRoom, TlPage}
	if strings.Join(kinds, ",") != strings.Join(want, ",") {
		t.Fatalf("timeline kinds = %v", kinds)
	}
}

func TestDeclareP3NoWarRoom(t *testing.T) {
	m, _, pg, _, _ := newTestManager(t)
	inc, err := m.Declare(context.Background(), Declaration{
		Severity: "P3", Title: "statement label typo", By: "support",
	})
	if err != nil {
		t.Fatalf("declare: %v", err)
	}
	if inc.WarRoom != "" || inc.Bridge != "" {
		t.Fatalf("P3 must not provision a war room: %+v", inc)
	}
	if got := pg.alerts[0].Details["targets"]; got != "exchange-ops-channel" {
		t.Fatalf("P3 targets = %q", got)
	}
}

func TestDeclareValidation(t *testing.T) {
	m, _, _, _, _ := newTestManager(t)
	if _, err := m.Declare(context.Background(), Declaration{Severity: "p0", Title: "x"}); err == nil {
		t.Fatal("lowercase severity accepted — must fail closed")
	}
	if _, err := m.Declare(context.Background(), Declaration{Severity: "P0"}); err == nil {
		t.Fatal("empty title accepted")
	}
}

func TestEscalationAndCadence(t *testing.T) {
	m, st, pg, _, clk := newTestManager(t)
	inc, _ := m.Declare(context.Background(), Declaration{
		Severity: "P0", Title: "halt", By: "sre"})
	_ = inc

	// 4 minutes: still inside the 5m ack stretch — nothing fires.
	clk.add(4 * time.Minute)
	m.Tick(context.Background())
	if n := len(pg.alerts); n != 1 {
		t.Fatalf("pages before ack timeout = %d, want 1", n)
	}

	// 5m+ unacknowledged → one escalation page, latched.
	clk.add(2 * time.Minute)
	m.Tick(context.Background())
	m.Tick(context.Background()) // idempotent — no second escalation
	var esc *observability.Alert
	for i := range pg.alerts {
		if pg.alerts[i].Code == "INCIDENT_ESCALATED" {
			esc = &pg.alerts[i]
		}
	}
	if esc == nil {
		t.Fatal("no escalation page")
	}
	if esc.Details["escalate_to"] != "secondary-eng-leadership" {
		t.Fatalf("escalate_to = %q", esc.Details["escalate_to"])
	}
	got, _ := st.Get(context.Background(), inc.ID)
	if !got.Escalated {
		t.Fatal("escalated flag not latched")
	}

	// 30m boundary → scheduled stakeholder update lands in the room.
	clk.add(30 * time.Minute)
	m.Tick(context.Background())
	got, _ = st.Get(context.Background(), inc.ID)
	if got.LastUpdateAt == nil {
		t.Fatal("no stakeholder update at 30m cadence")
	}

	// Acknowledgement stops further escalations (already latched, but a
	// fresh incident would escalate once — ack prevents even that).
	if _, err := m.Acknowledge(context.Background(), inc.ID, "core-eng-lead"); err != nil {
		t.Fatalf("ack: %v", err)
	}
	got, _ = st.Get(context.Background(), inc.ID)
	if got.Status != StatusAcknowledged || got.AckedBy != "core-eng-lead" {
		t.Fatalf("ack state = %+v", got)
	}
}

func TestEscalationSuppressedAfterAck(t *testing.T) {
	m, _, pg, _, clk := newTestManager(t)
	inc, _ := m.Declare(context.Background(), Declaration{
		Severity: "P1", Title: "shard down", By: "sre"})
	if _, err := m.Acknowledge(context.Background(), inc.ID, "component-lead"); err != nil {
		t.Fatalf("ack: %v", err)
	}
	clk.add(20 * time.Minute) // past the 15m P1 ack timeout
	m.Tick(context.Background())
	for _, a := range pg.alerts {
		if a.Code == "INCIDENT_ESCALATED" {
			t.Fatal("escalated after acknowledgement")
		}
	}
}

func TestPostMortemSLA(t *testing.T) {
	m, _, pg, _, clk := newTestManager(t)
	inc, _ := m.Declare(context.Background(), Declaration{
		Severity: "P1", Title: "rail down", By: "sre"})

	clk.add(48*time.Hour + time.Minute)
	m.Tick(context.Background())
	m.Tick(context.Background()) // raise-once latch
	var overdue int
	for _, a := range pg.alerts {
		if a.Code == "POSTMORTEM_OVERDUE" {
			overdue++
			if a.Severity != "P2" {
				t.Fatalf("postmortem-overdue severity = %q (want P2)", a.Severity)
			}
		}
	}
	if overdue != 1 {
		t.Fatalf("postmortem-overdue raises = %d, want 1", overdue)
	}
	if _, err := m.RecordPostMortem(context.Background(), inc.ID,
		"docs/incidents/inc-20260927-0001.md"); err != nil {
		t.Fatalf("record postmortem: %v", err)
	}
	got, _ := m.Get(context.Background(), inc.ID)
	if got.PostMortemURL == "" {
		t.Fatal("postmortem not recorded")
	}
}

func TestResolveSelfClearsPage(t *testing.T) {
	m, _, pg, chat, clk := newTestManager(t)
	inc, _ := m.Declare(context.Background(), Declaration{
		Severity: "P0", Title: "halt", By: "sre"})
	if _, err := m.Resolve(context.Background(), inc.ID, "engine restarted"); err != nil {
		t.Fatalf("resolve: %v", err)
	}
	codes := pg.codes()
	last := codes[len(codes)-1]
	if last != "INCIDENT_RESOLVED:resolved" {
		t.Fatalf("last page = %q", last)
	}
	msgs, _ := chat.Messages(inc.WarRoom)
	if !strings.Contains(msgs[len(msgs)-1], "RESOLVED") {
		t.Fatalf("room close-out missing: %v", msgs)
	}
	// Resolved incidents stop cadence updates but keep the PM clock.
	clk.add(40 * time.Minute)
	m.Tick(context.Background())
	got, _ := m.Get(context.Background(), inc.ID)
	if got.LastUpdateAt != nil {
		t.Fatal("post-resolution update posted")
	}
}

func TestPagerFailureIsSurfacedNotFabricated(t *testing.T) {
	m, st, pg, _, _ := newTestManager(t)
	pg.fail.Store(true)
	inc, err := m.Declare(context.Background(), Declaration{
		Severity: "P0", Title: "halt", By: "sre"})
	if err == nil {
		t.Fatal("pager failure must surface")
	}
	if inc == nil {
		t.Fatal("incident record must survive the page failure")
	}
	tl, _ := st.Timeline(context.Background(), inc.ID)
	var sawFail bool
	for _, e := range tl {
		if e.Kind == TlPageFailed {
			sawFail = true
		}
	}
	if !sawFail {
		t.Fatal("page failure not journaled")
	}
}

func TestHandleAlertAutoLifecycle(t *testing.T) {
	m, _, _, _, _ := newTestManager(t)
	a := observability.Alert{
		Rule: "AeronDriverDown", Severity: "P1", Code: "AERON_DOWN",
		Summary: "driver down on shard 0", Status: "firing",
	}
	inc, err := m.HandleAlert(context.Background(), a)
	if err != nil {
		t.Fatalf("handle alert: %v", err)
	}
	if inc.Source != "AeronDriverDown" || inc.WarRoom == "" {
		t.Fatalf("auto-declared incident = %+v", inc)
	}
	// Re-fire dedupes to the same incident.
	inc2, err := m.HandleAlert(context.Background(), a)
	if err != nil || inc2.ID != inc.ID {
		t.Fatalf("dedupe: %+v err=%v", inc2, err)
	}
	// Resolve edge closes it.
	a.Status = "resolved"
	res, err := m.HandleAlert(context.Background(), a)
	if err != nil || res.Status != StatusResolved {
		t.Fatalf("auto-resolve: %+v err=%v", res, err)
	}
}

func TestFileStoreRoundTrip(t *testing.T) {
	dir := t.TempDir()
	st1, err := NewFileStore(dir)
	if err != nil {
		t.Fatalf("filestore: %v", err)
	}
	clk := &clock{t: time.Date(2026, 9, 27, 12, 0, 0, 0, time.UTC)}
	m, err := NewManager(Options{
		Store: st1, Pager: newFakePager(),
		Chat: NewFileChatOps(t.TempDir()), Now: clk.now,
	})
	if err != nil {
		t.Fatalf("manager: %v", err)
	}
	inc, err := m.Declare(context.Background(), Declaration{
		Severity: "P0", Title: "wal corrupt", By: "sre"})
	if err != nil {
		t.Fatalf("declare: %v", err)
	}
	if _, err := m.Acknowledge(context.Background(), inc.ID, "leo"); err != nil {
		t.Fatalf("ack: %v", err)
	}
	// "Restart": a fresh FileStore over the same dir sees everything —
	// the ledger is durable without external accounts.
	st2, _ := NewFileStore(dir)
	got, err := st2.Get(context.Background(), inc.ID)
	if err != nil {
		t.Fatalf("reopen get: %v", err)
	}
	if got.Status != StatusAcknowledged || got.AckedBy != "leo" {
		t.Fatalf("persisted record = %+v", got)
	}
	tl, err := st2.Timeline(context.Background(), inc.ID)
	if err != nil || len(tl) < 4 {
		t.Fatalf("persisted timeline = %v err=%v", tl, err)
	}
	list, _ := st2.List(context.Background())
	if len(list) != 1 || list[0].ID != inc.ID {
		t.Fatalf("list = %v", list)
	}
}
