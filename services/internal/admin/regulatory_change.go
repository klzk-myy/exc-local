// Regulatory change governance policy — Phase-21 Task 21.3.25
// admin-side surface (spec §14.10.2, §24 #328). The register's domain
// store, lifecycle and sweeps live in compliance/regulatory_change.go;
// this file owns the §8.2 governance the register defers to — kept in
// the admin package because compliance→admin is the allowed dependency
// direction (quarantine/lifecycle_store already ride it):
//
//   - RegChangeMutationRoles / RegChangeReaderRoles — the role matrix:
//     Compliance Officer + Super Admin mutate; the Read-Only Auditor
//     additionally reads but only inside the scoped projection the
//     spec exposes (untriaged + near-deadline rows).
//   - GateRegChangeMutation / GateRegChangeRead — the resolver-backed
//     gates; the domain service calls them on every entry point so a
//     route-registry slip still fails closed.
//   - RegChangeAuditorScope / RegChangeAuditorVisible — the auditor
//     projection rule over primitives: TRACKED rows and rows effective
//     inside the 90-day window only.
//   - RegChangeTriageDeadline / BusinessDaysAfter — the 10-business-day
//     triage SLA calendar (weekday counting, holiday-set injectable).
//
// Statuses/kinds stay in the compliance package (they mirror the
// migration-080 CHECKs — data semantics); this file carries the policy
// that *uses* them.
package admin

import (
	"context"
	"time"

	excerrors "exchange/pkg/errors"
)

// RegChangeMutationRoles is the §8.2 mutation matrix for the register.
var RegChangeMutationRoles = map[string]bool{
	RoleComplianceOfficer: true,
	RoleSuperAdmin:        true,
}

// RegChangeReaderRoles additionally admits the auditor — scoped by
// RegChangeAuditorVisible, never the full register.
var RegChangeReaderRoles = map[string]bool{
	RoleComplianceOfficer: true,
	RoleSuperAdmin:        true,
	RoleReadOnlyAuditor:   true,
}

// RegChangeTriageSLA is the spec-pinned triage deadline in business days.
const RegChangeTriageSLA = 10

// RegChangeEffectiveWindow is the spec-pinned effective-date alert
// window — a live change effective inside it pages the officer.
const RegChangeEffectiveWindow = 90 * 24 * time.Hour

// GateRegChangeMutation enforces the officer mutation matrix.
func GateRegChangeMutation(ctx context.Context,
	resolver AdminRoleResolver, actor int64) error {
	if resolver == nil {
		return excerrors.New("INTERNAL_ERROR", "role resolver not wired")
	}
	role, err := resolver(ctx, actor)
	if err != nil {
		return excerrors.New("INTERNAL_ERROR", "role lookup: "+err.Error())
	}
	if !RegChangeMutationRoles[role] {
		return excerrors.New("UNAUTHORIZED_ROLE",
			"role "+role+" cannot mutate the regulatory watch register")
	}
	return nil
}

// GateRegChangeRead enforces the reader matrix and returns the role —
// callers branch the auditor projection on RegChangeAuditorScope.
func GateRegChangeRead(ctx context.Context,
	resolver AdminRoleResolver, actor int64) (string, error) {
	if resolver == nil {
		return "", excerrors.New("INTERNAL_ERROR", "role resolver not wired")
	}
	role, err := resolver(ctx, actor)
	if err != nil {
		return "", excerrors.New("INTERNAL_ERROR", "role lookup: "+err.Error())
	}
	if !RegChangeReaderRoles[role] {
		return "", excerrors.New("UNAUTHORIZED_ROLE",
			"role "+role+" cannot read the regulatory watch register")
	}
	return role, nil
}

// RegChangeAuditorScope reports whether a role is confined to the
// untriaged/near-deadline projection.
func RegChangeAuditorScope(role string) bool {
	return role == RoleReadOnlyAuditor
}

// RegChangeAuditorVisible is the scoped projection predicate over
// primitives — true when the row is untriaged (TRACKED) or effective
// inside the 90-day window (spec §14.10.2 item 6).
func RegChangeAuditorVisible(status string, triageDueAt time.Time,
	effectiveAt *time.Time, now time.Time) bool {
	if status == "TRACKED" {
		return true
	}
	if effectiveAt != nil && effectiveAt.Before(now.Add(RegChangeEffectiveWindow)) &&
		(status == "TRACKED" || status == "TRIAGED" || status == "SCOPED") {
		return true
	}
	return false
}

// RegChangeTriageDeadline computes published_at + RegChangeTriageSLA
// business days — weekday counting minus the injected holiday set.
func RegChangeTriageDeadline(publishedAt time.Time,
	holidays map[time.Time]bool) time.Time {
	return BusinessDaysAfter(publishedAt, RegChangeTriageSLA, holidays)
}

// BusinessDaysAfter adds n business days to t — the publication day is
// day 0, the deadline lands on the n-th following business day
// (weekdays minus date-truncated-UTC holiday keys).
func BusinessDaysAfter(t time.Time, n int,
	holidays map[time.Time]bool) time.Time {
	d := t
	for i := 0; i < n; {
		d = d.Add(24 * time.Hour)
		wd := d.Weekday()
		if wd == time.Saturday || wd == time.Sunday ||
			holidays[d.UTC().Truncate(24*time.Hour)] {
			continue
		}
		i++
	}
	return d
}
