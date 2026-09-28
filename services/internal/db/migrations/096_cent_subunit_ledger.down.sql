-- 096_cent_subunit_ledger.down.sql — revert the §5.41 convention comments.
-- The migration creates no tables; the down script only clears the
-- documentation comments it installed.

BEGIN;

COMMENT ON TABLE balances IS 'Spec §5.3 derived wallet cache.';
COMMENT ON COLUMN balances.available IS NULL;
COMMENT ON COLUMN balances.locked IS NULL;
COMMENT ON COLUMN balances.total IS NULL;
COMMENT ON TABLE ledger_lines IS 'GL journal lines (spec §5.21).';
COMMENT ON COLUMN ledger_lines.debit_amount IS NULL;
COMMENT ON COLUMN ledger_lines.credit_amount IS NULL;

COMMIT;
