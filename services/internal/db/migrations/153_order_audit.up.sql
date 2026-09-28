-- 153_order_audit.up.sql
-- Phase-05 Task 5.3.22 (spec §24 #81/#82): order-modify audit trail.
-- The task text cites "024_create_order_audit" — that slot is long past;
-- migrations are append-only, so this lands at the next free number.
--
-- Columns per the task contract (audit_id, order_id, account_id,
-- field_name, old_value, new_value, modified_by, modified_at,
-- ip_address) plus two extensions required by sibling tasks:
--   operation  — Task 5.3.32 mandates batch-operation audit logging in
--                this same table and Task 5.3.25/5.3.37 need to
--                distinguish MASS_CANCEL / CANCEL_REPLACE / AMEND rows
--                from plain MODIFY entries.
--   request_id — X-Request-Id correlation (spec §8.7) so an audit row
--                can be tied back to the exact HTTP request.

BEGIN;

CREATE TABLE order_audit (
    audit_id    BIGSERIAL    PRIMARY KEY,
    order_id    BIGINT       NOT NULL REFERENCES orders (id),
    account_id  BIGINT       NOT NULL REFERENCES accounts (id),
    operation   VARCHAR(24)  NOT NULL DEFAULT 'MODIFY',
        -- MODIFY | AMEND | CANCEL_REPLACE | CANCEL | BATCH_SUBMIT |
        -- BATCH_CANCEL | MASS_CANCEL
    field_name  VARCHAR(64)  NOT NULL,
    old_value   TEXT,
    new_value   TEXT,
    modified_by VARCHAR(128) NOT NULL,   -- actor: user sub / API key id / "system:<reason>"
    request_id  VARCHAR(64),             -- X-Request-Id correlation
    modified_at TIMESTAMPTZ  NOT NULL DEFAULT now(),
    ip_address  VARCHAR(64)              -- INET-safe text; stays NULL when unresolved
);

CREATE INDEX idx_order_audit_order   ON order_audit (order_id, modified_at, audit_id);
CREATE INDEX idx_order_audit_account ON order_audit (account_id, modified_at, audit_id);

COMMIT;
