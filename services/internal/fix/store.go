package fix

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// PgStore is the PostgreSQL Store over fix_sessions + fix_messages +
// the api_keys credential check (migrations 030/046).
type PgStore struct{ pool *pgxpool.Pool }

// NewPgStore builds the production Store.
func NewPgStore(pool *pgxpool.Pool) *PgStore { return &PgStore{pool: pool} }

func isNoRows(err error) bool { return errors.Is(err, pgx.ErrNoRows) }

const sessionCols = `
	id, session_id, protocol_version, sender_seq_num, target_seq_num,
	status::text, last_heartbeat_at, account_id, api_key_id,
	COALESCE(allowed_instruments,''), cancel_on_disconnect,
	max_msgs_per_sec, created_at, updated_at`

func scanSession(row pgx.Row) (*Session, error) {
	s := &Session{}
	err := row.Scan(&s.ID, &s.SessionID, &s.ProtocolVersion,
		&s.SenderSeqNum, &s.TargetSeqNum, &s.Status, &s.LastHeartbeatAt,
		&s.AccountID, &s.APIKeyID, &s.AllowedInstruments,
		&s.CancelOnDisconnect, &s.MaxMsgsPerSec,
		&s.CreatedAt, &s.UpdatedAt)
	if err != nil {
		return nil, err
	}
	return s, nil
}

func (s *PgStore) SessionByID(ctx context.Context, sessionID string) (*Session, error) {
	row, err := scanSession(s.pool.QueryRow(ctx,
		`SELECT `+sessionCols+` FROM fix_sessions WHERE session_id = $1`, sessionID))
	if isNoRows(err) {
		return nil, nil
	}
	return row, err
}

func (s *PgStore) CreateSession(ctx context.Context, in *Session) (*Session, error) {
	sess := *in
	if sess.ProtocolVersion == "" {
		sess.ProtocolVersion = "FIX.4.4"
	}
	if sess.SenderSeqNum < 1 {
		sess.SenderSeqNum = 1
	}
	if sess.TargetSeqNum < 1 {
		sess.TargetSeqNum = 1
	}
	if sess.Status == "" {
		sess.Status = "DISCONNECTED"
	}
	if sess.MaxMsgsPerSec <= 0 {
		sess.MaxMsgsPerSec = 100
	}
	row, err := scanSession(s.pool.QueryRow(ctx, `
		INSERT INTO fix_sessions
		  (session_id, protocol_version, sender_seq_num, target_seq_num,
		   status, account_id, api_key_id, allowed_instruments,
		   cancel_on_disconnect, max_msgs_per_sec)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)
		ON CONFLICT (session_id) DO UPDATE SET session_id = EXCLUDED.session_id
		RETURNING `+sessionCols,
		sess.SessionID, sess.ProtocolVersion, sess.SenderSeqNum,
		sess.TargetSeqNum, sess.Status, sess.AccountID, sess.APIKeyID,
		nullStr(sess.AllowedInstruments), sess.CancelOnDisconnect,
		sess.MaxMsgsPerSec))
	if err != nil {
		return nil, fmt.Errorf("create fix_session %q: %w", sess.SessionID, err)
	}
	return row, nil
}

func nullStr(s string) *string {
	if s == "" {
		return nil
	}
	v := s
	return &v
}

func (s *PgStore) SetStatus(ctx context.Context, sessionID, status string,
	heartbeatAt *time.Time) error {
	tag, err := s.pool.Exec(ctx, `
		UPDATE fix_sessions
		   SET status = $2,
		       last_heartbeat_at = COALESCE($3, last_heartbeat_at),
		       updated_at = now()
		 WHERE session_id = $1`,
		sessionID, status, heartbeatAt)
	if err != nil {
		return fmt.Errorf("set fix_session status %q: %w", sessionID, err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("set fix_session status: session %q not found", sessionID)
	}
	return nil
}

func (s *PgStore) SetSeq(ctx context.Context, sessionID string, sender, target int64) error {
	tag, err := s.pool.Exec(ctx, `
		UPDATE fix_sessions
		   SET sender_seq_num = $2, target_seq_num = $3, updated_at = now()
		 WHERE session_id = $1`,
		sessionID, sender, target)
	if err != nil {
		return fmt.Errorf("set fix_session seq %q: %w", sessionID, err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("set fix_session seq: session %q not found", sessionID)
	}
	return nil
}

func (s *PgStore) SeqState(ctx context.Context, sessionID string) (int64, int64, bool, error) {
	var sender, target int64
	err := s.pool.QueryRow(ctx,
		`SELECT sender_seq_num, target_seq_num FROM fix_sessions WHERE session_id = $1`,
		sessionID).Scan(&sender, &target)
	if isNoRows(err) {
		return 0, 0, false, nil
	}
	if err != nil {
		return 0, 0, false, fmt.Errorf("fix_session seq state %q: %w", sessionID, err)
	}
	return sender, target, true, nil
}

func (s *PgStore) RefreshSession(context.Context, string) error { return nil }

func (s *PgStore) SaveMessage(ctx context.Context, sessionID string, seqNum int64, msg []byte) error {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO fix_messages (session_id, seq_num, msg)
		VALUES ($1,$2,$3)
		ON CONFLICT (session_id, seq_num) DO NOTHING`,
		sessionID, seqNum, msg)
	if err != nil {
		return fmt.Errorf("save fix_message %q/%d: %w", sessionID, seqNum, err)
	}
	return nil
}

func (s *PgStore) Messages(ctx context.Context, sessionID string, begin, end int64) ([][]byte, error) {
	// FIX ResendRequest EndSeqNo(16)=0 means "to infinity"; the column
	// is BIGINT so cap it rather than branching the SQL.
	if end <= 0 {
		end = 1<<62 - 1
	}
	rows, err := s.pool.Query(ctx, `
		SELECT msg FROM fix_messages
		 WHERE session_id = $1 AND seq_num >= $2 AND seq_num <= $3
		 ORDER BY seq_num`,
		sessionID, begin, end)
	if err != nil {
		return nil, fmt.Errorf("fix_messages range %q [%d,%d]: %w", sessionID, begin, end, err)
	}
	defer rows.Close()
	var out [][]byte
	for rows.Next() {
		var b []byte
		if err := rows.Scan(&b); err != nil {
			return nil, fmt.Errorf("fix_messages scan: %w", err)
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

func (s *PgStore) PurgeMessages(ctx context.Context, sessionID string) error {
	_, err := s.pool.Exec(ctx,
		`DELETE FROM fix_messages WHERE session_id = $1`, sessionID)
	if err != nil {
		return fmt.Errorf("purge fix_messages %q: %w", sessionID, err)
	}
	return nil
}

// VerifyAPIKey authenticates the logon credential pair (Username 553 =
// key name e.g. "sub_xXx", Password 554 = secret). The api_keys row is
// keyed by its numeric id column bound on fix_sessions.api_key_id —
// the key name + secret must match THAT row (key_id text match +
// sha256(key_id.secret) == key_hash + status ACTIVE). Returns the
// key's account_id on success.
func (s *PgStore) VerifyAPIKey(ctx context.Context, keyRowID int64,
	keyName, secret string) (int64, bool, error) {
	var (
		storedKeyID string
		keyHash     string
		accountID   int64
		status      string
	)
	err := s.pool.QueryRow(ctx,
		`SELECT key_id, key_hash, account_id, status::text
		   FROM api_keys WHERE id = $1`, keyRowID).
		Scan(&storedKeyID, &keyHash, &accountID, &status)
	if isNoRows(err) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, fmt.Errorf("api_key lookup %d: %w", keyRowID, err)
	}
	if status != "ACTIVE" || storedKeyID != keyName {
		return 0, false, nil
	}
	sum := sha256.Sum256([]byte(keyName + "." + secret))
	if subtle.ConstantTimeCompare([]byte(hex.EncodeToString(sum[:])), []byte(keyHash)) != 1 {
		return 0, false, nil
	}
	return accountID, true, nil
}

func (s *PgStore) UpdateEntitlement(ctx context.Context, sessionID string,
	u EntitlementUpdate) (*Session, error) {
	// Read-modify-write under one row lock so concurrent admin updates
	// never clobber each other's fields.
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("entitlement update tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	cur, err := scanSession(tx.QueryRow(ctx,
		`SELECT `+sessionCols+` FROM fix_sessions WHERE session_id = $1 FOR UPDATE`,
		sessionID))
	if isNoRows(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("entitlement update read %q: %w", sessionID, err)
	}

	next := *cur
	switch {
	case u.ClearAccount:
		next.AccountID = nil
	case u.AccountID != nil:
		next.AccountID = u.AccountID
	}
	switch {
	case u.ClearAPIKey:
		next.APIKeyID = nil
	case u.APIKeyID != nil:
		next.APIKeyID = u.APIKeyID
	}
	switch {
	case u.SetAllInstruments:
		next.AllowedInstruments = ""
	case u.AllowedInstruments != nil:
		next.AllowedInstruments = *u.AllowedInstruments
	}
	if u.CancelOnDisconnect != nil {
		next.CancelOnDisconnect = *u.CancelOnDisconnect
	}
	if u.MaxMsgsPerSec != nil {
		next.MaxMsgsPerSec = *u.MaxMsgsPerSec
	}

	row, err := scanSession(tx.QueryRow(ctx, `
		UPDATE fix_sessions
		   SET account_id = $2,
		       api_key_id = $3,
		       allowed_instruments = $4,
		       cancel_on_disconnect = $5,
		       max_msgs_per_sec = $6,
		       updated_at = now()
		 WHERE session_id = $1
		 RETURNING `+sessionCols,
		sessionID, next.AccountID, next.APIKeyID,
		nullStr(next.AllowedInstruments), next.CancelOnDisconnect,
		next.MaxMsgsPerSec))
	if err != nil {
		return nil, fmt.Errorf("entitlement update write %q: %w", sessionID, err)
	}
	return row, tx.Commit(ctx)
}
