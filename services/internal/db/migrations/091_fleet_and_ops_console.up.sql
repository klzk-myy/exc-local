-- 091_fleet_and_ops_console.up.sql
-- Phase-09 Task 9.3.30 (spec §19.16, §24 #350) — environment context model,
-- fleet inventory, server actions and promotion gates behind the admin
-- console. Phase-15 Task 15.3.12 (spec §7.5, §24 #352) — listing_proposals
-- for the ops-console instrument workflow.
--
--   environments        the three §19.16.1 environments (dev/staging/
--                       production) with topology profile + promotion
--                       policy. Seeded; extend topology/promotion_policy
--                       rather than adding env names.
--   fleet_hosts         per-environment host inventory (role, shard, AZ/
--                       rack, hardware spec, health, lifecycle state).
--   server_actions      durable record of dual-controlled host actions
--                       (drain/cordon/reboot/decommission) with maker/
--                       checker evidence.
--   releases            deployable artifact registry (component, version,
--                       artifact hash, current env stage, gate evidence).
--   release_promotions  promotion attempts: direction check, gate snapshot,
--                       maker/approver — append-only audit spine.
--   deploy_windows      open deploy-window interlock for production
--                       promotions (§19.16.3).
--   listing_proposals   Phase-15 Task 15.3.12 instrument-listing pipeline
--                       (PROPOSED → IN_REVIEW → APPROVED/REJECTED →
--                       SCHEDULED).

BEGIN;

-- ---------------------------------------------------------------------------
-- Environments (spec §19.16.1). Three rows, one control plane.
-- promotion_policy documents the gate set per env (JSONB so the console can
-- render it; enforcement is code — services/internal/fleet).
-- ---------------------------------------------------------------------------
CREATE TABLE environments (
    id               BIGSERIAL    PRIMARY KEY,
    name             VARCHAR(16)  NOT NULL UNIQUE
                     CHECK (name IN ('dev','staging','production')),
    topology_profile JSONB        NOT NULL DEFAULT '{}'::jsonb,
    promotion_policy JSONB        NOT NULL DEFAULT '{}'::jsonb,
    created_at       TIMESTAMPTZ  NOT NULL DEFAULT now(),
    updated_at       TIMESTAMPTZ  NOT NULL DEFAULT now()
);

INSERT INTO environments (name, topology_profile, promotion_policy) VALUES
    ('dev',
     '{"profile":"single-host","notes":"Task 9.3.28 compose profile"}',
     '{"auto_deploy":true,"approvals_required":0}'),
    ('staging',
     '{"profile":"mini-mirror","notes":"1 metal + small K8s/PG/CH"}',
     '{"auto_deploy":false,"approvals_required":1,"approver_role":"Super Admin"}'),
    ('production',
     '{"profile":"full","notes":"spec §19.13 fleet"}',
     '{"auto_deploy":false,"approvals_required":2,"dual_control":true,"interlocks":["deploy_window_open","no_p0_p1_incidents","dr_standby_healthy"]}')
ON CONFLICT (name) DO NOTHING;

-- ---------------------------------------------------------------------------
-- Fleet inventory (spec §19.16.2). state machine:
--   ACTIVE → DRAINING → MAINTENANCE → ACTIVE | DECOMMISSIONED
--   ACTIVE → MAINTENANCE → ACTIVE ; DECOMMISSIONED is terminal.
-- health is the last-observed rollup from the systemd/K8s/PG/Redis/CH/NATS
-- health sync; UNKNOWN until a source reports.
-- ---------------------------------------------------------------------------
CREATE TABLE fleet_hosts (
    id             BIGSERIAL    PRIMARY KEY,
    environment_id BIGINT       NOT NULL REFERENCES environments (id),
    hostname       TEXT         NOT NULL,
    role           VARCHAR(16)  NOT NULL
                   CHECK (role IN ('CORE','GATEWAY','POSTGRES','REDIS',
                                   'CLICKHOUSE','NATS','FIX','EDGE',
                                   'MONITORING','WORKER')),
    shard_id       SMALLINT     CHECK (shard_id >= 0),
    az             VARCHAR(32)  NOT NULL DEFAULT '',
    rack           VARCHAR(32)  NOT NULL DEFAULT '',
    hw_spec        JSONB        NOT NULL DEFAULT '{}'::jsonb,
    health         VARCHAR(16)  NOT NULL DEFAULT 'UNKNOWN'
                   CHECK (health IN ('HEALTHY','DEGRADED','DOWN','UNKNOWN')),
    state          VARCHAR(16)  NOT NULL DEFAULT 'ACTIVE'
                   CHECK (state IN
                       ('ACTIVE','DRAINING','MAINTENANCE','DECOMMISSIONED')),
    created_at     TIMESTAMPTZ  NOT NULL DEFAULT now(),
    updated_at     TIMESTAMPTZ  NOT NULL DEFAULT now(),
    UNIQUE (environment_id, hostname)
);
CREATE INDEX fleet_hosts_env_state_idx
    ON fleet_hosts (environment_id, state);
CREATE INDEX fleet_hosts_shard_idx
    ON fleet_hosts (shard_id) WHERE shard_id IS NOT NULL;

-- ---------------------------------------------------------------------------
-- Server actions (spec §19.16.2/§19.16.4). Prod actions are dual-controlled
-- sensitive ops: requested_by/approved_by are the maker/checker pair and
-- both land in admin_audit_log via the service layer.
-- ---------------------------------------------------------------------------
CREATE TABLE server_actions (
    id           BIGSERIAL    PRIMARY KEY,
    host_id      BIGINT       NOT NULL REFERENCES fleet_hosts (id),
    action       VARCHAR(16)  NOT NULL
                 CHECK (action IN ('DRAIN','CORDON','REBOOT','DECOMMISSION')),
    status       VARCHAR(16)  NOT NULL DEFAULT 'PENDING'
                 CHECK (status IN
                     ('PENDING','APPROVED','EXECUTED','REJECTED','EXPIRED')),
    requested_by BIGINT       NOT NULL REFERENCES users (id),
    approved_by  BIGINT       REFERENCES users (id),
    reason       TEXT         NOT NULL,
    payload      JSONB        NOT NULL DEFAULT '{}'::jsonb,
    created_at   TIMESTAMPTZ  NOT NULL DEFAULT now(),
    decided_at   TIMESTAMPTZ,
    expires_at   TIMESTAMPTZ  NOT NULL DEFAULT now() + interval '15 minutes'
);
CREATE INDEX server_actions_host_idx ON server_actions (host_id, id);
CREATE INDEX server_actions_pending_idx
    ON server_actions (status) WHERE status = 'PENDING';

-- ---------------------------------------------------------------------------
-- Releases + promotions (spec §19.16.3).
--   releases.env          the environment stage the artifact currently sits
--                         at (dev → staging → production only; production
--                         never demotes).
--   releases.gate_evidence  JSONB bundle the promotion gates evaluate:
--                         soak report ref/freshness, checkpoint ids,
--                         load-test result refs.
--   release_promotions.gates  snapshot of every gate evaluated for the
--                         attempt — the audit spine for "why was this
--                         allowed/refused".
-- ---------------------------------------------------------------------------
CREATE TABLE releases (
    id             BIGSERIAL    PRIMARY KEY,
    component      TEXT         NOT NULL,
    version        TEXT         NOT NULL,
    artifact_hash  CHAR(64)     NOT NULL,   -- sha256 hex of the artifact
    env            VARCHAR(16)  NOT NULL
                   CHECK (env IN ('dev','staging','production')),
    status         VARCHAR(16)  NOT NULL DEFAULT 'REGISTERED'
                   CHECK (status IN
                       ('REGISTERED','DEPLOYED','REJECTED','SUPERSEDED')),
    gate_evidence  JSONB        NOT NULL DEFAULT '{}'::jsonb,
    notes          TEXT         NOT NULL DEFAULT '',
    created_by     BIGINT       NOT NULL REFERENCES users (id),
    created_at     TIMESTAMPTZ  NOT NULL DEFAULT now(),
    updated_at     TIMESTAMPTZ  NOT NULL DEFAULT now(),
    UNIQUE (component, version)
);
CREATE INDEX releases_env_idx ON releases (env, status);

CREATE TABLE release_promotions (
    id           BIGSERIAL    PRIMARY KEY,
    release_id   BIGINT       NOT NULL REFERENCES releases (id),
    from_env     VARCHAR(16)  NOT NULL
                 CHECK (from_env IN ('dev','staging','production')),
    to_env       VARCHAR(16)  NOT NULL
                 CHECK (to_env IN ('dev','staging','production')),
    status       VARCHAR(16)  NOT NULL DEFAULT 'PENDING'
                 CHECK (status IN
                     ('PENDING','APPROVED','EXECUTED','REJECTED','BLOCKED')),
    gates        JSONB        NOT NULL DEFAULT '[]'::jsonb,
    requested_by BIGINT       NOT NULL REFERENCES users (id),
    approved_by  BIGINT       REFERENCES users (id),
    reason       TEXT         NOT NULL DEFAULT '',
    created_at   TIMESTAMPTZ  NOT NULL DEFAULT now(),
    decided_at   TIMESTAMPTZ
);
CREATE INDEX release_promotions_release_idx
    ON release_promotions (release_id, id);

-- ---------------------------------------------------------------------------
-- Deploy windows (spec §19.16.3 interlock "open deploy window"): a
-- production promotion is only legal while an OPEN window covers now().
-- Windows are opened/closed through the console, not edited in place.
-- ---------------------------------------------------------------------------
CREATE TABLE deploy_windows (
    id          BIGSERIAL    PRIMARY KEY,
    environment VARCHAR(16)  NOT NULL
                CHECK (environment IN ('dev','staging','production')),
    opens_at    TIMESTAMPTZ  NOT NULL,
    closes_at   TIMESTAMPTZ  NOT NULL CHECK (closes_at > opens_at),
    status      VARCHAR(16)  NOT NULL DEFAULT 'SCHEDULED'
                CHECK (status IN ('SCHEDULED','OPEN','CLOSED','CANCELLED')),
    opened_by   BIGINT       NOT NULL REFERENCES users (id),
    reason      TEXT         NOT NULL DEFAULT '',
    created_at  TIMESTAMPTZ  NOT NULL DEFAULT now()
);
CREATE INDEX deploy_windows_open_idx
    ON deploy_windows (environment, opens_at, closes_at)
    WHERE status IN ('SCHEDULED','OPEN');

-- ---------------------------------------------------------------------------
-- Listing proposals (Phase-15 Task 15.3.12, spec §7.5, §24 #352).
-- Auto-checks (reference row well-formed, ≥2 oracle feeds, risk defaults
-- populated) land in the service layer; this table is the durable record.
-- ---------------------------------------------------------------------------
CREATE TABLE listing_proposals (
    id              BIGSERIAL    PRIMARY KEY,
    proposer_id     BIGINT       NOT NULL REFERENCES users (id),
    symbol          VARCHAR(32)  NOT NULL,
    reference_row   JSONB        NOT NULL DEFAULT '{}'::jsonb,
    oracle_coverage JSONB        NOT NULL DEFAULT '{}'::jsonb,
    risk_defaults   JSONB        NOT NULL DEFAULT '{}'::jsonb,
    auto_checks     JSONB        NOT NULL DEFAULT '{}'::jsonb,
    status          VARCHAR(16)  NOT NULL DEFAULT 'PROPOSED'
                    CHECK (status IN
                        ('PROPOSED','IN_REVIEW','APPROVED','REJECTED','SCHEDULED')),
    reason          TEXT         NOT NULL DEFAULT '',
    created_at      TIMESTAMPTZ  NOT NULL DEFAULT now(),
    updated_at      TIMESTAMPTZ  NOT NULL DEFAULT now()
);
-- One live proposal per symbol — re-proposal requires the prior row to be
-- REJECTED or SCHEDULED (both terminal for the pipeline).
CREATE UNIQUE INDEX listing_proposals_live_symbol_idx
    ON listing_proposals (symbol)
    WHERE status IN ('PROPOSED','IN_REVIEW','APPROVED');

COMMIT;
