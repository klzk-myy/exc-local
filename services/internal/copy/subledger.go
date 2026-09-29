package copy

import (
	"context"

	"exchange/internal/pamm"
)

// PammSubledgerAdapter writes copy-layer fee rows into the shared
// pamm_subledger_entries table (copy_follow_id set, pool_id NULL) via the
// PAMM store — one taxonomy, one table, one replay discipline.
type PammSubledgerAdapter struct {
	Store *pamm.PgxStore
}

// NewSubledgerWriter binds the production sub-ledger writer — nil store
// fails closed.
func NewSubledgerWriter(store *pamm.PgxStore) (*PammSubledgerAdapter, error) {
	if store == nil {
		return nil, errorf(CodeInternalError, "subledger writer requires a pamm store")
	}
	return &PammSubledgerAdapter{Store: store}, nil
}

// InsertSubledger implements SubledgerWriter.
func (a *PammSubledgerAdapter) InsertSubledger(ctx context.Context, rows []SubledgerRow) error {
	entries := make([]pamm.SubledgerEntry, len(rows))
	for i, r := range rows {
		fid := r.CopyFollowID
		entries[i] = pamm.SubledgerEntry{
			TxnType:        pamm.TxnType(r.TxnType),
			CopyFollowID:   &fid,
			AccountID:      r.AccountID,
			Currency:       r.Currency,
			Direction:      r.Direction,
			Amount:         r.Amount,
			JournalEntryID: r.JournalEntryID,
			ReferenceID:    r.ReferenceID,
			Narrative:      r.Narrative,
		}
	}
	return a.Store.InsertSubledger(ctx, entries)
}
