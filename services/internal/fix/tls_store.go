// tls_store.go — Task 18.3.11: PostgreSQL BindingLookup over the
// migration-052 certificate columns on fix_sessions (spec §5.20/§9.7).
package fix

import (
	"context"

	"github.com/jackc/pgx/v5/pgxpool"
)

// PgBindingLookup resolves presented certificate fingerprints to
// session bindings. Rollover rows only resolve while their rotation
// window is still open (the VerifyPeer path re-checks against the
// listener clock — this filter is the store-side fast path).
type PgBindingLookup struct {
	pool *pgxpool.Pool
}

// NewPgBindingLookup builds the production lookup.
func NewPgBindingLookup(pool *pgxpool.Pool) *PgBindingLookup {
	return &PgBindingLookup{pool: pool}
}

func (l *PgBindingLookup) BindingByFingerprint(ctx context.Context,
	fingerprint string) (*CertBinding, MatchPath, error) {
	b := &CertBinding{}
	var path MatchPath
	row := l.pool.QueryRow(ctx, `
		SELECT session_id, account_id, cert_fingerprint,
		       COALESCE(cert_cn,''), environment
		FROM fix_sessions
		WHERE cert_fingerprint = $1`, fingerprint)
	err := row.Scan(&b.SessionID, &b.AccountID, &b.Fingerprint,
		&b.CN, &b.Environment)
	if isNoRows(err) {
		// rollover path — only while the rotation window is open
		row = l.pool.QueryRow(ctx, `
			SELECT session_id, account_id, cert_fingerprint,
			       cert_rollover_fingerprint, cert_rotation_ends_at,
			       COALESCE(cert_cn,''), environment
			FROM fix_sessions
			WHERE cert_rollover_fingerprint = $1
			  AND cert_rotation_ends_at > now()`, fingerprint)
		var rollFP *string
		err = row.Scan(&b.SessionID, &b.AccountID, &b.Fingerprint,
			&rollFP, &b.RolloverEndsAt, &b.CN, &b.Environment)
		if isNoRows(err) {
			return nil, "", nil
		}
		if err != nil {
			return nil, "", err
		}
		if rollFP != nil {
			b.RolloverFingerprint = *rollFP
		}
		return b, MatchRollover, nil
	}
	if err != nil {
		return nil, "", err
	}
	path = MatchPrimary
	return b, path, nil
}

// MemoryBindingLookup is the test/dev BindingLookup.
type MemoryBindingLookup struct {
	ByFingerprint map[string]*CertBinding
}

// Lookup installs a primary binding; LookupRollover a rollover one.
func (m *MemoryBindingLookup) Lookup(fp string, b *CertBinding) {
	if m.ByFingerprint == nil {
		m.ByFingerprint = map[string]*CertBinding{}
	}
	m.ByFingerprint["P:"+fp] = b
}

// LookupRollover installs the fingerprint under the rollover path.
func (m *MemoryBindingLookup) LookupRollover(fp string, b *CertBinding) {
	if m.ByFingerprint == nil {
		m.ByFingerprint = map[string]*CertBinding{}
	}
	m.ByFingerprint["R:"+fp] = b
}

func (m *MemoryBindingLookup) BindingByFingerprint(_ context.Context,
	fingerprint string) (*CertBinding, MatchPath, error) {
	if m == nil {
		return nil, "", nil
	}
	if b, ok := m.ByFingerprint["P:"+fingerprint]; ok {
		return b, MatchPrimary, nil
	}
	if b, ok := m.ByFingerprint["R:"+fingerprint]; ok {
		return b, MatchRollover, nil
	}
	return nil, "", nil
}
