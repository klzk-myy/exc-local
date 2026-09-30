package incident

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"exchange/internal/observability"
)

// Pager is the automated-paging seam — observability.Sink in disguise:
// production wires observability.FanoutSink{PublisherSink{Pub: nats,
// Subject: ops.alerts.monitoring}, LogSink{}} so pages land on the same
// ops paging path the alert evaluator (Task 7.3.10) uses.
type Pager interface {
	Raise(ctx context.Context, a observability.Alert) error
}

// Options wires Manager. Store, Pager and ChatOps are ALL mandatory —
// the bot exists to provision war rooms and page people; constructing
// it without either leg would manufacture a silent no-op (fail closed).
type Options struct {
	Store Store
	Pager Pager
	Chat  ChatOps
	Log   *slog.Logger
	Now   func() time.Time // tests only
	// TickInterval for Run(); default 30s.
	TickInterval time.Duration
}

// Manager is the incident-management bot.
type Manager struct {
	store Store
	pager Pager
	chat  ChatOps
	log   *slog.Logger
	now   func() time.Time
	tick  time.Duration

	// autoSrc maps an auto-declaring alert rule to its open incident id —
	// HandleAlert dedupes re-fires and resolves on the clear edge.
	autoSrc map[string]string
}

// NewManager validates wiring and returns the bot.
func NewManager(o Options) (*Manager, error) {
	if o.Store == nil {
		return nil, fmt.Errorf("incident: store required")
	}
	if o.Pager == nil {
		return nil, fmt.Errorf("incident: pager required — war-room paging is contractual (§19.8)")
	}
	if o.Chat == nil {
		return nil, fmt.Errorf("incident: chatops provider required — war-room provisioning is contractual (§19.8)")
	}
	m := &Manager{
		store: o.Store, pager: o.Pager, chat: o.Chat,
		log: o.Log, now: time.Now, tick: o.TickInterval,
		autoSrc: map[string]string{},
	}
	if m.log == nil {
		m.log = slog.Default()
	}
	if o.Now != nil {
		m.now = o.Now
	}
	if m.tick <= 0 {
		m.tick = 30 * time.Second
	}
	return m, nil
}

// page raises one alert on the ops paging path. Errors are returned to
// the caller AND journaled (TlPageFailed) — a page failure is itself a
// P2-worthy fact, never a silent gap.
func (m *Manager) page(ctx context.Context, inc *Incident, code, status, summary string, details map[string]string) error {
	if details == nil {
		details = map[string]string{}
	}
	details["incident_id"] = inc.ID
	if inc.WarRoom != "" {
		details["war_room"] = inc.WarRoom
	}
	return m.pager.Raise(ctx, observability.Alert{
		Rule:     "incident." + inc.ID,
		Severity: string(inc.Severity),
		Code:     code,
		Summary:  summary,
		Status:   status,
		Details:  details,
		FiredAt:  m.now().UTC().Format(time.RFC3339Nano),
	})
}

// tl journals one timeline entry; store errors are logged (the incident
// record itself already landed) — the ledger degrades, never panics.
func (m *Manager) tl(ctx context.Context, id, kind, by, text string) {
	if err := m.store.AppendTimeline(ctx, id, TimelineEntry{
		At: m.now().UTC(), Kind: kind, By: by, Text: text,
	}); err != nil {
		m.log.Error("incident: timeline append failed", "id", id, "kind", kind, "err", err)
	}
}

// Declare opens an incident and executes the declaration playbook:
// ledger record → severity-matrix paging → (P0/P1) war-room channel +
// bridge + commander + opening post. Every step is journaled. A paging
// or ChatOps failure is reported to the caller — the incident record
// still exists (fail closed: the ledger is the source of truth, the
// error tells the declarer which obligations did NOT complete).
func (m *Manager) Declare(ctx context.Context, d Declaration) (*Incident, error) {
	sev, err := ParseSeverity(d.Severity)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(d.Title) == "" {
		return nil, fmt.Errorf("incident: title required")
	}
	now := m.now().UTC()
	id, err := m.store.NextID(ctx, now.Format("20060102"))
	if err != nil {
		return nil, fmt.Errorf("incident: allocate id: %w", err)
	}
	row := Matrix(sev)
	src := d.Source
	if src == "" {
		src = "manual"
	}
	inc := &Incident{
		ID: id, Title: d.Title, Severity: sev, Status: StatusOpen,
		Summary: d.Summary, Source: src, DeclaredBy: d.By,
		DeclaredAt: now, Commander: d.Commander,
	}
	if inc.Commander == "" && row.WarRoom {
		inc.Commander = "oncall-sre" // runbook §3.2 default
	}
	if err := m.store.Create(ctx, inc); err != nil {
		return nil, err
	}
	m.tl(ctx, id, TlDeclared, d.By,
		fmt.Sprintf("%s declared: %s (%s)", sev, d.Title, src))

	// War room first (P0/P1): the channel exists before pages reference it.
	var warRoomErr error
	if row.WarRoom {
		warRoomErr = m.provisionWarRoom(ctx, inc)
		if uerr := m.store.Update(ctx, inc); uerr != nil {
			m.log.Error("incident: record update failed", "id", id, "err", uerr)
		}
	}

	// First page — every matrix target simultaneously (the paging path
	// fans out by severity; targets are journaled for the post-mortem).
	pageErr := m.page(ctx, inc, "INCIDENT_DECLARED", "firing",
		fmt.Sprintf("%s %s: %s", inc.ID, sev, d.Title),
		map[string]string{
			"targets": strings.Join(row.PageTargets, ","),
			"summary": d.Summary,
		})
	if pageErr != nil {
		m.tl(ctx, id, TlPageFailed, "incident-bot", pageErr.Error())
	} else {
		m.tl(ctx, id, TlPage, "incident-bot",
			"paged "+strings.Join(row.PageTargets, ","))
	}

	switch {
	case warRoomErr != nil && pageErr != nil:
		return inc, fmt.Errorf("incident: war room: %v; page: %v", warRoomErr, pageErr)
	case warRoomErr != nil:
		return inc, fmt.Errorf("incident: war room: %w", warRoomErr)
	case pageErr != nil:
		return inc, fmt.Errorf("incident: page: %w", pageErr)
	}
	return inc, nil
}

// provisionWarRoom runs runbook §3 steps 1–2: incident channel, then
// bridge, then the opening post with commander + severity context.
// Failures journal into the timeline and return to Declare.
func (m *Manager) provisionWarRoom(ctx context.Context, inc *Incident) error {
	name := inc.RoomName()
	room, err := m.chat.CreateRoom(ctx, Room{
		Name:  name,
		Topic: fmt.Sprintf("%s %s — %s", inc.ID, inc.Severity, inc.Title),
	})
	if err != nil {
		m.tl(ctx, inc.ID, TlPageFailed, "incident-bot",
			"war-room create failed: "+err.Error())
		return fmt.Errorf("create room: %w", err)
	}
	inc.WarRoom = room.Name
	m.tl(ctx, inc.ID, TlWarRoom, "incident-bot",
		"war room #"+room.Name+" provisioned")

	bridge, err := m.chat.OpenBridge(ctx, room)
	if err != nil {
		m.tl(ctx, inc.ID, TlPageFailed, "incident-bot",
			"bridge spin-up failed: "+err.Error())
		return fmt.Errorf("open bridge: %w", err)
	}
	inc.Bridge = bridge.URI
	m.tl(ctx, inc.ID, TlWarRoom, "incident-bot",
		"conference bridge: "+bridge.URI)

	opening := fmt.Sprintf(
		"[%s %s] %s — commander: %s. Impact/mitigation updates every %s. "+
			"Narrate all mitigation actions in-channel before execution.",
		inc.ID, inc.Severity, inc.Title, inc.Commander,
		Matrix(inc.Severity).UpdateEvery)
	if err := m.chat.PostMessage(ctx, room, "incident-bot", opening); err != nil {
		m.tl(ctx, inc.ID, TlPageFailed, "incident-bot",
			"opening post failed: "+err.Error())
		return fmt.Errorf("opening post: %w", err)
	}
	return nil
}

// Acknowledge records who took the page — stops the escalation clock.
func (m *Manager) Acknowledge(ctx context.Context, id, by string) (*Incident, error) {
	inc, err := m.get(ctx, id)
	if err != nil {
		return nil, err
	}
	if inc.Status == StatusResolved {
		return nil, fmt.Errorf("incident: %s already resolved", id)
	}
	now := m.now().UTC()
	inc.AckedAt, inc.AckedBy = &now, by
	inc.Status = StatusAcknowledged
	if err := m.store.Update(ctx, inc); err != nil {
		return nil, err
	}
	m.tl(ctx, id, TlAck, by, "acknowledged")
	return inc, nil
}

// PostUpdate posts a stakeholder update into the war room (P0/P1) and
// journals it — the manual counterpart of the Tick-driven cadence.
func (m *Manager) PostUpdate(ctx context.Context, id, by, text string) error {
	inc, err := m.get(ctx, id)
	if err != nil {
		return err
	}
	if inc.Status == StatusResolved {
		return fmt.Errorf("incident: %s already resolved", id)
	}
	if inc.WarRoom != "" {
		if err := m.chat.PostMessage(ctx, Room{Name: inc.WarRoom}, by, text); err != nil {
			m.tl(ctx, id, TlPageFailed, by, "update post failed: "+err.Error())
			return fmt.Errorf("incident: post update: %w", err)
		}
	}
	now := m.now().UTC()
	inc.LastUpdateAt = &now
	if uerr := m.store.Update(ctx, inc); uerr != nil {
		m.log.Error("incident: record update failed", "id", id, "err", uerr)
	}
	m.tl(ctx, id, TlUpdate, by, text)
	return nil
}

// Resolve closes the incident: resolves the paging edge (self-clearing
// channel semantics — same contract as the alert evaluator), posts the
// close-out to the war room, and stamps ResolvedAt. The post-mortem
// clock keeps running — Resolve does not excuse the 48h SLA.
func (m *Manager) Resolve(ctx context.Context, id, summary string) (*Incident, error) {
	inc, err := m.get(ctx, id)
	if err != nil {
		return nil, err
	}
	if inc.Status == StatusResolved {
		return inc, nil // idempotent
	}
	now := m.now().UTC()
	inc.Status = StatusResolved
	inc.ResolvedAt = &now
	if err := m.store.Update(ctx, inc); err != nil {
		return nil, err
	}
	m.tl(ctx, id, TlResolve, "incident-bot", summary)
	if inc.WarRoom != "" {
		if err := m.chat.PostMessage(ctx, Room{Name: inc.WarRoom},
			"incident-bot", "RESOLVED: "+summary); err != nil {
			m.tl(ctx, id, TlPageFailed, "incident-bot",
				"resolve post failed: "+err.Error())
		}
	}
	if err := m.page(ctx, inc, "INCIDENT_RESOLVED", "resolved",
		fmt.Sprintf("%s resolved: %s", inc.ID, summary), nil); err != nil {
		m.tl(ctx, id, TlPageFailed, "incident-bot",
			"resolve page failed: "+err.Error())
		return inc, fmt.Errorf("incident: resolve page: %w", err)
	}
	return inc, nil
}

// RecordPostMortem files the blameless post-mortem (runbook §5) —
// clears the 48h SLA obligation.
func (m *Manager) RecordPostMortem(ctx context.Context, id, url string) (*Incident, error) {
	inc, err := m.get(ctx, id)
	if err != nil {
		return nil, err
	}
	now := m.now().UTC()
	inc.PostMortemURL = url
	inc.PostMortemRecordedAt = &now
	if err := m.store.Update(ctx, inc); err != nil {
		return nil, err
	}
	m.tl(ctx, id, TlPostMortem, "incident-bot", "post-mortem filed: "+url)
	return inc, nil
}

// Tick runs one automation pass over open incidents — call it from Run
// or directly in tests:
//
//   - unacknowledged page past the matrix AckTimeout → escalate once to
//     EscalateTo (P0: 5m, P1: 15m, P2: 4h; P3 has no escalation tier);
//   - stakeholder update due (matrix UpdateEvery, 30m for P0/P1) →
//     post the status template into the war room and journal it;
//   - post-mortem SLA breached (DeclaredAt+48h, P0/P1) with no filing →
//     raise a P2 POSTMORTEM_OVERDUE alert once and journal it.
func (m *Manager) Tick(ctx context.Context) {
	incs, err := m.store.List(ctx)
	if err != nil {
		m.log.Error("incident: tick list failed", "err", err)
		return
	}
	now := m.now().UTC()
	for _, inc := range incs {
		row := Matrix(inc.Severity)
		if inc.Status != StatusResolved {
			// Escalation clock.
			if inc.AckedAt == nil && !inc.Escalated && len(row.EscalateTo) > 0 &&
				row.AckTimeout > 0 && now.Sub(inc.DeclaredAt) >= row.AckTimeout {
				m.escalate(ctx, inc, row)
			}
			// Stakeholder-update cadence.
			if inc.WarRoom != "" && row.UpdateEvery > 0 {
				last := inc.DeclaredAt
				if inc.LastUpdateAt != nil {
					last = *inc.LastUpdateAt
				}
				if now.Sub(last) >= row.UpdateEvery {
					m.postScheduledUpdate(ctx, inc)
				}
			}
		}
		// Post-mortem SLA — checked even post-resolution: resolving the
		// incident does not retire the write-up obligation.
		if row.PostMortemSLA > 0 && inc.PostMortemURL == "" &&
			!inc.PostMortemOverdueRaised &&
			now.Sub(inc.DeclaredAt) >= row.PostMortemSLA {
			m.raisePostMortemOverdue(ctx, inc)
		}
	}
}

// escalate pages the matrix's second tier — once.
func (m *Manager) escalate(ctx context.Context, inc *Incident, row MatrixRow) {
	to := strings.Join(row.EscalateTo, ",")
	err := m.page(ctx, inc, "INCIDENT_ESCALATED", "firing",
		fmt.Sprintf("%s unacknowledged %s — escalating", inc.ID, row.AckTimeout),
		map[string]string{"escalate_to": to})
	inc.Escalated = true // latched regardless — a paging outage must not spam
	if uerr := m.store.Update(ctx, inc); uerr != nil {
		m.log.Error("incident: record update failed", "id", inc.ID, "err", uerr)
	}
	if err != nil {
		m.tl(ctx, inc.ID, TlPageFailed, "incident-bot", "escalation page failed: "+err.Error())
		return
	}
	m.tl(ctx, inc.ID, TlEscalate, "incident-bot", "escalated to "+to)
}

// postScheduledUpdate writes the runbook §3.3 status template:
// impact / scope / current mode / mitigation in progress / next update.
func (m *Manager) postScheduledUpdate(ctx context.Context, inc *Incident) {
	room := Room{Name: inc.WarRoom}
	next := m.now().UTC().Add(Matrix(inc.Severity).UpdateEvery)
	text := fmt.Sprintf(
		"[status update] %s %s — impact: %s | status: %s | commander: %s | "+
			"mitigation: in progress | next update: %s",
		inc.ID, inc.Severity, firstLine(inc.Summary, inc.Title),
		inc.Status, inc.Commander, next.Format("15:04Z"))
	if err := m.chat.PostMessage(ctx, room, "incident-bot", text); err != nil {
		m.tl(ctx, inc.ID, TlPageFailed, "incident-bot", "scheduled update failed: "+err.Error())
		return
	}
	now := m.now().UTC()
	inc.LastUpdateAt = &now
	if err := m.store.Update(ctx, inc); err != nil {
		m.log.Error("incident: record update failed", "id", inc.ID, "err", err)
	}
	m.tl(ctx, inc.ID, TlUpdate, "incident-bot", text)
}

// raisePostMortemOverdue pages P2 once when the 48h SLA lapses.
func (m *Manager) raisePostMortemOverdue(ctx context.Context, inc *Incident) {
	sev := inc.Severity
	inc.Severity = SeverityP2 // the OVERDUE signal itself is a P2 event
	err := m.page(ctx, inc, "POSTMORTEM_OVERDUE", "firing",
		fmt.Sprintf("%s post-mortem not filed within 48h SLA", inc.ID), nil)
	inc.Severity = sev
	inc.PostMortemOverdueRaised = true
	if uerr := m.store.Update(ctx, inc); uerr != nil {
		m.log.Error("incident: record update failed", "id", inc.ID, "err", uerr)
	}
	if err != nil {
		m.tl(ctx, inc.ID, TlPageFailed, "incident-bot", "postmortem-overdue page failed: "+err.Error())
		return
	}
	m.tl(ctx, inc.ID, TlPostMortemDue, "incident-bot",
		"48h post-mortem SLA breached — P2 alert raised")
}

// HandleAlert is the ops.alerts.monitoring subscriber hook: a firing
// P0–P3 alert auto-declares an incident (deduped by rule id — one
// incident per firing root, matching runbook §6 cross-service cascade
// semantics), a resolved edge resolves the auto-opened incident.
func (m *Manager) HandleAlert(ctx context.Context, a observability.Alert) (*Incident, error) {
	sev, err := ParseSeverity(a.Severity)
	if err != nil {
		return nil, fmt.Errorf("incident: alert severity: %w", err)
	}
	if a.Rule == "" {
		return nil, fmt.Errorf("incident: alert rule id required")
	}
	switch a.Status {
	case "firing":
		if id, ok := m.autoSrc[a.Rule]; ok {
			return m.store.Get(ctx, id) // already declared for this root
		}
		inc, err := m.Declare(ctx, Declaration{
			Severity: string(sev),
			Title:    fmt.Sprintf("%s — %s", a.Rule, a.Summary),
			Summary:  a.Summary,
			By:       "alert-evaluator",
			Source:   a.Rule,
		})
		if err == nil {
			m.autoSrc[a.Rule] = inc.ID
		}
		return inc, err
	case "resolved":
		id, ok := m.autoSrc[a.Rule]
		if !ok {
			return nil, nil // nothing auto-opened — manual incidents stay put
		}
		delete(m.autoSrc, a.Rule)
		return m.Resolve(ctx, id, fmt.Sprintf("alert %s resolved", a.Rule))
	default:
		return nil, fmt.Errorf("incident: unknown alert status %q", a.Status)
	}
}

// Run ticks on TickInterval until ctx cancels — the bot's loop next to
// the alert-evaluator wiring.
func (m *Manager) Run(ctx context.Context) {
	t := time.NewTicker(m.tick)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			m.Tick(ctx)
		}
	}
}

// Get returns one incident (exported for ops tooling/tests).
func (m *Manager) Get(ctx context.Context, id string) (*Incident, error) {
	return m.get(ctx, id)
}

func (m *Manager) get(ctx context.Context, id string) (*Incident, error) {
	if !idPattern.MatchString(id) {
		return nil, fmt.Errorf("incident: malformed id %q", id)
	}
	return m.store.Get(ctx, id)
}

// firstLine returns s if non-empty, else fallback — truncated at the
// first newline so the update template stays single-line.
func firstLine(s, fallback string) string {
	if s == "" {
		s = fallback
	}
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	return s
}
