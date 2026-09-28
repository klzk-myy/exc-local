package ws

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	goredis "github.com/redis/go-redis/v9"
)

// DedupState is the outcome of claiming a request_id inside the Task
// 5.3.42 WS dedup window (60s).
type DedupState int

const (
	// DedupProceed — the caller owns the slot; dispatch and then Complete.
	DedupProceed DedupState = iota
	// DedupReplay — a completed record exists for the same payload;
	// Replay carries the verbatim stored response frame to re-emit.
	DedupReplay
	// DedupMismatch — same request_id with a different payload; reject
	// IDEMPOTENCY_KEY_MISMATCH (HTTP-mapped 422).
	DedupMismatch
	// DedupInFlight — same request_id and payload but the original
	// dispatch is still running; reject IDEMPOTENCY_KEY_COLLISION (409).
	DedupInFlight
)

// DedupResult is the Begin verdict.
type DedupResult struct {
	State  DedupState
	Replay []byte // populated when State == DedupReplay
}

// DedupStore is the WS request_id dedup seam. Keys are namespaced
// "idem:ws:{identity}:{request_id}" where identity is account-scoped
// (spec §8.8 key-shape). Implementations must be atomic under concurrent
// Begin for the same key.
type DedupStore interface {
	Begin(ctx context.Context, key, payloadHash string, ttl time.Duration) (DedupResult, error)
	// Complete stores the terminal response frame for replay; a nil
	// frame releases the pending claim (internal-error retries allowed).
	Complete(ctx context.Context, key, payloadHash string, frame []byte) error
}

// DedupKey builds the canonical Redis key shape.
func DedupKey(namespace, requestID string) string {
	return "idem:ws:" + namespace + ":" + requestID
}

// PayloadHash fingerprints action+params for the same-key-same-payload
// comparison (Task 5.3.42 item 2).
func PayloadHash(action string, params []byte) string {
	sum := sha256.Sum256(append(append([]byte(action), 0), params...))
	return hex.EncodeToString(sum[:])
}

// ---------------------------------------------------------------------------
// Redis implementation (production)
// ---------------------------------------------------------------------------

// dedupRecord is the value stored under the claim key. Resp is a string
// (not []byte): the Lua round-trip stores it via cjson, and a Go []byte
// field would expect base64 — the frame text must survive verbatim.
type dedupRecord struct {
	PH   string `json:"ph"`             // payload hash
	Done bool   `json:"done"`           // terminal frame stored
	Resp string `json:"resp,omitempty"` // stored response frame (verbatim JSON text)
}

// RedisDedupStore implements DedupStore on the coordination Redis.
type RedisDedupStore struct {
	rdb *goredis.Client
}

// NewRedisDedupStore binds the store to a Redis client.
func NewRedisDedupStore(rdb *goredis.Client) *RedisDedupStore {
	return &RedisDedupStore{rdb: rdb}
}

// dedupCompleteScript atomically compares the payload hash and either
// stores the terminal frame (KEEPTTL preserves the 60s window) or deletes
// the claim when frame is empty (abort path). Returns 1 when applied.
var dedupCompleteScript = goredis.NewScript(`
local v = redis.call('GET', KEYS[1])
if not v then return 0 end
local rec = cjson.decode(v)
if rec.ph ~= ARGV[1] then return -1 end
if ARGV[2] == '' then
  redis.call('DEL', KEYS[1])
else
  rec.done = true
  rec.resp = ARGV[2]
  redis.call('SET', KEYS[1], cjson.encode(rec), 'KEEPTTL')
end
return 1
`)

// Begin claims the request slot. SET NX is atomic; on collision GET the
// existing record to classify replay/mismatch/in-flight.
func (s *RedisDedupStore) Begin(ctx context.Context, key, payloadHash string, ttl time.Duration) (DedupResult, error) {
	pending, _ := json.Marshal(dedupRecord{PH: payloadHash})
	ok, err := s.rdb.SetNX(ctx, key, pending, ttl).Result()
	if err != nil {
		return DedupResult{}, fmt.Errorf("ws dedup begin %s: %w", key, err)
	}
	if ok {
		return DedupResult{State: DedupProceed}, nil
	}
	raw, err := s.rdb.Get(ctx, key).Bytes()
	if err == goredis.Nil {
		// Expired between SETNX and GET — safe to proceed by retrying the
		// claim inline so the caller never spins.
		ok, err = s.rdb.SetNX(ctx, key, pending, ttl).Result()
		if err != nil {
			return DedupResult{}, fmt.Errorf("ws dedup re-begin %s: %w", key, err)
		}
		if ok {
			return DedupResult{State: DedupProceed}, nil
		}
		return DedupResult{State: DedupInFlight}, nil
	}
	if err != nil {
		return DedupResult{}, fmt.Errorf("ws dedup read %s: %w", key, err)
	}
	var rec dedupRecord
	if err := json.Unmarshal(raw, &rec); err != nil {
		// Corrupt record — fail closed as in-flight rather than
		// double-dispatching an order.
		return DedupResult{State: DedupInFlight}, nil
	}
	if rec.PH != payloadHash {
		return DedupResult{State: DedupMismatch}, nil
	}
	if rec.Done {
		return DedupResult{State: DedupReplay, Replay: []byte(rec.Resp)}, nil
	}
	return DedupResult{State: DedupInFlight}, nil
}

// Complete stores the terminal frame (or releases the claim on nil).
func (s *RedisDedupStore) Complete(ctx context.Context, key, payloadHash string, frame []byte) error {
	res, err := dedupCompleteScript.Run(ctx, s.rdb, []string{key}, payloadHash, string(frame)).Int()
	if err != nil {
		return fmt.Errorf("ws dedup complete %s: %w", key, err)
	}
	if res != 1 {
		return fmt.Errorf("ws dedup complete %s: claim missing or payload drift", key)
	}
	return nil
}

// ---------------------------------------------------------------------------
// In-memory implementation (tests, embedded deployments)
// ---------------------------------------------------------------------------

type memDedupEntry struct {
	ph      string
	done    bool
	resp    []byte
	expires time.Time
}

// MemDedupStore is the non-durable DedupStore — test seam and a
// correctness-complete fallback when Redis is unavailable at wiring time.
type MemDedupStore struct {
	mu    sync.Mutex
	now   func() time.Time
	byKey map[string]*memDedupEntry
}

// NewMemDedupStore builds the in-memory store.
func NewMemDedupStore() *MemDedupStore {
	return &MemDedupStore{now: time.Now, byKey: map[string]*memDedupEntry{}}
}

func (s *MemDedupStore) Begin(_ context.Context, key, payloadHash string, ttl time.Duration) (DedupResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if e, ok := s.byKey[key]; ok {
		if s.now().Before(e.expires) {
			if e.ph != payloadHash {
				return DedupResult{State: DedupMismatch}, nil
			}
			if e.done {
				return DedupResult{State: DedupReplay, Replay: e.resp}, nil
			}
			return DedupResult{State: DedupInFlight}, nil
		}
		delete(s.byKey, key) // expired — reclaim slot
	}
	s.byKey[key] = &memDedupEntry{ph: payloadHash, expires: s.now().Add(ttl)}
	return DedupResult{State: DedupProceed}, nil
}

func (s *MemDedupStore) Complete(_ context.Context, key, payloadHash string, frame []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.byKey[key]
	if !ok || e.ph != payloadHash {
		return fmt.Errorf("ws dedup complete %s: claim missing or payload drift", key)
	}
	if frame == nil {
		delete(s.byKey, key)
		return nil
	}
	e.done = true
	e.resp = frame
	return nil
}
