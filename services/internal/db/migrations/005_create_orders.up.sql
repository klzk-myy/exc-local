-- 005_create_orders.up.sql
-- Spec §5.4 orders — baseline columns only.
-- Excluded (owned by later migrations):
--   post_only, reduce_only, display_qty, peg_offset, peg_mode, stp_mode,
--   oco_group_id, algo_params (038);
--   strike, option_type, exercise_style, expiry_at, barrier_type, barrier_level,
--   value_date, near_leg_value_date, far_leg_value_date, premium,
--   premium_currency, fixing_benchmark (039);
--   trigger_source (066); fix_session_id / cod_exempt (#35);
--   expiry_reason / prevented_qty (072); discretionary_offset_pips (103);
--   settlement_intent (104).

BEGIN;

CREATE TYPE order_side_enum   AS ENUM ('BUY', 'SELL');
CREATE TYPE order_type_enum   AS ENUM (
    'LIMIT', 'MARKET', 'STOP', 'STOP_LIMIT', 'ICEBERG', 'TWAP', 'VWAP',
    'TRAILING_STOP', 'BRACKET', 'OCO', 'SPREAD', 'SCALE', 'PEG', 'FIXING',
    'MOO', 'MOC'
);
CREATE TYPE time_in_force_enum AS ENUM ('GTC', 'IOC', 'FOK', 'GTD', 'DAY');
CREATE TYPE order_status_enum  AS ENUM (
    'PENDING', 'RESERVED', 'ACTIVE', 'PARTIALLY_FILLED',
    'FILLED', 'CANCELLED', 'REJECTED', 'EXPIRED'
);

CREATE TABLE orders (
    id              BIGSERIAL PRIMARY KEY,
    account_id      BIGINT NOT NULL REFERENCES accounts (id),
    instrument_id   BIGINT NOT NULL REFERENCES instruments (id),
    client_order_id VARCHAR(64),                        -- client-provided, unique per account (partial unique index in 020)
    side            order_side_enum   NOT NULL,
    order_type      order_type_enum   NOT NULL,
    quantity        DECIMAL(28,8)     NOT NULL,
    price           DECIMAL(20,8),                      -- NULL for market
    stop_price      DECIMAL(20,8),                      -- NULL if no stop
    time_in_force   time_in_force_enum NOT NULL,
    status          order_status_enum  NOT NULL DEFAULT 'PENDING',
    filled_qty      DECIMAL(28,8)      NOT NULL DEFAULT 0,
    avg_fill_price  DECIMAL(20,8),
    shard_id        SMALLINT,
    book_seq        BIGINT,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);

COMMIT;
