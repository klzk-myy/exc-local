// secretdrill — Phase-09 Task 9.3.29 emergency secret-rotation drill.
//
// Exercises the leak-triggered rotation lifecycle end-to-end against the
// live secrets_inventory register (migration 089): a drill secret is
// registered with a backdated last_rotated_at so it is already past its
// rotation SLA, the evaluator pages SECRET_ROTATION_OVERDUE (the same
// set the admin read handler maps to HTTP 503), the runbook leg
// (deploy/scripts/rotate-secrets.sh) is invoked by the wrapper script,
// then MarkRotated{Emergency: true, IncidentRef: DRILL-…} records the
// completed rotation, the evaluator clears on its next pass, and the
// admin_audit_log rows for both mutations are verified. The drill row
// is removed afterwards; the audit trail persists.
//
// The Vault-side revoke/reissue step is environment-bound (no Vault in
// dev); rotate-secrets.sh owns it and runs dry-run without a Vault CLI.
//
// Env: DATABASE_URL (default postgres://exchange:exchange_dev@127.0.0.1:5433/exchange?sslmode=disable)
// Exit: 0 pass · 1 a check failed · 2 setup error.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"exchange/internal/security"
)

type captureAlerter struct {
	raised []string
}

func (a *captureAlerter) Raise(_ context.Context, severity, code, _ string) error {
	a.raised = append(a.raised, severity+":"+code)
	return nil
}

type report struct {
	Secret        string   `json:"secret"`
	OverdueSeen   bool     `json:"overdue_detected"`
	AlertRaised   bool     `json:"secret_rotation_overdue_raised"`
	AlertPayload  []string `json:"alerts"`
	ListedOverdue bool     `json:"listed_in_overdue_view"`
	MarkedClean   bool     `json:"emergency_mark_cleared"`
	EvalCleared   bool     `json:"evaluator_cleared"`
	AuditRows     int      `json:"audit_rows"`
	VaultLegNote  string   `json:"vault_leg"`
	Pass          bool     `json:"pass"`
	Fail          []string `json:"failures,omitempty"`
}

func fail(rep *report, f string, args ...any) {
	rep.Fail = append(rep.Fail, fmt.Sprintf(f, args...))
}

func main() {
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		dsn = "postgres://exchange:exchange_dev@127.0.0.1:5433/exchange?sslmode=disable"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		fmt.Fprintln(os.Stderr, "pg connect:", err)
		os.Exit(2)
	}
	defer pool.Close()
	if err := pool.Ping(ctx); err != nil {
		fmt.Fprintln(os.Stderr, "pg ping:", err)
		os.Exit(2)
	}

	name := fmt.Sprintf("drill-rot-%d", time.Now().Unix())
	rep := &report{Secret: name,
		VaultLegNote: "env-bound: no Vault in dev — rotate-secrets.sh dry-run leg in wrapper"}

	store := security.NewPgInventoryStore(pool)
	alerter := &captureAlerter{}
	resolver := security.RoleResolver(
		func(context.Context, int64) (string, error) { return "Super Admin", nil })
	svc := security.NewInventoryService(store, resolver, alerter, nil)
	const adminID int64 = 1

	// Step 1 — register a secret already past its rotation SLA.
	stale := time.Now().Add(-720 * time.Hour) // 30d — past every class ceiling
	if _, err := svc.UpsertEntry(ctx, adminID, security.InventoryEntryInput{
		SecretName:        name,
		Class:             security.ClassJWTSigningKey,
		Category:          "drill",
		Owner:             "sre-drill",
		Consumers:         []string{"drill"},
		TTL:               24 * time.Hour,
		AlertLead:         time.Hour,
		RotationProcedure: "deploy/scripts/rotate-secrets.sh rotate jwt --apply",
		LastRotatedAt:     &stale,
	}, "127.0.0.1"); err != nil {
		fmt.Fprintln(os.Stderr, "upsert:", err)
		os.Exit(2)
	}
	defer func() {
		_, _ = pool.Exec(context.Background(),
			"DELETE FROM secrets_inventory WHERE secret_name = $1", name)
	}()

	// Step 2 — evaluator pass: drill row must page SECRET_ROTATION_OVERDUE.
	states, err := svc.EvaluateRotation(ctx)
	if err != nil {
		fmt.Fprintln(os.Stderr, "evaluate:", err)
		os.Exit(2)
	}
	for _, st := range states {
		if st.Name == name && st.State == security.StateOverdue {
			rep.OverdueSeen = true
		}
	}
	rep.AlertPayload = alerter.raised
	for _, a := range alerter.raised {
		if a == "P2:"+security.CodeSecretRotationOverdue {
			rep.AlertRaised = true
		}
	}
	if !rep.OverdueSeen {
		fail(rep, "drill secret not assessed overdue by evaluator")
	}
	if !rep.AlertRaised {
		fail(rep, "evaluator did not page %s", security.CodeSecretRotationOverdue)
	}

	// Step 3 — the admin read view lists it overdue (the handler maps this
	// set to 503 SECRET_ROTATION_OVERDUE; handler-level mapping is covered
	// by handlers_secrets_inventory_test.go).
	view, err := svc.List(ctx, adminID)
	if err != nil {
		fmt.Fprintln(os.Stderr, "list:", err)
		os.Exit(2)
	}
	for _, o := range view.Overdue {
		if o == name {
			rep.ListedOverdue = true
		}
	}
	if !rep.ListedOverdue {
		fail(rep, "drill secret absent from overdue view")
	}

	// Step 4 — leak response: record the completed emergency rotation.
	incident := fmt.Sprintf("DRILL-%d", time.Now().Unix())
	if _, err := svc.MarkRotated(ctx, adminID, name, security.RotationMark{
		Emergency:   true,
		IncidentRef: incident,
	}, "127.0.0.1"); err != nil {
		fmt.Fprintln(os.Stderr, "mark-rotated:", err)
		os.Exit(2)
	}
	e, found, err := store.Get(ctx, name)
	if err != nil || !found {
		fmt.Fprintln(os.Stderr, "get:", err)
		os.Exit(2)
	}
	if e.LastRotatedAt != nil && time.Since(*e.LastRotatedAt) < 5*time.Minute {
		rep.MarkedClean = true
	}
	if !rep.MarkedClean {
		fail(rep, "last_rotated_at not refreshed by emergency mark")
	}

	// Step 5 — next evaluator pass clears the latch (no re-page).
	alerter.raised = nil
	states, err = svc.EvaluateRotation(ctx)
	if err != nil {
		fmt.Fprintln(os.Stderr, "re-evaluate:", err)
		os.Exit(2)
	}
	for _, st := range states {
		if st.Name == name && st.State == security.StateOK {
			rep.EvalCleared = true
		}
	}
	for _, a := range alerter.raised {
		if a == "P2:"+security.CodeSecretRotationOverdue {
			fail(rep, "evaluator re-paged after rotation cleared")
		}
	}
	if !rep.EvalCleared {
		fail(rep, "evaluator did not clear drill secret after mark")
	}

	// Step 6 — audit trail: upsert + mark-rotated rows for the drill.
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM admin_audit_log
		  WHERE target_type = 'secrets_inventory'
		    AND action IN ('secrets_inventory.upsert','secrets_inventory.mark_rotated')
		    AND after_state->>'secret_name' = $1`, name,
	).Scan(&rep.AuditRows); err != nil {
		fail(rep, "audit query: %v", err)
	} else if rep.AuditRows < 2 {
		fail(rep, "audit rows = %d, want >= 2 (upsert + mark)", rep.AuditRows)
	}

	rep.Pass = len(rep.Fail) == 0
	out, _ := json.MarshalIndent(rep, "", "  ")
	fmt.Println(string(out))
	if !rep.Pass {
		os.Exit(1)
	}
}
