// Postgres-gated integration test for PgStore — same convention as
// internal/compliance (EXC_PG_TEST=1, dev DSN or EXC_PG_DSN). Seeds a
// real trade through users→accounts→instruments→orders→trades so
// ResolveTrade exercises its production join, then drives the full
// lifecycle: NEWT → submissions → idempotent replay → NACK → repair
// break → corrected resubmit (CORR chain) against real serializable txs.
package reporting

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

func itPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	if os.Getenv("EXC_PG_TEST") != "1" {
		t.Skip("set EXC_PG_TEST=1 to run postgres-gated tests")
	}
	dsn := os.Getenv("EXC_PG_DSN")
	if dsn == "" {
		dsn = "postgres://exchange:exchange_dev@localhost:5433/exchange?sslmode=disable"
	}
	pool, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Skipf("postgres unreachable: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		t.Skipf("postgres unreachable: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

type itSeed struct {
	userIDs      []int64
	accountIDs   []int64
	instrumentID int64
	orderIDs     []int64
	tradeID      int64
}

// itSeedTrade inserts a throwaway EUR/USD SPOT fill. trades is
// partitioned on created_at — now() lands in the default partition when
// no daily partition covers it.
func itSeedTrade(t *testing.T, pool *pgxpool.Pool) *itSeed {
	t.Helper()
	ctx := context.Background()
	s := &itSeed{}
	tag := fmt.Sprintf("reg_it_%d", time.Now().UnixNano())

	var buyerUser, sellerUser int64
	if err := pool.QueryRow(ctx,
		`INSERT INTO users (email, status)
		 VALUES ($1,'ACTIVE') RETURNING id`,
		tag+"_b@example.com").Scan(&buyerUser); err != nil {
		t.Fatalf("seed buyer user: %v", err)
	}
	if err := pool.QueryRow(ctx,
		`INSERT INTO users (email, status)
		 VALUES ($1,'ACTIVE') RETURNING id`,
		tag+"_s@example.com").Scan(&sellerUser); err != nil {
		t.Fatalf("seed seller user: %v", err)
	}
	s.userIDs = []int64{buyerUser, sellerUser}

	var buyerAcct, sellerAcct int64
	if err := pool.QueryRow(ctx,
		`INSERT INTO accounts (user_id, account_type, kyc_tier)
		 VALUES ($1,'MARGIN','T1') RETURNING id`, buyerUser).Scan(&buyerAcct); err != nil {
		t.Fatalf("seed buyer account: %v", err)
	}
	if err := pool.QueryRow(ctx,
		`INSERT INTO accounts (user_id, account_type, kyc_tier)
		 VALUES ($1,'MARGIN','T1') RETURNING id`, sellerUser).Scan(&sellerAcct); err != nil {
		t.Fatalf("seed seller account: %v", err)
	}
	s.accountIDs = []int64{buyerAcct, sellerAcct}

	if err := pool.QueryRow(ctx, `
		INSERT INTO instruments
		  (symbol, base_currency, quote_currency, instrument_type,
		   tick_size, lot_size, min_order_qty, max_order_qty,
		   settlement_cycle, max_leverage, contract_size,
		   decimal_places, pip_size, status)
		VALUES ($1,'EUR','USD','SPOT',0.0001,1000,1,10000000,
		        1,30,100000,4,0.0001,'ACTIVE')
		RETURNING id`, "ZZIT"+tag).Scan(&s.instrumentID); err != nil {
		t.Fatalf("seed instrument: %v", err)
	}

	var buyOrder, sellOrder int64
	if err := pool.QueryRow(ctx, `
		INSERT INTO orders
		  (account_id, instrument_id, side, order_type, quantity,
		   time_in_force, price)
		VALUES ($1,$2,'BUY','LIMIT',1000000,'GTC',1.0850)
		RETURNING id`, buyerAcct, s.instrumentID).Scan(&buyOrder); err != nil {
		t.Fatalf("seed buy order: %v", err)
	}
	if err := pool.QueryRow(ctx, `
		INSERT INTO orders
		  (account_id, instrument_id, side, order_type, quantity,
		   time_in_force, price)
		VALUES ($1,$2,'SELL','LIMIT',1000000,'GTC',1.0850)
		RETURNING id`, sellerAcct, s.instrumentID).Scan(&sellOrder); err != nil {
		t.Fatalf("seed sell order: %v", err)
	}
	s.orderIDs = []int64{buyOrder, sellOrder}

	if err := pool.QueryRow(ctx, `
		INSERT INTO trades
		  (instrument_id, buy_order_id, sell_order_id,
		   buyer_account_id, seller_account_id, price, quantity,
		   created_at)
		VALUES ($1,$2,$3,$4,$5,1.0850,1000000,now())
		RETURNING id`,
		s.instrumentID, buyOrder, sellOrder,
		buyerAcct, sellerAcct).Scan(&s.tradeID); err != nil {
		t.Fatalf("seed trade: %v", err)
	}

	t.Cleanup(func() {
		c, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		uti := UTIFor(testVenueLEI(t), "TRADE", s.tradeID)
		// reporting rows first (FK: acks → submissions → events)
		_, _ = pool.Exec(c, `DELETE FROM regulatory_report_acks WHERE
		  event_id IN (SELECT event_id FROM regulatory_report_events
		      WHERE trade_id=$1 OR uti=$2)`, s.tradeID, uti)
		_, _ = pool.Exec(c, `DELETE FROM regulatory_report_submissions WHERE
		  event_id IN (SELECT event_id FROM regulatory_report_events
		    WHERE trade_id=$1 OR uti=$2)`, s.tradeID, uti)
		_, _ = pool.Exec(c, `DELETE FROM regulatory_report_breaks WHERE uti=$1`, uti)
		_, _ = pool.Exec(c, `DELETE FROM regulatory_report_events WHERE
		  trade_id=$1 OR uti=$2`, s.tradeID, uti)
		for _, a := range s.accountIDs {
			_, _ = pool.Exec(c, `DELETE FROM party_identifiers WHERE account_id=$1`, a)
			_, _ = pool.Exec(c, `DELETE FROM admin_audit_log WHERE
			  target_type='account' AND target_id=$1`, a)
		}
		_, _ = pool.Exec(c, `DELETE FROM trades WHERE id=$1`, s.tradeID)
		for _, o := range s.orderIDs {
			_, _ = pool.Exec(c, `DELETE FROM orders WHERE id=$1`, o)
		}
		_, _ = pool.Exec(c, `DELETE FROM instruments WHERE id=$1`, s.instrumentID)
		for _, a := range s.accountIDs {
			_, _ = pool.Exec(c, `DELETE FROM accounts WHERE id=$1`, a)
		}
		for _, u := range s.userIDs {
			_, _ = pool.Exec(c, `DELETE FROM users WHERE id=$1`, u)
		}
	})
	return s
}

func TestPgStore_Lifecycle_EndToEnd(t *testing.T) {
	pool := itPool(t)
	seed := itSeedTrade(t, pool)
	ctx := context.Background()

	st, err := NewPgStore(pool)
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	svc, err := NewService(st, Config{
		VenueLEI: testVenueLEI(t), VenueMIC: "XEXC", USINamespace: "EXC",
		DualSided: true, RepairSLA: 2 * time.Hour, StaleAfter: 24 * time.Hour,
		DerivativeEnrich: enrichStub,
		Now:              func() time.Time { return testNow },
	})
	if err != nil {
		t.Fatalf("service: %v", err)
	}

	// Party LEI for the buyer — real UpsertParty path.
	buyerLEI := leiWithChecksum(t, "ITBUYER00000000001")
	if err := st.UpsertParty(ctx, PartyIdentifiers{
		AccountID: seed.accountIDs[0], LEI: buyerLEI,
	}, seed.userIDs[0], "127.0.0.1"); err != nil {
		t.Fatalf("upsert party: %v", err)
	}
	p, err := st.PartyFor(ctx, seed.accountIDs[0])
	if err != nil || p == nil || p.LEI != buyerLEI {
		t.Fatalf("party: %+v err=%v", p, err)
	}

	tc, err := st.ResolveTrade(ctx, seed.tradeID)
	if err != nil || tc == nil {
		t.Fatalf("resolve trade %d: tc=%v err=%v", seed.tradeID, tc, err)
	}
	if tc.InstrumentCode == "" || tc.InstrumentType != "SPOT" ||
		!strings.HasPrefix(tc.Price, "1.085") ||
		!strings.HasPrefix(tc.Quantity, "1000000") {
		t.Fatalf("bad trade context: %+v", tc)
	}

	evs, err := svc.RecordExecution(ctx, tc)
	if err != nil {
		t.Fatalf("record: %v", err)
	}
	if len(evs) != 1 || evs[0].Regime != RegimeMIFID2 {
		t.Fatalf("expected MIFID2-only fan-out, got %v", evs)
	}
	e := evs[0]
	if e.Status != EventValidated {
		t.Fatalf("status %s errors %s", e.Status, e.ValidationErrors)
	}
	if e.BuyerLEI != buyerLEI {
		t.Fatalf("buyer LEI %q, want %q", e.BuyerLEI, buyerLEI)
	}
	subs, err := st.SubmissionsForEvent(ctx, e.EventID)
	if err != nil || len(subs) != 2 {
		t.Fatalf("submissions: %d err=%v", len(subs), err)
	}

	// Idempotent replay — same NEWT row, no duplicate.
	evs2, err := svc.RecordExecution(ctx, tc)
	if err != nil || len(evs2) != 1 || evs2[0].EventID != e.EventID {
		t.Fatalf("replay: evs=%v err=%v", evs2, err)
	}
	if n, err := st.SubmissionsForEvent(ctx, e.EventID); err != nil || len(n) != 2 {
		t.Fatalf("replay duplicated artifacts: %v err=%v", n, err)
	}

	// NACK the ARM artifact → submission NACKED + repair break opens.
	var arm *Submission
	for i := range subs {
		if subs[i].Destination == DestinationARM {
			arm = &subs[i]
		}
	}
	if arm == nil {
		t.Fatal("no ARM artifact")
	}
	br, err := svc.IngestAck(ctx, Ack{
		ReportSubmissionID: arm.ReportSubmissionID, EventID: e.EventID,
		AckStatus: AckReject, AckCode: "REJ-201", AckText: "bad field",
		ReceivedAt: testNow,
	})
	if err != nil || br == nil {
		t.Fatalf("ingest nack: br=%v err=%v", br, err)
	}
	got, err := st.SubmissionByID(ctx, arm.ReportSubmissionID)
	if err != nil || got == nil || got.Status != SubNacked {
		t.Fatalf("submission after nack: %+v err=%v", got, err)
	}
	ev, err := st.EventByID(ctx, e.EventID)
	if err != nil || ev.Status != EventRejected {
		t.Fatalf("event after nack: %+v err=%v", ev, err)
	}
	breaks, err := st.OpenBreaks(ctx, RegimeMIFID2, 100)
	if err != nil {
		t.Fatalf("breaks: %v", err)
	}
	var foundBreak bool
	for _, b := range breaks {
		if b.BreakID == br.BreakID {
			foundBreak = true
		}
	}
	if !foundBreak {
		t.Fatal("repair break not visible in OpenBreaks")
	}

	// Corrected resubmit → CORR event (seq 2, supersedes NEWT) + fresh artifact.
	fresh, err := svc.Resubmit(ctx, arm.ReportSubmissionID,
		map[string]any{"price": "1.0851"}, 0)
	if err != nil {
		t.Fatalf("resubmit: %v", err)
	}
	corr, err := st.LatestEvent(ctx, e.UTI, RegimeMIFID2)
	if err != nil || corr == nil {
		t.Fatalf("latest event: %v err=%v", corr, err)
	}
	if corr.Action != ActionCorrect || corr.ReportSeq != 2 ||
		corr.SupersedesEventID == nil || *corr.SupersedesEventID != e.EventID {
		t.Fatalf("corr chain broken: %+v", corr)
	}
	if fresh.Destination != DestinationARM || fresh.Status != SubPending {
		t.Fatalf("fresh artifact: %+v", fresh)
	}
}
