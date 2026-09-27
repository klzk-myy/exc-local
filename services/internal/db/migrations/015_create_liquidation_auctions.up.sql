-- 015_create_liquidation_auctions.up.sql
-- Spec §5.14 liquidation_auctions (CALL → FILL → EXTEND ≤60s → FORCE_CASH per §13.4).

BEGIN;

CREATE TYPE auction_phase_enum AS ENUM ('CALL', 'FILL', 'EXTEND', 'FORCE_CASH');

CREATE TABLE liquidation_auctions (
    id             BIGSERIAL PRIMARY KEY,
    instrument_id  BIGINT NOT NULL,
    position_id    BIGINT NOT NULL,
    phase          auction_phase_enum NOT NULL DEFAULT 'CALL',
    floor_price    DECIMAL(20,8),
    unfilled_qty   DECIMAL(28,8),
    filled_qty     DECIMAL(28,8) NOT NULL DEFAULT 0,
    avg_fill_price DECIMAL(20,8),
    phase_start_at TIMESTAMPTZ,
    phase_end_at   TIMESTAMPTZ,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now()
);

COMMIT;
