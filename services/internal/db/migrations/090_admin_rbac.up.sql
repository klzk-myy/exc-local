-- 090_admin_rbac.up.sql
-- Phase-07 Tasks 7.3.11/7.3.12 — scoped RBAC role bindings, role-system
-- separation, lifecycle (expiry/recertification) and break-glass grants,
-- per spec §8.2/§8.2a/§8.2b/§19.16.4 and §24 #348–#349.
--
--   principal_role_systems    one role system per principal — the DB-level
--                             disjoint-system exclusion (VENUE_ADMIN |
--                             CLIENT_DELEGATED | EXTERNAL_AUDITOR). Phase-12
--                             (migration 074, client_role_bindings) and
--                             Phase-24 (Task 24.3.18, EXTERNAL_AUDITOR)
--                             register their principals here too.
--   admin_role_bindings       VENUE_ADMIN bindings only — role, scope JSONB
--                             ({desks,regions,currencies,env}; NULL = global),
--                             granter, granted/expires, status lifecycle.
--   admin_dual_control_requests  generic four-eyes pending queue (Task 7.3.2):
--                             maker creates PENDING, a distinct approver
--                             confirms inside the 15-minute window.
--   admin_recert_campaigns /  quarterly campaign + per-binding decision rows;
--   admin_recert_decisions    PENDING bindings suspend at ends_at + 14 days.
--   admin_break_glass_grants  ≤4h incident-confined Super Admin grants with
--                             mandatory post-review inside 2 business days;
--                             overdue review suspends the granter's binding.

BEGIN;

-- ---------------------------------------------------------------------------
-- Disjoint role-system registry (spec §8.2a item 2).
-- ---------------------------------------------------------------------------
CREATE TABLE principal_role_systems (
    principal_id      BIGINT      PRIMARY KEY REFERENCES users (id),
    role_system       VARCHAR(24) NOT NULL
                      CHECK (role_system IN
                          ('VENUE_ADMIN', 'CLIENT_DELEGATED', 'EXTERNAL_AUDITOR')),
    first_granted_by  BIGINT      NOT NULL,
    created_at        TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- ---------------------------------------------------------------------------
-- Scoped admin role bindings (spec §8.2a items 1+3, §19.16.4).
-- status lifecycle: ACTIVE → SUSPENDED (recert/break-glass review breach) |
-- REVOKED (explicit) | EXPIRED (expires_at sweep + session kill).
-- ---------------------------------------------------------------------------
CREATE TABLE admin_role_bindings (
    id             BIGSERIAL    PRIMARY KEY,
    user_id        BIGINT       NOT NULL REFERENCES users (id),
    role           VARCHAR(32)  NOT NULL
                   CHECK (role IN
                       ('Super Admin','Risk Manager','Compliance Officer',
                        'Finance Ops','Support Agent','Read-Only Auditor')),
    kind           VARCHAR(16)  NOT NULL DEFAULT 'STANDARD'
                   CHECK (kind IN ('STANDARD','BREAK_GLASS')),
    scope          JSONB,                              -- NULL = global
    granter_id     BIGINT       NOT NULL REFERENCES users (id),
    granted_at     TIMESTAMPTZ  NOT NULL DEFAULT now(),
    expires_at     TIMESTAMPTZ  NOT NULL,
    status         VARCHAR(16)  NOT NULL DEFAULT 'ACTIVE'
                   CHECK (status IN ('ACTIVE','SUSPENDED','REVOKED','EXPIRED')),
    suspended_at   TIMESTAMPTZ,
    suspend_reason VARCHAR(64),
    revoked_at     TIMESTAMPTZ,
    revoked_by     BIGINT,
    revoke_reason  TEXT,
    updated_at     TIMESTAMPTZ  NOT NULL DEFAULT now(),
    -- Lifecycle caps (spec §8.2b): ≤12 months general, 90 days for
    -- Super Admin, ≤4h for break-glass.
    CONSTRAINT arb_expiry_cap
        CHECK (expires_at <= granted_at + interval '12 months'),
    CONSTRAINT arb_super_admin_cap
        CHECK (role <> 'Super Admin' OR kind = 'BREAK_GLASS'
               OR expires_at <= granted_at + interval '90 days'),
    CONSTRAINT arb_break_glass_cap
        CHECK (kind <> 'BREAK_GLASS'
               OR expires_at <= granted_at + interval '4 hours'),
    -- Scope shape: object with optional array axes desks/regions/
    -- currencies/env (§8.2a.3 / §19.16.4) plus optional scalar incident tag.
    CONSTRAINT arb_scope_shape CHECK (scope IS NULL OR (
        jsonb_typeof(scope) = 'object'
        AND (scope->'desks'      IS NULL OR jsonb_typeof(scope->'desks')      = 'array')
        AND (scope->'regions'    IS NULL OR jsonb_typeof(scope->'regions')    = 'array')
        AND (scope->'currencies' IS NULL OR jsonb_typeof(scope->'currencies') = 'array')
        AND (scope->'env'        IS NULL OR jsonb_typeof(scope->'env')        = 'array')
        AND (scope->'incident'   IS NULL OR jsonb_typeof(scope->'incident')   = 'string')
    ))
);

-- At most one ACTIVE binding per (user, role, kind).
CREATE UNIQUE INDEX admin_role_bindings_active_uniq
    ON admin_role_bindings (user_id, role, kind) WHERE status = 'ACTIVE';
CREATE INDEX idx_admin_role_bindings_user
    ON admin_role_bindings (user_id) WHERE status = 'ACTIVE';
CREATE INDEX idx_admin_role_bindings_expiry_sweep
    ON admin_role_bindings (expires_at) WHERE status = 'ACTIVE';

-- DB-level disjoint-system exclusion: inserting an admin_role_bindings row
-- registers the principal as VENUE_ADMIN; a principal already bound to
-- CLIENT_DELEGATED or EXTERNAL_AUDITOR is rejected at grant time (§8.2a.2).
CREATE OR REPLACE FUNCTION admin_role_binding_system_guard()
RETURNS trigger AS $$
DECLARE
    sys VARCHAR(24);
BEGIN
    INSERT INTO principal_role_systems (principal_id, role_system, first_granted_by)
    VALUES (NEW.user_id, 'VENUE_ADMIN', NEW.granter_id)
    ON CONFLICT (principal_id) DO NOTHING;
    SELECT role_system INTO sys
      FROM principal_role_systems WHERE principal_id = NEW.user_id;
    IF sys IS DISTINCT FROM 'VENUE_ADMIN' THEN
        RAISE EXCEPTION
            'principal % already bound to role system % — cross-system grant rejected',
            NEW.user_id, sys
            USING ERRCODE = 'check_violation';
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER trg_admin_role_bindings_system
    BEFORE INSERT ON admin_role_bindings
    FOR EACH ROW EXECUTE FUNCTION admin_role_binding_system_guard();

-- ---------------------------------------------------------------------------
-- Generic four-eyes pending queue (Task 7.3.2, spec §8.2 dual control).
-- Maker creates PENDING (expires_at = created_at + 15min); a distinct
-- eligible approver confirms → APPROVED/EXECUTED; window lapse → EXPIRED.
-- ---------------------------------------------------------------------------
CREATE TABLE admin_dual_control_requests (
    id            BIGSERIAL    PRIMARY KEY,
    operation     VARCHAR(64)  NOT NULL,  -- kill-switch | balance-adjustment |
                  -- manual-liquidation | withdrawal-override | admin-role-change |
                  -- fee-tier-change | release-suspended-account |
                  -- deploy-to-production | break-glass | ...
    target_type   VARCHAR(64)  NOT NULL,
    target_id     VARCHAR(128) NOT NULL,  -- text: covers non-numeric targets
    payload       JSONB        NOT NULL DEFAULT '{}'::jsonb,
    required_role VARCHAR(32)  NOT NULL,  -- §8.2 role the approver must hold
    requested_by  BIGINT       NOT NULL REFERENCES users (id),
    approved_by   BIGINT       REFERENCES users (id),
    status        VARCHAR(16)  NOT NULL DEFAULT 'PENDING'
                  CHECK (status IN ('PENDING','APPROVED','EXECUTED','REJECTED','EXPIRED')),
    reason        TEXT,
    created_at    TIMESTAMPTZ  NOT NULL DEFAULT now(),
    expires_at    TIMESTAMPTZ  NOT NULL,
    decided_at    TIMESTAMPTZ,
    CONSTRAINT dcr_distinct_approvers
        CHECK (approved_by IS NULL OR approved_by <> requested_by),
    CONSTRAINT dcr_window_cap
        CHECK (expires_at <= created_at + interval '15 minutes')
);

CREATE INDEX idx_admin_dual_control_pending
    ON admin_dual_control_requests (status, expires_at) WHERE status = 'PENDING';

-- ---------------------------------------------------------------------------
-- Quarterly recertification (Task 7.3.12 item 2, spec §8.2b.1).
-- ---------------------------------------------------------------------------
CREATE TABLE admin_recert_campaigns (
    id         BIGSERIAL    PRIMARY KEY,
    label      VARCHAR(16)  NOT NULL UNIQUE,   -- e.g. '2026-Q4'
    started_by BIGINT       NOT NULL REFERENCES users (id),
    started_at TIMESTAMPTZ  NOT NULL DEFAULT now(),
    ends_at    TIMESTAMPTZ  NOT NULL,
    status     VARCHAR(16)  NOT NULL DEFAULT 'OPEN'
               CHECK (status IN ('OPEN','CLOSED')),
    closed_at  TIMESTAMPTZ,
    CONSTRAINT recert_window CHECK (ends_at > started_at)
);

CREATE TABLE admin_recert_decisions (
    id          BIGSERIAL   PRIMARY KEY,
    campaign_id BIGINT      NOT NULL REFERENCES admin_recert_campaigns (id),
    binding_id  BIGINT      NOT NULL REFERENCES admin_role_bindings (id),
    user_id     BIGINT      NOT NULL,
    role        VARCHAR(32) NOT NULL,
    decision    VARCHAR(16) NOT NULL DEFAULT 'PENDING'
                CHECK (decision IN ('PENDING','APPROVED','SUSPENDED')),
    decided_by  BIGINT,
    decided_at  TIMESTAMPTZ,
    suspend_at  TIMESTAMPTZ  NOT NULL,   -- campaign ends_at + 14 days
    UNIQUE (campaign_id, binding_id)
);

CREATE INDEX idx_admin_recert_pending
    ON admin_recert_decisions (suspend_at) WHERE decision = 'PENDING';

-- ---------------------------------------------------------------------------
-- Break-glass grants (Task 7.3.12 item 3, spec §8.2b.2).
-- ---------------------------------------------------------------------------
CREATE TABLE admin_break_glass_grants (
    id                 BIGSERIAL    PRIMARY KEY,
    binding_id         BIGINT       NOT NULL UNIQUE REFERENCES admin_role_bindings (id),
    grantee_id         BIGINT       NOT NULL REFERENCES users (id),
    granter_id         BIGINT       NOT NULL REFERENCES users (id),
    second_approver_id BIGINT       REFERENCES users (id),  -- NULL only via the
                                    -- unreachable-approver path (p0 alert raised)
    p0_alert           BOOLEAN      NOT NULL DEFAULT FALSE,
    incident_ref       VARCHAR(64)  NOT NULL,
    reason             TEXT         NOT NULL,
    granted_at         TIMESTAMPTZ  NOT NULL DEFAULT now(),
    expires_at         TIMESTAMPTZ  NOT NULL,
    review_due_at      TIMESTAMPTZ  NOT NULL,   -- granted_at + 2 business days
    reviewed_at        TIMESTAMPTZ,
    reviewed_by        BIGINT,
    review_notes       TEXT,
    status             VARCHAR(24)  NOT NULL DEFAULT 'ACTIVE'
                       CHECK (status IN
                           ('ACTIVE','EXPIRED','REVIEWED','GRANTER_SUSPENDED')),
    CONSTRAINT bg_timebox
        CHECK (expires_at <= granted_at + interval '4 hours'),
    CONSTRAINT bg_distinct_approvers
        CHECK (second_approver_id IS NULL OR second_approver_id <> granter_id),
    CONSTRAINT bg_no_self_grant
        CHECK (grantee_id <> granter_id)
);

CREATE INDEX idx_admin_break_glass_active
    ON admin_break_glass_grants (expires_at) WHERE status = 'ACTIVE';
CREATE INDEX idx_admin_break_glass_review_sweep
    ON admin_break_glass_grants (review_due_at)
    WHERE reviewed_at IS NULL AND status IN ('ACTIVE','EXPIRED');

COMMIT;
