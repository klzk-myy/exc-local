-- 238_demo_accounts.up.sql
-- Phase-08.5 Task 8.5.3.2 — demo / paper trading environment
-- (§24 #266; ESMA/FCA/ASIC practice-account mandate for retail FX).
--
--   * account_type_enum gains 'DEMO' — the demo deployment
--     (environment label "demo", demo-api./demo-ws.{domain}) issues
--     DEMO accounts at registration instead of SPOT. DEMO rows carry
--     virtual balances only and are hard-rejected by the funding rail
--     seam (internal/demo WrapFundingChecker) — real banking is
--     unreachable from a demo account.
--   * accounts.demo_expires_at is the precomputed inactivity deadline
--     (last activity + 30 days): provisioned at registration and bumped
--     by the auth login/refresh path, so the daily expiry sweep is a
--     pure indexed read. login_history was rejected as the activity
--     signal — it is user-scoped and misses API-key/FIX-only activity;
--     a refreshed deadline column covers every authenticated surface.
--   * The partial index serves ONLY the sweep predicate
--     (account_type='DEMO', status<>'CLOSED', deadline lapsed) — the
--     accounts PK is untouched and non-demo rows never enter it.

ALTER TYPE account_type_enum ADD VALUE IF NOT EXISTS 'DEMO';

BEGIN;

ALTER TABLE accounts ADD COLUMN IF NOT EXISTS demo_expires_at TIMESTAMPTZ;

CREATE INDEX IF NOT EXISTS idx_accounts_demo_expiry
    ON accounts (demo_expires_at)
    WHERE account_type = 'DEMO' AND status <> 'CLOSED';

COMMIT;
