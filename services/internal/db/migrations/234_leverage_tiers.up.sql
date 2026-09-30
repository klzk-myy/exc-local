-- 234_leverage_tiers.up.sql
-- Phase-19 Tasks 19.3.17 (spec §13.6f, §24 #231) and 19.3.23 (§13.13,
-- §24 #366).
--
--   1. leverage_tiers — notional-band leverage schedule per
--      instrument_group × regulatory_regime: max_leverage steps down
--      as gross notional crosses band boundaries. notional_to NULL =
--      open-ended top band. Bands must tile contiguously per cell
--      (the resolver validates coverage at load).
--
--   2. account_leverage — the account's chosen leverage: per-symbol
--      rows (instrument_id set) plus the account-default row
--      (instrument_id NULL, UNIQUE NULLS NOT DISTINCT). The runtime
--      endpoint POST /api/v1/account/leverage writes here after the
--      resolver proves the request is within the resolved cap.
--      Chosen leverage is one input into the effective-resolution
--      minimum — never authoritative on its own.

BEGIN;

CREATE TABLE leverage_tiers (
    id                BIGSERIAL PRIMARY KEY,
    instrument_group  VARCHAR(8)   NOT NULL
                      CHECK (instrument_group IN ('MAJOR','MINOR','EXOTIC')),
    regulatory_regime VARCHAR(16)  NOT NULL
                      CHECK (regulatory_regime IN ('ESMA','CFTC','PROFESSIONAL')),
    notional_from     DECIMAL(28,8) NOT NULL CHECK (notional_from >= 0),
    notional_to       DECIMAL(28,8)
                      CHECK (notional_to IS NULL OR notional_to > notional_from),
    max_leverage      INTEGER       NOT NULL CHECK (max_leverage > 0),
    created_at        TIMESTAMPTZ   NOT NULL DEFAULT now(),
    updated_at        TIMESTAMPTZ   NOT NULL DEFAULT now(),
    CONSTRAINT leverage_tiers_band_uq
        UNIQUE (instrument_group, regulatory_regime, notional_from)
);

-- Seed bands (spec §13.6f example: ESMA retail majors 0–$1M 30:1,
-- $1M–$5M 20:1, $5M–$10M 10:1, >$10M 5:1). Other regimes/groups scale
-- proportionally from their flat caps; every cell is admin-editable
-- under dual control.
INSERT INTO leverage_tiers
    (instrument_group, regulatory_regime, notional_from, notional_to, max_leverage)
VALUES
    -- ESMA
    ('MAJOR','ESMA',       0,  1000000, 30),
    ('MAJOR','ESMA', 1000000,  5000000, 20),
    ('MAJOR','ESMA', 5000000, 10000000, 10),
    ('MAJOR','ESMA',10000000,     NULL,  5),
    ('MINOR','ESMA',       0,  1000000, 20),
    ('MINOR','ESMA', 1000000,  5000000, 15),
    ('MINOR','ESMA', 5000000, 10000000, 10),
    ('MINOR','ESMA',10000000,     NULL,  5),
    ('EXOTIC','ESMA',       0,  1000000, 10),
    ('EXOTIC','ESMA', 1000000,  5000000,  8),
    ('EXOTIC','ESMA', 5000000, 10000000,  5),
    ('EXOTIC','ESMA',10000000,     NULL,  2),
    -- CFTC (50:1 major / 20:1 minor retail)
    ('MAJOR','CFTC',       0,  1000000, 50),
    ('MAJOR','CFTC', 1000000,  5000000, 35),
    ('MAJOR','CFTC', 5000000, 10000000, 20),
    ('MAJOR','CFTC',10000000,     NULL, 10),
    ('MINOR','CFTC',       0,  1000000, 20),
    ('MINOR','CFTC', 1000000,  5000000, 15),
    ('MINOR','CFTC', 5000000, 10000000, 10),
    ('MINOR','CFTC',10000000,     NULL,  5),
    ('EXOTIC','CFTC',       0,  1000000, 10),
    ('EXOTIC','CFTC', 1000000,  5000000,  8),
    ('EXOTIC','CFTC', 5000000, 10000000,  5),
    ('EXOTIC','CFTC',10000000,     NULL,  2),
    -- PROFESSIONAL (negotiable ceilings — mirrors entity policy seeds)
    ('MAJOR','PROFESSIONAL',       0,  1000000, 200),
    ('MAJOR','PROFESSIONAL', 1000000,  5000000, 150),
    ('MAJOR','PROFESSIONAL', 5000000, 10000000, 100),
    ('MAJOR','PROFESSIONAL',10000000,     NULL,  50),
    ('MINOR','PROFESSIONAL',       0,  1000000, 100),
    ('MINOR','PROFESSIONAL', 1000000,  5000000,  75),
    ('MINOR','PROFESSIONAL', 5000000, 10000000,  50),
    ('MINOR','PROFESSIONAL',10000000,     NULL,  25),
    ('EXOTIC','PROFESSIONAL',       0,  1000000, 50),
    ('EXOTIC','PROFESSIONAL', 1000000,  5000000, 40),
    ('EXOTIC','PROFESSIONAL', 5000000, 10000000, 25),
    ('EXOTIC','PROFESSIONAL',10000000,     NULL, 10)
ON CONFLICT (instrument_group, regulatory_regime, notional_from) DO NOTHING;

CREATE TABLE account_leverage (
    id            BIGSERIAL PRIMARY KEY,
    account_id    BIGINT NOT NULL REFERENCES accounts (id),
    instrument_id BIGINT REFERENCES instruments (id),  -- NULL = account default
    leverage      INTEGER NOT NULL CHECK (leverage > 0),
    updated_by    BIGINT,                              -- caller user id (audit)
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT account_leverage_cell_uq
        UNIQUE NULLS NOT DISTINCT (account_id, instrument_id)
);

COMMIT;
