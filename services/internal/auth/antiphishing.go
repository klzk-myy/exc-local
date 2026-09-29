package auth

// antiphishing.go — Phase-12 Task 12.3.8: anti-phishing code.
//
// users.anti_phishing_code (migration 070) is a user-chosen 4–32-char
// phrase the notification service renders inside every outbound email so
// the user can distinguish genuine exchange mail from phishing. Unset =
// NULL; the notification AntiPhishLookup seam (notifications/antiphish.go)
// renders a "configure your anti-phishing code" banner on NULL — this
// service never invents a default.
//
// Writes go through SetAntiPhishingCode — the PUT
// /api/v1/account/settings/anti-phishing-code handler additionally gates
// on a verified second factor (route-level RequireTwoFactor).

import (
	"context"
	"fmt"
	"unicode/utf8"

	"github.com/jackc/pgx/v5/pgxpool"
)

// AntiPhishing bounds — spec §5.16 / Phase-10 settings UI.
const (
	AntiPhishingMinLen = 4
	AntiPhishingMaxLen = 32
)

// ValidateAntiPhishingCode enforces the 4–32 char window (rune length —
// the CHECK constraint is char_length, so both count characters).
// An empty code means "clear"; the caller maps it to NULL.
func ValidateAntiPhishingCode(code string) error {
	n := utf8.RuneCountInString(code)
	if n < AntiPhishingMinLen || n > AntiPhishingMaxLen {
		return newError(CodeInvalidRequest,
			fmt.Sprintf("anti-phishing code must be %d-%d characters",
				AntiPhishingMinLen, AntiPhishingMaxLen))
	}
	return nil
}

// AntiPhishingService is the narrow store seam for the settings route
// and for notification consumers.
type AntiPhishingService struct {
	pool *pgxpool.Pool
}

func NewAntiPhishingService(pool *pgxpool.Pool) (*AntiPhishingService, error) {
	if pool == nil {
		return nil, newError(CodeAuthInternal, "anti-phishing pool is nil")
	}
	return &AntiPhishingService{pool: pool}, nil
}

// Set validates and stores the code. Passing "" clears the setting
// (stored as NULL).
func (s *AntiPhishingService) Set(ctx context.Context, userID int64, code string) error {
	if code != "" {
		if err := ValidateAntiPhishingCode(code); err != nil {
			return err
		}
	}
	var arg *string
	if code != "" {
		arg = &code
	}
	tag, err := s.pool.Exec(ctx,
		`UPDATE users SET anti_phishing_code = $2, updated_at = now()
		  WHERE id = $1`, userID, arg)
	if err != nil {
		return fmt.Errorf("anti-phishing code update: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return newError(CodeUnauthorized, "user not found")
	}
	return nil
}

// Get returns the configured code, or nil when unset — the exact signal
// the notification template consumes to render its configuration banner.
func (s *AntiPhishingService) Get(ctx context.Context, userID int64) (*string, error) {
	var code *string
	err := s.pool.QueryRow(ctx,
		`SELECT anti_phishing_code FROM users WHERE id = $1`, userID).Scan(&code)
	if err != nil {
		return nil, fmt.Errorf("anti-phishing code read: %w", err)
	}
	return code, nil
}
