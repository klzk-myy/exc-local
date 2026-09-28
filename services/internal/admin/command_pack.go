// command_pack.go — Phase-07 Task 7.3.13, spec §7.6/§5.43, §24 #378.
//
// CEO_DAILY executive roll-up: a 06:00 UTC job assembles one pack from
// owned live sources — no new data collection, only assembly. Each
// section is read through the PackSource seam; a source that errors or
// is not yet provisioned marks its section STALE (CEO packs never block,
// per the task edge cases). The rendered content is marshalled
// deterministically and SHA-256 hashed — "what the executive saw is
// provable" — and the row is hash-chained via prev_pack_hash.
//
// The same machinery serves Task 7.3.14 (board_pack.go adds the
// BOARD_QUARTERLY/BOARD_ADHOC source sets plus dual-controlled release).
package admin

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"exchange/internal/audit"
	excerrors "exchange/pkg/errors"
)

// Pack kinds (spec §5.43).
const (
	PackKindCEODaily       = "CEO_DAILY"
	PackKindBoardQuarterly = "BOARD_QUARTERLY"
	PackKindBoardAdhoc     = "BOARD_ADHOC"
)

func validPackKind(k string) bool {
	switch k {
	case PackKindCEODaily, PackKindBoardQuarterly, PackKindBoardAdhoc:
		return true
	}
	return false
}

// Pack statuses.
const (
	PackGenerated = "GENERATED"
	PackReleased  = "RELEASED"
)

// Section statuses: OK = live read; STALE = source errored/late (CEO
// packs); ABSENT = source table not provisioned yet (board packs mark
// ABSENT with owner + due date and still release, per 7.3.14 edge cases).
const (
	SectionOK     = "OK"
	SectionStale  = "STALE"
	SectionAbsent = "ABSENT"
)

// PackPeriod is the reporting window the pack covers.
type PackPeriod struct {
	Label string    `json:"label"` // 'YYYY-MM-DD' | 'YYYYQn' | ad-hoc label
	Start time.Time `json:"start"`
	End   time.Time `json:"end"`
}

// DailyPeriod covers one UTC calendar day (the 06:00 job assembles the
// prior day).
func DailyPeriod(day time.Time) PackPeriod {
	d := time.Date(day.UTC().Year(), day.UTC().Month(), day.UTC().Day(),
		0, 0, 0, 0, time.UTC)
	return PackPeriod{
		Label: d.Format("2006-01-02"),
		Start: d, End: d.Add(24 * time.Hour),
	}
}

// QuarterlyPeriod covers a calendar quarter; quarter must be 1–4.
func QuarterlyPeriod(year, quarter int) (PackPeriod, error) {
	if quarter < 1 || quarter > 4 || year < 2000 || year > 2100 {
		return PackPeriod{}, excerrors.New("INVALID_REQUEST",
			"quarterly period needs year (2000–2100) and quarter (1–4)")
	}
	start := time.Date(year, time.Month(3*(quarter-1)+1), 1, 0, 0, 0, 0, time.UTC)
	return PackPeriod{
		Label: fmt.Sprintf("%dQ%d", year, quarter),
		Start: start, End: start.AddDate(0, 3, 0),
	}, nil
}

// AdhocPeriod labels an emergency/off-cycle pack (Task 7.3.14 edge case).
func AdhocPeriod(label string) (PackPeriod, error) {
	l := strings.TrimSpace(label)
	if l == "" || len(l) > 32 {
		return PackPeriod{}, excerrors.New("INVALID_REQUEST",
			"ad-hoc pack requires a 1–32 char period label")
	}
	return PackPeriod{Label: l}, nil
}

// PackSection is one rendered section.
type PackSection struct {
	Name          string         `json:"name"`
	Status        string         `json:"status"` // OK | STALE | ABSENT
	Data          map[string]any `json:"data,omitempty"`
	SourceVersion string         `json:"source_version,omitempty"`
	Owner         string         `json:"owner,omitempty"`    // owning task (ABSENT marking)
	DueDate       string         `json:"due_date,omitempty"` // narrative due date (ABSENT marking)
	Error         string         `json:"error,omitempty"`
}

// errSourceAbsent marks a source whose backing store is not provisioned
// yet (the owning phase has not landed) — board packs mark these ABSENT;
// CEO packs mark all source failures STALE. It is a plain sentinel (not a
// coded error): source failures are rendered as section status and never
// escape to the HTTP layer, so no §23 registry row is needed.
var errSourceAbsent = errors.New("pack source not provisioned")

// PackSource reads ONE pack section from a live store. Implementations
// never fabricate: they read real tables or return an error. The returned
// sourceVersion (e.g. "pg:trades@max(id)=12345") is recorded per section
// so pack regeneration is reproducible (Task 7.3.14 item 3).
type PackSource interface {
	Section() string
	Owner() string // owning task ref, e.g. "Phase-09 Task 9.3.18"
	Collect(ctx context.Context, p PackPeriod) (map[string]any, string, error)
}

// PackDeliverySink delivers a generated pack to executives (dashboard
// publish / email). nil → delivery recorded as QUEUED (CEO binding
// absent or transport down must never block generation — task edge).
type PackDeliverySink interface {
	DeliverPack(ctx context.Context, p GovernancePack) (channel string, err error)
}

// PackDelivery is one recorded delivery attempt (deliveries JSONB).
type PackDelivery struct {
	Channel string    `json:"channel"` // "dashboard" | "email" | "queued"
	At      time.Time `json:"at"`
	OK      bool      `json:"ok"`
	Err     string    `json:"err,omitempty"`
}

// GovernancePack is one governance_packs row.
type GovernancePack struct {
	PackID             int64           `json:"pack_id"`
	Kind               string          `json:"kind"`
	Period             string          `json:"period"`
	Status             string          `json:"status"`
	Content            json.RawMessage `json:"content"`
	ContentHash        string          `json:"content_hash"`
	PrevPackHash       string          `json:"prev_pack_hash,omitempty"`
	SourceVersions     json.RawMessage `json:"source_versions"`
	GeneratedAt        time.Time       `json:"generated_at"`
	GeneratedBy        string          `json:"generated_by"`
	ReleaseInitiatedBy *int64          `json:"release_initiated_by,omitempty"`
	ReleaseInitiatedAt *time.Time      `json:"release_initiated_at,omitempty"`
	ReleasedBy         *int64          `json:"released_by,omitempty"`
	ReleasedAt         *time.Time      `json:"released_at,omitempty"`
	ReleaseReason      string          `json:"release_reason,omitempty"`
	Deliveries         json.RawMessage `json:"deliveries,omitempty"`
}

// packContent is the canonical rendered form — the hashed object. Field
// order in the struct plus encoding/json's sorted map keys make the hash
// deterministic for identical inputs.
type packContent struct {
	Kind        string        `json:"kind"`
	Period      string        `json:"period"`
	PeriodStart string        `json:"period_start"`
	PeriodEnd   string        `json:"period_end"`
	GeneratedAt string        `json:"generated_at"`
	GeneratedBy string        `json:"generated_by"`
	Sections    []PackSection `json:"sections"`
}

// PackContentHash computes the sha256 hex of canonical pack content —
// the "what the executive saw is provable" pin.
func PackContentHash(content json.RawMessage) string {
	sum := sha256.Sum256(content)
	return hex.EncodeToString(sum[:])
}

// ---------------------------------------------------------------------------
// Store seam
// ---------------------------------------------------------------------------

type packStore interface {
	insertPack(ctx context.Context, p GovernancePack) (*GovernancePack, error)
	getPack(ctx context.Context, packID int64) (*GovernancePack, error)
	listPacks(ctx context.Context, kind string, releasedOnly bool, limit int) ([]GovernancePack, error)
	latestPackHash(ctx context.Context, kind string) (string, error)
	recordDelivery(ctx context.Context, packID int64, d PackDelivery) error
	releasePack(ctx context.Context, packID int64, initiator, approver int64, reason, clientIP string) (*GovernancePack, error)
}

type pgPackStore struct{ pool *pgxpool.Pool }

const packColumns = `pack_id, kind, period, status, content, content_hash,
                     COALESCE(prev_pack_hash,''), source_versions, generated_at,
                     generated_by, release_initiated_by, release_initiated_at,
                     released_by, released_at, COALESCE(release_reason,''), deliveries`

func scanPack(row pgx.Row) (*GovernancePack, error) {
	var p GovernancePack
	err := row.Scan(&p.PackID, &p.Kind, &p.Period, &p.Status, &p.Content,
		&p.ContentHash, &p.PrevPackHash, &p.SourceVersions, &p.GeneratedAt,
		&p.GeneratedBy, &p.ReleaseInitiatedBy, &p.ReleaseInitiatedAt,
		&p.ReleasedBy, &p.ReleasedAt, &p.ReleaseReason, &p.Deliveries)
	if err != nil {
		return nil, err
	}
	return &p, nil
}

func (s *pgPackStore) insertPack(ctx context.Context, p GovernancePack) (*GovernancePack, error) {
	var prev any
	if p.PrevPackHash != "" {
		prev = p.PrevPackHash
	}
	row := s.pool.QueryRow(ctx, `
		INSERT INTO governance_packs
		    (kind, period, content, content_hash, prev_pack_hash,
		     source_versions, generated_by)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
		RETURNING `+packColumns,
		p.Kind, p.Period, p.Content, p.ContentHash, prev,
		p.SourceVersions, p.GeneratedBy)
	out, err := scanPack(row)
	if err != nil {
		return nil, excerrors.Wrap("INTERNAL_ERROR", "insert pack", err)
	}
	return out, nil
}

func (s *pgPackStore) getPack(ctx context.Context, packID int64) (*GovernancePack, error) {
	p, err := scanPack(s.pool.QueryRow(ctx,
		`SELECT `+packColumns+` FROM governance_packs WHERE pack_id = $1`, packID))
	if err == pgx.ErrNoRows {
		return nil, excerrors.New("NOT_FOUND",
			fmt.Sprintf("governance pack %d not found", packID))
	}
	if err != nil {
		return nil, excerrors.Wrap("INTERNAL_ERROR", "read pack", err)
	}
	return p, nil
}

func (s *pgPackStore) listPacks(ctx context.Context, kind string, releasedOnly bool, limit int) ([]GovernancePack, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	q := `SELECT ` + packColumns + ` FROM governance_packs WHERE true`
	args := []any{}
	if kind != "" {
		args = append(args, kind)
		q += fmt.Sprintf(` AND kind = $%d`, len(args))
	}
	if releasedOnly {
		q += ` AND status = 'RELEASED'`
	}
	args = append(args, limit)
	q += fmt.Sprintf(` ORDER BY pack_id DESC LIMIT $%d`, len(args))
	rows, err := s.pool.Query(ctx, q, args...)
	if err != nil {
		return nil, excerrors.Wrap("INTERNAL_ERROR", "list packs", err)
	}
	defer rows.Close()
	out := []GovernancePack{}
	for rows.Next() {
		var p GovernancePack
		if err := rows.Scan(&p.PackID, &p.Kind, &p.Period, &p.Status, &p.Content,
			&p.ContentHash, &p.PrevPackHash, &p.SourceVersions, &p.GeneratedAt,
			&p.GeneratedBy, &p.ReleaseInitiatedBy, &p.ReleaseInitiatedAt,
			&p.ReleasedBy, &p.ReleasedAt, &p.ReleaseReason, &p.Deliveries); err != nil {
			return nil, excerrors.Wrap("INTERNAL_ERROR", "scan pack", err)
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// latestPackHash is the chain link: the content_hash of the most recent
// pack of the same kind (generated or released — the chain covers every
// hash-pinned snapshot, not just released ones).
func (s *pgPackStore) latestPackHash(ctx context.Context, kind string) (string, error) {
	var h string
	err := s.pool.QueryRow(ctx, `
		SELECT content_hash FROM governance_packs
		 WHERE kind = $1 ORDER BY pack_id DESC LIMIT 1`, kind).Scan(&h)
	if err == pgx.ErrNoRows {
		return "", nil
	}
	if err != nil {
		return "", excerrors.Wrap("INTERNAL_ERROR", "read pack chain tail", err)
	}
	return h, nil
}

func (s *pgPackStore) recordDelivery(ctx context.Context, packID int64, d PackDelivery) error {
	body, _ := json.Marshal(d)
	tag, err := s.pool.Exec(ctx, `
		UPDATE governance_packs
		   SET deliveries = deliveries || $2::jsonb
		 WHERE pack_id = $1`, packID, "["+string(body)+"]")
	if err != nil {
		return excerrors.Wrap("INTERNAL_ERROR", "record delivery", err)
	}
	if tag.RowsAffected() == 0 {
		return excerrors.New("NOT_FOUND",
			fmt.Sprintf("governance pack %d not found", packID))
	}
	return nil
}

// releasePack is the dual-controlled release transaction: the row is
// locked and must still be GENERATED; both approver ids land on the row
// (schema CHECK enforces distinctness) and the admin_audit_log row +
// audit_hash_chain link commit atomically with it. The DB trigger makes
// any post-release mutation impossible.
func (s *pgPackStore) releasePack(ctx context.Context, packID int64,
	initiator, approver int64, reason, clientIP string) (*GovernancePack, error) {

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, excerrors.Wrap("INTERNAL_ERROR", "begin tx", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var status, kind string
	err = tx.QueryRow(ctx,
		`SELECT status, kind FROM governance_packs WHERE pack_id = $1 FOR UPDATE`,
		packID).Scan(&status, &kind)
	if err == pgx.ErrNoRows {
		return nil, excerrors.New("NOT_FOUND",
			fmt.Sprintf("governance pack %d not found", packID))
	}
	if err != nil {
		return nil, excerrors.Wrap("INTERNAL_ERROR", "lock pack", err)
	}
	if kind == PackKindCEODaily {
		return nil, excerrors.New("INVALID_REQUEST",
			"CEO_DAILY packs are generated-only — release applies to board packs")
	}
	if status != PackGenerated {
		return nil, excerrors.New("INVALID_REQUEST",
			fmt.Sprintf("pack %d is %s — only GENERATED packs can be released", packID, status))
	}

	var out *GovernancePack
	row := tx.QueryRow(ctx, `
		UPDATE governance_packs
		   SET status = 'RELEASED',
		       release_initiated_by = $2, release_initiated_at = now(),
		       released_by = $3, released_at = now(), release_reason = $4
		 WHERE pack_id = $1
		RETURNING `+packColumns, packID, initiator, approver, reason)
	if out, err = scanPack(row); err != nil {
		return nil, excerrors.Wrap("INTERNAL_ERROR", "release pack", err)
	}

	after, _ := json.Marshal(map[string]any{
		"status": "RELEASED", "released_by": approver,
		"initiated_by": initiator, "content_hash": out.ContentHash,
	})
	if err := insertAdminAudit(ctx, tx, initiator, "governance_pack.release",
		"governance_pack", packID,
		json.RawMessage(`{"status":"GENERATED"}`), json.RawMessage(after), clientIP); err != nil {
		return nil, excerrors.Wrap("INTERNAL_ERROR", "admin audit insert", err)
	}

	// Hash-chain link (spec §5.8) in the same transaction — the release
	// cannot commit without its WORM trail. payload stays nil so the row
	// is self-verifiable by `exchange verify-audit` (audit.go convention).
	if _, err := audit.Append(ctx, tx, "governance_packs", &packID, "UPDATE",
		nil); err != nil {
		return nil, excerrors.Wrap("INTERNAL_ERROR", "audit chain append", err)
	}

	if err := tx.Commit(ctx); err != nil {
		// The partial unique index (kind,period) WHERE RELEASED makes a
		// second release of the same period fail here.
		if isUniqueViolation(err) {
			return nil, excerrors.New("INVALID_REQUEST",
				fmt.Sprintf("a %s pack for period %s is already released", kind, out.Period))
		}
		return nil, excerrors.Wrap("INTERNAL_ERROR", "commit release", err)
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// Service
// ---------------------------------------------------------------------------

// GovernancePackService assembles, persists and releases governance
// packs. Sources are injected per kind — Task 7.3.13 wires the CEO set,
// 7.3.14 the board set (both live on this service; board_pack.go carries
// the board-specific surface).
type GovernancePackService struct {
	store        packStore
	resolver     AdminRoleResolver
	ceoSources   []PackSource
	boardSources []PackSource
	delivery     PackDeliverySink
	approver     DualControlApprover // nil → roleDualControl{resolver}
	now          func() time.Time
}

// NewGovernancePackService wires the production service. ceoSources /
// boardSources default to the live-PG sets (ceoDefaultSources /
// boardDefaultSources); delivery may be nil (QUEUED marking).
func NewGovernancePackService(pool *pgxpool.Pool, resolver AdminRoleResolver,
	ops OpsSource, delivery PackDeliverySink) *GovernancePackService {
	return newGovernancePackService(&pgPackStore{pool: pool}, resolver,
		ceoDefaultSources(pool, ops), boardDefaultSources(pool), delivery)
}

// newGovernancePackService is the unit-test seam.
func newGovernancePackService(store packStore, resolver AdminRoleResolver,
	ceo, board []PackSource, delivery PackDeliverySink) *GovernancePackService {
	return &GovernancePackService{
		store: store, resolver: resolver, ceoSources: ceo,
		boardSources: board, delivery: delivery, now: time.Now,
	}
}

// sourcesFor returns the section source set for a pack kind.
func (s *GovernancePackService) sourcesFor(kind string) []PackSource {
	if kind == PackKindCEODaily {
		return s.ceoSources
	}
	return s.boardSources
}

// Generate assembles a pack from live sources and persists it
// hash-pinned + hash-chained. On-demand (actor-driven) generation
// requires Super Admin per the route registry; the scheduled job calls
// GenerateScheduled (generated_by "system") which skips the gate.
func (s *GovernancePackService) Generate(ctx context.Context, actor AdminActor,
	kind string, period PackPeriod) (*GovernancePack, error) {
	if !validPackKind(kind) {
		return nil, excerrors.New("INVALID_REQUEST", "kind must be CEO_DAILY|BOARD_QUARTERLY|BOARD_ADHOC")
	}
	if err := s.requireGenerateRole(ctx, actor); err != nil {
		return nil, err
	}
	return s.generate(ctx, kind, period, fmt.Sprintf("admin:%d", actor.UserID))
}

// GenerateScheduled is the 06:00 UTC job entry point — assembly-only, no
// role gate (no caller identity); generated_by records "system".
func (s *GovernancePackService) GenerateScheduled(ctx context.Context,
	kind string, period PackPeriod) (*GovernancePack, error) {
	if !validPackKind(kind) {
		return nil, excerrors.New("INVALID_REQUEST", "kind must be CEO_DAILY|BOARD_QUARTERLY|BOARD_ADHOC")
	}
	return s.generate(ctx, kind, period, "system")
}

func (s *GovernancePackService) requireGenerateRole(ctx context.Context, actor AdminActor) error {
	if actor.UserID == 0 {
		return excerrors.New("UNAUTHORIZED", "admin identity required")
	}
	if s.resolver == nil {
		return excerrors.New("UNAUTHORIZED_ROLE",
			"role resolver not configured (Phase-07 RBAC seam)")
	}
	role, err := s.resolver(ctx, actor.UserID)
	if err != nil {
		return excerrors.Wrap("INTERNAL_ERROR", "role lookup", err)
	}
	if !packReleaseRoles[role] && role != "Super Admin" {
		return excerrors.New("UNAUTHORIZED_ROLE",
			fmt.Sprintf("governance pack generation requires Super Admin (got %q)", role))
	}
	return nil
}

// generate runs the assembly: every source is collected; failures mark
// the section STALE (CEO) or ABSENT-with-owner+due (board packs, source
// not provisioned) — the pack never blocks on a missing input.
func (s *GovernancePackService) generate(ctx context.Context, kind string,
	period PackPeriod, generatedBy string) (*GovernancePack, error) {

	sources := s.sourcesFor(kind)
	sections := make([]PackSection, 0, len(sources))
	versions := map[string]string{}
	for _, src := range sources {
		sec := PackSection{Name: src.Section(), Owner: src.Owner()}
		data, version, err := src.Collect(ctx, period)
		switch {
		case err == nil:
			sec.Status = SectionOK
			sec.Data = data
			sec.SourceVersion = version
			versions[src.Section()] = version
		case errorsIsAbsent(err) && kind != PackKindCEODaily:
			sec.Status = SectionAbsent
			// Narrative due date: quarter/period end + 30 days (board
			// reporting convention; not spec-pinned — §27 note).
			due := period.End
			if due.IsZero() {
				due = s.now().UTC()
			}
			sec.DueDate = due.AddDate(0, 0, 30).Format("2006-01-02")
		default:
			sec.Status = SectionStale
			sec.Error = err.Error()
		}
		sections = append(sections, sec)
	}

	now := s.now().UTC()
	content := packContent{
		Kind: kind, Period: period.Label,
		PeriodStart: period.Start.Format(time.RFC3339),
		PeriodEnd:   period.End.Format(time.RFC3339),
		GeneratedAt: now.Format(time.RFC3339),
		GeneratedBy: generatedBy,
		Sections:    sections,
	}
	body, err := json.Marshal(content)
	if err != nil {
		return nil, excerrors.Wrap("INTERNAL_ERROR", "render pack", err)
	}
	verJSON, _ := json.Marshal(versions)

	prev, err := s.store.latestPackHash(ctx, kind)
	if err != nil {
		return nil, err
	}
	pack := GovernancePack{
		Kind: kind, Period: period.Label, Status: PackGenerated,
		Content: body, ContentHash: PackContentHash(body),
		PrevPackHash: prev, SourceVersions: verJSON,
		GeneratedBy: generatedBy,
	}
	stored, err := s.store.insertPack(ctx, pack)
	if err != nil {
		return nil, err
	}

	// Delivery: dashboard/email sink; absent or failing transport records
	// a QUEUED delivery — generation already succeeded and is provable.
	s.deliver(ctx, stored)
	return stored, nil
}

// deliver pushes the pack to the executive surface and records the
// receipt on the row (deliveries JSONB).
func (s *GovernancePackService) deliver(ctx context.Context, p *GovernancePack) {
	d := PackDelivery{At: s.now().UTC()}
	if s.delivery == nil {
		d.Channel, d.OK, d.Err = "queued", false, "no delivery sink wired"
	} else if ch, err := s.delivery.DeliverPack(ctx, *p); err != nil {
		d.Channel, d.OK, d.Err = "queued", false, err.Error()
	} else {
		d.Channel, d.OK = ch, true
	}
	if err := s.store.recordDelivery(ctx, p.PackID, d); err == nil {
		p.Deliveries = json.RawMessage(`[` + mustJSON(d) + `]`)
	}
}

// Get returns one pack. Auditor roles (Read-Only Auditor /
// EXTERNAL_AUDITOR) may read RELEASED packs and CEO_DAILY roll-ups only —
// draft board packs stay Super Admin / Risk Manager.
func (s *GovernancePackService) Get(ctx context.Context, actor AdminActor, packID int64) (*GovernancePack, error) {
	if packID <= 0 {
		return nil, excerrors.New("INVALID_REQUEST", "pack_id must be a positive integer")
	}
	role, err := s.resolvePackRole(ctx, actor)
	if err != nil {
		return nil, err
	}
	p, err := s.store.getPack(ctx, packID)
	if err != nil {
		return nil, err
	}
	if !packReadAllRoles[role] {
		if p.Status != PackReleased && p.Kind != PackKindCEODaily {
			return nil, excerrors.New("FORBIDDEN",
				"auditor roles may read released board packs only")
		}
	}
	return p, nil
}

// List returns packs (auditor roles are restricted to released board
// packs + all CEO roll-ups).
func (s *GovernancePackService) List(ctx context.Context, actor AdminActor,
	kind string, limit int) ([]GovernancePack, error) {
	role, err := s.resolvePackRole(ctx, actor)
	if err != nil {
		return nil, err
	}
	if kind != "" && !validPackKind(kind) {
		return nil, excerrors.New("INVALID_REQUEST", "kind must be CEO_DAILY|BOARD_QUARTERLY|BOARD_ADHOC")
	}
	releasedOnly := !packReadAllRoles[role]
	packs, err := s.store.listPacks(ctx, kind, releasedOnly, limit)
	if err != nil {
		return nil, err
	}
	if releasedOnly {
		// CEO_DAILY packs never release — keep them visible to auditors
		// (they are the record of what executives saw).
		out := packs[:0]
		for _, p := range packs {
			if p.Status == PackReleased || p.Kind == PackKindCEODaily {
				out = append(out, p)
			}
		}
		packs = out
	}
	return packs, nil
}

// resolvePackRole gates pack reads: broad-read roles or auditor roles.
func (s *GovernancePackService) resolvePackRole(ctx context.Context, actor AdminActor) (string, error) {
	if actor.UserID == 0 {
		return "", excerrors.New("UNAUTHORIZED", "admin identity required")
	}
	if s.resolver == nil {
		return "", excerrors.New("UNAUTHORIZED_ROLE",
			"role resolver not configured (Phase-07 RBAC seam)")
	}
	role, err := s.resolver(ctx, actor.UserID)
	if err != nil {
		return "", excerrors.Wrap("INTERNAL_ERROR", "role lookup", err)
	}
	if !packReadAllRoles[role] && !packAuditorRoles[role] {
		return "", excerrors.New("UNAUTHORIZED_ROLE",
			fmt.Sprintf("role %q may not read governance packs", role))
	}
	return role, nil
}

// VerifyHash recomputes the content hash — callers can prove the stored
// snapshot is unmodified.
func VerifyHash(p *GovernancePack) bool {
	return p != nil && PackContentHash(p.Content) == p.ContentHash
}

// errorsIsAbsent reports whether the error is the absent-source sentinel.
func errorsIsAbsent(err error) bool {
	return errors.Is(err, errSourceAbsent)
}

func mustJSON(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

// ---------------------------------------------------------------------------
// Live PG sources (Task 7.3.13 source set; spec §7.6.1)
// ---------------------------------------------------------------------------

// OpsSource is the ops-health seam for the CEO pack's ops section: the
// current degradation mode (Phase-02 ModeManager / Redis
// system:degradation:* record — same shape middleware.DegradationReader
// consumes) and SLO burn. nil → section STALE.
type OpsSource interface {
	OpsSnapshot(ctx context.Context, p PackPeriod) (map[string]any, string, error)
}

// sqlSectionSource is the generic live-read source: it probes a backing
// table (to_regclass) then runs a query expected to return exactly one
// JSONB row (SELECT to_jsonb(t) FROM (...) t). Missing table →
// errSourceAbsent (board packs mark ABSENT); any other failure → STALE.
type sqlSectionSource struct {
	pool    *pgxpool.Pool
	section string
	owner   string
	probe   string // table whose absence means "not provisioned"
	query   string // single-row to_jsonb query; $1=period start, $2=period end
}

func (s sqlSectionSource) Section() string { return s.section }
func (s sqlSectionSource) Owner() string   { return s.owner }

func (s sqlSectionSource) Collect(ctx context.Context, p PackPeriod) (map[string]any, string, error) {
	var reg *string
	if err := s.pool.QueryRow(ctx, `SELECT to_regclass($1)::text`, s.probe).Scan(&reg); err != nil {
		return nil, "", fmt.Errorf("probe %s: %w", s.probe, err)
	}
	if reg == nil {
		return nil, "", fmt.Errorf("%w: table %s (%s)", errSourceAbsent, s.probe, s.owner)
	}
	var raw json.RawMessage
	if err := s.pool.QueryRow(ctx, s.query, p.Start, p.End).Scan(&raw); err != nil {
		return nil, "", fmt.Errorf("collect %s: %w", s.section, err)
	}
	var data map[string]any
	if err := json.Unmarshal(raw, &data); err != nil {
		return nil, "", fmt.Errorf("decode %s: %w", s.section, err)
	}
	// Source version = the probe table plus the newest row id it saw —
	// "pg:<table>@max_id=<n>" — so a regenerated pack is reproducible.
	ver := fmt.Sprintf("pg:%s@max_id=%v", s.probe, data["max_id"])
	return data, ver, nil
}

// opsSourceSection adapts the OpsSource seam to a PackSource.
type opsSourceSection struct{ ops OpsSource }

func (o opsSourceSection) Section() string { return "ops" }
func (o opsSourceSection) Owner() string   { return "Phase-02 ModeManager / Phase-09 Task 9.3.18" }
func (o opsSourceSection) Collect(ctx context.Context, p PackPeriod) (map[string]any, string, error) {
	if o.ops == nil {
		return nil, "", fmt.Errorf("%w: ops source not wired", errSourceAbsent)
	}
	return o.ops.OpsSnapshot(ctx, p)
}

// ceoDefaultSources is the Task 7.3.13 source set: overnight incidents +
// open RCAs, treasury buffer, regulatory-change queue, finance KPIs,
// risk, ops — plus the venue-health roll-ups the parent spec calls out
// (trade volume, active users, open tickets, open LP alerts). Probes
// name the owning table; unprovisioned stores surface STALE.
func ceoDefaultSources(pool *pgxpool.Pool, ops OpsSource) []PackSource {
	return []PackSource{
		sqlSectionSource{pool, "incidents", "Phase-09 Task 9.3.18", "incidents",
			`SELECT to_jsonb(t) FROM (
			    SELECT count(*) FILTER (WHERE severity IN ('P0','P1')
			             AND created_at >= $1 AND created_at < $2) AS overnight_p0_p1,
			           count(*) FILTER (WHERE rca_status = 'OPEN') AS open_rcas,
			           COALESCE(max(id),0) AS max_id
			      FROM incidents) t`},
		sqlSectionSource{pool, "treasury", "Phase-24 Task 24.3.17", "treasury_own_funds",
			`SELECT to_jsonb(t) FROM (
			    SELECT COALESCE(sum(balance),0)::text AS own_funds_total,
			           COALESCE(min(target),0)::text AS buffer_target,
			           COALESCE(max(id),0) AS max_id
			      FROM treasury_own_funds) t`},
		sqlSectionSource{pool, "regulatory_changes", "Phase-21 Task 21.3.25", "regulatory_changes",
			`SELECT to_jsonb(t) FROM (
			    SELECT count(*) FILTER (WHERE triage_due_at < now()
			             AND status NOT IN ('CLOSED','IMPLEMENTED')) AS past_triage_sla,
			           count(*) AS queue_depth,
			           COALESCE(max(id),0) AS max_id
			      FROM regulatory_changes) t`},
		sqlSectionSource{pool, "finance", "Phase-20 Task 20.3.7 / Phase-19 Task 19.3.14", "trades",
			`SELECT to_jsonb(t) FROM (
			    SELECT COALESCE(sum(buyer_fee + seller_fee),0)::text AS fee_revenue_period,
			           (SELECT COALESCE(sum(total),0)::text FROM balances) AS client_equity,
			           (SELECT COALESCE(sum(balance),0)::text FROM insurance_fund) AS insurance_fund,
			           COALESCE(max(id),0) AS max_id
			      FROM trades WHERE created_at >= $1 AND created_at < $2) t`},
		sqlSectionSource{pool, "risk", "Phase-19 Task 19.3.13", "margin_validation_runs",
			`SELECT to_jsonb(t) FROM (
			    SELECT count(*) FILTER (WHERE status <> 'PASS'
			             AND created_at >= $1 AND created_at < $2) AS margin_validation_failures,
			           (SELECT count(*) FROM nbp_events
			             WHERE created_at >= $1 AND created_at < $2) AS nbp_events,
			           COALESCE(max(id),0) AS max_id
			      FROM margin_validation_runs) t`},
		opsSourceSection{ops},
		sqlSectionSource{pool, "trade_volume", "Phase-03 settlement", "trades",
			`SELECT to_jsonb(t) FROM (
			    SELECT count(*) AS trades,
			           COALESCE(sum(quantity),0)::text AS base_volume,
			           count(DISTINCT instrument_id) AS instruments_traded,
			           COALESCE(max(id),0) AS max_id
			      FROM trades WHERE created_at >= $1 AND created_at < $2) t`},
		sqlSectionSource{pool, "active_users", "Phase-12 accounts", "accounts",
			`SELECT to_jsonb(t) FROM (
			    SELECT count(DISTINCT aid) AS active_accounts,
			           (SELECT count(*) FROM accounts WHERE status='ACTIVE') AS total_active,
			           (SELECT COALESCE(max(id),0) FROM accounts) AS max_id
			      FROM (
			        SELECT account_id AS aid FROM orders
			          WHERE created_at >= $1 AND created_at < $2
			        UNION
			        SELECT buyer_account_id FROM trades
			          WHERE created_at >= $1 AND created_at < $2
			        UNION
			        SELECT seller_account_id FROM trades
			          WHERE created_at >= $1 AND created_at < $2) act) t`},
		sqlSectionSource{pool, "support", "Phase-07 Task 7.3.7", "support_tickets",
			`SELECT to_jsonb(t) FROM (
			    SELECT count(*) FILTER (WHERE status IN ('OPEN','PENDING','IN_PROGRESS')) AS open_tickets,
			           count(*) FILTER (WHERE queue='COMPLIANCE' AND status IN ('OPEN','PENDING','IN_PROGRESS')) AS open_complaints,
			           count(*) FILTER (WHERE status NOT IN ('RESOLVED','CLOSED') AND sla_due_at < now()) AS sla_breaches,
			           COALESCE(max(id),0) AS max_id
			      FROM support_tickets) t`},
		sqlSectionSource{pool, "alerts", "Phase-07 Task 7.3.9/7.3.10", "lp_performance_alerts",
			`SELECT to_jsonb(t) FROM (
			    SELECT count(*) FILTER (WHERE status='OPEN') AS open_alerts,
			           count(*) FILTER (WHERE emitted_at >= $1 AND emitted_at < $2) AS emitted_in_period,
			           COALESCE(max(id),0) AS max_id
			      FROM lp_performance_alerts) t`},
	}
}
