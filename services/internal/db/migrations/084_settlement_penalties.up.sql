-- 084_settlement_penalties.up.sql
-- Phase-24 Task 24.3.13 (CSDR settlement discipline: fails, penalties,
-- mandatory buy-in) as amended by Task 24.3.19 / spec §17.14 item 4
-- (remediation #24).
--
-- REGIME NOTE (supersede): CSDR Art. 7 cash penalties + mandatory buy-in
-- govern CSD-settled securities, not CLS/correspondent-bank FX. For FX
-- settlement legs the regime flag is 'FX_CLOSEOUT' — replacement-cost
-- close-out + fail interest (policy rate + 100bps from ISD+1) per
-- §17.14/ISDA-FX-Global-Code treatment, computed by Task 24.3.19
-- (fx_fail_closeouts, migration 258). The CSDR penalty/buy-in machinery
-- below is retained for the securities-venue scope the spec retains
-- (settlement_fails.regime='CSDR'); FX legs never accrue CSDR penalties.
--
-- Fail detection (retained): the daily ISD+1 scan flags late legs via
-- settlement_instructions.fail_flag='SETTLEMENT_FAIL' and one
-- settlement_fails row per instruction (UNIQUE for idempotent scans).

BEGIN;

ALTER TABLE settlement_instructions
    ADD COLUMN IF NOT EXISTS fail_flag VARCHAR(32); -- 'SETTLEMENT_FAIL' when ISD+1 scan fires
CREATE INDEX IF NOT EXISTS settlement_instructions_fail_ix
    ON settlement_instructions (settlement_date)
    WHERE fail_flag = 'SETTLEMENT_FAIL';

CREATE TYPE settlement_fail_regime_enum  AS ENUM ('CSDR', 'FX_CLOSEOUT');
CREATE TYPE settlement_fail_status_enum  AS ENUM
    ('OPEN', 'CLOSED_OUT', 'BUYIN_NOTIFIED', 'BOUGHT_IN', 'RESOLVED');
CREATE TYPE liquidity_class_enum         AS ENUM ('LIQUID', 'ILLIQUID');

CREATE TABLE settlement_fails (
    id                        BIGSERIAL PRIMARY KEY,
    settlement_instruction_id BIGINT NOT NULL UNIQUE
                              REFERENCES settlement_instructions (id),
    trade_id                  BIGINT      NOT NULL,
    account_id                BIGINT      NOT NULL,
    currency                  VARCHAR(3)  NOT NULL,
    amount                    DECIMAL(28,8) NOT NULL,
    isd                       DATE        NOT NULL,  -- intended settlement date
    detected_at               TIMESTAMPTZ NOT NULL,  -- ISD+1 detection time
    regime                    settlement_fail_regime_enum NOT NULL,
    liquidity_class           liquidity_class_enum NOT NULL DEFAULT 'LIQUID',
    cls_settled               BOOLEAN     NOT NULL DEFAULT false, -- CLS legs exempt (§24 task)
    status                    settlement_fail_status_enum NOT NULL DEFAULT 'OPEN',
    buyin_notified_at         TIMESTAMPTZ,
    closed_out_at             TIMESTAMPTZ,
    resolved_at               TIMESTAMPTZ,
    created_at                TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at                TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX settlement_fails_open_ix
    ON settlement_fails (isd) WHERE status = 'OPEN';
CREATE INDEX settlement_fails_trade_ix ON settlement_fails (trade_id);

-- Daily cash penalty accrual (CSDR Art. 7). Bilateral: every accrual writes
-- a PAYABLE row (failing party owes) and a RECEIVABLE row (receiving party
-- is owed). Rates: 1bp/day liquid, 0.5bp/day illiquid.
CREATE TYPE penalty_direction_enum AS ENUM ('PAYABLE', 'RECEIVABLE');
CREATE TYPE penalty_status_enum    AS ENUM ('ACCRUED', 'INVOICED', 'SETTLED', 'DISPUTED', 'WAIVED');

CREATE TABLE settlement_penalties (
    id             BIGSERIAL PRIMARY KEY,
    fail_id        BIGINT        NOT NULL REFERENCES settlement_fails (id),
    accrual_date   DATE          NOT NULL,
    regime         settlement_fail_regime_enum NOT NULL,
    rate_bp        DECIMAL(10,4) NOT NULL,          -- 1.0000 liquid / 0.5000 illiquid
    base_amount    DECIMAL(28,8) NOT NULL,          -- unsettled settlement value
    penalty_amount DECIMAL(28,8) NOT NULL,          -- base_amount × rate_bp / 10000
    currency       VARCHAR(3)    NOT NULL,
    direction      penalty_direction_enum NOT NULL,
    counterparty_id BIGINT,                          -- account the bilateral line is owed by/to
    status         penalty_status_enum NOT NULL DEFAULT 'ACCRUED',
    created_at     TIMESTAMPTZ   NOT NULL DEFAULT now(),
    -- One accrual per fail per day per side — the daily job replays safely.
    UNIQUE (fail_id, accrual_date, direction)
);
CREATE INDEX settlement_penalties_fail_ix ON settlement_penalties (fail_id);
CREATE INDEX settlement_penalties_date_ix ON settlement_penalties (accrual_date);

-- Mandatory buy-in lifecycle (CSD scope only): ISD+4 notification to the
-- failing party, ISD+7 execution at market with the price differential
-- charged back. FX_CLOSEOUT fails never reach this table.
CREATE TYPE buyin_kind_enum   AS ENUM ('NOTIFICATION', 'EXECUTION');
CREATE TYPE buyin_status_enum AS ENUM ('ISSUED', 'ACKED', 'EXECUTED', 'CANCELLED');

CREATE TABLE buy_in_events (
    id                  BIGSERIAL PRIMARY KEY,
    fail_id             BIGINT       NOT NULL REFERENCES settlement_fails (id),
    kind                buyin_kind_enum   NOT NULL,
    market_price        DECIMAL(28,8),                 -- execution fill price
    original_price      DECIMAL(28,8),                 -- failing leg's reference rate
    price_differential  DECIMAL(28,8),                 -- charged to failing counterparty
    currency            VARCHAR(3),
    status              buyin_status_enum NOT NULL DEFAULT 'ISSUED',
    created_at          TIMESTAMPTZ  NOT NULL DEFAULT now(),
    UNIQUE (fail_id, kind)                             -- one notification + one execution per fail
);
CREATE INDEX buy_in_events_fail_ix ON buy_in_events (fail_id);

COMMIT;
