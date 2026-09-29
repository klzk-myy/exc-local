-- 045_market_maker_program.up.sql
-- Phase-18 Task 18.3.10 — Market-Maker Program & MMP protection
-- (spec §5.27, §9.6, §24 #139).
--
-- mm_programs          — registered market makers with quoting
--                        obligations (min two-sided size, max spread,
--                        minimum daily presence), MMP auto-cancel
--                        thresholds, the maker rebate tier and the MM
--                        OTR allowance consumed by the Task 13.3.6 RTS-9
--                        limiter (risk.OtrMonitor WithMMAllowance seam).
-- mm_compliance        — daily obligation rollup: per-minute samples of
--                        two-sided-presence compliance, computed
--                        presence_pct, and the breach flag the rolling
--                        3-in-7 suspension rule counts.
-- mm_rebate_accruals   — per-fill maker-rebate accrual rows; posted to
--                        the GL monthly as one journal per (account,
--                        currency) then stamped with the journal id
--                        (§9.6 "reconciled via the double-entry GL").
--
-- instrument_id NULL = program-wide row (one obligation set covering
-- every instrument the account quotes); a set instrument_id is a
-- per-instrument override evaluated alongside the program-wide row.

BEGIN;

CREATE TYPE mm_program_status_enum AS ENUM ('ACTIVE', 'SUSPENDED');

CREATE TABLE mm_programs (
    id              BIGSERIAL PRIMARY KEY,
    account_id      BIGINT       NOT NULL REFERENCES accounts (id),
    instrument_id   BIGINT                REFERENCES instruments (id),
    min_quote_size  DECIMAL(28,8) NOT NULL CHECK (min_quote_size > 0),
    max_spread_bps  DECIMAL(8,4)  NOT NULL CHECK (max_spread_bps > 0),
    presence_pct    DECIMAL(5,2)  NOT NULL
                    CHECK (presence_pct > 0 AND presence_pct <= 100),
    mmp_max_fills   INTEGER       NOT NULL CHECK (mmp_max_fills > 0),
    -- §24.x MM Program row: the sliding MMP window is bounded to
    -- [100ms, 5000ms]; the DB enforces the same band.
    mmp_window_ms   INTEGER       NOT NULL
                    CHECK (mmp_window_ms BETWEEN 100 AND 5000),
    rebate_bps      DECIMAL(6,4)  NOT NULL DEFAULT 0 CHECK (rebate_bps >= 0),
    -- MM OTR allowance (§9.6): a higher order-to-trade ratio for the
    -- registered MM's quoting flow, resolved by risk.OtrMonitor ahead
    -- of the venue default. NULL = venue/risk_limits ratio applies.
    otr_allowance   DECIMAL(28,8)          CHECK (otr_allowance IS NULL OR otr_allowance > 0),
    status          mm_program_status_enum NOT NULL DEFAULT 'ACTIVE',
    created_at      TIMESTAMPTZ   NOT NULL DEFAULT now(),
    updated_at      TIMESTAMPTZ   NOT NULL DEFAULT now()
);

-- One program row per (account, instrument); program-wide rows share
-- instrument_id NULL — NULLS NOT DISTINCT keeps them unique too (PG15+).
CREATE UNIQUE INDEX mm_programs_account_instrument_uq
    ON mm_programs (account_id, instrument_id) NULLS NOT DISTINCT;

CREATE INDEX mm_programs_account_idx ON mm_programs (account_id)
    WHERE status = 'ACTIVE';

CREATE TABLE mm_compliance (
    id                BIGSERIAL PRIMARY KEY,
    program_id        BIGINT       NOT NULL REFERENCES mm_programs (id) ON DELETE CASCADE,
    day               DATE         NOT NULL,                -- UTC trading day
    samples_total     INTEGER      NOT NULL DEFAULT 0,      -- per-minute obligation samples
    samples_compliant INTEGER      NOT NULL DEFAULT 0,      -- samples meeting size+spread
    presence_pct      DECIMAL(5,2),                         -- rollup output: compliant/total*100
    breach            BOOLEAN      NOT NULL DEFAULT false,  -- presence_pct < program.presence_pct
    breach_reason     VARCHAR(128),                         -- e.g. PRESENCE_SHORTFALL
    created_at        TIMESTAMPTZ  NOT NULL DEFAULT now(),
    updated_at        TIMESTAMPTZ  NOT NULL DEFAULT now(),
    UNIQUE (program_id, day)
);

CREATE INDEX mm_compliance_breach_idx ON mm_compliance (program_id, day)
    WHERE breach;

CREATE TABLE mm_rebate_accruals (
    id               BIGSERIAL PRIMARY KEY,
    program_id       BIGINT       NOT NULL REFERENCES mm_programs (id) ON DELETE CASCADE,
    account_id       BIGINT       NOT NULL REFERENCES accounts (id),
    instrument_id    BIGINT       NOT NULL REFERENCES instruments (id),
    fill_ref         VARCHAR(64)  NOT NULL,                 -- caller idempotency token
    day              DATE         NOT NULL,                 -- accrual bucket (UTC)
    currency         VARCHAR(3)   NOT NULL,                 -- instrument quote currency
    amount           DECIMAL(28,8) NOT NULL CHECK (amount >= 0),
    posted_journal_id BIGINT,                             -- journal_entries.id once posted
    created_at       TIMESTAMPTZ  NOT NULL DEFAULT now(),
    UNIQUE (program_id, fill_ref)                         -- per-fill idempotency
);

CREATE INDEX mm_rebate_accruals_unposted_idx
    ON mm_rebate_accruals (account_id, currency)
    WHERE posted_journal_id IS NULL;

COMMIT;
