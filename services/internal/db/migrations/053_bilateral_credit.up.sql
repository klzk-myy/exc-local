-- Migration 053: bilateral credit (spec §5.30, Phase-19 Task 19.3.10).
--
-- Directed institutional credit lines. A credit_group is owned by one
-- grantor account; each credit_relationship is a directed edge
-- grantor_group → grantee account with gross/net limits, product-pool
-- scope, effective/expiry window, block flag and optimistic version.
--
-- credit_parties (additive mapping not spelled out in §5.30, recorded
-- under §27) assigns every credit-addressable account a fixed 0..1023
-- index — the row/column coordinate in the C++ /exchange_credit_matrix
-- shared-memory ABI (1024 parties, cell[grantor][grantee]).
--
-- credit_reservations records every atomic pre-trade headroom debit so
-- reservation → utilization transitions survive restart/replay.
BEGIN;

CREATE TABLE IF NOT EXISTS credit_groups (
    id                  BIGSERIAL PRIMARY KEY,
    grantor_account_id  BIGINT NOT NULL REFERENCES accounts(id),
    name                VARCHAR(64) NOT NULL,
    profile             VARCHAR(16) NOT NULL DEFAULT 'ONE_POOL'
        CHECK (profile IN ('ONE_POOL', 'TWO_POOL')),
    status              VARCHAR(16) NOT NULL DEFAULT 'ACTIVE'
        CHECK (status IN ('ACTIVE', 'SUSPENDED', 'CLOSED')),
    created_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (grantor_account_id, name)
);

-- Account → fixed matrix party index (0..1023 = BilateralCreditMatrix
-- MAX_PARTIES). Assignment is monotonic and never reused; a party that
-- retires keeps its index so historical rows still decode.
CREATE TABLE IF NOT EXISTS credit_parties (
    account_id          BIGINT PRIMARY KEY REFERENCES accounts(id),
    party_index         INT NOT NULL UNIQUE
        CHECK (party_index >= 0 AND party_index < 1024),
    created_at          TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS credit_relationships (
    id                  BIGSERIAL PRIMARY KEY,
    grantor_group_id    BIGINT NOT NULL REFERENCES credit_groups(id),
    grantee_account_id  BIGINT NOT NULL REFERENCES accounts(id),
    product_pool        VARCHAR(16) NOT NULL DEFAULT 'ALL'
        CHECK (product_pool IN ('ALL', 'SPOT', 'FORWARD_NDF')),
    -- NULL limit = uncapped (still screened against the opposite edge).
    gross_limit         DECIMAL(28,8),
    net_limit           DECIMAL(28,8),
    current_gross       DECIMAL(28,8) NOT NULL DEFAULT 0
        CHECK (current_gross >= 0),
    current_net         DECIMAL(28,8) NOT NULL DEFAULT 0,
    effective_at        TIMESTAMPTZ,
    expires_at          TIMESTAMPTZ,
    blocked             BOOLEAN NOT NULL DEFAULT false,
    version             BIGINT NOT NULL DEFAULT 1,
    created_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (grantor_group_id, grantee_account_id, product_pool),
    CHECK (gross_limit IS NULL OR gross_limit >= 0),
    CHECK (net_limit IS NULL OR net_limit >= 0)
);

CREATE INDEX IF NOT EXISTS idx_credit_relationships_grantee
    ON credit_relationships (grantee_account_id);
CREATE INDEX IF NOT EXISTS idx_credit_relationships_group
    ON credit_relationships (grantor_group_id);

CREATE TABLE IF NOT EXISTS credit_reservations (
    id                  BIGSERIAL PRIMARY KEY,
    order_id            BIGINT NOT NULL,
    relationship_id     BIGINT REFERENCES credit_relationships(id),
    account_id          BIGINT NOT NULL REFERENCES accounts(id),
    product_pool        VARCHAR(16) NOT NULL DEFAULT 'ALL',
    reserved_amount     DECIMAL(28,8) NOT NULL CHECK (reserved_amount >= 0),
    status              VARCHAR(16) NOT NULL DEFAULT 'ACTIVE'
        CHECK (status IN ('ACTIVE', 'CONSUMED', 'RELEASED', 'EXPIRED')),
    expires_at          TIMESTAMPTZ NOT NULL,
    created_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at          TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE UNIQUE INDEX IF NOT EXISTS uq_credit_reservations_order
    ON credit_reservations (order_id) WHERE status = 'ACTIVE';
CREATE INDEX IF NOT EXISTS idx_credit_reservations_status
    ON credit_reservations (status, expires_at);
CREATE INDEX IF NOT EXISTS idx_credit_reservations_rel
    ON credit_reservations (relationship_id) WHERE status = 'ACTIVE';

COMMIT;
