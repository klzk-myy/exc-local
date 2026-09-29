// Admin surface for fix_sessions — backs the live
// PUT /api/v1/admin/fix-sessions/{id} route (Task 18.3.9). Validation
// lives here so the gateway handler stays a thin HTTP adapter and the
// same rules hold for any future provisioning surface (CLI, WS admin).
package fix

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"exchange/internal/config"
	excerrors "exchange/pkg/errors"
)

// EntitlementPatch is the admin request shape: every field has an
// explicit "set" guard so JSON null and absent stay distinguishable.
type EntitlementPatch struct {
	// AccountIDSet+AccountID binds the session for order entry;
	// AccountIDSet+nil clears the binding (drop-copy-only session —
	// the only legal NULL per spec §5.20).
	AccountIDSet bool
	AccountID    *int64
	// APIKeyIDSet+APIKeyID rebinds the logon credential;
	// APIKeyIDSet+nil clears it (logons then always fail
	// SESSION_NOT_PROVISIONED — a session-lock switch).
	APIKeyIDSet bool
	APIKeyID    *int64
	// AllowedInstrumentsSet replaces the allowlist — canonicalized +
	// sorted; "" → NULL (entitled to every instrument, spec §5.20).
	AllowedInstrumentsSet bool
	AllowedInstruments    string
	// CancelOnDisconnect flips the §24 #153 mass-cancel flag.
	CancelOnDisconnect *bool
	// MaxMsgsPerSec replaces the throttle cap (1..65535).
	MaxMsgsPerSec *int
}

// AccountChecker verifies a bound account exists — the gateway passes
// its account store; nil skips the existence check (the FK still
// enforces at the DB layer).
type AccountChecker interface {
	AccountExists(ctx context.Context, accountID int64) (bool, error)
}

// KeyChecker verifies a bound api_keys row exists.
type KeyChecker interface {
	APIKeyExists(ctx context.Context, keyID int64) (bool, error)
}

// ApplyEntitlementPatch validates the patch, translates it to the
// store-level EntitlementUpdate and returns the refreshed row for the
// HTTP response.
func ApplyEntitlementPatch(ctx context.Context, s Store, sessionID string,
	p EntitlementPatch, accts AccountChecker, keys KeyChecker) (*Session, error) {
	if strings.TrimSpace(sessionID) == "" {
		return nil, excerrors.New("INVALID_REQUEST", "session_id is required")
	}
	existing, err := s.SessionByID(ctx, sessionID)
	if err != nil {
		return nil, err
	}
	if existing == nil {
		return nil, excerrors.New("ORDER_NOT_FOUND", "fix session not found")
	}

	var u EntitlementUpdate
	if p.AccountIDSet {
		if p.AccountID == nil {
			u.ClearAccount = true
		} else {
			if *p.AccountID <= 0 {
				return nil, excerrors.New("INVALID_REQUEST",
					"account_id must be positive")
			}
			if accts != nil {
				ok, cerr := accts.AccountExists(ctx, *p.AccountID)
				if cerr != nil {
					return nil, cerr
				}
				if !ok {
					return nil, excerrors.New("ACCOUNT_NOT_FOUND",
						fmt.Sprintf("account %d not found", *p.AccountID))
				}
			}
			v := *p.AccountID
			u.AccountID = &v
		}
	}
	if p.APIKeyIDSet {
		if p.APIKeyID == nil {
			u.ClearAPIKey = true
		} else {
			if *p.APIKeyID <= 0 {
				return nil, excerrors.New("INVALID_REQUEST",
					"api_key_id must be positive")
			}
			if keys != nil {
				ok, kerr := keys.APIKeyExists(ctx, *p.APIKeyID)
				if kerr != nil {
					return nil, kerr
				}
				if !ok {
					return nil, excerrors.New("API_KEY_INVALID",
						fmt.Sprintf("api key %d not found", *p.APIKeyID))
				}
			}
			v := *p.APIKeyID
			u.APIKeyID = &v
		}
	}
	if p.AllowedInstrumentsSet {
		list := canonicalInstrumentList(p.AllowedInstruments)
		if list == "" {
			u.SetAllInstruments = true
		} else {
			u.AllowedInstruments = &list
		}
	}
	u.CancelOnDisconnect = p.CancelOnDisconnect
	if p.MaxMsgsPerSec != nil {
		if *p.MaxMsgsPerSec < 1 || *p.MaxMsgsPerSec > 65535 {
			return nil, excerrors.New("INVALID_REQUEST",
				"max_msgs_per_sec must be 1..65535")
		}
		u.MaxMsgsPerSec = p.MaxMsgsPerSec
	}
	return s.UpdateEntitlement(ctx, sessionID, u)
}

// PgCheckers builds the account/api-key existence checkers the admin
// route validates bindings against.
type PgCheckers struct{ pool *pgxpool.Pool }

// NewPgCheckers wires both checkers to one pool.
func NewPgCheckers(pool *pgxpool.Pool) *PgCheckers { return &PgCheckers{pool: pool} }

// AccountExists implements AccountChecker.
func (c *PgCheckers) AccountExists(ctx context.Context, accountID int64) (bool, error) {
	var n int
	err := c.pool.QueryRow(ctx,
		`SELECT 1 FROM accounts WHERE id = $1`, accountID).Scan(&n)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}

// APIKeyExists implements KeyChecker.
func (c *PgCheckers) APIKeyExists(ctx context.Context, keyID int64) (bool, error) {
	var n int
	err := c.pool.QueryRow(ctx,
		`SELECT 1 FROM api_keys WHERE id = $1`, keyID).Scan(&n)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}

// canonicalInstrumentList parses the comma-separated allowlist into a
// deduplicated, sorted, canonical-symbol string.
func canonicalInstrumentList(raw string) string {
	seen := map[string]bool{}
	var out []string
	for _, tok := range strings.Split(raw, ",") {
		s := config.CanonicalSymbol(tok)
		if s == "" || seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	sort.Strings(out)
	return strings.Join(out, ",")
}
