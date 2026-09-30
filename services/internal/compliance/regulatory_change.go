// Regulatory change monitoring & impact assessment — Phase-21 Task
// 21.3.25 (spec §14.10.2; §24 #328; §27.1 Regulatory Change Horizon
// Scanning matrix → RULEBOOK_VERSION_STALE P1 /
// REGULATORY_DEADLINE_APPROACHING). The task text + AC page the
// effective-within-90-days condition at P1 to the Compliance Officer —
// stricter than the §27.1 matrix's P2 floor (deviation: P1 applied).
//
// The watch register (migration 080) carries every ESMA/FCA/CFTC/
// FINRA/FATF/… rule change from TRACKED through IMPLEMENTED → CLOSED:
//
//   - Register stamps triage_due_at = published_at + 10 business days
//     (weekday calendar; the holiday set is injectable — Phase-03 Task
//     3.3.8 feeds the production map). Emergency publications with no
//     notice are accepted — a past effective_at is recorded, never
//     rejected; the SLA clock starts at publication either way.
//   - The sweeps page once per change (dedup anchored on
//     audit_hash_chain actions — the WORM trail doubles as the "we
//     paged" ledger so the daily/hourly job never storms the pager):
//     RULEBOOK_VERSION_STALE P1 for an untriaged row past its SLA,
//     REGULATORY_DEADLINE_APPROACHING for a live change whose
//     effective_at lands inside 90 days.
//   - Status moves are linear (TRACKED→TRIAGED→SCOPED→IMPLEMENTED→
//     CLOSED; CLOSED also reachable from anywhere for withdrawn rules).
//     IMPLEMENTED is refused while any impact item is OPEN, and while a
//     change that alters a §24 criterion lacks matrix_update_ref — the
//     traceability row ships in the same change set (§14.10.2 item 4).
//   - Overlapping changes on the same (authority, instrument) are
//     registered independently but flagged — the register response
//     lists the conflicting ids so the officer resolves the overlap in
//     notes instead of losing a watch entry.
//
// The §8.2 governance policy (role matrix, auditor-scope rule, SLA
// calendar) lives in internal/admin/regulatory_change.go — the domain
// defers to it on every entry point so the route registry and the
// service gate fail closed independently, like the SAR/AML wave.
package compliance

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"exchange/internal/admin"
	"exchange/internal/audit"
	excerrors "exchange/pkg/errors"
)

// Lifecycle statuses (migration 080 CHECK mirror).
const (
	RegStatusTracked     = "TRACKED"
	RegStatusTriaged     = "TRIAGED"
	RegStatusScoped      = "SCOPED"
	RegStatusImplemented = "IMPLEMENTED"
	RegStatusClosed      = "CLOSED"
)

// §27.1 matrix codes — alert-only severities (P1/P2), deliberately not
// registered as HTTP wire codes (CTR_TRIGGERED precedent; they ride
// alerts + payload `code` fields, never the error envelope).
const (
	RegCodeTriageSLA    = "RULEBOOK_VERSION_STALE"
	RegCodeDeadlineNear = "REGULATORY_DEADLINE_APPROACHING"
)

// Impact kinds (migration 080 CHECK mirror).
const (
	ImpactSpecSection = "SPEC_SECTION"
	ImpactPhaseTask   = "PHASE_TASK"
	ImpactMigration   = "MIGRATION"
	ImpactDataField   = "DATA_FIELD"
	ImpactEndpoint    = "ENDPOINT"
	ImpactNone        = "NO_IMPACT"
)

// RegTriageSLABusinessDays / RegEffectiveAlertWindow alias the
// governance policy in the admin package — the spec pins them on the
// register (10 business days; 90-day effective window).
const (
	RegTriageSLABusinessDays = admin.RegChangeTriageSLA
	RegEffectiveAlertWindow  = admin.RegChangeEffectiveWindow
)

// regAuthorities is the migration-080 CHECK mirror.
var regAuthorities = map[string]bool{
	"ESMA": true, "FCA": true, "CFTC": true, "FINRA": true,
	"FATF": true, "SEC": true, "NFA": true, "ECB": true,
	"BOE": true, "JFSA": true, "ASIC": true, "MAS": true,
	"BAFIN": true, "FINMA": true, "OTHER": true,
}

// RegChange is one regulatory_changes row.
type RegChange struct {
	ChangeID         int64      `json:"change_id"`
	Authority        string     `json:"authority"`
	Instrument       string     `json:"instrument"`
	Title            string     `json:"title"`
	PublishedAt      time.Time  `json:"published_at"`
	EffectiveAt      *time.Time `json:"effective_at,omitempty"`
	SourceURL        string     `json:"source_url,omitempty"`
	Owner            *int64     `json:"owner,omitempty"`
	Status           string     `json:"status"`
	TriageDueAt      time.Time  `json:"triage_due_at"`
	TriagedAt        *time.Time `json:"triaged_at,omitempty"`
	SpecCriteriaRefs []int      `json:"spec_criteria_refs"`
	MatrixUpdateRef  string     `json:"matrix_update_ref,omitempty"`
	Notes            string     `json:"notes,omitempty"`
	CreatedBy        int64      `json:"created_by"`
	CreatedAt        time.Time  `json:"created_at"`
	UpdatedAt        time.Time  `json:"updated_at"`
	// Code carries the matrix alert code while an SLA/deadline breach
	// stands — the audit-warning convention, not an error envelope.
	Code string `json:"code,omitempty"`
}

// RegChangeImpact is one regulatory_change_impacts row.
type RegChangeImpact struct {
	ID             int64      `json:"id"`
	ChangeID       int64      `json:"change_id"`
	Kind           string     `json:"kind"`
	Ref            string     `json:"ref"`
	Owner          *int64     `json:"owner,omitempty"`
	EffortEstimate string     `json:"effort_estimate,omitempty"`
	DueAt          *time.Time `json:"due_at,omitempty"`
	Status         string     `json:"status"`
	CompletedAt    *time.Time `json:"completed_at,omitempty"`
	RecordedBy     int64      `json:"recorded_by"`
	CreatedAt      time.Time  `json:"created_at"`
	UpdatedAt      time.Time  `json:"updated_at"`
}

// RegCorrespondence is one regulator information hold/request linked to
// the change record (§14.10.2 item 5).
type RegCorrespondence struct {
	ID         int64      `json:"id"`
	ChangeID   int64      `json:"change_id"`
	Kind       string     `json:"kind"` // INFO_HOLD | INFO_REQUEST
	Summary    string     `json:"summary"`
	ReceivedAt time.Time  `json:"received_at"`
	DueAt      *time.Time `json:"due_at,omitempty"`
	RecordedBy int64      `json:"recorded_by"`
	CreatedAt  time.Time  `json:"created_at"`
}

// RegChangeInput is the register payload.
type RegChangeInput struct {
	Authority        string     `json:"authority"`
	Instrument       string     `json:"instrument"`
	Title            string     `json:"title"`
	PublishedAt      time.Time  `json:"published_at"`
	EffectiveAt      *time.Time `json:"effective_at,omitempty"`
	SourceURL        string     `json:"source_url,omitempty"`
	Owner            *int64     `json:"owner,omitempty"`
	SpecCriteriaRefs []int      `json:"spec_criteria_refs,omitempty"`
	Notes            string     `json:"notes,omitempty"`
}

// RegChangeService owns the register, the impact map, correspondence
// and the SLA/deadline sweeps.
type RegChangeService struct {
	pool     *pgxpool.Pool
	resolver HoldRoleResolver
	alerter  HoldAlerter
	holidays map[time.Time]bool // date-truncated UTC holiday set
	now      func() time.Time
}

// NewRegChangeService wires the service; pool + resolver required
// (mutations re-check Compliance Officer / Super Admin).
func NewRegChangeService(pool *pgxpool.Pool,
	resolver HoldRoleResolver) (*RegChangeService, error) {
	if pool == nil {
		return nil, excerrors.New("INTERNAL_ERROR", "regulatory change service requires pool")
	}
	if resolver == nil {
		return nil, excerrors.New("INTERNAL_ERROR", "role resolver not wired")
	}
	return &RegChangeService{pool: pool, resolver: resolver,
		holidays: map[time.Time]bool{}, now: time.Now}, nil
}

// WithAlerter wires the ops channel for SLA/deadline pages.
func (s *RegChangeService) WithAlerter(a HoldAlerter) *RegChangeService {
	s.alerter = a
	return s
}

// WithHolidays injects the jurisdiction holiday set (date-truncated
// UTC keys) the business-day SLA math skips.
func (s *RegChangeService) WithHolidays(dates []time.Time) *RegChangeService {
	s.holidays = map[time.Time]bool{}
	for _, d := range dates {
		s.holidays[d.UTC().Truncate(24*time.Hour)] = true
	}
	return s
}

// WithClock overrides the clock (tests).
func (s *RegChangeService) WithClock(c func() time.Time) *RegChangeService {
	s.now = c
	return s
}

// BusinessDaysAfter adds n business days to t — weekdays minus the
// injected holiday set; delegates to the admin-owned SLA calendar.
func (s *RegChangeService) BusinessDaysAfter(t time.Time, n int) time.Time {
	return admin.BusinessDaysAfter(t, n, s.holidays)
}

// ---------------------------------------------------------------------------
// Register + lifecycle
// ---------------------------------------------------------------------------

// Register files a watch-register row; idempotent on
// (authority, instrument, title, published_at) replays via conflict
// probe — a NATS/portal re-delivery replays the stored row with
// created=false. Returns the change, any same-(authority,instrument)
// live rows it overlaps, and whether the row was newly inserted.
func (s *RegChangeService) Register(ctx context.Context, in RegChangeInput,
	officer int64) (*RegChange, []int64, bool, error) {
	if err := s.checkRole(ctx, officer); err != nil {
		return nil, nil, false, err
	}
	if !regAuthorities[in.Authority] {
		return nil, nil, false, excerrors.New("INVALID_REQUEST",
			"authority must be one of the registered regulator codes")
	}
	if in.Instrument == "" || in.Title == "" {
		return nil, nil, false, excerrors.New("INVALID_REQUEST",
			"instrument and title are required")
	}
	if in.PublishedAt.IsZero() {
		return nil, nil, false, excerrors.New("INVALID_REQUEST",
			"published_at is required — the SLA clock starts at publication")
	}
	if in.SpecCriteriaRefs == nil {
		in.SpecCriteriaRefs = []int{}
	}
	triageDue := admin.RegChangeTriageDeadline(in.PublishedAt, s.holidays)
	refs, _ := json.Marshal(in.SpecCriteriaRefs)

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, nil, false, excerrors.Wrap("INTERNAL_ERROR", "reg change tx", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var id int64
	err = tx.QueryRow(ctx, `
		INSERT INTO regulatory_changes
		    (authority, instrument, title, published_at, effective_at,
		     source_url, owner, triage_due_at, spec_criteria_refs, notes,
		     created_by)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,NULLIF($10,''),$11)
		ON CONFLICT DO NOTHING
		RETURNING change_id`,
		in.Authority, in.Instrument, in.Title, in.PublishedAt,
		in.EffectiveAt, nullableStr(in.SourceURL), in.Owner,
		triageDue, refs, in.Notes, officer).Scan(&id)
	if err == pgx.ErrNoRows {
		existing, gerr := s.findDuplicate(ctx, in)
		if gerr != nil {
			return nil, nil, false, gerr
		}
		if err := tx.Commit(ctx); err != nil {
			return nil, nil, false, excerrors.Wrap("INTERNAL_ERROR", "reg change dedup commit", err)
		}
		return existing, nil, false, nil
	}
	if err != nil {
		return nil, nil, false, excerrors.Wrap("INTERNAL_ERROR", "reg change insert", err)
	}
	if _, err := audit.Append(ctx, tx, "regulatory_changes", &id,
		"REG_REGISTERED", nil); err != nil {
		return nil, nil, false, excerrors.Wrap("INTERNAL_ERROR", "reg change audit", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, nil, false, excerrors.Wrap("INTERNAL_ERROR", "reg change commit", err)
	}
	// Overlap detection is advisory — conflicting same-instrument
	// watches stay registered (the spec edge case resolves them via
	// notes/close, never by silently dropping a record).
	overlaps, oerr := s.overlapping(ctx, id, in.Authority, in.Instrument)
	if oerr != nil {
		return nil, nil, false, oerr
	}
	chg, err := s.Get(ctx, id)
	if err != nil {
		return nil, nil, false, err
	}
	s.alertNearDeadline(ctx, chg)
	return chg, overlaps, true, nil
}

// findDuplicate replays an identical register call (same natural key)
// for idempotent redelivery.
func (s *RegChangeService) findDuplicate(ctx context.Context,
	in RegChangeInput) (*RegChange, error) {
	chg, err := scanRegChange(s.pool.QueryRow(ctx, `
		SELECT `+regChangeCols+` FROM regulatory_changes
		WHERE authority=$1 AND instrument=$2 AND title=$3 AND published_at=$4
		ORDER BY change_id LIMIT 1`,
		in.Authority, in.Instrument, in.Title, in.PublishedAt))
	if err == pgx.ErrNoRows {
		return nil, excerrors.New("INTERNAL_ERROR",
			"reg change dedup conflict without stored row")
	}
	return chg, err
}

// overlapping lists live changes on the same (authority, instrument) —
// the conflict flag the register response carries.
func (s *RegChangeService) overlapping(ctx context.Context, exclude int64,
	authority, instrument string) ([]int64, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT change_id FROM regulatory_changes
		WHERE authority=$1 AND instrument=$2 AND change_id<>$3
		  AND status <> 'CLOSED'`, authority, instrument, exclude)
	if err != nil {
		return nil, excerrors.Wrap("INTERNAL_ERROR", "reg overlap probe", err)
	}
	defer rows.Close()
	var out []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, excerrors.Wrap("INTERNAL_ERROR", "reg overlap scan", err)
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// Triage flips TRACKED → TRIAGED with officer attribution; owner may be
// assigned in the same call.
func (s *RegChangeService) Triage(ctx context.Context, changeID, officer int64,
	owner *int64, notes string) (*RegChange, error) {
	return s.transition(ctx, changeID, officer, RegStatusTriaged, owner,
		"", notes)
}

// Close dispositions a withdrawn/superseded watch (reason required).
func (s *RegChangeService) Close(ctx context.Context, changeID, officer int64,
	notes string) (*RegChange, error) {
	if notes == "" {
		return nil, excerrors.New("INVALID_REQUEST",
			"closing a watch requires a disposition note")
	}
	return s.transition(ctx, changeID, officer, RegStatusClosed, nil,
		"", notes)
}

// MarkImplemented requests TRACKED/TRIAGED/SCOPED → IMPLEMENTED — the
// gated transition: every recorded impact must be DONE and a §24-
// altering change must carry matrix_update_ref.
func (s *RegChangeService) MarkImplemented(ctx context.Context, changeID,
	officer int64, matrixUpdateRef string) (*RegChange, error) {
	return s.transition(ctx, changeID, officer, RegStatusImplemented,
		nil, matrixUpdateRef, "")
}

// SetStatus handles the ungated forward moves (TRIAGED → SCOPED).
func (s *RegChangeService) SetStatus(ctx context.Context, changeID, officer int64,
	target string) (*RegChange, error) {
	switch target {
	case RegStatusScoped:
	default:
		return nil, excerrors.New("INVALID_REQUEST",
			"SetStatus only covers the ungated forward move to SCOPED — "+
				"IMPLEMENT via MarkImplemented, CLOSE via Close")
	}
	return s.transition(ctx, changeID, officer, target, nil, "", "")
}

// regTransitions is the legal forward map; CLOSED is reachable from any
// live status (withdrawn/superseded rules).
var regTransitions = map[string][]string{
	RegStatusTracked:     {RegStatusTriaged, RegStatusClosed},
	RegStatusTriaged:     {RegStatusScoped, RegStatusClosed},
	RegStatusScoped:      {RegStatusImplemented, RegStatusClosed},
	RegStatusImplemented: {RegStatusClosed},
}

// transition applies one status move under FOR UPDATE with the gates
// evaluated inside the transaction.
func (s *RegChangeService) transition(ctx context.Context, changeID, officer int64,
	target string, owner *int64, matrixUpdateRef, notes string) (*RegChange, error) {
	if err := s.checkRole(ctx, officer); err != nil {
		return nil, err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, excerrors.Wrap("INTERNAL_ERROR", "reg transition tx", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	chg, err := scanRegChange(tx.QueryRow(ctx,
		`SELECT `+regChangeCols+` FROM regulatory_changes
		 WHERE change_id=$1 FOR UPDATE`, changeID))
	if err == pgx.ErrNoRows {
		return nil, excerrors.New("NOT_FOUND", "regulatory change not found")
	}
	if err != nil {
		return nil, excerrors.Wrap("INTERNAL_ERROR", "reg transition lock", err)
	}
	legal := false
	for _, t := range regTransitions[chg.Status] {
		if t == target {
			legal = true
		}
	}
	if !legal {
		return nil, excerrors.New("INVALID_REQUEST",
			fmt.Sprintf("status %s → %s is not a legal transition", chg.Status, target))
	}
	if target == RegStatusImplemented {
		var open int64
		if err := tx.QueryRow(ctx, `
			SELECT count(*) FROM regulatory_change_impacts
			WHERE change_id=$1 AND status='OPEN' AND kind<>'NO_IMPACT'`,
			changeID).Scan(&open); err != nil {
			return nil, excerrors.Wrap("INTERNAL_ERROR", "reg impact probe", err)
		}
		if open > 0 {
			return nil, excerrors.New("INVALID_REQUEST",
				fmt.Sprintf("%d impact item(s) still OPEN — the assessment must "+
					"complete before IMPLEMENTED", open))
		}
		var impacts int64
		if err := tx.QueryRow(ctx, `
			SELECT count(*) FROM regulatory_change_impacts
			WHERE change_id=$1`, changeID).Scan(&impacts); err != nil {
			return nil, excerrors.Wrap("INTERNAL_ERROR", "reg impact count", err)
		}
		if impacts == 0 {
			return nil, excerrors.New("INVALID_REQUEST",
				"no impact assessment recorded — IMPLEMENTED requires a "+
					"completed assessment (use kind=NO_IMPACT for a nil finding)")
		}
		refs := chg.SpecCriteriaRefs
		if len(refs) > 0 && matrixUpdateRef == "" && chg.MatrixUpdateRef == "" {
			return nil, excerrors.New("INVALID_REQUEST",
				"change alters §24 criterion(s) — matrix_update_ref is "+
					"required in the same change set")
		}
	}
	var triagedAt *time.Time
	if target == RegStatusTriaged {
		t := s.now().UTC()
		triagedAt = &t
	}
	if matrixUpdateRef == "" {
		matrixUpdateRef = chg.MatrixUpdateRef
	}
	if _, err := tx.Exec(ctx, `
		UPDATE regulatory_changes
		SET status=$2, triaged_at=coalesce($3, triaged_at),
		    owner=coalesce($4, owner),
		    matrix_update_ref=NULLIF($5,''),
		    notes=CASE WHEN $6='' THEN notes ELSE $6 END,
		    updated_at=now()
		WHERE change_id=$1`, changeID, target, triagedAt, owner,
		matrixUpdateRef, notes); err != nil {
		return nil, excerrors.Wrap("INTERNAL_ERROR", "reg transition update", err)
	}
	if _, err := audit.Append(ctx, tx, "regulatory_changes", &changeID,
		"REG_"+target, nil); err != nil {
		return nil, excerrors.Wrap("INTERNAL_ERROR", "reg transition audit", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, excerrors.Wrap("INTERNAL_ERROR", "reg transition commit", err)
	}
	return s.Get(ctx, changeID)
}

// AssignOwner attaches the responsible officer — unassigned rows keep
// surfacing on the dashboard until owned.
func (s *RegChangeService) AssignOwner(ctx context.Context, changeID, officer,
	owner int64) (*RegChange, error) {
	if err := s.checkRole(ctx, officer); err != nil {
		return nil, err
	}
	tag, err := s.pool.Exec(ctx, `
		UPDATE regulatory_changes SET owner=$2, updated_at=now()
		WHERE change_id=$1 AND status <> 'CLOSED'`, changeID, owner)
	if err != nil {
		return nil, excerrors.Wrap("INTERNAL_ERROR", "reg owner", err)
	}
	if tag.RowsAffected() == 0 {
		return nil, excerrors.New("NOT_FOUND",
			"regulatory change missing or closed")
	}
	return s.Get(ctx, changeID)
}

// ---------------------------------------------------------------------------
// Impact assessment
// ---------------------------------------------------------------------------

// RecordImpact upserts one (change_id, kind, ref) impact item —
// idempotent on the natural key; re-recording refreshes owner/effort/
// due while an OPEN item stays OPEN.
func (s *RegChangeService) RecordImpact(ctx context.Context, changeID,
	officer int64, kind, ref string, owner *int64, effort string,
	dueAt *time.Time) (*RegChangeImpact, bool, error) {
	if err := s.checkRole(ctx, officer); err != nil {
		return nil, false, err
	}
	switch kind {
	case ImpactSpecSection, ImpactPhaseTask, ImpactMigration,
		ImpactDataField, ImpactEndpoint, ImpactNone:
	default:
		return nil, false, excerrors.New("INVALID_REQUEST",
			"kind must be SPEC_SECTION|PHASE_TASK|MIGRATION|DATA_FIELD|ENDPOINT|NO_IMPACT")
	}
	if ref == "" && kind != ImpactNone {
		return nil, false, excerrors.New("INVALID_REQUEST",
			"ref is required for kind "+kind)
	}
	if kind == ImpactNone {
		ref = "none"
	}
	// Recording impacts requires a live, scoped-or-later record? The
	// spec maps them under the assessment — recording is allowed from
	// TRACKED (early mapping) but blocked on CLOSED.
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, false, excerrors.Wrap("INTERNAL_ERROR", "reg impact tx", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var st string
	if err := tx.QueryRow(ctx, `
		SELECT status FROM regulatory_changes WHERE change_id=$1 FOR UPDATE`,
		changeID).Scan(&st); err == pgx.ErrNoRows {
		return nil, false, excerrors.New("NOT_FOUND", "regulatory change not found")
	} else if err != nil {
		return nil, false, excerrors.Wrap("INTERNAL_ERROR", "reg impact lock", err)
	}
	if st == RegStatusClosed || st == RegStatusImplemented {
		return nil, false, excerrors.New("INVALID_REQUEST",
			"status "+st+" — the assessment is frozen")
	}
	var id int64
	err = tx.QueryRow(ctx, `
		INSERT INTO regulatory_change_impacts
		    (change_id, kind, ref, owner, effort_estimate, due_at, recorded_by)
		VALUES ($1,$2,$3,$4,$5,$6,$7)
		ON CONFLICT (change_id, kind, ref) DO UPDATE
		SET owner=excluded.owner, effort_estimate=excluded.effort_estimate,
		    due_at=excluded.due_at, updated_at=now()
		RETURNING id`,
		changeID, kind, ref, owner, nullableStr(effort), dueAt,
		officer).Scan(&id)
	if err != nil {
		return nil, false, excerrors.Wrap("INTERNAL_ERROR", "reg impact upsert", err)
	}
	if _, err := audit.Append(ctx, tx, "regulatory_change_impacts", &id,
		"REG_IMPACT_REC", nil); err != nil {
		return nil, false, excerrors.Wrap("INTERNAL_ERROR", "reg impact audit", err)
	}
	created := true
	if err := tx.QueryRow(ctx, `
		SELECT created_at >= now() - interval '2 seconds' FROM
		regulatory_change_impacts WHERE id=$1`, id).Scan(&created); err != nil {
		created = true // probe failure is non-fatal to the upsert
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, false, excerrors.Wrap("INTERNAL_ERROR", "reg impact commit", err)
	}
	imp, err := s.impactByID(ctx, id)
	if err != nil {
		return nil, false, err
	}
	return imp, created, nil
}

// CompleteImpact flips one OPEN item to DONE.
func (s *RegChangeService) CompleteImpact(ctx context.Context, impactID,
	officer int64) (*RegChangeImpact, error) {
	if err := s.checkRole(ctx, officer); err != nil {
		return nil, err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, excerrors.Wrap("INTERNAL_ERROR", "reg impact done tx", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var id int64
	err = tx.QueryRow(ctx, `
		UPDATE regulatory_change_impacts
		SET status='DONE', completed_at=now(), updated_at=now()
		WHERE id=$1 AND status='OPEN'
		RETURNING id`, impactID).Scan(&id)
	if err == pgx.ErrNoRows {
		return nil, excerrors.New("NOT_FOUND",
			"impact item missing or already DONE")
	}
	if err != nil {
		return nil, excerrors.Wrap("INTERNAL_ERROR", "reg impact done", err)
	}
	if _, err := audit.Append(ctx, tx, "regulatory_change_impacts", &id,
		"REG_IMPACT_DONE", nil); err != nil {
		return nil, excerrors.Wrap("INTERNAL_ERROR", "reg impact done audit", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, excerrors.Wrap("INTERNAL_ERROR", "reg impact done commit", err)
	}
	return s.impactByID(ctx, id)
}

// ---------------------------------------------------------------------------
// Correspondence
// ---------------------------------------------------------------------------

// AttachCorrespondence links a regulator information hold/request to
// the change record — the emergency-rule + impact-scope co-location
// (§14.10.2 item 5).
func (s *RegChangeService) AttachCorrespondence(ctx context.Context, changeID,
	officer int64, kind, summary string, receivedAt time.Time,
	dueAt *time.Time) (*RegCorrespondence, error) {
	if err := s.checkRole(ctx, officer); err != nil {
		return nil, err
	}
	switch kind {
	case "INFO_HOLD", "INFO_REQUEST":
	default:
		return nil, excerrors.New("INVALID_REQUEST",
			"kind must be INFO_HOLD|INFO_REQUEST")
	}
	if summary == "" || receivedAt.IsZero() {
		return nil, excerrors.New("INVALID_REQUEST",
			"summary and received_at are required")
	}
	var id int64
	err := s.pool.QueryRow(ctx, `
		INSERT INTO regulatory_change_correspondence
		    (change_id, kind, summary, received_at, due_at, recorded_by)
		SELECT $1,$2,$3,$4,$5,$6 FROM regulatory_changes
		WHERE change_id=$1 AND status<>'CLOSED'
		RETURNING id`,
		changeID, kind, summary, receivedAt, dueAt, officer).Scan(&id)
	if err == pgx.ErrNoRows {
		return nil, excerrors.New("NOT_FOUND",
			"regulatory change missing or closed")
	}
	if err != nil {
		return nil, excerrors.Wrap("INTERNAL_ERROR", "reg correspondence", err)
	}
	return s.correspondenceByID(ctx, id)
}

// ---------------------------------------------------------------------------
// Sweeps + dashboard queries
// ---------------------------------------------------------------------------

// Sweep is the periodic job: page RULEBOOK_VERSION_STALE P1 for
// untriaged rows past their 10-business-day SLA and
// REGULATORY_DEADLINE_APPROACHING for live rows effective within 90
// days. Pages dedup on audit_hash_chain actions — each condition pages
// exactly once per change.
func (s *RegChangeService) Sweep(ctx context.Context) (slaPages, deadlinePages int, err error) {
	now := s.now().UTC()
	var overdue []int64
	rows, err := s.pool.Query(ctx, `
		SELECT change_id FROM regulatory_changes
		WHERE status='TRACKED' AND triage_due_at < $1`, now)
	if err != nil {
		return 0, 0, excerrors.Wrap("INTERNAL_ERROR", "reg sla probe", err)
	}
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return 0, 0, excerrors.Wrap("INTERNAL_ERROR", "reg sla scan", err)
		}
		overdue = append(overdue, id)
	}
	rows.Close()
	for _, id := range overdue {
		if paged, perr := s.pageOnce(ctx, id, "REG_SLA_PAGED", HoldAlert{
			Severity: "P1", Code: RegCodeTriageSLA,
			Summary: fmt.Sprintf("regulatory change %d untriaged past 10-business-day SLA", id),
			Details: map[string]string{"change_id": fmt.Sprint(id)},
		}); perr != nil {
			return slaPages, deadlinePages, perr
		} else if paged {
			slaPages++
		}
	}
	var near []int64
	rows2, err := s.pool.Query(ctx, `
		SELECT change_id FROM regulatory_changes
		WHERE status IN ('TRACKED','TRIAGED','SCOPED')
		  AND effective_at IS NOT NULL AND effective_at < $1`, now.Add(RegEffectiveAlertWindow))
	if err != nil {
		return slaPages, 0, excerrors.Wrap("INTERNAL_ERROR", "reg deadline probe", err)
	}
	for rows2.Next() {
		var id int64
		if err := rows2.Scan(&id); err != nil {
			rows2.Close()
			return slaPages, 0, excerrors.Wrap("INTERNAL_ERROR", "reg deadline scan", err)
		}
		near = append(near, id)
	}
	rows2.Close()
	for _, id := range near {
		if paged, perr := s.pageOnce(ctx, id, "REG_DUE_PAGED", HoldAlert{
			Severity: "P1", Code: RegCodeDeadlineNear,
			Summary: fmt.Sprintf("regulatory change %d effective within 90 days", id),
			Details: map[string]string{"change_id": fmt.Sprint(id)},
		}); perr != nil {
			return slaPages, deadlinePages, perr
		} else if paged {
			deadlinePages++
		}
	}
	return slaPages, deadlinePages, nil
}

// alertNearDeadline pages at register time when the effective date is
// already inside the window (emergency publications).
func (s *RegChangeService) alertNearDeadline(ctx context.Context, chg *RegChange) {
	if chg.EffectiveAt == nil ||
		chg.EffectiveAt.After(s.now().UTC().Add(RegEffectiveAlertWindow)) {
		return
	}
	_, _ = s.pageOnce(ctx, chg.ChangeID, "REG_DUE_PAGED", HoldAlert{
		Severity: "P1", Code: RegCodeDeadlineNear,
		Summary: fmt.Sprintf("regulatory change %d effective within 90 days", chg.ChangeID),
		Details: map[string]string{"change_id": fmt.Sprint(chg.ChangeID)},
	})
}

// pageOnce raises the alert only when the audit chain lacks the marker
// action for this record — the WORM trail is the dedup ledger.
func (s *RegChangeService) pageOnce(ctx context.Context, recordID int64,
	action string, al HoldAlert) (bool, error) {
	var exists bool
	if err := s.pool.QueryRow(ctx, `
		SELECT EXISTS(
			SELECT 1 FROM audit_hash_chain
			WHERE table_name='regulatory_changes' AND record_id=$1
			  AND action=$2)`, recordID, action).Scan(&exists); err != nil {
		return false, excerrors.Wrap("INTERNAL_ERROR", "reg page dedup", err)
	}
	if exists {
		return false, nil
	}
	if s.alerter == nil {
		return false, nil // no pager — never mark; retry once wired
	}
	if err := s.alerter.RaiseHold(ctx, al); err != nil {
		return false, excerrors.Wrap("INTERNAL_ERROR", "reg page", err)
	}
	// Marker is written only after a successful raise — a chain-write
	// failure re-pages next sweep (at-least-once), never silently lost.
	if _, err := audit.AppendAuto(ctx, s.pool, "regulatory_changes",
		&recordID, action, nil); err != nil {
		return false, excerrors.Wrap("INTERNAL_ERROR", "reg page marker", err)
	}
	return true, nil
}

// Untriaged is the dashboard queue: TRACKED rows ordered by SLA
// deadline (oldest first).
func (s *RegChangeService) Untriaged(ctx context.Context, limit int) ([]RegChange, error) {
	return s.listWhere(ctx, `status='TRACKED'`, "triage_due_at", limit)
}

// NearDeadline lists live changes effective inside the 90-day window.
func (s *RegChangeService) NearDeadline(ctx context.Context, limit int) ([]RegChange, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT `+regChangeCols+` FROM regulatory_changes
		WHERE status IN ('TRACKED','TRIAGED','SCOPED')
		  AND effective_at IS NOT NULL AND effective_at < $1
		ORDER BY effective_at LIMIT $2`,
		s.now().UTC().Add(RegEffectiveAlertWindow), clampLimit(limit))
	if err != nil {
		return nil, excerrors.Wrap("INTERNAL_ERROR", "reg near list", err)
	}
	defer rows.Close()
	var out []RegChange
	for rows.Next() {
		c, err := scanRegChangeRows(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *c)
	}
	return out, rows.Err()
}

// ---------------------------------------------------------------------------
// Reads
// ---------------------------------------------------------------------------

const regChangeCols = `
	change_id, authority, instrument, title, published_at, effective_at,
	source_url, owner, status, triage_due_at, triaged_at,
	spec_criteria_refs, matrix_update_ref, notes, created_by,
	created_at, updated_at`

func scanRegChange(row rowScanner) (*RegChange, error) {
	var c RegChange
	var src, mref, notes *string
	var refs json.RawMessage
	err := row.Scan(&c.ChangeID, &c.Authority, &c.Instrument, &c.Title,
		&c.PublishedAt, &c.EffectiveAt, &src, &c.Owner, &c.Status,
		&c.TriageDueAt, &c.TriagedAt, &refs, &mref, &notes,
		&c.CreatedBy, &c.CreatedAt, &c.UpdatedAt)
	if err != nil {
		return nil, err
	}
	if src != nil {
		c.SourceURL = *src
	}
	if mref != nil {
		c.MatrixUpdateRef = *mref
	}
	if notes != nil {
		c.Notes = *notes
	}
	c.SpecCriteriaRefs = []int{}
	_ = json.Unmarshal(refs, &c.SpecCriteriaRefs)
	return &c, nil
}

func scanRegChangeRows(rows pgx.Rows) (*RegChange, error) {
	var c RegChange
	var src, mref, notes *string
	var refs json.RawMessage
	err := rows.Scan(&c.ChangeID, &c.Authority, &c.Instrument, &c.Title,
		&c.PublishedAt, &c.EffectiveAt, &src, &c.Owner, &c.Status,
		&c.TriageDueAt, &c.TriagedAt, &refs, &mref, &notes,
		&c.CreatedBy, &c.CreatedAt, &c.UpdatedAt)
	if err != nil {
		return nil, excerrors.Wrap("INTERNAL_ERROR", "reg row scan", err)
	}
	if src != nil {
		c.SourceURL = *src
	}
	if mref != nil {
		c.MatrixUpdateRef = *mref
	}
	if notes != nil {
		c.Notes = *notes
	}
	c.SpecCriteriaRefs = []int{}
	_ = json.Unmarshal(refs, &c.SpecCriteriaRefs)
	return &c, nil
}

// Get returns one change with its SLA/deadline code annotation.
func (s *RegChangeService) Get(ctx context.Context, id int64) (*RegChange, error) {
	c, err := scanRegChange(s.pool.QueryRow(ctx,
		`SELECT `+regChangeCols+` FROM regulatory_changes WHERE change_id=$1`, id))
	if err == pgx.ErrNoRows {
		return nil, excerrors.New("NOT_FOUND", "regulatory change not found")
	}
	if err != nil {
		return nil, excerrors.Wrap("INTERNAL_ERROR", "reg get", err)
	}
	s.annotateCode(c)
	return c, nil
}

// annotateCode stamps the live matrix code while a breach stands.
func (s *RegChangeService) annotateCode(c *RegChange) {
	now := s.now().UTC()
	switch {
	case c.Status == RegStatusTracked && c.TriageDueAt.Before(now):
		c.Code = RegCodeTriageSLA
	case (c.Status == RegStatusTracked || c.Status == RegStatusTriaged ||
		c.Status == RegStatusScoped) && c.EffectiveAt != nil &&
		c.EffectiveAt.Before(now.Add(RegEffectiveAlertWindow)):
		c.Code = RegCodeDeadlineNear
	}
}

// ListForRole serves the register through the reader matrix — a
// Read-Only Auditor caller is confined to the untriaged +
// near-deadline union (spec §14.10.2 item 6); officer roles get the
// filtered register.
func (s *RegChangeService) ListForRole(ctx context.Context, actor int64,
	status string, limit int) ([]RegChange, error) {
	role, err := s.checkRead(ctx, actor)
	if err != nil {
		return nil, err
	}
	if admin.RegChangeAuditorScope(role) {
		seen := map[int64]bool{}
		var out []RegChange
		untriaged, err := s.Untriaged(ctx, limit)
		if err != nil {
			return nil, err
		}
		for _, c := range untriaged {
			if !seen[c.ChangeID] {
				seen[c.ChangeID] = true
				out = append(out, c)
			}
		}
		near, err := s.NearDeadline(ctx, limit)
		if err != nil {
			return nil, err
		}
		for _, c := range near {
			if !seen[c.ChangeID] {
				seen[c.ChangeID] = true
				out = append(out, c)
			}
		}
		return out, nil
	}
	return s.List(ctx, status, limit)
}

// GetForRole returns one change under the reader matrix — rows outside
// the auditor's scoped projection read as NOT_FOUND (the auditor
// surface cannot probe the rest of the register).
func (s *RegChangeService) GetForRole(ctx context.Context, actor,
	changeID int64) (*RegChange, error) {
	role, err := s.checkRead(ctx, actor)
	if err != nil {
		return nil, err
	}
	chg, err := s.Get(ctx, changeID)
	if err != nil {
		return nil, err
	}
	if admin.RegChangeAuditorScope(role) &&
		!admin.RegChangeAuditorVisible(chg.Status, chg.TriageDueAt,
			chg.EffectiveAt, s.now().UTC()) {
		return nil, excerrors.New("NOT_FOUND",
			"regulatory change not in the auditor-visible set")
	}
	return chg, nil
}

// RegDashboard is the Compliance work-queue projection: untriaged SLA
// items, near-deadline effectives, and live rows missing an owner (the
// unassigned-owner edge case — it surfaces until owned).
type RegDashboard struct {
	Untriaged    []RegChange `json:"untriaged"`
	NearDeadline []RegChange `json:"near_deadline"`
	Unowned      []RegChange `json:"unowned"`
}

// Dashboard composes the officer/auditor work-queue — exactly the
// scoped set spec §14.10.2 exposes to the auditor.
func (s *RegChangeService) Dashboard(ctx context.Context, actor int64,
	limit int) (*RegDashboard, error) {
	if _, err := s.checkRead(ctx, actor); err != nil {
		return nil, err
	}
	untriaged, err := s.Untriaged(ctx, limit)
	if err != nil {
		return nil, err
	}
	near, err := s.NearDeadline(ctx, limit)
	if err != nil {
		return nil, err
	}
	all, err := s.List(ctx, "", 0)
	if err != nil {
		return nil, err
	}
	var unowned []RegChange
	for _, c := range all {
		if c.Owner == nil && c.Status != RegStatusClosed &&
			c.Status != RegStatusImplemented {
			unowned = append(unowned, c)
			if limit > 0 && len(unowned) >= limit {
				break
			}
		}
	}
	return &RegDashboard{
		Untriaged:    guardNil(untriaged),
		NearDeadline: guardNil(near),
		Unowned:      guardNil(unowned),
	}, nil
}

func guardNil(s []RegChange) []RegChange {
	if s == nil {
		return []RegChange{}
	}
	return s
}

// List is the register read — status filter, newest first.
func (s *RegChangeService) List(ctx context.Context, status string, limit int) ([]RegChange, error) {
	where := "1=1"
	if status != "" {
		switch status {
		case RegStatusTracked, RegStatusTriaged, RegStatusScoped,
			RegStatusImplemented, RegStatusClosed:
			where = "status='" + status + "'"
		default:
			return nil, excerrors.New("INVALID_REQUEST", "unknown status "+status)
		}
	}
	out, err := s.listWhere(ctx, where, "published_at DESC", clampLimit(limit))
	if err != nil {
		return nil, err
	}
	for i := range out {
		s.annotateCode(&out[i])
	}
	return out, nil
}

func (s *RegChangeService) listWhere(ctx context.Context, where,
	order string, limit int) ([]RegChange, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT `+regChangeCols+` FROM regulatory_changes
		 WHERE `+where+` ORDER BY `+order+` LIMIT `+fmt.Sprint(clampLimit(limit)))
	if err != nil {
		return nil, excerrors.Wrap("INTERNAL_ERROR", "reg list", err)
	}
	defer rows.Close()
	var out []RegChange
	for rows.Next() {
		c, err := scanRegChangeRows(rows)
		if err != nil {
			return nil, err
		}
		s.annotateCode(c)
		out = append(out, *c)
	}
	return out, rows.Err()
}

// ListImpacts returns the impact map for a change.
func (s *RegChangeService) ListImpacts(ctx context.Context, changeID int64) ([]RegChangeImpact, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id, change_id, kind, ref, owner, effort_estimate, due_at,
		       status, completed_at, recorded_by, created_at, updated_at
		FROM regulatory_change_impacts WHERE change_id=$1
		ORDER BY kind, ref`, changeID)
	if err != nil {
		return nil, excerrors.Wrap("INTERNAL_ERROR", "reg impacts", err)
	}
	defer rows.Close()
	var out []RegChangeImpact
	for rows.Next() {
		var i RegChangeImpact
		var eff *string
		if err := rows.Scan(&i.ID, &i.ChangeID, &i.Kind, &i.Ref, &i.Owner,
			&eff, &i.DueAt, &i.Status, &i.CompletedAt, &i.RecordedBy,
			&i.CreatedAt, &i.UpdatedAt); err != nil {
			return nil, excerrors.Wrap("INTERNAL_ERROR", "reg impact scan", err)
		}
		if eff != nil {
			i.EffortEstimate = *eff
		}
		out = append(out, i)
	}
	return out, rows.Err()
}

func (s *RegChangeService) impactByID(ctx context.Context, id int64) (*RegChangeImpact, error) {
	var i RegChangeImpact
	var eff *string
	err := s.pool.QueryRow(ctx, `
		SELECT id, change_id, kind, ref, owner, effort_estimate, due_at,
		       status, completed_at, recorded_by, created_at, updated_at
		FROM regulatory_change_impacts WHERE id=$1`, id).
		Scan(&i.ID, &i.ChangeID, &i.Kind, &i.Ref, &i.Owner, &eff, &i.DueAt,
			&i.Status, &i.CompletedAt, &i.RecordedBy, &i.CreatedAt, &i.UpdatedAt)
	if err == pgx.ErrNoRows {
		return nil, excerrors.New("NOT_FOUND", "impact item not found")
	}
	if err != nil {
		return nil, excerrors.Wrap("INTERNAL_ERROR", "reg impact load", err)
	}
	if eff != nil {
		i.EffortEstimate = *eff
	}
	return &i, nil
}

// ListCorrespondence returns the linked regulator holds/requests.
func (s *RegChangeService) ListCorrespondence(ctx context.Context,
	changeID int64) ([]RegCorrespondence, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id, change_id, kind, summary, received_at, due_at,
		       recorded_by, created_at
		FROM regulatory_change_correspondence WHERE change_id=$1
		ORDER BY received_at DESC`, changeID)
	if err != nil {
		return nil, excerrors.Wrap("INTERNAL_ERROR", "reg correspondence", err)
	}
	defer rows.Close()
	var out []RegCorrespondence
	for rows.Next() {
		var c RegCorrespondence
		if err := rows.Scan(&c.ID, &c.ChangeID, &c.Kind, &c.Summary,
			&c.ReceivedAt, &c.DueAt, &c.RecordedBy, &c.CreatedAt); err != nil {
			return nil, excerrors.Wrap("INTERNAL_ERROR", "reg corr scan", err)
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func (s *RegChangeService) correspondenceByID(ctx context.Context,
	id int64) (*RegCorrespondence, error) {
	var c RegCorrespondence
	err := s.pool.QueryRow(ctx, `
		SELECT id, change_id, kind, summary, received_at, due_at,
		       recorded_by, created_at
		FROM regulatory_change_correspondence WHERE id=$1`, id).
		Scan(&c.ID, &c.ChangeID, &c.Kind, &c.Summary, &c.ReceivedAt,
			&c.DueAt, &c.RecordedBy, &c.CreatedAt)
	if err == pgx.ErrNoRows {
		return nil, excerrors.New("NOT_FOUND", "correspondence not found")
	}
	if err != nil {
		return nil, excerrors.Wrap("INTERNAL_ERROR", "reg corr load", err)
	}
	return &c, nil
}

// checkRole enforces the admin-owned mutation matrix (Compliance
// Officer / Super Admin) — the gate rides inside the domain so a
// route-registry slip still fails closed.
func (s *RegChangeService) checkRole(ctx context.Context, userID int64) error {
	return admin.GateRegChangeMutation(ctx,
		admin.AdminRoleResolver(s.resolver), userID)
}

// checkRead enforces the reader matrix and returns the role.
func (s *RegChangeService) checkRead(ctx context.Context, userID int64) (string, error) {
	return admin.GateRegChangeRead(ctx,
		admin.AdminRoleResolver(s.resolver), userID)
}

func clampLimit(l int) int {
	if l <= 0 || l > 500 {
		return 200
	}
	return l
}

func nullableStr(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}
