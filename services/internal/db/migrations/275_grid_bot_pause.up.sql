-- 275_grid_bot_pause.up.sql
-- Phase-10 Task 10.3.26 (engine Phase-16 Task 16.3.19) — grid-bot
-- pause/resume.
--
--   * grid_bot_status_enum gains 'PAUSED': a frozen-but-live state.
--     Pausing keeps working child orders on the book (real fills still
--     book) while the engine suspends counter placement and TP/SL
--     excursions; stopped_at/stop_reason stay untouched (a pause is not
--     a stop).
--   * PAUSED holds an R13 concurrency slot — the five-bot cap counts
--     RUNNING + PAUSED so pausing can never free a slot for a 6th bot.
--     Both live-bot partial indexes widen to match so the cap count and
--     live-bot lookups stay index-covered.
--
-- ALTER TYPE ... ADD VALUE runs outside the transaction per repo
-- convention (pre-PG12 runners cannot execute it inside a tx; the value
-- is not used by this migration itself).

ALTER TYPE grid_bot_status_enum ADD VALUE IF NOT EXISTS 'PAUSED';

BEGIN;

DROP INDEX grid_bots_running_ix;
CREATE INDEX grid_bots_running_ix ON grid_bots (account_id)
    WHERE status IN ('RUNNING', 'PAUSED');

DROP INDEX grid_bots_instrument_ix;
CREATE INDEX grid_bots_instrument_ix ON grid_bots (instrument_id)
    WHERE status IN ('RUNNING', 'PAUSED');

COMMIT;
