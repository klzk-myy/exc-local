// store.go — certification Store implementations: PgStore over
// fix_certifications (migration 052) and MemoryStore for tests/dev.
package certification

import (
	"context"
	"errors"
	"sort"
	"sync"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ---------------------------------------------------------------------------
// PgStore
// ---------------------------------------------------------------------------

// PgStore is the production Store over fix_certifications.
type PgStore struct{ pool *pgxpool.Pool }

// NewPgStore builds the production store.
func NewPgStore(pool *pgxpool.Pool) *PgStore { return &PgStore{pool: pool} }

const certCols = `
	id, participant_id, client_build, dictionary_version,
	venue_schema_version, environment, pack_version, result,
	evidence_hash, certified_at, expires_at, status,
	COALESCE(revoked_reason,'')`

func scanCert(row pgx.Row) (*Record, error) {
	r := &Record{}
	err := row.Scan(&r.ID, &r.ParticipantID, &r.ClientBuild,
		&r.DictionaryVersion, &r.VenueSchemaVersion, &r.Environment,
		&r.PackVersion, &r.Result, &r.EvidenceHash,
		&r.CertifiedAt, &r.ExpiresAt, &r.Status, &r.RevokedReason)
	return r, err
}

func (s *PgStore) Record(ctx context.Context, r *Record) (*Record, error) {
	var id int64
	err := s.pool.QueryRow(ctx, `
		INSERT INTO fix_certifications
		    (participant_id, client_build, dictionary_version,
		     venue_schema_version, environment, pack_version, result,
		     evidence_hash, certified_at, expires_at, status, revoked_reason)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12)
		RETURNING id`,
		r.ParticipantID, r.ClientBuild, r.DictionaryVersion,
		r.VenueSchemaVersion, r.Environment, r.PackVersion, r.Result,
		r.EvidenceHash, r.CertifiedAt, r.ExpiresAt, string(r.Status),
		nullStr(r.RevokedReason)).Scan(&id)
	if err != nil {
		return nil, err
	}
	out := *r
	out.ID = id
	return &out, nil
}

func nullStr(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func (s *PgStore) Latest(ctx context.Context, participant, build, dict,
	schema, env string) (*Record, error) {
	r, err := scanCert(s.pool.QueryRow(ctx, `
		SELECT `+certCols+` FROM fix_certifications
		WHERE participant_id = $1 AND client_build = $2
		  AND dictionary_version = $3 AND venue_schema_version = $4
		  AND environment = $5
		ORDER BY certified_at DESC, id DESC
		LIMIT 1`, participant, build, dict, schema, env))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	return r, err
}

func (s *PgStore) Revoke(ctx context.Context, id int64, reason string) error {
	_, err := s.pool.Exec(ctx, `
		UPDATE fix_certifications
		SET status = 'REVOKED', revoked_reason = $2, updated_at = now()
		WHERE id = $1`, id, reason)
	return err
}

func (s *PgStore) RevokeSchema(ctx context.Context, schemaVersion,
	reason string) (int64, error) {
	tag, err := s.pool.Exec(ctx, `
		UPDATE fix_certifications
		SET status = 'REVOKED', revoked_reason = $2, updated_at = now()
		WHERE venue_schema_version = $1 AND status = 'ACTIVE'`,
		schemaVersion, reason)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

// ---------------------------------------------------------------------------
// MemoryStore — tests/dev
// ---------------------------------------------------------------------------

// MemoryStore keeps records in insertion order; Latest returns the
// newest row per tuple (same semantics as PgStore).
type MemoryStore struct {
	mu     sync.Mutex
	rows   []*Record
	nextID int64
}

// NewMemoryStore builds an empty store.
func NewMemoryStore() *MemoryStore { return &MemoryStore{nextID: 1} }

func (m *MemoryStore) Record(_ context.Context, r *Record) (*Record, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	cp := *r
	cp.ID = m.nextID
	m.nextID++
	m.rows = append(m.rows, &cp)
	out := cp
	return &out, nil
}

func (m *MemoryStore) Latest(_ context.Context, participant, build, dict,
	schema, env string) (*Record, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var best *Record
	for _, r := range m.rows {
		if r.ParticipantID == participant && r.ClientBuild == build &&
			r.DictionaryVersion == dict && r.VenueSchemaVersion == schema &&
			r.Environment == env {
			if best == nil || r.CertifiedAt.After(best.CertifiedAt) ||
				(r.CertifiedAt.Equal(best.CertifiedAt) && r.ID > best.ID) {
				best = r
			}
		}
	}
	if best == nil {
		return nil, nil
	}
	out := *best
	return &out, nil
}

func (m *MemoryStore) Revoke(_ context.Context, id int64, reason string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, r := range m.rows {
		if r.ID == id {
			r.Status = StatusRevoked
			r.RevokedReason = reason
			return nil
		}
	}
	return errors.New("certification: record not found")
}

func (m *MemoryStore) RevokeSchema(_ context.Context, schemaVersion,
	reason string) (int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var n int64
	for _, r := range m.rows {
		if r.VenueSchemaVersion == schemaVersion && r.Status == StatusActive {
			r.Status = StatusRevoked
			r.RevokedReason = reason
			n++
		}
	}
	return n, nil
}

// List returns all rows newest-first — ops/tests introspection.
func (m *MemoryStore) List() []*Record {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := append([]*Record(nil), m.rows...)
	sort.Slice(out, func(i, j int) bool { return out[i].ID > out[j].ID })
	return out
}
