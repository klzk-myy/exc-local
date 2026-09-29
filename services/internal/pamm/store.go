package pamm

import (
	"context"
	stderrors "errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"exchange/pkg/decimal"
)

// Store is the engine's persistence seam. PgxStore is production; unit
// tests substitute fakes. All reads that gate money movement take the
// rows FOR UPDATE inside their caller's transaction where noted.
type Store interface {
	// AccountMeta resolves user_id/kyc/status/parent for an account.
	AccountMeta(ctx context.Context, accountID int64) (*AccountMeta, error)
	// CreatePool atomically creates the pool sub-account + pamm_pools row.
	CreatePool(ctx context.Context, managerAccountID int64, name, currency string,
		minInvestment decimal.Decimal) (*Pool, error)
	// PoolByID / PoolByAccountID resolve pool rows.
	PoolByID(ctx context.Context, poolID int64) (*Pool, error)
	PoolByAccountID(ctx context.Context, poolAccountID int64) (*Pool, error)
	// AllocationForUpdate locks and returns the investor's allocation row
	// (nil when none exists).
	AllocationForUpdate(ctx context.Context, poolID, investorAccountID int64) (*Allocation, error)
	// BumpInvested applies a signed delta to an allocation's invested
	// capital (creating the row on first invest). The DEC(28,8) CHECK
	// rejects a redeem that would drive invested negative — fail closed.
	BumpInvested(ctx context.Context, poolID, investorAccountID int64,
		currency string, delta decimal.Decimal) (*Allocation, error)
	// InsertSubledger appends sub-ledger rows; the idempotency_key UNIQUE
	// index makes keyed replays resolve without double-posting.
	InsertSubledger(ctx context.Context, entries []SubledgerEntry) error
	// SubledgerByKey resolves a prior keyed movement (replay path).
	SubledgerByKey(ctx context.Context, key string) (*SubledgerEntry, error)
	// ActiveAllocations returns ACTIVE allocations for a pool in stable
	// allocation_id order — the pro-rata weight set.
	ActiveAllocations(ctx context.Context, poolID int64) ([]Allocation, error)
	// InsertFillAllocations writes pro-rata fill rows; UNIQUE
	// (master_trade_id, allocation_id) dedups replays. Returns rows applied.
	InsertFillAllocations(ctx context.Context, rows []FillAllocation) (int, error)
	// PoolAccountIDs lists ACTIVE pool account ids — the engine's
	// master-account filter.
	PoolAccountIDs(ctx context.Context) ([]int64, error)
}

// AccountMeta is the accounts-row fields the engine needs.
type AccountMeta struct {
	ID        int64
	UserID    int64
	AcctType  string
	KycTier   string
	Status    string
	ParentID  *int64
	CreatedAt time.Time
}

// PgxStore is the production Store over pgx.
type PgxStore struct {
	pool *pgxpool.Pool
}

// NewPgxStore binds the OLTP pool.
func NewPgxStore(pool *pgxpool.Pool) (*PgxStore, error) {
	if pool == nil {
		return nil, fmt.Errorf("pamm: store requires a pgx pool (fail closed)")
	}
	return &PgxStore{pool: pool}, nil
}

// AccountMeta implements Store.
func (s *PgxStore) AccountMeta(ctx context.Context, accountID int64) (*AccountMeta, error) {
	var m AccountMeta
	err := s.pool.QueryRow(ctx, `
		SELECT id, user_id, account_type::text, kyc_tier::text, status::text,
		       parent_account_id, created_at
		  FROM accounts WHERE id = $1`, accountID).
		Scan(&m.ID, &m.UserID, &m.AcctType, &m.KycTier, &m.Status, &m.ParentID, &m.CreatedAt)
	if err == pgx.ErrNoRows {
		return nil, errorf(CodeNotFound, "account %d not found", accountID)
	}
	if err != nil {
		return nil, errorf(CodeInternalError, "read account %d: %v", accountID, err)
	}
	return &m, nil
}

// CreatePool implements Store — manager row locked, pool sub-account and
// pamm_pools row inserted atomically.
func (s *PgxStore) CreatePool(ctx context.Context, managerAccountID int64,
	name, currency string, minInvestment decimal.Decimal) (*Pool, error) {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return nil, errorf(CodeInternalError, "begin tx: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	mgr, err := accountMetaTx(ctx, tx, managerAccountID, true)
	if err != nil {
		return nil, err
	}
	if mgr.Status != "ACTIVE" {
		return nil, errorf(CodeForbidden,
			"manager account %d is %s — pools require an ACTIVE manager",
			managerAccountID, mgr.Status)
	}
	if mgr.ParentID != nil {
		return nil, errorf(CodeForbidden,
			"pool manager %d is itself a sub-account (one level only)", managerAccountID)
	}

	var poolAcctID int64
	if err := tx.QueryRow(ctx, `
		INSERT INTO accounts (user_id, account_type, kyc_tier, parent_account_id)
		VALUES ($1, 'MARGIN', $2, $3) RETURNING id`,
		mgr.UserID, mgr.KycTier, managerAccountID).Scan(&poolAcctID); err != nil {
		return nil, errorf(CodeInternalError, "create pool account: %v", err)
	}

	var p Pool
	err = tx.QueryRow(ctx, `
		INSERT INTO pamm_pools
		    (manager_account_id, pool_account_id, name, currency, min_investment)
		VALUES ($1, $2, $3, $4, $5)
		RETURNING pool_id, created_at`,
		managerAccountID, poolAcctID, name, currency, minInvestment.String()).
		Scan(&p.PoolID, &p.CreatedAt)
	if err != nil {
		return nil, errorf(CodeInternalError, "create pool: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, errorf(CodeInternalError, "commit pool: %v", err)
	}
	p.ManagerAccountID = managerAccountID
	p.PoolAccountID = poolAcctID
	p.Name, p.Currency, p.MinInvestment, p.Status = name, currency, minInvestment, PoolActive
	return &p, nil
}

func accountMetaTx(ctx context.Context, tx pgx.Tx, accountID int64, forUpdate bool) (*AccountMeta, error) {
	q := `SELECT id, user_id, account_type::text, kyc_tier::text, status::text,
	             parent_account_id, created_at
	        FROM accounts WHERE id = $1`
	if forUpdate {
		q += " FOR UPDATE"
	}
	var m AccountMeta
	err := tx.QueryRow(ctx, q, accountID).
		Scan(&m.ID, &m.UserID, &m.AcctType, &m.KycTier, &m.Status, &m.ParentID, &m.CreatedAt)
	if err == pgx.ErrNoRows {
		return nil, errorf(CodeNotFound, "account %d not found", accountID)
	}
	if err != nil {
		return nil, errorf(CodeInternalError, "read account %d: %v", accountID, err)
	}
	return &m, nil
}

func scanPool(row pgx.Row) (*Pool, error) {
	var p Pool
	var min string
	err := row.Scan(&p.PoolID, &p.ManagerAccountID, &p.PoolAccountID, &p.Name,
		&p.Currency, &min, &p.Status, &p.CreatedAt)
	if err == pgx.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	d, err := decimal.NewFromString(min)
	if err != nil {
		return nil, err
	}
	p.MinInvestment = d
	return &p, nil
}

const poolCols = `pool_id, manager_account_id, pool_account_id, name, currency,
                  min_investment::text, status::text, created_at`

// PoolByID implements Store; nil,nil when absent.
func (s *PgxStore) PoolByID(ctx context.Context, poolID int64) (*Pool, error) {
	p, err := scanPool(s.pool.QueryRow(ctx,
		`SELECT `+poolCols+` FROM pamm_pools WHERE pool_id = $1`, poolID))
	if err != nil {
		return nil, errorf(CodeInternalError, "read pool %d: %v", poolID, err)
	}
	return p, nil
}

// PoolByAccountID implements Store — resolves a pool by its pool account.
func (s *PgxStore) PoolByAccountID(ctx context.Context, poolAccountID int64) (*Pool, error) {
	p, err := scanPool(s.pool.QueryRow(ctx,
		`SELECT `+poolCols+` FROM pamm_pools WHERE pool_account_id = $1`, poolAccountID))
	if err != nil {
		return nil, errorf(CodeInternalError, "read pool by account %d: %v", poolAccountID, err)
	}
	return p, nil
}

// AllocationForUpdate implements Store — locks the row when present.
func (s *PgxStore) AllocationForUpdate(ctx context.Context, poolID, investorAccountID int64) (*Allocation, error) {
	var a Allocation
	var invested string
	err := s.pool.QueryRow(ctx, `
		SELECT allocation_id, pool_id, investor_account_id, currency,
		       invested::text, status, created_at
		  FROM pamm_allocations
		 WHERE pool_id = $1 AND investor_account_id = $2
		 FOR UPDATE`, poolID, investorAccountID).
		Scan(&a.AllocationID, &a.PoolID, &a.InvestorAccountID, &a.Currency,
			&invested, &a.Status, &a.CreatedAt)
	if err == pgx.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, errorf(CodeInternalError, "read allocation: %v", err)
	}
	a.Invested = decimal.RequireFromString(invested)
	return &a, nil
}

// BumpInvested implements Store — UPDATE-then-INSERT (an upsert whose
// INSERT candidate carried the negative delta would trip the CHECK
// before conflict arbitration, so the delta is applied in the UPDATE
// predicate instead; the WHERE guard turns over-redemption into a
// coded INSUFFICIENT_BALANCE, and the CHECK still backs the invariant).
func (s *PgxStore) BumpInvested(ctx context.Context, poolID, investorAccountID int64,
	currency string, delta decimal.Decimal) (*Allocation, error) {
	var a Allocation
	var invested string
	err := s.pool.QueryRow(ctx, `
		UPDATE pamm_allocations
		   SET invested = invested + $3::numeric, updated_at = now()
		 WHERE pool_id = $1 AND investor_account_id = $2
		   AND invested + $3::numeric >= 0
		RETURNING allocation_id, pool_id, investor_account_id, currency,
		          invested::text, status, created_at`,
		poolID, investorAccountID, delta.String()).
		Scan(&a.AllocationID, &a.PoolID, &a.InvestorAccountID, &a.Currency,
			&invested, &a.Status, &a.CreatedAt)
	if err == pgx.ErrNoRows {
		if delta.IsNegative() {
			return nil, errorf(CodeInsufficientBalance,
				"redeem exceeds invested capital for investor %d in pool %d",
				investorAccountID, poolID)
		}
		err = s.pool.QueryRow(ctx, `
			INSERT INTO pamm_allocations (pool_id, investor_account_id, currency, invested)
			VALUES ($1, $2, $3, $4::numeric)
			RETURNING allocation_id, pool_id, investor_account_id, currency,
			          invested::text, status, created_at`,
			poolID, investorAccountID, currency, delta.String()).
			Scan(&a.AllocationID, &a.PoolID, &a.InvestorAccountID, &a.Currency,
				&invested, &a.Status, &a.CreatedAt)
	}
	if err != nil {
		return nil, errorf(CodeInternalError, "bump invested: %v", err)
	}
	a.Invested = decimal.RequireFromString(invested)
	return &a, nil
}

// InsertSubledger implements Store.
func (s *PgxStore) InsertSubledger(ctx context.Context, entries []SubledgerEntry) error {
	for _, e := range entries {
		if _, err := s.pool.Exec(ctx, `
			INSERT INTO pamm_subledger_entries
			    (txn_type, pool_id, copy_follow_id, account_id, currency,
			     direction, amount, journal_entry_id, reference_id, narrative,
			     idempotency_key)
			VALUES ($1,$2,$3,$4,$5,$6,$7::numeric,$8,NULLIF($9::bigint,0),NULLIF($10,''),NULLIF($11,''))`,
			string(e.TxnType), e.PoolID, e.CopyFollowID, e.AccountID, e.Currency,
			e.Direction, e.Amount.String(), e.JournalEntryID, e.ReferenceID,
			e.Narrative, e.IdempotencyKey); err != nil {
			if isUniqueViolation(err) {
				return errIdempotentReplay
			}
			return errorf(CodeInternalError, "insert subledger %s: %v", e.TxnType, err)
		}
	}
	return nil
}

// SubledgerByKey implements Store.
func (s *PgxStore) SubledgerByKey(ctx context.Context, key string) (*SubledgerEntry, error) {
	var e SubledgerEntry
	var amt string
	err := s.pool.QueryRow(ctx, `
		SELECT txn_type::text, pool_id, copy_follow_id, account_id, currency,
		       direction, amount::text, journal_entry_id, reference_id, narrative
		  FROM pamm_subledger_entries WHERE idempotency_key = $1`, key).
		Scan(&e.TxnType, &e.PoolID, &e.CopyFollowID, &e.AccountID, &e.Currency,
			&e.Direction, &amt, &e.JournalEntryID, &e.ReferenceID, &e.Narrative)
	if err == pgx.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, errorf(CodeInternalError, "subledger by key: %v", err)
	}
	e.Amount = decimal.RequireFromString(amt)
	return &e, nil
}

// ActiveAllocations implements Store — stable allocation_id order so the
// pro-rata tiebreak is deterministic.
func (s *PgxStore) ActiveAllocations(ctx context.Context, poolID int64) ([]Allocation, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT allocation_id, pool_id, investor_account_id, currency,
		       invested::text, status, created_at
		  FROM pamm_allocations
		 WHERE pool_id = $1 AND status = 'ACTIVE' AND invested > 0
		 ORDER BY allocation_id`, poolID)
	if err != nil {
		return nil, errorf(CodeInternalError, "list allocations: %v", err)
	}
	defer rows.Close()
	out := []Allocation{}
	for rows.Next() {
		var a Allocation
		var invested string
		if err := rows.Scan(&a.AllocationID, &a.PoolID, &a.InvestorAccountID,
			&a.Currency, &invested, &a.Status, &a.CreatedAt); err != nil {
			return nil, errorf(CodeInternalError, "scan allocation: %v", err)
		}
		a.Invested = decimal.RequireFromString(invested)
		out = append(out, a)
	}
	return out, rows.Err()
}

// InsertFillAllocations implements Store — ON CONFLICT dedups replays.
func (s *PgxStore) InsertFillAllocations(ctx context.Context, rowsIn []FillAllocation) (int, error) {
	applied := 0
	for _, r := range rowsIn {
		tag, err := s.pool.Exec(ctx, `
			INSERT INTO pamm_fill_allocations
			    (pool_id, master_trade_id, allocation_id, investor_account_id,
			     instrument_id, side, quantity, price)
			VALUES ($1,$2,$3,$4,$5,$6,$7::numeric,$8::numeric)
			ON CONFLICT (master_trade_id, allocation_id) DO NOTHING`,
			r.PoolID, r.MasterTradeID, r.AllocationID, r.InvestorAccountID,
			r.InstrumentID, r.Side, r.Quantity.String(), r.Price.String())
		if err != nil {
			return applied, errorf(CodeInternalError, "insert fill allocation: %v", err)
		}
		applied += int(tag.RowsAffected())
	}
	return applied, nil
}

// PoolAccountIDs implements Store.
func (s *PgxStore) PoolAccountIDs(ctx context.Context) ([]int64, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT pool_account_id FROM pamm_pools WHERE status = 'ACTIVE' ORDER BY pool_account_id`)
	if err != nil {
		return nil, errorf(CodeInternalError, "list pool accounts: %v", err)
	}
	defer rows.Close()
	out := []int64{}
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, errorf(CodeInternalError, "scan pool account: %v", err)
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// ---------------------------------------------------------------------------
// shared error helpers
// ---------------------------------------------------------------------------

// errIdempotentReplay marks a subledger idempotency_key collision —
// resolved by the service layer into a replayed result.
var errIdempotentReplay = fmt.Errorf("pamm: subledger idempotency replay")

func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return stderrors.As(err, &pgErr) && pgErr.Code == "23505"
}
