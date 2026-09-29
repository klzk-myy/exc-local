-- 029_create_surveillance_signals.up.sql
-- Phase-17 Task 17.3.3 — market-abuse surveillance signals.
--
-- Phase-17 emits signals computed from the L3 order-level stream;
-- Phase-21 (Task 21.3.x case management) consumes this table to open
-- surveillance cases — no enforcement lives here. Columns mirror the
-- L3 wire contract (account_hash is the anonymized account id the
-- stream carries; compliance resolves it to an account on review) plus
-- the seq/wal windows that make a signal re-derivable from the feed.
--
-- Idempotency: dedup_key is a deterministic content hash computed by
-- the emitter over (signal_type, symbol, account_hash, evidence seq
-- window). Re-delivery/replay of the same L3 events regenerates the
-- same key; INSERT ... ON CONFLICT DO NOTHING makes emission safe to
-- retry (spec §2.7 — at-least-once upstream, exactly-once storage).
--
-- Retention: §19.12 surveillance 5y — in-table (retention policy row
-- already registered: StoreTablePG, TSColumn created_at, 1825d).

BEGIN;

CREATE TABLE surveillance_signals (
    id            BIGSERIAL    PRIMARY KEY,
    signal_type   VARCHAR(32)  NOT NULL,          -- SPOOFING | LAYERING | WASH_TRADING | MARKING_THE_CLOSE | MOMENTUM_IGNITION | FRONT_RUNNING | INSIDER_DEALING
    symbol        VARCHAR(32)  NOT NULL,          -- canonical instrument symbol
    account_hash  BIGINT       NOT NULL,          -- anonymized account id from the L3 stream
    order_id      BIGINT,                          -- primary order leg (nullable — patterns span orders)
    counter_order_id BIGINT,                       -- wash-trade counterparty leg
    first_l3_seq  BIGINT       NOT NULL,          -- first l3_seq in the detected window
    last_l3_seq   BIGINT       NOT NULL,          -- last l3_seq in the detected window
    first_wal_seq BIGINT,                          -- WAL correspondence (spec §11.1)
    last_wal_seq  BIGINT,
    window_start  TIMESTAMPTZ  NOT NULL,          -- event-time window begin
    window_end    TIMESTAMPTZ  NOT NULL,          -- event-time window end
    evidence      JSONB        NOT NULL,          -- detector-produced detail (event refs, qty, prices, thresholds)
    dedup_key     VARCHAR(64)  NOT NULL,          -- content hash — idempotent emission
    status        VARCHAR(16)  NOT NULL DEFAULT 'OPEN', -- OPEN | CASED | DISMISSED (Phase-21 transitions)
    created_at    TIMESTAMPTZ  NOT NULL DEFAULT now(),
    updated_at    TIMESTAMPTZ  NOT NULL DEFAULT now(),
    CONSTRAINT surveillance_signals_type_ck CHECK (signal_type IN (
        'SPOOFING','LAYERING','WASH_TRADING','MARKING_THE_CLOSE',
        'MOMENTUM_IGNITION','FRONT_RUNNING','INSIDER_DEALING')),
    CONSTRAINT surveillance_signals_status_ck CHECK (status IN ('OPEN','CASED','DISMISSED')),
    CONSTRAINT surveillance_signals_seq_ck CHECK (last_l3_seq >= first_l3_seq)
);

-- Idempotent emission: one row per deterministic detection window.
CREATE UNIQUE INDEX uq_surveillance_signals_dedup ON surveillance_signals (dedup_key);

-- Review surfaces: newest signals per symbol/type, per-account audits.
CREATE INDEX idx_surveillance_signals_symbol ON surveillance_signals (symbol, id DESC);
CREATE INDEX idx_surveillance_signals_type   ON surveillance_signals (signal_type, id DESC);
CREATE INDEX idx_surveillance_signals_acct   ON surveillance_signals (account_hash, id DESC);
-- Phase-21 case pipeline: open signals awaiting triage.
CREATE INDEX idx_surveillance_signals_open   ON surveillance_signals (id)
    WHERE status = 'OPEN';

COMMIT;
