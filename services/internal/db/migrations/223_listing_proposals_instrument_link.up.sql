-- 223_listing_proposals_instrument_link.up.sql
-- Phase-15 Task 15.3.12 (spec §7.5, §24 #352) — narrow listing_proposals
-- extension. 091 owns the proposal table and its PROPOSED → IN_REVIEW →
-- APPROVED/REJECTED → SCHEDULED CHECK + live-symbol partial index; this
-- migration adds the columns the approval pipeline needs:
--
--   reviewed_by / reviewed_at   the reviewer's principal + instant (the
--                               approval-pair evidence — proposer is
--                               proposer_id, reviewer is reviewed_by, and
--                               the four-eyes approver lands on the
--                               dual-control request).
--   dual_control_id             the instrument-listing four-eyes request
--                               the review-APPROVE leg submits; approval
--                               executes the DRAFT create in-tx.
--   instrument_id               the DRAFT instruments row the approved
--                               listing created.
--   activate_at                 the scheduled DRAFT→ACTIVE instant (next
--                               weekly open per the §7.4 session calendar,
--                               or an explicit operator-supplied instant).
--
-- All columns additive + nullable; the 091 CHECK and partial index are
-- untouched. Numbered after 091 because it alters the 091-owned table —
-- migrations apply in filename order.

BEGIN;

ALTER TABLE listing_proposals
    ADD COLUMN IF NOT EXISTS reviewed_by      BIGINT REFERENCES users (id),
    ADD COLUMN IF NOT EXISTS reviewed_at      TIMESTAMPTZ,
    ADD COLUMN IF NOT EXISTS dual_control_id  BIGINT,
    ADD COLUMN IF NOT EXISTS instrument_id    BIGINT REFERENCES instruments (id),
    ADD COLUMN IF NOT EXISTS activate_at      TIMESTAMPTZ;

-- Ops queue read path: pending proposals sorted oldest-first.
CREATE INDEX IF NOT EXISTS listing_proposals_status_idx
    ON listing_proposals (status, created_at);

COMMIT;
