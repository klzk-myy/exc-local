package reconciliation

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"exchange/pkg/decimal"
)

// Category names the nine Task 13.3.2 financial-correctness domains.
type Category string

const (
	CatBalances      Category = "BALANCES"
	CatPositions     Category = "POSITIONS"
	CatOrders        Category = "ORDERS"
	CatTrades        Category = "TRADES"
	CatFunding       Category = "FUNDING"
	CatSettlement    Category = "SETTLEMENT"
	CatFees          Category = "FEES"
	CatPnL           Category = "PNL"
	CatGeneralLedger Category = "GENERAL_LEDGER"
)

// AllCategories is the Task 13.3.2 category list in canonical order.
var AllCategories = []Category{
	CatBalances, CatPositions, CatOrders, CatTrades, CatFunding,
	CatSettlement, CatFees, CatPnL, CatGeneralLedger,
}

// Severity is the finding disposition. Only MISMATCH emits a halt;
// INCONCLUSIVE is a verification gap (fail-closed pessimism: an
// unreachable leg is never silently clean).
type Severity string

const (
	SevMismatch     Severity = "MISMATCH"
	SevInconclusive Severity = "INCONCLUSIVE"
	SevInfo         Severity = "INFO"
)

// Unit qualifies the expected/actual/delta triple.
type Unit string

const (
	UnitAmount Unit = "AMOUNT" // fixed-point money (8dp)
	UnitCount  Unit = "COUNT"  // row/entity counts
	UnitQty    Unit = "QTY"    // quantity units (8dp)
	UnitState  Unit = "STATE"  // non-numeric state divergence (amounts nil)
)

// Finding is one divergent (category, subject) pair. Expected/Actual/
// Delta are 8dp fixed-point; Delta == Actual − Expected when both legs
// measured. HaltScope/HaltTarget carry the finding's protective scope —
// "" scope escalates to GLOBAL at dispatch (ruling R4).
type Finding struct {
	Category   Category         `json:"category"`
	Subject    string           `json:"subject"`
	Leg        string           `json:"leg"`
	Expected   *decimal.Decimal `json:"expected,omitempty"`
	Actual     *decimal.Decimal `json:"actual,omitempty"`
	Delta      *decimal.Decimal `json:"delta,omitempty"`
	Unit       Unit             `json:"unit"`
	Severity   Severity         `json:"severity"`
	HaltScope  string           `json:"halt_scope,omitempty"`
	HaltTarget string           `json:"halt_target,omitempty"`
	Detail     map[string]any   `json:"detail,omitempty"`
}

// AmountFinding builds a MISMATCH finding with the measured triple.
func AmountFinding(cat Category, subject, leg string, expected, actual decimal.Decimal, unit Unit) Finding {
	exp := expected.Round(8)
	act := actual.Round(8)
	dlt := act.Sub(exp)
	return Finding{
		Category: cat, Subject: subject, Leg: leg,
		Expected: &exp, Actual: &act, Delta: &dlt,
		Unit: unit, Severity: SevMismatch,
	}
}

// InconclusiveFinding records a leg that could not be verified.
func InconclusiveFinding(cat Category, subject, leg, reason string) Finding {
	return Finding{
		Category: cat, Subject: subject, Leg: leg,
		Unit:     UnitState,
		Severity: SevInconclusive,
		Detail:   map[string]any{"reason": reason},
	}
}

// WithHalt stamps the protective scope onto a MISMATCH finding.
func (f Finding) WithHalt(scope, target string) Finding {
	f.HaltScope, f.HaltTarget = scope, target
	return f
}

// WithDetail attaches free-form evidence.
func (f Finding) WithDetail(kv map[string]any) Finding {
	f.Detail = kv
	return f
}

// ---------------------------------------------------------------------------
// Run — the persisted sweep record (reconciliation_runs, migration 207).
// ---------------------------------------------------------------------------

// RunStatus is the reconciliation_runs.status vocabulary.
type RunStatus string

const (
	RunRunning      RunStatus = "RUNNING"
	RunClean        RunStatus = "CLEAN"
	RunMismatch     RunStatus = "MISMATCH"
	RunInconclusive RunStatus = "INCONCLUSIVE"
	RunError        RunStatus = "ERROR"
)

// HaltRecord is one emitted auto-halt (trading_suspensions row id +
// Redis flag) recorded on the run.
type HaltRecord struct {
	Scope        string `json:"scope"`
	Target       string `json:"target"`
	Reason       string `json:"reason"`
	SuspensionID int64  `json:"suspension_id,omitempty"`
	Error        string `json:"error,omitempty"`
}

// Run is the sweep report row.
type Run struct {
	ID                int64        `json:"id"`
	StartedAt         time.Time    `json:"started_at"`
	FinishedAt        *time.Time   `json:"finished_at,omitempty"`
	Status            RunStatus    `json:"status"`
	CategoriesChecked int          `json:"categories_checked"`
	FindingsCount     int          `json:"findings_count"`
	MismatchCount     int          `json:"mismatch_count"`
	InconclusiveCount int          `json:"inconclusive_count"`
	Halts             []HaltRecord `json:"halts_emitted,omitempty"`
	Error             string       `json:"error,omitempty"`
}

// FindingRow is a persisted finding (reconciliation_findings).
type FindingRow struct {
	ID        int64     `json:"id"`
	RunID     int64     `json:"run_id"`
	CreatedAt time.Time `json:"created_at"`
	Finding
}

// ---------------------------------------------------------------------------
// Checker contract
// ---------------------------------------------------------------------------

// Scope carries the shared inputs every checker may consult.
type Scope struct {
	Pool    *pgxpool.Pool // nil → every PG leg inconclusive
	WalDirs []string      // per-shard WAL segment directories
	Now     time.Time
}

// Checker verifies one category. Run MUST return findings for the legs
// it could evaluate — including INCONCLUSIVE findings for legs whose
// inputs were unreachable — and MAY return an error when the whole
// category could not run (the engine records that as INCONCLUSIVE too;
// an error is never a clean pass).
type Checker interface {
	Name() Category
	Run(ctx context.Context, s Scope) ([]Finding, error)
}

func (c Category) String() string { return string(c) }

// subject helpers — uniform "kind:id" addressing in findings.
func accountSubject(accountID int64, currency string) string {
	return fmt.Sprintf("account:%d:%s", accountID, currency)
}
func orderSubject(orderID int64) string { return fmt.Sprintf("order:%d", orderID) }
func tradeSubject(tradeID int64) string { return fmt.Sprintf("trade:%d", tradeID) }
func fundingSubject(id int64) string    { return fmt.Sprintf("funding:%d", id) }
func settlementSubject(id int64) string { return fmt.Sprintf("settlement:%d", id) }
func journalSubject(id int64, ccy string) string {
	return fmt.Sprintf("journal:%d:%s", id, ccy)
}
func currencySubject(ccy string) string { return "currency:" + ccy }
func positionSubject(accountID, instrumentID int64) string {
	return fmt.Sprintf("position:%d:%d", accountID, instrumentID)
}
