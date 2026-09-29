-- 206_circuit_breaker_events.up.sql
-- Phase-13 Tasks 13.3.1 / 13.3.9 — durable audit record of every
-- five-tier circuit-breaker state transition (spec §2.6, §24 #313).
--
-- Redis `circuit_breaker:{scope}:{id}` HASH keys are the hot-path state
-- store consulted on every order admission; this table is the immutable
-- telemetry trail — trigger parameters at trip time, the acting admin on
-- manual transitions (NULL = automated), effective hold (post-flap
-- doubling) and probe counts on probe-window resolutions.

BEGIN;

CREATE TYPE circuit_breaker_scope_enum AS ENUM (
    'INSTRUMENT', 'ACCOUNT', 'VOLUME_SPIKE', 'OPTIONS_VOLATILITY', 'MARKET_WIDE'
);
CREATE TYPE circuit_breaker_state_enum AS ENUM ('CLOSED', 'OPEN', 'HALF_OPEN');

CREATE TABLE circuit_breaker_events (
    event_id    BIGSERIAL PRIMARY KEY,
    scope       circuit_breaker_scope_enum NOT NULL,
    target_id   VARCHAR(128) NOT NULL,              -- symbol | account id | 'MARKET'
    from_state  circuit_breaker_state_enum NOT NULL,
    to_state    circuit_breaker_state_enum NOT NULL,
    reason      TEXT NOT NULL DEFAULT '',
    trigger     JSONB NOT NULL DEFAULT '{}',        -- trigger params at transition
    actor_id    BIGINT,                             -- admin user_id; NULL = automated
    hold_ms     BIGINT NOT NULL DEFAULT 0,          -- effective hold (post-flap doubling)
    probes      INTEGER NOT NULL DEFAULT 0,         -- probes admitted at transition
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX idx_circuit_breaker_events_scope
    ON circuit_breaker_events (scope, target_id, created_at DESC);
CREATE INDEX idx_circuit_breaker_events_time
    ON circuit_breaker_events (created_at DESC);

COMMIT;
