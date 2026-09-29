-- 046_fix_sessions_entitlement.up.sql
-- Phase-18 Task 18.3.9 — FIX session entitlement, cancel-on-disconnect
-- and per-session inbound throttling (spec §5.20, §9.3, §24 #135–137,
-- #153).
--
-- account_id           — order-entry binding; NULL = drop-copy
--                        (read-only) session per spec §5.20/§9.3.
-- api_key_id           — logon credential binding (Task 18.3.1 item 9:
--                        "SenderCompID + API-key auth on Logon"). The
--                        client authenticates with Username(553)=api
--                        key_id and Password(554)=secret, verified
--                        against api_keys.key_hash. Lives with the
--                        entitlement columns because it binds WHO may
--                        open the session — the row is meaningless for
--                        auth until this lands.
-- allowed_instruments  — spec §5.20 VARCHAR(512): NULL = all
--                        instruments; else comma-separated canonical
--                        symbols (the plan's "array" wording is
--                        superseded — spec column type wins).
-- cancel_on_disconnect — §9.3: abnormal disconnect / logon timeout
--                        mass-cancels the session's resting orders.
-- max_msgs_per_sec     — §9.3 inbound throttle (default 100); excess
--                        rejected SESSION_THROTTLED, never dropped.

BEGIN;

ALTER TABLE fix_sessions
    ADD COLUMN account_id          BIGINT REFERENCES accounts (id),
    ADD COLUMN api_key_id          BIGINT REFERENCES api_keys (id),
    ADD COLUMN allowed_instruments VARCHAR(512),
    ADD COLUMN cancel_on_disconnect BOOLEAN NOT NULL DEFAULT TRUE,
    ADD COLUMN max_msgs_per_sec    INTEGER NOT NULL DEFAULT 100
        CHECK (max_msgs_per_sec > 0);

-- Order-entry sessions must bind an account OR a credential-less
-- read-only intent is impossible: every live session row carries
-- api_key_id (unknown credentials can never satisfy a logon check
-- against a row that has none — fail closed). Enforced at write time
-- by the fix session admin service; the column stays nullable so
-- pre-provisioned rows can be staged before key issuance.
CREATE INDEX idx_fix_sessions_account
    ON fix_sessions (account_id)
    WHERE account_id IS NOT NULL;

COMMIT;
