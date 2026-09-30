// store_pg.go — PostgreSQL implementation of the marketmaking Store
// seam over the migration-045 tables.
package marketmaking

import (
	"context"
	stderrors "errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"exchange/internal/config"
	"exchange/pkg/decimal"
)

// PgStore implements Store over pgx.
type PgStore struct {
	Pool *pgxpool.Pool
}

// NewPgStore wires the store.
func NewPgStore(pool *pgxpool.Pool) *PgStore { return &PgStore{Pool: pool} }

const programCols = `id, account_id, instrument_id, min_quote_size::text,
	max_spread_bps::text, presence_pct::text, mmp_max_fills, mmp_window_ms,
	rebate_bps::text, otr_allowance::text, status, created_at, updated_at`

func scanProgram(row pgx.Row) (*Program, error) {
	var (
		p                      Program
		minSz, sprd, pres, reb string
		otr                    *string
		status                 string
	)
	err := row.Scan(&p.ID, &p.AccountID, &p.InstrumentID, &minSz, &sprd,
		&pres, &p.MMPMaxFills, &p.MMPWindowMs, &reb, &otr, &status,
		&p.CreatedAt, &p.UpdatedAt)
	if err != nil {
		return nil, err
	}
	if p.MinQuoteSize, err = decimal.NewFromString(minSz); err != nil {
		return nil, fmt.Errorf("program %d min_quote_size %q: %w", p.ID, minSz, err)
	}
	if p.MaxSpreadBps, err = decimal.NewFromString(sprd); err != nil {
		return nil, fmt.Errorf("program %d max_spread_bps %q: %w", p.ID, sprd, err)
	}
	if p.PresencePct, err = decimal.NewFromString(pres); err != nil {
		return nil, fmt.Errorf("program %d presence_pct %q: %w", p.ID, pres, err)
	}
	if p.RebateBps, err = decimal.NewFromString(reb); err != nil {
		return nil, fmt.Errorf("program %d rebate_bps %q: %w", p.ID, reb, err)
	}
	if otr != nil {
		v, err := decimal.NewFromString(*otr)
		if err != nil {
			return nil, fmt.Errorf("program %d otr_allowance %q: %w", p.ID, *otr, err)
		}
		p.OtrAllowance = &v
	}
	p.Status = Status(status)
	return &p, nil
}

func (s *PgStore) CreateProgram(ctx context.Context, p *Program) error {
	row := s.Pool.QueryRow(ctx, `
		INSERT INTO mm_programs
		  (account_id, instrument_id, min_quote_size, max_spread_bps,
		   presence_pct, mmp_max_fills, mmp_window_ms, rebate_bps,
		   otr_allowance, status)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,'ACTIVE')
		RETURNING `+programCols,
		p.AccountID, p.InstrumentID, p.MinQuoteSize.String(), p.MaxSpreadBps.String(),
		p.PresencePct.String(), p.MMPMaxFills, p.MMPWindowMs, p.RebateBps.String(),
		decPtrStr(p.OtrAllowance))
	out, err := scanProgram(row)
	if err != nil {
		return err
	}
	*p = *out
	return nil
}

func decPtrStr(d *decimal.Decimal) any {
	if d == nil {
		return nil
	}
	return d.String()
}

func (s *PgStore) UpdateProgram(ctx context.Context, p *Program) error {
	tag, err := s.Pool.Exec(ctx, `
		UPDATE mm_programs
		   SET account_id=$2, instrument_id=$3, min_quote_size=$4,
		       max_spread_bps=$5, presence_pct=$6, mmp_max_fills=$7,
		       mmp_window_ms=$8, rebate_bps=$9, otr_allowance=$10,
		       status=$11, updated_at=now()
		 WHERE id=$1`,
		p.ID, p.AccountID, p.InstrumentID, p.MinQuoteSize.String(),
		p.MaxSpreadBps.String(), p.PresencePct.String(), p.MMPMaxFills,
		p.MMPWindowMs, p.RebateBps.String(), decPtrStr(p.OtrAllowance), p.Status)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return pgx.ErrNoRows
	}
	return nil
}

func (s *PgStore) ProgramByID(ctx context.Context, id int64) (*Program, error) {
	p, err := scanProgram(s.Pool.QueryRow(ctx,
		`SELECT `+programCols+` FROM mm_programs WHERE id = $1`, id))
	if stderrors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	return p, err
}

func (s *PgStore) ProgramsFor(ctx context.Context, accountID, instrumentID int64) ([]Program, error) {
	rows, err := s.Pool.Query(ctx, `
		SELECT `+programCols+` FROM mm_programs
		 WHERE account_id = $1
		   AND (instrument_id IS NULL OR instrument_id = $2)
		 ORDER BY instrument_id NULLS LAST, id`, accountID, instrumentID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Program
	for rows.Next() {
		p, err := scanProgram(rows)
		if err != nil {
			return nil, err
		}
		if p.InstrumentID != nil {
			if sym, serr := s.InstrumentSymbol(ctx, *p.InstrumentID); serr == nil {
				p.Symbol = sym
			}
		}
		out = append(out, *p)
	}
	return out, rows.Err()
}

func (s *PgStore) ListPrograms(ctx context.Context, accountID int64, status Status) ([]Program, error) {
	q := `SELECT ` + programCols + ` FROM mm_programs`
	args := []any{}
	where := ""
	if accountID > 0 {
		args = append(args, accountID)
		where += fmt.Sprintf(" AND account_id = $%d", len(args))
	}
	if status != "" {
		args = append(args, string(status))
		where += fmt.Sprintf(" AND status = $%d", len(args))
	}
	if where != "" {
		q += " WHERE " + where[5:]
	}
	q += " ORDER BY id"
	rows, err := s.Pool.Query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Program
	for rows.Next() {
		p, err := scanProgram(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *p)
	}
	return out, rows.Err()
}

// LPForAccount resolves the liquidity_providers entity bound to the
// account via lp_accounts (migration 271) — the account→LP hop the
// SCOPE_LP kill-switch needs on the FIX Mass-Quote ingress path
// (Phase-11 Task 11.3.12, spec §24 #409). (0, nil) means the account is
// not LP-bound: the LP suspension scope simply does not apply to it
// (mm_programs entitlement remains the quoting admission gate).
func (s *PgStore) LPForAccount(ctx context.Context, accountID int64) (int64, error) {
	var lpID int64
	err := s.Pool.QueryRow(ctx,
		`SELECT lp_id FROM lp_accounts WHERE account_id = $1`,
		accountID).Scan(&lpID)
	if stderrors.Is(err, pgx.ErrNoRows) {
		return 0, nil
	}
	return lpID, err
}

func (s *PgStore) InstrumentBySymbol(ctx context.Context, symbol string) (int64, string, error) {
	var id int64
	var ccy string
	err := s.Pool.QueryRow(ctx,
		`SELECT id, quote_currency FROM instruments WHERE symbol = $1`,
		config.CanonicalSymbol(symbol)).
		Scan(&id, &ccy)
	if stderrors.Is(err, pgx.ErrNoRows) {
		return 0, "", nil
	}
	return id, ccy, err
}

func (s *PgStore) InstrumentSymbol(ctx context.Context, instrumentID int64) (string, error) {
	var sym string
	err := s.Pool.QueryRow(ctx,
		`SELECT symbol FROM instruments WHERE id = $1`, instrumentID).Scan(&sym)
	if stderrors.Is(err, pgx.ErrNoRows) {
		return "", fmt.Errorf("instrument %d not found", instrumentID)
	}
	return sym, err
}

func (s *PgStore) QuoteCurrency(ctx context.Context, instrumentID int64) (string, error) {
	var ccy string
	err := s.Pool.QueryRow(ctx,
		`SELECT quote_currency FROM instruments WHERE id = $1`, instrumentID).Scan(&ccy)
	if stderrors.Is(err, pgx.ErrNoRows) {
		return "", fmt.Errorf("instrument %d not found", instrumentID)
	}
	return ccy, err
}

func (s *PgStore) RecordSample(ctx context.Context, programID int64, day time.Time, compliant bool) error {
	inc := 0
	if compliant {
		inc = 1
	}
	_, err := s.Pool.Exec(ctx, `
		INSERT INTO mm_compliance (program_id, day, samples_total, samples_compliant)
		VALUES ($1, $2, 1, $3)
		ON CONFLICT (program_id, day) DO UPDATE
		  SET samples_total = mm_compliance.samples_total + 1,
		      samples_compliant = mm_compliance.samples_compliant + $3,
		      updated_at = now()`,
		programID, day, inc)
	return err
}

func (s *PgStore) FinalizeCompliance(ctx context.Context, programID int64,
	day time.Time, presence decimal.Decimal, breach bool, reason string) error {
	_, err := s.Pool.Exec(ctx, `
		INSERT INTO mm_compliance (program_id, day, presence_pct, breach, breach_reason)
		VALUES ($1, $2, $3, $4, NULLIF($5,''))
		ON CONFLICT (program_id, day) DO UPDATE
		  SET presence_pct = $3, breach = $4,
		      breach_reason = NULLIF($5,''), updated_at = now()`,
		programID, day, presence.String(), breach, reason)
	return err
}

const complianceCols = `id, program_id, day, samples_total, samples_compliant,
	presence_pct::text, breach, COALESCE(breach_reason,''), created_at, updated_at`

func scanCompliance(row pgx.Row) (*ComplianceRow, error) {
	var (
		c    ComplianceRow
		pres *string
	)
	err := row.Scan(&c.ID, &c.ProgramID, &c.Day, &c.SamplesTotal,
		&c.SamplesCompliant, &pres, &c.Breach, &c.BreachReason,
		&c.CreatedAt, &c.UpdatedAt)
	if err != nil {
		return nil, err
	}
	if pres != nil {
		v, err := decimal.NewFromString(*pres)
		if err != nil {
			return nil, fmt.Errorf("compliance %d presence_pct %q: %w", c.ID, *pres, err)
		}
		c.PresencePct = &v
	}
	return &c, nil
}

func (s *PgStore) ComplianceRows(ctx context.Context, programID int64,
	from, to time.Time) ([]ComplianceRow, error) {
	rows, err := s.Pool.Query(ctx, `
		SELECT `+complianceCols+` FROM mm_compliance
		 WHERE program_id = $1 AND day >= $2 AND day <= $3
		 ORDER BY day`, programID, from, to)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ComplianceRow
	for rows.Next() {
		c, err := scanCompliance(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *c)
	}
	return out, rows.Err()
}

func (s *PgStore) BreachCount(ctx context.Context, programID int64, since time.Time) (int, error) {
	var n int
	err := s.Pool.QueryRow(ctx, `
		SELECT count(*) FROM mm_compliance
		 WHERE program_id = $1 AND breach AND day >= $2::date`,
		programID, since).Scan(&n)
	return n, err
}

func (s *PgStore) AccrueRebate(ctx context.Context, a RebateAccrual) error {
	_, err := s.Pool.Exec(ctx, `
		INSERT INTO mm_rebate_accruals
		  (program_id, account_id, instrument_id, fill_ref, day, currency, amount)
		VALUES ($1,$2,$3,$4,$5,$6,$7)
		ON CONFLICT (program_id, fill_ref) DO NOTHING`,
		a.ProgramID, a.AccountID, a.InstrumentID, a.FillRef, a.Day,
		a.Currency, a.Amount.String())
	return err
}

const rebateCols = `id, program_id, account_id, instrument_id, fill_ref, day,
	currency, amount::text, posted_journal_id, created_at`

func scanRebate(row pgx.Row) (*RebateAccrual, error) {
	var (
		a   RebateAccrual
		amt string
	)
	err := row.Scan(&a.ID, &a.ProgramID, &a.AccountID, &a.InstrumentID,
		&a.FillRef, &a.Day, &a.Currency, &amt, &a.PostedJournalID, &a.CreatedAt)
	if err != nil {
		return nil, err
	}
	if a.Amount, err = decimal.NewFromString(amt); err != nil {
		return nil, fmt.Errorf("accrual %d amount %q: %w", a.ID, amt, err)
	}
	return &a, nil
}

func (s *PgStore) UnpostedRebates(ctx context.Context) ([]RebateAccrual, error) {
	rows, err := s.Pool.Query(ctx, `
		SELECT `+rebateCols+` FROM mm_rebate_accruals
		 WHERE posted_journal_id IS NULL ORDER BY account_id, currency, id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []RebateAccrual
	for rows.Next() {
		a, err := scanRebate(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *a)
	}
	return out, rows.Err()
}

func (s *PgStore) MarkRebatesPosted(ctx context.Context, ids []int64, journalID int64) error {
	_, err := s.Pool.Exec(ctx, `
		UPDATE mm_rebate_accruals SET posted_journal_id = $1
		 WHERE id = ANY($2) AND posted_journal_id IS NULL`,
		journalID, ids)
	return err
}

func (s *PgStore) ListRebates(ctx context.Context, programID int64, limit int) ([]RebateAccrual, error) {
	if limit <= 0 || limit > 500 {
		limit = 200
	}
	rows, err := s.Pool.Query(ctx, `
		SELECT `+rebateCols+` FROM mm_rebate_accruals
		 WHERE program_id = $1 ORDER BY id DESC LIMIT $2`,
		programID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []RebateAccrual
	for rows.Next() {
		a, err := scanRebate(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *a)
	}
	return out, rows.Err()
}
