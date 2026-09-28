// Task 5.3.34 — progressive IP-ban escalation, admin surface.
//
// Escalation contract (task text + §24 #258):
//
//	offense := a request received while the rl429:{ip} marker is armed
//	           (i.e. within 60s of a served HTTP 429).
//	strike N → ban duration BanSchedule[min(N,3)-1]: 2min / 30min / 24h.
//	during-ban requests return HTTP 418 without accruing new strikes —
//	the schedule ratchets only through post-429 re-offense.
//
// Admin override paths audit into ip_ban_audit (same list the Lua strike
// path appends to) with actor identity, so the §24 #258 "admin override
// and audit" criterion is one reviewable trail.
package ratelimit

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"time"
)

// AuditEvent is the admin-facing record appended to ip_ban_audit for
// manual actions (system strikes self-audit inside the Lua script).
type AuditEvent struct {
	Action string `json:"action"` // ban | unban | pardon | allowlist_add | allowlist_remove
	IP     string `json:"ip"`
	Actor  string `json:"actor"`            // admin identity
	Detail string `json:"detail,omitempty"` // reason / notes
	At     int64  `json:"at_ms"`
}

// BanAdmin wraps a Backend with the review/override operations the admin
// handlers expose. actor identifies the operator for the audit trail.
type BanAdmin struct {
	B     Backend
	Now   func() time.Time
	Actor string // fallback actor when the caller passes ""
}

func (a *BanAdmin) now() time.Time {
	if a.Now != nil {
		return a.Now()
	}
	return time.Now()
}

func (a *BanAdmin) audit(ctx context.Context, e AuditEvent) {
	if e.Actor == "" {
		e.Actor = a.Actor
	}
	if e.Actor == "" {
		e.Actor = "admin"
	}
	e.At = a.now().UnixMilli()
	if rec, err := json.Marshal(e); err == nil {
		// Audit failure must never block the override itself (the ban
		// state change already landed); callers that need strict audit
		// ordering inspect the error path via ListBans evidence.
		_ = a.B.Audit(ctx, string(rec))
	}
}

// BanIP imposes a manual ban (admin override). duration<=0 maps to the
// top of the escalation schedule (24h).
func (a *BanAdmin) BanIP(ctx context.Context, ip, reason, actor string, duration time.Duration) (*Ban, error) {
	if net.ParseIP(ip) == nil {
		return nil, fmt.Errorf("ratelimit: %q is not an IP", ip)
	}
	if duration <= 0 {
		duration = BanSchedule[len(BanSchedule)-1]
	}
	now := a.now()
	ban := Ban{
		IP: ip, Level: len(BanSchedule), Strikes: 0,
		Reason:    orDefault(reason, "admin manual ban"),
		BannedAt:  now.UnixMilli(),
		ExpiresAt: now.Add(duration).UnixMilli(),
		Actor:     orDefault(actor, "admin"),
	}
	if err := a.B.SetBan(ctx, ban); err != nil {
		return nil, err
	}
	a.audit(ctx, AuditEvent{Action: "ban", IP: ip, Actor: actor, Detail: ban.Reason})
	return &ban, nil
}

// UnbanIP clears the active ban; the strike counter survives so a
// re-offense re-escalates rather than restarting at level 1.
func (a *BanAdmin) UnbanIP(ctx context.Context, ip, actor string) (bool, error) {
	removed, err := a.B.ClearBan(ctx, ip)
	if err != nil {
		return false, err
	}
	if removed {
		a.audit(ctx, AuditEvent{Action: "unban", IP: ip, Actor: actor})
	}
	return removed, nil
}

// PardonIP clears ban + strikes + marker — the full forgiveness path.
func (a *BanAdmin) PardonIP(ctx context.Context, ip, actor string) error {
	if _, err := a.B.ClearBan(ctx, ip); err != nil {
		return err
	}
	if err := a.B.ClearStrikes(ctx, ip); err != nil {
		return err
	}
	a.audit(ctx, AuditEvent{Action: "pardon", IP: ip, Actor: actor})
	return nil
}

// AllowlistIP exempts an IP from the ban machinery (§8.8 allowlist
// bypass). Rate limits still apply.
func (a *BanAdmin) AllowlistIP(ctx context.Context, ip, actor string) error {
	if net.ParseIP(ip) == nil {
		return fmt.Errorf("ratelimit: %q is not an IP", ip)
	}
	if err := a.B.AddAllowlist(ctx, ip); err != nil {
		return err
	}
	a.audit(ctx, AuditEvent{Action: "allowlist_add", IP: ip, Actor: actor})
	return nil
}

// UnallowlistIP removes the exemption.
func (a *BanAdmin) UnallowlistIP(ctx context.Context, ip, actor string) error {
	if err := a.B.RemoveAllowlist(ctx, ip); err != nil {
		return err
	}
	a.audit(ctx, AuditEvent{Action: "allowlist_remove", IP: ip, Actor: actor})
	return nil
}

func orDefault(s, d string) string {
	if s == "" {
		return d
	}
	return s
}
