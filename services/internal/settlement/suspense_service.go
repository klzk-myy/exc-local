// suspense_service.go — Task 24.3.21 unidentified-deposit suspense
// routing (spec §17.16b, §24 #415).
//
// This is deliberately a THIN adapter over Phase-11 funding.DepositGuard
// — the guard already owns the durable behaviour:
//
//   - suspense GL posting Dr 1010_NOSTRO_{CCY} / Cr
//     2150_SUSPENSE_DEPOSITS_{CCY} (balanced, decimal-exact, idempotent
//     on bank_tx_id),
//   - the quarantine record in suspense_account_mappings (bank tx id,
//     remitter name/account, bank reference, amount, currency,
//     unmatched_reason, 48h SLA),
//   - the compliance/finance-ops alert,
//   - four-eyes ResolveSuspense (RELEASE_TO_CLIENT: reverse suspense,
//     credit 2010 customer liability + wallet available —
//     RETURN_TO_SOURCE: suspense → 2160 clearing transit + return wire),
//   - idempotent replay on duplicate bank_tx_id.
//
// NOTE on package boundaries: settlement cannot import funding
// (funding → accounts → settlement is an existing edge), so this file
// declares mirror wire/result types + the DepositScreener seam; the
// composition-root adapter translating them to funding.InboundWire lives
// in internal/api/handlers_backoffice_settlement.go. Nothing here
// re-implements the quarantine machinery — a nil screener fails closed
// (SUSPENSE_ROUTER_MISSING) rather than dropping unidentified funds.
package settlement

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/shopspring/decimal"

	excerrors "exchange/pkg/errors"
)

// CodeSuspenseRouterMissing — no DepositGuard wired; statement credits
// cannot be quarantined (fail-closed — never pretend the funds parked
// somewhere).
const CodeSuspenseRouterMissing = "SUSPENSE_ROUTER_MISSING"

// SuspenseInbound mirrors funding.InboundWire (kept package-local — see
// header for the import-cycle constraint).
type SuspenseInbound struct {
	BankTxID          string
	Rail              string // SWIFT|SEPA|FEDNOW|ACH|CHAPS|TARGET2|WIRE
	Currency          string
	Amount            decimal.Decimal
	OriginatorName    string
	OriginatorAccount string
	OriginatorBIC     string
	Reference         string
	RemittanceInfo    string
	ReceivedAt        time.Time
}

// SuspenseScreenResult mirrors the guard's ScreenResult surface the
// router consumes.
type SuspenseScreenResult struct {
	Disposition string // ACCEPTED | QUARANTINED
	SuspenseID  int64  // suspense_account_mappings.id when quarantined
	Reason      string
	Idempotent  bool
}

// SuspenseResolveOutcome mirrors funding.ResolveOutcome.
type SuspenseResolveOutcome struct {
	SuspenseID int64
	Action     string // RELEASE_TO_CLIENT | RETURN_TO_SOURCE
	Status     string
	JournalID  *int64
}

// DepositScreener is the funding.DepositGuard seam (ScreenInbound +
// ResolveSuspense); the api-layer adapter satisfies it.
type DepositScreener interface {
	ScreenInbound(ctx context.Context, w SuspenseInbound) (*SuspenseScreenResult, error)
	ResolveSuspense(ctx context.Context, suspenseID int64,
		action string, investigatorID int64, notes string) (*SuspenseResolveOutcome, error)
}

// SuspenseService routes unmatched inbound credits into the Phase-11
// quarantine pipeline and exposes the resolution surface.
type SuspenseService struct {
	guard DepositScreener
}

// NewSuspenseService wires the adapter; guard is required (fail-closed).
func NewSuspenseService(guard DepositScreener) (*SuspenseService, error) {
	if guard == nil {
		return nil, fmt.Errorf("suspense router: nil deposit guard")
	}
	return &SuspenseService{guard: guard}, nil
}

// RouteUnmatchedCredit implements SuspenseRouter for the statement
// ingestion path (statement_parser.go): converts the unmatched credit
// entry into a SuspenseInbound and drives the guard. Returns the
// suspense_account_mappings id once quarantined; a non-quarantined
// disposition (auto-accepted attribution) returns 0 — the guard resolved
// it to a client.
func (s *SuspenseService) RouteUnmatchedCredit(ctx context.Context, e ParsedEntry, bankTxID string) (int64, error) {
	if s.guard == nil {
		return 0, excerrors.New(CodeSuspenseRouterMissing,
			"suspense router: deposit guard not wired")
	}
	bankTxID = strings.TrimSpace(bankTxID)
	if bankTxID == "" {
		bankTxID = strings.TrimSpace(e.UETR)
	}
	if bankTxID == "" {
		return 0, excerrors.New("INVALID_REQUEST",
			"suspense router: bank transaction id required for quarantine")
	}
	ref := strings.TrimSpace(firstNonEmptyStr(e.EndToEndID, e.EntryRef))
	res, err := s.guard.ScreenInbound(ctx, SuspenseInbound{
		BankTxID:          bankTxID,
		Rail:              "WIRE", // statement-sourced credits carry no rail envelope
		Currency:          strings.ToUpper(e.Currency),
		Amount:            e.Amount,
		OriginatorName:    e.RemitterName,
		OriginatorAccount: e.RemitterAccount,
		Reference:         ref,
		RemittanceInfo:    e.Narrative,
		ReceivedAt:        e.ValueDate,
	})
	if err != nil {
		return 0, fmt.Errorf("suspense router: screen %s: %w", bankTxID, err)
	}
	if res == nil {
		return 0, excerrors.New(CodeSuspenseRouterMissing,
			"suspense router: guard returned nil result")
	}
	if res.Disposition == "QUARANTINED" {
		return res.SuspenseID, nil
	}
	return 0, nil // guard attributed the wire to a client deposit
}

// RouteCredit is the statement-parser-independent entry point for ops /
// reconciliation callers that hold raw wire fields rather than a
// ParsedEntry.
func (s *SuspenseService) RouteCredit(ctx context.Context, w SuspenseInbound) (*SuspenseScreenResult, error) {
	if s.guard == nil {
		return nil, excerrors.New(CodeSuspenseRouterMissing,
			"suspense router: deposit guard not wired")
	}
	return s.guard.ScreenInbound(ctx, w)
}

// Resolve applies a four-eyes quarantine resolution through the guard
// (RELEASE_TO_CLIENT | RETURN_TO_SOURCE). The guard enforces the state
// machine + GL reversal journal; the API layer enforces approver ≠
// resolver (dual control, Task 11.3.8 conventions).
func (s *SuspenseService) Resolve(ctx context.Context, suspenseID int64,
	action string, investigatorID int64, notes string) (*SuspenseResolveOutcome, error) {
	if s.guard == nil {
		return nil, excerrors.New(CodeSuspenseRouterMissing,
			"suspense router: deposit guard not wired")
	}
	return s.guard.ResolveSuspense(ctx, suspenseID, action, investigatorID, notes)
}

func firstNonEmptyStr(vs ...string) string {
	for _, v := range vs {
		if strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}
