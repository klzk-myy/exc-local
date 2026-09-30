-- 079_employee_dealing.up.sql
-- Phase-21 Task 21.3.24 — Employee Dealing & Insider-Information
-- Controls (spec §14.10.1, §24 #327).
--
--   accounts.employee_account / employee_role — the §5.2 staff flags
--       (employee_account was reserved by remediation #35 in migration
--       003's column map; this is its owning migration).
--   restricted_lists       — blackout windows around privileged venue
--                            events (auctions, oracle outages,
--                            maintenance, emergency rule changes, rate
--                            fixes). scope steers who is covered.
--   pre_clearance_requests — employee trade pre-clearance; APPROVED
--                            grants order entry until expires_at
--                            (≤24h). outcome transitions are written
--                            once — decided rows are immutable
--                            (trigger below) so the audit trail is a
--                            pure append.
--   employee_trade_reviews — every fill on an employee account lands
--                            here for same-day Compliance Officer
--                            review (spec §14.10.1 item: staff trades
--                            route to the officer queue).

BEGIN;

ALTER TABLE accounts
    ADD COLUMN IF NOT EXISTS employee_account BOOLEAN    NOT NULL DEFAULT false,
    ADD COLUMN IF NOT EXISTS employee_role    VARCHAR(32);          -- NULL = non-staff / undesignated

COMMENT ON COLUMN accounts.employee_account IS
    'Phase-21 Task 21.3.24 — staff account flag: excluded from '
    'liquidity/STP/rebate programs; order entry gates on '
    'pre-clearance for restricted instruments and SENSITIVE_ROLES.';
COMMENT ON COLUMN accounts.employee_role IS
    'Phase-21 Task 21.3.24 — organisational role driving '
    'SENSITIVE_ROLES membership (CORE_ENGINE|CORE_OPS|COMPLIANCE|'
    'LP_MANAGEMENT|PRODUCT are sensitive).';

CREATE TABLE restricted_lists (
    id            BIGSERIAL    PRIMARY KEY,
    event_id      VARCHAR(64)  NOT NULL,                -- operator event key (auction id, maint ticket, fix date…)
    event_type    VARCHAR(24)  NOT NULL CHECK (event_type IN (
                       'AUCTION','ORACLE_OUTAGE','MAINTENANCE',
                       'EMERGENCY_RULE_CHANGE','RATE_FIX')),
    instruments   TEXT[]       NOT NULL DEFAULT '{}',   -- empty = venue-wide blackout
    window_start  TIMESTAMPTZ  NOT NULL,                -- stored window (auto-widened by sync)
    window_end    TIMESTAMPTZ  NOT NULL,
    widen_minutes INTEGER      NOT NULL DEFAULT 30 CHECK (widen_minutes BETWEEN 0 AND 1440),
    scope         VARCHAR(16)  NOT NULL DEFAULT 'ALL_EMPLOYEES' CHECK (scope IN (
                       'ALL_EMPLOYEES','ROLE','NAMED')),
    scope_role    VARCHAR(32),                          -- SCOPE=ROLE target (employee_role value)
    named_accounts BIGINT[]    NOT NULL DEFAULT '{}',   -- SCOPE=NAMED target account ids
    status        VARCHAR(12)  NOT NULL DEFAULT 'ACTIVE' CHECK (status IN ('ACTIVE','RETIRED')),
    reason        TEXT         NOT NULL DEFAULT '',
    created_by    BIGINT       NOT NULL,                -- Compliance Officer user id
    created_at    TIMESTAMPTZ  NOT NULL DEFAULT now(),
    updated_at    TIMESTAMPTZ  NOT NULL DEFAULT now(),
    CONSTRAINT restricted_lists_window_ck CHECK (window_end > window_start),
    CONSTRAINT restricted_lists_scope_ck  CHECK (
        scope <> 'ROLE'  OR scope_role IS NOT NULL)
);

CREATE INDEX idx_restricted_lists_active ON restricted_lists (window_start, window_end)
    WHERE status = 'ACTIVE';
CREATE INDEX idx_restricted_lists_event  ON restricted_lists (event_id);

COMMENT ON TABLE restricted_lists IS
    'Phase-21 Task 21.3.24 — employee-dealing restricted list. '
    'Employee order entry on covered instruments inside '
    '[window_start - widen_minutes, window_end + widen_minutes] '
    'rejects EMPLOYEE_DEALING_PRECLEARANCE_REQUIRED (422); the gateway '
    'admission seam is authoritative (UI blocking is UX only).';

CREATE TABLE pre_clearance_requests (
    id           BIGSERIAL    PRIMARY KEY,
    request_id   VARCHAR(40)  NOT NULL UNIQUE,          -- "pcl_<26urlsafe>" public id
    account_id   BIGINT       NOT NULL REFERENCES accounts (id),
    instrument   VARCHAR(32)  NOT NULL,                 -- canonical symbol or '' = all covered instruments
    side         VARCHAR(4)   CHECK (side IN ('BUY','SELL')),
    notional_cap DECIMAL(28,8),                         -- optional approved-notional bound
    reason       TEXT         NOT NULL,                 -- employee's stated purpose (mandatory)
    outcome      VARCHAR(8)   NOT NULL DEFAULT 'PENDING' CHECK (outcome IN (
                      'PENDING','APPROVED','DENIED','EXPIRED')),
    expires_at   TIMESTAMPTZ  NOT NULL,                 -- request AND approval horizon (≤24h)
    requested_by BIGINT       NOT NULL,                 -- employee user id
    decided_by   BIGINT,                                -- independent controller (role-gated)
    decided_at   TIMESTAMPTZ,
    decision_note TEXT,
    first_used_at TIMESTAMPTZ,                          -- first order admitted under the grant
    created_at   TIMESTAMPTZ  NOT NULL DEFAULT now(),
    updated_at   TIMESTAMPTZ  NOT NULL DEFAULT now()
);

CREATE INDEX idx_pre_clearance_pending ON pre_clearance_requests (created_at)
    WHERE outcome = 'PENDING';
CREATE INDEX idx_pre_clearance_account ON pre_clearance_requests
    (account_id, instrument, outcome, expires_at);

COMMENT ON TABLE pre_clearance_requests IS
    'Phase-21 Task 21.3.24 — employee trade pre-clearance. APPROVED + '
    'unexpired admits the covered order flow at the gateway admission '
    'seam; DENIED/EXPIRED are terminal. Decisions are append-only '
    '(immutability trigger) — re-clearance means a new request.';

-- Decided pre-clearance rows are immutable: only first_used_at may be
-- stamped afterwards (the consumption marker), never the decision.
CREATE OR REPLACE FUNCTION p21_pre_clearance_immutable() RETURNS trigger
    LANGUAGE plpgsql AS $$
BEGIN
    IF OLD.outcome IN ('APPROVED','DENIED','EXPIRED') THEN
        IF NEW.outcome IS DISTINCT FROM OLD.outcome
           OR NEW.decided_by IS DISTINCT FROM OLD.decided_by
           OR NEW.decision_note IS DISTINCT FROM OLD.decision_note THEN
            RAISE EXCEPTION 'decided pre_clearance_requests rows are immutable';
        END IF;
    END IF;
    RETURN NEW;
END $$;

CREATE TRIGGER pre_clearance_immutable_trg
    BEFORE UPDATE ON pre_clearance_requests
    FOR EACH ROW EXECUTE FUNCTION p21_pre_clearance_immutable();

CREATE TABLE employee_trade_reviews (
    id          BIGSERIAL    PRIMARY KEY,
    account_id  BIGINT       NOT NULL REFERENCES accounts (id),
    order_id    BIGINT,                                -- orders row when resolvable
    trade_id    BIGINT,                                -- fills row when resolvable
    symbol      VARCHAR(32)  NOT NULL DEFAULT '',
    side        VARCHAR(4),
    quantity    DECIMAL(28,8),
    price       DECIMAL(28,8),
    restricted_event_id VARCHAR(64),                   -- set when inside a blackout window
    status      VARCHAR(12)  NOT NULL DEFAULT 'PENDING' CHECK (status IN (
                     'PENDING','REVIEWED','ESCALATED')),
    reviewed_by BIGINT,
    reviewed_at TIMESTAMPTZ,
    review_note TEXT,
    escalated_at TIMESTAMPTZ,                          -- P1 marker time
    created_at  TIMESTAMPTZ  NOT NULL DEFAULT now()
);

CREATE INDEX idx_employee_trade_reviews_open ON employee_trade_reviews (created_at)
    WHERE status = 'PENDING';
CREATE INDEX idx_employee_trade_reviews_acct ON employee_trade_reviews (account_id, id DESC);

COMMENT ON TABLE employee_trade_reviews IS
    'Phase-21 Task 21.3.24 — staff-trade review queue: every fill on an '
    'employee_account lands here for same-day Compliance Officer '
    'review; a profitable-looking trade inside a restricted window '
    'escalates P1 (restricted_event_id set by the observer).';

COMMIT;
