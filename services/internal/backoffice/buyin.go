// Task 24.3.13 (continued) — CSDR mandatory buy-in lifecycle.
//
// CSD scope ONLY (settlement_fails.regime='CSDR'): at ISD+4 the extension
// period ends and a buy-in notification goes to the failing party; at
// ISD+7 the buy-in executes — equivalent position acquired at market, the
// price differential charged to the failing counterparty (GL claim on
// 1020_SETTLEMENT_FAIL_CLAIM). FX_CLOSEOUT fails never reach this path —
// ops_hardening.go handles them under §17.14 (Task 24.3.19 supersession).

package backoffice

import (
	"context"
	stderrors "errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"exchange/pkg/decimal"
	excerrors "exchange/pkg/errors"
)

// Buy-in timetable (CSDR regime, per Task 24.3.13 step 3).
const (
	BuyInNotifyDays  = 4 // ISD+4 — notification to failing party
	BuyInExecuteDays = 7 // ISD+7 — execution at market
)

// BuyInEvent is one buy_in_events row.
type BuyInEvent struct {
	ID                int64
	FailID            int64
	Kind              string // NOTIFICATION | EXECUTION
	MarketPrice       decimal.Decimal
	OriginalPrice     decimal.Decimal
	PriceDifferential decimal.Decimal
	Currency          string
	Status            string // ISSUED | ACKED | EXECUTED | CANCELLED
	CreatedAt         time.Time
}

// MarketPricer is the market-price seam for buy-in execution (and the
// §17.14 FX replacement-cost close-out) — production binds the mark/oracle
// mid (Phase-19.5). Nil pricer → executions refuse (fail closed).
type MarketPricer interface {
	MarkPrice(ctx context.Context, tradeID int64) (decimal.Decimal, error)
}

// BuyInStore is the persistence seam; production impl is pgxBuyInTx
// riding PgxFailStore's InTx.
type BuyInStore interface {
	InTx(ctx context.Context, fn func(ctx context.Context, tx BuyInTx) error) error
}

// BuyInTx is the transactional view.
type BuyInTx interface {
	// CSDRFailsDue lists CSDR-regime fails whose ISD is on/before
	// isdBefore and whose status is in `states`, FOR UPDATE.
	CSDRFailsDue(ctx context.Context, isdBefore time.Time, states []FailStatus) ([]SettlementFail, error)
	// InsertBuyIn records one event; UNIQUE(fail_id, kind) makes the
	// notification/execution replays idempotent — created=false on replay.
	InsertBuyIn(ctx context.Context, e BuyInEvent) (BuyInEvent, bool, error)
	SetFailStatus(ctx context.Context, id int64, st FailStatus, at time.Time) error
	// TradePrice resolves the failing leg's original rate (trades.price).
	TradePrice(ctx context.Context, tradeID int64) (decimal.Decimal, bool, error)
	// PostJournal writes the price-differential GL claim.
	PostJournal(ctx context.Context, entryType, description, postedBy, idemKey string,
		referenceID int64, lines []JournalLine) (int64, error)
}

// BuyInService drives the ISD+4 notification / ISD+7 execution ladder.
type BuyInService struct {
	store   BuyInStore
	pricer  MarketPricer
	now     func() time.Time
	onAlert func(ctx context.Context, code, summary string, detail map[string]any)
}

// NewBuyInService wires the service; nil pricer fails executions closed.
func NewBuyInService(store BuyInStore, pricer MarketPricer,
	onAlert func(ctx context.Context, code, summary string, detail map[string]any)) *BuyInService {
	return &BuyInService{store: store, pricer: pricer, onAlert: onAlert,
		now: func() time.Time { return time.Now().UTC() }}
}

// SetClockForTest overrides the clock; tests only.
func (s *BuyInService) SetClockForTest(now func() time.Time) { s.now = now }

func (s *BuyInService) alert(ctx context.Context, code, summary string, detail map[string]any) {
	if s.onAlert != nil {
		s.onAlert(ctx, code, summary, detail)
	}
}

// NotifyDue issues buy-in notifications for CSDR fails at ISD+4 still
// OPEN. Idempotent — UNIQUE(fail_id,'NOTIFICATION').
func (s *BuyInService) NotifyDue(ctx context.Context, day time.Time) (int, error) {
	if s.store == nil {
		return 0, excerrors.New(CodeServiceDegraded, "buy-in store not configured")
	}
	isdBefore := day.Truncate(24*time.Hour).AddDate(0, 0, -BuyInNotifyDays)
	var n int
	err := s.store.InTx(ctx, func(ctx context.Context, tx BuyInTx) error {
		fails, err := tx.CSDRFailsDue(ctx, isdBefore, []FailStatus{FailOpen})
		if err != nil {
			return fmt.Errorf("list buy-in notify candidates: %w", err)
		}
		for _, f := range fails {
			ev, created, err := tx.InsertBuyIn(ctx, BuyInEvent{
				FailID: f.ID, Kind: "NOTIFICATION", Currency: f.Currency,
				Status: "ISSUED", CreatedAt: s.now(),
			})
			if err != nil {
				return excerrors.Wrap(CodeBuyInTriggered,
					fmt.Sprintf("notification fail %d", f.ID), err)
			}
			if !created {
				continue
			}
			if err := tx.SetFailStatus(ctx, f.ID, FailBuyInNotified, s.now()); err != nil {
				return fmt.Errorf("mark fail %d notified: %w", f.ID, err)
			}
			s.alert(ctx, "BUY_IN_NOTIFICATION", fmt.Sprintf(
				"buy-in notification issued — fail %d trade %d %s %s",
				f.ID, f.TradeID, f.Amount, f.Currency), map[string]any{
				"fail_id": f.ID, "trade_id": f.TradeID, "buyin_event": ev.ID,
			})
			n++
		}
		return nil
	})
	return n, err
}

// ExecuteDue executes buy-ins for CSDR fails at ISD+7 still unsettled:
// the position is acquired at the current mark and the price differential
// (|mark − original| × unsettled amount) is journaled as a claim on the
// failing counterparty. Idempotent — UNIQUE(fail_id,'EXECUTION').
func (s *BuyInService) ExecuteDue(ctx context.Context, day time.Time) (int, error) {
	if s.store == nil {
		return 0, excerrors.New(CodeServiceDegraded, "buy-in store not configured")
	}
	if s.pricer == nil {
		return 0, excerrors.New(CodeServiceDegraded, "market pricer not configured")
	}
	isdBefore := day.Truncate(24*time.Hour).AddDate(0, 0, -BuyInExecuteDays)
	var n int
	err := s.store.InTx(ctx, func(ctx context.Context, tx BuyInTx) error {
		fails, err := tx.CSDRFailsDue(ctx, isdBefore,
			[]FailStatus{FailOpen, FailBuyInNotified})
		if err != nil {
			return fmt.Errorf("list buy-in execute candidates: %w", err)
		}
		for _, f := range fails {
			mark, err := s.pricer.MarkPrice(ctx, f.TradeID)
			if err != nil {
				return excerrors.Wrap(CodeBuyInTriggered,
					fmt.Sprintf("mark price fail %d trade %d", f.ID, f.TradeID), err)
			}
			orig, found, err := tx.TradePrice(ctx, f.TradeID)
			if err != nil {
				return fmt.Errorf("original price trade %d: %w", f.TradeID, err)
			}
			if !found {
				return excerrors.New("NOT_FOUND",
					fmt.Sprintf("trade %d for fail %d not found", f.TradeID, f.ID))
			}
			diff := mark.Sub(orig).Abs().Mul(f.Amount)
			ev, created, err := tx.InsertBuyIn(ctx, BuyInEvent{
				FailID: f.ID, Kind: "EXECUTION",
				MarketPrice: mark, OriginalPrice: orig,
				PriceDifferential: diff, Currency: f.Currency,
				Status: "EXECUTED", CreatedAt: s.now(),
			})
			if err != nil {
				return excerrors.Wrap(CodeBuyInTriggered,
					fmt.Sprintf("execution fail %d", f.ID), err)
			}
			if !created {
				continue
			}
			// Price differential charged to the failing counterparty —
			// claim asset vs nostro funding of the buy-in.
			if diff.IsPositive() {
				if _, err := tx.PostJournal(ctx, "ADJUSTMENT",
					fmt.Sprintf("buy-in price differential — fail %d trade %d", f.ID, f.TradeID),
					"buyin-service", fmt.Sprintf("buyin-diff:%d", f.ID), f.TradeID,
					[]JournalLine{
						{AccountCode: "1020_SETTLEMENT_FAIL_CLAIM_" + f.Currency,
							Debit: diff, Currency: f.Currency,
							Narrative: "buy-in differential charged to failing party"},
						{AccountCode: "1010_NOSTRO_" + f.Currency,
							Credit: diff, Currency: f.Currency,
							Narrative: "buy-in executed at market"},
					}); err != nil {
					return excerrors.Wrap(CodeBuyInTriggered, "differential journal", err)
				}
			}
			if err := tx.SetFailStatus(ctx, f.ID, FailBoughtIn, s.now()); err != nil {
				return fmt.Errorf("mark fail %d bought-in: %w", f.ID, err)
			}
			s.alert(ctx, CodeBuyInTriggered, fmt.Sprintf(
				"buy-in executed — fail %d trade %d diff %s %s",
				f.ID, f.TradeID, diff, f.Currency), map[string]any{
				"fail_id": f.ID, "trade_id": f.TradeID,
				"buyin_event": ev.ID, "differential": diff.String(),
			})
			n++
		}
		return nil
	})
	return n, err
}

// ---------------------------------------------------------------------------
// pgx impl — rides the same store pool as PgxFailStore
// ---------------------------------------------------------------------------

// BuyInStoreOf wraps a PgxFailStore — same tables, same pool.
func BuyInStoreOf(s *PgxFailStore) BuyInStore { return pgxBuyInStore{q: s.Q} }

type pgxBuyInStore struct{ q Querier }

func (s pgxBuyInStore) InTx(ctx context.Context, fn func(ctx context.Context, tx BuyInTx) error) error {
	return RunInTx(ctx, s.q, func(q Querier) error {
		return fn(ctx, pgxBuyInTx{q: q})
	})
}

type pgxBuyInTx struct{ q Querier }

func (t pgxBuyInTx) CSDRFailsDue(ctx context.Context, isdBefore time.Time,
	states []FailStatus) ([]SettlementFail, error) {
	list := "("
	for i, st := range states {
		if i > 0 {
			list += ","
		}
		list += fmt.Sprintf("'%s'", st)
	}
	list += ")"
	rows, err := t.q.Query(ctx, `
		SELECT `+failCols+` FROM settlement_fails
		 WHERE regime='CSDR' AND isd <= $1 AND status IN `+list+`
		 ORDER BY id FOR UPDATE`, isdBefore)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SettlementFail
	for rows.Next() {
		f, err := scanFail(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

func (t pgxBuyInTx) InsertBuyIn(ctx context.Context, e BuyInEvent) (BuyInEvent, bool, error) {
	var mkt, orig, diff any
	if !e.MarketPrice.IsZero() {
		mkt = e.MarketPrice.String()
	}
	if !e.OriginalPrice.IsZero() {
		orig = e.OriginalPrice.String()
	}
	if !e.PriceDifferential.IsZero() {
		diff = e.PriceDifferential.String()
	}
	err := t.q.QueryRow(ctx, `
		INSERT INTO buy_in_events
		    (fail_id, kind, market_price, original_price, price_differential,
		     currency, status)
		VALUES ($1,$2::buyin_kind_enum,$3::numeric,$4::numeric,$5::numeric,$6,
		        $7::buyin_status_enum)
		ON CONFLICT (fail_id, kind) DO NOTHING
		RETURNING id, created_at`,
		e.FailID, e.Kind, mkt, orig, diff, e.Currency, e.Status).
		Scan(&e.ID, &e.CreatedAt)
	if stderrors.Is(err, pgx.ErrNoRows) {
		return e, false, nil
	}
	return e, err == nil, err
}

func (t pgxBuyInTx) SetFailStatus(ctx context.Context, id int64, st FailStatus, at time.Time) error {
	return pgxFailTx{q: t.q}.SetFailStatus(ctx, id, st, at)
}

func (t pgxBuyInTx) TradePrice(ctx context.Context, tradeID int64) (decimal.Decimal, bool, error) {
	var price string
	err := t.q.QueryRow(ctx,
		`SELECT price::text FROM trades WHERE id=$1`, tradeID).Scan(&price)
	if stderrors.Is(err, pgx.ErrNoRows) {
		return decimal.Zero, false, nil
	}
	if err != nil {
		return decimal.Zero, false, err
	}
	p, err := decimal.NewFromString(price)
	return p, true, err
}

func (t pgxBuyInTx) PostJournal(ctx context.Context, entryType, description,
	postedBy, idemKey string, referenceID int64, lines []JournalLine) (int64, error) {
	return pgxExceptionTx{q: t.q}.PostJournal(ctx, entryType, description,
		postedBy, idemKey, referenceID, lines)
}
