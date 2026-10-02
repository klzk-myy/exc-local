package accounts

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"exchange/pkg/decimal"
)

// Sub-account ceiling defaults (Task 5.3.11 item 1; spec §5.2
// accounts.max_sub_accounts). The column is NULLABLE: NULL resolves to
// the tier default below, an explicit value is the admin-set ceiling.
const (
	DefaultSubAccountLimitRetail    = 20   // T0/T1 masters
	DefaultSubAccountLimitCorporate = 100  // T2 masters
	MaxSubAccountLimit              = 1000 // institutional/ECP/PB hard cap
)

// EffectiveSubAccountLimit resolves a master's sub-account ceiling:
// explicit max_sub_accounts wins; NULL falls back to the KYC-tier
// default (T0/T1 → 20, T2 → 100). Admin adjustment is capped at
// MaxSubAccountLimit (1000) by the DB CHECK and re-checked here.
func EffectiveSubAccountLimit(kycTier string, configured *int) int {
	if configured != nil {
		return *configured
	}
	if kycTier == "T2" {
		return DefaultSubAccountLimitCorporate
	}
	return DefaultSubAccountLimitRetail
}

// AccountStatus mirrors account_status_enum (migration 003).
type AccountStatus string

const (
	StatusActive    AccountStatus = "ACTIVE"
	StatusSuspended AccountStatus = "SUSPENDED"
	StatusFrozen    AccountStatus = "FROZEN"
	StatusClosed    AccountStatus = "CLOSED"
)

// SubAccount is one row of the sub-account listing (Task 5.3.11 item 4).
type SubAccount struct {
	ID             int64         `json:"id"`
	MasterID       int64         `json:"master_account_id"`
	AccountType    string        `json:"account_type"`
	KYCTier        string        `json:"kyc_tier"`
	Status         AccountStatus `json:"status"`
	TradingEnabled bool          `json:"trading_enabled"` // status == ACTIVE
	CreatedAt      time.Time     `json:"created_at"`
	Balances       []BalanceLine `json:"balances"`
}

// BalanceLine is a read-model row from balances (never written here —
// spec §5.3 ledger rule).
type BalanceLine struct {
	Currency  string          `json:"currency"`
	Available decimal.Decimal `json:"available"`
	Locked    decimal.Decimal `json:"locked"`
	Total     decimal.Decimal `json:"total"`
}

// FamilyBalances is the master's aggregated view across itself and all
// owned sub-accounts (Task 5.3.11 item 3).
type FamilyBalances struct {
	MasterID   int64                   `json:"master_account_id"`
	AccountIDs []int64                 `json:"account_ids"`
	ByCurrency []BalanceLine           `json:"by_currency"` // summed across the family
	PerAccount map[int64][]BalanceLine `json:"per_account"`
}

// FamilyPosition is one open position in the aggregated family view.
type FamilyPosition struct {
	AccountID    int64           `json:"account_id"`
	InstrumentID int64           `json:"instrument_id"`
	Symbol       string          `json:"symbol"`
	Side         string          `json:"side"`
	Quantity     decimal.Decimal `json:"quantity"`
	EntryPrice   decimal.Decimal `json:"entry_price"`
}

// FamilyView is the master account's aggregate (balances + positions).
type FamilyView struct {
	MasterID  int64            `json:"master_account_id"`
	Balances  FamilyBalances   `json:"balances"`
	Positions []FamilyPosition `json:"positions"`
}

// SubAccountService owns the master→sub-account hierarchy (Task 5.3.11).
// One level deep only: a sub-account can never itself be a master.
type SubAccountService struct {
	pool *pgxpool.Pool
}

// NewSubAccountService builds the service over the OLTP pool.
func NewSubAccountService(pool *pgxpool.Pool) *SubAccountService {
	return &SubAccountService{pool: pool}
}

// Create provisions a sub-account under masterID. The master row is
// locked FOR UPDATE inside the transaction so concurrent creates cannot
// overshoot the ceiling. masterID must itself be a master
// (parent_account_id IS NULL) and ACTIVE.
func (s *SubAccountService) Create(ctx context.Context, masterID int64) (*SubAccount, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, errorf("INTERNAL_ERROR", "begin tx: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var (
		userID     int64
		acctType   string
		kycTier    string
		status     string
		parent     *int64
		configured *int
	)
	err = tx.QueryRow(ctx,
		`SELECT user_id, account_type, kyc_tier, status, parent_account_id, max_sub_accounts
		   FROM accounts WHERE id = $1 FOR UPDATE`, masterID).
		Scan(&userID, &acctType, &kycTier, &status, &parent, &configured)
	if err == pgx.ErrNoRows {
		return nil, errorf(CodeNotFound, "master account %d not found", masterID)
	}
	if err != nil {
		return nil, errorf("INTERNAL_ERROR", "read master account: %v", err)
	}
	if parent != nil {
		return nil, newError(CodeForbidden,
			"sub-accounts cannot own sub-accounts (hierarchy is one level deep)")
	}
	if status != string(StatusActive) {
		return nil, errorf(statusCode(status), "master account %d is %s", masterID, status)
	}

	var activeCount int
	if err := tx.QueryRow(ctx,
		`SELECT count(*) FROM accounts
		  WHERE parent_account_id = $1 AND status <> 'CLOSED'`, masterID).
		Scan(&activeCount); err != nil {
		return nil, errorf("INTERNAL_ERROR", "count sub-accounts: %v", err)
	}
	limit := EffectiveSubAccountLimit(kycTier, configured)
	if activeCount >= limit {
		return nil, errorf(CodeForbidden,
			"sub-account ceiling reached: %d active of max_sub_accounts=%d", activeCount, limit)
	}

	var id int64
	var createdAt time.Time
	if err := tx.QueryRow(ctx,
		`INSERT INTO accounts (user_id, account_type, kyc_tier, parent_account_id, fee_tier_id)
		 VALUES ($1, $2, $3, $4, COALESCE(
		     (SELECT fee_tier_id FROM accounts WHERE id=$4),
		     (SELECT id FROM fee_tiers WHERE tier_name='STANDARD'),
		     (SELECT id FROM fee_tiers ORDER BY id LIMIT 1)))
		 RETURNING id, created_at`,
		userID, acctType, kycTier, masterID).Scan(&id, &createdAt); err != nil {
		return nil, errorf("INTERNAL_ERROR", "create sub-account: %v", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, errorf("INTERNAL_ERROR", "commit sub-account: %v", err)
	}

	return &SubAccount{
		ID:             id,
		MasterID:       masterID,
		AccountType:    acctType,
		KYCTier:        kycTier,
		Status:         StatusActive,
		TradingEnabled: true,
		CreatedAt:      createdAt,
		Balances:       []BalanceLine{},
	}, nil
}

// List returns all non-CLOSED sub-accounts of masterID with balances,
// status and trading permissions (Task 5.3.11 item 4). Masters see only
// their own children — callers must pass an owned master id.
func (s *SubAccountService) List(ctx context.Context, masterID int64) ([]SubAccount, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT id, account_type, kyc_tier, status, created_at
		   FROM accounts
		  WHERE parent_account_id = $1 AND status <> 'CLOSED'
		  ORDER BY id`, masterID)
	if err != nil {
		return nil, errorf("INTERNAL_ERROR", "list sub-accounts: %v", err)
	}
	defer rows.Close()

	out := []SubAccount{}
	ids := []int64{}
	for rows.Next() {
		var sa SubAccount
		sa.MasterID = masterID
		if err := rows.Scan(&sa.ID, &sa.AccountType, &sa.KYCTier, &sa.Status, &sa.CreatedAt); err != nil {
			return nil, errorf("INTERNAL_ERROR", "scan sub-account: %v", err)
		}
		sa.TradingEnabled = sa.Status == StatusActive
		sa.Balances = []BalanceLine{}
		out = append(out, sa)
		ids = append(ids, sa.ID)
	}
	if err := rows.Err(); err != nil {
		return nil, errorf("INTERNAL_ERROR", "list sub-accounts: %v", err)
	}
	if len(ids) == 0 {
		return out, nil
	}

	bal, err := s.balancesFor(ctx, ids)
	if err != nil {
		return nil, err
	}
	for i := range out {
		if b, ok := bal[out[i].ID]; ok {
			out[i].Balances = b
		}
	}
	return out, nil
}

// Aggregate returns the family view a master sees (Task 5.3.11 item 3):
// per-currency balance sums across the master and every owned
// sub-account, plus the family's open positions.
func (s *SubAccountService) Aggregate(ctx context.Context, masterID int64) (*FamilyView, error) {
	ids, err := s.familyIDs(ctx, masterID)
	if err != nil {
		return nil, err
	}

	perAccount, err := s.balancesFor(ctx, ids)
	if err != nil {
		return nil, err
	}
	totals := map[string]*BalanceLine{}
	order := []string{}
	for _, id := range ids {
		for _, b := range perAccount[id] {
			t, ok := totals[b.Currency]
			if !ok {
				cpy := BalanceLine{Currency: b.Currency}
				t = &cpy
				totals[b.Currency] = t
				order = append(order, b.Currency)
			}
			t.Available = t.Available.Add(b.Available)
			t.Locked = t.Locked.Add(b.Locked)
			t.Total = t.Total.Add(b.Total)
		}
	}
	byCurrency := make([]BalanceLine, 0, len(order))
	for _, c := range order {
		byCurrency = append(byCurrency, *totals[c])
	}

	positions, err := s.familyPositions(ctx, ids)
	if err != nil {
		return nil, err
	}

	return &FamilyView{
		MasterID: masterID,
		Balances: FamilyBalances{
			MasterID:   masterID,
			AccountIDs: ids,
			ByCurrency: byCurrency,
			PerAccount: perAccount,
		},
		Positions: positions,
	}, nil
}

// IsSubAccountOf reports whether candidateID is a direct, non-CLOSED
// sub-account of masterID — the ownership check used by the API-key and
// transfer paths.
func (s *SubAccountService) IsSubAccountOf(ctx context.Context, masterID, candidateID int64) (bool, error) {
	var ok bool
	if err := s.pool.QueryRow(ctx,
		`SELECT EXISTS(
		   SELECT 1 FROM accounts
		    WHERE id = $1 AND parent_account_id = $2 AND status <> 'CLOSED')`,
		candidateID, masterID).Scan(&ok); err != nil {
		return false, errorf("INTERNAL_ERROR", "sub-account ownership check: %v", err)
	}
	return ok, nil
}

// SetLimit adjusts a master account's ceiling (Task 5.3.11 item 6 —
// Risk Manager / Super Admin via the admin endpoint; role enforcement is
// the Phase-07 stub boundary). limit is capped at MaxSubAccountLimit.
func (s *SubAccountService) SetLimit(ctx context.Context, accountID int64, limit int) error {
	if limit < 0 || limit > MaxSubAccountLimit {
		return errorf(CodeInvalidRequest,
			"sub-account limit %d outside 0..%d", limit, MaxSubAccountLimit)
	}
	tag, err := s.pool.Exec(ctx,
		`UPDATE accounts SET max_sub_accounts = $2, updated_at = now()
		  WHERE id = $1 AND parent_account_id IS NULL AND status <> 'CLOSED'`,
		accountID, limit)
	if err != nil {
		return errorf("INTERNAL_ERROR", "set sub-account limit: %v", err)
	}
	if tag.RowsAffected() == 0 {
		return errorf(CodeNotFound, "master account %d not found", accountID)
	}
	return nil
}

// familyIDs returns [masterID, ...sub-account ids] for the family.
// masterID is verified to exist and be a master (fail-closed).
func (s *SubAccountService) familyIDs(ctx context.Context, masterID int64) ([]int64, error) {
	var isMaster bool
	if err := s.pool.QueryRow(ctx,
		`SELECT parent_account_id IS NULL FROM accounts WHERE id = $1`, masterID).
		Scan(&isMaster); err == pgx.ErrNoRows {
		return nil, errorf(CodeNotFound, "account %d not found", masterID)
	} else if err != nil {
		return nil, errorf("INTERNAL_ERROR", "read account %d: %v", masterID, err)
	}
	if !isMaster {
		return nil, newError(CodeForbidden, "aggregate view is master-only")
	}
	rows, err := s.pool.Query(ctx,
		`SELECT id FROM accounts
		  WHERE (id = $1 OR parent_account_id = $1) AND status <> 'CLOSED'
		  ORDER BY id`, masterID)
	if err != nil {
		return nil, errorf("INTERNAL_ERROR", "family ids: %v", err)
	}
	defer rows.Close()
	ids := []int64{}
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, errorf("INTERNAL_ERROR", "family ids scan: %v", err)
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// balancesFor returns per-account balance lines for the given ids.
func (s *SubAccountService) balancesFor(ctx context.Context, ids []int64) (map[int64][]BalanceLine, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT account_id, currency, available, locked, total
		   FROM balances WHERE account_id = ANY($1)
		  ORDER BY account_id, currency`, ids)
	if err != nil {
		return nil, errorf("INTERNAL_ERROR", "read balances: %v", err)
	}
	defer rows.Close()
	out := map[int64][]BalanceLine{}
	for rows.Next() {
		var id int64
		var b BalanceLine
		if err := rows.Scan(&id, &b.Currency, &b.Available, &b.Locked, &b.Total); err != nil {
			return nil, errorf("INTERNAL_ERROR", "scan balance: %v", err)
		}
		out[id] = append(out[id], b)
	}
	return out, rows.Err()
}

// familyPositions lists open (quantity <> 0) positions across the family.
func (s *SubAccountService) familyPositions(ctx context.Context, ids []int64) ([]FamilyPosition, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT p.account_id, p.instrument_id, i.symbol, p.side::text, p.quantity, p.entry_price
		   FROM positions p JOIN instruments i ON i.id = p.instrument_id
		  WHERE p.account_id = ANY($1) AND p.quantity <> 0
		  ORDER BY p.account_id, p.id`, ids)
	if err != nil {
		return nil, errorf("INTERNAL_ERROR", "read family positions: %v", err)
	}
	defer rows.Close()
	out := []FamilyPosition{}
	for rows.Next() {
		var p FamilyPosition
		if err := rows.Scan(&p.AccountID, &p.InstrumentID, &p.Symbol, &p.Side, &p.Quantity, &p.EntryPrice); err != nil {
			return nil, errorf("INTERNAL_ERROR", "scan position: %v", err)
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// statusCode maps a stored account status to the code a mutation should
// reject with (FROZEN is a dedicated §23 code; the rest are generic).
func statusCode(status string) string {
	switch AccountStatus(status) {
	case StatusFrozen:
		return CodeAccountFrozen
	case StatusClosed:
		return CodeNotFound
	default:
		return CodeForbidden
	}
}
