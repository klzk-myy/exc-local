// Task 5.3.30 — PG-backed manual-liquidation integration test.
//
// Gated: EXC_PG_TEST=1; DSN via EXC_PG_DSN / EXC_TEST_DSN (default:
// scratch w2d on the /tmp socket, port 55433). Uses the scratch account
// + instrument rows; asserts the atomic record (manual_liquidations row
// + admin_audit_log entry + audit_hash_chain link) and the emitted
// event, then cleans up the rows it created.
package api

import (
	"context"
	"encoding/json"
	"os"
	"strconv"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

func TestManualLiquidationIntegration(t *testing.T) {
	if os.Getenv("EXC_PG_TEST") != "1" {
		t.Skip("set EXC_PG_TEST=1 to run PostgreSQL integration tests")
	}
	dsn := os.Getenv("EXC_PG_DSN")
	if dsn == "" {
		dsn = os.Getenv("EXC_TEST_DSN")
	}
	if dsn == "" {
		dsn = "postgres://postgres@/w2d?host=/tmp&port=55433"
	}
	pool, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Fatalf("pg pool: %v", err)
	}
	defer pool.Close()
	ctx := context.Background()

	var acctID, instID int64
	if err := pool.QueryRow(ctx,
		`SELECT id FROM accounts ORDER BY id LIMIT 1`).Scan(&acctID); err != nil {
		t.Skipf("scratch DB has no account: %v", err)
	}
	if err := pool.QueryRow(ctx,
		`SELECT id FROM instruments ORDER BY id LIMIT 1`).Scan(&instID); err != nil {
		t.Skipf("scratch DB has no instrument: %v", err)
	}

	// Seed one open position for the account. The scratch DB is shared and
	// persistent — clear a stale row from a prior run so repeat runs stay
	// idempotent (the liquidation may mutate it either way).
	if _, err := pool.Exec(ctx,
		`DELETE FROM positions WHERE account_id=$1 AND instrument_id=$2 AND side='LONG'`,
		acctID, instID); err != nil {
		t.Fatalf("clear stale position: %v", err)
	}
	var posID int64
	err = pool.QueryRow(ctx, `
		INSERT INTO positions (account_id, instrument_id, side, quantity,
		                       entry_price, mark_price)
		VALUES ($1, $2, 'LONG', 100000, 1.10, 1.11) RETURNING id`,
		acctID, instID).Scan(&posID)
	if err != nil {
		t.Fatalf("seed position: %v", err)
	}

	sink := &fakeSink{}
	svc := NewManualLiquidationService(pool,
		func(_ context.Context, id int64) (string, error) {
			if id == 90001 || id == 90002 {
				return "Risk Manager", nil
			}
			return "Support Agent", nil
		}, sink)

	res, err := svc.Execute(ctx,
		AdminActor{UserID: 90001, ClientIP: "198.51.100.7"},
		ManualLiquidationRequest{
			AccountID:  json.RawMessage(strconv.FormatInt(acctID, 10)),
			Reason:     "integration test force-close",
			ApproverID: json.RawMessage("90002"),
		})
	// Cleanup regardless of outcome.
	defer func() {
		if res != nil {
			_, _ = pool.Exec(ctx,
				`DELETE FROM manual_liquidations WHERE id=$1`, res.LiquidationID)
		}
		_, _ = pool.Exec(ctx, `DELETE FROM positions WHERE id=$1`, posID)
	}()
	if err != nil {
		t.Fatalf("execute: %v", err)
	}

	if res.Status != "DISPATCHED" || res.AccountID != acctID ||
		len(res.EstimatedFills) == 0 {
		t.Fatalf("result: %+v", res)
	}
	// Durable record.
	var status, src string
	err = pool.QueryRow(ctx,
		`SELECT status, source FROM manual_liquidations WHERE id=$1`,
		res.LiquidationID).Scan(&status, &src)
	if err != nil || status != "DISPATCHED" || src != "MANUAL" {
		t.Fatalf("record row: %v status=%s source=%s", err, status, src)
	}
	// Admin audit entry in the same logical commit.
	var n int
	err = pool.QueryRow(ctx, `
		SELECT count(*) FROM admin_audit_log
		 WHERE action='liquidation.manual' AND target_id=$1
		   AND after_state->>'liquidation_id'=$2`,
		acctID, strconv.FormatInt(res.LiquidationID, 10)).Scan(&n)
	if err != nil || n != 1 {
		t.Fatalf("admin audit entry: %v n=%d", err, n)
	}
	// Hash-chain link (spec §5.8).
	err = pool.QueryRow(ctx, `
		SELECT count(*) FROM audit_hash_chain
		 WHERE table_name='manual_liquidations' AND record_id=$1`,
		res.LiquidationID).Scan(&n)
	if err != nil || n != 1 {
		t.Fatalf("audit chain link: %v n=%d", err, n)
	}
	// Event emitted to the sink with the position snapshot.
	if sink.ev.Event != "MANUAL_LIQUIDATION" ||
		sink.ev.LiquidationID != res.LiquidationID ||
		sink.ev.ApprovedBy != 90002 || len(sink.ev.Positions) == 0 {
		t.Fatalf("event: %+v", sink.ev)
	}
}
