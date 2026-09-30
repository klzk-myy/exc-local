// Package incident implements the Phase-09 Task 9.3.18 incident
// management bot: P0–P3 classification, automated paging per the
// escalation matrix in docs/runbooks/incident-escalation.md, war-room
// provisioning, 30-minute stakeholder update cadence, unacknowledged-
// page escalation, and 48-hour post-mortem SLA tracking
// (spec §19.8, §24 #183).
//
// The bot is transport-agnostic by design — every external integration
// is a seam interface and nothing is ever "fabricated":
//
//   - Pager:  observability.Sink — production wires
//     observability.PublisherSink onto the existing
//     ops.alerts.monitoring NATS subject (the ops paging path the
//     alert evaluator already uses), fanned out with LogSink so a
//     paging-provider outage never goes silent.
//   - ChatOps: war-room provider — FileChatOps is the shipped dev/test
//     adapter (rooms and message history are real files on disk);
//     a Slack/Teams adapter implements the same interface and is
//     wired at cmd level when credentials exist.
//   - Store:  incident ledger — FileStore writes one directory per
//     incident (incident.json + timeline.jsonl); MemStore serves
//     tests. A PG adapter can persist into ops_incidents
//     (migration 197) without changing Manager.
//
// Fail closed (spec §2.7): Store, Pager and ChatOps are mandatory —
// NewManager refuses partial wiring; a failed page surfaces as an error
// and a timeline entry, never as a phantom success.
package incident

import (
	"fmt"
	"regexp"
	"time"
)

// Severity vocabulary — spec §19.8 incident classes; mirrors the
// ops_incidents.severity CHECK constraint (migration 197) and the
// alert severity labels in deploy/prometheus.
type Severity string

const (
	SeverityP0 Severity = "P0" // critical — halt/corruption/breach
	SeverityP1 Severity = "P1" // major — shard loss, degraded path
	SeverityP2 Severity = "P2" // moderate — non-critical degradation
	SeverityP3 Severity = "P3" // minor — cosmetic/informational
)

// ParseSeverity validates a severity token (exact canonical casing).
func ParseSeverity(s string) (Severity, error) {
	switch Severity(s) {
	case SeverityP0, SeverityP1, SeverityP2, SeverityP3:
		return Severity(s), nil
	}
	return "", fmt.Errorf("incident: unknown severity %q (want P0|P1|P2|P3)", s)
}

// Incident status lattice — OPEN while the incident needs a commander,
// ACKNOWLEDGED once a responder has taken it, RESOLVED at close.
// (MONITORING exists on the ops_incidents public table; the bot's
// internal lifecycle resolves straight to RESOLVED — post-resolution
// monitoring is the alerting system's job, not the ledger's.)
const (
	StatusOpen         = "OPEN"
	StatusAcknowledged = "ACKNOWLEDGED"
	StatusResolved     = "RESOLVED"
)

// Incident is one ledger record — the unit the bot automates around.
type Incident struct {
	ID         string    `json:"id"` // INC-YYYYMMDD-NNNN
	Title      string    `json:"title"`
	Severity   Severity  `json:"severity"`
	Status     string    `json:"status"`
	Summary    string    `json:"summary"`
	Source     string    `json:"source"` // "manual" | alert rule id that auto-declared it
	DeclaredBy string    `json:"declared_by"`
	DeclaredAt time.Time `json:"declared_at"`
	Commander  string    `json:"commander"` // incident commander (war-room protocol §3.2)

	AckedBy   string     `json:"acked_by,omitempty"`
	AckedAt   *time.Time `json:"acked_at,omitempty"`
	Escalated bool       `json:"escalated,omitempty"`

	// War-room provisioning (P0/P1 per the runbook; empty for P2/P3).
	WarRoom string `json:"war_room,omitempty"` // room name, e.g. inc-20260927-0003
	Bridge  string `json:"bridge,omitempty"`   // conference bridge handle/URI

	// Stakeholder-update cadence bookkeeping (P0/P1).
	LastUpdateAt *time.Time `json:"last_update_at,omitempty"`

	// Post-mortem SLA bookkeeping (P0/P1: due = DeclaredAt + 48h).
	PostMortemURL           string     `json:"postmortem_url,omitempty"`
	PostMortemRecordedAt    *time.Time `json:"postmortem_recorded_at,omitempty"`
	PostMortemOverdueRaised bool       `json:"postmortem_overdue_raised,omitempty"`

	ResolvedAt *time.Time `json:"resolved_at,omitempty"`
}

// Declaration is the Declare() input — what a human, the alert
// subscriber, or a runbook script hands the bot.
type Declaration struct {
	Severity  string `json:"severity"`
	Title     string `json:"title"`
	Summary   string `json:"summary"`
	By        string `json:"by"`     // declarer identity (on-call handle, "alert-evaluator")
	Source    string `json:"source"` // "manual" or the alert rule id
	Commander string `json:"commander,omitempty"`
}

// TimelineEntry is one line of the incident ledger (timeline.jsonl in
// FileStore) — the audit trail the post-mortem is assembled from.
type TimelineEntry struct {
	At   time.Time `json:"at"`
	Kind string    `json:"kind"` // declared|war_room|page|ack|escalate|update|resolve|postmortem_due|postmortem
	By   string    `json:"by,omitempty"`
	Text string    `json:"text"`
}

// Timeline kinds.
const (
	TlDeclared      = "declared"
	TlWarRoom       = "war_room"
	TlPage          = "page"
	TlPageFailed    = "page_failed"
	TlAck           = "ack"
	TlEscalate      = "escalate"
	TlUpdate        = "update"
	TlResolve       = "resolve"
	TlPostMortemDue = "postmortem_due"
	TlPostMortem    = "postmortem"
)

// idPattern validates caller-supplied incident ids on lookup/mutation.
var idPattern = regexp.MustCompile(`^INC-\d{8}-\d{4}$`)

// RoomName derives the war-room channel name — runbook §3:
// "#inc-YYYYMMDD-id" (the '#' is the provider's channel-marker; the
// stored name omits it so it is a legal filename under FileChatOps).
func (i *Incident) RoomName() string {
	if i.ID == "" {
		return ""
	}
	return "inc-" + i.ID[4:]
}
