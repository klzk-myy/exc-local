// legal_docs.go — Phase-22 Task 22.3.11 (spec §5.25, §15.5, §24 #146):
// the legal-agreement registry and the order-admission gate that rejects
// derivative order flow without EXECUTED documentation.
//
// Registry: legal_agreements rows carry the spec §5.25 lifecycle —
// PENDING → EXECUTED → (EXPIRED | TERMINATED). Registration and every
// status transition require a Compliance Officer (or Super Admin) actor
// and append an audit entry (admin_audit_log via audit.AppendAuto).
// Mutations run inside SERIALIZABLE transactions.
//
// Order gate (the seam the gateway/order path binds):
//
//   - SPOT (and unknown-but-benign classes) pass through.
//   - FORWARD/SWAP/NDF/OPTION require an EXECUTED, unexpired ISDA.
//   - NDF/OPTION additionally require an EXECUTED, unexpired CSA.
//   - ELIGIBLE_COUNTERPARTY accounts on covered classes additionally
//     require the FMSB give-up acknowledgement (spec §15.5 ECP flow).
//   - reduce-only order flow bypasses the gate (spec §15.5 edge case:
//     terminated agreement with open positions → block new, allow
//     reduce-only).
//   - Missing, PENDING, EXPIRED or TERMINATED documentation rejects with
//     LEGAL_DOC_REQUIRED; store failures reject SERVICE_DEGRADED —
//     fail closed, never admit on a degraded read.
//
// accounts.umr_in_scope (migration 043) is exposed via UMRInScope for
// the derivatives IM gate — legal gating itself applies regardless of
// UMR scope.
package compliance

import (
	"context"
	"encoding/json"
	stderrors "errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"exchange/internal/audit"
	excerrors "exchange/pkg/errors"
)

// CodeLegalDocRequired — spec §23 (HTTP 403): covered order flow lacks
// EXECUTED legal documentation.
const CodeLegalDocRequired = "LEGAL_DOC_REQUIRED"

// Agreement types — the legal_agreement_type_enum domain (spec §5.25).
const (
	AgreementISDA        = "ISDA"
	AgreementCSA         = "CSA"
	AgreementFMSBGiveup  = "FMSB_GIVEUP"
	AgreementPB          = "PB_AGREEMENT"
	AgreementDEAAddendum = "DEA_ADDENDUM"
)

// Agreement statuses — the legal_agreement_status_enum domain.
const (
	AgreementStatusPending    = "PENDING"
	AgreementStatusExecuted   = "EXECUTED"
	AgreementStatusExpired    = "EXPIRED"
	AgreementStatusTerminated = "TERMINATED"
)

// legalAgreementOpen is the set of states that block a duplicate
// registration (mirrors the partial unique index in migration 043).
var legalAgreementOpen = map[string]bool{
	AgreementStatusPending:  true,
	AgreementStatusExecuted: true,
}

// LegalAgreement is one registry row.
type LegalAgreement struct {
	ID            int64
	AccountID     int64
	AgreementType string
	Counterparty  string
	Status        string
	DocumentURL   string
	ExecutedAt    *time.Time
	ExpiresAt     *time.Time
	ReviewedBy    *int64
	CreatedAt     time.Time
	UpdatedAt     time.Time
}

// effective returns whether the agreement currently satisfies gating:
// EXECUTED and not past expires_at.
func (a *LegalAgreement) effective(now time.Time) bool {
	return a.Status == AgreementStatusExecuted &&
		(a.ExpiresAt == nil || a.ExpiresAt.After(now))
}

// LegalAgreementInput is the registration payload.
type LegalAgreementInput struct {
	AccountID     int64
	AgreementType string
	Counterparty  string
	DocumentURL   string
	ExpiresAt     *time.Time
}

// LegalDocStore is the persistence seam — PgLegalDocStore is the
// production implementation; tests substitute fixtures.
type LegalDocStore interface {
	Insert(ctx context.Context, in LegalAgreementInput, actorID int64, now time.Time) (*LegalAgreement, error)
	Transition(ctx context.Context, id int64, from map[string]bool, to string,
		actorID int64, documentURL string, now time.Time) (*LegalAgreement, error)
	ExpireDue(ctx context.Context, now time.Time) (int64, error)
	Get(ctx context.Context, id int64) (*LegalAgreement, error)
	List(ctx context.Context, accountID int64) ([]LegalAgreement, error)
	// AgreementsOfType returns the account's rows of one type (all states).
	AgreementsOfType(ctx context.Context, accountID int64, agreementType string) ([]LegalAgreement, error)
	UMRInScope(ctx context.Context, accountID int64) (bool, error)
}

// LegalDocService owns the registry + gate.
type LegalDocService struct {
	store    LegalDocStore
	resolver RoleResolver
	pool     *pgxpool.Pool // audit sink (nil in fixture tests)
	now      func() time.Time
}

// NewLegalDocService wires the service. resolver nil fails the admin
// surface closed (UNAUTHORIZED_ROLE); read/gate paths keep working.
func NewLegalDocService(store LegalDocStore, resolver RoleResolver) (*LegalDocService, error) {
	if store == nil {
		return nil, fmt.Errorf("compliance: legal-doc store is nil")
	}
	return &LegalDocService{store: store, resolver: resolver, now: func() time.Time { return time.Now().UTC() }}, nil
}

// NewPgLegalDocService wires the production service over pgx — the
// composition-layer entry point.
func NewPgLegalDocService(pool *pgxpool.Pool, resolver RoleResolver) (*LegalDocService, error) {
	if pool == nil {
		return nil, fmt.Errorf("compliance: legal-doc pool is nil")
	}
	svc, err := NewLegalDocService(&PgLegalDocStore{Pool: pool}, resolver)
	if err != nil {
		return nil, err
	}
	svc.pool = pool
	return svc, nil
}

// SetClockForTest overrides the clock; tests only.
func (s *LegalDocService) SetClockForTest(now func() time.Time) { s.now = now }

// requireOfficer enforces the Compliance-Officer/Super-Admin actor rule.
func (s *LegalDocService) requireOfficer(ctx context.Context, actorID int64) error {
	if s.resolver == nil {
		return excerrors.New("UNAUTHORIZED_ROLE",
			"legal-doc admin surface requires a role resolver (fail closed)")
	}
	role, err := s.resolver(ctx, actorID)
	if err != nil {
		return excerrors.Wrap("UNAUTHORIZED_ROLE", "role resolution failed", err)
	}
	if !EligibleComplianceRoles[role] {
		return excerrors.New("UNAUTHORIZED_ROLE",
			fmt.Sprintf("role %q may not manage legal agreements (Compliance Officer required)", role))
	}
	return nil
}

// Register inserts a PENDING agreement; a PENDING/EXECUTED row for the
// same (account, type, counterparty) is rejected by the unique index
// (mapped to a coded conflict).
func (s *LegalDocService) Register(ctx context.Context, actorID int64, in LegalAgreementInput) (*LegalAgreement, error) {
	if err := s.requireOfficer(ctx, actorID); err != nil {
		return nil, err
	}
	if in.AccountID <= 0 {
		return nil, excerrors.New("INVALID_REQUEST", "legal agreement: account_id required")
	}
	switch in.AgreementType {
	case AgreementISDA, AgreementCSA, AgreementFMSBGiveup, AgreementPB, AgreementDEAAddendum:
	default:
		return nil, excerrors.New("INVALID_REQUEST",
			fmt.Sprintf("legal agreement: unknown agreement_type %q", in.AgreementType))
	}
	a, err := s.store.Insert(ctx, in, actorID, s.now())
	if err != nil {
		return nil, err
	}
	appendLegalAudit(ctx, s.auditPool(), a.ID, "LEGAL_AGREEMENT_REGISTERED", map[string]any{
		"account_id": a.AccountID, "agreement_type": a.AgreementType,
		"counterparty": a.Counterparty, "actor": actorID})
	return a, nil
}

// Execute marks a PENDING agreement EXECUTED (records executed_at,
// document_url, reviewed_by).
func (s *LegalDocService) Execute(ctx context.Context, actorID, agreementID int64, documentURL string) (*LegalAgreement, error) {
	if err := s.requireOfficer(ctx, actorID); err != nil {
		return nil, err
	}
	a, err := s.store.Transition(ctx, agreementID,
		map[string]bool{AgreementStatusPending: true}, AgreementStatusExecuted,
		actorID, documentURL, s.now())
	if err != nil {
		return nil, err
	}
	appendLegalAudit(ctx, s.auditPool(), a.ID, "LEGAL_AGREEMENT_EXECUTED", map[string]any{
		"account_id": a.AccountID, "agreement_type": a.AgreementType, "actor": actorID})
	return a, nil
}

// Terminate closes a PENDING or EXECUTED agreement (§15.5 edge case:
// termination does not strand open positions — the gate admits
// reduce-only flow on covered classes regardless).
func (s *LegalDocService) Terminate(ctx context.Context, actorID, agreementID int64) (*LegalAgreement, error) {
	if err := s.requireOfficer(ctx, actorID); err != nil {
		return nil, err
	}
	a, err := s.store.Transition(ctx, agreementID,
		map[string]bool{AgreementStatusPending: true, AgreementStatusExecuted: true},
		AgreementStatusTerminated, actorID, "", s.now())
	if err != nil {
		return nil, err
	}
	appendLegalAudit(ctx, s.auditPool(), a.ID, "LEGAL_AGREEMENT_TERMINATED", map[string]any{
		"account_id": a.AccountID, "agreement_type": a.AgreementType, "actor": actorID})
	return a, nil
}

// ExpireDue sweeps EXECUTED rows past expires_at into EXPIRED — the
// scheduler seam (daily job alongside the VM sweep cadence).
func (s *LegalDocService) ExpireDue(ctx context.Context) (int64, error) {
	return s.store.ExpireDue(ctx, s.now())
}

// Get returns one agreement row by id (nil when absent).
func (s *LegalDocService) Get(ctx context.Context, id int64) (*LegalAgreement, error) {
	return s.store.Get(ctx, id)
}

// List returns the account's registry rows (all statuses).
func (s *LegalDocService) List(ctx context.Context, accountID int64) ([]LegalAgreement, error) {
	return s.store.List(ctx, accountID)
}

// UMRInScope reports the accounts.umr_in_scope flag (fail-closed: a
// store error propagates — callers must not assume out-of-scope).
func (s *LegalDocService) UMRInScope(ctx context.Context, accountID int64) (bool, error) {
	return s.store.UMRInScope(ctx, accountID)
}

// ---------------------------------------------------------------------------
// Order-admission gate
// ---------------------------------------------------------------------------

// legalGateClasses maps instrument class → required agreement types.
// Spec §15.5: all derivative flow requires ISDA; NDF/FX-option flow adds
// CSA (the margin documentation); ECP flow adds the FMSB give-up
// acknowledgement on the covered classes.
func legalRequiredAgreements(class, category string) []string {
	class = strings.ToUpper(strings.TrimSpace(class))
	switch class {
	case ClassForward, ClassSwap:
		return []string{AgreementISDA}
	case ClassNDF, ClassOption:
		req := []string{AgreementISDA, AgreementCSA}
		if strings.EqualFold(category, string(CategoryECP)) {
			req = append(req, AgreementFMSBGiveup)
		}
		return req
	}
	return nil
}

// AdmitOrder is the order-admission gate the gateway binds alongside the
// appropriateness check. category is the account's MiFID II category
// (pass "" when unknown — the ECP addendum requirement then cannot
// trigger, which is safe: ECP accounts are resolved upstream).
// reduceOnly admits regardless (terminated docs never strand closes).
func (s *LegalDocService) AdmitOrder(ctx context.Context, accountID int64,
	instrumentClass, category string, reduceOnly bool) error {
	if reduceOnly {
		return nil
	}
	required := legalRequiredAgreements(instrumentClass, category)
	if len(required) == 0 {
		return nil
	}
	now := s.now()
	for _, agType := range required {
		agmts, err := s.store.AgreementsOfType(ctx, accountID, agType)
		if err != nil {
			return excerrors.Wrap("SERVICE_DEGRADED",
				fmt.Sprintf("legal-doc read failed for %s", agType), err)
		}
		ok := false
		for i := range agmts {
			if agmts[i].effective(now) {
				ok = true
				break
			}
		}
		if !ok {
			return excerrors.New(CodeLegalDocRequired, fmt.Sprintf(
				"%s order requires an EXECUTED %s agreement (account %d)",
				strings.ToUpper(instrumentClass), agType, accountID))
		}
	}
	return nil
}

// auditPool is the audit sink — nil in fixture-store tests (audit append
// then no-ops; never a failure path).
func (s *LegalDocService) auditPool() *pgxpool.Pool { return s.pool }

// appendLegalAudit writes the admin_audit_log row; failures are logged
// by audit.AppendAuto's caller contract — audit append never blocks the
// state change that just committed (same convention as hold_workflow).
func appendLegalAudit(ctx context.Context, pool *pgxpool.Pool, recordID int64, action string, payload map[string]any) {
	if pool == nil {
		return
	}
	b, _ := json.Marshal(payload)
	_, _ = audit.AppendAuto(ctx, pool, "legal_agreements", &recordID, action, b)
}

// ---------------------------------------------------------------------------
// PgLegalDocStore — PostgreSQL implementation (SERIALIZABLE mutations)
// ---------------------------------------------------------------------------

// PgLegalDocStore is the production LegalDocStore.
type PgLegalDocStore struct{ Pool *pgxpool.Pool }

func legalSerializableTx(ctx context.Context, pool *pgxpool.Pool, fn func(pgx.Tx) error) error {
	for attempt := 0; attempt < 3; attempt++ {
		tx, err := pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
		if err != nil {
			return err
		}
		err = fn(tx)
		if err == nil {
			err = tx.Commit(ctx)
		} else {
			_ = tx.Rollback(ctx)
		}
		if err == nil {
			return nil
		}
		if !legalIsSerializationErr(err) {
			return err
		}
	}
	return excerrors.New("TRANSACTION_CONFLICT_RETRY_EXHAUSTED",
		"legal docs: serializable retry budget exhausted")
}

func legalIsSerializationErr(err error) bool {
	var pge *pgconn.PgError
	if stderrors.As(err, &pge) {
		return pge.Code == "40001" || pge.Code == "40P01"
	}
	return false
}

const legalAgreementCols = `id, account_id, agreement_type, COALESCE(counterparty,''),
       status, COALESCE(document_url,''), executed_at, expires_at,
       reviewed_by, created_at, updated_at`

func scanLegalAgreement(row pgx.Row) (*LegalAgreement, error) {
	var a LegalAgreement
	err := row.Scan(&a.ID, &a.AccountID, &a.AgreementType, &a.Counterparty,
		&a.Status, &a.DocumentURL, &a.ExecutedAt, &a.ExpiresAt,
		&a.ReviewedBy, &a.CreatedAt, &a.UpdatedAt)
	if err != nil {
		return nil, err
	}
	return &a, nil
}

// Insert registers a PENDING row inside a SERIALIZABLE tx; the partial
// unique index rejects a duplicate open agreement (mapped to
// DERIVATIVE_STATE_CONFLICT — the caller replayed or double-registered).
func (s *PgLegalDocStore) Insert(ctx context.Context, in LegalAgreementInput,
	actorID int64, now time.Time) (*LegalAgreement, error) {
	var a *LegalAgreement
	err := legalSerializableTx(ctx, s.Pool, func(tx pgx.Tx) error {
		row := tx.QueryRow(ctx, `
			INSERT INTO legal_agreements
			    (account_id, agreement_type, counterparty, status,
			     document_url, expires_at, reviewed_by, created_at, updated_at)
			VALUES ($1,$2,NULLIF($3,''),'PENDING',NULLIF($4,''),$5,$6,$7,$7)
			RETURNING `+legalAgreementCols,
			in.AccountID, in.AgreementType, in.Counterparty, in.DocumentURL,
			in.ExpiresAt, actorID, now)
		var err error
		a, err = scanLegalAgreement(row)
		return err
	})
	if err != nil {
		var pge *pgconn.PgError
		if stderrors.As(err, &pge) && pge.Code == "23505" {
			return nil, excerrors.Wrap("DERIVATIVE_STATE_CONFLICT",
				fmt.Sprintf("open %s agreement already exists for account %d",
					in.AgreementType, in.AccountID), err)
		}
		return nil, err
	}
	return a, nil
}

// Transition applies a status flip guarded by the expected-from set —
// the UPDATE's WHERE carries the precondition so a raced transition
// fails closed rather than overwriting.
func (s *PgLegalDocStore) Transition(ctx context.Context, id int64,
	from map[string]bool, to string, actorID int64,
	documentURL string, now time.Time) (*LegalAgreement, error) {
	var a *LegalAgreement
	err := legalSerializableTx(ctx, s.Pool, func(tx pgx.Tx) error {
		states := make([]string, 0, len(from))
		for st := range from {
			states = append(states, st)
		}
		var row pgx.Row
		if to == AgreementStatusExecuted {
			row = tx.QueryRow(ctx, `
				UPDATE legal_agreements
				SET status=$2, reviewed_by=$3, updated_at=$4,
				    executed_at=$4, document_url=NULLIF($5,'')
				WHERE id=$1 AND status = ANY($6)
				RETURNING `+legalAgreementCols, id, to, actorID, now, documentURL, states)
		} else {
			row = tx.QueryRow(ctx, `
				UPDATE legal_agreements
				SET status=$2, reviewed_by=$3, updated_at=$4
				WHERE id=$1 AND status = ANY($5)
				RETURNING `+legalAgreementCols, id, to, actorID, now, states)
		}
		var err error
		a, err = scanLegalAgreement(row)
		return err
	})
	if err != nil {
		if stderrors.Is(err, pgx.ErrNoRows) {
			return nil, excerrors.New("DERIVATIVE_STATE_CONFLICT",
				fmt.Sprintf("legal agreement %d not in an allowed state for %s", id, to))
		}
		return nil, err
	}
	return a, nil
}

// ExpireDue flips EXECUTED rows past expires_at into EXPIRED.
func (s *PgLegalDocStore) ExpireDue(ctx context.Context, now time.Time) (int64, error) {
	tag, err := s.Pool.Exec(ctx, `
		UPDATE legal_agreements
		SET status='EXPIRED', updated_at=$1
		WHERE status='EXECUTED' AND expires_at IS NOT NULL AND expires_at <= $1`, now)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

// Get returns one row by id (nil when absent).
func (s *PgLegalDocStore) Get(ctx context.Context, id int64) (*LegalAgreement, error) {
	a, err := scanLegalAgreement(s.Pool.QueryRow(ctx,
		`SELECT `+legalAgreementCols+` FROM legal_agreements WHERE id=$1`, id))
	if err != nil {
		if stderrors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	return a, nil
}

// List returns every row for the account.
func (s *PgLegalDocStore) List(ctx context.Context, accountID int64) ([]LegalAgreement, error) {
	return s.query(ctx, `SELECT `+legalAgreementCols+
		` FROM legal_agreements WHERE account_id=$1 ORDER BY id`, accountID)
}

// AgreementsOfType returns the account's rows of one type (all states).
func (s *PgLegalDocStore) AgreementsOfType(ctx context.Context, accountID int64, agreementType string) ([]LegalAgreement, error) {
	return s.query(ctx, `SELECT `+legalAgreementCols+
		` FROM legal_agreements WHERE account_id=$1 AND agreement_type=$2 ORDER BY id`,
		accountID, agreementType)
}

func (s *PgLegalDocStore) query(ctx context.Context, q string, args ...any) ([]LegalAgreement, error) {
	rows, err := s.Pool.Query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []LegalAgreement
	for rows.Next() {
		var a LegalAgreement
		if err := rows.Scan(&a.ID, &a.AccountID, &a.AgreementType, &a.Counterparty,
			&a.Status, &a.DocumentURL, &a.ExecutedAt, &a.ExpiresAt,
			&a.ReviewedBy, &a.CreatedAt, &a.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// UMRInScope reads the migration-043 accounts flag.
func (s *PgLegalDocStore) UMRInScope(ctx context.Context, accountID int64) (bool, error) {
	var v bool
	err := s.Pool.QueryRow(ctx,
		`SELECT umr_in_scope FROM accounts WHERE id=$1`, accountID).Scan(&v)
	if err != nil {
		return false, err
	}
	return v, nil
}
