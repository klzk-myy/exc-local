package incident

import "time"

// MatrixRow is one row of the escalation matrix — the machine-readable
// mirror of docs/runbooks/incident-escalation.md §2 (which itself
// mirrors spec §19.8 with the remediation-#35 internal stretch clocks).
// Keep this table and the runbook in lockstep: the runbook is the
// human contract, this is what the bot executes.
type MatrixRow struct {
	Severity Severity

	// PageTargets is the first-page roster (paged simultaneously through
	// the ops paging seam — alertmanager/PagerDuty routes by severity).
	PageTargets []string

	// AckTimeout is the internal stretch acknowledgment clock: a page
	// still unacknowledged past it escalates to EscalateTo.
	AckTimeout time.Duration
	EscalateTo []string

	// WarRoom: whether Declare provisions an incident channel + bridge.
	WarRoom bool

	// UpdateEvery is the stakeholder status-update cadence inside the
	// war room (P0/P1 per runbook §3.3); 0 = no automated cadence.
	UpdateEvery time.Duration

	// PostMortemSLA is the blameless post-mortem completion clock from
	// declaration (P0/P1: 48h); 0 = post-mortem not mandated.
	PostMortemSLA time.Duration
}

// EscalationMatrix is the canonical table. Values:
//
//	P0  page on-call SRE + Core Eng Lead + VP Eng + CTO simultaneously;
//	    unacked 5m → secondary engineering leadership; war room; 30m
//	    stakeholder updates; 48h post-mortem.
//	P1  page on-call SRE + Component Lead; unacked 15m → primary on-call
//	    manager; war room; 30m updates; 48h post-mortem.
//	P2  ticket + #exchange-ops (ops channel via the paging seam);
//	    unacked 4h → team-lead queue review; no war room, no PM SLA.
//	P3  #exchange-ops only; 24h target; no escalation tier.
var EscalationMatrix = map[Severity]MatrixRow{
	SeverityP0: {
		Severity:      SeverityP0,
		PageTargets:   []string{"oncall-sre", "core-eng-lead", "vp-eng", "cto"},
		AckTimeout:    5 * time.Minute,
		EscalateTo:    []string{"secondary-eng-leadership"},
		WarRoom:       true,
		UpdateEvery:   30 * time.Minute,
		PostMortemSLA: 48 * time.Hour,
	},
	SeverityP1: {
		Severity:      SeverityP1,
		PageTargets:   []string{"oncall-sre", "component-lead"},
		AckTimeout:    15 * time.Minute,
		EscalateTo:    []string{"primary-oncall-manager"},
		WarRoom:       true,
		UpdateEvery:   30 * time.Minute,
		PostMortemSLA: 48 * time.Hour,
	},
	SeverityP2: {
		Severity:      SeverityP2,
		PageTargets:   []string{"ops-ticket", "exchange-ops-channel"},
		AckTimeout:    4 * time.Hour,
		EscalateTo:    []string{"team-lead-queue"},
		WarRoom:       false,
		UpdateEvery:   0,
		PostMortemSLA: 0,
	},
	SeverityP3: {
		Severity:      SeverityP3,
		PageTargets:   []string{"exchange-ops-channel"},
		AckTimeout:    24 * time.Hour,
		EscalateTo:    nil, // terminal tier — ticket queue review only
		WarRoom:       false,
		UpdateEvery:   0,
		PostMortemSLA: 0,
	},
}

// Matrix returns the row for sev. Callers use ParseSeverity first, so a
// miss is a programming error — return the zero row and let the caller
// decide (Manager treats a zero row as a wiring bug and fails closed).
func Matrix(sev Severity) MatrixRow { return EscalationMatrix[sev] }
