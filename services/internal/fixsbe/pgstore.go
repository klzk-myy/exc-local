// pgstore.go — Postgres-backed SessionStore + schema-registry loader
// (IMP-PLAN Phase-3 Task 4; the migration-228 tables the transport
// package references but never bound).
//
// Provisioning contract: fixsbe_sessions.session_key stores the LOWERCASE
// hex of the client's 32-byte Ed25519 public key — hex(pub) is the
// lookup convention the admin/ops tooling and this store share. The
// table's `state` column maps ACTIVE/DRAINING onto ProvisionedSession
// verbatim; CLOSED and any other value reject at the negotiator.
//
// LoadRegistry materialises fixsbe_schema_registry into the shared
// Phase-06 sbe.Registry — every replica negotiates schema lifecycle from
// one truth instead of per-process schema.yaml copies.
package fixsbe

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"exchange/internal/sbe"
)

// PgSessionStore resolves Ed25519 session keys via the fixsbe_sessions
// table (migration 228).
type PgSessionStore struct{ pool *pgxpool.Pool }

func NewPgSessionStore(pool *pgxpool.Pool) *PgSessionStore {
	return &PgSessionStore{pool: pool}
}

// SessionByPubKey hex-encodes pub and looks up the session_key row.
// Unknown key → (nil, nil); the negotiator turns that into a reject.
func (s *PgSessionStore) SessionByPubKey(ctx context.Context,
	pub [32]byte) (*ProvisionedSession, error) {
	key := hex.EncodeToString(pub[:])
	var sess ProvisionedSession
	var account *int64
	err := s.pool.QueryRow(ctx, `
		SELECT comp_id, account_id, sni, state
		FROM fixsbe_sessions
		WHERE session_key = $1`, key).
		Scan(&sess.SessionID, &account, &sess.SNIHostname, &sess.Status)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("fixsbe: session lookup: %w", err)
	}
	if account != nil {
		sess.AccountID = *account
	}
	sess.PubKey = pub
	return &sess, nil
}

// TouchSession refreshes last_seen_at and the in/out message counters —
// the ops-visible liveness signal on fixsbe_sessions.
func (s *PgSessionStore) TouchSession(ctx context.Context, sessionID string,
	msgsIn, msgsOut int64) error {
	_, err := s.pool.Exec(ctx, `
		UPDATE fixsbe_sessions
		SET last_seen_at = now(),
		    messages_in  = messages_in + $2,
		    messages_out = messages_out + $3
		WHERE comp_id = $1`, sessionID, msgsIn, msgsOut)
	return err
}

// LoadRegistry reads fixsbe_schema_registry into a fresh sbe.Registry.
// State strings come from the table's CHECK (ACTIVE/DEPRECATED/RETIRED);
// the sbe enum spells them lowercase — normalise here. A retired/unknown
// schema then fails closed inside Registry.Negotiate.
func LoadRegistry(ctx context.Context, pool *pgxpool.Pool) (*sbe.Registry, error) {
	rows, err := pool.Query(ctx, `
		SELECT schema_id, version, state, deprecated_at, retired_at, notes
		FROM fixsbe_schema_registry`)
	if err != nil {
		return nil, fmt.Errorf("fixsbe: schema registry load: %w", err)
	}
	defer rows.Close()

	reg := sbe.NewRegistry()
	n := 0
	for rows.Next() {
		var (
			schemaID, version int16
			state             string
			deprecatedAt      *time.Time
			retiredAt         *time.Time
			notes             string
		)
		if err := rows.Scan(&schemaID, &version, &state,
			&deprecatedAt, &retiredAt, &notes); err != nil {
			return nil, fmt.Errorf("fixsbe: registry row: %w", err)
		}
		sv := sbe.SchemaVersion{
			SchemaID: uint16(schemaID), Version: uint16(version),
			Note: notes,
		}
		switch state {
		case "ACTIVE":
			sv.State = sbe.LifecycleActive
		case "DEPRECATED":
			sv.State = sbe.LifecycleDeprecated
		case "RETIRED":
			sv.State = sbe.LifecycleRetired
		default:
			return nil, fmt.Errorf("fixsbe: schema %d v%d unknown state %q",
				schemaID, version, state)
		}
		if deprecatedAt != nil {
			sv.DeprecatedAt = *deprecatedAt
		}
		if retiredAt != nil {
			sv.RetiresAt = *retiredAt
		}
		if err := reg.Register(sv); err != nil {
			return nil, err
		}
		n++
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("fixsbe: registry scan: %w", err)
	}
	if n == 0 {
		return nil, errors.New("fixsbe: schema registry empty — no schema can negotiate")
	}
	return reg, nil
}
