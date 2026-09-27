-- 001_create_instruments.up.sql
-- Spec §5.1 instruments — baseline columns only.
-- Later-migration-owned columns intentionally excluded:
--   min_notional (migration 050), settlement_mode (031),
--   contract_size / decimal_places / pip_size (087).

BEGIN;

CREATE TYPE instrument_type_enum AS ENUM ('SPOT', 'FORWARD', 'SWAP', 'NDF', 'OPTION');
CREATE TYPE instrument_status_enum AS ENUM ('DRAFT', 'ACTIVE', 'CANCEL_ONLY', 'SUSPENDED', 'HALTED', 'RESTRICTED', 'DELISTED');

CREATE TABLE instruments (
    id                  BIGSERIAL PRIMARY KEY,
    symbol              VARCHAR(32) NOT NULL UNIQUE,
    base_currency       VARCHAR(3)  NOT NULL,
    quote_currency      VARCHAR(3)  NOT NULL,
    instrument_type     instrument_type_enum   NOT NULL,
    tick_size           DECIMAL(20,8) NOT NULL,
    lot_size            DECIMAL(20,8) NOT NULL,
    min_order_qty       DECIMAL(20,8) NOT NULL,
    max_order_qty       DECIMAL(20,8) NOT NULL,
    price_band_pct_up   DECIMAL(5,2)  NOT NULL DEFAULT 2.00,
    price_band_pct_down DECIMAL(5,2)  NOT NULL DEFAULT 5.00,
    settlement_cycle    SMALLINT      NOT NULL,          -- T+1=1, T+2=2, same-day=0
    max_leverage        INTEGER       NOT NULL,          -- ESMA retail: 30 major / 20 minor / 10 exotic
    status              instrument_status_enum NOT NULL DEFAULT 'DRAFT',
    created_at          TIMESTAMPTZ   NOT NULL DEFAULT now(),
    updated_at          TIMESTAMPTZ   NOT NULL DEFAULT now()
);

-- Seed instruments (spec §5.1 / Phase-01 Task 1.3.3 step 4).
-- Pipette convention: tick_size 0.00001 and lot_size 1000 for all seeds.
-- settlement_cycle: T+1 for majors; same-day (0) for USD/CAD AND USD/MXN
-- (canonical per AGENTS.md — supersedes older USD/CAD wording; spec §6.3).
-- max_leverage: 30 for ESMA major pairs, 10 for USD/MXN (exotic).
INSERT INTO instruments
    (symbol, base_currency, quote_currency, instrument_type,
     tick_size, lot_size, min_order_qty, max_order_qty,
     price_band_pct_up, price_band_pct_down,
     settlement_cycle, max_leverage, status)
VALUES
    ('EUR/USD', 'EUR', 'USD', 'SPOT', 0.00001, 1000, 1000, 100000000, 2.00, 5.00, 1, 30, 'ACTIVE'),
    ('GBP/USD', 'GBP', 'USD', 'SPOT', 0.00001, 1000, 1000, 100000000, 2.00, 5.00, 1, 30, 'ACTIVE'),
    ('USD/JPY', 'USD', 'JPY', 'SPOT', 0.00001, 1000, 1000, 100000000, 2.00, 5.00, 1, 30, 'ACTIVE'),
    ('AUD/USD', 'AUD', 'USD', 'SPOT', 0.00001, 1000, 1000, 100000000, 2.00, 5.00, 1, 30, 'ACTIVE'),
    ('USD/CAD', 'USD', 'CAD', 'SPOT', 0.00001, 1000, 1000, 100000000, 2.00, 5.00, 0, 30, 'ACTIVE'),
    ('USD/CHF', 'USD', 'CHF', 'SPOT', 0.00001, 1000, 1000, 100000000, 2.00, 5.00, 1, 30, 'ACTIVE'),
    ('NZD/USD', 'NZD', 'USD', 'SPOT', 0.00001, 1000, 1000, 100000000, 2.00, 5.00, 1, 30, 'ACTIVE'),
    ('USD/MXN', 'USD', 'MXN', 'SPOT', 0.00001, 1000, 1000, 100000000, 2.00, 5.00, 0, 10, 'ACTIVE');

COMMIT;
