-- 078_withdrawal_whitelist_settings.up.sql
-- Spec §5.44 item 11 (registry attribution, remediation #35) — Phase-11
-- Task 11.3.10: per-account withdrawal whitelist mode with the 24-hour
-- beneficiary addition timelock and the account-scoped 24-hour
-- deactivation safety lock (§24 #391 / AC row 33).
--
-- Semantics:
--   * mode ALLOW_ALL       — any destination (subject to cooldown,
--                            unverified-destination hold and fiat caps).
--   * mode WHITELIST_ONLY  — withdrawals may only target a VERIFIED
--                            bank_accounts row (migration 040, §5.23)
--                            whose unlocked_at has lapsed.
--   * timelock_until       — re-enable latch: set to disable_time + 24h
--                            on WHITELIST_ONLY → ALLOW_ALL; enabling is
--                            refused while it is in the future
--                            (rate-limited re-enable — the documented
--                            unlock path is to wait out the latch; ops
--                            may not shorten it via the API).
--   * withdrawal_lock_until — the deactivation safety lock: all client
--                            withdrawals for THIS account are refused
--                            for 24h after whitelist-only mode is
--                            disabled. Account-scoped — never global
--                            (remediation #35 supersedes the prior
--                            platform-wide lock).
--
-- The task text's `whitelist_only_enabled BOOL` is carried as a stored
-- generated projection of mode so both the spec's enum column and the
-- task's boolean read agree by construction.

BEGIN;

CREATE TYPE withdrawal_whitelist_mode_enum AS ENUM (
    'ALLOW_ALL', 'WHITELIST_ONLY'
);

CREATE TABLE withdrawal_whitelist_settings (
    account_id              BIGINT PRIMARY KEY REFERENCES accounts(id),
    mode                    withdrawal_whitelist_mode_enum NOT NULL DEFAULT 'ALLOW_ALL',
    whitelist_only_enabled  BOOLEAN GENERATED ALWAYS AS (mode = 'WHITELIST_ONLY') STORED,
    timelock_until          TIMESTAMPTZ,                     -- re-enable latch (24h after disable)
    withdrawal_lock_until   TIMESTAMPTZ,                     -- account-scoped 24h egress lock on disable
    updated_by              BIGINT,                          -- users.id of the actor
    updated_at              TIMESTAMPTZ NOT NULL DEFAULT now()
);

COMMIT;
