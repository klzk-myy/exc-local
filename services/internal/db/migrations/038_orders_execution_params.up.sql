-- 038_orders_execution_params.up.sql
-- Phase-16 Task 16.3.10 — execution-parameter persistence (spec §5.4,
-- §6.2, §24 #66/#71/#76; columns documented by 005_create_orders).
--
-- RESIDUAL SCOPE: this migration lands only the columns not already
-- shipped elsewhere. post_only / reduce_only / display_qty / stp_mode
-- materialized early via 155_order_execution_flags (recorded there as
-- the 038 carry-over) and oco_group_id via 218_oco_group_link — they
-- are deliberately NOT re-added here.
--
--   peg_offset        signed price offset (price scale, same DECIMAL(20,8)
--                     as orders.price) applied to the peg reference.
--   peg_mode          MID | PRIMARY | MARKET — absent ⇒ not pegged.
--   peg_limit         optional limit collar for pegged orders.
--   algo_type         parent strategy discriminator for delayed/algo
--                     execution (TWAP/VWAP/TRAILING_STOP/SCALED/…).
--   algo_params       strategy parameter blob — schema-validated per
--                     algo_type at admission (size cap enforced in the
--                     gateway, Phase-16).
--   fixing_benchmark  WM_R_4PM | ECB_1415 | TOKYO_0955 — the canonical
--                     spec §6.4 order-level vocabulary (supersedes the
--                     plan strings WM_REFINITIV_4PM_LDN / ECB_1415_CET;
--                     the auction_calendar scheduler vocabulary
--                     WM_LONDON_4PM / ECB_REF_1415 maps onto these).
--   hidden            Phase-16 Task 16.3.13 dark/hidden liquidity flag.
--   gslo              Phase-16 Task 16.3.16 guaranteed-stop flag — the
--                     premium + exposure-cap journal posts ride this row.

BEGIN;

ALTER TABLE orders
    ADD COLUMN peg_offset       DECIMAL(20,8),
    ADD COLUMN peg_mode         VARCHAR(32),
    ADD COLUMN peg_limit        DECIMAL(20,8),
    ADD COLUMN algo_type        VARCHAR(24),
    ADD COLUMN algo_params      JSONB,
    ADD COLUMN fixing_benchmark VARCHAR(16),
    ADD COLUMN hidden           BOOLEAN NOT NULL DEFAULT FALSE,
    ADD COLUMN gslo             BOOLEAN NOT NULL DEFAULT FALSE;

ALTER TABLE orders
    ADD CONSTRAINT orders_peg_mode_check
        CHECK (peg_mode IS NULL OR peg_mode IN ('MID', 'PRIMARY', 'MARKET')),
    ADD CONSTRAINT orders_fixing_benchmark_check
        CHECK (fixing_benchmark IS NULL
               OR fixing_benchmark IN ('WM_R_4PM', 'ECB_1415', 'TOKYO_0955'));

COMMIT;
