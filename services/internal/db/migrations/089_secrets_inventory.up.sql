-- 089_secrets_inventory
-- Phase-09 Task 9.3.29 item 4 (spec §19.14, §24 #340) — the secrets
-- inventory register. One row per secret (banking API keys, FIX mTLS
-- certs, OAuth secrets, KMS grants, alerting keys, data-path creds):
-- owner, TTL, rotation procedure and last-rotated timestamp, plus the
-- DR-criticality flag item 5 verifies against the quarterly drill.
--
-- Metadata only — no secret material is ever stored here (docs/ops/
-- secrets-inventory.md rule zero); values live exclusively in
-- Vault/KMS and deploy-time env injection.
--
-- secret_class is check-constrained to the Phase-13.5 Task 13.5.3.5
-- SecretClass taxonomy (services/internal/security/rotation.go) so the
-- row joins the per-class RotationPolicy table; the ops doc's finer
-- §1 labels (OAuth secret / KMS grant / PagerDuty key / …) ride in
-- `category`.
--
-- The overdue predicate the SECRET_ROTATION_OVERDUE (503) enforcer
-- evaluates is last_rotated_at + ttl_seconds < now(); the per-row
-- alert_lead_seconds carries the 14-day P2 lead (default 1209600s) so
-- short-TTL secrets can tighten it. A NULL last_rotated_at means the
-- secret was inventoried but never rotated — surfaced as UNROTATED,
-- never silently OK.

BEGIN;

CREATE TABLE secrets_inventory (
    id                 BIGSERIAL    PRIMARY KEY,
    secret_name        TEXT         NOT NULL UNIQUE,      -- canonical name, e.g. 'jwt-hs256-key', 'fix/mtls/client-certs/session-1'
    secret_class       VARCHAR(32)  NOT NULL
                       CHECK (secret_class IN (
                           'jwt_signing_key', 'api_key_material', 'db_credential',
                           'redis_password', 'aeron_auth_token',
                           'tls_certificate', 'banking_api_key')),
    category           TEXT         NOT NULL DEFAULT '',  -- ops doc §1 label (banking API key / FIX mTLS cert / OAuth secret / KMS grant / webhook / PagerDuty key / Slack webhook / DB credential / S3 credential)
    owner              TEXT         NOT NULL,             -- team + named individual
    consumers          TEXT[]       NOT NULL DEFAULT '{}',-- services/hosts that resolve it
    ttl_seconds        BIGINT       NOT NULL CHECK (ttl_seconds > 0),
    alert_lead_seconds BIGINT       NOT NULL DEFAULT 1209600  -- 14-day P2 alert lead (spec §19.14)
                       CHECK (alert_lead_seconds > 0
                              AND alert_lead_seconds <= ttl_seconds),
    rotation_procedure TEXT         NOT NULL DEFAULT '',  -- runbook ref or issue→store→distribute→reload→verify→mark steps
    last_rotated_at    TIMESTAMPTZ,                        -- NULL = never rotated (UNROTATED)
    dr_critical        BOOLEAN      NOT NULL DEFAULT false,-- item 5: requires tested secondary-region copy
    break_glass_path   TEXT         NOT NULL DEFAULT '',  -- emergency path reference (Task 7.3.12 break-glass + post-review)
    created_at         TIMESTAMPTZ  NOT NULL DEFAULT now(),
    updated_at         TIMESTAMPTZ  NOT NULL DEFAULT now()
);

COMMENT ON TABLE  secrets_inventory IS 'Phase-09 Task 9.3.29 item 4: per-secret rotation inventory (metadata only — values live in Vault/KMS). Overdue rows raise SECRET_ROTATION_OVERDUE (503).';
COMMENT ON COLUMN secrets_inventory.last_rotated_at    IS 'Last completed rotation; NULL = never rotated (UNROTATED state).';
COMMENT ON COLUMN secrets_inventory.alert_lead_seconds IS 'P2 alert lead before the rotation deadline; bounded by ttl_seconds.';
COMMENT ON COLUMN secrets_inventory.dr_critical        IS 'Item 5: secret requires a tested secondary-region copy for the quarterly DR drill.';
COMMENT ON COLUMN secrets_inventory.break_glass_path   IS 'Emergency path reference; uses delegate to Task 7.3.12 break-glass grants with mandatory post-review.';

CREATE INDEX secrets_inventory_class_ix    ON secrets_inventory (secret_class);
CREATE INDEX secrets_inventory_rotation_ix ON secrets_inventory (last_rotated_at);
CREATE INDEX secrets_inventory_dr_ix       ON secrets_inventory (dr_critical) WHERE dr_critical;

COMMIT;
