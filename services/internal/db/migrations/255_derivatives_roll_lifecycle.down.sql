-- 255_derivatives_roll_lifecycle.down.sql — drop the Phase-22
-- roll/lifecycle stores (Tasks 22.3.8 + 22.3.10). The 'ROLLED' enum value
-- stays: Postgres cannot drop an enum value; harmless residue (the same
-- discipline as sibling enum additions).

BEGIN;

DROP TABLE IF EXISTS option_expiry_runs;
DROP TABLE IF EXISTS option_exercise_prefs;
DROP TABLE IF EXISTS option_assignments;
DROP TABLE IF EXISTS option_premium_settlements;
DROP TABLE IF EXISTS option_positions;
DROP TABLE IF EXISTS auto_roll_config;
DROP TABLE IF EXISTS contract_rolls;

COMMIT;
