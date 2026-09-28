// Task 4.3.10/4.3.11 — PostgreSQL-backed audit data provider and
// recovery_reports sink (pgx). Tests use in-memory fakes; this is the
// production wiring consumed by cmd/recovery-orchestrator.
//
// Shard scoping note: the GL (ledger_lines), customer balances, margin
// accounts and nostro books are venue-global — only trades carry a
// shard_id column. The shardID parameter therefore scopes the sharded
// checks (execution chain) while global checks run identically per shard,
// matching the §18.6 model where the venue audit result feeds every
// shard's verdict.
package recovery

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"exchange/pkg/decimal"
)

// OrchPgAudit implements OrchAuditData over the OLTP schema.
type OrchPgAudit struct {
	Pool *pgxpool.Pool
}

// NewOrchPgAudit builds the provider; nil pool is a construction error
// (fail closed — never silently degrade to a stub).
func NewOrchPgAudit(pool *pgxpool.Pool) (*OrchPgAudit, error) {
	if pool == nil {
		return nil, fmt.Errorf("pg audit source: nil pool")
	}
	return &OrchPgAudit{Pool: pool}, nil
}

func (a *OrchPgAudit) GLSums(ctx context.Context, _ int, sinceJournalSeq uint64) ([]OrchGLSum, error) {
	rows, err := a.Pool.Query(ctx, `
		SELECT currency, COALESCE(SUM(debit_amount),0), COALESCE(SUM(credit_amount),0)
		FROM ledger_lines
		WHERE journal_entry_id > $1
		GROUP BY currency`, int64(sinceJournalSeq))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []OrchGLSum
	for rows.Next() {
		var s OrchGLSum
		var d, c decimal.Decimal
		if err := rows.Scan(&s.Currency, &d, &c); err != nil {
			return nil, err
		}
		s.Debits, s.Credits = d, c
		out = append(out, s)
	}
	return out, rows.Err()
}

func (a *OrchPgAudit) BalanceDeltas(ctx context.Context, _ int, sinceJournalSeq uint64) ([]OrchBalanceDelta, error) {
	rows, err := a.Pool.Query(ctx, `
		SELECT account_code, currency, COALESCE(SUM(credit_amount - debit_amount),0)
		FROM ledger_lines
		WHERE journal_entry_id > $1
		GROUP BY account_code, currency`, int64(sinceJournalSeq))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []OrchBalanceDelta
	for rows.Next() {
		var d OrchBalanceDelta
		if err := rows.Scan(&d.AccountCode, &d.Currency, &d.Net); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// BookSeqWatermark returns the persisted book_seq watermark — the newest
// digest checkpoint's book_seq. Live book_seq-vs-wal_tail equivalence is
// checked separately via OrchEngineTelemetry (the C++ engine's IPC seam).
func (a *OrchPgAudit) BookSeqWatermark(ctx context.Context, shardID int) (uint64, error) {
	var v uint64
	err := a.Pool.QueryRow(ctx,
		`SELECT COALESCE(MAX(book_seq),0) FROM recovery_digests WHERE shard_id=$1`,
		int16(shardID)).Scan(&v)
	return v, err
}

func (a *OrchPgAudit) JournalWatermark(ctx context.Context, _ int) (uint64, error) {
	var v uint64
	err := a.Pool.QueryRow(ctx,
		`SELECT COALESCE(MAX(id),0) FROM journal_entries`).Scan(&v)
	return v, err
}

// NostroCoverage: settled customer balances + house equity + insurance ==
// nostro cash, per currency (spec stage 1 second clause).
func (a *OrchPgAudit) NostroCoverage(ctx context.Context, _ int) ([]OrchNostroCoverage, error) {
	rows, err := a.Pool.Query(ctx, `
		WITH cust AS (
			SELECT currency, SUM(total) AS t FROM balances GROUP BY currency
		), ins AS (
			SELECT currency, SUM(balance) AS b FROM insurance_fund GROUP BY currency
		), eq AS (
			SELECT l.currency, SUM(l.credit_amount - l.debit_amount) AS e
			FROM ledger_lines l
			JOIN chart_of_accounts c ON c.account_code = l.account_code
			WHERE c.account_type = 'EQUITY'
			GROUP BY l.currency
		), nos AS (
			SELECT currency, SUM(balance) AS n
			FROM nostro_accounts WHERE status='ACTIVE' GROUP BY currency
		)
		SELECT ccy.currency,
		       COALESCE(cust.t,0) + COALESCE(eq.e,0) + COALESCE(ins.b,0) AS required,
		       COALESCE(nos.n,0) AS nostro
		FROM (
			SELECT currency FROM cust
			UNION SELECT currency FROM ins
			UNION SELECT currency FROM eq
			UNION SELECT currency FROM nos
		) ccy
		LEFT JOIN cust ON cust.currency = ccy.currency
		LEFT JOIN ins  ON ins.currency  = ccy.currency
		LEFT JOIN eq   ON eq.currency   = ccy.currency
		LEFT JOIN nos  ON nos.currency  = ccy.currency`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []OrchNostroCoverage
	for rows.Next() {
		var c OrchNostroCoverage
		if err := rows.Scan(&c.Currency, &c.Required, &c.NostroCash); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func (a *OrchPgAudit) NegativeBalances(ctx context.Context, _ int) ([]OrchNegativeBalance, error) {
	rows, err := a.Pool.Query(ctx,
		`SELECT account_id, currency, total FROM balances WHERE total < 0`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []OrchNegativeBalance
	for rows.Next() {
		var n OrchNegativeBalance
		if err := rows.Scan(&n.AccountID, &n.Currency, &n.Total); err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

// ExecutionChain — spec stage 4: every trade's buy/sell order must exist,
// and per-shard trade_seq must be strictly increasing in commit (id) order.
func (a *OrchPgAudit) ExecutionChain(ctx context.Context, shardID int) ([]uint64, bool, error) {
	orphanRows, err := a.Pool.Query(ctx, `
		SELECT t.id FROM trades t
		LEFT JOIN orders ob ON ob.id = t.buy_order_id
		LEFT JOIN orders os ON os.id = t.sell_order_id
		WHERE (ob.id IS NULL OR os.id IS NULL)
		  AND (t.shard_id = $1 OR (t.shard_id IS NULL AND $1 = 0))`, int16(shardID))
	if err != nil {
		return nil, false, err
	}
	var orphans []uint64
	for orphanRows.Next() {
		var id uint64
		if err := orphanRows.Scan(&id); err != nil {
			orphanRows.Close()
			return nil, false, err
		}
		orphans = append(orphans, id)
	}
	orphanRows.Close()
	if err := orphanRows.Err(); err != nil {
		return nil, false, err
	}

	var violations int64
	err = a.Pool.QueryRow(ctx, `
		SELECT COUNT(*) FROM (
			SELECT trade_seq, LAG(trade_seq) OVER (ORDER BY id) AS prev
			FROM trades WHERE shard_id = $1 AND trade_seq IS NOT NULL
		) s WHERE s.trade_seq <= s.prev`, int16(shardID)).Scan(&violations)
	if err != nil {
		return nil, false, err
	}
	return orphans, violations == 0, nil
}

func (a *OrchPgAudit) MarginViolations(ctx context.Context, _ int) ([]OrchMarginViolation, error) {
	rows, err := a.Pool.Query(ctx, `
		SELECT account_id, equity, used_margin, available_margin
		FROM margin_accounts
		WHERE equity < 0 OR used_margin < 0 OR available_margin < 0`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []OrchMarginViolation
	for rows.Next() {
		var acct uint64
		var eq, um, am decimal.Decimal
		if err := rows.Scan(&acct, &eq, &um, &am); err != nil {
			return nil, err
		}
		if eq.IsNegative() {
			out = append(out, OrchMarginViolation{AccountID: acct, Field: "equity", Value: eq})
		}
		if um.IsNegative() {
			out = append(out, OrchMarginViolation{AccountID: acct, Field: "used_margin", Value: um})
		}
		if am.IsNegative() {
			out = append(out, OrchMarginViolation{AccountID: acct, Field: "available_margin", Value: am})
		}
	}
	return out, rows.Err()
}

func (a *OrchPgAudit) OmnibusMovements(ctx context.Context, _ int) ([]OrchBankMovement, error) {
	rows, err := a.Pool.Query(ctx, `
		SELECT COALESCE(confirmation_ref,''), currency, amount, direction::text
		FROM nostro_movements
		WHERE status <> 'VOID'`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []OrchBankMovement
	for rows.Next() {
		var m OrchBankMovement
		if err := rows.Scan(&m.Ref, &m.Currency, &m.Amount, &m.Direction); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// ---------------------------------------------------------------------------
// recovery_reports sink (migration 065 contract — owned by Task 4.3.9)
// ---------------------------------------------------------------------------

// OrchPgReportSink writes OrchRecoveryReport rows into recovery_reports.
type OrchPgReportSink struct {
	Pool *pgxpool.Pool
}

func NewOrchPgReportSink(pool *pgxpool.Pool) (*OrchPgReportSink, error) {
	if pool == nil {
		return nil, fmt.Errorf("pg report sink: nil pool")
	}
	return &OrchPgReportSink{Pool: pool}, nil
}

func (s *OrchPgReportSink) Record(ctx context.Context, r OrchRecoveryReport) error {
	detail, err := json.Marshal(r.Detail)
	if err != nil {
		return fmt.Errorf("report detail marshal: %w", err)
	}
	_, err = s.Pool.Exec(ctx, `
		INSERT INTO recovery_reports
		    (shard_id, book_seq, wal_tail, last_valid_seq, snapshot_seq,
		     first_divergent_seq, stage, outcome, detail)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)`,
		int16(r.ShardID), r.BookSeq, r.WalTail, r.LastValidSeq,
		r.SnapshotSeq, r.FirstDivergentSeq, r.Stage, r.Outcome, detail)
	if err != nil {
		return fmt.Errorf("recovery_reports insert: %w", err)
	}
	return nil
}

// ---------------------------------------------------------------------------
// recovery_digests store (migration 092 — owned by this task)
// ---------------------------------------------------------------------------

// OrchPgDigestStore persists OrchDigest rows to recovery_digests.
type OrchPgDigestStore struct {
	Pool *pgxpool.Pool
}

func NewOrchPgDigestStore(pool *pgxpool.Pool) (*OrchPgDigestStore, error) {
	if pool == nil {
		return nil, fmt.Errorf("pg digest store: nil pool")
	}
	return &OrchPgDigestStore{Pool: pool}, nil
}

func (s *OrchPgDigestStore) LatestDigest(ctx context.Context, shardID int) (OrchDigest, bool, error) {
	var d OrchDigest
	err := s.Pool.QueryRow(ctx, `
		SELECT shard_id, checkpoint_seq, journal_seq, book_seq,
		       gl_zero_sum_hash, balance_delta_hash, trade_count, created_at
		FROM recovery_digests
		WHERE shard_id = $1
		ORDER BY checkpoint_seq DESC LIMIT 1`, int16(shardID)).
		Scan(&d.ShardID, &d.CheckpointSeq, &d.JournalSeq, &d.BookSeq,
			&d.GLZeroSumHash, &d.BalanceDeltaHash, &d.TradeCount, &d.CreatedAt)
	if err != nil {
		if err == pgx.ErrNoRows {
			return OrchDigest{}, false, nil
		}
		return OrchDigest{}, false, err
	}
	return d, true, nil
}

func (s *OrchPgDigestStore) StoreDigest(ctx context.Context, d OrchDigest) error {
	_, err := s.Pool.Exec(ctx, `
		INSERT INTO recovery_digests
		    (shard_id, checkpoint_seq, journal_seq, book_seq,
		     gl_zero_sum_hash, balance_delta_hash, trade_count)
		VALUES ($1,$2,$3,$4,$5,$6,$7)
		ON CONFLICT (shard_id, checkpoint_seq) DO UPDATE SET
		    journal_seq        = EXCLUDED.journal_seq,
		    book_seq           = EXCLUDED.book_seq,
		    gl_zero_sum_hash   = EXCLUDED.gl_zero_sum_hash,
		    balance_delta_hash = EXCLUDED.balance_delta_hash,
		    trade_count        = EXCLUDED.trade_count`,
		int16(d.ShardID), int64(d.CheckpointSeq), int64(d.JournalSeq),
		int64(d.BookSeq), d.GLZeroSumHash, d.BalanceDeltaHash, int64(d.TradeCount))
	if err != nil {
		return fmt.Errorf("recovery_digests upsert: %w", err)
	}
	return nil
}

func (s *OrchPgDigestStore) DigestsSince(ctx context.Context, shardID int, from uint64) ([]OrchDigest, error) {
	rows, err := s.Pool.Query(ctx, `
		SELECT shard_id, checkpoint_seq, journal_seq, book_seq,
		       gl_zero_sum_hash, balance_delta_hash, trade_count, created_at
		FROM recovery_digests
		WHERE shard_id = $1 AND checkpoint_seq >= $2
		ORDER BY checkpoint_seq`, int16(shardID), int64(from))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []OrchDigest
	for rows.Next() {
		var d OrchDigest
		if err := rows.Scan(&d.ShardID, &d.CheckpointSeq, &d.JournalSeq, &d.BookSeq,
			&d.GLZeroSumHash, &d.BalanceDeltaHash, &d.TradeCount, &d.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}
