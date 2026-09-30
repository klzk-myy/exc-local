-- 237_depreciation_episodes.up.sql
-- Phase-20 Task 20.3.15 — Leveraged-Position Depreciation Notifications
-- (spec §16.9, §24 #375; MiFID II retail 10% depreciation rule).
--
-- Durable per-position episode state for the 10%-rule notifier. The
-- notifications dedupe path (notification_deliveries / RecentDeliveries)
-- cannot carry this state honestly:
--
--   * RecentDeliveries is per-USER and capped at the latest 500 rows —
--     an episode that outlives 500 intervening deliveries would lose its
--     HWM and re-notify inside a live episode (duplicate statutory
--     notice) or miss a new-episode re-notify.
--   * The "recovery above -5% resets the episode" rule needs mutable
--     episode state; the delivery log is append-only and cannot express
--     "episode ended" without emitting noise to the client.
--   * Fail-closed (spec §2.7): a dedupe store that silently swallows a
--     statutory same-business-day notice is an L1 defect.
--
-- Lifecycle: one OPEN episode per position while it is in drawdown past
-- a notified threshold (deepest_notified_pct tracks the high-water mark
-- — the most negative 10% multiple already notified). Recovery above
-- -5% marks the episode RESET; position close marks it CLOSED; a later
-- re-crossing opens episode_seq+1 and notifies its thresholds again.

BEGIN;

CREATE TABLE depreciation_episodes (
    id                    BIGSERIAL     PRIMARY KEY,
    position_id           BIGINT        NOT NULL,   -- positions.id; NO FK —
                                        -- the audit row must outlive a
                                        -- deleted/reset positions row
    account_id            BIGINT        NOT NULL,
    instrument_id         BIGINT        NOT NULL,
    episode_seq           INTEGER       NOT NULL,   -- 1,2,3… per position
    deepest_notified_pct  DECIMAL(9,4)  NOT NULL DEFAULT 0,
                                        -- most negative threshold already
                                        -- notified this episode (-10, -20…)
    last_dep_pct          DECIMAL(9,4)  NOT NULL,   -- depreciation % at last eval
    last_mark             DECIMAL(28,8),            -- mark at last eval
    last_quantity         DECIMAL(28,8),            -- qty at last eval (pre-close)
    entry_price           DECIMAL(28,8) NOT NULL,
    currency              VARCHAR(3)    NOT NULL,   -- instrument quote ccy
    status                VARCHAR(8)    NOT NULL DEFAULT 'OPEN'
                          CHECK (status IN ('OPEN','RESET','CLOSED')),
    started_at            TIMESTAMPTZ   NOT NULL,
    updated_at            TIMESTAMPTZ   NOT NULL DEFAULT now(),
    ended_at              TIMESTAMPTZ,
    UNIQUE (position_id, episode_seq)
);

-- One live episode per position.
CREATE UNIQUE INDEX depreciation_episodes_open_ux
    ON depreciation_episodes (position_id) WHERE status = 'OPEN';

-- Sweep support: which positions have ever had an episode.
CREATE INDEX depreciation_episodes_acct_ix
    ON depreciation_episodes (account_id, position_id);

COMMIT;
