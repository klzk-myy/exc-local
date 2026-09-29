-- 074_client_delegation.up.sql
-- Phase-12 Task 12.3.11 — Institutional Delegated Logins and Client
-- Multi-Validator Controls (spec §24 #285).
--
-- Client-side workforce RBAC, DISTINCT from the venue-admin RBAC of
-- migration 090 (admin_role_bindings). A master account names human
-- logins (client_delegated_users), grants them client roles with
-- explicit account/sub-account/instrument scopes (client_role_bindings),
-- and configures M-of-N approval policies over money-moving operations
-- (client_approval_policies / client_approval_requests /
-- client_approval_decisions). Every transition appends to the
-- append-only client_delegation_events audit trail.
--
-- Delegated principals register in principal_role_systems (migration
-- 090) with role_system='CLIENT_DELEGATED' — the disjoint-system
-- exclusion means a VENUE_ADMIN principal can never also be a client
-- delegate and vice versa.
--
-- Roles (CHECK constraint, not enum — keeps the vocabulary grep-able):
--   CLIENT_READ_ONLY        read-only views
--   CLIENT_TRADER           order entry/cancel within scoped instruments;
--                           cannot withdraw, transfer, or change security
--   CLIENT_FINANCE_MANAGER  internal transfers within the entitled
--                           master→sub-account hierarchy only
--   CLIENT_APPROVER         M-of-N approver; may approve but never
--                           initiate configured operations
--
-- Approval operations: WITHDRAWAL, BENEFICIARY_CHANGE,
-- API_KEY_PRIVILEGE_CHANGE, INTERNAL_TRANSFER (high-value).

BEGIN;

-- ---------------------------------------------------------------------------
-- Named human logins under an institutional master account.
-- status: ACTIVE → SUSPENDED (temporary disable) | REVOKED (terminal).
-- ---------------------------------------------------------------------------
CREATE TABLE client_delegated_users (
    id                BIGSERIAL    PRIMARY KEY,
    master_account_id BIGINT       NOT NULL REFERENCES accounts (id),
    user_id           BIGINT       NOT NULL REFERENCES users (id),
    display_name      VARCHAR(128) NOT NULL,
    status            VARCHAR(16)  NOT NULL DEFAULT 'ACTIVE'
                      CHECK (status IN ('ACTIVE','SUSPENDED','REVOKED')),
    created_by        BIGINT       NOT NULL,               -- master owner user id
    created_at        TIMESTAMPTZ  NOT NULL DEFAULT now(),
    updated_at        TIMESTAMPTZ  NOT NULL DEFAULT now(),
    suspended_at      TIMESTAMPTZ,
    suspend_reason    VARCHAR(128),
    revoked_at        TIMESTAMPTZ,
    revoked_by        BIGINT,
    revoke_reason     TEXT,
    UNIQUE (master_account_id, user_id)
);
CREATE INDEX cdu_master_active_ix
    ON client_delegated_users (master_account_id) WHERE status = 'ACTIVE';
CREATE INDEX cdu_user_active_ix
    ON client_delegated_users (user_id) WHERE status = 'ACTIVE';

-- ---------------------------------------------------------------------------
-- Role + scope bindings. At most one ACTIVE binding per delegated user
-- (a scope change rewrites the binding; history stays on superseded rows).
-- scope shape: {"account_ids": [bigint,...], "instruments": ["EURUSD",...]}
-- — each axis is an explicit allowlist; an absent/empty axis grants nothing.
-- ---------------------------------------------------------------------------
CREATE TABLE client_role_bindings (
    id                BIGSERIAL   PRIMARY KEY,
    delegated_user_id BIGINT      NOT NULL REFERENCES client_delegated_users (id),
    role              VARCHAR(24) NOT NULL
                      CHECK (role IN ('CLIENT_READ_ONLY','CLIENT_FINANCE_MANAGER',
                                      'CLIENT_TRADER','CLIENT_APPROVER')),
    scope             JSONB       NOT NULL DEFAULT '{}',
    granted_by        BIGINT      NOT NULL,
    granted_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    expires_at        TIMESTAMPTZ,                          -- NULL = no expiry
    status            VARCHAR(16) NOT NULL DEFAULT 'ACTIVE'
                      CHECK (status IN ('ACTIVE','SUSPENDED','REVOKED','EXPIRED')),
    revoked_at        TIMESTAMPTZ,
    revoked_by        BIGINT,
    revoke_reason     TEXT,
    updated_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT crb_scope_shape CHECK (
        jsonb_typeof(scope) = 'object'
        AND (scope->'account_ids' IS NULL OR jsonb_typeof(scope->'account_ids') = 'array')
        AND (scope->'instruments' IS NULL OR jsonb_typeof(scope->'instruments') = 'array'))
);
-- One live binding per delegated user.
CREATE UNIQUE INDEX crb_active_uq
    ON client_role_bindings (delegated_user_id) WHERE status = 'ACTIVE';
CREATE INDEX crb_delegated_ix ON client_role_bindings (delegated_user_id);

-- ---------------------------------------------------------------------------
-- M-of-N approval policies. One ACTIVE policy per (master, operation).
-- required_approvals = M; N is the live count of CLIENT_APPROVER bindings
-- (evaluated at request time). threshold_amount/currency scope the policy
-- to high-value operations; NULL threshold applies to every amount.
-- expires_in_seconds bounds the approval window (60s..24h).
-- ---------------------------------------------------------------------------
CREATE TABLE client_approval_policies (
    id                 BIGSERIAL     PRIMARY KEY,
    master_account_id  BIGINT        NOT NULL REFERENCES accounts (id),
    operation          VARCHAR(48)   NOT NULL
                       CHECK (operation IN ('WITHDRAWAL','BENEFICIARY_CHANGE',
                                            'API_KEY_PRIVILEGE_CHANGE','INTERNAL_TRANSFER')),
    required_approvals INT           NOT NULL CHECK (required_approvals >= 1),
    threshold_amount   DECIMAL(28,8) CHECK (threshold_amount IS NULL OR threshold_amount > 0),
    threshold_currency VARCHAR(3),
    expires_in_seconds INT           NOT NULL DEFAULT 3600
                       CHECK (expires_in_seconds BETWEEN 60 AND 86400),
    status             VARCHAR(16)   NOT NULL DEFAULT 'ACTIVE'
                       CHECK (status IN ('ACTIVE','DISABLED')),
    created_by         BIGINT        NOT NULL,
    created_at         TIMESTAMPTZ   NOT NULL DEFAULT now(),
    updated_at         TIMESTAMPTZ   NOT NULL DEFAULT now(),
    UNIQUE (master_account_id, operation)
);

-- ---------------------------------------------------------------------------
-- Approval requests: PENDING → APPROVED | REJECTED | EXPIRED | CANCELLED;
-- APPROVED → CONSUMED once the gated operation replays through the seam.
-- fingerprint = sha256 of the canonical operation+payload — lets the
-- funding path match an approved request to a resubmitted withdrawal.
-- ---------------------------------------------------------------------------
CREATE TABLE client_approval_requests (
    id                     BIGSERIAL   PRIMARY KEY,
    master_account_id      BIGINT      NOT NULL REFERENCES accounts (id),
    policy_id              BIGINT      NOT NULL REFERENCES client_approval_policies (id),
    operation              VARCHAR(48) NOT NULL,
    payload                JSONB       NOT NULL DEFAULT '{}',
    fingerprint            VARCHAR(64) NOT NULL,
    requested_by_user      BIGINT      NOT NULL,
    requested_by_delegated BIGINT      REFERENCES client_delegated_users (id),
    status                 VARCHAR(16) NOT NULL DEFAULT 'PENDING'
                           CHECK (status IN ('PENDING','APPROVED','REJECTED',
                                             'EXPIRED','CANCELLED','CONSUMED')),
    approvals_count        INT         NOT NULL DEFAULT 0,
    required_approvals     INT         NOT NULL,
    expires_at             TIMESTAMPTZ NOT NULL,
    created_at             TIMESTAMPTZ NOT NULL DEFAULT now(),
    decided_at             TIMESTAMPTZ,
    consumed_at            TIMESTAMPTZ
);
CREATE INDEX car_master_status_ix
    ON client_approval_requests (master_account_id, status);
CREATE INDEX car_pending_expiry_ix
    ON client_approval_requests (expires_at) WHERE status = 'PENDING';
CREATE INDEX car_fingerprint_ix
    ON client_approval_requests (master_account_id, operation, fingerprint);

-- ---------------------------------------------------------------------------
-- Approver votes. UNIQUE(request_id, approver_user_id) enforces one vote
-- per approver; anti-self-approval is enforced in the service layer
-- (approver_user_id <> requested_by_user).
-- ---------------------------------------------------------------------------
CREATE TABLE client_approval_decisions (
    id                BIGSERIAL   PRIMARY KEY,
    request_id        BIGINT      NOT NULL REFERENCES client_approval_requests (id),
    approver_user_id  BIGINT      NOT NULL,
    delegated_user_id BIGINT      REFERENCES client_delegated_users (id),
    decision          VARCHAR(8)  NOT NULL CHECK (decision IN ('APPROVE','REJECT')),
    note              TEXT,
    created_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (request_id, approver_user_id)
);
CREATE INDEX cad_request_ix ON client_approval_decisions (request_id);

-- ---------------------------------------------------------------------------
-- Append-only audit trail: every delegation, login, action, approval,
-- revocation and scope change. Insert-only by contract — no UPDATE or
-- DELETE is ever issued against this table (spec §2.7 auditability).
-- ---------------------------------------------------------------------------
CREATE TABLE client_delegation_events (
    id                BIGSERIAL   PRIMARY KEY,
    master_account_id BIGINT      NOT NULL,
    actor_user_id     BIGINT,                       -- NULL = system
    delegated_user_id BIGINT,
    event             VARCHAR(48) NOT NULL,          -- e.g. DELEGATION_CREATED
    target_type       VARCHAR(32),
    target_id         BIGINT,
    detail            JSONB       NOT NULL DEFAULT '{}',
    created_at        TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX cde_master_ix ON client_delegation_events (master_account_id, created_at);
CREATE INDEX cde_delegated_ix
    ON client_delegation_events (delegated_user_id, created_at);

COMMIT;
