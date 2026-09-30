// Task 20.3.12 — per-account income ledger (spec §16.7, §24 #361).
//
// The ClickHouse `income_ledger` table is sibling-owned
// (deploy/clickhouse/schema/007_income_ledger.sql, database
// exchange_analytics):
//
//	income_ledger (posted_at DateTime64(3), account_id UInt64,
//	    currency FixedString(3), income_type LowCardinality(String),
//	    entry_type LowCardinality(String), amount Decimal(38,8) signed,
//	    ledger_entry_id UInt64, journal_entry_id UInt64,
//	    reference_id UInt64, description String, ver UInt64)
//	  ENGINE = ReplacingMergeTree(ver)
//	  ORDER BY (account_id, currency, ledger_entry_id)
//
// NOTE — schema drift vs the task text: the landed DDL carries
// `income_type`/`reference_id`/`description` and has NO dedicated `symbol`
// column. The `?symbol=` filter is therefore served by a derived-symbol
// read projection (narrative extraction + bounded `trades` join); see
// incomeQuerySQL. PG (ledger_entries ⋈ journal_entries ⋈ ledger_lines)
// remains the book of record for disputes.
//
// Write path: Task 20.3.1's Ingester.PollIncomeOnce (etl.go) projects
// every ledger_entries row verbatim with a coarse entry_type map
// (FEE→COMMISSION etc.). IncomeSync below is the REFINED §16.7
// projection: it classifies through the journal's GL ledger_lines account
// codes + idempotency-key/description markers, so REBATE, DUST_CONVERT
// and FUNDING_FEE — which share the FEE/TRANSFER entry types — land as
// distinct income types (see ClassifyIncome). Operations should run one
// income writer; when both run, ReplacingMergeTree(ver) keeps the newest
// version per (account_id, currency, ledger_entry_id) so re-syncs stay
// idempotent but the two classifiers flip the income_type of refined
// rows. Follow-up: point Ingester.PollIncomeOnce at ClassifyIncome and
// retire mapIncomeType (etl.go is outside this task's write scope).
//
// Read path (handler): ClickHouse only — a dead cluster answers
// SERVICE_DEGRADED (fail-closed, spec §2.7); PG is never silently
// substituted for the projection.
package analytics

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"exchange/pkg/decimal"
)

// IncomeTable is the exchange_analytics projection name — the single
// constant every writer/reader shares (drift hatch for the sibling DDL).
const IncomeTable = "income_ledger"

// TradesTable is the enriched trade-fill projection (schema 002) the
// income read joins when a journal's symbol only exists at the trade
// reference (commission/rebate narratives carry trade=NNN, not a pair).
const TradesTable = "trades"

// incomeColumns is the insert tuple — same column order as the
// tableColumns["income_ledger"] registry in etl.go.
const incomeColumns = "posted_at, account_id, currency, income_type, " +
	"entry_type, amount, ledger_entry_id, journal_entry_id, " +
	"reference_id, description, ver"

// ---------------------------------------------------------------------------
// §16.7 income taxonomy
// ---------------------------------------------------------------------------

// Canonical income types required by §16.7 / Task 20.3.12.
const (
	IncomeCommission   = "COMMISSION"
	IncomeSwapRollover = "SWAP_ROLLOVER"
	IncomeRebate       = "REBATE"
	IncomeNBPAdjust    = "NBP_ADJUSTMENT"
	IncomeDustConvert  = "DUST_CONVERT"
	IncomeFundingFee   = "FUNDING_FEE"
)

// Extended taxonomy — emitted by ClassifyIncome for revenue/expense
// postings outside the six §16.7 names so the ledger stays complete and
// every client-visible wallet movement is queryable by a stable type
// token (ledger_entry_type_enum has no dedicated values for these).
const (
	IncomeTradingFee       = "TRADING_FEE"         // 4010 spread-markup trade fee
	IncomeConversionFee    = "CONVERSION_FEE"      // 4400 non-dust conversion spread
	IncomeSwapFreeAdminFee = "SWAPFREE_ADMIN_FEE"  // 4020/4300 swap-free admin fee
	IncomeInactivityFee    = "INACTIVITY_FEE"      // 4500 inactivity/dormancy fee
	IncomeLiquidationPen   = "LIQUIDATION_PENALTY" // 5010 liquidation penalty clearing
)

// incomeAccountPrefix maps the numeric prefix of a ledger_lines
// account_code ("4030_COMMISSION_REVENUE_USD" → "4030") to the income
// type — the GL-side discriminator for journals whose wallet entry_type
// is shared (FEE covers commission, rebates and funding fees alike).
var incomeAccountPrefix = map[string]string{
	"4030": IncomeCommission,
	"4100": IncomeSwapRollover, // interbank leg
	"4110": IncomeSwapRollover, // admin markup leg
	"5100": IncomeRebate,
	"5200": IncomeNBPAdjust,
	"4200": IncomeFundingFee,
	"4400": IncomeConversionFee,
	"4010": IncomeTradingFee,
	"4020": IncomeSwapFreeAdminFee,
	"4300": IncomeSwapFreeAdminFee,
	"4500": IncomeInactivityFee,
	"5010": IncomeLiquidationPen,
}

// incomeNarrativePrefixes are poster-specific description/idempotency_key
// markers checked before the account-code map — they disambiguate
// postings whose counterparty accounts overlap (dust conversion spread
// also lands on 4400, so DUST_CONVERT must win over CONVERSION_FEE).
var incomeNarrativePrefixes = []struct {
	prefix     string // matched against journal description
	idemPrefix string // matched against journal_entries.idempotency_key
	incomeType string
}{
	{"DUST_CONVERT", "dust:", IncomeDustConvert},
	// "nbp:" catches the insurance-fund debit path whose journal
	// description is "retail NBP restitution acct …" (nbp.go
	// fundRestitution) rather than the NBP_RESTITUTION_POSTED marker.
	{"NBP_RESTITUTION", "nbp:", IncomeNBPAdjust},
	{"MAKER_REBATE", "makerrebate:", IncomeRebate},
	{"MM_REBATE", "mmrebate:", IncomeRebate},
	{"COMMISSION", "commission:", IncomeCommission},
	{"TOMNEXT", "", IncomeSwapRollover},
	{"", "funding-fee:", IncomeFundingFee},
}

// incomeKeyRe validates the `type=` query token (set membership is NOT
// enforced — the taxonomy is deliberately open so verbatim passthrough
// types like DEPOSIT/TRADE_FILL stay queryable per the DDL contract).
var incomeKeyRe = regexp.MustCompile(`^[A-Z0-9_]{2,32}$`)

// ValidIncomeType reports whether raw is a well-formed type filter token.
func ValidIncomeType(raw string) bool { return incomeKeyRe.MatchString(raw) }

// incomeUmbrellaTypes is the set of wallet entry_types whose journals
// routinely mix the client-facing movement with house revenue/expense
// lines on the same GL journal — exactly the ambiguity the
// ledger_lines account-code discriminator resolves. TRADE_FILL must NOT
// consult it: a fill journal posts its fee on 4010 next to the fill
// legs, and mistyping the wallet fill row as TRADING_FEE would corrupt
// the client's income stream (statement integrity, §16.7).
var incomeUmbrellaTypes = map[string]bool{
	"FEE": true, "TRANSFER": true, "ADJUSTMENT": true,
}

// ClassifyIncome resolves the §16.7 income type for one wallet
// ledger_entries row from its journal's metadata. entryType is the
// ledger_entries.entry_type (ledger_entry_type_enum: FEE/TRANSFER/…);
// journalType/idempotencyKey/description come from journal_entries;
// lineAccounts are the journal's ledger_lines account codes (empty for
// journal-less rows — ledger_entries.journal_entry_id is nullable).
//
// Order: explicit narrative/idempotency markers → GL counterparty
// account-code prefixes (umbrella types only) → the coarse entry_type
// map (identical to mapIncomeType in etl.go) → verbatim entry_type
// passthrough.
func ClassifyIncome(entryType, journalType, idempotencyKey, description string, lineAccounts []string) string {
	for _, r := range incomeNarrativePrefixes {
		if r.prefix != "" && strings.HasPrefix(description, r.prefix) {
			return r.incomeType
		}
		if r.idemPrefix != "" && strings.HasPrefix(idempotencyKey, r.idemPrefix) {
			return r.incomeType
		}
	}
	if incomeUmbrellaTypes[entryType] {
		for _, code := range lineAccounts {
			prefix, _, ok := strings.Cut(code, "_")
			if !ok {
				continue
			}
			if t, found := incomeAccountPrefix[prefix]; found {
				return t
			}
		}
	}
	// GL-aware fallbacks: entry/journal type shape when no revenue/
	// expense counterparty account is present.
	if journalType == "EOD_ROLLOVER" || entryType == "ROLLOVER" {
		return IncomeSwapRollover
	}
	switch entryType {
	case "FEE":
		return IncomeCommission // coarse map parity (etl.go)
	case "ADJUSTMENT":
		return IncomeNBPAdjust
	}
	return entryType // verbatim passthrough per the sibling DDL contract
}

// ---------------------------------------------------------------------------
// Store — ClickHouse write + read projection
// ---------------------------------------------------------------------------

// IncomeRow is one income_ledger row. Symbol is populated by the read
// path only (derived column — the table has none; see file header).
type IncomeRow struct {
	PostedAt       time.Time
	AccountID      int64
	Currency       string
	IncomeType     string
	EntryType      string // raw PG ledger_entry_type
	Symbol         string // derived: narrative pair or trade join
	Amount         decimal.Decimal
	LedgerEntryID  uint64
	JournalEntryID uint64
	ReferenceID    uint64
	Description    string
}

// IncomeCursor is the (posted_at, ledger_entry_id) keyset position for
// the DESC read stream; the api layer renders it as the §8.8 opaque
// cursor token.
type IncomeCursor struct {
	PostedAt      time.Time
	LedgerEntryID uint64
}

// IncomeQuery is one bounded read of the income projection. Rows return
// newest-first; From inclusive, To exclusive; a zero bound is unbounded.
type IncomeQuery struct {
	AccountID int64
	Type      string        // "" = all types
	Symbol    string        // canonical "EUR/USD"; "" = unfiltered
	From      time.Time     // inclusive
	To        time.Time     // exclusive
	After     *IncomeCursor // keyset continuation
	Limit     int           // 0 → store default (100)
}

// IncomeTotal is one (income_type, currency) signed sum over a window —
// the reconciliation quantum for ReconcileToStatement.
type IncomeTotal struct {
	IncomeType string
	Currency   string
	Total      decimal.Decimal
}

// incomeSelectCore is the derived-symbol FROM clause shared by the page
// and count queries. The trades join is bounded to this account's
// referenced trades so the right-side hash table never scales with the
// whole tape. '→'/'->' arrow narratives (DUST_CONVERT) normalize to the
// canonical BASE/QUOTE slash form.
const incomeSelectCore = `FROM ` + IncomeTable + ` AS il FINAL
LEFT ANY JOIN (
    SELECT trade_id, any(symbol) AS symbol
    FROM ` + TradesTable + ` FINAL
    WHERE trade_id IN (
        SELECT reference_id FROM ` + IncomeTable + ` FINAL
        WHERE account_id = ? AND reference_id != 0
    )
    GROUP BY trade_id
) AS t ON t.trade_id = il.reference_id`

// incomeDerivedSymbol resolves the instrument token the row trades at:
//  1. 'XXX/YYY' already in the narrative (canonical form);
//  2. 'XXX→YYY'/'XXX->YYY' arrow narratives (DUST_CONVERT) normalized to
//     the slash form — extract returns its FIRST capture group, so the
//     outer parens capture the whole pair and the inner group is the
//     arrow replaced by '/';
//  3. the trades-table symbol for reference_id (commission/rebate rows
//     carry trade=NNN, not a pair name).
//
// Patterns deliberately avoid {n} quantifiers: the clickhouse-go client
// treats '{…:…}' text as a named-parameter token and rejects positional
// binds (bindQueryOrAppendParameters — observed as "unsupported query
// parameter type").
const incomeDerivedSymbol = `coalesce(
    nullIf(extract(il.description, '[A-Z][A-Z][A-Z]/[A-Z][A-Z][A-Z]'), ''),
    nullIf(replaceRegexpAll(
        extract(il.description, '([A-Z][A-Z][A-Z](→|->)[A-Z][A-Z][A-Z])'),
        '(→|->)', '/'), ''),
    nullIf(t.symbol, ''),
    '') AS symbol`

// IncomeStore is the income_ledger read/write path over the shared Conn
// seam. nil conn fails closed at call time.
type IncomeStore struct {
	conn Conn
	now  func() time.Time
}

// NewIncomeStore wires the store.
func NewIncomeStore(conn Conn) *IncomeStore {
	return &IncomeStore{conn: conn, now: func() time.Time { return time.Now().UTC() }}
}

// WithClock swaps the version clock (tests inject a fixed now for
// deterministic `ver` ordering).
func (s *IncomeStore) WithClock(now func() time.Time) *IncomeStore {
	if now != nil {
		s.now = now
	}
	return s
}

func (s *IncomeStore) requireConn() error {
	if s == nil || s.conn == nil {
		return errors.New("analytics: income store has no ClickHouse conn")
	}
	return nil
}

// incomeRowValues projects a row minus `ver` — the same column order the
// etl.go tableColumns registry / InsertWithSpool consumes.
func incomeRowValues(r IncomeRow) []any {
	return []any{
		r.PostedAt.UTC(), uint64(r.AccountID), r.Currency, r.IncomeType,
		r.EntryType, r.Amount, r.LedgerEntryID, r.JournalEntryID,
		r.ReferenceID, r.Description,
	}
}

// Insert writes one batch (PrepareBatch, native columnar path). Replays
// are safe: ReplacingMergeTree(ver) collapses the same
// (account_id, currency, ledger_entry_id) key to the newest version.
func (s *IncomeStore) Insert(ctx context.Context, rows []IncomeRow) error {
	if err := s.requireConn(); err != nil {
		return err
	}
	if len(rows) == 0 {
		return nil
	}
	ver := uint64(s.now().UnixNano())
	batch, err := s.conn.PrepareBatch(ctx,
		"INSERT INTO "+IncomeTable+" ("+incomeColumns+")")
	if err != nil {
		return fmt.Errorf("analytics: income prepare batch: %w", err)
	}
	for i := range rows {
		v := incomeRowValues(rows[i])
		if err := batch.Append(append(v, ver)...); err != nil {
			_ = batch.Abort()
			return fmt.Errorf("analytics: income append ledger_entry=%d: %w",
				rows[i].LedgerEntryID, err)
		}
	}
	if err := batch.Send(); err != nil {
		return fmt.Errorf("analytics: income batch send (%d rows): %w", len(rows), err)
	}
	return nil
}

// MaxLedgerEntryID is the sync watermark: the highest ledger_entries.id
// already projected. Strictly finer-grained than a journal_entry_id
// watermark — a batch crash cannot strand the tail rows of a partially
// written journal (task text names journal_entry_id; ledger_entry_id is
// the correct resume boundary and the table's ordering key).
func (s *IncomeStore) MaxLedgerEntryID(ctx context.Context) (uint64, error) {
	if err := s.requireConn(); err != nil {
		return 0, err
	}
	// max() on an empty table returns 0 for UInt64 (not NULL), so a
	// fresh projection resumes at cursor 0.
	rows, err := s.conn.Query(ctx,
		"SELECT max(ledger_entry_id) FROM "+IncomeTable)
	if err != nil {
		return 0, fmt.Errorf("analytics: income watermark: %w", err)
	}
	defer rows.Close()
	var v uint64
	if rows.Next() {
		if err := rows.Scan(&v); err != nil {
			return 0, fmt.Errorf("analytics: income watermark scan: %w", err)
		}
	}
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("analytics: income watermark rows: %w", err)
	}
	return v, nil
}

// HasAny reports whether the projection holds any row at all — the
// handler's fail-closed "projection never populated" probe (503), kept
// distinct from a legitimately empty account page (200 + empty data).
func (s *IncomeStore) HasAny(ctx context.Context) (bool, error) {
	if err := s.requireConn(); err != nil {
		return false, err
	}
	rows, err := s.conn.Query(ctx,
		"SELECT 1 FROM "+IncomeTable+" LIMIT 1")
	if err != nil {
		return false, fmt.Errorf("analytics: income emptiness probe: %w", err)
	}
	defer rows.Close()
	found := rows.Next()
	if err := rows.Err(); err != nil {
		return false, fmt.Errorf("analytics: income emptiness rows: %w", err)
	}
	return found, nil
}

// incomePredicate renders the outer WHERE (leading "AND …" fragments)
// plus its args. The inner join-subquery arg (account_id) is prepended
// by the caller — predicate args always follow it positionally.
func (q IncomeQuery) incomePredicate() (string, []any) {
	var b strings.Builder
	var args []any
	b.WriteString(" WHERE account_id = ?")
	args = append(args, uint64(q.AccountID))
	if q.Type != "" {
		b.WriteString(" AND income_type = ?")
		args = append(args, q.Type)
	}
	if q.Symbol != "" {
		// Canonical slash form covers narrative-derived symbols; the flat
		// form covers trades.symbol conventions that store "EURUSD".
		flat := strings.ReplaceAll(q.Symbol, "/", "")
		b.WriteString(" AND symbol IN (?, ?)")
		args = append(args, q.Symbol, flat)
	}
	if !q.From.IsZero() {
		b.WriteString(" AND posted_at >= ?")
		args = append(args, q.From.UTC())
	}
	if !q.To.IsZero() {
		b.WriteString(" AND posted_at < ?")
		args = append(args, q.To.UTC())
	}
	if q.After != nil {
		b.WriteString(" AND (posted_at, ledger_entry_id) < (?, ?)")
		args = append(args, q.After.PostedAt.UTC(), q.After.LedgerEntryID)
	}
	return b.String(), args
}

// Query returns one keyset page (newest first) plus the total matching
// row count for the §8.8 envelope. FINAL collapses in-flight
// ReplacingMergeTree duplicates so a mid-sync replay never double-counts.
func (s *IncomeStore) Query(ctx context.Context, q IncomeQuery) ([]IncomeRow, int64, error) {
	if err := s.requireConn(); err != nil {
		return nil, 0, err
	}
	limit := q.Limit
	if limit <= 0 {
		limit = 100
	}
	pred, predArgs := q.incomePredicate()
	subquery := "(SELECT il.posted_at, il.account_id, il.currency, il.income_type, " +
		"il.entry_type, il.amount, il.ledger_entry_id, il.journal_entry_id, " +
		"il.reference_id, il.description, " + incomeDerivedSymbol + " " +
		incomeSelectCore + ")"
	args := append([]any{uint64(q.AccountID)}, predArgs...)

	var total int64
	cnt, err := s.conn.Query(ctx,
		"SELECT count() FROM "+subquery+pred, args...)
	if err != nil {
		return nil, 0, fmt.Errorf("analytics: income count: %w", err)
	}
	if cnt.Next() {
		var n uint64
		if err := cnt.Scan(&n); err != nil {
			cnt.Close()
			return nil, 0, fmt.Errorf("analytics: income count scan: %w", err)
		}
		total = int64(n)
	}
	cnt.Close()
	if err := cnt.Err(); err != nil {
		return nil, 0, fmt.Errorf("analytics: income count rows: %w", err)
	}

	pageArgs := append(append([]any(nil), args...), uint64(limit))
	rows, err := s.conn.Query(ctx,
		"SELECT posted_at, account_id, currency, income_type, entry_type, "+
			"amount, ledger_entry_id, journal_entry_id, reference_id, "+
			"description, symbol FROM "+subquery+pred+
			" ORDER BY posted_at DESC, ledger_entry_id DESC LIMIT ?",
		pageArgs...)
	if err != nil {
		return nil, 0, fmt.Errorf("analytics: income query acct=%d: %w", q.AccountID, err)
	}
	defer rows.Close()

	out := make([]IncomeRow, 0, limit)
	for rows.Next() {
		var r IncomeRow
		var acct, leID, jeID, refID uint64
		if err := rows.Scan(&r.PostedAt, &acct, &r.Currency, &r.IncomeType,
			&r.EntryType, &r.Amount, &leID, &jeID, &refID,
			&r.Description, &r.Symbol); err != nil {
			return nil, 0, fmt.Errorf("analytics: income row scan: %w", err)
		}
		r.AccountID = int64(acct)
		r.LedgerEntryID, r.JournalEntryID, r.ReferenceID = leID, jeID, refID
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("analytics: income rows: %w", err)
	}
	return out, total, nil
}

// Totals returns per-(income_type, currency) signed sums over
// [from, to) — the CH side of the statements reconciliation.
func (s *IncomeStore) Totals(ctx context.Context, accountID int64, from, to time.Time) ([]IncomeTotal, error) {
	if err := s.requireConn(); err != nil {
		return nil, err
	}
	rows, err := s.conn.Query(ctx,
		"SELECT income_type, currency, sum(amount) FROM "+IncomeTable+" FINAL "+
			"WHERE account_id = ? AND posted_at >= ? AND posted_at < ? "+
			"GROUP BY income_type, currency ORDER BY income_type, currency",
		uint64(accountID), from.UTC(), to.UTC())
	if err != nil {
		return nil, fmt.Errorf("analytics: income totals: %w", err)
	}
	defer rows.Close()
	var out []IncomeTotal
	for rows.Next() {
		var t IncomeTotal
		if err := rows.Scan(&t.IncomeType, &t.Currency, &t.Total); err != nil {
			return nil, fmt.Errorf("analytics: income totals scan: %w", err)
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// ---------------------------------------------------------------------------
// PG → CH sync (the refined §16.7 projection)
// ---------------------------------------------------------------------------

// incomeRowsSQL reads one page of wallet ledger entries newer than the
// cursor, joined to journal_entries for the entry-type/idempotency/
// description markers and to ledger_lines for the GL counterparty
// account codes that discriminate the income taxonomy.
const incomeRowsSQL = `
SELECT le.id, le.account_id, le.currency, le.direction::text,
       le.amount::text, le.entry_type::text,
       COALESCE(le.reference_id, 0), COALESCE(le.journal_entry_id, 0),
       le.posted_at, COALESCE(le.description, ''),
       COALESCE(je.entry_type::text, ''), COALESCE(je.idempotency_key, ''),
       COALESCE(je.description, ''),
       COALESCE((SELECT array_agg(DISTINCT ll.account_code)
                   FROM ledger_lines ll
                  WHERE ll.journal_entry_id = le.journal_entry_id),
                '{}'::varchar[]) AS line_accounts
FROM ledger_entries le
LEFT JOIN journal_entries je ON je.id = le.journal_entry_id
WHERE le.id > $1
ORDER BY le.id ASC
LIMIT $2`

// IncomeSyncRows reads + classifies one ledger_entries page (strictly
// after cursor). Returns the projected rows and the next cursor
// (max ledger_entry_id seen — the resume boundary).
func IncomeSyncRows(ctx context.Context, pg PGQuerier, cursor uint64, limit int) ([]IncomeRow, uint64, error) {
	if limit <= 0 {
		limit = 5_000
	}
	rows, err := pg.Query(ctx, incomeRowsSQL, int64(cursor), limit)
	if err != nil {
		return nil, cursor, fmt.Errorf("income sync read: %w", err)
	}
	defer rows.Close()

	out := make([]IncomeRow, 0, limit)
	next := cursor
	for rows.Next() {
		var (
			id, acct, ref, journal int64
			ccy, dir, etype        string
			amountTxt              string
			posted                 time.Time
			desc, jtype, idem, jd  string
			lineAccounts           []string
		)
		if err := rows.Scan(&id, &acct, &ccy, &dir, &amountTxt,
			&etype, &ref, &journal, &posted, &desc,
			&jtype, &idem, &jd, &lineAccounts); err != nil {
			return nil, cursor, fmt.Errorf("income sync scan: %w", err)
		}
		amount, err := decimal.NewFromString(amountTxt)
		if err != nil {
			return nil, cursor, fmt.Errorf("income sync amount %q: %w", amountTxt, err)
		}
		// Signed from the wallet's perspective (§16.7 / migration 102):
		// DEBIT = value into the wallet (+), CREDIT = value out (−).
		if dir == "CREDIT" {
			amount = amount.Neg()
		}
		if jd != "" { // journal description wins over the wallet copy
			desc = jd
		}
		out = append(out, IncomeRow{
			PostedAt: posted.UTC(), AccountID: acct, Currency: ccy,
			IncomeType: ClassifyIncome(etype, jtype, idem, jd, lineAccounts),
			EntryType:  etype, Amount: amount,
			LedgerEntryID: uint64(id), JournalEntryID: uint64(journal),
			ReferenceID: uint64(ref), Description: desc,
		})
		if uint64(id) > next {
			next = uint64(id)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, cursor, fmt.Errorf("income sync rows: %w", err)
	}
	return out, next, nil
}

// IncomeSyncResult reports one catch-up pass.
type IncomeSyncResult struct {
	Rows      int
	Watermark uint64 // max ledger_entry_id projected
}

// IncomeSync runs the standalone PG→CH projection to exhaustion: it
// resumes from the max(ledger_entry_id) watermark already in the table
// (self-healing after restarts; reruns are idempotent — every re-insert
// collapses onto the same (account_id, currency, ledger_entry_id) key
// with a newer ver). Intended for backfill/CLI operation and as the
// reference implementation the Task 20.3.1 ETL daemon converges on.
func IncomeSync(ctx context.Context, pg PGQuerier, store *IncomeStore, pageSize int) (*IncomeSyncResult, error) {
	cursor, err := store.MaxLedgerEntryID(ctx)
	if err != nil {
		return nil, err
	}
	res := &IncomeSyncResult{Watermark: cursor}
	for {
		rows, next, err := IncomeSyncRows(ctx, pg, cursor, pageSize)
		if err != nil {
			return res, err
		}
		if len(rows) == 0 {
			return res, nil
		}
		if err := store.Insert(ctx, rows); err != nil {
			return res, err
		}
		res.Rows += len(rows)
		res.Watermark = next
		cursor = next
		if len(rows) < pageSize {
			return res, nil
		}
	}
}

// PollIncomeOnceRefined is the Ingester-driven variant of one income
// page: identical durability (InsertWithSpool → CH or disk spool) and
// cursor contract as PollIncomeOnce (etl.go), but rows carry the refined
// ClassifyIncome type. ETL daemons wanting the six-type §16.7 taxonomy
// call this instead.
func (g *Ingester) PollIncomeOnceRefined(ctx context.Context, q PGQuerier, cursor uint64, limit int) (uint64, int, error) {
	rows, next, err := IncomeSyncRows(ctx, q, cursor, limit)
	if err != nil {
		return cursor, 0, err
	}
	if len(rows) == 0 {
		return cursor, 0, nil
	}
	raw := make([][]any, len(rows))
	for i, r := range rows {
		raw[i] = incomeRowValues(r)
	}
	if err := g.InsertWithSpool(ctx, IncomeTable, raw); err != nil {
		return cursor, 0, err
	}
	g.m.incIncomeRows(len(rows))
	var maxPosted time.Time
	for _, r := range rows {
		if r.PostedAt.After(maxPosted) {
			maxPosted = r.PostedAt
		}
	}
	if !maxPosted.IsZero() {
		g.m.SetIncomeLag(time.Since(maxPosted))
	}
	return next, len(raw), nil
}

// ---------------------------------------------------------------------------
// Statements reconciliation (spec §16.7 AC: "reconciled to statements")
// ---------------------------------------------------------------------------

// incomeTotalsPGSQL recomputes the same window straight from the book of
// record — ledger_entries joined to journal_entries + ledger_lines, the
// identical derivation the Task 20.3.6 statements read (both sides of
// ReconcileToStatement trace to the same GL rows).
const incomeTotalsPGSQL = `
SELECT le.currency, le.direction::text, le.amount::text,
       le.entry_type::text,
       COALESCE(je.entry_type::text, ''), COALESCE(je.idempotency_key, ''),
       COALESCE(je.description, ''),
       COALESCE((SELECT array_agg(DISTINCT ll.account_code)
                   FROM ledger_lines ll
                  WHERE ll.journal_entry_id = le.journal_entry_id),
                '{}'::varchar[]) AS line_accounts
FROM ledger_entries le
LEFT JOIN journal_entries je ON je.id = le.journal_entry_id
WHERE le.account_id = $1 AND le.posted_at >= $2 AND le.posted_at < $3`

// IncomeTotalsPG computes the GL-side income sums for one account over
// [from, to) — classification identical to the projection (drift between
// the two would make reconciliation vacuous).
func IncomeTotalsPG(ctx context.Context, pg PGQuerier, accountID int64, from, to time.Time) ([]IncomeTotal, error) {
	rows, err := pg.Query(ctx, incomeTotalsPGSQL, accountID, from.UTC(), to.UTC())
	if err != nil {
		return nil, fmt.Errorf("income totals pg: %w", err)
	}
	defer rows.Close()
	type key struct{ t, c string }
	sums := map[key]decimal.Decimal{}
	var order []key
	for rows.Next() {
		var (
			ccy, dir, amountTxt string
			etype, jtype, idem  string
			jd                  string
			lineAccounts        []string
		)
		if err := rows.Scan(&ccy, &dir, &amountTxt,
			&etype, &jtype, &idem, &jd, &lineAccounts); err != nil {
			return nil, fmt.Errorf("income totals pg scan: %w", err)
		}
		amount, err := decimal.NewFromString(amountTxt)
		if err != nil {
			return nil, fmt.Errorf("income totals pg amount %q: %w", amountTxt, err)
		}
		if dir == "CREDIT" {
			amount = amount.Neg()
		}
		k := key{ClassifyIncome(etype, jtype, idem, jd, lineAccounts), ccy}
		if _, ok := sums[k]; !ok {
			order = append(order, k)
		}
		sums[k] = sums[k].Add(amount)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out := make([]IncomeTotal, 0, len(order))
	for _, k := range order {
		out = append(out, IncomeTotal{IncomeType: k.t, Currency: k.c, Total: sums[k]})
	}
	return out, nil
}

// ReconcileLine compares one (income_type, currency) bucket across the
// projection and the GL book of record.
type ReconcileLine struct {
	IncomeType     string          `json:"income_type"`
	Currency       string          `json:"currency"`
	LedgerTotal    decimal.Decimal `json:"ledger_total"`    // CH income_ledger
	StatementTotal decimal.Decimal `json:"statement_total"` // PG GL-side recompute
	Match          bool            `json:"match"`
}

// ReconcileReport is the output of ReconcileToStatement — reportable as
// the §24 #361 "reconciled to statements" check evidence.
type ReconcileReport struct {
	AccountID int64           `json:"account_id"`
	From      time.Time       `json:"from"`
	To        time.Time       `json:"to"`
	Lines     []ReconcileLine `json:"lines"`
	OK        bool            `json:"ok"`
}

// ReconcileToStatement compares a period's income-ledger sums (CH
// projection) against the GL-side statement computation for the same
// window (PG ledger_entries ⋈ journal_entries ⋈ ledger_lines — the rows
// statements are drawn from). Any sum mismatch or bucket asymmetry
// fails the report — including a missing bucket on either side — so ETL
// lag/loss surfaces as OK=false rather than a silent drift.
func ReconcileToStatement(ctx context.Context, pg PGQuerier, store *IncomeStore,
	accountID int64, from, to time.Time) (*ReconcileReport, error) {
	ch, err := store.Totals(ctx, accountID, from, to)
	if err != nil {
		return nil, err
	}
	gl, err := IncomeTotalsPG(ctx, pg, accountID, from, to)
	if err != nil {
		return nil, err
	}
	type key struct{ t, c string }
	chSums := map[key]decimal.Decimal{}
	glSums := map[key]decimal.Decimal{}
	seen := map[key]bool{}
	var order []key
	for _, t := range ch {
		k := key{t.IncomeType, t.Currency}
		chSums[k] = t.Total
		if !seen[k] {
			seen[k] = true
			order = append(order, k)
		}
	}
	for _, t := range gl {
		k := key{t.IncomeType, t.Currency}
		glSums[k] = glSums[k].Add(t.Total)
		if !seen[k] {
			seen[k] = true
			order = append(order, k)
		}
	}
	rep := &ReconcileReport{AccountID: accountID, From: from.UTC(), To: to.UTC(), OK: true}
	for _, k := range order {
		l := ReconcileLine{
			IncomeType: k.t, Currency: k.c,
			LedgerTotal:    chSums[k], // zero value when bucket absent
			StatementTotal: glSums[k],
		}
		l.Match = l.LedgerTotal.Equal(l.StatementTotal)
		if !l.Match {
			rep.OK = false
		}
		rep.Lines = append(rep.Lines, l)
	}
	return rep, nil
}
