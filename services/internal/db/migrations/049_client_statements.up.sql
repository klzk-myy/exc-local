-- 049_client_statements.up.sql
-- Phase-20 Tasks 20.3.6 + 20.3.7 (spec §5.28, §8.4, §16.5; §24 #143/#205).
--
--   client_statements    — generated daily/monthly client statement
--                          records; file_ref is the object-store key stem
--                          (objects stored as "<file_ref>.pdf" /
--                          "<file_ref>.csv" — one ref, two renderings).
--   trade_confirmations  — per-fill MiFID Art. 59 / Art. 25 contract-note
--                          records; versioned so a busted/price-adjusted
--                          trade keeps the superseded row AND the
--                          regenerated ADJUSTED confirmation (spec §5.29 —
--                          corrected trades are retained flagged, never
--                          deleted).
--   fee_invoices         — monthly institutional fee invoices
--                          (trading fees + connectivity − MM rebates).
--   trial_balances       — Task 20.3.7 persisted read-model cache of the
--                          daily EOD trial balance (one row per
--                          (business_date, currency, GL account)); the
--                          report is always recomputable from
--                          ledger_lines — these rows are a cache, never
--                          the book of record.
--   erp_delivery_log     — ERP batch dispatch journal with replay
--                          protection: run_id UNIQUE rejects a same-run
--                          resend once SENT; seq is a deployment-monotonic
--                          sequence so the ERP can order batches.
--
-- Retention (spec §19.12 / MiFID II record-keeping):
--   * client_statements, trade_confirmations — retained >= 5 years.
--   * fee_invoices, trial_balances, erp_delivery_log — retained >= 7
--     years (audit/tax horizon).
--   The Phase-09 Task 9.3.22 retention enforcer MUST NOT purge these
--   tables below their horizons; deletion, where permitted at all, is
--   archival-to-S3 first (hot-warm-cold tiering, Task 9.3.24).

BEGIN;

CREATE TYPE statement_period_enum      AS ENUM ('DAILY', 'MONTHLY');
CREATE TYPE confirmation_status_enum   AS ENUM ('GENERATED', 'DELIVERED', 'ADJUSTED', 'FAILED');
CREATE TYPE invoice_status_enum        AS ENUM ('ISSUED', 'PAID', 'VOID');
CREATE TYPE erp_delivery_status_enum   AS ENUM ('PENDING', 'SENT', 'FAILED');

CREATE TABLE client_statements (
    statement_id    BIGSERIAL PRIMARY KEY,
    account_id      BIGINT       NOT NULL REFERENCES accounts (id),
    period          statement_period_enum NOT NULL,
    period_start    DATE         NOT NULL,
    period_end      DATE         NOT NULL,               -- exclusive bound (period covers [start, end))
    statement_type  VARCHAR(32)  NOT NULL DEFAULT 'ACCOUNT', -- ACCOUNT | future regulatory variants
    file_ref        VARCHAR(512) NOT NULL,               -- object-store key stem; "<ref>.pdf"/"<ref>.csv"
    content_sha256  CHAR(64)     NOT NULL,               -- sha256 of the PDF rendering (integrity anchor)
    generated_at    TIMESTAMPTZ  NOT NULL DEFAULT now(),
    UNIQUE (account_id, period, period_start, statement_type),
    CHECK (period_end > period_start)
);
CREATE INDEX client_statements_acct_ix
    ON client_statements (account_id, period, period_start DESC);
COMMENT ON TABLE client_statements IS
    'Task 20.3.6 client statements (spec §5.28). file_ref is an object-store '
    'key stem carrying "<ref>.pdf" + "<ref>.csv". RETENTION: >= 5 years '
    '(MiFID record-keeping) — do not purge below the horizon; archive '
    'tiering only (Phase-09 Tasks 9.3.22/9.3.24).';

CREATE TABLE trade_confirmations (
    confirmation_id BIGSERIAL PRIMARY KEY,
    trade_id        BIGINT       NOT NULL,           -- logical ref -> trades.id: trades is
                                                     -- partitioned by created_at so no
                                                     -- declarative FK (same discipline as
                                                     -- trade_busts, migration 051).
    account_id      BIGINT       NOT NULL REFERENCES accounts (id),
    version         INTEGER      NOT NULL DEFAULT 1 CHECK (version >= 1),
    status          confirmation_status_enum NOT NULL DEFAULT 'GENERATED',
    file_ref        VARCHAR(512) NOT NULL,
    content_sha256  CHAR(64)     NOT NULL,
    generated_at    TIMESTAMPTZ  NOT NULL DEFAULT now(),
    delivered_at    TIMESTAMPTZ,                       -- stamped by the Task 20.3.8 delivery engine
    supersedes_id   BIGINT       REFERENCES trade_confirmations (confirmation_id),
    UNIQUE (trade_id, account_id, version)
);
CREATE INDEX trade_confirmations_acct_ix
    ON trade_confirmations (account_id, generated_at DESC);
CREATE INDEX trade_confirmations_trade_ix ON trade_confirmations (trade_id, account_id);
COMMENT ON TABLE trade_confirmations IS
    'Task 20.3.6 per-fill trade confirmations / contract notes (spec §5.28, '
    'MiFID II Art. 25/59). Generation is invoked <=60s post-execution; the '
    'multi-channel delivery engine (Phase-20 Task 20.3.8, internal/reporting) '
    'stamps delivered_at. A busted/price-adjusted trade sets the original row '
    'ADJUSTED and inserts a version+1 row (supersedes_id links the chain). '
    'RETENTION: >= 5 years (MiFID record-keeping).';

CREATE TABLE fee_invoices (
    invoice_id        BIGSERIAL PRIMARY KEY,
    account_id        BIGINT       NOT NULL REFERENCES accounts (id),
    month             DATE         NOT NULL,            -- first day of the invoice month
    currency          VARCHAR(3)   NOT NULL,
    trading_fees      DECIMAL(28,8) NOT NULL DEFAULT 0, -- commission + trading-fee ledger credits
    mm_rebates        DECIMAL(28,8) NOT NULL DEFAULT 0, -- mm_rebate_accruals (>= 0; reduces total)
    connectivity_fees DECIMAL(28,8) NOT NULL DEFAULT 0, -- FIX/connectivity fee source (0 = none)
    total             DECIMAL(28,8) NOT NULL DEFAULT 0, -- trading_fees + connectivity_fees - mm_rebates
    status            invoice_status_enum NOT NULL DEFAULT 'ISSUED',
    file_ref          VARCHAR(512),                     -- object-store key stem "<ref>.pdf"/"<ref>.csv"
    content_sha256    CHAR(64),
    generated_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (account_id, month, currency),
    CHECK (trading_fees >= 0 AND mm_rebates >= 0 AND connectivity_fees >= 0)
);
CREATE INDEX fee_invoices_acct_ix ON fee_invoices (account_id, month DESC);
COMMENT ON TABLE fee_invoices IS
    'Task 20.3.6 monthly institutional fee invoices (spec §16.5). trading_fees '
    'reconcile to GL 4010/4030 revenue credits posted on the account''s fee '
    'journals; mm_rebates derive from mm_rebate_accruals and are absent/zero '
    'for suspended MM programs. RETENTION: >= 7 years (audit/tax).';

CREATE TABLE trial_balances (
    id            BIGSERIAL PRIMARY KEY,
    business_date DATE        NOT NULL,
    currency      VARCHAR(3)  NOT NULL,
    account_code  VARCHAR(48) NOT NULL REFERENCES chart_of_accounts (account_code),
    account_type  gl_account_type_enum NOT NULL,
    debit_total   DECIMAL(28,8) NOT NULL DEFAULT 0,   -- cumulative debits through EOD
    credit_total  DECIMAL(28,8) NOT NULL DEFAULT 0,   -- cumulative credits through EOD
    net_balance   DECIMAL(28,8) NOT NULL DEFAULT 0,   -- debit_total - credit_total (signed)
    generated_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (business_date, currency, account_code)
);
CREATE INDEX trial_balances_date_ix ON trial_balances (business_date, currency);
COMMENT ON TABLE trial_balances IS
    'Task 20.3.7 daily trial-balance read-model cache (spec §16.5, §24 #205): '
    'one row per (business_date, currency, GL account) snapshotting cumulative '
    'ledger_lines totals after the EOD rollover. Always recomputable from '
    'journal_entries/ledger_lines — a cache, never the book of record. '
    'RETENTION: >= 7 years (audit/tax).';

CREATE TABLE erp_delivery_log (
    id              BIGSERIAL PRIMARY KEY,
    run_id          VARCHAR(96) NOT NULL UNIQUE,        -- deterministic per business day
    seq             BIGINT      NOT NULL CHECK (seq > 0), -- deployment-monotonic attempt number
    business_date   DATE        NOT NULL,
    adapter         VARCHAR(16) NOT NULL,               -- 'sftp-dir-drop' | 'webhook'
    manifest_sha256 CHAR(64)    NOT NULL,               -- sha256 of the manifest JSON
    file_count      INTEGER     NOT NULL,
    status          erp_delivery_status_enum NOT NULL DEFAULT 'PENDING',
    delivery_ref    VARCHAR(256),                       -- adapter receipt (dir path / HTTP status)
    error_text      VARCHAR(512),
    attempted_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    delivered_at    TIMESTAMPTZ,
    UNIQUE (seq)
);
COMMENT ON TABLE erp_delivery_log IS
    'Task 20.3.7 ERP nightly-batch dispatch journal (spec §16.5). run_id UNIQUE '
    'is the replay-protection key: a run_id whose row reached SENT is never '
    're-sent (IDEMPOTENCY_KEY_MISMATCH); FAILED attempts retry with a new '
    'monotonic seq so downstream ERP ordering is preserved. RETENTION: >= 7 '
    'years (audit/tax).';

COMMIT;
