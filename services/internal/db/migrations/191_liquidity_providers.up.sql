-- 191_liquidity_providers.up.sql
-- Phase-07 Task 7.3.9 — Liquidity Provider (LP) management module
-- (spec §5.44 item 3, §24 #227). Numbered 191: neither the task text nor
-- spec §5.44 cite a migration number for the LP tables — 191 is the next
-- free slot after 190.
--
-- liquidity_providers      — entity rows; scorecard JSONB is the last
--                            computed snapshot (spec §5.44 column). status
--                            lifecycle ONBOARDING→ACTIVE→SUSPENDED (task
--                            text; the spec's (ACTIVE|SUSPENDED) shorthand
--                            omits ONBOARDING — the task lifecycle governs).
-- lp_instrument_configs    — per-LP per-instrument pricing feed config:
--                            spread markup/skew applied before market-data
--                            distribution plus the per-instrument price
--                            staleness timeout (default 5s per spec).
-- lp_performance_alerts    — durable alert trail; threshold breaches are
--                            evaluated on scorecard refresh (fill_ratio <
--                            80% or availability < 95% over a 1h window)
--                            and dispatched to the Risk Manager dashboard
--                            via the AlertSink seam (ops.alerts). OPEN is
--                            deduped per (lp_id, metric).

BEGIN;

CREATE TABLE liquidity_providers (
    lp_id                BIGSERIAL    PRIMARY KEY,
    name                 VARCHAR(128) NOT NULL UNIQUE,
    status               VARCHAR(12)  NOT NULL DEFAULT 'ONBOARDING'
                         CHECK (status IN ('ONBOARDING','ACTIVE','SUSPENDED')),
    connection_type      VARCHAR(8)   NOT NULL
                         CHECK (connection_type IN ('FIX','REST','WS')),
    session_config       JSONB        NOT NULL DEFAULT '{}'::jsonb,  -- FIX session params (Phase-18 binds)
    contact              JSONB        NOT NULL DEFAULT '{}'::jsonb,  -- desk email/phone
    settlement_terms     JSONB        NOT NULL DEFAULT '{}'::jsonb,  -- nostro refs / settlement cycle
    fix_session_enabled  BOOLEAN      NOT NULL DEFAULT FALSE,        -- admin session gate (Task item 3)
    staleness_timeout_ms INT          NOT NULL DEFAULT 5000
                         CHECK (staleness_timeout_ms > 0),           -- spec §6.5 staleness gate 5s
    scorecard            JSONB        NOT NULL DEFAULT '{}'::jsonb,  -- last computed snapshot (§5.44)
    created_by           BIGINT,                                    -- admin user id
    created_at           TIMESTAMPTZ  NOT NULL DEFAULT now(),
    updated_at           TIMESTAMPTZ  NOT NULL DEFAULT now()
);

CREATE TABLE lp_instrument_configs (
    id                    BIGSERIAL    PRIMARY KEY,
    lp_id                 BIGINT       NOT NULL REFERENCES liquidity_providers (lp_id) ON DELETE CASCADE,
    instrument_id         BIGINT       NOT NULL REFERENCES instruments (id),
    enabled               BOOLEAN      NOT NULL DEFAULT TRUE,
    spread_markup_bid_bps NUMERIC(12,4) NOT NULL DEFAULT 0,   -- added to LP bid before distribution
    spread_markup_ask_bps NUMERIC(12,4) NOT NULL DEFAULT 0,   -- subtracted from LP ask (stored positive)
    skew_bps              NUMERIC(12,4) NOT NULL DEFAULT 0,   -- inventory skew shift (signed)
    staleness_timeout_ms  INT          NOT NULL DEFAULT 5000
                          CHECK (staleness_timeout_ms > 0),
    created_at            TIMESTAMPTZ  NOT NULL DEFAULT now(),
    updated_at            TIMESTAMPTZ  NOT NULL DEFAULT now(),
    UNIQUE (lp_id, instrument_id)
);

CREATE TABLE lp_performance_alerts (
    id          BIGSERIAL    PRIMARY KEY,
    lp_id       BIGINT       REFERENCES liquidity_providers (lp_id) ON DELETE CASCADE,
                                     -- NULL = venue-level alert (all_lps_down)
    metric      VARCHAR(32)  NOT NULL,   -- fill_ratio | availability_pct | all_lps_down | ...
    observed    NUMERIC(14,6) NOT NULL,
    threshold   NUMERIC(14,6) NOT NULL,
    eval_window VARCHAR(16)  NOT NULL DEFAULT '1h',     -- 'window' is reserved
    status      VARCHAR(12)  NOT NULL DEFAULT 'OPEN'
                CHECK (status IN ('OPEN','ACKED','RESOLVED')),
    emitted_at  TIMESTAMPTZ  NOT NULL DEFAULT now(),
    acked_by    BIGINT,
    acked_at    TIMESTAMPTZ
);

CREATE INDEX idx_lp_instrument_configs_lp
    ON lp_instrument_configs (lp_id);
CREATE INDEX idx_lp_performance_alerts_lp
    ON lp_performance_alerts (lp_id, emitted_at DESC);
CREATE INDEX idx_lp_performance_alerts_open
    ON lp_performance_alerts (status) WHERE status = 'OPEN';

COMMIT;
