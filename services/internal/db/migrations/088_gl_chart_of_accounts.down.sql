-- 088_gl_chart_of_accounts.down.sql — reverse spec §5.21a additions.
-- CoA rows seeded by 088 are removed only where no ledger_lines reference
-- them (the 036 subset rows are preserved).

BEGIN;

DROP TABLE IF EXISTS swap_accrual_records;
DROP TYPE IF EXISTS swap_side_enum;
DROP TABLE IF EXISTS non_trading_fee_schedule;
DROP TYPE IF EXISTS non_trading_fee_status_enum;
DROP TYPE IF EXISTS non_trading_fee_unit_enum;
DROP TYPE IF EXISTS non_trading_fee_kind_enum;
DROP TABLE IF EXISTS swap_markup_policies;
DROP TYPE IF EXISTS swap_markup_status_enum;
DROP TABLE IF EXISTS currency_day_counts;
DROP TYPE IF EXISTS day_count_enum;

-- Remove the 088-only chart rows (keep the four 036 seed prefixes).
DELETE FROM chart_of_accounts a
WHERE NOT EXISTS (SELECT 1 FROM ledger_lines l WHERE l.account_code = a.account_code)
  AND a.account_code !~ '^(1010_NOSTRO|2010_CUSTOMER_LIABILITY|4010_TRADING_FEE_REVENUE|5010_LIQUIDATION_PENALTY)_';

COMMIT;
