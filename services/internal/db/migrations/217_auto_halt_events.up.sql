-- 217_auto_halt_events.up.sql
-- Phase-14 Task 14.3.2 — auto-halt on anomaly audit trail.
--
-- The halt itself is the five-tier circuit breaker's job (spec §2.6 —
-- circuit_breaker_events, migration 206, records every state
-- transition). This table records the DETECTOR layer: which anomaly
-- detector fired, the readings that breached, whether the P1 ops alert
-- and the user notifications were dispatched, and when the instrument
-- auto-resumed. It answers "why was this instrument halted" for
-- surveillance/ops review without reconstructing detector math from
-- breaker trigger JSON.
--
-- Actions:
--   HALTED   detector-driven suspension entered (breaker OPEN)
--   RESUMED  breaker returned to CLOSED (probe window completed or
--            dual-controlled reset) — the "auto-resume if cleared" record
--
-- Detector names are fixed tokens; unknown detectors would silently
-- lose audit attribution, so the CHECK fails closed.

BEGIN;

CREATE TABLE auto_halt_events (
    event_id      BIGSERIAL PRIMARY KEY,
    symbol        VARCHAR(32) NOT NULL,                -- canonical, e.g. 'EUR/USD'
    scope         VARCHAR(24) NOT NULL,                -- INSTRUMENT | VOLUME_SPIKE
    detector      VARCHAR(24) NOT NULL,
    action        VARCHAR(8)  NOT NULL,
    observed      JSONB       NOT NULL DEFAULT '{}'::jsonb, -- detector readings at fire time
    breaker_state VARCHAR(12) NOT NULL,                -- OPEN | HALF_OPEN | CLOSED
    alert_sent    BOOLEAN     NOT NULL DEFAULT false,  -- P1 ops alert dispatched
    notified      INTEGER     NOT NULL DEFAULT 0,      -- user notifications queued
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT auto_halt_events_detector_ck CHECK
        (detector IN ('PRICE_SPIKE','VOLUME_SPIKE','LATENCY_SPIKE','ERROR_RATE_SPIKE')),
    CONSTRAINT auto_halt_events_action_ck CHECK
        (action IN ('HALTED','RESUMED')),
    CONSTRAINT auto_halt_events_notified_nonneg CHECK (notified >= 0)
);

-- Per-instrument incident review, newest first.
CREATE INDEX idx_auto_halt_events_symbol ON auto_halt_events (symbol, event_id DESC);
-- Detector-level stats (which detector is noisy).
CREATE INDEX idx_auto_halt_events_detector ON auto_halt_events (detector, event_id DESC);

COMMIT;
