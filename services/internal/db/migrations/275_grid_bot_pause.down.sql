-- 275_grid_bot_pause.down.sql — Phase-10 Task 10.3.26 rollback.
--
-- NOTE: 'PAUSED' cannot be removed from grid_bot_status_enum once added —
-- PostgreSQL has no DROP VALUE for enum types (documented one-way
-- change, same convention as 238_demo_accounts). The down narrows the
-- live-bot partial indexes back to RUNNING-only; any PAUSED rows should
-- be resumed or stopped first — the enum value itself stays valid.

BEGIN;

DROP INDEX grid_bots_running_ix;
CREATE INDEX grid_bots_running_ix ON grid_bots (account_id)
    WHERE status = 'RUNNING';

DROP INDEX grid_bots_instrument_ix;
CREATE INDEX grid_bots_instrument_ix ON grid_bots (instrument_id)
    WHERE status = 'RUNNING';

COMMIT;
