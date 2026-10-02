// Migration-285 persistence for the DORA Art. 28 ICT provider register.
package dora

import (
	"context"
	stderrors "errors"

	"time"

	"github.com/jackc/pgx/v5"
)

const providerCols = `id, name, ict_service, functions_supported,
	locations_subcontractors, concentration, contract_terms,
	termination_notice_days, exit_strategy, substitution_plan,
	last_substitution_test_at, renewal_at, next_review_at,
	owner, status, notes, created_at, updated_at`

func scanProvider(row pgx.Row) (*Provider, bool, error) {
	var p Provider
	err := row.Scan(&p.ID, &p.Name, &p.ICTService, &p.FunctionsSupported,
		&p.LocationsSubcontractors, &p.Concentration, &p.ContractTerms,
		&p.TerminationNoticeDays, &p.ExitStrategy, &p.SubstitutionPlan,
		&p.LastSubstitutionTestAt, &p.RenewalAt, &p.NextReviewAt,
		&p.Owner, &p.Status, &p.Notes, &p.CreatedAt, &p.UpdatedAt)
	if stderrors.Is(err, pgx.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	return &p, true, nil
}

// InsertProvider implements VendorStore.
func (s *PgxStore) InsertProvider(ctx context.Context, p *Provider) (int64, error) {
	var id int64
	err := s.Pool.QueryRow(ctx, `
		INSERT INTO ict_providers (name, ict_service, functions_supported,
		    locations_subcontractors, concentration, contract_terms,
		    termination_notice_days, exit_strategy, substitution_plan,
		    last_substitution_test_at, renewal_at, next_review_at,
		    owner, status, notes)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15)
		RETURNING id`,
		p.Name, p.ICTService, p.FunctionsSupported,
		p.LocationsSubcontractors, p.Concentration, p.ContractTerms,
		p.TerminationNoticeDays, p.ExitStrategy, p.SubstitutionPlan,
		p.LastSubstitutionTestAt, p.RenewalAt, p.NextReviewAt,
		p.Owner, p.Status, p.Notes).Scan(&id)
	return id, err
}

// UpdateProvider implements VendorStore — COALESCE-free: the caller
// writes the full row (PUT semantics), nullable schedule fields pass
// through as-is.
func (s *PgxStore) UpdateProvider(ctx context.Context, p *Provider) (bool, error) {
	ct, err := s.Pool.Exec(ctx, `
		UPDATE ict_providers SET name=$2, ict_service=$3,
		    functions_supported=$4, locations_subcontractors=$5,
		    concentration=$6, contract_terms=$7,
		    termination_notice_days=$8, exit_strategy=$9,
		    substitution_plan=$10, renewal_at=$11, next_review_at=$12,
		    owner=$13, notes=$14, updated_at=now()
		WHERE id=$1`,
		p.ID, p.Name, p.ICTService, p.FunctionsSupported,
		p.LocationsSubcontractors, p.Concentration, p.ContractTerms,
		p.TerminationNoticeDays, p.ExitStrategy, p.SubstitutionPlan,
		p.RenewalAt, p.NextReviewAt, p.Owner, p.Notes)
	if err != nil {
		return false, err
	}
	return ct.RowsAffected() > 0, nil
}

// SetProviderStatus implements VendorStore.
func (s *PgxStore) SetProviderStatus(ctx context.Context, id int64,
	status string, at time.Time) (bool, error) {
	ct, err := s.Pool.Exec(ctx, `
		UPDATE ict_providers SET status=$2, updated_at=$3
		WHERE id=$1 AND status <> $2`, id, status, at)
	if err != nil {
		return false, err
	}
	return ct.RowsAffected() > 0, nil
}

// GetProvider implements VendorStore.
func (s *PgxStore) GetProvider(ctx context.Context, id int64) (*Provider, bool, error) {
	return scanProvider(s.Pool.QueryRow(ctx,
		`SELECT `+providerCols+` FROM ict_providers WHERE id=$1`, id))
}

// ListProviders implements VendorStore (status "" = all rows).
func (s *PgxStore) ListProviders(ctx context.Context, status string) ([]Provider, error) {
	q := `SELECT ` + providerCols + ` FROM ict_providers`
	args := []any{}
	if status != "" {
		q += ` WHERE status=$1`
		args = append(args, status)
	}
	q += ` ORDER BY id`
	rows, err := s.Pool.Query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Provider
	for rows.Next() {
		var p Provider
		if err := rows.Scan(&p.ID, &p.Name, &p.ICTService,
			&p.FunctionsSupported, &p.LocationsSubcontractors,
			&p.Concentration, &p.ContractTerms, &p.TerminationNoticeDays,
			&p.ExitStrategy, &p.SubstitutionPlan, &p.LastSubstitutionTestAt,
			&p.RenewalAt, &p.NextReviewAt, &p.Owner, &p.Status, &p.Notes,
			&p.CreatedAt, &p.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// InsertReview implements VendorStore.
func (s *PgxStore) InsertReview(ctx context.Context, r *Review) (int64, error) {
	var id int64
	err := s.Pool.QueryRow(ctx, `
		INSERT INTO ict_provider_reviews (provider_id, kind, outcome,
		    evidence_ref, notes, actor, reviewed_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7) RETURNING id`,
		r.ProviderID, r.Kind, r.Outcome, r.EvidenceRef, r.Notes,
		r.Actor, r.ReviewedAt).Scan(&id)
	return id, err
}

// ListReviews implements VendorStore.
func (s *PgxStore) ListReviews(ctx context.Context, providerID int64) ([]Review, error) {
	rows, err := s.Pool.Query(ctx, `
		SELECT id, provider_id, kind, outcome, evidence_ref, notes,
		       actor, reviewed_at
		FROM ict_provider_reviews WHERE provider_id=$1
		ORDER BY reviewed_at DESC, id DESC`, providerID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Review
	for rows.Next() {
		var r Review
		if err := rows.Scan(&r.ID, &r.ProviderID, &r.Kind, &r.Outcome,
			&r.EvidenceRef, &r.Notes, &r.Actor, &r.ReviewedAt); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// SetProviderSchedule implements VendorStore — nil pointers leave the
// column unchanged.
func (s *PgxStore) SetProviderSchedule(ctx context.Context, id int64,
	nextReview, lastTest *time.Time) error {
	_, err := s.Pool.Exec(ctx, `
		UPDATE ict_providers SET
		    next_review_at = COALESCE($2, next_review_at),
		    last_substitution_test_at = COALESCE($3, last_substitution_test_at),
		    updated_at = now()
		WHERE id=$1`, id, nextReview, lastTest)
	return err
}
