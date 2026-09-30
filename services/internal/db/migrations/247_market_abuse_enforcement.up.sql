-- 247_market_abuse_enforcement.up.sql
-- Phase-21 Task 21.3.8 — Market-Abuse Enforcement (spec §14.9.1–14.9.2).
--
-- enforcement_actions: the action ledger — every WARN / THROTTLE /
-- RESTRICT / SUSPEND / DISMISS taken on a surveillance signal lands
-- here (auto and manual). UNIQUE (signal_id, action) keeps retries and
-- the duplicate NATS delivery path idempotent — the second attempt is
-- a no-op rather than a duplicate freeze/throttle.

BEGIN;

CREATE TABLE enforcement_actions (
    id          BIGSERIAL    PRIMARY KEY,
    action_id   VARCHAR(40)  NOT NULL UNIQUE,           -- "enf_<26urlsafe>"
    signal_id   BIGINT       REFERENCES surveillance_signals (id),
    case_id     BIGINT,                                 -- set when taken from a surveillance case
    account_id  BIGINT       REFERENCES accounts (id),  -- NULL while the L3 hash is unresolvable
    account_hash BIGINT      NOT NULL DEFAULT 0,        -- marketdata.L3AccountHash pseudonym from the signal
    action      VARCHAR(12)  NOT NULL CHECK (action IN (
                     'WARN','THROTTLE','RESTRICT','SUSPEND','DISMISS')),
    source      VARCHAR(8)   NOT NULL DEFAULT 'MANUAL' CHECK (source IN ('AUTO','MANUAL')),
    params      JSONB        NOT NULL DEFAULT '{}',     -- throttle max_msgs_per_sec etc.
    status      VARCHAR(12)  NOT NULL DEFAULT 'ACTIVE' CHECK (status IN (
                     'ACTIVE','EXPIRED','REVOKED','DISMISSED')),
    actor_id    BIGINT       NOT NULL,                  -- officer id (machine acts as system id)
    note        TEXT         NOT NULL DEFAULT '',
    expires_at  TIMESTAMPTZ,                            -- THROTTLE/RESTRICT horizon
    created_at  TIMESTAMPTZ  NOT NULL DEFAULT now(),
    updated_at  TIMESTAMPTZ  NOT NULL DEFAULT now()
);

CREATE UNIQUE INDEX idx_enforcement_actions_dedup
    ON enforcement_actions (signal_id, action)
    WHERE signal_id IS NOT NULL;
CREATE INDEX idx_enforcement_actions_account
    ON enforcement_actions (account_id, status, created_at DESC);
CREATE INDEX idx_enforcement_actions_case
    ON enforcement_actions (case_id) WHERE case_id IS NOT NULL;

COMMENT ON TABLE enforcement_actions IS
    'Phase-21 Task 21.3.8 — market-abuse enforcement ledger. Actions '
    'materialise through existing seams: WARN → advisory row + notify; '
    'THROTTLE → Redis admission cap keyed on account id; RESTRICT → '
    'SCOPE_ACCOUNT kill-switch suspension; SUSPEND → compliance hold '
    '(holdSvc — cancels resting orders, freezes account).';

COMMIT;
