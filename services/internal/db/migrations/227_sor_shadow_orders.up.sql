-- 227_sor_shadow_orders.up.sql
-- Phase-18 Task 18.3.14 — Smart Order Routing (SOR) shadow orders
-- (spec §9.8, §24 #193/#242).
--
-- Every externally routed order mints a shadow row in the local OMS —
-- parent order, venue, external order id, the canonical 5-state
-- lifecycle, and fill accumulation — plus the FILL_BRIDGE dedup key
-- (venue_id, external_order_id, exec_id) so replayed venue reports
-- never double-apply.
--
-- Lifecycle (spec §24 #242):
--   PENDING_ROUTE → ROUTED → PARTIALLY_FILLED_EXTERNAL →
--   FILLED_EXTERNAL | CANCELLED_EXTERNAL
-- 500ms venue non-response timeout auto-cancels and walks to the next
-- venue (SOR_TIMEOUT on exhaustion). The partial unique index below
-- enforces the race-prevention invariant in SQL: a parent order may
-- hold at most one NON-terminal shadow, so a live external route and a
-- local working order can never coexist for the same parent.

BEGIN;

CREATE TABLE sor_shadow_orders (
    id                 BIGSERIAL PRIMARY KEY,
    parent_order_id    BIGINT       NOT NULL REFERENCES orders (id),
    account_id         BIGINT       NOT NULL REFERENCES accounts (id),
    venue_id           VARCHAR(32)  NOT NULL,
    external_order_id  VARCHAR(64)  NOT NULL DEFAULT '',
    symbol             VARCHAR(32)  NOT NULL,
    side               VARCHAR(4)   NOT NULL CHECK (side IN ('BUY','SELL')),
    qty                DECIMAL(28,8) NOT NULL CHECK (qty > 0),
    filled_qty         DECIMAL(28,8) NOT NULL DEFAULT 0 CHECK (filled_qty >= 0),
    avg_fill_price     DECIMAL(20,8),
    state              VARCHAR(26)  NOT NULL DEFAULT 'PENDING_ROUTE'
                       CHECK (state IN ('PENDING_ROUTE','ROUTED',
                                        'PARTIALLY_FILLED_EXTERNAL',
                                        'FILLED_EXTERNAL','CANCELLED_EXTERNAL')),
    attempt            INTEGER      NOT NULL DEFAULT 1,
    last_error         TEXT         NOT NULL DEFAULT '',
    routed_at          TIMESTAMPTZ,
    completed_at       TIMESTAMPTZ,
    created_at         TIMESTAMPTZ  NOT NULL DEFAULT now(),
    updated_at         TIMESTAMPTZ  NOT NULL DEFAULT now()
);

-- One open shadow per parent order (race-prevention invariant).
CREATE UNIQUE INDEX sor_shadow_open_parent_uq
    ON sor_shadow_orders (parent_order_id)
    WHERE state NOT IN ('FILLED_EXTERNAL','CANCELLED_EXTERNAL');

CREATE INDEX sor_shadow_venue_ext_idx
    ON sor_shadow_orders (venue_id, external_order_id);

CREATE INDEX sor_shadow_account_idx
    ON sor_shadow_orders (account_id, created_at DESC);

-- FILL_BRIDGE reconciliation dedup: venue exec ids are unique per
-- (venue, external order); replayed reports ON CONFLICT DO NOTHING.
CREATE TABLE sor_fill_dedup (
    id                 BIGSERIAL PRIMARY KEY,
    shadow_order_id    BIGINT       NOT NULL REFERENCES sor_shadow_orders (id),
    venue_id           VARCHAR(32)  NOT NULL,
    external_order_id  VARCHAR(64)  NOT NULL,
    exec_id            VARCHAR(64)  NOT NULL,
    qty                DECIMAL(28,8) NOT NULL,
    price              DECIMAL(20,8) NOT NULL,
    created_at         TIMESTAMPTZ  NOT NULL DEFAULT now(),
    UNIQUE (venue_id, external_order_id, exec_id)
);

CREATE INDEX sor_fill_dedup_shadow_idx ON sor_fill_dedup (shadow_order_id);

COMMIT;
