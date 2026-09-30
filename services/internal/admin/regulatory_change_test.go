// Phase-21 Task 21.3.25 — regulatory-change policy pure tests (no PG):
// business-day triage SLA, §8.2 role matrix, auditor scope projection.
package admin

import (
	"context"
	"testing"
	"time"
)

// Publication day is day 0; the 10-business-day SLA skips weekends
// and injected holiday dates.
func TestBusinessDaysAfter(t *testing.T) {
	// Wed 2026-01-07 +1 → Thu 01-08.
	if got := BusinessDaysAfter(
		time.Date(2026, 1, 7, 0, 0, 0, 0, time.UTC), 1, nil); got.Day() != 8 ||
		got.Weekday() != time.Thursday {
		t.Fatalf("+1 from Wed = %s want Thu 8", got)
	}
	// Fri 2026-01-09 +1 → Mon 01-12 (weekend skipped).
	if got := BusinessDaysAfter(
		time.Date(2026, 1, 9, 0, 0, 0, 0, time.UTC), 1, nil); got.Day() != 12 ||
		got.Weekday() != time.Monday {
		t.Fatalf("+1 from Fri = %s want Mon 12", got)
	}
	// Holiday shifts: Wed +1 with Thu a holiday → Fri.
	hols := map[time.Time]bool{
		time.Date(2026, 1, 8, 0, 0, 0, 0, time.UTC): true,
	}
	if got := BusinessDaysAfter(
		time.Date(2026, 1, 7, 0, 0, 0, 0, time.UTC), 1, hols); got.Day() != 9 {
		t.Fatalf("+1 across holiday = %s want Fri 9", got)
	}
	// Full SLA: Mon 2026-01-05 +10 business days → Mon 01-19.
	got := RegChangeTriageDeadline(
		time.Date(2026, 1, 5, 0, 0, 0, 0, time.UTC), nil)
	if got.Weekday() != time.Monday || got.Day() != 19 {
		t.Fatalf("triage deadline=%s want Mon 2026-01-19", got)
	}
	// 10 days over a weekend-heavy fortnight still lands a weekday.
	got = BusinessDaysAfter(
		time.Date(2026, 1, 9, 0, 0, 0, 0, time.UTC), 10, nil)
	if got.Weekday() == time.Saturday || got.Weekday() == time.Sunday {
		t.Fatalf("deadline on weekend: %s", got)
	}
	if got.Day() != 23 || got.Weekday() != time.Friday {
		t.Fatalf("Fri +10 = %s want Fri 23", got)
	}
}

// §14.10.2 item 6: the auditor projection admits untriaged rows and
// anything effective inside 90 days; closed/implemented work is hidden.
func TestRegChangeAuditorVisible(t *testing.T) {
	now := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	near := now.Add(30 * 24 * time.Hour)
	far := now.Add(200 * 24 * time.Hour)
	cases := []struct {
		name      string
		status    string
		effective *time.Time
		want      bool
	}{
		{"tracked no date", "TRACKED", nil, true},
		{"tracked far date", "TRACKED", &far, true},
		{"triaged near", "TRIAGED", &near, true},
		{"scoped near", "SCOPED", &near, true},
		{"triaged far", "TRIAGED", &far, false},
		{"implemented near", "IMPLEMENTED", &near, false},
		{"closed", "CLOSED", &near, false},
	}
	for _, c := range cases {
		got := RegChangeAuditorVisible(c.status, now, c.effective, now)
		if got != c.want {
			t.Fatalf("%s: visible=%v want %v", c.name, got, c.want)
		}
	}
}

// §8.2 mutation matrix: Compliance Officer + Super Admin only; every
// other binding (and every lookup failure) fails closed.
func TestGateRegChangeMutation(t *testing.T) {
	resolver := mapResolver(map[int64]string{
		1: RoleComplianceOfficer, 2: RoleSuperAdmin,
		3: RoleRiskManager, 4: RoleReadOnlyAuditor,
	})
	for _, id := range []int64{1, 2} {
		if err := GateRegChangeMutation(context.Background(),
			resolver, id); err != nil {
			t.Fatalf("admin %d must mutate: %v", id, err)
		}
	}
	for _, id := range []int64{3, 4} {
		err := GateRegChangeMutation(context.Background(), resolver, id)
		if err == nil {
			t.Fatalf("admin %d must be refused", id)
		}
	}
	if err := GateRegChangeMutation(context.Background(),
		resolver, 99); err == nil {
		t.Fatal("unbound admin must fail closed")
	}
	if err := GateRegChangeMutation(context.Background(), nil, 1); err == nil {
		t.Fatal("nil resolver must fail closed")
	}
}

// Read matrix additionally admits the auditor — and only the auditor
// is confined to the scoped projection.
func TestGateRegChangeRead(t *testing.T) {
	resolver := mapResolver(map[int64]string{
		1: RoleComplianceOfficer, 2: RoleReadOnlyAuditor,
		3: RoleFinanceOps,
	})
	for _, id := range []int64{1, 2} {
		role, err := GateRegChangeRead(context.Background(), resolver, id)
		if err != nil {
			t.Fatalf("admin %d must read: %v", id, err)
		}
		if wantScope := id == 2; RegChangeAuditorScope(role) != wantScope {
			t.Fatalf("admin %d auditor scope=%v want %v",
				id, !wantScope, wantScope)
		}
	}
	if _, err := GateRegChangeRead(context.Background(),
		resolver, 3); err == nil {
		t.Fatal("Finance Ops must be refused register reads")
	}
}
