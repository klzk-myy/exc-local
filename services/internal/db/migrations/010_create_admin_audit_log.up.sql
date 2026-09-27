-- 010_create_admin_audit_log.up.sql
-- Spec §5.9 admin_audit_log.

BEGIN;

CREATE TABLE admin_audit_log (
    id            BIGSERIAL PRIMARY KEY,
    admin_user_id BIGINT NOT NULL,
    action        VARCHAR(128) NOT NULL,                    -- e.g. withdrawal.approve, instrument.suspend
    target_type   VARCHAR(64),
    target_id     BIGINT,
    before_state  JSONB,
    after_state   JSONB,
    ip_address    INET,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);

COMMIT;
