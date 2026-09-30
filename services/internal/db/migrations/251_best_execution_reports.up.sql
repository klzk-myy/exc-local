-- 251_best_execution_reports.up.sql
-- Phase-21 Task 21.3.19 — MiFID II RTS 27/28 best-execution quality &
-- venue reporting (spec §14.5, §24 #202; §27.1 TCA / RTS 28 matrix row).
--
--   rts27_daily_stats — the daily materialization (per instrument +
--                       day) the plan calls "daily materialized views".
--                       DEVIATION (recorded for §27): a ClickHouse MV
--                       cannot express this rollup — the metrics join
--                       `trades` (fills/prices/aggressor) with
--                       `tca_results` (arrival/VWAP/fix slippage) and
--                       `volume_stats` (order counters) and with the
--                       PostgreSQL orders ledger (order→fill latency),
--                       and CH materialized views are single-source
--                       INSERT triggers. The daily materialization is
--                       therefore computed in Go from ClickHouse
--                       `trades`/`tca_results`/`volume_stats` and
--                       persisted here — idempotent on
--                       (instrument_id, day) upsert, so re-runs repair
--                       rather than duplicate. gaps[] names every
--                       metric the sources cannot supply (never
--                       fabricated — §2.7).
--   rts27_reports     — quarterly per-instrument-class execution-quality
--                       publications (ESMA RTS 27). Versioned per
--                       (quarter_start, instrument_class); PUBLISHED
--                       rows back the public download surface.
--   rts28_reports     — annual top-5-venues publications per instrument
--                       class with per-client-category breakdowns
--                       (ESMA RTS 28). venues JSONB carries the ranked
--                       rows; categories carries the per-category
--                       sections.
--
-- Retention (§19.12 / MiFID II): >= 5 years; PUBLISHED artifacts are
-- retained indefinitely as the public-record evidence.

BEGIN;

CREATE TABLE rts27_daily_stats (
    id                  BIGSERIAL    PRIMARY KEY,
    day                 DATE         NOT NULL,           -- UTC trading day
    instrument_id       BIGINT       NOT NULL,
    symbol              VARCHAR(32)  NOT NULL,
    product_type        VARCHAR(8)   NOT NULL,           -- SPOT|FORWARD|SWAP|NDF|OPTION
    pair_class          VARCHAR(12)  NOT NULL,           -- FX_MAJOR|FX_MINOR|FX_EXOTIC
    fills               BIGINT       NOT NULL DEFAULT 0,
    volume_base         DECIMAL(38,8) NOT NULL DEFAULT 0,
    volume_quote        DECIMAL(38,8) NOT NULL DEFAULT 0,
    vwap                DECIMAL(38,8),
    price_min           DECIMAL(38,8),
    price_max           DECIMAL(38,8),
    price_median        DECIMAL(38,8),
    price_mean          DECIMAL(38,8),
    agg_buy_fills       BIGINT       NOT NULL DEFAULT 0,
    agg_sell_fills      BIGINT       NOT NULL DEFAULT 0,
    unknown_fills       BIGINT       NOT NULL DEFAULT 0,
    agg_buy_volume      DECIMAL(38,8) NOT NULL DEFAULT 0,
    agg_sell_volume     DECIMAL(38,8) NOT NULL DEFAULT 0,
    orders_submitted    BIGINT,                          -- NULL = no counter
    orders_filled       BIGINT,                          --   data that day
    fill_rate           DECIMAL(10,6),                   -- orders_filled/submitted
    median_order_to_fill_ms DECIMAL(20,3),               -- order→fill latency
                                                       -- (aggressor side, PG ledger)
    tca_fills           BIGINT       NOT NULL DEFAULT 0, -- fills with TCA rows
    slip_arrival_avg_bps  DECIMAL(20,6),
    slip_arrival_med_bps  DECIMAL(20,6),
    slip_vwap_avg_bps     DECIMAL(20,6),
    improvement_avg       DECIMAL(38,8),
    spread_avg_bps        DECIMAL(20,6),                 -- NULL: no persisted
                                                       -- quote stream (gap)
    inputs_complete     BOOLEAN      NOT NULL DEFAULT true,
    gaps                JSONB        NOT NULL DEFAULT '[]'::jsonb,
    created_at          TIMESTAMPTZ  NOT NULL DEFAULT now(),
    updated_at          TIMESTAMPTZ  NOT NULL DEFAULT now(),
    UNIQUE (instrument_id, day)
);
CREATE INDEX rts27_daily_stats_day_ix ON rts27_daily_stats (day, instrument_id);
COMMENT ON TABLE rts27_daily_stats IS
    'Task 21.3.19 daily execution-quality materialization (spec §14.5, '
    '§24 #202): one row per instrument per UTC day computed from '
    'ClickHouse trades/tca_results/volume_stats plus the PG orders '
    'ledger. gaps[] lists metrics the sources cannot supply — NULL '
    'columns are honest gaps, never fabricated values.';

CREATE TABLE rts27_reports (
    id               BIGSERIAL    PRIMARY KEY,
    quarter_start    DATE         NOT NULL,              -- UTC quarter open
    instrument_class VARCHAR(24)  NOT NULL,              -- 'PRODUCT:PAIR_CLASS'
    version          INTEGER      NOT NULL DEFAULT 1,
    status           VARCHAR(12)  NOT NULL DEFAULT 'DRAFT'
                     CHECK (status IN ('DRAFT','PUBLISHED')),
    metrics          JSONB        NOT NULL,              -- ESMA RTS 27
                     -- field set per instrument class
    csv              TEXT         NOT NULL,              -- publication artifact
    days_covered     INTEGER      NOT NULL DEFAULT 0,
    zero_activity    BOOLEAN      NOT NULL DEFAULT false,
    generated_by     BIGINT       NOT NULL,
    created_at       TIMESTAMPTZ  NOT NULL DEFAULT now(),
    published_by     BIGINT,
    published_at     TIMESTAMPTZ,
    UNIQUE (quarter_start, instrument_class, version)
);
CREATE INDEX rts27_reports_pub_ix
    ON rts27_reports (status, quarter_start DESC);
COMMENT ON TABLE rts27_reports IS
    'Task 21.3.19 quarterly RTS 27 venue execution-quality publications '
    '(spec §14.5, §24 #202): versioned per (quarter, instrument_class); '
    'PUBLISHED rows serve the unauthenticated public download surface.';

CREATE TABLE rts28_reports (
    id               BIGSERIAL    PRIMARY KEY,
    year             INTEGER      NOT NULL,
    instrument_class VARCHAR(24)  NOT NULL,              -- 'PRODUCT:PAIR_CLASS'
    version          INTEGER      NOT NULL DEFAULT 1,
    status           VARCHAR(12)  NOT NULL DEFAULT 'DRAFT'
                     CHECK (status IN ('DRAFT','PUBLISHED')),
    categories       JSONB        NOT NULL,              -- client_category →
                     -- top-5 venue table (ESMA RTS 28 tables 1/2)
    qualitative_assessment TEXT,                          -- officer-authored on publish
    csv              TEXT         NOT NULL,
    generated_by     BIGINT       NOT NULL,
    created_at       TIMESTAMPTZ  NOT NULL DEFAULT now(),
    published_by     BIGINT,
    published_at     TIMESTAMPTZ,
    UNIQUE (year, instrument_class, version)
);
CREATE INDEX rts28_reports_pub_ix
    ON rts28_reports (status, year DESC);
COMMENT ON TABLE rts28_reports IS
    'Task 21.3.19 annual RTS 28 top-5-venues publications (spec §14.5, '
    '§24 #202): per instrument class with RETAIL/PROFESSIONAL category '
    'breakdowns; PUBLISHED rows serve the public download surface.';

COMMIT;
