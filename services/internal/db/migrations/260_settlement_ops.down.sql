-- 260_settlement_ops.down.sql — drop Task 24.3.6/.7/.14/.19 schema.
-- (renumbered from the 258 planning designation — 258 was taken by a
-- sibling task before this file landed).
--
-- NOTE: 'VOID' on settlement_status_enum cannot be removed without
-- recreating the type — enum values are append-only in PostgreSQL; the
-- value is left in place (harmless: no leg can reach VOID once the
-- tables below are gone).

BEGIN;

DROP TABLE IF EXISTS lp_default_events;
DROP TABLE IF EXISTS fx_fail_closeouts;
DROP TABLE IF EXISTS cls_payin_events;
DROP TABLE IF EXISTS herstatt_exposures;
DROP TABLE IF EXISTS rail_failover_queue;
DROP TABLE IF EXISTS rail_cutoff_matrix;
DROP TABLE IF EXISTS nostro_funding_thresholds;
DROP TABLE IF EXISTS recon_tolerances;
DROP TABLE IF EXISTS settlement_write_offs;
DROP TABLE IF EXISTS pb_credit_restitutions;
DROP TABLE IF EXISTS pb_giveup_collateral_moves;
DROP TABLE IF EXISTS pb_recon_events;
DROP TABLE IF EXISTS pb_recon_breaks;
DROP TABLE IF EXISTS pb_recon_runs;
DROP TABLE IF EXISTS settlement_exception_events;
DROP TABLE IF EXISTS settlement_exceptions;

DELETE FROM chart_of_accounts
 WHERE account_code ~ '^(5990_SETTLEMENT_WRITE_OFF|4600_FAIL_INTEREST_REVENUE|1020_SETTLEMENT_FAIL_CLAIM|1090_SETTLEMENT_FAIL_MEMO|2090_PB_CREDIT_MEMO)_';

COMMIT;
