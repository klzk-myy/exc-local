-- 151_oauth_clients.up.sql
-- Phase-05 Task 5.3.1 — OAuth2 client-credentials grant for institutional
-- programmatic clients (spec §8.1). Secret stored as bcrypt (cost 12) per
-- spec §8.8 item 3 secret-handling convention; the raw secret is shown
-- once at registration and never persisted in recoverable form.

BEGIN;

CREATE TYPE oauth_client_status_enum AS ENUM ('ACTIVE', 'SUSPENDED', 'REVOKED');

CREATE TABLE oauth_clients (
    id                 BIGSERIAL PRIMARY KEY,
    client_id          VARCHAR(64)  NOT NULL UNIQUE,   -- 'oc_' + random, sent as client_id
    client_secret_hash VARCHAR(100) NOT NULL,          -- bcrypt $2b$12$...
    account_id         BIGINT       NOT NULL REFERENCES accounts (id),
    name               VARCHAR(128) NOT NULL DEFAULT '',
    scopes             TEXT[]       NOT NULL DEFAULT '{}', -- granted scope ceiling
    status             oauth_client_status_enum NOT NULL DEFAULT 'ACTIVE',
    last_token_at      TIMESTAMPTZ,
    created_at         TIMESTAMPTZ  NOT NULL DEFAULT now(),
    updated_at         TIMESTAMPTZ  NOT NULL DEFAULT now()
);

CREATE INDEX oauth_clients_account_idx ON oauth_clients (account_id)
    WHERE status = 'ACTIVE';

COMMIT;
