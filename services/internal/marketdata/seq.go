// Per-symbol sequence cursors (spec §10.2: `md:seq:{symbol}` in Redis;
// §10.7: the cursor survives restarts).
//
// The conflator's hot path allocates sequences from an in-memory counter
// seeded by Load; Store mirrors the cursor to Redis after each flush so
// the key never becomes a per-event round-trip on the feed path. A
// restart re-seeds from Redis — clients resuming with a stale cursor get
// the replay-or-resync contract, never a reset-to-0 storm.
package marketdata

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	goredis "github.com/redis/go-redis/v9"
)

// SeqKey is the canonical Redis key for a symbol's market-data cursor.
func SeqKey(symbol string) string { return "md:seq:" + symbol }

// SeqStore is the durable cursor seam.
type SeqStore interface {
	// Load returns the persisted cursor (0 = never emitted).
	Load(ctx context.Context, symbol string) (uint64, error)
	// Store persists the cursor.
	Store(ctx context.Context, symbol string, seq uint64) error
}

// RedisSeqStore implements SeqStore on the coordination Redis.
type RedisSeqStore struct {
	rdb *goredis.Client
}

// NewRedisSeqStore binds the store to a Redis client.
func NewRedisSeqStore(rdb *goredis.Client) *RedisSeqStore {
	return &RedisSeqStore{rdb: rdb}
}

// Load reads md:seq:{symbol}; a missing key means no events were ever
// emitted for the symbol → cursor 0.
func (s *RedisSeqStore) Load(ctx context.Context, symbol string) (uint64, error) {
	v, err := s.rdb.Get(ctx, SeqKey(symbol)).Uint64()
	if errors.Is(err, goredis.Nil) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("redis load seq %s: %w", symbol, err)
	}
	return v, nil
}

// seqMaxScript keeps md:seq:{symbol} a HIGH-WATER MARK: mirror writes run
// on fire-and-forget goroutines off the emit path and can land out of
// order — a stale write must never regress the cursor (a low seed on
// restart would fabricate ReplayInvalidSeq resyncs).
var seqMaxScript = goredis.NewScript(`
local cur = redis.call('GET', KEYS[1])
if cur and tonumber(cur) >= tonumber(ARGV[1]) then
  return 0
end
redis.call('SET', KEYS[1], ARGV[1])
return 1
`)

// Store writes md:seq:{symbol} as a high-water mark. SET-conditional (not
// INCR) because the conflator owns allocation in-process — Redis is the
// mirror, not the allocator.
func (s *RedisSeqStore) Store(ctx context.Context, symbol string, seq uint64) error {
	if err := seqMaxScript.Run(ctx, s.rdb, []string{SeqKey(symbol)},
		seq).Err(); err != nil {
		return fmt.Errorf("redis store seq %s: %w", symbol, err)
	}
	return nil
}

// MemSeqStore is the non-durable SeqStore — tests and embedded
// deployments. Behaviour is identical minus restart durability.
type MemSeqStore struct {
	mu    sync.Mutex
	bySym map[string]uint64
}

// NewMemSeqStore builds the in-memory store.
func NewMemSeqStore() *MemSeqStore {
	return &MemSeqStore{bySym: map[string]uint64{}}
}

// Load implements SeqStore.
func (s *MemSeqStore) Load(_ context.Context, symbol string) (uint64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.bySym[symbol], nil
}

// Store implements SeqStore as a high-water mark — stale async writes
// are ignored rather than erroring (mirrors RedisSeqStore's keep-max).
func (s *MemSeqStore) Store(_ context.Context, symbol string, seq uint64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if cur, ok := s.bySym[symbol]; ok && cur > seq {
		return nil // stale async write — keep the high-water mark
	}
	s.bySym[symbol] = seq
	return nil
}

// ---------------------------------------------------------------------------
// Gap journal (Task 6.3.22 item 1, spec §10.7): a bounded, durable record
// of sequence discontinuities per seq domain. The high-water cursor alone
// proves "no reset to 0"; the journal proves WHERE continuity broke (or
// could have broken) so resume/audit can distinguish a real gap from a
// cursor regression. Entries are append-only, newest-first, bounded.
// ---------------------------------------------------------------------------

// Gap journal reasons — stable vocabulary for operators and audits.
const (
	// GapRestartBoundary marks a process restart where the cursor
	// continued from the durable high-water mark (From=To=seeded seq).
	// No messages were lost in the seq domain; the entry documents the
	// resume-relevant continuity edge (the in-memory ring emptied).
	GapRestartBoundary = "restart_boundary"
	// GapInputSaturation marks delta seqs burned when the conflator
	// input queue saturated (fail-loud drop — the emitted envelope shows
	// the hole; this entry makes it durable across restarts).
	GapInputSaturation = "input_saturation"
	// GapSeedFailed marks a restart where the durable cursor could not
	// be loaded — local cursor began at 0 and any client seq above it
	// reads as an invalid-cursor resync.
	GapSeedFailed = "seed_failed"
	// GapResumeHorizon records a resume attempt whose cursor predated
	// the replay horizon (gap_too_large resync emitted).
	GapResumeHorizon = "resume_horizon_miss"
	// GapResumeInvalid records a resume attempt whose cursor was ahead
	// of the channel tail (invalid_sequence resync emitted).
	GapResumeInvalid = "invalid_cursor"
)

// SeqGap is one journal entry: the seq range implicated and why.
type SeqGap struct {
	From   uint64 `json:"from"`   // first affected seq (inclusive)
	To     uint64 `json:"to"`     // last affected seq (inclusive)
	Reason string `json:"reason"` // Gap* constants
	AtMs   int64  `json:"at_ms"`
}

// GapJournal is the durable gap-log seam (spec §10.7). key is the seq
// domain — a symbol for public L2 channels, "private:{channel}:{acct}"
// for private channels, a channel token for producer-agnostic domains.
type GapJournal interface {
	// Record appends one gap entry (bounded retention).
	Record(ctx context.Context, key string, g SeqGap) error
	// Recent returns the newest entries, most recent first.
	Recent(ctx context.Context, key string, limit int) ([]SeqGap, error)
}

// gapJournalCap bounds retained entries per domain (§10.7 "bounded gap
// log"). 1024 entries ≪ one key per symbol per restart cadence.
const gapJournalCap = 1024

// GapKey derives the journal key for a channel token — the seq domain
// lives behind the channel name. Public depth variants share the
// symbol's domain, so callers may pass either.
func GapKey(channel string) string { return "md:gaps:" + channel }

// RedisGapJournal implements GapJournal on the coordination Redis:
// md:gaps:{key} is a newest-first list, JSON entries, LTRIM-bounded.
type RedisGapJournal struct {
	rdb *goredis.Client
}

// NewRedisGapJournal binds the journal to a Redis client.
func NewRedisGapJournal(rdb *goredis.Client) *RedisGapJournal {
	return &RedisGapJournal{rdb: rdb}
}

// gapPushScript appends and trims atomically so a crash between LPUSH
// and LTRIM can never grow the list past the cap.
var gapPushScript = goredis.NewScript(`
redis.call('LPUSH', KEYS[1], ARGV[1])
redis.call('LTRIM', KEYS[1], 0, tonumber(ARGV[2]) - 1)
return 1
`)

// Record implements GapJournal.
func (j *RedisGapJournal) Record(ctx context.Context, key string, g SeqGap) error {
	b, err := json.Marshal(g)
	if err != nil {
		return fmt.Errorf("gap journal marshal: %w", err)
	}
	if err := gapPushScript.Run(ctx, j.rdb, []string{GapKey(key)},
		string(b), gapJournalCap).Err(); err != nil {
		return fmt.Errorf("redis gap record %s: %w", key, err)
	}
	return nil
}

// Recent implements GapJournal.
func (j *RedisGapJournal) Recent(ctx context.Context, key string, limit int) ([]SeqGap, error) {
	if limit <= 0 || limit > gapJournalCap {
		limit = gapJournalCap
	}
	raw, err := j.rdb.LRange(ctx, GapKey(key), 0, int64(limit-1)).Result()
	if err != nil {
		return nil, fmt.Errorf("redis gap recent %s: %w", key, err)
	}
	out := make([]SeqGap, 0, len(raw))
	for _, s := range raw {
		var g SeqGap
		if json.Unmarshal([]byte(s), &g) == nil {
			out = append(out, g)
		}
	}
	return out, nil
}

// MemGapJournal is the non-durable journal — tests and embedded wiring.
type MemGapJournal struct {
	mu    sync.Mutex
	byKey map[string][]SeqGap // newest-first
}

// NewMemGapJournal builds the in-memory journal.
func NewMemGapJournal() *MemGapJournal {
	return &MemGapJournal{byKey: map[string][]SeqGap{}}
}

// Record implements GapJournal.
func (j *MemGapJournal) Record(_ context.Context, key string, g SeqGap) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	l := append([]SeqGap{g}, j.byKey[key]...)
	if len(l) > gapJournalCap {
		l = l[:gapJournalCap]
	}
	j.byKey[key] = l
	return nil
}

// Recent implements GapJournal.
func (j *MemGapJournal) Recent(_ context.Context, key string, limit int) ([]SeqGap, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	l := j.byKey[key]
	if limit > 0 && len(l) > limit {
		l = l[:limit]
	}
	out := make([]SeqGap, len(l))
	copy(out, l)
	return out, nil
}

// noteGap is the fire-and-forget journal write used on hot paths — a
// journal failure is logged, never fatal (the cursor contract is the
// durable one; the journal is audit evidence).
func noteGap(j GapJournal, log *slog.Logger, key string, g SeqGap) {
	if j == nil {
		return
	}
	if g.AtMs == 0 {
		g.AtMs = time.Now().UnixMilli()
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := j.Record(ctx, key, g); err != nil && log != nil {
		log.Warn("marketdata: gap journal write failed", "key", key, "err", err)
	}
}
