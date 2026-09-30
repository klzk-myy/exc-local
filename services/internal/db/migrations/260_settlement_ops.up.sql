-- 260_settlement_ops.up.sql
-- Phase-24 settlement-ops schema for Tasks 24.3.6 (failed settlement
-- exceptions), 24.3.7 (PB give-up reconciliation + atomic collateral
-- rebalance), 24.3.14 (PB credit restitution journal) and 24.3.19
-- (settlement ops hardening: write-offs, recon tolerances, nostro funding
-- thresholds, rail cut-off matrix, rail failover queue, Herstatt exposure,
-- CLS pay-in ops, FX fail close-outs, LP-default escalations).
--
-- `settlement_exceptions` is the shared break-investigation queue —
-- Task 24.3.12's statement parser (sibling task) routes unmatched bank
-- entries here too (UNMATCHED_STATEMENT/AMOUNT_MISMATCH/MISSING_PAYMENT/
-- UNEXPECTED_CREDIT), so the shape deliberately tolerates a NULL
-- settlement_instruction_id.
--
-- GL chart additions per currency: 5990_SETTLEMENT_WRITE_OFF (write-off
-- expense, §17.14.1 authority matrix), 4600_FAIL_INTEREST_REVENUE
-- (§17.14 fail-interest contra), 1020_SETTLEMENT_FAIL_CLAIM (buy-in
-- differential / FX replacement-cost claim on the failing counterparty),
-- 1090_SETTLEMENT_FAIL_MEMO + 2090_PB_CREDIT_MEMO (zero-net memo pair for
-- the Task 24.3.14 DSL restitution journal — a utilization correction,
-- not a cash movement).
--
-- Task 24.3.6 reversal needs a terminal leg state: 'VOID' is added to
-- settlement_status_enum (PG12+ allows ADD VALUE inside a transaction;
-- the value is not used by this migration itself).

BEGIN;

ALTER TYPE settlement_status_enum ADD VALUE IF NOT EXISTS 'VOID';

INSERT INTO chart_of_accounts (account_code, account_name, account_type, currency)
SELECT fmt.code || '_' || c.ccy, fmt.name || ' (' || c.ccy || ')', fmt.typ::gl_account_type_enum, c.ccy
FROM (VALUES
    ('USD'), ('EUR'), ('GBP'), ('JPY'), ('AUD'),
    ('CAD'), ('CHF'), ('NZD'), ('MXN')
) AS c (ccy)
CROSS JOIN (VALUES
    ('5990_SETTLEMENT_WRITE_OFF', 'Settlement break write-off expense (§17.14)',  'EXPENSE'),
    ('4600_FAIL_INTEREST_REVENUE','Settlement fail-interest income (§17.14.4)',   'REVENUE'),
    ('1020_SETTLEMENT_FAIL_CLAIM','Buy-in/FX close-out claim on failing party',   'ASSET'),
    ('1090_SETTLEMENT_FAIL_MEMO', 'Settlement-fail control memo (non-cash)',      'ASSET'),
    ('2090_PB_CREDIT_MEMO',       'PB credit restitution memo (non-cash)',        'LIABILITY')
) AS fmt (code, name, typ)
ON CONFLICT (account_code) DO NOTHING;

-- ── Task 24.3.6 — settlement exceptions + audit trail ────────────────────
CREATE TABLE settlement_exceptions (
    id                BIGSERIAL PRIMARY KEY,
    settlement_instruction_id BIGINT REFERENCES settlement_instructions (id),
    trade_id          BIGINT,
    account_id        BIGINT,
    currency          VARCHAR(3),
    amount            DECIMAL(28,8),
    exception_type    VARCHAR(32) NOT NULL CHECK (exception_type IN
        ('SWIFT_REJECTION','INSUFFICIENT_NOSTRO','COUNTERPARTY',
         'UNMATCHED_STATEMENT','AMOUNT_MISMATCH','MISSING_PAYMENT',
         'UNEXPECTED_CREDIT','CLS_MISMATCH','OTHER')),
    status            VARCHAR(20) NOT NULL DEFAULT 'OPEN' CHECK (status IN
        ('OPEN','INVESTIGATING','RESOLVED_RETRY','RESOLVED_REVERSED',
         'RESOLVED_MANUAL','WRITTEN_OFF')),
    detected_by       VARCHAR(64) NOT NULL,            -- source component
    detail            TEXT,
    assigned_to       BIGINT,                          -- finance-ops assignee
    resolution_action VARCHAR(12) CHECK (resolution_action IN
        ('RETRY','REVERSE','MANUAL','WRITE_OFF')),
    resolution_notes  TEXT,
    resolved_by       BIGINT,                          -- maker
    approved_by       BIGINT,                          -- checker (dual control)
    dual_control_request_id BIGINT,
    reversal_journal_id BIGINT REFERENCES journal_entries (id),
    detected_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    resolved_at       TIMESTAMPTZ,
    created_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at        TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX settlement_exceptions_open_ix
    ON settlement_exceptions (detected_at)
    WHERE status IN ('OPEN','INVESTIGATING');
CREATE INDEX settlement_exceptions_instruction_ix
    ON settlement_exceptions (settlement_instruction_id)
    WHERE settlement_instruction_id IS NOT NULL;

CREATE TABLE settlement_exception_events (
    id           BIGSERIAL PRIMARY KEY,
    exception_id BIGINT      NOT NULL REFERENCES settlement_exceptions (id),
    actor_id     BIGINT,                                -- NULL = system
    action       VARCHAR(48) NOT NULL,                  -- detect|investigate|resolve|retry|reverse|write_off|assign
    detail       JSONB,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX settlement_exception_events_ix
    ON settlement_exception_events (exception_id, created_at);

-- ── Task 24.3.7 — PB give-up reconciliation ──────────────────────────────
CREATE TABLE pb_recon_runs (
    id              BIGSERIAL PRIMARY KEY,
    prime_broker_id BIGINT       NOT NULL REFERENCES prime_brokers (id),
    run_date        DATE         NOT NULL,
    source          VARCHAR(16)  NOT NULL CHECK (source IN ('AFFIRMATION','BLOTTER')),
    trades_scanned  INT          NOT NULL DEFAULT 0,
    auto_matched    INT          NOT NULL DEFAULT 0,
    breaks_detected INT          NOT NULL DEFAULT 0,
    auto_match_rate DECIMAL(7,4),                       -- pct; ≥98 KPI (§17.14.1)
    detail          JSONB,
    created_at      TIMESTAMPTZ  NOT NULL DEFAULT now(),
    UNIQUE (prime_broker_id, run_date, source)          -- one run per feed per day
);
CREATE INDEX pb_recon_runs_date_ix ON pb_recon_runs (run_date);

CREATE TABLE pb_recon_breaks (
    id               BIGSERIAL PRIMARY KEY,
    run_id           BIGINT      NOT NULL REFERENCES pb_recon_runs (id),
    giveup_trade_id  BIGINT      REFERENCES pb_giveup_trades (id),
    prime_broker_id  BIGINT      NOT NULL REFERENCES prime_brokers (id),
    break_type       VARCHAR(24) NOT NULL CHECK (break_type IN
        ('RATE_MISMATCH','QUANTITY_MISMATCH','PAIR_MISMATCH','MISSING_TICKET',
         'UNAFFIRMED_TIMEOUT','MISSING_AT_PB','MISSING_LOCALLY')),
    status           VARCHAR(16) NOT NULL DEFAULT 'OPEN' CHECK (status IN
        ('OPEN','INVESTIGATING','RESOLVED','WRITTEN_OFF')),
    expected         JSONB,                             -- our side
    actual           JSONB,                             -- PB/blotter side
    assigned_to      BIGINT,
    resolution_note  TEXT,
    escalated_at     TIMESTAMPTZ,                       -- T+2 escalation stamp
    write_off_id     BIGINT,
    resolved_by      BIGINT,
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    resolved_at      TIMESTAMPTZ
);
CREATE INDEX pb_recon_breaks_open_ix
    ON pb_recon_breaks (created_at)
    WHERE status IN ('OPEN','INVESTIGATING');
-- One open break per (give-up, type) — sweep replays don't duplicate.
CREATE UNIQUE INDEX pb_recon_breaks_open_ux
    ON pb_recon_breaks (giveup_trade_id, break_type)
    WHERE status IN ('OPEN','INVESTIGATING');

CREATE TABLE pb_recon_events (
    id         BIGSERIAL PRIMARY KEY,
    break_id   BIGINT      REFERENCES pb_recon_breaks (id),
    giveup_id  BIGINT      REFERENCES pb_giveup_trades (id),
    actor_id   BIGINT,
    action     VARCHAR(48) NOT NULL,
    detail     JSONB,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX pb_recon_events_break_ix ON pb_recon_events (break_id, created_at);

-- Atomic collateral-rebalance evidence for give-up/position transfers
-- (spec §13.9/§5.22 invariant — balances.locked moves with the exposure).
CREATE TABLE pb_giveup_collateral_moves (
    id              BIGSERIAL PRIMARY KEY,
    giveup_trade_id BIGINT        NOT NULL REFERENCES pb_giveup_trades (id),
    from_account_id BIGINT        NOT NULL REFERENCES accounts (id),
    to_account_id   BIGINT        NOT NULL REFERENCES accounts (id),
    currency        VARCHAR(3)    NOT NULL,
    amount          DECIMAL(28,8) NOT NULL CHECK (amount > 0),
    created_at      TIMESTAMPTZ   NOT NULL DEFAULT now()
);
CREATE INDEX pb_giveup_collateral_moves_giveup_ix
    ON pb_giveup_collateral_moves (giveup_trade_id);

-- ── Task 24.3.14 — PB credit restitution correction journal ──────────────
-- One row per settlement event = the idempotency contract (duplicate
-- SETTLEMENT_FAILED deliveries never double-credit DSL/NOP).
CREATE TABLE pb_credit_restitutions (
    id                        BIGSERIAL PRIMARY KEY,
    settlement_instruction_id BIGINT       NOT NULL UNIQUE
                              REFERENCES settlement_instructions (id),
    trade_id                  BIGINT       NOT NULL,
    prime_broker_id           BIGINT       REFERENCES prime_brokers (id),
    client_account_id         BIGINT       NOT NULL REFERENCES accounts (id),
    currency_pair             VARCHAR(16),                  -- scope (NULL=global)
    dsl_credit_usd            DECIMAL(28,8) NOT NULL DEFAULT 0,
    nop_delta_usd             DECIMAL(28,8) NOT NULL DEFAULT 0, -- signed
    reason                    VARCHAR(48)   NOT NULL DEFAULT 'SETTLEMENT_FAIL_RESTITUTION',
    status                    VARCHAR(12)   NOT NULL DEFAULT 'APPLIED'
                              CHECK (status IN ('APPLIED','BLOCKED')),
    blocked_reason            TEXT,
    journal_entry_id          BIGINT REFERENCES journal_entries (id),
    pb_notified               BOOLEAN       NOT NULL DEFAULT false,
    margin_recalc_queued      BOOLEAN       NOT NULL DEFAULT false,
    created_at                TIMESTAMPTZ   NOT NULL DEFAULT now()
);
CREATE INDEX pb_credit_restitutions_client_ix
    ON pb_credit_restitutions (client_account_id, created_at);

-- ── Task 24.3.19 — ops hardening ─────────────────────────────────────────
-- Write-off authority matrix rows (dual control enforced by the queue).
CREATE TABLE settlement_write_offs (
    id                      BIGSERIAL PRIMARY KEY,
    exception_id            BIGINT REFERENCES settlement_exceptions (id),
    break_id                BIGINT REFERENCES pb_recon_breaks (id),
    currency                VARCHAR(3)    NOT NULL,
    amount                  DECIMAL(28,8) NOT NULL CHECK (amount >= 0),
    tier                    VARCHAR(8)    NOT NULL CHECK (tier IN ('T1','T2','T3')),
    required_role           VARCHAR(32)   NOT NULL,     -- matrix: T1 Finance Ops, T2 Risk Manager, T3 Super Admin
    reason                  TEXT          NOT NULL,
    dual_control_request_id BIGINT,
    requested_by            BIGINT        NOT NULL,
    approved_by             BIGINT,
    gl_journal_id           BIGINT        REFERENCES journal_entries (id),
    status                  VARCHAR(12)   NOT NULL DEFAULT 'PENDING'
                            CHECK (status IN ('PENDING','EXECUTED','REJECTED')),
    created_at              TIMESTAMPTZ   NOT NULL DEFAULT now(),
    decided_at              TIMESTAMPTZ,
    CHECK ((exception_id IS NOT NULL) OR (break_id IS NOT NULL))
);
CREATE INDEX settlement_write_offs_pending_ix
    ON settlement_write_offs (created_at) WHERE status = 'PENDING';

-- Per-currency auto-match tolerances for the reconcilers (§17.14.1).
CREATE TABLE recon_tolerances (
    currency        VARCHAR(3) PRIMARY KEY,
    amount_abs      DECIMAL(28,8) NOT NULL,              -- absolute tolerance
    amount_pct      DECIMAL(9,6)  NOT NULL DEFAULT 0,    -- relative tolerance (0.0001 = 1bp)
    rate_tolerance_bp DECIMAL(10,4) NOT NULL DEFAULT 0,  -- rate match band (bp)
    updated_at      TIMESTAMPTZ   NOT NULL DEFAULT now()
);
INSERT INTO recon_tolerances (currency, amount_abs, amount_pct, rate_tolerance_bp) VALUES
    ('USD', 1000, 0.0001, 0.5), ('EUR', 1000, 0.0001, 0.5),
    ('GBP', 1000, 0.0001, 0.5), ('JPY', 100000, 0.0001, 0.5),
    ('AUD', 1000, 0.0001, 0.5), ('CAD', 1000, 0.0001, 0.5),
    ('CHF', 1000, 0.0001, 0.5), ('NZD', 1000, 0.0001, 0.5),
    ('MXN', 20000, 0.0001, 0.5)
ON CONFLICT (currency) DO NOTHING;

-- Nostro funding threshold methodology per currency per correspondent:
-- required cover = 3-day scheduled outflows + CLS pay-in cover; breach →
-- P2 alert + backup-correspondent failover routing (§17.14.2).
CREATE TABLE nostro_funding_thresholds (
    id                      BIGSERIAL PRIMARY KEY,
    nostro_account_id       BIGINT       NOT NULL UNIQUE REFERENCES nostro_accounts (id),
    currency                VARCHAR(3)   NOT NULL,
    min_outflow_cover_days  INT          NOT NULL DEFAULT 3,
    cls_payin_cover         DECIMAL(28,8) NOT NULL DEFAULT 0,
    computed_threshold      DECIMAL(28,8) NOT NULL DEFAULT 0,
    concentration_limit_pct DECIMAL(5,2)  NOT NULL DEFAULT 40.00, -- max % of ccy funding at one correspondent
    backup_nostro_id        BIGINT       REFERENCES nostro_accounts (id), -- designated backup correspondent
    breach                  BOOLEAN      NOT NULL DEFAULT false,
    last_evaluated_at       TIMESTAMPTZ,
    updated_at              TIMESTAMPTZ  NOT NULL DEFAULT now()
);
CREATE INDEX nostro_funding_thresholds_ccy_ix ON nostro_funding_thresholds (currency);

-- Complete per-rail-per-currency cut-off matrix (extends Phase-11
-- Task 11.3.7's SWIFT/SEPA examples; §17.14.3).
CREATE TABLE rail_cutoff_matrix (
    id            BIGSERIAL PRIMARY KEY,
    rail          VARCHAR(16) NOT NULL CHECK (rail IN
        ('SWIFT','SEPA','FEDNOW','ACH','CHAPS','TARGET2','CLS')),
    currency      VARCHAR(3)  NOT NULL,
    cutoff_utc    TIME        NOT NULL,                    -- dispatch cut-off (UTC)
    value_date_roll BOOLEAN   NOT NULL DEFAULT true,       -- late → next mutual business day (Task 3.3.8 calendar)
    late_fee      DECIMAL(28,8) NOT NULL DEFAULT 0,        -- pass-through fee on late payments
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (rail, currency)
);
INSERT INTO rail_cutoff_matrix (rail, currency, cutoff_utc, late_fee) VALUES
    ('SWIFT','USD','22:00',50),('SWIFT','EUR','16:00',50),('SWIFT','GBP','15:30',50),
    ('SWIFT','JPY','01:00',60),('SWIFT','AUD','07:00',50),('SWIFT','CAD','21:30',50),
    ('SWIFT','CHF','15:00',50),('SWIFT','NZD','08:00',50),('SWIFT','MXN','20:00',60),
    ('SEPA','EUR','16:00',25),('FEDNOW','USD','23:30',10),('ACH','USD','20:30',15),
    ('CHAPS','GBP','15:30',25),('TARGET2','EUR','16:30',25),
    ('CLS','USD','06:30',0),('CLS','EUR','06:30',0),('CLS','GBP','06:30',0),
    ('CLS','JPY','06:30',0),('CLS','AUD','06:30',0),('CLS','CAD','06:30',0),
    ('CLS','CHF','06:30',0),('CLS','NZD','06:30',0),('CLS','MXN','13:00',0)
ON CONFLICT (rail, currency) DO NOTHING;

-- Queued-payment failover/retry: 15m/1h/4h timetable + duplicate-payment
-- guard (payment_ref UNIQUE + DISPATCHED latch; §17.14.5).
CREATE TABLE rail_failover_queue (
    id             BIGSERIAL PRIMARY KEY,
    payment_ref    VARCHAR(64) NOT NULL UNIQUE,            -- :20:/MsgId — dup guard key
    rail           VARCHAR(16) NOT NULL,
    fallback_rail  VARCHAR(16),                            -- e.g. FEDNOW→ACH
    currency       VARCHAR(3)  NOT NULL,
    amount         DECIMAL(28,8) NOT NULL,
    payload        JSONB        NOT NULL,
    attempts       INT          NOT NULL DEFAULT 0,
    next_attempt_at TIMESTAMPTZ NOT NULL,
    last_error     TEXT,
    status         VARCHAR(12)  NOT NULL DEFAULT 'QUEUED'
                   CHECK (status IN ('QUEUED','RETRYING','DISPATCHED','EXHAUSTED','CANCELLED')),
    dispatched_at  TIMESTAMPTZ,
    created_at     TIMESTAMPTZ  NOT NULL DEFAULT now(),
    updated_at     TIMESTAMPTZ  NOT NULL DEFAULT now()
);
CREATE INDEX rail_failover_due_ix
    ON rail_failover_queue (next_attempt_at)
    WHERE status IN ('QUEUED','RETRYING');

-- Herstatt principal exposure: paid-but-not-received per
-- counterparty/currency with a duration cap, monitored intraday.
CREATE TABLE herstatt_exposures (
    id               BIGSERIAL PRIMARY KEY,
    counterparty_id  BIGINT        NOT NULL,               -- account/PB we paid to
    currency         VARCHAR(3)    NOT NULL,
    paid_amount      DECIMAL(28,8) NOT NULL,               -- our leg paid
    receivable_amount DECIMAL(28,8) NOT NULL,              -- counter-leg still owed
    window_open_at   TIMESTAMPTZ   NOT NULL,               -- pay-in timestamp
    duration_cap_minutes INT       NOT NULL DEFAULT 120,   -- principal-at-risk cap
    breached         BOOLEAN       NOT NULL DEFAULT false,
    settled_at       TIMESTAMPTZ,                          -- counter-leg received
    created_at       TIMESTAMPTZ   NOT NULL DEFAULT now()
);
CREATE INDEX herstatt_open_ix
    ON herstatt_exposures (window_open_at) WHERE settled_at IS NULL;

-- CLS pay-in operations: pre-funding (which nostro, funded T-1 by
-- 22:00 UTC), failed-pay-in consequence ladder, member-outage fallback.
CREATE TABLE cls_payin_events (
    id               BIGSERIAL PRIMARY KEY,
    currency         VARCHAR(3)    NOT NULL,
    value_date       DATE          NOT NULL,
    nostro_account_id BIGINT       NOT NULL REFERENCES nostro_accounts (id),
    required_amount  DECIMAL(28,8) NOT NULL,
    funded_amount    DECIMAL(28,8) NOT NULL DEFAULT 0,
    prefund_deadline TIMESTAMPTZ   NOT NULL,               -- T-1 22:00 UTC
    ladder_step      INT           NOT NULL DEFAULT 0,     -- 0 none,1 alert,2 borrow,3 defer,4 bilateral waterfall
    member_outage    BOOLEAN       NOT NULL DEFAULT false,
    fallback         VARCHAR(24),                          -- ALT_PVP|NETTING|CONTROLLED_GROSS
    status           VARCHAR(12)   NOT NULL DEFAULT 'SCHEDULED'
                     CHECK (status IN ('SCHEDULED','FUNDED','SHORT','PAID_IN','FAILED')),
    created_at       TIMESTAMPTZ   NOT NULL DEFAULT now(),
    updated_at       TIMESTAMPTZ   NOT NULL DEFAULT now(),
    UNIQUE (currency, value_date, nostro_account_id)
);

-- FX fail economics (24.3.19 item 5 — supersedes CSDR penalties for FX):
-- replacement-cost close-out + fail interest at policy+100bps from ISD+1.
CREATE TABLE fx_fail_closeouts (
    id                   BIGSERIAL PRIMARY KEY,
    fail_id              BIGINT       NOT NULL UNIQUE REFERENCES settlement_fails (id),
    original_rate        DECIMAL(28,8) NOT NULL,
    closeout_rate        DECIMAL(28,8) NOT NULL,           -- market at close-out
    replacement_cost     DECIMAL(28,8) NOT NULL,           -- |closeout-original| × unsettled qty
    currency             VARCHAR(3)    NOT NULL,
    policy_rate_bp       DECIMAL(10,4) NOT NULL,
    fail_interest_amount DECIMAL(28,8) NOT NULL,           -- amount × rate × days/dcc
    accrual_days         INT           NOT NULL,
    isda_treatment       VARCHAR(32)   NOT NULL DEFAULT 'ISDA_CLOSEOUT', -- ISDA/FX-Global-Code record
    created_at           TIMESTAMPTZ   NOT NULL DEFAULT now()
);

-- LP-default playbook escalations (§17.14.5 tail).
CREATE TABLE lp_default_events (
    id         BIGSERIAL PRIMARY KEY,
    lp_id      BIGINT       NOT NULL,
    stage      VARCHAR(20)  NOT NULL CHECK (stage IN
        ('QUOTE_WITHDRAWN','FLOORS_WIDENED','ADL_Q5')),
    detail     JSONB,
    created_at TIMESTAMPTZ  NOT NULL DEFAULT now()
);
CREATE INDEX lp_default_events_lp_ix ON lp_default_events (lp_id, created_at);

COMMIT;
