-- 014_create_positions.up.sql
-- Spec §5.13 positions — baseline columns only.
-- Excluded (owned by later migrations): isolated_margin_allocated,
--   auto_margin_replenish (106).

BEGIN;

CREATE TYPE position_side_enum AS ENUM ('LONG', 'SHORT');

CREATE TABLE positions (
    id                BIGSERIAL PRIMARY KEY,
    account_id        BIGINT NOT NULL,
    instrument_id     BIGINT NOT NULL,
    side              position_side_enum NOT NULL,
    quantity          DECIMAL(28,8) NOT NULL,
    entry_price       DECIMAL(20,8) NOT NULL,
    mark_price        DECIMAL(20,8),
    unrealized_pnl    DECIMAL(28,8) NOT NULL DEFAULT 0,
    realized_pnl      DECIMAL(28,8) NOT NULL DEFAULT 0,
    liquidation_price DECIMAL(20,8),
    margin_used       DECIMAL(28,8) NOT NULL DEFAULT 0,
    updated_at        TIMESTAMPTZ NOT NULL DEFAULT now()
);

COMMIT;
