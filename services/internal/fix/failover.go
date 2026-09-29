// Package fix implements the FIX protocol gateway internals for
// Phase-18 (spec §9).
//
// This file owns Task 18.3.12 — session failover, bidirectional
// sequence state synchronization and in-flight duplicate suppression
// (spec §9.8, §24 #189):
//
//   - Per-session sequence state (in_seq_num / out_seq_num /
//     last_heard) is maintained atomically in the Redis Sentinel
//     coordination cluster via Lua CAS (key `fix:seq:{session_id}`),
//     with the PostgreSQL `fix_sessions` table (spec §5.20) as the
//     durable fallback.
//   - A hot-standby gateway claims a session through an epoch-fenced
//     ownership lease before resuming it — two live owners can never
//     interleave sequence writes.
//   - On client Logon (35=A) to the standby, the shared state is
//     compared against the client's MsgSeqNum(34); gaps are bridged
//     with ResendRequest(35=2) / SequenceReset-GapFill(35=4) per the
//     resend machinery in gapfill.go.
//   - Replayed NewOrderSingle (PossDupFlag=Y) submissions are
//     deduplicated on ClOrdID so a failover cannot double-execute.
//
// Fail-closed posture (spec §2.7): every store error propagates to the
// caller; the standby never resumes on guessed sequence numbers. When
// Redis is unavailable the PG fallback preserves atomicity through
// conditional UPDATEs; owner fencing degrades to a heartbeat-lease
// claim (no epoch column exists in spec §5.20 — documented deviation).
package fix

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	goredis "github.com/redis/go-redis/v9"
)

// ---------------------------------------------------------------------------
// Sequence state
// ---------------------------------------------------------------------------

// SeqState is the shared per-session sequence record. InSeqNum is the
// next expected inbound MsgSeqNum(34); OutSeqNum is the next outbound
// MsgSeqNum (fix_sessions.target_seq_num / sender_seq_num in spec §5.20
// terms). Owner/Epoch fence standby takeovers.
type SeqState struct {
	SessionID string    // canonical session key — quickfix SessionID.String() == fix_sessions.session_id
	InSeqNum  int64     // next expected inbound MsgSeqNum (target_seq_num)
	OutSeqNum int64     // next outbound MsgSeqNum (sender_seq_num)
	LastHeard time.Time // wall-clock of last inbound message
	Owner     string    // owning gateway instance id; "" = unclaimed
	Epoch     int64     // fencing epoch — incremented on every ownership transfer
}

// ErrSessionNotFound is returned when no sequence state exists for the
// session (never logged on, or state pruned).
var ErrSessionNotFound = errors.New("fix: session sequence state not found")

// ErrSessionOwnedElsewhere is returned when a live peer gateway still
// holds the ownership lease — the standby must not resume.
var ErrSessionOwnedElsewhere = errors.New("fix: session owned by another live gateway")

// ErrInboundSeqMismatch is returned by RecordInbound when the received
// MsgSeqNum does not equal the shared expected value; the caller runs
// the §9.9 gap-resolution flow (AssessInbound in gapfill.go).
var ErrInboundSeqMismatch = errors.New("fix: inbound MsgSeqNum mismatch")

// SeqStore is the failover coordination seam for per-session sequence
// state. Redis (SeqStoreRedis) is authoritative; PGSeqStore is the
// durable fallback. Implementations must apply writes atomically —
// concurrent CAS attempts from racing owners must never tear in/out
// pairs.
type SeqStore interface {
	// Load returns the stored state or ErrSessionNotFound.
	Load(ctx context.Context, sessionID string) (SeqState, error)
	// CAS swaps stored state to `next` iff the stored in/out seqs equal
	// want.InSeqNum/want.OutSeqNum. Returns the post-image (current state
	// on a lost race) and whether the swap landed.
	CAS(ctx context.Context, want, next SeqState) (SeqState, bool, error)
	// Claim takes or renews ownership of sessionID for `owner`. It
	// succeeds when the session is unowned, the lease has expired, or the
	// same owner re-claims. Returns the post-image and whether the claim
	// landed; on a lost claim the post-image carries the live owner.
	Claim(ctx context.Context, sessionID, owner string, lease time.Duration, now time.Time) (SeqState, bool, error)
}

// seqKey is the coordination-cluster key for per-session sequence state
// (spec §4 naming convention — colon-separated namespace).
func seqKey(sessionID string) string { return "fix:seq:" + sessionID }

// ---------------------------------------------------------------------------
// Redis Sentinel sequence store (primary)
// ---------------------------------------------------------------------------

// seqCASScript is the atomic compare-and-swap on the sequence hash.
// When want carries a non-empty owner the swap additionally fences on
// it — a demoted gateway that lost the lease cannot regress the
// counters. owner/owner_exp_ns/epoch are owned by the claim script and
// are never written here. Returns {applied, in, out, heard_ns, epoch,
// owner}: applied=0 carries the LOSING post-image so the caller can
// retry or branch without a second round-trip.
var seqCASScript = goredis.NewScript(`
local cur_in    = tonumber(redis.call('HGET', KEYS[1], 'in') or '0')
local cur_out   = tonumber(redis.call('HGET', KEYS[1], 'out') or '0')
local cur_owner = redis.call('HGET', KEYS[1], 'owner') or ''
if cur_in ~= tonumber(ARGV[1]) or cur_out ~= tonumber(ARGV[2])
   or (ARGV[6] ~= '' and cur_owner ~= ARGV[6]) then
  return {0, cur_in, cur_out,
          redis.call('HGET', KEYS[1], 'heard_ns') or '0',
          redis.call('HGET', KEYS[1], 'epoch') or '0',
          cur_owner}
end
redis.call('HSET', KEYS[1], 'in', ARGV[3], 'out', ARGV[4], 'heard_ns', ARGV[5])
return {1, ARGV[3], ARGV[4], ARGV[5],
        redis.call('HGET', KEYS[1], 'epoch') or '0', cur_owner}
`)

// seqClaimScript is the ownership-lease CAS: the claim lands only when
// the session is unowned, the prior lease expired, or the same owner
// re-claims. Every landed claim bumps `epoch` — a demoted gateway that
// briefly kept a stale lease cannot act on a stale epoch (same fencing
// contract as engine:leader epoch leases, spec §18.6.2).
var seqClaimScript = goredis.NewScript(`
local owner = redis.call('HGET', KEYS[1], 'owner') or ''
local exp   = tonumber(redis.call('HGET', KEYS[1], 'owner_exp_ns') or '0')
local now   = tonumber(ARGV[2])
if owner ~= '' and owner ~= ARGV[1] and exp > now then
  return {0, tonumber(redis.call('HGET', KEYS[1], 'epoch') or '0'), owner}
end
local epoch = tonumber(redis.call('HGET', KEYS[1], 'epoch') or '0') + 1
redis.call('HSET', KEYS[1], 'owner', ARGV[1], 'owner_exp_ns', now + tonumber(ARGV[3]), 'epoch', epoch)
return {1, epoch, ARGV[1]}
`)

// RedisSeqStore implements SeqStore on the Sentinel coordination
// cluster. The key lives on the noeviction primary (deploy/redis
// topology) — sequence state must never be evicted.
type RedisSeqStore struct {
	rdb *goredis.Client
}

// NewRedisSeqStore binds the store to a coordination-instance client
// (internal/redis.NewFailoverClient output satisfies *goredis.Client).
func NewRedisSeqStore(rdb *goredis.Client) *RedisSeqStore {
	return &RedisSeqStore{rdb: rdb}
}

// seqCASResult unpacks the Lua {applied, in, out, heard_ns, epoch,
// owner} tuple.
func seqCASResult(v any) (applied bool, st SeqState, err error) {
	arr, ok := v.([]interface{})
	if !ok || len(arr) != 6 {
		return false, SeqState{}, fmt.Errorf("fix: unexpected CAS script result %#v", v)
	}
	toInt := func(i int) (int64, error) {
		switch n := arr[i].(type) {
		case int64:
			return n, nil
		case string:
			var v int64
			_, err := fmt.Sscanf(n, "%d", &v)
			return v, err
		default:
			return 0, fmt.Errorf("fix: CAS result field %d type %T", i, arr[i])
		}
	}
	appliedInt, err := toInt(0)
	if err != nil {
		return false, SeqState{}, err
	}
	in, err := toInt(1)
	if err != nil {
		return false, SeqState{}, err
	}
	out, err := toInt(2)
	if err != nil {
		return false, SeqState{}, err
	}
	heard, err := toInt(3)
	if err != nil {
		return false, SeqState{}, err
	}
	epoch, err := toInt(4)
	if err != nil {
		return false, SeqState{}, err
	}
	owner, _ := arr[5].(string)
	st = SeqState{
		InSeqNum:  in,
		OutSeqNum: out,
		LastHeard: time.Unix(0, heard).UTC(),
		Epoch:     epoch,
		Owner:     owner,
	}
	return appliedInt == 1, st, nil
}

// Load reads fix:seq:{session}; an absent hash means the session never
// persisted state → ErrSessionNotFound.
func (s *RedisSeqStore) Load(ctx context.Context, sessionID string) (SeqState, error) {
	h, err := s.rdb.HGetAll(ctx, seqKey(sessionID)).Result()
	if err != nil {
		return SeqState{}, fmt.Errorf("redis load seq %s: %w", sessionID, err)
	}
	if len(h) == 0 {
		return SeqState{}, ErrSessionNotFound
	}
	st := SeqState{SessionID: sessionID, Owner: h["owner"]}
	var heard int64
	fmt.Sscanf(h["in"], "%d", &st.InSeqNum)
	fmt.Sscanf(h["out"], "%d", &st.OutSeqNum)
	fmt.Sscanf(h["heard_ns"], "%d", &heard)
	fmt.Sscanf(h["epoch"], "%d", &st.Epoch)
	if heard != 0 {
		st.LastHeard = time.Unix(0, heard).UTC()
	}
	return st, nil
}

// CAS applies the swap atomically via seqCASScript. want.Owner is a
// fencing precondition when non-empty: the swap lands only while this
// gateway still holds the lease. owner/epoch are read back but never
// written — ownership moves only through Claim.
func (s *RedisSeqStore) CAS(ctx context.Context, want, next SeqState) (SeqState, bool, error) {
	res, err := seqCASScript.Run(ctx, s.rdb, []string{seqKey(want.SessionID)},
		want.InSeqNum, want.OutSeqNum,
		next.InSeqNum, next.OutSeqNum,
		next.LastHeard.UnixNano(), want.Owner,
	).Result()
	if err != nil {
		return SeqState{}, false, fmt.Errorf("redis CAS seq %s: %w", want.SessionID, err)
	}
	applied, st, err := seqCASResult(res)
	if err != nil {
		return SeqState{}, false, err
	}
	st.SessionID = want.SessionID
	return st, applied, nil
}

// Claim runs the ownership-lease CAS via seqClaimScript.
func (s *RedisSeqStore) Claim(ctx context.Context, sessionID, owner string, lease time.Duration, now time.Time) (SeqState, bool, error) {
	res, err := seqClaimScript.Run(ctx, s.rdb, []string{seqKey(sessionID)},
		owner, now.UnixNano(), lease.Nanoseconds(),
	).Result()
	if err != nil {
		return SeqState{}, false, fmt.Errorf("redis claim seq %s: %w", sessionID, err)
	}
	arr, ok := res.([]interface{})
	if !ok || len(arr) != 3 {
		return SeqState{}, false, fmt.Errorf("fix: unexpected claim result %#v", res)
	}
	claimed, _ := arr[0].(int64)
	var epoch int64
	switch e := arr[1].(type) {
	case int64:
		epoch = e
	case string:
		fmt.Sscanf(e, "%d", &epoch)
	}
	liveOwner, _ := arr[2].(string)
	st, lerr := s.Load(ctx, sessionID)
	if lerr != nil && !errors.Is(lerr, ErrSessionNotFound) {
		return SeqState{}, false, lerr
	}
	if errors.Is(lerr, ErrSessionNotFound) {
		st = SeqState{SessionID: sessionID}
	}
	st.Epoch = epoch
	st.Owner = liveOwner
	return st, claimed == 1, nil
}

// ---------------------------------------------------------------------------
// PostgreSQL sequence store (durable fallback — spec §5.20 fix_sessions)
// ---------------------------------------------------------------------------

// PGSeqStore implements SeqStore against fix_sessions
// (sender_seq_num / target_seq_num / last_heartbeat_at / status).
//
// Deviation (documented, spec §27 candidate): spec §5.20 has no owner /
// epoch columns, so the PG fallback fences ownership with a
// heartbeat-lease conditional UPDATE instead of an epoch CAS. It is
// safe for a single standby takeover but weaker than the Redis path —
// a partitioned ex-owner whose own write stalls past the lease can lose
// the claim yet still hold a local lease timer. The session layer must
// therefore stop acting on session input as soon as its Claim renewal
// fails, regardless of store.
type PGSeqStore struct {
	pool *pgxpool.Pool
}

// NewPGSeqStore binds the fallback store.
func NewPGSeqStore(pool *pgxpool.Pool) *PGSeqStore {
	return &PGSeqStore{pool: pool}
}

// Load maps fix_sessions columns onto SeqState.
func (s *PGSeqStore) Load(ctx context.Context, sessionID string) (SeqState, error) {
	var st SeqState
	var heard *time.Time
	err := s.pool.QueryRow(ctx,
		`SELECT target_seq_num, sender_seq_num, last_heartbeat_at
		   FROM fix_sessions WHERE session_id = $1`, sessionID).
		Scan(&st.InSeqNum, &st.OutSeqNum, &heard)
	if errors.Is(err, pgx.ErrNoRows) {
		return SeqState{}, ErrSessionNotFound
	}
	if err != nil {
		return SeqState{}, fmt.Errorf("pg load seq %s: %w", sessionID, err)
	}
	st.SessionID = sessionID
	if heard != nil {
		st.LastHeard = heard.UTC()
	}
	return st, nil
}

// CAS is a conditional UPDATE — the swap lands only while the stored
// pair still equals `want`. On a lost race the current row is read back
// so the caller sees the same post-image contract as the Redis path.
func (s *PGSeqStore) CAS(ctx context.Context, want, next SeqState) (SeqState, bool, error) {
	tag, err := s.pool.Exec(ctx,
		`UPDATE fix_sessions
		    SET target_seq_num = $2, sender_seq_num = $3,
		        last_heartbeat_at = $4, updated_at = now()
		  WHERE session_id = $1
		    AND target_seq_num = $5 AND sender_seq_num = $6`,
		want.SessionID, next.InSeqNum, next.OutSeqNum, next.LastHeard,
		want.InSeqNum, want.OutSeqNum)
	if err != nil {
		return SeqState{}, false, fmt.Errorf("pg CAS seq %s: %w", want.SessionID, err)
	}
	if tag.RowsAffected() == 1 {
		next.SessionID = want.SessionID
		return next, true, nil
	}
	cur, lerr := s.Load(ctx, want.SessionID)
	if lerr != nil {
		return SeqState{}, false, lerr
	}
	return cur, false, nil
}

// Claim falls back to a heartbeat-lease conditional UPDATE: the claim
// lands when the row is absent (bootstrap INSERT), or the session is
// not ACTIVE, or the recorded heartbeat is older than the lease. See
// the type comment for the fencing-strength caveat.
func (s *PGSeqStore) Claim(ctx context.Context, sessionID, owner string, lease time.Duration, now time.Time) (SeqState, bool, error) {
	// Bootstrap: create the row if this session has no durable state.
	_, err := s.pool.Exec(ctx,
		`INSERT INTO fix_sessions (session_id, sender_seq_num, target_seq_num, status, last_heartbeat_at)
		 VALUES ($1, 1, 1, 'ACTIVE', $2)
		 ON CONFLICT (session_id) DO NOTHING`, sessionID, now)
	if err != nil {
		return SeqState{}, false, fmt.Errorf("pg claim bootstrap %s: %w", sessionID, err)
	}
	tag, err := s.pool.Exec(ctx,
		`UPDATE fix_sessions
		    SET status = 'ACTIVE', last_heartbeat_at = $2, updated_at = now()
		  WHERE session_id = $1
		    AND (status <> 'ACTIVE' OR last_heartbeat_at IS NULL
		         OR last_heartbeat_at < $3)`,
		sessionID, now, now.Add(-lease))
	if err != nil {
		return SeqState{}, false, fmt.Errorf("pg claim seq %s: %w", sessionID, err)
	}
	st, lerr := s.Load(ctx, sessionID)
	if lerr != nil {
		return SeqState{}, false, lerr
	}
	if tag.RowsAffected() == 1 {
		st.Owner = owner
		return st, true, nil
	}
	return st, false, nil
}

// Checkpoint mirrors `st` into fix_sessions as a high-water merge —
// restart seeding for the PG path must never regress sequence numbers
// (a low seed would fabricate ResendRequest storms on re-logon).
func (s *PGSeqStore) Checkpoint(ctx context.Context, st SeqState) error {
	_, err := s.pool.Exec(ctx,
		`INSERT INTO fix_sessions (session_id, sender_seq_num, target_seq_num, status, last_heartbeat_at)
		 VALUES ($1, $2, $3, 'ACTIVE', $4)
		 ON CONFLICT (session_id) DO UPDATE SET
		   sender_seq_num = GREATEST(fix_sessions.sender_seq_num, EXCLUDED.sender_seq_num),
		   target_seq_num = GREATEST(fix_sessions.target_seq_num, EXCLUDED.target_seq_num),
		   last_heartbeat_at = GREATEST(fix_sessions.last_heartbeat_at, EXCLUDED.last_heartbeat_at),
		   updated_at = now()`,
		st.SessionID, st.OutSeqNum, st.InSeqNum, st.LastHeard)
	if err != nil {
		return fmt.Errorf("pg checkpoint seq %s: %w", st.SessionID, err)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Layered store — Redis authoritative, PG fallback
// ---------------------------------------------------------------------------

// Checkpointer is the optional high-water-merge capability a fallback
// may expose (PGSeqStore does): Checkpoint persists `st` without ever
// regressing stored counters — restart seeding must not fabricate
// ResendRequest storms by writing a low seed.
type Checkpointer interface {
	Checkpoint(ctx context.Context, st SeqState) error
}

// FailoverStore layers the PG fallback under the Redis primary per
// Task 18.3.12 item 1: reads and writes hit Redis first; a Redis error
// (not a lost CAS race — that is semantic, not transport) transparently
// drops to fix_sessions. Successful Redis writes are checkpointed to PG
// lazily via Checkpoint — the session layer calls it on logon/logout
// and on a slow ticker, never on the per-message hot path.
type FailoverStore struct {
	primary  SeqStore
	fallback SeqStore // nil = no fallback (fail-closed on Redis loss)
	log      *slog.Logger
}

// NewFailoverStore binds primary + fallback. fallback may be nil —
// failover then fails closed when Redis is unreachable rather than
// fabricating sequence state.
func NewFailoverStore(primary SeqStore, fallback SeqStore, log *slog.Logger) *FailoverStore {
	return &FailoverStore{primary: primary, fallback: fallback, log: log}
}

// Load reads primary, falling back to fix_sessions on transport error.
func (s *FailoverStore) Load(ctx context.Context, sessionID string) (SeqState, error) {
	st, err := s.primary.Load(ctx, sessionID)
	if err == nil || errors.Is(err, ErrSessionNotFound) || s.fallback == nil {
		return st, err
	}
	s.warn("redis seq load failed; falling back to fix_sessions", sessionID, err)
	return s.fallback.Load(ctx, sessionID)
}

// CAS writes primary, falling back on transport error. A clean
// swapped=false is propagated as a race, never retried across stores.
func (s *FailoverStore) CAS(ctx context.Context, want, next SeqState) (SeqState, bool, error) {
	st, ok, err := s.primary.CAS(ctx, want, next)
	if err == nil {
		return st, ok, nil
	}
	if s.fallback == nil {
		return SeqState{}, false, err
	}
	s.warn("redis seq CAS failed; falling back to fix_sessions", want.SessionID, err)
	return s.fallback.CAS(ctx, want, next)
}

// Claim routes through the same layering.
func (s *FailoverStore) Claim(ctx context.Context, sessionID, owner string, lease time.Duration, now time.Time) (SeqState, bool, error) {
	st, ok, err := s.primary.Claim(ctx, sessionID, owner, lease, now)
	if err == nil {
		return st, ok, nil
	}
	if s.fallback == nil {
		return SeqState{}, false, err
	}
	s.warn("redis seq claim failed; falling back to fix_sessions", sessionID, err)
	return s.fallback.Claim(ctx, sessionID, owner, lease, now)
}

// Checkpoint mirrors state into the fallback when it supports the
// high-water merge (PGSeqStore does); no-op otherwise.
func (s *FailoverStore) Checkpoint(ctx context.Context, st SeqState) error {
	cp, ok := s.fallback.(Checkpointer)
	if !ok {
		return nil
	}
	return cp.Checkpoint(ctx, st)
}

func (s *FailoverStore) warn(msg, sessionID string, err error) {
	if s.log != nil {
		s.log.Warn(msg, "session", sessionID, "err", err)
	}
}

// ---------------------------------------------------------------------------
// In-flight execution deduplication (ClOrdID registry)
// ---------------------------------------------------------------------------

// DedupRecord is the ClOrdID→ExecutionReport registry entry. A replayed
// NewOrderSingle (PossDupFlag=Y) that resolves to a record echoes the
// stored report verbatim instead of re-submitting to the matching core.
type DedupRecord struct {
	ClOrdID   string `json:"clord_id"`
	ExecID    string `json:"exec_id,omitempty"`
	OrderID   int64  `json:"order_id,omitempty"`
	Report    []byte `json:"report,omitempty"` // marshalled outbound ExecutionReport(35=8)
	Pending   bool   `json:"pending"`          // claimed but the engine ack has not been bound yet
	CreatedAt int64  `json:"created_at_unix_ns"`
}

// ExecDedup is the duplicate-submission seam.
type ExecDedup interface {
	// ClaimOrGet atomically claims clOrdID for sessionID. claimed=true
	// means this caller owns first submission and must call Resolve with
	// the engine result; claimed=false returns the existing record (the
	// replay path — echo rec.Report, never resubmit).
	ClaimOrGet(ctx context.Context, sessionID, clOrdID string, ttl time.Duration) (rec DedupRecord, claimed bool, err error)
	// Resolve binds the execution outcome to a claimed clOrdID.
	Resolve(ctx context.Context, sessionID, clOrdID string, rec DedupRecord) error
}

// dedupKey — ClOrdID is unique per session per trading day; the 24h TTL
// covers the full session lifetime plus replay margin.
func dedupKey(sessionID, clOrdID string) string {
	return "fix:dedup:" + sessionID + ":" + clOrdID
}

// DefaultDedupTTL bounds the ClOrdID registry horizon.
const DefaultDedupTTL = 24 * time.Hour

// RedisExecDedup implements ExecDedup on the coordination cluster.
type RedisExecDedup struct {
	rdb *goredis.Client
}

// NewRedisExecDedup binds the registry.
func NewRedisExecDedup(rdb *goredis.Client) *RedisExecDedup {
	return &RedisExecDedup{rdb: rdb}
}

// ClaimOrGet uses SET NX PX — the claim and the first-seen check are one
// atomic round-trip; a lost claim reads back the winner's record.
func (d *RedisExecDedup) ClaimOrGet(ctx context.Context, sessionID, clOrdID string, ttl time.Duration) (DedupRecord, bool, error) {
	if ttl <= 0 {
		ttl = DefaultDedupTTL
	}
	key := dedupKey(sessionID, clOrdID)
	placeholder, err := json.Marshal(DedupRecord{
		ClOrdID: clOrdID, Pending: true, CreatedAt: time.Now().UnixNano(),
	})
	if err != nil {
		return DedupRecord{}, false, fmt.Errorf("fix: dedup marshal: %w", err)
	}
	ok, err := d.rdb.SetArgs(ctx, key, placeholder, goredis.SetArgs{
		Mode: "NX", TTL: ttl,
	}).Result()
	if err != nil && !errors.Is(err, goredis.Nil) {
		return DedupRecord{}, false, fmt.Errorf("redis dedup claim %s: %w", key, err)
	}
	if ok == "OK" {
		return DedupRecord{ClOrdID: clOrdID, Pending: true}, true, nil
	}
	raw, err := d.rdb.Get(ctx, key).Bytes()
	if err != nil {
		// Won the SET-NX race but the loser read after expiry — the
		// record vanished between commands; treat as claimed so the
		// submission proceeds rather than dropping an order (fail-safe
		// for the client, the engine-side idempotency fence is the
		// second layer).
		if errors.Is(err, goredis.Nil) {
			return DedupRecord{ClOrdID: clOrdID, Pending: true}, true, nil
		}
		return DedupRecord{}, false, fmt.Errorf("redis dedup read %s: %w", key, err)
	}
	var rec DedupRecord
	if err := json.Unmarshal(raw, &rec); err != nil {
		return DedupRecord{}, false, fmt.Errorf("redis dedup decode %s: %w", key, err)
	}
	return rec, false, nil
}

// dedupResolveScript binds the result only when the key still exists —
// a Resolve after TTL expiry must not resurrect a phantom claim.
var dedupResolveScript = goredis.NewScript(`
if redis.call('EXISTS', KEYS[1]) == 0 then
  return 0
end
redis.call('SET', KEYS[1], ARGV[1], 'KEEPTTL')
return 1
`)

// Resolve stores the outcome under the claimed key, keeping the TTL.
func (d *RedisExecDedup) Resolve(ctx context.Context, sessionID, clOrdID string, rec DedupRecord) error {
	rec.Pending = false
	raw, err := json.Marshal(rec)
	if err != nil {
		return fmt.Errorf("fix: dedup marshal: %w", err)
	}
	n, err := dedupResolveScript.Run(ctx, d.rdb, []string{dedupKey(sessionID, clOrdID)}, raw).Int()
	if err != nil {
		return fmt.Errorf("redis dedup resolve %s: %w", clOrdID, err)
	}
	if n == 0 {
		return fmt.Errorf("redis dedup resolve %s: claim expired before resolve", clOrdID)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Failover manager — ownership, Logon re-synchronization, dedup
// ---------------------------------------------------------------------------

// DefaultSessionLeaseTTL is the ownership lease for a claimed session —
// same epoch-lease discipline as engine:leader (§18.6.2): 2s lease,
// renewed well inside the window by the session layer.
const DefaultSessionLeaseTTL = 2 * time.Second

// ResyncAction is the Logon(35=A) re-synchronization verdict.
type ResyncAction int

const (
	// ResyncInSync — client MsgSeqNum == expected; resume normally.
	ResyncInSync ResyncAction = iota
	// ResyncIssueResendRequest — client is AHEAD of us: it sent messages
	// the dead primary never persisted. Emit ResendRequest(35=2)
	// BeginSeqNo=ExpectedIn, EndSeqNo=0 (through latest, FIX convention).
	ResyncIssueResendRequest
	// ResyncLowSeq — client is BEHIND us: it re-sent an already-consumed
	// sequence or mis-reset. Fail-closed default is to reject the logon
	// (ResyncRejectLogon); with RecoverLowSeq configured the session may
	// instead replay outbound traffic from the client's watermark.
	ResyncLowSeq
	// ResyncReset — ResetSeqNumFlag(141)=Y: both counters restart at 1.
	ResyncReset
)

// ResyncPlan is what the session layer must do next.
type ResyncPlan struct {
	Action     ResyncAction
	SessionID  string
	ExpectedIn int64 // gateway's next expected inbound MsgSeqNum
	BeginSeqNo int64 // ResendRequest begin (== ExpectedIn when issuing)
	EndSeqNo   int64 // ResendRequest end; 0 = through latest
	Owner      string
	Epoch      int64
}

// FailoverConfig tunes Failover behaviour.
type FailoverConfig struct {
	// GatewayID is this instance's fencing identity (hostname/Pod name).
	GatewayID string
	// LeaseTTL bounds the ownership claim; DefaultSessionLeaseTTL when 0.
	LeaseTTL time.Duration
	// RecoverLowSeq permits re-serving outbound traffic when a client
	// logs on below our expected inbound seq. Default false — a low seq
	// without ResetSeqNumFlag is a protocol violation and the logon is
	// refused (fail-closed, spec §2.7).
	RecoverLowSeq bool
	// DedupTTL for the ClOrdID registry; DefaultDedupTTL when 0.
	DedupTTL time.Duration
}

// Failover coordinates standby takeover and sequence re-synchronization.
type Failover struct {
	store SeqStore
	dedup ExecDedup // nil = dedup disabled (tests); production always wires RedisExecDedup
	cfg   FailoverConfig
	now   func() time.Time
}

// NewFailover binds the manager. now is injectable for tests.
func NewFailover(store SeqStore, dedup ExecDedup, cfg FailoverConfig, now func() time.Time) *Failover {
	if cfg.LeaseTTL <= 0 {
		cfg.LeaseTTL = DefaultSessionLeaseTTL
	}
	if cfg.DedupTTL <= 0 {
		cfg.DedupTTL = DefaultDedupTTL
	}
	if now == nil {
		now = time.Now
	}
	return &Failover{store: store, dedup: dedup, cfg: cfg, now: now}
}

// ClaimSession takes (or renews) the ownership lease for sessionID.
// ErrSessionOwnedElsewhere means a peer gateway still holds a live
// lease — the caller must not resume the session.
func (f *Failover) ClaimSession(ctx context.Context, sessionID string) (SeqState, error) {
	st, ok, err := f.store.Claim(ctx, sessionID, f.cfg.GatewayID, f.cfg.LeaseTTL, f.now())
	if err != nil {
		return SeqState{}, err
	}
	if !ok {
		return SeqState{}, fmt.Errorf("%w (owner %q)", ErrSessionOwnedElsewhere, st.Owner)
	}
	st.SessionID = sessionID
	return st, nil
}

// ResumeOnLogon evaluates a client Logon(35=A) received at this gateway
// after takeover (or cold start). It claims/renews ownership, loads the
// shared sequence state, and returns the resync verdict the session
// layer must act on:
//
//	client seq > expected  → ResyncIssueResendRequest (BeginSeqNo=expected, EndSeqNo=0)
//	client seq == expected → ResyncInSync
//	client seq < expected  → ResyncLowSeq (reject logon unless RecoverLowSeq)
//	141=Y                  → ResyncReset (counters restart at 1)
func (f *Failover) ResumeOnLogon(ctx context.Context, sessionID string, clientSeqNum int64, resetSeqNum bool) (ResyncPlan, error) {
	st, err := f.ClaimSession(ctx, sessionID)
	if err != nil {
		return ResyncPlan{}, err
	}
	plan := ResyncPlan{
		SessionID: sessionID, Owner: f.cfg.GatewayID, Epoch: st.Epoch,
	}
	if resetSeqNum {
		// 141=Y is the negotiated fresh start: counters restart at 1
		// (the first post-reset message carries MsgSeqNum=1).
		if err := f.resetSequences(ctx, sessionID); err != nil {
			return ResyncPlan{}, err
		}
		plan.Action = ResyncReset
		plan.ExpectedIn = 1
		return plan, nil
	}
	st, err = f.store.Load(ctx, sessionID)
	switch {
	case err != nil && !errors.Is(err, ErrSessionNotFound):
		return ResyncPlan{}, err
	case errors.Is(err, ErrSessionNotFound) || st.InSeqNum <= 0:
		// Cold start (no row, or a bare ownership hash from Claim):
		// seed 1/1 under a 0/0 want — the CAS treats absent counters
		// as zero-state, so a racing claim cannot clobber.
		var ok bool
		st, ok, err = f.store.CAS(ctx,
			SeqState{SessionID: sessionID},
			SeqState{SessionID: sessionID, InSeqNum: 1, OutSeqNum: 1,
				LastHeard: f.now().UTC(), Owner: f.cfg.GatewayID, Epoch: st.Epoch})
		if err != nil {
			return ResyncPlan{}, err
		}
		if !ok {
			// Lost the create race — reload what the winner wrote.
			st, err = f.store.Load(ctx, sessionID)
			if err != nil {
				return ResyncPlan{}, err
			}
		}
	}
	plan.ExpectedIn = st.InSeqNum
	switch {
	case clientSeqNum > st.InSeqNum:
		plan.Action = ResyncIssueResendRequest
		plan.BeginSeqNo = st.InSeqNum
		plan.EndSeqNo = 0 // FIX: 0 = resend through latest
	case clientSeqNum < st.InSeqNum:
		plan.Action = ResyncLowSeq
		plan.BeginSeqNo = clientSeqNum // caller may replay outbound from here when RecoverLowSeq
		plan.EndSeqNo = st.InSeqNum - 1
	default:
		plan.Action = ResyncInSync
	}
	return plan, nil
}

// resetSequences CASes both counters back to 1 on ResetSeqNumFlag=141=Y.
// Bounded retries: the only concurrent writer is a racing failover.
func (f *Failover) resetSequences(ctx context.Context, sessionID string) error {
	for attempt := 0; attempt < 3; attempt++ {
		cur, err := f.store.Load(ctx, sessionID)
		if errors.Is(err, ErrSessionNotFound) {
			cur = SeqState{SessionID: sessionID}
		} else if err != nil {
			return err
		}
		next := cur
		next.InSeqNum, next.OutSeqNum = 1, 1
		next.LastHeard = f.now().UTC()
		next.Owner = f.cfg.GatewayID
		_, ok, err := f.store.CAS(ctx, cur, next)
		if err != nil {
			return err
		}
		if ok {
			return nil
		}
	}
	return fmt.Errorf("fix: reset seq for %s lost CAS race 3 times", sessionID)
}

// RecordInbound advances the shared inbound seq when recvSeq matches the
// stored expectation. On mismatch it returns ErrInboundSeqMismatch with
// the loaded state — the caller feeds (recvSeq, expected) into
// AssessInbound for the §9.9 gap-resolution verdict.
func (f *Failover) RecordInbound(ctx context.Context, sessionID string, recvSeq int64) (SeqState, error) {
	for attempt := 0; attempt < 3; attempt++ {
		cur, err := f.store.Load(ctx, sessionID)
		if err != nil {
			return SeqState{}, err
		}
		if cur.InSeqNum != recvSeq {
			return cur, fmt.Errorf("%w: expected %d got %d",
				ErrInboundSeqMismatch, cur.InSeqNum, recvSeq)
		}
		next := cur
		next.InSeqNum = recvSeq + 1
		next.LastHeard = f.now().UTC()
		next.Owner = f.cfg.GatewayID
		st, ok, err := f.store.CAS(ctx, cur, next)
		if err != nil {
			return SeqState{}, err
		}
		if ok {
			return st, nil
		}
	}
	return SeqState{}, fmt.Errorf("fix: inbound seq for %s lost CAS race 3 times", sessionID)
}

// RecordOutbound advances the shared outbound seq after transmission.
func (f *Failover) RecordOutbound(ctx context.Context, sessionID string, sentSeq int64) (SeqState, error) {
	for attempt := 0; attempt < 3; attempt++ {
		cur, err := f.store.Load(ctx, sessionID)
		if err != nil {
			return SeqState{}, err
		}
		if cur.OutSeqNum != sentSeq {
			return cur, fmt.Errorf("fix: outbound seq mismatch for %s: expected %d got %d",
				sessionID, cur.OutSeqNum, sentSeq)
		}
		next := cur
		next.OutSeqNum = sentSeq + 1
		next.Owner = f.cfg.GatewayID
		st, ok, err := f.store.CAS(ctx, cur, next)
		if err != nil {
			return SeqState{}, err
		}
		if ok {
			return st, nil
		}
	}
	return SeqState{}, fmt.Errorf("fix: outbound seq for %s lost CAS race 3 times", sessionID)
}

// HandleNewOrder implements Task 18.3.12 item 5 — duplicate in-flight
// NewOrderSingle suppression:
//
//   - First sight of clOrdID (claimed=true): the caller submits to the
//     matching core, then ResolveDedup binds the outcome.
//   - Repeat sight (claimed=false, possDup or not): the caller echoes
//     rec.Report verbatim — it MUST NOT resubmit. A repeat without
//     PossDupFlag=Y is additionally a protocol violation the session
//     layer should flag with a session Reject (35=3).
func (f *Failover) HandleNewOrder(ctx context.Context, sessionID, clOrdID string, possDup bool) (DedupRecord, bool, error) {
	if f.dedup == nil {
		return DedupRecord{}, false, errors.New("fix: exec dedup registry not wired")
	}
	rec, claimed, err := f.dedup.ClaimOrGet(ctx, sessionID, clOrdID, f.cfg.DedupTTL)
	if err != nil {
		return DedupRecord{}, false, err
	}
	return rec, !claimed, nil
}

// ResolveDedup binds the engine result to a claimed ClOrdID.
func (f *Failover) ResolveDedup(ctx context.Context, sessionID, clOrdID string, rec DedupRecord) error {
	if f.dedup == nil {
		return errors.New("fix: exec dedup registry not wired")
	}
	return f.dedup.Resolve(ctx, sessionID, clOrdID, rec)
}

// ---------------------------------------------------------------------------
// Session row access — the sibling Store seam
// ---------------------------------------------------------------------------
//
// fix_sessions reads/writes for CoD and drain go through the sibling
// Store seam (types.go) / PgStore (store.go): SessionByID +
// SetStatus cover everything the resilience paths need, so this file
// defines no second session-row model. Session.AccountID == nil marks
// a drop-copy (read-only) session — it owns no orders and CoD is a
// no-op for it (spec §9.3).
