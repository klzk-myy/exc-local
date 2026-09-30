// Phase-09 Task 9.3.29 item 4 (spec §19.14, §24 #340) — the secrets
// inventory register backed by migration 089's `secrets_inventory`
// table. This is the metadata half of the ops procedure in
// docs/ops/secrets-inventory.md: one row per secret (banking API keys,
// FIX mTLS certs, OAuth secrets, KMS grants, alerting keys, data-path
// credentials) carrying owner, TTL, rotation procedure and the
// last-rotated timestamp the rotation-SLA enforcer evaluates.
//
// Layout:
//   - PgInventoryStore — the table's CRUD: Upsert/Get/List/
//     LastRotated/MarkRotated plus the overdue join
//     (last_rotated_at + ttl_seconds < now). Store methods accept a
//     pgQuerier so the service can run row mutation + admin_audit_log +
//     hash-chain link inside one transaction (spec §5.9 — a secrets
//     register change cannot commit without its audit trail).
//   - InventoryService — role gate (doc §5: reads/writes restricted to
//     the security-operating lead — the §8.2 canon carries no dedicated
//     Security/SRE role, so Super Admin is the gate), fail-closed input
//     validation (per-row TTL may tighten but never exceed the class
//     MaxAge ceiling from the Phase-13.5 rotation Registry), the
//     coverage check ("every secret inventoried" leg), and the
//     overdue evaluator that drives the SECRET_ROTATION_OVERDUE
//     emission.
//
// Emission contract (spec §23): a rotation-SLA breach raises
// SECRET_ROTATION_OVERDUE (HTTP 503). Two surfaces share the one
// evaluation: the admin read endpoint answers 503 with the overdue
// set while any row is overdue (the enforcer surface spec §19.14
// prescribes), and EvaluateRotation pages P2 once per overdue secret
// through the Alerter seam until a MarkRotated clears it (doc §3 step
// 5 — the evaluator clears on next pass).
//
// Emergency rotation is a mark, not a mechanism: the leak runbook
// (rotate-secrets.sh) does the revoke/reissue at the source, then
// records completion here via MarkRotated{Emergency: true,
// IncidentRef: required} — doc §3 "rotate → revoke → invalidate →
// incident record". Break-glass executions carry the Task 7.3.12
// grant id so the mandatory post-review can join mark → grant.
package security

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"exchange/internal/admin"

	excerrors "exchange/pkg/errors"
)

// CodeSecretRotationOverdue is the §23 code (503) raised when a secret
// passes its rotation SLA with no completed emergency rotation.
const CodeSecretRotationOverdue = "SECRET_ROTATION_OVERDUE"

// inventoryAlertSeverity is the ops-alert tier for an overdue rotation —
// P2 per spec §19.14 ("rotation-failure alert beyond the 14-day P2").
const inventoryAlertSeverity = "P2"

// validInventoryClass mirrors the migration-089 CHECK constraint — the
// SecretClass taxonomy is the join key into the rotation Registry.
func validInventoryClass(c SecretClass) bool {
	switch c {
	case ClassJWTSigningKey, ClassAPIKeyMaterial, ClassDBCredential,
		ClassRedisPassword, ClassAeronToken, ClassTLSCertificate,
		ClassBankingAPIKey:
		return true
	}
	return false
}

// ---------------------------------------------------------------------------
// Row model
// ---------------------------------------------------------------------------

// InventoryEntry is one secrets_inventory row — metadata only, never
// secret material (docs/ops/secrets-inventory.md rule zero).
type InventoryEntry struct {
	ID                int64         `json:"id"`
	SecretName        string        `json:"secret_name"`
	Class             SecretClass   `json:"secret_class"`
	Category          string        `json:"category"` // ops doc §1 finer label
	Owner             string        `json:"owner"`
	Consumers         []string      `json:"consumers"`
	TTL               time.Duration `json:"ttl_seconds"`
	AlertLead         time.Duration `json:"alert_lead_seconds"`
	RotationProcedure string        `json:"rotation_procedure"`
	LastRotatedAt     *time.Time    `json:"last_rotated_at,omitempty"`
	DRCritical        bool          `json:"dr_critical"`
	BreakGlassPath    string        `json:"break_glass_path"`
	CreatedAt         time.Time     `json:"created_at"`
	UpdatedAt         time.Time     `json:"updated_at"`
}

// InventoryEntryInput is the upsert payload. TTL/AlertLead are
// durations here, seconds at the column. LastRotatedAt nil preserves
// the existing timestamp on update (backfill sets it once).
type InventoryEntryInput struct {
	SecretName        string
	Class             SecretClass
	Category          string
	Owner             string
	Consumers         []string
	TTL               time.Duration
	AlertLead         time.Duration // 0 → 14-day P2 default
	RotationProcedure string
	LastRotatedAt     *time.Time
	DRCritical        bool
	BreakGlassPath    string
}

// assess maps the row to its rotation state at now:
//
//	last_rotated_at NULL                    → StateUnrotated
//	now ≥ last_rotated_at + ttl             → StateOverdue  (the 503 leg)
//	now ≥ deadline − alert_lead             → StateDueSoon  (P2 lead)
//	otherwise                               → StateOK
func (e *InventoryEntry) assess(now time.Time) SecretStatus {
	st := SecretStatus{Name: e.SecretName, Class: e.Class}
	if e.LastRotatedAt == nil {
		st.State = StateUnrotated
		return st
	}
	deadline := e.LastRotatedAt.Add(e.TTL)
	st.SecondsUntilExpiry = deadline.Sub(now).Seconds()
	st.SecondsUntilRenewal = deadline.Add(-e.AlertLead).Sub(now).Seconds()
	switch {
	case !now.Before(deadline):
		st.State = StateOverdue
	case !now.Before(deadline.Add(-e.AlertLead)):
		st.State = StateDueSoon
	default:
		st.State = StateOK
	}
	return st
}

// InventoryRow is the read surface: the entry plus its assessed state.
type InventoryRow struct {
	InventoryEntry
	State              RotationState `json:"state"`
	SecondsUntilExpiry float64       `json:"seconds_until_expiry"`
}

// InventoryView is the admin GET payload — the assessed rows plus the
// overdue set the 503 emission reports.
type InventoryView struct {
	Rows      []InventoryRow  `json:"secrets"`
	Overdue   []string        `json:"overdue"`   // secret_names past rotation SLA
	Unrotated []string        `json:"unrotated"` // inventoried, never rotated
	Coverage  *CoverageReport `json:"coverage,omitempty"`
}

// CoverageReport is the fail-closed "every secret inventoried" verdict
// (Task 9.3.29 item 4): a class with zero rows or a required ref with
// no row is reported, never silently OK.
type CoverageReport struct {
	OK             bool          `json:"ok"`
	MissingClasses []SecretClass `json:"missing_classes,omitempty"`
	MissingSecrets []string      `json:"missing_secrets,omitempty"`
	Unrotated      []string      `json:"unrotated,omitempty"`
	Rows           int           `json:"rows"`
}

// ---------------------------------------------------------------------------
// PgInventoryStore — migration 089 CRUD
// ---------------------------------------------------------------------------

// pgQuerier is satisfied by *pgxpool.Pool and pgx.Tx so the service can
// run mutation+audit in one transaction.
type pgQuerier interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// PgInventoryStore is the secrets_inventory table accessor.
type PgInventoryStore struct {
	pool *pgxpool.Pool
}

// NewPgInventoryStore binds the store to the pool.
func NewPgInventoryStore(pool *pgxpool.Pool) *PgInventoryStore {
	return &PgInventoryStore{pool: pool}
}

const inventoryCols = `id, secret_name, secret_class::text, category, owner,
	consumers, ttl_seconds, alert_lead_seconds, rotation_procedure,
	last_rotated_at, dr_critical, break_glass_path, created_at, updated_at`

func scanInventory(row pgx.Row) (*InventoryEntry, error) {
	var e InventoryEntry
	var ttl, lead int64
	err := row.Scan(&e.ID, &e.SecretName, &e.Class, &e.Category, &e.Owner,
		&e.Consumers, &ttl, &lead, &e.RotationProcedure, &e.LastRotatedAt,
		&e.DRCritical, &e.BreakGlassPath, &e.CreatedAt, &e.UpdatedAt)
	if err != nil {
		return nil, err
	}
	e.TTL = time.Duration(ttl) * time.Second
	e.AlertLead = time.Duration(lead) * time.Second
	return &e, nil
}

func (s *PgInventoryStore) upsert(ctx context.Context, q pgQuerier, in InventoryEntryInput) (*InventoryEntry, error) {
	lead := in.AlertLead
	if lead <= 0 {
		lead = defaultAlertWindow
	}
	var lastRotated any
	if in.LastRotatedAt != nil {
		lastRotated = *in.LastRotatedAt
	}
	row := q.QueryRow(ctx, `
		INSERT INTO secrets_inventory
			(secret_name, secret_class, category, owner, consumers,
			 ttl_seconds, alert_lead_seconds, rotation_procedure,
			 last_rotated_at, dr_critical, break_glass_path)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)
		ON CONFLICT (secret_name) DO UPDATE SET
			secret_class       = EXCLUDED.secret_class,
			category           = EXCLUDED.category,
			owner              = EXCLUDED.owner,
			consumers          = EXCLUDED.consumers,
			ttl_seconds        = EXCLUDED.ttl_seconds,
			alert_lead_seconds = EXCLUDED.alert_lead_seconds,
			rotation_procedure = EXCLUDED.rotation_procedure,
			last_rotated_at    = COALESCE(EXCLUDED.last_rotated_at,
			                       secrets_inventory.last_rotated_at),
			dr_critical        = EXCLUDED.dr_critical,
			break_glass_path   = EXCLUDED.break_glass_path,
			updated_at         = now()
		RETURNING `+inventoryCols,
		in.SecretName, string(in.Class), in.Category, in.Owner, in.Consumers,
		int64(in.TTL.Seconds()), int64(lead.Seconds()), in.RotationProcedure,
		lastRotated, in.DRCritical, in.BreakGlassPath)
	return scanInventory(row)
}

// Upsert inserts or updates one inventory row.
func (s *PgInventoryStore) Upsert(ctx context.Context, in InventoryEntryInput) (*InventoryEntry, error) {
	return s.upsert(ctx, s.pool, in)
}

// Get fetches one row by canonical name.
func (s *PgInventoryStore) Get(ctx context.Context, name string) (*InventoryEntry, bool, error) {
	e, err := scanInventory(s.pool.QueryRow(ctx,
		`SELECT `+inventoryCols+` FROM secrets_inventory WHERE secret_name = $1`, name))
	if err != nil {
		if err == pgx.ErrNoRows {
			return nil, false, nil
		}
		return nil, false, err
	}
	return e, true, nil
}

// List returns every row ordered by canonical name.
func (s *PgInventoryStore) List(ctx context.Context) ([]InventoryEntry, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT `+inventoryCols+` FROM secrets_inventory ORDER BY secret_name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []InventoryEntry
	for rows.Next() {
		var e InventoryEntry
		var ttl, lead int64
		if err := rows.Scan(&e.ID, &e.SecretName, &e.Class, &e.Category,
			&e.Owner, &e.Consumers, &ttl, &lead, &e.RotationProcedure,
			&e.LastRotatedAt, &e.DRCritical, &e.BreakGlassPath,
			&e.CreatedAt, &e.UpdatedAt); err != nil {
			return nil, err
		}
		e.TTL = time.Duration(ttl) * time.Second
		e.AlertLead = time.Duration(lead) * time.Second
		out = append(out, e)
	}
	return out, rows.Err()
}

// LastRotated reports the last completed rotation for name — found=false
// when the secret is not inventoried; at.IsZero()+found=true when it is
// inventoried but never rotated.
func (s *PgInventoryStore) LastRotated(ctx context.Context, name string) (at time.Time, found bool, err error) {
	var ts *time.Time
	err = s.pool.QueryRow(ctx,
		`SELECT last_rotated_at FROM secrets_inventory WHERE secret_name = $1`,
		name).Scan(&ts)
	if err != nil {
		if err == pgx.ErrNoRows {
			return time.Time{}, false, nil
		}
		return time.Time{}, false, err
	}
	if ts != nil {
		at = *ts
	}
	return at, true, nil
}

// markRotated sets last_rotated_at = at inside q (pool or tx).
func (s *PgInventoryStore) markRotated(ctx context.Context, q pgQuerier, name string, at time.Time) (*InventoryEntry, error) {
	return scanInventory(q.QueryRow(ctx, `
		UPDATE secrets_inventory
		   SET last_rotated_at = $2, updated_at = now()
		 WHERE secret_name = $1
		RETURNING `+inventoryCols, name, at))
}

// MarkRotated records a completed rotation (scheduled runbook or
// leak-triggered emergency) against the inventory row.
func (s *PgInventoryStore) MarkRotated(ctx context.Context, name string, at time.Time) (*InventoryEntry, error) {
	return s.markRotated(ctx, s.pool, name, at)
}

// Overdue returns every row whose rotation deadline has passed at now:
// last_rotated_at + ttl_seconds ≤ now. Never-rotated rows are NOT
// overdue (they have no SLA breach — coverage reports them
// separately); callers needing the pessimistic union add Unrotated.
func (s *PgInventoryStore) Overdue(ctx context.Context, now time.Time) ([]InventoryEntry, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT `+inventoryCols+` FROM secrets_inventory
		 WHERE last_rotated_at IS NOT NULL
		   AND last_rotated_at + (ttl_seconds * interval '1 second') <= $1
		 ORDER BY secret_name`, now)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []InventoryEntry
	for rows.Next() {
		var e InventoryEntry
		var ttl, lead int64
		if err := rows.Scan(&e.ID, &e.SecretName, &e.Class, &e.Category,
			&e.Owner, &e.Consumers, &ttl, &lead, &e.RotationProcedure,
			&e.LastRotatedAt, &e.DRCritical, &e.BreakGlassPath,
			&e.CreatedAt, &e.UpdatedAt); err != nil {
			return nil, err
		}
		e.TTL = time.Duration(ttl) * time.Second
		e.AlertLead = time.Duration(lead) * time.Second
		out = append(out, e)
	}
	return out, rows.Err()
}

// ---------------------------------------------------------------------------
// InventoryService — role gate, validation, audit, emission
// ---------------------------------------------------------------------------

// RotationMark describes one completed rotation record.
type RotationMark struct {
	RotatedAt   time.Time // zero → service clock
	Emergency   bool      // leak-triggered emergency rotation (doc §3)
	IncidentRef string    // required when Emergency — the incident record
	BreakGlass  bool      // executed under a Task 7.3.12 break-glass grant
	GrantID     int64     // admin_break_glass_grants.id; required when BreakGlass
}

// InventoryService is the inventory workflow over PgInventoryStore:
// role gating (Super Admin — doc §5 "Security + SRE leads"; the §8.2
// canon carries no dedicated security role), fail-closed validation
// against the rotation Registry ceilings, admin_audit_log linkage on
// every mutation, the coverage check, and the overdue evaluator behind
// the SECRET_ROTATION_OVERDUE emission.
type InventoryService struct {
	store    *PgInventoryStore
	resolver RoleResolver
	reg      *Registry
	alerter  Alerter
	now      func() time.Time

	mu      sync.Mutex
	alerted map[string]bool // names already paged overdue — clears on rotate
}

// NewInventoryService wires the service. resolver nil → admin ops fail
// closed UNAUTHORIZED_ROLE; reg nil → DefaultRegistry (the spec §24
// #111 table); alerter nil → overdue eval computes but does not page.
func NewInventoryService(store *PgInventoryStore, resolver RoleResolver,
	alerter Alerter, reg *Registry) *InventoryService {
	if reg == nil {
		reg = DefaultRegistry()
	}
	return &InventoryService{
		store: store, resolver: resolver, reg: reg, alerter: alerter,
		now: time.Now, alerted: map[string]bool{},
	}
}

// inventoryRole is the doc §5 gate: reads and writes alike are
// restricted to the security-operating lead — Super Admin in the §8.2
// canon. Support/Risk/Finance/Auditor roles never touch the register.
func (s *InventoryService) requireRole(ctx context.Context, adminID int64) error {
	if adminID <= 0 {
		return excerrors.New("UNAUTHORIZED", "admin identity required")
	}
	if s.resolver == nil {
		return excerrors.New("UNAUTHORIZED_ROLE",
			"role resolver not configured (Phase-07 RBAC stub boundary)")
	}
	role, err := s.resolver(ctx, adminID)
	if err != nil {
		return excerrors.Wrap("INTERNAL_ERROR", "role lookup", err)
	}
	if role != "Super Admin" {
		return excerrors.New("UNAUTHORIZED_ROLE",
			fmt.Sprintf("secrets inventory requires a security/SRE lead (got %q)", role))
	}
	return nil
}

// validate fails closed on the fields the schema cannot hold or the
// rotation contract forbids. Per-row TTL may tighten the class policy
// but never exceed its MaxAge ceiling — a weaker TTL is a defect, not
// configuration (§2.7).
func (s *InventoryService) validate(in InventoryEntryInput) error {
	name := strings.TrimSpace(in.SecretName)
	if name == "" || len(name) > 256 {
		return excerrors.New("INVALID_REQUEST",
			"secret_name required (≤256 chars)")
	}
	for _, r := range name {
		if r < 0x21 || r == 0x7f { // no whitespace/control — names are canonical tokens
			return excerrors.New("INVALID_REQUEST",
				"secret_name must not contain whitespace or control characters")
		}
	}
	if !validInventoryClass(in.Class) {
		return excerrors.New("INVALID_REQUEST",
			"unknown secret_class "+string(in.Class))
	}
	if strings.TrimSpace(in.Owner) == "" {
		return excerrors.New("INVALID_REQUEST", "owner required (team + individual)")
	}
	if in.TTL <= 0 {
		return excerrors.New("INVALID_REQUEST", "ttl_seconds must be positive")
	}
	if max := s.reg.Policy(in.Class).MaxAge; in.TTL > max {
		return excerrors.New("INVALID_REQUEST",
			fmt.Sprintf("ttl %s exceeds the %s rotation ceiling for class %s",
				in.TTL, max, in.Class))
	}
	lead := in.AlertLead
	if lead <= 0 {
		lead = defaultAlertWindow
	}
	if lead > in.TTL {
		return excerrors.New("INVALID_REQUEST",
			"alert lead exceeds ttl — shorten the lead or raise the ttl")
	}
	return nil
}

// UpsertEntry registers or updates one inventory row (admin path — the
// row change and its admin_audit_log + hash-chain link commit in one
// transaction per doc §5).
func (s *InventoryService) UpsertEntry(ctx context.Context, adminID int64,
	in InventoryEntryInput, clientIP string) (*InventoryEntry, error) {
	if err := s.requireRole(ctx, adminID); err != nil {
		return nil, err
	}
	if err := s.validate(in); err != nil {
		return nil, err
	}
	in.SecretName = strings.TrimSpace(in.SecretName)

	tx, err := s.store.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("secrets inventory upsert: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	e, err := s.store.upsert(ctx, tx, in)
	if err != nil {
		return nil, fmt.Errorf("secrets inventory upsert %s: %w", in.SecretName, err)
	}
	if _, _, err := admin.Log(ctx, tx, admin.AuditEntry{
		AdminUserID: adminID,
		Action:      "secrets_inventory.upsert",
		TargetType:  "secrets_inventory",
		AfterState: map[string]any{
			"secret_name":      e.SecretName,
			"secret_class":     string(e.Class),
			"category":         e.Category,
			"owner":            e.Owner,
			"ttl_seconds":      int64(e.TTL.Seconds()),
			"dr_critical":      e.DRCritical,
			"break_glass_path": e.BreakGlassPath,
		},
		IPAddress: clientIP,
	}); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("secrets inventory upsert commit: %w", err)
	}
	return e, nil
}

// MarkRotated records a completed rotation — the "mark" step of the
// doc §3 procedure, for both scheduled runbooks (rotate-secrets.sh)
// and leak-triggered emergency rotations. Emergency marks must carry
// the security-incident reference; break-glass executions must carry
// the Task 7.3.12 grant id so the mandatory post-review joins
// mark → grant → review.
func (s *InventoryService) MarkRotated(ctx context.Context, adminID int64,
	name string, m RotationMark, clientIP string) (*InventoryEntry, error) {
	if err := s.requireRole(ctx, adminID); err != nil {
		return nil, err
	}
	name = strings.TrimSpace(name)
	if name == "" {
		return nil, excerrors.New("INVALID_REQUEST", "secret_name required")
	}
	if m.Emergency && strings.TrimSpace(m.IncidentRef) == "" {
		return nil, excerrors.New("INVALID_REQUEST",
			"emergency rotation requires an incident_ref (docs/ops/secrets-inventory.md §3)")
	}
	if m.BreakGlass && m.GrantID <= 0 {
		return nil, excerrors.New("INVALID_REQUEST",
			"break-glass rotation requires the Task 7.3.12 grant id")
	}
	at := m.RotatedAt
	if at.IsZero() {
		at = s.now()
	}

	tx, err := s.store.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("secrets inventory mark: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	e, err := s.store.markRotated(ctx, tx, name, at)
	if err != nil {
		if err == pgx.ErrNoRows {
			return nil, excerrors.New("NOT_FOUND",
				"secret "+name+" not inventoried")
		}
		return nil, fmt.Errorf("secrets inventory mark %s: %w", name, err)
	}
	if _, _, err := admin.Log(ctx, tx, admin.AuditEntry{
		AdminUserID: adminID,
		Action:      "secrets_inventory.mark_rotated",
		TargetType:  "secrets_inventory",
		AfterState: map[string]any{
			"secret_name":     e.SecretName,
			"last_rotated_at": e.LastRotatedAt,
			"emergency":       m.Emergency,
			"incident_ref":    m.IncidentRef,
			"break_glass":     m.BreakGlass,
			"grant_id":        m.GrantID,
		},
		IPAddress: clientIP,
	}); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("secrets inventory mark commit: %w", err)
	}
	// A completed rotation clears the paged-overdue latch — the next
	// EvaluateRotation pass reports the cleared state (doc §3 step 5).
	s.mu.Lock()
	delete(s.alerted, name)
	s.mu.Unlock()
	return e, nil
}

// List returns the assessed inventory for the admin read surface. The
// caller (handler) emits SECRET_ROTATION_OVERDUE/503 while the Overdue
// set is non-empty — the payload stays available in details so the
// responder sees the whole register, not just the breach count.
func (s *InventoryService) List(ctx context.Context, adminID int64) (*InventoryView, error) {
	if err := s.requireRole(ctx, adminID); err != nil {
		return nil, err
	}
	rows, err := s.store.List(ctx)
	if err != nil {
		return nil, fmt.Errorf("secrets inventory list: %w", err)
	}
	return s.view(rows, s.now()), nil
}

func (s *InventoryService) view(rows []InventoryEntry, now time.Time) *InventoryView {
	v := &InventoryView{Rows: make([]InventoryRow, 0, len(rows))}
	for i := range rows {
		e := rows[i]
		st := e.assess(now)
		v.Rows = append(v.Rows, InventoryRow{
			InventoryEntry:     e,
			State:              st.State,
			SecondsUntilExpiry: st.SecondsUntilExpiry,
		})
		switch st.State {
		case StateOverdue:
			v.Overdue = append(v.Overdue, e.SecretName)
		case StateUnrotated:
			v.Unrotated = append(v.Unrotated, e.SecretName)
		}
	}
	return v
}

// CoverageCheck is the "every secret inventoried" leg (Task 9.3.29
// item 4): every rotation-Registry class must carry at least one row
// and every canonical ref passed in (the deployment's known secrets —
// required or not) must have an inventory row. Gaps are reported, never
// silently OK — the caller treats OK=false as a defect.
func (s *InventoryService) CoverageCheck(ctx context.Context, refs []SecretRef) (*CoverageReport, error) {
	rows, err := s.store.List(ctx)
	if err != nil {
		return nil, fmt.Errorf("secrets inventory coverage: %w", err)
	}
	return s.coverageReport(rows, refs), nil
}

// coverageReport is the pure half of CoverageCheck — separated so the
// verdict logic is unit-testable without PG.
func (s *InventoryService) coverageReport(rows []InventoryEntry, refs []SecretRef) *CoverageReport {
	rep := &CoverageReport{Rows: len(rows)}
	byClass := map[SecretClass]int{}
	byName := map[string]bool{}
	for _, e := range rows {
		byClass[e.Class]++
		byName[e.SecretName] = true
		if e.LastRotatedAt == nil {
			rep.Unrotated = append(rep.Unrotated, e.SecretName)
		}
	}
	s.reg.mu.RLock()
	classes := make([]SecretClass, 0, len(s.reg.policies))
	for c := range s.reg.policies {
		classes = append(classes, c)
	}
	s.reg.mu.RUnlock()
	sort.Slice(classes, func(i, j int) bool { return classes[i] < classes[j] })
	for _, c := range classes {
		if byClass[c] == 0 {
			rep.MissingClasses = append(rep.MissingClasses, c)
		}
	}
	for _, ref := range refs {
		if !byName[ref.Name] {
			rep.MissingSecrets = append(rep.MissingSecrets, ref.Name)
		}
	}
	sort.Strings(rep.MissingSecrets)
	sort.Strings(rep.Unrotated)
	rep.OK = len(rep.MissingClasses) == 0 && len(rep.MissingSecrets) == 0
	return rep
}

// EvaluateRotation is the sweeper pass (run on the hourly ops cadence):
// computes each row's state and pages SECRET_ROTATION_OVERDUE (P2,
// ops.alerts.security) once per overdue secret until a MarkRotated
// clears the latch — the evaluator "clears on next pass" per doc §3
// step 5. Returns the full assessment so the caller can also feed
// metrics/health. The 503 API emission is the admin read handler's
// view of the same Overdue set.
func (s *InventoryService) EvaluateRotation(ctx context.Context) ([]SecretStatus, error) {
	rows, err := s.store.List(ctx)
	if err != nil {
		return nil, fmt.Errorf("secrets inventory eval: %w", err)
	}
	now := s.now()
	out := make([]SecretStatus, 0, len(rows))
	var newlyOverdue []InventoryEntry
	s.mu.Lock()
	for i := range rows {
		st := rows[i].assess(now)
		out = append(out, st)
		if st.State == StateOverdue && !s.alerted[rows[i].SecretName] {
			s.alerted[rows[i].SecretName] = true
			newlyOverdue = append(newlyOverdue, rows[i])
		}
	}
	s.mu.Unlock()
	if s.alerter != nil {
		for _, e := range newlyOverdue {
			_ = s.alerter.Raise(ctx, inventoryAlertSeverity, CodeSecretRotationOverdue,
				fmt.Sprintf("secret %s (class %s, owner %s) past rotation SLA — "+
					"run its rotation_procedure, then MarkRotated; "+
					"runbook: docs/runbooks/secret-rotation-overdue.md",
					e.SecretName, e.Class, e.Owner))
		}
	}
	return out, nil
}
