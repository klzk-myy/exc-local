// PostgreSQL integration test — Phase-07 Task 7.3.7.
// Gated on EXC_PG_TEST=1; targets EXC_TEST_DSN (default: the dev
// database). Requires migration 048 applied; the test applies it
// idempotently when support_tickets is absent.
package support

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"exchange/internal/db"
)

func pgGate(t *testing.T) (*pgxpool.Pool, context.Context) {
	t.Helper()
	if os.Getenv("EXC_PG_TEST") != "1" {
		t.Skip("set EXC_PG_TEST=1 to run Postgres integration tests")
	}
	dsn := os.Getenv("EXC_TEST_DSN")
	if dsn == "" {
		dsn = "postgres://exchange:exchange_dev@127.0.0.1:5433/exchange?sslmode=disable"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	pool, err := db.NewPool(ctx, dsn, 4)
	if err != nil {
		cancel()
		t.Skipf("postgres unreachable: %v", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		cancel()
		t.Skipf("postgres unreachable: %v", err)
	}
	t.Cleanup(func() { pool.Close(); cancel() })

	// Ensure migration 048 (idempotent against partially-applied DBs).
	var hasTable bool
	if err := pool.QueryRow(ctx, `
		SELECT EXISTS (SELECT 1 FROM information_schema.tables
		 WHERE table_name = 'support_tickets')`).Scan(&hasTable); err != nil {
		t.Fatalf("table check: %v", err)
	}
	if !hasTable {
		up, err := os.ReadFile("../db/migrations/048_support_tickets.up.sql")
		if err != nil {
			t.Fatalf("read migration 048: %v", err)
		}
		if _, err := pool.Exec(ctx, string(up)); err != nil {
			t.Fatalf("apply migration 048: %v", err)
		}
	}
	return pool, ctx
}

// seedAccount creates a user+account pair for the FK chain.
func seedAccount(t *testing.T, ctx context.Context, pool *pgxpool.Pool) (userID, accountID int64) {
	t.Helper()
	email := fmt.Sprintf("t737-%d@example.invalid", time.Now().UnixNano())
	if err := pool.QueryRow(ctx,
		`INSERT INTO users (email) VALUES ($1) RETURNING id`, email).Scan(&userID); err != nil {
		t.Fatalf("seed user: %v", err)
	}
	if err := pool.QueryRow(ctx,
		`INSERT INTO accounts (user_id, account_type) VALUES ($1, 'SPOT') RETURNING id`,
		userID).Scan(&accountID); err != nil {
		t.Fatalf("seed account: %v", err)
	}
	return userID, accountID
}

type recAlerter struct {
	raised [][3]string
}

func (a *recAlerter) Raise(_ context.Context, sev, code, msg string) error {
	a.raised = append(a.raised, [3]string{sev, code, msg})
	return nil
}

func TestTicketsIntegration(t *testing.T) {
	pool, ctx := pgGate(t)
	_, acct := seedAccount(t, ctx, pool)
	_, other := seedAccount(t, ctx, pool)
	adminID := int64(900001)

	resolver := RoleResolver(func(context.Context, int64) (string, error) {
		return "Support Agent", nil
	})
	alerter := &recAlerter{}
	svc := NewService(pool, resolver, alerter)

	// --- create + own-list + foreign-account isolation ---
	tk, err := svc.Create(ctx, CreateRequest{
		AccountID: acct, Category: CategoryFunding,
		Subject: "wire deposit missing", Body: "sent 3 days ago",
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if tk.Status != StatusOpen || tk.Queue != QueueSupport || tk.Type != TypeSupport {
		t.Fatalf("new ticket state: %+v", tk)
	}
	if tk.SLADueAt.IsZero() {
		t.Fatal("sla_due_at must be set at create")
	}
	if tk.FinalResponseDueAt != nil {
		t.Fatal("support tickets carry no statutory final-response deadline")
	}

	mine, err := svc.ListMine(ctx, acct, nil, 10)
	if err != nil || len(mine) != 1 {
		t.Fatalf("ListMine: %v n=%d", err, len(mine))
	}
	got, _, err := svc.GetMine(ctx, acct, tk.ID)
	if err != nil || got.ID != tk.ID {
		t.Fatalf("GetMine: %v", err)
	}
	if _, _, err := svc.GetMine(ctx, other, tk.ID); err == nil ||
		!strings.Contains(err.Error(), "TICKET_NOT_FOUND") {
		t.Fatalf("foreign account read must 404-not-leak, got %v", err)
	}

	// --- validation ---
	if _, err := svc.Create(ctx, CreateRequest{AccountID: acct,
		Category: "BILLING", Subject: "x"}); err == nil {
		t.Fatal("invalid category must be rejected")
	}

	// --- admin queue confinement + update + note, all audited ---
	asCompliance := NewService(pool, RoleResolver(
		func(context.Context, int64) (string, error) {
			return "Compliance Officer", nil
		}), nil)
	if _, err := asCompliance.Update(ctx, adminID,
		AdminUpdate{TicketID: tk.ID, Status: StatusInProgress}, "10.0.0.1"); err == nil {
		t.Fatal("Compliance Officer must not mutate SUPPORT-queue tickets")
	}

	upd, err := svc.Update(ctx, adminID, AdminUpdate{
		TicketID: tk.ID, Status: StatusInProgress, AssignToMe: true,
		Note: "checking the rail", Priority: PriorityHigh,
	}, "10.0.0.2")
	if err != nil {
		t.Fatalf("admin update: %v", err)
	}
	if upd.Status != StatusInProgress || upd.AcknowledgedAt == nil ||
		upd.AssigneeAdminID == nil || *upd.AssigneeAdminID != adminID {
		t.Fatalf("post-update state: %+v", upd)
	}

	if _, err := svc.Update(ctx, adminID, AdminUpdate{
		TicketID: tk.ID, Status: StatusOpen}, "10.0.0.2"); err == nil {
		t.Fatal("IN_PROGRESS→OPEN must be rejected")
	}

	note, err := svc.AddNote(ctx, adminID, tk.ID, "rail says settled tomorrow", true, "10.0.0.2")
	if err != nil || note.NoteID == 0 {
		t.Fatalf("add note: %v", err)
	}
	// Internal notes never surface on the client read path.
	_, notes, err := svc.GetMine(ctx, acct, tk.ID)
	if err != nil {
		t.Fatalf("get mine: %v", err)
	}
	for _, n := range notes {
		if n.Internal {
			t.Fatal("internal note leaked to client view")
		}
	}

	// --- complaint: compliance queue + statutory clocks + ADR ---
	cmp, err := svc.Create(ctx, CreateRequest{
		AccountID: acct, Category: CategoryComplaint,
		Subject: "execution complaint", OriginChannel: "EMAIL",
	})
	if err != nil {
		t.Fatalf("create complaint: %v", err)
	}
	if cmp.Queue != QueueCompliance || cmp.Type != TypeComplaint {
		t.Fatalf("complaint routing: %+v", cmp)
	}
	if cmp.FinalResponseDueAt == nil {
		t.Fatal("complaint must carry the 8-week final-response deadline")
	}

	// Support Agent cannot see compliance-queue tickets.
	if _, _, err := svc.Get(ctx, adminID, cmp.ID); err == nil ||
		!strings.Contains(err.Error(), "UNAUTHORIZED_ROLE") {
		t.Fatalf("support agent must not read compliance tickets, got %v", err)
	}
	reg, err := asCompliance.ComplaintRegister(ctx, adminID, nil, 10)
	if err != nil || len(reg) == 0 {
		t.Fatalf("complaint register: %v", err)
	}
	if _, err := asCompliance.Update(ctx, adminID, AdminUpdate{
		TicketID: cmp.ID, Status: StatusInProgress,
		ADRRequested: ptr(true), ADRScheme: "FOS", ADRReference: "FOS-1",
	}, "10.0.0.3"); err != nil {
		t.Fatalf("complaint ADR update: %v", err)
	}
	// ADR fields on a support ticket are rejected.
	if _, err := svc.Update(ctx, adminID, AdminUpdate{
		TicketID: tk.ID, ADRScheme: "FOS"}, "10.0.0.2"); err == nil {
		t.Fatal("ADR fields must be rejected on SUPPORT-queue tickets")
	}

	// --- audit linkage: update + note wrote admin_audit_log rows ---
	var auditRows int
	if err := pool.QueryRow(ctx, `
		SELECT count(*) FROM admin_audit_log
		 WHERE target_type = 'support_ticket' AND target_id = $1`,
		tk.ID).Scan(&auditRows); err != nil {
		t.Fatalf("audit count: %v", err)
	}
	if auditRows < 2 { // update + note
		t.Fatalf("expected ≥2 audit rows for ticket %d, got %d", tk.ID, auditRows)
	}
	var chainRows int
	if err := pool.QueryRow(ctx, `
		SELECT count(*) FROM audit_hash_chain c
		 JOIN admin_audit_log l ON c.record_id = l.id
		 WHERE c.table_name = 'admin_audit_log'
		   AND l.target_type = 'support_ticket' AND l.target_id = $1`,
		tk.ID).Scan(&chainRows); err != nil {
		t.Fatalf("chain count: %v", err)
	}
	if chainRows < 2 {
		t.Fatalf("expected ≥2 chain links for ticket %d audit rows, got %d", tk.ID, chainRows)
	}

	// --- SLA sweep: fresh UNACKNOWLEDGED tickets (the updates above set
	// acknowledged_at, which retires the ack clock) ---
	stale, err := svc.Create(ctx, CreateRequest{
		AccountID: acct, Category: CategoryTechnical, Subject: "stale",
	})
	if err != nil {
		t.Fatalf("create stale: %v", err)
	}
	staleCmp, err := svc.Create(ctx, CreateRequest{
		AccountID: acct, Category: CategoryComplaint, Subject: "stale complaint",
	})
	if err != nil {
		t.Fatalf("create stale complaint: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`UPDATE support_tickets SET sla_due_at = now() - interval '1 hour'
		 WHERE id = ANY($1)`, []int64{stale.ID, staleCmp.ID}); err != nil {
		t.Fatalf("backdate sla: %v", err)
	}
	before := len(alerter.raised)
	n, err := svc.SweepAlerts(ctx, time.Now().UTC())
	if err != nil || n < 2 {
		t.Fatalf("sweep: n=%d err=%v", n, err)
	}
	sawP3, sawP2 := false, false
	for _, a := range alerter.raised[before:] {
		switch a[1] {
		case "TICKET_SLA_BREACH":
			if a[0] == "P3" {
				sawP3 = true
			}
		case "COMPLAINT_SLA_BREACH":
			if a[0] == "P2" {
				sawP2 = true
			}
		}
	}
	if !sawP3 || !sawP2 {
		t.Fatalf("expected P3 TICKET_SLA_BREACH + P2 COMPLAINT_SLA_BREACH, got %v",
			alerter.raised[before:])
	}
	// Second sweep must not re-alert — the once-only flag columns
	// (ack_breach_flagged_at) claimed the rows.
	before = len(alerter.raised)
	if _, err := svc.SweepAlerts(ctx, time.Now().UTC()); err != nil {
		t.Fatalf("sweep2: %v", err)
	}
	for _, a := range alerter.raised[before:] {
		if strings.Contains(a[2], fmt.Sprintf("ticket %d ", stale.ID)) ||
			strings.Contains(a[2], fmt.Sprintf("ticket %d ", staleCmp.ID)) {
			t.Fatalf("double-alert on already-flagged ticket: %v", a)
		}
	}
}

func ptr[T any](v T) *T { return &v }
