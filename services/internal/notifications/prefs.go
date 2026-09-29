// Notification preference model (Task 12.3.6): per-event, per-channel
// opt-in/out matrix plus a UTC quiet-hours window.
//
// Rulings:
//   - Defaults: email + WS enabled for every event; SMS + push
//     disabled (they need a verified recipient/device the user has not
//     configured yet — opt-in, not opt-out).
//   - Quiet hours are evaluated in UTC on the server's clock. A
//     per-user-timezone axis is deliberately deferred (documented in
//     migration 202's header).
//   - CRITICAL events (security_alert, liquidation_warning) bypass
//     quiet hours entirely — see criticalEvents.
//   - An enabled window with start == end is invalid (a 24h window
//     would starve all non-critical mail forever); PUT rejects it.
package notifications

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	excerrors "exchange/pkg/errors"
)

// QuietHours is the do-not-disturb window, "HH:MM" UTC endpoints.
type QuietHours struct {
	Enabled bool   `json:"enabled"`
	Start   string `json:"start,omitempty"` // "HH:MM" UTC, inclusive
	End     string `json:"end,omitempty"`   // "HH:MM" UTC, exclusive; may wrap midnight
}

// Preferences is the per-user row model (migration 202). Matrix is
// {event: {channel: enabled}}; absent cells resolve to defaults.
type Preferences struct {
	UserID    int64                      `json:"user_id"`
	Matrix    map[string]map[string]bool `json:"matrix"`
	Quiet     QuietHours                 `json:"quiet_hours"`
	UpdatedAt time.Time                  `json:"updated_at,omitempty"`
}

// defaultOn is the factory matrix cell set: email + ws.
var defaultOn = map[string]bool{ChannelEmail: true, ChannelWS: true}

// DefaultPreferences returns the all-default preference set for a user
// with no stored row.
func DefaultPreferences(userID int64) *Preferences {
	return &Preferences{UserID: userID, Matrix: map[string]map[string]bool{}}
}

// Enabled resolves (event, channel): an explicit matrix cell wins;
// absent cells resolve to the default policy (email + ws on).
func (p *Preferences) Enabled(event, channel string) bool {
	if p == nil {
		return defaultOn[channel]
	}
	if ch, ok := p.Matrix[event]; ok {
		if v, ok := ch[channel]; ok {
			return v
		}
	}
	return defaultOn[channel]
}

// parseHHMM validates and converts "HH:MM" to minutes since midnight.
// Returns -1 on malformed input.
func parseHHMM(s string) int {
	parts := strings.Split(s, ":")
	if len(parts) != 2 {
		return -1
	}
	h, err1 := strconv.Atoi(parts[0])
	m, err2 := strconv.Atoi(parts[1])
	if err1 != nil || err2 != nil || h < 0 || h > 23 || m < 0 || m > 59 ||
		len(parts[0]) != 2 || len(parts[1]) != 2 {
		return -1
	}
	return h*60 + m
}

// Validate enforces the matrix vocabulary and quiet-hours shape —
// unknown events/channels and malformed windows reject INVALID_REQUEST
// (a typo'd preference must not silently dead-letter real mail).
func (p *Preferences) Validate() error {
	for ev, chans := range p.Matrix {
		if !ValidEvent(ev) {
			return excerrors.New("INVALID_REQUEST",
				fmt.Sprintf("unknown notification event %q", ev))
		}
		for ch := range chans {
			if !ValidChannel(ch) {
				return excerrors.New("INVALID_REQUEST",
					fmt.Sprintf("unknown notification channel %q", ch))
			}
		}
	}
	q := p.Quiet
	if !q.Enabled {
		if q.Start != "" || q.End != "" {
			return excerrors.New("INVALID_REQUEST",
				"quiet_hours start/end require enabled=true")
		}
		return nil
	}
	st, en := parseHHMM(q.Start), parseHHMM(q.End)
	if st < 0 || en < 0 {
		return excerrors.New("INVALID_REQUEST",
			"quiet_hours start/end must be \"HH:MM\" UTC")
	}
	if st == en {
		return excerrors.New("INVALID_REQUEST",
			"quiet_hours start and end must differ")
	}
	return nil
}

// quietMinutes returns (start, end, ok) in minutes-since-midnight UTC.
func (p *Preferences) quietMinutes() (int, int, bool) {
	if p == nil || !p.Quiet.Enabled {
		return 0, 0, false
	}
	st, en := parseHHMM(p.Quiet.Start), parseHHMM(p.Quiet.End)
	return st, en, st >= 0 && en >= 0 && st != en
}

// InQuietHours reports whether now falls inside the configured window
// (UTC). Wrap-around windows (22:00→07:00) are supported.
func (p *Preferences) InQuietHours(now time.Time) bool {
	st, en, ok := p.quietMinutes()
	if !ok {
		return false
	}
	m := now.UTC().Hour()*60 + now.UTC().Minute()
	if st < en {
		return m >= st && m < en
	}
	return m >= st || m < en
}

// QuietEndAfter returns the next instant the current quiet window ends
// (UTC) — the requeue time for deferred non-critical deliveries.
// Callers must only invoke it while InQuietHours(now) holds.
func (p *Preferences) QuietEndAfter(now time.Time) time.Time {
	st, en, ok := p.quietMinutes()
	if !ok {
		return now.UTC()
	}
	u := now.UTC()
	end := time.Date(u.Year(), u.Month(), u.Day(), en/60, en%60, 0, 0, time.UTC)
	m := u.Hour()*60 + u.Minute()
	// Inside a wrapped window, tonight's end already passed when m >= st.
	if st > en && m >= st {
		end = end.Add(24 * time.Hour)
	}
	if !end.After(u) {
		end = end.Add(24 * time.Hour)
	}
	return end
}
