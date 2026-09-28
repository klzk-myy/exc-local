// releases.go — release registry + promotion gates (spec §19.16.3,
// migration 091 `releases` / `release_promotions` / `deploy_windows`).
//
// Direction is enforced before anything else: promotion flows
// dev → staging → production only. Registering a release lands it in dev
// with status DEPLOYED (dev auto-deploys); staging promotion needs a
// distinct eligible approver; production promotion needs that plus the
// §19.16.3 interlocks (open deploy window, no open P0/P1 incident,
// healthy DR standby) plus fresh gate evidence (PASS soak ≤14d, zero
// pending checkpoints). Every attempt — pass or block — writes a
// release_promotions row with the evaluated gate snapshot.
//
// Fail-closed: an interlock whose source cannot be consulted fails the
// gate ("unavailable", passed=false); only non-production targets skip
// the infra probes.
package fleet

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"exchange/internal/admin"
	"exchange/internal/gateway"

	excerrors "exchange/pkg/errors"
)

// Release lifecycle statuses.
const (
	RelRegistered = "REGISTERED" // recorded, not yet auto-deployed
	RelDeployed   = "DEPLOYED"   // live in its env stage
	RelRejected   = "REJECTED"
	RelSuperseded = "SUPERSEDED"
)

// Promotion statuses.
const (
	PromoExecuted = "EXECUTED"
	PromoBlocked  = "BLOCKED"
)

// SoakMaxAge is the freshness bound on the gate-evidence soak report —
// a promotion to production rides on recent proof, not history.
const SoakMaxAge = 14 * 24 * time.Hour

// nextEnv is the promotion direction lattice (§19.16.1). dev auto-deploys
// — it is a source stage, never a promote target; production is a sink.
var nextEnv = map[string]string{
	EnvDev:     EnvStaging,
	EnvStaging: EnvProduction,
}

// Release is one releases row.
type Release struct {
	ID           int64           `json:"id"`
	Component    string          `json:"component"`
	Version      string          `json:"version"`
	ArtifactHash string          `json:"artifact_hash"`
	Env          string          `json:"env"`
	Status       string          `json:"status"`
	GateEvidence json.RawMessage `json:"gate_evidence"`
	Notes        string          `json:"notes,omitempty"`
	CreatedBy    int64           `json:"created_by"`
	CreatedAt    time.Time       `json:"created_at"`
	UpdatedAt    time.Time       `json:"updated_at"`
}

const releaseCols = `id, component, version, artifact_hash, env, status,
	gate_evidence, notes, created_by, created_at, updated_at`

// Promotion is one release_promotions row.
type Promotion struct {
	ID          int64           `json:"id"`
	ReleaseID   int64           `json:"release_id"`
	FromEnv     string          `json:"from_env"`
	ToEnv       string          `json:"to_env"`
	Status      string          `json:"status"`
	Gates       json.RawMessage `json:"gates"`
	RequestedBy int64           `json:"requested_by"`
	ApprovedBy  *int64          `json:"approved_by,omitempty"`
	Reason      string          `json:"reason,omitempty"`
	CreatedAt   time.Time       `json:"created_at"`
	DecidedAt   *time.Time      `json:"decided_at,omitempty"`
}

// GateResult is one evaluated gate in the promotion-evidence snapshot.
type GateResult struct {
	Name   string `json:"name"`
	Passed bool   `json:"passed"`
	Detail string `json:"detail,omitempty"`
}

// Probes are the injectable §19.16.3 interlock sources. nil fields select
// the PG-backed defaults (deploy_windows / incidents / pg_stat_replication).
type Probes struct {
	// DeployWindowOpen reports whether a production deploy window covers
	// now; err = source unavailable (gate then fails closed).
	DeployWindowOpen func(ctx context.Context) (open bool, detail string, err error)
	// OpenIncidents returns the count of open P0/P1 incidents and the
	// source name; err = source unavailable (gate then fails closed).
	OpenIncidents func(ctx context.Context) (open int, source string, err error)
	// DRStandbyHealthy reports whether the secondary-region standby is
	// streaming inside the §18.3 lag bound; err = source unavailable.
	DRStandbyHealthy func(ctx context.Context) (healthy bool, detail string, err error)
}

// gateEvidence is the decoded releases.gate_evidence contract:
//
//	{"soak": {"status":"PASS","finished_at":"RFC3339"},
//	 "checkpoints": {"pending": 0}}
type gateEvidence struct {
	Soak struct {
		Status     string `json:"status"`
		FinishedAt string `json:"finished_at"`
	} `json:"soak"`
	Checkpoints struct {
		Pending int `json:"pending"`
	} `json:"checkpoints"`
}

// ListReleases returns releases inside the actor's environment context —
// the env scope applies to queries as well as mutations (§19.16.1). The
// optional env filter must equal the session env; status filters freely.
func (s *Service) ListReleases(ctx context.Context, actor Actor, env, status string) ([]Release, error) {
	sessEnv, err := requireEnv(actor)
	if err != nil {
		return nil, err
	}
	target := sessEnv
	if env != "" {
		target = admin.NormalizeEnv(env)
		if target != sessEnv {
			return nil, excerrors.New("FORBIDDEN",
				"release listing is scoped to the session's environment context")
		}
	}
	q := `SELECT ` + releaseCols + ` FROM releases WHERE env = $1`
	args := []any{target}
	if status != "" {
		args = append(args, status)
		q += fmt.Sprintf(" AND status = $%d", len(args))
	}
	q += " ORDER BY id DESC LIMIT 500"
	rows, err := s.pool.Query(ctx, q, args...)
	if err != nil {
		return nil, excerrors.Wrap("INTERNAL_ERROR", "list releases", err)
	}
	defer rows.Close()
	out := []Release{}
	for rows.Next() {
		var r Release
		if err := rows.Scan(&r.ID, &r.Component, &r.Version, &r.ArtifactHash,
			&r.Env, &r.Status, &r.GateEvidence, &r.Notes, &r.CreatedBy,
			&r.CreatedAt, &r.UpdatedAt); err != nil {
			return nil, excerrors.Wrap("INTERNAL_ERROR", "scan release", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// RegisterRelease records a new artifact version in dev — dev auto-deploys
// (§19.16.3), so the row lands DEPLOYED. Gate evidence is attached at
// registration and evaluated on promotion.
func (s *Service) RegisterRelease(ctx context.Context, actor Actor, in Release) (*Release, error) {
	if actor.UserID <= 0 {
		return nil, excerrors.New("UNAUTHORIZED", "admin identity required")
	}
	if _, err := requireEnv(actor); err != nil {
		return nil, err
	}
	if in.Component == "" || in.Version == "" {
		return nil, excerrors.New("INVALID_REQUEST",
			"component and version are required")
	}
	if len(in.ArtifactHash) != 64 {
		return nil, excerrors.New("INVALID_REQUEST",
			"artifact_hash must be a 64-hex sha256")
	}
	if len(in.GateEvidence) == 0 {
		in.GateEvidence = json.RawMessage(`{}`)
	}
	var r Release
	err := s.pool.QueryRow(ctx, `
		INSERT INTO releases
		    (component, version, artifact_hash, env, status, gate_evidence,
		     notes, created_by)
		VALUES ($1,$2,$3,'dev','DEPLOYED',$4,$5,$6)
		RETURNING `+releaseCols,
		in.Component, in.Version, in.ArtifactHash, in.GateEvidence,
		in.Notes, actor.UserID).
		Scan(&r.ID, &r.Component, &r.Version, &r.ArtifactHash, &r.Env,
			&r.Status, &r.GateEvidence, &r.Notes, &r.CreatedBy,
			&r.CreatedAt, &r.UpdatedAt)
	if err != nil {
		return nil, excerrors.Wrap("INTERNAL_ERROR", "register release", err)
	}
	return &r, nil
}

// evaluateGates runs the §19.16.3 gate set for a promotion attempt.
// Direction is a hard precondition evaluated by Promote before this runs
// (a direction violation is FORBIDDEN, not a gate failure). targetEnv is
// the normalized destination.
func (s *Service) evaluateGates(ctx context.Context, rel *Release, actor Actor, targetEnv string) []GateResult {
	gates := []GateResult{}

	// --- approval gate (staging: release-manager; production: four-eyes) ---
	if actor.ApproverID <= 0 {
		gates = append(gates, GateResult{Name: "approval", Passed: false,
			Detail: "approver_id required"})
	} else if actor.ApproverID == actor.UserID {
		gates = append(gates, GateResult{Name: "approval", Passed: false,
			Detail: "approver must differ from requester"})
	} else if s.resolve == nil {
		gates = append(gates, GateResult{Name: "approval", Passed: false,
			Detail: "role resolver unavailable"})
	} else if role, err := s.resolve(ctx, actor.ApproverID); err != nil {
		gates = append(gates, GateResult{Name: "approval", Passed: false,
			Detail: "approver role lookup failed: " + err.Error()})
	} else if !admin.Permits(gateway.RoleSuperAdmin, role) {
		gates = append(gates, GateResult{Name: "approval", Passed: false,
			Detail: "approver must satisfy Super Admin (release-manager authority maps to the §8.2 strongest role)"})
	} else {
		gates = append(gates, GateResult{Name: "approval", Passed: true,
			Detail: "approved_by " + fmt.Sprint(actor.ApproverID)})
	}

	if targetEnv != EnvProduction {
		return gates // staging: approval is the only gate
	}

	// --- production interlocks (§19.16.3) ---
	gates = append(gates, s.gateDeployWindow(ctx))
	gates = append(gates, s.gateIncidents(ctx))
	gates = append(gates, s.gateDRStandby(ctx))
	gates = append(gates, s.gateEvidence(rel)...)
	return gates
}

// gateDeployWindow checks an OPEN (or SCHEDULED-and-current) deploy window
// covers now for production.
func (s *Service) gateDeployWindow(ctx context.Context) GateResult {
	if probe := s.probes.DeployWindowOpen; probe != nil {
		open, detail, err := probe(ctx)
		if err != nil {
			return GateResult{Name: "deploy_window_open", Passed: false,
				Detail: "window source unavailable: " + err.Error()}
		}
		return GateResult{Name: "deploy_window_open", Passed: open, Detail: detail}
	}
	var n int
	err := s.pool.QueryRow(ctx, `
		SELECT count(*) FROM deploy_windows
		 WHERE environment = 'production'
		   AND status IN ('SCHEDULED','OPEN')
		   AND opens_at <= now() AND closes_at > now()`).Scan(&n)
	if err != nil {
		return GateResult{Name: "deploy_window_open", Passed: false,
			Detail: "window store unavailable: " + err.Error()}
	}
	if n == 0 {
		return GateResult{Name: "deploy_window_open", Passed: false,
			Detail: "no open production deploy window"}
	}
	return GateResult{Name: "deploy_window_open", Passed: true,
		Detail: fmt.Sprintf("%d open window(s)", n)}
}

// gateIncidents checks the open P0/P1 incident count via the probe;
// the PG default reads `incidents` (Phase-09 Task 9.3.18 store) — a
// missing table reports source-unavailable and fails the gate closed.
func (s *Service) gateIncidents(ctx context.Context) GateResult {
	probe := s.probes.OpenIncidents
	if probe == nil {
		probe = s.pgOpenIncidents
	}
	open, source, err := probe(ctx)
	if err != nil {
		return GateResult{Name: "no_p0_p1_incidents", Passed: false,
			Detail: "incident source unavailable: " + err.Error()}
	}
	if open > 0 {
		return GateResult{Name: "no_p0_p1_incidents", Passed: false,
			Detail: fmt.Sprintf("%d open P0/P1 incident(s) via %s", open, source)}
	}
	return GateResult{Name: "no_p0_p1_incidents", Passed: true,
		Detail: "none open via " + source}
}

func (s *Service) pgOpenIncidents(ctx context.Context) (int, string, error) {
	var n int
	err := s.pool.QueryRow(ctx, `
		SELECT count(*) FROM incidents
		 WHERE severity IN ('P0','P1')
		   AND upper(status) NOT IN ('RESOLVED','CLOSED')`).Scan(&n)
	if err != nil {
		return 0, "incidents", err
	}
	return n, "incidents", nil
}

// gateDRStandby checks the DR standby health via the probe; the PG
// default reads pg_stat_replication for a standby replaying within the
// §18.3 15s bound.
func (s *Service) gateDRStandby(ctx context.Context) GateResult {
	probe := s.probes.DRStandbyHealthy
	if probe == nil {
		probe = s.pgDRStandby
	}
	healthy, detail, err := probe(ctx)
	if err != nil {
		return GateResult{Name: "dr_standby_healthy", Passed: false,
			Detail: "DR source unavailable: " + err.Error()}
	}
	return GateResult{Name: "dr_standby_healthy", Passed: healthy, Detail: detail}
}

func (s *Service) pgDRStandby(ctx context.Context) (bool, string, error) {
	var n int
	err := s.pool.QueryRow(ctx, `
		SELECT count(*) FROM pg_stat_replication
		 WHERE state = 'streaming'
		   AND reply_time > now() - interval '15 seconds'`).Scan(&n)
	if err != nil {
		return false, "", err
	}
	if n == 0 {
		return false, "no standby streaming inside 15s", nil
	}
	return true, fmt.Sprintf("%d standby(s) streaming ≤15s", n), nil
}

// gateEvidence evaluates the release's evidence bundle: a fresh PASS soak
// report and zero pending checkpoints (parent's "checkpoints pending,
// soak stale" block conditions).
func (s *Service) gateEvidence(rel *Release) []GateResult {
	var ev gateEvidence
	gates := []GateResult{}
	if err := json.Unmarshal(rel.GateEvidence, &ev); err != nil {
		return append(gates,
			GateResult{Name: "soak_fresh", Passed: false,
				Detail: "gate_evidence unparseable"},
			GateResult{Name: "checkpoints_clear", Passed: false,
				Detail: "gate_evidence unparseable"})
	}
	fin, err := time.Parse(time.RFC3339, ev.Soak.FinishedAt)
	switch {
	case ev.Soak.Status != "PASS":
		gates = append(gates, GateResult{Name: "soak_fresh", Passed: false,
			Detail: "no PASS soak report in gate_evidence"})
	case err != nil:
		gates = append(gates, GateResult{Name: "soak_fresh", Passed: false,
			Detail: "soak.finished_at missing or not RFC3339"})
	case s.now().Sub(fin) > SoakMaxAge:
		gates = append(gates, GateResult{Name: "soak_fresh", Passed: false,
			Detail: "soak report stale (>14d)"})
	default:
		gates = append(gates, GateResult{Name: "soak_fresh", Passed: true,
			Detail: "soak PASS " + fin.Format("2006-01-02")})
	}
	// checkpoints.pending absent on an unparseable bundle is already
	// covered by the early return; a parseable bundle must declare it.
	var raw map[string]json.RawMessage
	_ = json.Unmarshal(rel.GateEvidence, &raw)
	if _, ok := raw["checkpoints"]; !ok {
		gates = append(gates, GateResult{Name: "checkpoints_clear", Passed: false,
			Detail: "gate_evidence.checkpoints missing"})
	} else if ev.Checkpoints.Pending > 0 {
		gates = append(gates, GateResult{Name: "checkpoints_clear", Passed: false,
			Detail: fmt.Sprintf("%d checkpoint(s) pending", ev.Checkpoints.Pending)})
	} else {
		gates = append(gates, GateResult{Name: "checkpoints_clear", Passed: true})
	}
	return gates
}

// Promote attempts a promotion of release id to toEnv under the actor's
// session env (the session must target the SOURCE env — a promotion is
// authored where the artifact currently sits). Every attempt persists a
// release_promotions row with the gate snapshot; blocked attempts return
// FORBIDDEN with the failed gate list.
func (s *Service) Promote(ctx context.Context, actor Actor, releaseID int64, toEnv, reason string) (*Promotion, []GateResult, error) {
	if actor.UserID <= 0 {
		return nil, nil, excerrors.New("UNAUTHORIZED", "admin identity required")
	}
	sessEnv, err := requireEnv(actor)
	if err != nil {
		return nil, nil, err
	}
	target := admin.NormalizeEnv(toEnv)

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, nil, excerrors.Wrap("INTERNAL_ERROR", "begin tx", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var rel Release
	err = tx.QueryRow(ctx, `
		SELECT `+releaseCols+` FROM releases WHERE id = $1 FOR UPDATE`, releaseID).
		Scan(&rel.ID, &rel.Component, &rel.Version, &rel.ArtifactHash,
			&rel.Env, &rel.Status, &rel.GateEvidence, &rel.Notes,
			&rel.CreatedBy, &rel.CreatedAt, &rel.UpdatedAt)
	if err == pgx.ErrNoRows {
		return nil, nil, excerrors.New("NOT_FOUND",
			fmt.Sprintf("release %d not found", releaseID))
	}
	if err != nil {
		return nil, nil, excerrors.Wrap("INTERNAL_ERROR", "lock release", err)
	}
	// Direction lattice (§19.16.1): dev→staging→production only; every
	// other move — including anything out of production or a prod-down
	// data move — is FORBIDDEN per the task text.
	if next, ok := nextEnv[rel.Env]; !ok || next != target {
		return nil, nil, excerrors.New("FORBIDDEN",
			fmt.Sprintf("promotion %s→%s violates the dev→staging→production direction", rel.Env, target))
	}
	// The session must target the DESTINATION env — a staging→production
	// promotion is authorized by a production-scoped binding (§8.2a env
	// axis, §19.16.4: a dev/staging binding grants nothing in prod). The
	// route's env-scoped flag already forced an explicit env axis.
	if target != sessEnv {
		return nil, nil, excerrors.New("FORBIDDEN",
			"session environment must target the promotion destination")
	}
	if rel.Status != RelDeployed && rel.Status != RelRegistered {
		return nil, nil, excerrors.New("INVALID_LIFECYCLE_TRANSITION",
			"release is "+rel.Status)
	}

	gates := s.evaluateGates(ctx, &rel, actor, target)
	blocked := false
	for _, g := range gates {
		if !g.Passed {
			blocked = true
		}
	}
	gatesJSON, _ := json.Marshal(gates)

	var approver *int64
	if actor.ApproverID > 0 {
		v := actor.ApproverID
		approver = &v
	}
	var p Promotion
	status := PromoExecuted
	if blocked {
		status = PromoBlocked
	}
	err = tx.QueryRow(ctx, `
		INSERT INTO release_promotions
		    (release_id, from_env, to_env, status, gates, requested_by,
		     approved_by, reason, decided_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8, now())
		RETURNING id, release_id, from_env, to_env, status, gates,
		          requested_by, approved_by, reason, created_at, decided_at`,
		releaseID, rel.Env, target, status, gatesJSON, actor.UserID,
		approver, reason).
		Scan(&p.ID, &p.ReleaseID, &p.FromEnv, &p.ToEnv, &p.Status, &p.Gates,
			&p.RequestedBy, &p.ApprovedBy, &p.Reason, &p.CreatedAt, &p.DecidedAt)
	if err != nil {
		return nil, nil, excerrors.Wrap("INTERNAL_ERROR", "record promotion", err)
	}

	if !blocked {
		if _, err := tx.Exec(ctx, `
			UPDATE releases SET env = $2, status = 'DEPLOYED', updated_at = now()
			 WHERE id = $1`, releaseID, target); err != nil {
			return nil, nil, excerrors.Wrap("INTERNAL_ERROR", "advance release", err)
		}
		// A promoted release supersedes prior releases of the same
		// component in the target env (registry hygiene — one live
		// version per component/env).
		if _, err := tx.Exec(ctx, `
			UPDATE releases SET status = 'SUPERSEDED', updated_at = now()
			 WHERE component = $1 AND env = $2 AND id <> $3
			   AND status = 'DEPLOYED'`,
			rel.Component, target, releaseID); err != nil {
			return nil, nil, excerrors.Wrap("INTERNAL_ERROR", "supersede", err)
		}
	}

	// Prod-context watermark (session env == promotion target): a
	// staging→production promotion is the prod context switch that
	// §19.16.4 requires watermarked in admin_audit_log.
	if err := s.WatermarkProdContext(ctx, tx, actor, "release.promote"); err != nil {
		return nil, nil, err
	}
	if _, _, err := admin.Log(ctx, tx, admin.AuditEntry{
		AdminUserID: actor.UserID,
		Action:      "fleet.release.promote",
		TargetType:  "release",
		TargetID:    &releaseID,
		BeforeState: map[string]any{"env": rel.Env, "status": rel.Status},
		AfterState: map[string]any{
			"to_env":      target,
			"promotion":   p.ID,
			"result":      status,
			"approved_by": approver,
			"gates":       gates,
		},
		IPAddress: actor.ClientIP,
	}); err != nil {
		return nil, nil, excerrors.Wrap("INTERNAL_ERROR", "audit log", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, nil, excerrors.Wrap("INTERNAL_ERROR", "commit", err)
	}
	if blocked {
		return &p, gates, excerrors.New("FORBIDDEN",
			"promotion gates failed: "+gateFailures(gates))
	}
	return &p, gates, nil
}

// gateFailures renders the failed-gate names for the FORBIDDEN detail.
func gateFailures(gates []GateResult) string {
	out := ""
	for _, g := range gates {
		if g.Passed {
			continue
		}
		if out != "" {
			out += ", "
		}
		out += g.Name
	}
	return out
}
