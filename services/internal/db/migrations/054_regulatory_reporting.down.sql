-- 054_regulatory_reporting.down.sql
-- Symmetric down-migration: drop in FK-safe order (acks → submissions →
-- events first, then the independent tables, then the enum types).

BEGIN;

DROP TABLE IF EXISTS venue_members;
DROP TABLE IF EXISTS cftc_position_limits;
DROP TABLE IF EXISTS party_identifiers;
DROP TABLE IF EXISTS regulatory_schema_versions;
DROP TABLE IF EXISTS regulatory_report_breaks;
DROP TABLE IF EXISTS regulatory_report_acks;
DROP TABLE IF EXISTS regulatory_report_submissions;
DROP TABLE IF EXISTS regulatory_report_events;

DROP TYPE IF EXISTS reg_break_status_enum;
DROP TYPE IF EXISTS reg_break_type_enum;
DROP TYPE IF EXISTS reg_ack_status_enum;
DROP TYPE IF EXISTS reg_submission_status_enum;
DROP TYPE IF EXISTS reg_destination_enum;
DROP TYPE IF EXISTS reg_event_status_enum;
DROP TYPE IF EXISTS reg_event_type_enum;
DROP TYPE IF EXISTS reg_action_enum;
DROP TYPE IF EXISTS reg_regime_enum;

COMMIT;
