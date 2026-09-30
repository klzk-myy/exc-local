-- 257_premium_feed_subscriptions.up.sql
-- Phase-23 Task 23.3.3 — premium data feed subscriptions (spec §10;
-- bundle: premium_l3 / depth_full / auction / greeks per Tasks
-- 23.3.3 + 23.3.5, §24 #250).
--
-- premium_feed_subscriptions: one row per (account, feed) subscription.
--   status:   ACTIVE | CANCELLED — ACTIVE alone entitles the WS bind
--             (FeedEntitlements gate); CANCELLED is the revocation
--             path the backoffice runs (payment failure is a billing
--             concern, never a mid-period gate — sweeps retry).
--   billed_monthly_minor: monthly price in minor currency units (int8
--             per task contract; exponent resolved per ISO 4217 — JPY
--             class 0, BHD class 3, default 2 — marketdata.ccyExponent).
--   renews_at: next billing boundary; the monthly sweep bills the
--             period STARTING at renews_at and rolls it +1 month.
--   last_journal_id: the GL journal_entries row for the last charge —
--             audit linkage to ledger_lines (zero-GL-bypass, §5.3).
--
-- Partial unique index enforces at most one ACTIVE row per
-- (account_id, feed_name) — a resubscribe must cancel or revive the
-- prior row, never fork two parallel subscriptions.

BEGIN;

CREATE TYPE premium_feed_status_enum AS ENUM ('ACTIVE', 'CANCELLED');

CREATE TABLE premium_feed_subscriptions (
    id                   BIGSERIAL    PRIMARY KEY,
    account_id           BIGINT       NOT NULL REFERENCES accounts (id),
    feed_name            VARCHAR(32)  NOT NULL CHECK (feed_name IN (
                          'premium_l3','depth_full','auction','greeks')),
    status               premium_feed_status_enum NOT NULL DEFAULT 'ACTIVE',
    started_at           TIMESTAMPTZ  NOT NULL DEFAULT now(),
    billed_monthly_minor BIGINT       NOT NULL CHECK (billed_monthly_minor >= 0),
    currency             VARCHAR(3)   NOT NULL,          -- ISO 4217 char(3)
    renews_at            TIMESTAMPTZ  NOT NULL,          -- next billing boundary
    last_billed_at       TIMESTAMPTZ,
    last_journal_id      BIGINT,                          -- → journal_entries.id (audit link, no FK)
    cancelled_at         TIMESTAMPTZ,
    created_at           TIMESTAMPTZ  NOT NULL DEFAULT now(),
    updated_at           TIMESTAMPTZ  NOT NULL DEFAULT now(),
    CHECK (status <> 'CANCELLED' OR cancelled_at IS NOT NULL)
);

-- One ACTIVE subscription per (account, feed).
CREATE UNIQUE INDEX premium_feed_subscriptions_active_ux
    ON premium_feed_subscriptions (account_id, feed_name)
    WHERE status = 'ACTIVE';

-- Monthly billing sweep: due ACTIVE rows oldest-first.
CREATE INDEX premium_feed_subscriptions_due_ix
    ON premium_feed_subscriptions (renews_at)
    WHERE status = 'ACTIVE';

COMMENT ON TABLE premium_feed_subscriptions IS
    'Phase-23 Task 23.3.3 — per-feed monthly premium market-data subscriptions. '
    'ACTIVE row entitles the WS bind (FeedEntitlements); the billing sweep posts '
    'a FEE journal per renewal (ledger.JournalPoster seam).';

COMMIT;
