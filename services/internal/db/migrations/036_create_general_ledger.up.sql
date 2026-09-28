-- 036_create_general_ledger.up.sql
-- Spec §5.21 double-entry general ledger: chart_of_accounts, journal_entries,
-- ledger_lines (Phase-03 Task 3.3.6).
--
-- Invariants enforced at the database layer (fail-closed, spec §2.7/§5.40):
--   * every ledger line is a pure debit XOR pure credit, amount > 0
--     (CHECK — a two-sided or empty line is malformed, never valid);
--   * SUM(debit_amount) == SUM(credit_amount) per currency per journal entry,
--     enforced by the DEFERRABLE INITIALLY DEFERRED constraint trigger
--     gl_journal_zero_sum_chk which raises LEDGER_IMBALANCE_ABORT at commit;
--   * ledger_lines.account_code FK -> chart_of_accounts: postings to an
--     unseeded/unknown account abort the transaction (Task 3.3.19 AC).
--
-- Deviations from spec §5.21 column lists (additive only, recorded for §27):
--   * chart_of_accounts.account_code widened VARCHAR(32) -> VARCHAR(48):
--     Task 3.3.22's canonical code 2011_PENDING_SETTLEMENT_DELIVERY_{CCY}
--     is 36 chars and would not fit the spec width.
--   * journal_entries: + posted_by VARCHAR(64) (service/admin identity, mirroring
--     ledger_entries.posted_by §5.3), + idempotency_key VARCHAR(128) UNIQUE
--     (zero double-spend — identical replays resolve to the original journal),
--     + payload_sha256 CHAR(64) (replay verification: a replayed key carrying a
--     DIFFERENT payload is rejected IDEMPOTENCY_KEY_MISMATCH, never reapplied).
--   * gl_entry_type_enum: spec's 7 values PLUS 'TRANSFER' and 'ADJUSTMENT'
--     (§5.3 invariant 4 requires internal transfers to post journals with
--     entry_type='TRANSFER'; ADJUSTMENT covers operator corrections/NBP).
--   * ledger_lines: + narrative VARCHAR(255) (per-line audit narrative —
--     negative-rate sign preservation per §5.21a lives on the swap lines).

BEGIN;

CREATE TYPE gl_account_type_enum AS ENUM ('ASSET', 'LIABILITY', 'EQUITY', 'REVENUE', 'EXPENSE');
CREATE TYPE gl_entry_type_enum AS ENUM (
    'TRADE_FILL', 'DEPOSIT', 'WITHDRAWAL', 'FEE',
    'EOD_ROLLOVER', 'LIQUIDATION', 'SETTLEMENT',
    'TRANSFER', 'ADJUSTMENT'                     -- §5.3 zero-GL-bypass additions
);

CREATE TABLE chart_of_accounts (
    id           BIGSERIAL PRIMARY KEY,
    account_code VARCHAR(48)  NOT NULL UNIQUE,   -- e.g. '1010_NOSTRO_USD'
    account_name VARCHAR(128) NOT NULL,
    account_type gl_account_type_enum NOT NULL,
    currency     VARCHAR(3)   NOT NULL,          -- ISO fiat currency
    created_at   TIMESTAMPTZ  NOT NULL DEFAULT now()
);

CREATE TABLE journal_entries (
    id              BIGSERIAL PRIMARY KEY,
    entry_type      gl_entry_type_enum NOT NULL,
    reference_id    BIGINT,                       -- FK to trades/funding_transactions/…
    description     VARCHAR(255) NOT NULL,
    posted_by       VARCHAR(64)  NOT NULL,        -- service or admin identifier
    idempotency_key VARCHAR(128),                 -- NULL = no replay dedup
    payload_sha256  CHAR(64),                     -- canonical journal hash (replay check)
    posted_at       TIMESTAMPTZ  NOT NULL DEFAULT now()
);

-- Replay dedup: a re-delivered posting request resolves to the original journal.
CREATE UNIQUE INDEX journal_entries_idem_ux
    ON journal_entries (idempotency_key)
    WHERE idempotency_key IS NOT NULL;
CREATE INDEX journal_entries_ref_ix ON journal_entries (entry_type, reference_id);

CREATE TABLE ledger_lines (
    id               BIGSERIAL PRIMARY KEY,
    journal_entry_id BIGINT       NOT NULL REFERENCES journal_entries (id),
    account_code     VARCHAR(48)  NOT NULL REFERENCES chart_of_accounts (account_code),
    debit_amount     DECIMAL(28,8) NOT NULL DEFAULT 0,
    credit_amount    DECIMAL(28,8) NOT NULL DEFAULT 0,
    currency         VARCHAR(3)   NOT NULL,
    narrative        VARCHAR(255),
    created_at       TIMESTAMPTZ  NOT NULL DEFAULT now(),
    -- A line is a pure debit XOR a pure credit; zero-sided/two-sided lines are malformed.
    CHECK (debit_amount >= 0 AND credit_amount >= 0),
    CHECK ((debit_amount > 0 AND credit_amount = 0) OR (credit_amount > 0 AND debit_amount = 0))
);
CREATE INDEX ledger_lines_journal_ix ON ledger_lines (journal_entry_id);
CREATE INDEX ledger_lines_account_ix ON ledger_lines (account_code);

-- Zero-sum invariant (spec §5.21/§5.40): checked per currency at COMMIT so a
-- journal's lines may be inserted in any order inside the transaction.
-- Covers UPDATE/DELETE too — mutating a committed journal's lines trips the
-- same abort (ledger corrections must be reversal journals, not edits).
CREATE OR REPLACE FUNCTION gl_check_journal_balance() RETURNS trigger AS $$
DECLARE
    v_journal BIGINT := COALESCE(NEW.journal_entry_id, OLD.journal_entry_id);
BEGIN
    IF EXISTS (
        SELECT 1
        FROM ledger_lines
        WHERE journal_entry_id = v_journal
        GROUP BY currency
        HAVING SUM(debit_amount) <> SUM(credit_amount)
    ) THEN
        RAISE EXCEPTION 'LEDGER_IMBALANCE_ABORT: journal_entry % violates SUM(debit)=SUM(credit) per currency',
            v_journal;
    END IF;
    RETURN NULL;
END;
$$ LANGUAGE plpgsql;

CREATE CONSTRAINT TRIGGER gl_journal_zero_sum_chk
    AFTER INSERT OR UPDATE OR DELETE ON ledger_lines
    DEFERRABLE INITIALLY DEFERRED
    FOR EACH ROW EXECUTE FUNCTION gl_check_journal_balance();

-- Task 3.3.6 seed set (per-currency sub-accounts; spec §5.21 examples).
-- The full production chart lands in migration 088 (Task 3.3.19) and seeds
-- this four-account set as a subset.
INSERT INTO chart_of_accounts (account_code, account_name, account_type, currency)
SELECT fmt.code || '_' || c.ccy, fmt.name, fmt.typ::gl_account_type_enum, c.ccy
FROM (VALUES
    ('USD'), ('EUR'), ('GBP'), ('JPY'), ('AUD'),
    ('CAD'), ('CHF'), ('NZD'), ('MXN')
) AS c (ccy)
CROSS JOIN (VALUES
    ('1010_NOSTRO',             'Nostro operating account',     'ASSET'),
    ('2010_CUSTOMER_LIABILITY', 'Customer balance liability',   'LIABILITY'),
    ('4010_TRADING_FEE_REVENUE','Trading fee revenue',          'REVENUE'),
    ('5010_LIQUIDATION_PENALTY','Liquidation penalty clearing', 'EXPENSE')
) AS fmt (code, name, typ);

COMMIT;
