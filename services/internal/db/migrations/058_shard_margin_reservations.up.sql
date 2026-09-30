-- 058_shard_margin_reservations.up.sql
-- Spec §5.35 shard_margin_reservations — Phase-19 Task 19.3.11
-- cross-shard portfolio margin coherence (§13.1, §24 #176).
--
-- The Go Risk Coordinator (services/cmd/risk) runs the two-phase
-- headroom reservation protocol over Aeron IPC margin_ctl_{in,out}_{shard}
-- channels (ipc/margin_ctl.go wire codec, byte-identical to
-- core/include/risk/CrossShardMarginCoordinator.h). A reservation locks a
-- slice of haircut-adjusted account equity while a shard engine admits an
-- order; COMMITTED rows convert to maintenance margin on fill, RELEASED
-- on cancel/reject/expiry.
--
-- Status lifecycle: PENDING → COMMITTED → RELEASED, plus DENIED as the
-- NACK tombstone so a replayed REQ deterministically re-NACKs (mirrors
-- ReservationState::Denied in the C++ engine-side table). release_reason
-- carries the wire ReleaseReason/NackReason token for audit.

BEGIN;

CREATE TYPE shard_reservation_status_enum AS ENUM (
    'PENDING',     -- REQ logged, grant decision in-flight (relay path)
    'COMMITTED',   -- ACK'd slice — counts against global headroom
    'RELEASED',    -- terminal: released / expired / compensated
    'DENIED'       -- terminal: NACK tombstone (replay dedupe)
);

CREATE TABLE shard_margin_reservations (
    reservation_id BIGINT       NOT NULL PRIMARY KEY,   -- issuer id: high16=shard, low48=seq
    account_id     BIGINT       NOT NULL REFERENCES accounts (id),
    consumer_shard SMALLINT     NOT NULL,               -- shard whose order needs the slice
    host_shard     SMALLINT     NOT NULL,               -- shard holding the locked headroom
    instrument_id  BIGINT       NOT NULL,
    order_id       BIGINT       NOT NULL DEFAULT 0,     -- admission context (0 = none)
    reserved_amount DECIMAL(28,8) NOT NULL,             -- requested slice
    granted_amount  DECIMAL(28,8),                      -- ACK'd slice (<= reserved; NULL pre-ACK)
    currency       VARCHAR(3)   NOT NULL,               -- account base (USD numeraire)
    req_flags      SMALLINT     NOT NULL DEFAULT 0,     -- wire req_flags (bit0 correlation-offset)
    status         shard_reservation_status_enum NOT NULL DEFAULT 'PENDING',
    release_reason VARCHAR(32),                          -- ReleaseReason/NackReason token
    expires_at     TIMESTAMPTZ  NOT NULL,               -- authoritative TTL (5s default)
    created_at     TIMESTAMPTZ  NOT NULL DEFAULT now(),
    updated_at     TIMESTAMPTZ  NOT NULL DEFAULT now(),
    released_at    TIMESTAMPTZ
);

-- One open reservation per account per instrument (Task 19.3.11 spec):
-- the SERIALIZABLE claim transaction relies on this to reject concurrent
-- duplicate-slice requests deterministically.
CREATE UNIQUE INDEX shard_margin_reservations_open_uq
    ON shard_margin_reservations (account_id, instrument_id)
    WHERE status IN ('PENDING', 'COMMITTED');

-- Per-account open-reservation cap (Phase-02 Task 2.3.14: max 10
-- concurrent cross-shard operations) and the expiry sweeper both read
-- these indexes.
CREATE INDEX shard_margin_reservations_open_account
    ON shard_margin_reservations (account_id)
    WHERE status IN ('PENDING', 'COMMITTED');

CREATE INDEX shard_margin_reservations_expiry
    ON shard_margin_reservations (expires_at)
    WHERE status IN ('PENDING', 'COMMITTED');

CREATE INDEX shard_margin_reservations_account_created
    ON shard_margin_reservations (account_id, created_at DESC);

COMMIT;
