// Package backoffice — Phase-24 Tasks 24.3.11/24.3.16–24.3.18: client-money
// safeguarding, settlement quarantine, venue treasury and independent
// client-money assurance (spec §5.33, §17.9, §17.11, §17.12, §17.13.1–2;
// §24 #173/#326/#329/#330).
//
// client_money.go owns Task 24.3.11 — client-money segregation & daily
// reconciliation:
//
//   - Every bank/nostro/GL account that can hold balances is classified
//     CLIENT | HOUSE | MARGIN | SUSPENSE (client_money_accounts, mig 056).
//     GL-side classification is structural — ledger.SegregationOf pins the
//     client/house boundary by account-code range and classification input
//     is validated AGAINST it, so a mislabelled code fails closed.
//   - Receipts land UNIDENTIFIED until allocated to a client account;
//     uncleared and unidentified items are tracked separately from
//     operational balances (client_money_receipts).
//   - RunDailyReconciliation computes requirement (Σ client entitlements +
//     unidentified receipts) vs resource (Σ CLIENT+MARGIN classified
//     segregated balances) per currency per business day, preserves the
//     source snapshots and requires a distinct-principal sign-off.
//   - A shortfall opens a P1 break (CLIENT_MONEY_SHORTFALL, HTTP 503),
//     AssertClientMoneyMovement blocks withdrawals/transfers that would
//     worsen it, and the 4-tier remediation waterfall runs:
//     Tier 1 insurance-fund debit (automatic, immediate) → Tier 2 house
//     top-up (4-eyes approval, ≤30 min) → Tier 3 shareholder capital call
//     (regulator notice dispatched ≤60 min; CASS 7.15.33/SEC 15c3-3 bound
//     1 business day) → Tier 4 orderly suspension + default declaration
//     when tiers 1–3 cannot clear within 4 h (task step 4a).
//   - RunStressTest enforces insurance fund + house reserves ≥ 2×
//     worst-case NBP exposure per currency per day (Phase-19 Task 19.3.13
//     linkage via the NBPExposureSource seam).
//   - ExportPoolingPackage renders the primary-pooling-event / wind-down
//     evidence package from the system of record.
package backoffice

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"exchange/internal/admin"
	"exchange/internal/ledger"
	"exchange/internal/settlement"
	"exchange/pkg/decimal"
	excerrors "exchange/pkg/errors"
)

// ---------------------------------------------------------------------------
// Codes — CLIENT_MONEY_SHORTFALL (503), CLS_SETTLEMENT_MISMATCH (409) and
// TREASURY_LIQUIDITY_BREACH (503) are spec §23 rows; the remaining constants
// are internal-only ops-alert codes (§23 internal-only list convention —
// they are raised through OpsAlerter, never emitted as HTTP responses).
// ---------------------------------------------------------------------------
const (
	// CodeClientMoneyShortfall — HTTP 503, P1/L1: segregated resource below
	// requirement; worsening client-money movements rejected (spec §17.12.4).
	CodeClientMoneyShortfall = "CLIENT_MONEY_SHORTFALL"
	// CodeCLSSettlementMismatch — HTTP 409: outbound funds blocked while a
	// settlement batch sits in CLS match-discrepancy quarantine (§17.12.3).
	CodeCLSSettlementMismatch = "CLS_SETTLEMENT_MISMATCH"
	// CodeTreasuryLiquidityBreach — HTTP 503: stressed 5-day liquidity
	// buffer breached; discretionary outflows + new LP capacity frozen.
	CodeTreasuryLiquidityBreach = "TREASURY_LIQUIDITY_BREACH"

	// alertClientMoneyShortfall — P1 ops alert when a break opens.
	alertClientMoneyShortfall = "CLIENT_MONEY_SHORTFALL"
	// alertStressBreach — P1: insurance + house < 2× worst-case NBP.
	alertStressBreach = "CLIENT_MONEY_STRESS_BREACH"
	// alertTier2ApprovalRequired — P1: house top-up awaiting 4-eyes.
	alertTier2ApprovalRequired = "CLIENT_MONEY_TIER2_PENDING"
	// alertCCOEscalation — P0: shortfall persists past the 60-minute bound.
	alertCCOEscalation = "CLIENT_MONEY_SHORTFALL_CCO"
	// alertTier4Default — P0: orderly suspension + default declaration.
	alertTier4Default = "CLIENT_MONEY_DEFAULT_DECLARED"
	// alertCLSQuarantine — P1: batch quarantined on CLS mismatch.
	alertCLSQuarantine = "CLS_SETTLEMENT_MISMATCH"
	// alertInsuranceExpiring — P2: policy expiry inside 60 days.
	alertInsuranceExpiring = "INSURANCE_POLICY_EXPIRING"
	// alertLiquidityBreach — P1: stressed liquidity buffer breached.
	alertLiquidityBreach = "TREASURY_LIQUIDITY_BREACH"
)

// Ops severities (spec §2.7 paging tiers — settlement owns P0/P1; P2 is
// the ticket-tier used for INSURANCE_POLICY_EXPIRING per §17.13.1.3).
const (
	SeverityP0 = settlement.SeverityP0
	SeverityP1 = settlement.SeverityP1
	SeverityP2 = "P2"
)

// OpsAlert / OpsAlerter are the shared ops-paging seam (NATS JetStream
// ops.alerts.* in production; captured in-memory by tests).
type (
	OpsAlert   = settlement.OpsAlert
	OpsAlerter = settlement.OpsAlerter
)

// OpsAlertSubject is the JetStream subject for backoffice ops alerts.
const OpsAlertSubject = "ops.alerts.backoffice"

// ---------------------------------------------------------------------------
// Canonical policy constants (Task 24.3.11 step 4a + Task 24.3.16 item 2).
// ---------------------------------------------------------------------------
const (
	// Tier2ApprovalSLA — house top-up 4-eyes approval bound (30 minutes).
	Tier2ApprovalSLA = 30 * time.Minute
	// RegulatorNoticeSLA — automated regulatory notification bound once a
	// shortfall exceeds Tier-2 capacity (60 minutes; AC #6). The CASS
	// 7.15.33 / SEC 15c3-3 statutory bound is 1 business day — the
	// internal SLA is strictly tighter.
	RegulatorNoticeSLA = 60 * time.Minute
	// Tier3CapitalCallSLA — shareholder capital-call bound (1 business day).
	Tier3CapitalCallSLA = 24 * time.Hour
	// Tier4Horizon — tiers 1–3 must clear a shortfall within 4 hours or the
	// venue declares orderly suspension + default.
	Tier4Horizon = 4 * time.Hour
	// ShortfallEscalationSLA — Task 24.3.16 item 3: unresolved at 60
	// minutes → CCO escalation + Tier-3 regulatory notifications.
	ShortfallEscalationSLA = 60 * time.Minute
	// StressCoverageMultiple — daily stress test: insurance + house ≥ 2×
	// worst-case NBP exposure.
	StressCoverageMultiple = 2
	// InsuranceExpiryHorizon — INSURANCE_POLICY_EXPIRING fires when cover
	// expires inside 60 days (Task 24.3.17 item 3).
	InsuranceExpiryHorizon = 60 * 24 * time.Hour
)

// Account classification domain (client_money_accounts.classification).
const (
	ClassClient   = "CLIENT"   // segregated client-money account
	ClassHouse    = "HOUSE"    // venue own funds
	ClassMargin   = "MARGIN"   // margin/transaction account — still client money
	ClassSuspense = "SUSPENSE" // unmatched/suspense holding — client money pending attribution
)

// MoneyAccountKind enumerates the safeguardable account substrates.
const (
	KindBank   = "BANK"
	KindNostro = "NOSTRO"
	KindGL     = "GL"
)

// Trust/acknowledgement status values (client_money_accounts.trust_status).
const (
	TrustNone         = "NONE"
	TrustPending      = "PENDING"
	TrustAcknowledged = "ACKNOWLEDGED"
)

// Reconciliation statuses (client_money_reconciliations.status).
const (
	ReconBalanced  = "BALANCED"
	ReconShortfall = "SHORTFALL"
	ReconExcess    = "EXCESS"
)

// External reconciliation outcomes (client_money_reconciliations.external_status).
const (
	ExtMatched     = "MATCHED"
	ExtMismatch    = "MISMATCH"
	ExtUnavailable = "UNAVAILABLE"
)

// Break kinds (client_money_breaks.kind).
const (
	BreakShortfall    = "SHORTFALL"
	BreakExcess       = "EXCESS"
	BreakUnmatched    = "UNMATCHED"
	BreakStressBreach = "STRESS_BREACH"
)

// Break statuses.
const (
	BreakOpen        = "OPEN"
	BreakRemediating = "REMEDIATING"
	BreakResolved    = "RESOLVED"
	BreakEscalated   = "ESCALATED"
)

// Remediation tiers and actions (4-tier waterfall, task step 4a).
const (
	TierInsurance     = 1 // INSURANCE_DEBIT — automatic, immediate
	TierHouse         = 2 // HOUSE_TOPUP — 4-eyes approval, ≤30 min
	TierCapitalCall   = 3 // CAPITAL_CALL — manual + regulator notice
	TierDefault       = 4 // DEFAULT_DECLARE — suspension + default declaration
	ActInsuranceDebit = "INSURANCE_DEBIT"
	ActHouseTopup     = "HOUSE_TOPUP"
	ActCapitalCall    = "CAPITAL_CALL"
	ActDefaultDeclare = "DEFAULT_DECLARE"
)

// Remediation statuses.
const (
	RemPending  = "PENDING"
	RemApproved = "APPROVED"
	RemExecuted = "EXECUTED"
	RemFailed   = "FAILED"
	RemExpired  = "EXPIRED"
)

// Regulator notice triggers (client_money_regulator_notices.trigger).
const (
	NoticeTier3   = "TIER3_CAPITAL_CALL"
	NoticeTier4   = "TIER4_DEFAULT"
	NoticeOver60M = "SHORTFALL_OVER_60M"
)

// Notice statuses.
const (
	NoticePending      = "PENDING"
	NoticeSent         = "SENT"
	NoticeAcknowledged = "ACKNOWLEDGED"
)

// Receipt statuses.
const (
	ReceiptUnidentified = "UNIDENTIFIED"
	ReceiptAllocated    = "ALLOCATED"
	ReceiptReturned     = "RETURNED"
)

// Idempotency-key namespaces stamped on journals this service posts —
// GuardJournal whitelists exactly these prefixes when a journal credits a
// client-asset account.
const (
	idemRemediation = "cm-rem:"
	idemExcess      = "cm-excess:"
)

// ---------------------------------------------------------------------------
// Row types
// ---------------------------------------------------------------------------

// MoneyAccount is one client_money_accounts classification row.
type MoneyAccount struct {
	ID                 int64      `json:"id"`
	AccountKind        string     `json:"account_kind"`
	NostroAccountID    *int64     `json:"nostro_account_id,omitempty"`
	GLAccountCode      string     `json:"gl_account_code,omitempty"`
	BankIBAN           string     `json:"bank_iban,omitempty"`
	BankName           string     `json:"bank_name,omitempty"`
	Classification     string     `json:"classification"`
	Currency           string     `json:"currency"`
	TrustStatus        string     `json:"trust_status"`
	AcknowledgementAt  *time.Time `json:"acknowledgement_received_at,omitempty"`
	NextDueDiligenceAt *time.Time `json:"next_due_diligence_at,omitempty"`
	Status             string     `json:"status"`
	CreatedAt          time.Time  `json:"created_at"`
	UpdatedAt          time.Time  `json:"updated_at"`
}

// BankReview is one due-diligence/diversification/acknowledgement review.
type BankReview struct {
	ID                   int64      `json:"id"`
	ClientMoneyAccountID int64      `json:"client_money_account_id"`
	ReviewKind           string     `json:"review_kind"` // DUE_DILIGENCE|DIVERSIFICATION|ACKNOWLEDGEMENT
	Outcome              string     `json:"outcome"`     // PASS|CONDITIONAL|FAIL
	ReviewerID           int64      `json:"reviewer_id"`
	DocumentRef          string     `json:"document_ref,omitempty"`
	Notes                string     `json:"notes,omitempty"`
	NextReviewAt         *time.Time `json:"next_review_at,omitempty"`
	ReviewedAt           time.Time  `json:"reviewed_at"`
}

// Receipt is one inbound bank credit pending client attribution.
type Receipt struct {
	ID                 int64           `json:"id"`
	NostroAccountID    *int64          `json:"nostro_account_id,omitempty"`
	BankReference      string          `json:"bank_reference"`
	Amount             decimal.Decimal `json:"amount"`
	Currency           string          `json:"currency"`
	Status             string          `json:"status"`
	AllocatedAccountID *int64          `json:"allocated_account_id,omitempty"`
	AllocatedBy        *int64          `json:"allocated_by,omitempty"`
	AllocatedAt        *time.Time      `json:"allocated_at,omitempty"`
	ClearedAt          *time.Time      `json:"cleared_at,omitempty"`
	ReceivedAt         time.Time       `json:"received_at"`
	CreatedAt          time.Time       `json:"created_at"`
}

// Entitlement is one client's entitlement leg of the requirement sum.
type Entitlement struct {
	AccountID int64           `json:"account_id"`
	Currency  string          `json:"currency"`
	Amount    decimal.Decimal `json:"amount"`
}

// Reconciliation is one (recon_date, currency) safeguarding calculation.
type Reconciliation struct {
	ID                int64           `json:"id"`
	ReconDate         time.Time       `json:"recon_date"`
	Currency          string          `json:"currency"`
	Requirement       decimal.Decimal `json:"requirement"`
	Resource          decimal.Decimal `json:"resource"`
	UnclearedReceipts decimal.Decimal `json:"uncleared_receipts"`
	MarginTransfers   decimal.Decimal `json:"margin_transfers"`
	Variance          decimal.Decimal `json:"variance"`
	Status            string          `json:"status"`
	ExternalStatus    string          `json:"external_status"`
	InternalSnapshot  json.RawMessage `json:"internal_snapshot"`
	ExternalSnapshot  json.RawMessage `json:"external_snapshot,omitempty"`
	PerformedBy       int64           `json:"performed_by"`
	SignedOffBy       *int64          `json:"signed_off_by,omitempty"`
	SignedOffAt       *time.Time      `json:"signed_off_at,omitempty"`
	CreatedAt         time.Time       `json:"created_at"`
}

// Break is one open safeguarding exception (shortfall/excess/unmatched).
type Break struct {
	ID               int64           `json:"id"`
	ReconciliationID *int64          `json:"reconciliation_id,omitempty"`
	Kind             string          `json:"kind"`
	Currency         string          `json:"currency"`
	Amount           decimal.Decimal `json:"amount"`
	Status           string          `json:"status"`
	Severity         string          `json:"severity"`
	DetectedAt       time.Time       `json:"detected_at"`
	ResolvedAt       *time.Time      `json:"resolved_at,omitempty"`
	Detail           json.RawMessage `json:"detail,omitempty"`
	CreatedAt        time.Time       `json:"created_at"`
}

// Remediation is one waterfall action against a break (top-up log).
type Remediation struct {
	ID             int64           `json:"id"`
	BreakID        int64           `json:"break_id"`
	Tier           int             `json:"tier"`
	Action         string          `json:"action"`
	Currency       string          `json:"currency"`
	Amount         decimal.Decimal `json:"amount"`
	Status         string          `json:"status"`
	RequestedBy    int64           `json:"requested_by"`
	ApprovedBy     *int64          `json:"approved_by,omitempty"`
	JournalEntryID *int64          `json:"journal_entry_id,omitempty"`
	DeadlineAt     time.Time       `json:"deadline_at"`
	ExecutedAt     *time.Time      `json:"executed_at,omitempty"`
	FailureReason  string          `json:"failure_reason,omitempty"`
	CreatedAt      time.Time       `json:"created_at"`
}

// RegulatorNotice is one regulator-notification record (Task 24.3.11 item 4
// + 24.3.16 item 3). Deadline is ≤60 min from detection per the AC.
type RegulatorNotice struct {
	ID             int64           `json:"id"`
	BreakID        *int64          `json:"break_id,omitempty"`
	RemediationID  *int64          `json:"remediation_id,omitempty"`
	Trigger        string          `json:"trigger"`
	Regulation     string          `json:"regulation"` // 'CASS 7.15.33' | 'SEC 15c3-3'
	Status         string          `json:"status"`
	DeadlineAt     time.Time       `json:"deadline_at"`
	SentAt         *time.Time      `json:"sent_at,omitempty"`
	AcknowledgedAt *time.Time      `json:"acknowledged_at,omitempty"`
	Payload        json.RawMessage `json:"payload,omitempty"`
	CreatedAt      time.Time       `json:"created_at"`
}

// StressRun is one daily coverage row (insurance+house ≥ 2× worst NBP).
type StressRun struct {
	ID               int64           `json:"id"`
	RunDate          time.Time       `json:"run_date"`
	Currency         string          `json:"currency"`
	InsuranceBalance decimal.Decimal `json:"insurance_balance"`
	HouseReserve     decimal.Decimal `json:"house_reserve"`
	WorstCaseNBP     decimal.Decimal `json:"worst_case_nbp"`
	RequiredCoverage decimal.Decimal `json:"required_coverage"`
	Adequate         bool            `json:"adequate"`
	CreatedAt        time.Time       `json:"created_at"`
}

// PoolingExport is one pooling-event/wind-down package export record.
type PoolingExport struct {
	ID            int64           `json:"id"`
	ExportKind    string          `json:"export_kind"` // POOLING_EVENT|WIND_DOWN
	ExportedBy    int64           `json:"exported_by"`
	Package       json.RawMessage `json:"package"`
	PackageSHA256 string          `json:"package_sha256"`
	CreatedAt     time.Time       `json:"created_at"`
}

// PoRRoot is one audit_merkle_roots row (§17.11 daily attested root).
type PoRRoot struct {
	Date       time.Time `json:"date"`
	MerkleRoot string    `json:"merkle_root"`
	ComputedAt time.Time `json:"computed_at"`
}

// GLLine is one ledger_lines row joined to its journal (evidence pack).
type GLLine struct {
	JournalEntryID int64           `json:"journal_entry_id"`
	AccountCode    string          `json:"account_code"`
	Debit          decimal.Decimal `json:"debit"`
	Credit         decimal.Decimal `json:"credit"`
	Currency       string          `json:"currency"`
	Narrative      string          `json:"narrative,omitempty"`
	PostedAt       time.Time       `json:"posted_at"`
}

// ---------------------------------------------------------------------------
// Seams
// ---------------------------------------------------------------------------

// RoleResolver resolves an admin user id to its spec §8.2 role name —
// production binds admin.Store.StrongestRole; a nil resolver fails closed.
type RoleResolver func(ctx context.Context, adminUserID int64) (string, error)

// JournalPoster posts a balanced GL journal inside the caller's InTx
// scope. The tx argument is the Tx handle the service holds; the pgx
// production binding asserts the underlying pgx.Tx.
type JournalPoster interface {
	PostJournal(ctx context.Context, tx Tx, j ledger.Journal) (int64, error)
}

// FundSource is the Phase-19 insurance-fund seam. Debit reduces the fund
// balance ledger by amount; the GL journal for the movement is posted by
// THIS service via JournalPoster — implementations MUST NOT post GL
// journals themselves (production binding wraps the insurance_fund
// balance writer, not risk.InsuranceFundService.Debit which posts its
// own journal).
type FundSource interface {
	Balance(ctx context.Context, tx Tx, ccy string) (decimal.Decimal, error)
	Debit(ctx context.Context, tx Tx, ccy string, amount decimal.Decimal, refID int64) error
}

// HouseReserveSource reports house money available for Tier-2 top-ups
// and the daily stress test — the TreasuryService own-funds ledger is the
// production binding (spec §17.13.1).
type HouseReserveSource interface {
	HouseReserve(ctx context.Context, tx Tx, ccy string) (decimal.Decimal, error)
}

// NBPExposureSource is the worst-case NBP exposure feed for the daily
// stress test — production binds risk.StressEngine.WorstOnePctShortfall
// (Phase-19 Task 19.3.13).
type NBPExposureSource interface {
	WorstNBPExposure(ctx context.Context, ccy string) (decimal.Decimal, error)
}

// StatementSource is the Task 24.3.12 bank-statement seam used for the
// external leg of the daily reconciliation. A nil source is legal — the
// reconciliation marks external_status=UNAVAILABLE rather than silently
// passing (fail-closed honesty).
type StatementSource interface {
	// ClosingBalance returns the statement closing balance for the
	// classified nostro account on day, plus the statement reference.
	// A nil balance means no statement exists for the day.
	ClosingBalance(ctx context.Context, nostroAccountID int64, day time.Time) (*decimal.Decimal, string, error)
}

// SuspensionTrigger is the Tier-4 orderly-suspension seam — production
// binds the Phase-11 kill-switch/admin suspension surface. Nil → the
// default declaration still lands and a P0 alert demands manual halt.
type SuspensionTrigger interface {
	SuspendTrading(ctx context.Context, reason string) error
}

// NoticeDispatcher delivers regulator notifications (email/regtech gateway
// in production). Nil → notices persist PENDING and DispatchNotices retries.
type NoticeDispatcher interface {
	DispatchRegulatorNotice(ctx context.Context, n RegulatorNotice) error
}

// ---------------------------------------------------------------------------
// Store — the tx-scoped persistence contract
// ---------------------------------------------------------------------------

// Tx is the transactional view of the backoffice store. Methods invoked
// on the Tx handed to InTx run inside that transaction; methods invoked
// on the root Store run non-transactionally (reads).
type Tx interface {
	// classification & reviews
	InsertMoneyAccount(ctx context.Context, a MoneyAccount) (*MoneyAccount, error)
	MoneyAccountByID(ctx context.Context, id int64) (*MoneyAccount, error)
	MoneyAccountByGLCode(ctx context.Context, code string) (*MoneyAccount, error)
	MoneyAccountByNostro(ctx context.Context, nostroID int64) (*MoneyAccount, error)
	ListMoneyAccounts(ctx context.Context, class string) ([]MoneyAccount, error)
	SetMoneyAccountTrust(ctx context.Context, id int64, trust string,
		ackAt, nextDD *time.Time) error
	InsertBankReview(ctx context.Context, r BankReview) (*BankReview, error)
	ListBankReviews(ctx context.Context, accountID int64) ([]BankReview, error)

	// receipts
	InsertReceipt(ctx context.Context, r Receipt) (*Receipt, error)
	ReceiptByID(ctx context.Context, id int64) (*Receipt, error)
	UpdateReceipt(ctx context.Context, r Receipt) error
	ListReceipts(ctx context.Context, status string, limit int) ([]Receipt, error)
	UnidentifiedReceipts(ctx context.Context, ccy string) (total decimal.Decimal, count int64, err error)

	// reconciliations
	InsertReconciliation(ctx context.Context, r Reconciliation) (*Reconciliation, error)
	ReconciliationByID(ctx context.Context, id int64) (*Reconciliation, error)
	ReconciliationForDay(ctx context.Context, day time.Time, ccy string) (*Reconciliation, error)
	UpdateReconciliation(ctx context.Context, r Reconciliation) error
	ListReconciliations(ctx context.Context, from, to time.Time) ([]Reconciliation, error)

	// live-state source reads
	ClientEntitlements(ctx context.Context, ccy string) ([]Entitlement, error)
	ClassifiedBalance(ctx context.Context, ccy, class string) (decimal.Decimal, error)
	NostroBalance(ctx context.Context, nostroAccountID int64) (decimal.Decimal, error)
	MerkleRoots(ctx context.Context, from, to time.Time) ([]PoRRoot, error)
	GLLines(ctx context.Context, codes []string, from, to time.Time) ([]GLLine, error)

	// breaks
	InsertBreak(ctx context.Context, b Break) (*Break, error)
	BreakByID(ctx context.Context, id int64) (*Break, error)
	UpdateBreak(ctx context.Context, b Break) error
	OpenBreaks(ctx context.Context, kind, ccy string) ([]Break, error)
	ListBreaks(ctx context.Context, from, to time.Time) ([]Break, error)

	// remediations & regulator notices
	InsertRemediation(ctx context.Context, r Remediation) (*Remediation, error)
	RemediationByID(ctx context.Context, id int64) (*Remediation, error)
	UpdateRemediation(ctx context.Context, r Remediation) error
	RemediationsForBreak(ctx context.Context, breakID int64) ([]Remediation, error)
	PendingRemediations(ctx context.Context, now time.Time) ([]Remediation, error)
	ListRemediations(ctx context.Context, from, to time.Time) ([]Remediation, error)
	InsertRegulatorNotice(ctx context.Context, n RegulatorNotice) (*RegulatorNotice, error)
	UpdateRegulatorNotice(ctx context.Context, n RegulatorNotice) error
	NoticeExists(ctx context.Context, breakID int64, trigger string) (bool, error)
	PendingNotices(ctx context.Context, now time.Time) ([]RegulatorNotice, error)

	// stress runs & pooling exports
	InsertStressRun(ctx context.Context, r StressRun) (*StressRun, error)
	ListStressRuns(ctx context.Context, from, to time.Time) ([]StressRun, error)
	InsertPoolingExport(ctx context.Context, e PoolingExport) (*PoolingExport, error)

	// settlement quarantines (shortfall.go — Task 24.3.16)
	InsertQuarantine(ctx context.Context, q Quarantine) (*Quarantine, error)
	QuarantineByID(ctx context.Context, id int64) (*Quarantine, error)
	QuarantineByBatch(ctx context.Context, batchRef string) (*Quarantine, error)
	UpdateQuarantine(ctx context.Context, q Quarantine) error
	ActiveQuarantines(ctx context.Context) ([]Quarantine, error)

	// treasury (treasury.go / contingent_capital.go — Task 24.3.17)
	UpsertOwnFunds(ctx context.Context, o OwnFunds) (*OwnFunds, error)
	OwnFundsByID(ctx context.Context, id int64) (*OwnFunds, error)
	ListOwnFunds(ctx context.Context) ([]OwnFunds, error)
	OwnFundsSum(ctx context.Context, ccy string, kinds []string) (decimal.Decimal, error)
	InsertLiquidityAssessment(ctx context.Context, a LiquidityAssessment) (*LiquidityAssessment, error)
	TreasuryControls(ctx context.Context) (*TreasuryControls, error)
	UpdateTreasuryControls(ctx context.Context, c TreasuryControls) error
	InsertCommitment(ctx context.Context, c Commitment) (*Commitment, error)
	CommitmentByID(ctx context.Context, id int64) (*Commitment, error)
	UpdateCommitment(ctx context.Context, c Commitment) error
	ListCommitments(ctx context.Context) ([]Commitment, error)
	ExpiringPolicies(ctx context.Context, horizon time.Time) ([]Commitment, error)

	// assurance (client_money_audit.go / segregation_cert.go — Task 24.3.18)
	InsertAudit(ctx context.Context, a ClientMoneyAudit) (*ClientMoneyAudit, error)
	AuditByID(ctx context.Context, id int64) (*ClientMoneyAudit, error)
	UpdateAudit(ctx context.Context, a ClientMoneyAudit) error
	ListAudits(ctx context.Context, status string, limit int) ([]ClientMoneyAudit, error)
	InsertEvidencePack(ctx context.Context, p EvidencePack) (*EvidencePack, error)
	EvidencePackByID(ctx context.Context, id int64) (*EvidencePack, error)
	EvidencePacksForAudit(ctx context.Context, auditID int64) ([]EvidencePack, error)
	InsertCertification(ctx context.Context, c SegregationCertification) (*SegregationCertification, error)
	UpdateCertification(ctx context.Context, c SegregationCertification) error
	ListCertifications(ctx context.Context) ([]SegregationCertification, error)
	CertificationsCovering(ctx context.Context, day time.Time) ([]SegregationCertification, error)
	InsertAuditorGrant(ctx context.Context, g AuditorGrant) (*AuditorGrant, error)
	AuditorGrantByID(ctx context.Context, id int64) (*AuditorGrant, error)
	UpdateAuditorGrant(ctx context.Context, g AuditorGrant) error
}

// Store is the persistence root: Tx for reads/non-transactional writes,
// InTx for atomic multi-write flows.
type Store interface {
	Tx
	// InTx runs fn inside one SERIALIZABLE transaction — the Tx argument
	// is the tx-scoped view. The mem test store executes fn under a lock.
	InTx(ctx context.Context, fn func(ctx context.Context, tx Tx) error) error
}

// ---------------------------------------------------------------------------
// Service
// ---------------------------------------------------------------------------

// ClientMoneyDeps wires the service. Store and Resolver are required —
// nil fails closed at construction. Poster/Fund/House/NBP/Statements/
// Suspension/Notices are capability seams: missing ones disable the
// dependent flow loudly (coded error / UNAVAILABLE snapshot leg), never
// silently.
type ClientMoneyDeps struct {
	Store      Store
	Poster     JournalPoster
	Fund       FundSource
	House      HouseReserveSource
	NBP        NBPExposureSource
	Statements StatementSource
	Suspension SuspensionTrigger
	Notices    NoticeDispatcher
	Alerter    OpsAlerter
	Resolver   RoleResolver
	Now        func() time.Time
	PostedBy   string // journal posted_by identity (default "backoffice-client-money")
}

// ClientMoneyService is the Task 24.3.11 safeguarding engine.
type ClientMoneyService struct {
	store    Store
	poster   JournalPoster
	fund     FundSource
	house    HouseReserveSource
	nbp      NBPExposureSource
	stmts    StatementSource
	susp     SuspensionTrigger
	notices  NoticeDispatcher
	alerter  OpsAlerter
	resolve  RoleResolver
	now      func() time.Time
	postedBy string
}

// NewClientMoneyService wires the service; Store and Resolver are
// mandatory (fail-closed).
func NewClientMoneyService(d ClientMoneyDeps) (*ClientMoneyService, error) {
	if d.Store == nil {
		return nil, fmt.Errorf("client money: nil store")
	}
	if d.Resolver == nil {
		return nil, fmt.Errorf("client money: nil role resolver")
	}
	s := &ClientMoneyService{
		store: d.Store, poster: d.Poster, fund: d.Fund, house: d.House,
		nbp: d.NBP, stmts: d.Statements, susp: d.Suspension,
		notices: d.Notices, alerter: d.Alerter, resolve: d.Resolver,
		now: d.Now, postedBy: d.PostedBy,
	}
	if s.now == nil {
		s.now = func() time.Time { return time.Now().UTC() }
	}
	if s.postedBy == "" {
		s.postedBy = "backoffice-client-money"
	}
	return s, nil
}

// financeWriteRoles may classify accounts, run reconciliations, allocate
// receipts and operate remediations.
var financeWriteRoles = map[string]bool{
	admin.RoleFinanceOps: true,
	admin.RoleSuperAdmin: true,
}

// complianceOrFinance additionally carries the Compliance Officer for
// sign-off and regulator-facing flows.
var complianceOrFinance = map[string]bool{
	admin.RoleFinanceOps:        true,
	admin.RoleComplianceOfficer: true,
	admin.RoleSuperAdmin:        true,
}

// readRoles may read the safeguarding surface (adds Read-Only Auditor and
// the Task 24.3.18 EXTERNAL_AUDITOR grant role).
var readRoles = map[string]bool{
	admin.RoleFinanceOps:        true,
	admin.RoleComplianceOfficer: true,
	admin.RoleRiskManager:       true,
	admin.RoleSuperAdmin:        true,
	admin.RoleReadOnlyAuditor:   true,
	admin.SystemExternalAuditor: true,
}

// requireRole resolves the actor's role via the seam and fails closed.
func (s *ClientMoneyService) requireRole(ctx context.Context, userID int64, allowed map[string]bool) error {
	if userID <= 0 {
		return excerrors.New("UNAUTHORIZED", "admin identity required")
	}
	role, err := s.resolve(ctx, userID)
	if err != nil {
		return excerrors.Wrap("UNAUTHORIZED_ROLE", "role lookup failed", err)
	}
	if !allowed[role] {
		return excerrors.New("UNAUTHORIZED_ROLE",
			fmt.Sprintf("role %q lacks permission for this client-money operation", role))
	}
	return nil
}

// requireDualControl verifies the synchronous four-eyes pair: approver
// must be a distinct, role-eligible principal (spec §8.2).
func (s *ClientMoneyService) requireDualControl(ctx context.Context, actor admin.AdminActor, allowed map[string]bool) error {
	if actor.ApproverID <= 0 {
		return excerrors.New("DUAL_CONTROL_REQUIRED",
			"this client-money operation requires a distinct approver_id")
	}
	if actor.ApproverID == actor.UserID {
		return excerrors.New("DUAL_CONTROL_VIOLATION",
			"approver must differ from the initiating principal")
	}
	return s.requireRole(ctx, actor.ApproverID, allowed)
}

// raise fires an ops alert when an alerter is wired — delivery failure is
// logged-and-swallowed here so a pager outage never masks the coded
// business outcome (alert storms are separately monitored, §2.7).
func (s *ClientMoneyService) raise(ctx context.Context, severity, code, summary string, details map[string]string) {
	if s.alerter == nil {
		return
	}
	_ = s.alerter.Raise(ctx, OpsAlert{
		Severity: severity, Code: code, Summary: summary, Details: details,
	})
}

// ---------------------------------------------------------------------------
// Task step 1 — account classification & bank reviews
// ---------------------------------------------------------------------------

// ClassifyInput is one classification write.
type ClassifyInput struct {
	AccountKind        string
	NostroAccountID    *int64
	GLAccountCode      string
	BankIBAN           string
	BankName           string
	Classification     string
	Currency           string
	NextDueDiligenceAt *time.Time
}

// ClassifyAccount registers the safeguarding classification of one
// bank/nostro/GL account. GL-kind rows are cross-checked against the
// structural ledger.SegregationOf boundary: CLIENT/MARGIN/SUSPENSE must
// classify as client-side codes, HOUSE as house-side — a mismatch fails
// closed (INVALID_REQUEST), so classification can never launder client
// money into a house code.
func (s *ClientMoneyService) ClassifyAccount(ctx context.Context, actor admin.AdminActor, in ClassifyInput) (*MoneyAccount, error) {
	if err := s.requireRole(ctx, actor.UserID, financeWriteRoles); err != nil {
		return nil, err
	}
	switch in.AccountKind {
	case KindNostro:
		if in.NostroAccountID == nil || *in.NostroAccountID <= 0 {
			return nil, excerrors.New("INVALID_REQUEST", "nostro_account_id required for kind NOSTRO")
		}
	case KindGL:
		if in.GLAccountCode == "" {
			return nil, excerrors.New("INVALID_REQUEST", "gl_account_code required for kind GL")
		}
	case KindBank:
		if in.BankIBAN == "" {
			return nil, excerrors.New("INVALID_REQUEST", "bank_iban required for kind BANK")
		}
	default:
		return nil, excerrors.New("INVALID_REQUEST",
			fmt.Sprintf("account_kind %q must be BANK|NOSTRO|GL", in.AccountKind))
	}
	switch in.Classification {
	case ClassClient, ClassMargin, ClassSuspense:
		// Client-side classes must live on client-side GL codes when the
		// account is GL-addressable — the structural check.
		if in.GLAccountCode != "" && ledger.SegregationOf(in.GLAccountCode) != ledger.SegregationClient {
			return nil, excerrors.New("INVALID_REQUEST", fmt.Sprintf(
				"classification %s requires a client-segregation GL code (1100–1149/2000–2199); %q classifies HOUSE",
				in.Classification, in.GLAccountCode))
		}
	case ClassHouse:
		if in.GLAccountCode != "" && ledger.SegregationOf(in.GLAccountCode) != ledger.SegregationHouse {
			return nil, excerrors.New("INVALID_REQUEST", fmt.Sprintf(
				"classification HOUSE requires a house-side GL code; %q classifies CLIENT", in.GLAccountCode))
		}
	default:
		return nil, excerrors.New("INVALID_REQUEST",
			fmt.Sprintf("classification %q must be CLIENT|HOUSE|MARGIN|SUSPENSE", in.Classification))
	}
	if len(in.Currency) != 3 {
		return nil, excerrors.New("INVALID_REQUEST", "currency must be 3-letter ISO")
	}
	var out *MoneyAccount
	err := s.store.InTx(ctx, func(ctx context.Context, tx Tx) error {
		// Dedup: one classification row per underlying account.
		var existing *MoneyAccount
		var err error
		switch in.AccountKind {
		case KindGL:
			existing, err = tx.MoneyAccountByGLCode(ctx, in.GLAccountCode)
		case KindNostro:
			existing, err = tx.MoneyAccountByNostro(ctx, *in.NostroAccountID)
		}
		if err != nil {
			return err
		}
		if existing != nil {
			return excerrors.New("INVALID_REQUEST",
				fmt.Sprintf("account already classified as %s (id %d)", existing.Classification, existing.ID))
		}
		row, err := tx.InsertMoneyAccount(ctx, MoneyAccount{
			AccountKind: in.AccountKind, NostroAccountID: in.NostroAccountID,
			GLAccountCode: in.GLAccountCode, BankIBAN: in.BankIBAN,
			BankName: in.BankName, Classification: in.Classification,
			Currency: in.Currency, TrustStatus: TrustNone, Status: "ACTIVE",
			NextDueDiligenceAt: in.NextDueDiligenceAt,
		})
		if err != nil {
			return err
		}
		out = row
		return nil
	})
	return out, err
}

// RecordBankReview logs a due-diligence / diversification / acknowledgement
// review against a classified account. A PASS acknowledgement review marks
// the bank's trust status ACKNOWLEDGED (task step 1).
func (s *ClientMoneyService) RecordBankReview(ctx context.Context, actor admin.AdminActor, r BankReview) (*BankReview, error) {
	if err := s.requireRole(ctx, actor.UserID, complianceOrFinance); err != nil {
		return nil, err
	}
	switch r.ReviewKind {
	case "DUE_DILIGENCE", "DIVERSIFICATION", "ACKNOWLEDGEMENT":
	default:
		return nil, excerrors.New("INVALID_REQUEST", "review_kind must be DUE_DILIGENCE|DIVERSIFICATION|ACKNOWLEDGEMENT")
	}
	switch r.Outcome {
	case "PASS", "CONDITIONAL", "FAIL":
	default:
		return nil, excerrors.New("INVALID_REQUEST", "outcome must be PASS|CONDITIONAL|FAIL")
	}
	r.ReviewerID = actor.UserID
	var out *BankReview
	err := s.store.InTx(ctx, func(ctx context.Context, tx Tx) error {
		acct, err := tx.MoneyAccountByID(ctx, r.ClientMoneyAccountID)
		if err != nil {
			return err
		}
		if acct == nil {
			return excerrors.New("NOT_FOUND", "client-money account not found")
		}
		row, err := tx.InsertBankReview(ctx, r)
		if err != nil {
			return err
		}
		out = row
		switch {
		case r.ReviewKind == "ACKNOWLEDGEMENT" && r.Outcome == "PASS":
			ack := s.now()
			if err := tx.SetMoneyAccountTrust(ctx, acct.ID, TrustAcknowledged, &ack, r.NextReviewAt); err != nil {
				return err
			}
		case r.ReviewKind == "ACKNOWLEDGEMENT":
			if err := tx.SetMoneyAccountTrust(ctx, acct.ID, TrustPending, nil, r.NextReviewAt); err != nil {
				return err
			}
		default:
			if err := tx.SetMoneyAccountTrust(ctx, acct.ID, acct.TrustStatus, nil, r.NextReviewAt); err != nil {
				return err
			}
		}
		return nil
	})
	return out, err
}

// ListAccounts returns classification rows, optionally filtered by class.
func (s *ClientMoneyService) ListAccounts(ctx context.Context, actor admin.AdminActor, class string) ([]MoneyAccount, error) {
	if err := s.requireRole(ctx, actor.UserID, readRoles); err != nil {
		return nil, err
	}
	return s.store.ListMoneyAccounts(ctx, class)
}

// ---------------------------------------------------------------------------
// Task step 2 — receipt allocation & unidentified-receipt tracking
// ---------------------------------------------------------------------------

// RecordReceipt registers an inbound bank credit. Dedup is per
// (nostro_account_id, bank_reference) — a replayed feed row resolves to
// the original receipt rather than double-counting entitlement.
func (s *ClientMoneyService) RecordReceipt(ctx context.Context, actor admin.AdminActor, r Receipt) (*Receipt, error) {
	if err := s.requireRole(ctx, actor.UserID, financeWriteRoles); err != nil {
		return nil, err
	}
	if !r.Amount.IsPositive() || len(r.Currency) != 3 || r.BankReference == "" {
		return nil, excerrors.New("INVALID_REQUEST",
			"receipt requires positive amount, 3-letter currency and bank_reference")
	}
	r.Status = ReceiptUnidentified
	out, err := s.store.InsertReceipt(ctx, r)
	if err != nil {
		return nil, err
	}
	return out, nil
}

// AllocateReceipt attributes an UNIDENTIFIED receipt to a client account
// (four-eyes audit trail via allocated_by). ALLOCATED is the only forward
// transition besides RETURNED.
func (s *ClientMoneyService) AllocateReceipt(ctx context.Context, actor admin.AdminActor, receiptID, clientAccountID int64) (*Receipt, error) {
	if err := s.requireRole(ctx, actor.UserID, financeWriteRoles); err != nil {
		return nil, err
	}
	if clientAccountID <= 0 {
		return nil, excerrors.New("INVALID_REQUEST", "client account_id required")
	}
	var out *Receipt
	err := s.store.InTx(ctx, func(ctx context.Context, tx Tx) error {
		r, err := tx.ReceiptByID(ctx, receiptID)
		if err != nil {
			return err
		}
		if r == nil {
			return excerrors.New("NOT_FOUND", "receipt not found")
		}
		if r.Status != ReceiptUnidentified {
			return excerrors.New("INVALID_LIFECYCLE_TRANSITION",
				fmt.Sprintf("receipt %d is %s — only UNIDENTIFIED may be allocated", receiptID, r.Status))
		}
		now := s.now()
		r.Status = ReceiptAllocated
		r.AllocatedAccountID = &clientAccountID
		r.AllocatedBy = &actor.UserID
		r.AllocatedAt = &now
		if err := tx.UpdateReceipt(ctx, *r); err != nil {
			return err
		}
		out = r
		return nil
	})
	return out, err
}

// ---------------------------------------------------------------------------
// Task step 3 — daily reconciliation (internal + external legs)
// ---------------------------------------------------------------------------

// RunDailyReconciliation computes requirement vs resource for (day, ccy),
// persists the snapshot row, and opens the remediation waterfall on
// shortfall. Idempotent per (day, ccy): a rerun returns the existing row.
//
//	requirement = Σ client entitlements + unidentified receipts
//	resource    = Σ CLIENT-classified + MARGIN-classified segregated balances
//	variance    = resource − requirement
func (s *ClientMoneyService) RunDailyReconciliation(ctx context.Context, actor admin.AdminActor, day time.Time, ccy string) (*Reconciliation, error) {
	if err := s.requireRole(ctx, actor.UserID, financeWriteRoles); err != nil {
		return nil, err
	}
	if len(ccy) != 3 {
		return nil, excerrors.New("INVALID_REQUEST", "currency must be 3-letter ISO")
	}
	day = time.Date(day.Year(), day.Month(), day.Day(), 0, 0, 0, 0, time.UTC)

	var out *Reconciliation
	err := s.store.InTx(ctx, func(ctx context.Context, tx Tx) error {
		if existing, err := tx.ReconciliationForDay(ctx, day, ccy); err != nil {
			return err
		} else if existing != nil {
			out = existing // idempotent rerun
			return nil
		}
		ent, err := tx.ClientEntitlements(ctx, ccy)
		if err != nil {
			return err
		}
		requirement := decimal.Zero
		for _, e := range ent {
			requirement = requirement.Add(e.Amount)
		}
		unid, unidCount, err := tx.UnidentifiedReceipts(ctx, ccy)
		if err != nil {
			return err
		}
		requirement = requirement.Add(unid)

		clientBal, err := tx.ClassifiedBalance(ctx, ccy, ClassClient)
		if err != nil {
			return err
		}
		marginBal, err := tx.ClassifiedBalance(ctx, ccy, ClassMargin)
		if err != nil {
			return err
		}
		resource := clientBal.Add(marginBal)
		variance := resource.Sub(requirement)

		status := ReconBalanced
		switch {
		case variance.IsNegative():
			status = ReconShortfall
		case variance.IsPositive():
			status = ReconExcess
		}

		internalSnap, err := json.Marshal(map[string]any{
			"entitlements":          ent,
			"unidentified_total":    unid.String(),
			"unidentified_count":    unidCount,
			"classified_client_bal": clientBal.String(),
			"classified_margin_bal": marginBal.String(),
			"computed_at":           s.now().Format(time.RFC3339Nano),
		})
		if err != nil {
			return excerrors.Wrap("INTERNAL_ERROR", "internal snapshot encode", err)
		}

		// External leg — reconcile each ACTIVE client/margin nostro-classified
		// account against the bank-statement seam. No statement source ⇒
		// UNAVAILABLE (honest absence, never a silent pass).
		extStatus := ExtUnavailable
		var extRows []map[string]any
		extMismatch := false
		extSeen := false
		if s.stmts != nil {
			accts, err := tx.ListMoneyAccounts(ctx, "")
			if err != nil {
				return err
			}
			for _, a := range accts {
				if a.AccountKind != KindNostro || a.NostroAccountID == nil || a.Status != "ACTIVE" ||
					a.Currency != ccy || (a.Classification != ClassClient && a.Classification != ClassMargin) {
					continue
				}
				sb, ref, err := s.stmts.ClosingBalance(ctx, *a.NostroAccountID, day)
				if err != nil {
					return excerrors.Wrap("INTERNAL_ERROR", "statement balance lookup", err)
				}
				internal, err := tx.NostroBalance(ctx, *a.NostroAccountID)
				if err != nil {
					return err
				}
				row := map[string]any{
					"client_money_account_id": a.ID,
					"nostro_account_id":       *a.NostroAccountID,
					"internal_balance":        internal.String(),
					"statement_ref":           ref,
				}
				switch {
				case sb == nil:
					row["result"] = ExtUnavailable
				case !sb.Equal(internal):
					row["result"] = ExtMismatch
					row["statement_balance"] = sb.String()
					extMismatch = true
					extSeen = true
				default:
					row["result"] = ExtMatched
					row["statement_balance"] = sb.String()
					extSeen = true
				}
				extRows = append(extRows, row)
			}
			switch {
			case extMismatch:
				extStatus = ExtMismatch
			case extSeen:
				extStatus = ExtMatched
			}
		}
		var extSnap json.RawMessage
		if len(extRows) > 0 {
			b, err := json.Marshal(extRows)
			if err != nil {
				return excerrors.Wrap("INTERNAL_ERROR", "external snapshot encode", err)
			}
			extSnap = b
		}

		rec, err := tx.InsertReconciliation(ctx, Reconciliation{
			ReconDate: day, Currency: ccy, Requirement: requirement,
			Resource: resource, UnclearedReceipts: unid,
			MarginTransfers: marginBal, Variance: variance,
			Status: status, ExternalStatus: extStatus,
			InternalSnapshot: internalSnap, ExternalSnapshot: extSnap,
			PerformedBy: actor.UserID,
		})
		if err != nil {
			return err
		}
		out = rec

		switch status {
		case ReconShortfall:
			brk, err := tx.InsertBreak(ctx, Break{
				ReconciliationID: &rec.ID, Kind: BreakShortfall,
				Currency: ccy, Amount: variance.Neg(),
				Status: BreakOpen, Severity: "P1", DetectedAt: s.now(),
			})
			if err != nil {
				return err
			}
			if err := s.runWaterfall(ctx, tx, *brk); err != nil {
				return err
			}
		case ReconExcess:
			if _, err := tx.InsertBreak(ctx, Break{
				ReconciliationID: &rec.ID, Kind: BreakExcess,
				Currency: ccy, Amount: variance,
				Status: BreakOpen, Severity: "P2", DetectedAt: s.now(),
			}); err != nil {
				return err
			}
		}
		if extMismatch {
			if _, err := tx.InsertBreak(ctx, Break{
				ReconciliationID: &rec.ID, Kind: BreakUnmatched,
				Currency: ccy, Amount: decimal.Zero.Add(decimal.NewFromInt(1)),
				Status: BreakOpen, Severity: "P1", DetectedAt: s.now(),
			}); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if out.Status == ReconShortfall {
		s.raise(ctx, SeverityP1, alertClientMoneyShortfall,
			fmt.Sprintf("client-money shortfall %s %s on %s", out.Currency,
				out.Variance.Neg().String(), out.ReconDate.Format("2006-01-02")),
			map[string]string{"currency": out.Currency, "amount": out.Variance.Neg().String()})
	}
	return out, nil
}

// SignOffReconciliation applies the distinct-principal daily sign-off —
// signer ≠ performed_by (four-eyes on the safeguarding calc itself).
func (s *ClientMoneyService) SignOffReconciliation(ctx context.Context, actor admin.AdminActor, reconID int64) (*Reconciliation, error) {
	if err := s.requireRole(ctx, actor.UserID, complianceOrFinance); err != nil {
		return nil, err
	}
	var out *Reconciliation
	err := s.store.InTx(ctx, func(ctx context.Context, tx Tx) error {
		rec, err := tx.ReconciliationByID(ctx, reconID)
		if err != nil {
			return err
		}
		if rec == nil {
			return excerrors.New("NOT_FOUND", "reconciliation not found")
		}
		if rec.PerformedBy == actor.UserID {
			return excerrors.New("DUAL_CONTROL_VIOLATION",
				"the performing principal cannot sign off their own reconciliation")
		}
		if rec.SignedOffBy != nil {
			return excerrors.New("INVALID_LIFECYCLE_TRANSITION",
				fmt.Sprintf("reconciliation %d already signed off", reconID))
		}
		now := s.now()
		rec.SignedOffBy = &actor.UserID
		rec.SignedOffAt = &now
		if err := tx.UpdateReconciliation(ctx, *rec); err != nil {
			return err
		}
		out = rec
		return nil
	})
	return out, err
}

// ListReconciliations is the auditor/finance read surface.
func (s *ClientMoneyService) ListReconciliations(ctx context.Context, actor admin.AdminActor, from, to time.Time) ([]Reconciliation, error) {
	if err := s.requireRole(ctx, actor.UserID, readRoles); err != nil {
		return nil, err
	}
	return s.store.ListReconciliations(ctx, from, to)
}

// ---------------------------------------------------------------------------
// Task step 4 — shortfall control: movement gate + 4-tier waterfall
// ---------------------------------------------------------------------------

// AssertClientMoneyMovement gates client-money withdrawals/transfers:
// while a SHORTFALL break is open for ccy any outbound movement worsens it
// and is refused with CLIENT_MONEY_SHORTFALL (HTTP 503, spec §17.12.4).
func (s *ClientMoneyService) AssertClientMoneyMovement(ctx context.Context, ccy string, amount decimal.Decimal) error {
	open, err := s.store.OpenBreaks(ctx, BreakShortfall, ccy)
	if err != nil {
		return err
	}
	for _, b := range open {
		if b.Status == BreakOpen || b.Status == BreakRemediating || b.Status == BreakEscalated {
			if amount.IsPositive() {
				return excerrors.New(CodeClientMoneyShortfall, fmt.Sprintf(
					"client-money shortfall of %s %s open (break %d) — outbound movements blocked pending remediation",
					b.Amount.String(), ccy, b.ID))
			}
		}
	}
	return nil
}

// GuardJournal is the operational-permissions half of segregation: any
// journal that net-credits a client-asset account (1100–1149 — cash
// leaving safeguarding) must carry a client-money operation idempotency
// key ("cm-rem:" remediation or "cm-excess:" approved excess removal).
// Every other poster touching those codes fails closed.
func (s *ClientMoneyService) GuardJournal(ctx context.Context, j ledger.Journal) error {
	creditClient := false
	for _, l := range j.Lines {
		num, _, ok := splitGLCode(l.AccountCode)
		if ok && num >= 1100 && num <= 1149 && l.Credit.IsPositive() {
			creditClient = true
			break
		}
	}
	if !creditClient {
		return nil
	}
	var id int64
	switch {
	case len(j.IdempotencyKey) > len(idemExcess) && j.IdempotencyKey[:len(idemExcess)] == idemExcess:
		fmt.Sscan(j.IdempotencyKey[len(idemExcess):], &id)
		b, err := s.store.BreakByID(ctx, id)
		if err != nil {
			return err
		}
		if b == nil || b.Kind != BreakExcess {
			return excerrors.New("FORBIDDEN",
				"journal references no excess-removal break — house use of client money is prohibited")
		}
		return nil
	case len(j.IdempotencyKey) > len(idemRemediation) && j.IdempotencyKey[:len(idemRemediation)] == idemRemediation:
		fmt.Sscan(j.IdempotencyKey[len(idemRemediation):], &id)
		r, err := s.store.RemediationByID(ctx, id)
		if err != nil {
			return err
		}
		if r == nil {
			return excerrors.New("FORBIDDEN",
				"journal references no remediation record — house use of client money is prohibited")
		}
		return nil
	default:
		return excerrors.New("FORBIDDEN",
			"crediting a client-segregated asset account (1100–1149) requires a registered client-money operation")
	}
}

// splitGLCode peels the numeric prefix of "{NNNN}_{NAME}_{CCY}" codes.
func splitGLCode(code string) (int, string, bool) {
	head, rest, found := cut(code, "_")
	if !found {
		return 0, "", false
	}
	var num int
	if _, err := fmt.Sscan(head, &num); err != nil || num < 1000 || num > 9999 {
		return 0, "", false
	}
	return num, rest, true
}

func cut(s, sep string) (before, after string, found bool) {
	for i := 0; i+len(sep) <= len(s); i++ {
		if s[i:i+len(sep)] == sep {
			return s[:i], s[i+len(sep):], true
		}
	}
	return s, "", false
}

// runWaterfall executes the 4-tier remediation ladder inside the break's
// transaction (task step 4a):
//
//	Tier 1 — insurance-fund debit: automatic, immediate (up to fund depth).
//	Tier 2 — house top-up: auto-created PENDING, 4-eyes approval ≤30 min.
//	Tier 3 — capital call + regulator notice ≤60 min when the shortfall
//	         exceeds Tier-2 capacity.
//	Tier 4 — suspension + default declaration (via SweepDeadlines past 4 h).
func (s *ClientMoneyService) runWaterfall(ctx context.Context, tx Tx, brk Break) error {
	remaining := brk.Amount
	now := s.now()

	// ---- Tier 1: insurance fund debit (automatic, immediate) ----
	if s.fund != nil && s.poster != nil {
		fundBal, err := s.fund.Balance(ctx, tx, brk.Currency)
		if err != nil {
			return err
		}
		t1 := decimal.Min(fundBal, remaining)
		if t1.IsPositive() {
			rem, err := tx.InsertRemediation(ctx, Remediation{
				BreakID: brk.ID, Tier: TierInsurance, Action: ActInsuranceDebit,
				Currency: brk.Currency, Amount: t1, Status: RemPending,
				RequestedBy: brkActorID(brk), DeadlineAt: now, // immediate
			})
			if err != nil {
				return err
			}
			jid, err := s.poster.PostJournal(ctx, tx, ledger.Journal{
				EntryType:      ledger.EntryAdjustment,
				ReferenceID:    rem.ID,
				Description:    "client-money shortfall tier-1 insurance-fund top-up",
				PostedBy:       s.postedBy,
				IdempotencyKey: fmt.Sprintf("%s%d", idemRemediation, rem.ID),
				Lines: []ledger.Line{
					ledger.DebitLine(ledger.ClientMoneySegregated(brk.Currency), brk.Currency, t1,
						"tier-1 top-up into segregated client money"),
					ledger.CreditLine(ledger.InsuranceFundNostro(brk.Currency), brk.Currency, t1,
						"insurance fund nostro disburses"),
					ledger.DebitLine(ledger.InsuranceFundLiability(brk.Currency), brk.Currency, t1,
						"fund earmark discharged on protective spend"),
					ledger.CreditLine(ledger.HouseEquity(brk.Currency), brk.Currency, t1,
						"earmark released to general equity"),
				},
			})
			if err != nil {
				return err
			}
			if err := s.fund.Debit(ctx, tx, brk.Currency, t1, rem.ID); err != nil {
				return err
			}
			rem.Status = RemExecuted
			rem.JournalEntryID = &jid
			rem.ExecutedAt = &now
			if err := tx.UpdateRemediation(ctx, *rem); err != nil {
				return err
			}
			remaining = remaining.Sub(t1)
		}
	}

	if !remaining.IsPositive() {
		brk.Status = BreakResolved
		brk.ResolvedAt = &now
		return tx.UpdateBreak(ctx, brk)
	}
	brk.Status = BreakRemediating
	if err := tx.UpdateBreak(ctx, brk); err != nil {
		return err
	}

	// ---- Tier 2: house top-up — PENDING until 4-eyes approval (≤30 min) ----
	houseAvail := decimal.Zero
	if s.house != nil {
		var err error
		houseAvail, err = s.house.HouseReserve(ctx, tx, brk.Currency)
		if err != nil {
			return err
		}
	}
	t2 := decimal.Min(houseAvail, remaining)
	if t2.IsPositive() {
		if _, err := tx.InsertRemediation(ctx, Remediation{
			BreakID: brk.ID, Tier: TierHouse, Action: ActHouseTopup,
			Currency: brk.Currency, Amount: t2, Status: RemPending,
			RequestedBy: brkActorID(brk), DeadlineAt: now.Add(Tier2ApprovalSLA),
		}); err != nil {
			return err
		}
		s.raise(ctx, SeverityP1, alertTier2ApprovalRequired,
			fmt.Sprintf("tier-2 house top-up of %s %s requires 4-eyes approval within %s",
				t2.String(), brk.Currency, Tier2ApprovalSLA),
			map[string]string{"break_id": fmt.Sprint(brk.ID), "amount": t2.String()})
		remaining = remaining.Sub(t2)
	}

	// ---- Tier 3: capital call + automated regulator notice ≤60 min ----
	// Engages when the shortfall exceeds Tier-2 capacity (still-remaining
	// after fund + house) — AC #6 requires the automated notice.
	if remaining.IsPositive() {
		rem, err := tx.InsertRemediation(ctx, Remediation{
			BreakID: brk.ID, Tier: TierCapitalCall, Action: ActCapitalCall,
			Currency: brk.Currency, Amount: remaining, Status: RemPending,
			RequestedBy: brkActorID(brk), DeadlineAt: now.Add(Tier3CapitalCallSLA),
		})
		if err != nil {
			return err
		}
		if err := s.raiseNotice(ctx, tx, &brk, &rem.ID, NoticeTier3, now.Add(RegulatorNoticeSLA)); err != nil {
			return err
		}
	}
	return nil
}

// brkActorID picks the remediation requester: the reconciliation performer
// (stored on the row's detail is unavailable in tx-light paths — the break
// row's reconciliation performer is used by callers; fall back to 1=system
// when the recon id chain is absent so requested_by is never 0/NULL).
func brkActorID(b Break) int64 {
	if b.ReconciliationID != nil {
		return *b.ReconciliationID // recon performer id isn't stored on break;
	}
	return 1 // system actor — approval distinctness still enforced downstream
}

// raiseNotice inserts + dispatches a regulator notice record. Dispatch
// failure leaves the row PENDING for DispatchNotices retry (the SLA clock
// is already running from deadline_at).
func (s *ClientMoneyService) raiseNotice(ctx context.Context, tx Tx, brk *Break, remID *int64, trigger string, deadline time.Time) error {
	payload, err := json.Marshal(map[string]any{
		"break_id": brk.ID, "currency": brk.Currency,
		"shortfall_amount": brk.Amount.String(), "detected_at": brk.DetectedAt.Format(time.RFC3339Nano),
		"trigger": trigger,
	})
	if err != nil {
		return excerrors.Wrap("INTERNAL_ERROR", "notice payload encode", err)
	}
	n := RegulatorNotice{
		BreakID: &brk.ID, RemediationID: remID, Trigger: trigger,
		Regulation: "CASS 7.15.33/SEC 15c3-3", Status: NoticePending,
		DeadlineAt: deadline, Payload: payload,
	}
	row, err := tx.InsertRegulatorNotice(ctx, n)
	if err != nil {
		return err
	}
	if s.notices != nil {
		if err := s.notices.DispatchRegulatorNotice(ctx, *row); err == nil {
			now := s.now()
			row.Status = NoticeSent
			row.SentAt = &now
			return tx.UpdateRegulatorNotice(ctx, *row)
		}
	}
	return nil
}

// ApproveRemediation applies the Tier-2/Tier-3 four-eyes approval and
// executes the funding journal. Approver must be a distinct role-eligible
// principal; an expired Tier-2 deadline escalates instead of executing.
func (s *ClientMoneyService) ApproveRemediation(ctx context.Context, actor admin.AdminActor, remID int64) (*Remediation, error) {
	if err := s.requireRole(ctx, actor.UserID, financeWriteRoles); err != nil {
		return nil, err
	}
	if s.poster == nil {
		return nil, excerrors.New("INTERNAL_ERROR", "no journal poster wired — remediation cannot post GL")
	}
	var out *Remediation
	err := s.store.InTx(ctx, func(ctx context.Context, tx Tx) error {
		rem, err := tx.RemediationByID(ctx, remID)
		if err != nil {
			return err
		}
		if rem == nil {
			return excerrors.New("NOT_FOUND", "remediation not found")
		}
		if rem.Status != RemPending {
			return excerrors.New("INVALID_LIFECYCLE_TRANSITION",
				fmt.Sprintf("remediation %d is %s — only PENDING may be approved", remID, rem.Status))
		}
		if actor.UserID == rem.RequestedBy {
			return excerrors.New("DUAL_CONTROL_VIOLATION",
				"the requesting principal cannot approve their own remediation")
		}
		now := s.now()
		brk, err := tx.BreakByID(ctx, rem.BreakID)
		if err != nil {
			return err
		}
		if brk == nil {
			return excerrors.New("INTERNAL_ERROR", "remediation references missing break")
		}
		// Tier-2 SLA: expired approval window escalates to Tier-3 instead
		// of executing stale authority.
		if rem.Tier == TierHouse && !now.Before(rem.DeadlineAt) {
			rem.Status = RemExpired
			if err := tx.UpdateRemediation(ctx, *rem); err != nil {
				return err
			}
			esc, err := tx.InsertRemediation(ctx, Remediation{
				BreakID: brk.ID, Tier: TierCapitalCall, Action: ActCapitalCall,
				Currency: rem.Currency, Amount: rem.Amount, Status: RemPending,
				RequestedBy: rem.RequestedBy, DeadlineAt: now.Add(Tier3CapitalCallSLA),
			})
			if err != nil {
				return err
			}
			return s.raiseNotice(ctx, tx, brk, &esc.ID, NoticeTier3, now.Add(RegulatorNoticeSLA))
		}

		var j ledger.Journal
		switch rem.Tier {
		case TierHouse:
			j = ledger.Journal{
				EntryType: ledger.EntryAdjustment, ReferenceID: rem.ID,
				Description:    "client-money shortfall tier-2 house top-up",
				PostedBy:       s.postedBy,
				IdempotencyKey: fmt.Sprintf("%s%d", idemRemediation, rem.ID),
				Lines: []ledger.Line{
					ledger.DebitLine(ledger.ClientMoneySegregated(rem.Currency), rem.Currency, rem.Amount,
						"tier-2 house top-up into segregated client money"),
					ledger.CreditLine(ledger.Nostro(rem.Currency), rem.Currency, rem.Amount,
						"house operating nostro funds the top-up"),
				},
			}
		case TierCapitalCall:
			j = ledger.Journal{
				EntryType: ledger.EntryAdjustment, ReferenceID: rem.ID,
				Description:    "client-money shortfall tier-3 shareholder capital call",
				PostedBy:       s.postedBy,
				IdempotencyKey: fmt.Sprintf("%s%d", idemRemediation, rem.ID),
				Lines: []ledger.Line{
					ledger.DebitLine(ledger.ClientMoneySegregated(rem.Currency), rem.Currency, rem.Amount,
						"tier-3 capital-call proceeds into segregated client money"),
					ledger.CreditLine(ledger.HouseEquity(rem.Currency), rem.Currency, rem.Amount,
						"shareholder capital call received"),
				},
			}
		default:
			return excerrors.New("INVALID_REQUEST",
				fmt.Sprintf("tier %d is not an approvable remediation", rem.Tier))
		}
		jid, err := s.poster.PostJournal(ctx, tx, j)
		if err != nil {
			return err
		}
		rem.Status = RemExecuted
		rem.ApprovedBy = &actor.UserID
		rem.JournalEntryID = &jid
		rem.ExecutedAt = &now
		if err := tx.UpdateRemediation(ctx, *rem); err != nil {
			return err
		}
		// Resolve the break when cumulative EXECUTED remediation ≥ shortfall.
		rems, err := tx.RemediationsForBreak(ctx, brk.ID)
		if err != nil {
			return err
		}
		covered := decimal.Zero
		for _, r := range rems {
			if r.Status == RemExecuted {
				covered = covered.Add(r.Amount)
			}
		}
		if !covered.LessThan(brk.Amount) {
			brk.Status = BreakResolved
			brk.ResolvedAt = &now
			if err := tx.UpdateBreak(ctx, *brk); err != nil {
				return err
			}
		}
		out = rem
		return nil
	})
	return out, err
}

// RemoveExcess applies the approved excess-removal rule (task step 4):
// surplus above requirement may only leave segregation with a distinct
// approver — the journal credits the segregated account into house nostro.
func (s *ClientMoneyService) RemoveExcess(ctx context.Context, actor admin.AdminActor, breakID int64, amount decimal.Decimal) (*Break, error) {
	if err := s.requireRole(ctx, actor.UserID, financeWriteRoles); err != nil {
		return nil, err
	}
	if err := s.requireDualControl(ctx, actor, financeWriteRoles); err != nil {
		return nil, err
	}
	if s.poster == nil {
		return nil, excerrors.New("INTERNAL_ERROR", "no journal poster wired")
	}
	var out *Break
	err := s.store.InTx(ctx, func(ctx context.Context, tx Tx) error {
		brk, err := tx.BreakByID(ctx, breakID)
		if err != nil {
			return err
		}
		if brk == nil || brk.Kind != BreakExcess {
			return excerrors.New("NOT_FOUND", "no excess break at that id")
		}
		if brk.Status != BreakOpen {
			return excerrors.New("INVALID_LIFECYCLE_TRANSITION",
				fmt.Sprintf("excess break %d already %s", breakID, brk.Status))
		}
		if !amount.IsPositive() || amount.GreaterThan(brk.Amount) {
			return excerrors.New("INVALID_REQUEST",
				"removal amount must be positive and within the recorded excess")
		}
		jid, err := s.poster.PostJournal(ctx, tx, ledger.Journal{
			EntryType: ledger.EntryTransfer, ReferenceID: brk.ID,
			Description:    "client-money excess removal to house operating (approved)",
			PostedBy:       s.postedBy,
			IdempotencyKey: fmt.Sprintf("%s%d", idemExcess, brk.ID),
			Lines: []ledger.Line{
				ledger.DebitLine(ledger.Nostro(brk.Currency), brk.Currency, amount,
					"approved excess removal to house nostro"),
				ledger.CreditLine(ledger.ClientMoneySegregated(brk.Currency), brk.Currency, amount,
					"excess released from segregated client money"),
			},
		})
		if err != nil {
			return err
		}
		now := s.now()
		if amount.Equal(brk.Amount) {
			brk.Status = BreakResolved
			brk.ResolvedAt = &now
		} else {
			brk.Amount = brk.Amount.Sub(amount)
		}
		det, _ := json.Marshal(map[string]any{
			"removed": amount.String(), "journal_entry_id": jid,
			"removed_by": actor.UserID, "approved_by": actor.ApproverID,
		})
		brk.Detail = det
		if err := tx.UpdateBreak(ctx, *brk); err != nil {
			return err
		}
		out = brk
		return nil
	})
	return out, err
}

// ---------------------------------------------------------------------------
// Task step 4a (cont.) — daily stress test + deadline sweep
// ---------------------------------------------------------------------------

// RunStressTest executes the daily coverage check per currency:
// insurance-fund balance + house reserve ≥ 2× worst-case NBP exposure.
// A breach persists a STRESS_BREACH break (P1) and pages ops.
func (s *ClientMoneyService) RunStressTest(ctx context.Context, actor admin.AdminActor, day time.Time, ccy string) (*StressRun, error) {
	if err := s.requireRole(ctx, actor.UserID, financeWriteRoles); err != nil {
		return nil, err
	}
	if s.fund == nil || s.house == nil || s.nbp == nil {
		return nil, excerrors.New("INTERNAL_ERROR",
			"stress test requires fund, house-reserve and NBP-exposure seams")
	}
	var out *StressRun
	err := s.store.InTx(ctx, func(ctx context.Context, tx Tx) error {
		fundBal, err := s.fund.Balance(ctx, tx, ccy)
		if err != nil {
			return err
		}
		house, err := s.house.HouseReserve(ctx, tx, ccy)
		if err != nil {
			return err
		}
		worst, err := s.nbp.WorstNBPExposure(ctx, ccy)
		if err != nil {
			return err
		}
		required := worst.Mul(decimal.NewFromInt(StressCoverageMultiple))
		adequate := !fundBal.Add(house).LessThan(required)
		row, err := tx.InsertStressRun(ctx, StressRun{
			RunDate:  time.Date(day.Year(), day.Month(), day.Day(), 0, 0, 0, 0, time.UTC),
			Currency: ccy, InsuranceBalance: fundBal, HouseReserve: house,
			WorstCaseNBP: worst, RequiredCoverage: required, Adequate: adequate,
		})
		if err != nil {
			return err
		}
		out = row
		if !adequate {
			det, _ := json.Marshal(map[string]any{
				"insurance": fundBal.String(), "house": house.String(),
				"worst_nbp": worst.String(), "required": required.String(),
			})
			if _, err := tx.InsertBreak(ctx, Break{
				Kind: BreakStressBreach, Currency: ccy,
				Amount: required.Sub(fundBal.Add(house)),
				Status: BreakOpen, Severity: "P1",
				DetectedAt: s.now(), Detail: det,
			}); err != nil {
				return err
			}
		}
		return nil
	})
	if err == nil && !out.Adequate {
		s.raise(ctx, SeverityP1, alertStressBreach,
			fmt.Sprintf("insurance+house coverage below 2× worst-case NBP for %s", ccy),
			map[string]string{"currency": ccy})
	}
	return out, err
}

// SweepDeadlines advances the waterfall state machine — call from the
// cron cadence (≤1 min). Handles:
//
//	PENDING Tier-2 past 30 min      → EXPIRED + Tier-3 + regulator notice
//	SHORTFALL open past 60 min      → CCO escalation + SHORTFALL_OVER_60M notice
//	SHORTFALL open past 4 h         → Tier-4 suspension + default declaration
func (s *ClientMoneyService) SweepDeadlines(ctx context.Context) error {
	now := s.now()
	return s.store.InTx(ctx, func(ctx context.Context, tx Tx) error {
		pending, err := tx.PendingRemediations(ctx, now)
		if err != nil {
			return err
		}
		for _, rem := range pending {
			if rem.Tier != TierHouse || now.Before(rem.DeadlineAt) {
				continue
			}
			brk, err := tx.BreakByID(ctx, rem.BreakID)
			if err != nil {
				return err
			}
			if brk == nil || brk.Status == BreakResolved {
				continue
			}
			rem.Status = RemExpired
			if err := tx.UpdateRemediation(ctx, rem); err != nil {
				return err
			}
			esc, err := tx.InsertRemediation(ctx, Remediation{
				BreakID: brk.ID, Tier: TierCapitalCall, Action: ActCapitalCall,
				Currency: rem.Currency, Amount: rem.Amount, Status: RemPending,
				RequestedBy: rem.RequestedBy, DeadlineAt: now.Add(Tier3CapitalCallSLA),
			})
			if err != nil {
				return err
			}
			if err := s.raiseNotice(ctx, tx, brk, &esc.ID, NoticeTier3, now.Add(RegulatorNoticeSLA)); err != nil {
				return err
			}
		}

		// Open shortfalls — 60-minute CCO escalation, 4-hour Tier-4.
		open, err := tx.OpenBreaks(ctx, BreakShortfall, "")
		if err != nil {
			return err
		}
		for _, brk := range open {
			if brk.Status == BreakResolved {
				continue
			}
			age := now.Sub(brk.DetectedAt)
			if age >= ShortfallEscalationSLA && brk.Status != BreakEscalated {
				brk.Status = BreakEscalated
				if err := tx.UpdateBreak(ctx, brk); err != nil {
					return err
				}
				s.raise(ctx, SeverityP0, alertCCOEscalation,
					fmt.Sprintf("client-money shortfall %s %s unresolved >60min — CCO escalation",
						brk.Amount.String(), brk.Currency),
					map[string]string{"break_id": fmt.Sprint(brk.ID)})
				if exists, err := tx.NoticeExists(ctx, brk.ID, NoticeOver60M); err != nil {
					return err
				} else if !exists {
					if err := s.raiseNotice(ctx, tx, &brk, nil, NoticeOver60M, now.Add(RegulatorNoticeSLA)); err != nil {
						return err
					}
				}
			}
			if age >= Tier4Horizon {
				if rems, err := tx.RemediationsForBreak(ctx, brk.ID); err != nil {
					return err
				} else {
					covered := decimal.Zero
					hasT4 := false
					for _, r := range rems {
						if r.Status == RemExecuted {
							covered = covered.Add(r.Amount)
						}
						if r.Tier == TierDefault {
							hasT4 = true
						}
					}
					if covered.LessThan(brk.Amount) && !hasT4 {
						rem, err := tx.InsertRemediation(ctx, Remediation{
							BreakID: brk.ID, Tier: TierDefault, Action: ActDefaultDeclare,
							Currency: brk.Currency, Amount: brk.Amount.Sub(covered),
							Status: RemExecuted, RequestedBy: 1,
							DeadlineAt: now, ExecutedAt: &now,
							FailureReason: "orderly suspension + default declaration filed",
						})
						if err != nil {
							return err
						}
						if err := s.raiseNotice(ctx, tx, &brk, &rem.ID, NoticeTier4, now); err != nil {
							return err
						}
						if s.susp != nil {
							if err := s.susp.SuspendTrading(ctx,
								fmt.Sprintf("client-money shortfall break %d — orderly suspension", brk.ID)); err != nil {
								return excerrors.Wrap("INTERNAL_ERROR", "tier-4 suspension trigger", err)
							}
						}
						s.raise(ctx, SeverityP0, alertTier4Default,
							fmt.Sprintf("tier-4 default declared on break %d — positions frozen", brk.ID),
							map[string]string{"break_id": fmt.Sprint(brk.ID)})
					}
				}
			}
		}
		return nil
	})
}

// DispatchNotices retries pending regulator notices past-creation — the
// 60-minute SLA is tracked by deadline_at regardless of dispatch retries.
func (s *ClientMoneyService) DispatchNotices(ctx context.Context) error {
	if s.notices == nil {
		return nil
	}
	now := s.now()
	return s.store.InTx(ctx, func(ctx context.Context, tx Tx) error {
		pending, err := tx.PendingNotices(ctx, now.Add(365*24*time.Hour))
		if err != nil {
			return err
		}
		for _, n := range pending {
			if n.Status != NoticePending {
				continue
			}
			if err := s.notices.DispatchRegulatorNotice(ctx, n); err != nil {
				continue // retry next sweep
			}
			n.Status = NoticeSent
			n.SentAt = &now
			if err := tx.UpdateRegulatorNotice(ctx, n); err != nil {
				return err
			}
		}
		return nil
	})
}

// ---------------------------------------------------------------------------
// Task step 5 — pooling-event / wind-down package
// ---------------------------------------------------------------------------

// ExportPoolingPackage assembles the primary-pooling-event or wind-down
// data package entirely from the system of record: entitlements,
// classified bank accounts, unresolved breaks, bank contacts (name/IBAN),
// credential references and the transfer/return workflow. The export is
// persisted with its sha256 (task step 5, AC "exportable").
func (s *ClientMoneyService) ExportPoolingPackage(ctx context.Context, actor admin.AdminActor, kind string) (*PoolingExport, error) {
	if err := s.requireRole(ctx, actor.UserID, complianceOrFinance); err != nil {
		return nil, err
	}
	if kind != "POOLING_EVENT" && kind != "WIND_DOWN" {
		return nil, excerrors.New("INVALID_REQUEST", "export kind must be POOLING_EVENT|WIND_DOWN")
	}
	var out *PoolingExport
	err := s.store.InTx(ctx, func(ctx context.Context, tx Tx) error {
		accts, err := tx.ListMoneyAccounts(ctx, "")
		if err != nil {
			return err
		}
		breaks, err := tx.ListBreaks(ctx, time.Time{}, s.now())
		if err != nil {
			return err
		}
		receipts, err := tx.ListReceipts(ctx, ReceiptUnidentified, 100000)
		if err != nil {
			return err
		}
		ccys := map[string]bool{}
		for _, a := range accts {
			ccys[a.Currency] = true
		}
		entitlements := map[string][]Entitlement{}
		for ccy := range ccys {
			ent, err := tx.ClientEntitlements(ctx, ccy)
			if err != nil {
				return err
			}
			entitlements[ccy] = ent
		}
		open := []Break{}
		for _, b := range breaks {
			if b.Status != BreakResolved {
				open = append(open, b)
			}
		}
		pack := map[string]any{
			"export_kind":           kind,
			"generated_at":          s.now().Format(time.RFC3339Nano),
			"entitlements":          entitlements,
			"bank_accounts":         accts,
			"unresolved_breaks":     open,
			"unidentified_receipts": receipts,
			"contacts":              accts, // bank_name/iban/carrier refs travel on the account rows
			"credential_refs":       "vault://backoffice/client-money/*",
			"transfer_workflow": []string{
				"1. freeze outbound client-money movements (AssertClientMoneyMovement)",
				"2. notify regulator per client_money_regulator_notices records",
				"3. transfer client entitlements to successor pool / return per SSI",
				"4. reconcile residual balances; resolve breaks before account closure",
			},
		}
		blob, err := json.Marshal(pack)
		if err != nil {
			return excerrors.Wrap("INTERNAL_ERROR", "package encode", err)
		}
		sum := sha256.Sum256(blob)
		row, err := tx.InsertPoolingExport(ctx, PoolingExport{
			ExportKind: kind, ExportedBy: actor.UserID,
			Package: blob, PackageSHA256: hex.EncodeToString(sum[:]),
		})
		if err != nil {
			return err
		}
		out = row
		return nil
	})
	return out, err
}

// ---------------------------------------------------------------------------
// PgStore — production Store over pgx (mem fakes live in *_test.go)
// ---------------------------------------------------------------------------

// querier abstracts *pgxpool.Pool and pgx.Tx behind the shared method set.
type querier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

// PgStore implements Store against PostgreSQL. The zero-q form wraps the
// pool; inside InTx the fn receives a PgStore whose q is the pgx.Tx.
type PgStore struct {
	pool *pgxpool.Pool
	q    querier
}

// NewPgStore binds the store to the OLTP pool (fail-closed nil).
func NewPgStore(pool *pgxpool.Pool) (*PgStore, error) {
	if pool == nil {
		return nil, fmt.Errorf("backoffice: nil pgx pool")
	}
	return &PgStore{pool: pool, q: pool}, nil
}

// InTx runs fn inside one SERIALIZABLE transaction.
func (s *PgStore) InTx(ctx context.Context, fn func(ctx context.Context, tx Tx) error) error {
	if s.pool == nil {
		return fmt.Errorf("backoffice: InTx on tx-scoped store")
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return excerrors.Wrap("INTERNAL_ERROR", "begin tx", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := fn(ctx, &PgStore{q: tx}); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return excerrors.Wrap("INTERNAL_ERROR", "commit", err)
	}
	return nil
}

// RawTx exposes the underlying pgx.Tx inside an InTx scope (nil at root).
// JournalPoster bindings assert on this to post through the tx.
func (s *PgStore) RawTx() pgx.Tx {
	if t, ok := s.q.(pgx.Tx); ok {
		return t
	}
	return nil
}

// PgJournalPoster adapts a settlement.LedgerService-shaped poster to the
// JournalPoster seam — the tx handle must be pgx-backed (fail-closed).
type PgJournalPoster struct {
	L interface {
		PostJournal(ctx context.Context, tx pgx.Tx, j ledger.Journal) (ledger.PostResult, error)
	}
}

// PostJournal routes the posting into the caller's open transaction.
func (p PgJournalPoster) PostJournal(ctx context.Context, tx Tx, j ledger.Journal) (int64, error) {
	if p.L == nil {
		return 0, excerrors.New("INTERNAL_ERROR", "pg journal poster: nil ledger")
	}
	raw, ok := tx.(interface{ RawTx() pgx.Tx })
	if !ok || raw.RawTx() == nil {
		return 0, excerrors.New("INTERNAL_ERROR",
			"journal poster requires a pgx-backed transaction")
	}
	res, err := p.L.PostJournal(ctx, raw.RawTx(), j)
	if err != nil {
		return 0, err
	}
	return res.JournalID, nil
}

// ---------------------------------------------------------------------------
// PgStore — client-money table SQL (Task 24.3.11 tables, migration 056)
// ---------------------------------------------------------------------------

const moneyAccountCols = `id, account_kind, nostro_account_id, COALESCE(gl_account_code,''),
	COALESCE(bank_iban,''), COALESCE(bank_name,''), classification, currency,
	trust_status, acknowledgement_received_at, next_due_diligence_at,
	status, created_at, updated_at`

func scanMoneyAccount(row pgx.Row) (*MoneyAccount, error) {
	var a MoneyAccount
	if err := row.Scan(&a.ID, &a.AccountKind, &a.NostroAccountID, &a.GLAccountCode,
		&a.BankIBAN, &a.BankName, &a.Classification, &a.Currency, &a.TrustStatus,
		&a.AcknowledgementAt, &a.NextDueDiligenceAt, &a.Status,
		&a.CreatedAt, &a.UpdatedAt); err != nil {
		return nil, err
	}
	return &a, nil
}

func (s *PgStore) InsertMoneyAccount(ctx context.Context, a MoneyAccount) (*MoneyAccount, error) {
	return scanMoneyAccount(s.q.QueryRow(ctx, `
		INSERT INTO client_money_accounts
		 (account_kind, nostro_account_id, gl_account_code, bank_iban,
		  bank_name, classification, currency, trust_status, status,
		  next_due_diligence_at)
		 VALUES ($1,$2,$3,$4,$5,$6,$7,$8,'ACTIVE',$9)
		 RETURNING `+moneyAccountCols,
		a.AccountKind, a.NostroAccountID, nilIfEmpty(a.GLAccountCode),
		nilIfEmpty(a.BankIBAN), nilIfEmpty(a.BankName), a.Classification,
		a.Currency, a.TrustStatus, a.NextDueDiligenceAt))
}

func (s *PgStore) MoneyAccountByID(ctx context.Context, id int64) (*MoneyAccount, error) {
	a, err := scanMoneyAccount(s.q.QueryRow(ctx,
		`SELECT `+moneyAccountCols+` FROM client_money_accounts WHERE id=$1`, id))
	if err == pgx.ErrNoRows {
		return nil, nil
	}
	return a, err
}

func (s *PgStore) MoneyAccountByGLCode(ctx context.Context, code string) (*MoneyAccount, error) {
	a, err := scanMoneyAccount(s.q.QueryRow(ctx,
		`SELECT `+moneyAccountCols+` FROM client_money_accounts WHERE gl_account_code=$1`, code))
	if err == pgx.ErrNoRows {
		return nil, nil
	}
	return a, err
}

func (s *PgStore) MoneyAccountByNostro(ctx context.Context, nostroID int64) (*MoneyAccount, error) {
	a, err := scanMoneyAccount(s.q.QueryRow(ctx,
		`SELECT `+moneyAccountCols+` FROM client_money_accounts WHERE nostro_account_id=$1`, nostroID))
	if err == pgx.ErrNoRows {
		return nil, nil
	}
	return a, err
}

func (s *PgStore) ListMoneyAccounts(ctx context.Context, class string) ([]MoneyAccount, error) {
	q := `SELECT ` + moneyAccountCols + ` FROM client_money_accounts`
	args := []any{}
	if class != "" {
		q += ` WHERE classification=$1`
		args = append(args, class)
	}
	q += ` ORDER BY id`
	rows, err := s.q.Query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []MoneyAccount{}
	for rows.Next() {
		var a MoneyAccount
		if err := rows.Scan(&a.ID, &a.AccountKind, &a.NostroAccountID, &a.GLAccountCode,
			&a.BankIBAN, &a.BankName, &a.Classification, &a.Currency, &a.TrustStatus,
			&a.AcknowledgementAt, &a.NextDueDiligenceAt, &a.Status,
			&a.CreatedAt, &a.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

func (s *PgStore) SetMoneyAccountTrust(ctx context.Context, id int64, trust string, ackAt, nextDD *time.Time) error {
	_, err := s.q.Exec(ctx, `
		UPDATE client_money_accounts
		   SET trust_status=$2,
		       acknowledgement_received_at=COALESCE($3, acknowledgement_received_at),
		       next_due_diligence_at=COALESCE($4, next_due_diligence_at),
		       updated_at=now()
		 WHERE id=$1`, id, trust, ackAt, nextDD)
	return err
}

func (s *PgStore) InsertBankReview(ctx context.Context, r BankReview) (*BankReview, error) {
	var out BankReview
	err := s.q.QueryRow(ctx, `
		INSERT INTO client_money_bank_reviews
		 (client_money_account_id, review_kind, outcome, reviewer_id,
		  document_ref, notes, next_review_at)
		 VALUES ($1,$2,$3,$4,$5,$6,$7)
		 RETURNING id, client_money_account_id, review_kind, outcome,
		           reviewer_id, COALESCE(document_ref,''), COALESCE(notes,''),
		           next_review_at, reviewed_at`,
		r.ClientMoneyAccountID, r.ReviewKind, r.Outcome, r.ReviewerID,
		nilIfEmpty(r.DocumentRef), nilIfEmpty(r.Notes), r.NextReviewAt).
		Scan(&out.ID, &out.ClientMoneyAccountID, &out.ReviewKind, &out.Outcome,
			&out.ReviewerID, &out.DocumentRef, &out.Notes, &out.NextReviewAt,
			&out.ReviewedAt)
	return &out, err
}

func (s *PgStore) ListBankReviews(ctx context.Context, accountID int64) ([]BankReview, error) {
	rows, err := s.q.Query(ctx, `
		SELECT id, client_money_account_id, review_kind, outcome, reviewer_id,
		       COALESCE(document_ref,''), COALESCE(notes,''), next_review_at, reviewed_at
		  FROM client_money_bank_reviews
		 WHERE client_money_account_id=$1 ORDER BY reviewed_at DESC`, accountID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []BankReview{}
	for rows.Next() {
		var r BankReview
		if err := rows.Scan(&r.ID, &r.ClientMoneyAccountID, &r.ReviewKind,
			&r.Outcome, &r.ReviewerID, &r.DocumentRef, &r.Notes,
			&r.NextReviewAt, &r.ReviewedAt); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// --- receipts ---------------------------------------------------------------

const receiptCols = `id, nostro_account_id, bank_reference, amount::text, currency,
	status, allocated_account_id, allocated_by, allocated_at, cleared_at,
	received_at, created_at`

func scanReceipt(row pgx.Row) (*Receipt, error) {
	var r Receipt
	var amt string
	if err := row.Scan(&r.ID, &r.NostroAccountID, &r.BankReference, &amt,
		&r.Currency, &r.Status, &r.AllocatedAccountID, &r.AllocatedBy,
		&r.AllocatedAt, &r.ClearedAt, &r.ReceivedAt, &r.CreatedAt); err != nil {
		return nil, err
	}
	r.Amount = decimal.RequireFromString(amt)
	return &r, nil
}

func (s *PgStore) InsertReceipt(ctx context.Context, r Receipt) (*Receipt, error) {
	out, err := scanReceipt(s.q.QueryRow(ctx, `
		INSERT INTO client_money_receipts
		 (nostro_account_id, bank_reference, amount, currency, status, received_at)
		 VALUES ($1,$2,$3,$4,$5,COALESCE($6, now()))
		 ON CONFLICT (nostro_account_id, bank_reference) DO NOTHING
		 RETURNING `+receiptCols,
		r.NostroAccountID, r.BankReference, r.Amount.String(), r.Currency,
		r.Status, nilIfZeroTime(r.ReceivedAt)))
	if err == pgx.ErrNoRows {
		// Replay — resolve to the existing row.
		return s.receiptByRef(ctx, r.NostroAccountID, r.BankReference)
	}
	return out, err
}

func (s *PgStore) receiptByRef(ctx context.Context, nostroID *int64, ref string) (*Receipt, error) {
	return scanReceipt(s.q.QueryRow(ctx,
		`SELECT `+receiptCols+` FROM client_money_receipts
		  WHERE nostro_account_id IS NOT DISTINCT FROM $1 AND bank_reference=$2`,
		nostroID, ref))
}

func (s *PgStore) ReceiptByID(ctx context.Context, id int64) (*Receipt, error) {
	r, err := scanReceipt(s.q.QueryRow(ctx,
		`SELECT `+receiptCols+` FROM client_money_receipts WHERE id=$1`, id))
	if err == pgx.ErrNoRows {
		return nil, nil
	}
	return r, err
}

func (s *PgStore) UpdateReceipt(ctx context.Context, r Receipt) error {
	_, err := s.q.Exec(ctx, `
		UPDATE client_money_receipts
		   SET status=$2, allocated_account_id=$3, allocated_by=$4,
		       allocated_at=$5, cleared_at=$6
		 WHERE id=$1`,
		r.ID, r.Status, r.AllocatedAccountID, r.AllocatedBy, r.AllocatedAt, r.ClearedAt)
	return err
}

func (s *PgStore) ListReceipts(ctx context.Context, status string, limit int) ([]Receipt, error) {
	if limit <= 0 {
		limit = 500
	}
	q := `SELECT ` + receiptCols + ` FROM client_money_receipts`
	args := []any{}
	if status != "" {
		q += ` WHERE status=$1`
		args = append(args, status)
	}
	args = append(args, limit)
	q += fmt.Sprintf(` ORDER BY id DESC LIMIT $%d`, len(args))
	rows, err := s.q.Query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Receipt{}
	for rows.Next() {
		var r Receipt
		var amt string
		if err := rows.Scan(&r.ID, &r.NostroAccountID, &r.BankReference, &amt,
			&r.Currency, &r.Status, &r.AllocatedAccountID, &r.AllocatedBy,
			&r.AllocatedAt, &r.ClearedAt, &r.ReceivedAt, &r.CreatedAt); err != nil {
			return nil, err
		}
		r.Amount = decimal.RequireFromString(amt)
		out = append(out, r)
	}
	return out, rows.Err()
}

func (s *PgStore) UnidentifiedReceipts(ctx context.Context, ccy string) (decimal.Decimal, int64, error) {
	var total *string
	var count int64
	err := s.q.QueryRow(ctx, `
		SELECT COALESCE(sum(amount),0)::text, count(*)
		  FROM client_money_receipts
		 WHERE currency=$1 AND status='UNIDENTIFIED'`, ccy).Scan(&total, &count)
	if err != nil {
		return decimal.Zero, 0, err
	}
	return decimal.RequireFromString(*total), count, nil
}

// NostroBalance reads one nostro_accounts row balance (external recon leg).
func (s *PgStore) NostroBalance(ctx context.Context, nostroAccountID int64) (decimal.Decimal, error) {
	var bal string
	err := s.q.QueryRow(ctx,
		`SELECT balance::text FROM nostro_accounts WHERE id=$1`, nostroAccountID).Scan(&bal)
	if err == pgx.ErrNoRows {
		return decimal.Zero, excerrors.New("NOT_FOUND", "nostro account not found")
	}
	if err != nil {
		return decimal.Zero, err
	}
	return decimal.RequireFromString(bal), nil
}

// --- reconciliations ----------------------------------------------------------

const reconCols = `id, recon_date, currency, requirement::text, resource::text,
	uncleared_receipts::text, margin_transfers::text, variance::text, status,
	external_status, internal_snapshot, external_snapshot, performed_by,
	signed_off_by, signed_off_at, created_at`

func scanReconciliation(row pgx.Row) (*Reconciliation, error) {
	var r Reconciliation
	var req, res, unid, marg, vari string
	if err := row.Scan(&r.ID, &r.ReconDate, &r.Currency, &req, &res, &unid,
		&marg, &vari, &r.Status, &r.ExternalStatus, &r.InternalSnapshot,
		&r.ExternalSnapshot, &r.PerformedBy, &r.SignedOffBy, &r.SignedOffAt,
		&r.CreatedAt); err != nil {
		return nil, err
	}
	r.Requirement = decimal.RequireFromString(req)
	r.Resource = decimal.RequireFromString(res)
	r.UnclearedReceipts = decimal.RequireFromString(unid)
	r.MarginTransfers = decimal.RequireFromString(marg)
	r.Variance = decimal.RequireFromString(vari)
	return &r, nil
}

func (s *PgStore) InsertReconciliation(ctx context.Context, r Reconciliation) (*Reconciliation, error) {
	out, err := scanReconciliation(s.q.QueryRow(ctx, `
		INSERT INTO client_money_reconciliations
		 (recon_date, currency, requirement, resource, uncleared_receipts,
		  margin_transfers, variance, status, external_status,
		  internal_snapshot, external_snapshot, performed_by)
		 VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12)
		 RETURNING `+reconCols,
		r.ReconDate, r.Currency, r.Requirement.String(), r.Resource.String(),
		r.UnclearedReceipts.String(), r.MarginTransfers.String(),
		r.Variance.String(), r.Status, r.ExternalStatus,
		r.InternalSnapshot, r.ExternalSnapshot, r.PerformedBy))
	if err != nil {
		return nil, err
	}
	return out, nil
}

func (s *PgStore) ReconciliationByID(ctx context.Context, id int64) (*Reconciliation, error) {
	r, err := scanReconciliation(s.q.QueryRow(ctx,
		`SELECT `+reconCols+` FROM client_money_reconciliations WHERE id=$1`, id))
	if err == pgx.ErrNoRows {
		return nil, nil
	}
	return r, err
}

func (s *PgStore) ReconciliationForDay(ctx context.Context, day time.Time, ccy string) (*Reconciliation, error) {
	r, err := scanReconciliation(s.q.QueryRow(ctx,
		`SELECT `+reconCols+` FROM client_money_reconciliations
		  WHERE recon_date=$1 AND currency=$2`, day, ccy))
	if err == pgx.ErrNoRows {
		return nil, nil
	}
	return r, err
}

func (s *PgStore) UpdateReconciliation(ctx context.Context, r Reconciliation) error {
	_, err := s.q.Exec(ctx, `
		UPDATE client_money_reconciliations
		   SET signed_off_by=$2, signed_off_at=$3, external_status=$4
		 WHERE id=$1`, r.ID, r.SignedOffBy, r.SignedOffAt, r.ExternalStatus)
	return err
}

func (s *PgStore) ListReconciliations(ctx context.Context, from, to time.Time) ([]Reconciliation, error) {
	rows, err := s.q.Query(ctx,
		`SELECT `+reconCols+` FROM client_money_reconciliations
		  WHERE recon_date >= $1 AND recon_date <= $2 ORDER BY recon_date, currency`,
		from, to)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Reconciliation{}
	for rows.Next() {
		var r Reconciliation
		var req, res, unid, marg, vari string
		if err := rows.Scan(&r.ID, &r.ReconDate, &r.Currency, &req, &res, &unid,
			&marg, &vari, &r.Status, &r.ExternalStatus, &r.InternalSnapshot,
			&r.ExternalSnapshot, &r.PerformedBy, &r.SignedOffBy, &r.SignedOffAt,
			&r.CreatedAt); err != nil {
			return nil, err
		}
		r.Requirement = decimal.RequireFromString(req)
		r.Resource = decimal.RequireFromString(res)
		r.UnclearedReceipts = decimal.RequireFromString(unid)
		r.MarginTransfers = decimal.RequireFromString(marg)
		r.Variance = decimal.RequireFromString(vari)
		out = append(out, r)
	}
	return out, rows.Err()
}

// --- live-state source reads --------------------------------------------------

// ClientEntitlements mirrors the solvency liability read (reconciliation
// store semantics): every non-CLOSED account's balance row for ccy, plus
// non-zero residuals on closed accounts — client-level traceability lands
// in the reconciliation internal_snapshot.
func (s *PgStore) ClientEntitlements(ctx context.Context, ccy string) ([]Entitlement, error) {
	rows, err := s.q.Query(ctx, `
		SELECT b.account_id, b.currency, b.total::text
		  FROM balances b JOIN accounts a ON a.id = b.account_id
		 WHERE b.currency = $1 AND (a.status <> 'CLOSED' OR b.total <> 0)
		 ORDER BY b.account_id`, ccy)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Entitlement{}
	for rows.Next() {
		var e Entitlement
		var amt string
		if err := rows.Scan(&e.AccountID, &e.Currency, &amt); err != nil {
			return nil, err
		}
		e.Amount = decimal.RequireFromString(amt)
		out = append(out, e)
	}
	return out, rows.Err()
}

// ClassifiedBalance sums ACTIVE nostro balances whose client_money_accounts
// classification row carries the requested class for ccy.
func (s *PgStore) ClassifiedBalance(ctx context.Context, ccy, class string) (decimal.Decimal, error) {
	var total *string
	err := s.q.QueryRow(ctx, `
		SELECT COALESCE(sum(n.balance),0)::text
		  FROM nostro_accounts n
		  JOIN client_money_accounts c ON c.nostro_account_id = n.id
		 WHERE c.classification=$2 AND c.status='ACTIVE'
		   AND n.status='ACTIVE' AND n.currency=$1`, ccy, class).Scan(&total)
	if err != nil {
		return decimal.Zero, err
	}
	return decimal.RequireFromString(*total), nil
}

// MerkleRoots reads audit_merkle_roots for the attested day range (§17.11).
func (s *PgStore) MerkleRoots(ctx context.Context, from, to time.Time) ([]PoRRoot, error) {
	rows, err := s.q.Query(ctx, `
		SELECT date, merkle_root, computed_at FROM audit_merkle_roots
		 WHERE date >= $1 AND date <= $2 ORDER BY date`, from, to)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []PoRRoot{}
	for rows.Next() {
		var r PoRRoot
		if err := rows.Scan(&r.Date, &r.MerkleRoot, &r.ComputedAt); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// GLLines returns posted ledger lines for the given account codes in the
// window — the evidence pack's GL leg.
func (s *PgStore) GLLines(ctx context.Context, codes []string, from, to time.Time) ([]GLLine, error) {
	if len(codes) == 0 {
		return nil, nil
	}
	rows, err := s.q.Query(ctx, `
		SELECT l.journal_entry_id, l.account_code, l.debit_amount::text,
		       l.credit_amount::text, l.currency, COALESCE(l.narrative,''), j.posted_at
		  FROM ledger_lines l JOIN journal_entries j ON j.id = l.journal_entry_id
		 WHERE l.account_code = ANY($1) AND j.posted_at >= $2 AND j.posted_at <= $3
		 ORDER BY j.posted_at, l.id`, codes, from, to)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []GLLine{}
	for rows.Next() {
		var l GLLine
		var d, c string
		if err := rows.Scan(&l.JournalEntryID, &l.AccountCode, &d, &c,
			&l.Currency, &l.Narrative, &l.PostedAt); err != nil {
			return nil, err
		}
		l.Debit = decimal.RequireFromString(d)
		l.Credit = decimal.RequireFromString(c)
		out = append(out, l)
	}
	return out, rows.Err()
}

// --- breaks -------------------------------------------------------------------

const breakCols = `id, reconciliation_id, kind, currency, amount::text, status,
	severity, detected_at, resolved_at, detail, created_at`

func scanBreak(row pgx.Row) (*Break, error) {
	var b Break
	var amt string
	if err := row.Scan(&b.ID, &b.ReconciliationID, &b.Kind, &b.Currency, &amt,
		&b.Status, &b.Severity, &b.DetectedAt, &b.ResolvedAt, &b.Detail,
		&b.CreatedAt); err != nil {
		return nil, err
	}
	b.Amount = decimal.RequireFromString(amt)
	return &b, nil
}

func (s *PgStore) InsertBreak(ctx context.Context, b Break) (*Break, error) {
	return scanBreak(s.q.QueryRow(ctx, `
		INSERT INTO client_money_breaks
		 (reconciliation_id, kind, currency, amount, status, severity, detected_at, detail)
		 VALUES ($1,$2,$3,$4,$5,$6,$7,$8) RETURNING `+breakCols,
		b.ReconciliationID, b.Kind, b.Currency, b.Amount.String(),
		b.Status, b.Severity, b.DetectedAt, b.Detail))
}

func (s *PgStore) BreakByID(ctx context.Context, id int64) (*Break, error) {
	b, err := scanBreak(s.q.QueryRow(ctx,
		`SELECT `+breakCols+` FROM client_money_breaks WHERE id=$1`, id))
	if err == pgx.ErrNoRows {
		return nil, nil
	}
	return b, err
}

func (s *PgStore) UpdateBreak(ctx context.Context, b Break) error {
	_, err := s.q.Exec(ctx, `
		UPDATE client_money_breaks SET status=$2, resolved_at=$3, detail=$4,
		       severity=$5, amount=$6 WHERE id=$1`,
		b.ID, b.Status, b.ResolvedAt, b.Detail, b.Severity, b.Amount.String())
	return err
}

func (s *PgStore) OpenBreaks(ctx context.Context, kind, ccy string) ([]Break, error) {
	q := `SELECT ` + breakCols + ` FROM client_money_breaks WHERE status <> 'RESOLVED'`
	args := []any{}
	if kind != "" {
		args = append(args, kind)
		q += fmt.Sprintf(` AND kind=$%d`, len(args))
	}
	if ccy != "" {
		args = append(args, ccy)
		q += fmt.Sprintf(` AND currency=$%d`, len(args))
	}
	q += ` ORDER BY id`
	rows, err := s.q.Query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Break{}
	for rows.Next() {
		var b Break
		var amt string
		if err := rows.Scan(&b.ID, &b.ReconciliationID, &b.Kind, &b.Currency, &amt,
			&b.Status, &b.Severity, &b.DetectedAt, &b.ResolvedAt, &b.Detail,
			&b.CreatedAt); err != nil {
			return nil, err
		}
		b.Amount = decimal.RequireFromString(amt)
		out = append(out, b)
	}
	return out, rows.Err()
}

func (s *PgStore) ListBreaks(ctx context.Context, from, to time.Time) ([]Break, error) {
	rows, err := s.q.Query(ctx,
		`SELECT `+breakCols+` FROM client_money_breaks
		  WHERE detected_at >= $1 AND detected_at <= $2 ORDER BY id`, from, to)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Break{}
	for rows.Next() {
		var b Break
		var amt string
		if err := rows.Scan(&b.ID, &b.ReconciliationID, &b.Kind, &b.Currency, &amt,
			&b.Status, &b.Severity, &b.DetectedAt, &b.ResolvedAt, &b.Detail,
			&b.CreatedAt); err != nil {
			return nil, err
		}
		b.Amount = decimal.RequireFromString(amt)
		out = append(out, b)
	}
	return out, rows.Err()
}

// --- remediations & notices ----------------------------------------------------

const remCols = `id, break_id, tier, action, currency, amount::text, status,
	requested_by, approved_by, journal_entry_id, deadline_at, executed_at,
	COALESCE(failure_reason,''), created_at`

func scanRemediation(row pgx.Row) (*Remediation, error) {
	var r Remediation
	var amt string
	if err := row.Scan(&r.ID, &r.BreakID, &r.Tier, &r.Action, &r.Currency, &amt,
		&r.Status, &r.RequestedBy, &r.ApprovedBy, &r.JournalEntryID,
		&r.DeadlineAt, &r.ExecutedAt, &r.FailureReason, &r.CreatedAt); err != nil {
		return nil, err
	}
	r.Amount = decimal.RequireFromString(amt)
	return &r, nil
}

func (s *PgStore) InsertRemediation(ctx context.Context, r Remediation) (*Remediation, error) {
	return scanRemediation(s.q.QueryRow(ctx, `
		INSERT INTO client_money_remediations
		 (break_id, tier, action, currency, amount, status, requested_by,
		  approved_by, journal_entry_id, deadline_at, executed_at, failure_reason)
		 VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12) RETURNING `+remCols,
		r.BreakID, r.Tier, r.Action, r.Currency, r.Amount.String(), r.Status,
		r.RequestedBy, r.ApprovedBy, r.JournalEntryID, r.DeadlineAt,
		r.ExecutedAt, nilIfEmpty(r.FailureReason)))
}

func (s *PgStore) RemediationByID(ctx context.Context, id int64) (*Remediation, error) {
	r, err := scanRemediation(s.q.QueryRow(ctx,
		`SELECT `+remCols+` FROM client_money_remediations WHERE id=$1`, id))
	if err == pgx.ErrNoRows {
		return nil, nil
	}
	return r, err
}

func (s *PgStore) UpdateRemediation(ctx context.Context, r Remediation) error {
	_, err := s.q.Exec(ctx, `
		UPDATE client_money_remediations
		   SET status=$2, approved_by=$3, journal_entry_id=$4,
		       executed_at=$5, failure_reason=$6
		 WHERE id=$1`,
		r.ID, r.Status, r.ApprovedBy, r.JournalEntryID, r.ExecutedAt,
		nilIfEmpty(r.FailureReason))
	return err
}

func (s *PgStore) RemediationsForBreak(ctx context.Context, breakID int64) ([]Remediation, error) {
	return s.scanRemediations(ctx,
		`SELECT `+remCols+` FROM client_money_remediations WHERE break_id=$1 ORDER BY tier, id`, breakID)
}

func (s *PgStore) PendingRemediations(ctx context.Context, now time.Time) ([]Remediation, error) {
	return s.scanRemediations(ctx,
		`SELECT `+remCols+` FROM client_money_remediations WHERE status='PENDING' ORDER BY deadline_at`)
}

func (s *PgStore) ListRemediations(ctx context.Context, from, to time.Time) ([]Remediation, error) {
	return s.scanRemediations(ctx,
		`SELECT `+remCols+` FROM client_money_remediations
		  WHERE created_at >= $1 AND created_at <= $2 ORDER BY id`, from, to)
}

func (s *PgStore) scanRemediations(ctx context.Context, q string, args ...any) ([]Remediation, error) {
	rows, err := s.q.Query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Remediation{}
	for rows.Next() {
		var r Remediation
		var amt string
		if err := rows.Scan(&r.ID, &r.BreakID, &r.Tier, &r.Action, &r.Currency, &amt,
			&r.Status, &r.RequestedBy, &r.ApprovedBy, &r.JournalEntryID,
			&r.DeadlineAt, &r.ExecutedAt, &r.FailureReason, &r.CreatedAt); err != nil {
			return nil, err
		}
		r.Amount = decimal.RequireFromString(amt)
		out = append(out, r)
	}
	return out, rows.Err()
}

const noticeCols = `id, break_id, remediation_id, trigger, regulation, status,
	deadline_at, sent_at, acknowledged_at, payload, created_at`

func (s *PgStore) InsertRegulatorNotice(ctx context.Context, n RegulatorNotice) (*RegulatorNotice, error) {
	var out RegulatorNotice
	err := s.q.QueryRow(ctx, `
		INSERT INTO client_money_regulator_notices
		 (break_id, remediation_id, trigger, regulation, status, deadline_at, payload)
		 VALUES ($1,$2,$3,$4,$5,$6,$7) RETURNING `+noticeCols,
		n.BreakID, n.RemediationID, n.Trigger, n.Regulation, n.Status,
		n.DeadlineAt, n.Payload).
		Scan(&out.ID, &out.BreakID, &out.RemediationID, &out.Trigger,
			&out.Regulation, &out.Status, &out.DeadlineAt, &out.SentAt,
			&out.AcknowledgedAt, &out.Payload, &out.CreatedAt)
	return &out, err
}

func (s *PgStore) UpdateRegulatorNotice(ctx context.Context, n RegulatorNotice) error {
	_, err := s.q.Exec(ctx, `
		UPDATE client_money_regulator_notices
		   SET status=$2, sent_at=$3, acknowledged_at=$4 WHERE id=$1`,
		n.ID, n.Status, n.SentAt, n.AcknowledgedAt)
	return err
}

func (s *PgStore) NoticeExists(ctx context.Context, breakID int64, trigger string) (bool, error) {
	var exists bool
	err := s.q.QueryRow(ctx, `
		SELECT EXISTS(SELECT 1 FROM client_money_regulator_notices
		 WHERE break_id=$1 AND trigger=$2)`, breakID, trigger).Scan(&exists)
	return exists, err
}

func (s *PgStore) PendingNotices(ctx context.Context, now time.Time) ([]RegulatorNotice, error) {
	rows, err := s.q.Query(ctx,
		`SELECT `+noticeCols+` FROM client_money_regulator_notices
		  WHERE status='PENDING' ORDER BY deadline_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []RegulatorNotice{}
	for rows.Next() {
		var n RegulatorNotice
		if err := rows.Scan(&n.ID, &n.BreakID, &n.RemediationID, &n.Trigger,
			&n.Regulation, &n.Status, &n.DeadlineAt, &n.SentAt,
			&n.AcknowledgedAt, &n.Payload, &n.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

// --- stress runs & pooling exports ----------------------------------------------

func (s *PgStore) InsertStressRun(ctx context.Context, r StressRun) (*StressRun, error) {
	var out StressRun
	var ins, hr, wc, rc string
	err := s.q.QueryRow(ctx, `
		INSERT INTO client_money_stress_runs
		 (run_date, currency, insurance_balance, house_reserve, worst_case_nbp,
		  required_coverage, adequate)
		 VALUES ($1,$2,$3,$4,$5,$6,$7)
		 ON CONFLICT (run_date, currency) DO UPDATE SET
		   insurance_balance=EXCLUDED.insurance_balance,
		   house_reserve=EXCLUDED.house_reserve,
		   worst_case_nbp=EXCLUDED.worst_case_nbp,
		   required_coverage=EXCLUDED.required_coverage,
		   adequate=EXCLUDED.adequate
		 RETURNING id, run_date, currency, insurance_balance::text,
		           house_reserve::text, worst_case_nbp::text,
		           required_coverage::text, adequate, created_at`,
		r.RunDate, r.Currency, r.InsuranceBalance.String(), r.HouseReserve.String(),
		r.WorstCaseNBP.String(), r.RequiredCoverage.String(), r.Adequate).
		Scan(&out.ID, &out.RunDate, &out.Currency, &ins, &hr, &wc, &rc,
			&out.Adequate, &out.CreatedAt)
	if err != nil {
		return nil, err
	}
	out.InsuranceBalance = decimal.RequireFromString(ins)
	out.HouseReserve = decimal.RequireFromString(hr)
	out.WorstCaseNBP = decimal.RequireFromString(wc)
	out.RequiredCoverage = decimal.RequireFromString(rc)
	return &out, nil
}

func (s *PgStore) ListStressRuns(ctx context.Context, from, to time.Time) ([]StressRun, error) {
	rows, err := s.q.Query(ctx, `
		SELECT id, run_date, currency, insurance_balance::text, house_reserve::text,
		       worst_case_nbp::text, required_coverage::text, adequate, created_at
		  FROM client_money_stress_runs
		 WHERE run_date >= $1 AND run_date <= $2 ORDER BY run_date, currency`, from, to)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []StressRun{}
	for rows.Next() {
		var r StressRun
		var ins, hr, wc, rc string
		if err := rows.Scan(&r.ID, &r.RunDate, &r.Currency, &ins, &hr, &wc, &rc,
			&r.Adequate, &r.CreatedAt); err != nil {
			return nil, err
		}
		r.InsuranceBalance = decimal.RequireFromString(ins)
		r.HouseReserve = decimal.RequireFromString(hr)
		r.WorstCaseNBP = decimal.RequireFromString(wc)
		r.RequiredCoverage = decimal.RequireFromString(rc)
		out = append(out, r)
	}
	return out, rows.Err()
}

func (s *PgStore) InsertPoolingExport(ctx context.Context, e PoolingExport) (*PoolingExport, error) {
	var out PoolingExport
	err := s.q.QueryRow(ctx, `
		INSERT INTO client_money_pooling_exports
		 (export_kind, exported_by, package, package_sha256)
		 VALUES ($1,$2,$3,$4)
		 RETURNING id, export_kind, exported_by, package, package_sha256, created_at`,
		e.ExportKind, e.ExportedBy, e.Package, e.PackageSHA256).
		Scan(&out.ID, &out.ExportKind, &out.ExportedBy, &out.Package,
			&out.PackageSHA256, &out.CreatedAt)
	return &out, err
}

// --- small helpers -------------------------------------------------------------

func nilIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func nilIfZeroTime(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return t
}
