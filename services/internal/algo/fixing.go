// Package algo — the Go half of Phase-16 advanced order types.
//
// This file owns Task 16.3.9 benchmark fixing orders: admission cutoff,
// balance reservations, the fix-time executor that consumes
// benchmark_fixings rows (migration 222, Phase-15 Task 15.3.13) and
// crosses queued orders at the published rate, plus the honest residual
// seam when the book cannot cross (no fabricated LP fills).
package algo

import (
	"context"
	"encoding/json"
	"fmt"

	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"exchange/internal/instruments"
	"exchange/internal/ledger"
	"exchange/internal/orders"
	"exchange/pkg/decimal"
	excerrors "exchange/pkg/errors"
)

// ---------------------------------------------------------------------------
// Benchmark vocabulary bridge (Task 16.3.9)
// ---------------------------------------------------------------------------

// The orders table stores the canonical spec §6.4 order-level vocabulary
// (WM_R_4PM / ECB_1415 / TOKYO_0955 — supersedes the plan strings
// WM_REFINITIV_4PM_LDN / ECB_1415_CET). The auction_calendar +
// benchmark_fixings tables use the scheduler vocabulary
// (WM_LONDON_4PM / ECB_REF_1415 / TOKYO_0955). The mapping below is the
// single translation point — never mix the vocabularies implicitly.
func SchedulerBenchmark(orderBenchmark string) (string, bool) {
	switch orderBenchmark {
	case orders.FixingBenchmarkWMR4PM:
		return instruments.BenchWMLondon, true
	case orders.FixingBenchmarkECB1415:
		return instruments.BenchECB, true
	case orders.FixingBenchmarkTokyo0955:
		return instruments.BenchTokyo, true
	}
	return "", false
}

// OrderBenchmark maps a scheduler benchmark back to the order-level
// vocabulary — the direction the executor needs when selecting orders
// for a benchmark_fixings row.
func OrderBenchmark(schedulerBenchmark string) (string, bool) {
	switch schedulerBenchmark {
	case instruments.BenchWMLondon:
		return orders.FixingBenchmarkWMR4PM, true
	case instruments.BenchECB:
		return orders.FixingBenchmarkECB1415, true
	case instruments.BenchTokyo:
		return orders.FixingBenchmarkTokyo0955, true
	}
	return "", false
}

// FixingCutoff is the T-15-minute order-entry/cancellation cutoff before
// a benchmark publication (spec §6.4).
const FixingCutoff = 15 * time.Minute

// ---------------------------------------------------------------------------
// Seams
// ---------------------------------------------------------------------------

// JournalPoster is the full-path ledger posting seam —
// *settlement.LedgerService satisfies it (account locks + SERIALIZABLE +
// post-commit dispatch).
type JournalPoster interface {
	Post(ctx context.Context, j ledger.Journal) (ledger.PostResult, error)
}

// TxJournalPoster is the in-tx ledger posting seam — the fix executor
// settles the cross atomically with the order/trade/audit writes inside
// one SERIALIZABLE transaction (caller locks the wallet + order rows).
type TxJournalPoster interface {
	PostJournal(ctx context.Context, tx pgx.Tx, j ledger.Journal) (ledger.PostResult, error)
}

// CalendarSource resolves a symbol's auction_calendar rows —
// *instruments.CalendarStore satisfies it.
type CalendarSource interface {
	CalendarFor(ctx context.Context, symbol string) ([]instruments.CalendarEntry, error)
}

// RefPricer is the reference-price seam (orders.PgStore satisfies it) —
// used to size the worst-case buy-side reservation.
type RefPricer interface {
	ReferencePrice(ctx context.Context, instrumentID int64) (*decimal.Decimal, error)
}

// EventPublisher is the post-commit balance-event fan-out
// (settlement.NatsPublisher satisfies it); nil-tolerated.
type EventPublisher interface {
	Publish(ctx context.Context, subject string, payload []byte) error
}

// ---------------------------------------------------------------------------
// Service
// ---------------------------------------------------------------------------

// FixingService implements orders.FixingHooks (admission + reservation
// lifecycle) and the benchmark_fixings consumer (Tick).
type FixingService struct {
	pool    *pgxpool.Pool
	poster  JournalPoster   // submit-path journals (reserve/release)
	txPost  TxJournalPoster // executor fill journals (in-tx)
	cal     CalendarSource
	ref     RefPricer
	pub     EventPublisher
	valueAt func(base, quote string, tradeDate time.Time) (time.Time, error)
	now     func() time.Time
	logf    func(format string, args ...any)
}

// FixingDeps wires the service; every seam is required except the
// optional event publisher + value-date resolver (documented fallbacks).
type FixingDeps struct {
	Pool      *pgxpool.Pool
	Poster    JournalPoster   // *settlement.LedgerService
	TxPoster  TxJournalPoster // same service — separate seam for tests
	Calendar  CalendarSource  // *instruments.CalendarStore
	Ref       RefPricer       // *orders.PgStore (or any ReferencePrice impl)
	Pub       EventPublisher  // optional — balance:changed fan-out
	ValueDate func(base, quote string, tradeDate time.Time) (time.Time, error)
	Now       func() time.Time
	Logf      func(format string, args ...any)
}

func NewFixingService(d FixingDeps) (*FixingService, error) {
	if d.Pool == nil {
		return nil, fmt.Errorf("algo: fixing: pool is nil")
	}
	if d.Poster == nil || d.TxPoster == nil {
		return nil, fmt.Errorf("algo: fixing: ledger posters are nil (fail closed)")
	}
	if d.Calendar == nil {
		return nil, fmt.Errorf("algo: fixing: calendar source is nil")
	}
	s := &FixingService{
		pool: d.Pool, poster: d.Poster, txPost: d.TxPoster,
		cal: d.Calendar, ref: d.Ref, pub: d.Pub,
		valueAt: d.ValueDate, now: d.Now, logf: d.Logf,
	}
	if s.now == nil {
		s.now = func() time.Time { return time.Now().UTC() }
	}
	if s.logf == nil {
		s.logf = func(string, ...any) {}
	}
	return s, nil
}

// ---------------------------------------------------------------------------
// Admission + mutability gate (T-15 cutoff)
// ---------------------------------------------------------------------------

// nextFixing returns the next publication instant for the order-level
// benchmark on this symbol's auction_calendar.
func (s *FixingService) nextFixing(ctx context.Context, inst *orders.Instrument,
	orderBenchmark string, now time.Time) (time.Time, error) {
	schedBench, ok := SchedulerBenchmark(orderBenchmark)
	if !ok {
		return time.Time{}, excerrors.New("INVALID_REQUEST",
			"unknown fixing benchmark "+orderBenchmark)
	}
	rows, err := s.cal.CalendarFor(ctx, inst.Symbol)
	if err != nil {
		return time.Time{}, err
	}
	var best time.Time
	for _, e := range rows {
		if !e.Enabled || e.AuctionType != instruments.AuctionFixing ||
			e.Benchmark != schedBench {
			continue
		}
		at, err := e.NextOccurrence(now)
		if err != nil {
			continue // unresolvable recurrence — skip, keep scanning
		}
		if best.IsZero() || at.Before(best) {
			best = at
		}
	}
	if best.IsZero() {
		return time.Time{}, excerrors.New("INVALID_REQUEST",
			fmt.Sprintf("no enabled %s fixing window on %s", orderBenchmark, inst.Symbol))
	}
	return best, nil
}

// AdmitSubmit enforces the T-15 cutoff for FIXING submissions.
func (s *FixingService) AdmitSubmit(ctx context.Context, inst *orders.Instrument,
	req *orders.SubmitRequest, now time.Time) error {
	at, err := s.nextFixing(ctx, inst, req.FixingBenchmark, now)
	if err != nil {
		return err
	}
	if !now.Before(at.Add(-FixingCutoff)) {
		return excerrors.New("FIXING_CUTOFF_EXCEEDED", fmt.Sprintf(
			"%s fixing on %s publishes at %s — order entry closed %s before",
			req.FixingBenchmark, inst.Symbol, at.UTC().Format(time.RFC3339), FixingCutoff))
	}
	return nil
}

// AssertMutable gates cancel/modify through the same cutoff — once the
// window opens the queued order is committed to the fix.
func (s *FixingService) AssertMutable(ctx context.Context, o *orders.Order,
	inst *orders.Instrument, now time.Time) error {
	if o.FixingBenchmark == nil {
		return excerrors.New("FIXING_CANCELLATION_RESTRICTED",
			"fixing order carries no benchmark")
	}
	at, err := s.nextFixing(ctx, inst, *o.FixingBenchmark, now)
	if err != nil {
		// No upcoming occurrence — the queue is done with this row; a
		// cancel against a finished window stays admissible so stale
		// queue entries can always be unwound.
		return nil
	}
	if !now.Before(at.Add(-FixingCutoff)) {
		return excerrors.New("FIXING_CANCELLATION_RESTRICTED", fmt.Sprintf(
			"%s fixing window is open (publication %s) — cancels/amends locked",
			*o.FixingBenchmark, at.UTC().Format(time.RFC3339)))
	}
	return nil
}

// ---------------------------------------------------------------------------
// Reservations (balance lock through the GL path — never direct writes)
// ---------------------------------------------------------------------------

// fixingReservation is the reservation evidence persisted inside
// orders.algo_params — {currency, amount, consumed}.
type fixingReservation struct {
	Currency string `json:"currency"`
	Amount   string `json:"amount"`
	Consumed string `json:"consumed"`
}

func reservationOf(params []byte) (*fixingReservation, error) {
	if len(params) == 0 {
		return nil, nil
	}
	var p struct {
		Res *fixingReservation `json:"fixing_reservation"`
	}
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, fmt.Errorf("algo: fixing: algo_params unreadable: %w", err)
	}
	return p.Res, nil
}

func (r *fixingReservation) amount() decimal.Decimal {
	d, err := decimal.NewFromString(r.Amount)
	if err != nil {
		return decimal.Zero
	}
	return d
}

func (r *fixingReservation) consumed() decimal.Decimal {
	d, err := decimal.NewFromString(r.Consumed)
	if err != nil {
		return decimal.Zero
	}
	return d
}

func (r *fixingReservation) remaining() decimal.Decimal {
	rem := r.amount().Sub(r.consumed())
	if rem.IsNegative() {
		return decimal.Zero
	}
	return rem
}

// reserveJournal builds the available→locked shift:
// debit 2010 (client liability out of free liability) / credit 2160
// (clearing transit — the reserved slot); wallet effect is net-zero.
func reserveJournal(orderID int64, ccy string, amount decimal.Decimal, narrative string) ledger.Journal {
	return ledger.Journal{
		EntryType:      ledger.EntryAdjustment,
		ReferenceID:    orderID,
		Description:    truncate255(narrative),
		PostedBy:       "algo-fixing",
		IdempotencyKey: fmt.Sprintf("fixing-reserve:%d", orderID),
		Lines: []ledger.Line{
			ledger.DebitLine("2010_CUSTOMER_LIABILITY", ccy, amount,
				"fixing order reservation released from client liability"),
			ledger.CreditLine("2160_CLEARING_TRANSIT", ccy, amount,
				"fixing order reservation held in clearing transit"),
		},
		Effects: []ledger.AccountEffect{{
			// AccountID patched by the caller (the journal builder stays
			// order-id agnostic).
			Currency:       ccy,
			AvailableDelta: amount.Neg(),
			LockedDelta:    amount,
		}},
	}
}

// releaseJournal is the mirror image: debit transit, credit client
// liability; wallet shift locked→available.
func releaseJournal(orderID, accountID int64, ccy string, amount decimal.Decimal,
	key, narrative string) ledger.Journal {
	return ledger.Journal{
		EntryType:      ledger.EntryAdjustment,
		ReferenceID:    orderID,
		Description:    truncate255(narrative),
		PostedBy:       "algo-fixing",
		IdempotencyKey: key,
		Lines: []ledger.Line{
			ledger.DebitLine("2160_CLEARING_TRANSIT", ccy, amount,
				"fixing reservation released from clearing transit"),
			ledger.CreditLine("2010_CUSTOMER_LIABILITY", ccy, amount,
				"fixing reservation returned to client liability"),
		},
		Effects: []ledger.AccountEffect{{
			AccountID:      accountID,
			Currency:       ccy,
			AvailableDelta: amount,
			LockedDelta:    amount.Neg(),
		}},
	}
}

// reservationNeed sizes the worst-case lock for a FIXING order:
// SELL reserves the full base quantity; BUY reserves qty × reference
// × (1 + price_band_up) — the same worst-case cap checkBalance applies.
// A missing reference on a BUY fails closed INSUFFICIENT_BALANCE.
func (s *FixingService) reservationNeed(ctx context.Context, o *orders.Order,
	inst *orders.Instrument) (currency string, amount decimal.Decimal, err error) {
	if o.Side == orders.SideSell {
		return inst.BaseCurrency, o.Quantity, nil
	}
	ref, err := s.ref.ReferencePrice(ctx, inst.ID)
	if err != nil {
		return "", decimal.Zero, fmt.Errorf("reference price read: %w", err)
	}
	if ref == nil || !ref.IsPositive() {
		return "", decimal.Zero, excerrors.New("INSUFFICIENT_BALANCE",
			"cannot size fixing reservation without a reference price")
	}
	cap_ := ref.Mul(decimal.One.Add(inst.PriceBandPctUp.Div(decimal.NewFromInt(100))))
	return inst.QuoteCurrency, o.Quantity.Mul(cap_).Round(8), nil
}

// Reserve posts the placement reservation and stamps it into
// orders.algo_params for later release/adjust/execution.
func (s *FixingService) Reserve(ctx context.Context, o *orders.Order, inst *orders.Instrument) error {
	ccy, amount, err := s.reservationNeed(ctx, o, inst)
	if err != nil {
		return err
	}
	j := reserveJournal(o.ID, ccy, amount, fmt.Sprintf(
		"FIXING order %d %s reservation %.8s %s", o.ID, *o.FixingBenchmark, amount, ccy))
	j.Effects[0].AccountID = o.AccountID
	if _, err := s.poster.Post(ctx, j); err != nil {
		return err
	}
	res := fixingReservation{Currency: ccy, Amount: amount.String(), Consumed: "0"}
	blob, _ := json.Marshal(map[string]any{"fixing_reservation": res})
	_, err = s.pool.Exec(ctx, `
		UPDATE orders SET algo_params = COALESCE(algo_params,'{}'::jsonb) || $2::jsonb
		WHERE id = $1`, o.ID, blob)
	return err
}

// Release returns the unspent reservation. Idempotent — the journal key
// dedups replays and a nil/zero reservation is a no-op.
func (s *FixingService) Release(ctx context.Context, o *orders.Order, inst *orders.Instrument) error {
	res, err := reservationOf(o.AlgoParams)
	if err != nil {
		return err
	}
	if res == nil {
		return nil // nothing was ever reserved (pre-hook rows)
	}
	rem := res.remaining()
	if !rem.IsPositive() {
		return nil
	}
	j := releaseJournal(o.ID, o.AccountID, res.Currency, rem,
		fmt.Sprintf("fixing-release:%d", o.ID),
		fmt.Sprintf("FIXING order %d reservation release %.8s %s", o.ID, rem, res.Currency))
	if _, err := s.poster.Post(ctx, j); err != nil {
		return err
	}
	// Mark the reservation fully consumed so a second release no-ops.
	blob, _ := json.Marshal(map[string]any{"fixing_reservation": fixingReservation{
		Currency: res.Currency, Amount: res.Amount, Consumed: res.Amount,
	}})
	_, err = s.pool.Exec(ctx, `
		UPDATE orders SET algo_params = COALESCE(algo_params,'{}'::jsonb) || $2::jsonb
		WHERE id = $1`, o.ID, blob)
	return err
}

// AdjustReservation re-sizes the lock after a quantity amend: extra need
// locks more, reduced need releases the surplus — one delta journal.
func (s *FixingService) AdjustReservation(ctx context.Context, o *orders.Order, inst *orders.Instrument) error {
	res, err := reservationOf(o.AlgoParams)
	if err != nil {
		return err
	}
	if res == nil {
		// Never reserved (pre-hook row) — reserve for the amended size.
		return s.Reserve(ctx, o, inst)
	}
	ccy, need, err := s.reservationNeed(ctx, o, inst)
	if err != nil {
		return err
	}
	// Post-amend total reservation target = consumed + remaining need on
	// the residual quantity.
	need = need.Add(res.consumed()).Round(8)
	delta := need.Sub(res.amount())
	if delta.IsZero() {
		return nil
	}
	var j ledger.Journal
	if delta.IsPositive() {
		j = reserveJournal(o.ID, ccy, delta, fmt.Sprintf(
			"FIXING order %d reservation top-up %.8s %s", o.ID, delta, ccy))
		j.Effects[0].AccountID = o.AccountID
		j.IdempotencyKey = fmt.Sprintf("fixing-adjust:%d:%d", o.ID, o.OrderSeq)
	} else {
		j = releaseJournal(o.ID, o.AccountID, ccy, delta.Neg(),
			fmt.Sprintf("fixing-adjust:%d:%d", o.ID, o.OrderSeq),
			fmt.Sprintf("FIXING order %d reservation release %.8s %s", o.ID, delta.Neg(), ccy))
	}
	if _, err := s.poster.Post(ctx, j); err != nil {
		return err
	}
	blob, _ := json.Marshal(map[string]any{"fixing_reservation": fixingReservation{
		Currency: res.Currency, Amount: need.String(), Consumed: res.Consumed,
	}})
	_, err = s.pool.Exec(ctx, `
		UPDATE orders SET algo_params = COALESCE(algo_params,'{}'::jsonb) || $2::jsonb
		WHERE id = $1`, o.ID, blob)
	return err
}

// ---------------------------------------------------------------------------
// Executor — consume benchmark_fixings rows, cross at the published rate
// ---------------------------------------------------------------------------

// fixingRow is one RECORDED benchmark_fixings row awaiting execution.
type fixingRow struct {
	ID           int64
	InstrumentID int64
	Symbol       string
	Benchmark    string
	ScheduledAt  time.Time
	Rate         decimal.Decimal
}

// fixingOrder is the queued-order projection the executor works on.
type fixingOrder struct {
	ID           int64
	AccountID    int64
	InstrumentID int64
	Side         string
	Quantity     decimal.Decimal
	FilledQty    decimal.Decimal
	Status       string
	AlgoParams   []byte
	ShardID      *int
}

func (o *fixingOrder) remaining() decimal.Decimal {
	r := o.Quantity.Sub(o.FilledQty)
	if r.IsNegative() {
		return decimal.Zero
	}
	return r
}

// Run drives Tick until ctx cancels.
func (s *FixingService) Run(ctx context.Context, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if n, err := s.Tick(ctx); err != nil {
				s.logf("fixing executor tick: %v", err)
			} else if n > 0 {
				s.logf("fixing executor: %d fill(s) executed", n)
			}
		}
	}
}

// Tick executes every RECORDED fixing row once per eligible order. Each
// cross settles atomically: order rows FOR UPDATE → fill journal
// (PostJournal in-tx) → order updates → trades row → audit → commit.
// A SKIPPED row never reaches here — no fills are fabricated off a
// missing rate.
func (s *FixingService) Tick(ctx context.Context) (int, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id, instrument_id, symbol, benchmark, scheduled_at, rate::text
		FROM benchmark_fixings
		WHERE status = 'RECORDED' AND rate IS NOT NULL
		ORDER BY scheduled_at, id`)
	if err != nil {
		return 0, err
	}
	var fixes []fixingRow
	for rows.Next() {
		var f fixingRow
		var rate *string
		if err := rows.Scan(&f.ID, &f.InstrumentID, &f.Symbol, &f.Benchmark,
			&f.ScheduledAt, &rate); err != nil {
			rows.Close()
			return 0, err
		}
		f.Rate = decimal.RequireFromString(*rate)
		fixes = append(fixes, f)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}

	fills := 0
	for _, f := range fixes {
		ob, ok := OrderBenchmark(f.Benchmark)
		if !ok {
			s.logf("fixing %d: unmapped benchmark %s — skipped", f.ID, f.Benchmark)
			continue
		}
		os, err := s.openFixingOrders(ctx, f.InstrumentID, ob,
			f.ScheduledAt.Add(-FixingCutoff))
		if err != nil {
			return fills, err
		}
		var buys, sells []*fixingOrder
		for _, o := range os {
			if o.Side == orders.SideBuy {
				buys = append(buys, o)
			} else {
				sells = append(sells, o)
			}
		}
		// Deterministic order: order-id sequence on both sides; a cross
		// consumes the smaller residual, so at least one side advances.
		i, j := 0, 0
		for i < len(buys) && j < len(sells) {
			b, sl := buys[i], sells[j]
			res, err := s.executeCross(ctx, &f, b.ID, sl.ID)
			if err != nil {
				return fills, fmt.Errorf("fixing %d cross %d↔%d: %w",
					f.ID, b.ID, sl.ID, err)
			}
			if res.Qty.IsPositive() {
				fills++
				b.FilledQty = b.FilledQty.Add(res.Qty)
				sl.FilledQty = sl.FilledQty.Add(res.Qty)
			}
			if res.BuyDone || b.remaining().IsZero() {
				i++
			}
			if res.SellDone || sl.remaining().IsZero() {
				j++
			}
		}
		// Residual seam: unmatched orders stay queued for the next
		// publication (or cancel/expire honestly). The imbalance is
		// recorded per order per fixing — the institutional-LP leg is a
		// documented seam; no fabricated fills are ever injected.
		for _, o := range append(append([]*fixingOrder{}, buys[i:]...), sells[j:]...) {
			if err := s.markImbalance(ctx, &f, o); err != nil {
				s.logf("fixing %d imbalance audit order %d: %v", f.ID, o.ID, err)
			}
		}
	}
	return fills, nil
}

// openFixingOrders selects the queued FIXING orders eligible for this
// publication: the order-level benchmark must match and the order must
// have queued at/before the T-15-minute cutoff — admission enforces it,
// the query repeats it so a publication can never sweep in post-cutoff
// orders.
func (s *FixingService) openFixingOrders(ctx context.Context, instrumentID int64,
	orderBenchmark string, at time.Time) ([]*fixingOrder, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id, account_id, instrument_id, side::text, quantity::text,
		       filled_qty::text, status::text, algo_params, shard_id
		FROM orders
		WHERE instrument_id = $1 AND order_type = 'FIXING'
		  AND fixing_benchmark = $2
		  AND status IN ('RESERVED','PARTIALLY_FILLED')
		  AND created_at <= $3
		ORDER BY id`, instrumentID, orderBenchmark, at)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*fixingOrder
	for rows.Next() {
		var o fixingOrder
		var qty, filled, status string
		var params []byte
		if err := rows.Scan(&o.ID, &o.AccountID, &o.InstrumentID, &o.Side,
			&qty, &filled, &status, &params, &o.ShardID); err != nil {
			return nil, err
		}
		o.Quantity = decimal.RequireFromString(qty)
		o.FilledQty = decimal.RequireFromString(filled)
		o.Status = status
		o.AlgoParams = params
		out = append(out, &o)
	}
	return out, rows.Err()
}

// Compile-time contract check — the orders seam must be satisfied.
var _ orders.FixingHooks = (*FixingService)(nil)
