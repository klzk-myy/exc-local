// data_residency.go — Phase-21 Task 21.3.18: jurisdictional data-
// residency enforcement and cross-border administrative access
// controls.
//
// Spec surface: §14.13 residency, §19.12 retention, Phase-21 AC:
// associate participant profiles/order streams/KYC documents with a
// legal residency jurisdiction (jurisdiction_code), resolve to regional
// PostgreSQL/ClickHouse/S3 placement + region-scoped KMS keys, reject
// unknown jurisdictions, require justification + audit for cross-
// border administrative queries, and verify data-protection legality
// during replication.
//
// The DB copy (data_residency_policies, migration 244) is authoritative
// at runtime; deploy/storage/residency_policy.yml seeds bootstraps and
// documents the intended environment split.
package compliance

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"gopkg.in/yaml.v3"

	"exchange/internal/audit"
	excerrors "exchange/pkg/errors"
)

// ResidencyPolicy is one jurisdiction → placement row.
type ResidencyPolicy struct {
	JurisdictionCode   string   `json:"jurisdiction_code" yaml:"jurisdiction_code"`
	HomeRegion         string   `json:"home_region" yaml:"home_region"`
	KMSKeyID           string   `json:"kms_key_id" yaml:"kms_key_id"`
	S3Bucket           string   `json:"s3_bucket" yaml:"s3_bucket"`
	PGPartition        string   `json:"pg_partition" yaml:"pg_partition"`
	AppliesTo          []string `json:"applies_to" yaml:"applies_to"`
	Adequate           bool     `json:"adequate" yaml:"adequate"`
	TransferInstrument string   `json:"transfer_instrument" yaml:"transfer_instrument"`
	Description        string   `json:"description" yaml:"description"`
}

// ResidencyService resolves jurisdiction → placement and enforces
// cross-border access rules.
type ResidencyService struct {
	pool *pgxpool.Pool
	now  func() time.Time

	mu        sync.RWMutex
	policies  map[string]ResidencyPolicy // jurisdiction_code → policy
	byCountry map[string]string          // country → jurisdiction_code
	loadedAt  time.Time
	ttl       time.Duration
}

// NewResidencyService binds the pool.
func NewResidencyService(pool *pgxpool.Pool) (*ResidencyService, error) {
	if pool == nil {
		return nil, fmt.Errorf("compliance: residency pool is nil")
	}
	return &ResidencyService{
		pool: pool, now: time.Now, ttl: 60 * time.Second,
		policies:  map[string]ResidencyPolicy{},
		byCountry: map[string]string{},
	}, nil
}

// SetClockForTest overrides the clock; tests only.
func (s *ResidencyService) SetClockForTest(now func() time.Time) { s.now = now }

// SetPoliciesForTest injects the policy set; tests only.
func (s *ResidencyService) SetPoliciesForTest(ps []ResidencyPolicy) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.policies = map[string]ResidencyPolicy{}
	s.byCountry = map[string]string{}
	for _, p := range ps {
		s.policies[p.JurisdictionCode] = p
		for _, cc := range p.AppliesTo {
			s.byCountry[strings.ToUpper(cc)] = p.JurisdictionCode
		}
	}
	s.loadedAt = s.now()
}

// policyFile mirrors deploy/storage/residency_policy.yml.
type policyFile struct {
	Version  int               `yaml:"version"`
	Policies []ResidencyPolicy `yaml:"policies"`
}

// LoadPolicyFile parses a residency_policy.yml — used at bootstrap to
// seed the table when empty and by tests for seed parity.
func LoadPolicyFile(path string) ([]ResidencyPolicy, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("residency policy read: %w", err)
	}
	var f policyFile
	if err := yaml.Unmarshal(b, &f); err != nil {
		return nil, fmt.Errorf("residency policy parse: %w", err)
	}
	if len(f.Policies) == 0 {
		return nil, fmt.Errorf("residency policy: no policies in %s", path)
	}
	for _, p := range f.Policies {
		if p.JurisdictionCode == "" || p.HomeRegion == "" ||
			p.KMSKeyID == "" || p.S3Bucket == "" {
			return nil, fmt.Errorf(
				"residency policy %q incomplete", p.JurisdictionCode)
		}
	}
	return f.Policies, nil
}

// reload refreshes the cached policy set (TTL). Fail-closed: a DB error
// keeps the last good set; if nothing was ever loaded the error
// propagates and callers refuse.
func (s *ResidencyService) reload(ctx context.Context) error {
	s.mu.RLock()
	fresh := s.now().Sub(s.loadedAt) < s.ttl && len(s.policies) > 0
	s.mu.RUnlock()
	if fresh {
		return nil
	}
	rows, err := s.pool.Query(ctx, `
		SELECT jurisdiction_code, home_region, kms_key_id, s3_bucket,
		       pg_partition, applies_to, adequate, transfer_instrument,
		       description
		  FROM data_residency_policies`)
	if err != nil {
		s.mu.RLock()
		loaded := len(s.policies) > 0
		s.mu.RUnlock()
		if loaded {
			return nil // stale beats down
		}
		return fmt.Errorf("residency policies unavailable: %w", err)
	}
	defer rows.Close()
	m := map[string]ResidencyPolicy{}
	byCountry := map[string]string{}
	for rows.Next() {
		var p ResidencyPolicy
		var applies []byte
		if err := rows.Scan(&p.JurisdictionCode, &p.HomeRegion,
			&p.KMSKeyID, &p.S3Bucket, &p.PGPartition, &applies,
			&p.Adequate, &p.TransferInstrument, &p.Description); err != nil {
			return fmt.Errorf("residency policy scan: %w", err)
		}
		p.AppliesTo = parseStringList(applies)
		m[p.JurisdictionCode] = p
		for _, cc := range p.AppliesTo {
			byCountry[strings.ToUpper(cc)] = p.JurisdictionCode
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("residency policy read: %w", err)
	}
	s.mu.Lock()
	s.policies, s.byCountry, s.loadedAt = m, byCountry, s.now()
	s.mu.Unlock()
	return nil
}

// ResolveCountry maps an ISO country code to its residency tag.
// Fail-closed: unknown → error (never guess a placement). 'ROW' is
// returned only for countries no policy claims.
func (s *ResidencyService) ResolveCountry(ctx context.Context, country string) (string, error) {
	if err := s.reload(ctx); err != nil {
		return "", excerrors.Wrap("SERVICE_DEGRADED", "residency policy", err)
	}
	cc := strings.ToUpper(strings.TrimSpace(country))
	if cc == "" {
		return "", excerrors.New("INVALID_REQUEST", "empty country")
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if jc, ok := s.byCountry[cc]; ok {
		return jc, nil
	}
	// Unmapped country → ROW default when present (documented
	// catch-all); absent ROW is a hard failure.
	if _, ok := s.policies["ROW"]; ok {
		return "ROW", nil
	}
	return "", excerrors.New("INVALID_REQUEST",
		fmt.Sprintf("no residency policy covers country %q", cc))
}

// Policy returns the placement record for a jurisdiction tag.
func (s *ResidencyService) Policy(ctx context.Context, jurisdictionCode string) (*ResidencyPolicy, error) {
	if err := s.reload(ctx); err != nil {
		return nil, excerrors.Wrap("SERVICE_DEGRADED", "residency policy", err)
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	p, ok := s.policies[strings.ToUpper(jurisdictionCode)]
	if !ok {
		return nil, excerrors.New("INVALID_REQUEST",
			fmt.Sprintf("unsupported jurisdiction_code %q", jurisdictionCode))
	}
	return &p, nil
}

// Policies lists the active policy set (admin/audit surface).
func (s *ResidencyService) Policies(ctx context.Context) ([]ResidencyPolicy, error) {
	if err := s.reload(ctx); err != nil {
		return nil, excerrors.Wrap("SERVICE_DEGRADED", "residency policy", err)
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]ResidencyPolicy, 0, len(s.policies))
	for _, p := range s.policies {
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].JurisdictionCode < out[j].JurisdictionCode
	})
	return out, nil
}

// JurisdictionForAccount resolves the account's residency tag: the
// pinned accounts.jurisdiction_code wins; otherwise fall back to the
// user's declared country → policy map → kyc_submissions jurisdiction.
// Fail-closed: no resolvable tag → error.
func (s *ResidencyService) JurisdictionForAccount(ctx context.Context, accountID int64) (string, error) {
	var (
		pinned, country *string
	)
	err := s.pool.QueryRow(ctx, `
		SELECT a.jurisdiction_code, u.country
		  FROM accounts a JOIN users u ON u.id = a.user_id
		 WHERE a.id=$1`, accountID).Scan(&pinned, &country)
	if err == pgx.ErrNoRows {
		return "", excerrors.New("NOT_FOUND", "account not found")
	}
	if err != nil {
		return "", fmt.Errorf("residency: account read: %w", err)
	}
	if pinned != nil && *pinned != "" {
		return *pinned, nil
	}
	if country != nil && *country != "" {
		return s.ResolveCountry(ctx, *country)
	}
	// Last resort: the most recent KYC submission's jurisdiction.
	var kycJ *string
	err = s.pool.QueryRow(ctx, `
		SELECT jurisdiction FROM kyc_submissions
		 WHERE account_id=$1 AND jurisdiction IS NOT NULL
		 ORDER BY id DESC LIMIT 1`, accountID).Scan(&kycJ)
	if err == nil && kycJ != nil && *kycJ != "" {
		return s.ResolveCountry(ctx, *kycJ)
	}
	if err != nil && err != pgx.ErrNoRows {
		return "", fmt.Errorf("residency: kyc jurisdiction read: %w", err)
	}
	return "", excerrors.New("SERVICE_DEGRADED",
		fmt.Sprintf("no residency jurisdiction resolvable for account %d", accountID))
}

// PinAccount writes the residency tag (idempotent for the same code;
// repinning to a different code is a privileged change — audited with
// the prior value).
func (s *ResidencyService) PinAccount(ctx context.Context, accountID int64,
	jurisdictionCode, actor string) (*ResidencyPolicy, error) {
	pol, err := s.Policy(ctx, jurisdictionCode)
	if err != nil {
		return nil, err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("residency: pin tx: %w", err)
	}
	defer tx.Rollback(ctx)
	var prior *string
	if err := tx.QueryRow(ctx,
		`SELECT jurisdiction_code FROM accounts WHERE id=$1 FOR UPDATE`,
		accountID).Scan(&prior); err != nil {
		return nil, fmt.Errorf("residency: account lock: %w", err)
	}
	if _, err := tx.Exec(ctx,
		`UPDATE accounts SET jurisdiction_code=$2, updated_at=now()
		 WHERE id=$1`, accountID, pol.JurisdictionCode); err != nil {
		return nil, fmt.Errorf("residency: pin write: %w", err)
	}
	if _, err := audit.Append(ctx, tx, "accounts", &accountID,
		"RESIDENCY_PIN", nil); err != nil {
		return nil, fmt.Errorf("residency: pin audit: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("residency: pin commit: %w", err)
	}
	return pol, nil
}

// AuthorizeAccess enforces the cross-border rule for administrative
// access to a jurisdiction's resident data:
//
//   - same-region access (admin's region grants include the account's
//     home_region, or the admin is global) → allowed;
//   - cross-border → requires a non-empty justification; every decision
//     lands in data_residency_access_log. The caller attaches the log
//     row's masked flag to the response path (non-local auditors see
//     masked PII; dual-control elevation is a Task 21.3.21/separate
//     gate — this seam reports `masked=true` so the read path redacts).
//
// Fail-closed: unresolvable jurisdiction, missing justification, or a
// policy miss all refuse.
func (s *ResidencyService) AuthorizeAccess(ctx context.Context, adminUserID int64,
	adminRegions []string, accountID int64, action, justification string) (masked bool, err error) {

	jc, err := s.JurisdictionForAccount(ctx, accountID)
	if err != nil {
		return false, err
	}
	pol, err := s.Policy(ctx, jc)
	if err != nil {
		return false, err
	}

	cross := true
	for _, r := range adminRegions {
		if strings.EqualFold(strings.TrimSpace(r), pol.HomeRegion) ||
			strings.EqualFold(strings.TrimSpace(r), "*") {
			cross = false
			break
		}
	}
	if len(adminRegions) == 0 {
		cross = true // no region grants at all = cross-border everywhere
	}
	masked = cross
	if cross && strings.TrimSpace(justification) == "" {
		return false, excerrors.New("CROSS_BORDER_JUSTIFICATION_REQUIRED",
			fmt.Sprintf("account %d is resident in %s (%s) — cross-border "+
				"administrative access requires justification",
				accountID, jc, pol.HomeRegion))
	}

	regionsJSON := mustJSON(adminRegions)
	if _, err := s.pool.Exec(ctx, `
		INSERT INTO data_residency_access_log
		    (admin_user_id, admin_regions, account_id, jurisdiction_code,
		     home_region, action, cross_border, justification, masked)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)`,
		adminUserID, regionsJSON, accountID, jc, pol.HomeRegion,
		action, cross, nilIfEmpty(justification), masked); err != nil {
		return false, fmt.Errorf("residency: access log: %w", err)
	}
	return masked, nil
}

// AssertReplicationAllowed is the data-protection legality check for
// cross-border flows (replication, DR failover, exports to a foreign
// region). A transfer is legal when the destination jurisdiction's
// policy is adequate OR carries a valid transfer instrument (SCC/BCR).
// Fail-closed: neither → error.
func (s *ResidencyService) AssertReplicationAllowed(ctx context.Context,
	srcJurisdiction, dstJurisdiction string) error {
	src, err := s.Policy(ctx, srcJurisdiction)
	if err != nil {
		return err
	}
	dst, err := s.Policy(ctx, dstJurisdiction)
	if err != nil {
		return err
	}
	if src.JurisdictionCode == dst.JurisdictionCode ||
		src.HomeRegion == dst.HomeRegion {
		return nil // intra-region replication is always legal
	}
	if dst.Adequate || dst.TransferInstrument == "ADEQUACY" ||
		dst.TransferInstrument == "SCC" || dst.TransferInstrument == "BCR" {
		return nil
	}
	return excerrors.New("RESIDENCY_VIOLATION",
		fmt.Sprintf("transfer %s→%s unlawful: destination %s has no "+
			"adequacy decision or transfer instrument",
			src.JurisdictionCode, dst.JurisdictionCode, dst.HomeRegion))
}

// VerifyPlacement checks that a storage target matches the
// jurisdiction's policy — used by writers before persisting resident
// data (fail-closed on mismatch).
func (s *ResidencyService) VerifyPlacement(ctx context.Context,
	jurisdictionCode, bucket, kmsKeyID string) error {
	pol, err := s.Policy(ctx, jurisdictionCode)
	if err != nil {
		return err
	}
	if pol.S3Bucket != bucket {
		return excerrors.New("RESIDENCY_VIOLATION",
			fmt.Sprintf("jurisdiction %s requires bucket %s, got %s",
				jurisdictionCode, pol.S3Bucket, bucket))
	}
	if kmsKeyID != "" && pol.KMSKeyID != kmsKeyID {
		return excerrors.New("RESIDENCY_VIOLATION",
			fmt.Sprintf("jurisdiction %s requires KMS key %s, got %s",
				jurisdictionCode, pol.KMSKeyID, kmsKeyID))
	}
	return nil
}

// AccessLog lists the audit trail (admin read surface).
func (s *ResidencyService) AccessLog(ctx context.Context, limit int) ([]map[string]any, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	rows, err := s.pool.Query(ctx, `
		SELECT id, admin_user_id, admin_regions, account_id,
		       jurisdiction_code, home_region, action, cross_border,
		       justification, masked, created_at
		  FROM data_residency_access_log ORDER BY id DESC LIMIT $1`, limit)
	if err != nil {
		return nil, fmt.Errorf("residency: access log read: %w", err)
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		var (
			id, adminID        int64
			regions            []byte
			acct               *int64
			jc, region, action string
			cross, masked      bool
			just               *string
			at                 time.Time
		)
		if err := rows.Scan(&id, &adminID, &regions, &acct, &jc, &region,
			&action, &cross, &just, &masked, &at); err != nil {
			return nil, err
		}
		out = append(out, map[string]any{
			"id": id, "admin_user_id": adminID,
			"admin_regions": json.RawMessage(regions), "account_id": acct,
			"jurisdiction_code": jc, "home_region": region,
			"action": action, "cross_border": cross,
			"justification": just, "masked": masked, "at": at.UTC(),
		})
	}
	return out, rows.Err()
}

func nilIfEmpty(s string) *string {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	return &s
}

// parseStringList decodes a JSON string array (applies_to).
func parseStringList(b []byte) []string {
	var out []string
	if err := json.Unmarshal(b, &out); err != nil {
		return nil
	}
	return out
}
