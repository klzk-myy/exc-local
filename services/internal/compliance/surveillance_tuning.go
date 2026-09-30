// Phase-21 Task 21.3.27 (part 2) — per-signal surveillance tuning,
// backtest harness and false-positive calibration.
//
// surveillance_signal_tuning holds versioned detector parameters per
// Phase-17 signal class. Activation swaps one ACTIVE row per
// signal_type atomically — the detection pipeline re-reads
// ActiveTuning() so tuning deploys without downtime. fp_target_pct
// is the per-class calibration target the monthly summary and
// backtest report measure against (spec §14.9.2).
package compliance

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"exchange/internal/admin"

	excerrors "exchange/pkg/errors"
)

// SignalTuning is one surveillance_signal_tuning row.
type SignalTuning struct {
	ID            int64           `json:"id"`
	SignalType    string          `json:"signal_type"`
	Version       int             `json:"version"`
	Params        json.RawMessage `json:"params"`
	FPTargetPct   float64         `json:"fp_target_pct"`
	Calibration   json.RawMessage `json:"calibration"`
	Status        string          `json:"status"` // DRAFT|ACTIVE|SUPERSEDED
	EffectiveFrom *time.Time      `json:"effective_from,omitempty"`
	CreatedBy     int64           `json:"created_by"`
	CreatedAt     time.Time       `json:"created_at"`
	UpdatedAt     time.Time       `json:"updated_at"`
}

// FP-target defaults per signal class (percent) — spec §14.9.2
// calibration goals; tighter classes tolerate higher FP.
var DefaultFPTargets = map[string]float64{
	"SPOOFING":          15.0,
	"LAYERING":          15.0,
	"WASH_TRADING":      5.0,
	"MARKING_THE_CLOSE": 10.0,
	"MOMENTUM_IGNITION": 10.0,
	"FRONT_RUNNING":     5.0,
	"INSIDER_DEALING":   5.0,
}

// TuningService owns the tuning register.
type TuningService struct {
	pool     *pgxpool.Pool
	resolver HoldRoleResolver
	now      func() time.Time
}

// NewTuningService binds the register.
func NewTuningService(pool *pgxpool.Pool,
	resolver HoldRoleResolver) *TuningService {
	return &TuningService{pool: pool, resolver: resolver, now: time.Now}
}

// WithClock overrides the clock (tests).
func (s *TuningService) WithClock(f func() time.Time) *TuningService {
	if f != nil {
		s.now = f
	}
	return s
}

func (s *TuningService) checkRole(ctx context.Context, userID int64) error {
	if s.resolver == nil {
		return excerrors.New("INTERNAL_ERROR", "role resolver not wired")
	}
	role, err := s.resolver(ctx, userID)
	if err != nil {
		return excerrors.New("INTERNAL_ERROR", "role lookup: "+err.Error())
	}
	if role != "Compliance Officer" && role != "Super Admin" {
		return excerrors.New("UNAUTHORIZED_ROLE",
			"role "+role+" cannot tune surveillance")
	}
	return nil
}

// Propose creates a DRAFT tuning version (version = max+1 for the
// signal class). params shape mirrors the detector's Config keys —
// unknown keys are preserved verbatim for forward compatibility.
func (s *TuningService) Propose(ctx context.Context, actorID int64,
	signalType string, params json.RawMessage,
	fpTargetPct float64) (*SignalTuning, error) {
	if err := s.checkRole(ctx, actorID); err != nil {
		return nil, err
	}
	if signalType == "" || len(params) == 0 {
		return nil, excerrors.New("INVALID_REQUEST",
			"signal_type and params are required")
	}
	if fpTargetPct <= 0 {
		fpTargetPct = DefaultFPTargets[signalType]
	}
	if fpTargetPct <= 0 {
		fpTargetPct = 10.0
	}
	var t SignalTuning
	err := s.pool.QueryRow(ctx, `
		INSERT INTO surveillance_signal_tuning
		    (signal_type, version, params, fp_target_pct, created_by)
		VALUES ($1::varchar,
		        COALESCE((SELECT MAX(version) FROM surveillance_signal_tuning
		                  WHERE signal_type = $1::varchar), 0) + 1,
		        $2, $3, $4)
		RETURNING id, signal_type, version, params, fp_target_pct,
		          calibration, status, effective_from, created_by,
		          created_at, updated_at`,
		signalType, params, fpTargetPct, actorID).
		Scan(tuningScan(&t)...)
	if err != nil {
		return nil, excerrors.New("INTERNAL_ERROR",
			"tuning propose: "+err.Error())
	}
	_, _, _ = admin.LogAuto(ctx, s.pool, admin.AuditEntry{
		AdminUserID: actorID, Action: "surveillance.tuning.propose",
		TargetType: "signal_tuning", TargetID: &t.ID,
		AfterState: t,
	})
	return &t, nil
}

// Activate promotes a DRAFT (or SUPERSEDED for rollback) version to
// ACTIVE inside one tx — the prior ACTIVE row flips SUPERSEDED. The
// partial unique index keeps exactly one ACTIVE per signal_type.
func (s *TuningService) Activate(ctx context.Context, actorID int64,
	signalType string, version int) (*SignalTuning, error) {
	if err := s.checkRole(ctx, actorID); err != nil {
		return nil, err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, excerrors.New("INTERNAL_ERROR", "activate tx: "+err.Error())
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var id int64
	var status string
	if err := tx.QueryRow(ctx, `
		SELECT id, status FROM surveillance_signal_tuning
		 WHERE signal_type = $1 AND version = $2 FOR UPDATE`,
		signalType, version).Scan(&id, &status); err == pgx.ErrNoRows {
		return nil, excerrors.New("NOT_FOUND", "tuning version not found")
	} else if err != nil {
		return nil, excerrors.New("INTERNAL_ERROR", "tuning lock: "+err.Error())
	}
	if status == "ACTIVE" {
		return nil, excerrors.New("INVALID_REQUEST",
			"version is already active")
	}
	if _, err := tx.Exec(ctx, `
		UPDATE surveillance_signal_tuning
		   SET status = 'SUPERSEDED', updated_at = now()
		 WHERE signal_type = $1 AND status = 'ACTIVE'`,
		signalType); err != nil {
		return nil, excerrors.New("INTERNAL_ERROR", "supersede: "+err.Error())
	}
	var t SignalTuning
	err = tx.QueryRow(ctx, `
		UPDATE surveillance_signal_tuning
		   SET status = 'ACTIVE', effective_from = now(), updated_at = now()
		 WHERE id = $1
		RETURNING id, signal_type, version, params, fp_target_pct,
		          calibration, status, effective_from, created_by,
		          created_at, updated_at`, id).
		Scan(tuningScan(&t)...)
	if err != nil {
		return nil, excerrors.New("INTERNAL_ERROR", "activate: "+err.Error())
	}
	if _, _, err := admin.Log(ctx, tx, admin.AuditEntry{
		AdminUserID: actorID, Action: "surveillance.tuning.activate",
		TargetType: "signal_tuning", TargetID: &id,
		AfterState: map[string]any{
			"signal_type": signalType, "version": version,
		},
	}); err != nil {
		return nil, excerrors.New("INTERNAL_ERROR", "activate audit: "+err.Error())
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, excerrors.New("INTERNAL_ERROR", "activate commit: "+err.Error())
	}
	return &t, nil
}

// ActiveTuning returns the ACTIVE params for a signal class — the
// detection pipeline's read seam (false when untuned → built-in
// defaults apply).
func (s *TuningService) ActiveTuning(ctx context.Context,
	signalType string) (*SignalTuning, bool, error) {
	var t SignalTuning
	err := s.pool.QueryRow(ctx, `
		SELECT id, signal_type, version, params, fp_target_pct,
		       calibration, status, effective_from, created_by,
		       created_at, updated_at
		  FROM surveillance_signal_tuning
		 WHERE signal_type = $1 AND status = 'ACTIVE'`, signalType).
		Scan(tuningScan(&t)...)
	if err == pgx.ErrNoRows {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, excerrors.New("INTERNAL_ERROR",
			"tuning read: "+err.Error())
	}
	return &t, true, nil
}

// List reads the tuning register, newest versions first.
func (s *TuningService) List(ctx context.Context,
	signalType string, limit int) ([]SignalTuning, error) {
	if limit <= 0 || limit > 200 {
		limit = 100
	}
	q := `SELECT id, signal_type, version, params, fp_target_pct,
	             calibration, status, effective_from, created_by,
	             created_at, updated_at
	        FROM surveillance_signal_tuning`
	args := []any{}
	if signalType != "" {
		q += " WHERE signal_type = $1"
		args = append(args, signalType)
	}
	q += fmt.Sprintf(" ORDER BY signal_type, version DESC LIMIT %d", limit)
	rows, err := s.pool.Query(ctx, q, args...)
	if err != nil {
		return nil, excerrors.New("INTERNAL_ERROR", "tuning list: "+err.Error())
	}
	defer rows.Close()
	var out []SignalTuning
	for rows.Next() {
		var t SignalTuning
		if err := rows.Scan(tuningScan(&t)...); err != nil {
			return nil, excerrors.New("INTERNAL_ERROR", "tuning row: "+err.Error())
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// BacktestResult is the calibration report for one signal class over
// a historical window — measured on dispositioned cases, not a replay:
// the "harness" measures realized accuracy (what the current tuning
// produced) so calibration proposals can be compared against the FP
// target before activation.
type BacktestResult struct {
	SignalType    string  `json:"signal_type"`
	WindowStart   string  `json:"window_start"`
	WindowEnd     string  `json:"window_end"`
	Signals       int     `json:"signals"`
	Cases         int     `json:"cases"`
	FalsePositive int     `json:"false_positives"`
	Escalated     int     `json:"escalated"`
	FPRate        float64 `json:"fp_rate_pct"`
	FPTargetPct   float64 `json:"fp_target_pct"`
	MeetsTarget   bool    `json:"meets_target"`
}

// Backtest measures realized false-positive rate for a signal class
// over [from, to): cased+dispositioned outcomes — DISMISSED signals
// and CLOSED_FALSE_POSITIVE cases count as false positives.
func (s *TuningService) Backtest(ctx context.Context, signalType string,
	from, to time.Time) (*BacktestResult, error) {
	r := &BacktestResult{SignalType: signalType,
		WindowStart: from.UTC().Format("2006-01-02"),
		WindowEnd:   to.UTC().Format("2006-01-02"),
		FPTargetPct: DefaultFPTargets[signalType]}
	if err := s.pool.QueryRow(ctx, `
		SELECT count(*),
		       count(*) FILTER (WHERE status = 'DISMISSED')
		  FROM surveillance_signals
		 WHERE signal_type = $1 AND created_at >= $2 AND created_at < $3`,
		signalType, from, to).Scan(&r.Signals, &r.FalsePositive); err != nil {
		return nil, excerrors.New("INTERNAL_ERROR", "backtest signals: "+err.Error())
	}
	var fpCases int
	if err := s.pool.QueryRow(ctx, `
		SELECT count(*),
		       count(*) FILTER (WHERE status = 'CLOSED_FALSE_POSITIVE'),
		       count(*) FILTER (WHERE status IN
		           ('ESCALATED_SAR','ESCALATED_STR','ESCALATED_ACTION'))
		  FROM surveillance_cases
		 WHERE signal_type = $1 AND opened_at >= $2 AND opened_at < $3`,
		signalType, from, to).Scan(&r.Cases, &fpCases,
		&r.Escalated); err != nil {
		return nil, excerrors.New("INTERNAL_ERROR", "backtest cases: "+err.Error())
	}
	// Fold signal-level dismissals and case-level FPs together.
	r.FalsePositive += fpCases
	denom := r.Signals
	if denom == 0 {
		r.FPRate = 0
		r.MeetsTarget = true
		return r, nil
	}
	r.FPRate = float64(r.FalsePositive) / float64(denom) * 100.0
	r.MeetsTarget = r.FPRate <= r.FPTargetPct
	return r, nil
}

// RecordCalibration stamps the backtest payload onto a tuning version
// — activation reviewers see measured accuracy before approving.
func (s *TuningService) RecordCalibration(ctx context.Context, actorID int64,
	signalType string, version int, result *BacktestResult) error {
	if err := s.checkRole(ctx, actorID); err != nil {
		return err
	}
	b, err := json.Marshal(result)
	if err != nil {
		return excerrors.New("INVALID_REQUEST", "calibration marshal")
	}
	tag, err := s.pool.Exec(ctx, `
		UPDATE surveillance_signal_tuning
		   SET calibration = $3, updated_at = now()
		 WHERE signal_type = $1 AND version = $2`,
		signalType, version, b)
	if err != nil {
		return excerrors.New("INTERNAL_ERROR", "calibration: "+err.Error())
	}
	if tag.RowsAffected() == 0 {
		return excerrors.New("NOT_FOUND", "tuning version not found")
	}
	return nil
}

func tuningScan(t *SignalTuning) []any {
	return []any{&t.ID, &t.SignalType, &t.Version, &t.Params,
		&t.FPTargetPct, &t.Calibration, &t.Status, &t.EffectiveFrom,
		&t.CreatedBy, &t.CreatedAt, &t.UpdatedAt}
}
