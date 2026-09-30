-- 271_lp_accounts.up.sql
-- Phase-11 Task 11.3.12 — LP↔account binding for the SCOPE_LP
-- kill-switch (spec §7.2b, §24 #409).
--
-- The scoped LP kill-switch (killswitch.go ScopeLP / `halt:lp:{target}`)
-- targets a liquidity_providers entity, but FIX Mass Quote ingress
-- (fix/quoting.go SubmitMassQuote) only knows the session's account.
-- This table is the minimal mapping hop: account → owning LP entity.
--
-- Contract:
--   * SCOPE_LP target_id is the liquidity_providers.lp_id in string
--     form — consistent with COUNTERPARTY targeting accounts.id and
--     with the durable record written by KillSwitchService.Set.
--   * An account binds to AT MOST one LP (PRIMARY KEY account_id) —
--     quoting-order attribution would otherwise be ambiguous.
--   * An LP may bind many quoting accounts (lp_id index for the
--     LP→accounts direction used by the admin LP surface).
--   * Accounts without a row are NOT LP-bound: the LP kill-switch does
--     not apply to their quote flow (mm_programs entitlement remains
--     the quoting admission gate).

BEGIN;

CREATE TABLE lp_accounts (
    account_id BIGINT PRIMARY KEY REFERENCES accounts (id) ON DELETE CASCADE,
    lp_id      BIGINT NOT NULL REFERENCES liquidity_providers (lp_id) ON DELETE CASCADE,
    created_by BIGINT,                                    -- admin user id (audit)
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX lp_accounts_lp_idx ON lp_accounts (lp_id);

COMMIT;
