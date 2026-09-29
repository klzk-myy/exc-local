// pnl.go — Phase-13 Task 13.3.4: real-time account P&L service.
//
// Computes, for one account:
//   - realized P&L   — the cumulative positions.realized_pnl column the
//     settlement PositionService (Task 3.3.2) maintains per
//     (account, instrument); flat rows keep their realized total, so the
//     account-level realized figure is the per-currency SUM over every
//     position row, open or closed;
//   - unrealized P&L — recomputed live as signedQty × (mark − entry)
//     per open position, where `mark` comes from the Phase-19.5
//     PriceOracle seam. The placeholder bound in this phase is the
//     last-trade reference price (orders.PgStore.ReferencePrice — the
//     same seam order admission uses), falling back to the position's
//     stored mark_price and finally to entry_price (uPnL 0 — the
//     position's own cost basis, never a fabricated number). The chosen
//     source is stamped per position for transparency.
//
// All P&L is denominated in the instrument's QUOTE currency
// (spec §13.1/§16.4 — the same convention settlement.PositionService
// and balance.SettlementService use). The response always carries the
// per-currency breakdown; when a Converter (internal/position, Task
// 3.3.9) is bound the service additionally aggregates to the account's
// base_currency — a conversion failure aborts the whole snapshot
// (fail-closed §2.7: never report a P&L whose FX leg was guessed).
//
// Push path: OnTrade / OnMarkUpdate recompute and publish a `pnl` event
// through the private-channel seam (PublishPrivate → "private:pnl" —
// account-isolated per the §10.5 private:* contract; the task's
// "account:{id}" channel maps onto the account-isolated private
// channel, which is the existing infrastructure's equivalent).
package risk

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"exchange/internal/position"
	"exchange/pkg/decimal"
	excerrors "exchange/pkg/errors"
)

// PnlChannel is the §10.5 private channel P&L events fan out on —
// registered in ws.PrivateChannels; delivery is account-isolated by
// ws.Server.PublishPrivate.
const PnlChannel = "private:pnl"

// ---------------------------------------------------------------------------
// Seams
// ---------------------------------------------------------------------------

// PnlMarkSource is the mark-price seam — the Phase-19.5 PriceOracle
// placeholder. *orders.PgStore satisfies it (ReferencePrice returns the
// last trade price, nil when the instrument has never traded).
type PnlMarkSource interface {
	ReferencePrice(ctx context.Context, instrumentID int64) (*decimal.Decimal, error)
}

// PnlStore is the P&L read seam; PnlPgStore is the production
// implementation, tests substitute a fake.
type PnlStore interface {
	// PnlRows returns EVERY position row for the account (open and flat)
	// joined to its instrument symbol + quote currency — flat rows carry
	// the realized P&L accumulated before the position closed.
	PnlRows(ctx context.Context, accountID int64) ([]PnlRow, error)
	// AccountBaseCurrency returns accounts.base_currency.
	AccountBaseCurrency(ctx context.Context, accountID int64) (string, error)
	// AccountsHoldingInstrument returns the account ids with a non-zero
	// position on instrumentID (mark-update fanout).
	AccountsHoldingInstrument(ctx context.Context, instrumentID int64) ([]int64, error)
}

// PnlConverter converts a quote-currency P&L figure into the account
// base currency — *position.Converter satisfies it.
type PnlConverter interface {
	Convert(ctx context.Context, amount decimal.Decimal, from, to string) (position.Conversion, error)
}

// PnlPublisher is the private-stream push seam — *ws.Server satisfies it.
type PnlPublisher interface {
	PublishPrivate(accountID int64, channel string, data any)
}

// ---------------------------------------------------------------------------
// Model — money serializes as decimal strings, never floats
// ---------------------------------------------------------------------------

// PnlRow is one positions row joined to its instrument.
type PnlRow struct {
	InstrumentID  int64
	Symbol        string
	QuoteCurrency string
	Side          string // LONG | SHORT
	Quantity      decimal.Decimal
	EntryPrice    decimal.Decimal
	StoredMark    *decimal.Decimal
	RealizedPnL   decimal.Decimal // cumulative, quote currency
}

// PositionPnL is one position's live P&L figure.
type PositionPnL struct {
	InstrumentID  int64  `json:"instrument_id"`
	Symbol        string `json:"symbol"`
	Side          string `json:"side"`
	Quantity      string `json:"quantity"`
	EntryPrice    string `json:"entry_price"`
	MarkPrice     string `json:"mark_price"`
	MarkSource    string `json:"mark_source"` // last_trade | stored_mark | entry
	UnrealizedPnL string `json:"unrealized_pnl"`
	RealizedPnL   string `json:"realized_pnl"`
	QuoteCurrency string `json:"quote_currency"`
}

// CurrencyPnL aggregates one quote currency.
type CurrencyPnL struct {
	Currency      string `json:"currency"`
	RealizedPnL   string `json:"realized_pnl"`
	UnrealizedPnL string `json:"unrealized_pnl"`
	TotalPnL      string `json:"total_pnl"`
	// RateToBase / RatePath describe the conversion leg when a converter
	// is bound (omitted otherwise).
	RateToBase string `json:"rate_to_base,omitempty"`
	RatePath   string `json:"rate_path,omitempty"`
}

// PnlView is the GET /api/v1/account/pnl response model and the
// private:pnl event payload.
type PnlView struct {
	Event         string        `json:"event"` // "pnl"
	AccountID     int64         `json:"account_id"`
	BaseCurrency  string        `json:"base_currency"`
	Converted     bool          `json:"converted"` // false ⇒ totals are per-currency only
	RealizedPnL   *string       `json:"realized_pnl,omitempty"`
	UnrealizedPnL *string       `json:"unrealized_pnl,omitempty"`
	TotalPnL      *string       `json:"total_pnl,omitempty"`
	ByCurrency    []CurrencyPnL `json:"by_currency"`
	Positions     []PositionPnL `json:"positions"`
	TsMs          int64         `json:"ts_ms"`
}

// ---------------------------------------------------------------------------
// Service
// ---------------------------------------------------------------------------

// PnlService computes account P&L on demand and republishes on trade /
// mark events.
type PnlService struct {
	store PnlStore
	marks PnlMarkSource
	conv  PnlConverter // optional — nil ⇒ per-currency only
	push  PnlPublisher // optional — nil ⇒ no WS fanout
	now   func() time.Time
}

// PnlOptions wires the service; Store and Marks are required.
type PnlOptions struct {
	Store     PnlStore
	Marks     PnlMarkSource
	Converter PnlConverter
	Publisher PnlPublisher
	Now       func() time.Time
}

// NewPnlService builds the service; store and marks must be non-nil
// (fail-closed — a P&L service without reads must not exist).
func NewPnlService(o PnlOptions) (*PnlService, error) {
	if o.Store == nil {
		return nil, excerrors.New(CodeRiskLimitsInternal, "pnl: store is nil")
	}
	if o.Marks == nil {
		return nil, excerrors.New(CodeRiskLimitsInternal, "pnl: mark source is nil")
	}
	now := o.Now
	if now == nil {
		now = time.Now
	}
	return &PnlService{store: o.Store, marks: o.Marks, conv: o.Converter,
		push: o.Publisher, now: now}, nil
}

// markFor resolves the position's mark: oracle (last-trade placeholder)
// → stored position mark → entry price. A non-positive oracle price is
// never trusted — the lookup falls through to the stored mark (the
// converter's own MARK_PRICE_OUT_OF_BOUNDS discipline, applied at the
// source).
func (s *PnlService) markFor(ctx context.Context, r PnlRow) (decimal.Decimal, string, error) {
	if s.marks != nil {
		m, err := s.marks.ReferencePrice(ctx, r.InstrumentID)
		if err != nil {
			return decimal.Zero, "", fmt.Errorf("pnl: mark lookup %s: %w", r.Symbol, err)
		}
		if m != nil && m.IsPositive() {
			return *m, "last_trade", nil
		}
	}
	if r.StoredMark != nil && r.StoredMark.IsPositive() {
		return *r.StoredMark, "stored_mark", nil
	}
	// No mark anywhere — the cost basis is the only honest reference.
	return r.EntryPrice, "entry", nil
}

// Snapshot computes the account's current P&L view.
func (s *PnlService) Snapshot(ctx context.Context, accountID int64) (*PnlView, error) {
	base, err := s.store.AccountBaseCurrency(ctx, accountID)
	if err != nil {
		return nil, fmt.Errorf("pnl: account meta: %w", err)
	}
	rows, err := s.store.PnlRows(ctx, accountID)
	if err != nil {
		return nil, fmt.Errorf("pnl: positions: %w", err)
	}

	type agg struct {
		realized   decimal.Decimal
		unrealized decimal.Decimal
	}
	byCcy := map[string]*agg{}
	v := &PnlView{
		Event:        "pnl",
		AccountID:    accountID,
		BaseCurrency: strings.ToUpper(base),
		ByCurrency:   []CurrencyPnL{},
		Positions:    []PositionPnL{},
		TsMs:         s.now().UnixMilli(),
	}
	for _, r := range rows {
		mark, src, err := s.markFor(ctx, r)
		if err != nil {
			return nil, err
		}
		signed := r.Quantity
		if r.Side == "SHORT" {
			signed = signed.Neg()
		}
		// Unrealized = signedQty × (mark − entry), in quote currency,
		// rounded to the ledger quantum (DECIMAL(28,8)).
		u := signed.Mul(mark.Sub(r.EntryPrice)).Round(8)
		realized := r.RealizedPnL.Round(8)
		v.Positions = append(v.Positions, PositionPnL{
			InstrumentID:  r.InstrumentID,
			Symbol:        r.Symbol,
			Side:          r.Side,
			Quantity:      r.Quantity.String(),
			EntryPrice:    r.EntryPrice.String(),
			MarkPrice:     mark.String(),
			MarkSource:    src,
			UnrealizedPnL: u.String(),
			RealizedPnL:   realized.String(),
			QuoteCurrency: r.QuoteCurrency,
		})
		a := byCcy[r.QuoteCurrency]
		if a == nil {
			a = &agg{}
			byCcy[r.QuoteCurrency] = a
		}
		a.realized = a.realized.Add(realized)
		a.unrealized = a.unrealized.Add(u)
	}

	ccys := make([]string, 0, len(byCcy))
	for c := range byCcy {
		ccys = append(ccys, c)
	}
	sort.Strings(ccys)

	if s.conv != nil {
		var totR, totU decimal.Decimal
		for _, c := range ccys {
			a := byCcy[c]
			item := CurrencyPnL{
				Currency:      c,
				RealizedPnL:   a.realized.String(),
				UnrealizedPnL: a.unrealized.String(),
				TotalPnL:      a.realized.Add(a.unrealized).String(),
			}
			if c == v.BaseCurrency {
				item.RateToBase = "1"
				item.RatePath = "identity"
			} else {
				// One conversion per currency covers the realized +
				// unrealized legs (same rate — a second Convert call for
				// the unrealized leg would re-resolve the identical pair).
				rc, err := s.conv.Convert(ctx, a.realized, c, v.BaseCurrency)
				if err != nil {
					return nil, fmt.Errorf("pnl: convert %s→%s: %w", c, v.BaseCurrency, err)
				}
				uc, err := s.conv.Convert(ctx, a.unrealized, c, v.BaseCurrency)
				if err != nil {
					return nil, fmt.Errorf("pnl: convert %s→%s: %w", c, v.BaseCurrency, err)
				}
				totR = totR.Add(rc.ToAmount.Round(8))
				totU = totU.Add(uc.ToAmount.Round(8))
				item.RateToBase = rc.Rate.String()
				item.RatePath = rc.Path
			}
			if c == v.BaseCurrency {
				totR = totR.Add(a.realized)
				totU = totU.Add(a.unrealized)
			}
			v.ByCurrency = append(v.ByCurrency, item)
		}
		v.Converted = true
		rs := totR.Round(8).String()
		us := totU.Round(8).String()
		ts := totR.Add(totU).Round(8).String()
		v.RealizedPnL, v.UnrealizedPnL, v.TotalPnL = &rs, &us, &ts
		return v, nil
	}
	for _, c := range ccys {
		a := byCcy[c]
		v.ByCurrency = append(v.ByCurrency, CurrencyPnL{
			Currency:      c,
			RealizedPnL:   a.realized.String(),
			UnrealizedPnL: a.unrealized.String(),
			TotalPnL:      a.realized.Add(a.unrealized).String(),
		})
	}
	return v, nil
}

// pushAccount recomputes and publishes the account's view. Best-effort:
// a snapshot error is returned to the caller for logging but never
// blocks the fill pipeline.
func (s *PnlService) pushAccount(ctx context.Context, accountID int64) {
	if s.push == nil {
		return
	}
	v, err := s.Snapshot(ctx, accountID)
	if err != nil || v == nil {
		return
	}
	s.push.PublishPrivate(accountID, PnlChannel, v)
}

// OnTrade is the post-fill hook: republish the trading account's P&L and
// — because a fill moves the instrument's last-trade mark — republish
// every account holding that instrument (mark-price update path).
func (s *PnlService) OnTrade(ctx context.Context, accountID, instrumentID int64) {
	s.pushAccount(ctx, accountID)
	s.OnMarkUpdate(ctx, instrumentID)
}

// OnMarkUpdate republishes P&L for every account holding instrumentID —
// called whenever the mark source moved (Phase-19.5 wires the real
// oracle tick; today fills are the only mark-mutating event).
func (s *PnlService) OnMarkUpdate(ctx context.Context, instrumentID int64) {
	if s.push == nil {
		return
	}
	accounts, err := s.store.AccountsHoldingInstrument(ctx, instrumentID)
	if err != nil {
		return // fanout failure is logged at the call site via Snapshot errors
	}
	for _, id := range accounts {
		s.pushAccount(ctx, id)
	}
}

// ---------------------------------------------------------------------------
// PnlPgStore — PostgreSQL implementation
// ---------------------------------------------------------------------------

// PnlPgStore implements PnlStore over pgx. Numerics cross as ::text
// (same convention as PgStore).
type PnlPgStore struct {
	pool *pgxpool.Pool
}

// NewPnlPgStore wraps pool.
func NewPnlPgStore(pool *pgxpool.Pool) *PnlPgStore { return &PnlPgStore{pool: pool} }

// PnlRows returns every position row (open AND flat — flat rows carry
// realized P&L) joined to symbol + quote currency.
func (s *PnlPgStore) PnlRows(ctx context.Context, accountID int64) ([]PnlRow, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT p.instrument_id, i.symbol, i.quote_currency, p.side::text,
		       p.quantity::text, p.entry_price::text, p.mark_price::text,
		       p.realized_pnl::text
		FROM positions p
		JOIN instruments i ON i.id = p.instrument_id
		WHERE p.account_id = $1
		ORDER BY p.id`, accountID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []PnlRow
	for rows.Next() {
		var (
			r           PnlRow
			qty, ep, rp string
			mp          *string
		)
		if err := rows.Scan(&r.InstrumentID, &r.Symbol, &r.QuoteCurrency,
			&r.Side, &qty, &ep, &mp, &rp); err != nil {
			return nil, err
		}
		r.Quantity = decimal.RequireFromString(qty)
		r.EntryPrice = decimal.RequireFromString(ep)
		r.RealizedPnL = decimal.RequireFromString(rp)
		if mp != nil {
			d := decimal.RequireFromString(*mp)
			r.StoredMark = &d
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// AccountBaseCurrency resolves accounts.base_currency for conversion.
func (s *PnlPgStore) AccountBaseCurrency(ctx context.Context, accountID int64) (string, error) {
	var base string
	err := s.pool.QueryRow(ctx,
		`SELECT base_currency FROM accounts WHERE id = $1`, accountID).Scan(&base)
	if err == pgx.ErrNoRows {
		return "", excerrors.New("ACCOUNT_NOT_FOUND",
			fmt.Sprintf("pnl: account %d not found", accountID))
	}
	return base, err
}

// AccountsHoldingInstrument returns distinct accounts with open exposure.
func (s *PnlPgStore) AccountsHoldingInstrument(ctx context.Context, instrumentID int64) ([]int64, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT DISTINCT account_id FROM positions
		WHERE instrument_id = $1 AND quantity <> 0`, instrumentID)
	if err != nil {
		return nil, err
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

// ---------------------------------------------------------------------------
// LastTradeRates — placeholder Phase-19.5 oracle for the FX converter
// ---------------------------------------------------------------------------

// PairInstrumentResolver resolves an FX pair to a tradable instrument —
// PnlPgStore.PairInstrument implements it.
type PairInstrumentResolver interface {
	PairInstrument(ctx context.Context, base, quote string) (instrumentID int64, ok bool, err error)
}

// PairInstrument finds the instrument for an ordered currency pair.
func (s *PnlPgStore) PairInstrument(ctx context.Context, base, quote string) (int64, bool, error) {
	var id int64
	err := s.pool.QueryRow(ctx, `
		SELECT id FROM instruments
		WHERE base_currency = $1 AND quote_currency = $2
		ORDER BY id LIMIT 1`, base, quote).Scan(&id)
	if err == pgx.ErrNoRows {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, err
	}
	return id, true, nil
}

// LastTradeRates adapts the last-trade mark seam into a
// position.FXRateProvider — the Phase-19.5 PriceOracle placeholder for
// P&L conversion. A pair without an instrument or without any trade
// resolves ErrPairNotFound so position.Converter walks its
// direct→inverse→USD-cross chain.
type LastTradeRates struct {
	Instruments PairInstrumentResolver
	Marks       PnlMarkSource
}

// MidRate implements position.FXRateProvider: rate = units of
// pair.Quote per 1 pair.Base at the pair instrument's last trade.
func (r LastTradeRates) MidRate(ctx context.Context, p position.Pair) (decimal.Decimal, error) {
	id, ok, err := r.Instruments.PairInstrument(ctx, p.Base, p.Quote)
	if err != nil {
		return decimal.Zero, err
	}
	if !ok {
		return decimal.Zero, position.ErrPairNotFound
	}
	px, err := r.Marks.ReferencePrice(ctx, id)
	if err != nil {
		return decimal.Zero, err
	}
	if px == nil {
		return decimal.Zero, position.ErrPairNotFound
	}
	return *px, nil
}
