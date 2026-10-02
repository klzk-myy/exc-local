-- 281_processed_trades_raw_frame.up.sql
-- Phase-24 follow-up — durable republish source for the shm-only
-- topology (spec §5.3 fill-commit → event fan-out seam).
--
-- The fill consumer commits the settlement transaction and then
-- republishes the raw Event frame to JetStream from memory. A crash in
-- the commit→publish window leaves the trade settled but absent from
-- trades.*/settlements.* — analytics/klines/CH starve silently and no
-- replay source exists (the shm ring frame was already consumed).
--
-- raw_frame persists the verbatim Event frame inside the same
-- transaction as the dedup row, so a boot-time backlog repair can
-- re-emit it under the bridge's Nats-Msg-Id dedup key. NULL for
-- pre-migration rows and non-wire fills.

BEGIN;

ALTER TABLE processed_trades
    ADD COLUMN raw_frame BYTEA;

-- Boot-time backlog repair: fills committed within the restart window
-- whose frames may never have reached JetStream.
CREATE INDEX ix_processed_trades_backlog
    ON processed_trades (shard_id, processed_at)
    WHERE raw_frame IS NOT NULL;

COMMIT;
