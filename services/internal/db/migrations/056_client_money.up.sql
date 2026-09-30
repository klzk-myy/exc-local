-- 056_client_money.up.sql
-- Phase-24 Task 24.3.11 (client-money segregation & daily reconciliation,
-- spec §5.33/§17.9/§24 #173) + Task 24.3.16 (CLS match discrepancy
-- quarantine, spec §17.12/§24 #326).
--
-- Client money is legally and operationally segregated: every bank/nostro/GL
-- account that can hold balances is classified CLIENT|HOUSE|MARGIN|SUSPENSE
-- here, and safeguarding reconciliation compares requirement (client
-- entitlement + unidentified receipts) to resource (classified segregated
-- balances) each business day, preserving source snapshots and dual-principal
-- sign-off.

BEGIN;

-- ---------------------------------------------------------------------------
-- Account classification (task step 1): one row per safeguardable account.
-- GL-side classification is structural (ledger.SegregationOf ranges); this
-- table is the bank/nostro mirror plus trust/acknowledgement state.
-- ---------------------------------------------------------------------------
CREATE TABLE client_money_accounts (
    id               BIGSERIAL PRIMARY KEY,
    account_kind     VARCHAR(8)  NOT NULL
        CHECK (account_kind IN ('BANK','NOSTRO','GL')),
    nostro_account_id BIGINT REFERENCES nostro_accounts (id),   -- kind=NOSTRO
    gl_account_code   VARCHAR(48),                              -- kind=GL → chart_of_accounts.account_code
    bank_iban         VARCHAR(34),                              -- kind=BANK external segregated account
    bank_name         VARCHAR(128),
    classification   VARCHAR(8)  NOT NULL
        CHECK (classification IN ('CLIENT','HOUSE','MARGIN','SUSPENSE')),
    currency          VARCHAR(3) NOT NULL,
    -- Bank acknowledgement/trust-letter status (CASS-style safeguarding
    -- confirmation that the bank acknowledges client-money trust).
    trust_status      VARCHAR(12) NOT NULL DEFAULT 'NONE'
        CHECK (trust_status IN ('NONE','PENDING','ACKNOWLEDGED')),
    acknowledgement_received_at TIMESTAMPTZ,
    next_due_diligence_at       TIMESTAMPTZ,      -- periodic bank review due
    status            VARCHAR(12) NOT NULL DEFAULT 'ACTIVE'
        CHECK (status IN ('ACTIVE','SUSPENDED','CLOSED')),
    created_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    CHECK (nostro_account_id IS NOT NULL OR gl_account_code IS NOT NULL
           OR bank_iban IS NOT NULL)
);
-- One classification row per underlying account.
CREATE UNIQUE INDEX cma_nostro_ux ON client_money_accounts (nostro_account_id)
    WHERE nostro_account_id IS NOT NULL;
CREATE UNIQUE INDEX cma_gl_ux ON client_money_accounts (gl_account_code)
    WHERE gl_account_code IS NOT NULL;
CREATE INDEX cma_class_ix ON client_money_accounts (classification, currency, status);

-- Bank safeguarding due-diligence / diversification review log (task step 1).
CREATE TABLE client_money_bank_reviews (
    id                     BIGSERIAL PRIMARY KEY,
    client_money_account_id BIGINT NOT NULL REFERENCES client_money_accounts (id),
    review_kind            VARCHAR(16) NOT NULL
        CHECK (review_kind IN ('DUE_DILIGENCE','DIVERSIFICATION','ACKNOWLEDGEMENT')),
    outcome                VARCHAR(16) NOT NULL
        CHECK (outcome IN ('PASS','CONDITIONAL','FAIL')),
    reviewer_id            BIGINT NOT NULL,       -- admin user id
    document_ref           VARCHAR(128),          -- acknowledgement letter / DD pack ref
    notes                  TEXT,
    next_review_at         TIMESTAMPTZ,
    reviewed_at            TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX cmbr_account_ix ON client_money_bank_reviews (client_money_account_id, reviewed_at DESC);

-- ---------------------------------------------------------------------------
-- Receipt allocation (task step 2): bank credits pending client attribution.
-- ---------------------------------------------------------------------------
CREATE TABLE client_money_receipts (
    id                BIGSERIAL PRIMARY KEY,
    nostro_account_id BIGINT REFERENCES nostro_accounts (id),
    bank_reference    VARCHAR(64) NOT NULL,
    amount            DECIMAL(28,8) NOT NULL CHECK (amount > 0),
    currency          VARCHAR(3) NOT NULL,
    status            VARCHAR(14) NOT NULL DEFAULT 'UNIDENTIFIED'
        CHECK (status IN ('UNIDENTIFIED','ALLOCATED','RETURNED')),
    allocated_account_id BIGINT,                -- client accounts.id once matched
    allocated_by      BIGINT,                   -- admin user id (four-eyes audit)
    allocated_at      TIMESTAMPTZ,
    cleared_at        TIMESTAMPTZ,              -- NULL = uncleared item in recon
    received_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    created_at        TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX cmr_status_ix ON client_money_receipts (status, currency);
CREATE UNIQUE INDEX cmr_bankref_ux ON client_money_receipts (nostro_account_id, bank_reference);

-- ---------------------------------------------------------------------------
-- Daily safeguarding reconciliation (task step 3). One row per
-- (recon_date, currency); snapshots preserve the source state at run time.
-- ---------------------------------------------------------------------------
CREATE TABLE client_money_reconciliations (
    id                 BIGSERIAL PRIMARY KEY,
    recon_date         DATE NOT NULL,
    currency           VARCHAR(3) NOT NULL,
    requirement        DECIMAL(28,8) NOT NULL,   -- client entitlement + unidentified receipts
    resource           DECIMAL(28,8) NOT NULL,   -- CLIENT+MARGIN classified segregated balances
    uncleared_receipts DECIMAL(28,8) NOT NULL DEFAULT 0,
    margin_transfers   DECIMAL(28,8) NOT NULL DEFAULT 0, -- MARGIN-classified subset of resource
    variance           DECIMAL(28,8) NOT NULL,   -- resource - requirement (<0 shortfall)
    status             VARCHAR(16) NOT NULL
        CHECK (status IN ('BALANCED','SHORTFALL','EXCESS')),
    external_status    VARCHAR(16) NOT NULL DEFAULT 'UNAVAILABLE'
        CHECK (external_status IN ('MATCHED','MISMATCH','UNAVAILABLE')),
    internal_snapshot  JSONB NOT NULL,           -- per-client entitlement rows + inputs
    external_snapshot  JSONB,                    -- per-bank statement comparison rows
    performed_by       BIGINT NOT NULL,
    signed_off_by      BIGINT,                   -- distinct second principal
    signed_off_at      TIMESTAMPTZ,
    created_at         TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (recon_date, currency)
);
CREATE INDEX cmrec_status_ix ON client_money_reconciliations (status, recon_date DESC);

-- ---------------------------------------------------------------------------
-- Breaks (task step 4): shortfall/excess/unmatched records.
-- ---------------------------------------------------------------------------
CREATE TABLE client_money_breaks (
    id                BIGSERIAL PRIMARY KEY,
    reconciliation_id BIGINT REFERENCES client_money_reconciliations (id),
    kind              VARCHAR(20) NOT NULL
        CHECK (kind IN ('SHORTFALL','EXCESS','UNMATCHED','STRESS_BREACH')),
    currency          VARCHAR(3) NOT NULL,
    amount            DECIMAL(28,8) NOT NULL CHECK (amount > 0),
    status            VARCHAR(14) NOT NULL DEFAULT 'OPEN'
        CHECK (status IN ('OPEN','REMEDIATING','RESOLVED','ESCALATED')),
    severity          VARCHAR(4) NOT NULL DEFAULT 'P1'
        CHECK (severity IN ('P0','P1','P2')),
    detected_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    resolved_at       TIMESTAMPTZ,
    detail            JSONB,
    created_at        TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX cmb_open_ix ON client_money_breaks (status, kind, currency);

-- ---------------------------------------------------------------------------
-- Remediation / top-up log — the 4-tier waterfall (task step 4a):
--   1 INSURANCE_DEBIT   automatic, immediate
--   2 HOUSE_TOPUP       automatic, 4-eyes approval, ≤30 min
--   3 CAPITAL_CALL      manual, regulator notification ≤1 business day
--                      (AC: automated notice dispatched ≤60 min)
--   4 DEFAULT_DECLARE   orderly suspension + regulatory default declaration
--                      when tiers 1–3 insufficient within 4h
-- Every executed remediation posts a balanced GL journal (journal_entry_id).
-- ---------------------------------------------------------------------------
CREATE TABLE client_money_remediations (
    id              BIGSERIAL PRIMARY KEY,
    break_id        BIGINT NOT NULL REFERENCES client_money_breaks (id),
    tier            SMALLINT NOT NULL CHECK (tier BETWEEN 1 AND 4),
    action          VARCHAR(20) NOT NULL
        CHECK (action IN ('INSURANCE_DEBIT','HOUSE_TOPUP','CAPITAL_CALL','DEFAULT_DECLARE')),
    currency        VARCHAR(3) NOT NULL,
    amount          DECIMAL(28,8) NOT NULL CHECK (amount >= 0),
    status          VARCHAR(12) NOT NULL DEFAULT 'PENDING'
        CHECK (status IN ('PENDING','APPROVED','EXECUTED','FAILED','EXPIRED')),
    requested_by    BIGINT NOT NULL,            -- system actor id or admin
    approved_by     BIGINT,                     -- distinct approver (tiers 2–4)
    journal_entry_id BIGINT,                    -- gl journal posted on execution
    deadline_at     TIMESTAMPTZ NOT NULL,       -- tier SLA bound
    executed_at     TIMESTAMPTZ,
    failure_reason  TEXT,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX cmrem_break_ix ON client_money_remediations (break_id, tier);
CREATE INDEX cmrem_pending_ix ON client_money_remediations (status, deadline_at)
    WHERE status = 'PENDING';

-- Regulator notification records (task steps 4/4a + Task 24.3.16 item 3).
CREATE TABLE client_money_regulator_notices (
    id          BIGSERIAL PRIMARY KEY,
    break_id    BIGINT REFERENCES client_money_breaks (id),
    remediation_id BIGINT REFERENCES client_money_remediations (id),
    trigger     VARCHAR(20) NOT NULL
        CHECK (trigger IN ('TIER3_CAPITAL_CALL','TIER4_DEFAULT','SHORTFALL_OVER_60M')),
    regulation  VARCHAR(40) NOT NULL,           -- 'CASS 7.15.33' / 'SEC 15c3-3'
    status      VARCHAR(12) NOT NULL DEFAULT 'PENDING'
        CHECK (status IN ('PENDING','SENT','ACKNOWLEDGED')),
    deadline_at TIMESTAMPTZ NOT NULL,           -- ≤60 min from detection (AC #6)
    sent_at     TIMESTAMPTZ,
    acknowledged_at TIMESTAMPTZ,
    payload     JSONB,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX cmrn_pending_ix ON client_money_regulator_notices (status, deadline_at)
    WHERE status = 'PENDING';

-- Daily stress-test log (task step 4a): insurance fund + house reserves
-- must cover ≥ 2× worst-case NBP exposure per currency.
CREATE TABLE client_money_stress_runs (
    id                   BIGSERIAL PRIMARY KEY,
    run_date             DATE NOT NULL,
    currency             VARCHAR(3) NOT NULL,
    insurance_balance    DECIMAL(28,8) NOT NULL,
    house_reserve        DECIMAL(28,8) NOT NULL,
    worst_case_nbp       DECIMAL(28,8) NOT NULL,
    required_coverage    DECIMAL(28,8) NOT NULL,  -- 2× worst_case_nbp
    adequate             BOOLEAN NOT NULL,
    created_at           TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (run_date, currency)
);

-- Primary-pooling-event / wind-down package export log (task step 5).
CREATE TABLE client_money_pooling_exports (
    id           BIGSERIAL PRIMARY KEY,
    export_kind  VARCHAR(16) NOT NULL
        CHECK (export_kind IN ('POOLING_EVENT','WIND_DOWN')),
    exported_by  BIGINT NOT NULL,
    package      JSONB NOT NULL,                -- entitlements, accounts, breaks, contacts, workflow
    package_sha256 CHAR(64) NOT NULL,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- ---------------------------------------------------------------------------
-- Task 24.3.16: settlement batch quarantine on CLS match discrepancy.
-- Outbound funds are blocked while outbound_blocked is true; release needs
-- a distinct affirming principal (dual control).
-- ---------------------------------------------------------------------------
CREATE TABLE settlement_quarantines (
    id               BIGSERIAL PRIMARY KEY,
    batch_ref        VARCHAR(64) NOT NULL,
    reason_code      VARCHAR(32) NOT NULL DEFAULT 'CLS_SETTLEMENT_MISMATCH',
    discrepancy      JSONB NOT NULL,             -- differing amounts/currency/date drift
    outbound_blocked BOOLEAN NOT NULL DEFAULT TRUE,
    status           VARCHAR(12) NOT NULL DEFAULT 'QUARANTINED'
        CHECK (status IN ('QUARANTINED','RELEASED')),
    quarantined_by   BIGINT NOT NULL,
    released_by      BIGINT,
    release_approved_by BIGINT,                  -- distinct second principal
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    released_at      TIMESTAMPTZ
);
CREATE INDEX sq_active_ix ON settlement_quarantines (batch_ref)
    WHERE status = 'QUARANTINED';

COMMIT;
