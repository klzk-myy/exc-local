// bilateral_credit.go — Phase-19 Task 19.3.10: bilateral credit groups,
// mutual screening and atomic pre-match reservations (spec §5.30, §13.8,
// §24 #165; migration 053).
//
// Model. A credit_group belongs to one grantor account and carries a
// profile — ONE_POOL (single line across products) or TWO_POOL (separate
// spot vs forward/NDF pools). Each credit_relationship is a DIRECTED edge
// grantor_group → grantee_account with gross/net limits, an
// effective/expiry window, a block flag and an optimistic version.
// A candidate match between maker account M and taker account T requires
// BOTH directed edges: M's group → T AND T's group → M, each with headroom
// for the fill notional in the instrument's product pool. Margin,
// prefunding and PB NOP/DSL checks (pb_credit.go) are separate controls —
// bilateral credit is required IN ADDITION to them, never implied by them.
//
// Counter semantics (mirrors the PB store, deliberately unsigned):
// current_gross and current_net both accumulate |fill notional| (USD).
// They differ only in how they are re-based — ReconcileTx projects both
// counters from the credit_reservations ledger (ACTIVE + CONSUMED rows),
// which makes the authoritative state a pure function of the ledger and
// therefore deterministic under WAL replay: replaying a
// reserve/consume/release event reproduces the identical counter value.
// A per-pool "true" signed netting figure arrives with the settlement
// re-base, same contract as PBCreditService.SyncNetOpenPosition.
//
// Reservation lifecycle (all-or-nothing inside one tx):
//
//	ReserveMatchTx   FOR UPDATE both directed edges → check headroom →
//	                 debit counters → INSERT one ACTIVE row per order
//	                 (taker order carries the edge where T is grantee;
//	                 maker order carries the edge where M is grantee —
//	                 satisfying uq_credit_reservations_order, one ACTIVE
//	                 row per order, which also guarantees a resting order
//	                 can never be doubly reserved by concurrent matches).
//	ConsumeTx        fill commit — pro-rata: the consumed portion keeps
//	                 the counters debited (utilization), the remainder is
//	                 credited back and recorded as a RELEASED sibling row.
//	ReleaseTx        cancel/reject/abort — ACTIVE → RELEASED, counters
//	                 credited back exactly.
//	SweepTx          ACTIVE rows past expires_at → EXPIRED + credited;
//	                 ACTIVE rows on terminal/missing orders → RELEASED +
//	                 credited (crash-orphan safety net).
//
// Matrix ABI (services/internal/ipc/credit_matrix.go):
// /dev/shm/exchange_credit_matrix cell[a][b] = directed remaining credit
// in 1e8 USD notional ticks where a/b are the credit_parties indexes of
// the grantor/grantee accounts. The matrix is pool-agnostic (one u64 per
// directed pair), so published cell = MIN remaining across all extant
// relationship rows for that pair — the conservative projection: a
// blocked or exhausted row can only shrink the published cell, never
// widen it. The C++ consume_or_skip screen is therefore fail-closed; the
// PG check inside ReserveMatchTx remains authoritative for the exact
// product pool. Intraday changes propagate as versioned deltas
// (PublishDelta) or full snapshots (PublishSnapshot); VerifyMatrix
// sweeps for the dangerous divergence direction (cell > PG remaining ⇒
// the core could over-admit) and republishes + pages the Risk Manager.
//
// Fail-closed (spec §2.7): nil store → construction error; unknown
// instrument type / missing relationship / breached limit / stale
// version / divergent matrix cell → BILATERAL_CREDIT_EXCEEDED or a
// wrapped internal error, never a silent admit.
package risk

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"exchange/internal/position"
	excredis "exchange/internal/redis"
	"exchange/pkg/decimal"
	excerrors "exchange/pkg/errors"
)

// ---------------------------------------------------------------------------
// Enums & constants (migration 053 CHECK constraints — mirror exactly)
// ---------------------------------------------------------------------------

// Credit group profiles (spec §13.8.2).
const (
	CreditProfileOnePool = "ONE_POOL" // single line across all products
	CreditProfileTwoPool = "TWO_POOL" // separate SPOT vs FORWARD_NDF lines
)

// Product pools carried on credit_relationships.product_pool.
const (
	CreditPoolAll        = "ALL"
	CreditPoolSpot       = "SPOT"
	CreditPoolForwardNDF = "FORWARD_NDF"
)

// Credit group lifecycle.
const (
	CreditGroupStatusActive    = "ACTIVE"
	CreditGroupStatusSuspended = "SUSPENDED"
	CreditGroupStatusClosed    = "CLOSED"
)

// Reservation lifecycle (migration 053 CHECK).
const (
	CreditReservationActive   = "ACTIVE"
	CreditReservationConsumed = "CONSUMED"
	CreditReservationReleased = "RELEASED"
	CreditReservationExpired  = "EXPIRED"
)

// CreditUtilizationAlertPct is the spec §13.8.2 telemetry threshold —
// utilization ≥ 90% of either bound pages the Risk Manager.
const CreditUtilizationAlertPct = 90

// CreditTicksPerUnit scales USD notional into the matrix's u64 cell units
// (1e8 ticks per currency unit — same 1e8 contract as the C++ ABI).
const CreditTicksPerUnit int64 = 100_000_000

// CodeBilateralCreditExceeded is the registered §23 code (422/L2).
const CodeBilateralCreditExceeded = "BILATERAL_CREDIT_EXCEEDED"

// Ops-alert codes (internal-only alert vocabulary; not HTTP codes).
const (
	codeCreditUtilization = "BILATERAL_CREDIT_UTILIZATION"
	codeCreditDivergence  = "BILATERAL_CREDIT_DIVERGENCE"
)

// DefaultCreditReservationTTL bounds how long an unconsumed reservation
// may survive a crash before SweepTx expires it. Reservations are meant
// to live only for the reserve→commit span (µs-scale); the TTL is purely
// the orphan safety net.
const DefaultCreditReservationTTL = 30 * time.Second

// bilateralCreditAlertKey is the Redis NX dedupe key for the 90% alert —
// one page per (relationship, bound) per TTL window.
func bilateralCreditAlertKey(relID int64, bound string) string {
	return fmt.Sprintf("bilateral_credit_alert:%d:%s", relID, bound)
}

// CreditPoolForInstrumentType maps instruments.instrument_type_enum
// (migration 001) onto the §13.8.2 two-pool split. Fail-closed: unknown
// or empty types are an error — a product we cannot classify never
// silently routes into a pool. OPTION is grouped with forward-dated
// products (deliverable FX options carry counterparty settlement
// exposure, not spot).
func CreditPoolForInstrumentType(instrumentType string) (string, error) {
	switch strings.ToUpper(strings.TrimSpace(instrumentType)) {
	case "SPOT":
		return CreditPoolSpot, nil
	case "FORWARD", "SWAP", "NDF", "OPTION":
		return CreditPoolForwardNDF, nil
	case "":
		return "", fmt.Errorf("bilateral credit: empty instrument type")
	default:
		return "", fmt.Errorf("bilateral credit: unknown instrument type %q", instrumentType)
	}
}

// CreditTicks converts a USD notional into the matrix's u64 tick units.
// Negative → 0 (fail-closed); values above u64 saturate (an effectively
// uncapped line). Deterministic: truncation toward zero.
func CreditTicks(usd decimal.Decimal) uint64 {
	if !usd.IsPositive() {
		return 0
	}
	maxTicks := decimal.NewFromInt(math.MaxInt64).Shift(-8)
	if usd.Cmp(maxTicks) >= 0 {
		return math.MaxUint64
	}
	return uint64(usd.Shift(8).IntPart())
}

// ---------------------------------------------------------------------------
// Row types
// ---------------------------------------------------------------------------

// CreditGroup mirrors one credit_groups row (migration 053).
type CreditGroup struct {
	ID               int64
	GrantorAccountID int64
	Name             string
	Profile          string // ONE_POOL | TWO_POOL
	Status           string // ACTIVE | SUSPENDED | CLOSED
	CreatedAt        time.Time
	UpdatedAt        time.Time
}

// CreditRelationship is one directed credit edge grantor → grantee with
// its group's fields joined in (GrantorAccountID/GrantorProfile/
// GrantorStatus). NULL limits scan as nil pointers = uncapped.
type CreditRelationship struct {
	ID               int64
	GrantorGroupID   int64
	GrantorAccountID int64
	GrantorProfile   string
	GrantorStatus    string
	GranteeAccountID int64
	ProductPool      string
	GrossLimit       *decimal.Decimal
	NetLimit         *decimal.Decimal
	CurrentGross     decimal.Decimal
	CurrentNet       decimal.Decimal
	EffectiveAt      *time.Time
	ExpiresAt        *time.Time
	Blocked          bool
	Version          int64
	UpdatedAt        time.Time
}

// ActiveAt evaluates the full activity predicate at instant now:
// group ACTIVE, edge not blocked, inside the [effective_at, expires_at)
// window (NULL bounds are open).
func (r CreditRelationship) ActiveAt(now time.Time) bool {
	if r.GrantorStatus != CreditGroupStatusActive || r.Blocked {
		return false
	}
	if r.EffectiveAt != nil && now.Before(*r.EffectiveAt) {
		return false
	}
	if r.ExpiresAt != nil && !now.Before(*r.ExpiresAt) {
		return false
	}
	return true
}

// AppliesToPool — row applies to the requested pool when it is the
// wildcard ALL line or the pool-specific line.
func (r CreditRelationship) AppliesToPool(pool string) bool {
	return r.ProductPool == CreditPoolAll || r.ProductPool == pool
}

// Remaining computes the USD headroom left on this edge at instant now:
// the min over the capped bounds (NULL bound = uncapped side); an
// inactive edge contributes 0. Never negative.
func (r CreditRelationship) Remaining(now time.Time) decimal.Decimal {
	if !r.ActiveAt(now) {
		return decimal.Zero
	}
	rem := decimal.NewFromInt(math.MaxInt64) // effectively uncapped
	if r.GrossLimit != nil {
		if g := r.GrossLimit.Sub(r.CurrentGross); g.Cmp(rem) < 0 {
			rem = g
		}
	}
	if r.NetLimit != nil {
		if n := r.NetLimit.Sub(r.CurrentNet); n.Cmp(rem) < 0 {
			rem = n
		}
	}
	if rem.IsNegative() {
		return decimal.Zero
	}
	return rem
}

// HeadroomOK is the per-edge admission predicate for an USD amount.
func (r CreditRelationship) HeadroomOK(now time.Time, amount decimal.Decimal) bool {
	return r.ActiveAt(now) && r.Remaining(now).Cmp(amount) >= 0
}

// UtilizationPct returns the higher of gross/net utilization in percent —
// 0 for uncapped bounds — for the 90% alert and admin telemetry.
func (r CreditRelationship) UtilizationPct() decimal.Decimal {
	max := decimal.Zero
	if r.GrossLimit != nil && r.GrossLimit.IsPositive() {
		if p := r.CurrentGross.Div(*r.GrossLimit).Mul(decimal.NewFromInt(100)); p.Cmp(max) > 0 {
			max = p
		}
	}
	if r.NetLimit != nil && r.NetLimit.IsPositive() {
		if p := r.CurrentNet.Div(*r.NetLimit).Mul(decimal.NewFromInt(100)); p.Cmp(max) > 0 {
			max = p
		}
	}
	return max
}

// CreditReservation mirrors one credit_reservations row.
type CreditReservation struct {
	ID             int64
	OrderID        int64
	RelationshipID int64
	AccountID      int64
	ProductPool    string
	ReservedAmount decimal.Decimal
	Status         string
	ExpiresAt      time.Time
	CreatedAt      time.Time
	UpdatedAt      time.Time
}

// ---------------------------------------------------------------------------
// Errors
// ---------------------------------------------------------------------------

// CreditBreachError carries which directed edge failed and why — the
// service maps every kind to BILATERAL_CREDIT_EXCEEDED.
type CreditBreachError struct {
	Kind             string // NO_RELATIONSHIP | INACTIVE | GROSS | NET
	GrantorAccountID int64
	GranteeAccountID int64
	RelationshipID   int64 // 0 when no candidate row exists
	Limit            *decimal.Decimal
	Current          decimal.Decimal
	Requested        decimal.Decimal
}

func (e *CreditBreachError) Error() string {
	return fmt.Sprintf("bilateral credit %s: grantor %d → grantee %d (rel %d limit %v current %s req %s)",
		e.Kind, e.GrantorAccountID, e.GranteeAccountID, e.RelationshipID,
		decPtr(e.Limit), e.Current.String(), e.Requested.String())
}

// CreditVersionError is the optimistic-concurrency failure on
// version-checked mutations — the caller observed a stale row.
type CreditVersionError struct {
	RelationshipID  int64
	ExpectedVersion int64
}

func (e *CreditVersionError) Error() string {
	return fmt.Sprintf("bilateral credit: relationship %d version stale (expected %d)",
		e.RelationshipID, e.ExpectedVersion)
}

// ---------------------------------------------------------------------------
// Candidate selection — pure helpers shared by read and write paths
// ---------------------------------------------------------------------------

// pickDirectedEdge chooses THE relationship row a reservation will debit
// among the candidate directed edges for (grantor → grantee). Determinism
// is required for replay: pool-specific rows beat the wildcard ALL row;
// ties break on ascending id. Any row that is active AND has headroom is
// admissible; the first in deterministic order wins. A nil candidate set
// or an all-failing set produces a *CreditBreachError, never a silent pick.
func pickDirectedEdge(edges []CreditRelationship, pool string,
	amount decimal.Decimal, now time.Time,
	grantorID, granteeID int64) (*CreditRelationship, error) {

	var best *CreditRelationship
	var firstInactive *CreditRelationship
	for i := range edges {
		r := edges[i]
		if !r.AppliesToPool(pool) {
			continue
		}
		// Deterministic order: exact-pool rows first, then id ascending.
		specific := 1
		if r.ProductPool == pool && pool != CreditPoolAll {
			specific = 0
		}
		if best != nil {
			bs := 1
			if best.ProductPool == pool && pool != CreditPoolAll {
				bs = 0
			}
			if specific > bs || (specific == bs && r.ID > best.ID) {
				continue
			}
		}
		if !r.ActiveAt(now) {
			if firstInactive == nil {
				cp := r
				firstInactive = &cp
			}
			continue
		}
		if r.HeadroomOK(now, amount) {
			cp := r
			best = &cp
			continue
		}
	}
	if best != nil {
		return best, nil
	}
	// Failure taxonomy, reported in order of "closest to admitting":
	// (a) an ACTIVE applicable row that merely lacks headroom → GROSS/NET;
	// (b) rows exist but none are active (blocked/expired/not yet
	//     effective/suspended group) → INACTIVE;
	// (c) no applicable row at all → NO_RELATIONSHIP.
	var cand *CreditRelationship
	for i := range edges {
		r := edges[i]
		if !r.AppliesToPool(pool) || !r.ActiveAt(now) {
			continue
		}
		if cand == nil || r.ID < cand.ID {
			cp := r
			cand = &cp
		}
	}
	if cand != nil {
		be := &CreditBreachError{Kind: "GROSS", GrantorAccountID: grantorID,
			GranteeAccountID: granteeID, RelationshipID: cand.ID,
			Current: cand.CurrentGross, Requested: amount, Limit: cand.GrossLimit}
		if cand.GrossLimit == nil ||
			cand.CurrentGross.Add(amount).Cmp(*cand.GrossLimit) <= 0 {
			be.Kind = "NET"
			be.Limit = cand.NetLimit
			be.Current = cand.CurrentNet
		}
		return nil, be
	}
	if firstInactive != nil {
		return nil, &CreditBreachError{Kind: "INACTIVE",
			GrantorAccountID: grantorID, GranteeAccountID: granteeID,
			RelationshipID: firstInactive.ID, Requested: amount}
	}
	return nil, &CreditBreachError{Kind: "NO_RELATIONSHIP",
		GrantorAccountID: grantorID, GranteeAccountID: granteeID,
		Requested: amount}
}

// ---------------------------------------------------------------------------
// Store seam
// ---------------------------------------------------------------------------

// CreditReserveSpec is the atomic two-direction reservation input.
// Amount is USD notional (the service converts quote-ccy before calling).
type CreditReserveSpec struct {
	MakerOrderID   int64
	MakerAccountID int64
	TakerOrderID   int64
	TakerAccountID int64
	Pool           string // resolved product pool: SPOT | FORWARD_NDF | ALL
	Amount         decimal.Decimal
	Now            time.Time
	ExpiresAt      time.Time // reservation TTL bound
}

// CreditStore is the persistence seam — PgBilateralCreditStore is the
// production implementation; tests inject fakes.
type CreditStore interface {
	// --- admin / config ---
	CreateGroup(ctx context.Context, g CreditGroup) (CreditGroup, error)
	SetGroupStatus(ctx context.Context, groupID int64, status string) (CreditGroup, error)
	GroupByID(ctx context.Context, groupID int64) (CreditGroup, error)
	GroupsForGrantor(ctx context.Context, grantorAccountID int64) ([]CreditGroup, error)

	// RelationshipByID returns the joined edge; a missing id is an
	// error (fail closed — never nil,nil for a referenced row).
	RelationshipByID(ctx context.Context, relID int64) (CreditRelationship, error)
	// DirectedEdges lists grantor→grantee edges. pool=="" returns every
	// row for the pair; otherwise only rows applicable to the pool
	// (ALL or the named pool). Deterministic order: pool-specific first,
	// then id.
	DirectedEdges(ctx context.Context, grantorAccountID, granteeAccountID int64,
		pool string) ([]CreditRelationship, error)
	EdgesForGrantor(ctx context.Context, grantorAccountID int64) ([]CreditRelationship, error)
	// AllEdges returns every relationship row (any status) joined with
	// its group — the snapshot/reconcile source.
	AllEdges(ctx context.Context) ([]CreditRelationship, error)
	CreateRelationship(ctx context.Context, r CreditRelationship) (CreditRelationship, error)
	// UpdateRelationship is the optimistic-versioned write: updates the
	// row only when version matches r.Version, then bumps version+1.
	// *CreditVersionError on staleness; not-found error on missing id.
	UpdateRelationship(ctx context.Context, r CreditRelationship) (CreditRelationship, error)

	// --- reservation lifecycle (each atomic) ---
	// ReserveMatchTx debits both directed edges and inserts one ACTIVE
	// reservation per order, all-or-nothing. Self-match (same account on
	// both sides) is a no-op returning nil. *CreditBreachError on
	// insufficient/inactive credit — nothing is debited.
	ReserveMatchTx(ctx context.Context, spec CreditReserveSpec) ([]CreditReservation, error)
	// ConsumeTx converts up to `amount` of the order's ACTIVE reservation
	// into utilization (counters keep the consumed portion; the remainder
	// is credited back as a RELEASED sibling). Idempotent — no ACTIVE row
	// is a no-op; amount > reserved fails closed.
	ConsumeTx(ctx context.Context, orderID int64, amount decimal.Decimal) error
	// ConsumeMatchTx applies ConsumeTx to both orders of a match in one
	// transaction.
	ConsumeMatchTx(ctx context.Context, makerOrderID, takerOrderID int64,
		amount decimal.Decimal) error
	// ReleaseTx returns the order's whole ACTIVE reservation to the edge
	// counters (cancel/reject/abort). Idempotent.
	ReleaseTx(ctx context.Context, orderID int64) error
	// ReleaseMatchTx releases both orders' reservations atomically — the
	// match-abort path.
	ReleaseMatchTx(ctx context.Context, makerOrderID, takerOrderID int64) error
	// SweepTx expires TTL-dead ACTIVE rows (→EXPIRED) and releases rows
	// whose order is terminal or missing (→RELEASED), crediting counters.
	SweepTx(ctx context.Context, now time.Time) (int64, error)
	// ReconcileTx re-bases current_gross/current_net to the reservation
	// ledger (Σ ACTIVE+CONSUMED) — the deterministic anti-drift/replay
	// anchor.
	ReconcileTx(ctx context.Context, relID int64) (CreditRelationship, error)
	ReservationsFor(ctx context.Context, orderID int64) ([]CreditReservation, error)

	// --- party indexes (credit_parties ↔ matrix row/col) ---
	// PartyIndex returns the account's fixed matrix index; missing → error.
	PartyIndex(ctx context.Context, accountID int64) (uint32, error)
	// EnsurePartyIndex allocates the next free 0..1023 index on first use
	// (monotonic, never reused); concurrent allocation is safe.
	EnsurePartyIndex(ctx context.Context, accountID int64) (uint32, error)
	// Parties returns the full account→index map (snapshot source).
	Parties(ctx context.Context) (map[int64]uint32, error)
}

// ---------------------------------------------------------------------------
// Pg implementation
// ---------------------------------------------------------------------------

// PgBilateralCreditStore is the production CreditStore.
type PgBilateralCreditStore struct{ P *pgxpool.Pool }

var _ CreditStore = (*PgBilateralCreditStore)(nil)

// NewPgBilateralCreditStore binds the pool; nil fails construction
// (fail-closed — same contract as NewPgLiquidationStore).
func NewPgBilateralCreditStore(p *pgxpool.Pool) (*PgBilateralCreditStore, error) {
	if p == nil {
		return nil, fmt.Errorf("bilateral credit store: nil pgx pool")
	}
	return &PgBilateralCreditStore{P: p}, nil
}

const creditGroupCols = `id, grantor_account_id, name, profile, status, created_at, updated_at`

const creditRelCols = `r.id, r.grantor_group_id, r.grantee_account_id, r.product_pool,
    r.gross_limit::text, r.net_limit::text, r.current_gross::text, r.current_net::text,
    r.effective_at, r.expires_at, r.blocked, r.version, r.updated_at,
    g.grantor_account_id, g.profile, g.status`

func scanCreditGroup(row interface{ Scan(...any) error }) (CreditGroup, error) {
	var g CreditGroup
	err := row.Scan(&g.ID, &g.GrantorAccountID, &g.Name, &g.Profile, &g.Status,
		&g.CreatedAt, &g.UpdatedAt)
	return g, err
}

// scanCreditRelationship parses ::text projections fail-closed — an
// unparseable numeric is an error, never a zero substitute (§2.7).
func scanCreditRelationship(row interface{ Scan(...any) error }) (CreditRelationship, error) {
	var r CreditRelationship
	var grossL, netL, curG, curN *string
	err := row.Scan(&r.ID, &r.GrantorGroupID, &r.GranteeAccountID, &r.ProductPool,
		&grossL, &netL, &curG, &curN, &r.EffectiveAt, &r.ExpiresAt,
		&r.Blocked, &r.Version, &r.UpdatedAt,
		&r.GrantorAccountID, &r.GrantorProfile, &r.GrantorStatus)
	if err != nil {
		return r, err
	}
	parse := func(name string, s *string) (decimal.Decimal, error) {
		if s == nil {
			return decimal.Zero, fmt.Errorf("bilateral credit: rel %d %s is NULL (must be NOT NULL)", r.ID, name)
		}
		d, err := decimal.NewFromString(*s)
		if err != nil {
			return decimal.Zero, fmt.Errorf("bilateral credit: rel %d %s %q unparseable: %w", r.ID, name, *s, err)
		}
		return d, nil
	}
	parsePtr := func(name string, s *string) (*decimal.Decimal, error) {
		if s == nil {
			return nil, nil
		}
		d, err := decimal.NewFromString(*s)
		if err != nil {
			return nil, fmt.Errorf("bilateral credit: rel %d %s %q unparseable: %w", r.ID, name, *s, err)
		}
		return &d, nil
	}
	if r.CurrentGross, err = parse("current_gross", curG); err != nil {
		return r, err
	}
	if r.CurrentNet, err = parse("current_net", curN); err != nil {
		return r, err
	}
	if r.GrossLimit, err = parsePtr("gross_limit", grossL); err != nil {
		return r, err
	}
	if r.NetLimit, err = parsePtr("net_limit", netL); err != nil {
		return r, err
	}
	return r, nil
}

func validCreditGroupStatus(s string) bool {
	switch s {
	case CreditGroupStatusActive, CreditGroupStatusSuspended, CreditGroupStatusClosed:
		return true
	}
	return false
}

func validCreditProfile(s string) bool {
	return s == CreditProfileOnePool || s == CreditProfileTwoPool
}

func validCreditPool(s string) bool {
	switch s {
	case CreditPoolAll, CreditPoolSpot, CreditPoolForwardNDF:
		return true
	}
	return false
}

func (s *PgBilateralCreditStore) CreateGroup(ctx context.Context, g CreditGroup) (CreditGroup, error) {
	if !validCreditProfile(g.Profile) {
		return g, fmt.Errorf("bilateral credit: invalid profile %q", g.Profile)
	}
	if g.Status == "" {
		g.Status = CreditGroupStatusActive
	}
	if !validCreditGroupStatus(g.Status) {
		return g, fmt.Errorf("bilateral credit: invalid group status %q", g.Status)
	}
	return scanCreditGroup(s.P.QueryRow(ctx, `
		INSERT INTO credit_groups (grantor_account_id, name, profile, status)
		VALUES ($1, $2, $3, $4) RETURNING `+creditGroupCols,
		g.GrantorAccountID, g.Name, g.Profile, g.Status))
}

func (s *PgBilateralCreditStore) SetGroupStatus(ctx context.Context, groupID int64,
	status string) (CreditGroup, error) {
	if !validCreditGroupStatus(status) {
		return CreditGroup{}, fmt.Errorf("bilateral credit: invalid group status %q", status)
	}
	g, err := scanCreditGroup(s.P.QueryRow(ctx, `
		UPDATE credit_groups SET status=$2, updated_at=now()
		WHERE id=$1 RETURNING `+creditGroupCols, groupID, status))
	if errors.Is(err, pgx.ErrNoRows) {
		return g, fmt.Errorf("bilateral credit: group %d not found", groupID)
	}
	return g, err
}

func (s *PgBilateralCreditStore) GroupByID(ctx context.Context, groupID int64) (CreditGroup, error) {
	g, err := scanCreditGroup(s.P.QueryRow(ctx, `
		SELECT `+creditGroupCols+` FROM credit_groups WHERE id=$1`, groupID))
	if errors.Is(err, pgx.ErrNoRows) {
		return g, fmt.Errorf("bilateral credit: group %d not found", groupID)
	}
	return g, err
}

func (s *PgBilateralCreditStore) GroupsForGrantor(ctx context.Context,
	grantorAccountID int64) ([]CreditGroup, error) {
	rows, err := s.P.Query(ctx, `
		SELECT `+creditGroupCols+` FROM credit_groups
		WHERE grantor_account_id=$1 ORDER BY id`, grantorAccountID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []CreditGroup
	for rows.Next() {
		g, err := scanCreditGroup(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, g)
	}
	return out, rows.Err()
}

func (s *PgBilateralCreditStore) RelationshipByID(ctx context.Context,
	relID int64) (CreditRelationship, error) {
	r, err := scanCreditRelationship(s.P.QueryRow(ctx, `
		SELECT `+creditRelCols+` FROM credit_relationships r
		JOIN credit_groups g ON g.id = r.grantor_group_id
		WHERE r.id=$1`, relID))
	if errors.Is(err, pgx.ErrNoRows) {
		return r, fmt.Errorf("bilateral credit: relationship %d not found", relID)
	}
	return r, err
}

func (s *PgBilateralCreditStore) DirectedEdges(ctx context.Context,
	grantorAccountID, granteeAccountID int64, pool string) ([]CreditRelationship, error) {
	rows, err := s.P.Query(ctx, `
		SELECT `+creditRelCols+` FROM credit_relationships r
		JOIN credit_groups g ON g.id = r.grantor_group_id
		WHERE g.grantor_account_id=$1 AND r.grantee_account_id=$2
		  AND ($3='' OR r.product_pool='ALL' OR r.product_pool=$3)
		ORDER BY CASE WHEN $3<>'' AND r.product_pool=$3 THEN 0 ELSE 1 END, r.id`,
		grantorAccountID, granteeAccountID, pool)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []CreditRelationship
	for rows.Next() {
		r, err := scanCreditRelationship(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func (s *PgBilateralCreditStore) EdgesForGrantor(ctx context.Context,
	grantorAccountID int64) ([]CreditRelationship, error) {
	rows, err := s.P.Query(ctx, `
		SELECT `+creditRelCols+` FROM credit_relationships r
		JOIN credit_groups g ON g.id = r.grantor_group_id
		WHERE g.grantor_account_id=$1 ORDER BY r.id`, grantorAccountID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []CreditRelationship
	for rows.Next() {
		r, err := scanCreditRelationship(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func (s *PgBilateralCreditStore) AllEdges(ctx context.Context) ([]CreditRelationship, error) {
	rows, err := s.P.Query(ctx, `
		SELECT `+creditRelCols+` FROM credit_relationships r
		JOIN credit_groups g ON g.id = r.grantor_group_id ORDER BY r.id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []CreditRelationship
	for rows.Next() {
		r, err := scanCreditRelationship(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func (s *PgBilateralCreditStore) CreateRelationship(ctx context.Context,
	r CreditRelationship) (CreditRelationship, error) {
	if !validCreditPool(r.ProductPool) {
		return r, fmt.Errorf("bilateral credit: invalid product_pool %q", r.ProductPool)
	}
	if r.GrossLimit != nil && r.GrossLimit.IsNegative() {
		return r, fmt.Errorf("bilateral credit: negative gross_limit")
	}
	if r.NetLimit != nil && r.NetLimit.IsNegative() {
		return r, fmt.Errorf("bilateral credit: negative net_limit")
	}
	var id int64
	err := s.P.QueryRow(ctx, `
		INSERT INTO credit_relationships
		    (grantor_group_id, grantee_account_id, product_pool,
		     gross_limit, net_limit, effective_at, expires_at, blocked)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8)
		RETURNING id`,
		r.GrantorGroupID, r.GranteeAccountID, r.ProductPool,
		decOrNil(r.GrossLimit), decOrNil(r.NetLimit), r.EffectiveAt, r.ExpiresAt, r.Blocked).
		Scan(&id)
	if err != nil {
		return r, fmt.Errorf("bilateral credit: create relationship: %w", err)
	}
	return s.RelationshipByID(ctx, id)
}

func decOrNil(d *decimal.Decimal) any {
	if d == nil {
		return nil
	}
	return *d
}

// UpdateRelationship is the optimistic-versioned write: the UPDATE lands
// only when version matches; zero rows-affected distinguishes stale
// (row exists → *CreditVersionError) from missing (not-found error).
// The row is re-read post-commit so the returned Version is the bumped one.
func (s *PgBilateralCreditStore) UpdateRelationship(ctx context.Context,
	r CreditRelationship) (CreditRelationship, error) {
	if !validCreditPool(r.ProductPool) {
		return r, fmt.Errorf("bilateral credit: invalid product_pool %q", r.ProductPool)
	}
	if r.GrossLimit != nil && r.GrossLimit.IsNegative() {
		return r, fmt.Errorf("bilateral credit: negative gross_limit")
	}
	if r.NetLimit != nil && r.NetLimit.IsNegative() {
		return r, fmt.Errorf("bilateral credit: negative net_limit")
	}
	tag, err := s.P.Exec(ctx, `
		UPDATE credit_relationships SET
		    gross_limit=$2, net_limit=$3, effective_at=$4, expires_at=$5,
		    blocked=$6, version=version+1, updated_at=now()
		WHERE id=$1 AND version=$7`,
		r.ID, decOrNil(r.GrossLimit), decOrNil(r.NetLimit),
		r.EffectiveAt, r.ExpiresAt, r.Blocked, r.Version)
	if err != nil {
		return r, err
	}
	if tag.RowsAffected() == 0 {
		if _, err := s.RelationshipByID(ctx, r.ID); err != nil {
			return r, err // genuinely missing
		}
		return r, &CreditVersionError{RelationshipID: r.ID, ExpectedVersion: r.Version}
	}
	return s.RelationshipByID(ctx, r.ID)
}

// ReserveMatchTx — the §13.8.3 atomic two-direction reserve. Candidate
// rows are gathered unlocked first (id list), then locked in ONE ordered
// FOR UPDATE statement (ascending id ⇒ deterministic global lock order,
// no lock-order inversion between concurrent A↔B matches). Validation and
// headroom checks run on the LOCKED row contents — anything that flipped
// between the gather and the lock is re-evaluated fail-closed.
func (s *PgBilateralCreditStore) ReserveMatchTx(ctx context.Context,
	spec CreditReserveSpec) ([]CreditReservation, error) {
	if spec.MakerAccountID == spec.TakerAccountID {
		return nil, nil // same legal entity — no bilateral exposure
	}
	if !validCreditPool(spec.Pool) {
		return nil, fmt.Errorf("bilateral credit: invalid pool %q", spec.Pool)
	}
	if !spec.Amount.IsPositive() {
		return nil, fmt.Errorf("bilateral credit: reserve amount must be positive")
	}
	tx, err := s.P.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// Gather candidate ids for BOTH directed edges.
	var ids []int64
	rows, err := tx.Query(ctx, `
		SELECT r.id FROM credit_relationships r
		JOIN credit_groups g ON g.id = r.grantor_group_id
		WHERE ((g.grantor_account_id=$1 AND r.grantee_account_id=$2)
		    OR (g.grantor_account_id=$2 AND r.grantee_account_id=$1))
		  AND (r.product_pool='ALL' OR r.product_pool=$3)`,
		spec.MakerAccountID, spec.TakerAccountID, spec.Pool)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, err
		}
		ids = append(ids, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	var locked []CreditRelationship
	if len(ids) > 0 {
		lrows, err := tx.Query(ctx, `
			SELECT `+creditRelCols+` FROM credit_relationships r
			JOIN credit_groups g ON g.id = r.grantor_group_id
			WHERE r.id = ANY($1) ORDER BY r.id FOR UPDATE OF r`, ids)
		if err != nil {
			return nil, err
		}
		for lrows.Next() {
			r, err := scanCreditRelationship(lrows)
			if err != nil {
				lrows.Close()
				return nil, err
			}
			locked = append(locked, r)
		}
		lrows.Close()
		if err := lrows.Err(); err != nil {
			return nil, err
		}
	}

	// Directed picks on locked data.
	var mt, tm []CreditRelationship
	for _, r := range locked {
		switch {
		case r.GrantorAccountID == spec.MakerAccountID && r.GranteeAccountID == spec.TakerAccountID:
			mt = append(mt, r)
		case r.GrantorAccountID == spec.TakerAccountID && r.GranteeAccountID == spec.MakerAccountID:
			tm = append(tm, r)
		}
	}
	edgeMT, err := pickDirectedEdge(mt, spec.Pool, spec.Amount, spec.Now,
		spec.MakerAccountID, spec.TakerAccountID)
	if err != nil {
		return nil, err
	}
	edgeTM, err := pickDirectedEdge(tm, spec.Pool, spec.Amount, spec.Now,
		spec.TakerAccountID, spec.MakerAccountID)
	if err != nil {
		return nil, err
	}

	// Debit both directions (unsigned accumulation — see file header).
	for _, e := range []*CreditRelationship{edgeMT, edgeTM} {
		if _, err := tx.Exec(ctx, `
			UPDATE credit_relationships SET
			    current_gross = current_gross + $2,
			    current_net   = current_net + $2,
			    updated_at = now()
			WHERE id=$1`, e.ID, spec.Amount); err != nil {
			return nil, err
		}
	}

	// One ACTIVE reservation per order: the taker's row carries the edge
	// where the taker is the grantee (maker→taker), and symmetrically the
	// maker's row carries taker→maker.
	var out []CreditReservation
	type ins struct {
		orderID, relID, acctID int64
	}
	for _, in := range []ins{
		{spec.TakerOrderID, edgeMT.ID, spec.TakerAccountID},
		{spec.MakerOrderID, edgeTM.ID, spec.MakerAccountID},
	} {
		var res CreditReservation
		var amt string
		err := tx.QueryRow(ctx, `
			INSERT INTO credit_reservations
			    (order_id, relationship_id, account_id, product_pool,
			     reserved_amount, expires_at)
			VALUES ($1,$2,$3,$4,$5,$6)
			RETURNING id, order_id, relationship_id, account_id, product_pool,
			    reserved_amount::text, status, expires_at, created_at, updated_at`,
			in.orderID, in.relID, in.acctID, spec.Pool,
			spec.Amount, spec.ExpiresAt).Scan(
			&res.ID, &res.OrderID, &res.RelationshipID, &res.AccountID,
			&res.ProductPool, &amt, &res.Status, &res.ExpiresAt,
			&res.CreatedAt, &res.UpdatedAt)
		if err != nil {
			return nil, err
		}
		d, err := decimal.NewFromString(amt)
		if err != nil {
			return nil, fmt.Errorf("bilateral credit: reservation amount %q: %w", amt, err)
		}
		res.ReservedAmount = d
		out = append(out, res)
	}
	return out, tx.Commit(ctx)
}

// consumeLocked applies the pro-rata consume to one order's ACTIVE row
// inside tx. amount==reserved consumes fully; amount<reserved splits —
// the original row becomes CONSUMED with the consumed amount and a
// RELEASED sibling row records the credited-back remainder.
func consumeLocked(ctx context.Context, tx pgx.Tx, orderID int64,
	amount decimal.Decimal) error {
	if amount.IsNegative() {
		return fmt.Errorf("bilateral credit: consume amount must be >= 0")
	}
	var res CreditReservation
	var relID *int64
	var amtStr string
	err := tx.QueryRow(ctx, `
		SELECT id, relationship_id, reserved_amount::text
		FROM credit_reservations
		WHERE order_id=$1 AND status='ACTIVE' FOR UPDATE`, orderID).
		Scan(&res.ID, &relID, &amtStr)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil // idempotent replay — nothing outstanding
	}
	if err != nil {
		return err
	}
	res.ReservedAmount, err = decimal.NewFromString(amtStr)
	if err != nil {
		return fmt.Errorf("bilateral credit: reservation %d amount %q: %w", res.ID, amtStr, err)
	}
	if relID == nil {
		return fmt.Errorf("bilateral credit: reservation %d has NULL relationship_id", res.ID)
	}
	if amount.Cmp(res.ReservedAmount) > 0 {
		return fmt.Errorf("bilateral credit: consume %s exceeds reserved %s (res %d)",
			amount, res.ReservedAmount, res.ID)
	}
	remainder := res.ReservedAmount.Sub(amount)
	if _, err := tx.Exec(ctx, `
		UPDATE credit_reservations SET status='CONSUMED',
		    reserved_amount=$2, updated_at=now()
		WHERE id=$1`, res.ID, amount); err != nil {
		return err
	}
	if remainder.IsPositive() {
		if _, err := tx.Exec(ctx, `
			UPDATE credit_relationships SET
			    current_gross = GREATEST(current_gross - $2, 0),
			    current_net   = GREATEST(current_net - $2, 0),
			    updated_at=now()
			WHERE id=$1`, *relID, remainder); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO credit_reservations
			    (order_id, relationship_id, account_id, product_pool,
			     reserved_amount, status, expires_at)
			SELECT order_id, relationship_id, account_id, product_pool,
			       $2, 'RELEASED', expires_at
			FROM credit_reservations WHERE id=$1`, res.ID, remainder); err != nil {
			return err
		}
	}
	return nil
}

func (s *PgBilateralCreditStore) ConsumeTx(ctx context.Context, orderID int64,
	amount decimal.Decimal) error {
	tx, err := s.P.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := consumeLocked(ctx, tx, orderID, amount); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (s *PgBilateralCreditStore) ConsumeMatchTx(ctx context.Context,
	makerOrderID, takerOrderID int64, amount decimal.Decimal) error {
	tx, err := s.P.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := consumeLocked(ctx, tx, takerOrderID, amount); err != nil {
		return err
	}
	if makerOrderID != takerOrderID {
		if err := consumeLocked(ctx, tx, makerOrderID, amount); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

// releaseLocked credits one order's ACTIVE reservation back to its edge.
func releaseLocked(ctx context.Context, tx pgx.Tx, orderID int64,
	newStatus string) (int64, error) {
	rows, err := tx.Query(ctx, `
		SELECT id, relationship_id, reserved_amount::text
		FROM credit_reservations
		WHERE order_id=$1 AND status='ACTIVE' FOR UPDATE`, orderID)
	if err != nil {
		return 0, err
	}
	type rel struct {
		id    int64
		relID *int64
		amt   decimal.Decimal
	}
	var rels []rel
	for rows.Next() {
		var r rel
		var amt string
		if err := rows.Scan(&r.id, &r.relID, &amt); err != nil {
			rows.Close()
			return 0, err
		}
		d, err := decimal.NewFromString(amt)
		if err != nil {
			rows.Close()
			return 0, fmt.Errorf("bilateral credit: reservation amount %q: %w", amt, err)
		}
		r.amt = d
		rels = append(rels, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}
	for _, r := range rels {
		if r.relID == nil {
			return 0, fmt.Errorf("bilateral credit: reservation %d NULL relationship_id", r.id)
		}
		if _, err := tx.Exec(ctx, `
			UPDATE credit_relationships SET
			    current_gross = GREATEST(current_gross - $2, 0),
			    current_net   = GREATEST(current_net - $2, 0),
			    updated_at=now()
			WHERE id=$1`, *r.relID, r.amt); err != nil {
			return 0, err
		}
		if _, err := tx.Exec(ctx, `
			UPDATE credit_reservations SET status=$2, updated_at=now()
			WHERE id=$1`, r.id, newStatus); err != nil {
			return 0, err
		}
	}
	return int64(len(rels)), nil
}

func (s *PgBilateralCreditStore) ReleaseTx(ctx context.Context, orderID int64) error {
	tx, err := s.P.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := releaseLocked(ctx, tx, orderID, CreditReservationReleased); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (s *PgBilateralCreditStore) ReleaseMatchTx(ctx context.Context,
	makerOrderID, takerOrderID int64) error {
	tx, err := s.P.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := releaseLocked(ctx, tx, takerOrderID, CreditReservationReleased); err != nil {
		return err
	}
	if makerOrderID != takerOrderID {
		if _, err := releaseLocked(ctx, tx, makerOrderID, CreditReservationReleased); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

// SweepTx — expiry + orphan safety net in one tx. Rows past expires_at
// become EXPIRED; rows on terminal or missing orders become RELEASED.
// Both credit the edge counters back.
func (s *PgBilateralCreditStore) SweepTx(ctx context.Context, now time.Time) (int64, error) {
	tx, err := s.P.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	rows, err := tx.Query(ctx, `
		SELECT r.id, r.relationship_id, r.reserved_amount::text,
		       CASE WHEN r.expires_at <= $1 THEN 'EXPIRED' ELSE 'RELEASED' END
		FROM credit_reservations r
		LEFT JOIN orders o ON o.id = r.order_id
		WHERE r.status='ACTIVE'
		  AND (r.expires_at <= $1
		       OR o.id IS NULL
		       OR o.status IN ('FILLED','CANCELLED','REJECTED','EXPIRED'))
		ORDER BY r.id FOR UPDATE OF r`, now)
	if err != nil {
		return 0, err
	}
	type rel struct {
		id       int64
		relID    *int64
		amt      decimal.Decimal
		newState string
	}
	var rels []rel
	for rows.Next() {
		var r rel
		var amt string
		if err := rows.Scan(&r.id, &r.relID, &amt, &r.newState); err != nil {
			rows.Close()
			return 0, err
		}
		d, err := decimal.NewFromString(amt)
		if err != nil {
			rows.Close()
			return 0, fmt.Errorf("bilateral credit: reservation amount %q: %w", amt, err)
		}
		r.amt = d
		rels = append(rels, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}
	for _, r := range rels {
		if r.relID == nil {
			return 0, fmt.Errorf("bilateral credit: reservation %d NULL relationship_id", r.id)
		}
		if _, err := tx.Exec(ctx, `
			UPDATE credit_relationships SET
			    current_gross = GREATEST(current_gross - $2, 0),
			    current_net   = GREATEST(current_net - $2, 0),
			    updated_at=now()
			WHERE id=$1`, *r.relID, r.amt); err != nil {
			return 0, err
		}
		if _, err := tx.Exec(ctx, `
			UPDATE credit_reservations SET status=$2, updated_at=now()
			WHERE id=$1`, r.id, r.newState); err != nil {
			return 0, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, err
	}
	return int64(len(rels)), nil
}

// ReconcileTx re-bases the counters from the reservation ledger —
// Σ(ACTIVE+CONSUMED) is the deterministic projection that survives replay.
func (s *PgBilateralCreditStore) ReconcileTx(ctx context.Context,
	relID int64) (CreditRelationship, error) {
	tx, err := s.P.Begin(ctx)
	if err != nil {
		return CreditRelationship{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var cur CreditRelationship
	row := tx.QueryRow(ctx, `
		SELECT `+creditRelCols+` FROM credit_relationships r
		JOIN credit_groups g ON g.id = r.grantor_group_id
		WHERE r.id=$1 FOR UPDATE OF r`, relID)
	cur, err = scanCreditRelationship(row)
	if err != nil {
		return cur, fmt.Errorf("bilateral credit: reconcile rel %d: %w", relID, err)
	}
	if _, err := tx.Exec(ctx, `
		UPDATE credit_relationships SET
		    current_gross = COALESCE((
		        SELECT SUM(reserved_amount) FROM credit_reservations
		        WHERE relationship_id=$1 AND status IN ('ACTIVE','CONSUMED')), 0),
		    current_net = COALESCE((
		        SELECT SUM(reserved_amount) FROM credit_reservations
		        WHERE relationship_id=$1 AND status IN ('ACTIVE','CONSUMED')), 0),
		    updated_at=now()
		WHERE id=$1`, relID); err != nil {
		return cur, err
	}
	if err := tx.Commit(ctx); err != nil {
		return cur, err
	}
	return s.RelationshipByID(ctx, relID)
}

func (s *PgBilateralCreditStore) ReservationsFor(ctx context.Context,
	orderID int64) ([]CreditReservation, error) {
	rows, err := s.P.Query(ctx, `
		SELECT id, order_id, relationship_id, account_id, product_pool,
		       reserved_amount::text, status, expires_at, created_at, updated_at
		FROM credit_reservations WHERE order_id=$1 ORDER BY id`, orderID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []CreditReservation
	for rows.Next() {
		var r CreditReservation
		var amt string
		var relID *int64
		if err := rows.Scan(&r.ID, &r.OrderID, &relID, &r.AccountID,
			&r.ProductPool, &amt, &r.Status, &r.ExpiresAt,
			&r.CreatedAt, &r.UpdatedAt); err != nil {
			return nil, err
		}
		d, err := decimal.NewFromString(amt)
		if err != nil {
			return nil, fmt.Errorf("bilateral credit: reservation amount %q: %w", amt, err)
		}
		r.ReservedAmount = d
		if relID != nil {
			r.RelationshipID = *relID
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func (s *PgBilateralCreditStore) PartyIndex(ctx context.Context, accountID int64) (uint32, error) {
	var idx int
	err := s.P.QueryRow(ctx,
		`SELECT party_index FROM credit_parties WHERE account_id=$1`, accountID).Scan(&idx)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, fmt.Errorf("bilateral credit: account %d has no party index", accountID)
	}
	if err != nil {
		return 0, err
	}
	return uint32(idx), nil
}

// EnsurePartyIndex allocates monotonically (max+1); the unique index on
// party_index serializes concurrent allocators — a collision retries the
// whole read-then-insert cycle.
func (s *PgBilateralCreditStore) EnsurePartyIndex(ctx context.Context,
	accountID int64) (uint32, error) {
	if idx, err := s.PartyIndex(ctx, accountID); err == nil {
		return idx, nil
	}
	for attempt := 0; attempt < 4; attempt++ {
		tag, err := s.P.Exec(ctx, `
			INSERT INTO credit_parties (account_id, party_index)
			SELECT $1, COALESCE(MAX(party_index), -1) + 1 FROM credit_parties
			ON CONFLICT (account_id) DO NOTHING`, accountID)
		if err == nil && tag.RowsAffected() == 1 {
			return s.PartyIndex(ctx, accountID)
		}
		if err != nil {
			// unique-violation on party_index under a concurrent insert →
			// the winner's row is now visible; re-read.
			var pgErr interface{ SQLState() string }
			if errors.As(err, &pgErr) && pgErr.SQLState() == "23505" {
				continue
			}
			return 0, fmt.Errorf("bilateral credit: ensure party %d: %w", accountID, err)
		}
		// RowsAffected 0 → another allocator inserted this account first.
		return s.PartyIndex(ctx, accountID)
	}
	return s.PartyIndex(ctx, accountID)
}

func (s *PgBilateralCreditStore) Parties(ctx context.Context) (map[int64]uint32, error) {
	rows, err := s.P.Query(ctx,
		`SELECT account_id, party_index FROM credit_parties ORDER BY party_index`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int64]uint32{}
	for rows.Next() {
		var a int64
		var p int
		if err := rows.Scan(&a, &p); err != nil {
			return nil, err
		}
		out[a] = uint32(p)
	}
	return out, rows.Err()
}

// ---------------------------------------------------------------------------
// Credit-screened market view (spec §13.8.4) — pure, no I/O
// ---------------------------------------------------------------------------

// ScreenedOrder is one resting order evaluated for accessibility. The
// counterparty identity exists ONLY on input (Party = matrix party index)
// and is never copied into the output — the screened view aggregates by
// price level so an observer cannot infer which counterparty was
// suppressed.
type ScreenedOrder struct {
	Party         uint32          // counterparty party index
	Price         decimal.Decimal // resting limit price
	Quantity      decimal.Decimal // remaining executable quantity
	NotionalTicks uint64          // qty×price in 1e8 USD ticks (caller computes)
}

// ScreenedLevel is one aggregated price level of the executable view.
type ScreenedLevel struct {
	Price    decimal.Decimal
	Quantity decimal.Decimal
}

// ScreenedDepth is the output book view. CreditScreened labels the view
// per §13.8.4 — private FIX/SBE views set true; the public anonymous
// aggregate sets false (PublicDepth helper).
type ScreenedDepth struct {
	CreditScreened bool
	Levels         []ScreenedLevel // in input (book) order, aggregated by price
}

// PublicDepth wraps an already-anonymous aggregate view — labelled
// non-credit-screened per §13.8.4.
func PublicDepth(levels []ScreenedLevel) ScreenedDepth {
	return ScreenedDepth{CreditScreened: false, Levels: levels}
}

// HeadroomFunc reports whether directed mutual credit between maker
// (resting side) and taker (viewer) covers notionalTicks — the exact
// signature of (*ipc.CreditMatrix).HasHeadroom.
type HeadroomFunc func(makerParty, takerParty uint32, notionalTicks uint64) bool

// CreditScreener is the structural seam *ipc.CreditMatrix satisfies.
type CreditScreener interface {
	HasHeadroom(maker, taker uint32, notionalTicks uint64) bool
}

// HeadroomFromMatrix adapts a matrix reader to HeadroomFunc. nil → nil
// (ScreenLiquidity treats a missing oracle as zero credit — fail closed).
func HeadroomFromMatrix(s CreditScreener) HeadroomFunc {
	if s == nil {
		return nil
	}
	return s.HasHeadroom
}

// ScreenLiquidity filters the opposite book to the liquidity `viewerParty`
// may actually execute against: each resting order survives only when the
// headroom oracle approves BOTH directed edges for the order's full
// remaining notional. Suppressed orders leave the book untouched
// conceptually (this view is a read model — the engine's consume_or_skip
// preserves priority on the real book). Output aggregates by price level
// and contains no counterparty identity. A nil oracle yields an empty
// screened view — fail closed.
func ScreenLiquidity(viewerParty uint32, resting []ScreenedOrder,
	headroom HeadroomFunc) ScreenedDepth {
	out := ScreenedDepth{CreditScreened: true}
	if headroom == nil {
		return out
	}
	idx := map[string]int{}
	for _, o := range resting {
		if !headroom(o.Party, viewerParty, o.NotionalTicks) {
			continue // inaccessible — suppressed without identity
		}
		k := o.Price.String()
		if i, ok := idx[k]; ok {
			out.Levels[i].Quantity = out.Levels[i].Quantity.Add(o.Quantity)
			continue
		}
		idx[k] = len(out.Levels)
		out.Levels = append(out.Levels,
			ScreenedLevel{Price: o.Price, Quantity: o.Quantity})
	}
	return out
}

// ---------------------------------------------------------------------------
// Matrix publish seams (structural — *ipc.CreditMatrix satisfies both)
// ---------------------------------------------------------------------------

// CreditCellWriter is the publish seam onto /exchange_credit_matrix.
type CreditCellWriter interface {
	ApplyUpdate(a, b uint32, newLimit uint64) bool
}

// CreditCellReader reads cells back for the divergence sweep.
type CreditCellReader interface {
	CreditLimit(a, b uint32) uint64
}

// ---------------------------------------------------------------------------
// Service
// ---------------------------------------------------------------------------

// CreditMatchRequest describes one candidate match for screening/reserve.
// Notional is qty×price in QuoteCurrency; the service converts to USD.
type CreditMatchRequest struct {
	MakerOrderID   int64
	MakerAccountID int64
	TakerOrderID   int64
	TakerAccountID int64
	InstrumentType string // instruments.instrument_type (SPOT|FORWARD|SWAP|NDF|OPTION)
	QuoteCurrency  string // e.g. "USD"; identity conversion for USD
	Notional       decimal.Decimal
	ReservationTTL time.Duration // 0 → DefaultCreditReservationTTL
}

// CreditUtilization is the telemetry/alert view of one edge.
type CreditUtilization struct {
	Relationship CreditRelationship
	Pct          decimal.Decimal // max(gross%, net%); 0 when uncapped
	Alerted      bool            // ≥ CreditUtilizationAlertPct
}

// BilateralCreditService enforces §13.8 mutual credit around the match
// path and owns PG→matrix propagation. Safe for concurrent use; every
// store call is self-transactional.
type BilateralCreditService struct {
	store   CreditStore
	conv    *position.Converter
	rdb     *excredis.Client // best-effort 90% alert dedupe; nil → fire each time
	alerter OpsAlerter       // Risk Manager paging seam (NATS ops.alerts.risk)
	writer  CreditCellWriter // nil → publishing disabled (dev)
	reader  CreditCellReader // nil → divergence probe skipped
	resTTL  time.Duration
	now     func() time.Time
	logf    func(string, ...any)
}

// BilateralCreditOptions wires the service.
type BilateralCreditOptions struct {
	Store          CreditStore         // required — nil fails construction
	Conv           *position.Converter // nil → converter with identity-USD only
	Redis          *excredis.Client    // optional
	Alerter        OpsAlerter          // optional — divergence/utilization pages
	Writer         CreditCellWriter    // optional — *ipc.CreditMatrix
	Reader         CreditCellReader    // optional — same handle for VerifyMatrix
	ReservationTTL time.Duration
	Now            func() time.Time
	Logf           func(string, ...any)
}

func NewBilateralCreditService(o BilateralCreditOptions) (*BilateralCreditService, error) {
	if o.Store == nil {
		return nil, fmt.Errorf("bilateral credit: store is nil")
	}
	conv := o.Conv // nil → non-USD notionals fail closed in toUSD
	now := o.Now
	if now == nil {
		now = func() time.Time { return time.Now().UTC() }
	}
	ttl := o.ReservationTTL
	if ttl <= 0 {
		ttl = DefaultCreditReservationTTL
	}
	return &BilateralCreditService{
		store: o.Store, conv: conv, rdb: o.Redis, alerter: o.Alerter,
		writer: o.Writer, reader: o.Reader, resTTL: ttl, now: now,
		logf: o.Logf,
	}, nil
}

func (s *BilateralCreditService) log(format string, args ...any) {
	if s.logf != nil {
		s.logf("bilateral credit: "+format, args...)
	}
}

// toUSD converts quote-ccy notional to USD at mark — identical contract
// to PBCreditService.toUSD: conversion failure fails closed.
func (s *BilateralCreditService) toUSD(ctx context.Context, amount decimal.Decimal,
	quoteCcy string) (decimal.Decimal, error) {
	q := strings.ToUpper(strings.TrimSpace(quoteCcy))
	if q == "" || q == "USD" {
		return amount.Abs(), nil
	}
	if s.conv == nil {
		return decimal.Zero, excerrors.New(CodeRiskLimitsInternal,
			fmt.Sprintf("bilateral credit: %s→USD converter not bound", q))
	}
	c, err := s.conv.Convert(ctx, amount.Abs(), q, "USD")
	if err != nil {
		return decimal.Zero, excerrors.Wrap(CodeRiskLimitsInternal,
			fmt.Sprintf("bilateral credit: %s→USD mark unavailable", q), err)
	}
	return c.ToAmount, nil
}

// breachError maps every CreditBreachError kind to the registered code.
func (s *BilateralCreditService) breachError(be *CreditBreachError) error {
	return excerrors.New(CodeBilateralCreditExceeded, be.Error())
}

// versionError — stale observed version is the fail-closed reject named
// by the task: BILATERAL_CREDIT_EXCEEDED + a Risk Manager page.
func (s *BilateralCreditService) versionError(ctx context.Context,
	ve *CreditVersionError) error {
	s.raiseAlert(ctx, SeverityP1, codeCreditDivergence,
		fmt.Sprintf("stale credit relationship version: rel %d expected %d",
			ve.RelationshipID, ve.ExpectedVersion),
		map[string]string{"relationship_id": fmt.Sprint(ve.RelationshipID)})
	return excerrors.New(CodeBilateralCreditExceeded, ve.Error())
}

// CheckMatch is the read-only pre-screen: BOTH directed edges must be
// active with headroom for the fill. When a matrix reader is bound the
// published cells are probed for the unsafe divergence direction —
// cell[a][b] > PG remaining means the core could over-admit, which fails
// closed with a rejection AND a Risk Manager page (spec §13.8.5 + AC).
// Self-match (same account) is a no-op.
func (s *BilateralCreditService) CheckMatch(ctx context.Context,
	m CreditMatchRequest) error {
	if s.store == nil {
		return excerrors.New(CodeRiskLimitsInternal, "bilateral credit store not configured")
	}
	if m.MakerAccountID == m.TakerAccountID {
		return nil // same legal entity — no bilateral exposure
	}
	pool, err := CreditPoolForInstrumentType(m.InstrumentType)
	if err != nil {
		return excerrors.Wrap(CodeRiskLimitsInternal, "bilateral credit pool", err)
	}
	usd, err := s.toUSD(ctx, m.Notional, m.QuoteCurrency)
	if err != nil {
		return err
	}
	now := s.now()
	for _, dir := range [][2]int64{
		{m.MakerAccountID, m.TakerAccountID},
		{m.TakerAccountID, m.MakerAccountID},
	} {
		edges, err := s.store.DirectedEdges(ctx, dir[0], dir[1], pool)
		if err != nil {
			return excerrors.Wrap(CodeRiskLimitsInternal, "bilateral credit edges", err)
		}
		if _, err := pickDirectedEdge(edges, pool, usd, now, dir[0], dir[1]); err != nil {
			var be *CreditBreachError
			if errors.As(err, &be) {
				return s.breachError(be)
			}
			return excerrors.Wrap(CodeRiskLimitsInternal, "bilateral credit pick", err)
		}
	}
	return s.probeDivergence(ctx, m.MakerAccountID, m.TakerAccountID, now)
}

// probeDivergence compares published matrix cells against PG remaining —
// only the over-admission direction (cell > remaining) is divergence; a
// lower cell is normal engine debit. Divergence → reject + alert.
func (s *BilateralCreditService) probeDivergence(ctx context.Context,
	accountA, accountB int64, now time.Time) error {
	if s.reader == nil {
		return nil
	}
	pa, errA := s.store.PartyIndex(ctx, accountA)
	pb, errB := s.store.PartyIndex(ctx, accountB)
	if errA != nil || errB != nil {
		return nil // no party index → pair was never credit-onboarded
	}
	for _, dir := range [][2]int64{{accountA, accountB}, {accountB, accountA}} {
		edges, err := s.store.DirectedEdges(ctx, dir[0], dir[1], "")
		if err != nil {
			return excerrors.Wrap(CodeRiskLimitsInternal, "bilateral credit edges", err)
		}
		expected := cellRemainingTicks(edges, now)
		var a, b uint32
		if dir[0] == accountA {
			a, b = pa, pb
		} else {
			a, b = pb, pa
		}
		if got := s.reader.CreditLimit(a, b); got > expected {
			s.raiseAlert(ctx, SeverityP1, codeCreditDivergence, fmt.Sprintf(
				"matrix cell[%d][%d]=%d exceeds PG remaining %d ticks",
				a, b, got, expected), map[string]string{
				"party_a": fmt.Sprint(a), "party_b": fmt.Sprint(b),
				"cell": fmt.Sprint(got), "expected": fmt.Sprint(expected)})
			return excerrors.New(CodeBilateralCreditExceeded, fmt.Sprintf(
				"bilateral credit state divergent: cell[%d][%d]=%d > remaining %d",
				a, b, got, expected))
		}
	}
	return nil
}

// ReserveMatch performs the atomic two-direction reservation before a
// match commits (§13.8.3). A CreditBreachError maps to
// BILATERAL_CREDIT_EXCEEDED; store/plumbing failures map to
// RISK_LIMITS_INTERNAL. Post-commit the touched edges are checked for
// the 90% utilization band.
func (s *BilateralCreditService) ReserveMatch(ctx context.Context,
	m CreditMatchRequest) error {
	if s.store == nil {
		return excerrors.New(CodeRiskLimitsInternal, "bilateral credit store not configured")
	}
	if m.MakerAccountID == m.TakerAccountID {
		return nil
	}
	pool, err := CreditPoolForInstrumentType(m.InstrumentType)
	if err != nil {
		return excerrors.Wrap(CodeRiskLimitsInternal, "bilateral credit pool", err)
	}
	usd, err := s.toUSD(ctx, m.Notional, m.QuoteCurrency)
	if err != nil {
		return err
	}
	ttl := m.ReservationTTL
	if ttl <= 0 {
		ttl = s.resTTL
	}
	now := s.now()
	res, err := s.store.ReserveMatchTx(ctx, CreditReserveSpec{
		MakerOrderID: m.MakerOrderID, MakerAccountID: m.MakerAccountID,
		TakerOrderID: m.TakerOrderID, TakerAccountID: m.TakerAccountID,
		Pool: pool, Amount: usd, Now: now, ExpiresAt: now.Add(ttl),
	})
	if err != nil {
		var be *CreditBreachError
		if errors.As(err, &be) {
			return s.breachError(be)
		}
		var ve *CreditVersionError
		if errors.As(err, &ve) {
			return s.versionError(ctx, ve)
		}
		return excerrors.Wrap(CodeRiskLimitsInternal, "bilateral credit reserve", err)
	}
	s.checkUtilization(ctx, res)
	return nil
}

// ConsumeFill converts the order's reservation into utilization at fill
// commit — pro-rata for partial fills (notional < reserved credits the
// remainder back). notional is quote-ccy, converted to USD.
func (s *BilateralCreditService) ConsumeFill(ctx context.Context, orderID int64,
	quoteCcy string, notional decimal.Decimal) error {
	if s.store == nil {
		return excerrors.New(CodeRiskLimitsInternal, "bilateral credit store not configured")
	}
	usd, err := s.toUSD(ctx, notional, quoteCcy)
	if err != nil {
		return err
	}
	if err := s.store.ConsumeTx(ctx, orderID, usd); err != nil {
		return excerrors.Wrap(CodeRiskLimitsInternal, "bilateral credit consume", err)
	}
	return nil
}

// ConsumeMatchFill consumes both sides of a committed match atomically.
func (s *BilateralCreditService) ConsumeMatchFill(ctx context.Context,
	makerOrderID, takerOrderID int64, quoteCcy string, notional decimal.Decimal) error {
	if s.store == nil {
		return excerrors.New(CodeRiskLimitsInternal, "bilateral credit store not configured")
	}
	usd, err := s.toUSD(ctx, notional, quoteCcy)
	if err != nil {
		return err
	}
	if err := s.store.ConsumeMatchTx(ctx, makerOrderID, takerOrderID, usd); err != nil {
		return excerrors.Wrap(CodeRiskLimitsInternal, "bilateral credit consume-match", err)
	}
	return nil
}

// ReleaseOrder returns the order's outstanding reservation on
// cancel/reject — idempotent.
func (s *BilateralCreditService) ReleaseOrder(ctx context.Context, orderID int64) error {
	if s.store == nil {
		return excerrors.New(CodeRiskLimitsInternal, "bilateral credit store not configured")
	}
	if err := s.store.ReleaseTx(ctx, orderID); err != nil {
		return excerrors.Wrap(CodeRiskLimitsInternal, "bilateral credit release", err)
	}
	return nil
}

// ReleaseMatch aborts both directions atomically.
func (s *BilateralCreditService) ReleaseMatch(ctx context.Context,
	makerOrderID, takerOrderID int64) error {
	if s.store == nil {
		return excerrors.New(CodeRiskLimitsInternal, "bilateral credit store not configured")
	}
	if err := s.store.ReleaseMatchTx(ctx, makerOrderID, takerOrderID); err != nil {
		return excerrors.Wrap(CodeRiskLimitsInternal, "bilateral credit release-match", err)
	}
	return nil
}

// Sweep expires TTL-dead reservations and releases orphans — wire to the
// periodic risk sweeper (same cadence class as PBCreditService.SweepOrphans).
func (s *BilateralCreditService) Sweep(ctx context.Context) (int64, error) {
	if s.store == nil {
		return 0, excerrors.New(CodeRiskLimitsInternal, "bilateral credit store not configured")
	}
	return s.store.SweepTx(ctx, s.now())
}

// --- admin / config ---------------------------------------------------------

// CreateGroup provisions a grantor-owned group.
func (s *BilateralCreditService) CreateGroup(ctx context.Context,
	g CreditGroup) (CreditGroup, error) {
	if s.store == nil {
		return g, excerrors.New(CodeRiskLimitsInternal, "bilateral credit store not configured")
	}
	out, err := s.store.CreateGroup(ctx, g)
	if err != nil {
		return out, excerrors.Wrap(CodeRiskLimitsInternal, "bilateral credit create group", err)
	}
	return out, nil
}

// SetGroupStatus flips the group lifecycle flag and republishes every
// edge of the group (suspension must reach the core immediately).
func (s *BilateralCreditService) SetGroupStatus(ctx context.Context,
	groupID int64, status string) (CreditGroup, error) {
	if s.store == nil {
		return CreditGroup{}, excerrors.New(CodeRiskLimitsInternal, "bilateral credit store not configured")
	}
	g, err := s.store.SetGroupStatus(ctx, groupID, status)
	if err != nil {
		return g, excerrors.Wrap(CodeRiskLimitsInternal, "bilateral credit group status", err)
	}
	edges, err := s.store.EdgesForGrantor(ctx, g.GrantorAccountID)
	if err != nil {
		return g, excerrors.Wrap(CodeRiskLimitsInternal, "bilateral credit edges", err)
	}
	for _, e := range edges {
		if err := s.publishDelta(ctx, e.GrantorAccountID, e.GranteeAccountID); err != nil {
			return g, err
		}
	}
	return g, nil
}

// CreateRelationship provisions a directed edge (version=1) and publishes
// the new cell.
func (s *BilateralCreditService) CreateRelationship(ctx context.Context,
	r CreditRelationship) (CreditRelationship, error) {
	if s.store == nil {
		return r, excerrors.New(CodeRiskLimitsInternal, "bilateral credit store not configured")
	}
	out, err := s.store.CreateRelationship(ctx, r)
	if err != nil {
		return out, excerrors.Wrap(CodeRiskLimitsInternal, "bilateral credit create rel", err)
	}
	if err := s.publishDelta(ctx, out.GrantorAccountID, out.GranteeAccountID); err != nil {
		return out, err
	}
	return out, nil
}

// UpdateRelationship applies the optimistic-versioned write; a stale
// version is a fail-closed BILATERAL_CREDIT_EXCEEDED + alert. The version
// bump is what the core-side delta is keyed on.
func (s *BilateralCreditService) UpdateRelationship(ctx context.Context,
	r CreditRelationship) (CreditRelationship, error) {
	if s.store == nil {
		return r, excerrors.New(CodeRiskLimitsInternal, "bilateral credit store not configured")
	}
	out, err := s.store.UpdateRelationship(ctx, r)
	if err != nil {
		var ve *CreditVersionError
		if errors.As(err, &ve) {
			return out, s.versionError(ctx, ve)
		}
		return out, excerrors.Wrap(CodeRiskLimitsInternal, "bilateral credit update rel", err)
	}
	if err := s.publishDelta(ctx, out.GrantorAccountID, out.GranteeAccountID); err != nil {
		return out, err
	}
	return out, nil
}

// --- matrix propagation -----------------------------------------------------

// cellRemainingTicks is the conservative pool-agnostic projection of a
// directed pair's relationships onto the single shm cell: MIN remaining
// across ALL extant rows (inactive/blocked rows contribute 0 → one bad
// line can only shrink the cell, never widen it).
func cellRemainingTicks(edges []CreditRelationship, now time.Time) uint64 {
	if len(edges) == 0 {
		return 0
	}
	min := decimal.NewFromInt(math.MaxInt64)
	for _, e := range edges {
		if rem := e.Remaining(now); rem.Cmp(min) < 0 {
			min = rem
		}
	}
	return CreditTicks(min)
}

// publishDelta writes the current directed cell for one (grantor,
// grantee) pair — the versioned delta path. Writer nil → no-op (dev).
// Party indexes are auto-assigned so every credit-relevant account has a
// matrix coordinate; failure to publish pages Risk Manager (a stale cell
// is a divergence the engine would trade against).
func (s *BilateralCreditService) publishDelta(ctx context.Context,
	grantorAccountID, granteeAccountID int64) error {
	if s.writer == nil {
		return nil
	}
	pa, err := s.store.EnsurePartyIndex(ctx, grantorAccountID)
	if err != nil {
		return excerrors.Wrap(CodeRiskLimitsInternal, "bilateral credit party index", err)
	}
	pb, err := s.store.EnsurePartyIndex(ctx, granteeAccountID)
	if err != nil {
		return excerrors.Wrap(CodeRiskLimitsInternal, "bilateral credit party index", err)
	}
	edges, err := s.store.DirectedEdges(ctx, grantorAccountID, granteeAccountID, "")
	if err != nil {
		return excerrors.Wrap(CodeRiskLimitsInternal, "bilateral credit edges", err)
	}
	ticks := cellRemainingTicks(edges, s.now())
	if !s.writer.ApplyUpdate(pa, pb, ticks) {
		s.raiseAlert(ctx, SeverityP1, codeCreditDivergence, fmt.Sprintf(
			"matrix ApplyUpdate failed for cell[%d][%d]", pa, pb), nil)
		return excerrors.New(CodeRiskLimitsInternal, fmt.Sprintf(
			"bilateral credit: matrix update cell[%d][%d] failed", pa, pb))
	}
	return nil
}

// PublishDelta is the exported delta path for external callers that
// mutated relationships directly (admin tooling). Prefer the service
// admin methods which publish automatically.
func (s *BilateralCreditService) PublishDelta(ctx context.Context,
	grantorAccountID, granteeAccountID int64) error {
	if s.store == nil {
		return excerrors.New(CodeRiskLimitsInternal, "bilateral credit store not configured")
	}
	return s.publishDelta(ctx, grantorAccountID, granteeAccountID)
}

// PublishSnapshot rewrites every directed cell from PG — the boot/WAL-
// replay restore path. Rows that lost all activity (blocked, expired,
// closed group) publish 0; pairs that never had a relationship keep the
// ABI zero-init default (no credit). Returns the number of cells written.
func (s *BilateralCreditService) PublishSnapshot(ctx context.Context) (int, error) {
	if s.store == nil {
		return 0, excerrors.New(CodeRiskLimitsInternal, "bilateral credit store not configured")
	}
	if s.writer == nil {
		return 0, excerrors.New(CodeRiskLimitsInternal, "bilateral credit matrix writer not bound")
	}
	parties, err := s.store.Parties(ctx)
	if err != nil {
		return 0, excerrors.Wrap(CodeRiskLimitsInternal, "bilateral credit parties", err)
	}
	edges, err := s.store.AllEdges(ctx)
	if err != nil {
		return 0, excerrors.Wrap(CodeRiskLimitsInternal, "bilateral credit edges", err)
	}
	now := s.now()
	byPair := map[[2]int64][]CreditRelationship{}
	for _, e := range edges {
		k := [2]int64{e.GrantorAccountID, e.GranteeAccountID}
		byPair[k] = append(byPair[k], e)
	}
	cells := 0
	for pair, es := range byPair {
		pa, ok := parties[pair[0]]
		if !ok {
			var err error
			pa, err = s.store.EnsurePartyIndex(ctx, pair[0])
			if err != nil {
				return cells, excerrors.Wrap(CodeRiskLimitsInternal,
					"bilateral credit party index", err)
			}
			parties[pair[0]] = pa
		}
		pb, ok := parties[pair[1]]
		if !ok {
			var err error
			pb, err = s.store.EnsurePartyIndex(ctx, pair[1])
			if err != nil {
				return cells, excerrors.Wrap(CodeRiskLimitsInternal,
					"bilateral credit party index", err)
			}
			parties[pair[1]] = pb
		}
		if !s.writer.ApplyUpdate(pa, pb, cellRemainingTicks(es, now)) {
			s.raiseAlert(ctx, SeverityP1, codeCreditDivergence, fmt.Sprintf(
				"matrix ApplyUpdate failed for cell[%d][%d] during snapshot", pa, pb), nil)
			return cells, excerrors.New(CodeRiskLimitsInternal, fmt.Sprintf(
				"bilateral credit: snapshot cell[%d][%d] failed", pa, pb))
		}
		cells++
	}
	return cells, nil
}

// VerifyMatrix sweeps every directed pair and compares the published
// cell against PG remaining. cell > remaining is the unsafe divergence
// (the core could admit a match PG would reject) → republish the correct
// cell, page Risk Manager, and surface an error. cell < remaining is
// normal engine consumption — the cell is republished to restore
// availability. Returns the divergence count.
func (s *BilateralCreditService) VerifyMatrix(ctx context.Context) (int, error) {
	if s.store == nil {
		return 0, excerrors.New(CodeRiskLimitsInternal, "bilateral credit store not configured")
	}
	if s.reader == nil {
		return 0, excerrors.New(CodeRiskLimitsInternal, "bilateral credit matrix reader not bound")
	}
	parties, err := s.store.Parties(ctx)
	if err != nil {
		return 0, excerrors.Wrap(CodeRiskLimitsInternal, "bilateral credit parties", err)
	}
	edges, err := s.store.AllEdges(ctx)
	if err != nil {
		return 0, excerrors.Wrap(CodeRiskLimitsInternal, "bilateral credit edges", err)
	}
	now := s.now()
	byPair := map[[2]int64][]CreditRelationship{}
	for _, e := range edges {
		k := [2]int64{e.GrantorAccountID, e.GranteeAccountID}
		byPair[k] = append(byPair[k], e)
	}
	divergent := 0
	for pair, es := range byPair {
		pa, okA := parties[pair[0]]
		pb, okB := parties[pair[1]]
		if !okA || !okB {
			s.raiseAlert(ctx, SeverityP1, codeCreditDivergence, fmt.Sprintf(
				"credit edge %d→%d lacks party index — cell unverifiable",
				pair[0], pair[1]), nil)
			divergent++
			continue
		}
		expected := cellRemainingTicks(es, now)
		got := s.reader.CreditLimit(pa, pb)
		if got == expected {
			continue
		}
		if got > expected {
			divergent++
			s.raiseAlert(ctx, SeverityP1, codeCreditDivergence, fmt.Sprintf(
				"matrix cell[%d][%d]=%d exceeds PG remaining %d — republishing",
				pa, pb, got, expected), map[string]string{
				"party_a": fmt.Sprint(pa), "party_b": fmt.Sprint(pb),
				"cell": fmt.Sprint(got), "expected": fmt.Sprint(expected)})
		}
		if s.writer != nil {
			if !s.writer.ApplyUpdate(pa, pb, expected) {
				return divergent, excerrors.New(CodeRiskLimitsInternal, fmt.Sprintf(
					"bilateral credit: divergence repair cell[%d][%d] failed", pa, pb))
			}
		}
	}
	return divergent, nil
}

// Utilization returns the live per-edge view for a grantor — the admin/
// telemetry surface that also fires the 90% alert band.
func (s *BilateralCreditService) Utilization(ctx context.Context,
	grantorAccountID int64) ([]CreditUtilization, error) {
	if s.store == nil {
		return nil, excerrors.New(CodeRiskLimitsInternal, "bilateral credit store not configured")
	}
	edges, err := s.store.EdgesForGrantor(ctx, grantorAccountID)
	if err != nil {
		return nil, excerrors.Wrap(CodeRiskLimitsInternal, "bilateral credit edges", err)
	}
	out := make([]CreditUtilization, 0, len(edges))
	for _, e := range edges {
		u := CreditUtilization{Relationship: e, Pct: e.UtilizationPct()}
		u.Alerted = u.Pct.Cmp(decimal.NewFromInt(CreditUtilizationAlertPct)) >= 0
		out = append(out, u)
	}
	return out, nil
}

// checkUtilization pages the Risk Manager on each edge that just crossed
// the 90% band. Redis NX dedupes one alert per (relationship, bound) per
// 24h window; a nil Redis fires on every crossing (alert is contractual).
func (s *BilateralCreditService) checkUtilization(ctx context.Context,
	res []CreditReservation) {
	if len(res) == 0 {
		return
	}
	for _, r := range res {
		rel, err := s.store.RelationshipByID(ctx, r.RelationshipID)
		if err != nil {
			s.log("utilization re-read rel %d: %v", r.RelationshipID, err)
			continue
		}
		type bound struct {
			name    string
			limit   *decimal.Decimal
			current decimal.Decimal
		}
		for _, b := range []bound{
			{"GROSS", rel.GrossLimit, rel.CurrentGross},
			{"NET", rel.NetLimit, rel.CurrentNet},
		} {
			if b.limit == nil || !b.limit.IsPositive() {
				continue
			}
			pct := b.current.Div(*b.limit).Mul(decimal.NewFromInt(100))
			if pct.Cmp(decimal.NewFromInt(CreditUtilizationAlertPct)) < 0 {
				continue
			}
			if s.rdb != nil {
				ok, err := s.rdb.SetNX(ctx,
					bilateralCreditAlertKey(rel.ID, b.name), "1", 24*time.Hour).Result()
				if err != nil || !ok {
					continue
				}
			}
			s.raiseAlert(ctx, SeverityP1, codeCreditUtilization, fmt.Sprintf(
				"credit edge %d (%d→%d %s) %s utilization %s%% ≥ %d%%",
				rel.ID, rel.GrantorAccountID, rel.GranteeAccountID,
				rel.ProductPool, b.name, pct.String(), CreditUtilizationAlertPct),
				map[string]string{
					"relationship_id": fmt.Sprint(rel.ID),
					"bound":           b.name,
					"pct":             pct.String(),
				})
		}
	}
}

// raiseAlert delivers an ops alert on a bounded fresh context — pager
// outages are logged, never propagated past the caller's coded error.
func (s *BilateralCreditService) raiseAlert(ctx context.Context, severity, code,
	summary string, details map[string]string) {
	if s.alerter == nil {
		s.log("alert %s undeliverable (no alerter): %s", code, summary)
		return
	}
	actx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := s.alerter.Raise(actx, OpsAlert{
		Severity: severity, Code: code, Summary: summary, Details: details,
	}); err != nil {
		s.log("alert %s dispatch failed: %v", code, err)
	}
}
