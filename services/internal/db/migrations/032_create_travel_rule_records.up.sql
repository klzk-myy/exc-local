-- 032_create_travel_rule_records.up.sql
-- Phase-21 Task 21.3.2 — FATF Travel Rule (Recommendation 16 / IVMS 101)
-- for fiat transfers >= $1,000 (spec §14.3, §5.19; §24 #107; §27.1
-- Travel Rule matrix row).
--
-- One row per (funding transaction, direction). Outbound rows are
-- written by the dispatch gate (funding.DispatchService →
-- compliance.TravelRuleService.EnforceOutbound) before the rail
-- envelope is built; inbound rows land at dual-source confirmation
-- (funding.DepositService → TravelRuleService.CheckInbound).
--
--   originator   JSONB {name, account_number, address, country} — the
--                required R.16 originator set. For outbound wires the
--                originator is our client (fields 50K); for inbound it
--                is the sender carried on the wire confirmation.
--   beneficiary  JSONB {name, account_number} — field 59 on SWIFT.
--   missing_fields carries every absent required member; status
--   MISSING_INFO holds the transfer (outbound: dispatch queue HELD /
--   inbound: deposit PENDING_REVIEW) until an officer supplies the data
--   via POST /api/v1/admin/travel-rule/{id}/supply.
--
-- Regulated-record posture: no DELETE path — rows persist under the
-- §19.12 compliance retention horizon; down-migration drops the table.

BEGIN;

CREATE TABLE travel_rule_records (
    id             BIGSERIAL    PRIMARY KEY,
    transfer_id    BIGINT       NOT NULL REFERENCES funding_transactions (id),
    direction      VARCHAR(8)   NOT NULL CHECK (direction IN ('INBOUND','OUTBOUND')),
    account_id     BIGINT       NOT NULL REFERENCES accounts (id),
    rail           VARCHAR(16),                          -- SWIFT | SEPA | FEDNOW | ACH | CHAPS | TARGET2
    currency       VARCHAR(3)   NOT NULL,
    amount         NUMERIC(28,8) NOT NULL,
    usd_amount     NUMERIC(28,8),                        -- converted when priced; NULL = unpriced non-USD
    threshold_usd  NUMERIC(28,8) NOT NULL DEFAULT 1000,  -- recorded threshold in force at capture
    originator     JSONB        NOT NULL DEFAULT '{}',   -- {name, account_number, address, country}
    beneficiary    JSONB        NOT NULL DEFAULT '{}',   -- {name, account_number}
    status         VARCHAR(16)  NOT NULL DEFAULT 'MISSING_INFO'
                   CHECK (status IN ('MISSING_INFO','COMPLETE','REJECTED')),
    missing_fields TEXT[]       NOT NULL DEFAULT '{}',
    swift_field_ref VARCHAR(64),                         -- 'MT103:50K/59' | 'pacs.008:Dbtr/Cdtr' | ...
    hold_ref       VARCHAR(64),                          -- queue/hold reference when a hold was applied
    supplied_by    BIGINT,                               -- officer user id that cured MISSING_INFO
    created_at     TIMESTAMPTZ  NOT NULL DEFAULT now(),
    updated_at     TIMESTAMPTZ  NOT NULL DEFAULT now()
);

-- Idempotent capture: one record per transfer per direction. The gate
-- re-evaluates the same row on retry (stored fields win over
-- re-resolution so officer-supplied data is never overwritten).
CREATE UNIQUE INDEX uq_travel_rule_transfer
    ON travel_rule_records (transfer_id, direction);

-- Review surfaces: missing-info queue, per-account audit trail.
CREATE INDEX ix_travel_rule_status
    ON travel_rule_records (status) WHERE status = 'MISSING_INFO';
CREATE INDEX ix_travel_rule_account
    ON travel_rule_records (account_id, created_at DESC);

COMMIT;
