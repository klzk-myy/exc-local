// comms_recording.go — Phase-21 Task 21.3.20: MiFID II Art. 16(7) /
// RTS 6 communications recording ("taping").
//
// Pipeline: DB capture triggers (migration 062) write
// comms_capture_outbox rows inside the source transaction — a
// client-facing communication can never exist without a pending
// recording obligation. Drain() claims outbox rows, writes the content
// to the WORM object store (S3 Object Lock COMPLIANCE + SSE-KMS), and
// appends the register row hash-chained per UTC day. Record() is the
// direct in-process seam for channels without a DB trigger (in-app
// chat, future transports).
//
// Integrity model:
//
//	sha256          = SHA-256 of the stored object bytes (canonical
//	                  JSON envelope — deterministic, key-sorted);
//	chain_hash      = SHA-256("COMMS1|" recording_id "|" account_id "|"
//	                  user_id "|" channel "|" direction "|" source "|"
//	                  source_id "|" started_at_unixmicro "|"
//	                  ended_at_unixmicro "|" content_ref "|" sha256 "|"
//	                  retention_until_unixmicro "|" prev_chain_hash)
//	prev_chain_hash = chain_hash of the day's previous register row;
//	                  the day's first row anchors to the shared genesis
//	                  SHA-256("") (audit.GenesisPrevHash).
//
// Fail-closed (spec §2.7): a retrieval whose recomputed sha256 or day
// chain disagrees with the register refuses the read with
// COMMS_INTEGRITY_FAILURE; deletion before retention_until is refused
// by the service AND by the row guard trigger; a recording without a
// resolvable object Put fails the drain item, not silently records.
//
// GDPR Art. 17(3)(b): recordings survive erasure — the Task 21.3.7
// erasure manifest lists comms_recordings under retained carve-outs.
package compliance

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"exchange/internal/audit"
	"exchange/internal/objectstore"
	excerrors "exchange/pkg/errors"
)

// CommsRetentionFloor is the MiFID II Art. 16(7) minimum — five years.
const CommsRetentionFloor = 5 * 365 * 24 * time.Hour

// Recording is the register row.
type Recording struct {
	RecordingID    int64     `json:"recording_id"`
	AccountID      *int64    `json:"account_id,omitempty"`
	UserID         *int64    `json:"user_id,omitempty"`
	Channel        string    `json:"channel"`
	Direction      string    `json:"direction"`
	Source         string    `json:"source"`
	SourceID       int64     `json:"source_id"`
	StartedAt      time.Time `json:"started_at"`
	EndedAt        time.Time `json:"ended_at"`
	ContentRef     string    `json:"content_ref"`
	SHA256         string    `json:"sha256"`
	SizeBytes      int64     `json:"size_bytes"`
	StorageClass   string    `json:"storage_class"`
	DayBucket      time.Time `json:"day_bucket"`
	PrevChainHash  string    `json:"prev_chain_hash"`
	ChainHash      string    `json:"chain_hash"`
	RetentionUntil time.Time `json:"retention_until"`
	Sealed         bool      `json:"sealed"`
	CreatedAt      time.Time `json:"created_at"`
}

// RecordInput is the direct-capture seam (channels that bypass the DB
// trigger outbox — e.g. in-app chat, FIX drop-copy adapters).
type RecordInput struct {
	AccountID *int64
	UserID    *int64
	Channel   string // EMAIL|SMS|PUSH|WS|IN_APP_CHAT|SUPPORT_MESSAGE|OTHER
	Direction string // INBOUND|OUTBOUND|INTERNAL
	Source    string // logical origin tag, e.g. "in_app_chat"
	SourceID  int64  // caller's idempotency key — collides loudly on reuse
	StartedAt time.Time
	EndedAt   time.Time
	Body      []byte // content bytes to tape (stored verbatim)
	// RetentionUntil overrides the 5y default when a competent-authority
	// extension applies; shorter-than-floor values are rejected.
	RetentionUntil *time.Time
}

// captureEnvelope is the canonical content document hashed + stored.
// Struct (not map) so encoding/json field order is deterministic.
type captureEnvelope struct {
	Format     string          `json:"format"` // "comms-tape/v1"
	Source     string          `json:"source"`
	SourceID   int64           `json:"source_id"`
	AccountID  *int64          `json:"account_id,omitempty"`
	UserID     *int64          `json:"user_id,omitempty"`
	Channel    string          `json:"channel"`
	Direction  string          `json:"direction"`
	HappenedAt time.Time       `json:"happened_at"`
	Payload    json.RawMessage `json:"payload"`
	Body       []byte          `json:"body,omitempty"` // raw bytes for non-JSON captures
}

// CommsRecordingService owns the register.
type CommsRecordingService struct {
	pool      *pgxpool.Pool
	objects   objectstore.Client // WORM bucket
	kmsKey    string             // SSE-KMS key id ("" = bucket default)
	resolver  HoldRoleResolver
	now       func() time.Time
	retention time.Duration
}

// NewCommsRecordingService binds dependencies; objects and resolver are
// mandatory — a recorder without WORM storage cannot honour the
// contract, and retrieval without a role resolver cannot dual-control.
func NewCommsRecordingService(pool *pgxpool.Pool, objects objectstore.Client,
	kmsKey string, resolver HoldRoleResolver) (*CommsRecordingService, error) {
	if pool == nil || objects == nil || resolver == nil {
		return nil, fmt.Errorf("compliance: comms recorder requires pool, objects, resolver")
	}
	return &CommsRecordingService{
		pool: pool, objects: objects, kmsKey: kmsKey, resolver: resolver,
		now: time.Now, retention: CommsRetentionFloor,
	}, nil
}

// SetClockForTest overrides the clock; tests only.
func (s *CommsRecordingService) SetClockForTest(now func() time.Time) {
	s.now = now
}

// SetRetentionForTest shrinks the floor; tests only.
func (s *CommsRecordingService) SetRetentionForTest(d time.Duration) {
	s.retention = d
}

// ---------------------------------------------------------------------------
// Recording write path
// ---------------------------------------------------------------------------

// chainHash recomputes the register hash for a row.
func commsChainHash(rec *Recording) string {
	acct := ""
	if rec.AccountID != nil {
		acct = strconv.FormatInt(*rec.AccountID, 10)
	}
	uid := ""
	if rec.UserID != nil {
		uid = strconv.FormatInt(*rec.UserID, 10)
	}
	h := sha256.New()
	fmt.Fprintf(h, "COMMS1|%d|%s|%s|%s|%s|%s|%d|%d|%d|%s|%s|%d|%s",
		rec.RecordingID, acct, uid, rec.Channel, rec.Direction,
		rec.Source, rec.SourceID,
		rec.StartedAt.UTC().UnixMicro(), rec.EndedAt.UTC().UnixMicro(),
		rec.ContentRef, rec.SHA256,
		rec.RetentionUntil.UTC().UnixMicro(), rec.PrevChainHash)
	return fmt.Sprintf("%x", h.Sum(nil))
}

// retentionFloor computes the earliest legal deletion instant.
func (s *CommsRecordingService) retentionFloor(ended time.Time) time.Time {
	return ended.UTC().Add(s.retention)
}

// Record is the direct-capture API — synchronous: WORM Put then the
// register append, both fail-closed.
func (s *CommsRecordingService) Record(ctx context.Context, in RecordInput) (*Recording, error) {
	env := captureEnvelope{
		Format: "comms-tape/v1", Source: in.Source, SourceID: in.SourceID,
		AccountID: in.AccountID, UserID: in.UserID,
		Channel: normalizeChannel(in.Channel), Direction: in.Direction,
		HappenedAt: in.EndedAt.UTC(), Body: in.Body,
	}
	body, err := json.Marshal(env)
	if err != nil {
		return nil, fmt.Errorf("comms: envelope marshal: %w", err)
	}
	return s.appendRecording(ctx, appendArgs{
		AccountID: in.AccountID, UserID: in.UserID,
		Channel: env.Channel, Direction: env.Direction,
		Source: in.Source, SourceID: in.SourceID,
		StartedAt: in.StartedAt, EndedAt: in.EndedAt,
		RetentionUntil: in.RetentionUntil,
		Body:           body,
	})
}

type appendArgs struct {
	AccountID      *int64
	UserID         *int64
	Channel        string
	Direction      string
	Source         string
	SourceID       int64
	StartedAt      time.Time
	EndedAt        time.Time
	RetentionUntil *time.Time
	Body           []byte // canonical stored bytes
}

// appendRecording is the single write path: object first (fail before
// register), then the chain-append tx with a per-day advisory lock.
func (s *CommsRecordingService) appendRecording(ctx context.Context, a appendArgs) (*Recording, error) {
	if a.EndedAt.Before(a.StartedAt) {
		return nil, excerrors.New("INVALID_REQUEST",
			"ended_at precedes started_at")
	}
	retention := s.retentionFloor(a.EndedAt)
	if a.RetentionUntil != nil {
		if a.RetentionUntil.Before(retention) {
			return nil, excerrors.New("INVALID_REQUEST",
				"retention_until below the 5-year MiFID II floor")
		}
		retention = a.RetentionUntil.UTC()
	}
	sum := fmt.Sprintf("%x", sha256.Sum256(a.Body))
	day := a.StartedAt.UTC().Truncate(24 * time.Hour)
	dayStr := day.Format("2006-01-02") // DATE param — avoids session-TZ casts

	// WORM write — COMPLIANCE lock until retention. Object-first so a
	// storage failure never leaves a register row pointing at nothing.
	key := fmt.Sprintf("comms/%s/%s-%d-%s.json",
		day.Format("2006-01-02"), a.Source, a.SourceID, sum[:16])
	obj, err := s.objects.Put(ctx, objectstore.PutInput{
		Key: key, Body: bytes.NewReader(a.Body), Size: int64(len(a.Body)),
		ContentType:           "application/json",
		ObjectLockMode:        "COMPLIANCE",
		ObjectLockRetainUntil: retention,
		ServerSideEncryption:  "aws:kms",
		SSEKMSKeyID:           s.kmsKey,
	})
	if err != nil {
		return nil, excerrors.Wrap("SERVICE_DEGRADED",
			"comms WORM write failed", err)
	}
	if obj.ObjectLockMode != "" && obj.ObjectLockMode != "COMPLIANCE" &&
		obj.ObjectLockMode != "GOVERNANCE" {
		return nil, excerrors.New("INTERNAL_ERROR",
			"comms store rejected object-lock mode "+obj.ObjectLockMode)
	}

	rec := &Recording{
		AccountID: a.AccountID, UserID: a.UserID,
		Channel: a.Channel, Direction: a.Direction,
		Source: a.Source, SourceID: a.SourceID,
		StartedAt: a.StartedAt.UTC(), EndedAt: a.EndedAt.UTC(),
		ContentRef: key, SHA256: sum, SizeBytes: int64(len(a.Body)),
		StorageClass: "WORM", DayBucket: day,
		RetentionUntil: retention, Sealed: s.kmsKey != "",
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("comms: record tx: %w", err)
	}
	defer tx.Rollback(ctx)

	// Serialize the day's chain append — concurrent recorders must
	// agree on the tip.
	if _, err := tx.Exec(ctx,
		`SELECT pg_advisory_xact_lock(hashtext('comms-recordings-day'), hashtext($1))`,
		dayStr); err != nil {
		return nil, fmt.Errorf("comms: chain lock: %w", err)
	}
	var prev string
	err = tx.QueryRow(ctx, `
		SELECT chain_hash FROM comms_recordings
		 WHERE day_bucket=$1::date ORDER BY recording_id DESC LIMIT 1`, dayStr).
		Scan(&prev)
	if err == pgx.ErrNoRows {
		prev = audit.GenesisPrevHash
	} else if err != nil {
		return nil, fmt.Errorf("comms: chain tip: %w", err)
	}
	rec.PrevChainHash = prev

	// Reserve the PK first — chain_hash covers recording_id and the
	// register's WORM guard forbids any post-insert UPDATE, so the id
	// must be known before hashing.
	if err := tx.QueryRow(ctx,
		`SELECT nextval(pg_get_serial_sequence('comms_recordings','recording_id'))`).
		Scan(&rec.RecordingID); err != nil {
		return nil, fmt.Errorf("comms: id reserve: %w", err)
	}
	rec.ChainHash = commsChainHash(rec)

	err = tx.QueryRow(ctx, `
		INSERT INTO comms_recordings
		    (recording_id, account_id, user_id, channel, direction,
		     source, source_id, started_at, ended_at, content_ref,
		     sha256, size_bytes, storage_class, day_bucket,
		     prev_chain_hash, chain_hash, retention_until, sealed)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,'WORM',$13,$14,$15,$16,$17)
		RETURNING created_at`,
		rec.RecordingID, rec.AccountID, rec.UserID, rec.Channel,
		rec.Direction, rec.Source, rec.SourceID, rec.StartedAt,
		rec.EndedAt, rec.ContentRef, rec.SHA256, rec.SizeBytes,
		dayStr, rec.PrevChainHash, rec.ChainHash,
		rec.RetentionUntil, rec.Sealed).
		Scan(&rec.CreatedAt)
	if err != nil {
		if isUniqueViolationErr(err) {
			// Idempotent re-capture: the register row already exists.
			var existing Recording
			serr := tx.QueryRow(ctx, `
				SELECT recording_id, chain_hash FROM comms_recordings
				 WHERE source=$1 AND source_id=$2`, a.Source, a.SourceID).
				Scan(&existing.RecordingID, &existing.ChainHash)
			if serr == nil {
				rec.RecordingID = existing.RecordingID
				rec.ChainHash = existing.ChainHash
				return rec, tx.Commit(ctx)
			}
		}
		return nil, fmt.Errorf("comms: register insert: %w", err)
	}
	if _, err := audit.Append(ctx, tx, "comms_recordings",
		&rec.RecordingID, "RECORDED", nil); err != nil {
		return nil, fmt.Errorf("comms: record audit: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("comms: record commit: %w", err)
	}
	return rec, nil
}

// ---------------------------------------------------------------------------
// Outbox drain (trigger-fed capture)
// ---------------------------------------------------------------------------

// Drain claims pending outbox rows and records them. Returns the number
// recorded; individual failures abort the batch (fail-closed — a dead
// row retries next sweep because recorded_id stays NULL).
func (s *CommsRecordingService) Drain(ctx context.Context, limit int) (int, error) {
	if limit <= 0 {
		limit = 100
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, fmt.Errorf("comms: drain tx: %w", err)
	}
	rows, err := tx.Query(ctx, `
		SELECT id, source, source_id, account_id, user_id, channel,
		       direction, happened_at, payload
		  FROM comms_capture_outbox
		 WHERE recorded_id IS NULL
		 ORDER BY id LIMIT $1
		 FOR UPDATE SKIP LOCKED`, limit)
	if err != nil {
		tx.Rollback(ctx)
		return 0, fmt.Errorf("comms: drain claim: %w", err)
	}
	type outboxRow struct {
		id, sourceID int64
		source       string
		acct, user   *int64
		channel, dir string
		happened     time.Time
		payload      []byte
	}
	var batch []outboxRow
	for rows.Next() {
		var r outboxRow
		if err := rows.Scan(&r.id, &r.source, &r.sourceID, &r.acct,
			&r.user, &r.channel, &r.dir, &r.happened, &r.payload); err != nil {
			rows.Close()
			tx.Rollback(ctx)
			return 0, fmt.Errorf("comms: drain scan: %w", err)
		}
		batch = append(batch, r)
	}
	rows.Close()
	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("comms: drain claim commit: %w", err)
	}

	n := 0
	for _, r := range batch {
		acct := r.acct
		if acct == nil && r.user != nil {
			acct = s.masterAccountFor(ctx, *r.user)
		}
		env := captureEnvelope{
			Format: "comms-tape/v1", Source: r.source, SourceID: r.sourceID,
			AccountID: acct, UserID: r.user,
			Channel: normalizeChannel(r.channel), Direction: r.dir,
			HappenedAt: r.happened.UTC(),
			Payload:    json.RawMessage(r.payload),
		}
		body, err := json.Marshal(env)
		if err != nil {
			return n, fmt.Errorf("comms: drain envelope %d: %w", r.id, err)
		}
		rec, err := s.appendRecording(ctx, appendArgs{
			AccountID: acct, UserID: r.user, Channel: env.Channel,
			Direction: env.Direction, Source: r.source, SourceID: r.sourceID,
			StartedAt: r.happened, EndedAt: r.happened, Body: body,
		})
		if err != nil {
			return n, fmt.Errorf("comms: drain record %s/%d: %w",
				r.source, r.sourceID, err)
		}
		if _, err := s.pool.Exec(ctx, `
			UPDATE comms_capture_outbox SET recorded_id=$2, claimed_at=now()
			 WHERE id=$1`, r.id, rec.RecordingID); err != nil {
			return n, fmt.Errorf("comms: drain mark %d: %w", r.id, err)
		}
		n++
	}
	return n, nil
}

// masterAccountFor resolves a user's primary account (the parent-less
// account row; user-level sources need an account_id for evidence
// queries).
func (s *CommsRecordingService) masterAccountFor(ctx context.Context, userID int64) *int64 {
	var id int64
	err := s.pool.QueryRow(ctx, `
		SELECT id FROM accounts
		 WHERE user_id=$1 AND parent_account_id IS NULL
		 ORDER BY id LIMIT 1`, userID).Scan(&id)
	if err != nil {
		return nil
	}
	return &id
}

// ---------------------------------------------------------------------------
// Retrieval — dual control + audit + integrity
// ---------------------------------------------------------------------------

// RetrievedRecording is the verified content + register view.
type RetrievedRecording struct {
	Recording *Recording      `json:"recording"`
	Body      json.RawMessage `json:"body"`
}

// Retrieve implements the dual-control evidence read: both officer and
// approver must resolve to Compliance Officer or Super Admin, approver
// ≠ officer (DUAL_CONTROL_VIOLATION), justification is mandatory, and
// the access row + audit chain append land before the object is read.
// The stored sha256 and the day's chain are both re-verified.
func (s *CommsRecordingService) Retrieve(ctx context.Context, recordingID int64,
	officer, approver int64, justification, caseRef string) (*RetrievedRecording, error) {

	if officer == approver {
		return nil, excerrors.New("DUAL_CONTROL_VIOLATION",
			"approver must differ from the requesting officer")
	}
	if strings.TrimSpace(justification) == "" {
		return nil, excerrors.New("INVALID_REQUEST",
			"retrieval justification is required")
	}
	for _, uid := range []int64{officer, approver} {
		if err := s.checkCommsRole(ctx, uid); err != nil {
			return nil, err
		}
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("comms: retrieve tx: %w", err)
	}
	defer tx.Rollback(ctx)

	rec, err := s.loadRecordingTx(ctx, tx, recordingID)
	if err != nil {
		return nil, err
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO comms_recording_access
		    (recording_id, accessed_by, approved_by, action, justification, case_ref)
		VALUES ($1,$2,$3,'RETRIEVE',$4,$5)`,
		recordingID, officer, approver, justification, nilIfEmpty(caseRef)); err != nil {
		return nil, fmt.Errorf("comms: access log: %w", err)
	}
	if _, err := audit.Append(ctx, tx, "comms_recordings",
		&rec.RecordingID, "RETRIEVED", nil); err != nil {
		return nil, fmt.Errorf("comms: retrieve audit: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("comms: retrieve commit: %w", err)
	}

	// Post-commit object read + integrity verification.
	body, _, err := s.objects.Get(ctx, rec.ContentRef)
	if err != nil {
		return nil, excerrors.Wrap("SERVICE_DEGRADED",
			"comms object read failed", err)
	}
	if fmt.Sprintf("%x", sha256.Sum256(body)) != rec.SHA256 {
		return nil, excerrors.New("COMMS_INTEGRITY_FAILURE",
			fmt.Sprintf("recording %d object sha256 mismatch", recordingID))
	}
	if ok, err := s.VerifyDay(ctx, rec.DayBucket); err != nil {
		return nil, err
	} else if !ok {
		return nil, excerrors.New("COMMS_INTEGRITY_FAILURE",
			fmt.Sprintf("recording %d day chain broken (%s)",
				recordingID, rec.DayBucket.Format("2006-01-02")))
	}
	return &RetrievedRecording{
		Recording: rec, Body: json.RawMessage(body),
	}, nil
}

// loadRecordingTx reads the register row FOR UPDATE (the tx pins the
// row while the access log writes).
func (s *CommsRecordingService) loadRecordingTx(ctx context.Context, tx pgx.Tx,
	id int64) (*Recording, error) {
	var r Recording
	err := tx.QueryRow(ctx, `
		SELECT recording_id, account_id, user_id, channel, direction,
		       source, source_id, started_at, ended_at, content_ref,
		       sha256, size_bytes, storage_class, day_bucket,
		       prev_chain_hash, chain_hash, retention_until, sealed,
		       created_at
		  FROM comms_recordings WHERE recording_id=$1 FOR UPDATE`, id).
		Scan(&r.RecordingID, &r.AccountID, &r.UserID, &r.Channel,
			&r.Direction, &r.Source, &r.SourceID, &r.StartedAt, &r.EndedAt,
			&r.ContentRef, &r.SHA256, &r.SizeBytes, &r.StorageClass,
			&r.DayBucket, &r.PrevChainHash, &r.ChainHash,
			&r.RetentionUntil, &r.Sealed, &r.CreatedAt)
	if err == pgx.ErrNoRows {
		return nil, excerrors.New("NOT_FOUND",
			fmt.Sprintf("recording %d not found", id))
	}
	if err != nil {
		return nil, fmt.Errorf("comms: recording load: %w", err)
	}
	return &r, nil
}

// VerifyDay recomputes one day partition's chain — used by retrieval
// and by periodic integrity sweeps. Returns (true, nil) when the chain
// recomputes end-to-end.
func (s *CommsRecordingService) VerifyDay(ctx context.Context, day time.Time) (bool, error) {
	dayStr := day.UTC().Format("2006-01-02")
	rows, err := s.pool.Query(ctx, `
		SELECT recording_id, account_id, user_id, channel, direction,
		       source, source_id, started_at, ended_at, content_ref,
		       sha256, size_bytes, storage_class, day_bucket,
		       prev_chain_hash, chain_hash, retention_until, sealed,
		       created_at
		  FROM comms_recordings WHERE day_bucket=$1::date
		 ORDER BY recording_id`, dayStr)
	if err != nil {
		return false, fmt.Errorf("comms: verify day read: %w", err)
	}
	defer rows.Close()
	prev := audit.GenesisPrevHash
	for rows.Next() {
		var r Recording
		if err := rows.Scan(&r.RecordingID, &r.AccountID, &r.UserID,
			&r.Channel, &r.Direction, &r.Source, &r.SourceID,
			&r.StartedAt, &r.EndedAt, &r.ContentRef, &r.SHA256,
			&r.SizeBytes, &r.StorageClass, &r.DayBucket,
			&r.PrevChainHash, &r.ChainHash, &r.RetentionUntil,
			&r.Sealed, &r.CreatedAt); err != nil {
			return false, fmt.Errorf("comms: verify day scan: %w", err)
		}
		if r.PrevChainHash != prev || commsChainHash(&r) != r.ChainHash {
			return false, nil
		}
		prev = r.ChainHash
	}
	return true, rows.Err()
}

// Delete is the post-retention legal purge. Pre-expiry deletion refuses
// (service check + DB trigger belt-and-suspenders).
func (s *CommsRecordingService) Delete(ctx context.Context, recordingID int64,
	officer int64) error {
	if err := s.checkCommsRole(ctx, officer); err != nil {
		return err
	}
	var until time.Time
	var ref string
	err := s.pool.QueryRow(ctx, `
		SELECT retention_until, content_ref FROM comms_recordings
		 WHERE recording_id=$1`, recordingID).Scan(&until, &ref)
	if err == pgx.ErrNoRows {
		return excerrors.New("NOT_FOUND", "recording not found")
	}
	if err != nil {
		return fmt.Errorf("comms: retention read: %w", err)
	}
	if until.After(s.now().UTC()) {
		return excerrors.New("COMMS_RETENTION_ACTIVE",
			fmt.Sprintf("recording %d under retention until %s",
				recordingID, until.Format(time.RFC3339)))
	}
	if err := s.objects.Delete(ctx, ref); err != nil && !objectstore.IsNotFound(err) {
		return fmt.Errorf("comms: object delete: %w", err)
	}
	if _, err := s.pool.Exec(ctx,
		`DELETE FROM comms_recordings WHERE recording_id=$1`, recordingID); err != nil {
		return fmt.Errorf("comms: register delete: %w", err)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Read surface (admin)
// ---------------------------------------------------------------------------

// List returns register rows newest-first (metadata only — content is
// Retrieve-gated).
func (s *CommsRecordingService) List(ctx context.Context, accountID *int64,
	limit int) ([]Recording, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	var (
		rows pgx.Rows
		err  error
	)
	if accountID != nil {
		rows, err = s.pool.Query(ctx, `
			SELECT recording_id, account_id, user_id, channel, direction,
			       source, source_id, started_at, ended_at, content_ref,
			       sha256, size_bytes, storage_class, day_bucket,
			       prev_chain_hash, chain_hash, retention_until, sealed,
			       created_at
			  FROM comms_recordings WHERE account_id=$1
			 ORDER BY recording_id DESC LIMIT $2`, *accountID, limit)
	} else {
		rows, err = s.pool.Query(ctx, `
			SELECT recording_id, account_id, user_id, channel, direction,
			       source, source_id, started_at, ended_at, content_ref,
			       sha256, size_bytes, storage_class, day_bucket,
			       prev_chain_hash, chain_hash, retention_until, sealed,
			       created_at
			  FROM comms_recordings
			 ORDER BY recording_id DESC LIMIT $1`, limit)
	}
	if err != nil {
		return nil, fmt.Errorf("comms: list: %w", err)
	}
	defer rows.Close()
	out := []Recording{}
	for rows.Next() {
		var r Recording
		if err := rows.Scan(&r.RecordingID, &r.AccountID, &r.UserID,
			&r.Channel, &r.Direction, &r.Source, &r.SourceID,
			&r.StartedAt, &r.EndedAt, &r.ContentRef, &r.SHA256,
			&r.SizeBytes, &r.StorageClass, &r.DayBucket,
			&r.PrevChainHash, &r.ChainHash, &r.RetentionUntil,
			&r.Sealed, &r.CreatedAt); err != nil {
			return nil, fmt.Errorf("comms: list scan: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// Get returns one register row (metadata only).
func (s *CommsRecordingService) Get(ctx context.Context, recordingID int64) (*Recording, error) {
	var r Recording
	err := s.pool.QueryRow(ctx, `
		SELECT recording_id, account_id, user_id, channel, direction,
		       source, source_id, started_at, ended_at, content_ref,
		       sha256, size_bytes, storage_class, day_bucket,
		       prev_chain_hash, chain_hash, retention_until, sealed,
		       created_at
		  FROM comms_recordings WHERE recording_id=$1`, recordingID).
		Scan(&r.RecordingID, &r.AccountID, &r.UserID, &r.Channel,
			&r.Direction, &r.Source, &r.SourceID, &r.StartedAt, &r.EndedAt,
			&r.ContentRef, &r.SHA256, &r.SizeBytes, &r.StorageClass,
			&r.DayBucket, &r.PrevChainHash, &r.ChainHash,
			&r.RetentionUntil, &r.Sealed, &r.CreatedAt)
	if err == pgx.ErrNoRows {
		return nil, excerrors.New("NOT_FOUND", "recording not found")
	}
	if err != nil {
		return nil, fmt.Errorf("comms: get: %w", err)
	}
	return &r, nil
}

// AttachToCase links a recording to a surveillance case (Task 21.3.21
// seam) — records an ATTACH access row; the case store itself owns the
// evidence blob reference.
func (s *CommsRecordingService) AttachToCase(ctx context.Context,
	recordingID int64, officer int64, caseRef string) error {
	if err := s.checkCommsRole(ctx, officer); err != nil {
		return err
	}
	if strings.TrimSpace(caseRef) == "" {
		return excerrors.New("INVALID_REQUEST", "case_ref is required")
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, err := s.loadRecordingTx(ctx, tx, recordingID); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO comms_recording_access
		    (recording_id, accessed_by, action, justification, case_ref)
		VALUES ($1,$2,'ATTACH','surveillance evidence attach',$3)`,
		recordingID, officer, caseRef); err != nil {
		return fmt.Errorf("comms: attach log: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return err
	}
	return nil
}

func (s *CommsRecordingService) checkCommsRole(ctx context.Context, userID int64) error {
	role, err := s.resolver(ctx, userID)
	if err != nil {
		return excerrors.New("INTERNAL_ERROR", "role lookup: "+err.Error())
	}
	if role != "Compliance Officer" && role != "Super Admin" {
		return excerrors.New("UNAUTHORIZED_ROLE",
			"role "+role+" cannot access communications recordings")
	}
	return nil
}

// normalizeChannel upper-cases the persisted channel vocabulary.
func normalizeChannel(c string) string {
	return strings.ToUpper(strings.TrimSpace(c))
}

// isUniqueViolationErr reports PG 23505 (renamed — the regreporting
// dispatcher owns the package-level isUniqueViolation name).
func isUniqueViolationErr(err error) bool {
	return err != nil && strings.Contains(err.Error(), "23505")
}
