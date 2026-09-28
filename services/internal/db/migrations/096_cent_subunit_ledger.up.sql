-- 096_cent_subunit_ledger.up.sql
-- Phase-03 Task 3.3.21 — Cent-Denominated Sub-Unit Ledger Accounting
-- (spec §5.41, §24 #370). CONVENTION MIGRATION: intentionally creates no
-- tables. It records the minor-unit posting convention as schema comments
-- so the rule is versioned with the DDL it governs.
--
-- Convention (spec §5.41.2):
--   * accounts whose product profile (account_product_profiles, migration
--     095 — Phase-14 Task 14.3.13) carries subunit_divisor = 100 (seed code
--     'CENT') store balances.available / balances.locked /
--     balances.total / ledger_lines.debit_amount / ledger_lines.credit_amount
--     in MINOR units (cents). STANDARD profiles (subunit_divisor = 1)
--     continue in major units. The two never mix within an account.
--   * The per-currency zero-sum invariant (SUM(debits) == SUM(credits),
--     Task 3.3.18 / gl_journal_zero_sum_chk) is enforced in minor units —
--     it is unit-agnostic by construction and needs no schema change.
--   * STANDARD↔CENT profile switching is permitted only when every balance
--     row for the account is zero (else INVALID_REQUEST); dust-convert
--     (Task 3.3.20) is the pre-switch path for stranded minors.
--   * Every read boundary divides stored amounts by the account's profile
--     divisor through the single ledger.DisplayAmount helper
--     (services/internal/ledger/subunit.go) — no per-consumer conversion
--     logic. The CoA (Task 3.3.19) is unchanged: minor-unit postings
--     resolve to the same chart_of_accounts codes.

BEGIN;

COMMENT ON TABLE balances IS
    'Spec §5.3 derived wallet cache. Amounts are major units for STANDARD '
    'profiles and MINOR units (×subunit_divisor, cents) for CENT profiles '
    '(spec §5.41, migration 096 convention). Readers divide by the account '
    'profile divisor via ledger.DisplayAmount — never assume units.';

COMMENT ON COLUMN balances.available IS
    'Major units (STANDARD) or minor units (CENT, subunit_divisor=100) '
    'per spec §5.41 — account-consistent, never mixed.';

COMMENT ON COLUMN balances.locked IS
    'Same unit convention as balances.available (spec §5.41).';

COMMENT ON COLUMN balances.total IS
    'GENERATED available+locked — inherits the account profile unit '
    'convention (spec §5.41).';

COMMENT ON TABLE ledger_lines IS
    'GL journal lines (spec §5.21). debit_amount/credit_amount are stored '
    'in the posting account''s profile units: minor units (cents) for CENT '
    'profiles (subunit_divisor=100), major units otherwise (spec §5.41, '
    'migration 096). The per-currency SUM(debits)==SUM(credits) invariant '
    '(gl_journal_zero_sum_chk) holds in minor units.';

COMMENT ON COLUMN ledger_lines.debit_amount IS
    'Posting-account profile units — minor units for CENT profiles '
    '(spec §5.41).';

COMMENT ON COLUMN ledger_lines.credit_amount IS
    'Posting-account profile units — minor units for CENT profiles '
    '(spec §5.41).';

COMMIT;
