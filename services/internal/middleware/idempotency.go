// Task 5.3.42 item 2 — cross-endpoint idempotency middleware.
//
// The Idempotency-Key header is REQUIRED on the money-moving POSTs
// (spec §8.8 + the task list): /withdrawals, /transfers,
// /funding/bank-accounts, /orders/batch and /funding/deposits (the task
// prose omits deposits but spec §8.8 names it — both enforced, the
// stricter reading). Keys must be UUIDv7, are account-scoped
// (Redis key `idem:{account_id}:{idempotency_key}` — global unscoped
// keys are prohibited), and carry a 24h dedup window.
//
// Semantics: same key + same payload replays the stored response
// verbatim (header Idempotency-Replayed: true); same key + different
// payload rejects IDEMPOTENCY_KEY_MISMATCH (HTTP 422); a still-pending
// key is IDEMPOTENCY_KEY_COLLISION (409). 5xx responses release the
// claim so a genuine retry can re-execute — replaying a transient
// failure would be worse than the dedup gap it prevents.
//
// Fail-closed (spec §2.7): store errors emit SERVICE_DEGRADED rather
// than letting a money-moving request run unprotected.
package middleware

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	goredis "github.com/redis/go-redis/v9"

	"exchange/internal/auth"
)

// IdempotencyTTL is the §8.8 dedup window.
const IdempotencyTTL = 24 * time.Hour

// IdemKeyHeader is the required request header.
const IdemKeyHeader = "Idempotency-Key"

// IdemReplayedHeader marks replayed responses.
const IdemReplayedHeader = "Idempotency-Replayed"

// IdemRoute is one (method, exact-path) pair that requires the header.
type IdemRoute struct {
	Method string
	Path   string
}

// IdempotencyRequired is the task+spec union of protected routes.
var IdempotencyRequired = []IdemRoute{
	{Method: http.MethodPost, Path: "/api/v1/withdrawals"},
	{Method: http.MethodPost, Path: "/api/v1/transfers"},
	{Method: http.MethodPost, Path: "/api/v1/funding/bank-accounts"},
	{Method: http.MethodPost, Path: "/api/v1/orders/batch"},
	{Method: http.MethodPost, Path: "/api/v1/funding/deposits"},
}

// ---------------------------------------------------------------------------
// Store seam
// ---------------------------------------------------------------------------

// IdemState is the Begin verdict.
type IdemState int

const (
	IdemProceed  IdemState = iota // caller owns the slot
	IdemReplay                    // completed record, same payload
	IdemMismatch                  // same key, different payload → 422
	IdemInFlight                  // same key+payload, still pending → 409
)

// IdemResult is Begin's return.
type IdemResult struct {
	State       IdemState
	HTTPStatus  int
	Body        []byte
	ContentType string
}

// IdempotencyStore is the dedup ledger seam. Redis is the production
// store (spec §8.8 keyshape); PGIdemStore is the durable variant backed
// by the §8.8 idempotency_keys table (migration 154). Implementations
// must make Begin atomic. endpoint is the canonical "METHOD /path" tag
// persisted for operator forensics.
type IdempotencyStore interface {
	Begin(ctx context.Context, key, endpoint, payloadHash string, ttl time.Duration) (IdemResult, error)
	// Complete stores the terminal response; status 0 releases the
	// pending claim (transient-failure retry path).
	Complete(ctx context.Context, key, payloadHash string, httpStatus int, contentType string, body []byte) error
}

// IdemKey builds the canonical account-scoped key.
func IdemKey(accountID int64, key string) string {
	return "idem:" + strconv.FormatInt(accountID, 10) + ":" + key
}

// ---------------------------------------------------------------------------
// Redis store
// ---------------------------------------------------------------------------

// idemRecord is the stored ledger value. Body is a string (not []byte):
// the Lua complete script writes ARGV[2] via cjson, and a Go []byte
// field would expect base64 — the response bytes must survive verbatim.
type idemRecord struct {
	PH          string `json:"ph"`
	Done        bool   `json:"done"`
	HTTPStatus  int    `json:"http_status,omitempty"`
	Body        string `json:"body,omitempty"`
	ContentType string `json:"ct,omitempty"`
}

// RedisIdemStore keeps the ledger on the coordination Redis.
type RedisIdemStore struct {
	rdb *goredis.Client
}

// NewRedisIdemStore binds the store to a Redis client.
func NewRedisIdemStore(rdb *goredis.Client) *RedisIdemStore {
	return &RedisIdemStore{rdb: rdb}
}

var idemCompleteScript = goredis.NewScript(`
local v = redis.call('GET', KEYS[1])
if not v then return 0 end
local rec = cjson.decode(v)
if rec.ph ~= ARGV[1] then return -1 end
if ARGV[2] == '' then
  redis.call('DEL', KEYS[1])
else
  rec.done = true
  rec.http_status = tonumber(ARGV[3])
  rec.ct = ARGV[4]
  rec.body = ARGV[2]
  redis.call('SET', KEYS[1], cjson.encode(rec), 'KEEPTTL')
end
return 1
`)

func (s *RedisIdemStore) Begin(ctx context.Context, key, _ string, payloadHash string, ttl time.Duration) (IdemResult, error) {
	pending, _ := json.Marshal(idemRecord{PH: payloadHash})
	ok, err := s.rdb.SetNX(ctx, key, pending, ttl).Result()
	if err != nil {
		return IdemResult{}, fmt.Errorf("idem begin %s: %w", key, err)
	}
	if ok {
		return IdemResult{State: IdemProceed}, nil
	}
	raw, err := s.rdb.Get(ctx, key).Bytes()
	if err == goredis.Nil {
		ok, err = s.rdb.SetNX(ctx, key, pending, ttl).Result()
		if err != nil {
			return IdemResult{}, fmt.Errorf("idem re-begin %s: %w", key, err)
		}
		if ok {
			return IdemResult{State: IdemProceed}, nil
		}
		return IdemResult{State: IdemInFlight}, nil
	}
	if err != nil {
		return IdemResult{}, fmt.Errorf("idem read %s: %w", key, err)
	}
	var rec idemRecord
	if err := json.Unmarshal(raw, &rec); err != nil {
		return IdemResult{State: IdemInFlight}, nil // corrupt → fail closed
	}
	if rec.PH != payloadHash {
		return IdemResult{State: IdemMismatch}, nil
	}
	if rec.Done {
		return IdemResult{State: IdemReplay, HTTPStatus: rec.HTTPStatus,
			Body: []byte(rec.Body), ContentType: rec.ContentType}, nil
	}
	return IdemResult{State: IdemInFlight}, nil
}

func (s *RedisIdemStore) Complete(ctx context.Context, key, payloadHash string, httpStatus int, contentType string, body []byte) error {
	var bodyStr string
	if httpStatus != 0 {
		bodyStr = string(body)
	}
	res, err := idemCompleteScript.Run(ctx, s.rdb, []string{key},
		payloadHash, bodyStr, httpStatus, contentType).Int()
	if err != nil {
		return fmt.Errorf("idem complete %s: %w", key, err)
	}
	if res != 1 {
		return fmt.Errorf("idem complete %s: claim missing or payload drift", key)
	}
	return nil
}

// ---------------------------------------------------------------------------
// PostgreSQL store — durable ledger on the §8.8 idempotency_keys table
// (migration 154). Schema fit:
//   status_code = 0      → pending claim (inserted by Begin)
//   status_code > 0      → completed response, replayable
//   response_body        → JSONB; 'null' while pending; completed rows
//                          always wrap the bytes as
//                          {"raw": "<text>", "content_type": "<ct>"} —
//                          JSONB normalization (whitespace/key order)
//                          would otherwise corrupt byte-verbatim replay
//   created_at + ttl     → rows outside the 24h window are dead weight
//                          for the purge job AND are reaped in-line here
// The PRIMARY KEY hit IS the dedup check (23505 = concurrent claim or
// stored verdict).
// ---------------------------------------------------------------------------

// PGIdemStore is the durable IdempotencyStore.
type PGIdemStore struct {
	pool *pgxpool.Pool
}

// NewPGIdemStore binds the durable store.
func NewPGIdemStore(pool *pgxpool.Pool) *PGIdemStore { return &PGIdemStore{pool: pool} }

// splitIdemKey parses "idem:{account_id}:{uuid}".
func splitIdemKey(key string) (int64, string, error) {
	parts := strings.SplitN(key, ":", 3)
	if len(parts) != 3 || parts[0] != "idem" {
		return 0, "", fmt.Errorf("idem key %q malformed", key)
	}
	accountID, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil {
		return 0, "", fmt.Errorf("idem key %q bad account", key)
	}
	return accountID, parts[2], nil
}

func (s *PGIdemStore) Begin(ctx context.Context, key, endpoint, payloadHash string, ttl time.Duration) (IdemResult, error) {
	accountID, idemKey, err := splitIdemKey(key)
	if err != nil {
		return IdemResult{}, err
	}
	tag, err := s.pool.Exec(ctx, `
		INSERT INTO idempotency_keys
		    (account_id, idempotency_key, endpoint, request_hash,
		     status_code, response_body)
		VALUES ($1, $2, $3, $4, 0, 'null'::jsonb)
		ON CONFLICT (account_id, idempotency_key) DO NOTHING`,
		accountID, idemKey, endpoint, payloadHash)
	if err != nil {
		return IdemResult{}, fmt.Errorf("idem insert: %w", err)
	}
	if tag.RowsAffected() == 1 {
		return IdemResult{State: IdemProceed}, nil
	}
	var ph string
	var status int
	var body []byte
	err = s.pool.QueryRow(ctx, `
		SELECT TRIM(request_hash), status_code, response_body::text
		  FROM idempotency_keys
		 WHERE account_id=$1 AND idempotency_key=$2
		   AND created_at > now() - $3::interval`,
		accountID, idemKey, ttl.String()).Scan(&ph, &status, &body)
	if err == pgx.ErrNoRows {
		// Expired row holds the PK — reap it and retry the claim once.
		if _, err := s.pool.Exec(ctx, `
			DELETE FROM idempotency_keys
			 WHERE account_id=$1 AND idempotency_key=$2
			   AND created_at <= now() - $3::interval`,
			accountID, idemKey, ttl.String()); err != nil {
			return IdemResult{}, fmt.Errorf("idem expire-stale: %w", err)
		}
		return s.Begin(ctx, key, endpoint, payloadHash, ttl)
	}
	if err != nil {
		return IdemResult{}, fmt.Errorf("idem select: %w", err)
	}
	if ph != payloadHash {
		return IdemResult{State: IdemMismatch}, nil
	}
	if status == 0 {
		return IdemResult{State: IdemInFlight}, nil
	}
	// Completed row: response_body is either the verbatim JSON document
	// or {"raw": "<text>", "content_type": "<ct>"} for non-JSON bodies.
	var doc struct {
		Raw         *string `json:"raw"`
		ContentType string  `json:"content_type"`
	}
	if json.Unmarshal(body, &doc) == nil && doc.Raw != nil {
		return IdemResult{State: IdemReplay, HTTPStatus: status,
			Body: []byte(*doc.Raw), ContentType: doc.ContentType}, nil
	}
	return IdemResult{State: IdemReplay, HTTPStatus: status,
		Body: body, ContentType: "application/json"}, nil
}

func (s *PGIdemStore) Complete(ctx context.Context, key, payloadHash string, httpStatus int, contentType string, body []byte) error {
	accountID, idemKey, err := splitIdemKey(key)
	if err != nil {
		return err
	}
	if httpStatus == 0 { // release the claim (5xx/overflow path)
		tag, err := s.pool.Exec(ctx, `
			DELETE FROM idempotency_keys
			 WHERE account_id=$1 AND idempotency_key=$2
			   AND request_hash=$3 AND status_code=0`,
			accountID, idemKey, payloadHash)
		if err != nil {
			return fmt.Errorf("idem release: %w", err)
		}
		if tag.RowsAffected() == 0 {
			return fmt.Errorf("idem release %s: claim missing or payload drift", key)
		}
		return nil
	}
	// The body is ALWAYS stored wrapped — JSONB normalization (whitespace,
	// key order) would otherwise corrupt byte-verbatim replay.
	doc := map[string]any{"raw": string(body), "content_type": contentType}
	tag, err := s.pool.Exec(ctx, `
		UPDATE idempotency_keys
		   SET status_code=$4, response_body=$5::jsonb
		 WHERE account_id=$1 AND idempotency_key=$2
		   AND request_hash=$3 AND status_code=0`,
		accountID, idemKey, payloadHash, httpStatus, doc)
	if err != nil {
		return fmt.Errorf("idem complete: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("idem complete %s: claim missing or payload drift", key)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Middleware
// ---------------------------------------------------------------------------

// idemMaxReplay caps the buffered response we are willing to store.
const idemMaxReplay = 1 << 20 // 1MB — same bound as the body limiter

// captureWriter writes through to the real ResponseWriter while keeping
// a bounded copy for the dedup ledger.
type captureWriter struct {
	http.ResponseWriter
	status      int
	buf         bytes.Buffer
	overflowed  bool
	wroteHeader bool
}

func (w *captureWriter) WriteHeader(code int) {
	if !w.wroteHeader {
		w.status = code
		w.wroteHeader = true
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *captureWriter) Write(p []byte) (int, error) {
	if !w.wroteHeader {
		w.status = http.StatusOK
		w.wroteHeader = true
	}
	if w.buf.Len()+len(p) <= idemMaxReplay {
		w.buf.Write(p)
	} else {
		w.overflowed = true
	}
	return w.ResponseWriter.Write(p)
}

// Unwrap exposes the real writer (ResponseController / http.Hijacker).
func (w *captureWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// IsUUIDv7 enforces the §8.8 key format: canonical UUID text with the
// version nibble '7' and RFC 4122 variant bits.
func IsUUIDv7(s string) bool {
	if len(s) != 36 {
		return false
	}
	for i, r := range s {
		switch i {
		case 8, 13, 18, 23:
			if r != '-' {
				return false
			}
		case 14:
			if r != '7' {
				return false
			}
		case 19:
			if r != '8' && r != '9' && r != 'a' && r != 'b' &&
				r != 'A' && r != 'B' {
				return false
			}
		default:
			if !((r >= '0' && r <= '9') || (r >= 'a' && r <= 'f') || (r >= 'A' && r <= 'F')) {
				return false
			}
		}
	}
	return true
}

// AccountResolver resolves the dedup account scope for a request. The
// middleware runs BEFORE the handler's own auth (Bearer claims from a
// preceding auth middleware are read from context; endpoints that also
// accept X-API-KEY signed requests get their account via the resolver —
// a READ-ONLY key lookup, never Verify, so the handler's replay-guarded
// verification is not double-consumed). ok=false rejects UNAUTHORIZED:
// these routes are all authenticated money-moving surfaces anyway.
type AccountResolver func(ctx context.Context, r *http.Request) (int64, bool)

// ClaimsAccountResolver is the default resolver: Bearer-derived claims
// only (auth middleware must precede Idempotency in the chain).
func ClaimsAccountResolver(_ context.Context, r *http.Request) (int64, bool) {
	c := auth.ClaimsFrom(r.Context())
	if c != nil && c.AccountID != 0 {
		return c.AccountID, true
	}
	return 0, false
}

// Idempotency returns middleware enforcing the header on the required
// route set. emit resolves §23 envelopes (gateway.Router.WriteError in
// production; nil → local writer).
func Idempotency(store IdempotencyStore, routes []IdemRoute, resolve AccountResolver, emit ErrorEmitter) func(http.Handler) http.Handler {
	if routes == nil {
		routes = IdempotencyRequired
	}
	protected := map[string]bool{}
	for _, rt := range routes {
		protected[rt.Method+" "+rt.Path] = true
	}
	if emit == nil {
		emit = defaultEmit
	}
	if resolve == nil {
		resolve = ClaimsAccountResolver
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			path := strings.TrimSuffix(r.URL.Path, "/")
			if !protected[r.Method+" "+path] {
				next.ServeHTTP(w, r)
				return
			}

			accountID, ok := resolve(r.Context(), r)
			if !ok || accountID == 0 {
				emit(w, r, "UNAUTHORIZED", "authentication required", nil)
				return
			}

			key := r.Header.Get(IdemKeyHeader)
			if key == "" {
				emit(w, r, "INVALID_REQUEST",
					"Idempotency-Key header is required on this endpoint",
					map[string]any{"required_header": IdemKeyHeader})
				return
			}
			if !IsUUIDv7(key) {
				emit(w, r, "INVALID_REQUEST",
					"Idempotency-Key must be a UUIDv7", nil)
				return
			}

			// Payload hash covers method+path+body — a key replayed
			// against a different endpoint mismatches like a payload
			// change would (cross-endpoint replays are prohibited).
			var body []byte
			if r.Body != nil {
				b, err := io.ReadAll(io.LimitReader(r.Body, idemMaxReplay+1))
				if err != nil || len(b) > idemMaxReplay {
					emit(w, r, "INVALID_REQUEST", "request body unreadable", nil)
					return
				}
				body = b
				r.Body = io.NopCloser(bytes.NewReader(b))
			}
			h := sha256.New()
			h.Write([]byte(r.Method))
			h.Write([]byte{0})
			h.Write([]byte(path))
			h.Write([]byte{0})
			h.Write(body)
			ph := hex.EncodeToString(h.Sum(nil))

			storeKey := IdemKey(accountID, key)
			res, err := store.Begin(r.Context(), storeKey,
				r.Method+" "+path, ph, IdempotencyTTL)
			if err != nil {
				emit(w, r, "SERVICE_DEGRADED", "idempotency store unavailable", nil)
				return
			}
			switch res.State {
			case IdemMismatch:
				emit(w, r, "IDEMPOTENCY_KEY_MISMATCH",
					"Idempotency-Key was already used with a different payload", nil)
				return
			case IdemInFlight:
				emit(w, r, "IDEMPOTENCY_KEY_COLLISION",
					"Idempotency-Key request still in flight", nil)
				return
			case IdemReplay:
				if res.ContentType != "" {
					w.Header().Set("Content-Type", res.ContentType)
				}
				w.Header().Set(IdemReplayedHeader, "true")
				w.WriteHeader(res.HTTPStatus)
				_, _ = w.Write(res.Body)
				return
			}

			cw := &captureWriter{ResponseWriter: w, status: http.StatusOK}
			next.ServeHTTP(cw, r)

			// 5xx releases the claim — a retriable server fault must not
			// poison the 24h key window.
			status := cw.status
			if status >= 500 || cw.overflowed {
				status = 0
			}
			if err := store.Complete(r.Context(), storeKey, ph, status,
				cw.Header().Get("Content-Type"), cw.buf.Bytes()); err != nil {
				// Ledger write failed — the response already went out.
				// The window degrades to "in-flight" for the remaining
				// TTL, which is fail-closed (blocks silent double-exec).
				slog.Default().Warn("idempotency complete failed",
					"key", storeKey, "err", err)
			}
		})
	}
}
