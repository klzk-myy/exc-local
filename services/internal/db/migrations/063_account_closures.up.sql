-- Migration 063 — Account closure & offboarding records
-- (Phase-14 Task 14.3.9).
--
-- One immutable row per closure: the persisted preconditions snapshot,
-- residual-sweep references and completion stamp survive the account
-- row's CLOSED transition for read-only audit retention (spec §14.9 /
-- Phase-14 Task 14.3.9). Closed accounts are never reopened — there is
-- deliberately no REOPENED/REOPEN_* status below.

BEGIN;

CREATE TABLE account_closures (
    id                     BIGSERIAL    PRIMARY KEY,
    closure_id             VARCHAR(40)  NOT NULL UNIQUE,  -- "aclose_<24hex>" public/audit id
    account_id             BIGINT       NOT NULL REFERENCES accounts(id),
    user_id                BIGINT       NOT NULL,          -- account owner at closure time
    reason                 TEXT         NOT NULL,          -- mandatory, client- or officer-provided
    forced                 BOOLEAN      NOT NULL DEFAULT false, -- compliance-forced closure
    initiated_by           BIGINT       NOT NULL,          -- client user id, or placing officer id
    approved_by            BIGINT,                           -- dual-control approver (forced path)
    dual_control_request_id BIGINT,                          -- admin_dual_control_requests.id (forced path)
    preconditions_snapshot JSONB        NOT NULL DEFAULT '{}', -- open counts + balance snapshot at check time
    sweep_refs             JSONB        NOT NULL DEFAULT '[]',  -- [{currency, withdrawal_id, amount, beneficiary_ref}]
    status                 VARCHAR(16)  NOT NULL DEFAULT 'COMPLETED' CHECK (status IN ('COMPLETED')),
    created_at             TIMESTAMPTZ  NOT NULL DEFAULT now(),
    completed_at           TIMESTAMPTZ
);

CREATE INDEX idx_account_closures_account ON account_closures (account_id);
CREATE INDEX idx_account_closures_user    ON account_closures (user_id);
CREATE INDEX idx_account_closures_created ON account_closures (created_at);

COMMENT ON TABLE account_closures IS
    'Phase-14 Task 14.3.9 — durable account-closure records. A row is '
    'written in the same transaction as the accounts.status → CLOSED '
    'transition; preconditions_snapshot records the precondition read '
    '(open positions/orders/settlements/funding) and sweep_refs the '
    'residual withdrawal ids sent through the Phase-11 pipeline. '
    'Closure is permanent — no reopen path exists by design.';

COMMIT;
