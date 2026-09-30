// margin_store.go — PgMarginStore: the PostgreSQL implementation of
// MarginStore (Phase-19 Task 19.3.1). All monetary columns scan as
// ::text into the decimal facade — never float64.
package risk

import (
	"context"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"exchange/pkg/decimal"
)

// PgMarginStore implements MarginStore over pgx.
type PgMarginStore struct {
	Pool *pgxpool.Pool
}

// NewPgMarginStore binds the pool.
func NewPgMarginStore(pool *pgxpool.Pool) (*PgMarginStore, error) {
	if pool == nil {
		return nil, fmt.Errorf("margin store: nil pgx pool")
	}
	return &PgMarginStore{Pool: pool}, nil
}

// MarginAccount implements MarginStore.
func (s *PgMarginStore) MarginAccount(ctx context.Context, accountID int64) (*MarginAccount, error) {
	var mode, status string
	var equity, used *string
	err := s.Pool.QueryRow(ctx, `
		SELECT margin_mode::text, status::text, equity::text, used_margin::text
		FROM margin_accounts WHERE account_id = $1`, accountID).
		Scan(&mode, &status, &equity, &used)
	if err == pgx.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("margin account read %d: %w", accountID, err)
	}
	a := &MarginAccount{AccountID: accountID, Status: status}
	m, ok := normalizeMode(mode)
	if !ok {
		return nil, fmt.Errorf("margin account %d: unknown mode %q", accountID, mode)
	}
	a.Mode = m
	if equity != nil {
		d, err := decimal.NewFromString(*equity)
		if err != nil {
			return nil, fmt.Errorf("margin account %d equity: %w", accountID, err)
		}
		a.Equity = d
	}
	if used != nil {
		d, err := decimal.NewFromString(*used)
		if err != nil {
			return nil, fmt.Errorf("margin account %d used_margin: %w", accountID, err)
		}
		a.UsedMargin = d
	}
	return a, nil
}

// SetMarginMode implements MarginStore — upsert keyed on
// uq_margin_accounts_account_id (migration 020).
func (s *PgMarginStore) SetMarginMode(ctx context.Context, accountID int64, mode MarginMode) error {
	tag, err := s.Pool.Exec(ctx, `
		INSERT INTO margin_accounts (account_id, margin_mode, status)
		VALUES ($1, $2::margin_mode_enum, 'NORMAL')
		ON CONFLICT (account_id)
		DO UPDATE SET margin_mode = EXCLUDED.margin_mode, updated_at = now()`,
		accountID, string(mode))
	if err != nil {
		return fmt.Errorf("set margin mode acct %d: %w", accountID, err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("set margin mode acct %d: no row affected", accountID)
	}
	return nil
}

// OpenPositionCount implements MarginStore.
func (s *PgMarginStore) OpenPositionCount(ctx context.Context, accountID int64) (int64, error) {
	var n int64
	if err := s.Pool.QueryRow(ctx, `
		SELECT count(*) FROM positions
		WHERE account_id = $1 AND quantity <> 0`, accountID).Scan(&n); err != nil {
		return 0, fmt.Errorf("open position count acct %d: %w", accountID, err)
	}
	return n, nil
}

// AccountCategory implements MarginStore — accounts.client_category
// (migration 042); missing account row is NOT_FOUND-shaped: "" + nil
// would silently default retail, so a missing row is an error.
func (s *PgMarginStore) AccountCategory(ctx context.Context, accountID int64) (string, error) {
	var cat string
	err := s.Pool.QueryRow(ctx, `
		SELECT client_category::text FROM accounts WHERE id = $1`, accountID).Scan(&cat)
	if err == pgx.ErrNoRows {
		return "", fmt.Errorf("account %d not found", accountID)
	}
	if err != nil {
		return "", fmt.Errorf("account category %d: %w", accountID, err)
	}
	return cat, nil
}

// AccountBaseCurrency resolves accounts.base_currency (migration 110).
func (s *PgMarginStore) AccountBaseCurrency(ctx context.Context, accountID int64) (string, error) {
	var ccy string
	err := s.Pool.QueryRow(ctx, `
		SELECT base_currency FROM accounts WHERE id = $1`, accountID).Scan(&ccy)
	if err == pgx.ErrNoRows {
		return "", fmt.Errorf("account %d not found", accountID)
	}
	if err != nil {
		return "", fmt.Errorf("account base currency %d: %w", accountID, err)
	}
	return strings.ToUpper(strings.TrimSpace(ccy)), nil
}

// Balances implements MarginStore.
func (s *PgMarginStore) Balances(ctx context.Context, accountID int64) ([]BalanceAmount, error) {
	rows, err := s.Pool.Query(ctx, `
		SELECT currency, available::text, locked::text
		FROM balances WHERE account_id = $1`, accountID)
	if err != nil {
		return nil, fmt.Errorf("balances acct %d: %w", accountID, err)
	}
	defer rows.Close()
	var out []BalanceAmount
	for rows.Next() {
		var b BalanceAmount
		var avail, locked string
		if err := rows.Scan(&b.Currency, &avail, &locked); err != nil {
			return nil, fmt.Errorf("balances acct %d scan: %w", accountID, err)
		}
		if b.Available, err = decimal.NewFromString(avail); err != nil {
			return nil, fmt.Errorf("balances acct %d available %q: %w", accountID, avail, err)
		}
		if b.Locked, err = decimal.NewFromString(locked); err != nil {
			return nil, fmt.Errorf("balances acct %d locked %q: %w", accountID, locked, err)
		}
		b.Currency = strings.ToUpper(b.Currency)
		out = append(out, b)
	}
	return out, rows.Err()
}

// MarginPositions implements MarginStore — open positions joined to
// instruments. quantity<>0 selects only live positions.
func (s *PgMarginStore) MarginPositions(ctx context.Context, accountID int64) ([]MarginPosition, error) {
	rows, err := s.Pool.Query(ctx, `
		SELECT p.id, p.instrument_id, i.symbol, i.base_currency, i.quote_currency,
		       COALESCE(i.max_leverage, 0),
		       p.side::text, p.quantity::text, p.entry_price::text,
		       p.mark_price::text, p.margin_used::text,
		       COALESCE(p.isolated_margin_allocated, 0)::text,
		       COALESCE(p.auto_margin_replenish, false)
		FROM positions p
		JOIN instruments i ON i.id = p.instrument_id
		WHERE p.account_id = $1 AND p.quantity <> 0
		ORDER BY p.id`, accountID)
	if err != nil {
		return nil, fmt.Errorf("positions acct %d: %w", accountID, err)
	}
	defer rows.Close()
	var out []MarginPosition
	for rows.Next() {
		var p MarginPosition
		var qty, entry, marginUsed, iso string
		var mark *string
		if err := rows.Scan(&p.ID, &p.InstrumentID, &p.Symbol,
			&p.BaseCurrency, &p.QuoteCurrency, &p.MaxLeverage,
			&p.Side, &qty, &entry, &mark, &marginUsed, &iso,
			&p.AutoReplenish); err != nil {
			return nil, fmt.Errorf("positions acct %d scan: %w", accountID, err)
		}
		var err error
		if p.Quantity, err = decimal.NewFromString(qty); err != nil {
			return nil, fmt.Errorf("positions %d quantity: %w", p.ID, err)
		}
		if p.EntryPrice, err = decimal.NewFromString(entry); err != nil {
			return nil, fmt.Errorf("positions %d entry_price: %w", p.ID, err)
		}
		if p.MarginUsed, err = decimal.NewFromString(marginUsed); err != nil {
			return nil, fmt.Errorf("positions %d margin_used: %w", p.ID, err)
		}
		if p.IsolatedAllocated, err = decimal.NewFromString(iso); err != nil {
			return nil, fmt.Errorf("positions %d isolated_allocated: %w", p.ID, err)
		}
		if mark != nil && *mark != "" {
			d, err := decimal.NewFromString(*mark)
			if err != nil {
				return nil, fmt.Errorf("positions %d mark_price: %w", p.ID, err)
			}
			p.StoredMark = &d
		}
		p.QuoteCurrency = strings.ToUpper(p.QuoteCurrency)
		p.BaseCurrency = strings.ToUpper(p.BaseCurrency)
		out = append(out, p)
	}
	return out, rows.Err()
}

// FxPairInstruments implements MarginStore — resolves each currency's
// USD conversion instrument in ONE query: {CCY}/USD direct first,
// USD/{CCY} inverse otherwise. No coverage → currency absent from the
// map (callers fail closed or disclose as unvalued).
func (s *PgMarginStore) FxPairInstruments(ctx context.Context, ccys []string) (map[string]FxPair, error) {
	out := map[string]FxPair{}
	if len(ccys) == 0 {
		return out, nil
	}
	rows, err := s.Pool.Query(ctx, `
		SELECT symbol, base_currency, quote_currency FROM instruments
		WHERE status = 'ACTIVE'
		  AND ( (quote_currency = 'USD' AND base_currency = ANY($1))
		     OR (base_currency  = 'USD' AND quote_currency = ANY($1)) )`,
		ccys)
	if err != nil {
		return nil, fmt.Errorf("fx pair instruments: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var sym, base, quote string
		if err := rows.Scan(&sym, &base, &quote); err != nil {
			return nil, fmt.Errorf("fx pair scan: %w", err)
		}
		if base == "USD" {
			// USD/{CCY} → rate = 1/mark. Keep the FIRST inverse row per
			// ccy (deterministic ordering by symbol keeps the pick stable).
			ccy := strings.ToUpper(quote)
			if cur, ok := out[ccy]; !ok || (cur.Inverted && sym < cur.Symbol) {
				if !ok || cur.Inverted {
					out[ccy] = FxPair{Symbol: sym, Inverted: true}
				}
			}
		} else if quote == "USD" {
			// {CCY}/USD direct always wins over an inverse pairing.
			out[strings.ToUpper(base)] = FxPair{Symbol: sym}
		}
	}
	return out, rows.Err()
}

// OpenPositionIndex implements MarginStore — the engine's tick→account
// fan-out index, rebuilt wholesale (one query, never per-symbol scans).
func (s *PgMarginStore) OpenPositionIndex(ctx context.Context) (map[string][]int64, error) {
	rows, err := s.Pool.Query(ctx, `
		SELECT i.symbol, p.account_id FROM positions p
		JOIN instruments i ON i.id = p.instrument_id
		WHERE p.quantity <> 0
		ORDER BY i.symbol, p.account_id`)
	if err != nil {
		return nil, fmt.Errorf("open position index: %w", err)
	}
	defer rows.Close()
	out := map[string][]int64{}
	for rows.Next() {
		var sym string
		var acct int64
		if err := rows.Scan(&sym, &acct); err != nil {
			return nil, fmt.Errorf("open position index scan: %w", err)
		}
		out[sym] = append(out[sym], acct)
	}
	return out, rows.Err()
}

// OpenInterest implements OpenInterestSource — the §13.12
// concentration add-on's denominator: aggregate open notional for the
// instrument, Σ |quantity| × COALESCE(mark_price, entry_price) over
// quantity <> 0 rows (same formula as PgLiquidationStore.OpenInterest;
// the shared convention lives in liquidation_store.go's header note).
func (s *PgMarginStore) OpenInterest(ctx context.Context, instrumentID int64) (decimal.Decimal, error) {
	var txt string
	if err := s.Pool.QueryRow(ctx, `
		SELECT COALESCE(SUM(ABS(quantity) * COALESCE(mark_price, entry_price)), 0)::text
		FROM positions
		WHERE instrument_id = $1 AND quantity <> 0`, instrumentID).Scan(&txt); err != nil {
		return decimal.Zero, fmt.Errorf("open interest instr %d: %w", instrumentID, err)
	}
	d, err := decimal.NewFromString(txt)
	if err != nil {
		return decimal.Zero, fmt.Errorf("open interest instr %d parse %q: %w", instrumentID, txt, err)
	}
	return d, nil
}

// WriteMarginSnapshot implements MarginStore — persists the evaluation
// (equity/used/utilization/status) to margin_accounts, creating the row
// when absent. Called by the engine on status transitions only; the
// hash remains the live authority between writes.
func (s *PgMarginStore) WriteMarginSnapshot(ctx context.Context, snap MarginSnapshot) error {
	util := decimal.Zero
	if snap.Equity.IsPositive() {
		util = snap.UsedMargin.Div(snap.Equity)
	}
	_, err := s.Pool.Exec(ctx, `
		INSERT INTO margin_accounts (account_id, margin_mode, equity, used_margin,
		                             available_margin, margin_utilization, status)
		VALUES ($1, $2::margin_mode_enum, $3, $4, $5, $6, $7::margin_account_status_enum)
		ON CONFLICT (account_id) DO UPDATE SET
			equity             = EXCLUDED.equity,
			used_margin        = EXCLUDED.used_margin,
			available_margin   = EXCLUDED.available_margin,
			margin_utilization = EXCLUDED.margin_utilization,
			status             = EXCLUDED.status,
			updated_at         = now()`,
		snap.AccountID, string(snap.Mode), snap.Equity.String(),
		snap.UsedMargin.String(), snap.AvailableMargin.String(),
		util.String(), snap.Status)
	if err != nil {
		return fmt.Errorf("write margin snapshot acct %d: %w", snap.AccountID, err)
	}
	return nil
}
