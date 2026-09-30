package compliance

// Phase-21 Task 21.3.23 — provider-outage quarantine, pending-screen
// queue semantics (FIFO + dedupe + inflight recovery) and ordered
// replay; plus Task 21.3.23's ARM/APA resubmission repair sweep.

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestProviderGateTransitionAndRecoverHook(t *testing.T) {
	gate := NewProviderGate([]string{"vendorA", "vendorB"})
	ctx := context.Background()
	var recovered int
	gate.OnRecover(func(context.Context) { recovered++ })
	// One provider down out of two → still healthy.
	gate.ReportResult(ctx, "vendorA", errors.New("timeout"))
	if gate.Quarantined() {
		t.Fatal("one of two providers down must not quarantine")
	}
	// All down → quarantined.
	gate.ReportResult(ctx, "vendorB", errors.New("refused"))
	if !gate.Quarantined() {
		t.Fatal("all providers down must quarantine")
	}
	if err := gate.Check(); err == nil {
		t.Fatal("quarantined gate must fail Check")
	}
	// A single success recovers + fires the hook once.
	gate.ReportResult(ctx, "vendorA", nil)
	if gate.Quarantined() || recovered != 1 {
		t.Fatalf("recovery: quarantined=%v hooks=%d", gate.Quarantined(), recovered)
	}
	// Timeout observation alone quarantines regardless of others.
	gate.ReportTimeout(ctx, "vendorA")
	if !gate.Quarantined() {
		t.Fatal("provider timeout must quarantine")
	}
}

func TestMemoryScreenQueueFIFODedupeRecovery(t *testing.T) {
	q := NewMemoryScreenQueue()
	ctx := context.Background()
	a := PendingScreen{AccountID: 1, Flow: ScreenFlowWithdrawal,
		Candidates: []string{"A"}}
	b := PendingScreen{AccountID: 2, Flow: ScreenFlowWithdrawal,
		Candidates: []string{"B"}}
	idA, _ := q.Enqueue(ctx, a)
	if _, err := q.Enqueue(ctx, b); err != nil {
		t.Fatal(err)
	}
	// Duplicate (flow, account) collapses — returns the existing id.
	dup, err := q.Enqueue(ctx, a)
	if err != nil || dup != idA {
		t.Fatalf("dedupe: id=%d err=%v", dup, err)
	}
	if d, _ := q.Depth(ctx); d != 2 {
		t.Fatalf("depth: %d", d)
	}
	// Claim is FIFO.
	items, err := q.Claim(ctx, 1)
	if err != nil || len(items) != 1 || items[0].AccountID != 1 {
		t.Fatalf("claim: %+v err=%v", items, err)
	}
	// Inflight items return to pending on recovery — nothing is lost.
	if n, _ := q.RecoverInflight(ctx); n != 1 {
		t.Fatalf("recover: %d", n)
	}
	items, _ = q.Claim(ctx, 10)
	if len(items) != 2 || items[0].AccountID != 1 || items[1].AccountID != 2 {
		t.Fatalf("fifo after recovery: %+v", items)
	}
	if err := q.Ack(ctx, items...); err != nil {
		t.Fatal(err)
	}
	if d, _ := q.Depth(ctx); d != 0 {
		t.Fatalf("depth after ack: %d", d)
	}
	// Acked dedupe key releases — a new obligation enqueues fresh.
	if id2, _ := q.Enqueue(ctx, a); id2 == 0 || id2 == idA {
		t.Fatalf("post-ack re-enqueue id=%d", id2)
	}
}

func TestQuarantinedScreenerEnqueuesAndDelegates(t *testing.T) {
	s := screenerWith(t, map[string]string{"dev.txt": "Blocked Beneficiary Trading\n"})
	gate := NewProviderGate([]string{"vendorA"})
	queue := NewMemoryScreenQueue()
	qs := NewQuarantinedScreener(s, gate, queue)
	ctx := context.Background()
	// Healthy → delegates to the real screener (hit passes through).
	hit, err := qs.ScreenWithdrawal(ctx, 7, "Blocked Beneficiary Trading", "")
	if err != nil || !hit {
		t.Fatalf("healthy delegate: hit=%v err=%v", hit, err)
	}
	// Quarantined → ErrQuarantined + obligation queued.
	gate.ReportResult(ctx, "vendorA", errors.New("down"))
	hit, err = qs.ScreenWithdrawal(ctx, 7, "Blocked Beneficiary Trading", "")
	if !errors.Is(err, ErrQuarantined) && err == nil {
		t.Fatal("quarantined screen did not fail closed")
	}
	if hit {
		t.Fatal("quarantined screen returned a pass")
	}
	if d, _ := queue.Depth(ctx); d != 1 {
		t.Fatalf("queue depth: %d", d)
	}
}

func TestQueueReplayerDrain(t *testing.T) {
	s := screenerWith(t, map[string]string{"dev.txt": "Blocked Beneficiary Trading\n"})
	queue := NewMemoryScreenQueue()
	ctx := context.Background()
	// One clean + one hitting obligation.
	_, _ = queue.Enqueue(ctx, PendingScreen{AccountID: 1,
		Flow: ScreenFlowDeposit, Candidates: []string{"Clean Alice"}})
	_, _ = queue.Enqueue(ctx, PendingScreen{AccountID: 2,
		Flow: ScreenFlowWithdrawal, Candidates: []string{"Blocked Beneficiary Trading"}})
	var hitAccounts []int64
	rep := NewQueueReplayer(queue, s).
		WithOnHit(func(_ context.Context, p PendingScreen, _ []MatchHit) error {
			hitAccounts = append(hitAccounts, p.AccountID)
			return nil
		})
	report, err := rep.Replay(ctx)
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	if report.Claimed != 2 || report.HitCount != 1 || report.Rescreened != 1 {
		t.Fatalf("report: %+v", report)
	}
	if len(hitAccounts) != 1 || hitAccounts[0] != 2 {
		t.Fatalf("onHit accounts: %v", hitAccounts)
	}
	if d, _ := queue.Depth(ctx); d != 0 {
		t.Fatalf("queue not drained: %d", d)
	}
}

// TestQueueReplayerFailClosed — a screener error mid-drain leaves the
// item un-acked (still claimable on the next pass).
func TestQueueReplayerFailClosed(t *testing.T) {
	dir := fixtureDir(t, map[string]string{"dev.txt": "Blocked Beneficiary Trading\n"})
	s, err := NewListScreener(dir)
	if err != nil {
		t.Fatal(err)
	}
	queue := NewMemoryScreenQueue()
	ctx := context.Background()
	_, _ = queue.Enqueue(ctx, PendingScreen{AccountID: 3,
		Flow: ScreenFlowDeposit, Candidates: []string{"Whoever"}})
	// Corrupt the list mid-flight: zero-usable-names reload fails, but
	// simulate a screener that errors on screen by quarantining a gate.
	gate := NewProviderGate([]string{"vendorA"})
	gate.ReportResult(ctx, "vendorA", errors.New("down"))
	s.WithProviderGate(gate)
	rep := NewQueueReplayer(queue, s)
	report, rerr := rep.Replay(ctx)
	if rerr == nil {
		t.Fatal("replay must fail while the screener is down")
	}
	// The item stays owed work — claimed (inflight) + recoverable, not acked.
	if report.Backlog == 0 {
		t.Fatalf("backlog lost: %+v", report)
	}
	if n, _ := queue.RecoverInflight(ctx); n != 1 {
		t.Fatalf("inflight recovery: %d", n)
	}
}

// ---------------------------------------------------------------------------
// Resubmission repair (Task 21.3.23 ARM/APA leg)
// ---------------------------------------------------------------------------

type fakeResubmitter struct {
	fail  error
	calls int
	ref   string
}

func (f *fakeResubmitter) Submit(_ context.Context,
	_ RepairSubmission) (string, error) {
	f.calls++
	if f.fail != nil {
		return "", f.fail
	}
	return f.ref, nil
}

func TestResubmissionSweep(t *testing.T) {
	store := NewMemorySubmissionRepairStore()
	ctx := context.Background()
	now := time.Now().UTC()
	okID := store.Insert(RepairSubmission{
		RegulationType: "MIFID2_RTS1", DestinationType: "APA",
		AckStatus: "NACK", CreatedAt: now.Add(-time.Hour)})
	failID := store.Insert(RepairSubmission{
		RegulationType: "MIFID2_RTS22", DestinationType: "ARM",
		AckStatus: "PENDING", CreatedAt: now.Add(-time.Hour)})
	oldID := store.Insert(RepairSubmission{
		RegulationType: "MIFID2_RTS1", DestinationType: "APA",
		AckStatus: "NACK", CreatedAt: now.Add(-3 * time.Hour)}) // past deadline

	sender := &fakeResubmitter{ref: "VENDOR-42"}
	svc := NewResubmissionService(store, sender)
	rep, err := svc.SweepOnce(ctx)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if rep.Claimed != 3 {
		t.Fatalf("claimed: %+v", rep)
	}
	r1, _ := store.GetRow(okID)
	r2, _ := store.GetRow(failID)
	r3, _ := store.GetRow(oldID)
	if r1.AckStatus != "ACK" || r2.AckStatus != "ACK" {
		t.Fatalf("fresh rows not ACKed: %+v / %+v", r1, r2)
	}
	// The 3h-old row succeeded on the wire but is past the 2h
	// resubmission deadline — the task wants it FAILED + escalated,
	// not ACKed.
	if r3.AckStatus != "FAILED" {
		t.Fatalf("deadline row not failed: %+v", r3)
	}
	if rep.Deadlined != 1 {
		t.Fatalf("deadlined count: %+v", rep)
	}
}

func TestResubmissionBackoffAndDeadline(t *testing.T) {
	store := NewMemorySubmissionRepairStore()
	ctx := context.Background()
	id := store.Insert(RepairSubmission{
		RegulationType: "MIFID2_RTS22", DestinationType: "ARM",
		AckStatus: "PENDING", CreatedAt: time.Now().UTC()})
	sender := &fakeResubmitter{fail: errors.New("connection refused")}
	svc := NewResubmissionService(store, sender)
	rep, err := svc.SweepOnce(ctx)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if rep.Retried != 1 {
		t.Fatalf("retried: %+v", rep)
	}
	row, _ := store.GetRow(id)
	if row.AckStatus != "PENDING" || row.NextAttemptAt == nil ||
		row.RepairAttempts != 1 {
		t.Fatalf("retry row: %+v", row)
	}
}
