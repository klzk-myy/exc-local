// Package deprecation implements Phase-05 Task 5.3.20 — the enforced
// API deprecation policy.
//
// Contract (spec §8.6, §24 #91):
//   - an endpoint is announced deprecated at announced_at and sunsets at
//     sunset_at ≥ announced_at + 6 months (DB CHECK, migration 182);
//   - between announce and sunset every matching response carries
//     `Deprecation: <unix>` and `Sunset: <HTTP-date>` (RFC 8594 /
//     draft-ietf-httpapi-deprecation-header) plus a `Link:
//     </developer/migration>; rel="deprecation"` pointer;
//   - at/after sunset the endpoint is gone — the middleware answers a
//     deterministic 410 ENDPOINT_GONE problem body before the handler;
//   - a rule with method NULL covers every method on the path;
//     match_prefix rules cover subtrees.
//
// The middleware wraps the mux and consults a cached rule set; rules
// come from api_deprecations (PG) or a static seed for tests.
package deprecation

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// MinNotice is the published policy floor — 6 months announce→sunset.
const MinNotice = 183 * 24 * time.Hour // ~6 months; the DB CHECK is the real guard

// Rule is one api_deprecations row.
type Rule struct {
	ID           int64     `json:"id"`
	Method       *string   `json:"method,omitempty"` // nil = all methods
	Path         string    `json:"path"`
	MatchPrefix  bool      `json:"match_prefix"`
	AnnouncedAt  time.Time `json:"announced_at"`
	SunsetAt     time.Time `json:"sunset_at"`
	Replacement  *string   `json:"replacement,omitempty"`
	MigrationURL string    `json:"migration_url"`
	Notice       *string   `json:"notice,omitempty"`
	CreatedBy    int64     `json:"created_by"`
	CreatedAt    time.Time `json:"created_at"`
}

// Store loads and writes api_deprecations rows.
type Store struct {
	pool *pgxpool.Pool
	now  func() time.Time
}

// NewStore binds the store.
func NewStore(pool *pgxpool.Pool) (*Store, error) {
	if pool == nil {
		return nil, fmt.Errorf("deprecation: nil pool")
	}
	return &Store{pool: pool, now: time.Now}, nil
}

// SetClockForTest overrides the clock; tests only.
func (s *Store) SetClockForTest(now func() time.Time) { s.now = now }

// Validate enforces the six-month notice floor before insert — the DB
// CHECK is the backstop; this produces a clean error.
func (s *Store) Validate(r Rule) error {
	if !strings.HasPrefix(r.Path, "/") {
		return fmt.Errorf("deprecation: path must be absolute")
	}
	if r.Method != nil {
		switch strings.ToUpper(*r.Method) {
		case "GET", "POST", "PUT", "DELETE", "PATCH":
		default:
			return fmt.Errorf("deprecation: unsupported method %q", *r.Method)
		}
	}
	if r.SunsetAt.Before(r.AnnouncedAt.Add(MinNotice)) {
		return fmt.Errorf("deprecation: sunset_at must be at least six months after announced_at")
	}
	return nil
}

// Announce inserts a rule after validation. sunset_at must satisfy the
// six-month floor; announced_at defaults to now.
func (s *Store) Announce(ctx context.Context, r Rule) (*Rule, error) {
	if r.AnnouncedAt.IsZero() {
		r.AnnouncedAt = s.now().UTC()
	}
	if r.MigrationURL == "" {
		r.MigrationURL = "/developer/migration"
	}
	if r.CreatedBy <= 0 {
		return nil, fmt.Errorf("deprecation: created_by must be positive")
	}
	if err := s.Validate(r); err != nil {
		return nil, err
	}
	var method *string
	if r.Method != nil {
		m := strings.ToUpper(*r.Method)
		method = &m
	}
	var id int64
	err := s.pool.QueryRow(ctx,
		`INSERT INTO api_deprecations
		 (method, path, match_prefix, announced_at, sunset_at,
		  replacement, migration_url, notice, created_by)
		 VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9) RETURNING id`,
		method, r.Path, r.MatchPrefix, r.AnnouncedAt, r.SunsetAt,
		r.Replacement, r.MigrationURL, r.Notice, r.CreatedBy).Scan(&id)
	if err != nil {
		return nil, fmt.Errorf("deprecation: announce: %w", err)
	}
	return s.Get(ctx, id)
}

// Get returns one rule.
func (s *Store) Get(ctx context.Context, id int64) (*Rule, error) {
	r, err := scanRule(s.pool.QueryRow(ctx,
		`SELECT `+ruleCols+` FROM api_deprecations WHERE id=$1`, id).Scan)
	if err != nil {
		return nil, fmt.Errorf("deprecation: rule %d: %w", id, err)
	}
	return r, nil
}

const ruleCols = `id, method, path, match_prefix, announced_at, sunset_at,
	replacement, migration_url, notice, created_by, created_at`

func scanRule(scan func(dest ...any) error) (*Rule, error) {
	var r Rule
	err := scan(&r.ID, &r.Method, &r.Path, &r.MatchPrefix, &r.AnnouncedAt,
		&r.SunsetAt, &r.Replacement, &r.MigrationURL, &r.Notice,
		&r.CreatedBy, &r.CreatedAt)
	if err != nil {
		return nil, err
	}
	return &r, nil
}

// List returns all rules, newest announcement first.
func (s *Store) List(ctx context.Context) ([]Rule, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT `+ruleCols+` FROM api_deprecations
		  ORDER BY announced_at DESC, id DESC`)
	if err != nil {
		return nil, fmt.Errorf("deprecation: list: %w", err)
	}
	defer rows.Close()
	out := []Rule{}
	for rows.Next() {
		r, err := scanRule(rows.Scan)
		if err != nil {
			return nil, fmt.Errorf("deprecation: scan: %w", err)
		}
		out = append(out, *r)
	}
	return out, rows.Err()
}

// ---------------------------------------------------------------------------
// Enforcement middleware.
// ---------------------------------------------------------------------------

// ErrGone is returned for sunset-expired rules; the middleware emits the
// 410 problem body itself.
var ErrGone = errors.New("deprecation: endpoint past sunset")

// Rules resolves the active rule for a request; Store satisfies it.
type Rules interface {
	Active(ctx context.Context) ([]Rule, error)
}

// Active implements Rules — all rules (matching + sunset check happen
// per-request so a freshly-inserted rule takes effect on the next
// request).
func (s *Store) Active(ctx context.Context) ([]Rule, error) { return s.List(ctx) }

// cachedRules wraps a Rules source with a TTL cache — the middleware
// resolves rules per request, and a raw Store.Active would be a PG
// round-trip per request. Rules are admin-written (rare); a 5-second
// staleness bound keeps the cache honest while making the middleware
// effectively free.
type cachedRules struct {
	src     Rules
	ttl     time.Duration
	now     func() time.Time
	mu      sync.Mutex
	rules   []Rule
	loaded  time.Time
	haveSet bool
}

// CachedRules returns a Rules source backed by src with a ttl refresh
// bound (ttl<=0 defaults to 5s). A failed refresh falls back to the last
// good set when one exists — a DB blip must not un-deprecate a sunset
// endpoint (fail-closed); with no cached set the error propagates and
// the middleware degrades to pass-through.
func CachedRules(src Rules, ttl time.Duration) Rules {
	if ttl <= 0 {
		ttl = 5 * time.Second
	}
	return &cachedRules{src: src, ttl: ttl, now: time.Now}
}

func (c *cachedRules) Active(ctx context.Context) ([]Rule, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.haveSet && c.now().Sub(c.loaded) < c.ttl {
		return c.rules, nil
	}
	rules, err := c.src.Active(ctx)
	if err != nil {
		if c.haveSet {
			return c.rules, nil // stale beats absent for sunset enforcement
		}
		return nil, err
	}
	c.rules, c.loaded, c.haveSet = rules, c.now(), true
	return rules, nil
}

// match finds the most specific live rule for (method, path): exact
// method+path beats NULL-method beats prefix rules (longest prefix wins).
func match(rules []Rule, method, path string, now time.Time) *Rule {
	var best *Rule
	bestScore := -1
	for i := range rules {
		r := &rules[i]
		if r.Method != nil && !strings.EqualFold(*r.Method, method) {
			continue
		}
		score := -1
		if r.MatchPrefix {
			if strings.HasPrefix(path, r.Path) {
				score = 1 + len(r.Path)
			}
		} else if r.Path == path {
			score = 1000 + len(r.Path)
		}
		if score < 0 {
			continue
		}
		if r.Method != nil {
			score += 1_000_000 // exact-method beats any-method
		}
		_ = now
		if score > bestScore {
			bestScore = score
			best = r
		}
	}
	return best
}

// Middleware wraps next: stamps Deprecation/Sunset/Link headers on
// announced endpoints, and returns 410 ENDPOINT_GONE once sunset has
// passed. next receives RFC7807 emission via the writeProblem seam so
// this package stays transport-agnostic.
func Middleware(src Rules, writeProblem func(w http.ResponseWriter, r *http.Request, code int, errCode, msg string), now func() time.Time) func(http.Handler) http.Handler {
	if now == nil {
		now = time.Now
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			rules, err := src.Active(r.Context())
			if err == nil {
				if rule := match(rules, r.Method, r.URL.Path, now()); rule != nil {
					if !now().Before(rule.SunsetAt) {
						writeProblem(w, r, http.StatusGone, "ENDPOINT_GONE",
							fmt.Sprintf("endpoint sunset at %s; see %s",
								rule.SunsetAt.UTC().Format(time.RFC1123), rule.MigrationURL))
						return
					}
					w.Header().Set("Deprecation", fmt.Sprintf("@%d", rule.AnnouncedAt.Unix()))
					w.Header().Set("Sunset", rule.SunsetAt.UTC().Format(http.TimeFormat))
					w.Header().Add("Link", fmt.Sprintf(`<%s>; rel="deprecation"`, rule.MigrationURL))
				}
			}
			next.ServeHTTP(w, r)
		})
	}
}
