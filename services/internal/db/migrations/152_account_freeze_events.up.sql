-- 152_account_freeze_events.up.sql
-- Phase-05 Task 5.3.12: FROZEN legal-hold audit trail.
-- One row per freeze/unfreeze transition on accounts.status. The
-- admin_audit_log row is written alongside for the generic admin trail;
-- this table is the dedicated legal-hold record (reason + four-eyes
-- participants are first-class columns for regulator export).
-- approved_by is the second approver of the dual-control pair. During
-- Phase-05 the service enforces "approver != initiator" as the dual-
-- control stub; Phase-07 Tasks 7.3.1/7.3.2 replace the stub with full
-- RBAC + four-eyes middleware.

BEGIN;

CREATE TYPE freeze_action_enum AS ENUM ('FREEZE', 'UNFREEZE');

CREATE TABLE account_freeze_events (
    id           BIGSERIAL PRIMARY KEY,
    account_id   BIGINT             NOT NULL REFERENCES accounts (id),
    action       freeze_action_enum NOT NULL,
    reason       TEXT               NOT NULL,
    initiated_by BIGINT             NOT NULL,      -- admin user id
    approved_by  BIGINT             NOT NULL,      -- second approver (dual control)
    prev_status  VARCHAR(16)        NOT NULL,
    new_status   VARCHAR(16)        NOT NULL,
    metadata     JSONB              NOT NULL DEFAULT '{}',
    created_at   TIMESTAMPTZ        NOT NULL DEFAULT now()
);

CREATE INDEX idx_account_freeze_events_account ON account_freeze_events (account_id, created_at);

COMMIT;
