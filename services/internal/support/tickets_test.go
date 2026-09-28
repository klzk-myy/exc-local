// Unit tests — Phase-07 Task 7.3.7 enums, state machine, SLA clocks.
package support

import (
	"testing"
	"time"
)

func TestValidCategory(t *testing.T) {
	for _, c := range []string{"FUNDING", "TRADING", "KYC", "TECHNICAL", "COMPLAINT"} {
		if !ValidCategory(c) {
			t.Errorf("category %s must be valid", c)
		}
	}
	for _, c := range []string{"", "GENERAL", "complaint", "BILLING"} {
		if ValidCategory(c) {
			t.Errorf("category %q must be rejected", c)
		}
	}
}

func TestValidStatusAndTransitions(t *testing.T) {
	for _, s := range []string{"OPEN", "PENDING", "IN_PROGRESS", "RESOLVED", "CLOSED"} {
		if !ValidStatus(s) {
			t.Errorf("status %s must be valid", s)
		}
	}
	// Canonical moves.
	for _, tr := range [][2]string{
		{"OPEN", "PENDING"}, {"OPEN", "IN_PROGRESS"}, {"OPEN", "RESOLVED"}, {"OPEN", "CLOSED"},
		{"PENDING", "IN_PROGRESS"}, {"PENDING", "RESOLVED"}, {"PENDING", "CLOSED"},
		{"IN_PROGRESS", "PENDING"}, {"IN_PROGRESS", "RESOLVED"}, {"IN_PROGRESS", "CLOSED"},
		{"RESOLVED", "IN_PROGRESS"}, {"RESOLVED", "CLOSED"}, // reopen path
	} {
		if !ValidTransition(tr[0], tr[1]) {
			t.Errorf("transition %s→%s must be allowed", tr[0], tr[1])
		}
	}
	// Forbidden moves: CLOSED is terminal; no backwards jumps to OPEN.
	for _, tr := range [][2]string{
		{"CLOSED", "OPEN"}, {"CLOSED", "IN_PROGRESS"}, {"CLOSED", "RESOLVED"},
		{"PENDING", "OPEN"}, {"IN_PROGRESS", "OPEN"}, {"RESOLVED", "OPEN"},
		{"OPEN", "OPEN"},
	} {
		if ValidTransition(tr[0], tr[1]) {
			t.Errorf("transition %s→%s must be rejected", tr[0], tr[1])
		}
	}
}

func TestComplaintRouting(t *testing.T) {
	if got := typeFor(CategoryComplaint); got != TypeComplaint {
		t.Fatalf("COMPLAINT category must produce type COMPLAINT, got %s", got)
	}
	for _, c := range []string{CategoryFunding, CategoryTrading, CategoryKYC, CategoryTechnical} {
		if got := typeFor(c); got != TypeSupport {
			t.Errorf("%s must map to SUPPORT, got %s", c, got)
		}
	}
	if got := queueFor(TypeComplaint); got != QueueCompliance {
		t.Fatalf("complaints must route to the COMPLIANCE queue, got %s", got)
	}
	if got := queueFor(TypeDispute); got != QueueCompliance {
		t.Fatalf("disputes must route to the COMPLIANCE queue, got %s", got)
	}
	if got := queueFor(TypeSupport); got != QueueSupport {
		t.Fatalf("support tickets must route to the SUPPORT queue, got %s", got)
	}
	// Queue→role confinement (MiFID complaint-handling segregation):
	// Support Agent must not touch the compliance queue.
	if rolesForQueue[QueueCompliance]["Support Agent"] {
		t.Fatal("Support Agent must not be eligible for the compliance queue")
	}
	if !rolesForQueue[QueueCompliance]["Compliance Officer"] {
		t.Fatal("Compliance Officer must own the compliance queue")
	}
	if !rolesForQueue[QueueSupport]["Support Agent"] {
		t.Fatal("Support Agent must own the support queue")
	}
}

func TestComplaintAckSLA_StatutoryBoundWinsOnWeekend(t *testing.T) {
	// Friday 21:00 UTC — weekend between submission and the 8-business-
	// hour internal clock. The statutory 48h wall-clock deadline must cap
	// sla_due_at (§27.1: "respect the relevant statutory acknowledgment
	// deadline").
	fri := time.Date(2026, 9, 25, 21, 0, 0, 0, time.UTC) // a Friday
	got := slaFor(TypeComplaint, fri)
	statutory := fri.Add(ComplaintStatutoryAckSLA) // Sunday 21:00
	if !got.Equal(statutory) {
		t.Fatalf("complaint SLA on Friday evening must be the statutory 48h bound %s, got %s", statutory, got)
	}
	// Internal business-hour clock would land Monday — verify the cap
	// actually did something.
	if internal := businessHours(fri, ComplaintAckSLA); !internal.After(got) {
		t.Fatalf("expected internal 8bh clock (%s) to exceed the statutory bound (%s)", internal, got)
	}
}

func TestComplaintAckSLA_InternalClockWinsMidweek(t *testing.T) {
	mon := time.Date(2026, 9, 28, 10, 0, 0, 0, time.UTC) // a Monday
	got := slaFor(TypeComplaint, mon)
	want := mon.Add(8 * time.Hour) // 8 business hours land same-day
	if !got.Equal(want) {
		t.Fatalf("midweek complaint ack SLA want %s, got %s", want, got)
	}
}

func TestSupportSLA_IsPlain24h(t *testing.T) {
	mon := time.Date(2026, 9, 28, 10, 0, 0, 0, time.UTC)
	if got, want := slaFor(TypeSupport, mon), mon.Add(24*time.Hour); !got.Equal(want) {
		t.Fatalf("support SLA want %s, got %s", want, got)
	}
}

func TestFinalResponseDue(t *testing.T) {
	mon := time.Date(2026, 9, 28, 10, 0, 0, 0, time.UTC)
	d := finalResponseDue(TypeComplaint, mon)
	if d == nil || !d.Equal(mon.Add(ComplaintFinalResponseSLA)) {
		t.Fatalf("complaint final-response deadline want +8 weeks, got %v", d)
	}
	if finalResponseDue(TypeSupport, mon) != nil {
		t.Fatal("support tickets must not carry a statutory final-response deadline")
	}
}

func TestBusinessHours_SkipsWeekend(t *testing.T) {
	// Friday 20:00 + 8 business hours: Fri 21,22,23 (3h) + Mon 00..04
	// (5h) → Monday 04:00.
	fri := time.Date(2026, 9, 25, 20, 0, 0, 0, time.UTC)
	want := time.Date(2026, 9, 28, 4, 0, 0, 0, time.UTC)
	if got := businessHours(fri, 8*time.Hour); !got.Equal(want) {
		t.Fatalf("businessHours: want %s, got %s", want, got)
	}
}

func TestTicketMarkBreach(t *testing.T) {
	now := time.Now().UTC()
	tick := &Ticket{
		Status:   StatusOpen,
		SLADueAt: now.Add(-time.Hour),
	}
	tick.markBreach(now)
	if !tick.SLABreached {
		t.Fatal("overdue unacknowledged open ticket must flag sla_breached")
	}
	// Acknowledged clears the ack breach.
	tick.AcknowledgedAt = &now
	tick.markBreach(now)
	if tick.SLABreached {
		t.Fatal("acknowledged ticket must not flag sla_breached")
	}
	// Resolved tickets never flag.
	tick2 := &Ticket{Status: StatusResolved, SLADueAt: now.Add(-time.Hour)}
	tick2.markBreach(now)
	if tick2.SLABreached || tick2.FinalSLABreached {
		t.Fatal("resolved ticket must not flag breaches")
	}
	// Final-response clock.
	fin := now.Add(-time.Hour)
	tick3 := &Ticket{Status: StatusInProgress, SLADueAt: now.Add(time.Hour),
		AcknowledgedAt: &now, FinalResponseDueAt: &fin}
	tick3.markBreach(now)
	if !tick3.FinalSLABreached || tick3.SLABreached {
		t.Fatal("acknowledged complaint past final deadline must flag final_sla_breached only")
	}
}
