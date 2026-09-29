-- 051_trade_busts.up.sql
-- Phase-15 Task 15.3.5 (spec §5.29, §7.2 admin-ops matrix, §7.3.4
-- obvious-error deadlines, §24 #138): the trade bust / price-adjust
-- review record, the trades.status post-trade lifecycle flag deferred
-- here by migration 006, and 'VOID' on settlement_status_enum so a
-- not-yet-dispatched settlement instruction can be voided on bust.
--
-- Documented additives/deviations (AGENTS.md change protocol):
--   * trade_busts.trade_id is a plain BIGINT — trades is partitioned by
--     created_at with PK (id, created_at), so a declarative FK on id is
--     structurally impossible; referential integrity is enforced by the
--     bust service (same discipline as settlement_instructions, mig. 019).
--   * trade_busts.reference_price — the market reference the obvious-error
--     deviation was measured against (§7.3.4 evidence column; additive —
--     the §5.29 column set is unchanged).
--   * settlement_status_enum gains 'VOID' — spec §5.29 requires voiding an
--     undispatched settlement record; the enum lacked a terminal void state.
--   * Partial unique indexes pin the §5.29 invariants at the DB layer:
--     at most one PENDING_APPROVAL review and at most one EXECUTED
--     correction per trade (a busted trade can never be re-busted).

ALTER TYPE settlement_status_enum ADD VALUE IF NOT EXISTS 'VOID';

BEGIN;

-- trades.status — the §5.5 post-trade lifecycle flag (set by this workflow;
-- migration 006 deliberately deferred it here).
CREATE TYPE trade_status_enum AS ENUM ('COMPLETED', 'BUSTED', 'PRICE_ADJUSTED');

ALTER TABLE trades
    ADD COLUMN status trade_status_enum NOT NULL DEFAULT 'COMPLETED';

-- trade_busts (spec §5.29) — the obvious-error review record. Busted/
-- adjusted trades are RETAINED flagged, never deleted (MiFID record-
-- keeping); status lifecycle PENDING_APPROVAL → EXECUTED | REJECTED.
CREATE TABLE trade_busts (
    id              BIGSERIAL PRIMARY KEY,
    trade_id        BIGINT       NOT NULL,               -- logical ref → trades.id (see header)
    action          VARCHAR(12)  NOT NULL
                    CHECK (action IN ('BUST','PRICE_ADJUST')),
    adjusted_price  DECIMAL(20,8),                       -- PRICE_ADJUST only
    reason          VARCHAR(255) NOT NULL,
    reference_price DECIMAL(20,8),                       -- market ref used for the deviation test
    initiated_by    BIGINT       NOT NULL REFERENCES users (id),
    approved_by     BIGINT       REFERENCES users (id),
    status          VARCHAR(16)  NOT NULL DEFAULT 'PENDING_APPROVAL'
                    CHECK (status IN ('PENDING_APPROVAL','EXECUTED','REJECTED')),
    created_at      TIMESTAMPTZ  NOT NULL DEFAULT now(),
    executed_at     TIMESTAMPTZ,
    CONSTRAINT trade_busts_adjust_price_chk
        CHECK (action = 'BUST' OR adjusted_price IS NOT NULL),
    CONSTRAINT trade_busts_distinct_approvers
        CHECK (approved_by IS NULL OR approved_by <> initiated_by),
    CONSTRAINT trade_busts_executed_chk
        CHECK ((status = 'EXECUTED') = (executed_at IS NOT NULL))
);

CREATE INDEX trade_busts_trade_ix ON trade_busts (trade_id);
CREATE INDEX trade_busts_status_ix ON trade_busts (status, created_at);

-- One open review per trade; one executed correction ever.
CREATE UNIQUE INDEX trade_busts_pending_ux
    ON trade_busts (trade_id) WHERE status = 'PENDING_APPROVAL';
CREATE UNIQUE INDEX trade_busts_executed_ux
    ON trade_busts (trade_id) WHERE status = 'EXECUTED';

COMMIT;
