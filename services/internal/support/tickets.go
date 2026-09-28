// Package support implements the Phase-07 Task 7.3.7 support ticket +
// complaint workflow: client ticket creation/listing, the admin
// assign/transition/notes surface, complaint routing to the Compliance
// Officer queue with ADR fields (spec §14.10.3 remediation #17), and the
// SLA-breach scan that feeds PagerDuty (P3 support SLA, P2 complaint
// acknowledgment per the §27.1 ADR matrix).
//
// Storage: support_tickets + ticket_notes (migration 048, spec §5.28/§5.44).
// Every admin mutation writes admin_audit_log + the audit_hash_chain
// link inside the same transaction via internal/admin.Log — a ticket
// state change cannot commit without its audit trail.
package support

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"exchange/internal/admin"

	excerrors "exchange/pkg/errors"
)

// Ticket categories (spec §5.28 / Task 7.3.7 field list — the pinned
// five-value enum; clients pick the nearest bucket, free-text detail
// lives in subject/body).
const (
	CategoryFunding   = "FUNDING"
	CategoryTrading   = "TRADING"
	CategoryKYC       = "KYC"
	CategoryTechnical = "TECHNICAL"
	CategoryComplaint = "COMPLAINT"
)

// Ticket types (spec §5.28 enum). COMPLAINT/DISPUTE route to the
// Compliance Officer queue.
const (
	TypeSupport   = "SUPPORT"
	TypeComplaint = "COMPLAINT"
	TypeDispute   = "DISPUTE"
)

// Queues.
const (
	QueueSupport    = "SUPPORT"
	QueueCompliance = "COMPLIANCE"
)

// Priorities.
const (
	PriorityLow    = "LOW"
	PriorityNormal = "NORMAL"
	PriorityHigh   = "HIGH"
	PriorityUrgent = "URGENT"
)

// Statuses — union of the task text (OPEN|PENDING|RESOLVED|CLOSED) and
// the spec §5.28 enum (OPEN|IN_PROGRESS|RESOLVED|CLOSED).
const (
	StatusOpen       = "OPEN"
	StatusPending    = "PENDING"     // waiting on the client
	StatusInProgress = "IN_PROGRESS" // actively worked
	StatusResolved   = "RESOLVED"
	StatusClosed     = "CLOSED"
)

// Origin channels a complaint can arrive on (spec §14.10.3).
var OriginChannels = map[string]bool{
	"PORTAL": true, "EMAIL": true, "API": true, "LETTER": true,
}

// ValidCategory / ValidPriority / ValidStatus gate user input.
func ValidCategory(c string) bool {
	switch c {
	case CategoryFunding, CategoryTrading, CategoryKYC, CategoryTechnical,
		CategoryComplaint:
		return true
	}
	return false
}

func ValidPriority(p string) bool {
	switch p {
	case PriorityLow, PriorityNormal, PriorityHigh, PriorityUrgent:
		return true
	}
	return false
}

func ValidStatus(s string) bool {
	switch s {
	case StatusOpen, StatusPending, StatusInProgress, StatusResolved, StatusClosed:
		return true
	}
	return false
}

// transitions is the ticket state machine. CLOSED is terminal; RESOLVED
// can reopen to IN_PROGRESS (client reply) but only admins move there.
var transitions = map[string][]string{
	StatusOpen:       {StatusPending, StatusInProgress, StatusResolved, StatusClosed},
	StatusPending:    {StatusInProgress, StatusResolved, StatusClosed},
	StatusInProgress: {StatusPending, StatusResolved, StatusClosed},
	StatusResolved:   {StatusInProgress, StatusClosed},
	StatusClosed:     {},
}

// ValidTransition reports whether from→to is an allowed move.
func ValidTransition(from, to string) bool {
	for _, s := range transitions[from] {
		if s == to {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// Model
// ---------------------------------------------------------------------------

// Ticket is one support_tickets row.
type Ticket struct {
	ID              int64     `json:"ticket_id"`
	AccountID       int64     `json:"account_id"`
	Type            string    `json:"type"`
	Category        string    `json:"category"`
	Priority        string    `json:"priority"`
	Subject         string    `json:"subject"`
	Body            string    `json:"body,omitempty"`
	Status          string    `json:"status"`
	Queue           string    `json:"queue"`
	AssigneeAdminID *int64    `json:"assignee_admin_id,omitempty"`
	OriginChannel   string    `json:"origin_channel"`
	SLADueAt        time.Time `json:"sla_due_at"`
	// FinalResponseDueAt is the statutory 8-week final-response deadline
	// (complaints/disputes only; nil for plain support tickets).
	FinalResponseDueAt *time.Time `json:"final_response_due_at,omitempty"`
	AcknowledgedAt     *time.Time `json:"acknowledged_at,omitempty"`
	ResolvedAt         *time.Time `json:"resolved_at,omitempty"`
	ADRRequested       bool       `json:"adr_requested"`
	ADRScheme          string     `json:"adr_scheme,omitempty"`
	ADRReference       string     `json:"adr_reference,omitempty"`
	ADRAckedAt         *time.Time `json:"adr_acknowledged_at,omitempty"`
	CreatedAt          time.Time  `json:"created_at"`
	UpdatedAt          time.Time  `json:"updated_at"`
	// SLABreached / FinalSLABreached are computed, not stored: now past
	// the respective deadline while the ticket is unresolved.
	SLABreached      bool `json:"sla_breached"`
	FinalSLABreached bool `json:"final_sla_breached,omitempty"`
}

// Note is one ticket_notes row.
type Note struct {
	NoteID     int64     `json:"note_id"`
	TicketID   int64     `json:"ticket_id"`
	AuthorID   int64     `json:"author_id"`
	AuthorKind string    `json:"author_kind"` // ADMIN | CLIENT
	Internal   bool      `json:"internal"`
	Body       string    `json:"body"`
	CreatedAt  time.Time `json:"created_at"`
}

// CreateRequest is the client-side ticket open.
type CreateRequest struct {
	AccountID     int64
	Category      string
	Subject       string
	Body          string
	Priority      string // optional — default NORMAL
	OriginChannel string // optional — default PORTAL
	Now           time.Time
}

// AdminUpdate is the admin mutation surface (PUT
// /api/v1/admin/support/tickets): assignment, status transition,
// priority, and the ADR-routing fields for complaints.
type AdminUpdate struct {
	TicketID        int64
	Status          string // optional transition target
	AssigneeAdminID *int64 // explicit assign (nil = unchanged); use AssignToMe instead where intended
	AssignToMe      bool
	Priority        string // optional change
	Note            string // optional internal note written in the same tx
	// ADR routing (complaints only): client-requested external
	// resolution to the jurisdiction's ombudsman scheme.
	ADRRequested *bool
	ADRScheme    string
	ADRReference string
	ADRAck       bool // mark adr_acknowledged_at = now
}

// AdminFilter narrows the admin ticket list.
type AdminFilter struct {
	Status       string
	Category     string
	Type         string
	Queue        string
	AssigneeID   *int64
	Unassigned   bool
	BreachedOnly bool
	AccountID    *int64
}

// RoleResolver resolves an admin's §8.2 role name — the Phase-07 Task
// 7.3.1 seam (internal/admin rbac.go, owned by the concurrent RBAC
// work). Signature-compatible with api.AdminRoleResolver; nil fails
// closed with UNAUTHORIZED_ROLE.
type RoleResolver func(ctx context.Context, adminUserID int64) (string, error)

// Alerter raises operational alerts (PagerDuty in production). The SLA
// sweeper reports breaches through it; nil-safe implementations log.
type Alerter interface {
	Raise(ctx context.Context, severity, code, message string) error
}

// SLA durations (canonical values, Task 7.3.7 item 3 + §27.1 ADR row):
// complaints run the internal 8-business-hour acknowledgment clock BUT
// the statutory 48-hour acknowledgment deadline is an outer bound — a
// Friday-evening complaint must not wait out the weekend past 48h.
// Unresolved complaints also carry an 8-week statutory final-response
// deadline (§27.1 "48h acknowledgment, 8-week final response").
const (
	ComplaintAckSLA           = 8 * time.Hour          // business hours — see businessHours
	ComplaintStatutoryAckSLA  = 48 * time.Hour         // wall-clock statutory outer bound
	ComplaintFinalResponseSLA = 8 * 7 * 24 * time.Hour // 8 weeks, wall-clock
	SupportAckSLA             = 24 * time.Hour
)

// businessHours adds n business hours (Mon–Fri) to t — the complaint SLA
// runs on business time (24/5 venue calendar; weekend hours don't count).
func businessHours(t time.Time, n time.Duration) time.Time {
	t = t.UTC()
	remaining := n
	for remaining > 0 {
		t = t.Add(time.Hour)
		if wd := t.Weekday(); wd != time.Saturday && wd != time.Sunday {
			remaining -= time.Hour
		}
	}
	return t
}

// slaFor returns the acknowledgment deadline for a new ticket: support
// tickets get 24h; complaints get the earlier of 8 business hours and
// the statutory 48h wall-clock deadline.
func slaFor(ticketType string, now time.Time) time.Time {
	if ticketType == TypeComplaint || ticketType == TypeDispute {
		internal := businessHours(now, ComplaintAckSLA)
		statutory := now.Add(ComplaintStatutoryAckSLA)
		if statutory.Before(internal) {
			return statutory
		}
		return internal
	}
	return now.Add(SupportAckSLA)
}

// finalResponseDue returns the 8-week statutory final-response deadline
// for compliance-queue tickets; nil for plain support tickets.
func finalResponseDue(ticketType string, now time.Time) *time.Time {
	if ticketType != TypeComplaint && ticketType != TypeDispute {
		return nil
	}
	d := now.Add(ComplaintFinalResponseSLA)
	return &d
}

// typeFor maps the client-facing category to the §5.28 type: a
// COMPLAINT-category ticket IS a complaint (MiFID complaint-handling
// record); everything else is a SUPPORT ticket.
func typeFor(category string) string {
	if category == CategoryComplaint {
		return TypeComplaint
	}
	return TypeSupport
}

// queueFor routes the ticket: complaints and disputes go to the
// Compliance Officer queue; support tickets to the Support Agent queue.
func queueFor(ticketType string) string {
	if ticketType == TypeComplaint || ticketType == TypeDispute {
		return QueueCompliance
	}
	return QueueSupport
}

// rolesForQueue maps a queue to the admin roles allowed to mutate its
// tickets. Compliance-queue tickets are Compliance Officer / Super Admin
// only — Support Agents never touch complaints (MiFID complaint-handling
// segregation).
var rolesForQueue = map[string]map[string]bool{
	QueueSupport:    {"Support Agent": true, "Super Admin": true},
	QueueCompliance: {"Compliance Officer": true, "Super Admin": true},
}

// ---------------------------------------------------------------------------
// Service
// ---------------------------------------------------------------------------

// Service is the ticket workflow. All admin mutations run in one
// transaction: the row update + optional ticket_notes insert +
// admin_audit_log + audit_hash_chain link.
type Service struct {
	pool     *pgxpool.Pool
	resolver RoleResolver
	alerter  Alerter
}

// NewService wires the production service. resolver nil → admin ops fail
// closed UNAUTHORIZED_ROLE (Phase-07 RBAC seam); alerter nil → SLA
// breaches are logged-only (no paging).
func NewService(pool *pgxpool.Pool, resolver RoleResolver, alerter Alerter) *Service {
	return &Service{pool: pool, resolver: resolver, alerter: alerter}
}

// checkRole resolves the actor's role and fails closed when the seam is
// absent or the role is ineligible for the ticket's queue.
func (s *Service) checkRole(ctx context.Context, adminID int64, queue string) (string, error) {
	if adminID <= 0 {
		return "", excerrors.New("UNAUTHORIZED", "admin identity required")
	}
	if s.resolver == nil {
		return "", excerrors.New("UNAUTHORIZED_ROLE",
			"role resolver not configured (Phase-07 RBAC stub boundary)")
	}
	role, err := s.resolver(ctx, adminID)
	if err != nil {
		return "", excerrors.Wrap("INTERNAL_ERROR", "role lookup", err)
	}
	if !rolesForQueue[queue][role] {
		return "", excerrors.New("UNAUTHORIZED_ROLE",
			fmt.Sprintf("%s tickets require %s queue role (got %q)",
				queue, queue, role))
	}
	return role, nil
}

// scanTicket reads one ticket row.
const ticketCols = `id, account_id, type::text, category::text, priority::text,
	subject, body, status::text, queue, assignee_admin_id, origin_channel,
	sla_due_at, final_response_due_at, acknowledged_at, resolved_at,
	adr_requested,
	COALESCE(adr_scheme,''), COALESCE(adr_reference,''), adr_acknowledged_at,
	created_at, updated_at`

func scanTicket(row pgx.Row) (*Ticket, error) {
	var t Ticket
	err := row.Scan(&t.ID, &t.AccountID, &t.Type, &t.Category, &t.Priority,
		&t.Subject, &t.Body, &t.Status, &t.Queue, &t.AssigneeAdminID,
		&t.OriginChannel, &t.SLADueAt, &t.FinalResponseDueAt,
		&t.AcknowledgedAt, &t.ResolvedAt,
		&t.ADRRequested, &t.ADRScheme, &t.ADRReference, &t.ADRAckedAt,
		&t.CreatedAt, &t.UpdatedAt)
	if err != nil {
		return nil, err
	}
	return &t, nil
}

func (t *Ticket) markBreach(now time.Time) {
	unresolved := t.Status != StatusResolved && t.Status != StatusClosed
	t.SLABreached = unresolved && t.AcknowledgedAt == nil && now.After(t.SLADueAt)
	t.FinalSLABreached = unresolved && t.FinalResponseDueAt != nil &&
		now.After(*t.FinalResponseDueAt)
}

// ---------------------------------------------------------------------------
// Client surface
// ---------------------------------------------------------------------------

// Create validates and stores a client ticket. FROZEN accounts can still
// open tickets — a frozen client needs a channel to reach support (the
// freeze gates trading/funding, not support).
func (s *Service) Create(ctx context.Context, req CreateRequest) (*Ticket, error) {
	if req.AccountID <= 0 {
		return nil, excerrors.New("UNAUTHORIZED", "account context required")
	}
	if !ValidCategory(req.Category) {
		return nil, excerrors.New("INVALID_REQUEST",
			"category must be FUNDING|TRADING|KYC|TECHNICAL|COMPLAINT")
	}
	if subj := len(req.Subject); subj == 0 || subj > 255 {
		return nil, excerrors.New("INVALID_REQUEST", "subject is required (≤255 chars)")
	}
	priority := req.Priority
	if priority == "" {
		priority = PriorityNormal
	} else if !ValidPriority(priority) {
		return nil, excerrors.New("INVALID_REQUEST", "invalid priority")
	}
	channel := req.OriginChannel
	if channel == "" {
		channel = "PORTAL"
	} else if !OriginChannels[channel] {
		return nil, excerrors.New("INVALID_REQUEST",
			"origin_channel must be PORTAL|EMAIL|API|LETTER")
	}
	now := req.Now
	if now.IsZero() {
		now = time.Now().UTC()
	}
	typ := typeFor(req.Category)
	t := &Ticket{
		AccountID: req.AccountID, Type: typ, Category: req.Category,
		Priority: priority, Subject: req.Subject, Body: req.Body,
		Status: StatusOpen, Queue: queueFor(typ), OriginChannel: channel,
		SLADueAt:           slaFor(typ, now),
		FinalResponseDueAt: finalResponseDue(typ, now),
		CreatedAt:          now, UpdatedAt: now,
	}
	err := s.pool.QueryRow(ctx, `
		INSERT INTO support_tickets
		    (account_id, type, category, priority, subject, body, status,
		     queue, origin_channel, sla_due_at, final_response_due_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)
		RETURNING `+ticketCols,
		t.AccountID, t.Type, t.Category, t.Priority, t.Subject, t.Body,
		t.Status, t.Queue, t.OriginChannel, t.SLADueAt, t.FinalResponseDueAt).
		Scan(&t.ID, &t.AccountID, &t.Type, &t.Category, &t.Priority,
			&t.Subject, &t.Body, &t.Status, &t.Queue, &t.AssigneeAdminID,
			&t.OriginChannel, &t.SLADueAt, &t.FinalResponseDueAt,
			&t.AcknowledgedAt, &t.ResolvedAt,
			&t.ADRRequested, &t.ADRScheme, &t.ADRReference, &t.ADRAckedAt,
			&t.CreatedAt, &t.UpdatedAt)
	if err != nil {
		return nil, fmt.Errorf("ticket insert: %w", err)
	}
	return t, nil
}

// ListMine returns the account's tickets newest-first (keyset cursor).
func (s *Service) ListMine(ctx context.Context, accountID int64,
	after *struct {
		Time time.Time
		ID   int64
	}, limit int) ([]Ticket, error) {
	if accountID <= 0 {
		return nil, excerrors.New("UNAUTHORIZED", "account context required")
	}
	q := `SELECT ` + ticketCols + ` FROM support_tickets
	       WHERE account_id = $1`
	args := []any{accountID}
	if after != nil {
		q += ` AND (created_at, id) < ($2, $3)`
		args = append(args, after.Time, after.ID)
	}
	q += ` ORDER BY created_at DESC, id DESC LIMIT ` + fmt.Sprintf("%d", limit)
	return s.queryTickets(ctx, q, args...)
}

// GetMine returns one ticket only if the account owns it.
func (s *Service) GetMine(ctx context.Context, accountID, ticketID int64) (*Ticket, []Note, error) {
	t, err := s.get(ctx, ticketID)
	if err != nil {
		return nil, nil, err
	}
	if t.AccountID != accountID {
		// Foreign ticket → 404, not 403: do not confirm the id exists.
		return nil, nil, excerrors.New("TICKET_NOT_FOUND",
			fmt.Sprintf("ticket %d not found", ticketID))
	}
	notes, err := s.listNotes(ctx, ticketID, false)
	if err != nil {
		return nil, nil, err
	}
	return t, notes, nil
}

func (s *Service) get(ctx context.Context, id int64) (*Ticket, error) {
	t, err := scanTicket(s.pool.QueryRow(ctx,
		`SELECT `+ticketCols+` FROM support_tickets WHERE id = $1`, id))
	if err == pgx.ErrNoRows {
		return nil, excerrors.New("TICKET_NOT_FOUND",
			fmt.Sprintf("ticket %d not found", id))
	}
	if err != nil {
		return nil, fmt.Errorf("ticket read: %w", err)
	}
	return t, nil
}

func (s *Service) queryTickets(ctx context.Context, q string, args ...any) ([]Ticket, error) {
	rows, err := s.pool.Query(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("ticket query: %w", err)
	}
	defer rows.Close()
	out := []Ticket{}
	now := time.Now().UTC()
	for rows.Next() {
		var t Ticket
		if err := rows.Scan(&t.ID, &t.AccountID, &t.Type, &t.Category,
			&t.Priority, &t.Subject, &t.Body, &t.Status, &t.Queue,
			&t.AssigneeAdminID, &t.OriginChannel, &t.SLADueAt,
			&t.FinalResponseDueAt, &t.AcknowledgedAt, &t.ResolvedAt,
			&t.ADRRequested, &t.ADRScheme,
			&t.ADRReference, &t.ADRAckedAt, &t.CreatedAt, &t.UpdatedAt); err != nil {
			return nil, fmt.Errorf("ticket scan: %w", err)
		}
		t.markBreach(now)
		out = append(out, t)
	}
	return out, rows.Err()
}

func (s *Service) listNotes(ctx context.Context, ticketID int64, includeInternal bool) ([]Note, error) {
	q := `SELECT note_id, ticket_id, author_id, author_kind, internal, body, created_at
	        FROM ticket_notes WHERE ticket_id = $1`
	if !includeInternal {
		q += ` AND internal = false`
	}
	q += ` ORDER BY created_at, note_id`
	rows, err := s.pool.Query(ctx, q, ticketID)
	if err != nil {
		return nil, fmt.Errorf("ticket notes: %w", err)
	}
	defer rows.Close()
	out := []Note{}
	for rows.Next() {
		var n Note
		if err := rows.Scan(&n.NoteID, &n.TicketID, &n.AuthorID,
			&n.AuthorKind, &n.Internal, &n.Body, &n.CreatedAt); err != nil {
			return nil, fmt.Errorf("note scan: %w", err)
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

// ---------------------------------------------------------------------------
// Admin surface
// ---------------------------------------------------------------------------

// List runs the admin queue query with the filter set. Role gating:
// Support Agent sees the SUPPORT queue; Compliance Officer sees the
// COMPLIANCE queue; Super Admin and Read-Only Auditor see both (the
// auditor reads for the complaint register). Scope filtering (desk/
// region/currency) is the Phase-07 Task 7.3.11 middleware's job.
func (s *Service) List(ctx context.Context, adminID int64, f AdminFilter,
	after *struct {
		Time time.Time
		ID   int64
	}, limit int) ([]Ticket, error) {
	role, err := s.role(ctx, adminID)
	if err != nil {
		return nil, err
	}
	q := `SELECT ` + ticketCols + ` FROM support_tickets WHERE 1=1`
	args := []any{}
	next := func(v any) string { args = append(args, v); return fmt.Sprintf("$%d", len(args)) }

	// Queue confinement is hard: a role sees only the queues it can act
	// on (Support Agent never lists complaints). Auditor gets both,
	// read-only.
	switch {
	case role == "Super Admin" || role == "Read-Only Auditor":
		// both queues
	case rolesForQueue[QueueSupport][role]:
		q += ` AND queue = ` + next(QueueSupport)
	case rolesForQueue[QueueCompliance][role]:
		q += ` AND queue = ` + next(QueueCompliance)
	default:
		return nil, excerrors.New("UNAUTHORIZED_ROLE",
			fmt.Sprintf("role %q may not list support tickets", role))
	}
	if f.Queue != "" {
		q += ` AND queue = ` + next(f.Queue)
	}
	if f.Status != "" {
		if !ValidStatus(f.Status) {
			return nil, excerrors.New("INVALID_REQUEST", "invalid status filter")
		}
		q += ` AND status = ` + next(f.Status)
	}
	if f.Category != "" {
		q += ` AND category = ` + next(f.Category)
	}
	if f.Type != "" {
		q += ` AND type = ` + next(f.Type)
	}
	if f.AssigneeID != nil {
		q += ` AND assignee_admin_id = ` + next(*f.AssigneeID)
	}
	if f.Unassigned {
		q += ` AND assignee_admin_id IS NULL`
	}
	if f.AccountID != nil {
		q += ` AND account_id = ` + next(*f.AccountID)
	}
	if f.BreachedOnly {
		q += ` AND status NOT IN ('RESOLVED','CLOSED')
		       AND ((acknowledged_at IS NULL AND sla_due_at < ` + next(time.Now().UTC()) + `)
		         OR (final_response_due_at IS NOT NULL AND final_response_due_at < ` + next(time.Now().UTC()) + `))`
	}
	if after != nil {
		q += ` AND (created_at, id) < (` + next(after.Time) + ", " + next(after.ID) + ")"
	}
	q += ` ORDER BY created_at DESC, id DESC LIMIT ` + fmt.Sprintf("%d", limit)
	return s.queryTickets(ctx, q, args...)
}

// ComplaintRegister exports the MiFID complaint-handling register:
// every COMPLAINT/DISPUTE row — the system of record the ADR routing is
// additive to (spec §14.10.3).
func (s *Service) ComplaintRegister(ctx context.Context, adminID int64,
	after *struct {
		Time time.Time
		ID   int64
	}, limit int) ([]Ticket, error) {
	return s.List(ctx, adminID, AdminFilter{Queue: QueueCompliance}, after, limit)
}

// Get returns one ticket for an admin authorized for its queue.
func (s *Service) Get(ctx context.Context, adminID, ticketID int64) (*Ticket, []Note, error) {
	t, err := s.get(ctx, ticketID)
	if err != nil {
		return nil, nil, err
	}
	if _, err := s.checkRole(ctx, adminID, t.Queue); err != nil {
		return nil, nil, err
	}
	notes, err := s.listNotes(ctx, ticketID, true)
	if err != nil {
		return nil, nil, err
	}
	t.markBreach(time.Now().UTC())
	return t, notes, nil
}

func (s *Service) role(ctx context.Context, adminID int64) (string, error) {
	if adminID <= 0 {
		return "", excerrors.New("UNAUTHORIZED", "admin identity required")
	}
	if s.resolver == nil {
		return "", excerrors.New("UNAUTHORIZED_ROLE",
			"role resolver not configured (Phase-07 RBAC stub boundary)")
	}
	role, err := s.resolver(ctx, adminID)
	if err != nil {
		return "", excerrors.Wrap("INTERNAL_ERROR", "role lookup", err)
	}
	return role, nil
}

// Update applies the admin mutation in one transaction: row lock,
// transition validation, ticket update, optional internal note,
// admin_audit_log row + hash-chain link.
func (s *Service) Update(ctx context.Context, adminID int64, u AdminUpdate, clientIP string) (*Ticket, error) {
	if u.TicketID <= 0 {
		return nil, excerrors.New("INVALID_REQUEST", "ticket_id is required")
	}
	if u.Status == "" && u.AssigneeAdminID == nil && !u.AssignToMe &&
		u.Priority == "" && u.Note == "" && u.ADRRequested == nil &&
		u.ADRScheme == "" && u.ADRReference == "" && !u.ADRAck {
		return nil, excerrors.New("INVALID_REQUEST", "no mutation fields supplied")
	}
	if u.Status != "" && !ValidStatus(u.Status) {
		return nil, excerrors.New("INVALID_REQUEST", "invalid status")
	}
	if u.Priority != "" && !ValidPriority(u.Priority) {
		return nil, excerrors.New("INVALID_REQUEST", "invalid priority")
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	before, err := scanTicket(tx.QueryRow(ctx,
		`SELECT `+ticketCols+` FROM support_tickets WHERE id = $1 FOR UPDATE`,
		u.TicketID))
	if err == pgx.ErrNoRows {
		return nil, excerrors.New("TICKET_NOT_FOUND",
			fmt.Sprintf("ticket %d not found", u.TicketID))
	}
	if err != nil {
		return nil, fmt.Errorf("ticket lock: %w", err)
	}

	// Role gate on the ticket's queue.
	if _, err := s.checkRole(ctx, adminID, before.Queue); err != nil {
		return nil, err
	}

	now := time.Now().UTC()
	sets := []string{"updated_at = " + "now()"}
	args := []any{}
	next := func(v any) string { args = append(args, v); return fmt.Sprintf("$%d", len(args)) }

	if u.Status != "" && u.Status != before.Status {
		if !ValidTransition(before.Status, u.Status) {
			return nil, excerrors.New("INVALID_REQUEST",
				fmt.Sprintf("invalid status transition %s → %s", before.Status, u.Status))
		}
		sets = append(sets, "status = "+next(u.Status))
		// First staff touch on an OPEN ticket acknowledges it (the SLA
		// the complaint clock runs against).
		if before.AcknowledgedAt == nil && u.Status != StatusOpen {
			sets = append(sets, "acknowledged_at = "+next(now))
		}
		if u.Status == StatusResolved || u.Status == StatusClosed {
			sets = append(sets, "resolved_at = "+next(now))
		}
	}
	if u.AssignToMe {
		sets = append(sets, "assignee_admin_id = "+next(adminID))
	} else if u.AssigneeAdminID != nil {
		sets = append(sets, "assignee_admin_id = "+next(*u.AssigneeAdminID))
	}
	if u.Priority != "" {
		sets = append(sets, "priority = "+next(u.Priority))
	}

	// ADR routing applies to compliance-queue tickets only — support
	// tickets have no ombudsman path (spec §14.10.3).
	if u.ADRRequested != nil || u.ADRScheme != "" || u.ADRReference != "" || u.ADRAck {
		if before.Queue != QueueCompliance {
			return nil, excerrors.New("INVALID_REQUEST",
				"ADR routing applies to COMPLAINT/DISPUTE tickets only")
		}
		if u.ADRRequested != nil {
			sets = append(sets, "adr_requested = "+next(*u.ADRRequested))
		}
		if u.ADRScheme != "" {
			sets = append(sets, "adr_scheme = "+next(u.ADRScheme))
		}
		if u.ADRReference != "" {
			sets = append(sets, "adr_reference = "+next(u.ADRReference))
		}
		if u.ADRAck {
			sets = append(sets, "adr_acknowledged_at = "+next(now))
		}
	}

	after, err := scanTicket(tx.QueryRow(ctx, `
		UPDATE support_tickets SET `+joinCSV(sets)+`
		 WHERE id = `+next(u.TicketID)+`
		RETURNING `+ticketCols, args...))
	if err != nil {
		return nil, fmt.Errorf("ticket update: %w", err)
	}

	if u.Note != "" {
		if _, err := tx.Exec(ctx, `
			INSERT INTO ticket_notes (ticket_id, author_id, author_kind, internal, body)
			VALUES ($1, $2, 'ADMIN', true, $3)`, u.TicketID, adminID, u.Note); err != nil {
			return nil, fmt.Errorf("note insert: %w", err)
		}
	}

	// Audit row + hash-chain link in the same tx (spec §5.9/§5.40.3).
	if _, _, err := admin.Log(ctx, tx, admin.AuditEntry{
		AdminUserID: adminID,
		Action:      "support.ticket.update",
		TargetType:  "support_ticket",
		TargetID:    &u.TicketID,
		BeforeState: ticketAuditState(before),
		AfterState:  ticketAuditState(after),
		IPAddress:   clientIP,
	}); err != nil {
		return nil, fmt.Errorf("audit: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("commit: %w", err)
	}
	return after, nil
}

// AddNote appends an internal note — its own audited mutation.
func (s *Service) AddNote(ctx context.Context, adminID, ticketID int64, body string, internal bool, clientIP string) (*Note, error) {
	if body == "" {
		return nil, excerrors.New("INVALID_REQUEST", "note body is required")
	}
	// Role gate on the ticket's queue.
	t, err := s.get(ctx, ticketID)
	if err != nil {
		return nil, err
	}
	if _, err := s.checkRole(ctx, adminID, t.Queue); err != nil {
		return nil, err
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var n Note
	err = tx.QueryRow(ctx, `
		INSERT INTO ticket_notes (ticket_id, author_id, author_kind, internal, body)
		VALUES ($1, $2, 'ADMIN', $3, $4)
		RETURNING note_id, ticket_id, author_id, author_kind, internal, body, created_at`,
		ticketID, adminID, internal, body).
		Scan(&n.NoteID, &n.TicketID, &n.AuthorID, &n.AuthorKind, &n.Internal, &n.Body, &n.CreatedAt)
	if err != nil {
		return nil, fmt.Errorf("note insert: %w", err)
	}
	if _, _, err := admin.Log(ctx, tx, admin.AuditEntry{
		AdminUserID: adminID,
		Action:      "support.ticket.note",
		TargetType:  "support_ticket",
		TargetID:    &ticketID,
		AfterState:  map[string]any{"note_id": n.NoteID, "internal": internal},
		IPAddress:   clientIP,
	}); err != nil {
		return nil, fmt.Errorf("audit: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("commit: %w", err)
	}
	return &n, nil
}

// ticketAuditState is the compact before/after image written to
// admin_audit_log — status/assignee/ADR deltas, not the full body.
func ticketAuditState(t *Ticket) map[string]any {
	return map[string]any{
		"status":            t.Status,
		"priority":          t.Priority,
		"queue":             t.Queue,
		"assignee_admin_id": t.AssigneeAdminID,
		"adr_requested":     t.ADRRequested,
		"adr_scheme":        t.ADRScheme,
		"adr_reference":     t.ADRReference,
	}
}

func joinCSV(parts []string) string {
	out := ""
	for i, p := range parts {
		if i > 0 {
			out += ", "
		}
		out += p
	}
	return out
}

// ---------------------------------------------------------------------------
// SLA breach scan — Task 7.3.7 item 5 / §27.1 ADR row.
//
// Two clocks are swept:
//   - acknowledgment (sla_due_at, until acknowledged_at): support → P3
//     TICKET_SLA_BREACH; compliance queue → P2 COMPLAINT_SLA_BREACH;
//   - statutory final response (final_response_due_at, complaints only):
//     always P2 COMPLAINT_SLA_BREACH.
//
// The *_breach_flagged_at columns make each alert once-only per ticket
// per clock — the UPDATE ... WHERE flag IS NULL claim is atomic so
// concurrent sweepers can't double-page.
// ---------------------------------------------------------------------------

// breachKind identifies which deadline tripped.
type breachKind struct{ flagCol string }

var (
	ackBreach   = breachKind{flagCol: "ack_breach_flagged_at"}
	finalBreach = breachKind{flagCol: "final_breach_flagged_at"}
)

// claimBreaches atomically flags and returns the ticket ids newly
// breached on the given clock — already-flagged rows are skipped, so
// each alert fires exactly once per ticket per deadline.
func (s *Service) claimBreaches(ctx context.Context, kind breachKind,
	deadlineCol string, extraWhere string, now time.Time, limit int) ([]Ticket, error) {
	rows, err := s.pool.Query(ctx, `
		UPDATE support_tickets
		   SET `+kind.flagCol+` = $1, updated_at = now()
		 WHERE id IN (
		     SELECT id FROM support_tickets
		      WHERE `+kind.flagCol+` IS NULL
		        AND status NOT IN ('RESOLVED','CLOSED')
		        AND `+deadlineCol+` IS NOT NULL
		        AND `+deadlineCol+` < $1
		        `+extraWhere+`
		      ORDER BY `+deadlineCol+`
		      LIMIT $2
		      FOR UPDATE SKIP LOCKED)
		RETURNING `+ticketCols, now, limit)
	if err != nil {
		return nil, fmt.Errorf("breach claim: %w", err)
	}
	defer rows.Close()
	out := []Ticket{}
	for rows.Next() {
		var t Ticket
		if err := rows.Scan(&t.ID, &t.AccountID, &t.Type, &t.Category,
			&t.Priority, &t.Subject, &t.Body, &t.Status, &t.Queue,
			&t.AssigneeAdminID, &t.OriginChannel, &t.SLADueAt,
			&t.FinalResponseDueAt, &t.AcknowledgedAt, &t.ResolvedAt,
			&t.ADRRequested, &t.ADRScheme, &t.ADRReference, &t.ADRAckedAt,
			&t.CreatedAt, &t.UpdatedAt); err != nil {
			return nil, fmt.Errorf("breach scan: %w", err)
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// Breached returns unresolved tickets past their acknowledgment SLA
// (read-only report — no flagging; used by the admin ?breached=1 list
// filter path and tests).
func (s *Service) Breached(ctx context.Context, now time.Time, limit int) ([]Ticket, error) {
	return s.queryTickets(ctx, `
		SELECT `+ticketCols+` FROM support_tickets
		 WHERE status NOT IN ('RESOLVED','CLOSED')
		   AND ((acknowledged_at IS NULL AND sla_due_at < $1)
		     OR (final_response_due_at IS NOT NULL AND final_response_due_at < $1))
		 ORDER BY sla_due_at LIMIT `+fmt.Sprintf("%d", limit), now)
}

// SweepAlerts claims newly breached tickets on both clocks and raises
// one alert each through the Alerter. Returns the number of alerts
// raised. Call on a timer (the gateway runs it every 60s).
func (s *Service) SweepAlerts(ctx context.Context, now time.Time) (int, error) {
	raised := 0
	// Acknowledgment clock — skipped once acknowledged_at is set.
	ackRows, err := s.claimBreaches(ctx, ackBreach, "sla_due_at",
		"AND acknowledged_at IS NULL", now, 500)
	if err != nil {
		return 0, err
	}
	for _, t := range ackRows {
		sev, code := "P3", "TICKET_SLA_BREACH"
		if t.Queue == QueueCompliance {
			sev, code = "P2", "COMPLAINT_SLA_BREACH"
		}
		if s.alerter == nil {
			raised++
			continue
		}
		if err := s.alerter.Raise(ctx, sev, code,
			fmt.Sprintf("ticket %d (%s queue, account %d) acknowledgment SLA breached at %s",
				t.ID, t.Queue, t.AccountID, t.SLADueAt.Format(time.RFC3339))); err != nil {
			return raised, fmt.Errorf("alert dispatch: %w", err)
		}
		raised++
	}
	// Statutory final-response clock — complaints/disputes only.
	finalRows, err := s.claimBreaches(ctx, finalBreach, "final_response_due_at",
		"", now, 500)
	if err != nil {
		return raised, err
	}
	for _, t := range finalRows {
		if s.alerter == nil {
			raised++
			continue
		}
		if err := s.alerter.Raise(ctx, "P2", "COMPLAINT_SLA_BREACH",
			fmt.Sprintf("ticket %d (COMPLIANCE queue, account %d) statutory final-response deadline breached at %s",
				t.ID, t.AccountID, t.FinalResponseDueAt.Format(time.RFC3339))); err != nil {
			return raised, fmt.Errorf("alert dispatch: %w", err)
		}
		raised++
	}
	return raised, nil
}
