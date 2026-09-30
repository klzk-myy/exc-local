-- 055_trade_allocations.up.sql
-- Phase-24 Tasks 24.3.10 / 24.3.15 — bunched-order average-price
-- allocation and post-trade block-trade allocation (spec §5.31, §17.8,
-- §24 #172/#237).
--
-- Objects:
--   average_price_groups          — the pre-declared bunched-order group:
--                                   manager omnibus account, instrument,
--                                   side, capacity (CLIENT|PROPRIETARY),
--                                   allocation method (MANUAL |
--                                   PRO_RATA | RULE_BASED | EQUAL_SPLIT),
--                                   computed VWAP + fill totals, and the
--                                   settlement_locked latch flipped when
--                                   the group is submitted to settlement.
--   average_price_group_accounts  — eligible beneficial accounts
--                                   registered BEFORE order entry, with
--                                   declared capacity, optional split
--                                   weight (RULE_BASED/PRO_RATA presets)
--                                   and PartyID/LEI identifiers carried
--                                   to confirmations/PB/reporting.
--   average_price_group_fills     — the grouped source fills (trades.id
--                                   logical ref; partitioned parent, same
--                                   discipline as settlement_instructions
--                                   migration 019). A fill joins at most
--                                   one group (UNIQUE trade_id) so filled
--                                   quantity can never be double-allocated.
--   trade_allocations             — the beneficiary split per §5.31:
--                                   (trade_id, group_id, beneficiary,
--                                   quantity, avg_price, status,
--                                   claimed_at) plus the correction
--                                   columns (kind PRIMARY|OFFSET|
--                                   REPLACEMENT, corrects_allocation_id),
--                                   the confirmation/settlement links, and
--                                   the PartyID/LEI propagation fields.
--   trade_allocation_events       — append-only audit history per §5.31;
--                                   every lifecycle transition writes a
--                                   row carrying immutable before/after
--                                   JSONB evidence and both principals on
--                                   dual-controlled corrections.
--
-- DB-enforced invariants (fail-closed, spec §2.7):
--   * average_price_groups CHECK: allocated_qty <= total_qty — the group's
--     allocated total can never exceed grouped fill quantity.
--   * apg_account_capacity_chk trigger: an eligible account whose declared
--     capacity differs from its group's capacity is refused — proprietary
--     and client interest can never share a group.
--   * trade_allocations_conservation_chk trigger: the sum of ACTIVE
--     allocations (PRIMARY/REPLACEMENT, non-terminal status) per fill can
--     never exceed that fill's attached quantity — allocated <= filled.
--   * trade_allocation_events is append-only (corrections are superseding
--     events, never edits) — same latch as migration 226.
--
-- chart_of_accounts: seeds 2090_BLOCK_ALLOCATION_CLEARING_{CCY} — the
-- dedicated clearing liability every confirmed block allocation debits/
-- credits against (Task 24.3.15 step 6; net GL impact zero, parent trade
-- audit preserved). The code builder lives in
-- internal/backoffice/allocation_engine.go (BlockAllocationClearing);
-- the canonical chart list is internal/ledger/chart.go — a later
-- reconciliation may add it there; this seed stands alone meanwhile.

BEGIN;

CREATE TYPE alloc_capacity_enum  AS ENUM ('CLIENT', 'PROPRIETARY');
CREATE TYPE alloc_method_enum    AS ENUM ('MANUAL', 'PRO_RATA', 'RULE_BASED', 'EQUAL_SPLIT');
CREATE TYPE avg_group_status_enum AS ENUM
    ('OPEN', 'ALLOCATED', 'SETTLEMENT_LOCKED', 'CLOSED', 'CANCELLED');
CREATE TYPE trade_alloc_status_enum AS ENUM
    ('PENDING', 'ALLOCATED', 'CLAIMED', 'REJECTED', 'CANCELLED', 'CORRECTED', 'SETTLED');
CREATE TYPE trade_alloc_kind_enum AS ENUM ('PRIMARY', 'OFFSET', 'REPLACEMENT');
CREATE TYPE alloc_source_enum    AS ENUM ('REST', 'FIX');

CREATE TABLE average_price_groups (
    id                  BIGSERIAL PRIMARY KEY,
    group_ref           VARCHAR(64)  NOT NULL UNIQUE,      -- client/PM idempotency ref
    manager_account_id  BIGINT       NOT NULL REFERENCES accounts (id),
    instrument_id       BIGINT       NOT NULL REFERENCES instruments (id),
    side                CHAR(1)      NOT NULL CHECK (side IN ('1','2')),  -- FIX 54: 1=BUY 2=SELL
    capacity            alloc_capacity_enum NOT NULL,
    allocation_method   alloc_method_enum   NOT NULL,
    status              avg_group_status_enum NOT NULL DEFAULT 'OPEN',
    total_qty           DECIMAL(28,8) NOT NULL DEFAULT 0,   -- grouped filled quantity
    allocated_qty       DECIMAL(28,8) NOT NULL DEFAULT 0,   -- Σ active allocations
    avg_price           DECIMAL(28,8),                      -- VWAP over grouped fills
    price_residual      DECIMAL(28,8) NOT NULL DEFAULT 0,   -- Σp·q − allocated_qty·avg (deterministic remainder evidence)
    settlement_locked   BOOLEAN       NOT NULL DEFAULT FALSE,
    source              alloc_source_enum NOT NULL DEFAULT 'REST',
    fix_allocation_id   BIGINT,                             -- fix_allocations.id (35=J intake)
    escalated_at        TIMESTAMPTZ,                        -- T+0 EOD compliance-escalation latch
    created_by          BIGINT,
    created_at          TIMESTAMPTZ   NOT NULL DEFAULT now(),
    updated_at          TIMESTAMPTZ   NOT NULL DEFAULT now(),
    submitted_at        TIMESTAMPTZ,
    CHECK (allocated_qty >= 0),
    CHECK (allocated_qty <= total_qty)                      -- never over-allocate a group
);
CREATE INDEX avg_price_groups_manager_ix ON average_price_groups (manager_account_id);
CREATE INDEX avg_price_groups_status_ix  ON average_price_groups (status);
CREATE INDEX avg_price_groups_unallocated_ix ON average_price_groups (created_at)
    WHERE status IN ('OPEN','ALLOCATED') AND escalated_at IS NULL;

CREATE TABLE average_price_group_accounts (
    id                      BIGSERIAL PRIMARY KEY,
    group_id                BIGINT       NOT NULL REFERENCES average_price_groups (id) ON DELETE CASCADE,
    beneficiary_account_id  BIGINT       NOT NULL REFERENCES accounts (id),
    capacity                alloc_capacity_enum NOT NULL,  -- declared capacity; must equal group's
    weight                  DECIMAL(28,12) CHECK (weight IS NULL OR weight > 0), -- RULE_BASED/PRO_RATA preset
    party_id                VARCHAR(64),                    -- FIX Parties(448) id
    party_lei               VARCHAR(20),                    -- ISO 17442 LEI
    status                  VARCHAR(16)  NOT NULL DEFAULT 'ELIGIBLE'
                            CHECK (status IN ('ELIGIBLE','INELIGIBLE')),
    created_at              TIMESTAMPTZ  NOT NULL DEFAULT now(),
    UNIQUE (group_id, beneficiary_account_id)
);
CREATE INDEX avg_price_group_accounts_acct_ix
    ON average_price_group_accounts (beneficiary_account_id);

-- Capacity-mixing guard: the eligible account's declared capacity must
-- equal the group's; a CLIENT group can never carry a PROPRIETARY leg and
-- vice versa (Task 24.3.10 step 1).
CREATE OR REPLACE FUNCTION apg_account_capacity_chk() RETURNS trigger AS $$
DECLARE grp_cap alloc_capacity_enum;
BEGIN
    SELECT capacity INTO grp_cap FROM average_price_groups WHERE id = NEW.group_id;
    IF grp_cap IS NULL THEN
        RAISE EXCEPTION 'ALLOCATION_INVALID: group % not found', NEW.group_id;
    END IF;
    IF NEW.capacity <> grp_cap THEN
        RAISE EXCEPTION 'ALLOCATION_CAPACITY_MIX: account % capacity % <> group % capacity %',
            NEW.beneficiary_account_id, NEW.capacity, NEW.group_id, grp_cap;
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER apg_account_capacity_chk
    BEFORE INSERT OR UPDATE ON average_price_group_accounts
    FOR EACH ROW EXECUTE FUNCTION apg_account_capacity_chk();

CREATE TABLE average_price_group_fills (
    id               BIGSERIAL PRIMARY KEY,
    group_id         BIGINT        NOT NULL REFERENCES average_price_groups (id),
    trade_id         BIGINT        NOT NULL,                -- logical ref → trades.id (partitioned parent)
    account_id       BIGINT        NOT NULL,                -- manager omnibus account on the fill
    quantity         DECIMAL(28,8) NOT NULL CHECK (quantity > 0),
    price            DECIMAL(20,8) NOT NULL CHECK (price > 0),
    settlement_date  DATE,                                  -- parent value date (inherited by children)
    created_at       TIMESTAMPTZ   NOT NULL DEFAULT now(),
    UNIQUE (trade_id)                                       -- a fill joins at most one group
);
CREATE INDEX avg_price_group_fills_group_ix ON average_price_group_fills (group_id);

CREATE TABLE trade_allocations (
    id                          BIGSERIAL PRIMARY KEY,
    trade_id                    BIGINT        NOT NULL,     -- source fill (logical ref → trades.id)
    group_id                    BIGINT        NOT NULL REFERENCES average_price_groups (id),
    beneficiary_account_id      BIGINT        NOT NULL REFERENCES accounts (id),
    quantity                    DECIMAL(28,8) NOT NULL CHECK (quantity > 0),
    avg_price                   DECIMAL(28,8) NOT NULL,
    status                      trade_alloc_status_enum NOT NULL DEFAULT 'PENDING',
    kind                        trade_alloc_kind_enum   NOT NULL DEFAULT 'PRIMARY',
    corrects_allocation_id      BIGINT        REFERENCES trade_allocations (id),
    party_id                    VARCHAR(64),
    party_lei                   VARCHAR(20),
    alloc_account_ref           VARCHAR(64),              -- FIX AllocAccount(79) verbatim / fund ref
    confirmation_ref            VARCHAR(64),              -- 35=AK AllocReportID / fund-ops ref
    settlement_instruction_ids  JSONB         NOT NULL DEFAULT '[]'::jsonb, -- child settlement_instructions ids
    claimed_at                  TIMESTAMPTZ,
    rejected_reason             VARCHAR(255),
    created_at                  TIMESTAMPTZ   NOT NULL DEFAULT now(),
    updated_at                  TIMESTAMPTZ   NOT NULL DEFAULT now()
);
CREATE INDEX trade_allocations_group_ix   ON trade_allocations (group_id);
CREATE INDEX trade_allocations_trade_ix   ON trade_allocations (trade_id);
CREATE INDEX trade_allocations_account_ix ON trade_allocations (beneficiary_account_id);
CREATE INDEX trade_allocations_status_ix  ON trade_allocations (status);

-- Conservation guard: ACTIVE allocations (PRIMARY/REPLACEMENT not yet
-- terminal) against one fill may never exceed the fill's attached quantity
-- — "allocated quantity may never exceed filled quantity" enforced at the
-- DB layer as well as in the engine.
CREATE OR REPLACE FUNCTION trade_allocations_conservation_chk() RETURNS trigger AS $$
DECLARE fill_qty DECIMAL(28,8); active_qty DECIMAL(28,8);
BEGIN
    SELECT quantity INTO fill_qty
      FROM average_price_group_fills
     WHERE group_id = NEW.group_id AND trade_id = NEW.trade_id;
    IF fill_qty IS NULL THEN
        RAISE EXCEPTION 'ALLOCATION_INVALID: fill % is not attached to group %',
            NEW.trade_id, NEW.group_id;
    END IF;
    SELECT COALESCE(SUM(quantity),0) INTO active_qty
      FROM trade_allocations
     WHERE group_id = NEW.group_id AND trade_id = NEW.trade_id
       AND kind IN ('PRIMARY','REPLACEMENT')
       AND status IN ('PENDING','ALLOCATED','CLAIMED','SETTLED');
    IF active_qty > fill_qty THEN
        RAISE EXCEPTION 'ALLOCATION_SUM_MISMATCH: active allocation % exceeds fill % quantity %',
            active_qty, NEW.trade_id, fill_qty;
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER trade_allocations_conservation_chk
    AFTER INSERT OR UPDATE ON trade_allocations
    FOR EACH ROW EXECUTE FUNCTION trade_allocations_conservation_chk();

-- Append-only audit history (spec §5.31): lifecycle transitions with
-- immutable before/after evidence; approved_by carries the second
-- principal on dual-controlled corrections.
CREATE TABLE trade_allocation_events (
    id            BIGSERIAL PRIMARY KEY,
    group_id      BIGINT       NOT NULL REFERENCES average_price_groups (id),
    allocation_id BIGINT       REFERENCES trade_allocations (id),
    seq           INTEGER      NOT NULL,
    event_type    VARCHAR(32)  NOT NULL,
    before_state  JSONB,
    after_state   JSONB        NOT NULL,
    actor         VARCHAR(64),
    approved_by   BIGINT,
    created_at    TIMESTAMPTZ  NOT NULL DEFAULT now(),
    UNIQUE (group_id, seq)
);

CREATE OR REPLACE FUNCTION trade_allocation_events_immutable() RETURNS trigger AS $$
BEGIN
    RAISE EXCEPTION 'ALLOCATION_AUDIT_IMMUTABLE: trade_allocation_events is append-only (corrections are superseding events, never edits)';
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER trade_allocation_events_no_update
    BEFORE UPDATE OR DELETE ON trade_allocation_events
    FOR EACH ROW EXECUTE FUNCTION trade_allocation_events_immutable();

-- 2090_BLOCK_ALLOCATION_CLEARING — dedicated clearing liability for
-- block-trade allocation rebooking (Task 24.3.15 step 6). Same currency
-- sweep as migrations 036/088/110/216.
INSERT INTO chart_of_accounts (account_code, account_name, account_type, currency)
SELECT '2090_BLOCK_ALLOCATION_CLEARING_' || c,
       'Block-allocation clearing (' || c || ')', 'LIABILITY', c
FROM (VALUES ('USD'),('EUR'),('GBP'),('JPY'),('AUD'),('CAD'),('CHF'),('NZD'),('MXN')) AS v(c)
ON CONFLICT (account_code) DO NOTHING;

COMMIT;
