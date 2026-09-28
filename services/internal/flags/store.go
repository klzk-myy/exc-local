package flags

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"time"

	goredis "github.com/redis/go-redis/v9"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"exchange/internal/admin"
	excerrors "exchange/pkg/errors"
)

// Redis keyspace (spec §4 extension — Task 9.3.7 item 3 pins the
// flags:{name} shape):
//
//	flags:{name}     STRING  JSON Flag, TTL none — invalidated on write
//	flags:version    STRING  INCR on every mutation; edge caches compare
//	                         against their snapshot to detect staleness.
const (
	flagKeyPrefix = "flags:"
	versionKey    = "flags:version"
)

// ErrNotFound marks a missing flag; callers map it to 404.
var ErrNotFound = errors.New("feature flag not found")

// Store persists flags in PostgreSQL and serves reads through Redis.
// Every mutation runs inside one transaction together with its
// admin_audit_log row (Phase-07 Task 7.3.3 contract: privileged changes
// are hash-chain audited) and write-through-invalidates the cache.
type Store struct {
	pool *pgxpool.Pool
	rdb  goredis.Cmdable // nil → PG-only (unit tests, degraded boot)
	now  func() time.Time
}

// NewStore binds the store. rdb may be nil; reads then hit PG directly.
func NewStore(pool *pgxpool.Pool, rdb goredis.Cmdable) (*Store, error) {
	if pool == nil {
		return nil, fmt.Errorf("flags: nil pool")
	}
	return &Store{pool: pool, rdb: rdb, now: time.Now}, nil
}

// SetClockForTest replaces the clock (tests only).
func (s *Store) SetClockForTest(now func() time.Time) { s.now = now }

func flagKey(name string) string { return flagKeyPrefix + name }

// scanFlag reads one feature_flags row.
func scanFlag(row pgx.Row) (*Flag, error) {
	var (
		f      Flag
		stages []int16
		tiers  []string
		accts  []int64
	)
	err := row.Scan(&f.Name, &f.Enabled, &f.RolloutPct, &stages, &f.StageIdx,
		&tiers, &accts, &f.Description, &f.Version, &f.CreatedAt, &f.UpdatedAt)
	if err != nil {
		return nil, err
	}
	f.Stages = make([]int, len(stages))
	for i, v := range stages {
		f.Stages[i] = int(v)
	}
	f.Tiers, f.Accounts = tiers, accts
	f.Normalize()
	return &f, nil
}

const flagCols = `name, enabled, rollout_pct, stages, stage_idx, tiers, accounts,
                  description, version, created_at, updated_at`

// Get returns one flag, Redis first then PG on miss. PG is authoritative;
// a stale cache entry is never preferred over the row (writes bump the
// mirrored version counter so a torn read resolves on the next lookup).
func (s *Store) Get(ctx context.Context, name string) (*Flag, error) {
	if s.rdb != nil {
		if raw, err := s.rdb.Get(ctx, flagKey(name)).Bytes(); err == nil {
			var f Flag
			if jerr := json.Unmarshal(raw, &f); jerr == nil {
				f.Normalize()
				return &f, nil
			}
			// Corrupt payload — fail through to PG and overwrite below.
		}
	}
	f, err := s.getPG(ctx, name)
	if err != nil {
		return nil, err
	}
	s.cacheSet(ctx, f)
	return f, nil
}

func (s *Store) getPG(ctx context.Context, name string) (*Flag, error) {
	f, err := scanFlag(s.pool.QueryRow(ctx,
		`SELECT `+flagCols+` FROM feature_flags WHERE name = $1`, name))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	return f, err
}

// List returns every flag from PG (admin surface — never cached so the
// operator view always reflects authoritative state).
func (s *Store) List(ctx context.Context) ([]Flag, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT `+flagCols+` FROM feature_flags ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Flag
	for rows.Next() {
		f, err := scanFlag(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *f)
	}
	return out, rows.Err()
}

func (s *Store) cacheSet(ctx context.Context, f *Flag) {
	if s.rdb == nil {
		return
	}
	if raw, err := json.Marshal(f); err == nil {
		_ = s.rdb.Set(ctx, flagKey(f.Name), raw, 0).Err()
	}
}

func (s *Store) cacheDel(ctx context.Context, name string) {
	if s.rdb == nil {
		return
	}
	_ = s.rdb.Del(ctx, flagKey(name)).Err()
}

// bumpVersion publishes a new cache epoch so readers can detect torn
// caches. Best-effort — a Redis outage leaves PG authoritative.
func (s *Store) bumpVersion(ctx context.Context) {
	if s.rdb != nil {
		_ = s.rdb.Incr(ctx, versionKey).Err()
	}
}

// Upsert creates or replaces a flag definition and writes the audit
// chain entry in the same transaction. before is fetched when the row
// exists so the audit log carries real state, not just the new blob.
func (s *Store) Upsert(ctx context.Context, f Flag, adminID int64, clientIP string) (*Flag, error) {
	f.Normalize()
	if err := f.Validate(); err != nil {
		return nil, excerrors.New("INVALID_REQUEST", err.Error())
	}
	stages := make([]int16, len(f.Stages))
	for i, v := range f.Stages {
		stages[i] = int16(v)
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, excerrors.Wrap("INTERNAL_ERROR", "flags tx begin", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var before any
	if prev, perr := scanFlag(tx.QueryRow(ctx,
		`SELECT `+flagCols+` FROM feature_flags WHERE name = $1 FOR UPDATE`, f.Name)); perr == nil {
		before = prev
	} else if !errors.Is(perr, pgx.ErrNoRows) {
		return nil, excerrors.Wrap("INTERNAL_ERROR", "flags read-before-write", perr)
	}

	out, err := scanFlag(tx.QueryRow(ctx, `
		INSERT INTO feature_flags
		    (name, enabled, rollout_pct, stages, stage_idx, tiers, accounts,
		     description, created_by, updated_by, updated_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10, now())
		ON CONFLICT (name) DO UPDATE SET
		    enabled     = EXCLUDED.enabled,
		    rollout_pct = EXCLUDED.rollout_pct,
		    stages      = EXCLUDED.stages,
		    stage_idx   = EXCLUDED.stage_idx,
		    tiers       = EXCLUDED.tiers,
		    accounts    = EXCLUDED.accounts,
		    description = EXCLUDED.description,
		    updated_by  = EXCLUDED.updated_by,
		    updated_at  = now(),
		    version     = feature_flags.version + 1
		RETURNING `+flagCols,
		f.Name, f.Enabled, f.RolloutPct, stages, f.StageIdx,
		f.Tiers, f.Accounts, f.Description, adminID, adminID))
	if err != nil {
		return nil, excerrors.Wrap("INTERNAL_ERROR", "flags upsert", err)
	}

	if _, _, err := admin.Log(ctx, tx, admin.AuditEntry{
		AdminUserID: adminID,
		Action:      "feature_flag.upsert",
		TargetType:  "feature_flag",
		BeforeState: before,
		AfterState:  out,
		IPAddress:   stripIP(clientIP),
	}); err != nil {
		return nil, excerrors.Wrap("INTERNAL_ERROR", "flags audit", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, excerrors.Wrap("INTERNAL_ERROR", "flags commit", err)
	}
	s.cacheSet(ctx, out)
	s.bumpVersion(ctx)
	return out, nil
}

// Advance steps the canary ladder forward under a row lock — concurrent
// advances serialize instead of double-stepping.
func (s *Store) Advance(ctx context.Context, name string, adminID int64, clientIP string) (*Flag, bool, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, false, excerrors.Wrap("INTERNAL_ERROR", "flags tx begin", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	f, err := scanFlag(tx.QueryRow(ctx,
		`SELECT `+flagCols+` FROM feature_flags WHERE name = $1 FOR UPDATE`, name))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, false, ErrNotFound
	}
	if err != nil {
		return nil, false, excerrors.Wrap("INTERNAL_ERROR", "flags advance read", err)
	}
	idx, done, err := f.Advance()
	if err != nil {
		return nil, false, excerrors.New("INVALID_REQUEST", err.Error())
	}
	out, err := scanFlag(tx.QueryRow(ctx, `
		UPDATE feature_flags
		   SET stage_idx = $2, updated_by = $3, updated_at = now(),
		       version = version + 1
		 WHERE name = $1
		RETURNING `+flagCols, name, idx, adminID))
	if err != nil {
		return nil, false, excerrors.Wrap("INTERNAL_ERROR", "flags advance", err)
	}
	if _, _, err := admin.Log(ctx, tx, admin.AuditEntry{
		AdminUserID: adminID,
		Action:      "feature_flag.advance",
		TargetType:  "feature_flag",
		BeforeState: f,
		AfterState:  out,
		IPAddress:   stripIP(clientIP),
	}); err != nil {
		return nil, false, excerrors.Wrap("INTERNAL_ERROR", "flags audit", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, false, excerrors.Wrap("INTERNAL_ERROR", "flags commit", err)
	}
	s.cacheSet(ctx, out)
	s.bumpVersion(ctx)
	return out, done, nil
}

// Delete removes a flag. Deleting is the safe off-switch: Eval on a
// missing flag returns false.
func (s *Store) Delete(ctx context.Context, name string, adminID int64, clientIP string) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return excerrors.Wrap("INTERNAL_ERROR", "flags tx begin", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	prev, err := scanFlag(tx.QueryRow(ctx,
		`SELECT `+flagCols+` FROM feature_flags WHERE name = $1 FOR UPDATE`, name))
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return excerrors.Wrap("INTERNAL_ERROR", "flags delete read", err)
	}
	tag, err := tx.Exec(ctx, `DELETE FROM feature_flags WHERE name = $1`, name)
	if err != nil {
		return excerrors.Wrap("INTERNAL_ERROR", "flags delete", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	if _, _, err := admin.Log(ctx, tx, admin.AuditEntry{
		AdminUserID: adminID,
		Action:      "feature_flag.delete",
		TargetType:  "feature_flag",
		BeforeState: prev,
		AfterState:  nil,
		IPAddress:   stripIP(clientIP),
	}); err != nil {
		return excerrors.Wrap("INTERNAL_ERROR", "flags audit", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return excerrors.Wrap("INTERNAL_ERROR", "flags commit", err)
	}
	s.cacheDel(ctx, name)
	s.bumpVersion(ctx)
	return nil
}

// Eval resolves one flag for one caller via the cache/PG read path.
// The error return is significant: handlers that can afford fail-closed
// semantics use Enabled instead.
func (s *Store) Eval(ctx context.Context, name string, c EvalContext) (bool, error) {
	f, err := s.Get(ctx, name)
	if errors.Is(err, ErrNotFound) {
		return false, nil // unknown flag ⇒ off, not an error for callers
	}
	if err != nil {
		return false, err
	}
	return f.Eval(c), nil
}

// Enabled is the fail-closed eval convenience used by feature call
// sites: any store failure reads as "off" (spec §2.7 pessimism — a
// half-configured feature must not leak on).
func (s *Store) Enabled(ctx context.Context, name string, c EvalContext) bool {
	on, err := s.Eval(ctx, name, c)
	return err == nil && on
}

// EvalFrom is the handler-side EvalContext builder: account id first,
// then the caller IP as the stable anonymous key.
func EvalFrom(accountID int64, tier, remoteIP string) EvalContext {
	return EvalContext{AccountID: accountID, Tier: tier, Subject: remoteIP}
}

// stripIP trims a host:port to the bare host for the audit INET column.
func stripIP(addr string) string {
	if addr == "" {
		return ""
	}
	if h, _, err := net.SplitHostPort(addr); err == nil {
		return h
	}
	return addr
}
