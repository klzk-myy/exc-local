// Phase-21 Task 21.3.27 (part 1) — tabulated reporting values.
//
// Reporters must resolve every reportable field to a tabulated value
// or a validated identifier — venue LEI, CFTC large-trader thresholds,
// APA/ARM endpoint refs — never a silent hardcode (spec §14.11).
// reporting_values (migration 242) is the register; writes are
// audit-logged (Compliance Officer), reads are open to reporters and
// the Read-Only Auditor.
package compliance

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"exchange/internal/admin"
	"exchange/internal/compliance/reporting"

	excerrors "exchange/pkg/errors"
)

// Reporting scope vocabulary — extend by adding rows, no schema churn.
const (
	RepScopeVenueLEI       = "VENUE_LEI"
	RepScopeReportEndpoint = "REPORT_ENDPOINT"
	RepScopeCFTCThreshold  = "CFTC_LIMIT"
	RepScopeAPAARM         = "APA_ARM"
	RepScopeNCARef         = "NCA_REFERENCE" // regulator contact refs
)

// ReportingValue is one register row.
type ReportingValue struct {
	ID            int64           `json:"id"`
	Scope         string          `json:"scope"`
	Key           string          `json:"key"`
	Value         json.RawMessage `json:"value"`
	Source        string          `json:"source"`
	EffectiveFrom time.Time       `json:"effective_from"`
	UpdatedAt     time.Time       `json:"updated_at"`
}

// ReportingValues is the register service.
type ReportingValues struct {
	pool     *pgxpool.Pool
	resolver HoldRoleResolver
}

// NewReportingValues binds the register.
func NewReportingValues(pool *pgxpool.Pool,
	resolver HoldRoleResolver) *ReportingValues {
	return &ReportingValues{pool: pool, resolver: resolver}
}

// Get resolves (scope, key); ok=false when unset (the caller must
// then fail its submission — never substitute a guess).
func (s *ReportingValues) Get(ctx context.Context, scope,
	key string) (json.RawMessage, bool, error) {
	var v []byte
	err := s.pool.QueryRow(ctx,
		`SELECT value FROM reporting_values WHERE scope=$1 AND key=$2`,
		scope, key).Scan(&v)
	if err == pgx.ErrNoRows {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, excerrors.New("INTERNAL_ERROR",
			"reporting value lookup: "+err.Error())
	}
	return json.RawMessage(v), true, nil
}

// GetString resolves a string-valued row.
func (s *ReportingValues) GetString(ctx context.Context, scope,
	key string) (string, bool, error) {
	v, ok, err := s.Get(ctx, scope, key)
	if err != nil || !ok {
		return "", ok, err
	}
	var str string
	if err := json.Unmarshal(v, &str); err != nil {
		return "", false, excerrors.New("INTERNAL_ERROR",
			fmt.Sprintf("reporting value %s/%s is not a string", scope, key))
	}
	return str, true, nil
}

// Set upserts a value — Compliance Officer / Super Admin; the change
// is audit-logged. VENUE_LEI values are checksum-validated (ISO 17442)
// before they land.
func (s *ReportingValues) Set(ctx context.Context, actorID int64,
	scope, key string, value json.RawMessage, source string) (*ReportingValue, error) {
	if scope == "" || key == "" || len(value) == 0 {
		return nil, excerrors.New("INVALID_REQUEST",
			"scope, key and value are required")
	}
	if scope == RepScopeVenueLEI {
		var lei string
		if json.Unmarshal(value, &lei) != nil ||
			reporting.ValidateLEI(lei) != nil {
			return nil, excerrors.New("INVALID_REQUEST",
				"VENUE_LEI value must be a valid ISO 17442 LEI")
		}
	}
	if s.resolver != nil {
		role, err := s.resolver(ctx, actorID)
		if err != nil {
			return nil, excerrors.New("INTERNAL_ERROR",
				"role lookup: "+err.Error())
		}
		if role != "Compliance Officer" && role != "Super Admin" {
			return nil, excerrors.New("UNAUTHORIZED_ROLE",
				"role "+role+" cannot set reporting values")
		}
	}
	var out ReportingValue
	err := s.pool.QueryRow(ctx, `
		INSERT INTO reporting_values (scope, key, value, source)
		VALUES ($1,$2,$3,$4)
		ON CONFLICT (scope, key) DO UPDATE SET
		    value=$3, source=$4, updated_at=now()
		RETURNING id, scope, key, value, source, effective_from, updated_at`,
		scope, key, value, source).
		Scan(&out.ID, &out.Scope, &out.Key, &out.Value, &out.Source,
			&out.EffectiveFrom, &out.UpdatedAt)
	if err != nil {
		return nil, excerrors.New("INTERNAL_ERROR",
			"reporting value set: "+err.Error())
	}
	_, _, _ = admin.LogAuto(ctx, s.pool, admin.AuditEntry{
		AdminUserID: actorID, Action: "reporting_values.set",
		TargetType: "reporting_value", TargetID: &out.ID,
		AfterState: out,
	})
	return &out, nil
}

// List reads the register (auditor dashboard).
func (s *ReportingValues) List(ctx context.Context,
	scope string, limit int) ([]ReportingValue, error) {
	if limit <= 0 || limit > 500 {
		limit = 200
	}
	q := `SELECT id, scope, key, value, source, effective_from, updated_at
	        FROM reporting_values`
	args := []any{}
	if scope != "" {
		q += " WHERE scope = $1"
		args = append(args, scope)
	}
	q += fmt.Sprintf(" ORDER BY scope, key LIMIT %d", limit)
	rows, err := s.pool.Query(ctx, q, args...)
	if err != nil {
		return nil, excerrors.New("INTERNAL_ERROR",
			"reporting values: "+err.Error())
	}
	defer rows.Close()
	var out []ReportingValue
	for rows.Next() {
		var v ReportingValue
		if err := rows.Scan(&v.ID, &v.Scope, &v.Key, &v.Value,
			&v.Source, &v.EffectiveFrom, &v.UpdatedAt); err != nil {
			return nil, excerrors.New("INTERNAL_ERROR",
				"reporting value row: "+err.Error())
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// CFTCThreshold resolves the large-trader reporting level for a pair —
// the tabulated value Task 21.3.9's reporter consumes (class default
// rows first; instrument-scoped rows win when present).
func (s *ReportingValues) CFTCThreshold(ctx context.Context,
	pair string) (string, bool, error) {
	var thr string
	err := s.pool.QueryRow(ctx, `
		SELECT large_trader_threshold::text FROM cftc_position_limits
		 WHERE currency_pair = $1 AND instrument_type = 'SWAP'
		   AND (effective_to IS NULL OR effective_to >= CURRENT_DATE)
		 ORDER BY instrument_id NULLS LAST, effective_from DESC
		 LIMIT 1`, pair).Scan(&thr)
	if err == pgx.ErrNoRows {
		return "", false, nil
	}
	if err != nil {
		return "", false, excerrors.New("INTERNAL_ERROR",
			"cftc threshold: "+err.Error())
	}
	return thr, true, nil
}

// ValidateLEI re-exports the reporting package's ISO 17442 validator —
// onboarding flows validate counterparty LEIs without importing the
// reporting internals.
func ValidateLEI(lei string) error { return reporting.ValidateLEI(lei) }
