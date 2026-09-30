-- 033_create_sar_reports.down.sql
-- Phase-21 Tasks 21.3.3/21.3.6 — drops the SAR/CTR/AML program schema.

BEGIN;

DROP TABLE IF EXISTS aml_account_assessments;
DROP TABLE IF EXISTS aml_program_artifacts;
DROP TABLE IF EXISTS aml_monitoring_events;
DROP TABLE IF EXISTS ctr_reports;
DROP TRIGGER IF EXISTS trg_sar_reports_immutable ON sar_reports;
DROP FUNCTION IF EXISTS sar_reports_filed_immutable();
DROP TABLE IF EXISTS sar_reports;

COMMIT;
