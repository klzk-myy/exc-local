-- 250_venue_governance.down.sql — drop the venue-governance stores and
-- the venue_members column extensions.

BEGIN;

DROP TABLE IF EXISTS venue_launch_prerequisites;
DROP TABLE IF EXISTS cco_reports;
DROP TABLE IF EXISTS venue_self_assessments;
DROP TABLE IF EXISTS venue_conflicts;
DROP TABLE IF EXISTS venue_case_evidence;
DROP TABLE IF EXISTS venue_cases;
DROP TABLE IF EXISTS venue_interventions;
DROP TABLE IF EXISTS venue_rule_acks;
DROP TABLE IF EXISTS venue_rule_notices;
DROP TABLE IF EXISTS venue_rulebooks;
DROP TABLE IF EXISTS venue_member_reviews;
DROP TABLE IF EXISTS venue_member_events;

ALTER TABLE venue_members
    DROP COLUMN IF EXISTS jurisdiction,
    DROP COLUMN IF EXISTS terminated_at,
    DROP COLUMN IF EXISTS terminated_by,
    DROP COLUMN IF EXISTS termination_reason;

COMMIT;
