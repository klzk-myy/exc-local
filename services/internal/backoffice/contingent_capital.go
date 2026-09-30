// contingent_capital.go — Phase-24 Task 24.3.17 items 2/3/6: the
// insurance-fund funding waterfall's contingent-capital commitments
// (spec §17.13.1.2, §24 #329).
//
// contingent_capital_commitments (mig 082) records house capital first,
// then committed backstops (sponsor, credit facility, insurer) — each
// with committed amount, activation trigger, draw window and governing
// agreement reference. INSURANCE_POLICY rows carry business-interruption /
// cyber / key-person / E&O cover (expiry, limit, excess, broker).
//
// The depletion sequence defined by Phase-19 (Tasks 19.3.9/19.3.14) must
// terminate in a funded backstop, never an unbacked deficit —
// BackstopFunded asserts the ordered waterfall ends in an EXECUTED
// commitment with undrawn headroom.
package backoffice

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"exchange/internal/admin"
	"exchange/pkg/decimal"
	excerrors "exchange/pkg/errors"
)

// Commitment kinds (contingent_capital_commitments.commitment_kind).
const (
	CommitHouseCapital    = "HOUSE_CAPITAL"
	CommitSponsor         = "SPONSOR"
	CommitCreditFacility  = "CREDIT_FACILITY"
	CommitInsurer         = "INSURER"
	CommitInsurancePolicy = "INSURANCE_POLICY" // cover tracking (item 3)
)

// Commitment statuses.
const (
	CommitCommitted = "COMMITTED" // agreed, not yet executed/funded
	CommitExecuted  = "EXECUTED"  // agreement executed — drawable backstop
	CommitDrawn     = "DRAWN"     // partially/fully drawn (drawn_amount tracks)
	CommitLapsed    = "LAPSED"    // provider withdrew / agreement terminated
	CommitExpired   = "EXPIRED"   // policy expired unrenewed
)

// Insurance policy cover kinds (contingent_capital_commitments.policy_type).
const (
	PolicyBusinessInterruption = "BUSINESS_INTERRUPTION"
	PolicyCyber                = "CYBER"
	PolicyKeyPerson            = "KEY_PERSON"
	PolicyErrorsOmissions      = "ERRORS_OMISSIONS"
)

// Commitment is one contingent_capital_commitments row.
type Commitment struct {
	ID                int64           `json:"id"`
	ProviderName      string          `json:"provider_name"`
	Kind              string          `json:"commitment_kind"`
	PrioritySeq       int             `json:"priority_seq"` // waterfall draw order (ascending)
	CommittedAmount   decimal.Decimal `json:"committed_amount"`
	DrawnAmount       decimal.Decimal `json:"drawn_amount"`
	Currency          string          `json:"currency"`
	ActivationTrigger string          `json:"activation_trigger"`
	DrawWindowDays    int             `json:"draw_window_days"`
	AgreementRef      string          `json:"agreement_ref"`
	PolicyType        string          `json:"policy_type,omitempty"`
	CoverLimit        decimal.Decimal `json:"cover_limit,omitempty"`
	Excess            decimal.Decimal `json:"excess,omitempty"`
	Broker            string          `json:"broker,omitempty"`
	ExpiresAt         *time.Time      `json:"expires_at,omitempty"`
	Status            string          `json:"status"`
	ExecutedAt        *time.Time      `json:"executed_at,omitempty"`
	ExpiryAlertedAt   *time.Time      `json:"expiry_alerted_at,omitempty"`
	CreatedBy         int64           `json:"created_by"`
	CreatedAt         time.Time       `json:"created_at"`
	UpdatedAt         time.Time       `json:"updated_at"`
}

// Headroom is the undrawn funded remainder.
func (c Commitment) Headroom() decimal.Decimal {
	return c.CommittedAmount.Sub(c.DrawnAmount)
}

// Live reports whether the commitment can still serve the waterfall.
func (c Commitment) Live() bool {
	return c.Status == CommitCommitted || c.Status == CommitExecuted || c.Status == CommitDrawn
}

// RecordCommitment registers a waterfall commitment (Finance Ops).
// priority_seq fixes the depletion order; house capital should sit first.
func (s *TreasuryService) RecordCommitment(ctx context.Context, actor admin.AdminActor, c Commitment) (*Commitment, error) {
	if err := s.requireRole(ctx, actor.UserID, financeWriteRoles); err != nil {
		return nil, err
	}
	switch c.Kind {
	case CommitHouseCapital, CommitSponsor, CommitCreditFacility, CommitInsurer:
		if !c.CommittedAmount.IsPositive() {
			return nil, excerrors.New("INVALID_REQUEST", "committed_amount must be > 0")
		}
	case CommitInsurancePolicy:
		switch c.PolicyType {
		case PolicyBusinessInterruption, PolicyCyber, PolicyKeyPerson, PolicyErrorsOmissions:
		default:
			return nil, excerrors.New("INVALID_REQUEST",
				"policy_type must be BUSINESS_INTERRUPTION|CYBER|KEY_PERSON|ERRORS_OMISSIONS")
		}
		if c.ExpiresAt == nil {
			return nil, excerrors.New("INVALID_REQUEST", "insurance policy cover requires expires_at")
		}
	default:
		return nil, excerrors.New("INVALID_REQUEST",
			fmt.Sprintf("commitment_kind %q not in the waterfall domain", c.Kind))
	}
	if len(c.Currency) != 3 || c.AgreementRef == "" || c.ActivationTrigger == "" {
		return nil, excerrors.New("INVALID_REQUEST",
			"commitment requires currency, agreement_ref and activation_trigger")
	}
	c.Status = CommitCommitted
	c.CreatedBy = actor.UserID
	out, err := s.store.InsertCommitment(ctx, c)
	if err != nil {
		return nil, err
	}
	return out, nil
}

// ExecuteCommitment marks an executed (funded/legally effective)
// commitment — the drawable state the admission gate requires.
func (s *TreasuryService) ExecuteCommitment(ctx context.Context, actor admin.AdminActor, id int64) (*Commitment, error) {
	if err := s.requireRole(ctx, actor.UserID, financeWriteRoles); err != nil {
		return nil, err
	}
	var out *Commitment
	err := s.store.InTx(ctx, func(ctx context.Context, tx Tx) error {
		c, err := tx.CommitmentByID(ctx, id)
		if err != nil {
			return err
		}
		if c == nil {
			return excerrors.New("NOT_FOUND", "commitment not found")
		}
		if c.Status != CommitCommitted {
			return excerrors.New("INVALID_LIFECYCLE_TRANSITION",
				fmt.Sprintf("commitment %d is %s — only COMMITTED may be executed", id, c.Status))
		}
		now := s.now()
		c.Status = CommitExecuted
		c.ExecutedAt = &now
		if err := tx.UpdateCommitment(ctx, *c); err != nil {
			return err
		}
		out = c
		return nil
	})
	return out, err
}

// DrawCommitment records a backstop draw — EXECUTED → DRAWN within
// committed headroom; the depletion sequence can never overdraw a
// commitment (§17.13.1.2: terminate in funded backstop, never unbacked).
func (s *TreasuryService) DrawCommitment(ctx context.Context, actor admin.AdminActor, id int64, amount decimal.Decimal) (*Commitment, error) {
	if err := s.requireRole(ctx, actor.UserID, financeWriteRoles); err != nil {
		return nil, err
	}
	if !amount.IsPositive() {
		return nil, excerrors.New("INVALID_REQUEST", "draw amount must be > 0")
	}
	var out *Commitment
	err := s.store.InTx(ctx, func(ctx context.Context, tx Tx) error {
		c, err := tx.CommitmentByID(ctx, id)
		if err != nil {
			return err
		}
		if c == nil {
			return excerrors.New("NOT_FOUND", "commitment not found")
		}
		if c.Status != CommitExecuted && c.Status != CommitDrawn {
			return excerrors.New("INVALID_LIFECYCLE_TRANSITION",
				fmt.Sprintf("commitment %d is %s — only EXECUTED commitments draw", id, c.Status))
		}
		if amount.GreaterThan(c.Headroom()) {
			return excerrors.New("INVALID_REQUEST",
				fmt.Sprintf("draw %s exceeds undrawn headroom %s", amount.String(), c.Headroom().String()))
		}
		c.DrawnAmount = c.DrawnAmount.Add(amount)
		c.Status = CommitDrawn
		if err := tx.UpdateCommitment(ctx, *c); err != nil {
			return err
		}
		out = c
		return nil
	})
	return out, err
}

// LapseCommitment terminates a commitment (provider withdrawal, agreement
// termination). Lapsing the waterfall's last funded backstop surfaces
// through BackstopFunded=false at the next admission check.
func (s *TreasuryService) LapseCommitment(ctx context.Context, actor admin.AdminActor, id int64) (*Commitment, error) {
	if err := s.requireRole(ctx, actor.UserID, complianceOrFinance); err != nil {
		return nil, err
	}
	var out *Commitment
	err := s.store.InTx(ctx, func(ctx context.Context, tx Tx) error {
		c, err := tx.CommitmentByID(ctx, id)
		if err != nil {
			return err
		}
		if c == nil {
			return excerrors.New("NOT_FOUND", "commitment not found")
		}
		if !c.Live() {
			return excerrors.New("INVALID_LIFECYCLE_TRANSITION",
				fmt.Sprintf("commitment %d already %s", id, c.Status))
		}
		c.Status = CommitLapsed
		if err := tx.UpdateCommitment(ctx, *c); err != nil {
			return err
		}
		out = c
		return nil
	})
	return out, err
}

// ListCommitments is the waterfall read surface.
func (s *TreasuryService) ListCommitments(ctx context.Context, actor admin.AdminActor) ([]Commitment, error) {
	if err := s.requireRole(ctx, actor.UserID, readRoles); err != nil {
		return nil, err
	}
	return s.store.ListCommitments(ctx)
}

// BackstopFunded asserts the depletion waterfall terminates in a funded
// backstop (item 2): ordered by priority_seq, the LAST live
// (non-insurance-policy) commitment must be EXECUTED with undrawn
// headroom — otherwise the sequence ends in an unbacked deficit and the
// admission gate fails closed.
func (s *TreasuryService) BackstopFunded(ctx context.Context) (bool, error) {
	cs, err := s.store.ListCommitments(ctx)
	if err != nil {
		return false, err
	}
	var last *Commitment
	for i := range cs {
		c := cs[i]
		if c.Kind == CommitInsurancePolicy || !c.Live() {
			continue // cover tracking rows are not capital backstops
		}
		if last == nil || c.PrioritySeq >= last.PrioritySeq {
			cp := c
			last = &cp
		}
	}
	if last == nil {
		return false, nil // no waterfall defined — unbacked
	}
	drawable := last.Status == CommitExecuted || last.Status == CommitDrawn
	return drawable && last.Headroom().IsPositive(), nil
}

// --- PgStore: contingent_capital_commitments ----------------------------------

const commitmentCols = `id, provider_name, commitment_kind, priority_seq,
	committed_amount::text, drawn_amount::text, currency, activation_trigger,
	draw_window_days, agreement_ref, COALESCE(policy_type,''),
	cover_limit::text, excess::text, COALESCE(broker,''), expires_at, status,
	executed_at, expiry_alerted_at, created_by, created_at, updated_at`

func scanCommitment(row pgx.Row) (*Commitment, error) {
	var c Commitment
	var committed, drawn string
	var limit, excess *string
	if err := row.Scan(&c.ID, &c.ProviderName, &c.Kind, &c.PrioritySeq,
		&committed, &drawn, &c.Currency, &c.ActivationTrigger, &c.DrawWindowDays,
		&c.AgreementRef, &c.PolicyType, &limit, &excess, &c.Broker, &c.ExpiresAt,
		&c.Status, &c.ExecutedAt, &c.ExpiryAlertedAt, &c.CreatedBy,
		&c.CreatedAt, &c.UpdatedAt); err != nil {
		return nil, err
	}
	c.CommittedAmount = decimal.RequireFromString(committed)
	c.DrawnAmount = decimal.RequireFromString(drawn)
	if limit != nil {
		d := decimal.RequireFromString(*limit)
		c.CoverLimit = d
	}
	if excess != nil {
		d := decimal.RequireFromString(*excess)
		c.Excess = d
	}
	return &c, nil
}

func decPtr(d decimal.Decimal) any {
	if d.IsZero() {
		return nil
	}
	return d.String()
}

func (s *PgStore) InsertCommitment(ctx context.Context, c Commitment) (*Commitment, error) {
	return scanCommitment(s.q.QueryRow(ctx, `
		INSERT INTO contingent_capital_commitments
		 (provider_name, commitment_kind, priority_seq, committed_amount,
		  drawn_amount, currency, activation_trigger, draw_window_days,
		  agreement_ref, policy_type, cover_limit, excess, broker, expires_at,
		  status, created_by)
		 VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16)
		 RETURNING `+commitmentCols,
		c.ProviderName, c.Kind, c.PrioritySeq, c.CommittedAmount.String(),
		c.DrawnAmount.String(), c.Currency, c.ActivationTrigger, c.DrawWindowDays,
		c.AgreementRef, nilIfEmpty(c.PolicyType), decPtr(c.CoverLimit),
		decPtr(c.Excess), nilIfEmpty(c.Broker), c.ExpiresAt, c.Status, c.CreatedBy))
}

func (s *PgStore) CommitmentByID(ctx context.Context, id int64) (*Commitment, error) {
	c, err := scanCommitment(s.q.QueryRow(ctx,
		`SELECT `+commitmentCols+` FROM contingent_capital_commitments WHERE id=$1`, id))
	if err == pgx.ErrNoRows {
		return nil, nil
	}
	return c, err
}

func (s *PgStore) UpdateCommitment(ctx context.Context, c Commitment) error {
	_, err := s.q.Exec(ctx, `
		UPDATE contingent_capital_commitments
		   SET status=$2, drawn_amount=$3, executed_at=$4,
		       expiry_alerted_at=$5, expires_at=$6, updated_at=now()
		 WHERE id=$1`,
		c.ID, c.Status, c.DrawnAmount.String(), c.ExecutedAt,
		c.ExpiryAlertedAt, c.ExpiresAt)
	return err
}

func (s *PgStore) ListCommitments(ctx context.Context) ([]Commitment, error) {
	rows, err := s.q.Query(ctx,
		`SELECT `+commitmentCols+` FROM contingent_capital_commitments
		  ORDER BY priority_seq, id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Commitment{}
	for rows.Next() {
		var c Commitment
		var committed, drawn string
		var limit, excess *string
		if err := rows.Scan(&c.ID, &c.ProviderName, &c.Kind, &c.PrioritySeq,
			&committed, &drawn, &c.Currency, &c.ActivationTrigger, &c.DrawWindowDays,
			&c.AgreementRef, &c.PolicyType, &limit, &excess, &c.Broker, &c.ExpiresAt,
			&c.Status, &c.ExecutedAt, &c.ExpiryAlertedAt, &c.CreatedBy,
			&c.CreatedAt, &c.UpdatedAt); err != nil {
			return nil, err
		}
		c.CommittedAmount = decimal.RequireFromString(committed)
		c.DrawnAmount = decimal.RequireFromString(drawn)
		if limit != nil {
			c.CoverLimit = decimal.RequireFromString(*limit)
		}
		if excess != nil {
			c.Excess = decimal.RequireFromString(*excess)
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// ExpiringPolicies returns live INSURANCE_POLICY commitments expiring on
// or before horizon — the CheckInsuranceExpiries input set.
func (s *PgStore) ExpiringPolicies(ctx context.Context, horizon time.Time) ([]Commitment, error) {
	rows, err := s.q.Query(ctx,
		`SELECT `+commitmentCols+` FROM contingent_capital_commitments
		  WHERE commitment_kind='INSURANCE_POLICY' AND expires_at IS NOT NULL
		    AND expires_at <= $1 AND status IN ('COMMITTED','EXECUTED','DRAWN')
		  ORDER BY expires_at`, horizon)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Commitment{}
	for rows.Next() {
		var c Commitment
		var committed, drawn string
		var limit, excess *string
		if err := rows.Scan(&c.ID, &c.ProviderName, &c.Kind, &c.PrioritySeq,
			&committed, &drawn, &c.Currency, &c.ActivationTrigger, &c.DrawWindowDays,
			&c.AgreementRef, &c.PolicyType, &limit, &excess, &c.Broker, &c.ExpiresAt,
			&c.Status, &c.ExecutedAt, &c.ExpiryAlertedAt, &c.CreatedBy,
			&c.CreatedAt, &c.UpdatedAt); err != nil {
			return nil, err
		}
		c.CommittedAmount = decimal.RequireFromString(committed)
		c.DrawnAmount = decimal.RequireFromString(drawn)
		if limit != nil {
			c.CoverLimit = decimal.RequireFromString(*limit)
		}
		if excess != nil {
			c.Excess = decimal.RequireFromString(*excess)
		}
		out = append(out, c)
	}
	return out, rows.Err()
}
