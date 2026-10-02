-- 278_accounts_test_scoped.up.sql
-- Reconciliation must never escalate integration-test artifacts into
-- trading halts: harness-created accounts (chaos/NBP/account-lifecycle/
-- GDPR fixtures) carry deliberately inconsistent state — positions with
-- no fill projection, balances with no ledger rows — and recon kept
-- translating that drift into GLOBAL halts in the dev environment.
--
-- test_scoped is the durable classifier: test fixtures stamp it at
-- creation; the backfill marks the known harness email patterns used by
-- the integration/pentest suites. Recon legs exclude test-scoped rows —
-- the accounts stay halted/suspended as recorded, they just stop being
-- re-flagged as production drift.

BEGIN;

ALTER TABLE accounts
    ADD COLUMN test_scoped BOOLEAN NOT NULL DEFAULT FALSE;

UPDATE accounts a
   SET test_scoped = TRUE
  FROM users u
 WHERE a.user_id = u.id
   AND (u.email LIKE 'acct\_it\_%'
     OR u.email LIKE 'nbp-itest-%'
     OR u.email LIKE 'chaos\_%'
     OR u.email LIKE 't737-%'
     OR u.email LIKE 'gdpr\_it\_%');

CREATE INDEX idx_accounts_test_scoped ON accounts (id) WHERE test_scoped;

COMMIT;
