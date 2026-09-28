package balance

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"

	"exchange/internal/ledger"
	"exchange/internal/position"
	"exchange/pkg/decimal"
	excerrors "exchange/pkg/errors"
)

// ---------------------------------------------------------------------------
// In-memory Store + recording JournalPoster for unit tests.
// ---------------------------------------------------------------------------

type memProfile struct {
	base string
	mode string
}

type memStore struct {
	mu          sync.Mutex
	profiles    map[int64]memProfile
	balances    map[string]decimal.Decimal // "acct:ccy" → available
	conversions []ConversionRecord
}

func newMemStore() *memStore {
	return &memStore{
		profiles: map[int64]memProfile{},
		balances: map[string]decimal.Decimal{},
	}
}

func (m *memStore) AccountProfile(_ context.Context, accountID int64) (string, string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	p, ok := m.profiles[accountID]
	if !ok {
		return "", "", excerrors.New(CodeAccountNotFound, fmt.Sprintf("account %d not found", accountID))
	}
	return p.base, p.mode, nil
}

func balKey(accountID int64, ccy string) string { return fmt.Sprintf("%d:%s", accountID, ccy) }

func (m *memStore) ConversionExists(_ context.Context, rt string, refID int64, from string) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, c := range m.conversions {
		if c.ReferenceType == rt && c.ReferenceID == refID && c.FromCurrency == from {
			return true, nil
		}
	}
	return false, nil
}

func (m *memStore) RecordSettlement(_ context.Context, _ int64, recs ...ConversionRecord) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, c := range m.conversions {
		if c.ReferenceType == recs[0].ReferenceType &&
			c.ReferenceID == recs[0].ReferenceID &&
			c.FromCurrency == recs[0].FromCurrency {
			return true, nil // receipt exists → duplicate
		}
	}
	m.conversions = append(m.conversions, recs...)
	return false, nil
}

// recordingPoster mimics settlement.LedgerService: validates the journal
// AND applies wallet Effects (that's the real contract — balances move
// inside the poster, never via direct SQL here).
type recordingPoster struct {
	mu       sync.Mutex
	journals []ledger.Journal
	nextID   int64
	err      error
	apply    func(e ledger.AccountEffect)
}

func (p *recordingPoster) Post(_ context.Context, j ledger.Journal) (ledger.PostResult, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.err != nil {
		return ledger.PostResult{}, p.err
	}
	if err := j.Validate(); err != nil { // the real ledger re-validates too
		return ledger.PostResult{}, err
	}
	for _, e := range j.Effects {
		if p.apply != nil {
			p.apply(e)
		}
	}
	p.nextID++
	p.journals = append(p.journals, j)
	return ledger.PostResult{JournalID: p.nextID, Committed: true}, nil
}

// stub FX oracle
type stubRates map[position.Pair]decimal.Decimal

func (s stubRates) MidRate(_ context.Context, p position.Pair) (decimal.Decimal, error) {
	if v, ok := s[p]; ok {
		return v, nil
	}
	return decimal.Zero, position.ErrPairNotFound
}

func d(s string) decimal.Decimal { return decimal.MustFromString(s) }

func applyTo(st *memStore) func(e ledger.AccountEffect) {
	return func(e ledger.AccountEffect) {
		k := balKey(e.AccountID, e.Currency)
		st.balances[k] = st.balances[k].Add(e.AvailableDelta).Add(e.LockedDelta)
	}
}

func TestSettleQuoteCurrencyProfit(t *testing.T) {
	st := newMemStore()
	st.profiles[10] = memProfile{base: "USD", mode: SettleQuoteCurrency}
	poster := &recordingPoster{apply: applyTo(st)}
	svc := NewSettlementService(st, position.NewConverter(stubRates{}, ""), poster)

	res, err := svc.SettleRealizedPnL(context.Background(), SettlementRequest{
		AccountID: 10, TradeID: 100, QuoteCurrency: "GBP",
		RealizedPnL: d("250"), Description: "EUR/GBP close",
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.BookedCurrency != "GBP" || !res.BookedAmount.Equal(d("250")) || res.Converted {
		t.Fatalf("bad result: %+v", res)
	}
	if !st.balances["10:GBP"].Equal(d("250")) {
		t.Fatalf("GBP balance=%s want 250", st.balances["10:GBP"])
	}
	if len(poster.journals) != 1 {
		t.Fatalf("expected 1 journal, got %d", len(poster.journals))
	}
	j := poster.journals[0]
	// client profit: D 4040_GBP / C 2010_GBP
	if j.Lines[0].AccountCode != "4040_REALIZED_TRADING_PNL_GBP" || !j.Lines[0].Debit.Equal(d("250")) {
		t.Fatalf("line0: %+v", j.Lines[0])
	}
	if j.Lines[1].AccountCode != "2010_CUSTOMER_LIABILITY_GBP" || !j.Lines[1].Credit.Equal(d("250")) {
		t.Fatalf("line1: %+v", j.Lines[1])
	}
	if j.EntryType != ledger.EntrySettlement {
		t.Fatalf("entry_type=%s want SETTLEMENT", j.EntryType)
	}
	if j.IdempotencyKey != "realized-pnl:10:100" {
		t.Fatalf("idempotency key=%s", j.IdempotencyKey)
	}
}

func TestSettleQuoteCurrencyLoss(t *testing.T) {
	st := newMemStore()
	st.profiles[10] = memProfile{base: "USD", mode: SettleQuoteCurrency}
	poster := &recordingPoster{apply: applyTo(st)}
	svc := NewSettlementService(st, position.NewConverter(stubRates{}, ""), poster)

	res, err := svc.SettleRealizedPnL(context.Background(), SettlementRequest{
		AccountID: 10, TradeID: 101, QuoteCurrency: "JPY", RealizedPnL: d("-15000"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Converted || res.BookedCurrency != "JPY" {
		t.Fatalf("quote-mode loss booked wrongly: %+v", res)
	}
	if !st.balances["10:JPY"].Equal(d("-15000")) {
		t.Fatalf("JPY balance=%s want -15000", st.balances["10:JPY"])
	}
	j := poster.journals[0]
	// client loss: D 2010_JPY / C 4040_JPY
	if j.Lines[0].AccountCode != "2010_CUSTOMER_LIABILITY_JPY" || !j.Lines[0].Debit.Equal(d("15000")) {
		t.Fatalf("line0: %+v", j.Lines[0])
	}
}

func TestSettleSweepToBase(t *testing.T) {
	st := newMemStore()
	st.profiles[10] = memProfile{base: "USD", mode: SettleSweepToBase}
	poster := &recordingPoster{apply: applyTo(st)}
	conv := position.NewConverter(stubRates{position.Pair{Base: "GBP", Quote: "USD"}: d("1.25")}, "")
	svc := NewSettlementService(st, conv, poster)

	res, err := svc.SettleRealizedPnL(context.Background(), SettlementRequest{
		AccountID: 10, TradeID: 102, QuoteCurrency: "GBP", RealizedPnL: d("80"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if !res.Converted || res.BookedCurrency != "USD" || !res.BookedAmount.Equal(d("100")) {
		t.Fatalf("bad sweep result: %+v", res)
	}
	if !st.balances["10:USD"].Equal(d("100")) {
		t.Fatalf("USD balance=%s want 100", st.balances["10:USD"])
	}
	if _, ok := st.balances["10:GBP"]; ok {
		t.Fatal("quote balance must not move in sweep mode")
	}
	j := poster.journals[0]
	if len(j.Lines) != 4 {
		t.Fatalf("sweep journal must have 4 lines, got %d", len(j.Lines))
	}
	// GBP leg: D 4040_GBP 80 / C 1200_GBP 80 ; USD leg: D 1200_USD 100 / C 2010_USD 100
	if j.Lines[0].AccountCode != "4040_REALIZED_TRADING_PNL_GBP" || !j.Lines[0].Debit.Equal(d("80")) {
		t.Fatalf("line0: %+v", j.Lines[0])
	}
	if j.Lines[1].AccountCode != "1200_MULTI_CURRENCY_CLEARING_GBP" || !j.Lines[1].Credit.Equal(d("80")) {
		t.Fatalf("line1: %+v", j.Lines[1])
	}
	if j.Lines[3].AccountCode != "2010_CUSTOMER_LIABILITY_USD" || !j.Lines[3].Credit.Equal(d("100")) {
		t.Fatalf("line3: %+v", j.Lines[3])
	}
	// two conversion audit rows: settle receipt + sweep
	if len(st.conversions) != 2 {
		t.Fatalf("expected 2 conversion rows, got %d", len(st.conversions))
	}
	if st.conversions[1].ReferenceType != RefRealizedPnLSweep || !st.conversions[1].Rate.Equal(d("1.25")) {
		t.Fatalf("sweep record: %+v", st.conversions[1])
	}
}

func TestSettleSweepLoss(t *testing.T) {
	st := newMemStore()
	st.profiles[10] = memProfile{base: "USD", mode: SettleSweepToBase}
	poster := &recordingPoster{apply: applyTo(st)}
	conv := position.NewConverter(stubRates{position.Pair{Base: "USD", Quote: "JPY"}: d("150")}, "")
	svc := NewSettlementService(st, conv, poster)

	res, err := svc.SettleRealizedPnL(context.Background(), SettlementRequest{
		AccountID: 10, TradeID: 103, QuoteCurrency: "JPY", RealizedPnL: d("-15000"),
	})
	if err != nil {
		t.Fatal(err)
	}
	// -15000 JPY → -100 USD debited from base
	if !res.BookedAmount.Round(8).Equal(d("-100")) || !st.balances["10:USD"].Round(8).Equal(d("-100")) {
		t.Fatalf("loss sweep wrong: %+v bal=%s", res, st.balances["10:USD"])
	}
}

func TestSettleDuplicateRejected(t *testing.T) {
	st := newMemStore()
	st.profiles[10] = memProfile{base: "USD", mode: SettleQuoteCurrency}
	poster := &recordingPoster{apply: applyTo(st)}
	svc := NewSettlementService(st, position.NewConverter(stubRates{}, ""), poster)
	req := SettlementRequest{AccountID: 10, TradeID: 200, QuoteCurrency: "USD", RealizedPnL: d("50")}
	if _, err := svc.SettleRealizedPnL(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	res, err := svc.SettleRealizedPnL(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Duplicate {
		t.Fatal("replayed settlement not flagged duplicate")
	}
	if !st.balances["10:USD"].Equal(d("50")) {
		t.Fatalf("double-booked: USD=%s want 50", st.balances["10:USD"])
	}
}

func TestSettleZeroPnLNoOp(t *testing.T) {
	st := newMemStore()
	st.profiles[10] = memProfile{base: "USD", mode: SettleQuoteCurrency}
	poster := &recordingPoster{}
	svc := NewSettlementService(st, position.NewConverter(stubRates{}, ""), poster)
	res, err := svc.SettleRealizedPnL(context.Background(), SettlementRequest{
		AccountID: 10, TradeID: 300, QuoteCurrency: "USD", RealizedPnL: decimal.Zero,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !res.NoOp || len(poster.journals) != 0 {
		t.Fatalf("zero P&L must be a no-op: %+v", res)
	}
}

func TestSettleNoPosterFailsClosed(t *testing.T) {
	st := newMemStore()
	st.profiles[10] = memProfile{base: "USD", mode: SettleQuoteCurrency}
	svc := NewSettlementService(st, position.NewConverter(stubRates{}, ""), nil)
	_, err := svc.SettleRealizedPnL(context.Background(), SettlementRequest{
		AccountID: 10, TradeID: 400, QuoteCurrency: "USD", RealizedPnL: d("10"),
	})
	var e *excerrors.Error
	if !errors.As(err, &e) || e.Code != CodeJournalPosterNil {
		t.Fatalf("want LEDGER_POSTER_UNAVAILABLE, got %v", err)
	}
	if len(st.balances) != 0 {
		t.Fatal("balance mutated without journal posting")
	}
}

func TestSettleMissingAccountFails(t *testing.T) {
	st := newMemStore()
	poster := &recordingPoster{}
	svc := NewSettlementService(st, position.NewConverter(stubRates{}, ""), poster)
	_, err := svc.SettleRealizedPnL(context.Background(), SettlementRequest{
		AccountID: 999, TradeID: 500, QuoteCurrency: "USD", RealizedPnL: d("10"),
	})
	var e *excerrors.Error
	if !errors.As(err, &e) || e.Code != CodeAccountNotFound {
		t.Fatalf("want ACCOUNT_NOT_FOUND, got %v", err)
	}
}

func TestSettlePosterErrorAborts(t *testing.T) {
	st := newMemStore()
	st.profiles[10] = memProfile{base: "USD", mode: SettleQuoteCurrency}
	poster := &recordingPoster{err: errors.New("ledger down")}
	svc := NewSettlementService(st, position.NewConverter(stubRates{}, ""), poster)
	_, err := svc.SettleRealizedPnL(context.Background(), SettlementRequest{
		AccountID: 10, TradeID: 600, QuoteCurrency: "USD", RealizedPnL: d("10"),
	})
	if err == nil {
		t.Fatal("poster failure must abort settlement")
	}
	if len(st.conversions) != 0 {
		t.Fatal("audit row written despite aborted settlement")
	}
}
