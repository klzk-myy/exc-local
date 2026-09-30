-- 085_option_spread_offsets.up.sql
-- Phase-22 Task 22.3.13 (spec §15.7, §13.11, §24 #398): option spread
-- margin offsets — recognized-combination margin relief for PORTFOLIO-mode
-- accounts, with admin-configurable relief percentages.
--
-- Two tables:
--   * option_spread_offset_params — the admin-configured reduction bps per
--     spread type. Maker-checker: a param row may not be ACTIVE unless a
--     different principal approved it; at most one ACTIVE row per type.
--   * option_spread_offsets — one row per recognized spread pair found in
--     a live book. spread_id is a deterministic key
--     (acct:kind:legA:legB) so re-detection after position churn resolves
--     to the same row — the (spread_id) live-unique index is the
--     no-double-count guard.
--
-- order_id attributes the offset to the entry order when detection came
-- from a multi-leg order (§5.44 note); NULL for book-derived detection.
-- bound_margin_usd is the combined margin after relief; offset_bps records
-- the percentage actually applied (audit — params may change later).

BEGIN;

CREATE TYPE option_spread_type_enum AS ENUM
    ('VERTICAL_CALL', 'VERTICAL_PUT', 'STRADDLE', 'STRANGLE', 'CALENDAR');
CREATE TYPE option_spread_offset_status_enum AS ENUM
    ('DETECTED', 'APPLIED', 'BROKEN', 'RETIRED');
CREATE TYPE option_spread_param_status_enum AS ENUM
    ('PENDING_APPROVAL', 'ACTIVE', 'RETIRED');

CREATE TABLE option_spread_offsets (
    id                BIGSERIAL PRIMARY KEY,
    spread_id         VARCHAR(96) NOT NULL,
    account_id        BIGINT      NOT NULL REFERENCES accounts (id),
    instrument_id     BIGINT      NOT NULL REFERENCES instruments (id), -- underlying instrument
    order_id          BIGINT      REFERENCES orders (id),
    spread_type       option_spread_type_enum NOT NULL,
    long_position_id  BIGINT      NOT NULL,
    short_position_id BIGINT,                        -- NULL: all-long combos
    matched_qty       DECIMAL(28,8) NOT NULL CHECK (matched_qty > 0),
    offset_bps        DECIMAL(9,4)  NOT NULL CHECK (offset_bps >= 0 AND offset_bps <= 10000),
    offset_amount_usd DECIMAL(28,8) NOT NULL DEFAULT 0,
    bound_margin_usd  DECIMAL(28,8) NOT NULL DEFAULT 0,
    status            option_spread_offset_status_enum NOT NULL DEFAULT 'DETECTED',
    validated_by      BIGINT,
    validated_at      TIMESTAMPTZ,
    detected_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    created_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at        TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- One live recognition row per spread pair — no-double-count guard.
CREATE UNIQUE INDEX option_spread_offsets_live_ux
    ON option_spread_offsets (spread_id)
    WHERE status IN ('DETECTED', 'APPLIED');
CREATE INDEX option_spread_offsets_account_ix
    ON option_spread_offsets (account_id, status);

CREATE TABLE option_spread_offset_params (
    id             BIGSERIAL PRIMARY KEY,
    spread_type    option_spread_type_enum NOT NULL,
    offset_bps     DECIMAL(9,4) NOT NULL CHECK (offset_bps >= 0 AND offset_bps <= 10000),
    status         option_spread_param_status_enum NOT NULL DEFAULT 'PENDING_APPROVAL',
    proposed_by    VARCHAR(64) NOT NULL,
    approved_by    VARCHAR(64),
    effective_from TIMESTAMPTZ NOT NULL DEFAULT now(),
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    -- Maker-checker: ACTIVE requires a different approver than proposer.
    CHECK (status <> 'ACTIVE'
           OR (approved_by IS NOT NULL AND approved_by <> proposed_by))
);

CREATE UNIQUE INDEX option_spread_offset_params_active_ux
    ON option_spread_offset_params (spread_type)
    WHERE status = 'ACTIVE';

COMMIT;
