// Phase-11 Task 11.3.4 / 11.3.8 / 11.3.12 — trading suspension &
// scoped kill-switch domain types, the Redis flag resolver and the
// durable-record store.
//
// Architecture: two planes share the `halt:*` key namespace owned by
// internal/redis/client.go.
//
//   - Data plane (hot path): KillSwitchResolver MGETs the applicable
//     halt flags in ONE round-trip per order admission / funding op /
//     quote ingress check, first-match-wins in precedence order. Any
//     lookup error fails CLOSED — the caller treats an unreadable flag
//     store as suspended (spec §2.7 pessimism), never as "open".
//   - Control plane: killswitch_service.go mutates flags through
//     KillSwitchService under the §8.2 dual-control conventions
//     (synchronous approver_id — the §8.2 contract for ops that cannot
//     wait on the DualControlService queue), records every transition
//     in trading_suspensions (migration 200) + admin_audit_log, and
//     fans out advisories (public announcement, WS frame, Aeron/NATS
//     control topic exchange:control:killswitch).
//
// Scope lattice (suspension_scope_enum):
//
//	Task 11.3.4   GLOBAL                                — full venue halt
//	Task 11.3.8   ACCOUNT, INSTRUMENT, FIX_SESSION      — §7.2/§24 #152
//	Task 11.3.12  COUNTERPARTY, LP, RAIL                — §24 #409
//	prompt extras INSTRUMENT_CLASS (instruments.instrument_type axis),
//	              REGION, ENV, DESK — administrative axes resolved only
//	              when the caller supplies the dimension value.
//
// Precedence on the order-admission path (first match wins):
// ACCOUNT → COUNTERPARTY → FIX_SESSION → INSTRUMENT → INSTRUMENT_CLASS
// → DESK → REGION → ENV → GLOBAL. LP/RAIL resolve on their own paths
// (FIX Mass-Quote ingress, funding rail dispatch).
package admin

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	exchredis "exchange/internal/redis"
	excerrors "exchange/pkg/errors"
)

// ---------------------------------------------------------------------------
// Scope vocabulary — matches suspension_scope_enum (migration 200).
// ---------------------------------------------------------------------------

const (
	ScopeGlobal          = "GLOBAL"
	ScopeAccount         = "ACCOUNT"
	ScopeCounterparty    = "COUNTERPARTY"
	ScopeInstrument      = "INSTRUMENT"
	ScopeInstrumentClass = "INSTRUMENT_CLASS"
	ScopeFixSession      = "FIX_SESSION"
	ScopeLP              = "LP"
	ScopeRail            = "RAIL"
	ScopeRegion          = "REGION"
	ScopeEnv             = "ENV"
	ScopeDesk            = "DESK"
)

// ValidScope vets an operator-supplied scope token (fail closed on the
// unknown — never coerce to GLOBAL, which would silently widen a scoped
// intent into a full-venue halt).
func ValidScope(s string) bool {
	switch strings.ToUpper(strings.TrimSpace(s)) {
	case ScopeGlobal, ScopeAccount, ScopeCounterparty, ScopeInstrument,
		ScopeInstrumentClass, ScopeFixSession, ScopeLP, ScopeRail,
		ScopeRegion, ScopeEnv, ScopeDesk:
		return true
	}
	return false
}

// TradingScopes are the dimensions consulted on the order-admission
// path; LP and RAIL live on the quote-ingress and funding paths.
var TradingScopes = []string{
	ScopeGlobal, ScopeAccount, ScopeCounterparty, ScopeInstrument,
	ScopeInstrumentClass, ScopeFixSession, ScopeRegion, ScopeEnv, ScopeDesk,
}

// ---------------------------------------------------------------------------
// Suspension — durable record row (trading_suspensions, migration 200).
// ---------------------------------------------------------------------------

type Suspension struct {
	SuspensionID    int64      `json:"suspension_id"`
	Scope           string     `json:"scope"`
	TargetID        string     `json:"target_id"`
	Reason          string     `json:"reason"`
	State           string     `json:"state"` // ACTIVE | CLEARED
	InitiatedBy     int64      `json:"initiated_by"`
	ApprovedBy      *int64     `json:"approved_by,omitempty"`
	ClearedBy       *int64     `json:"cleared_by,omitempty"`
	ClearApprovedBy *int64     `json:"clear_approved_by,omitempty"`
	ClearedReason   string     `json:"cleared_reason,omitempty"`
	ClientIP        string     `json:"client_ip,omitempty"`
	CreatedAt       time.Time  `json:"created_at"`
	ClearedAt       *time.Time `json:"cleared_at,omitempty"`
}

// ---------------------------------------------------------------------------
// Resolver — the single kill-switch lookup every service calls
// (fail closed on lookup error per spec §2.7).
// ---------------------------------------------------------------------------

// HaltFlags is the read seam the resolver needs — *redis.Client
// satisfies it; tests substitute an in-memory fake.
type HaltFlags interface {
	HaltScopeScan(ctx context.Context, keys []string) (map[string]string, error)
	IsHalted(ctx context.Context) (bool, error)
}

// HaltQuery carries whichever dimensions the caller can supply; zero
// values skip that dimension. AccountID doubles as the COUNTERPARTY
// target on the order path (§24 #409 scopes institutional users by id).
type HaltQuery struct {
	AccountID       int64
	Symbol          string // canonical instrument symbol
	InstrumentClass string // instruments.instrument_type (SPOT|FORWARD|...)
	SessionID       string // FIX/WS session id
	Rail            string // banking rail (funding path)
	LP              string // liquidity-provider id (quote-ingress path)
	Region          string
	Env             string
	Desk            string
}

// Decision is the resolution result. Suspended=false with Err=nil means
// "verified open"; Suspended=true carries the matching scope, target and
// the operator reason stored on the flag.
type Decision struct {
	Suspended bool   `json:"suspended"`
	Scope     string `json:"scope,omitempty"`
	Target    string `json:"target,omitempty"`
	Reason    string `json:"reason,omitempty"`
}

// KillSwitchResolver resolves halt flags. env is bound at construction
// so the ENV dimension is always evaluated even when the caller does
// not pass it.
type KillSwitchResolver struct {
	rdb HaltFlags
	env string
}

// NewKillSwitchResolver builds the resolver over the coordination Redis
// client. env is the deployment environment name (cfg.Environment);
// "" disables the ENV axis.
func NewKillSwitchResolver(rdb HaltFlags, env string) *KillSwitchResolver {
	return &KillSwitchResolver{rdb: rdb, env: strings.ToUpper(strings.TrimSpace(env))}
}

// Env returns the bound environment axis (ENV flag target).
func (r *KillSwitchResolver) Env() string { return r.env }

// canonicalTarget normalizes a flag-key target for the axes that are
// case-insensitive elsewhere in the system (symbols, instrument classes,
// rails, regions, environments). Opaque ids — account/counterparty,
// FIX session, LP, desk — pass through trimmed so operator intent is
// preserved exactly. The same canonicalization runs on the WRITE path
// (killswitch_service.go normalizeScope) so reads and sets agree.
func canonicalTarget(scope, target string) string {
	t := strings.TrimSpace(target)
	switch scope {
	case ScopeInstrument, ScopeInstrumentClass, ScopeRail,
		ScopeRegion, ScopeEnv:
		return strings.ToUpper(t)
	}
	return t
}

// orderKeys renders the flag-key candidate list in precedence order —
// ACCOUNT → COUNTERPARTY → FIX_SESSION → INSTRUMENT → INSTRUMENT_CLASS →
// DESK → REGION → ENV → GLOBAL (GLOBAL always last as the fallback).
func (r *KillSwitchResolver) orderKeys(q HaltQuery) []struct{ scope, target, key string } {
	var keys []struct{ scope, target, key string }
	add := func(scope, target string) {
		t := canonicalTarget(scope, target)
		if t == "" && scope != ScopeGlobal {
			return
		}
		keys = append(keys, struct{ scope, target, key string }{
			scope, t, exchredis.HaltKey(scope, t)})
	}
	if q.AccountID > 0 {
		id := strconv.FormatInt(q.AccountID, 10)
		add(ScopeAccount, id)
		add(ScopeCounterparty, id)
	}
	add(ScopeFixSession, q.SessionID)
	add(ScopeInstrument, q.Symbol)
	add(ScopeInstrumentClass, strings.ToUpper(q.InstrumentClass))
	add(ScopeDesk, q.Desk)
	add(ScopeRegion, strings.ToUpper(q.Region))
	add(ScopeEnv, r.env)
	add(ScopeGlobal, "")
	return keys
}

// Resolve evaluates the full lattice for the caller's dimensions.
// Lookup failure returns a non-nil error — callers MUST fail closed
// (reject with TRADING_HALTED / refuse the operation) rather than treat
// it as open.
func (r *KillSwitchResolver) Resolve(ctx context.Context, q HaltQuery) (Decision, error) {
	if r == nil || r.rdb == nil {
		return Decision{}, excerrors.New("INTERNAL_ERROR",
			"kill-switch resolver unavailable — cannot verify trading state")
	}
	keys := r.orderKeys(q)
	raw := make([]string, 0, len(keys))
	for _, k := range keys {
		raw = append(raw, k.key)
	}
	set, err := r.rdb.HaltScopeScan(ctx, raw)
	if err != nil {
		return Decision{}, err
	}
	for _, k := range keys {
		if reason, ok := set[k.key]; ok {
			return Decision{Suspended: true, Scope: k.scope,
				Target: k.target, Reason: reason}, nil
		}
	}
	return Decision{}, nil
}

// OrderHalt is the orders.Service admission seam (orders.KillSwitch).
// Returns the suspended-scope label ("" = open), a human-facing detail
// string, or a lookup error — the caller fails closed on error.
func (r *KillSwitchResolver) OrderHalt(ctx context.Context, accountID int64,
	symbol, instrumentClass, sessionID string) (scope string, detail string, err error) {
	d, err := r.Resolve(ctx, HaltQuery{
		AccountID:       accountID,
		Symbol:          symbol,
		InstrumentClass: instrumentClass,
		SessionID:       sessionID,
	})
	if err != nil {
		return "", "", err
	}
	if !d.Suspended {
		return "", "", nil
	}
	return d.Scope, fmt.Sprintf("%s%s: %s", d.Scope, targetSuffix(d.Target), d.Reason), nil
}

func targetSuffix(t string) string {
	if t == "" {
		return ""
	}
	return "[" + t + "]"
}

// GlobalHalted is the middleware admission-gate read — the global flag
// only; errors propagate (gate fails closed).
func (r *KillSwitchResolver) GlobalHalted(ctx context.Context) (bool, error) {
	if r == nil || r.rdb == nil {
		return false, excerrors.New("INTERNAL_ERROR", "kill-switch resolver unavailable")
	}
	return r.rdb.IsHalted(ctx)
}

// RailSuspended is the funding-path check (Task 11.3.12 SCOPE_RAIL):
// a suspended rail rejects deposit/withdrawal processing for that rail
// while sibling rails continue. Errors fail closed upstream.
func (r *KillSwitchResolver) RailSuspended(ctx context.Context, rail string) (bool, string, error) {
	d, err := r.ResolveScope(ctx, ScopeRail, rail)
	if err != nil {
		return false, "", err
	}
	if !d.Suspended {
		return false, "", nil
	}
	return true, d.Reason, nil
}

// LPSuspended is the quote-ingress check (Task 11.3.12 SCOPE_LP): FIX
// Mass Quotes (35=i) from the LP are rejected while CLOB trading
// continues.
func (r *KillSwitchResolver) LPSuspended(ctx context.Context, lpID string) (bool, string, error) {
	d, err := r.ResolveScope(ctx, ScopeLP, lpID)
	if err != nil {
		return false, "", err
	}
	if !d.Suspended {
		return false, "", nil
	}
	return true, d.Reason, nil
}

// ResolveScope checks a single (scope, target) flag — the generic
// single-dimension lookup for services that only care about one axis.
func (r *KillSwitchResolver) ResolveScope(ctx context.Context, scope, target string) (Decision, error) {
	if r == nil || r.rdb == nil {
		return Decision{}, excerrors.New("INTERNAL_ERROR", "kill-switch resolver unavailable")
	}
	key := exchredis.HaltKey(scope, canonicalTarget(scope, target))
	set, err := r.rdb.HaltScopeScan(ctx, []string{key})
	if err != nil {
		return Decision{}, err
	}
	if reason, ok := set[key]; ok {
		return Decision{Suspended: true, Scope: scope, Target: target, Reason: reason}, nil
	}
	return Decision{}, nil
}

// ---------------------------------------------------------------------------
// Store — trading_suspensions durable record (migration 200).
// ---------------------------------------------------------------------------

// KillSwitchStore is the suspension-record persistence seam.
type KillSwitchStore struct {
	pool *pgxpool.Pool
}

func NewKillSwitchStore(pool *pgxpool.Pool) *KillSwitchStore {
	return &KillSwitchStore{pool: pool}
}

const suspensionCols = `
	suspension_id, scope::text, target_id, reason, state::text,
	initiated_by, approved_by, cleared_by, clear_approved_by,
	cleared_reason, client_ip, created_at, cleared_at`

func scanSuspension(row pgx.Row) (*Suspension, error) {
	var s Suspension
	var ap, cb, cab *int64
	var cr *string
	var cip *string
	var cat *time.Time
	err := row.Scan(&s.SuspensionID, &s.Scope, &s.TargetID, &s.Reason,
		&s.State, &s.InitiatedBy, &ap, &cb, &cab, &cr, &cip,
		&s.CreatedAt, &cat)
	if err != nil {
		return nil, err
	}
	s.ApprovedBy, s.ClearedBy, s.ClearApprovedBy = ap, cb, cab
	if cr != nil {
		s.ClearedReason = *cr
	}
	if cip != nil {
		s.ClientIP = *cip
	}
	s.ClearedAt = cat
	return &s, nil
}

// InsertActive records a new ACTIVE suspension (the admin_audit_log
// row commits in the same tx via admin.Log).
func (s *KillSwitchStore) InsertActive(ctx context.Context, tx pgx.Tx, in *Suspension) (int64, error) {
	var id int64
	var st string
	err := tx.QueryRow(ctx, `
		INSERT INTO trading_suspensions
		    (scope, target_id, reason, initiated_by, approved_by, client_ip)
		VALUES ($1::suspension_scope_enum, $2, $3, $4, $5, NULLIF($6,''))
		RETURNING suspension_id, state::text, created_at`,
		in.Scope, in.TargetID, in.Reason, in.InitiatedBy, in.ApprovedBy,
		in.ClientIP).Scan(&id, &st, &in.CreatedAt)
	if err != nil {
		return 0, fmt.Errorf("insert suspension: %w", err)
	}
	in.State = st
	return id, nil
}

// ActiveByScope returns the ACTIVE suspension for (scope, target), or
// nil — used by Clear so resume flips the durable record, not just the
// Redis flag.
func (s *KillSwitchStore) ActiveByScope(ctx context.Context, tx pgx.Tx,
	scope, target string) (*Suspension, error) {
	row, err := scanSuspension(tx.QueryRow(ctx,
		`SELECT `+suspensionCols+` FROM trading_suspensions
		 WHERE scope = $1::suspension_scope_enum AND target_id = $2
		   AND state = 'ACTIVE'`, scope, target))
	if err == pgx.ErrNoRows {
		return nil, nil
	}
	return row, err
}

// MarkCleared flips an ACTIVE row to CLEARED inside the mutation tx.
func (s *KillSwitchStore) MarkCleared(ctx context.Context, tx pgx.Tx,
	id int64, clearedBy int64, approver *int64, reason string) error {
	tag, err := tx.Exec(ctx, `
		UPDATE trading_suspensions
		   SET state = 'CLEARED', cleared_by = $2,
		       clear_approved_by = $3, cleared_reason = NULLIF($4,''),
		       cleared_at = now()
		 WHERE suspension_id = $1 AND state = 'ACTIVE'`,
		id, clearedBy, approver, reason)
	if err != nil {
		return fmt.Errorf("clear suspension: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return excerrors.New("NOT_FOUND",
			"suspension already cleared by a concurrent reset")
	}
	return nil
}

// ListActive enumerates all ACTIVE suspensions (admin status surface +
// boot-time flag reconcile).
func (s *KillSwitchStore) ListActive(ctx context.Context) ([]Suspension, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT `+suspensionCols+` FROM trading_suspensions
		 WHERE state = 'ACTIVE' ORDER BY created_at`)
	if err != nil {
		return nil, fmt.Errorf("list suspensions: %w", err)
	}
	defer rows.Close()
	var out []Suspension
	for rows.Next() {
		s, err := scanSuspension(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *s)
	}
	return out, rows.Err()
}

// Recent returns the newest rows (any state) for the admin audit view.
func (s *KillSwitchStore) Recent(ctx context.Context, limit int) ([]Suspension, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	rows, err := s.pool.Query(ctx,
		`SELECT `+suspensionCols+` FROM trading_suspensions
		 ORDER BY created_at DESC LIMIT $1`, limit)
	if err != nil {
		return nil, fmt.Errorf("recent suspensions: %w", err)
	}
	defer rows.Close()
	var out []Suspension
	for rows.Next() {
		s, err := scanSuspension(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *s)
	}
	return out, rows.Err()
}
