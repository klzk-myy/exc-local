-- IMP-PLAN Phase-3 Task 4 — cross-shard basket operation ledger.
--
-- The basket's 128-bit operation id (op_hi, op_lo) is the engine's dedup
-- key (OptimisticShardCoordinator caches the OptResult per id — §2.2a).
-- `baskets` is the queryable record of record; `basket_legs` joins each
-- leg's real `orders` row (the leg fills arrive through the ordinary
-- TradeFill projection — legs are real engine orders).
--
-- status mirrors OptStatus: MATCHING | COMMITTED | UNWINDING |
-- COMPENSATED | FAILED | REJECTED (uppercase wire names).

CREATE TABLE baskets (
    op_id_hi    BIGINT      NOT NULL,
    op_id_lo    BIGINT      NOT NULL,
    account_id  BIGINT      NOT NULL REFERENCES accounts (id),
    status      TEXT        NOT NULL DEFAULT 'MATCHING',
    code        INTEGER     NOT NULL DEFAULT 0,
    leg_count   SMALLINT    NOT NULL,
    legs_filled SMALLINT    NOT NULL DEFAULT 0,
    legs_unwound SMALLINT   NOT NULL DEFAULT 0,
    slippage_ticks BIGINT   NOT NULL DEFAULT 0,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (op_id_hi, op_id_lo)
);

CREATE TABLE basket_legs (
    op_id_hi      BIGINT   NOT NULL,
    op_id_lo      BIGINT   NOT NULL,
    leg_index     SMALLINT NOT NULL,
    order_id      BIGINT   NOT NULL REFERENCES orders (id),
    instrument_id BIGINT   NOT NULL REFERENCES instruments (id),
    shard_id      INTEGER  NOT NULL,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (op_id_hi, op_id_lo, leg_index),
    FOREIGN KEY (op_id_hi, op_id_lo)
        REFERENCES baskets (op_id_hi, op_id_lo)
);

CREATE INDEX idx_basket_legs_order ON basket_legs (order_id);
CREATE INDEX idx_baskets_account ON baskets (account_id, created_at DESC);
