-- 181_fee_promo_windows.up.sql
-- Phase-05 Task 5.3.15 — fee promotion windows.
--
-- fee_tiers.promo_until / promo_maker_bps / promo_taker_bps (migration
-- 012, spec §5.11/§8.5) are the engine seam — settlement.FeeTier.RateBps
-- already applies promo rates strictly while promo_until > now(). This
-- table is the governed write path: a promo window is created
-- PENDING_APPROVAL and only a *second* approver applies it to fee_tiers
-- inside the spec §8.2 four-eyes window (fee-tier change is on the
-- dual-control list; 15-minute approval window, approver ≠ creator).

BEGIN;

-- The promo range contract is 0..10000 bps (spec §8.5 Task 5.3.15);
-- DECIMAL(6,4) tops out at 99.9999 — widen the fee_tiers promo columns
-- (base maker/taker_bps keep DECIMAL(6,4): base rates are sub-1%) so the
-- full legal range is representable where the window is applied.
ALTER TABLE fee_tiers
    ALTER COLUMN promo_maker_bps TYPE DECIMAL(9,4),
    ALTER COLUMN promo_taker_bps TYPE DECIMAL(9,4);

CREATE TABLE fee_promo_windows (
    id               BIGSERIAL PRIMARY KEY,
    fee_tier_id      BIGINT       NOT NULL REFERENCES fee_tiers (id),
    promo_maker_bps  DECIMAL(9,4),                         -- NULL = maker side not promoted
    promo_taker_bps  DECIMAL(9,4),                         -- NULL = taker side not promoted
    ends_at          TIMESTAMPTZ  NOT NULL,                -- applied as fee_tiers.promo_until
    status           VARCHAR(20)  NOT NULL DEFAULT 'PENDING_APPROVAL',
                     -- PENDING_APPROVAL | APPLIED | REJECTED | EXPIRED
    created_by       BIGINT       NOT NULL,
    approved_by      BIGINT,
    approved_at      TIMESTAMPTZ,
    note             TEXT,
    created_at       TIMESTAMPTZ  NOT NULL DEFAULT now(),
    updated_at       TIMESTAMPTZ  NOT NULL DEFAULT now(),
    CONSTRAINT fee_promo_windows_some_rate CHECK
        (promo_maker_bps IS NOT NULL OR promo_taker_bps IS NOT NULL),
    CONSTRAINT fee_promo_windows_four_eyes CHECK
        (approved_by IS NULL OR approved_by <> created_by)
);

CREATE INDEX idx_fee_promo_windows_pending ON fee_promo_windows (created_at)
    WHERE status = 'PENDING_APPROVAL';
CREATE INDEX idx_fee_promo_windows_tier ON fee_promo_windows (fee_tier_id, status);

COMMIT;
