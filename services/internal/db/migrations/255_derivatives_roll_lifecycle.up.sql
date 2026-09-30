-- 255_derivatives_roll_lifecycle.up.sql
-- Phase-22 Task 22.3.8 (Roll Management) + Task 22.3.10 (Option
-- Lifecycle — premium settlement, exercise cutoff, auto-exercise);
-- spec §15.4, §24 #158/#393; Phase-22 AC rows 27–29/32/39/43.
--
--   contract_rolls              — close+open roll records over
--                                 derivative_contracts (migration 254);
--                                 roll_price is the inter-leg spread
--                                 (open_rate - close_rate).
--   auto_roll_config            — per-account automatic-roll switch +
--                                 timing rule (lead_days before
--                                 value_date) + roll tenor (Task 22.3.8).
--   option_positions            — per-position option contract terms +
--                                 lifecycle state. Order wire fields
--                                 (strike/option_type/…) ride migration
--                                 039; this table is the exercised/
--                                 settled position ledger the lifecycle
--                                 batch consumes.
--   option_premium_settlements  — holder→writer premium transfers, due
--                                 trade date + T+2 (§15.4, §24 #158);
--                                 failure flags + queues the Phase-19
--                                 Task 19.3.3 margin-call workflow
--                                 (§24 #393, PREMIUM_INSUFFICIENT).
--   option_assignments          — pro-rata writer assignment events with
--                                 exercise_price, assignment_price and
--                                 margin_impact (§15.4 amended: pro-rata
--                                 by OI with random tie-break; the seed
--                                 is stored for replay determinism).
--   option_exercise_prefs       — per-account ATM ±0.5%-band auto-
--                                 exercise preference (§15.4).
--   option_expiry_runs          — daily 15:00 UTC expiry-batch
--                                 bookkeeping (rollover_runs pattern:
--                                 UNIQUE run_date, resume-safe).
--
-- 'ROLLED' joins derivative_contract_status_enum: the terminal state of
-- an expiring contract closed into a replacement (Task 22.3.8). The
-- maturity sweep (derivative_contracts_due_ix, OPEN/PARTIALLY_SETTLED
-- only) never re-picks a rolled contract.

BEGIN;

ALTER TYPE derivative_contract_status_enum ADD VALUE IF NOT EXISTS 'ROLLED';

-- ── Task 22.3.8: roll records ──────────────────────────────────────────
CREATE TABLE contract_rolls (
    id                   BIGSERIAL    PRIMARY KEY,
    account_id           BIGINT       NOT NULL REFERENCES accounts (id),
    source_contract_id   BIGINT       NOT NULL REFERENCES derivative_contracts (id),
    target_contract_id   BIGINT       REFERENCES derivative_contracts (id),
    kind                 derivative_kind_enum NOT NULL,
    side                 VARCHAR(4)   NOT NULL CHECK (side IN ('BUY','SELL')),
    notional             DECIMAL(28,8) NOT NULL CHECK (notional > 0),
    source_value_date    DATE         NOT NULL,   -- expiring leg maturity
    target_value_date    DATE         NOT NULL,   -- replacement maturity
    close_rate           DECIMAL(20,8),           -- fair close of the expiring leg
    open_rate            DECIMAL(20,8),           -- forward rate of the replacement
    roll_price           DECIMAL(20,8),           -- open_rate - close_rate
    -- close_pnl: mark-to-market of the expiring contract at the close
    -- rate (signed, quote currency) — booked as a dated
    -- settlement_instructions leg linked to the source contract.
    close_pnl            DECIMAL(28,8),
    close_leg_id         BIGINT,
    status               VARCHAR(9)   NOT NULL DEFAULT 'PENDING'
        CHECK (status IN ('PENDING','COMPLETED','FAILED')),
    source               VARCHAR(6)   NOT NULL DEFAULT 'MANUAL'
        CHECK (source IN ('MANUAL','AUTO')),
    idempotency_key      VARCHAR(128),
    failure_reason       TEXT,
    created_at           TIMESTAMPTZ  NOT NULL DEFAULT now(),
    completed_at         TIMESTAMPTZ
);
CREATE UNIQUE INDEX contract_rolls_idem_ux
    ON contract_rolls (idempotency_key) WHERE idempotency_key IS NOT NULL;
CREATE INDEX contract_rolls_account_ix
    ON contract_rolls (account_id, created_at DESC);
CREATE INDEX contract_rolls_source_ix
    ON contract_rolls (source_contract_id);

-- ── Task 22.3.8: automatic roll configuration (per account) ────────────
CREATE TABLE auto_roll_config (
    account_id    BIGINT      PRIMARY KEY REFERENCES accounts (id),
    enabled       BOOLEAN     NOT NULL DEFAULT FALSE,
    -- lead_days is the timing rule: the sweep rolls an OPEN contract when
    -- its value_date <= today + lead_days. 0 = on the value date.
    lead_days     SMALLINT    NOT NULL DEFAULT 1
        CHECK (lead_days BETWEEN 0 AND 10),
    -- tenor is the replacement maturity on the §7.4 grid (e.g. '1M',
    -- '3M'); the sweep resolves it through the holiday calendar.
    tenor         VARCHAR(8)  NOT NULL DEFAULT '1M'
        CHECK (tenor IN ('1W','2W','1M','2M','3M','6M','9M','1Y')),
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- ── Task 22.3.10: option contract terms + lifecycle state ──────────────
-- One row per option-bearing positions row (positions.id UNIQUE). The
-- position's side gives the book side: LONG = holder, SHORT = writer.
CREATE TABLE option_positions (
    id                       BIGSERIAL    PRIMARY KEY,
    position_id              BIGINT       NOT NULL UNIQUE REFERENCES positions (id),
    account_id               BIGINT       NOT NULL REFERENCES accounts (id),
    instrument_id            BIGINT       NOT NULL REFERENCES instruments (id),
    -- Deliverable spot instrument for PHYSICAL settlement (same
    -- base/quote SPOT row); required for PHYSICAL, optional for CASH.
    underlying_instrument_id BIGINT       REFERENCES instruments (id),
    side                     position_side_enum NOT NULL,
    option_type              VARCHAR(6)   NOT NULL
        CHECK (option_type IN ('CALL', 'PUT', 'BINARY')),
    exercise_style           VARCHAR(8)   NOT NULL
        CHECK (exercise_style IN ('EUROPEAN', 'AMERICAN')),
    settlement               VARCHAR(8)   NOT NULL DEFAULT 'PHYSICAL'
        CHECK (settlement IN ('PHYSICAL', 'CASH')),
    strike                   DECIMAL(28,8) NOT NULL CHECK (strike > 0),
    -- quantity is the live open interest: EXERCISED/ASSIGNED/EXPIRED rows
    -- burn down to 0 (the lifecycle transition ledger), so the check
    -- admits 0 even though new positions always register positive.
    quantity                 DECIMAL(28,8) NOT NULL CHECK (quantity >= 0),
    expiry_at                TIMESTAMPTZ  NOT NULL,
    premium                  DECIMAL(28,8) NOT NULL DEFAULT 0
        CHECK (premium >= 0),
    premium_currency         VARCHAR(3),
    premium_status           VARCHAR(8)   NOT NULL DEFAULT 'PENDING'
        CHECK (premium_status IN ('PENDING', 'SETTLED', 'FAILED')),
    premium_due_date         DATE,                  -- trade date + T+2
    -- BINARY only: fixed per-unit payout paid when the option expires
    -- in-the-money (Task 22.3.6 payout evaluation edge case).
    payout                   DECIMAL(28,8),
    status                   VARCHAR(16)  NOT NULL DEFAULT 'OPEN'
        CHECK (status IN ('OPEN', 'EXERCISED', 'ASSIGNED', 'EXPIRED')),
    -- Holder's do-not-exercise instruction (§15.4); amendable until the
    -- 15:00 UTC expiry cutoff. Applies to auto-exercise only — an
    -- explicit manual exercise instruction supersedes it.
    do_not_exercise          BOOLEAN      NOT NULL DEFAULT FALSE,
    exercise_mark            DECIMAL(20,8),          -- mark used at exercise/expiry
    exercised_at             TIMESTAMPTZ,
    created_at               TIMESTAMPTZ  NOT NULL DEFAULT now(),
    updated_at               TIMESTAMPTZ  NOT NULL DEFAULT now()
);
CREATE INDEX option_positions_expiry_ix
    ON option_positions (expiry_at) WHERE status = 'OPEN';
CREATE INDEX option_positions_instr_ix
    ON option_positions (instrument_id, side, status);
CREATE INDEX option_positions_premium_due_ix
    ON option_positions (premium_due_date)
    WHERE premium_status = 'PENDING' AND premium > 0;

-- ── Task 22.3.10: premium settlement ledger (T+2, §24 #158/#393) ───────
CREATE TABLE option_premium_settlements (
    id                  BIGSERIAL    PRIMARY KEY,
    holder_position_id  BIGINT       NOT NULL UNIQUE
        REFERENCES option_positions (id),
    writer_position_id  BIGINT       REFERENCES option_positions (id),
    holder_account_id   BIGINT       NOT NULL REFERENCES accounts (id),
    writer_account_id   BIGINT       NOT NULL REFERENCES accounts (id),
    amount              DECIMAL(28,8) NOT NULL CHECK (amount > 0),
    currency            VARCHAR(3)   NOT NULL,
    due_date            DATE         NOT NULL,     -- trade date + T+2
    status              VARCHAR(8)   NOT NULL DEFAULT 'PENDING'
        CHECK (status IN ('PENDING', 'SETTLED', 'FAILED')),
    journal_entry_id    BIGINT       REFERENCES journal_entries (id),
    -- TRUE once the Phase-19 Task 19.3.3 margin-call workflow was
    -- triggered for a failed premium debit (§24 #393).
    margin_call_queued  BOOLEAN      NOT NULL DEFAULT FALSE,
    failure_reason      TEXT,
    created_at          TIMESTAMPTZ  NOT NULL DEFAULT now(),
    settled_at          TIMESTAMPTZ
);
CREATE INDEX option_premium_due_ix
    ON option_premium_settlements (due_date) WHERE status = 'PENDING';

-- ── Task 22.3.10: writer assignment events (AC: all assignment events
-- logged with timestamp, exercise_price, assignment_price, margin_impact)
CREATE TABLE option_assignments (
    id                  BIGSERIAL    PRIMARY KEY,
    holder_position_id  BIGINT       NOT NULL REFERENCES option_positions (id),
    holder_account_id   BIGINT       NOT NULL REFERENCES accounts (id),
    writer_position_id  BIGINT       NOT NULL REFERENCES option_positions (id),
    writer_account_id   BIGINT       NOT NULL REFERENCES accounts (id),
    instrument_id       BIGINT       NOT NULL REFERENCES instruments (id),
    quantity            DECIMAL(28,8) NOT NULL CHECK (quantity > 0),
    exercise_price      DECIMAL(28,8) NOT NULL,   -- the exercised strike
    assignment_price    DECIMAL(28,8) NOT NULL,   -- price the writer leg booked at
    mark_at_expiry      DECIMAL(20,8),            -- oracle mark at evaluation
    margin_impact       DECIMAL(28,8),            -- writer-side IM delta
    -- TRUE when the exercising account failed the IM check and the
    -- delivery proceeded into the liquidation path (Task 22.3.14).
    margin_shortfall    BOOLEAN      NOT NULL DEFAULT FALSE,
    mode                VARCHAR(8)   NOT NULL CHECK (mode IN ('PHYSICAL', 'CASH')),
    source              VARCHAR(6)   NOT NULL CHECK (source IN ('MANUAL', 'AUTO')),
    -- Pro-rata tie-break seed — stored so the assignment replays
    -- deterministically (spec §15.7 item 3).
    assignment_seed     BIGINT       NOT NULL,
    gl_journal_id       BIGINT       REFERENCES journal_entries (id),
    created_at          TIMESTAMPTZ  NOT NULL DEFAULT now()
);
CREATE INDEX option_assignments_instr_ix
    ON option_assignments (instrument_id, created_at DESC);
CREATE INDEX option_assignments_writer_ix
    ON option_assignments (writer_position_id, created_at DESC);

-- ── Task 22.3.10: per-account exercise preference (ATM ±0.5% band) ─────
CREATE TABLE option_exercise_prefs (
    account_id          BIGINT      PRIMARY KEY REFERENCES accounts (id),
    auto_exercise_atm   BOOLEAN     NOT NULL DEFAULT FALSE,
    updated_at          TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- ── Task 22.3.10: expiry-batch run bookkeeping (15:00 UTC daily) ───────
CREATE TABLE option_expiry_runs (
    id                  BIGSERIAL    PRIMARY KEY,
    run_date            DATE         NOT NULL UNIQUE,  -- UTC expiry date processed
    status              VARCHAR(16)  NOT NULL DEFAULT 'RUNNING'
        CHECK (status IN ('RUNNING', 'COMPLETED', 'FAILED')),
    started_at          TIMESTAMPTZ  NOT NULL DEFAULT now(),
    completed_at        TIMESTAMPTZ,
    positions_evaluated INTEGER      NOT NULL DEFAULT 0,
    auto_exercised      INTEGER      NOT NULL DEFAULT 0,
    expired             INTEGER      NOT NULL DEFAULT 0,
    assignments         INTEGER      NOT NULL DEFAULT 0,
    premiums_settled    INTEGER      NOT NULL DEFAULT 0,
    premiums_failed     INTEGER      NOT NULL DEFAULT 0,
    error_text          TEXT
);
CREATE INDEX option_expiry_runs_status_ix ON option_expiry_runs (status, run_date);

COMMIT;
