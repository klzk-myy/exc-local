-- 086_vip_tiers.up.sql
-- Phase-03 Task 3.3.16 — VIP 0–9 tier schedule & daily recalculation engine
-- (spec §8.5, §24 #290). Three artefacts:
--   1. accounts.vip_tier — the account's currently effective VIP tier.
--   2. vip_tier_schedule — the transparent qualification matrix
--      (30-day trailing USD volume OR 30-day average equity, whichever
--      qualifies higher). maker_bps may be negative for VIP 4+ (Task 3.3.17
--      negative maker fee rebates).
--   3. account_vip_history — append-only audit of every recalculation.
-- account_equity_snapshots backs the 30-day average-equity input: the
-- daily 00:00 UTC job upserts one row per master account per day.

BEGIN;

ALTER TABLE accounts
    ADD COLUMN vip_tier SMALLINT NOT NULL DEFAULT 0
        CHECK (vip_tier BETWEEN 0 AND 9);

CREATE TABLE vip_tier_schedule (
    vip_tier               SMALLINT PRIMARY KEY CHECK (vip_tier BETWEEN 0 AND 9),
    min_30d_volume_usd     DECIMAL(28,8) NOT NULL DEFAULT 0,   -- trailing 30d notional, USD equiv
    min_30d_avg_equity_usd DECIMAL(28,8) NOT NULL DEFAULT 0,   -- 30d avg equity, USD equiv
    maker_bps              DECIMAL(8,4)  NOT NULL,             -- negative = rebate (VIP 4+)
    taker_bps              DECIMAL(8,4)  NOT NULL,
    updated_at             TIMESTAMPTZ   NOT NULL DEFAULT now()
);

-- Default qualification matrix (volume OR equity qualifies). Institutional
-- FX volume ladder; maker rebates begin at VIP 4 per Task 3.3.17.
INSERT INTO vip_tier_schedule
    (vip_tier, min_30d_volume_usd, min_30d_avg_equity_usd, maker_bps, taker_bps)
VALUES
    (0,             0,          0,  1.0000, 1.5000),
    (1,     1000000,      25000,  0.9000, 1.4000),
    (2,     5000000,     100000,  0.8000, 1.3000),
    (3,    25000000,     250000,  0.6000, 1.2000),
    (4,   100000000,    1000000, -0.0500, 1.0000),
    (5,   250000000,    2500000, -0.1000, 0.9000),
    (6,   500000000,    5000000, -0.1500, 0.8000),
    (7,  1000000000,   10000000, -0.2000, 0.7000),
    (8,  2500000000,   25000000, -0.3000, 0.6000),
    (9,  5000000000,   50000000, -0.5000, 0.5000);

CREATE TABLE account_equity_snapshots (
    account_id    BIGINT         NOT NULL REFERENCES accounts (id),
    snapshot_date DATE           NOT NULL,                -- UTC calendar day
    equity_usd    DECIMAL(28,8)  NOT NULL,
    created_at    TIMESTAMPTZ    NOT NULL DEFAULT now(),
    PRIMARY KEY (account_id, snapshot_date)
);

CREATE TABLE account_vip_history (
    id                 BIGSERIAL PRIMARY KEY,
    account_id         BIGINT        NOT NULL REFERENCES accounts (id),
    previous_tier      SMALLINT,                              -- NULL on first calc
    vip_tier           SMALLINT      NOT NULL CHECK (vip_tier BETWEEN 0 AND 9),
    volume_30d_usd     DECIMAL(28,8) NOT NULL,
    avg_equity_30d_usd DECIMAL(28,8) NOT NULL,
    source             VARCHAR(16)   NOT NULL DEFAULT 'DAILY_JOB',
    calculated_at      TIMESTAMPTZ   NOT NULL DEFAULT now()
);

CREATE INDEX idx_account_vip_history_account
    ON account_vip_history (account_id, calculated_at DESC);

COMMIT;
