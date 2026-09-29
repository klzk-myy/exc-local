-- 219_instrument_change_log.up.sql
-- Phase-15 Task 15.3.8 (spec §7.1/§7.2/§7.4, §24 #234): maker-checker
-- instrument maintenance workflow — creation, parameter changes and
-- delisting — with a durable request spine plus the spec-named immutable
-- audit log.
--
--   instrument_change_requests   the mutable workflow spine (one row per
--                                proposed change; stage machine below).
--                                Additive — spec names only
--                                instrument_change_log; the request row
--                                carries the who/what pending state the
--                                immutable log cannot mutate.
--   instrument_change_log        immutable append-only audit (spec
--                                §15.3.8 item 7): one row per workflow
--                                event — who/what/when/why. UPDATE/DELETE
--                                rejected by trigger (regulatory review).
--   instruments.param_overrides  JSONB bag for maintenance parameters with
--                                no dedicated column (margin_rate,
--                                trading_hours) — additive deviation,
--                                applied via jsonb_set merge.
--
-- The delisting ladder itself (RESTRICTED 24h notice → DELISTED → 30d
-- close-only) is owned by the lifecycle cluster (Task 15.3.1/15.3.2 —
-- RestrictedGrace/DelistedGrace off instruments.updated_at); the DELIST
-- request rows here record the governance trail and the dual-control
-- reference.

BEGIN;

-- ---------------------------------------------------------------------------
-- Workflow spine. stage machine:
--   CREATE: PENDING_REVIEW (Risk Manager proposed)
--           → PENDING_APPROVAL (Compliance Officer reviewed)
--           → APPLIED (Super Admin approved; instruments row created DRAFT)
--           → REJECTED (any reviewer turn-down)
--   PARAM:  PENDING_APPROVAL (maker submitted)
--           → SCHEDULED (checker approved; effective_at = next session open)
--           → APPLIED (sweep applied at session boundary)
--           → REJECTED | CANCELLED
--           emergency: Super Admin approves → APPLIED immediately + P1 alert
--   DELIST: approved dual-control request lands APPLIED for the RESTRICTED
--           leg and SCHEDULED (effective_at = +24h) for the DELISTED leg;
--           the 30-day close-only tail is owned by the lifecycle cluster
--           (Task 15.3.1).
-- ---------------------------------------------------------------------------
CREATE TABLE instrument_change_requests (
    id             BIGSERIAL    PRIMARY KEY,
    instrument_id  BIGINT       REFERENCES instruments (id), -- NULL for CREATE
    symbol         VARCHAR(32)  NOT NULL,
    change_type    VARCHAR(8)   NOT NULL
                   CHECK (change_type IN ('CREATE','PARAM','DELIST')),
    field          VARCHAR(64)  NOT NULL,  -- param name; 'INSTRUMENT' for CREATE/'status' for DELIST
    old_value      TEXT,
    new_value      TEXT,
    payload        JSONB        NOT NULL DEFAULT '{}'::jsonb, -- full creation spec / delist detail
    reason         TEXT         NOT NULL,
    stage          VARCHAR(16)  NOT NULL DEFAULT 'PENDING_REVIEW'
                   CHECK (stage IN ('PENDING_REVIEW','PENDING_APPROVAL',
                                    'SCHEDULED','APPLIED','REJECTED','CANCELLED')),
    requested_by   BIGINT       NOT NULL REFERENCES users (id),
    reviewed_by    BIGINT       REFERENCES users (id),
    approved_by    BIGINT       REFERENCES users (id),
    emergency      BOOLEAN      NOT NULL DEFAULT false,
    effective_at   TIMESTAMPTZ,            -- scheduled activation (session boundary / delist ladder)
    created_at     TIMESTAMPTZ  NOT NULL DEFAULT now(),
    reviewed_at    TIMESTAMPTZ,
    decided_at     TIMESTAMPTZ,
    applied_at     TIMESTAMPTZ
);

-- Effective-date sweep (scheduled activations + delist ladder).
CREATE INDEX icr_due_ix
    ON instrument_change_requests (effective_at)
    WHERE stage = 'SCHEDULED';

-- Concurrent-change guard: at most one open request per (instrument, field).
-- NULL instrument_id (CREATE rows) never conflicts here; the pending-CREATE
-- symbol guard below covers that case.
CREATE UNIQUE INDEX icr_open_field_ux
    ON instrument_change_requests (instrument_id, field)
    WHERE stage IN ('PENDING_REVIEW','PENDING_APPROVAL','SCHEDULED')
      AND instrument_id IS NOT NULL;

-- One open CREATE proposal per symbol.
CREATE UNIQUE INDEX icr_open_create_ux
    ON instrument_change_requests (symbol)
    WHERE change_type = 'CREATE'
      AND stage IN ('PENDING_REVIEW','PENDING_APPROVAL');

-- ---------------------------------------------------------------------------
-- Immutable audit log (spec §15.3.8 item 7) — append-only, trigger-enforced.
-- ---------------------------------------------------------------------------
CREATE TABLE instrument_change_log (
    id            BIGSERIAL    PRIMARY KEY,
    request_id    BIGINT       NOT NULL REFERENCES instrument_change_requests (id),
    instrument_id BIGINT,                                -- resolved at write time
    symbol        VARCHAR(32)  NOT NULL,
    action        VARCHAR(16)  NOT NULL
                  CHECK (action IN
                      ('SUBMITTED','REVIEWED','APPROVED','REJECTED',
                       'CANCELLED','APPLIED')),
    field         VARCHAR(64)  NOT NULL,
    old_value     TEXT,
    new_value     TEXT,
    actor_id      BIGINT       NOT NULL,                 -- admin user id acting
    reason        TEXT,
    created_at    TIMESTAMPTZ  NOT NULL DEFAULT now()
);

CREATE INDEX instrument_change_log_instrument_ix
    ON instrument_change_log (instrument_id, created_at);
CREATE INDEX instrument_change_log_request_ix
    ON instrument_change_log (request_id);

CREATE OR REPLACE FUNCTION instrument_change_log_immutable() RETURNS trigger AS $$
BEGIN
    RAISE EXCEPTION 'instrument_change_log is append-only — % is forbidden', TG_OP
        USING ERRCODE = 'raise_exception';
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER trg_instrument_change_log_immutable
    BEFORE UPDATE OR DELETE ON instrument_change_log
    FOR EACH ROW EXECUTE FUNCTION instrument_change_log_immutable();

-- ---------------------------------------------------------------------------
-- instruments: non-column parameter bag. Lifecycle transition timing is
-- already tracked via instruments.updated_at (the lifecycle Sweep's
-- grace-window source — migration 001); a second stamp would diverge.
-- ---------------------------------------------------------------------------
ALTER TABLE instruments
    ADD COLUMN IF NOT EXISTS param_overrides JSONB NOT NULL DEFAULT '{}'::jsonb;

COMMIT;
