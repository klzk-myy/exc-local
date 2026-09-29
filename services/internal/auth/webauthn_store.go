package auth

// webauthn_store.go — persistence for Phase-12 Task 12.3.7:
//   - PgWebAuthnStore: webauthn_credentials rows (migration 068).
//   - RedisWebAuthnChallengeStore: webauthn:challenge:{id} records with
//     ~60s TTL and atomic single-use consume (GETDEL), so a replayed
//     ceremony response can never verify twice.

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	exchredis "exchange/internal/redis"

	goredis "github.com/redis/go-redis/v9"
)

// ---------------------------------------------------------------------------
// PostgreSQL credential store
// ---------------------------------------------------------------------------

// PgWebAuthnStore implements WebAuthnStore over pgx.
type PgWebAuthnStore struct {
	pool *pgxpool.Pool
}

// NewPgWebAuthnStore binds the store; a nil pool fails closed at
// construction.
func NewPgWebAuthnStore(pool *pgxpool.Pool) (*PgWebAuthnStore, error) {
	if pool == nil {
		return nil, newError(CodeAuthInternal, "webauthn store pool is nil")
	}
	return &PgWebAuthnStore{pool: pool}, nil
}

func (s *PgWebAuthnStore) CreateCredential(ctx context.Context, c WebAuthnCredential) (int64, error) {
	transports := c.Transports
	if transports == nil {
		transports = []string{} // NOT NULL column — empty set, never NULL
	}
	var id int64
	err := s.pool.QueryRow(ctx,
		`INSERT INTO webauthn_credentials
		   (user_id, credential_id, public_key, sign_count, transports,
		    aaguid, flags, name)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
		 RETURNING id`,
		c.UserID, c.CredentialID, c.PublicKey, int64(c.SignCount),
		transports, c.AAGUID, int16(c.Flags), c.Name).Scan(&id)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			return 0, newError(CodeWebAuthnFailed, "credential already registered")
		}
		return 0, fmt.Errorf("webauthn credential insert: %w", err)
	}
	return id, nil
}

func (s *PgWebAuthnStore) ActiveCredentialsForUser(ctx context.Context, userID int64) ([]WebAuthnCredential, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT id, user_id, credential_id, public_key, sign_count,
		        transports, aaguid, flags, name, created_at, last_used_at
		   FROM webauthn_credentials
		  WHERE user_id = $1 AND revoked_at IS NULL
		  ORDER BY id`, userID)
	if err != nil {
		return nil, fmt.Errorf("webauthn credential list: %w", err)
	}
	defer rows.Close()
	out := []WebAuthnCredential{}
	for rows.Next() {
		var c WebAuthnCredential
		var signCount int64
		var flags int16
		if err := rows.Scan(&c.ID, &c.UserID, &c.CredentialID, &c.PublicKey,
			&signCount, &c.Transports, &c.AAGUID, &flags, &c.Name,
			&c.CreatedAt, &c.LastUsedAt); err != nil {
			return nil, fmt.Errorf("webauthn credential scan: %w", err)
		}
		c.SignCount = uint32(signCount)
		c.Flags = uint8(flags)
		out = append(out, c)
	}
	return out, rows.Err()
}

// UpdateSignCount persists the verified post-ceremony counter. The
// conditional `sign_count <= $2` guard makes the write itself
// downgrade-resistant: a racing assertion presenting an older counter
// cannot rewind the stored value — combined with the library's
// CloneWarning check this enforces the §12.6 counter invariant at the
// persistence layer too.
func (s *PgWebAuthnStore) UpdateSignCount(ctx context.Context, id int64, signCount uint32) error {
	tag, err := s.pool.Exec(ctx,
		`UPDATE webauthn_credentials
		    SET sign_count = GREATEST(sign_count, $2), last_used_at = now()
		  WHERE id = $1 AND revoked_at IS NULL`, id, int64(signCount))
	if err != nil {
		return fmt.Errorf("webauthn counter update: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return newError(CodeWebAuthnFailed, "credential revoked or missing")
	}
	return nil
}

func (s *PgWebAuthnStore) RevokeCredential(ctx context.Context, id int64) error {
	_, err := s.pool.Exec(ctx,
		`UPDATE webauthn_credentials
		    SET revoked_at = now()
		  WHERE id = $1 AND revoked_at IS NULL`, id)
	if err != nil {
		return fmt.Errorf("webauthn credential revoke: %w", err)
	}
	return nil
}

func (s *PgWebAuthnStore) UserEmail(ctx context.Context, userID int64) (string, error) {
	var email string
	err := s.pool.QueryRow(ctx,
		`SELECT email FROM users WHERE id = $1`, userID).Scan(&email)
	if err == pgx.ErrNoRows {
		return "", newError(CodeUnauthorized, "user not found")
	}
	if err != nil {
		return "", fmt.Errorf("user lookup: %w", err)
	}
	return email, nil
}

// ---------------------------------------------------------------------------
// Redis challenge store — webauthn:challenge:{id}, ~60s, single-use
// ---------------------------------------------------------------------------

// RedisWebAuthnChallengeStore implements WebAuthnChallengeStore on the
// coordination instance (same noeviction keyspace as sessions).
type RedisWebAuthnChallengeStore struct {
	c *exchredis.Client
}

func NewRedisWebAuthnChallengeStore(c *exchredis.Client) *RedisWebAuthnChallengeStore {
	return &RedisWebAuthnChallengeStore{c: c}
}

func (r *RedisWebAuthnChallengeStore) PutChallenge(ctx context.Context, id string, payload []byte, ttl time.Duration) error {
	if err := r.c.Set(ctx, webAuthnChallengePrefix+id, payload, ttl).Err(); err != nil {
		return fmt.Errorf("redis challenge write: %w", err)
	}
	return nil
}

// ConsumeChallenge is GETDEL: the payload is returned exactly once —
// the single-use contract for both ceremonies.
func (r *RedisWebAuthnChallengeStore) ConsumeChallenge(ctx context.Context, id string) ([]byte, bool, error) {
	b, err := r.c.GetDel(ctx, webAuthnChallengePrefix+id).Bytes()
	if err != nil {
		if errors.Is(err, goredis.Nil) {
			return nil, false, nil
		}
		return nil, false, fmt.Errorf("redis challenge consume: %w", err)
	}
	return b, true, nil
}
