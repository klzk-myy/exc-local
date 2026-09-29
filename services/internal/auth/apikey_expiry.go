// Task 13.3.8 — API-key auto-expiration policy (defense-in-depth against
// orphaned high-privilege keys).
//
// Policy (migration 208 columns):
//   - a LIVE key (status ACTIVE, not revoked) older than 90 days carrying
//     the privileged scopes {trade, transfer} with NO ip_allowlist gets
//     those scopes stripped — the key itself stays valid for read;
//   - 7 days ahead of the deadline the holder gets a security_alert
//     notification (expiry_notified_at makes the warning once-only);
//   - the strip is recorded (permissions_revoked_at + revoked_scopes) and
//     RESTORED verbatim when an ip_allowlist is later configured — the
//     sweep re-checks and reinstates;
//   - expiry_override_until (dual-control PUT
//     /api/v1/admin/api-keys/{id}/extend-expiry) postpones the deadline;
//   - expires_at/last_used_at are untouched — this policy is about
//     privilege decay, not key validity.
//
// Sweep scheduling: the gateway runs Sweep on a ticker (repo-conventional
// in-process sweeper) AND deploy/crons/api-key-expiry.sh invokes the
// daily `exchange api-key-expiry-sweep` job — both are idempotent so a
// double-fire is harmless.
package auth

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// Policy constants (task-pinned).
const (
	// APIKeyPrivilegeMaxAge is the age at which an un-allowlisted key
	// loses privileged scopes.
	APIKeyPrivilegeMaxAge = 90 * 24 * time.Hour
	// APIKeyExpiryWarnBefore is the warning lead time (notification at
	// day 83 of 90).
	APIKeyExpiryWarnBefore = 7 * 24 * time.Hour
	// APIKeyMaxExpiryOverride bounds the dual-control grace extension —
	// an override is a postponement, not an exemption.
	APIKeyMaxExpiryOverride = 180 * 24 * time.Hour
)

// privilegedScopes is the scope set the policy strips.
var privilegedScopes = []string{"trade", "transfer"}

// ExpiryCandidate is the sweep's working row (no secret material).
type ExpiryCandidate struct {
	ID                  int64
	KeyID               string
	AccountID           int64
	UserID              int64
	Scopes              []string
	CreatedAt           time.Time
	ExpiryOverrideUntil *time.Time
}

// effectiveDeadline is the instant the sweep revokes absent an
// allowlist: the 90-day base, pushed out by an active override.
func (c ExpiryCandidate) effectiveDeadline(now time.Time) time.Time {
	d := c.CreatedAt.Add(APIKeyPrivilegeMaxAge)
	if c.ExpiryOverrideUntil != nil && c.ExpiryOverrideUntil.After(now) &&
		c.ExpiryOverrideUntil.After(d) {
		return *c.ExpiryOverrideUntil
	}
	return d
}

// SecurityEventSink is the notification seam — satisfied by the
// gateway's securityEventAdapter (notifies via the canonical
// security_alert event; Task 12.3.5 pipeline). nil disables warnings.
type SecurityEventSink interface {
	NotifySecurityEvent(ctx context.Context, userID int64, event string, attrs map[string]any) error
}

// KeyExpiryPolicy drives the warn → revoke → restore sweep.
type KeyExpiryPolicy struct {
	keys   *KeyStore
	notify SecurityEventSink
	now    func() time.Time
	logf   func(format string, args ...any)
}

// NewKeyExpiryPolicy wires the policy; keys is mandatory.
func NewKeyExpiryPolicy(keys *KeyStore, notify SecurityEventSink) (*KeyExpiryPolicy, error) {
	if keys == nil {
		return nil, newError(CodeAuthInternal, "key expiry policy requires a key store")
	}
	return &KeyExpiryPolicy{keys: keys, notify: notify, now: time.Now}, nil
}

// WithLogger wires a diagnostic sink.
func (p *KeyExpiryPolicy) WithLogger(f func(format string, args ...any)) *KeyExpiryPolicy {
	p.logf = f
	return p
}

// WithClock overrides the clock (tests).
func (p *KeyExpiryPolicy) WithClock(c func() time.Time) *KeyExpiryPolicy {
	p.now = c
	return p
}

func (p *KeyExpiryPolicy) log(format string, args ...any) {
	if p.logf != nil {
		p.logf(format, args...)
	}
}

// SweepResult reports one pass.
type SweepResult struct {
	Warned   int `json:"warned"`
	Revoked  int `json:"revoked"`
	Restored int `json:"restored"`
}

// Sweep runs one full policy pass: warn (T-7d) → revoke (T+90d) → restore
// (allowlist configured). Per-key failures are logged and skipped — one
// bad row must not stall the sweep; a query-level failure aborts with the
// error (fail closed — a silent skip would defeat the defense).
func (p *KeyExpiryPolicy) Sweep(ctx context.Context) (SweepResult, error) {
	var res SweepResult
	now := p.now().UTC()

	// Phase 1 — T-7d warnings (once per key via expiry_notified_at). Only
	// keys that actually still carry a privileged scope are warned — a
	// read-only key has nothing to lose. Two warn branches:
	//   (a) base deadline: key inside the 83–90d window AND no extension
	//       pushes the deadline further out;
	//   (b) override deadline: an active extension expires within 7d —
	//       revocation fires at the override, so the warning must too.
	// Keys already past 90d on first deployment revoke immediately (no
	// retroactive warning is meaningful — phase 2 owns them).
	warnRows, err := p.keys.expiryCandidates(ctx, `
		WHERE status='ACTIVE' AND revoked_at IS NULL
		  AND permissions_revoked_at IS NULL
		  AND (ip_allowlist IS NULL OR cardinality(ip_allowlist) = 0)
		  AND expiry_notified_at IS NULL
		  AND scopes && $3::text[]
		  AND (
		        (created_at <= now() - $1::interval
		         AND created_at > now() - $2::interval
		         AND (expiry_override_until IS NULL
		              OR expiry_override_until <= created_at + $2::interval))
		     OR (expiry_override_until IS NOT NULL
		         AND expiry_override_until > now()
		         AND expiry_override_until <= now() + $4::interval)
		      )
		ORDER BY id`, interval(APIKeyPrivilegeMaxAge-APIKeyExpiryWarnBefore),
		interval(APIKeyPrivilegeMaxAge), privilegedScopes,
		interval(APIKeyExpiryWarnBefore))
	if err != nil {
		return res, err
	}
	for _, c := range warnRows {
		// Notify FIRST, then stamp the once-only gate: a failed mark just
		// re-notifies next pass (harmless duplicate); a failed notify
		// after marking would silently burn the only warning.
		p.notifyUser(ctx, c, "api_key_expiry_warning", map[string]any{
			"key_id": c.KeyID, "account_id": c.AccountID,
			"scopes_at_risk": privilegedScopes,
			"revocation_at":  c.effectiveDeadline(now).Format(time.RFC3339),
			"action":         "configure an ip_allowlist on the key to keep trading/transfer scopes",
		})
		if err := p.keys.MarkExpiryNotified(ctx, c.ID, now); err != nil {
			p.log("apikey-expiry: notify-mark key %d: %v", c.ID, err)
			continue
		}
		res.Warned++
	}

	// Phase 2 — revocation at/past the deadline (respecting overrides).
	dueRows, err := p.keys.expiryCandidates(ctx, `
		WHERE status='ACTIVE' AND revoked_at IS NULL
		  AND permissions_revoked_at IS NULL
		  AND (ip_allowlist IS NULL OR cardinality(ip_allowlist) = 0)
		  AND scopes && $1::text[]
		  AND created_at <= now() - $2::interval
		  AND (expiry_override_until IS NULL OR expiry_override_until <= now())
		ORDER BY id`, privilegedScopes, interval(APIKeyPrivilegeMaxAge))
	if err != nil {
		return res, err
	}
	for _, c := range dueRows {
		removed, err := p.keys.RevokePrivilegedScopes(ctx, c.ID, now)
		if err != nil {
			p.log("apikey-expiry: revoke key %d: %v", c.ID, err)
			continue
		}
		if len(removed) == 0 {
			continue // raced with a scope edit — nothing stripped
		}
		res.Revoked++
		p.notifyUser(ctx, c, "api_key_permissions_revoked", map[string]any{
			"key_id": c.KeyID, "account_id": c.AccountID,
			"revoked_scopes": removed,
			"reason":         "key older than 90 days without an ip_allowlist",
			"restore":        "configure an ip_allowlist — the next sweep restores the scopes",
		})
	}

	// Phase 3 — restore: allowlist configured after the strip.
	restored, err := p.keys.RestoreAllowlistedScopes(ctx)
	if err != nil {
		return res, err
	}
	res.Restored = len(restored)
	for _, c := range restored {
		p.notifyUser(ctx, c, "api_key_permissions_restored", map[string]any{
			"key_id": c.KeyID, "account_id": c.AccountID,
			"restored_scopes": c.Scopes,
		})
	}
	return res, nil
}

// notifyUser emits the security_alert via the wired sink (best-effort —
// a dropped warning must not un-mark or un-revoke; the DB state is the
// record, the notification is courtesy).
func (p *KeyExpiryPolicy) notifyUser(ctx context.Context, c ExpiryCandidate, kind string, attrs map[string]any) {
	if p.notify == nil {
		return
	}
	defer func() {
		if r := recover(); r != nil {
			p.log("apikey-expiry: notifier panic on %s: %v", kind, r)
		}
	}()
	if err := p.notify.NotifySecurityEvent(ctx, c.UserID, kind, attrs); err != nil {
		p.log("apikey-expiry: notify %s key %d: %v", kind, c.ID, err)
	}
}

// interval renders a duration as a Postgres interval literal argument.
func interval(d time.Duration) string { return fmt.Sprintf("%f seconds", d.Seconds()) }

// expiryCandidates is the shared candidate query — the WHERE fragment is
// supplied by the caller (phase-specific predicates); args are appended.
func (s *KeyStore) expiryCandidates(ctx context.Context, where string, args ...any) ([]ExpiryCandidate, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT id, key_id, account_id, user_id, scopes, created_at,
		        expiry_override_until
		   FROM api_keys `+where, args...)
	if err != nil {
		return nil, wrapError(CodeAuthInternal, "expiry candidates", err)
	}
	defer rows.Close()
	out := []ExpiryCandidate{}
	for rows.Next() {
		var c ExpiryCandidate
		if err := rows.Scan(&c.ID, &c.KeyID, &c.AccountID, &c.UserID,
			&c.Scopes, &c.CreatedAt, &c.ExpiryOverrideUntil); err != nil {
			return nil, wrapError(CodeAuthInternal, "expiry candidate scan", err)
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// MarkExpiryNotified stamps the once-only warning gate.
func (s *KeyStore) MarkExpiryNotified(ctx context.Context, id int64, at time.Time) error {
	tag, err := s.pool.Exec(ctx,
		`UPDATE api_keys SET expiry_notified_at=$2, updated_at=now()
		 WHERE id=$1 AND expiry_notified_at IS NULL`, id, at)
	if err != nil {
		return wrapError(CodeAuthInternal, "expiry notified mark", err)
	}
	if tag.RowsAffected() == 0 {
		return newError(CodeAPIKeyNotFound, "key not found or already notified")
	}
	return nil
}

// RevokePrivilegedScopes strips trade/transfer from the key's scopes
// inside a FOR UPDATE tx: removed scopes land on revoked_scopes and
// permissions_revoked_at is stamped. Returns the scopes actually removed
// (empty when the key no longer holds any — idempotent).
func (s *KeyStore) RevokePrivilegedScopes(ctx context.Context, id int64, at time.Time) ([]string, error) {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return nil, wrapError(CodeAuthInternal, "revoke tx begin", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var scopes []string
	var alreadyRevoked *time.Time
	err = tx.QueryRow(ctx,
		`SELECT scopes, permissions_revoked_at FROM api_keys
		  WHERE id=$1 AND status='ACTIVE' AND revoked_at IS NULL
		  FOR UPDATE`, id).Scan(&scopes, &alreadyRevoked)
	if err == pgx.ErrNoRows {
		return nil, newError(CodeAPIKeyNotFound, "key not found or not live")
	}
	if err != nil {
		return nil, wrapError(CodeAuthInternal, "key lock", err)
	}
	if alreadyRevoked != nil {
		return nil, nil // already stripped — idempotent no-op
	}
	var removed, kept []string
	for _, sc := range scopes {
		if sc == "trade" || sc == "transfer" {
			removed = append(removed, sc)
		} else {
			kept = append(kept, sc)
		}
	}
	if len(removed) == 0 {
		return nil, nil
	}
	if kept == nil {
		kept = []string{}
	}
	if _, err := tx.Exec(ctx, `
		UPDATE api_keys
		   SET scopes=$2, revoked_scopes=$3, permissions_revoked_at=$4,
		       updated_at=now()
		 WHERE id=$1`, id, kept, removed, at); err != nil {
		return nil, wrapError(CodeAuthInternal, "privilege revoke", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, wrapError(CodeAuthInternal, "revoke commit", err)
	}
	return removed, nil
}

// RestoreAllowlistedScopes reinstates revoked_scopes on every stripped
// key that now carries an ip_allowlist. Returns the restored candidates
// (for the courtesy notification).
func (s *KeyStore) RestoreAllowlistedScopes(ctx context.Context) ([]ExpiryCandidate, error) {
	// UPDATE ... FROM returns the FROM row's PRE-update values in
	// RETURNING (PG convention) — old.revoked_scopes is the stripped set,
	// captured before the target row's clear. The DISTINCT-merge keeps the
	// restore idempotent when a scope was re-added manually before the
	// allowlist landed (a duplicated scope would be an assertion hazard).
	rows, err := s.pool.Query(ctx, `
		UPDATE api_keys k
		   SET scopes = ARRAY(SELECT DISTINCT s FROM unnest(k.scopes || old.revoked_scopes) s),
		       revoked_scopes='{}', permissions_revoked_at=NULL,
		       expiry_notified_at=NULL, updated_at=now()
		  FROM (SELECT id, revoked_scopes FROM api_keys
		         WHERE permissions_revoked_at IS NOT NULL
		           AND revoked_at IS NULL AND status='ACTIVE'
		           AND ip_allowlist IS NOT NULL AND cardinality(ip_allowlist) > 0) old
		 WHERE k.id = old.id
		 RETURNING k.id, k.key_id, k.account_id, k.user_id,
		           old.revoked_scopes, k.created_at`)
	if err != nil {
		return nil, wrapError(CodeAuthInternal, "allowlist restore", err)
	}
	defer rows.Close()
	out := []ExpiryCandidate{}
	for rows.Next() {
		var c ExpiryCandidate
		var restored []string
		if err := rows.Scan(&c.ID, &c.KeyID, &c.AccountID, &c.UserID,
			&restored, &c.CreatedAt); err != nil {
			return nil, wrapError(CodeAuthInternal, "restore scan", err)
		}
		c.Scopes = restored
		out = append(out, c)
	}
	return out, rows.Err()
}

// ExtendExpiryOverrideTx applies the dual-control grace extension inside
// the approval transaction (internal/admin Executor seam): the sweep
// deadline becomes `until` (bounded by APIKeyMaxExpiryOverride). Runs
// in-tx so the grant and its four-eyes record commit atomically.
func (s *KeyStore) ExtendExpiryOverrideTx(ctx context.Context, tx pgx.Tx,
	id int64, until time.Time) error {
	if !until.After(s.now()) {
		return newError(CodeInvalidRequest, "expiry extension must be in the future")
	}
	if until.After(s.now().Add(APIKeyMaxExpiryOverride)) {
		return newError(CodeInvalidRequest,
			fmt.Sprintf("expiry extension exceeds the %d-day bound", int(APIKeyMaxExpiryOverride.Hours()/24)))
	}
	tag, err := tx.Exec(ctx, `
		UPDATE api_keys SET expiry_override_until=$2, updated_at=now()
		 WHERE id=$1 AND status='ACTIVE' AND revoked_at IS NULL`, id, until)
	if err != nil {
		return wrapError(CodeAuthInternal, "expiry override", err)
	}
	if tag.RowsAffected() == 0 {
		return newError(CodeAPIKeyNotFound, "key not found or not live")
	}
	return nil
}
