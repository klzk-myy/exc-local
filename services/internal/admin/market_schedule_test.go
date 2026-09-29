// market_schedule_test.go — Phase-15 Task 15.3.4 coverage.
//
// Unit tests exercise the OverrideInput validator (pure, no stores).
// The integration test is gated on EXC_PG_TEST=1 AND EXC_REDIS_TEST=1:
// it applies migration 221, drives CRUD against PostgreSQL and asserts
// the market:hours Redis projection is republished on every mutation
// and unconditionally rewritten by Reconcile (the boot contract).
package admin

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// Unit: validation
// ---------------------------------------------------------------------------

func testScheduleSvc(now time.Time) *MarketScheduleService {
	// Store-less service — validate() only reads the clock.
	return &MarketScheduleService{now: func() time.Time { return now }}
}

func TestScheduleOverrideValidation(t *testing.T) {
	now := time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC) // Monday
	svc := testScheduleSvc(now)
	future := now.AddDate(0, 0, 7).Format("2006-01-02")

	cases := []struct {
		name string
		in   OverrideInput
		ok   bool
	}{
		{"closed no reason", OverrideInput{Date: future, Closed: true}, false},
		{"bad date", OverrideInput{Date: "10/12/2026", Closed: true, Reason: "x"}, false},
		{"past date", OverrideInput{Date: "2026-10-01", Closed: true, Reason: "x"}, false},
		{"closed with times", OverrideInput{Date: future, Closed: true, Open: "10:00", Close: "12:00", Reason: "x"}, false},
		{"valid closed", OverrideInput{Date: future, Closed: true, Reason: "Christmas"}, true},
		{"partial missing close", OverrideInput{Date: future, Open: "21:00", Reason: "x"}, false},
		{"partial bad time", OverrideInput{Date: future, Open: "9:00", Close: "17:00", Reason: "x"}, false},
		{"partial inverted", OverrideInput{Date: future, Open: "18:00", Close: "12:00", Reason: "x"}, false},
		{"partial equal", OverrideInput{Date: future, Open: "12:00", Close: "12:00", Reason: "x"}, false},
		{"partial hour edge", OverrideInput{Date: future, Open: "24:00", Close: "25:00", Reason: "x"}, false},
		{"valid partial", OverrideInput{Date: future, Open: "13:30", Close: "16:00", Reason: "early close"}, true},
		{"today ok", OverrideInput{Date: now.Format("2006-01-02"), Closed: true, Reason: "x"}, true},
	}
	for _, tc := range cases {
		err := svc.validate(tc.in)
		if tc.ok && err != nil {
			t.Errorf("%s: want ok, got %v", tc.name, err)
		}
		if !tc.ok && err == nil {
			t.Errorf("%s: want rejection, got nil", tc.name)
		}
		if !tc.ok && err != nil && excerrors_code(err) != "INVALID_REQUEST" {
			t.Errorf("%s: want INVALID_REQUEST, got %s", tc.name, excerrors_code(err))
		}
	}
}

// ---------------------------------------------------------------------------
// Integration: CRUD → republish; Reconcile → boot rewrite; audit rows.
// ---------------------------------------------------------------------------

func TestMarketScheduleIntegration(t *testing.T) {
	pool, ctx := pgGate(t)
	rdb, _ := redisGate(t)
	for _, anchor := range []string{"admin_audit_log", "audit_hash_chain"} {
		if !hasTable(t, ctx, pool, anchor) {
			t.Skipf("baseline table %s missing — target a migrated database", anchor)
		}
	}
	applyMigration(t, ctx, pool, "market_schedule_overrides", "221_market_schedule_overrides.up.sql")

	rm, auditor := int64(915301), int64(915309)
	svc, err := NewMarketScheduleService(pool, rdb,
		mapResolver(map[int64]string{rm: "Risk Manager", auditor: "Read-Only Auditor"}))
	if err != nil {
		t.Fatalf("service: %v", err)
	}
	start := time.Now().UTC()

	readDoc := func() marketHoursDoc {
		raw, err := rdb.Get(ctx, MarketHoursKey).Result()
		if err != nil {
			t.Fatalf("GET %s: %v", MarketHoursKey, err)
		}
		var doc marketHoursDoc
		if err := json.Unmarshal([]byte(raw), &doc); err != nil {
			t.Fatalf("decode market:hours: %v", err)
		}
		return doc
	}

	// Boot contract: garbage in the key is unconditionally overwritten.
	if err := rdb.Set(ctx, MarketHoursKey, `{"stale":true}`, 0).Err(); err != nil {
		t.Fatalf("seed stale key: %v", err)
	}
	if err := svc.Reconcile(ctx); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	doc := readDoc()
	if doc.OpenUTC != marketOpenUTC || doc.CloseUTC != marketCloseUTC ||
		doc.PreOpenUTC != marketPreOpenUTC {
		t.Fatalf("canonical window drifted: %+v", doc)
	}
	if len(doc.Overrides) != 0 {
		t.Fatalf("expected clean override set, got %+v", doc.Overrides)
	}

	// Read surface: any venue admin reads; auditor reads too.
	if _, err := svc.Current(ctx, AdminActor{UserID: auditor}); err != nil {
		t.Fatalf("auditor read: %v", err)
	}
	// Mutation role gate: auditor must not mutate.
	date := time.Now().UTC().AddDate(0, 0, 10).Format("2006-01-02")
	if _, err := svc.CreateOverride(ctx, AdminActor{UserID: auditor},
		OverrideInput{Date: date, Closed: true, Reason: "x"}); excerrors_code(err) != "UNAUTHORIZED_ROLE" {
		t.Fatalf("auditor create must fail UNAUTHORIZED_ROLE, got %v", err)
	}

	// Create → republish carries the override.
	ov, err := svc.CreateOverride(ctx, AdminActor{UserID: rm, ClientIP: "198.51.100.20"},
		OverrideInput{Date: date, Closed: true, Reason: "integration holiday"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(),
			`DELETE FROM market_schedule_overrides WHERE override_id=$1`, ov.ID)
	})
	doc = readDoc()
	if len(doc.Overrides) != 1 || doc.Overrides[0].Date != date || !doc.Overrides[0].Closed {
		t.Fatalf("projection missing override: %+v", doc.Overrides)
	}

	// Duplicate date → INVALID_REQUEST (unique constraint mapped).
	if _, err := svc.CreateOverride(ctx, AdminActor{UserID: rm},
		OverrideInput{Date: date, Closed: true, Reason: "dup"}); excerrors_code(err) != "INVALID_REQUEST" {
		t.Fatalf("duplicate date must fail INVALID_REQUEST, got %v", err)
	}

	// Update → partial session; projection reflects the new shape.
	upd, err := svc.UpdateOverride(ctx, AdminActor{UserID: rm}, ov.ID,
		OverrideInput{Closed: false, Open: "13:00", Close: "17:00", Reason: "shortened day"})
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if upd.Closed || upd.Open != "13:00" || upd.Close != "17:00" {
		t.Fatalf("update shape: %+v", upd)
	}
	doc = readDoc()
	if len(doc.Overrides) != 1 || doc.Overrides[0].Closed ||
		doc.Overrides[0].Open != "13:00" {
		t.Fatalf("projection not republished after update: %+v", doc.Overrides)
	}

	// Version is monotonic across publishes.
	v1 := doc.Version
	if err := svc.Reconcile(ctx); err != nil {
		t.Fatalf("second reconcile: %v", err)
	}
	if readDoc().Version <= v1 {
		t.Fatal("version must increase on republish")
	}

	// Delete → projection drops the row; NOT_FOUND on second delete.
	if err := svc.DeleteOverride(ctx, AdminActor{UserID: rm}, ov.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if doc := readDoc(); len(doc.Overrides) != 0 {
		t.Fatalf("deleted override still in projection: %+v", doc.Overrides)
	}
	if err := svc.DeleteOverride(ctx, AdminActor{UserID: rm}, ov.ID); excerrors_code(err) != "NOT_FOUND" {
		t.Fatalf("second delete must be NOT_FOUND, got %v", err)
	}

	// Auditability: create + update + delete each logged with hash-chain
	// links (the Log helper writes both atomically).
	var n int
	if err := pool.QueryRow(ctx, `
		SELECT count(*) FROM admin_audit_log
		 WHERE target_type='market_schedule_override' AND target_id=$1
		   AND created_at >= $2`, ov.ID, start).Scan(&n); err != nil || n != 3 {
		t.Fatalf("audit rows=%d err=%v (want 3)", n, err)
	}
	actions := []string{
		"market_schedule.override.create",
		"market_schedule.override.update",
		"market_schedule.override.delete",
	}
	for _, a := range actions {
		var found int
		if err := pool.QueryRow(ctx, `
			SELECT count(*) FROM admin_audit_log
			 WHERE target_id=$1 AND action=$2 AND created_at >= $3`,
			ov.ID, a, start).Scan(&found); err != nil || found != 1 {
			t.Fatalf("audit action %s found=%d err=%v", a, found, err)
		}
	}
	var chain int
	if err := pool.QueryRow(ctx, `
		SELECT count(*) FROM audit_hash_chain c
		 JOIN admin_audit_log l ON c.record_id = l.id
		 WHERE c.table_name='admin_audit_log'
		   AND l.target_type='market_schedule_override' AND l.target_id=$1
		   AND l.created_at >= $2 AND c.created_at >= $2`,
		ov.ID, start).Scan(&chain); err != nil || chain < 3 {
		t.Fatalf("hash-chain links=%d err=%v", chain, err)
	}

	// List view sees the (deleted) row's history is gone; Expired rows
	// stay in List but never re-enter the projection.
	past := "2020-01-01"
	if _, err := pool.Exec(ctx, `
		INSERT INTO market_schedule_overrides
		  (override_date, closed, reason, created_by)
		VALUES ($1, true, 'historical', $2)
		ON CONFLICT (override_date) DO NOTHING`, past, rm); err != nil {
		t.Fatalf("seed past override: %v", err)
	}
	defer func() {
		_, _ = pool.Exec(context.Background(),
			`DELETE FROM market_schedule_overrides WHERE override_date=$1`, past)
	}()
	list, err := svc.ListOverrides(ctx, AdminActor{UserID: rm})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	sawPast := false
	for _, o := range list {
		if o.Date == past {
			sawPast = true
		}
	}
	if !sawPast {
		t.Fatal("admin list must include expired overrides (audit-visible)")
	}
	if err := svc.Reconcile(ctx); err != nil {
		t.Fatalf("reconcile after past-row insert: %v", err)
	}
	for _, o := range readDoc().Overrides {
		if strings.Compare(o.Date, time.Now().UTC().Format("2006-01-02")) < 0 {
			t.Fatalf("expired override leaked into projection: %+v", o)
		}
	}
}
