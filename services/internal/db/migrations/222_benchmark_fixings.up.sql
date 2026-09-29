-- 222_benchmark_fixings.up.sql
-- Phase-15 Task 15.3.13 (spec §6.4, §7.1, §24 #401) — benchmark fixing
-- execution records.
--
-- The fixing scheduler fires at the three benchmark times (WM/R London
-- 4PM, ECB 14:15 CET reference rate, Tokyo 09:55 JST) and must "record
-- the fixing rate and execution timestamp". No existing table covers
-- per-occurrence fixing evidence — auction_calendar holds the schedule
-- (087) and orders carries the FIXING orders themselves (005 enum) —
-- so this dedicated record table is genuinely new.
--
-- rate is NULL when the fixing could not be priced: the Phase-19.5
-- Price Oracle seam (instruments.PriceSource) is absent this phase, so
-- the honest state is SKIPPED with skip_reason populated — never a
-- fabricated rate (fail-closed, spec §2.7).

BEGIN;

CREATE TABLE benchmark_fixings (
    id            BIGSERIAL    PRIMARY KEY,
    instrument_id BIGINT       NOT NULL REFERENCES instruments (id),
    symbol        VARCHAR(32)  NOT NULL,
    benchmark     VARCHAR(32)  NOT NULL
                  CHECK (benchmark IN ('WM_LONDON_4PM','ECB_REF_1415','TOKYO_0955')),
    scheduled_at  TIMESTAMPTZ  NOT NULL,   -- holiday-resolved fixing instant (UTC)
    fired_at      TIMESTAMPTZ  NOT NULL,   -- when the scheduler fired (UTC)
    rate          DECIMAL(20,8),           -- NULL when skipped/failed
    rate_source   VARCHAR(64)  NOT NULL DEFAULT '',  -- oracle feed id (Phase-19.5)
    status        VARCHAR(12)  NOT NULL
                  CHECK (status IN ('RECORDED','SKIPPED','FAILED')),
    skip_reason   TEXT         NOT NULL DEFAULT '',
    order_ids     JSONB        NOT NULL DEFAULT '[]'::jsonb, -- FIXING orders queued for injection
    created_at    TIMESTAMPTZ  NOT NULL DEFAULT now(),
    UNIQUE (instrument_id, benchmark, scheduled_at)
);
CREATE INDEX benchmark_fixings_symbol_time_idx
    ON benchmark_fixings (symbol, scheduled_at DESC);

COMMIT;
