// trial_balance.go — Phase-20 Task 20.3.7: the house finance layer over
// the double-entry GL (spec §16.5, §24 #205).
//
// The daily trial balance snapshots cumulative ledger_lines totals per
// (currency, GL account) as of EOD — computed AFTER the Tom-Next
// rollover run (rollover posts the day's last journals). Persisted rows
// in trial_balances (migration 049) are a read-model cache; the report
// is always recomputable from journal_entries/ledger_lines.
//
// Invariants asserted on every compute (fail-closed, spec §2.7):
//
//	Σ debits == Σ credits per currency            (GL zero-sum)
//	Assets == Liabilities + Equity + (Revenue − Expenses)
//
// The second is the accounting equation with open P&L accounts — the
// phase text's "Assets = Liabilities + Equity (category-9 zero-sum)"
// holds because revenue−expense is the not-yet-closed equity delta. A
// breach is a hard error; a trial balance is never emitted "best effort".
//
// Edge-case rulings (documented per the task's edge-case list):
//   - first-run / empty GL day: produces an empty currency set with
//     invariants trivially satisfied — a zero trial balance, not an
//     error;
//   - weekend/holiday EOD: the job runs every calendar day regardless
//     (the GL is a daily record; a no-activity day snapshots zero-delta);
//   - currency with no activity: its accounts are OMITTED from the
//     currency sections (totals stay exact rather than padded with
//     zero rows for 36 chart accounts × 9 currencies).
package analytics

import (
	"bytes"
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"exchange/pkg/decimal"
	excerrors "exchange/pkg/errors"
)

// TBLine is one (currency, account) trial-balance line.
type TBLine struct {
	AccountCode string          `json:"account_code"`
	AccountName string          `json:"account_name"`
	AccountType string          `json:"account_type"` // gl_account_type_enum
	Currency    string          `json:"currency"`
	Debits      decimal.Decimal `json:"debit_total"`
	Credits     decimal.Decimal `json:"credit_total"`
	Net         decimal.Decimal `json:"net_balance"` // debits − credits (signed)
}

// CurrencyTB is one currency section of the trial balance.
type CurrencyTB struct {
	Currency    string          `json:"currency"`
	Lines       []TBLine        `json:"lines"`
	Debits      decimal.Decimal `json:"total_debits"`
	Credits     decimal.Decimal `json:"total_credits"`
	Assets      decimal.Decimal `json:"assets"`      // net debit balance of ASSET accounts
	Liabilities decimal.Decimal `json:"liabilities"` // net credit balance of LIABILITY accounts
	Equity      decimal.Decimal `json:"equity"`      // net credit balance of EQUITY accounts
	Revenue     decimal.Decimal `json:"revenue"`     // net credit balance of REVENUE accounts
	Expenses    decimal.Decimal `json:"expenses"`    // net debit balance of EXPENSE accounts
	// NetIncome is Revenue − Expenses (the unclosed P&L flowing to equity).
	NetIncome decimal.Decimal `json:"net_income"`
	// EquationVariance is Assets − (Liabilities + Equity + NetIncome) —
	// must be zero. AccountingEquationOK reports the check.
	EquationVariance     decimal.Decimal `json:"equation_variance"`
	AccountingEquationOK bool            `json:"accounting_equation_ok"`
	ZeroSumOK            bool            `json:"zero_sum_ok"`
}

// TrialBalance is the full daily snapshot.
type TrialBalance struct {
	Day         time.Time    `json:"business_date"`
	Currencies  []CurrencyTB `json:"currencies"`
	GeneratedAt time.Time    `json:"generated_at"`
}

// ReconCheck is one sub-ledger reconciliation line — expected is the
// GL-implied figure, actual the sub-ledger's own record; variance is
// expected − actual (zero = reconciled).
type ReconCheck struct {
	Name     string          `json:"name"` // e.g. "client_balances", "nostro", "insurance_fund", "fee_income"
	Currency string          `json:"currency"`
	Expected decimal.Decimal `json:"expected"`
	Actual   decimal.Decimal `json:"actual"`
	Variance decimal.Decimal `json:"variance"`
	OK       bool            `json:"ok"`
}

// TrialBalanceService computes and persists the daily trial balance and
// the house P&L / balance-sheet extracts.
type TrialBalanceService struct {
	pool *pgxpool.Pool
	now  func() time.Time
}

// NewTrialBalanceService wires the service (pool required).
func NewTrialBalanceService(pool *pgxpool.Pool) (*TrialBalanceService, error) {
	if pool == nil {
		return nil, fmt.Errorf("trialbalance: nil pgx pool")
	}
	return &TrialBalanceService{pool: pool, now: time.Now}, nil
}

// SetClockForTest overrides the clock; tests only.
func (s *TrialBalanceService) SetClockForTest(now func() time.Time) { s.now = now }

// Compute builds the trial balance as of end-of-day `day` (cumulative
// through day+1 00:00 UTC). Currencies with no GL activity are omitted;
// a fully empty GL yields an empty TrialBalance with no error — the
// first-run edge case is a valid zero report.
func (s *TrialBalanceService) Compute(ctx context.Context, day time.Time) (*TrialBalance, error) {
	cutoff := normalizeUTCDate(day).AddDate(0, 0, 1)
	tb := &TrialBalance{
		Day:         normalizeUTCDate(day),
		GeneratedAt: s.now().UTC(),
	}
	rows, err := s.pool.Query(ctx, `
		SELECT coa.currency, coa.account_code, coa.account_name,
		       coa.account_type::text,
		       COALESCE(SUM(ll.debit_amount),0)::text,
		       COALESCE(SUM(ll.credit_amount),0)::text
		FROM chart_of_accounts coa
		LEFT JOIN ledger_lines ll
		  ON ll.account_code = coa.account_code
		 AND EXISTS (SELECT 1 FROM journal_entries je
		              WHERE je.id = ll.journal_entry_id
		                AND je.posted_at < $1)
		WHERE EXISTS (SELECT 1 FROM ledger_lines x
		               JOIN journal_entries j ON j.id = x.journal_entry_id
		              WHERE x.account_code = coa.account_code
		                AND j.posted_at < $1)
		GROUP BY coa.currency, coa.account_code, coa.account_name, coa.account_type
		ORDER BY coa.currency, coa.account_code`, cutoff)
	if err != nil {
		return nil, fmt.Errorf("trialbalance: compute: %w", err)
	}
	defer rows.Close()

	byCcy := map[string]*CurrencyTB{}
	for rows.Next() {
		var (
			l    TBLine
			d, c string
		)
		if err := rows.Scan(&l.Currency, &l.AccountCode, &l.AccountName,
			&l.AccountType, &d, &c); err != nil {
			return nil, err
		}
		var err2 error
		if l.Debits, err2 = decimal.NewFromString(d); err2 != nil {
			return nil, fmt.Errorf("trialbalance: debit parse %q: %w", d, err2)
		}
		if l.Credits, err2 = decimal.NewFromString(c); err2 != nil {
			return nil, fmt.Errorf("trialbalance: credit parse %q: %w", c, err2)
		}
		l.Net = l.Debits.Sub(l.Credits)
		ct := byCcy[l.Currency]
		if ct == nil {
			ct = &CurrencyTB{Currency: l.Currency}
			byCcy[l.Currency] = ct
		}
		ct.Lines = append(ct.Lines, l)
		ct.Debits = ct.Debits.Add(l.Debits)
		ct.Credits = ct.Credits.Add(l.Credits)
		switch l.AccountType {
		case "ASSET":
			ct.Assets = ct.Assets.Add(l.Net)
		case "LIABILITY":
			ct.Liabilities = ct.Liabilities.Add(l.Net.Neg())
		case "EQUITY":
			ct.Equity = ct.Equity.Add(l.Net.Neg())
		case "REVENUE":
			ct.Revenue = ct.Revenue.Add(l.Net.Neg())
		case "EXPENSE":
			ct.Expenses = ct.Expenses.Add(l.Net)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for _, ct := range byCcy {
		ct.NetIncome = ct.Revenue.Sub(ct.Expenses)
		ct.ZeroSumOK = ct.Debits.Equal(ct.Credits)
		ct.EquationVariance = ct.Assets.Sub(ct.Liabilities.Add(ct.Equity).Add(ct.NetIncome))
		ct.AccountingEquationOK = ct.EquationVariance.IsZero()
		tb.Currencies = append(tb.Currencies, *ct)
	}
	sort.Slice(tb.Currencies, func(i, j int) bool {
		return tb.Currencies[i].Currency < tb.Currencies[j].Currency
	})

	// Fail-closed invariant gate — an imbalanced TB is an error, never
	// a document.
	for _, ct := range tb.Currencies {
		if !ct.ZeroSumOK {
			return nil, excerrors.New("LEDGER_IMBALANCE_ABORT", fmt.Sprintf(
				"trial balance %s: debits %s != credits %s", ct.Currency, ct.Debits, ct.Credits))
		}
		if !ct.AccountingEquationOK {
			return nil, excerrors.New("LEDGER_IMBALANCE_ABORT", fmt.Sprintf(
				"trial balance %s: assets %s != L+E+NI (variance %s)",
				ct.Currency, ct.Assets, ct.EquationVariance))
		}
	}
	return tb, nil
}

// Persist upserts the computed lines into the trial_balances read-model
// cache (migration 049; UNIQUE (business_date, currency, account_code)).
func (s *TrialBalanceService) Persist(ctx context.Context, tb *TrialBalance) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("trialbalance: persist tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	for _, ct := range tb.Currencies {
		for _, l := range ct.Lines {
			if _, err := tx.Exec(ctx, `
				INSERT INTO trial_balances
				    (business_date, currency, account_code, account_type,
				     debit_total, credit_total, net_balance)
				VALUES ($1, $2, $3, $4, $5, $6, $7)
				ON CONFLICT (business_date, currency, account_code)
				DO UPDATE SET account_type = EXCLUDED.account_type,
				              debit_total = EXCLUDED.debit_total,
				              credit_total = EXCLUDED.credit_total,
				              net_balance = EXCLUDED.net_balance,
				              generated_at = now()`,
				tb.Day, l.Currency, l.AccountCode, l.AccountType,
				l.Debits.StringFixed(8), l.Credits.StringFixed(8),
				l.Net.StringFixed(8)); err != nil {
				return fmt.Errorf("trialbalance: persist %s/%s: %w",
					l.Currency, l.AccountCode, err)
			}
		}
	}
	return tx.Commit(ctx)
}

// Reconcile runs the sub-ledger checks the task requires and returns the
// variance table (each check is OK=true when variance == 0). Checks:
//
//	client_balances — Σ balances.total per currency vs the GL
//	                  2010_CUSTOMER_LIABILITY_* net credit balance;
//	nostro          — Σ nostro_accounts.balance (ACTIVE) vs the
//	                  1010_NOSTRO_* net debit balance;
//	insurance_fund  — Σ insurance_fund.balance vs the
//	                  2210_INSURANCE_FUND_LIABILITY_* net credit balance;
//	fee_income      — Σ wallet-side FEE credits (all accounts) vs
//	                  4010/4030_* revenue net credit balances.
func (s *TrialBalanceService) Reconcile(ctx context.Context, day time.Time) ([]ReconCheck, error) {
	cutoff := normalizeUTCDate(day).AddDate(0, 0, 1)
	var out []ReconCheck

	// client balances vs 2010 liability
	gl2010, err := s.glNetByPrefix(ctx, "2010_CUSTOMER_LIABILITY", cutoff, false)
	if err != nil {
		return nil, err
	}
	sub, err := s.subLedgerSum(ctx,
		`SELECT currency, SUM(total)::text FROM balances GROUP BY currency`)
	if err != nil {
		return nil, fmt.Errorf("trialbalance: balances subledger: %w", err)
	}
	out = append(out, mergeChecks("client_balances", gl2010, sub)...)

	// nostro vs 1010 asset
	gl1010, err := s.glNetByPrefix(ctx, "1010_NOSTRO", cutoff, true)
	if err != nil {
		return nil, err
	}
	sub, err = s.subLedgerSum(ctx,
		`SELECT currency, SUM(balance)::text FROM nostro_accounts
		  WHERE status = 'ACTIVE' GROUP BY currency`)
	if err != nil {
		return nil, fmt.Errorf("trialbalance: nostro subledger: %w", err)
	}
	out = append(out, mergeChecks("nostro", gl1010, sub)...)

	// insurance fund vs 2210 liability
	gl2210, err := s.glNetByPrefix(ctx, "2210_INSURANCE_FUND_LIABILITY", cutoff, false)
	if err != nil {
		return nil, err
	}
	sub, err = s.subLedgerSum(ctx,
		`SELECT currency, SUM(balance)::text FROM insurance_fund GROUP BY currency`)
	if err != nil {
		return nil, fmt.Errorf("trialbalance: insurance fund subledger: %w", err)
	}
	out = append(out, mergeChecks("insurance_fund", gl2210, sub)...)

	// fee income: wallet FEE credits vs 4010+4030 revenue
	glFees, err := s.glFeeRevenue(ctx, cutoff)
	if err != nil {
		return nil, err
	}
	sub, err = s.subLedgerSum(ctx, `
		SELECT currency, SUM(amount)::text FROM ledger_entries
		WHERE entry_type = 'FEE' AND direction = 'CREDIT' AND posted_at < $1
		GROUP BY currency`, cutoff)
	if err != nil {
		return nil, fmt.Errorf("trialbalance: fee subledger: %w", err)
	}
	out = append(out, mergeChecks("fee_income", glFees, sub)...)

	sort.Slice(out, func(i, j int) bool {
		if out[i].Name != out[j].Name {
			return out[i].Name < out[j].Name
		}
		return out[i].Currency < out[j].Currency
	})
	return out, nil
}

// glNetByPrefix returns the net balance per currency of chart accounts
// matching "PREFIX_%" through cutoff. debitNormal=true returns the
// debit−credit balance (asset convention); false returns credit−debit
// (liability/equity convention — the value a credit-side account
// "carries").
func (s *TrialBalanceService) glNetByPrefix(ctx context.Context, prefix string, cutoff time.Time, debitNormal bool) (map[string]decimal.Decimal, error) {
	sign := "ll.debit_amount - ll.credit_amount"
	if !debitNormal {
		sign = "ll.credit_amount - ll.debit_amount"
	}
	rows, err := s.pool.Query(ctx, fmt.Sprintf(`
		SELECT ll.currency, COALESCE(SUM(%s),0)::text
		FROM ledger_lines ll
		JOIN journal_entries je ON je.id = ll.journal_entry_id
		WHERE ll.account_code LIKE $1 AND je.posted_at < $2
		GROUP BY ll.currency`, sign), prefix+`\_%`, cutoff)
	if err != nil {
		return nil, fmt.Errorf("trialbalance: gl prefix %s: %w", prefix, err)
	}
	defer rows.Close()
	return scanDecMap(rows)
}

// glFeeRevenue sums net credit balances of 4010_TRADING_FEE_REVENUE and
// 4030_COMMISSION_REVENUE through cutoff.
func (s *TrialBalanceService) glFeeRevenue(ctx context.Context, cutoff time.Time) (map[string]decimal.Decimal, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT ll.currency, COALESCE(SUM(ll.credit_amount - ll.debit_amount),0)::text
		FROM ledger_lines ll
		JOIN journal_entries je ON je.id = ll.journal_entry_id
		WHERE (ll.account_code LIKE '4010\_TRADING\_FEE\_REVENUE\_%'
		       OR ll.account_code LIKE '4030\_COMMISSION\_REVENUE\_%')
		  AND je.posted_at < $1
		GROUP BY ll.currency`, cutoff)
	if err != nil {
		return nil, fmt.Errorf("trialbalance: gl fee revenue: %w", err)
	}
	defer rows.Close()
	return scanDecMap(rows)
}

func (s *TrialBalanceService) subLedgerSum(ctx context.Context, q string, args ...any) (map[string]decimal.Decimal, error) {
	rows, err := s.pool.Query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanDecMap(rows)
}

// scanDecMap scans (currency, decimal-text) row sets.
func scanDecMap(rows interface {
	Next() bool
	Scan(...any) error
	Err() error
}) (map[string]decimal.Decimal, error) {
	out := map[string]decimal.Decimal{}
	for rows.Next() {
		var ccy, sum string
		if err := rows.Scan(&ccy, &sum); err != nil {
			return nil, err
		}
		d, err := decimal.NewFromString(sum)
		if err != nil {
			return nil, fmt.Errorf("trialbalance: sum parse %q: %w", sum, err)
		}
		out[ccy] = d
	}
	return out, rows.Err()
}

// mergeChecks pairs the GL expectation against the sub-ledger actuals
// over the union of currencies — a currency present on only one side
// still produces a check row (variance = the lone side, loudly nonzero).
func mergeChecks(name string, expected, actual map[string]decimal.Decimal) []ReconCheck {
	ccys := map[string]struct{}{}
	for c := range expected {
		ccys[c] = struct{}{}
	}
	for c := range actual {
		ccys[c] = struct{}{}
	}
	sorted := make([]string, 0, len(ccys))
	for c := range ccys {
		sorted = append(sorted, c)
	}
	sort.Strings(sorted)
	out := make([]ReconCheck, 0, len(sorted))
	for _, c := range sorted {
		e := expected[c]
		a := actual[c]
		v := e.Sub(a)
		out = append(out, ReconCheck{
			Name: name, Currency: c, Expected: e, Actual: a,
			Variance: v, OK: v.IsZero(),
		})
	}
	return out
}

// ---------------------------------------------------------------------------
// Period flows — house P&L over an arbitrary [start,end) window.
// ---------------------------------------------------------------------------

// FlowLine is one account's period net flow (revenue/expense view).
type FlowLine struct {
	AccountCode string `json:"account_code"`
	AccountName string `json:"account_name"`
	AccountType string `json:"account_type"` // REVENUE | EXPENSE
	Currency    string `json:"currency"`
	// Net is credit-normal for REVENUE, debit-normal for EXPENSE —
	// the account's signed contribution in its natural direction.
	Net decimal.Decimal `json:"net"`
}

// PeriodPnL is the house P&L section for one currency over the window.
type PeriodPnL struct {
	Currency  string          `json:"currency"`
	Lines     []FlowLine      `json:"lines"`
	Revenue   decimal.Decimal `json:"total_revenue"`
	Expenses  decimal.Decimal `json:"total_expenses"`
	NetIncome decimal.Decimal `json:"net_income"`
}

// PeriodPnL aggregates journal-window REVENUE/EXPENSE flows per currency
// (the income statement is a period flow, not a point-in-time balance —
// unlike the trial balance this reads [start,end), never cumulative).
func (s *TrialBalanceService) PeriodPnL(ctx context.Context, start, end time.Time) ([]PeriodPnL, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT coa.currency, coa.account_code, coa.account_name,
		       coa.account_type::text,
		       COALESCE(SUM(ll.credit_amount - ll.debit_amount),0)::text AS credit_net,
		       COALESCE(SUM(ll.debit_amount - ll.credit_amount),0)::text AS debit_net
		FROM chart_of_accounts coa
		JOIN ledger_lines ll ON ll.account_code = coa.account_code
		JOIN journal_entries je ON je.id = ll.journal_entry_id
		WHERE coa.account_type IN ('REVENUE','EXPENSE')
		  AND je.posted_at >= $1 AND je.posted_at < $2
		GROUP BY coa.currency, coa.account_code, coa.account_name, coa.account_type
		ORDER BY coa.currency, coa.account_code`, start, end)
	if err != nil {
		return nil, fmt.Errorf("trialbalance: period pnl: %w", err)
	}
	defer rows.Close()
	byCcy := map[string]*PeriodPnL{}
	for rows.Next() {
		var (
			l      FlowLine
			cn, dn string
		)
		if err := rows.Scan(&l.Currency, &l.AccountCode, &l.AccountName,
			&l.AccountType, &cn, &dn); err != nil {
			return nil, err
		}
		var err2 error
		switch l.AccountType {
		case "REVENUE":
			if l.Net, err2 = decimal.NewFromString(cn); err2 != nil {
				return nil, fmt.Errorf("trialbalance: pnl revenue parse: %w", err2)
			}
		case "EXPENSE":
			if l.Net, err2 = decimal.NewFromString(dn); err2 != nil {
				return nil, fmt.Errorf("trialbalance: pnl expense parse: %w", err2)
			}
		}
		pl := byCcy[l.Currency]
		if pl == nil {
			pl = &PeriodPnL{Currency: l.Currency}
			byCcy[l.Currency] = pl
		}
		pl.Lines = append(pl.Lines, l)
		if l.AccountType == "REVENUE" {
			pl.Revenue = pl.Revenue.Add(l.Net)
		} else {
			pl.Expenses = pl.Expenses.Add(l.Net)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out := make([]PeriodPnL, 0, len(byCcy))
	for _, pl := range byCcy {
		pl.NetIncome = pl.Revenue.Sub(pl.Expenses)
		out = append(out, *pl)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Currency < out[j].Currency })
	return out, nil
}

// RenderPnLCSV renders the period P&L export (per-currency lines + net
// income) — the house P&L statement wire format.
func RenderPnLCSV(pls []PeriodPnL, start, end time.Time, checks []ReconCheck) []byte {
	var b bytes.Buffer
	fmt.Fprintf(&b, "report,pnl\nperiod_start,%s\nperiod_end,%s\n\n",
		start.Format("2006-01-02"), end.Format("2006-01-02"))
	b.WriteString("currency,account_code,account_name,side,net\n")
	for _, pl := range pls {
		for _, l := range pl.Lines {
			fmt.Fprintf(&b, "%s,%s,%s,%s,%s\n",
				l.Currency, l.AccountCode, csvq(l.AccountName), l.AccountType,
				l.Net.StringFixed(8))
		}
		fmt.Fprintf(&b, "%s,NET_INCOME,,REVENUE_MINUS_EXPENSE,%s\n",
			pl.Currency, pl.NetIncome.StringFixed(8))
	}
	if checks != nil {
		b.WriteString("\nreconciliation: check,currency,gl_expected,subledger_actual,variance,ok\n")
		for _, c := range checks {
			fmt.Fprintf(&b, "%s,%s,%s,%s,%s,%t\n",
				c.Name, c.Currency, c.Expected.StringFixed(8),
				c.Actual.StringFixed(8), c.Variance.StringFixed(8), c.OK)
		}
	}
	return b.Bytes()
}

// ---------------------------------------------------------------------------
// CSV exports — trial-balance / house P&L / balance sheet.
// ---------------------------------------------------------------------------

// ExportKind selects the finance CSV surface.
type ExportKind string

const (
	ExportTrialBalance ExportKind = "trial-balance"
	ExportPnL          ExportKind = "pnl"
	ExportBalanceSheet ExportKind = "balance-sheet"
)

// ExportCSV renders the trial balance, house P&L or balance sheet for a
// day, with the sub-ledger reconciliation footer appended (the task's
// variance report). Parquet is deliberately omitted — CSV is the
// mandated wire (documented deviation; add a columnar codec if an ERP
// consumer ever requires it).
func (s *TrialBalanceService) ExportCSV(ctx context.Context, kind ExportKind, day time.Time) ([]byte, *TrialBalance, []ReconCheck, error) {
	tb, err := s.Compute(ctx, day)
	if err != nil {
		return nil, nil, nil, err
	}
	checks, err := s.Reconcile(ctx, day)
	if err != nil {
		return nil, nil, nil, err
	}
	// Fail closed: a sub-ledger↔GL variance aborts the export — the
	// variance-report footer describes a CLEAN export, never a best-effort
	// document that papers over a mismatch (§2.7 pessimism).
	for _, c := range checks {
		if !c.OK {
			return nil, nil, nil, excerrors.New("LEDGER_IMBALANCE_ABORT", fmt.Sprintf(
				"finance export %s reconciliation mismatch %s/%s: expected %s actual %s variance %s",
				kind, c.Name, c.Currency, c.Expected, c.Actual, c.Variance))
		}
	}
	var b bytes.Buffer
	fmt.Fprintf(&b, "report,%s\nbusiness_date,%s\ngenerated_at,%s\n\n",
		kind, tb.Day.Format("2006-01-02"), tb.GeneratedAt.Format(time.RFC3339))
	switch kind {
	case ExportTrialBalance:
		b.WriteString("currency,account_code,account_name,account_type,debit_total,credit_total,net_balance\n")
		for _, ct := range tb.Currencies {
			for _, l := range ct.Lines {
				fmt.Fprintf(&b, "%s,%s,%s,%s,%s,%s,%s\n",
					l.Currency, l.AccountCode, csvq(l.AccountName), l.AccountType,
					l.Debits.StringFixed(8), l.Credits.StringFixed(8), l.Net.StringFixed(8))
			}
		}
	case ExportPnL:
		b.WriteString("currency,account_code,account_name,side,net_balance\n")
		for _, ct := range tb.Currencies {
			for _, l := range ct.Lines {
				if l.AccountType != "REVENUE" && l.AccountType != "EXPENSE" {
					continue
				}
				fmt.Fprintf(&b, "%s,%s,%s,%s,%s\n",
					l.Currency, l.AccountCode, csvq(l.AccountName),
					l.AccountType, l.Net.Neg().StringFixed(8))
			}
			fmt.Fprintf(&b, "%s,NET_INCOME,,REVENUE_MINUS_EXPENSE,%s\n",
				ct.Currency, ct.NetIncome.StringFixed(8))
		}
	case ExportBalanceSheet:
		b.WriteString("currency,section,account_code,account_name,balance\n")
		for _, ct := range tb.Currencies {
			for _, l := range ct.Lines {
				var section, bal string
				switch l.AccountType {
				case "ASSET":
					section, bal = "ASSETS", l.Net.StringFixed(8)
				case "LIABILITY":
					section, bal = "LIABILITIES", l.Net.Neg().StringFixed(8)
				case "EQUITY":
					section, bal = "EQUITY", l.Net.Neg().StringFixed(8)
				default:
					continue // P&L accounts are excluded from the balance sheet
				}
				fmt.Fprintf(&b, "%s,%s,%s,%s,%s\n",
					l.Currency, section, l.AccountCode, csvq(l.AccountName), bal)
			}
			fmt.Fprintf(&b, "%s,MEMO,NET_INCOME_UNCLOSED,Revenue minus expenses (P&L, unclosed),%s\n",
				ct.Currency, ct.NetIncome.StringFixed(8))
		}
	default:
		return nil, nil, nil, excerrors.New("INVALID_REQUEST",
			fmt.Sprintf("unknown finance export kind %q", kind))
	}
	// Reconciliation footer — the variance report.
	b.WriteString("\nreconciliation: check,currency,gl_expected,subledger_actual,variance,ok\n")
	for _, c := range checks {
		fmt.Fprintf(&b, "%s,%s,%s,%s,%s,%t\n",
			c.Name, c.Currency, c.Expected.StringFixed(8),
			c.Actual.StringFixed(8), c.Variance.StringFixed(8), c.OK)
	}
	return b.Bytes(), tb, checks, nil
}

// ---------------------------------------------------------------------------
// DailyTrialBalanceJob — the scheduler seam.
// ---------------------------------------------------------------------------

// DailyTrialBalanceJob is the unit the orchestrator binds to the EOD
// scheduler — strictly after the Tom-Next rollover (its journals are the
// last GL postings of the trading day). The job runs every calendar day
// including weekends/holidays: the GL is a daily record and a zero-delta
// day still snapshots + reconciles. Target: complete by EOD+1h.
type DailyTrialBalanceJob struct {
	svc *TrialBalanceService
	now func() time.Time
}

// NewDailyTrialBalanceJob wires the job.
func NewDailyTrialBalanceJob(svc *TrialBalanceService) *DailyTrialBalanceJob {
	return &DailyTrialBalanceJob{svc: svc, now: time.Now}
}

// SetClockForTest overrides the job clock; tests only.
func (j *DailyTrialBalanceJob) SetClockForTest(now func() time.Time) { j.now = now }

// RunOnce computes + persists the trial balance for `day` and returns it
// with the reconciliation checks for the ops surface.
func (j *DailyTrialBalanceJob) RunOnce(ctx context.Context, day time.Time) (*TrialBalance, []ReconCheck, error) {
	tb, err := j.svc.Compute(ctx, day)
	if err != nil {
		return nil, nil, err
	}
	if err := j.svc.Persist(ctx, tb); err != nil {
		return nil, nil, err
	}
	checks, err := j.svc.Reconcile(ctx, day)
	if err != nil {
		return nil, nil, err
	}
	return tb, checks, nil
}
