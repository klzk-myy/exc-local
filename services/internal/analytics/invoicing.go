// invoicing.go — Phase-20 Task 20.3.6 item 5: monthly institutional fee
// invoices (spec §16.5) over the GL/ledger records.
//
// Invoice composition per (account, currency, month):
//
//	trading_fees      — wallet-side FEE ledger_entries credits (fees are
//	                    value OUT of the client wallet) inside the month;
//	                    reconciled to GL 4010_TRADING_FEE_REVENUE /
//	                    4030_COMMISSION_REVENUE credits on the same
//	                    journal set before the invoice is issued.
//	mm_rebates        — mm_rebate_accruals summed over the month
//	                    (migration 045). A SUSPENDED program accrues
//	                    nothing → the rebate line is simply 0; the
//	                    invoice still issues (required edge case).
//	connectivity_fees — FIX/session connectivity charges via the
//	                    ConnectivityFeeSource seam (nil → 0).
//	total             — trading_fees + connectivity_fees − mm_rebates.
//
// Only institutional accounts are invoiced (client_category
// PROFESSIONAL / ELIGIBLE_COUNTERPARTY — migration 042); RETAIL fee
// visibility rides the statements instead.
package analytics

import (
	"bytes"
	"context"
	"crypto/sha256"
	stderrors "errors"
	"fmt"
	"sort"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"exchange/pkg/decimal"
	excerrors "exchange/pkg/errors"
)

// ConnectivityFeeSource resolves the monthly connectivity charge per
// institutional account per currency (FIX session fees, dedicated
// lines). nil on the service means "no connectivity fees" — never a
// hidden default.
type ConnectivityFeeSource interface {
	MonthlyConnectivityFee(ctx context.Context, accountID int64, month time.Time) (map[string]decimal.Decimal, error)
}

// Invoice is one fee_invoices row view.
type Invoice struct {
	InvoiceID        int64           `json:"invoice_id"`
	AccountID        int64           `json:"account_id"`
	Month            time.Time       `json:"month"` // first day of the invoice month, UTC
	Currency         string          `json:"currency"`
	TradingFees      decimal.Decimal `json:"trading_fees"`
	MMRebates        decimal.Decimal `json:"mm_rebates"`
	ConnectivityFees decimal.Decimal `json:"connectivity_fees"`
	Total            decimal.Decimal `json:"total"`
	Status           string          `json:"status"`
	FileRef          string          `json:"file_ref,omitempty"`
	ContentSHA256    string          `json:"content_sha256,omitempty"`
	GeneratedAt      time.Time       `json:"generated_at"`
}

// InvoiceReport is the monthly generation run summary.
type InvoiceReport struct {
	Month    time.Time        `json:"month"`
	Issued   int              `json:"issued"`
	Failed   int              `json:"failed"`
	AcctErrs map[int64]string `json:"account_errors,omitempty"`
}

// InvoiceService issues monthly institutional invoices.
type InvoiceService struct {
	pool     *pgxpool.Pool
	files    FileStore
	connFees ConnectivityFeeSource // optional; nil = zero connectivity leg
	now      func() time.Time
	docs     *DocCipher
}

// NewInvoiceService wires the service. files may be nil — invoices then
// persist rows only (file_ref NULL); pass a store for the full render.
func NewInvoiceService(pool *pgxpool.Pool, files FileStore, connFees ConnectivityFeeSource) (*InvoiceService, error) {
	if pool == nil {
		return nil, fmt.Errorf("invoicing: nil pgx pool")
	}
	return &InvoiceService{pool: pool, files: files, connFees: connFees, now: time.Now}, nil
}

// SetClockForTest overrides the clock; tests only.
func (s *InvoiceService) SetClockForTest(now func() time.Time) { s.now = now }

// SetDocCipher installs the document cipher (Task 20.3.8 — archived
// invoices are client-facing PDFs). Rendering fails closed while nil
// whenever a FileStore is configured.
func (s *InvoiceService) SetDocCipher(c *DocCipher) { s.docs = c }

// institutionalAccounts lists accounts eligible for invoicing —
// PROFESSIONAL / ELIGIBLE_COUNTERPARTY per migration 042.
func (s *InvoiceService) institutionalAccounts(ctx context.Context) ([]int64, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id FROM accounts
		WHERE client_category IN ('PROFESSIONAL','ELIGIBLE_COUNTERPARTY')
		  AND status <> 'CLOSED'
		ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("invoicing: institutional accounts: %w", err)
	}
	defer rows.Close()
	var out []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// tradingFees sums the account's FEE ledger_entries credits inside
// [start,end) per currency and returns the backing journal ids for the
// GL reconciliation check.
func (s *InvoiceService) tradingFees(ctx context.Context, accountID int64, start, end time.Time) (map[string]decimal.Decimal, []int64, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT currency, SUM(amount)::text, array_agg(DISTINCT journal_entry_id)
		FROM ledger_entries
		WHERE account_id = $1 AND entry_type = 'FEE' AND direction = 'CREDIT'
		  AND posted_at >= $2 AND posted_at < $3
		GROUP BY currency`, accountID, start, end)
	if err != nil {
		return nil, nil, fmt.Errorf("invoicing: trading fees: %w", err)
	}
	defer rows.Close()
	fees := map[string]decimal.Decimal{}
	jset := map[int64]struct{}{}
	for rows.Next() {
		var (
			ccy, sum string
			jids     []int64
		)
		if err := rows.Scan(&ccy, &sum, &jids); err != nil {
			return nil, nil, err
		}
		d, err := decimal.NewFromString(sum)
		if err != nil {
			return nil, nil, fmt.Errorf("invoicing: fee sum parse: %w", err)
		}
		fees[ccy] = d
		for _, j := range jids {
			jset[j] = struct{}{}
		}
	}
	journalIDs := make([]int64, 0, len(jset))
	for j := range jset {
		journalIDs = append(journalIDs, j)
	}
	sort.Slice(journalIDs, func(i, j int) bool { return journalIDs[i] < journalIDs[j] })
	return fees, journalIDs, rows.Err()
}

// verifyFeeGL reconciles the wallet-side fee total against the GL
// revenue side of the same journals: per currency, the client's FEE
// credits must equal the credits posted to 4010_TRADING_FEE_REVENUE +
// 4030_COMMISSION_REVENUE on those journals. A divergence means the fee
// pipeline under/over-posted revenue — the invoice is refused, never
// issued "best effort" (fail-closed, spec §2.7).
func (s *InvoiceService) verifyFeeGL(ctx context.Context, journalIDs []int64, fees map[string]decimal.Decimal) error {
	if len(journalIDs) == 0 {
		return nil
	}
	rows, err := s.pool.Query(ctx, `
		SELECT ll.currency, SUM(ll.credit_amount)::text
		FROM ledger_lines ll
		WHERE ll.journal_entry_id = ANY($1)
		  AND (ll.account_code LIKE '4010\_TRADING\_FEE\_REVENUE\_%'
		       OR ll.account_code LIKE '4030\_COMMISSION\_REVENUE\_%')
		GROUP BY ll.currency`, journalIDs)
	if err != nil {
		return fmt.Errorf("invoicing: gl fee check: %w", err)
	}
	defer rows.Close()
	gl := map[string]decimal.Decimal{}
	for rows.Next() {
		var ccy, sum string
		if err := rows.Scan(&ccy, &sum); err != nil {
			return err
		}
		d, err := decimal.NewFromString(sum)
		if err != nil {
			return fmt.Errorf("invoicing: gl sum parse: %w", err)
		}
		gl[ccy] = d
	}
	if err := rows.Err(); err != nil {
		return err
	}
	for ccy, fee := range fees {
		rev, ok := gl[ccy]
		if !ok {
			return excerrors.New("LEDGER_IMBALANCE_ABORT", fmt.Sprintf(
				"invoice: %s client fees %s posted with no GL revenue leg", ccy, fee))
		}
		if !fee.Equal(rev) {
			return excerrors.New("LEDGER_IMBALANCE_ABORT", fmt.Sprintf(
				"invoice: %s client fees %s != GL revenue credits %s", ccy, fee, rev))
		}
	}
	return nil
}

// mmRebates sums accrued maker rebates for the month. Suspended MM
// programs accrue nothing while suspended (AccrueRebate gates on ACTIVE)
// — the edge case resolves to a 0 line here; the invoice still issues.
func (s *InvoiceService) mmRebates(ctx context.Context, accountID int64, start, end time.Time) (map[string]decimal.Decimal, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT currency, SUM(amount)::text
		FROM mm_rebate_accruals
		WHERE account_id = $1 AND day >= $2 AND day < $3
		GROUP BY currency`, accountID, start, end)
	if err != nil {
		return nil, fmt.Errorf("invoicing: mm rebates: %w", err)
	}
	defer rows.Close()
	out := map[string]decimal.Decimal{}
	for rows.Next() {
		var ccy, sum string
		if err := rows.Scan(&ccy, &sum); err != nil {
			return nil, err
		}
		d, err := decimal.NewFromString(sum)
		if err != nil {
			return nil, fmt.Errorf("invoicing: rebate sum parse: %w", err)
		}
		out[ccy] = d
	}
	return out, rows.Err()
}

// GenerateMonthly issues invoices for the UTC calendar month containing
// `month` to every institutional account. Currencies with all-zero
// components emit no row (a zero invoice carries no receivable).
func (s *InvoiceService) GenerateMonthly(ctx context.Context, month time.Time) (InvoiceReport, error) {
	start := normalizeUTCDate(month)
	start = time.Date(start.Year(), start.Month(), 1, 0, 0, 0, 0, time.UTC)
	end := start.AddDate(0, 1, 0)
	rep := InvoiceReport{Month: start}

	accts, err := s.institutionalAccounts(ctx)
	if err != nil {
		return rep, err
	}
	var firstErr error
	for _, acct := range accts {
		if err := s.generateForAccount(ctx, acct, start, end); err != nil {
			rep.Failed++
			if rep.AcctErrs == nil {
				rep.AcctErrs = map[int64]string{}
			}
			rep.AcctErrs[acct] = err.Error()
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
	}
	// count issued rows for the report (even on partial failure —
	// AcctErrs carries the per-account detail)
	if err := s.pool.QueryRow(ctx,
		`SELECT count(*) FROM fee_invoices WHERE month = $1`, start).Scan(&rep.Issued); err != nil {
		return rep, fmt.Errorf("invoicing: issued count: %w", err)
	}
	return rep, firstErr
}

// generateForAccount builds + persists one account's invoice rows.
func (s *InvoiceService) generateForAccount(ctx context.Context, accountID int64, start, end time.Time) error {
	fees, journalIDs, err := s.tradingFees(ctx, accountID, start, end)
	if err != nil {
		return err
	}
	if err := s.verifyFeeGL(ctx, journalIDs, fees); err != nil {
		return err
	}
	rebates, err := s.mmRebates(ctx, accountID, start, end)
	if err != nil {
		return err
	}
	var conn map[string]decimal.Decimal
	if s.connFees != nil {
		conn, err = s.connFees.MonthlyConnectivityFee(ctx, accountID, start)
		if err != nil {
			return fmt.Errorf("invoicing: connectivity fees: %w", err)
		}
	}
	ccys := map[string]struct{}{}
	for c := range fees {
		ccys[c] = struct{}{}
	}
	for c := range rebates {
		ccys[c] = struct{}{}
	}
	for c := range conn {
		ccys[c] = struct{}{}
	}
	sorted := make([]string, 0, len(ccys))
	for c := range ccys {
		sorted = append(sorted, c)
	}
	sort.Strings(sorted)

	for _, ccy := range sorted {
		fee := fees[ccy]
		reb := rebates[ccy]
		cf := conn[ccy]
		total := fee.Add(cf).Sub(reb)
		if total.IsZero() {
			continue // no receivable — no invoice row
		}
		inv := Invoice{
			AccountID: accountID, Month: start, Currency: ccy,
			TradingFees: fee, MMRebates: reb, ConnectivityFees: cf,
			Total: total, Status: "ISSUED", GeneratedAt: s.now().UTC(),
		}
		var ref, sumHex string
		if s.files != nil {
			ref = fmt.Sprintf("invoices/%d/%s/%s", accountID,
				start.Format("2006-01"), ccy)
			pdf := renderInvoicePDF(&inv)
			encPDF, err := s.docs.Encrypt(pdf, accountID, ref+".pdf")
			if err != nil {
				return fmt.Errorf("invoices: encrypt %q: %w", ref, err)
			}
			if _, err := s.files.Put(ctx, ref+".pdf", encPDF, "application/pdf"); err != nil {
				return err
			}
			if _, err := s.files.Put(ctx, ref+".csv", renderInvoiceCSV(&inv), "text/csv"); err != nil {
				return err
			}
			sum := sha256.Sum256(encPDF)
			sumHex = fmt.Sprintf("%x", sum)
			inv.FileRef, inv.ContentSHA256 = ref, sumHex
		}
		if _, err := s.pool.Exec(ctx, `
			INSERT INTO fee_invoices
			    (account_id, month, currency, trading_fees, mm_rebates,
			     connectivity_fees, total, status, file_ref, content_sha256)
			VALUES ($1, $2, $3, $4, $5, $6, $7, 'ISSUED', $8, $9)
			ON CONFLICT (account_id, month, currency)
			DO UPDATE SET trading_fees = EXCLUDED.trading_fees,
			              mm_rebates = EXCLUDED.mm_rebates,
			              connectivity_fees = EXCLUDED.connectivity_fees,
			              total = EXCLUDED.total,
			              status = 'ISSUED',
			              file_ref = EXCLUDED.file_ref,
			              content_sha256 = EXCLUDED.content_sha256,
			              generated_at = now()`,
			accountID, start, ccy, fee.StringFixed(8), reb.StringFixed(8),
			cf.StringFixed(8), total.StringFixed(8), nullStr(ref), nullStr(sumHex)); err != nil {
			return fmt.Errorf("invoicing: persist %s: %w", ccy, err)
		}
	}
	return nil
}

// List returns invoices newest-first; accountID 0 = all accounts (the
// Finance Ops admin surface), month nil = all months. limit is
// caller-clamped.
func (s *InvoiceService) List(ctx context.Context, accountID int64, month *time.Time, limit int) ([]Invoice, error) {
	args := []any{}
	where := "WHERE TRUE"
	if accountID > 0 {
		args = append(args, accountID)
		where += fmt.Sprintf(" AND account_id = $%d", len(args))
	}
	if month != nil {
		args = append(args, *month)
		where += fmt.Sprintf(" AND month = $%d", len(args))
	}
	args = append(args, limit)
	rows, err := s.pool.Query(ctx, `
		SELECT invoice_id, account_id, month, currency,
		       trading_fees::text, mm_rebates::text, connectivity_fees::text,
		       total::text, status::text, COALESCE(file_ref,''),
		       COALESCE(content_sha256,''), generated_at
		FROM fee_invoices `+where+`
		ORDER BY month DESC, invoice_id DESC
		LIMIT `+fmt.Sprintf("$%d", len(args)), args...)
	if err != nil {
		return nil, fmt.Errorf("invoicing: list: %w", err)
	}
	defer rows.Close()
	var out []Invoice
	for rows.Next() {
		var (
			inv             Invoice
			fee, reb, cf, t string
		)
		if err := rows.Scan(&inv.InvoiceID, &inv.AccountID, &inv.Month, &inv.Currency,
			&fee, &reb, &cf, &t, &inv.Status, &inv.FileRef, &inv.ContentSHA256,
			&inv.GeneratedAt); err != nil {
			return nil, err
		}
		for label, dst := range map[string]*decimal.Decimal{
			"trading": &inv.TradingFees, "rebates": &inv.MMRebates,
			"conn": &inv.ConnectivityFees, "total": &inv.Total,
		} {
			raw := map[string]string{"trading": fee, "rebates": reb, "conn": cf, "total": t}[label]
			d, err := decimal.NewFromString(raw)
			if err != nil {
				return nil, fmt.Errorf("invoicing: parse %s %q: %w", label, raw, err)
			}
			*dst = d
		}
		out = append(out, inv)
	}
	return out, rows.Err()
}

// FetchFile resolves a stored invoice document (pdf|csv) — Finance Ops
// export path.
func (s *InvoiceService) FetchFile(ctx context.Context, invoiceID int64, format string) ([]byte, string, error) {
	var ref string
	err := s.pool.QueryRow(ctx,
		`SELECT COALESCE(file_ref,'') FROM fee_invoices WHERE invoice_id = $1`,
		invoiceID).Scan(&ref)
	if stderrors.Is(err, pgx.ErrNoRows) {
		return nil, "", excerrors.New("NOT_FOUND", "invoice not found")
	}
	if err != nil {
		return nil, "", fmt.Errorf("invoicing: fetch row: %w", err)
	}
	if ref == "" || s.files == nil {
		return nil, "", excerrors.New("NOT_FOUND", "invoice document not stored")
	}
	var key, ct string
	switch format {
	case "", "pdf":
		key, ct = ref+".pdf", "application/pdf"
	case "csv":
		key, ct = ref+".csv", "text/csv; charset=utf-8"
	default:
		return nil, "", excerrors.New("INVALID_REQUEST", "format must be pdf|csv")
	}
	body, err := s.files.Get(ctx, key)
	if err != nil {
		return nil, "", fmt.Errorf("invoicing: fetch object: %w", err)
	}
	return body, ct, nil
}

func nullStr(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// renderInvoiceCSV / renderInvoicePDF produce the invoice documents.
func renderInvoiceCSV(inv *Invoice) []byte {
	var b bytes.Buffer
	fmt.Fprintf(&b, "invoice_account,%d\n", inv.AccountID)
	fmt.Fprintf(&b, "month,%s\ncurrency,%s\n", inv.Month.Format("2006-01"), inv.Currency)
	fmt.Fprintf(&b, "trading_fees,%s\n", inv.TradingFees.StringFixed(8))
	fmt.Fprintf(&b, "mm_rebates,%s\n", inv.MMRebates.StringFixed(8))
	fmt.Fprintf(&b, "connectivity_fees,%s\n", inv.ConnectivityFees.StringFixed(8))
	fmt.Fprintf(&b, "total,%s\n", inv.Total.StringFixed(8))
	fmt.Fprintf(&b, "status,%s\ngenerated_at,%s\n", inv.Status, inv.GeneratedAt.UTC().Format(time.RFC3339))
	return b.Bytes()
}

func renderInvoicePDF(inv *Invoice) []byte {
	title := "Exchange — Institutional Fee Invoice"
	lines := []docLine{
		{title, 14},
		{fmt.Sprintf("Account %d   Month %s   Currency %s",
			inv.AccountID, inv.Month.Format("2006-01"), inv.Currency), 11},
		{fmt.Sprintf("Generated %s UTC", inv.GeneratedAt.UTC().Format("2006-01-02 15:04:05")), 9},
		{" ", 8},
		{fmt.Sprintf("%-24s %18s", "Trading fees:", inv.TradingFees.StringFixed(2)), 10},
		{fmt.Sprintf("%-24s %18s", "Connectivity fees:", inv.ConnectivityFees.StringFixed(2)), 10},
		{fmt.Sprintf("%-24s %18s", "MM rebates:", "-"+inv.MMRebates.StringFixed(2)), 10},
		{" ", 8},
		{fmt.Sprintf("%-24s %18s %s", "TOTAL DUE:", inv.Total.StringFixed(2), inv.Currency), 12},
	}
	return renderTextPDF(title, lines)
}
