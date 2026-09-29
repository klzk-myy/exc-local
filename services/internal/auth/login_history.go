package auth

// login_history.go — Phase-12 Task 12.3.9: login history recording +
// user-scoped read (migration 069).
//
// LoginRecorder is the seam cluster-1's login handler injects: it calls
// Record(ctx, ev) on every authentication outcome. Geo fields stay NULL
// unless a GeoResolver is configured — nothing fabricates geography.

import (
	"context"
	"encoding/base64"
	"fmt"
	"net"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// LoginResult is the constrained login_history.result vocabulary.
type LoginResult string

const (
	LoginResultSuccess   LoginResult = "SUCCESS"
	LoginResultFailed    LoginResult = "FAILED"
	LoginResult2FAFailed LoginResult = "2FA_FAILED"
	LoginResultLocked    LoginResult = "LOCKED"
)

// LoginEvent is one authentication-attempt record.
type LoginEvent struct {
	ID                int64       `json:"id,omitempty"`
	UserID            int64       `json:"-"`
	Timestamp         time.Time   `json:"timestamp"`
	IP                string      `json:"ip,omitempty"`
	UserAgent         string      `json:"user_agent,omitempty"`
	DeviceFingerprint string      `json:"device_fingerprint,omitempty"`
	GeoCity           string      `json:"geo_city,omitempty"`
	GeoCountry        string      `json:"geo_country,omitempty"`
	Result            LoginResult `json:"result"`
	SessionID         string      `json:"session_id,omitempty"`
}

// LoginRecorder is the dependency injected into cluster-1's login
// handler: every authentication outcome — success, bad password, failed
// 2FA, rejected-because-locked — produces one row.
type LoginRecorder interface {
	Record(ctx context.Context, ev LoginEvent) error
}

// GeoResolver maps a client IP to (city, ISO-3166 alpha-2 country). It
// is optional: nil means geo columns stay NULL — the spec's "populate
// from trusted sources only, never fabricate" rule.
type GeoResolver func(ctx context.Context, ip string) (city, country string, ok bool)

// LoginHistoryService records and serves the 90-day login audit trail.
type LoginHistoryService struct {
	pool *pgxpool.Pool
	geo  GeoResolver
	now  func() time.Time
}

func NewLoginHistoryService(pool *pgxpool.Pool) (*LoginHistoryService, error) {
	if pool == nil {
		return nil, newError(CodeAuthInternal, "login history pool is nil")
	}
	return &LoginHistoryService{pool: pool, now: time.Now}, nil
}

// WithGeo binds the optional GeoIP resolver.
func (s *LoginHistoryService) WithGeo(g GeoResolver) *LoginHistoryService {
	s.geo = g
	return s
}

// Record implements LoginRecorder; also exported as RecordLogin per the
// task's named-API contract.
func (s *LoginHistoryService) Record(ctx context.Context, ev LoginEvent) error {
	return s.RecordLogin(ctx, ev)
}

// RecordLogin inserts one history row. Geo columns resolve through the
// configured GeoResolver only; absent a resolver they are NULL.
func (s *LoginHistoryService) RecordLogin(ctx context.Context, ev LoginEvent) error {
	if ev.UserID <= 0 {
		return newError(CodeInvalidRequest, "login history: user_id required")
	}
	switch ev.Result {
	case LoginResultSuccess, LoginResultFailed, LoginResult2FAFailed, LoginResultLocked:
	default:
		return newError(CodeInvalidRequest, "login history: invalid result")
	}
	ts := ev.Timestamp
	if ts.IsZero() {
		ts = s.now()
	}
	city, country := ev.GeoCity, ev.GeoCountry
	if s.geo != nil && (city == "" && country == "") && ev.IP != "" {
		if c, co, ok := s.geo(ctx, ev.IP); ok {
			city, country = c, co
		}
	}
	var ipParam *net.IP
	if ev.IP != "" {
		if ip := net.ParseIP(ev.IP); ip != nil {
			ipParam = &ip
		}
	}
	_, err := s.pool.Exec(ctx,
		`INSERT INTO login_history
		   (user_id, "timestamp", ip, user_agent, device_fingerprint,
		    geo_city, geo_country, result, session_id)
		 VALUES ($1, $2, $3, NULLIF($4,''), NULLIF($5,''),
		         NULLIF($6,''), NULLIF($7,''), $8, NULLIF($9,''))`,
		ev.UserID, ts.UTC(), ipParam, ev.UserAgent, ev.DeviceFingerprint,
		city, country, string(ev.Result), ev.SessionID)
	if err != nil {
		return fmt.Errorf("login history insert: %w", err)
	}
	return nil
}

// LoginHistoryPage is one keyset page of the 90-day trail.
type LoginHistoryPage struct {
	Events     []LoginEvent `json:"events"`
	NextCursor string       `json:"next_cursor,omitempty"`
}

// List returns the caller's last-90-days history, newest first, keyset-
// paginated on (timestamp, id). cursor is the opaque token returned by
// the previous page ("" = first page); limit is clamped [1,100].
func (s *LoginHistoryService) List(ctx context.Context, userID int64, limit int, cursor string) (LoginHistoryPage, error) {
	if limit <= 0 || limit > 100 {
		limit = 25
	}
	since := s.now().Add(-90 * 24 * time.Hour)
	var beforeTS time.Time
	var beforeID int64
	if cursor != "" {
		var err error
		beforeTS, beforeID, err = decodeLoginCursor(cursor)
		if err != nil {
			return LoginHistoryPage{}, newError(CodeInvalidRequest, "invalid cursor")
		}
	}
	var rows interface {
		Close()
		Next() bool
		Scan(...any) error
		Err() error
	}
	var err error
	if cursor == "" {
		rows, err = s.pool.Query(ctx,
			`SELECT id, "timestamp", host(ip), user_agent, device_fingerprint,
			        geo_city, geo_country, result::text, session_id
			   FROM login_history
			  WHERE user_id = $1 AND "timestamp" >= $2
			  ORDER BY "timestamp" DESC, id DESC
			  LIMIT $3`, userID, since, limit+1)
	} else {
		rows, err = s.pool.Query(ctx,
			`SELECT id, "timestamp", host(ip), user_agent, device_fingerprint,
			        geo_city, geo_country, result::text, session_id
			   FROM login_history
			  WHERE user_id = $1 AND "timestamp" >= $2
			    AND ("timestamp", id) < ($3, $4)
			  ORDER BY "timestamp" DESC, id DESC
			  LIMIT $5`, userID, since, beforeTS, beforeID, limit+1)
	}
	if err != nil {
		return LoginHistoryPage{}, fmt.Errorf("login history read: %w", err)
	}
	defer rows.Close()
	evs := []LoginEvent{}
	for rows.Next() {
		var ev LoginEvent
		var ip, ua, fp, city, country, sid *string
		if err := rows.Scan(&ev.ID, &ev.Timestamp, &ip, &ua, &fp,
			&city, &country, &ev.Result, &sid); err != nil {
			return LoginHistoryPage{}, fmt.Errorf("login history scan: %w", err)
		}
		ev.UserID = userID
		if ip != nil {
			ev.IP = *ip
		}
		if ua != nil {
			ev.UserAgent = *ua
		}
		if fp != nil {
			ev.DeviceFingerprint = *fp
		}
		if city != nil {
			ev.GeoCity = *city
		}
		if country != nil {
			ev.GeoCountry = *country
		}
		if sid != nil {
			ev.SessionID = *sid
		}
		evs = append(evs, ev)
	}
	if err := rows.Err(); err != nil {
		return LoginHistoryPage{}, fmt.Errorf("login history read: %w", err)
	}
	page := LoginHistoryPage{Events: evs}
	if len(evs) > limit {
		last := evs[limit-1]
		page.Events = evs[:limit]
		page.NextCursor = encodeLoginCursor(last.Timestamp, last.ID)
	}
	return page, nil
}

func encodeLoginCursor(ts time.Time, id int64) string {
	return b64urlEncode([]byte(fmt.Sprintf("%d|%d", ts.UTC().UnixNano(), id)))
}

func decodeLoginCursor(cur string) (time.Time, int64, error) {
	raw, err := base64.RawURLEncoding.DecodeString(cur)
	if err != nil {
		return time.Time{}, 0, err
	}
	var nanos, id int64
	if _, err := fmt.Sscanf(string(raw), "%d|%d", &nanos, &id); err != nil {
		return time.Time{}, 0, err
	}
	return time.Unix(0, nanos).UTC(), id, nil
}
