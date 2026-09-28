-- 048_support_tickets.up.sql
-- Phase-07 Task 7.3.7 — support tickets, complaints & the Support Agent /
-- Compliance Officer workflow (spec §5.28, §14.10.3, §24 #163/#333).
--
-- The column set is the union of the spec §5.28 table (id, account_id,
-- type SUPPORT|COMPLAINT|DISPUTE, subject, status, assignee_admin_id,
-- created_at, resolved_at) and the Task 7.3.7 field list (category
-- FUNDING|TRADING|KYC|TECHNICAL|COMPLAINT, priority, sla_due_at, plus the
-- remediation #17 ADR-routing columns origin_channel / adr_*). The task's
-- `ticket_id` is the BIGSERIAL PK `id` (spec §5.28 names the PK `id`;
-- ticket_notes.ticket_id is the FK per spec §5.44 item 8).
--
-- status enum is the union of both texts: task pins
-- OPEN|PENDING|RESOLVED|CLOSED, spec §5.28 pins
-- OPEN|IN_PROGRESS|RESOLVED|CLOSED — IN_PROGRESS is the working state,
-- PENDING is waiting-on-client.
--
-- queue is the routing surface: COMPLAINT/DISPUTE rows land in the
-- COMPLIANCE queue (Compliance Officer); everything else in SUPPORT
-- (Support Agent). The internal MiFID complaint register is
-- `WHERE type IN ('COMPLAINT','DISPUTE')` — the system of record; ADR
-- routing is additive (spec §14.10.3).

BEGIN;

CREATE TYPE support_ticket_type_enum AS ENUM ('SUPPORT', 'COMPLAINT', 'DISPUTE');
CREATE TYPE support_ticket_category_enum AS ENUM (
    'FUNDING', 'TRADING', 'KYC', 'TECHNICAL', 'COMPLAINT'
);
CREATE TYPE support_ticket_priority_enum AS ENUM ('LOW', 'NORMAL', 'HIGH', 'URGENT');
CREATE TYPE support_ticket_status_enum AS ENUM (
    'OPEN', 'PENDING', 'IN_PROGRESS', 'RESOLVED', 'CLOSED'
);

CREATE TABLE support_tickets (
    id                  BIGSERIAL PRIMARY KEY,
    account_id          BIGINT                      NOT NULL REFERENCES accounts (id),
    type                support_ticket_type_enum    NOT NULL DEFAULT 'SUPPORT',
    category            support_ticket_category_enum NOT NULL,
    priority            support_ticket_priority_enum NOT NULL DEFAULT 'NORMAL',
    subject             VARCHAR(255)                NOT NULL,
    body                TEXT                        NOT NULL DEFAULT '',
    status              support_ticket_status_enum  NOT NULL DEFAULT 'OPEN',
    queue               VARCHAR(16)                 NOT NULL DEFAULT 'SUPPORT'
                        CHECK (queue IN ('SUPPORT', 'COMPLIANCE')),
    assignee_admin_id   BIGINT,                                    -- admin user_id
    origin_channel      VARCHAR(32)                 NOT NULL DEFAULT 'PORTAL',  -- §14.10.3
    sla_due_at          TIMESTAMPTZ                 NOT NULL,       -- ack SLA (min(8 business hours, 48h statutory) for complaints)
    -- §27.1 ADR row: statutory complaint clocks — final response
    -- deadline (8 weeks) + breach-flag columns so the 60s sweeper
    -- alerts each SLA boundary exactly once.
    final_response_due_at TIMESTAMPTZ,
    acknowledged_at     TIMESTAMPTZ,
    ack_breach_flagged_at   TIMESTAMPTZ,
    final_breach_flagged_at TIMESTAMPTZ,
    resolved_at         TIMESTAMPTZ,
    -- External ADR/ombudsman routing (Task 7.3.7 remediation #17);
    -- additive to the internal register, never a replacement.
    adr_requested       BOOLEAN                     NOT NULL DEFAULT FALSE,
    adr_scheme          VARCHAR(64),                               -- e.g. FOS, AFCA
    adr_reference       VARCHAR(128),
    adr_acknowledged_at TIMESTAMPTZ,
    created_at          TIMESTAMPTZ                 NOT NULL DEFAULT now(),
    updated_at          TIMESTAMPTZ                 NOT NULL DEFAULT now()
);

CREATE TABLE ticket_notes (
    note_id     BIGSERIAL PRIMARY KEY,
    ticket_id   BIGINT      NOT NULL REFERENCES support_tickets (id) ON DELETE CASCADE,
    author_id   BIGINT      NOT NULL,                              -- admin user_id or account owner
    author_kind VARCHAR(8)  NOT NULL DEFAULT 'ADMIN'
                CHECK (author_kind IN ('ADMIN', 'CLIENT')),
    internal    BOOLEAN     NOT NULL DEFAULT TRUE,                 -- internal notes never surface to clients
    body        TEXT        NOT NULL,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Client list: own tickets newest-first (keyset reads use (created_at,id)).
CREATE INDEX idx_support_tickets_account
    ON support_tickets (account_id, created_at DESC, id DESC);
-- Admin queue views.
CREATE INDEX idx_support_tickets_queue_status
    ON support_tickets (queue, status, created_at DESC, id DESC);
-- SLA-breach sweep: unresolved rows ordered by due time (ack clock and
-- the complaint final-response clock are swept separately).
CREATE INDEX idx_support_tickets_sla
    ON support_tickets (sla_due_at)
    WHERE status NOT IN ('RESOLVED', 'CLOSED');
CREATE INDEX idx_support_tickets_final_sla
    ON support_tickets (final_response_due_at)
    WHERE status NOT IN ('RESOLVED', 'CLOSED') AND final_response_due_at IS NOT NULL;
CREATE INDEX idx_ticket_notes_ticket
    ON ticket_notes (ticket_id, created_at, note_id);

COMMIT;
