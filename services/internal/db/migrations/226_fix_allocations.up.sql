-- 226_fix_allocations.up.sql
-- Phase-18 Task 18.3.13 FIX AllocationInstruction (35=J) / AllocationReport
-- (35=AK) persistence: instruction headers, per-leg booking records with
-- parent_exec_id linkage, and the append-only allocation_events audit trail.
--
-- Relationship to spec §5.31: `average_price_groups` + `trade_allocations`
-- (migration 055, Phase-24 Task 24.3.10) are the SETTLEMENT-side allocation
-- records built on top of this FIX-layer intake; the names here are
-- deliberately distinct so the two schemas never collide. Phase-24 consumes
-- `fix_allocation_legs` (settlement_locked flips when the settlement engine
-- claims a leg; REPLACE/CANCEL is rejected once locked).
--
-- Immutability: allocation_events is append-only — an UPDATE/DELETE trigger
-- raises ALLOCATION_AUDIT_IMMUTABLE (§2.7 fail-closed; corrections are
-- recorded as superseding events, never edits).

BEGIN;

CREATE TYPE fix_alloc_method_enum  AS ENUM ('PRO_RATA', 'MANUAL', 'STEP_OUT');
CREATE TYPE fix_alloc_status_enum  AS ENUM ('RECEIVED', 'ACCEPTED', 'REJECTED', 'REPLACED', 'CANCELLED');
CREATE TYPE alloc_leg_status_enum  AS ENUM ('BOOKED', 'REJECTED', 'CANCELLED', 'EXTERNAL_BOOKED');

CREATE TABLE fix_allocations (
    id                 BIGSERIAL PRIMARY KEY,
    alloc_id           VARCHAR(64) NOT NULL UNIQUE,       -- Tag 70, client-supplied
    ref_alloc_id       VARCHAR(64),                        -- Tag 72, REPLACE/CANCEL target
    alloc_trans_type   SMALLINT    NOT NULL CHECK (alloc_trans_type IN (0, 1, 2)), -- Tag 71: 0=NEW 1=REPLACE 2=CANCEL
    method             fix_alloc_method_enum NOT NULL,
    status             fix_alloc_status_enum NOT NULL DEFAULT 'RECEIVED',
    session_id         VARCHAR(64) NOT NULL,               -- submitting FIX session
    master_account_id  BIGINT      NOT NULL REFERENCES accounts (id),
    symbol             VARCHAR(32) NOT NULL,
    side               CHAR(1),                            -- Tag 54: '1'=BUY '2'=SELL
    exec_qty           DECIMAL(28,8) NOT NULL,             -- referenced executed quantity
    exec_refs          JSONB       NOT NULL,               -- [{exec_id,order_id}] Tag 17/37 refs
    settlement_locked  BOOLEAN     NOT NULL DEFAULT FALSE, -- Phase-24 settlement claim latch
    reject_reason      VARCHAR(255),
    created_at         TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at         TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX fix_allocations_master_ix ON fix_allocations (master_account_id);
CREATE INDEX fix_allocations_status_ix ON fix_allocations (status);

CREATE TABLE fix_allocation_legs (
    id               BIGSERIAL PRIMARY KEY,
    allocation_id    BIGINT        NOT NULL REFERENCES fix_allocations (id),
    leg_no           INTEGER       NOT NULL,
    alloc_account    VARCHAR(64)   NOT NULL,               -- Tag 79 verbatim
    alloc_account_id BIGINT        REFERENCES accounts (id), -- resolved sub-account
    alloc_qty        DECIMAL(28,8) NOT NULL CHECK (alloc_qty >= 0),
    alloc_price      DECIMAL(20,8),
    step_out_broker  VARCHAR(64),                          -- STEP_OUT external broker mnemonic
    leg_status       alloc_leg_status_enum NOT NULL DEFAULT 'BOOKED',
    parent_exec_id   VARCHAR(64),                          -- source fill linkage
    child_exec_id    VARCHAR(64)   UNIQUE,                 -- generated booking ref ALLOC-{id}-{leg}
    created_at       TIMESTAMPTZ   NOT NULL DEFAULT now(),
    UNIQUE (allocation_id, leg_no)
);
CREATE INDEX fix_allocation_legs_account_ix ON fix_allocation_legs (alloc_account_id);

-- Append-only audit trail: every instruction/state transition lands as an
-- immutable event row; seq is per-allocation monotonic.
CREATE TABLE allocation_events (
    id            BIGSERIAL PRIMARY KEY,
    allocation_id BIGINT       NOT NULL REFERENCES fix_allocations (id),
    seq           INTEGER      NOT NULL,
    event_type    VARCHAR(32)  NOT NULL,
    payload       JSONB        NOT NULL,
    actor         VARCHAR(64),                             -- FIX session id or service identity
    created_at    TIMESTAMPTZ  NOT NULL DEFAULT now(),
    UNIQUE (allocation_id, seq)
);

CREATE OR REPLACE FUNCTION allocation_events_immutable() RETURNS trigger AS $$
BEGIN
    RAISE EXCEPTION 'ALLOCATION_AUDIT_IMMUTABLE: allocation_events is append-only (corrections are superseding events, never edits)';
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER allocation_events_no_update
    BEFORE UPDATE OR DELETE ON allocation_events
    FOR EACH ROW EXECUTE FUNCTION allocation_events_immutable();

COMMIT;
