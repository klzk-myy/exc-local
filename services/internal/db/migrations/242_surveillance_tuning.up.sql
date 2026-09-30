-- 242_surveillance_tuning.up.sql
-- Phase-21 Task 21.3.27 — reporting values, surveillance tuning &
-- audit-trail query support.
--
-- reporting_values: the tabulated reporting register — every
--   reportable field resolves to a row here (venue LEI, CFTC
--   large-trader thresholds, APA/ARM endpoint refs) rather than a
--   hardcoded literal. scope + key are the lookup pair; JSONB value
--   keeps per-scheme payloads shaped without schema churn.
--
-- surveillance_signal_tuning: per-signal detection parameter register
--   with versioned DRAFT → ACTIVE → SUPERSEDED lifecycle. Activation
--   flips the live version atomically (detectors re-read
--   ActiveTuning() — tuning deploys without downtime); the FP-rate
--   target + backtest calibration ride each version.

BEGIN;

CREATE TABLE reporting_values (
    id         BIGSERIAL    PRIMARY KEY,
    scope      VARCHAR(48)  NOT NULL,                 -- 'CFTC_LIMIT' | 'VENUE_LEI' | 'REPORT_ENDPOINT' | …
    key        VARCHAR(64)  NOT NULL,                 -- pair / field name within the scope
    value      JSONB        NOT NULL,
    source     VARCHAR(120) NOT NULL DEFAULT '',      -- provenance (schedule name / regulator doc)
    effective_from TIMESTAMPTZ NOT NULL DEFAULT now(),
    created_at TIMESTAMPTZ  NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ  NOT NULL DEFAULT now(),
    UNIQUE (scope, key)
);

COMMENT ON TABLE reporting_values IS
    'Phase-21 Task 21.3.27 — tabulated reporting values. Reporters '
    'resolve every reportable field from this register or a validated '
    'identifier (LEI checksum, spec §14.9.1) — no silent hardcodes.';

-- Venue-calibration CFTC large-trader reference thresholds — class
-- defaults (instrument_id NULL) per spec §14.11 "tabulated values".
-- USD notional; operators refine per regulator guidance. Idempotent:
-- guarded by NOT EXISTS (UNIQUE(instrument_id, effective_from) does
-- not dedupe NULL instrument_ids).
INSERT INTO cftc_position_limits
    (instrument_id, instrument_type, currency_pair, all_months_limit,
     large_trader_threshold, currency, effective_from, source)
SELECT NULL, 'SWAP', pair, lim, thr, 'USD', '2026-01-01', 'VENUE-CALIBRATION-21.3.27'
FROM (VALUES
    ('EUR/USD', 500000000,  25000000),
    ('USD/JPY', 400000000,  20000000),
    ('GBP/USD', 300000000,  15000000),
    ('USD/CHF', 200000000,  10000000),
    ('USD/CAD', 200000000,  10000000),
    ('AUD/USD', 150000000,   7500000),
    ('NZD/USD',  80000000,   4000000),
    ('EUR/GBP', 200000000,  10000000),
    ('EUR/JPY', 200000000,  10000000),
    ('GBP/JPY', 150000000,   7500000)
) AS seed(pair, lim, thr)
WHERE NOT EXISTS (
    SELECT 1 FROM cftc_position_limits c
    WHERE c.instrument_id IS NULL
      AND c.instrument_type = 'SWAP'
      AND c.currency_pair   = seed.pair);

CREATE TABLE surveillance_signal_tuning (
    id           BIGSERIAL    PRIMARY KEY,
    signal_type  VARCHAR(32)  NOT NULL,                -- surveillance SignalType vocabulary
    version      INTEGER      NOT NULL,
    params       JSONB        NOT NULL,                -- detector knobs (mirrors surveillance.Config keys)
    fp_target_pct DECIMAL(6,3) NOT NULL,               -- false-positive target % per spec §14.9.2 table
    calibration  JSONB        NOT NULL DEFAULT '{}',   -- backtest result payload (Backtest)
    status       VARCHAR(12)  NOT NULL DEFAULT 'DRAFT' CHECK (status IN ('DRAFT','ACTIVE','SUPERSEDED')),
    effective_from TIMESTAMPTZ,
    created_by   BIGINT       NOT NULL,
    created_at   TIMESTAMPTZ  NOT NULL DEFAULT now(),
    updated_at   TIMESTAMPTZ  NOT NULL DEFAULT now(),
    UNIQUE (signal_type, version)
);

CREATE UNIQUE INDEX idx_signal_tuning_active
    ON surveillance_signal_tuning (signal_type) WHERE status = 'ACTIVE';

COMMENT ON TABLE surveillance_signal_tuning IS
    'Phase-21 Task 21.3.27 — per-signal detection tuning. One ACTIVE '
    'row per signal_type (partial unique); ActivateTuning swaps '
    'atomically so detectors pick up params without restart.';

COMMIT;
