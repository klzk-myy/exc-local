package recovery

// reconcile.go — Task 4.3.9 daily financial-ledger reconciliation.
//
// The append-only wallet ledger (ledger_entries, migration 102) is the
// immutable source of truth; balances.total (migration 004, GENERATED
// available+locked) is the live cache that MUST equal it. This job
// rebuilds every (account_id, currency) net position from the ledger —
// SUM(DEBIT amount) − SUM(CREDIT amount) — and diffs it against
// balances.total. journal_sums.net_balance (the per-wallet verification
// cache) is cross-checked as a second leg; its drift is recorded in the
// report detail.
//
// On ANY mismatch: ALERT_P1 is raised through the injected alerter seam
// (OrchLogAlerter satisfies the signature — same RaiseP1 contract) and a
// recovery_reports row with stage='daily_reconcile' is persisted. Clean
// runs write no row (the table is the exceptions log, not a heartbeat).
//
// All exported names are Rec-prefixed per the task contract — this package
// is concurrently owned by the orchestrator/archive files (Orch* names).

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/shopspring/decimal"
)

// RecAlerter is the P1 paging seam — signature-compatible with
// OrchAlerter so OrchLogAlerter (and any NATS-backed alerter) plugs in.
type RecAlerter interface {
	RaiseP1(ctx context.Context, summary string, detail map[string]any)
}

// recSlogAlerter is the fallback when the caller passes nil — a
// structured slog P1 line, same convention as OrchLogAlerter.
type recSlogAlerter struct{}

func (recSlogAlerter) RaiseP1(_ context.Context, summary string, detail map[string]any) {
	args := make([]any, 0, len(detail)*2+2)
	args = append(args, "severity", "P1")
	for k, v := range detail {
		args = append(args, k, v)
	}
	slog.Error("ALERT_P1 "+summary, args...)
}

// RecWalletMismatch is one divergent (account_id, currency) pair.
type RecWalletMismatch struct {
	AccountID      int64  `json:"account_id"`
	Currency       string `json:"currency"`
	LedgerNet      string `json:"ledger_net"`       // rebuilt from ledger_entries
	LiveTotal      string `json:"live_total"`       // balances.total
	JournalSumsNet string `json:"journal_sums_net"` // journal_sums.net_balance; "" = no row
	EntryCount     int64  `json:"entry_count"`
	LastEntryID    int64  `json:"last_entry_id"`
	Kind           string `json:"kind"` // amount | no_ledger_stream | no_balance_row
}

// RecReconcileResult is the job's report surface (also embedded in the
// recovery_reports detail on mismatch).
type RecReconcileResult struct {
	WalletsChecked  int                 `json:"wallets_checked"`
	BalancesChecked int                 `json:"balances_checked"`
	LedgerRows      int64               `json:"ledger_rows"`
	Mismatches      []RecWalletMismatch `json:"mismatches,omitempty"`
	ReportRowID     int64               `json:"report_row_id,omitempty"`
}

// RecWalletReconcileDiff runs the scan+diff legs of the wallet
// reconciliation — ledger_entries rebuild vs balances.total, with
// journal_sums cross-checked — and returns the result WITHOUT alerting
// or persisting. Exported for the Phase-13 Task 13.3.2 reconciliation
// engine (BALANCES category), which composes this ledger leg and owns
// its own alert/halt/report pipeline. Read-only; never mutates.
func RecWalletReconcileDiff(ctx context.Context, pool *pgxpool.Pool) (*RecReconcileResult, error) {
	if pool == nil {
		return nil, fmt.Errorf("reconcile: nil pool")
	}
	res := &RecReconcileResult{}

	// --- leg 1: rebuild from the append-only ledger -----------------------
	type walletKey struct {
		AccountID int64
		Currency  string
	}
	type walletState struct {
		net       decimal.Decimal
		entries   int64
		lastEntry int64
	}
	rebuilt := make(map[walletKey]*walletState)
	rows, err := pool.Query(ctx, `
		SELECT account_id, currency,
		       SUM(CASE WHEN direction = 'DEBIT' THEN amount
		                ELSE -amount END) AS net,
		       COUNT(*), MAX(id)
		FROM ledger_entries
		GROUP BY account_id, currency`)
	if err != nil {
		return nil, fmt.Errorf("reconcile: ledger scan: %w", err)
	}
	for rows.Next() {
		var accountID int64
		var ccy string
		var net decimal.Decimal
		var cnt, lastID int64
		if err := rows.Scan(&accountID, &ccy, &net, &cnt, &lastID); err != nil {
			rows.Close()
			return nil, fmt.Errorf("reconcile: ledger row: %w", err)
		}
		rebuilt[walletKey{accountID, ccy}] =
			&walletState{net: net, entries: cnt, lastEntry: lastID}
		res.LedgerRows += cnt
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("reconcile: ledger scan: %w", err)
	}
	res.WalletsChecked = len(rebuilt)

	// --- leg 2: live balances.total ---------------------------------------
	live := make(map[walletKey]decimal.Decimal)
	brows, err := pool.Query(ctx,
		`SELECT account_id, currency, total FROM balances`)
	if err != nil {
		return nil, fmt.Errorf("reconcile: balances scan: %w", err)
	}
	for brows.Next() {
		var accountID int64
		var ccy string
		var total decimal.Decimal
		if err := brows.Scan(&accountID, &ccy, &total); err != nil {
			brows.Close()
			return nil, fmt.Errorf("reconcile: balances row: %w", err)
		}
		live[walletKey{accountID, ccy}] = total
	}
	brows.Close()
	if err := brows.Err(); err != nil {
		return nil, fmt.Errorf("reconcile: balances scan: %w", err)
	}
	res.BalancesChecked = len(live)

	// --- leg 3 (cross-check): journal_sums.net_balance --------------------
	sums := make(map[walletKey]decimal.Decimal)
	srows, err := pool.Query(ctx,
		`SELECT account_id, currency, net_balance FROM journal_sums`)
	if err != nil {
		return nil, fmt.Errorf("reconcile: journal_sums scan: %w", err)
	}
	for srows.Next() {
		var accountID int64
		var ccy string
		var net decimal.Decimal
		if err := srows.Scan(&accountID, &ccy, &net); err != nil {
			srows.Close()
			return nil, fmt.Errorf("reconcile: journal_sums row: %w", err)
		}
		sums[walletKey{accountID, ccy}] = net
	}
	srows.Close()
	if err := srows.Err(); err != nil {
		return nil, fmt.Errorf("reconcile: journal_sums scan: %w", err)
	}

	// --- diff ---------------------------------------------------------------
	// Every ledger-covered wallet must equal balances.total; every live
	// balance row with no ledger stream is suspect unless its total is 0.
	for key, ws := range rebuilt {
		total, ok := live[key]
		if !ok {
			res.Mismatches = append(res.Mismatches, RecWalletMismatch{
				AccountID: key.AccountID, Currency: key.Currency,
				LedgerNet: ws.net.String(), LiveTotal: "",
				EntryCount: ws.entries, LastEntryID: ws.lastEntry,
				Kind: "no_balance_row",
			})
			continue
		}
		sumNet, sumOK := sums[key]
		if !total.Equal(ws.net) || (sumOK && !sumNet.Equal(ws.net)) {
			kind := "amount"
			sn := ""
			if sumOK {
				sn = sumNet.String()
				if !sumNet.Equal(ws.net) && total.Equal(ws.net) {
					kind = "journal_sums_drift" // cache drifted, ledger==live
				}
			}
			res.Mismatches = append(res.Mismatches, RecWalletMismatch{
				AccountID: key.AccountID, Currency: key.Currency,
				LedgerNet: ws.net.String(), LiveTotal: total.String(),
				JournalSumsNet: sn,
				EntryCount:     ws.entries, LastEntryID: ws.lastEntry,
				Kind: kind,
			})
		}
	}
	for key, total := range live {
		if _, ok := rebuilt[key]; !ok && !total.IsZero() {
			res.Mismatches = append(res.Mismatches, RecWalletMismatch{
				AccountID: key.AccountID, Currency: key.Currency,
				LedgerNet: "0", LiveTotal: total.String(),
				Kind: "no_ledger_stream",
			})
		}
	}
	return res, nil
}

// RecReconcile rebuilds wallet balances from ledger_entries and diffs
// them against balances.total. On mismatch it pages P1 and persists a
// recovery_reports row (stage='daily_reconcile',
// outcome='RECONCILE_MISMATCH'). Never mutates balances — reconciliation
// is read-only diagnosis; correction is an operator action (fail-closed
// pessimism: report first, never silently repair money).
func RecReconcile(ctx context.Context, pool *pgxpool.Pool, alerter RecAlerter) (*RecReconcileResult, error) {
	if alerter == nil {
		alerter = recSlogAlerter{}
	}
	res, err := RecWalletReconcileDiff(ctx, pool)
	if err != nil {
		return nil, err
	}
	if len(res.Mismatches) == 0 {
		return res, nil
	}

	// --- fail-loud: P1 + recovery_reports row ------------------------------
	detail := map[string]any{
		"wallets_checked":  res.WalletsChecked,
		"balances_checked": res.BalancesChecked,
		"ledger_rows":      res.LedgerRows,
		"mismatch_count":   len(res.Mismatches),
		"mismatches":       res.Mismatches,
		"runbook":          RecHaltRunbook,
	}
	alerter.RaiseP1(ctx, "ledger reconciliation mismatch", detail)

	detailJSON, err := json.Marshal(detail)
	if err != nil {
		return res, fmt.Errorf("reconcile: detail marshal: %w", err)
	}
	id, err := RecInsertReport(ctx, pool, &RecReport{
		ShardID:           -1, // shard-agnostic job (wallet domain)
		BookSeq:           -1,
		WalTail:           0,
		LastValidSeq:      -1,
		SnapshotSeq:       0,
		FirstDivergentSeq: -1,
		Stage:             RecStageDailyReconcile,
		Outcome:           "RECONCILE_MISMATCH",
		Detail:            detailJSON,
	})
	if err != nil {
		return res, err
	}
	res.ReportRowID = id
	return res, nil
}
