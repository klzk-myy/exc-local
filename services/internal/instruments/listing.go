// Phase-15 Task 15.3.12 — listing proposals, delisting impact preview
// and the §7.5 grace ladder (spec §7.5, §24 #352; migration 091
// listing_proposals + the 223 review/dual-control/activation extension).
//
// Pipeline: PROPOSED → IN_REVIEW → APPROVED → SCHEDULED (| REJECTED).
// The APPROVE leg is four-eyes per the §7.2 matrix (instrument create is
// a dual-controlled operation): the approving reviewer submits
// OpInstrumentListing and the second approver's approval runs the DRAFT
// creation inside the approval transaction — the maker-checker record,
// the reference row and the instrument row commit or fail together.
// SCHEDULED proposals flip DRAFT→ACTIVE at activate_at (the next weekly
// open by default) through the ActivateDue sweep.
//
// Delisting: PreviewDelist aggregates the §7.5 impact read (open
// positions, resting orders, sub-account exposure). RequestDelist starts
// the ladder — RESTRICTED + 24h notice now, OpInstrumentDelist
// four-eyes submission once the notice elapses (AdvanceDelisting sweep),
// then the lifecycle engine's 30-day DELISTED close-only window; the
// sweep marks the schedule PURGE_READY at the window's end — the
// force-close/purge itself rides the Task 15.3.8 maker-checker (sibling
// scope). A DELISTED row that arrived via the direct admin endpoint
// (bypassing this service) is adopted: the sweep materializes a
// CLOSE_ONLY schedule from the row's updated_at.
package instruments

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/shopspring/decimal"

	"exchange/internal/admin"
	"exchange/internal/audit"

	excerrors "exchange/pkg/errors"
)

// ---------------------------------------------------------------------------
// Proposal model (migration 091 + 223)
// ---------------------------------------------------------------------------

// Proposal statuses — verbatim 091 CHECK values.
const (
	PropProposed  = "PROPOSED"
	PropInReview  = "IN_REVIEW"
	PropApproved  = "APPROVED"
	PropRejected  = "REJECTED"
	PropScheduled = "SCHEDULED"
)

// Proposal is one listing_proposals row.
type Proposal struct {
	ID             int64           `json:"id"`
	ProposerID     int64           `json:"proposer_id"`
	Symbol         string          `json:"symbol"`
	ReferenceRow   json.RawMessage `json:"reference_row"`
	OracleCoverage json.RawMessage `json:"oracle_coverage"`
	RiskDefaults   json.RawMessage `json:"risk_defaults"`
	AutoChecks     json.RawMessage `json:"auto_checks"`
	Status         string          `json:"status"`
	Reason         string          `json:"reason"`
	ReviewedBy     *int64          `json:"reviewed_by,omitempty"`
	ReviewedAt     *time.Time      `json:"reviewed_at,omitempty"`
	DualControlID  *int64          `json:"dual_control_id,omitempty"`
	InstrumentID   *int64          `json:"instrument_id,omitempty"`
	ActivateAt     *time.Time      `json:"activate_at,omitempty"`
	CreatedAt      time.Time       `json:"created_at"`
	UpdatedAt      time.Time       `json:"updated_at"`
}

// ProposalInput is the POST /admin/listing-proposals body.
type ProposalInput struct {
	Symbol         string         `json:"symbol"`
	Reference      ReferenceInput `json:"reference"`
	OracleFeeds    []string       `json:"oracle_feeds"` // ≥2 distinct feed ids
	OracleCoverage map[string]any `json:"oracle_coverage,omitempty"`
	RiskDefaults   map[string]any `json:"risk_defaults"`
	ActivateAt     *time.Time     `json:"activate_at,omitempty"` // nil → next weekly open
	Reason         string         `json:"reason"`
}

// autoChecks is the durable auto_checks payload — every check carries a
// pass flag so the review leg and the ops board render failures verbatim.
type autoChecks struct {
	ReferenceOK     bool     `json:"reference_ok"`
	ReferenceFails  []string `json:"reference_fails,omitempty"`
	SymbolAvailable bool     `json:"symbol_available"`
	OracleFeedCount int      `json:"oracle_feed_count"`
	OracleOK        bool     `json:"oracle_ok"`
	RiskDefaultsOK  bool     `json:"risk_defaults_ok"`
}

// AllPass reports the review-gate verdict.
func (c autoChecks) AllPass() bool {
	return c.ReferenceOK && c.SymbolAvailable && c.OracleOK && c.RiskDefaultsOK
}

// ---------------------------------------------------------------------------
// Listing service
// ---------------------------------------------------------------------------

// ListingDeps wires the service. Pool, Roles and Instruments are
// required; Dual may be nil (the approve leg then fails closed with
// DUAL_CONTROL_REQUIRED).
type ListingDeps struct {
	Pool        *pgxpool.Pool
	Roles       admin.AdminRoleResolver
	Instruments *admin.InstrumentService
	Dual        *admin.DualControlService
	Sessions    *SessionCalendar            // nil → UTC-next-24h fallback for activate_at
	WS          admin.InstrumentWSPublisher // nil → no WS broadcast
	Now         func() time.Time
	Logf        func(format string, args ...any)
}

// ListingService drives the listing_proposals pipeline + delist ladder.
type ListingService struct {
	pool        *pgxpool.Pool
	roles       admin.AdminRoleResolver
	instruments *admin.InstrumentService
	dual        *admin.DualControlService
	sessions    *SessionCalendar
	ws          admin.InstrumentWSPublisher
	now         func() time.Time
	logf        func(format string, args ...any)
}

// Role sets (spec §7.2 — "Role X" admits X and Super Admin).
var (
	listProposeRoles = map[string]bool{admin.RoleRiskManager: true, admin.RoleSuperAdmin: true}
	listReviewRoles  = map[string]bool{
		admin.RoleRiskManager: true, admin.RoleComplianceOfficer: true, admin.RoleSuperAdmin: true}
	listApproveRoles = map[string]bool{admin.RoleSuperAdmin: true}
)

// NewListingService wires the service — missing mandatory deps fail
// closed.
func NewListingService(d ListingDeps) (*ListingService, error) {
	if d.Pool == nil {
		return nil, fmt.Errorf("listing: pgx pool is nil")
	}
	if d.Roles == nil {
		return nil, fmt.Errorf("listing: role resolver is nil")
	}
	if d.Instruments == nil {
		return nil, fmt.Errorf("listing: lifecycle service is nil")
	}
	s := &ListingService{
		pool: d.Pool, roles: d.Roles, instruments: d.Instruments,
		dual: d.Dual, sessions: d.Sessions, ws: d.WS, now: d.Now,
		logf: d.Logf,
	}
	if s.now == nil {
		s.now = time.Now
	}
	if s.logf == nil {
		s.logf = func(string, ...any) {}
	}
	return s, nil
}

func (s *ListingService) requireRole(ctx context.Context, userID int64,
	allowed map[string]bool, what string) error {
	role, err := s.roles(ctx, userID)
	if err != nil {
		return excerrors.Wrap("INTERNAL_ERROR", "role resolution", err)
	}
	if !allowed[role] {
		return excerrors.New("UNAUTHORIZED_ROLE",
			what+" requires a binding in the operation's role set")
	}
	return nil
}

// ---------------------------------------------------------------------------
// Reads
// ---------------------------------------------------------------------------

const proposalCols = `
	id, proposer_id, symbol, reference_row, oracle_coverage, risk_defaults,
	auto_checks, status, reason, reviewed_by, reviewed_at, dual_control_id,
	instrument_id, activate_at, created_at, updated_at`

func scanProposal(row pgx.Row) (*Proposal, error) {
	var p Proposal
	err := row.Scan(&p.ID, &p.ProposerID, &p.Symbol, &p.ReferenceRow,
		&p.OracleCoverage, &p.RiskDefaults, &p.AutoChecks, &p.Status,
		&p.Reason, &p.ReviewedBy, &p.ReviewedAt, &p.DualControlID,
		&p.InstrumentID, &p.ActivateAt, &p.CreatedAt, &p.UpdatedAt)
	if err != nil {
		return nil, err
	}
	return &p, nil
}

// Get returns one proposal (nil when unknown).
func (s *ListingService) Get(ctx context.Context, id int64) (*Proposal, error) {
	p, err := scanProposal(s.pool.QueryRow(ctx,
		`SELECT `+proposalCols+` FROM listing_proposals WHERE id=$1`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, excerrors.Wrap("INTERNAL_ERROR", "proposal lookup", err)
	}
	return p, nil
}

// List returns proposals — optional status filter, newest first.
func (s *ListingService) List(ctx context.Context, status string) ([]Proposal, error) {
	q := `SELECT ` + proposalCols + ` FROM listing_proposals`
	var args []any
	if status != "" {
		q += ` WHERE status = $1`
		args = append(args, strings.ToUpper(strings.TrimSpace(status)))
	}
	q += ` ORDER BY id DESC`
	rows, err := s.pool.Query(ctx, q, args...)
	if err != nil {
		return nil, excerrors.Wrap("INTERNAL_ERROR", "list proposals", err)
	}
	defer rows.Close()
	out := []Proposal{}
	for rows.Next() {
		var p Proposal
		if err := rows.Scan(&p.ID, &p.ProposerID, &p.Symbol, &p.ReferenceRow,
			&p.OracleCoverage, &p.RiskDefaults, &p.AutoChecks, &p.Status,
			&p.Reason, &p.ReviewedBy, &p.ReviewedAt, &p.DualControlID,
			&p.InstrumentID, &p.ActivateAt, &p.CreatedAt, &p.UpdatedAt); err != nil {
			return nil, excerrors.Wrap("INTERNAL_ERROR", "scan proposal", err)
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// ---------------------------------------------------------------------------
// Auto-checks (spec §7.5 item 1)
// ---------------------------------------------------------------------------

// check computes the auto_checks payload. symbolAvailable requires no
// non-DELISTED instruments row with the symbol.
func (s *ListingService) check(ctx context.Context, in ProposalInput) autoChecks {
	c := autoChecks{}

	fails := in.Reference.Validate()
	c.ReferenceOK = len(fails) == 0
	c.ReferenceFails = fails

	// Symbol availability — a live (non-DELISTED) instruments row with
	// the symbol blocks the proposal.
	var existing string
	err := s.pool.QueryRow(ctx,
		`SELECT status::text FROM instruments WHERE symbol=$1`,
		in.Symbol).Scan(&existing)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		c.SymbolAvailable = true
	case err != nil:
		c.SymbolAvailable = false
		c.ReferenceFails = append(c.ReferenceFails, "symbol lookup failed: "+err.Error())
	default:
		c.SymbolAvailable = existing == admin.InstDelisted
		if !c.SymbolAvailable {
			c.ReferenceFails = append(c.ReferenceFails,
				"symbol "+in.Symbol+" already exists with status "+existing)
		}
	}

	feeds := map[string]bool{}
	for _, f := range in.OracleFeeds {
		f = strings.TrimSpace(f)
		if f != "" {
			feeds[f] = true
		}
	}
	c.OracleFeedCount = len(feeds)
	c.OracleOK = len(feeds) >= 2 // spec §7.5: at least two oracle feeds

	// Risk defaults must populate the per-symbol band limits the create
	// path consumes.
	c.RiskDefaultsOK = len(in.RiskDefaults) > 0 &&
		in.RiskDefaults["price_band_pct_up"] != nil &&
		in.RiskDefaults["price_band_pct_down"] != nil
	return c
}

// ---------------------------------------------------------------------------
// Propose
// ---------------------------------------------------------------------------

// Propose inserts a PROPOSED proposal with the auto-check verdict —
// failing checks do not block the row (the review leg gates approval on
// a fresh recompute) but are recorded verbatim for the reviewer.
func (s *ListingService) Propose(ctx context.Context, actor admin.AdminActor,
	in ProposalInput) (*Proposal, error) {

	if err := s.requireRole(ctx, actor.UserID, listProposeRoles, "listing proposal"); err != nil {
		return nil, err
	}
	base, quote, err := admin.ValidatePairSymbol(in.Symbol)
	if err != nil {
		return nil, err
	}
	in.Symbol = strings.ToUpper(strings.TrimSpace(in.Symbol))
	in.Reference.BaseCurrency, in.Reference.QuoteCurrency = base, quote
	if strings.TrimSpace(in.Reason) == "" {
		return nil, excerrors.New("INVALID_REQUEST",
			"a reason is mandatory for listing proposals")
	}
	checks := s.check(ctx, in)
	checksRaw, _ := json.Marshal(checks)

	refRaw, _ := json.Marshal(in.Reference)
	coverage := in.OracleCoverage
	if coverage == nil {
		coverage = map[string]any{}
	}
	coverage["feeds"] = in.OracleFeeds
	covRaw, _ := json.Marshal(coverage)
	riskRaw, _ := json.Marshal(in.RiskDefaults)
	if in.RiskDefaults == nil {
		riskRaw = []byte(`{}`)
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, excerrors.Wrap("INTERNAL_ERROR", "begin tx", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	p, err := scanProposal(tx.QueryRow(ctx, `
		INSERT INTO listing_proposals
		    (proposer_id, symbol, reference_row, oracle_coverage,
		     risk_defaults, auto_checks, reason)
		VALUES ($1,$2,$3,$4,$5,$6,$7)
		RETURNING `+proposalCols,
		actor.UserID, in.Symbol, refRaw, covRaw, riskRaw, checksRaw,
		in.Reason))
	if err != nil {
		return nil, excerrors.Wrap("INTERNAL_ERROR", "insert proposal", err)
	}
	if _, _, err := admin.Log(ctx, tx, admin.AuditEntry{
		AdminUserID: actor.UserID, Action: "instrument.listing.propose",
		TargetType: "listing_proposal", TargetID: &p.ID,
		AfterState: map[string]any{
			"symbol": in.Symbol, "auto_checks": checks,
		},
		IPAddress: actor.ClientIP,
	}); err != nil {
		return nil, excerrors.Wrap("INTERNAL_ERROR", "audit log", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, excerrors.Wrap("INTERNAL_ERROR", "commit", err)
	}
	return p, nil
}

// ---------------------------------------------------------------------------
// Review — PROPOSED/IN_REVIEW → APPROVED (four-eyes submission) | REJECTED
// ---------------------------------------------------------------------------

// ReviewAction values for the /review endpoint.
const (
	ReviewMark    = "REVIEW"  // PROPOSED → IN_REVIEW
	ReviewApprove = "APPROVE" // → APPROVED + OpInstrumentListing submit
	ReviewReject  = "REJECT"  // → REJECTED
)

// Review decides a proposal. APPROVE recomputes the auto-checks — all
// must pass — and files the §7.2 four-eyes request whose approval runs
// the DRAFT create in-tx. The reviewer must differ from the proposer
// (§8.2 principal distinctness); APPROVE additionally requires a
// Super-Admin binding because the resulting create is an SA-class
// operation (the maker must carry the op's authority at submit time).
func (s *ListingService) Review(ctx context.Context, actor admin.AdminActor,
	id int64, action, note string, activateAt *time.Time) (*Proposal, *admin.DualControlRequest, error) {

	action = strings.ToUpper(strings.TrimSpace(action))
	switch action {
	case ReviewMark, ReviewApprove, ReviewReject:
	default:
		return nil, nil, excerrors.New("INVALID_REQUEST",
			"action must be REVIEW|APPROVE|REJECT")
	}
	if err := s.requireRole(ctx, actor.UserID, listReviewRoles, "listing review"); err != nil {
		return nil, nil, err
	}

	p, err := s.Get(ctx, id)
	if err != nil {
		return nil, nil, err
	}
	if p == nil {
		return nil, nil, excerrors.New("NOT_FOUND",
			fmt.Sprintf("listing proposal %d not found", id))
	}
	if actor.UserID == p.ProposerID {
		return nil, nil, excerrors.New("DUAL_CONTROL_VIOLATION",
			"the proposer cannot review their own listing proposal (§8.2)")
	}

	switch action {
	case ReviewMark:
		if p.Status != PropProposed {
			return nil, nil, excerrors.New("INVALID_REQUEST",
				"only PROPOSED proposals can enter review (status "+p.Status+")")
		}
		out, err := s.setStatus(ctx, actor, p, PropInReview, "instrument.listing.review", note)
		return out, nil, err

	case ReviewReject:
		if p.Status != PropProposed && p.Status != PropInReview {
			return nil, nil, excerrors.New("INVALID_REQUEST",
				"only PROPOSED/IN_REVIEW proposals can be rejected (status "+p.Status+")")
			// APPROVED proposals carry a live four-eyes request — the
			// maker cancels it through the dual-control endpoint instead.
		}
		out, err := s.setStatus(ctx, actor, p, PropRejected, "instrument.listing.reject", note)
		return out, nil, err
	}

	// APPROVE leg.
	if err := s.requireRole(ctx, actor.UserID, listApproveRoles, "listing approval"); err != nil {
		return nil, nil, err
	}
	if p.Status == PropApproved && p.DualControlID != nil && s.dual != nil {
		// Resubmission is allowed only after the filed four-eyes request
		// reached a dead end (REJECTED/EXPIRED).
		prev, err := s.dual.Get(ctx, *p.DualControlID)
		if err != nil {
			return nil, nil, err
		}
		if prev != nil && prev.Status != admin.ReqRejected &&
			prev.Status != admin.ReqExpired {
			return nil, nil, excerrors.New("INVALID_REQUEST",
				"proposal already APPROVED with a live four-eyes request — the dual-control endpoint decides it")
		}
	}
	if p.Status != PropProposed && p.Status != PropInReview && p.Status != PropApproved {
		return nil, nil, excerrors.New("INVALID_REQUEST",
			"proposal status "+p.Status+" is not approvable")
	}
	var ref ReferenceInput
	if err := json.Unmarshal(p.ReferenceRow, &ref); err != nil {
		return nil, nil, excerrors.Wrap("INTERNAL_ERROR", "decode reference row", err)
	}
	var coverage map[string]any
	_ = json.Unmarshal(p.OracleCoverage, &coverage)
	var feeds []string
	if v, ok := coverage["feeds"].([]any); ok {
		for _, f := range v {
			if s, ok := f.(string); ok {
				feeds = append(feeds, s)
			}
		}
	}
	var risk map[string]any
	_ = json.Unmarshal(p.RiskDefaults, &risk)
	recheck := s.check(ctx, ProposalInput{
		Symbol:         p.Symbol,
		Reference:      ref,
		OracleFeeds:    feeds,
		OracleCoverage: coverage,
		RiskDefaults:   risk,
	})
	if !recheck.AllPass() {
		return nil, nil, excerrors.New("INVALID_REQUEST",
			fmt.Sprintf("listing auto-checks fail — cannot approve: %v",
				recheck.ReferenceFails))
	}
	if s.dual == nil {
		return nil, nil, excerrors.New("DUAL_CONTROL_REQUIRED",
			"listing approval requires the four-eyes queue — dual-control service unwired")
	}
	if activateAt == nil {
		t := s.defaultActivateAt()
		activateAt = &t
	}
	dc, err := s.dual.Submit(ctx, admin.SubmitInput{
		Operation:    admin.OpInstrumentListing,
		TargetType:   "listing_proposal",
		TargetID:     strconv.FormatInt(p.ID, 10),
		RequiredRole: admin.RoleSuperAdmin,
		RequestedBy:  actor.UserID,
		Reason:       note,
		ClientIP:     actor.ClientIP,
		Payload: map[string]any{
			"proposal_id": p.ID,
			"symbol":      p.Symbol,
			"reference":   ref,
			"activate_at": activateAt.UTC().Format(time.RFC3339),
			"client_ip":   actor.ClientIP,
		},
	})
	if err != nil {
		return nil, nil, err
	}
	out, err := s.setStatusDual(ctx, actor, p, PropApproved,
		"instrument.listing.approve", note, dc.ID, activateAt)
	if err != nil {
		return nil, nil, err
	}
	return out, dc, nil
}

// defaultActivateAt resolves the activation instant — the next weekly
// open per the §7.4 session calendar (Sunday 17:00 ET), or next UTC day
// boundary when the session calendar is unwired.
func (s *ListingService) defaultActivateAt() time.Time {
	if s.sessions != nil {
		return s.sessions.NextWeeklyOpen(s.now())
	}
	n := s.now().UTC()
	return time.Date(n.Year(), n.Month(), n.Day()+1, 0, 0, 0, 0, time.UTC)
}

// setStatus flips a proposal's status with reviewer stamps + audit.
func (s *ListingService) setStatus(ctx context.Context, actor admin.AdminActor,
	p *Proposal, to, auditAction, note string) (*Proposal, error) {
	return s.setStatusDual(ctx, actor, p, to, auditAction, note, 0, nil)
}

func (s *ListingService) setStatusDual(ctx context.Context, actor admin.AdminActor,
	p *Proposal, to, auditAction, note string, dualID int64,
	activateAt *time.Time) (*Proposal, error) {

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, excerrors.Wrap("INTERNAL_ERROR", "begin tx", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var out *Proposal
	err = func() error {
		q := `UPDATE listing_proposals
		         SET status=$2, reviewed_by=$3, reviewed_at=now(), updated_at=now()`
		args := []any{p.ID, to, actor.UserID}
		n := 4
		if dualID > 0 {
			q += `, dual_control_id=$` + strconv.Itoa(n)
			args = append(args, dualID)
			n++
		}
		if activateAt != nil {
			q += `, activate_at=$` + strconv.Itoa(n)
			args = append(args, *activateAt)
			n++
		}
		if note != "" {
			q += `, reason = reason || ' | ' || $` + strconv.Itoa(n)
			args = append(args, note)
			n++
		}
		q += ` WHERE id=$1 AND status=$` + strconv.Itoa(n) +
			` RETURNING ` + proposalCols
		args = append(args, p.Status)
		var err error
		out, err = scanProposal(tx.QueryRow(ctx, q, args...))
		return err
	}()
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, excerrors.New("INVALID_REQUEST",
			"proposal "+strconv.FormatInt(p.ID, 10)+" changed status concurrently — retry")
	}
	if err != nil {
		return nil, excerrors.Wrap("INTERNAL_ERROR", "proposal status update", err)
	}
	if _, _, err := admin.Log(ctx, tx, admin.AuditEntry{
		AdminUserID: actor.UserID, Action: auditAction,
		TargetType: "listing_proposal", TargetID: &p.ID,
		BeforeState: map[string]any{"status": p.Status},
		AfterState: map[string]any{
			"status": to, "note": note, "dual_control_id": dualID,
			"proposer_id": p.ProposerID, "reviewer_id": actor.UserID,
		},
		IPAddress: actor.ClientIP,
	}); err != nil {
		return nil, excerrors.Wrap("INTERNAL_ERROR", "audit log", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, excerrors.Wrap("INTERNAL_ERROR", "commit", err)
	}
	if s.ws != nil {
		s.ws.Publish(admin.InstrumentStatusChannel, admin.InstrumentStatusEvent{
			Event: "LISTING_PROPOSAL", Symbol: p.Symbol, To: to,
			Reason: note, ActorID: p.ProposerID, ApproverID: actor.UserID,
			Source: "listing", TsMs: s.now().UnixMilli(),
		})
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// Four-eyes executor — approval lands the DRAFT + links the proposal
// ---------------------------------------------------------------------------

// RegisterListingExecutor attaches the OpInstrumentListing executor —
// the approval transaction runs the DRAFT insert (sibling CreateInTx),
// pins the proposer's reference values, upserts the §5.44.10 envelope and
// flips the proposal SCHEDULED, all atomically.
func RegisterListingExecutor(dual *admin.DualControlService,
	svc *ListingService) {
	dual.RegisterExecutor(admin.OpInstrumentListing,
		func(ctx context.Context, tx pgx.Tx, req *admin.DualControlRequest) error {
			return svc.applyListingTx(ctx, tx, req)
		})
}

// applyListingTx is the executor body (in the approval transaction).
func (s *ListingService) applyListingTx(ctx context.Context, tx pgx.Tx,
	req *admin.DualControlRequest) error {

	var p struct {
		ProposalID int64          `json:"proposal_id"`
		Reference  ReferenceInput `json:"reference"`
		ActivateAt string         `json:"activate_at"`
		ClientIP   string         `json:"client_ip"`
	}
	if err := json.Unmarshal(req.Payload, &p); err != nil {
		return excerrors.Wrap("INVALID_REQUEST", "decode listing payload", err)
	}

	// Lock the proposal — must still be APPROVED (a concurrent reject or
	// a re-executed request refuses; SCHEDULED is the idempotent no-op).
	var status string
	var symbol string
	err := tx.QueryRow(ctx, `
		SELECT status, symbol FROM listing_proposals WHERE id=$1 FOR UPDATE`,
		p.ProposalID).Scan(&status, &symbol)
	if errors.Is(err, pgx.ErrNoRows) {
		return excerrors.New("NOT_FOUND", "listing proposal not found")
	}
	if err != nil {
		return excerrors.Wrap("INTERNAL_ERROR", "lock proposal", err)
	}
	if status == PropScheduled {
		return nil // idempotent — already applied
	}
	if status != PropApproved {
		return excerrors.New("INVALID_REQUEST",
			"listing proposal is "+status+" — executor requires APPROVED")
	}

	ref := p.Reference
	if fails := ref.Validate(); len(fails) > 0 {
		return excerrors.New("INVALID_REQUEST",
			fmt.Sprintf("reference row revalidation failed: %v", fails))
	}

	activateAt, err := time.Parse(time.RFC3339, p.ActivateAt)
	if err != nil {
		return excerrors.Wrap("INVALID_REQUEST", "bad activate_at", err)
	}

	// The DRAFT insert rides the sibling path — the §7.4 reference
	// columns the create payload cannot carry are pinned immediately
	// after with the proposer's explicit values (they would otherwise
	// take the migration-087 trigger's convention-derived defaults).
	actor := admin.AdminActor{UserID: req.RequestedBy, ClientIP: p.ClientIP}
	if req.ApprovedBy != nil {
		actor.ApproverID = *req.ApprovedBy
	}
	inst, err := s.instruments.CreateInTx(ctx, tx, actor, admin.InstrumentCreate{
		Symbol:          symbol,
		BaseCurrency:    ref.BaseCurrency,
		QuoteCurrency:   ref.QuoteCurrency,
		InstrumentType:  ref.InstrumentType,
		TickSize:        ref.TickSize,
		LotSize:         ref.LotSize,
		MinOrderQty:     ref.MinOrderQty,
		MaxOrderQty:     ref.MaxOrderQty,
		SettlementCycle: ref.SettlementCycle,
		MaxLeverage:     ref.MaxLeverage,
		Reason:          "listing proposal #" + strconv.FormatInt(p.ProposalID, 10),
	})
	if err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `
		UPDATE instruments
		   SET contract_size=$2, decimal_places=$3, pip_size=$4,
		       min_notional=$5
		 WHERE id=$1`,
		inst.ID, ref.ContractSize, ref.DecimalPlaces, ref.PipSize,
		ref.MinNotional); err != nil {
		return excerrors.Wrap("INTERNAL_ERROR", "pin reference columns", err)
	}
	// §5.44.10 envelope — margin rate derives from max leverage; trading
	// hours + tenor grid land as the documented production defaults.
	marginRate := "0"
	if ref.MaxLeverage > 0 {
		marginRate = decimal.NewFromInt(1).Div(
			decimal.NewFromInt(ref.MaxLeverage)).String()
	}
	trading, _ := json.Marshal(map[string]any{
		"open_day": "SUN", "open_local": "17:00",
		"close_day": "FRI", "close_local": "17:00",
		"timezone": VenueTZ, "pre_open_minutes": 15,
		"daily_cutoff": "17:00 " + VenueTZ,
	})
	tenor, _ := json.Marshal(TenorGrid)
	fixing, _ := json.Marshal(map[string]any{
		"ndf_fixing_cut": "10:00 " + VenueTZ,
		"venue_cut":      "15:00 UTC",
	})
	if err := upsertReferenceEnvelope(ctx, tx, Reference{
		InstrumentID: inst.ID, Symbol: symbol, MarginRate: marginRate,
		ContractSize: ref.ContractSize, DecimalPlaces: ref.DecimalPlaces,
		PipSize:      ref.PipSize,
		TradingHours: trading, Tenor: tenor, FixingCalendar: fixing,
	}); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `
		UPDATE listing_proposals
		   SET status=$2, instrument_id=$3, activate_at=$4, updated_at=now()
		 WHERE id=$1`,
		p.ProposalID, PropScheduled, inst.ID, activateAt); err != nil {
		return excerrors.Wrap("INTERNAL_ERROR", "mark proposal scheduled", err)
	}
	if _, _, err := admin.Log(ctx, tx, admin.AuditEntry{
		AdminUserID: req.RequestedBy, Action: "instrument.listing.applied",
		TargetType: "instrument", TargetID: &inst.ID,
		AfterState: map[string]any{
			"proposal_id": p.ProposalID, "symbol": symbol,
			"status": admin.InstDraft, "activate_at": activateAt,
			"approver_id": req.ApprovedBy,
		},
		IPAddress: p.ClientIP,
	}); err != nil {
		return excerrors.Wrap("INTERNAL_ERROR", "audit log", err)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Scheduled activation — SCHEDULED proposal → DRAFT→ACTIVE
// ---------------------------------------------------------------------------

// ActivateDue flips every SCHEDULED proposal whose activate_at has
// arrived: the DRAFT→ACTIVE status write uses the lifecycle audit shape
// (action instrument.lifecycle.activate, before/after status) so the
// committed publication runs through the sibling's PublishCommitted —
// engine status key + WS broadcast land exactly as a manual activate's.
// Returns the number activated.
func (s *ListingService) ActivateDue(ctx context.Context) (int, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT p.id, p.instrument_id, p.symbol, i.status::text, p.reviewed_by
		  FROM listing_proposals p JOIN instruments i ON i.id = p.instrument_id
		 WHERE p.status = 'SCHEDULED' AND p.activate_at <= now()
		 ORDER BY p.id`)
	if err != nil {
		return 0, excerrors.Wrap("INTERNAL_ERROR", "due activations scan", err)
	}
	type due struct {
		propID     int64
		instID     int64
		symbol     string
		status     string
		reviewedBy *int64
	}
	var list []due
	for rows.Next() {
		var d due
		if err := rows.Scan(&d.propID, &d.instID, &d.symbol, &d.status,
			&d.reviewedBy); err != nil {
			rows.Close()
			return 0, excerrors.Wrap("INTERNAL_ERROR", "scan activation", err)
		}
		list = append(list, d)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, excerrors.Wrap("INTERNAL_ERROR", "activation rows", err)
	}

	activated := 0
	for _, d := range list {
		if d.status != admin.InstDraft {
			continue // already moved — reconciler/no-op
		}
		actorID := int64(0)
		if d.reviewedBy != nil {
			actorID = *d.reviewedBy
		}
		if actorID <= 0 {
			s.logf("listing: proposal %d has no reviewer attribution — skipped", d.propID)
			continue
		}
		if err := s.activateOne(ctx, d.propID, d.instID, d.symbol, actorID); err != nil {
			s.logf("listing: activate %s: %v", d.symbol, err)
			continue // retry next tick — fail closed, never half-activate
		}
		activated++
	}
	return activated, nil
}

// activateOne commits one DRAFT→ACTIVE flip, then publishes through the
// sibling's committed-derivation path.
func (s *ListingService) activateOne(ctx context.Context, propID, instID int64,
	symbol string, actorID int64) error {

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return excerrors.Wrap("INTERNAL_ERROR", "begin tx", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var cur string
	err = tx.QueryRow(ctx,
		`SELECT status::text FROM instruments WHERE id=$1 FOR UPDATE`,
		instID).Scan(&cur)
	if err != nil {
		return excerrors.Wrap("INTERNAL_ERROR", "lock instrument", err)
	}
	if cur != admin.InstDraft {
		_ = tx.Rollback(ctx)
		return nil // raced — already activated
	}
	if _, err := tx.Exec(ctx, `
		UPDATE instruments SET status='ACTIVE', updated_at=now()
		 WHERE id=$1`, instID); err != nil {
		return excerrors.Wrap("INTERNAL_ERROR", "activate instrument", err)
	}
	if _, _, err := admin.Log(ctx, tx, admin.AuditEntry{
		AdminUserID: actorID, Action: "instrument.lifecycle.activate",
		TargetType: "instrument", TargetID: &instID,
		BeforeState: map[string]any{"status": cur},
		AfterState: map[string]any{
			"status": admin.InstActive, "reason": "scheduled listing activation",
			"auction": false, "approver_id": actorID, "scheduled": true,
			"proposal_id": propID,
		},
	}); err != nil {
		return excerrors.Wrap("INTERNAL_ERROR", "audit log", err)
	}
	if _, err := tx.Exec(ctx, `
		UPDATE listing_proposals
		   SET auto_checks = auto_checks || $2::jsonb, updated_at=now()
		 WHERE id=$1`,
		propID, fmt.Sprintf(`{"activated_at":%q}`,
			s.now().UTC().Format(time.RFC3339))); err != nil {
		return excerrors.Wrap("INTERNAL_ERROR", "mark activation", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return excerrors.Wrap("INTERNAL_ERROR", "commit", err)
	}
	if err := s.instruments.PublishCommitted(ctx, instID); err != nil {
		s.logf("listing: publish activation %s: %v", symbol, err)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Delisting — §7.5 impact preview + RESTRICTED→DELISTED→purge ladder
// ---------------------------------------------------------------------------

// DelistImpact is the §7.5 pre-transition read: open positions, resting
// orders and sub-account exposure for the symbol.
type DelistImpact struct {
	InstrumentID     int64  `json:"instrument_id"`
	Symbol           string `json:"symbol"`
	Status           string `json:"status"`
	OpenPositions    int64  `json:"open_positions"`
	PositionQty      string `json:"position_qty"` // gross |qty| across positions
	MarginUsed       string `json:"margin_used"`  // sum of positions.margin_used
	RestingOrders    int64  `json:"resting_orders"`
	RestingOrderQty  string `json:"resting_order_qty"` // unfilled remainder
	DistinctAccounts int64  `json:"distinct_accounts"`
	SubAccounts      int64  `json:"sub_accounts"` // accounts with parent_account_id set
	CapturedAt       int64  `json:"captured_at_ms"`
}

// PreviewDelist aggregates the impact read (spec §7.5 item 2). Read-only
// — the numbers ride into the delist_schedule audit payload.
func (s *ListingService) PreviewDelist(ctx context.Context, symbol string) (*DelistImpact, error) {
	symbol = strings.ToUpper(strings.TrimSpace(symbol))
	var imp DelistImpact
	err := s.pool.QueryRow(ctx, `
		SELECT id, symbol, status::text FROM instruments WHERE symbol=$1`,
		symbol).Scan(&imp.InstrumentID, &imp.Symbol, &imp.Status)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, excerrors.New("NOT_FOUND", "instrument "+symbol+" not found")
	}
	if err != nil {
		return nil, excerrors.Wrap("INTERNAL_ERROR", "instrument lookup", err)
	}
	err = s.pool.QueryRow(ctx, `
		SELECT count(*), COALESCE(sum(abs(quantity))::text,'0'),
		       COALESCE(sum(margin_used)::text,'0'),
		       count(DISTINCT account_id),
		       count(DISTINCT account_id) FILTER (
		           WHERE a.parent_account_id IS NOT NULL)
		  FROM positions p JOIN accounts a ON a.id = p.account_id
		 WHERE p.instrument_id=$1 AND p.quantity <> 0`,
		imp.InstrumentID).Scan(&imp.OpenPositions, &imp.PositionQty,
		&imp.MarginUsed, &imp.DistinctAccounts, &imp.SubAccounts)
	if err != nil {
		return nil, excerrors.Wrap("INTERNAL_ERROR", "position impact", err)
	}
	var restingQty string
	err = s.pool.QueryRow(ctx, `
		SELECT count(*), COALESCE(sum(quantity - filled_qty)::text,'0')
		  FROM orders
		 WHERE instrument_id=$1
		   AND status IN ('PENDING','RESERVED','ACTIVE','PARTIALLY_FILLED')`,
		imp.InstrumentID).Scan(&imp.RestingOrders, &restingQty)
	if err != nil {
		return nil, excerrors.Wrap("INTERNAL_ERROR", "order impact", err)
	}
	imp.RestingOrderQty = restingQty
	imp.CapturedAt = s.now().UnixMilli()
	return &imp, nil
}

// DelistNotice is the 24-hour RESTRICTED notice window (§7.5).
const DelistNotice = 24 * time.Hour

// DelistCloseOnly is the 30-day close-only window after DELISTED (§7.5).
const DelistCloseOnly = 30 * 24 * time.Hour

// RequestDelist starts the §7.5 ladder: aggregates the impact preview,
// runs the RESTRICTED transition through the lifecycle engine (the
// single-approver restrict op — the initiator must hold Risk Manager or
// Super Admin), and records the durable ladder state in
// instruments_reference.delist_schedule. The DELISTED leg files the
// OpInstrumentDelist four-eyes request once the 24h notice elapses —
// AdvanceDelisting drives that, so the notice window cannot be skipped
// by an early approval.
//
// Already-RESTRICTED instruments re-enter cleanly: the schedule refreshes
// and the notice counts from the earlier restriction (entered_at in the
// schedule), not from this call — an instrument restricted 26h ago files
// its delist request on the next sweep.
func (s *ListingService) RequestDelist(ctx context.Context, actor admin.AdminActor,
	symbol, reason string) (*DelistImpact, error) {

	if err := s.requireRole(ctx, actor.UserID, listApproveRoles, "delist request"); err != nil {
		return nil, err
	}
	if strings.TrimSpace(reason) == "" {
		return nil, excerrors.New("INVALID_REQUEST", "a reason is mandatory for delisting")
	}
	imp, err := s.PreviewDelist(ctx, symbol)
	if err != nil {
		return nil, err
	}
	switch imp.Status {
	case admin.InstDelisted, admin.InstDraft:
		return nil, excerrors.New("INVALID_LIFECYCLE_TRANSITION",
			"cannot delist from "+imp.Status)
	}

	// RESTRICTED leg through the lifecycle engine — owns the edge check,
	// the audit row and the engine-feed publication. A row already
	// RESTRICTED keeps its earlier entered_at for the notice clock.
	var enteredAt time.Time
	if imp.Status != admin.InstRestricted {
		inst, err := s.instruments.Transition(ctx, actor, imp.InstrumentID,
			admin.LcOpRestrict, admin.TransitionInput{
				Reason: "delist notice: " + reason})
		if err != nil {
			return nil, err
		}
		enteredAt = inst.UpdatedAt
	} else {
		var i *admin.Instrument
		if i, err = s.instruments.Get(ctx, imp.InstrumentID); err != nil {
			return nil, err
		}
		enteredAt = i.UpdatedAt
	}
	noticeUntil := enteredAt.Add(DelistNotice)

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, excerrors.Wrap("INTERNAL_ERROR", "begin tx", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := mergeDelistSchedule(ctx, tx, imp.InstrumentID, map[string]any{
		"phase":         "RESTRICTED_NOTICE",
		"restricted_at": enteredAt.UTC().Format(time.RFC3339),
		"notice_until":  noticeUntil.UTC().Format(time.RFC3339),
		"initiated_by":  actor.UserID,
		"reason":        reason,
		"impact":        imp,
	}); err != nil {
		return nil, err
	}
	if _, _, err := admin.Log(ctx, tx, admin.AuditEntry{
		AdminUserID: actor.UserID, Action: "instrument.delist.notice",
		TargetType: "instrument", TargetID: &imp.InstrumentID,
		AfterState: map[string]any{
			"symbol": imp.Symbol, "notice_until": noticeUntil,
			"impact": imp, "reason": reason,
		},
		IPAddress: actor.ClientIP,
	}); err != nil {
		return nil, excerrors.Wrap("INTERNAL_ERROR", "audit log", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, excerrors.Wrap("INTERNAL_ERROR", "commit", err)
	}
	if s.ws != nil {
		s.ws.Publish(admin.InstrumentStatusChannel, admin.InstrumentStatusEvent{
			Event: "DELIST_NOTICE", Symbol: imp.Symbol, To: admin.InstRestricted,
			Reason: reason, ActorID: actor.UserID,
			GraceDeadline: noticeUntil.UnixMilli(),
			Source:        "listing", TsMs: s.now().UnixMilli(),
		})
	}
	return imp, nil
}

// AdvanceDelisting drives the ladder one step per row per call —
// idempotent, safe to run every tick:
//
//	RESTRICTED_NOTICE + notice elapsed → submit OpInstrumentDelist
//	    (four-eyes; the sibling executor lands DELISTED on approval)
//	DELIST_PENDING_APPROVAL/any + status=DELISTED → CLOSE_ONLY +30d
//	status=DELISTED with no schedule → adopt (direct-admin delist)
//	CLOSE_ONLY + window elapsed → PURGE_READY marker (the force-close/
//	    purge itself is the Task 15.3.8 maker-checker's scope)
func (s *ListingService) AdvanceDelisting(ctx context.Context) error {
	type ladderRow struct {
		instID   int64
		symbol   string
		status   string
		schedule map[string]any
		updated  time.Time
	}
	rows, err := s.pool.Query(ctx, `
		SELECT r.instrument_id, r.symbol, i.status::text, r.delist_schedule,
		       i.updated_at
		  FROM instruments_reference r
		  JOIN instruments i ON i.id = r.instrument_id
		 WHERE r.delist_schedule <> '{}'::jsonb
		    OR i.status = 'DELISTED'`)
	if err != nil {
		return excerrors.Wrap("INTERNAL_ERROR", "delist ladder scan", err)
	}
	var list []ladderRow
	for rows.Next() {
		var l ladderRow
		var raw json.RawMessage
		if err := rows.Scan(&l.instID, &l.symbol, &l.status, &raw, &l.updated); err != nil {
			rows.Close()
			return excerrors.Wrap("INTERNAL_ERROR", "scan ladder row", err)
		}
		l.schedule = map[string]any{}
		_ = json.Unmarshal(raw, &l.schedule)
		list = append(list, l)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return excerrors.Wrap("INTERNAL_ERROR", "ladder rows", err)
	}

	for _, l := range list {
		if err := s.advanceOne(ctx, l.instID, l.symbol, l.status,
			l.schedule, l.updated); err != nil {
			s.logf("delist ladder %s: %v", l.symbol, err)
		}
	}
	return nil
}

func (s *ListingService) advanceOne(ctx context.Context, instID int64,
	symbol, status string, schedule map[string]any, updated time.Time) error {

	now := s.now()
	phase, _ := schedule["phase"].(string)

	// Adoption: DELISTED without a schedule (direct admin delist).
	if status == admin.InstDelisted && phase == "" {
		closeUntil := updated.Add(DelistCloseOnly)
		return s.writeSchedule(ctx, instID, symbol, map[string]any{
			"phase":            "CLOSE_ONLY",
			"delisted_at":      updated.UTC().Format(time.RFC3339),
			"close_only_until": closeUntil.UTC().Format(time.RFC3339),
			"adopted":          true,
		}, "instrument.delist.adopted", 0)
	}

	switch phase {
	case "RESTRICTED_NOTICE":
		until, _ := time.Parse(time.RFC3339,
			fmt.Sprint(schedule["notice_until"]))
		if now.Before(until) || status == admin.InstDelisted {
			return nil
		}
		if status != admin.InstRestricted {
			return nil // resumed/restricted-elsewhere — leave the schedule
		}
		if s.dual == nil {
			return excerrors.New("DUAL_CONTROL_REQUIRED",
				"delist requires the four-eyes queue — dual-control service unwired")
		}
		initiator, _ := schedule["initiated_by"].(float64)
		reason, _ := schedule["reason"].(string)
		dc, err := s.dual.Submit(ctx, admin.SubmitInput{
			Operation:    admin.OpInstrumentDelist,
			TargetType:   "instrument",
			TargetID:     strconv.FormatInt(instID, 10),
			RequiredRole: admin.RoleSuperAdmin,
			RequestedBy:  int64(initiator),
			Reason:       "delist after 24h notice: " + reason,
			Payload: map[string]any{
				"instrument_id": instID, "reason": reason,
			},
		})
		if err != nil {
			return err // retry next tick
		}
		return s.writeSchedule(ctx, instID, symbol, map[string]any{
			"phase":           "DELIST_PENDING_APPROVAL",
			"dual_control_id": dc.ID,
			"submitted_at":    now.UTC().Format(time.RFC3339),
		}, "instrument.delist.submitted", int64(initiator))

	case "DELIST_PENDING_APPROVAL":
		if status != admin.InstDelisted {
			return nil // still waiting on the approver
		}
		closeUntil := updated.Add(DelistCloseOnly)
		return s.writeSchedule(ctx, instID, symbol, map[string]any{
			"phase":            "CLOSE_ONLY",
			"delisted_at":      updated.UTC().Format(time.RFC3339),
			"close_only_until": closeUntil.UTC().Format(time.RFC3339),
		}, "instrument.delist.close_only", 0)

	case "CLOSE_ONLY":
		until, _ := time.Parse(time.RFC3339,
			fmt.Sprint(schedule["close_only_until"]))
		if now.Before(until) || status != admin.InstDelisted {
			return nil
		}
		return s.writeSchedule(ctx, instID, symbol, map[string]any{
			"phase":          "PURGE_READY",
			"purge_ready_at": now.UTC().Format(time.RFC3339),
		}, "instrument.delist.purge_ready", 0)
	}
	return nil
}

// writeSchedule merges the patch and writes the audit row in one tx.
// actorID 0 = system-driven step (admin_user_id carries no FK; the
// after_state records system:true for attribution).
func (s *ListingService) writeSchedule(ctx context.Context, instID int64,
	symbol string, patch map[string]any, action string, actorID int64) error {

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return excerrors.Wrap("INTERNAL_ERROR", "begin tx", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := mergeDelistSchedule(ctx, tx, instID, patch); err != nil {
		return err
	}
	after, _ := json.Marshal(patch)
	var auditID int64
	if err := tx.QueryRow(ctx, `
		INSERT INTO admin_audit_log
		    (admin_user_id, action, target_type, target_id, after_state)
		VALUES ($1,$2,'instrument',$3,$4) RETURNING id`,
		actorID, action, instID, json.RawMessage(after)).Scan(&auditID); err != nil {
		return excerrors.Wrap("INTERNAL_ERROR", "audit insert", err)
	}
	if _, err := audit.Append(ctx, tx, "admin_audit_log", &auditID, "INSERT", nil); err != nil {
		return excerrors.Wrap("INTERNAL_ERROR", "audit chain", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return excerrors.Wrap("INTERNAL_ERROR", "commit", err)
	}
	if s.ws != nil {
		s.ws.Publish(admin.InstrumentStatusChannel, admin.InstrumentStatusEvent{
			Event: "DELIST_LADDER", Symbol: symbol,
			Reason: fmt.Sprint(patch["phase"]),
			Source: "listing", TsMs: s.now().UnixMilli(),
		})
	}
	return nil
}
