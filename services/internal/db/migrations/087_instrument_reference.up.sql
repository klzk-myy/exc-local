-- 087_instrument_reference.up.sql
-- Phase-15 Task 15.3.11 (spec §5.1, §5.44 item 10, §7.1, §7.4, §24 #343/#401).
--
-- Three artifacts:
--
--   1. instruments gains the complete §7.4 per-symbol reference columns
--      (contract_size, decimal_places, pip_size) and every row is
--      backfilled to the production reference set: majors 5dp tick
--      0.00001, JPY-quoted pairs 3dp tick 0.001 (USD/JPY's migration-001
--      pipette tick of 0.00001 is corrected here — §7.4 JPY convention
--      supersedes the 001 seed note), pip_size 0.0001/0.01, contract
--      size 100,000 base units, min_notional in quote currency. Four
--      symbols from the §2.2 production shard universe that 001 never
--      seeded land here: EUR/GBP, EUR/JPY, EUR/CHF, USD/BRL.
--
--   2. instruments_reference (spec §5.44 item 10) — the per-instrument
--      reference envelope: margin_rate, trading_hours JSONB session
--      boundaries, contract_size/decimal_places/pip_size mirrors, tenor
--      grid JSONB, fixing_calendar JSONB (NDF 10:00 NY cut, venue 15:00
--      UTC cut) and delist_schedule JSONB — the durable home for the
--      §7.5 delisting ladder state (RESTRICTED notice → DELISTED →
--      30d close-only → purge) consumed by Task 15.3.12.
--
--   3. auction_calendar (spec §7.1 daily closing-auction calendar,
--      Task 15.3.13) — per-instrument rows with auction_type
--      (DAILY_CLOSE | BENCHMARK_FIXING | INTRADAY), trigger_time,
--      timezone, cron-like recurrence and enabled flag. trigger_time is
--      the local wall-clock time in `timezone`; the engine resolves the
--      UTC instant per occurrence — that is what makes the recurrence
--      DST-aware (16:00 Europe/London stays 16:00 London through the
--      BST switch). For UTC-pinned triggers use timezone='UTC'.
--      Seeds: Friday 22:00 UTC weekly close auction + the three
--      benchmark fixings (WM/R London 4PM, ECB 14:15 CET, Tokyo 09:55
--      JST) per instrument.
--
--   The narrow listing_proposals extension (Task 15.3.12 — activation
--   schedule + DRAFT link + review/dual-control columns) lives in
--   migration 223, after the 091 fleet migration that creates the table.
--
-- No corporate actions: fiat spot FX has none — no event table exists
-- here or elsewhere; CORPORATE_ACTION_SCHEDULED stays reserved and is
-- never emitted (spec §7.4 item 4). Redenomination / peg-break follows
-- the RESTRICTED → DELISTED ladder with force-close under the Task
-- 15.3.8 maker-checker.

BEGIN;

-- ---------------------------------------------------------------------------
-- 1a. instruments: complete §7.4 reference columns
-- ---------------------------------------------------------------------------
ALTER TABLE instruments
    ADD COLUMN contract_size  DECIMAL(20,8),
    ADD COLUMN decimal_places SMALLINT,
    ADD COLUMN pip_size       DECIMAL(10,8);

-- Per-symbol production reference values. USD/JPY's tick is corrected
-- to the JPY 3dp convention (0.001) per §7.4 — the 001 seed note
-- ("tick_size 0.00001 for all seeds") is superseded for JPY quotes.
UPDATE instruments SET
    contract_size  = 100000,
    decimal_places = CASE WHEN quote_currency = 'JPY' THEN 3 ELSE 5 END,
    pip_size       = CASE WHEN quote_currency = 'JPY' THEN 0.01 ELSE 0.0001 END,
    tick_size      = CASE WHEN quote_currency = 'JPY' THEN 0.001 ELSE tick_size END,
    min_notional   = CASE symbol
        WHEN 'USD/JPY' THEN 100000   -- ~$650 notional floor in JPY
        WHEN 'USD/MXN' THEN 20000    -- ~$1000 in MXN
        ELSE 1000                    -- quote-ccy units (USD/CAD/CHF)
    END;

ALTER TABLE instruments
    ALTER COLUMN contract_size  SET NOT NULL,
    ALTER COLUMN decimal_places SET NOT NULL,
    ALTER COLUMN pip_size       SET NOT NULL;

ALTER TABLE instruments
    ADD CONSTRAINT instruments_contract_size_positive CHECK (contract_size > 0),
    ADD CONSTRAINT instruments_decimal_places_range   CHECK (decimal_places BETWEEN 0 AND 8),
    ADD CONSTRAINT instruments_pip_size_positive      CHECK (pip_size > 0);

-- Convention-derived insert defaults: a DRAFT created without the new
-- reference fields (e.g. the Task 15.3.2 admin create, whose payload
-- predates 087) still lands a complete row — pip_size/decimal_places
-- follow the JPY-quote convention, contract_size the standard lot.
-- These are derivations, not synthetic values; the Task 15.3.12 listing
-- approval overwrites them with the proposer's explicit reference row.
CREATE FUNCTION instruments_reference_defaults() RETURNS trigger AS $$
BEGIN
    IF NEW.pip_size IS NULL THEN
        NEW.pip_size := CASE WHEN NEW.quote_currency = 'JPY'
                             THEN 0.01 ELSE 0.0001 END;
    END IF;
    IF NEW.decimal_places IS NULL THEN
        NEW.decimal_places := CASE WHEN NEW.quote_currency = 'JPY'
                                   THEN 3 ELSE 5 END;
    END IF;
    IF NEW.contract_size IS NULL THEN
        NEW.contract_size := 100000;
    END IF;
    RETURN NEW;
END
$$ LANGUAGE plpgsql;

CREATE TRIGGER instruments_reference_defaults_trg
    BEFORE INSERT ON instruments
    FOR EACH ROW EXECUTE FUNCTION instruments_reference_defaults();

-- ---------------------------------------------------------------------------
-- 1b. Production symbol seed — §2.2 shard universe rows missing from 001.
-- ESMA leverage: EUR/GBP, EUR/JPY, EUR/CHF are major-set pairs (both legs
-- in {USD,EUR,JPY,GBP,CAD,CHF}) → 30:1; USD/BRL is exotic → 10:1.
-- Settlement: T+1 per the §6.3 default and §17.3a CLS table; USD/BRL
-- settles T+2 (exotic pair per the canonical settlement rule).
-- ---------------------------------------------------------------------------
INSERT INTO instruments
    (symbol, base_currency, quote_currency, instrument_type,
     tick_size, lot_size, min_order_qty, max_order_qty, min_notional,
     contract_size, decimal_places, pip_size,
     price_band_pct_up, price_band_pct_down,
     settlement_cycle, max_leverage, status)
VALUES
    ('EUR/GBP', 'EUR', 'GBP', 'SPOT', 0.00001, 1000, 1000, 100000000, 1000,
     100000, 5, 0.0001, 2.00, 5.00, 1, 30, 'ACTIVE'),
    ('EUR/JPY', 'EUR', 'JPY', 'SPOT', 0.001, 1000, 1000, 100000000, 100000,
     100000, 3, 0.01, 2.00, 5.00, 1, 30, 'ACTIVE'),
    ('EUR/CHF', 'EUR', 'CHF', 'SPOT', 0.00001, 1000, 1000, 100000000, 1000,
     100000, 5, 0.0001, 2.00, 5.00, 1, 30, 'ACTIVE'),
    ('USD/BRL', 'USD', 'BRL', 'SPOT', 0.00001, 1000, 1000, 100000000, 5000,
     100000, 5, 0.0001, 2.00, 5.00, 2, 10, 'ACTIVE')
ON CONFLICT (symbol) DO NOTHING;

-- ---------------------------------------------------------------------------
-- 2. instruments_reference (spec §5.44 item 10) — reference envelope.
--    delist_schedule JSONB carries the §7.5 ladder bookkeeping for
--    Task 15.3.12 (intent, impact snapshot, notice/delist/purge marks).
-- ---------------------------------------------------------------------------
CREATE TABLE instruments_reference (
    id              BIGSERIAL     PRIMARY KEY,
    instrument_id   BIGINT        NOT NULL UNIQUE REFERENCES instruments (id) ON DELETE CASCADE,
    symbol          VARCHAR(32)   NOT NULL UNIQUE,
    margin_rate     DECIMAL(10,6) NOT NULL,
    trading_hours   JSONB         NOT NULL DEFAULT '{}'::jsonb,
    contract_size   DECIMAL(20,8) NOT NULL,
    decimal_places  SMALLINT      NOT NULL,
    pip_size        DECIMAL(10,8) NOT NULL,
    tenor           JSONB         NOT NULL DEFAULT '[]'::jsonb,
    fixing_calendar JSONB         NOT NULL DEFAULT '{}'::jsonb,
    delist_schedule JSONB         NOT NULL DEFAULT '{}'::jsonb,
    created_at      TIMESTAMPTZ   NOT NULL DEFAULT now(),
    updated_at      TIMESTAMPTZ   NOT NULL DEFAULT now()
);

INSERT INTO instruments_reference
    (instrument_id, symbol, margin_rate, trading_hours,
     contract_size, decimal_places, pip_size, tenor, fixing_calendar)
SELECT
    i.id,
    i.symbol,
    ROUND(1.0 / i.max_leverage, 6),
    jsonb_build_object(
        'open_day',        'SUN',
        'open_local',      '17:00',
        'close_day',       'FRI',
        'close_local',     '17:00',
        'timezone',        'America/New_York',
        'pre_open_minutes', 15,
        'daily_cutoff',    '17:00 America/New_York',
        'description',     '24/5 FX week — 17:00 ET Sunday open through 17:00 ET Friday close (21:00/22:00 UTC DST-resolved)')
    ,
    i.contract_size,
    i.decimal_places,
    i.pip_size,
    '["ON","TN","SN","1W","2W","1M","2M","3M","6M","9M","1Y","2Y"]'::jsonb,
    jsonb_build_object(
        'ndf_fixing_cut', '10:00 America/New_York',
        'venue_cut',      '15:00 UTC',
        'benchmarks',     jsonb_build_array(
            jsonb_build_object('code', 'WM_LONDON_4PM', 'local', '16:00', 'timezone', 'Europe/London'),
            jsonb_build_object('code', 'ECB_REF_1415',  'local', '14:15', 'timezone', 'Europe/Berlin'),
            jsonb_build_object('code', 'TOKYO_0955',    'local', '09:55', 'timezone', 'Asia/Tokyo')))
FROM instruments i;

-- ---------------------------------------------------------------------------
-- 3. auction_calendar (spec §7.1, Task 15.3.13).
--    trigger_time = wall-clock in `timezone` (UTC instants resolved per
--    occurrence — DST-aware recurrence). benchmark discriminates the
--    BENCHMARK_FIXING rows; NULL for DAILY_CLOSE/INTRADAY.
-- ---------------------------------------------------------------------------
CREATE TABLE auction_calendar (
    id            BIGSERIAL    PRIMARY KEY,
    instrument_id BIGINT       NOT NULL REFERENCES instruments (id) ON DELETE CASCADE,
    symbol        VARCHAR(32)  NOT NULL,
    auction_type  VARCHAR(24)  NOT NULL
                  CHECK (auction_type IN ('DAILY_CLOSE','BENCHMARK_FIXING','INTRADAY')),
    benchmark     VARCHAR(32)
                  CHECK (benchmark IS NULL OR benchmark IN
                      ('WM_LONDON_4PM','ECB_REF_1415','TOKYO_0955')),
    trigger_time  TIME         NOT NULL,   -- wall-clock in `timezone`
    timezone      VARCHAR(64)  NOT NULL DEFAULT 'UTC',
    recurrence    VARCHAR(64)  NOT NULL DEFAULT 'MON-FRI',
    enabled       BOOLEAN      NOT NULL DEFAULT true,
    last_fired_at TIMESTAMPTZ,             -- occurrence bookkeeping (durable dedupe)
    created_at    TIMESTAMPTZ  NOT NULL DEFAULT now(),
    updated_at    TIMESTAMPTZ  NOT NULL DEFAULT now(),
    CONSTRAINT auction_calendar_benchmark_required
        CHECK (auction_type <> 'BENCHMARK_FIXING' OR benchmark IS NOT NULL),
    CONSTRAINT auction_calendar_unique_slot
        UNIQUE NULLS NOT DISTINCT (instrument_id, auction_type, benchmark, trigger_time)
);
CREATE INDEX auction_calendar_enabled_idx
    ON auction_calendar (enabled) WHERE enabled;

-- Default calendar per §7.1: weekly Friday 22:00 UTC session close plus
-- the three benchmark fixing triggers per instrument.
INSERT INTO auction_calendar
    (instrument_id, symbol, auction_type, benchmark, trigger_time, timezone, recurrence)
SELECT i.id, i.symbol, 'DAILY_CLOSE', NULL, '22:00', 'UTC', 'FRI'
FROM instruments i;

INSERT INTO auction_calendar
    (instrument_id, symbol, auction_type, benchmark, trigger_time, timezone, recurrence)
SELECT i.id, i.symbol, 'BENCHMARK_FIXING', 'WM_LONDON_4PM', '16:00', 'Europe/London', 'MON-FRI'
FROM instruments i;

INSERT INTO auction_calendar
    (instrument_id, symbol, auction_type, benchmark, trigger_time, timezone, recurrence)
SELECT i.id, i.symbol, 'BENCHMARK_FIXING', 'ECB_REF_1415', '14:15', 'Europe/Berlin', 'MON-FRI'
FROM instruments i;

INSERT INTO auction_calendar
    (instrument_id, symbol, auction_type, benchmark, trigger_time, timezone, recurrence)
SELECT i.id, i.symbol, 'BENCHMARK_FIXING', 'TOKYO_0955', '09:55', 'Asia/Tokyo', 'MON-FRI'
FROM instruments i;

COMMIT;
