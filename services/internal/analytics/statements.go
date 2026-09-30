// statements.go — Phase-20 Task 20.3.6: client statements, trade
// confirmations (contract notes) and the FileStore seam backing the
// generated-document store (spec §5.28, §8.4; §24 #143).
//
// The statement/confirmation records live in migration 049 tables
// (client_statements, trade_confirmations); rendered documents land in
// the object store under "<file_ref>.pdf" and "<file_ref>.csv" —
// file_ref is the extension-less key stem, so one row always points at
// both renderings.
//
// Fail-closed economics (spec §2.7/§5.21): every statement is built from
// the wallet ledger (ledger_entries — itself the GL-mirrored §5.3 record)
// and cross-checked against the GL journal set behind it:
//
//  1. opening + Σ movements == closing per currency — a gap means a
//     missing/duplicated ledger row and the statement is refused;
//  2. VerifyGLConservation asserts Σ debits == Σ credits per currency
//     over every journal referenced by the statement window.
//
// Statement == ledger, never "best effort" (AGENTS.md convention).
//
// Delivery split: this package OWNS generation + durable storage. The
// multi-channel delivery engine (secure portal / email / MT515) is the
// Task 20.3.8 sibling in internal/reporting — it calls
// ConfirmationService.Generate (within the 60s contract-note window),
// ConfirmationService.MarkDelivered to stamp delivered_at, and reads the
// stored bytes via GetFile. Do not grow a second generation path.
package analytics

import (
	"bytes"
	"context"
	"crypto/md5"
	"crypto/sha256"
	"encoding/hex"
	stderrors "errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"exchange/internal/objectstore"
	"exchange/pkg/decimal"
	excerrors "exchange/pkg/errors"
)

// ---------------------------------------------------------------------------
// FileStore — the generated-document object-store seam.
// ---------------------------------------------------------------------------

// FileStore is the narrow put/get surface for generated statement,
// confirmation, invoice and ERP documents. Implementations:
//
//	S3FileStore   — wraps objectstore.Client (real S3/R2 or the devs3
//	                endpoint-override client; keys are namespaced under
//	                Prefix so one bucket can host every document class).
//	MemFileStore  — in-memory fake for unit tests.
type FileStore interface {
	// Put stores body at key and returns the durable reference to persist
	// in *_file_ref columns. Implementations may prefix/qualify the key.
	Put(ctx context.Context, key string, body []byte, contentType string) (string, error)
	// Get fetches the object recorded under ref exactly as Put returned it.
	Get(ctx context.Context, ref string) ([]byte, error)
}

// S3FileStore is the production/dev adapter over objectstore.Client.
type S3FileStore struct {
	Client objectstore.Client
	Prefix string // key namespace, e.g. "reports" — "" stores at bucket root
}

func (s *S3FileStore) fullKey(key string) string {
	if s.Prefix == "" {
		return key
	}
	return strings.TrimSuffix(s.Prefix, "/") + "/" + key
}

// Put uploads the document and verifies the returned ETag equals
// md5(body) — the zero-loss write contract of objectstore.Client.
func (s *S3FileStore) Put(ctx context.Context, key string, body []byte, contentType string) (string, error) {
	if s.Client == nil {
		return "", excerrors.New("INTERNAL_ERROR", "filestore: nil objectstore client")
	}
	obj, err := s.Client.Put(ctx, objectstore.PutInput{
		Key:         s.fullKey(key),
		Body:        bytes.NewReader(body),
		Size:        int64(len(body)),
		ContentType: contentType,
	})
	if err != nil {
		return "", fmt.Errorf("filestore: put %q: %w", key, err)
	}
	sum := md5.Sum(body)
	if obj.ETag != "" && !strings.EqualFold(strings.Trim(obj.ETag, `"`), hex.EncodeToString(sum[:])) {
		return "", fmt.Errorf("filestore: etag mismatch on %q (wrote anyway? refusing)", key)
	}
	return obj.Key, nil
}

// Get reads a stored object; a miss is wrapped in objectstore.NotFoundError.
func (s *S3FileStore) Get(ctx context.Context, ref string) ([]byte, error) {
	if s.Client == nil {
		return nil, excerrors.New("INTERNAL_ERROR", "filestore: nil objectstore client")
	}
	body, _, err := s.Client.Get(ctx, ref)
	if err != nil {
		return nil, fmt.Errorf("filestore: get %q: %w", ref, err)
	}
	return body, nil
}

// MemFileStore is the deterministic test fake (single-process).
type MemFileStore struct {
	Objects map[string][]byte
	Err     error // when set, Put/Get fail with it (failure-path tests)
}

// NewMemFileStore returns an empty fake.
func NewMemFileStore() *MemFileStore {
	return &MemFileStore{Objects: map[string][]byte{}}
}

// Put implements FileStore.
func (m *MemFileStore) Put(_ context.Context, key string, body []byte, _ string) (string, error) {
	if m.Err != nil {
		return "", m.Err
	}
	if m.Objects == nil {
		m.Objects = map[string][]byte{}
	}
	m.Objects[key] = append([]byte(nil), body...)
	return key, nil
}

// Get implements FileStore.
func (m *MemFileStore) Get(_ context.Context, ref string) ([]byte, error) {
	if m.Err != nil {
		return nil, m.Err
	}
	b, ok := m.Objects[ref]
	if !ok {
		return nil, &objectstore.NotFoundError{Key: ref, Err: stderrors.New("memstore miss")}
	}
	return append([]byte(nil), b...), nil
}

// ---------------------------------------------------------------------------
// Statement domain types
// ---------------------------------------------------------------------------

// StatementPeriod mirrors statement_period_enum (migration 049).
type StatementPeriod string

const (
	StmtPeriodDaily   StatementPeriod = "DAILY"
	StmtPeriodMonthly StatementPeriod = "MONTHLY"
)

// ParseStatementPeriod validates ?period= — empty means "all".
func ParseStatementPeriod(s string) (StatementPeriod, error) {
	switch strings.ToUpper(strings.TrimSpace(s)) {
	case "":
		return "", nil
	case string(StmtPeriodDaily):
		return StmtPeriodDaily, nil
	case string(StmtPeriodMonthly):
		return StmtPeriodMonthly, nil
	}
	return "", excerrors.New("INVALID_REQUEST",
		fmt.Sprintf("period %q must be DAILY|MONTHLY", s))
}

// LedgerMovement is one wallet-ledger row inside the statement window.
type LedgerMovement struct {
	ID          int64           `json:"id"`
	EntryType   string          `json:"entry_type"`
	Direction   string          `json:"direction"` // DEBIT = value in, CREDIT = value out (§5.3)
	Amount      decimal.Decimal `json:"amount"`
	Currency    string          `json:"currency"`
	Description string          `json:"description,omitempty"`
	JournalID   *int64          `json:"journal_entry_id,omitempty"`
	PostedAt    time.Time       `json:"posted_at"`
}

// SignedAmount renders the movement's effect on the wallet balance.
func (m LedgerMovement) SignedAmount() decimal.Decimal {
	if m.Direction == "DEBIT" {
		return m.Amount
	}
	return m.Amount.Neg()
}

// PendingMovement is a funding transaction still in-flight at period end
// (spec §7.x review tiers / pending settlement). Pending rows are shown
// flagged on the statement — never silently dropped, never folded into
// the movement totals (they haven't posted).
type PendingMovement struct {
	FundingID int64           `json:"funding_id"`
	Type      string          `json:"type"`
	Status    string          `json:"status"` // PENDING | CONFIRMED | PENDING_REVIEW
	Amount    decimal.Decimal `json:"amount"`
	Currency  string          `json:"currency"`
	CreatedAt time.Time       `json:"created_at"`
}

// StatementTrade is one fill row for the trades section.
type StatementTrade struct {
	TradeID    int64           `json:"trade_id"`
	Symbol     string          `json:"symbol"`
	Side       string          `json:"side"` // BUY|SELL from this account's perspective
	Price      decimal.Decimal `json:"price"`
	Quantity   decimal.Decimal `json:"quantity"`
	Fee        decimal.Decimal `json:"fee"`
	FeeCcy     string          `json:"fee_currency"` // instrument quote currency
	ExecutedAt time.Time       `json:"executed_at"`
}

// PositionPnl is one open-position mark for the unrealized-P&L section.
type PositionPnl struct {
	Symbol        string          `json:"symbol"`
	Side          string          `json:"side"`
	Quantity      decimal.Decimal `json:"quantity"`
	UnrealizedPnl decimal.Decimal `json:"unrealized_pnl"`
	Currency      string          `json:"currency"` // instrument quote currency
}

// CurrencyStatement is the per-currency reconciliation block: opening +
// movements == closing, exactly (fail-closed assert before rendering).
type CurrencyStatement struct {
	Currency  string           `json:"currency"`
	Opening   decimal.Decimal  `json:"opening_balance"`
	NetMove   decimal.Decimal  `json:"net_movement"`
	Closing   decimal.Decimal  `json:"closing_balance"`
	Movements []LedgerMovement `json:"movements,omitempty"`
}

// Statement is the fully assembled client statement document.
type Statement struct {
	AccountID   int64               `json:"account_id"`
	Period      StatementPeriod     `json:"period"`
	PeriodStart time.Time           `json:"period_start"`
	PeriodEnd   time.Time           `json:"period_end"` // exclusive
	GeneratedAt time.Time           `json:"generated_at"`
	Currencies  []CurrencyStatement `json:"currencies"`
	Trades      []StatementTrade    `json:"trades"`
	Pending     []PendingMovement   `json:"pending_movements"`
	Positions   []PositionPnl       `json:"open_positions"`
}

// StatementRow is the client_statements list/read row.
type StatementRow struct {
	StatementID   int64           `json:"statement_id"`
	AccountID     int64           `json:"account_id"`
	Period        StatementPeriod `json:"period"`
	PeriodStart   time.Time       `json:"period_start"`
	PeriodEnd     time.Time       `json:"period_end"`
	StatementType string          `json:"statement_type"`
	FileRef       string          `json:"file_ref"`
	ContentSHA256 string          `json:"content_sha256"`
	GeneratedAt   time.Time       `json:"generated_at"`
}

// StatementFile is a downloaded document payload.
type StatementFile struct {
	Body        []byte
	ContentType string
	FileRef     string
	Row         *StatementRow
}

// ---------------------------------------------------------------------------
// StatementService
// ---------------------------------------------------------------------------

// StatementService generates and serves client statements.
type StatementService struct {
	pool  *pgxpool.Pool
	files FileStore
	now   func() time.Time
	docs  *DocCipher
}

// NewStatementService wires the service; both dependencies are required
// (fail-closed — a nil store would emit statements with no durable file).
func NewStatementService(pool *pgxpool.Pool, files FileStore) (*StatementService, error) {
	if pool == nil {
		return nil, fmt.Errorf("statements: nil pgx pool")
	}
	if files == nil {
		return nil, fmt.Errorf("statements: nil FileStore")
	}
	return &StatementService{pool: pool, files: files, now: time.Now}, nil
}

// SetClockForTest overrides the clock; tests only.
func (s *StatementService) SetClockForTest(now func() time.Time) { s.now = now }

// SetDocCipher installs the document cipher (Task 20.3.8 — client PDFs
// are AES-128 encrypted at render). Generation fails closed while nil.
func (s *StatementService) SetDocCipher(c *DocCipher) { s.docs = c }

// DocumentPIN returns the account's document-open password for the
// portal surface; empty when the cipher is unconfigured.
func (s *StatementService) DocumentPIN(accountID int64) string { return s.docs.PIN(accountID) }

// fundedAccounts returns every account that has ever held a wallet row or
// posted a ledger movement before end — the "per funded account" set.
func (s *StatementService) fundedAccounts(ctx context.Context, end time.Time) ([]int64, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT DISTINCT account_id FROM balances
		UNION
		SELECT DISTINCT account_id FROM ledger_entries WHERE posted_at < $1
		ORDER BY 1`, end)
	if err != nil {
		return nil, fmt.Errorf("statements: funded accounts: %w", err)
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// GenerateForAccount builds one account's statement for [start, end),
// asserts the ledger-conservation invariants, renders + stores PDF/CSV,
// and upserts the client_statements row (idempotent regenerate).
func (s *StatementService) GenerateForAccount(ctx context.Context, accountID int64, period StatementPeriod, start, end time.Time) (*StatementRow, *Statement, error) {
	st, err := s.assemble(ctx, accountID, period, start, end)
	if err != nil {
		return nil, nil, err
	}
	// Invariant 1 — per-currency opening + Σ movements == closing.
	for _, cs := range st.Currencies {
		if !cs.Opening.Add(cs.NetMove).Equal(cs.Closing) {
			return nil, nil, excerrors.New("LEDGER_IMBALANCE_ABORT", fmt.Sprintf(
				"statement account %d %s: opening %s + movements %s != closing %s",
				accountID, cs.Currency, cs.Opening, cs.NetMove, cs.Closing))
		}
	}
	// Invariant 2 — the GL journals behind the movements are zero-sum.
	journalIDs := map[int64]struct{}{}
	for _, cs := range st.Currencies {
		for _, m := range cs.Movements {
			if m.JournalID != nil {
				journalIDs[*m.JournalID] = struct{}{}
			}
		}
	}
	ids := make([]int64, 0, len(journalIDs))
	for id := range journalIDs {
		ids = append(ids, id)
	}
	if err := s.VerifyGLConservation(ctx, ids); err != nil {
		return nil, nil, err
	}

	pdf := RenderStatementPDF(st)
	csv := RenderStatementCSV(st)
	ref := fmt.Sprintf("statements/%d/%s/%s", accountID, period,
		start.UTC().Format("2006-01-02"))
	encPDF, err := s.docs.Encrypt(pdf, accountID, ref+".pdf")
	if err != nil {
		return nil, nil, fmt.Errorf("statements: encrypt %q: %w", ref, err)
	}
	if _, err := s.files.Put(ctx, ref+".pdf", encPDF, "application/pdf"); err != nil {
		return nil, nil, err
	}
	if _, err := s.files.Put(ctx, ref+".csv", csv, "text/csv"); err != nil {
		return nil, nil, err
	}
	sum := sha256.Sum256(encPDF)
	row := &StatementRow{
		AccountID: accountID, Period: period,
		PeriodStart: start, PeriodEnd: end, StatementType: "ACCOUNT",
		FileRef: ref, ContentSHA256: hex.EncodeToString(sum[:]),
		GeneratedAt: st.GeneratedAt,
	}
	err = s.pool.QueryRow(ctx, `
		INSERT INTO client_statements
		    (account_id, period, period_start, period_end, statement_type, file_ref, content_sha256)
		VALUES ($1, $2, $3, $4, 'ACCOUNT', $5, $6)
		ON CONFLICT (account_id, period, period_start, statement_type)
		DO UPDATE SET file_ref = EXCLUDED.file_ref,
		              content_sha256 = EXCLUDED.content_sha256,
		              generated_at = now()
		RETURNING statement_id`,
		accountID, string(period), start, end, ref, row.ContentSHA256).Scan(&row.StatementID)
	if err != nil {
		return nil, nil, fmt.Errorf("statements: persist row: %w", err)
	}
	return row, st, nil
}

// assemble loads the statement sections. All reads are plain committed
// snapshots — statements are built EOD over a closed window.
func (s *StatementService) assemble(ctx context.Context, accountID int64, period StatementPeriod, start, end time.Time) (*Statement, error) {
	st := &Statement{
		AccountID: accountID, Period: period,
		PeriodStart: start, PeriodEnd: end,
		GeneratedAt: s.now().UTC(),
	}

	opening, err := s.balanceAt(ctx, accountID, start)
	if err != nil {
		return nil, err
	}
	closing, err := s.balanceAt(ctx, accountID, end)
	if err != nil {
		return nil, err
	}
	movements, err := s.loadMovements(ctx, accountID, start, end)
	if err != nil {
		return nil, err
	}

	byCcy := map[string]*CurrencyStatement{}
	get := func(ccy string) *CurrencyStatement {
		if cs, ok := byCcy[ccy]; ok {
			return cs
		}
		cs := &CurrencyStatement{Currency: ccy}
		byCcy[ccy] = cs
		return cs
	}
	for ccy, b := range opening {
		get(ccy).Opening = b
	}
	for ccy, b := range closing {
		get(ccy).Closing = b
	}
	for _, m := range movements {
		cs := get(m.Currency)
		cs.Movements = append(cs.Movements, m)
		cs.NetMove = cs.NetMove.Add(m.SignedAmount())
	}
	for _, cs := range byCcy {
		st.Currencies = append(st.Currencies, *cs)
	}
	sort.Slice(st.Currencies, func(i, j int) bool {
		return st.Currencies[i].Currency < st.Currencies[j].Currency
	})

	if st.Trades, err = s.loadTrades(ctx, accountID, start, end); err != nil {
		return nil, err
	}
	if st.Pending, err = s.loadPending(ctx, accountID, start, end); err != nil {
		return nil, err
	}
	if st.Positions, err = s.loadPositions(ctx, accountID); err != nil {
		return nil, err
	}
	return st, nil
}

// balanceAt resolves the wallet balance per currency at instant t from the
// ledger_entries serial trail (running_balance of the last entry < t).
func (s *StatementService) balanceAt(ctx context.Context, accountID int64, t time.Time) (map[string]decimal.Decimal, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT DISTINCT ON (currency) currency, running_balance::text
		FROM ledger_entries
		WHERE account_id = $1 AND posted_at < $2
		ORDER BY currency, id DESC`, accountID, t)
	if err != nil {
		return nil, fmt.Errorf("statements: balance at %s: %w", t.Format(time.RFC3339), err)
	}
	defer rows.Close()
	out := map[string]decimal.Decimal{}
	for rows.Next() {
		var ccy, amt string
		if err := rows.Scan(&ccy, &amt); err != nil {
			return nil, err
		}
		d, err := decimal.NewFromString(amt)
		if err != nil {
			return nil, fmt.Errorf("statements: balance parse %q: %w", amt, err)
		}
		out[ccy] = d
	}
	return out, rows.Err()
}

func (s *StatementService) loadMovements(ctx context.Context, accountID int64, start, end time.Time) ([]LedgerMovement, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id, entry_type::text, direction::text, amount::text, currency,
		       COALESCE(description,''), journal_entry_id, posted_at
		FROM ledger_entries
		WHERE account_id = $1 AND posted_at >= $2 AND posted_at < $3
		ORDER BY id`, accountID, start, end)
	if err != nil {
		return nil, fmt.Errorf("statements: movements: %w", err)
	}
	defer rows.Close()
	var out []LedgerMovement
	for rows.Next() {
		var (
			m   LedgerMovement
			amt string
			jid *int64
		)
		if err := rows.Scan(&m.ID, &m.EntryType, &m.Direction, &amt,
			&m.Currency, &m.Description, &jid, &m.PostedAt); err != nil {
			return nil, err
		}
		if m.Amount, err = decimal.NewFromString(amt); err != nil {
			return nil, fmt.Errorf("statements: amount parse %q: %w", amt, err)
		}
		m.JournalID = jid
		out = append(out, m)
	}
	return out, rows.Err()
}

func (s *StatementService) loadTrades(ctx context.Context, accountID int64, start, end time.Time) ([]StatementTrade, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT t.id, i.symbol, t.price::text, t.quantity::text,
		       COALESCE(t.buyer_fee,0)::text, COALESCE(t.seller_fee,0)::text,
		       i.quote_currency, t.created_at,
		       CASE WHEN t.buyer_account_id = $1 THEN 'BUY' ELSE 'SELL' END
		FROM trades t
		JOIN instruments i ON i.id = t.instrument_id
		WHERE (t.buyer_account_id = $1 OR t.seller_account_id = $1)
		  AND t.created_at >= $2 AND t.created_at < $3
		ORDER BY t.id`, accountID, start, end)
	if err != nil {
		return nil, fmt.Errorf("statements: trades: %w", err)
	}
	defer rows.Close()
	var out []StatementTrade
	for rows.Next() {
		var (
			tr         StatementTrade
			price, qty string
			bfee, sfee string
		)
		if err := rows.Scan(&tr.TradeID, &tr.Symbol, &price, &qty,
			&bfee, &sfee, &tr.FeeCcy, &tr.ExecutedAt, &tr.Side); err != nil {
			return nil, err
		}
		var err2 error
		if tr.Price, err2 = decimal.NewFromString(price); err2 != nil {
			return nil, fmt.Errorf("statements: trade price: %w", err2)
		}
		if tr.Quantity, err2 = decimal.NewFromString(qty); err2 != nil {
			return nil, fmt.Errorf("statements: trade qty: %w", err2)
		}
		feeRaw := sfee
		if tr.Side == "BUY" {
			feeRaw = bfee
		}
		if tr.Fee, err2 = decimal.NewFromString(feeRaw); err2 != nil {
			return nil, fmt.Errorf("statements: trade fee: %w", err2)
		}
		out = append(out, tr)
	}
	return out, rows.Err()
}

// loadPending returns in-flight funding movements overlapping the window —
// flagged on the statement, never silently dropped (pending-settlement
// edge case; they are excluded from the posted movement totals by design:
// a PENDING funding transaction has not posted a wallet ledger entry).
func (s *StatementService) loadPending(ctx context.Context, accountID int64, start, end time.Time) ([]PendingMovement, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id, type::text, status::text, amount::text, currency, created_at
		FROM funding_transactions
		WHERE account_id = $1 AND created_at >= $2 AND created_at < $3
		  AND status IN ('PENDING','CONFIRMED','PENDING_REVIEW')
		ORDER BY id`, accountID, start, end)
	if err != nil {
		return nil, fmt.Errorf("statements: pending: %w", err)
	}
	defer rows.Close()
	var out []PendingMovement
	for rows.Next() {
		var p PendingMovement
		var amt string
		if err := rows.Scan(&p.FundingID, &p.Type, &p.Status, &amt, &p.Currency, &p.CreatedAt); err != nil {
			return nil, err
		}
		var err2 error
		if p.Amount, err2 = decimal.NewFromString(amt); err2 != nil {
			return nil, fmt.Errorf("statements: pending amount: %w", err2)
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

func (s *StatementService) loadPositions(ctx context.Context, accountID int64) ([]PositionPnl, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT i.symbol, p.side::text, p.quantity::text, p.unrealized_pnl::text,
		       i.quote_currency
		FROM positions p
		JOIN instruments i ON i.id = p.instrument_id
		WHERE p.account_id = $1 AND p.quantity > 0
		ORDER BY i.symbol`, accountID)
	if err != nil {
		return nil, fmt.Errorf("statements: positions: %w", err)
	}
	defer rows.Close()
	var out []PositionPnl
	for rows.Next() {
		var (
			p         PositionPnl
			qty, upnl string
		)
		if err := rows.Scan(&p.Symbol, &p.Side, &qty, &upnl, &p.Currency); err != nil {
			return nil, err
		}
		var err2 error
		if p.Quantity, err2 = decimal.NewFromString(qty); err2 != nil {
			return nil, fmt.Errorf("statements: position qty: %w", err2)
		}
		if p.UnrealizedPnl, err2 = decimal.NewFromString(upnl); err2 != nil {
			return nil, fmt.Errorf("statements: unrealized pnl: %w", err2)
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// VerifyGLConservation asserts the §5.21 zero-sum invariant across the
// supplied journal set: per currency, Σ debit_amount == Σ credit_amount
// over ledger_lines. An empty set trivially conserves. A divergence is a
// hard LEDGER_IMBALANCE_ABORT — statements never ride a broken journal.
func (s *StatementService) VerifyGLConservation(ctx context.Context, journalIDs []int64) error {
	if len(journalIDs) == 0 {
		return nil
	}
	sort.Slice(journalIDs, func(i, j int) bool { return journalIDs[i] < journalIDs[j] })
	rows, err := s.pool.Query(ctx, `
		SELECT currency, SUM(debit_amount)::text, SUM(credit_amount)::text
		FROM ledger_lines
		WHERE journal_entry_id = ANY($1)
		GROUP BY currency`, journalIDs)
	if err != nil {
		return fmt.Errorf("statements: gl conservation query: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var ccy, dSum, cSum string
		if err := rows.Scan(&ccy, &dSum, &cSum); err != nil {
			return err
		}
		d, err := decimal.NewFromString(dSum)
		if err != nil {
			return fmt.Errorf("statements: debit sum parse: %w", err)
		}
		c, err := decimal.NewFromString(cSum)
		if err != nil {
			return fmt.Errorf("statements: credit sum parse: %w", err)
		}
		if !d.Equal(c) {
			return excerrors.New("LEDGER_IMBALANCE_ABORT", fmt.Sprintf(
				"GL conservation breach in statement journals: %s debits %s != credits %s",
				ccy, d, c))
		}
	}
	return rows.Err()
}

// GenerateReport is the per-period run summary returned to the scheduler.
type GenerateReport struct {
	Period      StatementPeriod  `json:"period"`
	PeriodStart time.Time        `json:"period_start"`
	PeriodEnd   time.Time        `json:"period_end"`
	Generated   int              `json:"generated"`
	Failed      int              `json:"failed"`
	AccountErrs map[int64]string `json:"account_errors,omitempty"`
}

// GenerateDaily emits DAILY statements covering [day, day+1) UTC for
// every funded account. Per-account failures are collected (the healthy
// book still generates) and surfaced on the report; a nil error return
// requires Failed == 0.
func (s *StatementService) GenerateDaily(ctx context.Context, day time.Time) (GenerateReport, error) {
	start := normalizeUTCDate(day)
	return s.generateAll(ctx, StmtPeriodDaily, start, start.AddDate(0, 0, 1))
}

// GenerateMonthly emits MONTHLY statements covering the UTC calendar
// month containing `month` (normalized to its first day).
func (s *StatementService) GenerateMonthly(ctx context.Context, month time.Time) (GenerateReport, error) {
	start := normalizeUTCDate(month)
	start = time.Date(start.Year(), start.Month(), 1, 0, 0, 0, 0, time.UTC)
	return s.generateAll(ctx, StmtPeriodMonthly, start, start.AddDate(0, 1, 0))
}

func (s *StatementService) generateAll(ctx context.Context, period StatementPeriod, start, end time.Time) (GenerateReport, error) {
	rep := GenerateReport{Period: period, PeriodStart: start, PeriodEnd: end}
	accts, err := s.fundedAccounts(ctx, end)
	if err != nil {
		return rep, err
	}
	var firstErr error
	for _, id := range accts {
		if _, _, err := s.GenerateForAccount(ctx, id, period, start, end); err != nil {
			rep.Failed++
			if rep.AccountErrs == nil {
				rep.AccountErrs = map[int64]string{}
			}
			rep.AccountErrs[id] = err.Error()
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		rep.Generated++
	}
	return rep, firstErr
}

// List returns the account's statement rows newest-first with the
// (generated_at, statement_id) keyset — matching the §8.8 cursor
// convention (pagination.go). after is nil for the first page; limit is
// caller-clamped.
func (s *StatementService) List(ctx context.Context, accountID int64, period StatementPeriod, limit int, afterGen *time.Time, afterID int64) ([]StatementRow, int64, error) {
	args := []any{accountID}
	where := "WHERE account_id = $1"
	if period != "" {
		args = append(args, string(period))
		where += fmt.Sprintf(" AND period = $%d", len(args))
	}
	var total int64
	if err := s.pool.QueryRow(ctx,
		"SELECT count(*) FROM client_statements "+where, args...).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("statements: count: %w", err)
	}
	if afterGen != nil {
		args = append(args, *afterGen, afterID)
		where += fmt.Sprintf(" AND (generated_at, statement_id) < ($%d, $%d)",
			len(args)-1, len(args))
	}
	args = append(args, limit)
	rows, err := s.pool.Query(ctx, `
		SELECT statement_id, account_id, period::text, period_start, period_end,
		       statement_type, file_ref, content_sha256, generated_at
		FROM client_statements `+where+`
		ORDER BY generated_at DESC, statement_id DESC
		LIMIT `+fmt.Sprintf("$%d", len(args)), args...)
	if err != nil {
		return nil, 0, fmt.Errorf("statements: list: %w", err)
	}
	defer rows.Close()
	var out []StatementRow
	for rows.Next() {
		var r StatementRow
		var p string
		if err := rows.Scan(&r.StatementID, &r.AccountID, &p,
			&r.PeriodStart, &r.PeriodEnd, &r.StatementType,
			&r.FileRef, &r.ContentSHA256, &r.GeneratedAt); err != nil {
			return nil, 0, err
		}
		r.Period = StatementPeriod(p)
		out = append(out, r)
	}
	return out, total, rows.Err()
}

// Fetch resolves one statement document for the owning account. format is
// "pdf" or "csv" (file_ref carries the stem; the extension selects the
// rendering). Ownership is enforced — a foreign statement_id is NOT_FOUND
// (no existence oracle across accounts).
func (s *StatementService) Fetch(ctx context.Context, accountID, statementID int64, format string) (*StatementFile, error) {
	var row StatementRow
	var period string
	err := s.pool.QueryRow(ctx, `
		SELECT statement_id, account_id, period::text, period_start, period_end,
		       statement_type, file_ref, content_sha256, generated_at
		FROM client_statements
		WHERE statement_id = $1 AND account_id = $2`,
		statementID, accountID).Scan(&row.StatementID, &row.AccountID, &period,
		&row.PeriodStart, &row.PeriodEnd, &row.StatementType,
		&row.FileRef, &row.ContentSHA256, &row.GeneratedAt)
	if stderrors.Is(err, pgx.ErrNoRows) {
		return nil, excerrors.New("NOT_FOUND", "statement not found")
	}
	if err != nil {
		return nil, fmt.Errorf("statements: fetch row: %w", err)
	}
	row.Period = StatementPeriod(period)

	var ref, ct string
	switch strings.ToLower(format) {
	case "", "pdf":
		ref, ct = row.FileRef+".pdf", "application/pdf"
	case "csv":
		ref, ct = row.FileRef+".csv", "text/csv; charset=utf-8"
	default:
		return nil, excerrors.New("INVALID_REQUEST", "format must be pdf|csv")
	}
	body, err := s.files.Get(ctx, ref)
	if err != nil {
		return nil, fmt.Errorf("statements: fetch object: %w", err)
	}
	return &StatementFile{Body: body, ContentType: ct, FileRef: ref, Row: &row}, nil
}

// ---------------------------------------------------------------------------
// Shared scan coercion helpers — pgx/CH drivers may return int64, float64,
// or []byte numerics depending on path; coerce defensively. Package-level
// helpers shared with the sibling income projection (income.go).
// ---------------------------------------------------------------------------

func scanInt64(v any) (int64, error) {
	switch t := v.(type) {
	case nil:
		return 0, nil
	case int64:
		return t, nil
	case int32:
		return int64(t), nil
	case int:
		return int64(t), nil
	case float64:
		return int64(t), nil
	case []byte:
		return strconv.ParseInt(string(t), 10, 64)
	case string:
		return strconv.ParseInt(t, 10, 64)
	default:
		return 0, fmt.Errorf("scanInt64: unsupported %T", v)
	}
}

func scanTime(v any) (time.Time, error) {
	switch t := v.(type) {
	case nil:
		return time.Time{}, nil
	case time.Time:
		return t, nil
	case string:
		return time.Parse(time.RFC3339Nano, t)
	case []byte:
		return time.Parse(time.RFC3339Nano, string(t))
	default:
		return time.Time{}, fmt.Errorf("scanTime: unsupported %T", v)
	}
}

func scanDecimal(v any) (decimal.Decimal, error) {
	switch t := v.(type) {
	case nil:
		return decimal.Zero, nil
	case decimal.Decimal:
		return t, nil
	case float64:
		return decimal.NewFromFloat(t), nil
	case int64:
		return decimal.NewFromInt(t), nil
	case []byte:
		return decimal.NewFromString(string(t))
	case string:
		return decimal.NewFromString(t)
	default:
		return decimal.Zero, fmt.Errorf("scanDecimal: unsupported %T", v)
	}
}

// ---------------------------------------------------------------------------
// StatementJob — the scheduler seam (Task 20.3.6 item 2).
// ---------------------------------------------------------------------------

// StatementJob is the unit the orchestrator binds to the EOD scheduler:
//
//   - DAILY:   after the Tom-Next rollover run completes (rollover posts
//     the last GL movements of the trading day — statements run
//     strictly after it so closing balances include swaps);
//   - MONTHLY: on the 1st of each UTC month for the month just closed.
//
// RunOnce(now) covers both: it emits the previous UTC day's DAILY set
// and, when now's UTC day-of-month is 1, the previous month's MONTHLY
// set. Generation is idempotent (UNIQUE upsert), so overlapping
// invocations are safe.
type StatementJob struct {
	svc *StatementService
	now func() time.Time
}

// NewStatementJob wires the job around an existing service.
func NewStatementJob(svc *StatementService) *StatementJob {
	return &StatementJob{svc: svc, now: time.Now}
}

// SetClockForTest overrides the job clock; tests only.
func (j *StatementJob) SetClockForTest(now func() time.Time) { j.now = now }

// RunOnce executes the due generations for `now` and returns both reports.
func (j *StatementJob) RunOnce(ctx context.Context) (daily GenerateReport, monthly *GenerateReport, err error) {
	now := j.now().UTC()
	yesterday := normalizeUTCDate(now.AddDate(0, 0, -1))
	daily, err = j.svc.GenerateDaily(ctx, yesterday)
	if err != nil {
		return daily, nil, fmt.Errorf("statements job daily: %w", err)
	}
	if now.Day() == 1 {
		prev := normalizeUTCDate(now.AddDate(0, 0, -1))
		m, merr := j.svc.GenerateMonthly(ctx, prev)
		if merr != nil {
			return daily, nil, fmt.Errorf("statements job monthly: %w", merr)
		}
		monthly = &m
	}
	return daily, monthly, nil
}

// normalizeUTCDate truncates to the UTC calendar day.
func normalizeUTCDate(t time.Time) time.Time {
	u := t.UTC()
	return time.Date(u.Year(), u.Month(), u.Day(), 0, 0, 0, 0, time.UTC)
}

// ---------------------------------------------------------------------------
// ConfirmationService — per-fill trade confirmations (contract notes).
// ---------------------------------------------------------------------------

// Confirmation is the trade_confirmations row view.
type Confirmation struct {
	ConfirmationID int64      `json:"confirmation_id"`
	TradeID        int64      `json:"trade_id"`
	AccountID      int64      `json:"account_id"`
	Version        int        `json:"version"`
	Status         string     `json:"status"` // GENERATED|DELIVERED|ADJUSTED|FAILED
	FileRef        string     `json:"file_ref"`
	ContentSHA256  string     `json:"content_sha256"`
	GeneratedAt    time.Time  `json:"generated_at"`
	DeliveredAt    *time.Time `json:"delivered_at,omitempty"`
	SupersedesID   *int64     `json:"supersedes_id,omitempty"`
}

// ConfirmationFile is a downloaded confirmation document.
type ConfirmationFile struct {
	Body        []byte
	ContentType string
	FileRef     string
	Row         *Confirmation
}

// tradeDetail is the joined trades+instruments projection.
type tradeDetail struct {
	TradeID        int64
	Symbol         string
	QuoteCurrency  string
	Price          decimal.Decimal
	Quantity       decimal.Decimal
	BuyerFee       decimal.Decimal
	SellerFee      decimal.Decimal
	BuyerAccount   int64
	SellerAccount  int64
	SettlementDate *time.Time
	ExecutedAt     time.Time
}

// ConfirmationService generates, stores and serves per-fill contract
// notes. The ≤60s post-execution contract (MiFID II Art. 59) is met by
// the orchestrator invoking Generate synchronously on the fill-commit
// path (this call is sub-second: one read + one doc render + two row
// writes per counterparty side); the Task 20.3.8 delivery engine consumes
// the GENERATED rows and calls MarkDelivered.
type ConfirmationService struct {
	pool  *pgxpool.Pool
	files FileStore
	now   func() time.Time
	docs  *DocCipher
}

// NewConfirmationService wires the service; both dependencies required.
func NewConfirmationService(pool *pgxpool.Pool, files FileStore) (*ConfirmationService, error) {
	if pool == nil {
		return nil, fmt.Errorf("confirmations: nil pgx pool")
	}
	if files == nil {
		return nil, fmt.Errorf("confirmations: nil FileStore")
	}
	return &ConfirmationService{pool: pool, files: files, now: time.Now}, nil
}

// SetClockForTest overrides the clock; tests only.
func (s *ConfirmationService) SetClockForTest(now func() time.Time) { s.now = now }

// SetDocCipher installs the document cipher (Task 20.3.8 — the emailed
// confirmation PDF must be encrypted). Generation fails closed while nil.
func (s *ConfirmationService) SetDocCipher(c *DocCipher) { s.docs = c }

// loadTrade resolves the trade + instrument join. A missing trade is a
// coded NOT_FOUND (fail-closed — a confirmation must never guess).
func (s *ConfirmationService) loadTrade(ctx context.Context, tradeID int64) (*tradeDetail, error) {
	var (
		td         tradeDetail
		price, qty string
		bfee, sfee string
	)
	err := s.pool.QueryRow(ctx, `
		SELECT t.id, i.symbol, i.quote_currency, t.price::text, t.quantity::text,
		       COALESCE(t.buyer_fee,0)::text, COALESCE(t.seller_fee,0)::text,
		       t.buyer_account_id, t.seller_account_id, t.settlement_date, t.created_at
		FROM trades t
		JOIN instruments i ON i.id = t.instrument_id
		WHERE t.id = $1`, tradeID).Scan(
		&td.TradeID, &td.Symbol, &td.QuoteCurrency, &price, &qty,
		&bfee, &sfee, &td.BuyerAccount, &td.SellerAccount,
		&td.SettlementDate, &td.ExecutedAt)
	if stderrors.Is(err, pgx.ErrNoRows) {
		return nil, excerrors.New("NOT_FOUND", fmt.Sprintf("trade %d not found", tradeID))
	}
	if err != nil {
		return nil, fmt.Errorf("confirmations: load trade %d: %w", tradeID, err)
	}
	for label, dst := range map[string]*decimal.Decimal{
		"price": &td.Price, "quantity": &td.Quantity,
		"buyer_fee": &td.BuyerFee, "seller_fee": &td.SellerFee,
	} {
		raw := map[string]string{"price": price, "quantity": qty, "buyer_fee": bfee, "seller_fee": sfee}[label]
		d, err := decimal.NewFromString(raw)
		if err != nil {
			return nil, fmt.Errorf("confirmations: trade %s parse %q: %w", label, raw, err)
		}
		*dst = d
	}
	return &td, nil
}

// Generate issues the v1 confirmations for a fill — one row per
// counterparty account (buyer + seller). Idempotent: a re-invocation
// after a partial failure resumes at the next version number per
// (trade,account) rather than duplicating the same document.
func (s *ConfirmationService) Generate(ctx context.Context, tradeID int64) ([]Confirmation, error) {
	td, err := s.loadTrade(ctx, tradeID)
	if err != nil {
		return nil, err
	}
	var out []Confirmation
	for _, acct := range []int64{td.BuyerAccount, td.SellerAccount} {
		c, err := s.issue(ctx, td, acct, nil)
		if err != nil {
			return out, err
		}
		out = append(out, *c)
	}
	return out, nil
}

// MarkAdjusted implements the busted/price-adjusted-trade edge case (spec
// §5.29): the standing confirmation rows for the trade flip to ADJUSTED
// and a version+1 amended confirmation is issued per account
// (supersedes_id links the chain — the superseded row is retained for
// the MiFID record trail, never deleted).
func (s *ConfirmationService) MarkAdjusted(ctx context.Context, tradeID int64) ([]Confirmation, error) {
	td, err := s.loadTrade(ctx, tradeID)
	if err != nil {
		return nil, err
	}
	var out []Confirmation
	for _, acct := range []int64{td.BuyerAccount, td.SellerAccount} {
		var prevID int64
		err := s.pool.QueryRow(ctx, `
			SELECT confirmation_id FROM trade_confirmations
			WHERE trade_id = $1 AND account_id = $2
			ORDER BY version DESC LIMIT 1`, tradeID, acct).Scan(&prevID)
		if stderrors.Is(err, pgx.ErrNoRows) {
			// No prior confirmation (bust before the 60s generation
			// window) — still issue the v1 doc marked adjusted.
			prevID = 0
		} else if err != nil {
			return out, fmt.Errorf("confirmations: prior row: %w", err)
		}
		if prevID > 0 {
			if _, err := s.pool.Exec(ctx, `
				UPDATE trade_confirmations SET status = 'ADJUSTED'
				WHERE trade_id = $1 AND account_id = $2 AND status <> 'ADJUSTED'`,
				tradeID, acct); err != nil {
				return out, fmt.Errorf("confirmations: mark adjusted: %w", err)
			}
		}
		c, err := s.issue(ctx, td, acct, &prevID)
		if err != nil {
			return out, err
		}
		out = append(out, *c)
	}
	return out, nil
}

// issue renders + stores one account's confirmation and inserts the row.
// supersedes carries the prior confirmation id for amended issues
// (nil/zero = original v1 document).
func (s *ConfirmationService) issue(ctx context.Context, td *tradeDetail, accountID int64, supersedes *int64) (*Confirmation, error) {
	var version int
	err := s.pool.QueryRow(ctx, `
		SELECT COALESCE(MAX(version),0)+1 FROM trade_confirmations
		WHERE trade_id = $1 AND account_id = $2`,
		td.TradeID, accountID).Scan(&version)
	if err != nil {
		return nil, fmt.Errorf("confirmations: next version: %w", err)
	}
	adjusted := supersedes != nil && *supersedes > 0
	ref := fmt.Sprintf("confirmations/%d/%d/v%d", accountID, td.TradeID, version)
	doc := renderConfirmationJSON(td, accountID, version, adjusted, s.now().UTC())
	pdf := RenderConfirmationPDF(td, accountID, version, adjusted, s.now().UTC())
	encPDF, err := s.docs.Encrypt(pdf, accountID, ref+".pdf")
	if err != nil {
		return nil, fmt.Errorf("confirmations: encrypt %q: %w", ref, err)
	}
	if _, err := s.files.Put(ctx, ref+".json", doc, "application/json"); err != nil {
		return nil, err
	}
	if _, err := s.files.Put(ctx, ref+".pdf", encPDF, "application/pdf"); err != nil {
		return nil, err
	}
	sum := sha256.Sum256(encPDF)
	c := &Confirmation{
		TradeID: td.TradeID, AccountID: accountID, Version: version,
		Status: "GENERATED", FileRef: ref,
		ContentSHA256: hex.EncodeToString(sum[:]),
		GeneratedAt:   s.now().UTC(),
	}
	var sid *int64
	if adjusted {
		sid = supersedes
	}
	err = s.pool.QueryRow(ctx, `
		INSERT INTO trade_confirmations
		    (trade_id, account_id, version, status, file_ref, content_sha256, supersedes_id)
		VALUES ($1, $2, $3, 'GENERATED', $4, $5, $6)
		RETURNING confirmation_id`,
		td.TradeID, accountID, version, ref, c.ContentSHA256, sid).Scan(&c.ConfirmationID)
	if err != nil {
		return nil, fmt.Errorf("confirmations: insert row: %w", err)
	}
	c.SupersedesID = sid
	return c, nil
}

// MarkDelivered stamps delivered_at — invoked by the Task 20.3.8 delivery
// engine after a channel send succeeds (portal fetch does not count as
// delivery; the engine owns that semantic).
func (s *ConfirmationService) MarkDelivered(ctx context.Context, confirmationID int64) error {
	tag, err := s.pool.Exec(ctx, `
		UPDATE trade_confirmations
		SET status = 'DELIVERED', delivered_at = now()
		WHERE confirmation_id = $1 AND status = 'GENERATED'`, confirmationID)
	if err != nil {
		return fmt.Errorf("confirmations: mark delivered: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return excerrors.New("NOT_FOUND",
			"confirmation not found or not in GENERATED state")
	}
	return nil
}

// Get returns the latest confirmation metadata for (trade, account) —
// the caller's own side only (foreign trade_ids are NOT_FOUND).
func (s *ConfirmationService) Get(ctx context.Context, accountID, tradeID int64) (*Confirmation, error) {
	var (
		c   Confirmation
		sid *int64
	)
	err := s.pool.QueryRow(ctx, `
		SELECT confirmation_id, trade_id, account_id, version, status::text,
		       file_ref, content_sha256, generated_at, delivered_at, supersedes_id
		FROM trade_confirmations
		WHERE trade_id = $1 AND account_id = $2
		ORDER BY version DESC LIMIT 1`, tradeID, accountID).Scan(
		&c.ConfirmationID, &c.TradeID, &c.AccountID, &c.Version, &c.Status,
		&c.FileRef, &c.ContentSHA256, &c.GeneratedAt, &c.DeliveredAt, &sid)
	if stderrors.Is(err, pgx.ErrNoRows) {
		return nil, excerrors.New("NOT_FOUND", "confirmation not found")
	}
	if err != nil {
		return nil, fmt.Errorf("confirmations: get: %w", err)
	}
	c.SupersedesID = sid
	return &c, nil
}

// GetFile fetches the stored document for the latest confirmation:
// format json (default metadata document) or pdf.
func (s *ConfirmationService) GetFile(ctx context.Context, accountID, tradeID int64, format string) (*ConfirmationFile, error) {
	c, err := s.Get(ctx, accountID, tradeID)
	if err != nil {
		return nil, err
	}
	var ref, ct string
	switch strings.ToLower(format) {
	case "", "json":
		ref, ct = c.FileRef+".json", "application/json"
	case "pdf":
		ref, ct = c.FileRef+".pdf", "application/pdf"
	default:
		return nil, excerrors.New("INVALID_REQUEST", "format must be json|pdf")
	}
	body, err := s.files.Get(ctx, ref)
	if err != nil {
		return nil, fmt.Errorf("confirmations: fetch object: %w", err)
	}
	return &ConfirmationFile{Body: body, ContentType: ct, FileRef: ref, Row: c}, nil
}

// ---------------------------------------------------------------------------
// Rendering — CSV + minimal dependency-free PDF.
//
// The PDF writer replicates the internal/tax/render.go RenderPDF pattern
// (PDF-1.4, Courier Type1, 612×792 letter pages). The generic page
// packer here — renderTextPDF/docLine — is shared by statements,
// confirmations, invoices and the trial-balance export inside this
// package. internal/api/pdfdoc.go.RenderPDFDoc is the equivalent helper
// for handler-side rendering; analytics renders at generation time and
// must not import internal/api (layering).
// ---------------------------------------------------------------------------

// docLine is one text row on a generated-document page.
type docLine struct {
	text string
	size int
}

// renderTextPDF assembles a valid PDF-1.4 document: title + lines packed
// at 44 rows/page.
func renderTextPDF(title string, lines []docLine) []byte {
	const perPage = 44
	pages := [][]docLine{}
	for len(lines) > 0 {
		n := perPage
		if len(lines) < n {
			n = len(lines)
		}
		pages = append(pages, lines[:n])
		lines = lines[n:]
	}
	if len(pages) == 0 {
		pages = [][]docLine{{{text: title, size: 16}}}
	}
	var out bytes.Buffer
	offsets := []int{}
	obj := func(id int, body string) {
		offsets = append(offsets, out.Len())
		fmt.Fprintf(&out, "%d 0 obj\n%s\nendobj\n", id, body)
	}
	out.WriteString("%PDF-1.4\n")
	n := len(pages)
	kids := ""
	for i := 0; i < n; i++ {
		kids += fmt.Sprintf("%d 0 R ", 4+i*2)
	}
	obj(1, "<< /Type /Catalog /Pages 2 0 R >>")
	obj(2, fmt.Sprintf("<< /Type /Pages /Kids [%s] /Count %d >>", kids, n))
	obj(3, "<< /Type /Font /Subtype /Type1 /BaseFont /Courier >>")
	for i, pg := range pages {
		contentID := 4 + i*2 + 1
		obj(4+i*2, fmt.Sprintf(
			"<< /Type /Page /Parent 2 0 R /MediaBox [0 0 612 792] "+
				"/Resources << /Font << /F1 3 0 R >> >> /Contents %d 0 R >>",
			contentID))
		var stream bytes.Buffer
		stream.WriteString("BT\n40 750 Td\n")
		for _, l := range pg {
			sz := l.size
			if sz == 0 {
				sz = 9
			}
			fmt.Fprintf(&stream, "/F1 %d Tf (%s) Tj 0 -%d Td\n",
				sz, pdfEsc(l.text), sz+6)
		}
		stream.WriteString("ET")
		obj(contentID, fmt.Sprintf("<< /Length %d >>\nstream\n%s\nendstream",
			stream.Len(), stream.String()))
	}
	xref := out.Len()
	fmt.Fprintf(&out, "xref\n0 %d\n0000000000 65535 f \n", 4+2*n)
	for _, off := range offsets {
		fmt.Fprintf(&out, "%010d 00000 n \n", off)
	}
	fmt.Fprintf(&out, "trailer\n<< /Size %d /Root 1 0 R >>\nstartxref\n%d\n%%%%EOF\n",
		4+2*n, xref)
	return out.Bytes()
}

// pdfEsc escapes PDF literal-string specials; non-ASCII is dropped
// (standard-14 Courier encoding — documents emit ASCII only).
func pdfEsc(s string) string {
	var out []byte
	for _, c := range s {
		switch c {
		case '\\':
			out = append(out, `\\`...)
		case '(':
			out = append(out, `\(`...)
		case ')':
			out = append(out, `\)`...)
		default:
			if c < 128 {
				out = append(out, byte(c))
			}
		}
	}
	return string(out)
}

// csvq quotes a CSV field containing separators/quotes/newlines.
func csvq(s string) string {
	if strings.ContainsAny(s, ",\"\n") {
		return `"` + strings.ReplaceAll(s, `"`, `""`) + `"`
	}
	return s
}

func trunc24(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max]
}

// RenderStatementCSV renders the statement as fixed-point CSV — header
// block, per-currency balance/movement sections, trades, pending
// movements (flagged, outside the totals), open positions.
func RenderStatementCSV(st *Statement) []byte {
	var b bytes.Buffer
	fmt.Fprintf(&b, "report,statement\n")
	fmt.Fprintf(&b, "account_id,%d\n", st.AccountID)
	fmt.Fprintf(&b, "period,%s\nperiod_start,%s\nperiod_end,%s\n",
		st.Period, st.PeriodStart.Format("2006-01-02"), st.PeriodEnd.Format("2006-01-02"))
	fmt.Fprintf(&b, "generated_at,%s\n\n", st.GeneratedAt.UTC().Format(time.RFC3339))

	b.WriteString("currency,opening_balance,net_movement,closing_balance\n")
	for _, cs := range st.Currencies {
		fmt.Fprintf(&b, "%s,%s,%s,%s\n", cs.Currency,
			cs.Opening.StringFixed(8), cs.NetMove.StringFixed(8), cs.Closing.StringFixed(8))
	}
	b.WriteString("\nmovements: id,entry_type,direction,amount,currency,posted_at,journal_id,description\n")
	for _, cs := range st.Currencies {
		for _, m := range cs.Movements {
			jid := ""
			if m.JournalID != nil {
				jid = fmt.Sprintf("%d", *m.JournalID)
			}
			fmt.Fprintf(&b, "%d,%s,%s,%s,%s,%s,%s,%s\n",
				m.ID, m.EntryType, m.Direction, m.Amount.StringFixed(8),
				m.Currency, m.PostedAt.UTC().Format(time.RFC3339), jid, csvq(m.Description))
		}
	}
	b.WriteString("\ntrades: trade_id,symbol,side,quantity,price,fee,fee_currency,executed_at\n")
	for _, t := range st.Trades {
		fmt.Fprintf(&b, "%d,%s,%s,%s,%s,%s,%s,%s\n",
			t.TradeID, t.Symbol, t.Side, t.Quantity.StringFixed(8),
			t.Price.StringFixed(8), t.Fee.StringFixed(8), t.FeeCcy,
			t.ExecutedAt.UTC().Format(time.RFC3339))
	}
	if len(st.Pending) > 0 {
		b.WriteString("\npending_movements (FLAGGED — in-flight, excluded from totals): funding_id,type,status,amount,currency,created_at\n")
		for _, p := range st.Pending {
			fmt.Fprintf(&b, "%d,%s,%s,%s,%s,%s\n",
				p.FundingID, p.Type, p.Status, p.Amount.StringFixed(8),
				p.Currency, p.CreatedAt.UTC().Format(time.RFC3339))
		}
	}
	if len(st.Positions) > 0 {
		b.WriteString("\nopen_positions: symbol,side,quantity,unrealized_pnl,currency\n")
		for _, p := range st.Positions {
			fmt.Fprintf(&b, "%s,%s,%s,%s,%s\n",
				p.Symbol, p.Side, p.Quantity.StringFixed(8),
				p.UnrealizedPnl.StringFixed(8), p.Currency)
		}
	}
	return b.Bytes()
}

// RenderStatementPDF renders the PDF statement document.
func RenderStatementPDF(st *Statement) []byte {
	title := fmt.Sprintf("Exchange — %s Account Statement", st.Period)
	lines := []docLine{
		{title, 14},
		{fmt.Sprintf("Account %d   Period %s to %s (UTC, end-exclusive)",
			st.AccountID, st.PeriodStart.Format("2006-01-02"),
			st.PeriodEnd.Format("2006-01-02")), 10},
		{fmt.Sprintf("Generated %s UTC",
			st.GeneratedAt.UTC().Format("2006-01-02 15:04:05")), 9},
		{" ", 8},
		{fmt.Sprintf("%-8s %16s %16s %16s", "Currency", "Opening", "NetMovement", "Closing"), 10},
	}
	for _, cs := range st.Currencies {
		lines = append(lines, docLine{fmt.Sprintf("%-8s %16s %16s %16s",
			cs.Currency, cs.Opening.StringFixed(2), cs.NetMove.StringFixed(2),
			cs.Closing.StringFixed(2)), 10})
	}
	if len(st.Currencies) == 0 {
		lines = append(lines, docLine{"(no ledger activity in period)", 9})
	}
	lines = append(lines, docLine{" ", 8})
	if len(st.Trades) > 0 {
		lines = append(lines, docLine{"Trades", 11})
		lines = append(lines, docLine{fmt.Sprintf("%-10s %-8s %-4s %12s %12s %12s %-4s",
			"TradeID", "Symbol", "Side", "Qty", "Price", "Fee", "Ccy"), 9})
		for _, t := range st.Trades {
			lines = append(lines, docLine{fmt.Sprintf("%-10d %-8s %-4s %12s %12s %12s %-4s",
				t.TradeID, trunc24(t.Symbol, 8), t.Side,
				t.Quantity.StringFixed(4), t.Price.StringFixed(8),
				t.Fee.StringFixed(4), t.FeeCcy), 9})
		}
	}
	if len(st.Pending) > 0 {
		lines = append(lines, docLine{" ", 8})
		lines = append(lines, docLine{"Pending movements (in-flight — excluded from posted totals)", 10})
		for _, p := range st.Pending {
			lines = append(lines, docLine{fmt.Sprintf("  #%d %s %s %s %s",
				p.FundingID, p.Type, p.Status, p.Amount.StringFixed(8), p.Currency), 9})
		}
	}
	if len(st.Positions) > 0 {
		lines = append(lines, docLine{" ", 8})
		lines = append(lines, docLine{"Open positions (mark-to-market)", 11})
		for _, p := range st.Positions {
			lines = append(lines, docLine{fmt.Sprintf("  %-10s %-5s qty=%s  uPnL=%s %s",
				trunc24(p.Symbol, 10), p.Side, p.Quantity.StringFixed(4),
				p.UnrealizedPnl.StringFixed(2), p.Currency), 9})
		}
	}
	return renderTextPDF(title, lines)
}

// renderConfirmationJSON is the machine-readable contract-note document
// stored alongside the PDF rendering.
func renderConfirmationJSON(td *tradeDetail, accountID int64, version int, adjusted bool, at time.Time) []byte {
	side := "BUY"
	fee := td.BuyerFee
	if accountID == td.SellerAccount {
		side, fee = "SELL", td.SellerFee
	}
	settle := ""
	if td.SettlementDate != nil {
		settle = td.SettlementDate.Format("2006-01-02")
	}
	return []byte(fmt.Sprintf(`{"type":"trade_confirmation","version":%d,"adjusted":%t,`+
		`"trade_id":%d,"account_id":%d,"symbol":%q,"side":%q,`+
		`"price":%q,"quantity":%q,"fee":%q,"fee_currency":%q,`+
		`"settlement_date":%q,"executed_at":%q,"generated_at":%q,"venue":"EXCHANGE"}`,
		version, adjusted, td.TradeID, accountID, td.Symbol, side,
		td.Price.StringFixed(8), td.Quantity.StringFixed(8),
		fee.StringFixed(8), td.QuoteCurrency, settle,
		td.ExecutedAt.UTC().Format(time.RFC3339), at.Format(time.RFC3339)))
}

// RenderConfirmationPDF renders the contract-note PDF (spec §5.28 —
// instrument, side, quantity, price, fee, settlement date, execution
// timestamp; the AMENDED banner on busted/price-adjusted reissues).
func RenderConfirmationPDF(td *tradeDetail, accountID int64, version int, adjusted bool, at time.Time) []byte {
	side := "BUY"
	fee := td.BuyerFee
	if accountID == td.SellerAccount {
		side, fee = "SELL", td.SellerFee
	}
	title := "Exchange — Trade Confirmation (Contract Note)"
	lines := []docLine{{title, 14}}
	if adjusted {
		lines = append(lines, docLine{
			"AMENDED CONFIRMATION — the original note was voided by a trade bust/price-adjust (spec s5.29)", 10})
	}
	settle := "T+1"
	if td.SettlementDate != nil {
		settle = td.SettlementDate.Format("2006-01-02")
	}
	lines = append(lines,
		docLine{fmt.Sprintf("Confirmation v%d   Account %d   Trade %d", version, accountID, td.TradeID), 11},
		docLine{" ", 8},
		docLine{fmt.Sprintf("%-18s %s", "Instrument:", td.Symbol), 10},
		docLine{fmt.Sprintf("%-18s %s", "Side:", side), 10},
		docLine{fmt.Sprintf("%-18s %s", "Quantity:", td.Quantity.StringFixed(8)), 10},
		docLine{fmt.Sprintf("%-18s %s", "Price:", td.Price.StringFixed(8)), 10},
		docLine{fmt.Sprintf("%-18s %s %s", "Fee:", fee.StringFixed(8), td.QuoteCurrency), 10},
		docLine{fmt.Sprintf("%-18s %s", "Settlement date:", settle), 10},
		docLine{fmt.Sprintf("%-18s %s UTC", "Executed at:", td.ExecutedAt.UTC().Format("2006-01-02 15:04:05.000")), 10},
		docLine{fmt.Sprintf("%-18s %s", "Venue:", "EXCHANGE"), 10},
		docLine{fmt.Sprintf("%-18s %s UTC", "Generated at:", at.Format("2006-01-02 15:04:05")), 10},
	)
	return renderTextPDF(title, lines)
}
