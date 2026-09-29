// Task 18.3.9 item 5 — live FIX-session admin surface.
//
//	PUT /api/v1/admin/fix-sessions/{id}
//
// Updates the admin-managed columns of one fix_sessions row
// (spec §5.20): account binding (null → drop-copy-only), api_key
// credential binding (null → session lock), allowed_instruments
// ("" → NULL = all instruments), cancel_on_disconnect, max_msgs_per_sec.
//
// Auth: Super Admin via the route registry's RBAC wrap (§8.2). Every
// mutation lands in admin_audit_log through the caller's audit wrap —
// the handler itself validates via internal/fix.ApplyEntitlementPatch.
package api

import (
	"encoding/json"
	"net/http"

	"exchange/internal/fix"
	"exchange/internal/gateway"
	excerrors "exchange/pkg/errors"
)

// FixSessionUpdateHandler builds the PUT handler. deps are the fix
// Store plus the account/api-key existence checkers (fix.PgCheckers
// satisfies both).
func FixSessionUpdateHandler(st fix.Store, accts fix.AccountChecker,
	keys fix.KeyChecker) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if requireAdmin(w, r) == nil {
			return
		}
		sessionID := r.PathValue("id")
		// Field presence matters: "account_id": null clears the
		// binding while an absent key leaves it — decode into a raw map.
		var raw map[string]json.RawMessage
		if err := json.NewDecoder(r.Body).Decode(&raw); err != nil {
			WriteError(w, "INVALID_REQUEST", "malformed body",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		var p fix.EntitlementPatch
		if v, ok := raw["account_id"]; ok {
			p.AccountIDSet = true
			if string(v) != "null" {
				var id int64
				if err := json.Unmarshal(v, &id); err != nil {
					WriteError(w, "INVALID_REQUEST", "account_id must be integer or null",
						gateway.RequestIDFrom(r.Context()), nil)
					return
				}
				p.AccountID = &id
			}
		}
		if v, ok := raw["api_key_id"]; ok {
			p.APIKeyIDSet = true
			if string(v) != "null" {
				var id int64
				if err := json.Unmarshal(v, &id); err != nil {
					WriteError(w, "INVALID_REQUEST", "api_key_id must be integer or null",
						gateway.RequestIDFrom(r.Context()), nil)
					return
				}
				p.APIKeyID = &id
			}
		}
		if v, ok := raw["allowed_instruments"]; ok {
			p.AllowedInstrumentsSet = true
			if string(v) != "null" {
				var s string
				if err := json.Unmarshal(v, &s); err != nil {
					WriteError(w, "INVALID_REQUEST", "allowed_instruments must be string or null",
						gateway.RequestIDFrom(r.Context()), nil)
					return
				}
				p.AllowedInstruments = s
			}
		}
		if v, ok := raw["cancel_on_disconnect"]; ok {
			var b bool
			if err := json.Unmarshal(v, &b); err != nil {
				WriteError(w, "INVALID_REQUEST", "cancel_on_disconnect must be boolean",
					gateway.RequestIDFrom(r.Context()), nil)
				return
			}
			p.CancelOnDisconnect = &b
		}
		if v, ok := raw["max_msgs_per_sec"]; ok {
			var n int
			if err := json.Unmarshal(v, &n); err != nil {
				WriteError(w, "INVALID_REQUEST", "max_msgs_per_sec must be integer",
					gateway.RequestIDFrom(r.Context()), nil)
				return
			}
			p.MaxMsgsPerSec = &n
		}

		row, err := fix.ApplyEntitlementPatch(r.Context(), st, sessionID, p, accts, keys)
		if err != nil {
			switch excerrors.CodeOf(err) {
			case "ORDER_NOT_FOUND":
				WriteError(w, "ORDER_NOT_FOUND", "fix session not found",
					gateway.RequestIDFrom(r.Context()), nil)
			case "INVALID_REQUEST", "ACCOUNT_NOT_FOUND", "API_KEY_INVALID":
				WriteError(w, excerrors.CodeOf(err), err.Error(),
					gateway.RequestIDFrom(r.Context()), nil)
			default:
				WriteError(w, "SERVICE_DEGRADED", "fix session store unavailable",
					gateway.RequestIDFrom(r.Context()), nil)
			}
			return
		}
		WriteJSON(w, http.StatusOK, fixSessionView(row))
	}
}

// fixSessionView serializes the refreshed row — decimals/numerics as
// plain JSON values per the §5.3 admin wire contract.
func fixSessionView(s *fix.Session) map[string]any {
	v := map[string]any{
		"session_id":           s.SessionID,
		"protocol_version":     s.ProtocolVersion,
		"sender_seq_num":       s.SenderSeqNum,
		"target_seq_num":       s.TargetSeqNum,
		"status":               s.Status,
		"cancel_on_disconnect": s.CancelOnDisconnect,
		"max_msgs_per_sec":     s.MaxMsgsPerSec,
		"created_at":           s.CreatedAt,
		"updated_at":           s.UpdatedAt,
	}
	if s.AccountID != nil {
		v["account_id"] = *s.AccountID
	} else {
		v["account_id"] = nil
	}
	if s.APIKeyID != nil {
		v["api_key_id"] = *s.APIKeyID
	} else {
		v["api_key_id"] = nil
	}
	if s.AllowedInstruments == "" {
		v["allowed_instruments"] = nil // NULL = all instruments
	} else {
		v["allowed_instruments"] = s.AllowedInstruments
	}
	if s.LastHeartbeatAt != nil {
		v["last_heartbeat_at"] = *s.LastHeartbeatAt
	} else {
		v["last_heartbeat_at"] = nil
	}
	return v
}
