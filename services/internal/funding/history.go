// History read service — Task 5.3.45 (transfer history) + Task 5.3.6
// (funding history). Pure read path: no mutations, no ledger involvement.
//
// Visibility is account-family scoped: a caller sees rows touching any
// account owned by their user_id (master + sub-accounts). The
// sub_account_id filter narrows the perspective; requesting a foreign id
// is FORBIDDEN. Both queries use the spec §8.8 keyset envelope —
// {data, next_cursor, limit, total} on cursor (created_at, id).
package funding

import (
	"context"
	"fmt"
	"time"
)

// HistoryService serves the two paginated journal reads.
type HistoryService struct {
	store Store
}

// NewHistoryService wires the read service.
func NewHistoryService(store Store) *HistoryService {
	return &HistoryService{store: store}
}

// callerAccounts resolves the caller's visible account set (the whole
// user-owned family). UNAUTHORIZED-grade identity problems surface as
// NOT_FOUND/FORBIDDEN before any data is read.
func (s *HistoryService) callerAccounts(ctx context.Context, callerAccountID int64) ([]int64, error) {
	caller, err := s.store.AccountMeta(ctx, callerAccountID)
	if err != nil {
		return nil, err
	}
	return s.store.UserAccountIDs(ctx, caller.UserID)
}

// Transfers implements GET /api/v1/transfers: every transfer touching a
// caller-owned account, filtered by currency / direction (IN|OUT) /
// date range / sub_account_id (the perspective account). Rows carry
// source, destination, amount, journal_entry_id (the GL reference),
// status and actor — the §24 #363 shape.
func (s *HistoryService) Transfers(ctx context.Context, callerAccountID int64,
	f TransferFilter) ([]TransferRow, string, int64, error) {
	ids, err := s.callerAccounts(ctx, callerAccountID)
	if err != nil {
		return nil, "", 0, err
	}
	if f.PerspectiveID != 0 {
		ok := false
		for _, id := range ids {
			if id == f.PerspectiveID {
				ok = true
				break
			}
		}
		if !ok {
			return nil, "", 0, errf("FORBIDDEN",
				"sub_account_id %d is not one of the caller's accounts", f.PerspectiveID)
		}
	}
	if f.Direction != "" && f.Direction != "IN" && f.Direction != "OUT" {
		return nil, "", 0, errCode("INVALID_REQUEST", "direction must be IN or OUT")
	}
	if f.Limit <= 0 || f.Limit > 500 {
		f.Limit = 50
	}
	rows, total, err := s.store.TransferHistory(ctx, ids, f)
	if err != nil {
		return nil, "", 0, err
	}
	var next string
	if len(rows) > f.Limit {
		last := rows[f.Limit-1]
		next = encodeCursor(last.CreatedAt, last.ID)
		rows = rows[:f.Limit]
	}
	return rows, next, total, nil
}

// Funding pages GET /api/v1/funding for the caller's account set —
// deposits, withdrawals and adjustments visible to the family.
func (s *HistoryService) Funding(ctx context.Context, callerAccountID int64,
	f FundingFilter) ([]FundingTxRow, string, int64, error) {
	ids, err := s.callerAccounts(ctx, callerAccountID)
	if err != nil {
		return nil, "", 0, err
	}
	if f.Limit <= 0 || f.Limit > 500 {
		f.Limit = 50
	}
	rows, total, err := s.store.FundingHistory(ctx, ids, f)
	if err != nil {
		return nil, "", 0, err
	}
	var next string
	if len(rows) > f.Limit {
		last := rows[f.Limit-1]
		next = encodeCursor(last.CreatedAt, last.ID)
		rows = rows[:f.Limit]
	}
	return rows, next, total, nil
}

// DecodeCursor exposes the §8.8 cursor codec for handler parsing —
// returns (created_at, id) of the page boundary row.
func DecodeCursor(s string) (time.Time, int64, error) {
	return decodeCursor(s)
}

// DepositReference is the stable account-scoped wire reference clients
// quote on bank deposits (EXC{account}-{CCY}) — the reconciliation key.
func DepositReference(accountID int64, currency string) string {
	return fmt.Sprintf("EXC%08d-%s", accountID, currency)
}
