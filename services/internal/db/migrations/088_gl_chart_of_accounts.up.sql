-- 088_gl_chart_of_accounts.up.sql
-- Spec §5.21a (Phase-03 Task 3.3.19): production chart of accounts,
-- swap-markup policy, per-currency day-count, non-trading fee schedule,
-- and the swap accrual audit trail.
--
-- Numbering plan — client-vs-house segregation is STRUCTURAL:
--   1xxx  house assets        1100–1149 client-segregated money assets
--                           1150–1199  restricted house assets
--   2xxx  liabilities         2000–2199  client liabilities / suspense / transit
--                           2200–2999  house liabilities
--   3xxx  house equity        4xxx  house revenue      5xxx  house expense
--
-- Doc contradiction noted for §27 ruling: Task 3.3.23 text credits
-- 4020_SWAPFREE_ADMIN_REVENUE_{CCY} while spec §5.45.3 says "GL 4300" —
-- BOTH codes are seeded (4020_SWAPFREE_ADMIN_REVENUE and
-- 4300_SWAPFREE_ADMIN_FEE) so either consumer resolves fail-safe.

BEGIN;

-- ── 1. Full chart of accounts (supersedes the 036 three-example seed;
--       the Task 3.3.6 set is a strict subset — ON CONFLICT keeps it) ────
INSERT INTO chart_of_accounts (account_code, account_name, account_type, currency)
SELECT fmt.code || '_' || c.ccy, fmt.name || ' (' || c.ccy || ')', fmt.typ::gl_account_type_enum, c.ccy
FROM (VALUES
    ('USD'), ('EUR'), ('GBP'), ('JPY'), ('AUD'),
    ('CAD'), ('CHF'), ('NZD'), ('MXN')
) AS c (ccy)
CROSS JOIN (VALUES
    -- house assets
    ('1010_NOSTRO',                  'Nostro operating account',                    'ASSET'),
    ('1020_NOSTRO_CLEARING',         'Nostro clearing / in-transit',                'ASSET'),
    ('1200_MULTI_CURRENCY_CLEARING', 'Multi-currency clearing (auto-exchange)',     'ASSET'),
    -- client-segregated money assets (1100–1149 = CLIENT side)
    ('1110_CLIENT_MONEY_SEGREGATED', 'Segregated client-money bank account',        'ASSET'),
    -- restricted house assets
    ('1150_INSURANCE_FUND_NOSTRO',   'Insurance fund segregated nostro',            'ASSET'),
    -- client liabilities & transit (2000–2199 = CLIENT side)
    ('2010_CUSTOMER_LIABILITY',      'Customer balance liability',                  'LIABILITY'),
    ('2011_PENDING_SETTLEMENT_DELIVERY', 'Physical-delivery pending settlement',    'LIABILITY'),
    ('2100_CLIENT_COLLATERAL',       'Client collateral held',                      'LIABILITY'),
    ('2150_SUSPENSE_DEPOSITS',       'Suspense — unmatched deposits',               'LIABILITY'),
    ('2160_CLEARING_TRANSIT',        'Suspense — clearing transit',                 'LIABILITY'),
    -- house liabilities
    ('2210_INSURANCE_FUND_LIABILITY','Insurance fund liability',                    'LIABILITY'),
    -- house equity
    ('3010_HOUSE_EQUITY',            'House equity',                                'EQUITY'),
    ('3020_RETAINED_EARNINGS',       'Retained earnings',                           'EQUITY'),
    -- house revenue
    ('4010_TRADING_FEE_REVENUE',     'Trading fee revenue',                         'REVENUE'),
    ('4020_SWAPFREE_ADMIN_REVENUE',  'Swap-free admin fee revenue (Task 3.3.23)',   'REVENUE'),
    ('4030_COMMISSION_REVENUE',      'Commission revenue (raw-spread model)',       'REVENUE'),
    ('4100_SWAP_ROLLOVER_REVENUE',   'Swap/rollover interbank financing revenue',   'REVENUE'),
    ('4110_SWAP_MARKUP_REVENUE',     'Swap admin markup revenue',                   'REVENUE'),
    ('4200_FUNDING_FEE_REVENUE',     'Funding fee revenue',                         'REVENUE'),
    ('4300_SWAPFREE_ADMIN_FEE',      'Swap-free admin fee revenue (§5.45 GL 4300)', 'REVENUE'),
    ('4400_CONVERSION_SPREAD_REVENUE','Currency conversion spread revenue',         'REVENUE'),
    ('4500_INACTIVITY_FEE_REVENUE',  'Inactivity/dormancy fee revenue',             'REVENUE'),
    -- house expense
    ('5010_LIQUIDATION_PENALTY',     'Liquidation penalty clearing',                'EXPENSE'),
    ('5100_LIQUIDITY_REBATE_EXPENSE','Liquidity-provider rebate expense',           'EXPENSE'),
    ('5200_NBP_RESTITUTION_EXPENSE', 'Retail NBP restitution expense',              'EXPENSE'),
    ('5300_BANK_RAIL_FEE_EXPENSE',   'Banking-rail fee expense',                    'EXPENSE')
) AS fmt (code, name, typ)
ON CONFLICT (account_code) DO NOTHING;

-- ── 2. Per-currency accrual day-count (§5.21a.2, Task 22.3.1 convention) ──
CREATE TYPE day_count_enum AS ENUM ('ACT/360', 'ACT/365');

CREATE TABLE currency_day_counts (
    currency   VARCHAR(3) PRIMARY KEY,
    day_count  day_count_enum NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Money-market convention: ACT/360 for USD/EUR/JPY/CHF/CAD/MXN corridors;
-- ACT/365 for GBP/AUD/NZD (sterling & commonwealth convention).
INSERT INTO currency_day_counts (currency, day_count) VALUES
    ('USD', 'ACT/360'), ('EUR', 'ACT/360'), ('JPY', 'ACT/360'),
    ('CHF', 'ACT/360'), ('CAD', 'ACT/360'), ('MXN', 'ACT/360'),
    ('GBP', 'ACT/365'), ('AUD', 'ACT/365'), ('NZD', 'ACT/365');

-- ── 3. Swap markup policy (per instrument, bps, dual-controlled) ─────────
CREATE TYPE swap_markup_status_enum AS ENUM ('PENDING_APPROVAL', 'ACTIVE', 'RETIRED');

CREATE TABLE swap_markup_policies (
    id               BIGSERIAL PRIMARY KEY,
    instrument_id    BIGINT REFERENCES instruments (id),   -- NULL = global default
    long_markup_bps  DECIMAL(9,4) NOT NULL DEFAULT 0,
    short_markup_bps DECIMAL(9,4) NOT NULL DEFAULT 0,
    status           swap_markup_status_enum NOT NULL DEFAULT 'PENDING_APPROVAL',
    proposed_by      VARCHAR(64) NOT NULL,
    approved_by      VARCHAR(64),
    effective_from   TIMESTAMPTZ NOT NULL DEFAULT now(),
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    CHECK (long_markup_bps >= 0 AND short_markup_bps >= 0),
    -- dual control: ACTIVE requires a distinct approver, structurally
    CHECK (status <> 'ACTIVE' OR (approved_by IS NOT NULL AND approved_by <> proposed_by))
);
CREATE INDEX swap_markup_policies_ix ON swap_markup_policies (instrument_id, status);

-- ── 4. Non-trading fee schedule (§5.21a.3) ───────────────────────────────
CREATE TYPE non_trading_fee_kind_enum   AS ENUM ('INACTIVITY', 'DORMANCY', 'CONVERSION_SPREAD', 'FINANCING_SPREAD');
CREATE TYPE non_trading_fee_unit_enum   AS ENUM ('FLAT', 'BPS', 'PCT_PER_ANNUM');
CREATE TYPE non_trading_fee_status_enum AS ENUM ('PENDING_APPROVAL', 'ACTIVE', 'RETIRED');

CREATE TABLE non_trading_fee_schedule (
    id               BIGSERIAL PRIMARY KEY,
    kind             non_trading_fee_kind_enum NOT NULL,
    instrument_id    BIGINT REFERENCES instruments (id),   -- NULL = global default
    days_dormant_min INTEGER NOT NULL DEFAULT 0,           -- dormancy tier lower bound
    days_dormant_max INTEGER NOT NULL DEFAULT 0,           -- 0 = unbounded
    amount           DECIMAL(28,8) NOT NULL,
    unit             non_trading_fee_unit_enum NOT NULL DEFAULT 'FLAT',
    currency         VARCHAR(3) NOT NULL,
    vat_applicable   BOOLEAN NOT NULL DEFAULT FALSE,       -- consumed by Phase-20 invoicing (Task 20.3.6)
    status           non_trading_fee_status_enum NOT NULL DEFAULT 'PENDING_APPROVAL',
    proposed_by      VARCHAR(64) NOT NULL,
    approved_by      VARCHAR(64),
    effective_from   TIMESTAMPTZ NOT NULL DEFAULT now(),
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    CHECK (amount >= 0),
    CHECK (days_dormant_min >= 0 AND days_dormant_max >= 0),
    CHECK (days_dormant_max = 0 OR days_dormant_max >= days_dormant_min),
    CHECK (status <> 'ACTIVE' OR (approved_by IS NOT NULL AND approved_by <> proposed_by))
);
CREATE INDEX non_trading_fee_ix ON non_trading_fee_schedule (kind, instrument_id, currency, status);

-- ── 5. Swap accrual audit trail — interbank/markup split for §17.15
--       history and the swap-free foregone-amount report (§5.21a.2) ───────
CREATE TYPE swap_side_enum AS ENUM ('LONG', 'SHORT');

CREATE TABLE swap_accrual_records (
    id               BIGSERIAL PRIMARY KEY,
    journal_entry_id BIGINT REFERENCES journal_entries (id),  -- NULL for swap-free accruals
    account_id       BIGINT NOT NULL,
    position_id      BIGINT NOT NULL,
    instrument_id    BIGINT NOT NULL,
    symbol           VARCHAR(32) NOT NULL,
    side             swap_side_enum NOT NULL,
    currency         VARCHAR(3) NOT NULL,
    days             INTEGER NOT NULL CHECK (days >= 1),
    day_count        day_count_enum NOT NULL,
    interbank_amount DECIMAL(28,8) NOT NULL,        -- signed client delta, interbank leg
    markup_amount    DECIMAL(28,8) NOT NULL,        -- house markup charge (>= 0)
    client_delta     DECIMAL(28,8) NOT NULL,        -- signed total applied to the wallet
    markup_bps       DECIMAL(9,4) NOT NULL,
    swap_free        BOOLEAN NOT NULL DEFAULT FALSE,
    foregone_amount  DECIMAL(28,8) NOT NULL DEFAULT 0, -- swap-free: would-be accrual
    narrative        VARCHAR(255) NOT NULL,
    accrued_at       TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX swap_accrual_records_ix
    ON swap_accrual_records (instrument_id, currency, accrued_at);

COMMIT;
