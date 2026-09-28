// PostgreSQL integration tests — Phase-07 Tasks 7.3.9 (LP management),
// 7.3.13 (CEO daily pack) and 7.3.14 (board pack + dual-controlled
// release).
//
// Gated on EXC_PG_TEST=1; targets EXC_TEST_DSN (default: the dev
// database). Migrations 191 + 101 are applied when their tables are
// absent; baseline anchors (instruments, admin_audit_log,
// audit_hash_chain, trades/accounts/orders/balances/insurance_fund/
// support_tickets/audit_merkle_roots) are expected from earlier
// migrations — a section whose backing store is missing simply renders
// STALE/ABSENT, which the assertions accommodate.
package admin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/shopspring/decimal"
)

func hasTable(t *testing.T, ctx context.Context, pool *pgxpool.Pool, name string) bool {
	t.Helper()
	var exists bool
	if err := pool.QueryRow(ctx, `SELECT to_regclass($1) IS NOT NULL`, name).Scan(&exists); err != nil {
		t.Fatalf("table probe %s: %v", name, err)
	}
	return exists
}

func applyMigration(t *testing.T, ctx context.Context, pool *pgxpool.Pool, table, file string) {
	t.Helper()
	if hasTable(t, ctx, pool, table) {
		return
	}
	up, err := os.ReadFile("../db/migrations/" + file)
	if err != nil {
		t.Fatalf("read %s: %v", file, err)
	}
	if _, err := pool.Exec(ctx, string(up)); err != nil {
		t.Fatalf("apply %s: %v", file, err)
	}
}

// ensureLPSchema applies migration 191 (skipping when the dev DB already
// has it); baseline anchors must exist — the test skips cleanly when a
// bare scratch DB lacks them.
func ensureLPSchema(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	for _, anchor := range []string{"instruments", "admin_audit_log", "audit_hash_chain"} {
		if !hasTable(t, ctx, pool, anchor) {
			t.Skipf("baseline table %s missing — target a migrated database", anchor)
		}
	}
	applyMigration(t, ctx, pool, "liquidity_providers", "191_liquidity_providers.up.sql")
}

func ensurePackSchema(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	for _, anchor := range []string{"admin_audit_log", "audit_hash_chain"} {
		if !hasTable(t, ctx, pool, anchor) {
			t.Skipf("baseline table %s missing — target a migrated database", anchor)
		}
	}
	applyMigration(t, ctx, pool, "governance_packs", "101_governance_packs.up.sql")
}

func pickInstrument(t *testing.T, ctx context.Context, pool *pgxpool.Pool) int64 {
	t.Helper()
	var id int64
	if err := pool.QueryRow(ctx,
		`SELECT id FROM instruments ORDER BY id LIMIT 1`).Scan(&id); err != nil {
		t.Skipf("no instrument seeded: %v", err)
	}
	return id
}

// mapResolver resolves roles for a fixed admin-id map (integration tests
// bypass the binding store — Task 7.3.1 owns that).
func mapResolver(roles map[int64]string) AdminRoleResolver {
	return func(_ context.Context, id int64) (string, error) {
		r, ok := roles[id]
		if !ok {
			return "", errors.New("no role binding")
		}
		return r, nil
	}
}

// ---------------------------------------------------------------------------
// Task 7.3.9 — LP lifecycle + scorecard persistence + audit chain
// ---------------------------------------------------------------------------

func TestLPIntegration(t *testing.T) {
	pool, ctx := pgGate(t)
	ensureLPSchema(t, ctx, pool)
	instID := pickInstrument(t, ctx, pool)
	start := time.Now().UTC() // audit assertions scope to this run

	rm := int64(910101)
	svc := NewLPService(pool, mapResolver(map[int64]string{rm: "Risk Manager"}), nil, nil)
	actor := AdminActor{UserID: rm, ClientIP: "198.51.100.7"}

	name := fmt.Sprintf("int-lp-%d", time.Now().UnixNano())
	lp, err := svc.Create(ctx, actor, LPCreate{
		Name: name, ConnectionType: "FIX",
		SessionConfig:   []byte(`{"sender_comp_id":"EXCH"}`),
		Contact:         []byte(`{"desk":"ops@acme.example"}`),
		SettlementTerms: []byte(`{"cycle":"T+1"}`),
		Instruments: []LPInstrumentConfig{{
			InstrumentID: instID, Enabled: true,
			SpreadMarkupBidBps: "1.25", SpreadMarkupAskBps: "1.50",
			SkewBps: "-0.5",
		}},
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if lp.Status != LPStatusOnboarding || lp.LPID == 0 {
		t.Fatalf("created LP: %+v", lp)
	}
	if len(lp.Instruments) != 1 || lp.Instruments[0].StalenessTimeoutMS != DefaultLPStalenessTimeoutMS {
		t.Fatalf("instrument config round-trip: %+v", lp.Instruments)
	}
	// NUMERIC round-trips normalized ("1.25" → "1.2500") — compare as
	// decimals, not strings.
	if d, err := decimal.NewFromString(lp.Instruments[0].SpreadMarkupBidBps); err != nil ||
		!d.Equal(decimal.NewFromFloat(1.25)) {
		t.Fatalf("bps round-trip: %+v", lp.Instruments[0])
	}

	// Detail + filtered list.
	got, err := svc.Get(ctx, actor, lp.LPID)
	if err != nil || got.Name != name {
		t.Fatalf("get: %v %+v", err, got)
	}
	lst, err := svc.List(ctx, actor, "ONBOARDING")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	seen := false
	for _, l := range lst {
		if l.LPID == lp.LPID {
			seen = true
		}
	}
	if !seen {
		t.Fatal("new LP missing from ONBOARDING list")
	}

	// Guarded lifecycle: ONBOARDING→ACTIVE, then ACTIVE→ONBOARDING must
	// fail INVALID_LIFECYCLE_TRANSITION.
	act, onboarding := "ACTIVE", "ONBOARDING"
	upd, err := svc.Update(ctx, actor, LPUpdate{LPID: lp.LPID, Status: &act,
		Reason: "session certified"})
	if err != nil || upd.Status != LPStatusActive {
		t.Fatalf("activate: %v %+v", err, upd)
	}
	if _, err := svc.Update(ctx, actor, LPUpdate{LPID: lp.LPID, Status: &onboarding}); err == nil {
		t.Fatal("ACTIVE→ONBOARDING must be rejected")
	} else if codeOf(t, err) != "INVALID_LIFECYCLE_TRANSITION" {
		t.Fatalf("want INVALID_LIFECYCLE_TRANSITION, got %s", eCode(err))
	}
	// Suspend then resume — the documented cycle.
	susp := "SUSPENDED"
	if _, err := svc.Update(ctx, actor, LPUpdate{LPID: lp.LPID, Status: &susp}); err != nil {
		t.Fatalf("suspend: %v", err)
	}
	if _, err := svc.Update(ctx, actor, LPUpdate{LPID: lp.LPID, Status: &act,
		FIXSessionEnabled: ptrBool(true)}); err != nil {
		t.Fatalf("resume: %v", err)
	}

	// Unique-name conflict maps to INVALID_REQUEST.
	if _, err := svc.Create(ctx, actor, LPCreate{Name: name, ConnectionType: "REST"}); err == nil {
		t.Fatal("duplicate name must fail")
	}

	// Scorecard: no metrics source + no snapshot → NOT_FOUND; after a
	// snapshot lands the read path serves it marked stale.
	if _, _, err := svc.Scorecard(ctx, actor, lp.LPID, 0); err == nil {
		t.Fatal("empty scorecard must not fabricate")
	}
	snap := LPScorecard{
		Window: "1h", QuotesReceived: 500, Fills: 470, Rejections: 10,
		FillRatio: 0.94, RejectionRate: 0.02, AvgResponseTimeMS: 1.8,
		AvailabilityPct: 99.9, SpreadQualityBps: 0.3,
		ComputedAt: time.Now().UTC(), Source: "test",
	}
	if err := svc.store.saveScorecard(ctx, lp.LPID, snap); err != nil {
		t.Fatalf("persist scorecard: %v", err)
	}
	sc, fired, err := svc.Scorecard(ctx, actor, lp.LPID, 0)
	if err != nil {
		t.Fatalf("scorecard read: %v", err)
	}
	if !sc.Stale || sc.FillRatio != 0.94 || len(fired) != 0 {
		t.Fatalf("persisted scorecard must surface stale: %+v fired=%d", sc, len(fired))
	}

	// Coverage: one ACTIVE LP exists now → venue coverage ok, no alert.
	if ok, err := svc.EvaluateCoverage(ctx); err != nil || !ok {
		t.Fatalf("evaluate coverage: ok=%v err=%v", ok, err)
	}
	stored, inserted, err := svc.store.insertAlert(ctx, LPAlert{
		LPID: lp.LPID, Metric: "fill_ratio", Observed: 0.5,
		Threshold: 0.8, Window: "1h"})
	if err != nil || !inserted || stored.Status != "OPEN" {
		t.Fatalf("alert insert: %v inserted=%v", err, inserted)
	}
	if _, inserted, err := svc.store.insertAlert(ctx, LPAlert{
		LPID: lp.LPID, Metric: "fill_ratio", Observed: 0.4,
		Threshold: 0.8, Window: "1h"}); err != nil || inserted {
		t.Fatal("duplicate OPEN alert must dedup")
	}
	alerts, err := svc.Alerts(ctx, actor, lp.LPID, true)
	if err != nil || len(alerts) != 1 || alerts[0].Status != "OPEN" {
		t.Fatalf("alerts: %v %+v", err, alerts)
	}

	// Audit: create + both updates wrote admin_audit_log rows, each with
	// an audit_hash_chain link (spec §5.8/§5.9 — atomic in one tx).
	var auditRows int
	if err := pool.QueryRow(ctx, `
		SELECT count(*) FROM admin_audit_log
		 WHERE target_type = 'liquidity_provider' AND target_id = $1
		   AND created_at >= $2`,
		lp.LPID, start).Scan(&auditRows); err != nil {
		t.Fatalf("audit count: %v", err)
	}
	if auditRows < 3 { // create + activate + suspend (+ resume)
		t.Fatalf("expected ≥3 audit rows for lp %d, got %d", lp.LPID, auditRows)
	}
	var chainRows int
	if err := pool.QueryRow(ctx, `
		SELECT count(*) FROM audit_hash_chain c
		 JOIN admin_audit_log l ON c.record_id = l.id
		 WHERE c.table_name = 'admin_audit_log'
		   AND l.target_type = 'liquidity_provider' AND l.target_id = $1
		   AND l.created_at >= $2 AND c.created_at >= $2`,
		lp.LPID, start).Scan(&chainRows); err != nil {
		t.Fatalf("chain count: %v", err)
	}
	if chainRows < auditRows {
		t.Fatalf("audit rows without chain links: rows=%d chain=%d", auditRows, chainRows)
	}
}

func ptrBool(v bool) *bool { return &v }

// ---------------------------------------------------------------------------
// Tasks 7.3.13/7.3.14 — pack generation, hash chain, dual-controlled
// immutable release
// ---------------------------------------------------------------------------

func TestGovernancePackIntegration(t *testing.T) {
	pool, ctx := pgGate(t)
	ensureLPSchema(t, ctx, pool)   // lp_performance_alerts feeds the alerts section
	ensurePackSchema(t, ctx, pool) // governance_packs

	maker, checker, auditor := int64(910201), int64(910202), int64(910209)
	svc := NewGovernancePackService(pool, mapResolver(map[int64]string{
		maker: "Super Admin", checker: "Super Admin", auditor: "Read-Only Auditor",
	}), nil, nil)

	// --- CEO_DAILY: generates from live sources; missing stores → STALE.
	day := DailyPeriod(time.Now().UTC().AddDate(0, 0, -1))
	ceo, err := svc.Generate(ctx, AdminActor{UserID: maker, ClientIP: "198.51.100.8"},
		PackKindCEODaily, day)
	if err != nil {
		t.Fatalf("generate CEO pack: %v", err)
	}
	if ceo.Status != PackGenerated || !VerifyHash(ceo) {
		t.Fatalf("CEO pack state: %+v", ceo)
	}
	var content packContent
	if err := json.Unmarshal(ceo.Content, &content); err != nil {
		t.Fatalf("decode content: %v", err)
	}
	if len(content.Sections) != 10 {
		t.Fatalf("CEO pack must carry 10 sections, got %d", len(content.Sections))
	}
	byName := map[string]PackSection{}
	for _, s := range content.Sections {
		byName[s.Name] = s
	}
	// Sections backed by migrated tables must be OK; unprovisioned stores
	// mark STALE (CEO semantics — never ABSENT, never blocking).
	for _, sec := range content.Sections {
		switch sec.Status {
		case SectionOK, SectionStale:
		default:
			t.Fatalf("CEO section %s has board-only status %s", sec.Name, sec.Status)
		}
	}
	if s := byName["alerts"]; s.Status != SectionOK {
		t.Fatalf("alerts section (probe lp_performance_alerts) must be OK: %+v", s)
	}
	if s := byName["trade_volume"]; hasTable(t, ctx, pool, "trades") && s.Status != SectionOK {
		t.Fatalf("trade_volume should be OK when trades exists: %+v", s)
	}
	if s := byName["incidents"]; hasTable(t, ctx, pool, "incidents") == false && s.Status != SectionStale {
		t.Fatalf("incidents (unprovisioned) must be STALE: %+v", s)
	}
	var ver map[string]string
	if err := json.Unmarshal(ceo.SourceVersions, &ver); err != nil {
		t.Fatalf("source_versions: %v", err)
	}
	if v := ver["alerts"]; !strings.HasPrefix(v, "pg:lp_performance_alerts@") {
		t.Fatalf("alerts source_version: %v", ver)
	}
	// QUEUED delivery recorded (no sink wired).
	var deliv string
	if err := pool.QueryRow(ctx,
		`SELECT deliveries::text FROM governance_packs WHERE pack_id = $1`,
		ceo.PackID).Scan(&deliv); err != nil {
		t.Fatalf("read deliveries: %v", err)
	}
	if !strings.Contains(deliv, "queued") {
		t.Fatalf("expected queued delivery record, got %s", deliv)
	}

	// --- Hash chain: the next CEO pack links to this one.
	ceo2, err := svc.GenerateScheduled(ctx, PackKindCEODaily, day)
	if err != nil {
		t.Fatalf("scheduled regenerate: %v", err)
	}
	if ceo2.PrevPackHash != ceo.ContentHash {
		t.Fatalf("chain link: prev=%q want %q", ceo2.PrevPackHash, ceo.ContentHash)
	}
	if ceo2.GeneratedBy != "system" {
		t.Fatalf("scheduled pack generated_by: %q", ceo2.GeneratedBy)
	}

	// --- BOARD pack: 8 sections, ABSENT marking, dual-controlled
	//     release, immutable afterwards. BOARD_ADHOC with a unique label
	//     so the (kind,period) RELEASED uniqueness survives re-runs.
	q, err := AdhocPeriod(fmt.Sprintf("it-%d", time.Now().UnixNano()))
	if err != nil {
		t.Fatalf("period: %v", err)
	}
	board, err := svc.GenerateBoard(ctx, AdminActor{UserID: maker}, PackKindBoardAdhoc, q)
	if err != nil {
		t.Fatalf("generate board pack: %v", err)
	}
	var bContent packContent
	if err := json.Unmarshal(board.Content, &bContent); err != nil {
		t.Fatalf("decode board content: %v", err)
	}
	if len(bContent.Sections) != 8 {
		t.Fatalf("board pack must carry 8 sections, got %d", len(bContent.Sections))
	}
	for _, s := range bContent.Sections {
		if s.Status == SectionAbsent && (s.Owner == "" || s.DueDate == "") {
			t.Fatalf("ABSENT section %s missing owner/due: %+v", s.Name, s)
		}
	}

	// Auditor cannot read the draft.
	if _, err := svc.Get(ctx, AdminActor{UserID: auditor}, board.PackID); err == nil {
		t.Fatal("draft board pack must not be auditor-readable")
	}

	// Dual-control gate: same-id approver rejected.
	if _, err := svc.Release(ctx, AdminActor{UserID: maker, ApproverID: maker},
		board.PackID, "q1"); err == nil {
		t.Fatal("self-approval must fail")
	} else if codeOf(t, err) != "DUAL_CONTROL_REQUIRED" {
		t.Fatalf("want DUAL_CONTROL_REQUIRED, got %s", eCode(err))
	}

	releaseStart := time.Now().UTC()
	rel, err := svc.Release(ctx, AdminActor{UserID: maker, ApproverID: checker,
		ClientIP: "198.51.100.8"}, board.PackID, "board sign-off")
	if err != nil {
		t.Fatalf("release: %v", err)
	}
	if rel.Status != PackReleased || rel.ReleasedBy == nil || *rel.ReleasedBy != checker {
		t.Fatalf("released state: %+v", rel)
	}
	if rel.ReleaseInitiatedBy == nil || *rel.ReleaseInitiatedBy != maker {
		t.Fatalf("maker recorded: %+v", rel)
	}

	// Auditor can read it now.
	if _, err := svc.Get(ctx, AdminActor{UserID: auditor}, board.PackID); err != nil {
		t.Fatalf("released pack must be auditor-readable: %v", err)
	}

	// Immutability: direct UPDATE on the RELEASED row must hit the trigger.
	if _, err := pool.Exec(ctx,
		`UPDATE governance_packs SET period = period WHERE pack_id = $1`,
		board.PackID); err == nil {
		t.Fatal("RELEASED row must reject UPDATE (immutable trigger)")
	}
	if _, err := pool.Exec(ctx,
		`DELETE FROM governance_packs WHERE pack_id = $1`,
		board.PackID); err == nil {
		t.Fatal("RELEASED row must reject DELETE (immutable trigger)")
	}
	// Re-release through the service fails at the GENERATED gate.
	if _, err := svc.Release(ctx, AdminActor{UserID: checker, ApproverID: maker},
		board.PackID, "again"); err == nil {
		t.Fatal("re-release must fail")
	}

	// One RELEASED pack per (kind, period): a second GENERATED pack for
	// the same period can be created but releasing it violates the
	// partial unique index.
	board2, err := svc.GenerateBoard(ctx, AdminActor{UserID: maker},
		PackKindBoardAdhoc, q)
	if err != nil {
		t.Fatalf("regenerate same period: %v", err)
	}
	if _, err := svc.Release(ctx, AdminActor{UserID: maker, ApproverID: checker},
		board2.PackID, "supersede"); err == nil {
		t.Fatal("second RELEASED pack for the same (kind,period) must fail")
	}

	// Release wrote an audit row + hash-chain link in the same tx.
	// (pack_id recycles across test DB resets — scope by time/hash.)
	var n int
	if err := pool.QueryRow(ctx, `
		SELECT count(*) FROM admin_audit_log
		 WHERE action = 'governance_pack.release' AND target_id = $1
		   AND created_at >= $2
		   AND after_state->>'content_hash' = $3`,
		board.PackID, releaseStart, board.ContentHash).Scan(&n); err != nil || n != 1 {
		t.Fatalf("release audit row: n=%d err=%v", n, err)
	}
	if err := pool.QueryRow(ctx, `
		SELECT count(*) FROM audit_hash_chain
		 WHERE table_name = 'governance_packs' AND record_id = $1
		   AND action = 'UPDATE' AND created_at >= $2`,
		board.PackID, releaseStart).Scan(&n); err != nil || n != 1 {
		t.Fatalf("release chain link: n=%d err=%v", n, err)
	}

	// CEO_DAILY release is a no-op domain-wise — rejected.
	if _, err := svc.Release(ctx, AdminActor{UserID: maker, ApproverID: checker},
		ceo.PackID, ""); err == nil {
		t.Fatal("CEO_DAILY release must be rejected")
	}
}
