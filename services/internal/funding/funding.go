// Package funding — Phase-05 Wave-2 Cluster B: the funding/transfer domain
// services behind the gateway's account & funding endpoints.
//
// Tasks implemented here:
//
//	5.3.6  Funding Endpoints (deposit instructions, withdrawal create +
//	       15-minute confirm token flow, funding history)
//	5.3.18 Chargeback Handling (dispute lifecycle + automated evidence
//	       collection + optional account freeze)
//	5.3.23 Internal Transfers (same-user / master↔sub money movement)
//	5.3.45 Transfer History Query (cursor-paginated journal read)
//	5.3.4  Account & Balance read models (balances/positions/nostro reads
//	       consumed by internal/api handlers)
//
// Invariants (spec §5.3/§2.7): every wallet mutation rides
// settlement.LedgerService.Post — never a direct UPDATE on balances — so
// GL lines, wallet effects, ledger_entries and journal_sums commit inside
// one SERIALIZABLE tx with per-account Redis locks. Account-scoped
// idempotency (spec §8.8) is enforced by composite UNIQUE keys
// (account_id, idempotency_key) on funding_transactions (migration 160)
// and transfers (migration 161); payload_sha256 distinguishes a replayed
// request from a key-reuse conflict (IDEMPOTENCY_KEY_MISMATCH, 422).
//
// Canonical values (AGENTS.md): withdrawal confirm window = exactly
// 15 minutes; review tiers < $10K AUTO / $10K–$50K STANDARD /
// > $50K PENDING_REVIEW with a 4-hour review deadline; the same tiers
// classify deposit anti-fraud review.
package funding

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"
	"time"

	"exchange/internal/accounts"
	"exchange/internal/ledger"
	"exchange/internal/settlement"
	"exchange/pkg/decimal"
	excerrors "exchange/pkg/errors"
)

// ---------------------------------------------------------------------------
// Canonical constants (AGENTS.md — do not drift)
// ---------------------------------------------------------------------------

// WithdrawalConfirmWindow is the exact 15-minute confirmation window.
const WithdrawalConfirmWindow = 15 * time.Minute

// ReviewWindow is the 4-hour ops review deadline applied to >$50K
// withdrawals that enter PENDING_REVIEW.
const ReviewWindow = 4 * time.Hour

var (
	usdAutoMax     = decimal.NewFromInt(10_000) // < $10K → AUTO
	usdStandardMax = decimal.NewFromInt(50_000) // $10K–$50K → STANDARD
)

// Review tiers stored on funding_transactions.review_tier.
const (
	ReviewTierAuto          = "AUTO"
	ReviewTierStandard      = "STANDARD"
	ReviewTierPendingReview = "PENDING_REVIEW"
)

// TierForUSD classifies an operation by its USD-equivalent amount. A nil
// usd value (no conversion rate available) fails closed to the strictest
// tier — an unpriced large withdrawal must never skip review.
func TierForUSD(usd *decimal.Decimal) string {
	if usd == nil {
		return ReviewTierPendingReview
	}
	if usd.LessThan(usdAutoMax) {
		return ReviewTierAuto
	}
	if !usd.GreaterThan(usdStandardMax) {
		return ReviewTierStandard
	}
	return ReviewTierPendingReview
}

// Funding / transfer statuses (enums owned by migrations 007/161).
const (
	FundingPending       = "PENDING"
	FundingConfirmed     = "CONFIRMED"
	FundingCompleted     = "COMPLETED"
	FundingFailed        = "FAILED"
	FundingAutoCancelled = "AUTO_CANCELLED"
	FundingPendingReview = "PENDING_REVIEW"

	TransferPending   = "PENDING"
	TransferCompleted = "COMPLETED"
	TransferFailed    = "FAILED"
)

// ---------------------------------------------------------------------------
// Error codes
// ---------------------------------------------------------------------------

// CodeWithdrawalConfirmExpired is emitted when a confirm token arrives
// after the 15-minute window (withdrawal is auto-cancelled, hold released).
// Registered as a local row in internal/errs (spec §23 row pending).
const CodeWithdrawalConfirmExpired = "WITHDRAWAL_CONFIRM_EXPIRED"

// ---------------------------------------------------------------------------
// Service seams — production implementations come from
// internal/settlement, internal/accounts, internal/risk.
// ---------------------------------------------------------------------------

// JournalPoster is the §5.3 DoubleEntryLedgerService seam
// (*settlement.LedgerService). Post validates, locks accounts, writes
// journal+lines+effects in one SERIALIZABLE tx and dispatches
// BalanceChanged events after commit.
type JournalPoster interface {
	Post(ctx context.Context, j ledger.Journal) (ledger.PostResult, error)
}

// MutableChecker enforces the FROZEN/SUSPENDED gate before any money
// movement (*accounts.FreezeService.AssertMutable). Codes propagate:
// FROZEN → ACCOUNT_FROZEN 403, other non-ACTIVE → FORBIDDEN 403.
type MutableChecker interface {
	AssertMutable(ctx context.Context, accountID int64) error
}

// UsdConverter converts an amount to USD for review-tier classification
// (settlement.RedisUsdConverter / StaticUsdConverter). Implementations
// fail closed: an unknown rate is an error, never an implicit 1.0.
type UsdConverter interface {
	ToUSD(ctx context.Context, currency string, amount decimal.Decimal) (decimal.Decimal, error)
}

// WithdrawalLimiter enforces per-account withdrawal caps
// (*risk.LimitsService.CheckWithdrawal/RecordWithdrawal). Optional: nil
// skips cap checks (risk.LimitsService owns tiered limits; funding
// enforces whatever a wired limiter reports — fail closed on its errors).
type WithdrawalLimiter interface {
	CheckWithdrawal(ctx context.Context, accountID int64, tier string, amount decimal.Decimal) error
	RecordWithdrawal(ctx context.Context, accountID int64, amount decimal.Decimal) (decimal.Decimal, error)
}

// Freezer performs the dual-controlled FROZEN transition during a
// chargeback dispute (*accounts.FreezeService.Freeze). It requires a
// Compliance Officer/Super Admin actor and a distinct approver —
// DUAL_CONTROL_REQUIRED propagates when the request omits one.
type Freezer interface {
	Freeze(ctx context.Context, actor accounts.AdminActor, accountID int64, reason, clientIP string) error
}

// OpsAlert is the settlement package's alert shape (kept as an alias so
// PublisherAlerter satisfies OpsAlerter without an adapter).
type OpsAlert = settlement.OpsAlert

// OpsAlerter raises paging-grade ops alerts (settlement.PublisherAlerter
// over NATS). Used for >$50K PENDING_REVIEW withdrawals; delivery failure
// is logged, never masks the committed state.
type OpsAlerter interface {
	Raise(ctx context.Context, a OpsAlert) error
}

// ---------------------------------------------------------------------------
// Validation + token/hash/cursor helpers (package-internal)
// ---------------------------------------------------------------------------

func errCode(code, msg string) *excerrors.Error { return excerrors.New(code, msg) }

func errf(code, format string, args ...any) *excerrors.Error {
	return excerrors.New(code, fmt.Sprintf(format, args...))
}

func wrapCode(code, msg string, err error) *excerrors.Error {
	return excerrors.Wrap(code, msg, err)
}

// normalizeCurrency uppercases and validates an ISO-4217 3-letter code.
func normalizeCurrency(s string) (string, error) {
	s = strings.ToUpper(strings.TrimSpace(s))
	if len(s) != 3 {
		return "", errf("INVALID_REQUEST", "currency %q must be a 3-letter ISO code", s)
	}
	for _, r := range s {
		if r < 'A' || r > 'Z' {
			return "", errf("INVALID_REQUEST", "currency %q must be alphabetic", s)
		}
	}
	return s, nil
}

// parseMoney parses a positive fixed-point amount at the DECIMAL(28,8)
// quantum (spec §5.3): sub-quantum precision is rejected, never rounded.
func parseMoney(s string) (decimal.Decimal, error) {
	d, err := decimal.NewFromString(strings.TrimSpace(s))
	if err != nil {
		return decimal.Zero, errf("INVALID_REQUEST", "invalid amount %q", s)
	}
	if !d.IsPositive() {
		return decimal.Zero, errCode("INVALID_REQUEST", "amount must be positive")
	}
	if !d.Round(8).Equal(d) {
		return decimal.Zero, errf("INVALID_REQUEST",
			"amount %s exceeds the 8-decimal quantum", d.String())
	}
	return d, nil
}

// decText scans a DECIMAL column selected via ::text.
func decText(s *string) (decimal.Decimal, error) {
	if s == nil {
		return decimal.Zero, nil
	}
	return decimal.NewFromString(*s)
}

// sha256Hex returns the lowercase hex sha256 of b.
func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// payloadHash fingerprints a logical request body so a replayed
// Idempotency-Key with a different payload surfaces
// IDEMPOTENCY_KEY_MISMATCH instead of replaying the wrong operation.
func payloadHash(parts ...string) string {
	return sha256Hex([]byte(strings.Join(parts, "|")))
}

// newConfirmToken mints the withdrawal confirmation token: 32 random
// bytes, returned hex-encoded; only its sha256 is persisted.
func newConfirmToken() (token, hash string, err error) {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", "", fmt.Errorf("confirm token: %w", err)
	}
	token = hex.EncodeToString(b[:])
	return token, sha256Hex([]byte(token)), nil
}

// ---------------------------------------------------------------------------
// Cursor codec — Task 5.3.42/§8.8 envelope: opaque base64 "unixnano:id".
// ---------------------------------------------------------------------------

func encodeCursor(t time.Time, id int64) string {
	return base64.RawURLEncoding.EncodeToString(
		[]byte(strconv.FormatInt(t.UnixNano(), 10) + ":" + strconv.FormatInt(id, 10)))
}

func decodeCursor(s string) (time.Time, int64, error) {
	raw, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return time.Time{}, 0, errCode("INVALID_REQUEST", "malformed cursor")
	}
	tok := string(raw)
	i := strings.LastIndexByte(tok, ':')
	if i <= 0 || i == len(tok)-1 {
		return time.Time{}, 0, errCode("INVALID_REQUEST", "malformed cursor")
	}
	nsec, err := strconv.ParseInt(tok[:i], 10, 64)
	if err != nil {
		return time.Time{}, 0, errCode("INVALID_REQUEST", "malformed cursor timestamp")
	}
	id, err := strconv.ParseInt(tok[i+1:], 10, 64)
	if err != nil {
		return time.Time{}, 0, errCode("INVALID_REQUEST", "malformed cursor id")
	}
	return time.Unix(0, nsec).UTC(), id, nil
}
